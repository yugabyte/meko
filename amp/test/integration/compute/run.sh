#!/usr/bin/env bash
#
# Integration test entry point. Thin wrapper over the Rust harness
# (`integration_tests` crate). The harness brings up one yb_amp_local cluster
# against an ephemeral YB database and runs every scenario in each
# requested category, scanning the pageserver log for corruption
# markers after each. See test/integration/compute/main.rs.
#
# All scenarios share a single cluster + tenant + `main` endpoint; the
# tenant + timeline persist across scenarios, so scenarios use unique
# table names. The `.sql` workloads under fast/ and slow/ are diffed
# against their checked-in `.out` goldens (regenerate with
# INTEGRATION_REGEN_EXPECTED=1).
#
# This script builds the harness + cluster binaries AND the yb_amp Postgres
# extension (both incrementally — a no-op when nothing changed) on every run,
# then execs the harness.
set -euo pipefail

usage() {
  cat <<'EOF'
Usage: test/integration/compute/run.sh [options] [selection]

Selection (default: unit then fast):
  unit            only the live-DB tests — the pageserver/safekeeper
                  modules (page_store, page_writer, yb_wal_storage) plus
                  the yb_thin_client live suite (tests/live.rs) — pointed at
                  this run's cluster; no developer-managed cluster on
                  5433/yb_storage needed
  fast            only the fast suite
  slow            only the slow suite
  all             unit, fast, then slow
  pg_regress      the full upstream PostgreSQL suite — hands off to
                  pg_regress.sh (see -h there), in this script's profile and
                  with the remaining arguments forwarded
  <name>          one test by name (substring also matches), e.g.
                  branch_ancestor, restart_wal_replay_yb, smoke

Options:
  -n <count>      repeat the selection <count> times and print a
                  cumulative pass/fail count (run-all: failures are
                  tallied, not fatal). Useful for flakiness hunting.
                  Iterations are isolated: a scenario's cluster and branch
                  names carry the iteration, and the previous iteration's
                  endpoints are destroyed (logs kept under
                  iteration_<n>_logs/ in the run dir).
  --release       build + run release throughout — cluster binaries, the
                  extension, and PostgreSQL — the same meaning build.sh gives
                  it. A release PostgreSQL has assertions compiled OUT, so the
                  run says so; CI always runs the default. On a checkout that
                  has only run these suites the first --release builds a whole
                  second PostgreSQL. (Default: debug, which is what the
                  dev/integration loop normally wants.)
  --no-fail-stop  on failure, leave the cluster + DB running for postmortem
                  (default: a failed run stops the cluster + drops the DB but
                  keeps its *.log files under .integration-tests/run_<id>/)
  -h, --help      show this help

Honoured env vars (passed through): INTEGRATION_REGEN_EXPECTED, TEST_DIR,
YB_AMP_REPO_DIR, YB_DBNAME, YB_HOST, YB_PORT, COMPUTE_PORT,
YB_SSLMODE, YB_SSLROOTCERT, YB_SSLCERT, YB_SSLKEY (the daemons' TLS to YB;
scripts/yb-local.sh with YB_TLS=1 produces the files), YB_RPC_PORT (tserver
RPC for the thin lane, default 9100; yb-local.sh prints the offset one),
YB_TLS_CA_KEY (the CA key tls_cert_reload re-issues the client cert with;
default: ca.key beside YB_SSLROOTCERT), YB_TLS_RELOAD_POLL_SECS (default 2 here),
SOAK_SECS, PS_HTTP_TIMEOUT, ENDPOINT_START_TIMEOUT,
YB_PS_VERIFY_WAL_ROUNDTRIP, YB_AMP, INTEGRATION_PORT_BASE.

Every run is isolated: it synthesizes its own config on a free port
block (auto-picked in 20000..60000), uses an ephemeral db + run dir, and
scopes teardown to itself — so suites can run CONCURRENTLY (and never
touch a developer's local cluster). INTEGRATION_PORT_BASE=<n> pins the block.

INTEGRATION_SKIP=<substr>[,<substr>...] drops matching tests from the run (e.g.
INTEGRATION_SKIP=going_away,restart_wal_replay_yb to skip some); the skipped names
are printed, never silently dropped.

Examples:
  test/integration/compute/run.sh                       # unit + fast, debug
  test/integration/compute/run.sh all                   # unit, fast, then slow
  test/integration/compute/run.sh unit                  # live-DB cargo tests only
  test/integration/compute/run.sh smoke                 # one golden workload
  test/integration/compute/run.sh -n 20 restart_wal_replay_yb   # repeat 20x, count
  test/integration/compute/run.sh --release all         # full suite, release build
  test/integration/compute/run.sh pg_regress            # upstream PostgreSQL suite
EOF
}

# The amp tree root. Derived from this script's own location rather than
# `git rev-parse --show-toplevel`, which returns the repository root — not the
# same directory once amp/ is a subdirectory of a larger repo.
REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
cd "$REPO"

PROFILE=debug
ARGS=()
for arg in "$@"; do
  case "$arg" in
  -h | --help)
    usage
    exit 0
    ;;
  --release) PROFILE=release ;;
  --no-fail-stop) export INTEGRATION_NO_FAIL_STOP=1 ;;
  *) ARGS+=("$arg") ;; # forwarded to the harness (selection, -n, …)
  esac
done

# Build the harness and every binary it drives. Debug by default (per
# project convention for the dev/integration loop); --release for release
# verification.
#
# `yb_amp_local` launches the cluster by spawning the server binaries
# from target/<profile>/ at runtime — it does not build them — so they
# must be compiled here too. Omitting one silently runs a STALE binary
# from a previous build, which surfaces as baffling runtime mismatches
# (e.g. a fresh launcher sending a request shape an old controller
# can't parse). Keep this list in sync with the binaries `yb_amp_local`
# spawns: controller, pageserver, safekeeper,
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
  --bin pageserver
  --bin safekeeper
  --bin compute-ctl
  # yb_amp_local runs each endpoint's three daemons under this, the same
  # binary that is PID 1 of a deployed endpoint pod. Without it `endpoint
  # start` fails on a missing executable.
  --bin endpoint-supervisor
)
TOOL_BINS=(--bin dump-pages --bin dump-wals --bin reset-consistent-lsn)

# Preflight: every suite needs a YugabyteDB reachable at YB_HOST:YB_PORT — the
# harness connects to it but never starts it. Check before the (multi-minute)
# build so a missing YB fails fast with a pointer, not after a long build.
_yb_host="${YB_HOST:-127.0.0.1}"
_yb_port="${YB_PORT:-5433}"
if ! timeout 5 bash -c ": <>/dev/tcp/${_yb_host}/${_yb_port}" 2>/dev/null; then
  cat >&2 <<EOF
run.sh: no YugabyteDB reachable at ${_yb_host}:${_yb_port}.
The integration harness needs one running — it does not start it for you. Either:
  * start the CI-matched image:  scripts/yb-local.sh start
  * point at an existing YB:      YB_HOST=<host> YB_PORT=<port> $0 ...
EOF
  exit 1
fi

# The upstream pg_regress suite has its own driver (own cluster, own schedule, no
# Rust harness), so hand off — after the preflight above, so a missing YB fails the
# same way for it. The selection is positional like any other, and this script's
# conventions win over the driver's own defaults: debug unless --release, with the
# rest of the arguments forwarded (an option only this script knows, e.g. -n, comes
# back as the driver's "unknown arg" error rather than being ignored).
if [[ " ${ARGS[*]-} " == *" pg_regress "* ]]; then
  _forward=()
  for _arg in ${ARGS[0]+"${ARGS[@]}"}; do
    [[ $_arg == pg_regress ]] || _forward+=("$_arg")
  done
  exec "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/pg_regress.sh" \
    "--$PROFILE" ${_forward[0]+"${_forward[@]}"}
fi

# The pageserver and safekeeper link against the prebuilt `libyb_thin_client.so`
# C++ shim. They now carry their own DT_RUNPATH to the pinned shim (each crate's
# build.rs emits it from the `DEP_YB_THIN_CLIENT_LIB_DIR` cargo passes down), so
# this export is no longer required for a repo-local run.
#
# It is kept deliberately, for two reasons. It pins the search path explicitly
# rather than relying on whatever a binary happens to have been built against —
# which matters when a stale `target/` predates a shim version bump. And
# RUNPATH is searched AFTER LD_LIBRARY_PATH, so setting it here still wins,
# exactly as it does in the deploy images. Cheap insurance: build.rs fetches and
# caches the shim itself, so this call is a cache hit that only prints the path.
export LD_LIBRARY_PATH="$("$REPO/scripts/fetch-yb-thin-client.sh")${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}"

# Resolve the assertion-enabled PostgreSQL tree this suite runs against, and
# point the cluster at it (POSTGRES_DISTRIB_DIR). See pg_tree.sh for why it is
# separate from build.sh's release install.
# shellcheck source=test/integration/compute/pg_tree.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/pg_tree.sh"

# Ensure that install matches the checked-out submodule commit before cargo runs
# — the compute's extension .so links against it, so a stale one (after a
# submodule bump or a PG header/source change) surfaces as baffling ABI/wire
# mismatches. run.sh does not build PG itself; delegate that single step to the
# build entry point, which rebuilds only when the submodule commit drifted and
# drops the extension stamp so the guard below recompiles the .so against the
# new headers.
pg_tree_build

# Say which PostgreSQL this run got — the suite's assertions are the whole point.
# Before the cargo/extension builds below, for the same reason the YugabyteDB
# preflight is: a run that would be refused should be refused in seconds, not
# after several minutes of compiling.
pg_tree_report

# The safekeeper is built with its `testing` feature so failpoints
# (`PUT /v1/failpoints`) are usable by fault-injection scenarios.
# Failpoints are inert unless configured, so the binary
# behaves identically to the production build otherwise.
if [[ $PROFILE == release ]]; then
  cargo build --release --features pageserver/testing,safekeeper/testing \
    --bin integration_tests \
    "${CLUSTER_BINS[@]}" "${TOOL_BINS[@]}"
  export YB_AMP=${YB_AMP:-$REPO/target/release/yb_amp_local}
  BIN=$REPO/target/release/integration_tests
  _PROFILE_DIR=release
else
  cargo build --features pageserver/testing,safekeeper/testing \
    --bin integration_tests \
    "${CLUSTER_BINS[@]}" "${TOOL_BINS[@]}"
  BIN=$REPO/target/debug/integration_tests
  _PROFILE_DIR=debug
fi

# The controller is a Go binary (not a cargo target). yb_amp_local spawns it
# from beside itself (target/<profile>/controller), so build it into the same
# directory the cargo binaries land in.
(cd "$REPO/controller" && go build -o "$REPO/target/$_PROFILE_DIR/controller" ./cmd/controller)

# The yb_amp_local launcher is also a Go binary now (was the Rust control_plane
# crate). Build it into the same target/<profile>/ dir the cargo binaries land in,
# so the harness (YB_AMP / target/<profile>/yb_amp_local) and the launcher's own
# next-to-itself controller lookup both resolve.
(cd "$REPO/controller" && go build -o "$REPO/target/$_PROFILE_DIR/yb_amp_local" ./local/cmd/yb_amp_local)

# Rebuild the yb_amp Postgres extension when its C sources changed — see
# pg_tree.sh for why cargo does not build it and why the rebuild is clean.
pg_tree_rebuild_ext_if_stale

# Run-scoped identifiers shared by the harness and the cleanup trap below.
# The harness honours these env vars (src/cluster.rs::setup); pre-setting
# them lets the trap tear down THIS run's cluster on a signal, where the
# harness's own Drop-based teardown never runs. Honour caller-provided
# values (e.g. a developer pinning a dir/db).
RUN_ID="${EPOCHSECONDS:-$(date +%s)}_$$"
export YB_AMP_REPO_DIR="${YB_AMP_REPO_DIR:-$REPO/.integration-tests/run_${RUN_ID}}"
export YB_DBNAME="${YB_DBNAME:-yb_storage_integration_${RUN_ID}}"

# Liveness lock for THIS run. We hold an exclusive flock on the SIBLING
# file `<run_dir>.lock` for run.sh's whole lifetime; the kernel releases it
# when run.sh exits OR is killed (SIGKILL / OOM included). So "lock held"
# reliably means "this run is alive", which is what reap.sh keys on to tell
# a dead run's orphans from a live concurrent run. Held by run.sh only —
# closed in the harness child below — so an orphaned cluster process can
# never keep it held.
#
# The lock MUST live outside the run dir: the harness's own setup clean
# (`Cluster::setup` -> clean.sh) removes and recreates the run dir, and an
# in-dir lock file vanishing made reap.sh treat every live run as dead —
# a concurrently starting run then killed its processes and dropped its
# ephemeral DB mid-run.
mkdir -p "$YB_AMP_REPO_DIR"
exec {RUN_LOCK_FD}>"${YB_AMP_REPO_DIR}.lock"
flock -n "$RUN_LOCK_FD" ||
  {
    echo "run.sh: $YB_AMP_REPO_DIR is locked by another run; aborting" >&2
    exit 1
  }

# Reap orphans left by runs that are no longer alive (compute postgres that
# outlived a hung `endpoint stop`, or anything from a SIGKILL'd/OOM'd run)
# BEFORE we build and bring up — that is exactly when the box is most
# starved. Scoped to dead runs; a live concurrent run holds its lock and is
# skipped. Best-effort: never let a reap hiccup fail the run.
bash "$REPO/test/integration/compute/reap.sh" "$REPO/.integration-tests" "$YB_AMP_REPO_DIR" || true

# Backstop teardown for ABNORMAL termination only. A normal exit (success
# or failure — including --no-fail-stop's deliberate keep-alive) is handled
# by the harness's own Drop, so we do NOT trap EXIT; only the signals that
# bypass Drop (Ctrl-C, kill, hangup). clean.sh is scoped to this run via the
# env above, so it never touches a concurrent run or a dev .yb_amp cluster.
cleanup() {
  trap - INT TERM HUP
  echo >&2
  echo "run.sh: signalled — tearing down this run's integration cluster (db=$YB_DBNAME)" >&2
  if [ -n "${HARNESS_PID:-}" ]; then kill "$HARNESS_PID" 2>/dev/null || true; fi
  YB_HOST="${YB_HOST:-127.0.0.1}" YB_PORT="${YB_PORT:-5433}" \
    bash "$REPO/test/integration/compute/clean.sh" >/dev/null 2>&1 || true
  exit 130
}
trap cleanup INT TERM HUP

# Foreground (not exec) so the trap stays installed and fires on a signal.
# Close the run lock FD in the child before exec'ing the harness, so neither
# the harness nor any cluster process it spawns (compute_ctl -> postgres, …)
# inherits it. Otherwise an orphaned postgres would keep the lock held and
# reap.sh could never tell the run had died.
(
  exec {RUN_LOCK_FD}>&-
  exec "$BIN" ${ARGS[@]+"${ARGS[@]}"}
) &
HARNESS_PID=$!
wait "$HARNESS_PID" || EXIT_CODE=$?
exit "${EXIT_CODE:-0}"
