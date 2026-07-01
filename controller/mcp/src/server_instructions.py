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

Resources are nested **storage pool → project → branch → endpoint**:

- **Storage pool** — a top-level workspace owned by a user. Create one with \
`create_storage_pool` (you must pass `owner_user_id`).
- **Project** — a database living in a pool. Creating a project also creates \
its default branch named "main".
- **Branch** — a Postgres timeline in a project. `create_branch` forks an \
existing branch (the project's default branch unless you pass \
`parent_branch_id`).
- **Endpoint** — the compute (one container/pod) bound to a branch. A branch \
admits at most one `read_write` endpoint.

Address every nested resource by the full chain of ids (`pool_id`, \
`project_id`, `branch_id`, `endpoint_id`).

## No user create/list; owner_user_id is a per-call argument

User creation and listing are intentionally NOT exposed. There is no \
server-level default user. Wherever the API needs an owner (today only \
`create_storage_pool`), pass `owner_user_id` explicitly. `get_user` is \
available to read a known user by id.

## Asynchronous operations — poll get_task

All `create_*` and `delete_*` tools (projects, branches, endpoints) are \
**asynchronous**. They return a **Task** object immediately, not the finished \
resource. After calling one, take the Task's `id` and poll \
`get_task(task_id=<id>)` until its `status` reaches a terminal value \
(`succeeded` or `failed`). On success, read the resource (e.g. `get_project`, \
`get_branch`, `get_endpoint`) to see its final state, such as a project's \
`default_branch` or an endpoint's `address`. `list_tasks` lists tasks, \
optionally filtered by `status`, `kind`, or `project_id`.

## Pagination

List tools take `limit` (1..1000, default 100) and `offset` (>=0, default 0). \
Responses include `total` (all matches) and `count` (this page) alongside the \
items.

## Errors

Failures raise an error carrying a stable `code` (`invalid_request`, \
`not_found`, `conflict`, `internal`, ...) and a human-readable message.
"""
