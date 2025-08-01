// Copyright 2016 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package flowinfra

import (
	"context"
	"io"

	"github.com/cockroachdb/cockroach/pkg/sql/execinfra"
	"github.com/cockroachdb/cockroach/pkg/sql/execinfrapb"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgcode"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgerror"
	"github.com/cockroachdb/cockroach/pkg/sql/types"
	"github.com/cockroachdb/cockroach/pkg/util/admission"
	"github.com/cockroachdb/cockroach/pkg/util/cancelchecker"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/errors"
)

// InboundStreamHandler is a handler of an inbound stream.
type InboundStreamHandler interface {
	// Run is called once a FlowStream RPC is handled and a stream is obtained to
	// make this stream accessible to the rest of the flow.
	Run(
		ctx context.Context, stream execinfrapb.RPCDistSQL_FlowStreamStream, firstMsg *execinfrapb.ProducerMessage, f *FlowBase,
	) error
	// Timeout is called with an error, which results in the teardown of the
	// stream strategy with the given error.
	// WARNING: timeout may block.
	Timeout(err error)
}

// RowInboundStreamHandler is an InboundStreamHandler for the row based flow.
// It is exported since it is the default for the flow infrastructure.
type RowInboundStreamHandler struct {
	execinfra.RowReceiver
	Types []*types.T
}

var _ InboundStreamHandler = RowInboundStreamHandler{}

// Run is part of the InboundStreamHandler interface.
func (s RowInboundStreamHandler) Run(
	ctx context.Context,
	stream execinfrapb.RPCDistSQL_FlowStreamStream,
	firstMsg *execinfrapb.ProducerMessage,
	f *FlowBase,
) error {
	return processInboundStream(ctx, stream, firstMsg, s.RowReceiver, f, s.Types)
}

// Timeout is part of the InboundStreamHandler interface.
func (s RowInboundStreamHandler) Timeout(err error) {
	s.Push(
		nil, /* row */
		&execinfrapb.ProducerMetadata{Err: err},
	)
	s.ProducerDone()
}

// processInboundStream receives rows from a RPCDistSQL_FlowStreamStream and sends
// them to a RowReceiver. Optionally processes an initial StreamMessage that was
// already received (because the first message contains the flow and stream IDs,
// it needs to be received before we can get here).
func processInboundStream(
	ctx context.Context,
	stream execinfrapb.RPCDistSQL_FlowStreamStream,
	firstMsg *execinfrapb.ProducerMessage,
	dst execinfra.RowReceiver,
	f *FlowBase,
	types []*types.T,
) error {

	err := processInboundStreamHelper(ctx, stream, firstMsg, dst, f, types)

	// err, if set, will also be propagated to the producer
	// as the last record that the producer gets.
	if err != nil {
		log.VEventf(ctx, 1, "inbound stream error: %s", err)
		return err
	}
	log.VEventf(ctx, 1, "inbound stream done")
	// We are now done. The producer, if it's still around, will receive an EOF
	// error over its side of the stream.
	return nil
}

func processInboundStreamHelper(
	ctx context.Context,
	stream execinfrapb.RPCDistSQL_FlowStreamStream,
	firstMsg *execinfrapb.ProducerMessage,
	dst execinfra.RowReceiver,
	f *FlowBase,
	types []*types.T,
) error {
	draining := false
	var sd StreamDecoder
	sd.Init(types)

	sendErrToConsumer := func(err error) {
		if err != nil {
			dst.Push(nil, &execinfrapb.ProducerMetadata{Err: err})
		}
		dst.ProducerDone()
	}

	if firstMsg != nil {
		if res := processProducerMessage(
			ctx, f, stream, dst, &sd, &draining, firstMsg,
		); res.err != nil || res.consumerClosed {
			sendErrToConsumer(res.err)
			return res.err
		}
	}

	// There's two goroutines involved in handling the RPC - the current one (the
	// "parent"), which is watching for context cancellation, and a "reader" one
	// that receives messages from the stream. This is all because a stream.Recv()
	// call doesn't react to context cancellation. The idea is that, if the parent
	// detects a canceled context, it will return from this RPC handler, which
	// will cause the stream to be closed. Because the parent cannot wait for the
	// reader to finish (that being the whole point of the different goroutines),
	// the reader sending an error to the parent might race with the parent
	// finishing. In that case, nobody cares about the reader anymore and so its
	// result channel is buffered.
	errChan := make(chan error, 1)

	f.GetWaitGroup().Add(1)
	go func() {
		defer f.GetWaitGroup().Done()
		for {
			msg, err := stream.Recv()
			if err != nil {
				if err != io.EOF {
					// Communication error.
					log.VEventf(ctx, 2, "Inbox communication error: %v", err)
					err = pgerror.Wrap(err, pgcode.InternalConnectionFailure, "inbox communication error")
					sendErrToConsumer(err)
					errChan <- err
					return
				}
				// End of the stream.
				sendErrToConsumer(nil)
				errChan <- nil
				return
			}

			log.VEvent(ctx, 2, "Inbox received message")
			if res := processProducerMessage(
				ctx, f, stream, dst, &sd, &draining, msg,
			); res.err != nil || res.consumerClosed {
				sendErrToConsumer(res.err)
				errChan <- res.err
				return
			}
		}
	}()

	// Check for context cancellation while reading from the stream on another
	// goroutine.
	select {
	case <-f.GetCtxDone():
		return cancelchecker.QueryCanceledError
	case err := <-errChan:
		return err
	}
}

// sendDrainSignalToProducer is called when the consumer wants to signal the
// producer that it doesn't need any more rows and the producer should drain. A
// signal is sent on stream to the producer to ask it to send metadata.
func sendDrainSignalToStreamProducer(
	ctx context.Context, stream execinfrapb.RPCDistSQL_FlowStreamStream,
) error {
	log.VEvent(ctx, 1, "sending drain signal to producer")
	sig := execinfrapb.ConsumerSignal{DrainRequest: &execinfrapb.DrainRequest{}}
	return stream.Send(&sig)
}

// processProducerMessage is a helper function to process data from the producer
// and send it along to the consumer. It keeps track of whether or not it's
// draining between calls. If err in the result is set (or if the consumer is
// closed), the caller must return the error to the producer.
func processProducerMessage(
	ctx context.Context,
	flowBase *FlowBase,
	stream execinfrapb.RPCDistSQL_FlowStreamStream,
	dst execinfra.RowReceiver,
	sd *StreamDecoder,
	draining *bool,
	msg *execinfrapb.ProducerMessage,
) processMessageResult {
	err := sd.AddMessage(ctx, msg)
	if err != nil {
		return processMessageResult{
			err: errors.Wrapf(err, "%s",
				// TODO(knz): Instead of pre-formatting the string here, use
				// errors.WithContextTags() here and let the error formatter
				// show the tags later.
				log.FormatWithContextTags(ctx, "decoding error")),
			consumerClosed: false,
		}
	}
	var admissionQ *admission.WorkQueue
	if flowBase.Cfg != nil {
		admissionQ = flowBase.Cfg.SQLSQLResponseAdmissionQ
	}
	if admissionQ != nil {
		if _, err := admissionQ.Admit(ctx, flowBase.admissionInfo); err != nil {
			return processMessageResult{err: err, consumerClosed: false}
		}
	}
	for {
		row, meta, err := sd.GetRow(nil /* rowBuf */)
		if err != nil {
			return processMessageResult{err: err, consumerClosed: false}
		}
		if row == nil && meta == nil {
			// No more rows in the last message.
			return processMessageResult{err: nil, consumerClosed: false}
		}

		// TODO(yuzefovich): consider removing this logging since the verbosity
		// check is not exactly free.
		if log.V(3) && row != nil {
			log.Infof(ctx, "inbound stream pushing row %s", row.String(sd.types))
		}
		if *draining && meta == nil {
			// Don't forward data rows when we're draining.
			continue
		}
		switch dst.Push(row, meta) {
		case execinfra.NeedMoreRows:
			continue
		case execinfra.SwitchToAnotherPortal:
			return processMessageResult{err: errors.AssertionFailedf("not allowed to switch to another portal")}
		case execinfra.DrainRequested:
			// The rest of rows are not needed by the consumer. We'll send a drain
			// signal to the producer and expect it to quickly send trailing
			// metadata and close its side of the stream, at which point we also
			// close the consuming side of the stream and call dst.ProducerDone().
			if !*draining {
				*draining = true
				if err := sendDrainSignalToStreamProducer(ctx, stream); err != nil {
					log.Errorf(ctx, "draining error: %s", err)
				}
			}
		case execinfra.ConsumerClosed:
			return processMessageResult{err: nil, consumerClosed: true}
		}
	}
}

type processMessageResult struct {
	err            error
	consumerClosed bool
}
