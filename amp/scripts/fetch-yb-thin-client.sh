#!/usr/bin/env bash
#
# Ensure the prebuilt `libyb_thin_client` C++ shim is in the repo's shim cache
# and print the `lib/` directory holding it. The shim is a YugabyteDB build
# target published as a public, versioned, per-arch tarball on
# downloads.yugabyte.com — no credentials required, so CI and the Docker builds
# fetch it with plain `curl`. The tarball is verified against its pin in
# downloads_checksum.txt (scripts/safe_curl.sh) before it is unpacked.
#
# `yb_thin_client`'s build.rs resolves the shim through this same cache and runs
# this script only on a cache miss, so a build needs no environment setup at
# all. Callers that need the *runtime* path (LD_LIBRARY_PATH for the daemons)
# use this script's stdout.
#
# The version + commit are pinned in the crate so the fetched shim always
# matches the vendored ABI header:
#   compute/libs/yb_thin_client/shim/VERSION
#   compute/libs/yb_thin_client/shim/COMMIT
# (`build.rs` re-checks version.txt against the pinned version.)
#
# Idempotent by design: an already-extracted shim is reused with no download and
# no re-extraction, so repeated calls cost a couple of stats — but only one this
# script extracted from a VERIFIED tarball. Each extraction carries a `.verified`
# stamp, the tarball's pinned sha256, written only after the tarball matched its
# pin in downloads_checksum.txt. An extraction without a matching stamp (one made
# before the check existed, or a pin that moved) is discarded and rebuilt from a
# verified tarball. build.rs applies the same rule before it links a cache entry. The cache is keyed by
# version+commit+arch, so bumping the pin fetches alongside the old entry
# rather than overwriting it.
#
# Usage:
#   scripts/fetch-yb-thin-client.sh                       # prints the lib dir
#   export LD_LIBRARY_PATH="$(scripts/fetch-yb-thin-client.sh)"
#
# Env overrides:
#   YB_THIN_CLIENT_DOWNLOAD_BASE  base URL (default: the public S3 path).
#                                 For a mirror / air-gapped host. It cannot
#                                 select a different shim: the version and
#                                 commit always come from the pinned files,
#                                 and the tarball must match the checksum
#                                 pinned for the public URL, wherever it
#                                 was fetched from.
#
# All diagnostics go to stderr; the ONLY thing printed to stdout is the lib dir.
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
shim_dir="$repo_root/compute/libs/yb_thin_client/shim"
ver=$(tr -d '[:space:]' <"$shim_dir/VERSION")
commit=$(tr -d '[:space:]' <"$shim_dir/COMMIT")

case "$(uname -m)" in
x86_64 | amd64) arch=x86_64 ;;
aarch64 | arm64) arch=aarch64 ;;
*)
  echo "fetch-yb-thin-client: unsupported arch $(uname -m)" >&2
  exit 1
  ;;
esac

public_base=https://s3.us-west-2.amazonaws.com/downloads.yugabyte.com/yb_thin_client
base=${YB_THIN_CLIENT_DOWNLOAD_BASE:-$public_base}
tarball="yb_thin_client-${ver}-${commit}-release-linux-${arch}.tar.gz"
# Fixed, in-repo cache (gitignored). Deliberately NOT configurable: build.rs
# derives the same path, and a second knob is a second way for the two to
# disagree and link a shim that isn't the pinned one.
cache="$repo_root/.shim-cache"
extract="$cache/${tarball%.tar.gz}"

# Locate the .so by searching, so we don't depend on the tarball's top-level
# directory name. `|| true` because a cold cache means `$extract` does not exist
# and find exits non-zero, which `set -e` would otherwise treat as fatal.
find_so() { find "$extract" -name libyb_thin_client.so -print -quit 2>/dev/null || true; }

# The pin is keyed by the public URL, so a mirror has to serve the same bytes.
verify() { sh "$repo_root/scripts/safe_curl.sh" verify "$public_base/$tarball" "$1"; }
pin=$(sh "$repo_root/scripts/safe_curl.sh" lookup "$public_base/$tarball") || exit 1

so=$(find_so)
if [ -n "$so" ] && [ "$(cat "$extract/.verified" 2>/dev/null)" = "$pin" ]; then
  # Extracted from a tarball that matched this pin — reuse it untouched.
  dirname "$so"
  exit 0
fi
# Present but not stamped for this pin: nothing says it came from the file we
# pinned, so it is rebuilt from a verified tarball below.
[ -n "$so" ] && echo "fetch-yb-thin-client: $extract has no .verified stamp for the current pin; re-extracting" >&2

mkdir -p "$cache"
if [ ! -s "$cache/$tarball" ]; then
  echo "fetch-yb-thin-client: downloading $base/$tarball" >&2
  curl -fSL --retry 3 --retry-delay 2 -o "$cache/$tarball.part" "$base/$tarball" >&2
  verify "$cache/$tarball.part" || {
    rm -f "$cache/$tarball.part"
    exit 1
  }
  mv "$cache/$tarball.part" "$cache/$tarball"
else
  # A cached tarball is re-verified before it is extracted: a tarball is only
  # ever trusted by its checksum, not by where it came from.
  verify "$cache/$tarball" || {
    echo "fetch-yb-thin-client: $cache/$tarball does not match its pin; delete it to re-fetch" >&2
    exit 1
  }
fi

# Extract into a temp sibling and rename, so a killed run can't leave a
# half-unpacked tree that the reuse check above would then accept.
staging="$extract.unpack.$$"
rm -rf "$staging"
mkdir -p "$staging"
tar xzf "$cache/$tarball" -C "$staging"
# Written only here, after the tarball above matched its pin.
printf '%s\n' "$pin" >"$staging/.verified"
rm -rf "$extract"
mv "$staging" "$extract"

so=$(find_so)
if [ -z "$so" ]; then
  echo "fetch-yb-thin-client: libyb_thin_client.so not found in $tarball" >&2
  exit 1
fi
dirname "$so"
