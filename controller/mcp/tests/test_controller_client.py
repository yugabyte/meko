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


def test_create_project_async_returns_task_body():
    task = {"id": "t1", "kind": "create_project", "status": "pending"}
    handler = RecordingHandler(
        {"POST /api/v1/storage-pools/p1/projects": (202, task)}
    )
    client = make_client(handler)
    result = client.create_project(pool_id="p1", name="db", pg_version=15)
    assert result == task
    assert handler.last_json() == {"name": "db", "pg_version": 15}


def test_create_project_omits_pg_version_when_none():
    handler = RecordingHandler(
        {"POST /api/v1/storage-pools/p1/projects": (202, {"id": "t1"})}
    )
    client = make_client(handler)
    client.create_project(pool_id="p1", name="db")
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
    assert client.delete_project("p1", "pr1") == {}


def test_uninitialized_client_raises():
    client = ControllerClient()
    with pytest.raises(RuntimeError):
        _ = client.client


def test_list_tasks_filters():
    handler = RecordingHandler(
        {"GET /api/v1/tasks": {"tasks": [], "total": 0, "count": 0}}
    )
    client = make_client(handler)
    client.list_tasks(status="failed", kind="create_branch", project_id="pr1")
    params = dict(handler.last.url.params)
    assert params["status"] == "failed"
    assert params["kind"] == "create_branch"
    assert params["project_id"] == "pr1"
