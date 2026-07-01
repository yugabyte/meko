{{/*
Expand the name of the chart.
*/}}
{{- define "yb-amp-controller.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "yb-amp-controller.fullname" -}}
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

{{- define "yb-amp-controller.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "yb-amp-controller.labels" -}}
helm.sh/chart: {{ include "yb-amp-controller.chart" . }}
{{ include "yb-amp-controller.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "yb-amp-controller.selectorLabels" -}}
app.kubernetes.io/name: {{ include "yb-amp-controller.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Stable workload names. The controller's Deployment/Service/RBAC use the fixed
"amp-controller" name (endpoints register against amp-controller.<ns>.svc), and
the MCP workload is "amp-controller-mcp". These are deliberately release-stable
so the in-cluster DNS the controller and endpoints assume keeps working.
*/}}
{{- define "yb-amp-controller.controllerName" -}}
amp-controller
{{- end }}

{{- define "yb-amp-controller.mcpName" -}}
amp-controller-mcp
{{- end }}

{{/*
Selector labels per workload (controller vs MCP) so the two Deployments/Services
don't fan out onto each other's pods.
*/}}
{{- define "yb-amp-controller.controllerSelectorLabels" -}}
{{ include "yb-amp-controller.selectorLabels" . }}
app.kubernetes.io/component: controller
app: amp-controller
{{- end }}

{{- define "yb-amp-controller.mcpSelectorLabels" -}}
{{ include "yb-amp-controller.selectorLabels" . }}
app.kubernetes.io/component: mcp
app: amp-controller-mcp
{{- end }}

{{/*
Image references; tags fall back to the chart appVersion.
*/}}
{{- define "yb-amp-controller.controllerImage" -}}
{{- $tag := .Values.controller.image.tag | default .Chart.AppVersion -}}
{{- printf "%s:%s" .Values.controller.image.repository $tag -}}
{{- end }}

{{- define "yb-amp-controller.mcpImage" -}}
{{- $tag := .Values.mcp.image.tag | default .Chart.AppVersion -}}
{{- printf "%s:%s" .Values.mcp.image.repository $tag -}}
{{- end }}

{{/*
In-cluster URL of the controller's managed storage controller (SC) port, handed
to deployed endpoints (AMP_DEPLOY_STORAGE_CONTROLLER_URL).
*/}}
{{- define "yb-amp-controller.storageControllerUrl" -}}
{{- printf "http://%s.%s.svc.cluster.local:%v" (include "yb-amp-controller.controllerName" .) .Release.Namespace .Values.controller.ports.sc -}}
{{- end }}

{{/*
Base URL the MCP server points at (AMP_CONTROLLER_URL): the explicit override if
set, else the in-cluster controller Service DNS on the api port.
*/}}
{{- define "yb-amp-controller.mcpControllerUrl" -}}
{{- if .Values.mcp.controllerUrl -}}
{{- .Values.mcp.controllerUrl -}}
{{- else -}}
{{- printf "http://%s.%s.svc.cluster.local:%v" (include "yb-amp-controller.controllerName" .) .Release.Namespace .Values.controller.ports.api -}}
{{- end -}}
{{- end }}

{{/*
Render a list of "key=value" strings as Service `annotations:` map entries.
Usage: include "yb-amp-controller.kvAnnotations" (dict "items" <list>) | nindent N
*/}}
{{- define "yb-amp-controller.kvAnnotations" -}}
{{- $out := dict -}}
{{- range .items -}}
{{- $kv := splitn "=" 2 . -}}
{{- $_ := set $out $kv._0 $kv._1 -}}
{{- end -}}
{{- toYaml $out -}}
{{- end }}
