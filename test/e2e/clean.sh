#!/usr/bin/env bash
#
# Tear down any e2e cluster state from a previous run so the next
# `yb_light_local init + start` is a clean slate:
#
# - Kill any neon-stack processes still running locally
#   (yb-pageserver, safekeeper, storage_broker, endpoint_storage,
#   storage_controller, yb_light_local, compute_ctl).
# - Kill any leftover compute postgres processes anchored under
#   YB_LIGHT_REPO_DIR (compute_ctl spawns postgres as a child; if a prior
#   test's `endpoint stop` hung on pg_ctl, the postgres process can
#   outlive the test and squat on COMPUTE_PORT, breaking the next
#   `endpoint start`).
# - Terminate sessions on the YugabyteDB `$YB_DBNAME` database
#   (defaults to `yb_storage`) and drop it.
# - Remove the on-disk `YB_LIGHT_REPO_DIR` so init can start fresh.
#   With `KEEP_LOGS=1` (passing-run teardown) the heavy cluster state is
#   pruned but every `*.log` is left in place, so the run's logs persist
#   under .e2e-tests for postmortem.
#
# Idempotent — re-running on an already-clean tree is a no-op.
#
# Run.sh calls this before each suite unless `--noclean` is passed.

set -uo pipefail

REPO=$(git rev-parse --show-toplevel)

YB_LIGHT_REPO_DIR=${YB_LIGHT_REPO_DIR:-$REPO/.e2e-tests}
YB_HOST=${YB_HOST:-127.0.0.1}
YB_PORT=${YB_PORT:-5433}
YB_DBNAME=${YB_DBNAME:-yb_storage}

step() { echo; printf '%(%H:%M:%S)T === %s ===\n' -1 "$*"; }

step "Stop any running pageserver/neon processes"
pkill -9 -f "yb-pageserver|safekeeper|storage_broker|endpoint_storag|storage_controller|yb_light_local|compute_ctl" 2>/dev/null || true
# Compute postgres survives a hung pg_ctl shutdown. It runs as
# `postgres -D <YB_LIGHT_REPO_DIR>/endpoints/<id>/pgdata`, so anchor the
# pattern on the repo dir to avoid hitting the YugabyteDB postgres.
pkill -9 -f "postgres .*${YB_LIGHT_REPO_DIR}/endpoints" 2>/dev/null || true
sleep 2

step "Drop $YB_DBNAME"
PGPASSWORD=yugabyte psql -h "$YB_HOST" -p "$YB_PORT" -U yugabyte -d yugabyte \
    -c "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname='$YB_DBNAME'" >/dev/null 2>&1 || true
PGPASSWORD=yugabyte psql -h "$YB_HOST" -p "$YB_PORT" -U yugabyte -d yugabyte \
    -c "DROP DATABASE IF EXISTS \"$YB_DBNAME\"" >/dev/null

if [[ ${KEEP_LOGS:-0} == 1 && -d $YB_LIGHT_REPO_DIR ]]; then
    # Teardown of a passing run: drop the heavy cluster state but leave
    # every *.log in place, so the run's logs persist under .e2e-tests
    # for postmortem (find -delete is depth-first, so emptied dirs go too).
    step "Prune $YB_LIGHT_REPO_DIR (keeping *.log)"
    find "$YB_LIGHT_REPO_DIR" -mindepth 1 ! -type d ! -name '*.log' -delete 2>/dev/null || true
    find "$YB_LIGHT_REPO_DIR" -type d -empty -delete 2>/dev/null || true
else
    step "Remove $YB_LIGHT_REPO_DIR"
    rm -rf "$YB_LIGHT_REPO_DIR"
fi

echo "clean."
