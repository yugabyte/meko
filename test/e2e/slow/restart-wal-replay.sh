#!/usr/bin/env bash
#
# End-to-end check that a restarted pageserver catches up to a
# safekeeper that is well ahead of the PS's persisted
# `disk_consistent_lsn`.
#
# Reproduces the failure mode from findings-ps-wal-replay-restart-bug-
# 2026-06-04.md: the PS stops while the compute keeps writing into the
# safekeeper, so on PS restart the SK is holding a sizeable WAL backlog
# above the PS's resume LSN. With the fix in place, the walreceiver
# streams the backlog forward; without it, the SK terminates the
# interpreted-WAL stream immediately ("interpreted wal reader
# finished") and the PS is wedged for reads.
#
# Sequence:
#   1. Boot a fresh cluster, create a one-row table on a single heap
#      page and CHECKPOINT so the initial state is durable.
#   2. Start a background tight UPDATE loop on that row.
#   3. Stop the pageserver. The compute keeps caching the page in
#      shared_buffers and the UPDATE loop continues writing WAL into
#      the safekeeper.
#   4. After a soak window, snapshot the safekeeper's commit LSN — this
#      is the gap the PS will have to replay forward on restart.
#   5. Stop the safekeeper. This blocks the compute's writes.
#   6. Restart the safekeeper, then the pageserver. Kill the now-stuck
#      background loop.
#   7. Restart the endpoint. The compute reconnects via basebackup,
#      which requires the PS to have caught up. If the bug is present
#      this hangs (or basebackup fails with an LSN timeout); if fixed,
#      the value of n reflects the writes done while the PS was down.
#
# Run via test/e2e/run.sh — it provides $NEON, $NEON_REPO_DIR,
# $COMPUTE_PORT, $TID and a fresh cluster + endpoint.

set -euo pipefail

REPO=$(git rev-parse --show-toplevel)
cd "$REPO"

YB_HOST=${YB_HOST:-127.0.0.1}
YB_PORT=${YB_PORT:-5433}

# Soak window after the PS stop, before we stop the SK. Larger values
# accumulate more WAL gap — make this overrideable for stress runs.
SOAK_SECS=${SOAK_SECS:-10}

step() { echo; printf "%(%H:%M:%S)T === %s ===\n" -1 "$*"; }

cleanup() {
    # Best-effort kill the background UPDATE loop and its psql, in case
    # we exit on an assertion before the orderly teardown step. Idempotent.
    if [[ -n ${BG_PID:-} ]] && kill -0 "$BG_PID" 2>/dev/null; then
        kill "$BG_PID" 2>/dev/null || true
        wait "$BG_PID" 2>/dev/null || true
    fi
}
trap cleanup EXIT

step "Seed a single-page table and checkpoint"
# autovacuum off keeps the row pinned to one page despite HOT churn.
PGPASSWORD= psql -h 127.0.0.1 -p "$COMPUTE_PORT" -U cloud_admin postgres -v ON_ERROR_STOP=1 <<'SQL'
DROP TABLE IF EXISTS single_page;
CREATE TABLE single_page (
    id INT PRIMARY KEY,
    n  BIGINT NOT NULL
) WITH (autovacuum_enabled = false);
INSERT INTO single_page (id, n) VALUES (1, 0);
CHECKPOINT;
SQL

# Snapshot the starting LSN at the safekeeper (commit_lsn). Compared
# against the post-soak value to confirm the loop actually drove WAL.
ts_start_lsn=$(PGPASSWORD= psql -h 127.0.0.1 -p "$COMPUTE_PORT" -U cloud_admin postgres -At \
    -c "SELECT pg_current_wal_lsn()")
echo "starting compute LSN: $ts_start_lsn"

step "Create the bump procedure (each iteration is its own transaction)"
# Using a PROCEDURE with COMMIT inside flushes WAL per iteration —
# essential for the test, since the SK only sees WAL that's been
# committed. A long DO block holds the work in one transaction and
# never flushes to the SK until the very end.
PGPASSWORD= psql -h 127.0.0.1 -p "$COMPUTE_PORT" -U cloud_admin postgres -v ON_ERROR_STOP=1 <<'SQL'
CREATE OR REPLACE PROCEDURE bump_loop(iters INT) AS $$
BEGIN
    FOR i IN 1..iters LOOP
        UPDATE single_page SET n = n + 1 WHERE id = 1;
        COMMIT;
    END LOOP;
END;
$$ LANGUAGE plpgsql;
SQL

step "Spawn background UPDATE loop (CALL bump_loop, COMMIT per iter)"
# ON_ERROR_STOP=0 because we expect the loop to die when the SK is
# stopped — that's the orderly hand-off, not a failure.
( PGPASSWORD= psql -h 127.0.0.1 -p "$COMPUTE_PORT" -U cloud_admin postgres \
    -v ON_ERROR_STOP=0 -q -c "CALL bump_loop(2000000);" >/dev/null 2>&1 ) &
BG_PID=$!

# Let the loop accumulate a small WAL window before we yank the PS.
sleep 2
echo "background loop PID=$BG_PID, ran for 2s pre-stop"

step "Snapshot n after warmup"
n_before_stop=$(PGPASSWORD= psql -h 127.0.0.1 -p "$COMPUTE_PORT" -U cloud_admin postgres -At \
    -c "SELECT n FROM single_page WHERE id = 1")
echo "n at PS-stop time: $n_before_stop"

step "Stop pageserver (immediate — no GoingAway frame, simulates a kill -9 restart)"
# Immediate stop also leaves disk_consistent_lsn further behind, which
# is closer to the production failure mode that originally triggered
# this finding (a serving PS restarted with a large in-memory backlog).
"$NEON" pageserver stop -m immediate >/dev/null

step "Soak for ${SOAK_SECS}s with PS down — UPDATEs build a WAL gap on the SK"
sleep "$SOAK_SECS"

step "Snapshot SK commit LSN — this is the gap the PS must replay forward"
# Best-effort. Compute backends that touch SLRU / catalog pages during
# the PS-down window can fail to read them and produce a connection
# error; that's fine for this assertion — the LSN is informational and
# the WAL-gap-existence check at the end (n advanced > 0) is the
# load-bearing one.
ts_pre_sk_stop_lsn=$(PGPASSWORD= psql -h 127.0.0.1 -p "$COMPUTE_PORT" -U cloud_admin postgres -At \
    -c "SELECT pg_current_wal_lsn()" 2>/dev/null || echo "(unavailable while PS is down)")
echo "compute LSN before SK stop: $ts_pre_sk_stop_lsn"

step "Stop safekeeper -m immediate (this will break the background loop's next commit)"
"$NEON" safekeeper stop -m immediate >/dev/null

step "Reap the background loop (it should be wedged on SK)"
kill "$BG_PID" 2>/dev/null || true
wait "$BG_PID" 2>/dev/null || true
BG_PID=

step "Restart safekeeper, then pageserver"
"$NEON" safekeeper start >/dev/null
"$NEON" pageserver start

# Give the PS a moment to open the walreceiver to the SK. The bug
# manifests within ~5 ms of the first connect; a fix lands in
# whatever time it takes to stream the gap.
sleep 2

step "Restart compute endpoint (forces basebackup from PS)"
# Stopping + starting forces the compute to ask the PS for a fresh
# basebackup at the safekeeper's current commit LSN. If the PS hasn't
# caught up the basebackup request times out with an LSN-timeout
# error against last_record_lsn.
#
# `--mode immediate` because the SK was just restarted from
# `-m immediate` and the walproposer may not have re-synced yet; a
# graceful `pg_ctl -m fast` shutdown tries to flush WAL through that
# connection and hangs. We're about to re-bootstrap via basebackup
# anyway, so a clean shutdown adds nothing.
"$NEON" endpoint stop main --mode immediate >/dev/null
ENDPOINT_START_TIMEOUT=${ENDPOINT_START_TIMEOUT:-60}
if ! timeout "$ENDPOINT_START_TIMEOUT" "$NEON" endpoint start main >/dev/null; then
    echo "FAIL — endpoint failed to start within ${ENDPOINT_START_TIMEOUT}s"
    echo "  most likely the pageserver could not catch up to the safekeeper's WAL gap"
    echo "  (tail of pageserver log:)"
    DEST="${RUN_LOG_DIR:-${NEON_REPO_DIR}-logs}"
    mkdir -p "$DEST"
    cp "$NEON_REPO_DIR/pageserver_1/pageserver.log" "$DEST/restart-wal-replay-ps-$$.log" 2>/dev/null || true
    find "$NEON_REPO_DIR" -name 'safekeeper.log' -exec cp {} "$DEST/restart-wal-replay-sk-$$.log" \; 2>/dev/null || true
    tail -n 500 "$NEON_REPO_DIR/pageserver_1/pageserver.log"
    exit 1
fi

preserve_logs_on_failure() {
    local DEST="${RUN_LOG_DIR:-${NEON_REPO_DIR}-logs}"
    mkdir -p "$DEST"
    cp "$NEON_REPO_DIR/pageserver_1/pageserver.log" "$DEST/restart-wal-replay-ps-$$.log" 2>/dev/null || true
    find "$NEON_REPO_DIR" -name 'safekeeper.log' -exec cp {} "$DEST/restart-wal-replay-sk-$$.log" \; 2>/dev/null || true
}

step "Read back n and verify catch-up"
# Capture stderr separately so the libpq version-info warnings (printed
# unconditionally by psql against this libdir) don't pollute the numeric
# value we hand to `(( ... ))`. Anything on stderr that looks like an
# ERROR line is preserved as a test-failure indicator.
psql_err=$(mktemp)
n_after=$(PGPASSWORD= psql -h 127.0.0.1 -p "$COMPUTE_PORT" -U cloud_admin postgres -At -t \
    -c "SELECT n FROM single_page WHERE id = 1" 2>"$psql_err")
psql_stderr=$(cat "$psql_err")
rm -f "$psql_err"
echo "n after restart: $n_after  (before PS stop: $n_before_stop)"

if [[ -z $n_after ]] || [[ "$psql_stderr" == *ERROR* ]]; then
    echo "FAIL — could not read row back from restarted cluster"
    echo "  psql stderr: $psql_stderr"
    preserve_logs_on_failure
    exit 1
fi
if (( n_after < n_before_stop )); then
    echo "FAIL — n went backwards (${n_after} < ${n_before_stop}): basebackup served a stale snapshot"
    exit 1
fi
if (( n_after == n_before_stop )); then
    # If n didn't advance at all, either the soak loop didn't actually
    # commit anything past the PS-stop point or the PS skipped replay
    # of the gap WAL. Either way, the test isn't exercising the bug.
    echo "FAIL — n did not advance during the SK-only window (still $n_after)"
    echo "  the WAL gap was zero, so the test isn't exercising the catch-up path"
    exit 1
fi
echo "  ok — n advanced by $(( n_after - n_before_stop )) during the SK-only window"

step "Scan pageserver log for catch-up failure markers"
LOG=$NEON_REPO_DIR/pageserver_1/pageserver.log
[[ -r $LOG ]] || { echo "FAIL — pageserver log not readable at $LOG"; exit 1; }

# The signature of the bug is the walreceiver retry loop hammering
# "interpreted wal reader finished" against the SK. A handful of
# retries during normal restart is fine; >5 occurrences strongly
# indicates the PS is wedged.
WEDGE_HITS=$(grep -c "interpreted wal reader finished" "$LOG" || true)
if (( WEDGE_HITS > 5 )); then
    echo "FAIL — pageserver log shows $WEDGE_HITS 'interpreted wal reader finished' retries"
    echo "  (last few:)"
    grep "interpreted wal reader finished" "$LOG" | tail -n 5
    exit 1
fi
echo "  ok — $WEDGE_HITS 'interpreted wal reader finished' occurrence(s) (threshold 5)"

# Corruption markers — same set as restart-correctness.sh
MARKERS='MATERIALIZER REDO ERROR|ABORT|PANIC|early eof|specified item offset|image row at lsn|deserialize error|WRITE-PATH ROUNDTRIP'
HITS=$(grep -cE "$MARKERS" "$LOG" || true)
if (( HITS > 0 )); then
    echo "FAIL — $HITS corruption marker(s) in pageserver.log"
    grep -E "$MARKERS" "$LOG" | head -5
    exit 1
fi
echo "  pageserver log clean of corruption markers"

step "Final verdict"
echo "PASS — pageserver caught up to safekeeper across a ${SOAK_SECS}s WAL gap (n advanced $(( n_after - n_before_stop )))"
