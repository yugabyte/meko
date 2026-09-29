#!/usr/bin/env bash
# Bring up the full local Meko stack with one command.
#
#   1. Validate prereqs (.env, Docker, yugabyted with mage+pgvector+pg_dist_rag).
#   1c.Verify every component source dir exists, show its git branch, and
#      ask for confirmation before starting.
#   2. Start yugabyted natively. By default existing data is preserved and the
#      cluster is resumed (reused if already running); pass --clean to destroy
#      it first and start fresh.
#   3. Create vector/mage/pg_dist_rag extensions in the yugabyte DB.
#   4. `docker compose up -d --build` for everything else.
#   5. Wait for api_server health, then rewrite the seed yb_universe row
#      to point at host.docker.internal:5433.
#   6. Print the URLs.
#
# Flags (see --help for the full list):
#   -y, --yes          skip the pre-deploy confirmation prompt (also: MEKO_ASSUME_YES=1).
#   -c, --clean        DESTROY existing yugabyted data and start fresh (also: MEKO_CLEAN=1).
#                      Without it, existing data is preserved/resumed.
#   --reload <t>...    reload one or more components (stack already up) and exit; --help lists targets.
#                      Use 'all' (or --reload-all) to reload every service.
#   --update <t>...    git-pull latest and reload what changed, then exit ('all'/--update-all
#                      = the sibling repos, not yugabyte-db). --no-apply pulls only;
#                      --apply skips the confirm before a slow rebuild (langfuse).
#   --reconfigure      re-ask every required value (current shown as default), then deploy.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ENV_FILE="${SCRIPT_DIR}/.env"
ENV_EXAMPLE="${SCRIPT_DIR}/.env.example"

# ANSI-C quoting ($'...') so these hold real escape bytes — needed because
# they're expanded both via printf (below) and inside a cat heredoc (Step 6),
# and cat does no escape interpretation.
GREEN=$'\033[0;32m'; RED=$'\033[0;31m'; YEL=$'\033[0;33m'; CYAN=$'\033[0;36m'; NC=$'\033[0m'
info()  { printf "${CYAN}▸${NC} %s\n" "$*"; }
ok()    { printf "${GREEN}✓${NC} %s\n" "$*"; }
warn()  { printf "${YEL}⚠${NC} %s\n" "$*"; }
die()   { printf "${RED}✗${NC} %s\n" "$*" >&2; exit 1; }

# Numbered phase banner so a reader of the run log always sees the current stage.
# Auto-increments; bump STAGE_TOTAL if you add or remove a stage() call.
# substage() numbers within the current stage ([N.M]) for long, multi-part phases.
# stage() is silent in --reload/--update mode, which run no numbered deploy stages.
STAGE_NUM=0; STAGE_TOTAL=8; SUBSTAGE_NUM=0
stage()    { { [ -n "${RELOAD_TARGETS:-}" ] || [ -n "${UPDATE_TARGETS:-}" ]; } && return 0; STAGE_NUM=$((STAGE_NUM + 1)); SUBSTAGE_NUM=0; printf "\n${CYAN}━━━ [%d/%d] %s ━━━${NC}\n" "${STAGE_NUM}" "${STAGE_TOTAL}" "$*"; }
substage() { SUBSTAGE_NUM=$((SUBSTAGE_NUM + 1)); printf "\n${CYAN}── [%d.%d] %s ──${NC}\n" "${STAGE_NUM}" "${SUBSTAGE_NUM}" "$*"; }

# docker compose for this stack (env-file + compose file), in one place.
dc() { docker compose --env-file "${ENV_FILE}" -f "${SCRIPT_DIR}/docker-compose.yml" "$@"; }

# Detect the non-loopback IP YSQL bound to and export HOST_GATEWAY_IP so the
# containers map host.docker.internal to it (Linux); fall back to 127.0.0.1
# (macOS / no ss). Sets the global YB_LISTEN_HOST. Silent — callers log.
detect_ysql_host() {
  YB_LISTEN_HOST="127.0.0.1"
  command -v ss >/dev/null 2>&1 || return 0
  local detected
  detected="$(ss -ltnH "sport = :${YB_PORT}" 2>/dev/null \
    | awk '{print $4}' | sed -E 's/:[0-9]+$//' \
    | grep -vE '^(127\.|0\.0\.0\.0$|\[?::1?\]?$|\*$)' | head -1 || true)"
  if [ -n "${detected}" ]; then
    YB_LISTEN_HOST="${detected}"
    export HOST_GATEWAY_IP="${detected}"
  fi
}

# detect_ysql_host + log the container mapping, for the reload/update paths (which
# assume the stack is already up). Warns on Linux when no routable address is found
# — a --force-recreate would then leave containers unable to reach the DB, usually
# because yugabyted isn't running.
detect_and_report_db_host() {
  detect_ysql_host
  if [ "${YB_LISTEN_HOST}" != "127.0.0.1" ]; then
    info "host.docker.internal → ${YB_LISTEN_HOST}"
  elif [ "$(uname)" = "Linux" ]; then
    warn "couldn't detect a routable YSQL address (is yugabyted running?); a recreate may leave containers unable to reach the DB"
  fi
}

# Map a --reload alias to its canonical target, or "" if unknown. Aliases live
# here only; reload_one() switches on canonical names.
canon_target() {
  case "$1" in
    mcp|mem0|meko-mcp-server)                         echo mcp ;;
    mcp-rebuild)                                      echo mcp-rebuild ;;
    gateway|inference-gateway|meko-inference-gateway) echo gateway ;;
    rag|rag-agent|meko-rag-agent)                     echo rag ;;
    rag-rebuild)                                      echo rag-rebuild ;;
    proxy|nginx|meko-proxy)                           echo proxy ;;
    api|api-server|meko-api-server)                   echo api ;;
    ui|meko-ui)                                       echo ui ;;
    langfuse|langfuse-web|langfuse-worker)            echo langfuse ;;
    all|everything)                                   echo all ;;
    *)                                                echo "" ;;
  esac
}

# Reload ONE canonical target (already validated). Bind-mounted sources (Python,
# nginx conf) just restart; compiled/built ones (Go, Vite, Next) rebuild + recreate.
reload_one() {
  case "$1" in
    mcp)      info "restart meko-mcp-server (mem0 + meko-mcp-server/src are bind-mounted)"
              dc restart meko-mcp-server ;;
    mcp-rebuild) info "rebuild + recreate meko-mcp-server (dependencies, Dockerfile or mcp_auth changed)"
              dc build meko-mcp-server
              dc up -d --force-recreate meko-mcp-server ;;
    gateway)  info "restart meko-inference-gateway (bind-mounted source)"
              dc restart meko-inference-gateway ;;
    rag)      info "restart both rag workers (rag_agent source is shared by TEXT + PDF)"
              dc restart meko-rag-agent meko-rag-agent-pdf ;;
    rag-rebuild) info "rebuild + recreate both rag workers (requirements or Dockerfile changed)"
              dc build meko-rag-agent
              dc up -d --force-recreate meko-rag-agent meko-rag-agent-pdf ;;
    proxy)    info "restart meko-proxy (nginx/default.conf is bind-mounted)"
              dc restart meko-proxy ;;
    api)      info "rebuild + recreate api-server (+migrate), restart proxy"
              dc build meko-api-server
              dc up -d --force-recreate meko-api-server-migrate meko-api-server
              dc restart meko-proxy ;;
    ui)       info "rebuild + recreate meko-ui, restart proxy"
              dc build meko-ui
              dc up -d --force-recreate meko-ui
              dc restart meko-proxy ;;
    langfuse) info "rebuild + recreate langfuse web/worker, restart proxy (slow, ~10-15m)"
              dc build langfuse-web langfuse-worker
              dc up -d --force-recreate langfuse-web langfuse-worker
              dc restart meko-proxy ;;
    *)        die "internal: unhandled reload target '$1'" ;;
  esac
}

# --reload: apply the minimal correct reload for one or more components and exit,
# assuming the rest of the stack is up. detect_and_report_db_host first so a
# --force-recreate keeps the host.docker.internal → yugabyted mapping (see the
# README HOST_GATEWAY_IP note). Targets are validated at parse time.
do_reload() {  # $@ = canonical targets
  detect_and_report_db_host
  # "all" reloads every service in one pass: rebuild all images (cache-fast; only
  # changed ones get a new image) then force-recreate every container — this picks
  # up compiled changes AND restarts the bind-mounted Python services. Cheaper than
  # looping per target (one build, one recreate, no repeated proxy bounces). The
  # langfuse image rebuild can still take ~10-15m the first time.
  case " $* " in
    *" all "*)
      info "reloading ALL services: build + force-recreate (langfuse rebuild can be slow)"
      dc build
      dc up -d --force-recreate
      ok "reload complete: all"
      return ;;
  esac
  local t
  for t in "$@"; do reload_one "$t"; done
  ok "reload complete: $*"
}

# --update target lookup (distinct from --reload's: here mem0 and mcp are separate
# repos). mode=kind → "repo" | "refuse:<why>" | "" — a NAME-only check, safe at
# parse time before component dirs are resolved. mode=dir → the repo's dir, and is
# only called from do_update after resolve_dir has run.
update_target() {  # $1 = name, $2 = kind|dir
  case "$1" in
    mem0|meko-mem0)
      if [ "$2" = dir ]; then echo "${MEKO_MEM0_DIR:-}"; else echo repo; fi ;;
    mcp|mcp-server|meko-mcp-server)
      if [ "$2" = dir ]; then echo "${MEKO_MCP_SERVER_DIR:-}"; else echo repo; fi ;;
    langfuse|meko-langfuse)
      if [ "$2" = dir ]; then echo "${MEKO_LANGFUSE_DIR:-}"; else echo repo; fi ;;
    rag|rag-agent|yugabyte|yugabyte-db|yb)
      if [ "$2" = dir ]; then echo "${MEKO_YB_HOME:-}"; else echo repo; fi ;;
    api|api-server|ui|meko-ui|gateway|inference-gateway)
      echo "refuse:it lives in the meko repo (this working tree) — pull it with git yourself" ;;
    *)
      echo "" ;;
  esac
}

# Pull latest and replay local work on top, for one repo dir: git pull --rebase
# --autostash on the branch's configured upstream (falling back to origin/<branch>
# when none is set). Local commits are rebased and uncommitted edits are
# autostashed/reapplied. Sets the globals UPDATE_CHANGED (0|1) and UPDATE_BEFORE
# (the pre-pull commit, when changed). On a failed pull, or
# when the rebase/autostash leaves merge conflicts, it stops and points at the dir
# to fix by hand rather than reporting success.
update_repo() {  # $1 = label, $2 = dir
  local label="$1" dir="$2" branch before after pull_args
  UPDATE_CHANGED=0; UPDATE_BEFORE=""
  git -C "$dir" rev-parse --is-inside-work-tree >/dev/null 2>&1 \
    || die "${label}: not a git checkout (${dir}) — nothing to pull (a release tarball?)"
  branch="$(git -C "$dir" symbolic-ref --short -q HEAD || true)"
  [ -n "$branch" ] || die "${label}: detached HEAD in ${dir} — check out a branch first"
  if git -C "$dir" rev-parse --abbrev-ref '@{upstream}' >/dev/null 2>&1; then
    pull_args=""                 # use the branch's configured upstream
  else
    pull_args="origin ${branch}" # no upstream set → try origin/<branch>
  fi
  before="$(git -C "$dir" rev-parse HEAD)"
  info "${label}: pull --rebase, replaying local changes (${dir})"
  # shellcheck disable=SC2086  # pull_args is "" or "origin <branch>" — intentional split
  if ! git -C "$dir" pull --rebase --autostash ${pull_args} 2>&1 | sed 's/^/    /'; then
    git -C "$dir" rebase --abort >/dev/null 2>&1 || true
    die "${label}: pull/rebase failed — ${dir} may need cleanup (check 'git -C ${dir} status')"
  fi
  if [ -n "$(git -C "$dir" ls-files -u 2>/dev/null)" ]; then
    die "${label}: reapplying your local changes hit conflicts in ${dir}. Resolve them ('git -C ${dir} status'); your pre-pull edits are also saved in the autostash — after resolving, drop it with 'git -C ${dir} stash drop'."
  fi
  after="$(git -C "$dir" rev-parse HEAD)"
  if [ "$before" != "$after" ]; then
    UPDATE_CHANGED=1; UPDATE_BEFORE="$before"
    ok "${label}: updated to ${after:0:9} (${branch})"
  else
    ok "${label}: already up to date (${branch})"
  fi
}

# The --reload target that applies a pulled --update for a component. Only source
# is bind-mounted; a pull touching what the image bakes in at build time needs a
# rebuild, or new code runs on the old installed packages.
update_apply_target() {  # $1 = target, $2 = repo dir; uses UPDATE_BEFORE
  local changed
  changed="$(git -C "$2" diff --name-only "${UPDATE_BEFORE}" HEAD 2>/dev/null || true)"
  case "$1" in
    mem0|meko-mem0)
      if printf '%s\n' "$changed" | grep -qxE 'pyproject\.toml|uv\.lock'; then
        warn "meko-mem0's dependencies changed; the MCP image installs them from meko-mcp-server's lock, so new ones arrive only when that pin moves (then: --update mcp)" >&2
      fi
      echo mcp ;;
    mcp|mcp-server|meko-mcp-server)
      if printf '%s\n' "$changed" | grep -qE '^(pyproject\.toml|uv\.lock|Dockerfile|mcp_auth/)'; then
        echo mcp-rebuild
      else
        echo mcp
      fi ;;
    langfuse|meko-langfuse) echo langfuse ;;
    rag|rag-agent|yugabyte|yugabyte-db|yb)
      if printf '%s\n' "$changed" | grep -qE '^python/ai/rag_agent/(requirements[^/]*\.txt|Dockerfile)$'; then
        echo rag-rebuild
      else
        echo rag
      fi ;;
  esac
}

# Reload targets that mean a slow rebuild rather than a quick restart.
is_expensive_reload() { case "$1" in langfuse) return 0 ;; *) return 1 ;; esac; }

# Confirm a slow rebuild before doing it. Auto-yes with --apply or -y; otherwise
# prompt on a terminal; otherwise No — never trigger a ~15m rebuild unattended.
confirm_rebuild() {  # $1 = target
  if [ "${APPLY_FORCE:-0}" = "1" ] || [ "${ASSUME_YES:-0}" = "1" ]; then return 0; fi
  can_prompt || return 1
  local reply=""
  printf "${CYAN}▸${NC} %s changed and needs a rebuild (~10-15 min). Rebuild now? [y/N] " "$1"
  read -r reply || reply=""
  case "$reply" in y|Y) return 0 ;; *) return 1 ;; esac
}

# Append $1 to do_update's `reloads`, deduped; a rebuild supersedes a plain restart
# of the same service (mem0 + mcp both map to mcp, and either may need mcp-rebuild).
add_reload() {
  local base="${1%-rebuild}" x out=""
  for x in ${reloads}; do
    if [ "$x" = "$1" ] || [ "$x" = "${base}-rebuild" ]; then return 0; fi
    [ "$x" = "$base" ] && continue      # $1 is ${base}-rebuild: drop the restart
    out="${out:+${out} }$x"
  done
  reloads="${out:+${out} }$1"
}

# --update: pull+rebase one or more component repos, then exit. Targets are
# validated at parse time. By DEFAULT it also reloads the components that actually
# changed (deduped, mem0+mcp → one mcp reload) — cheap restarts run automatically,
# a slow rebuild (langfuse) is confirmed first (auto-yes with --apply/-y).
# --no-apply pulls only.
do_update() {  # $@ = raw target names
  local t dir reloads="" r applied="" skipped=""
  for t in "$@"; do
    dir="$(update_target "$t" dir)"
    [ -n "$dir" ] || die "internal: no repo dir for --update target '$t'"
    update_repo "$t" "$dir"
    if [ "${UPDATE_CHANGED}" = "1" ]; then
      add_reload "$(update_apply_target "$t" "$dir")"
    fi
  done
  ok "update complete: $*"

  # --no-apply: pull only; just point at the reload command for what changed.
  if [ "${APPLY_AFTER}" != "1" ]; then
    if [ -n "${reloads}" ]; then
      # shellcheck disable=SC2086
      info "changed — apply with:  ./start.sh --reload ${reloads}"
    else
      info "nothing changed"
    fi
    return
  fi

  if [ -z "${reloads}" ]; then info "nothing changed — no reload needed"; return; fi
  # Applying restarts/rebuilds containers, which needs Docker. The pull already
  # succeeded, so if Docker isn't up just point at the reload command.
  if ! { command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1; }; then
    # shellcheck disable=SC2086
    warn "pulled, but Docker isn't available to reload — apply with:  ./start.sh --reload ${reloads}"
    return
  fi
  detect_and_report_db_host
  # shellcheck disable=SC2086  # canonical reload targets are single words
  for r in ${reloads}; do
    if is_expensive_reload "$r" && ! confirm_rebuild "$r"; then
      skipped="${skipped:+${skipped} }$r"; continue
    fi
    reload_one "$r"; applied="${applied:+${applied} }$r"
  done
  if [ -n "${applied}" ]; then ok "applied: ${applied}"; fi
  if [ -n "${skipped}" ]; then
    # shellcheck disable=SC2086
    warn "skipped rebuild: ${skipped} — apply later with:  ./start.sh --reload ${skipped}"
  fi
}

# Can we interactively ask the user? Only with a real terminal and no -y.
can_prompt() { [ -t 0 ] && [ "${ASSUME_YES:-0}" != "1" ]; }

# Secrets get a hidden (no-echo) prompt; the rest echo normally.
is_secret_var() {
  case "$1" in COGNITO_CLIENT_SECRET|OPENAI_API_KEY|AWS_SECRET_ACCESS_KEY|AWS_SESSION_TOKEN) return 0 ;; *) return 1 ;; esac
}

# Write NAME=VALUE into .env, replacing the existing line in place (any value,
# incl. blank) or appending if absent. awk+ENVIRON writes the value verbatim (no
# sed-escaping pitfalls). NOTE: .env is later sourced as shell (`. .env`), so a value
# must be safe unquoted — prompt_var restricts what can be entered, and the only
# other caller writes an empty value.
set_env_var() {  # $1 = name, $2 = value
  local name="$1" tmp; export SEV_VAL="$2"
  tmp="$(mktemp "${TMPDIR:-/tmp}/meko-env.XXXXXX")"   # explicit template: BSD/macOS mktemp needs one
  awk -v n="$name" '
    $0 ~ "^"n"=" { print n"="ENVIRON["SEV_VAL"]; found=1; next }
    { print }
    END { if (!found) print n"="ENVIRON["SEV_VAL"] }
  ' "${ENV_FILE}" > "$tmp" && cat "$tmp" > "${ENV_FILE}"
  rm -f "$tmp"; unset SEV_VAL
}

# Prompt for one var, validate, persist to .env, and export it. An optional default
# (current value) shows in [brackets]; empty input keeps it. Secrets are hidden.
prompt_var() {  # $1 = name, $2 = default (optional)
  local name="$1" def="${2:-}" val=""
  while :; do
    if is_secret_var "$name"; then
      if [ -n "$def" ]; then printf "${CYAN}▸${NC} %s (hidden, Enter=keep current): " "$name"
      else                   printf "${CYAN}▸${NC} %s (hidden): " "$name"; fi
      # -n puts the tty in non-canonical mode: macOS caps a canonical line at 1024
      # bytes (MAX_CANON) and silently refuses the rest of a longer AWS_SESSION_TOKEN.
      read -rs -n 8192 val || { printf '\n'; die "aborted (EOF)"; }; printf '\n'
    else
      if [ -n "$def" ]; then printf "${CYAN}▸${NC} %s [%s]: " "$name" "$def"
      else                   printf "${CYAN}▸${NC} %s: " "$name"; fi
      read -r val || die "aborted (EOF)"
    fi
    [ -n "$val" ] || val="$def"          # empty input keeps the shown default
    [ -n "$val" ] || { warn "${name} cannot be empty"; continue; }
    case "$val" in
      *[!A-Za-z0-9_./:+=@-]*)
        warn "${name}: has a character not allowed in .env (it is sourced as shell) — use letters, digits or _ . / : + = - @, no spaces/quotes/\$"; continue ;;
    esac
    if [ "$name" = "MEKO_YB_HOME" ] && [ ! -x "${val}/bin/yugabyted" ]; then
      warn "no bin/yugabyted under '${val}' — enter the path to your YugabyteDB tree"; continue
    fi
    break
  done
  # Unchanged (Enter kept the default): the var is already set/exported from .env, so
  # don't rewrite it — that would expand a raw default like ${HOME}/… to a literal path.
  [ "$val" = "${!name:-}" ] && return 0
  set_env_var "$name" "$val"
  export "${name}=${val}"
}

# Interactive setup over REQUIRED_VARS (which must be set before this is called).
# On a first run or --reconfigure it prompts for EVERY required var with its current
# value — .env, i.e. the .env.example defaults on a first run — shown as the default:
# Enter keeps it, or type to override. On a normal run it prompts only the blank ones.
# Secrets are hidden. No-op for reload/update and when we can't prompt (the
# missing-vars check then reports any blanks).
configure_vars() {
  { [ -n "${RELOAD_TARGETS:-}" ] || [ -n "${UPDATE_TARGETS:-}" ]; } && return 0
  can_prompt || return 0
  local v cur confirm=0 prompted=0
  if [ "${FRESH_ENV:-0}" = "1" ] || [ "${RECONFIGURE:-0}" = "1" ]; then confirm=1; fi
  for v in "${REQUIRED_VARS[@]}"; do
    cur="${!v:-}"
    if [ -n "$cur" ] && [ "$confirm" != "1" ]; then continue; fi   # normal run: blanks only
    if [ "$prompted" = "0" ]; then
      if [ "$confirm" = "1" ]; then info "review configuration — Enter keeps the [current] value, or type to change:"
      else                          info "some required values are unset — enter them (saved to .env):"; fi
      prompted=1
    fi
    prompt_var "$v" "$cur"
  done
  # The session token follows the key as entered above, not as it was in .env: ask
  # for it with a temporary (ASIA…) key — always on a review, else only when blank.
  if [ "${USES_BEDROCK:-0}" = "1" ] && [[ "${AWS_ACCESS_KEY_ID:-}" == ASIA* ]] \
     && { [ -z "${AWS_SESSION_TOKEN:-}" ] || [ "$confirm" = "1" ]; }; then
    prompt_var AWS_SESSION_TOKEN "${AWS_SESSION_TOKEN:-}"; prompted=1
  fi
  [ "$prompted" = "1" ] || return 0
  chmod 600 "${ENV_FILE}" 2>/dev/null || true
  ok "configuration saved to .env"
}

usage() {
  cat <<USAGE
start.sh — bring up (or reload part of) the local Meko stack.

Usage:
  ./start.sh [-y] [-c]             full deploy: yugabyted + extensions + docker stack
  ./start.sh --reload <t> [t...]   rebuild/restart one or more components (stack up), then exit
  ./start.sh --update <t> [t...]   git-pull latest + reload what changed, then exit
  ./start.sh --update <t> --no-apply   ...pull only, don't reload
  ./start.sh --reconfigure         re-ask every required value (Enter keeps current), then deploy
  ./start.sh -h | --help

Flags:
  -y, --yes             skip the pre-deploy confirmation prompt (env: MEKO_ASSUME_YES=1)
  -c, --clean           DESTROY existing yugabyted data and start fresh (env: MEKO_CLEAN=1);
                        without it, existing data is preserved/resumed
      --reload T [T...] reload one or more components (below) and exit, e.g.
                        './start.sh --reload mcp rag'; keeps the
                        host.docker.internal → yugabyted mapping intact
      --update T [T...] for each repo, 'git pull --rebase --autostash' on its current
                        branch — pulls the latest commit and replays your local
                        commits + uncommitted edits on top. By DEFAULT it then
                        reloads the components that changed (cheap restarts run
                        automatically; a slow rebuild like langfuse is confirmed
                        first). Then exit.
      --no-apply        with --update: pull only, don't reload anything
      --apply           with --update: reload changed components without confirming
                        the slow rebuilds (implies -y for the rebuild prompt)
      --reconfigure     re-ask every required value (current shown as default,
                        Enter keeps), then deploy. Needs a terminal.
  -h, --help            show this help

Interactive setup: on a first run or --reconfigure the script prompts for EVERY
required value, each pre-filled with its current .env value (the .env.example
default on a first run) — Enter keeps it, or type to override; secrets are hidden.
On a normal run it prompts only for values that are still blank. Non-interactively
(piped, CI, or -y) it doesn't prompt — it reports any blanks and uses defaults as-is.

--reload targets (bind-mounted source → fast restart; compiled/built → rebuild):
  mcp        restart meko-mcp-server         picks up mem0 + meko-mcp-server/src *.py
  mcp-rebuild  rebuild + recreate meko-mcp-server  its pyproject/uv.lock, Dockerfile, mcp_auth
  gateway    restart meko-inference-gateway  picks up inference_gateway *.py
  rag        restart meko-rag-agent + -pdf   picks up rag_agent *.py (BOTH workers)
  rag-rebuild  rebuild + recreate both rag workers  rag_agent requirements*.txt, Dockerfile
  proxy      restart meko-proxy              picks up nginx/default.conf
  api        rebuild api-server(+migrate) + restart proxy   api_server *.go
  ui         rebuild meko-ui + restart proxy                meko_ui (TS/Vite)
  langfuse   rebuild langfuse web/worker + restart proxy    meko-langfuse (Next.js, ~10-15m)
  all        every service: build + force-recreate all (also './start.sh --reload-all')

--update targets (standalone repos only):
  mem0       meko-mem0 checkout (MEKO_MEM0_DIR)
  mcp        meko-mcp-server checkout (MEKO_MCP_SERVER_DIR)
  langfuse   meko-langfuse checkout (MEKO_LANGFUSE_DIR)
  rag        yugabyte-db checkout (MEKO_YB_HOME) — only if it is a git repo, not a tarball
  all        the sibling repos: mem0 + mcp + langfuse (NOT yugabyte-db); also './start.sh --update-all'
  (api/ui/gateway are NOT here: they live in this meko repo — pull it with git yourself)

Env overrides: MEKO_YB_HOME, MEKO_*_DIR (component dirs), MEKO_GIT_BASE,
  MEKO_NO_CLONE=1 (don't auto-clone siblings), RUDDERSTACK_ENABLED=true.
USAGE
}

# ─── Arg parsing ──────────────────────────────────────────────────────────
ASSUME_YES="${MEKO_ASSUME_YES:-0}"
CLEAN="${MEKO_CLEAN:-0}"
RELOAD_TARGETS=""   # space-separated canonical targets (single-word, safe to split)
UPDATE_TARGETS=""   # space-separated raw component names to git-pull
APPLY_AFTER=1       # --update reloads changed components by default; --no-apply opts out
APPLY_FORCE=0       # --apply: also do the slow rebuilds (langfuse) without confirming
APPLY_FLAG_SEEN=0   # did the user pass --apply / --no-apply explicitly?
RECONFIGURE=0       # re-ask every required value (current shown as default), then deploy

# Canonicalize + validate one raw --reload target, then append it.
add_reload_target() {
  local c; c="$(canon_target "$1")"
  [ -n "$c" ] || die "unknown --reload target '$1' (run './start.sh --help' for the list)"
  RELOAD_TARGETS="${RELOAD_TARGETS:+$RELOAD_TARGETS }$c"
}

# The sibling repos --update-all pulls. yugabyte-db (rag) is deliberately excluded:
# it is usually a release tarball (can't be pulled) and is a heavy repo you rarely
# batch-update — run `--update rag` explicitly when it's a git checkout.
UPDATE_ALL_TARGETS="mem0 mcp langfuse"

# Validate one raw --update target by name (must be a standalone repo), then append.
# `all`/`everything` expands to the sibling repos (not yugabyte-db).
add_update_target() {
  case "$1" in
    all|everything) UPDATE_TARGETS="${UPDATE_TARGETS:+$UPDATE_TARGETS }${UPDATE_ALL_TARGETS}"; return ;;
  esac
  local k; k="$(update_target "$1" kind)"
  case "$k" in
    "")        die "unknown --update target '$1' (run './start.sh --help' for the list)" ;;
    refuse:*)  die "cannot --update '$1': ${k#refuse:}" ;;
  esac
  UPDATE_TARGETS="${UPDATE_TARGETS:+$UPDATE_TARGETS }$1"
}

while [ $# -gt 0 ]; do
  case "$1" in
    -y|--yes) ASSUME_YES=1 ;;
    -c|--clean) CLEAN=1 ;;
    --reload)
      # Consume every following non-flag word as a target: `--reload mcp rag`.
      shift
      [ $# -gt 0 ] && [ "${1#-}" = "$1" ] || die "--reload needs at least one target (run './start.sh --help')"
      while [ $# -gt 0 ] && [ "${1#-}" = "$1" ]; do add_reload_target "$1"; shift; done
      continue ;;
    --reload=*) add_reload_target "${1#*=}" ;;
    --reload-all) add_reload_target all ;;
    --update)
      shift
      [ $# -gt 0 ] && [ "${1#-}" = "$1" ] || die "--update needs at least one target (run './start.sh --help')"
      while [ $# -gt 0 ] && [ "${1#-}" = "$1" ]; do add_update_target "$1"; shift; done
      continue ;;
    --update=*) add_update_target "${1#*=}" ;;
    --update-all) add_update_target all ;;
    --apply) APPLY_FORCE=1; APPLY_FLAG_SEEN=1 ;;
    --no-apply) APPLY_AFTER=0; APPLY_FLAG_SEEN=1 ;;
    --reconfigure) RECONFIGURE=1 ;;
    -h|--help) usage; exit 0 ;;
    *) die "unknown flag: $1 (run './start.sh --help')" ;;
  esac
  shift
done

if [ "${APPLY_FLAG_SEEN}" = "1" ] && [ -z "${UPDATE_TARGETS}" ]; then
  die "--apply / --no-apply only apply to --update (run './start.sh --help')"
fi
if [ "${RECONFIGURE}" = 1 ] && { [ -n "${RELOAD_TARGETS}" ] || [ -n "${UPDATE_TARGETS}" ]; }; then
  die "--reconfigure can't be combined with --reload/--update (run './start.sh --help')"
fi

# ─── Step 0: .env handling ────────────────────────────────────────────────
stage "Validating configuration (.env)"
FRESH_ENV=0
if [ ! -f "${ENV_FILE}" ]; then
  info "creating .env from .env.example"
  cp "${ENV_EXAMPLE}" "${ENV_FILE}"
  FRESH_ENV=1   # first run → confirm the placeholder-defaulted vars below
  # Interactively we fill the blanks below; non-interactively there's nobody to
  # ask, so keep the old behaviour: tell the user to edit it and re-run.
  can_prompt || die ".env created. Fill in the required values (Cognito, OPENAI_API_KEY, MEKO_YB_HOME, and AWS_* for the default Bedrock chat or INFERENCE_LLM_PROVIDER=openai) and re-run."
fi

# Load .env so checks below can read it. set -a auto-exports.
set -a
# shellcheck source=/dev/null
. "${ENV_FILE}"
set +a

# --reconfigure re-asks every required var below (current value shown as the default,
# Enter keeps it). It doesn't wipe anything — override only what you want to change.
if [ "${RECONFIGURE}" = "1" ]; then
  can_prompt || die "--reconfigure needs an interactive terminal (and not -y)"
  info "--reconfigure: re-asking every configuration value (Enter keeps current)"
fi

# The required vars, in prompt order. AWS creds are added only when a Bedrock
# provider is selected (they're unused otherwise).
REQUIRED_VARS=(
  COGNITO_REGION COGNITO_POOL_ID COGNITO_CLIENT_ID COGNITO_CLIENT_SECRET
  OPENAI_API_KEY
  MEKO_YB_HOME YB_BASE_DIR YB_HOST YB_PORT YB_USER
  UI_HOST_PORT API_SERVER_HOST_PORT MCP_SERVER_HOST_PORT INFERENCE_GATEWAY_HOST_PORT
)
USES_BEDROCK=0
if [ "${INFERENCE_LLM_PROVIDER:-}" = "bedrock" ] || [ "${LLM_PROVIDER:-}" = "aws_bedrock" ] \
   || [ "${EMBEDDER_PROVIDER:-}" = "aws_bedrock" ]; then
  USES_BEDROCK=1
  REQUIRED_VARS+=(AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY AWS_REGION)
fi

# Interactive setup over REQUIRED_VARS (prompts all with defaults on a first run /
# --reconfigure, blanks only otherwise), plus AWS_SESSION_TOKEN for an ASIA… key.
# Saves to .env.
configure_vars

# A session token belongs only with a temporary (ASIA…) key. Sent alongside a
# long-term key, Bedrock rejects every call, so blank a stale one — left in .env
# after switching keys, or exported in this shell (compose prefers the shell's
# value over --env-file).
if [ -n "${AWS_ACCESS_KEY_ID:-}" ] && [[ "${AWS_ACCESS_KEY_ID}" != ASIA* ]] \
   && [ -n "${AWS_SESSION_TOKEN:-}" ]; then
  warn "clearing AWS_SESSION_TOKEN: AWS_ACCESS_KEY_ID is a long-term key, which takes no session token"
  if grep -q '^AWS_SESSION_TOKEN=.' "${ENV_FILE}"; then set_env_var AWS_SESSION_TOKEN ""; fi
  export AWS_SESSION_TOKEN=""
fi

# Validation is a DEPLOY concern — the --reload/--update subcommands exit early below
# and don't touch these, so skip it for them (otherwise a bad/temp AWS key in .env
# would wrongly block a git-pull or a container restart).
if [ -z "${RELOAD_TARGETS}${UPDATE_TARGETS}" ]; then
  missing=()
  for v in "${REQUIRED_VARS[@]}"; do
    if [ -z "${!v:-}" ]; then missing+=("$v"); fi
  done
  if [ ${#missing[@]} -gt 0 ]; then
    die "missing required values in .env: ${missing[*]}"
  fi
  # Bedrock rejects a temporary key sent without its session token as
  # "UnrecognizedClientException: The security token included in the request is invalid".
  if [ "$USES_BEDROCK" = "1" ] && [[ "${AWS_ACCESS_KEY_ID}" == ASIA* ]] && [ -z "${AWS_SESSION_TOKEN:-}" ]; then
    die "AWS_ACCESS_KEY_ID is a temporary (ASIA…) key but AWS_SESSION_TOKEN is empty in .env; Bedrock needs all three values."
  fi
fi

# Auto-generate the MCP↔api_server shared secret if blank.
if [ -z "${MEKO_MCP_INTERNAL_SECRET:-}" ]; then
  info "generating MEKO_MCP_INTERNAL_SECRET"
  SECRET="$(openssl rand -hex 32)"
  # Replace the empty line in-place so subsequent runs reuse the same secret.
  if grep -q '^MEKO_MCP_INTERNAL_SECRET=$' "${ENV_FILE}"; then
    if [[ "$(uname)" == "Darwin" ]]; then
      sed -i '' "s|^MEKO_MCP_INTERNAL_SECRET=$|MEKO_MCP_INTERNAL_SECRET=${SECRET}|" "${ENV_FILE}"
    else
      sed -i "s|^MEKO_MCP_INTERNAL_SECRET=$|MEKO_MCP_INTERNAL_SECRET=${SECRET}|" "${ENV_FILE}"
    fi
  else
    echo "MEKO_MCP_INTERNAL_SECRET=${SECRET}" >> "${ENV_FILE}"
  fi
  export MEKO_MCP_INTERNAL_SECRET="${SECRET}"
  ok "wrote new MEKO_MCP_INTERNAL_SECRET to .env"
fi

# ─── Resolve + export component source dirs ────────────────────────────────
stage "Resolving component sources & cloning missing repos"
# Each component dir is overridable via .env; unset → the default below
# (the standard sibling layout). Defaults live here, once. We resolve each
# to an absolute path and export it so docker compose interpolation picks it
# up — exported shell vars win over --env-file values, the same pattern used
# for HOST_GATEWAY_IP. compose keeps inline ${VAR:-default} backstops for
# direct `docker compose` invocations that don't go through this script.
resolve_dir() {  # $1 = var name, $2 = default (relative to SCRIPT_DIR)
  local name="$1" val="${!1:-$2}"
  case "$val" in /*) ;; *) val="${SCRIPT_DIR}/${val}" ;; esac
  if [ -d "$val" ]; then val="$(cd "$val" && pwd)"; fi   # normalize only if present
  printf -v "$name" '%s' "$val"
  # shellcheck disable=SC2163  # exporting the var *named by* $name is intentional
  export "$name"
}
resolve_dir MEKO_API_SERVER_DIR        ../../api_server
resolve_dir MEKO_INFERENCE_GATEWAY_DIR ../../inference_gateway
resolve_dir MEKO_UI_DIR                ../../meko_ui
resolve_dir MEKO_LANGFUSE_DIR          ../../../../meko-langfuse
resolve_dir MEKO_MCP_SERVER_DIR        ../../../../meko-mcp-server
resolve_dir MEKO_MEM0_DIR              ../../../../meko-mem0

# ─── Auto-clone the meko-* sibling repos when missing ──────────────────────
# api_server/inference_gateway/meko_ui live in THIS repo; yugabyte-db is built
# or unpacked separately (MEKO_YB_HOME). The other three are sibling repos —
# rather than make a fresh setup clone them by hand, clone any that are missing
# into their resolved dir. Existing checkouts are left untouched (no fetch /
# checkout). Override the host with MEKO_GIT_BASE (e.g. git@github.com:yugabyte
# for SSH); set MEKO_NO_CLONE=1 to disable and manage checkouts yourself.
MEKO_GIT_BASE="${MEKO_GIT_BASE:-https://github.com/yugabyte}"
MEKO_NO_CLONE="${MEKO_NO_CLONE:-0}"

ensure_repo() {  # $1 = dir var name (already resolved), $2 = repo name, $3 = optional branch
  local name="$1" repo="$2" branch="${3:-}" dir="${!1}"
  [ -d "$dir" ] && return 0
  if [ "${MEKO_NO_CLONE}" = "1" ]; then
    die "component dir missing: $dir (MEKO_NO_CLONE=1 — clone ${MEKO_GIT_BASE}/${repo}.git there yourself, or unset MEKO_NO_CLONE)"
  fi
  command -v git >/dev/null 2>&1 || die "git not found on PATH (needed to clone ${repo})"
  local clone_args=()
  [ -n "$branch" ] && clone_args+=(--branch "$branch")
  info "cloning ${repo} → ${dir}${branch:+ (branch ${branch})}"
  git clone "${clone_args[@]}" "${MEKO_GIT_BASE}/${repo}.git" "$dir" \
    || die "git clone failed: git clone ${clone_args[*]} ${MEKO_GIT_BASE}/${repo}.git ${dir} (do you have access to the internal repo?)"
  dir="$(cd "$dir" && pwd)"            # normalize to absolute now that it exists
  printf -v "$name" '%s' "$dir"
  # shellcheck disable=SC2163
  export "$name"
  ok "cloned ${repo}"
}

# Auto-clone only for a full deploy — reload/update act on existing checkouts and
# must never clone anything.
if [ -z "${RELOAD_TARGETS}${UPDATE_TARGETS}" ]; then
  ensure_repo MEKO_LANGFUSE_DIR   meko-langfuse
  ensure_repo MEKO_MCP_SERVER_DIR meko-mcp-server
  # mem0 builds from a feature branch, not main.
  ensure_repo MEKO_MEM0_DIR       meko-mem0 patch/date-aware-extraction
fi

# ─── Rudderstack analytics toggle ──────────────────────────────────────────
# Disabled by default. Set RUDDERSTACK_ENABLED=true in .env to enable. Since
# meko_ui #312 these are RUNTIME env on the meko-ui container (served to the
# browser via /runtime-config.js as window.__MEKO_RUNTIME__ — no rebuild
# needed to flip them). When off (the default), we force the write key empty
# so analytics stays off regardless of any values left in .env — the UI's
# telemetry module no-ops on an empty write key. These exported vars win over
# --env-file during compose interpolation.
case "${RUDDERSTACK_ENABLED:-}" in
  1|true|TRUE|yes|YES|on|ON)
    [ -n "${VITE_RUDDERSTACK_WRITE_KEY:-}" ] \
      || die "RUDDERSTACK_ENABLED is on but VITE_RUDDERSTACK_WRITE_KEY is empty in .env"
    export VITE_RUDDERSTACK_WRITE_KEY
    export VITE_RUDDERSTACK_DATA_PLANE_URL="${VITE_RUDDERSTACK_DATA_PLANE_URL:-}"
    ok "Rudderstack analytics ENABLED (write key set${VITE_RUDDERSTACK_DATA_PLANE_URL:+, custom data plane})"
    ;;
  *)
    export VITE_RUDDERSTACK_WRITE_KEY=""
    export VITE_RUDDERSTACK_DATA_PLANE_URL=""
    info "Rudderstack analytics disabled (set RUDDERSTACK_ENABLED=true in .env to enable)"
    ;;
esac

# --update: git-pull the named repo(s) and exit. Runs before the Docker checks
# because a pull needs only git + resolved dirs; if it then reloads (default), it
# checks for Docker at that point.
if [ -n "${UPDATE_TARGETS}" ]; then
  command -v git >/dev/null 2>&1 || die "git not found on PATH (needed for --update)"
  # shellcheck disable=SC2086  # target names are single words; split into args
  do_update ${UPDATE_TARGETS}
  exit 0
fi

# ─── Step 1: prereq checks ─────────────────────────────────────────────────
stage "Checking prerequisites (Docker, yugabyted)"
command -v docker >/dev/null 2>&1 || die "docker not found on PATH"
# podman-docker's `docker` passes every check below, but rootless `podman build`
# caps open files at 1024 (hard), which fails the meko-ui and langfuse-web
# builds with EMFILE ~20 min in.
if docker --version 2>/dev/null | grep -qi podman; then
  die "'docker' is Podman (podman-docker). This stack needs Docker Engine (Linux) or Docker Desktop."
fi
docker info >/dev/null 2>&1 || die "Docker daemon is not running (open Docker Desktop and retry)"
docker compose version >/dev/null 2>&1 || die "'docker compose' subcommand not available (need Compose v2)"

# --reload: reload the requested component(s) and exit — the rest of the stack is
# assumed up, so we skip yugabyted start and the full deploy. .env, component dirs
# and the docker checks above have already run, which is all a reload needs.
if [ -n "${RELOAD_TARGETS}" ]; then
  # shellcheck disable=SC2086  # canonical targets are single words; split into args
  do_reload ${RELOAD_TARGETS}
  exit 0
fi

[ -x "${MEKO_YB_HOME}/bin/yugabyted" ] || die "yugabyted not executable at ${MEKO_YB_HOME}/bin/yugabyted (did you build the source tree?)"
[ -x "${MEKO_YB_HOME}/bin/ysqlsh" ]    || die "ysqlsh not found at ${MEKO_YB_HOME}/bin/ysqlsh"

# ─── Step 1b: disk space precheck ─────────────────────────────────────────
# yugabyted's tserver rejects every write once its disk is under 5% free
# (reject_writes_min_disk_space_pct=5), and a first build adds ~41 GiB of
# images + build cache (measured on Linux). When both share a disk, a first
# build that starts above the YB floor can end below it, and the Langfuse
# schema load then fails with "insufficient disk space". Bypass with
# MEKO_SKIP_DISK_CHECK=1 for unusual disk layouts.
if [ "${MEKO_SKIP_DISK_CHECK:-0}" != "1" ]; then
  FREE_SPACE_HINT="Free space with 'docker builder prune' (build cache only; keeps images and volumes), then 'docker image prune -a' if needed, and re-run. Bypass with MEKO_SKIP_DISK_CHECK=1."
  # Nearest existing ancestor: YB_BASE_DIR doesn't exist before the first start.
  existing_path() {
    local p="$1"
    while [ ! -e "$p" ] && [ "$p" != "/" ]; do p="$(dirname "$p")"; done
    printf '%s' "$p"
  }
  # Prints "<free GiB> <total GiB> <mount>". -Pk is portable across BSD and GNU df.
  disk_stats() {
    df -Pk "$(existing_path "$1")" | awk 'NR==2 { printf "%d %d %s\n", $4/1048576, $2/1048576, $NF }'
  }

  read -r YB_FREE_GB YB_TOTAL_GB YB_MOUNT <<<"$(disk_stats "${YB_BASE_DIR}")"
  # 5% rounded up, plus headroom for WAL, logs and data growth.
  YB_FLOOR_GB=$(( (YB_TOTAL_GB * 5 + 99) / 100 + 3 ))
  if [ "$YB_FREE_GB" -lt "$YB_FLOOR_GB" ]; then
    die "only ${YB_FREE_GB}G free on ${YB_MOUNT} (yugabyted base_dir); YugabyteDB rejects writes below 5% free, so need at least ${YB_FLOOR_GB}G. ${FREE_SPACE_HINT}"
  fi

  MISSING_IMAGES=0
  for img in meko-api-server meko-mcp-server meko-inference-gateway meko-ui meko-rag-agent langfuse-web langfuse-worker; do
    docker image inspect "${img}:meko-local" >/dev/null 2>&1 || MISSING_IMAGES=$((MISSING_IMAGES + 1))
  done
  DISK_FREE_GB="$YB_FREE_GB"; DISK_MOUNT="$YB_MOUNT"; DISK_WARN_MIN_GB=0
  if [ "$MISSING_IMAGES" -gt 0 ]; then
    DOCKER_ROOT="$(docker info --format '{{.DockerRootDir}}' 2>/dev/null || true)"
    # Docker Desktop's root dir is inside its VM; the VM's disk file lives on the host disk.
    [ -n "$DOCKER_ROOT" ] && [ -d "$DOCKER_ROOT" ] || DOCKER_ROOT="$SCRIPT_DIR"
    read -r DISK_FREE_GB _ DISK_MOUNT <<<"$(disk_stats "${DOCKER_ROOT}")"
    DISK_WARN_MIN_GB=45
    [ "$DISK_MOUNT" = "$YB_MOUNT" ] && DISK_WARN_MIN_GB=$(( DISK_WARN_MIN_GB + YB_FLOOR_GB ))
  fi
  if [ "$DISK_FREE_GB" -lt "$DISK_WARN_MIN_GB" ]; then
    warn "only ${DISK_FREE_GB}G free on ${DISK_MOUNT}; building ${MISSING_IMAGES} missing image(s) needs ~${DISK_WARN_MIN_GB}G (~45G for images + build cache, plus YugabyteDB's 5% floor when on the same disk). The build or the database setup may run out of space. ${FREE_SPACE_HINT}"
    if [ "$ASSUME_YES" != "1" ]; then
      printf "${CYAN}▸${NC} Proceed anyway? [y/N] "
      read -r reply || reply=""
      case "$reply" in
        y|Y) info "proceeding with tight disk";;
        *) info "aborted; free up space and retry"; exit 0;;
      esac
    else
      info "proceeding (--yes set); disk may run out mid-build"
    fi
  fi
fi

# ─── Step 1c: verify component sources ─────────────────────────────────────
# Confirm every component dir exists and show its git branch BEFORE Step 2's
# destructive `yugabyted destroy`. For MEKO_YB_HOME, distinguish a source
# checkout (git work tree → branch) from a release tarball (version_metadata).
git_branch() { git -C "$1" rev-parse --abbrev-ref HEAD 2>/dev/null || echo "(not a git repo)"; }

stage "Verifying component sources"
for entry in \
  "api_server:${MEKO_API_SERVER_DIR}" \
  "inference_gateway:${MEKO_INFERENCE_GATEWAY_DIR}" \
  "meko_ui:${MEKO_UI_DIR}" \
  "meko-langfuse:${MEKO_LANGFUSE_DIR}" \
  "meko-mcp-server:${MEKO_MCP_SERVER_DIR}" \
  "meko-mem0:${MEKO_MEM0_DIR}"; do
  name="${entry%%:*}"; dir="${entry#*:}"
  [ -d "$dir" ] || die "component '$name' dir not found: $dir"
  printf "    %-22s %s  [%s]\n" "$name" "$dir" "$(git_branch "$dir")"
done

# MEKO_YB_HOME already exists (binary-checked above); classify it.
if git -C "${MEKO_YB_HOME}" rev-parse --is-inside-work-tree >/dev/null 2>&1; then
  printf "    %-22s %s  [branch: %s  (cloned/build repo)]\n" \
    "yugabyte (MEKO_YB_HOME)" "${MEKO_YB_HOME}" "$(git_branch "${MEKO_YB_HOME}")"
elif [ -f "${MEKO_YB_HOME}/version_metadata.json" ]; then
  vmeta="${MEKO_YB_HOME}/version_metadata.json"
  # No jq dependency. Per-key grep+sed; tolerant of single- or multi-line JSON.
  ver="$(grep -oE '"version_number"[[:space:]]*:[[:space:]]*"[^"]*"' "$vmeta" | sed -E 's/.*"([^"]*)"$/\1/')"
  build="$(grep -oE '"build_number"[[:space:]]*:[[:space:]]*"[^"]*"' "$vmeta" | sed -E 's/.*"([^"]*)"$/\1/')"
  printf "    %-22s %s  [release tarball — version %s build %s]\n" \
    "yugabyte (MEKO_YB_HOME)" "${MEKO_YB_HOME}" "${ver:-?}" "${build:-?}"
else
  warn "unknown YugabyteDB distribution at ${MEKO_YB_HOME} (no .git, no version_metadata.json)"
fi

# docker-compose.yml mounts this meko-mem0 checkout over the version the MCP
# image installs, so a checkout outside the MCP server's pin runs silently
# ver_lt A B: true if dotted-numeric version A < B. Pure bash; macOS sort may lack -V.
ver_lt() {
  local i a b
  IFS=. read -ra a <<<"$1"
  IFS=. read -ra b <<<"$2"
  for ((i = 0; i < ${#a[@]} || i < ${#b[@]}; i++)); do
    [ "${a[i]:-0}" -lt "${b[i]:-0}" ] && return 0
    [ "${a[i]:-0}" -gt "${b[i]:-0}" ] && return 1
  done
  return 1
}
mem0_ver="$(sed -n 's/^version *= *"\(.*\)"/\1/p' "${MEKO_MEM0_DIR}/pyproject.toml" 2>/dev/null | head -1 || true)"
mem0_pin="$(grep -oE 'meko-mem0>=[0-9.]+,<[0-9.]+' "${MEKO_MCP_SERVER_DIR}/pyproject.toml" 2>/dev/null | head -1 || true)"
if [[ "${mem0_ver}" =~ ^[0-9.]+$ ]] && [ -n "${mem0_pin}" ]; then
  mem0_lo="${mem0_pin#meko-mem0>=}"; mem0_lo="${mem0_lo%%,*}"; mem0_hi="${mem0_pin##*,<}"
  if ver_lt "${mem0_ver}" "${mem0_lo}" || ! ver_lt "${mem0_ver}" "${mem0_hi}"; then
    warn "meko-mem0 checkout is ${mem0_ver}, but meko-mcp-server pins ${mem0_pin#meko-mem0}; the checkout is what runs."
  fi
fi

# Confirmation gate — defaults to abort, runs BEFORE Step 2.
if [ "${ASSUME_YES}" != "1" ]; then
  if [ "${CLEAN}" = "1" ]; then
    printf "${CYAN}▸${NC} Proceed with these sources? --clean will DESTROY existing yugabyted data. [y/N] "
  else
    printf "${CYAN}▸${NC} Proceed with these sources? Existing yugabyted data is preserved (use --clean to wipe). [y/N] "
  fi
  read -r reply || reply=""
  case "$reply" in
    y|Y) ;;
    *) info "aborted; no changes made"; exit 0 ;;
  esac
fi

# ─── Step 2: start yugabyted natively ────────────────────────────────────
stage "Starting YugabyteDB"
# By default existing data is preserved: if yugabyted is already running we
# reuse it, otherwise `start` resumes the existing base_dir. Pass --clean to
# `destroy` the prior state first for a fresh cluster. yugabyted is a
# self-contained wrapper that manages its own master/tserver lifecycle; we
# only invoke `destroy`/`start` and never touch the processes ourselves.

# `yugabyted status` EXITS NON-ZERO whenever YSQL isn't Ready (e.g. a half-up
# instance: Status=Running but YSQL Status=Not Ready). So we must judge by the
# OUTPUT, never the exit code — `... || return 1` on the command itself would
# report "not running/ready" for a process that is very much up, which then
# makes us try to `start` it again (→ "already running!" → set -e abort).
# Hence `|| true` on capture and content greps only. POSIX [[:space:]] (not
# \s) and no \b — those are GNU-grep-only and silently fail under macOS grep.
yb_status_out() { "${MEKO_YB_HOME}/bin/yugabyted" status --base_dir="${YB_BASE_DIR}" 2>/dev/null || true; }

# Is the yugabyted process up at all (Status: Running), regardless of YSQL?
yb_running() {
  yb_status_out | grep -qE "Status[[:space:]]*:[[:space:]]*Running"
}

# Is it fully ready (Status: Running AND YSQL Status: Ready — "Not Ready" must
# not match, which the explicit ":[[:space:]]*Ready" anchor guarantees)?
yb_status_ready() {
  local out; out="$(yb_status_out)"
  printf '%s\n' "$out" | grep -qE "Status[[:space:]]*:[[:space:]]*Running"      || return 1
  printf '%s\n' "$out" | grep -qE "YSQL Status[[:space:]]*:[[:space:]]*Ready"   || return 1
  return 0
}

if [ "${CLEAN}" = "1" ]; then
  info "--clean: destroying any prior yugabyted state (base_dir=${YB_BASE_DIR})"
  "${MEKO_YB_HOME}/bin/yugabyted" destroy --base_dir="${YB_BASE_DIR}" 2>&1 | tail -5 || true
  ok "yugabyted state destroyed"
fi

# Only `start` when the process is genuinely down. If it's already up (whether
# fully Ready or stuck at YSQL Not Ready), starting again just errors
# "yugabyted is already running!" and aborts under set -e — so we skip start
# and let the readiness poll below decide if it's healthy or hung.
if yb_running; then
  ok "yugabyted process already running — not starting a new one (waiting for readiness below)"
else
  # Foreign-listener check: `yugabyted status` says OUR base_dir isn't
  # running, but a DIFFERENT YugabyteDB install (another base_dir / release
  # tree) may still hold the YSQL port — `start` would fail on ports and the
  # readiness poll would time out 180s later with a misleading message, while
  # the docker stack silently talks to the WRONG database. Fail fast instead.
  # `|| true` inside the substitutions: lsof/ss exit non-zero when NOTHING
  # listens (the healthy case) — without it, set -e + pipefail would silently
  # kill the script right here whenever the port is free.
  listeners=""
  if command -v lsof >/dev/null 2>&1; then
    listeners="$(lsof -nP -iTCP:"${YB_PORT}" -sTCP:LISTEN 2>/dev/null | tail -n +2 || true)"
  elif command -v ss >/dev/null 2>&1; then
    listeners="$(ss -ltnp "sport = :${YB_PORT}" 2>/dev/null | tail -n +2 || true)"
  fi
  if [ -n "${listeners}" ]; then
    warn "port ${YB_PORT} is already in use, but yugabyted (base_dir=${YB_BASE_DIR}) is not running:"
    printf '%s\n' "${listeners}" | sed 's/^/    /' >&2
    die "another YugabyteDB/postgres instance holds the YSQL port. Stop it (e.g. <its-yb-home>/bin/yugabyted stop --base_dir=<its-data-dir>) or change YB_PORT in .env, then re-run."
  fi
  # yugabyted's default advertise address is release-specific and on Linux comes
  # from resolving the hostname, which lands on loopback where /etc/hosts maps
  # it there; containers can't reach host loopback via host.docker.internal.
  # Advertise the default-route source address instead (Step 2b maps
  # host.docker.internal to it). macOS/Docker Desktop reaches host loopback, so
  # leave it unset there. `ip route get` reads the routing table only; it fails
  # with no default route, hence `|| true` (set -e + pipefail would exit silently).
  YB_ADVERTISE=""
  if [ "$(uname)" = "Linux" ] && command -v ip >/dev/null 2>&1; then
    YB_ADVERTISE="$(ip route get 1.1.1.1 2>/dev/null \
      | awk '{for (i = 1; i < NF; i++) if ($i == "src") { print $(i + 1); exit }}' || true)"
  fi
  # A resumed base_dir keeps the address it was created with (yugabyted.conf and
  # the master's raft config), so only a new one gets --advertise_address.
  YB_STORED_ADVERTISE="$(sed -n 's/.*"advertise_address"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' \
    "${YB_BASE_DIR}/conf/yugabyted.conf" 2>/dev/null | head -1 || true)"
  if [ -n "${YB_STORED_ADVERTISE}" ]; then
    if [ -n "${YB_ADVERTISE}" ] && [ "${YB_ADVERTISE}" != "${YB_STORED_ADVERTISE}" ]; then
      warn "this base_dir advertises ${YB_STORED_ADVERTISE}, but the host's default-route address is now ${YB_ADVERTISE}; if yugabyted fails to start or containers can't reach it, re-run with --clean"
    fi
    YB_ADVERTISE=""
  fi
  # Meko's migrated schema is ~1,650 tablets (four 128-partition tables), over
  # the per-node cap yugabyted enforces on hosts with less than ~19 GiB RAM
  # (1462 × 10% of the tserver's memory). With tablet_replicas_per_gib_limit=0
  # and the per-core limit at its default of 0 there is no cap. It can't go via
  # enforce_tablet_replica_limits=false: yugabyted appends =true after user
  # flags. Local-dev only; yugabyted.conf keeps it across restarts.
  start_args=(
    --insecure
    "--base_dir=${YB_BASE_DIR}"
    "--master_flags=ysql_yb_enable_mage=true,tablet_replicas_per_gib_limit=0"
    "--tserver_flags=ysql_yb_enable_mage=true"
    "--background=true"
  )
  [ -n "${YB_ADVERTISE}" ] && start_args+=("--advertise_address=${YB_ADVERTISE}")
  # --ysql_port is accepted but hidden from yugabyted's --help; only pass it
  # when moving off the default.
  [ "${YB_PORT}" != "5433" ] && start_args+=("--ysql_port=${YB_PORT}")
  info "starting yugabyted (base_dir=${YB_BASE_DIR}${YB_ADVERTISE:+, advertise=${YB_ADVERTISE}}${YB_STORED_ADVERTISE:+, advertise=${YB_STORED_ADVERTISE} (stored)})"
  "${MEKO_YB_HOME}/bin/yugabyted" start "${start_args[@]}"
  ok "yugabyted start returned"
fi

# Poll `yugabyted status` until both Status and YSQL Status report Ready.
info "waiting for yugabyted Status=Running and YSQL Status=Ready"
for ((i=1; i<=90; i++)); do
  if yb_status_ready; then
    ok "yugabyted Ready (Status=Running, YSQL Status=Ready)"
    break
  fi
  sleep 2
  [ "$i" -eq 90 ] && {
    warn "yugabyted not Ready within 180s — last status output:"
    "${MEKO_YB_HOME}/bin/yugabyted" status --base_dir="${YB_BASE_DIR}" 2>&1 | sed 's/^/    /' >&2
    die "yugabyted didn't reach 'YSQL Status: Ready'. If it's stuck half-up, recreate it: ./stop.sh && ./start.sh --clean"
  }
done

# Advisory: with tablet_replicas_per_gib_limit=0 the tserver may run close
# to its memory limit on small hosts once meko's ~1,650-tablet schema is
# fully migrated. Print a heads-up on machines under 16 GiB so slowness or
# an OOM later isn't a mystery. Warn only — total host RAM is a poor proxy
# for what actually matters (Docker's allocated memory + free RAM at
# runtime), so we don't gate on it.
if [[ "$(uname)" == "Darwin" ]]; then
  HOST_RAM_GIB=$(( $(sysctl -n hw.memsize 2>/dev/null || echo 0) / 1024 / 1024 / 1024 ))
else
  HOST_RAM_GIB=$(( $(awk '/^MemTotal:/ {print $2}' /proc/meminfo 2>/dev/null || echo 0) / 1024 / 1024 ))
fi
if [ "$HOST_RAM_GIB" -gt 0 ] && [ "$HOST_RAM_GIB" -lt 16 ]; then
  warn "host has ${HOST_RAM_GIB}G RAM; with the tablet-cap disabled, yugabyted's tserver may run close to its memory limit under meko's ~1,650-tablet schema. Slowness or OOM here is likely a RAM issue, not a bug."
fi

# ─── Step 2b: figure out where YSQL actually bound ─────────────────────────
# macOS: Docker Desktop routes host.docker.internal to the host and yugabyted
# listens on loopback, so the default host-gateway mapping works and ysqlsh
# reaches 127.0.0.1. Linux: yugabyted binds YSQL to the node's routable LAN IP
# (not loopback), and host.docker.internal:host-gateway points at the docker
# bridge gateway where nothing listens. Detect the address YSQL bound to and
# use it for BOTH the host-side ysqlsh calls below AND the containers'
# host.docker.internal mapping (exported as HOST_GATEWAY_IP, consumed by
# docker-compose.yml). If we can't detect it (e.g. macOS, no `ss`), fall back
# to 127.0.0.1 / host-gateway and preserve the original behaviour.
detect_ysql_host
if [ "${YB_LISTEN_HOST}" != "127.0.0.1" ]; then
  info "YSQL bound to ${YB_LISTEN_HOST}; containers map host.docker.internal → ${YB_LISTEN_HOST}"
fi

# ─── Step 3: create extensions ─────────────────────────────────────────────
stage "Creating database extensions"
info "creating extensions (vector, mage, pg_dist_rag)"
PGPASSWORD="${YB_PASSWORD}" "${MEKO_YB_HOME}/bin/ysqlsh" \
  -h "${YB_LISTEN_HOST}" -p "${YB_PORT}" -U "${YB_USER}" \
  -d yugabyte -v ON_ERROR_STOP=1 \
  -f "${SCRIPT_DIR}/init/yb-extensions.sql"
ok "extensions ready"

# ─── Step 4: docker compose up ─────────────────────────────────────────────
# build then up (equivalent to `up -d --build`) so each half gets its own sub-stage.
stage "Building & starting Docker services"

substage "Building service images (parallel; first run takes a while)"
# List the buildable services so the reader knows what's in flight — they build in
# parallel, so docker's own per-line service tags carry progress from here. (mem0 is
# bind-mounted into meko-mcp-server, not built, so it isn't listed.)
build_svcs="$(dc config 2>/dev/null | awk '
  /^services:/            { in_s=1; next }
  in_s && /^[^[:space:]]/ { in_s=0 }
  in_s && /^  [A-Za-z0-9._-]+:[[:space:]]*$/ { svc=$1; sub(/:$/,"",svc) }
  in_s && /^    build:/    { print svc }
' | tr '\n' ' ' || true)"
[ -n "${build_svcs}" ] && info "building: ${build_svcs}"
dc build
ok "images built"

substage "Creating & starting containers"
dc up -d
ok "containers started"

# ─── Step 5: wait for api_server, then rewrite universe seed ──────────────
stage "Waiting for services & finalizing"
info "waiting for api_server health"
for ((i=1; i<=60; i++)); do
  if curl -fsS "http://127.0.0.1:${API_SERVER_HOST_PORT}/api/v1/health" >/dev/null 2>&1; then
    ok "api_server healthy"
    break
  fi
  sleep 2
  [ "$i" -eq 60 ] && warn "api_server didn't report healthy within 120s — check 'docker compose logs meko-api-server'"
done

info "repointing meko_system.yb_universes seed row to local yugabyted"
PGPASSWORD="${YB_PASSWORD}" "${MEKO_YB_HOME}/bin/ysqlsh" \
  -h "${YB_LISTEN_HOST}" -p "${YB_PORT}" -U "${YB_USER}" \
  -d yugabyte -v ON_ERROR_STOP=1 -v yb_port="${YB_PORT}" \
  -f "${SCRIPT_DIR}/init/yb-fix-universe.sql" >/dev/null
ok "universe row repointed"

# ─── Step 6: print URLs ────────────────────────────────────────────────────
cat <<EOF

${GREEN}━━━ Meko stack is up ━━━${NC}

  UI                  →  http://localhost:${UI_HOST_PORT}/
  Meko API server     →  http://localhost:${API_SERVER_HOST_PORT}/api/v1/
  Inference gateway   →  http://localhost:${INFERENCE_GATEWAY_HOST_PORT}/
  Langfuse (via UI)   →  http://localhost:${UI_HOST_PORT}/observe/
  Langfuse (direct)   →  http://localhost:${UI_HOST_PORT}/observe/   (login: ${LANGFUSE_INIT_USER_EMAIL} / ${LANGFUSE_INIT_USER_PASSWORD})
  MCP server          →  http://localhost:${MCP_SERVER_HOST_PORT}/mcp
  MinIO console       →  http://localhost:${MINIO_CONSOLE_PORT}/    (login: ${MINIO_ROOT_USER} / ${MINIO_ROOT_PASSWORD})

Running on a remote dev-server? Forward these ports from your laptop over SSH
(replace <user>@<dev-server> with the box this stack runs on):

  ssh -N \\
    -L ${UI_HOST_PORT}:localhost:${UI_HOST_PORT} \\
    -L ${API_SERVER_HOST_PORT}:localhost:${API_SERVER_HOST_PORT} \\
    -L ${INFERENCE_GATEWAY_HOST_PORT}:localhost:${INFERENCE_GATEWAY_HOST_PORT} \\
    -L ${MCP_SERVER_HOST_PORT}:localhost:${MCP_SERVER_HOST_PORT} \\
    -L ${MINIO_API_PORT}:localhost:${MINIO_API_PORT} \\
    -L ${MINIO_CONSOLE_PORT}:localhost:${MINIO_CONSOLE_PORT} \\
    <user>@<dev-server>

Then open http://localhost:${UI_HOST_PORT}/ on your laptop.

No sign-in user yet? Create one (needs AWS auth to the Cognito pool):
          ${SCRIPT_DIR}/create-cognito-user.sh

Logs:     docker compose -f ${SCRIPT_DIR}/docker-compose.yml logs -f <service>
Restart:  docker compose -f ${SCRIPT_DIR}/docker-compose.yml restart <service>
Stop:     ${SCRIPT_DIR}/stop.sh

EOF
