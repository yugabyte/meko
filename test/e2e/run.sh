#!/usr/bin/env bash
#
# End-to-end test runner. Brings up one neon_local cluster against an
# ephemeral YB database, then iterates over every test in each
# requested category. Each test gets its own clean+init+start so it
# starts from a known-good state — the *.sh tests routinely stop PS
# or SK as part of their scenarios and don't reliably reset, so
# sharing the cluster across tests gets messy.
#
#   *.sh   — runs after a fresh tenant + `main` endpoint. Exports
#            $TID, $COMPUTE_PORT, $NEON, $NEON_REPO_DIR, $YB_DBNAME.
#   *.sql  — psql workload against the same shared cluster, set up
#            once after all the .sh tests finish.
#
# After every test the pageserver log is scanned for corruption
# markers; the first hit aborts the run.
#
# Usage:
#   test/e2e/run.sh                # fast suite
#   test/e2e/run.sh fast           # explicit
#   test/e2e/run.sh slow           # slow only
#   test/e2e/run.sh all            # fast then slow
#
set -euo pipefail

REPO=$(git rev-parse --show-toplevel)
cd "$REPO"

CATEGORY=fast
for arg in "$@"; do
    case "$arg" in
        fast|slow|all) CATEGORY=$arg ;;
        *) echo "usage: $0 [fast|slow|all]"; exit 2;;
    esac
done

NEON_REPO_DIR=${NEON_REPO_DIR:-$REPO/.e2e-tests}
export NEON_REPO_DIR
export YB_PS_VERIFY_WAL_ROUNDTRIP=${YB_PS_VERIFY_WAL_ROUNDTRIP:-1}

YB_HOST=${YB_HOST:-127.0.0.1}
YB_PORT=${YB_PORT:-5433}
COMPUTE_PORT=${COMPUTE_PORT:-55432}
export COMPUTE_PORT

# Ephemeral YB database for this run; dropped on exit so concurrent
# runs (and the dev box's manually-managed yb_storage) don't collide.
YB_DBNAME=${YB_DBNAME:-yb_storage_e2e_$$_$RANDOM}
export YB_DBNAME

# Per-run init TOML, derived from /tmp/neon-init.toml with the YB
# dbname swapped to the ephemeral one in every place that references
# it: SC's `database_url` and the inline-table `dbname` on the PS
# `remote_storage` / SK `wal_yb_config` lines.
INIT_TOML=/tmp/neon-init-e2e-$$.toml
sed -E \
    -e "s#/yb_storage\\b#/$YB_DBNAME#g" \
    -e "s#user = 'yugabyte'#user = 'yugabyte', dbname = '$YB_DBNAME'#g" \
    /tmp/neon-init.toml > "$INIT_TOML"

cleanup_on_exit() {
    "$REPO/test/e2e/clean.sh" >/dev/null 2>&1 || true
    rm -f "$INIT_TOML"
}
trap cleanup_on_exit EXIT

NEON=${NEON:-$REPO/target/debug/neon_local}
export NEON
[[ -x $NEON ]] || { echo "neon_local binary not found at $NEON; run 'cargo build --bin neon_local'"; exit 1; }

step() { echo; printf '%(%H:%M:%S)T === %s ===\n' -1 "$*"; }

MARKERS='MATERIALIZER REDO ERROR|ABORT|PANIC|early eof|specified item offset|image row at lsn|deserialize error|WRITE-PATH ROUNDTRIP'

# Bring up a fresh cluster + tenant + endpoint. clean.sh kills any
# leftover procs and drops the ephemeral DB; neon_local init applies
# the diesel migrations against the freshly created DB; neon_local
# start spawns SC, PS, SK, endpoint_storage. Exports $TID for the
# caller.
setup_cluster() {
    "$REPO/test/e2e/clean.sh"

    step "neon_local init + start (db=$YB_DBNAME)"
    "$NEON" init --config "$INIT_TOML"
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
    setup_cluster
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

# Phase 1: every *.sh test, in lexical order, across every requested
# category. Each gets a fresh cluster.
for cat in "${categories[@]}"; do
    run_sh_tests "$cat"
done

# Phase 2: SQL workloads against a fresh cluster.
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
