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


async def test_round_trip_create_cluster_returns_task():
    task = {"id": "t1", "kind": "create_cluster", "status": "pending"}
    mcp, handler = build_in_memory(
        {"POST /api/v1/storage-pools/p1/clusters": (202, task)}
    )
    async with Client(mcp) as client:
        result = await client.call_tool(
            "create_cluster", {"pool_id": "p1", "name": "db"}
        )
    assert result.data["kind"] == "create_cluster"
    assert result.data["status"] == "pending"


async def test_round_trip_create_migration_is_synchronous():
    migration = {"id": "m1", "name": "shop", "state": "created"}
    mcp, handler = build_in_memory({"POST /api/v1/migrations": (201, migration)})
    expected = {**migration, "migration_id": "m1"}
    async with Client(mcp) as client:
        result = await client.call_tool(
            "create_migration",
            {
                "name": "shop",
                "host": "db",
                "port": 5432,
                "username": "postgres",
                "password": "pw",
                "database": "shop",
                "schemas": ["public"],
            },
        )
    assert result.data == expected
    assert handler.last.url.path == "/api/v1/migrations"
    # The password is sent to the controller but the tool never echoes it back.
    assert "password" not in result.data


async def test_round_trip_migration_ladder_polls_get_migration():
    """The assess/provision/migrate/end launchers return a Task, but progress is
    read from get_migration — exercise that full loop over a mocked controller.
    """
    assess_task = {"id": "t1", "kind": "voyager_start_migration_run", "status": "pending"}
    provision_task = {"id": "t2", "kind": "voyager_create_migration_target", "status": "pending"}
    migrate_task = {"id": "t3", "kind": "voyager_start_migration_run", "status": "pending"}
    end_task = {"id": "t4", "kind": "voyager_start_migration_run", "status": "pending"}
    mcp, handler = build_in_memory(
        {
            "POST /api/v1/migrations/m1/assess": (202, assess_task),
            "GET /api/v1/migrations/m1": {"id": "m1", "state": "assessed"},
            "GET /api/v1/migrations/m1/assessment": {"complexity": "LOW"},
            "POST /api/v1/migrations/m1/provision-target": (202, provision_task),
            "POST /api/v1/migrations/m1/migrate": (202, migrate_task),
            "POST /api/v1/migrations/m1/end": (202, end_task),
        }
    )
    async with Client(mcp) as client:
        assert (
            await client.call_tool("start_migration_assessment", {"migration_id": "m1"})
        ).data["kind"] == "voyager_start_migration_run"
        # Progress is tracked via get_migration, not get_task.
        assert (
            await client.call_tool("get_migration", {"migration_id": "m1"})
        ).data["state"] == "assessed"
        assert (
            await client.call_tool("get_migration_assessment", {"migration_id": "m1"})
        ).data["complexity"] == "LOW"
        prov = (
            await client.call_tool(
                "provision_migration_target", {"migration_id": "m1", "pool_id": "p1"}
            )
        ).data
        assert prov["status"] == "pending"
        assert handler.last_json() == {"pool_id": "p1"}
        assert (
            await client.call_tool("start_migration", {"migration_id": "m1"})
        ).data["status"] == "pending"
        assert (
            await client.call_tool("end_migration", {"migration_id": "m1"})
        ).data["status"] == "pending"


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
    """Full ladder against a live controller: pool -> cluster -> branch, the
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
        assert len(await client.list_tools()) == 33

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
                "create_cluster", {"pool_id": pool_id, "name": f"itest-proj-{sfx}"}
            )
        ).data
        assert proj_task["kind"] == "create_cluster"
        assert _await_status(await _poll_task(client, proj_task["id"])) == "succeeded"

        projs = (await client.call_tool("list_clusters", {"pool_id": pool_id})).data
        proj = next(p for p in projs["clusters"] if p["name"] == f"itest-proj-{sfx}")
        cluster_id = proj["id"]
        default_branch_id = proj["default_branch"]["branch_id"]
        assert (
            await client.call_tool(
                "get_cluster", {"pool_id": pool_id, "cluster_id": cluster_id}
            )
        ).data["id"] == cluster_id

        # fork_point_lsn is required when no deployer can resolve the parent's
        # latest LSN; the tool exposes it.
        branch_task = (
            await client.call_tool(
                "create_branch",
                {
                    "pool_id": pool_id,
                    "cluster_id": cluster_id,
                    "name": f"itest-branch-{sfx}",
                    "fork_point_lsn": "0/0",
                },
            )
        ).data
        assert _await_status(await _poll_task(client, branch_task["id"])) == "succeeded"
        branches = (
            await client.call_tool(
                "list_branches", {"pool_id": pool_id, "cluster_id": cluster_id}
            )
        ).data
        assert branches["total"] >= 2

        # Error path: surfaced as a ToolError when the deployer is disabled.
        try:
            await client.call_tool(
                "create_endpoint",
                {
                    "pool_id": pool_id,
                    "cluster_id": cluster_id,
                    "branch_id": default_branch_id,
                },
            )
        except ToolError:
            pass

        assert (
            await client.call_tool("get_user", {"user_id": user_id})
        ).data["id"] == user_id
        tasks = (await client.call_tool("list_tasks", {"kind": "create_cluster"})).data
        assert any(t["id"] == proj_task["id"] for t in tasks["tasks"])

        # Delete cascade: succeeds with a deployer, fails by design without one.
        del_task = (
            await client.call_tool(
                "delete_cluster", {"pool_id": pool_id, "cluster_id": cluster_id}
            )
        ).data
        assert _await_status(await _poll_task(client, del_task["id"])) in (
            "succeeded",
            "failed",
        )

        # Rename, then delete the pool the cascade just emptied. Delete works even
        # though the cascade left the cluster row behind as a tombstone.
        renamed = (
            await client.call_tool(
                "rename_storage_pool",
                {"pool_id": pool_id, "name": f"itest-pool-renamed-{sfx}"},
            )
        ).data
        assert renamed["name"] == f"itest-pool-renamed-{sfx}"

        await client.call_tool("delete_storage_pool", {"pool_id": pool_id})
        pools = (await client.call_tool("list_storage_pools", {})).data
        assert not any(p["id"] == pool_id for p in pools["storage_pools"])


def _await_status(task):
    return task["status"] if isinstance(task, dict) else task
