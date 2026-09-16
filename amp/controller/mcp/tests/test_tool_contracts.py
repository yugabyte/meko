"""Wire-contract lock for all 32 MCP tools.

What this solves: it pins the exact HTTP method, path, request body, and query
params each tool sends to the controller, so silent API drift (a renamed route,
a moved/flattened field, a sync tool turned async) fails a test here instead of
only surfacing as a 400/404 in production. It complements the registration test
(names only) and the real-controller integration test (live acceptance); this
layer asserts "we send exactly the right request" for every tool, mock-only.
"""

import httpx
import pytest

import tools
from controller_client import controller_client
from helpers import RecordingHandler

# Path fragments for the nested resource ladder, mirroring the controller's
# /api/v1 routes. Ids here match the kwargs used in the cases below.
P = "/api/v1/storage-pools/p1"
PR = f"{P}/clusters/pr1"
B = f"{PR}/branches/b1"
E = f"{B}/endpoints/e1"

TASK = {"id": "t1", "kind": "op", "status": "pending"}

# Migration source fields shared by test_source_connectivity + create_migration.
MIGRATION_SOURCE = {
    "host": "db.example.com",
    "port": 5432,
    "username": "postgres",
    "password": "s3cret",
    "database": "shop",
    "schemas": ["public"],
}
CONN_KWARGS = dict(MIGRATION_SOURCE)
CREATE_MIGRATION_KWARGS = {"name": "shop-migration", **MIGRATION_SOURCE}
# The controller receives db_type (defaulted by the tool) alongside the source.
SOURCE_BODY = {"db_type": "postgresql", **MIGRATION_SOURCE}


@pytest.fixture
def wire():
    """Initialize the module-level client against a RecordingHandler.

    Returns a callable that takes a routes map and yields the handler for
    request assertions. The client is torn down after each test.
    """

    def _wire(routes: dict) -> RecordingHandler:
        handler = RecordingHandler(routes)
        controller_client.initialize(
            controller_url="http://controller:8080",
            transport=httpx.MockTransport(handler),
        )
        return handler

    yield _wire
    controller_client.shutdown()


# (tool name, kwargs, expected method, expected path, canned response spec).
# One row per tool — the full 32-tool surface.
CASES = [
    ("get_user", {"user_id": "u1"}, "GET", "/api/v1/users/u1", {"id": "u1"}),
    ("create_storage_pool", {"name": "n", "owner_user_id": "u9"}, "POST",
     "/api/v1/storage-pools", (201, {"id": "p1", "name": "n"})),
    ("list_storage_pools", {}, "GET", "/api/v1/storage-pools",
     {"storage_pools": [], "total": 0, "count": 0}),
    ("get_storage_pool", {"pool_id": "p1"}, "GET", P, {"id": "p1"}),
    ("rename_storage_pool", {"pool_id": "p1", "name": "n2"}, "PATCH", P,
     {"id": "p1", "name": "n2"}),
    # Delete is synchronous and answers 204: no body, no Task to poll.
    ("delete_storage_pool", {"pool_id": "p1"}, "DELETE", P, (204, None)),
    ("create_cluster", {"pool_id": "p1", "name": "db"}, "POST",
     f"{P}/clusters", (202, TASK)),
    ("list_clusters", {"pool_id": "p1"}, "GET", f"{P}/clusters",
     {"clusters": [], "total": 0, "count": 0}),
    ("get_cluster", {"pool_id": "p1", "cluster_id": "pr1"}, "GET", PR,
     {"id": "pr1"}),
    ("delete_cluster", {"pool_id": "p1", "cluster_id": "pr1"}, "DELETE", PR,
     (202, TASK)),
    ("create_branch", {"pool_id": "p1", "cluster_id": "pr1", "name": "f"},
     "POST", f"{PR}/branches", (202, TASK)),
    ("list_branches", {"pool_id": "p1", "cluster_id": "pr1"}, "GET",
     f"{PR}/branches", {"branches": [], "total": 0, "count": 0}),
    ("get_branch", {"pool_id": "p1", "cluster_id": "pr1", "branch_id": "b1"},
     "GET", B, {"branch_id": "b1"}),
    ("add_branch_extensions",
     {"pool_id": "p1", "cluster_id": "pr1", "branch_id": "b1",
      "extensions": ["pgvector"]}, "POST", f"{B}/extensions", (202, TASK)),
    ("delete_branch", {"pool_id": "p1", "cluster_id": "pr1", "branch_id": "b1"},
     "DELETE", B, (202, TASK)),
    ("create_endpoint",
     {"pool_id": "p1", "cluster_id": "pr1", "branch_id": "b1"}, "POST",
     f"{B}/endpoints", (202, TASK)),
    ("list_endpoints", {"pool_id": "p1", "cluster_id": "pr1", "branch_id": "b1"},
     "GET", f"{B}/endpoints", {"endpoints": [], "total": 0, "count": 0}),
    ("get_endpoint",
     {"pool_id": "p1", "cluster_id": "pr1", "branch_id": "b1",
      "endpoint_id": "e1"}, "GET", E, {"id": "e1"}),
    ("delete_endpoint",
     {"pool_id": "p1", "cluster_id": "pr1", "branch_id": "b1",
      "endpoint_id": "e1"}, "DELETE", E, (202, TASK)),
    ("resize_endpoint",
     {"pool_id": "p1", "cluster_id": "pr1", "branch_id": "b1",
      "endpoint_id": "e1", "cpu": "4"}, "POST", f"{E}/resize", (202, TASK)),
    # Settings are synchronous: a 200 with what was stored and what it could be
    # applied to, not a Task to poll.
    ("update_branch_settings",
     {"pool_id": "p1", "cluster_id": "pr1", "branch_id": "b1",
      "pg_settings": {"work_mem": "64MB"}}, "PATCH", f"{B}/settings",
     {"applied": True}),
    ("update_endpoint_settings",
     {"pool_id": "p1", "cluster_id": "pr1", "branch_id": "b1",
      "endpoint_id": "e1", "pg_settings": {"work_mem": "64MB"}},
     "PATCH", f"{E}/settings", {"applied": True}),
    ("list_tasks", {}, "GET", "/api/v1/tasks",
     {"tasks": [], "total": 0, "count": 0}),
    ("get_task", {"task_id": "t1"}, "GET", "/api/v1/tasks/t1",
     {"id": "t1", "status": "succeeded"}),
    ("test_source_connectivity", CONN_KWARGS, "POST",
     "/api/v1/migrations/connectivity-checks", {"reachable": True}),
    ("create_migration", CREATE_MIGRATION_KWARGS, "POST", "/api/v1/migrations",
     (201, {"id": "m1", "state": "created"})),
    ("list_migrations", {}, "GET", "/api/v1/migrations",
     {"migrations": [], "total": 0, "count": 0}),
    ("get_migration", {"migration_id": "m1"}, "GET", "/api/v1/migrations/m1",
     {"id": "m1", "state": "assessed"}),
    ("get_migration_assessment", {"migration_id": "m1"}, "GET",
     "/api/v1/migrations/m1/assessment", {"complexity": "LOW"}),
    ("start_migration_assessment", {"migration_id": "m1"}, "POST",
     "/api/v1/migrations/m1/assess", (202, TASK)),
    ("provision_migration_target", {"migration_id": "m1", "pool_id": "p1"},
     "POST", "/api/v1/migrations/m1/provision-target", (202, TASK)),
    ("start_migration", {"migration_id": "m1"}, "POST",
     "/api/v1/migrations/m1/migrate", (202, TASK)),
]


def test_cases_cover_all_tools():
    assert len({name for name, *_ in CASES}) == 32


@pytest.mark.parametrize("name,kwargs,method,path,spec", CASES, ids=[c[0] for c in CASES])
def test_tool_hits_expected_method_and_path(wire, name, kwargs, method, path, spec):
    """Every tool must call the exact controller method + path."""
    handler = wire({f"{method} {path}": spec})
    getattr(tools, name)(**kwargs)
    assert handler.last.method == method
    assert handler.last.url.path == path


# ---- Request bodies (writes) ----

def test_create_storage_pool_body(wire):
    handler = wire({"POST /api/v1/storage-pools": (201, {"id": "p1"})})
    tools.create_storage_pool(name="n", owner_user_id="u9")
    assert handler.last_json() == {"name": "n", "owner_user_id": "u9"}


def test_create_cluster_includes_pg_version_when_set(wire):
    handler = wire({f"POST {P}/clusters": (202, TASK)})
    tools.create_cluster(pool_id="p1", name="db", pg_version=18)
    assert handler.last_json() == {"name": "db", "pg_version": 18}


def test_create_cluster_includes_extensions_when_set(wire):
    handler = wire({f"POST {P}/clusters": (202, TASK)})
    tools.create_cluster(pool_id="p1", name="db", extensions=["pgvector"])
    assert handler.last_json() == {"name": "db", "extensions": ["pgvector"]}


def test_create_cluster_includes_engine_when_set(wire):
    handler = wire({f"POST {P}/clusters": (202, TASK)})
    tools.create_cluster(pool_id="p1", name="db", engine="aidb")
    assert handler.last_json() == {"name": "db", "engine": "aidb"}


def test_create_cluster_engine_and_extensions_compose(wire):
    """The controller unions the engine's bundle with `extensions`; the client
    sends both verbatim rather than resolving the bundle itself."""
    handler = wire({f"POST {P}/clusters": (202, TASK)})
    tools.create_cluster(
        pool_id="p1", name="db", engine="oracle", extensions=["pgvector"]
    )
    assert handler.last_json() == {
        "name": "db",
        "engine": "oracle",
        "extensions": ["pgvector"],
    }


def test_add_branch_extensions_body(wire):
    handler = wire({f"POST {B}/extensions": (202, TASK)})
    tools.add_branch_extensions(
        pool_id="p1", cluster_id="pr1", branch_id="b1", extensions=["pgvector"]
    )
    assert handler.last_json() == {"extensions": ["pgvector"]}


def test_create_cluster_omits_pg_version_when_none(wire):
    # Locks the happy path: with no version we send only {name}, so the
    # controller applies its own default (18) — the "17" doc trap can't recur.
    handler = wire({f"POST {P}/clusters": (202, TASK)})
    tools.create_cluster(pool_id="p1", name="db")
    assert handler.last_json() == {"name": "db"}


def test_create_branch_omits_optional_fields(wire):
    handler = wire({f"POST {PR}/branches": (202, TASK)})
    tools.create_branch(pool_id="p1", cluster_id="pr1", name="f")
    assert handler.last_json() == {"name": "f"}


def test_create_branch_includes_optional_fields(wire):
    handler = wire({f"POST {PR}/branches": (202, TASK)})
    tools.create_branch(
        pool_id="p1", cluster_id="pr1", name="f",
        parent_branch_id="b0", fork_point_lsn="0/16ABCD0",
    )
    assert handler.last_json() == {
        "name": "f", "parent_branch_id": "b0", "fork_point_lsn": "0/16ABCD0",
    }


def test_create_endpoint_nests_cpu_and_memory_under_resources(wire):
    # The controller expects a nested `resources` object, NOT flat cpu/memory.
    handler = wire({f"POST {B}/endpoints": (202, TASK)})
    tools.create_endpoint(
        pool_id="p1", cluster_id="pr1", branch_id="b1",
        role="read_only", disk_type="network", cpu="2", memory="4Gi",
    )
    assert handler.last_json() == {
        "role": "read_only",
        "disk_type": "network",
        "resources": {"cpu": "2", "memory": "4Gi"},
    }


def test_create_endpoint_omits_empty_body_and_resources(wire):
    handler = wire({f"POST {B}/endpoints": (202, TASK)})
    tools.create_endpoint(pool_id="p1", cluster_id="pr1", branch_id="b1")
    # No password either. The tool stays permissive — the CONTROLLER requires it
    # and answers 400 — so the caller sees its message rather than a client-side
    # guess at the contract that would go stale the moment the rule moves.
    assert handler.last_json() == {}


def test_create_endpoint_sends_the_password(wire):
    handler = wire({f"POST {B}/endpoints": (202, TASK)})
    tools.create_endpoint(
        pool_id="p1", cluster_id="pr1", branch_id="b1", password="hunter2hunter2",
    )
    assert handler.last_json() == {"password": "hunter2hunter2"}


def test_create_endpoint_partial_resources(wire):
    handler = wire({f"POST {B}/endpoints": (202, TASK)})
    tools.create_endpoint(pool_id="p1", cluster_id="pr1", branch_id="b1", cpu="1500m")
    assert handler.last_json() == {"resources": {"cpu": "1500m"}}


def test_create_endpoint_sends_the_three_settings_maps(wire):
    # Each goes at the top level of the body, not nested under `resources`.
    handler = wire({f"POST {B}/endpoints": (202, TASK)})
    tools.create_endpoint(
        pool_id="p1", cluster_id="pr1", branch_id="b1",
        pg_settings={"work_mem": "64MB"},
        pageserver_settings={"tenant_config.enable_gc": True},
        safekeeper_settings={"wal_cache_size_mib": 512},
    )
    assert handler.last_json() == {
        "pg_settings": {"work_mem": "64MB"},
        "pageserver_settings": {"tenant_config.enable_gc": True},
        "safekeeper_settings": {"wal_cache_size_mib": 512},
    }


def test_create_endpoint_passes_dotted_keys_through_verbatim(wire):
    # Splitting the dotted key into a nested object here would be rejected by the
    # controller as a non-scalar value.
    handler = wire({f"POST {B}/endpoints": (202, TASK)})
    tools.create_endpoint(
        pool_id="p1", cluster_id="pr1", branch_id="b1",
        pageserver_settings={
            "tenant_config.enable_gc": True,
            "tenant_config.pitr_interval": "7 days",
        },
    )
    assert handler.last_json()["pageserver_settings"] == {
        "tenant_config.enable_gc": True,
        "tenant_config.pitr_interval": "7 days",
    }


def test_create_endpoint_omits_empty_settings_maps(wire):
    handler = wire({f"POST {B}/endpoints": (202, TASK)})
    tools.create_endpoint(
        pool_id="p1", cluster_id="pr1", branch_id="b1",
        pg_settings={}, pageserver_settings={}, safekeeper_settings={},
    )
    assert handler.last_json() == {}


# ---- Query params (lists) ----

def test_list_omits_pagination_when_unset(wire):
    handler = wire({"GET /api/v1/storage-pools": {"storage_pools": []}})
    tools.list_storage_pools()
    assert dict(handler.last.url.params) == {}


def test_list_sends_pagination_when_set(wire):
    handler = wire({f"GET {PR}/branches": {"branches": []}})
    tools.list_branches(pool_id="p1", cluster_id="pr1", limit=50, offset=10)
    assert dict(handler.last.url.params) == {"limit": "50", "offset": "10"}


def test_list_tasks_sends_filters(wire):
    handler = wire({"GET /api/v1/tasks": {"tasks": []}})
    tools.list_tasks(status="failed", kind="create_branch", cluster_id="pr1")
    params = dict(handler.last.url.params)
    assert params["status"] == "failed"
    assert params["kind"] == "create_branch"
    assert params["cluster_id"] == "pr1"


# ---- Sync vs. async contract ----

def test_create_storage_pool_is_synchronous(wire):
    # Guards finding: pool create returns the object (201), NOT a Task. If this
    # is ever turned async, the returned shape changes and this fails.
    handler = wire({"POST /api/v1/storage-pools": (201, {"id": "p1", "name": "n"})})
    result = tools.create_storage_pool(name="n", owner_user_id="u9")
    assert result == {"id": "p1", "name": "n"}
    assert "status" not in result  # not a Task


@pytest.mark.parametrize(
    "name,kwargs,path,method",
    [
        ("create_cluster", {"pool_id": "p1", "name": "db"}, f"{P}/clusters", "POST"),
        ("create_branch", {"pool_id": "p1", "cluster_id": "pr1", "name": "f"},
         f"{PR}/branches", "POST"),
        ("create_endpoint",
         {"pool_id": "p1", "cluster_id": "pr1", "branch_id": "b1"},
         f"{B}/endpoints", "POST"),
        ("add_branch_extensions",
         {"pool_id": "p1", "cluster_id": "pr1", "branch_id": "b1",
          "extensions": ["pgvector"]},
         f"{B}/extensions", "POST"),
        ("delete_cluster", {"pool_id": "p1", "cluster_id": "pr1"}, PR, "DELETE"),
        ("delete_branch", {"pool_id": "p1", "cluster_id": "pr1", "branch_id": "b1"},
         B, "DELETE"),
        ("delete_endpoint",
         {"pool_id": "p1", "cluster_id": "pr1", "branch_id": "b1", "endpoint_id": "e1"},
         E, "DELETE"),
        ("resize_endpoint",
         {"pool_id": "p1", "cluster_id": "pr1", "branch_id": "b1", "endpoint_id": "e1",
          "cpu": "4"},
         f"{E}/resize", "POST"),
    ],
)
def test_async_tools_return_task_body(wire, name, kwargs, path, method):
    handler = wire({f"{method} {path}": (202, TASK)})
    result = getattr(tools, name)(**kwargs)
    assert result == TASK


def test_resize_endpoint_nests_cpu_and_memory_under_resources(wire):
    handler = wire({f"POST {E}/resize": (202, TASK)})
    tools.resize_endpoint(
        pool_id="p1", cluster_id="pr1", branch_id="b1", endpoint_id="e1",
        cpu="4", memory="8Gi",
    )
    assert handler.last_json() == {"resources": {"cpu": "4", "memory": "8Gi"}}


# ---- Migrations ----

def test_test_source_connectivity_wraps_source_with_default_db_type(wire):
    handler = wire(
        {"POST /api/v1/migrations/connectivity-checks": {"reachable": True}}
    )
    tools.test_source_connectivity(**CONN_KWARGS)
    assert handler.last_json() == {"source": SOURCE_BODY}


def test_create_migration_sends_name_and_source(wire):
    handler = wire({"POST /api/v1/migrations": (201, {"id": "m1"})})
    tools.create_migration(**CREATE_MIGRATION_KWARGS)
    assert handler.last_json() == {"name": "shop-migration", "source": SOURCE_BODY}


def test_provision_migration_target_includes_optionals_when_set(wire):
    handler = wire(
        {"POST /api/v1/migrations/m1/provision-target": (202, TASK)}
    )
    tools.provision_migration_target(
        migration_id="m1", pool_id="p1", vcpu=4, project_name="shop-target"
    )
    assert handler.last_json() == {
        "pool_id": "p1", "vcpu": 4, "project_name": "shop-target",
    }


def test_provision_migration_target_omits_optionals(wire):
    handler = wire(
        {"POST /api/v1/migrations/m1/provision-target": (202, TASK)}
    )
    tools.provision_migration_target(migration_id="m1", pool_id="p1")
    assert handler.last_json() == {"pool_id": "p1"}


def test_start_migration_includes_parallel_jobs_when_set(wire):
    handler = wire({"POST /api/v1/migrations/m1/migrate": (202, TASK)})
    tools.start_migration(migration_id="m1", parallel_jobs=16)
    assert handler.last_json() == {"parallel_jobs": 16}


def test_start_migration_omits_parallel_jobs_when_none(wire):
    handler = wire({"POST /api/v1/migrations/m1/migrate": (202, TASK)})
    tools.start_migration(migration_id="m1")
    assert handler.last_json() == {}


def test_create_migration_is_synchronous(wire):
    # Locks the create/assess split: create_migration returns the Migration
    # (201), not a Task — only the three launchers are async.
    handler = wire({"POST /api/v1/migrations": (201, {"id": "m1", "state": "created"})})
    result = tools.create_migration(**CREATE_MIGRATION_KWARGS)
    assert result == {"id": "m1", "migration_id": "m1", "state": "created"}
    assert "status" not in result  # not a Task


@pytest.mark.parametrize(
    "name,kwargs,path",
    [
        ("start_migration_assessment", {"migration_id": "m1"},
         "/api/v1/migrations/m1/assess"),
        ("provision_migration_target", {"migration_id": "m1", "pool_id": "p1"},
         "/api/v1/migrations/m1/provision-target"),
        ("start_migration", {"migration_id": "m1"},
         "/api/v1/migrations/m1/migrate"),
    ],
)
def test_migration_launchers_return_task_body(wire, name, kwargs, path):
    handler = wire({f"POST {path}": (202, TASK)})
    result = getattr(tools, name)(**kwargs)
    assert result == TASK


# ---- Response pass-through (list envelopes) ----

def test_list_returns_resource_named_envelope(wire):
    body = {"storage_pools": [{"id": "p1"}], "total": 1, "count": 1}
    handler = wire({"GET /api/v1/storage-pools": body})
    assert tools.list_storage_pools() == body
