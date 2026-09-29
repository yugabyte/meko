#!/usr/bin/env bash
# =============================================================================
# End-to-end GKE bring-up for the meko Helm chart, pulling images from AWS ECR.
#
# Captures the full runbook validated on GKE `itest-mcs`:
#   - ECR image-pull secret (cross-cloud AWS ECR -> GKE)
#   - optional throwaway single-node YugabyteDB (via the yugabytedb Helm chart)
#     with the disk-space gflag fix small PVCs need
#   - app + system databases and the required YB extensions
#   - reference Secrets (dummy by default; override via env for real values)
#   - a single-step chart install; the DB migration runs as an initContainer
#     (scope=system) after Helm creates the ConfigMap/SA/Secrets
#   - an internal LoadBalancer for the UI
#
# This is a DEMO/bootstrap script (throwaway YB, dummy secrets). For a real
# deploy see charts/meko/README.md; supply a real DB + real Secrets + real
# Cognito, and skip BRING_UP_YB.
#
# Usage:
#   CONTEXT=gke_yugabyte_us-west1_itest-mcs ./deploy-gke.sh
# Env (all optional; defaults shown):
#   CONTEXT           kube-context (required)
#   NS=meko-system    release namespace
#   RELEASE=meko
#   ECR_PROFILE=meko-dev
#   ECR_REGISTRY=161908593886.dkr.ecr.us-east-2.amazonaws.com
#   ECR_REGION=us-east-2
#   API_TAG=1.0.26-20260710   api-server image (must support --system-db-host)
#   BRING_UP_YB=true          deploy a throwaway YB; false = use EXTERNAL_YB_HOST
#   EXTERNAL_YB_HOST=...       required when BRING_UP_YB=false
#   COGNITO_CLIENT_SECRET=...  real value enables login; placeholder otherwise
#   LANGFUSE_INIT_USER_EMAIL=...     Langfuse demo owner login; see below
#   LANGFUSE_INIT_USER_PASSWORD=...  freshly random per run unless set (MEKO-650:
#                                    never default this to a fixed value — a
#                                    HackerOne report used exactly that to get
#                                    RCE on a real deployment's Langfuse worker)
#   UI_PORT=8600              local port-forward for the UI
# =============================================================================
set -euo pipefail

CONTEXT="${CONTEXT:?set CONTEXT to your kube-context}"
NS="${NS:-meko-system}"
RELEASE="${RELEASE:-meko}"
ECR_PROFILE="${ECR_PROFILE:-meko-dev}"
ECR_REGISTRY="${ECR_REGISTRY:-161908593886.dkr.ecr.us-east-2.amazonaws.com}"
ECR_REGION="${ECR_REGION:-us-east-2}"
API_TAG="${API_TAG:-1.0.26-20260710}"
BRING_UP_YB="${BRING_UP_YB:-true}"
COGNITO_CLIENT_SECRET="${COGNITO_CLIENT_SECRET:-PLACEHOLDER}"
LANGFUSE_INIT_USER_EMAIL="${LANGFUSE_INIT_USER_EMAIL:-demo@local.langfuse}"
LANGFUSE_INIT_USER_PASSWORD="${LANGFUSE_INIT_USER_PASSWORD:-$(openssl rand -hex 16)}"
UI_PORT="${UI_PORT:-8600}"
CHART_DIR="$(cd "$(dirname "$0")/.." && pwd)"
K="kubectl --context=$CONTEXT"
NAMESPACES=(meko-system meko-tenants meko-mcp meko-rag-workers meko-langfuse meko-ingress)
POD_NAMESPACES=(meko-system meko-mcp meko-rag-workers meko-langfuse meko-ingress)

log() { printf '\n\033[1;36m==> %s\033[0m\n' "$*"; }

log "Namespaces"
for ns in "${NAMESPACES[@]}"; do $K create ns "$ns" --dry-run=client -o yaml | $K apply -f - >/dev/null; done

log "ECR image-pull secret (ecr-registry-creds) in every pod namespace"
PW="$(aws ecr get-login-password --profile "$ECR_PROFILE" --region "$ECR_REGION")"
for ns in "${POD_NAMESPACES[@]}"; do
  $K -n "$ns" create secret docker-registry ecr-registry-creds \
    --docker-server="$ECR_REGISTRY" --docker-username=AWS --docker-password="$PW" \
    --dry-run=client -o yaml | $K apply -f - >/dev/null
done

YB_HOST="${EXTERNAL_YB_HOST:-yb-tservers.${NS}.svc.cluster.local}"
if [ "$BRING_UP_YB" = "true" ]; then
  log "YugabyteDB (throwaway single-node) via yugabytedb Helm chart"
  helm repo add yugabytedb https://charts.yugabyte.com >/dev/null 2>&1 || true
  helm repo update yugabytedb >/dev/null 2>&1 || true
  # gflags: small PVCs trip YB's disk-full write rejection + 100GB log-retain floor.
  helm upgrade --install yb yugabytedb/yugabyte --kube-context "$CONTEXT" -n "$NS" \
    --set replicas.master=1,replicas.tserver=1 \
    --set resource.master.requests.cpu=0.3,resource.master.requests.memory=0.5Gi \
    --set resource.tserver.requests.cpu=0.5,resource.tserver.requests.memory=1Gi \
    --set resource.master.limits.cpu=1,resource.master.limits.memory=1Gi \
    --set resource.tserver.limits.cpu=1.5,resource.tserver.limits.memory=2Gi \
    --set storage.master.size=2Gi,storage.tserver.size=5Gi \
    --set enableLoadBalancer=false \
    --set gflags.master.reject_writes_when_disk_full=false \
    --set gflags.tserver.reject_writes_when_disk_full=false \
    --set gflags.master.log_stop_retaining_min_disk_mb=100 \
    --set gflags.tserver.log_stop_retaining_min_disk_mb=100
  log "Waiting for YB tserver/master to be Ready"
  $K -n "$NS" rollout status statefulset/yb-master --timeout=300s
  $K -n "$NS" rollout status statefulset/yb-tserver --timeout=300s
fi

# DB + extension bootstrap only applies to the in-cluster YB (execs into
# yb-tserver-0). With an external DB (BRING_UP_YB=false) create these yourself.
if [ "$BRING_UP_YB" = "true" ]; then
  log "Create databases + required extensions"
  ysql() { $K -n "$NS" exec yb-tserver-0 -c yb-tserver -- ysqlsh -h "$YB_HOST" -U yugabyte -d "$2" -c "$1"; }
  for db in mekodev meko_system langfuse; do ysql "CREATE DATABASE $db" yugabyte 2>/dev/null || true; done
  for db in mekodev meko_system; do
    for ext in '"uuid-ossp"' pgcrypto vector pg_dist_rag; do
      ysql "CREATE EXTENSION IF NOT EXISTS $ext" "$db" || true
    done
    # 'mage' (Apache AGE graph) is a YB preview extension not enabled here; the
    # RAG/memory datapack migration needs it. Login only needs the system schema.
  done
else
  log "BRING_UP_YB=false — skipping DB/extension creation (do this on your external DB)"
fi

log "Reference Secrets (dummy unless overridden; DB creds point at $YB_HOST)"
mk() { $K -n "$1" create secret generic "$2" "${@:3}" --dry-run=client -o yaml | $K apply -f - >/dev/null; }
mk meko-system meko-api-server-cognito --from-literal=cognito-client-secret="$COGNITO_CLIENT_SECRET"
mk meko-system meko-api-server-db --from-literal=db-password=yugabyte
mk meko-system meko-api-server-s3 --from-literal=s3-access-key=x --from-literal=s3-secret-key=x
mk meko-system meko-api-server-llm --from-literal=openai-api-key=x
mk meko-system meko-api-server-system-db --from-literal=db-password=yugabyte
mk meko-system meko-mcp-internal-secret --from-literal=secret-key=sharedsecret123
mk meko-system meko-ui-secret --from-literal=RUDDERSTACK_WRITE_KEY=x
mk meko-system inference-gateway-aws --from-literal=AWS_ACCESS_KEY_ID=x --from-literal=AWS_SECRET_ACCESS_KEY=x
mk meko-mcp yugabytedb-credentials --from-literal=password=yugabyte --from-literal=system_password=yugabyte
mk meko-mcp openai-credentials --from-literal=password=x
mk meko-mcp cognito-client-secret --from-literal=password="$COGNITO_CLIENT_SECRET"
mk meko-mcp meko-mcp-internal-secret --from-literal=secret-key=sharedsecret123
mk meko-rag-workers rag-agent-secret --from-literal=YUGABYTEDB_CONNECTION_STRING=x --from-literal=AWS_ACCESS_KEY_ID=x --from-literal=AWS_SECRET_ACCESS_KEY=x --from-literal=OPENAI_API_KEY=x
mk meko-langfuse langfuse-secrets \
  --from-literal=SALT=saltsaltsalt --from-literal=NEXTAUTH_SECRET=nextauthsecret \
  --from-literal=ENCRYPTION_KEY=0000000000000000000000000000000000000000000000000000000000000000 \
  --from-literal=DATABASE_URL="postgresql://yugabyte:yugabyte@${YB_HOST}:5433/langfuse" \
  --from-literal=TRACING_DATABASE_URL="postgresql://yugabyte:yugabyte@${YB_HOST}:5433/langfuse" \
  --from-literal=LANGFUSE_INIT_USER_EMAIL="$LANGFUSE_INIT_USER_EMAIL" \
  --from-literal=LANGFUSE_INIT_USER_PASSWORD="$LANGFUSE_INIT_USER_PASSWORD"

# Deploy-time overrides: point DBs at the in-cluster YB, no LBs, rag on default pool.
OVR="$(mktemp)"; trap 'rm -f "$OVR"' EXIT; cat > "$OVR" <<EOF
createNamespaces: false
ecrCredentialRefresh: { enabled: false }
ingress: { className: none }
langfuse: { lb: { enabled: false } }
apiServer:
  image: { tag: "${API_TAG}" }
  config:
    DB_HOST: ${YB_HOST}
    DB_SSL_MODE: disable
    SYSTEM_DB_HOST: ${YB_HOST}
    SYSTEM_DB_SSL_MODE: disable
    LANGFUSE_DB_HOST: ${YB_HOST}
    LANGFUSE_DB_SSL_MODE: disable
mekoMcpServer:
  config: { DB_HOST: ${YB_HOST}, SYSTEM_DB_HOST: ${YB_HOST}, MEM0_HOST: ${YB_HOST} }
ragAgent:
  text:
    nodeSelector: { node_type: null, cloud.google.com/gke-nodepool: default-pool }
    resources: { requests: { cpu: 100m, memory: 256Mi, ephemeral-storage: 1Gi } }
  conversations:
    nodeSelector: { node_type: null, cloud.google.com/gke-nodepool: default-pool }
    resources: { requests: { cpu: 100m, memory: 256Mi, ephemeral-storage: 1Gi } }
EOF

# Single-step install. The migrate Job is a POST-install/upgrade hook, so it
# runs after Helm has created the ConfigMap/SA/Secrets — no two-step needed.
# scope=system bootstraps the auth/accounts schema (scope=all also needs the
# graph 'mage' extension, a YB preview feature not enabled here).
log "Install + migrate (migration initContainer, scope=system)"
helm upgrade --install "$RELEASE" "$CHART_DIR" --kube-context "$CONTEXT" -n "$NS" \
  -f "$CHART_DIR/values-gcp.yaml" -f "$CHART_DIR/values-dev.yaml" -f "$OVR" \
  --set mekoUi.lb.enabled=true \
  --set apiServer.migrate.enabled=true --set apiServer.migrate.scope=system \
  --timeout 6m
rm -f "$OVR"

log "Waiting for api-server"
$K -n "$NS" rollout status deploy/meko-api-server --timeout=180s || true
$K -n "$NS" get pods -o wide | grep -v '^yb-' || true

log "Waiting for the UI internal LoadBalancer IP"
LB_IP=""
for i in $(seq 1 20); do
  LB_IP="$($K -n "$NS" get svc meko-ui-lb -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null || true)"
  [ -n "$LB_IP" ] && break; sleep 12
done

cat <<EOF

------------------------------------------------------------------------------
Stack is up. UI internal LoadBalancer: http://${LB_IP:-<pending — kubectl get svc -n $NS meko-ui-lb>}
  (internal LB — reach it from inside the VPC: VPN / peered network / bastion.
   For laptop access without a proxy, flip to external: remove the
   cloud.google.com/load-balancer-type annotation from mekoUi.lb.annotations.)

To enable Cognito login:
  1. re-create meko-api-server-cognito (and meko-mcp/cognito-client-secret) with
     the REAL client secret, then: kubectl -n $NS rollout restart deploy/meko-api-server
  2. register the UI URL (http://${LB_IP:-<lb-ip>}) as an allowed callback on
     the dev Cognito app client. NOTE: Cognito often rejects a bare-IP/HTTP
     callback — put a DNS name + TLS in front of the LB if so.

Langfuse demo owner login (generated fresh this run unless overridden):
  email:    $LANGFUSE_INIT_USER_EMAIL
  password: $LANGFUSE_INIT_USER_PASSWORD

Teardown:
  helm uninstall $RELEASE yb --kube-context $CONTEXT -n $NS
  kubectl --context=$CONTEXT delete crd mekouseraccounts.saas.mekodata.ai
  kubectl --context=$CONTEXT delete ns ${NAMESPACES[*]}
------------------------------------------------------------------------------
EOF
