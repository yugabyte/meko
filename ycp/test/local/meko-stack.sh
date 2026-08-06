#!/usr/bin/env bash
#
# Bootstrap the real Meko stack in a local data plane, by commanding it the way an
# operator would: one POST of desired state to the control plane, after which the
# agent does the work.
#
# What this adds over test/e2e/run.sh: that run proves the loop with images built
# from this repo, so it needs no credentials and runs in CI. This one declares the
# *private* Meko images — meko-mcp-server, and optionally the RAG worker — so what
# comes up is the thing customers run. That needs ECR credentials, which is exactly
# why it is not part of the CI e2e.
#
# It does not create a cluster. Run the e2e first and keep it:
#
#   YCP_E2E_KEEP=1 ycp/test/e2e/run.sh
#   ycp/test/local/meko-stack.sh
#
# Environment:
#   AWS_PROFILE           profile that can read the Meko ECR (meko-dev here).
#   MEKO_MCP_IMAGE        full image ref; default = newest tag in ECR.
#   MEKO_RAG_IMAGE        set to also declare a rag-worker. Off by default: it wants
#                         OpenAI + S3 credentials to do anything useful.
#   OPENAI_API_KEY        written to the data-plane Secret when set. The MCP server
#                         starts without it and fails on the first embedding.
#   YCP_E2E_CLUSTER       kind cluster name (default ycp-e2e).
#   YCP_E2E_BASE_PATH     subpath the control plane is mounted under (default /ycp).
#
set -euo pipefail

CLUSTER="${YCP_E2E_CLUSTER:-ycp-e2e}"
CTX="kind-${CLUSTER}"
NS=ycp-e2e
ENV_NAME=e2e-cell
ADMIN_KEY=e2eadminkey
PORT="${YCP_LOCAL_PORT:-18081}"
BASE="${YCP_E2E_BASE_PATH-/ycp}"
TIMEOUT="${YCP_LOCAL_TIMEOUT:-600s}"
ECR="161908593886.dkr.ecr.us-east-2.amazonaws.com"
ECR_REGION=us-east-2
PULL_SECRET=meko-ecr
CREDS_SECRET=meko-data-plane

log() { printf '\n==> %s\n' "$*"; }
die() { printf '\nFAIL: %s\n' "$*" >&2; exit 1; }
k() { kubectl --context "$CTX" "$@"; }
api() {
  local method="$1" path="$2"; shift 2
  curl -fsS -X "$method" "http://127.0.0.1:$PORT$BASE$path" \
    -H "X-Meko-Admin-Secret: $ADMIN_KEY" "$@"
}

for bin in kubectl curl python3 aws; do
  command -v "$bin" >/dev/null 2>&1 || die "$bin is required but not on PATH"
done

cleanup() {
  rc=$?
  for pid in ${PF_PID:-} ${MCP_PF_PID:-}; do kill "$pid" 2>/dev/null || true; done
  exit "$rc"
}
trap cleanup EXIT

# Same context guard as the e2e: only ever talk to the kind cluster, never to
# whatever a kubeconfig happens to point at.
k get nodes -o name 2>/dev/null | grep -qx "node/${CLUSTER}-control-plane" ||
  die "context $CTX is not a kind cluster named $CLUSTER — start one with: YCP_E2E_KEEP=1 ycp/test/e2e/run.sh"
k -n "$NS" get deploy meko-agent >/dev/null 2>&1 ||
  die "no meko-agent in $NS — run the e2e first (YCP_E2E_KEEP=1 ycp/test/e2e/run.sh)"

# ---------------------------------------------------------------- the image

MCP_IMAGE="${MEKO_MCP_IMAGE:-}"
if [ -z "$MCP_IMAGE" ]; then
  log "resolving the newest meko-mcp-server tag in ECR"
  tag="$(aws ecr describe-images --repository-name meko/meko-mcp-server --region "$ECR_REGION" \
    --query 'reverse(sort_by(imageDetails,&imagePushedAt))[0].imageTags[0]' --output text 2>/dev/null || true)"
  [ -n "$tag" ] && [ "$tag" != "None" ] ||
    die "could not read ECR (set AWS_PROFILE to a profile with access, or pass MEKO_MCP_IMAGE)"
  MCP_IMAGE="$ECR/meko/meko-mcp-server:$tag"
fi
echo "    mcp-server image: $MCP_IMAGE"

# ---------------------------------------------------------------- credentials
#
# An ECR pull Secret rather than side-loading the image: the image is ~11 GB
# uncompressed, so `kind load` means writing it twice, and a pull Secret is also what
# a real data plane needs. Note the token is valid ~12 h — a cluster kept for longer
# needs this step re-run before the next pull.

log "creating the ECR pull Secret ($PULL_SECRET)"
ecr_token="$(aws ecr get-login-password --region "$ECR_REGION")" ||
  die "aws ecr get-login-password failed — is AWS_PROFILE set to a profile with ECR access?"
k -n "$NS" create secret docker-registry "$PULL_SECRET" \
  --docker-server="$ECR" --docker-username=AWS --docker-password="$ecr_token" \
  --dry-run=client -o yaml | k apply -f - >/dev/null
echo "    $PULL_SECRET written (ECR tokens expire after ~12h; re-run this to refresh)"

# The credentials Secret is referenced by NAME in desired state and mounted whole
# with envFrom, so nothing secret ever travels through the control plane.
log "creating the data-plane credentials Secret ($CREDS_SECRET)"
creds=(--from-literal=placeholder=unused)
if [ -n "${OPENAI_API_KEY:-}" ]; then
  creds=(--from-literal=OPENAI_API_KEY="$OPENAI_API_KEY")
  echo "    OPENAI_API_KEY included"
else
  echo "    OPENAI_API_KEY not set — the MCP server will start and fail on the first embedding"
fi
k -n "$NS" create secret generic "$CREDS_SECRET" "${creds[@]}" \
  --dry-run=client -o yaml | k apply -f - >/dev/null

# ---------------------------------------------------------------- the command

log "port-forwarding the control plane"
k -n "$NS" port-forward svc/meko-api-server "$PORT:80" >/dev/null 2>&1 &
PF_PID=$!
for _ in $(seq 1 30); do
  curl -fsS "http://127.0.0.1:$PORT$BASE/api/v1/health" >/dev/null 2>&1 && break
  sleep 1
done
curl -fsS "http://127.0.0.1:$PORT$BASE/api/v1/health" >/dev/null 2>&1 ||
  die "the control plane did not answer on 127.0.0.1:$PORT$BASE"

# The database spec is left empty, exactly as the e2e leaves it. That keeps the two
# levels declaring the same thing, so running this on a cluster the e2e left behind
# reuses that database instead of asking for a StatefulSet change Kubernetes forbids
# in place (volumeClaimTemplates are immutable, so a differing storage_size is
# rejected on every tick, forever).
resources="$(python3 - "$MCP_IMAGE" "${MEKO_RAG_IMAGE:-}" "$PULL_SECRET" "$CREDS_SECRET" <<'PY'
import json, sys
mcp_image, rag_image, pull_secret, creds = sys.argv[1:5]
resources = [
    {"kind": "database", "name": "meko", "spec": {}},
    {"kind": "mcp-server", "name": "mcp", "spec": {
        "image": mcp_image,
        "image_pull_secret": pull_secret,
        "secret_ref": creds,
    }},
]
if rag_image:
    resources.append({"kind": "rag-worker", "name": "rag", "spec": {
        "image": rag_image, "image_pull_secret": pull_secret, "secret_ref": creds,
    }})
print(json.dumps({"resources": resources}))
PY
)"

log "declaring the Meko stack as desired state"
api POST "/api/v1/environments/$ENV_NAME/desired" -H 'content-type: application/json' \
  -d "$resources" >/dev/null || die "POST /environments/$ENV_NAME/desired failed"
python3 -c 'import json,sys; [print("    "+r["kind"]+"/"+r["name"]) for r in json.loads(sys.argv[1])["resources"]]' "$resources"

# ---------------------------------------------------------------- watch it come up

log "waiting for the database (the agent creates it on its next tick, ~10s)"
for _ in $(seq 1 30); do
  k -n "$NS" get statefulset meko >/dev/null 2>&1 && break
  sleep 2
done
k -n "$NS" get statefulset meko >/dev/null 2>&1 ||
  die "the agent never created the database StatefulSet — check: kubectl -n $NS logs deploy/meko-agent"
k -n "$NS" rollout status statefulset/meko --timeout="$TIMEOUT"

log "waiting for meko-mcp-server (first pull is several GB — this is the slow part)"
for _ in $(seq 1 60); do
  k -n "$NS" get deploy mcp >/dev/null 2>&1 && break
  sleep 2
done
k -n "$NS" get deploy mcp >/dev/null 2>&1 ||
  die "the agent never created the mcp Deployment — check: kubectl -n $NS logs deploy/meko-agent"
if ! k -n "$NS" rollout status deploy/mcp --timeout="$TIMEOUT"; then
  k -n "$NS" get pods -l app=mcp -o wide || true
  k -n "$NS" describe pods -l app=mcp | grep -E '^(  Warning|    Reason|    State|    Message)' || true
  k -n "$NS" logs deploy/mcp --tail=40 || true
  die "meko-mcp-server never became ready (see above)"
fi

log "checking the MCP server answers"
k -n "$NS" port-forward svc/mcp 18000:8000 >/dev/null 2>&1 &
MCP_PF_PID=$!
ping=""
for _ in $(seq 1 30); do
  ping="$(curl -fsS "http://127.0.0.1:18000/ping" 2>/dev/null || true)"
  [ -n "$ping" ] && break
  sleep 2
done
[ -n "$ping" ] || die "GET /ping never answered"
echo "    /ping -> $ping"

# /ping only proves the process is up. The MCP handshake proves the thing the agent
# provisioned is the product: a real MCP endpoint over the database it was pointed at.
# The response is an SSE stream, hence the accept header.
mcp="$(curl -sS -X POST "http://127.0.0.1:18000/mcp" \
  -H 'content-type: application/json' -H 'accept: application/json, text/event-stream' \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"meko-stack.sh","version":"1"}}}' 2>/dev/null || true)"
printf '%s' "$mcp" | grep -q '"serverInfo"' ||
  die "POST /mcp did not complete an MCP initialize: ${mcp:0:300}"
printf '    /mcp initialize -> %s\n' "$(printf '%s' "$mcp" |
  python3 -c 'import json,sys
raw = sys.stdin.read()
line = next(l for l in raw.splitlines() if l.startswith("data:"))
info = json.loads(line[5:])["result"]["serverInfo"]
print("%s %s" % (info.get("name"), info.get("version")))' 2>/dev/null || echo "ok")"

# Poll rather than read once: the fleet view is whatever the agent last reported, so
# for up to one tick (~10s) after a workload becomes ready it still says 0/1. Reading
# it a single time turns that lag into a failure.
log "waiting for the fleet API to report the stack healthy"
for _ in $(seq 1 20); do
  envs="$(api GET /api/v1/environments 2>/dev/null || true)"
  printf '%s' "$envs" | grep -q '"name":"mcp-server"' &&
    python3 - "$ENV_NAME" "$envs" >/dev/null 2>&1 <<'PY' && break
import json, sys
items = json.loads(sys.argv[2] or "{}").get("data", {}).get("items") or []
e = next((x for x in items if x["id"] == sys.argv[1]), None)
comps = (e or {}).get("components") or []
ok = any(c["name"] == "mcp-server" and c["total"] > 0 and c["healthy"] == c["total"] for c in comps)
sys.exit(0 if ok else 1)
PY
  sleep 5
done
python3 - "$ENV_NAME" "${envs:-}" <<'PY' || die "the fleet API does not report a healthy stack"
import json, sys
items = json.loads(sys.argv[2] or "{}").get("data", {}).get("items") or []
e = next((x for x in items if x["id"] == sys.argv[1]), None)
if e is None:
    sys.exit("%s not in the fleet" % sys.argv[1])
comps = e.get("components") or []
print("    state=%s components: %s" % (
    e["state"], ", ".join("%s %d/%d%s" % (
        c["name"], c["healthy"], c["total"],
        " (" + c["error"] + ")" if c.get("error") else "") for c in comps)))
if not any(c["name"] == "mcp-server" and c["total"] > 0 and c["healthy"] == c["total"] for c in comps):
    sys.exit("mcp-server is not reported healthy")
PY

cat <<EOF

==> Meko is running in the local data plane.

    Console:      kubectl --context $CTX -n $NS port-forward svc/meko-api-server 18080:80
                  then open http://127.0.0.1:18080$BASE/  (admin secret: $ADMIN_KEY)
    MCP server:   kubectl --context $CTX -n $NS port-forward svc/mcp 18000:8000
                  then POST http://127.0.0.1:18000/mcp
    Agent logs:   kubectl --context $CTX -n $NS logs -f deploy/meko-agent

    Change what runs by editing desired state — in the console under
    "Edit desired state", or with:

      curl -X POST http://127.0.0.1:$PORT$BASE/api/v1/environments/$ENV_NAME/desired \\
        -H "X-Meko-Admin-Secret: $ADMIN_KEY" -H 'content-type: application/json' \\
        -d '{"resources":[...]}'

    Tear down:    kind delete cluster --name $CLUSTER
EOF
