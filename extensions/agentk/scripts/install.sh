#!/bin/sh
# Install or upgrade AgentK for the current user. AgentK itself needs no root; missing
# system tools are installed with sudo on Linux, only after showing the commands and asking.
#
# Internal repo (needs an authenticated `gh`). The repo's latest release is Meko's (whose
# `meko install_extension agentk` installs AgentK pointed at it), so AgentK alone comes
# from the newest AgentK release, tagged agentk-v<version> (v<version> before releases were tagged by product):
#   gh release download "$(gh release list --repo yugabyte/agent-k-local --exclude-drafts \
#     --exclude-pre-releases --limit 50 --json tagName \
#     --jq '[.[].tagName | select(test("^(agentk-v|v)[0-9]"))][0]')" \
#     --repo yugabyte/agent-k-local --pattern install.sh --output - | sh
#
# Options (pass through `sh -s -- ...` when piping):
#   --version <X.Y.Z>   install a specific version instead of the newest
#   --install-deps      install missing tools (git, curl, gh, a coding agent, and Meko's
#                       setup for the agents) without asking
#   --agent <claude|opencode|kiro>
#                       the coding agent to install when none of Claude Code, OpenCode and Kiro
#                       CLI is there (default: ask; Claude Code with --install-deps or when
#                       there's no one to ask)
#   --no-install-deps   never install tools; stop and say what's missing instead
#   --deps-only         check (and install) the tools, then stop
#   --reinstall         download and set up again even though AgentK is already installed
#   --print-home        print the folder AgentK is (or would be) installed in, and stop
#   anything else       forwarded to `agentk install`
#                       (--non-interactive, --repos-root, --meko-key, --meko-url, --bind, --port,
#                       --no-service); without a Meko Cloud key, AgentK uses the Meko running on
#                       this computer (Enter at the key prompt)
#
# Missing tools are installed with Homebrew on macOS and the system package manager on Linux
# (with sudo), gh on Linux from its GitHub release (checksum-verified) into ~/.local/bin, and a
# coding agent when none of Claude Code, OpenCode and Kiro CLI is installed: Claude Code with
# Anthropic's installer from claude.ai or Kiro CLI with Kiro's from cli.kiro.dev, into ~/.local/bin,
# or OpenCode with its own from opencode.ai, into ~/.opencode/bin, all over HTTPS (none publishes a
# checksum for its script). The installer shows the exact commands and asks first; with
# --non-interactive it installs only with --install-deps. With GITHUB_TOKEN set, a missing gh
# is only installed with --install-deps; otherwise downloads use curl and the token. Afterwards
# it checks the tools themselves: Homebrew exiting with an error after they work is a warning.
#
# Run again where AgentK is already installed and set up (services included), it says so
# and stops without changing anything, unless given --reinstall, --deps-only, another --version
# or options for `agentk install` (--non-interactive aside), which go on as before. An install from
# before the rename to AgentK (no ~/.local/bin/agentk) is installed again, which moves it to the new
# names.
#
# AgentK's home is in Meko's data folder, <data folder>/extensions/agentk, and nothing picks
# another: MEKO_HOME when it's set (Meko's `meko install_extension` sets it), else ~/.meko, or for a
# home folder on a network filesystem $XDG_STATE_HOME/meko when that's local, else
# /var/tmp/<user>/meko. An install from before the move keeps ~/.agent-k-local until its server
# moves it (src/server/movehome.ts).
#
# Environment: MEKO_HOME, AK_REPO (default yugabyte/agent-k-local), GITHUB_TOKEN (used by the
# curl fallback when `gh` is unavailable).
set -eu

repo=${AK_REPO:-yugabyte/agent-k-local}
version=""
agent=""
install_deps=ask
deps_only=false
reinstall=false
print_home=false
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
    --version) [ $# -ge 2 ] || die "--version needs a value"; version=$2; shift 2 ;;
    --version=*) version=${1#--version=}; shift ;;
    --agent) [ $# -ge 2 ] || die "--agent needs a value: claude, opencode or kiro"; agent=$2; shift 2 ;;
    --agent=*) agent=${1#--agent=}; shift ;;
    --install-deps) install_deps=yes; shift ;;
    --no-install-deps) install_deps=no; shift ;;
    --deps-only) deps_only=true; shift ;;
    --reinstall) reinstall=true; shift ;;
    --print-home) print_home=true; shift ;;
    --non-interactive) interactive=false; forward="$forward '--non-interactive'"; shift ;;
    *) setup_flags=true; forward="$forward $(printf "'%s'" "$(printf '%s' "$1" | sed "s/'/'\\\\''/g")")"; shift ;;
  esac
done
case $agent in ''|claude|opencode|kiro) ;; *) die "--agent takes claude, opencode or kiro (got $agent)" ;; esac
# A version, or its tag (agentk-v<version>, or v<version> from before).
version=${version#agentk-v}
version=${version#v}

# ---- where AgentK lives ----
# Meko's data folder by Meko's own rule (Resolve in backend/internal/paths/paths.go, and mekoHome in
# src/server/paths.ts; all three are tested against backend/internal/paths/testdata/resolve-cases.json).
fs_type() {
  if [ "$(uname -s)" = Darwin ]; then
    # BSD stat has no -c: find the mount with df, then read its type from mount.
    mp=$(df -P "$1" 2>/dev/null | tail -n 1 | sed -n 's/.*[0-9]%[[:space:]]*//p')
    [ -n "$mp" ] && mount 2>/dev/null | sed -n "s|.* on $mp (\([^,)]*\).*|\1|p" | head -n 1
  else
    stat -f -c %T "$1" 2>/dev/null
  fi
}
on_network() {
  dir=$1
  while [ ! -e "$dir" ] && [ "$dir" != / ]; do dir=$(dirname "$dir"); done
  case $(fs_type "$dir" || true) in
    nfs|nfs4|cifs|smb2|smbfs|fuse.sshfs|afs|lustre|gpfs|afpfs|webdav|macfuse|osxfuse) return 0 ;;
  esac
  return 1
}
meko_home() {
  if [ -n "${MEKO_HOME:-}" ]; then
    case $MEKO_HOME in /*) m=$MEKO_HOME ;; *) m=$(pwd)/$MEKO_HOME ;; esac
    if on_network "$m"; then
      die "Meko's data folder $m (MEKO_HOME) is on a network filesystem, which Meko refuses. Choose a folder on a local disk with MEKO_HOME."
    fi
    printf '%s\n' "$m"
  elif ! on_network "$HOME/.meko"; then
    printf '%s\n' "$HOME/.meko"
  elif [ -n "${XDG_STATE_HOME:-}" ] && case $XDG_STATE_HOME in /*) true ;; *) false ;; esac && ! on_network "$XDG_STATE_HOME/meko"; then
    printf '%s\n' "$XDG_STATE_HOME/meko"
  else
    printf '%s\n' "/var/tmp/$(id -un)/meko"
  fi
}
# An install already here is found by the command link this installer made (it may be in a home
# from before the move: ~/.agent-k-local, or one named by AK_HOME back then), so it's updated in
# place and moves by itself; else the home in Meko's data folder, or ~/.agent-k-local when only that
# is there.
installed_home() {
  for c in agentk agent-k-local; do
    l=$(readlink "$HOME/.local/bin/$c" 2>/dev/null) || continue
    case $l in */app/current/bin/*) h=${l%/app/current/bin/*}; [ -d "$h/app" ] && { printf '%s\n' "$h"; return 0; } ;; esac
  done
  return 1
}
meko_dir=$(meko_home) || exit 1
if ak_home=$(installed_home); then
  :
elif [ -d "$meko_dir/extensions/agentk" ] || [ ! -d "$HOME/.agent-k-local" ]; then
  ak_home=$meko_dir/extensions/agentk
else
  ak_home=$HOME/.agent-k-local
fi
if $print_home; then
  printf '%s\n' "$ak_home"
  exit 0
fi

# A folder of our own in a shared place like /var/tmp, where another user could have made it first:
# refused unless it and its parent are real folders of ours and others can't write in the parent.
private_dir() {
  mkdir -p "$1" || die "can't create $1"
  for dir in "$(dirname "$1")" "$1"; do
    [ -L "$dir" ] && die "$dir is a symlink. Remove it, or set MEKO_HOME to a local folder only you can write to."
    [ -n "$(find "$dir" -prune -user "$(id -u)")" ] || die "$dir is owned by another user. Remove it, or set MEKO_HOME to a local folder only you can write to."
  done
  if [ -n "$(find "$(dirname "$1")" -prune -perm -0002)" ]; then
    die "$(dirname "$1") can be written to by other users. Remove it, or set MEKO_HOME to a local folder only you can write to."
  fi
  chmod 700 "$1"
}

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
# The agents' installers, gh (as installed here) and agentk itself live in ~/.local/bin,
# which a fresh shell may not have on PATH yet: look there too, so they aren't installed twice.
# The user's own PATH, to tell them later whether their shell will find agentk.
user_path=$PATH
case ":$PATH:" in *":$HOME/.local/bin:"*) ;; *) PATH="$PATH:$HOME/.local/bin"; export PATH ;; esac
short_home=$(printf '%s' "$ak_home" | sed "s|^$HOME|~|")

# The AgentK-alone install, as a command to show: this script from the newest AgentK release
# (the repo's latest release is Meko's).
alone_cmd() {
  printf '%s' "gh release download \"\$(gh release list --repo $repo --exclude-drafts --exclude-pre-releases --limit 50 --json tagName --jq '[.[].tagName | select(test(\"^(agentk-v|v)[0-9]\"))][0]')\" --repo $repo --pattern install.sh --output - | sh"
}

# ---- already installed ----
# Set up all the way: a release linked as app/current and as ~/.local/bin/agentk, a
# config.toml, and both background services, the last step of `agentk install` (which
# `agentk uninstall` removes). Anything less, like an install that failed at its services
# or one made with --no-service, goes on to install again. So do options for `agentk
# install`: they'd be dropped otherwise. A service may still have its name from before the
# rename to AgentK (agent-k-local): `agentk install` moves the session host's only once it stops.
service_set_up() {
  if [ "$platform" = darwin ]; then
    launchctl print "gui/$(id -u)/com.yugabyte.$1" >/dev/null 2>&1 ||
      launchctl print "gui/$(id -u)/com.yugabyte.$2" >/dev/null 2>&1
  else
    systemctl --user is-enabled "$1" >/dev/null 2>&1 || systemctl --user is-enabled "$2" >/dev/null 2>&1
  fi
}
services_set_up() {
  if [ "$platform" = darwin ]; then
    service_set_up agentk agent-k-local && service_set_up agentk.host agent-k-local.host
  else
    service_set_up agentk.service agent-k-local.service && service_set_up agentk-host.service agent-k-local-host.service
  fi
}
if ! $reinstall && ! $deps_only && ! $setup_flags && [ -x "$ak_home/app/current/bin/agentk" ] &&
  [ -f "$ak_home/config.toml" ] && [ -x "$HOME/.local/bin/agentk" ] && services_set_up; then
  installed=$(readlink "$ak_home/app/current" 2>/dev/null) || installed=""
  installed=${installed##*/}
  if [ -z "$version" ] || [ "$version" = "$installed" ]; then
    # shellcheck disable=SC2088 # shown to the user, not run
    case ":$user_path:" in *":$HOME/.local/bin:"*) cmd=agentk ;; *) cmd="~/.local/bin/agentk" ;; esac
    row() { printf '    %s%-13s%s %s\n' "$d" "$1" "$n" "$2" >&2; }
    say ""
    say "${b}AgentK installer${n}"
    say ""
    ok "Installed" "${installed:-unknown version} in $short_home"
    say ""
    say "  AgentK is already installed, so nothing was changed."
    row "Status" "$cmd status"
    row "Setup" "$cmd install   (the repos folder, access, port, Meko, or the services again)"
    row "Update" "the Update banner or button in AgentK, when there's a newer release"
    row "Reinstall" "$(alone_cmd) -s -- --reinstall"
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
say "${b}AgentK installer${n}"
say ""

# What's missing, and the plan to install it: `plan` is shown to the user, `pkgs` goes to the
# package manager, `gh_download` fetches gh from its GitHub release (Linux).
missing="" pkgs="" plan="" gh_download=false claude_install=false opencode_install=false kiro_install=false
add_missing() { missing="${missing:+$missing, }$1"; }
git_ok || { add_missing git; pkgs="$pkgs git"; }
command -v curl >/dev/null 2>&1 || { add_missing curl; pkgs="$pkgs curl"; }
gh_skipped=false
if ! command -v gh >/dev/null 2>&1; then
  # With GITHUB_TOKEN, downloads work through curl, so an unattended install can go on without
  # gh (AgentK then can't open PRs, follow reviews or update itself until it's installed).
  if [ -n "${GITHUB_TOKEN:-}" ] && [ "$install_deps" != yes ]; then gh_skipped=true
  else
    add_missing "gh (GitHub CLI)"
    if [ "$platform" = darwin ]; then pkgs="$pkgs gh"; else gh_download=true; fi
  fi
fi
# OpenCode's installer puts it in ~/.opencode/bin, and adds that to a shell's rc file only.
opencode_bin="$HOME/.opencode/bin"
has_opencode() { command -v opencode >/dev/null 2>&1 || [ -x "$opencode_bin/opencode" ]; }
# Sessions run on Claude Code, OpenCode or Kiro CLI: one of them is enough. With none, one is
# installed: the one --agent names, else the one picked here, else Claude Code. --install-deps never asks.
if ! command -v claude >/dev/null 2>&1 && ! has_opencode && ! command -v kiro-cli >/dev/null 2>&1; then
  if [ -z "$agent" ] && $interactive && [ "$install_deps" = ask ]; then
    say "${b}Coding agent${n}"
    warn "Missing" "AgentK runs sessions on Claude Code, OpenCode or Kiro CLI, and none is installed"
    printf '    Install Claude Code (1), OpenCode (2) or Kiro CLI (3)? %s[1]%s ' "$d" "$n" >&2
    read -r answer </dev/tty || answer=""
    case $answer in
      ''|1|[Cc]*) agent=claude ;;
      2|[Oo]*) agent=opencode ;;
      3|[Kk]*) agent=kiro ;;
      *) die "pick 1 (Claude Code), 2 (OpenCode) or 3 (Kiro CLI), or pass --agent claude, --agent opencode or --agent kiro." ;;
    esac
  fi
  if [ "$agent" = opencode ]; then
    add_missing "OpenCode"; opencode_install=true
    # OpenCode's installer unpacks a tarball on Linux and a zip on macOS.
    if [ "$platform" = darwin ]; then command -v unzip >/dev/null 2>&1 || { add_missing unzip; pkgs="$pkgs unzip"; }
    else command -v tar >/dev/null 2>&1 || { add_missing tar; pkgs="$pkgs tar"; }
    fi
  elif [ "$agent" = kiro ]; then
    add_missing "Kiro CLI"; kiro_install=true
    # Kiro's installer unpacks a zip on Linux.
    [ "$platform" = darwin ] || command -v unzip >/dev/null 2>&1 || { add_missing unzip; pkgs="$pkgs unzip"; }
  elif [ "$agent" = claude ]; then
    add_missing "Claude Code"; claude_install=true
  else
    add_missing "Claude Code (or OpenCode or Kiro CLI, with --agent opencode or --agent kiro)"; claude_install=true
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
$opencode_install && plan="$plan
      curl -fsSL https://opencode.ai/install | bash      (OpenCode, into ~/.opencode/bin)"
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
  if $opencode_install; then
    command -v bash >/dev/null 2>&1 || die "OpenCode's installer needs bash, which wasn't found. Install OpenCode yourself: https://opencode.ai"
    # Saved first rather than piped, so a failed download isn't mistaken for a finished install.
    oc_tmp=$(mktemp -d)
    { curl -fsSL -o "$oc_tmp/install" https://opencode.ai/install && bash "$oc_tmp/install" </dev/null; } ||
      { rm -rf "$oc_tmp"; die "OpenCode's installer failed (see the output above). Install it yourself: https://opencode.ai"; }
    rm -rf "$oc_tmp"
  fi
  if $kiro_install; then
    command -v bash >/dev/null 2>&1 || die "Kiro CLI's installer needs bash, which wasn't found. Install Kiro CLI yourself: https://kiro.dev/cli/"
    # Saved first rather than piped, so a failed download isn't mistaken for a finished install.
    kiro_tmp=$(mktemp -d)
    { curl -fsSL -o "$kiro_tmp/install.sh" https://cli.kiro.dev/install && bash "$kiro_tmp/install.sh" </dev/null; } ||
      { rm -rf "$kiro_tmp"; die "Kiro CLI's installer failed (see the output above). Install it yourself: https://kiro.dev/cli/"; }
    rm -rf "$kiro_tmp"
  fi
  # gh and the agents land here; `agentk install` (run below) needs them too.
  case ":$PATH:" in *":$HOME/.local/bin:"*) ;; *) PATH="$HOME/.local/bin:$PATH"; export PATH ;; esac
  hash -r 2>/dev/null || true
  # (Homebrew's gh was checked with the other packages.)
  ! $gh_download || command -v gh >/dev/null 2>&1 || die "gh is still not available after installing it."
  ! $claude_install || command -v claude >/dev/null 2>&1 || die "Claude Code is still not available after installing it."
  ! $opencode_install || has_opencode || die "OpenCode is still not available after installing it."
  ! $kiro_install || command -v kiro-cli >/dev/null 2>&1 || die "Kiro CLI is still not available after installing it."
  ok "Installed" "$missing"
fi

# Each agent there has to be signed in before AgentK can run sessions on it.
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
# OpenCode needs a model provider: one signed in with `opencode auth login`, or a provider's key in
# the environment, which `opencode auth list` names under Environment.
if has_opencode; then
  agents="${agents:+$agents, }OpenCode"
  oc=opencode; command -v opencode >/dev/null 2>&1 || oc="$opencode_bin/opencode"
  oc_auth=$("$oc" auth list </dev/null 2>/dev/null || true)
  case $oc_auth in
    *[!0-9]"0 credentials"*)
      case $oc_auth in
        *"environment variable"*) ;;
        *)
          if $interactive && ask "  OpenCode has no model provider signed in. Sign in now (opencode auth login)?"; then
            "$oc" auth login </dev/tty || warn "OpenCode" "sign-in didn't finish; run 'opencode auth login' later"
          else
            warn "OpenCode" "no model provider signed in: run 'opencode auth login' before starting sessions"
          fi
          ;;
      esac
      ;;
  esac
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
  if $interactive && ask "  gh isn't signed in to GitHub. AgentK needs it to download releases and for PRs, reviews and updates. Sign in now (gh auth login)?"; then
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
# The newest AgentK release, not the repo's latest (Meko's): tagged agentk-v<version>, or v<version>
# from before releases were tagged by product. Drafts and prereleases are skipped.
if [ -z "$version" ]; then
  if $have_gh; then
    tags=$(gh api "repos/$repo/releases?per_page=100" \
      --jq '[.[] | select((.draft or .prerelease) | not) | .tag_name | select(test("^(agentk-v|v)[0-9]"))][0] // empty' 2>/dev/null) || tags=""
  else
    auth=""
    [ -n "${GITHUB_TOKEN:-}" ] && auth="Authorization: Bearer $GITHUB_TOKEN"
    # GitHub's JSON comes one key per line, each release's tag_name before its draft and prerelease.
    tags=$(curl -fsSL ${auth:+-H "$auth"} "https://api.github.com/repos/$repo/releases?per_page=100" 2>/dev/null | awk '
      /^ *"tag_name": / { t = $0; sub(/^ *"tag_name": *"/, "", t); sub(/".*/, "", t); d = "" }
      /^ *"draft": / { d = $2 }
      /^ *"prerelease": / { if (t ~ /^(agentk-v|v)[0-9]/ && d ~ /^false/ && $2 ~ /^false/) { print t; exit } }') || tags=""
  fi
  [ -n "$tags" ] || die "could not find the newest AgentK release of $repo. The repo is internal: install and log in to the GitHub CLI ('gh auth login') and try again."
  version=${tags#agentk-v}
  version=${version#v}
else
  # The tag of a version asked for: either scheme's.
  tags="agentk-v$version v$version"
fi
# ---- download + verify ----
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT INT TERM

# The tarball is named for its command: agentk, or agent-k-local in releases from before the
# rename to AgentK (newer ones carry both).
got=false
for tag in $tags; do
  for name in agentk agent-k-local; do
    tarball="$name-$version-$platform-$arch.tar.gz"
    rm -f "$tmp/SHA256SUMS"
    if $have_gh; then
      # It downloads SHA256SUMS alone, and succeeds, when the release has no such tarball.
      gh release download "$tag" --repo "$repo" --pattern "$tarball" --pattern SHA256SUMS --dir "$tmp" 2>"$tmp/.err" &&
        [ -f "$tmp/$tarball" ] && { got=true; break 2; }
    else
      base="https://github.com/$repo/releases/download/$tag"
      auth=""
      [ -n "${GITHUB_TOKEN:-}" ] && auth="Authorization: Bearer $GITHUB_TOKEN"
      curl -fsSL ${auth:+-H "$auth"} -o "$tmp/$tarball" "$base/$tarball" 2>"$tmp/.err" &&
        curl -fsSL ${auth:+-H "$auth"} -o "$tmp/SHA256SUMS" "$base/SHA256SUMS" 2>"$tmp/.err" &&
        { got=true; break 2; }
    fi
  done
done
if ! $got; then
  tarball="agentk-$version-$platform-$arch.tar.gz"
  cat "$tmp/.err" >&2
  if $have_gh; then die "download of $tarball from $repo failed.$missing_hint"; fi
  die "download of $tarball failed. The repo is internal: install and log in to the GitHub CLI ('gh auth login') and try again."
fi
rm -f "$tmp/.err"

(cd "$tmp" && grep " $tarball\$" SHA256SUMS | sha256_check >/dev/null) ||
  die "checksum verification failed for $tarball"
ok "Release" "$version ($platform-$arch), checksum verified"

# ---- unpack into app/<version> ----
case $ak_home in "/var/tmp/$(id -un)/meko/"*) private_dir "/var/tmp/$(id -un)/meko" ;; esac
# Owner-only, like Meko's own data folder (which may not be there yet).
(umask 077 && mkdir -p "$ak_home/app")
staging="$ak_home/app/.staging.$$"
rm -rf "$staging"
mkdir -p "$staging"
tar -xzf "$tmp/$tarball" -C "$staging"
[ -x "$staging/$name-$version/bin/$name" ] || die "unexpected tarball layout"

dest="$ak_home/app/$version"
if [ -e "$dest" ]; then
  rm -rf "$dest.old"
  mv "$dest" "$dest.old"
fi
mv "$staging/$name-$version" "$dest"
rm -rf "$staging" "$dest.old"

ln -sfn "$version" "$ak_home/app/current.tmp"
# Rename over the old link atomically. `mv -T` is GNU-only, so use the bundled Node.
"$dest/node/bin/node" -e 'require("fs").renameSync(process.argv[1], process.argv[2])' \
  "$ak_home/app/current.tmp" "$ak_home/app/current"

# A manual (re)install supersedes any pending self-update rollback.
rm -f "$ak_home/update-pending.json"

mkdir -p "$HOME/.local/bin"
# Both names: agent-k-local, from before the rename, is what older services, scripts and Meko run.
for link in agentk agent-k-local; do
  [ -x "$dest/bin/$link" ] && ln -sfn "$ak_home/app/current/bin/$link" "$HOME/.local/bin/$link"
done
ok "Installed" "$short_home/app/$version"
case ":$user_path:" in
  *":$HOME/.local/bin:"*) ok "Command" "$name (in ~/.local/bin)" ;;
  *) warn "Command" "add ~/.local/bin to your PATH to run '$name' (and tools installed there) directly" ;;
esac

# Releases from before --install-deps existed would refuse the flag: pass it only to one that has it.
if grep -q 'install-deps' "$dest/lib/dist/server/install.js" 2>/dev/null; then
  case $install_deps in
    yes) forward="$forward '--install-deps'" ;;
    no) forward="$forward '--no-install-deps'" ;;
  esac
fi

# A release finds its home from its own folder; one from before the move into Meko's data folder needs it named.
export AK_HOME="$ak_home"
# Tells `agentk install` the heading is already printed.
export AK_INSTALLER=1
# When piped into sh, stdin is the script itself: give the setup prompts the terminal.
if [ -r /dev/tty ] && [ -t 2 ]; then
  eval "exec \"\$ak_home/app/current/bin/\$name\" install $forward" </dev/tty
else
  eval "exec \"\$ak_home/app/current/bin/\$name\" install $forward"
fi
