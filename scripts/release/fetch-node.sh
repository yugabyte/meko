#!/bin/sh
# Downloads the Node build a release tarball bundles from nodejs.org, checks it against
# SHASUMS256.txt, unpacks it into <dir> and prints the unpacked folder,
# <dir>/node-v<version>-<platform>. AgentK's, Meko's and Presenter's release workflows use it.
#
#   scripts/release/fetch-node.sh <version> <linux|darwin>-<arch> <dir>
#
# Every download retries: nodejs.org has stalled and broken HTTP/2 streams mid-release. A transfer
# under 256 kB/s for 30s counts as stalled. AlmaLinux 8's curl (7.61), which the Linux builds use,
# has no --retry-all-errors, so the loop retries what --retry leaves alone, such as a reset
# connection or a broken HTTP/2 stream.
set -eu
[ $# -eq 3 ] || { echo "usage: fetch-node.sh <version> <linux|darwin>-<arch> <dir>" >&2; exit 2; }
version=$1
platform=$2
case $platform in
  linux-*) ext=tar.xz ;;
  darwin-*) ext=tar.gz ;;
  *) echo "fetch-node.sh: unknown platform $platform" >&2; exit 2 ;;
esac
dist="node-v${version}-${platform}"

fetch() {
  for try in 1 2 3; do
    curl -fsSLO --retry 3 --retry-connrefused --connect-timeout 20 --speed-limit 262144 --speed-time 30 \
      "https://nodejs.org/dist/v${version}/$1" && return 0
    echo "Downloading $1 failed (try $try of 3)" >&2
    sleep 10
  done
  return 1
}

mkdir -p "$3"
cd "$3"
fetch "${dist}.${ext}"
fetch SHASUMS256.txt
sum=$(grep " ${dist}.${ext}\$" SHASUMS256.txt) || { echo "${dist}.${ext} isn't in SHASUMS256.txt" >&2; exit 1; }
if command -v sha256sum >/dev/null 2>&1; then
  echo "$sum" | sha256sum -c - >&2
else
  echo "$sum" | shasum -a 256 -c - >&2
fi
case $ext in
  tar.xz) tar -xJf "${dist}.${ext}" ;;
  *) tar -xzf "${dist}.${ext}" ;;
esac
echo "$PWD/${dist}"
