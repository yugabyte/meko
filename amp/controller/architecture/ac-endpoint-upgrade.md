# ac — Endpoint upgrade (parent task, per-endpoint subtasks)

**Status:** Draft — for review
**Last updated:** 2026-09-01

Move every deployed endpoint — or a caller-named subset — onto a target version in
one call. The first, dev-facing slice of
[#809](https://github.com/yugabyte/meko/issues/809) (under
[#455](https://github.com/yugabyte/meko/issues/455)): no per-cluster pinning, no
maintenance windows, no canary, no PG major-version upgrade.

Read [`ac-tasks.md`](ac-tasks.md) and [`ac-locking.md`](ac-locking.md) first. This
is one new task kind, two new steps, and **one new thing in the framework itself**:
a `subtasks` table, and a task that owns rows in it and runs them in-process (§5).

An earlier attempt at this was written as
[meko#867](https://github.com/yugabyte/meko/pull/867) and closed unmerged when the
locking foundation changed under it (its branch and
[design doc](https://github.com/yugabyte/meko/issues/809#issuecomment-5375942830) are
kept). §7 records what carries over and what `pglease` lets us delete.

**Landed:** all of it — §4.1 (the spec assembler), §2–3 (the version columns and
the per-endpoint override), §4 (`upgrade_endpoint`), §5 (the `subtasks` table, the
runner, and the `plan_endpoint_upgrades` parent) and §6 (the API).

## 1. What was missing

Before §2–3, an endpoint's version was **static deployer configuration**, not a
property of the endpoint:

- `--endpoint-image-tag` / `AMP_ENDPOINT_IMAGE_TAG` became
  `KubernetesConfig.EndpointImageTag`, stamped into every release's `image.tag`;
- `deploy.EndpointSpec` had no version field, so nothing per-endpoint could ask for a
  different one;
- `amp_registry.endpoints` had no version column, so the controller could not say what
  an endpoint was running.

Delete-and-recreate was the only path onto a new build. Three pieces close that: a
per-endpoint version on the deploy spec (§3), a recorded running version on the
endpoint row (§2), and a task that moves one to the other (§4–5).

The convergence machinery is already right. `CreateEndpoint` is `helm upgrade
--install` + `--wait`, and `add_branch_extensions` already re-renders live endpoints
through it. Re-rendering a release at a new version is the existing deploy path with
one value changed.

**What an upgrade costs:** the pod is replaced, so the endpoint is briefly down and
`compute_ctl` re-fetches a basebackup on start. That is safe by construction — PGDATA
is ephemeral, durable state is in YugabyteDB, and two overlapping instances are fenced
by the resource token, not by hoping the old pod is gone. So the cost is an outage per
endpoint, not a correctness risk, and it is why the roll is bounded (§5.4) rather than
fired at the whole fleet at once.

## 2. Recording what an endpoint runs

Migration `0011`:

```sql
ALTER TABLE amp_registry.endpoints
    ADD COLUMN IF NOT EXISTS version text,
    ADD COLUMN IF NOT EXISTS target_version text;
```

`version` is what the endpoint is recorded as **running**, written after a deploy
reports back; every controller-driven redeploy renders from it. `target_version` is
what it **should** run, written before the deploy. The two differing is an upgrade
that has not converged — which is what makes "did this endpoint actually upgrade?"
answerable from the row.

**A version is an opaque string the deployer knows how to run.** Today it is the
endpoint image tag verbatim, and version → `image.tag` is decided in exactly one
function (§3) — so a version that later becomes a bundle id or a manifest reference
changes that function, not the schema, the API, or the task args.

**NULL means unknown**, which is what every endpoint predating this has. Unknown is
upgradeable — there is nothing to compare against — and the upgrade writes the
column, so one pass moves the fleet from unknown to known. **No backfill:** guessing
that an existing endpoint runs the controller's *currently configured* tag is exactly
the assumption that breaks after the first controller upgrade.

**The deployer reports what it deployed**, and the step persists that.
`deploy.Endpoint` (the `CreateEndpoint` return) gains `Version string`, so
`create_endpoint` and `upgrade_endpoint` both record a fact rather than re-deriving
one from config the deployer may have defaulted.

Not modelled: the image *repository* (it stays deployer config — changing it is a
controller reconfiguration, not an endpoint upgrade) and the resolved *digest*. The
consequence of no digest, stated plainly: an endpoint already recorded at the target
version is skipped, so a **mutable tag whose contents changed underneath it cannot be
re-rolled through this API**. Re-pulling `latest` wants the digest column and a
redeploy verb; both belong with whatever needs them.

## 3. Making the version per-endpoint

`deploy.EndpointSpec` gains:

```go
// Version overrides the deployer's configured endpoint version for this endpoint.
// Empty = the deployer default (KubernetesConfig.EndpointImageTag).
Version string
```

Each deployer maps it onto its own image reference in one place, and reports back
the version it actually ran:

- **Kubernetes** renders `spec.Version` as `image.tag` when set, falling back to
  `cfg.EndpointImageTag`. That fallback is exactly today's behaviour, so nothing
  changes for an endpoint that never asks. The fallback is resolved in Go, not left
  to the chart: `image.tag` has no `omitempty` and the chart reads an empty tag as
  its own `appVersion`.
- **Docker** configures one full image reference rather than a repository/tag pair,
  so it substitutes the tag out of it — parsed, because a registry host with a port
  (`localhost:5000/img`) has a colon that is not a tag. A **digest-pinned** reference
  is refused rather than retagged: the digest is what selects the image, so honouring
  the version would deploy the pinned image under a new recorded version. Every
  refusal — the digest case and a version that is not a legal tag — has to happen
  **before** the deployer removes the running container, or a bad version costs the
  endpoint its container with nothing left to bring it back. That is why this driver
  validates the version's own syntax rather than letting `docker run` reject it.
- **local** places nothing, so it echoes the version back — the only one it can name.
  Echoing keeps the record convergent (a roll would otherwise re-select the endpoint
  forever); the usual case is empty in, empty out, which stays unknown.

**`create_endpoint` must carry it too.** Its args gain `Version`, and the step writes
what the deployer reported to the `version` column — otherwise any later
controller-driven redeploy of an upgraded endpoint silently reverts it to the
deployer default. Same argument as `cpu`/`memory` in `resize_endpoint`: desired state
has to be durable on the row, or the next redeploy undoes it. On the column, not in
the `config` blob, so there is one answer to what the endpoint runs.

A consequence worth naming: an endpoint created after this records the tag it ran,
so bumping the controller's `--endpoint-image-tag` no longer moves it on its next
redeploy. Moving it is what §4–6 are for. Endpoints created *before* this have a NULL
version and keep following the configured default until something records one.

**Watch the defaults when testing this.** `--endpoint-image-tag` defaults to `latest`
and `--deploy-endpoint-image` to `yb-amp/neon-endpoint:dev`, and both are required to
be non-empty — so on a default-configured controller *every* endpoint records the
literal `latest` (or `dev`). Combined with §5.1's skip-if-already-at-target, a fleet
of endpoints all recorded at `latest` cannot be rolled to `latest` again, which is the
mutable-tag limitation above meeting the shipped default. Any real exercise of the
upgrade path has to run the controller with a genuine version tag, not the default.

## 4. `upgrade_endpoint` — one endpoint

One step, args `{endpoint_id, version}`:

1. **Record the target** — `target_version`, before anything moves.
2. **Redeploy** through `deploy.Deployer.CreateEndpoint`, re-rendering the endpoint's
   spec with the target version on it.
3. **Record what is running** — `version`, from what the deployer reported, plus the
   address.

The two columns are what make an unconverged upgrade legible: a crash anywhere in
(1)–(3) leaves `target_version` ahead of `version`, which is exactly "asked for, not
yet running". Replay finishes it, and nothing has to infer state from a row that
claims a version the pod may not be running.

Idempotent throughout: absolute `UPDATE`s, and a release already at the target
version converges to a no-op.

An upgrade moves the version and nothing else. It re-resolves the branch's extension
set so the redeploy keeps its mounts, but does not rewrite the recorded set — an
upgrade that changed an endpoint's extensions would be a bug, so it has no business
writing that column.

### 4.1 One assembler for the deploy spec

The upgrade does not re-render an endpoint in a new way — it becomes the **third**
caller that renders one:

| | persists desired state | renders from | deploys |
|---|---|---|---|
| `create_endpoint` | `EnsureEndpointRecord`, from its args | a `core.EndpointDeploy` built from its args | `CreateEndpoint` |
| `add_branch_extensions` | `AddBranchExtensions`, the branch set | `BranchEndpointDeploys` | `CreateEndpoint` |
| `upgrade_endpoint` | `target_version`, §4 | a by-id read of its own row | `CreateEndpoint` |

All three are the same shape — persist desired state, render from it, deploy — and
they arrived there independently: create writes the row first so it claims the
branch's single-writer slot before compute is spent, and the upgrade writes its target
first so an interrupted one stays visible. (`resize_endpoint` is deployer-first and
looks like a counterexample, but it patches a `ResizeSpec` in place and never renders.)

Adding a third hand-rolled `deploy.EndpointSpec{…}` literal is what to avoid. The
struct has ten fields, drawn from three places — six from durable state, three from
deps (`YB`, `ControllerURL`, the catalog), one derived from the id — and **omitting
one is silent**: the deploy succeeds and the endpoint comes back missing a mount,
reverted to a default size, or running the deployer's default version.

So spec assembly lives in one function, beside the type it builds, in
`internal/deploy`:

```go
func EndpointSpecFrom(e core.EndpointDeploy, yb YBConn, controllerURL string,
    mounts []Extension) EndpointSpec
```

It is pure — no context, no error, every input a value — so there is one place to read
to know what a rendered spec contains. `core.EndpointDeploy` stops being "the row
`add_branch_extensions` reads" and becomes the general renderable desired state — which
is why `create_endpoint` **constructs** one from `CreateEndpointArgs` instead of reading
a row back. It carries the endpoint's id and its branch's — `branch_id` is a primary
key, so the branch (and its extension set) is fully addressed by it, no cluster id — and
it gains `Version` in §2.

**It takes the state, it does not read it.** Reading by endpoint id would be safer —
a caller could not under-populate the input — but it is N+1 queries for a fleet roll
and would make create re-read a row it just wrote. Enumeration stays with the callers,
who each already have what they need.

**Extensions are the exception, and that is why `mounts` is a parameter.** They cannot
come off the record: the opt-in set is on the branch and the name → image mapping is in
the catalog, so somebody has to read. That read is the task layer's — domain state,
resolved through `StateStore` and the catalog — so `resolveBranchExtensions` stays in
`internal/task/endpointspec.go` and each caller passes the set it resolved. A caller that
redeploys many endpoints holds the set it resolved and reuses it, which is what keeps a
branch-wide redeploy on a single snapshot: `add_branch_extensions` keeps a per-branch map
across its loop and so does one read for its whole roll, and `create_endpoint` renders one
endpoint and needs no cache at all.

**Extraction alone does not close the hole — the caller-boundary test does.** Moving
the literal only moves where a field can go missing: from the `EndpointSpec{…}` to the
`core.EndpointDeploy{…}` feeding it. A test that hands the assembler a fully-populated
record passes no matter what the *step* put in there. Verified by mutation: dropping
`PgVersion` from `create_endpoint`'s record leaves build, vet and the whole suite green
while an endpoint pinned to pg 15 deploys pg 18, because
`internal/deploy/kubernetes.go` defaults a zero. So each caller needs an assertion
on the spec the deployer actually **received**, and each backend that fills the
record needs one too — `TestClusterExtensions` covers the `MemBackend` redeploy
path for that reason. Treat those as part of the abstraction, not as optional
coverage.

**One consequence of rendering rather than replaying: an upgrade moves extension
images too.** The catalog is pinned to the controller build, so re-resolving picks up
whatever the *running controller* pins — the same thing `add_branch_extensions`
already does on any redeploy. `version` is the endpoint tag this API sets, but a roll
also lands the extension-image bumps that came with the controller. Almost certainly
wanted; recorded so it is not discovered.

## 5. `upgrade_endpoints` — the parent task

This is the part that is new to the framework. The fleet roll is **one parent task
that owns N subtasks and runs them itself**: the parent is the only thing the claim
loop can see, the only thing holding a task lease, and the thing whose replay
re-drives the roll.

### 5.1 Shape: a subtask is not a task

Migration `0012` adds a table, and changes nothing about `tasks`:

```sql
CREATE TABLE subtasks (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    parent_id  uuid NOT NULL REFERENCES tasks (id),
    kind       text NOT NULL,   -- a STEP kind: the Registry key, not a task kind
    status     step_status NOT NULL DEFAULT 'pending',
    args       jsonb NOT NULL DEFAULT '{}'::jsonb,
    result     jsonb,
    error      text,
    owner      text,            -- the parent's owner, for the write fence
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX subtasks_by_parent ON subtasks (parent_id);
```

**A subtask is one registered step with durable status, not a task with a step
list.** There is no `subtask_steps`. The step machinery exists to resume a multi-step
task at the right step; a fan-out child is one unit of work and gets nothing from it.
A child that ever needs two phases wants to be a real task, and that is the signal to
make it one.

Three things fall out of the separate table, and they are the reason for it (§5.2):
`tasks` needs no migration and no query change; the runner shrinks to build → lock →
run → record; and children never appear in `GET /tasks`, so no default filter is
needed to keep one roll from flooding every task list in the system.

The parent's single self-contained step `plan_endpoint_upgrades` (args `{version,
endpoint_ids?, max_parallel}`) at Execute time:

1. **Enumerates** targets — the caller's `endpoint_ids`, or every live endpoint — as
   `[]core.EndpointDeploy`, the renderable desired state §4.1 defines. One query, so
   a roll of N endpoints costs one read rather than N.

   **"Every live endpoint" excludes one that was never deployed** — no recorded
   address, the same condition the GC sweeper reclaims a failed-create orphan by.
   `create_endpoint` writes the row *before* deploying, so a failed create leaves a
   row with no pod; rolling it waits out helm's `--wait`, fails, and fail-fast then
   halts the roll. One leaked orphan would otherwise make the whole API unusable
   until GC caught up, with a multi-minute timeout as the only symptom. It also
   excludes a `local`-driver compute, which has no controller-assigned address and
   which a roll could not move anyway. Whole-fleet rule only: an endpoint named in
   `endpoint_ids` is still attempted.
2. **Filters** out endpoints already recorded at the target version — an
   optimization over *desired* state, not a progress signal. See the warning below.
3. **Creates** one `upgrade_endpoint` subtask per remaining endpoint.
4. **Runs** them in-process, up to `max_parallel` at once (§5.4).
5. **Reports** one line per endpoint on the step result: `upgraded`, `skipped` (with
   why), or `failed` (with the subtask id to go and read).

**The filter reads `version`, the running one (§2).** A child that records its target
and then fails its deploy leaves `target_version` ahead of `version`, so the endpoint
still reads as not-at-target and a re-issued roll picks it up. Two rules still hold:

- **Progress within a roll comes from subtask status, not the column.** A reclaimed
  parent re-filtering on state alone cannot tell "already there" from "this roll put
  it there", and would report a roll clean whose children failed.
- **An explicitly named endpoint is never skipped.** The already-at-target filter
  applies only to whole-fleet enumeration, so naming an endpoint re-runs it — the
  operator's remedy after a partial roll, and the case where re-issuing the same call
  would otherwise skip the one endpoint that needs it.

Self-contained for the same reason `delete_cluster` is (`ac-tasks.md` §3): the set is
not known until Execute time, and the worker has no dynamic sub-step expansion.

### 5.2 Who holds which lease, and why the split is structural

| | task lease `task:<id>` | resource lease `endpoint:<id>` |
|---|---|---|
| parent `upgrade_endpoints` (a `tasks` row) | **yes** — taken by the claim loop | no (`fleet:endpoints`, §5.6) |
| child `upgrade_endpoint` (a `subtasks` row) | **impossible** — not in `tasks` | yes, one, while it runs |

**A child is not claimable because it is not in the table the claim loop reads.** The
alternative considered was `tasks.parent_id` plus `AND parent_id IS NULL` in
`PgStore.Candidates`. It was rejected because the failure mode is not the one it
looks like.

If a child were ever claimed while its parent was also running it, **neither the
endpoint lock nor the owner fence would catch it**:

- `PgLocks.AcquireResource` names the lease owner `task=<id> kind=<kind> proc=<proc>`,
  so two runs of the same child in one process compute an **identical owner string**,
  and `pglease` grants unconditionally to the current holder.
- `pglease`'s in-process `reserve()` guard — which exists for exactly this — is
  per-`Client`, and `AcquireResource` constructs a fresh `Client` on every call. It
  never fires.
- The owner fence on every store write compares `tasks.owner`, which is the same
  process both times.

So both runs hold a live `Held` over one row, which is the case `pglease/lease.go:165`
calls load-bearing: one Release deletes the row while the other still reports a live
lease, and a **third** task is then granted `endpoint:<id>` while the first is still
writing. Two concurrent `helm upgrade`s on one release is the visible symptom; a
corrupted lease row is the worse one.

A `WHERE` clause in a Go string literal is not the right custodian of that. A separate
table makes it unrepresentable.

**The table closes the claim path; it does not close the in-process one.** A child is
unclaimable, but nothing stops a parent calling `Run` twice on one child concurrently —
and the way in is ordinary rather than exotic: `Create` returns the same row for a
duplicate (that is what the derived id is for), so a planner that appends every
`Create` result to its work list gets two entries for one row the moment its
enumeration yields an endpoint twice. A repeated id in a caller-supplied
`endpoint_ids` is enough. So `SubtaskRunner` keeps a per-id reservation for the
duration of a `Run` and refuses the second — the `reserve()` guard `pglease` has, at
the layer where it can actually fire, since a fresh `Client` per acquire means its own
never does.

That leaves the lease-count argument, which is the other half of why the layers exist:

| | 867 (children were free-standing tasks) | this design |
|---|---|---|
| leases held by one roll of N endpoints | up to 2N (task + endpoint per child) | **2 + max_parallel** |
| execution slots consumed of `MaxConcurrent` (50) | up to N | **1** |

`pkg/pglease/fleet_perf_test.go` is the measurement behind that first row: renewals
and real work share one pool sized `max(4, NumCPU)`, so at high lease counts the pool
is the bottleneck, delayed renewals follow, and a delayed renewal is what exits the
process. A fleet roll is precisely the workload that would have found that cliff.

The second row is a behaviour change worth naming: **a fleet roll now costs one
execution slot, so it cannot crowd out ordinary work** — but equally, `max_parallel`
is now the only thing bounding a roll's concurrency, since `MaxConcurrent` no longer
applies to its children.

### 5.3 Running a child

Because a subtask is one step, the runner is not a factoring of `Worker.execute` —
it is the inner four lines of it: rebuild the step from `(kind, args)` through the
`Registry`, take the resource lock it declares, run it under the
serialization-abort retry, record the outcome.

```go
// Subtasks runs a parent's children to completion. No task lease is taken: a
// subtask is not claimable (§5.2), so the parent's lease is its liveness.
type Subtasks interface {
    // Create records a subtask (idempotent on id, §5.5).
    Create(ctx context.Context, parentID uuid.UUID, kind string, args json.RawMessage) (*Subtask, error)
    // Run builds, locks, executes and records one subtask under owner.
    Run(ctx context.Context, s *Subtask, owner string) error
}
```

Three things to get right:

- **The child carries the parent's owner.** `subtasks.owner` is stamped with the
  parent's process owner when it starts, so the child's writes get the same fence
  every `tasks` write has. A reclaimed parent re-stamps its children, which fences out
  a previous process still limping along.
- **`Worker.lockFor` is reused as-is, keyed on the SUBTASK id.** The lease owner is
  `resourceOwner(subtaskID, kind, procOwner)`, so "what holds endpoint X?" answers
  with an id that appears in the parent's step result.
- **The one-lock rule becomes a lock ORDER, and that is a real weakening.** Each unit
  of work still takes at most one, but the parent holds `fleet:endpoints` *while* its
  children hold `endpoint:<id>` — nested, which is exactly the shape "a task may hold
  at most one" existed to make unreachable, and which `lockFor` cannot see because a
  child arrives holding nothing. What replaces it: **`fleet` is acquired before
  `endpoint`, and nothing may acquire them in the other order.** Nothing today can
  violate that (no step takes an endpoint lock and then wants a fleet one), but the
  invariant is now a convention rather than a mechanism, so it has to be written down
  — here and on `lockFor` — rather than assumed.
- **The *process* holds `1 + max_parallel + 1` leases from one execution slot** (task,
  children, fleet). That is the accounting in §5.2, not a change to the one-lock rule.

The step catalog is shared: `subtasks.kind` is a **step** kind — the same `Registry`
key `task_steps.kind` uses — so `upgrade_endpoint` is registered once and is
reachable both as a subtask and, if a future planner wants it, as a step of an
ordinary task.

### 5.4 Concurrency and failure

`max_parallel` (default **4**, validated `1 ≤ max_parallel ≤ 50`) is a **bounded
pool, not waves**: a child starts as soon as a slot frees, rather than every child
waiting on the slowest of its wave. Waves buy nothing here — the barrier is what made
867 need `on_failure` to describe halting *at a wave boundary*, and a pool halts more
precisely.

**Fail-fast:** the first child failure stops new children from starting; in-flight
ones finish (killing them mid-`helm --wait` would leave the release in a state the
next roll has to clean up). The parent then fails, and its result names what
succeeded, what failed, and what was never attempted.

Fail-fast is the default **because §7 drops the registry existence probe**. A typo'd
version is the common mistake, and without the probe it surfaces as
`ImagePullBackOff` → helm `--wait` timeout, minutes per endpoint. Stopping after the
first one is what keeps that from costing `N × timeout` and taking a slice of the
fleet down on the way. The two decisions only make sense together — if the probe is
added back later, `on_failure=continue` becomes reasonable to add with it.

**A contended endpoint waits, it does not fail.** A child whose endpoint is held by a
resize blocks in `AcquireResource` up to `--task-lock-wait` (10m) and then proceeds.

**A child that exhausts that wait is `unattempted`, not `failed`,** and fail-fast must
not halt on it. `ErrLockHeld` says only that somebody else holds the endpoint — it is
not evidence the upgrade would fail. Treating it as a failure has a specific bad
outcome: a predecessor process fenced out of a reclaimed parent keeps renewing its
`endpoint:<id>` lease on its own goroutine, finishes the deploy *successfully*, and has
its `Complete` silently no-op on the owner fence; the successor then times out on the
lock and durably records `failed` for an endpoint that is at the target version and
whose row says so. Fail-fast would amplify that into a halted fleet roll. The window is
about one TTL (a process that truly loses a lease exits), but it is narrow rather than
closed.
The parent detects nothing and routes around nothing — that is what moving the lock
off the task-insert path bought. But note the compounding: a child that waits holds
its pool slot while it waits, so a fleet with several contended endpoints serializes
behind them.

### 5.5 Replay and reclaim

The parent's step is idempotent and **converges on recorded state, not on
bookkeeping**. There is no progress cursor.

A process dying mid-roll leaves the parent `running` with children in mixed states.
Then:

1. The parent's lease lapses; `Candidates` offers it (nothing about the claim query
   changed); a surviving instance acquires and re-claims it.
2. The step re-runs from the top: re-enumerate, re-filter — and then reconciles
   against **its own children's status**, which is what actually says what it did.
   It cannot use the version column for that, per the warning below.
3. Children still `pending` or `running` are re-created idempotently, via
   `INSERT ... ON CONFLICT (id) DO NOTHING` returning the existing row — the same
   shape `Store.Create` already uses for a task planned with a preassigned id. This
   works only if a child's id is **derived deterministically from
   `(parent_id, endpoint_id)`** rather than `uuid.New()`, which is the reason to
   derive it.
4. A child left `running` re-runs on the step idempotency contract. Its endpoint lease
   from the dead process lapses within one TTL, and the reclaimed child waits that out.

**A child can never be orphaned**, because the parent only reaches terminal once every
child has, and a non-terminal parent is always reclaimable. With children in their own
table this is the invariant to test directly — an orphaned subtask is work nothing will
ever run, and unlike the claim-loop hazard in §5.2 the schema cannot prevent it.

### 5.6 One roll at a time

Two concurrent fleet rolls have no sensible interleaving. 867 enforced that with a
second partial unique index on `tasks`, which dragged in the
[#400](https://github.com/yugabyte/meko/issues/400) trap — YSQL leaves
`PgError.ConstraintName` empty, so the violation has to be classified structurally,
and the pinned index-inventory test exists to make that safe.

**`pglease` makes that unnecessary.** The parent step declares an ordinary resource
lock:

```go
func (s *planEndpointUpgradesStep) Lock() ResourceLock {
    return ResourceLock{Type: "fleet", ID: "endpoints"}
}
```

`fleet:endpoints` is a third namespace in `resource_token`, disjoint from
`endpoint:<id>` and `task:<id>` like they are from `branch:<id>`. No migration, no
new index, no classifier — and the existing `lockFor` takes it, so the parent still
holds exactly one lock and the children still hold one each.

**This changes the contract from refuse to queue:** a second roll waits out the first
rather than getting a `409`. That is better for the idempotent case — the second roll
re-enumerates, finds the fleet already converged, and does almost nothing — and worse
for the case where the two rolls name *different* target versions, which now flips
the fleet twice instead of refusing. **Open decision (§10).**

## 6. API

```
POST /api/v1/endpoint-upgrades
```

Top-level, because the operation is fleet-scoped and every existing endpoint route is
nested under pool/cluster/branch. Kebab-case noun, matching `/storage-pools` and
`/migrations/connectivity-checks`; the API spells verbs only as sub-resources
(`/endpoints/{id}/resize`), and there is no resource to hang this one off.

| field | default | meaning |
|---|---|---|
| `version` (required) | — | target version; today, the endpoint image tag |
| `endpoint_ids` | — | explicit subset; **absent = the whole fleet** |
| `max_parallel` | `4` | endpoints rolled at once (§5.4) |

`202` + the `Task` + `Location`, like every other mutation. `400` for a missing
version or an out-of-range `max_parallel`; `501` with no deployer.

**The handler checks only what is true of the request** — the version is present, the
knobs are in range. Whether a given endpoint can move is the planner's, answered once
at Execute time when it is still true. Answering it in the handler too would be N
reads of a fleet that moves in between. So an id that no longer exists, and one
already at the target, are `skipped` lines in the task result — a roll is never
refused because one of its endpoints cannot move.

Read side: `Endpoint` carries `version`, **absent when unknown** rather than an empty
string, so "we do not know" and "it runs a version named by the empty string" stay
distinguishable. This is the hook QA asked for on #455 — a test can assert the upgrade
*converged* rather than inferring it from pod images. It landed with §2–3 rather than
here, because it is what lets an end-to-end test see the recorded column at all.

Drilling into a roll is `GET /tasks/{taskId}/subtasks` — one row per endpoint with its
status, result and error. `GET /tasks` needs no change at all: subtasks are not tasks,
so a fleet roll adds exactly one row to it however many endpoints it touches.

The MCP server is a mechanical wrapper of the REST surface; adding the tool is a
follow-up, not a design question.

## 7. What 867 had that this drops

867 was a much larger diff. Deliberately not in this pass:

| dropped | why | cost of dropping |
|---|---|---|
| `upgrade.Policy` + `SemverPolicy` | version ordering is a real feature, but it is a guard against rolling *backwards*, not something the first pass needs to express | nothing stops a downgrade. Acceptable dev-facing; the `Policy` seam goes back in unchanged when it matters |
| the OCI registry existence probe | it is a network dependency, a credentials problem (a private repo answers 401 anonymously), and its own package | a typo'd version fails at `helm --wait`, minutes in. §5.4's fail-fast is the compensation |
| `force` | only meaningful against a `Policy` verdict | — |
| `skip_version_check` | only meaningful against the probe | — |
| `on_failure` | the pool + fail-fast replaces what it selected between (§5.4) | a partial roll must be re-run rather than pushed through |
| the fleet-upgrade unique index | replaced by `fleet:endpoints` (§5.6) | contract changes from refuse to queue |

What carries over essentially unchanged: §2 (the opaque version column, NULL-means-
unknown, no backfill), §3 (the deploy spec override and the single mapping point),
and §4's persist-before-deploy ordering.

**867 was never reviewed** — the review bot declined it at 3011 changed lines against
a 3000-line limit, and it was closed when the locking foundation changed under it, not
for defects (its migrations applied and the itest-dev smoke test passed 32/32). So
none of the above carries a reviewer's endorsement; it carries only that it was
written down and nothing has since contradicted it.

That size is itself a design input. **This should land as a stack, not one PR:**

1. **The spec assembler** (§4.1) — extract `deploy.EndpointSpecFrom` and the branch
   extension resolver, convert `create_endpoint` and `add_branch_extensions` onto
   them. Pure refactor, no behaviour change, and it makes every PR after it smaller.
2. **The version column and deploy-spec override** (§2–3).
3. **`upgrade_endpoint`** (§4) — the per-endpoint step, usable on its own.
4. **The `subtasks` table and runner** (§5) — migration `0012`; the numbers are
   assigned in stack order, since two migrations claiming one version wedges the
   controller's startup.
5. **The API** (§6).

Each is reviewable alone, and 1–3 are useful without the rest. Converting both
existing callers in step 1 is not optional: a shared assembler with one caller still
hand-rolling a spec literal is worse than either end state.

## 8. Testing

Unit, against `MemStore`, `MemLocks` and the existing fakes:

- **`deploy.EndpointSpecFrom`** (§4.1) — every field rendered from one
  `core.EndpointDeploy`; `resolveBranchExtensions`'s own tests cover the catalog
  mapping, and `add_branch_extensions`' cover its one-read-per-roll.
  **Plus an assertion per caller** on the spec the deployer actually received: the
  assembler's own test passes on a fully-populated record regardless of what the step
  put there, so it does not pin the caller boundary and cannot be the only test.
- **`upgrade_endpoint`** — happy path, idempotent replay, the spec carrying the
  version override, persist-before-deploy ordering.
- **the parent step** — whole-fleet and subset enumeration, endpoints already at the
  target skipped, `max_parallel` respected, fail-fast stopping new starts while
  in-flight children finish, and the per-endpoint result shape.
- **replay** — a parent re-run after a simulated crash creates no duplicate children
  (the derived-id property, §5.5) and skips endpoints whose children succeeded.
- **the orphan invariant** — a parent never reaches terminal with a non-terminal
  child. The schema rules out the §5.2 hazard, but not this one, so it is the
  invariant that has to be tested rather than designed away.
- **the handler** — the `400`s and `max_parallel` bounds.

**The two real deployers' `CreateEndpoint` bodies are not unit-testable**, and that
is where the version is consumed and reported back. Both shell out (`exec.Command` to
`docker` / `helm`) with no runner seam, so `values()` and `endpointImage()` are pinned
as pure functions while the hop from those into the returned `deploy.Endpoint` is not.
It matters because `create_endpoint` treats an empty reported version as "no
information" and skips the write — a deployer that stopped reporting would silently
never record a version, with the suite green. Until a runner seam exists, a real
deploy is the only thing that covers it, so an end-to-end check that the `version`
column is populated after a create belongs in whatever exercises a live cluster.

Live-database (`store_pg_test.go` / `locks_pg_test.go`, `AMP_TEST_DATABASE_URL`):
`TestMigrateRoundTrip` with 0012; the subtask store's idempotent create against real
SQL; and `fleet:endpoints` serializing two parents, at a short lease as the other
`TestPg…` lock tests do.

Integration: extend the upgrade scenario QA added under #455 to assert the endpoint's
reported `version` after a roll, rather than recording the pod roll as a property.

## 9. Deliberately not in this slice

- **PG major-version upgrade.** This moves the endpoint's *binaries*, not the
  Postgres catalog version. `pg_version` is untouched.
- **Image repository changes** — the repo stays deployer config (§2).
- **Zero-downtime upgrade** — §1: the pod is replaced. Making that seamless is a
  separate problem from choosing what version to run.
- **Per-cluster pinning, maintenance windows, canary, auto-upgrade on release.** All
  are different planners over the same `upgrade_endpoint` child, which is the reason
  the child is a task rather than a loop body.

## 10. Decisions

Settled in review, recorded so the reasoning is not re-litigated:

- **Subtasks are their own table**, not `tasks.parent_id` + a claim-query predicate
  (§5.2). The predicate would have been the only thing preventing a double-`helm`
  and a corrupted lease row, and neither the endpoint lock nor the owner fence backs
  it up.
- **A subtask is one step**, with no `subtask_steps` table (§5.1). A child needing two
  phases is the signal to make it a real task.
- **The route is `/endpoint-upgrades`** — kebab noun, matching the rest of the API.
- **`Subtasks` is a framework concept**, not upgrade-local (§5.3), because "different
  planners over the same child" (§9) is the reason for the shape.
- **The registry existence probe stays dropped** for this slice; a typo costs one
  failed endpoint and a stopped roll (§5.4). Tracked as future work:
  [meko#982](https://github.com/yugabyte/meko/issues/982).
- **Spec assembly is one function taking `core.EndpointDeploy`**, not a function that
  reads by endpoint id and not a package of its own (§4.1). Reading would be N+1 on a
  fleet roll; a package would knot the imports for three callers that all live in
  `internal/task`. Promote it if a fourth caller appears outside that package.

Settled while building §5–6, because the design did not say:

- **A failed roll's per-endpoint account is `GET /tasks/{taskId}/subtasks`, not the
  step result.** The worker records a failed step's *cause*, not its output, so the
  `endpoints` report only survives on the success path. The parent's error carries the
  tally and the first failure; the children's own rows carry the rest. The
  alternatives were persisting the output of a failed step (a change to the task
  framework, for one caller) or returning `Ok` on a partial roll, which would report a
  failed roll as succeeded.
- **`unattempted` fails the roll; it just does not halt it.** `ErrLockHeld` is not
  evidence the upgrade would fail (§5.4), so it never stops new children starting —
  but a roll that left an endpoint unattempted did not converge the fleet, and
  `succeeded` has to keep meaning "every targeted endpoint is at the target". The
  operator re-runs it; the second roll skips what the first did.
- **A fan-out step reads its task id and owner off the context**, stamped by the
  worker, rather than out of its args. The owner is per-execution — a reclaimed parent
  must stamp its children with the NEW one, which is what fences out a predecessor —
  so it cannot be planned into args at all.

Still open:

1. **`fleet:endpoints` queues rather than refuses (§5.6).** Queue is better for the
   idempotent re-run — the common case is re-firing a roll that half-failed — and
   worse for two rolls naming *different* versions, which now rolls the fleet twice
   instead of returning a `409`. Cheap mitigation if wanted: after acquiring, the
   parent re-reads its own `params` and no-ops if a newer roll superseded it.
   Recommendation: ship the queue, revisit if it bites. **Shipped as the queue.**

2. **A failed-create endpoint is in the fleet.** "Every live endpoint" is
   `eb.dropped_at IS NULL`, which includes an endpoint whose `create_endpoint` task
   failed before its pod came up (`address IS NULL` — `core/orphans.go` already tracks
   exactly these for the GC sweeper). A whole-fleet roll will try to upgrade one, wait
   out helm's `--wait`, fail, and fail-fast then halts the roll — so one orphan stalls
   every roll until GC reaps it. Not excluded here: the enumeration is deliberately one
   read of one projection, the sweeper is what reclaims those rows, and excluding them
   is a policy the design did not state. Revisit if a stalled roll traces back to one.
