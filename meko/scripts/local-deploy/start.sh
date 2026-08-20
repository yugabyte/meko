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
# Flags:
#   -y, --yes     skip the pre-deploy confirmation prompt (also: MEKO_ASSUME_YES=1).
#   -c, --clean   DESTROY existing yugabyted data and start fresh (also: MEKO_CLEAN=1).
#                 Without it, existing data is preserved/resumed.

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

# ─── Arg parsing ──────────────────────────────────────────────────────────
ASSUME_YES="${MEKO_ASSUME_YES:-0}"
CLEAN="${MEKO_CLEAN:-0}"
for arg in "$@"; do
  case "$arg" in
    -y|--yes) ASSUME_YES=1 ;;
    -c|--clean) CLEAN=1 ;;
    -h|--help) sed -n '2,19p' "$0"; exit 0 ;;
    *) die "unknown flag: $arg (use -y/--yes, -c/--clean)" ;;
  esac
done

# ─── Step 0: .env handling ────────────────────────────────────────────────
if [ ! -f "${ENV_FILE}" ]; then
  info "creating .env from .env.example"
  cp "${ENV_EXAMPLE}" "${ENV_FILE}"
  die ".env created. Fill in the required values (Cognito, OPENAI_API_KEY, MEKO_YB_HOME) and re-run."
fi

# Load .env so checks below can read it. set -a auto-exports.
set -a
# shellcheck source=/dev/null
. "${ENV_FILE}"
set +a

REQUIRED_VARS=(
  COGNITO_REGION COGNITO_POOL_ID COGNITO_CLIENT_ID COGNITO_CLIENT_SECRET
  OPENAI_API_KEY
  AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY AWS_REGION
  MEKO_YB_HOME YB_BASE_DIR YB_HOST YB_PORT YB_USER
  UI_HOST_PORT API_SERVER_HOST_PORT MCP_SERVER_HOST_PORT INFERENCE_GATEWAY_HOST_PORT
)
missing=()
for v in "${REQUIRED_VARS[@]}"; do
  if [ -z "${!v:-}" ]; then missing+=("$v"); fi
done
if [ ${#missing[@]} -gt 0 ]; then
  die "missing required values in .env: ${missing[*]}"
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

ensure_repo MEKO_LANGFUSE_DIR   meko-langfuse meko-embed-ui-setup
ensure_repo MEKO_MCP_SERVER_DIR meko-mcp-server
ensure_repo MEKO_MEM0_DIR       meko-mem0

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

# ─── Step 1: prereq checks ─────────────────────────────────────────────────
command -v docker >/dev/null 2>&1 || die "docker not found on PATH"
docker info >/dev/null 2>&1 || die "Docker daemon is not running (open Docker Desktop and retry)"
docker compose version >/dev/null 2>&1 || die "'docker compose' subcommand not available (need Compose v2)"

[ -x "${MEKO_YB_HOME}/bin/yugabyted" ] || die "yugabyted not executable at ${MEKO_YB_HOME}/bin/yugabyted (did you build the source tree?)"
[ -x "${MEKO_YB_HOME}/bin/ysqlsh" ]    || die "ysqlsh not found at ${MEKO_YB_HOME}/bin/ysqlsh"

# ─── Step 1c: verify component sources ─────────────────────────────────────
# Confirm every component dir exists and show its git branch BEFORE Step 2's
# destructive `yugabyted destroy`. For MEKO_YB_HOME, distinguish a source
# checkout (git work tree → branch) from a release tarball (version_metadata).
git_branch() { git -C "$1" rev-parse --abbrev-ref HEAD 2>/dev/null || echo "(not a git repo)"; }

info "Verifying component sources"
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
  info "starting yugabyted (base_dir=${YB_BASE_DIR})"
  "${MEKO_YB_HOME}/bin/yugabyted" start \
    --insecure \
    --base_dir="${YB_BASE_DIR}" \
    --master_flags="ysql_yb_enable_mage=true" \
    --tserver_flags="ysql_yb_enable_mage=true" \
    --background=true
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
YB_LISTEN_HOST="127.0.0.1"
if command -v ss >/dev/null 2>&1; then
  detected="$(ss -ltnH "sport = :${YB_PORT}" 2>/dev/null \
    | awk '{print $4}' | sed -E 's/:[0-9]+$//' \
    | grep -vE '^(127\.|0\.0\.0\.0$|\[?::1?\]?$|\*$)' | head -1)"
  if [ -n "${detected}" ]; then
    YB_LISTEN_HOST="${detected}"
    export HOST_GATEWAY_IP="${detected}"
    info "YSQL bound to ${YB_LISTEN_HOST}; containers map host.docker.internal → ${YB_LISTEN_HOST}"
  fi
fi

# ─── Step 3: create extensions ─────────────────────────────────────────────
info "creating extensions (vector, mage, pg_dist_rag)"
PGPASSWORD="${YB_PASSWORD}" "${MEKO_YB_HOME}/bin/ysqlsh" \
  -h "${YB_LISTEN_HOST}" -p "${YB_PORT}" -U "${YB_USER}" \
  -d yugabyte -v ON_ERROR_STOP=1 \
  -f "${SCRIPT_DIR}/init/yb-extensions.sql"
ok "extensions ready"

# ─── Step 4: docker compose up ─────────────────────────────────────────────
info "building + starting docker compose stack (first run takes a while)"
docker compose --env-file "${ENV_FILE}" \
  -f "${SCRIPT_DIR}/docker-compose.yml" \
  up -d --build

# ─── Step 5: wait for api_server, then rewrite universe seed ──────────────
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
  -d yugabyte -v ON_ERROR_STOP=1 \
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

Logs:     docker compose -f ${SCRIPT_DIR}/docker-compose.yml logs -f <service>
Restart:  docker compose -f ${SCRIPT_DIR}/docker-compose.yml restart <service>
Stop:     ${SCRIPT_DIR}/stop.sh

EOF
