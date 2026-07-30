#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
COMPOSE_DIR="${SCRIPT_DIR}/docs/docker"
COMPOSE_FILE="${COMPOSE_DIR}/docker-compose.yml"

# Build dynamic volume args for git/gh credentials
build_credential_mounts() {
    local args=()
    if [ -f "${HOME}/.gitconfig" ]; then
        args+=(-v "${HOME}/.gitconfig:/root/.gitconfig:ro")
    fi
    if [ -d "${HOME}/.config/gh" ]; then
        args+=(-v "${HOME}/.config/gh:/root/.config/gh:ro")
    fi
    echo "${args[@]:-}"
}

compose() {
    docker compose -f "${COMPOSE_FILE}" "$@"
}

case "${1:-start}" in
    start)
        echo "Building and starting meko docs dev server..."
        CRED_MOUNTS=$(build_credential_mounts)
        # shellcheck disable=SC2086
        docker compose -f "${COMPOSE_FILE}" run --rm --service-ports \
            ${CRED_MOUNTS} docs
        ;;
    stop)
        compose down
        ;;
    shell)
        CRED_MOUNTS=$(build_credential_mounts)
        # shellcheck disable=SC2086
        docker compose -f "${COMPOSE_FILE}" run --rm \
            ${CRED_MOUNTS} docs bash
        ;;
    setup-fork)
        CRED_MOUNTS=$(build_credential_mounts)
        # shellcheck disable=SC2086
        docker compose -f "${COMPOSE_FILE}" run --rm \
            ${CRED_MOUNTS} docs setup-fork
        ;;
    build)
        compose run --rm docs npm run build
        ;;
    clean)
        echo "Removing containers, volumes, and local images..."
        compose down -v --rmi local
        ;;
    logs)
        compose logs -f docs
        ;;
    *)
        echo "Usage: $0 {start|stop|shell|setup-fork|build|clean|logs}"
        exit 1
        ;;
esac
