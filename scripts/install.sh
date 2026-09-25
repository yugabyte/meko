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

say() { printf 'agent-k-local: %s\n' "$*" >&2; }
die() { say "$*"; exit 1; }

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
[ "$(uname -s)" = Linux ] || die "only Linux is supported for now (found $(uname -s))"
case $(uname -m) in
  x86_64|amd64) arch=x64 ;;
  aarch64|arm64) arch=arm64 ;;
  *) die "unsupported CPU architecture: $(uname -m)" ;;
esac

# ---- prerequisites ----
need() { command -v "$1" >/dev/null 2>&1 || die "$1 is required but was not found on PATH. $2"; }
need tmux "Install tmux 2.7 or newer (e.g. 'sudo dnf install tmux')."
need git "Install git."
need claude "Install Claude Code: https://claude.com/claude-code"
need tar ""
need sha256sum "It is part of coreutils."

tmux_ver=$(tmux -V | sed -n 's/^tmux [^0-9]*\([0-9][0-9]*\)\.\([0-9][0-9]*\).*/\1 \2/p')
[ -n "$tmux_ver" ] || die "could not parse the tmux version from '$(tmux -V)'"
# shellcheck disable=SC2086 # intentional split into major/minor
set -- $tmux_ver
if [ "$1" -lt 2 ] || { [ "$1" -eq 2 ] && [ "$2" -lt 7 ]; }; then
  die "tmux 2.7 or newer is required (found $1.$2)"
fi

have_gh=false
if command -v gh >/dev/null 2>&1 && gh auth status >/dev/null 2>&1; then have_gh=true; fi

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
tarball="agent-k-local-$version-linux-$arch.tar.gz"
say "installing $version ($arch) into $ak_home"

# ---- download + verify ----
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT INT TERM

if $have_gh; then
  gh release download "$tag" --repo "$repo" --pattern "$tarball" --pattern SHA256SUMS --dir "$tmp" ||
    die "download of $tarball from $repo failed"
else
  base="https://github.com/$repo/releases/download/$tag"
  auth=""
  [ -n "${GITHUB_TOKEN:-}" ] && auth="Authorization: Bearer $GITHUB_TOKEN"
  for f in "$tarball" SHA256SUMS; do
    curl -fsSL ${auth:+-H "$auth"} -o "$tmp/$f" "$base/$f" ||
      die "download of $f failed. The repo is internal: install and log in to the GitHub CLI ('gh auth login') and try again."
  done
fi

(cd "$tmp" && grep " $tarball\$" SHA256SUMS | sha256sum -c - >/dev/null) ||
  die "checksum verification failed for $tarball"

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
mv -Tf "$ak_home/app/current.tmp" "$ak_home/app/current"

# A manual (re)install supersedes any pending self-update rollback.
rm -f "$ak_home/update-pending.json"

mkdir -p "$HOME/.local/bin"
ln -sfn "$ak_home/app/current/bin/agent-k-local" "$HOME/.local/bin/agent-k-local"
case ":$PATH:" in
  *":$HOME/.local/bin:"*) ;;
  *) say "add ~/.local/bin to your PATH to run 'agent-k-local' directly" ;;
esac

say "installed $version; running setup"
export AK_HOME="$ak_home"
# When piped into sh, stdin is the script itself: give the setup prompts the terminal.
if [ -r /dev/tty ] && [ -t 2 ]; then
  eval "exec \"\$ak_home/app/current/bin/agent-k-local\" install $forward" </dev/tty
else
  eval "exec \"\$ak_home/app/current/bin/agent-k-local\" install $forward"
fi
