# Amp Controller — Creating a cluster

**Status:** Draft
**Last updated:** 2026-06-18

How the Amp Controller ("cAmp") creates a new cluster (and its default branch). It
follows the `yb_amp_local` workflow (`controller/local/cmd/yb_amp_local/main.go`),
expressed in cAmp's resource model. **Precondition:** the storage pool already exists.

## Resource model

`app_user` → **storage pool** → **cluster** → **branch** (`pg_cluster`) →
**endpoint**.

- A **cluster** is the user-facing "database" — a purely control-plane grouping
  that maps to **no** storage object. A **branch** (`pg_cluster`) is a line of
  history within the cluster (the default branch, plus its forks); a branch's
  `branch_id` **is** the storage layer's `BranchId` verbatim — the flat storage
  identity, with no enclosing tenant.
- An **endpoint** is a single-container pod (or Docker container) — compute plus
  its *own* pageserver and safekeeper — deployed by cAmp's provisioner
  (`internal/deploy`).
- The **controller** — which cAmp runs as a managed subprocess; client in
  `internal/registry` — is the branch registry. cAmp drives it over the flat
  `/v1/branch/{branch_id}` surface. It is not the fencing authority: write
  fencing is a lease the writing endpoint takes for itself
  (compute/architecture/compute-fencing.md).

## Principle: declare at create, materialize at the first endpoint

cAmp records a new branch as a **recipe** and builds its storage layer lazily,
when the first endpoint for that branch is deployed. The endpoint's own
pageserver bootstraps the branch (`initdb`); the controller **records**
the result — it does not run `initdb` itself.

## Step 1 — `create-cluster`

`POST /storage-pools/{p}/clusters`. cAmp:

1. Allocates the cluster id (a control-plane-only id) and the default branch id
   (= the storage layer's `branch_id`).
2. Writes the control-plane rows to its database: `cluster` (under the pool) and
   its default `branch` (`name = "main"`, `pg_version`, `is_default = true`,
   `parent_branch_id = NULL` for the default branch). The default branch is always
   named `main`, independent of the cluster name; forks off it choose their own
   names (unique within the cluster).
3. Seeds the branch's `amp_storage.branch_state` row — the per-branch state a
   pageserver reads at attach to decide bootstrap vs fork — through the
   in-process store, via the `record_branch_recipe` step.
4. Returns the cluster, with its embedded `default_branch`.

Nothing is materialized, no `initdb` runs, and no compute is started. There is one
branch record, not two: the `branch` row written in step 2 is the one every
reader sees, including `GET /api/v1/.../branches`. The only other row is the
data-plane `amp_storage.branch_state` seed from step 3.

## Step 2 — Materialize at the first endpoint

`POST …/clusters/{proj}/branches/{br}/endpoints`. cAmp deploys the endpoint, the
endpoint materializes the branch, and cAmp finalizes.

**cAmp deploys:**
- Records the `endpoint` row.
- Deploys the endpoint (Helm pod or Docker container) with the endpoint image's
  env-var contract — `BRANCH_ID` (#168) — plus `pg_version`,
  `CONTROLLER_URL` (the cAmp-managed controller), and
  `BOOTSTRAP=true`.

**The endpoint materializes** (its supervisor, during bring-up):
1. Takes the branch's **write lease** and starts its pageserver.
   (Neither the pageserver node nor the safekeeper is registered with the
   controller — the endpoint owns them.)
2. Runs **`initdb`** — the pageserver bootstraps the branch, taking its init
   mode from the branch's seeded `amp_storage.branch_state` row.
3. Once the branch is `Active`, reads its `start_lsn` and creates the branch on
   its safekeeper (single-member membership, generation 1). Nothing is reported
   back to the controller: the branch's row was written at branch create and
   needs no update.
4. Starts compute (Postgres).

**cAmp finalizes**, once the endpoint is ready:
- Reads the endpoint's address from the provisioner, records it on the
  `endpoint` row, and returns it to the caller.
- Marks the branch materialized. The controller's `branches` row is not
  consumed: it is the branch's durable record for as long as the branch exists
  (and, once dropped, is tombstoned via `dropped_at` rather than deleted), and
  `amp_storage.branch_state.consistent_lsn` is what says the pageserver has
  materialized it.

## What actually creates the branch's storage

The **endpoint's pageserver** runs `initdb` (driven by `BOOTSTRAP=true`) — the
same role `yb_amp_local`'s pageserver `Bootstrap` identity plays. The storage
controller is **not** asked to create the branch (there is no branch-create
call); it is *told* the branch exists, through the branch `register` route. A
forked branch (`parent_branch_id` set, so the recipe's
`ancestor_branch_id` is set) is identical, except the pageserver clones from the
ancestor at `fork_lsn` instead of running `initdb`.

## Forking a branch

`POST …/clusters/{proj}/branches` (#130) is the same recipe-then-materialize
flow, forking an existing branch instead of bootstrapping a fresh one:

1. cAmp resolves the **parent** branch within the cluster — the cluster's default
   branch, or any branch named with `parent_branch_id` (branch-of-branch) — then
   writes a `branch` row under the **same cluster** with a new branch id (=
   storage-layer `branch_id`), `is_default = false`, `parent_branch_id = parent`,
   the inherited `pg_version`, and `fork_lsn` (`core.Backend.CreateBranch`).
   The parent must be **live**: `RecordBranch` enforces that with a conditional
   write on the parent's row rather than a read of it, so the check serializes
   against a concurrent drop of that parent instead of write-skewing past it
   (see [`pageserver-gc.md`](../../compute/architecture/pageserver-gc.md), the
   cutoff-publish step).
2. The **fork point** defaults to the parent's durable WAL end — the
   `flush_lsn` of the safekeeper co-located with the parent's `read_write`
   endpoint, read directly from that node (`node.Client.TimelineFlushLSN` via
   `deploy.Deployer.EndpointSafekeeperURL`) — or an explicit `fork_point_lsn`.
   Reading the ancestor's **own** node is routing-correct in the co-located
   topology, where each endpoint has its own pageserver + safekeeper but the
   controller tracks one attachment per branch; its per-branch
   `describe` routes to whichever pageserver attached last and misses the parent
   branch once the branch has more than one endpoint (#133). `flush_lsn` also
   never lags durable WAL the way a pageserver's `last_record_lsn` can (#130).
   The parent must therefore have a `read_write` endpoint; without one (or
   without a deployer configured) a default fork point can't be resolved and
   `fork_point_lsn` is required. A parent whose safekeeper is unreachable is
   `503` (retryable), not `400` (#724).
3. cAmp records the pending-init recipe with `ancestor_branch_id = parent` and
   `start_lsn = fork_lsn` (vs. `null` / `0` for the default branch).

Materialization is unchanged (Step 2): the first endpoint deployed on the branch
brings up its pageserver/safekeeper and registers the branch — except the
pageserver clones from the ancestor at `fork_lsn` rather than running `initdb`.
Pre-fork data stays under the ancestor's `branch_id` and is served via
lineage-aware reads; nothing is copied. As with create, the branch's recipe
writes are synchronous today and move onto the task framework under #113.

### The fork floor

A fork starts from its parent's catalog as of `fork_lsn`, `cloud_admin`'s
password included, so its writer is created without one and answers to the
parent's ([`ac-k8s-deploy.md`](ac-k8s-deploy.md)). A fork taken before the
parent had a password would have none (yugabyte/meko#1461), so every branch has a **floor**,
the lowest LSN it can be forked at, held in its own `fork_lsn`:

- A fork's floor is the LSN it was taken at.
- A root's (`parent_branch_id` NULL) is NULL until its first `read_write`
  endpoint's supervisor reports applying the password; the `create_endpoint`
  step then records the LSN the supervisor read after that commit
  (`GET /v1/remote-auth`,
  [`single-container-endpoint.md`](../../compute/architecture/single-container-endpoint.md)),
  first writer wins ([`ac-tasks.md`](ac-tasks.md)). When the supervisor reports
  a password it cannot date (`already_set`, `applied_no_lsn`, or a `404` from an
  older image, `unsupported`), the step records `0/0`. A backfill at controller
  start gives `0/0` to a root whose first writer succeeded with no floor recorded
  (it predates this, or ran on an older controller), unless that step's result
  reported `unset`, `failed` or `unknown`: the catalog may then have no
  password, and a floor would stop the next writer setting one.

`createBranch` refuses a fork point below the parent's floor (`400`), and the
fork inserts (`RecordBranch`, `EnsureBranch`, `CreateBranch`) refuse it again as
`ErrForkBelowFloor`; a replayed branch id stays a no-op. `password_required`
reads the same NULL: only a root with no floor needs a `password` on its next
writer. Fork-vs-root is decided by `parent_branch_id` everywhere, never by
`fork_lsn`.

A root with no floor cannot be forked at all: `createBranch` answers `503` +
`Retry-After`. That covers a root that never had a writer, one whose first
writer is still being created (its safekeeper answers before the password
commits, so a default fork point could land below it), and one whose writer's
password was not seen to land. It is a `503` because the floor is normally
moments away, and the YCP agent retries a `503` but holds a `400` as permanent —
it can create a fork before the root's writer. First the handler runs the
backfill for that one root, so a writer created by a controller that records no
floors (an older replica mid-rollout) does not block forks until the next start.
A writer whose password was not seen to land (its create's `compute_password` is
`unset`, `failed` or `unknown`) blocks forks until it is replaced: the new
writer's supervisor applies the password or finds it set, and either records a
floor. Not on the local driver, which applies no password.

**Gaps.** `0/0` allows every fork point, so a root whose password was set at an
unknown LSN can be forked below it. A fork whose catalog has no
password cannot get one through the API, because a fork's writer drops the
`password` it is sent; set it from inside the endpoint pod over loopback
(`ALTER ROLE cloud_admin PASSWORD ...`).

## `yb_amp_local` → Amp Controller

| `yb_amp_local` | Amp Controller (cAmp) |
|---|---|
| `record_branch` (per branch) | `create-cluster` → `cluster` + default `branch` **and** that branch's `amp_storage.branch_state` seed, keyed by `branch_id`, written by the `record_branch_recipe` step. A root branch is a branch row with a NULL parent. |
| control plane starts SK | the **endpoint** starts its safekeeper (not registered with the controller — see #171) |
| control plane inits PS + `register_and_attach` | the **endpoint** starts its pageserver, under the write lease its supervisor holds |
| PS `Bootstrap` runs `initdb` | the **endpoint** runs `initdb`, driven by the seeded `branch_state` row |
| SK branch create + SC `timeline_register` | the **endpoint** creates the branch on its safekeeper; there is no register call back to the controller |
| `endpoint.start` after provisioning | the endpoint starts compute once the branch is `Active` |
| `delete_pending_timeline_init` | **cAmp** marks the branch materialized; the controller's branch row is not consumed |

## Notes

- **Who owns what.** The endpoint owns its pageserver and safekeeper, so it
  performs registration, attach, `initdb`, the safekeeper branch create, and the
  branch `register` call. cAmp records the recipe, deploys the endpoint, records the
  address, and clears the recipe — it never reaches into the endpoint's
  pageserver/safekeeper directly.
- **Fencing** is the endpoint's write lease, taken by its supervisor and
  enforced by the daemons themselves; no generation is pinned or issued by the
  controller.
- **Controller calls used:** record a branch, keyed by `branch_id`
  (the `record_branch_recipe` step, by cAmp).
- **Direction (planned).** The SC is being integrated into amp-controller and the
  create flow is moving to *create-then-configure*: endpoints created unassigned
  and bound to a branch on demand (a pre-warm pool, for faster turnaround). amp
  mints `branch_id` and pushes a `configure` to the pod; the pod's branch
  `register` folds into the configure response
  (`start_lsn` is the pod's lone output). See
  [`sc-integration.md`](sc-integration.md).
- **Durability.** These steps run synchronously today; #113 tracks moving them
  onto cAmp's write-ahead task framework as durable, resumable steps.
