#!/bin/sh
# Assemble a self-contained release tarball.
#
#   scripts/package-release.sh <version> <arch> <node-dir> <out-dir>
#
# Expects `npm run build` and `npm prune --omit=dev` to have run in extensions/agentk, on the
# platform the tarball is for (node-pty is compiled natively). Produces
# <out-dir>/agent-k-local-<version>-<linux|darwin>-<arch>.tar.gz containing:
#
#   agent-k-local-<version>/
#     bin/agent-k-local        POSIX sh wrapper (packaging/agent-k-local)
#     node/bin/node            bundled Node runtime
#     node/LICENSE
#     lib/package.json
#     lib/dist/                compiled server + web assets, and AgentK's mod for Claude Code (dist/mod)
#     lib/node_modules/        production dependencies only
#     lib/packaging/systemd/   service templates used by `agent-k-local install` (Linux)
#     lib/packaging/launchd/   service templates used by `agent-k-local install` (macOS)
#     VERSION
#     CALL_HOME_URL            the usage stats collector, when AK_CALL_HOME_URL is set
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

case $(uname -s) in
  Linux) platform=linux ;;
  Darwin) platform=darwin ;;
  *) echo "unsupported platform: $(uname -s)" >&2; exit 2 ;;
esac

repo=$(cd "$(dirname "$0")/.." && pwd)
for p in "$repo/dist/server/cli.js" "$repo/dist/web/index.html" "$repo/dist/mod/agentk/hooks/register.ts" "$repo/node_modules" "$node_dir/bin/node"; do
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
cp -R "$repo/packaging/launchd" "$root/lib/packaging/launchd"
# The license covers the whole repo, so it sits at the root, above extensions/agentk.
cp "$repo/../../LICENSE" "$root/LICENSE"
printf '%s\n' "$version" > "$root/VERSION"
# Kept out of the repo: release builds get it from the CALL_HOME_URL repo variable.
if [ -n "${AK_CALL_HOME_URL:-}" ]; then
  printf '%s\n' "$AK_CALL_HOME_URL" > "$root/CALL_HOME_URL"
fi

# node-pty ships prebuilds for some platforms; we compile it ourselves.
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
if [ "$platform" = darwin ]; then
  # On macOS node-pty starts every process through spawn-helper; without the execute
  # bit each spawn fails with "posix_spawnp failed".
  helper="$root/lib/node_modules/node-pty/build/Release/spawn-helper"
  [ -f "$helper" ] || { echo "node-pty spawn-helper missing from build" >&2; exit 1; }
  chmod 755 "$helper"
fi

mkdir -p "$out_dir"
tarball="$out_dir/$name-$platform-$arch.tar.gz"
# COPYFILE_DISABLE keeps macOS tar from adding ._ metadata files.
COPYFILE_DISABLE=1 tar -C "$stage" -czf "$tarball" "$name"
echo "$tarball"
