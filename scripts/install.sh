#!/bin/sh
# Install or upgrade AgentK Local for the current user. AgentK Local itself needs no root; missing
# system tools are installed with sudo on Linux, only after showing the commands and asking.
#
# Internal repo (needs an authenticated `gh`):
#   gh release download --repo yugabyte/agent-k-local --pattern install.sh --output - | sh
#
# Options (pass through `sh -s -- ...` when piping):
#   --version <X.Y.Z>   install a specific version instead of the latest
#   --install-deps      install missing tools (git, curl, gh, a coding agent, and Meko's
#                       setup for the agents) without asking
#   --agent <claude|kiro>
#                       the coding agent to install when neither Claude Code nor Kiro CLI is
#                       there (default: ask; Claude Code with --install-deps or when there's no
#                       one to ask)
#   --no-install-deps   never install tools; stop and say what's missing instead
#   --deps-only         check (and install) the tools, then stop
#   --reinstall         download and set up again even though AgentK Local is already installed
#   anything else       forwarded to `agent-k-local install`
#                       (--non-interactive, --repos-root, --meko-key, --no-meko, --bind, --port,
#                       --no-service); Meko is optional: without a key the install goes on without it
#
# Missing tools are installed with Homebrew on macOS and the system package manager on Linux
# (with sudo), gh on Linux from its GitHub release (checksum-verified) into ~/.local/bin, and a
# coding agent when neither Claude Code nor Kiro CLI is installed: Claude Code with Anthropic's
# installer from claude.ai, or Kiro CLI with Kiro's from cli.kiro.dev, both over HTTPS (neither
# publishes a checksum for its script), into ~/.local/bin. The installer shows the exact commands and asks first; with
# --non-interactive it installs only with --install-deps. With GITHUB_TOKEN set, a missing gh
# is only installed with --install-deps; otherwise downloads use curl and the token. Afterwards
# it checks the tools themselves: Homebrew exiting with an error after they work is a warning.
#
# Run again where AgentK Local is already installed and set up (services included), it says so
# and stops without changing anything, unless given --reinstall, --deps-only, another --version
# or options for `agent-k-local install` (--non-interactive aside), which go on as before.
#
# Environment: AK_HOME (default ~/.agent-k-local), AK_REPO (default yugabyte/agent-k-local),
# GITHUB_TOKEN (used by the curl fallback when `gh` is unavailable).
set -eu

repo=${AK_REPO:-yugabyte/agent-k-local}
ak_home=${AK_HOME:-$HOME/.agent-k-local}
version=""
agent=""
install_deps=ask
deps_only=false
reinstall=false
setup_flags=false
interactive=true

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
ok() { printf '  %s%s%s %-13s %s%s%s\n' "$g" "$ok_mark" "$n" "$1" "$d" "${2:-}" "$n" >&2; }
warn() { printf '  %s%s%s %-13s %s\n' "$y" "$warn_mark" "$n" "$1" "${2:-}" >&2; }
die() { printf '\n  %s%s%s %s\n' "$r" "$fail_mark" "$n" "$*" >&2; exit 1; }

# ---- arguments ----
forward=""
while [ $# -gt 0 ]; do
  case $1 in
    --version) [ $# -ge 2 ] || die "--version needs a value"; version=${2#v}; shift 2 ;;
    --version=*) version=${1#--version=}; version=${version#v}; shift ;;
    --agent) [ $# -ge 2 ] || die "--agent needs a value: claude or kiro"; agent=$2; shift 2 ;;
    --agent=*) agent=${1#--agent=}; shift ;;
    --install-deps) install_deps=yes; shift ;;
    --no-install-deps) install_deps=no; shift ;;
    --deps-only) deps_only=true; shift ;;
    --reinstall) reinstall=true; shift ;;
    --non-interactive) interactive=false; forward="$forward '--non-interactive'"; shift ;;
    *) setup_flags=true; forward="$forward $(printf "'%s'" "$(printf '%s' "$1" | sed "s/'/'\\\\''/g")")"; shift ;;
  esac
done
case $agent in ''|claude|kiro) ;; *) die "--agent takes claude or kiro (got $agent)" ;; esac

# ---- platform ----
case $(uname -s) in
  Linux)
    platform=linux
    case $(uname -m) in
      x86_64|amd64) arch=x64 ;;
      aarch64|arm64) arch=arm64 ;;
      *) die "unsupported CPU architecture: $(uname -m)" ;;
    esac
    missing_hint=""
    ;;
  Darwin)
    platform=darwin
    # uname -m says x86_64 in a shell running under Rosetta; ask the hardware.
    [ "$(sysctl -n hw.optional.arm64 2>/dev/null)" = 1 ] || die "only Apple Silicon Macs are supported (this Mac has an Intel CPU)"
    arch=arm64
    missing_hint=" Releases from before macOS support have no macOS build."
    # Homebrew's tools may not be on PATH yet in a fresh shell. (AK_BREW_DIRS is for tests.)
    for brew_dir in ${AK_BREW_DIRS-/opt/homebrew/bin /usr/local/bin}; do
      case ":$PATH:" in *":$brew_dir:"*) ;; *) [ -x "$brew_dir/brew" ] && PATH="$brew_dir:$PATH" ;; esac
    done
    ;;
  *) die "only Linux and macOS are supported (found $(uname -s))" ;;
esac

# ---- prerequisites ----
# Questions go to the terminal: when piped into sh, stdin is this script.
{ [ -t 2 ] && (: </dev/tty) 2>/dev/null; } || interactive=false
ask() {
  printf '%s %s[Y/n]%s ' "$1" "$d" "$n" >&2
  read -r answer </dev/tty || answer=n
  case $answer in ''|[Yy]*) return 0 ;; *) return 1 ;; esac
}
need() { command -v "$1" >/dev/null 2>&1 || die "$1 is required but was not found on PATH. $2"; }
# The agents' installers, gh (as installed here) and agent-k-local itself live in ~/.local/bin,
# which a fresh shell may not have on PATH yet: look there too, so they aren't installed twice.
# The user's own PATH, to tell them later whether their shell will find agent-k-local.
user_path=$PATH
case ":$PATH:" in *":$HOME/.local/bin:"*) ;; *) PATH="$PATH:$HOME/.local/bin"; export PATH ;; esac
short_home=$(printf '%s' "$ak_home" | sed "s|^$HOME|~|")

# ---- already installed ----
# Set up all the way: a release linked as app/current and as ~/.local/bin/agent-k-local, a
# config.toml, and both background services, the last step of `agent-k-local install` (which
# `agent-k-local uninstall` removes). Versions before the session host had a tmux service in its
# place; their server adds the host service once it updates itself. Anything less, like an install that failed at its services
# or one made with --no-service, goes on to install again. So do options for `agent-k-local
# install`: they'd be dropped otherwise.
services_set_up() {
  if [ "$platform" = darwin ]; then
    launchctl print "gui/$(id -u)/com.yugabyte.agent-k-local" >/dev/null 2>&1 &&
      { launchctl print "gui/$(id -u)/com.yugabyte.agent-k-local.host" >/dev/null 2>&1 ||
        launchctl print "gui/$(id -u)/com.yugabyte.agent-k-local.tmux" >/dev/null 2>&1; }
  else
    systemctl --user is-enabled agent-k-local.service >/dev/null 2>&1 &&
      { systemctl --user is-enabled agent-k-local-host.service >/dev/null 2>&1 ||
        systemctl --user is-enabled agent-k-local-tmux.service >/dev/null 2>&1; }
  fi
}
if ! $reinstall && ! $deps_only && ! $setup_flags && [ -x "$ak_home/app/current/bin/agent-k-local" ] &&
  [ -f "$ak_home/config.toml" ] && [ -x "$HOME/.local/bin/agent-k-local" ] && services_set_up; then
  installed=$(readlink "$ak_home/app/current" 2>/dev/null) || installed=""
  installed=${installed##*/}
  if [ -z "$version" ] || [ "$version" = "$installed" ]; then
    # shellcheck disable=SC2088 # shown to the user, not run
    case ":$user_path:" in *":$HOME/.local/bin:"*) cmd=agent-k-local ;; *) cmd="~/.local/bin/agent-k-local" ;; esac
    row() { printf '    %s%-13s%s %s\n' "$d" "$1" "$n" "$2" >&2; }
    say ""
    say "${b}AgentK Local installer${n}"
    say ""
    ok "Installed" "${installed:-unknown version} in $short_home"
    say ""
    say "  AgentK Local is already installed, so nothing was changed."
    row "Status" "$cmd status"
    row "Setup" "$cmd install   (the repos folder, access, port, Meko, or the services again)"
    row "Update" "the Update banner or button in AgentK Local, when there's a newer release"
    row "Reinstall" "gh release download --repo $repo --pattern install.sh --output - | sh -s -- --reinstall"
    exit 0
  fi
fi

need tar ""
if command -v sha256sum >/dev/null 2>&1; then
  sha256_check() { sha256sum -c -; }
elif command -v shasum >/dev/null 2>&1; then
  sha256_check() { shasum -a 256 -c -; }
else
  die "sha256sum (coreutils) or shasum is required but neither was found on PATH."
fi

git_ok() {
  command -v git >/dev/null 2>&1 || return 1
  # macOS ships a git that only asks to install the Command Line Tools.
  [ "$platform" != darwin ] || xcode-select -p >/dev/null 2>&1 || [ -x "$(brew --prefix 2>/dev/null)/bin/git" ]
}

say ""
say "${b}AgentK Local installer${n}"
say ""

# What's missing, and the plan to install it: `plan` is shown to the user, `pkgs` goes to the
# package manager, `gh_download` fetches gh from its GitHub release (Linux).
missing="" pkgs="" plan="" gh_download=false claude_install=false kiro_install=false
add_missing() { missing="${missing:+$missing, }$1"; }
git_ok || { add_missing git; pkgs="$pkgs git"; }
command -v curl >/dev/null 2>&1 || { add_missing curl; pkgs="$pkgs curl"; }
gh_skipped=false
if ! command -v gh >/dev/null 2>&1; then
  # With GITHUB_TOKEN, downloads work through curl, so an unattended install can go on without
  # gh (AgentK Local then can't open PRs, follow reviews or update itself until it's installed).
  if [ -n "${GITHUB_TOKEN:-}" ] && [ "$install_deps" != yes ]; then gh_skipped=true
  else
    add_missing "gh (GitHub CLI)"
    if [ "$platform" = darwin ]; then pkgs="$pkgs gh"; else gh_download=true; fi
  fi
fi
# Sessions run on Claude Code or Kiro CLI: one of them is enough. With neither, one is installed:
# the one --agent names, else the one picked here, else Claude Code. --install-deps never asks.
if ! command -v claude >/dev/null 2>&1 && ! command -v kiro-cli >/dev/null 2>&1; then
  if [ -z "$agent" ] && $interactive && [ "$install_deps" = ask ]; then
    say "${b}Coding agent${n}"
    warn "Missing" "AgentK Local runs sessions on Claude Code or Kiro CLI, and neither is installed"
    printf '    Install Claude Code (1) or Kiro CLI (2)? %s[1]%s ' "$d" "$n" >&2
    read -r answer </dev/tty || answer=""
    case $answer in ''|1|[Cc]*) agent=claude ;; 2|[Kk]*) agent=kiro ;; *) die "pick 1 (Claude Code) or 2 (Kiro CLI), or pass --agent claude or --agent kiro." ;; esac
  fi
  if [ "$agent" = kiro ]; then
    add_missing "Kiro CLI"; kiro_install=true
    # Kiro's installer unpacks a zip on Linux.
    [ "$platform" = darwin ] || command -v unzip >/dev/null 2>&1 || { add_missing unzip; pkgs="$pkgs unzip"; }
  elif [ "$agent" = claude ]; then
    add_missing "Claude Code"; claude_install=true
  else
    add_missing "Claude Code (or Kiro CLI, with --agent kiro)"; claude_install=true
  fi
fi

sudo=""
if [ "$platform" = darwin ]; then
  if [ -n "$pkgs" ]; then
    command -v brew >/dev/null 2>&1 ||
      die "missing: $missing. Install Homebrew (https://brew.sh), then run this installer again; or install them yourself."
    plan="$plan
      brew install$pkgs"
  fi
else
  if [ -n "$pkgs" ]; then
    if [ "$(id -u)" = 0 ]; then sudo=""
    elif command -v sudo >/dev/null 2>&1; then
      # Without a terminal there's no one to type a password: fail rather than wait.
      if $interactive; then sudo="sudo"; else sudo="sudo -n"; fi
    else sudo="(as root)"
    fi
    pm=""
    for c in apt-get dnf yum zypper pacman apk; do
      command -v "$c" >/dev/null 2>&1 && { pm=$c; break; }
    done
    # How the commands are shown: without sudo they're for root to run.
    case $sudo in '(as root)'|'') s="" ;; *) s="$sudo " ;; esac
    case $pm in
      apt-get) plan="$plan
      ${s}apt-get update && ${s}env DEBIAN_FRONTEND=noninteractive apt-get install -y$pkgs" ;;
      dnf|yum) plan="$plan
      ${s}$pm install -y$pkgs" ;;
      zypper) plan="$plan
      ${s}zypper --non-interactive install$pkgs" ;;
      pacman) plan="$plan
      ${s}pacman -S --needed --noconfirm$pkgs" ;;
      apk) plan="$plan
      ${s}apk add$pkgs" ;;
      *) die "missing: $missing, and no supported package manager (apt-get, dnf, yum, zypper, pacman, apk) was found. Install them yourself and run this installer again." ;;
    esac
    [ "$sudo" = "(as root)" ] &&
      die "missing: $missing. Installing them needs root, and sudo isn't available. As root, run:$plan"
  fi
  $gh_download && plan="$plan
      download gh from github.com/cli/cli releases into ~/.local/bin (no root needed)"
fi
# The agents' own installers, the same on Linux and macOS: no root.
$claude_install && plan="$plan
      curl -fsSL https://claude.ai/install.sh | bash      (Claude Code, into ~/.local/bin)"
$kiro_install && plan="$plan
      curl -fsSL https://cli.kiro.dev/install | bash      (Kiro CLI, into ~/.local/bin)"

install_system_pkgs() {
  # shellcheck disable=SC2086 # intentional word splitting of package lists
  if [ "$platform" = darwin ]; then
    # It may have installed its tools even when it fails (see below).
    brew install $pkgs </dev/null
    return
  fi
  # shellcheck disable=SC2086 # intentional word splitting of the sudo prefix and package list
  case $pm in
    apt-get) $sudo apt-get update && $sudo env DEBIAN_FRONTEND=noninteractive apt-get install -y $pkgs ;;
    dnf|yum) $sudo "$pm" install -y $pkgs ;;
    zypper) $sudo zypper --non-interactive install $pkgs ;;
    pacman) $sudo pacman -S --needed --noconfirm $pkgs ;;
    apk) $sudo apk add $pkgs ;;
  esac </dev/null
}

# The first tool in $pkgs that still isn't there, if any.
first_missing() {
  for p in $pkgs; do
    case $p in
      git) git_ok ;;
      *) command -v "$p" >/dev/null 2>&1 ;;
    esac || { echo "$p"; return 0; }
  done
}

# gh from its own GitHub release (the distro's repos often lack it), checksum-verified.
install_gh() {
  case $arch in x64) gh_arch=amd64 ;; *) gh_arch=$arch ;; esac
  latest=$(curl -fsSLI -o /dev/null -w '%{url_effective}' https://github.com/cli/cli/releases/latest) || return 1
  gh_ver=${latest##*/v}
  [ -n "$gh_ver" ] && [ "$gh_ver" != "$latest" ] || return 1
  gh_tmp=$(mktemp -d)
  gh_name="gh_${gh_ver}_linux_${gh_arch}"
  gh_base="https://github.com/cli/cli/releases/download/v$gh_ver"
  curl -fsSL -o "$gh_tmp/$gh_name.tar.gz" "$gh_base/$gh_name.tar.gz" &&
    curl -fsSL -o "$gh_tmp/checksums.txt" "$gh_base/gh_${gh_ver}_checksums.txt" &&
    (cd "$gh_tmp" && grep " $gh_name.tar.gz\$" checksums.txt | sha256_check >/dev/null) &&
    tar -xzf "$gh_tmp/$gh_name.tar.gz" -C "$gh_tmp" &&
    mkdir -p "$HOME/.local/bin" &&
    cp "$gh_tmp/$gh_name/bin/gh" "$HOME/.local/bin/gh.tmp" &&
    chmod 755 "$HOME/.local/bin/gh.tmp" &&
    mv "$HOME/.local/bin/gh.tmp" "$HOME/.local/bin/gh"
  rc=$?
  rm -rf "$gh_tmp"
  return $rc
}

if [ -n "$plan" ]; then
  say "${b}Tools${n}"
  warn "Missing" "$missing"
  say "    To install them, the installer will run:$plan"
  case $install_deps in
    yes) ;;
    no) die "missing: $missing. Install them and run this installer again, or drop --no-install-deps." ;;
    ask)
      $interactive || die "missing: $missing. Rerun with --install-deps to install them, or install them yourself."
      ask "    Install them now?" || die "missing: $missing. Install them and run this installer again."
      ;;
  esac
  if [ -n "$pkgs" ]; then
    brew_failed=false
    if ! install_system_pkgs; then
      # On Linux a failed install can leave the package database half-configured: stop there.
      [ "$platform" = darwin ] || die "installing$pkgs failed (see the output above). Install them yourself and run this installer again."
      brew_failed=true
    fi
    hash -r 2>/dev/null || true
    # Check the result, not the package manager's word for it: Homebrew can exit with an error
    # after installing what it was asked for (on a macOS it no longer builds bottles for, it
    # builds from source, and a dependency's step can fail without stopping the tool asked for).
    bad=$(first_missing)
    if [ -n "$bad" ]; then
      problem="$bad is still not available after installing it."
      if $brew_failed; then
        die "brew failed (see the output above: its error can be well before its last lines). $problem Run 'brew install $bad' yourself to see what went wrong, then run this installer again."
      fi
      die "$problem"
    fi
    $brew_failed && warn "brew" "exited with an error, but$pkgs work: going on"
  fi
  if $gh_download; then
    install_gh || die "couldn't install gh from github.com/cli/cli. Install it yourself (https://cli.github.com) and run this installer again."
  fi
  if $claude_install; then
    command -v bash >/dev/null 2>&1 || die "Claude Code's installer needs bash, which wasn't found. Install Claude Code yourself: https://claude.com/claude-code"
    # Saved first rather than piped, so a failed download isn't mistaken for a finished install.
    cc_tmp=$(mktemp -d)
    { curl -fsSL -o "$cc_tmp/install.sh" https://claude.ai/install.sh && bash "$cc_tmp/install.sh" </dev/null; } ||
      { rm -rf "$cc_tmp"; die "Claude Code's installer failed (see the output above). Install it yourself: https://claude.com/claude-code"; }
    rm -rf "$cc_tmp"
  fi
  if $kiro_install; then
    command -v bash >/dev/null 2>&1 || die "Kiro CLI's installer needs bash, which wasn't found. Install Kiro CLI yourself: https://kiro.dev/cli/"
    # Saved first rather than piped, so a failed download isn't mistaken for a finished install.
    kiro_tmp=$(mktemp -d)
    { curl -fsSL -o "$kiro_tmp/install.sh" https://cli.kiro.dev/install && bash "$kiro_tmp/install.sh" </dev/null; } ||
      { rm -rf "$kiro_tmp"; die "Kiro CLI's installer failed (see the output above). Install it yourself: https://kiro.dev/cli/"; }
    rm -rf "$kiro_tmp"
  fi
  # gh and the agents land here; `agent-k-local install` (run below) needs them too.
  case ":$PATH:" in *":$HOME/.local/bin:"*) ;; *) PATH="$HOME/.local/bin:$PATH"; export PATH ;; esac
  hash -r 2>/dev/null || true
  # (Homebrew's gh was checked with the other packages.)
  ! $gh_download || command -v gh >/dev/null 2>&1 || die "gh is still not available after installing it."
  ! $claude_install || command -v claude >/dev/null 2>&1 || die "Claude Code is still not available after installing it."
  ! $kiro_install || command -v kiro-cli >/dev/null 2>&1 || die "Kiro CLI is still not available after installing it."
  ok "Installed" "$missing"
fi

# Each agent there has to be signed in before AgentK Local can run sessions on it.
agents=""
if command -v claude >/dev/null 2>&1; then
  agents="Claude Code"
  if ! claude auth status --json 2>/dev/null | grep -q '"loggedIn": *true'; then
    if $interactive && ask "  Claude Code isn't signed in. Sign in now (claude auth login)?"; then
      claude auth login </dev/tty || warn "Claude Code" "sign-in didn't finish; run 'claude auth login' later"
    else
      warn "Claude Code" "not signed in: run 'claude auth login' before starting sessions"
    fi
  fi
fi
if command -v kiro-cli >/dev/null 2>&1; then
  agents="${agents:+$agents, }Kiro"
  if ! kiro-cli whoami >/dev/null 2>&1; then
    if $interactive && ask "  Kiro isn't signed in. Sign in now (kiro-cli login)?"; then
      kiro-cli login </dev/tty || warn "Kiro" "sign-in didn't finish; run 'kiro-cli login' later"
    else
      warn "Kiro" "not signed in: run 'kiro-cli login' before starting sessions"
    fi
  fi
fi

# gh signs in to GitHub for downloads (the repo is internal), and for PRs, reviews and updates.
if $gh_skipped; then
  warn "GitHub" "gh isn't installed: downloading with GITHUB_TOKEN; install gh for PRs, reviews and updates"
elif ! gh auth status >/dev/null 2>&1; then
  if $interactive && ask "  gh isn't signed in to GitHub. AgentK Local needs it to download releases and for PRs, reviews and updates. Sign in now (gh auth login)?"; then
    gh auth login </dev/tty || warn "GitHub" "sign-in didn't finish; run 'gh auth login' later"
  else
    warn "GitHub" "gh isn't signed in: run 'gh auth login' (needed for PRs, reviews and updates)"
  fi
fi

if $deps_only; then
  ok "Tools" "git, curl, gh, $agents"
  exit 0
fi

have_gh=false
if command -v gh >/dev/null 2>&1 && gh auth status >/dev/null 2>&1; then have_gh=true; fi

say "${b}Download${n}"
ok "Prerequisites" "git, curl, gh, $agents"

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
ok "Installed" "$short_home/app/$version"
case ":$user_path:" in
  *":$HOME/.local/bin:"*) ok "Command" "agent-k-local (in ~/.local/bin)" ;;
  *) warn "Command" "add ~/.local/bin to your PATH to run 'agent-k-local' (and tools installed there) directly" ;;
esac

# Releases from before --install-deps existed would refuse the flag: pass it only to one that has it.
if grep -q 'install-deps' "$dest/lib/dist/server/install.js" 2>/dev/null; then
  case $install_deps in
    yes) forward="$forward '--install-deps'" ;;
    no) forward="$forward '--no-install-deps'" ;;
  esac
fi

export AK_HOME="$ak_home"
# Tells `agent-k-local install` the heading is already printed.
export AK_INSTALLER=1
# When piped into sh, stdin is the script itself: give the setup prompts the terminal.
if [ -r /dev/tty ] && [ -t 2 ]; then
  eval "exec \"\$ak_home/app/current/bin/agent-k-local\" install $forward" </dev/tty
else
  eval "exec \"\$ak_home/app/current/bin/agent-k-local\" install $forward"
fi
