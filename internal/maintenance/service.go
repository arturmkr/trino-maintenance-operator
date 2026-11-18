// Package maintenance implements the Gateway and coordinator steps of one
// reconciliation. Kubernetes ownership and durable checkpoints live in the controller.
package maintenance

import (
	"context"
	"fmt"
	"strings"

	api "github.com/arturmkr/trino-maintenance-operator/api/v1alpha1"
	"github.com/arturmkr/trino-maintenance-operator/internal/gateway"
	"github.com/arturmkr/trino-maintenance-operator/internal/trino"
)

type Target struct {
	Backend, Group string
	Coordinator    trino.Coordinator
}

type Service struct {
	Gateway gateway.Client
	Trino   trino.Client
}

func NewService(gw gateway.Client, tr trino.Client) *Service { return &Service{gw, tr} }

type Snapshot struct {
	Backend      gateway.Backend
	HealthyPeers int32
}

// Inspect never combines Kubernetes declarations with Gateway membership. A
// misregistered coordinator must not be allowed to prove another backend idle.
func (s *Service) Inspect(ctx context.Context, t Target, minAvailable int32) (Snapshot, error) {
	bs, err := s.Gateway.ListBackends(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	b, ok := gateway.FindBackend(bs, t.Backend)
	if !ok {
		return Snapshot{}, fmt.Errorf("backend %q is not registered in Trino Gateway", t.Backend)
	}
	if b.RoutingGroup != t.Group || strings.TrimRight(b.ProxyTo, "/") != strings.TrimRight(t.Coordinator.URL, "/") {
		return Snapshot{}, fmt.Errorf("backend %q: Gateway routingGroup or proxyTo does not match TrinoCluster", b.Name)
	}
	view := Snapshot{Backend: b}
	for _, peer := range bs {
		if !b.Active || view.HealthyPeers >= minAvailable {
			break
		}
		if peer.Name == b.Name || peer.RoutingGroup != t.Group || !peer.Active || peer.Status != "HEALTHY" {
			continue
		}
		// Gateway's last monitor result can outlive a coordinator. /v1/info
		// is public in Trino; never send the target's credentials to a peer.
		if s.Healthy(ctx, trino.Coordinator{URL: peer.ProxyTo}) == nil {
			view.HealthyPeers++
		}
	}
	return view, nil
}

func (v Snapshot) CanDrain(minAvailable int32) bool {
	return !v.Backend.Active || v.HealthyPeers >= minAvailable
}

type Outcome struct {
	Phase   api.MaintenancePhase
	Message string
	Stats   *trino.QueryStats
}

func Result(phase api.MaintenancePhase, format string, args ...any) Outcome {
	return Outcome{Phase: phase, Message: fmt.Sprintf(format, args...)}
}

// Drain performs at most one Gateway write and always observes OFF on a later
// read before checking queries. verified means the same continuously OFF target
// was already proven idle before a failed rollout.
func (s *Service) Drain(ctx context.Context, t Target, v Snapshot, minAvailable int32, verified bool) Outcome {
	if !v.CanDrain(minAvailable) {
		return Result(api.PhaseWaiting, "only %d healthy peers in %s (minAvailable=%d)", v.HealthyPeers, t.Group, minAvailable)
	}
	if v.Backend.Active {
		if err := s.Gateway.DeactivateBackend(ctx, t.Backend); err != nil {
			return Result(api.PhaseFailed, "deactivating backend: %v", err)
		}
		return Result(api.PhaseDraining, "deactivation sent; waiting to observe backend OFF")
	}
	stats, err := s.Trino.QueryStats(ctx, t.Coordinator)
	if err != nil {
		if verified {
			return Result(api.PhaseDrained, "backend remains OFF after its previously verified drain")
		}
		return Result(api.PhaseDraining, "backend is OFF; waiting for coordinator metrics: %v", err)
	}
	if stats.Idle() {
		return Outcome{api.PhaseDrained, "backend is OFF; no unfinished queries", &stats}
	}
	return Outcome{api.PhaseDraining, fmt.Sprintf("waiting for active queries: %d running, %d queued, %d unfinished", stats.Running, stats.Queued, stats.Unfinished), &stats}
}

// Healthy checks the coordinator before allowing activation. Gateway's own
// health observation is checked separately after the write, before releasing the group.
func (s *Service) Healthy(ctx context.Context, c trino.Coordinator) error {
	info, err := s.Trino.Info(ctx, c)
	if err != nil {
		return err
	}
	if !info.Coordinator || info.Starting {
		return fmt.Errorf("not a ready coordinator (coordinator=%t, starting=%t)", info.Coordinator, info.Starting)
	}
	return nil
}

func (s *Service) Resume(ctx context.Context, t Target, v Snapshot) Outcome {
	if !v.Backend.Active {
		if err := s.Gateway.ActivateBackend(ctx, t.Backend); err != nil {
			return Result(api.PhaseFailed, "activating backend: %v", err)
		}
		return Result(api.PhaseResuming, "activation sent; waiting for Gateway to report active and healthy")
	}
	if v.Backend.Status != "HEALTHY" {
		return Result(api.PhaseResuming, "backend is active; waiting for Gateway health (status %q)", v.Backend.Status)
	}
	return Result(api.PhaseCompleted, "coordinator is ready and backend is routable in Gateway")
}
