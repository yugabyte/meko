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
# Standalone it builds what it needs, defaulting to DEBUG — the same default
# run.sh has, so the two share one build instead of recompiling the workspace
# under a second profile. (Their cargo feature sets were already aligned for
# that reason; diverging defaults undid it.) Pass --release for a faster run of
# the full 230-test suite, which also means a release PostgreSQL with assertions
# off (see pg_tree.sh). Or --no-build to run on a build a caller already
# produced — which is what CI does, straight after run.sh in the same job.
#
# This script also builds the regression suite's C support module (regress)
# via PGXS and installs it into the compute's $libdir, then points pg_regress
# at $libdir with --dlpath, so the dynlib-gated tests actually run rather than
# erroring on the restricted-loader path.
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
                      and cluster binaries already in the tree, for the profile
                      this run selected (--release, else debug). Fails if any is
                      missing. What CI uses, running straight after run.sh in the
                      same job.
  --debug             the default; accepted so `run.sh pg_regress` forwards cleanly
  --release           build/run release throughout — cluster binaries, extension
                      and PostgreSQL. Faster over 230 tests, but a release
                      PostgreSQL has assertions compiled out; the run says so
  -j, --jobs <n>      pg_regress --max-connections (default: 8). Higher = faster
                      but more PS->YB connection-pool pressure.
  --schedule <file>   schedule to run (default: the vendored parallel_schedule).
                      Relative to the regress dir or an absolute path.
  --contrib <name>    also run contrib/<name>'s own upstream REGRESS tests (not
                      its ISOLATION specs) against the same cluster, after the
                      schedule above. Repeatable. The
                      module must be one postgres.mk installs, or its .so is
                      absent and every test fails to load it. A by-design amp
                      divergence is patched in from
                      contrib_regress_patches/<name>.patch. Naming a module
                      with no REGRESS tests (sslinfo) fails, since nothing
                      would run.
  --all-contrib       --contrib for each of postgres.mk's CONTRIB_MODULES, skipping
                      any with no REGRESS tests. What CI runs.
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

PROFILE=debug
JOBS=8
SCHEDULE=parallel_schedule
CONTRIB=()
NAMED=" " # the modules --contrib named, as opposed to --all-contrib's
ALL_CONTRIB=0
KEEP=0
NO_BUILD=0
while [[ $# -gt 0 ]]; do
  case "$1" in
  --no-build) NO_BUILD=1 ;;
  --debug) PROFILE=debug ;;
  --release) PROFILE=release ;;
  -j | --jobs)
    JOBS="$2"
    shift
    ;;
  --schedule)
    SCHEDULE="$2"
    shift
    ;;
  --contrib)
    CONTRIB+=("$2")
    NAMED+="$2 "
    shift
    ;;
  --all-contrib) ALL_CONTRIB=1 ;;
  --keep) KEEP=1 ;;
  -h | --help)
    usage
    exit 0
    ;;
  *)
    echo "unknown arg: $1" >&2
    usage
    exit 2
    ;;
  esac
  shift
done

if [[ $ALL_CONTRIB == 1 ]]; then
  _mods="$(env -u PROFILE make -s -C "$REPO" -f postgres.mk \
    --eval='amp-print-%: ; @echo $($*)' amp-print-CONTRIB_MODULES)"
  [[ -n $_mods ]] || {
    echo "--all-contrib: postgres.mk has no CONTRIB_MODULES" >&2
    exit 1
  }
  for _m in $_mods; do CONTRIB+=("$_m"); done
  unset _mods _m
fi

REG="$REPO/compute/vendor/postgres-v18/src/test/regress"

CLUSTER_BINS=(pageserver safekeeper compute-ctl
  # yb_amp_local runs the three daemons under this.
  endpoint-supervisor
  # Go, not cargo targets: the launcher and the controller it spawns.
  yb_amp_local controller)

# 0) Settle which profile this run uses, before anything derives from it.
#    --no-build reuses the build for the profile the caller NAMED — debug unless
#    --release — instead of probing target/ for whichever one looks complete.
#    Probing had to break a tie by mtime, and the profile now also picks the
#    PostgreSQL tree, so a stale target/release could take a run the caller meant
#    as debug and quietly run it with assertions off.
#    This has to happen BEFORE pg_tree.sh is sourced, which derives that tree
#    from $PROFILE.
if [[ $NO_BUILD == 1 ]]; then
  _PROFILE_DIR=$PROFILE
  # run.sh has no --debug flag: debug is its default, and it would take the
  # unknown flag for a test name.
  _hint=""
  [[ $PROFILE == release ]] && _hint=" --release"
  for _b in "${CLUSTER_BINS[@]}"; do
    [[ -x "$REPO/target/$_PROFILE_DIR/$_b" ]] || {
      echo "--no-build: target/$_PROFILE_DIR/$_b is missing or not executable." >&2
      echo "Run 'test/integration/compute/run.sh$_hint' first, or drop --no-build." >&2
      exit 1
    }
  done
  unset _b _hint
fi

# The PostgreSQL tree this suite runs against, and the exports that point the
# cluster at it. Follows $PROFILE. See pg_tree.sh.
# shellcheck source=test/integration/compute/pg_tree.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/pg_tree.sh"
PGINST="$PG_TREE_INSTALL_VERSION_DIR"

# 1) Vendored Postgres source carries the suite (sql/expected/schedule).
if [[ ! -f "$REG/parallel_schedule" ]]; then
  echo ">>> populating compute/vendor/postgres-v18 submodule (one-time)..."
  git submodule update --init compute/vendor/postgres-v18
fi
[[ "$SCHEDULE" = /* ]] || SCHEDULE="$REG/$SCHEDULE"
[[ -f "$SCHEDULE" ]] || {
  echo "schedule not found: $SCHEDULE" >&2
  exit 1
}

# 2) Everything the suite needs to run: a Postgres install matching the
#    checked-out submodule (postgres_ffi binds against its headers, so the cargo
#    build needs it too), a yb_amp.so built from this tree's C sources, and the
#    binaries yb_amp_local spawns at runtime — it does not build them, and a
#    stale one surfaces as a baffling runtime mismatch.
#
#    --no-build: a caller already produced all of it in this tree, so touch
#    nothing. CI relies on that — it runs this straight after run.sh in the same
#    job, on exactly what that step built. Its profile was settled in 0).
if [[ $NO_BUILD == 1 ]]; then
  if [[ ! -x "$PGINST/bin/postgres" ]] || ! pg_tree_has_pgxn_libs; then
    echo "--no-build: $PGINST has no postgres or lacks one of: $PG_TREE_PGXN_LIBS." >&2
    exit 1
  fi
  YB_AMP="$REPO/target/$_PROFILE_DIR/yb_amp_local"
  echo ">>> reusing the existing $_PROFILE_DIR build (--no-build)"
  pg_tree_report
else
  pg_tree_build
  # Before the extension + cluster builds below: a run that would be refused
  # should be refused in seconds, not after minutes of compiling.
  pg_tree_report
  pg_tree_rebuild_ext_if_stale

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
  (cd "$REPO/controller" && go build -o "$REPO/target/$_PROFILE_DIR/yb_amp_local" ./local/cmd/yb_amp_local)
  (cd "$REPO/controller" && go build -o "$REPO/target/$_PROFILE_DIR/controller" ./cmd/controller)
fi

# 2b) Build + install the regression suite's C support module. The core
#     tests load helper functions/types from regress.so, which does not ship
#     in pg_install. The trigger tests' refint.so and autoinc.so do: postgres.mk
#     installs contrib/spi, built with the -DREFINT_VERBOSE their cascade
#     NOTICEs need. yb-amp's loader (dfmgr.c) only accepts libraries resident
#     in pkglib, so build regress via PGXS and drop it there. pg_regress is
#     pointed at pkglib via --dlpath below so the suite's @libdir@/<mod>
#     references resolve.
PG_CONFIG="$PGINST/bin/pg_config"
export LD_LIBRARY_PATH="$PGINST/lib:${LD_LIBRARY_PATH:-}" # pg_config/make need it
# Resolve the REAL pkglib the running postgres uses: the install root is a
# symlink in worktrees and `pg_config --pkglibdir` carries a stale baked build
# path, so derive it from the binary location — that's what the loader strips.
PKGLIB="$(readlink -f "$PGINST/lib/postgresql")"
echo ">>> building the regression support module (regress) -> $PKGLIB"
tmod="$(mktemp -d)"
cp "$REG/regress.c" "$tmod/"
cat >"$tmod/Makefile" <<MK
MODULES = regress
PG_CONFIG = $PG_CONFIG
PGXS := \$(shell \$(PG_CONFIG) --pgxs)
include \$(PGXS)
MK
# `env -u PROFILE` for the same reason as the extension build above: PGXS reads
# $PROFILE as C flags, and ours holds debug|release.
if ! env -u PROFILE make -C "$tmod" >"$tmod/build.log" 2>&1; then
  echo "test-module build failed:" >&2
  tail -20 "$tmod/build.log" >&2
  rm -rf "$tmod"
  exit 1
fi
cp "$tmod"/regress.so "$PKGLIB/"
rm -rf "$tmod"

# 3) Run-scoped, isolated cluster identifiers (mirrors run.sh / clean.sh).
RUN_ID="${EPOCHSECONDS:-$(date +%s)}_$$"
export YB_HOST="${YB_HOST:-127.0.0.1}"
export YB_PORT="${YB_PORT:-5433}"
export YB_DBNAME="${YB_DBNAME:-yb_storage_pgregress_${RUN_ID}}"
export YB_AMP_REPO_DIR="${YB_AMP_REPO_DIR:-$REPO/.integration-tests/pg_regress_${RUN_ID}}"
# POSTGRES_DISTRIB_DIR (how the cluster finds the install at runtime) is exported
# by pg_tree.sh. The compute and pg_regress's psql need its libs.
export LD_LIBRARY_PATH="$PGINST/lib:${LD_LIBRARY_PATH:-}"

# Low end of the OS ephemeral (dynamic) port range; mirrors the Rust harness's
# ephemeral_floor (test/integration/compute/cluster.rs).
ephemeral_floor() {
  local lo=""
  if [[ -r /proc/sys/net/ipv4/ip_local_port_range ]]; then
    lo=$(cut -f1 /proc/sys/net/ipv4/ip_local_port_range 2>/dev/null)
  fi
  if [[ $lo =~ ^[0-9]+$ ]] && ((lo > 10000)); then echo "$lo"; else echo 32768; fi
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
    ((b + 700 < floor)) || continue
    busy=0
    # Every port the block actually binds. Probing only some of them let a
    # busy safekeeper or pageserver-grpc port through, and the daemon then
    # died on "Address already in use" instead of this moving to the next block.
    for p in "$b" $((b + 100)) $((b + 200)) $((b + 300)) $((b + 400)) $((b + 500)) $((b + 700)); do
      ss -ltn 2>/dev/null | grep -q ":${p}\b" && {
        busy=1
        break
      }
    done
    [[ $busy == 0 ]] && {
      echo "$b"
      return 0
    }
  done
  echo "no free port block below the ephemeral floor ($floor)" >&2
  return 1
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
    echo ">>> tearing down"
    clean
  fi
  exit "$rc"
}
trap finish EXIT INT TERM HUP
clean # clear any leftover state for this (db, dir) up front

echo ">>> bringing up yb-amp cluster (db=$YB_DBNAME base=$BASE compute=$COMPUTE)"
yb() { "$YB_AMP" "$@"; }
yb init --yb-dbname "$YB_DBNAME" --yb-host "$YB_HOST" --yb-port "$YB_PORT" --port-base "$BASE"
yb start
BID=$(yb cluster create pgregress --pg-version 18 | grep -oE '[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}' | head -1)
# Teardown is finish()'s job, not this branch's: exiting non-zero through the trap
# is what keeps the failed bring-up's logs.
[[ -n "$BID" ]] || {
  echo "cluster create failed" >&2
  exit 1
}
# yb_amp.regress_test_mode is the escape hatch yb_amp_ddl_handler.c defines for
# exactly this suite: it lets CREATE TABLESPACE through and skips the
# event-trigger superuser gating, both of which the upstream goldens assume.
# Without it test_setup, tablespace and stats fail on the tablespace rejection.
# compute_query_id=regress is what upstream's own `make check` uses: preloading
# pg_stat_statements turns the default `auto` on, and EXPLAIN VERBOSE would then
# print a "Query Identifier" line the goldens don't have. `regress` keeps the ids
# (pg_stat_statements needs them) and suppresses that line.
# max_prepared_transactions as pg_regress's own temp instance has it; the other two
# as test_decoding's logical.conf, plus a slot for the walproposer's own.
yb endpoint create main --branch-id "$BID" \
  --pg-setting yb_amp.regress_test_mode=on \
  --pg-setting compute_query_id=regress \
  --pg-setting max_prepared_transactions=2 \
  --pg-setting max_replication_slots=5 \
  --pg-setting logical_decoding_work_mem=64kB \
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
rm -rf "$OUTDIR"
mkdir -p "$OUTDIR"
status=0
"$PGINST/lib/postgresql/pgxs/src/test/regress/pg_regress" \
  --inputdir="$REG" --outputdir="$OUTDIR" --bindir="$PGINST/bin" \
  --dlpath="$PKGLIB" \
  --schedule="$SCHEDULE" \
  --host=127.0.0.1 --port="$COMPUTE" --user=cloud_admin --dbname=regression \
  --max-connections="$JOBS" || status=$?

# pg_stat_statements' suite lists the server-wide view, so untrack the compute's own
# background sessions (compute_ctl's monitor, pgsentinel), both cloud_admin in
# `postgres`. They connect at startup, hence the restart.
if [[ " ${CONTRIB[*]-} " == *" pg_stat_statements "* ]]; then
  echo ">>> untracking cloud_admin in postgres for pg_stat_statements; restarting the endpoint"
  "$PGINST/bin/psql" -h 127.0.0.1 -p "$COMPUTE" -U cloud_admin -d postgres -v ON_ERROR_STOP=1 \
    -c "ALTER ROLE cloud_admin IN DATABASE postgres SET pg_stat_statements.track = 'none'" >/dev/null
  yb endpoint stop main
  yb endpoint start main --start-timeout 600s
fi

# Contrib modules keep their suite next to their source (sql/ + expected/) with
# no schedule, so drive pg_regress at the test list from their Makefile's REGRESS.
# Same endpoint as above — the cluster bring-up is the expensive part.
ran=" "
for mod in ${CONTRIB[@]+"${CONTRIB[@]}"}; do
  [[ $ran != *" $mod "* ]] || continue
  ran+="$mod "
  src="$REPO/compute/vendor/postgres-v18/contrib/$mod"
  [[ -d "$src" ]] || {
    echo ">>> there is no contrib/$mod" >&2
    status=1
    continue
  }
  mkdir -p "$OUTDIR/contrib-$mod"

  # A by-design amp divergence is a patch, contrib_regress_patches/<mod>.patch,
  # applied to a copy of the suite so the submodule stays clean. --forward: an
  # already-applied patch is an error, where --batch alone would reverse it.
  patchf="$REPO/test/integration/compute/contrib_regress_patches/$mod.patch"
  if [[ -f $patchf ]]; then
    cp -r "$src" "$OUTDIR/contrib-$mod/src"
    src="$OUTDIR/contrib-$mod/src"
    patch --batch --forward --fuzz=0 -s -p1 -d "$src" <"$patchf" || {
      echo ">>> $mod.patch no longer applies to contrib/$mod" >&2
      status=1
      continue
    }
  fi

  # Read REGRESS through make, from the patched copy: btree_gin and others
  # continue it over lines.
  tests="$(env -u PROFILE make -s -C "$src" USE_PGXS=1 PG_CONFIG="$PG_CONFIG" \
    --eval='amp-print-%: ; @echo $($*)' amp-print-REGRESS)" || {
    echo ">>> cannot read REGRESS from contrib/$mod" >&2
    status=1
    continue
  }
  # As `make installcheck` does, run nothing for a module with no REGRESS tests:
  # sslinfo's are the TAP tests in src/test/ssl, which need a server of their
  # own. --all-contrib skips it; a module named with --contrib fails instead,
  # because a run that tested nothing must not read as a pass.
  if [[ -z $tests ]]; then
    rm -rf "${OUTDIR:?}/contrib-$mod"
    if [[ $NAMED == *" $mod "* ]]; then
      echo ">>> contrib/$mod has no REGRESS tests to run" >&2
      status=1
    else
      echo ">>> contrib/$mod has no REGRESS tests; skipping"
    fi
    continue
  fi

  # Add a peer superuser role for the run, as extensions.sh does for
  # hexabridge: amp's superuser is cloud_admin, but upstream suites assume the
  # role their own CI runs as. Here it cannot be a fixed name — postgres_fdw's
  # `CREATE USER MAPPING FOR public` passes no `user`, so libpq derives the OS
  # user for the wrapper's loopback connection, and pg_regress has to connect
  # as that same role or the user-mapping tests do not resolve. Superuser
  # because the suite does CREATE EXTENSION on an untrusted module and sets
  # password_required. Dies with the cluster in finish().
  # psql cannot bind :'var' inside a dollar-quoted block, so the name is
  # interpolated by the shell; format('%I') quotes it as an identifier, and
  # `id -un` is machine state, not input.
  osuser="$(id -un)"
  "$PGINST/bin/psql" -h 127.0.0.1 -p "$COMPUTE" -U cloud_admin -d postgres -v ON_ERROR_STOP=1 \
    -c "DO \$\$ BEGIN IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = '$osuser') THEN EXECUTE format('CREATE ROLE %I SUPERUSER LOGIN', '$osuser'); END IF; END \$\$" >/dev/null

  echo ">>> running pg_regress (contrib/$mod: $tests) against 127.0.0.1:$COMPUTE as $osuser"
  # From the module dir, as `make installcheck` runs it: pg_trgm's \copy
  # paths are relative to it.
  (cd "$src" && "$PGINST/lib/postgresql/pgxs/src/test/regress/pg_regress" \
    --inputdir="$src" --outputdir="$OUTDIR/contrib-$mod" --bindir="$PGINST/bin" \
    --dlpath="$PKGLIB" \
    --host=127.0.0.1 --port="$COMPUTE" --user="$osuser" --dbname="contrib_$mod" \
    $tests) || {
    status=$?
    # The results dir survives teardown but the workflow's artifact upload
    # globs *.log, so a diff is unreachable from CI unless printed here —
    # same reason extensions.sh cats its own.
    if [[ -s "$OUTDIR/contrib-$mod/regression.diffs" ]]; then
      echo ">>> regression.diffs (contrib/$mod):"
      cat "$OUTDIR/contrib-$mod/regression.diffs"
    fi
  }
done

echo
echo ">>> results under: $OUTDIR"
[[ -s "$OUTDIR/regression.diffs" ]] && echo ">>> diffs:        $OUTDIR/regression.diffs"
exit "$status" # teardown (or --keep) handled by the finish() EXIT trap
