#!/usr/bin/env bash
#
# Reap integration processes left behind by runs that are no longer alive.
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
#   - The one exception is the `controller`: it keeps no local data
#     dir (all state is in YB), so its argv carries the run's ephemeral DB
#     name but NO run-dir path, and the dir match above misses it. It (and
#     any process from a dir-GC'd run) is reaped in the DB loop below by
#     matching the run-unique `yb_storage_integration_<id>` name — under the exact
#     same dead-run gate as the DROP, so a live run is still never touched.
#   - run.sh takes the lock BEFORE it starts the harness, so a run only ever
#     has processes once it holds the lock. "Unlocked dir WITH live
#     processes" is therefore always a dead run, never one still starting —
#     so there is no race that kills a starting run.
#
# Usage: reap.sh [test_dir] [current_run_dir_to_skip]
#   test_dir              parent holding run_<id>/ dirs (default .integration-tests)
#   current_run_dir       a run dir to leave alone (this run's own dir)
#
# Idempotent; exits 0 even if it finds nothing.

set -uo pipefail

REPO=$(git rev-parse --show-toplevel 2>/dev/null || pwd)
TEST_DIR="${1:-$REPO/.integration-tests}"
SKIP_DIR="${2:-${YB_LIGHT_REPO_DIR:-}}"

[ -d "$TEST_DIR" ] || exit 0

# A dead run's lock is free; flock -n acquires it (and immediately releases
# it on return). A live run's lock is held; flock -n fails. The lock is the
# SIBLING file `<run_dir>.lock` — it must live outside the run dir because
# the harness's setup clean removes and recreates the dir (an in-dir lock
# vanishing made every live run look dead). Fall back to the legacy in-dir
# `.run.lock` for runs started by an older run.sh; a run with neither is
# legacy/dead.
run_is_dead() {
    local lock="$1.lock"
    [ -e "$lock" ] || lock="$1/.run.lock"
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

# Housekeeping: drop sibling lock files whose run dir is gone (e.g. the
# harness's 7-day dir GC removed it) and whose lock is free. A held lock
# with a missing dir is a LIVE run inside its setup clean-and-recreate
# window — leave it alone.
for lock in "$TEST_DIR"/run_*.lock; do
    dir="${lock%.lock}"
    [ -d "$dir" ] && continue
    flock -n "$lock" -c true 2>/dev/null && rm -f "$lock"
done

# Drop ephemeral YB databases left by dead runs. Each run's DB is
# `yb_storage_integration_<epoch>_<pid>`, where `<epoch>` is its creation time (see
# run.sh `RUN_ID`).
#
# IMPORTANT: the YB cluster (and thus the `pg_database` namespace) is SHARED
# across worktrees, but each worktree's run dirs / locks live under its own
# `.integration-tests`. So the per-run lock is only authoritative for DBs created by
# THIS worktree. We therefore reap on two rules:
#   - Own runs (a `run_<id>` dir exists here): reap as soon as the run is
#     dead (lock free) — fast local GC of our own leaks.
#   - Foreign DBs (no run dir here — another worktree's run, or one whose
#     dir was already GC'd): reap only once they are older than the
#     cross-worktree grace period, by CREATION DATE parsed from the name.
#     A concurrent worktree's live run always has a recent DB, so this never
#     drops a DB out from under it (the bug that caused "terminating
#     connection due to administrator command" mid-run).
# Best-effort: silently skip if psql or YB is unreachable.
CROSS_REAP_GRACE_SECS=$(( 2 * 24 * 60 * 60 ))   # 2 days
YB_HOST="${YB_HOST:-127.0.0.1}"
YB_PORT="${YB_PORT:-5433}"
SKIP_DB="${YB_DBNAME:-}"
yb() { PGPASSWORD=yugabyte psql -h "$YB_HOST" -p "$YB_PORT" -U yugabyte -d yugabyte -tAc "$1" 2>/dev/null; }
if command -v psql >/dev/null 2>&1; then
    now=$(date +%s)
    dropped=0
    for db in $(yb "SELECT datname FROM pg_database WHERE datname LIKE 'yb_storage_integration_%'"); do
        [ "$db" = "$SKIP_DB" ] && continue
        id="${db#yb_storage_integration_}"          # <epoch>_<pid>
        dir="$TEST_DIR/run_${id}"
        if [ -d "$dir" ]; then
            # Our own run: reap as soon as it is dead.
            run_is_dead "$dir" || continue  # live run -> keep its DB
        else
            # Foreign / dir-GC'd run: reap only by create date, past the
            # cross-worktree grace period.
            created="${id%%_*}"
            case "$created" in
                ''|*[!0-9]*) continue ;;    # unparseable epoch -> leave it
            esac
            [ $(( now - created )) -ge "$CROSS_REAP_GRACE_SECS" ] || continue
        fi
        # Kill any orphan process still bound to this dead run's DB before we
        # drop it. The dir-based pass above misses the controller
        # (no run-dir in its argv) and every process of a dir-GC'd run; the
        # run-unique DB name catches them. Gated by the same dead-run rules as
        # the DROP, and the DB name embeds <epoch>_<pid>, so it can't
        # false-match an unrelated process or a live run's controller.
        if pgrep -f -- "$db" >/dev/null 2>&1; then
            echo "reap.sh: killing orphan process(es) bound to dead-run DB $db" >&2
            pkill -9 -f -- "$db" 2>/dev/null || true
        fi
        echo "reap.sh: dropping ephemeral DB of dead run: $db" >&2
        yb "DROP DATABASE IF EXISTS \"$db\" WITH (FORCE)" >/dev/null
        dropped=$((dropped + 1))
    done
    [ "$dropped" -gt 0 ] && echo "reap.sh: dropped $dropped dead-run DB(s)" >&2
fi
exit 0
