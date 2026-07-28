#!/usr/bin/env bash
#
# YCP build + deploy end-to-end smoke test.
#
# Builds the three images from source, loads them into a throwaway kind cluster,
# applies the real deploy manifest through the ./kustomization.yaml overlay, and
# asserts that:
#   1. every workload rolls out,
#   2. the control plane serves /healthz,
#   3. the agent completes the enrollment handshake against it.
#
# That covers the whole chain the demo depends on -- image build, image start
# (distroless has no shell, so a broken ENTRYPOINT fails here), manifest
# correctness, RBAC, service DNS, Postgres wiring, and the token handshake.
#
# Safety: every kubectl call is pinned with --context to the kind cluster this
# script creates, and it refuses to proceed unless that context resolves to a
# kind node. It never reads or writes your current kubeconfig context.
#
# Usage:
#   ycp/test/e2e/run.sh              # create, test, tear down
#   YCP_E2E_KEEP=1 ycp/test/e2e/run.sh   # leave the cluster up to poke at
#
set -euo pipefail

CLUSTER="${YCP_E2E_CLUSTER:-ycp-e2e}"
CTX="kind-${CLUSTER}"
NS=ycp-demo
TAG="${YCP_E2E_TAG:-e2e}"
KEEP="${YCP_E2E_KEEP:-0}"
PORT="${YCP_E2E_PORT:-18080}"
# ycp log.Fatal's if Postgres is not up yet, so it can crash-loop through a few
# backoffs before its first successful start. Keep this generous.
TIMEOUT="${YCP_E2E_TIMEOUT:-300s}"

# This script lives at ycp/test/e2e/run.sh; all paths below are relative to ycp/.
YCP_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$YCP_DIR"

log() { printf '\n==> %s\n' "$*"; }
die() { printf '\nFAIL: %s\n' "$*" >&2; exit 1; }
k() { kubectl --context "$CTX" "$@"; }

for bin in docker kind kubectl curl; do
  command -v "$bin" >/dev/null 2>&1 || die "$bin is required but not on PATH"
done

diagnose() {
  log "diagnostics (the run failed)"
  k -n "$NS" get pods -o wide 2>&1 || true
  k -n "$NS" describe pods 2>&1 | grep -E '^(Name:|  Warning|  Normal|    Reason|    State|    Last State|    Exit Code)' || true
  for d in ycp meko-agent postgres dp-postgres; do
    echo "--- logs: deploy/$d ---"
    k -n "$NS" logs "deploy/$d" --tail=40 --all-containers --prefix 2>&1 | head -50 || true
  done
  echo "--- events ---"
  k -n "$NS" get events --sort-by=.lastTimestamp 2>&1 | tail -25 || true
}

cleanup() {
  rc=$?
  if [ -n "${PF_PID:-}" ]; then kill "$PF_PID" 2>/dev/null || true; fi
  [ "$rc" -ne 0 ] && diagnose
  if [ "$KEEP" = "1" ]; then
    log "keeping cluster (YCP_E2E_KEEP=1); remove with: kind delete cluster --name $CLUSTER"
  else
    log "tearing down cluster $CLUSTER"
    kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
  fi
  exit "$rc"
}
trap cleanup EXIT

log "building images from source"
docker build -f Dockerfile       -t "ycp:$TAG"        .
docker build -f Dockerfile.agent -t "meko-agent:$TAG" .
docker build -f Dockerfile.dpmcp -t "dpmcp:$TAG"      .

log "creating kind cluster $CLUSTER"
kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
kind create cluster --name "$CLUSTER" --wait 120s

# Context guard: only ever talk to the kind cluster created above. If $CTX somehow
# resolved to a real cluster, its nodes would not be named <cluster>-control-plane.
k get nodes -o name 2>/dev/null | grep -qx "node/${CLUSTER}-control-plane" ||
  die "context guard: --context $CTX is not the kind cluster we just created; refusing to apply"

log "loading images into the cluster"
kind load docker-image "ycp:$TAG" "meko-agent:$TAG" "dpmcp:$TAG" --name "$CLUSTER"

log "applying the deploy manifest (e2e overlay of deploy/ycp-demo.yaml)"
k apply -k test/e2e

# Postgres first: ycp exits if it cannot reach its DB, so waiting here keeps the
# ycp rollout from burning its timeout on crash-loop backoff.
log "waiting for Postgres"
k -n "$NS" rollout status deploy/postgres    --timeout="$TIMEOUT"
k -n "$NS" rollout status deploy/dp-postgres --timeout="$TIMEOUT"

log "waiting for the control plane and agent"
k -n "$NS" rollout status deploy/ycp        --timeout="$TIMEOUT"
k -n "$NS" rollout status deploy/meko-agent --timeout="$TIMEOUT"

# 1. The control plane serves /healthz. The ycp image is distroless (no shell, no
#    curl), so this is checked from the host over a port-forward.
log "checking /healthz"
k -n "$NS" port-forward "svc/ycp" "$PORT:80" >/dev/null 2>&1 &
PF_PID=$!
healthz=""
for _ in $(seq 1 30); do
  if [ "$(curl -fsS "http://127.0.0.1:$PORT/healthz" 2>/dev/null || true)" = "ok" ]; then
    healthz=ok
    break
  fi
  sleep 2
done
[ "$healthz" = "ok" ] || die "/healthz never returned ok"
echo "    /healthz -> ok"

# 2. The agent enrolled. This is the real handshake: the agent reads the shared
#    token from the Secret, reaches ycp over service DNS, and reports cluster
#    facts it had to get from the k8s API through its RBAC.
log "checking agent enrollment"
enrolled=""
for _ in $(seq 1 60); do
  if k -n "$NS" logs deploy/ycp --tail=-1 2>/dev/null | grep -q "agent enrolled: env=itest-mcs"; then
    enrolled=yes
    break
  fi
  sleep 2
done
[ "$enrolled" = "yes" ] || die "agent never enrolled (no 'agent enrolled: env=itest-mcs' in ycp logs)"
k -n "$NS" logs deploy/ycp --tail=-1 | grep "agent enrolled: env=itest-mcs" | tail -1 | sed 's/^/    /'

# 3. The environment is reported live through the API the console reads.
log "checking the fleet API reports the environment"
env_json="$(curl -fsS "http://127.0.0.1:$PORT/api/v1/environments" 2>/dev/null || true)"
echo "$env_json" | grep -q 'itest-mcs' ||
  die "GET /api/v1/environments did not mention itest-mcs; got: ${env_json:0:200}"
echo "    /api/v1/environments includes itest-mcs"

log "PASS — build and deploy verified"
