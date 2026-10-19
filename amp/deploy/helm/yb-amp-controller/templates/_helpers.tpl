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

{{/*
The ServiceAccount endpoint pods run as. Empty only when the chart creates none
and names none, and then endpoints keep the namespace's `default`. Read with dig:
a --reuse-values upgrade of a release predating the key has no serviceAccount
map at all, and should still get one.
*/}}
{{- define "yb-amp-controller.endpointServiceAccountCreate" -}}
{{- if dig "serviceAccount" "create" true .Values.controller.endpoint }}true{{ end }}
{{- end }}

{{- define "yb-amp-controller.endpointServiceAccountName" -}}
{{- $name := dig "serviceAccount" "name" "" .Values.controller.endpoint -}}
{{- if $name -}}
{{- $name -}}
{{- else if include "yb-amp-controller.endpointServiceAccountCreate" . -}}
amp-endpoint
{{- end -}}
{{- end }}

{{- define "yb-amp-controller.agentSelectorLabels" -}}
{{ include "yb-amp-controller.selectorLabels" . }}
app.kubernetes.io/component: reconcile-agent
app: amp-reconcile-agent
{{- end }}

{{- /*
Everything wrong with a YCP-managed install's telemetry, one per line, or empty when
there is nothing to say. NOTES.txt prints it — helm has no warn primitive, and NOTES
is the only channel it gives an operator.

These were `fail`s. They are warnings for now because the BYOC door is still being
rolled out fleet by fleet: an install that cannot ship is as likely to be a fleet
problem as an operator mistake, and failing their release over it teaches them to
work around the check. Each line therefore has to carry the fix, since nothing stops
to make them read it.

Only rendered for agent.enabled — a non-YCP install ships to whatever in-cluster
store it was pointed at and none of this applies.
*/}}
{{- define "yb-amp-controller.telemetryWarnings" -}}
{{- $w := list -}}
{{- if dig "enabled" false .Values.agent -}}
{{- if not (dig "enabled" true .Values.otel) -}}
{{- $w = append $w "otel.enabled is false, so nothing collects metrics or logs at all. Set otel.enabled=true." -}}
{{- end -}}
{{- if not (dig "byoc" "enabled" false .Values.otel) -}}
{{- $w = append $w "otel.byoc.enabled is false, so the collectors run but send nowhere the YCP console can read. Take the full --set list from the install hint the console prints." -}}
{{- end -}}
{{- if not (.Capabilities.APIVersions.Has "opentelemetry.io/v1beta1") -}}
{{- $w = append $w "the OpenTelemetry Operator is not installed (no opentelemetry.io/v1beta1 CRDs), so the collectors were skipped entirely and this environment ships nothing. Install it, then helm upgrade." -}}
{{- end -}}
{{- if not (or (dig "collectors" "controller" "enabled" true .Values.otel) (dig "collectors" "compute" "enabled" true .Values.otel)) -}}
{{- $w = append $w "both metrics collectors are disabled, so nothing scrapes amp." -}}
{{- end -}}
{{- if not (dig "collectors" "logs" "enabled" true .Values.otel) -}}
{{- $w = append $w "otel.collectors.logs.enabled is false: byoc ships metrics AND logs, and a YCP-managed environment wants both." -}}
{{- end -}}
{{- if dig "byoc" "enabled" false .Values.otel -}}
{{- if not (dig "byoc" "metricsEnabled" true .Values.otel) -}}
{{- $w = append $w "otel.byoc.metricsEnabled is false: the console reads environment health from these metrics." -}}
{{- end -}}
{{- if not (dig "byoc" "logsEnabled" true .Values.otel) -}}
{{- $w = append $w "otel.byoc.logsEnabled is false: byoc ships metrics AND logs, and a YCP-managed environment wants both." -}}
{{- end -}}
{{- $base := include "yb-amp-controller.byocURL" . -}}
{{- if not $base -}}
{{- $w = append $w (printf "otel.byoc.env is %q: use dev, portal, prod or sre. Nothing ships to the BYOC ingestion endpoint until it is fixed." (dig "byoc" "env" "" .Values.otel)) -}}
{{- else if not (hasPrefix "https://" $base) -}}
{{- $w = append $w (printf "otel.byoc.metricsEndpoint is %q: the BYOC ingestion endpoint is BasicAuth over TLS, and a plaintext URL would put the password on the wire." (dig "byoc" "metricsEndpoint" "" .Values.otel)) -}}
{{- end -}}
{{- end -}}
{{- if or (not (dig "byoc" "username" "" .Values.otel)) (not (dig "byoc" "passwordSecret" "name" "" .Values.otel)) (not (dig "byoc" "passwordSecret" "key" "" .Values.otel)) -}}
{{- $w = append $w "otel.byoc.username or otel.byoc.passwordSecret is unset, so the BYOC ingestion endpoint has no credential and the collectors will not start." -}}
{{- end -}}
{{- end -}}
{{- $w | join "\n" -}}
{{- end }}

{{- /*
The BYOC ingestion endpoint of the YBM environment in otel.byoc.env, or empty
for an unknown env. Empty turns the BYOC exporters off and telemetryWarnings
says why, rather than failing the release.

An otel.byoc.metricsEndpoint set by an older install hint wins, and both signals
use its scheme and host. Otherwise an install pinned to the dev endpoint by that value would
ship logs to the env default. It also keeps the URL the control plane compares at
enroll from moving under an upgrade.
*/}}
{{- define "yb-amp-controller.byocURL" -}}
{{- $legacy := dig "byoc" "metricsEndpoint" "" .Values.otel -}}
{{- if $legacy -}}
{{- regexFind "^[a-z]+://[^/?#]+" $legacy -}}
{{- else -}}
{{- $env := dig "byoc" "env" "prod" .Values.otel | default "prod" -}}
{{- $domains := dict "dev" "devcloud.yugabyte.com" "portal" "cloudportal.yugabyte.com" "prod" "cloud.yugabyte.com" "sre" "srecloud.yugabyte.com" -}}
{{- with get $domains $env -}}
{{- printf "https://byoc.%s" . -}}
{{- end -}}
{{- end -}}
{{- end }}

{{- /* Where the collectors ship metrics, or empty when byoc or its metrics are off.
       An older hint's metricsEndpoint is kept verbatim, since enroll compares it. */}}
{{- define "yb-amp-controller.byocMetricsEndpoint" -}}
{{- if and (dig "byoc" "enabled" false .Values.otel) (dig "byoc" "metricsEnabled" true .Values.otel) -}}
{{- with dig "byoc" "metricsEndpoint" "" .Values.otel -}}
{{- . -}}
{{- else -}}
{{- with include "yb-amp-controller.byocURL" . }}{{ printf "%s/api/v1/otlp/metrics" . }}{{ end -}}
{{- end -}}
{{- end -}}
{{- end }}

{{- /* Where the logs daemonset ships, or empty when byoc or its logs are off. */}}
{{- define "yb-amp-controller.byocLogsEndpoint" -}}
{{- if and (dig "byoc" "enabled" false .Values.otel) (dig "byoc" "logsEnabled" true .Values.otel) -}}
{{- with include "yb-amp-controller.byocURL" . }}{{ printf "%s/api/v1/otlp/logs" . }}{{ end -}}
{{- end -}}
{{- end }}

{{- /*
The CONTROLLER collector's own-telemetry endpoint, for the preflight to read
otelcol_exporter_sent_metric_points off. The OpenTelemetry Operator creates one
`<collector CR name>-collector-monitoring` Service per collector, serving :8888;
the CR names come from otel-collectors.yaml, so the two cannot drift.

ONLY the controller collector, and that is load-bearing: awaitExport requires every
URL it is given to export before it passes, and the compute collector's only targets
are endpoint pods. A freshly installed environment owns none — which is exactly when
the console prints this recipe — so listing it would make the probe report a failure
on every first install, while the door and the controller collector were fine. The
logs daemonset is out for a related reason: it ships only what containers write, so
a quiet environment looks identical to a broken one.

telemetrywatch.go and agent-deployment.yaml exclude the same collectors for the same
reason; the three have to agree.

That Service name is the OPERATOR's convention, not this chart's, so it was
verified against a live collector rather than assumed (itest-dev, operator-managed
contrib 0.158.0: `yb-amp-controller-controller-collector-monitoring` :8888). If a
future operator release renames it the preflight fails every install on a timeout,
which is loud but misleading — check here first.
*/}}
{{- define "yb-amp-controller.preflightCollectorURLs" -}}
{{- $urls := list -}}
{{- if dig "collectors" "controller" "enabled" true .Values.otel -}}
{{- $urls = append $urls (printf "http://%s-controller-collector-monitoring.%s.svc.cluster.local:8888/metrics" (include "yb-amp-controller.fullname" .) .Release.Namespace) -}}
{{- end -}}
{{- $urls | join "," -}}
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

{{/*
Relabel rules copying an endpoint pod's identity labels (the controller's
deploy.IdentityLabels) onto every series scraped from it, so a query can group
by cluster, pool, branch or role without a join. A pod created before these
labels yields an empty value, which Prometheus drops rather than storing.

`branchFill` is for the pageserver and safekeeper, which already put branch_id
on their branch-scoped series (the same id: each serves exactly the pod's one
branch). A target label named branch_id would make Prometheus rename theirs to
exported_branch_id, so the pod's branch goes on as pod_branch_id instead, and
endpointBranchFill moves it into branch_id only on the series that have none.
*/}}
{{- define "yb-amp-controller.endpointIdentityRelabels" -}}
- {source_labels: [__meta_kubernetes_pod_label_amp_yugabyte_com_endpoint_id], target_label: endpoint_id}
- {source_labels: [__meta_kubernetes_pod_label_amp_yugabyte_com_branch_id], target_label: {{ if .branchFill }}pod_branch_id{{ else }}branch_id{{ end }}}
- {source_labels: [__meta_kubernetes_pod_label_amp_yugabyte_com_cluster_id], target_label: cluster_id}
- {source_labels: [__meta_kubernetes_pod_label_amp_yugabyte_com_storage_pool_id], target_label: storage_pool_id}
- {source_labels: [__meta_kubernetes_pod_label_amp_yugabyte_com_ep_type], target_label: ep_type}
{{- end }}

{{/*
metric_relabel_configs for a job that set pod_branch_id (endpointIdentityRelabels
with branchFill). Source labels join with ";", so ";<id>" matches only a series
whose own branch_id is empty: the exporter's value is never overwritten. Then
pod_branch_id is dropped, so it is never stored.
*/}}
{{- define "yb-amp-controller.endpointBranchFill" -}}
- {source_labels: [branch_id, pod_branch_id], regex: ";(.+)", target_label: branch_id, replacement: "$1"}
- {action: labeldrop, regex: pod_branch_id}
{{- end }}
