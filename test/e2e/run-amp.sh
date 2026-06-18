#!/usr/bin/env bash
#
# amp-on-Docker end-to-end entry point. Starts the amp control plane as a
# container (with its Docker endpoint provisioner enabled) and runs the thin e2e
# suite against it. Provisioning lives in amp, across two REST calls: create the
# timeline, then create an endpoint on it — amp deploys the compute container
# synchronously (via the Docker provisioner, over the daemon socket mounted here)
# and returns its address. The e2e only calls amp's REST API and runs psql
# against that address.
#
# This drives the SC-free self-bootstrap path: amp runs the endpoint with
# --deploy-bootstrap, so it provisions the storage schema and initdb's the
# recorded tenant/timeline on first start (there is no create-database flow yet).
# Storage-controller-coordinated deploy is future work.
#
# Prereqs: Docker, and a YugabyteDB on the host at :5433 (yugabyte/yugabyte).
# Builds yb-light/amp:dev locally; requires the endpoint image
# yb-light/neon-endpoint:dev to already exist.
set -euo pipefail

usage() {
    cat <<'EOF'
Usage: test/e2e/run-amp.sh [-n <count>] [-h|--help]

Starts amp with the Docker endpoint provisioner and runs the amp e2e suite
(create user -> storage pool -> timeline -> endpoint, then psql the endpoint).

  -n <count>    repeat the suite <count> times (forwarded to the harness).

Builds yb-light/amp:dev locally each run. Requires yb-light/neon-endpoint:dev and
a YugabyteDB on the host (:5433).
EOF
}

ARGS=()
for arg in "$@"; do
    case "$arg" in
        -h|--help) usage; exit 0 ;;
        *) ARGS+=("$arg") ;;
    esac
done

REPO=$(git rev-parse --show-toplevel)
cd "$REPO"

AMP_IMAGE=yb-light/amp:dev
ENDPOINT_IMAGE=yb-light/neon-endpoint:dev

# YB admin connection, for creating/dropping the per-run databases. We exec
# ysqlsh inside the YB container (the host usually has no psql); yugabyted binds
# the container's own IP rather than loopback, so resolve that IP instead of
# hardcoding a bridge address. Override the container name with YB_CONTAINER.
YB_CONTAINER=${YB_CONTAINER:-yugabyte}
YB_IP=$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "$YB_CONTAINER" 2>/dev/null)
[ -n "$YB_IP" ] || { echo "error: could not resolve the IP of container '$YB_CONTAINER' (is it running?)" >&2; exit 1; }
yb() { docker exec "$YB_CONTAINER" ysqlsh -h "$YB_IP" -p 5433 -U yugabyte -d yugabyte "$@"; }

RUN_ID="$$_$(date +%s)"
NET="ybamp-e2e-${RUN_ID}"
AMP_NAME="yb-amp-e2e-${RUN_ID}"
CTL_DB="amp_ctl_e2e_${RUN_ID}"
STORE_DB="amp_store_e2e_${RUN_ID}"
# A free host port for amp's REST API (no pipeline, to avoid SIGPIPE under pipefail).
AMP_HOST_PORT=$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()')

cleanup() {
    echo "==> teardown"
    # endpoints amp launched (joined to our network) + the amp container itself
    docker ps -aq --filter "network=${NET}" 2>/dev/null | xargs -r docker rm -f >/dev/null 2>&1 || true
    docker rm -f "$AMP_NAME" >/dev/null 2>&1 || true
    docker network rm "$NET" >/dev/null 2>&1 || true
    for db in "$STORE_DB" "$CTL_DB"; do
        yb -c "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname='$db'" >/dev/null 2>&1 || true
        yb -c "DROP DATABASE IF EXISTS \"$db\"" >/dev/null 2>&1 || true
    done
}
trap cleanup EXIT

echo "==> Building the amp bundle"
make amp-bundle-amp-only
echo "==> Building the amp image ($AMP_IMAGE)"
DOCKER_BUILDKIT=1 docker build -t "$AMP_IMAGE" -f deploy/amp/Dockerfile .

if ! docker image inspect "$ENDPOINT_IMAGE" >/dev/null 2>&1; then
    echo "error: endpoint image $ENDPOINT_IMAGE not found. Build it with:" >&2
    echo "  docker build -t $ENDPOINT_IMAGE -f deploy/endpoint/Dockerfile ." >&2
    exit 1
fi

echo "==> Building the e2e harness"
cargo build --bin e2e_tests

echo "==> Creating network $NET + databases ($CTL_DB, $STORE_DB)"
docker network create "$NET" >/dev/null
yb -c "CREATE DATABASE \"$CTL_DB\"" >/dev/null
yb -c "CREATE DATABASE \"$STORE_DB\"" >/dev/null

echo "==> Starting amp ($AMP_NAME) on port $AMP_HOST_PORT (docker runtime)"
# --user root + the mounted socket let amp's Docker provisioner launch endpoints;
# the endpoints join $NET so amp and the client can reach them.
docker run -d --name "$AMP_NAME" --hostname "$AMP_NAME" --network "$NET" \
    --user root \
    -p "${AMP_HOST_PORT}:8080" \
    --add-host host.docker.internal:host-gateway \
    -v /var/run/docker.sock:/var/run/docker.sock \
    -e "AMP_DATABASE_URL=postgres://yugabyte@host.docker.internal:5433/${CTL_DB}" \
    -e AMP_LOG_FORMAT=text \
    -e AMP_DEPLOY=true \
    -e AMP_DEPLOY_DRIVER=docker \
    -e AMP_DEPLOY_NETWORK="$NET" \
    -e AMP_DEPLOY_ENDPOINT_IMAGE="$ENDPOINT_IMAGE" \
    -e AMP_DEPLOY_EXTERNAL_HOST=127.0.0.1 \
    -e AMP_DEPLOY_BOOTSTRAP=true \
    -e AMP_DEPLOY_YB_HOST_PORTS=host.docker.internal:5433 \
    -e AMP_DEPLOY_YB_DBNAME="$STORE_DB" \
    "$AMP_IMAGE" >/dev/null

echo "==> Waiting for amp to be ready"
for i in $(seq 1 60); do
    if curl -sf -m2 "http://127.0.0.1:${AMP_HOST_PORT}/readyz" >/dev/null 2>&1; then break; fi
    if ! docker ps -q --filter "name=$AMP_NAME" | grep -q .; then
        echo "amp exited; logs:" >&2; docker logs "$AMP_NAME" 2>&1 | tail -30 >&2; exit 1
    fi
    sleep 1
done

echo "==> Running the amp-on-Docker suite"
AMP_BASE_URL="http://127.0.0.1:${AMP_HOST_PORT}" "$REPO/target/debug/e2e_tests" amp ${ARGS[@]+"${ARGS[@]}"}
