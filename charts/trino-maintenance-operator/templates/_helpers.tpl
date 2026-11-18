{{/*
Expand the name of the chart.
*/}}
{{- define "trino-maintenance-operator.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "trino-maintenance-operator.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Create chart name and version as used by the chart label.
*/}}
{{- define "trino-maintenance-operator.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "trino-maintenance-operator.labels" -}}
helm.sh/chart: {{ include "trino-maintenance-operator.chart" . }}
{{ include "trino-maintenance-operator.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "trino-maintenance-operator.selectorLabels" -}}
app.kubernetes.io/name: {{ include "trino-maintenance-operator.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
control-plane: controller-manager
{{- end }}

{{/*
Create the name of the service account to use
*/}}
{{- define "trino-maintenance-operator.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "trino-maintenance-operator.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Image reference
*/}}
{{- define "trino-maintenance-operator.image" -}}
{{- printf "%s:%s" .Values.image.repository (default .Chart.AppVersion .Values.image.tag) }}
{{- end }}

{{/*
Validate gateway values
*/}}
{{- define "trino-maintenance-operator.validateGateway" -}}
{{- if not .Values.gateway.url }}
{{- fail "gateway.url is required (base URL of the Trino Gateway)" }}
{{- end }}
{{- with .Values.gateway.credentialsSecret }}
{{- if and .namespace (ne .namespace $.Release.Namespace) }}
{{- fail (printf "gateway.credentialsSecret.namespace (%s) must equal the release namespace (%s): the Secret is injected into the operator Pod as environment variables" .namespace $.Release.Namespace) }}
{{- end }}
{{- end }}
{{- end }}
