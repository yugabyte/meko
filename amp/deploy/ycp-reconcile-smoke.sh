#!/usr/bin/env bash
#
# Agent-driven YCP smoke: prove the reconcile-agent converges an amp environment
# to YCP's desired state and reports its inventory back to the control plane.
#
# This is the amp_deploy_gke_ycp assertion. It replaces the old "controller
# reports its own inventory" check: the reconcile-agent is authoritative, so the
# environment is driven ENTIRELY through YCP desired state, never the amp API.
# (An out-of-band resource would just be garbage-collected on the next tick.)
#
# Preconditions (the workflow sets these up before calling us):
#   - amp/deploy/ycp-up.sh stood up the control plane in $NS, registered a kind=amp
#     environment, and emitted YCP_ENV_ID + YCP_ADMIN_KEY;
#   - amp/deploy/up-gke.sh deployed the amp controller AND the reconcile-agent
#     (agent.enabled=true), which has enrolled and is polling desired state.
#
# In order it:
#   1. port-forwards the control plane (meko-api-server) and the amp controller;
#   2. picks the pool owner id — the agent materializes that user before creating
#      the pool (users aren't a YCP kind, but the owner is carried in the pool spec);
#   3. POSTs desired state to YCP: a pool -> cluster -> (default) branch -> a
#      read_write endpoint, named by the agent's hierarchical path convention;
#   4. waits for the agent to create them and report the endpoint READY in the
#      environment's inventory (GET /environments), asserting the whole tree
#      (storage-pool/cluster/branch/endpoint) shows up;
#   5. (SMOKE_EXTENSIONS=1) enables an extension on the branch by editing its
#      spec — the only path an operator has for a branch that already exists —
#      and asserts the agent applies it: the branch reports it in
#      attrs.extensions AND the endpoint the add redeploys comes back Ready with
#      the extension mounted. The attr alone proves little, since
#      add_branch_extensions unions the branch row before it touches any
#      endpoint, so it converges even if every redeploy fails. Opt-in for the
#      same reason as smoke-test.sh's extension scenario: the images and the
#      controller catalog only exist under /ci amp_deploy_gke_extensions;
#   6. withdraws the cluster subtree from desired state (leaving just the pool)
#      and asserts the agent deletes it — the authoritative GC path.
#
# It leaves the pool in desired state on purpose: storage pools have no controller
# delete API, so withdrawing one would leave the agent retrying a delete it can
# never complete. The namespace teardown in the workflow removes everything.
#
# Prereqs: kubectl (pointed at the cluster), curl, jq.
#
# Usage:
#   NS=ci-pr-1 YCP_ENV_ID=amp-gke-smoke YCP_ADMIN_KEY=... [SMOKE_EXTENSIONS=1] \
#     amp/deploy/ycp-reconcile-smoke.sh
set -uo pipefail

NS="${NS:?NS (namespace) is required}"
YCP_ENV_ID="${YCP_ENV_ID:?YCP_ENV_ID is required}"
YCP_ADMIN_KEY="${YCP_ADMIN_KEY:?YCP_ADMIN_KEY is required}"

AMP_PF_PORT="${AMP_PF_PORT:-18080}"
YCP_PF_PORT="${YCP_PF_PORT:-18092}"
SUFFIX="${SUFFIX:-$RANDOM}"

# Resource paths the agent maps onto amp's pool -> cluster -> branch -> endpoint
# hierarchy (see ycp/reconcile/ampbackend). The branch is the cluster's default
# "main", which the cluster create materializes; declaring it keeps the
# authoritative agent from garbage-collecting it.
POOL_NAME="rec-pool-$SUFFIX"
CLUSTER_NAME="recproj"
BRANCH_NAME="main"
EP_NAME="rw"
# Enabled in a SECOND desired-state write, not seeded through the cluster spec,
# so this exercises the addBranchExtensions path — the only one an operator has
# for a branch that already exists.
BRANCH_EXT="pgvector"
# The file the extension's ImageVolume mounts, checked in the redeployed pod.
BRANCH_EXT_FILE="/extensions/pgvector/share/postgresql/extension/vector.control"

pass=0; fail=0
hdr() { printf '\n\033[1;36m== %s\033[0m\n' "$*"; }
ok()  { printf '  \033[1;32mok\033[0m   %s\n' "$*"; pass=$((pass+1)); }
bad() { printf '  \033[1;31mBAD\033[0m  %s\n' "$*"; fail=$((fail+1)); }

AMP_PF_PID=""; YCP_PF_PID=""
cleanup() {
  [ -n "$AMP_PF_PID" ] && kill "$AMP_PF_PID" 2>/dev/null
  [ -n "$YCP_PF_PID" ] && kill "$YCP_PF_PID" 2>/dev/null
}
trap cleanup EXIT

# --- port-forwards ---
hdr "port-forward control plane + amp controller"
kubectl -n "$NS" port-forward svc/meko-api-server "${YCP_PF_PORT}:80" >/tmp/rec-ycp-pf.log 2>&1 &
YCP_PF_PID=$!
kubectl -n "$NS" port-forward svc/amp-controller "${AMP_PF_PORT}:8080" >/tmp/rec-amp-pf.log 2>&1 &
AMP_PF_PID=$!
CP="http://127.0.0.1:${YCP_PF_PORT}/api/v1"
for ((i=0; i<30; i++)); do curl -sf "$CP/health" >/dev/null 2>&1 && break; sleep 1; done
for ((i=0; i<30; i++)); do curl -sf "http://127.0.0.1:${AMP_PF_PORT}/healthz" >/dev/null 2>&1 && break; sleep 1; done
curl -sf "$CP/health" >/dev/null 2>&1 && ok "control plane reachable" || bad "control plane /health unreachable"

# admin GET of the environment as a compact JSON object (empty on miss).
env_json() {
  curl -sf -H "X-Meko-Admin-Secret: ${YCP_ADMIN_KEY}" "$CP/environments" \
    | jq -c --arg id "$YCP_ENV_ID" '.data.items[]|select(.id==$id)' 2>/dev/null
}
# set_desired <resources-json-array> — replace the environment's desired state.
set_desired() {
  curl -sf -X POST "$CP/environments/${YCP_ENV_ID}/desired" \
    -H "X-Meko-Admin-Secret: ${YCP_ADMIN_KEY}" \
    -H 'Content-Type: application/json' \
    -d "$(jq -nc --argjson r "$1" '{resources:$r}')" >/dev/null
}

# --- owner user id (materialized by the agent, not created here) ---
# The pool's owner_user_id is carried in desired state; the reconcile-agent
# ensures that user exists before creating the pool. So the operator only picks
# an id — this asserts the agent-driven path, not an out-of-band POST /users.
hdr "pick pool owner id (agent materializes the user)"
UID_=$(cat /proc/sys/kernel/random/uuid)
[ -n "$UID_" ] && ok "owner id ($UID_)" || bad "generate owner id"

# --- desired state: the full tree ---
hdr "POST desired state (pool -> cluster -> branch -> endpoint)"
DESIRED=$(jq -nc \
  --arg pool "$POOL_NAME" --arg cluster "$CLUSTER_NAME" \
  --arg branch "$BRANCH_NAME" --arg ep "$EP_NAME" --arg uid "$UID_" '
  [ {kind:"storage-pool", name:$pool,                                 spec:{owner_user_id:$uid}},
    {kind:"cluster",      name:($pool+"/"+$cluster),                  spec:{}},
    {kind:"branch",       name:($pool+"/"+$cluster+"/"+$branch),      spec:{}},
    {kind:"endpoint",     name:($pool+"/"+$cluster+"/"+$branch+"/"+$ep), spec:{role:"read_write"}} ]')
set_desired "$DESIRED" && ok "desired state accepted" || bad "POST desired failed"

# --- wait for the agent to converge + report ---
hdr "agent converges the tree and reports endpoint $EP_NAME READY"
# Budget generously: agent tick + create_cluster task + endpoint pod deploy. The
# smoke deploy forces a fast reconcile interval, so most of this is the endpoint
# coming up.
state=""; ep_state=""; kinds=""
for ((i=0; i<60; i++)); do
  ej=$(env_json)
  state=$(printf '%s' "$ej" | jq -r '.state // empty' 2>/dev/null)
  ep_state=$(printf '%s' "$ej" | jq -r --arg n "$EP_NAME" \
    '.resources[]?|select(.kind=="endpoint" and .name==$n)|.state' 2>/dev/null | head -1)
  kinds=$(printf '%s' "$ej" | jq -r '[.resources[]?.kind]|sort|unique|join(",")' 2>/dev/null)
  [ "$ep_state" = ready ] && break
  sleep 5
done

[ "$ep_state" = ready ] && ok "endpoint $EP_NAME reported READY by the agent" \
  || bad "endpoint $EP_NAME not READY (state='${ep_state:-none}', kinds seen: ${kinds:-none})"
[ "$state" = READY ] && ok "environment $YCP_ENV_ID is READY" \
  || bad "environment state=$state (want READY)"
case ",${kinds}," in
  *,storage-pool,*) : ;;
  *) bad "inventory missing storage-pool (kinds: ${kinds:-none})" ;;
esac
for want in cluster branch endpoint; do
  case ",${kinds}," in
    *,"$want",*) ok "inventory has $want" ;;
    *) bad "inventory missing $want (kinds: ${kinds:-none})" ;;
  esac
done

# --- enable an extension on the existing branch (opt-in) ---
if [ "${SMOKE_EXTENSIONS:-0}" != 1 ]; then
  hdr "branch extensions"
  printf '  \033[1;33mskip\033[0m extension assertions (SMOKE_EXTENSIONS != 1; needs /ci amp_deploy_gke_extensions)\n'
else
  # Editing the branch's spec bumps its generation, which is what gets the agent to
  # call addBranchExtensions. That redeploys every endpoint on the branch, so give
  # it the same budget as the endpoint deploy above.
  hdr "enable $BRANCH_EXT on branch $BRANCH_NAME (desired-state edit)"
  EP_ID=$(env_json | jq -r --arg n "$EP_NAME" \
    '.resources[]?|select(.kind=="endpoint" and .name==$n)|.attrs.id' 2>/dev/null | head -1)
  [ -n "$EP_ID" ] && ok "endpoint id ($EP_ID)" || bad "no endpoint id in the inventory"

  WITH_EXT=$(jq -nc --argjson r "$DESIRED" --arg n "${POOL_NAME}/${CLUSTER_NAME}/${BRANCH_NAME}" --arg ext "$BRANCH_EXT" \
    '[ $r[] | if .kind=="branch" and .name==$n then .spec.extensions=[$ext] else . end ]')
  set_desired "$WITH_EXT" && ok "desired state updated with extensions" || bad "POST desired (extensions) failed"

  exts=""
  for ((i=0; i<60; i++)); do
    exts=$(env_json | jq -r --arg n "$BRANCH_NAME" \
      '.resources[]?|select(.kind=="branch" and .name==$n)|.attrs.extensions // empty' 2>/dev/null | head -1)
    case ",${exts}," in *,"$BRANCH_EXT",*) break ;; esac
    sleep 5
  done
  case ",${exts}," in
    *,"$BRANCH_EXT",*) ok "branch attrs.extensions has $BRANCH_EXT (${exts})" ;;
    *) bad "branch attrs.extensions='${exts:-none}' (want it to include $BRANCH_EXT)" ;;
  esac

  # The half the branch row cannot show. The add redeploys the endpoint (a new pod,
  # since the ImageVolume set is fixed at pod start) and the agent cannot tell you
  # whether that worked: the amp API does not report what an endpoint mounts, and
  # the recorded address is never blanked, so the inventory keeps saying "ready".
  # A pull failure on the extension image looks exactly like success from YCP.
  #
  # Wait the endpoint's whole startup budget, not the create-path budget above.
  # This pod restarts onto a branch that already has data, so /data (an emptyDir)
  # is rebuilt from YugabyteDB before Postgres listens — the case the endpoint's
  # startupProbe is sized for (period 10s x threshold 61 = 610s, yb-amp-endpoint
  # values.yaml). Giving up sooner would report a healthy pod as a failed redeploy.
  hdr "endpoint $EP_NAME is redeployed Ready with $BRANCH_EXT mounted"
  if [ -n "$EP_ID" ]; then
    pod=""
    for ((i=0; i<130; i++)); do
      pod=$(kubectl -n "$NS" get pods -o name 2>/dev/null | grep "yb-ep-$EP_ID" | head -1)
      [ -n "$pod" ] && [ "$(kubectl -n "$NS" get "$pod" -o jsonpath='{.status.containerStatuses[0].ready}' 2>/dev/null)" = true ] && break
      pod=""
      sleep 5
    done
    pod="${pod#pod/}"
    [ -n "$pod" ] && ok "endpoint pod Ready after the redeploy ($pod)" \
      || bad "endpoint pod for $EP_ID never came back Ready after addBranchExtensions"
    if [ -n "$pod" ]; then
      kubectl -n "$NS" exec "$pod" -- test -f "$BRANCH_EXT_FILE" 2>/dev/null \
        && ok "$BRANCH_EXT ImageVolume mounted ($BRANCH_EXT_FILE)" \
        || bad "$BRANCH_EXT not mounted in $pod — the redeploy did not take"
    fi
fi

fi

# --- authoritative GC: withdraw the cluster subtree, keep the pool ---
hdr "withdraw cluster from desired -> agent deletes it (cascade)"
KEEP_POOL=$(jq -nc --arg pool "$POOL_NAME" --arg uid "$UID_" \
  '[ {kind:"storage-pool", name:$pool, spec:{owner_user_id:$uid}} ]')
set_desired "$KEEP_POOL" && ok "desired state reduced to the pool" || bad "POST desired (reduce) failed"

gone=""; left=""
for ((i=0; i<48; i++)); do
  ej=$(env_json)
  left=$(printf '%s' "$ej" | jq -r '[.resources[]?|select(.kind=="cluster" or .kind=="endpoint")]|length' 2>/dev/null)
  [ "${left:-1}" = 0 ] && { gone=1; break; }
  sleep 5
done
[ "$gone" = 1 ] && ok "agent deleted the cluster subtree (cascade)" \
  || bad "cluster/endpoint still present after withdrawal (count=${left:-?})"

# --- summary ---
hdr "summary"
printf '  passed: %d   failed: %d\n' "$pass" "$fail"
[ "$fail" -eq 0 ] || exit 1
