#!/bin/sh
# AgentK's prerequisites: git, curl, gh and a coding agent (Claude Code, OpenCode or Kiro CLI; one is
# enough), each agent signed in, and gh signed in to GitHub. `me_koder install` runs it first, on the
# terminal Meko's `meko extension install agentk` attached (a release has it as lib/scripts/deps.sh),
# and passes its options on:
#
#   --install-deps      install missing tools without asking
#   --no-install-deps   never install tools; stop and say what's missing instead
#   --agent <claude|opencode|kiro>
#                       the coding agent to install when none of Claude Code, OpenCode and Kiro CLI
#                       is there (default: ask; Claude Code with --install-deps or when there's no one
#                       to ask)
#   --non-interactive   ask nothing: install only with --install-deps
#
# Missing tools are installed with Homebrew on macOS and the system package manager on Linux (with
# sudo), gh on Linux from its GitHub release (checksum-verified) into ~/.local/bin, and a coding agent
# when none is installed: Claude Code with Anthropic's installer from claude.ai or Kiro CLI with
# Kiro's from cli.kiro.dev, into ~/.local/bin, or OpenCode with its own from opencode.ai, into
# ~/.opencode/bin, all over HTTPS (none publishes a checksum for its script). It shows the exact
# commands and asks first. Afterwards it checks the tools themselves: Homebrew exiting with an
# error after they work is a warning. It exits 1, saying why, when something is still missing.
set -eu

agent=""
install_deps=ask
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
while [ $# -gt 0 ]; do
  case $1 in
    --agent) [ $# -ge 2 ] || die "--agent needs a value: claude, opencode or kiro"; agent=$2; shift 2 ;;
    --agent=*) agent=${1#--agent=}; shift ;;
    --install-deps) install_deps=yes; shift ;;
    --no-install-deps) install_deps=no; shift ;;
    --non-interactive) interactive=false; shift ;;
    *) die "unknown option $1 (deps.sh takes --install-deps, --no-install-deps, --agent and --non-interactive)" ;;
  esac
done
case $agent in ''|claude|opencode|kiro) ;; *) die "--agent takes claude, opencode or kiro (got $agent)" ;; esac

# ---- platform ----
case $(uname -s) in
  Linux)
    platform=linux
    case $(uname -m) in
      x86_64|amd64) arch=x64 ;;
      aarch64|arm64) arch=arm64 ;;
      *) die "unsupported CPU architecture: $(uname -m)" ;;
    esac
    ;;
  Darwin)
    platform=darwin
    arch=arm64  # only for gh, which Homebrew installs here
    # Homebrew's tools may not be on PATH yet in a fresh shell. (AK_BREW_DIRS is for tests.)
    for brew_dir in ${AK_BREW_DIRS-/opt/homebrew/bin /usr/local/bin}; do
      case ":$PATH:" in *":$brew_dir:"*) ;; *) [ -x "$brew_dir/brew" ] && PATH="$brew_dir:$PATH" ;; esac
    done
    ;;
  *) die "only Linux and macOS are supported (found $(uname -s))" ;;
esac

# ---- prerequisites ----
# Questions go to the terminal.
{ [ -t 2 ] && (: </dev/tty) 2>/dev/null; } || interactive=false
# The sign-ins read the keyboard from the terminal's own device (/dev/ttys003, /dev/pts/1), named
# from stderr, rather than from /dev/tty: Claude Code and OpenCode run on Bun, which on macOS stops
# with "EINVAL: invalid argument, kqueue" when its input is /dev/tty, since kqueue can't watch it.
term=/dev/tty
if $interactive; then
  dev=$(tty <&2 2>/dev/null) && [ -r "$dev" ] && term=$dev
fi
ask() {
  printf '%s %s[Y/n]%s ' "$1" "$d" "$n" >&2
  read -r answer </dev/tty || answer=n
  case $answer in ''|[Yy]*) return 0 ;; *) return 1 ;; esac
}
# The agents' installers and gh (as installed here) live in ~/.local/bin, which a fresh shell may not
# have on PATH yet: look there too, so they aren't installed twice.
case ":$PATH:" in *":$HOME/.local/bin:"*) ;; *) PATH="$PATH:$HOME/.local/bin"; export PATH ;; esac

if command -v sha256sum >/dev/null 2>&1; then
  sha256_check() { sha256sum -c -; }
elif command -v shasum >/dev/null 2>&1; then
  sha256_check() { shasum -a 256 -c -; }
else
  sha256_check() { die "sha256sum (coreutils) or shasum is required to check gh's download, and neither was found on PATH."; }
fi

git_ok() {
  command -v git >/dev/null 2>&1 || return 1
  # macOS ships a git that only asks to install the Command Line Tools.
  [ "$platform" != darwin ] || xcode-select -p >/dev/null 2>&1 || [ -x "$(brew --prefix 2>/dev/null)/bin/git" ]
}

# What's missing, and the plan to install it: `plan` is shown to the user, `pkgs` goes to the
# package manager, `gh_download` fetches gh from its GitHub release (Linux).
missing="" pkgs="" plan="" gh_download=false claude_install=false opencode_install=false kiro_install=false
add_missing() { missing="${missing:+$missing, }$1"; }
git_ok || { add_missing git; pkgs="$pkgs git"; }
command -v curl >/dev/null 2>&1 || { add_missing curl; pkgs="$pkgs curl"; }
gh_skipped=false
if ! command -v gh >/dev/null 2>&1; then
  # With GITHUB_TOKEN, Meko downloads with the token, so an unattended install can go on without gh
  # (AgentK then can't open PRs or follow reviews until it's installed).
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
      die "missing: $missing. Install Homebrew (https://brew.sh), then run meko extension install agentk again; or install them yourself."
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
      *) die "missing: $missing, and no supported package manager (apt-get, dnf, yum, zypper, pacman, apk) was found. Install them yourself and run meko extension install agentk again." ;;
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
      curl -fsSL https://opencode.ai/v2/install | bash   (OpenCode 2, into ~/.opencode/bin)"
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
  say "    To install them, AgentK's setup will run:$plan"
  case $install_deps in
    yes) ;;
    no) die "missing: $missing. Install them and run meko extension install agentk again, or drop --no-install-deps." ;;
    ask)
      $interactive || die "missing: $missing. Run meko extension install agentk -- --install-deps to install them, or install them yourself."
      ask "    Install them now?" || die "missing: $missing. Install them and run meko extension install agentk again."
      ;;
  esac
  if [ -n "$pkgs" ]; then
    brew_failed=false
    if ! install_system_pkgs; then
      # On Linux a failed install can leave the package database half-configured: stop there.
      [ "$platform" = darwin ] || die "installing$pkgs failed (see the output above). Install them yourself and run meko extension install agentk again."
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
        die "brew failed (see the output above: its error can be well before its last lines). $problem Run 'brew install $bad' yourself to see what went wrong, then run meko extension install agentk again."
      fi
      die "$problem"
    fi
    $brew_failed && warn "brew" "exited with an error, but$pkgs work: going on"
  fi
  if $gh_download; then
    install_gh || die "couldn't install gh from github.com/cli/cli. Install it yourself (https://cli.github.com) and run meko extension install agentk again."
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
    # opencode.ai/install is OpenCode 1's, which AgentK doesn't run.
    { curl -fsSL -o "$oc_tmp/install" https://opencode.ai/v2/install && bash "$oc_tmp/install" </dev/null; } ||
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
      claude auth login <"$term" || warn "Claude Code" "sign-in didn't finish; run 'claude auth login' later"
    else
      warn "Claude Code" "not signed in: run 'claude auth login' before starting sessions"
    fi
  fi
fi
# OpenCode needs a model provider: one signed in with `opencode auth login`, or a provider's key in
# the environment, which `opencode auth list` lists too. Both run on a server of their own
# (--standalone): OpenCode 2's shared background service has another environment, and AgentK never
# starts it.
if has_opencode; then
  agents="${agents:+$agents, }OpenCode"
  oc=opencode; command -v opencode >/dev/null 2>&1 || oc="$opencode_bin/opencode"
  oc_auth=$("$oc" auth list --standalone </dev/null 2>/dev/null || true)
  case $oc_auth in
    *"No authenticated integrations"*)
      if $interactive && ask "  OpenCode has no model provider signed in. Sign in now (opencode auth login)?"; then
        "$oc" auth login --standalone <"$term" || warn "OpenCode" "sign-in didn't finish; run 'opencode auth login' later"
      else
        warn "OpenCode" "no model provider signed in: run 'opencode auth login' before starting sessions"
      fi
      ;;
  esac
fi
if command -v kiro-cli >/dev/null 2>&1; then
  agents="${agents:+$agents, }Kiro"
  if ! kiro-cli whoami >/dev/null 2>&1; then
    if $interactive && ask "  Kiro isn't signed in. Sign in now (kiro-cli login)?"; then
      kiro-cli login <"$term" || warn "Kiro" "sign-in didn't finish; run 'kiro-cli login' later"
    else
      warn "Kiro" "not signed in: run 'kiro-cli login' before starting sessions"
    fi
  fi
fi

# gh signs in to GitHub for PRs and reviews, and Meko downloads AgentK's releases with its token.
if $gh_skipped; then
  warn "GitHub" "gh isn't installed: install gh for PRs and reviews"
elif ! gh auth status >/dev/null 2>&1; then
  if $interactive && ask "  gh isn't signed in to GitHub. AgentK needs it for PRs and reviews, and Meko for AgentK's updates. Sign in now (gh auth login)?"; then
    gh auth login <"$term" || warn "GitHub" "sign-in didn't finish; run 'gh auth login' later"
  else
    warn "GitHub" "gh isn't signed in: run 'gh auth login' (needed for PRs, reviews and updates)"
  fi
fi

tools="git, curl"
$gh_skipped || tools="$tools, gh"
ok "Tools" "$tools, $agents"
