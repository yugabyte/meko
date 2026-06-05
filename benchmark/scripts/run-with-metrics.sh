#!/usr/bin/env bash
# Run an arbitrary workload command with server-side CPU + disk IOPS/throughput
# sampling around it. Captures into a timestamped results dir.
#   ./run-with-metrics.sh <target> <label> -- <command...>
# Example:
#   ./run-with-metrics.sh ybLight L -- bash ~/benchmark/scripts/big-load.sh ybLight
set -uo pipefail
target="${1:?usage: $0 <target> <label> -- <cmd...>}"; shift
label="${1:?need label}"; shift
[[ "${1:-}" == "--" ]] && shift

source ~/benchmark/scripts/_env.sh "$target"
source ~/benchmark/scripts/_metrics.sh

ts=$(date -u +%Y%m%d-%H%M)
outdir=~/benchmark/results/${ts}-${target}-8kcolo-${label}
mkdir -p "$outdir"
export CPU_SAMPLE_HOST=172.151.29.184   # ybLight (compute + PS + control plane)

echo "[run-with-metrics] label=$label outdir=$outdir"
echo "[run-with-metrics] cmd: $*"

cpu_pid=$(start_server_cpu_sampler "$outdir/server-cpu.log")
io_pid=$(start_server_io_sampler  "$outdir/server-io.log" nvme1n1)
mark_timestamp "$outdir" START

# Run the workload, teeing its output.
( "$@" ) 2>&1 | tee "$outdir/run.log"
rc=${PIPESTATUS[0]}

mark_timestamp "$outdir" END
stop_server_cpu_sampler "$cpu_pid"
stop_server_io_sampler  "$io_pid"

{
  echo "=== $label  (rc=$rc) ==="
  echo "--- disk IOPS/throughput (nvme1n1 = /mnt/d0) ---"
  summarize_io_sampler "$outdir/server-io.log"
  echo
  echo "--- compute/PS CPU (peak per-proc samples; see server-cpu.log) ---"
  # Top CPU consumers across the run, by process name.
  awk '/pid=/ {
         match($0,/cpu=[ ]*[0-9.]+/); c=substr($0,RSTART+4,RLENGTH-4)+0;
         name=$NF; if(c>max[name]) max[name]=c; sum[name]+=c; n[name]++ }
       END { for(k in max) printf "  %-16s peak=%5.1f%%  avg=%5.1f%%  (n=%d)\n", k, max[k], sum[k]/n[k], n[k] }' \
    "$outdir/server-cpu.log" | sort -t= -k2 -rn
} | tee "$outdir/metrics-summary.txt"

echo "[run-with-metrics] done rc=$rc  results in $outdir"
exit $rc
