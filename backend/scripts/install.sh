#!/bin/sh
# Install or update Meko for the current user; no root needed. It installs Meko alone: add AgentK
# afterwards with `meko extension install agentk`.
#
#   curl -fsSL https://mekodata.ai/install.sh | sh
#
# mekodata.ai/install.sh redirects to the latest release's install.sh, which is Meko's.
#
# Options (pass through `sh -s -- ...` when piping):
#   --version <X.Y.Z>   install that release instead of the newest
#   --reinstall         download and set up again even though that version is installed
#   --no-service        don't set up the login service (start Meko with meko start)
#   --non-interactive   ask nothing (it asks nothing today; accepted for scripts)
#
# It downloads meko-<version>-<platform>-<arch>.tar.gz and SHA256SUMS from the meko-v<version>
# release, checks the checksum, unpacks it into ~/.local/share/meko/app/<version> and runs its
# `meko install`, which points app/current and ~/.local/bin/meko at it and sets up the service.
#
# Environment: XDG_DATA_HOME (releases go in its meko/app, default ~/.local/share/meko/app; the data
# folder, ~/.meko or MEKO_HOME, is apart, so meko destroy never removes them), MEKO_REPO (default
# yugabyte/meko), GITHUB_TOKEN (used when gh isn't signed in; with neither it downloads anonymously,
# which needs a public repo).
set -eu

# Everything runs from main, called on the last line, so a download cut short runs nothing, and
# meko install's stdin isn't the rest of this script when it's piped to sh.
main() {

repo=${MEKO_REPO:-yugabyte/meko}
app="${XDG_DATA_HOME:-$HOME/.local/share}/meko/app"
version=""
reinstall=false
no_service=false

if [ -t 2 ] && [ -z "${NO_COLOR:-}" ] && [ "${TERM:-}" != dumb ]; then
  b=$(printf '\033[1m'); d=$(printf '\033[2m'); g=$(printf '\033[32m'); r=$(printf '\033[31m'); n=$(printf '\033[0m')
else
  b=''; d=''; g=''; r=''; n=''
fi
say() { printf '%s\n' "$*" >&2; }
ok() { printf '  %s✓%s %-11s %s%s%s\n' "$g" "$n" "$1" "$d" "${2:-}" "$n" >&2; }
die() { printf '\n  %s✗%s %s\n' "$r" "$n" "$*" >&2; exit 1; }

agentk_hint="To add AgentK, install Meko and then run: meko extension install agentk (pass its options after --, such as -- --agent kiro)"

while [ $# -gt 0 ]; do
  case $1 in
    --version) [ $# -ge 2 ] || die "--version needs a value"; version=${2#meko-v}; version=${version#v}; shift 2 ;;
    --version=*) version=${1#--version=}; version=${version#meko-v}; version=${version#v}; shift ;;
    --reinstall) reinstall=true; shift ;;
    --no-service) no_service=true; shift ;;
    --non-interactive) shift ;;
    -h|--help) awk 'NR > 1 && !/^#/ { exit } NR > 1 { sub(/^# ?/, ""); print }' "$0" >&2 2>/dev/null || true; exit 0 ;;
    *) die "unknown option $1. Meko's installer takes --version, --reinstall, --no-service and --non-interactive. $agentk_hint" ;;
  esac
done

# ---- platform ----
case ${MEKO_TEST_UNAME_S:-$(uname -s)} in
  Linux)
    platform=linux
    case ${MEKO_TEST_UNAME_M:-$(uname -m)} in
      x86_64|amd64) arch=x64 ;;
      aarch64|arm64) arch=arm64 ;;
      *) die "unsupported CPU architecture: $(uname -m)" ;;
    esac
    ;;
  Darwin)
    platform=darwin
    # uname -m says x86_64 in a shell running under Rosetta; ask the hardware.
    [ "${MEKO_TEST_ARM64:-$(sysctl -n hw.optional.arm64 2>/dev/null)}" = 1 ] ||
      die "only Apple Silicon Macs are supported (this Mac has an Intel CPU)"
    macos=${MEKO_TEST_MACOS:-$(sw_vers -productVersion 2>/dev/null)}
    [ "${macos%%.*}" -ge 14 ] 2>/dev/null || die "Meko needs macOS 14 or newer (this is macOS $macos)"
    arch=arm64
    ;;
  *) die "only Linux and macOS are supported (found $(uname -s))" ;;
esac

say ""
say "${b}Meko installer${n}"
say ""

command -v tar >/dev/null 2>&1 || die "tar is required but was not found on PATH"
if command -v sha256sum >/dev/null 2>&1; then
  sha256_check() { sha256sum -c -; }
elif command -v shasum >/dev/null 2>&1; then
  sha256_check() { shasum -a 256 -c -; }
else
  die "sha256sum (coreutils) or shasum is required but neither was found on PATH"
fi

# A signed-in gh, else curl with GITHUB_TOKEN, else curl with no token (a public repo).
have_gh=false
auth=""
if command -v gh >/dev/null 2>&1 && gh auth status >/dev/null 2>&1; then
  have_gh=true
else
  command -v curl >/dev/null 2>&1 || die "curl is required (or a signed-in gh)"
  [ -n "${GITHUB_TOKEN:-}" ] && auth="Authorization: Bearer $GITHUB_TOKEN"
fi
internal_hint="If $repo is still internal, sign in with gh auth login (or set GITHUB_TOKEN) and run this again"

# ---- already installed ----
installed=$(readlink "$app/current" 2>/dev/null) || installed=""
installed=${installed##*/}
if ! $reinstall && [ -n "$installed" ] && [ -x "$app/current/bin/meko" ] && { [ -z "$version" ] || [ "$version" = "$installed" ]; }; then
  ok "Installed" "meko $installed in $app"
  say ""
  say "  Meko is already installed, so nothing was changed. It updates itself; to set it up again:"
  say "    $app/current/bin/meko install     or this installer with --reinstall"
  exit 0
fi

# ---- resolve the version ----
if [ -z "$version" ]; then
  # The newest Meko release: its tags are meko-v<version>; the repo's other releases are AgentK's.
  if $have_gh; then
    tag=$(gh api "repos/$repo/releases?per_page=100" \
      --jq '[.[] | select((.draft or .prerelease) | not) | .tag_name | select(test("^meko-v[0-9]+\\.[0-9]+\\.[0-9]+$"))] | first // empty' 2>/dev/null) || tag=""
  elif [ -n "$auth" ]; then
    tag=$(curl -fsSL -H "$auth" "https://api.github.com/repos/$repo/releases?per_page=100" 2>/dev/null |
      sed -n 's/^ *"tag_name": *"\(meko-v[0-9][0-9]*\.[0-9][0-9]*\.[0-9][0-9]*\)".*/\1/p' | head -n 1) || tag=""
  else
    # Meko's releases are the repo's latest, and github.com/<repo>/releases/latest redirects to its
    # tag. That spends none of the API's 60 calls an hour a machine gets without a token.
    tag=$(curl -fsSLI -o /dev/null -w '%{url_effective}' "https://github.com/$repo/releases/latest" 2>/dev/null) || tag=""
    tag=${tag##*/}
    case $tag in meko-v[0-9]*.[0-9]*.[0-9]*) ;; *) tag="" ;; esac
  fi
  if [ -z "$tag" ]; then
    $have_gh || [ -n "$auth" ] || die "could not find a Meko release in $repo. $internal_hint"
    die "could not find a Meko release in $repo"
  fi
  version=${tag#meko-v}
fi
tag="meko-v$version"
tarball="meko-$version-$platform-$arch.tar.gz"

# ---- download and verify ----
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT INT TERM
if $have_gh; then
  gh release download "$tag" --repo "$repo" --pattern "$tarball" --pattern SHA256SUMS --dir "$tmp" ||
    die "download of $tarball from $repo's release $tag failed"
else
  base="https://github.com/$repo/releases/download/$tag"
  for f in "$tarball" SHA256SUMS; do
    if ! curl -fsSL ${auth:+-H "$auth"} -o "$tmp/$f" "$base/$f"; then
      [ -n "$auth" ] || die "download of $f failed. $internal_hint"
      die "download of $f failed"
    fi
  done
fi
(cd "$tmp" && grep " $tarball\$" SHA256SUMS | sha256_check >/dev/null) || die "checksum verification failed for $tarball"
ok "Release" "$version ($platform-$arch), checksum verified"

# ---- unpack into app/<version> ----
mkdir -p "$app"
staging="$app/.staging.$$"
rm -rf "$staging"
mkdir -p "$staging"
tar -xzf "$tmp/$tarball" -C "$staging"
[ -x "$staging/meko-$version/bin/meko" ] || die "unexpected tarball layout"
# A tarball downloaded in a browser would carry macOS's quarantine flag; gh and curl don't add it.
if [ "$platform" = darwin ]; then xattr -dr com.apple.quarantine "$staging" 2>/dev/null || true; fi
dest="$app/$version"
rm -rf "$dest.old"
[ -e "$dest" ] && mv "$dest" "$dest.old"
mv "$staging/meko-$version" "$dest"
rm -rf "$staging" "$dest.old"
ok "Unpacked" "$dest"

# ---- set up: app/current, the meko command, the client token and the service ----
set -- install
$no_service && set -- "$@" --no-service
exec "$dest/bin/meko" "$@"
}

main "$@"
