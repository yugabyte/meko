# Amp Controller — Creating a timeline

**Status:** Draft
**Last updated:** 2026-06-18

How the Amp Controller ("cAmp") creates a new timeline. It follows the
`yb_light_local` workflow (`control_plane/src/bin/yb_light_local.rs`), expressed
in cAmp's resource model. **Precondition:** the storage pool already exists.

## Resource model

`app_user` → **storage pool** → **tenant** → **timeline** (`pg_cluster`) →
**endpoint**.

- A **tenant** is the storage-layer unit; a **timeline** (`pg_cluster`) is a line
  of history within it (`main`, plus branches).
- An **endpoint** is a single-container pod (or Docker container) — compute plus
  its *own* pageserver and safekeeper — deployed by cAmp's provisioner
  (`internal/deploy`).
- The **storage controller** — which cAmp runs as a managed subprocess; client in
  `internal/storagecontroller` — is the tenant/node registry and the
  generation-fencing authority.

## Principle: declare at create, materialize at the first endpoint

cAmp records a new timeline as a **recipe** and builds its storage layer lazily,
when the first endpoint for that timeline is deployed. The endpoint's own
pageserver bootstraps the timeline (`initdb`); the storage controller **records**
the result — it does not run `initdb` itself.

## Step 1 — `create-timeline`

`POST /users/{u}/storage-pools/{p}/timelines`. cAmp:

1. Allocates `tenant_id` and `timeline_id`.
2. Writes the control-plane rows to its database: `tenant` (under the pool) and
   `pg_cluster` (`name`, `pg_version`, `is_main = true`,
   `parent_timeline_id = NULL` for a root timeline).
3. Records the same recipe in the storage controller:
   `record_pending_timeline_init(tenant_id, timeline_id, { branch_name,
   pg_version, ancestor_timeline_id = NULL, start_lsn = 0 })`.
4. Returns the timeline.

No tenant is attached, no `initdb` runs, and no compute is started. The recipe
now lives in **both** cAmp's database (`pg_cluster`) and the storage controller
(`pending_timeline_inits`).

## Step 2 — Materialize at the first endpoint

`POST …/timelines/{t}/endpoints`. cAmp deploys the endpoint, the endpoint
materializes the timeline, and cAmp finalizes.

**cAmp deploys:**
- Records the `endpoint` row.
- Deploys the endpoint (Helm pod or Docker container) with `tenant_id`,
  `timeline_id`, `pg_version`, `STORAGE_CONTROLLER_URL` (the cAmp-managed storage
  controller), and `BOOTSTRAP=true`.

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
  and marks the timeline materialized.

## What actually creates the timeline

The **endpoint's pageserver** runs `initdb` (driven by `BOOTSTRAP=true`) — the
same role `yb_light_local`'s pageserver `Bootstrap` identity plays. The storage
controller is **not** asked to create the timeline (there is no
`tenant_timeline_create` call); it is *told* the timeline exists, through
`timeline_register`. A **branch** timeline (`parent_timeline_id` set) is
identical, except the pageserver clones from the ancestor at `fork_lsn` instead
of running `initdb`.

## Branching a timeline

`POST …/timelines/{parent}/branch` (#130) is the same recipe-then-materialize
flow, forking an existing timeline instead of bootstrapping a fresh one:

1. cAmp resolves the **parent** (the timeline in the path) within the pool, then
   writes a `pg_cluster` row under the **same tenant** with a new `timeline_id`,
   `is_main = false`, `parent_timeline_id = parent`, the inherited `pg_version`,
   and `fork_lsn` (`core.Backend.BranchTimeline`).
2. The **fork point** defaults to the parent's latest LSN — read from the storage
   controller (`GetTimeline`'s `last_record_lsn`) — or an explicit
   `fork_point_lsn`. (`last_record_lsn` is the pageserver's ingested position,
   which can lag the safekeeper's durable `flush_lsn` that `yb_light_local`
   branches at; the controller doesn't expose `flush_lsn`, so an exact point
   needs `fork_point_lsn`.)
3. cAmp records the pending-init recipe with `ancestor_timeline_id = parent` and
   `start_lsn = fork_lsn` (vs. `null` / `0` for a root timeline).

Materialization is unchanged (Step 2): the first endpoint deployed on the branch
brings up its pageserver/safekeeper and registers the timeline — except the
pageserver clones from the ancestor at `fork_lsn` rather than running `initdb`.
Pre-fork data stays under the ancestor's `timeline_id` and is served via
lineage-aware reads; nothing is copied. As with create, the branch's recipe
writes are synchronous today and move onto the task framework under #113.

## `yb_light_local` → Amp Controller

| `yb_light_local` | Amp Controller (cAmp) |
|---|---|
| `tenant create` → `pending_timeline_inits` | `create-timeline` → `tenant` + `pg_cluster` (cAmp DB) **and** `record_pending_timeline_init` (storage controller) |
| control plane starts SK + `register_safekeeper` | the **endpoint** registers its safekeeper |
| control plane inits PS + `register_and_attach` | the **endpoint** registers its pageserver + runs the attach-hook (generation) |
| PS `Bootstrap` runs `initdb` | the **endpoint** runs `initdb` (`BOOTSTRAP=true`) |
| SK `create_timeline` + SC `timeline_register` | the **endpoint** creates the timeline on its safekeeper, then calls `timeline_register` |
| `endpoint.start` after provisioning | the endpoint starts compute once the timeline is `Active` |
| `delete_pending_timeline_init` | **cAmp** deletes the pending init and marks the timeline materialized |

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
