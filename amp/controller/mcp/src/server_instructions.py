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

A setting nothing can apply — a name Postgres does not know, or one compiled \
into the binary — comes back in `not_applied`. Nothing is restarted for it, \
because a restart would not put it in force either. Tell the user: the value is \
stored and doing nothing, and a silent success is how a typo survives.

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

## Pagination

List tools take `limit` (1..1000, default 100) and `offset` (>=0, default 0). \
Responses include `total` (all matches) and `count` (this page) alongside the \
items.

## Errors

Failures raise an error carrying a stable `code` (`invalid_request`, \
`not_found`, `conflict`, `internal`, ...) and a human-readable message.
"""
