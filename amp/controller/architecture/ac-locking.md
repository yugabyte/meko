# ac — Task and endpoint locking

**Status:** Draft
**Last updated:** 2026-08-27

How the controller decides who runs a task and who may touch an endpoint, once
more than one controller process exists. Both answers are leases from
[`pkg/pglease`](../pkg/pglease/README.md) ([meko#920](https://github.com/yugabyte/meko/issues/920)) over the
`amp_registry.resource_token` table migration 0006 already owns.

The locks are in `internal/task` — `locks.go` (the ports and `PgLocks`),
`memlocks.go` (the in-memory model the unit tests run against). The **resize
contract has not moved**: a second concurrent resize is still refused at plan time
with `409` by `tasks_one_active_resize_per_endpoint`, and turning that into a
`202` that waits is the remaining piece of
[[meko#839](https://github.com/yugabyte/meko/issues/839)](https://github.com/yugabyte/meko/issues/839).

| Lock | Key | Held by | Excludes |
|---|---|---|---|
| **task execution** | `task:<task-id>` | the controller *process* | a second controller instance executing the same task |
| **endpoint** | `endpoint:<endpoint-id>` | the *unit of work* — a task, or one of a parent's subtasks | anything else blocking on that endpoint, whichever process owns it |
| **endpoint fleet** | `fleet:endpoints` | a fleet roll's parent task — `plan_endpoint_upgrades` or `plan_endpoint_rerenders` | a second fleet roll **of either kind**, which is **refused** rather than made to wait (the one exception to "contention waits") |

No new table and no new migration: the compute write fence keys the same table as
`branch:<id>`, so the four namespaces coexist. Sharing 0006 is also what keeps
one definition of the rule — `token_acquire` decides every grant, for these locks
and for the Rust write-fence client both.

## Scope: the task-level gate, not multi-region

Running the controller multi-region is a great deal more than locking. Leader
election for the singletons (the GC sweeper, [meko#444](https://github.com/yugabyte/meko/issues/444)), request routing, backing-DB
topology, and the clock assumptions that go with all of it are **out of scope**.
What is in scope is the gate at the task level: no two instances execute one task,
and no two blocking tasks touch one endpoint.

That gate has to exist before the rest is worth attempting, and it earns its keep
at one instance anyway — the endpoint lock is what [meko#809](https://github.com/yugabyte/meko/issues/809)'s upgrade work needs in
order to ask "is this endpoint busy?" and wait.

## The resize index, which is still there

`tasks_one_active_resize_per_endpoint` (a partial unique index on `tasks
(target_ref)` for non-terminal `resize_endpoint` tasks,
`migrations/0001_task_log.up.sql`) is what still refuses a second concurrent
resize at plan time.

The endpoint lock sits beside it rather than extending it, because it is not an
endpoint lock: it is uniqueness on one task kind, so nothing in it stops
`delete_endpoint` or a second `create_endpoint` racing a resize, each new
endpoint-mutating kind would cost a migration and a classifier change, and there
is no way to *ask* — acquire is "attempt the insert and interpret SQLSTATE 23505".

## Lock 1 — task execution

The lease is the authority for "who is running this task".

### `tasks.lease_expiry` is deprecated, not dropped

The lease row already holds an expiry, on the database's own clock, so a column
shadowing it would have to be re-stamped on every renewal to stay true — a write
per task per interval buying nothing, and a column that reads as authoritative
while the real answer lives elsewhere. Nothing reads or writes it now.

It is left in the schema rather than migrated away: dropping it would make every
fresh install create the column and then lose it. Removing it belongs to a schema
tidy-up ([meko#975](https://github.com/yugabyte/meko/issues/975)).

**A downgrade to a pre-lease controller strands in-flight tasks.** The old claim
query selected `status = 'running' AND lease_expiry < now()`, and `NULL < now()` is
NULL — so a task claimed under a lease has no expiry and an older binary never
offers it again. One statement recovers it, and the runbook carries it:
`UPDATE tasks SET lease_expiry = now() WHERE status = 'running'`.

### `tasks.owner` stays, and carries a nonce

It is stamped once at claim and never refreshed, so it is a fact rather than a
cache: which process claimed the task. Every task and step mutation keeps its
`AND owner = $currentOwner` fence.

It is the `pglease` owner string, so the fence and the lease name the same thing.
`worker.ownerID()` delegates to `pglease.NewOwner("task-worker")` for the reason
that function exists: a lease is granted unconditionally to its current holder, so
the old `host/pid` spelling made a restarted pod that recycled a pid *the same
owner*, and it would have been handed a lease it never held. The nonce makes it a
different one.

### Claiming is acquire-then-transition

A grant **is** the claim. The transition follows it, so a task never reaches
`running` unowned; the reverse order leaves a `running` task with no holder and
nothing to reclaim it.

Reclaimability is read from the lease rather than from a column, and only for
`running` rows — `pending` is the common path and needs no lookup:

```sql
SELECT id, ... FROM tasks
WHERE status = 'pending'
   OR (status = 'running'
       AND NOT EXISTS (SELECT 1 FROM amp_registry.token_holder('task:'||id::text, $skew)))
ORDER BY created_at
LIMIT $k
```

`token_holder` reports a row for as long as takeover is blocked, so "no row"
already means *free to a new owner*: this reuses 0006's definition instead of
restating it, and it is a point lookup on a hash primary key. It stays a **hint**
regardless — the acquire is what decides, so a candidate whose read raced a
takeover is refused and the loop moves on.

**`SKIP LOCKED` stops being load-bearing, and something has to replace what it
did.** Two instances racing one candidate is settled by the lease rather than by a
row lock, which is the part that survives instances not sharing a tablet's leader.
But `SKIP LOCKED` was also spreading N workers across N different rows, and
without it every instance picks the same oldest pending task and all but one waste
an acquire. Hence `LIMIT $k`: the claim takes a small candidate *set* and tries
them in order until one is granted. Holding a row lock across the acquire is not
the alternative — that is the cross-statement distributed lock this is getting
away from.

**A denied task lock is not waited on.** A denial means another instance is
already running that task; there is nothing to wait for, so the loop tries the
next candidate. This is the opposite of the endpoint lock, and the distinction is
the point: a contended endpoint is a queue you join, a contended task is work
somebody else is already doing.

### Renewal and loss come from `pglease`

The task runs under the worker's own context, and `pglease` renews in the
background, so there is no heartbeat of the controller's own. Nothing watches the
lease either: losing one exits the process, so a lease that ends mid-task is not a
state surviving code can observe. Watching it would buy a cancellation
microseconds before `os.Exit` — which stops no in-flight helm call, and is the
reason process death was chosen over cancellation to begin with.
`--task-lease-ttl` and `--task-lease-renew-interval` map onto `Config.TTL` and
`Config.RenewEvery` (30s against 10s validates against the 2s default safety
margin).

## Lock 2 — endpoint

A step declares the resource it needs; the worker acquires it before running that
step and holds it until the task leaves execution — not just for the one step,
because a later step of the same task may still be operating on the resource.

```go
type Locking interface{ Lock() ResourceLock }
```

`create_endpoint`, `resize_endpoint`, `upgrade_endpoint` and `delete_endpoint`
declare `endpoint:<endpoint-id>`. The declarations sit together in `locks.go`
rather than one per step file, so what serializes against what is answerable from
one place.

**One lock per unit of work.** Deadlock needs one holding two, and refusing the
second is what keeps that unreachable without a lock manager to detect one.

The unit is not always a task. A fleet roll's parent holds `fleet:endpoints` and
runs children that each take `endpoint:<id>`, so the process does hold two — nested,
one per unit of work, which `lockFor` cannot see because a child arrives holding
nothing. What keeps that safe is an ORDER: `fleet` before `endpoint`, never the
reverse.

That leaves the fan-out steps unprotected, because they need every endpoint under a
branch or cluster and one lock cannot express that: `delete_branch` and
`delete_cluster` retire them all, and **`add_branch_extensions` redeploys them all
through the same deployer create and resize use** — so it can run `helm upgrade`
against a release a locked single-endpoint task is mid-way through. Covering these
needs intent locks on the branch and cluster above the endpoints:
[meko#976](https://github.com/yugabyte/meko/issues/976).

With `upgrade_endpoint` that gap can also make a row lie: a fan-out redeploying from
a pre-upgrade snapshot puts the endpoint back on the old version while
`endpoints.version` reads the new one, and both tasks succeed, so no replay runs.

**The owner names the unit of work, not the process.** `pglease` grants
unconditionally to the current holder, so two units in one process sharing a
process-scoped owner id would both be granted, and mutual exclusion would rest on
the library's in-process `reserve()` guard rather than on the database. Naming the
unit — a task, or one of a parent's subtasks — makes one rule cover same-process
and cross-process contention identically, and makes the
`Holder` probe answer "what holds endpoint X, and what kind is it" — the
debuggability a `reason` column would otherwise be for, which matters because
`resource_token` has no metadata column and the owner string is the only place to
put it.

In practice: one process-scoped `pglease.Client` for task leases, and a fresh one
per endpoint *acquire* — not per task. The distinction matters, because pglease's
in-process `reserve` guard is per-Client, so it does not apply to endpoint leases at
all. Two different tasks are excluded by the database (different owner strings);
what stops one task acquiring the same endpoint twice is the worker holding a single
lock per execution.

**Contention waits**, bounded by `--task-lock-wait` (10m). `AcquireWait` sleeps
the denial's `RetryAfter`, floored at 250ms; a task that exhausts the bound fails
cleanly rather than hanging, so a lock nothing ever releases costs one task
instead of every task queued behind it. A wait longer than a second is logged with
the lock and the elapsed time.

**`fleet:endpoints` is the exception, and it is the only one.** It sets
`ResourceLock.NoWait`, so `AcquireResource` takes pglease's single-attempt
`Acquire` instead of `AcquireWait` and a second fleet roll is refused rather than
queued (Lock 3 for why that resource is different). `NoWait` lives on the lock
rather than on the acquire, so what waits and what refuses is decided per
resource, once, in `locks.go` — the endpoint lock cannot pick it up by accident.

**What `NoWait` refuses is another HOLDER, not a held row**, and `noWaitDenial` is
where that is decided. The owner string names the unit of work, so a denial whose
`task=` field is the id we are acquiring *for* is this holder's own row, left by a
predecessor process — waited out on the ordinary bounded path, because refusing it
would fail a reclaimed task against its own corpse (Lock 3). Any other id, or a
holder the statement could not see, is refused. The classification comes first:
only `pglease.ErrNotAcquired` / `ErrAlreadyHeld` are contention, and anything else
— a dead pool, an expired context, a `token_acquire` RAISE — gets the neutral
wrapper the queueing path uses rather than a sentence about fleet upgrades.

## Lock 3 — the endpoint fleet

`plan_endpoint_upgrades` and `plan_endpoint_rerenders` both declare
`fleet:endpoints`: a third namespace in `resource_token`, disjoint from
`endpoint:<id>` and `task:<id>` as they are from the compute fence's
`branch:<id>`. No migration, no index, no classifier — the existing `lockFor`
takes it like any other resource lock.

Two declarers, one lease, and the lease is `NoWait`: the second roll's step
fails rather than queueing. But that failure happens in the worker, so the
refusal a caller should see has to come earlier — `Service.UpgradeEndpoints` and
`Service.RerenderEndpoints` both call `noFleetRollInFlight` at plan time and
return `ErrFleetRollInProgress` (the API's 409) for either kind.

It is what makes it **one fleet roll at a time**, and it is `NoWait`: a second
roll is **refused**, not queued. Two rolls redeploying one endpoint at once is
what the endpoint lock already stops; what queueing would add is a roll firing
long after its caller asked for it, against a fleet that has since moved, with
the caller told `202` and finding out afterwards.

This lease is the **authority** on that; the `409` the API returns comes from a
plan-time query over `tasks` for a non-terminal `upgrade_endpoints` task, which is
racy on its own (and is a query rather than a probe of this lease precisely
because a `pending` roll holds no lease yet). A roll that wins the race at plan
time and loses it here fails naming the roll that beat it, rather than with the
generic lock-held wording — an operator must not read it as their endpoint being
busy. [`ac-endpoint-upgrade.md`](ac-endpoint-upgrade.md) §5.6.

**A roll reclaiming its own crashed execution is not a second roll**, and this is
the one thing `NoWait` has to wait for. The task lease is what makes the roll
claimable; `fleet:endpoints` is a separate row on its own renewal, so it can still
be live when the successor re-enters the step. Refusing there fails the roll
durably against its own dead predecessor and strands the subtasks that execution
created. The holder's `task=` field is what tells the two apart — see the `NoWait`
paragraph under Lock 2.

It does **not** close the meko#976 gap: it serializes rolls against each other,
not a roll against a cascade delete or `add_branch_extensions`.

## Losing a lease kills the process

All three locks keep `pglease`'s default `AbortProcess`: cause and goroutine dump
straight to stderr, `os.Exit(66)`, no deferred cleanup.

The alternative was to cancel only the affected task and keep the other
`--task-max-concurrent` (50) running. It is rejected because a lease is only lost
after `TTL - SafetyMargin` with no confirmed renewal, which means the backing
database has been unreachable for most of half a minute. The other tasks are not
healthy in that window either — their own writes are failing — so keeping the
process alive preserves the appearance of progress rather than progress.

It also makes the endpoint lock's fence real rather than advisory. A cancelled
context does not stop a helm call already in flight; process death does. What
survives is a mutation the Kubernetes API server has already accepted, which is a
far narrower window than a step running on under a lease it has lost.

**Operationally** this is a restart, and on a sustained outage a restart loop —
visible as `CrashLoopBackOff` rather than as a controller that is up and silently
doing nothing. Exit 66 is the compute fence's code too, so one supervisor rule and
one log scan recognise "fenced out" across both
([`compute-fencing.md`](../../compute/architecture/compute-fencing.md)).

## Sequences

### A new task

1. An API handler plans the task. `Store.Create` writes the `task` row
   (`pending`, null owner) and its `task_step` rows in one transaction. Planning
   takes no lease: the task is durable intent, and nobody owns it.
2. The claim loop takes an execution slot from the semaphore **before** claiming,
   so it never holds a lease on a task it cannot immediately start.
3. It runs the candidate query above — a plain read, no row locks, no state
   change — and gets up to `k` candidates, oldest first.
4. It walks them calling `Acquire("task:<id>")`:
   - **granted** — we own it; stop walking;
   - **`*Denial`** — another instance has it; try the next candidate;
   - **`ErrAlreadyHeld`** — this process already runs it (the read raced our own
     transition); skip it;
   - **all denied** — nothing claimable: release the slot and idle one poll
     interval.
5. Only now does the state change:
   `UPDATE tasks SET status='running', owner=$owner, updated_at=now() WHERE id=$id
   AND status IN ('pending','running')`. Zero rows means the task went terminal or
   was deleted between the read and now, so release the lease and move on.
6. The step loop runs under the worker's own context; `pglease` renews in the
   background and nothing here watches the lease (see above — losing one exits the
   process, so there is no in-process recovery to run).
7. At terminal the owner-fenced `CompleteTask` lands, then `held.Release(ctx)`
   drops the row. Release skips the skew grace deliberately, so the key is free
   immediately rather than after a TTL.

`defer held.Release(ctx)` is the idiom, and it is safe: Release is idempotent, and
on a *lost* lease it deliberately declines to delete the row — by then the row may
belong to a successor.

### Restart and reclaim

**Graceful shutdown needs no special handling.** The `defer held.Release(ctx)`
above sits in the per-task goroutine, so a task that unwinds within
`ShutdownGrace` releases its own lease and is immediately reclaimable elsewhere. A
task still stuck when the process exits simply does not release, and its lease
expires normally. There is no shutdown-time lease sweep to write.

**A crash or a fenced-out exit releases nothing.** The row sits in
`resource_token` until `token + skew <= now`, so up to one TTL after the last
successful renewal. Then:

1. A surviving or restarted instance runs its candidate query. The dead task is
   `running`, `token_holder` now returns no row for it, and it enters the
   candidate set.
2. `Acquire` succeeds as a **takeover** rather than a renewal, because the nonce
   makes the restarted process a different owner. This is the case the old
   `host/pid` spelling got wrong.
3. The transition re-stamps `tasks.owner`, which is what fences out the previous
   process's writes if it is somehow still alive.
4. `Worker.execute` proceeds unchanged: load the step records in `step_ids` order,
   skip the `succeeded` ones, and re-run the one left `running` on the step
   idempotency contract.

**Reclaim latency after a fenced-out exit is up to one full TTL, for every
in-flight task** — not only the one whose lease lapsed. `AbortProcess` fires on
the first lease to hit its deadline while the other 49 may have renewed more
recently, so their rows still carry most of a TTL. Releasing on the way out is not
an option: the process is dying because the database is unreachable, so the
release would fail anyway, and writing on the way out is precisely what a fenced
process must not do. This is the same 30s as today's `--task-lease-ttl`, but
fail-fast makes it the normal recovery path rather than a rare one, which is the
argument against raising the TTL.

## Still to do: resize returns 202, not 409

The resize contract has not moved. A second concurrent resize is still refused at
plan time, and `409` is still the answer. The endpoint lock runs *underneath* that
today: it is what stops a resize racing a create or a delete, which the index never
covered.

What did move is that the plan-time refusal is no longer resize-only.
`add_branch_extensions` needs the same lock — two concurrent adds redeploy
overlapping endpoint sets, which is concurrent helm upgrades of the same release
([`ac-ycp.md`](ac-ycp.md)) — so the mechanism is now per-kind:
`Kind.lockIndex` names the partial unique index and `Kind.inProgress` the conflict
it reports, `taskLockViolation` (formerly `isResizeLockViolation`) classifies by
kind, and `TestPgUniqueConstraintsOnTask` derives the expected index set from
`allKinds` instead of pinning a literal.

Turning the refusal into a `202` plus a task that waits is still the rest of
[meko#839](https://github.com/yugabyte/meko/issues/839). Acquiring at execution
time means there is nothing to refuse at plan time, so the 409 stops being the only
safe answer — it existed because a second resize would otherwise race. When that
lands, both indexes, the classifier and the per-kind conflicts go with it, which is
what finally removes the [meko#400](https://github.com/yugabyte/meko/issues/400)
trap; until then it is contained by classifying on the kind, whose index predicates
are disjoint. See [`ac-apis.md`](ac-apis.md).

## Testing

The unit tests run against `MemLocks`, which models the decision rule — the holder
is granted unconditionally, a different owner only once the row is free — but
**not the clock**: a row is held until released or until the test says the holder
is gone. A test that has to outwait a real lease is a slow test that proves
nothing about the SQL.

The clock is covered where it lives, against a real YugabyteDB (`locks_pg_test.go`,
the `TestPg…` gate): two instances contending for one task, a crashed controller
still holding its task until the lease lapses and a survivor picking it up only
then, a graceful restart handing over at once instead of costing a TTL, and an
endpoint lock outliving a controller restart with the tasks either side of it
never overlapping. Those run at a 5s lease rather than the production 30s — what
is asserted is that the wait *is* the lease, not that it is any particular number
of seconds.

Every test that takes a lease must give it back before its pool closes. A live
lease whose renewals start failing is a lost lease, and losing one exits the
process — which in a test binary takes the whole run with it.

## Open points

- `k` in the candidate query (`claimCandidates`, 10) is unsized. It only has to
  exceed the number of instances plausibly colliding on the oldest tasks.
- The endpoint lock is not surfaced on the API: a task waiting on one reports
  nothing to a poller until it moves. That arrives with the 202 contract.
