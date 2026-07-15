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
#     and storage_controller schemas live there and persist across re-deploys)
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
#   ENDPOINT_SERVICE_TYPE  ClusterIP | NodePort | LoadBalancer        (default: ClusterIP)
#
# Endpoint data volume:
#   ENDPOINT_STORAGE_CLASS  storage class for each endpoint's /data PVC (EBS/PD);
#                           empty (default) => an ephemeral emptyDir
#   ENDPOINT_DISK_SIZE      PVC size when ENDPOINT_STORAGE_CLASS is set (default: 10Gi)
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

# Endpoint Service exposure: ClusterIP (default) | NodePort | LoadBalancer.
# ClusterIP/NodePort avoid a per-endpoint cloud LB (CreateEndpoint returns at once);
# NodePort reports <nodeIP>:<nodePort> as the endpoint's client address.
ENDPOINT_SERVICE_TYPE="${ENDPOINT_SERVICE_TYPE:-ClusterIP}"
# Annotations for a LoadBalancer endpoint Service (e.g. internal-LB); empty for ClusterIP.
ENDPOINT_SERVICE_ANNOTATIONS="${ENDPOINT_SERVICE_ANNOTATIONS:-}"

# Endpoint data volume: the storage class each endpoint's /data PVC is
# provisioned from (a network block volume — EBS on AWS, Persistent Disk on GCP),
# of size ENDPOINT_DISK_SIZE. Empty => an ephemeral emptyDir (no persistent
# volume). The class must exist in the cluster (`kubectl get storageclass`).
ENDPOINT_STORAGE_CLASS="${ENDPOINT_STORAGE_CLASS:-}"
ENDPOINT_DISK_SIZE="${ENDPOINT_DISK_SIZE:-10Gi}"

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
echo ">> endpoint storage class=${ENDPOINT_STORAGE_CLASS:-<emptyDir>}  disk size=${ENDPOINT_DISK_SIZE}"

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
  "controller.service.type=${CONTROLLER_EXPOSE}"
  "controller.service.nodePortValue=${CONTROLLER_NODEPORT}"
  "mcp.enabled=${MCP_ENABLED}"
  "mcp.image.repository=${MCP_IMAGE_REPOSITORY}"
  "mcp.image.tag=${MCP_TAG}"
  "ui.enabled=${UI_ENABLED}"
  "ui.image.repository=${UI_IMAGE_REPOSITORY}"
  "ui.image.tag=${UI_TAG}"
)

# --set-string for the DSN ('?' + '=' would be mis-tokenized by --set). The
# Service annotation lists go via the values file below.
HELM_SET_STRING=(
  "controller.databaseUrl=${DB_URL}"
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
if [ -n "${CTRL_ANNOTATIONS}" ] || [ -n "${ENDPOINT_SERVICE_ANNOTATIONS}" ]; then
  HELM_VALUES_FILE="$(mktemp)"
  {
    echo "controller:"
    if [ -n "${CTRL_ANNOTATIONS}" ]; then
      echo "  service:"
      echo "    annotations:"
      emit_csv_list "      " "${CTRL_ANNOTATIONS}"
    fi
    if [ -n "${ENDPOINT_SERVICE_ANNOTATIONS}" ]; then
      echo "  endpoint:"
      echo "    service:"
      echo "      annotations:"
      emit_csv_list "        " "${ENDPOINT_SERVICE_ANNOTATIONS}"
    fi
  } >"${HELM_VALUES_FILE}"
fi

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
