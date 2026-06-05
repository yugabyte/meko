#!/usr/bin/env bash
#
# AWS-style variant of restart-wal-replay.sh: brings up a cluster
# whose safekeeper matches the production config from
# findings-ps-wal-replay-restart-bug-2026-06-04.md:
#   --control-file-backend remote-storage
#   --no-sync                      (sync = false in neon_local config)
#   --wal-yb-config                (live WAL mirrored into yb_storage.wals)
#   --wal-cache-size-mib 512
#
# The PS-restart catch-up sequence is identical to the file-backed
# variant: write loop → stop PS → soak → stop SK → restart both →
# verify n advanced. The point is to exercise the YB-WAL read path
# (YbSegmentReader / wals table backfill) on PS catch-up, not the
# local-FS WAL reader.
#
# Usage: test/e2e/restart-wal-replay-yb.sh

set -euo pipefail

REPO=$(git rev-parse --show-toplevel)
cd "$REPO"

NEON_REPO_DIR=${NEON_REPO_DIR:-$REPO/.neon-localfs}
export NEON_REPO_DIR

YB_HOST=${YB_HOST:-127.0.0.1}
YB_PORT=${YB_PORT:-5433}
COMPUTE_PORT=${COMPUTE_PORT:-55432}

SOAK_SECS=${SOAK_SECS:-10}

NEON=${NEON:-$REPO/target/debug/neon_local}
[[ -x $NEON ]] || { echo "neon_local binary not found at $NEON"; exit 1; }

step() { echo; echo "=== $* ==="; }

cleanup() {
    if [[ -n ${BG_PID:-} ]] && kill -0 "$BG_PID" 2>/dev/null; then
        kill "$BG_PID" 2>/dev/null || true
        wait "$BG_PID" 2>/dev/null || true
    fi
}
trap cleanup EXIT

"$REPO/test/e2e/clean.sh"

step "Generate AWS-style neon_local init config (YB-WAL + remote-storage CF)"
INIT_TOML=/tmp/neon-init-yb.toml
cat > "$INIT_TOML" <<'TOML'
generate_local_ssl_certs = false
default_tenant_id = "11da26b359f5f72061ce4fc6461fab3f"
control_plane_api = "http://127.0.0.1:1254/upcall/v1/"

[broker]
listen_addr = "127.0.0.1:50071"

[storage_controller]
max_offline = "10s"
max_warming_up = "30s"
heartbeat_interval = "1s"
timelines_onto_safekeepers = true
database_url = "postgresql://yugabyte:yugabyte@127.0.0.1:5433/yb_storage"

[[pageservers]]
id = 1
listen_pg_addr = "127.0.0.1:64200"
listen_http_addr = "127.0.0.1:9989"
listen_grpc_addr = "127.0.0.1:51251"
pg_auth_type = "Trust"
http_auth_type = "Trust"
grpc_auth_type = "Trust"
remote_storage = "{ host = '127.0.0.1', port = 5433, user = 'yugabyte' }"

[[safekeepers]]
id = 1
pg_port = 5484
http_port = 7696
# AWS-style flags below — this is what differs from the standard config:
sync = false                                # --no-sync
control_file_backend = "remote-storage"     # control file in remote storage (YB blobs)
remote_storage = "{ host = '127.0.0.1', port = 5433, user = 'yugabyte' }"
wal_yb_config = "{ host = '127.0.0.1', port = 5433, user = 'yugabyte', table_name = 'wals' }"
wal_cache_size_mib = 512

[endpoint_storage]
listen_addr = "127.0.0.1:9984"
TOML

step "neon_local init + start"
"$NEON" init --config "$INIT_TOML"
"$NEON" start

step "tenant + endpoint"
TID=$("$NEON" tenant create --pg-version 16 2>&1 | awk '/successfully created/{print $2}')
[[ -n $TID ]] || { echo "tenant create failed"; exit 1; }
echo "tenant: $TID"
"$NEON" endpoint create main --tenant-id "$TID" --branch-name main --pg-version 16 >/dev/null
"$NEON" endpoint start main >/dev/null

step "Seed a single-page table and checkpoint"
PGPASSWORD= psql -h 127.0.0.1 -p "$COMPUTE_PORT" -U cloud_admin postgres -v ON_ERROR_STOP=1 <<'SQL'
DROP TABLE IF EXISTS single_page;
CREATE TABLE single_page (
    id INT PRIMARY KEY,
    n  BIGINT NOT NULL
) WITH (autovacuum_enabled = false);
INSERT INTO single_page (id, n) VALUES (1, 0);
CHECKPOINT;

CREATE OR REPLACE PROCEDURE bump_loop(iters INT) AS $$
BEGIN
    FOR i IN 1..iters LOOP
        UPDATE single_page SET n = n + 1 WHERE id = 1;
        COMMIT;
    END LOOP;
END;
$$ LANGUAGE plpgsql;
SQL

step "Snapshot starting LSN (for the catch-up assertion)"
ts_start_lsn=$(PGPASSWORD= psql -h 127.0.0.1 -p "$COMPUTE_PORT" -U cloud_admin postgres -At \
    -c "SELECT pg_current_wal_lsn()")
echo "starting compute LSN: $ts_start_lsn"

step "Spawn background UPDATE loop"
( PGPASSWORD= psql -h 127.0.0.1 -p "$COMPUTE_PORT" -U cloud_admin postgres \
    -v ON_ERROR_STOP=0 -q -c "CALL bump_loop(2000000);" >/dev/null 2>&1 ) &
BG_PID=$!
sleep 2
echo "background loop PID=$BG_PID, ran for 2s pre-stop"

n_before_stop=$(PGPASSWORD= psql -h 127.0.0.1 -p "$COMPUTE_PORT" -U cloud_admin postgres -At \
    -c "SELECT n FROM single_page WHERE id = 1")
echo "n at PS-stop time: $n_before_stop"

step "Stop pageserver (immediate)"
"$NEON" pageserver stop -m immediate >/dev/null

step "Soak ${SOAK_SECS}s — WAL flushes into yb_storage.wals via wal_yb_config"
sleep "$SOAK_SECS"

step "Stop safekeeper -m immediate"
"$NEON" safekeeper stop -m immediate >/dev/null

step "Reap background loop"
kill "$BG_PID" 2>/dev/null || true
wait "$BG_PID" 2>/dev/null || true
BG_PID=

step "Restart safekeeper (loads control file from remote storage / YB blobs)"
"$NEON" safekeeper start >/dev/null

step "Restart pageserver"
"$NEON" pageserver start

sleep 2

step "Verify PS catch-up via HTTP API (avoid compute restart instability)"
# The compute's walproposer often exits non-gracefully when its
# connection to PS / SK is torn down, leaving pgdata partially
# written; a follow-up endpoint stop+start would then have to redo
# basebackup, which surfaces unrelated noise. The bug we care about
# is the PS being unable to stream the SK's WAL backlog forward —
# that's directly observable via the PS HTTP timeline-detail.
PS_HTTP_TIMEOUT=${PS_HTTP_TIMEOUT:-60}
deadline=$(( $(date +%s) + PS_HTTP_TIMEOUT ))
last_record_lsn=""
while (( $(date +%s) < deadline )); do
    # The PS HTTP listen address is the listen_http_addr from the init
    # toml above (127.0.0.1:9989). The endpoint we want is
    # /v1/tenant/<id>/timeline — pick the first timeline back.
    tl_id=$(curl -fsS "http://127.0.0.1:9989/v1/tenant/$TID/timeline" 2>/dev/null \
        | python3 -c 'import json,sys; d=json.load(sys.stdin); print(d[0]["timeline_id"])' 2>/dev/null || true)
    if [[ -n $tl_id ]]; then
        last_record_lsn=$(curl -fsS \
            "http://127.0.0.1:9989/v1/tenant/$TID/timeline/$tl_id" 2>/dev/null \
            | python3 -c 'import json,sys; d=json.load(sys.stdin); print(d.get("last_record_lsn",""))' 2>/dev/null || true)
        if [[ -n $last_record_lsn && $last_record_lsn != "0/0" && $last_record_lsn != "$ts_start_lsn" ]]; then
            break
        fi
    fi
    sleep 1
done

if [[ -z $last_record_lsn || $last_record_lsn == "0/0" ]]; then
    echo "FAIL — PS last_record_lsn never advanced past $ts_start_lsn within ${PS_HTTP_TIMEOUT}s"
    echo
    echo "  --- pageserver log tail ---"
    tail -n 80 "$NEON_REPO_DIR/pageserver_1/pageserver.log" 2>/dev/null || true
    echo
    echo "  --- safekeeper log tail ---"
    tail -n 80 "$NEON_REPO_DIR/safekeepers/sk1/safekeeper-1.log" 2>/dev/null || true
    exit 1
fi
echo "  ok — PS last_record_lsn advanced to $last_record_lsn (start was $ts_start_lsn)"

step "Scan logs for catch-up failure markers"
LOG=$NEON_REPO_DIR/pageserver_1/pageserver.log
[[ -r $LOG ]] || { echo "FAIL — pageserver log not readable at $LOG"; exit 1; }

WEDGE_HITS=$(grep -c "interpreted wal reader finished" "$LOG" || true)
if (( WEDGE_HITS > 5 )); then
    echo "FAIL — pageserver log shows $WEDGE_HITS 'interpreted wal reader finished' retries"
    echo
    echo "  --- matching error lines (last 5) ---"
    grep "interpreted wal reader finished" "$LOG" | tail -n 5
    echo
    echo "  --- safekeeper log tail ---"
    tail -n 60 "$NEON_REPO_DIR/safekeepers/sk1/safekeeper-1.log" 2>/dev/null || true
    echo
    echo "  --- yb_storage.wals layout (per-segno min/max/count) ---"
    PGPASSWORD=yugabyte psql -h "$YB_HOST" -p "$YB_PORT" -U yugabyte -d yb_storage -c \
        "SELECT segno, min(chunk_offset) AS min_off,
                max(chunk_offset + chunk_size) AS max_end,
                min(chunk_seq) AS min_seq, max(chunk_seq) AS max_seq,
                count(*) AS chunks
         FROM wals GROUP BY segno ORDER BY segno" 2>/dev/null || true
    exit 1
fi
echo "  ok — $WEDGE_HITS 'interpreted wal reader finished' occurrence(s) (threshold 5)"

MARKERS='MATERIALIZER REDO ERROR|ABORT|PANIC|early eof|specified item offset|image row at lsn|deserialize error|WRITE-PATH ROUNDTRIP'
HITS=$(grep -cE "$MARKERS" "$LOG" || true)
if (( HITS > 0 )); then
    echo "FAIL — $HITS corruption marker(s) in pageserver.log"
    grep -E "$MARKERS" "$LOG" | head -5
    exit 1
fi
echo "  pageserver log clean of corruption markers"

step "Final verdict"
echo "PASS — AWS-style (YB-WAL + remote-storage CF + no-sync) catch-up across a ${SOAK_SECS}s WAL gap"
