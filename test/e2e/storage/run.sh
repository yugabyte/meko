#!/usr/bin/env bash
#
# End-to-end test entry point. Thin wrapper over the Rust harness
# (`e2e_tests` crate). The harness brings up one yb_light_local cluster
# against an ephemeral YB database and runs every scenario in each
# requested category, scanning the pageserver log for corruption
# markers after each. See test/e2e/storage/main.rs.
#
# All scenarios share a single cluster + tenant + `main` endpoint; the
# tenant + timeline persist across scenarios, so scenarios use unique
# table names. The `.sql` workloads under fast/ and slow/ are diffed
# against their checked-in `.out` goldens (regenerate with
# E2E_REGEN_EXPECTED=1).
#
# This script builds the harness + cluster binaries AND the yb_amp Postgres
# extension (both incrementally — a no-op when nothing changed) on every run,
# then execs the harness.
set -euo pipefail

usage() {
    cat <<'EOF'
Usage: test/e2e/storage/run.sh [options] [selection]

Selection (default: fast):
  fast            the fast suite
  slow            the slow suite
  all             fast then slow
  <name>          one test by name (substring also matches), e.g.
                  branch_ancestor, restart_wal_replay_yb, smoke

Options:
  -n <count>      repeat the selection <count> times and print a
                  cumulative pass/fail count (run-all: failures are
                  tallied, not fatal). Useful for flakiness hunting.
  --release       build + run in release mode (default: debug, which is
                  what the dev/e2e loop normally wants)
  --no-fail-clean on failure, leave the cluster + DB up for postmortem
                  (default: a failed run is torn down like a passing one,
                  keeping only its logs under .e2e-tests/run_<id>/)
  -h, --help      show this help

Honoured env vars (passed through): E2E_REGEN_EXPECTED, TEST_DIR,
YB_LIGHT_REPO_DIR, YB_DBNAME, YB_HOST, YB_PORT, COMPUTE_PORT, SOAK_SECS,
PS_HTTP_TIMEOUT, ENDPOINT_START_TIMEOUT, YB_PS_VERIFY_WAL_ROUNDTRIP,
YB_LIGHT, E2E_PORT_BASE.

Every run is isolated: it synthesizes its own config on a free port
block (auto-picked in 20000..60000), uses an ephemeral db + run dir, and
scopes teardown to itself — so suites can run CONCURRENTLY (and never
touch a developer's local cluster). E2E_PORT_BASE=<n> pins the block.

E2E_SKIP=<substr>[,<substr>...] drops matching tests from the run (e.g.
E2E_SKIP=going_away,restart_wal_replay_yb to skip some); the skipped names
are printed, never silently dropped.

Examples:
  test/e2e/storage/run.sh                       # fast suite, debug
  test/e2e/storage/run.sh all                   # fast then slow
  test/e2e/storage/run.sh smoke                 # one golden workload
  test/e2e/storage/run.sh -n 20 restart_wal_replay_yb   # repeat 20x, count
  test/e2e/storage/run.sh --release all         # full suite, release build
EOF
}

REPO=$(git rev-parse --show-toplevel)
cd "$REPO"

PROFILE=debug
ARGS=()
for arg in "$@"; do
    case "$arg" in
        -h|--help) usage; exit 0 ;;
        --release) PROFILE=release ;;
        --no-fail-clean) export E2E_NO_FAIL_CLEAN=1 ;;
        *) ARGS+=("$arg") ;;  # forwarded to the harness (selection, -n, …)
    esac
done

# Build the harness and every binary it drives. Debug by default (per
# project convention for the dev/e2e loop); --release for release
# verification.
#
# `yb_light_local` launches the cluster by spawning the server binaries
# from target/<profile>/ at runtime — it does not build them — so they
# must be compiled here too. Omitting one silently runs a STALE binary
# from a previous build, which surfaces as baffling runtime mismatches
# (e.g. a fresh launcher sending a request shape an old storage_controller
# can't parse). Keep this list in sync with the binaries `yb_light_local`
# spawns: storage_controller, yb-pageserver, safekeeper, endpoint_storage,
# compute_ctl.
#
# The diagnostic tools are compiled bins too; the `tools` scenario shells
# out to them, so build them in the same step.
#
# `pageserver/testing` compiles in the `fail_point!` macro so scenarios can
# drive failpoints via the pageserver's /v1/failpoints API (e.g.
# ps_pipeline_ordering forces out-of-order batch completion). The
# `pkg/feature` form scopes it to the pageserver crate only; untriggered
# failpoints cost a single atomic load per site.
CLUSTER_BINS=(
    --bin storage_controller
    --bin yb-pageserver
    --bin yb-safekeeper
    --bin yb-endpoint-storage
    --bin yb-compute-ctl
)
TOOL_BINS=(--bin yb-dump-pages --bin yb-dump-wals --bin yb-reset-consistent-lsn)
if [[ $PROFILE == release ]]; then
    cargo build --release --features pageserver/testing --bin e2e_tests --bin yb_light_local \
        "${CLUSTER_BINS[@]}" "${TOOL_BINS[@]}"
    export YB_LIGHT=${YB_LIGHT:-$REPO/target/release/yb_light_local}
    BIN=$REPO/target/release/e2e_tests
else
    cargo build --features pageserver/testing --bin e2e_tests --bin yb_light_local \
        "${CLUSTER_BINS[@]}" "${TOOL_BINS[@]}"
    BIN=$REPO/target/debug/e2e_tests
fi

# Rebuild the yb_amp Postgres extension when its C sources changed. cargo does
# NOT build it — the launcher spawns a compute whose postgres loads
# pg_install/v18/lib/postgresql/yb_amp.so at runtime (the walproposer +
# pagestore smgr live in that .so). A stale .so silently runs an old compute
# against freshly-built cluster binaries; when the change is to the
# walproposer<->safekeeper wire/structs that surfaces as a hang or a segfault
# that looks like a logic bug but is really a build skew.
#
# The extension's PGXS build has no header-dependency tracking (no .deps/), so
# incremental `make` rebuilds a .o only when its own .c is newer, never when a
# shared header like walproposer.h changes — which would leave stale .o files
# with an old struct layout (an ABI mismatch). So when anything changed we
# force a CLEAN rebuild of the extension build dir.
#
# Staleness is measured against a stamp file we `touch` after a successful
# build — NOT the installed .so, whose mtime `ninstall.sh -C` deliberately
# preserves when the rebuilt .so is byte-identical (which would make the guard
# fire on every run). This is a no-op on the common Rust-only edit (the mtime
# check is instant) and only pays the ~1-2 min recompile when C actually
# changed.
EXT_SO=$REPO/pg_install/v18/lib/postgresql/yb_amp.so
EXT_STAMP=$REPO/build/.yb_amp_ext.stamp
if [[ ! -f $EXT_SO ]] || [[ ! -f $EXT_STAMP ]] \
    || [[ -n "$(find "$REPO/pgxn" -type f \( -name '*.c' -o -name '*.h' -o -name 'Makefile' \) -newer "$EXT_STAMP" -print -quit)" ]]; then
    echo "run.sh: yb_amp extension is stale (C sources changed or not yet built) — clean-rebuilding" >&2
    rm -rf "$REPO/build/pgxn-v18"
    make -C "$REPO" neon-pg-ext-v18
    mkdir -p "$REPO/build"
    touch "$EXT_STAMP"
fi

# Run-scoped identifiers shared by the harness and the cleanup trap below.
# The harness honours these env vars (src/cluster.rs::setup); pre-setting
# them lets the trap tear down THIS run's cluster on a signal, where the
# harness's own Drop-based teardown never runs. Honour caller-provided
# values (e.g. a developer pinning a dir/db).
RUN_ID="${EPOCHSECONDS:-$(date +%s)}_$$"
export YB_LIGHT_REPO_DIR="${YB_LIGHT_REPO_DIR:-$REPO/.e2e-tests/run_${RUN_ID}}"
export YB_DBNAME="${YB_DBNAME:-yb_storage_e2e_${RUN_ID}}"

# Liveness lock for THIS run. We hold an exclusive flock on the run dir's
# `.run.lock` for run.sh's whole lifetime; the kernel releases it when run.sh
# exits OR is killed (SIGKILL / OOM included). So "lock held" reliably means
# "this run is alive", which is what reap.sh keys on to tell a dead run's
# orphans from a live concurrent run. Held by run.sh only — closed in the
# harness child below — so an orphaned cluster process can never keep it held.
mkdir -p "$YB_LIGHT_REPO_DIR"
exec {RUN_LOCK_FD}>"$YB_LIGHT_REPO_DIR/.run.lock"
flock -n "$RUN_LOCK_FD" \
    || { echo "run.sh: $YB_LIGHT_REPO_DIR is locked by another run; aborting" >&2; exit 1; }

# Reap orphans left by runs that are no longer alive (compute postgres that
# outlived a hung `endpoint stop`, or anything from a SIGKILL'd/OOM'd run)
# BEFORE we build and bring up — that is exactly when the box is most
# starved. Scoped to dead runs; a live concurrent run holds its lock and is
# skipped. Best-effort: never let a reap hiccup fail the run.
bash "$REPO/test/e2e/storage/reap.sh" "$REPO/.e2e-tests" "$YB_LIGHT_REPO_DIR" || true

# Backstop teardown for ABNORMAL termination only. A normal exit (success
# or failure — including --no-fail-clean's deliberate keep-alive) is handled
# by the harness's own Drop, so we do NOT trap EXIT; only the signals that
# bypass Drop (Ctrl-C, kill, hangup). clean.sh is scoped to this run via the
# env above, so it never touches a concurrent run or a dev .yb_light cluster.
cleanup() {
    trap - INT TERM HUP
    echo >&2
    echo "run.sh: signalled — tearing down this run's e2e cluster (db=$YB_DBNAME)" >&2
    if [ -n "${HARNESS_PID:-}" ]; then kill "$HARNESS_PID" 2>/dev/null || true; fi
    YB_HOST="${YB_HOST:-127.0.0.1}" YB_PORT="${YB_PORT:-5433}" \
        bash "$REPO/test/e2e/storage/clean.sh" >/dev/null 2>&1 || true
    exit 130
}
trap cleanup INT TERM HUP

# Foreground (not exec) so the trap stays installed and fires on a signal.
# Close the run lock FD in the child before exec'ing the harness, so neither
# the harness nor any cluster process it spawns (compute_ctl -> postgres, …)
# inherits it. Otherwise an orphaned postgres would keep the lock held and
# reap.sh could never tell the run had died.
( exec {RUN_LOCK_FD}>&-; exec "$BIN" ${ARGS[@]+"${ARGS[@]}"} ) &
HARNESS_PID=$!
wait "$HARNESS_PID" || EXIT_CODE=$?
exit "${EXIT_CODE:-0}"
