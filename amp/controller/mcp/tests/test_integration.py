import os

import httpx
import pytest
from fastmcp import Client
from fastmcp.exceptions import ToolError

import server
from conftest import requires_real_controller
from helpers import RecordingHandler


def build_in_memory(routes: dict):
    handler = RecordingHandler(routes)
    config = server.parse_config(["--transport", "stdio"])
    mcp = server.build_server(config, transport=httpx.MockTransport(handler))
    return mcp, handler


async def test_round_trip_get_storage_pool():
    mcp, handler = build_in_memory(
        {"GET /api/v1/storage-pools/p1": {"id": "p1", "name": "pool"}}
    )
    async with Client(mcp) as client:
        result = await client.call_tool("get_storage_pool", {"pool_id": "p1"})
    assert result.data == {"id": "p1", "name": "pool"}
    assert handler.last.url.path == "/api/v1/storage-pools/p1"


async def test_round_trip_create_project_returns_task():
    task = {"id": "t1", "kind": "create_project", "status": "pending"}
    mcp, handler = build_in_memory(
        {"POST /api/v1/storage-pools/p1/projects": (202, task)}
    )
    async with Client(mcp) as client:
        result = await client.call_tool(
            "create_project", {"pool_id": "p1", "name": "db"}
        )
    assert result.data["kind"] == "create_project"
    assert result.data["status"] == "pending"


async def test_round_trip_not_found_surfaces_as_tool_error():
    mcp, _ = build_in_memory({})  # everything 404s
    async with Client(mcp) as client:
        with pytest.raises(ToolError):
            await client.call_tool("get_user", {"user_id": "nope"})


async def test_healthz_route_serves_ok():
    config = server.parse_config(["--transport", "http"])
    mcp = server.build_server(config)
    app = mcp.http_app()
    transport = httpx.ASGITransport(app=app)
    async with httpx.AsyncClient(
        transport=transport, base_url="http://test"
    ) as client:
        resp = await client.get("/healthz")
    assert resp.status_code == 200
    assert resp.json() == {"status": "ok"}


async def _poll_task(client, task_id, timeout=30.0):
    import asyncio
    import time

    deadline = time.monotonic() + timeout
    last = None
    while time.monotonic() < deadline:
        last = (await client.call_tool("get_task", {"task_id": task_id})).data
        if last["status"] in ("succeeded", "failed"):
            return last
        await asyncio.sleep(1.0)
    return last


@requires_real_controller
async def test_real_controller_resource_ladder():
    """Full ladder against a live controller: pool -> project -> branch, the
    async-task polling contract, the read paths, and an error path.

    Tolerant of both a deployer-enabled controller (cascades succeed) and a
    minimal no-deployer one (endpoint deploy / delete cascade fail by design);
    either way the MCP layer must faithfully surface the result. Mints its own
    user over REST (createUser is intentionally not an MCP tool).
    """
    base = os.environ["AMP_CONTROLLER_URL"].rstrip("/")
    async with httpx.AsyncClient(base_url=base, timeout=30.0) as http:
        resp = await http.post("/api/v1/users", json={"is_admin": False})
        resp.raise_for_status()
        user_id = resp.json()["id"]

    sfx = os.urandom(4).hex()
    config = server.parse_config([])
    config.controller_url = base
    mcp = server.build_server(config)
    async with Client(mcp) as client:
        assert len(await client.list_tools()) == 18

        pool = (
            await client.call_tool(
                "create_storage_pool",
                {"name": f"itest-pool-{sfx}", "owner_user_id": user_id},
            )
        ).data
        pool_id = pool["id"]
        assert pool["owner_user_id"] == user_id

        pools = (await client.call_tool("list_storage_pools", {})).data
        assert any(p["id"] == pool_id for p in pools["storage_pools"])
        assert (await client.call_tool("get_storage_pool", {"pool_id": pool_id})).data[
            "id"
        ] == pool_id

        proj_task = (
            await client.call_tool(
                "create_project", {"pool_id": pool_id, "name": f"itest-proj-{sfx}"}
            )
        ).data
        assert proj_task["kind"] == "create_project"
        assert _await_status(await _poll_task(client, proj_task["id"])) == "succeeded"

        projs = (await client.call_tool("list_projects", {"pool_id": pool_id})).data
        proj = next(p for p in projs["projects"] if p["name"] == f"itest-proj-{sfx}")
        project_id = proj["id"]
        default_branch_id = proj["default_branch"]["branch_id"]
        assert (
            await client.call_tool(
                "get_project", {"pool_id": pool_id, "project_id": project_id}
            )
        ).data["id"] == project_id

        # fork_point_lsn is required when no deployer can resolve the parent's
        # latest LSN; the tool exposes it.
        branch_task = (
            await client.call_tool(
                "create_branch",
                {
                    "pool_id": pool_id,
                    "project_id": project_id,
                    "name": f"itest-branch-{sfx}",
                    "fork_point_lsn": "0/0",
                },
            )
        ).data
        assert _await_status(await _poll_task(client, branch_task["id"])) == "succeeded"
        branches = (
            await client.call_tool(
                "list_branches", {"pool_id": pool_id, "project_id": project_id}
            )
        ).data
        assert branches["total"] >= 2

        # Error path: surfaced as a ToolError when the deployer is disabled.
        try:
            await client.call_tool(
                "create_endpoint",
                {
                    "pool_id": pool_id,
                    "project_id": project_id,
                    "branch_id": default_branch_id,
                },
            )
        except ToolError:
            pass

        assert (
            await client.call_tool("get_user", {"user_id": user_id})
        ).data["id"] == user_id
        tasks = (await client.call_tool("list_tasks", {"kind": "create_project"})).data
        assert any(t["id"] == proj_task["id"] for t in tasks["tasks"])

        # Delete cascade: succeeds with a deployer, fails by design without one.
        del_task = (
            await client.call_tool(
                "delete_project", {"pool_id": pool_id, "project_id": project_id}
            )
        ).data
        assert _await_status(await _poll_task(client, del_task["id"])) in (
            "succeeded",
            "failed",
        )


def _await_status(task):
    return task["status"] if isinstance(task, dict) else task
