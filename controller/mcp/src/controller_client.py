"""HTTP client for the amp controller REST API.

Singleton wrapping the controller's `/api/v1` surface. Success responses carry
the raw resource JSON (no `data` wrapper); errors carry an `{"error": {...}}`
envelope. Async create/delete operations return 202 + a Task body, which this
client returns unchanged for the caller to poll via `get_task`.
"""

import logging
import os
from typing import Any, Optional

import httpx

logger = logging.getLogger("amp-mcp.controller")

DEFAULT_CONTROLLER_URL = "http://localhost:8080"
DEFAULT_REQUEST_TIMEOUT = 30.0
# Cap on a non-envelope error body (e.g. an HTML page from a proxy) so a failure
# doesn't blow the caller's context window.
MAX_ERROR_MESSAGE_LEN = 500


class ControllerApiError(Exception):
    """Raised when the controller returns an error envelope or a >=400 status."""

    def __init__(self, code: str, message: str, status_code: Optional[int] = None):
        self.code = code
        self.message = message
        self.status_code = status_code
        super().__init__(f"[{code}] {message}")


class ControllerClient:
    """Manages HTTP communication with the amp controller API."""

    def __init__(self):
        self._base_url: str = ""
        self._client: Optional[httpx.Client] = None

    def initialize(
        self,
        controller_url: Optional[str] = None,
        request_timeout: Optional[float] = None,
        transport: Optional[httpx.BaseTransport] = None,
    ) -> None:
        url = (
            controller_url
            or os.environ.get("AMP_CONTROLLER_URL", DEFAULT_CONTROLLER_URL)
        ).rstrip("/")
        self._base_url = f"{url}/api/v1"

        timeout = request_timeout
        if timeout is None:
            timeout = float(
                os.environ.get("AMP_MCP_REQUEST_TIMEOUT", DEFAULT_REQUEST_TIMEOUT)
            )

        self._client = httpx.Client(
            base_url=self._base_url,
            headers={"Content-Type": "application/json"},
            timeout=timeout,
            transport=transport,
        )
        logger.info("Controller client ready (base_url=%s)", self._base_url)

    def shutdown(self) -> None:
        if self._client:
            self._client.close()
            self._client = None
            logger.info("Controller client shut down")

    @property
    def client(self) -> httpx.Client:
        if self._client is None:
            raise RuntimeError(
                "ControllerClient not initialized. Call initialize() first."
            )
        return self._client

    def _request(self, method: str, path: str, **kwargs) -> Any:
        logger.debug("controller %s %s", method, path)
        try:
            resp = self.client.request(method, path, **kwargs)
        except httpx.RequestError as exc:
            # Controller down / DNS / connect / read failure: surface a clean
            # tool error rather than a raw traceback.
            raise ControllerApiError(
                code="controller_unreachable",
                message=f"could not reach the controller at {self._base_url}: {exc}",
                status_code=503,
            ) from exc

        try:
            body = resp.json()
        except ValueError:
            body = None

        err = body.get("error") if isinstance(body, dict) else None
        if isinstance(err, dict):
            raise ControllerApiError(
                code=err.get("code") or "unknown",
                message=err.get("message") or resp.text or resp.reason_phrase,
                status_code=resp.status_code,
            )

        if resp.status_code >= 400:
            message = resp.text or resp.reason_phrase
            if len(message) > MAX_ERROR_MESSAGE_LEN:
                message = message[:MAX_ERROR_MESSAGE_LEN] + "..."
            raise ControllerApiError(
                code=f"http_{resp.status_code}",
                message=message,
                status_code=resp.status_code,
            )

        if body is None:
            return {}
        return body

    @staticmethod
    def _page_params(limit: Optional[int], offset: Optional[int]) -> dict:
        params: dict[str, Any] = {}
        if limit is not None:
            params["limit"] = limit
        if offset is not None:
            params["offset"] = offset
        return params

    # ---- Users ----

    def get_user(self, user_id: str) -> dict:
        return self._request("GET", f"/users/{user_id}")

    # ---- Storage pools ----

    def create_storage_pool(self, name: str, owner_user_id: str) -> dict:
        return self._request(
            "POST",
            "/storage-pools",
            json={"name": name, "owner_user_id": owner_user_id},
        )

    def list_storage_pools(
        self, limit: Optional[int] = None, offset: Optional[int] = None
    ) -> dict:
        return self._request(
            "GET", "/storage-pools", params=self._page_params(limit, offset)
        )

    def get_storage_pool(self, pool_id: str) -> dict:
        return self._request("GET", f"/storage-pools/{pool_id}")

    # ---- Projects ----

    def create_project(
        self, pool_id: str, name: str, pg_version: Optional[int] = None
    ) -> dict:
        body: dict[str, Any] = {"name": name}
        if pg_version is not None:
            body["pg_version"] = pg_version
        return self._request(
            "POST", f"/storage-pools/{pool_id}/projects", json=body
        )

    def list_projects(
        self, pool_id: str, limit: Optional[int] = None, offset: Optional[int] = None
    ) -> dict:
        return self._request(
            "GET",
            f"/storage-pools/{pool_id}/projects",
            params=self._page_params(limit, offset),
        )

    def get_project(self, pool_id: str, project_id: str) -> dict:
        return self._request(
            "GET", f"/storage-pools/{pool_id}/projects/{project_id}"
        )

    def delete_project(self, pool_id: str, project_id: str) -> dict:
        return self._request(
            "DELETE", f"/storage-pools/{pool_id}/projects/{project_id}"
        )

    # ---- Branches ----

    def create_branch(
        self,
        pool_id: str,
        project_id: str,
        name: str,
        parent_branch_id: Optional[str] = None,
        fork_point_lsn: Optional[str] = None,
    ) -> dict:
        body: dict[str, Any] = {"name": name}
        if parent_branch_id is not None:
            body["parent_branch_id"] = parent_branch_id
        if fork_point_lsn is not None:
            body["fork_point_lsn"] = fork_point_lsn
        return self._request(
            "POST",
            f"/storage-pools/{pool_id}/projects/{project_id}/branches",
            json=body,
        )

    def list_branches(
        self,
        pool_id: str,
        project_id: str,
        limit: Optional[int] = None,
        offset: Optional[int] = None,
    ) -> dict:
        return self._request(
            "GET",
            f"/storage-pools/{pool_id}/projects/{project_id}/branches",
            params=self._page_params(limit, offset),
        )

    def get_branch(self, pool_id: str, project_id: str, branch_id: str) -> dict:
        return self._request(
            "GET",
            f"/storage-pools/{pool_id}/projects/{project_id}/branches/{branch_id}",
        )

    def delete_branch(self, pool_id: str, project_id: str, branch_id: str) -> dict:
        return self._request(
            "DELETE",
            f"/storage-pools/{pool_id}/projects/{project_id}/branches/{branch_id}",
        )

    # ---- Endpoints ----

    def create_endpoint(
        self,
        pool_id: str,
        project_id: str,
        branch_id: str,
        role: Optional[str] = None,
        disk_type: Optional[str] = None,
        cpu: Optional[str] = None,
        memory: Optional[str] = None,
    ) -> dict:
        body: dict[str, Any] = {}
        if role is not None:
            body["role"] = role
        if disk_type is not None:
            body["disk_type"] = disk_type
        resources: dict[str, Any] = {}
        if cpu is not None:
            resources["cpu"] = cpu
        if memory is not None:
            resources["memory"] = memory
        if resources:
            body["resources"] = resources
        return self._request(
            "POST",
            f"/storage-pools/{pool_id}/projects/{project_id}/branches/{branch_id}/endpoints",
            json=body,
        )

    def list_endpoints(
        self,
        pool_id: str,
        project_id: str,
        branch_id: str,
        limit: Optional[int] = None,
        offset: Optional[int] = None,
    ) -> dict:
        return self._request(
            "GET",
            f"/storage-pools/{pool_id}/projects/{project_id}/branches/{branch_id}/endpoints",
            params=self._page_params(limit, offset),
        )

    def get_endpoint(
        self, pool_id: str, project_id: str, branch_id: str, endpoint_id: str
    ) -> dict:
        return self._request(
            "GET",
            f"/storage-pools/{pool_id}/projects/{project_id}/branches/{branch_id}/endpoints/{endpoint_id}",
        )

    def delete_endpoint(
        self, pool_id: str, project_id: str, branch_id: str, endpoint_id: str
    ) -> dict:
        return self._request(
            "DELETE",
            f"/storage-pools/{pool_id}/projects/{project_id}/branches/{branch_id}/endpoints/{endpoint_id}",
        )

    # ---- Tasks ----

    def list_tasks(
        self,
        status: Optional[str] = None,
        kind: Optional[str] = None,
        project_id: Optional[str] = None,
        limit: Optional[int] = None,
        offset: Optional[int] = None,
    ) -> dict:
        params = self._page_params(limit, offset)
        if status is not None:
            params["status"] = status
        if kind is not None:
            params["kind"] = kind
        if project_id is not None:
            params["project_id"] = project_id
        return self._request("GET", "/tasks", params=params)

    def get_task(self, task_id: str) -> dict:
        return self._request("GET", f"/tasks/{task_id}")


controller_client = ControllerClient()
