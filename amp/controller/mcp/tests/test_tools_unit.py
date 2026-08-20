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
    [{"POST /api/v1/storage-pools/p1/clusters": (202, {"id": "t1", "kind": "create_cluster", "status": "pending"})}],
    indirect=True,
)
def test_create_cluster_returns_task(wired):
    result = tools.create_cluster(pool_id="p1", name="db")
    assert result == {"id": "t1", "kind": "create_cluster", "status": "pending"}


@pytest.mark.parametrize(
    "wired",
    [{"DELETE /api/v1/storage-pools/p1/clusters/pr1/branches/b1/endpoints/e1": (202, {"id": "t2", "kind": "delete_endpoint", "status": "pending"})}],
    indirect=True,
)
def test_delete_endpoint_returns_task(wired):
    result = tools.delete_endpoint(
        pool_id="p1", cluster_id="pr1", branch_id="b1", endpoint_id="e1"
    )
    assert result["kind"] == "delete_endpoint"
    assert wired.last.method == "DELETE"


@pytest.mark.parametrize(
    "wired",
    [{"POST /api/v1/storage-pools/p1/clusters/pr1/branches": (202, {"id": "t3"})}],
    indirect=True,
)
def test_create_branch_optional_fields(wired):
    tools.create_branch(
        pool_id="p1",
        cluster_id="pr1",
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


@pytest.mark.parametrize(
    "wired",
    [{"POST /api/v1/storage-pools/p1/clusters/pr1/branches/b1/endpoints/e1/resize": (202, {"id": "t9", "kind": "resize_endpoint", "status": "pending"})}],
    indirect=True,
)
def test_resize_endpoint_returns_task_and_nests_resources(wired):
    result = tools.resize_endpoint(
        pool_id="p1", cluster_id="pr1", branch_id="b1", endpoint_id="e1",
        cpu="4", memory="8Gi",
    )
    assert result["kind"] == "resize_endpoint"
    assert wired.last.method == "POST"
    assert wired.last.url.path.endswith("/endpoints/e1/resize")
    assert wired.last_json() == {"resources": {"cpu": "4", "memory": "8Gi"}}


@pytest.mark.parametrize(
    "wired",
    [{"POST /api/v1/storage-pools/p1/clusters/pr1/branches/b1/endpoints/e1/resize": (202, {"id": "t9"})}],
    indirect=True,
)
def test_resize_endpoint_omits_unset_field(wired):
    tools.resize_endpoint(
        pool_id="p1", cluster_id="pr1", branch_id="b1", endpoint_id="e1", cpu="4",
    )
    assert wired.last_json() == {"resources": {"cpu": "4"}}


# The settings tools rest on a distinction the wire format makes and Python
# hides: an argument left unset must not appear in the body at all ("leave that
# surface alone"), while `{}` must appear ("clear it"). Collapsing them would
# make an omitted argument silently destructive — a caller changing a lease
# would blank the branch's pg_settings.
@pytest.mark.parametrize(
    "wired",
    [{"PATCH /api/v1/storage-pools/p1/clusters/pr1/branches/b1/settings": (200, {"applied": True})}],
    indirect=True,
)
def test_update_branch_settings_omits_unset_surfaces(wired):
    tools.update_branch_settings(
        pool_id="p1", cluster_id="pr1", branch_id="b1",
        write_token_settings={"ttl_ms": 15000, "renew_seconds": 5},
    )
    assert wired.last.method == "PATCH"
    assert wired.last.url.path.endswith("/branches/b1/settings")
    # pg_settings is absent, not null and not {}.
    assert wired.last_json() == {
        "write_token_settings": {"ttl_ms": 15000, "renew_seconds": 5}
    }


@pytest.mark.parametrize(
    "wired",
    [{"PATCH /api/v1/storage-pools/p1/clusters/pr1/branches/b1/settings": (200, {"applied": True})}],
    indirect=True,
)
def test_update_branch_settings_sends_empty_to_clear(wired):
    tools.update_branch_settings(
        pool_id="p1", cluster_id="pr1", branch_id="b1", pg_settings={},
    )
    assert wired.last_json() == {"pg_settings": {}}


@pytest.mark.parametrize(
    "wired",
    [{"PATCH /api/v1/storage-pools/p1/clusters/pr1/branches/b1/endpoints/e1/settings":
      (200, {"applied": True, "endpoints": [{"endpoint_id": "e1", "restarted": ["pageserver"]}]})}],
    indirect=True,
)
def test_update_endpoint_settings_reports_what_restarted(wired):
    result = tools.update_endpoint_settings(
        pool_id="p1", cluster_id="pr1", branch_id="b1", endpoint_id="e1",
        pageserver_settings={"tenant_config.enable_gc": True},
    )
    assert wired.last_json() == {
        "pageserver_settings": {"tenant_config.enable_gc": True}
    }
    # The caller is told which services were bounced, which is the part with a
    # cost: restarting the compute interrupts Postgres connections.
    assert result["endpoints"][0]["restarted"] == ["pageserver"]
