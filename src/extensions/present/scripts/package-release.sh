#!/bin/sh
# Assemble a self-contained release tarball, which Meko installs and updates (meko
# extension install present, meko extension update present).
#
#   scripts/package-release.sh <version> <linux|darwin> <x64|arm64> <node-dir> <out-dir>
#
# Expects `npm run build` and `npm prune --omit=dev` to have run in src/extensions/present.
# Present has no native modules, so one build packages every platform: only <node-dir>, an unpacked Node
# from nodejs.org for that platform, differs. Produces
# <out-dir>/present-<version>-<platform>-<arch>.tar.gz containing:
#
#   present-<version>/
#     bin/me_present           POSIX sh launcher (packaging/me_present): the program Meko runs
#                              (meko present <command>)
#     node/bin/me_present      bundled Node runtime, named so its process says it's Present's
#     node/bin/node            a link to it
#     node/LICENSE
#     lib/package.json
#     lib/dist/                compiled server + web pages
#     lib/node_modules/        production dependencies only
#     LICENSE
#     VERSION
set -eu

if [ $# -ne 5 ]; then
  echo "usage: $0 <version> <linux|darwin> <x64|arm64> <node-dir> <out-dir>" >&2
  exit 2
fi

version=$1
platform=$2
arch=$3
node_dir=$4
out_dir=$5

case $platform in
  linux|darwin) ;;
  *) echo "unsupported platform: $platform" >&2; exit 2 ;;
esac
case $arch in
  x64|arm64) ;;
  *) echo "unsupported arch: $arch" >&2; exit 2 ;;
esac

repo=$(cd "$(dirname "$0")/.." && pwd)
for p in "$repo/dist/server/cli.js" "$repo/dist/web/index.html" "$repo/node_modules" "$node_dir/bin/node"; do
  [ -e "$p" ] || { echo "missing $p (build first)" >&2; exit 1; }
done
# A dev dependency left in node_modules would ship: the web libraries are bundled into dist/web.
[ ! -d "$repo/node_modules/vite" ] || { echo "node_modules has dev dependencies: run npm prune --omit=dev first" >&2; exit 1; }

name="present-$version"
stage=$(mktemp -d)
trap 'rm -rf "$stage"' EXIT INT TERM
root="$stage/$name"

mkdir -p "$root/bin" "$root/node/bin" "$root/lib"
cp "$repo/packaging/me_present" "$root/bin/me_present"
chmod 755 "$root/bin/me_present"

cp "$node_dir/bin/node" "$root/node/bin/me_present"
ln -s me_present "$root/node/bin/node"
cp "$node_dir/LICENSE" "$root/node/LICENSE"

cp "$repo/package.json" "$root/lib/package.json"
cp -R "$repo/dist" "$root/lib/dist"
cp -R "$repo/node_modules" "$root/lib/node_modules"
# The license covers the whole repo, so it sits at the root, above src/extensions/present.
cp "$repo/../../../LICENSE" "$root/LICENSE"
printf '%s\n' "$version" > "$root/VERSION"

mkdir -p "$out_dir"
tarball="$out_dir/$name-$platform-$arch.tar.gz"
# COPYFILE_DISABLE keeps macOS tar from adding ._ metadata files.
COPYFILE_DISABLE=1 tar -C "$stage" -czf "$tarball" "$name"
echo "$tarball"
