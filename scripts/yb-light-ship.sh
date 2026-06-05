#!/usr/bin/env bash
# yb-light release build + bundle staging.
#
# Builds the release-mode binaries needed by the yb-pageserver deployment,
# stages them into a versioned bundle directory under /tmp, and prints
# the rsync command for shipping to the deployment host.
set -euo pipefail

# protoc comes from `protoc-bin-vendored` via pageserver/page_api/build.rs;
# no PROTOC env required.

SHA=$(git rev-parse --short HEAD)
DATE=$(date -u +%Y-%m-%d)
# Single bundle dir, reused across builds — `rm -rf` below wipes it
# each run so the dev box doesn't accumulate 1.4 GB per build. The
# remote ship still names the destination dir per commit so AWS
# keeps the per-build history. Override via $BUNDLE_DIR if you need
# multiple local bundles side-by-side.
BUNDLE=${BUNDLE_DIR:-/tmp/yb-light-bundle}

echo ">>> Building release binaries (yb-pageserver + supporting bins)..."
cargo build --release \
    -p pageserver \
    -p control_plane \
    -p safekeeper \
    -p storage_controller \
    -p compute_tools \
    -p endpoint_storage

echo
echo ">>> Building compute extensions (pgxn/neon/*.so for v17)..."
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

# Binaries actually launched by neon_local (see local_env.rs).
for bin in yb-pageserver neon_local safekeeper storage_controller compute_ctl endpoint_storage; do
    cp -v "target/release/${bin}" "${BUNDLE}/${bin}"
done

# Operator-side diagnostic tools (live under tools/ in the repo).
# Currently the `yb-dump-*` helpers for inspecting the row-store
# tables during an incident; add to this list when new operator
# tools are introduced.
for tool in yb-dump-pages yb-dump-wals; do
    cp -v "tools/${tool}" "${BUNDLE}/tools/${tool}"
done

# Runtime libs not present on stock AlmaLinux/RHEL 9 (OpenSSL 1.1, ICU 60).
# See memory/bundle-runtime-libs.md for the background.
for lib in libssl.so.1.1 libcrypto.so.1.1 libicui18n.so.60 libicuuc.so.60 libicudata.so.60; do
    cp -v "/usr/lib64/${lib}" "${BUNDLE}/lib/${lib}"
done

# pg_install/v17/ — postgres binaries the compute spawns, plus the
# neon extension .so files. Without this, the compute on the target
# loads an older neon.so that's wire-incompatible with this bundle's
# safekeeper. Use rsync inside the script so we get incremental
# behavior locally (the tree is ~370 MB, mostly unchanging postgres).
echo
echo ">>> Staging pg_install/v17/ ($(du -sh pg_install/v17 | awk '{print $1}'))..."
mkdir -p "${BUNDLE}/pg_install"
rsync -a pg_install/v17 "${BUNDLE}/pg_install/"

cat > "${BUNDLE}/REVISION.txt" <<TXT
Build host: $(hostname)
Build date: $(date -u +"%Y-%m-%d %H:%M:%S UTC")
Commit:     $(git rev-parse HEAD)
Subject:    $(git log -1 --pretty=%s)

neon_local launches yb-pageserver (see local_env.rs::pageserver_bin).
TXT

echo
echo ">>> Bundle ready:"
ls -la "${BUNDLE}/"
echo
echo ">>> To ship to ec2-user@<host>:"
echo "    rsync -avzh -e 'ssh -i ~/.yugabyte/yb-15-aws-portal-1-key.pem' \\"
echo "        ${BUNDLE}/ ec2-user@<host>:~/yb-light/builds/${DATE}-${SHA}/"
