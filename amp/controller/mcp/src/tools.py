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


def create_cluster(
    pool_id: str, name: str, pg_version: Optional[int] = None
) -> dict[str, Any]:
    """Create a cluster (and its default branch "main") in a pool. ASYNCHRONOUS.

    Returns a Task, not the finished cluster. Poll `get_task(task_id=<id>)`
    until `status` is `succeeded` or `failed`, then call `get_cluster` to read
    the cluster and its `default_branch`. `pg_version` defaults to 18; only 18
    is currently accepted — any other value is rejected with 400, so omit it
    unless you have a reason to set it.
    """
    return controller_client.create_cluster(pool_id, name, pg_version)


def list_clusters(
    pool_id: str, limit: Optional[int] = None, offset: Optional[int] = None
) -> dict[str, Any]:
    """List the clusters in a pool (each with its default branch).

    Paginated via `limit`/`offset`. Items are under `clusters` plus `total` and
    `count`.
    """
    return controller_client.list_clusters(pool_id, limit, offset)


def get_cluster(pool_id: str, cluster_id: str) -> dict[str, Any]:
    """Get a cluster (with its default branch) by id."""
    return controller_client.get_cluster(pool_id, cluster_id)


def delete_cluster(pool_id: str, cluster_id: str) -> dict[str, Any]:
    """Delete a cluster, cascading to its branches and endpoints. ASYNCHRONOUS.

    Returns a Task, not a final result. Poll `get_task(task_id=<id>)` until
    `status` is `succeeded` or `failed`.
    """
    return controller_client.delete_cluster(pool_id, cluster_id)


def create_branch(
    pool_id: str,
    cluster_id: str,
    name: str,
    parent_branch_id: Optional[str] = None,
    fork_point_lsn: Optional[str] = None,
) -> dict[str, Any]:
    """Fork a branch into a new branch in a cluster. ASYNCHRONOUS.

    The parent defaults to the cluster's default branch; pass
    `parent_branch_id` to fork off another branch. `fork_point_lsn` (storage
    "X/X" form) defaults to the parent's latest LSN. Returns a Task; poll
    `get_task(task_id=<id>)` until `status` is `succeeded` or `failed`, then
    call `get_branch` to read the new branch.
    """
    return controller_client.create_branch(
        pool_id, cluster_id, name, parent_branch_id, fork_point_lsn
    )


def list_branches(
    pool_id: str,
    cluster_id: str,
    limit: Optional[int] = None,
    offset: Optional[int] = None,
) -> dict[str, Any]:
    """List the branches in a cluster (default branch plus its forks).

    Paginated via `limit`/`offset`. Items are under `branches` plus `total` and
    `count`.
    """
    return controller_client.list_branches(pool_id, cluster_id, limit, offset)


def get_branch(pool_id: str, cluster_id: str, branch_id: str) -> dict[str, Any]:
    """Get a branch by id."""
    return controller_client.get_branch(pool_id, cluster_id, branch_id)


def delete_branch(
    pool_id: str, cluster_id: str, branch_id: str
) -> dict[str, Any]:
    """Delete a branch, cascading to its endpoints. ASYNCHRONOUS.

    Returns a Task; poll `get_task(task_id=<id>)` until `status` is `succeeded`
    or `failed`. A branch with child branches is rejected synchronously
    (`conflict`).
    """
    return controller_client.delete_branch(pool_id, cluster_id, branch_id)


def create_endpoint(
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
) -> dict[str, Any]:
    """Create and deploy an endpoint (compute) on a branch. ASYNCHRONOUS.

    `role` is `read_write` (default) or `read_only`; a branch admits at most one
    `read_write` endpoint. `disk_type` defaults to `network` (`local` is not yet
    supported and is rejected with 400). `cpu` (e.g. "2", "1500m") and `memory`
    (e.g. "4Gi") are the pod's compute reservation, applied as request == limit
    (Guaranteed QoS); both default to 2 cores / 4Gi. Returns a Task; poll
    `get_task(task_id=<id>)` until `status` is `succeeded` or `failed`, then call
    `get_endpoint` to read its `address` (and `tailnet_address`, if the endpoint
    is exposed on the tailnet).

    Three optional settings maps configure what runs inside the pod. To change
    one later use `update_endpoint_settings` — the endpoint restarts the
    services whose config actually moved, so a `pg_settings` change interrupts
    its Postgres connections.

    `pg_settings` is `{name: value}` PostgreSQL GUCs, values all strings, e.g.
    `{"work_mem": "64MB"}`. They merge over the branch's and win.

    `pageserver_settings` and `safekeeper_settings` override the two storage
    daemons. **Keys may be dotted paths, and for the pageserver's GC knobs they
    must be** — those live under `tenant_config`, not at the top level:

        pageserver_settings={"tenant_config.enable_gc": True,
                             "tenant_config.pitr_interval": "7 days"}

    A flat `{"enable_gc": True}` is a 400, not a silent no-op. Values are scalars
    (bool/int/float/str) — the nesting goes in the key — with durations and sizes
    as strings in the daemons' format ("7 days", "1 GiB"). Unknown keys,
    wrong-typed values and protected identity/wiring keys (`branch_id`, the
    `listen_*` addresses, `yb_storage_conn`, `pg_distrib_dir`,
    `control_plane_api`, `safekeeper_pg_addr`) all 400 naming the key — read that
    message rather than retrying, since it often suggests the dotted form.
    """
    return controller_client.create_endpoint(
        pool_id, cluster_id, branch_id, role, disk_type, cpu, memory,
        pg_settings, pageserver_settings, safekeeper_settings
    )


def list_endpoints(
    pool_id: str,
    cluster_id: str,
    branch_id: str,
    limit: Optional[int] = None,
    offset: Optional[int] = None,
) -> dict[str, Any]:
    """List the endpoints bound to a branch.

    Paginated via `limit`/`offset`. Items are under `endpoints` plus `total` and
    `count`.
    """
    return controller_client.list_endpoints(
        pool_id, cluster_id, branch_id, limit, offset
    )


def get_endpoint(
    pool_id: str, cluster_id: str, branch_id: str, endpoint_id: str
) -> dict[str, Any]:
    """Get an endpoint by id."""
    return controller_client.get_endpoint(
        pool_id, cluster_id, branch_id, endpoint_id
    )


def delete_endpoint(
    pool_id: str, cluster_id: str, branch_id: str, endpoint_id: str
) -> dict[str, Any]:
    """Retire an endpoint (remove its running instance, delete the record). ASYNCHRONOUS.

    Returns a Task; poll `get_task(task_id=<id>)` until `status` is `succeeded`
    or `failed`.
    """
    return controller_client.delete_endpoint(
        pool_id, cluster_id, branch_id, endpoint_id
    )


def resize_endpoint(
    pool_id: str,
    cluster_id: str,
    branch_id: str,
    endpoint_id: str,
    cpu: Optional[str] = None,
    memory: Optional[str] = None,
) -> dict[str, Any]:
    """Change an endpoint's compute reservation (vertical resize). ASYNCHRONOUS.

    Works for either role (a branch's `read_write` endpoint or any `read_only`
    one). `cpu` (e.g. "2", "1500m") and `memory` (e.g. "4Gi") are Kubernetes
    quantities applied as request == limit (Guaranteed QoS); pass at least one
    (an omitted field keeps its current value — sending neither is rejected with
    400). The controller persists the new size, updates the Deployment, and
    resizes the running pod in place where the node allows it. Returns a Task;
    poll `get_task(task_id=<id>)` until `status` is `succeeded` or `failed` — the
    task result carries `in_place_resize` (`completed` / `pending` / `infeasible`
    / `unsupported`). Then call `get_endpoint` to read the new reservation.
    """
    return controller_client.resize_endpoint(
        pool_id, cluster_id, branch_id, endpoint_id, cpu, memory
    )


def update_branch_settings(
    pool_id: str,
    cluster_id: str,
    branch_id: str,
    pg_settings: Optional[dict] = None,
    write_token_settings: Optional[dict] = None,
) -> dict[str, Any]:
    """Change settings that apply to every compute on a branch. SYNCHRONOUS.

    `pg_settings` is a flat `{name: value}` map of PostgreSQL settings — the
    baseline for the branch, which an endpoint's own `pg_settings` merge over
    and win on a shared key. `write_token_settings` is the branch's write lease
    (`{"ttl_ms": 15000, "renew_seconds": 5}`); `renew_seconds` must be under
    `ttl_ms`, since the gap is how long a failing renewal may be retried before
    anything stops writing.

    **An omitted argument is left alone; pass `{}` to clear a surface.** So
    sending only `pg_settings` never touches the lease.

    The settings are stored first — that is what `applied` reports — and the
    controller then tells each of the branch's endpoints. Each restarts the
    services whose config actually changed and reports them in `restarted`; a
    change that renders identical config restarts nothing. Restarting the
    compute bounces Postgres, so a `pg_settings` change interrupts connections
    on **every endpoint on the branch**. An endpoint that could not be reached
    comes back in `unreachable` and picks the change up when it next starts.
    """
    return controller_client.update_branch_settings(
        pool_id, cluster_id, branch_id, pg_settings, write_token_settings
    )


def update_endpoint_settings(
    pool_id: str,
    cluster_id: str,
    branch_id: str,
    endpoint_id: str,
    pg_settings: Optional[dict] = None,
    pageserver_settings: Optional[dict] = None,
    safekeeper_settings: Optional[dict] = None,
) -> dict[str, Any]:
    """Change one endpoint's settings. SYNCHRONOUS.

    `pg_settings` merge over the branch's and win on a shared key.
    `pageserver_settings` and `safekeeper_settings` are this endpoint's alone,
    flat `{key: scalar}` maps whose keys may be **dotted paths**
    (`tenant_config.enable_gc`, which is how the pageserver's GC knobs are
    reached at all). They are validated against the daemons' own config types,
    so an unknown key, a wrong type or a protected key is rejected — a key the
    daemon does not know would abort its startup.

    **An omitted argument is left alone; pass `{}` to clear a surface.**

    Stored first (`applied`), then the endpoint is told: it restarts the
    services whose config actually changed and reports them in `restarted`. A
    change that renders identical config restarts nothing. Restarting the
    compute bounces Postgres, so a `pg_settings` change interrupts this
    endpoint's connections; `unreachable` says why an endpoint could not be
    told, and it picks the change up when it next starts.
    """
    return controller_client.update_endpoint_settings(
        pool_id,
        cluster_id,
        branch_id,
        endpoint_id,
        pg_settings,
        pageserver_settings,
        safekeeper_settings,
    )


def list_tasks(
    status: Optional[str] = None,
    kind: Optional[str] = None,
    cluster_id: Optional[str] = None,
    limit: Optional[int] = None,
    offset: Optional[int] = None,
) -> dict[str, Any]:
    """List write-ahead tasks, most recent first.

    Optionally filter by `status` (`pending`/`running`/`succeeded`/`failed`),
    `kind` (e.g. `create_cluster`), or `cluster_id`. Paginated via
    `limit`/`offset`; items are under `tasks` plus `total` and `count`.
    """
    return controller_client.list_tasks(status, kind, cluster_id, limit, offset)


def get_task(task_id: str) -> dict[str, Any]:
    """Get the status of a task.

    Poll this after any create/delete tool until `status` is `succeeded` or
    `failed`. On failure, `error` carries the reason.
    """
    return controller_client.get_task(task_id)
