#!/usr/bin/env bash
#
# Stand up an ephemeral YCP control plane in an existing namespace, register an
# `amp` environment on it, and drop the agent's credentials into the Secret the
# reconcile-agent reads — the control-plane half of the amp_deploy_gke_ycp
# smoke path (see .github/workflows/amp-control-plane-gke.yml).
#
# There is no YBM here to mint a real API key, so the environment is registered
# with a generated api_key_id and the agent runs in dev mode: the Secret carries
# dev-admin-secret + dev-api-key-id, which the agent sends as the headers YBM
# would have stamped after validating a real key.
#
# It is deliberately namespace-scoped and self-cleaning by proxy: everything it
# creates lands in $NS, which the workflow deletes at the end of the run. By default
# there is no PVC and no external Service — the control plane is reached over
# in-cluster DNS (meko-api-server.$NS.svc.cluster.local) by the amp controller, and
# over a transient port-forward by this script (to register) and by smoke-test.sh (to
# assert). Nothing outside the cluster ever talks to it.
#
# For a long-lived dev environment (not the throwaway CI namespace), set
# YCP_POSTGRES_PVC_SIZE (e.g. 5Gi) to back Postgres with a PVC so the fleet registry
# — the environment record, its desired state, and the agent's key binding — survives
# a postgres restart or node recycle. Without it a reschedule wipes the registry and
# every enrolled agent is left unauthorized until re-registered.
#
# In order it:
#   1. ensures the namespace and, if PULL_SECRET_DOCKERCONFIGJSON is set, mints the
#      Quay pull secret (so the api-server image can be pulled from Quay);
#   2. renders ycp/controlplane.yaml (envsubst) and applies it;
#   3. waits for Postgres + api-server to be Ready;
#   4. port-forwards the api-server and POSTs a kind=amp environment with a
#      generated api_key_id, capturing the environment id;
#   5. creates the Secret amp-agent-api-key (keys: dev-admin-secret, dev-api-key-id);
#   6. emits YCP_URL / YCP_ENV_ID / YCP_ADMIN_KEY / YCP_AGENT_SECRET — appended to
#      $GITHUB_ENV when running in Actions, and always echoed as a summary.
#
# Prereqs: kubectl (context on the target cluster), curl, jq, envsubst, openssl, uuidgen.
#
# Usage:
#   NS=ci-pr-1 YCP_IMAGE=quay.io/yugabyte/ycp-api-server:custom-abc \
#     PULL_SECRET=quay-pull-secret \
#     PULL_SECRET_DOCKERCONFIGJSON="$(cat cfg.json)" \
#     amp/deploy/ycp-up.sh
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
MANIFEST="${SCRIPT_DIR}/ycp/controlplane.yaml"

# --- configuration (override via environment) ---
NS="${NS:?NS (target namespace) is required}"
YCP_IMAGE="${YCP_IMAGE:?YCP_IMAGE (ycp-api-server ref, incl tag) is required}"
# Name to register the amp environment under. The control plane slugs it into the id;
# we read that id back rather than recomputing the slug here, so the two can never
# drift — which is what keeps this script correct across a change in the derivation.
# Stable, so a re-run reaches the environment it stood up before. The amp controller
# binds to one environment id for the life of its database, so a name that changed per
# run would strand a controller whose storage outlived the control plane
# (YCP_POSTGRES_PVC_SIZE) against a binding it can never satisfy.
YCP_ENV_NAME="${YCP_ENV_NAME:-gke-smoke}"
YCP_PLACEMENT="${YCP_PLACEMENT:-cloud}"
YCP_REGION="${YCP_REGION:-us-west1}"
# Admin secret gating the fleet/console routes AND, paired with the api key id,
# the agent's dev-mode headers. Generated per run unless supplied; it dies with
# the namespace.
YCP_ADMIN_KEY="${YCP_ADMIN_KEY:-$(openssl rand -hex 24)}"
# Secret the reconcile-agent reads its YCP credentials from (must match the
# agent.apiKeySecret.name that up-gke.sh sets).
YCP_AGENT_SECRET="${YCP_AGENT_SECRET:-amp-agent-api-key}"
# The api key id the environment is bound to. In dev there is no real key — the
# id alone, sent with the admin secret, is the agent's identity.
YCP_API_KEY_ID="${YCP_API_KEY_ID:-$(uuidgen | tr 'A-Z' 'a-z')}"

# Postgres persistence for the control-plane state (fleet registry). Empty ->
# emptyDir: ephemeral, the CI default, wiped on a pod reschedule. A size such as
# "5Gi" -> a PVC named ycp-postgres-data so the registry survives a restart / node
# recycle. YCP_POSTGRES_STORAGE_CLASS picks the class (empty -> cluster default).
YCP_POSTGRES_PVC_SIZE="${YCP_POSTGRES_PVC_SIZE:-}"
YCP_POSTGRES_STORAGE_CLASS="${YCP_POSTGRES_STORAGE_CLASS:-}"

# Pull auth for the api-server image (Quay). Two modes:
#   PULL_SECRET + PULL_SECRET_DOCKERCONFIGJSON -> (re)mint that dockerconfigjson
#                                                 secret from the raw config.json.
#   PULL_SECRET only                           -> use an existing secret of that name.
#   PULL_SECRET empty                          -> no imagePullSecrets (node SA pulls).
PULL_SECRET="${PULL_SECRET:-}"
PULL_SECRET_DOCKERCONFIGJSON="${PULL_SECRET_DOCKERCONFIGJSON:-}"

# Local port for the transient register port-forward.
YCP_PF_PORT="${YCP_PF_PORT:-18090}"

say()  { printf '\n\033[1;36m== %s\033[0m\n' "$*"; }
die()  { printf '\033[1;31merror: %s\033[0m\n' "$*" >&2; exit 1; }

# --- preflight ---
say "preflight"
for bin in kubectl curl jq envsubst openssl uuidgen; do
  command -v "$bin" >/dev/null 2>&1 || die "$bin not found on PATH"
done
[ -f "${MANIFEST}" ] || die "manifest not found: ${MANIFEST}"
CTX="$(kubectl config current-context 2>/dev/null || true)"
[ -n "$CTX" ] || die "no kubectl context set"
echo "   kube-context : ${CTX}"
echo "   namespace    : ${NS}"
echo "   image        : ${YCP_IMAGE}"
echo "   env name     : ${YCP_ENV_NAME}"

# --- 1. namespace + pull secret ---
say "namespace ${NS}"
kubectl create namespace "${NS}" --dry-run=client -o yaml | kubectl apply -f -

YCP_IPS="[]"
if [ -n "${PULL_SECRET}" ]; then
  YCP_IPS="[{name: ${PULL_SECRET}}]"
  if [ -n "${PULL_SECRET_DOCKERCONFIGJSON}" ]; then
    say "pull secret '${PULL_SECRET}' (creating from PULL_SECRET_DOCKERCONFIGJSON)"
    # Route the config.json through a temp file rather than --from-literal so the
    # credential never lands in argv / a process listing.
    _dcj="$(mktemp)"
    printf '%s' "${PULL_SECRET_DOCKERCONFIGJSON}" > "${_dcj}"
    kubectl -n "${NS}" create secret generic "${PULL_SECRET}" \
      --type=kubernetes.io/dockerconfigjson \
      --from-file=.dockerconfigjson="${_dcj}" \
      --dry-run=client -o yaml | kubectl apply -f -
    rm -f "${_dcj}"
  else
    say "pull secret: using existing '${PULL_SECRET}' in namespace ${NS}"
  fi
else
  say "pull secret: none (relying on the node service account to pull ${YCP_IMAGE})"
fi

# --- 2. render + apply the control plane ---
# Postgres data volume: a PVC when YCP_POSTGRES_PVC_SIZE is set, else the emptyDir
# default. The manifest takes it as ${YCP_PG_DATA_VOLUME} in flow style.
if [ -n "${YCP_POSTGRES_PVC_SIZE}" ]; then
  say "postgres persistence: PVC ycp-postgres-data (${YCP_POSTGRES_PVC_SIZE}${YCP_POSTGRES_STORAGE_CLASS:+, class ${YCP_POSTGRES_STORAGE_CLASS}})"
  kubectl apply -f - <<EOF
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: ycp-postgres-data
  namespace: ${NS}
  labels: { app: postgres, app.kubernetes.io/part-of: ycp-smoke }
spec:
  accessModes: ["ReadWriteOnce"]
$([ -n "${YCP_POSTGRES_STORAGE_CLASS}" ] && printf '  storageClassName: %s\n' "${YCP_POSTGRES_STORAGE_CLASS}")
  resources: { requests: { storage: ${YCP_POSTGRES_PVC_SIZE} } }
EOF
  YCP_PG_DATA_VOLUME="persistentVolumeClaim: { claimName: ycp-postgres-data }"
else
  say "postgres persistence: none (emptyDir — wiped on a pod reschedule)"
  YCP_PG_DATA_VOLUME="emptyDir: {}"
fi

say "control plane (Postgres + api_server --control-plane-mode)"
# Scope envsubst to exactly our vars so the k8s $(DB_PASSWORD) refs pass through.
NS="${NS}" YCP_IMAGE="${YCP_IMAGE}" YCP_ADMIN_KEY="${YCP_ADMIN_KEY}" YCP_IPS="${YCP_IPS}" YCP_PG_DATA_VOLUME="${YCP_PG_DATA_VOLUME}" \
  envsubst '${NS} ${YCP_IMAGE} ${YCP_ADMIN_KEY} ${YCP_IPS} ${YCP_PG_DATA_VOLUME}' < "${MANIFEST}" \
  | kubectl apply -f -

# --- 3. wait for readiness ---
say "waiting for control plane to be ready"
kubectl -n "${NS}" rollout status deploy/postgres --timeout=180s
kubectl -n "${NS}" rollout status deploy/meko-api-server --timeout=300s

# --- 4. register the amp environment (via a transient port-forward) ---
say "register amp environment '${YCP_ENV_NAME}'"
kubectl -n "${NS}" port-forward svc/meko-api-server "${YCP_PF_PORT}:80" >/tmp/ycp-up-pf.log 2>&1 &
PF_PID=$!
cleanup() { kill "${PF_PID}" 2>/dev/null || true; }
trap cleanup EXIT

CP="http://127.0.0.1:${YCP_PF_PORT}"
ready=0
for ((i=0; i<60; i++)); do
  if curl -sf "${CP}/api/v1/health" >/dev/null 2>&1; then ready=1; break; fi
  sleep 1
done
[ "${ready}" = 1 ] || die "control plane /api/v1/health never came up (port-forward log: $(cat /tmp/ycp-up-pf.log 2>/dev/null))"

# -w appends the status on its own line after the body, so a failure can report what
# the control plane answered.
REG="$(curl -s -w '\n%{http_code}' -X POST "${CP}/api/v1/environments" \
  -H "X-Meko-Admin-Secret: ${YCP_ADMIN_KEY}" \
  -H 'Content-Type: application/json' \
  -d "{\"name\":\"${YCP_ENV_NAME}\",\"kind\":\"amp\",\"placement\":\"${YCP_PLACEMENT}\",\"region\":\"${YCP_REGION}\",\"api_key_id\":\"${YCP_API_KEY_ID}\"}")"
REG_CODE="$(printf '%s' "${REG}" | tail -n1)"
REG_BODY="$(printf '%s' "${REG}" | sed '$d')"
[ "${REG_CODE}" = "201" ] ||
  die "register ${YCP_ENV_NAME} answered ${REG_CODE}: $(printf '%s' "${REG_BODY}" | jq -r '.error.message // .' 2>/dev/null)"

YCP_ENV_ID="$(printf '%s' "${REG_BODY}" | jq -r '.data.environment.id // empty')"
[ -n "${YCP_ENV_ID}" ] || die "register response had no environment id (resp: ${REG_BODY})"
echo "   environment id : ${YCP_ENV_ID}"

# --- 5. dev-mode credential Secret for the controller chart ---
say "agent secret '${YCP_AGENT_SECRET}' (keys dev-admin-secret, dev-api-key-id)"
kubectl -n "${NS}" create secret generic "${YCP_AGENT_SECRET}" \
  --from-literal=dev-admin-secret="${YCP_ADMIN_KEY}" \
  --from-literal=dev-api-key-id="${YCP_API_KEY_ID}" \
  --dry-run=client -o yaml | kubectl apply -f -

# --- 6. emit outputs ---
# The agent's base URL carries the API base; agent routes hang off it as /agent/*.
YCP_URL="http://meko-api-server.${NS}.svc.cluster.local/api/v1"
say "control plane up — outputs"
emit() {  # emit KEY VALUE  -> $GITHUB_ENV (if in Actions) + stdout
  local k="$1" v="$2"
  echo "   ${k}=${v}"
  [ -n "${GITHUB_ENV:-}" ] && printf '%s=%s\n' "${k}" "${v}" >> "${GITHUB_ENV}"
  return 0
}
# The admin key is a live credential; mask it in Actions logs.
[ -n "${GITHUB_ENV:-}" ] && echo "::add-mask::${YCP_ADMIN_KEY}"
emit YCP_URL "${YCP_URL}"
emit YCP_ENV_ID "${YCP_ENV_ID}"
emit YCP_ADMIN_KEY "${YCP_ADMIN_KEY}"
emit YCP_AGENT_SECRET "${YCP_AGENT_SECRET}"

exit 0
