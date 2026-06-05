#!/usr/bin/env bash
# Samples the off-box yb-pageserver RSS/CPU + backpressure metrics every 5s.
# Usage: ps-rss-sampler.sh <outfile> [pattern]
out="${1:?outfile}"
pat="${2:-yb-pageserver -D}"
ps_http="http://172.151.20.28:9988/metrics"
{
  echo "# ps-rss sampler start $(date -u +%FT%TZ) pattern='$pat'"
  while true; do
    ts=$(date +%T)
    for pid in $(pgrep -f "$pat"); do
      read -r cpu rss < <(ps -o %cpu=,rss= -p "$pid" 2>/dev/null)
      [[ -n "$rss" ]] && printf '%s pid=%s cpu=%s%% rss=%.1fMB' "$ts" "$pid" "$cpu" "$(awk "BEGIN{print $rss/1024}")"
    done
    # append key backpressure/queue gauges on the same line
    m=$(curl -s --max-time 3 "$ps_http" 2>/dev/null | grep -E '^pageserver_yb_(materializer_queue_depth|materializer_inflight_bytes|pagewriter_unflushed_bytes|pagewriter_backpressure|pagewriter_delta_counts_entries|pagewriter_delta_counts_approx_bytes|unified_pages_by_key_entries)' | tr '\n' ' ')
    printf '  %s\n' "$m"
    sleep 5
  done
} >"$out" 2>&1
