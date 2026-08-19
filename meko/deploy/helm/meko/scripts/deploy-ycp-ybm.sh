#!/usr/bin/env bash
# =============================================================================
# deploy-ycp-ybm.sh — lightweight sibling of deploy-gke.sh that deploys ONLY the
# YCP (Yugabyte Control Plane / MCCP) component to a GKE cluster, backed by an
# external YugabyteDB Aeon (YBM) database. Images pulled from AWS ECR with static
# keys (GKE has no AWS IAM). Follows charts/meko/YCP-RUNBOOK.md.
#
# mode: ycp → api-server --control-plane-mode + internal-LB console; NO data plane
# (meko-ui/mcp-server/provisioner/rag/langfuse suppressed by the mode). Composes
# values-gcp + values-dev + values-ycp (LAST) + a small override that sets the image
# tag, disables the bundled DB, and points both DB handles at the one YBM cluster.
#
# Namespace is ycp-system (NOT meko-system: the data plane's api-server Deployment
# shares that name and Helm won't adopt it).
#
# Env (defaults shown; REQUIRED noted):
#   CONTEXT       kube-context (default: current-context)
#   CHART_DIR     meko chart dir (default: this script's ../ = charts/meko)
#   NS=ycp-system   RELEASE=ycp
#   API_TAG       REQUIRED — carries --control-plane-mode + -only-controlplane-migrate
#                 and the web console (>= 1.0.36-20260729).
#   ECR_AWS_ACCESS_KEY_ID / ECR_AWS_SECRET_ACCESS_KEY  REQUIRED — meko-account keys
#                 with ECR read; used to bootstrap AND refresh the pull Secret.
#   ECR_REGISTRY=161908593886.dkr.ecr.us-east-2.amazonaws.com   ECR_REGION=us-east-2
#   DB_HOST       REQUIRED — YBM/Aeon YSQL endpoint (serves both app + system DB).
#   DB_PORT=5433  DB_USER=yugabyte  DB_NAME=yugabyte  DB_SSL_MODE=require
#   DB_PASSWORD   REQUIRED — the DB user's password.
#                 Reachability: the infra egress firewall must allow tcp:$DB_PORT to
#                 the DB, and the DB allow-list must include the infra egress IPs.
#   ADMIN_API_KEY  gates the console/fleet routes (auto-generated + PRINTED if unset —
#                  save it; those routes 503 without it and it can't be recovered).
#   MCP_INTERNAL_SECRET  auto-generated if unset.
#   BASE_PATH     serve the whole api-server under a URL prefix, e.g. /ycp for
#                 https://devcloud.yugabyte.com/ycp/. Empty (default) = domain
#                 root. Only set this once the fronting proxy forwards the prefix
#                 through UNSTRIPPED — with it set, the internal LB serves the
#                 console at $LB_IP/ycp/ and 404s at /. Needs an API_TAG with
#                 --base-path support.
#   DRY_RUN=false  preview only (kubectl client dry-run + helm --dry-run; no AWS/DB).
#   WAIT_LB=true   poll for the internal LB IP at the end.
# =============================================================================
set -euo pipefail

CONTEXT="${CONTEXT:-$(kubectl config current-context)}"
CHART_DIR="${CHART_DIR:-$(cd "$(dirname "$0")/.." && pwd)}"
NS="${NS:-ycp-system}"
RELEASE="${RELEASE:-ycp}"
BASE_PATH="${BASE_PATH:-}"
API_TAG="${API_TAG:?set API_TAG (>= 1.0.36-20260729: --control-plane-mode, -only-controlplane-migrate, console)}"
ECR_REGISTRY="${ECR_REGISTRY:-161908593886.dkr.ecr.us-east-2.amazonaws.com}"
ECR_REGION="${ECR_REGION:-us-east-2}"
ECR_AWS_ACCESS_KEY_ID="${ECR_AWS_ACCESS_KEY_ID:-}"
ECR_AWS_SECRET_ACCESS_KEY="${ECR_AWS_SECRET_ACCESS_KEY:-}"
DB_HOST="${DB_HOST:-}"
DB_PORT="${DB_PORT:-5433}"
DB_USER="${DB_USER:-yugabyte}"
DB_NAME="${DB_NAME:-yugabyte}"
DB_SSL_MODE="${DB_SSL_MODE:-require}"
DB_PASSWORD="${DB_PASSWORD:-}"
ADMIN_API_KEY="${ADMIN_API_KEY:-}"
MCP_INTERNAL_SECRET="${MCP_INTERNAL_SECRET:-}"
DRY_RUN="${DRY_RUN:-false}"
WAIT_LB="${WAIT_LB:-true}"

K="kubectl --context=$CONTEXT"
log() { printf '\n\033[1;36m==> %s\033[0m\n' "$*"; }
# DRY_RUN: applies become client dry-runs; helm gets --dry-run. Nothing is created.
kapply() { if [ "$DRY_RUN" = "true" ]; then $K apply --dry-run=client -f - ; else $K apply -f - >/dev/null ; fi ; }
DRY_FLAG=""; [ "$DRY_RUN" = "true" ] && DRY_FLAG="--dry-run"
rand() { openssl rand -hex "${1:-24}" 2>/dev/null || head -c "$((${1:-24}*2))" /dev/urandom | od -An -tx1 | tr -d ' \n'; }

[ -f "$CHART_DIR/values-ycp.yaml" ] || { echo "CHART_DIR=$CHART_DIR has no values-ycp.yaml — need a meko chart with ycp mode."; exit 1; }
if [ "$DRY_RUN" != "true" ]; then
  : "${DB_HOST:?set DB_HOST (the YBM/Aeon YSQL endpoint)}"
  : "${DB_PASSWORD:?set DB_PASSWORD}"
  : "${ECR_AWS_ACCESS_KEY_ID:?set ECR_AWS_ACCESS_KEY_ID (meko-account ECR key)}"
  : "${ECR_AWS_SECRET_ACCESS_KEY:?set ECR_AWS_SECRET_ACCESS_KEY}"
fi

# Admin key + MCP secret: generate if not supplied (admin key is printed below).
GEN_ADMIN=false
[ -z "$ADMIN_API_KEY" ] && { ADMIN_API_KEY="$(rand 24)"; GEN_ADMIN=true; }
[ -z "$MCP_INTERNAL_SECRET" ] && MCP_INTERNAL_SECRET="$(rand 24)"

log "Target: context=$CONTEXT ns=$NS release=$RELEASE mode=ycp db=$DB_USER@${DB_HOST:-<dry-run>}:$DB_PORT api_tag=$API_TAG base_path=${BASE_PATH:-<root>}"
[ "$DRY_RUN" != "true" ] || log "DRY_RUN: preview only — nothing will be created or modified"
[ "$DRY_RUN" = "true" ] || $K cluster-info >/dev/null

log "Namespace $NS"
$K create ns "$NS" --dry-run=client -o yaml | kapply

# --- ECR image-pull auth (static keys; GKE has no AWS IAM) ------------------
# Bootstrap ecr-registry-creds now; the chart's ecr-token-refresher CronJob keeps it
# fresh from aws-ecr-refresher-creds (ECR tokens expire ~12h).
log "ECR pull Secret + refresher creds in $NS"
if [ "$DRY_RUN" = "true" ]; then
  PW="DRY_RUN"
else
  PW="$(AWS_ACCESS_KEY_ID="$ECR_AWS_ACCESS_KEY_ID" AWS_SECRET_ACCESS_KEY="$ECR_AWS_SECRET_ACCESS_KEY" \
        aws ecr get-login-password --region "$ECR_REGION")"
fi
$K -n "$NS" create secret docker-registry ecr-registry-creds \
  --docker-server="$ECR_REGISTRY" --docker-username=AWS --docker-password="$PW" \
  --dry-run=client -o yaml | kapply
$K -n "$NS" create secret generic aws-ecr-refresher-creds \
  --from-literal=AWS_ACCESS_KEY_ID="${ECR_AWS_ACCESS_KEY_ID:-dryrun}" \
  --from-literal=AWS_SECRET_ACCESS_KEY="${ECR_AWS_SECRET_ACCESS_KEY:-dryrun}" \
  --dry-run=client -o yaml | kapply

# --- Secrets (the chart references these; it holds no secret material) -------
log "Secrets in $NS"
mk() { $K -n "$NS" create secret generic "$1" "${@:2}" --dry-run=client -o yaml | kapply; }
mk meko-api-server-db        --from-literal=db-password="${DB_PASSWORD:-dryrun}"
mk meko-api-server-system-db --from-literal=db-password="${DB_PASSWORD:-dryrun}"
mk meko-mcp-internal-secret  --from-literal=secret-key="$MCP_INTERNAL_SECRET"
mk meko-admin-api-key        --from-literal=admin-api-key="$ADMIN_API_KEY"

# --- Deploy-time overrides (composed AFTER values-ycp so they win) ----------
# controlPlaneDb off (external DB); both DB handles → the one YBM cluster; ECR
# refresher scoped to $NS (chart default targets all meko namespaces).
OVR="$(mktemp)"; trap 'rm -f "$OVR"' EXIT
cat > "$OVR" <<EOF
controlPlaneDb: { enabled: false }
apiServer:
  namespace: ${NS}
  image: { tag: "${API_TAG}" }
  basePath: "${BASE_PATH}"
  config:
    DB_HOST: ${DB_HOST}
    DB_PORT: "${DB_PORT}"
    DB_USER_NAME: ${DB_USER}
    DB_NAME: ${DB_NAME}
    DB_SSL_MODE: ${DB_SSL_MODE}
    SYSTEM_DB_HOST: ${DB_HOST}
    SYSTEM_DB_PORT: "${DB_PORT}"
    SYSTEM_DB_USER_NAME: ${DB_USER}
    SYSTEM_DB_NAME: ${DB_NAME}
    SYSTEM_DB_SSL_MODE: ${DB_SSL_MODE}
ecrCredentialRefresh:
  namespace: ${NS}
  namespaces: ["${NS}"]
EOF

log "helm upgrade --install $RELEASE (values: gcp + dev + ycp; migrate scope=controlplane from values-ycp)"
helm upgrade --install "$RELEASE" "$CHART_DIR" --kube-context "$CONTEXT" -n "$NS" \
  -f "$CHART_DIR/values-gcp.yaml" \
  -f "$CHART_DIR/values-dev.yaml" \
  -f "$CHART_DIR/values-ycp.yaml" \
  -f "$OVR" \
  $DRY_FLAG \
  --timeout 10m

if [ "$DRY_RUN" = "true" ]; then
  log "DRY_RUN complete — the manifests above are what WOULD be created (nothing applied)."
  exit 0
fi

log "Waiting for api-server (migrate init connects to YBM + runs controlplane migrations)"
$K -n "$NS" rollout status deploy/meko-api-server --timeout=300s || true
$K -n "$NS" get pods -o wide 2>/dev/null || true

LB_IP=""
if [ "$WAIT_LB" = "true" ]; then
  log "Waiting for the YCP internal LoadBalancer (meko-api-server-lb) IP"
  for _ in $(seq 1 20); do
    LB_IP="$($K -n "$NS" get svc meko-api-server-lb -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null || true)"
    [ -n "$LB_IP" ] && break; sleep 12
  done
fi

cat <<EOF

------------------------------------------------------------------------------
YCP control plane is up in $NS (release $RELEASE), backed by YBM $DB_HOST.
  Internal LB (console + fleet/agent API): http://${LB_IP:-<pending — kubectl -n $NS get svc meko-api-server-lb>}
  Console at /  (prompts for the admin secret). Health: /api/v1/health
  Reach it from inside the VPC (VPN / peered network / bastion), or:
    kubectl -n $NS port-forward svc/meko-api-server-service 8080:80
$([ "$GEN_ADMIN" = "true" ] && printf '\n  ADMIN API KEY (generated — SAVE THIS; it gates the console/fleet routes\n  and cannot be recovered):\n    %s\n' "$ADMIN_API_KEY")
Teardown:
  helm uninstall $RELEASE --kube-context $CONTEXT -n $NS && kubectl --context=$CONTEXT delete ns $NS
------------------------------------------------------------------------------
EOF
