#!/usr/bin/env bash
# Simple-latency micro-benchmark (L1..L9).
# Usage: ./simple-latency.sh ybLight|aurora [test_id ...]
#   With no test_id: runs all L1..L9 in order.
#   With test_id list: runs just those (e.g. ./simple-latency.sh ybLight L4 L5 L6).
#
# For each test, runs:
#   - 5 s warmup (discarded)
#   - 3 × 30 s measurement runs, single client, prepared statements, with -r

set -euo pipefail

if [[ $# -lt 1 ]]; then
  echo "Usage: $0 ybLight|aurora [L1 L2 ...]" >&2
  exit 1
fi

target="$1"; shift
source ~/benchmark/scripts/_env.sh "$target"
source ~/benchmark/scripts/_metrics.sh

# Connect to a dedicated "latbench" database so we don't pollute postgres.
DBNAME=latbench

create_db() {
  $PSQL -d $PGDATABASE -c "DROP DATABASE IF EXISTS $DBNAME;" >/dev/null
  $PSQL -d $PGDATABASE -c "CREATE DATABASE $DBNAME;" >/dev/null
}

seed_schema() {
  echo "[seed] loading 3×1M rows into $DBNAME..."
  $PSQL -d $DBNAME -v ON_ERROR_STOP=1 -f ~/benchmark/sql/simple-latency-schema.sql
}

# Default test list = all 12.
tests=("$@")
if [[ ${#tests[@]} -eq 0 ]]; then
  tests=(L1 L2 L3 L4 L5 L6 L7 L8 L9 L10 L11 L12)
fi

# Map test id -> SQL file + label.
declare -A SQLFILE=(
  [L1]=L1-heap-insert.sql   [L2]=L2-heap-update.sql   [L3]=L3-heap-delete.sql
  [L4]=L4-pk-insert.sql     [L5]=L5-pk-update.sql     [L6]=L6-pk-delete.sql
  [L7]=L7-pksi-insert.sql   [L8]=L8-pksi-update.sql   [L9]=L9-pksi-delete.sql
  [L10]=L10-heap-select.sql [L11]=L11-pk-select.sql   [L12]=L12-pksi-select.sql
)
declare -A LABEL=(
  [L1]="heap INSERT"       [L2]="heap UPDATE (seq scan)"   [L3]="heap DELETE (seq scan)"
  [L4]="pk INSERT"         [L5]="pk UPDATE by PK"          [L6]="pk DELETE by PK"
  [L7]="pk+si INSERT"      [L8]="pk+si UPDATE by skey"     [L9]="pk+si DELETE by skey"
  [L10]="heap SELECT (seq scan)"   [L11]="pk SELECT by PK"  [L12]="pk+si SELECT by skey"
)

# (Re)seed schema once at start.
create_db
seed_schema

ts=$(date -u +%Y%m%d-%H%M)
outdir=~/benchmark/results/${ts}-${target}-L
mkdir -p "$outdir"
echo "[outdir] $outdir"

summary="$outdir/summary.txt"
echo "target=$target  scale=1M rows/table  client=1  warmup=5s  measure=3×30s" >"$summary"
echo "" >>"$summary"
printf "%-4s %-30s %9s %9s %9s %9s %9s %9s\n" \
  "id" "label" "lat1_ms" "lat2_ms" "lat3_ms" "tps1" "tps2" "tps3" >>"$summary"
printf "%-4s %-30s %9s %9s %9s %9s %9s %9s\n" \
  "----" "------------------------------" "--------" "--------" "--------" "--------" "--------" "--------" >>"$summary"

for t in "${tests[@]}"; do
  sql=~/benchmark/sql/${SQLFILE[$t]}
  log="$outdir/${t}.log"
  echo "[$t] ${LABEL[$t]}  →  $log"

  # Per-test CPU + disk-IO sampler + off-box PS cpu/mem sampler (local on this box).
  cpu_pid=$(start_server_cpu_sampler "$outdir/${t}-server-cpu.log")
  io_pid=$(start_server_io_sampler  "$outdir/${t}-server-io.log" nvme1n1)
  ps_pid=$(start_local_proc_sampler "$outdir/${t}-ps-cpu-mem.log" "yb-pageserver -D /mnt/ps/endpoints/main/pageserver")

  # Warmup (5 s, discarded).
  $PGBENCH -n -c 1 -j 1 -T 5 -f "$sql" -d $DBNAME -h $PGHOST -p $PGPORT -U $PGUSER \
    >/dev/null 2>&1 || true

  lats=()
  tpss=()
  for run in 1 2 3; do
    out=$($PGBENCH -n -c 1 -j 1 -T 30 -r -M prepared -f "$sql" \
            -d $DBNAME -h $PGHOST -p $PGPORT -U $PGUSER 2>&1) || true
    echo "=== $t run $run ===" >>"$log"
    echo "$out" >>"$log"
    echo "" >>"$log"
    lat=$(echo "$out" | awk '/^latency average/ {gsub(/[^0-9.]/,"",$4); print $4}')
    # pgbench prints either "tps = <n> (including connections establishing)"
    # or "tps = <n> (without initial connection time)". Take the first one.
    tps=$(echo "$out" | awk '/^tps =/ {print $3; exit}')
    lats+=("$lat")
    tpss+=("$tps")
  done
  stop_server_cpu_sampler "$cpu_pid"
  stop_server_io_sampler  "$io_pid"
  stop_local_proc_sampler "$ps_pid"
  summarize_io_sampler "$outdir/${t}-server-io.log" >"$outdir/${t}-io-summary.txt" 2>&1
  summarize_proc_sampler "$outdir/${t}-ps-cpu-mem.log" >"$outdir/${t}-ps-cpu-mem-summary.txt" 2>&1
  printf "%-4s %-30s %9s %9s %9s %9s %9s %9s\n" \
    "$t" "${LABEL[$t]}" "${lats[0]}" "${lats[1]}" "${lats[2]}" \
    "${tpss[0]}" "${tpss[1]}" "${tpss[2]}" >>"$summary"
done

echo
echo "=== summary ==="
cat "$summary"
