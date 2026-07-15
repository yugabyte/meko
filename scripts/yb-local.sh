#!/usr/bin/env bash
#
# yb-local.sh — start/stop a local single-node YugabyteDB for amp development.
#
# amp's e2e harness (test/e2e/storage/run.sh) and the default dev cluster expect
# a YugabyteDB reachable at 127.0.0.1:5433 / db `yb_storage`. This runs the same
# pinned image CI uses (see .github/workflows/pr-build.yml), so local and CI
# exercise the identical YB build — one command, no manual tarball install.
#
# Usage:
#   scripts/yb-local.sh start     # run (first time pulls ~1GB) + wait until ready
#   scripts/yb-local.sh stop      # stop and remove the container
#   scripts/yb-local.sh status    # is it up and accepting connections?
#   scripts/yb-local.sh logs      # follow container logs
#
# Env overrides:
#   YB_IMAGE      container image (default: the CI-pinned release below)
#   YB_PORT       host port to publish YSQL on (default: 5433)
#   YB_CONTAINER  container name (default: amp-yb)
#
set -euo pipefail

# Keep this default in sync with .github/workflows/pr-build.yml (env: YB_IMAGE),
# so a local dev box and CI run byte-identical YugabyteDB builds.
YB_IMAGE="${YB_IMAGE:-yugabytedb/yugabyte:2026.1.0.0-b118}"
YB_PORT="${YB_PORT:-5433}"
YB_CONTAINER="${YB_CONTAINER:-amp-yb}"

die() { echo "yb-local: $*" >&2; exit 1; }

preflight() {
    command -v docker >/dev/null 2>&1 \
        || die "docker not found on PATH. Install Docker, then re-run."
    if ! docker info >/dev/null 2>&1; then
        cat >&2 <<'EOF'
yb-local: cannot talk to the Docker daemon. Usually one of:
  * daemon not running:
        sudo systemctl enable --now docker
  * your user can't reach the socket — add yourself to the 'docker' group:
        sudo usermod -aG docker "$USER"     # then log out/in, or: newgrp docker
EOF
        exit 1
    fi
}

# Prints one of: running | exited | created | missing | ...
# NB: `docker inspect -f` prints a blank line to stdout (not just stderr) for a
# missing container and exits non-zero, so capture, strip whitespace, and treat
# empty as "missing" — otherwise the caller's `case` sees a stray newline.
container_state() {
    local st
    st="$(docker inspect -f '{{.State.Status}}' "$YB_CONTAINER" 2>/dev/null)" || true
    st="${st//[[:space:]]/}"
    printf '%s' "${st:-missing}"
}

# A YSQL probe from inside the container — no host-side psql/pg_isready needed.
# `yugabyted start` binds YSQL to the container's advertised (eth0) address, not
# loopback, so connect to `hostname -i` rather than 127.0.0.1.
ysql_ok() {
    docker exec "$YB_CONTAINER" sh -c \
        'bin/ysqlsh -h "$(hostname -i)" -p 5433 -U yugabyte -d yugabyte -tAc "select 1"' \
        >/dev/null 2>&1
}

wait_ready() {
    echo "yb-local: waiting for YugabyteDB on 127.0.0.1:${YB_PORT} ..."
    for _ in $(seq 1 120); do
        if ysql_ok; then
            echo "yb-local: ready — postgresql://yugabyte@127.0.0.1:${YB_PORT}/yugabyte"
            return 0
        fi
        sleep 2
    done
    echo "yb-local: YugabyteDB never became ready; recent logs:" >&2
    docker logs --tail 50 "$YB_CONTAINER" >&2 || true
    return 1
}

cmd_start() {
    preflight
    case "$(container_state)" in
        running)
            echo "yb-local: '${YB_CONTAINER}' already running."
            wait_ready
            ;;
        exited)
            echo "yb-local: starting existing '${YB_CONTAINER}' ..."
            docker start "$YB_CONTAINER" >/dev/null
            wait_ready
            ;;
        missing)
            echo "yb-local: launching ${YB_IMAGE} (first run pulls ~1GB) ..."
            # Bind loopback only (a dev box shouldn't expose YB to the network);
            # single-node/RF=1, same topology as CI.
            docker run -d --name "$YB_CONTAINER" -p "127.0.0.1:${YB_PORT}:5433" \
                "$YB_IMAGE" bin/yugabyted start --background=false >/dev/null
            wait_ready
            ;;
    esac
}

cmd_stop() {
    preflight
    if [ "$(container_state)" = missing ]; then
        echo "yb-local: '${YB_CONTAINER}' not present."
        return 0
    fi
    docker rm -f "$YB_CONTAINER" >/dev/null
    echo "yb-local: removed '${YB_CONTAINER}'."
}

cmd_status() {
    preflight
    local st
    st="$(container_state)"
    echo "container=${YB_CONTAINER} state=${st} image=${YB_IMAGE} host_port=${YB_PORT}"
    [ "$st" = running ] || return 0
    if ysql_ok; then
        echo "ysql: accepting connections on 127.0.0.1:${YB_PORT}"
    else
        echo "ysql: container up but not accepting connections yet"
    fi
}

cmd_logs() {
    preflight
    docker logs -f --tail 100 "$YB_CONTAINER"
}

case "${1:-}" in
    start)  cmd_start ;;
    stop)   cmd_stop ;;
    status) cmd_status ;;
    logs)   cmd_logs ;;
    *)      echo "usage: $0 {start|stop|status|logs}" >&2; exit 2 ;;
esac
