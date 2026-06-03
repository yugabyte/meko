#!/usr/bin/env bash
#
# End-to-end verification of the prev_record_lsn restart fix.
#
# Brings up a fresh cluster, writes data, restarts the pageserver,
# then re-reads. With the fix in place, the pageserver attaches
# without re-deriving prev_record_lsn from scratch — the value
# persisted alongside disk_consistent_lsn on the previous flush is
# loaded back from `pageserver.timeline_state`. Without the fix the
# attach hardcoded prev = None.
#
# This script directly exercises the failure mode: without the
# patched UPDATE, prev_record_lsn stays NULL forever; without the
# patched attach, the value is read but discarded. Either gap
# leaves the walreceiver to guess prev on restart, which has
# historically led to corruption-class incidents (the original
# motivation for this task).
#
# Usage: test/e2e/restart-correctness.sh

set -euo pipefail

REPO=$(git rev-parse --show-toplevel)
cd "$REPO"

NEON_REPO_DIR=${NEON_REPO_DIR:-$REPO/.neon-localfs}
export NEON_REPO_DIR

YB_HOST=${YB_HOST:-127.0.0.1}
YB_PORT=${YB_PORT:-5433}
COMPUTE_PORT=${COMPUTE_PORT:-55432}

NEON=${NEON:-$REPO/target/debug/neon_local}
[[ -x $NEON ]] || { echo "neon_local binary not found at $NEON"; exit 1; }

step() { echo; echo "=== $* ==="; }

# Clean slate.
"$REPO/test/e2e/clean.sh"

step "neon_local init + start"
"$NEON" init --config /tmp/neon-init.toml
"$NEON" start

step "tenant + endpoint"
TID=$("$NEON" tenant create --pg-version 16 2>&1 | awk '/successfully created/{print $2}')
[[ -n $TID ]] || { echo "tenant create failed"; exit 1; }
echo "tenant: $TID"
"$NEON" endpoint create main --tenant-id "$TID" --branch-name main --pg-version 16 >/dev/null
"$NEON" endpoint start main >/dev/null

step "Write a small workload (table + inserts + updates)"
PGPASSWORD= psql -h 127.0.0.1 -p "$COMPUTE_PORT" -U cloud_admin postgres -v ON_ERROR_STOP=1 <<'SQL'
DROP TABLE IF EXISTS restart_check;
CREATE TABLE restart_check (id INT PRIMARY KEY, val TEXT NOT NULL);
INSERT INTO restart_check (id, val)
SELECT g, 'row-' || g FROM generate_series(1, 200) g;
UPDATE restart_check SET val = 'updated-' || id WHERE id <= 50;
CHECKPOINT;
SQL

step "Stop endpoint, then pageserver (graceful — should force_upsert pending dc_lsn)"
"$NEON" endpoint stop main >/dev/null
"$NEON" pageserver stop >/dev/null

step "Confirm prev_record_lsn was persisted (non-NULL after PS shutdown)"
PREV=$(PGPASSWORD=yugabyte psql -h "$YB_HOST" -p "$YB_PORT" -U yugabyte -d yb_storage \
    -At -c "SELECT prev_record_lsn FROM pageserver.timeline_state WHERE tenant_id = '$TID'")
if [[ -z $PREV ]]; then
    echo "FAIL — prev_record_lsn was not persisted (column is NULL after graceful shutdown)"
    exit 1
fi
echo "  prev_record_lsn persisted as: $PREV"

step "Restart pageserver"
"$NEON" pageserver start

step "Restart endpoint and verify data is intact"
"$NEON" endpoint start main >/dev/null
TOTAL=$(PGPASSWORD= psql -h 127.0.0.1 -p "$COMPUTE_PORT" -U cloud_admin postgres -At -c \
    "SELECT count(*) FROM restart_check")
UPDATED=$(PGPASSWORD= psql -h 127.0.0.1 -p "$COMPUTE_PORT" -U cloud_admin postgres -At -c \
    "SELECT count(*) FROM restart_check WHERE val LIKE 'updated-%'")
if [[ $TOTAL != "200" || $UPDATED != "50" ]]; then
    echo "FAIL — post-restart counts wrong: total=$TOTAL updated=$UPDATED (expected 200 / 50)"
    exit 1
fi
echo "  ok  (total=$TOTAL, updated=$UPDATED)"

step "Scan pageserver log for corruption markers introduced by the restart"
LOG=$NEON_REPO_DIR/pageserver_1/pageserver.log
MARKERS='MATERIALIZER REDO ERROR|ABORT|PANIC|early eof|specified item offset|image row at lsn|deserialize error|WRITE-PATH ROUNDTRIP'
HITS=$(grep -cE "$MARKERS" "$LOG" || true)
if (( HITS > 0 )); then
    echo "FAIL — $HITS corruption marker(s) in pageserver.log"
    grep -E "$MARKERS" "$LOG" | head -5
    exit 1
fi
echo "  pageserver log clean"

step "Final verdict"
echo "PASS — restart correctness verified"
