#!/usr/bin/env bash
#
# End-to-end verification of the prev_record_lsn restart fix.
#
# Writes data, restarts the pageserver, then re-reads. With the fix
# in place, the pageserver attaches without re-deriving
# prev_record_lsn from scratch — the value persisted alongside
# disk_consistent_lsn on the previous flush is loaded back from
# `pageserver.timeline_state`. Without the fix the attach hardcoded
# prev = None.
#
# This script directly exercises the failure mode: without the
# patched UPDATE, prev_record_lsn stays NULL forever; without the
# patched attach, the value is read but discarded. Either gap
# leaves the walreceiver to guess prev on restart, which has
# historically led to corruption-class incidents (the original
# motivation for this task).
#
# Run via test/e2e/run.sh — it provides $NEON, $YB_LIGHT_REPO_DIR,
# $COMPUTE_PORT, $TID and a fresh cluster + endpoint.

set -euo pipefail

REPO=$(git rev-parse --show-toplevel)
cd "$REPO"

YB_HOST=${YB_HOST:-127.0.0.1}
YB_PORT=${YB_PORT:-5433}

step() { echo; printf "%(%H:%M:%S)T === %s ===\n" -1 "$*"; }

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
PREV=$(PGPASSWORD=yugabyte psql -h "$YB_HOST" -p "$YB_PORT" -U yugabyte -d "$YB_DBNAME" \
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
LOG=$YB_LIGHT_REPO_DIR/pageserver_1/pageserver.log
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
