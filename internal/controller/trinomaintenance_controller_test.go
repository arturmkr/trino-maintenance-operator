//go:build integration

/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	trinov1alpha1 "github.com/arturmkr/trino-maintenance-operator/api/v1alpha1"
	"github.com/arturmkr/trino-maintenance-operator/internal/fakes"
	"github.com/arturmkr/trino-maintenance-operator/internal/gateway"
)

const (
	eventually = 10 * time.Second
	tick       = 50 * time.Millisecond
)

// fixture creates a namespace, a routing group and clusters backed by fake
// coordinators, all registered as active backends in the fake gateway.
type fixture struct {
	namespace string
	group     string
	clusters  []string
}

func newFixture(prefix string, minAvailable int32, clusters ...string) *fixture {
	f := &fixture{namespace: prefix + "-ns", group: prefix + "-group", clusters: clusters}
	Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: f.namespace}})).To(Succeed())
	Expect(k8sClient.Create(ctx, &trinov1alpha1.TrinoRoutingGroup{
		ObjectMeta: metav1.ObjectMeta{Name: f.group},
		Spec:       trinov1alpha1.TrinoRoutingGroupSpec{MinAvailable: minAvailable},
	})).To(Succeed())
	for _, name := range clusters {
		f.addCluster(name)
	}
	return f
}

func (f *fixture) addCluster(name string) {
	_, url := newCoordinator(f.namespace + "/" + name)
	f.register(name, url)
}

func (f *fixture) register(name, url string) {
	fakeGateway.Set(gateway.Backend{Name: f.backend(name), ProxyTo: url, Active: true, RoutingGroup: f.group, Status: "HEALTHY"})
	Expect(k8sClient.Create(ctx, &trinov1alpha1.TrinoCluster{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: f.namespace},
		Spec: trinov1alpha1.TrinoClusterSpec{
			RoutingGroup:   f.group,
			GatewayBackend: f.backend(name),
			Coordinator:    trinov1alpha1.CoordinatorSpec{URL: url},
			Workload: &trinov1alpha1.TrinoClusterWorkload{
				Coordinator: trinov1alpha1.WorkloadReference{Kind: trinov1alpha1.WorkloadKindDeployment, Name: name + "-coordinator"},
			},
		},
	})).To(Succeed())
}

func (f *fixture) backend(cluster string) string { return f.namespace + "-" + cluster }

func (f *fixture) coordinator(cluster string) *fakes.Coordinator {
	return coordinators[f.namespace+"/"+cluster]
}

func (f *fixture) maintenance(cluster string, action trinov1alpha1.MaintenanceAction) *trinov1alpha1.TrinoMaintenance {
	suffix := "-drain"
	if action == trinov1alpha1.ActionResume {
		suffix = "-resume"
	}
	m := &trinov1alpha1.TrinoMaintenance{
		ObjectMeta: metav1.ObjectMeta{Name: cluster + suffix, Namespace: f.namespace},
		Spec:       trinov1alpha1.TrinoMaintenanceSpec{ClusterName: cluster, Action: action},
	}
	Expect(k8sClient.Create(ctx, m)).To(Succeed())
	return m
}

func (f *fixture) routingGroup() *trinov1alpha1.TrinoRoutingGroup {
	g := &trinov1alpha1.TrinoRoutingGroup{}
	Expect(k8sClient.Get(ctx, types.NamespacedName{Name: f.group}, g)).To(Succeed())
	return g
}

func (f *fixture) owner() *trinov1alpha1.ActiveMaintenance {
	return f.routingGroup().Status.ActiveMaintenance
}

func (f *fixture) ownerName() string {
	if o := f.owner(); o != nil {
		return o.Name
	}
	return ""
}

func (f *fixture) deleteMaintenance(m *trinov1alpha1.TrinoMaintenance) {
	Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, m))).To(Succeed())
}

func get(m *trinov1alpha1.TrinoMaintenance) *trinov1alpha1.TrinoMaintenance {
	cur := &trinov1alpha1.TrinoMaintenance{}
	Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(m), cur)).To(Succeed())
	return cur
}

func phase(m *trinov1alpha1.TrinoMaintenance) func() trinov1alpha1.MaintenancePhase {
	return func() trinov1alpha1.MaintenancePhase { return get(m).Status.Phase }
}

func message(m *trinov1alpha1.TrinoMaintenance) func() string {
	return func() string { return get(m).Status.Message }
}

func expectReady(m *trinov1alpha1.TrinoMaintenance, status metav1.ConditionStatus, reason string) {
	GinkgoHelper()
	c := meta.FindStatusCondition(get(m).Status.Conditions, "Ready")
	Expect(c).NotTo(BeNil())
	Expect(c.Status).To(Equal(status))
	Expect(c.Reason).To(Equal(reason))
}

var _ = Describe("TrinoMaintenance Controller", func() {
	Describe("Drain", func() {
		It("deactivates the backend, waits for queries and reaches Drained", func() {
			f := newFixture("drain", 2, "general-1", "general-2", "general-3")
			f.coordinator("general-1").SetQueries(5, 1)

			m := f.maintenance("general-1", trinov1alpha1.ActionDrain)

			By("entering Draining while queries are running")
			Eventually(message(m), eventually, tick).Should(ContainSubstring("5 running, 1 queued"))
			cur := get(m)
			Expect(*cur.Status.RunningQueries).To(Equal(int64(5)))
			Expect(*cur.Status.QueuedQueries).To(Equal(int64(1)))
			Expect(cur.Status.Message).To(ContainSubstring("5 running, 1 queued"))
			Expect(fakeGateway.IsActive(f.backend("general-1"))).To(BeFalse(), "backend must be deactivated")
			Expect(fakeGateway.IsActive(f.backend("general-2"))).To(BeTrue())
			Expect(f.owner()).NotTo(BeNil())
			Expect(f.owner().Name).To(Equal("general-1-drain"))
			Expect(f.owner().ClusterName).To(Equal("general-1"))
			expectReady(m, metav1.ConditionFalse, "Draining")

			By("staying Draining while queued queries remain")
			f.coordinator("general-1").SetQueries(0, 1)
			Consistently(phase(m), 3*pollInterval, tick).Should(Equal(trinov1alpha1.PhaseDraining))

			By("reaching Drained once nothing runs")
			f.coordinator("general-1").SetQueries(0, 0)
			Eventually(phase(m), eventually, tick).Should(Equal(trinov1alpha1.PhaseDrained))
			cur = get(m)
			Expect(*cur.Status.RunningQueries).To(BeZero())
			Expect(*cur.Status.QueuedQueries).To(BeZero())
			Expect(cur.Status.ObservedGeneration).To(Equal(cur.Generation))
			expectReady(m, metav1.ConditionTrue, "Drained")

			By("keeping the routing group owned until Resume")
			Consistently(f.ownerName, 3*waitingInterval, tick).Should(Equal("general-1-drain"))
		})

		It("serialises maintenances within a routing group and hands over after Resume", func() {
			f := newFixture("serial", 2, "general-1", "general-2", "general-3")

			first := f.maintenance("general-1", trinov1alpha1.ActionDrain)
			Eventually(phase(first), eventually, tick).Should(Equal(trinov1alpha1.PhaseDrained))

			By("making the second drain wait for ownership")
			second := f.maintenance("general-2", trinov1alpha1.ActionDrain)
			Eventually(phase(second), eventually, tick).Should(Equal(trinov1alpha1.PhaseWaiting))
			Expect(message(second)()).To(ContainSubstring("under maintenance by " + f.namespace + "/general-1-drain"))
			Consistently(phase(second), 3*waitingInterval, tick).Should(Equal(trinov1alpha1.PhaseWaiting))
			Expect(fakeGateway.IsActive(f.backend("general-2"))).To(BeTrue(), "waiting drain must not touch the gateway")
			expectReady(second, metav1.ConditionFalse, "Waiting")

			By("resuming the first cluster")
			resume := f.maintenance("general-1", trinov1alpha1.ActionResume)
			Eventually(phase(resume), eventually, tick).Should(Equal(trinov1alpha1.PhaseCompleted))
			Expect(fakeGateway.IsActive(f.backend("general-1"))).To(BeTrue())
			expectReady(resume, metav1.ConditionTrue, "Completed")

			By("letting the second drain proceed")
			Eventually(phase(second), eventually, tick).Should(Equal(trinov1alpha1.PhaseDrained))
			Expect(fakeGateway.IsActive(f.backend("general-2"))).To(BeFalse())
			Expect(f.ownerName()).To(Equal("general-2-drain"))
		})

		It("waits when availability policy forbids draining and proceeds once it allows", func() {
			f := newFixture("avail", 2, "general-1", "general-2", "general-3")
			b3, _ := fakeGateway.Get(f.backend("general-3"))
			b3.Status = "UNHEALTHY"
			fakeGateway.Set(b3)

			m := f.maintenance("general-1", trinov1alpha1.ActionDrain)

			Eventually(phase(m), eventually, tick).Should(Equal(trinov1alpha1.PhaseWaiting))
			Expect(message(m)()).To(ContainSubstring("minAvailable=2"))
			Consistently(func() bool { return fakeGateway.IsActive(f.backend("general-1")) }, 3*waitingInterval, tick).Should(BeTrue())
			Expect(f.owner()).To(BeNil(), "do not reserve the group before a drain is permitted")

			By("not trusting a stale HEALTHY monitor result when the coordinator is down")
			f.coordinator("general-3").SetDown(true)
			b3.Status = "HEALTHY"
			fakeGateway.Set(b3)
			Consistently(phase(m), 3*waitingInterval, tick).Should(Equal(trinov1alpha1.PhaseWaiting))
			Expect(fakeGateway.IsActive(f.backend("general-1"))).To(BeTrue())
			By("recovering when the third coordinator really serves again")
			f.coordinator("general-3").SetDown(false)
			Eventually(phase(m), eventually, tick).Should(Equal(trinov1alpha1.PhaseDrained))
			Expect(fakeGateway.IsActive(f.backend("general-1"))).To(BeFalse())
		})

		It("is idempotent: a new Drain for an already drained backend completes without gateway writes", func() {
			f := newFixture("idem", 2, "general-1", "general-2", "general-3")
			first := f.maintenance("general-1", trinov1alpha1.ActionDrain)
			Eventually(phase(first), eventually, tick).Should(Equal(trinov1alpha1.PhaseDrained))
			f.deleteMaintenance(first) // ArgoCD BeforeHookCreation
			before := len(fakeGateway.Calls())

			again := f.maintenance("general-1", trinov1alpha1.ActionDrain)
			Eventually(phase(again), eventually, tick).Should(Equal(trinov1alpha1.PhaseDrained))
			expectReady(again, metav1.ConditionTrue, "Drained")
			Expect(f.ownerName()).To(Equal("general-1-drain"))
			for _, call := range fakeGateway.Calls()[before:] {
				Expect(call).NotTo(ContainSubstring("/deactivate/"), "no deactivate expected for an inactive backend")
			}
		})

		It("never treats unreadable metrics or missing workloads as idle", func() {
			f := newFixture("unknown", 0, "a")
			f.coordinator("a").SetQueries(5, 0)
			f.coordinator("a").SetDown(true)
			m := f.maintenance("a", trinov1alpha1.ActionDrain)
			Eventually(message(m), eventually, tick).Should(ContainSubstring("waiting for coordinator metrics"))
			Expect(get(m).Status.RunningQueries).To(BeNil())
			Expect(f.routingGroup().Status.ActiveMaintenance.VerifiedOffline).To(BeFalse())
			f.coordinator("a").SetDown(false)
			Eventually(message(m), eventually, tick).Should(ContainSubstring("5 running"))
			Expect(phase(m)()).To(Equal(trinov1alpha1.PhaseDraining))
		})
		It("allows the failed rollout to be repaired only with proof from the same offline window", func() {
			f := newFixture("repair", 2, "a", "b", "c")
			first := f.maintenance("a", trinov1alpha1.ActionDrain)
			Eventually(phase(first), eventually, tick).Should(Equal(trinov1alpha1.PhaseDrained))
			f.coordinator("a").SetDown(true)
			b, _ := fakeGateway.Get(f.backend("b"))
			b.Active = false
			fakeGateway.Set(b)
			f.deleteMaintenance(first)
			again := f.maintenance("a", trinov1alpha1.ActionDrain)
			Eventually(phase(again), eventually, tick).Should(Equal(trinov1alpha1.PhaseDrained))
			Expect(f.owner().DrainUID).To(Equal(again.UID))
			Expect(get(again).Status.RunningQueries).To(BeNil())
			Expect(fakeGateway.IsActive(f.backend("a"))).To(BeFalse())
		})
		It("fails closed on missing configuration or Gateway failure", func() {
			f := newFixture("missing", 0, "a")
			m := f.maintenance("ghost", trinov1alpha1.ActionDrain)
			Eventually(phase(m), eventually, tick).Should(Equal(trinov1alpha1.PhaseFailed))
			m = f.maintenance("ghost", trinov1alpha1.ActionResume)
			Eventually(phase(m), eventually, tick).Should(Equal(trinov1alpha1.PhaseFailed))
			fakeGateway.SetFail(true)
			DeferCleanup(func() { fakeGateway.SetFail(false) })
			m = f.maintenance("a", trinov1alpha1.ActionDrain)
			Eventually(phase(m), eventually, tick).Should(Equal(trinov1alpha1.PhaseFailed))
			Expect(f.owner()).To(BeNil())
		})
		It("does not steal a vanished owner's slot", func() {
			f := newFixture("held", 0, "a", "b")
			old := f.maintenance("a", trinov1alpha1.ActionDrain)
			Eventually(phase(old), eventually, tick).Should(Equal(trinov1alpha1.PhaseDrained))
			f.deleteMaintenance(old)
			m := f.maintenance("b", trinov1alpha1.ActionDrain)
			Eventually(phase(m), eventually, tick).Should(Equal(trinov1alpha1.PhaseWaiting))
			Expect(fakeGateway.IsActive(f.backend("b"))).To(BeTrue())
		})
		It("cannot write a late success onto a replacement hook", func() {
			f := newFixture("replace", 0, "b")
			gate := make(chan struct{})
			started := make(chan struct{})
			var once, release sync.Once
			var scrapes atomic.Int32
			unblock := func() { release.Do(func() { close(gate) }) }
			coord := fakes.NewCoordinator("481")
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if scrapes.Add(1) == 2 {
					once.Do(func() { close(started) })
					<-gate
				}
				coord.ServeHTTP(w, r)
			}))
			defer srv.Close()
			defer unblock()
			f.register("a", srv.URL)
			old := f.maintenance("a", trinov1alpha1.ActionDrain)
			Eventually(started, eventually, tick).Should(BeClosed())
			f.deleteMaintenance(old)
			replacement := &trinov1alpha1.TrinoMaintenance{ObjectMeta: metav1.ObjectMeta{Name: old.Name, Namespace: f.namespace}, Spec: trinov1alpha1.TrinoMaintenanceSpec{ClusterName: "b", Action: trinov1alpha1.ActionDrain}}
			Expect(k8sClient.Create(ctx, replacement)).To(Succeed())
			unblock()
			Eventually(phase(replacement), eventually, tick).Should(Equal(trinov1alpha1.PhaseWaiting))
			Expect(fakeGateway.IsActive(f.backend("b"))).To(BeTrue())
		})
	})
	Describe("Resume", func() {
		It("rechecks the Drain identity after a slow coordinator health check", func() {
			f := newFixture("slow-resume", 0)
			started, release := make(chan struct{}), make(chan struct{})
			var once, released sync.Once
			unblock := func() { released.Do(func() { close(release) }) }
			coord := fakes.NewCoordinator("482")
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/info" {
					once.Do(func() { close(started) })
					<-release
				}
				coord.ServeHTTP(w, r)
			}))
			defer srv.Close()
			defer unblock()
			f.register("a", srv.URL)
			old := f.maintenance("a", trinov1alpha1.ActionDrain)
			Eventually(phase(old), eventually, tick).Should(Equal(trinov1alpha1.PhaseDrained))
			resume := f.maintenance("a", trinov1alpha1.ActionResume)
			Eventually(started, eventually, tick).Should(BeClosed())
			f.deleteMaintenance(old)
			coord.SetQueries(5, 0)
			next := f.maintenance("a", trinov1alpha1.ActionDrain)
			unblock()
			Eventually(phase(resume), eventually, tick).Should(Equal(trinov1alpha1.PhaseFailed))
			Eventually(message(next), eventually, tick).Should(ContainSubstring("5 running"))
			Expect(fakeGateway.IsActive(f.backend("a"))).To(BeFalse())
		})
		It("requires the matching completed Drain and waits for both health checks", func() {
			f := newFixture("resume", 0, "a", "b")
			drain := f.maintenance("a", trinov1alpha1.ActionDrain)
			Eventually(phase(drain), eventually, tick).Should(Equal(trinov1alpha1.PhaseDrained))
			wrong := f.maintenance("b", trinov1alpha1.ActionResume)
			Eventually(phase(wrong), eventually, tick).Should(Equal(trinov1alpha1.PhaseFailed))
			Expect(f.ownerName()).To(Equal(drain.Name))
			f.coordinator("a").SetStarting(true)
			m := f.maintenance("a", trinov1alpha1.ActionResume)
			Eventually(phase(m), eventually, tick).Should(Equal(trinov1alpha1.PhaseResuming))
			Expect(fakeGateway.IsActive(f.backend("a"))).To(BeFalse())
			b, _ := fakeGateway.Get(f.backend("a"))
			b.Status = "PENDING"
			fakeGateway.Set(b)
			f.coordinator("a").SetStarting(false)
			Eventually(message(m), eventually, tick).Should(ContainSubstring("waiting for Gateway health"))
			Expect(f.owner().VerifiedOffline).To(BeFalse())
			Expect(f.ownerName()).To(Equal(drain.Name))
			b, _ = fakeGateway.Get(f.backend("a"))
			b.Status = "HEALTHY"
			fakeGateway.Set(b)
			Eventually(phase(m), eventually, tick).Should(Equal(trinov1alpha1.PhaseCompleted))
			Expect(f.owner()).To(BeNil())
		})
		It("cannot activate an administratively offline backend without a drain", func() {
			f := newFixture("unpaired", 0, "a")
			b, _ := fakeGateway.Get(f.backend("a"))
			b.Active = false
			fakeGateway.Set(b)
			m := f.maintenance("a", trinov1alpha1.ActionResume)
			Eventually(phase(m), eventually, tick).Should(Equal(trinov1alpha1.PhaseFailed))
			Expect(fakeGateway.IsActive(b.Name)).To(BeFalse())
		})
		It("fences an old pending Resume when the same named Drain is recreated", func() {
			f := newFixture("fence", 0, "a")
			old := f.maintenance("a", trinov1alpha1.ActionDrain)
			Eventually(phase(old), eventually, tick).Should(Equal(trinov1alpha1.PhaseDrained))
			f.coordinator("a").SetStarting(true)
			stale := f.maintenance("a", trinov1alpha1.ActionResume)
			Eventually(phase(stale), eventually, tick).Should(Equal(trinov1alpha1.PhaseResuming))
			f.deleteMaintenance(old)
			newDrain := f.maintenance("a", trinov1alpha1.ActionDrain)
			Eventually(phase(newDrain), eventually, tick).Should(Equal(trinov1alpha1.PhaseDrained))
			f.coordinator("a").SetStarting(false)
			Eventually(phase(stale), eventually, tick).Should(Equal(trinov1alpha1.PhaseFailed))
			Expect(fakeGateway.IsActive(f.backend("a"))).To(BeFalse())
			Expect(f.owner().DrainUID).To(Equal(newDrain.UID))
			f.deleteMaintenance(stale)
			fresh := f.maintenance("a", trinov1alpha1.ActionResume)
			Eventually(phase(fresh), eventually, tick).Should(Equal(trinov1alpha1.PhaseCompleted))
		})
	})
	Describe("Spec validation", func() {
		It("rejects spec changes after creation", func() {
			f := newFixture("immutable", 0, "general-1")
			m := f.maintenance("general-1", trinov1alpha1.ActionDrain)
			Eventually(phase(m), eventually, tick).Should(Equal(trinov1alpha1.PhaseDrained))

			cur := get(m)
			cur.Spec.Action = trinov1alpha1.ActionResume
			err := k8sClient.Update(ctx, cur)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("spec is immutable"))
		})

		It("rejects unknown actions", func() {
			f := newFixture("badaction", 0)
			m := &trinov1alpha1.TrinoMaintenance{
				ObjectMeta: metav1.ObjectMeta{Name: "bad", Namespace: f.namespace},
				Spec:       trinov1alpha1.TrinoMaintenanceSpec{ClusterName: "x", Action: "Restart"},
			}
			Expect(k8sClient.Create(ctx, m)).NotTo(Succeed())
		})
	})

	Describe("End-to-end upgrade of a routing group", func() {
		It("upgrades three backends one at a time and keeps a failed backend off", func() {
			f := newFixture("dod", 2, "general-1", "general-2", "general-3")
			for _, c := range f.clusters {
				f.coordinator(c).SetQueries(2, 0)
			}

			By("ArgoCD creating all three PreSync drains at once")
			drains := map[string]*trinov1alpha1.TrinoMaintenance{}
			for _, c := range f.clusters {
				drains[c] = f.maintenance(c, trinov1alpha1.ActionDrain)
			}

			By("exactly one backend being deactivated")
			Eventually(func() int {
				n := 0
				for _, c := range f.clusters {
					if !fakeGateway.IsActive(f.backend(c)) {
						n++
					}
				}
				return n
			}, eventually, tick).Should(Equal(1))
			Consistently(func() int {
				n := 0
				for _, c := range f.clusters {
					if !fakeGateway.IsActive(f.backend(c)) {
						n++
					}
				}
				return n
			}, 3*waitingInterval, tick).Should(Equal(1))

			owner := f.owner()
			Expect(owner).NotTo(BeNil())
			firstCluster := owner.ClusterName
			Expect(phase(drains[firstCluster])()).To(Equal(trinov1alpha1.PhaseDraining))
			for _, c := range f.clusters {
				if c != firstCluster {
					Expect(phase(drains[c])()).To(Equal(trinov1alpha1.PhaseWaiting))
				}
			}

			By(fmt.Sprintf("queries finishing on %s", firstCluster))
			f.coordinator(firstCluster).SetQueries(0, 0)
			Eventually(phase(drains[firstCluster]), eventually, tick).Should(Equal(trinov1alpha1.PhaseDrained))

			By("the upgraded coordinator failing: Resume must not activate it and nobody else proceeds")
			f.coordinator(firstCluster).SetDown(true)
			resume := f.maintenance(firstCluster, trinov1alpha1.ActionResume)
			Eventually(phase(resume), eventually, tick).Should(Equal(trinov1alpha1.PhaseResuming))
			Consistently(func() bool { return fakeGateway.IsActive(f.backend(firstCluster)) }, 3*pollInterval, tick).Should(BeFalse())
			for _, c := range f.clusters {
				if c != firstCluster {
					Expect(phase(drains[c])()).To(Equal(trinov1alpha1.PhaseWaiting))
					Expect(fakeGateway.IsActive(f.backend(c))).To(BeTrue())
				}
			}

			By("the fix rolling out: the coordinator comes back and Resume completes")
			f.coordinator(firstCluster).SetDown(false)
			Eventually(phase(resume), eventually, tick).Should(Equal(trinov1alpha1.PhaseCompleted))
			Expect(fakeGateway.IsActive(f.backend(firstCluster))).To(BeTrue())

			By("the remaining backends being upgraded one after the other")
			done := map[string]bool{firstCluster: true}
			for range 2 {
				var next string
				Eventually(func() string {
					if o := f.owner(); o != nil && !done[o.ClusterName] {
						next = o.ClusterName
						return next
					}
					return ""
				}, eventually, tick).ShouldNot(BeEmpty())
				done[next] = true
				Eventually(phase(drains[next]), eventually, tick).Should(Equal(trinov1alpha1.PhaseDraining))
				f.coordinator(next).SetQueries(0, 0)
				Eventually(phase(drains[next]), eventually, tick).Should(Equal(trinov1alpha1.PhaseDrained))
				r := f.maintenance(next, trinov1alpha1.ActionResume)
				Eventually(phase(r), eventually, tick).Should(Equal(trinov1alpha1.PhaseCompleted))
				Expect(fakeGateway.IsActive(f.backend(next))).To(BeTrue())
				f.deleteMaintenance(drains[next])
				f.deleteMaintenance(r)
			}
			Eventually(func() *trinov1alpha1.ActiveMaintenance { return f.owner() }, eventually, tick).Should(BeNil())
			for _, c := range f.clusters {
				Expect(fakeGateway.IsActive(f.backend(c))).To(BeTrue())
			}
		})
	})
})
