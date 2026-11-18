package controller

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crcontroller "sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/recorder"

	api "github.com/arturmkr/trino-maintenance-operator/api/v1alpha1"
	"github.com/arturmkr/trino-maintenance-operator/internal/maintenance"
	"github.com/arturmkr/trino-maintenance-operator/internal/trino"
)

type TrinoMaintenanceReconciler struct {
	client.Client
	// Safety decisions need current API objects, not an eventually consistent informer.
	Reader                        client.Reader
	Recorder                      recorder.EventRecorder
	Maintenance                   *maintenance.Service
	WaitingInterval, PollInterval time.Duration
}

// +kubebuilder:rbac:groups=trino.example.io,resources=trinomaintenances;trinoroutinggroups,verbs=get;list;watch
// +kubebuilder:rbac:groups=trino.example.io,resources=trinomaintenances/status;trinoroutinggroups/status,verbs=update
// +kubebuilder:rbac:groups=trino.example.io,resources=trinoclusters,verbs=get
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

func (r *TrinoMaintenanceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	m := &api.TrinoMaintenance{}
	if err := r.Reader.Get(ctx, req.NamespacedName, m); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !m.DeletionTimestamp.IsZero() || m.Status.Phase.IsTerminal() {
		return ctrl.Result{}, nil
	}
	old := m.Status.DeepCopy()
	out, err := r.step(ctx, m)
	if err != nil {
		return ctrl.Result{}, err
	}
	m.Status.Phase, m.Status.Message, m.Status.ObservedGeneration = out.Phase, out.Message, m.Generation
	m.Status.RunningQueries, m.Status.QueuedQueries, m.Status.UnfinishedQueries = nil, nil, nil
	if out.Stats != nil {
		m.Status.RunningQueries = &out.Stats.Running
		m.Status.QueuedQueries = &out.Stats.Queued
		m.Status.UnfinishedQueries = &out.Stats.Unfinished
	}
	condition := metav1.ConditionFalse
	if out.Phase == api.PhaseDrained || out.Phase == api.PhaseCompleted {
		condition = metav1.ConditionTrue
	}
	meta.SetStatusCondition(&m.Status.Conditions, metav1.Condition{Type: "Ready", Status: condition, Reason: string(out.Phase), Message: out.Message, ObservedGeneration: m.Generation})
	if !equality.Semantic.DeepEqual(*old, m.Status) {
		// Update carries resourceVersion: an old hook cannot patch its replacement by name.
		if err := r.Status().Update(ctx, m); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
		if old.Phase != out.Phase {
			ctrl.LoggerFrom(ctx).Info("Reconciled TrinoMaintenance", "phase", out.Phase, "cluster", m.Spec.ClusterName, "message", out.Message)
			if r.Recorder != nil {
				typ := corev1.EventTypeNormal
				if out.Phase == api.PhaseFailed {
					typ = corev1.EventTypeWarning
				}
				r.Recorder.Eventf(m, nil, typ, string(out.Phase), string(m.Spec.Action), "%s", out.Message)
			}
		}
	}
	switch out.Phase {
	case api.PhaseWaiting:
		return ctrl.Result{RequeueAfter: r.WaitingInterval}, nil
	case api.PhaseDraining, api.PhaseResuming:
		return ctrl.Result{RequeueAfter: r.PollInterval}, nil
	default:
		return ctrl.Result{}, nil
	}
}

func (r *TrinoMaintenanceReconciler) step(ctx context.Context, m *api.TrinoMaintenance) (maintenance.Outcome, error) {
	c := &api.TrinoCluster{}
	key := client.ObjectKey{Namespace: m.Namespace, Name: m.Spec.ClusterName}
	if err := r.Reader.Get(ctx, key, c); err != nil {
		return configError("TrinoCluster "+key.String(), err)
	}
	g := &api.TrinoRoutingGroup{}
	if err := r.Reader.Get(ctx, client.ObjectKey{Name: c.Spec.RoutingGroup}, g); err != nil {
		return configError("TrinoRoutingGroup "+c.Spec.RoutingGroup, err)
	}
	if !c.DeletionTimestamp.IsZero() || !g.DeletionTimestamp.IsZero() {
		return maintenance.Result(api.PhaseFailed, "backend configuration is being deleted"), nil
	}
	t := maintenance.Target{Backend: c.Spec.GatewayBackend, Group: g.Name, Coordinator: trino.Coordinator{URL: c.Spec.Coordinator.URL}}
	if m.Spec.Action == api.ActionResume && m.Status.ActivatedDrainUID != "" {
		return r.finishResume(ctx, g, m)
	}
	if m.Spec.Action == api.ActionDrain && g.Status.ActiveMaintenance != nil && !sameDrain(g.Status.ActiveMaintenance, c, m) {
		o := g.Status.ActiveMaintenance
		return maintenance.Result(api.PhaseWaiting, "routing group %s is under maintenance by %s/%s", g.Name, o.Namespace, o.Name), nil
	}
	if ref := c.Spec.Coordinator.CredentialsSecret; ref != nil {
		secret := &corev1.Secret{}
		if err := r.Reader.Get(ctx, client.ObjectKey{Namespace: c.Namespace, Name: ref.Name}, secret); err != nil {
			return maintenance.Result(api.PhaseFailed, "reading coordinator credentials: %v", err), nil
		}
		t.Coordinator.Username = string(secret.Data["username"])
		t.Coordinator.Password = string(secret.Data["password"])
		if t.Coordinator.Username == "" || t.Coordinator.Password == "" {
			return maintenance.Result(api.PhaseFailed, "coordinator Secret needs username and password"), nil
		}
	}
	var minAvailable int32
	if m.Spec.Action == api.ActionDrain {
		minAvailable = g.Spec.MinAvailable
	}
	v, err := r.Maintenance.Inspect(ctx, t, minAvailable)
	if err != nil {
		return maintenance.Result(api.PhaseFailed, "reading Gateway: %v", err), nil
	}
	switch m.Spec.Action {
	case api.ActionDrain:
		return r.drain(ctx, m, c, g, t, v)
	case api.ActionResume:
		return r.resume(ctx, m, c, g, t, v)
	default:
		return maintenance.Result(api.PhaseFailed, "unknown action %q", m.Spec.Action), nil
	}
}

func configError(resource string, err error) (maintenance.Outcome, error) {
	if apierrors.IsNotFound(err) {
		return maintenance.Result(api.PhaseFailed, "%s not found; refusing maintenance", resource), nil
	}
	return maintenance.Outcome{}, err
}

func sameDrain(o *api.ActiveMaintenance, c *api.TrinoCluster, m *api.TrinoMaintenance) bool {
	return o.ClusterUID == c.UID && o.Namespace == m.Namespace && o.Name == m.Name
}

func (r *TrinoMaintenanceReconciler) drain(ctx context.Context, m *api.TrinoMaintenance, c *api.TrinoCluster, g *api.TrinoRoutingGroup, t maintenance.Target, v maintenance.Snapshot) (maintenance.Outcome, error) {
	owner := g.Status.ActiveMaintenance
	// Do not reserve an unused slot while waiting for enough serving peers. An
	// already-off owner, on the other hand, must be able to repair even below the floor.
	if !v.CanDrain(g.Spec.MinAvailable) {
		out := maintenance.Result(api.PhaseWaiting, "only %d healthy peers in %s (minAvailable=%d)", v.HealthyPeers, g.Name, g.Spec.MinAvailable)
		if owner != nil {
			g.Status.ActiveMaintenance = nil
			return out, r.Status().Update(ctx, g)
		}
		return out, nil
	}
	if owner == nil || owner.DrainUID != m.UID {
		// Hooks have stable names, but every creation has a new UID. Fence off
		// the Resume that already exists, even on a retry of the same Git SHA.
		previous := &api.TrinoMaintenance{}
		err := r.Reader.Get(ctx, client.ObjectKey{Namespace: m.Namespace, Name: c.Name + "-resume"}, previous)
		if client.IgnoreNotFound(err) != nil {
			return maintenance.Outcome{}, err
		}
		g.Status.ActiveMaintenance = &api.ActiveMaintenance{
			Namespace: m.Namespace, Name: m.Name, ClusterName: c.Name,
			ClusterUID: c.UID, DrainUID: m.UID, PreviousResumeUID: previous.UID,
			VerifiedOffline: owner != nil && owner.VerifiedOffline && !v.Backend.Active,
		}
		// Re-inspect Gateway after the CAS and before any write.
		return maintenance.Result(api.PhaseDraining, "acquired routing group; checking Gateway"), r.Status().Update(ctx, g)
	}
	if v.Backend.Active && owner.VerifiedOffline {
		owner.VerifiedOffline = false
		if err := r.Status().Update(ctx, g); err != nil {
			return maintenance.Outcome{}, err
		}
	}
	out := r.Maintenance.Drain(ctx, t, v, g.Spec.MinAvailable, owner.VerifiedOffline)
	// Metrics are a scrape, not an atomic admission barrier. Confirm an idle
	// observation on the next poll rather than rolling immediately on the first.
	if out.Phase == api.PhaseDrained && !owner.VerifiedOffline && !previouslyIdle(m.Status) {
		out.Phase, out.Message = api.PhaseDraining, "first idle observation; confirming on the next poll"
		return out, nil
	}
	verified := out.Phase == api.PhaseDrained
	if verified != owner.VerifiedOffline {
		owner.VerifiedOffline = verified
		if err := r.Status().Update(ctx, g); err != nil {
			return maintenance.Outcome{}, err
		}
	}
	return out, nil
}

func previouslyIdle(s api.TrinoMaintenanceStatus) bool {
	return s.RunningQueries != nil && *s.RunningQueries == 0 && s.QueuedQueries != nil && *s.QueuedQueries == 0 &&
		s.UnfinishedQueries != nil && *s.UnfinishedQueries == 0
}

func (r *TrinoMaintenanceReconciler) resume(ctx context.Context, m *api.TrinoMaintenance, c *api.TrinoCluster, g *api.TrinoRoutingGroup, t maintenance.Target, v maintenance.Snapshot) (maintenance.Outcome, error) {
	o := g.Status.ActiveMaintenance
	if o == nil || o.ClusterUID != c.UID || m.Name != c.Name+"-resume" || o.PreviousResumeUID == m.UID {
		return maintenance.Result(api.PhaseFailed, "Resume does not belong to the current drain"), nil
	}
	if err := r.Maintenance.Healthy(ctx, t.Coordinator); err != nil {
		return maintenance.Result(api.PhaseResuming, "waiting for coordinator: %v", err), nil
	}
	// A slow health probe may straddle BeforeHookCreation. Check both live
	// hook identities after the probe, before invalidating the checkpoint or writing.
	current := &api.TrinoMaintenance{}
	if err := r.Reader.Get(ctx, client.ObjectKeyFromObject(m), current); err != nil {
		return configError("current Resume", err)
	}
	if current.UID != m.UID || !current.DeletionTimestamp.IsZero() {
		return maintenance.Result(api.PhaseFailed, "Resume hook was replaced"), nil
	}
	drain := &api.TrinoMaintenance{}
	if err := r.Reader.Get(ctx, client.ObjectKey{Namespace: o.Namespace, Name: o.Name}, drain); err != nil {
		return configError("owning Drain", err)
	}
	if drain.UID != o.DrainUID || !drain.DeletionTimestamp.IsZero() || drain.Status.Phase != api.PhaseDrained {
		return maintenance.Result(api.PhaseFailed, "owning Drain is not the verified live hook"), nil
	}
	// Persist invalidation BEFORE the POST: a timeout or crash is not proof
	// that Gateway stayed closed. A later Drain must scrape the target again.
	if o.VerifiedOffline {
		o.VerifiedOffline = false
		if err := r.Status().Update(ctx, g); err != nil {
			return maintenance.Outcome{}, err
		}
	}
	out := r.Maintenance.Resume(ctx, t, v)
	if out.Phase == api.PhaseCompleted {
		m.Status.ActivatedDrainUID = o.DrainUID
		return maintenance.Result(api.PhaseResuming, "observed routable backend; releasing group"), nil
	}
	return out, nil
}

func (r *TrinoMaintenanceReconciler) finishResume(ctx context.Context, g *api.TrinoRoutingGroup, m *api.TrinoMaintenance) (maintenance.Outcome, error) {
	if o := g.Status.ActiveMaintenance; o != nil && o.DrainUID == m.Status.ActivatedDrainUID {
		g.Status.ActiveMaintenance = nil
		if err := r.Status().Update(ctx, g); err != nil {
			return maintenance.Outcome{}, err
		}
	}
	return maintenance.Result(api.PhaseCompleted, "backend resumed; routing group released"), nil
}

func (r *TrinoMaintenanceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Maintenance == nil || r.PollInterval <= 0 || r.WaitingInterval <= 0 {
		return fmt.Errorf("maintenance service and positive poll/wait intervals are required")
	}
	r.Reader = mgr.GetAPIReader()
	return ctrl.NewControllerManagedBy(mgr).
		For(&api.TrinoMaintenance{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&api.TrinoRoutingGroup{}, handler.EnqueueRequestsFromMapFunc(r.wakeWaiters)).
		WithOptions(crcontroller.Options{MaxConcurrentReconciles: 1}).
		Complete(r)
}

// Wake waiters promptly on release; the ordinary poll remains the fallback.
// Only scheduling uses cached reads. Admission always uses the live API reader.
func (r *TrinoMaintenanceReconciler) wakeWaiters(ctx context.Context, obj client.Object) []ctrl.Request {
	if obj.(*api.TrinoRoutingGroup).Status.ActiveMaintenance != nil {
		return nil
	}
	var list api.TrinoMaintenanceList
	if err := r.List(ctx, &list); err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "Could not list waiting TrinoMaintenance objects")
		return nil
	}
	var requests []ctrl.Request
	for i := range list.Items {
		m := &list.Items[i]
		if m.Status.Phase == api.PhaseWaiting {
			requests = append(requests, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(m)})
		}
	}
	return requests
}
