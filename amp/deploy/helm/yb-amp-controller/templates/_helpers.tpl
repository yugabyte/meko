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

{{- define "yb-amp-controller.uiName" -}}
amp-controller-ui
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

{{- define "yb-amp-controller.uiSelectorLabels" -}}
{{ include "yb-amp-controller.selectorLabels" . }}
app.kubernetes.io/component: ui
app: amp-controller-ui
{{- end }}

{{/*
Image references.

A repository that names no registry host of its own is prefixed with
.Values.imageRegistry, so one value repoints every amp image. One that already
carries a host is used verbatim — that is what the deploy scripts and the QA
framework pass (gcr.io/..., <acct>.dkr.ecr.<region>.amazonaws.com/..., quay.io/...),
and prefixing those would corrupt them. "Has a host" is the usual registry rule:
a dot or a colon in the first path segment.

Tags resolve per-image tag -> .Values.imageTag -> chart appVersion. One rule for
every image, the agent included: an install that names no tag gets an appVersion
ref that is not published, uniformly, rather than one image silently riding a
floating tag.
*/}}
{{- define "yb-amp-controller.imageRepo" -}}
{{- $head := splitList "/" .repo | first -}}
{{- if or (contains "." $head) (contains ":" $head) -}}
{{- .repo -}}
{{- else if .ctx.Values.imageRegistry -}}
{{- printf "%s/%s" (trimSuffix "/" .ctx.Values.imageRegistry) .repo -}}
{{- else -}}
{{- .repo -}}
{{- end -}}
{{- end }}

{{- define "yb-amp-controller.imageTag" -}}
{{- .tag | default .ctx.Values.imageTag | default .ctx.Chart.AppVersion -}}
{{- end }}

{{- define "yb-amp-controller.controllerImage" -}}
{{- printf "%s:%s"
      (include "yb-amp-controller.imageRepo" (dict "repo" .Values.controller.image.repository "ctx" .))
      (include "yb-amp-controller.imageTag" (dict "tag" .Values.controller.image.tag "ctx" .)) -}}
{{- end }}

{{- define "yb-amp-controller.endpointImageRepo" -}}
{{- include "yb-amp-controller.imageRepo" (dict "repo" .Values.controller.endpoint.image.repository "ctx" .) -}}
{{- end }}

{{- define "yb-amp-controller.endpointImageTag" -}}
{{- include "yb-amp-controller.imageTag" (dict "tag" .Values.controller.endpoint.image.tag "ctx" .) -}}
{{- end }}

{{- define "yb-amp-controller.mcpImage" -}}
{{- printf "%s:%s"
      (include "yb-amp-controller.imageRepo" (dict "repo" .Values.mcp.image.repository "ctx" .))
      (include "yb-amp-controller.imageTag" (dict "tag" .Values.mcp.image.tag "ctx" .)) -}}
{{- end }}

{{- define "yb-amp-controller.uiImage" -}}
{{- printf "%s:%s"
      (include "yb-amp-controller.imageRepo" (dict "repo" .Values.ui.image.repository "ctx" .))
      (include "yb-amp-controller.imageTag" (dict "tag" .Values.ui.image.tag "ctx" .)) -}}
{{- end }}

{{- define "yb-amp-controller.agentName" -}}
amp-reconcile-agent
{{- end }}

{{- define "yb-amp-controller.agentSelectorLabels" -}}
{{ include "yb-amp-controller.selectorLabels" . }}
app.kubernetes.io/component: reconcile-agent
app: amp-reconcile-agent
{{- end }}

{{- define "yb-amp-controller.agentImage" -}}
{{- printf "%s:%s"
      (include "yb-amp-controller.imageRepo" (dict "repo" .Values.agent.image.repository "ctx" .))
      (include "yb-amp-controller.imageTag" (dict "tag" .Values.agent.image.tag "ctx" .)) -}}
{{- end }}

{{/*
Controller REST base URL the reconcile-agent drives (AMP_CONTROLLER_URL): the
explicit override if set, else the in-cluster controller Service DNS on the api
port, WITH the /api/v1 base path the agent's client expects.
*/}}
{{- define "yb-amp-controller.agentControllerUrl" -}}
{{- if .Values.agent.controllerUrl -}}
{{- .Values.agent.controllerUrl -}}
{{- else -}}
{{- printf "http://%s.%s.svc.cluster.local:%v/api/v1" (include "yb-amp-controller.controllerName" .) .Release.Namespace .Values.controller.ports.api -}}
{{- end -}}
{{- end }}

{{/*
In-cluster URL deployed endpoints use to reach the controller's storage-layer
control surface (AMP_DEPLOY_CONTROLLER_URL). The surface is served on the
controller's API port.
*/}}
{{- define "yb-amp-controller.endpointControllerUrl" -}}
{{- printf "http://%s.%s.svc.cluster.local:%v" (include "yb-amp-controller.controllerName" .) .Release.Namespace .Values.controller.ports.api -}}
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
Controller origin the UI's nginx proxies /api/ to (CONTROLLER_URL): the explicit
override if set, else the in-cluster controller Service DNS on the api port. No
trailing path — nginx forwards the original request URI to it.
*/}}
{{- define "yb-amp-controller.uiControllerUrl" -}}
{{- if .Values.ui.controllerUrl -}}
{{- .Values.ui.controllerUrl -}}
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
