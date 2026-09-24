# shellcheck shell=bash
#
# Which PostgreSQL the integration suites run against. Sourced by run.sh and
# pg_regress.sh; not executable on its own. Requires $REPO (the amp tree root)
# and $PROFILE (debug|release), which each caller parses from its own flags.
#
# The type follows $PROFILE, so `--release` means here what it means to build.sh:
# release everything. The default, debug, gets an assertion-enabled PostgreSQL —
# these suites exist to catch corruption and lost-write bugs, and an Assert
# firing on the spot is the point. CI runs the default on every push, so that is
# where the assertions are guaranteed; `--release` is a developer opting into a
# faster run and is told what it got.
#
# Each build type has its own install and build tree (pg_install-debug/ and
# pg_install-release/), so neither can clobber the other. Before that split the
# type was decided by whichever consumer configured build/<v>/config.status
# first, and nothing reported which you got — so the suite quietly ran with
# assertions compiled out on developer machines (#1094).
#
# Layout for a debug run — the install dir and the build dir are SIBLINGS, only
# the version dir is nested:
#
#   <repo>/pg_install-debug        PG_TREE_INSTALL_DIR
#   <repo>/pg_install-debug/v18    PG_TREE_INSTALL_VERSION_DIR
#   <repo>/build-debug             PG_TREE_BUILD_DIR
#
# Sets, always:
#   PG_TREE_BUILD_TYPE           debug | release — the same knob build.sh takes
#   PG_TREE_INSTALL_DIR          the install dir for that type
#   PG_TREE_INSTALL_VERSION_DIR  its v18 subdir — what the compute and walredo run
#   PG_TREE_BUILD_DIR            PG build tree, extension objects, extension stamp
#
# Exports:
#   POSTGRES_DISTRIB_DIR  always, = PG_TREE_INSTALL_DIR. How the launcher,
#                      pageserver and compute find the install at runtime; also
#                      read by the harness's psql helper. They have no built-in
#                      default, so it is always needed.
#   POSTGRES_INSTALL_DIR  ONLY for release. Where postgres_ffi's build script
#                      looks for the server headers it generates bindings from.
#                      That script already defaults to the debug tree, and it
#                      declares cargo:rerun-if-env-changed on this variable, so
#                      exporting the same path anyway would flip set/unset
#                      against a bare `cargo build` and recompile postgres_ffi
#                      and its dependents on every alternation.
#
#                      Do NOT read POSTGRES_INSTALL_DIR to find the tree — it is
#                      empty on a debug run. PG_TREE_INSTALL_DIR is the one that
#                      is always set.
#
# Alternating build types recompiles postgres_ffi and its dependents either way,
# since the bindings follow the selected tree's headers. That is the accepted
# cost of one shared `target/`: splitting it per build type would double it, and
# CI's caches are per job so CI never alternates.

: "${PROFILE:?pg_tree.sh: PROFILE (debug|release) must be set before sourcing}"
PG_TREE_BUILD_TYPE=$PROFILE

# Ask make for the paths rather than re-deriving the `-release` suffix here, so
# they cannot disagree with where make installs.
# `env -u POSTGRES_INSTALL_DIR`: it is a Makefile override (`?=`), and an ambient
# one — a shell that exported it to point some other cluster at a release install
# — would otherwise redirect the suite onto a tree it does not own. Our own value
# is re-exported below, so every sub-make and cargo build agrees with it. BUILD_DIR
# needs no such care: the Makefile fixes it with `:=`, precisely so it cannot
# diverge from PG_TREE_BUILD_DIR here.
# shellcheck disable=SC2034  # PG_TREE_BUILD_DIR is read by the sourcing script
while IFS='=' read -r _key _val; do
    case $_key in
        POSTGRES_INSTALL_DIR) PG_TREE_INSTALL_DIR=$_val ;;
        BUILD_DIR) PG_TREE_BUILD_DIR=$_val ;;
    esac
done <<<"$(env -u POSTGRES_INSTALL_DIR \
    make -s -C "$REPO" print-pg-dirs "BUILD_TYPE=$PG_TREE_BUILD_TYPE")"
unset _key _val
[[ -n ${PG_TREE_INSTALL_DIR:-} && -n ${PG_TREE_BUILD_DIR:-} ]] || {
    echo "pg_tree.sh: 'make print-pg-dirs' did not report both paths" >&2
    exit 1
}
PG_TREE_INSTALL_VERSION_DIR="$PG_TREE_INSTALL_DIR/v18"
export POSTGRES_DISTRIB_DIR="$PG_TREE_INSTALL_DIR"
# Exported only when it differs from the path postgres_ffi's build script infers
# on its own, which is the debug tree — the same rule scripts/build.py follows.
# That script declares cargo:rerun-if-env-changed on the variable, so setting it
# to the path cargo would have used anyway is NOT a no-op: it flips set/unset
# against a bare `cargo build` or rust-analyzer and recompiles postgres_ffi and
# every dependent on each alternation. Unset rather than left alone, so an
# ambient value cannot point the build at a tree this run does not own.
if [[ $PG_TREE_BUILD_TYPE == debug ]]; then
    unset POSTGRES_INSTALL_DIR
else
    export POSTGRES_INSTALL_DIR="$PG_TREE_INSTALL_DIR"
fi

# Build it, or confirm the existing one is current. build.sh owns every
# PostgreSQL build — it is the only caller that runs configure, and it rebuilds
# only when the submodule commit drifted or the tree is the wrong type. Callers
# must therefore pass PG_INSTALL_CACHED=1 to their own `make`, or it would
# configure the tree itself and be back to deciding the build type by accident.
#
# `env -u PROFILE`: PostgreSQL's Makefile.global reads $PROFILE as profiling C
# flags, and ours holds debug|release (see the extension build below).
pg_tree_build() {
    local flags=(--pg-only)
    [[ $PG_TREE_BUILD_TYPE == release ]] && flags+=(--release)
    env -u PROFILE "$REPO/build.sh" "${flags[@]}"
}

# Rebuild the yb_amp extension into that tree when its C sources changed. cargo
# does NOT build it — the launcher spawns a compute whose postgres loads
# $PG_TREE_INSTALL_VERSION_DIR/lib/postgresql/yb_amp.so at runtime (the walproposer and
# pagestore smgr live in that .so). A stale .so silently runs an old compute
# against freshly-built cluster binaries; when the change is to the
# walproposer<->safekeeper wire or structs, that surfaces as a hang or a segfault
# that looks like a logic bug but is really a build skew.
#
# Staleness is measured against a stamp we `touch` after a successful build, NOT
# the installed .so, whose mtime `ninstall.sh -C` deliberately preserves when the
# rebuilt .so is byte-identical — which would fire the guard on every run. When it
# does fire, the extension build dir is wiped rather than rebuilt incrementally:
# insurance against a stale .o with an old struct layout reaching the .so, paid
# only when C actually changed (~1-2 min). The debug tree's PGXS does get header
# dependency tracking, since that install is configured --enable-depend, so there
# the wipe is belt-and-braces rather than the only thing that would catch it.
pg_tree_rebuild_ext_if_stale() {
    local ext_so="$PG_TREE_INSTALL_VERSION_DIR/lib/postgresql/yb_amp.so"
    local ext_stamp="$PG_TREE_BUILD_DIR/.yb_amp_ext.stamp"
    # The stamp records the cargo profile, not just the time: the .so statically
    # links libcommunicator.a, which follows $PROFILE. One build type per tree
    # makes that agree by construction — but POSTGRES_INSTALL_DIR can be
    # overridden to point both at one directory, and then a stale .so linked
    # against the other profile's library would look fresh.
    if [[ -f $ext_so && -f $ext_stamp ]] \
        && [[ "$(cat "$ext_stamp" 2>/dev/null)" == "$PROFILE" ]] \
        && [[ -z "$(find "$REPO/compute/pgxn" -type f \
                \( -name '*.c' -o -name '*.h' -o -name 'Makefile' \) \
                -newer "$ext_stamp" -print -quit)" ]]; then
        return 0
    fi
    echo ">>> yb_amp extension is stale (C sources changed or not yet built) — clean-rebuilding" >&2
    rm -rf "$PG_TREE_BUILD_DIR/pgxn-v18"
    # BUILD_TYPE="$PROFILE" so the Rust half of the extension (libcommunicator.a,
    # linked into the .so) follows the caller's profile like every other binary.
    # It is passed as a make variable while PROFILE itself is stripped from the
    # environment: PGXS reads $PROFILE as profiling C flags (Makefile.global:
    # `ifdef PROFILE -> CFLAGS += $(PROFILE)`), which would land as a bogus C flag.
    #
    # BUILD_TYPE also installs into the tree resolved above. PG_INSTALL_CACHED=1
    # keeps this make away from PostgreSQL itself: `yb-amp-pg-ext-v18` depends on
    # `postgres-install-v18`, so without it this step would configure the tree —
    # and so decide its build type — behind pg_tree_build's back, which is the
    # ambiguity #1094 was.
    env -u PROFILE PG_INSTALL_CACHED=1 make -C "$REPO" \
        "BUILD_TYPE=$PROFILE" yb-amp-pg-ext-v18
    mkdir -p "$PG_TREE_BUILD_DIR"
    printf '%s\n' "$PROFILE" > "$ext_stamp"
}

# Report which PostgreSQL the run actually got. Cheap half of the fix above: a
# regression of this kind goes from invisible to one line of output. A run that
# asked for debug and did not get it is refused rather than reported — that is
# the #1094 case, where leftover build state decided the type. `--release` asking
# for a release PostgreSQL is a choice, so it is stated and allowed.
pg_tree_report() {
    local configure
    configure=$("$PG_TREE_INSTALL_VERSION_DIR/bin/pg_config" --configure 2>/dev/null) || configure=""
    if [[ $configure == *--enable-cassert* ]]; then
        echo ">>> PostgreSQL: debug, assertions ON ($PG_TREE_INSTALL_VERSION_DIR)" >&2
        return 0
    fi
    if [[ $PG_TREE_BUILD_TYPE == release ]]; then
        echo ">>> PostgreSQL: release, assertions OFF — release profile ($PG_TREE_INSTALL_VERSION_DIR)" >&2
        return 0
    fi
    cat >&2 <<EOF
PostgreSQL at $PG_TREE_INSTALL_VERSION_DIR has assertions compiled out (or is not built),
but this run asked for the debug tree. It will not run blind. Remove
$PG_TREE_INSTALL_DIR and re-run to rebuild it.
EOF
    exit 1
}
