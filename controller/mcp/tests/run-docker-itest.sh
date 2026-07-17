#!/usr/bin/env bash
#
# Local docker-based integration test for the amp controller MCP server.
#
# Brings the whole thing up from nothing and drives it end to end:
#   1. a single-node YugabyteDB in a container (the controller's durable store);
#   2. the amp controller (Go binary, built here) wired to that YB — REST API only,
#      no endpoint deployer and no storage controller (kept minimal so the test
#      needs neither the endpoint image nor the Rust toolchain);
#   3. the MCP server as its OWN container, built from controller/mcp/Dockerfile,
#      pointed at the controller (the same shape it runs in on Kubernetes).
#
# It then (a) smoke-tests the MCP container over HTTP (GET /healthz + MCP tool
# enumeration over the Streamable-HTTP transport) and (b) runs the committed
# real-controller ladder test (tests/test_integration.py::test_real_controller_
# resource_ladder) against the controller. Everything is torn down on exit.
#
# This is the controller-MCP analogue of test/integration/control_plane/run-controller.sh.
#
# Prereqs: docker, uv, go, curl. No YugabyteDB, psql, or Rust toolchain needed.
#
# Usage:  controller/mcp/tests/run-docker-itest.sh
#   override ports if they collide:
#     YB_PORT=5434 CONTROLLER_PORT=18080 MCP_PORT=18000 controller/mcp/tests/run-docker-itest.sh
set -euo pipefail

YB_PORT="${YB_PORT:-5434}"
CONTROLLER_PORT="${CONTROLLER_PORT:-18080}"
MCP_PORT="${MCP_PORT:-18000}"
YB_IMAGE="${YB_IMAGE:-yugabytedb/yugabyte:latest}"
MCP_IMAGE="${MCP_IMAGE:-amp/controller-mcp:dev}"

YB_NAME="amp-mcp-itest-yb"
MCP_NAME="amp-mcp-itest-srv"

REPO="$(git -C "$(dirname "${BASH_SOURCE[0]}")" rev-parse --show-toplevel)"
MCP_DIR="$REPO/controller/mcp"
WORK="$(mktemp -d)"
CONTROLLER_BIN="${CONTROLLER_BIN:-$WORK/controller}"
CONTROLLER_PID=""

say()  { printf '\n\033[1;36m== %s\033[0m\n' "$*"; }
die()  { printf '\033[1;31merror: %s\033[0m\n' "$*" >&2; exit 1; }

cleanup() {
  set +e
  [ -n "$CONTROLLER_PID" ] && kill "$CONTROLLER_PID" >/dev/null 2>&1
  docker rm -f "$MCP_NAME" "$YB_NAME" >/dev/null 2>&1
  rm -rf "$WORK"
  return 0
}
trap cleanup EXIT

for bin in docker uv go curl; do
  command -v "$bin" >/dev/null 2>&1 || die "$bin not found on PATH"
done

say "build the controller binary (pure Go, no storage_controller)"
go -C "$REPO/controller" build -o "$CONTROLLER_BIN" ./cmd/controller

say "build the MCP server image ($MCP_IMAGE)"
docker build -t "$MCP_IMAGE" -f "$MCP_DIR/Dockerfile" "$MCP_DIR"

say "start YugabyteDB ($YB_IMAGE) on host port $YB_PORT"
docker rm -f "$YB_NAME" >/dev/null 2>&1 || true
docker run -d --name "$YB_NAME" -p "${YB_PORT}:5433" "$YB_IMAGE" \
  bash -c 'exec bin/yugabyted start --background=false --base_dir=/tmp/ybd --tserver_flags=ysql_enable_auth=false' >/dev/null
YB_FQDN="$(docker exec "$YB_NAME" hostname -f)"
echo -n "   waiting for YSQL"
for i in $(seq 1 60); do
  if docker exec "$YB_NAME" bin/ysqlsh -h "$YB_FQDN" -p 5433 -U yugabyte -d yugabyte -tAc "select 1" >/dev/null 2>&1; then
    echo " ready"; break
  fi
  echo -n "."; sleep 3
  [ "$i" = 60 ] && die "YugabyteDB did not become ready"
done

say "start the controller on 127.0.0.1:$CONTROLLER_PORT (REST API only)"
AMP_LOG_FORMAT=text "$CONTROLLER_BIN" serve \
  --database-url "postgres://yugabyte@127.0.0.1:${YB_PORT}/yugabyte?sslmode=disable" \
  --listen "127.0.0.1:${CONTROLLER_PORT}" >"$WORK/controller.log" 2>&1 &
CONTROLLER_PID=$!
echo -n "   waiting for /readyz"
for i in $(seq 1 30); do
  [ "$(curl -s -m 3 -o /dev/null -w '%{http_code}' "http://127.0.0.1:${CONTROLLER_PORT}/readyz")" = "200" ] && { echo " ready"; break; }
  echo -n "."; sleep 2
  [ "$i" = 30 ] && { tail -20 "$WORK/controller.log"; die "controller did not become ready"; }
done

say "start the MCP server container (--network host -> controller on 127.0.0.1:$CONTROLLER_PORT)"
docker rm -f "$MCP_NAME" >/dev/null 2>&1 || true
docker run -d --name "$MCP_NAME" --network host \
  -e "AMP_CONTROLLER_URL=http://127.0.0.1:${CONTROLLER_PORT}" \
  -e AMP_MCP_TRANSPORT=http -e AMP_MCP_HOST=0.0.0.0 -e "AMP_MCP_PORT=${MCP_PORT}" \
  "$MCP_IMAGE" >/dev/null
echo -n "   waiting for MCP /healthz"
for i in $(seq 1 30); do
  [ "$(curl -s -m 3 -o /dev/null -w '%{http_code}' "http://127.0.0.1:${MCP_PORT}/healthz")" = "200" ] && { echo " ready"; break; }
  echo -n "."; sleep 2
  [ "$i" = 30 ] && { docker logs "$MCP_NAME" 2>&1 | tail -20; die "MCP server did not become ready"; }
done

say "smoke-test the MCP container over the Streamable-HTTP transport"
MCP_URL="http://127.0.0.1:${MCP_PORT}/mcp" uv run --project "$MCP_DIR" python - <<'PY'
import asyncio, os
from fastmcp import Client
async def main():
    async with Client(os.environ["MCP_URL"]) as c:
        names = sorted(t.name for t in await c.list_tools())
        assert len(names) == 18, f"expected 18 tools, got {len(names)}: {names}"
        assert "create_user" not in names and "list_users" not in names
        print(f"   MCP container serves {len(names)} tools; create_user/list_users absent")
asyncio.run(main())
PY

say "run the committed real-controller ladder test (pytest)"
AMP_CONTROLLER_URL="http://127.0.0.1:${CONTROLLER_PORT}" \
  uv run --project "$MCP_DIR" pytest "$MCP_DIR/tests/test_integration.py" -k real_controller -v

say "PASS — MCP server integration verified end to end against a dockerized stack"
