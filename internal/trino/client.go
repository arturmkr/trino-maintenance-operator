// Package trino is a minimal client for the Trino coordinator HTTP API: the
// OpenMetrics /metrics endpoint (query counts) and /v1/info (health).
package trino

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// DefaultUser is sent as X-Trino-User when no credentials are configured.
// Trino requires a user even on unsecured deployments.
const DefaultUser = "trino-maintenance-operator"

// Coordinator identifies one coordinator and how to authenticate against it.
type Coordinator struct {
	URL      string
	Username string
	Password string
}

// QueryStats are the query counters relevant for draining.
type QueryStats struct {
	Running int64
	Queued  int64
	// Unfinished includes planning, dispatching and finishing queries that
	// neither gauge counts. Submitted and completed are process-lifetime totals.
	Unfinished int64
}

func (s QueryStats) Idle() bool { return s.Running == 0 && s.Queued == 0 && s.Unfinished == 0 }

// Info is the subset of GET /v1/info the operator uses.
type Info struct {
	NodeVersion struct {
		Version string `json:"version"`
	} `json:"nodeVersion"`
	Environment string `json:"environment"`
	Coordinator bool   `json:"coordinator"`
	Starting    bool   `json:"starting"`
	Uptime      string `json:"uptime"`
}

// Client is the subset of the coordinator API the operator needs.
type Client interface {
	// QueryStats reads running/queued query counts from /metrics.
	QueryStats(ctx context.Context, c Coordinator) (QueryStats, error)
	// Info reads /v1/info. A coordinator is healthy when Info succeeds,
	// Coordinator is true and Starting is false.
	Info(ctx context.Context, c Coordinator) (Info, error)
}

// HTTPClient talks to coordinators over HTTP.
type HTTPClient struct {
	http *http.Client
}

// NewHTTPClient returns a client; a nil http.Client selects a 15s-timeout default.
func NewHTTPClient(hc *http.Client) *HTTPClient {
	if hc == nil {
		hc = &http.Client{Timeout: 15 * time.Second}
	}
	return &HTTPClient{http: hc}
}

func (h *HTTPClient) QueryStats(ctx context.Context, c Coordinator) (QueryStats, error) {
	body, err := h.get(ctx, c, "/metrics", "application/openmetrics-text, text/plain")
	if err != nil {
		return QueryStats{}, err
	}
	defer func() { _ = body.Close() }()
	return ParseQueryStats(body)
}

func (h *HTTPClient) Info(ctx context.Context, c Coordinator) (Info, error) {
	body, err := h.get(ctx, c, "/v1/info", "application/json")
	if err != nil {
		return Info{}, err
	}
	defer func() { _ = body.Close() }()
	var wire struct {
		Info
		Starting *bool `json:"starting"`
	}
	if err := json.NewDecoder(body).Decode(&wire); err != nil {
		return Info{}, fmt.Errorf("decoding /v1/info from %s: %w", c.URL, err)
	}
	if wire.Starting == nil {
		return Info{}, fmt.Errorf("coordinator %s did not report startup state", c.URL)
	}
	wire.Info.Starting = *wire.Starting
	return wire.Info, nil
}

func (h *HTTPClient) get(ctx context.Context, c Coordinator, path, accept string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(c.URL, "/")+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", accept)
	if c.Username != "" {
		req.SetBasicAuth(c.Username, c.Password)
		req.Header.Set("X-Trino-User", c.Username)
	} else {
		req.Header.Set("X-Trino-User", DefaultUser)
	}
	resp, err := h.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("calling coordinator %s%s: %w", c.URL, path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		_ = resp.Body.Close()
		return nil, fmt.Errorf("coordinator %s%s returned HTTP %d: %s", c.URL, path, resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return resp.Body, nil
}

// ParseQueryStats extracts QueryManager gauges and submitted/completed totals from an
// OpenMetrics / Prometheus text exposition.
func ParseQueryStats(r io.Reader) (QueryStats, error) {
	var stats QueryStats
	var submitted, completed int64
	want := map[string]*int64{
		"RunningQueries": &stats.Running, "QueuedQueries": &stats.Queued,
		"SubmittedQueries": &submitted, "CompletedQueries": &completed,
	}
	seen := map[string]bool{}
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 4<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		end := strings.IndexAny(line, "{ \t")
		if end < 0 {
			continue
		}
		_, name, ok := strings.Cut(line[:end], "_QueryManager_")
		dst := want[name]
		if !ok || dst == nil {
			continue
		}
		if seen[name] {
			return QueryStats{}, fmt.Errorf("ambiguous QueryManager metric %s", name)
		}
		rest := line[end:]
		if rest[0] == '{' {
			_, rest, ok = strings.Cut(rest, "}")
		}
		fields := strings.Fields(rest)
		if !ok || len(fields) == 0 {
			return QueryStats{}, fmt.Errorf("invalid sample for %s", name)
		}
		// Airlift emits gauges as doubles (possibly in scientific notation)
		// and lifetime counters as exact decimal longs. Never round a counter.
		n, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			f, ferr := strconv.ParseFloat(fields[0], 64)
			if ferr != nil || !(f >= 0 && f < 1<<53) || float64(int64(f)) != f {
				return QueryStats{}, fmt.Errorf("invalid query count for %s", name)
			}
			n = int64(f)
		}
		if n < 0 {
			return QueryStats{}, fmt.Errorf("negative query count for %s", name)
		}
		*dst, seen[name] = n, true
	}
	if err := scanner.Err(); err != nil {
		return QueryStats{}, fmt.Errorf("reading metrics: %w", err)
	}
	for name := range want {
		if !seen[name] {
			return QueryStats{}, fmt.Errorf("missing QueryManager metric %s", name)
		}
	}
	if completed > submitted {
		return QueryStats{}, fmt.Errorf("inconsistent query totals: %d completed, %d submitted", completed, submitted)
	}
	stats.Unfinished = submitted - completed
	return stats, nil
}
