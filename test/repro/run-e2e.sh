#!/usr/bin/env bash
#
# End-to-end corruption smoke test. Brings up neon_local against an
# already-running YB cluster (assumed at 127.0.0.1:5433, yugabyte/yugabyte),
# creates a tenant + endpoint, runs the canonical workload, and scans
# the pageserver log for the marker set used by the 2026-06-02 Btree
# INSERT_UPPER walredo PANIC.
#
# Exit code 0 = clean. Non-zero = at least one marker fired.
#
# Usage:
#   test/repro/run-e2e.sh              # default — full reset, fresh tenant
#   SKIP_RESET=1 test/repro/run-e2e.sh # reuse existing cluster (assumes endpoint 'main' is up)
#
set -euo pipefail

REPO=$(git rev-parse --show-toplevel)
cd "$REPO"

NEON_REPO_DIR=${NEON_REPO_DIR:-$REPO/.neon-localfs}
export NEON_REPO_DIR
export YB_PS_VERIFY_WAL_ROUNDTRIP=${YB_PS_VERIFY_WAL_ROUNDTRIP:-1}

YB_HOST=${YB_HOST:-127.0.0.1}
YB_PORT=${YB_PORT:-5433}
COMPUTE_PORT=${COMPUTE_PORT:-55432}

NEON=${NEON:-$REPO/target/release/neon_local}
[[ -x $NEON ]] || { echo "neon_local binary not found at $NEON; run 'cargo build --release --bin neon_local'"; exit 1; }

step() { echo; echo "=== $* ==="; }

if [[ "${SKIP_RESET:-0}" == "0" ]]; then
    step "Stop any running pageserver/neon processes"
    pkill -9 -f "yb-pageserver|safekeeper|storage_broker|endpoint_storag|storage_controller|neon_local" 2>/dev/null || true
    sleep 2

    step "Drop + recreate yb_storage (terminating idle sessions first)"
    PGPASSWORD=yugabyte psql -h "$YB_HOST" -p "$YB_PORT" -U yugabyte -d yugabyte \
        -c "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname='yb_storage'" >/dev/null 2>&1 || true
    PGPASSWORD=yugabyte psql -h "$YB_HOST" -p "$YB_PORT" -U yugabyte -d yugabyte -c "DROP DATABASE IF EXISTS yb_storage" >/dev/null
    PGPASSWORD=yugabyte psql -h "$YB_HOST" -p "$YB_PORT" -U yugabyte -d yugabyte -c "CREATE DATABASE yb_storage" >/dev/null
    rm -rf "$NEON_REPO_DIR"

    step "neon_local init"
    "$NEON" init --config /tmp/neon-init.toml

    step "neon_local start (first attempt — may wedge on Phase 4.k schema bootstrap)"
    if ! "$NEON" start --start-timeout 240s; then
        echo "First start failed; workaround Phase 4.k (manually create pageserver.timelines + restart)…"
        PGPASSWORD=yugabyte psql -h "$YB_HOST" -p "$YB_PORT" -U yugabyte -d yb_storage -c "
            CREATE TABLE IF NOT EXISTS pageserver.timelines (
                tenant_id              TEXT    NOT NULL,
                timeline_id            TEXT    NOT NULL,
                last_record_lsn        BIGINT  NOT NULL,
                disk_consistent_lsn    BIGINT  NOT NULL,
                applied_gc_cutoff_lsn  BIGINT  NOT NULL,
                ancestor_timeline_id   TEXT,
                ancestor_lsn           BIGINT,
                current_generation     INT     NOT NULL,
                pg_version             INT     NOT NULL,
                PRIMARY KEY ((tenant_id) HASH, timeline_id))" >/dev/null
        pkill -9 -f "neon_local|yb-pageserver|safekeeper|storage_broker|endpoint_storag|storage_controller" 2>/dev/null || true
        sleep 2
        "$NEON" start --start-timeout 240s
    fi

    step "tenant + endpoint"
    TID=$("$NEON" tenant create --pg-version 16 2>&1 | awk '/successfully created/{print $2}')
    [[ -n $TID ]] || { echo "tenant create failed"; exit 1; }
    echo "tenant: $TID"
    "$NEON" endpoint create main --tenant-id "$TID" --branch-name main --pg-version 16 >/dev/null
    "$NEON" endpoint start main >/dev/null
fi

step "Run workload"
WORKLOAD=$REPO/test/repro/workload.sql
PGPASSWORD= psql -h 127.0.0.1 -p "$COMPUTE_PORT" -U cloud_admin postgres -f "$WORKLOAD"

step "Verdict — scan pageserver.log for corruption markers"
LOG=$NEON_REPO_DIR/pageserver_1/pageserver.log
MARKERS='MATERIALIZER REDO ERROR|ABORT|PANIC|early eof|specified item offset|image row at lsn|deserialize error|WRITE-PATH ROUNDTRIP'
HITS=$(grep -cE "$MARKERS" "$LOG" || true)
if (( HITS > 0 )); then
    echo "FAIL — $HITS corruption marker(s) in $LOG"
    grep -E "$MARKERS" "$LOG" | head -5
    exit 1
fi

step "Counter check"
COUNTER=$(curl -sS http://127.0.0.1:9989/metrics 2>/dev/null \
    | grep -E '^pageserver_yb_correctness_errors_total\{' || true)
if [[ -n $COUNTER ]]; then
    echo "FAIL — correctness counter non-zero:"
    echo "$COUNTER"
    exit 1
fi

echo
echo "PASS — workload clean, no corruption markers, counter at 0"
