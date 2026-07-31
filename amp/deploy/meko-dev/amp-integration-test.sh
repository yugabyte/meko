#!/usr/bin/env bash
# amp integration test: drive the amp controller API to provision a fresh
# read_write endpoint, assert it comes up as PostgreSQL 18 and does a real
# write/read, then tear it down. Repeatable and non-destructive to the stack.
#
# Usage:   ./amp-integration-test.sh
# Env:     NS (default amp-dev), KCTX (default meko-dev-eks), PGBIN (auto-detected)
# Exit:    0 on PASS, 1 on FAIL (CI-friendly).
set -uo pipefail

NS="${NS:-amp-dev}"
KCTX="${KCTX:-meko-dev-eks}"
PGBIN="${PGBIN:-$(command -v psql || echo /opt/homebrew/opt/postgresql@16/bin/psql)}"
K="kubectl --context $KCTX -n $NS"
API_PF_PORT="${API_PF_PORT:-18080}"
API="http://127.0.0.1:${API_PF_PORT}/api/v1"
RUN="itest-$(date +%s)"
PF_PIDS=()
POOL="" PROJ="" BR="" EP="" EP_POD="" PGV=""
FAIL=0

log(){ printf '\033[1m[itest]\033[0m %s\n' "$*"; }
err(){ printf '\033[31m[itest FAIL]\033[0m %s\n' "$*"; FAIL=1; }
ok(){  printf '\033[32m[itest OK]\033[0m %s\n' "$*"; }

cleanup(){
  log "cleanup ($RUN)"
  [ -n "$EP" ]   && curl -s -X DELETE "$API/storage-pools/$POOL/projects/$PROJ/branches/$BR/endpoints/$EP" >/dev/null 2>&1 && log "  deleted endpoint $EP"
  [ -n "$PROJ" ] && curl -s -X DELETE "$API/storage-pools/$POOL/projects/$PROJ" >/dev/null 2>&1 && log "  deleted project $PROJ"
  for p in "${PF_PIDS[@]:-}"; do kill "$p" 2>/dev/null; done
}
trap cleanup EXIT

# --- preflight ---
command -v "$PGBIN" >/dev/null 2>&1 || { err "psql not found (set PGBIN)"; exit 1; }
$K get deploy amp-controller >/dev/null 2>&1 || { err "amp-controller not found in $NS ($KCTX)"; exit 1; }
log "controller image: $($K get deploy amp-controller -o jsonpath='{.spec.template.spec.containers[0].image}')"

# --- port-forward the controller API ---
$K port-forward deploy/amp-controller "${API_PF_PORT}:8080" >/tmp/itest-api-pf.log 2>&1 &
PF_PIDS+=($!); sleep 4
curl -sf "http://127.0.0.1:${API_PF_PORT}/healthz" >/dev/null || { err "controller API not reachable"; exit 1; }
ok "controller API reachable"

# --- pick a storage pool (reuse first; pools have no delete API) ---
POOL=$(curl -s "$API/storage-pools" | python3 -c 'import json,sys;d=json.load(sys.stdin);print(d["storage_pools"][0]["id"] if d.get("count") else "")')
[ -n "$POOL" ] || { err "no storage pool exists (create one first)"; exit 1; }
log "using storage pool $POOL"

# --- create project (async: returns a task; default 'main' branch created by it) ---
RESP=$(curl -s -X POST "$API/storage-pools/$POOL/projects" -H 'Content-Type: application/json' -d "{\"name\":\"$RUN\"}")
PROJ=$(echo "$RESP" | python3 -c 'import json,sys;print(json.load(sys.stdin).get("project_id",""))')
TASK=$(echo "$RESP" | python3 -c 'import json,sys;print(json.load(sys.stdin).get("id",""))')
[ -n "$PROJ" ] || { err "createProject failed: $RESP"; exit 1; }
for i in $(seq 1 30); do
  ST=$(curl -s "$API/tasks/$TASK" | python3 -c 'import json,sys;print(json.load(sys.stdin).get("status",""))' 2>/dev/null)
  [ "$ST" = succeeded ] && break
  [ "$ST" = failed ] && { err "create_project task failed"; exit 1; }
  sleep 3
done
read BR PGV < <(curl -s "$API/storage-pools/$POOL/projects/$PROJ/branches" | python3 -c '
import json,sys
d=json.load(sys.stdin); a=d.get("branches", d if isinstance(d,list) else [])
b=next((x for x in a if x.get("is_default")), a[0] if a else {})
print(b.get("branch_id",""), b.get("pg_version",""))')
[ -n "$BR" ] || { err "no branch found for project $PROJ"; exit 1; }
ok "project $PROJ  branch $BR  (branch pg_version=$PGV)"
[ "$PGV" = "18" ] || err "expected branch pg_version 18, got $PGV"

# --- create a read_write endpoint (async: task's target_ref is the endpoint id) ---
RESP=$(curl -s -X POST "$API/storage-pools/$POOL/projects/$PROJ/branches/$BR/endpoints" \
  -H 'Content-Type: application/json' -d '{"role":"read_write"}')
EP=$(echo "$RESP" | python3 -c 'import json,sys;print(json.load(sys.stdin).get("target_ref",""))')
[ -n "$EP" ] || { err "createEndpoint produced no endpoint: $RESP"; exit 1; }
ok "endpoint $EP (provisioning)"

# --- wait for the endpoint pod to be Ready ---
log "waiting for endpoint pod yb-ep-$EP to be Ready (image pull + bootstrap)..."
for i in $(seq 1 40); do
  EP_POD=$($K get pods -o name 2>/dev/null | grep "yb-ep-$EP" | head -1 | cut -d/ -f2)
  if [ -n "$EP_POD" ]; then
    R=$($K get pod "$EP_POD" -o jsonpath='{range .status.containerStatuses[*]}{.ready}{"\n"}{end}' 2>/dev/null)
    tot=$(echo "$R" | grep -c .); rdy=$(echo "$R" | grep -c true)
    echo "  $(date +%T) $EP_POD ready=$rdy/$tot"
    [ "$tot" -gt 0 ] && [ "$rdy" = "$tot" ] && { ok "endpoint pod Ready ($rdy/$tot)"; break; }
  else echo "  $(date +%T) pod not created yet"; fi
  sleep 12
done
[ -n "$EP_POD" ] || { err "endpoint pod never appeared"; exit 1; }

# --- connect to the compute via port-forward and assert PG18 + write/read ---
$K port-forward "pod/$EP_POD" 15433:55433 >/tmp/itest-pg-pf.log 2>&1 &
PF_PIDS+=($!); sleep 4
export PGPASSWORD=cloud_admin
CONN="host=127.0.0.1 port=15433 user=cloud_admin dbname=postgres sslmode=prefer connect_timeout=15"
VER=$($PGBIN "$CONN" -tAc "show server_version" 2>/tmp/itest-pg.err)
if echo "$VER" | grep -q '^18'; then ok "server_version = $VER (PG18)"; else err "expected PG18, got '$VER' ($(cat /tmp/itest-pg.err))"; fi
TBL="itest_${RUN//-/_}"
$PGBIN "$CONN" -q >/dev/null 2>>/tmp/itest-pg.err <<SQL
create table $TBL (id int primary key, v text);
insert into $TBL values (1,'$RUN');
SQL
OUT=$($PGBIN "$CONN" -tAc "select v from $TBL where id=1" 2>>/tmp/itest-pg.err)
$PGBIN "$CONN" -q -c "drop table $TBL" >/dev/null 2>&1
if [ "$OUT" = "$RUN" ]; then ok "write/read round-trip returned '$OUT'"; else err "write/read mismatch: got '$OUT' ($(tail -1 /tmp/itest-pg.err))"; fi

# --- verdict ---
if [ "$FAIL" = 0 ]; then printf '\n\033[32m===== INTEGRATION TEST PASSED =====\033[0m\n'; else printf '\n\033[31m===== INTEGRATION TEST FAILED =====\033[0m\n'; fi
exit $FAIL
