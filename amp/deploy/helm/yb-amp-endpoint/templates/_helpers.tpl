{{/*
Expand the name of the chart.
*/}}
{{- define "yb-amp-endpoint.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "yb-amp-endpoint.fullname" -}}
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

{{- define "yb-amp-endpoint.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "yb-amp-endpoint.labels" -}}
helm.sh/chart: {{ include "yb-amp-endpoint.chart" . }}
{{ include "yb-amp-endpoint.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "yb-amp-endpoint.selectorLabels" -}}
app.kubernetes.io/name: {{ include "yb-amp-endpoint.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Image reference; tag falls back to the chart appVersion.
*/}}
{{- define "yb-amp-endpoint.image" -}}
{{- $tag := .Values.image.tag | default .Chart.AppVersion -}}
{{- printf "%s:%s" .Values.image.repository $tag -}}
{{- end }}

{{/*
Name of the Secret holding the YB password (existing or chart-managed).
*/}}
{{- define "yb-amp-endpoint.secretName" -}}
{{- if .Values.yugabyte.existingSecret -}}
{{- .Values.yugabyte.existingSecret -}}
{{- else -}}
{{- include "yb-amp-endpoint.fullname" . -}}
{{- end -}}
{{- end }}

{{/*
Key within the Secret that holds the YB password.
*/}}
{{- define "yb-amp-endpoint.secretKey" -}}
{{- if .Values.yugabyte.existingSecret -}}
{{- .Values.yugabyte.existingSecretKey -}}
{{- else -}}
password
{{- end -}}
{{- end }}

{{/*
Whether a YB password is configured at all (inline or via an existing secret).
*/}}
{{- define "yb-amp-endpoint.hasPassword" -}}
{{- if or .Values.yugabyte.password .Values.yugabyte.existingSecret -}}
true
{{- end -}}
{{- end }}
