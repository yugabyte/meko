#!/usr/bin/env bash
# Build yb-light binaries + compute extensions.
#
# Modes (pick one; default is debug):
#   (no flag)        debug build   (cargo dev profile -> target/debug)
#   --release        release build (-> target/release)
#   --release-tar    release build + package a shippable tarball
#                    (yb-light-bundle-<date>-<sha>.tar.gz under /tmp)
#
# In EVERY mode the vendor/postgres-v17 submodule is kept honest: if the
# gitlink recorded in HEAD differs from the commit checked out in the
# working tree (e.g. a rebase/pull bumped it), or PostgreSQL was never
# installed, the submodule is synced and PG is rebuilt from source. When the
# submodule already matches and pg_install/v17 exists, the cached PG install
# is reused (PG_INSTALL_CACHED=1) and only the C extensions are recompiled.
# This closes the footgun where a bumped gitlink would otherwise ship a
# stale Postgres against the cached install.
set -euo pipefail

# protoc comes from `protoc-bin-vendored` via pageserver/page_api/build.rs;
# no PROTOC env required.

usage() {
    sed -n '2,16p' "$0" | sed 's/^# \{0,1\}//'
    exit "${1:-0}"
}

MODE=debug
case "${1:-}" in
    "")            MODE=debug ;;
    --release)     MODE=release ;;
    --release-tar) MODE=release-tar ;;
    -h|--help)     usage 0 ;;
    *) echo "error: unknown argument '$1'" >&2; usage 2 ;;
esac

# Map mode -> cargo profile flag, make BUILD_TYPE, and the target subdir the
# resulting binaries land in. release-tar is a release build that also packages.
if [ "${MODE}" = debug ]; then
    BUILD_TYPE=debug
    CARGO_PROFILE_FLAGS=()          # cargo dev profile
    TARGET_DIR=target/debug
else
    BUILD_TYPE=release
    CARGO_PROFILE_FLAGS=(--release)
    TARGET_DIR=target/release
fi

echo ">>> Mode: ${MODE} (BUILD_TYPE=${BUILD_TYPE}, ${TARGET_DIR})"

# --- Keep the postgres submodule + install in sync ------------------------
# `git submodule status` prints a leading '+' when the checked-out commit
# differs from the gitlink; we compare the two SHAs directly so the decision
# is explicit. A missing pg_install/v17 also forces a build.
SUBMODULE=vendor/postgres-v17
want_pg=$(git rev-parse "HEAD:${SUBMODULE}")
have_pg=$(git -C "${SUBMODULE}" rev-parse HEAD 2>/dev/null || echo none)
REBUILD_PG=0
if [ "${want_pg}" != "${have_pg}" ]; then
    echo ">>> ${SUBMODULE} gitlink (${want_pg:0:12}) != working tree (${have_pg:0:12})"
    echo ">>> Syncing submodule and rebuilding PostgreSQL from source..."
    git submodule update --init --recursive -- "${SUBMODULE}"
    REBUILD_PG=1
elif [ ! -x pg_install/v17/bin/postgres ]; then
    echo ">>> pg_install/v17 missing — building PostgreSQL from source..."
    git submodule update --init --recursive -- "${SUBMODULE}"
    REBUILD_PG=1
else
    echo ">>> ${SUBMODULE} up to date (${have_pg:0:12}) — reusing cached pg_install/v17"
fi

echo
echo ">>> Building ${MODE} binaries (yb-pageserver + supporting bins)..."
cargo build "${CARGO_PROFILE_FLAGS[@]}" \
    -p pageserver \
    -p control_plane \
    -p safekeeper \
    -p storage_controller \
    -p compute_tools \
    -p endpoint_storage

echo
echo ">>> Building compute extensions (pgxn/yb_amp/*.so for v17)..."
# `neon.so` + walredo/test/utils extensions are loaded by the compute's
# postgres at startup. The walproposer wire protocol with the safekeeper has
# to match these binaries; shipping the server binaries without a matching
# `pg_install/` is exactly the version-skew that breaks compute init on a
# wipe-restart. We always rebuild the extensions here even if pg_install/v17
# looks fresh, because the C extensions link against the Rust walproposer-lib
# whose source rolls with every commit. When REBUILD_PG is set we also drop
# PG_INSTALL_CACHED so the `postgres-install-v17` prereq actually recompiles
# PostgreSQL from the just-synced submodule source.
if [ "${REBUILD_PG}" = 1 ]; then
    make BUILD_TYPE="${BUILD_TYPE}" neon-pg-ext-v17
else
    PG_INSTALL_CACHED=1 make BUILD_TYPE="${BUILD_TYPE}" neon-pg-ext-v17 >/dev/null
fi

if [ "${MODE}" != release-tar ]; then
    echo
    echo ">>> Build complete (${MODE}). Binaries in ${TARGET_DIR}/, extensions in pg_install/v17/."
    echo ">>> Re-run with --release-tar to package a shippable bundle."
    exit 0
fi

# --- release-tar: stage + package the shippable bundle --------------------
SHA=$(git rev-parse --short HEAD)
DATE=$(date -u +%Y-%m-%d)
# Single staging dir, reused across builds — `rm -rf` below wipes it each
# run so the dev box doesn't accumulate 1.4 GB per build. The output tarball
# is named per commit (date + short SHA) so a build host keeps per-build
# history. Override the staging dir via $BUNDLE_DIR, or the tarball path via
# $TARBALL.
BUNDLE=${BUNDLE_DIR:-/tmp/yb-light-bundle}
TARBALL=${TARBALL:-/tmp/yb-light-bundle-${DATE}-${SHA}.tar.gz}

echo
echo ">>> Staging bundle at ${BUNDLE}"
rm -rf "${BUNDLE}"
mkdir -p "${BUNDLE}/lib" "${BUNDLE}/tools"

# Binaries actually launched by yb_light_local (see local_env.rs).
for bin in yb-pageserver yb_light_local yb-safekeeper storage_controller yb-compute-ctl yb-endpoint-storage; do
    cp -v "${TARGET_DIR}/${bin}" "${BUNDLE}/${bin}"
done

# Operator-side diagnostic tools. All compiled pageserver bins (built
# above by `-p pageserver`, so they already exist in ${TARGET_DIR}/) that
# share the connection flags via `ysql_client::YbConnArgs` and work against
# both no-TLS and TLS clusters (`--sslmode`):
#   - yb-dump-pages          dump / decode pageserver.pages chains
#   - yb-dump-wals           dump / summarise public.wals chunks
#   - yb-reset-consistent-lsn  rewind timeline_state.consistent_lsn to a
#                              real WAL record boundary during replay-repair
for tool in yb-dump-pages yb-dump-wals yb-reset-consistent-lsn; do
    cp -v "${TARGET_DIR}/${tool}" "${BUNDLE}/tools/${tool}"
done

# Runtime libs not present on stock AlmaLinux/RHEL 9 (OpenSSL 1.1, ICU 60).
# See memory/bundle-runtime-libs.md for the background. Staged into the
# top-level lib/ for the daemons (yb_light_local passes LD_LIBRARY_PATH
# via the child-env allowlist) and, below, into pg_install/v17/lib/ for
# the walredo postgres child (the PS spawns it with env_clear() and
# LD_LIBRARY_PATH=pg_install/v17/lib only — see #33).
RUNTIME_LIBS=(libssl.so.1.1 libcrypto.so.1.1 libicui18n.so.60 libicuuc.so.60 libicudata.so.60)
for lib in "${RUNTIME_LIBS[@]}"; do
    cp -v "/usr/lib64/${lib}" "${BUNDLE}/lib/${lib}"
done

# pg_install/v17/ — postgres binaries the compute spawns, plus the
# neon extension .so files. Without this, the compute on the target
# loads an older neon.so that's wire-incompatible with this bundle's
# safekeeper. `cp -a` preserves the tree's perms/symlinks (the tree is
# ~370 MB, mostly unchanging postgres).
echo
echo ">>> Staging pg_install/v17/ ($(du -sh pg_install/v17 | awk '{print $1}'))..."
mkdir -p "${BUNDLE}/pg_install"
cp -a pg_install/v17 "${BUNDLE}/pg_install/"

# Same runtime libs into pg_install/v17/lib/ so the walredo postgres
# child can load them; its LD_LIBRARY_PATH is only this dir (#33).
for lib in "${RUNTIME_LIBS[@]}"; do
    cp -v "/usr/lib64/${lib}" "${BUNDLE}/pg_install/v17/lib/${lib}"
done

cat > "${BUNDLE}/REVISION.txt" <<TXT
Build host: $(hostname)
Build date: $(date -u +"%Y-%m-%d %H:%M:%S UTC")
Commit:     $(git rev-parse HEAD)
Subject:    $(git log -1 --pretty=%s)
PG commit:  $(git -C "${SUBMODULE}" rev-parse HEAD)

yb_light_local launches yb-pageserver (see local_env.rs::pageserver_bin).
TXT

echo
echo ">>> Bundle staged:"
ls -la "${BUNDLE}/"

# Package the staged bundle into a single versioned tarball — the shippable
# artifact. `-C "${BUNDLE}" .` so it unpacks into the destination dir without
# a leading bundle/ path component.
echo
echo ">>> Packaging tarball ${TARBALL} ..."
tar -czf "${TARBALL}" -C "${BUNDLE}" .

echo
echo ">>> Bundle tarball ready:"
ls -lh "${TARBALL}"
echo
echo ">>> Ship it to the deployment host, e.g.:"
echo "    scp ${TARBALL} <host>:~/yb-light/builds/"
echo "    ssh <host> 'mkdir -p ~/yb-light/builds/${DATE}-${SHA} \\"
echo "        && tar -xzf ~/yb-light/builds/$(basename "${TARBALL}") \\"
echo "             -C ~/yb-light/builds/${DATE}-${SHA}'"
