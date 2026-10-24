#!/bin/sh
# Pack an AgentK release into one file for people without access to the repo (it's internal).
#
#   scripts/offline-bundle.sh <version> [out-dir]
#
# Needs an authenticated `gh`. Produces <out-dir>/agentk-<version>-offline.tar.gz containing:
#
#   agentk-<version>-offline/
#     install.sh                                 the release's installer
#     install-offline.sh                         runs it with --tarball on this folder
#     agentk-<version>-<platform>-<arch>.tar.gz  the release's tarballs, one per platform
#     SHA256SUMS                                 their checksums
#     LICENSE
#
# Unpacked anywhere, `sh install-offline.sh [options for install.sh]` installs AgentK from the
# tarball for that computer without downloading it.
#
# Environment: AK_REPO (default yugabyte/agent-k-local).
set -eu

if [ $# -lt 1 ] || [ $# -gt 2 ]; then
  echo "usage: $0 <version> [out-dir]" >&2
  exit 2
fi
# A version, or its tag (agentk-v<version>, or v<version>), as install.sh takes it.
version=${1#agentk-v}
version=${version#v}
out_dir=${2:-.}
repo=${AK_REPO:-yugabyte/agent-k-local}
here=$(cd "$(dirname "$0")" && pwd)

name="agentk-$version-offline"
stage=$(mktemp -d)
trap 'rm -rf "$stage"' EXIT INT TERM
root="$stage/$name"
mkdir -p "$root"

gh release download "agentk-v$version" --repo "$repo" --dir "$root" \
  --pattern "agentk-$version-*.tar.gz" --pattern SHA256SUMS --pattern install.sh
grep -q -e '--tarball' "$root/install.sh" ||
  { echo "the installer of $version can't install from a tarball: bundle a newer release" >&2; exit 1; }
# Only the tarballs bundled: not the copies under the name from before the rename to AgentK.
grep " agentk-$version-" "$root/SHA256SUMS" > "$root/SHA256SUMS.new" ||
  { echo "SHA256SUMS of $version lists no agentk-$version-* tarball" >&2; exit 1; }
mv "$root/SHA256SUMS.new" "$root/SHA256SUMS"
# The license covers the whole repo, so it sits at the root, above extensions/agentk.
cp "$here/../../../LICENSE" "$root/LICENSE"

cat > "$root/install-offline.sh" <<'EOF'
#!/bin/sh
# Installs AgentK from the tarball in this folder for this computer, without downloading it.
#   sh install-offline.sh [options for install.sh, e.g. --repos-root ~/src]
set -eu
dir=$(cd "$(dirname "$0")" && pwd)
exec sh "$dir/install.sh" --tarball "$dir" "$@"
EOF
chmod 755 "$root/install.sh" "$root/install-offline.sh"

mkdir -p "$out_dir"
bundle="$out_dir/$name.tar.gz"
# COPYFILE_DISABLE keeps macOS tar from adding ._ metadata files.
COPYFILE_DISABLE=1 tar -C "$stage" -czf "$bundle" "$name"
echo "$bundle"
