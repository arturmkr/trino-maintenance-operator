# Trino Maintenance Operator

Drain one Trino backend at a time for Argo CD upgrades. Three CRDs, one Go controller,
no workload restarts or Git rollback performed by the operator.

```
PreSync: lock group -> check serving peers -> turn backend OFF -> verify idle
Sync:    Argo rolls out the workload and waits for it to become healthy
PostSync: check coordinator -> turn backend ON -> observe routable -> unlock
```

## What makes an upgrade safe?

- **Availability**: before each deactivation, count *other* backends in the actual Gateway
  routing group that are both `active` and `HEALTHY` and still answer a live `/v1/info` readiness
  check. The operator must be able to reach the peers' public info endpoints. With `minAvailable: 2`,
  an unhealthy third backend blocks a new drain. An already-OFF backend can be repaired even
  below the floor. This is an admission check, not protection against subsequent independent failures.
- **Queries**: observe OFF from Gateway, then read the coordinator's `/metrics`. Require zero
  `RunningQueries`, zero `QueuedQueries`, and `SubmittedQueries == CompletedQueries` on two polls.
  The last two are coordinator-wide lifetime totals, not per-user counts: a planning or finishing
  query is still outstanding. Missing, malformed or inconsistent evidence does not unblock the rollout.
  No query text, SQL polling, Prometheus server or Kubernetes replica counts are involved.
- **Ownership**: a `TrinoRoutingGroup.status.activeMaintenance` update uses Kubernetes optimistic
  concurrency. The slot stays reserved throughout the rollout, even if PreSync is deleted or the
  operator restarts. A pending Resume from a previous window cannot reopen the backend. A stale
  controller status update cannot complete a replacement hook. The standard manager Lease is only
  for leader election. Never run independent managers against the same Gateway.
- **Recovery**: if the rollout fails, PostSync does not run. Fix the workload and sync the same
  Application. The same registered backend, still OFF, can reuse its durable, previously verified
  idle checkpoint if the broken coordinator cannot serve metrics. A successful non-idle reading
  overrides that checkpoint. The controller invalidates the checkpoint *before* attempting activation,
  even if the write times out. No positive checkpoint means no shortcut.

### Operating contract

All new query traffic must enter via this Gateway, and the operator must be the only writer of
backend activation during maintenance. Use full Argo syncs with the health customization installed:
selective resource syncs skip hooks. Do not delete or edit the group, backend identity or lock during
a rollout. One coordinator endpoint must address exactly one backend. Only trusted platform/Argo
identities should be able to write the CRs. No controller can protect traffic or deployments that bypass it.

The Gateway account needs `USER` for `POST /webapp/getAllBackends` (administrative state **and**
monitor health) and `API` for `/gateway/backend/{activate,deactivate}/{name}`. See the
[Gateway 22 implementation](https://github.com/trinodb/trino-gateway/blob/22/gateway-ha/src/main/java/io/trino/gateway/ha/resource/GatewayWebAppResource.java).
The configured coordinator URL must equal the registered `proxyTo` (trailing slash ignored).
Verify your distribution exposes the four `*_QueryManager_{RunningQueries,QueuedQueries,SubmittedQueries,CompletedQueries}`
samples. Trino 481/482 use [dispatch query stats](https://github.com/trinodb/trino/blob/481/core/trino-main/src/main/java/io/trino/execution/QueryManagerStats.java).
A different Gateway API or metric schema is not silently treated as success.

## Install and register

After the first release, install a published version without cloning the repo:

```sh
helm install tmo oci://ghcr.io/arturmkr/charts/trino-maintenance-operator \
  --version "${VERSION:?Set to a published version, e.g. 0.1.0-rc.1}" \
  --kubeconfig "${TARGET_KUBECONFIG:?Set to the intended cluster kubeconfig}" \
  -n trino-operator-system --create-namespace --set-string gateway.url=https://gateway.example.com
```

Maintainers: wait for **Lint**, **Tests** and **E2E Tests** to pass for the intended commit on
`main`. In GitHub, open **Releases > Draft a new release**, select or create `vX.Y.Z` or
`vX.Y.Z-rc.N` on that commit, then click **Publish release**. A tag or draft alone does nothing.
The release workflow checks the existing `main` push results for that exact commit; it does not
rerun tests. If CI was still running, wait for it to pass and use **Re-run failed jobs** on the
release workflow. Only then are the matching amd64/arm64 image and OCI chart published to GHCR.
No `latest` tag or cluster deployment. Use a new version for code fixes; do not move release tags.
For anonymous installs, set **both** GHCR packages to public after their first publication.

Alternatively, to install from source with your own image:

```sh
helm install tmo charts/trino-maintenance-operator -n trino-operator-system --create-namespace \
  --set image.repository=<your-registry>/trino-maintenance-operator --set image.tag=<your-tag> \
  --set gateway.url=https://gateway.example.com --set gateway.credentialsSecret.name=gateway-auth
```

Gateway credentials come from `username`/`password` or `token` in a Secret in the **release
namespace**. Gateway URL is required. Replicas share leader election (enabled by default).
Poll/wait intervals default to 5s/15s. A private CA can be mounted with `extraVolumeMounts`/`extraVolumes`.
Kustomize deployment is also available (`make install`, `make deploy IMG=...`; configure `GATEWAY_URL` first).

```yaml
apiVersion: trino.example.io/v1alpha1
kind: TrinoRoutingGroup
metadata: {name: general}
spec: {minAvailable: 2}
---
apiVersion: trino.example.io/v1alpha1
kind: TrinoCluster
metadata: {namespace: trino, name: general-1}
spec:
  routingGroup: general
  gatewayBackend: general-1
  coordinator:
    url: http://general-1.trino.svc:8080
  # workload.coordinator / workload.workers are optional, informational references.
```

`TrinoCluster` lives with the workload. Its routing group, backend name and coordinator URL are
immutable: finish maintenance before registering a different backend. The policy is cluster-scoped
so peers can live in different namespaces. Optional `coordinator.credentialsSecret.name` uses
`username`/`password` for Basic auth; use HTTPS. By default the operator cannot read **any** Secrets
from the API. For this option grant its ServiceAccount a RoleBinding in the backend namespace to a
Role with `apiGroups: [""]`, `resources: [secrets]`, `resourceNames: [<that-secret>]`, `verbs: [get]`.

## Operate

See [the Argo integration guide](integrations/argocd/README.md) for the mandatory health check and
registration-only first sync. The temporary requests have immutable `spec.clusterName` and `spec.action`
(`Drain` or `Resume`). Use stable hook names and `<clusterName>-resume` for the matching Resume.

```sh
kubectl get trinomaintenances -A -o wide
kubectl get trinoroutinggroups
kubectl describe trinomaintenance general-1-drain -n trino
```

`Waiting` means another backend owns the group or serving capacity is insufficient. `Draining` and
`Resuming` show the current check in `status.message`. `Drained` permits rollout; `Completed` means
the backend was observed routable and ownership released. `Failed` requires a new request (fix the
reported issue and re-sync). Never delete a lock to fix a crash-loop. If configuration was destroyed,
stop syncs and verify actual routing, coordinator and other backends before manual recovery.

## Develop

| Command | Scope |
| --- | --- |
| `make test` | Go clients, decisions and Lua health, in a container with no network |
| `make helm-lint` | Render both bootstrap and protected charts in a container |
| `make test-controller` | Controller regressions, CRDs and API concurrency on a new Testcontainers K3s |
| `make e2e-argocd` | Real Argo CD Core and operator in K3s. Three Applications upgrade fake backends; the first fails, is repaired and unblocks the other two |
| `make e2e-trino` | Real Gateway 22, PostgreSQL and two single-node Trino 481 servers, no Kubernetes. The production Drain service waits for a slow SQL query while a new query goes to its peer |

Only a local Linux-container Docker engine, Compose v2 and `make` are required. If the local
socket is not `/var/run/docker.sock`, pass `TEST_DOCKER_SOCKET=/absolute/path/to/docker.sock`.
There is no test shell script or host `kubectl`, Helm, Go, credentials or kubeconfig dependency.
The integration runner talks to its **own disposable Docker daemon** on loopback, not a mounted
host socket. K3s credentials come from that specific container; no existing cluster mode exists.
Make streams only the suite's allowlisted public Docker images (`test/images.env`) into the private
daemon. K3s imports its pod images from there. This reuses downloads without copying registry
credentials or requiring an inner cluster to trust a workstation's corporate TLS proxy.
If Docker or the registry is unavailable the test fails, without falling back. Privileged nested
containers are required for K3s; use a disposable CI VM, not a shared production Docker host.

PRs run lint, unit tests and chart checks. Pushes to `main` also run the controller regressions
and both acceptance suites. The two acceptance targets run **sequentially** in their own workflow;
either can still be run manually for debugging. Chart releases in
the Argo test are served by a temporary in-cluster Helm repository; no external Git writes.
The SQL test uses the [Gateway module from PR #4056](https://github.com/testcontainers/testcontainers-go/pull/4056)
pinned by commit in `go.mod`. Remove the `replace` when the module is released. These are two
complementary tests, not a claim that Argo rolled out real Trino in the same run.
