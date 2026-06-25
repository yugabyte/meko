# Amp Controller — Creating a project

**Status:** Draft
**Last updated:** 2026-06-18

How the Amp Controller ("cAmp") creates a new project (and its default branch). It
follows the `yb_light_local` workflow (`control_plane/src/bin/yb_light_local.rs`),
expressed in cAmp's resource model. **Precondition:** the storage pool already exists.

## Resource model

`app_user` → **storage pool** → **project** → **branch** (`pg_cluster`) →
**endpoint**.

- A **project** is the user-facing "database"; it *is* the storage layer's
  **tenant** — cAmp carries the storage id verbatim (`project.id` is the
  `tenant_id`) and maps project→tenant only at the storage boundary. A **branch**
  (`pg_cluster`) is a line of history within the project (the default branch, plus
  its forks); a branch *is* the storage layer's **timeline**
  (`branch.branch_id` is the `timeline_id`).
- An **endpoint** is a single-container pod (or Docker container) — compute plus
  its *own* pageserver and safekeeper — deployed by cAmp's provisioner
  (`internal/deploy`).
- The **storage controller** — which cAmp runs as a managed subprocess; client in
  `internal/storagecontroller` — is the tenant/node registry and the
  generation-fencing authority. Its API is unchanged by the rename: cAmp still
  drives it over the `/v1/tenant/{tenant}/timeline` surface.

## Principle: declare at create, materialize at the first endpoint

cAmp records a new branch as a **recipe** and builds its storage layer lazily,
when the first endpoint for that branch is deployed. The endpoint's own
pageserver bootstraps the timeline (`initdb`); the storage controller **records**
the result — it does not run `initdb` itself.

## Step 1 — `create-project`

`POST /storage-pools/{p}/projects`. cAmp:

1. Allocates the project id (= the storage layer's `tenant_id`) and the default
   branch id (= the `timeline_id`).
2. Writes the control-plane rows to its database: `project` (under the pool) and
   its default `branch` (`name = "main"`, `pg_version`, `is_default = true`,
   `parent_branch_id = NULL` for the default branch). The default branch is always
   named `main`, independent of the project name; forks off it choose their own
   names (unique within the project).
3. Records the same recipe in the storage controller, keyed by the storage-layer
   ids: `record_pending_timeline_init(tenant_id, timeline_id, { branch_name,
   pg_version, ancestor_timeline_id = NULL, start_lsn = 0 })`.
4. Returns the project, with its embedded `default_branch`.

No tenant is attached, no `initdb` runs, and no compute is started. The recipe
now lives in **both** cAmp's database (`branch`) and the storage controller
(`pending_timeline_inits`).

## Step 2 — Materialize at the first endpoint

`POST …/projects/{proj}/branches/{br}/endpoints`. cAmp deploys the endpoint, the
endpoint materializes the timeline, and cAmp finalizes.

**cAmp deploys:**
- Records the `endpoint` row.
- Deploys the endpoint (Helm pod or Docker container) with `tenant_id`,
  `timeline_id` (the storage-layer ids of the project/branch), `pg_version`,
  `STORAGE_CONTROLLER_URL` (the cAmp-managed storage controller), and
  `BOOTSTRAP=true`.

**The endpoint materializes** (its supervisor, during bring-up):
1. Registers its safekeeper and its pageserver node with the storage controller
   and runs the attach-hook; the storage controller attaches the tenant to this
   pageserver and grants its **generation**.
2. Runs **`initdb`** (`BOOTSTRAP=true`) — the pageserver bootstraps the timeline.
3. Once the timeline is `Active`, reads its `start_lsn`, creates the timeline on
   its safekeeper (single-member membership, generation 1), and **registers it
   with the storage controller** (`timeline_register`: `start_lsn` plus this
   endpoint's safekeeper as the `sk_set`).
4. Starts compute (Postgres).

**cAmp finalizes**, once the endpoint is ready:
- Reads the endpoint's address from the provisioner, records it on the
  `endpoint` row, and returns it to the caller.
- Clears the recipe: deletes the storage controller's `pending_timeline_init`
  and marks the branch materialized.

## What actually creates the timeline

The **endpoint's pageserver** runs `initdb` (driven by `BOOTSTRAP=true`) — the
same role `yb_light_local`'s pageserver `Bootstrap` identity plays. The storage
controller is **not** asked to create the timeline (there is no
`tenant_timeline_create` call); it is *told* the timeline exists, through
`timeline_register`. A forked branch (`parent_branch_id` set, so the recipe's
`ancestor_timeline_id` is set) is identical, except the pageserver clones from the
ancestor at `fork_lsn` instead of running `initdb`.

## Forking a branch

`POST …/projects/{proj}/branches` (#130) is the same recipe-then-materialize
flow, forking an existing branch instead of bootstrapping a fresh one:

1. cAmp resolves the **parent** branch within the project — the project's default
   branch, or any branch named with `parent_branch_id` (branch-of-branch) — then
   writes a `branch` row under the **same project** with a new branch id (=
   `timeline_id`), `is_default = false`, `parent_branch_id = parent`, the
   inherited `pg_version`, and `fork_lsn` (`core.Backend.CreateBranch`).
2. The **fork point** defaults to the parent's durable WAL end — the
   `flush_lsn` of the safekeeper co-located with the parent's `read_write`
   endpoint, read directly from that node (`node.Client.TimelineFlushLSN` via
   `deploy.Deployer.EndpointSafekeeperURL`) — or an explicit `fork_point_lsn`.
   Reading the ancestor's **own** node is routing-correct in the co-located
   topology, where each endpoint has its own pageserver + safekeeper but the
   storage controller tracks one attachment per tenant; its per-tenant
   `describe` routes to whichever pageserver attached last and misses the parent
   timeline once the tenant has more than one endpoint (#133). `flush_lsn` also
   never lags durable WAL the way a pageserver's `last_record_lsn` can (#130).
   The parent must therefore have a `read_write` endpoint; without one (or
   without a deployer configured) a default fork point can't be resolved and
   `fork_point_lsn` is required.
3. cAmp records the pending-init recipe with `ancestor_timeline_id = parent` and
   `start_lsn = fork_lsn` (vs. `null` / `0` for the default branch).

Materialization is unchanged (Step 2): the first endpoint deployed on the branch
brings up its pageserver/safekeeper and registers the timeline — except the
pageserver clones from the ancestor at `fork_lsn` rather than running `initdb`.
Pre-fork data stays under the ancestor's `timeline_id` and is served via
lineage-aware reads; nothing is copied. As with create, the branch's recipe
writes are synchronous today and move onto the task framework under #113.

## `yb_light_local` → Amp Controller

| `yb_light_local` | Amp Controller (cAmp) |
|---|---|
| `tenant create` → `pending_timeline_inits` | `create-project` → `project` + default `branch` (cAmp DB) **and** `record_pending_timeline_init` (storage controller) |
| control plane starts SK + `register_safekeeper` | the **endpoint** registers its safekeeper |
| control plane inits PS + `register_and_attach` | the **endpoint** registers its pageserver + runs the attach-hook (generation) |
| PS `Bootstrap` runs `initdb` | the **endpoint** runs `initdb` (`BOOTSTRAP=true`) |
| SK `create_timeline` + SC `timeline_register` | the **endpoint** creates the timeline on its safekeeper, then calls `timeline_register` |
| `endpoint.start` after provisioning | the endpoint starts compute once the timeline is `Active` |
| `delete_pending_timeline_init` | **cAmp** deletes the pending init and marks the branch materialized |

## Notes

- **Who owns what.** The endpoint owns its pageserver and safekeeper, so it
  performs registration, attach, `initdb`, the safekeeper timeline create, and
  `timeline_register`. cAmp records the recipe, deploys the endpoint, records the
  address, and clears the recipe — it never reaches into the endpoint's
  pageserver/safekeeper directly.
- **Generation** is granted by the storage controller's attach-hook when the
  endpoint attaches; it is never pinned.
- **Storage-controller calls used:** `record_pending_timeline_init` and
  `delete_pending_timeline_init` (cAmp); pageserver-node register, safekeeper
  register, attach-hook, and `timeline_register` (the endpoint).
- **Durability.** These steps run synchronously today; #113 tracks moving them
  onto cAmp's write-ahead task framework as durable, resumable steps.
