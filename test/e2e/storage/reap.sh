#!/usr/bin/env bash
#
# Reap e2e processes left behind by runs that are no longer alive.
#
# Each `run.sh` holds an exclusive `flock` on its run dir's `.run.lock` for
# the whole run (see run.sh). The kernel releases that lock when run.sh
# exits OR is killed — including SIGKILL and the OOM killer, which bypass
# both the harness's Drop teardown and run.sh's signal trap. So a run dir
# whose `.run.lock` is NOT currently locked belongs to a DEAD run, and any
# process still anchored to that dir is an orphan that is safe to kill.
#
# This is the missing piece behind the recurring "compute startup timed out
# / pageserver 500 / connection refused" flakiness under back-to-back runs:
# a compute `postgres` that outlives a hung `endpoint stop` (or the whole
# stack from a SIGKILL'd / OOM-killed run) is reparented to init and keeps
# squatting on memory, CPU, and ports forever. clean.sh only reaps the
# CURRENT run; the in-harness GC only deletes dirs older than 7 days and
# never kills processes. Nothing reaped these. Now this does.
#
# Safety / scoping:
#   - A LIVE concurrent run holds its lock, so its dir is skipped.
#   - Processes are killed only by matching the dead run's dir in their
#     argv (`pkill -f <dir>`), so the YugabyteDB cluster and a developer's
#     `.yb_light` cluster — which carry no run dir in argv — are never hit.
#   - run.sh takes the lock BEFORE it starts the harness, so a run only ever
#     has processes once it holds the lock. "Unlocked dir WITH live
#     processes" is therefore always a dead run, never one still starting —
#     so there is no race that kills a starting run.
#
# Usage: reap.sh [test_dir] [current_run_dir_to_skip]
#   test_dir              parent holding run_<id>/ dirs (default .e2e-tests)
#   current_run_dir       a run dir to leave alone (this run's own dir)
#
# Idempotent; exits 0 even if it finds nothing.

set -uo pipefail

REPO=$(git rev-parse --show-toplevel 2>/dev/null || pwd)
TEST_DIR="${1:-$REPO/.e2e-tests}"
SKIP_DIR="${2:-${YB_LIGHT_REPO_DIR:-}}"

[ -d "$TEST_DIR" ] || exit 0

# A dead run's lock is free; flock -n acquires it (and immediately releases
# it on return). A live run's lock is held; flock -n fails. A run dir with
# no lock file at all is a legacy/pre-lock run — also treated as dead.
run_is_dead() {
    local lock="$1/.run.lock"
    [ -e "$lock" ] || return 0           # no lock -> legacy/dead
    flock -n "$lock" -c true 2>/dev/null  # 0 = acquired -> dead; 1 = held -> alive
}

shopt -s nullglob
reaped=0
for dir in "$TEST_DIR"/run_*; do
    [ -d "$dir" ] || continue
    [ "$dir" = "$SKIP_DIR" ] && continue

    run_is_dead "$dir" || continue        # live run -> leave it alone

    # Dead run. Kill anything still anchored to its dir (compute postgres
    # that outlived `endpoint stop`, or a whole SIGKILL'd stack). Scoped to
    # this dir's argv, so nothing outside this run is touched.
    if pgrep -f -- "$dir" >/dev/null 2>&1; then
        echo "reap.sh: reaping orphan processes from dead run $(basename "$dir")" >&2
        pkill -9 -f -- "$dir" 2>/dev/null || true
        reaped=$((reaped + 1))
    fi
done

[ "$reaped" -gt 0 ] && echo "reap.sh: reaped $reaped dead run(s)" >&2

# Drop ephemeral YB databases left by dead runs. Same leak, same liveness
# rule: each run's DB is `yb_storage_e2e_<run_id>`, and the harness only
# creates it AFTER run.sh has taken the run's lock — so a DB whose run is
# dead (lock free / dir gone) is always safe to drop, never one still
# starting. A DROP that the run's own teardown never reached (SIGKILL / hung
# clean) bloats YB's catalog and starves later runs. Best-effort: silently
# skip if psql or YB is unreachable.
YB_HOST="${YB_HOST:-127.0.0.1}"
YB_PORT="${YB_PORT:-5433}"
SKIP_DB="${YB_DBNAME:-}"
yb() { PGPASSWORD=yugabyte psql -h "$YB_HOST" -p "$YB_PORT" -U yugabyte -d yugabyte -tAc "$1" 2>/dev/null; }
if command -v psql >/dev/null 2>&1; then
    dropped=0
    for db in $(yb "select datname from pg_database where datname like 'yb_storage_e2e_%'"); do
        [ "$db" = "$SKIP_DB" ] && continue
        run_is_dead "$TEST_DIR/run_${db#yb_storage_e2e_}" || continue  # live run -> keep its DB
        echo "reap.sh: dropping ephemeral DB of dead run: $db" >&2
        yb "select pg_terminate_backend(pid) from pg_stat_activity where datname='$db'" >/dev/null
        yb "drop database if exists \"$db\"" >/dev/null
        dropped=$((dropped + 1))
    done
    [ "$dropped" -gt 0 ] && echo "reap.sh: dropped $dropped dead-run DB(s)" >&2
fi
exit 0
