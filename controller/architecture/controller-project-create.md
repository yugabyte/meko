# Amp Controller — Creating a project

**Status:** Draft
**Last updated:** 2026-06-18

How the Amp Controller ("cAmp") creates a new project (and its default branch). It
follows the `yb_light_local` workflow (`control_plane/src/bin/yb_light_local.rs`),
expressed in cAmp's resource model. **Precondition:** the storage pool already exists.

## Resource model

`app_user` → **storage pool** → **project** → **branch** (`pg_cluster`) →
**endpoint**.

- A **project** is the user-facing "database" — a purely control-plane grouping
  that maps to **no** storage object. A **branch** (`pg_cluster`) is a line of
  history within the project (the default branch, plus its forks); a branch's
  `branch_id` **is** the storage layer's `BranchId` verbatim — the flat storage
  identity, with no enclosing tenant.
- An **endpoint** is a single-container pod (or Docker container) — compute plus
  its *own* pageserver and safekeeper — deployed by cAmp's provisioner
  (`internal/deploy`).
- The **storage controller** — which cAmp runs as a managed subprocess; client in
  `internal/storagecontroller` — is the branch/node registry and the
  generation-fencing authority. cAmp drives it over the flat
  `/v1/branch/{branch_id}` surface.

## Principle: declare at create, materialize at the first endpoint

cAmp records a new branch as a **recipe** and builds its storage layer lazily,
when the first endpoint for that branch is deployed. The endpoint's own
pageserver bootstraps the branch (`initdb`); the storage controller **records**
the result — it does not run `initdb` itself.

## Step 1 — `create-project`

`POST /storage-pools/{p}/projects`. cAmp:

1. Allocates the project id (a control-plane-only id) and the default branch id
   (= the storage layer's `branch_id`).
2. Writes the control-plane rows to its database: `project` (under the pool) and
   its default `branch` (`name = "main"`, `pg_version`, `is_default = true`,
   `parent_branch_id = NULL` for the default branch). The default branch is always
   named `main`, independent of the project name; forks off it choose their own
   names (unique within the project).
3. Records the same recipe in the storage controller, keyed by the storage-layer
   `branch_id` (`POST /control/v1/branch/{branch_id}/pending_init` with
   `{ branch_name, pg_version, ancestor_branch_id = NULL, start_lsn = 0 }`).
4. Returns the project, with its embedded `default_branch`.

Nothing is materialized, no `initdb` runs, and no compute is started. The recipe
now lives in **both** cAmp's database (`branch`) and the storage controller (its
`pending_timeline_inits` table, listed flat via
`GET /control/v1/pending_timeline_inits`).

## Step 2 — Materialize at the first endpoint

`POST …/projects/{proj}/branches/{br}/endpoints`. cAmp deploys the endpoint, the
endpoint materializes the branch, and cAmp finalizes.

**cAmp deploys:**
- Records the `endpoint` row.
- Deploys the endpoint (Helm pod or Docker container) with the endpoint image's
  env-var contract — the flat `BRANCH_ID` (#168) — plus `pg_version`,
  `STORAGE_CONTROLLER_URL` (the cAmp-managed storage controller), and
  `BOOTSTRAP=true`.

**The endpoint materializes** (its supervisor, during bring-up):
1. Registers its pageserver node with the storage controller and runs the
   attach-hook; the storage controller attaches the branch to this pageserver
   and grants its **generation**. (The safekeeper is not registered with the
   controller — the endpoint owns it; see #171.)
2. Runs **`initdb`** (`BOOTSTRAP=true`) — the pageserver bootstraps the branch.
3. Once the branch is `Active`, reads its `start_lsn`, creates the branch on
   its safekeeper (single-member membership, generation 1), and **registers it
   with the storage controller** (`POST /control/v1/branch/{branch_id}/register`:
   `start_lsn`, optional `branch_name` — the controller just records the branch
   row).
4. Starts compute (Postgres).

**cAmp finalizes**, once the endpoint is ready:
- Reads the endpoint's address from the provisioner, records it on the
  `endpoint` row, and returns it to the caller.
- Clears the recipe: deletes the storage controller's pending init
  (`DELETE /control/v1/branch/{branch_id}/pending_init`) and marks the branch
  materialized.

## What actually creates the branch's storage

The **endpoint's pageserver** runs `initdb` (driven by `BOOTSTRAP=true`) — the
same role `yb_light_local`'s pageserver `Bootstrap` identity plays. The storage
controller is **not** asked to create the branch (there is no branch-create
call); it is *told* the branch exists, through the branch `register` route. A
forked branch (`parent_branch_id` set, so the recipe's
`ancestor_branch_id` is set) is identical, except the pageserver clones from the
ancestor at `fork_lsn` instead of running `initdb`.

## Forking a branch

`POST …/projects/{proj}/branches` (#130) is the same recipe-then-materialize
flow, forking an existing branch instead of bootstrapping a fresh one:

1. cAmp resolves the **parent** branch within the project — the project's default
   branch, or any branch named with `parent_branch_id` (branch-of-branch) — then
   writes a `branch` row under the **same project** with a new branch id (=
   storage-layer `branch_id`), `is_default = false`, `parent_branch_id = parent`,
   the inherited `pg_version`, and `fork_lsn` (`core.Backend.CreateBranch`).
2. The **fork point** defaults to the parent's durable WAL end — the
   `flush_lsn` of the safekeeper co-located with the parent's `read_write`
   endpoint, read directly from that node (`node.Client.TimelineFlushLSN` via
   `deploy.Deployer.EndpointSafekeeperURL`) — or an explicit `fork_point_lsn`.
   Reading the ancestor's **own** node is routing-correct in the co-located
   topology, where each endpoint has its own pageserver + safekeeper but the
   storage controller tracks one attachment per branch; its per-branch
   `describe` routes to whichever pageserver attached last and misses the parent
   branch once the branch has more than one endpoint (#133). `flush_lsn` also
   never lags durable WAL the way a pageserver's `last_record_lsn` can (#130).
   The parent must therefore have a `read_write` endpoint; without one (or
   without a deployer configured) a default fork point can't be resolved and
   `fork_point_lsn` is required.
3. cAmp records the pending-init recipe with `ancestor_branch_id = parent` and
   `start_lsn = fork_lsn` (vs. `null` / `0` for the default branch).

Materialization is unchanged (Step 2): the first endpoint deployed on the branch
brings up its pageserver/safekeeper and registers the branch — except the
pageserver clones from the ancestor at `fork_lsn` rather than running `initdb`.
Pre-fork data stays under the ancestor's `branch_id` and is served via
lineage-aware reads; nothing is copied. As with create, the branch's recipe
writes are synchronous today and move onto the task framework under #113.

## `yb_light_local` → Amp Controller

| `yb_light_local` | Amp Controller (cAmp) |
|---|---|
| `record_pending_timeline_init` (per branch) | `create-project` → `project` + default `branch` (cAmp DB) **and** the storage controller's pending init, keyed by `branch_id` (`POST /control/v1/branch/{branch_id}/pending_init`). There is no separate tenant-create step anymore — a root branch is just a pending `Bootstrap` init. |
| control plane starts SK | the **endpoint** starts its safekeeper (not registered with the controller — see #171) |
| control plane inits PS + `register_and_attach` | the **endpoint** registers its pageserver + runs the attach-hook (generation) |
| PS `Bootstrap` runs `initdb` | the **endpoint** runs `initdb` (`BOOTSTRAP=true`) |
| SK branch create + SC `timeline_register` | the **endpoint** creates the branch on its safekeeper, then calls the branch `register` route (`POST /control/v1/branch/{branch_id}/register`) |
| `endpoint.start` after provisioning | the endpoint starts compute once the branch is `Active` |
| `delete_pending_timeline_init` | **cAmp** deletes the pending init and marks the branch materialized |

## Notes

- **Who owns what.** The endpoint owns its pageserver and safekeeper, so it
  performs registration, attach, `initdb`, the safekeeper branch create, and the
  branch `register` call. cAmp records the recipe, deploys the endpoint, records the
  address, and clears the recipe — it never reaches into the endpoint's
  pageserver/safekeeper directly.
- **Generation** is granted by the storage controller's attach-hook when the
  endpoint attaches; it is never pinned.
- **Storage-controller calls used:** record / delete pending init, keyed by
  `branch_id` (`POST`/`DELETE /control/v1/branch/{branch_id}/pending_init`, by
  cAmp); pageserver-node register, attach-hook, and the
  branch `register` route (`POST /control/v1/branch/{branch_id}/register`, by the
  endpoint). (No safekeeper register — the safekeeper is endpoint-owned; see #171.)
- **Direction (planned).** The SC is being integrated into amp-controller and the
  create flow is moving to *create-then-configure*: endpoints created unassigned
  and bound to a branch on demand (a pre-warm pool, for faster turnaround). amp
  mints `branch_id` + `generation` (the data plane only enforces the generation)
  and pushes a `configure` to the pod; node-register + attach-hook move into amp's
  deploy code; the pod's branch `register` folds into the configure response
  (`start_lsn` is the pod's lone output). See
  [`sc-integration-migration.md`](sc-integration-migration.md).
- **Durability.** These steps run synchronously today; #113 tracks moving them
  onto cAmp's write-ahead task framework as durable, resumable steps.
