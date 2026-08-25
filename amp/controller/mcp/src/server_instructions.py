"""MCP server instructions — surfaced to every connected client so LLMs can
describe the amp controller's capabilities without enumerating raw tools.

Kept in its own module (no heavy imports) so tests can assert on the string
cheaply.
"""

AMP_CONTROLLER_SERVER_INSTRUCTIONS = """\
This server wraps the **amp controller** control-plane REST API as MCP tools. \
The controller manages a single placement group of Postgres-compatible \
databases backed by branchable storage.

## Resource ladder

Resources are nested **storage pool → cluster → branch → endpoint**:

- **Storage pool** — a top-level workspace owned by a user. Create one with \
`create_storage_pool` (you must pass `owner_user_id`).
- **Cluster** — a database living in a pool. Creating a cluster also creates \
its default branch named "main".
- **Branch** — a Postgres timeline in a cluster. `create_branch` forks an \
existing branch (the cluster's default branch unless you pass \
`parent_branch_id`).
- **Endpoint** — the compute (one container/pod) bound to a branch. A branch \
admits at most one `read_write` endpoint.

Address every nested resource by the full chain of ids (`pool_id`, \
`cluster_id`, `branch_id`, `endpoint_id`).

## No user create/list; owner_user_id is a per-call argument

User creation and listing are intentionally NOT exposed. There is no \
server-level default user. Wherever the API needs an owner (today only \
`create_storage_pool`), pass `owner_user_id` explicitly. `get_user` is \
available to read a known user by id.

## Settings changes are usually applied without an interruption

`update_branch_settings` and `update_endpoint_settings` are synchronous. An \
omitted surface is left alone; `{}` clears it. They store the change, then each \
endpoint applies it the cheapest way that actually applies it and says which: \
`reloaded` for settings that took effect while the server kept running, \
`restarted` for services that had to be bounced, and `restart_forced_by` naming \
the Postgres setting that forced one.

Most Postgres settings reload. A few — `shared_buffers` and friends — can only \
change when the server starts, and those restart the compute and drop its \
connections; on a branch, on every endpoint of it. Warn the user before doing \
that to anything live, and report `restart_forced_by` when it happens so they \
know which setting cost them. A storage daemon's settings \
(`pageserver_settings`, `safekeeper_settings`) always restart that daemon — \
those have no reload path.

A setting Postgres will not take — a name it does not know, a value it cannot \
parse — comes back in `not_applied` as `{name, reason}`, and is deliberately not \
written to the config file at all, because a file containing one does not load. \
Nothing is restarted for it. Report the reason, not just the name: \
`unrecognized parameter` is a typo to fix, `invalid value` is the wrong value \
for a real setting. A silent success is how either survives.

## Asynchronous operations — poll get_task

All `create_*` and `delete_*` tools (clusters, branches, endpoints) plus \
`resize_endpoint` are **asynchronous**. They return a **Task** object \
immediately, not the finished \
resource. After calling one, take the Task's `id` and poll \
`get_task(task_id=<id>)` until its `status` reaches a terminal value \
(`succeeded` or `failed`). On success, read the resource (e.g. `get_cluster`, \
`get_branch`, `get_endpoint`) to see its final state, such as a cluster's \
`default_branch` or an endpoint's `address` (and `tailnet_address`, if the \
endpoint is exposed on the tailnet). `list_tasks` lists tasks, optionally \
filtered by `status`, `kind`, or `cluster_id`.

## Migrations — move a Postgres database onto AMP

A second family of tools drives Voyager database migrations from an external \
Postgres source into a freshly provisioned AMP cluster. This is a linear \
workflow, keyed by a single `migration_id`:

1. `test_source_connectivity` — optional pre-check that the source Postgres is \
reachable (stateless; nothing is created).
2. `create_storage_pool` — create the workspace that will hold the target \
cluster, or reuse an existing pool via `list_storage_pools` / \
`get_storage_pool`. Save the pool's `id` as `pool_id`.
3. `create_migration` — record the migration from a name + source config. \
Synchronous; returns the migration with `state=created`. Save the returned \
`id` (also present as `migration_id`) and reuse it on every later call.
4. `start_migration_assessment` — kick off the Voyager assessment, then poll \
`get_migration` until `state=assessed`.
5. `get_migration_assessment` — read the sizing report and pick `vcpu`.
6. `provision_migration_target` — create and attach a right-sized target \
cluster in `pool_id` (the storage-pool id from step 2). Poll \
`get_migration` until `state=target_ready`.
7. `start_migration` — run the offline export/import. Poll `get_migration` \
until `state=completed` (or `failed`).

**Poll `get_migration`, not `get_task`, for migration progress.** The three \
launcher tools (`start_migration_assessment`, `provision_migration_target`, \
`start_migration`) are asynchronous, but the Task they return only covers the \
launch. Track real progress by polling `get_migration(migration_id=<id>)` and \
watching its derived `state` advance through `created` → `assessing` → \
`assessed` → `provisioning_target` → `target_ready` → `migrating` → \
`completed` (or `failed`). `list_migrations` lists them, most recent first. \
Source passwords are stored for the jobs but never returned in any response.

## Pagination

List tools take `limit` (1..1000, default 100) and `offset` (>=0, default 0). \
Responses include `total` (all matches) and `count` (this page) alongside the \
items.

## Errors

Failures raise an error carrying a stable `code` (`invalid_request`, \
`not_found`, `conflict`, `internal`, ...) and a human-readable message.
"""
