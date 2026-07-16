"""MCP tool functions wrapping the amp controller API.

Each tool delegates to the module-level `controller_client` singleton (wired up
in the server lifespan). Create/delete tools are asynchronous on the controller:
they return a Task that the caller must poll via `get_task`.
"""

from typing import Any, Optional

from controller_client import controller_client


def get_user(user_id: str) -> dict[str, Any]:
    """Get a user by id.

    Returns the user (`id`, `is_admin`, `created_at`). User create/list are not
    exposed by this server; this read-only lookup is the only user tool.
    """
    return controller_client.get_user(user_id)


def create_storage_pool(name: str, owner_user_id: str) -> dict[str, Any]:
    """Create a storage pool owned by `owner_user_id`.

    `name` must be unique across the placement group. There is no default user,
    so `owner_user_id` is required. Returns the created storage pool.
    """
    return controller_client.create_storage_pool(name, owner_user_id)


def list_storage_pools(
    limit: Optional[int] = None, offset: Optional[int] = None
) -> dict[str, Any]:
    """List every storage pool in the placement group.

    Paginated via `limit` (1..1000, default 100) and `offset`. The response
    holds the items under `storage_pools` plus `total` and `count`.
    """
    return controller_client.list_storage_pools(limit, offset)


def get_storage_pool(pool_id: str) -> dict[str, Any]:
    """Get a storage pool by id."""
    return controller_client.get_storage_pool(pool_id)


def create_project(
    pool_id: str, name: str, pg_version: Optional[int] = None
) -> dict[str, Any]:
    """Create a project (and its default branch "main") in a pool. ASYNCHRONOUS.

    Returns a Task, not the finished project. Poll `get_task(task_id=<id>)`
    until `status` is `succeeded` or `failed`, then call `get_project` to read
    the project and its `default_branch`. `pg_version` defaults to 17.
    """
    return controller_client.create_project(pool_id, name, pg_version)


def list_projects(
    pool_id: str, limit: Optional[int] = None, offset: Optional[int] = None
) -> dict[str, Any]:
    """List the projects in a pool (each with its default branch).

    Paginated via `limit`/`offset`. Items are under `projects` plus `total` and
    `count`.
    """
    return controller_client.list_projects(pool_id, limit, offset)


def get_project(pool_id: str, project_id: str) -> dict[str, Any]:
    """Get a project (with its default branch) by id."""
    return controller_client.get_project(pool_id, project_id)


def delete_project(pool_id: str, project_id: str) -> dict[str, Any]:
    """Delete a project, cascading to its branches and endpoints. ASYNCHRONOUS.

    Returns a Task, not a final result. Poll `get_task(task_id=<id>)` until
    `status` is `succeeded` or `failed`.
    """
    return controller_client.delete_project(pool_id, project_id)


def create_branch(
    pool_id: str,
    project_id: str,
    name: str,
    parent_branch_id: Optional[str] = None,
    fork_point_lsn: Optional[str] = None,
) -> dict[str, Any]:
    """Fork a branch into a new branch in a project. ASYNCHRONOUS.

    The parent defaults to the project's default branch; pass
    `parent_branch_id` to fork off another branch. `fork_point_lsn` (storage
    "X/X" form) defaults to the parent's latest LSN. Returns a Task; poll
    `get_task(task_id=<id>)` until `status` is `succeeded` or `failed`, then
    call `get_branch` to read the new branch.
    """
    return controller_client.create_branch(
        pool_id, project_id, name, parent_branch_id, fork_point_lsn
    )


def list_branches(
    pool_id: str,
    project_id: str,
    limit: Optional[int] = None,
    offset: Optional[int] = None,
) -> dict[str, Any]:
    """List the branches in a project (default branch plus its forks).

    Paginated via `limit`/`offset`. Items are under `branches` plus `total` and
    `count`.
    """
    return controller_client.list_branches(pool_id, project_id, limit, offset)


def get_branch(pool_id: str, project_id: str, branch_id: str) -> dict[str, Any]:
    """Get a branch by id."""
    return controller_client.get_branch(pool_id, project_id, branch_id)


def delete_branch(
    pool_id: str, project_id: str, branch_id: str
) -> dict[str, Any]:
    """Delete a branch, cascading to its endpoints. ASYNCHRONOUS.

    Returns a Task; poll `get_task(task_id=<id>)` until `status` is `succeeded`
    or `failed`. A branch with child branches is rejected synchronously
    (`conflict`).
    """
    return controller_client.delete_branch(pool_id, project_id, branch_id)


def create_endpoint(
    pool_id: str,
    project_id: str,
    branch_id: str,
    role: Optional[str] = None,
    disk_type: Optional[str] = None,
    cpu: Optional[str] = None,
    memory: Optional[str] = None,
) -> dict[str, Any]:
    """Create and deploy an endpoint (compute) on a branch. ASYNCHRONOUS.

    `role` is `read_write` (default) or `read_only`; a branch admits at most one
    `read_write` endpoint. `disk_type` defaults to `network`. `cpu` (e.g. "2",
    "1500m") and `memory` (e.g. "4Gi") are the pod's compute reservation, applied
    as request == limit (Guaranteed QoS); both default to 2 cores / 4Gi. Returns
    a Task; poll `get_task(task_id=<id>)` until `status` is `succeeded` or
    `failed`, then call `get_endpoint` to read its `address`.
    """
    return controller_client.create_endpoint(
        pool_id, project_id, branch_id, role, disk_type, cpu, memory
    )


def list_endpoints(
    pool_id: str,
    project_id: str,
    branch_id: str,
    limit: Optional[int] = None,
    offset: Optional[int] = None,
) -> dict[str, Any]:
    """List the endpoints bound to a branch.

    Paginated via `limit`/`offset`. Items are under `endpoints` plus `total` and
    `count`.
    """
    return controller_client.list_endpoints(
        pool_id, project_id, branch_id, limit, offset
    )


def get_endpoint(
    pool_id: str, project_id: str, branch_id: str, endpoint_id: str
) -> dict[str, Any]:
    """Get an endpoint by id."""
    return controller_client.get_endpoint(
        pool_id, project_id, branch_id, endpoint_id
    )


def delete_endpoint(
    pool_id: str, project_id: str, branch_id: str, endpoint_id: str
) -> dict[str, Any]:
    """Retire an endpoint (remove its running instance, delete the record). ASYNCHRONOUS.

    Returns a Task; poll `get_task(task_id=<id>)` until `status` is `succeeded`
    or `failed`.
    """
    return controller_client.delete_endpoint(
        pool_id, project_id, branch_id, endpoint_id
    )


def list_tasks(
    status: Optional[str] = None,
    kind: Optional[str] = None,
    project_id: Optional[str] = None,
    limit: Optional[int] = None,
    offset: Optional[int] = None,
) -> dict[str, Any]:
    """List write-ahead tasks, most recent first.

    Optionally filter by `status` (`pending`/`running`/`succeeded`/`failed`),
    `kind` (e.g. `create_project`), or `project_id`. Paginated via
    `limit`/`offset`; items are under `tasks` plus `total` and `count`.
    """
    return controller_client.list_tasks(status, kind, project_id, limit, offset)


def get_task(task_id: str) -> dict[str, Any]:
    """Get the status of a task.

    Poll this after any create/delete tool until `status` is `succeeded` or
    `failed`. On failure, `error` carries the reason.
    """
    return controller_client.get_task(task_id)
