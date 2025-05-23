// Copyright 2019 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package executor_test

import (
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"github.com/pingcap/failpoint"
	"github.com/pingcap/fn"
	"github.com/pingcap/sysutil"
	"github.com/pingcap/tidb/pkg/executor"
	"github.com/pingcap/tidb/pkg/testkit"
	pmodel "github.com/prometheus/common/model"
	"github.com/stretchr/testify/require"
	pd "github.com/tikv/pd/client/http"
	"google.golang.org/grpc"
)

func TestMetricTableData(t *testing.T) {
	store := testkit.CreateMockStore(t)

	fpName := "github.com/pingcap/tidb/pkg/executor/mockMetricsPromData"
	require.NoError(t, failpoint.Enable(fpName, "return"))
	defer func() { require.NoError(t, failpoint.Disable(fpName)) }()

	// mock prometheus data
	matrix := pmodel.Matrix{}
	metric := map[pmodel.LabelName]pmodel.LabelValue{
		"instance": "127.0.0.1:10080",
	}
	tt, err := time.ParseInLocation("2006-01-02 15:04:05.999", "2019-12-23 20:11:35", time.Local)
	require.NoError(t, err)
	v1 := pmodel.SamplePair{
		Timestamp: pmodel.Time(tt.UnixMilli()),
		Value:     pmodel.SampleValue(0.1),
	}
	matrix = append(matrix, &pmodel.SampleStream{Metric: metric, Values: []pmodel.SamplePair{v1}})

	ctx := context.WithValue(context.Background(), executor.MockMetricsPromDataKey{}, matrix)
	ctx = failpoint.WithHook(ctx, func(ctx context.Context, fpname string) bool {
		return fpname == fpName
	})

	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use metrics_schema")

	cases := []struct {
		sql string
		exp []string
	}{
		{
			sql: "select time,instance,quantile,value from tidb_query_duration;",
			exp: []string{
				"2019-12-23 20:11:35.000000 127.0.0.1:10080 0.9 0.1",
			},
		},
		{
			sql: "select time,instance,quantile,value from tidb_query_duration where quantile in (0.85, 0.95);",
			exp: []string{
				"2019-12-23 20:11:35.000000 127.0.0.1:10080 0.85 0.1",
				"2019-12-23 20:11:35.000000 127.0.0.1:10080 0.95 0.1",
			},
		},
		{
			sql: "select time,instance,quantile,value from tidb_query_duration where quantile=0.5",
			exp: []string{
				"2019-12-23 20:11:35.000000 127.0.0.1:10080 0.5 0.1",
			},
		},
	}

	for _, cas := range cases {
		rs, err := tk.Session().Execute(ctx, cas.sql)
		require.NoError(t, err)
		result := tk.ResultSetToResultWithCtx(ctx, rs[0], fmt.Sprintf("sql: %s", cas.sql))
		result.Check(testkit.Rows(cas.exp...))
	}
}

func TestTiDBClusterConfig(t *testing.T) {
	store := testkit.CreateMockStore(t)

	// mock PD http server
	router := mux.NewRouter()

	type mockServer struct {
		address string
		server  *httptest.Server
	}
	const testServerCount = 3
	testServers := make([]*mockServer, 0, testServerCount)
	for range testServerCount {
		server := httptest.NewServer(router)
		address := strings.TrimPrefix(server.URL, "http://")
		testServers = append(testServers, &mockServer{
			address: address,
			server:  server,
		})
	}
	defer func() {
		for _, server := range testServers {
			server.server.Close()
		}
	}()

	// We check the counter to valid how many times request has been sent
	var requestCounter int32
	var mockConfig = func() (map[string]any, error) {
		atomic.AddInt32(&requestCounter, 1)
		configuration := map[string]any{
			"key1": "value1",
			"key2": map[string]string{
				"nest1": "n-value1",
				"nest2": "n-value2",
			},
			// We need hide the follow config
			// TODO: we need remove it when index usage is GA.
			"performance": map[string]string{
				"index-usage-sync-lease": "0s",
				"INDEX-USAGE-SYNC-LEASE": "0s",
			},
			"enable-batch-dml": "false",
			"prepared-plan-cache": map[string]string{
				"enabled": "true",
			},
		}
		return configuration, nil
	}

	// pd config
	router.Handle(pd.Config, fn.Wrap(mockConfig))
	// TiDB/TiKV config
	router.Handle("/config", fn.Wrap(mockConfig))
	// Tiproxy config
	router.Handle("/api/admin/config", fn.Wrap(mockConfig))
	// TSO config
	router.Handle("/tso/api/v1/config", fn.Wrap(mockConfig))
	// Scheduling config
	router.Handle("/scheduling/api/v1/config", fn.Wrap(mockConfig))

	// mock servers
	var servers []string
	for _, typ := range []string{"tidb", "tikv", "tiflash", "tiproxy", "pd", "tso", "scheduling"} {
		for _, server := range testServers {
			servers = append(servers, strings.Join([]string{typ, server.address, server.address}, ","))
		}
	}

	fpName := "github.com/pingcap/tidb/pkg/executor/mockClusterConfigServerInfo"
	fpExpr := strings.Join(servers, ";")
	require.NoError(t, failpoint.Enable(fpName, fmt.Sprintf(`return("%s")`, fpExpr)))
	defer func() { require.NoError(t, failpoint.Disable(fpName)) }()

	tk := testkit.NewTestKit(t, store)
	tk.MustQuery("select type, `key`, value from information_schema.cluster_config").Check(testkit.Rows(
		"tidb key1 value1",
		"tidb key2.nest1 n-value1",
		"tidb key2.nest2 n-value2",
		"tidb key1 value1",
		"tidb key2.nest1 n-value1",
		"tidb key2.nest2 n-value2",
		"tidb key1 value1",
		"tidb key2.nest1 n-value1",
		"tidb key2.nest2 n-value2",
		"tikv key1 value1",
		"tikv key2.nest1 n-value1",
		"tikv key2.nest2 n-value2",
		"tikv key1 value1",
		"tikv key2.nest1 n-value1",
		"tikv key2.nest2 n-value2",
		"tikv key1 value1",
		"tikv key2.nest1 n-value1",
		"tikv key2.nest2 n-value2",
		"tiflash key1 value1",
		"tiflash key2.nest1 n-value1",
		"tiflash key2.nest2 n-value2",
		"tiflash key1 value1",
		"tiflash key2.nest1 n-value1",
		"tiflash key2.nest2 n-value2",
		"tiflash key1 value1",
		"tiflash key2.nest1 n-value1",
		"tiflash key2.nest2 n-value2",
		"tiproxy key1 value1",
		"tiproxy key2.nest1 n-value1",
		"tiproxy key2.nest2 n-value2",
		"tiproxy key1 value1",
		"tiproxy key2.nest1 n-value1",
		"tiproxy key2.nest2 n-value2",
		"tiproxy key1 value1",
		"tiproxy key2.nest1 n-value1",
		"tiproxy key2.nest2 n-value2",
		"pd key1 value1",
		"pd key2.nest1 n-value1",
		"pd key2.nest2 n-value2",
		"pd key1 value1",
		"pd key2.nest1 n-value1",
		"pd key2.nest2 n-value2",
		"pd key1 value1",
		"pd key2.nest1 n-value1",
		"pd key2.nest2 n-value2",
		"tso key1 value1",
		"tso key2.nest1 n-value1",
		"tso key2.nest2 n-value2",
		"tso key1 value1",
		"tso key2.nest1 n-value1",
		"tso key2.nest2 n-value2",
		"tso key1 value1",
		"tso key2.nest1 n-value1",
		"tso key2.nest2 n-value2",
		"scheduling key1 value1",
		"scheduling key2.nest1 n-value1",
		"scheduling key2.nest2 n-value2",
		"scheduling key1 value1",
		"scheduling key2.nest1 n-value1",
		"scheduling key2.nest2 n-value2",
		"scheduling key1 value1",
		"scheduling key2.nest1 n-value1",
		"scheduling key2.nest2 n-value2",
	))
	warnings := tk.Session().GetSessionVars().StmtCtx.GetWarnings()
	require.Len(t, warnings, 0, fmt.Sprintf("unexpected warnings: %+v", warnings))
	require.Equal(t, int32(21), requestCounter)

	// TODO: we need remove it when index usage is GA.
	rs := tk.MustQuery("show config").Rows()
	for _, r := range rs {
		s, ok := r[2].(string)
		require.True(t, ok)
		require.NotContains(t, s, "index-usage-sync-lease")
		require.NotContains(t, s, "INDEX-USAGE-SYNC-LEASE")
		// Should not contain deprecated items
		// https://github.com/pingcap/tidb/issues/34867
		require.NotContains(t, s, "enable-batch-dml")
		require.NotContains(t, s, "prepared-plan-cache.enabled")
	}

	// type => server index => row
	rows := map[string][][]string{}
	for _, typ := range []string{"tidb", "tikv", "tiflash", "tiproxy", "pd", "tso", "scheduling"} {
		for _, server := range testServers {
			rows[typ] = append(rows[typ], []string{
				fmt.Sprintf("%s %s key1 value1", typ, server.address),
				fmt.Sprintf("%s %s key2.nest1 n-value1", typ, server.address),
				fmt.Sprintf("%s %s key2.nest2 n-value2", typ, server.address),
			})
		}
	}
	var flatten = func(ss ...[]string) []string {
		var result []string
		for _, xs := range ss {
			result = append(result, xs...)
		}
		return result
	}
	var cases = []struct {
		sql      string
		reqCount int32
		rows     []string
	}{
		{
			sql:      "select * from information_schema.cluster_config",
			reqCount: 21,
			rows: flatten(
				rows["tidb"][0],
				rows["tidb"][1],
				rows["tidb"][2],
				rows["tikv"][0],
				rows["tikv"][1],
				rows["tikv"][2],
				rows["tiflash"][0],
				rows["tiflash"][1],
				rows["tiflash"][2],
				rows["tiproxy"][0],
				rows["tiproxy"][1],
				rows["tiproxy"][2],
				rows["pd"][0],
				rows["pd"][1],
				rows["pd"][2],
				rows["tso"][0],
				rows["tso"][1],
				rows["tso"][2],
				rows["scheduling"][0],
				rows["scheduling"][1],
				rows["scheduling"][2],
			),
		},
		{
			sql:      "select * from information_schema.cluster_config where type='pd' or type='tikv'",
			reqCount: 6,
			rows: flatten(
				rows["tikv"][0],
				rows["tikv"][1],
				rows["tikv"][2],
				rows["pd"][0],
				rows["pd"][1],
				rows["pd"][2],
			),
		},
		{
			sql:      "select * from information_schema.cluster_config where type='pd' or instance='" + testServers[0].address + "'",
			reqCount: 21,
			rows: flatten(
				rows["tidb"][0],
				rows["tikv"][0],
				rows["tiflash"][0],
				rows["tiproxy"][0],
				rows["pd"][0],
				rows["pd"][1],
				rows["pd"][2],
				rows["tso"][0],
				rows["scheduling"][0],
			),
		},
		{
			sql:      "select * from information_schema.cluster_config where type='pd' and type='tikv'",
			reqCount: 0,
		},
		{
			sql:      "select * from information_schema.cluster_config where type='tikv'",
			reqCount: 3,
			rows: flatten(
				rows["tikv"][0],
				rows["tikv"][1],
				rows["tikv"][2],
			),
		},
		{
			sql:      "select * from information_schema.cluster_config where type='pd'",
			reqCount: 3,
			rows: flatten(
				rows["pd"][0],
				rows["pd"][1],
				rows["pd"][2],
			),
		},
		{
			sql:      "select * from information_schema.cluster_config where type='tidb'",
			reqCount: 3,
			rows: flatten(
				rows["tidb"][0],
				rows["tidb"][1],
				rows["tidb"][2],
			),
		},
		{
			sql:      "select * from information_schema.cluster_config where 'tidb'=type",
			reqCount: 3,
			rows: flatten(
				rows["tidb"][0],
				rows["tidb"][1],
				rows["tidb"][2],
			),
		},
		{
			sql:      "select * from information_schema.cluster_config where type in ('tidb', 'tikv')",
			reqCount: 6,
			rows: flatten(
				rows["tidb"][0],
				rows["tidb"][1],
				rows["tidb"][2],
				rows["tikv"][0],
				rows["tikv"][1],
				rows["tikv"][2],
			),
		},
		{
			sql:      "select * from information_schema.cluster_config where type in ('tidb', 'tikv', 'pd')",
			reqCount: 9,
			rows: flatten(
				rows["tidb"][0],
				rows["tidb"][1],
				rows["tidb"][2],
				rows["tikv"][0],
				rows["tikv"][1],
				rows["tikv"][2],
				rows["pd"][0],
				rows["pd"][1],
				rows["pd"][2],
			),
		},
		{
			sql: fmt.Sprintf(`select * from information_schema.cluster_config where instance='%s'`,
				testServers[0].address),
			reqCount: 7,
			rows: flatten(
				rows["tidb"][0],
				rows["tikv"][0],
				rows["tiflash"][0],
				rows["tiproxy"][0],
				rows["pd"][0],
				rows["tso"][0],
				rows["scheduling"][0],
			),
		},
		{
			sql: fmt.Sprintf(`select * from information_schema.cluster_config where type='tidb' and instance='%s'`,
				testServers[0].address),
			reqCount: 1,
			rows: flatten(
				rows["tidb"][0],
			),
		},
		{
			sql: fmt.Sprintf(`select * from information_schema.cluster_config where type in ('tidb', 'tikv') and instance='%s'`,
				testServers[0].address),
			reqCount: 2,
			rows: flatten(
				rows["tidb"][0],
				rows["tikv"][0],
			),
		},
		{
			sql: fmt.Sprintf(`select * from information_schema.cluster_config where type in ('tidb', 'tikv') and instance in ('%s', '%s')`,
				testServers[0].address, testServers[0].address),
			reqCount: 2,
			rows: flatten(
				rows["tidb"][0],
				rows["tikv"][0],
			),
		},
		{
			sql: fmt.Sprintf(`select * from information_schema.cluster_config where type in ('tidb', 'tikv') and instance in ('%s', '%s')`,
				testServers[0].address, testServers[1].address),
			reqCount: 4,
			rows: flatten(
				rows["tidb"][0],
				rows["tidb"][1],
				rows["tikv"][0],
				rows["tikv"][1],
			),
		},
		{
			sql: fmt.Sprintf(`select * from information_schema.cluster_config where type in ('tidb', 'tikv') and type='pd' and instance in ('%s', '%s')`,
				testServers[0].address, testServers[1].address),
			reqCount: 0,
		},
		{
			sql: fmt.Sprintf(`select * from information_schema.cluster_config where type in ('tidb', 'tikv') and instance in ('%s', '%s') and instance='%s'`,
				testServers[0].address, testServers[1].address, testServers[2].address),
			reqCount: 0,
		},
		{
			sql: fmt.Sprintf(`select * from information_schema.cluster_config where type in ('tidb', 'tikv') and instance in ('%s', '%s') and instance='%s'`,
				testServers[0].address, testServers[1].address, testServers[0].address),
			reqCount: 2,
			rows: flatten(
				rows["tidb"][0],
				rows["tikv"][0],
			),
		},
	}

	for _, ca := range cases {
		// reset the request counter
		requestCounter = 0
		tk.MustQuery(ca.sql).Check(testkit.Rows(ca.rows...))
		warnings := tk.Session().GetSessionVars().StmtCtx.GetWarnings()
		require.Len(t, warnings, 0, fmt.Sprintf("unexpected warnigns: %+v", warnings))
		require.Equal(t, ca.reqCount, requestCounter, fmt.Sprintf("SQL: %s", ca.sql))
	}
}

func writeTmpFile(t *testing.T, dir, filename string, lines []string) {
	err := os.WriteFile(filepath.Join(dir, filename), []byte(strings.Join(lines, "\n")), os.ModePerm)
	require.NoError(t, err, fmt.Sprintf("write tmp file %s failed", filename))
}

type testServer struct {
	typ     string
	server  *grpc.Server
	address string
	tmpDir  string
	logFile string
}

func TestTiDBClusterLog(t *testing.T) {
	testServers := createClusterGRPCServer(t)
	defer func() {
		for _, s := range testServers {
			s.server.Stop()
			require.NoError(t, os.RemoveAll(s.tmpDir), fmt.Sprintf("remove tmpDir %v failed", s.tmpDir))
		}
	}()

	// time format of log file
	var logtime = func(s string) string {
		tt, err := time.ParseInLocation("2006/01/02 15:04:05.000", s, time.Local)
		require.NoError(t, err)
		return tt.Format("[2006/01/02 15:04:05.000 -07:00]")
	}

	// time format of query output
	var restime = func(s string) string {
		tt, err := time.ParseInLocation("2006/01/02 15:04:05.000", s, time.Local)
		require.NoError(t, err)
		return tt.Format("2006/01/02 15:04:05.000")
	}

	// prepare log files
	// TiDB
	writeTmpFile(t, testServers["tidb"].tmpDir, "tidb.log", []string{
		logtime(`2019/08/26 06:19:13.011`) + ` [INFO] [test log message tidb 1, foo]`,
		logtime(`2019/08/26 06:19:14.011`) + ` [DEBUG] [test log message tidb 2, foo]`,
		logtime(`2019/08/26 06:19:15.011`) + ` [error] [test log message tidb 3, foo]`,
		logtime(`2019/08/26 06:19:16.011`) + ` [trace] [test log message tidb 4, foo]`,
		logtime(`2019/08/26 06:19:17.011`) + ` [CRITICAL] [test log message tidb 5, foo]`,
	})
	writeTmpFile(t, testServers["tidb"].tmpDir, "tidb-1.log", []string{
		logtime(`2019/08/26 06:25:13.011`) + ` [info] [test log message tidb 10, bar]`,
		logtime(`2019/08/26 06:25:14.011`) + ` [debug] [test log message tidb 11, bar]`,
		logtime(`2019/08/26 06:25:15.011`) + ` [ERROR] [test log message tidb 12, bar]`,
		logtime(`2019/08/26 06:25:16.011`) + ` [TRACE] [test log message tidb 13, bar]`,
		logtime(`2019/08/26 06:25:17.011`) + ` [critical] [test log message tidb 14, bar]`,
	})

	// TiKV
	writeTmpFile(t, testServers["tikv"].tmpDir, "tikv.log", []string{
		logtime(`2019/08/26 06:19:13.011`) + ` [INFO] [test log message tikv 1, foo]`,
		logtime(`2019/08/26 06:20:14.011`) + ` [DEBUG] [test log message tikv 2, foo]`,
		logtime(`2019/08/26 06:21:15.011`) + ` [error] [test log message tikv 3, foo]`,
		logtime(`2019/08/26 06:22:16.011`) + ` [trace] [test log message tikv 4, foo]`,
		logtime(`2019/08/26 06:23:17.011`) + ` [CRITICAL] [test log message tikv 5, foo]`,
	})
	writeTmpFile(t, testServers["tikv"].tmpDir, "tikv-1.log", []string{
		logtime(`2019/08/26 06:24:15.011`) + ` [info] [test log message tikv 10, bar]`,
		logtime(`2019/08/26 06:25:16.011`) + ` [debug] [test log message tikv 11, bar]`,
		logtime(`2019/08/26 06:26:17.011`) + ` [ERROR] [test log message tikv 12, bar]`,
		logtime(`2019/08/26 06:27:18.011`) + ` [TRACE] [test log message tikv 13, bar]`,
		logtime(`2019/08/26 06:28:19.011`) + ` [critical] [test log message tikv 14, bar]`,
	})

	// TiProxy
	writeTmpFile(t, testServers["tiproxy"].tmpDir, "tiproxy.log", []string{
		logtime(`2019/08/26 06:19:13.011`) + ` [INFO] [test log message tiproxy 1, foo]`,
		logtime(`2019/08/26 06:20:14.011`) + ` [DEBUG] [test log message tiproxy 2, foo]`,
		logtime(`2019/08/26 06:21:15.011`) + ` [error] [test log message tiproxy 3, foo]`,
		logtime(`2019/08/26 06:22:16.011`) + ` [trace] [test log message tiproxy 4, foo]`,
		logtime(`2019/08/26 06:23:17.011`) + ` [CRITICAL] [test log message tiproxy 5, foo]`,
	})
	writeTmpFile(t, testServers["tiproxy"].tmpDir, "tiproxy-1.log", []string{
		logtime(`2019/08/26 06:24:15.011`) + ` [info] [test log message tiproxy 10, bar]`,
		logtime(`2019/08/26 06:25:16.011`) + ` [debug] [test log message tiproxy 11, bar]`,
		logtime(`2019/08/26 06:26:17.011`) + ` [ERROR] [test log message tiproxy 12, bar]`,
		logtime(`2019/08/26 06:27:18.011`) + ` [TRACE] [test log message tiproxy 13, bar]`,
		logtime(`2019/08/26 06:28:19.011`) + ` [critical] [test log message tiproxy 14, bar]`,
	})

	// TiCDC
	writeTmpFile(t, testServers["ticdc"].tmpDir, "ticdc.log", []string{
		logtime(`2019/08/26 06:19:13.011`) + ` [INFO] [test log message ticdc 1, foo]`,
		logtime(`2019/08/26 06:20:14.011`) + ` [DEBUG] [test log message ticdc 2, foo]`,
		logtime(`2019/08/26 06:21:15.011`) + ` [error] [test log message ticdc 3, foo]`,
		logtime(`2019/08/26 06:22:16.011`) + ` [trace] [test log message ticdc 4, foo]`,
		logtime(`2019/08/26 06:23:17.011`) + ` [CRITICAL] [test log message ticdc 5, foo]`,
	})
	writeTmpFile(t, testServers["ticdc"].tmpDir, "ticdc-1.log", []string{
		logtime(`2019/08/26 06:24:15.011`) + ` [info] [test log message ticdc 10, bar]`,
		logtime(`2019/08/26 06:25:16.011`) + ` [debug] [test log message ticdc 11, bar]`,
		logtime(`2019/08/26 06:26:17.011`) + ` [ERROR] [test log message ticdc 12, bar]`,
		logtime(`2019/08/26 06:27:18.011`) + ` [TRACE] [test log message ticdc 13, bar]`,
		logtime(`2019/08/26 06:28:19.011`) + ` [critical] [test log message ticdc 14, bar]`,
	})

	// PD
	writeTmpFile(t, testServers["pd"].tmpDir, "pd.log", []string{
		logtime(`2019/08/26 06:18:13.011`) + ` [INFO] [test log message pd 1, foo]`,
		logtime(`2019/08/26 06:19:14.011`) + ` [DEBUG] [test log message pd 2, foo]`,
		logtime(`2019/08/26 06:20:15.011`) + ` [error] [test log message pd 3, foo]`,
		logtime(`2019/08/26 06:21:16.011`) + ` [trace] [test log message pd 4, foo]`,
		logtime(`2019/08/26 06:22:17.011`) + ` [CRITICAL] [test log message pd 5, foo]`,
	})
	writeTmpFile(t, testServers["pd"].tmpDir, "pd-1.log", []string{
		logtime(`2019/08/26 06:23:13.011`) + ` [info] [test log message pd 10, bar]`,
		logtime(`2019/08/26 06:24:14.011`) + ` [debug] [test log message pd 11, bar]`,
		logtime(`2019/08/26 06:25:15.011`) + ` [ERROR] [test log message pd 12, bar]`,
		logtime(`2019/08/26 06:26:16.011`) + ` [TRACE] [test log message pd 13, bar]`,
		logtime(`2019/08/26 06:27:17.011`) + ` [critical] [test log message pd 14, bar]`,
	})

	fullLogs := [][]string{
		{"2019/08/26 06:18:13.011", "pd", "INFO", "[test log message pd 1, foo]"},
		{"2019/08/26 06:19:13.011", "ticdc", "INFO", "[test log message ticdc 1, foo]"},
		{"2019/08/26 06:19:13.011", "tidb", "INFO", "[test log message tidb 1, foo]"},
		{"2019/08/26 06:19:13.011", "tikv", "INFO", "[test log message tikv 1, foo]"},
		{"2019/08/26 06:19:13.011", "tiproxy", "INFO", "[test log message tiproxy 1, foo]"},
		{"2019/08/26 06:19:14.011", "pd", "DEBUG", "[test log message pd 2, foo]"},
		{"2019/08/26 06:19:14.011", "tidb", "DEBUG", "[test log message tidb 2, foo]"},
		{"2019/08/26 06:19:15.011", "tidb", "error", "[test log message tidb 3, foo]"},
		{"2019/08/26 06:19:16.011", "tidb", "trace", "[test log message tidb 4, foo]"},
		{"2019/08/26 06:19:17.011", "tidb", "CRITICAL", "[test log message tidb 5, foo]"},
		{"2019/08/26 06:20:14.011", "ticdc", "DEBUG", "[test log message ticdc 2, foo]"},
		{"2019/08/26 06:20:14.011", "tikv", "DEBUG", "[test log message tikv 2, foo]"},
		{"2019/08/26 06:20:14.011", "tiproxy", "DEBUG", "[test log message tiproxy 2, foo]"},
		{"2019/08/26 06:20:15.011", "pd", "error", "[test log message pd 3, foo]"},
		{"2019/08/26 06:21:15.011", "ticdc", "error", "[test log message ticdc 3, foo]"},
		{"2019/08/26 06:21:15.011", "tikv", "error", "[test log message tikv 3, foo]"},
		{"2019/08/26 06:21:15.011", "tiproxy", "error", "[test log message tiproxy 3, foo]"},
		{"2019/08/26 06:21:16.011", "pd", "trace", "[test log message pd 4, foo]"},
		{"2019/08/26 06:22:16.011", "ticdc", "trace", "[test log message ticdc 4, foo]"},
		{"2019/08/26 06:22:16.011", "tikv", "trace", "[test log message tikv 4, foo]"},
		{"2019/08/26 06:22:16.011", "tiproxy", "trace", "[test log message tiproxy 4, foo]"},
		{"2019/08/26 06:22:17.011", "pd", "CRITICAL", "[test log message pd 5, foo]"},
		{"2019/08/26 06:23:13.011", "pd", "info", "[test log message pd 10, bar]"},
		{"2019/08/26 06:23:17.011", "ticdc", "CRITICAL", "[test log message ticdc 5, foo]"},
		{"2019/08/26 06:23:17.011", "tikv", "CRITICAL", "[test log message tikv 5, foo]"},
		{"2019/08/26 06:23:17.011", "tiproxy", "CRITICAL", "[test log message tiproxy 5, foo]"},
		{"2019/08/26 06:24:14.011", "pd", "debug", "[test log message pd 11, bar]"},
		{"2019/08/26 06:24:15.011", "ticdc", "info", "[test log message ticdc 10, bar]"},
		{"2019/08/26 06:24:15.011", "tikv", "info", "[test log message tikv 10, bar]"},
		{"2019/08/26 06:24:15.011", "tiproxy", "info", "[test log message tiproxy 10, bar]"},
		{"2019/08/26 06:25:13.011", "tidb", "info", "[test log message tidb 10, bar]"},
		{"2019/08/26 06:25:14.011", "tidb", "debug", "[test log message tidb 11, bar]"},
		{"2019/08/26 06:25:15.011", "pd", "ERROR", "[test log message pd 12, bar]"},
		{"2019/08/26 06:25:15.011", "tidb", "ERROR", "[test log message tidb 12, bar]"},
		{"2019/08/26 06:25:16.011", "ticdc", "debug", "[test log message ticdc 11, bar]"},
		{"2019/08/26 06:25:16.011", "tidb", "TRACE", "[test log message tidb 13, bar]"},
		{"2019/08/26 06:25:16.011", "tikv", "debug", "[test log message tikv 11, bar]"},
		{"2019/08/26 06:25:16.011", "tiproxy", "debug", "[test log message tiproxy 11, bar]"},
		{"2019/08/26 06:25:17.011", "tidb", "critical", "[test log message tidb 14, bar]"},
		{"2019/08/26 06:26:16.011", "pd", "TRACE", "[test log message pd 13, bar]"},
		{"2019/08/26 06:26:17.011", "ticdc", "ERROR", "[test log message ticdc 12, bar]"},
		{"2019/08/26 06:26:17.011", "tikv", "ERROR", "[test log message tikv 12, bar]"},
		{"2019/08/26 06:26:17.011", "tiproxy", "ERROR", "[test log message tiproxy 12, bar]"},
		{"2019/08/26 06:27:17.011", "pd", "critical", "[test log message pd 14, bar]"},
		{"2019/08/26 06:27:18.011", "ticdc", "TRACE", "[test log message ticdc 13, bar]"},
		{"2019/08/26 06:27:18.011", "tikv", "TRACE", "[test log message tikv 13, bar]"},
		{"2019/08/26 06:27:18.011", "tiproxy", "TRACE", "[test log message tiproxy 13, bar]"},
		{"2019/08/26 06:28:19.011", "ticdc", "critical", "[test log message ticdc 14, bar]"},
		{"2019/08/26 06:28:19.011", "tikv", "critical", "[test log message tikv 14, bar]"},
		{"2019/08/26 06:28:19.011", "tiproxy", "critical", "[test log message tiproxy 14, bar]"},
	}

	var cases = []struct {
		conditions []string
		expected   [][]string
	}{
		{
			conditions: []string{
				"time>='2019/08/26 06:18:13.011'",
				"time<='2099/08/26 06:28:19.011'",
				"message like '%'",
			},
			expected: fullLogs,
		},
		{
			conditions: []string{
				"time>='2019/08/26 06:19:13.011'",
				"time<='2019/08/26 06:21:15.011'",
				"message like '%'",
			},
			expected: [][]string{
				{"2019/08/26 06:19:13.011", "ticdc", "INFO", "[test log message ticdc 1, foo]"},
				{"2019/08/26 06:19:13.011", "tidb", "INFO", "[test log message tidb 1, foo]"},
				{"2019/08/26 06:19:13.011", "tikv", "INFO", "[test log message tikv 1, foo]"},
				{"2019/08/26 06:19:13.011", "tiproxy", "INFO", "[test log message tiproxy 1, foo]"},
				{"2019/08/26 06:19:14.011", "pd", "DEBUG", "[test log message pd 2, foo]"},
				{"2019/08/26 06:19:14.011", "tidb", "DEBUG", "[test log message tidb 2, foo]"},
				{"2019/08/26 06:19:15.011", "tidb", "error", "[test log message tidb 3, foo]"},
				{"2019/08/26 06:19:16.011", "tidb", "trace", "[test log message tidb 4, foo]"},
				{"2019/08/26 06:19:17.011", "tidb", "CRITICAL", "[test log message tidb 5, foo]"},
				{"2019/08/26 06:20:14.011", "ticdc", "DEBUG", "[test log message ticdc 2, foo]"},
				{"2019/08/26 06:20:14.011", "tikv", "DEBUG", "[test log message tikv 2, foo]"},
				{"2019/08/26 06:20:14.011", "tiproxy", "DEBUG", "[test log message tiproxy 2, foo]"},
				{"2019/08/26 06:20:15.011", "pd", "error", "[test log message pd 3, foo]"},
				{"2019/08/26 06:21:15.011", "ticdc", "error", "[test log message ticdc 3, foo]"},
				{"2019/08/26 06:21:15.011", "tikv", "error", "[test log message tikv 3, foo]"},
				{"2019/08/26 06:21:15.011", "tiproxy", "error", "[test log message tiproxy 3, foo]"},
			},
		},
		{
			conditions: []string{
				"time>='2019/08/26 06:19:13.011'",
				"time<='2019/08/26 06:21:15.011'",
				"message like '%'",
				"type='pd'",
			},
			expected: [][]string{
				{"2019/08/26 06:19:14.011", "pd", "DEBUG", "[test log message pd 2, foo]"},
				{"2019/08/26 06:20:15.011", "pd", "error", "[test log message pd 3, foo]"},
			},
		},
		{
			conditions: []string{
				"time>='2019/08/26 06:18:13.011'",
				"time>='2019/08/26 06:19:13.011'",
				"time>='2019/08/26 06:19:14.011'",
				"time<='2019/08/26 06:21:15.011'",
				"type='pd'",
			},
			expected: [][]string{
				{"2019/08/26 06:19:14.011", "pd", "DEBUG", "[test log message pd 2, foo]"},
				{"2019/08/26 06:20:15.011", "pd", "error", "[test log message pd 3, foo]"},
			},
		},
		{
			conditions: []string{
				"time>='2019/08/26 06:18:13.011'",
				"time>='2019/08/26 06:19:13.011'",
				"time='2019/08/26 06:19:14.011'",
				"message like '%'",
				"type='pd'",
			},
			expected: [][]string{
				{"2019/08/26 06:19:14.011", "pd", "DEBUG", "[test log message pd 2, foo]"},
			},
		},
		{
			conditions: []string{
				"time>='2019/08/26 06:19:13.011'",
				"time<='2019/08/26 06:21:15.011'",
				"message like '%'",
				"type='tidb'",
			},
			expected: [][]string{
				{"2019/08/26 06:19:13.011", "tidb", "INFO", "[test log message tidb 1, foo]"},
				{"2019/08/26 06:19:14.011", "tidb", "DEBUG", "[test log message tidb 2, foo]"},
				{"2019/08/26 06:19:15.011", "tidb", "error", "[test log message tidb 3, foo]"},
				{"2019/08/26 06:19:16.011", "tidb", "trace", "[test log message tidb 4, foo]"},
				{"2019/08/26 06:19:17.011", "tidb", "CRITICAL", "[test log message tidb 5, foo]"},
			},
		},
		{
			conditions: []string{
				"time>='2019/08/26 06:19:13.011'",
				"time<='2019/08/26 06:21:15.011'",
				"message like '%'",
				"type='tikv'",
			},
			expected: [][]string{
				{"2019/08/26 06:19:13.011", "tikv", "INFO", "[test log message tikv 1, foo]"},
				{"2019/08/26 06:20:14.011", "tikv", "DEBUG", "[test log message tikv 2, foo]"},
				{"2019/08/26 06:21:15.011", "tikv", "error", "[test log message tikv 3, foo]"},
			},
		},
		{
			conditions: []string{
				"time>='2019/08/26 06:19:13.011'",
				"time<='2019/08/26 06:21:15.011'",
				"message like '%'",
				fmt.Sprintf("instance='%s'", testServers["pd"].address),
			},
			expected: [][]string{
				{"2019/08/26 06:19:14.011", "pd", "DEBUG", "[test log message pd 2, foo]"},
				{"2019/08/26 06:20:15.011", "pd", "error", "[test log message pd 3, foo]"},
			},
		},
		{
			conditions: []string{
				"time>='2019/08/26 06:19:13.011'",
				"time<='2019/08/26 06:21:15.011'",
				"message like '%'",
				fmt.Sprintf("instance='%s'", testServers["tidb"].address),
			},
			expected: [][]string{
				{"2019/08/26 06:19:13.011", "tidb", "INFO", "[test log message tidb 1, foo]"},
				{"2019/08/26 06:19:14.011", "tidb", "DEBUG", "[test log message tidb 2, foo]"},
				{"2019/08/26 06:19:15.011", "tidb", "error", "[test log message tidb 3, foo]"},
				{"2019/08/26 06:19:16.011", "tidb", "trace", "[test log message tidb 4, foo]"},
				{"2019/08/26 06:19:17.011", "tidb", "CRITICAL", "[test log message tidb 5, foo]"},
			},
		},
		{
			conditions: []string{
				"time>='2019/08/26 06:19:13.011'",
				"time<='2019/08/26 06:21:15.011'",
				"message like '%'",
				fmt.Sprintf("instance='%s'", testServers["tikv"].address),
			},
			expected: [][]string{
				{"2019/08/26 06:19:13.011", "tikv", "INFO", "[test log message tikv 1, foo]"},
				{"2019/08/26 06:20:14.011", "tikv", "DEBUG", "[test log message tikv 2, foo]"},
				{"2019/08/26 06:21:15.011", "tikv", "error", "[test log message tikv 3, foo]"},
			},
		},
		{
			conditions: []string{
				"time>='2019/08/26 06:19:13.011'",
				"time<='2019/08/26 06:21:15.011'",
				"message like '%'",
				fmt.Sprintf("instance in ('%s', '%s')", testServers["pd"].address, testServers["tidb"].address),
			},
			expected: [][]string{
				{"2019/08/26 06:19:13.011", "tidb", "INFO", "[test log message tidb 1, foo]"},
				{"2019/08/26 06:19:14.011", "pd", "DEBUG", "[test log message pd 2, foo]"},
				{"2019/08/26 06:19:14.011", "tidb", "DEBUG", "[test log message tidb 2, foo]"},
				{"2019/08/26 06:19:15.011", "tidb", "error", "[test log message tidb 3, foo]"},
				{"2019/08/26 06:19:16.011", "tidb", "trace", "[test log message tidb 4, foo]"},
				{"2019/08/26 06:19:17.011", "tidb", "CRITICAL", "[test log message tidb 5, foo]"},
				{"2019/08/26 06:20:15.011", "pd", "error", "[test log message pd 3, foo]"},
			},
		},
		{
			conditions: []string{
				"time>='2019/08/26 06:18:13.011'",
				"time<='2019/08/26 06:28:19.011'",
				"message like '%'",
				"level='critical'",
			},
			expected: [][]string{
				{"2019/08/26 06:19:17.011", "tidb", "CRITICAL", "[test log message tidb 5, foo]"},
				{"2019/08/26 06:22:17.011", "pd", "CRITICAL", "[test log message pd 5, foo]"},
				{"2019/08/26 06:23:17.011", "ticdc", "CRITICAL", "[test log message ticdc 5, foo]"},
				{"2019/08/26 06:23:17.011", "tikv", "CRITICAL", "[test log message tikv 5, foo]"},
				{"2019/08/26 06:23:17.011", "tiproxy", "CRITICAL", "[test log message tiproxy 5, foo]"},
				{"2019/08/26 06:25:17.011", "tidb", "critical", "[test log message tidb 14, bar]"},
				{"2019/08/26 06:27:17.011", "pd", "critical", "[test log message pd 14, bar]"},
				{"2019/08/26 06:28:19.011", "ticdc", "critical", "[test log message ticdc 14, bar]"},
				{"2019/08/26 06:28:19.011", "tikv", "critical", "[test log message tikv 14, bar]"},
				{"2019/08/26 06:28:19.011", "tiproxy", "critical", "[test log message tiproxy 14, bar]"},
			},
		},
		{
			conditions: []string{
				"time>='2019/08/26 06:18:13.011'",
				"time<='2019/08/26 06:28:19.011'",
				"message like '%'",
				"level='critical'",
				"type in ('pd', 'tikv')",
			},
			expected: [][]string{
				{"2019/08/26 06:22:17.011", "pd", "CRITICAL", "[test log message pd 5, foo]"},
				{"2019/08/26 06:23:17.011", "tikv", "CRITICAL", "[test log message tikv 5, foo]"},
				{"2019/08/26 06:27:17.011", "pd", "critical", "[test log message pd 14, bar]"},
				{"2019/08/26 06:28:19.011", "tikv", "critical", "[test log message tikv 14, bar]"},
			},
		},
		{
			conditions: []string{
				"time>='2019/08/26 06:18:13.011'",
				"time<='2019/08/26 06:28:19.011'",
				"message like '%'",
				"level='critical'",
				"(type='pd' or type='tikv')",
			},
			expected: [][]string{
				{"2019/08/26 06:22:17.011", "pd", "CRITICAL", "[test log message pd 5, foo]"},
				{"2019/08/26 06:23:17.011", "tikv", "CRITICAL", "[test log message tikv 5, foo]"},
				{"2019/08/26 06:27:17.011", "pd", "critical", "[test log message pd 14, bar]"},
				{"2019/08/26 06:28:19.011", "tikv", "critical", "[test log message tikv 14, bar]"},
			},
		},
		{
			conditions: []string{
				"time>='2019/08/26 06:18:13.011'",
				"time<='2019/08/26 06:28:19.011'",
				"level='critical'",
				"message like '%pd%'",
			},
			expected: [][]string{
				{"2019/08/26 06:22:17.011", "pd", "CRITICAL", "[test log message pd 5, foo]"},
				{"2019/08/26 06:27:17.011", "pd", "critical", "[test log message pd 14, bar]"},
			},
		},
		{
			conditions: []string{
				"time>='2019/08/26 06:18:13.011'",
				"time<='2019/08/26 06:28:19.011'",
				"level='critical'",
				"message like '%pd%'",
				"message like '%5%'",
			},
			expected: [][]string{
				{"2019/08/26 06:22:17.011", "pd", "CRITICAL", "[test log message pd 5, foo]"},
			},
		},
		{
			conditions: []string{
				"time>='2019/08/26 06:18:13.011'",
				"time<='2019/08/26 06:28:19.011'",
				"level='critical'",
				"message like '%pd%'",
				"message like '%5%'",
				"message like '%x%'",
			},
			expected: [][]string{},
		},
		{
			conditions: []string{
				"time>='2019/08/26 06:18:13.011'",
				"time<='2019/08/26 06:28:19.011'",
				"level='critical'",
				"message regexp '.*pd.*'",
			},
			expected: [][]string{
				{"2019/08/26 06:22:17.011", "pd", "CRITICAL", "[test log message pd 5, foo]"},
				{"2019/08/26 06:27:17.011", "pd", "critical", "[test log message pd 14, bar]"},
			},
		},
		{
			conditions: []string{
				"time>='2019/08/26 06:18:13.011'",
				"time<='2019/08/26 06:28:19.011'",
				"level='critical'",
				"message regexp '.*pd.*'",
				"message regexp '.*foo]$'",
			},
			expected: [][]string{
				{"2019/08/26 06:22:17.011", "pd", "CRITICAL", "[test log message pd 5, foo]"},
			},
		},
		{
			conditions: []string{
				"time>='2019/08/26 06:18:13.011'",
				"time<='2019/08/26 06:28:19.011'",
				"level='critical'",
				"message regexp '.*pd.*'",
				"message regexp '.*5.*'",
				"message regexp '.*x.*'",
			},
			expected: [][]string{},
		},
		{
			conditions: []string{
				"time>='2019/08/26 06:18:13.011'",
				"time<='2019/08/26 06:28:19.011'",
				"level='critical'",
				"(message regexp '.*pd.*' or message regexp '.*tidb.*')",
			},
			expected: [][]string{
				{"2019/08/26 06:19:17.011", "tidb", "CRITICAL", "[test log message tidb 5, foo]"},
				{"2019/08/26 06:22:17.011", "pd", "CRITICAL", "[test log message pd 5, foo]"},
				{"2019/08/26 06:25:17.011", "tidb", "critical", "[test log message tidb 14, bar]"},
				{"2019/08/26 06:27:17.011", "pd", "critical", "[test log message pd 14, bar]"},
			},
		},
		{
			conditions: []string{
				"time>='2019/08/26 06:18:13.011'",
				"time<='2099/08/26 06:28:19.011'",
				// this pattern verifies that there is no optimization breaking
				// length of multiple wildcards, for example, %% may be
				// converted to %, but %_ cannot be converted to %.
				"message like '%tidb_%_4%'",
			},
			expected: [][]string{
				{"2019/08/26 06:25:17.011", "tidb", "critical", "[test log message tidb 14, bar]"},
			},
		},
	}

	var servers = make([]string, 0, len(testServers))
	for _, s := range testServers {
		servers = append(servers, strings.Join([]string{s.typ, s.address, s.address}, ","))
	}
	fpName := "github.com/pingcap/tidb/pkg/executor/mockClusterLogServerInfo"
	fpExpr := strings.Join(servers, ";")
	require.NoError(t, failpoint.Enable(fpName, fmt.Sprintf(`return("%s")`, fpExpr)))
	defer func() { require.NoError(t, failpoint.Disable(fpName)) }()

	store := testkit.CreateMockStore(t)
	tk := testkit.NewTestKit(t, store)
	for _, cas := range cases {
		sql := "select * from information_schema.cluster_log"
		if len(cas.conditions) > 0 {
			sql = fmt.Sprintf("%s where %s", sql, strings.Join(cas.conditions, " and "))
		}
		result := tk.MustQuery(sql)
		warnings := tk.Session().GetSessionVars().StmtCtx.GetWarnings()
		require.Len(t, warnings, 0, fmt.Sprintf("unexpected warnigns: %+v", warnings))
		var expected []string
		for _, row := range cas.expected {
			expectedRow := []string{
				restime(row[0]),             // time column
				row[1],                      // type column
				testServers[row[1]].address, // instance column
				strings.ToUpper(sysutil.ParseLogLevel(row[2]).String()), // level column
				row[3], // message column
			}
			expected = append(expected, strings.Join(expectedRow, " "))
		}
		result.Check(testkit.Rows(expected...))
	}
}

func TestTiDBClusterLogError(t *testing.T) {
	store := testkit.CreateMockStore(t)
	tk := testkit.NewTestKit(t, store)
	fpName := "github.com/pingcap/tidb/pkg/executor/mockClusterLogServerInfo"
	require.NoError(t, failpoint.Enable(fpName, `return("")`))
	defer func() { require.NoError(t, failpoint.Disable(fpName)) }()

	// Test without start time error.
	err := tk.QueryToErr("select * from information_schema.cluster_log")
	require.EqualError(t, err, "denied to scan logs, please specified the start time, such as `time > '2020-01-01 00:00:00'`")

	// Test without end time error.
	err = tk.QueryToErr("select * from information_schema.cluster_log where time>='2019/08/26 06:18:13.011'")
	require.EqualError(t, err, "denied to scan logs, please specified the end time, such as `time < '2020-01-01 00:00:00'`")

	// Test without specified message error.
	err = tk.QueryToErr("select * from information_schema.cluster_log where time>='2019/08/26 06:18:13.011' and time<'2019/08/26 16:18:13.011'")
	require.EqualError(t, err, "denied to scan full logs (use `SELECT * FROM cluster_log WHERE message LIKE '%'` explicitly if intentionally)")
}
