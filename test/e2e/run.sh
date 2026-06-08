#!/usr/bin/env bash
#
# End-to-end test runner. Each category directory under
# `test/e2e/<cat>/` mixes two kinds of tests:
#
#   *.sh   — run against a tenant+endpoint that this runner sets up
#            fresh before each script (clean.sh + init + start +
#            tenant create + endpoint create + endpoint start). The
#            tenant id is exported as $TID; the script just runs its
#            scenario.
#   *.sql  — psql workloads against a shared cluster set up once per
#            run, after all the .sh tests have finished.
#
# After every workload the pageserver log is scanned for corruption
# markers; the first hit aborts the run.
#
# Usage:
#   test/e2e/run.sh                          # fast suite, fresh cluster
#   test/e2e/run.sh fast                     # explicit
#   test/e2e/run.sh slow                     # slow suite only
#   test/e2e/run.sh all                      # fast then slow
#   test/e2e/run.sh fast --noclean           # skip the shared-cluster
#                                            # setup at the end. .sh
#                                            # tests still get a fresh
#                                            # cluster each.
#
set -euo pipefail

REPO=$(git rev-parse --show-toplevel)
cd "$REPO"

CATEGORY=fast
NOCLEAN=0
for arg in "$@"; do
    case "$arg" in
        fast|slow|all) CATEGORY=$arg ;;
        --noclean)     NOCLEAN=1 ;;
        *) echo "usage: $0 [fast|slow|all] [--noclean]"; exit 2;;
    esac
done

NEON_REPO_DIR=${NEON_REPO_DIR:-$REPO/.neon-localfs}
export NEON_REPO_DIR
export YB_PS_VERIFY_WAL_ROUNDTRIP=${YB_PS_VERIFY_WAL_ROUNDTRIP:-1}

YB_HOST=${YB_HOST:-127.0.0.1}
YB_PORT=${YB_PORT:-5433}
COMPUTE_PORT=${COMPUTE_PORT:-55432}
export COMPUTE_PORT

NEON=${NEON:-$REPO/target/debug/neon_local}
export NEON
[[ -x $NEON ]] || { echo "neon_local binary not found at $NEON; run 'cargo build --bin neon_local'"; exit 1; }

step() { echo; echo "=== $* ==="; }

MARKERS='MATERIALIZER REDO ERROR|ABORT|PANIC|early eof|specified item offset|image row at lsn|deserialize error|WRITE-PATH ROUNDTRIP'

# Bring up a single neon_local cluster with one tenant and one
# "main" endpoint. Exports $TID so the caller (a *.sh test or the
# *.sql phase below) can use it.
setup_cluster() {
    "$REPO/test/e2e/clean.sh"

    step "neon_local init + start"
    "$NEON" init --config /tmp/neon-init.toml
    "$NEON" start

    step "tenant + endpoint"
    TID=$("$NEON" tenant create --pg-version 17 2>&1 | awk '/successfully created/{print $2}')
    [[ -n $TID ]] || { echo "tenant create failed"; exit 1; }
    export TID
    echo "tenant: $TID"
    "$NEON" endpoint create main --tenant-id "$TID" --branch-name main --pg-version 17 >/dev/null
    "$NEON" endpoint start main >/dev/null
}

LOG=$NEON_REPO_DIR/pageserver_1/pageserver.log

# Per-SQL-workload runner: prints the workload's psql output, then
# attributes any new corruption markers to it.
run_workload() {
    local sql=$1
    local rel=${sql#$REPO/}
    step "workload: $rel"
    local before_lines after_lines
    before_lines=$(wc -l < "$LOG")
    local t0 t1
    t0=$(date +%s)
    PGPASSWORD= psql -h 127.0.0.1 -p "$COMPUTE_PORT" -U cloud_admin postgres \
        -v ON_ERROR_STOP=1 -f "$sql"
    t1=$(date +%s)
    after_lines=$(wc -l < "$LOG")
    local new_hits
    new_hits=$(tail -n $((after_lines - before_lines)) "$LOG" | grep -cE "$MARKERS" || true)
    if (( new_hits > 0 )); then
        echo "FAIL — $new_hits corruption marker(s) introduced by $rel"
        tail -n $((after_lines - before_lines)) "$LOG" | grep -E "$MARKERS" | head -5
        exit 1
    fi
    echo "  ok  ($(( t1 - t0 ))s, +$((after_lines - before_lines)) log lines)"
}

# Run every *.sh test in a category directory. Each gets a fresh
# cluster + tenant + endpoint via setup_cluster() so the previous
# test's state can't bleed in.
run_sh_tests() {
    local cat=$1
    local dir=$REPO/test/e2e/$cat
    [[ -d $dir ]] || return 0
    shopt -s nullglob
    local scripts=("$dir"/*.sh)
    shopt -u nullglob
    for sh in "${scripts[@]}"; do
        local rel=${sh#$REPO/}
        step "script: $rel"
        setup_cluster
        local t0 t1
        t0=$(date +%s)
        bash "$sh"
        t1=$(date +%s)
        echo "  ok  ($(( t1 - t0 ))s)"
    done
}

run_sql_workloads() {
    local cat=$1
    local dir=$REPO/test/e2e/$cat
    [[ -d $dir ]] || { echo "skip $cat sql (no directory)"; return 0; }
    shopt -s nullglob
    local sqls=("$dir"/*.sql)
    shopt -u nullglob
    if (( ${#sqls[@]} == 0 )); then
        echo "skip $cat sql (no .sql files)"
        return 0
    fi
    for sql in "${sqls[@]}"; do
        run_workload "$sql"
    done
}

categories=()
case "$CATEGORY" in
    fast) categories=(fast) ;;
    slow) categories=(slow) ;;
    all)  categories=(fast slow) ;;
esac

# Phase 1: *.sh tests, in order, across every requested category.
# Each gets its own fresh cluster.
for cat in "${categories[@]}"; do
    run_sh_tests "$cat"
done

# Phase 2: shared cluster + *.sql workloads. Skipped under
# --noclean for callers reusing an already-running cluster.
if (( NOCLEAN == 0 )); then
    setup_cluster
fi

for cat in "${categories[@]}"; do
    run_sql_workloads "$cat"
done

step "Final verdict"
COUNTER=$(curl -sS http://127.0.0.1:9989/metrics 2>/dev/null \
    | grep -E '^pageserver_yb_correctness_errors_total\{' || true)
if [[ -n $COUNTER ]]; then
    echo "FAIL — correctness counter non-zero:"
    echo "$COUNTER"
    exit 1
fi
echo "PASS — all workloads clean, counter at 0"
