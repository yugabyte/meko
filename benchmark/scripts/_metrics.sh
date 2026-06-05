#!/usr/bin/env bash
# Helpers for capturing CPU + DB metrics during a measurement run.
# Source after sourcing _env.sh.

# Start a server-side CPU sampler. Only meaningful for ybLight (we have ssh).
# Writes to $1 (output file path). Returns sampler PID.
start_server_cpu_sampler() {
  local outfile="$1"
  if [[ "$TARGET" != ybLight* ]]; then
    echo "(server-cpu sampling only supported for ybLight target)" >"$outfile"
    return 0
  fi
  # Sample the compute host (== PGHOST for ybLight targets) unless overridden.
  ssh -o StrictHostKeyChecking=no -i ~/.ssh/yb-15-aws-portal-1-key.pem \
    ec2-user@"${CPU_SAMPLE_HOST:-$PGHOST}" \
    'while true; do
       echo "--- $(date +%T) ---"
       top -bn2 -d 1 -w 512 2>/dev/null \
         | awk "BEGIN{it=0} /^top - / {it++} it==2 && /Cpu\\(s\\)/ {print; next} \
                it==2 && /^[[:space:]]*[0-9]+ / && \$NF ~ /pageserver|safekeeper|storage_broker|storage_controller|endpoint_storage|postgres|compute_ctl/ \
                  {printf \"  pid=%-6s cpu=%5s%%  res=%-6s  %s\\n\", \$1, \$9, \$6, \$NF}"
       sleep 3
     done' >"$outfile" 2>&1 &
  echo $!
}

stop_server_cpu_sampler() {
  local pid="$1"
  if [[ -n "$pid" ]]; then
    kill "$pid" 2>/dev/null || true
    wait "$pid" 2>/dev/null || true
  fi
}

# Sample a LOCAL process' CPU% + RSS over time. Use this for the off-box
# pageserver running on the same host as the harness (172.151.20.28), where the
# remote start_server_cpu_sampler does not reach. $1=outfile, $2=pgrep pattern
# (default "bin/pageserver"). Returns sampler PID. RSS is reported in MB.
start_local_proc_sampler() {
  local outfile="$1" pat="${2:-bin/pageserver}"
  ( while true; do
      ts=$(date +%T)
      for pid in $(pgrep -f "$pat"); do
        # %cpu is instantaneous-since-last-top; rss is in KB from ps.
        read -r cpu rss cmd < <(ps -o %cpu=,rss=,comm= -p "$pid" 2>/dev/null)
        [[ -n "$rss" ]] && printf '%s pid=%-6s cpu=%5s%%  rss=%6.1fMB  %s\n' \
          "$ts" "$pid" "$cpu" "$(awk "BEGIN{print $rss/1024}")" "$cmd"
      done
      sleep 3
    done ) >"$outfile" 2>&1 &
  echo $!
}

stop_local_proc_sampler() {
  local pid="$1"
  if [[ -n "$pid" ]]; then
    kill "$pid" 2>/dev/null || true
    wait "$pid" 2>/dev/null || true
  fi
}

# Summarize a start_local_proc_sampler logfile: avg/peak cpu% and avg/peak RSS(MB)
# across all samples for the matched process. $1=logfile.
summarize_proc_sampler() {
  local logfile="$1"
  awk '
    /yb-pageserver/ {
      c=""; r=""
      if (match($0, /cpu=[[:space:]]*[0-9.]+/)) { c=substr($0,RSTART,RLENGTH); gsub(/[^0-9.]/,"",c) }
      if (match($0, /rss=[[:space:]]*[0-9.]+/)) { r=substr($0,RSTART,RLENGTH); gsub(/[^0-9.]/,"",r) }
      if (c!="" && r!="") { n++; cs+=c; if(c>cp)cp=c; rs+=r; if(r>rp)rp=r }
    }
    END {
      if (n==0) { print "(no proc samples)"; exit }
      printf "samples:   %d\n", n
      printf "cpu%%:      avg=%.1f peak=%.1f\n", cs/n, cp
      printf "rss(MB):   avg=%.1f peak=%.1f\n", rs/n, rp
    }' "$logfile"
}

# Fetch the yb-pageserver backpressure/queue Prometheus metrics from the off-box
# PS HTTP endpoint into $1. Captures the new backpressure counters/gauges so we
# can see whether backpressure engaged during a run.
snapshot_ps_metrics() {
  local outfile="$1"
  curl -s "http://172.151.20.28:9988/metrics" 2>/dev/null \
    | grep -E 'pagewriter_backpressure_waits_total|pagewriter_unflushed_bytes|materializer_queue_depth' \
    >"$outfile" 2>&1 || echo "(metrics fetch failed)" >"$outfile"
}

# Start a server-side disk IOPS/throughput sampler via iostat. Only meaningful
# for ybLight (we have ssh). Samples the device backing /mnt/d0 (all neon-state:
# pgdata, WAL, safekeeper, LFC file) every 3s. $1=outfile, $2=device (default
# nvme1n1). Returns sampler PID. Each report block is prefixed with a timestamp.
start_server_io_sampler() {
  local outfile="$1" dev="${2:-nvme1n1}"
  if [[ "$TARGET" != ybLight* ]]; then
    echo "(server-io sampling only supported for ybLight target)" >"$outfile"
    return 0
  fi
  ssh -o StrictHostKeyChecking=no -i ~/.ssh/yb-15-aws-portal-1-key.pem \
    ec2-user@"${CPU_SAMPLE_HOST:-$PGHOST}" \
    "stdbuf -oL -eL iostat -dxyt 3 $dev" \
    >"$outfile" 2>&1 &
  echo $!
}

stop_server_io_sampler() {
  local pid="$1"
  if [[ -n "$pid" ]]; then
    kill "$pid" 2>/dev/null || true
    wait "$pid" 2>/dev/null || true
  fi
}

# Summarize an iostat sampler logfile: avg/peak r/s, w/s, rkB/s, wkB/s, %util.
# Column positions follow `iostat -dx` (sysstat). $1=logfile.
summarize_io_sampler() {
  local logfile="$1"
  python3 - "$logfile" <<'PY'
import sys
rows=[]
hdr=None
for ln in open(sys.argv[1]):
    parts=ln.split()
    if not parts: continue
    if parts[0]=='Device':
        hdr=parts; continue
    # device data rows start with the device name and have many numeric cols
    if hdr and parts[0]==hdr[0]: continue
    if hdr and len(parts)==len(hdr) and parts[0] not in ('Device','Linux'):
        rows.append(parts)
if not rows or not hdr:
    print("(no iostat samples)"); sys.exit(0)
# map header names -> index
idx={name:i for i,name in enumerate(hdr)}
def col(name,alts=()):
    for n in (name,)+alts:
        if n in idx: return idx[n]
    return None
def series(name,alts=()):
    i=col(name,alts)
    if i is None: return []
    out=[]
    for r in rows:
        try: out.append(float(r[i]))
        except (IndexError,ValueError): pass
    return out
def stat(name,alts=()):
    s=series(name,alts)
    if not s: return "n/a"
    return f"avg={sum(s)/len(s):.1f} peak={max(s):.1f}"
print(f"samples:   {len(rows)}")
print(f"r/s:       {stat('r/s')}")
print(f"w/s:       {stat('w/s')}")
print(f"rkB/s:     {stat('rkB/s')}")
print(f"wkB/s:     {stat('wkB/s')}")
print(f"%util:     {stat('%util')}")
print(f"aqu-sz:    {stat('aqu-sz',('avgqu-sz',))}")
PY
}

# Record an ISO + epoch timestamp line to outdir/timestamps.txt.
# Use START / END markers around the measurement run for easy CloudWatch lookup.
mark_timestamp() {
  local outdir="$1" marker="$2"
  printf "%s  %s  %s\n" \
    "$marker" "$(date -u --iso-8601=seconds)" "$(date -u +%s)" \
    >>"$outdir/timestamps.txt"
}

# Snapshot pg_stat_database for the given dbname. Captures the columns we
# care about: tx count, hit/read, tuples written.
snapshot_pg_stat() {
  local dbname="$1" outfile="$2"
  $PSQL -d "$dbname" -h "$PGHOST" -p "$PGPORT" -U "$PGUSER" -At -F'|' -c "
    SELECT
      extract(epoch FROM now())::numeric(20,3),
      xact_commit, xact_rollback,
      blks_hit, blks_read,
      tup_returned, tup_fetched,
      tup_inserted, tup_updated, tup_deleted,
      conflicts, deadlocks
    FROM pg_stat_database WHERE datname=current_database();
  " >"$outfile" 2>&1
}

# Diff two snapshots into a human-readable summary (writes to stdout).
diff_pg_stat() {
  local start="$1" end="$2"
  python3 - "$start" "$end" <<'PY'
import sys
def parse(p):
    with open(p) as f: line=f.read().strip()
    if not line or '|' not in line: return None
    p=line.split('|')
    return {
        'ts':float(p[0]),
        'commit':int(p[1]),'rollback':int(p[2]),
        'hit':int(p[3]),'read':int(p[4]),
        'returned':int(p[5]),'fetched':int(p[6]),
        'ins':int(p[7]),'upd':int(p[8]),'del':int(p[9]),
        'conflicts':int(p[10]),'deadlocks':int(p[11]),
    }
a=parse(sys.argv[1]); b=parse(sys.argv[2])
if not a or not b: print("(snapshot missing/empty)"); sys.exit(0)
dt=b['ts']-a['ts']
if dt<=0: print("(no time elapsed)"); sys.exit(0)
def d(k): return b[k]-a[k]
total_blks=d('hit')+d('read')
hit_pct = (d('hit')/total_blks*100) if total_blks else 0
print(f"window:           {dt:.1f}s")
print(f"tps (commit+rb):  {(d('commit')+d('rollback'))/dt:.1f}")
print(f"commit/s:         {d('commit')/dt:.1f}    rollback/s: {d('rollback')/dt:.1f}")
print(f"blks_hit/s:       {d('hit')/dt:.0f}     blks_read/s: {d('read')/dt:.0f}")
print(f"hit ratio:        {hit_pct:.2f}%")
print(f"tuples ins/upd/del per s: {d('ins')/dt:.0f}/{d('upd')/dt:.0f}/{d('del')/dt:.0f}")
print(f"conflicts:        {d('conflicts')}    deadlocks: {d('deadlocks')}")
PY
}
