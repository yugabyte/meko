#!/usr/bin/env bash
#
# YCP control-plane build + deploy smoke test.
#
# Builds the control plane (api_server, run with --control-plane-mode) from source,
# loads it into a throwaway kind cluster, and asserts the surface an agent and an
# operator depend on:
#
#   1. every workload rolls out, migrate init container included,
#   2. the control plane serves <base>/api/v1/health with both DB checks ok,
#   2b. the whole surface really is mounted under the /ycp base path -- console at
#      /ycp/, API under /ycp/api/v1, nothing at the domain root except the health
#      alias that the kubelet probe uses,
#   3. the fleet API rejects an unauthenticated call and accepts the admin secret,
#   4. registering an environment binds it to an api_key_id,
#   4c. GET /environments/{id} hands back the same install hint register returned, so
#      an operator who did not run it there and then can still find it -- including
#      the YBM pass-through base a registration gave, which is stored for it,
#   4d. a name stays claimed after deregistration, so re-registering it is a 409
#      naming the collision rather than a 500,
#   5. the agent surface refuses a non-JSON content type (415), an empty body (400)
#      and a blank state (400) -- and leaves the environment's state untouched
#      when it does.
#
# What it no longer covers: an agent converging anything. It used to drive the
# retired meko-agent against a public postgres image, which was the only workload it
# could stand up without credentials. The reconcile-agent converges resources through
# meko's api_server instead, and every image in the pinned release is private, so
# there is nothing for CI to point it at. Standing that up is
# test/local/meko-reconcile.sh, which needs AWS credentials and so cannot run here.
# New coverage for the reconcile loop is owed; this file is deliberately the
# control-plane half in the meantime.
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
# Its own slug -- the server derives the environment id as slug(name).
ENV_NAME=e2e-cell
ADMIN_KEY=e2eadminkey   # matches ycp-e2e-secrets in controlplane.yaml
# The api key id the environment is bound to. An agent would authenticate with it as
# YCP_DEV_API_KEY_ID -- there is no YBM here to mint a real key.
API_KEY_ID="$(python3 -c 'import uuid; print(uuid.uuid4())')"
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
  for d in postgres meko-api-server; do
    echo "--- logs: deploy/$d ---"
    k -n "$NS" logs "deploy/$d" --tail=40 --all-containers --prefix 2>&1 | head -60 || true
  done
  echo "--- events ---"
  k -n "$NS" get events --sort-by=.lastTimestamp 2>&1 | tail -25 || true
}

cleanup() {
  rc=$?
  if [ -n "${PF_PID:-}" ]; then kill "$PF_PID" 2>/dev/null || true; fi
  if [ -n "${DP_PF_PID:-}" ]; then kill "$DP_PF_PID" 2>/dev/null || true; fi
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
# that looks unrelated (an admin-gate 404). __API_KEY_ID__ is the run's minted
# binding, rendered the same way for the same reason.
render() {
  out="$TMPDIR_E2E/$(basename "$1")"
  sed -e "s|__BASE_PATH__|$BASE|g" -e "s|__API_KEY_ID__|$API_KEY_ID|g" "$1" > "$out"
  printf '%s' "$out"
}

# ---------------------------------------------------------------- build & cluster

log "building the control-plane image from source ($ENGINE)"
# Built from meko/api_server/, its own Go module.
#
# Host architecture on purpose -- no --platform. The kind node image is multi-arch,
# so an arm64 laptop gets an arm64 cluster running arm64 images, and cross-building
# under emulation would only make this slower.
API_IMG="${IMG_PREFIX}meko-api-server:$TAG"
"$ENGINE" build -f "$MEKO_DIR/api_server/Dockerfile" -t "$API_IMG" "$MEKO_DIR/api_server"

log "creating kind cluster $CLUSTER"
kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
kind create cluster --name "$CLUSTER" --wait 120s

# Context guard: only ever talk to the kind cluster created above. If $CTX somehow
# resolved to a real cluster, its nodes would not be named <cluster>-control-plane.
k get nodes -o name 2>/dev/null | grep -qx "node/${CLUSTER}-control-plane" ||
  die "context guard: --context $CTX is not the kind cluster we just created; refusing to apply"

log "loading the image into the cluster"
if [ "$ENGINE" = docker ]; then
  kind load docker-image "$API_IMG" --name "$CLUSTER"
else
  # `kind load docker-image` shells out to `docker` for the local image lookup even
  # under the podman provider, so on a machine with no docker it reports every image
  # as "not present locally". `load image-archive` has no such dependency: it just
  # streams a tarball into the node's containerd.
  tar="$TMPDIR_E2E/$(printf '%s' "$API_IMG" | tr '/:' '__').tar"
  "$ENGINE" save --format docker-archive -o "$tar" "$API_IMG"
  kind load image-archive "$tar" --name "$CLUSTER"
  rm -f "$tar"
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
  # Read the body, THEN match it — never `curl | grep -q` under `set -o pipefail`.
  # grep -q exits the moment it matches, and the marker is in the <title> at byte 8, so
  # it closes the pipe while curl still has ~42 KB to write. curl then dies with
  # "(23) Failure writing output to destination" and pipefail fails the pipeline even
  # though the grep succeeded. It passed for as long as the page was small enough for
  # curl to finish writing first; growing the console by 6 KB made it deterministic.
  console="$(curl -fsS "http://127.0.0.1:$PORT$BASE/")"
  case "$console" in
    *"Yugabyte Control Plane"*) ;;
    *) die "GET $BASE/ did not serve the console HTML" ;;
  esac
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

# 4. Register the environment, bound to this run's api_key_id. This is the
#    operator step (YBM performs it in production).
# 4a. Registration seeds nothing — a data plane's workloads come from its chart — so
#     what an environment declares is what its caller declared. This one declares the
#     two kinds a meko agent serves, which is also the positive half of the kind check
#     below. No agent attaches to it: converging these needs an api-server to converge
#     against, and this cluster has none.
log "registering environment $ENV_NAME (api_key_id ${API_KEY_ID:0:8}...)"
reg="$(api POST /api/v1/environments -H 'content-type: application/json' \
  -d "{\"name\":\"$ENV_NAME\",\"account_id\":\"11111111-1111-1111-1111-111111111111\",\"placement\":\"cloud\",\"region\":\"local\",
       \"api_key_id\":\"$API_KEY_ID\",
       \"desired\":[{\"kind\":\"agent\",\"name\":\"ycp\"},
                   {\"kind\":\"datapack\",\"name\":\"ycp/smoke\",\"spec\":{\"description\":\"declared by the e2e\"}}]}")"
bound="$(printf '%s' "$reg" | python3 -c 'import json,sys; print(json.load(sys.stdin)["data"]["environment"]["api_key_id"])')"
[ "$bound" = "$API_KEY_ID" ] || die "register response binding is $bound, want $API_KEY_ID: ${reg:0:200}"
echo "    environment bound to the api key id"

# 4b. What was declared is what reads back, and a kind no meko agent serves is refused
#     where the operator is standing rather than a tick later as permanent drift. The
#     404-vs-400 distinction matters: a 400 here proves the kind check ran, not that
#     the route or the environment is missing.
declared="$(api GET "/api/v1/environments/$ENV_NAME/desired" |
  python3 -c 'import json,sys; r=json.load(sys.stdin)["data"]["resources"]; print(",".join(x["kind"]+"/"+x["name"] for x in r))')"
[ "$declared" = "agent/ycp,datapack/ycp/smoke" ] ||
  die "declared desired state reads back as '$declared', want 'agent/ycp,datapack/ycp/smoke'"
code="$(curl -s -o /dev/null -w '%{http_code}' -X POST \
  "http://127.0.0.1:$PORT$BASE/api/v1/environments/$ENV_NAME/desired" \
  -H "X-Meko-Admin-Secret: $ADMIN_KEY" -H 'content-type: application/json' \
  -d '{"resources":[{"kind":"database","name":"meko","spec":{}}]}')"
[ "$code" = "400" ] || die "declaring kind 'database' on a meko environment returned $code, want 400"
echo "    desired state reads back, and a kind the agent cannot serve is refused (400)"

# 4c. The install hint has to outlive the register response: an operator who did not
#     paste it there and then reads it back off the environment, byte for byte. This
#     registration sends no agent_base_url, so both renderings take the
#     request-address fallback.
reg_hint="$(printf '%s' "$reg" |
  python3 -c 'import json,sys; print(json.load(sys.stdin)["data"]["install_hint"])')"
got_hint="$(api GET "/api/v1/environments/$ENV_NAME" |
  python3 -c 'import json,sys; print(json.load(sys.stdin)["data"].get("install_hint",""))')"
[ -n "$got_hint" ] || die "GET /environments/$ENV_NAME returned no install hint"
[ "$got_hint" = "$reg_hint" ] || die "the environment's install hint is not register's"
echo "    the install hint reads back off the environment"

# 4c'. The extension catalog, on a throwaway amp environment -- only amp has one. Null
#      before any agent relays one (the console's cue to fall back, so it must not read
#      as []), the relayed catalog after; and a meko environment's key is refused.
ext_key="$(python3 -c 'import uuid; print(uuid.uuid4())')"
api POST /api/v1/environments -H 'content-type: application/json' \
  -d '{"name":"ext-check","kind":"amp","account_id":"11111111-1111-1111-1111-111111111111",
       "placement":"cloud","region":"local","api_key_id":"'"$ext_key"'"}' >/dev/null ||
  die "registering ext-check failed"
ext_of() {
  api GET /api/v1/environments/ext-check/extensions |
    python3 -c 'import json,sys; print(json.dumps(json.load(sys.stdin)["data"]["extensions"]))'
}
put_ext() {
  curl -s -o /dev/null -w '%{http_code}' -X PUT "http://127.0.0.1:$PORT$BASE/api/v1/agent/extensions" \
    -H "X-Meko-Admin-Secret: $ADMIN_KEY" -H "X-Meko-Api-Key-ID: $1" \
    -H 'content-type: application/json' \
    -d '{"extensions":[{"name":"pgvector","builtin":true,"preload":false}]}'
}
[ "$(ext_of)" = "null" ] || die "a never-reported catalog reads as $(ext_of), want null"
code="$(put_ext "$ext_key")"
[ "$code" = "204" ] || die "PUT /agent/extensions for an amp environment returned $code, want 204"
got_ext="$(ext_of)"
[ "$got_ext" = '[{"name": "pgvector", "builtin": true, "preload": false}]' ] ||
  die "the relayed catalog reads back as $got_ext"
code="$(put_ext "$API_KEY_ID")"
[ "$code" = "400" ] || die "PUT /agent/extensions for a meko environment returned $code, want 400"
api DELETE /api/v1/environments/ext-check >/dev/null 2>&1 || true
echo "    the extension catalog is null until relayed, then reads back; meko's is refused"

#     A YBM-registered environment reads back the pass-through base its registration
#     gave, which is the whole reason that value is stored: rendered from the request
#     address instead, the hint would point its agent somewhere it cannot reach.
#     Throwaway environment, like the one above — no agent attaches to it.
hint_key="$(python3 -c 'import uuid; print(uuid.uuid4())')"
api POST /api/v1/environments -H 'content-type: application/json' \
  -d '{"name":"hint-check","account_id":"11111111-1111-1111-1111-111111111111","placement":"cloud",
       "region":"local","api_key_id":"'"$hint_key"'",
       "agent_base_url":"https://cloud.yugabyte.com/api/public/v1"}' >/dev/null ||
  die "registering hint-check failed"
stored_hint="$(api GET /api/v1/environments/hint-check |
  python3 -c 'import json,sys; print(json.load(sys.stdin)["data"].get("install_hint",""))')"
api DELETE /api/v1/environments/hint-check >/dev/null 2>&1 || true
# 4d below asserts that a DEREGISTERED name stays claimed, so it has to know the
# deregister actually happened -- the DELETE above is best-effort because the hint check
# does not care, and a silently failed one would leave 4d passing against a LIVE row
# while printing that a deregistered name stays claimed.
hint_state="$(api GET /api/v1/environments/hint-check 2>/dev/null |
  python3 -c 'import json,sys; print(json.load(sys.stdin)["data"].get("state",""))' || true)"
[ "$hint_state" = "DEREGISTERED" ] ||
  die "hint-check reads back as '$hint_state' after DELETE, want DEREGISTERED"
want_url="reconcileAgent.ycpUrl=https://cloud.yugabyte.com/api/public/v1"
want_url="$want_url/accounts/11111111-1111-1111-1111-111111111111/ycp"
case "$stored_hint" in
  *"$want_url"*) echo "    a stored agent_base_url is what the hint dials" ;;
  *) die "hint-check's hint does not dial its stored agent base url: ${stored_hint:0:400}" ;;
esac

# 4d. A deregister is a state change, not a row delete, so hint-check's name is still
#     claimed and re-registering it is the 409 an operator hits on every repeat run.
#
#     Postgres here, so this CANNOT catch the shape of the bug it was added for -- YSQL
#     reports the violation with no constraint name, which the repository once keyed on
#     (CLOUDGA-36978); that case lives in the repository unit tests. What this covers is
#     the whole chain through a real server.
#
#     A FRESH api_key_id so the request is a name conflict and nothing else, and the
#     code asserted rather than just the status -- Insert refuses at the environments
#     row, before the key binding, so a 409 that was API_KEY_BOUND would mean the
#     ordering changed underneath this. Inline curl because api() sends -f.
dup_key="$(python3 -c 'import uuid; print(uuid.uuid4())')"
dup="$(curl -sS -w '\n%{http_code}' -X POST "http://127.0.0.1:$PORT$BASE/api/v1/environments" \
  -H "X-Meko-Admin-Secret: $ADMIN_KEY" -H 'content-type: application/json' \
  -d '{"name":"hint-check","account_id":"11111111-1111-1111-1111-111111111111","placement":"cloud",
       "region":"local","api_key_id":"'"$dup_key"'"}')"
[ "$(printf '%s' "$dup" | tail -n1)" = "409" ] ||
  die "re-registering the deregistered name hint-check returned $(printf '%s' "$dup" | tail -n1), want 409: $dup"
printf '%s' "$dup" | grep -q ENVIRONMENT_EXISTS ||
  die "the repeat registration was refused, but not as ENVIRONMENT_EXISTS: $dup"
printf '%s' "$dup" | grep -q hint-check ||
  die "the repeat registration's 409 does not name the environment that collided: $dup"
echo "    a deregistered name stays claimed: re-registering is 409 ENVIRONMENT_EXISTS"

# 5. The agent surface refuses a request it cannot read, and — the part that matters
#    — refuses it without touching the environment. CLOUDGA-36984: a status POST
#    whose body the gateway had dropped bound to a zero-valued report and wrote the
#    environment's state as empty, on a row whose heartbeat the same statement
#    refreshed. Unit tests cover the codes; this covers them over the wire, through
#    the real middleware chain, against real Postgres.
log "probing the agent surface with requests it must refuse"
agent_post_code() {
  curl -s -o /dev/null -w '%{http_code}' -X POST \
    "http://127.0.0.1:$PORT$BASE/api/v1/agent/status" \
    -H "X-Meko-Admin-Secret: $ADMIN_KEY" -H "X-Meko-Api-Key-ID: $API_KEY_ID" \
    -H "content-type: $1" --data-binary "$2"
}
state_before="$(api GET "/api/v1/environments/$ENV_NAME" |
  python3 -c 'import json,sys; print(json.load(sys.stdin)["data"]["state"])')"

code="$(agent_post_code text/plain hello)"
[ "$code" = "415" ] || die "POST /agent/status as text/plain returned $code, want 415"
code="$(agent_post_code application/json '')"
[ "$code" = "400" ] || die "POST /agent/status with an empty body returned $code, want 400"
code="$(agent_post_code application/json '{"environment_state":""}')"
[ "$code" = "400" ] || die "POST /agent/status with a blank state returned $code, want 400"

state_after="$(api GET "/api/v1/environments/$ENV_NAME" |
  python3 -c 'import json,sys; print(json.load(sys.stdin)["data"]["state"])')"
[ "$state_after" = "$state_before" ] ||
  die "the refused probes moved the environment's state from '$state_before' to '$state_after'"
echo "    refused (415/400/400), and the environment's state survived them"

log "PASS — control plane built, deployed, gated, and registering against api_server --control-plane-mode"
