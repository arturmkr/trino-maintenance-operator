package trino_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/arturmkr/trino-maintenance-operator/internal/fakes"
	"github.com/arturmkr/trino-maintenance-operator/internal/trino"
)

func TestParseQueryStats(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    trino.QueryStats
		wantErr bool
	}{
		{
			name: "trino exposition",
			input: `# TYPE trino_execution_name_QueryManager_RunningQueries gauge
trino_execution_name_QueryManager_RunningQueries 5.0
# TYPE trino_execution_name_QueryManager_QueuedQueries gauge
trino_execution_name_QueryManager_QueuedQueries 2.0
trino_execution_name_QueryManager_SubmittedQueries 27
trino_execution_name_QueryManager_CompletedQueries 20
# EOF
`,
			want: trino.QueryStats{Running: 5, Queued: 2, Unfinished: 7},
		},
		{
			name: "different package prefix and labels",
			input: `com_starburst_execution_name_QueryManager_RunningQueries{instance="c1"} 1 1700000000
com_starburst_execution_name_QueryManager_QueuedQueries{instance="c1"} 0 1700000000
com_starburst_execution_name_QueryManager_SubmittedQueries{instance="c1"} 9
com_starburst_execution_name_QueryManager_CompletedQueries{instance="c1"} 7
`,
			want: trino.QueryStats{Running: 1, Queued: 0, Unfinished: 2},
		},
		{
			name: "unrelated metrics are ignored",
			input: `io_airlift_http_client_type_HttpClient_name_ForDiscoveryClient_CurrentResponseProcessTime_Min NaN
trino_execution_name_QueryManager_RunningQueries 0.0
trino_execution_name_QueryManager_QueuedQueries 0.0
trino_execution_name_QueryManager_SubmittedQueries 9007199254740993
trino_execution_name_QueryManager_CompletedQueries 9007199254740993
trino_execution_name_QueryManager_RunningQueries_OneMinute_Rate 0.3
`,
			want: trino.QueryStats{},
		},
		{
			name:    "zero gauges alone are not sufficient",
			input:   "trino_execution_name_QueryManager_RunningQueries 0\ntrino_execution_name_QueryManager_QueuedQueries 0\n",
			wantErr: true,
		},
		{
			name:    "missing queued metric",
			input:   "trino_execution_name_QueryManager_RunningQueries 0.0\n",
			wantErr: true,
		},
		{
			name:    "empty body",
			input:   "",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := trino.ParseQueryStats(strings.NewReader(tt.input))
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestHTTPClient_AgainstFakeCoordinator(t *testing.T) {
	fc := fakes.NewCoordinator("482")
	fc.SetQueries(3, 1)
	srv := httptest.NewServer(fc)
	t.Cleanup(srv.Close)
	c := trino.NewHTTPClient(nil)
	coord := trino.Coordinator{URL: srv.URL + "/"}
	ctx := context.Background()

	stats, err := c.QueryStats(ctx, coord)
	if err != nil {
		t.Fatal(err)
	}
	if stats != (trino.QueryStats{Running: 3, Queued: 1, Unfinished: 4}) || stats.Idle() {
		t.Fatalf("stats = %+v", stats)
	}

	info, err := c.Info(ctx, coord)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Coordinator || info.Starting || info.NodeVersion.Version != "482" {
		t.Fatalf("info = %+v", info)
	}

	fc.SetStarting(true)
	if info, err = c.Info(ctx, coord); err != nil || !info.Starting {
		t.Fatalf("expected starting coordinator, info=%+v err=%v", info, err)
	}

	fc.SetDown(true)
	if _, err := c.QueryStats(ctx, coord); err == nil {
		t.Fatal("expected error when coordinator is down")
	}
	if _, err := c.Info(ctx, coord); err == nil {
		t.Fatal("expected error when coordinator is down")
	}
}

func TestHTTPClient_Auth(t *testing.T) {
	var user, pass, trinoUser string
	var hasBasic bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, hasBasic = r.BasicAuth()
		trinoUser = r.Header.Get("X-Trino-User")
		_, _ = w.Write([]byte("trino_execution_name_QueryManager_RunningQueries 0\ntrino_execution_name_QueryManager_QueuedQueries 0\n" +
			"trino_execution_name_QueryManager_SubmittedQueries 1\ntrino_execution_name_QueryManager_CompletedQueries 1\n"))
	}))
	t.Cleanup(srv.Close)
	c := trino.NewHTTPClient(nil)

	if _, err := c.QueryStats(context.Background(), trino.Coordinator{URL: srv.URL}); err != nil {
		t.Fatal(err)
	}
	if hasBasic || trinoUser != trino.DefaultUser {
		t.Fatalf("without credentials: basic=%v X-Trino-User=%q", hasBasic, trinoUser)
	}

	if _, err := c.QueryStats(context.Background(), trino.Coordinator{URL: srv.URL, Username: "ops", Password: "pw"}); err != nil {
		t.Fatal(err)
	}
	if !hasBasic || user != "ops" || pass != "pw" || trinoUser != "ops" {
		t.Fatalf("with credentials: basic=%v user=%q pass=%q X-Trino-User=%q", hasBasic, user, pass, trinoUser)
	}
}
