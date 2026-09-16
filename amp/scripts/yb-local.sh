#!/usr/bin/env bash
#
# yb-local.sh — start/stop a local single-node YugabyteDB for amp development.
#
# amp's integration harness (test/integration/compute/run.sh) and the default dev cluster expect
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
#   YB_TLS        1 = TLS on both YSQL (5433) and tserver RPC (9100), with
#                 certificate authentication for the `yugabyte` role and
#                 plaintext still accepted (the permissive transition shape:
#                 hba `hostssl all all all cert` + `host all all all trust`,
#                 allow_insecure_connections=true). Uses host networking so
#                 the tserver RPC port is reachable at 127.0.0.1 too.
#   YB_TLS_PORT_OFFSET  added to every port in TLS mode (YSQL 5433, tserver
#                 RPC 9100, master RPC 7100, web 7000/9000, UI 15433, YCQL
#                 9042) so a TLS instance can sit beside a plain one on the
#                 same host. The harness reads YB_PORT and YB_RPC_PORT.
#   YB_TLS_DIR    where ca.crt / client.crt / client.key land on the host
#                 (default: .yb-local-tls next to this repo's amp/). Point the
#                 daemons at them: sslmode=verify-full sslrootcert=$YB_TLS_DIR/ca.crt
#                 sslcert=$YB_TLS_DIR/client.crt sslkey=$YB_TLS_DIR/client.key
#
set -euo pipefail

# Keep this default in sync with .github/workflows/amp-pr-build.yml (env: YB_IMAGE),
# so a local dev box and CI run byte-identical YugabyteDB builds.
YB_IMAGE="${YB_IMAGE:-yugabytedb/yugabyte:2026.1.0.0-b118}"
YB_PORT="${YB_PORT:-5433}"
YB_CONTAINER="${YB_CONTAINER:-amp-yb}"
YB_TLS="${YB_TLS:-0}"
YB_TLS_DIR="${YB_TLS_DIR:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/.yb-local-tls}"
YB_TLS_PORT_OFFSET="${YB_TLS_PORT_OFFSET:-0}"
if [ "$YB_TLS" = 1 ]; then
    YB_PORT=$((5433 + YB_TLS_PORT_OFFSET))
fi

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
    # sslmode=disable: in TLS mode the `hostssl ... cert` rule would demand a
    # client certificate the probe does not carry, and libpq's default
    # `prefer` tries TLS first.
    if [ "$YB_TLS" = 1 ]; then
        docker exec "$YB_CONTAINER" sh -c \
            "bin/ysqlsh 'host=127.0.0.1 port=${YB_PORT} user=yugabyte dbname=yugabyte sslmode=disable' -tAc 'select 1'" \
            >/dev/null 2>&1
    else
        docker exec "$YB_CONTAINER" sh -c \
            'bin/ysqlsh -h "$(hostname -i)" -p 5433 -U yugabyte -d yugabyte -tAc "select 1"' \
            >/dev/null 2>&1
    fi
}

# TLS mode: generate the server certs for 127.0.0.1 and a client cert for the
# `yugabyte` role inside the container, then start yugabyted with TLS on both
# the RPC and YSQL ports while still admitting plaintext. Host networking puts
# YSQL 5433, tserver RPC 9100 and master 7100 all on 127.0.0.1, which is also
# the only name the server certificate carries.
start_tls() {
    local off="$YB_TLS_PORT_OFFSET"
    local ports="--ysql_port=$((5433 + off)) --ycql_port=$((9042 + off)) \
--master_rpc_port=$((7100 + off)) --tserver_rpc_port=$((9100 + off)) \
--master_webserver_port=$((7000 + off)) --tserver_webserver_port=$((9000 + off)) \
--webserver_port=$((15433 + off))"
    docker run -d --name "$YB_CONTAINER" --network host -e "YB_PORTS=$ports" "$YB_IMAGE" bash -c '
        set -euo pipefail
        # Certificates are minted here rather than with `yugabyted cert`: that
        # tool writes the IP as a DNS SAN, which rustls (the daemons YSQL lane)
        # will not match for an IP host. yugabyted also probes the master with
        # yb-admin as node.<hostname>, and under host networking the hostname is
        # the box itself, so the server cert doubles as that client cert.
        # -extfile rather than -addext/-copy_extensions: the image ships OpenSSL 1.1.
        me=$(hostname -f 2>/dev/null || hostname)
        certs="$HOME/var/tls-certs"
        mkdir -p "$certs" /tmp/tls
        openssl req -x509 -new -newkey rsa:2048 -nodes -days 3650 -subj "/CN=yb-local CA" \
            -keyout "$certs/ca.key" -out "$certs/ca.crt" 2>/dev/null
        openssl req -new -newkey rsa:2048 -nodes -subj "/CN=127.0.0.1" \
            -keyout "$certs/node.127.0.0.1.key" -out /tmp/tls/node.csr 2>/dev/null
        printf "subjectAltName=IP:127.0.0.1,DNS:127.0.0.1,DNS:localhost,DNS:%s\nextendedKeyUsage=serverAuth,clientAuth\n" "$me" > /tmp/tls/node.ext
        openssl x509 -req -in /tmp/tls/node.csr -CA "$certs/ca.crt" -CAkey "$certs/ca.key" \
            -CAcreateserial -days 365 -extfile /tmp/tls/node.ext -out "$certs/node.127.0.0.1.crt" 2>/dev/null
        cp "$certs/node.127.0.0.1.crt" "$certs/node.$me.crt"
        cp "$certs/node.127.0.0.1.key" "$certs/node.$me.key"
        printf "extendedKeyUsage=clientAuth\n" > /tmp/tls/client.ext
        openssl req -new -newkey rsa:2048 -nodes -subj "/CN=yugabyte" \
            -keyout /tmp/tls/client.key -out /tmp/tls/client.csr 2>/dev/null
        openssl x509 -req -in /tmp/tls/client.csr -CA "$certs/ca.crt" -CAkey "$certs/ca.key" \
            -CAcreateserial -days 365 -extfile /tmp/tls/client.ext -out /tmp/tls/client.crt 2>/dev/null
        rm -f /tmp/tls/*.csr /tmp/tls/*.ext
        # ca.key travels too so a test can re-issue the client cert
        # (tls_cert_reload) against the CA the tservers trust.
        cp "$certs/ca.crt" /tmp/tls/ca.crt
        cp "$certs/ca.key" /tmp/tls/ca.key
        chmod 600 "$certs"/*.key /tmp/tls/client.key /tmp/tls/ca.key
        chmod 644 /tmp/tls/ca.crt /tmp/tls/client.crt
        common="use_node_to_node_encryption=true,certs_dir=$certs,node_to_node_encryption_use_client_certificates=true,allow_insecure_connections=true"
        # shellcheck disable=SC2086  # YB_PORTS is a deliberate word list
        exec bin/yugabyted start --advertise_address=127.0.0.1 --background=false $YB_PORTS \
            --master_flags="$common" \
            --tserver_flags="$common,use_client_to_server_encryption=true,certs_for_client_dir=$certs,ysql_hba_conf_csv={hostssl all all all cert,host all all all trust}"
    ' >/dev/null
    wait_ready
    mkdir -p "$YB_TLS_DIR"
    docker cp "$YB_CONTAINER:/tmp/tls/." "$YB_TLS_DIR/"
    chmod 600 "$YB_TLS_DIR/client.key" "$YB_TLS_DIR/ca.key"
    echo "yb-local: TLS material in $YB_TLS_DIR (ca.crt + ca.key, client.crt CN=yugabyte, client.key)"
    echo "yb-local: YSQL 127.0.0.1:${YB_PORT}, tserver RPC 127.0.0.1:$((9100 + off)); for the harness:"
    echo "  YB_PORT=${YB_PORT} YB_RPC_PORT=$((9100 + off)) YB_SSLMODE=verify-full \\"
    echo "  YB_SSLROOTCERT=$YB_TLS_DIR/ca.crt YB_SSLCERT=$YB_TLS_DIR/client.crt YB_SSLKEY=$YB_TLS_DIR/client.key"
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
    # yugabyted draws a spinner with carriage returns; split it so the log stays readable.
    docker logs --tail 50 "$YB_CONTAINER" 2>&1 | tr '\r' '\n' | grep -v 'Starting the YugabyteDB Processes' | tail -40 >&2 || true
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
            if [ "$YB_TLS" = 1 ]; then
                start_tls
                return
            fi
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
