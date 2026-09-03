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

    def rename_storage_pool(self, pool_id: str, name: str) -> dict:
        return self._request(
            "PATCH", f"/storage-pools/{pool_id}", json={"name": name}
        )

    def delete_storage_pool(self, pool_id: str) -> dict:
        return self._request("DELETE", f"/storage-pools/{pool_id}")

    # ---- Clusters ----

    def create_cluster(
        self,
        pool_id: str,
        name: str,
        pg_version: Optional[int] = None,
        engine: Optional[str] = None,
        extensions: Optional[list[str]] = None,
    ) -> dict:
        body: dict[str, Any] = {"name": name}
        if pg_version is not None:
            body["pg_version"] = pg_version
        if engine:
            body["engine"] = engine
        if extensions:
            body["extensions"] = extensions
        return self._request(
            "POST", f"/storage-pools/{pool_id}/clusters", json=body
        )

    def add_branch_extensions(
        self, pool_id: str, cluster_id: str, branch_id: str, extensions: list[str]
    ) -> dict:
        return self._request(
            "POST",
            f"/storage-pools/{pool_id}/clusters/{cluster_id}"
            f"/branches/{branch_id}/extensions",
            json={"extensions": extensions},
        )

    def list_clusters(
        self, pool_id: str, limit: Optional[int] = None, offset: Optional[int] = None
    ) -> dict:
        return self._request(
            "GET",
            f"/storage-pools/{pool_id}/clusters",
            params=self._page_params(limit, offset),
        )

    def get_cluster(self, pool_id: str, cluster_id: str) -> dict:
        return self._request(
            "GET", f"/storage-pools/{pool_id}/clusters/{cluster_id}"
        )

    def delete_cluster(self, pool_id: str, cluster_id: str) -> dict:
        return self._request(
            "DELETE", f"/storage-pools/{pool_id}/clusters/{cluster_id}"
        )

    # ---- Branches ----

    def create_branch(
        self,
        pool_id: str,
        cluster_id: str,
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
            f"/storage-pools/{pool_id}/clusters/{cluster_id}/branches",
            json=body,
        )

    def list_branches(
        self,
        pool_id: str,
        cluster_id: str,
        limit: Optional[int] = None,
        offset: Optional[int] = None,
    ) -> dict:
        return self._request(
            "GET",
            f"/storage-pools/{pool_id}/clusters/{cluster_id}/branches",
            params=self._page_params(limit, offset),
        )

    def get_branch(self, pool_id: str, cluster_id: str, branch_id: str) -> dict:
        return self._request(
            "GET",
            f"/storage-pools/{pool_id}/clusters/{cluster_id}/branches/{branch_id}",
        )

    def delete_branch(self, pool_id: str, cluster_id: str, branch_id: str) -> dict:
        return self._request(
            "DELETE",
            f"/storage-pools/{pool_id}/clusters/{cluster_id}/branches/{branch_id}",
        )

    # ---- Endpoints ----

    def create_endpoint(
        self,
        pool_id: str,
        cluster_id: str,
        branch_id: str,
        role: Optional[str] = None,
        disk_type: Optional[str] = None,
        cpu: Optional[str] = None,
        memory: Optional[str] = None,
        pg_settings: Optional[dict[str, str]] = None,
        pageserver_settings: Optional[dict[str, Any]] = None,
        safekeeper_settings: Optional[dict[str, Any]] = None,
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
        # Omitted when empty: there is nothing to say by sending `{}`.
        if pg_settings:
            body["pg_settings"] = pg_settings
        if pageserver_settings:
            body["pageserver_settings"] = pageserver_settings
        if safekeeper_settings:
            body["safekeeper_settings"] = safekeeper_settings
        return self._request(
            "POST",
            f"/storage-pools/{pool_id}/clusters/{cluster_id}/branches/{branch_id}/endpoints",
            json=body,
        )

    def list_endpoints(
        self,
        pool_id: str,
        cluster_id: str,
        branch_id: str,
        limit: Optional[int] = None,
        offset: Optional[int] = None,
    ) -> dict:
        return self._request(
            "GET",
            f"/storage-pools/{pool_id}/clusters/{cluster_id}/branches/{branch_id}/endpoints",
            params=self._page_params(limit, offset),
        )

    def get_endpoint(
        self, pool_id: str, cluster_id: str, branch_id: str, endpoint_id: str
    ) -> dict:
        return self._request(
            "GET",
            f"/storage-pools/{pool_id}/clusters/{cluster_id}/branches/{branch_id}/endpoints/{endpoint_id}",
        )

    def delete_endpoint(
        self, pool_id: str, cluster_id: str, branch_id: str, endpoint_id: str
    ) -> dict:
        return self._request(
            "DELETE",
            f"/storage-pools/{pool_id}/clusters/{cluster_id}/branches/{branch_id}/endpoints/{endpoint_id}",
        )

    def resize_endpoint(
        self,
        pool_id: str,
        cluster_id: str,
        branch_id: str,
        endpoint_id: str,
        cpu: Optional[str] = None,
        memory: Optional[str] = None,
    ) -> dict:
        # The controller nests the new reservation under `resources` and requires
        # a non-empty body; an omitted field keeps its current value. Same shape
        # as create_endpoint's resources block.
        resources: dict[str, Any] = {}
        if cpu is not None:
            resources["cpu"] = cpu
        if memory is not None:
            resources["memory"] = memory
        return self._request(
            "POST",
            f"/storage-pools/{pool_id}/clusters/{cluster_id}/branches/{branch_id}/endpoints/{endpoint_id}/resize",
            json={"resources": resources},
        )

    def update_branch_settings(
        self,
        pool_id: str,
        cluster_id: str,
        branch_id: str,
        pg_settings: Optional[dict] = None,
        write_token_settings: Optional[dict] = None,
    ) -> dict:
        # Only the named surfaces are sent. An omitted key means "leave it
        # alone" and `{}` means "clear it", so the two cannot be collapsed —
        # dropping a None would be indistinguishable from clearing.
        body: dict[str, Any] = {}
        if pg_settings is not None:
            body["pg_settings"] = pg_settings
        if write_token_settings is not None:
            body["write_token_settings"] = write_token_settings
        return self._request(
            "PATCH",
            f"/storage-pools/{pool_id}/clusters/{cluster_id}/branches/{branch_id}/settings",
            json=body,
        )

    def update_endpoint_settings(
        self,
        pool_id: str,
        cluster_id: str,
        branch_id: str,
        endpoint_id: str,
        pg_settings: Optional[dict] = None,
        pageserver_settings: Optional[dict] = None,
        safekeeper_settings: Optional[dict] = None,
    ) -> dict:
        body: dict[str, Any] = {}
        if pg_settings is not None:
            body["pg_settings"] = pg_settings
        if pageserver_settings is not None:
            body["pageserver_settings"] = pageserver_settings
        if safekeeper_settings is not None:
            body["safekeeper_settings"] = safekeeper_settings
        return self._request(
            "PATCH",
            f"/storage-pools/{pool_id}/clusters/{cluster_id}/branches/{branch_id}"
            f"/endpoints/{endpoint_id}/settings",
            json=body,
        )

    # ---- Tasks ----

    def list_tasks(
        self,
        status: Optional[str] = None,
        kind: Optional[str] = None,
        cluster_id: Optional[str] = None,
        limit: Optional[int] = None,
        offset: Optional[int] = None,
    ) -> dict:
        params = self._page_params(limit, offset)
        if status is not None:
            params["status"] = status
        if kind is not None:
            params["kind"] = kind
        if cluster_id is not None:
            params["cluster_id"] = cluster_id
        return self._request("GET", "/tasks", params=params)

    def get_task(self, task_id: str) -> dict:
        return self._request("GET", f"/tasks/{task_id}")

    # ---- Migrations ----

    @staticmethod
    def _migration_source(
        db_type: str,
        host: str,
        port: int,
        username: str,
        password: str,
        database: str,
        schemas: list[str],
    ) -> dict:
        # MigrationSourceRequest: every field is required by the controller,
        # including at least one schema. `password` is stored for the Voyager
        # jobs and never echoed back in any response.
        return {
            "db_type": db_type,
            "host": host,
            "port": port,
            "username": username,
            "password": password,
            "database": database,
            "schemas": schemas,
        }

    @staticmethod
    def _with_migration_id(payload: Any) -> Any:
        """Copy controller `id` onto `migration_id` so agents can reuse one name.

        The REST resource still uses `id` (same as pools/clusters). The MCP
        tools take `migration_id` on every later call, so echoing that name
        on the create/get/list responses removes the id vs migration_id split.
        """
        if isinstance(payload, dict) and "id" in payload and "migration_id" not in payload:
            payload = dict(payload)
            payload["migration_id"] = payload["id"]
        return payload

    def test_source_connectivity(
        self,
        db_type: str,
        host: str,
        port: int,
        username: str,
        password: str,
        database: str,
        schemas: list[str],
    ) -> dict:
        source = self._migration_source(
            db_type, host, port, username, password, database, schemas
        )
        return self._request(
            "POST", "/migrations/connectivity-checks", json={"source": source}
        )

    def create_migration(
        self,
        name: str,
        db_type: str,
        host: str,
        port: int,
        username: str,
        password: str,
        database: str,
        schemas: list[str],
    ) -> dict:
        source = self._migration_source(
            db_type, host, port, username, password, database, schemas
        )
        return self._with_migration_id(
            self._request(
                "POST", "/migrations", json={"name": name, "source": source}
            )
        )

    def list_migrations(
        self, limit: Optional[int] = None, offset: Optional[int] = None
    ) -> dict:
        result = self._request(
            "GET", "/migrations", params=self._page_params(limit, offset)
        )
        items = result.get("migrations")
        if isinstance(items, list):
            result = dict(result)
            result["migrations"] = [
                self._with_migration_id(item) if isinstance(item, dict) else item
                for item in items
            ]
        return result

    def get_migration(self, migration_id: str) -> dict:
        return self._with_migration_id(
            self._request("GET", f"/migrations/{migration_id}")
        )

    def get_migration_assessment(self, migration_id: str) -> dict:
        return self._request("GET", f"/migrations/{migration_id}/assessment")

    def start_migration_assessment(self, migration_id: str) -> dict:
        # No request body; the launcher task is planned entirely from the
        # migration's persisted source config.
        return self._request("POST", f"/migrations/{migration_id}/assess")

    def provision_migration_target(
        self,
        migration_id: str,
        pool_id: str,
        vcpu: Optional[int] = None,
        project_name: Optional[str] = None,
    ) -> dict:
        body: dict[str, Any] = {"pool_id": pool_id}
        if vcpu is not None:
            body["vcpu"] = vcpu
        if project_name is not None:
            body["project_name"] = project_name
        return self._request(
            "POST", f"/migrations/{migration_id}/provision-target", json=body
        )

    def start_migration(
        self, migration_id: str, parallel_jobs: Optional[int] = None
    ) -> dict:
        body: dict[str, Any] = {}
        if parallel_jobs is not None:
            body["parallel_jobs"] = parallel_jobs
        return self._request(
            "POST", f"/migrations/{migration_id}/migrate", json=body
        )


controller_client = ControllerClient()
