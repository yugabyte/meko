#!/bin/sh
# Assemble a self-contained release tarball.
#
#   scripts/package-release.sh <version> <arch> <node-dir> <out-dir>
#
# Expects `npm run build` and `npm prune --omit=dev` to have run in extensions/agentk, on the
# platform the tarball is for (node-pty is compiled natively). Produces
# <out-dir>/agentk-<version>-<linux|darwin>-<arch>.tar.gz containing:
#
#   agentk-<version>/
#     bin/me_koder             POSIX sh wrapper (packaging/me_koder): the command
#     bin/agentk               a link to it: its name before, which services installed then, older
#                              Mekos and their registries run
#     bin/agent-k-local        a link to it: its name before the rename to AgentK, which older services start
#     node/bin/me_koder        bundled Node runtime, named so its processes say they're AgentK's
#     node/bin/node            a link to it, which the MCP configs AgentK writes for sessions name
#     node/LICENSE
#     lib/package.json
#     lib/dist/                compiled server + web assets, AgentK's mod for Claude Code (dist/mod), the session tools' skill (dist/tools) and the bundled workshops (dist/workshops)
#     lib/node_modules/        production dependencies only
#     lib/packaging/systemd/   service templates used by `me_koder install` (Linux)
#     lib/packaging/launchd/   service templates used by `me_koder install` (macOS)
#     lib/scripts/deps.sh      the prerequisites check `me_koder install` runs first (git, curl, gh, a coding agent)
#     VERSION
#
# and the same files as <out-dir>/agent-k-local-<version>-<linux|darwin>-<arch>.tar.gz, in
# agent-k-local-<version>/: the name and layout the updaters and installers from before the rename
# look for, so those installs still update.
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
for p in "$repo/dist/server/cli.js" "$repo/dist/server/mcpmain.js" "$repo/dist/web/index.html" "$repo/dist/mod/agentk/hooks/register.ts" "$repo/dist/tools/skill/SKILL.md" "$repo/dist/workshops/agentk-101/GUIDE.md" "$repo/node_modules" "$node_dir/bin/node"; do
  [ -e "$p" ] || { echo "missing $p (build first)" >&2; exit 1; }
done

name="agentk-$version"
stage=$(mktemp -d)
trap 'rm -rf "$stage"' EXIT INT TERM
root="$stage/$name"

mkdir -p "$root/bin" "$root/node/bin" "$root/lib/packaging" "$root/lib/scripts"
cp "$repo/packaging/me_koder" "$root/bin/me_koder"
chmod 755 "$root/bin/me_koder"
ln -s me_koder "$root/bin/agentk"
ln -s me_koder "$root/bin/agent-k-local"

cp "$node_dir/bin/node" "$root/node/bin/me_koder"
ln -s me_koder "$root/node/bin/node"
cp "$node_dir/LICENSE" "$root/node/LICENSE"

cp "$repo/package.json" "$root/lib/package.json"
cp -R "$repo/dist" "$root/lib/dist"
cp -R "$repo/node_modules" "$root/lib/node_modules"
cp -R "$repo/packaging/systemd" "$root/lib/packaging/systemd"
cp -R "$repo/packaging/launchd" "$root/lib/packaging/launchd"
cp "$repo/scripts/deps.sh" "$root/lib/scripts/deps.sh"
# The license covers the whole repo, so it sits at the root, above extensions/agentk.
cp "$repo/../../LICENSE" "$root/LICENSE"
printf '%s\n' "$version" > "$root/VERSION"

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
old="agent-k-local-$version"
mv "$root" "$stage/$old"
COPYFILE_DISABLE=1 tar -C "$stage" -czf "$out_dir/$old-$platform-$arch.tar.gz" "$old"
echo "$tarball"
