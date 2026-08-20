#!/usr/bin/env bash
#
# Run the upstream PostgreSQL regression suite (pg_regress) against a
# yb-amp compute endpoint backed by YugabyteDB — see issue #119.
#
# Unlike run.sh (the Rust integration harness with checked-in golden workloads),
# this drives the FULL upstream suite straight from the vendored Postgres
# source (compute/vendor/postgres-v18/src/test/regress) via the stock pg_regress
# binary. It brings up a throwaway yb_amp_local cluster (compute ->
# pageserver/safekeeper -> YugabyteDB), creates the `regression`
# database, runs the parallel schedule, and tears the cluster down.
#
# Standalone it builds what it needs, defaulting to RELEASE: the full suite is 230
# tests with getpage/WAL round-trips, so a debug build is slower. Pass --debug to
# override, or --no-build to run on a build a caller already produced — which is
# what CI does, straight after run.sh in the same job.
#
# This script also builds the regression C support modules (regress,
# refint, autoinc) via PGXS and installs them into the compute's $libdir,
# then points pg_regress at them with --dlpath, so the dynlib-gated tests
# actually run rather than erroring on the restricted-loader path.
#
# Expected result on PG18: 230/230, matching the upstream goldens exactly.
# The compute runs with `yb_amp.regress_test_mode=on` (see below), which is
# what keeps it there — without it the by-design `CREATE TABLESPACE` rejection
# fails test_setup, tablespace and stats.
set -euo pipefail

usage() {
    cat <<'EOF'
Usage: test/integration/compute/pg_regress.sh [options]

Options:
  --no-build          build nothing: use the Postgres install, yb_amp extension
                      and cluster binaries already in the tree, taking the profile
                      from whichever of target/{release,debug} holds them. Fails
                      if any is missing. What CI uses, running straight after
                      run.sh in the same job.
  --debug             build/run the cluster binaries in debug (default: release)
  --release           the default; accepted so `run.sh --release pg_regress`
                      forwards cleanly
  -j, --jobs <n>      pg_regress --max-connections (default: 8). Higher = faster
                      but more PS->YB connection-pool pressure.
  --schedule <file>   schedule to run (default: the vendored parallel_schedule).
                      Relative to the regress dir or an absolute path.
  --keep              leave the cluster + regression DB up for postmortem
                      (default: tear down on exit).
  -h, --help          show this help

Honoured env vars: YB_HOST, YB_PORT (the backing YugabyteDB; default
127.0.0.1:5433), INTEGRATION_PORT_BASE (pin the cluster's port block),
YB_AMP_REPO_DIR, YB_DBNAME.

Output (results + regression.diffs) is written under the run dir and
printed at the end. Exit status is pg_regress's.
EOF
}

# The amp tree root. Derived from this script's own location rather than
# `git rev-parse --show-toplevel`, which returns the repository root — not the
# same directory once amp/ is a subdirectory of a larger repo.
REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
cd "$REPO"

PROFILE=release
JOBS=8
SCHEDULE=parallel_schedule
KEEP=0
NO_BUILD=0
while [[ $# -gt 0 ]]; do
    case "$1" in
        --no-build) NO_BUILD=1 ;;
        --debug) PROFILE=debug ;;
        --release) PROFILE=release ;;
        -j|--jobs) JOBS="$2"; shift ;;
        --schedule) SCHEDULE="$2"; shift ;;
        --keep) KEEP=1 ;;
        -h|--help) usage; exit 0 ;;
        *) echo "unknown arg: $1" >&2; usage; exit 2 ;;
    esac
    shift
done

REG="$REPO/compute/vendor/postgres-v18/src/test/regress"
PGINST="$REPO/pg_install/v18"

# 1) Vendored Postgres source carries the suite (sql/expected/schedule).
if [[ ! -f "$REG/parallel_schedule" ]]; then
    echo ">>> populating compute/vendor/postgres-v18 submodule (one-time)..."
    git submodule update --init compute/vendor/postgres-v18
fi
[[ "$SCHEDULE" = /* ]] || SCHEDULE="$REG/$SCHEDULE"
[[ -f "$SCHEDULE" ]] || { echo "schedule not found: $SCHEDULE" >&2; exit 1; }

# 2) Everything the suite needs to run: a Postgres install matching the
#    checked-out submodule (postgres_ffi binds against its headers, so the cargo
#    build needs it too), a yb_amp.so built from this tree's C sources, and the
#    binaries yb_amp_local spawns at runtime — it does not build them, and a
#    stale one surfaces as a baffling runtime mismatch.
#
#    --no-build: a caller already produced all of it in this tree, so touch
#    nothing. CI relies on that — it runs this straight after run.sh in the same
#    job, and building here would not just duplicate work but UNDO it: build.sh
#    requires a release Postgres, run.sh's extension make leaves a cassert one
#    (Makefile BUILD_TYPE defaults to debug), so each would wipe and rebuild
#    pg_install behind the other, dragging the postgres_ffi-dependent crates with
#    it. The profile comes from whatever is in target/, so it follows run.sh
#    rather than needing to agree with it.
CLUSTER_BINS=(pageserver safekeeper compute-ctl
              # yb_amp_local runs the three daemons under this.
              endpoint-supervisor
              # Go, not cargo targets: the launcher and the controller it spawns.
              yb_amp_local controller)
if [[ $NO_BUILD == 1 ]]; then
    for _p in release debug; do
        _missing=0
        for _b in "${CLUSTER_BINS[@]}"; do
            [[ -x "$REPO/target/$_p/$_b" ]] || { _missing=1; break; }
        done
        [[ $_missing == 0 ]] && { _PROFILE_DIR=$_p; break; }
    done
    [[ -n ${_PROFILE_DIR:-} ]] || {
        echo "--no-build: no complete cluster build in target/{release,debug}." >&2
        echo "Run test/integration/compute/run.sh first, or drop --no-build." >&2
        exit 1
    }
    [[ -x "$PGINST/bin/postgres" && -f "$PGINST/lib/postgresql/yb_amp.so" ]] || {
        echo "--no-build: pg_install/v18 has no postgres or no yb_amp.so." >&2
        exit 1
    }
    PROFILE=$_PROFILE_DIR
    YB_AMP="$REPO/target/$_PROFILE_DIR/yb_amp_local"
    echo ">>> reusing the existing $_PROFILE_DIR build (--no-build)"
else
    # The extension gets a CLEAN rebuild when its sources changed — PGXS has no
    # header dependency tracking — keyed on a stamp rather than the installed .so,
    # whose mtime ninstall.sh preserves when the rebuild is byte-identical.
    # `env -u PROFILE`: PGXS reads $PROFILE as C flags, and ours holds debug|release.
    env -u PROFILE "$REPO/build.sh" --pg-only
    EXT_SO="$PGINST/lib/postgresql/yb_amp.so"
    EXT_STAMP="$REPO/build/.yb_amp_ext.stamp"
    if [[ ! -f $EXT_SO ]] || [[ ! -f $EXT_STAMP ]] \
        || [[ -n "$(find "$REPO/compute/pgxn" -type f \( -name '*.c' -o -name '*.h' -o -name 'Makefile' \) -newer "$EXT_STAMP" -print -quit)" ]]; then
        echo ">>> yb_amp extension is stale (C sources changed or not yet built) — clean-rebuilding"
        rm -rf "$REPO/build/pgxn-v18"
        env -u PROFILE make -C "$REPO" yb-amp-pg-ext-v18
        mkdir -p "$REPO/build"
        touch "$EXT_STAMP"
    fi

    # Same features run.sh builds the cluster binaries with, so a tree it already
    # built is not recompiled under a second feature set.
    echo ">>> building cluster binaries ($PROFILE)..."
    _CARGO_BINS=(--bin pageserver --bin safekeeper --bin compute-ctl --bin endpoint-supervisor)
    FEATURES=(--features "pageserver/testing,safekeeper/testing")
    if [[ $PROFILE == release ]]; then
        cargo build --release "${FEATURES[@]}" "${_CARGO_BINS[@]}"
        _PROFILE_DIR=release
    else
        cargo build "${FEATURES[@]}" "${_CARGO_BINS[@]}"
        _PROFILE_DIR=debug
    fi
    YB_AMP="$REPO/target/$_PROFILE_DIR/yb_amp_local"
    ( cd "$REPO/controller" && go build -o "$REPO/target/$_PROFILE_DIR/yb_amp_local" ./local/cmd/yb_amp_local )
    ( cd "$REPO/controller" && go build -o "$REPO/target/$_PROFILE_DIR/controller" ./cmd/controller )
fi

# 2b) Build + install the regression suite's C support modules. The core
#     tests load helper functions/types from regress.so, and the trigger
#     tests from contrib/spi (refint.so, autoinc.so); none of these ship in
#     pg_install. yb-amp's loader (dfmgr.c) only accepts libraries resident
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
   "$REPO/compute/vendor/postgres-v18/contrib/spi/refint.c" \
   "$REPO/compute/vendor/postgres-v18/contrib/spi/autoinc.c" "$tmod/"
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
# `env -u PROFILE` for the same reason as the extension build above: PGXS reads
# $PROFILE as C flags, and ours holds debug|release.
if ! env -u PROFILE make -C "$tmod" >"$tmod/build.log" 2>&1; then
    echo "test-module build failed:" >&2; tail -20 "$tmod/build.log" >&2; rm -rf "$tmod"; exit 1
fi
cp "$tmod"/regress.so "$tmod"/refint.so "$tmod"/autoinc.so "$PKGLIB/"
rm -rf "$tmod"

# 3) Run-scoped, isolated cluster identifiers (mirrors run.sh / clean.sh).
RUN_ID="${EPOCHSECONDS:-$(date +%s)}_$$"
export YB_HOST="${YB_HOST:-127.0.0.1}"
export YB_PORT="${YB_PORT:-5433}"
export YB_DBNAME="${YB_DBNAME:-yb_storage_pgregress_${RUN_ID}}"
export YB_AMP_REPO_DIR="${YB_AMP_REPO_DIR:-$REPO/.integration-tests/pg_regress_${RUN_ID}}"
# The cluster needs to find the Postgres install at runtime; the compute
# (and pg_regress's psql) need its libs.
export POSTGRES_DISTRIB_DIR="$REPO/pg_install"
export LD_LIBRARY_PATH="$PGINST/lib:${LD_LIBRARY_PATH:-}"

# Low end of the OS ephemeral (dynamic) port range; mirrors the Rust harness's
# ephemeral_floor (test/integration/compute/cluster.rs).
ephemeral_floor() {
    local lo=""
    if [[ -r /proc/sys/net/ipv4/ip_local_port_range ]]; then
        lo=$(cut -f1 /proc/sys/net/ipv4/ip_local_port_range 2>/dev/null)
    fi
    if [[ $lo =~ ^[0-9]+$ ]] && (( lo > 10000 )); then echo "$lo"; else echo 32768; fi
}

# Pick a free port block (base..base+700): SC=base, SK=+100/+200,
# PS pg/http/grpc=+300/+400/+500, compute=+700.
#
# The whole block must sit BELOW the ephemeral floor, like the harness's
# pick_port_base. `ss` sees only listeners, so a port up there can be taken
# between this probe and the bind by an outbound connection — including our own
# daemons' connections to YB, which is how a CI run lost its compute port on
# :55700 with the cluster already up.
pick_base() {
    local floor b p busy
    floor=$(ephemeral_floor)
    for b in "${INTEGRATION_PORT_BASE:-18000}" 19000 20000 21000 22000 23000; do
        (( b + 700 < floor )) || continue
        busy=0
        # Every port the block actually binds. Probing only some of them let a
        # busy safekeeper or pageserver-grpc port through, and the daemon then
        # died on "Address already in use" instead of this moving to the next block.
        for p in "$b" $((b+100)) $((b+200)) $((b+300)) $((b+400)) $((b+500)) $((b+700)); do
            ss -ltn 2>/dev/null | grep -q ":${p}\b" && { busy=1; break; }
        done
        [[ $busy == 0 ]] && { echo "$b"; return 0; }
    done
    echo "no free port block below the ephemeral floor ($floor)" >&2; return 1
}
BASE=$(pick_base)
COMPUTE=$((BASE + 700))
export COMPUTE_PORT="$COMPUTE"

clean() { bash "$REPO/test/integration/compute/clean.sh" >/dev/null 2>&1 || true; }
# Tear down on ANY exit (success, set -e failure, or signal) unless --keep,
# so a failed bring-up never leaks a half-up cluster. clean.sh is scoped to
# this run's db + dir.
finish() {
    local rc=$?
    trap - EXIT INT TERM HUP
    # A failed run's daemon logs are the postmortem the results dir does not
    # carry, so keep them the way the Rust harness does: clean.sh strips
    # everything but *.log instead of removing the run dir, which is what the
    # workflow's artifact upload globs.
    if [[ $rc -ne 0 ]]; then export INTEGRATION_KEEP_LOGS=1; fi
    if [[ $KEEP == 1 ]]; then
        echo ">>> --keep: cluster left up (compute 127.0.0.1:${COMPUTE:-?}, db=$YB_DBNAME); results in ${OUTDIR:-n/a}"
        echo "    tear down: YB_DBNAME=$YB_DBNAME YB_AMP_REPO_DIR=$YB_AMP_REPO_DIR COMPUTE_PORT=${COMPUTE:-} bash test/integration/compute/clean.sh"
    else
        echo ">>> tearing down"; clean
    fi
    exit "$rc"
}
trap finish EXIT INT TERM HUP
clean   # clear any leftover state for this (db, dir) up front

echo ">>> bringing up yb-amp cluster (db=$YB_DBNAME base=$BASE compute=$COMPUTE)"
yb() { "$YB_AMP" "$@"; }
yb init --yb-dbname "$YB_DBNAME" --yb-host "$YB_HOST" --yb-port "$YB_PORT" --port-base "$BASE"
yb start
BID=$(yb cluster create pgregress --pg-version 18 | grep -oE '[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}' | head -1)
# Teardown is finish()'s job, not this branch's: exiting non-zero through the trap
# is what keeps the failed bring-up's logs.
[[ -n "$BID" ]] || { echo "cluster create failed" >&2; exit 1; }
# yb_amp.regress_test_mode is the escape hatch yb_amp_ddl_handler.c defines for
# exactly this suite: it lets CREATE TABLESPACE through and skips the
# event-trigger superuser gating, both of which the upstream goldens assume.
# Without it test_setup, tablespace and stats fail on the tablespace rejection.
# compute_query_id=regress is what upstream's own `make check` uses: preloading
# pg_stat_statements turns the default `auto` on, and EXPLAIN VERBOSE would then
# print a "Query Identifier" line the goldens don't have. `regress` keeps the ids
# (pg_stat_statements needs them) and suppresses that line.
yb endpoint create main --branch-id "$BID" \
    --pg-setting yb_amp.regress_test_mode=on \
    --pg-setting compute_query_id=regress \
    --pg-version 18 --pg-port "$COMPUTE"
yb endpoint start main --start-timeout 600s

echo ">>> running pg_regress (schedule=$(basename "$SCHEDULE") jobs=$JOBS) against 127.0.0.1:$COMPUTE"
# Results live OUTSIDE the run dir so they survive teardown (clean.sh rm's
# YB_AMP_REPO_DIR). Nothing else reaps them — the harness's GC and reap.sh both
# key on the `run_` prefix, and these deliberately do not use it (reap.sh treats a
# lockless run_ dir as dead and kills its processes, which would hit a live run
# here) — so age out this script's own leftovers, both dirs and results.
find "$REPO/.integration-tests" -maxdepth 1 -type d -name 'pg_regress_*' -mtime +7 \
    -exec rm -rf {} + 2>/dev/null || true
OUTDIR="${YB_AMP_REPO_DIR}_out"
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
