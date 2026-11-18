# Trino Maintenance Operator

## 1. High-level diagram

```text
                         +----------------------+
                         |       Git / PR       |
                         |  Trino version bump  |
                         +----------+-----------+
                                    |
                                    v
                         +----------------------+
                         |        Argo CD       |
                         |  PreSync / Sync /    |
                         |      PostSync        |
                         +----------+-----------+
                                    |
                    PreSync creates | TrinoMaintenance (Drain)
                                    v
+------------------------------------------------------------------+
|                 Trino Maintenance Operator                       |
|                                                                  |
|  Watches:                                                        |
|  - TrinoCluster                                                  |
|  - TrinoRoutingGroup                                             |
|  - TrinoMaintenance                                              |
|                                                                  |
|  Logic:                                                          |
|  1. Check routing group availability                             |
|  2. Deactivate backend in Trino Gateway                          |
|  3. Poll Trino coordinator metrics                               |
|  4. Wait until running queries = 0 and queued queries = 0        |
|  5. Mark maintenance as Drained                                  |
|  6. After upgrade, verify health                                 |
|  7. Reactivate backend in Trino Gateway                          |
+-------------------+--------------------------+-------------------+
                    |                          |
                    |                          |
                    v                          v
       +----------------------+    +----------------------------+
       |    Trino Gateway     |    |     Trino Coordinator      |
       | activate/deactivate  |    | /metrics, health endpoint  |
       +----------+-----------+    +-------------+--------------+
                  |                                  |
                  v                                  v
        +------------------------------------------------------+
        |              Trino / Starburst Clusters              |
        |      general-1, general-2, general-3, etc.          |
        +------------------------------------------------------+

Example rolling upgrade flow for routing group "general":

general-1 -> drain -> upgrade -> health check -> enable
general-2 -> wait   -> drain   -> upgrade      -> enable
general-3 -> wait   -> wait    -> drain        -> enable
```

---

## 2. Project idea

This project implements a Kubernetes operator for safe maintenance and rolling upgrades of Trino clusters behind Trino Gateway.

The main problem is that a query started on one Trino coordinator must finish on the same coordinator. Because of that, a backend must first be removed from Trino Gateway, then drained, and only after that it can be restarted or upgraded.

The operator automates this process and prevents unsafe parallel maintenance inside the same routing group.

---

## 3. Main goal

The goal of version 0.1 is to support **safe Argo CD based upgrades** for Trino clusters.

The operator should:

- temporarily disable one backend in Trino Gateway,
- wait until active queries finish,
- allow Argo CD to deploy the new version,
- verify that the new backend is healthy,
- enable the backend again,
- continue with the next backend in the same routing group.

---

## 4. Core Kubernetes resources

### `TrinoCluster`
Represents one Trino or Starburst backend.

Contains:
- routing group name,
- gateway backend name,
- Trino coordinator URL,
- workload references.

### `TrinoRoutingGroup`
Represents an availability policy.

Example:
- routing group `general`
- `minAvailable = 2`

This means that at least 2 backends must remain available while one backend is being upgraded.

### `TrinoMaintenance`
Represents one temporary maintenance request.

Actions in v0.1:
- `Drain`
- `Resume`

---

## 5. Main workflow

### Upgrade flow

1. A Git pull request changes the Trino version.
2. Argo CD starts synchronization.
3. PreSync creates a `TrinoMaintenance` object with action `Drain`.
4. The operator:
    - checks routing group safety,
    - disables the backend in Trino Gateway,
    - polls Trino metrics,
    - waits until:
        - running queries = 0
        - queued queries = 0
5. The operator marks the backend as `Drained`.
6. Argo CD continues the deployment.
7. After the new version becomes healthy, PostSync creates `TrinoMaintenance` with action `Resume`.
8. The operator verifies health and enables the backend again in Trino Gateway.

---

## 6. Example use case

A routing group contains 3 clusters:

- `general-1`
- `general-2`
- `general-3`

All three need to be upgraded from version 481 to 482.

Even if Argo CD starts all applications at the same time, the operator serializes the work:

1. upgrade `general-1`
2. upgrade `general-2`
3. upgrade `general-3`

This prevents downtime and keeps enough healthy backends available.

---

## 7. Failure handling

If the new version fails to start correctly:

- the backend stays disabled in Trino Gateway,
- other healthy backends continue serving traffic,
- the operator does not automatically roll back,
- the next backend in the routing group is not upgraded yet.

This makes the process safe and predictable.

---

## 8. Technology stack

- **Language:** Go
- **Framework:** Kubebuilder / controller-runtime
- **Platform:** Kubernetes
- **GitOps integration:** Argo CD
- **Traffic control:** Trino Gateway
- **Query drain detection:** Trino `/metrics` endpoint

---

## 9. Testing strategy

The project should use a simple layered testing approach:

1. **Unit tests**
    - maintenance logic,
    - routing group rules,
    - drain decision logic.

2. **envtest**
    - Kubernetes API server simulation for controller tests.

3. **Kind / lightweight E2E**
    - operator deployment,
    - fake or minimal Trino/Gateway setup.

The first version should stay small and test only the main upgrade scenario.

---

## 10. Version 0.1 scope

Included:
- TrinoCluster CRD
- TrinoRoutingGroup CRD
- TrinoMaintenance CRD
- safe upgrade flow with Argo CD
- query draining using Trino metrics
- backend activation/deactivation in Trino Gateway

Not included:
- Flux support
- scheduled restarts
- manual restart feature
- node drain integration
- Slack notifications
- automatic rollback

---

## 11. Expected result

The final result is a working Kubernetes operator that performs **safe rolling maintenance for Trino clusters** and can be extended later with additional triggers and integrations.