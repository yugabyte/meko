#!/usr/bin/env bash
#
# Drive a real controller upgrade from YCP desired state and prove it does not
# take the environment's endpoints with it.
#
# The `controller` kind moves three image tags at once (controller, agent, and
# the endpoint PIN), and it does it by helm-upgrading the release the agent is
# itself a member of — so the agent's own pod is restarted by the command it
# issued. The risk that buys is not "does helm work": it is that a restart in the
# middle of a release makes the agent lose track of what it was doing and either
# re-issue forever or garbage-collect the endpoints it can no longer see. This
# asserts the opposite, end to end and entirely through YCP:
#
#   1. desired state declares pool -> clusters -> endpoints, and the
#      controller at CTL_V1. The controller row must report ready at V1 with no
#      upgrade issued — a steady state that churns is a broken one.
#   2. desired state moves the controller to CTL_V2. The row must reach ready at
#      V2, and the release's helm revision must have advanced exactly once.
#   3. SETTLE_SECONDS (5 minutes by default) pass. The SAME endpoints — same amp
#      ids, same Kubernetes Deployment UIDs, so neither recreated nor collected —
#      must still be ready.
#   4. two more endpoints are declared. They must converge, and they must run the
#      V2 endpoint image: that is the proof the upgrade moved the pin rather than
#      only the controller.
#
#   5. desired state declares the fleet-version at CTL_V2. Every endpoint — the
#      two left on V1 and the two created on V2 — must end up on the V2 image, and
#      the fleet row must report ready.
#   6. with CTL_V3 set, desired state moves the controller AND the fleet to CTL_V3
#      in one write: the agent must upgrade the controller first and then roll.
#
# Endpoints already running stay on V1 through step 4 on purpose: the controller
# upgrade rolls nothing, and moving them is the fleet-version kind's job.
#
# The fleet is one CLUSTER per endpoint, each endpoint alone on that cluster's own
# default branch. Two things force that shape:
#
#   - two writers on one branch contend for the write token and neither starts, so
#     each read_write endpoint needs a branch to itself;
#   - a branch that is not a cluster's default has to be forked, and a fork needs
#     a fork point, which a branch with no endpoint on it cannot supply ("cannot
#     determine a fork point for the parent branch"). A desired state declaring
#     the whole fleet at once has no way to order that, and the create is refused
#     rather than retried.
#
# A cluster's default branch is materialized by the cluster create, so this needs
# no fork and no ordering. It must still be DECLARED: the agent is authoritative
# and collects what desired state does not name, and deleting a cluster's default
# branch wedges the cluster permanently — one with no live default branch is
# invisible to the list the agent reconciles against, so every tick re-issues
# create_cluster and the name conflicts with the row already there.
#
# Endpoint leaf names are unique across the fleet because the inventory reports a
# resource by its LEAF; endpoints sharing one would be indistinguishable in
# exactly the assertions this test turns on.
#
# Preconditions:
#   - amp/deploy/ycp-up.sh stood up the control plane in $NS and registered an
#     amp environment, emitting YCP_ENV_ID + YCP_ADMIN_KEY;
#   - amp/deploy/up-gke.sh deployed the stack at TAG=$CTL_V1 with YCP_ENABLED=1
#     and YCP_CONTROLLER_CHART pointing at a chart the agent can pull
#     (ycp/test/local/controller-chart-publish.sh publishes one);
#   - $CTL_V2 (and $CTL_V3, if set) exists in the registry for the controller,
#     endpoint and agent repos.
#
# Prereqs: kubectl (pointed at the cluster), curl, jq, helm.
#
# Usage:
#   NS=shubin-ctl-up YCP_ENV_ID=amp-... YCP_ADMIN_KEY=... \
#     CTL_V1=ctlup1 CTL_V2=ctlup2 amp/deploy/ycp-controller-upgrade-test.sh
set -uo pipefail

NS="${NS:?NS (namespace) is required}"
YCP_ENV_ID="${YCP_ENV_ID:?YCP_ENV_ID is required}"
YCP_ADMIN_KEY="${YCP_ADMIN_KEY:?YCP_ADMIN_KEY is required}"
CTL_V1="${CTL_V1:?CTL_V1 (the image tag deployed now) is required}"
CTL_V2="${CTL_V2:?CTL_V2 (the image tag to upgrade to) is required}"
CTL_V3="${CTL_V3:-}"
FLEET_V="" # when set, desired_json also declares the fleet-version at it

RELEASE="${RELEASE:-yb-amp-controller}"
YCP_PF_PORT="${YCP_PF_PORT:-18094}"
SUFFIX="${SUFFIX:-$RANDOM}"
SETTLE_SECONDS="${SETTLE_SECONDS:-300}"
EP_PASSWORD="${EP_PASSWORD:-ctlup-pw-1}"

POOL_NAME="ctlup-pool-$SUFFIX"
FIRST=(ep1 ep2) # endpoint epN sits on cluster cN's default branch
LATER=(ep3 ep4)

pass=0
fail=0
hdr() { printf '\n\033[1;36m== %s\033[0m\n' "$*"; }
ok() {
  printf '  \033[1;32mok\033[0m   %s\n' "$*"
  pass=$((pass + 1))
}
bad() {
  printf '  \033[1;31mBAD\033[0m  %s\n' "$*"
  fail=$((fail + 1))
}
note() { printf '       %s\n' "$*"; }

PF_PID=""
cleanup() { [ -n "$PF_PID" ] && kill "$PF_PID" 2>/dev/null; }
trap cleanup EXIT

hdr "port-forward control plane"
kubectl -n "$NS" port-forward svc/meko-api-server "${YCP_PF_PORT}:80" >/tmp/ctlup-pf.log 2>&1 &
PF_PID=$!
CP="http://127.0.0.1:${YCP_PF_PORT}/api/v1"
for ((i = 0; i < 30; i++)); do
  curl -sf "$CP/health" >/dev/null 2>&1 && break
  sleep 1
done
curl -sf "$CP/health" >/dev/null 2>&1 && ok "control plane reachable" || {
  bad "control plane unreachable"
  exit 1
}

env_json() {
  curl -sf -H "X-Meko-Admin-Secret: ${YCP_ADMIN_KEY}" "$CP/environments" |
    jq -c --arg id "$YCP_ENV_ID" '.data.items[]|select(.id==$id)' 2>/dev/null
}
set_desired() {
  curl -sf -X POST "$CP/environments/${YCP_ENV_ID}/desired" \
    -H "X-Meko-Admin-Secret: ${YCP_ADMIN_KEY}" -H 'Content-Type: application/json' \
    -d "$(jq -nc --argjson r "$1" '{resources:$r}')" >/dev/null
}
# The agent reports ResourceStatus.Name as the LEAF of the path, not the path.
res_attr() { env_json | jq -r --arg k "$1" --arg n "$2" --arg a "$3" \
  '.resources[]?|select(.kind==$k and .name==$n)|.attrs[$a] // empty' 2>/dev/null | head -1; }
res_state() { env_json | jq -r --arg k "$1" --arg n "$2" \
  '.resources[]?|select(.kind==$k and .name==$n)|.state // empty' 2>/dev/null | head -1; }

helm_revision() { helm -n "$NS" status "$RELEASE" -o json 2>/dev/null | jq -r '.version // empty'; }
deploy_tag() { kubectl -n "$NS" get deploy "$1" \
  -o jsonpath='{.spec.template.spec.containers[0].image}' 2>/dev/null | sed 's/.*://'; }
ep_deploy() { kubectl -n "$NS" get deploy "yb-ep-$1" -o jsonpath="{$2}" 2>/dev/null; }

# desired_json <controller version> <endpoint leaf...> — endpoint N gets cluster
# cN and that cluster's default branch, both declared so neither is collected.
desired_json() {
  local version="$1"
  shift
  local tree='[]' n=0
  for e in "$@"; do
    n=$((n + 1))
    tree=$(jq -nc --argjson a "$tree" --arg pool "$POOL_NAME" --arg cl "c${n}" \
      --arg ep "$e" --arg pw "$EP_PASSWORD" '
      $a + [ {kind:"cluster",  name:($pool+"/"+$cl),               spec:{}},
             {kind:"branch",   name:($pool+"/"+$cl+"/main"),       spec:{}},
             {kind:"endpoint", name:($pool+"/"+$cl+"/main/"+$ep),
               spec:{role:"read_write", password:$pw}} ]')
  done
  jq -nc --arg pool "$POOL_NAME" --arg uid "$UID_" --arg v "$version" --arg fv "$FLEET_V" --argjson tree "$tree" '
    [ {kind:"storage-pool", name:$pool,        spec:{owner_user_id:$uid}},
      {kind:"controller",   name:"controller", spec:{version:$v}} ]
    + (if $fv == "" then [] else [{kind:"fleet-version", name:"fleet", spec:{version:$fv}}] end)
    + $tree'
}

# wait_ready <timeout secs> <kind> <leaf...>
wait_ready() {
  local budget="$1" kind="$2"
  shift 2
  local deadline=$((SECONDS + budget)) all e
  while [ "$SECONDS" -lt "$deadline" ]; do
    all=1
    for e in "$@"; do [ "$(res_state "$kind" "$e")" = ready ] || all=0; done
    [ "$all" = 1 ] && return 0
    sleep 5
  done
  return 1
}
dump_inventory() { env_json | jq -c '.resources[]?|{kind,name,state}' 2>/dev/null | sed 's/^/       /'; }

hdr "preconditions"
UID_=$(cat /proc/sys/kernel/random/uuid)
[ "$(deploy_tag amp-controller)" = "$CTL_V1" ] && ok "controller Deployment is at ${CTL_V1}" ||
  bad "controller Deployment is at '$(deploy_tag amp-controller)', want ${CTL_V1} — deploy at CTL_V1 first"
[ "$(deploy_tag amp-reconcile-agent)" = "$CTL_V1" ] && ok "agent Deployment is at ${CTL_V1}" ||
  bad "agent Deployment is at '$(deploy_tag amp-reconcile-agent)', want ${CTL_V1}"
CHART_REF=$(kubectl -n "$NS" get deploy amp-reconcile-agent \
  -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="CONTROLLER_CHART")].value}' 2>/dev/null)
note "chart: ${CHART_REF:-<the agent default, oci://quay.io/yugabyte/charts/yb-amp-controller>}"
REV0=$(helm_revision)
note "helm revision now: ${REV0:-?}"

# --- 1. the tree plus the controller at the version already deployed ---
hdr "desired state: pool -> 2 clusters -> 2 endpoints, controller@${CTL_V1}"
set_desired "$(desired_json "$CTL_V1" "${FIRST[@]}")" &&
  ok "desired state accepted (controller is a kind the control plane allows)" ||
  {
    bad "POST desired failed — the control plane rejected the document"
    exit 1
  }

hdr "the tree converges"
if wait_ready 900 endpoint "${FIRST[@]}"; then
  ok "both endpoints report ready"
else
  bad "endpoints did not converge in 15m"
  dump_inventory
fi

# Declaring the version the release is already at must issue NOTHING. A kind that
# cannot recognise its own steady state upgrades on every tick, which on this one
# restarts the agent that is doing the upgrading.
hdr "controller@${CTL_V1} is a steady state, not an upgrade"
if wait_ready 300 controller controller; then
  ok "controller row reports ready"
else bad "controller row is '$(res_state controller controller)' (attrs: $(env_json | jq -c '.resources[]?|select(.kind=="controller")|.attrs' 2>/dev/null))"; fi
[ "$(res_attr controller controller image_tag)" = "$CTL_V1" ] &&
  ok "controller row reports image_tag=${CTL_V1}" ||
  bad "controller row reports image_tag='$(res_attr controller controller image_tag)'"
REV1=$(helm_revision)
[ "$REV1" = "$REV0" ] && ok "helm revision unchanged (${REV1}) — no upgrade was issued" ||
  bad "helm revision moved ${REV0} -> ${REV1}; the kind re-upgraded a release already at the declared version"

# --- 2. record what must survive ---
hdr "record the endpoints that must survive the upgrade"
EP_IDS=()
EP_UIDS=()
EP_IMAGES=()
for e in "${FIRST[@]}"; do
  id=$(res_attr endpoint "$e" id)
  [ -n "$id" ] || {
    bad "no endpoint id for $e"
    continue
  }
  EP_IDS+=("$id")
  EP_UIDS+=("$(ep_deploy "$id" .metadata.uid)")
  EP_IMAGES+=("$(ep_deploy "$id" '.spec.template.spec.containers[0].image')")
  note "$e = ${id}  deploy-uid=${EP_UIDS[-1]:-none}  image=${EP_IMAGES[-1]:-none}"
done
[ "${#EP_IDS[@]}" = 2 ] && ok "2 endpoints recorded" || bad "recorded ${#EP_IDS[@]} endpoints, want 2"

# --- 3. the upgrade ---
hdr "desired state: controller@${CTL_V2}"
set_desired "$(desired_json "$CTL_V2" "${FIRST[@]}")" && ok "upgrade declared" ||
  bad "POST desired (upgrade) failed"

hdr "the upgrade converges (the agent restarts itself doing this)"
UP_START=$SECONDS
converged=0
for ((i = 0; i < 180; i++)); do # 15m
  if [ "$(res_state controller controller)" = ready ] &&
    [ "$(res_attr controller controller image_tag)" = "$CTL_V2" ]; then
    converged=1
    break
  fi
  sleep 5
done
if [ "$converged" = 1 ]; then
  ok "controller row reports ready at ${CTL_V2} after $((SECONDS - UP_START))s"
else
  bad "controller did not reach ready@${CTL_V2} in 15m (state='$(res_state controller controller)', image_tag='$(res_attr controller controller image_tag)')"
  kubectl -n "$NS" logs deploy/amp-reconcile-agent --tail=40 2>/dev/null | sed 's/^/       /'
fi
[ "$(deploy_tag amp-controller)" = "$CTL_V2" ] && ok "controller Deployment pod template is at ${CTL_V2}" ||
  bad "controller Deployment is at '$(deploy_tag amp-controller)'"
[ "$(deploy_tag amp-reconcile-agent)" = "$CTL_V2" ] && ok "agent Deployment pod template is at ${CTL_V2} (it upgraded itself)" ||
  bad "agent Deployment is at '$(deploy_tag amp-reconcile-agent)'"
EP_PIN=$(kubectl -n "$NS" get deploy amp-controller \
  -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="AMP_ENDPOINT_IMAGE_TAG")].value}' 2>/dev/null)
[ "$EP_PIN" = "$CTL_V2" ] && ok "the endpoint pin moved to ${CTL_V2}" ||
  bad "the controller's endpoint pin is '${EP_PIN:-unset}', want ${CTL_V2} — the upgrade moved the controller but not what it deploys"
REV2=$(helm_revision)
[ "$REV2" = "$((REV1 + 1))" ] && ok "helm revision advanced exactly once (${REV1} -> ${REV2})" ||
  bad "helm revision went ${REV1} -> ${REV2}; an upgrade re-issued per tick is the failure mode this checks"

# --- 4. five minutes later, the same endpoints ---
hdr "settle ${SETTLE_SECONDS}s, then re-check the SAME endpoints"
sleep "$SETTLE_SECONDS"
for i in "${!EP_IDS[@]}"; do
  id="${EP_IDS[$i]}"
  st=$(env_json | jq -r --arg id "$id" \
    '.resources[]?|select(.kind=="endpoint" and .attrs.id==$id)|.state' 2>/dev/null | head -1)
  [ "$st" = ready ] && ok "endpoint ${id} still ready" || bad "endpoint ${id} state='${st:-GONE}'"
  uid=$(ep_deploy "$id" .metadata.uid)
  [ -n "$uid" ] && [ "$uid" = "${EP_UIDS[$i]}" ] &&
    ok "endpoint ${id} is the same Deployment (uid unchanged) — not recreated, not collected" ||
    bad "endpoint ${id} Deployment uid '${uid:-GONE}' != '${EP_UIDS[$i]}'"
  avail=$(ep_deploy "$id" .status.availableReplicas)
  [ "${avail:-0}" -ge 1 ] && ok "endpoint ${id} has a running pod" || bad "endpoint ${id} has no available replica"
  img=$(ep_deploy "$id" '.spec.template.spec.containers[0].image')
  [ "$img" = "${EP_IMAGES[$i]}" ] &&
    ok "endpoint ${id} still runs its original image (the upgrade rolls nothing)" ||
    bad "endpoint ${id} image moved ${EP_IMAGES[$i]} -> ${img}"
done

# --- 5. two more endpoints, on the upgraded controller ---
hdr "declare 2 more endpoints against the upgraded controller"
ALL=("${FIRST[@]}" "${LATER[@]}")
set_desired "$(desired_json "$CTL_V2" "${ALL[@]}")" && ok "desired state accepted" ||
  bad "POST desired (2 more endpoints) failed"

if wait_ready 900 endpoint "${ALL[@]}"; then
  ok "all 4 endpoints report ready"
else
  bad "the new endpoints did not converge in 15m"
  dump_inventory
fi

COUNT=$(env_json | jq -r '[.resources[]?|select(.kind=="endpoint")]|length' 2>/dev/null)
[ "$COUNT" = 4 ] && ok "the environment has 4 endpoints" || bad "the environment has ${COUNT:-?} endpoints, want 4"
for i in "${!FIRST[@]}"; do
  [ "$(res_attr endpoint "${FIRST[$i]}" id)" = "${EP_IDS[$i]}" ] &&
    ok "original endpoint ${FIRST[$i]} still has id ${EP_IDS[$i]}" ||
    bad "original endpoint ${FIRST[$i]} is now '$(res_attr endpoint "${FIRST[$i]}" id)', was ${EP_IDS[$i]}"
done
# The new pods are the proof the pin moved: the controller that created them is
# the upgraded one, so they carry the V2 endpoint image while the originals do not.
for e in "${LATER[@]}"; do
  id=$(res_attr endpoint "$e" id)
  img=$(ep_deploy "$id" '.spec.template.spec.containers[0].image')
  case "$img" in
  *:"$CTL_V2") ok "new endpoint $e (${id}) runs the ${CTL_V2} endpoint image" ;;
  *) bad "new endpoint $e (${id}) runs '${img:-none}', want the ${CTL_V2} pin" ;;
  esac
done

# --- 6. roll the fleet onto the controller's version ---
# wait_fleet <version> <timeout secs>: the fleet row ready at <version>.
wait_fleet() {
  local deadline=$((SECONDS + $2))
  while [ "$SECONDS" -lt "$deadline" ]; do
    [ "$(res_state fleet-version fleet)" = ready ] && [ "$(res_attr fleet-version fleet version)" = "$1" ] && return 0
    sleep 5
  done
  return 1
}
# check_fleet_images <version>: every endpoint's Deployment runs the <version> image.
check_fleet_images() {
  local e id img
  for e in "${ALL[@]}"; do
    id=$(res_attr endpoint "$e" id)
    img=$(ep_deploy "$id" '.spec.template.spec.containers[0].image')
    case "$img" in
    *:"$1") ok "endpoint $e (${id}) runs the $1 image" ;;
    *) bad "endpoint $e (${id}) runs '${img:-none}', want $1" ;;
    esac
  done
}

hdr "desired state: fleet-version@${CTL_V2}"
FLEET_V="$CTL_V2"
set_desired "$(desired_json "$CTL_V2" "${ALL[@]}")" && ok "fleet-version declared" || bad "POST desired (fleet-version) failed — is fleet-version an allowed kind?"
ROLL_START=$SECONDS
if wait_fleet "$CTL_V2" 1200; then
  ok "fleet row reports ready at ${CTL_V2} after $((SECONDS - ROLL_START))s"
else
  bad "fleet did not reach ready@${CTL_V2} in 20m (state='$(res_state fleet-version fleet)', attrs: $(env_json | jq -c '.resources[]?|select(.kind=="fleet-version")|.attrs' 2>/dev/null))"
  kubectl -n "$NS" logs deploy/amp-reconcile-agent --tail=40 2>/dev/null | sed 's/^/       /'
fi
check_fleet_images "$CTL_V2"
if wait_ready 600 endpoint "${ALL[@]}"; then
  ok "all 4 endpoints report ready after the roll"
else
  bad "endpoints not ready after the roll"
  dump_inventory
fi

# --- 7. controller and fleet in one write: upgrade, then roll ---
if [ -n "$CTL_V3" ]; then
  hdr "desired state: controller@${CTL_V3} + fleet-version@${CTL_V3} in one write"
  FLEET_V="$CTL_V3"
  set_desired "$(desired_json "$CTL_V3" "${ALL[@]}")" && ok "declared" || bad "POST desired (V3) failed"
  UP_START=$SECONDS
  if wait_fleet "$CTL_V3" 1800; then
    ok "fleet row reports ready at ${CTL_V3} after $((SECONDS - UP_START))s"
  else
    bad "fleet did not reach ready@${CTL_V3} in 30m (state='$(res_state fleet-version fleet)')"
    dump_inventory
  fi
  [ "$(deploy_tag amp-controller)" = "$CTL_V3" ] && ok "controller Deployment is at ${CTL_V3}" || bad "controller Deployment is at '$(deploy_tag amp-controller)'"
  check_fleet_images "$CTL_V3"
else
  note "CTL_V3 unset: skipping the one-write controller + fleet upgrade"
fi

hdr "result"
printf '  %d passed, %d failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ] || exit 1
