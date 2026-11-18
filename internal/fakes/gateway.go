// Package fakes provides in-memory stand-ins for Trino Gateway and a Trino
// coordinator. They are used by unit tests and the K3s suites, so
// no real Trino is needed for development.
package fakes

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"sync"

	"github.com/arturmkr/trino-maintenance-operator/internal/gateway"
)

// Gateway is an in-memory Trino Gateway exposing the backend API.
type Gateway struct {
	mu       sync.Mutex
	backends map[string]gateway.Backend
	// Username/Password, when set, require HTTP basic auth on every request.
	Username, Password string
	// Fail, when set, makes every request answer HTTP 503 (simulates an outage).
	Fail  bool
	calls []string
}

// NewGateway creates a fake gateway pre-populated with backends.
func NewGateway(backends ...gateway.Backend) *Gateway {
	g := &Gateway{backends: map[string]gateway.Backend{}}
	for _, b := range backends {
		g.backends[b.Name] = b
	}
	return g
}

// Set adds or replaces a backend.
func (g *Gateway) Set(b gateway.Backend) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.backends[b.Name] = b
}

// Get returns a backend by name.
func (g *Gateway) Get(name string) (gateway.Backend, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	b, ok := g.backends[name]
	return b, ok
}

// IsActive reports whether the named backend exists and is active.
func (g *Gateway) IsActive(name string) bool {
	b, ok := g.Get(name)
	return ok && b.Active
}

// Calls returns a copy of "METHOD /path" for every request received so far.
func (g *Gateway) Calls() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.calls...)
}

// SetFail toggles outage simulation.
func (g *Gateway) SetFail(fail bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.Fail = fail
}

func (g *Gateway) list(activeOnly bool) []gateway.Backend {
	out := make([]gateway.Backend, 0, len(g.backends))
	for _, b := range g.backends {
		if !activeOnly || b.Active {
			out = append(out, b)
		}
	}
	slices.SortFunc(out, func(a, b gateway.Backend) int { return strings.Compare(a.Name, b.Name) })
	return out
}

func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls = append(g.calls, r.Method+" "+r.URL.Path)
	if g.Fail {
		http.Error(w, "gateway unavailable", http.StatusServiceUnavailable)
		return
	}
	if g.Username != "" {
		u, p, ok := r.BasicAuth()
		if !ok || u != g.Username || p != g.Password {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	}
	path := r.URL.Path
	switch {
	case r.Method == http.MethodPost && path == "/webapp/getAllBackends":
		writeJSON(w, map[string]any{"code": 200, "data": g.list(false)})
	case r.Method == http.MethodGet && path == "/gateway/backend/all":
		writeJSON(w, g.list(false))
	case r.Method == http.MethodGet && path == "/gateway/backend/active":
		writeJSON(w, g.list(true))
	case r.Method == http.MethodPost && strings.HasPrefix(path, "/gateway/backend/activate/"):
		g.setActive(w, strings.TrimPrefix(path, "/gateway/backend/activate/"), true)
	case r.Method == http.MethodPost && strings.HasPrefix(path, "/gateway/backend/deactivate/"):
		g.setActive(w, strings.TrimPrefix(path, "/gateway/backend/deactivate/"), false)
	case r.Method == http.MethodPost && (path == "/gateway/backend/modify/add" || path == "/gateway/backend/modify/update"):
		var b gateway.Backend
		if err := json.NewDecoder(r.Body).Decode(&b); err != nil || b.Name == "" {
			http.Error(w, "invalid backend", http.StatusBadRequest)
			return
		}
		g.backends[b.Name] = b
		writeJSON(w, b)
	default:
		http.NotFound(w, r)
	}
}

func (g *Gateway) setActive(w http.ResponseWriter, name string, active bool) {
	b, ok := g.backends[name]
	if !ok {
		http.Error(w, "backend not found: "+name, http.StatusNotFound)
		return
	}
	b.Active = active
	g.backends[name] = b
	w.WriteHeader(http.StatusOK)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
