# amp-controller — Task Framework & Step Catalog

**Status:** Draft
**Last updated:** 2026-08-27

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
world-changing operations (`createCluster`, `createBranch`, `createEndpoint`,
`deleteCluster`, `deleteBranch`, `deleteEndpoint`) validate cheaply, plan a task,
and return `202` + the `Task` + a `Location` header for the client to poll
([#113](https://github.com/yugabyte/amp/issues/113); the wire contract is in
[`ac-apis.md`](ac-apis.md)).

**Terminology.** The controller's user-facing ladder is **storage pool → cluster
→ branch → endpoint** ([#147](https://github.com/yugabyte/amp/issues/147)).
A cluster is a purely control-plane grouping (it maps to no storage object); a
branch's `branch_id` **is** the storage layer's `BranchId`, the same uuid in the
same spelling. The controller carries it verbatim to the storage seam, and
`deploy.EndpointSpec` passes it as the endpoint image's `BRANCH_ID` env var
(#168).

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

A task is two kinds of row (schema in migration `0002_resource_model.up.sql` — §4.1; DDL
sketch in §8.1 of the high-level doc):

- **`task`** — one per operation, the user-visible unit (`GET /tasks/{id}`). Holds
  the kind, a coarse status, the target cluster/ref (for monitoring), the
  originating request (`params`), `step_ids` (its steps in execution order), the
  owner, and (on failure) the error plus `error_code` — `conflict`, `not_found`
  or `cancelled`, derived from the step's cause by `errors.Is`. A code classifies
  the failure; it does not say whether a retry can succeed — a `conflict` can be a
  second writer, or a delete the controller asks the caller to retry
  (`DeleteBranch` blocked by a child). Ownership's *expiry* is not here — it is a
  lease in `amp_registry.resource_token` ([`ac-locking.md`](ac-locking.md)), and
  `tasks.lease_expiry` is a deprecated leftover nothing reads.
- **`task_step`** — the idempotent steps, one row each, keyed by its own `id`
  (referenced from `task.step_ids`, which fixes their order); a `task_id` backref
  keeps "the steps of this task" a cheap lookup. The `args` are written when the
  task is created; `result` and `status` are updated as it runs, and `started_at`
  (migration 0014) is stamped each time an attempt starts — `updated_at` is
  rewritten by every transition, so it is the only record of when a step began.
  These rows **are** the write-ahead log.
- **`subtasks`** (migration `0006`) — a third kind of row, and the exception to
  everything above. A task that fans out over a set it cannot know until Execute time
  creates one subtask per item and **runs them itself**, in its own process. A subtask
  is *one registered step* — `kind` is a `task_step.kind`, there is no step list — with
  its own status, args, result and error, fenced on the parent's `owner`.

  **A subtask is not claimable**, and that is the point of a separate table rather than
  a `parent_id` on `tasks`: `Candidates` reads `tasks`, so there is no predicate to
  forget. The parent holds the only task lease; each child holds only the resource lock
  its step declares. A child's id is derived from `(parent_id, kind, args)`, so a
  replaying parent converges on the row it already made. The one user today is the
  fleet endpoint upgrade — [`ac-endpoint-upgrade.md`](ac-endpoint-upgrade.md) §5 is the
  design, and §5.2 is why the shape is what it is.

**Creation is write-ahead.** A `Service.CreateX` method (`tasks.go`) generates
*all* the ids the operation will need — cluster, branch, endpoint — up front and
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
resource can interleave. Two tasks mutating one *endpoint* are now serialized by
the endpoint lock ([`ac-locking.md`](ac-locking.md)), the first concrete case of
[meko#454](https://github.com/yugabyte/meko/issues/454) (amp#176 before the
renumber). The steps that fan out over many endpoints still are not:
[meko#976](https://github.com/yugabyte/meko/issues/976).

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

**Cancellation is cooperative** (`POST /tasks/{taskId}/cancel`).
The API stamps `tasks.cancel_requested_at`; the worker re-reads the row at the top
of each step iteration and, if the request is there, abandons the task's
unfinished children and calls `FailStep` on the step that would have run next. So
a cancel takes effect at a **step boundary** — nothing in flight is killed, and a
task whose last step is already done still succeeds.

**What that does not reach.** A step already running is never interrupted, the
fleet roll below aside. Most kinds plan a single step, so for those the only
boundary is the one before that step starts, and neither a task whose step is
running nor one still `pending` is stopped: the flag is read inside `execute`, which runs only after a claim, so a
task no worker claims keeps both the request and its status indefinitely. That
last case is the wedge [meko#1007](https://github.com/yugabyte/meko/issues/1007)
is about, and this does not clear it — retiring the row by hand is still the
remedy (`controller_runbook.md`). `abandonChildren` is likewise reachable only on
a *reclaimed* fan-out parent, never on one this process is running.

**Where the reach is wider.** An `upgrade_endpoints` roll re-reads the flag
between its children (§5), so the longest-running task there is can be stopped
mid-flight instead of running the whole fleet regardless. It is the one kind with
a real boundary inside a step.

**What it reaches but does not clean up.** The check sits inside the step loop,
after the skip for already-succeeded steps, so it fires before *every* unfinished
step — the four kinds that plan more than one (`create_cluster` 2–3, `branch` 1–2,
`voyager_start_migration_run` 2, `voyager_create_migration_target` 4) can therefore
stop mid-task, with their earlier steps committed and no compensation run. Two of
those leave state nothing converges: a cancelled `voyager_start_migration_run`
wedges its migration behind `run_in_progress`, and a cancelled
`voyager_create_migration_target` leaks a live endpoint past both GC sweepers.
[meko#1385](https://github.com/yugabyte/meko/issues/1385) is the fix; until it
lands, do not cancel a voyager task.

It ends `failed`, not under a status of its own: terminal-ness is spelled out as
the literal `status IN ('pending','running')` across the claim predicate, both
self-releasing partial unique indexes, `MemStore` and the failed-create GC
sweeper, and the sweeper in particular reclaims a half-provisioned create only
when it reads `failed` — a cancelled `create_endpoint` under a fifth status would
hold its branch's single-writer slot forever. `failed` also reaches the reconcile
agent through an arm it already has, which re-plans; a status it does not know
falls through its default arm and is read as success. `cancel_requested_at` is what
tells a cancelled task apart from a broken one.

`Store.RequestCancel` carries **no owner fence**, deliberately: it comes from an
API caller who is not the owner, and a `pending` task has no owner at all, so a
fence would make it a permanent no-op. It is guarded on non-terminal status alone
and never writes `status`, `owner` or `error` — taking a task terminal stays the
fenced worker's write. A running step cannot see the request through its own args
(the registry decodes them into the step by value), so a step long enough to want
it re-reads the row through `Deps.Tasks` (`TaskReader`). It is not the only unfenced write on `tasks`: `Store.Claim`
is status-only too, because re-stamping `owner` *is* the reclaim. What every
fenced write has in common is that it takes a task terminal.

**Ownership and recovery** (§8.2) is a **lease**, and it lives in
`amp_registry.resource_token` rather than on the task row —
[`ac-locking.md`](ac-locking.md) is the design; this is what the worker does with
it. `Store.Candidates` offers the tasks that may be claimable (pending, plus
running ones whose lease nobody holds), the worker takes `task:<id>` on one of
them, and only then does `Store.Claim` transition it to `running` and stamp
`owner`. The grant is the claim: a task never reaches `running` unowned. A task is
reclaimable exactly when its lease has lapsed, which is how a crashed owner's work
comes back — the new owner runs the same loop, the already-`succeeded` steps are
skipped, and replaying the step that was in flight is safe because **every step is
idempotent**.

The task runs under the worker's own context and `pglease` renews in the
background, so there is no heartbeat here, and a step that legitimately outlives the TTL (a `create_endpoint` helm deploy
waits minutes for the pod) is not reclaimed and run a second time. Without that, a
reclaimed second execution races the first — the two `helm` runs uninstall each
other's in-flight release (`Error: services "…" not found`) and their terminal
writes clobber, flipping the task's status (the flake behind #340). Losing the
lease **exits the process** (`ac-locking.md`), so the reclaim path is a restart.

Every task/step mutation (`StartStep`, `CompleteStep`, `FailStep`, `CompleteTask`)
is additionally **owner-fenced** — each `UPDATE` carries `AND owner =
$currentOwner`, steps through their parent task's owner. A worker that has lost
the lease cannot advance or finish a task another instance now owns even if it
briefly keeps running: its writes are no-ops. `Claim` re-stamps `owner`, which is
what closes the fence behind a takeover ([#90](https://github.com/yugabyte/amp/issues/90)).

**Endpoint locks.** A step may declare a resource it needs exclusively
(`Locking`); the worker takes that lock before running the step, waits out a
holder, and releases it when the task leaves execution. `create_endpoint`,
`resize_endpoint`, `upgrade_endpoint` and `delete_endpoint` all declare the
endpoint they touch, so
two tasks cannot mutate one endpoint's compute at once whichever process owns
them. A task may hold at most one. The cascade deletes declare nothing — they
touch many endpoints, and that needs intent locks.

`plan_endpoint_upgrades` declares `fleet:endpoints` instead — one fleet roll at a
time, and the only lock that **refuses** rather than waits: a second roll is
denied it and fails, since two rolls at different versions would flip the fleet
twice ([`ac-locking.md`](ac-locking.md) Lock 3). Its
children each take an `endpoint:<id>`, so the process holds two at once, nested,
one per unit of work. The one-lock rule still holds per unit; what keeps the pair
safe is an **order** — `fleet` before `endpoint`, never the reverse
([`ac-endpoint-upgrade.md`](ac-endpoint-upgrade.md) §5.3).

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
| `create_cluster` | pool, cluster, branch ids, name, pg_version, engine, extensions | Create the cluster + its default branch (the create-time extension set is written onto the default branch row; forks inherit it). `engine` is the named bundle the cluster was provisioned as — the handler has already folded its extensions into `extensions`, so the step only records it on the cluster row. | `EnsureCluster`: `INSERT … ON CONFLICT (pk) DO NOTHING` on both rows — same ids re-run is a no-op. A *name* collision (different ids) is a real error. When the cluster INSERT actually inserts, it also writes the pool row (`SET dropped_at = dropped_at WHERE dropped_at IS NULL`) — the pool-level twin of `create_endpoint`'s branch interlock below: `deleteStoragePool` guards on a read, so without this write a create planned while the pool was live lands a cluster under a tombstoned pool. |
| `create_branch` | parent, cluster, branch ids, name, fork LSN, pg_version | Fork a branch into a new branch (records ancestry). | `EnsureBranch`: same `ON CONFLICT (pk) DO NOTHING`; a missing parent or name collision is a real error. |
| `record_branch_recipe` | cluster, branch ids, branch name, pg_version, ancestor (opt), start LSN | Record the controller's pending-init recipe (materialized later by the first endpoint). | The controller upserts the recipe; with no controller the step no-ops with a warning (the recipe then lives only in the `branch` row). A fork's ancestor interlock is inside this step's single transaction (`registry.RecordBranch`). The abort it provokes is re-run by the worker (see the interlock-retry invariant below), so a fork racing any other writer of the ancestor's row — a drop, another fork, an endpoint create on it, or GC advancing its cutoff — is not a task failure. |
| `create_endpoint` | endpoint, cluster, branch ids, role, pg_version, cpu, memory, ancestor (opt) | Ensure the endpoint record, deploy the compute (with its CPU/memory reservation), record the address and version, advance the fleet version when every endpoint is at this one's (`ac-endpoint-upgrade.md` §6.1), clear the branch recipe, record a root branch's fork floor (below). **The shared step** create-cluster, branch, and scale-up all reuse (§8). | Record insert is id-keyed `ON CONFLICT (id) DO NOTHING` (so the single-writer index is not re-violated on replay); deploy is keyed by the endpoint's stable name (deployer idempotency); address is an absolute `UPDATE`; recipe clear tolerates an already-absent recipe. `cpu`/`memory` are resolved by the API handler — 2 cores by default, with memory following CPU at the control plane's memory-per-vCPU ratio, and a pair below that ratio refused — and carried in the args, so a reclaim/replay redeploys the same size. Every planner that appends this step resolves them the same way (cluster create, fork-with-writer, Voyager target), because a step that reaches the deployer with an empty size lands on its fallback instead. |
| `resize_endpoint` | endpoint id, cpu, memory | Change the endpoint's compute reservation: resize the **running pod in place**, then persist the new size to the endpoint `config` (`internal/task/step_resize_endpoint.go`). Records the in-place outcome on the step result. | Resize-first, then persist, so a size the pod rejected never lands in config: the deployer resize is idempotent and `UpdateEndpointResources` is an absolute jsonb merge, so a replay converges. A deferred/infeasible in-place outcome is not an error and is still persisted, so a controller-driven redeploy lands the new size. The deployer does **not** patch the Deployment template (that would trigger a rollout and recreate the pod — the opposite of in-place); an involuntary recreate before the next redeploy comes back at the old size (durable in DB, converges). **First-writer-wins lock**: the partial unique index `tasks_one_active_resize_per_endpoint` (migration 0001) admits at most one non-terminal `resize_endpoint` per endpoint (`target_ref`); a concurrent one is `ErrResizeInProgress` → `409`. The lock self-releases when the task reaches terminal (a crashed task is reclaimed by its lease, so it can't wedge). |
| `update_endpoint_connectivity` | endpoint id, allowed_cidrs | Replace the endpoint's allowed source ranges in place (`internal/task/step_update_endpoint_connectivity.go`): on an `nlb_*` endpoint, merge-patch `loadBalancerSourceRanges` on the live `<release>-lb` Service (`deploy.SourceRangesUpdater`), then persist the list to `config.allowed_cidrs`. Never re-renders the release, which after a resize would roll the pod. Takes the endpoint lock. | **Patch-first**, like resize: the row is what `GET` reports and what the YCP agent compares its spec to, so persisting first would let a failed patch read as applied. The patch is an absolute merge and the write an absolute jsonb merge, so a replay converges. **First-writer-wins lock**: `tasks_one_active_connectivity_per_endpoint` (migration 0013), `ErrConnectivityInProgress` → `409`. |
| `upgrade_endpoint` | endpoint id, version | Move one endpoint onto a target version: record `target_version`, redeploy the endpoint's re-rendered spec with that version on it, then record the running `version` and the new address (`internal/task/step_upgrade_endpoint.go`). | The two columns make an interrupted upgrade legible: a crash anywhere leaves `target_version` ahead of `version`, so the endpoint still reads as not-at-target and replay picks it up (`ac-endpoint-upgrade.md` §4). Moves the version and nothing else — it re-resolves the branch's extensions so the redeploy keeps its mounts, but does not rewrite the recorded set. Declares the endpoint lock. Rebuilt from `{endpoint_id, version}` alone, so it reads its own row rather than being handed one: a record carried in args would silently revert a resize that landed between plan and replay. |
| `plan_endpoint_upgrades` | version, endpoint ids (opt), max_parallel | The fleet roll's parent (`internal/task/step_plan_endpoint_upgrades.go`): enumerate the target endpoints at Execute time, create one `upgrade_endpoint` **subtask** each, and run them in-process through a bounded pool. Reports one line per endpoint (`upgraded`/`skipped`/`failed`/`unattempted`) on the step result. On success, re-reads the fleet and records its target as the fleet version when every endpoint is at it (`ac-endpoint-upgrade.md` §6.1). | Self-contained like the cascade deletes, and its progress comes from its children's status rather than from any cursor — a re-filter on state alone cannot tell "already there" from "this roll put it there", and would report a roll clean whose children failed. `Create` converges on an id derived from `(parent, kind, args)`, so a replaying parent gets back the rows it already made and re-runs only the non-succeeded ones. Caller-supplied ids are deduplicated before creating (two work-list entries for one row is a double `helm` — `ac-endpoint-upgrade.md` §5.2), and the already-at-target filter applies to whole-fleet enumeration only, never to an endpoint the caller named. Declares `fleet:endpoints`. |
| `delete_endpoint` | endpoint id | Retire the running instance, then delete the record. | Both effects tolerate an already-absent target. Order matters: retire the instance *before* deleting the record, so a crash between them leaves a record whose instance can still be retired on replay (never an orphaned instance with no record). |
| `delete_branch` | cluster, branch ids | Cascade-delete a branch: enumerate the branch's endpoints, retire each (instance + record), then delete the branch row. | Self-contained — it **enumerates children at Execute time** (`EndpointIDsForBranch`), so an endpoint added between planning and execution is still retired on replay. Cascade order is **endpoints → branch**: the FKs are `RESTRICT`, so children go first; every effect tolerates an already-absent target, so a re-run mid-cascade converges. A still-referencing child fork makes `DeleteBranch` a real error (correct), as does an endpoint still on the branch — an endpoint being created *concurrently with* the tombstone is caught by the other side's branch-row interlock (§3, `create_endpoint`). |
| `delete_cluster` | cluster id | Cascade-delete a cluster: enumerate **all** the cluster's endpoints, retire each (instance + record), then delete the cluster — which drops all its branches. | Self-contained, enumerating children at Execute time (`EndpointIDsForCluster`). Cascade order is **endpoints → branches → cluster**; `DeleteCluster` nulls the branches' in-cluster parent pointers before dropping them to sidestep the self-FK `RESTRICT` ordering. Idempotent throughout. |
| `add_branch_extensions` | cluster id, branch id, extensions | Union the names into **that branch's** extension set, then **redeploy the endpoints whose recorded mount set is behind it** so the changed ImageVolume mounts take effect. | Self-contained like `delete_branch` — enumerates the branch's endpoints at Execute time (`BranchEndpointDeploys`), so a concurrently-added endpoint is still remounted on replay. `AddBranchExtensions` is an absolute union; each redeploy is keyed by the endpoint's stable name (deployer idempotency) and the config write is an absolute merge, so a re-run mid-cascade converges. **First-writer-wins lock**: `tasks_one_active_ext_add_per_branch` (migration 0001) admits at most one non-terminal `add_branch_extensions` per branch (`target_ref`); a concurrent one is `ErrBranchExtensionsInProgress` → `409`. Needed because two concurrent adds would redeploy overlapping endpoint sets — concurrent helm upgrades of the same release. |
| `plan_endpoint_rerenders` | endpoint ids (optional), max_parallel | The re-render roll: enumerate the fleet (or the named subset) and own a `rerender_endpoint` **subtask** per endpoint, run through the same bounded pool `plan_endpoint_upgrades` uses (`internal/task/endpointroll.go`). What an operator runs after enabling TLS (`ac-endpoint-tls.md`). | Self-contained and re-enumerating, like the upgrade roll. Holds `fleet:endpoints`, which is `NoWait` — so one fleet roll of either kind at a time, and the plan-time `noFleetRollInFlight` check is what turns the second into a 409 rather than a task that fails in the worker; each child holds its own endpoint lock. Nothing is filtered — the endpoint row does not record which configuration it was rendered with — and a re-render of a converged endpoint is a no-op Helm upgrade. |
| `rerender_endpoint` | endpoint id | *(a subtask, not a task step)* Redeploy one endpoint from its stored record, unchanged, so deployer-level configuration (TLS mode, CA bundle, image) reaches a release rendered before it (`internal/task/step_rerender_endpoint.go`). Re-records the address. | `upgrade_endpoint` with no mutation, and the same create-or-upgrade keyed by the endpoint's stable name, so a replay converges. An endpoint deleted since planning is a skipped success rather than a fail-fast trip. |

`delete_branch`, `delete_cluster`, `plan_endpoint_upgrades` and
`plan_endpoint_rerenders` are **single self-contained steps**, not a planned
per-endpoint fan-out, because the worker has no dynamic sub-step expansion: the
set of endpoints isn't known until Execute time, so the one step enumerates it
itself. All four require a deployer. The
difference is what they do with the set — the cascades act on it inline, while the
roll records a `subtasks` row per endpoint and runs those, so each endpoint has a
durable status of its own and takes its own endpoint lock.

A few invariants worth calling out:

- **A task can declare the YCP generation it applies, and the *worker* stamps it
  after the last step succeeds.** `tasks.ycp_resource` / `ycp_resource_id` /
  `ycp_generation` (migration 0001) name the endpoint or branch row and the
  generation; `execute` writes it through `StateStore` between the step loop and
  `CompleteTask`, so the recorded value means the work landed rather than that the
  handler returned `202` ([ac-ycp.md](ac-ycp.md)). Not a step, deliberately: a step
  is one more thing each planner must remember and one more place a task can fail
  with the work already done. `Service.createWithID` derives it from the kind, so a
  planner passes nothing extra, and the write is `GREATEST(...)` so an older task
  completing late cannot regress it.
- **A task `Kind` carries its own metadata, declared with unkeyed literals.**
  `Kind{name, ycpResource, lockIndex}` — which resource `target_ref` names for YCP,
  and which partial unique index locks the kind — so adding a kind does not compile
  until both are answered, and both first-writer-wins checks (`PgStore`, `MemStore`)
  read `lockIndex` instead of naming a kind, so they cannot drift.
  `TestPgUniqueConstraintsOnTask` derives its expectation from `allKinds`, so a
  `lockIndex` with no index in the schema — or an index no kind names, whose
  `23505` would surface as a 500 — fails there.

- **`create_endpoint` records the row before it deploys.** The record claims the
  single-writer slot (a second writer is rejected by the partial unique index
  before any compute is spent) and gives the deploy a stable id.
- **The record has to land on a *live* branch, and that check is a write.** The
  FK cannot enforce it — tombstoning leaves the branch's row in place — and a read
  of `dropped_at` does not hold against a concurrent drop: the drop's own guard
  (`NOT EXISTS (endpoints)`) is a read too, and neither side writes what the other
  reads, so under snapshot isolation both commit (write skew) and leave a live
  endpoint on a tombstoned branch. Nothing then enumerates that endpoint to retire
  it — every cascade reader filters `dropped_at IS NULL` — while its compute keeps
  serving, and branch-drop GC reclaims the branch's pages and WAL out from under
  it. So `EnsureEndpointRecord` runs the insert and a conditional
  self-assignment **`UPDATE amp_registry.branches SET dropped_at = dropped_at
  WHERE branch_id = … AND dropped_at IS NULL`** in one transaction: both sides now
  write the branch row, so they serialize. Zero rows ⇒ the branch was dropped
  concurrently ⇒ the whole record rolls back and the step fails `ErrNotFound`; a
  serialization abort carries no sentinel — it is not a verdict on the request —
  and the worker re-runs the step on it (invariant below). The interlock is gated
  on the insert having actually inserted, so a replay stays an idempotent no-op.
  `delete_cluster` tombstones every branch row in the cluster, so it conflicts the
  same way. Same shape as the fork↔cutoff interlock in `registry.RecordBranch`.
- **Fork vs. default branch.** When `create_endpoint`'s args carry an ancestor,
  the endpoint clones from that ancestor at the fork LSN (`Bootstrap` forced off);
  otherwise a default branch runs `initdb` when the deployment's bootstrap flag is
  set. The deploy
  spec carries the branch id verbatim as the spec's `BranchID` (and the ancestor's
  for a fork); a cluster no longer maps to a storage tenant (#168).
- **A step that loses a write interlock is re-run, not failed.** The interlocks
  above *provoke* a serialization abort (SQLSTATE 40001) by design: two writers are
  made to collide on one row so one of them loses, instead of both committing on
  stale views of each other. Losing is the mechanism working, and the condition
  clears the moment the winner commits — so the worker re-runs the step (6 attempts,
  jittered exponential from 15ms, and a 2s wall-clock budget) rather than failing
  the task, which would strand an operation a background GC tick was enough to
  provoke (#578). Re-run **in place**, holding the lease: a re-queue would cost a
  `PollInterval` per attempt for a conflict that resolves in milliseconds.
  The unit re-run is the whole **step**, not the transaction that aborted, so a step
  whose external effects precede its retryable write repeats them once per attempt
  (`delete_branch` retires endpoint instances before tombstoning the branch row).
  They are idempotent by the Step contract, so that is waste rather than damage, and
  the wall-clock budget is what bounds it — an attempt count bounds re-runs, not
  their cost.
  The class is **derived** from the driver error — `db.IsAbortedTransaction`, one
  definition shared by the worker that acts on it and the interlock owners that
  raise it — rather than declared by a sentinel the failing step attaches: an abort
  is not a verdict on the request, so it is a member of neither store's error
  vocabulary, and a sentinel a producer must remember to attach is how #578
  happened. It names the *precondition* (the transaction rolled back in full) rather
  than "retryable", because that is what makes re-running safe; this is sound only
  while an operation that can surface one is atomic, an invariant stated on the
  ports in `internal/task/deps.go`.
- **Clearing the recipe is best-effort.** A lingering recipe is harmless (nothing
  reads it once the branch is materialized), so a clear failure logs a warning and
  the step still succeeds.
- **`create_endpoint` records a root's fork floor, best-effort.** When the
  branch is a root with no floor (`RootForkLSNPending`), the args carry a
  verifier, and the deployer gives a supervisor URL and the endpoint a
  controller URL (without which neither deployer opens the API), the step polls the
  supervisor's `GET /v1/remote-auth` (`Deps.RemoteAuth`) every 2 s until it has
  decided, for up to 3 minutes. `applied` records its `lsn`; `already_set`,
  `applied_no_lsn` and `unsupported` record `0/0`, since the catalog has a
  password set at an LSN nothing knows and recording "now" would refuse forks
  into history that has it. Both go through `SetRootForkLSN`, first writer wins.
  `unset`, `failed` and `unknown` record nothing, and no outcome or error fails
  the step. A shutdown during
  the wait interrupts the step so the next controller re-runs it; a replay that
  finds the floor set skips the wait. The step's result carries the outcome as
  `compute_password`: the supervisor's state, `applied_no_lsn`, `unknown` (still
  pending at the budget, or unreachable) or `unsupported` (a `404` from an
  endpoint image older than the route). Roots whose writer succeeded without a
  floor — created before this, or by an older controller mid-rollout — get `0/0`
  from `BackfillRootForkLSN`, which skips a writer that reported `unset`,
  `failed` or `unknown` and which `cmd/controller` runs once per start
  off the startup path, and `createBranch` for a root it is asked to fork
  ([`controller-cluster-create.md`](controller-cluster-create.md#the-fork-floor)).
- **Direction (planned).** `create_endpoint` is the landing site for the SC
  integration, evolving
  toward a create-then-configure model (create an unassigned pod, then push a
  `configure` binding it to a branch). See
  [`sc-integration.md`](sc-integration.md).

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
| `create_cluster` | `Service.CreateCluster` | `create_cluster` → `record_branch_recipe` → (`create_endpoint`, when a first writer is requested). |
| `branch` | `Service.Branch` | `create_branch` → `record_branch_recipe` (ancestor set) → (`create_endpoint` cloning from the ancestor, when requested). |
| `create_endpoint` (scale-up) | `Service.CreateEndpoint` | `create_endpoint`. |
| `delete_endpoint` (scale-down) | `Service.DeleteEndpoint` | `delete_endpoint`. |
| `delete_branch` | `Service.DeleteBranch` | `delete_branch`. |
| `delete_cluster` | `Service.DeleteCluster` | `delete_cluster`. |
| `add_branch_extensions` | `Service.AddBranchExtensions` | `add_branch_extensions`. |
| `rerender_endpoints` | `Service.RerenderEndpoints` | `plan_endpoint_rerenders`, which fans out to a `rerender_endpoint` subtask per endpoint. |

The kind constants (`KindCreateCluster`, `KindBranch`, `KindCreateEndpoint`,
`KindDeleteEndpoint`, `KindDeleteBranch`, `KindDeleteCluster`,
`KindAddBranchExtensions`, `KindRerenderEndpoints`) are the
`task.kind` values the API exposes and `listTasks` filters on.

`create_endpoint` appearing in three of the eight tasks is the point: it is one
step, reused, not three copies (§8). Branch fork-point resolution (the default
"latest LSN") is a *read* done before the builder runs, so the fork LSN reaches it
already resolved — building a task performs no I/O of its own. The two cascade
deletes are each a single self-contained step (§3) — the children are enumerated
at Execute time, not planned up front.

### 4.1 Reading tasks back: `Get` and `List`

`Service.Get` reads one task back (`GET /tasks/{id}`); `Service.List(ctx, filter,
limit, offset)` returns a page of tasks newest-first (`created_at` DESC) plus the
total matching count for the **`listTasks`** endpoint (`GET /tasks`). The filter
(`TaskFilter`: `status`, `kind`, `cluster_id`, any of which may be zero-valued =
unfiltered) maps straight from the spec's query parameters. `List` is backed by
`Store.List`, whose scan is kept cheap by the task-list indexes in `0001_task_log`
(§2): `tasks_by_created_at` (the DESC order), `tasks_by_cluster`, and
`tasks_by_status` (the optional filters). These are distinct from the
`tasks_runnable` partial index (also `0001_task_log`), which serves the worker's claim path,
not the list path.

`TaskFilter.UpdatedSince` turns the same endpoint into a **change feed**: only tasks
whose `updated_at` is at or after it, ordered `updated_at` ASC instead of the default
`created_at` DESC, backed by `tasks_by_updated_at` (`0001_task_log`).
The ordering has to change with the filter, which is why it is one field and not two.

It exists for the YCP reconcile agent, which relays this log to the control plane so a
console can show what is running and what failed. Neither of the obvious alternatives
works. Polling `status=pending|running` misses any task created and completed between
two of the agent's ticks — `create_branch` routinely is — because it is never observed
in flight. And a watermark cannot be walked over the default ordering: a task created
an hour ago that has just reached terminal sorts *below* rows the reader already
consumed, so `created_at` DESC skips exactly the transitions the reader most wants.
`updated_at` is stamped on every status transition (`store.go` — claim, fail, succeed),
so it is the one column that orders changes rather than creations.

The bound is inclusive (`>=`): two tasks can share a timestamp, and an exclusive bound
set to the last row's `updated_at` would drop its same-instant twin. Readers therefore
re-receive the boundary rows and must tolerate the repeat. `TaskList.ordered_by` echoes
the ordering actually applied, so a reader can tell a change feed from the default page
instead of assuming its parameter was honoured — a controller predating `updated_since`
ignores it and omits the field, and a reader that assumed otherwise would advance its
watermark over a `created_at`-ordered page and lose everything it had not already seen.

## 5. Dependency ports

Steps depend on narrow interfaces, not concrete collaborators, so they
unit-test against in-memory fakes and the framework stays decoupled. The ports
are bundled into `Deps`, which the registry binds into each step at construction
(so a built step keeps only the ports it uses, and `Execute` needs only a
context):

- **`StateStore`** — the idempotent, id-keyed cluster/branch operations. The
  create/scale path: `EnsureCluster`, `EnsureBranch`, `EnsureEndpointRecord`,
  `SetEndpointAddress`, `DeleteEndpoint`. The cascade-delete path adds the
  enumeration + drop primitives the `delete_branch`/`delete_cluster` steps use:
  `EndpointIDsForBranch` and `EndpointIDsForCluster` (the at-Execute-time child
  enumeration — an absent target yields an empty slice, not an error, so a re-run
  mid-cascade converges), `DeleteBranch` (drops one branch row; the parent FK is
  `RESTRICT`, so a still-referencing child fork is a real error), and
  `DeleteCluster` (drops the cluster and all its branches, nulling the branches'
  in-cluster parent pointers first to sidestep the self-FK ordering). All are
  id-keyed and idempotent (an absent row is a no-op). `*core.PgBackend` satisfies
  the interface structurally; these are deliberately *not* on the `core.Backend`
  interface, so the synchronous API read/pre-check path keeps using the
  id-generating create methods.
- **`RecipeStore`** — record the controller's pending-init recipe, keyed by the
  branch's `branch_id` uuid. A `BranchRecipe` decouples the task package from the
  registry store's types; `cmd/controller` bridges the two (`recipeStore`, mapping
  `BranchRecipe → registry.BranchCreateRecord`). May be nil (no registry store):
  a fork then **fails**, because this step is the branch's only creator and an Ok
  would report success having created nothing; a cluster's default branch warns and
  continues, its row already written by create_cluster.
- **`deploy.Deployer`** — the existing runtime-agnostic endpoint deployer, reused
  as-is (its `EndpointSpec` passes the `branch_id` as the endpoint image's
  `BRANCH_ID` env var, #168).
- **`RemoteAuthReader`** — the `node.Client` the API resolves fork points with,
  which `create_endpoint` asks for a root's floor. Nil without a deployer.

`ProductionRegistry(deps)` registers the catalog's builders and holds the deps,
so each step it builds comes out fully wired; `cmd/controller` builds the `Deps`
from the same `Provisioning` the API handlers' cheap pre-checks use and passes
them to `ProductionRegistry`, then hands the registry to `NewWorker`. It also
wraps the store in a `Service` and hands that to the API. The mutation handlers
plan tasks through that `Service` (#113), so the worker live in `serve` now has
real work to claim: every `createCluster`/`createBranch`/`createEndpoint`/
`deleteEndpoint`/`deleteBranch`/`deleteCluster` request records a task it runs to
completion.

## 6. Testing

The package is tested mostly without a database (matching `core.MemBackend`; the
one live-database exception is below): in-memory fakes for every port and the
in-memory `MemStore` for the `Store` itself — `MemStore` is a real, exported type
(`mem.go`), so the API package's tests run against it too rather than carrying
their own task-store fake. The tests cover

- the **worker**, two ways: the per-task `execute` path against a faithful
  in-memory store — runs all steps to completion, **resumes** a reclaimed task
  skipping already-succeeded steps, **replays an in-flight `running` step**,
  **fails** the task and halts later steps on a step error, fails on an unknown
  step kind, and leaves a cancelled task reclaimable; and the `Run` **claim loop**
  — it drains a backlog concurrently and never exceeds `MaxConcurrent` tasks in
  flight (a gated step asserts the cap), stops cleanly on cancel, recovers a
  panicking step into a task failure, logs a shutdown interruption as such (not as
  an error), returns within `ShutdownGrace` even if a step ignores cancel,
  keeps a long-running task off its own candidate list (the held lease is what
  does it), and gives the lease back when a task leaves execution — on success, on
  failure, and on a shutdown mid-step — which is what makes a restart cheap;
- **every step** — its happy path, **idempotent replay** (run twice, converge),
  error propagation, the fork-vs-default-branch deploy spec, and the best-effort
  recipe clear;
- the **registry** — building a step for a registered kind, the not-found error
  for an unregistered one, and rejecting undecodable persisted args (the
  reconstruction seam where bad input is now caught);
- the **task service** — its `CreateX` methods against the in-memory store:
  correct ordered step kinds, ids generated and threaded consistently across
  steps, and branch ancestor wiring.

The `TestPg*` tests need a real database because `MemStore` cannot stand in.
`store_pg_test.go` covers the resize lock's `SQLSTATE 23505 → ErrResizeInProgress →
409` mapping (`TestPgTaskLocks`, over every locked kind) and the schema assumption it rests on
(`TestPgUniqueConstraintsOnTask`, below); `locks_pg_test.go` covers the lease
timings that `MemLocks` deliberately does not model
([`ac-locking.md`](ac-locking.md)). `MemStore` rejects
a concurrent resize with a plain in-memory check, so it exercises none of the
mapping — which is how #400 shipped a lock that worked while every concurrent
resize returned **500** instead of `409`. Both migrate a schema of their own and
skip unless `AMP_TEST_DATABASE_URL` points at a YugabyteDB; CI's "Migrations &
live-DB store tests" job provides one.

That bug is also a **platform rule worth keeping**: do not key an error check on
`pgconn.PgError.ConstraintName`. Upstream PostgreSQL raises a unique violation
with `errtableconstraint()`, which populates the wire protocol's `C` field that
pgx exposes as `ConstraintName`; YSQL builds the same error in `pg_yb_utils.c`,
formatting the constraint name into the **message** and leaving `ConstraintName`
and `Detail` empty (confirmed on 2026.1: a verbose YSQL unique violation carries
SQLSTATE, message and `LOCATION`, no `CONSTRAINT NAME`). There is therefore **no
portable field to read a constraint name from** — and scraping it out of the
message text only trades a silent break for a brittle one.

So identify a violation by SQLSTATE plus **what the statement was doing**, not by
name. `taskLockViolation` takes the `TaskSpec` being inserted and reports that
kind's `inProgress` conflict for a `23505` on a kind declaring a `lockIndex` with a
`target_ref`. That stays exact with more than one lock because the indexes'
predicates each name one kind, so a violation from this INSERT can only be the
index for the kind being inserted; the only other unique constraint on `task` is
its primary key, whose conflict `Create` already absorbs with `ON CONFLICT (id) DO
NOTHING`. Since that is an assumption about the schema,
`TestPgUniqueConstraintsOnTask` derives the expected inventory from `allKinds` —
`task_pkey` plus every declared `lockIndex` — so a lock with no index, or an index
no kind names, fails there. The other mappings in `core/pg.go` already switch on
`Code` alone and are unaffected.

What is otherwise **not** unit-tested here is the rest of `PgStore`'s SQL (the
claim/lease query, the transactional `Create` writes, the `step_ids`-ordered
`Steps` query) and `PgBackend`'s `Ensure*` SQL (the same gap as the rest of
`PgBackend`). That SQL is exercised by the integration suite; the `0002` schema +
the new `uuid[]` / `array_position` queries were validated by applying the
migrations against YugabyteDB directly.

## Failed-create GC (implemented)

A resource's row is written mid-provisioning, before the steps that build what it
describes, so a create that fails afterwards leaves the row behind. Two kinds leak
that way, and nothing else reclaims either:

- **clusters** — `create_cluster`'s `EnsureCluster` writes the row before the
  storage steps run. The read path hides these (`core`'s
  `clusterCreateNotFailed`, surfaced as `include_failed` on the list API for the
  operator fleet view), which keeps them from being mistaken for real clusters but
  leaves the rows and their Kubernetes resources in place indefinitely.
- **endpoints** — `create_endpoint` records the row *before* deploying,
  deliberately, so the row claims the branch's single-writer slot before any
  compute is spent. A deploy that then fails leaves that slot held by an endpoint
  with no pod, and `endpoints_one_writer_per_branch` makes every later attempt to
  give the branch a writer conflict. This one is not merely untidy: the branch
  cannot be written to again until the row is gone.

**Storage pools are not swept.** Creating one is a single INSERT with nothing to
provision, so a failed create leaves no row — there is no half-created pool to
reclaim, and no delete-pool API to reclaim it with.

`internal/gc`'s **Sweeper** reclaims both. It cannot be a compensating step on
the failure path: the controller can crash *during* that path, and an inline
rollback that never runs leaks silently. So it is a reconciler, and it borrows
this framework's durability rather than adding its own — every tick it runs one
pass per resource kind, selecting that kind's orphans past a grace window and
enqueueing an ordinary **`delete_cluster`** or **`delete_endpoint`** for each.
Teardown is therefore the same code an operator's delete runs, and a crash
mid-teardown is recovered by the ordinary lease reclaim (a task running with an
expired lease is claimable again).

Clusters are swept before endpoints, because a cluster teardown cascades to the
endpoints under it. `BatchLimit` is per kind, so a backlog of one cannot starve
the other. A broken selection query for one kind does not stop the other's pass;
the errors are joined and reported.

**Cluster selection** is the positive form of `clusterCreateNotFailed`, resting on
the same invariant: a cluster has exactly one `create_cluster` task, because a
retry mints a fresh cluster id, so one failed task is definitive. Clusters with a
`delete_cluster` already pending or running are excluded, and a succeeded delete
tombstones the row, which `dropped_at` excludes.

**Endpoint selection** (`core`'s `failedCreateEndpoints`) keys off the
`create_endpoint` *step*, not the task kind, because three kinds create endpoints:
`create_cluster` and `branch` each plan one for `with_writer`, and
`create_endpoint` is the standalone scale-up. Only the last carries the endpoint id
in `tasks.target_ref`, so the step's `args` are what cover all three — which is how
the pass sees a `with_writer` fork whose branch was recorded but whose endpoint
never came up. Beyond "the creating task failed", an endpoint must also have
`address IS NULL` (it never reached the point where an address was recorded, so
nothing usable was handed out — an endpoint that *did* get one is left alone even
if its task later failed, since it may be serving and its owner can delete it
through the API) and a live branch. Endpoints are skipped when a `delete_endpoint`
is already pending or running, and when their cluster is itself a failed-create
orphan, since `delete_cluster` cascades and enqueueing both would spend a task to
no effect.

Before each destructive enqueue the sweeper re-checks that one orphan
(`IsFailedCreateCluster` / `IsFailedCreateEndpoint`), so a batch that went stale —
deleted by hand, tombstoned, or already claimed — cannot cause a redundant
teardown. `Sweep` holds a mutex for the whole pass, which makes "one Sweeper
enqueues a resource once" hold however it is called. Two *separate* controllers
could still both enqueue; that is left unguarded, because the teardown tasks must
be idempotent regardless (the worker re-runs steps after a crash) and the deployed
topology is a single controller. Two controllers would want an atomic conditional
insert in the store.

| flag | env | default | meaning |
|---|---|---|---|
| `--gc-enabled` | `AMP_GC_ENABLED` | `true` | run the sweeper |
| `--gc-interval` | `AMP_GC_INTERVAL` | `5m` | how often a sweep runs |
| `--gc-grace` | `AMP_GC_GRACE` | `30m` | how long an orphan is left alone after its create failed, so a fresh failure can be inspected and a manual retry is not raced |
| `--gc-batch-limit` | `AMP_GC_BATCH_LIMIT` | `20` | max orphans enqueued per sweep, **per resource kind** |

Observability: `amp_controller_gc_sweeps_total`,
`amp_controller_gc_sweep_duration_seconds`, and
`amp_controller_gc_orphans_total{resource=cluster|endpoint,outcome=enqueued|skipped|failed}`.
A flat `orphans_total` against a rising `sweeps_total` means the sweeper is running
and idle rather than stuck; the `resource` label is what makes "endpoints are
piling up while clusters are clean" answerable from the metric alone. Each orphan
acted on is logged with its resource kind, its ids, and when its create failed.
