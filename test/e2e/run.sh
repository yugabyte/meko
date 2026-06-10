#!/usr/bin/env bash
#
# End-to-end test runner. Brings up one yb_light_local cluster against an
# ephemeral YB database, then iterates over every test in each
# requested category.
#
# All tests share a single cluster + tenant + endpoint to minimize
# setup cost; between tests we re-up any cluster components the prior
# test left stopped and recycle the compute endpoint so each test
# starts from a clean compute state (fresh basebackup, fresh
# walproposer). The tenant + timeline persist across tests, so test
# scripts must use unique table names.
#
#   *.sh   — runs after the prep step has ensured the cluster + a
#            running endpoint named `main`. Exports $TID,
#            $COMPUTE_PORT, $NEON, $YB_LIGHT_REPO_DIR, $YB_DBNAME.
#   *.sql  — psql workload against the same shared cluster.
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

TEST_DIR=${TEST_DIR:-$REPO/.e2e-tests}
export TEST_DIR
RUN_ID=$(date +%Y%m%d-%H%M%S)

YB_LIGHT_REPO_DIR=${YB_LIGHT_REPO_DIR:-$TEST_DIR/run_$RUN_ID}
export YB_LIGHT_REPO_DIR
mkdir -p "$YB_LIGHT_REPO_DIR"
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

# Capture stdout+stderr to a per-run log. The log lives outside
# YB_LIGHT_REPO_DIR because clean.sh wipes that dir at the start of every
# run; siblings persist across runs and let you diff successive runs.
RUN_LOG_DIR=${RUN_LOG_DIR:-${TEST_DIR}}
mkdir -p "$RUN_LOG_DIR"
RUN_LOG=$RUN_LOG_DIR/run_$RUN_ID.log
exec > >(tee -a "$RUN_LOG") 2>&1
echo "Logging run to $RUN_LOG"

NEON=${NEON:-$REPO/target/debug/yb_light_local}
export NEON
[[ -x $NEON ]] || { echo "yb_light_local binary not found at $NEON; run 'cargo build --bin yb_light_local'"; exit 1; }

step() { echo; printf '%(%H:%M:%S)T === %s ===\n' -1 "$*"; }

MARKERS='MATERIALIZER REDO ERROR|ABORT|PANIC|early eof|specified item offset|image row at lsn|deserialize error|WRITE-PATH ROUNDTRIP'

# Pidfile locations under YB_LIGHT_REPO_DIR.
PS_PIDFILE=$YB_LIGHT_REPO_DIR/pageserver_1/pageserver.pid
SK_PIDFILE=$YB_LIGHT_REPO_DIR/safekeepers/sk1/safekeeper.pid
SC_PIDFILE=$YB_LIGHT_REPO_DIR/storage_controller_1/storage_controller.pid
ES_PIDFILE=$YB_LIGHT_REPO_DIR/endpoint_storage/endpoint_storage.pid
EP_COMPUTE_PIDFILE=$YB_LIGHT_REPO_DIR/endpoints/main/compute_ctl.pid
EP_PG_PIDFILE=$YB_LIGHT_REPO_DIR/endpoints/main/pgdata/postmaster.pid

LOG=$YB_LIGHT_REPO_DIR/pageserver_1/pageserver.log

is_running() {
    local pidfile=$1
    [[ -f $pidfile ]] || return 1
    local pid
    pid=$(cat "$pidfile" 2>/dev/null) || return 1
    [[ -n $pid ]] && kill -0 "$pid" 2>/dev/null
}

# Bring the cluster up from scratch. Runs once at the start of the
# suite: clean leftover state, init+start the stack, create one
# tenant and one endpoint that every test will share. Exports $TID.
setup_cluster() {
    "$REPO/test/e2e/clean.sh"

    step "yb_light_local init + start (db=$YB_DBNAME)"
    "$NEON" init --config "$INIT_TOML"
    "$NEON" start

    step "tenant + endpoint"
    TID=$("$NEON" tenant create --pg-version 17 2>&1 | awk '/successfully created/{print $2}')
    [[ -n $TID ]] || { echo "tenant create failed"; exit 1; }
    export TID
    echo "tenant: $TID"
    # Default port allocation picks pg=55432, ext_http=55433,
    # int_http=55434 for the first endpoint — matches the
    # $COMPUTE_PORT default. (If $COMPUTE_PORT was overridden, the
    # caller is on their own.)
    "$NEON" endpoint create main --tenant-id "$TID" --branch-name main \
        --pg-version 17 >/dev/null
    "$NEON" endpoint start main >/dev/null
}

# Restart any cluster component the prior test left stopped, then
# recycle the compute endpoint. `endpoint stop` followed by
# `endpoint start` kills the running postgres + compute_ctl, redoes
# basebackup against the (possibly-reattached) pageserver, and
# spawns a fresh walproposer — so the next test starts from a clean
# compute state regardless of what the previous test did.
#
# Pidfiles are checked rather than blindly invoking `start`: the
# start path doesn't no-op when a daemon is already running, it
# tries to bind the port and fails.
prep_for_next_test() {
    if ! is_running "$SC_PIDFILE"; then
        step "restart storage_controller (prior test left it down)"
        "$NEON" storage_controller start >/dev/null
    fi
    if ! is_running "$PS_PIDFILE"; then
        step "restart pageserver (prior test left it down)"
        "$NEON" pageserver start >/dev/null
    fi
    if ! is_running "$SK_PIDFILE"; then
        step "restart safekeeper (prior test left it down)"
        "$NEON" safekeeper start >/dev/null
    fi
    if ! is_running "$ES_PIDFILE"; then
        step "restart endpoint_storage (prior test left it down)"
        "$NEON" endpoint_storage start >/dev/null
    fi

    # Always recycle the compute. `endpoint stop` is tolerant of the
    # postgres / compute_ctl already being gone — pg_ctl returns
    # cleanly when there's no postmaster.pid — so we don't need to
    # gate on status here. We use `immediate` because some tests
    # (restart-wal-replay-yb) leave the compute mid-flight talking
    # to a pageserver that just churned through catch-up; a fast
    # shutdown blocks on the next walproposer round-trip and adds
    # 10+ s. The compute's WAL has already been verified by the
    # test itself, so we don't need the clean checkpoint.
    local t0 t1
    t0=$(date +%s)
    "$NEON" endpoint stop main --mode immediate >/dev/null 2>&1 || true
    "$NEON" endpoint start main >/dev/null
    t1=$(date +%s)
    if (( t1 - t0 > 2 )); then echo "  (endpoint recycle: $((t1 - t0))s)"; fi
}

run_sh_test() {
    local sh=$1
    local rel=${sh#$REPO/}
    step "script: $rel"
    local before_lines after_lines
    before_lines=$([[ -f $LOG ]] && wc -l < "$LOG" || echo 0)
    local t0 t1
    t0=$(date +%s)
    bash "$sh"
    t1=$(date +%s)
    after_lines=$([[ -f $LOG ]] && wc -l < "$LOG" || echo "$before_lines")
    local new_hits=0
    if (( after_lines > before_lines )); then
        new_hits=$(tail -n $((after_lines - before_lines)) "$LOG" | grep -cE "$MARKERS" || true)
    fi
    if (( new_hits > 0 )); then
        echo "FAIL — $new_hits corruption marker(s) introduced by $rel"
        tail -n $((after_lines - before_lines)) "$LOG" | grep -E "$MARKERS" | head -5
        exit 1
    fi
    echo "  ok  ($(( t1 - t0 ))s)"
}

run_sql_workload() {
    local sql=$1
    local rel=${sql#$REPO/}
    step "workload: $rel"
    local before_lines after_lines
    before_lines=$([[ -f $LOG ]] && wc -l < "$LOG" || echo 0)
    local t0 t1
    t0=$(date +%s)
    PGPASSWORD= psql -h 127.0.0.1 -p "$COMPUTE_PORT" -U cloud_admin postgres \
        -v ON_ERROR_STOP=1 -f "$sql"
    t1=$(date +%s)
    after_lines=$([[ -f $LOG ]] && wc -l < "$LOG" || echo "$before_lines")
    local new_hits=0
    if (( after_lines > before_lines )); then
        new_hits=$(tail -n $((after_lines - before_lines)) "$LOG" | grep -cE "$MARKERS" || true)
    fi
    if (( new_hits > 0 )); then
        echo "FAIL — $new_hits corruption marker(s) introduced by $rel"
        tail -n $((after_lines - before_lines)) "$LOG" | grep -E "$MARKERS" | head -5
        exit 1
    fi
    echo "  ok  ($(( t1 - t0 ))s, +$((after_lines - before_lines)) log lines)"
}

# Collect all test files (.sh and .sql, in lexical order) for each
# requested category.
collect_tests() {
    local cat=$1
    local dir=$REPO/test/e2e/$cat
    [[ -d $dir ]] || return 0
    shopt -s nullglob
    local f
    for f in "$dir"/*.sh "$dir"/*.sql; do
        echo "$f"
    done
    shopt -u nullglob
}

categories=()
case "$CATEGORY" in
    fast) categories=(fast) ;;
    slow) categories=(slow) ;;
    all)  categories=(fast slow) ;;
esac

setup_cluster

for cat in "${categories[@]}"; do
    while IFS= read -r f; do
        [[ -z $f ]] && continue
        prep_for_next_test
        case "$f" in
            *.sh)  run_sh_test "$f" ;;
            *.sql) run_sql_workload "$f" ;;
        esac
    done < <(collect_tests "$cat")
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
