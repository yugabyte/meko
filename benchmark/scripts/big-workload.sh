#!/usr/bin/env bash
# Run big-table workloads (B1..B5) against a target.
# Usage: ./big-workload.sh ybLight|aurora [config_suffix] [test_id ...]
#   config_suffix : optional dir suffix (e.g. "noLfc", "ps1g")
#   test_id list  : default B1 B2 B3 B4 B5

set -euo pipefail
target="${1:?usage: $0 ybLight|aurora [suffix] [B1...]}"; shift
suffix="${1:-}"
[[ "$suffix" =~ ^B[1-5]$ ]] && suffix="" || { [[ $# -gt 0 ]] && shift; :; }

source ~/benchmark/scripts/_env.sh "$target"
source ~/benchmark/scripts/_metrics.sh

DBNAME=bigbench

declare -A SQLFILE=(
  [B1]=B1-random-point.sql
  [B2]=B2-range-aggregate.sql
  [B3]=B3-random-update.sql
  [B4]=B4-range-insert.sql
  [B5]=B5-range-update.sql
)
declare -A LABEL=(
  [B1]="random point lookup (PK)"
  [B2]="range aggregate (by cat)"
  [B3]="random update (by PK)"
  [B4]="range insert (1000-row batch)"
  [B5]="range update (by cat)"
)

tests=("$@")
[[ ${#tests[@]} -eq 0 ]] && tests=(B1 B2 B3 B4 B5)

ts=$(date -u +%Y%m%d-%H%M)
outdir=~/benchmark/results/${ts}-${target}-B${suffix:+-$suffix}
mkdir -p "$outdir"
echo "[outdir] $outdir"

summary="$outdir/summary.txt"
echo "target=$target  dataset=big_table (100M rows ~20GB)  client=1  warmup=30s  measure=3x60s${suffix:+  config=$suffix}" >"$summary"
echo "" >>"$summary"
printf "%-4s %-34s %9s %9s %9s %9s %9s %9s\n" \
  "id" "label" "lat1_ms" "lat2_ms" "lat3_ms" "tps1" "tps2" "tps3" >>"$summary"
printf "%-4s %-34s %9s %9s %9s %9s %9s %9s\n" \
  "----" "----------------------------------" "--------" "--------" "--------" "--------" "--------" "--------" >>"$summary"

for t in "${tests[@]}"; do
  sql=~/benchmark/sql/${SQLFILE[$t]}
  log="$outdir/${t}.log"
  echo "[$t] ${LABEL[$t]}  →  $log"

  # Per-test CPU + disk-IO sampler + pg-stat
  cpu_pid=$(start_server_cpu_sampler "$outdir/${t}-server-cpu.log")
  io_pid=$(start_server_io_sampler  "$outdir/${t}-server-io.log" nvme1n1)
  snapshot_pg_stat "$DBNAME" "$outdir/${t}-pg-stat-start.raw"
  mark_timestamp "$outdir" "${t}_START"

  # Warmup (30 s discarded)
  $PGBENCH -n -c 1 -j 1 -T 30 -M prepared -f "$sql" \
    -d $DBNAME -h $PGHOST -p $PGPORT -U $PGUSER >/dev/null 2>&1 || true

  lats=(); tpss=()
  for run in 1 2 3; do
    out=$($PGBENCH -n -c 1 -j 1 -T 60 -r -M prepared -f "$sql" \
            -d $DBNAME -h $PGHOST -p $PGPORT -U $PGUSER 2>&1)
    echo "=== $t run $run ===" >>"$log"
    echo "$out" >>"$log"
    echo "" >>"$log"
    lat=$(echo "$out" | awk '/^latency average/ {gsub(/[^0-9.]/,"",$4); print $4}')
    tps=$(echo "$out" | awk '/^tps =/ {print $3; exit}')
    lats+=("$lat"); tpss+=("$tps")
  done

  mark_timestamp "$outdir" "${t}_END"
  stop_server_cpu_sampler "$cpu_pid"
  stop_server_io_sampler  "$io_pid"
  snapshot_pg_stat "$DBNAME" "$outdir/${t}-pg-stat-end.raw"
  diff_pg_stat "$outdir/${t}-pg-stat-start.raw" "$outdir/${t}-pg-stat-end.raw" \
    >"$outdir/${t}-pg-stat-delta.txt" 2>&1
  summarize_io_sampler "$outdir/${t}-server-io.log" >"$outdir/${t}-io-summary.txt" 2>&1

  printf "%-4s %-34s %9s %9s %9s %9s %9s %9s\n" \
    "$t" "${LABEL[$t]}" "${lats[0]}" "${lats[1]}" "${lats[2]}" \
    "${tpss[0]}" "${tpss[1]}" "${tpss[2]}" >>"$summary"
done

echo
echo "=== summary ==="
cat "$summary"
