#!/bin/sh
# Build an offline bundle of an AgentK release, for people without access to the repo.
#
#   scripts/offline-bundle.sh <version> [out-dir]
#
# Needs an authenticated `gh`. Produces <out-dir>/agentk-<version>-offline.tar.gz containing:
#
#   agentk-<version>-offline/
#     install.sh                              this checkout's installer (it has --tarball)
#     install-offline.sh                      runs install.sh on the tarball for this computer
#     agentk-<version>-<platform>-<arch>.tar.gz   the release's tarballs, every platform
#     SHA256SUMS
#     LICENSE
set -eu

if [ $# -lt 1 ] || [ $# -gt 2 ]; then
  echo "usage: $0 <version> [out-dir]" >&2
  exit 2
fi
version=${1#agentk-v}
out_dir=${2:-.}
repo=${AK_REPO:-yugabyte/agent-k-local}
here=$(cd "$(dirname "$0")" && pwd)

name="agentk-$version-offline"
stage=$(mktemp -d)
trap 'rm -rf "$stage"' EXIT INT TERM
root="$stage/$name"
mkdir -p "$root"

gh release download "agentk-v$version" --repo "$repo" --dir "$root" \
  --pattern "agentk-$version-*.tar.gz" --pattern SHA256SUMS
# Only the tarballs in the bundle: drop the old-name copies' sums.
grep " agentk-$version-" "$root/SHA256SUMS" > "$root/SHA256SUMS.new"
mv "$root/SHA256SUMS.new" "$root/SHA256SUMS"
cp "$here/install.sh" "$root/install.sh"
cp "$here/../../../LICENSE" "$root/LICENSE"

cat > "$root/install-offline.sh" <<EOF
#!/bin/sh
# Install AgentK $version from this folder: no download of AgentK itself.
#   sh install-offline.sh [options for install.sh, e.g. --no-meko]
set -eu
dir=\$(cd "\$(dirname "\$0")" && pwd)
case \$(uname -s) in Linux) platform=linux ;; Darwin) platform=darwin ;; *) echo "unsupported OS" >&2; exit 1 ;; esac
case \$(uname -m) in x86_64|amd64) arch=x64 ;; *) arch=arm64 ;; esac
exec sh "\$dir/install.sh" --tarball "\$dir/agentk-$version-\$platform-\$arch.tar.gz" "\$@"
EOF
chmod 755 "$root/install.sh" "$root/install-offline.sh"

mkdir -p "$out_dir"
bundle="$out_dir/$name.tar.gz"
COPYFILE_DISABLE=1 tar -C "$stage" -czf "$bundle" "$name"
echo "$bundle"
