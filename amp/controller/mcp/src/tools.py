"""MCP tool functions wrapping the amp controller API.

Each tool delegates to the module-level `controller_client` singleton (wired up
in the server lifespan). Create/delete tools are asynchronous on the controller:
they return a Task that the caller must poll via `get_task` — except the
migration launchers (`start_migration_assessment`, `provision_migration_target`,
`start_migration`), whose progress is tracked via `get_migration` instead.
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
    pool_id: str,
    name: str,
    pg_version: Optional[int] = None,
    extensions: Optional[list[str]] = None,
) -> dict[str, Any]:
    """Create a cluster (and its default branch "main") in a pool. ASYNCHRONOUS.

    Returns a Task, not the finished cluster. Poll `get_task(task_id=<id>)`
    until `status` is `succeeded` or `failed`, then call `get_cluster` to read
    the cluster and its `default_branch`. `pg_version` defaults to 18; only 18
    is currently accepted — any other value is rejected with 400, so omit it
    unless you have a reason to set it.

    `extensions` enables externalized PostgreSQL extensions (by catalog name,
    e.g. `["pgvector"]`) on the cluster's default branch "main": every endpoint
    on that branch mounts them, and a fork inherits the set. Add more to a branch
    later with `add_branch_extensions`. An unknown name is rejected with 400, as
    is any value when the controller has no extension catalog (e.g. the Docker
    driver).
    """
    return controller_client.create_cluster(pool_id, name, pg_version, extensions)


def add_branch_extensions(
    pool_id: str, cluster_id: str, branch_id: str, extensions: list[str]
) -> dict[str, Any]:
    """Enable externalized PostgreSQL extensions on a branch. ASYNCHRONOUS.

    Adding is per branch: the names (catalog names, e.g. `["pgvector"]`) are
    unioned into this branch's set and every existing endpoint on the branch is
    redeployed to mount them, so `CREATE EXTENSION` resolves. Idempotent — an
    already-enabled extension is a no-op. Returns a Task; poll
    `get_task(task_id=<id>)` until `status` is `succeeded` or `failed`, then read
    the branch's `extensions` via `get_branch`. An unknown name, an empty list,
    or a controller without an extension catalog (e.g. the Docker driver) is
    rejected with 400.
    """
    return controller_client.add_branch_extensions(
        pool_id, cluster_id, branch_id, extensions
    )


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
    (Guaranteed QoS); both default to 2 cores / 4Gi. `memory` needs an explicit
    unit of Mi or larger — a unitless "4" is four BYTES and is rejected with 400.
    Returns a Task; poll `get_task(task_id=<id>)` until `status` is `succeeded`
    or `failed`, then call `get_endpoint` to read its `address` (and
    `tailnet_address`, if the endpoint is exposed on the tailnet).

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
    400). `memory` needs an explicit unit of Mi or larger; a unitless "4" is four
    BYTES and is rejected with 400. The controller persists the new size, updates
    the Deployment, and resizes the running pod in place where the node allows
    it. Returns a Task; poll `get_task(task_id=<id>)` until `status` is
    `succeeded` or `failed` — the task result carries `in_place_resize`
    (`completed` / `pending` / `infeasible` / `unsupported`). Then call
    `get_endpoint` to read the new reservation.
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
    controller then tells each of the branch's endpoints. Each applies what it
    can in place: `reloaded` lists services that took the change while running,
    `restarted` those that had to be bounced, and `restart_forced_by` names the
    Postgres setting that forced a restart. Most Postgres settings reload; the
    ones fixed at server start restart the compute and drop its connections —
    on a branch, on every endpoint of it. `not_applied` lists settings that are
    stored and not in force, each as `{name, reason}` — `unrecognized parameter`
    for a typo, `invalid value` for one Postgres cannot parse, and so on. Nothing
    is restarted for these, so it is the only signal the edit did nothing.
    An endpoint that could not be reached comes back in `unreachable` and picks
    the change up when it next starts.
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

    Stored first (`applied`), then the endpoint is told: `reloaded` lists what
    took the change while running, `restarted` what had to be bounced, and
    `restart_forced_by` names the Postgres setting that forced it. Most Postgres
    settings reload; ones fixed at server start restart the compute and drop its
    connections. `pageserver_settings` / `safekeeper_settings` always restart
    that daemon — they have no reload path. `not_applied` lists settings that are
    stored and not in force as `{name, reason}`, which nothing restarts for.
    `unreachable` says why an endpoint could not be told; it picks the change up
    when it next starts.
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


# ---- Migrations ----


def test_source_connectivity(
    host: str,
    port: int,
    username: str,
    password: str,
    database: str,
    schemas: list[str],
    db_type: str = "postgresql",
) -> dict[str, Any]:
    """Probe a source database's reachability before creating a migration.

    Stateless: dials the source Postgres from inside the cluster and reports
    `reachable`, `server_version`, `ssl`, and `latency_ms`. Nothing is
    persisted and no migration need exist yet. An unreachable source is still a
    success with `reachable=false` and a redacted `error` — only a malformed
    request errors. Credentials are never echoed back. `db_type` must be
    `postgresql` (the only supported source); `schemas` needs at least one
    entry (e.g. `["public"]`).
    """
    return controller_client.test_source_connectivity(
        db_type, host, port, username, password, database, schemas
    )


def create_migration(
    name: str,
    host: str,
    port: int,
    username: str,
    password: str,
    database: str,
    schemas: list[str],
    db_type: str = "postgresql",
) -> dict[str, Any]:
    """Create a migration from a name and a source Postgres config. SYNCHRONOUS.

    Records the migration and returns it immediately (`state=created`); no
    export volume or target compute is created yet. Save the returned
    `migration_id` (same value as `id`) and pass it to every later call.
    Full flow: optional `test_source_connectivity` → have a `pool_id`
    (`create_storage_pool` or an existing pool) → `create_migration` →
    `start_migration_assessment` → `get_migration_assessment` →
    `provision_migration_target(migration_id, pool_id, vcpu)` →
    `start_migration`, polling `get_migration` between launchers. The
    `password` is stored for the Voyager jobs but never returned. `db_type`
    must be `postgresql`; `schemas` needs at least one entry.
    """
    return controller_client.create_migration(
        name, db_type, host, port, username, password, database, schemas
    )


def list_migrations(
    limit: Optional[int] = None, offset: Optional[int] = None
) -> dict[str, Any]:
    """List migration summaries, most recent first.

    Each item is a lightweight summary (`id`/`migration_id`, `name`, `state`,
    `detail`, timestamps) — not the full `get_migration` status (no live job
    refresh, no source/target/runs/progress). `state` can therefore lag until
    the next `get_migration`. Paginated via `limit`/`offset`. The response is
    `{migrations, total, count}` where `total` is every matching migration
    across pages and `count` is how many summaries are in this page.
    """
    return controller_client.list_migrations(limit, offset)


def get_migration(migration_id: str) -> dict[str, Any]:
    """Get a migration's full, derived status. The primary polling surface.

    Returns the current derived `state` (`created`, `assessing`, `assessed`,
    `provisioning_target`, `target_ready`, `migrating`, `completed`, `failed`),
    a human `detail`, the source (password redacted), the attached target if
    any, the latest assess/migrate runs, and Voyager phase / per-table
    progress. Poll THIS (not `get_task`) after any async migration launcher
    to track progress. Passwords never appear.
    """
    return controller_client.get_migration(migration_id)


def get_migration_assessment(migration_id: str) -> dict[str, Any]:
    """Get the parsed assessment report for a migration.

    Returns the agent-relevant projection of the latest completed assessment:
    migration complexity, source database size facts, a compute sizing
    recommendation, and the flattened issues — enough to choose a target
    `vcpu` for `provision_migration_target`. Errors with `not_found` if no
    completed assessment exists yet (run `start_migration_assessment` and poll
    `get_migration` until `state` is `assessed`).
    """
    return controller_client.get_migration_assessment(migration_id)


def start_migration_assessment(migration_id: str) -> dict[str, Any]:
    """Start an assessment run for a migration. ASYNCHRONOUS.

    Plans a launcher task that records an assess run and submits the Voyager
    assess job, returning a **Task** that covers only the launch. Do NOT poll
    `get_task` for progress — poll `get_migration(migration_id=<id>)` until
    `state` reaches `assessed` (or `failed`), then read the report with
    `get_migration_assessment`. Errors: `not_found` if the migration is
    unknown, `run_in_progress` if a run is already in flight,
    `not_implemented` (501) if the Voyager job runner is not configured.
    """
    return controller_client.start_migration_assessment(migration_id)


def provision_migration_target(
    migration_id: str,
    pool_id: str,
    vcpu: Optional[int] = None,
    project_name: Optional[str] = None,
) -> dict[str, Any]:
    """Provision and attach a right-sized AMP target for a migration. ASYNCHRONOUS.

    Plans a task that creates a target cluster (its default branch + a
    read_write endpoint) in `pool_id` and attaches it to the migration, then
    returns a **Task** for the launch only. Size it with `vcpu`, chosen from
    `get_migration_assessment`; omit to take the deployer default.
    `project_name` defaults to the migration name. Poll
    `get_migration(migration_id=<id>)` until `state` reaches `target_ready` (or
    `failed`). Errors: `not_found`; `target_conflict` if a target is already
    attached or a provision task is in flight; `not_implemented` (501) without
    the endpoint deployer.
    """
    return controller_client.provision_migration_target(
        migration_id, pool_id, vcpu, project_name
    )


def start_migration(
    migration_id: str, parallel_jobs: Optional[int] = None
) -> dict[str, Any]:
    """Start the offline migration (export from source, import into target). ASYNCHRONOUS.

    Requires a target attached first (`provision_migration_target`); errors
    with `target_not_attached` otherwise. Plans a launcher task that records a
    migrate run and submits the Voyager migrate job, returning a **Task** for
    the launch only. `parallel_jobs` tunes `import data --parallel-jobs`
    (defaults to 8). Poll `get_migration(migration_id=<id>)` until `state`
    reaches `completed` (or `failed`). Errors: `not_found`;
    `target_not_attached` if no target is attached yet; `run_in_progress` if a
    run is already in flight; `not_implemented` (501) without the Voyager job
    runner.
    """
    return controller_client.start_migration(migration_id, parallel_jobs)
