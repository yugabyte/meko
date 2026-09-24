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
Name of the Secret cert-manager fills with this endpoint's YB client certificate.
*/}}
{{- define "yb-amp-endpoint.tlsSecretName" -}}
{{- include "yb-amp-endpoint.fullname" . }}-yb-client-tls
{{- end }}

{{/*
Whether a YB password is configured at all (inline or via an existing secret).
*/}}
{{- define "yb-amp-endpoint.hasPassword" -}}
{{- if or .Values.yugabyte.password .Values.yugabyte.existingSecret -}}
true
{{- end -}}
{{- end }}

{{/*
Kubernetes volume name for an extension's ImageVolume. A Volume/VolumeMount name
must be an RFC 1123 DNS label (lowercase alphanumeric + '-'), but an extension
name can carry '_' or uppercase (pg_cron, pg_stat_statements). Sanitize to keep
the object valid while the mount path / GUCs still use the raw name. Input is the
extension entry (with .name). Runs of invalid chars collapse to a single '-'.
*/}}
{{- define "yb-amp-endpoint.extVolumeName" -}}
{{- printf "ext-%s" (regexReplaceAll "[^a-z0-9]+" (.name | lower) "-" | trimAll "-") -}}
{{- end }}

{{/*
Whether the documentdb extension is mounted on this endpoint. The gateway only
has a SQL surface to front when it is, so this is what "enabled by default"
resolves to: on wherever documentdb is, off everywhere else.
documentdbGateway.enabled overrides it either way when set.
*/}}
{{- define "yb-amp-endpoint.documentdbGatewayEnabled" -}}
{{- if kindIs "bool" .Values.documentdbGateway.enabled -}}
{{- .Values.documentdbGateway.enabled -}}
{{- else -}}
{{- $found := false -}}
{{- range .Values.endpoint.extensions -}}{{- if eq .name "documentdb" -}}{{- $found = true -}}{{- end -}}{{- end -}}
{{- $found -}}
{{- end -}}
{{- end -}}
