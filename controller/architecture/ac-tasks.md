# amp-controller — Task Framework & Step Catalog

**Status:** Draft
**Last updated:** 2026-06-23

This is the design doc the high-level design defers to for the **step catalog**:
[`amp-controller.md`](amp-controller.md) §8 fixes the *shape* of the task
framework (a user-visible `task` plus its ordered, idempotent `task_step` rows,
planned write-ahead and executed by a resume-safe loop under a lease), and §8.2 /
§12 leave "which steps exist and their exact arguments" to "the task framework's
own design doc" — this one. Read §8 first; this doc assumes it.

It covers the package layout, the execution model as implemented, the step
catalog (kinds, arguments, results, and each step's idempotency contract), the
task plans that compose those steps, and the dependency ports that keep it
testable. It does **not** cover wiring the API mutation handlers onto tasks —
the handlers still run synchronously and moving them is
[#113](https://github.com/yugabyte/yb-light/issues/113); nor HA-safe lease
renewal during a long step, which is [#90](https://github.com/yugabyte/yb-light/issues/90).

**Terminology.** The controller's user-facing ladder is **storage pool → project
→ branch → endpoint** ([#147](https://github.com/yugabyte/yb-light/issues/147)).
A project is the storage layer's *tenant* and a branch its *timeline*; the
controller carries the storage ids verbatim and maps `project_id → tenant_id`,
`branch_id → timeline_id` only at the storage seam — the deploy spec and the
storage-controller recipe, via `hex32`. So the steps speak project/branch, and
only the `deploy.EndpointSpec` and the storage-controller client keep
tenant/timeline.

## 1. Where it lives

All of it is the `controller/internal/task` package, plus the idempotent state
operations on `core.PgBackend`:

| File | What |
|---|---|
| `task.go` | The `Task` / `Step` / `StepPlan` / `StepInput` types and the `Handler` interface. |
| `store.go` | The `Store` interface and `PgStore` — persistence of tasks and steps (the write-ahead log). |
| `worker.go` | The `Worker`: claim a task under a lease, drive its steps with the resume-safe loop. |
| `registry.go` | The `Registry` (kind → `Handler`), `DefaultRegistry` (noop only), and `ProductionRegistry` (the full catalog). |
| `deps.go` | The dependency *ports* the step handlers use: `StateStore`, `RecipeStore`, and the `Deps` bundle. |
| `steps.go` | The **step catalog** — one `Handler` per re-usable step. |
| `plan.go` | The **task plans** — builders that assign ids write-ahead and produce a `Plan` of ordered steps. |
| `core/provision.go` | The idempotent, id-keyed `Ensure*` state operations the steps drive (`*PgBackend` satisfies `task.StateStore`). |

## 2. Execution model (as built)

A task is two kinds of row (schema in migration `0001_init.up.sql`; DDL sketch in
§8.1 of the high-level doc):

- **`task`** — one per operation, the user-visible unit (`GET /tasks/{id}`). Holds
  the kind, a coarse status, the target project/ref (for monitoring), the
  originating request (`params`), the owner + lease, and (on failure) the error.
- **`task_step`** — the ordered, idempotent steps, one row each. The `args` are
  written when the task is planned; `result` and `status` are updated as it runs.
  These rows **are** the write-ahead log.

**Planning is write-ahead.** A plan builder (`plan.go`) generates *all* the ids
the operation will need — project, branch, endpoint — up front and bakes them
into the step `args`, then `Store.PlanFrom` writes the `task` and all its `pending`
steps in one transaction *before* any step runs. The plan is durable intent;
execution catches up to it. Because every id is fixed at plan time, a step is a
pure function of its `args` — which is what makes replay converge.

**Execution is a resume-safe loop** (`Worker.execute`): load the task's steps in
order, skip any already `succeeded`, and for each remaining step mark it
`running`, look its `kind` up in the registry, run the handler, then record the
result and mark it `succeeded` — or, on error, mark the step and the task
`failed` and stop. A step left `running` (started, never completed — the owner
died mid-step) is *not* skipped: it is re-run, which is safe precisely because
every step is idempotent. A missing handler fails the task rather than silently
skipping work.

One distinction the loop draws: a step error returned because the **worker's
context is cancelled** (shutdown) is *not* treated as a terminal failure. The
loop returns without marking the task `failed`, leaving it `running` so the lease
expires and it is reclaimed and replayed (§8.2) — durably failing a task over a
graceful shutdown would strand it. Only a genuine step error on a live context
fails the task.

**Ownership and recovery** (§8.2). `Store.ClaimNext` atomically transitions one
runnable task `pending → running`, stamping `owner` and `lease_expiry`
(`FOR UPDATE SKIP LOCKED`, so two instances never claim the same task). A task is
*runnable* if it is `pending`, or `running` with an expired lease — the latter is
how a crashed owner's work is reclaimed. The new owner runs the same loop; the
already-`succeeded` steps are skipped, so work resumes at the first unfinished
step. Replaying the step that was in flight when the owner died is safe because
**every step is idempotent**.

> The lease does not yet auto-renew, so a step that runs longer than the lease TTL
> can be reclaimed and run concurrently by a second instance. Idempotency keeps
> that correct (it converges), but the redundant work is wasteful; renewal is
> [#90](https://github.com/yugabyte/yb-light/issues/90).

## 3. The step catalog

Steps are the re-usable, idempotent units tasks are built from. Each is a
`Handler` (a `Kind()` and a `Run(ctx, StepInput) (result, error)`); the handler
reads its typed args from `Step.Args`, performs its effect through a narrow port
(§5), and returns a small JSON result (informational — read back via the task
API, never load-bearing). **Idempotency is the contract**: re-running a step
after a crash or partial execution must converge, not double-apply or error.

| Step kind | Args | Effect | How it stays idempotent |
|---|---|---|---|
| `create_project` | pool, project, branch ids, name, pg_version | Create the project + its default branch. | `EnsureProject`: `INSERT … ON CONFLICT (pk) DO NOTHING` on both rows — same ids re-run is a no-op. A *name* collision (different ids) is a real error. |
| `create_branch` | parent, project, branch ids, name, fork LSN, pg_version | Fork a branch into a new branch (records ancestry). | `EnsureBranch`: same `ON CONFLICT (pk) DO NOTHING`; a missing parent or name collision is a real error. |
| `record_branch_recipe` | project, branch ids, branch name, pg_version, ancestor (opt), start LSN | Record the storage controller's pending-init recipe (materialized later by the first endpoint). | The storage controller upserts the recipe; with no storage controller the step no-ops with a warning (the recipe then lives only in the `branch` row). |
| `create_endpoint` | endpoint, project, branch ids, role, pg_version, ancestor (opt) | Ensure the endpoint record, deploy the compute, record the address, clear the branch recipe. **The shared step** create-project, branch, and scale-up all reuse (§8). | Record insert is id-keyed `ON CONFLICT (id) DO NOTHING` (so the single-writer index is not re-violated on replay); deploy is keyed by the endpoint's stable name (deployer idempotency); address is an absolute `UPDATE`; recipe clear tolerates an already-absent recipe. |
| `delete_endpoint` | endpoint id | Retire the running instance, then delete the record. | Both effects tolerate an already-absent target. Order matters: retire the instance *before* deleting the record, so a crash between them leaves a record whose instance can still be retired on replay (never an orphaned instance with no record). |

A few invariants worth calling out:

- **`create_endpoint` records the row before it deploys.** The record claims the
  single-writer slot (a second writer is rejected by the partial unique index
  before any compute is spent) and gives the deploy a stable id.
- **Fork vs. default branch.** When `create_endpoint`'s args carry an ancestor,
  the endpoint clones from that ancestor at the fork LSN (`Bootstrap` forced off);
  otherwise a default branch runs `initdb` when the deployment's bootstrap flag is
  set. This mirrors the synchronous create-endpoint handler exactly. The deploy
  spec carries the storage ids (`hex32(project_id)` as the tenant, `hex32(branch_id)`
  as the timeline).
- **Clearing the recipe is best-effort.** A lingering recipe is harmless (nothing
  reads it once the branch is materialized), so a clear failure logs a warning and
  the step still succeeds.

## 4. Task plans

Plans compose the catalog. Each builder takes a typed input, generates the ids,
and returns a `Plan { Kind, ProjectID, TargetRef, Params, Steps }`.

| Task kind | Builder | Steps |
|---|---|---|
| `create_project` | `BuildCreateProject` | `create_project` → `record_branch_recipe` → (`create_endpoint`, when a first writer is requested). |
| `branch` | `BuildBranch` | `create_branch` → `record_branch_recipe` (ancestor set) → (`create_endpoint` cloning from the ancestor, when requested). |
| `create_endpoint` (scale-up) | `BuildCreateEndpoint` | `create_endpoint`. |
| `delete_endpoint` (scale-down) | `BuildDeleteEndpoint` | `delete_endpoint`. |

`create_endpoint` appearing in three of the four plans is the point: it is one
step, reused, not three copies (§8). Branch fork-point resolution (the default
"latest LSN") is a *read* done before planning, so the fork LSN reaches the plan
already resolved — the plan stays pure.

## 5. Dependency ports

Step handlers depend on narrow interfaces, not concrete collaborators, so they
unit-test against in-memory fakes and the framework stays decoupled:

- **`StateStore`** — the idempotent, id-keyed project/branch operations
  (`EnsureProject`, `EnsureBranch`, `EnsureEndpointRecord`, `SetEndpointAddress`,
  `DeleteEndpoint`). `*core.PgBackend` satisfies it structurally; these are
  deliberately *not* on the `core.Backend` interface, so the synchronous API path
  keeps using the id-generating create methods.
- **`RecipeStore`** — record/clear the storage controller's pending-init recipe,
  keyed by the 32-hex storage ids the controller maps a project/branch to. A
  `BranchRecipe` decouples the task package from the storage-controller client
  type; `cmd/controller` bridges the two (`scRecipeStore`, mapping `BranchRecipe →
  PendingTimelineInit`). May be nil (no managed storage controller) — the handlers
  degrade with a warning.
- **`deploy.Deployer`** — the existing runtime-agnostic endpoint deployer, reused
  as-is (its `EndpointSpec` keeps the storage layer's tenant/timeline ids).

`ProductionRegistry(Deps{…})` wires the catalog to the real collaborators;
`cmd/controller` builds the `Deps` from the same `Provisioning` the synchronous
handlers use. The registry is live in `serve`, but **no API path plans these task
kinds yet** (mutations still run synchronously), so the worker simply has nothing
of these kinds to claim until #113.

## 6. Testing

The package is tested without a database (matching `core.MemBackend` and the API
tests' `fakeTaskStore`): in-memory fakes for every port and an in-memory
`Store`. The tests cover

- the **worker** loop against a faithful in-memory store — runs all steps to
  completion, **resumes** a reclaimed task skipping already-succeeded steps,
  **replays an in-flight `running` step**, **fails** the task and halts later
  steps on a step error, fails on an unknown step kind, ignores a live-owned
  (unexpired-lease) task, and leaves a cancelled task reclaimable;
- **every step handler** — its happy path, **idempotent replay** (run twice,
  converge), error propagation, the fork-vs-default-branch deploy spec, and the
  best-effort recipe clear;
- the **plan builders** — correct ordered step kinds, ids generated and threaded
  consistently across steps, and branch ancestor wiring.

What is **not** unit-tested here is `PgStore`'s SQL (the claim/lease query, the
transactional plan writes) and `PgBackend`'s `Ensure*` SQL — there is no
DB-backed harness in the repo today (the same gap as the rest of `PgBackend`).
That SQL is exercised by the e2e suite.
