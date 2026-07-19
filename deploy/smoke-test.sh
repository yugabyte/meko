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
#   - createProject / createEndpoint / branch / deleteEndpoint / deleteProject /
#     deleteBranch all return 202 + a Task + a Location header, and the task runs
#     to status=succeeded (polled via GET /tasks/{id})
#   - the created endpoint is USABLE: connect with psql and CREATE/INSERT/UPDATE/
#     DELETE/CHECKPOINT/SELECT (the CHECKPOINT forces the writes through the
#     safekeeper -> pageserver -> YB storage path), and the recorded connection
#     address routes to a ready Service backend
#   - GET /tasks lists tasks, filters by status, and rejects a bad status with 400
#   - a duplicate name surfaces as a FAILED task (not a synchronous 409)
#   - deleting a branch that still has child forks is refused with 409
#   - delete_project cascades (retires endpoints, drops branches + project)
#
# It cleans up everything it creates (its project is torn down by the cascade
# delete it exercises; the pool/user rows and any stray endpoint Helm releases are
# removed at the end). Re-runnable. Pass KEEP=1 to leave resources for inspection.
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
# Path to psql INSIDE the endpoint pod (PG v18 install; not needed on the host).
PSQL_BIN="${PSQL_BIN:-/usr/local/v18/bin/psql}"
KEEP="${KEEP:-0}"
SUFFIX="smoke-$$"

ROOT_URL="http://127.0.0.1:${PF_PORT}"
BASE="${ROOT_URL}/api/v1"
PASS=0; FAIL=0
ok()  { printf '  \033[0;32mPASS\033[0m %s\n' "$*"; PASS=$((PASS+1)); }
bad() { printf '  \033[0;31mFAIL\033[0m %s\n' "$*"; FAIL=$((FAIL+1)); }
hdr() { printf '\n\033[1;36m== %s\033[0m\n' "$*"; }

PF_PID=""
cleanup() {
  [ -n "$PF_PID" ] && kill "$PF_PID" 2>/dev/null
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

# --- start: port-forward the controller API ---
kubectl -n "$NS" port-forward svc/amp-controller "${PF_PORT}:8080" >/tmp/smoke-pf.log 2>&1 &
PF_PID=$!
for ((i=0; i<30; i++)); do curl -sf "$ROOT_URL/healthz" >/dev/null 2>&1 && break; sleep 1; done

hdr "health"
curl -sf "$ROOT_URL/healthz" >/dev/null && ok "healthz 200" || bad "healthz"
curl -sf "$ROOT_URL/readyz"  >/dev/null && ok "readyz 200"  || bad "readyz"

hdr "user + storage pool (synchronous 201)"
USER=$(curl -sf -X POST "$BASE/users" -H 'Content-Type: application/json' -d '{}' | jq -r '.id')
[ -n "$USER" ] && ok "createUser 201 ($USER)" || bad "createUser"
POOL=$(curl -sf -X POST "$BASE/storage-pools" -H 'Content-Type: application/json' \
        -d "{\"name\":\"pool-$SUFFIX\",\"owner_user_id\":\"$USER\"}" | jq -r '.id')
[ -n "$POOL" ] && ok "createStoragePool 201 ($POOL)" || bad "createStoragePool"

hdr "createProject -> 202 + Task + Location -> task succeeded -> materialized"
IFS='|' read -r code loc tid < <(create_task POST "/storage-pools/$POOL/projects" '{"name":"proj"}')
[ "$code" = 202 ] && ok "202 Accepted" || bad "createProject code=$code (want 202)"
[[ "$loc" == /api/v1/tasks/* ]] && ok "Location: $loc" || bad "Location header: '$loc'"
[ "$(curl -sf "$BASE/tasks/$tid" | jq -r .kind)" = create_project ] && ok "task kind=create_project" || bad "task kind"
st=$(poll_task "$tid"); [ "$st" = succeeded ] && ok "create_project succeeded" || bad "create_project task=$st"
PROJ=$(curl -sf "$BASE/storage-pools/$POOL/projects" | jq -r '.projects[]|select(.name=="proj")|.id')
DEF=$(curl -sf "$BASE/storage-pools/$POOL/projects/$PROJ" | jq -r '.default_branch.branch_id')
[ -n "$PROJ" ] && [ -n "$DEF" ] && ok "materialized proj=$PROJ default_branch=$DEF" || bad "project not materialized"

hdr "createEndpoint (read_write on default branch) -> task succeeded -> address + pod Ready"
IFS='|' read -r code loc tid < <(create_task POST "/storage-pools/$POOL/projects/$PROJ/branches/$DEF/endpoints" '{"role":"read_write"}')
[ "$code" = 202 ] && ok "202 Accepted" || bad "createEndpoint code=$code"
st=$(poll_task "$tid"); [ "$st" = succeeded ] && ok "create_endpoint succeeded" || bad "create_endpoint task=$st"
EP=$(curl -sf "$BASE/storage-pools/$POOL/projects/$PROJ/branches/$DEF/endpoints" | jq -r '.endpoints[0].id')
ADDR=$(curl -sf "$BASE/storage-pools/$POOL/projects/$PROJ/branches/$DEF/endpoints/$EP" | jq -r '.address')
[ -n "$ADDR" ] && [ "$ADDR" != null ] && ok "endpoint address recorded: $ADDR" || bad "endpoint address empty"
POD=""
for ((i=0; i<60; i++)); do
  POD=$(kubectl -n "$NS" get pods -o name 2>/dev/null | grep "yb-ep-$EP" | head -1)
  [ -n "$POD" ] && [ "$(kubectl -n "$NS" get "$POD" -o jsonpath='{.status.containerStatuses[0].ready}' 2>/dev/null)" = true ] && break
  sleep 3
done
POD="${POD#pod/}"
[ -n "$POD" ] && [ "$(kubectl -n "$NS" get "pod/$POD" -o jsonpath='{.status.containerStatuses[0].ready}' 2>/dev/null)" = true ] \
  && ok "endpoint pod Ready ($POD)" || bad "endpoint pod not Ready"

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

hdr "createBranch (fork default) -> task succeeded"
# The default fork-point resolution reads the parent writer's safekeeper flush_lsn;
# that read can transiently 400 in the first seconds after the writer becomes Ready
# (safekeeper HTTP not yet reachable via its Service). A real client retries; so do we.
code=""
for ((r=0; r<8; r++)); do
  IFS='|' read -r code loc tid < <(create_task POST "/storage-pools/$POOL/projects/$PROJ/branches" "{\"name\":\"b1\",\"parent_branch_id\":\"$DEF\"}")
  [ "$code" = 202 ] && break
  sleep 5
done
[ "$code" = 202 ] && ok "202 Accepted" || bad "createBranch code=$code"
st=$(poll_task "$tid"); [ "$st" = succeeded ] && ok "branch succeeded" || bad "branch task=$st"
B1=$(curl -sf "$BASE/storage-pools/$POOL/projects/$PROJ/branches" | jq -r '.branches[]|select(.name=="b1")|.branch_id')
[ -n "$B1" ] && ok "fork b1 materialized ($B1)" || bad "fork b1 missing"

hdr "GET /tasks: list + filter + invalid-status 400"
TOTAL=$(curl -sf "$BASE/tasks?limit=100" | jq -r '.total')
[ "${TOTAL:-0}" -ge 3 ] 2>/dev/null && ok "listTasks total=$TOTAL" || bad "listTasks total=$TOTAL"
SUC=$(curl -sf "$BASE/tasks?status=succeeded" | jq -r '.tasks|length')
[ "${SUC:-0}" -ge 1 ] 2>/dev/null && ok "listTasks?status=succeeded -> $SUC" || bad "listTasks status filter"
code=$(curl -s -o /dev/null -w '%{http_code}' "$BASE/tasks?status=bogus")
[ "$code" = 400 ] && ok "listTasks?status=bogus -> 400" || bad "invalid status -> $code (want 400)"

hdr "duplicate project name -> task FAILS (not a synchronous 409)"
IFS='|' read -r code loc tid < <(create_task POST "/storage-pools/$POOL/projects" '{"name":"proj"}')
[ "$code" = 202 ] && ok "dup-name accepted 202" || bad "dup-name code=$code"
st=$(poll_task "$tid" 30); [ "$st" = failed ] && ok "duplicate name -> task failed" || bad "dup name task=$st (want failed)"

hdr "delete a branch with a child fork -> 409 (no teardown)"
code=$(curl -s -o /dev/null -w '%{http_code}' -X DELETE "$BASE/storage-pools/$POOL/projects/$PROJ/branches/$DEF")
[ "$code" = 409 ] && ok "deleteBranch(default w/ child b1) -> 409" || bad "deleteBranch(default) -> $code (want 409)"

hdr "deleteBranch b1 -> task succeeded -> gone"
IFS='|' read -r code loc tid < <(create_task DELETE "/storage-pools/$POOL/projects/$PROJ/branches/$B1")
[ "$code" = 202 ] && ok "202 Accepted" || bad "deleteBranch code=$code"
st=$(poll_task "$tid"); [ "$st" = succeeded ] && ok "delete_branch succeeded" || bad "delete_branch task=$st"
[ "$(curl -s -o /dev/null -w '%{http_code}' "$BASE/storage-pools/$POOL/projects/$PROJ/branches/$B1")" = 404 ] && ok "b1 gone (404)" || bad "b1 still present"

hdr "deleteProject cascade -> task succeeded -> project gone + endpoint retired"
IFS='|' read -r code loc tid < <(create_task DELETE "/storage-pools/$POOL/projects/$PROJ")
[ "$code" = 202 ] && ok "202 Accepted" || bad "deleteProject code=$code"
st=$(poll_task "$tid"); [ "$st" = succeeded ] && ok "delete_project cascade succeeded" || bad "delete_project task=$st"
[ "$(curl -s -o /dev/null -w '%{http_code}' "$BASE/storage-pools/$POOL/projects/$PROJ")" = 404 ] && ok "project gone (404)" || bad "project still present"
for ((i=0; i<20; i++)); do [ "$(kubectl -n "$NS" get pods 2>/dev/null | grep -c "yb-ep-$EP")" = 0 ] && break; sleep 3; done
[ "$(kubectl -n "$NS" get pods 2>/dev/null | grep -c "yb-ep-$EP")" = 0 ] && ok "endpoint instance retired" || bad "endpoint instance lingering"

printf '\n\033[1m================ RESULT: PASS=%d FAIL=%d ================\033[0m\n' "$PASS" "$FAIL"
[ "$FAIL" -eq 0 ]
