#!/usr/bin/env bash
# Generate a yb-light release build + bundle package.
#
# Builds the release-mode binaries needed by the yb-pageserver deployment,
# stages them into a bundle directory under /tmp, and packages that into a
# versioned tarball (yb-light-bundle-<date>-<sha>.tar.gz) ready to ship.
set -euo pipefail

# protoc comes from `protoc-bin-vendored` via pageserver/page_api/build.rs;
# no PROTOC env required.

SHA=$(git rev-parse --short HEAD)
DATE=$(date -u +%Y-%m-%d)
# Single staging dir, reused across builds — `rm -rf` below wipes it each
# run so the dev box doesn't accumulate 1.4 GB per build. The output tarball
# is named per commit (date + short SHA) so a build host keeps per-build
# history. Override the staging dir via $BUNDLE_DIR, or the tarball path via
# $TARBALL.
BUNDLE=${BUNDLE_DIR:-/tmp/yb-light-bundle}
TARBALL=${TARBALL:-/tmp/yb-light-bundle-${DATE}-${SHA}.tar.gz}

echo ">>> Building release binaries (yb-pageserver + supporting bins)..."
cargo build --release \
    -p pageserver \
    -p control_plane \
    -p safekeeper \
    -p storage_controller \
    -p compute_tools \
    -p endpoint_storage

echo
echo ">>> Building compute extensions (pgxn/yb_amp/*.so for v17)..."
# `neon.so` + walredo/test/utils extensions are loaded by the compute's
# postgres at startup. The walproposer wire protocol with the
# safekeeper has to match these binaries; shipping the server binaries
# without a matching `pg_install/` is exactly the version-skew that
# breaks compute init on a wipe-restart. We always rebuild here even
# if pg_install/v17 looks fresh, because the C extensions link against
# the Rust walproposer-lib whose source rolls with every commit.
PG_INSTALL_CACHED=1 make neon-pg-ext-v17 >/dev/null

echo
echo ">>> Staging bundle at ${BUNDLE}"
rm -rf "${BUNDLE}"
mkdir -p "${BUNDLE}/lib" "${BUNDLE}/tools"

# Binaries actually launched by yb_light_local (see local_env.rs).
for bin in yb-pageserver yb_light_local yb-safekeeper storage_controller yb-compute-ctl yb-endpoint-storage; do
    cp -v "target/release/${bin}" "${BUNDLE}/${bin}"
done

# Operator-side diagnostic tools. All compiled pageserver bins (built
# above by `-p pageserver`, so they already exist in target/release/) that
# share the connection flags via `ysql_client::YbConnArgs` and work against
# both no-TLS and TLS clusters (`--sslmode`):
#   - yb-dump-pages          dump / decode pageserver.pages chains
#   - yb-dump-wals           dump / summarise public.wals chunks
#   - yb-reset-consistent-lsn  rewind timeline_state.consistent_lsn to a
#                              real WAL record boundary during replay-repair
for tool in yb-dump-pages yb-dump-wals yb-reset-consistent-lsn; do
    cp -v "target/release/${tool}" "${BUNDLE}/tools/${tool}"
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
