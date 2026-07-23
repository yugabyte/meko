"""Wire-contract lock for all 18 MCP tools.

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
PR = f"{P}/projects/pr1"
B = f"{PR}/branches/b1"
E = f"{B}/endpoints/e1"

TASK = {"id": "t1", "kind": "op", "status": "pending"}


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
# One row per tool — the full 18-tool surface.
CASES = [
    ("get_user", {"user_id": "u1"}, "GET", "/api/v1/users/u1", {"id": "u1"}),
    ("create_storage_pool", {"name": "n", "owner_user_id": "u9"}, "POST",
     "/api/v1/storage-pools", (201, {"id": "p1", "name": "n"})),
    ("list_storage_pools", {}, "GET", "/api/v1/storage-pools",
     {"storage_pools": [], "total": 0, "count": 0}),
    ("get_storage_pool", {"pool_id": "p1"}, "GET", P, {"id": "p1"}),
    ("create_project", {"pool_id": "p1", "name": "db"}, "POST",
     f"{P}/projects", (202, TASK)),
    ("list_projects", {"pool_id": "p1"}, "GET", f"{P}/projects",
     {"projects": [], "total": 0, "count": 0}),
    ("get_project", {"pool_id": "p1", "project_id": "pr1"}, "GET", PR,
     {"id": "pr1"}),
    ("delete_project", {"pool_id": "p1", "project_id": "pr1"}, "DELETE", PR,
     (202, TASK)),
    ("create_branch", {"pool_id": "p1", "project_id": "pr1", "name": "f"},
     "POST", f"{PR}/branches", (202, TASK)),
    ("list_branches", {"pool_id": "p1", "project_id": "pr1"}, "GET",
     f"{PR}/branches", {"branches": [], "total": 0, "count": 0}),
    ("get_branch", {"pool_id": "p1", "project_id": "pr1", "branch_id": "b1"},
     "GET", B, {"branch_id": "b1"}),
    ("delete_branch", {"pool_id": "p1", "project_id": "pr1", "branch_id": "b1"},
     "DELETE", B, (202, TASK)),
    ("create_endpoint",
     {"pool_id": "p1", "project_id": "pr1", "branch_id": "b1"}, "POST",
     f"{B}/endpoints", (202, TASK)),
    ("list_endpoints", {"pool_id": "p1", "project_id": "pr1", "branch_id": "b1"},
     "GET", f"{B}/endpoints", {"endpoints": [], "total": 0, "count": 0}),
    ("get_endpoint",
     {"pool_id": "p1", "project_id": "pr1", "branch_id": "b1",
      "endpoint_id": "e1"}, "GET", E, {"id": "e1"}),
    ("delete_endpoint",
     {"pool_id": "p1", "project_id": "pr1", "branch_id": "b1",
      "endpoint_id": "e1"}, "DELETE", E, (202, TASK)),
    ("list_tasks", {}, "GET", "/api/v1/tasks",
     {"tasks": [], "total": 0, "count": 0}),
    ("get_task", {"task_id": "t1"}, "GET", "/api/v1/tasks/t1",
     {"id": "t1", "status": "succeeded"}),
]


def test_cases_cover_all_eighteen_tools():
    assert len({name for name, *_ in CASES}) == 18


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


def test_create_project_includes_pg_version_when_set(wire):
    handler = wire({f"POST {P}/projects": (202, TASK)})
    tools.create_project(pool_id="p1", name="db", pg_version=18)
    assert handler.last_json() == {"name": "db", "pg_version": 18}


def test_create_project_omits_pg_version_when_none(wire):
    # Locks the happy path: with no version we send only {name}, so the
    # controller applies its own default (18) — the "17" doc trap can't recur.
    handler = wire({f"POST {P}/projects": (202, TASK)})
    tools.create_project(pool_id="p1", name="db")
    assert handler.last_json() == {"name": "db"}


def test_create_branch_omits_optional_fields(wire):
    handler = wire({f"POST {PR}/branches": (202, TASK)})
    tools.create_branch(pool_id="p1", project_id="pr1", name="f")
    assert handler.last_json() == {"name": "f"}


def test_create_branch_includes_optional_fields(wire):
    handler = wire({f"POST {PR}/branches": (202, TASK)})
    tools.create_branch(
        pool_id="p1", project_id="pr1", name="f",
        parent_branch_id="b0", fork_point_lsn="0/16ABCD0",
    )
    assert handler.last_json() == {
        "name": "f", "parent_branch_id": "b0", "fork_point_lsn": "0/16ABCD0",
    }


def test_create_endpoint_nests_cpu_and_memory_under_resources(wire):
    # The controller expects a nested `resources` object, NOT flat cpu/memory.
    handler = wire({f"POST {B}/endpoints": (202, TASK)})
    tools.create_endpoint(
        pool_id="p1", project_id="pr1", branch_id="b1",
        role="read_only", disk_type="network", cpu="2", memory="4Gi",
    )
    assert handler.last_json() == {
        "role": "read_only",
        "disk_type": "network",
        "resources": {"cpu": "2", "memory": "4Gi"},
    }


def test_create_endpoint_omits_empty_body_and_resources(wire):
    handler = wire({f"POST {B}/endpoints": (202, TASK)})
    tools.create_endpoint(pool_id="p1", project_id="pr1", branch_id="b1")
    assert handler.last_json() == {}


def test_create_endpoint_partial_resources(wire):
    handler = wire({f"POST {B}/endpoints": (202, TASK)})
    tools.create_endpoint(pool_id="p1", project_id="pr1", branch_id="b1", cpu="1500m")
    assert handler.last_json() == {"resources": {"cpu": "1500m"}}


# ---- Query params (lists) ----

def test_list_omits_pagination_when_unset(wire):
    handler = wire({"GET /api/v1/storage-pools": {"storage_pools": []}})
    tools.list_storage_pools()
    assert dict(handler.last.url.params) == {}


def test_list_sends_pagination_when_set(wire):
    handler = wire({f"GET {PR}/branches": {"branches": []}})
    tools.list_branches(pool_id="p1", project_id="pr1", limit=50, offset=10)
    assert dict(handler.last.url.params) == {"limit": "50", "offset": "10"}


def test_list_tasks_sends_filters(wire):
    handler = wire({"GET /api/v1/tasks": {"tasks": []}})
    tools.list_tasks(status="failed", kind="create_branch", project_id="pr1")
    params = dict(handler.last.url.params)
    assert params["status"] == "failed"
    assert params["kind"] == "create_branch"
    assert params["project_id"] == "pr1"


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
        ("create_project", {"pool_id": "p1", "name": "db"}, f"{P}/projects", "POST"),
        ("create_branch", {"pool_id": "p1", "project_id": "pr1", "name": "f"},
         f"{PR}/branches", "POST"),
        ("create_endpoint",
         {"pool_id": "p1", "project_id": "pr1", "branch_id": "b1"},
         f"{B}/endpoints", "POST"),
        ("delete_project", {"pool_id": "p1", "project_id": "pr1"}, PR, "DELETE"),
        ("delete_branch", {"pool_id": "p1", "project_id": "pr1", "branch_id": "b1"},
         B, "DELETE"),
        ("delete_endpoint",
         {"pool_id": "p1", "project_id": "pr1", "branch_id": "b1", "endpoint_id": "e1"},
         E, "DELETE"),
    ],
)
def test_async_tools_return_task_body(wire, name, kwargs, path, method):
    handler = wire({f"{method} {path}": (202, TASK)})
    result = getattr(tools, name)(**kwargs)
    assert result == TASK


# ---- Response pass-through (list envelopes) ----

def test_list_returns_resource_named_envelope(wire):
    body = {"storage_pools": [{"id": "p1"}], "total": 1, "count": 1}
    handler = wire({"GET /api/v1/storage-pools": body})
    assert tools.list_storage_pools() == body
