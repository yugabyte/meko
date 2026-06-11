#!/usr/bin/env bash
# Block-size comparison battery for the split-topology ybLight stack.
# Runs on the pageserver host (172.151.20.28); the PS is local here, the
# compute is remote (172.151.29.184). SB=100MB, LFC disabled.
#
#   bash scripts/blocksize-bench.sh <label>     # label e.g. default | blk4mb
#
# Phases: (A) load 5M-row big_table + wall time, (B) wait upload drain to YB,
# (C) cold full-scan probe (wipe PS local layers, restart, time cold vs warm),
# (D) warm micro-battery G1..G6 with compute-CPU sampling, (E) summary.
set -uo pipefail

LABEL="${1:?usage: blocksize-bench.sh <label>}"
HERE="$(cd "$(dirname "$0")/.." && pwd)"
source "$HERE/scripts/_env.sh" ybLight
source "$HERE/scripts/_metrics.sh"

export DBNAME=bigbench
export ROWS=5000000
CHUNK=1000000

TENANT=530ed53aa3154f7d3c12af2237b89e3a
TIMELINE=86bda9db8ad069904cd33e619872c8a0
PS_API=http://172.151.20.28:9988
PS_DIR=/mnt/ps/endpoints/main/pageserver
PS_BIN=/home/ec2-user/yb-neon-bundle/bin/pageserver
export YB_STORAGE_PASSWORD='Yugabyte!1'

TS=$(date -u +%Y%m%d-%H%M)
OUT="$HERE/results/${TS}-ybLight-BLK-${LABEL}"
mkdir -p "$OUT"
SUMMARY="$OUT/SUMMARY.md"

pq() { $PSQL -h "$PGHOST" -p "$PGPORT" -U "$PGUSER" -d "$1" -v ON_ERROR_STOP=1 "${@:2}"; }
log() { echo "[$(date +%T)] $*" | tee -a "$OUT/run.log"; }

log "=== BLK battery label=$LABEL out=$OUT ==="

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

# ---- B. wait for upload drain to YB -----------------------------------------
log "B. waiting for PS upload drain (remote_consistent_lsn -> last_record_lsn)"
drain_start=$(date +%s)
lsn2int() { python3 -c "import sys;h,l=sys.argv[1].split('/');print(int(h,16)*2**32+int(l,16))" "$1"; }
for i in $(seq 1 360); do
  J=$(curl -s "$PS_API/v1/tenant/$TENANT/timeline/$TIMELINE")
  LAST=$(echo "$J" | python3 -c "import sys,json;print(json.load(sys.stdin)['last_record_lsn'])")
  REMOTE=$(echo "$J" | python3 -c "import sys,json;print(json.load(sys.stdin)['remote_consistent_lsn'])")
  PHYS=$(echo "$J" | python3 -c "import sys,json;print(json.load(sys.stdin)['current_physical_size'])")
  if [ "$(lsn2int "$REMOTE")" -ge "$(lsn2int "$LAST")" ]; then
    log "B. drained: remote=$REMOTE last=$LAST physical=$PHYS"
    break
  fi
  sleep 5
done
DRAIN_SECS=$(( $(date +%s) - drain_start ))
PHYS_SIZE=$PHYS
log "B. drain wait ${DRAIN_SECS}s, physical_size=$PHYS_SIZE"

# ---- C. full-scan timing (NO PS restart) -------------------------------------
# NOTE: a true cold probe (wipe PS local layers + restart PS) trips the yb-neon
# SK WAL-durability/reconnect wedge, so we do NOT restart the PS here. We measure
# a buffer-cold full scan instead: SB=100MB + LFC off means the 1.27 GB table
# cannot be cached, so each scan re-fetches pages from the PS regardless.
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

# compute-CPU average for postgres + the avg system busy
CPU_POSTGRES=$(awk -F'cpu=' '/postgres/{gsub(/%.*/,"",$2); s+=$2; n++} END{if(n)printf "%.1f", s/n}' "$CPULOG" 2>/dev/null)

# ---- E. summary --------------------------------------------------------------
log "E. writing $SUMMARY"
{
  echo "# BLK battery — label=$LABEL"
  echo
  echo "- Topology: compute on 172.151.29.184 (SB=100MB, LFC off), pageserver off-box on 172.151.20.28."
  echo "- Tenant $TENANT / timeline $TIMELINE."
  echo "- Dataset: $ROWCOUNT rows, total relation $HEAP, PS physical_size $PHYS_SIZE bytes."
  echo
  echo "## Load + storage"
  echo "| metric | value |"
  echo "|---|---|"
  echo "| load wall time | ${LOAD_SECS}s |"
  echo "| upload-drain wait | ${DRAIN_SECS}s |"
  echo "| PS physical size | $PHYS_SIZE bytes |"
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
  echo "## Compute CPU"
  echo "- postgres avg CPU during battery: ${CPU_POSTGRES}% (single-core %); see compute-cpu.log."
  echo
  echo "## pg_stat delta"
  echo '```'
  cat "$OUT/pgstat-delta.txt"
  echo '```'
} > "$SUMMARY"

log "=== DONE label=$LABEL -> $SUMMARY ==="
cat "$SUMMARY"
