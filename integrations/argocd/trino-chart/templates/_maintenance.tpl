{{/*
Name of the TrinoCluster: trino.commonLabels["trino-cluster"] or the release name.
*/}}
{{- define "trino.maintenance.clusterName" -}}
{{- $fromLabels := "" -}}
{{- with .Values.trino -}}
{{- with .commonLabels -}}
{{- $fromLabels = index . "trino-cluster" | default "" -}}
{{- end -}}
{{- end -}}
{{- default .Release.Name $fromLabels -}}
{{- end -}}

{{/*
Backend name in Trino Gateway.
*/}}
{{- define "trino.maintenance.gatewayBackend" -}}
{{- default (include "trino.maintenance.clusterName" .) .Values.maintenance.gatewayBackend -}}
{{- end -}}

{{/*
Coordinator URL.
*/}}
{{- define "trino.maintenance.coordinatorUrl" -}}
{{- default (printf "http://%s-trino.%s.svc:%v" .Release.Name .Release.Namespace .Values.maintenance.coordinator.port) .Values.maintenance.coordinator.url -}}
{{- end -}}
