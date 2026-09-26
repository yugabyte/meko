#!/bin/sh
# Install or upgrade agent-k-local for the current user (no root needed).
#
# Internal repo (needs an authenticated `gh`):
#   gh release download --repo yugabyte/agent-k-local --pattern install.sh --output - | sh
#
# Options (pass through `sh -s -- ...` when piping):
#   --version <X.Y.Z>   install a specific version instead of the latest
#   anything else       forwarded to `agent-k-local install`
#                       (--non-interactive, --repos-root, --meko-key, --bind, --port, --no-service)
#
# Environment: AK_HOME (default ~/.agent-k-local), AK_REPO (default yugabyte/agent-k-local),
# GITHUB_TOKEN (used by the curl fallback when `gh` is unavailable).
set -eu

repo=${AK_REPO:-yugabyte/agent-k-local}
ak_home=${AK_HOME:-$HOME/.agent-k-local}
version=""

# Output: color and marks only on a terminal, never with NO_COLOR.
if [ -t 2 ] && [ -z "${NO_COLOR:-}" ] && [ "${TERM:-}" != dumb ]; then
  b=$(printf '\033[1m'); d=$(printf '\033[2m'); g=$(printf '\033[32m'); y=$(printf '\033[33m'); r=$(printf '\033[31m'); n=$(printf '\033[0m')
else
  b=''; d=''; g=''; y=''; r=''; n=''
fi
case "${LC_ALL:-}${LC_CTYPE:-}${LANG:-}" in
  *[Uu][Tt][Ff]-8*|*[Uu][Tt][Ff]8*) ok_mark='✓'; warn_mark='!'; fail_mark='✗' ;;
  *) ok_mark='ok'; warn_mark='!!'; fail_mark='xx' ;;
esac
say() { printf '%s\n' "$*" >&2; }
ok() { printf '  %s%s%s %-14s%s%s%s\n' "$g" "$ok_mark" "$n" "$1" "$d" "${2:-}" "$n" >&2; }
warn() { printf '  %s%s%s %-14s%s\n' "$y" "$warn_mark" "$n" "$1" "${2:-}" >&2; }
die() { printf '\n  %s%s%s %s\n' "$r" "$fail_mark" "$n" "$*" >&2; exit 1; }

# ---- arguments ----
forward=""
while [ $# -gt 0 ]; do
  case $1 in
    --version) [ $# -ge 2 ] || die "--version needs a value"; version=${2#v}; shift 2 ;;
    --version=*) version=${1#--version=}; version=${version#v}; shift ;;
    *) forward="$forward $(printf "'%s'" "$(printf '%s' "$1" | sed "s/'/'\\\\''/g")")"; shift ;;
  esac
done

# ---- platform ----
case $(uname -s) in
  Linux)
    platform=linux
    case $(uname -m) in
      x86_64|amd64) arch=x64 ;;
      aarch64|arm64) arch=arm64 ;;
      *) die "unsupported CPU architecture: $(uname -m)" ;;
    esac
    tmux_min="2 7"
    pkg_hint="e.g. 'sudo dnf install tmux'"
    missing_hint=""
    ;;
  Darwin)
    platform=darwin
    # uname -m says x86_64 in a shell running under Rosetta; ask the hardware.
    [ "$(sysctl -n hw.optional.arm64 2>/dev/null)" = 1 ] || die "only Apple Silicon Macs are supported (this Mac has an Intel CPU)"
    arch=arm64
    # The launchd service runs tmux in the foreground (tmux -D), which needs 3.2.
    tmux_min="3 2"
    pkg_hint="e.g. 'brew install tmux'"
    missing_hint=" Releases from before macOS support have no macOS build."
    ;;
  *) die "only Linux and macOS are supported (found $(uname -s))" ;;
esac

# ---- prerequisites ----
need() { command -v "$1" >/dev/null 2>&1 || die "$1 is required but was not found on PATH. $2"; }
# shellcheck disable=SC2086 # intentional split into major/minor
set -- $tmux_min
min_major=$1 min_minor=$2
need tmux "Install tmux $min_major.$min_minor or newer ($pkg_hint)."
need git "Install git."
need claude "Install Claude Code: https://claude.com/claude-code"
need tar ""
if command -v sha256sum >/dev/null 2>&1; then
  sha256_check() { sha256sum -c -; }
elif command -v shasum >/dev/null 2>&1; then
  sha256_check() { shasum -a 256 -c -; }
else
  die "sha256sum (coreutils) or shasum is required but neither was found on PATH."
fi

tmux_ver=$(tmux -V | sed -n 's/^tmux [^0-9]*\([0-9][0-9]*\)\.\([0-9][0-9]*\).*/\1 \2/p')
[ -n "$tmux_ver" ] || die "could not parse the tmux version from '$(tmux -V)'"
# shellcheck disable=SC2086 # intentional split into major/minor
set -- $tmux_ver
if [ "$1" -lt "$min_major" ] || { [ "$1" -eq "$min_major" ] && [ "$2" -lt "$min_minor" ]; }; then
  die "tmux $min_major.$min_minor or newer is required (found $1.$2). Upgrade it ($pkg_hint)."
fi

have_gh=false
if command -v gh >/dev/null 2>&1 && gh auth status >/dev/null 2>&1; then have_gh=true; fi

say ""
say "${b}AgentK Local installer${n}"
say ""
say "${b}Download${n}"
ok "Prerequisites" "tmux $1.$2, git, claude$($have_gh && printf ', gh')"

# ---- resolve version ----
if [ -z "$version" ]; then
  if $have_gh; then
    tag=$(gh release view --repo "$repo" --json tagName --jq .tagName 2>/dev/null) || tag=""
  else
    auth=""
    [ -n "${GITHUB_TOKEN:-}" ] && auth="Authorization: Bearer $GITHUB_TOKEN"
    tag=$(curl -fsSL ${auth:+-H "$auth"} "https://api.github.com/repos/$repo/releases/latest" 2>/dev/null |
      sed -n 's/^ *"tag_name": *"\([^"]*\)".*/\1/p' | head -n 1) || tag=""
  fi
  [ -n "$tag" ] || die "could not find the latest release of $repo. The repo is internal: install and log in to the GitHub CLI ('gh auth login') and try again."
  version=${tag#v}
fi
tag="v$version"
tarball="agent-k-local-$version-$platform-$arch.tar.gz"

# ---- download + verify ----
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT INT TERM

if $have_gh; then
  gh release download "$tag" --repo "$repo" --pattern "$tarball" --pattern SHA256SUMS --dir "$tmp" ||
    die "download of $tarball from $repo failed.$missing_hint"
else
  base="https://github.com/$repo/releases/download/$tag"
  auth=""
  [ -n "${GITHUB_TOKEN:-}" ] && auth="Authorization: Bearer $GITHUB_TOKEN"
  for f in "$tarball" SHA256SUMS; do
    curl -fsSL ${auth:+-H "$auth"} -o "$tmp/$f" "$base/$f" ||
      die "download of $f failed. The repo is internal: install and log in to the GitHub CLI ('gh auth login') and try again."
  done
fi

(cd "$tmp" && grep " $tarball\$" SHA256SUMS | sha256_check >/dev/null) ||
  die "checksum verification failed for $tarball"
ok "Release" "$version ($platform-$arch), checksum verified"

# ---- unpack into app/<version> ----
mkdir -p "$ak_home/app"
staging="$ak_home/app/.staging.$$"
rm -rf "$staging"
mkdir -p "$staging"
tar -xzf "$tmp/$tarball" -C "$staging"
[ -x "$staging/agent-k-local-$version/bin/agent-k-local" ] || die "unexpected tarball layout"

dest="$ak_home/app/$version"
if [ -e "$dest" ]; then
  rm -rf "$dest.old"
  mv "$dest" "$dest.old"
fi
mv "$staging/agent-k-local-$version" "$dest"
rm -rf "$staging" "$dest.old"

ln -sfn "$version" "$ak_home/app/current.tmp"
# Rename over the old link atomically. `mv -T` is GNU-only, so use the bundled Node.
"$dest/node/bin/node" -e 'require("fs").renameSync(process.argv[1], process.argv[2])' \
  "$ak_home/app/current.tmp" "$ak_home/app/current"

# A manual (re)install supersedes any pending self-update rollback.
rm -f "$ak_home/update-pending.json"

mkdir -p "$HOME/.local/bin"
ln -sfn "$ak_home/app/current/bin/agent-k-local" "$HOME/.local/bin/agent-k-local"
short_home=$(printf '%s' "$ak_home" | sed "s|^$HOME|~|")
ok "Installed" "$short_home/app/$version"
case ":$PATH:" in
  *":$HOME/.local/bin:"*) ok "Command" "agent-k-local (in ~/.local/bin)" ;;
  *) warn "Command" "add ~/.local/bin to your PATH to run 'agent-k-local' directly" ;;
esac

export AK_HOME="$ak_home"
# Tells `agent-k-local install` the heading is already printed.
export AK_INSTALLER=1
# When piped into sh, stdin is the script itself: give the setup prompts the terminal.
if [ -r /dev/tty ] && [ -t 2 ]; then
  eval "exec \"\$ak_home/app/current/bin/agent-k-local\" install $forward" </dev/tty
else
  eval "exec \"\$ak_home/app/current/bin/agent-k-local\" install $forward"
fi
