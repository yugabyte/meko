#!/usr/bin/env bash
#
# Stand up a Meko data plane the way an operator does — the chart installs the stack,
# the reconcile-agent converges the datapacks YCP declares — and prove the property
# the whole design rests on: the agent touches its own datapacks and nothing else.
#
# This is the kind=meko half of what ycp-reconcile-smoke.sh does for amp. The
# difference from full-stack.sh, which it otherwise resembles, is which agent runs:
# there the meko-agent reconciles a seeded meko-stack into k8s, here the environment
# is registered declaring NOTHING, helm brings the stack up, and the reconcile-agent
# converges `datapack` resources over the api-server's REST API.
#
# In order it:
#   1. builds the reconcile-agent image from this tree and side-loads it (the Quay
#      tag is only pushed when CI holds QUAY_AMP_TOKEN, so nothing here depends on it);
#   2. registers a kind=meko environment with "desired": [] — no stack seeded;
#   3. helm-installs the chart with values-byoc.yaml + reconcileAgent.enabled=true,
#      which is the command the control plane's own install hint prints;
#   4. creates a datapack directly through the api-server, as a person would in the
#      UI, so there is something the agent must NOT touch;
#   5. declares two datapacks in desired state and waits for them to appear;
#   6. withdraws one and waits for it to be garbage-collected;
#   7. asserts the person's datapack survived every step.
#
# It does not create a cluster. Run the e2e first and keep it:
#
#   YCP_E2E_KEEP=1 ycp/test/e2e/run.sh
#   AWS_PROFILE=meko-dev ycp/test/local/meko-reconcile.sh
#
# Environment:
#   MEKO_API_TAG / MEKO_MCP_TAG / MEKO_UI_TAG
#                   Stack image tags. They default to the release pinned in
#                   meko/api_server/internal/service/dataplane_release.go, and they
#                   are NOT the same tag — each component is versioned separately, so
#                   one shared value renders three references that cannot be pulled.
#   AWS_PROFILE     a profile that can read the Meko ECR. Every stack image is private.
#   OPENAI_API_KEY  optional; without it the MCP server fails on the first embedding.
#                   Datapack reconciliation does not need it.
#   MEKO_RECONCILE_NS    namespace for the data plane (default ycp-meko-reconcile).
#   MEKO_RECONCILE_ENV   environment name (default carries a timestamp, so re-runs
#                        never collide with a previous row).
#   YCP_E2E_CLUSTER      kind cluster name (default ycp-e2e).
#   YCP_E2E_BASE_PATH    subpath the control plane is mounted under (default /ycp).
#
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$HERE/../../.." && pwd)"
CLUSTER="${YCP_E2E_CLUSTER:-ycp-e2e}"
CTX="kind-${CLUSTER}"
CP_NS=ycp-e2e
NS="${MEKO_RECONCILE_NS:-ycp-meko-reconcile}"
ENV_NAME="${MEKO_RECONCILE_ENV:-meko-reconcile-$(date +%s)}"
ADMIN_KEY=e2eadminkey
PORT="${YCP_LOCAL_PORT:-18084}"
BASE="${YCP_E2E_BASE_PATH-/ycp}"
CHART="$REPO_ROOT/meko/deploy/helm/meko"
ECR="161908593886.dkr.ecr.us-east-2.amazonaws.com"
ECR_REGION=us-east-2
PULL_SECRET=meko-ecr
AGENT_IMAGE="ycp-reconciler-agent:local"
# docker or podman, whichever this machine has. kind needs to be told too: with the
# podman provider `kind load docker-image` cannot reach the image, so the image goes
# in as an archive instead.
DOCKER="${DOCKER:-$(command -v docker >/dev/null 2>&1 && echo docker || echo podman)}"
if [ "$DOCKER" = podman ]; then export KIND_EXPERIMENTAL_PROVIDER=podman; fi
# The id the agent stamps on everything it creates, and the only ones it converges.
# It must match the chart default (reconcileAgent.agentId) — the assertions below
# distinguish the agent's datapacks from the person's by exactly this.
AGENT_ID=ycp-reconcile-agent
# What a person's datapack carries instead: the default agent every account gets at
# signup (repository.DefaultAgentID).
HUMAN_AGENT_ID=meko_agent

log() { printf '\n==> %s\n' "$*"; }
die() { printf '\nFAIL: %s\n' "$*" >&2; exit 1; }
k() { kubectl --context "$CTX" "$@"; }
api() {
  local method="$1" path="$2"; shift 2
  curl -fsS -X "$method" "http://127.0.0.1:$PORT$BASE$path" \
    -H "X-Meko-Admin-Secret: $ADMIN_KEY" "$@"
}
# The data plane's own api-server, over its port-forward. No admin secret: with
# Cognito off it authenticates nobody, which is the same door the agent comes in by.
dp() {
  local method="$1" path="$2"; shift 2
  curl -fsS -X "$method" "http://127.0.0.1:$DP_PORT/api/v1$path" "$@"
}

for bin in kubectl curl python3 helm aws kind "$DOCKER"; do
  command -v "$bin" >/dev/null 2>&1 || die "$bin is required but not on PATH"
done
# Defaults from meko/api_server/internal/service/dataplane_release.go. Kept in step
# with it by hand: this script runs against a released stack, so a drifted default
# fails loudly at pull time rather than silently testing the wrong images.
MEKO_API_TAG="${MEKO_API_TAG:-console-943437724}"
MEKO_MCP_TAG="${MEKO_MCP_TAG:-2.1.0-20260731-7b3fdd3}"
MEKO_UI_TAG="${MEKO_UI_TAG:-1.0.0-20260729-bf56db7}"

cleanup() {
  rc=$?
  for pid in ${PF_PID:-} ${DP_PF_PID:-}; do kill "$pid" 2>/dev/null || true; done
  exit "$rc"
}
trap cleanup EXIT

# Same context guard as the e2e: only ever talk to the kind cluster, never to whatever
# a kubeconfig happens to point at.
k get nodes -o name 2>/dev/null | grep -qx "node/${CLUSTER}-control-plane" ||
  die "context $CTX is not a kind cluster named $CLUSTER — start one with: YCP_E2E_KEEP=1 ycp/test/e2e/run.sh"
k -n "$CP_NS" get deploy meko-api-server >/dev/null 2>&1 ||
  die "no control plane in $CP_NS — run the e2e first (YCP_E2E_KEEP=1 ycp/test/e2e/run.sh)"

# ------------------------------------------------------------------ the agent image
#
# Built here rather than pulled: the Quay tag is only pushed when CI holds
# QUAY_AMP_TOKEN, so a tree that has never been released still has to be testable.

log "building the reconcile-agent image from this tree ($DOCKER)"
# linux/amd64 is not assumed: the kind node runs whatever this machine is, and an
# amd64 image on an arm64 node lands as CrashLoopBackOff with "exec format error".
"$DOCKER" build -q -f "$REPO_ROOT/ycp/Dockerfile.reconcile-agent" \
  -t "$AGENT_IMAGE" "$REPO_ROOT/ycp" >/dev/null || die "building $AGENT_IMAGE failed"
if [ "$DOCKER" = podman ]; then
  archive="$(mktemp -t ycp-agent-XXXXXX.tar)"
  trap 'rm -f "$archive"' RETURN 2>/dev/null || true
  podman save -o "$archive" "$AGENT_IMAGE" >/dev/null || die "podman save failed"
  kind load image-archive "$archive" --name "$CLUSTER" >/dev/null ||
    die "kind load image-archive failed"
  rm -f "$archive"
else
  kind load docker-image "$AGENT_IMAGE" --name "$CLUSTER" >/dev/null ||
    die "kind load of $AGENT_IMAGE failed"
fi
echo "    $AGENT_IMAGE loaded into $CLUSTER"

# ------------------------------------------------------------------ port-forward YCP
#
# A free port checked BEFORE forwarding: kubectl port-forward fails to bind a busy port
# and keeps running, so a stale forward silently keeps it and every call goes to the
# wrong control plane. Waiting for /health does not catch that — the wrong one answers.
log "picking a free local port"
port_free() { ! (exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null; }
pick_port() {
  local start="$1" candidate
  for candidate in $(seq "$start" $((start + 20))); do
    if port_free "$candidate"; then printf '%s' "$candidate"; return 0; fi
  done
  return 1
}
PORT="$(pick_port "$PORT")" || die "no free port near $PORT — close stale kubectl port-forwards"
DP_PORT="$(pick_port $((PORT + 1)))" || die "no free port for the data plane"
echo "    control plane 127.0.0.1:$PORT, data plane 127.0.0.1:$DP_PORT"

log "port-forwarding the control plane"
k -n "$CP_NS" port-forward svc/meko-api-server "$PORT:80" >/dev/null 2>&1 &
PF_PID=$!
for _ in $(seq 1 30); do
  curl -fsS "http://127.0.0.1:$PORT$BASE/api/v1/health" >/dev/null 2>&1 && break
  kill -0 "$PF_PID" 2>/dev/null || die "the port-forward exited — is $CP_NS/meko-api-server serving?"
  sleep 1
done
curl -fsS "http://127.0.0.1:$PORT$BASE/api/v1/health" >/dev/null 2>&1 ||
  die "the control plane did not answer on 127.0.0.1:$PORT$BASE"

# ---------------------------------------------------------------------- register
#
# "desired": [] is the whole opt-in. It registers an environment that declares
# nothing, which is what tells the control plane's install hint — and this script —
# that the stack is the chart's job and the agent's job is datapacks.

log "registering $ENV_NAME declaring nothing"
k delete namespace "$NS" --ignore-not-found --wait=true >/dev/null
API_KEY_ID="$(python3 -c 'import uuid; print(uuid.uuid4())')"
api POST /api/v1/environments -H 'content-type: application/json' \
  -d "{\"name\":\"$ENV_NAME\",\"kind\":\"meko\",\"account_id\":\"22222222-2222-2222-2222-222222222222\",\"placement\":\"cloud\",\"region\":\"local\",\"api_key_id\":\"$API_KEY_ID\",\"desired\":[]}" >/dev/null ||
  die "POST /api/v1/environments failed"

seeded="$(api GET "/api/v1/environments/$ENV_NAME/desired")"
python3 - "$seeded" <<'PY' || die "registration seeded something; it must seed nothing here"
import json, sys
res = json.loads(sys.argv[1])["data"]["resources"] or []
if res:
    sys.exit("    seeded %s, want nothing" % [r["kind"] for r in res])
print("    seeded nothing, as declared")
PY

# --------------------------------------------------------------------- credentials

log "creating namespace $NS and the ECR pull secret"
k create namespace "$NS" --dry-run=client -o yaml | k apply -f - >/dev/null
ecr_token="$(aws ecr get-login-password --region "$ECR_REGION")" ||
  die "aws ecr get-login-password failed — is AWS_PROFILE set to a profile with ECR access?"
k -n "$NS" create secret docker-registry "$PULL_SECRET" \
  --docker-server="$ECR" --docker-username=AWS --docker-password="$ecr_token" \
  --dry-run=client -o yaml | k apply -f - >/dev/null

# Dev-mode credentials: there is no YBM here to mint a real key, so the agent sends
# the admin secret + the key id as the headers YBM would have stamped.
k -n "$NS" create secret generic meko-agent-api-key \
  --from-literal=dev-admin-secret="$ADMIN_KEY" \
  --from-literal=dev-api-key-id="$API_KEY_ID" --dry-run=client -o yaml | k apply -f - >/dev/null

creds=(--from-literal=placeholder=unused)
[ -n "${OPENAI_API_KEY:-}" ] && creds=(--from-literal=OPENAI_API_KEY="$OPENAI_API_KEY")
k -n "$NS" create secret generic meko-data-plane "${creds[@]}" \
  --dry-run=client -o yaml | k apply -f - >/dev/null

# ------------------------------------------------------------------------- install
#
# This is the install hint's command, with the local agent image substituted for the
# Quay one and the pull secret named. If it drifts from the hint, the hint is wrong.

log "installing the stack and the reconcile-agent"
helm --kube-context "$CTX" upgrade --install meko "$CHART" -n "$NS" \
  -f "$CHART/values-aws.yaml" -f "$CHART/values-dev.yaml" -f "$CHART/values-byoc.yaml" \
  --set-string apiServer.image.tag="$MEKO_API_TAG" \
  --set-string mekoMcpServer.image.tag="$MEKO_MCP_TAG" \
  --set-string mekoUi.image.tag="$MEKO_UI_TAG" \
  --set "global.imagePullSecrets[0].name=$PULL_SECRET" \
  --set reconcileAgent.enabled=true \
  --set reconcileAgent.ycpUrl="http://meko-api-server.$CP_NS.svc.cluster.local:80$BASE/api/v1" \
  --set reconcileAgent.envName="$ENV_NAME" \
  --set reconcileAgent.image.registry=docker.io/library \
  --set reconcileAgent.image.repository=ycp-reconciler-agent \
  --set-string reconcileAgent.image.tag=local \
  --set global.imagePullPolicy=IfNotPresent \
  --wait --timeout 15m ||
  die "helm install failed"

log "waiting for the api-server"
k -n "$NS" rollout status deploy/meko-api-server --timeout=600s ||
  die "the api-server never became ready"

k -n "$NS" port-forward svc/meko-api-server-service "$DP_PORT:80" >/dev/null 2>&1 &
DP_PF_PID=$!
for _ in $(seq 1 60); do
  curl -fsS "http://127.0.0.1:$DP_PORT/api/v1/health" >/dev/null 2>&1 && break
  sleep 1
done
curl -fsS "http://127.0.0.1:$DP_PORT/api/v1/health" >/dev/null 2>&1 ||
  die "the data-plane api-server did not answer on 127.0.0.1:$DP_PORT"

# ------------------------------------------------------- something it must not touch
#
# A datapack made the way a person makes one: through the api-server, carrying the
# default agent id rather than the reconciler's. Everything below asserts it survives.

log "creating a person's datapack (must never be touched)"
dp POST /agents -H 'content-type: application/json' \
  -d "{\"agent_id\":\"$HUMAN_AGENT_ID\"}" >/dev/null 2>&1 || true  # 409 if it exists
dp POST /datapacks -H 'content-type: application/json' \
  -d "{\"name\":\"handmade\",\"agent_id\":\"$HUMAN_AGENT_ID\",\"description\":\"made in the UI\"}" >/dev/null ||
  die "could not create the person's datapack"

# Report every datapack as "<name> <created_by_agent_id>", the two facts the
# assertions turn on.
datapacks() {
  dp GET /datapacks | python3 -c '
import json, sys
for d in json.load(sys.stdin)["data"] or []:
    print(d["datapack_name"], d.get("created_by_agent_id") or "-")'
}

assert_human_intact() {
  datapacks | grep -qx "handmade $HUMAN_AGENT_ID" ||
    die "the person'\''s datapack is gone — the agent deleted something that was not its own"
}
assert_human_intact
echo "    handmade created"

# ------------------------------------------------------------------ declare and wait

wait_for() {
  local what="$1" want="$2" secs="${3:-180}"
  for _ in $(seq 1 "$secs"); do
    if datapacks | grep -q "$want"; then return 0; fi
    sleep 1
  done
  printf '    datapacks now:\n'; datapacks | sed 's/^/      /'
  die "$what"
}
wait_gone() {
  local what="$1" gone="$2" secs="${3:-180}"
  for _ in $(seq 1 "$secs"); do
    datapacks | grep -q "^$gone " || return 0
    sleep 1
  done
  printf '    datapacks now:\n'; datapacks | sed 's/^/      /'
  die "$what"
}

log "declaring two datapacks in desired state"
api POST "/api/v1/environments/$ENV_NAME/desired" -H 'content-type: application/json' \
  -d '{"resources":[
        {"kind":"datapack","name":"team-a","spec":{"description":"the A team"}},
        {"kind":"datapack","name":"team-b"}]}' >/dev/null ||
  die "POST desired failed"

wait_for "team-a was never created" "^team-a $AGENT_ID\$"
wait_for "team-b was never created" "^team-b $AGENT_ID\$"
assert_human_intact
echo "    both created, stamped $AGENT_ID; handmade untouched"

# ----------------------------------------------------------------------- withdraw

log "withdrawing team-b"
api POST "/api/v1/environments/$ENV_NAME/desired" -H 'content-type: application/json' \
  -d '{"resources":[{"kind":"datapack","name":"team-a","spec":{"description":"the A team"}}]}' >/dev/null ||
  die "POST desired failed"

wait_gone "team-b was never garbage-collected" "team-b"
datapacks | grep -qx "team-a $AGENT_ID" || die "team-a went away with team-b"
assert_human_intact
echo "    team-b collected; team-a and handmade untouched"

# -------------------------------------------------------------------------- report

log "the environment's reported inventory"
api GET "/api/v1/environments/$ENV_NAME" | python3 -c '
import json, sys
e = json.load(sys.stdin)["data"]
print("    state:", e.get("state"))
for r in json.loads(e.get("resources") or "[]"):
    print("    %-24s %-10s %s" % (r.get("id"), r.get("state"), r.get("error") or ""))'

cat <<EOF

PASS — the reconcile-agent converged this environment's datapacks and left the
       hand-made one alone.

  namespace:   $NS
  environment: $ENV_NAME
  agent logs:  kubectl --context $CTX -n $NS logs -f deploy/meko-reconcile-agent

Nothing here is cleaned up. Remove it with:
  helm --kube-context $CTX -n $NS uninstall meko && kubectl --context $CTX delete ns $NS
EOF
