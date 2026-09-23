#!/usr/bin/env bash
#
# Tear down any integration cluster state from a previous run so the next
# `yb_amp_local init + start` is a clean slate:
#
# - Kill the integration cluster's neon-stack processes (pageserver,
#   safekeeper, controller, compute-ctl). Kills
#   are SCOPED to THIS run — anchored on `$YB_AMP_REPO_DIR` (a unique
#   `run_<id>/`, services) and the ephemeral `$YB_DBNAME` (controller) —
#   so a concurrently-running integration cluster (its own port block + run dir +
#   db) and a developer's `.yb_amp` cluster are left untouched.
# - Kill any leftover compute postgres processes anchored under
#   YB_AMP_REPO_DIR (compute_ctl spawns postgres as a child; if a prior
#   test's `endpoint stop` hung on pg_ctl, the postgres process can
#   outlive the test and squat on COMPUTE_PORT, breaking the next
#   `endpoint start`).
# - Terminate sessions on the YugabyteDB `$YB_DBNAME` database
#   (defaults to `yb_storage`) and drop it.
# - Remove the on-disk `YB_AMP_REPO_DIR` so init can start fresh — or,
#   when `INTEGRATION_KEEP_LOGS=1` (failed-run teardown), keep only its `*.log` files.
#
# Idempotent — re-running on an already-clean tree is a no-op.
#
# Run.sh calls this before each suite unless `--noclean` is passed.

set -uo pipefail

# The amp tree root. Derived from this script's own location rather than
# `git rev-parse --show-toplevel`, which returns the repository root — not the
# same directory once amp/ is a subdirectory of a larger repo.
REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"

YB_AMP_REPO_DIR=${YB_AMP_REPO_DIR:-$REPO/.integration-tests}
YB_HOST=${YB_HOST:-127.0.0.1}
YB_PORT=${YB_PORT:-5433}
YB_DBNAME=${YB_DBNAME:-yb_storage}

step() { echo; printf '%(%H:%M:%S)T === %s ===\n' -1 "$*"; }

step "Stop integration cluster processes (scoped — leaves a developer's .yb_amp cluster alone)"
# Every kill is anchored to THIS run's repo dir (`$YB_AMP_REPO_DIR`, a
# unique `run_<id>/`) and THIS run's ephemeral db, so it never touches a
# concurrently-running integration cluster (each run gets its own port block, run
# dir, and db) or a developer's `.yb_amp` cluster. A stale crashed prior
# run can't interfere either — it lives on a different port block — and its
# dir is reaped by the 7-day GC.
#
# Every process for THIS run carries the unique run dir ($YB_AMP_REPO_DIR)
# in its argv, so one path anchor reaps them all (pageserver, safekeeper,
# compute_ctl, compute postgres), scoped to this run.
pkill -9 -f "${YB_AMP_REPO_DIR}" 2>/dev/null || true

# The controller has no run dir in argv; anchor on this run's ephemeral db name
# (from its --database-url) instead.
pkill -9 -f "/controller .*${YB_DBNAME}" 2>/dev/null || true
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

# On a failed run the harness sets INTEGRATION_KEEP_LOGS=1: the cluster is stopped
# and the db dropped above, but this run's *.log files (harness +
# compute/pageserver/safekeeper/controller) and any *.walredo dumps are
# preserved for postmortem and CI artifact upload — everything else (pgdata,
# sockets, config, ...) is dropped. Otherwise remove the whole run dir so the
# next init starts fresh.
if [ "${INTEGRATION_KEEP_LOGS:-}" = 1 ]; then
    step "Keep *.log and *.walredo under $YB_AMP_REPO_DIR; remove the rest"
    # `.walredo` dumps are `record_and_log`'s copy of an input walredo could not
    # apply — the one artifact that says *what* it choked on. Poisoned draws no
    # longer write them, so whatever survives a failed run is a genuine failure
    # worth keeping alongside the logs.
    find "$YB_AMP_REPO_DIR" -type f ! -name '*.log' ! -name '*.walredo' -delete 2>/dev/null || true
    find "$YB_AMP_REPO_DIR" -depth -type d -empty -delete 2>/dev/null || true
else
    step "Remove $YB_AMP_REPO_DIR"
    rm -rf "$YB_AMP_REPO_DIR"
fi

echo "clean."
