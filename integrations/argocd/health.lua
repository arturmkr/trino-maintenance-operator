-- ArgoCD custom health check for trino.example.io/TrinoMaintenance.
--
-- Hook resources are considered finished by ArgoCD as soon as they are Healthy,
-- so a PreSync Drain blocks the sync until the backend is deactivated in Trino
-- Gateway and no queries are running; a PostSync Resume keeps the sync running
-- until the backend is active again.
--
--   Waiting   -> Progressing
--   Draining  -> Progressing
--   Resuming  -> Progressing
--   Drained   -> Healthy
--   Completed -> Healthy
--   Failed    -> Degraded
--   (no status yet) -> Progressing
local hs = {}
hs.status = "Progressing"
hs.message = "Waiting for trino-maintenance-operator"

if obj.status ~= nil and obj.status.phase ~= nil then
  local phase = obj.status.phase
  local message = obj.status.message or ""
  if obj.spec ~= nil and ((obj.spec.action == "Drain" and phase == "Drained") or
                          (obj.spec.action == "Resume" and phase == "Completed")) then
    hs.status = "Healthy"
  elseif phase == "Failed" then
    hs.status = "Degraded"
  else
    hs.status = "Progressing"
  end
  if message ~= "" then
    hs.message = phase .. ": " .. message
  else
    hs.message = phase
  end
end

return hs
