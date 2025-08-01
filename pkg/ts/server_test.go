// Copyright 2015 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package ts_test

import (
	"context"
	"fmt"
	"io"
	"reflect"
	"sort"
	"testing"
	"unsafe"

	"github.com/cockroachdb/cockroach/pkg/base"
	"github.com/cockroachdb/cockroach/pkg/kv"
	"github.com/cockroachdb/cockroach/pkg/kv/kvpb"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver"
	"github.com/cockroachdb/cockroach/pkg/multitenant/tenantcapabilitiespb"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/security/username"
	"github.com/cockroachdb/cockroach/pkg/testutils/serverutils"
	"github.com/cockroachdb/cockroach/pkg/ts"
	"github.com/cockroachdb/cockroach/pkg/ts/tspb"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/errors"
	"github.com/kr/pretty"
	"github.com/stretchr/testify/require"
)

func TestServerQuery(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	s := serverutils.StartServerOnly(t, base.TestServerArgs{
		// For now, direct access to the tsdb is reserved to the storage layer.
		DefaultTestTenant: base.TestIsSpecificToStorageLayerAndNeedsASystemTenant,

		Knobs: base.TestingKnobs{
			Store: &kvserver.StoreTestingKnobs{
				DisableTimeSeriesMaintenanceQueue: true,
			},
		},
	})
	defer s.Stopper().Stop(context.Background())

	// Populate data directly.
	tsdb := s.TsDB().(*ts.DB)
	if err := tsdb.StoreData(context.Background(), ts.Resolution10s, []tspb.TimeSeriesData{
		{
			Name:   "test.metric",
			Source: "source1",
			Datapoints: []tspb.TimeSeriesDatapoint{
				{
					TimestampNanos: 400 * 1e9,
					Value:          100.0,
				},
				{
					TimestampNanos: 500 * 1e9,
					Value:          200.0,
				},
				{
					TimestampNanos: 520 * 1e9,
					Value:          300.0,
				},
			},
		},
		{
			Name:   "test.metric",
			Source: "source2",
			Datapoints: []tspb.TimeSeriesDatapoint{
				{
					TimestampNanos: 400 * 1e9,
					Value:          100.0,
				},
				{
					TimestampNanos: 500 * 1e9,
					Value:          200.0,
				},
				{
					TimestampNanos: 510 * 1e9,
					Value:          250.0,
				},
				{
					TimestampNanos: 530 * 1e9,
					Value:          350.0,
				},
			},
		},
		{
			Name: "other.metric",
			Datapoints: []tspb.TimeSeriesDatapoint{
				{
					TimestampNanos: 400 * 1e9,
					Value:          100.0,
				},
				{
					TimestampNanos: 500 * 1e9,
					Value:          200.0,
				},
				{
					TimestampNanos: 510 * 1e9,
					Value:          250.0,
				},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	expectedResult := &tspb.TimeSeriesQueryResponse{
		Results: []tspb.TimeSeriesQueryResponse_Result{
			{
				Query: tspb.Query{
					Name:    "test.metric",
					Sources: []string{"source1", "source2"},
				},
				Datapoints: []tspb.TimeSeriesDatapoint{
					{
						TimestampNanos: 500 * 1e9,
						Value:          400.0,
					},
					{
						TimestampNanos: 510 * 1e9,
						Value:          500.0,
					},
					{
						TimestampNanos: 520 * 1e9,
						Value:          600.0,
					},
				},
			},
			{
				Query: tspb.Query{
					Name:    "other.metric",
					Sources: []string{""},
				},
				Datapoints: []tspb.TimeSeriesDatapoint{
					{
						TimestampNanos: 500 * 1e9,
						Value:          200.0,
					},
					{
						TimestampNanos: 510 * 1e9,
						Value:          250.0,
					},
				},
			},
			{
				Query: tspb.Query{
					Name:             "test.metric",
					Sources:          []string{"source1", "source2"},
					Downsampler:      tspb.TimeSeriesQueryAggregator_MAX.Enum(),
					SourceAggregator: tspb.TimeSeriesQueryAggregator_MAX.Enum(),
					Derivative:       tspb.TimeSeriesQueryDerivative_DERIVATIVE.Enum(),
				},
				Datapoints: []tspb.TimeSeriesDatapoint{
					{
						TimestampNanos: 500 * 1e9,
						Value:          1.0,
					},
					{
						TimestampNanos: 510 * 1e9,
						Value:          5.0,
					},
					{
						TimestampNanos: 520 * 1e9,
						Value:          5.0,
					},
				},
			},
		},
	}

	conn := s.RPCClientConn(t, username.RootUserName())
	client := conn.NewTimeSeriesClient()
	response, err := client.Query(context.Background(), &tspb.TimeSeriesQueryRequest{
		StartNanos: 500 * 1e9,
		EndNanos:   526 * 1e9,
		Queries: []tspb.Query{
			{
				Name: "test.metric",
			},
			{
				Name: "other.metric",
			},
			{
				Name:             "test.metric",
				Downsampler:      tspb.TimeSeriesQueryAggregator_MAX.Enum(),
				SourceAggregator: tspb.TimeSeriesQueryAggregator_MAX.Enum(),
				Derivative:       tspb.TimeSeriesQueryDerivative_DERIVATIVE.Enum(),
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range response.Results {
		sort.Strings(r.Sources)
	}
	if !response.Equal(expectedResult) {
		t.Fatalf("actual response \n%v\n did not match expected response \n%v",
			response, expectedResult)
	}

	// Test a query with downsampling enabled. The query is a sum of maximums.
	expectedResult = &tspb.TimeSeriesQueryResponse{
		Results: []tspb.TimeSeriesQueryResponse_Result{
			{
				Query: tspb.Query{
					Name:        "test.metric",
					Sources:     []string{"source1", "source2"},
					Downsampler: tspb.TimeSeriesQueryAggregator_MAX.Enum(),
				},
				Datapoints: []tspb.TimeSeriesDatapoint{
					{
						TimestampNanos: 0,
						Value:          200.0,
					},
					{
						TimestampNanos: 500 * 1e9,
						Value:          650.0,
					},
				},
			},
		},
	}
	response, err = client.Query(context.Background(), &tspb.TimeSeriesQueryRequest{
		StartNanos:  0,
		EndNanos:    1000 * 1e9,
		SampleNanos: 500 * 1e9,
		Queries: []tspb.Query{
			{
				Name:        "test.metric",
				Downsampler: tspb.TimeSeriesQueryAggregator_MAX.Enum(),
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range response.Results {
		sort.Strings(r.Sources)
	}
	if !response.Equal(expectedResult) {
		t.Fatalf("actual response \n%v\n did not match expected response \n%v",
			response, expectedResult)
	}
}

// TestServerQueryStarvation tests a very specific scenario, wherein a single
// query request has more queries than the server's MaxWorkers count.
func TestServerQueryStarvation(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	workerCount := 20
	s := serverutils.StartServerOnly(t, base.TestServerArgs{
		// For now, direct access to the tsdb is reserved to the storage layer.
		DefaultTestTenant: base.TestIsSpecificToStorageLayerAndNeedsASystemTenant,

		TimeSeriesQueryWorkerMax: workerCount,
	})
	defer s.Stopper().Stop(context.Background())

	seriesCount := workerCount * 2
	if err := populateSeries(seriesCount, 10, 3, s.TsDB().(*ts.DB)); err != nil {
		t.Fatal(err)
	}

	conn := s.RPCClientConn(t, username.RootUserName())
	client := conn.NewTimeSeriesClient()

	queries := make([]tspb.Query, 0, seriesCount)
	for i := 0; i < seriesCount; i++ {
		queries = append(queries, tspb.Query{
			Name: seriesName(i),
		})
	}

	if _, err := client.Query(context.Background(), &tspb.TimeSeriesQueryRequest{
		StartNanos: 0 * 1e9,
		EndNanos:   500 * 1e9,
		Queries:    queries,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestServerQueryTenant(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	s := serverutils.StartServerOnly(t, base.TestServerArgs{
		DefaultTestTenant: base.TestControlsTenantsExplicitly,

		Knobs: base.TestingKnobs{
			Store: &kvserver.StoreTestingKnobs{
				DisableTimeSeriesMaintenanceQueue: true,
			},
		},
	})
	defer s.Stopper().Stop(context.Background())

	systemDB := s.SystemLayer().SQLConn(t)

	// This metric exists in the tenant registry since it's SQL-specific.
	tenantMetricName := "sql.insert.count"
	// This metric exists only in the host/system registry since it's process-level.
	hostMetricName := "sys.rss"

	// Populate data directly.
	tsdb := s.TsDB().(*ts.DB)
	if err := tsdb.StoreData(context.Background(), ts.Resolution10s, []tspb.TimeSeriesData{
		{
			Name:   tenantMetricName,
			Source: "1",
			Datapoints: []tspb.TimeSeriesDatapoint{
				{
					TimestampNanos: 400 * 1e9,
					Value:          100.0,
				},
				{
					TimestampNanos: 500 * 1e9,
					Value:          200.0,
				},
			},
		},
		{
			Name:   tenantMetricName,
			Source: "1-2",
			Datapoints: []tspb.TimeSeriesDatapoint{
				{
					TimestampNanos: 400 * 1e9,
					Value:          1.0,
				},
				{
					TimestampNanos: 500 * 1e9,
					Value:          2.0,
				},
			},
		},
		{
			Name:   tenantMetricName,
			Source: "10",
			Datapoints: []tspb.TimeSeriesDatapoint{
				{
					TimestampNanos: 400 * 1e9,
					Value:          200.0,
				},
				{
					TimestampNanos: 500 * 1e9,
					Value:          400.0,
				},
			},
		},
		{
			Name:   tenantMetricName,
			Source: "10-2",
			Datapoints: []tspb.TimeSeriesDatapoint{
				{
					TimestampNanos: 400 * 1e9,
					Value:          4.0,
				},
				{
					TimestampNanos: 500 * 1e9,
					Value:          5.0,
				},
			},
		},
		{
			Name:   hostMetricName,
			Source: "1",
			Datapoints: []tspb.TimeSeriesDatapoint{
				{
					TimestampNanos: 400 * 1e9,
					Value:          13.0,
				},
				{
					TimestampNanos: 500 * 1e9,
					Value:          27.0,
				},
			},
		},
		{
			Name:   hostMetricName,
			Source: "10",
			Datapoints: []tspb.TimeSeriesDatapoint{
				{
					TimestampNanos: 400 * 1e9,
					Value:          31.0,
				},
				{
					TimestampNanos: 500 * 1e9,
					Value:          57.0,
				},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	// Undefined tenant ID should aggregate across all tenants.
	expectedAggregatedResult := &tspb.TimeSeriesQueryResponse{
		Results: []tspb.TimeSeriesQueryResponse_Result{
			{
				Query: tspb.Query{
					Name:    tenantMetricName,
					Sources: []string{"1"},
				},
				Datapoints: []tspb.TimeSeriesDatapoint{
					{
						TimestampNanos: 400 * 1e9,
						Value:          101.0,
					},
					{
						TimestampNanos: 500 * 1e9,
						Value:          202.0,
					},
				},
			},
			{
				Query: tspb.Query{
					Name:    tenantMetricName,
					Sources: []string{"1", "10"},
				},
				Datapoints: []tspb.TimeSeriesDatapoint{
					{
						TimestampNanos: 400 * 1e9,
						Value:          305.0,
					},
					{
						TimestampNanos: 500 * 1e9,
						Value:          607.0,
					},
				},
			},
		},
	}

	conn := s.RPCClientConn(t, username.RootUserName())
	client := conn.NewTimeSeriesClient()
	aggregatedResponse, err := client.Query(context.Background(), &tspb.TimeSeriesQueryRequest{
		StartNanos: 400 * 1e9,
		EndNanos:   500 * 1e9,
		Queries: []tspb.Query{
			{
				Name:    tenantMetricName,
				Sources: []string{"1"},
			},
			{
				// Not providing a source (nodeID or storeID) will aggregate across all sources.
				Name: tenantMetricName,
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range aggregatedResponse.Results {
		sort.Strings(r.Sources)
	}
	require.Equal(t, expectedAggregatedResult, aggregatedResponse)

	// System tenant ID should provide system tenant ts data.
	systemID := roachpb.MustMakeTenantID(1)
	expectedSystemResult := &tspb.TimeSeriesQueryResponse{
		Results: []tspb.TimeSeriesQueryResponse_Result{
			{
				Query: tspb.Query{
					Name:     tenantMetricName,
					Sources:  []string{"1"},
					TenantID: systemID,
				},
				Datapoints: []tspb.TimeSeriesDatapoint{
					{
						TimestampNanos: 400 * 1e9,
						Value:          100.0,
					},
					{
						TimestampNanos: 500 * 1e9,
						Value:          200.0,
					},
				},
			},
			{
				Query: tspb.Query{
					Name:     tenantMetricName,
					Sources:  []string{"1", "10"},
					TenantID: systemID,
				},
				Datapoints: []tspb.TimeSeriesDatapoint{
					{
						TimestampNanos: 400 * 1e9,
						Value:          300.0,
					},
					{
						TimestampNanos: 500 * 1e9,
						Value:          600.0,
					},
				},
			},
		},
	}

	systemResponse, err := client.Query(context.Background(), &tspb.TimeSeriesQueryRequest{
		StartNanos: 400 * 1e9,
		EndNanos:   500 * 1e9,
		Queries: []tspb.Query{
			{
				Name:     tenantMetricName,
				Sources:  []string{"1"},
				TenantID: systemID,
			},
			{
				Name:     tenantMetricName,
				TenantID: systemID,
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range systemResponse.Results {
		sort.Strings(r.Sources)
	}
	require.Equal(t, expectedSystemResult, systemResponse)

	// App tenant should only report metrics with its tenant ID in the secondary source field
	tenantID := roachpb.MustMakeTenantID(2)
	expectedTenantResponse := &tspb.TimeSeriesQueryResponse{
		Results: []tspb.TimeSeriesQueryResponse_Result{
			{
				Query: tspb.Query{
					Name:     tenantMetricName,
					Sources:  []string{"1"},
					TenantID: tenantID,
				},
				Datapoints: []tspb.TimeSeriesDatapoint{
					{
						TimestampNanos: 400 * 1e9,
						Value:          1.0,
					},
					{
						TimestampNanos: 500 * 1e9,
						Value:          2.0,
					},
				},
			},
			{
				Query: tspb.Query{
					Name:     tenantMetricName,
					Sources:  []string{"1", "10"},
					TenantID: tenantID,
				},
				Datapoints: []tspb.TimeSeriesDatapoint{
					{
						TimestampNanos: 400 * 1e9,
						Value:          5.0,
					},
					{
						TimestampNanos: 500 * 1e9,
						Value:          7.0,
					},
				},
			},
		},
	}

	tenant, _ := serverutils.StartTenant(t, s, base.TestTenantArgs{TenantID: tenantID})
	_, err = systemDB.Exec("ALTER TENANT [2] GRANT CAPABILITY can_view_tsdb_metrics=true;\n")
	if err != nil {
		t.Fatal(err)
	}
	capability := map[tenantcapabilitiespb.ID]string{tenantcapabilitiespb.CanViewTSDBMetrics: "true"}
	serverutils.WaitForTenantCapabilities(t, s, tenantID, capability, "")
	tenantConn := tenant.RPCClientConn(t, username.RootUserName())
	tenantClient := tenantConn.NewTimeSeriesClient()

	tenantResponse, err := tenantClient.Query(context.Background(), &tspb.TimeSeriesQueryRequest{
		StartNanos: 400 * 1e9,
		EndNanos:   500 * 1e9,
		Queries: []tspb.Query{
			{
				Name:    tenantMetricName,
				Sources: []string{"1"},
			},
			{
				// Not providing a source (nodeID or storeID) will aggregate across all sources.
				Name: tenantMetricName,
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range tenantResponse.Results {
		sort.Strings(r.Sources)
	}
	require.Equal(t, expectedTenantResponse, tenantResponse)

	// Test that host metrics are inaccessible to tenant without capability.
	hostMetricsRequest := &tspb.TimeSeriesQueryRequest{
		StartNanos: 400 * 1e9,
		EndNanos:   500 * 1e9,
		Queries: []tspb.Query{
			{
				Name:    hostMetricName,
				Sources: []string{"1"},
			},
		},
	}

	_, err = tenantClient.Query(context.Background(), hostMetricsRequest)
	require.Error(t, err)

	// Test that after enabling all metrics capability, host metrics are returned.
	expectedTenantHostMetricsResponse := &tspb.TimeSeriesQueryResponse{
		Results: []tspb.TimeSeriesQueryResponse_Result{
			{
				Query: tspb.Query{
					Name:    hostMetricName,
					Sources: []string{"1"},
				},
				Datapoints: []tspb.TimeSeriesDatapoint{
					{
						TimestampNanos: 400 * 1e9,
						Value:          13.0,
					},
					{
						TimestampNanos: 500 * 1e9,
						Value:          27.0,
					},
				},
			},
		},
	}
	_, err = systemDB.Exec("ALTER TENANT [2] GRANT CAPABILITY can_view_all_metrics=true;\n")
	if err != nil {
		t.Fatal(err)
	}
	capability = map[tenantcapabilitiespb.ID]string{tenantcapabilitiespb.CanViewAllMetrics: "true"}
	serverutils.WaitForTenantCapabilities(t, s, tenantID, capability, "")

	tenantResponse, err = tenantClient.Query(context.Background(), &tspb.TimeSeriesQueryRequest{
		StartNanos: 400 * 1e9,
		EndNanos:   500 * 1e9,
		Queries: []tspb.Query{
			{
				Name:    hostMetricName,
				Sources: []string{"1"},
			},
		},
	})
	require.NoError(t, err)
	require.Equal(t, expectedTenantHostMetricsResponse, tenantResponse)
}

// TestServerQueryMemoryManagement verifies that queries succeed under
// constrained memory requirements.
func TestServerQueryMemoryManagement(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	// Number of workers that will be available to process data.
	workerCount := 20
	// Number of series that will be queried.
	seriesCount := workerCount * 2
	// Number of data sources that will be generated.
	sourceCount := 6
	// Number of slabs (hours) of data we want to generate
	slabCount := 5
	// Generated datapoints every 100 seconds, so compute how many we want to
	// generate data across the target number of hours.
	valueCount := int(ts.Resolution10s.SlabDuration()/(100*1e9)) * slabCount

	// MemoryBudget is a function of slab size and source count.
	samplesPerSlab := ts.Resolution10s.SlabDuration() / ts.Resolution10s.SampleDuration()
	sizeOfSlab := int64(unsafe.Sizeof(roachpb.InternalTimeSeriesData{})) + (int64(unsafe.Sizeof(roachpb.InternalTimeSeriesSample{})) * samplesPerSlab)
	budget := 3 * sizeOfSlab * int64(sourceCount) * int64(workerCount)

	s := serverutils.StartServerOnly(t, base.TestServerArgs{
		// For now, direct access to the tsdb is reserved to the storage layer.
		DefaultTestTenant: base.TestIsSpecificToStorageLayerAndNeedsASystemTenant,

		TimeSeriesQueryWorkerMax:    workerCount,
		TimeSeriesQueryMemoryBudget: budget,
	})
	defer s.Stopper().Stop(context.Background())

	if err := populateSeries(seriesCount, sourceCount, valueCount, s.TsDB().(*ts.DB)); err != nil {
		t.Fatal(err)
	}

	conn := s.RPCClientConn(t, username.RootUserName())
	client := conn.NewTimeSeriesClient()

	queries := make([]tspb.Query, 0, seriesCount)
	for i := 0; i < seriesCount; i++ {
		queries = append(queries, tspb.Query{
			Name: seriesName(i),
		})
	}

	if _, err := client.Query(context.Background(), &tspb.TimeSeriesQueryRequest{
		StartNanos: 0 * 1e9,
		EndNanos:   5 * 3600 * 1e9,
		Queries:    queries,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestServerDump(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	ctx := context.Background()

	seriesCount := 10
	sourceCount := 5
	// Number of slabs (hours) of data we want to generate
	slabCount := 5
	// Number of datapoints to generate every hour. Generated datapoints every
	// 100 seconds, so compute how many we want to generate data across one hour.
	numPointsEachHour := int(ts.Resolution10s.SlabDuration() / (100 * 1e9))
	// Number of total datapoints.
	valueCount := numPointsEachHour * slabCount
	// We'll dump [startVal, endVal) below. To simplify the test, pick them
	// according to a slab boundary.
	startSlab, endSlab := 2, 4
	startVal := numPointsEachHour * startSlab
	endVal := numPointsEachHour * endSlab

	// Generate expected data.
	expectedMap := make(map[string]map[string]tspb.TimeSeriesData)
	for series := 0; series < seriesCount; series++ {
		sourceMap := make(map[string]tspb.TimeSeriesData)
		expectedMap[seriesName(series)] = sourceMap
		for source := 0; source < sourceCount; source++ {
			sourceMap[sourceName(source)] = tspb.TimeSeriesData{
				Name:       seriesName(series),
				Source:     sourceName(source),
				Datapoints: generateTimeSeriesDatapoints(startVal, endVal),
			}
		}
	}

	expTotalMsgCount := seriesCount * sourceCount * (endSlab - startSlab)

	s := serverutils.StartServerOnly(t, base.TestServerArgs{
		// For now, direct access to the tsdb is reserved to the storage layer.
		DefaultTestTenant: base.TestIsSpecificToStorageLayerAndNeedsASystemTenant,

		Knobs: base.TestingKnobs{
			Store: &kvserver.StoreTestingKnobs{
				DisableTimeSeriesMaintenanceQueue: true,
			},
		},
	})
	defer s.Stopper().Stop(ctx)

	if err := populateSeries(seriesCount, sourceCount, valueCount, s.TsDB().(*ts.DB)); err != nil {
		t.Fatal(err)
	}

	names := make([]string, 0, seriesCount)
	for series := 0; series < seriesCount; series++ {
		names = append(names, seriesName(series))
	}

	conn := s.RPCClientConn(t, username.RootUserName())
	client := conn.NewTimeSeriesClient()

	dumpClient, err := client.Dump(ctx, &tspb.DumpRequest{
		Names:      names,
		StartNanos: datapointTimestampNanos(startVal),
		EndNanos:   datapointTimestampNanos(endVal),
	})
	if err != nil {
		t.Fatal(err)
	}

	readDataFromDump := func(t *testing.T, dumpClient tspb.RPCTimeSeries_DumpClient) (totalMsgCount int, _ map[string]map[string]tspb.TimeSeriesData) {
		t.Helper()
		// Read data from dump command.
		resultMap := make(map[string]map[string]tspb.TimeSeriesData)
		for {
			msg, err := dumpClient.Recv()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			sourceMap, ok := resultMap[msg.Name]
			if !ok {
				sourceMap = make(map[string]tspb.TimeSeriesData)
				resultMap[msg.Name] = sourceMap
			}
			if data, ok := sourceMap[msg.Source]; !ok {
				sourceMap[msg.Source] = *msg
			} else {
				data.Datapoints = append(data.Datapoints, msg.Datapoints...)
				sourceMap[msg.Source] = data
			}
			totalMsgCount++
		}
		return totalMsgCount, resultMap
	}

	totalMsgCount, resultMap := readDataFromDump(t, dumpClient)

	if a, e := totalMsgCount, expTotalMsgCount; a != e {
		t.Fatalf("dump returned %d messages, expected %d", a, e)
	}
	if a, e := resultMap, expectedMap; !reflect.DeepEqual(a, e) {
		for _, diff := range pretty.Diff(a, e) {
			t.Error(diff)
		}
	}

	// Verify that when we dump the raw KVs, we get the right number.
	// The next subtest verifies them in detail.
	dumpRawClient, err := client.DumpRaw(ctx, &tspb.DumpRequest{
		Names:      names,
		StartNanos: datapointTimestampNanos(startVal),
		EndNanos:   datapointTimestampNanos(endVal),
	})
	require.NoError(t, err)
	var kvs []*roachpb.KeyValue
	for {
		kv, err := dumpRawClient.Recv()
		if err == io.EOF {
			break
		}
		kvs = append(kvs, kv)
	}
	require.EqualValues(t, expTotalMsgCount, len(kvs))

	s.Stopper().Stop(ctx)

	// Start a new server, into which to write the raw dump.
	s = serverutils.StartServerOnly(t, base.TestServerArgs{
		// For now, direct access to the tsdb is reserved to the storage layer.
		DefaultTestTenant: base.TestIsSpecificToStorageLayerAndNeedsASystemTenant,

		Knobs: base.TestingKnobs{
			Store: &kvserver.StoreTestingKnobs{
				DisableTimeSeriesMaintenanceQueue: true,
			},
		},
	})
	defer s.Stopper().Stop(ctx)

	var b kv.Batch
	for _, kv := range kvs {
		p := kvpb.NewPut(kv.Key, kv.Value)
		p.(*kvpb.PutRequest).Inline = true
		b.AddRawRequest(p)
	}
	// Write and check multiple times, to make sure there aren't any issues
	// when overwriting existing timeseries kv pairs (which are usually written
	// via Merge commands).
	for i := 0; i < 3; i++ {
		require.NoError(t, s.DB().Run(ctx, &b))

		conn := s.RPCClientConn(t, username.RootUserName())
		client := conn.NewTimeSeriesClient()

		dumpClient, err := client.Dump(ctx, &tspb.DumpRequest{
			Names:      names,
			StartNanos: datapointTimestampNanos(startVal),
			EndNanos:   datapointTimestampNanos(endVal),
		})
		if err != nil {
			t.Fatal(err)
		}

		_, resultsMap := readDataFromDump(t, dumpClient)
		require.Equal(t, expectedMap, resultsMap)
	}
}

func BenchmarkServerQuery(b *testing.B) {
	defer log.Scope(b).Close(b)

	s := serverutils.StartServerOnly(b, base.TestServerArgs{})
	defer s.Stopper().Stop(context.Background())

	// Populate data for large number of time series.
	seriesCount := 50
	sourceCount := 10
	if err := populateSeries(seriesCount, sourceCount, 3, s.TsDB().(*ts.DB)); err != nil {
		b.Fatal(err)
	}

	conn := s.RPCClientConn(b, username.RootUserName())
	client := conn.NewTimeSeriesClient()

	queries := make([]tspb.Query, 0, seriesCount)
	for i := 0; i < seriesCount; i++ {
		queries = append(queries, tspb.Query{
			Name: fmt.Sprintf("metric.%d", i),
		})
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := client.Query(context.Background(), &tspb.TimeSeriesQueryRequest{
			StartNanos: 0 * 1e9,
			EndNanos:   500 * 1e9,
			Queries:    queries,
		}); err != nil {
			b.Fatal(err)
		}
	}
}

func seriesName(seriesNum int) string {
	return fmt.Sprintf("metric.%d", seriesNum)
}

func sourceName(sourceNum int) string {
	return fmt.Sprintf("source.%d", sourceNum)
}

func datapointTimestampNanos(val int) int64 {
	return int64(val * 100 * 1e9)
}

func datapointValue(val int) float64 {
	return float64(val * 100)
}

func generateTimeSeriesDatapoints(startValue, endValue int) []tspb.TimeSeriesDatapoint {
	result := make([]tspb.TimeSeriesDatapoint, 0, endValue-startValue)
	for i := startValue; i < endValue; i++ {
		result = append(result, tspb.TimeSeriesDatapoint{
			TimestampNanos: datapointTimestampNanos(i),
			Value:          datapointValue(i),
		})
	}
	return result
}

func populateSeries(seriesCount, sourceCount, valueCount int, tsdb *ts.DB) error {
	for series := 0; series < seriesCount; series++ {
		for source := 0; source < sourceCount; source++ {
			if err := tsdb.StoreData(context.Background(), ts.Resolution10s, []tspb.TimeSeriesData{
				{
					Name:       seriesName(series),
					Source:     sourceName(source),
					Datapoints: generateTimeSeriesDatapoints(0 /* startValue */, valueCount),
				},
			}); err != nil {
				return errors.Wrapf(
					err, "error storing data for series %d, source %d", series, source,
				)
			}
		}
	}
	return nil
}
