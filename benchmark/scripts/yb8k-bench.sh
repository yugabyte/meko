#!/usr/bin/env bash
# G-battery for the NEW yb-pageserver (8k-page direct, no layers/compaction),
# CO-LOCATED with the compute on the 2-core box ybLight (172.151.29.184).
# Client/orchestration runs on 172.151.20.28 (same as the prior off-box BLK
# battery), so the pgbench client path is identical for a fair comparison.
#
#   bash scripts/yb8k-bench.sh <label>     # label e.g. yb8k
#
# Phases: (A) load 5M-row big_table + wall time, (B) checkpoint + YB page-store
# size, (C) buffer-cold full-scan probe, (D) warm micro-battery G1..G6 with
# compute+PS CPU sampling, (E) summary.
set -uo pipefail

LABEL="${1:?usage: yb8k-bench.sh <label>}"
HERE="$(cd "$(dirname "$0")/.." && pwd)"
source "$HERE/scripts/_env.sh" ybLight
source "$HERE/scripts/_metrics.sh"
export PGPASSWORD='Yugabyte!1'

export DBNAME=bigbench
export ROWS=5000000
CHUNK=1000000

TENANT=3c2e69b50b2df934e5f85a9699716e91
TIMELINE=d91b99d6de69060d19df6fe59c61c84d
export YB_STORAGE_PASSWORD='Yugabyte!1'

# YB backing store (for page-store size probe)
YB_PSQL=/home/ec2-user/yb-neon-bundle/pg_install/v18/bin/psql
ybq() { PGPASSWORD='Yugabyte!1' $YB_PSQL -h 172.151.17.132 -p 5433 -U yugabyte -d yb_storage -tAc "$1" 2>/dev/null; }

TS=$(date -u +%Y%m%d-%H%M)
OUT="$HERE/results/${TS}-ybLight-YB8K-${LABEL}"
mkdir -p "$OUT"
SUMMARY="$OUT/SUMMARY.md"

pq() { $PSQL -h "$PGHOST" -p "$PGPORT" -U "$PGUSER" -d "$1" -v ON_ERROR_STOP=1 "${@:2}"; }
log() { echo "[$(date +%T)] $*" | tee -a "$OUT/run.log"; }

log "=== YB8K battery label=$LABEL out=$OUT (new yb-pageserver, 8k-direct, co-located) ==="

# ---- A. schema + load --------------------------------------------------------
log "A. (re)create db + schema"
pq postgres -tAc "SELECT 1 FROM pg_database WHERE datname='$DBNAME'" | grep -q 1 \
  && pq postgres -c "DROP DATABASE $DBNAME"
pq postgres -c "CREATE DATABASE $DBNAME"
pq "$DBNAME" -f "$HERE/sql/big-load-schema.sql"

log "A. load $ROWS rows (chunk=$CHUNK)"
load_start=$(date +%s)
lo=1
while [ "$lo" -le "$ROWS" ]; do
  hi=$(( lo + CHUNK - 1 )); [ "$hi" -gt "$ROWS" ] && hi=$ROWS
  pq "$DBNAME" -qc "INSERT INTO big_table (id,cat,val,payload,ts)
    SELECT g,(g*7)%10000+1,g,repeat(md5(g::text),5)::bytea,now()
    FROM generate_series($lo,$hi) g;" >/dev/null
  log "   loaded $lo..$hi"
  lo=$(( hi + 1 ))
done
load_end=$(date +%s)
LOAD_SECS=$(( load_end - load_start ))
ROWCOUNT=$(pq "$DBNAME" -tAc "SELECT count(*) FROM big_table")
HEAP=$(pq "$DBNAME" -tAc "SELECT pg_size_pretty(pg_total_relation_size('big_table'))")
log "A. load done: $ROWCOUNT rows, total relation $HEAP, ${LOAD_SECS}s"

# ---- B. checkpoint + YB page-store size -------------------------------------
# New engine has NO layer uploads, so the old remote_consistent_lsn drain check
# does not apply. Instead force a checkpoint and measure the 8k-page rows in YB.
log "B. checkpoint + measure YB page store"
pq "$DBNAME" -c "CHECKPOINT;" >/dev/null 2>&1 || true
sleep 5
PAGES=$(ybq "SELECT count(*) FROM pageserver.pages;")
SLRU=$(ybq "SELECT count(*) FROM pageserver.slru_pages;")
PAGE_LOGICAL=$(ybq "SELECT pg_size_pretty(count(*)::bigint*8192) FROM pageserver.pages;")
J=$(curl -s --max-time 10 "http://127.0.0.1:9988/v1/tenant/$TENANT/timeline/$TIMELINE" 2>/dev/null) || J=""
LAST=$(echo "$J" | python3 -c "import sys,json;print(json.load(sys.stdin).get('last_record_lsn','n/a'))" 2>/dev/null || echo n/a)
log "B. pages=$PAGES slru=$SLRU logical=$PAGE_LOGICAL last_record_lsn=$LAST"

# ---- C. full-scan timing (NO PS restart) ------------------------------------
# SB=100MB + LFC off means the ~1.27 GB table cannot be cached, so each scan
# re-fetches 8k pages from the co-located yb-pageserver (-> YB).
log "C. full-scan timing (no PS restart)"
timed() { local t0 t1; t0=$(date +%s.%N); pq "$DBNAME" -tAc "SELECT count(*),sum(val) FROM big_table" >/dev/null; t1=$(date +%s.%N); awk "BEGIN{printf \"%.2f\", $t1-$t0}"; }
COLD=$(timed)
WARM=$(timed)
log "C. full-scan run1=${COLD}s  run2=${WARM}s"

# ---- D. warm micro-battery ---------------------------------------------------
log "D. warm battery G1..G6 (pgbench -c1 -j1, 30s warmup + 1x600s)"
CPULOG="$OUT/compute-cpu.log"
SP=$(start_server_cpu_sampler "$CPULOG")
snapshot_pg_stat "$DBNAME" "$OUT/pgstat-start.txt"
mark_timestamp "$OUT" START

declare -A GNAME=( [G1]=point-lookup [G2]=aggregate [G3]=range-scan [G4]=update [G5]=insert [G6]=delete )
BATT="$OUT/battery.txt"
: >"$BATT"
for G in G1 G2 G3 G4 G5 G6; do
  f="$HERE/sql/${G}-${GNAME[$G]}.sql"
  log "D. $G ${GNAME[$G]} warmup 30s"
  $PGBENCH -h "$PGHOST" -p "$PGPORT" -U "$PGUSER" -d "$DBNAME" -n -c1 -j1 -T30 \
    -D rows="$ROWS" -f "$f" >/dev/null 2>&1
  log "D. $G ${GNAME[$G]} measured 600s"
  o=$($PGBENCH -h "$PGHOST" -p "$PGPORT" -U "$PGUSER" -d "$DBNAME" -n -c1 -j1 -T600 \
      -D rows="$ROWS" -f "$f" 2>&1)
  lat=$(echo "$o" | awk '/latency average/{print $4}')
  tps=$(echo "$o" | awk '/^tps/{print $3}')
  cnt=$(echo "$o" | awk '/number of transactions actually processed/{print $NF}')
  echo "$G ${GNAME[$G]} lat=${lat}ms tps=${tps} txns=${cnt}" | tee -a "$BATT"
done

mark_timestamp "$OUT" END
snapshot_pg_stat "$DBNAME" "$OUT/pgstat-end.txt"
stop_server_cpu_sampler "$SP"
diff_pg_stat "$OUT/pgstat-start.txt" "$OUT/pgstat-end.txt" > "$OUT/pgstat-delta.txt" 2>&1

CPU_POSTGRES=$(awk -F'cpu=' '/postgres/{gsub(/%.*/,"",$2); s+=$2; n++} END{if(n)printf "%.1f", s/n}' "$CPULOG" 2>/dev/null)
CPU_PS=$(awk -F'cpu=' '/pageserver/{gsub(/%.*/,"",$2); s+=$2; n++} END{if(n)printf "%.1f", s/n}' "$CPULOG" 2>/dev/null)

# ---- E. summary --------------------------------------------------------------
log "E. writing $SUMMARY"
{
  echo "# YB8K battery — label=$LABEL (NEW yb-pageserver, 8k-direct, no layers)"
  echo
  echo "- Topology: compute + yb-pageserver CO-LOCATED on 172.151.29.184 (2 vCPU, SB=100MB, LFC off)."
  echo "- Client/pgbench on 172.151.20.28 (same client path as prior off-box BLK battery)."
  echo "- Tenant $TENANT / timeline $TIMELINE."
  echo "- Dataset: $ROWCOUNT rows, total relation $HEAP."
  echo "- YB page store: $PAGES pages ($PAGE_LOGICAL logical), $SLRU slru pages."
  echo
  echo "## Load + storage"
  echo "| metric | value |"
  echo "|---|---|"
  echo "| load wall time | ${LOAD_SECS}s |"
  echo "| YB pages stored | $PAGES |"
  echo "| YB page logical size | $PAGE_LOGICAL |"
  echo "| full-scan run1 | ${COLD}s |"
  echo "| full-scan run2 | ${WARM}s |"
  echo
  echo "## Warm micro-battery (pgbench -c1 -j1, 30s warmup + 1x600s)"
  echo "| test | latency avg (ms) | tps | txns |"
  echo "|---|---|---|---|"
  for G in G1 G2 G3 G4 G5 G6; do
    lat=$(awk -v g="$G" '$1==g{print $3}' "$BATT" | sed 's/lat=//;s/ms//')
    tps=$(awk -v g="$G" '$1==g{print $4}' "$BATT" | sed 's/tps=//')
    txn=$(awk -v g="$G" '$1==g{print $5}' "$BATT" | sed 's/txns=//')
    echo "| $G ${GNAME[$G]} | $lat | $tps | $txn |"
  done
  echo
  echo "## CPU (co-located, single-core %)"
  echo "- postgres compute avg CPU: ${CPU_POSTGRES}%"
  echo "- yb-pageserver avg CPU:    ${CPU_PS}%"
  echo "- see compute-cpu.log for the full per-process trace."
  echo
  echo "## pg_stat delta"
  echo '```'
  cat "$OUT/pgstat-delta.txt"
  echo '```'
} > "$SUMMARY"

log "=== DONE label=$LABEL -> $SUMMARY ==="
cat "$SUMMARY"
