# amp-controller — Task Framework & Step Catalog

**Status:** Draft
**Last updated:** 2026-06-29

This is the design doc the high-level design defers to for the **step catalog**:
[`amp-controller.md`](amp-controller.md) §8 fixes the *shape* of the task
framework (a user-visible `task` plus its ordered, idempotent `task_step` rows,
planned write-ahead and executed by a resume-safe loop under a lease), and §8.2 /
§12 leave "which steps exist and their exact arguments" to "the task framework's
own design doc" — this one. Read §8 first; this doc assumes it.

It covers the package layout, the execution model as implemented, the step
catalog (kinds, arguments, results, and each step's idempotency contract), the
task builders that compose those steps, and the dependency ports that keep it
testable. The API mutation handlers now **plan** these tasks: the six
world-changing operations (`createProject`, `createBranch`, `createEndpoint`,
`deleteProject`, `deleteBranch`, `deleteEndpoint`) validate cheaply, plan a task,
and return `202` + the `Task` + a `Location` header for the client to poll
([#113](https://github.com/yugabyte/yb-light/issues/113); the wire contract is in
[`ac-apis.md`](ac-apis.md)). What this doc does **not** cover is HA-safe lease
renewal during a long step, which is [#90](https://github.com/yugabyte/yb-light/issues/90).

**Terminology.** The controller's user-facing ladder is **storage pool → project
→ branch → endpoint** ([#147](https://github.com/yugabyte/yb-light/issues/147)).
A project is a purely control-plane grouping (it maps to no storage object); a
branch's `branch_id` **is** the storage layer's flat `BranchId`. The controller
carries that `branch_id` verbatim to the storage seam — the storage-controller
recipe and client, via `hex32` — and the storage controller is keyed by the flat
`branch_id`. `deploy.EndpointSpec` likewise renders the flat `branch_id` as the
endpoint image's `BRANCH_ID` env var (#168).

## 1. Where it lives

All of it is the `controller/internal/task` package, plus the idempotent state
operations on `core.PgBackend`:

| File | What |
|---|---|
| `task.go` | The `Task` (carries `StepIDs`, its steps in order) / `StepRecord` (a step row, keyed by its own id) types; the `StepSpec` / `TaskSpec` plan-time descriptions; the `Step` interface (`Kind()` + `Execute(ctx) StepResult`); and `StepResult` (`Ok` / `Fail`). |
| `tasks.go` | The `Service` — the dependency anything that initiates a task takes (it holds the `Store`, so callers don't thread it through). One `CreateX(ctx, in)` method per kind builds the step `StepSpec`s and hands them to `Store.Create`; `Get` reads a task's status back. |
| `store.go` | The `Store` interface and `PgStore` — `Create` write-ahead-records the task + its steps; the claim/step-transition methods drive the write-ahead log. The `Service` wraps it for callers; the worker uses it directly. |
| `mem.go` | `MemStore` — an in-memory `Store` (real type, not a test fake) mirroring `core.MemBackend`, with a `Seed` helper for preloading states `Create` can't produce. The task and API unit tests run against it; the real SQL is the integration suite's job. |
| `worker.go` | The `Worker`: a claim loop that runs up to `MaxConcurrent` tasks at once, each in its own goroutine, driving it through the resume-safe step loop (the registry hands it steps already wired with their deps). |
| `registry.go` | The `Registry` (kind → `Builder`, which decodes a persisted step's args and binds the registry's `Deps` into a typed `Step`), `DefaultRegistry` (noop only), and `ProductionRegistry(deps)` (the full catalog, deps-wired). |
| `deps.go` | The dependency *ports* the steps use: `StateStore`, `RecipeStore`, and the `Deps` bundle the registry binds into each step. |
| `steps.go` | The step-catalog anchor: the shared `mustResult` helper and the catalog overview. |
| `step_<kind>.go` | One file per step — its kind constant, typed args struct, the `Step` implementation (args + deps as fields), and the `Builder` that rebuilds it from persisted args + deps. |
| `core/provision.go` | The idempotent, id-keyed `Ensure*` state operations the steps drive (`*PgBackend` satisfies `task.StateStore`). |

## 2. Execution model (as built)

A task is two kinds of row (schema in migrations `0001_init.up.sql` +
`0002_task_step_ids.up.sql`, plus `0003_task_list_indexes.up.sql` for the task
list endpoint — §4.1; DDL sketch in §8.1 of the high-level doc):

- **`task`** — one per operation, the user-visible unit (`GET /tasks/{id}`). Holds
  the kind, a coarse status, the target project/ref (for monitoring), the
  originating request (`params`), `step_ids` (its steps in execution order), the
  owner + lease, and (on failure) the error.
- **`task_step`** — the idempotent steps, one row each, keyed by its own `id`
  (referenced from `task.step_ids`, which fixes their order); a `task_id` backref
  keeps "the steps of this task" a cheap lookup. The `args` are written when the
  task is created; `result` and `status` are updated as it runs. These rows **are**
  the write-ahead log.

**Creation is write-ahead.** A `Service.CreateX` method (`tasks.go`) generates
*all* the ids the operation will need — project, branch, endpoint — up front and
bakes them into the step args (described as `StepSpec`s — pure data, never an
executable `Step`), then `Store.Create` assigns the task id + a step id per step and writes
the `task` (with its ordered `step_ids`) and all its `pending` steps in one
transaction *before* any step runs, returning the `Task`. The record is durable
intent; execution catches up to it. Because every id is fixed up front, a step is
a pure function of its `args` — which is what makes replay converge.

**Claiming and execution are decoupled, and concurrent** (`Worker.Run`). One
claim loop drives the worker: it acquires one of `cfg.MaxConcurrent` execution
slots (default 50), claims the next runnable task, and hands it to its own
goroutine that runs the step loop below and frees the slot when it finishes — so
up to `MaxConcurrent` tasks run at once. Acquiring the slot *before* claiming is
deliberate: the worker never takes a lease on a task it can't immediately start
(no claimed-but-idle task whose lease could expire while it waits). When nothing
is claimable it idles one poll interval; a persistent claim error (e.g. the DB is
down) backs off exponentially instead. A panic in a step is recovered and fails
that step's task, rather than taking the worker (and its other in-flight tasks)
down. On shutdown it stops claiming and waits up to `ShutdownGrace` for in-flight
tasks to unwind, then returns regardless — a step that ignores cancellation can't
wedge process exit (the unfinished tasks are simply reclaimed, §8.2). Tasks run
independently with no ordering between them, so two tasks touching the same
resource can interleave — the DB constraints and step idempotency keep that
correct, but per-resource serialization is future work
([#176](https://github.com/yugabyte/yb-light/issues/176)).

**Execution is a resume-safe loop** (`Worker.execute`, one per task goroutine):
load the task's step records in execution order (`task.step_ids`), skip any already `succeeded`, and for each remaining step
*rebuild* it from its `kind`+`args` via the registry (which binds in its `Deps`),
mark it `running`, run it (`Execute`), then record the result and mark it
`succeeded` — or, on failure, mark the step and the task `failed` and stop. A step
left `running` (started, never completed — the owner died mid-step) is *not*
skipped: it is rebuilt and re-run, which is safe precisely because every step is
idempotent. A step whose `kind` is unregistered, or whose persisted `args` no
longer decode, fails the task rather than silently skipping work.

One distinction the loop draws: a step failure whose cause is the **worker's
context being cancelled** (shutdown) is *not* treated as a terminal failure. The
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

Steps are the re-usable, idempotent units tasks are built from. Each implements
the `Step` interface (a `Kind()` and an `Execute(ctx) StepResult`) in its own
`step_<kind>.go` file. A step carries both its typed args *and* the narrow
port(s) it needs as fields, both bound by its `New…Step(args, deps)` constructor —
at plan time, or rebuilt from the persisted `args` + the registry's deps on
replay. The deps fields are unexported, so `json.Marshal(step)` is *only* the
args — exactly the persisted `task_step.args`. `Execute` performs the step's
effect through those ports (§5) and returns a `StepResult`: either an `Ok`
carrying a small JSON output (informational — read back via the task API, never
load-bearing) or a `Fail` carrying the cause. **Idempotency is the contract**:
re-running a step after a crash or partial execution must converge, not
double-apply or error.

| Step kind | Args | Effect | How it stays idempotent |
|---|---|---|---|
| `create_project` | pool, project, branch ids, name, pg_version | Create the project + its default branch. | `EnsureProject`: `INSERT … ON CONFLICT (pk) DO NOTHING` on both rows — same ids re-run is a no-op. A *name* collision (different ids) is a real error. |
| `create_branch` | parent, project, branch ids, name, fork LSN, pg_version | Fork a branch into a new branch (records ancestry). | `EnsureBranch`: same `ON CONFLICT (pk) DO NOTHING`; a missing parent or name collision is a real error. |
| `record_branch_recipe` | project, branch ids, branch name, pg_version, ancestor (opt), start LSN | Record the storage controller's pending-init recipe (materialized later by the first endpoint). | The storage controller upserts the recipe; with no storage controller the step no-ops with a warning (the recipe then lives only in the `branch` row). |
| `create_endpoint` | endpoint, project, branch ids, role, pg_version, cpu, memory, ancestor (opt) | Ensure the endpoint record, deploy the compute (with its CPU/memory reservation), record the address, clear the branch recipe. **The shared step** create-project, branch, and scale-up all reuse (§8). | Record insert is id-keyed `ON CONFLICT (id) DO NOTHING` (so the single-writer index is not re-violated on replay); deploy is keyed by the endpoint's stable name (deployer idempotency); address is an absolute `UPDATE`; recipe clear tolerates an already-absent recipe. `cpu`/`memory` are resolved (defaulted to 2 cores / 4Gi) by the API handler and carried in the args, so a reclaim/replay redeploys the same size. |
| `delete_endpoint` | endpoint id | Retire the running instance, then delete the record. | Both effects tolerate an already-absent target. Order matters: retire the instance *before* deleting the record, so a crash between them leaves a record whose instance can still be retired on replay (never an orphaned instance with no record). |
| `delete_branch` | project, branch ids | Cascade-delete a branch: enumerate the branch's endpoints, retire each (instance + record), then delete the branch row. | Self-contained — it **enumerates children at Execute time** (`EndpointIDsForBranch`), so an endpoint added between planning and execution is still retired on replay. Cascade order is **endpoints → branch**: the FKs are `RESTRICT`, so children go first; every effect tolerates an already-absent target, so a re-run mid-cascade converges. A still-referencing child fork makes `DeleteBranch` a real error (correct). |
| `delete_project` | project id | Cascade-delete a project: enumerate **all** the project's endpoints, retire each (instance + record), then delete the project — which drops all its branches. | Self-contained, enumerating children at Execute time (`EndpointIDsForProject`). Cascade order is **endpoints → branches → project**; `DeleteProject` nulls the branches' in-project parent pointers before dropping them to sidestep the self-FK `RESTRICT` ordering. Idempotent throughout. |

`delete_branch` and `delete_project` are **single self-contained steps**, not a
planned per-endpoint fan-out, because the worker has no dynamic sub-step
expansion: the set of endpoints to retire isn't known until Execute time, so the
one step enumerates and retires them itself. Both require a deployer (they retire
running instances, mirroring `delete_endpoint`).

A few invariants worth calling out:

- **`create_endpoint` records the row before it deploys.** The record claims the
  single-writer slot (a second writer is rejected by the partial unique index
  before any compute is spent) and gives the deploy a stable id.
- **Fork vs. default branch.** When `create_endpoint`'s args carry an ancestor,
  the endpoint clones from that ancestor at the fork LSN (`Bootstrap` forced off);
  otherwise a default branch runs `initdb` when the deployment's bootstrap flag is
  set. The deploy
  spec carries the storage layer's flat branch id — `hex32(branch_id)` as the
  spec's `BranchID` (and `hex32(ancestor_branch_id)` for a fork); a project no
  longer maps to a storage tenant (#168).
- **Clearing the recipe is best-effort.** A lingering recipe is harmless (nothing
  reads it once the branch is materialized), so a clear failure logs a warning and
  the step still succeeds.
- **Direction (planned).** `create_endpoint` is the landing site for the SC
  integration: it will additionally mint the pageserver `generation` (attach-hook)
  and register the node itself — moving that work out of the pod — and evolve
  toward a create-then-configure model (create an unassigned pod, then push a
  `configure` binding it to a branch). See
  [`sc-integration-migration.md`](sc-integration-migration.md).

## 4. The task service

The `Service` (holding a `Store`) is the dependency callers take to initiate
tasks; its `CreateX` methods compose the catalog. Each takes a typed input,
generates the ids, describes the ordered steps as `StepSpec`s (kind + marshaled
args — no deps, no executable `Step`), and hands a `TaskSpec` to `Store.Create`,
which persists the task + steps and returns the `Task` to surface to the user.
`Service.Get` reads a task's status back. The worker rebuilds the executable steps
(with its deps) later.

| Task kind | Service method | Steps |
|---|---|---|
| `create_project` | `Service.CreateProject` | `create_project` → `record_branch_recipe` → (`create_endpoint`, when a first writer is requested). |
| `branch` | `Service.Branch` | `create_branch` → `record_branch_recipe` (ancestor set) → (`create_endpoint` cloning from the ancestor, when requested). |
| `create_endpoint` (scale-up) | `Service.CreateEndpoint` | `create_endpoint`. |
| `delete_endpoint` (scale-down) | `Service.DeleteEndpoint` | `delete_endpoint`. |
| `delete_branch` | `Service.DeleteBranch` | `delete_branch`. |
| `delete_project` | `Service.DeleteProject` | `delete_project`. |

The kind constants (`KindCreateProject`, `KindBranch`, `KindCreateEndpoint`,
`KindDeleteEndpoint`, `KindDeleteBranch`, `KindDeleteProject`) are the
`task.kind` values the API exposes and `listTasks` filters on.

`create_endpoint` appearing in three of the six tasks is the point: it is one
step, reused, not three copies (§8). Branch fork-point resolution (the default
"latest LSN") is a *read* done before the builder runs, so the fork LSN reaches it
already resolved — building a task performs no I/O of its own. The two cascade
deletes are each a single self-contained step (§3) — the children are enumerated
at Execute time, not planned up front.

### 4.1 Reading tasks back: `Get` and `List`

`Service.Get` reads one task back (`GET /tasks/{id}`); `Service.List(ctx, filter,
limit, offset)` returns a page of tasks newest-first (`created_at` DESC) plus the
total matching count for the **`listTasks`** endpoint (`GET /tasks`). The filter
(`TaskFilter`: `status`, `kind`, `project_id`, any of which may be zero-valued =
unfiltered) maps straight from the spec's query parameters. `List` is backed by
`Store.List`, whose scan is kept cheap by migration `0003_task_list_indexes`
(§2): `task_by_created_at` (the DESC order), `task_by_project`, and
`task_by_status` (the optional filters). These are distinct from the
`task_runnable` partial index (0001/0002), which serves the worker's claim path,
not the list path.

## 5. Dependency ports

Steps depend on narrow interfaces, not concrete collaborators, so they
unit-test against in-memory fakes and the framework stays decoupled. The ports
are bundled into `Deps`, which the registry binds into each step at construction
(so a built step keeps only the ports it uses, and `Execute` needs only a
context):

- **`StateStore`** — the idempotent, id-keyed project/branch operations. The
  create/scale path: `EnsureProject`, `EnsureBranch`, `EnsureEndpointRecord`,
  `SetEndpointAddress`, `DeleteEndpoint`. The cascade-delete path adds the
  enumeration + drop primitives the `delete_branch`/`delete_project` steps use:
  `EndpointIDsForBranch` and `EndpointIDsForProject` (the at-Execute-time child
  enumeration — an absent target yields an empty slice, not an error, so a re-run
  mid-cascade converges), `DeleteBranch` (drops one branch row; the parent FK is
  `RESTRICT`, so a still-referencing child fork is a real error), and
  `DeleteProject` (drops the project and all its branches, nulling the branches'
  in-project parent pointers first to sidestep the self-FK ordering). All are
  id-keyed and idempotent (an absent row is a no-op). `*core.PgBackend` satisfies
  the interface structurally; these are deliberately *not* on the `core.Backend`
  interface, so the synchronous API read/pre-check path keeps using the
  id-generating create methods.
- **`RecipeStore`** — record/clear the storage controller's pending-init recipe,
  keyed by the 32-hex flat `branch_id` the controller maps a branch to. A
  `BranchRecipe` decouples the task package from the storage-controller client
  type; `cmd/controller` bridges the two (`scRecipeStore`, mapping `BranchRecipe →
  PendingTimelineInit`). May be nil (no managed storage controller) — the steps
  degrade with a warning.
- **`deploy.Deployer`** — the existing runtime-agnostic endpoint deployer, reused
  as-is (its `EndpointSpec` renders the flat `branch_id` as the endpoint image's
  `BRANCH_ID` env var, #168).

`ProductionRegistry(deps)` registers the catalog's builders and holds the deps,
so each step it builds comes out fully wired; `cmd/controller` builds the `Deps`
from the same `Provisioning` the API handlers' cheap pre-checks use and passes
them to `ProductionRegistry`, then hands the registry to `NewWorker`. It also
wraps the store in a `Service` and hands that to the API. The mutation handlers
plan tasks through that `Service` (#113), so the worker live in `serve` now has
real work to claim: every `createProject`/`createBranch`/`createEndpoint`/
`deleteEndpoint`/`deleteBranch`/`deleteProject` request records a task it runs to
completion.

## 6. Testing

The package is tested without a database (matching `core.MemBackend`): in-memory
fakes for every port and the in-memory `MemStore` for the `Store` itself —
`MemStore` is a real, exported type (`mem.go`), so the API package's tests run
against it too rather than carrying their own task-store fake. The tests cover

- the **worker**, two ways: the per-task `execute` path against a faithful
  in-memory store — runs all steps to completion, **resumes** a reclaimed task
  skipping already-succeeded steps, **replays an in-flight `running` step**,
  **fails** the task and halts later steps on a step error, fails on an unknown
  step kind, and leaves a cancelled task reclaimable; and the `Run` **claim loop**
  — it drains a backlog concurrently and never exceeds `MaxConcurrent` tasks in
  flight (a gated step asserts the cap), stops cleanly on cancel, recovers a
  panicking step into a task failure, logs a shutdown interruption as such (not as
  an error), and returns within `ShutdownGrace` even if a step ignores cancel;
- **every step** — its happy path, **idempotent replay** (run twice, converge),
  error propagation, the fork-vs-default-branch deploy spec, and the best-effort
  recipe clear;
- the **registry** — building a step for a registered kind, the not-found error
  for an unregistered one, and rejecting undecodable persisted args (the
  reconstruction seam where bad input is now caught);
- the **task service** — its `CreateX` methods against the in-memory store:
  correct ordered step kinds, ids generated and threaded consistently across
  steps, and branch ancestor wiring.

What is **not** unit-tested here is `PgStore`'s SQL (the claim/lease query, the
transactional `Create` writes, the `step_ids`-ordered `Steps` query) and
`PgBackend`'s `Ensure*` SQL — there is no DB-backed Go harness in the repo today
(the same gap as the rest of `PgBackend`). That SQL is exercised by the integration suite;
the `0002` schema + the new `uuid[]` / `array_position` queries were validated by
applying the migrations against YugabyteDB directly.
