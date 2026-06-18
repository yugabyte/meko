#!/usr/bin/env bash
#
# End-to-end test entry point. Thin wrapper over the Rust harness
# (`e2e_tests` crate). The harness brings up one yb_light_local cluster
# against an ephemeral YB database and runs every scenario in each
# requested category, scanning the pageserver log for corruption
# markers after each. See test/e2e/src/main.rs.
#
# All scenarios share a single cluster + tenant + `main` endpoint; the
# tenant + timeline persist across scenarios, so scenarios use unique
# table names. The `.sql` workloads under fast/ and slow/ are diffed
# against their checked-in `.out` goldens (regenerate with
# E2E_REGEN_EXPECTED=1).
#
# This script builds the harness + cluster binaries (incrementally — a
# no-op when nothing changed) on every run, then execs the harness.
set -euo pipefail

usage() {
    cat <<'EOF'
Usage: test/e2e/run.sh [options] [selection]

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
E2E_SKIP=going_away to skip a known-flaky one); the skipped names are
printed, never silently dropped.

Examples:
  test/e2e/run.sh                       # fast suite, debug
  test/e2e/run.sh all                   # fast then slow
  test/e2e/run.sh smoke                 # one golden workload
  test/e2e/run.sh -n 20 restart_wal_replay_yb   # repeat 20x, count
  test/e2e/run.sh --release all         # full suite, release build
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
CLUSTER_BINS=(
    --bin storage_controller
    --bin yb-pageserver
    --bin yb-safekeeper
    --bin yb-endpoint-storage
    --bin yb-compute-ctl
)
TOOL_BINS=(--bin yb-dump-pages --bin yb-dump-wals --bin yb-reset-consistent-lsn)
if [[ $PROFILE == release ]]; then
    cargo build --release --bin e2e_tests --bin yb_light_local \
        "${CLUSTER_BINS[@]}" "${TOOL_BINS[@]}"
    export YB_LIGHT=${YB_LIGHT:-$REPO/target/release/yb_light_local}
    BIN=$REPO/target/release/e2e_tests
else
    cargo build --bin e2e_tests --bin yb_light_local \
        "${CLUSTER_BINS[@]}" "${TOOL_BINS[@]}"
    BIN=$REPO/target/debug/e2e_tests
fi

exec "$BIN" ${ARGS[@]+"${ARGS[@]}"}
