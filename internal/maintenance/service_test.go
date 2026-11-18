package maintenance

import (
	"context"
	"errors"
	"testing"

	api "github.com/arturmkr/trino-maintenance-operator/api/v1alpha1"
	"github.com/arturmkr/trino-maintenance-operator/internal/gateway"
	"github.com/arturmkr/trino-maintenance-operator/internal/trino"
)

type stubGateway struct {
	backends []gateway.Backend
	writes   []string
	writeErr error
}

func (g *stubGateway) ListBackends(context.Context) ([]gateway.Backend, error) {
	return g.backends, nil
}
func (g *stubGateway) DeactivateBackend(context.Context, string) error {
	g.writes = append(g.writes, "off")
	return g.writeErr
}
func (g *stubGateway) ActivateBackend(context.Context, string) error {
	g.writes = append(g.writes, "on")
	return g.writeErr
}

type stubTrino struct {
	stats trino.QueryStats
	err   error
	info  trino.Info
}

func (t stubTrino) QueryStats(context.Context, trino.Coordinator) (trino.QueryStats, error) {
	return t.stats, t.err
}
func (t stubTrino) Info(context.Context, trino.Coordinator) (trino.Info, error) { return t.info, t.err }

var target = Target{Backend: "one", Group: "general", Coordinator: trino.Coordinator{URL: "http://one"}}

func TestAvailability(t *testing.T) {
	backend := gateway.Backend{Name: "one", Active: true, ProxyTo: "http://one/", RoutingGroup: "general"}
	peer := func(name, group, status string, active bool) gateway.Backend {
		return gateway.Backend{Name: name, RoutingGroup: group, Status: status, Active: active}
	}
	gw := &stubGateway{backends: []gateway.Backend{backend, peer("two", "general", "HEALTHY", true), peer("three", "general", "UNHEALTHY", true), peer("etl", "etl", "HEALTHY", true), peer("off", "general", "HEALTHY", false)}}
	s := NewService(gw, stubTrino{info: trino.Info{Coordinator: true}})
	ctx := t.Context()
	view, err := s.Inspect(ctx, target, 2)
	if err != nil || view.HealthyPeers != 1 || view.CanDrain(2) {
		t.Fatalf("unsafe peer count: %+v %v", view, err)
	}
	gw.backends[2].Status = "HEALTHY"
	view, err = s.Inspect(ctx, target, 2)
	if err != nil || !view.CanDrain(2) {
		t.Fatalf("two healthy peers should suffice: %v", err)
	}
	gw.backends[0].RoutingGroup = "etl"
	if _, err = s.Inspect(ctx, target, 2); err == nil {
		t.Fatal("wrong group accepted")
	}
	gw.backends[0] = backend
	gw.backends[0].ProxyTo = "http://someone-else"
	if _, err = s.Inspect(ctx, target, 2); err == nil {
		t.Fatal("wrong coordinator accepted")
	}
}
func TestDrain(t *testing.T) {
	failed := errors.New("unavailable")
	for _, tc := range []struct {
		name                 string
		active, verified     bool
		peers                int32
		stats                trino.QueryStats
		metricsErr, writeErr error
		phase                api.MaintenancePhase
		writes               int
	}{
		{name: "deactivate once, observe OFF on next reconcile", active: true, peers: 2, phase: api.PhaseDraining, writes: 1},
		{name: "never reduce capacity below floor", active: true, peers: 1, phase: api.PhaseWaiting},
		{name: "already OFF below floor, continue repair", phase: api.PhaseDrained},
		{name: "running", stats: trino.QueryStats{Running: 5}, phase: api.PhaseDraining},
		{name: "queued", stats: trino.QueryStats{Queued: 2}, phase: api.PhaseDraining},
		{name: "planning or finishing", stats: trino.QueryStats{Unfinished: 1}, phase: api.PhaseDraining},
		{name: "unknown is never zero", metricsErr: failed, phase: api.PhaseDraining},
		{name: "crashed rollout, previous proven drain", verified: true, metricsErr: failed, phase: api.PhaseDrained},
		{name: "positive observation overrides old receipt", verified: true, stats: trino.QueryStats{Unfinished: 1}, phase: api.PhaseDraining},
		{name: "ambiguous write not retried blindly", active: true, peers: 2, writeErr: failed, phase: api.PhaseFailed, writes: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := &stubGateway{writeErr: tc.writeErr}
			s := NewService(g, stubTrino{stats: tc.stats, err: tc.metricsErr})
			out := s.Drain(t.Context(), target, Snapshot{Backend: gateway.Backend{Active: tc.active}, HealthyPeers: tc.peers}, 2, tc.verified)
			if out.Phase != tc.phase || len(g.writes) != tc.writes {
				t.Fatalf("got %+v, writes %v", out, g.writes)
			}
			if tc.metricsErr != nil && out.Stats != nil {
				t.Fatal("invented query counts")
			}
		})
	}
}
func TestResumeWaitsForGateway(t *testing.T) {
	s := NewService(&stubGateway{}, stubTrino{})
	for _, tc := range []struct {
		status string
		phase  api.MaintenancePhase
	}{{"PENDING", api.PhaseResuming}, {"UNKNOWN", api.PhaseResuming}, {"HEALTHY", api.PhaseCompleted}} {
		out := s.Resume(t.Context(), target, Snapshot{Backend: gateway.Backend{Active: true, Status: tc.status}})
		if out.Phase != tc.phase {
			t.Fatalf("%s: %+v", tc.status, out)
		}
	}
}
