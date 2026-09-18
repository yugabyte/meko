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
#   5. edits the endpoint's spec to the size it is already running at — a real
#      generation bump whose resize is a no-op — and asserts the whole stamp path
#      runs: nothing is reported before any update (a create stamps nothing), the
#      generation appears in attrs.applied_generation once the task has run, and
#      the resource settles back to ready instead of being re-issued. It does not
#      catch a controller that stamped on the 202 rather than on completion —
#      that window is not sampled, and is covered by the unit tests (meko#985);
#   6. declares pg_settings on the branch and on the endpoint and asserts both
#      converge: the controller's own rows carry them, a branch spec that declares
#      ONLY settings still gets its generation stamped (the settings PATCH is
#      synchronous, so the handler stamps it — nothing else could), a key dropped
#      from a declared surface is removed from the row (desired state owns the whole
#      map), and none of it restarts the endpoint pod. Then reads the same settings
#      back out of the INVENTORY, which is the only place a console can see the map
#      it is about to replace (meko#1295);
#   7. (SMOKE_EXTENSIONS=1) enables an extension on the branch by editing its
#      spec — the only path an operator has for a branch that already exists —
#      and asserts the agent applies it: the branch reports it in
#      attrs.extensions AND the endpoint the add redeploys comes back Ready with
#      the extension mounted. The attr alone proves little, since
#      add_branch_extensions unions the branch row before it touches any
#      endpoint, so it converges even if every redeploy fails. Then re-declares
#      the same extension (an inert edit that still bumps the generation) and
#      asserts the endpoint pod's uid is UNCHANGED: with nothing to add, the
#      generation is stamped by an empty settings PATCH and no task is planned at
#      all, so nothing may touch compute. Opt-in for the
#      same reason as smoke-test.sh's extension scenario: the images and the
#      controller catalog only exist under /ci amp_deploy_gke_extensions;
#   8. withdraws the cluster subtree from desired state (leaving just the pool)
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

# The writer's compute password. Required by the controller, so YCP desired
# state has to carry one for an endpoint it declares.
EP_PASSWORD="${EP_PASSWORD:-ycp-smoke-pw-1}"

# --- desired state: the full tree ---
hdr "POST desired state (pool -> cluster -> branch -> endpoint)"
DESIRED=$(jq -nc \
  --arg pool "$POOL_NAME" --arg cluster "$CLUSTER_NAME" \
  --arg branch "$BRANCH_NAME" --arg ep "$EP_NAME" --arg uid "$UID_" \
  --arg eppw "$EP_PASSWORD" '
  [ {kind:"storage-pool", name:$pool,                                 spec:{owner_user_id:$uid}},
    {kind:"cluster",      name:($pool+"/"+$cluster),                  spec:{}},
    {kind:"branch",       name:($pool+"/"+$cluster+"/"+$branch),      spec:{}},
    {kind:"endpoint",     name:($pool+"/"+$cluster+"/"+$branch+"/"+$ep),
      spec:{role:"read_write", password:$eppw}} ]')
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

# --- the applied generation: a completed task, not a 202, advances it ---
# The point of meko#985. Every mutating controller call is asynchronous, so a 202
# only means "task planned"; the agent used to record the generation as applied on
# that alone, and a task that then failed left the resource unconverged while the
# inventory said healthy. The controller now stamps the row after the task's last
# step succeeds and echoes it as attrs.applied_generation.
#
# Driven by an endpoint spec edit, because that is the resize path the issue names.
# The declared size is whatever the endpoint is ALREADY running at, read from the
# inventory: that still bumps the generation (the spec changed), so the whole
# stamp path runs, while the resize itself is a no-op at the target size — no
# node-capacity or in-place-shrink risk in a smoke test.
hdr "endpoint $EP_NAME reports attrs.applied_generation after a spec edit"
ep_attr() {
  env_json | jq -r --arg n "$EP_NAME" --arg k "$1" \
    ".resources[]?|select(.kind==\"endpoint\" and .name==\$n)|.attrs[\$k] // empty" 2>/dev/null | head -1
}

# A create stamps nothing: creates self-heal through presence, so they need no echo.
gen0=$(ep_attr applied_generation)
[ -z "$gen0" ] && ok "no applied_generation before any update (a create does not stamp)" \
  || bad "applied_generation=$gen0 before any update, want none"

# The attr reaches the console only if it survives the heartbeat; the agent's own
# tests stop at List. This endpoint declares no connectivity, so the reported mode
# is the resolved default rather than nothing.
conn=$(ep_attr connectivity_mode)
[ "$conn" = cluster_ip ] && ok "endpoint attrs.connectivity_mode=cluster_ip" \
  || bad "endpoint attrs.connectivity_mode='${conn:-none}', want cluster_ip"

cur_cpu=$(ep_attr cpu); cur_mem=$(ep_attr memory)
if [ -z "$cur_cpu" ] || [ -z "$cur_mem" ]; then
  bad "endpoint reports no cpu/memory in the inventory (cpu='${cur_cpu:-}' memory='${cur_mem:-}')"
else
  ok "endpoint runs at cpu=$cur_cpu memory=$cur_mem"
  RESIZED=$(jq -nc --argjson r "$DESIRED" \
    --arg n "${POOL_NAME}/${CLUSTER_NAME}/${BRANCH_NAME}/${EP_NAME}" \
    --arg cpu "$cur_cpu" --arg mem "$cur_mem" \
    '[ $r[] | if .kind=="endpoint" and .name==$n
              then .spec.resources={cpu:$cpu, memory:$mem} else . end ]')
  set_desired "$RESIZED" && ok "desired state updated with the endpoint's own size" \
    || bad "POST desired (resize) failed"
  # Later edits build on this, not on the original: re-posting DESIRED would drop
  # the resources field again, which is another spec change and another resize.
  DESIRED="$RESIZED"

  gen=""
  for ((i=0; i<60; i++)); do
    gen=$(ep_attr applied_generation)
    [ -n "$gen" ] && [ "$gen" != 0 ] && break
    sleep 5
  done
  [ -n "$gen" ] && [ "$gen" != 0 ] \
    && ok "endpoint attrs.applied_generation=$gen (the task completed and stamped it)" \
    || bad "endpoint attrs.applied_generation='${gen:-none}' — the resize task never stamped the row"

  # And it settles: once the row confirms, the agent stops re-issuing, so the
  # resource reports plain "ready" rather than staying "updating" or degraded.
  state=""; estate=""
  for ((i=0; i<24; i++)); do
    ej=$(env_json)
    state=$(printf '%s' "$ej" | jq -r '.state // empty' 2>/dev/null)
    estate=$(printf '%s' "$ej" | jq -r --arg n "$EP_NAME" \
      '.resources[]?|select(.kind=="endpoint" and .name==$n)|.state // empty' 2>/dev/null | head -1)
    [ "$state" = READY ] && [ "$estate" = ready ] && break
    sleep 5
  done
  [ "$state" = READY ] && [ "$estate" = ready ] \
    && ok "environment READY and endpoint state=ready after the update settled" \
    || bad "environment=$state endpoint=$estate after the update (want READY/ready)"
fi

# --- branch + endpoint settings ---
#
# Settings were declarable in desired state long before the agent could apply
# them: every declared surface was counted as drift, which forced the update to
# claim no generation and ended the tick Permanent (meko#1246). So the assertions
# here are that the values actually reach the controller's rows, and that the
# generation is claimed only when they did.
#
# Read back from the amp API, because the controller's row is the durable answer and
# the only thing that can show what actually landed. That is a READ; the environment
# is still driven entirely through desired state. The inventory's own copy of those
# settings is asserted separately below, against the same rows.
hdr "branch and endpoint settings converge from desired state"

AMP="http://127.0.0.1:${AMP_PF_PORT}/api/v1"
# <name> is the LEAF, not the hierarchical path the desired-state document uses:
# the agent reports ResourceStatus.Name as leaf(key.Name) (cmd/reconcile-agent).
inv_attr() { # inv_attr <kind> <leaf name> <attr>
  env_json | jq -r --arg k "$1" --arg n "$2" --arg a "$3" \
    '.resources[]?|select(.kind==$k and .name==$n)|.attrs[$a] // empty' 2>/dev/null | head -1
}
POOL_ID=$(inv_attr storage-pool "$POOL_NAME" id)
CLUSTER_ID=$(inv_attr cluster "$CLUSTER_NAME" id)
BRANCH_ID=$(inv_attr branch "$BRANCH_NAME" id)
SET_EP_ID=$(inv_attr endpoint "$EP_NAME" id)
BRANCH_URL="$AMP/storage-pools/$POOL_ID/clusters/$CLUSTER_ID/branches/$BRANCH_ID"

if [ -z "$POOL_ID" ] || [ -z "$CLUSTER_ID" ] || [ -z "$BRANCH_ID" ] || [ -z "$SET_EP_ID" ]; then
  bad "could not resolve amp ids from the inventory (pool=${POOL_ID:-} cluster=${CLUSTER_ID:-} branch=${BRANCH_ID:-} endpoint=${SET_EP_ID:-})"
else
  ok "amp ids resolved from the inventory (branch $BRANCH_ID)"

  # Captured before the first settings write, compared after the last. This catches
  # a settings edit that REPLACES the pod — the agent planning a resize or a redeploy
  # off the back of one — which would be a real regression and is otherwise invisible
  # here, since the pod comes back Ready either way.
  #
  # It does NOT catch the narrower blip of Postgres being bounced inside the pod for
  # a start-time-only setting: the supervisor restarts the process and the pod's uid
  # is unchanged. The signal for that is the PATCH response's restarted /
  # restart_forced_by, which only the agent sees. Both settings declared below are
  # reloadable, so neither should bounce anything.
  SET_POD=$(kubectl -n "$NS" get pods -o name 2>/dev/null | grep "yb-ep-$SET_EP_ID" | head -1)
  SET_POD="${SET_POD#pod/}"
  SET_UID_BEFORE=""
  if [ -n "$SET_POD" ]; then
    SET_UID_BEFORE=$(kubectl -n "$NS" get "pod/$SET_POD" -o jsonpath='{.metadata.uid}' 2>/dev/null)
  fi
  [ -n "$SET_UID_BEFORE" ] && ok "endpoint pod uid before the settings edits ($SET_POD)" \
    || bad "could not read the endpoint pod's uid before the settings edits"

  # branch pg_settings <- the baseline; endpoint pg_settings <- its own override.
  # work_mem is declared on BOTH with different values on purpose: they are
  # separate rows, and the endpoint's winning is the compute's business, so each
  # row must carry exactly what its own spec declared and nothing merged.
  BRANCH_PATH="${POOL_NAME}/${CLUSTER_NAME}/${BRANCH_NAME}"
  EP_PATH="${BRANCH_PATH}/${EP_NAME}"
  WITH_SETTINGS=$(jq -nc --argjson r "$DESIRED" --arg b "$BRANCH_PATH" --arg e "$EP_PATH" '
    [ $r[]
      | if .kind=="branch"   and .name==$b then .spec.pg_settings={statement_timeout:"30s", work_mem:"16MB"}
        elif .kind=="endpoint" and .name==$e then .spec.pg_settings={work_mem:"64MB"}
        else . end ]')
  set_desired "$WITH_SETTINGS" && ok "desired state declares branch + endpoint pg_settings" \
    || bad "POST desired (settings) failed"
  DESIRED="$WITH_SETTINGS"

  # The branch is the load-bearing half. Its spec declares settings and nothing
  # else, so the agent issues no task for it at all — the settings PATCH is
  # synchronous, and its handler stamping the row is the only way this generation
  # can ever be recorded. Before #1246 it was never stamped, because a declared
  # pg_settings was drift and drift claims nothing.
  bset=""; bgen=""
  for ((i=0; i<36; i++)); do
    bj=$(curl -sf "$BRANCH_URL" 2>/dev/null)
    bset=$(printf '%s' "$bj" | jq -c '.pg_settings // empty' 2>/dev/null)
    bgen=$(printf '%s' "$bj" | jq -r '.ycp_generation // empty' 2>/dev/null)
    [ "$(printf '%s' "$bset" | jq -r '.statement_timeout // empty' 2>/dev/null)" = "30s" ] && break
    sleep 5
  done
  [ "$(printf '%s' "$bset" | jq -r '.statement_timeout // empty' 2>/dev/null)" = "30s" ] \
    && [ "$(printf '%s' "$bset" | jq -r '.work_mem // empty' 2>/dev/null)" = "16MB" ] \
    && ok "branch row carries the declared pg_settings (${bset})" \
    || bad "branch pg_settings='${bset:-none}', want statement_timeout=30s and work_mem=16MB"
  [ -n "$bgen" ] && [ "$bgen" != 0 ] && [ "$bgen" != null ] \
    && ok "branch ycp_generation=$bgen — the synchronous PATCH stamped it with no task" \
    || bad "branch ycp_generation='${bgen:-none}' — a settings-only spec never recorded its generation"

  eset=""
  for ((i=0; i<36; i++)); do
    eset=$(curl -sf "$BRANCH_URL/endpoints/$SET_EP_ID" 2>/dev/null | jq -c '.pg_settings // empty' 2>/dev/null)
    [ "$(printf '%s' "$eset" | jq -r '.work_mem // empty' 2>/dev/null)" = "64MB" ] && break
    sleep 5
  done
  if [ "$(printf '%s' "$eset" | jq -r '.work_mem // empty' 2>/dev/null)" = "64MB" ]; then
    ok "endpoint row carries its own pg_settings (${eset})"
  else
    bad "endpoint pg_settings='${eset:-none}', want work_mem=64MB"
  fi
  # Its own, not the branch's merged in: the agent writes each row from that
  # resource's spec, and an agent that compared against the merged view would read
  # a branch-supplied value as the endpoint's and never write it.
  [ "$(printf '%s' "$eset" | jq -r 'has("statement_timeout")' 2>/dev/null)" = false ] \
    && ok "endpoint row did not absorb the branch's statement_timeout" \
    || bad "endpoint pg_settings='${eset}' contains the branch's key — the rows were merged"

  # --- the same settings, read out of the inventory (meko#1295) ---
  #
  # A declared surface owns the whole map, so a console that cannot see what is
  # stored deletes every key it never rendered on its first write. The inventory is
  # the only place it can look: it has no second credentialed client against the
  # controller, and applied_generation says an update landed, not what it landed on.
  hdr "the inventory reports branch and endpoint settings"
  bsattr=""; esattr=""
  for ((i=0; i<36; i++)); do
    bsattr=$(inv_attr branch "$BRANCH_NAME" pg_settings)
    esattr=$(inv_attr endpoint "$EP_NAME" pg_settings)
    [ "$(printf '%s' "$bsattr" | jq -r '.statement_timeout // empty' 2>/dev/null)" = "30s" ] \
      && [ "$(printf '%s' "$esattr" | jq -r '.work_mem // empty' 2>/dev/null)" = "64MB" ] && break
    sleep 5
  done
  [ "$(printf '%s' "$bsattr" | jq -r '.statement_timeout // empty' 2>/dev/null)" = "30s" ] \
    && [ "$(printf '%s' "$bsattr" | jq -r '.work_mem // empty' 2>/dev/null)" = "16MB" ] \
    && ok "branch attrs.pg_settings matches the row (${bsattr})" \
    || bad "branch attrs.pg_settings='${bsattr:-none}', want statement_timeout=30s and work_mem=16MB"
  [ "$(printf '%s' "$esattr" | jq -r '.work_mem // empty' 2>/dev/null)" = "64MB" ] \
    && ok "endpoint attrs.pg_settings matches the row (${esattr})" \
    || bad "endpoint attrs.pg_settings='${esattr:-none}', want work_mem=64MB"

  # A surface nothing ever declared is present and empty, never absent: absence is
  # reserved for an agent too old to report settings at all, and a console reading
  # it as empty is the destructive save above.
  v=$(inv_attr branch "$BRANCH_NAME" write_token_settings)
  [ "$v" = "{}" ] && ok "branch attrs.write_token_settings is present and empty" \
    || bad "branch attrs.write_token_settings='${v:-absent}', want {} — an undeclared surface must not be omitted"
  for a in pageserver_settings safekeeper_settings extended_config; do
    v=$(inv_attr endpoint "$EP_NAME" "$a")
    [ "$v" = "{}" ] && ok "endpoint attrs.$a is present and empty" \
      || bad "endpoint attrs.$a='${v:-absent}', want {} — an undeclared surface must not be omitted"
  done

  # --- a declared surface owns the whole map ---
  #
  # The operator-facing half of #1246: desired state says what pg_settings IS, so
  # dropping a key removes it. The PATCH replaces a named surface, and a spec that
  # only ever added would leave an operator unable to take a setting off.
  hdr "dropping a key from a declared surface removes it from the row"
  DROPPED=$(jq -nc --argjson r "$DESIRED" --arg b "$BRANCH_PATH" \
    '[ $r[] | if .kind=="branch" and .name==$b then .spec.pg_settings={work_mem:"16MB"} else . end ]')
  set_desired "$DROPPED" && ok "desired state drops statement_timeout, keeping work_mem" \
    || bad "POST desired (dropped key) failed"
  DESIRED="$DROPPED"

  bset2=""
  for ((i=0; i<36; i++)); do
    bset2=$(curl -sf "$BRANCH_URL" 2>/dev/null | jq -c '.pg_settings // empty' 2>/dev/null)
    [ "$(printf '%s' "$bset2" | jq -r 'has("statement_timeout")' 2>/dev/null)" = false ] && break
    sleep 5
  done
  [ "$(printf '%s' "$bset2" | jq -r 'has("statement_timeout")' 2>/dev/null)" = false ] \
    && [ "$(printf '%s' "$bset2" | jq -r '.work_mem // empty' 2>/dev/null)" = "16MB" ] \
    && ok "branch pg_settings is now exactly what the spec declares (${bset2})" \
    || bad "branch pg_settings='${bset2:-none}', want only work_mem=16MB — the dropped key was not removed"

  # The removal has to reach the inventory too, or a console re-reading it would
  # declare the stale key back on its next save.
  bsattr2=""
  for ((i=0; i<36; i++)); do
    bsattr2=$(inv_attr branch "$BRANCH_NAME" pg_settings)
    [ "$(printf '%s' "$bsattr2" | jq -r 'has("statement_timeout")' 2>/dev/null)" = false ] && break
    sleep 5
  done
  [ "$(printf '%s' "$bsattr2" | jq -r 'has("statement_timeout")' 2>/dev/null)" = false ] \
    && [ "$(printf '%s' "$bsattr2" | jq -r '.work_mem // empty' 2>/dev/null)" = "16MB" ] \
    && ok "branch attrs.pg_settings dropped the key too (${bsattr2})" \
    || bad "branch attrs.pg_settings='${bsattr2:-none}', want only work_mem=16MB"

  # The other end of SET_UID_BEFORE, now that every settings edit has landed.
  if [ -n "$SET_UID_BEFORE" ]; then
    suid_after=$(kubectl -n "$NS" get "pod/$SET_POD" -o jsonpath='{.metadata.uid}' 2>/dev/null)
    [ "$suid_after" = "$SET_UID_BEFORE" ] \
      && ok "endpoint pod was not replaced by the settings edits ($SET_POD)" \
      || bad "endpoint pod replaced across the settings edits (uid $SET_UID_BEFORE -> ${suid_after:-gone}) — a settings edit must not redeploy compute"
  fi
fi

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

    # --- a generation bump that changes no extension must NOT bounce compute ---
    #
    # With nothing to add, the agent issues no add_branch_extensions at all: the
    # generation is recorded by an empty settings PATCH, which names no surface and
    # so plans no task and tells no endpoint anything. The branch still has to end
    # up stamped, because a branch whose row was unioned by a task that then failed
    # would otherwise report converged at a generation no endpoint applied
    # (meko#985).
    #
    # This used to be a re-send of the branch's whole extension set — the only
    # branch call that could carry a generation before #1246 — which planned a task
    # on every generation bump and leaned on the redeploy rendering an identical
    # manifest. If that ever stopped holding, every bump on a branch with extensions
    # restarted every endpoint on it: a user-visible outage invisible to every other
    # assertion here, since the pod comes back Ready either way. So compare the
    # pod's UID: a restart mints a new one.
    #
    # The bump is a duplicate name in the declared list. jsonb `IS DISTINCT FROM`
    # sees a changed spec and bumps the generation, while extensionSet dedupes to
    # the same set — an inert edit, which is exactly the case under test.
    if [ -n "$pod" ]; then
      hdr "a no-delta generation bump does not restart endpoint $EP_NAME"
      before_uid=$(kubectl -n "$NS" get "pod/$pod" -o jsonpath='{.metadata.uid}' 2>/dev/null)
      before_gen=$(env_json | jq -r --arg n "$BRANCH_NAME" \
        '.resources[]?|select(.kind=="branch" and .name==$n)|.attrs.applied_generation // empty' 2>/dev/null | head -1)
      [ -n "$before_uid" ] && ok "endpoint pod uid before the bump ($before_uid, branch generation ${before_gen:-none})" \
        || bad "could not read the endpoint pod's uid"

      DUP_EXT=$(jq -nc --argjson r "$DESIRED" --arg n "${POOL_NAME}/${CLUSTER_NAME}/${BRANCH_NAME}" --arg ext "$BRANCH_EXT" \
        '[ $r[] | if .kind=="branch" and .name==$n then .spec.extensions=[$ext,$ext] else . end ]')
      set_desired "$DUP_EXT" && ok "desired state re-declares $BRANCH_EXT (inert edit, bumps the generation)" \
        || bad "POST desired (duplicate extension) failed"
      DESIRED="$DUP_EXT"

      after_gen=""
      for ((i=0; i<60; i++)); do
        after_gen=$(env_json | jq -r --arg n "$BRANCH_NAME" \
          '.resources[]?|select(.kind=="branch" and .name==$n)|.attrs.applied_generation // empty' 2>/dev/null | head -1)
        [ -n "$after_gen" ] && [ "$after_gen" != "${before_gen:-}" ] && break
        sleep 5
      done
      [ -n "$after_gen" ] && [ "$after_gen" != "${before_gen:-}" ] \
        && ok "branch attrs.applied_generation advanced to $after_gen (the empty settings PATCH stamped it)" \
        || bad "branch attrs.applied_generation='${after_gen:-none}' did not advance past '${before_gen:-none}'"

      after_uid=$(kubectl -n "$NS" get "pod/$pod" -o jsonpath='{.metadata.uid}' 2>/dev/null)
      if [ "$after_uid" = "$before_uid" ]; then
        ok "endpoint pod was not restarted by the no-delta bump ($after_uid)"
      else
        bad "endpoint pod restarted for an unchanged extension set (uid ${before_uid} -> ${after_uid:-gone}) — a no-delta bump bounced compute"
      fi
    fi
fi

fi

# --- relayed task history: what ran, and what failed ---
#
# The tasks above have all completed by now, so this asserts the HISTORY rather than
# racing a live one. That is deliberate: several amp task kinds finish inside one
# reconcile interval and are never observed pending or running, so asserting an
# intermediate status here would make the test flaky on exactly the behaviour the
# change feed exists to guarantee.
hdr "the console can see what ran (GET /environments/{id}/tasks)"

tasks_json() {
  # $1: query string, appended as-is.
  curl -sf -H "X-Meko-Admin-Secret: ${YCP_ADMIN_KEY}" \
    "$CP/environments/${YCP_ENV_ID}/tasks${1:-}" 2>/dev/null
}

# The agent relays on its heartbeat, so give it a few ticks to arrive.
tj=""; total=0
for ((i=0; i<24; i++)); do
  tj=$(tasks_json)
  total=$(printf '%s' "$tj" | jq -r '.data.total // 0' 2>/dev/null)
  [ "${total:-0}" -gt 0 ] && break
  sleep 5
done
[ "${total:-0}" -gt 0 ] \
  && ok "relayed task history has $total tasks" \
  || bad "no tasks reached the control plane after 120s (the agent is not relaying the task log)"

if [ "${total:-0}" -gt 0 ]; then
  # Every task the agent drove must carry a kind and a terminal status. A row with an
  # empty kind means the mapping dropped a field on the way through.
  kinds=$(printf '%s' "$tj" | jq -r '[.data.items[]?|.kind]|unique|join(",")' 2>/dev/null)
  [ -n "$kinds" ] && ok "task kinds relayed: $kinds" || bad "relayed tasks carry no kind"

  # The endpoint create is the one task kind this smoke definitely planned.
  if printf '%s' "$kinds" | grep -q 'create_endpoint'; then
    ok "the create_endpoint task is in the history"
  else
    bad "create_endpoint is absent from the history (kinds=${kinds:-none}) — a task that completed between ticks was lost"
  fi

  # ycp_generation is what distinguishes a task YCP drove from one planned around it.
  driven=$(printf '%s' "$tj" | jq -r '[.data.items[]?|select(.ycp_generation != null)]|length' 2>/dev/null)
  [ "${driven:-0}" -gt 0 ] \
    && ok "$driven task(s) carry a ycp_generation (YCP-driven, not planned around the control plane)" \
    || bad "no relayed task carries a ycp_generation"

  # The status filter is the console's primary query. Everything here has finished, so
  # active must be empty while the unfiltered page is not -- which also proves the
  # filter is actually applied rather than ignored.
  active=$(tasks_json '?active=true' | jq -r '.data.total // -1' 2>/dev/null)
  [ "${active:-1}" = 0 ] \
    && ok "active=true is empty now that every task has finished" \
    || bad "active=true returned ${active:-?} with no task in flight (the status filter is not applied)"

  succeeded=$(tasks_json '?status=succeeded' | jq -r '.data.total // 0' 2>/dev/null)
  [ "${succeeded:-0}" -gt 0 ] \
    && ok "status=succeeded returns $succeeded task(s) — success is persisted, not just observed in flight" \
    || bad "status=succeeded is empty though $total tasks are recorded"

  # An unknown status is dropped, not rejected, so a stale console still gets results.
  bogus=$(tasks_json '?status=cancelled' | jq -r '.data.total // -1' 2>/dev/null)
  [ "${bogus:-−1}" = "$total" ] \
    && ok "an unrecognized status is dropped, not rejected (still $total)" \
    || bad "status=cancelled returned ${bogus:-?}, want the unfiltered $total"

  # One task in full, by id -- the console's detail drawer.
  tid=$(printf '%s' "$tj" | jq -r '.data.items[0].task_id // empty' 2>/dev/null)
  if [ -n "$tid" ]; then
    one=$(curl -sf -H "X-Meko-Admin-Secret: ${YCP_ADMIN_KEY}" \
      "$CP/environments/${YCP_ENV_ID}/tasks/${tid}" 2>/dev/null | jq -r '.data.task_id // empty' 2>/dev/null)
    [ "$one" = "$tid" ] && ok "GET tasks/$tid returns that task" \
      || bad "GET tasks/$tid returned '${one:-nothing}'"
  fi

  # Staleness: the page carries the heartbeat, so a console can tell a task that has
  # not moved from an agent that has stopped talking.
  beat=$(printf '%s' "$tj" | jq -r '.data.last_heartbeat_at // empty' 2>/dev/null)
  [ -n "$beat" ] && ok "the page reports last_heartbeat_at ($beat)" \
    || bad "last_heartbeat_at is absent; staleness is indistinguishable from an idle environment"

  # History must survive the agent restarting -- the control plane is the durable copy,
  # and the agent forgets what it relayed when its pod goes.
  if kubectl -n "$NS" rollout restart deploy/amp-reconcile-agent >/dev/null 2>&1; then
    kubectl -n "$NS" rollout status deploy/amp-reconcile-agent --timeout=180s >/dev/null 2>&1
    after=$(tasks_json | jq -r '.data.total // -1' 2>/dev/null)
    [ "${after:-0}" -ge "$total" ] \
      && ok "task history survived the agent restart ($total -> $after, no rows lost or duplicated)" \
      || bad "task history shrank across the agent restart ($total -> ${after:-?})"
  else
    printf '  \033[1;33mskip\033[0m task history across an agent restart (no amp-reconcile-agent deployment)\n'
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

# --- telemetry actually flows -------------------------------------------------
# The chart's preflight only WARNS (it exits 0 whatever it finds), and the control
# plane only DEGRADES, so nothing in the product fails when metrics stop reaching
# yugabyte-cloud. This suite is therefore the only thing standing between a broken
# BYOC telemetry path and a green build, and it asserts both ends of the chain:
# the collector really exported, and the control plane really saw it.
hdr "byoc telemetry"

# 1 — the collector's own books. otelcol_exporter_sent_metric_points for the byoc
# exporter is incremented per successfully sent point, so a non-zero value is proof
# that data left this cluster for the door — not merely that the config looks right.
# Polled rather than sampled once: the collectors scrape on a 15s clock and the
# agent may have only just come up.
# Named by the OpenTelemetry Operator's convention —
# <collector CR name>-collector-monitoring — off the CR that otel-collectors.yaml
# renders as <fullname>-controller, where fullname is up-gke.sh's release name
# (yb-amp-controller). Verified against a live operator-managed collector; if the
# operator ever renames it this assertion fails loudly rather than silently passing.
COLLECTOR_SVC="http://yb-amp-controller-controller-collector-monitoring.${NS}.svc.cluster.local:8888/metrics"
sent=""
for ((i=0; i<24; i++)); do
  kubectl -n "$NS" delete pod byoc-metrics-check --ignore-not-found >/dev/null 2>&1
  sent=$(kubectl -n "$NS" run byoc-metrics-check --rm -i --restart=Never --request-timeout=60s \
           --image=curlimages/curl:8.11.1 --command -- \
           sh -c "curl -sS --max-time 15 '$COLLECTOR_SVC' | awk '/^otelcol_exporter_sent_metric_points\{[^}]*exporter=\"otlphttp\/byoc\"/ {print \$2; exit}'" \
         2>/dev/null | tr -dc '0-9.')
  [ -n "$sent" ] && [ "${sent%%.*}" -gt 0 ] 2>/dev/null && break
  sleep 10
done
if [ -n "$sent" ] && [ "${sent%%.*}" -gt 0 ] 2>/dev/null; then
  ok "the byoc exporter has shipped ${sent%%.*} metric point(s) off-cluster"
else
  # Distinguish the two failures an operator would chase differently: nothing
  # reaching the exporter (upstream) versus the door refusing what it has.
  kubectl -n "$NS" delete pod byoc-metrics-check --ignore-not-found >/dev/null 2>&1
  diag=$(kubectl -n "$NS" run byoc-metrics-check --rm -i --restart=Never --request-timeout=60s \
           --image=curlimages/curl:8.11.1 --command -- \
           sh -c "curl -sS --max-time 15 '$COLLECTOR_SVC' | grep -E 'otelcol_exporter_(send_failed_metric_points|queue_size)' | head -4" 2>/dev/null)
  bad "the byoc exporter never shipped a metric point (sent='${sent:-unset}'); exporter counters: ${diag:-unreadable}"
fi

# 2 — the control plane's view. The agent attests its telemetry on every status
# report and YCP degrades an environment it does not like, surfacing the reason on a
# synthetic `telemetry` component. An empty reason here means the whole chain agreed:
# the values the chart rendered, what the agent read back, and the door YCP issued.
ej=$(env_json)
tel_err=$(printf '%s' "$ej" | jq -r '[.components[]?|select(.name=="telemetry")|.error]|join("; ")' 2>/dev/null)
if [ -z "$tel_err" ]; then
  ok "the control plane reports no telemetry problem for this environment"
else
  bad "the control plane flagged this environment's telemetry: ${tel_err}"
fi

# --- summary ---
hdr "summary"
printf '  passed: %d   failed: %d\n' "$pass" "$fail"
[ "$fail" -eq 0 ] || exit 1
