import httpx
import pytest

import tools
from controller_client import controller_client
from helpers import RecordingHandler


@pytest.fixture
def wired(request):
    """Initialize the module-level singleton against a RecordingHandler."""
    routes = getattr(request, "param", {})
    handler = RecordingHandler(routes)
    controller_client.initialize(
        controller_url="http://controller:8080",
        transport=httpx.MockTransport(handler),
    )
    yield handler
    controller_client.shutdown()


@pytest.mark.parametrize(
    "wired",
    [{"GET /api/v1/users/u1": {"id": "u1", "is_admin": False}}],
    indirect=True,
)
def test_get_user_calls_backend(wired):
    result = tools.get_user("u1")
    assert result == {"id": "u1", "is_admin": False}
    assert wired.last.url.path == "/api/v1/users/u1"


@pytest.mark.parametrize(
    "wired",
    [{"POST /api/v1/storage-pools": (201, {"id": "p1", "name": "n"})}],
    indirect=True,
)
def test_create_storage_pool_passes_owner(wired):
    result = tools.create_storage_pool(name="n", owner_user_id="u9")
    assert result["id"] == "p1"
    assert wired.last_json() == {"name": "n", "owner_user_id": "u9"}


@pytest.mark.parametrize(
    "wired",
    [{"POST /api/v1/storage-pools/p1/projects": (202, {"id": "t1", "kind": "create_project", "status": "pending"})}],
    indirect=True,
)
def test_create_project_returns_task(wired):
    result = tools.create_project(pool_id="p1", name="db")
    assert result == {"id": "t1", "kind": "create_project", "status": "pending"}


@pytest.mark.parametrize(
    "wired",
    [{"DELETE /api/v1/storage-pools/p1/projects/pr1/branches/b1/endpoints/e1": (202, {"id": "t2", "kind": "delete_endpoint", "status": "pending"})}],
    indirect=True,
)
def test_delete_endpoint_returns_task(wired):
    result = tools.delete_endpoint(
        pool_id="p1", project_id="pr1", branch_id="b1", endpoint_id="e1"
    )
    assert result["kind"] == "delete_endpoint"
    assert wired.last.method == "DELETE"


@pytest.mark.parametrize(
    "wired",
    [{"POST /api/v1/storage-pools/p1/projects/pr1/branches": (202, {"id": "t3"})}],
    indirect=True,
)
def test_create_branch_optional_fields(wired):
    tools.create_branch(
        pool_id="p1",
        project_id="pr1",
        name="feature",
        parent_branch_id="b0",
        fork_point_lsn="0/16ABCD0",
    )
    assert wired.last_json() == {
        "name": "feature",
        "parent_branch_id": "b0",
        "fork_point_lsn": "0/16ABCD0",
    }


@pytest.mark.parametrize(
    "wired",
    [{"GET /api/v1/tasks/t1": {"id": "t1", "status": "succeeded"}}],
    indirect=True,
)
def test_get_task(wired):
    assert tools.get_task("t1")["status"] == "succeeded"
