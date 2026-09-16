#!/usr/bin/env bash
#
# Re-deploy the amp controller (cAmp control plane + its managed storage
# controller) to Kubernetes, plus the controller's MCP server, via the
# yb-amp-controller Helm chart. The chart wires the controller to deploy
# compute endpoints; the controller's own exposure (CONTROLLER_EXPOSE) and the
# endpoints' Service type (ENDPOINT_SERVICE_TYPE) are both configurable;
# defaults avoid cloud LBs.
#
# Idempotent: `helm upgrade --install`, so re-running updates in place. Helm
# also prunes the now-stale external Service when CONTROLLER_EXPOSE changes
# (the old hand-rolled `kubectl delete svc` cleanup is no longer needed).
#
# Assumes these already exist in the namespace (NOT created here):
#   - the namespace
#   - the backing YugabyteDB (the controller + endpoints connect to it; the controller
#     and registry schemas live there and persist across re-deploys)
#
# It DOES (re)mint the ECR image pull secret each run (the ~12h token would otherwise
# go stale), resolving AWS creds via deploy/ecr-creds.sh (env keys > AWS_BOT_JSON >
# SSO). The amp/controller + amp/endpoint images must already be pushed to ECR.
#
# The images must already be pushed to ECR. To rebuild first:
#   make controller-bundle && docker build -f deploy/controller/Dockerfile -t <repo>/amp/controller:<tag> . && docker push <repo>/amp/controller:<tag>
#   docker build -t <repo>/amp/controller-ui:<tag> amp_ui && docker push <repo>/amp/controller-ui:<tag>
# then run this with CONTROLLER_TAG=<tag> (UI_TAG defaults to it).
#
# Usage:  deploy/controller/redeploy.sh
#   override any default via env, e.g.:  NS=amp CONTROLLER_TAG=latest deploy/controller/redeploy.sh
#   namespace also reads AMP_NAMESPACE (NS takes precedence); default is amp.
#   image repo paths default to amp/controller + amp/endpoint (override via
#   CONTROLLER_IMAGE_REPO / ENDPOINT_IMAGE_REPO).
#
# Exposure (avoid slow cloud LBs):
#   CONTROLLER_EXPOSE   nodeport | internal-lb | external-lb | none   (default: internal-lb)
#   ENDPOINT_EXPOSE     clusterip | nodeport | internal-lb | external-lb (wrapper)
#   ENDPOINT_SERVICE_TYPE  ClusterIP | NodePort | LoadBalancer        (default: ClusterIP)
#
# Endpoint data volume:
#   ENDPOINT_STORAGE_CLASS  storage class for each endpoint's /data PVC (EBS/PD);
#                           empty (default) => an ephemeral emptyDir
#   ENDPOINT_DISK_SIZE      PVC size when ENDPOINT_STORAGE_CLASS is set (default: 10Gi)
#   ENDPOINT_DISK_SIZE_OVERRIDE
#                           flat /data size for EVERY endpoint regardless of CPU,
#                           bypassing the 20 GiB-per-vCPU policy (empty = keep it).
#                           For a test cluster whose nodes are too small to back
#                           the ratio; leave empty in prod
#   ENDPOINT_MEMORY_GIB_PER_VCPU
#                           GiB of memory per endpoint vCPU (default 2 here, 8 in
#                           the product). Fills in the memory when a create names
#                           only CPU, and is the floor every request is held to.
#                           Lowered for a test cluster that packs many small
#                           tenants; its endpoints then run with less cache than
#                           the product intends, which is the trade
#   ENDPOINT_NODE_SELECTOR  "k=v,k=v" node labels endpoint pods must schedule onto;
#                           leave ENDPOINT_STORAGE_CLASS empty and point this at a
#                           local-NVMe-SSD ephemeral pool to put /data on NVMe
# The in-cluster ClusterIP Service (amp-controller, API + SC port) is always
# created. ClusterIP endpoints avoid a per-endpoint cloud LB entirely (reach via
# Service DNS in-cluster, or `kubectl port-forward` from outside).
#
# MCP server: deployed as its own pod (amp-controller-mcp) in the same chart,
# reaching the controller over in-cluster Service DNS, and exposed the same way
# as the controller (CONTROLLER_EXPOSE). Toggle with MCP_ENABLED; image via
# MCP_IMAGE_REPO / MCP_TAG (defaults alongside the controller image).
#
# UI: the amp_ui SPA (nginx), its own pod (amp-controller-ui), reverse-proxying
# /api/ to the controller over in-cluster Service DNS, and exposed the same way
# as the controller (CONTROLLER_EXPOSE). Toggle with UI_ENABLED; image via
# UI_IMAGE_REPO / UI_TAG (defaults alongside the controller image).
#
# Shared ingress: set INGRESS_ENABLED=true to put endpoints on a port of a
# SHARED load balancer instead of one each. It installs the first shard of each
# configured family beside the controller, so capacity exists as soon as the
# controller is up. Off (the default) installs no shard and leaves the ingress_*
# modes refused.
#   INGRESS_ENABLED         true to route endpoints through shared shards
#   INGRESS_ZONE_INTERNAL   DNS zone internal endpoint names are published under
#   INGRESS_ZONE_EXTERNAL   DNS zone external endpoint names are published under;
#                           kept separate so an internal name never reaches a
#                           public zone
#   INGRESS_PORT_RANGE      "lo-hi" the shard serves (default 20000-20049)
#   INGRESS_CLOUD           aws|gcp|azure; required, the shard will not guess
#   INGRESS_PROXY_PROTOCOL  true to trust PROXY protocol from the load balancer,
#                           without which per-endpoint allowlists see the LB
#   INGRESS_ALLOWED_CIDRS   "cidr,cidr" coarse gate on the shard's LB
# It also needs external-dns with its CRD source on, or the DNS records each
# endpoint renders are inert. See controller/architecture/ac-ingress.md.
#
# To stand up the prerequisites (namespace, pull secret, single-node YugabyteDB)
# from scratch and then deploy the controller, use deploy/up.sh instead.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHART_DIR="${SCRIPT_DIR}/../helm/yb-amp-controller"
# Shared ECR credential resolution + pull-secret (re)mint (see deploy/ecr-creds.sh).
source "${SCRIPT_DIR}/../ecr-creds.sh"

# --- configuration (override via environment) ---
# Namespace: NS wins, then AMP_NAMESPACE, then the default.
NS="${NS:-${AMP_NAMESPACE:-amp}}"
PULL_SECRET="${PULL_SECRET:-pull-secret}"
ECR="${ECR:-454529406029.dkr.ecr.us-west-2.amazonaws.com}"
REGION="${REGION:-${AWS_REGION:-${AWS_DEFAULT_REGION:-us-west-2}}}"  # for `aws ecr get-login-password`
# Default image tag = this build's version tag (release "v<VERSION>", or
# "v<VERSION>-dev.<commit>[-dirty]" for a dev build; "latest" if VERSION is
# absent) — centralised in scripts/version.sh. Override via CONTROLLER_TAG /
# ENDPOINT_TAG; set RELEASE=1 for the clean release tag.
_VERSION_TAG="$("${SCRIPT_DIR}/../../scripts/version.sh" image-tag 2>/dev/null || echo latest)"
CONTROLLER_TAG="${CONTROLLER_TAG:-${_VERSION_TAG}}"   # amp/controller image tag
ENDPOINT_TAG="${ENDPOINT_TAG:-${_VERSION_TAG}}"       # amp/endpoint image tag endpoints run
# Backing YugabyteDB (reused; not created here).
YB_HOST_PORTS="${YB_HOST_PORTS:-yb-tserver.${NS}.svc.cluster.local:5433}"
DB_URL="${DB_URL:-postgres://yugabyte@yb-tserver.${NS}.svc.cluster.local:5433/yb_storage?sslmode=disable}"
# TLS=1 mounts a cert-manager client certificate into the controller (tls.*) and
# has it render one into every endpoint (controller.endpoint.ybTlsMode). The
# issuer and CA bundle ConfigMap come from deploy/tls/trust-root.sh; DB_URL must
# then carry the matching sslmode/sslrootcert/sslcert/sslkey (see up-gke.sh).
TLS="${TLS:-0}"
TLS_ISSUER_KIND="${TLS_ISSUER_KIND:-ClusterIssuer}"
TLS_ISSUER_NAME="${TLS_ISSUER_NAME:-amp-ca}"
TLS_ISSUER_GROUP="${TLS_ISSUER_GROUP:-cert-manager.io}"
TLS_CA_CONFIGMAP="${TLS_CA_CONFIGMAP:-amp-ca-bundle}"
TLS_MODE="${TLS_MODE:-verify-full}"
# YSQL role endpoints connect as; with TLS it is also the CN of their certificate.
ENDPOINT_YB_USER="${ENDPOINT_YB_USER:-yugabyte}"
# Shared ingress (controller/architecture/ac-ingress.md). INGRESS_ENABLED is the
# whole opt-in: off (the default) installs no shard and leaves the controller
# refusing the ingress_* connectivity modes, which is every deployment today.
# The zones are separate and optional; a family with one gets published names.
INGRESS_ZONE_INTERNAL="${INGRESS_ZONE_INTERNAL:-}"
INGRESS_ZONE_EXTERNAL="${INGRESS_ZONE_EXTERNAL:-}"
INGRESS_ENABLED="${INGRESS_ENABLED:-$([ -n "${INGRESS_ZONE_INTERNAL}${INGRESS_ZONE_EXTERNAL}" ] && echo true || echo false)}"
INGRESS_PORT_RANGE="${INGRESS_PORT_RANGE:-20000-20049}"
# The cloud whose load-balancer annotations the shard renders. Required for an
# internal shard: every provider defaults a LoadBalancer Service to
# internet-facing, so the chart refuses to guess.
INGRESS_CLOUD="${INGRESS_CLOUD:-}"
# Trust PROXY protocol on the shard's Postgres entryPoints. Needed behind a load
# balancer that preserves the client address that way. Note that per-endpoint
# allowlists are NOT yet available on the ingress_* modes — the API refuses
# allowed_cidrs there — so INGRESS_ALLOWED_CIDRS below is currently the only
# source filter on a shard, and it applies to every endpoint on it.
INGRESS_PROXY_PROTOCOL="${INGRESS_PROXY_PROTOCOL:-false}"
# Who may assert a client address. The load balancer's own range and nothing
# wider — anything in this list can claim to be any client, walking past every
# per-endpoint allowlist on the shard. The chart refuses to render PROXY protocol
# without it, so there is no accidental trust-everything default.
INGRESS_PROXY_TRUSTED_IPS="${INGRESS_PROXY_TRUSTED_IPS:-}"
# Mirror of the Traefik image, for a cluster that pulls only from its own
# registry. Empty uses the chart's default (Docker Hub), which is anonymous and
# rate-limited for a proxy in every endpoint's connection path.
INGRESS_TRAEFIK_REPO="${INGRESS_TRAEFIK_REPO:-}"
# Coarse outer gate on the shard's load balancer, comma-separated CIDRs. Per
# endpoint filtering is done by each endpoint's own middleware; this is the union
# the shard should ever serve.
INGRESS_ALLOWED_CIDRS="${INGRESS_ALLOWED_CIDRS:-}"

# Internal-LB annotations (GKE: a private VPC IP). Applied to the controller LB
# when CONTROLLER_EXPOSE=internal-lb, and to endpoints via ENDPOINT_SERVICE_ANNOTATIONS.
LB_ANNOTATIONS="${LB_ANNOTATIONS:-networking.gke.io/load-balancer-type=Internal,networking.gke.io/internal-load-balancer-allow-global-access=true}"

# How to expose the controller UI/API externally:
#   nodeport     — a NodePort Service (node-ip:3xxxx), no cloud LB, VPC-reachable
#   internal-lb  — internal LoadBalancer (private VPC IP), uses LB_ANNOTATIONS
#   external-lb  — public LoadBalancer (public IP)
#   none         — in-cluster ClusterIP only (reach via `kubectl port-forward`)
CONTROLLER_EXPOSE="${CONTROLLER_EXPOSE:-internal-lb}"
# Fixed nodePort for CONTROLLER_EXPOSE=nodeport (30000–32767; empty = auto-assign).
CONTROLLER_NODEPORT="${CONTROLLER_NODEPORT:-}"

# Endpoint Service exposure. ENDPOINT_EXPOSE is a friendly wrapper (mirrors
# CONTROLLER_EXPOSE): clusterip (default) | nodeport | internal-lb | external-lb.
# Unset => honor an explicit ENDPOINT_SERVICE_TYPE (ClusterIP|NodePort|LoadBalancer).
# ClusterIP/NodePort avoid a per-endpoint cloud LB (CreateEndpoint returns at once);
# NodePort reports <nodeIP>:<nodePort> as the endpoint's client address; internal-lb
# gives each endpoint a private VPC LB IP (uses LB_ANNOTATIONS).
ENDPOINT_EXPOSE="${ENDPOINT_EXPOSE:-}"
case "${ENDPOINT_EXPOSE}" in
  clusterip)   ENDPOINT_SERVICE_TYPE="ClusterIP";    ENDPOINT_SERVICE_ANNOTATIONS="${ENDPOINT_SERVICE_ANNOTATIONS:-}" ;;
  nodeport)    ENDPOINT_SERVICE_TYPE="NodePort";     ENDPOINT_SERVICE_ANNOTATIONS="${ENDPOINT_SERVICE_ANNOTATIONS:-}" ;;
  internal-lb) ENDPOINT_SERVICE_TYPE="LoadBalancer"; ENDPOINT_SERVICE_ANNOTATIONS="${ENDPOINT_SERVICE_ANNOTATIONS:-${LB_ANNOTATIONS}}" ;;
  external-lb) ENDPOINT_SERVICE_TYPE="LoadBalancer"; ENDPOINT_SERVICE_ANNOTATIONS="${ENDPOINT_SERVICE_ANNOTATIONS:-}" ;;
  "")          ENDPOINT_SERVICE_TYPE="${ENDPOINT_SERVICE_TYPE:-ClusterIP}"; ENDPOINT_SERVICE_ANNOTATIONS="${ENDPOINT_SERVICE_ANNOTATIONS:-}" ;;
  *) echo "ERROR: ENDPOINT_EXPOSE must be clusterip|nodeport|internal-lb|external-lb, got '${ENDPOINT_EXPOSE}'" >&2; exit 1 ;;
esac

# Endpoint data volume: the storage class each endpoint's /data PVC is
# provisioned from (a network block volume — EBS on AWS, Persistent Disk on GCP),
# of size ENDPOINT_DISK_SIZE. Empty => an ephemeral emptyDir (no persistent
# volume). The class must exist in the cluster (`kubectl get storageclass`).
ENDPOINT_STORAGE_CLASS="${ENDPOINT_STORAGE_CLASS:-}"
ENDPOINT_DISK_SIZE="${ENDPOINT_DISK_SIZE:-10Gi}"
# Flat /data size for every endpoint whatever its CPU, bypassing the per-vCPU
# ratio and the floor above. Empty (the default) keeps CPU-proportional sizing.
ENDPOINT_DISK_SIZE_OVERRIDE="${ENDPOINT_DISK_SIZE_OVERRIDE:-}"
# GiB of endpoint memory per vCPU: fills in the memory when a create names only
# CPU, and is the floor every request is held to. 0 = the product's own 8.
#
# 2 here, for a cluster whose nodes cannot back 8 GiB/vCPU. The trade is real —
# endpoints deployed this way run with less cache than a production endpoint, so
# they are not the place to measure cache behaviour.
#
# Not what the QA suites use. Those clusters are stood up by ampstack, which
# renders the chart itself and passes its own value (env.ampstack.
# endpoint_memory_gib_per_vcpu); this script covers deployments made through
# deploy/up.sh.
#
# This reaches deployments that go through this script (deploy/up.sh). It does
# NOT reach the Jenkins portal pipeline: that job passes its own Helm values and
# has no parameter for this, so a portal deployed through it runs the product
# default. Verified by deploying hsunder/endpoint-memory-per-vcpu to a portal and
# asking it for 2 cpu / 4Gi — refused, naming 8 GiB/vCPU. Lowering it there means
# adding the parameter to that job.
ENDPOINT_MEMORY_GIB_PER_VCPU="${ENDPOINT_MEMORY_GIB_PER_VCPU:-2}"
# Node labels endpoint pods must schedule onto, as a "k=v,k=v" CSV. Leave
# ENDPOINT_STORAGE_CLASS empty and set this to a local-NVMe-SSD ephemeral pool
# (e.g. "cloud.google.com/gke-ephemeral-storage-local-ssd=true") to put each
# endpoint's emptyDir /data on NVMe. Empty => no selector (schedules anywhere).
ENDPOINT_NODE_SELECTOR="${ENDPOINT_NODE_SELECTOR:-}"

# Image repo paths within the registry; the canonical repos are amp/controller +
# amp/endpoint. Override via CONTROLLER_IMAGE_REPO / ENDPOINT_IMAGE_REPO if needed.
CONTROLLER_IMAGE_REPO="${CONTROLLER_IMAGE_REPO:-amp/controller}"
ENDPOINT_IMAGE_REPO="${ENDPOINT_IMAGE_REPO:-amp/endpoint}"

# MCP server: its own pod in this chart. MCP_ENABLED toggles it; the image
# defaults to amp/mcp in the same registry, at MCP_TAG (which defaults to the
# controller tag so the two roll together).
MCP_ENABLED="${MCP_ENABLED:-true}"
MCP_IMAGE_REPO="${MCP_IMAGE_REPO:-amp/mcp}"
MCP_TAG="${MCP_TAG:-${CONTROLLER_TAG}}"

# UI: its own pod in this chart. UI_ENABLED toggles it; the image defaults to
# amp/controller-ui in the same registry, at UI_TAG (which defaults to the
# controller tag so they roll together).
UI_ENABLED="${UI_ENABLED:-true}"
UI_IMAGE_REPO="${UI_IMAGE_REPO:-amp/controller-ui}"
UI_TAG="${UI_TAG:-${CONTROLLER_TAG}}"

# Validate exposure mode early (the chart also validates, but fail fast here).
case "${CONTROLLER_EXPOSE}" in
  nodeport|internal-lb|external-lb|none) ;;
  *)
    echo "ERROR: invalid CONTROLLER_EXPOSE=${CONTROLLER_EXPOSE} (want nodeport|internal-lb|external-lb|none)" >&2
    exit 1
    ;;
esac

# Full registry+repo (no tag) for the controller image and the endpoint image
# the controller deploys; the chart appends the tag via .image.tag.
CONTROLLER_IMAGE_REPOSITORY="${ECR}/${CONTROLLER_IMAGE_REPO}"
ENDPOINT_IMAGE_REPOSITORY="${ECR}/${ENDPOINT_IMAGE_REPO}"
MCP_IMAGE_REPOSITORY="${ECR}/${MCP_IMAGE_REPO}"
UI_IMAGE_REPOSITORY="${ECR}/${UI_IMAGE_REPO}"

echo ">> namespace=${NS}  controller=${CONTROLLER_IMAGE_REPOSITORY}:${CONTROLLER_TAG}  endpoint=${ENDPOINT_IMAGE_REPOSITORY}:${ENDPOINT_TAG}"
echo ">> mcp=${MCP_ENABLED} (${MCP_IMAGE_REPOSITORY}:${MCP_TAG})  ui=${UI_ENABLED} (${UI_IMAGE_REPOSITORY}:${UI_TAG})  expose=${CONTROLLER_EXPOSE}"
echo ">> endpoint storage class=${ENDPOINT_STORAGE_CLASS:-<emptyDir>}  disk size=${ENDPOINT_DISK_SIZE}  disk override=${ENDPOINT_DISK_SIZE_OVERRIDE:-<per-vCPU>}"

# Sanity: warn (don't fail) if the namespace is missing (up.sh / an admin creates it).
kubectl get ns "${NS}" >/dev/null 2>&1 || echo "WARN: namespace ${NS} not found"
# (Re)mint the ECR pull secret so a redeploy into an idle namespace doesn't
# ImagePullBackOff on a stale (~12h) token. Resolves AWS creds via ecr-creds.sh.
echo ">> ECR pull secret '${PULL_SECRET}' (refreshing — the ECR token is ~12h)"
ensure_pull_secret "${NS}" "${ECR}" "${PULL_SECRET}" "${REGION}"

# --set flags map the env-var interface onto the chart's values keys.
HELM_SET=(
  "imagePullSecrets[0].name=${PULL_SECRET}"
  "controller.image.repository=${CONTROLLER_IMAGE_REPOSITORY}"
  "controller.image.tag=${CONTROLLER_TAG}"
  "controller.endpoint.image.repository=${ENDPOINT_IMAGE_REPOSITORY}"
  "controller.endpoint.image.tag=${ENDPOINT_TAG}"
  "controller.endpoint.ybHostPorts=${YB_HOST_PORTS}"
  "controller.endpoint.service.type=${ENDPOINT_SERVICE_TYPE}"
  "controller.endpoint.storageClass=${ENDPOINT_STORAGE_CLASS}"
  "controller.endpoint.diskSize=${ENDPOINT_DISK_SIZE}"
  "controller.endpoint.diskSizeOverride=${ENDPOINT_DISK_SIZE_OVERRIDE}"
  "controller.endpoint.memoryGiBPerVCPU=${ENDPOINT_MEMORY_GIB_PER_VCPU}"
  "controller.endpoint.ybUser=${ENDPOINT_YB_USER}"
  "controller.endpoint.ingress.enabled=${INGRESS_ENABLED}"
  "controller.endpoint.ingress.zoneInternal=${INGRESS_ZONE_INTERNAL}"
  "controller.endpoint.ingress.zoneExternal=${INGRESS_ZONE_EXTERNAL}"
  "controller.endpoint.ingress.portRange=${INGRESS_PORT_RANGE}"
  "controller.service.type=${CONTROLLER_EXPOSE}"
  # cert-manager is owned outside this chart: deploy/tls/trust-root.sh installs
  # it in TLS mode, and a plaintext deploy needs none. Leaving the chart default
  # on would make the release a second owner and collide with an existing install.
  "controller.service.nodePortValue=${CONTROLLER_NODEPORT}"
  "mcp.enabled=${MCP_ENABLED}"
  "mcp.image.repository=${MCP_IMAGE_REPOSITORY}"
  "mcp.image.tag=${MCP_TAG}"
  "ui.enabled=${UI_ENABLED}"
  "ui.image.repository=${UI_IMAGE_REPOSITORY}"
  "ui.image.tag=${UI_TAG}"
)
if [ "${TLS}" = "1" ]; then
  HELM_SET+=(
    "tls.enabled=true"
    "tls.issuerRef.kind=${TLS_ISSUER_KIND}"
    "tls.issuerRef.name=${TLS_ISSUER_NAME}"
    "tls.issuerRef.group=${TLS_ISSUER_GROUP}"
    "tls.caBundleConfigMap=${TLS_CA_CONFIGMAP}"
    "controller.endpoint.ybTlsMode=${TLS_MODE}"
  )
fi

# --set-string for the DSN ('?' + '=' would be mis-tokenized by --set). The
# Service annotation lists go via the values file below.
#
# Commas are escaped: helm's --set-string grammar splits on them, so a multi-host
# DSN ("postgres://u@t0:5433,t1:5433,t2:5433/db" — which is how you give the
# controller more than one tserver to fail over to) would arrive as three
# fragments and the install would fail on the ones with no "=".
HELM_SET_STRING=(
  "controller.databaseUrl=${DB_URL//,/\\,}"
)

# The controller + endpoint Service annotations are YAML lists of "k=v" strings
# in the chart, but this script's env interface passes them as CSVs (and the GKE
# keys contain dots that --set treats as path separators). Render them into a
# tiny values file and pass it with -f, sidestepping --set entirely. The
# controller LB annotations apply only to internal-lb.
HELM_VALUES_FILE=""
# Keep the trap's final status truthy: a falsey last command in an EXIT trap
# would clobber the script's own exit code.
cleanup() { [ -n "${HELM_VALUES_FILE}" ] && rm -f "${HELM_VALUES_FILE}"; return 0; }
trap cleanup EXIT

# emit_csv_list <indent> <csv>: print each "k=v" of the CSV as a quoted YAML list item.
emit_csv_list() {
  local indent="$1" csv="$2" item
  local -a _items
  IFS=',' read -ra _items <<<"${csv}"
  for item in "${_items[@]}"; do
    [ -n "${item}" ] && printf '%s- "%s"\n' "${indent}" "${item}"
  done
}

CTRL_ANNOTATIONS=""
[ "${CONTROLLER_EXPOSE}" = "internal-lb" ] && CTRL_ANNOTATIONS="${LB_ANNOTATIONS}"
if [ -n "${CTRL_ANNOTATIONS}" ] || [ -n "${ENDPOINT_SERVICE_ANNOTATIONS}" ] || [ -n "${ENDPOINT_NODE_SELECTOR}" ]; then
  HELM_VALUES_FILE="$(mktemp)"
  {
    echo "controller:"
    if [ -n "${CTRL_ANNOTATIONS}" ]; then
      echo "  service:"
      echo "    annotations:"
      emit_csv_list "      " "${CTRL_ANNOTATIONS}"
    fi
    if [ -n "${ENDPOINT_SERVICE_ANNOTATIONS}" ] || [ -n "${ENDPOINT_NODE_SELECTOR}" ]; then
      echo "  endpoint:"
      if [ -n "${ENDPOINT_SERVICE_ANNOTATIONS}" ]; then
        echo "    service:"
        echo "      annotations:"
        emit_csv_list "        " "${ENDPOINT_SERVICE_ANNOTATIONS}"
      fi
      if [ -n "${ENDPOINT_NODE_SELECTOR}" ]; then
        echo "    nodeSelector:"
        emit_csv_list "      " "${ENDPOINT_NODE_SELECTOR}"
      fi
    fi
  } >"${HELM_VALUES_FILE}"
fi

# The chart declares cert-manager as a subchart, and helm refuses to render a
# chart whose declared dependency is not in charts/ even when its condition is
# false. Chart.lock pins the version; this fetches it.

HELM_ARGS=(upgrade --install yb-amp-controller "${CHART_DIR}" -n "${NS}")
for s in "${HELM_SET[@]}"; do HELM_ARGS+=(--set "${s}"); done
for s in "${HELM_SET_STRING[@]}"; do HELM_ARGS+=(--set-string "${s}"); done
[ -n "${HELM_VALUES_FILE}" ] && HELM_ARGS+=(-f "${HELM_VALUES_FILE}")

echo ">> helm ${HELM_ARGS[*]}"
helm "${HELM_ARGS[@]}"

echo ">> waiting for rollout..."
kubectl -n "${NS}" rollout status deploy/amp-controller --timeout=180s
if [ "${MCP_ENABLED}" = "true" ]; then
  kubectl -n "${NS}" rollout status deploy/amp-controller-mcp --timeout=180s
fi
if [ "${UI_ENABLED}" = "true" ]; then
  kubectl -n "${NS}" rollout status deploy/amp-controller-ui --timeout=180s
fi

# The baseline ingress shard, installed alongside the control plane rather than
# built by the controller on demand. A deployment with ingress enabled therefore
# is not left cold: the load-balancer build starts at deploy time rather than on
# the first endpoint create, which is what would otherwise block a create for
# minutes.
#
# It is NOT ready the moment the controller is: EnsureShard writes the row
# `building`, and the controller's 15s reconcile promotes it once the cloud has
# given the Service an address. Until then an ingress_* create fails with
# ErrNoCapacity, by design.
#
# The controller registers the matching ingress_shards row at startup and marks
# it ready once this Service has an address — the shard name is the join between
# the two, which is why it is fixed rather than generated.
if [ "${INGRESS_ENABLED}" = "true" ]; then
  # Checked here rather than at the shard install, which happens AFTER the
  # controller has been upgraded with ingress on — failing there leaves a
  # deployment whose API accepts the ingress_* modes with no shard to place them.
  case "${INGRESS_CLOUD}" in
    aws|gcp|azure) ;;
    *)
      echo "!! INGRESS_ENABLED=true needs INGRESS_CLOUD=aws|gcp|azure (got '${INGRESS_CLOUD}')." >&2
      echo "   Required for BOTH families: internal would otherwise default to" >&2
      echo "   internet-facing, and external would get a classic load balancer" >&2
      echo "   instead of the NLB this design needs." >&2
      exit 1 ;;
  esac
  if [ -z "${INGRESS_ZONE_INTERNAL}${INGRESS_ZONE_EXTERNAL}" ]; then
    echo "!! INGRESS_ENABLED=true needs at least one of INGRESS_ZONE_INTERNAL or" >&2
    echo "   INGRESS_ZONE_EXTERNAL. An endpoint's address is its published name, so" >&2
    echo "   the controller refuses to start without one and would crashloop." >&2
    exit 1
  fi

  # The Traefik CRDs, before any shard. The shard chart renders Traefik directly
  # rather than through its upstream chart, which is what used to install these —
  # so they are a prerequisite now, and their absence is quiet in the worst way:
  # the shard comes up healthy and its provider serves nothing, while every
  # ingress_* endpoint release fails at apply with "no matches for kind
  # IngressRouteTCP", after its slot has already been allocated.
  #
  # Pinned separately from the image, because they are different version series:
  # traefik/traefik-helm-chart is tagged by CHART version (v34.x, v37.x…), while
  # traefik.image.tag in our values is the proxy's own version (v3.7.13). Reading
  # one from the other gives a ref that does not resolve, so this is its own knob
  # and has to be bumped alongside the image.
  #
  # --force-conflicts because a cluster that once ran the upstream traefik chart
  # has CRDs owned by that release's field manager; without it server-side apply
  # refuses them and the deploy stops on exactly the clusters most likely to have
  # them.
  TRAEFIK_CHART_VERSION="${TRAEFIK_CHART_VERSION:-v37.1.2}"
  echo ">> installing the Traefik CRDs (chart ${TRAEFIK_CHART_VERSION})"
  if ! kubectl apply --server-side --force-conflicts -k \
    "https://github.com/traefik/traefik-helm-chart/traefik/crds/?ref=${TRAEFIK_CHART_VERSION}"; then
    echo "!! could not install the Traefik CRDs. Without them the shard installs and serves" >&2
    echo "   nothing, and every ingress_* endpoint create fails at apply. Check that" >&2
    echo "   TRAEFIK_CHART_VERSION (${TRAEFIK_CHART_VERSION}) is a real tag of" >&2
    echo "   traefik/traefik-helm-chart — it is the CHART version, not the image version." >&2
    exit 1
  fi

  # One shard per family that has a zone, matching what the controller registers
  # at startup — the shard NAME is the join between the row and this release, so
  # installing pgin-0 while the controller registered pgin-0-ext would leave both
  # halves waiting on each other forever.
  #
  # With no zone at all (a kind or CI cluster, where endpoints are addressed by
  # their shard) the internal family is the default: an external shard there
  # would be an internet-facing load balancer nobody asked for.
  INGRESS_FAMILIES=()
  [ -n "${INGRESS_ZONE_INTERNAL}" ] && INGRESS_FAMILIES+=("internal:pgin-0")
  [ -n "${INGRESS_ZONE_EXTERNAL}" ] && INGRESS_FAMILIES+=("external:pgin-0-ext")

  for _fam in "${INGRESS_FAMILIES[@]}"; do
    _family="${_fam%%:*}"
    _shard="${_fam##*:}"
    echo ">> installing ingress shard ${_shard} (${_family}, ${INGRESS_PORT_RANGE})"
    INGRESS_SET=(
      "shardName=${_shard}"
      "family=${_family}"
      "cloud=${INGRESS_CLOUD}"
      "ports.lo=${INGRESS_PORT_RANGE%%-*}"
      "ports.hi=${INGRESS_PORT_RANGE##*-}"
      "proxyProtocol.enabled=${INGRESS_PROXY_PROTOCOL}"
      # The chart's pull-secret hook: the Traefik image is the only public-registry
      # image in amp/deploy, so a cluster that mirrors it needs the same secret
      # every other release here gets.
      "imagePullSecrets[0].name=${PULL_SECRET}"
    )
    if [ -n "${INGRESS_TRAEFIK_REPO}" ]; then
      INGRESS_SET+=("traefik.image.repository=${INGRESS_TRAEFIK_REPO}")
    fi
    if [ -n "${INGRESS_PROXY_TRUSTED_IPS}" ]; then
      _t=0
      # shellcheck disable=SC2001
      for _ip in $(echo "${INGRESS_PROXY_TRUSTED_IPS}" | tr ',' ' '); do
        INGRESS_SET+=("proxyProtocol.trustedIPs[${_t}]=${_ip}")
        _t=$((_t + 1))
      done
    fi
    if [ -n "${INGRESS_ALLOWED_CIDRS}" ]; then
      _i=0
      # shellcheck disable=SC2001
      for _cidr in $(echo "${INGRESS_ALLOWED_CIDRS}" | tr ',' ' '); do
        INGRESS_SET+=("allowedCidrs[${_i}]=${_cidr}")
        _i=$((_i + 1))
      done
    fi
    _ingress_args=()
    for _s in "${INGRESS_SET[@]}"; do _ingress_args+=(--set "${_s}"); done
    # No --wait: helm counts a type=LoadBalancer Service as unready until the
    # cloud populates .status.loadBalancer.ingress, so waiting turns a normal
    # multi-minute LB build into a failed deploy — the very cold start this
    # design exists to keep off the endpoint path. The controller converges the
    # shard on its own (ReconcileShards promotes it once it has an address), so
    # the rollout below is the only readiness worth blocking on.
    helm upgrade --install "${_shard}" "${SCRIPT_DIR}/../helm/yb-amp-ingress" \
      -n "${NS}" "${_ingress_args[@]}" --timeout 5m
    kubectl -n "${NS}" rollout status "deploy/${_shard}" --timeout=180s
    SHARD_ADDR="$(kubectl -n "${NS}" get svc "${_shard}" \
      -o jsonpath='{.status.loadBalancer.ingress[0].hostname}{.status.loadBalancer.ingress[0].ip}' 2>/dev/null || true)"
    if [ -n "${SHARD_ADDR}" ]; then
      echo "   shard ${_shard} at ${SHARD_ADDR}, ports ${INGRESS_PORT_RANGE}"
    else
      echo "   shard ${_shard} installed; its load balancer is still provisioning."
      echo "   The controller marks it ready once it has an address — until then"
      echo "   an ingress_* create fails with 'no ingress capacity', by design."
    fi
  done
fi

echo ">> amp controller is up. Access (UI at /, API under /api/v1):"
case "${CONTROLLER_EXPOSE}" in
  internal-lb|external-lb)
    LBIP="$(kubectl -n "${NS}" get svc amp-controller-lb -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null || true)"
    if [ -n "${LBIP}" ]; then
      echo "   ${CONTROLLER_EXPOSE} (UI/API):  http://${LBIP}:8080"
    else
      echo "   ${CONTROLLER_EXPOSE} IP: still provisioning — check: kubectl -n ${NS} get svc amp-controller-lb -w"
    fi
    ;;
  nodeport)
    NP="$(kubectl -n "${NS}" get svc amp-controller-np -o jsonpath='{.spec.ports[0].nodePort}' 2>/dev/null || true)"
    # Prefer an ExternalIP (matches the controller's own node-IP resolver), else InternalIP.
    NODEIP="$(kubectl get nodes -o jsonpath='{.items[*].status.addresses[?(@.type=="ExternalIP")].address}' 2>/dev/null | awk '{print $1}' || true)"
    [ -n "${NODEIP}" ] || NODEIP="$(kubectl get nodes -o jsonpath='{.items[*].status.addresses[?(@.type=="InternalIP")].address}' 2>/dev/null | awk '{print $1}' || true)"
    echo "   NodePort (UI/API):  http://<any-node-ip>:${NP:-<pending>}   (reachable at any node IP)"
    [ -n "${NODEIP}" ] && [ -n "${NP}" ] && echo "      e.g.  http://${NODEIP}:${NP}"
    ;;
esac
echo "   or port-forward:       kubectl -n ${NS} port-forward svc/amp-controller 8080:8080   then http://localhost:8080"
if [ "${UI_ENABLED}" = "true" ]; then
  echo ">> amp_ui SPA is up (its own pod, proxies /api/ to the controller):"
  case "${CONTROLLER_EXPOSE}" in
    internal-lb|external-lb)
      UILBIP="$(kubectl -n "${NS}" get svc amp-controller-ui-lb -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null || true)"
      if [ -n "${UILBIP}" ]; then
        echo "   ${CONTROLLER_EXPOSE} (UI):  http://${UILBIP}"
      else
        echo "   ${CONTROLLER_EXPOSE} IP: still provisioning — check: kubectl -n ${NS} get svc amp-controller-ui-lb -w"
      fi
      ;;
    nodeport)
      UINP="$(kubectl -n "${NS}" get svc amp-controller-ui-np -o jsonpath='{.spec.ports[0].nodePort}' 2>/dev/null || true)"
      echo "   NodePort (UI):  http://<any-node-ip>:${UINP:-<pending>}   (reachable at any node IP)"
      ;;
  esac
  echo "   or port-forward:       kubectl -n ${NS} port-forward svc/amp-controller-ui 8081:80   then http://localhost:8081"
fi
if [ "${MCP_ENABLED}" = "true" ]; then
  echo "   MCP server (HTTP /mcp): kubectl -n ${NS} port-forward svc/amp-controller-mcp 8000:8000   then http://localhost:8000/mcp"
fi

# A bare `[ ... ]` as the script's final command would leak its (possibly
# falsey) status as our exit code; make success explicit.
exit 0
