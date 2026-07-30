#!/usr/bin/env bash
#
# YCP build + deploy end-to-end smoke test.
#
# Builds the control plane (api_server, run with --control-plane-mode) and the
# BYOC agent from source, loads both into a throwaway kind cluster, and drives the
# full bring-up an operator does by hand:
#
#   1. every workload rolls out, migrate init container included,
#   2. the control plane serves <base>/api/v1/health with both DB checks ok,
#   2b. the whole surface really is mounted under the /ycp base path -- console at
#      /ycp/, API under /ycp/api/v1, nothing at the domain root except the health
#      alias that the kubelet probe uses,
#   3. the fleet API rejects an unauthenticated call and accepts the admin secret,
#   4. registering an environment mints an enrollment token,
#   5. the agent enrolls with that token and reports itself READY with real
#      cluster facts,
#   6. the agent survives a restart WITHOUT re-enrolling, because the status token
#      it got at (5) was persisted on both sides.
#
# (6) is the assertion that matters most. The enrollment token is a 30-minute
# bootstrap credential, so an agent that re-enrolls on every restart works in a
# demo and then locks itself out an hour later. Nothing caught that before.
#
# There is no seed path here on purpose: the environment is registered through the
# real POST /api/v1/environments, so the token the agent uses is one the API
# actually minted. That is why this script is a sequence rather than a single
# `kubectl apply` -- the agent's Secret cannot exist until the control plane is up.
#
# Coverage this gives beyond `go test`: image build, image start, flag names
# (a renamed flag crash-loops the pod), the portable control-plane migration set
# against real Postgres, RBAC, service DNS, and the token handshake over the wire.
#
# Safety: every kubectl call is pinned with --context to the kind cluster this
# script creates, and it refuses to proceed unless that context resolves to a
# kind node. It never reads or writes your current kubeconfig context.
#
# Usage:
#   ycp/test/e2e/run.sh                  # create, test, tear down
#   YCP_E2E_KEEP=1 ycp/test/e2e/run.sh   # leave the cluster up to poke at
#
# Either docker or podman works; podman is auto-detected and kind is told to use
# it. Override with YCP_E2E_ENGINE=podman|docker.
#
set -euo pipefail

CLUSTER="${YCP_E2E_CLUSTER:-ycp-e2e}"
CTX="kind-${CLUSTER}"
NS=ycp-e2e
TAG="${YCP_E2E_TAG:-e2e}"
KEEP="${YCP_E2E_KEEP:-0}"
PORT="${YCP_E2E_PORT:-18080}"
TIMEOUT="${YCP_E2E_TIMEOUT:-300s}"
# Must match ENV_NAME in agent.yaml, and must be its own slug -- the server derives
# the environment id as slug(name), and the agent enrolls by name.
ENV_NAME=e2e-cell
ADMIN_KEY=e2eadminkey   # matches ycp-e2e-secrets in controlplane.yaml
# Subpath the control plane is mounted under. Non-empty on purpose: production
# fronts YCP at https://devcloud.yugabyte.com/ycp/, so the prefixed mount is the
# shape worth smoke-testing. `YCP_E2E_BASE_PATH= ./run.sh` tests the root mount
# instead -- the manifests pick this up through the __BASE_PATH__ placeholder
# render() substitutes, so there is nothing else to edit.
BASE="${YCP_E2E_BASE_PATH-/ycp}"

# This script lives at ycp/test/e2e/run.sh.
E2E_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
YCP_DIR="$(cd "$E2E_DIR/../.." && pwd)"
REPO_DIR="$(cd "$YCP_DIR/.." && pwd)"
# ycp/ sits at the repository root because it is shared with amp; the control
# plane it tests against lives in the meko tree.
MEKO_DIR="$REPO_DIR/meko"

log() { printf '\n==> %s\n' "$*"; }
die() { printf '\nFAIL: %s\n' "$*" >&2; exit 1; }
k() { kubectl --context "$CTX" "$@"; }

for bin in kind kubectl curl python3; do
  command -v "$bin" >/dev/null 2>&1 || die "$bin is required but not on PATH"
done

# Container engine. Both build the images and both work as a kind provider; podman
# needs to be named explicitly through KIND_EXPERIMENTAL_PROVIDER, and kind reads
# that for `create cluster`, `load docker-image` and `delete cluster` alike.
ENGINE="${YCP_E2E_ENGINE:-}"
if [ -z "$ENGINE" ]; then
  for e in docker podman; do
    command -v "$e" >/dev/null 2>&1 && { ENGINE="$e"; break; }
  done
fi
[ -n "$ENGINE" ] || die "need docker or podman on PATH (or set YCP_E2E_ENGINE)"
command -v "$ENGINE" >/dev/null 2>&1 || die "YCP_E2E_ENGINE=$ENGINE is not on PATH"
# Tag prefix for locally built images. Empty for docker; podman needs images
# fully qualified, because it stores a bare `name:tag` as `localhost/name:tag` and
# then neither `kind load docker-image name:tag` finds it nor — if forced in —
# would kubelet match it, since kubelet resolves the manifests' `name:tag` to
# `docker.io/library/name:tag`. Naming it that way from the start makes the two
# agree. The manifests keep the short name either way.
IMG_PREFIX=""

if [ "$ENGINE" = podman ]; then
  export KIND_EXPERIMENTAL_PROVIDER=podman
  IMG_PREFIX="docker.io/library/"
fi
"$ENGINE" info >/dev/null 2>&1 ||
  die "$ENGINE is installed but not usable — is the daemon/machine running? (podman machine start)"

# api() prints the response body and fails the call on a non-2xx, so a broken
# request surfaces here instead of as a confusing parse error downstream.
# api() prefixes $BASE, so every call below is written as the route is declared
# in the router and the subpath stays in one place.
api() {
  local method="$1" path="$2"; shift 2
  curl -fsS -X "$method" "http://127.0.0.1:$PORT$BASE$path" \
    -H "X-Meko-Admin-Secret: $ADMIN_KEY" "$@"
}

# Just the status code, no auth header, no -f. For asserting the gate.
# Takes a full path (no $BASE prefixing) so it can also assert what is NOT
# served at the domain root under a base path.
status_of() {
  curl -so /dev/null -w '%{http_code}' "http://127.0.0.1:$PORT$1" || true
}

diagnose() {
  log "diagnostics (the run failed)"
  k -n "$NS" get pods -o wide 2>&1 || true
  k -n "$NS" describe pods 2>&1 |
    grep -E '^(Name:|  Warning|  Normal|    Reason|    State|    Last State|    Exit Code)' || true
  for d in postgres meko-api-server meko-agent; do
    echo "--- logs: deploy/$d ---"
    k -n "$NS" logs "deploy/$d" --tail=40 --all-containers --prefix 2>&1 | head -60 || true
  done
  echo "--- events ---"
  k -n "$NS" get events --sort-by=.lastTimestamp 2>&1 | tail -25 || true
}

cleanup() {
  rc=$?
  if [ -n "${PF_PID:-}" ]; then kill "$PF_PID" 2>/dev/null || true; fi
  if [ -n "${TMPDIR_E2E:-}" ]; then rm -rf "$TMPDIR_E2E"; fi
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

# Scratch space for image archives (podman path) and rendered manifests — inside
# the trap's reach. Spelled out rather than `mktemp -t ycp-e2e`: BSD mktemp treats
# that as a prefix and appends randomness, GNU mktemp rejects it for having no
# trailing X's.
TMPDIR_E2E="$(mktemp -d "${TMPDIR:-/tmp}/ycp-e2e.XXXXXX")"

# Every base image these builds pull is public, so this run needs no registry
# credentials at all. Point podman at an empty auth file and it stops consulting
# ~/.docker/config.json — whose `credHelpers` it otherwise invokes for unrelated
# registries, failing the build outright when the helper's own credentials have
# expired (`gcloud.auth.docker-helper ... Reauthentication failed`). Set
# REGISTRY_AUTH_FILE yourself to override. Created here, after the trap, so an
# early exit (a stopped podman machine is the common one) cannot leak it.
if [ "$ENGINE" = podman ] && [ -z "${REGISTRY_AUTH_FILE:-}" ]; then
  AUTH_FILE="$TMPDIR_E2E/auth.json"
  printf '{"auths":{}}' > "$AUTH_FILE"
  export REGISTRY_AUTH_FILE="$AUTH_FILE"
fi

# $BASE is the single source of the subpath: the manifests carry a __BASE_PATH__
# placeholder and are rendered here at apply time. Hardcoding it in the YAML as
# well would let the two drift, and because the root health alias answers either
# way, a mismatch would sail past the health gate and fail later as something
# that looks unrelated (an admin-gate 404).
render() {
  out="$TMPDIR_E2E/$(basename "$1")"
  sed "s|__BASE_PATH__|$BASE|g" "$1" > "$out"
  printf '%s' "$out"
}

# ---------------------------------------------------------------- build & cluster

log "building images from source ($ENGINE)"
# Two build contexts: the control plane is meko/api_server/ (its own Go module), the
# agent is ycp/. Same split as the Dockerfiles the release workflows use.
#
# Host architecture on purpose -- no --platform. The kind node image is multi-arch,
# so an arm64 laptop gets an arm64 cluster running arm64 images, and cross-building
# under emulation would only make this slower.
API_IMG="${IMG_PREFIX}meko-api-server:$TAG"
AGENT_IMG="${IMG_PREFIX}meko-agent:$TAG"
"$ENGINE" build -f "$MEKO_DIR/api_server/Dockerfile" -t "$API_IMG"   "$MEKO_DIR/api_server"
"$ENGINE" build -f "$YCP_DIR/Dockerfile.agent"       -t "$AGENT_IMG" "$YCP_DIR"
# dpmcp is built but not loaded or deployed: this run provisions no datapacks, so
# nothing would pull it. Building it here is what keeps a broken Dockerfile.dpmcp
# from merging green and then failing ycp-images.yml on main, where the only
# symptom is a per-datapack MCP pod stuck in ImagePullBackOff.
"$ENGINE" build -f "$YCP_DIR/Dockerfile.dpmcp"       -t "${IMG_PREFIX}dpmcp:$TAG" "$YCP_DIR"

log "creating kind cluster $CLUSTER"
kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
kind create cluster --name "$CLUSTER" --wait 120s

# Context guard: only ever talk to the kind cluster created above. If $CTX somehow
# resolved to a real cluster, its nodes would not be named <cluster>-control-plane.
k get nodes -o name 2>/dev/null | grep -qx "node/${CLUSTER}-control-plane" ||
  die "context guard: --context $CTX is not the kind cluster we just created; refusing to apply"

log "loading images into the cluster"
if [ "$ENGINE" = docker ]; then
  kind load docker-image "$API_IMG" "$AGENT_IMG" --name "$CLUSTER"
else
  # `kind load docker-image` shells out to `docker` for the local image lookup even
  # under the podman provider, so on a machine with no docker it reports every image
  # as "not present locally". `load image-archive` has no such dependency: it just
  # streams a tarball into the node's containerd.
  for img in "$API_IMG" "$AGENT_IMG"; do
    tar="$TMPDIR_E2E/$(printf '%s' "$img" | tr '/:' '__').tar"
    "$ENGINE" save --format docker-archive -o "$tar" "$img"
    kind load image-archive "$tar" --name "$CLUSTER"
    rm -f "$tar"
  done
fi

# ------------------------------------------------------------------ control plane

log "applying the control plane"
k apply -f "$(render "$E2E_DIR/controlplane.yaml")"

log "waiting for Postgres"
k -n "$NS" rollout status deploy/postgres --timeout="$TIMEOUT"

# The migrate init container retries its own DB connection, so a few
# "database ping failed" lines here are expected rather than a failure.
log "waiting for the control plane (migrate init container, then api-server)"
k -n "$NS" rollout status deploy/meko-api-server --timeout="$TIMEOUT"

k -n "$NS" logs deploy/meko-api-server -c migrate --tail=-1 | grep -q 'migrations complete' ||
  die "the migrate init container did not report 'migrations complete'"
echo "    controlplane migrations applied"

log "port-forwarding the control plane"
k -n "$NS" port-forward svc/meko-api-server "$PORT:80" >/dev/null 2>&1 &
PF_PID=$!

# 2. Health, including both DB connections. The endpoint answers 200 even when a
#    DB is down, so the body is what has to be checked -- not the status code.
log "checking $BASE/api/v1/health"
health=""
for _ in $(seq 1 30); do
  health="$(curl -fsS "http://127.0.0.1:$PORT$BASE/api/v1/health" 2>/dev/null || true)"
  [ -n "$health" ] && break
  sleep 2
done
[ -n "$health" ] || die "$BASE/api/v1/health never responded"
python3 - "$health" <<'PY' || die "health reported a database as unavailable: see above"
import json, sys
d = json.loads(sys.argv[1])["data"]
bad = {k: v for k, v in d.items() if k.endswith("database") and v != "ok"}
if bad:
    print(f"    unhealthy: {bad}", file=sys.stderr)
    sys.exit(1)
print(f"    health -> {d}")
PY

# 2b. The subpath mount, which is how this is deployed: the console answers at
#     the prefix, the API lives under it, and the domain root serves nothing but
#     the health alias that kubelet's probe uses. Skipped when BASE is empty.
if [ -n "$BASE" ]; then
  log "checking the $BASE subpath mount"
  code="$(status_of "$BASE/")"
  [ "$code" = "200" ] || die "GET $BASE/ (console) returned $code, want 200"
  curl -fsS "http://127.0.0.1:$PORT$BASE/" | grep -q "Yugabyte Control Plane" ||
    die "GET $BASE/ did not serve the console HTML"
  code="$(status_of "$BASE")"
  [ "$code" = "200" ] || die "GET $BASE (no trailing slash) returned $code, want 200"
  code="$(status_of /api/v1/environments)"
  [ "$code" = "404" ] || die "GET /api/v1/environments at the domain root returned $code, want 404"
  code="$(status_of /)"
  [ "$code" = "404" ] || die "GET / at the domain root returned $code, want 404"
  # The pod is Ready, which kubelet only concluded by probing the unprefixed
  # path -- assert it directly so the reason is obvious if the alias regresses.
  code="$(status_of /api/v1/health)"
  [ "$code" = "200" ] || die "GET /api/v1/health (probe alias) returned $code, want 200"
  echo "    console at $BASE/ and $BASE, API under $BASE, root 404s, probe alias 200"
fi

# 3. The fleet routes are gated. 401 (not 503) also confirms the server was given
#    an admin key: without --admin-api-key the middleware answers 503 to
#    everything, and the whole flow below would fail with no obvious cause.
log "checking the admin-secret gate"
code="$(status_of "$BASE/api/v1/environments")"
[ "$code" = "401" ] || die "GET $BASE/api/v1/environments without a secret returned $code, want 401"
api GET /api/v1/environments >/dev/null || die "GET $BASE/api/v1/environments with the admin secret failed"
echo "    unauthenticated -> 401, authenticated -> 200"

# ------------------------------------------------------------------------- agent

# 4. Register the environment and take the one-time enrollment token from the
#    response. This is the operator step from YCP-RUNBOOK.md §4.
log "registering environment $ENV_NAME"
reg="$(api POST /api/v1/environments -H 'content-type: application/json' \
  -d "{\"name\":\"$ENV_NAME\",\"account_id\":\"e2e\",\"placement\":\"cloud\",\"region\":\"local\"}")"
TOKEN="$(printf '%s' "$reg" | python3 -c 'import json,sys; print(json.load(sys.stdin)["data"]["enrollment_token"])')"
[ -n "$TOKEN" ] || die "no enrollment_token in the register response: ${reg:0:200}"
case "$TOKEN" in
  mko_enr_*) ;;
  *) die "enrollment token has an unexpected prefix (want mko_enr_): ${TOKEN:0:12}..." ;;
esac
echo "    enrollment token minted (${TOKEN:0:12}...)"

log "handing the token to the agent and deploying it"
k -n "$NS" create secret generic meko-agent-enrollment --from-literal=enrollment-token="$TOKEN"
k apply -f "$(render "$E2E_DIR/agent.yaml")"
k -n "$NS" rollout status deploy/meko-agent --timeout="$TIMEOUT"

# 5. The real handshake: the agent read the token from its Secret, reached the
#    control plane over service DNS, and sent cluster facts it had to get from the
#    k8s API through its RBAC.
log "checking enrollment"
enrolled=""
for _ in $(seq 1 60); do
  if enrolled="$(k -n "$NS" logs deploy/meko-agent --tail=-1 2>/dev/null |
                   grep "enrolled: environment_id=$ENV_NAME" | tail -1)"; then
    [ -n "$enrolled" ] && break
  fi
  sleep 2
done
[ -n "$enrolled" ] || die "agent never enrolled (no 'enrolled: environment_id=$ENV_NAME' in its logs)"
printf '    %s\n' "$enrolled"

# READY + live + a real node count together are the signal the whole chain works:
# the agent authenticated with its status token, read cluster facts, and the
# control plane persisted them.
log "checking the fleet API reports it READY"
for _ in $(seq 1 60); do
  envs="$(api GET /api/v1/environments 2>/dev/null || true)"
  printf '%s' "$envs" | grep -q '"state":"READY"' && break
  sleep 2
done
# argv, not stdin: a heredoc already owns this python's stdin.
python3 - "$ENV_NAME" "${envs:-}" <<'PY' || die "environment never reached READY with live cluster facts"
import json, sys
want = sys.argv[1]
items = json.loads(sys.argv[2] or "{}").get("data", {}).get("items") or []
e = next((x for x in items if x["id"] == want), None)
if e is None:
    print(f"    {want} not in {[x['id'] for x in items]}", file=sys.stderr)
    sys.exit(1)
problems = []
if e["state"] != "READY":        problems.append(f"state={e['state']}")
if not e.get("live"):            problems.append("live=false")
if e.get("node_count", 0) < 1:   problems.append("node_count=0")
if not e.get("k8s_version"):     problems.append("k8s_version empty")
if problems:
    print("    " + ", ".join(problems), file=sys.stderr)
    sys.exit(1)
print(f"    {e['id']}: state=READY live=true nodes={e['node_count']} k8s={e['k8s_version']}")
PY

# 6. Restart the agent. It must come back on the persisted status token and NOT
#    re-enroll -- the enrollment token is a short-lived bootstrap credential, so an
#    agent that needs it again is one that breaks the first time it restarts after
#    the TTL. Deleting the pod (rather than rollout restart) keeps exactly one pod
#    alive, so the log assertions below cannot read the previous pod's output.
log "restarting the agent — it must resume, not re-enroll"
k -n "$NS" delete pod -l app=meko-agent --wait=true
k -n "$NS" rollout status deploy/meko-agent --timeout="$TIMEOUT"
resumed=""
for _ in $(seq 1 45); do
  if k -n "$NS" logs deploy/meko-agent --tail=-1 2>/dev/null |
      grep -q "resuming as env=$ENV_NAME with the persisted status token"; then
    resumed=yes
    break
  fi
  sleep 2
done
[ "$resumed" = "yes" ] ||
  die "the restarted agent did not resume from persisted state (expected 'resuming as env=$ENV_NAME ...'); \
it likely re-enrolled, which only works while the bootstrap token is unexpired"
if k -n "$NS" logs deploy/meko-agent --tail=-1 | grep -q "enrolled: environment_id="; then
  die "the restarted agent re-enrolled as well as resuming — the enrollment token is spent on every restart"
fi
echo "    agent resumed on the persisted status token, no re-enrollment"

log "PASS — build and deploy verified against api_server --control-plane-mode"
