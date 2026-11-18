package fakes

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// Coordinator is an in-memory Trino coordinator exposing /metrics (OpenMetrics)
// and /v1/info. Query counts can be changed at runtime, either from Go or via
// the /fake/queries?running=N&queued=M endpoint used by the Argo CD test.
type Coordinator struct {
	mu       sync.Mutex
	running  int64
	queued   int64
	other    int64 // unfinished, but neither RUNNING nor QUEUED
	starting bool
	version  string
	// Down, when set, makes every request answer HTTP 503 (coordinator unreachable).
	down bool
	// Username/Password, when set, require HTTP basic auth on /metrics.
	Username, Password string
}

// NewCoordinator creates a healthy coordinator with no queries.
func NewCoordinator(version string) *Coordinator {
	return &Coordinator{version: version}
}

// SetQueries sets the running and queued query counts.
func (c *Coordinator) SetQueries(running, queued int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.running, c.queued = running, queued
}

// SetStarting toggles the `starting` flag reported by /v1/info.
func (c *Coordinator) SetStarting(starting bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.starting = starting
}

// SetDown toggles outage simulation.
func (c *Coordinator) SetDown(down bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.down = down
}

// SetVersion changes the version reported by /v1/info.
func (c *Coordinator) SetVersion(v string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.version = v
}

func (c *Coordinator) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if r.URL.Path == "/fake/queries" {
		c.admin(w, r)
		return
	}
	if c.down {
		http.Error(w, "coordinator down", http.StatusServiceUnavailable)
		return
	}
	switch r.URL.Path {
	case "/metrics":
		if c.Username != "" {
			u, p, ok := r.BasicAuth()
			if !ok || u != c.Username || p != c.Password {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		w.Header().Set("Content-Type", "application/openmetrics-text; version=1.0.0; charset=utf-8")
		var b strings.Builder
		b.WriteString("# TYPE io_airlift_http_client_type_HttpClient_name_ForDiscoveryClient_CurrentResponseProcessTime_Min gauge\n")
		b.WriteString("io_airlift_http_client_type_HttpClient_name_ForDiscoveryClient_CurrentResponseProcessTime_Min NaN\n")
		b.WriteString("# TYPE trino_execution_name_QueryManager_RunningQueries gauge\n")
		fmt.Fprintf(&b, "trino_execution_name_QueryManager_RunningQueries %d.0\n", c.running)
		b.WriteString("# TYPE trino_execution_name_QueryManager_QueuedQueries gauge\n")
		fmt.Fprintf(&b, "trino_execution_name_QueryManager_QueuedQueries %d.0\n", c.queued)
		fmt.Fprintf(&b, "trino_execution_name_QueryManager_SubmittedQueries %d\n", 100+c.running+c.queued+c.other)
		b.WriteString("trino_execution_name_QueryManager_CompletedQueries 100\n")
		b.WriteString("# EOF\n")
		_, _ = io.WriteString(w, b.String())
	case "/v1/info":
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"nodeVersion": map[string]string{"version": c.version},
			"environment": "fake",
			"coordinator": true,
			"starting":    c.starting,
			"uptime":      "1.00m",
		})
	default:
		http.NotFound(w, r)
	}
}

// admin handles /fake/queries: GET or POST with optional running, queued,
// starting and down query parameters. It stays reachable while the fake is "down"
// so that e2e tests can bring it back. Caller holds the lock.
func (c *Coordinator) admin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		http.Error(w, "GET or POST only", http.StatusMethodNotAllowed)
		return
	}
	q := r.URL.Query()
	var err error
	if v := q.Get("running"); v != "" {
		if c.running, err = strconv.ParseInt(v, 10, 64); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	if v := q.Get("queued"); v != "" {
		if c.queued, err = strconv.ParseInt(v, 10, 64); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	if v := q.Get("starting"); v != "" {
		c.starting = v == "true"
	}
	if v := q.Get("other"); v != "" {
		if c.other, err = strconv.ParseInt(v, 10, 64); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	if v := q.Get("down"); v != "" {
		c.down = v == "true"
	}
	_, _ = fmt.Fprintf(w, "running=%d queued=%d starting=%t down=%t\n", c.running, c.queued, c.starting, c.down)
}
