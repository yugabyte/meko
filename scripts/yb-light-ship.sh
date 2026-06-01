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
BUNDLE=${BUNDLE_DIR:-/tmp/yb-light-${DATE}-${SHA}}

echo ">>> Building release binaries (yb-pageserver + supporting bins)..."
cargo build --release \
    -p pageserver \
    -p control_plane \
    -p safekeeper \
    -p storage_broker \
    -p storage_controller \
    -p compute_tools \
    -p endpoint_storage

echo
echo ">>> Staging bundle at ${BUNDLE}"
rm -rf "${BUNDLE}"
mkdir -p "${BUNDLE}/lib"

# Binaries actually launched by neon_local (see local_env.rs).
for bin in yb-pageserver neon_local safekeeper storage_broker storage_controller compute_ctl endpoint_storage; do
    cp -v "target/release/${bin}" "${BUNDLE}/${bin}"
done

# Runtime libs not present on stock AlmaLinux/RHEL 9 (OpenSSL 1.1, ICU 60).
# See memory/bundle-runtime-libs.md for the background.
for lib in libssl.so.1.1 libcrypto.so.1.1 libicui18n.so.60 libicuuc.so.60 libicudata.so.60; do
    cp -v "/usr/lib64/${lib}" "${BUNDLE}/lib/${lib}"
done

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
echo "        ${BUNDLE}/ ec2-user@<host>:~/yb-light/builds/${DATE}-$(basename "${BUNDLE}" | sed "s/yb-light-${DATE}-//")/"
