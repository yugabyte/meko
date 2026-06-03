#!/usr/bin/env bash
#
# End-to-end test runner. Iterates SQL workloads under test/e2e/<cat>/
# against a single shared neon_local cluster + tenant + endpoint,
# scans pageserver.log after each, and exits non-zero on the first
# corruption marker (or if any psql command errors out).
#
# Usage:
#   test/e2e/run.sh                # fast suite only (default)
#   test/e2e/run.sh fast           # explicit
#   test/e2e/run.sh slow           # slow suite only
#   test/e2e/run.sh all            # fast then slow
#   SKIP_RESET=1 test/e2e/run.sh   # reuse running cluster (assumes endpoint 'main' is up)
#
# Workloads are *.sql files under test/e2e/<category>/, run in lexical
# order. Each runs against a fresh schema (its own CREATE TABLE) so
# there's no cross-test state. The pageserver log is scanned ONCE at
# the end against the full set of corruption markers; per-workload
# scans add per-test attribution.
#
set -euo pipefail

REPO=$(git rev-parse --show-toplevel)
cd "$REPO"

CATEGORY=${1:-fast}
case "$CATEGORY" in
    fast|slow|all) ;;
    *) echo "usage: $0 [fast|slow|all]"; exit 2;;
esac

NEON_REPO_DIR=${NEON_REPO_DIR:-$REPO/.neon-localfs}
export NEON_REPO_DIR
export YB_PS_VERIFY_WAL_ROUNDTRIP=${YB_PS_VERIFY_WAL_ROUNDTRIP:-1}

YB_HOST=${YB_HOST:-127.0.0.1}
YB_PORT=${YB_PORT:-5433}
COMPUTE_PORT=${COMPUTE_PORT:-55432}

NEON=${NEON:-$REPO/target/debug/neon_local}
[[ -x $NEON ]] || { echo "neon_local binary not found at $NEON; run 'cargo build --bin neon_local'"; exit 1; }

step() { echo; echo "=== $* ==="; }

MARKERS='MATERIALIZER REDO ERROR|ABORT|PANIC|early eof|specified item offset|image row at lsn|deserialize error|WRITE-PATH ROUNDTRIP'

log_baseline_lines=0

# Bring up a fresh cluster + tenant + endpoint, unless caller opted out.
if [[ "${SKIP_RESET:-0}" == "0" ]]; then
    step "Stop any running pageserver/neon processes"
    pkill -9 -f "yb-pageserver|safekeeper|storage_broker|endpoint_storag|storage_controller|neon_local" 2>/dev/null || true
    sleep 2

    step "Drop + recreate yb_storage"
    PGPASSWORD=yugabyte psql -h "$YB_HOST" -p "$YB_PORT" -U yugabyte -d yugabyte \
        -c "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname='yb_storage'" >/dev/null 2>&1 || true
    PGPASSWORD=yugabyte psql -h "$YB_HOST" -p "$YB_PORT" -U yugabyte -d yugabyte -c "DROP DATABASE IF EXISTS yb_storage" >/dev/null
    PGPASSWORD=yugabyte psql -h "$YB_HOST" -p "$YB_PORT" -U yugabyte -d yugabyte -c "CREATE DATABASE yb_storage" >/dev/null
    rm -rf "$NEON_REPO_DIR"

    step "neon_local init + start"
    "$NEON" init --config /tmp/neon-init.toml
    "$NEON" start

    step "tenant + endpoint"
    TID=$("$NEON" tenant create --pg-version 16 2>&1 | awk '/successfully created/{print $2}')
    [[ -n $TID ]] || { echo "tenant create failed"; exit 1; }
    echo "tenant: $TID"
    "$NEON" endpoint create main --tenant-id "$TID" --branch-name main --pg-version 16 >/dev/null
    "$NEON" endpoint start main >/dev/null
fi

LOG=$NEON_REPO_DIR/pageserver_1/pageserver.log

# Per-workload runner: prints the workload's psql output, then
# attributes any new corruption markers to it.
run_workload() {
    local sql=$1
    local rel=${sql#$REPO/}
    step "workload: $rel"
    local before_lines after_lines
    before_lines=$(wc -l < "$LOG")
    local t0 t1
    t0=$(date +%s)
    PGPASSWORD= psql -h 127.0.0.1 -p "$COMPUTE_PORT" -U cloud_admin postgres \
        -v ON_ERROR_STOP=1 -f "$sql"
    t1=$(date +%s)
    after_lines=$(wc -l < "$LOG")
    local new_hits
    new_hits=$(tail -n $((after_lines - before_lines)) "$LOG" | grep -cE "$MARKERS" || true)
    if (( new_hits > 0 )); then
        echo "FAIL — $new_hits corruption marker(s) introduced by $rel"
        tail -n $((after_lines - before_lines)) "$LOG" | grep -E "$MARKERS" | head -5
        exit 1
    fi
    echo "  ok  ($(( t1 - t0 ))s, +$((after_lines - before_lines)) log lines)"
}

run_category() {
    local cat=$1
    local dir=$REPO/test/e2e/$cat
    [[ -d $dir ]] || { echo "skip $cat (no directory)"; return 0; }
    local found=0
    for sql in "$dir"/*.sql; do
        [[ -f $sql ]] || continue
        found=1
        run_workload "$sql"
    done
    if (( found == 0 )); then
        echo "skip $cat (no .sql files)"
    fi
}

case "$CATEGORY" in
    fast) run_category fast ;;
    slow) run_category slow ;;
    all)  run_category fast; run_category slow ;;
esac

step "Final verdict"
COUNTER=$(curl -sS http://127.0.0.1:9989/metrics 2>/dev/null \
    | grep -E '^pageserver_yb_correctness_errors_total\{' || true)
if [[ -n $COUNTER ]]; then
    echo "FAIL — correctness counter non-zero:"
    echo "$COUNTER"
    exit 1
fi
echo "PASS — all workloads clean, counter at 0"
