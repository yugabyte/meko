#!/usr/bin/env bash
#
# End-to-end smoke test for a DEPLOYED amp controller (the stack `deploy/up.sh`
# stands up). Drives the controller's REST API through the full async-task
# lifecycle and validates that a created endpoint is a real, writable Postgres.
#
# It is the deployment-side companion to the controller-on-Docker integration in
# test/integration/controller: that suite proves the control plane on Docker; this one
# proves an actual Kubernetes deployment, including the write-ahead task contract
# (#113) and real data I/O through the compute -> safekeeper -> pageserver -> YB
# storage path.
#
# What it checks (exits non-zero if any FAIL):
#   - health (/healthz, /readyz)
#   - createUser / createStoragePool stay synchronous (201 + resource)
#   - createCluster / createEndpoint / branch / deleteEndpoint / deleteCluster /
#     deleteBranch all return 202 + a Task + a Location header, and the task runs
#     to status=succeeded (polled via GET /tasks/{id})
#   - the created endpoint is USABLE: connect with psql and CREATE/INSERT/UPDATE/
#     DELETE/CHECKPOINT/SELECT (the CHECKPOINT forces the writes through the
#     safekeeper -> pageserver -> YB storage path), and the recorded connection
#     address routes to a ready Service backend
#   - kube-state-metrics exports the endpoint pod's CPU request and identity
#     labels (skipped when there is no compute collector)
#   - GET /tasks lists tasks, filters by status, and rejects a bad status with 400
#   - a duplicate name surfaces as a FAILED task (not a synchronous 409)
#   - deleting a branch that still has child forks is refused with 409
#   - delete_cluster cascades (retires endpoints, drops branches + cluster)
#
# Optional (SMOKE_EXTENSIONS=1): the externalized-extension path — extensions are
# seeded on the default branch at create, so create a cluster with
# extensions:["pgvector","age"], deploy an endpoint (which mounts the branch set
# with no per-endpoint opt-in), assert both ImageVolumes mounted at
# /extensions/<name>, `CREATE EXTENSION`, and store/read real vector rows with a
# nearest-neighbour (`<->`) query plus an AGE cypher round-trip. Then the preload
# path: a cluster with extensions:["pg_cron"], assert the ImageVolume mounted at
# /extensions/pg_cron, pg_cron is in shared_preload_libraries, `CREATE EXTENSION
# pg_cron`, and a scheduled job runs to success. Then a private-source extension:
# the extension registry: an admin registers the CloudNativePG PostGIS image
# through PUT /extensions/postgis (the product catalog names no third-party image
# at all; this one lives in a test fixture), a cluster opts into it, and
# the pod mounts it with the registered layout, bundled libraries and ABI waiver;
# a cluster with extensions:["hexabridge"], assert the ImageVolume mounted at
# /extensions/hexabridge, `CREATE EXTENSION hexabridge` (superuser-only), and an
# Oracle-compat function returns its expected value. Then the other half of the
# API: addBranchExtensions on the still-live pgvector+age branch — union pg_cron
# in, assert getBranch reports the unioned set, the live endpoint is redeployed
# with the new mount, and its address survives the redeploy unchanged. Finally,
# fork that branch and assert the fork inherits its parent's set (getBranch) and
# a fresh endpoint on the fork mounts pgvector with no per-endpoint opt-in.
# Last, the documentdb engine end to end: a cluster created with
# engine:"documentdb" gets an endpoint whose pod runs the gateway sidecar, and
# the API reports its documentdb_address, and compute_ctl has created the
# extension in its own documentdb database. A MongoDB driver then logs in as the
# default user with the createEndpoint password (SCRAM-SHA-256). It writes,
# queries, aggregates and deletes over the wire protocol, and SQL reads back
# the same documents. A wrong password is refused.
# Requires the cluster's ImageVolume feature gate and images pushed by
# deploy/extensions/build.sh, plus docker on this host for the client.
#
# Its cluster is torn down by the cascade delete it exercises, and cleanup()
# uninstalls any stray endpoint Helm releases. The pool and user rows are NOT
# removed — the API has no delete for either (#1083) — so the run reuses one
# well-known pool rather than creating a fresh one it could never reclaim.
# Re-runnable. Pass KEEP=1 to leave resources for inspection.
#
# Prereqs: kubectl (pointed at the cluster), curl, jq; a controller deployed in
# $NS (deploy/up.sh) with the endpoint deployer enabled. psql is NOT needed
# locally — the data-I/O checks exec the psql baked into the endpoint pod.
#
# Usage:
#   deploy/smoke-test.sh                 # against namespace amp-test
#   NS=amp deploy/smoke-test.sh          # another namespace
#   KEEP=1 deploy/smoke-test.sh          # don't delete the created resources
set -uo pipefail

NS="${NS:-amp-test}"
PF_PORT="${PF_PORT:-18080}"
# The documentdb check drives the gateway from this host through a port-forward,
# using pymongo from upstream documentdb's own functional-test image, pinned by
# digest as upstream pins it (documentdb-local/functional-tests/config/image.yml).
GW_PF_PORT="${GW_PF_PORT:-18260}"
DOCUMENTDB_CLIENT_IMAGE="${DOCUMENTDB_CLIENT_IMAGE:-ghcr.io/documentdb/functional-tests@sha256:79ed3d433bc2664655a31064a66eee3bef3cc2a8b4eabfaab3e3ef62d44ce3ba}"
# Path to psql INSIDE the endpoint pod (PG v18 install; not needed on the host).
PSQL_BIN="${PSQL_BIN:-/usr/local/v18/bin/psql}"
KEEP="${KEEP:-0}"
# The pool is ONE fixed row, created on the first run and reused by every later
# one. The old name came from $$ — a container PID, small and recurring — so a
# repeat made createStoragePool a 409 no run could recover from (#1082).
#
# #1083 added pool delete and rename, so a name is no longer claimed forever and
# a per-run name could now be cleaned up. The fixed row is still what this does:
# a unique name per run only stops leaking if every run's delete succeeds, and a
# run that fails before its cleanup leaks a pool that nothing else collects. ycp
# lists pools unpaged and gets the oldest 100 (ampbackend/backend.go ->
# defaultPageLimit, ordered created_at), so each leak walks the list toward the
# point where a YCP-managed pool drops off the page, Diff reads it as absent, and
# its cluster create fails "parent pool not found". One reused row cannot leak.
#
# Reusing by name (below) is also why this script must not rename the pool.
POOL_NAME="pool-smoke"
# The cluster IS deletable and clusters_name_per_pool is partial on
# `dropped_at IS NULL`, so a per-run cluster name leaves nothing that blocks
# reuse, and a run that dies mid-way cannot wedge the next one on a live
# duplicate.
SUFFIX="smoke-$(date +%s)-${RANDOM}"
PROJ_NAME="proj-${SUFFIX}"

ROOT_URL="http://127.0.0.1:${PF_PORT}"
BASE="${ROOT_URL}/api/v1"
PASS=0; FAIL=0
# The client address the controller should record for an endpoint, or "" when this
# deployment does not publish endpoints on a plain ClusterIP Service — under
# ENDPOINT_SERVICE_TYPE=NodePort or LoadBalancer the recorded address is
# legitimately an external <ip>:<port>, and there is no single string to expect.
want_endpoint_addr() {
  local ep="$1" type
  type=$(kubectl -n "$NS" get svc "yb-ep-$ep" -o jsonpath='{.spec.type}' 2>/dev/null)
  [ "$type" = ClusterIP ] || return 0
  printf '%s' "yb-ep-$ep.$NS.svc.cluster.local:55433"
}
ok()  { printf '  \033[0;32mPASS\033[0m %s\n' "$*"; PASS=$((PASS+1)); }
bad() { printf '  \033[0;31mFAIL\033[0m %s\n' "$*"; FAIL=$((FAIL+1)); }
hdr() { printf '\n\033[1;36m== %s\033[0m\n' "$*"; }

# POST json and assign the created id to the named variable. On failure it
# leaves that empty and puts the status + body in LAST_ERR, so the log says why.
# A bare `curl -sf | jq` prints nothing on an HTTP error, which left two dev
# promotions undiagnosable from the Jenkins console alone (#1079).
#
# The id comes back through a nameref rather than stdout: `VAR=$(post_id ...)`
# would run the body in a subshell, and LAST_ERR set there never reaches here.
LAST_ERR=""
post_id() {
  local __var="$1" url="$2" body="$3" out code
  out=$(curl -s -w '\n%{http_code}' --max-time 30 -X POST "$url" \
        -H 'Content-Type: application/json' -d "$body" 2>&1) || true
  code="${out##*$'\n'}"
  out="${out%$'\n'*}"
  if [ "$code" = 200 ] || [ "$code" = 201 ]; then
    local id
    id=$(printf '%s' "$out" | jq -r '.id // empty' 2>/dev/null) || id=""
    printf -v "$__var" '%s' "$id"
    # A 2xx carrying no usable id is still a failure. Without this the caller
    # printed a bare FAIL with no reason — the symptom #1079 is about — and jq's
    # own error goes to stderr this does not capture, so an unparseable body
    # lands here too.
    if [ -n "$id" ]; then
      LAST_ERR=""
    else
      LAST_ERR="code=$code but no .id in body=$(printf '%s' "$out" | tr -d '\n' | head -c 300)"
    fi
    return 0
  fi
  LAST_ERR="code=$code body=$(printf '%s' "$out" | tr -d '\n' | head -c 300)"
  printf -v "$__var" '%s' ''
  return 0
}

PF_PID=""
cleanup() {
  [ -n "$PF_PID" ] && kill "$PF_PID" 2>/dev/null
  [ -n "${GW_PF_PID:-}" ] && kill "$GW_PF_PID" 2>/dev/null
  if [ "$KEEP" != "1" ]; then
    # best-effort: drop any endpoint Helm releases this run may have left behind
    for r in $(helm -n "$NS" list -aq 2>/dev/null | grep '^yb-ep-'); do
      helm -n "$NS" uninstall "$r" --wait --timeout 60s >/dev/null 2>&1 &
    done
    wait 2>/dev/null
  fi
}
trap cleanup EXIT

# poll_task <id> [timeout_iters] -> echoes terminal status; rc 0 iff succeeded
poll_task() {
  local id="$1" max="${2:-210}" status
  [ -z "$id" ] && { echo "no-task"; return 1; }   # guard: a non-202 create has no id to poll
  for ((i=0; i<max; i++)); do
    status=$(curl -sf "$BASE/tasks/$id" | jq -r '.status' 2>/dev/null)
    case "$status" in
      succeeded) echo "$status"; return 0 ;;
      failed)    echo "$status"; return 1 ;;
    esac
    sleep 2
  done
  echo "timeout"; return 1
}

# create_task <METHOD> <path> [json-body] -> echoes "<http_code>|<location>|<task_id>"
create_task() {
  local method="$1" path="$2" body="${3:-}" code loc tid body_file hdr_file
  body_file=$(mktemp); hdr_file=$(mktemp)            # unique temp files (avoid CWE-377 races)
  code=$(curl -s -o "$body_file" -D "$hdr_file" -w '%{http_code}' \
          -X "$method" "$BASE$path" ${body:+-H 'Content-Type: application/json' -d "$body"})
  loc=$(grep -i '^location:' "$hdr_file" | tr -d '\r' | awk '{print $2}')
  tid=$(jq -r '.id // empty' "$body_file" 2>/dev/null)
  rm -f "$body_file" "$hdr_file"
  echo "${code}|${loc}|${tid}"
}

# psql_pod <pod> <host> <port>  (SQL on stdin) -> runs psql inside the endpoint pod.
# Connects as cloud_admin over loopback, which the endpoint's pg_hba trusts (no password).
psql_pod() {
  local pod="$1" host="$2" port="$3"
  # -tA: tuples-only + unaligned, so a SELECT prints the raw value (e.g. 3,24) with
  # no header/padding/footer — the grep assertions below anchor on that exact form.
  kubectl -n "$NS" exec -i "$pod" -- \
    "$PSQL_BIN" -h "$host" -p "$port" -U cloud_admin -d postgres -tA -v ON_ERROR_STOP=1
}

# wait_ready_pod <endpoint-id> -> echoes the endpoint pod name (no pod/ prefix)
# once its container is Ready; empty on timeout.
wait_ready_pod() {
  local ep="$1" pod=""
  for ((i=0; i<60; i++)); do
    pod=$(kubectl -n "$NS" get pods -o name 2>/dev/null | grep "yb-ep-$ep" | head -1)
    [ -n "$pod" ] && [ "$(kubectl -n "$NS" get "$pod" -o jsonpath='{.status.containerStatuses[0].ready}' 2>/dev/null)" = true ] && break
    sleep 3
  done
  echo "${pod#pod/}"
}

# --- start: port-forward the controller API ---
kubectl -n "$NS" port-forward svc/amp-controller "${PF_PORT}:8080" >/tmp/smoke-pf.log 2>&1 &
PF_PID=$!
for ((i=0; i<30; i++)); do curl -sf "$ROOT_URL/healthz" >/dev/null 2>&1 && break; sleep 1; done

hdr "health"
curl -sf "$ROOT_URL/healthz" >/dev/null && ok "healthz 200" || bad "healthz"
curl -sf "$ROOT_URL/readyz"  >/dev/null && ok "readyz 200"  || bad "readyz"

hdr "user + storage pool (synchronous 201)"
post_id USER "$BASE/users" '{}'
[ -n "$USER" ] && ok "createUser 201 ($USER)" || bad "createUser $LAST_ERR"
# One fixed pool, reused across runs (see the note by POOL_NAME). The first run
# creates it; every later run gets a 409 and looks the existing one up, which is
# the normal path — not an error.
post_id POOL "$BASE/storage-pools" \
        "{\"name\":\"$POOL_NAME\",\"owner_user_id\":\"$USER\"}"
if [ -n "$POOL" ]; then
  ok "createStoragePool 201 ($POOL)"
else
  POOL=$(curl -sf "$BASE/storage-pools" \
         | jq -r --arg n "$POOL_NAME" '.storage_pools[]|select(.name==$n)|.id' | head -1)
  [ -n "$POOL" ] && ok "storage pool reused ($POOL)" \
                 || bad "createStoragePool $LAST_ERR (and no existing $POOL_NAME to reuse)"
fi

hdr "createCluster -> 202 + Task + Location -> task succeeded -> materialized"
# Tagged so the endpoint below can be checked for the labels it inherits (`smoke-cluster`)
# and the one it overrides (`team`).
IFS='|' read -r code loc tid < <(create_task POST "/storage-pools/$POOL/clusters" "{\"name\":\"$PROJ_NAME\",\"tags\":{\"smoke-cluster\":\"c\",\"team\":\"cluster\"}}")
[ "$code" = 202 ] && ok "202 Accepted" || bad "createCluster code=$code (want 202)"
[[ "$loc" == /api/v1/tasks/* ]] && ok "Location: $loc" || bad "Location header: '$loc'"
[ "$(curl -sf "$BASE/tasks/$tid" | jq -r .kind)" = create_cluster ] && ok "task kind=create_cluster" || bad "task kind"
st=$(poll_task "$tid"); [ "$st" = succeeded ] && ok "create_cluster succeeded" || bad "create_cluster task=$st"
PROJ=$(curl -sf "$BASE/storage-pools/$POOL/clusters" | jq -r --arg n "$PROJ_NAME" '.clusters[]|select(.name==$n)|.id')
DEF=$(curl -sf "$BASE/storage-pools/$POOL/clusters/$PROJ" | jq -r '.default_branch.branch_id')
[ -n "$PROJ" ] && [ -n "$DEF" ] && ok "materialized proj=$PROJ default_branch=$DEF" || bad "cluster not materialized"

# A writer needs a client-role password; the controller generates none.
# Fixed for the run: this asserts the API contract, not secrecy.
EP_PASSWORD="${EP_PASSWORD:-smoke-test-pw-1}"

hdr "createEndpoint (read_write on default branch) -> task succeeded -> address + pod Ready"
IFS='|' read -r code loc tid < <(create_task POST "/storage-pools/$POOL/clusters/$PROJ/branches/$DEF/endpoints" "{\"role\":\"read_write\",\"password\":\"$EP_PASSWORD\",\"tags\":{\"team\":\"endpoint\"}}")
[ "$code" = 202 ] && ok "202 Accepted" || bad "createEndpoint code=$code"
st=$(poll_task "$tid"); [ "$st" = succeeded ] && ok "create_endpoint succeeded" || bad "create_endpoint task=$st"
EP=$(curl -sf "$BASE/storage-pools/$POOL/clusters/$PROJ/branches/$DEF/endpoints" | jq -r '.endpoints[0].id')
ADDR=$(curl -sf "$BASE/storage-pools/$POOL/clusters/$PROJ/branches/$DEF/endpoints/$EP" | jq -r '.address')
# On a ClusterIP deployment the recorded address must be the endpoint Service's
# DNS name. A pod IP here is CLOUDGA-36993: it dies at the endpoint's next
# reschedule, and the connection string the console handed a client dies with it.
WANT_ADDR=$(want_endpoint_addr "$EP")
if [ -n "$WANT_ADDR" ]; then
  [ "$ADDR" = "$WANT_ADDR" ] && ok "endpoint address is the Service DNS: $ADDR" \
    || bad "endpoint address $ADDR, want $WANT_ADDR"
else
  [ -n "$ADDR" ] && [ "$ADDR" != null ] && ok "endpoint address recorded: $ADDR" \
    || bad "endpoint address empty"
fi
POD=""
for ((i=0; i<60; i++)); do
  POD=$(kubectl -n "$NS" get pods -o name 2>/dev/null | grep "yb-ep-$EP" | head -1)
  [ -n "$POD" ] && [ "$(kubectl -n "$NS" get "$POD" -o jsonpath='{.status.containerStatuses[0].ready}' 2>/dev/null)" = true ] && break
  sleep 3
done
POD="${POD#pod/}"
if [ -n "$POD" ] && [ "$(kubectl -n "$NS" get "pod/$POD" -o jsonpath='{.status.containerStatuses[0].ready}' 2>/dev/null)" = true ]; then
  ok "endpoint pod Ready ($POD)"
else
  bad "endpoint pod not Ready"
  # Dump the endpoint's own state here, while the pod still exists: later
  # scenarios delete the cluster, which retires it, and the workflow's
  # end-of-job dump then has nothing left to look at. The supervisor's log is
  # the only place that says which child failed to come up.
  echo "---- endpoint pod state (${POD:-<none>}) ----"
  kubectl -n "$NS" get pods -o wide 2>&1 | sed 's/^/    /' || true
  if [ -n "$POD" ]; then
    kubectl -n "$NS" describe "pod/$POD" 2>&1 | sed -n '/Events:/,$p' | sed 's/^/    /' || true
    echo "---- supervisor log (current) ----"
    kubectl -n "$NS" logs "pod/$POD" -c endpoint --tail=200 2>&1 | sed 's/^/    /' || true
    echo "---- supervisor log (previous container, if it restarted) ----"
    kubectl -n "$NS" logs "pod/$POD" -c endpoint --previous --tail=200 2>&1 | sed 's/^/    /' || true
  fi
  echo "---- namespace events ----"
  kubectl -n "$NS" get events --sort-by=.lastTimestamp 2>&1 | tail -40 | sed 's/^/    /' || true
fi

hdr "tags: the endpoint's objects carry its cluster's tags under its own"
# Each resource reports only its own tags; the merge shows on the objects.
[ "$(curl -sf "$BASE/storage-pools/$POOL/clusters/$PROJ" | jq -cS '.tags')" = '{"smoke-cluster":"c","team":"cluster"}' ] \
  && ok "cluster reports its tags" || bad "cluster tags: $(curl -sf "$BASE/storage-pools/$POOL/clusters/$PROJ" | jq -c '.tags')"
[ "$(curl -sf "$BASE/storage-pools/$POOL/clusters/$PROJ/branches/$DEF/endpoints/$EP" | jq -cS '.tags')" = '{"team":"endpoint"}' ] \
  && ok "endpoint reports only its own tags" || bad "endpoint tags: $(curl -sf "$BASE/storage-pools/$POOL/clusters/$PROJ/branches/$DEF/endpoints/$EP" | jq -c '.tags')"
for obj in "pod/$POD" "deployment/yb-ep-$EP" "service/yb-ep-$EP"; do
  got=$(kubectl -n "$NS" get "$obj" -o json 2>/dev/null \
        | jq -r '.metadata.labels | "\(.["smoke-cluster"] // "")/\(.team // "")"')
  [ "$got" = "c/endpoint" ] && ok "$obj labelled smoke-cluster=c team=endpoint" \
    || bad "$obj labels smoke-cluster/team = '$got', want 'c/endpoint'"
done
got=$(kubectl -n "$NS" get "pod/$POD" -o json 2>/dev/null | jq -r '.metadata.labels |
  [.["amp.yugabyte.com/endpoint-id"], .["amp.yugabyte.com/branch-id"], .["amp.yugabyte.com/cluster-id"],
   .["amp.yugabyte.com/storage-pool-id"], .["amp.yugabyte.com/ep-type"]] | map(. // "") | join(" ")')
[ "$got" = "$EP $DEF $PROJ $POOL read_write" ] && ok "pod carries its endpoint/branch/cluster/pool/role identity labels" \
  || bad "pod identity labels = '$got', want '$EP $DEF $PROJ $POOL read_write'"
got=$(kubectl -n "$NS" get "pod/$POD" -o json 2>/dev/null | jq -r '.metadata.annotations["amp.yugabyte.com/storage-pool-name"] // ""')
[ "$got" = "$POOL_NAME" ] && ok "pod carries its pool name annotation" \
  || bad "pod storage-pool-name annotation = '$got', want '$POOL_NAME'"
# The selector must stay the chart's own: it is immutable, and the controller
# finds the pod by app.kubernetes.io/instance.
sel=$(kubectl -n "$NS" get "deployment/yb-ep-$EP" -o json 2>/dev/null | jq -c '.spec.selector.matchLabels | keys')
[ "$sel" = '["app.kubernetes.io/instance","app.kubernetes.io/name"]' ] && ok "Deployment selector untouched" \
  || bad "Deployment selector keys = $sel"

hdr "kube-state-metrics: the endpoint pod's requests, identity labels and pool name are exported"
# KSM's own /metrics, which proves the namespace scoping and the label allowlist.
# Not the store: itest-dev runs none, so the collector's pod filter and label
# rename are covered by the chart tests only.
KSM_POD=$(kubectl -n "$NS" get pods -l app.kubernetes.io/component=kube-state-metrics \
  -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)
COMPUTE_CR=$(kubectl -n "$NS" get opentelemetrycollectors -o name 2>/dev/null | grep -- '-compute$' | head -1)
if [ -z "$KSM_POD" ] && [ -z "$COMPUTE_CR" ]; then
  echo "  SKIP no compute collector in $NS (otel off, or no OpenTelemetry Operator)"
elif [ -z "$KSM_POD" ]; then
  bad "compute collector $COMPUTE_CR is deployed but no kube-state-metrics pod is"
else
  kubectl -n "$NS" wait --for=condition=Ready "pod/$KSM_POD" --timeout=120s >/dev/null 2>&1 \
    && ok "kube-state-metrics pod Ready ($KSM_POD)" || bad "kube-state-metrics pod $KSM_POD not Ready"
  ksm=$(kubectl get --raw "/api/v1/namespaces/$NS/pods/$KSM_POD:8080/proxy/metrics" 2>&1) || true
  echo "$ksm" | grep '^kube_pod_container_resource_requests{' | grep "pod=\"$POD\"" | grep 'resource="cpu"' >/dev/null \
    && ok "KSM exports the endpoint pod's CPU request" \
    || bad "KSM has no kube_pod_container_resource_requests{resource=cpu} for $POD: $(echo "$ksm" | head -c 300)"
  echo "$ksm" | grep '^kube_pod_labels{' | grep "pod=\"$POD\"" \
    | grep "label_amp_yugabyte_com_endpoint_id=\"$EP\"" | grep "label_amp_yugabyte_com_storage_pool_id=\"$POOL\"" >/dev/null \
    && ok "KSM exports the endpoint pod's identity labels on kube_pod_labels" \
    || bad "KSM kube_pod_labels for $POD lacks endpoint_id=$EP / storage_pool_id=$POOL"
  echo "$ksm" | grep '^kube_pod_annotations{' | grep "pod=\"$POD\"" \
    | grep "annotation_amp_yugabyte_com_storage_pool_name=\"$POOL_NAME\"" >/dev/null \
    && ok "KSM exports the endpoint pod's pool name on kube_pod_annotations" \
    || bad "KSM kube_pod_annotations for $POD lacks storage_pool_name=$POOL_NAME"
fi

hdr "endpoint is USABLE: write + read real data via psql"
# (a) loopback (pg_hba trusts loopback): full DDL/DML + CHECKPOINT (forces the
#     writes through safekeeper -> pageserver -> YugabyteDB), then read back.
out=$(psql_pod "$POD" 127.0.0.1 55433 <<'SQL' 2>&1
DROP TABLE IF EXISTS smoke;
CREATE TABLE smoke (id serial PRIMARY KEY, k text NOT NULL, v int);
INSERT INTO smoke (k,v) VALUES ('alpha',1),('beta',2),('gamma',3),('delta',4);
UPDATE smoke SET v = v*10 WHERE k='beta';
DELETE FROM smoke WHERE k='delta';
CHECKPOINT;
SELECT count(*)||','||sum(v) FROM smoke;
SQL
)
echo "$out" | grep -q '^3,24$' && ok "loopback CRUD + checkpoint + readback (3 rows, sum=24)" || bad "loopback data I/O (got: $(echo "$out"|tail -1))"
# (b) the recorded connection address routes to a live backend: the endpoint's Service
#     has a ready endpoint (the compute pod). This proves the address the API hands a
#     client points at a serving pod, without a flaky cross-pod psql (NodePort hairpin /
#     DNS / lib-path differ by environment) — the loopback CRUD above already proves the
#     compute serves real reads/writes through the storage stack.
EPS=$(kubectl -n "$NS" get endpoints "yb-ep-$EP" -o jsonpath='{.subsets[*].addresses[*].ip}' 2>/dev/null)
[ -n "$EPS" ] && ok "recorded address $ADDR routes to a ready Service backend ($EPS)" \
  || bad "endpoint Service has no ready backend for $ADDR"
# (c) pg_stat_statements is live without anyone asking for it: the chart preloads the
#     library and compute_ctl creates the extension after start. It is what the
#     exporter's statement collector and compute_ctl's /insights read. The create runs
#     in a background task, so give it a few seconds rather than racing it.
out=""
for ((r=0; r<10; r++)); do
  out=$(psql_pod "$POD" 127.0.0.1 55433 <<<'SELECT count(*) > 0 FROM public.pg_stat_statements;' 2>&1)
  echo "$out" | grep -q '^t$' && break
  sleep 3
done
echo "$out" | grep -q '^t$' && ok "pg_stat_statements preloaded + created, tracking statements" \
  || bad "pg_stat_statements not usable on a fresh endpoint (got: $(echo "$out"|tail -1))"
# (c') pgsentinel: the view must have rows, since a sampler that died at startup
#      leaves it readable but empty. pg_sleep keeps this session active across two
#      1 Hz ticks. Same CREATE EXTENSION retry as (c).
out=""
for ((r=0; r<10; r++)); do
  out=$(psql_pod "$POD" 127.0.0.1 55433 <<'SQL' 2>&1
SELECT pg_sleep(2);
SELECT count(*) > 0 FROM public.pg_active_session_history;
SQL
)
  echo "$out" | tail -1 | grep -q '^t$' && break
  sleep 3
done
echo "$out" | tail -1 | grep -q '^t$' && ok "pgsentinel preloaded + created, sampler recording into active session history" \
  || bad "pgsentinel not usable on a fresh endpoint (got: $(echo "$out"|tail -1))"
# (d) the shipped Postgres is a release build. `debug_assertions` reports whether it
#     was configured --enable-cassert, which the image must never be: assertions plus
#     the -O0 that comes with them are a large, silent slowdown, and nothing else here
#     looks at how the binary was built.
#
#     This has been wrong before (#1019): the Dockerfile builds Postgres twice, and the
#     second stage inherited no BUILD_TYPE — which selects PostgreSQL's build type and
#     defaults to `debug`, so a cassert build was quietly reinstalled over the release
#     one. A dev tree looked fine, because build.sh sets the type; only the image was
#     affected, and nothing asserted on it. Both stages of both Dockerfiles now state
#     BUILD_TYPE=release, and a debug build installs to a different tree entirely
#     (pg_install-release/ vs pg_install-debug/), so a mismatch fails the COPY rather than
#     silently shipping. This stays the check that would catch a new path in.
asrt=$(psql_pod "$POD" 127.0.0.1 55433 <<<'SHOW debug_assertions;' 2>&1)
if echo "$asrt" | grep -q '^off$'; then
  ok "shipped Postgres is a release build (debug_assertions=off)"
else
  bad "shipped Postgres has assertions enabled — an --enable-cassert build reached the image (debug_assertions: $(echo "$asrt"|tail -1))"
fi

# (e) the password the create supplied is stored as a SCRAM verifier. This is the
#     one property no other assertion here would catch: Postgres stores a
#     well-formed verifier verbatim but silently hashes a MALFORMED one AS the
#     password, so a broken verifier does not fail the deploy — it produces an
#     endpoint whose password is the verifier string sitting in the release
#     Secret, and whose real password authenticates nobody. `md5…` here means the
#     control plane regressed to md5, which the `scram-sha-256` hba line refuses.
#     13 characters, not 14: the verifier's next character is `$`, which would
#     have to be escaped in the grep pattern below rather than anchoring it.
rolpw=$(psql_pod "$POD" 127.0.0.1 55433 \
  <<<"SELECT left(rolpassword, 13) FROM pg_authid WHERE rolname = 'cloud_admin';" 2>&1)
echo "$rolpw" | grep -q '^SCRAM-SHA-256$' \
  && ok "cloud_admin's stored verifier is SCRAM-SHA-256" \
  || bad "cloud_admin's verifier is not SCRAM (got: $(echo "$rolpw"|tail -1)) — a malformed verifier is stored as the password itself"

# (f) and it actually authenticates: connect over TCP as a remote client would,
#     with the plaintext the create was given. Loopback is `trust`, so PGPASSWORD
#     alone proves nothing — `require_auth=scram-sha-256` makes libpq refuse to
#     authenticate any other way, which is what pins the negotiated method rather
#     than the hba keyword (the server log prints `md5` for both).
#     The pod dials its OWN pod IP rather than 127.0.0.1, which is what makes the
#     connection match `host all all all scram-sha-256` instead of the loopback
#     `trust` lines — and avoids the cross-pod psql that (b) explains is flaky.
POD_IP=$(kubectl -n "$NS" get pod "$POD" -o jsonpath='{.status.podIP}' 2>/dev/null)
authout=$(kubectl -n "$NS" exec -i "$POD" -- env PGPASSWORD="$EP_PASSWORD" \
  "$PSQL_BIN" "host=$POD_IP port=55433 user=cloud_admin dbname=postgres require_auth=scram-sha-256" \
  -tA -c 'SELECT 1;' 2>&1) || true
echo "$authout" | grep -q '^1$' \
  && ok "remote client authenticates with SCRAM (require_auth=scram-sha-256)" \
  || bad "SCRAM authentication failed for cloud_admin: $(echo "$authout"|tail -1)"

hdr "createBranch (fork default) -> task succeeded"
# The default fork-point resolution reads the parent writer's safekeeper flush_lsn;
# that read can transiently 400 in the first seconds after the writer becomes Ready
# (safekeeper HTTP not yet reachable via its Service). A real client retries; so do we.
code=""
for ((r=0; r<8; r++)); do
  IFS='|' read -r code loc tid < <(create_task POST "/storage-pools/$POOL/clusters/$PROJ/branches" "{\"name\":\"b1\",\"parent_branch_id\":\"$DEF\"}")
  [ "$code" = 202 ] && break
  sleep 5
done
[ "$code" = 202 ] && ok "202 Accepted" || bad "createBranch code=$code"
st=$(poll_task "$tid"); [ "$st" = succeeded ] && ok "branch succeeded" || bad "branch task=$st"
B1=$(curl -sf "$BASE/storage-pools/$POOL/clusters/$PROJ/branches" | jq -r '.branches[]|select(.name=="b1")|.branch_id')
[ -n "$B1" ] && ok "fork b1 materialized ($B1)" || bad "fork b1 missing"

hdr "GET /tasks: list + filter + invalid-status 400"
TOTAL=$(curl -sf "$BASE/tasks?limit=100" | jq -r '.total')
[ "${TOTAL:-0}" -ge 3 ] 2>/dev/null && ok "listTasks total=$TOTAL" || bad "listTasks total=$TOTAL"
SUC=$(curl -sf "$BASE/tasks?status=succeeded" | jq -r '.tasks|length')
[ "${SUC:-0}" -ge 1 ] 2>/dev/null && ok "listTasks?status=succeeded -> $SUC" || bad "listTasks status filter"
code=$(curl -s -o /dev/null -w '%{http_code}' "$BASE/tasks?status=bogus")
[ "$code" = 400 ] && ok "listTasks?status=bogus -> 400" || bad "invalid status -> $code (want 400)"

hdr "duplicate cluster name -> task FAILS (not a synchronous 409)"
IFS='|' read -r code loc tid < <(create_task POST "/storage-pools/$POOL/clusters" "{\"name\":\"$PROJ_NAME\"}")
[ "$code" = 202 ] && ok "dup-name accepted 202" || bad "dup-name code=$code"
st=$(poll_task "$tid" 30); [ "$st" = failed ] && ok "duplicate name -> task failed" || bad "dup name task=$st (want failed)"

hdr "delete a branch with a child fork -> 409 (no teardown)"
code=$(curl -s -o /dev/null -w '%{http_code}' -X DELETE "$BASE/storage-pools/$POOL/clusters/$PROJ/branches/$DEF")
[ "$code" = 409 ] && ok "deleteBranch(default w/ child b1) -> 409" || bad "deleteBranch(default) -> $code (want 409)"

hdr "deleteBranch b1 -> task succeeded -> gone"
IFS='|' read -r code loc tid < <(create_task DELETE "/storage-pools/$POOL/clusters/$PROJ/branches/$B1")
[ "$code" = 202 ] && ok "202 Accepted" || bad "deleteBranch code=$code"
st=$(poll_task "$tid"); [ "$st" = succeeded ] && ok "delete_branch succeeded" || bad "delete_branch task=$st"
[ "$(curl -s -o /dev/null -w '%{http_code}' "$BASE/storage-pools/$POOL/clusters/$PROJ/branches/$B1")" = 404 ] && ok "b1 gone (404)" || bad "b1 still present"

hdr "deleteCluster cascade -> task succeeded -> cluster gone + endpoint retired"
IFS='|' read -r code loc tid < <(create_task DELETE "/storage-pools/$POOL/clusters/$PROJ")
[ "$code" = 202 ] && ok "202 Accepted" || bad "deleteCluster code=$code"
st=$(poll_task "$tid"); [ "$st" = succeeded ] && ok "delete_cluster cascade succeeded" || bad "delete_cluster task=$st"
[ "$(curl -s -o /dev/null -w '%{http_code}' "$BASE/storage-pools/$POOL/clusters/$PROJ")" = 404 ] && ok "cluster gone (404)" || bad "cluster still present"
for ((i=0; i<20; i++)); do [ "$(kubectl -n "$NS" get pods 2>/dev/null | grep -c "yb-ep-$EP")" = 0 ] && break; sleep 3; done
[ "$(kubectl -n "$NS" get pods 2>/dev/null | grep -c "yb-ep-$EP")" = 0 ] && ok "endpoint instance retired" || bad "endpoint instance lingering"

if [ "${SMOKE_EXTENSIONS:-0}" = 1 ]; then
  hdr "externalized extensions: pgvector + age seeded on the default branch, CREATE EXTENSION, fork inherits the set"

  # A self-contained cluster that seeds BOTH pgvector and age on its default
  # branch at creation — the two non-preload catalog extensions. Every endpoint
  # on the branch mounts the branch's set, so this exercises multi-extension
  # ImageVolume mounting (each extension is its own mount at /extensions/<name>).
  IFS='|' read -r code loc tid < <(create_task POST "/storage-pools/$POOL/clusters" '{"name":"extproj","extensions":["pgvector","age"]}')
  st=$(poll_task "$tid"); [ "$st" = succeeded ] && ok "ext cluster created (extensions:[pgvector,age])" || bad "ext cluster task=$st"
  EPROJ=$(curl -sf "$BASE/storage-pools/$POOL/clusters" | jq -r '.clusters[]|select(.name=="extproj")|.id')
  EDEF=$(curl -sf "$BASE/storage-pools/$POOL/clusters/$EPROJ" | jq -r '.default_branch.branch_id')
  [ -n "$EPROJ" ] && [ -n "$EDEF" ] && ok "ext cluster materialized ($EPROJ / $EDEF)" || bad "ext cluster not materialized"

  # The endpoint mounts the cluster's set without any per-endpoint opt-in.
  IFS='|' read -r code loc tid < <(create_task POST "/storage-pools/$POOL/clusters/$EPROJ/branches/$EDEF/endpoints" "{\"role\":\"read_write\",\"password\":\"$EP_PASSWORD\"}")
  [ "$code" = 202 ] && ok "createEndpoint 202" || bad "createEndpoint(ext) code=$code"
  st=$(poll_task "$tid"); [ "$st" = succeeded ] && ok "create_endpoint(ext) succeeded" || bad "create_endpoint(ext) task=$st"
  EEP=$(curl -sf "$BASE/storage-pools/$POOL/clusters/$EPROJ/branches/$EDEF/endpoints" | jq -r '.endpoints[0].id')
  EADDR=$(curl -sf "$BASE/storage-pools/$POOL/clusters/$EPROJ/branches/$EDEF/endpoints/$EEP" | jq -r '.address // ""')
  EPOD=$(wait_ready_pod "$EEP")
  [ -n "$EPOD" ] && ok "ext endpoint pod Ready ($EPOD)" || bad "ext endpoint pod not Ready"

  # Both ImageVolumes are mounted, each at /extensions/<name> with its control file.
  kubectl -n "$NS" exec "$EPOD" -- test -f /extensions/pgvector/share/postgresql/extension/vector.control \
    && ok "pgvector ImageVolume mounted (/extensions/pgvector)" || bad "pgvector ImageVolume not mounted"
  kubectl -n "$NS" exec "$EPOD" -- test -f /extensions/age/share/postgresql/extension/age.control \
    && ok "age ImageVolume mounted (/extensions/age)" || bad "age ImageVolume not mounted"

  # CREATE EXTENSION + store real vector data + a nearest-neighbour query that
  # exercises the type end-to-end (insert -> CHECKPOINT through the storage stack
  # -> ORDER BY <-> read back). The NN of [3,3,3] is [2,2,2] (id=2) at distance
  # sqrt(3) = 1.732; the literal <-> also sanity-checks the operator (5.196).
  out=$(psql_pod "$EPOD" 127.0.0.1 55433 <<'SQL' 2>&1
CREATE EXTENSION IF NOT EXISTS vector;
DROP TABLE IF EXISTS items;
CREATE TABLE items (id serial PRIMARY KEY, embedding vector(3));
INSERT INTO items (embedding) VALUES ('[1,1,1]'), ('[2,2,2]'), ('[9,9,9]');
CHECKPOINT;
SELECT id || '|' || round((embedding <-> '[3,3,3]')::numeric, 3)
  FROM items ORDER BY embedding <-> '[3,3,3]' LIMIT 1;
SELECT round(('[1,2,3]'::vector <-> '[4,5,6]'::vector)::numeric, 3);
SQL
)
  { echo "$out" | grep -q '^2|1.732$' && echo "$out" | grep -q '^5.196$'; } \
    && ok "pgvector data I/O: NN([3,3,3])=id2 dist=1.732 + literal <-> =5.196" \
    || bad "pgvector data query (got: $(echo "$out" | tail -2 | tr '\n' ' '))"

  # AGE (openCypher) on the SAME endpoint. Non-preload: load it per session with
  # LOAD 'age'. Create a graph, a cypher() CREATE that writes a vertex (through
  # the storage stack after CHECKPOINT), then a cypher() MATCH that reads the
  # property back — asserting the value ('ada') round-trips.
  out=$(psql_pod "$EPOD" 127.0.0.1 55433 <<'SQL' 2>&1
CREATE EXTENSION IF NOT EXISTS age;
LOAD 'age';
SET search_path = ag_catalog, "$user", public;
SELECT create_graph('amp_age_probe');
SELECT * FROM cypher('amp_age_probe', $$ CREATE (:Person {name: 'ada'}) $$) AS (v agtype);
CHECKPOINT;
SELECT * FROM cypher('amp_age_probe', $$ MATCH (p:Person) RETURN p.name $$) AS (name agtype);
SQL
)
  echo "$out" | grep -q '^"ada"$' \
    && ok "age data I/O: LOAD 'age' + create_graph + cypher CREATE/MATCH round-trip (ada)" \
    || bad "age cypher query (got: $(echo "$out" | tail -2 | tr '\n' ' '))"

  hdr "externalized extensions: pg_cron opt-in preloads, CREATE EXTENSION, a scheduled job runs"

  # Its own cluster, opting into pg_cron at creation. It is a preload extension,
  # so the chart appends it to shared_preload_libraries and every endpoint comes
  # up with the scheduler running.
  IFS='|' read -r code loc tid < <(create_task POST "/storage-pools/$POOL/clusters" '{"name":"cronproj","extensions":["pg_cron"]}')
  st=$(poll_task "$tid"); [ "$st" = succeeded ] && ok "pg_cron cluster created (extensions:[pg_cron])" || bad "pg_cron cluster task=$st"
  CPROJ=$(curl -sf "$BASE/storage-pools/$POOL/clusters" | jq -r '.clusters[]|select(.name=="cronproj")|.id')
  CDEF=$(curl -sf "$BASE/storage-pools/$POOL/clusters/$CPROJ" | jq -r '.default_branch.branch_id')
  [ -n "$CPROJ" ] && [ -n "$CDEF" ] && ok "pg_cron cluster materialized ($CPROJ / $CDEF)" || bad "pg_cron cluster not materialized"

  # The endpoint mounts the cluster's pg_cron without any per-endpoint opt-in.
  IFS='|' read -r code loc tid < <(create_task POST "/storage-pools/$POOL/clusters/$CPROJ/branches/$CDEF/endpoints" "{\"role\":\"read_write\",\"password\":\"$EP_PASSWORD\"}")
  [ "$code" = 202 ] && ok "createEndpoint 202" || bad "createEndpoint(pg_cron) code=$code"
  st=$(poll_task "$tid"); [ "$st" = succeeded ] && ok "create_endpoint(pg_cron) succeeded" || bad "create_endpoint(pg_cron) task=$st"
  CEP=$(curl -sf "$BASE/storage-pools/$POOL/clusters/$CPROJ/branches/$CDEF/endpoints" | jq -r '.endpoints[0].id')
  CPOD=$(wait_ready_pod "$CEP")
  [ -n "$CPOD" ] && ok "pg_cron endpoint pod Ready ($CPOD)" || bad "pg_cron endpoint pod not Ready"

  kubectl -n "$NS" exec "$CPOD" -- test -f /extensions/pg_cron/share/postgresql/extension/pg_cron.control \
    && ok "pg_cron ImageVolume mounted (/extensions/pg_cron)" || bad "pg_cron ImageVolume not mounted"

  # The preload entry must be present, else CREATE EXTENSION errors and no job runs.
  spl=$(psql_pod "$CPOD" 127.0.0.1 55433 <<<'SHOW shared_preload_libraries;' 2>&1)
  if echo "$spl" | grep -q 'pg_cron'; then
    ok "shared_preload_libraries includes pg_cron ($spl)"
  else
    bad "pg_cron not in shared_preload_libraries (got: $spl)"
  fi

  # CREATE EXTENSION + schedule a seconds-granularity job that stamps a marker
  # row. cron.database_name defaults to postgres (the db we connect to), so the
  # job's write is visible here. Capture the jobid: cron.job_run_details is keyed
  # by jobid and has NO jobname column, and the cron.unschedule below drops the
  # cron.job row (so a name->jobid join wouldn't survive it either).
  out=$(psql_pod "$CPOD" 127.0.0.1 55433 <<'SQL' 2>&1
CREATE EXTENSION IF NOT EXISTS pg_cron;
DROP TABLE IF EXISTS cron_marker;
CREATE TABLE cron_marker (id bigserial PRIMARY KEY, at timestamptz NOT NULL DEFAULT now());
SELECT 'JOBID:'||cron.schedule('amp_pgcron_probe', '5 seconds', $$INSERT INTO cron_marker DEFAULT VALUES$$);
SQL
)
  jobid=$(printf '%s\n' "$out" | sed -n 's/^JOBID:\([0-9]*\)$/\1/p')
  [ -n "$jobid" ] \
    && ok "CREATE EXTENSION pg_cron + cron.schedule (jobid=$jobid)" \
    || bad "pg_cron CREATE EXTENSION / schedule failed (got: $(echo "$out" | tr '\n' ' '))"

  # Poll cron's own bookkeeping (by jobid) for a completed run.
  ran=0
  for ((i=0; i<40; i++)); do
    n=$(psql_pod "$CPOD" 127.0.0.1 55433 <<<"SELECT count(*) FROM cron.job_run_details WHERE jobid=${jobid:-0} AND status='succeeded';" 2>/dev/null | tr -d '[:space:]')
    if [ "${n:-0}" -gt 0 ] 2>/dev/null; then ran=1; break; fi
    sleep 3
  done
  if [ "$ran" = 1 ]; then
    ok "pg_cron scheduled job ran to success"
  else
    # No succeeded run — dump why: whether the scheduler fired the job at all
    # (cron.job_run_details rows + status/return_message), where it tried to
    # connect (cron.job.nodename:nodeport), and the endpoint's cron log lines.
    echo "--- pg_cron diagnostics (no succeeded run within ~120s) ---"
    psql_pod "$CPOD" 127.0.0.1 55433 <<<"SELECT jobid, schedule, active, database, username, nodename, nodeport FROM cron.job WHERE jobid=${jobid:-0};" 2>&1 || true
    psql_pod "$CPOD" 127.0.0.1 55433 <<<"SELECT runid, status, return_message, start_time, end_time FROM cron.job_run_details WHERE jobid=${jobid:-0} ORDER BY runid;" 2>&1 || true
    kubectl -n "$NS" logs "$CPOD" -c endpoint --tail=200 2>&1 | grep -iE 'cron|background worker|fatal|error' || true
    echo "--- end pg_cron diagnostics ---"
    bad "pg_cron job never completed within ~120s"
  fi

  psql_pod "$CPOD" 127.0.0.1 55433 <<<"SELECT cron.unschedule('amp_pgcron_probe');" >/dev/null 2>&1

  # KEEP=1 leaves the pg_cron cluster/endpoint up for hands-on inspection.
  if [ "$KEEP" = "1" ]; then
    echo "  KEEP set: leaving pg_cron cluster $CPROJ (endpoint pod $CPOD) up for inspection"
  else
    IFS='|' read -r code loc tid < <(create_task DELETE "/storage-pools/$POOL/clusters/$CPROJ")
    st=$(poll_task "$tid"); [ "$st" = succeeded ] && ok "pg_cron cluster cascade deleted" || bad "pg_cron cluster delete task=$st"
  fi

  hdr "externalized extensions: hexabridge opt-in mounts, CREATE EXTENSION, Oracle-compat fn"

  # Its own cluster, opting into hexabridge at creation. Like pg_cron it is a
  # preload extension (its _PG_init registers a dbms_scheduler background worker),
  # so the controller appends it to shared_preload_libraries; every endpoint on
  # the cluster comes up with the ImageVolume mounted and the library preloaded.
  IFS='|' read -r code loc tid < <(create_task POST "/storage-pools/$POOL/clusters" '{"name":"hbproj","extensions":["hexabridge"]}')
  st=$(poll_task "$tid"); [ "$st" = succeeded ] && ok "hexabridge cluster created (extensions:[hexabridge])" || bad "hexabridge cluster task=$st"
  HPROJ=$(curl -sf "$BASE/storage-pools/$POOL/clusters" | jq -r '.clusters[]|select(.name=="hbproj")|.id')
  HDEF=$(curl -sf "$BASE/storage-pools/$POOL/clusters/$HPROJ" | jq -r '.default_branch.branch_id')
  [ -n "$HPROJ" ] && [ -n "$HDEF" ] && ok "hexabridge cluster materialized ($HPROJ / $HDEF)" || bad "hexabridge cluster not materialized"

  # The endpoint mounts the cluster's hexabridge without any per-endpoint opt-in.
  IFS='|' read -r code loc tid < <(create_task POST "/storage-pools/$POOL/clusters/$HPROJ/branches/$HDEF/endpoints" "{\"role\":\"read_write\",\"password\":\"$EP_PASSWORD\"}")
  [ "$code" = 202 ] && ok "createEndpoint 202" || bad "createEndpoint(hexabridge) code=$code"
  st=$(poll_task "$tid"); [ "$st" = succeeded ] && ok "create_endpoint(hexabridge) succeeded" || bad "create_endpoint(hexabridge) task=$st"
  HEP=$(curl -sf "$BASE/storage-pools/$POOL/clusters/$HPROJ/branches/$HDEF/endpoints" | jq -r '.endpoints[0].id')
  HPOD=$(wait_ready_pod "$HEP")
  [ -n "$HPOD" ] && ok "hexabridge endpoint pod Ready ($HPOD)" || bad "hexabridge endpoint pod not Ready"

  # The ImageVolume is mounted at /extensions/hexabridge with the control file.
  kubectl -n "$NS" exec "$HPOD" -- test -f /extensions/hexabridge/share/postgresql/extension/hexabridge.control \
    && ok "hexabridge ImageVolume mounted (/extensions/hexabridge)" || bad "hexabridge ImageVolume not mounted"

  # CREATE EXTENSION (superuser-only; the smoke connection is the superuser) +
  # two deterministic Oracle-compat calls that exercise the loaded .so: nanvl
  # (returns the first arg when it is not NaN -> 1) and utl_raw over a cast
  # (length of the 2-byte raw for 'AB' -> 2). Both are IMMUTABLE/pure, so the
  # expected output is fixed.
  out=$(psql_pod "$HPOD" 127.0.0.1 55433 <<'SQL' 2>&1
CREATE EXTENSION IF NOT EXISTS hexabridge;
SELECT 'NANVL:'||hexabridge.nanvl(1::float8, 2::float8);
SELECT 'RAWLEN:'||utl_raw.length(utl_raw.cast_to_raw('AB'));
SQL
)
  { echo "$out" | grep -q '^NANVL:1$' && echo "$out" | grep -q '^RAWLEN:2$'; } \
    && ok "hexabridge CREATE EXTENSION + nanvl(1,2)=1 + utl_raw.length('AB')=2" \
    || bad "hexabridge function checks failed (got: $(echo "$out" | tr '\n' ' '))"

  # KEEP=1 leaves the hexabridge cluster/endpoint up for hands-on inspection.
  if [ "$KEEP" = "1" ]; then
    echo "  KEEP set: leaving hexabridge cluster $HPROJ (endpoint pod $HPOD) up for inspection"
  else
    IFS='|' read -r code loc tid < <(create_task DELETE "/storage-pools/$POOL/clusters/$HPROJ")
    st=$(poll_task "$tid"); [ "$st" = succeeded ] && ok "hexabridge cluster cascade deleted" || bad "hexabridge cluster delete task=$st"
  fi

  hdr "extension registry: an admin registers a third-party image, a branch mounts it"

  # The registration API end to end, against the real ImageVolume path. Nothing
  # in the product knows this image — it reaches amp only because we PUT it here,
  # from test/integration/compute/ext_fixtures/postgis.json. The reference lives
  # in test fixtures and docs, never in the product catalog.
  #
  # It exercises three things no built-in extension does: a non-amp in-image
  # layout (share + lib, not share/postgresql + lib/postgresql), the bundled
  # geos/proj/gdal reaching the loader via EXTENSION_LIBRARY_PATH, and the
  # FMGR_ABI_EXTRA waiver without which a stock-PG18 build refuses to load.
  GISFIX="$(dirname "$0")/../test/integration/compute/ext_fixtures/postgis.json"
  if [ ! -f "$GISFIX" ]; then
    bad "postgis fixture missing at $GISFIX"
  else
    # $USER was created with {} — not an admin — so it is exactly the caller that
    # must be refused: registration decides what image runs inside an endpoint pod
    # and can waive the server's ABI check.
    code=$(curl -s -o /dev/null -w '%{http_code}' -X PUT -H 'content-type: application/json' \
      --data-binary "@$GISFIX" "$BASE/extensions/postgis?user_id=$USER")
    [ "$code" = 403 ] && ok "registerExtension as non-admin -> 403" || bad "non-admin register code=$code (want 403)"

    post_id GISADMIN "$BASE/users" '{"is_admin":true}'
    [ -n "$GISADMIN" ] && ok "createUser(admin) 201 ($GISADMIN)" || bad "createUser(admin) $LAST_ERR"
    code=$(curl -s -o /dev/null -w '%{http_code}' -X PUT -H 'content-type: application/json' \
      --data-binary "@$GISFIX" "$BASE/extensions/postgis?user_id=$GISADMIN")
    [ "$code" = 200 ] && ok "registerExtension as admin -> 200" || bad "admin register code=$code (want 200)"

    # It shows up in the catalog, marked as not built-in, with the image we sent.
    # Read the expected reference back out of the fixture rather than repeating
    # it: the fixture is meant to be the only place in the repo that names a
    # third-party extension image.
    GISIMG=$(jq -r .image "$GISFIX")
    gis=$(curl -sf "$BASE/extensions" | jq -r '.extensions[]|select(.name=="postgis")|"\(.builtin)|\(.image)"')
    [ "$gis" = "false|$GISIMG" ] \
      && ok "listExtensions reports postgis as registered ($gis)" || bad "listExtensions postgis=$gis"

    # And a cluster may now opt into it by name, exactly like a built-in.
    IFS='|' read -r code loc tid < <(create_task POST "/storage-pools/$POOL/clusters" '{"name":"gisproj","extensions":["postgis"]}')
    st=$(poll_task "$tid"); [ "$st" = succeeded ] && ok "cluster created with the registered extension" || bad "gis cluster task=$st"
    GPROJ=$(curl -sf "$BASE/storage-pools/$POOL/clusters" | jq -r '.clusters[]|select(.name=="gisproj")|.id')
    GDEF=$(curl -sf "$BASE/storage-pools/$POOL/clusters/$GPROJ" | jq -r '.default_branch.branch_id')

    IFS='|' read -r code loc tid < <(create_task POST "/storage-pools/$POOL/clusters/$GPROJ/branches/$GDEF/endpoints" "{\"role\":\"read_write\",\"password\":\"$EP_PASSWORD\"}")
    st=$(poll_task "$tid"); [ "$st" = succeeded ] && ok "create_endpoint(postgis) succeeded" || bad "create_endpoint(postgis) task=$st"
    GEP=$(curl -sf "$BASE/storage-pools/$POOL/clusters/$GPROJ/branches/$GDEF/endpoints" | jq -r '.endpoints[0].id')
    GPOD=$(wait_ready_pod "$GEP")
    [ -n "$GPOD" ] && ok "postgis endpoint pod Ready ($GPOD)" || bad "postgis endpoint pod not Ready"

    # The registered layout reached the pod: the CNPG tree is share/extension,
    # not share/postgresql/extension, and the bundled libs and PROJ database are
    # things the endpoint image does not carry.
    kubectl -n "$NS" exec "$GPOD" -- test -f /extensions/postgis/share/extension/postgis.control \
      && ok "postgis ImageVolume mounted with the registered layout" || bad "postgis ImageVolume not mounted"
    kubectl -n "$NS" exec "$GPOD" -- test -f /extensions/postgis/system/libgeos_c.so.1 \
      && ok "bundled system libraries mounted (/extensions/postgis/system)" || bad "postgis system libs not mounted"
    kubectl -n "$NS" exec "$GPOD" -- test -f /extensions/postgis/share/proj/proj.db \
      && ok "PROJ database mounted (/extensions/postgis/share/proj)" || bad "postgis PROJ database not mounted"

    # CREATE EXTENSION is the ABI-waiver assertion: reaching any geometry
    # function proves the stock-PG18 .so loaded. ST_Transform is the PROJ_DATA
    # assertion — 4326 -> 3857 sends longitude 180 to x = 20037508.34.
    out=$(psql_pod "$GPOD" 127.0.0.1 55433 <<'SQL' 2>&1
CREATE EXTENSION IF NOT EXISTS postgis;
SELECT 'VERSION:'||extversion FROM pg_extension WHERE extname='postgis';
SELECT 'AREA:'||round(ST_Area(ST_GeomFromText('POLYGON((0 0,1 0,1 1,0 1,0 0))'))::numeric, 3);
SELECT 'MERCX:'||round(ST_X(ST_Transform(ST_SetSRID(ST_MakePoint(180, 0), 4326), 3857))::numeric, 2);
SQL
)
    { echo "$out" | grep -q '^VERSION:3.6.2$' && echo "$out" | grep -q '^AREA:1.000$' \
      && echo "$out" | grep -q '^MERCX:20037508.34$'; } \
      && ok "postgis CREATE EXTENSION (v3.6.2) + ST_Area=1 + ST_Transform x=20037508.34" \
      || bad "postgis checks failed (got: $(echo "$out" | tr '\n' ' '))"

    # Deregistering it now must be refused: the branch above still opts in, and
    # dropping the row would strip the mount on its next deploy.
    code=$(curl -s -o /dev/null -w '%{http_code}' -X DELETE "$BASE/extensions/postgis?user_id=$GISADMIN")
    [ "$code" = 409 ] && ok "deregisterExtension while a branch uses it -> 409" || bad "in-use deregister code=$code (want 409)"

    if [ "$KEEP" = "1" ]; then
      echo "  KEEP set: leaving postgis cluster $GPROJ (endpoint pod $GPOD) up for inspection"
    else
      IFS='|' read -r code loc tid < <(create_task DELETE "/storage-pools/$POOL/clusters/$GPROJ")
      st=$(poll_task "$tid"); [ "$st" = succeeded ] && ok "postgis cluster cascade deleted" || bad "postgis cluster delete task=$st"
      # With nothing referencing it, the registration goes.
      code=$(curl -s -o /dev/null -w '%{http_code}' -X DELETE "$BASE/extensions/postgis?user_id=$GISADMIN")
      [ "$code" = 204 ] && ok "deregisterExtension once unused -> 204" || bad "deregister code=$code (want 204)"
    fi
  fi

  hdr "externalized extensions: addBranchExtensions unions the set + redeploys the branch's live endpoints"

  # The other half of the API: add pg_cron to the extproj default branch that
  # already has a LIVE endpoint (EEP, mounting pgvector+age). add_branch_extensions
  # unions the name into the branch set and helm-upgrades every live endpoint on
  # the branch, recreating the pod with the new ImageVolume and (pg_cron being a
  # preload extension) a new shared_preload_libraries. This is the operationally
  # risky path — a stop-and-replace of running computes (Recreate, since an
  # endpoint is single-writer) — that createCluster does not cover.
  IFS='|' read -r code loc tid < <(create_task POST "/storage-pools/$POOL/clusters/$EPROJ/branches/$EDEF/extensions" '{"extensions":["pg_cron"]}')
  [ "$code" = 202 ] && ok "addBranchExtensions 202" || bad "addBranchExtensions code=$code"
  st=$(poll_task "$tid"); [ "$st" = succeeded ] && ok "add_branch_extensions succeeded" || bad "add_branch_extensions task=$st"

  # getBranch reports the unioned, sorted, de-duplicated set.
  exts=$(curl -sf "$BASE/storage-pools/$POOL/clusters/$EPROJ/branches/$EDEF" | jq -r '(.extensions//[])|sort|join(",")')
  [ "$exts" = "age,pg_cron,pgvector" ] && ok "getBranch reports unioned extensions ($exts)" \
    || bad "getBranch extensions=$exts (want age,pg_cron,pgvector)"

  # The redeploy recreates the pod (new name after the helm upgrade); it comes
  # back Ready and now mounts pg_cron alongside the originals.
  EPOD=$(wait_ready_pod "$EEP")
  [ -n "$EPOD" ] && ok "endpoint redeployed and Ready ($EPOD)" || bad "endpoint not Ready after addBranchExtensions"
  kubectl -n "$NS" exec "$EPOD" -- test -f /extensions/pg_cron/share/postgresql/extension/pg_cron.control \
    && ok "pg_cron ImageVolume mounted after addBranchExtensions" || bad "pg_cron ImageVolume not mounted after add"

  # The redeploy replaced the pod. Adding an extension is a supported operation a
  # customer is invited to perform, so the connection string they already hold has
  # to keep working: the recorded address must come back byte-identical. On a
  # ClusterIP deployment it must also be the Service DNS rather than anything
  # pod-scoped (CLOUDGA-36993).
  addr=$(curl -sf "$BASE/storage-pools/$POOL/clusters/$EPROJ/branches/$EDEF/endpoints/$EEP" | jq -r '.address // ""')
  want=$(want_endpoint_addr "$EEP")
  [ -n "$EADDR" ] && [ "$addr" = "$EADDR" ] && { [ -z "$want" ] || [ "$addr" = "$want" ]; } \
    && ok "endpoint address survived the redeploy ($addr)" \
    || bad "endpoint address $addr after the redeploy (was $EADDR, want ${want:-it unchanged})"

  hdr "externalized extensions: a fork inherits its parent branch's opted-in set"

  # A fork inherits its parent branch's extension set (the createCluster seed +
  # the addBranchExtensions add), so a fresh endpoint on the fork mounts the set
  # with no per-endpoint opt-in. Fork at the parent's current LSN (captured from
  # its live endpoint) rather than "latest", which is not settled straight after
  # the redeploy above. This is the create-cluster-with-extensions -> fork path;
  # it needs the #440 fix (meko#500/#531/#552: a fork endpoint crash-looped when
  # the ancestor's applied_gc_cutoff_lsn was still NULL), now landed.
  FLSN=$(printf 'SELECT pg_current_wal_lsn();' | psql_pod "$EPOD" 127.0.0.1 55433 2>/dev/null | tr -d '[:space:]')
  [ -n "$FLSN" ] && ok "captured parent fork LSN ($FLSN)" || bad "could not capture parent fork LSN"
  code=""
  for ((r=0; r<8; r++)); do
    IFS='|' read -r code loc tid < <(create_task POST "/storage-pools/$POOL/clusters/$EPROJ/branches" "{\"name\":\"extb1\",\"parent_branch_id\":\"$EDEF\",\"fork_point_lsn\":\"$FLSN\"}")
    [ "$code" = 202 ] && break
    sleep 5
  done
  [ "$code" = 202 ] && ok "ext fork extb1 accepted 202" || bad "ext fork code=$code"
  st=$(poll_task "$tid"); [ "$st" = succeeded ] && ok "ext fork extb1 succeeded" || bad "ext fork task=$st"
  EB1=$(curl -sf "$BASE/storage-pools/$POOL/clusters/$EPROJ/branches" | jq -r '.branches[]|select(.name=="extb1")|.branch_id')
  [ -n "$EB1" ] && ok "ext fork extb1 materialized ($EB1)" || bad "ext fork extb1 missing"

  # getBranch on the fork reports the inherited set (parent = age,pg_cron,pgvector).
  fexts=$(curl -sf "$BASE/storage-pools/$POOL/clusters/$EPROJ/branches/$EB1" | jq -r '(.extensions//[])|sort|join(",")')
  [ "$fexts" = "age,pg_cron,pgvector" ] && ok "fork inherited the parent's set ($fexts)" \
    || bad "fork extensions=$fexts (want age,pg_cron,pgvector)"

  # A fresh endpoint on the fork mounts the inherited set with no opt-in.
  IFS='|' read -r code loc tid < <(create_task POST "/storage-pools/$POOL/clusters/$EPROJ/branches/$EB1/endpoints" "{\"role\":\"read_write\",\"password\":\"$EP_PASSWORD\"}")
  [ "$code" = 202 ] && ok "fork createEndpoint 202" || bad "fork createEndpoint code=$code"
  st=$(poll_task "$tid"); [ "$st" = succeeded ] && ok "fork endpoint (no opt-in) succeeded" || bad "fork endpoint task=$st"
  FEP=$(curl -sf "$BASE/storage-pools/$POOL/clusters/$EPROJ/branches/$EB1/endpoints" | jq -r '.endpoints[0].id')
  FPOD=$(wait_ready_pod "$FEP")
  [ -n "$FPOD" ] && ok "fork endpoint pod Ready ($FPOD)" || bad "fork endpoint pod not Ready"
  kubectl -n "$NS" exec "$FPOD" -- test -f /extensions/pgvector/share/postgresql/extension/vector.control \
    && ok "fork endpoint inherited pgvector mount" || bad "fork endpoint did NOT inherit pgvector"

  # Tear down the ext cluster (cascade retires all endpoints, drops branches).
  IFS='|' read -r code loc tid < <(create_task DELETE "/storage-pools/$POOL/clusters/$EPROJ")
  st=$(poll_task "$tid"); [ "$st" = succeeded ] && ok "ext cluster cascade deleted" || bad "ext cluster delete task=$st"

  hdr "documentdb engine: the gateway serves the wire protocol and logs in the default user"

  # engine:"documentdb" seeds documentdb + pgvector + pg_cron on the default
  # branch, and an endpoint that mounts documentdb renders the gateway sidecar.
  IFS='|' read -r code loc tid < <(create_task POST "/storage-pools/$POOL/clusters" '{"name":"docproj","engine":"documentdb"}')
  st=$(poll_task "$tid"); [ "$st" = succeeded ] && ok "documentdb cluster created (engine:documentdb)" || bad "documentdb cluster task=$st"
  DPROJ=$(curl -sf "$BASE/storage-pools/$POOL/clusters" | jq -r '.clusters[]|select(.name=="docproj")|.id')
  DDEF=$(curl -sf "$BASE/storage-pools/$POOL/clusters/$DPROJ" | jq -r '.default_branch.branch_id')
  [ -n "$DPROJ" ] && [ -n "$DDEF" ] && ok "documentdb cluster materialized ($DPROJ / $DDEF)" || bad "documentdb cluster not materialized"

  IFS='|' read -r code loc tid < <(create_task POST "/storage-pools/$POOL/clusters/$DPROJ/branches/$DDEF/endpoints" "{\"role\":\"read_write\",\"password\":\"$EP_PASSWORD\"}")
  [ "$code" = 202 ] && ok "createEndpoint 202" || bad "createEndpoint(documentdb) code=$code"
  st=$(poll_task "$tid"); [ "$st" = succeeded ] && ok "create_endpoint(documentdb) succeeded" || bad "create_endpoint(documentdb) task=$st"
  DEP=$(curl -sf "$BASE/storage-pools/$POOL/clusters/$DPROJ/branches/$DDEF/endpoints" | jq -r '.endpoints[0].id')
  DPOD=$(wait_ready_pod "$DEP")
  [ -n "$DPOD" ] && ok "documentdb endpoint pod Ready ($DPOD)" || bad "documentdb endpoint pod not Ready"

  # wait_ready_pod reads only the first container. The gateway is a second
  # one, and a pod whose sidecar never came up would pass that check.
  gwready=""
  for ((i=0; i<40; i++)); do
    gwready=$(kubectl -n "$NS" get pod "$DPOD" -o jsonpath='{.status.containerStatuses[?(@.name=="documentdb-gateway")].ready}' 2>/dev/null)
    [ "$gwready" = true ] && break
    sleep 3
  done
  if [ "$gwready" = true ]; then
    ok "documentdb-gateway sidecar Ready"
  else
    bad "documentdb-gateway sidecar not Ready (ready=$gwready)"
    # This block deletes its cluster at the end, before the job's failure dump
    # runs, so the evidence has to be printed here: the gateway retries until the
    # schema exists, and compute_ctl's post_apply_config says why it does not.
    echo "--- documentdb diagnostics ---"
    kubectl -n "$NS" logs "$DPOD" -c documentdb-gateway --tail=20 2>&1 || true
    kubectl -n "$NS" logs "$DPOD" -c endpoint 2>&1 | grep -E 'post_apply_config|documentdb' | tail -20 || true
    echo "--- end documentdb diagnostics ---"
  fi

  # The API reports the gateway address where the Service publishes the
  # gateway port, and only there (deploy.DocumentDBGatewayReachable).
  daddr=$(curl -sf "$BASE/storage-pools/$POOL/clusters/$DPROJ/branches/$DDEF/endpoints/$DEP" | jq -r '.documentdb_address // ""')
  want=$(want_endpoint_addr "$DEP")
  if [ -n "$want" ]; then
    [ "$daddr" = "${want%:*}:10260" ] && ok "documentdb_address = $daddr" || bad "documentdb_address=$daddr (want ${want%:*}:10260)"
  else
    ok "documentdb_address not checked: the Service is not a plain ClusterIP (got '$daddr')"
  fi

  # Nobody runs CREATE EXTENSION: compute_ctl creates a `documentdb` database
  # and the extension in it whenever pg_documentdb is preloaded. The gateway
  # being Ready above already implies it — it binds only once the schema exists.
  out=$(kubectl -n "$NS" exec -i "$DPOD" -c endpoint -- \
    "$PSQL_BIN" -h 127.0.0.1 -p 55433 -U cloud_admin -d documentdb -tA -v ON_ERROR_STOP=1 \
    -c "SELECT 'EXT:'||extversion FROM pg_extension WHERE extname = 'documentdb'" 2>&1)
  echo "$out" | grep -q '^EXT:0.116-0$' \
    && ok "documentdb 0.116-0 created by compute_ctl in database documentdb" \
    || bad "documentdb extension not in database documentdb (got: $(echo "$out" | tail -2 | tr '\n' ' '))"

  # The client reaches the gateway through the endpoint's Service.
  kubectl -n "$NS" port-forward "svc/yb-ep-$DEP" "${GW_PF_PORT}:10260" >/tmp/smoke-gw-pf.log 2>&1 &
  GW_PF_PID=$!
  for ((i=0; i<30; i++)); do (exec 3<>"/dev/tcp/127.0.0.1/$GW_PF_PORT") 2>/dev/null && break; sleep 1; done

  # The gateway self-signs its certificate, so a client cannot verify it yet
  # (https://github.com/yugabyte/meko/issues/1509): TLS on, verification off.
  gw_uri() {
    printf 'mongodb://cloud_admin:%s@127.0.0.1:%s/?tls=true&tlsAllowInvalidCertificates=true&directConnection=true&authMechanism=SCRAM-SHA-256&serverSelectionTimeoutMS=30000' \
      "$1" "$GW_PF_PORT"
  }
  # pymongo from the pinned client image; the Python arrives on stdin.
  pymongo_run() {
    docker run --rm -i --network host -e URI="$1" --entrypoint python3 "$DOCUMENTDB_CLIENT_IMAGE" - 2>&1
  }

  # Every step goes client -> gateway -> documentdb on the compute (over the
  # pod's loopback) -> amp storage. The filter, sort, $inc and $group run in
  # documentdb, not in the driver.
  out=$(pymongo_run "$(gw_uri "$EP_PASSWORD")" <<'PY'
import os, pymongo
c = pymongo.MongoClient(os.environ["URI"])
print("PING:%d" % c.admin.command("ping")["ok"])
coll = c["amp_smoke"]["probe"]
coll.drop()
coll.insert_many([{"_id": i, "a": i, "tag": "t%d" % (i % 2)} for i in range(1, 6)])
coll.create_index([("a", pymongo.ASCENDING)])
print("FIND:%s" % ",".join(str(d["_id"]) for d in coll.find({"a": {"$gte": 3}}).sort("a", -1)))
print("UPDATE:%d" % coll.update_one({"_id": 1}, {"$inc": {"a": 100}}).modified_count)
agg = {d["_id"]: d["s"] for d in coll.aggregate([{"$group": {"_id": "$tag", "s": {"$sum": "$a"}}}])}
print("AGG:t0=%d,t1=%d" % (agg["t0"], agg["t1"]))
print("DELETE:%d" % coll.delete_one({"_id": 5}).deleted_count)
print("COUNT:%d" % coll.count_documents({}))
PY
)
  if echo "$out" | grep -q '^PING:1$' \
     && echo "$out" | grep -q '^FIND:5,4,3$' \
     && echo "$out" | grep -q '^UPDATE:1$' \
     && echo "$out" | grep -q '^AGG:t0=6,t1=109$' \
     && echo "$out" | grep -q '^DELETE:1$' \
     && echo "$out" | grep -q '^COUNT:4$'; then
    ok "wire protocol as cloud_admin: SCRAM login + insert/index/find/update/aggregate/delete"
  else
    bad "wire-protocol round trip (got: $(echo "$out" | tail -4 | tr '\n' ' '))"
  fi

  # The same documents, read back over SQL in the documentdb database after a
  # CHECKPOINT: what the gateway wrote is what the compute stored.
  out=$(kubectl -n "$NS" exec -i "$DPOD" -c endpoint -- \
    "$PSQL_BIN" -h 127.0.0.1 -p 55433 -U cloud_admin -d documentdb -tA -v ON_ERROR_STOP=1 <<'SQL' 2>&1
CHECKPOINT;
SELECT 'ROWS:'||count(*) FROM documentdb_api.collection('amp_smoke', 'probe');
SQL
)
  echo "$out" | grep -q '^ROWS:4$' && ok "SQL reads the 4 documents the gateway wrote" || bad "SQL row count (got: $(echo "$out" | tail -2 | tr '\n' ' '))"

  # And the login is real: a wrong password is refused.
  out=$(pymongo_run "$(gw_uri wrong-password)" <<'PY'
import os, pymongo
from pymongo.errors import OperationFailure
try:
    pymongo.MongoClient(os.environ["URI"]).admin.command("ping")
    print("AUTH:accepted")
except OperationFailure as e:
    print("AUTH:refused code=%s" % e.code)
PY
)
  echo "$out" | grep -q '^AUTH:refused' && ok "wrong password refused ($(echo "$out" | grep '^AUTH:' | tail -1))" || bad "wrong password not refused (got: $(echo "$out" | tail -2 | tr '\n' ' '))"

  kill "$GW_PF_PID" 2>/dev/null; GW_PF_PID=""
  IFS='|' read -r code loc tid < <(create_task DELETE "/storage-pools/$POOL/clusters/$DPROJ")
  st=$(poll_task "$tid"); [ "$st" = succeeded ] && ok "documentdb cluster cascade deleted" || bad "documentdb cluster delete task=$st"
fi

printf '\n\033[1m================ RESULT: PASS=%d FAIL=%d ================\033[0m\n' "$PASS" "$FAIL"
[ "$FAIL" -eq 0 ]
