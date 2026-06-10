#!/usr/bin/env bash
# G-battery for the NEW yb-pageserver (8k-page direct, no layers/compaction),
# run OFF-BOX (isolated) on the 16-vCPU host 172.151.20.28, with the compute on
# the 2-core box ybLight (172.151.29.184). This is the apples-to-apples peer of
# the prior off-box BLK battery (same topology, dataset, micro-battery), and it
# additionally measures the off-box pageserver's RSS + CPU.
#
#   bash scripts/yb8k-offbox-bench.sh <label>     # label e.g. offbox
#
# Phases: (A) load 5M-row big_table + wall time, (B) checkpoint + YB page-store
# delta, (C) buffer-cold full-scan probe, (D) warm micro-battery G1..G6 with
# compute CPU (ybLight) + off-box PS RSS/CPU (this box) sampling, (E) summary.
set -uo pipefail

LABEL="${1:?usage: yb8k-offbox-bench.sh <label>}"
HERE="$(cd "$(dirname "$0")/.." && pwd)"
source "$HERE/scripts/_env.sh" ybLight
source "$HERE/scripts/_metrics.sh"
export PGPASSWORD='Yugabyte!1'

export DBNAME=bigbench
export ROWS=5000000
CHUNK=1000000

# Fresh off-box tenant/timeline (created this session).
TENANT=afc00b5312231630aff5716f816605f3
TIMELINE=2122174f0982dc6fccf90d509850ca53
export YB_STORAGE_PASSWORD='Yugabyte!1'

# Off-box pageserver HTTP (lives on THIS box).
PS_HTTP=172.151.20.28:9988

# YB backing store (for page-store size probe)
YB_PSQL=/home/ec2-user/yb-neon-bundle/pg_install/v17/bin/psql
ybq() { PGPASSWORD='Yugabyte!1' $YB_PSQL "host=172.151.17.132 port=5433 user=yugabyte dbname=yb_storage sslmode=require" -tAc "$1" 2>/dev/null; }

TS=$(date -u +%Y%m%d-%H%M)
OUT="$HERE/results/${TS}-ybLight-YB8K-offbox-${LABEL}"
mkdir -p "$OUT"
SUMMARY="$OUT/SUMMARY.md"

pq() { $PSQL -h "$PGHOST" -p "$PGPORT" -U "$PGUSER" -d "$1" -v ON_ERROR_STOP=1 "${@:2}"; }
log() { echo "[$(date +%T)] $*" | tee -a "$OUT/run.log"; }

log "=== YB8K-OFFBOX battery label=$LABEL out=$OUT (new yb-pageserver, 8k-direct, OFF-BOX) ==="

# Start the off-box PS resource sampler for the WHOLE run (load is where peak
# RSS shows up). Sampler runs locally on this box where the PS lives.
PSLOG="$OUT/ps-resource.log"
PSSP=$(start_local_proc_sampler "$PSLOG" "yb-pageserver -D /mnt/ps/pageserver_yb4")

# ---- A. schema + load --------------------------------------------------------
log "A. (re)create db + schema"
pq postgres -tAc "SELECT 1 FROM pg_database WHERE datname='$DBNAME'" | grep -q 1 \
  && pq postgres -c "DROP DATABASE $DBNAME"
pq postgres -c "CREATE DATABASE $DBNAME"
pq "$DBNAME" -f "$HERE/sql/big-load-schema.sql"

PAGES0=$(ybq "SELECT count(*) FROM pageserver.pages;")
log "A. YB pageserver.pages baseline (pre-load, all tenants): $PAGES0"

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

# Peak PS RSS observed during load (MB).
PS_RSS_LOAD=$(awk -F'rss=' '/rss=/{gsub(/MB.*/,"",$2); if($2+0>m)m=$2+0} END{printf "%.1f", m}' "$PSLOG" 2>/dev/null)
log "A. off-box PS peak RSS during load: ${PS_RSS_LOAD}MB"

# ---- B. checkpoint + YB page-store size -------------------------------------
log "B. checkpoint + measure YB page store"
pq "$DBNAME" -c "CHECKPOINT;" >/dev/null 2>&1 || true
sleep 5
PAGES=$(ybq "SELECT count(*) FROM pageserver.pages;")
PAGES_DELTA=$(( PAGES - PAGES0 ))
SLRU=$(ybq "SELECT count(*) FROM pageserver.slru_pages;")
PAGE_LOGICAL=$(ybq "SELECT pg_size_pretty(${PAGES_DELTA}::bigint*8192);")
J=$(curl -s --max-time 10 "http://$PS_HTTP/v1/tenant/$TENANT/timeline/$TIMELINE" 2>/dev/null) || J=""
LAST=$(echo "$J" | python3 -c "import sys,json;print(json.load(sys.stdin).get('last_record_lsn','n/a'))" 2>/dev/null || echo n/a)
log "B. pages_total=$PAGES delta(this load)=$PAGES_DELTA slru=$SLRU logical(delta)=$PAGE_LOGICAL last_record_lsn=$LAST"

# ---- C. full-scan timing (NO PS restart) ------------------------------------
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
# Mark the PS sampler log so we can isolate the battery window for PS CPU.
echo "### BATTERY-START $(date +%T)" >>"$PSLOG"

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

echo "### BATTERY-END $(date +%T)" >>"$PSLOG"
mark_timestamp "$OUT" END
snapshot_pg_stat "$DBNAME" "$OUT/pgstat-end.txt"
stop_server_cpu_sampler "$SP"
kill "$PSSP" 2>/dev/null; wait "$PSSP" 2>/dev/null || true
diff_pg_stat "$OUT/pgstat-start.txt" "$OUT/pgstat-end.txt" > "$OUT/pgstat-delta.txt" 2>&1

CPU_POSTGRES=$(awk -F'cpu=' '/postgres/{gsub(/%.*/,"",$2); s+=$2; n++} END{if(n)printf "%.1f", s/n}' "$CPULOG" 2>/dev/null)

# Off-box PS CPU + RSS during the battery window (between the two markers).
read -r PS_CPU_BATT PS_RSS_BATT PS_RSS_PEAK < <(awk '
  /### BATTERY-START/{inw=1; next}
  /### BATTERY-END/{inw=0}
  inw && /cpu=/{
    c=$0; sub(/.*cpu=/,"",c); sub(/%.*/,"",c); cs+=c; cn++;
    r=$0; sub(/.*rss=/,"",r); sub(/MB.*/,"",r); rs+=r; rn++; if(r+0>rp)rp=r+0;
  }
  END{printf "%.1f %.1f %.1f", (cn?cs/cn:0), (rn?rs/rn:0), rp}' "$PSLOG" 2>/dev/null)

# ---- E. summary --------------------------------------------------------------
log "E. writing $SUMMARY"
{
  echo "# YB8K-OFFBOX battery — label=$LABEL (NEW yb-pageserver, 8k-direct, OFF-BOX)"
  echo
  echo "- Topology: compute on ybLight 172.151.29.184 (2 vCPU, SB=100MB, LFC off); yb-pageserver OFF-BOX on 172.151.20.28 (16 vCPU)."
  echo "- Apples-to-apples peer of the prior off-box BLK battery (same topology, 5M-row dataset, G1-G6 micro-battery)."
  echo "- Tenant $TENANT / timeline $TIMELINE."
  echo "- Dataset: $ROWCOUNT rows, total relation $HEAP."
  echo "- YB page store (8k-direct): +$PAGES_DELTA pages this load ($PAGE_LOGICAL logical); $SLRU slru pages; pageserver.pages baseline was $PAGES0 (stale pages from prior 8k runs share the table)."
  echo
  echo "## Load + storage"
  echo "| metric | value |"
  echo "|---|---|"
  echo "| load wall time | ${LOAD_SECS}s |"
  echo "| YB pages stored (delta) | $PAGES_DELTA |"
  echo "| YB page logical size (delta) | $PAGE_LOGICAL |"
  echo "| off-box PS peak RSS during load | ${PS_RSS_LOAD}MB |"
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
  echo "## CPU + memory"
  echo "- compute (postgres on ybLight) avg CPU during battery: ${CPU_POSTGRES}% (single-core %)."
  echo "- off-box PS avg CPU during battery: ${PS_CPU_BATT}% (single-core %)."
  echo "- off-box PS avg RSS during battery: ${PS_RSS_BATT}MB (peak ${PS_RSS_PEAK}MB)."
  echo "- off-box PS peak RSS during load:  ${PS_RSS_LOAD}MB."
  echo "- NOTE: this build is the fixed 8k-direct yb-pageserver (git 5f806d17): query #1 GROUP BY rewrite + image materializer + [yb_page_writer] read_cache_bytes=8GiB, materialize_queue_capacity=5000. See ps-resource.log + compute-cpu.log."
  echo
  echo "## pg_stat delta"
  echo '```'
  cat "$OUT/pgstat-delta.txt"
  echo '```'
} > "$SUMMARY"

log "=== DONE label=$LABEL -> $SUMMARY ==="
cat "$SUMMARY"
