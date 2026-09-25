#!/bin/sh
# Assemble a self-contained release tarball.
#
#   scripts/package-release.sh <version> <arch> <node-dir> <out-dir>
#
# Expects `npm run build` and `npm prune --omit=dev` to have run in the repo root.
# Produces <out-dir>/agent-k-local-<version>-linux-<arch>.tar.gz containing:
#
#   agent-k-local-<version>/
#     bin/agent-k-local        POSIX sh wrapper (packaging/agent-k-local)
#     node/bin/node            bundled Node runtime
#     node/LICENSE
#     lib/package.json
#     lib/dist/                compiled server + web assets
#     lib/node_modules/        production dependencies only
#     lib/packaging/systemd/   unit templates used by `agent-k-local install`
#     VERSION
set -eu

if [ $# -ne 4 ]; then
  echo "usage: $0 <version> <arch> <node-dir> <out-dir>" >&2
  exit 2
fi

version=$1
arch=$2
node_dir=$3
out_dir=$4

case $arch in
  x64|arm64) ;;
  *) echo "unsupported arch: $arch" >&2; exit 2 ;;
esac

repo=$(cd "$(dirname "$0")/.." && pwd)
for p in "$repo/dist/server/cli.js" "$repo/dist/web/index.html" "$repo/node_modules" "$node_dir/bin/node"; do
  [ -e "$p" ] || { echo "missing $p (build first)" >&2; exit 1; }
done

name="agent-k-local-$version"
stage=$(mktemp -d)
trap 'rm -rf "$stage"' EXIT INT TERM
root="$stage/$name"

mkdir -p "$root/bin" "$root/node/bin" "$root/lib/packaging"
cp "$repo/packaging/agent-k-local" "$root/bin/agent-k-local"
chmod 755 "$root/bin/agent-k-local"

cp "$node_dir/bin/node" "$root/node/bin/node"
cp "$node_dir/LICENSE" "$root/node/LICENSE"

cp "$repo/package.json" "$root/lib/package.json"
cp -R "$repo/dist" "$root/lib/dist"
cp -R "$repo/node_modules" "$root/lib/node_modules"
cp -R "$repo/packaging/systemd" "$root/lib/packaging/systemd"
cp "$repo/LICENSE" "$root/LICENSE"
printf '%s\n' "$version" > "$root/VERSION"

# node-pty ships prebuilds for other platforms; we compile for Linux ourselves.
rm -rf "$root/lib/node_modules/node-pty/prebuilds"
# Build intermediates are not needed at runtime.
if [ -d "$root/lib/node_modules/node-pty/build" ]; then
  find "$root/lib/node_modules/node-pty/build" -mindepth 1 -maxdepth 1 ! -name Release -exec rm -rf {} +
  find "$root/lib/node_modules/node-pty/build/Release" -mindepth 1 \
    ! -name '*.node' ! -name 'spawn-helper' -exec rm -rf {} + 2>/dev/null || true
fi
[ -f "$root/lib/node_modules/node-pty/build/Release/pty.node" ] || {
  echo "node-pty native module missing from build" >&2
  exit 1
}

mkdir -p "$out_dir"
tarball="$out_dir/$name-linux-$arch.tar.gz"
tar -C "$stage" -czf "$tarball" "$name"
echo "$tarball"
