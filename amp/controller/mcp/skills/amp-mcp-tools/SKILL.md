---
name: amp-mcp-tools
description: Behavioral guide for AI agents using the amp controller MCP tools. Triggers when creating or managing Postgres storage pools, clusters (databases), branches (timelines), or endpoints (compute) through the amp controller, or when polling amp tasks.
license: MIT
metadata:
  author: amp
  version: "1.0.0"
  tags: mcp, tools, amp, postgres, storage-pool, cluster, branch, endpoint, controller
---

# amp MCP Tools — Agent Behavioral Guide

The amp controller manages a placement group of Postgres-compatible databases
on branchable storage. This server exposes its control-plane REST API as **21
MCP tools**. This skill teaches you how to call them correctly on the first try
— above all, the **asynchronous task model**, which is the single most common
way agents get amp wrong.

## When to use this skill

- You are calling tools on an **amp controller** MCP server.
- You are creating/listing/deleting storage pools, clusters, branches, or endpoints.
- You provisioned or resized compute and need to know when it is actually ready.
- You hit a `conflict` / `invalid_request` error and want to know why.

## The resource ladder — everything nests

Resources form a strict hierarchy; you address a nested resource by the **full
chain of ids**:

```
storage pool          (a workspace, owned by a user)
└── cluster           (a database; created with a default branch named "main")
    └── branch         (a Postgres timeline; fork any branch into a new one)
        └── endpoint    (the compute — one pod — you actually connect to)
```

- **Storage pool** — top-level workspace. `create_storage_pool(name, owner_user_id)`.
- **Cluster** — a database in a pool. Creating one also creates its `main` branch.
- **Branch** — a timeline. `create_branch` forks an existing branch (defaults to `main`).
- **Endpoint** — compute bound to a branch. A branch admits **at most one `read_write`** endpoint (plus any number of `read_only`).

Every cluster/branch/endpoint call needs the ids above it: `pool_id`, then
`cluster_id`, then `branch_id`, then `endpoint_id`.

## ⚠️ The async model — the thing to get right

**Every `create_*`, `delete_*`, and `resize_endpoint` call is asynchronous.**
It does **not** return the finished resource — it returns a **Task**:

```json
{ "id": "t_abc", "kind": "create_endpoint", "status": "pending" }
```

The mandatory pattern for every mutation:

1. Call the tool → capture the returned Task `id`.
2. **Poll `get_task(task_id=<id>)`** until `status` is terminal: `succeeded` or `failed`.
3. On `succeeded`, **read the resource** (`get_cluster` / `get_branch` / `get_endpoint`) to see its final state — e.g. a cluster's `default_branch`, or an endpoint's `address` (and `tailnet_address` if tailnet-exposed).
4. On `failed`, the Task's `error` field says why.

Do **not** treat the Task as the resource. A freshly created endpoint has **no
`address` in the Task** — the address only exists after the task succeeds and
you `get_endpoint`. `list_tasks(status=, kind=, cluster_id=)` finds tasks if you
lose an id. The synchronous exceptions are reads (`get_*`, `list_*`) and
`create_storage_pool` (returns the pool directly).

## Roles and the one-read-write rule

`create_endpoint(role=...)` takes `read_write` (default) or `read_only`. **A
branch may have only one `read_write` endpoint** — a second `read_write` on the
same branch is rejected with `conflict`. Provision additional compute as
`read_only`, or put the writer on its own branch. `disk_type` defaults to
`network` (`local` is not yet supported and is rejected with
`unsupported_disk_type`).

## Settings: dotted keys reach the nested ones

`create_endpoint` takes three settings maps. They can also be changed
afterwards — see "Changing settings after create" below, which is the part with
a cost, since applying a change restarts the services whose config moved.

`pg_settings={"work_mem": "64MB"}` are PostgreSQL GUCs (values are strings) and
merge over the branch's, winning on a shared key.

`pageserver_settings` / `safekeeper_settings` configure the two storage daemons
in the pod. **A key may be a dotted path into a nested toml table, and for the
pageserver's GC knobs it must be** — they live under `tenant_config`, not at the
top level:

```
create_endpoint(..., pageserver_settings={"tenant_config.enable_gc": True,
                                          "tenant_config.pitr_interval": "7 days"})
```

A flat `{"enable_gc": True}` is a `400`, not a silent no-op. Values must be
scalars — the nesting goes in the **key**, never in the value — with durations
and sizes as strings in the daemons' own format (`"7 days"`, `"1 GiB"`).

Unknown keys, wrong types, and the protected identity/wiring keys (`branch_id`,
the `listen_*` addresses, `yb_storage_conn`, `pg_distrib_dir`,
`control_plane_api`, `safekeeper_pg_addr`) all return `400` naming the key.
**Read that message rather than retrying** — when a flat key has a nested form it
suggests it. Validation is against a key set generated from the daemons' own
config types, so it tracks the binaries rather than a hand-kept list.

The supervisor reads them from the endpoint's row when the pod comes up, which
is why applying a later change costs a restart of the services whose config
moved.

## Changing settings after create

`update_branch_settings(pg_settings?, write_token_settings?)` and
`update_endpoint_settings(pg_settings?, pageserver_settings?,
safekeeper_settings?)` are **synchronous**, unlike everything else that
changes a resource.

**Omitted means "leave it alone"; `{}` means "clear it".** Sending only
`write_token_settings` does not touch `pg_settings`. This is the mistake worth
avoiding: passing an empty map for a surface you did not mean to change wipes
it.

They store the settings first, then tell the running endpoints. Each restarts
the services whose config actually changed and reports them in `restarted`; a
change that renders identical config restarts nothing, and `unreachable` says
why an endpoint could not be told (it picks the change up when it next starts).

**Restarting the compute bounces Postgres.** A `pg_settings` change interrupts
connections on the endpoint — and on a *branch*, on every endpoint on it. Say so
before making one on anything live.

## Resize outcomes

`resize_endpoint(cpu?, memory?)` works for either role. Pass **at least one** of
`cpu` / `memory` (an omitted field keeps its current value; sending neither →
400). It is async like the rest, but its **task result carries an
`in_place_resize` field** telling you how the change landed:

| `in_place_resize` | What actually happened |
|---|---|
| `completed` | The **running pod** was resized live to the new size. |
| `pending` | The new size is **persisted as the desired reservation only**. The running pod is unchanged; it picks the size up on a future **controller-driven redeploy**. An involuntary restart may still come back at the old size (old Deployment template), so `pending` does **not** mean "applies on the next restart". |
| `infeasible` | The live resize is not feasible on the current node. The new size is **stored as desired** for a later controller-driven redeploy; the running pod keeps its current size. |
| `unsupported` | Live resize is not supported here. The new size is **stored as desired** for a future controller-driven redeploy. **No rollout is triggered.** |

**Only `completed` changes the live pod.** For `pending` / `infeasible` /
`unsupported` the change is **desired-only**: the controller records the target
resources for a future controller-driven redeploy, and the running pod keeps its
current size until then. A later `get_endpoint` returns the **stored (desired)
reservation**, so it confirms what was requested, not that the running pod was
actually resized. cpu/memory are Kubernetes quantities (`"2"`, `"1500m"`,
`"4Gi"`) applied as request == limit (Guaranteed QoS); both default to 2 cores /
4Gi on create.

## Users — no create/list; owner is a per-call arg

User creation and listing are **intentionally not exposed**, and there is no
server-level default user. Wherever an owner is needed (today only
`create_storage_pool`), pass `owner_user_id` explicitly. `get_user(user_id)`
reads a known user. If you don't have a user id, ask — don't invent one.

## Pagination

List tools take `limit` (1..1000, default 100) and `offset` (>=0, default 0).
Responses wrap items under the resource name (`storage_pools`, `clusters`,
`branches`, `endpoints`, `tasks`) plus `total` (all matches) and `count` (this
page).

## Errors

Failures raise an error with a stable `code` and a human message. Common ones:

- `invalid_request` — bad/missing arg (e.g. resize with neither cpu nor memory; `pg_version` other than 18; a `create_branch` with no `fork_point_lsn` whose parent has no reachable writer, see the fork precondition below).
- `unsupported_disk_type` — `disk_type: local` (only `network` is supported today).
- `not_found` — a wrong id in the chain.
- `conflict` — a second `read_write` on a branch, or deleting a branch that still has child branches.
- `internal` — controller-side failure; surface it, don't silently retry.

## Tool catalog (21)

**Users (1):** `get_user`
**Storage pools (3):** `create_storage_pool`, `list_storage_pools`, `get_storage_pool`
**Clusters / databases (4):** `create_cluster`*, `list_clusters`, `get_cluster`, `delete_cluster`*
**Branches (5):** `create_branch`*, `list_branches`, `get_branch`, `delete_branch`*, `update_branch_settings`
**Endpoints / compute (6):** `create_endpoint`*, `list_endpoints`, `get_endpoint`, `delete_endpoint`*, `resize_endpoint`*, `update_endpoint_settings`
**Tasks (2):** `list_tasks`, `get_task`

`*` = asynchronous → returns a Task, poll `get_task`.

Notes: `create_cluster` takes an optional `pg_version` that **defaults to 18,
and only 18 is accepted** — omit it.

**`create_branch` fork precondition.** It forks the cluster's default branch
(`main`) unless you pass `parent_branch_id`. When you **omit** `fork_point_lsn`,
the controller derives the fork point from the parent's live WAL: it must find a
**running `read_write` endpoint on the parent** and reach that endpoint's
**safekeeper** to read its `flush_lsn`. If the parent has **no writer** (or its
safekeeper is unreachable), `create_branch` **fails synchronously with
`invalid_request`** — it does **not** silently fall back to a "latest" LSN. To
fork a parent with no running writer, either **provision a `read_write` endpoint
on it first**, or **pass an explicit `fork_point_lsn`** (storage `"X/X"` form),
which skips the live-WAL lookup entirely.

## End-to-end workflows

**Spin up a database with connectable compute:**
```
1. create_storage_pool(name, owner_user_id)            → pool (sync)
2. create_cluster(pool_id, name)                        → Task → poll → get_cluster
3. create_endpoint(pool_id, cluster_id, branch_id=<default_branch>)
                                                        → Task → poll → get_endpoint
4. read endpoint.address  → connect: postgresql://cloud_admin@<address>/postgres
```

**Fork a branch for an experiment, give it its own writer:**
```
1. create_branch(pool_id, cluster_id, name="exp", parent_branch_id=<main>)
                                                        → Task → poll → get_branch
2. create_endpoint(pool_id, cluster_id, branch_id=<exp>, role="read_write")
                                                        → Task → poll → get_endpoint
```

**Resize, then tear down:**
```
resize_endpoint(..., cpu="4", memory="8Gi")  → Task → poll → check in_place_resize → get_endpoint
delete_endpoint(...)                          → Task → poll → succeeded
delete_branch(...)                            → Task → poll  (conflict if it has children)
```

## Gotchas checklist

- **Never skip the poll.** A `pending`/`running` task is not done; the resource isn't ready.
- **Read back after success** to get `address` / final state — the Task never carries it.
- **One `read_write` per branch** — provision extra compute as `read_only`.
- **`resize` needs at least one of cpu/memory**, and check `in_place_resize` in the task result.
- **Omit `pg_version`** (only 18 is valid) and leave `disk_type` at its `network` default.
- **Pass `owner_user_id`** on pool creation; there is no default user.
- **Address by the full id chain** — a `not_found` usually means a wrong id upstream.
