---
name: amp-mcp-tools
description: Behavioral guide for AI agents using the amp controller MCP tools. Triggers when creating or managing Postgres storage pools, clusters (databases), branches (timelines), or endpoints (compute) through the amp controller, when polling amp tasks, or when running a Voyager database migration from an external Postgres source onto amp.
license: MIT
metadata:
  author: amp
  version: "1.1.0"
  tags: mcp, tools, amp, postgres, storage-pool, cluster, branch, endpoint, controller, migration, voyager
---

# amp MCP Tools — Agent Behavioral Guide

The amp controller manages a placement group of Postgres-compatible databases
on branchable storage. This server exposes its control-plane REST API as **30
MCP tools**. This skill teaches you how to call them correctly on the first try
— above all, the **asynchronous task model**, which is the single most common
way agents get amp wrong.

## When to use this skill

- You are calling tools on an **amp controller** MCP server.
- You are creating/listing/deleting storage pools, clusters, branches, or endpoints.
- You provisioned or resized compute and need to know when it is actually ready.
- You are **migrating an external Postgres database onto amp** with Voyager.
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

- **Storage pool** — top-level workspace. `create_storage_pool(name, owner_user_id)`;
  `rename_storage_pool` any time, `delete_storage_pool` once no live cluster is left.
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
lose an id. The synchronous exceptions are reads (`get_*`, `list_*`) and the
storage-pool writes (`create_storage_pool` returns the pool directly; `rename_storage_pool`
and `delete_storage_pool` likewise plan no task).

## Roles and the one-read-write rule

`create_endpoint(role=...)` takes `read_write` (default) or `read_only`. **A
branch may have only one `read_write` endpoint** — a second `read_write` on the
same branch is rejected with `conflict`. Provision additional compute as
`read_only`, or put the writer on its own branch. `disk_type` defaults to
`network` (`local` is not yet supported and is rejected with
`unsupported_disk_type`).

## The endpoint's password is required, and you never see it again

`create_endpoint` sets `password` on the compute's default Postgres user
(`cloud_admin`), which is who clients connect as. **It is required — the
controller generates none.** Ask the user for the password; do not invent one on
their behalf, and do not echo it back in your reply.

Nothing stores it, and no call can recover it — a user who loses it has to reset
the role themselves over SQL, or create a new cluster.

A `read_only` endpoint takes no password (passing one is a `400`): the user lives
in the branch's catalog, which all of its endpoints share, so a replica already
answers to whatever its branch's writer set — and setting a password there would
rotate the credential the writer's clients are using.

`password` is 8-128 characters with no control characters. A malformed one — or
a writer with no password at all — is a `400` before any endpoint exists.

The username is not selectable yet; that is yugabyte/meko#1354.

## Settings: dotted keys reach the nested ones

`create_endpoint` takes three settings maps. They can also be changed
afterwards — see "Changing settings after create" below, which is the part with
a cost: most Postgres settings reload without an interruption, but one fixed
at server start restarts the compute, and a storage daemon's settings always
restart that daemon.

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

They store the settings first, then tell the running endpoints. Each applies
what it can in place: `reloaded` lists what took the change while running,
`restarted` what had to be bounced, `restart_forced_by` names the Postgres
setting that forced a restart, `not_applied` lists settings that are stored and
not in force as `{name, reason}`, and `unreachable` says why an endpoint could
not be told (it picks the change up when it next starts).

**Most Postgres settings reload — a few restart the compute.** Postgres fixes
settings like `shared_buffers` at server start, and those bounce the compute and
drop its connections; on a *branch*, on every endpoint of it. Warn before doing
that to anything live, and report `restart_forced_by` so the user knows which
setting cost them. `pageserver_settings` / `safekeeper_settings` always restart
that daemon — they have no reload path.

Nothing is restarted for a setting in `not_applied`, and one Postgres refuses
is not even written to the config file — a file containing it does not load, so
the compute would not start. Report the `reason`: `unrecognized parameter` is a
typo, `invalid value` is a real setting given an unusable value. The value is
stored and doing nothing, and a silent success is how either survives.

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
`"16Gi"`) applied as request == limit (Guaranteed QoS). **memory needs an
explicit unit** of Mi or larger — Kubernetes reads a unitless `"4"` as four
*bytes*, so it is a 400 on both create and resize.

**On create**, CPU defaults to 2 cores and memory follows it at 8 GiB per core
(2 → 16Gi, 8 → 64Gi): name only `cpu` and the memory is right by construction.
**On resize**, neither defaults — an omitted dimension keeps its current value —
so raising `cpu` alone leaves memory where it was and is the ordinary way to land
under the floor. Either way, less memory per core than the ratio is a 400 naming
the minimum for the CPU asked for: a core without the memory to feed it gives
Postgres a cache it cannot keep. An endpoint already below the floor (one created
before the rule) may still resize, as long as the change does not lower its
memory per core further.

## Migrations — poll `get_migration`, not `get_task`

The migration tools move an **external Postgres database onto amp** with
Voyager. They form a linear workflow keyed by a single `migration_id`:

```
1. test_source_connectivity(host, port, username, password, database, schemas)   (optional pre-check, stateless)
2. create_storage_pool(...)  (or reuse an existing pool)                         → save pool.id as pool_id
3. create_migration(name, host, port, username, password, database, schemas)     → SYNC; save returned id as migration_id
4. start_migration_assessment(migration_id)                                       → Task (launch only)
       ↳ poll get_migration until state == "assessed"
5. get_migration_assessment(migration_id)                                         → sizing report → pick vcpu
6. provision_migration_target(migration_id, pool_id, vcpu=<from report>)          → Task (launch only)
       ↳ poll get_migration until state == "target_ready"
7. start_migration(migration_id)                                                  → Task (launch only)
       ↳ poll get_migration until state == "completed"
8. verify target data, get explicit user confirmation, then end_migration(migration_id)
       ↳ IRREVERSIBLE, no backups; poll get_migration until state == "ended"
```

**The critical difference from the resource tools:** the four launchers
(`start_migration_assessment`, `provision_migration_target`, `start_migration`,
`end_migration`)
are asynchronous and return a `Task`, but that Task **only covers the launch of
the Voyager job, not the migration itself**. Do **not** poll `get_task` to know
when a phase is done — poll **`get_migration(migration_id)`** and watch its
derived `state`:

```
created → assessing → assessed → provisioning_target → target_ready → migrating → completed → ending → ended   (or failed at any step)
```

### Important notes

- **`migration_id`** is the `id` returned by `create_migration` (also copied
  onto the response as `migration_id`). Save it and pass it to every later
  call: `start_migration_assessment`, `get_migration`,
  `get_migration_assessment`, `provision_migration_target`, `start_migration`,
  `end_migration`.
- **`pool_id`** is a **storage pool** id — the workspace that will hold the
  target cluster. Get it from `create_storage_pool` (the pool's `id`) or from
  `list_storage_pools` / `get_storage_pool`. It is the same `pool_id` used by
  `create_cluster` / `create_endpoint`. `provision_migration_target` needs it
  because that is when the target compute is created.
- **`get_migration`** is the "how is my migration going" surface. It returns
  the current derived `state`, a human `detail`, the source (password
  redacted), the attached target if any, the latest assess/migrate/end runs, and
  Voyager phase / per-table progress.
- **`list_migrations`** is lighter: a page of summaries (`id`, `name`,
  `state`, `detail`, timestamps) plus `total` (all matching rows) and `count`
  (items on this page). It does **not** refresh live job state the way
  `get_migration` does, so listed `state` can lag.
- **`create_migration` is synchronous** — it returns the migration directly
  (like `create_storage_pool`), not a Task. `schemas` needs at least one
  entry (e.g. `["public"]`); `db_type` is `postgresql` (the only source).
- **Source passwords are stored for the jobs but never returned** — pass
  them, but don't expect to read them back.
- **`end_migration` is the final, irreversible command.** It is the last step
  of the workflow: it runs Voyager's cleanup with backups disabled, deleting
  Voyager metadata and export-directory contents. Once an end run exists the
  migration accepts no further runs (assess, migrate, or end), it cannot be
  undone, and the migration cannot be resumed. Verify the target data and get
  explicit user confirmation before calling it.
- If a launcher returns `not_implemented` (501), the controller was started
  without the Voyager job runner / endpoint deployer.
- Migration 409s use specific codes, not generic `conflict`:
  `run_in_progress` (another run is already in flight),
  `target_not_attached` (`start_migration` before the target is ready),
  `target_conflict` (a target is already attached, or provision is already
  in flight), `migration_not_completed` (end requested too early), and
  `already_ended`.

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
- `run_in_progress` — a migration launcher was called while another run is already in flight.
- `target_not_attached` — `start_migration` before `provision_migration_target` has reached `target_ready`.
- `target_conflict` — a target is already attached to the migration, or a provision task is already in flight.
- `migration_not_completed` — `end_migration` before the migrate run has completed successfully.
- `already_ended` — `end_migration` was already completed; do not retry.
- `service_unavailable` — transient: the controller could not reach its state store, or a dependency the call needs is not up yet. Wait the `Retry-After` seconds (2) and retry the same call.
- `internal` — controller-side failure; surface it, don't silently retry.

## Tool catalog (33)

**Users (1):** `get_user`
**Storage pools (5):** `create_storage_pool`, `list_storage_pools`, `get_storage_pool`, `rename_storage_pool`, `delete_storage_pool`
**Clusters / databases (4):** `create_cluster`*, `list_clusters`, `get_cluster`, `delete_cluster`*
**Branches (6):** `create_branch`*, `list_branches`, `get_branch`, `add_branch_extensions`*, `delete_branch`*, `update_branch_settings`
**Endpoints / compute (6):** `create_endpoint`*, `list_endpoints`, `get_endpoint`, `delete_endpoint`*, `resize_endpoint`*, `update_endpoint_settings`
**Tasks (2):** `list_tasks`, `get_task`
**Migrations (9):** `test_source_connectivity`, `create_migration`, `list_migrations`, `get_migration`, `get_migration_assessment`, `start_migration_assessment`†, `provision_migration_target`†, `start_migration`†, `end_migration`†

`*` = asynchronous → returns a Task, poll `get_task`.
`†` = asynchronous launcher → returns a Task for the launch, but poll
**`get_migration`** (not `get_task`) for real progress.

Notes: `create_cluster` takes an optional `pg_version` that **defaults to 18,
and only 18 is accepted** — omit it.

**Tags.** `create_cluster` and `create_endpoint` take optional `tags`
(`{key: value}`), which become Kubernetes labels on every object of the
endpoint's release. An endpoint carries its cluster's tags plus its own, and its
own win on a shared key. They follow label rules, the chart's own labels
(`app.kubernetes.io/instance` and the like) and `amp.yugabyte.com/` keys are
refused, and they **cannot be changed after create**, so
settle them before you call.

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

**Migrate an external Postgres database onto amp:**
```
1. test_source_connectivity(host, port, username, password, database, schemas=["public"])
                                                        (optional pre-check, stateless)
2. create_storage_pool(...)  (or reuse an existing pool)  → save pool.id as pool_id
3. create_migration(name, host, port, username, password, database, schemas=["public"])
                                                        → SYNC; save returned id as migration_id
4. start_migration_assessment(migration_id)   → Task → poll get_migration until state=="assessed"
5. get_migration_assessment(migration_id)     → read recommended size → choose vcpu
6. provision_migration_target(migration_id, pool_id, vcpu) → Task → poll get_migration until state=="target_ready"
7. start_migration(migration_id)              → Task → poll get_migration until state=="completed"
8. verify target data; confirm with the user; end_migration(migration_id)
                                               → Task → poll get_migration until state=="ended"
```

## Gotchas checklist

- **Never skip the poll.** A `pending`/`running` task is not done; the resource isn't ready.
- **For migrations, poll `get_migration`, not `get_task`.** The launcher Task only covers the job launch; the migration's real progress is in its derived `state`.
- **Save both ids before the launchers.** `create_migration`'s `id` is `migration_id`. `pool_id` is a storage-pool id from `create_storage_pool` (or an existing pool), needed later by `provision_migration_target`. Optionally run `test_source_connectivity` first.
- **Read back after success** to get `address` / final state — the Task never carries it.
- **One `read_write` per branch** — provision extra compute as `read_only`.
- **`resize` needs at least one of cpu/memory**, and check `in_place_resize` in the task result.
- **Omit `pg_version`** (only 18 is valid) and leave `disk_type` at its `network` default.
- **Pass `owner_user_id`** on pool creation; there is no default user.
- **Address by the full id chain** — a `not_found` usually means a wrong id upstream.
