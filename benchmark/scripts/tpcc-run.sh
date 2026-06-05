#!/usr/bin/env bash
# TPC-C (W5, W6) — drives the existing tpcc-stage benchmark.
# Usage: ./tpcc-run.sh ybLight|aurora W5|W6 [create|load|execute] ...
#   With no phase: runs create → load → execute
#   With phase list: runs only those phases (idempotent reuse)
#
# W5: 50 wh (fits in cache)
# W6: 250 wh (doesn't fit)

set -euo pipefail

if [[ $# -lt 2 ]]; then
  echo "Usage: $0 ybLight|aurora W5|W6 [create|load|execute] ..." >&2
  exit 1
fi

target="$1"; shift
test_id="$1"; shift
phases=("$@")
if [[ ${#phases[@]} -eq 0 ]]; then
  phases=(create load execute)
fi

source ~/benchmark/scripts/_env.sh "$target"
source ~/benchmark/scripts/_metrics.sh

declare -A WH=( [W5]=50 [W6]=250 [W7]=500 [W8]=750 [W9]=100 [W10]=1000 )
declare -A LABEL=( [W5]="TPC-C 50 wh" [W6]="TPC-C 250 wh" [W7]="TPC-C 500 wh" [W8]="TPC-C 750 wh" [W9]="TPC-C 100 wh" [W10]="TPC-C 1000 wh" )
wh="${WH[$test_id]}"
label="${LABEL[$test_id]}"

# Per-target XML — copy from a template, fix jdbcURL and warehouses.
xml=~/tpcc-stage/tpcc/config/workload_${target}_${test_id}.xml
case "$target" in
  ybLight*)  jdbc="jdbc:postgresql://$PGHOST:$PGPORT/tpcc?sslmode=disable&reWriteBatchedInserts=true&tcpKeepAlive=true"; user="cloud_admin" ;;
  aurora*)  jdbc="jdbc:postgresql://$PGHOST:$PGPORT/tpcc?sslmode=require&reWriteBatchedInserts=true&tcpKeepAlive=true"; user="postgres" ;;
esac

# Make sure tpcc DB exists.
$PSQL -d $PGDATABASE -tAc "SELECT 1 FROM pg_database WHERE datname='tpcc'" | grep -q 1 \
  || $PSQL -d $PGDATABASE -c "CREATE DATABASE tpcc;"

# Build the XML by patching the existing template.
cp ~/tpcc-stage/tpcc/config/workload_neon_pg.xml "$xml"
# In XML, & must be &amp;. Also escape & in the sed replacement (sed treats bare & as the match).
jdbc_xml=${jdbc//&/&amp;}
jdbc_sed=${jdbc_xml//&/\\&}
sed -i "s|<jdbcURL>.*</jdbcURL>|<jdbcURL>${jdbc_sed}</jdbcURL>|" "$xml"
sed -i "s|<username>.*</username>|<username>${user}</username>|" "$xml"
sed -i 's|<password>.*</password>|<password>Yugabyte!1</password>|' "$xml"
sed -i "s|<port>.*</port>|<port>${PGPORT}</port>|" "$xml"
# Force pacing + configurable runtime (seconds; default 10 min, set TPCC_RUNTIME=1800 for 30 min)
runtime="${TPCC_RUNTIME:-600}"
sed -i 's|<useKeyingTime>.*</useKeyingTime>|<useKeyingTime>true</useKeyingTime>|' "$xml"
sed -i 's|<useThinkTime>.*</useThinkTime>|<useThinkTime>true</useThinkTime>|' "$xml"
sed -i "s|<runtime>.*</runtime>|<runtime>${runtime}</runtime>|" "$xml"

ts=$(date -u +%Y%m%d-%H%M)
outdir=~/benchmark/results/${ts}-${target}-${test_id}
mkdir -p "$outdir"
cp "$xml" "$outdir/workload.xml"
echo "[$test_id] $label → $outdir"

export JAVA_HOME=~/jdk/jdk-17.0.19+10
export PATH=$JAVA_HOME/bin:$PATH

cd ~/tpcc-stage/tpcc

for phase in "${phases[@]}"; do
  case "$phase" in
    create)
      ./tpccbenchmark -c "$xml" --create=true --warehouses=$wh --nodes=$PGHOST \
        2>&1 | tee "$outdir/create.log"
      ;;
    load)
      # Cap loader threads to avoid OOM on the small ybLight box (PS RAM
      # backpressure when compute writes outrun YB uploads at 2 vCPU).
      lthreads=${TPCC_LOADER_THREADS:-4}
      cpu_pid=$(start_server_cpu_sampler "$outdir/server-cpu-load.log")
      io_pid=$(start_server_io_sampler  "$outdir/server-io-load.log" nvme1n1)
      ./tpccbenchmark -c "$xml" --load=true --warehouses=$wh --loaderthreads=$lthreads --nodes=$PGHOST \
        2>&1 | tee "$outdir/load.log"
      stop_server_cpu_sampler "$cpu_pid"
      stop_server_io_sampler  "$io_pid"
      summarize_io_sampler "$outdir/server-io-load.log" >"$outdir/io-summary-load.txt" 2>&1
      echo "--- io-summary (load) ---"; cat "$outdir/io-summary-load.txt"
      ;;
    execute)
      cpu_pid=$(start_server_cpu_sampler "$outdir/server-cpu.log")
      io_pid=$(start_server_io_sampler  "$outdir/server-io.log" nvme1n1)
      # Off-box pageserver runs on this harness host (16-core box) in split mode.
      ps_pid=$(start_local_proc_sampler "$outdir/ps-cpu-rss.log" yb-pageserver)
      snapshot_ps_metrics "$outdir/ps-metrics-start.txt"
      snapshot_pg_stat tpcc "$outdir/pg-stat-start.raw"
      mark_timestamp "$outdir" "START"
      ./tpccbenchmark -c "$xml" --execute=true --warehouses=$wh --nodes=$PGHOST \
        --num-connections=${TPCC_CONNS:-80} \
        --warmup-time-secs=${TPCC_WARMUP:-0} \
        2>&1 | tee "$outdir/execute.log"
      mark_timestamp "$outdir" "END"
      stop_server_cpu_sampler "$cpu_pid"
      stop_server_io_sampler  "$io_pid"
      stop_local_proc_sampler "$ps_pid"
      snapshot_ps_metrics "$outdir/ps-metrics-end.txt"
      summarize_proc_sampler "$outdir/ps-cpu-rss.log" >"$outdir/ps-cpu-rss-summary.txt" 2>&1
      echo "--- ps-cpu-rss summary ---"; cat "$outdir/ps-cpu-rss-summary.txt"
      snapshot_pg_stat tpcc "$outdir/pg-stat-end.raw"
      diff_pg_stat "$outdir/pg-stat-start.raw" "$outdir/pg-stat-end.raw" >"$outdir/pg-stat-delta.txt" 2>&1
      summarize_io_sampler "$outdir/server-io.log" >"$outdir/io-summary.txt" 2>&1
      echo "--- pg-stat-delta ---"; cat "$outdir/pg-stat-delta.txt"
      echo "--- io-summary (execute) ---"; cat "$outdir/io-summary.txt"
      ;;
  esac
done
