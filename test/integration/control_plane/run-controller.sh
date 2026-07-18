#!/usr/bin/env bash
#
# controller-on-Docker integration entry point. Starts the controller control plane as a
# container (with its Docker endpoint provisioner enabled) and runs the thin integration
# suite against it. Provisioning lives in controller: the suite drives its REST API
# (create a project, fork branches off it, then create an endpoint on each branch) —
# controller deploys the compute container synchronously (via the Docker provisioner,
# over the daemon socket mounted here) and returns its address. The integration only calls
# controller's REST API and runs Postgres against those addresses; it validates
# copy-on-write branch isolation (see test/integration/control_plane/README.md).
#
# The controller provisions the shared DB's schema (dataplane + registry) on
# startup, then deploys each endpoint with BOOTSTRAP=true so the endpoint initdb's
# the recorded tenant/timeline on first start (there is no create-database flow yet).
#
# Prereqs: Docker, and a YugabyteDB on the host at :5433 (yugabyte/yugabyte).
# Builds yb-light/controller:dev locally; requires the endpoint image
# yb-light/neon-endpoint:dev to already exist.
set -euo pipefail

usage() {
    cat <<'EOF'
Usage: test/integration/control_plane/run-controller.sh [-n <count>] [-h|--help]

Starts controller with the Docker endpoint provisioner and runs the controller integration suite
(create user -> storage pool -> project -> branch tree -> endpoints, then validate
copy-on-write branch isolation over Postgres).

  -n <count>    repeat the scenario <count> times, fresh project each (forwarded to the harness).

Builds yb-light/controller:dev locally each run. Requires yb-light/neon-endpoint:dev and
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

AMP_IMAGE=yb-light/controller:dev
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
NET="ybamp-integration-${RUN_ID}"
AMP_NAME="yb-amp-integration-${RUN_ID}"
# One database, as in production: helm points both the controller's
# AMP_DATABASE_URL and the endpoints' AMP_DEPLOY_YB_DBNAME at the same
# `yb_storage`. The controller provisions the dataplane (public/pageserver/
# safekeeper) + registry schemas into its own DB, and the endpoints read the
# dataplane from that same DB.
DB="amp_integration_${RUN_ID}"
# A free host port for controller's REST API (no pipeline, to avoid SIGPIPE under pipefail).
AMP_HOST_PORT=$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()')

cleanup() {
    echo "==> teardown"
    # endpoints controller launched (joined to our network) + the controller container itself
    docker ps -aq --filter "network=${NET}" 2>/dev/null | xargs -r docker rm -f >/dev/null 2>&1 || true
    docker rm -f "$AMP_NAME" >/dev/null 2>&1 || true
    docker network rm "$NET" >/dev/null 2>&1 || true
    yb -c "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname='$DB'" >/dev/null 2>&1 || true
    yb -c "DROP DATABASE IF EXISTS \"$DB\"" >/dev/null 2>&1 || true
}
trap cleanup EXIT

echo "==> Building the controller bundle"
make controller-bundle-only
echo "==> Building the controller image ($AMP_IMAGE)"
DOCKER_BUILDKIT=1 docker build -t "$AMP_IMAGE" -f deploy/controller/Dockerfile .

if ! docker image inspect "$ENDPOINT_IMAGE" >/dev/null 2>&1; then
    echo "error: endpoint image $ENDPOINT_IMAGE not found. Build it with:" >&2
    echo "  docker build -t $ENDPOINT_IMAGE -f deploy/endpoint/Dockerfile ." >&2
    exit 1
fi

echo "==> Building the integration harness"
cargo build --bin integration_control_plane

echo "==> Creating network $NET + database ($DB)"
docker network create "$NET" >/dev/null
yb -c "CREATE DATABASE \"$DB\"" >/dev/null

echo "==> Starting controller ($AMP_NAME) on port $AMP_HOST_PORT (docker runtime)"
# --user root + the mounted socket let controller's Docker provisioner launch endpoints;
# the endpoints join $NET so controller and the client can reach them. The controller
# owns cluster control in-process (generation authority + registries, served on :8080)
# and provisions its own DB's schemas on startup — no separate controller.
# AMP_DEPLOY_SUPERVISOR_PATH bind-mounts the repo's current supervisor.py into
# each endpoint (the controller passes it to `docker run -v`, resolved by the
# host daemon). The pre-built endpoint image is only rebuilt by hand, so its
# baked supervisor goes stale; without this, a supervisor predating branch
# support (#134) ignores ANCESTOR_TIMELINE_ID and branch endpoints come up empty.
docker run -d --name "$AMP_NAME" --hostname "$AMP_NAME" --network "$NET" \
    --user root \
    -p "${AMP_HOST_PORT}:8080" \
    --add-host host.docker.internal:host-gateway \
    -v /var/run/docker.sock:/var/run/docker.sock \
    -e "AMP_DATABASE_URL=postgres://yugabyte@host.docker.internal:5433/${DB}" \
    -e AMP_LOG_FORMAT=text \
    -e AMP_DEPLOY=true \
    -e AMP_DEPLOY_DRIVER=docker \
    -e AMP_DEPLOY_NETWORK="$NET" \
    -e AMP_DEPLOY_ENDPOINT_IMAGE="$ENDPOINT_IMAGE" \
    -e AMP_DEPLOY_EXTERNAL_HOST=127.0.0.1 \
    -e AMP_DEPLOY_BOOTSTRAP=true \
    -e AMP_DEPLOY_YB_HOST_PORTS=host.docker.internal:5433 \
    -e AMP_DEPLOY_YB_DBNAME="$DB" \
    -e AMP_DEPLOY_SUPERVISOR_PATH="$REPO/deploy/endpoint/supervisor.py" \
    "$AMP_IMAGE" >/dev/null

echo "==> Waiting for controller to be ready"
for i in $(seq 1 60); do
    if curl -sf -m2 "http://127.0.0.1:${AMP_HOST_PORT}/readyz" >/dev/null 2>&1; then break; fi
    if ! docker ps -q --filter "name=$AMP_NAME" | grep -q .; then
        echo "controller exited; logs:" >&2; docker logs "$AMP_NAME" 2>&1 | tail -30 >&2; exit 1
    fi
    sleep 1
done

echo "==> Running the controller-on-Docker suite"
AMP_BASE_URL="http://127.0.0.1:${AMP_HOST_PORT}" "$REPO/target/debug/integration_control_plane" ${ARGS[@]+"${ARGS[@]}"}
