#!/usr/bin/env bash
# G-battery for the LAYERED pageserver, run OFF-BOX (isolated) on the 16-vCPU host
# 172.151.20.28, with the compute on the 2-core box ybLight (172.151.29.184).
#
# This is the apples-to-apples peer of the FIXED 8k-direct yb-pageserver off-box
# run (same topology, dataset, micro-battery, RSS/CPU sampling), but with the
# layered engine given a SMALL self-managed local layer cache: an 8GB RAM
# page_cache (matching the 8k build's 8GiB read cache) plus a bounded
# disk_usage_based_eviction (min_avail_bytes set to cap LOCAL layers at ~1GiB,
# period 60s, LRU). The engine evicts cold layers on its own, so the cold path
# (full-scan, 5GB working set) must re-download layers from YB public.blobs,
# while the small warm micro-battery working set stays resident across the run.
# No manual eviction here (a continuous manual evictor death-spiralled against
# reads); the engine self-manages the bounded cache.
#
#   bash scripts/yb-blkL-offbox-bench.sh <label>     # label e.g. offbox
#
# Phases: (A) load 5M-row big_table + wall time, (B) upload-drain + manual
# eviction + YB blob-store size + LOCAL resident-layer bytes, (C) buffer-cold
# full-scan probe, (D) warm micro-battery G1..G6 with compute CPU (ybLight) +
# off-box PS RSS/CPU (this box) sampling, (E) summary.
set -uo pipefail

LABEL="${1:?usage: yb-blkL-offbox-bench.sh <label>}"
HERE="$(cd "$(dirname "$0")/.." && pwd)"
source "$HERE/scripts/_env.sh" ybLight
source "$HERE/scripts/_metrics.sh"
export PGPASSWORD='Yugabyte!1'

export DBNAME=bigbench
export ROWS=5000000
CHUNK=1000000

# Fresh off-box LAYERED tenant/timeline (created this session, node 2).
TENANT=fe6fcbbaab4e7338967a4c189c06e011
TIMELINE=8cb121e6af274b8d58abb9f4c1b5cb31
export NEON_YB_PASSWORD='Yugabyte!1'

# Off-box pageserver HTTP (lives on THIS box).
PS_HTTP=172.151.20.28:9988
# Local layer dir for resident-bytes measurement (proves eviction is working).
PS_TLDIR=/mnt/ps/pageserver_layered/tenants/$TENANT/timelines/$TIMELINE

# YB backing store (for blob-store size probe)
YB_PSQL=/home/ec2-user/yb-neon-bundle/pg_install/v17/bin/psql
ybq() { PGPASSWORD='Yugabyte!1' $YB_PSQL "host=172.151.17.132 port=5433 user=yugabyte dbname=yb_storage sslmode=require" -tAc "$1" 2>/dev/null; }

TS=$(date -u +%Y%m%d-%H%M)
OUT="$HERE/results/${TS}-ybLight-BLKL-offbox-${LABEL}"
mkdir -p "$OUT"
SUMMARY="$OUT/SUMMARY.md"

pq() { $PSQL -h "$PGHOST" -p "$PGPORT" -U "$PGUSER" -d "$1" -v ON_ERROR_STOP=1 "${@:2}"; }
log() { echo "[$(date +%T)] $*" | tee -a "$OUT/run.log"; }

# PS timeline detail field extractor.
ps_field() { curl -s --max-time 10 "http://$PS_HTTP/v1/tenant/$TENANT/timeline/$TIMELINE" 2>/dev/null \
  | python3 -c "import sys,json;print(json.load(sys.stdin).get('$1','n/a'))" 2>/dev/null || echo n/a; }
local_layer_bytes() { du -sb "$PS_TLDIR" 2>/dev/null | awk '{print $1}'; }

log "=== BLKL-OFFBOX battery label=$LABEL out=$OUT (LAYERED pageserver, OFF-BOX, remote-read forced) ==="

# Start the off-box PS resource sampler for the WHOLE run.
PSLOG="$OUT/ps-resource.log"
PSSP=$(start_local_proc_sampler "$PSLOG" "pageserver -D /mnt/ps/pageserver_layered")

# ---- A. schema + load --------------------------------------------------------
log "A. (re)create db + schema"
pq postgres -tAc "SELECT 1 FROM pg_database WHERE datname='$DBNAME'" | grep -q 1 \
  && pq postgres -c "DROP DATABASE $DBNAME"
pq postgres -c "CREATE DATABASE $DBNAME"
pq "$DBNAME" -f "$HERE/sql/big-load-schema.sql"

BLOBS0=$(ybq "SELECT count(*) FROM public.blobs WHERE tenant_id='$TENANT';")
log "A. YB blobs baseline (pre-load, this tenant): $BLOBS0"

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

PS_RSS_LOAD=$(awk -F'rss=' '/rss=/{gsub(/MB.*/,"",$2); if($2+0>m)m=$2+0} END{printf "%.1f", m}' "$PSLOG" 2>/dev/null)
LOCAL_LOAD=$(local_layer_bytes)
log "A. off-box PS peak RSS during load: ${PS_RSS_LOAD}MB; local layer bytes right after load: ${LOCAL_LOAD}"

# ---- B. checkpoint + bounded upload drain + eviction + sizes ----------------
# PS stdout log (where on-demand layer downloads are logged) — set when launched.
PS_MAIN_LOG=/tmp/layered-ps.log
dl_count() { grep -c "downloading on-demand" "$PS_MAIN_LOG" 2>/dev/null || echo 0; }

log "B. checkpoint + bounded upload drain (remote_consistent_lsn -> last_record_lsn)"
pq "$DBNAME" -c "CHECKPOINT;" >/dev/null 2>&1 || true
# Bounded drain: most layers upload quickly; the last in-memory open layer may
# linger (testing checkpoint API is disabled in this build), so cap at ~2min and
# proceed — the continuous evictor below cleans up whatever is uploaded.
for i in $(seq 1 12); do
  LAST=$(ps_field last_record_lsn); RC=$(ps_field remote_consistent_lsn)
  log "   drain check $i: last_record_lsn=$LAST remote_consistent_lsn=$RC"
  [ "$LAST" = "$RC" ] && [ "$LAST" != "n/a" ] && { log "   drained."; break; }
  sleep 10
done

# Local layers are bounded by the engine's own disk_usage_based_eviction
# (~1GiB cap, period 60s). No manual eviction — give it a moment to settle to
# steady state after the drain, then record sizes.
log "B. engine self-managed eviction (~1GiB cap); settle 30s then measure"
sleep 30

BLOBS=$(ybq "SELECT count(*) FROM public.blobs WHERE tenant_id='$TENANT';")
BLOBS_DELTA=$(( BLOBS - BLOBS0 ))
BLOB_BYTES=$(ybq "SELECT coalesce(sum(size_bytes),0) FROM public.blobs WHERE tenant_id='$TENANT';")
BLOB_PRETTY=$(ybq "SELECT pg_size_pretty(coalesce(sum(size_bytes),0)) FROM public.blobs WHERE tenant_id='$TENANT';")
PHYS=$(ps_field current_physical_size)
LOCAL_AFTER=$(local_layer_bytes)
LAST=$(ps_field last_record_lsn)
log "B. YB blobs=$BLOBS delta=$BLOBS_DELTA blob_bytes=$BLOB_BYTES ($BLOB_PRETTY); PS current_physical_size=$PHYS; LOCAL resident layer bytes=$LOCAL_AFTER; last_record_lsn=$LAST"

# ---- C. full-scan timing (NO PS restart) ------------------------------------
log "C. full-scan timing (no PS restart)"
timed() { local t0 t1; t0=$(date +%s.%N); pq "$DBNAME" -tAc "SELECT count(*),sum(val) FROM big_table" >/dev/null; t1=$(date +%s.%N); awk "BEGIN{printf \"%.2f\", $t1-$t0}"; }
DL_C0=$(dl_count)
COLD=$(timed)
DL_C1=$(dl_count)
WARM=$(timed)
DL_C2=$(dl_count)
LOCAL_SCAN=$(local_layer_bytes)
SCAN_DL_COLD=$(( DL_C1 - DL_C0 )); SCAN_DL_WARM=$(( DL_C2 - DL_C1 ))
log "C. full-scan run1=${COLD}s (on-demand downloads=${SCAN_DL_COLD})  run2=${WARM}s (downloads=${SCAN_DL_WARM}); local layer bytes after scans=$LOCAL_SCAN"

# ---- D. warm micro-battery ---------------------------------------------------
log "D. warm battery G1..G6 (pgbench -c1 -j1, 30s warmup + 1x600s)"
CPULOG="$OUT/compute-cpu.log"
SP=$(start_server_cpu_sampler "$CPULOG")
snapshot_pg_stat "$DBNAME" "$OUT/pgstat-start.txt"
mark_timestamp "$OUT" START
echo "### BATTERY-START $(date +%T)" >>"$PSLOG"

declare -A GNAME=( [G1]=point-lookup [G2]=aggregate [G3]=range-scan [G4]=update [G5]=insert [G6]=delete )
BATT="$OUT/battery.txt"
: >"$BATT"
DL_D0=$(dl_count)
for G in G1 G2 G3 G4 G5 G6; do
  f="$HERE/sql/${G}-${GNAME[$G]}.sql"
  dlg0=$(dl_count)
  log "D. $G ${GNAME[$G]} warmup 30s"
  $PGBENCH -h "$PGHOST" -p "$PGPORT" -U "$PGUSER" -d "$DBNAME" -n -c1 -j1 -T30 \
    -D rows="$ROWS" -f "$f" >/dev/null 2>&1
  log "D. $G ${GNAME[$G]} measured 600s"
  o=$($PGBENCH -h "$PGHOST" -p "$PGPORT" -U "$PGUSER" -d "$DBNAME" -n -c1 -j1 -T600 \
      -D rows="$ROWS" -f "$f" 2>&1)
  lat=$(echo "$o" | awk '/latency average/{print $4}')
  tps=$(echo "$o" | awk '/^tps/{print $3}')
  cnt=$(echo "$o" | awk '/number of transactions actually processed/{print $NF}')
  dlg=$(( $(dl_count) - dlg0 ))
  echo "$G ${GNAME[$G]} lat=${lat}ms tps=${tps} txns=${cnt} downloads=${dlg}" | tee -a "$BATT"
done

echo "### BATTERY-END $(date +%T)" >>"$PSLOG"
DL_D1=$(dl_count); BATT_DL=$(( DL_D1 - DL_D0 ))
log "D. on-demand layer downloads DURING battery: $BATT_DL (if ~0, the 8GB RAM page_cache served reads, not YB)"
mark_timestamp "$OUT" END
snapshot_pg_stat "$DBNAME" "$OUT/pgstat-end.txt"
stop_server_cpu_sampler "$SP"
LOCAL_END=$(local_layer_bytes)
kill "$PSSP" 2>/dev/null; wait "$PSSP" 2>/dev/null || true
diff_pg_stat "$OUT/pgstat-start.txt" "$OUT/pgstat-end.txt" > "$OUT/pgstat-delta.txt" 2>&1

CPU_POSTGRES=$(awk -F'cpu=' '/postgres/{gsub(/%.*/,"",$2); s+=$2; n++} END{if(n)printf "%.1f", s/n}' "$CPULOG" 2>/dev/null)

read -r PS_CPU_BATT PS_RSS_BATT PS_RSS_PEAK < <(awk '
  /### BATTERY-START/{inw=1; next}
  /### BATTERY-END/{inw=0}
  inw && /cpu=/{
    c=$0; sub(/.*cpu=/,"",c); sub(/%.*/,"",c); cs+=c; cn++;
    r=$0; sub(/.*rss=/,"",r); sub(/MB.*/,"",r); rs+=r; rn++; if(r+0>rp)rp=r+0;
  }
  END{printf "%.1f %.1f %.1f", (cn?cs/cn:0), (rn?rs/rn:0), rp}' "$PSLOG" 2>/dev/null)

human() { awk -v b="$1" 'BEGIN{ if(b=="" || b=="n/a"){print "n/a"; exit} s="B KB MB GB TB"; n=split(s,u," "); v=b+0; i=1; while(v>=1024 && i<n){v/=1024;i++} printf "%.1f%s", v, u[i] }'; }

# ---- E. summary --------------------------------------------------------------
log "E. writing $SUMMARY"
{
  echo "# BLKL-OFFBOX battery — label=$LABEL (LAYERED pageserver, OFF-BOX, remote-read forced)"
  echo
  echo "- Topology: compute on ybLight 172.151.29.184 (2 vCPU, SB=100MB, LFC off); LAYERED pageserver OFF-BOX on 172.151.20.28 (16 vCPU), node id 2."
  echo "- Apples-to-apples peer of the FIXED 8k-direct off-box run: same 5M-row dataset, G1-G6 micro-battery, 8GB RAM page cache."
  echo "- Small self-managed local cache: disk_usage_based_eviction min_avail_bytes caps LOCAL layers at ~1GiB (period 60s, LRU). The engine evicts cold layers itself, so the cold full-scan (5GB working set) must re-download layers from YB public.blobs, while the small warm micro-battery working set stays resident. No manual eviction (a continuous manual evictor death-spiralled against reads)."
  echo "- Tenant $TENANT / timeline $TIMELINE."
  echo "- Dataset: $ROWCOUNT rows, total relation $HEAP."
  echo
  echo "## Load + storage"
  echo "| metric | value |"
  echo "|---|---|"
  echo "| load wall time | ${LOAD_SECS}s |"
  echo "| YB blobs stored (delta) | $BLOBS_DELTA |"
  echo "| YB blob bytes (this tenant) | $(human "$BLOB_BYTES") ($BLOB_BYTES) |"
  echo "| PS current_physical_size | $(human "$PHYS") ($PHYS) |"
  echo "| LOCAL resident layer bytes right after load | $(human "$LOCAL_LOAD") ($LOCAL_LOAD) |"
  echo "| LOCAL resident layer bytes after drain+eviction | $(human "$LOCAL_AFTER") ($LOCAL_AFTER) |"
  echo "| LOCAL resident layer bytes after full-scan | $(human "$LOCAL_SCAN") ($LOCAL_SCAN) |"
  echo "| LOCAL resident layer bytes after battery | $(human "$LOCAL_END") ($LOCAL_END) |"
  echo "| off-box PS peak RSS during load | ${PS_RSS_LOAD}MB |"
  echo "| full-scan run1 (cold) | ${COLD}s (on-demand layer downloads: ${SCAN_DL_COLD}) |"
  echo "| full-scan run2 (warm) | ${WARM}s (downloads: ${SCAN_DL_WARM}) |"
  echo
  echo "## Warm micro-battery (pgbench -c1 -j1, 30s warmup + 1x600s; engine self-managed ~1GiB local cache)"
  echo "| test | latency avg (ms) | tps | txns | on-demand layer downloads |"
  echo "|---|---|---|---|---|"
  for G in G1 G2 G3 G4 G5 G6; do
    lat=$(awk -v g="$G" '$1==g{print $3}' "$BATT" | sed 's/lat=//;s/ms//')
    tps=$(awk -v g="$G" '$1==g{print $4}' "$BATT" | sed 's/tps=//')
    txn=$(awk -v g="$G" '$1==g{print $5}' "$BATT" | sed 's/txns=//')
    dlg=$(awk -v g="$G" '$1==g{print $6}' "$BATT" | sed 's/downloads=//')
    echo "| $G ${GNAME[$G]} | $lat | $tps | $txn | $dlg |"
  done
  echo "- Total on-demand layer downloads during battery: ${BATT_DL}. If ~0, the 8GB RAM page_cache served reads (local-layer eviction had no effect on warm reads -> the RAM cache, not local layer files, is what makes warm reads fast)."
  echo
  echo "## CPU + memory"
  echo "- compute (postgres on ybLight) avg CPU during battery: ${CPU_POSTGRES}% (single-core %)."
  echo "- off-box PS avg CPU during battery: ${PS_CPU_BATT}% (single-core %)."
  echo "- off-box PS avg RSS during battery: ${PS_RSS_BATT}MB (peak ${PS_RSS_PEAK}MB)."
  echo "- off-box PS peak RSS during load:  ${PS_RSS_LOAD}MB."
  echo "- NOTE: ps -o %cpu is a lifetime average and understates the read-storm CPU; cross-check ps-resource.log against top if needed."
  echo "- Granularity caveat: even with local layers evicted, the layered engine re-downloads whole LAYER FILES per miss; the 8k-direct engine fetches per-8KB-page. Comparison is fairer (both miss to YB) but not identical."
  echo
  echo "## pg_stat delta"
  echo '```'
  cat "$OUT/pgstat-delta.txt"
  echo '```'
} > "$SUMMARY"

log "=== DONE label=$LABEL -> $SUMMARY ==="
cat "$SUMMARY"
