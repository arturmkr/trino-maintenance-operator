//go:build integration

package e2e

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	tc "github.com/testcontainers/testcontainers-go"
	tcexec "github.com/testcontainers/testcontainers-go/exec"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	api "github.com/arturmkr/trino-maintenance-operator/api/v1alpha1"
	"github.com/arturmkr/trino-maintenance-operator/internal/gateway"
	"github.com/arturmkr/trino-maintenance-operator/test/harness"
)

const ns = "trino-e2e"

type argoTest struct {
	t           *testing.T
	ctx         context.Context
	cluster     harness.Cluster
	kube        *kubernetes.Clientset
	apps, hooks dynamic.ResourceInterface
	config      string
}

func TestArgoUpgrade(t *testing.T) {
	cluster := harness.StartK3s(t)
	ctx := t.Context()
	dyn := dynamic.NewForConfigOrDie(cluster.Config)
	appResource := schema.GroupVersionResource{Group: "argoproj.io", Version: "v1alpha1", Resource: "applications"}
	a := argoTest{t: t, ctx: ctx, cluster: cluster, kube: kubernetes.NewForConfigOrDie(cluster.Config),
		apps:   dyn.Resource(appResource).Namespace("argocd"),
		hooks:  dyn.Resource(api.SchemeGroupVersion.WithResource("trinomaintenances")).Namespace(ns),
		config: filepath.Join(t.TempDir(), "k3s.yaml")}
	require.NoError(t, os.WriteFile(a.config, cluster.YAML, 0600))
	a.install()
	t.Log("registering three backends before enabling hooks")

	for _, id := range []string{"1", "2", "3"} {
		a.version(id, "0.1.0")
	}
	for _, id := range []string{"1", "2", "3"} {
		a.synced(id, "0.1.0")
		a.queries(id, 3)
	}
	a.version("1", "0.2.0")
	a.waitHook("1-drain", "Draining", 3)
	a.version("2", "0.2.0")
	a.version("3", "0.2.0")
	for _, id := range []string{"2", "3"} {
		a.waitHook(id+"-drain", "Waiting", 0)
	}
	a.serving("general-2", "general-3")
	for _, id := range []string{"1", "2", "3"} {
		a.workload(id, "0.1.0")
	}

	a.queries("1", 0)
	t.Log("first backend idle; waiting for Argo to report the broken rollout")
	a.waitHook("1-drain", "Drained", 0)
	a.waitApp("1", func(app *unstructured.Unstructured) bool {
		return field(app, "status", "health", "status") == "Degraded"
	})
	_, err := a.hooks.Get(ctx, "general-1-resume", metav1.GetOptions{})
	require.True(t, apierrors.IsNotFound(err), "Argo must withhold PostSync: %v", err)
	a.serving("general-2", "general-3")
	for _, id := range []string{"2", "3"} {
		a.workload(id, "0.1.0")
		a.waitHook(id+"-drain", "Waiting", 0)
	}

	// A failed Sync wave may still be Running. Terminate it the way the
	// Argo UI does, then submit the fixed chart. Never touch the group lock.
	app, err := a.apps.Get(ctx, "general-1", metav1.GetOptions{})
	require.NoError(t, err)
	if field(app, "status", "operationState", "phase") == "Running" {
		_, err = a.apps.Patch(ctx, app.GetName(), types.MergePatchType,
			[]byte(`{"status":{"operationState":{"phase":"Terminating"}}}`), metav1.PatchOptions{})
		require.NoError(t, err)
		a.waitApp("1", func(obj *unstructured.Unstructured) bool {
			return slices.Contains([]string{"Failed", "Error"}, field(obj, "status", "operationState", "phase"))
		})
	}
	a.version("1", "0.3.0")
	t.Log("repairing first rollout; peers must proceed one at a time")
	a.synced("1", "0.3.0")
	a.waitHook("1-resume", "Completed", 0)
	pending := []string{"2", "3"}
	for len(pending) != 0 {
		var next string
		require.Eventually(t, func() bool {
			for _, id := range pending {
				if a.hook(id+"-drain", "Draining", 3) {
					next = id
					return true
				}
			}
			return false
		}, 2*time.Minute, time.Second)
		peers := []string{"general-1", "general-2", "general-3"}
		a.serving(slices.DeleteFunc(peers, func(peer string) bool { return peer == "general-"+next })...)
		pending = slices.DeleteFunc(pending, func(id string) bool { return id == next })
		for _, id := range pending {
			a.waitHook(id+"-drain", "Waiting", 0)
			a.workload(id, "0.1.0")
		}
		a.queries(next, 0)
		a.synced(next, "0.2.0")
		a.waitHook(next+"-resume", "Completed", 0)
	}
	a.serving("general-1", "general-2", "general-3")
}

func (a *argoTest) install() {
	t, ctx := a.t, a.ctx
	provider, err := tc.NewDockerProvider()
	require.NoError(t, err)
	defer func() { _ = provider.Close() }()
	images := make([]string, 0, 2)
	for dir, repo := range map[string]string{
		"manager": "example.com/operator", "fake": "example.com/trino-maintenance-fakebackend",
	} {
		ref, err := provider.BuildImage(ctx, &tc.ContainerRequest{FromDockerfile: tc.FromDockerfile{
			Context: filepath.Join("/fixtures", dir), Repo: repo, Tag: "e2e"}})
		require.NoError(t, err)
		images = append(images, ref)
	}
	require.NoError(t, a.cluster.LoadImages(ctx, images...))
	a.helm("install", "tmo", "../../charts/trino-maintenance-operator", "-n", "tmo-system", "--create-namespace", "--wait",
		"--set", "image.repository=example.com/operator,image.tag=e2e,operator.pollInterval=1s,operator.waitingInterval=1s",
		"--set", "gateway.url=http://fake-gateway."+ns+".svc:8080")
	a.apply(file(t, "testdata/fakes.yaml"))

	argocd := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "argocd"}}
	_, err = a.kube.CoreV1().Namespaces().Create(ctx, argocd, metav1.CreateOptions{})
	require.NoError(t, err)
	// Use the exact preloaded images, including behind a corporate registry proxy.
	core := strings.ReplaceAll(string(file(t, "/fixtures/argocd-core.yaml")),
		"imagePullPolicy: Always", "imagePullPolicy: IfNotPresent")
	a.apply([]byte(core), "-n", "argocd")
	// Core has no API server to initialize the signing key for the in-cluster destination.
	_, err = a.kube.CoreV1().Secrets("argocd").Patch(ctx, "argocd-secret", types.MergePatchType,
		[]byte(`{"stringData":{"server.secretkey":"`+rand.Text()+`"}}`), metav1.PatchOptions{})
	require.NoError(t, err)
	lua := file(t, "../../integrations/argocd/health.lua")
	patch, err := json.Marshal(map[string]any{"data": map[string]string{
		"timeout.reconciliation": "5s", "timeout.reconciliation.jitter": "0s",
		"resource.customizations.health.trino.example.io_TrinoMaintenance": string(lua)}})
	require.NoError(t, err)
	_, err = a.kube.CoreV1().ConfigMaps("argocd").Patch(ctx, "argocd-cm",
		types.MergePatchType, patch, metav1.PatchOptions{})
	require.NoError(t, err)

	chart, packages := t.TempDir(), t.TempDir()
	require.NoError(t, os.CopyFS(chart, os.DirFS("../../integrations/argocd/trino-chart")))
	workload := filepath.Join(chart, "templates", "workload.yaml")
	require.NoError(t, os.WriteFile(workload, file(t, "testdata/argo-workload.yaml"), 0600))
	for _, v := range []string{"0.1.0", "0.2.0", "0.3.0"} {
		a.helm("package", chart, "--version", v, "--destination", packages)
	}
	a.helm("repo", "index", packages)
	files, err := os.ReadDir(packages)
	require.NoError(t, err)
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "chart-packages"}, BinaryData: map[string][]byte{}}
	for _, f := range files {
		cm.BinaryData[f.Name()] = file(t, filepath.Join(packages, f.Name()))
	}
	_, err = a.kube.CoreV1().ConfigMaps(ns).Create(ctx, cm, metav1.CreateOptions{})
	require.NoError(t, err)
	repoManifest := string(file(t, "testdata/chart-repo.yaml"))
	a.apply([]byte(strings.ReplaceAll(repoManifest, "CHART_SERVER_IMAGE", os.Getenv("CHART_SERVER_IMAGE"))))
	// Avoid caching a failed chart comparison during repo-server startup.
	for namespace, name := range map[string]string{"argocd": "argocd-repo-server", ns: "charts"} {
		require.EventuallyWithT(t, func(c *assert.CollectT) {
			d, err := a.kube.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
			if assert.NoError(c, err) {
				assert.Positive(c, d.Status.AvailableReplicas, "%s/%s", namespace, name)
			}
		}, 3*time.Minute, time.Second)
	}
}

func file(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(name)
	require.NoError(t, err)
	return b
}

func (a *argoTest) helm(args ...string) {
	a.t.Helper()
	cmd := exec.CommandContext(a.ctx, "helm", append([]string{"--kubeconfig", a.config}, args...)...)
	out, err := cmd.CombinedOutput()
	require.NoError(a.t, err, "helm %v: %s", args, out)
}

// kubectl exists only IN the new K3s node. There is no host kubectl fallback.
func (a *argoTest) apply(manifest []byte, flags ...string) {
	a.t.Helper()
	const path = "/tmp/tmo-manifest.yaml"
	require.NoError(a.t, a.cluster.CopyToContainer(a.ctx, manifest, path, 0600))
	cmd := []string{"kubectl", "--kubeconfig=/etc/rancher/k3s/k3s.yaml", "apply", "--server-side", "-f", path}
	code, r, err := a.cluster.Exec(a.ctx, append(cmd, flags...), tcexec.Multiplexed())
	require.NoError(a.t, err)
	out, err := io.ReadAll(r)
	require.NoError(a.t, err)
	require.Zero(a.t, code, "applying manifest: %s", out)
}

func (a *argoTest) version(id, v string) {
	template := string(file(a.t, "testdata/application.yaml"))
	values := strings.NewReplacer("ID", id, "VERSION", v, "HOOKS", strconv.FormatBool(v != "0.1.0"))
	a.apply([]byte(values.Replace(template)))
}

func field(obj *unstructured.Unstructured, keys ...string) string {
	s, _, _ := unstructured.NestedString(obj.Object, keys...)
	return s
}

func (a *argoTest) waitApp(id string, ok func(*unstructured.Unstructured) bool) {
	a.t.Helper()
	require.EventuallyWithT(a.t, func(c *assert.CollectT) {
		app, err := a.apps.Get(a.ctx, "general-"+id, metav1.GetOptions{})
		if assert.NoError(c, err) {
			assert.True(c, ok(app), "general-%s status: %v", id, app.Object["status"])
		}
	}, 4*time.Minute, 2*time.Second)
}

func (a *argoTest) synced(id, v string) {
	a.waitApp(id, func(app *unstructured.Unstructured) bool {
		return field(app, "status", "operationState", "syncResult", "revision") == v &&
			field(app, "status", "operationState", "phase") == "Succeeded" &&
			field(app, "status", "health", "status") == "Healthy"
	})
}

func (a *argoTest) hook(name, phase string, running int64) bool {
	obj, err := a.hooks.Get(a.ctx, "general-"+name, metav1.GetOptions{})
	if err != nil {
		return false
	}
	n, _, _ := unstructured.NestedInt64(obj.Object, "status", "runningQueries")
	return field(obj, "status", "phase") == phase && n == running
}

func (a *argoTest) waitHook(name, phase string, n int64) {
	a.t.Helper()
	require.Eventually(a.t, func() bool { return a.hook(name, phase, n) },
		2*time.Minute, time.Second, "hook %s, expected %s (%d running)", name, phase, n)
}

func (a *argoTest) queries(id string, n int) {
	params := map[string]string{"running": strconv.Itoa(n)}
	_, err := a.kube.CoreV1().Services(ns).ProxyGet("http", "fake-trino-"+id, "8080", "fake/queries", params).
		DoRaw(a.ctx)
	require.NoError(a.t, err)
}

func (a *argoTest) serving(names ...string) {
	b, err := a.kube.CoreV1().Services(ns).ProxyGet("http", "fake-gateway", "8080", "gateway/backend/all", nil).
		DoRaw(a.ctx)
	require.NoError(a.t, err)
	var backends []gateway.Backend
	require.NoError(a.t, json.Unmarshal(b, &backends))
	var live []string
	for _, b := range backends {
		if b.Active && b.Status == "HEALTHY" {
			live = append(live, b.Name)
		}
	}
	require.ElementsMatch(a.t, names, live)
}

func (a *argoTest) workload(id, version string) {
	d, err := a.kube.AppsV1().Deployments(ns).Get(a.ctx, "fake-trino-"+id, metav1.GetOptions{})
	require.NoError(a.t, err)
	require.Contains(a.t, d.Spec.Template.Spec.Containers[0].Args, "--version="+version)
	require.EqualValues(a.t, 1, d.Status.ReadyReplicas)
}
