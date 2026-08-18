# Storage-layer control (in the controller)

The Go `controller/` owns cluster control for the storage layer, **in-process** —
there is no separate control-plane binary. This doc describes what it owns, the
schema it uses, and the HTTP surface it serves.

## What it owns

- ~~Fencing authority~~ — **not the controller's.** Write fencing is a lease the
  writing endpoint takes for itself, in `amp_registry.resource_token`
  (see [`compute-fencing.md`](../../compute/architecture/compute-fencing.md)).
  The controller neither issues it nor arbitrates it, so there is no second copy
  of the fencing state to keep in step and no round trip to issue one.
- **Branch and endpoint bookkeeping** — branch records + name mappings, seeding
  the per-branch state a pageserver needs to attach, and the launcher's durable
  endpoint records. Not a registry of its own: these are the control plane's own
  `branch` and `endpoint` rows, read and written in the storage layer's rendering.
- **Provisioning** — the whole `yb_storage` database. The controller's
  golang-migrate migrations create the whole `amp_storage` schema — blobs+enum,
  the page-store tables, the WAL mirror, and both per-branch state tables (the
  schema the compute daemons bind at startup — they create none of their own) —
  plus the control-plane schemas below.
- **Branch-drop GC** — reclaiming a dropped branch's data out of `amp_storage`,
  below.

## Schema

There is **one** `branch` table and **one** `endpoint` table, in the
`amp_registry` schema of the shared `yb_storage` DB, provisioned by
`controller/migrations/0002_resource_model.up.sql`. The product API and this surface are two
renderings of those same rows, not two copies:

| | product API | this surface |
|---|---|---|
| branch key | `(cluster_id uuid, branch_id uuid)` | flat 32-lowercase-hex `branch_id` |
| endpoint key | `id uuid` | caller-chosen handle in `endpoint.name` |

The hex form is `replace(branch_id::text, '-', '')` — the same 128 bits, so the
translation is lossless and total and there is nothing to keep in step. It is a
wire rendering only: the `amp_storage` tables key on the `uuid` itself. A branch
dropped through either surface is the same row with the same `dropped_at` stamp.

Two companion schemas are fixed rather than configurable: `controller` holds the
write-ahead task log, and `amp_storage` holds the data plane. `--db-schema`
(default `amp_registry`) relocates only the resource model.

Because the rows are cluster-scoped, so is this surface — see the routes below.
`yb_amp_local` provisions its user/pool/cluster through `/api/v1` at startup.

Dropped vs upstream: the heartbeat/availability loop, the scheduler, tenant
placement/sharding, the `/v1` pageserver passthrough, timeline imports, and
metadata health — none are needed in the per-branch topology.

## HTTP surface

There isn't one. Everything the storage layer's control functions touch —
branches, endpoints — is an `/api/v1` resource, created through the same routes
any other client uses. `controller/internal/registry` is what remains of the
surface: an in-process store, no handlers.

Readiness is `GET /readyz` on the API listener (`:8080`), alongside `/healthz`
and `/metrics`. In dev/local the controller is unauthenticated (the whole
controller is, today — auth is deferred, #189).

## Callers

- **`yb_amp_local`** (the dev harness, `controller/local/`) launches the
  `controller` binary in place of the old subprocess and drives `/api/v1` over
  HTTP via `controller/local/client` — its user and pool, its clusters, its
  branches, its endpoints. It starts the controller with
  `--deploy-driver=local`, so endpoint creates keep the record and launch
  nothing; the launcher spawns `compute-ctl` itself, and what it needs to do
  that rides on the endpoint row — its handle as `name`, its GUCs as
  `pg_settings`, its ports as the opaque `extended_config`. The integration
  harness (`test/integration/compute/`) exercises the same path.
- **Endpoint pods** (Kubernetes/Docker deploy) make **no** calls to the
  controller. They are handed `BRANCH_ID` and `CONTROLLER_URL` and need neither:
  the branch's `amp_storage.branch_state` row is seeded before the pod starts,
  and the pod's supervisor takes the branch's write lease itself.
- **The controller's own task framework** seeds branch state through the same store
  **in-process** (not over HTTP) — the `RecipeStore` port in
  `controller/internal/task` is satisfied by an in-process adapter, and it passes
  the cluster id along with the branch id because the row it writes is
  cluster-scoped.

## Branch-drop GC

Reclaiming a dropped branch's `amp_storage` rows is the controller's, because a
dropped branch's pageserver is terminated with it — so nothing would be left to
drain a queued GC candidate. What the flow is, and why `branch_state` goes first,
is documented with the rest of GC in
[`compute/architecture/pageserver-gc.md`](../../compute/architecture/pageserver-gc.md#branch-drop-gc);
the operator's triage query is in
[`runbooks/controller_runbook.md`](../../runbooks/controller_runbook.md). What is
controller-shaped, and lives here:

**Shape.** A `Reaper` goroutine (`internal/branchgc`) beside the task worker in
`cmd/controller`, on the same shutdown context. A ticker rather than a task,
because the task framework has neither retries nor a `run_after` column and so
cannot express periodic work. No leader election and no in-progress marker: the
deletes are idempotent per branch and `gc_completed_at` is a conditional UPDATE, so
every replica running a reaper is safe (concurrent reaps waste work; they cannot
corrupt).

**Two columns on `amp_registry.branches`** (`0002_resource_model.up.sql`): it is the
only per-branch record that survives a reap, it already carries `dropped_at`, and
the `branches_gc_due` partial index over the two covers only unreclaimed tombstones
(leading column explicitly `ASC` — YSQL's default HASH cannot seek the
`now() >= reap_after` range).

| column | written by | meaning |
|---|---|---|
| `reap_after` | the reaper, as `dropped_at + retention` | the earliest the data may be deleted |
| `gc_completed_at` | the reaper, after the last delete | the data is gone; the row is kept as the record that the id existed |

The drop writes `dropped_at` alone; the reaper stamps `reap_after` each tick for
every tombstone still missing one, so a tombstone from any path is picked up rather
than stranded as never-due. Deriving it from the stored `dropped_at` rather than
that tick's `now()` is what makes it a **snapshot** — a later change to
`--branch-gc-retention` cannot move a deadline already written, which is what keeps
this predicate and a future un-drop's strictly complementary.

**Each tick** selects the due branches
(`dropped_at IS NOT NULL AND gc_completed_at IS NULL AND now() >= reap_after`),
then deletes each one's rows in bounded batches and marks it complete. The scan is
capped at an internal `maxBranchesPerTick`, which bounds its result set rather than
the tick's work — the row budget below does that.

The scan also refuses any branch with a **live child fork** or a **surviving
endpoint**: a child reads its pre-fork history under this branch's id, and an
endpoint is a running compute. Neither can happen — the drop path refuses such a
branch and the fork / endpoint-create interlocks close the races — but the
predicate carries them anyway, because deleting is the only irreversible step
here. Filtering rather than skipping also means such a branch cannot occupy the
scan's window, which it otherwise would forever: it is permanently due and
permanently among the oldest.

A batch is a **key-only SELECT that yields `ybctid`s, then one
`DELETE ... WHERE ybctid = ANY(...)`**. `ybctid` is the DocDB row key, so the
delete is a direct point lookup per row and scans nothing itself.

The SELECT resumes from a **cursor**, which is what keeps a reap linear. Without
one, every batch restarts at the beginning of the branch's key range and
re-traverses the tombstones the previous batches left — measured on a live cluster
at 33 ms with nothing ahead of it, 615 ms behind 80k tombstones and 1015 ms behind
95k, while the cursored form stays flat at ~33 ms. (`Storage Rows Scanned` reads
the same in both: it counts live rows returned and hides tombstone traversal, so
only execution time shows this.)

Which cursor shape *seeks* is decided by the table's key, because YSQL turns a
range comparison into a DocDB seek only when it can compute one start key from it:

- `pages` / `wals` are **all-range**, led by `bucket_id`, so an anchored row
  constructor over a leading prefix of the primary key seeks — and being a true
  lexicographic "everything at or after this point", it crosses bucket and
  relation boundaries by itself. **No per-bucket fan-out**, which is why the
  reaper carries no `pages`/`wals` bucket count to drift out of step.
- `meta` / `slru_pages` / `pending_gc` are **hash-keyed**, where a row constructor
  cannot seek at all (the furthest DocDB can compute is the start of the branch's
  hash partition). They use equality on every leading key column plus one range
  comparison on the next. `pending_gc` additionally binds `bucket_id` by equality
  and loops it, since its `bucket_id` is inside the hash key.
- `blobs` needs no cursor (a branch's blob volume is initdb-archive-scale) and
  `branch_state` is a single row.

Termination is a **read** — the SELECT coming back empty — not an inference from
how many rows a DELETE affected. That is what makes concurrent reaps safe: a
second replica deleting rows out from under a batch cannot make this one conclude
a table is drained while rows remain.

| flag | default | |
|---|---|---|
| `--branch-gc` | true | run the reaper at all |
| `--branch-gc-interval` | 30s | tick cadence |
| `--branch-gc-retention` | 24h | window between the drop and the reap; matches the default PITR window |
| `--branch-gc-rows-per-delete` | 5000 | rows one DELETE names: the SELECT's `LIMIT`, and so the size of its `ybctid` list |
| `--branch-gc-rows-per-tick` | 20000 | pacing budget across every delete in the tick (~2.4M rows/hour at the default interval) |

Pacing is **open-loop** — a fixed rows/tick, not a reaction to cluster load — so
the defaults are deliberately conservative: this is garbage with no deadline, and
the only cost of going slowly is disk.

A dropped **cluster**'s branches are reclaimed too (its tombstone stamps them
exactly as a branch delete does); nothing reclaims the cluster row itself.

## Why fencing is taken by the endpoint, not injected at deploy

The fencing value is acquired on the **endpoint's own path**: its supervisor takes
a lease on the branch before it starts anything, under a client id generated per
process. This is deliberate and load-bearing — the fence must distinguish **every**
incarnation, so that a restarted or rescheduled pod cannot be mistaken for the one
it replaces. Kubernetes restarts-in-place and node-failure reschedules happen
**outside** the controller's deploy flow (kubelet drives them), so a value the
endpoint mints for itself at process start is what keeps those safe: a lingering
old instance holds a lease that has stopped being renewed, and its services stop
writing when it lapses.

Rejected alternative (closed #206 / #207 / #212 / #213): have amp mint the fencing
value once at deploy time and inject it into the pod (originally a `GENERATION` env
value). A kubelet restart would then **reuse** the injected value and fail to fence
the still-live old instance — a split-brain / corruption risk. That argument is
unchanged by the move from a per-spawn generation counter to a per-process lease,
and is exactly why `WRITE_TOKEN_CLIENT_ID` defaults to a per-process id rather than
anything the chart or the controller could template: two instances sharing one id
are both "the holder", and `token_acquire` grants to the holder unconditionally.
For the same reason, pageserver-node registration stays pod-driven rather than
moving in-process, and there is no liveness heartbeater (nothing consumes node
availability — the `nodes` registry is bookkeeping only).
