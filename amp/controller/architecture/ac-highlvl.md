# amp-controller (ac) — PRD Summary

## The one-paragraph version

**amp-controller (ac) is the control plane for YB Amp.** It owns the full
lifecycle of everything a customer cares about — clusters, their databases (branches),
the read/write compute "endpoints" that serve them, and the branching between them —
and exposes all of it through a single REST API. It keeps its own state in YBDB
it manages and reuses YB Amp's existing storage components rather than 
reimplementing them. YBLC will also be able to integrate with YBA if we want to 
add the ability to scale the universe directly from YBAmp Controller.

## 1. Parts of the Foundation

A **foundation** is one YugabyteDB universe plus everything running on top of it — the
unit YBLC manages. It is built from a handful of parts:

- **Managed YugabyteDB universe** — the storage substrate, and where the Controller
  keeps its own state.
- **The Controller (YBLC)** — the control plane: the single authority over every
  resource in the foundation, exposed through a REST API.
- **MCP server** — an agent-facing adapter that exposes the control plane as tool calls.
- **Storage layer** — internal components the Controller drives to provision
  and run databases.
- **Endpoints** — the running Postgres instances that serve each database over the wire.

There is **one Controller per foundation**; the Controller lives *inside* the foundation
it manages, and a deployment with N foundations has N independent controllers.
Coordinating *across* foundations (managing a fleet) is the job of a future cluster and
not currently in scope of YBAmp Controller.

These parts split into two planes — control and data — with the MCP server sitting in
front of the control plane, and the rule for reaching each is different.

**Control plane — always through the Controller.** Anything that changes the shape of
the world — create a cluster, branch a branch, add/remove/pause an
endpoint, scale — is a **control-plane mutation**, and the only way to perform one is
a Controller API call. Agents, the MCP server, future automation — they are all just
API consumers of the Controller. Neither the data nor the compute layer has the
knowledge or access needed to perform these operations; that capability lives only in
the control plane today.

**An MCP server ships with YBLC.** Alongside the REST API, YBLC exposes its control
plane as an MCP server so that AI agents can drive lifecycle operations — create a
cluster, branch a branch, stand up or pause an endpoint, scale — through
tool calls. The MCP server is a thin adapter over the same Controller API; it grants
no special privileges and bypasses nothing, it simply makes the control plane
directly consumable by agents.

**Data plane — straight to the endpoint.** To actually run SQL, a client connects
directly to an endpoint over the normal Postgres wire protocol. The Controller is not
in this path and does not proxy query traffic; the controller can be called to get the
connection string of an endpoint.

> **Rule of thumb:** *Change the shape of the world → Controller API. Read/write rows
> in a database → connect to the endpoint.* An MCP server typically does both: it issues
> control requests to the Controller, and opens direct SQL connections to endpoints for
> data.

```
           Agents / automation
              │              │
   MCP tool calls            │  REST (control: create, branch, scale, pause)
              ▼              │
   ┌──────────────┐          │
   │  MCP server  │          │
   └──────────────┘          │
              │  Controller  │
              │  API calls   │
              ▼              ▼
   ┌─────────────────────────────┐     drives       ┌──────────────┐
   │  YBLC                        │ ──────────────▶ │ storage layer │  (internal only —
   │  (control plane / REST API)  │  compute_ctl,    │  + computes   │   never called by
   └─────────────────────────────┘  storage ctlr    └──────────────┘   agents directly)

        │  (data: SQL) ── apps/agents connect DIRECTLY to ▶  [ endpoint = Postgres ]
```

## 2. The resource ladder: what each "create" step requires

Resources form a strict hierarchy: you cannot create a resource until its parent
exists. Requirements are cumulative going down the table.

```
Foundation (= one YugabyteDB universe, one Controller)
  └── Storage Pool       ≈ a Meko datapack; any user can create one (becomes owner)
        └── Cluster      a "database": created with its one default branch
              └── Branch          the default branch + its forks
                    └── Endpoint        a running Postgres serving that branch
```

| To create… | You need (cumulatively)… | Notes |
|---|---|---|
| **Storage pool** | A user account | Any user can create a pool and becomes its owner. It maps roughly to a Meko datapack; reaching someone else's pool requires it to be **shared** with you. |
| **Cluster** (with its default branch) | Access to the pool | Creating a cluster creates the cluster *and* its one **default** branch together, and optionally that branch's first (write) endpoint. The default branch is never created on its own. Access is then granted per cluster. |
| **Branch** (another branch) | Write access to the cluster | Fork an existing branch into a new one under the same cluster — off the default branch or off another branch (branch-of-branch). |
| **Endpoint** | An existing branch | A *write* endpoint is only allowed if the branch has no writer yet (single-writer rule); all others are read-only. |

Branching is the action of creating a new branch from an existing one (the default
branch or another branch); once created, a branch is an ordinary branch under the same
cluster. (Merging branches is future work.)

Creating clusters, branches, and endpoints can all be driven through the **MCP server**.

Under the hood a cluster is a purely control-plane grouping with **no** storage object;
a branch's `branch_id` **is** the storage layer's flat `BranchId` verbatim. The
Controller carries that storage-layer branch id verbatim when talking to the storage
controller.

Some notes on branches and endpoints:

- **Branches are cluster-scoped.** A branch is identified within the Controller by
  *(cluster, branch)*, the cluster being a control-plane scoping only; the `branch_id`
  alone is the storage-layer identity.
- **One writer per branch.** A second write endpoint cannot exist.

## 3. Main Workflows

Every one of these is a **Controller API call** — there is no other entry point — and
each runs as a **task**: a durable, idempotent, re-runnable workflow built from reusable
steps, so a crash mid-operation resumes rather than leaving a mess.

1. **Create storage pool.** Any user can create a storage pool and becomes its **owner**;
   it maps roughly to a Meko datapack. A user who did not create a pool reaches it only if
   it is **shared** with them.
2. **Create cluster.** The primary entry point. Under a storage pool the caller can
   reach, the Controller — in one task — creates a **cluster** (a purely control-plane
   grouping, assigning its **id**, which maps to no storage object), creates that
   cluster's one **default branch** *scoped to
   that cluster* (assigning its **branch id**, which *is* the storage layer's flat
   **`branch_id`**; a branch is identified within the Controller by the pair
   **(cluster, branch)**, never globally — the cluster being a control-plane scoping
   only), registers the branch with the storage
   layer, creates the database record, and can stand up its first write endpoint. The
   default branch is never created on its own — a cluster *is* a control-plane record
   plus its default branch. Access to the new cluster is then granted per user.
3. **Create endpoint.** On an existing branch — addressed by its **(cluster, branch)**,
   the storage-layer identity being the flat **`branch_id`** — the Controller assembles
   the endpoint
   *spec* (DB connection, branch binding, read/write role, version, TLS,
   telemetry), assigns the endpoint its own unique **`endpoint_id`** (endpoints have no
   names; they are addressed by id), provisions the compute, and hands the spec to that
   endpoint's `compute_ctl`. A write endpoint is rejected if the branch already has one
   (single-writer rule). The id is durable: replacing a failed compute reuses the same
   `endpoint_id` (new instance, same record).
4. **Branch.** Fork an existing branch at a chosen point — the default branch or another
   branch — producing a *new* branch (with its own `branch_id` under the same
   cluster) and database linked to their parent, and, optionally, a first endpoint on the
   branch. The pre-fork history is shared, not copied.
5. **Pause / resume.** A Controller API call targeting an endpoint. Pause suspends the
   compute while the endpoint record and its spec survive; resume recreates the running
   instance from that stored spec. Because endpoints are stateless, "resume" is just
   "recreate from the spec we already hold."

> **Resume nuance:** while an endpoint is paused, nothing is listening, so an incoming
> connection can't wake it — scale-up from zero is driven through the Controller API only.

## 4. The Controller as the writer authority

The single-writer rule — one writer per branch — is a correctness requirement, not
just a convenience: two endpoints writing the same branch at once would corrupt it.
The hard case is an endpoint that has gone **bad but is still running** — its process is
up and may still accept writes, yet the Controller has decided to replace it.

Two facts make this tractable: the Controller knows every endpoint and which one holds
the write role for each branch, and the Controller is the only thing that creates
endpoints. A second writer can therefore only appear if the Controller itself creates
one so the whole problem reduces to the Controller ordering its own actions.

**Kill before create.** When the Controller replaces a write endpoint's compute, it fully
retires the old **instance** — and *confirms* it is gone — before a new instance takes
over the write role, so there is never a window in which two live computes write the same
branch. (The endpoint *record* and its `endpoint_id` persist across the swap; it is the
running instance that changes — see §3.) On Kubernetes "retire" means **removing the
pod**, and for a bad-but-running endpoint this is a forceful termination — a bounded
graceful window, then a hard kill. The Controller does not fire-and-forget: it waits for the kill to complete
(the pod actually gone) before creating the replacement, and because the swap runs as a
task (§5), a controller crash mid-replace resumes the same kill→create sequence rather
than stranding two writers.

For a *planned* swap of a healthy endpoint, the Controller can instead hand the old compute
a spec that detaches it and let it step down cooperatively — but replacing a bad endpoint
never depends on that cooperation.

> **The cost:** Kill-before-create means a brief **write-availability gap**: no
> writer exists between the confirmed kill and the promoted replacement. For a
> single-writer branch this is better than the possibility of 2 writers.

**No second layer in storage today.** The safekeeper's term check only
serializes proposers *within one safekeeper process*, and each endpoint runs its **own**
safekeeper; WAL lands in a shared store (`amp_storage.wals`) with no term guard on the
write. So a stale writer on its own safekeeper is refused by nothing in storage —
single-writer rests entirely on kill-before-create. This method is not a guarantee under a
network partition: an isolated but live node can have its pod object deleted from the API
while the process keeps running and keeps writing to the shared store.
Closing that gap needs a real fence: for example a lease the writer must hold to commit.
That fence is **future work**; until it lands, the partition case is a known gap.

## 5. How the task framework keeps YBLC available across crashes

Because every operation is a task, YBLC can lose the instance that happens to be
running one and still finish the work. A task is **write-ahead**: it records the ordered
list of **steps** it will run before running any of them, then walks that list, marking
each step done as it goes (the `task` / `task_step` tables in §6). The framework rests on
three properties:

- **Task state lives in the managed YBDB, not in the running process.** A task and its
  steps — which step it is on, which steps already finished, their results — are
  persisted as rows. A controller crash loses no progress, because the progress was
  never only in memory.
- **Steps are idempotent and checkpointed.** Each step records its own completion, so
  re-running a task skips the steps that already succeeded and resumes from the one that
  didn't. Replaying a half-finished operation converges to the same result instead of
  double-applying it.
- **A task is owned by one instance at a time, and ownership is recoverable.** Claiming a
  task is an atomic transition (pending → running) under a lease, so two instances never
  run the same task at once. If the owner crashes, its task stays `running` until the
  lease expires; another instance then reclaims it and carries it to completion, skipping
  the steps the dead instance already finished.

## 6. State: a first look at the tables

Everything the Controller knows lives as rows in **its own schema in the managed
YugabyteDB universe**. The schema falls into three buckets: the **ownership/access**
hierarchy, the **physical** resources (branches and endpoints), and the **operational**
task log. A sketch in PostgreSQL DDL follows — a first cut to show the shape; authoritative
DDL lives with the code and its migrations.

A cluster's id is a control-plane-only id (it maps to no storage object); a branch's
`branch_id` *is* the storage layer's flat `BranchId` — a 128-bit id, carried here
verbatim as `uuid` and globally unique, so it stands alone as the branch's primary
key. The storage layer is flat: there is no enclosing tenant, and nothing above a
branch is part of its identity — `cluster_id` is an ordinary scoping column, and an
endpoint has none at all (it reaches its cluster through its branch). One
consequence: the fork FK can no longer also assert that a parent is in the same
cluster, so in-cluster lineage is an application invariant rather than a schema
one. All foreign keys are
`RESTRICT` (the PG default) unless noted: teardown is top-down through tasks, never via
`ON DELETE CASCADE` surprises.

**Ownership & access**

```sql
CREATE TABLE users (                        -- "user" is reserved in PG
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    is_admin    boolean NOT NULL DEFAULT false  -- admins skip all grants
);

-- A storage pool maps roughly to a Meko datapack. Any user can create one and is
-- recorded as its owner; others reach it only via a user_pool_grant (a "share").
CREATE TABLE storage_pools (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name           text NOT NULL UNIQUE,
    owner_user_id  uuid NOT NULL REFERENCES users(id)
);

CREATE TABLE clusters (                         -- a "database": one cluster, one default branch
    id               uuid PRIMARY KEY,   -- control-plane-only id; maps to no storage object
    storage_pool_id  uuid NOT NULL REFERENCES storage_pools(id),
    name             text NOT NULL,
    dropped_at       timestamptz                -- tombstone; see `branch` below
);

-- Name unique within its pool, among LIVE clusters.
CREATE UNIQUE INDEX clusters_name_per_pool ON cluster (storage_pool_id, name)
    WHERE dropped_at IS NULL;

CREATE TABLE user_pool_grants (                 -- the "share": create/see clusters in a pool
    user_id          uuid NOT NULL REFERENCES users(id),
    storage_pool_id  uuid NOT NULL REFERENCES storage_pools(id),
    PRIMARY KEY (user_id, storage_pool_id)
);

CREATE TYPE grant_role AS ENUM ('read_only', 'read_write');
CREATE TABLE user_cluster_grants (              -- per-cluster access; revoke = drop the row
    user_id     uuid NOT NULL REFERENCES users(id),
    cluster_id  uuid NOT NULL REFERENCES clusters(id),
    role        grant_role NOT NULL,
    PRIMARY KEY (user_id, cluster_id)
);
```

**Physical**

```sql
-- A branch's branch_id *is* the storage layer's flat BranchId, globally unique;
-- the storage layer is flat (no enclosing tenant), so the id alone is the whole
-- identity and cluster_id is a control-plane scoping column.
CREATE TABLE branches (
    branch_id           uuid PRIMARY KEY,                      -- = storage-layer flat branch_id
    cluster_id          uuid NOT NULL REFERENCES clusters(id), -- control-plane scoping only
    name                text NOT NULL,
    pg_version          int  NOT NULL, -- Is PG Version needed?
    is_default          boolean NOT NULL DEFAULT false,  -- the cluster's default (root) branch

    -- Branching
    parent_branch_id    uuid REFERENCES branches(branch_id),  -- fork ancestry; NULL for a root
    fork_lsn            pg_lsn,                -- the point the branch was taken at

    -- Extra Postgres settings for every compute on this branch, {name: value}.
    -- A fork inherits its ancestor's; an endpoint's own merge over these.
    pg_settings         jsonb NOT NULL DEFAULT '{}'::jsonb,

    -- A dropped branch is TOMBSTONED, not deleted: a branch id is never reused and
    -- its pages/WAL in amp_storage outlive the drop. Every "is this live?" reader
    -- filters `dropped_at IS NULL`. Clusters tombstone for the follow-on reason:
    -- the FK above is RESTRICT, so a cluster whose branches survive as tombstones
    -- could never be deleted.
    dropped_at          timestamptz,

    -- Branch-drop GC (sc-integration.md § Branch-drop GC): `reap_after` is the
    -- deadline, left NULL by the drop and stamped by the reaper as
    -- `dropped_at + --branch-gc-retention`; `gc_completed_at` says the branch's
    -- amp_storage rows are gone. The row itself is kept, so "reclaimed" stays
    -- distinguishable from "never existed".
    reap_after          timestamptz,
    gc_completed_at     timestamptz
);

-- "The branches of this cluster" is the hottest access path and the primary key
-- no longer serves it, so it gets an index of its own.
CREATE INDEX branches_by_cluster ON branches (cluster_id);
CREATE INDEX branches_by_parent  ON branches (parent_branch_id);

-- Name unique within its cluster, and exactly one default branch per cluster —
-- both among LIVE branches, so a drop frees the name and lets a new default be
-- designated. Partial because the rows stay behind forever.
CREATE UNIQUE INDEX branches_name_per_cluster ON branches (cluster_id, name)
    WHERE dropped_at IS NULL;
CREATE UNIQUE INDEX branches_one_default_per_cluster ON branches (cluster_id)
    WHERE is_default AND dropped_at IS NULL;

-- The branch-drop reaper's due-scan: partial on its exact predicate, so the index
-- holds only tombstones that are not yet reclaimed. ASC because it is a range
-- predicate (`now() >= reap_after`) — YSQL's default HASH cannot seek one.
CREATE INDEX branches_gc_due ON branches (reap_after ASC)
    WHERE dropped_at IS NOT NULL AND gc_completed_at IS NULL;

CREATE TYPE endpoint_role  AS ENUM ('read_write', 'read_only');

-- Durable record, ephemeral instance. Addressed by id; optionally named.
CREATE TABLE endpoints (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),

    -- the spec: complete enough to recreate the endpoint from scratch.
    -- No cluster_id: an endpoint reaches its cluster through its branch, so the
    -- two can never disagree.
    branch_id       uuid NOT NULL REFERENCES branches(branch_id),
    role            endpoint_role  NOT NULL,

    -- A caller-chosen handle, optional; an endpoint is always addressable by id.
    name            text,

    -- Access to the endpoint. Empty for a compute the controller did not place
    -- (--deploy-driver=local, where its owner starts it locally).
    address         TEXT,

    -- The config used to create an endpoint. Also carries the caller's
    -- `extended` object: configuration handed to the deployer WITHOUT being
    -- interpreted (for a local compute, the ports it binds), nested under one
    -- key so it cannot collide with a field the control plane owns.
    config          jsonb,

    -- This endpoint's OWN extra Postgres settings. The effective set for its
    -- compute is its branch's merged with these, these winning.
    pg_settings     jsonb NOT NULL DEFAULT '{}'::jsonb
);

-- The single-writer rule, enforced in the schema rather than only in code.
-- Deliberately not partial on a paused endpoint: a paused writer keeps its slot.
CREATE UNIQUE INDEX endpoints_one_writer_per_branch
    ON endpoints (branch_id)
    WHERE role = 'read_write';
-- A name identifies an endpoint within its branch, the way a branch name does
-- within its cluster — two branches may each have a `primary`. NULLs are
-- distinct, so any number of unnamed endpoints coexist.
CREATE UNIQUE INDEX endpoints_name_per_branch ON endpoints (branch_id, name);
CREATE INDEX endpoints_by_branch ON endpoints (branch_id);
```

These are the *only* branch and endpoint tables, and `/api/v1` is the only way in.
The storage layer keeps no copies of its own, and keys on the same `uuid` in the same
spelling. See [`sc-integration.md`](sc-integration.md).

**Operational**

The task log is the write-ahead structure from §5: one `task` row per operation (the
user-visible, API-monitorable unit) plus its ordered `task_step` rows — the steps it will
run, written before execution and marked complete as it goes.

```sql
CREATE TABLE tasks (                            -- one per operation; status is exposed via API
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    kind         text        NOT NULL,         -- 'create_cluster', 'branch', 'scale', ...
    status       text        NOT NULL DEFAULT 'pending',
    params       jsonb       NOT NULL,         -- the originating request
    step_ids     uuid[]      NOT NULL DEFAULT '{}',  -- its steps, in execution order
    error        text,                          -- set when status = 'failed'
    owner        text,                          -- which instance claimed it; fences its writes
    lease_expiry timestamptz                    -- DEPRECATED, unused (see ac-locking.md)
);

CREATE TABLE task_steps (                        -- the write-ahead steps of one task
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),  -- referenced by task.step_ids
    task_id    uuid        NOT NULL REFERENCES tasks(id),
    kind       text        NOT NULL,            -- which re-usable step
    status     text        NOT NULL DEFAULT 'pending',
    args       jsonb       NOT NULL,            -- inputs, written when the task is planned
    result     jsonb                             -- outputs, written when the step succeeds
);
```

The load-bearing rules are in the DDL above, not just in API checks: branch references
are **cluster-scoped composite FKs** (cross-cluster use or forks are unrepresentable), the
**single-writer** partial unique index, **one default branch per cluster**,
**scoped-not-global names**, and **restrict (not cascade) deletes** across the resource
hierarchy.

## What this doc intentionally leaves out

- UX (CLI/console)
- the cross foundation agent layer
- billing/quotas
- end-user API and authentication
- Meko integration: datapacks as storage pools, Meko users gaining access, and updating
the Meko API and MCP servers