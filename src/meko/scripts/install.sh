#!/bin/sh
# Install or upgrade Meko for the current user; no root needed. It installs Meko alone: a first
# install ends by offering the extensions (AgentK, Presenter), which meko extension install adds.
#
#   curl -fsSL https://mekodata.ai/install.sh | sh
#
# mekodata.ai/install.sh redirects to the latest release's install.sh, which is Meko's. Run again,
# it upgrades Meko to the newest release, or says it's up to date.
#
# Options (pass through `sh -s -- ...` when piping):
#   --version <X.Y.Z>   install that release instead of the newest
#   --reinstall         download and set up again even though that version is installed
#   --no-service        don't set up the login service (start Meko with meko start)
#   --non-interactive   ask nothing: no PATH question, no extension picker
#
# It downloads meko-<version>-<platform>-<arch>.tar.gz and SHA256SUMS from the meko-v<version>
# release with curl, no sign-in needed, checks the checksum, unpacks it into
# ~/.local/share/meko/app/<version> and runs its `meko install`, which points app/current and
# ~/.local/bin/meko at it, sets up the service and prints the rest of the run.
#
# Environment: XDG_DATA_HOME (releases go in its meko/app, default ~/.local/share/meko/app; the data
# folder, ~/.meko or MEKO_HOME, is apart, so meko destroy never removes them), MEKO_REPO (default
# yugabyte/meko), NO_COLOR (no colors).
set -eu

# Everything runs from main, called on the last line, so a download cut short runs nothing, and
# meko install's stdin isn't the rest of this script when it's piped to sh.
main() {

repo=${MEKO_REPO:-yugabyte/meko}
app="${XDG_DATA_HOME:-$HOME/.local/share}/meko/app"
version=""
reinstall=false
no_service=false
non_interactive=false
started=$(date +%s)

# The palette meko's rows use (internal/spin): base ANSI colors, which light and dark terminal
# themes each tune for their own background, and bold rather than white for emphasis.
if [ -t 2 ] && [ -z "${NO_COLOR:-}" ] && [ "${TERM:-}" != dumb ]; then
  tty=true
  b=$(printf '\033[1m'); d=$(printf '\033[2m'); g=$(printf '\033[32m'); r=$(printf '\033[31m')
  bl=$(printf '\033[34m'); n=$(printf '\033[0m')
else
  tty=false
  b=''; d=''; g=''; r=''; bl=''; n=''
fi
say() { printf '%s\n' "$*" >&2; }
# row <mark> <label> <text> [note]: "  ✓ Download        meko 1.2.3 · 121 MB in 6s · checksum verified"
row() { printf '  %s %-15s %s%s%s%s\n' "$1" "$2" "$3" "$d" "${4:+ · $4}" "$n" >&2; }
die() {
  [ -n "${spinning:-}" ] && printf '\r\033[K' >&2
  printf '  %s✗%s %s\n' "$r" "$n" "$*" >&2
  [ -z "${safe:-}" ] || printf '  %sNothing was left half-done: running the installer again is safe%s\n' "$d" "$n" >&2
  exit 1
}

agentk_hint="To add AgentK, install Meko and then run: meko extension install agentk (pass its options after --, such as -- --agent kiro)"

while [ $# -gt 0 ]; do
  case $1 in
    --version) [ $# -ge 2 ] || die "--version needs a value"; version=${2#meko-v}; version=${version#v}; shift 2 ;;
    --version=*) version=${1#--version=}; version=${version#meko-v}; version=${version#v}; shift ;;
    --reinstall) reinstall=true; shift ;;
    --no-service) no_service=true; shift ;;
    --non-interactive) non_interactive=true; shift ;;
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
    where="Linux · $arch"
    ;;
  Darwin)
    platform=darwin
    # uname -m says x86_64 in a shell running under Rosetta; ask the hardware.
    [ "${MEKO_TEST_ARM64:-$(sysctl -n hw.optional.arm64 2>/dev/null)}" = 1 ] ||
      die "only Apple Silicon Macs are supported (this Mac has an Intel CPU)"
    macos=${MEKO_TEST_MACOS:-$(sw_vers -productVersion 2>/dev/null)}
    [ "${macos%%.*}" -ge 14 ] 2>/dev/null || die "Meko needs macOS 14 or newer (this is macOS $macos)"
    arch=arm64
    where="macOS · Apple Silicon"
    ;;
  *) die "only Linux and macOS are supported (found $(uname -s))" ;;
esac

command -v tar >/dev/null 2>&1 || die "tar is required but was not found on PATH"
command -v curl >/dev/null 2>&1 || die "curl is required but was not found on PATH"
if command -v sha256sum >/dev/null 2>&1; then
  sha256_check() { sha256sum -c -; }
elif command -v shasum >/dev/null 2>&1; then
  sha256_check() { shasum -a 256 -c -; }
else
  die "sha256sum (coreutils) or shasum is required but neither was found on PATH"
fi

installed=$(readlink "$app/current" 2>/dev/null) || installed=""
installed=${installed##*/}
[ -x "$app/current/bin/meko" ] || installed=""

# ---- resolve the version ----
asked=$version
if [ -z "$version" ]; then
  # Meko's releases are the repo's latest, and github.com/<repo>/releases/latest redirects to its
  # tag (meko-v<version>). That spends none of the API's 60 calls an hour a machine gets.
  tag=$(curl -fsSLI -o /dev/null -w '%{url_effective}' "https://github.com/$repo/releases/latest" 2>/dev/null) || tag=""
  tag=${tag##*/}
  case $tag in meko-v[0-9]*.[0-9]*.[0-9]*) ;; *) die "could not find a Meko release in $repo" ;; esac
  version=${tag#meko-v}
fi

# ---- already installed: nothing to do ----
if ! $reinstall && [ "$installed" = "$version" ]; then
  if [ -n "$asked" ]; then
    say "  $g✓$n ${b}Meko $version is installed$n$d · --reinstall sets it up again$n"
  else
    say "  $g✓$n ${b}Meko $version is up to date$n$d · --reinstall sets it up again$n"
  fi
  exit 0
fi

# ---- header ----
say ""
if [ -n "$installed" ] && [ "$installed" != "$version" ]; then
  say "  $bl◆$n ${b}meko$n  $d$installed → $version · $where$n"
else
  say "  $bl◆$n ${b}meko$n  $d$version · $where$n"
  [ -n "$installed" ] || say "    ${d}Installs into your home folder, no sudo$n"
fi
say ""

tag="meko-v$version"
tarball="meko-$version-$platform-$arch.tar.gz"
base="https://github.com/$repo/releases/download/$tag"

# ---- download and verify ----
safe=1
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT INT TERM
began=$(date +%s)
if $tty; then
  # curl in the background, and the row's dots and the megabytes so far until it's done.
  curl -fsSL -o "$tmp/$tarball" "$base/$tarball" 2>"$tmp/curl.err" &
  pid=$!
  spinning=1
  i=0
  while kill -0 "$pid" 2>/dev/null; do
    case $((i % 8)) in
      0) f=⣾ ;; 1) f=⣽ ;; 2) f=⣻ ;; 3) f=⢿ ;; 4) f=⡿ ;; 5) f=⣟ ;; 6) f=⣯ ;; *) f=⣷ ;;
    esac
    size=$({ wc -c <"$tmp/$tarball"; } 2>/dev/null | tr -d ' ')
    printf '\r  %s%s%s %-15s meko %s %s· %s MB%s\033[K' "$bl" "$f" "$n" Download "$version" "$d" "$((${size:-0} / 1000000))" "$n" >&2
    i=$((i + 1))
    sleep 0.2 2>/dev/null || sleep 1
  done
  if ! wait "$pid"; then
    die "the download of $tarball failed: $(cat "$tmp/curl.err")"
  fi
  printf '\r\033[K' >&2
  spinning=""
else
  curl -fsSL -o "$tmp/$tarball" "$base/$tarball" || die "the download of $tarball failed"
fi
curl -fsSL -o "$tmp/SHA256SUMS" "$base/SHA256SUMS" || die "the download of SHA256SUMS failed"
(cd "$tmp" && grep " $tarball\$" SHA256SUMS | sha256_check >/dev/null) || die "checksum verification failed for $tarball"
size=$(wc -c <"$tmp/$tarball" | tr -d ' ')
row "$g✓$n" Download "meko $version" "$((size / 1000000)) MB in $(($(date +%s) - began))s · checksum verified"

# ---- unpack into app/<version> ----
mkdir -p "$app"
staging="$app/.staging.$$"
rm -rf "$staging"
mkdir -p "$staging"
tar -xzf "$tmp/$tarball" -C "$staging"
[ -x "$staging/meko-$version/bin/meko" ] || die "unexpected tarball layout"
# A tarball downloaded in a browser would carry macOS's quarantine flag; curl doesn't add it.
if [ "$platform" = darwin ]; then xattr -dr com.apple.quarantine "$staging" 2>/dev/null || true; fi
dest="$app/$version"
rm -rf "$dest.old"
[ -e "$dest" ] && mv "$dest" "$dest.old"
mv "$staging/meko-$version" "$dest"
rm -rf "$staging" "$dest.old"

# ---- set up: app/current, the meko command, the client token and the service ----
# meko install prints the rest of the run: when it began, the version it replaces, whether it's the
# first install (which ends with the extension picker), and whether it may ask anything.
MEKO_INSTALL_STARTED=$started
MEKO_INSTALL_FROM=""
MEKO_INSTALL_FIRST=""
MEKO_INSTALL_ASK=1
if [ -n "$installed" ] && [ "$installed" != "$version" ]; then MEKO_INSTALL_FROM=$installed; fi
[ -n "$installed" ] || MEKO_INSTALL_FIRST=1
! $non_interactive || MEKO_INSTALL_ASK=0
export MEKO_INSTALL_STARTED MEKO_INSTALL_FROM MEKO_INSTALL_FIRST MEKO_INSTALL_ASK
set -- install
$no_service && set -- "$@" --no-service
exec "$dest/bin/meko" "$@"
}

main "$@"
