#!/usr/bin/env bash
#
# The whole thing, the way an operator does it: stand up YCP, register an
# environment in it, and run the Helm command YCP prints. Nothing else. The agent
# enrolls itself and brings the Meko stack up from the desired state registration
# seeded — no `kubectl apply`, no POST of desired state, no second step.
#
# What this covers that the other two scripts do not:
#
#   test/e2e/run.sh          drives the API directly and applies raw manifests. It is
#                            the CI test, so it uses no chart and no credentials.
#   test/local/meko-stack.sh POSTs desired state by hand to a cluster the e2e left up.
#   this script              the real operator path: the `meko` Helm chart from
#                            meko-devops, installed with the command the control
#                            plane itself printed, against a control plane
#                            configured to seed the full stack.
#
# It therefore also tests the install hint. A hint that has drifted from the chart is
# not a cosmetic problem: it is the one instruction an operator is given, and every
# gap in it becomes a support question.
#
# Requires: kind, kubectl, helm, curl, python3, podman or docker, AWS credentials for
# the Meko ECR (the MCP server is a private image), and a meko-devops checkout.
#
# Usage:
#   AWS_PROFILE=meko-dev ycp/test/local/ycp-helm-bootstrap.sh
#   MEKO_DEVOPS=~/src/meko-devops AWS_PROFILE=meko-dev ycp/test/local/ycp-helm-bootstrap.sh
#
# Environment:
#   MEKO_DEVOPS      path to the meko-devops checkout (default ~/code/meko-devops)
#   MEKO_MCP_IMAGE   full image ref; default = newest tag in ECR
#   OPENAI_API_KEY   written into the data-plane Secret when set
#   YCP_HELM_KEEP    1 (default) leaves the cluster up; 0 tears it down at the end
#
set -euo pipefail

CLUSTER="${YCP_HELM_CLUSTER:-ycp-helm}"
CTX="kind-${CLUSTER}"
CP_NS=ycp-system      # the control plane
AGENT_NS=ycp-agent    # the data plane the agent manages, as the install hint names it
ENV_NAME=helm-cell
ADMIN_KEY=e2eadminkey # matches ycp-e2e-secrets in controlplane.yaml
TAG="${YCP_HELM_TAG:-helm}"
PORT="${YCP_HELM_PORT:-18082}"
BASE="${YCP_HELM_BASE_PATH-/ycp}"
TIMEOUT="${YCP_HELM_TIMEOUT:-600s}"
KEEP="${YCP_HELM_KEEP:-1}"
MEKO_DEVOPS="${MEKO_DEVOPS:-$HOME/code/meko-devops}"
ECR=161908593886.dkr.ecr.us-east-2.amazonaws.com
ECR_REGION=us-east-2
PULL_SECRET=meko-ecr
CREDS_SECRET=meko-data-plane

E2E_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../e2e" && pwd)"
YCP_DIR="$(cd "$E2E_DIR/../.." && pwd)"
REPO_DIR="$(cd "$YCP_DIR/.." && pwd)"
# ycp/ sits at the repository root because it is shared with amp; the control plane
# it builds lives in the meko tree.
MEKO_DIR="$REPO_DIR/meko"

log() { printf '\n==> %s\n' "$*"; }
die() { printf '\nFAIL: %s\n' "$*" >&2; exit 1; }
k() { kubectl --context "$CTX" "$@"; }
api() {
  local method="$1" path="$2"; shift 2
  curl -fsS -X "$method" "http://127.0.0.1:$PORT$BASE$path" \
    -H "X-Meko-Admin-Secret: $ADMIN_KEY" "$@"
}

for bin in kind kubectl helm curl python3 aws; do
  command -v "$bin" >/dev/null 2>&1 || die "$bin is required but not on PATH"
done
CHART="$MEKO_DEVOPS/charts/meko"
[ -d "$CHART" ] || die "no chart at $CHART — set MEKO_DEVOPS to your meko-devops checkout"

ENGINE="${YCP_HELM_ENGINE:-}"
if [ -z "$ENGINE" ]; then
  for e in docker podman; do command -v "$e" >/dev/null 2>&1 && { ENGINE="$e"; break; } done
fi
[ -n "$ENGINE" ] || die "need docker or podman on PATH"
# Podman stores a bare name:tag as localhost/name:tag, which neither `kind load` finds
# nor kubelet matches. Naming images docker.io/library/... from the start makes the
# two agree -- and that prefix is also what the chart wants as global.imageRegistry,
# since it refuses an unqualified first-party image ref on purpose.
REGISTRY=docker.io/library
if [ "$ENGINE" = podman ]; then
  export KIND_EXPERIMENTAL_PROVIDER=podman
fi
"$ENGINE" info >/dev/null 2>&1 || die "$ENGINE is not usable — is the machine running? (podman machine start)"

cleanup() {
  rc=$?
  for pid in ${PF_PID:-} ${MCP_PF_PID:-}; do kill "$pid" 2>/dev/null || true; done
  if [ -n "${TMP:-}" ]; then rm -rf "$TMP"; fi
  if [ "$rc" -ne 0 ]; then
    log "diagnostics (the run failed)"
    k -n "$CP_NS" get pods 2>&1 | head -10 || true
    k -n "$AGENT_NS" get pods,statefulsets,deployments 2>&1 | head -20 || true
    k -n "$AGENT_NS" logs deploy/meko-agent --tail=30 2>&1 | head -40 || true
    k -n "$AGENT_NS" describe pods -l app=mcp 2>&1 | grep -E '^(  Warning|    Reason|    Message)' | head -10 || true
  fi
  if [ "$KEEP" = "1" ]; then
    log "keeping cluster (YCP_HELM_KEEP=0 to tear down): kind delete cluster --name $CLUSTER"
  else
    kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
  fi
  exit "$rc"
}
trap cleanup EXIT

TMP="$(mktemp -d "${TMPDIR:-/tmp}/ycp-helm.XXXXXX")"
# Podman consults ~/.docker/config.json credHelpers even for unrelated registries, and
# a stale helper there fails builds outright. An empty auth file skips it; the ECR
# login below writes into this same file.
if [ -z "${REGISTRY_AUTH_FILE:-}" ]; then
  REGISTRY_AUTH_FILE="$TMP/auth.json"
  printf '{"auths":{}}' > "$REGISTRY_AUTH_FILE"
  export REGISTRY_AUTH_FILE
fi

# ----------------------------------------------------------------- images + cluster

log "building the control plane and agent from source ($ENGINE)"
API_IMG="$REGISTRY/meko/meko-api-server:$TAG"
AGENT_IMG="$REGISTRY/meko/meko-agent:$TAG"
"$ENGINE" build -q -f "$MEKO_DIR/api_server/Dockerfile" -t "$API_IMG"   "$MEKO_DIR/api_server" >/dev/null
"$ENGINE" build -q -f "$YCP_DIR/Dockerfile.agent"       -t "$AGENT_IMG" "$YCP_DIR" >/dev/null

log "creating kind cluster $CLUSTER"
kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
kind create cluster --name "$CLUSTER" --wait 120s >/dev/null
k get nodes -o name 2>/dev/null | grep -qx "node/${CLUSTER}-control-plane" ||
  die "context guard: $CTX is not the kind cluster just created; refusing to continue"

log "loading images into the cluster"
for img in "$API_IMG" "$AGENT_IMG"; do
  if [ "$ENGINE" = docker ]; then
    kind load docker-image "$img" --name "$CLUSTER" >/dev/null
  else
    tar="$TMP/$(printf '%s' "$img" | tr '/:' '__').tar"
    "$ENGINE" save --format docker-archive -o "$tar" "$img"
    kind load image-archive "$tar" --name "$CLUSTER" >/dev/null
    rm -f "$tar"
  fi
done

# --------------------------------------------------------------------- control plane
#
# Reuses the e2e's manifest, with the namespace renamed and the images pointed at the
# locally built refs. The e2e manifest is the one that is actually maintained; a
# second copy here would drift from it.

log "deploying YCP (api_server --control-plane-mode)"
sed -e "s|__BASE_PATH__|$BASE|g" -e "s|ycp-e2e|$CP_NS|g" \
    -e "s|meko-api-server:e2e|$API_IMG|g" "$E2E_DIR/controlplane.yaml" > "$TMP/controlplane.yaml"
k apply -f "$TMP/controlplane.yaml" >/dev/null
k -n "$CP_NS" rollout status deploy/postgres --timeout="$TIMEOUT"

# The seeding configuration: what a newly registered environment is told to run. This
# is the difference between an agent that enrolls and waits, and one that brings Meko
# up on its own. The MCP image has no default because it is private.
log "resolving the meko-mcp-server tag to seed with"
MCP_IMAGE="${MEKO_MCP_IMAGE:-}"
if [ -z "$MCP_IMAGE" ]; then
  tag="$(aws ecr describe-images --repository-name meko/meko-mcp-server --region "$ECR_REGION" \
    --query 'reverse(sort_by(imageDetails,&imagePushedAt))[0].imageTags[0]' --output text 2>/dev/null || true)"
  [ -n "$tag" ] && [ "$tag" != "None" ] ||
    die "could not read ECR (set AWS_PROFILE, or pass MEKO_MCP_IMAGE)"
  MCP_IMAGE="$ECR/meko/meko-mcp-server:$tag"
fi
echo "    $MCP_IMAGE"

k -n "$CP_NS" patch deploy meko-api-server --type=json -p "$(cat <<JSON
[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--dataplane-mcp-image=$MCP_IMAGE"},
 {"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--dataplane-image-pull-secret=$PULL_SECRET"},
 {"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--dataplane-secret-ref=$CREDS_SECRET"}]
JSON
)" >/dev/null
k -n "$CP_NS" rollout status deploy/meko-api-server --timeout="$TIMEOUT"

log "port-forwarding the control plane"
k -n "$CP_NS" port-forward svc/meko-api-server "$PORT:80" >/dev/null 2>&1 &
PF_PID=$!
for _ in $(seq 1 40); do
  curl -fsS "http://127.0.0.1:$PORT$BASE/api/v1/health" >/dev/null 2>&1 && break
  sleep 2
done
curl -fsS "http://127.0.0.1:$PORT$BASE/api/v1/health" >/dev/null 2>&1 ||
  die "the control plane never answered on 127.0.0.1:$PORT$BASE"

# ------------------------------------------------------------------------- register
#
# The operator step. Everything the agent will run is decided here.

log "registering environment $ENV_NAME in YCP"
API_KEY_ID="$(python3 -c 'import uuid; print(uuid.uuid4())')"
reg="$(api POST /api/v1/environments -H 'content-type: application/json' \
  -d "{\"name\":\"$ENV_NAME\",\"account_id\":\"22222222-2222-2222-2222-222222222222\",\"placement\":\"cloud\",\"region\":\"local\",\"api_key_id\":\"$API_KEY_ID\"}")"

echo
echo "--------- the install command YCP printed (install_hint) ---------"
printf '%s' "$reg" | python3 -c 'import json,sys; print(json.load(sys.stdin)["data"]["install_hint"])'
echo "-----------------------------------------------------------------"

# What it was seeded with, before anything is installed. This is the whole point: the
# desired state exists at registration time, not after some later command.
seeded="$(api GET "/api/v1/environments/$ENV_NAME/desired")"
printf '%s' "$seeded" | python3 -c '
import json, sys
res = json.load(sys.stdin)["data"]["resources"]
print("    seeded at registration: " + ", ".join(r["kind"] + "/" + r["name"] for r in res))
stack = next((r for r in res if r["kind"] == "meko-stack"), None)
if stack is None:
    sys.exit("    no meko-stack seeded — registration should resolve the whole release")
if not stack["spec"].get("images", {}).get("mcp_server"):
    sys.exit("    seeded stack has no images.mcp_server — is --dataplane-mcp-image set, "
             "or the pinned release missing it?")
' || die "registration did not seed the stack"

# ---------------------------------------------------------------------- credentials
#
# The two Secrets the chart deliberately does not carry: the agent's YCP credentials
# (dev-mode headers here — no YBM to mint a real key), and the registry/data-plane
# credentials the seeded desired state references BY NAME.

log "creating the agent namespace and its Secrets"
k create namespace "$AGENT_NS" --dry-run=client -o yaml | k apply -f - >/dev/null
k -n "$AGENT_NS" create secret generic meko-agent-api-key \
  --from-literal=dev-admin-secret="$ADMIN_KEY" \
  --from-literal=dev-api-key-id="$API_KEY_ID" --dry-run=client -o yaml | k apply -f - >/dev/null

ecr_token="$(aws ecr get-login-password --region "$ECR_REGION")" ||
  die "aws ecr get-login-password failed — set AWS_PROFILE to a profile with ECR access"
k -n "$AGENT_NS" create secret docker-registry "$PULL_SECRET" \
  --docker-server="$ECR" --docker-username=AWS --docker-password="$ecr_token" \
  --dry-run=client -o yaml | k apply -f - >/dev/null

creds=(--from-literal=placeholder=unused)
if [ -n "${OPENAI_API_KEY:-}" ]; then
  creds=(--from-literal=OPENAI_API_KEY="$OPENAI_API_KEY")
fi
k -n "$AGENT_NS" create secret generic "$CREDS_SECRET" "${creds[@]}" \
  --dry-run=client -o yaml | k apply -f - >/dev/null
echo "    meko-agent-api-key (dev-mode), $PULL_SECRET, $CREDS_SECRET"

# ---------------------------------------------------------------------------- helm
#
# The command from the hint, with three local adjustments: the in-cluster Service URL
# instead of the port-forward the hint saw, the side-loaded image tag, and the
# registry escape hatch the chart documents for exactly this case.

YCP_IN_CLUSTER="http://meko-api-server.$CP_NS.svc.cluster.local:80$BASE/api/v1"

log "helm upgrade --install meko-agent (chart: $CHART)"
set -x
helm --kube-context "$CTX" upgrade --install meko-agent "$CHART" -n "$AGENT_NS" \
  -f "$CHART/values-aws.yaml" -f "$CHART/values-dev.yaml" -f "$CHART/values-agent.yaml" \
  --set agent.ycpUrl="$YCP_IN_CLUSTER" \
  --set agent.envName="$ENV_NAME" \
  --set-string agent.image.tag="$TAG" \
  --set global.imageRegistry="$REGISTRY" \
  --set global.imagePullPolicy=IfNotPresent \
  --set ecrCredentialRefresh.enabled=false \
  --wait --timeout "$TIMEOUT" >/dev/null
set +x

# ------------------------------------------------------------ watch it bootstrap
#
# From here nothing else is run. Every assertion below is about what the agent did on
# its own, from the desired state registration seeded.

log "waiting for the agent to enroll"
enrolled=""
for _ in $(seq 1 45); do
  enrolled="$(k -n "$AGENT_NS" logs deploy/meko-agent --tail=-1 2>/dev/null |
                grep "enrolled: environment_id=$ENV_NAME" | tail -1 || true)"
  [ -n "$enrolled" ] && break
  sleep 2
done
[ -n "$enrolled" ] || die "the agent never enrolled — check: kubectl -n $AGENT_NS logs deploy/meko-agent"
printf '    %s\n' "$enrolled"

log "waiting for the database the agent was never told to run"
# Found by label, not by name. The stack names its objects after the resource it was
# seeded as (meko-yugabytedb), and a hardcoded name here is exactly what broke this
# script when registration started seeding a meko-stack instead of a database.
DB=""
for _ in $(seq 1 60); do
  DB="$(k -n "$AGENT_NS" get statefulset -l meko.dev/stack=true \
        -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)"
  [ -n "$DB" ] && break
  sleep 2
done
[ -n "$DB" ] ||
  die "no database StatefulSet — the agent enrolled but did not act on its desired state"
echo "    StatefulSet $DB"
k -n "$AGENT_NS" rollout status "statefulset/$DB" --timeout="$TIMEOUT"

log "waiting for the Meko services (the first meko-mcp-server pull is several GB)"
# Every Deployment the stack owns, by label. The inference gateway is skipped without an
# OPENAI_API_KEY: it builds its LLM client at import, so it exits rather than degrades,
# and that is missing configuration rather than a fault in the stack.
for dep in $(k -n "$AGENT_NS" get deploy -l meko.dev/stack=true \
             -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}'); do
  case "$dep" in
    *inference-gateway)
      if [ -z "${OPENAI_API_KEY:-}" ]; then
        echo "    skipping $dep — it needs OPENAI_API_KEY to start at all"
        continue
      fi ;;
  esac
  k -n "$AGENT_NS" rollout status "deploy/$dep" --timeout="$TIMEOUT" || die "$dep never became ready"
done

log "checking the MCP server answers"
k -n "$AGENT_NS" port-forward svc/meko-mcp-server 18000:8000 >/dev/null 2>&1 &
MCP_PF_PID=$!
ping=""
for _ in $(seq 1 30); do
  ping="$(curl -fsS http://127.0.0.1:18000/ping 2>/dev/null || true)"
  [ -n "$ping" ] && break
  sleep 2
done
[ -n "$ping" ] || die "GET /ping never answered"
echo "    /ping -> $ping"

mcp="$(curl -sS -X POST http://127.0.0.1:18000/mcp \
  -H 'content-type: application/json' -H 'accept: application/json, text/event-stream' \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"ycp-helm-bootstrap","version":"1"}}}' 2>/dev/null || true)"
printf '%s' "$mcp" | grep -q '"serverInfo"' || die "POST /mcp did not complete an initialize: ${mcp:0:300}"
printf '    /mcp initialize -> %s\n' "$(printf '%s' "$mcp" | python3 -c '
import json, sys
line = next(l for l in sys.stdin.read().splitlines() if l.startswith("data:"))
info = json.loads(line[5:])["result"]["serverInfo"]
print("%s %s" % (info.get("name"), info.get("version")))' 2>/dev/null || echo ok)"

log "waiting for the fleet API to report the stack healthy"
# `yugabytedb`, not `postgres`: a component reports the engine it runs, and the stack
# runs yugabytedb. The gateway is required only with an OPENAI_API_KEY — it builds its
# LLM client at import, so without one it never starts and READY is unreachable.
WANT="meko-agent yugabytedb api-server mcp-server rag-worker ui"
[ -n "${OPENAI_API_KEY:-}" ] && WANT="$WANT inference-gateway"
for _ in $(seq 1 40); do
  envs="$(api GET /api/v1/environments 2>/dev/null || true)"
  python3 - "$ENV_NAME" "$envs" "$WANT" >/dev/null 2>&1 <<'PY' && break
import json, sys
items = json.loads(sys.argv[2] or "{}").get("data", {}).get("items") or []
e = next((x for x in items if x["id"] == sys.argv[1]), None)
comps = {c["name"]: c for c in ((e or {}).get("components") or [])}
sys.exit(0 if all(w in comps and comps[w]["total"] > 0 and comps[w]["healthy"] == comps[w]["total"]
                  for w in sys.argv[3].split()) else 1)
PY
  sleep 5
done
python3 - "$ENV_NAME" "${envs:-}" "$WANT" "${OPENAI_API_KEY:-}" <<'PY' || die "the fleet API does not report a healthy stack"
import json, sys
items = json.loads(sys.argv[2] or "{}").get("data", {}).get("items") or []
e = next((x for x in items if x["id"] == sys.argv[1]), None)
if e is None:
    sys.exit("%s not in the fleet" % sys.argv[1])
comps = {c["name"]: c for c in (e.get("components") or [])}
print("    state=%s" % e["state"])
for c in (e.get("components") or []):
    print("      %-18s %d/%d%s" % (c["name"], c["healthy"], c["total"],
                                    "  ERROR: " + c["error"] if c.get("error") else ""))
missing = [w for w in sys.argv[3].split()
           if w not in comps or comps[w]["total"] == 0 or comps[w]["healthy"] != comps[w]["total"]]
if missing:
    sys.exit("not reported healthy: %s" % ", ".join(missing))
if not sys.argv[4]:
    print("    (PROVISIONING is expected without OPENAI_API_KEY: the gateway is declared "
          "but cannot start. Set it to reach READY.)")
elif e["state"] != "READY":
    sys.exit("state=%s, want READY" % e["state"])
PY

cat <<EOF

==> PASS — registered in YCP, installed with one helm command, Meko came up on its own.

    Console:      kubectl --context $CTX -n $CP_NS port-forward svc/meko-api-server 18080:80
                  http://127.0.0.1:18080$BASE/   (admin secret: $ADMIN_KEY)
    MCP server:   kubectl --context $CTX -n $AGENT_NS port-forward svc/meko-mcp-server 18000:8000
    Agent loop:   kubectl --context $CTX -n $AGENT_NS logs -f deploy/meko-agent
    What it runs: kubectl --context $CTX -n $AGENT_NS get all,cm -l meko.dev/stack=true

    Change what it runs (console: Edit desired state), or:
      curl -X POST http://127.0.0.1:$PORT$BASE/api/v1/environments/$ENV_NAME/desired \\
        -H "X-Meko-Admin-Secret: $ADMIN_KEY" -H 'content-type: application/json' \\
        -d '{"resources":[{"kind":"database","name":"meko","spec":{}},
                          {"kind":"mcp-server","name":"mcp","spec":{"image":"$MCP_IMAGE","image_pull_secret":"$PULL_SECRET","secret_ref":"$CREDS_SECRET"}},]}'

    Tear down:    kind delete cluster --name $CLUSTER
EOF
