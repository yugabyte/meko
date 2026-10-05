import httpx
import pytest

from controller_client import ControllerApiError, ControllerClient
from helpers import RecordingHandler


def make_client(handler) -> ControllerClient:
    client = ControllerClient()
    client.initialize(
        controller_url="http://controller:8080",
        transport=httpx.MockTransport(handler),
    )
    return client


def test_base_url_appends_api_v1():
    client = ControllerClient()
    client.initialize(controller_url="http://controller:8080/")
    assert str(client.client.base_url).rstrip("/") == "http://controller:8080/api/v1"
    client.shutdown()


def test_get_storage_pool_path_and_raw_body():
    handler = RecordingHandler(
        {"GET /api/v1/storage-pools/p1": {"id": "p1", "name": "pool"}}
    )
    client = make_client(handler)
    result = client.get_storage_pool("p1")
    assert result == {"id": "p1", "name": "pool"}
    assert handler.last.method == "GET"
    assert handler.last.url.path == "/api/v1/storage-pools/p1"


def test_create_storage_pool_sends_owner_user_id():
    handler = RecordingHandler(
        {"POST /api/v1/storage-pools": (201, {"id": "p1", "name": "n"})}
    )
    client = make_client(handler)
    result = client.create_storage_pool(name="n", owner_user_id="u9")
    assert result == {"id": "p1", "name": "n"}
    assert handler.last_json() == {"name": "n", "owner_user_id": "u9"}


def test_list_pagination_params():
    handler = RecordingHandler(
        {"GET /api/v1/storage-pools": {"storage_pools": [], "total": 0, "count": 0}}
    )
    client = make_client(handler)
    client.list_storage_pools(limit=50, offset=10)
    params = dict(handler.last.url.params)
    assert params == {"limit": "50", "offset": "10"}


def test_create_cluster_async_returns_task_body():
    task = {"id": "t1", "kind": "create_cluster", "status": "pending"}
    handler = RecordingHandler(
        {"POST /api/v1/storage-pools/p1/clusters": (202, task)}
    )
    client = make_client(handler)
    result = client.create_cluster(pool_id="p1", name="db", pg_version=15)
    assert result == task
    assert handler.last_json() == {"name": "db", "pg_version": 15}


def test_create_cluster_omits_pg_version_when_none():
    handler = RecordingHandler(
        {"POST /api/v1/storage-pools/p1/clusters": (202, {"id": "t1"})}
    )
    client = make_client(handler)
    client.create_cluster(pool_id="p1", name="db")
    assert handler.last_json() == {"name": "db"}


def test_error_envelope_raises_with_code():
    handler = RecordingHandler(
        {"GET /api/v1/users/missing": (404, {"error": {"code": "not_found", "message": "no user"}})}
    )
    client = make_client(handler)
    with pytest.raises(ControllerApiError) as exc:
        client.get_user("missing")
    assert exc.value.code == "not_found"
    assert exc.value.status_code == 404


def test_non_envelope_error_raises_http_code():
    def handler(request):
        return httpx.Response(500, text="boom")

    client = ControllerClient()
    client.initialize(
        controller_url="http://c:8080", transport=httpx.MockTransport(handler)
    )
    with pytest.raises(ControllerApiError) as exc:
        client.get_task("t1")
    assert exc.value.status_code == 500
    assert exc.value.code == "http_500"


def test_network_error_raises_controller_unreachable():
    def handler(request):
        raise httpx.ConnectError("connection refused", request=request)

    client = ControllerClient()
    client.initialize(
        controller_url="http://down:8080", transport=httpx.MockTransport(handler)
    )
    with pytest.raises(ControllerApiError) as exc:
        client.list_storage_pools()
    assert exc.value.code == "controller_unreachable"
    assert exc.value.status_code == 503


def test_long_non_envelope_error_is_truncated():
    def handler(request):
        return httpx.Response(502, text="x" * 5000)

    client = ControllerClient()
    client.initialize(
        controller_url="http://c:8080", transport=httpx.MockTransport(handler)
    )
    with pytest.raises(ControllerApiError) as exc:
        client.get_task("t1")
    assert exc.value.status_code == 502
    assert exc.value.message.endswith("...")
    assert len(exc.value.message) <= 503


def test_empty_2xx_body_returns_empty_dict():
    def handler(request):
        return httpx.Response(202, content=b"")

    client = ControllerClient()
    client.initialize(
        controller_url="http://c:8080", transport=httpx.MockTransport(handler)
    )
    assert client.delete_cluster("p1", "pr1") == {}


def test_uninitialized_client_raises():
    client = ControllerClient()
    with pytest.raises(RuntimeError):
        _ = client.client


def test_list_tasks_filters():
    handler = RecordingHandler(
        {"GET /api/v1/tasks": {"tasks": [], "total": 0, "count": 0}}
    )
    client = make_client(handler)
    client.list_tasks(status="failed", kind="create_branch", cluster_id="pr1")
    params = dict(handler.last.url.params)
    assert params["status"] == "failed"
    assert params["kind"] == "create_branch"
    assert params["cluster_id"] == "pr1"


RESIZE_PATH = "/api/v1/storage-pools/p1/clusters/pr1/branches/b1/endpoints/e1/resize"


def test_resize_endpoint_path_and_nested_resources():
    task = {"id": "t9", "kind": "resize_endpoint", "status": "pending"}
    handler = RecordingHandler({f"POST {RESIZE_PATH}": (202, task)})
    client = make_client(handler)
    result = client.resize_endpoint("p1", "pr1", "b1", "e1", cpu="4", memory="8Gi")
    assert result == task
    assert handler.last.method == "POST"
    assert handler.last.url.path == RESIZE_PATH
    assert handler.last_json() == {"resources": {"cpu": "4", "memory": "8Gi"}}


def test_resize_endpoint_omits_unset_field():
    handler = RecordingHandler({f"POST {RESIZE_PATH}": (202, {"id": "t9"})})
    client = make_client(handler)
    client.resize_endpoint("p1", "pr1", "b1", "e1", memory="8Gi")
    assert handler.last_json() == {"resources": {"memory": "8Gi"}}


# ---- Migrations ----

SOURCE_KWARGS = {
    "db_type": "postgresql",
    "host": "db.example.com",
    "port": 5432,
    "username": "postgres",
    "password": "s3cret",
    "database": "shop",
    "schemas": ["public"],
}
SOURCE_BODY = {
    "db_type": "postgresql",
    "host": "db.example.com",
    "port": 5432,
    "username": "postgres",
    "password": "s3cret",
    "database": "shop",
    "schemas": ["public"],
}


def test_test_source_connectivity_wraps_source():
    handler = RecordingHandler(
        {"POST /api/v1/migrations/connectivity-checks": {"reachable": True}}
    )
    client = make_client(handler)
    result = client.test_source_connectivity(**SOURCE_KWARGS)
    assert result == {"reachable": True}
    assert handler.last.method == "POST"
    assert handler.last.url.path == "/api/v1/migrations/connectivity-checks"
    assert handler.last_json() == {"source": SOURCE_BODY}


def test_create_migration_sends_name_and_source():
    handler = RecordingHandler(
        {"POST /api/v1/migrations": (201, {"id": "m1", "name": "shop-migration"})}
    )
    client = make_client(handler)
    result = client.create_migration(name="shop-migration", **SOURCE_KWARGS)
    assert result == {"id": "m1", "migration_id": "m1", "name": "shop-migration"}
    assert handler.last_json() == {"name": "shop-migration", "source": SOURCE_BODY}


def test_list_migrations_pagination_params():
    handler = RecordingHandler(
        {"GET /api/v1/migrations": {"migrations": [], "total": 0, "count": 0}}
    )
    client = make_client(handler)
    client.list_migrations(limit=25, offset=5)
    assert dict(handler.last.url.params) == {"limit": "25", "offset": "5"}


def test_list_migrations_aliases_migration_id():
    handler = RecordingHandler(
        {
            "GET /api/v1/migrations": {
                "migrations": [{"id": "m1", "name": "shop"}],
                "total": 1,
                "count": 1,
            }
        }
    )
    client = make_client(handler)
    assert client.list_migrations() == {
        "migrations": [{"id": "m1", "migration_id": "m1", "name": "shop"}],
        "total": 1,
        "count": 1,
    }


def test_get_migration_path():
    handler = RecordingHandler(
        {"GET /api/v1/migrations/m1": {"id": "m1", "state": "assessed"}}
    )
    client = make_client(handler)
    assert client.get_migration("m1") == {
        "id": "m1", "migration_id": "m1", "state": "assessed"
    }
    assert handler.last.url.path == "/api/v1/migrations/m1"


def test_get_migration_assessment_path():
    handler = RecordingHandler(
        {"GET /api/v1/migrations/m1/assessment": {"complexity": "MEDIUM"}}
    )
    client = make_client(handler)
    assert client.get_migration_assessment("m1") == {"complexity": "MEDIUM"}
    assert handler.last.url.path == "/api/v1/migrations/m1/assessment"


def test_start_migration_assessment_posts_without_body():
    task = {"id": "t1", "kind": "voyager_start_migration_run", "status": "pending"}
    handler = RecordingHandler(
        {"POST /api/v1/migrations/m1/assess": (202, task)}
    )
    client = make_client(handler)
    assert client.start_migration_assessment("m1") == task
    assert handler.last.method == "POST"
    assert handler.last_json() == {}


def test_provision_migration_target_full_body():
    task = {"id": "t2", "kind": "voyager_create_migration_target", "status": "pending"}
    handler = RecordingHandler(
        {"POST /api/v1/migrations/m1/provision-target": (202, task)}
    )
    client = make_client(handler)
    result = client.provision_migration_target(
        "m1", pool_id="p1", vcpu=4, project_name="shop-target"
    )
    assert result == task
    assert handler.last_json() == {
        "pool_id": "p1",
        "vcpu": 4,
        "project_name": "shop-target",
    }


def test_provision_migration_target_omits_optionals():
    handler = RecordingHandler(
        {"POST /api/v1/migrations/m1/provision-target": (202, {"id": "t2"})}
    )
    client = make_client(handler)
    client.provision_migration_target("m1", pool_id="p1")
    assert handler.last_json() == {"pool_id": "p1"}


def test_start_migration_includes_parallel_jobs_when_set():
    handler = RecordingHandler(
        {"POST /api/v1/migrations/m1/migrate": (202, {"id": "t3"})}
    )
    client = make_client(handler)
    client.start_migration("m1", parallel_jobs=16)
    assert handler.last_json() == {"parallel_jobs": 16}


def test_start_migration_omits_parallel_jobs_when_none():
    handler = RecordingHandler(
        {"POST /api/v1/migrations/m1/migrate": (202, {"id": "t3"})}
    )
    client = make_client(handler)
    client.start_migration("m1")
    assert handler.last_json() == {}


def test_end_migration_posts_without_body():
    task = {"id": "t4", "kind": "voyager_start_migration_run", "status": "pending"}
    handler = RecordingHandler(
        {"POST /api/v1/migrations/m1/end": (202, task)}
    )
    client = make_client(handler)
    assert client.end_migration("m1") == task
    assert handler.last.method == "POST"
    assert handler.last_json() == {}
