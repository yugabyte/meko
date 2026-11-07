#!/bin/sh
# Assemble a self-contained Meko release tarball.
#
#   scripts/package-release.sh <version> <x64|arm64> <node-dir> <out-dir>
#
# Run in src/meko on the platform the tarball is for (meko is a cgo binary, and the context engine's
# onnxruntime and sharp are native), after:
#
#   make build VERSION=<version>          bin/meko
#   make context_engine                    context_engine/dist and its dependencies
#   (cd context_engine && npm prune --omit=dev)   production dependencies only
#
# It downloads the context engine's pinned models with the bundled Node (which also proves that
# Node, the context engine and onnxruntime run here). Produces
# <out-dir>/meko-<version>-<linux|darwin>-<arch>.tar.gz holding:
#
#   meko-<version>/
#     bin/meko                   the CLI and server
#     node/bin/node, node/LICENSE  Meko's own Node
#     context_engine/package.json, dist/, node_modules/, stubs/
#                                the context engine, trimmed to this platform
#     models/                    the pinned embedder and reranker, read offline (MEKO_MODELS_OFFLINE)
#     VERSION, LICENSE
set -eu

if [ $# -ne 4 ]; then
  echo "usage: $0 <version> <x64|arm64> <node-dir> <out-dir>" >&2
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

meko=$(cd "$(dirname "$0")/.." && pwd)
for p in "$meko/bin/meko" "$meko/context_engine/dist/main.js" "$meko/context_engine/node_modules" \
  "$meko/web_ui/dist/index.html" "$node_dir/bin/node"; do
  [ -e "$p" ] || { echo "missing $p (build first)" >&2; exit 1; }
done
[ -d "$meko/context_engine/node_modules/typescript" ] && { echo "context_engine/node_modules has dev dependencies: run npm prune --omit=dev" >&2; exit 1; }
v=$("$meko/bin/meko" version --json | sed -n 's/.*"version":"\([^"]*\)".*/\1/p')
[ "$v" = "$version" ] || { echo "bin/meko is version $v, not $version: make build VERSION=$version" >&2; exit 1; }

name="meko-$version"
stage=$(mktemp -d)
trap 'rm -rf "$stage"' EXIT INT TERM
root="$stage/$name"
mkdir -p "$root/bin" "$root/node/bin" "$root/context_engine" "$root/web_ui"

cp "$meko/bin/meko" "$root/bin/meko"
chmod 755 "$root/bin/meko"
cp "$node_dir/bin/node" "$root/node/bin/node"
cp "$node_dir/LICENSE" "$root/node/LICENSE"
cp "$meko/context_engine/package.json" "$root/context_engine/package.json"
cp -R "$meko/context_engine/dist" "$root/context_engine/dist"
cp -R "$meko/context_engine/node_modules" "$root/context_engine/node_modules"
# npm links file: dependencies (the stubs mem0ai imports but Meko never uses) into node_modules.
cp -R "$meko/context_engine/stubs" "$root/context_engine/stubs"
cp -R "$meko/web_ui/dist" "$root/web_ui/dist"
cp "$meko/../../LICENSE" "$root/LICENSE"
printf '%s\n' "$version" > "$root/VERSION"

# Trim the context engine's dependencies to this platform. onnxruntime-node ships every platform's runtime
# (and, on linux-x64, a 270 MB CUDA provider); onnxruntime-web is bundled into transformers.js's
# Node build already; sharp's WebAssembly and musl builds aren't loaded on macOS or glibc Linux.
nm="$root/context_engine/node_modules"
rm -rf "$nm/.bin" "$nm/onnxruntime-web" "$nm/@img/sharp-wasm32"
find "$nm/@img" -maxdepth 1 -name '*linuxmusl*' -exec rm -rf {} + 2>/dev/null || true
for napi in "$nm"/onnxruntime-node/bin/napi-v*; do
  [ -d "$napi" ] || continue
  for os_dir in "$napi"/*; do
    if [ "$(basename "$os_dir")" != "$platform" ]; then rm -rf "$os_dir"; continue; fi
    for arch_dir in "$os_dir"/*; do
      [ "$(basename "$arch_dir")" = "$arch" ] || rm -rf "$arch_dir"
    done
  done
done
rm -f "$nm"/onnxruntime-node/bin/napi-v*/*/*/libonnxruntime_providers_cuda* \
  "$nm"/onnxruntime-node/bin/napi-v*/*/*/libonnxruntime_providers_tensorrt*
ls "$nm"/onnxruntime-node/bin/napi-v*/"$platform"/"$arch"/onnxruntime_binding.node >/dev/null 2>&1 ||
  { echo "onnxruntime-node has no $platform/$arch runtime" >&2; exit 1; }

# The model, fetched by the bundled Node through the trimmed context engine: a check that they run here.
mkdir -p "$root/context_engine/scripts"
cp "$meko/context_engine/scripts/fetch-model.mjs" "$root/context_engine/scripts/"
"$root/node/bin/node" "$root/context_engine/scripts/fetch-model.mjs" "$root/models"
rm -rf "$root/context_engine/scripts"

mkdir -p "$out_dir"
tarball="$out_dir/$name-$platform-$arch.tar.gz"
# COPYFILE_DISABLE keeps macOS tar from adding ._ metadata files.
COPYFILE_DISABLE=1 tar -C "$stage" -czf "$tarball" "$name"
echo "$tarball"
