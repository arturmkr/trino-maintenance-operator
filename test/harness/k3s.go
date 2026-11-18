//go:build integration

// Package harness starts disposable infrastructure in the test runner's private Docker daemon.
package harness

import (
	"os"
	"strings"
	"testing"
	"time"

	tc "github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/k3s"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// RequireRunner refuses BEFORE Testcontainers discovers a daemon on the host.
func RequireRunner(t *testing.T) {
	t.Helper()
	if _, err := os.Stat("/etc/tmo-isolated-runner"); err != nil || os.Getenv("DOCKER_HOST") != "tcp://127.0.0.1:2375" {
		t.Fatal("integration tests require the disposable Compose runner; use the make target")
	}
}

type Cluster struct {
	*k3s.K3sContainer
	Config *rest.Config
	YAML   []byte
}

func StartK3s(t *testing.T) Cluster {
	t.Helper()
	RequireRunner(t)
	ctx := t.Context()
	node, err := k3s.Run(ctx, os.Getenv("K3S_IMAGE"),
		tc.WithCmdArgs("--disable=servicelb,metrics-server,local-storage", "--snapshotter=native"))
	tc.CleanupContainer(t, node)
	if err != nil {
		t.Fatal(err)
	}
	if pods := strings.Fields(os.Getenv("POD_IMAGES")); len(pods) != 0 {
		if err := node.LoadImages(ctx, pods...); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := node.GetKubeConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := clientcmd.RESTConfigFromKubeConfig(raw)
	if err != nil {
		t.Fatal(err)
	}
	endpoint, err := node.PortEndpoint(ctx, "6443/tcp", "https")
	if err != nil || cfg.Host != endpoint || len(cfg.CAData) == 0 || len(cfg.CertData) == 0 {
		t.Fatal("Kubernetes credentials or endpoint do not match the new K3s container")
	}
	cfg.Timeout = 30 * time.Second
	return Cluster{node, cfg, raw}
}
