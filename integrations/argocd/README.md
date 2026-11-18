# Protect a Trino Application with Argo CD

1. Install the operator/CRDs and the platform-owned `TrinoRoutingGroup` first.
2. Register `health.lua` in Argo CD **before** enabling hooks. Without it Argo can consider an
   unrecognized hook successful on creation. Merge this key into the existing `argocd-cm`:

   ```yaml
   data:
     resource.customizations.health.trino.example.io_TrinoMaintenance: |
       # Paste the contents of health.lua here (Lua, not YAML).
   ```

3. Copy the **whole** `trino-chart/templates/` directory, including `_maintenance.tpl`, into your
   existing Trino chart and merge `trino-chart/values.yaml`. Set `maintenance.routingGroup`,
   `maintenance.gatewayBackend` and `maintenance.coordinator.url` to match Gateway. The cluster name
   comes from `trino.commonLabels["trino-cluster"]`, falling back to the Helm release name.
4. **Register without upgrading**: sync once with `maintenance.enabled: true` and
   `maintenance.hooksEnabled: false` (default). Confirm `kubectl get trinoclusters -A` and the live
   group/URL. This is separate from a workload upgrade. A missing registration during protected sync
   is a hard failure, never an implicit first install. If the backend is already registered, skip
   the registration-only sync.
5. Set `maintenance.hooksEnabled: true` before upgrading. Use **full syncs only**; a resource-selective
   sync does not execute hooks. Do not disable hooks on a protected workload to unblock a failure.

`PreSync/<cluster>-drain` stays Progressing until `Drained`; `PostSync/<cluster>-resume` stays
Progressing until `Completed`. `Failed` is Degraded. Neither the operator nor the chart restarts a
workload on its own. Use `BeforeHookCreation` (not `HookSucceeded`): Resume verifies the live Drain's
UID and status. Keep both hook names stable, one Application per backend. No revision parameter,
time comparison or special Argo privileges for the operator are required.

**Broken rollout:** no successful Sync means no PostSync. The backend stays OFF and peers wait. Fix
and re-sync the same Application; if Argo is still waiting on the old broken rollout, terminate that
sync first. A fresh Drain can recover using the previous proven idle checkpoint only while the same
backend is still OFF and no activation was attempted. Do not delete the routing group.

```sh
helm template general-1 integrations/argocd/trino-chart -n trino --set maintenance.hooksEnabled=true
make e2e-argocd  # disposable Testcontainers K3s; no remote repo or credentials
```
