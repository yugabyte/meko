#!/usr/bin/env bash
# pgbench RO/RW (W1..W4).
# Usage: ./pgbench-run.sh ybLight|aurora W1|W2|W3|W4 [...]
#
# W1: -S, scale=100  (RO, fits)
# W2: default, scale=100 (RW, fits)
# W3: -S, scale=1500 (RO, doesn't fit)
# W4: default, scale=1500 (RW, doesn't fit)
#
# Init is skipped if pgbench_accounts is already present at the requested scale.

set -euo pipefail

if [[ $# -lt 2 ]]; then
  echo "Usage: $0 ybLight|aurora W1|W2|W3|W4 [...]" >&2
  exit 1
fi

target="$1"; shift
source ~/benchmark/scripts/_env.sh "$target"
source ~/benchmark/scripts/_metrics.sh

declare -A W_SCALE=( [W1]=100 [W2]=100 [W3]=1500 [W4]=1500 )
declare -A W_KIND=(  [W1]=-S  [W2]=""  [W3]=-S   [W4]=""  )
declare -A W_LABEL=( [W1]="RO scale=100" [W2]="RW scale=100" [W3]="RO scale=1500" [W4]="RW scale=1500" )

# Use one pgbench DB per scale so init can be skipped when re-running.
db_for_scale() { echo "pgbench_s$1"; }

ensure_init() {
  local scale="$1" dbname; dbname=$(db_for_scale "$scale")
  if $PSQL -d $PGDATABASE -tAc "SELECT 1 FROM pg_database WHERE datname='$dbname'" | grep -q 1; then
    # pgbench_branches has exactly `scale` rows.
    local actual; actual=$($PSQL -d "$dbname" -tAc \
      "SELECT count(*) FROM pgbench_branches" 2>/dev/null || echo 0)
    if [[ "$actual" == "$scale" ]]; then
      echo "[init] $dbname already at scale=$actual — reusing"
      return 0
    fi
  fi
  echo "[init] creating $dbname at scale=$scale"
  $PSQL -d $PGDATABASE -c "DROP DATABASE IF EXISTS $dbname;" >/dev/null
  $PSQL -d $PGDATABASE -c "CREATE DATABASE $dbname;" >/dev/null
  $PGBENCH -i -s "$scale" \
    -d "$dbname" -h $PGHOST -p $PGPORT -U $PGUSER 2>&1 | tail -5
}

for w in "$@"; do
  scale="${W_SCALE[$w]}"
  kind="${W_KIND[$w]}"
  label="${W_LABEL[$w]}"
  dbname=$(db_for_scale "$scale")

  ensure_init "$scale"

  ts=$(date -u +%Y%m%d-%H%M)
  outdir=~/benchmark/results/${ts}-${target}-${w}
  mkdir -p "$outdir"
  echo "[$w] $label → $outdir"

  # Warmup (60 s, discarded)
  $PGBENCH $kind -c 8 -j 4 -T 60 -n \
    -d "$dbname" -h $PGHOST -p $PGPORT -U $PGUSER >"$outdir/warmup.log" 2>&1 || true

  # Start CPU sampler (compute, via ssh) + off-box PS cpu/mem sampler (local) + pg_stat snapshot
  cpu_pid=$(start_server_cpu_sampler "$outdir/server-cpu.log")
  ps_pid=$(start_local_proc_sampler "$outdir/ps-cpu-mem.log" "yb-pageserver -D /mnt/ps/endpoints/main/pageserver")
  snapshot_ps_metrics "$outdir/ps-metrics-start.txt"
  snapshot_pg_stat "$dbname" "$outdir/pg-stat-start.raw"
  mark_timestamp "$outdir" "START"

  # Measurement (5 min)
  $PGBENCH $kind -c 8 -j 4 -T 300 -P 10 -n -r \
    -d "$dbname" -h $PGHOST -p $PGPORT -U $PGUSER 2>&1 | tee "$outdir/stdout.log"

  # Stop sampler, take post snapshot, diff
  mark_timestamp "$outdir" "END"
  stop_server_cpu_sampler "$cpu_pid"
  stop_local_proc_sampler "$ps_pid"
  snapshot_ps_metrics "$outdir/ps-metrics-end.txt"
  snapshot_pg_stat "$dbname" "$outdir/pg-stat-end.raw"
  diff_pg_stat "$outdir/pg-stat-start.raw" "$outdir/pg-stat-end.raw" >"$outdir/pg-stat-delta.txt" 2>&1
  echo "--- pg-stat-delta ---"
  cat "$outdir/pg-stat-delta.txt"
  echo "--- off-box PS cpu/mem ---"
  summarize_proc_sampler "$outdir/ps-cpu-mem.log" | tee "$outdir/ps-cpu-mem-summary.txt"
done
