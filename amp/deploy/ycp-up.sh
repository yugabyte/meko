#!/usr/bin/env bash
#
# Stand up an ephemeral YCP control plane in an existing namespace, register an
# `amp` environment on it, and drop the resulting enrollment token into the Secret
# the yb-amp-controller Helm chart reads — the control-plane half of the
# amp_deploy_gke_ycp smoke path (see .github/workflows/amp-control-plane-gke.yml).
#
# It is deliberately namespace-scoped and self-cleaning by proxy: everything it
# creates lands in $NS, which the workflow deletes at the end of the run. There is
# no PVC and no external Service — the control plane is reached over in-cluster DNS
# (meko-api-server.$NS.svc.cluster.local) by the amp controller, and over a
# transient port-forward by this script (to register) and by smoke-test.sh (to
# assert). Nothing outside the cluster ever talks to it.
#
# In order it:
#   1. ensures the namespace and, if PULL_SECRET_DOCKERCONFIGJSON is set, mints the
#      Quay pull secret (so the api-server image can be pulled from Quay);
#   2. renders ycp/controlplane.yaml (envsubst) and applies it;
#   3. waits for Postgres + api-server to be Ready;
#   4. port-forwards the api-server and POSTs a kind=amp environment, capturing the
#      environment id and the one-time enrollment token;
#   5. creates the Secret amp-controller-ycp-enrollment (key: enrollment-token);
#   6. emits YCP_URL / YCP_ENV_ID / YCP_ADMIN_KEY / YCP_ENROLL_SECRET — appended to
#      $GITHUB_ENV when running in Actions, and always echoed as a summary.
#
# Prereqs: kubectl (context on the target cluster), curl, jq, envsubst, openssl.
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
# Name to register the amp environment under. The control plane slugs it and bakes
# it into the id as "amp-<slug(name)>"; we read that id back rather than recomputing
# the slug here, so the two can never drift.
YCP_ENV_NAME="${YCP_ENV_NAME:-gke-smoke}"
YCP_PLACEMENT="${YCP_PLACEMENT:-cloud}"
YCP_REGION="${YCP_REGION:-us-west1}"
# Admin secret gating the fleet/console routes. Generated per run unless supplied;
# ycp-up.sh, the controller (never — it uses the enrollment token) and smoke-test.sh
# are the only things that see it, and it dies with the namespace.
YCP_ADMIN_KEY="${YCP_ADMIN_KEY:-$(openssl rand -hex 24)}"
# Secret the controller chart reads the enrollment token from (must match the
# controller.ycp.enrollTokenSecret.name that up-gke.sh sets).
YCP_ENROLL_SECRET="${YCP_ENROLL_SECRET:-amp-controller-ycp-enrollment}"

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
for bin in kubectl curl jq envsubst openssl; do
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
say "control plane (Postgres + api_server --control-plane-mode)"
# Scope envsubst to exactly our vars so the k8s $(DB_PASSWORD) refs pass through.
NS="${NS}" YCP_IMAGE="${YCP_IMAGE}" YCP_ADMIN_KEY="${YCP_ADMIN_KEY}" YCP_IPS="${YCP_IPS}" \
  envsubst '${NS} ${YCP_IMAGE} ${YCP_ADMIN_KEY} ${YCP_IPS}' < "${MANIFEST}" \
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

RESP="$(curl -sf -X POST "${CP}/api/v1/environments" \
  -H "X-Meko-Admin-Secret: ${YCP_ADMIN_KEY}" \
  -H 'Content-Type: application/json' \
  -d "{\"name\":\"${YCP_ENV_NAME}\",\"kind\":\"amp\",\"placement\":\"${YCP_PLACEMENT}\",\"region\":\"${YCP_REGION}\"}")" \
  || die "register POST failed"

YCP_ENV_ID="$(printf '%s' "${RESP}" | jq -r '.data.environment.id // empty')"
TOKEN="$(printf '%s' "${RESP}" | jq -r '.data.enrollment_token // empty')"
[ -n "${YCP_ENV_ID}" ] || die "register response had no environment id (resp: ${RESP})"
[ -n "${TOKEN}" ] || die "register response had no enrollment_token (resp: ${RESP})"
echo "   environment id : ${YCP_ENV_ID}"

# --- 5. enrollment-token Secret for the controller chart ---
say "enrollment secret '${YCP_ENROLL_SECRET}' (key enrollment-token)"
kubectl -n "${NS}" create secret generic "${YCP_ENROLL_SECRET}" \
  --from-literal=enrollment-token="${TOKEN}" \
  --dry-run=client -o yaml | kubectl apply -f -

# --- 6. emit outputs ---
YCP_URL="http://meko-api-server.${NS}.svc.cluster.local"
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
emit YCP_ENROLL_SECRET "${YCP_ENROLL_SECRET}"

exit 0
