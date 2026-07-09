#!/usr/bin/env bash
#
# Run the upstream PostgreSQL regression suite (pg_regress) against a
# yb-light compute endpoint backed by YugabyteDB — see issue #119.
#
# Unlike run.sh (the Rust e2e harness with checked-in golden workloads),
# this drives the FULL upstream suite straight from the vendored Postgres
# source (vendor/postgres-v18/src/test/regress) via the stock pg_regress
# binary. It brings up a throwaway yb_light_local cluster (compute ->
# yb-pageserver/safekeeper -> YugabyteDB), creates the `regression`
# database, runs the parallel schedule, and tears the cluster down.
#
# Defaults to RELEASE: the full suite is ~200 tests with getpage/WAL
# round-trips, so a debug build is painfully slow. Pass --debug to override.
#
# This script also builds the regression C support modules (regress,
# refint, autoinc) via PGXS and installs them into the compute's $libdir,
# then points pg_regress at them with --dlpath, so the dynlib-gated tests
# actually run rather than erroring on the restricted-loader path.
#
# Expected result (PG18, see #119): 0 failures. The only diffs the suite
# produces are the one by-design limitation — `CREATE TABLESPACE`
# unsupported — in 4 tests (`test_setup`, `tablespace`, `stats`,
# `database`). Those are blessed via yb-light expected-output variants in
# regress_yb_expected/ (overlaid onto pg_regress's expected/<test>_N.out
# alternative-file mechanism, NOT by editing the upstream goldens), so a
# clean tree is a genuine green. No page-reconstruction or WAL bugs
# surface, and no genuine yb-light behavioral diffs remain.
set -euo pipefail

usage() {
    cat <<'EOF'
Usage: test/e2e/storage/pg_regress.sh [options]

Options:
  --debug             build/run the cluster binaries in debug (default: release)
  -j, --jobs <n>      pg_regress --max-connections (default: 8). Higher = faster
                      but more PS->YB connection-pool pressure.
  --schedule <file>   schedule to run (default: the vendored parallel_schedule).
                      Relative to the regress dir or an absolute path.
  --keep              leave the cluster + regression DB up for postmortem
                      (default: tear down on exit).
  -h, --help          show this help

Honoured env vars: YB_HOST, YB_PORT (the backing YugabyteDB; default
127.0.0.1:5433), E2E_PORT_BASE (pin the cluster's port block),
YB_LIGHT_REPO_DIR, YB_DBNAME.

Output (results + regression.diffs) is written under the run dir and
printed at the end. Exit status is pg_regress's (non-zero while the known
diffs remain un-allowlisted).
EOF
}

REPO=$(git rev-parse --show-toplevel)
cd "$REPO"

PROFILE=release
JOBS=8
SCHEDULE=parallel_schedule
KEEP=0
while [[ $# -gt 0 ]]; do
    case "$1" in
        --debug) PROFILE=debug ;;
        -j|--jobs) JOBS="$2"; shift ;;
        --schedule) SCHEDULE="$2"; shift ;;
        --keep) KEEP=1 ;;
        -h|--help) usage; exit 0 ;;
        *) echo "unknown arg: $1" >&2; usage; exit 2 ;;
    esac
    shift
done

REG="$REPO/vendor/postgres-v18/src/test/regress"
PGINST="$REPO/pg_install/v18"
# yb-light expected-output variants for the by-design CREATE TABLESPACE
# diffs; overlaid into the vendored expected/ at run time (see below).
YBEXP="$REPO/test/e2e/storage/regress_yb_expected"
OVERLAID=()   # variant files copied into expected/, removed by finish()

# 1) Vendored Postgres source carries the suite (sql/expected/schedule).
if [[ ! -f "$REG/parallel_schedule" ]]; then
    echo ">>> populating vendor/postgres-v18 submodule (one-time)..."
    git submodule update --init vendor/postgres-v18
fi
[[ "$SCHEDULE" = /* ]] || SCHEDULE="$REG/$SCHEDULE"
[[ -f "$SCHEDULE" ]] || { echo "schedule not found: $SCHEDULE" >&2; exit 1; }

# 2) Build the binaries yb_light_local spawns at runtime (it does not build
#    them itself — a stale one surfaces as baffling runtime mismatches).
CLUSTER_BINS=(--bin yb_light_local --bin storage_controller --bin yb-pageserver
              --bin yb-safekeeper --bin yb-endpoint-storage --bin yb-compute-ctl)
echo ">>> building cluster binaries ($PROFILE)..."
if [[ $PROFILE == release ]]; then
    cargo build --release "${CLUSTER_BINS[@]}"
    YB_LIGHT="$REPO/target/release/yb_light_local"
else
    cargo build "${CLUSTER_BINS[@]}"
    YB_LIGHT="$REPO/target/debug/yb_light_local"
fi

# 2b) Build + install the regression suite's C support modules. The core
#     tests load helper functions/types from regress.so, and the trigger
#     tests from contrib/spi (refint.so, autoinc.so); none of these ship in
#     pg_install. yb-light's loader (dfmgr.c) only accepts libraries resident
#     in pkglib, so build them via PGXS and drop them there. pg_regress is
#     pointed at pkglib via --dlpath below so the suite's @libdir@/<mod>
#     references resolve.
PG_CONFIG="$PGINST/bin/pg_config"
export LD_LIBRARY_PATH="$PGINST/lib:${LD_LIBRARY_PATH:-}"  # pg_config/make need it
# Resolve the REAL pkglib the running postgres uses: pg_install is a symlink
# in worktrees and `pg_config --pkglibdir` carries a stale baked build path,
# so derive it from the binary location — that's what the loader strips.
PKGLIB="$(readlink -f "$PGINST/lib/postgresql")"
echo ">>> building regression support modules (regress, refint, autoinc) -> $PKGLIB"
tmod="$(mktemp -d)"
cp "$REG/regress.c" \
   "$REPO/vendor/postgres-v18/contrib/spi/refint.c" \
   "$REPO/vendor/postgres-v18/contrib/spi/autoinc.c" "$tmod/"
# -DREFINT_VERBOSE matches contrib/spi/Makefile: without it refint's
# check_foreign_key skips the cascade NOTICEs the `triggers` test expects
# (the cascade still runs correctly — only the NOTICE text is gated).
cat > "$tmod/Makefile" <<MK
MODULES = regress refint autoinc
PG_CPPFLAGS = -DREFINT_VERBOSE
PG_CONFIG = $PG_CONFIG
PGXS := \$(shell \$(PG_CONFIG) --pgxs)
include \$(PGXS)
MK
if ! make -C "$tmod" >"$tmod/build.log" 2>&1; then
    echo "test-module build failed:" >&2; tail -20 "$tmod/build.log" >&2; rm -rf "$tmod"; exit 1
fi
cp "$tmod"/regress.so "$tmod"/refint.so "$tmod"/autoinc.so "$PKGLIB/"
rm -rf "$tmod"

# 3) Run-scoped, isolated cluster identifiers (mirrors run.sh / clean.sh).
RUN_ID="${EPOCHSECONDS:-$(date +%s)}_$$"
export YB_HOST="${YB_HOST:-127.0.0.1}"
export YB_PORT="${YB_PORT:-5433}"
export YB_DBNAME="${YB_DBNAME:-yb_storage_pgregress_${RUN_ID}}"
export YB_LIGHT_REPO_DIR="${YB_LIGHT_REPO_DIR:-$REPO/.e2e-tests/pg_regress_${RUN_ID}}"
# The cluster needs to find the Postgres install at runtime; the compute
# (and pg_regress's psql) need its libs.
export POSTGRES_DISTRIB_DIR="$REPO/pg_install"
export LD_LIBRARY_PATH="$PGINST/lib:${LD_LIBRARY_PATH:-}"

# Pick a free port block (base..base+700): SC=base, PS pg/http/grpc=+300/+400/+500,
# SK=+100/+200, endpoint_storage=+600, compute=+700.
pick_base() {
    local b
    for b in "${E2E_PORT_BASE:-53000}" 54000 55000 56000 57000 58000; do
        local busy=0 p
        for p in "$b" $((b+300)) $((b+400)) $((b+700)); do
            ss -ltn 2>/dev/null | grep -q ":${p}\b" && { busy=1; break; }
        done
        [[ $busy == 0 ]] && { echo "$b"; return 0; }
    done
    echo "no free port block found" >&2; return 1
}
BASE=$(pick_base)
COMPUTE=$((BASE + 700))
export COMPUTE_PORT="$COMPUTE"

clean() { bash "$REPO/test/e2e/storage/clean.sh" >/dev/null 2>&1 || true; }
# Tear down on ANY exit (success, set -e failure, or signal) unless --keep,
# so a failed bring-up never leaks a half-up cluster. clean.sh is scoped to
# this run's db + dir.
finish() {
    local rc=$?
    trap - EXIT INT TERM HUP
    # Remove the overlaid expected variants so the vendored submodule is
    # left pristine (do this even with --keep — it's a source-tree edit,
    # not cluster state).
    [[ ${#OVERLAID[@]} -gt 0 ]] && rm -f "${OVERLAID[@]}"
    if [[ $KEEP == 1 ]]; then
        echo ">>> --keep: cluster left up (compute 127.0.0.1:${COMPUTE:-?}, db=$YB_DBNAME); results in ${OUTDIR:-n/a}"
        echo "    tear down: YB_DBNAME=$YB_DBNAME YB_LIGHT_REPO_DIR=$YB_LIGHT_REPO_DIR COMPUTE_PORT=${COMPUTE:-} bash test/e2e/storage/clean.sh"
    else
        echo ">>> tearing down"; clean
    fi
    exit "$rc"
}
trap finish EXIT INT TERM HUP
clean   # clear any leftover state for this (db, dir) up front

# Overlay the yb-light expected variants. pg_regress tries expected/<test>_N.out
# (N=0..9) and passes if any matches, so these bless the by-design CREATE
# TABLESPACE diffs as known-good without editing the upstream goldens. Copied
# in here and removed by finish(), so the vendored submodule stays clean.
# Regenerate after an intended change: cp "$OUTDIR"/results/<test>.out to
# $YBEXP/<test>_<N>.out (N = an index not already used upstream).
if [[ -d $YBEXP ]]; then
    echo ">>> overlaying yb-light expected variants: $(cd "$YBEXP" && echo *.out)"
    for v in "$YBEXP"/*.out; do
        dst="$REG/expected/$(basename "$v")"
        [[ -e $dst ]] && { echo "refusing to clobber existing $dst" >&2; exit 1; }
        cp "$v" "$dst"; OVERLAID+=("$dst")
    done
fi

echo ">>> bringing up yb-light cluster (db=$YB_DBNAME base=$BASE compute=$COMPUTE)"
yb() { "$YB_LIGHT" "$@"; }
yb init --yb-dbname "$YB_DBNAME" --yb-host "$YB_HOST" --yb-port "$YB_PORT" --port-base "$BASE"
yb start
TID=$(yb tenant create --pg-version 18 | grep -oE '[0-9a-f]{32}' | head -1)
[[ -n "$TID" ]] || { echo "tenant create failed" >&2; clean; exit 1; }
yb endpoint create main --tenant-id "$TID" --branch-name main --pg-version 18 --pg-port "$COMPUTE"
yb endpoint start main --start-timeout 600s

echo ">>> running pg_regress (schedule=$(basename "$SCHEDULE") jobs=$JOBS) against 127.0.0.1:$COMPUTE"
# Results live OUTSIDE the run dir so they survive teardown (clean.sh rm's
# YB_LIGHT_REPO_DIR). Old result dirs are reaped by the same 7-day GC as runs.
OUTDIR="${YB_LIGHT_REPO_DIR}_out"
rm -rf "$OUTDIR"; mkdir -p "$OUTDIR"
status=0
"$PGINST/lib/postgresql/pgxs/src/test/regress/pg_regress" \
    --inputdir="$REG" --outputdir="$OUTDIR" --bindir="$PGINST/bin" \
    --dlpath="$PKGLIB" \
    --schedule="$SCHEDULE" \
    --host=127.0.0.1 --port="$COMPUTE" --user=cloud_admin --dbname=regression \
    --max-connections="$JOBS" || status=$?

echo
echo ">>> results under: $OUTDIR"
[[ -s "$OUTDIR/regression.diffs" ]] && echo ">>> diffs:        $OUTDIR/regression.diffs"
exit "$status"   # teardown (or --keep) handled by the finish() EXIT trap
