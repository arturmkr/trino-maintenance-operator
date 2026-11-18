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
	"context"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	trinov1alpha1 "github.com/arturmkr/trino-maintenance-operator/api/v1alpha1"
	"github.com/arturmkr/trino-maintenance-operator/internal/fakes"
	"github.com/arturmkr/trino-maintenance-operator/internal/gateway"
	"github.com/arturmkr/trino-maintenance-operator/internal/maintenance"
	"github.com/arturmkr/trino-maintenance-operator/internal/trino"
	"github.com/arturmkr/trino-maintenance-operator/test/harness"
	// +kubebuilder:scaffold:imports
)

// These tests use Ginkgo (BDD-style Go testing framework). Refer to
// http://onsi.github.io/ginkgo/ to learn more about Ginkgo.
//
// The suite runs the real controller against a disposable K3s control plane. Trino
// Gateway and the coordinators are the in-memory fakes served by httptest.

const (
	waitingInterval = 300 * time.Millisecond
	pollInterval    = 200 * time.Millisecond
)

var (
	ctx       context.Context
	cancel    context.CancelFunc
	cfg       *rest.Config
	k8sClient client.Client

	fakeGateway       *fakes.Gateway
	fakeGatewayServer *httptest.Server
	coordinators      = map[string]*fakes.Coordinator{}
)

func TestControllers(t *testing.T) {
	cfg = harness.StartK3s(t).Config
	RegisterFailHandler(Fail)

	RunSpecs(t, "Controller Suite")
}

var _ = BeforeSuite(func() {
	logf.SetLogger(zap.New(zap.WriteTo(GinkgoWriter), zap.UseDevMode(true)))

	ctx, cancel = context.WithCancel(context.TODO())

	var err error
	err = trinov1alpha1.AddToScheme(scheme.Scheme)
	Expect(err).NotTo(HaveOccurred())

	// +kubebuilder:scaffold:scheme

	By("bootstrapping test environment")
	_, err = envtest.InstallCRDs(cfg, envtest.CRDInstallOptions{
		Paths: []string{filepath.Join("..", "..", "config", "crd", "bases")}, ErrorIfPathMissing: true,
	})
	Expect(err).NotTo(HaveOccurred())

	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme.Scheme})
	Expect(err).NotTo(HaveOccurred())
	Expect(k8sClient).NotTo(BeNil())

	By("starting fake Trino Gateway")
	fakeGateway = fakes.NewGateway()
	fakeGatewayServer = httptest.NewServer(fakeGateway)
	gatewayClient, err := gateway.NewHTTPClient(fakeGatewayServer.URL, gateway.Credentials{})
	Expect(err).NotTo(HaveOccurred())

	By("starting the controller manager")
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:  scheme.Scheme,
		Metrics: metricsserver.Options{BindAddress: "0"},
	})
	Expect(err).NotTo(HaveOccurred())

	svc := maintenance.NewService(gatewayClient, trino.NewHTTPClient(nil))
	err = (&TrinoMaintenanceReconciler{
		Client:          mgr.GetClient(),
		Recorder:        mgr.GetEventRecorder("trino-maintenance-operator"),
		Maintenance:     svc,
		WaitingInterval: waitingInterval,
		PollInterval:    pollInterval,
	}).SetupWithManager(mgr)
	Expect(err).NotTo(HaveOccurred())

	go func() {
		defer GinkgoRecover()
		Expect(mgr.Start(ctx)).To(Succeed())
	}()
	Expect(mgr.GetCache().WaitForCacheSync(ctx)).To(BeTrue())
})

var _ = AfterSuite(func() {
	By("tearing down the test environment")
	cancel()
	if fakeGatewayServer != nil {
		fakeGatewayServer.Close()
	}
})

// newCoordinator starts a fake coordinator for the given cluster and returns its URL.
func newCoordinator(name string) (*fakes.Coordinator, string) {
	c := fakes.NewCoordinator("481")
	srv := httptest.NewServer(c)
	DeferCleanup(srv.Close)
	coordinators[name] = c
	return c, srv.URL
}
