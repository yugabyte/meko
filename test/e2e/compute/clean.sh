#!/usr/bin/env bash
#
# Tear down any e2e cluster state from a previous run so the next
# `yb_light_local init + start` is a clean slate:
#
# - Kill the e2e cluster's neon-stack processes (yb-pageserver,
#   yb-safekeeper, yb-endpoint-storage, storage_controller, yb-compute-ctl). Kills
#   are SCOPED to THIS run — anchored on `$YB_LIGHT_REPO_DIR` (a unique
#   `run_<id>/`, services) and the ephemeral `$YB_DBNAME` (controller) —
#   so a concurrently-running e2e cluster (its own port block + run dir +
#   db) and a developer's `.yb_light` cluster are left untouched.
# - Kill any leftover compute postgres processes anchored under
#   YB_LIGHT_REPO_DIR (compute_ctl spawns postgres as a child; if a prior
#   test's `endpoint stop` hung on pg_ctl, the postgres process can
#   outlive the test and squat on COMPUTE_PORT, breaking the next
#   `endpoint start`).
# - Terminate sessions on the YugabyteDB `$YB_DBNAME` database
#   (defaults to `yb_storage`) and drop it.
# - Remove the on-disk `YB_LIGHT_REPO_DIR` so init can start fresh — or,
#   when `E2E_KEEP_LOGS=1` (failed-run teardown), keep only its `*.log` files.
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

step "Stop e2e cluster processes (scoped — leaves a developer's .yb_light cluster alone)"
# Every kill is anchored to THIS run's repo dir (`$YB_LIGHT_REPO_DIR`, a
# unique `run_<id>/`) and THIS run's ephemeral db, so it never touches a
# concurrently-running e2e cluster (each run gets its own port block, run
# dir, and db) or a developer's `.yb_light` cluster. A stale crashed prior
# run can't interfere either — it lives on a different port block — and its
# dir is reaped by the 7-day GC.
#
# Every process for THIS run carries the unique run dir ($YB_LIGHT_REPO_DIR)
# in its argv, so one path anchor reaps them all (pageserver, safekeeper,
# endpoint_storage, compute_ctl, compute postgres), scoped to this run.
pkill -9 -f "${YB_LIGHT_REPO_DIR}" 2>/dev/null || true

# The storage_controller has no run dir in argv; anchor on this run's
# ephemeral db name (from its --yb-config) instead.
pkill -9 -f "storage_controller.*dbname = \"${YB_DBNAME}\"" 2>/dev/null || true
sleep 2
# pkill -9 is async w.r.t. the kernel releasing the listen sockets; wait
# for COMPUTE_PORT to actually free so `endpoint start` doesn't race a
# half-dead listener (the duplicate-endpoint check reads it as "running").
COMPUTE_PORT=${COMPUTE_PORT:-55432}
for _ in $(seq 1 20); do
    ss -ltn 2>/dev/null | grep -q ":${COMPUTE_PORT}\b" || break
    sleep 0.5
done

step "Drop $YB_DBNAME"
PGPASSWORD=yugabyte psql -h "$YB_HOST" -p "$YB_PORT" -U yugabyte -d yugabyte \
    -c "DROP DATABASE IF EXISTS \"$YB_DBNAME\" WITH (FORCE)" >/dev/null

# On a failed run the harness sets E2E_KEEP_LOGS=1: the cluster is stopped
# and the db dropped above, but this run's *.log files (harness +
# compute/pageserver/safekeeper/storage_controller/endpoint_storage) are
# preserved for postmortem and CI artifact upload — everything else (pgdata,
# sockets, config, ...) is dropped. Otherwise remove the whole run dir so the
# next init starts fresh.
if [ "${E2E_KEEP_LOGS:-}" = 1 ]; then
    step "Keep *.log under $YB_LIGHT_REPO_DIR; remove the rest"
    find "$YB_LIGHT_REPO_DIR" -type f ! -name '*.log' -delete 2>/dev/null || true
    find "$YB_LIGHT_REPO_DIR" -depth -type d -empty -delete 2>/dev/null || true
else
    step "Remove $YB_LIGHT_REPO_DIR"
    rm -rf "$YB_LIGHT_REPO_DIR"
fi

echo "clean."
