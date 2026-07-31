#!/usr/bin/env bash
# =============================================================================
# WalStress — maximum sustainable WAL generation rate, isolated from page reads.
# =============================================================================
#
# PURPOSE
#   Measure how much WAL/s the yb-neon split stack can push when the ENTIRE
#   working set is resident in shared_buffers, so the pageserver serves ~zero
#   getpage reads. This isolates the WAL write/durability pipeline
#       compute (WALInsert → walproposer)  →  safekeeper flush  →  durable
#   from page-materialization read stress. We verify isolation per-run by
#   diffing the pageserver getpage counter (want ~0).
#
# TOPOLOGY (as of this benchmark)
#   compute + safekeeper + control plane : ybAmp4  172.151.23.96 (4 vCPU/31GB)
#     shared_buffers=16GB, synchronous_commit=on, synchronous_standby_names=walproposer
#   pageserver (off-box)                 : this harness host 172.151.20.28 (16 vCPU)
#     HTTP metrics at :9988 ; YugabyteDB backing store shared.
#   safekeeper HTTP metrics              : ybAmp4:7676 (flush_lsn etc.)
#
# THE WORKLOAD
#   A single table `walstress(id int pk, v bigint, pad text)` sized to fit fully
#   in shared_buffers (default ROWS=16M ≈ 2.6 GB heap+pk). It is prewarmed into
#   shared_buffers with pg_prewarm (a seqscan would use the bulk-read ring buffer
#   and NOT populate SB). pgbench then runs BATCH random single-row UPDATEs per
#   transaction (amortizes commit/SyncRep overhead so we probe the WAL pipeline
#   ceiling, not per-commit latency).
#
# FPI vs NON-FPI  (full_page_writes)
#   With full_page_writes=on, the first touch of each page after a checkpoint
#   emits an 8 KB full-page image. Random updates over the whole table + a small
#   max_wal_size (1 GB) make checkpoints frequent, so ~100% of WAL bytes become
#   FPIs and the measured rate reflects FPI shoveling, not real change volume.
#     FPW=on  (default) → FPI-dominated rate.
#     FPW=off           → tuple-level "real change rate" ceiling. Safe on Neon:
#                         the pageserver reconstructs pages from WAL and is not
#                         exposed to torn-page risk. The script saves the prior
#                         value and ALWAYS restores it on exit (trap).
#
# HOW WAL RATE IS MEASURED
#   Deltas across the run window of:
#     - pg_stat_wal.wal_bytes / wal_records / wal_fpi   (authoritative)
#     - pg_current_wal_lsn()                            (raw LSN advance)
#     - safekeeper_flush_lsn metric                     (durable at safekeeper)
#   plus compute postgres CPU, safekeeper CPU, and pageserver CPU/RSS.
#
# USAGE
#   ./wal-stress.sh ybAmp4 create          # build + prewarm the table (once)
#   ./wal-stress.sh ybAmp4 run             # single run at $CONNS
#   ./wal-stress.sh ybAmp4 sweep           # concurrency sweep
#   ./wal-stress.sh ybAmp4 clean           # drop the table
#   FPW=off ./wal-stress.sh ybAmp4 sweep   # non-FPI variant
#
# KNOBS (env)
#   ROWS=16000000     table rows (~2.6 GB; must fit in shared_buffers)
#   CONNS=100         pgbench clients (run phase)
#   JOBS=             pgbench worker threads (default min(CONNS,nproc))
#   DURATION=90       seconds per run
#   BATCH=10          UPDATEs per transaction
#   FPW=on|off        full_page_writes during the run (default on)
#   SWEEP="50 100 200 300"   client counts for the sweep phase
#
# REFERENCE RESULTS (build 272b733ac, 2026-06-05, FPW=on)
#   c=50 →26.8  c=100 →23.7  c=200 →20.4  MB/s ; ~100% FPI ; getpage≈0.
#   Throughput FALLS as clients rise → WAL-insert/buffer-lock contention, not a
#   durability limit (SK CPU ~20%, compute ~2 of 4 cores).
# =============================================================================

set -euo pipefail

if [[ $# -lt 1 ]]; then
  echo "Usage: $0 ybAmp4 [create|run|sweep|clean]" >&2
  exit 1
fi
target="$1"; shift
phase="${1:-run}"

source ~/benchmark/scripts/_env.sh "$target"
source ~/benchmark/scripts/_metrics.sh

ROWS="${ROWS:-16000000}"
CONNS="${CONNS:-100}"
DURATION="${DURATION:-90}"
BATCH="${BATCH:-10}"
FPW="${FPW:-on}"
JOBS="${JOBS:-$(( CONNS < $(nproc) ? CONNS : $(nproc) ))}"
DB="$PGDATABASE"
TBL=walstress

PS_HTTP="http://172.151.20.28:9988/metrics"
SK_HTTP="http://127.0.0.1:7676/metrics"
SSH="ssh -o StrictHostKeyChecking=no -o ConnectTimeout=10 -i $HOME/.ssh/yb-15-aws-portal-1-key.pem"

psql_t() { timeout 60 $PSQL -d "$DB" -tAc "$1"; }

build_table() {
  echo "[WalStress] (re)building $TBL with $ROWS rows in db=$DB ..."
  psql_t "DROP TABLE IF EXISTS $TBL;"
  psql_t "CREATE TABLE $TBL (id int PRIMARY KEY, v bigint NOT NULL DEFAULT 0, pad text NOT NULL);"
  psql_t "INSERT INTO $TBL SELECT g, 0, repeat('x',100) FROM generate_series(1,$ROWS) g;"
  psql_t "ANALYZE $TBL;"
  echo "[WalStress] $TBL size = $(psql_t "SELECT pg_size_pretty(pg_total_relation_size('$TBL'));")"
}

prewarm() {
  timeout 60 $PSQL -d "$DB" -c "CREATE EXTENSION IF NOT EXISTS pg_prewarm;" >/dev/null 2>&1 || true
  local heap idx
  heap=$(psql_t "SELECT pg_prewarm('$TBL');")
  idx=$(psql_t "SELECT pg_prewarm('${TBL}_pkey');")
  echo "[WalStress] prewarmed heap=$heap pages, pk=$idx pages into shared_buffers"
}

# --- full_page_writes management (saves + restores original) -----------------
FPW_ORIG=""
set_fpw() {
  local want="$1"
  FPW_ORIG=$(psql_t "show full_page_writes;")
  if [[ "$want" != "$FPW_ORIG" ]]; then
    echo "[WalStress] full_page_writes: $FPW_ORIG -> $want"
    psql_t "ALTER SYSTEM SET full_page_writes = $want;" >/dev/null
    psql_t "SELECT pg_reload_conf();" >/dev/null
    sleep 1
    local now; now=$(psql_t "show full_page_writes;")
    [[ "$now" == "$want" ]] || { echo "[WalStress] WARN: full_page_writes is '$now', expected '$want'"; }
  fi
}
restore_fpw() {
  [[ -n "$FPW_ORIG" ]] || return 0
  local cur; cur=$(psql_t "show full_page_writes;" 2>/dev/null || echo "")
  if [[ -n "$cur" && "$cur" != "$FPW_ORIG" ]]; then
    echo "[WalStress] restoring full_page_writes -> $FPW_ORIG"
    psql_t "ALTER SYSTEM SET full_page_writes = $FPW_ORIG;" >/dev/null 2>&1 || true
    psql_t "SELECT pg_reload_conf();" >/dev/null 2>&1 || true
  fi
  FPW_ORIG=""
}
trap restore_fpw EXIT

# Generate the pgbench script: BATCH single-row UPDATEs per transaction.
gen_script() {
  local f="$1"; : >"$f"
  for ((i=0;i<BATCH;i++)); do
    printf '\\set id random(1, :rows)\nUPDATE %s SET v = v + 1 WHERE id = :id;\n' "$TBL" >>"$f"
  done
}

# epoch | wal_records | wal_fpi | wal_bytes | current_lsn(bytes)
wal_snapshot() {
  psql_t "SELECT extract(epoch FROM clock_timestamp())::numeric(20,3)||'|'||
            wal_records||'|'||wal_fpi||'|'||wal_bytes||'|'||
            pg_wal_lsn_diff(pg_current_wal_lsn(),'0/0')::numeric(40,0)
          FROM pg_stat_wal;" 2>/dev/null
}
sk_flush_lsn() {
  timeout 15 $SSH ec2-user@"$PGHOST" "curl -s $SK_HTTP" 2>/dev/null \
    | awk '/^safekeeper_flush_lsn/{print $2; exit}'
}
ps_getpage_count() {
  timeout 15 curl -s "$PS_HTTP" 2>/dev/null \
    | awk '/get_page_at_lsn/ && /smgr_query_seconds_count/{print $2; exit}'
}

one_run() {
  local conns="$1" outdir="$2"
  mkdir -p "$outdir"
  local script="$outdir/walstress.sql"; gen_script "$script"

  prewarm
  timeout 120 $PSQL -d "$DB" -c "CHECKPOINT;" >/dev/null 2>&1 || true

  local cpu_pid ps_pid
  cpu_pid=$(start_server_cpu_sampler "$outdir/server-cpu.log")
  ps_pid=$(start_local_proc_sampler "$outdir/ps-cpu-rss.log" yb-pageserver)

  local w0 w1 gp0 gp1 skf0 skf1
  w0=$(wal_snapshot); gp0=$(ps_getpage_count); skf0=$(sk_flush_lsn)

  echo "[WalStress] run conns=$conns jobs=$JOBS batch=$BATCH dur=${DURATION}s fpw=$FPW ..."
  timeout $((DURATION+120)) $PGBENCH -n -f "$script" -D rows="$ROWS" \
    -c "$conns" -j "$JOBS" -T "$DURATION" -P 10 \
    -h "$PGHOST" -p "$PGPORT" -U "$PGUSER" "$DB" 2>&1 | tee "$outdir/pgbench.log" || true

  w1=$(wal_snapshot); gp1=$(ps_getpage_count); skf1=$(sk_flush_lsn)
  stop_server_cpu_sampler "$cpu_pid"
  stop_local_proc_sampler "$ps_pid"

  python3 - "$w0" "$w1" "${gp0:-0}" "${gp1:-0}" "${skf0:-0}" "${skf1:-0}" "$conns" "$FPW" <<'PY' | tee "$outdir/wal-summary.txt"
import sys
w0=sys.argv[1].split('|'); w1=sys.argv[2].split('|')
gp0=float(sys.argv[3] or 0); gp1=float(sys.argv[4] or 0)
skf0=float(sys.argv[5] or 0); skf1=float(sys.argv[6] or 0)
conns=sys.argv[7]; fpw=sys.argv[8]
t0,rec0,fpi0,by0,lsn0=map(float,w0)
t1,rec1,fpi1,by1,lsn1=map(float,w1)
dt=t1-t0
mb=lambda b: b/1024/1024
dby=by1-by0; dfpi=fpi1-fpi0
print(f"=== WalStress  conns={conns}  fpw={fpw}  window={dt:.1f}s ===")
print(f"WAL bytes/s     : {mb(dby/dt):8.1f} MB/s   ({mb(dby):.0f} MB total)")
print(f"WAL LSN delta/s : {mb((lsn1-lsn0)/dt):8.1f} MB/s   (raw LSN advance)")
print(f"SK flush LSN/s  : {mb((skf1-skf0)/dt):8.1f} MB/s   (durable at safekeeper)")
print(f"WAL records/s   : {(rec1-rec0)/dt:10.0f}")
fpi_pct = (100*dfpi*8192/dby) if dby>0 else 0
print(f"WAL FPI/s       : {dfpi/dt:10.0f}   ({fpi_pct:.0f}% of bytes are full-page images)")
print(f"PS getpage calls in window: {gp1-gp0:.0f}   (want ~0 -> no read stress)")
PY

  echo "--- compute CPU (postgres total, of 400%) / safekeeper ---"
  awk '/^--- /{if(h){s+=c;n++;if(c>mx)mx=c}c=0;h=1;next}/postgres/{for(i=1;i<NF;i++)if($i=="cpu="){v=$(i+1);gsub(/%/,"",v);c+=v}}END{if(h){s+=c;n++;if(c>mx)mx=c}if(n)printf "postgres avg=%.1f%% peak=%.1f%%\n",s/n,mx}' "$outdir/server-cpu.log" 2>/dev/null
  awk '/safekeeper$/{for(i=1;i<NF;i++)if($i=="cpu="){v=$(i+1);gsub(/%/,"",v);s+=v;n++}}END{if(n)printf "safekeeper avg=%.1f%%\n",s/n}' "$outdir/server-cpu.log" 2>/dev/null
  echo "--- pageserver CPU/RSS ---"; summarize_proc_sampler "$outdir/ps-cpu-rss.log"
}

case "$phase" in
  create) build_table; prewarm ;;
  clean)  psql_t "DROP TABLE IF EXISTS $TBL;"; echo "[WalStress] dropped $TBL" ;;
  run)
    set_fpw "$FPW"
    ts=$(date -u +%Y%m%d-%H%M)
    out=~/benchmark/results/${ts}-${target}-WalStress-c${CONNS}-fpw${FPW}
    one_run "$CONNS" "$out"
    echo "[WalStress] results -> $out"
    ;;
  sweep)
    set_fpw "$FPW"
    ts=$(date -u +%Y%m%d-%H%M)
    base=~/benchmark/results/${ts}-${target}-WalStress-sweep-fpw${FPW}
    mkdir -p "$base"
    for c in ${SWEEP:-50 100 200 300}; do one_run "$c" "$base/c${c}"; echo; done
    echo "=== SWEEP SUMMARY (fpw=$FPW) ==="
    { for c in ${SWEEP:-50 100 200 300}; do
        printf "c=%-4s " "$c"; grep -h "WAL bytes/s" "$base/c${c}/wal-summary.txt" 2>/dev/null
      done; } | tee "$base/sweep-summary.txt"
    echo "[WalStress] sweep results -> $base"
    ;;
  *) echo "unknown phase: $phase" >&2; exit 1 ;;
esac
