#!/bin/sh
# Downloads the Node builds a release bundles from nodejs.org, checks them against SHASUMS256.txt,
# unpacks each into <dir> and prints each unpacked folder, <dir>/node-v<version>-<platform>, one a
# line. Kode's, Meko's and Presenter's release workflows use it.
#
#   scripts/release/fetch-node.sh <version> <dir> <linux|darwin>-<arch>...
#
# Every download retries: nodejs.org has stalled and broken HTTP/2 streams mid-release. A file gets
# 3 tries, 10s apart. A try ends after 20s without a connection, 30s under 256 kB/s, or 60s in all
# (nodejs.org normally serves a tarball in seconds), so a file gives up within 200s. The release
# steps' timeouts leave room for that for every file. The loop, not curl's --retry, does the
# retrying: AlmaLinux 8's curl (7.61), which the Linux builds use, has no --retry-all-errors, and
# its --retry leaves a reset connection or a broken HTTP/2 stream alone.
set -eu
[ $# -ge 3 ] || { echo "usage: fetch-node.sh <version> <dir> <linux|darwin>-<arch>..." >&2; exit 2; }
version=$1
dir=$2
shift 2
for platform; do
  case $platform in
    linux-* | darwin-*) ;;
    *) echo "fetch-node.sh: unknown platform $platform" >&2; exit 2 ;;
  esac
done

fetch() {
  try=1
  until curl -fsSLO --connect-timeout 20 --speed-limit 262144 --speed-time 30 --max-time 60 \
    "https://nodejs.org/dist/v${version}/$1"; do
    echo "Downloading $1 failed (try $try of 3)" >&2
    [ "$try" -lt 3 ] || return 1
    try=$((try + 1))
    sleep 10
  done
}

mkdir -p "$dir"
cd "$dir"
fetch SHASUMS256.txt
for platform; do
  case $platform in
    linux-*) file="node-v${version}-${platform}.tar.xz" ;;
    *) file="node-v${version}-${platform}.tar.gz" ;;
  esac
  fetch "$file"
  sum=$(grep " ${file}\$" SHASUMS256.txt) || { echo "${file} isn't in SHASUMS256.txt" >&2; exit 1; }
  if command -v sha256sum >/dev/null 2>&1; then
    echo "$sum" | sha256sum -c - >&2
  else
    echo "$sum" | shasum -a 256 -c - >&2
  fi
  case $file in
    *.tar.xz) tar -xJf "$file" ;;
    *) tar -xzf "$file" ;;
  esac
  echo "$PWD/node-v${version}-${platform}"
done
