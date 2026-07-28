#!/usr/bin/env bash
#
# Fetch the prebuilt `libyb_thin_client` C++ shim tarball and print the
# extracted `lib/` directory (the value to set as `YB_THIN_CLIENT_LIB_DIR`
# for `cargo build`). The shim is a YugabyteDB build target published as a
# public, versioned, per-arch tarball on downloads.yugabyte.com — no
# credentials required, so CI and the Docker builds fetch it with plain
# `curl`.
#
# The version + commit are pinned in the crate so the fetched shim always
# matches the vendored ABI header:
#   compute/libs/yb_thin_client/shim/VERSION
#   compute/libs/yb_thin_client/shim/COMMIT
# (`build.rs` re-checks version.txt against the pinned version.)
#
# Usage:
#   export YB_THIN_CLIENT_LIB_DIR="$(scripts/fetch-yb-thin-client.sh)"
#   # or, in GitHub Actions:
#   echo "YB_THIN_CLIENT_LIB_DIR=$(scripts/fetch-yb-thin-client.sh)" >> "$GITHUB_ENV"
#
# Env overrides:
#   YB_THIN_CLIENT_DOWNLOAD_BASE  base URL (default: the public S3 path)
#   YB_THIN_CLIENT_CACHE_DIR      where to download/extract (default: $TMPDIR/yb_thin_client)
#
# All diagnostics go to stderr; the ONLY thing printed to stdout is the lib dir.
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
shim_dir="$repo_root/compute/libs/yb_thin_client/shim"
ver=$(tr -d '[:space:]' < "$shim_dir/VERSION")
commit=$(tr -d '[:space:]' < "$shim_dir/COMMIT")

case "$(uname -m)" in
  x86_64 | amd64) arch=x86_64 ;;
  aarch64 | arm64) arch=aarch64 ;;
  *) echo "fetch-yb-thin-client: unsupported arch $(uname -m)" >&2; exit 1 ;;
esac

base=${YB_THIN_CLIENT_DOWNLOAD_BASE:-https://s3.us-west-2.amazonaws.com/downloads.yugabyte.com/yb_thin_client}
tarball="yb_thin_client-${ver}-${commit}-release-linux-${arch}.tar.gz"
cache=${YB_THIN_CLIENT_CACHE_DIR:-"${TMPDIR:-/tmp}/yb_thin_client"}
mkdir -p "$cache"

if [ ! -s "$cache/$tarball" ]; then
  echo "fetch-yb-thin-client: downloading $base/$tarball" >&2
  curl -fSL --retry 3 --retry-delay 2 -o "$cache/$tarball.part" "$base/$tarball" >&2
  mv "$cache/$tarball.part" "$cache/$tarball"
fi

extract="$cache/${tarball%.tar.gz}"
rm -rf "$extract"
mkdir -p "$extract"
tar xzf "$cache/$tarball" -C "$extract"

# Locate the lib dir by finding the .so, so we don't depend on the tarball's
# top-level directory name.
so=$(find "$extract" -name libyb_thin_client.so -print -quit)
if [ -z "$so" ]; then
  echo "fetch-yb-thin-client: libyb_thin_client.so not found in $tarball" >&2
  exit 1
fi
dirname "$so"
