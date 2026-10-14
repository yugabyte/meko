{{/*
_helpers.tpl — naming, labels, image refs, and the cloud-conditional
ingress/service/cert logic. Individual templates stay clean by delegating
all AWS-vs-GCP branching here.
*/}}

{{/*
meko.mode — the validated deploy mode. Three modes select which component set a
release deploys:
  mcp   (default) — data plane: the full Meko stack (api-server + meko-mcp-server
                    + provisioner + meko-ui + inference-gateway + rag-agent +
                    langfuse). Renders byte-identically to the pre-mode chart.
  ycp            — control plane (BYOC / YCP): api-server run with
                    --control-plane-mode (fleet registry + agent API) plus its
                    console, exposed via an internal LoadBalancer. No data-plane
                    components render.
  agent          — the BYOC pull agent, alone. For a customer/data-plane cluster
                    that enrolls into a YCP but runs none of the Meko stack
                    itself. Every other component is suppressed regardless of its
                    `.enabled` flag: without this, an agent install is seven
                    separate `--set <comp>.enabled=false` flags and forgetting one
                    puts a data-plane workload in someone else's cluster.
*/}}
{{- define "meko.mode" -}}
{{- $mode := .Values.mode | default "mcp" -}}
{{- if not (has $mode (list "mcp" "ycp" "agent")) -}}
{{- fail (printf "mode must be \"mcp\", \"ycp\" or \"agent\", got %q" $mode) -}}
{{- end -}}
{{- $mode -}}
{{- end -}}

{{/*
meko.componentEnabled — is a component active for the current deploy mode?
A component renders when its own `.enabled` is true AND it participates in the
active mode. In `ycp` mode only the control-plane components render; everything
else is data-plane and is suppressed regardless of its `.enabled` flag. Returns
"true" (active) or "" (inactive — falsy in `if`).
Usage: {{- if eq (include "meko.componentEnabled" (dict "root" $ "comp" "mekoUi")) "true" }}
*/}}
{{- define "meko.componentEnabled" -}}
{{- $root := .root -}}
{{- $comp := .comp -}}
{{- $mode := include "meko.mode" $root -}}
{{- $enabled := (index $root.Values $comp).enabled -}}
{{- /* Components that render in control-plane (ycp) mode; all others are data-plane. */ -}}
{{- $controlPlane := list "apiServer" "ecrCredentialRefresh" "controlPlaneDb" -}}
{{- /* Of those, the ones that make no sense in a data plane and are suppressed
       there even if enabled — the mirror of the rule above. */ -}}
{{- $controlPlaneOnly := list "controlPlaneDb" -}}
{{- /* agent mode renders the reconcile-agent and nothing else.
       ecrCredentialRefresh is allowed alongside because a GKE-from-ECR cluster
       needs the pull-credential CronJob for the images it pulls. */ -}}
{{- $agentMode := list "reconcileAgent" "ecrCredentialRefresh" -}}
{{- $active := $enabled -}}
{{- if eq $mode "ycp" -}}
{{- $active = and $enabled (has $comp $controlPlane) -}}
{{- else if eq $mode "agent" -}}
{{- $active = and $enabled (has $comp $agentMode) -}}
{{- else -}}
{{- $active = and $enabled (not (has $comp $controlPlaneOnly)) -}}
{{- end -}}
{{- if $active -}}true{{- end -}}
{{- end -}}

{{/* Base name, overridable via nameOverride/fullnameOverride. */}}
{{- define "meko.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "meko.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- .Chart.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{/* Common labels stamped on every object. */}}
{{- define "meko.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: meko
{{- end -}}

{{/*
Per-component selector labels. Preserves the legacy `app: <name>` selector the
kustomize manifests use, so Services keep matching existing pods across cutover.
Usage: include "meko.selectorLabels" (dict "app" "meko-api-server")
*/}}
{{- define "meko.selectorLabels" -}}
app: {{ .app }}
{{- end -}}

{{/*
Full labels for a component's objects = common + selector.
Usage: include "meko.componentLabels" (dict "app" "meko-api-server")
*/}}
{{- define "meko.componentLabels" -}}
{{ include "meko.selectorLabels" . }}
app.kubernetes.io/managed-by: Helm
app.kubernetes.io/part-of: meko
{{- end -}}

{{/*
meko.imagePullSecrets — pod-spec imagePullSecrets from global.imagePullSecrets
(a list of Secret names). Emits nothing when the list is empty (so EKS, where
node IAM authorizes ECR pulls, renders no imagePullSecrets and parity holds).
On GKE, set the list to the name of a dockerconfigjson Secret holding ECR creds
— it must exist in every namespace the chart deploys pods into.
Output is pre-indented to 6 spaces (pod-spec depth); call it bare on its own line:
      {{- include "meko.imagePullSecrets" . }}
*/}}
{{- define "meko.imagePullSecrets" -}}
{{- with .Values.global.imagePullSecrets }}
      imagePullSecrets:
{{- range . }}
        - name: {{ . }}
{{- end }}
{{- end }}
{{- end -}}

{{/*
meko.image — compose a full image ref from a component image dict and the
global registry. Honors an explicit per-component registry override.
Usage: include "meko.image" (dict "image" .Values.apiServer.image "global" .Values.global)

Both the registry and the tag are required, and a missing one is a template
error rather than a best-effort ref. Every image this helper composes is a
first-party `meko/*` repository in a private registry (public images —
postgres, nginx — are plain strings elsewhere and never come through here), so a
ref with no registry is never what anyone meant: it renders `meko/meko-agent:tag`,
which kubelet resolves against Docker Hub and fails with ImagePullBackOff several
minutes into a deploy. That is the exact shape of issue #42's registry item — an
agent install with only a cloud preset, since `global.imageRegistry` lives in the
ENV presets (dev and prod are different ECR accounts, so it cannot live in the
cloud ones).

To use a registry-less local image on purpose — a kind cluster with side-loaded
images, say — name it explicitly: --set global.imageRegistry=docker.io/library.
*/}}
{{- define "meko.image" -}}
{{- $img := .image -}}
{{- $global := .global -}}
{{- $registry := default $global.imageRegistry $img.registry -}}
{{- $repo := $img.repository -}}
{{- $tag := $img.tag -}}
{{- if not $tag -}}
{{- fail (printf "image tag is required (repository=%s) — set <comp>.image.tag" $repo) -}}
{{- end -}}
{{- if not $registry -}}
{{- fail (printf "image registry is required (repository=%s): set global.imageRegistry, usually by including an env preset (-f values-dev.yaml / -f values-prod.yaml) alongside the cloud preset. For a deliberately unqualified local image, pass --set global.imageRegistry=docker.io/library" $repo) -}}
{{- end -}}
{{- printf "%s/%s:%s" $registry $repo $tag -}}
{{- end -}}

{{/*
meko.ingressAnnotations — emit the correct annotation set for the active
ingress class. Handles ALB (AWS), ingress-nginx, and GKE GCE.
`certArn` (optional) overrides the global ACM cert (ALB). `gke` (optional dict:
managedCert, preSharedCert, staticIp) drives GKE-native TLS/IP wiring.
Usage:
  include "meko.ingressAnnotations" (dict
    "root" $ "healthcheckPath" "/api/v1/health" "certArn" $cert "gke" $c.ingress.gke)
*/}}
{{- define "meko.ingressAnnotations" -}}
{{- $root := .root -}}
{{- $ing := $root.Values.ingress -}}
{{- $hc := .healthcheckPath -}}
{{- $certArn := default $ing.tls.acmCertArn .certArn -}}
{{- $gke := default dict .gke -}}
{{- if eq $ing.className "alb" -}}
alb.ingress.kubernetes.io/scheme: internet-facing
alb.ingress.kubernetes.io/listen-ports: '[{"HTTPS":443}]'
alb.ingress.kubernetes.io/target-type: ip
{{- if .sslRedirect }}
alb.ingress.kubernetes.io/ssl-redirect: "443"
{{- end }}
alb.ingress.kubernetes.io/healthcheck-path: {{ $hc }}
{{- if eq $ing.tls.mode "acm-arn" }}
{{- if not $certArn }}{{ fail "ingress.tls.acmCertArn (or per-component ingress.acmCertArn) required for acm-arn mode" }}{{ end }}
alb.ingress.kubernetes.io/certificate-arn: {{ $certArn }}
{{- end }}
{{- else if eq $ing.className "nginx" -}}
nginx.ingress.kubernetes.io/ssl-redirect: "true"
{{- if eq $ing.tls.mode "cert-manager" }}
{{- if not $ing.tls.clusterIssuer }}{{ fail "ingress.tls.clusterIssuer required for cert-manager mode" }}{{ end }}
cert-manager.io/cluster-issuer: {{ $ing.tls.clusterIssuer }}
{{- end }}
{{- else if eq $ing.className "gce" -}}
kubernetes.io/ingress.class: gce
{{- with $gke.staticIp }}
kubernetes.io/ingress.global-static-ip-name: {{ . }}
{{- end }}
{{- if eq $ing.tls.mode "managed-cert" }}
{{- if $gke.preSharedCert }}
ingress.gcp.kubernetes.io/pre-shared-cert: {{ $gke.preSharedCert }}
{{- else if $gke.managedCert }}
networking.gke.io/managed-certificates: {{ $gke.managedCert }}
{{- else }}{{ fail "gce managed-cert mode needs ingress.gke.managedCert (host) or ingress.gke.preSharedCert (wildcard)" }}{{ end }}
{{- end }}
{{- end -}}
{{- with $ing.annotations }}
{{ toYaml . }}
{{- end }}
{{- end -}}

{{/*
meko.managedCertificate — emit a GKE ManagedCertificate for a host, only when
className=gce, tls.mode=managed-cert, and a managedCert name is set (skipped for
pre-shared / wildcard). Prepend it (with `---`) before the Ingress.
Usage: include "meko.managedCertificate" (dict "root" $ "name" n "namespace" ns "host" h)
*/}}
{{- define "meko.managedCertificate" -}}
{{- $ing := .root.Values.ingress -}}
{{- if and (eq $ing.className "gce") (eq $ing.tls.mode "managed-cert") .name -}}
apiVersion: networking.gke.io/v1
kind: ManagedCertificate
metadata:
  name: {{ .name }}
  namespace: {{ .namespace }}
  labels:
    {{- include "meko.labels" .root | nindent 4 }}
spec:
  domains:
    - {{ .host | quote }}
---
{{ end -}}
{{- end -}}

{{/*
meko.ingressClassField — `ingressClassName: X` for alb/nginx (real IngressClass
objects), nothing for gce (GKE keys off the kubernetes.io/ingress.class annotation).
*/}}
{{- define "meko.ingressClassField" -}}
{{- $cn := .root.Values.ingress.className -}}
{{- if ne $cn "gce" }}ingressClassName: {{ $cn }}{{ end -}}
{{- end -}}

{{/*
meko.ingressTLS — emit the spec.tls block for cert-manager / managed-cert.
ACM handles TLS at the ALB so this emits nothing in acm-arn mode.
Usage: include "meko.ingressTLS" (dict "root" $ "host" "api.mekodata.ai" "secretName" "api-tls")
*/}}
{{- define "meko.ingressTLS" -}}
{{- $ing := .root.Values.ingress -}}
{{- if eq $ing.tls.mode "cert-manager" }}
tls:
  - hosts:
      - {{ .host | quote }}
    secretName: {{ .secretName }}
{{- else if eq $ing.tls.mode "managed-cert" }}
{{- /* GKE ManagedCertificate is referenced via annotation on the ingress; no spec.tls block */ -}}
{{- end -}}
{{- end -}}

{{/*
meko.lbServiceAnnotations — per-cloud LoadBalancer Service annotations,
merged with any component-supplied extras.
Usage: include "meko.lbServiceAnnotations" (dict "root" $ "extra" .Values.langfuse.lb.annotations)
*/}}
{{- define "meko.lbServiceAnnotations" -}}
{{- with .extra }}
{{ toYaml . }}
{{- end }}
{{- end -}}

{{/*
meko.configChecksum — sha256 over a component's rendered ConfigMap/Secret
templates, stamped on the pod template so config changes trigger a rollout
(replaces kustomize name-hash behavior, disabled via disableNameSuffixHash).
Renders the referenced templates with `include` (Files.Get can't read
templates/) and hashes the result.
Usage:
  annotations:
    {{- include "meko.configChecksum" (dict "root" $ "files" (list "api-server/configmap.yaml")) | nindent 4 }}
*/}}
{{- define "meko.configChecksum" -}}
{{- $root := .root -}}
{{- $sum := "" -}}
{{- range .files -}}
{{- $sum = printf "%s%s" $sum (include (print $root.Template.BasePath "/" .) $root) -}}
{{- end -}}
checksum/config: {{ $sum | sha256sum }}
{{- end -}}

{{/*
meko.secretDocChecksum — sha256 of one Secret's rendered document out of
dataplane-secrets.yaml, which renders many. Hashing the manifest rather than the
values keeps a pod rolling on ANY change to that Secret, the way
meko.configChecksum does for a ConfigMap. Empty when no such document renders.
Usage: include "meko.secretDocChecksum" (dict "root" $ "name" "rag-agent-secret")
*/}}
{{- define "meko.secretDocChecksum" -}}
{{- $root := .root -}}
{{- $name := printf "\n  name: %s\n" .name -}}
{{- range splitList "\n---\n" (include (print $root.Template.BasePath "/dataplane-secrets.yaml") $root) -}}
{{- if contains $name (printf "\n%s\n" .) -}}
{{ . | sha256sum }}
{{- end -}}
{{- end -}}
{{- end -}}

{{/*
meko.ns — the namespace a component's objects go in.

With global.singleNamespace, every component collapses into the RELEASE namespace,
whatever its own `namespace` value says. That is what a BYOC data plane needs: one
namespace an operator can grant, inspect, and delete as a unit — and the only shape
the pull agent can install, since it manages exactly the namespace it runs in.

Off (the default), each component keeps its own namespace and the chart renders
exactly as it did before: meko-system, meko-mcp, meko-rag-workers, meko-langfuse.

Usage: {{ include "meko.ns" (dict "root" $ "ns" $c.namespace) }}
*/}}
{{- define "meko.ns" -}}
{{- $root := .root -}}
{{- if $root.Values.global.singleNamespace -}}
{{- $root.Release.Namespace -}}
{{- else -}}
{{- .ns | default $root.Release.Namespace -}}
{{- end -}}
{{- end -}}

{{/*
meko.upstream — an nginx upstream address, with its namespace corrected for
global.singleNamespace.

The defaults are fully-qualified into the namespaces the split layout uses
(meko-api-server-service.meko-system…, langfuse-web.meko-langfuse…). Collapse
everything into one namespace and those names no longer resolve — and nginx does not
degrade, it refuses to start:

  [emerg] host not found in upstream
    "meko-api-server-service.meko-system.svc.cluster.local:80"

so the UI pod crash-loops with the api-server sitting healthy beside it. Rewriting
only the namespace segment keeps the service name and port exactly as configured, and
leaves an address that is not an in-cluster Service DNS name (a hostname, an IP)
untouched.
*/}}
{{- define "meko.upstream" -}}
{{- $root := .root -}}
{{- $addr := .addr -}}
{{- if and $root.Values.global.singleNamespace (contains ".svc.cluster.local" $addr) -}}
{{- $hostport := splitList ":" $addr -}}
{{- $parts := splitList "." (index $hostport 0) -}}
{{- $svc := index $parts 0 -}}
{{- printf "%s.%s.svc.cluster.local" $svc $root.Release.Namespace -}}
{{- if gt (len $hostport) 1 }}:{{ index $hostport 1 }}{{ end -}}
{{- else -}}
{{- $addr -}}
{{- end -}}
{{- end -}}

{{/* The meko api-server the reconcile-agent converges datapacks against.
     reconcileAgent.mekoApiUrl wins; otherwise the in-cluster Service this release
     deploys, addressed by FQDN so the agent works from another namespace. The
     /api/v1 suffix is part of it: the agent dials the path as given, and an
     api-server behind --base-path needs that prefix here too. The FQDN is
     absolute (trailing dot): it has four dots, and ndots:5 would otherwise make
     the resolver walk the whole search list before trying the name as written. */}}
{{- define "meko.reconcileAgentApiUrl" -}}
{{- $root := . -}}
{{- $c := $root.Values.reconcileAgent -}}
{{- if $c.mekoApiUrl -}}
{{- $c.mekoApiUrl -}}
{{- else -}}
{{- $apiNs := include "meko.ns" (dict "root" $root "ns" $root.Values.apiServer.namespace) -}}
{{- /* basePath prefixes the whole api-server, /api/v1 included, so the agent's URL
       has to carry it — the same value the api-server itself is started with. */ -}}
{{- $base := default "" $root.Values.apiServer.basePath | trimSuffix "/" -}}
{{- printf "http://meko-api-server-service.%s.svc.cluster.local.:%v%s/api/v1" $apiNs $root.Values.apiServer.service.port $base -}}
{{- end -}}
{{- end -}}

{{/*
meko.otel.collectorRendered — "true" when templates/otel/collector.yaml renders its
collectors: otel on, a data-plane release, and the operator's CRDs present. For the
templates that point a workload at the collector, so none aims at a Service that
does not exist.
*/}}
{{- define "meko.otel.collectorRendered" -}}
{{- $otel := .Values.otel | default dict -}}
{{- and (dig "enabled" false $otel) (eq (include "meko.mode" .) "mcp") (.Capabilities.APIVersions.Has "opentelemetry.io/v1beta1") -}}
{{- end -}}
