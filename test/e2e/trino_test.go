//go:build integration

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	tc "github.com/testcontainers/testcontainers-go"
	tctrino "github.com/testcontainers/testcontainers-go/modules/trino"
	tcgateway "github.com/testcontainers/testcontainers-go/modules/trinogateway"
	"github.com/testcontainers/testcontainers-go/network"

	api "github.com/arturmkr/trino-maintenance-operator/api/v1alpha1"
	"github.com/arturmkr/trino-maintenance-operator/internal/gateway"
	"github.com/arturmkr/trino-maintenance-operator/internal/maintenance"
	"github.com/arturmkr/trino-maintenance-operator/internal/trino"
	"github.com/arturmkr/trino-maintenance-operator/test/harness"
)

func TestRealTrinoDrain(t *testing.T) {
	harness.RequireRunner(t)
	ctx, cancel := context.WithTimeout(t.Context(), 18*time.Minute)
	defer cancel()
	gw, err := tcgateway.Run(ctx, os.Getenv("GATEWAY_IMAGE"),
		tcgateway.WithDataStore(tcgateway.NewManagedPostgresProvider(os.Getenv("POSTGRES_IMAGE"))),
		tc.WithEnv(map[string]string{"JAVA_TOOL_OPTIONS": "-Xms128m -Xmx384m -XX:ActiveProcessorCount=2"}))
	tc.CleanupContainer(t, gw)
	require.NoError(t, err)
	gwURL, err := gw.ConnectionString(ctx)
	require.NoError(t, err)
	gwClient, err := gateway.NewHTTPClient(gwURL, gateway.Credentials{})
	require.NoError(t, err)
	trinoClient := trino.NewHTTPClient(nil)
	service := maintenance.NewService(gwClient, trinoClient)

	const identitySQL = "SELECT node_id FROM system.runtime.nodes WHERE coordinator"
	var targets [2]maintenance.Target
	var identity [2]string
	for i := range targets {
		name := fmt.Sprintf("trino-%d", i+1)
		node, err := tctrino.Run(ctx, os.Getenv("TRINO_IMAGE"), network.WithNetwork([]string{name}, gw.Network()),
			tc.WithEnv(map[string]string{
				"JAVA_TOOL_OPTIONS": "-Xms256m -Xmx768m -XX:ActiveProcessorCount=2 -Dhttp-server.process-forwarded=true",
			}),
			tc.WithFiles(tc.ContainerFile{Reader: strings.NewReader("connector.name=blackhole\n"),
				ContainerFilePath: "/etc/trino/catalog/blackhole.properties", FileMode: 0644}))
		tc.CleanupContainer(t, node)
		require.NoError(t, err)
		// Both the runner and Gateway can reach this address on the inner Docker bridge.
		ip, err := node.ContainerIP(ctx)
		require.NoError(t, err)
		require.NotEmpty(t, ip)
		url := "http://" + ip + ":8080"
		targets[i] = maintenance.Target{Backend: name, Group: "adhoc", Coordinator: trino.Coordinator{URL: url}}
		identity[i], err = scalar(ctx, url, identitySQL)
		require.NoError(t, err)
		_, err = scalar(ctx, url, `CREATE TABLE blackhole.default.slow (n bigint) WITH
			(split_count=1, pages_per_split=20, rows_per_page=1, page_processing_delay='2s')`)
		require.NoError(t, err)
		require.NoError(t, gw.AddBackend(ctx, tcgateway.Backend{Name: name, ProxyTo: url, RoutingGroup: "adhoc"}))
	}
	require.NotEqual(t, identity[0], identity[1])
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		for _, target := range targets {
			view, err := service.Inspect(ctx, target, 1)
			assert.NoError(c, err)
			assert.Equal(c, int32(1), view.HealthyPeers)
		}
	}, time.Minute, time.Second)

	type result struct {
		value string
		err   error
	}
	finished := make(chan result, 1)
	go func() {
		n, err := scalar(ctx, gwURL, "SELECT count_if(n = 0) FROM blackhole.default.slow")
		finished <- result{n, err}
	}()
	owner := -1
	require.Eventually(t, func() bool {
		for i, target := range targets {
			s, err := trinoClient.QueryStats(ctx, target.Coordinator)
			if err == nil && s.Running > 0 {
				owner = i
				return true
			}
		}
		return false
	}, 25*time.Second, 500*time.Millisecond, "long query never observed on either coordinator")
	target := targets[owner]
	drain := func() (maintenance.Outcome, error) {
		view, err := service.Inspect(ctx, target, 1)
		if err != nil {
			return maintenance.Outcome{}, err
		}
		return service.Drain(ctx, target, view, 1, false), nil
	}
	first, err := drain()
	require.NoError(t, err)
	require.Equal(t, api.PhaseDraining, first.Phase)
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		out, err := drain()
		if !assert.NoError(c, err) {
			return
		}
		assert.Equal(c, api.PhaseDraining, out.Phase, out.Message)
		if assert.NotNil(c, out.Stats, out.Message) {
			assert.Positive(c, out.Stats.Unfinished)
		}
	}, 10*time.Second, 500*time.Millisecond)
	servedBy, err := scalar(ctx, gwURL, identitySQL)
	require.NoError(t, err)
	require.Equal(t, identity[1-owner], servedBy, "new query must use the other backend")
	select {
	case r := <-finished:
		require.NoError(t, r.err)
		require.Equal(t, "20", r.value, "all pages of the pre-existing query survived")
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		out, err := drain()
		assert.NoError(c, err)
		assert.Equal(c, api.PhaseDrained, out.Phase, out.Message)
	}, time.Minute, time.Second)
	require.NoError(t, service.Healthy(ctx, target.Coordinator))
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		view, err := service.Inspect(ctx, target, 1)
		if assert.NoError(c, err) {
			out := service.Resume(ctx, target, view)
			assert.Equal(c, api.PhaseCompleted, out.Phase, out.Message)
		}
	}, time.Minute, time.Second)
}

// Minimal Trino client protocol. Consume every nextUri, not just the HTTP
// acknowledgement that the gateway accepted the statement.
func scalar(ctx context.Context, server, statement string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server+"/v1/statement", strings.NewReader(statement))
	if err != nil {
		return "", err
	}
	var value string
	for {
		req.Header.Set("X-Trino-User", "tmo-test")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return "", err
		}
		var page struct {
			NextURI string                    `json:"nextUri"`
			Data    [][]any                   `json:"data"`
			Error   *struct{ Message string } `json:"error"`
		}
		decodeErr := json.NewDecoder(resp.Body).Decode(&page)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK || decodeErr != nil {
			return "", fmt.Errorf("%s: %s (decoding: %v)", req.URL, resp.Status, decodeErr)
		}
		if page.Error != nil {
			return "", fmt.Errorf("Trino: %s", page.Error.Message)
		}
		for _, row := range page.Data {
			if len(row) != 0 {
				value = fmt.Sprint(row[0])
			}
		}
		if page.NextURI == "" {
			return value, nil
		}
		if !strings.HasPrefix(page.NextURI, server+"/") {
			return "", fmt.Errorf("nextUri escaped query endpoint %s: %s", server, page.NextURI)
		}
		next, err := http.NewRequestWithContext(ctx, http.MethodGet, page.NextURI, nil)
		if err != nil {
			return "", err
		}
		req = next
	}
}
