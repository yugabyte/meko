#!/usr/bin/env bash
# Tear down the local Meko stack.
#   - Docker compose: down (containers + networks, keeps named volumes by default).
#   - yugabyted: stop natively if it's running.
#
# Flags:
#   --volumes   also remove the named docker volumes (MinIO + Redis data).
#   --keep-yb   leave yugabyted running.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ENV_FILE="${SCRIPT_DIR}/.env"

REMOVE_VOLUMES=false
KEEP_YB=false
for arg in "$@"; do
  case "$arg" in
    --volumes) REMOVE_VOLUMES=true ;;
    --keep-yb) KEEP_YB=true ;;
    -h|--help)
      sed -n '2,9p' "$0"; exit 0 ;;
    *) echo "unknown flag: $arg" >&2; exit 2 ;;
  esac
done

if [ -f "${ENV_FILE}" ]; then
  set -a
  # shellcheck source=/dev/null
  . "${ENV_FILE}"
  set +a
fi

# Docker compose down.
DOWN_FLAGS=()
$REMOVE_VOLUMES && DOWN_FLAGS+=(--volumes)
docker compose --env-file "${ENV_FILE}" \
  -f "${SCRIPT_DIR}/docker-compose.yml" \
  down "${DOWN_FLAGS[@]}"

# yugabyted stop.
if ! $KEEP_YB; then
  if [ -n "${MEKO_YB_HOME:-}" ] && [ -x "${MEKO_YB_HOME}/bin/yugabyted" ] && [ -n "${YB_BASE_DIR:-}" ]; then
    # Capture the output first (|| true): `yugabyted status` exits non-zero when
    # YSQL isn't Ready, and under `set -o pipefail` that would override grep's
    # match and make us skip stopping a running-but-unhealthy instance.
    yb_status_out="$("${MEKO_YB_HOME}/bin/yugabyted" status --base_dir="${YB_BASE_DIR}" 2>&1 || true)"
    if printf '%s\n' "${yb_status_out}" | grep -qE "Status[[:space:]]*:[[:space:]]*Running"; then
      echo "stopping yugabyted (base_dir=${YB_BASE_DIR})"
      "${MEKO_YB_HOME}/bin/yugabyted" stop --base_dir="${YB_BASE_DIR}"
    else
      echo "yugabyted not running, nothing to stop"
    fi
  fi
fi

echo "done."
