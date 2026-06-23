# amp-controller (ac) — PRD Summary

## The one-paragraph version

**amp-controller (ac) is the control plane for YB Light.** It owns the full
lifecycle of everything a customer cares about — projects, their databases (branches),
the read/write compute "endpoints" that serve them, and the branching between them —
and exposes all of it through a single REST API. It keeps its own state in YBDB
it manages and reuses YB Light's existing storage components rather than 
reimplementing them. YBLC will also be able to integrate with YBA if we want to 
add the ability to scale the universe directly from YBLight Controller.

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
Coordinating *across* foundations (managing a fleet) is the job of a future project and
not currently in scope of YBLight Controller.

These parts split into two planes — control and data — with the MCP server sitting in
front of the control plane, and the rule for reaching each is different.

**Control plane — always through the Controller.** Anything that changes the shape of
the world — create a project, branch a branch, add/remove/pause an
endpoint, scale — is a **control-plane mutation**, and the only way to perform one is
a Controller API call. Agents, the MCP server, future automation — they are all just
API consumers of the Controller. Neither the data nor the compute layer has the
knowledge or access needed to perform these operations; that capability lives only in
the control plane today.

**An MCP server ships with YBLC.** Alongside the REST API, YBLC exposes its control
plane as an MCP server so that AI agents can drive lifecycle operations — create a
project, branch a branch, stand up or pause an endpoint, scale — through
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
        └── Project      a "database": created with its one default branch (PG cluster)
              └── Branch (PG cluster)   the default branch + its forks
                    └── Endpoint        a running Postgres serving that branch
```

| To create… | You need (cumulatively)… | Notes |
|---|---|---|
| **Storage pool** | A user account | Any user can create a pool and becomes its owner. It maps roughly to a Meko datapack; reaching someone else's pool requires it to be **shared** with you. |
| **Project** (with its default branch) | Access to the pool | Creating a project creates the project *and* its one **default** branch together, and optionally that branch's first (write) endpoint. The default branch is never created on its own. Access is then granted per project. |
| **Branch** (another branch) | Write access to the project | Fork an existing branch into a new one under the same project — off the default branch or off another branch (branch-of-branch). |
| **Endpoint** | An existing branch | A *write* endpoint is only allowed if the branch has no writer yet (single-writer rule); all others are read-only. |

Branching is the action of creating a new branch from an existing one (the default
branch or another branch); once created, a branch is an ordinary branch under the same
project. (Merging branches is future work.)

Creating projects, branches, and endpoints can all be driven through the **MCP server**.

Under the hood a project *is* the storage layer's **tenant** and a branch *is* its
**timeline**: the Controller carries the storage-layer ids verbatim and maps
project→tenant, branch→timeline only when talking to the storage controller.

Some notes on branches and endpoints:

- **Branches are project-scoped.** A branch is identified by *(project, branch)* — the
  storage layer's *(tenant, timeline)* pair.
- **One writer per branch.** A second write endpoint cannot exist.

## 3. Main Workflows

Every one of these is a **Controller API call** — there is no other entry point — and
each runs as a **task**: a durable, idempotent, re-runnable workflow built from reusable
steps, so a crash mid-operation resumes rather than leaving a mess.

1. **Create storage pool.** Any user can create a storage pool and becomes its **owner**;
   it maps roughly to a Meko datapack. A user who did not create a pool reaches it only if
   it is **shared** with them.
2. **Create project.** The primary entry point. Under a storage pool the caller can
   reach, the Controller — in one task — creates a **project** (assigning its **id**,
   which *is* the storage layer's **`tenant_id`**, the identifier every resource below it
   is scoped by), creates that project's one **default branch (PG cluster)** *scoped to
   that project* (assigning its **branch id**, which *is* the storage layer's
   **`timeline_id`**; a branch is identified by the pair **(project, branch)** =
   **(tenant_id, timeline_id)**, never globally), registers the branch with the storage
   layer, creates the database record, and can stand up its first write endpoint. The
   default branch is never created on its own — a project *is* a tenant plus its default
   branch (its main timeline). Access to the new project is then granted per user.
3. **Create endpoint.** On an existing branch — addressed by its **(project, branch)** =
   storage-layer **(tenant_id, timeline_id)** — the Controller assembles the endpoint
   *spec* (DB connection, branch/timeline binding, read/write role, version, TLS,
   telemetry), assigns the endpoint its own unique **`endpoint_id`** (endpoints have no
   names; they are addressed by id), provisions the compute, and hands the spec to that
   endpoint's `compute_ctl`. A write endpoint is rejected if the branch already has one
   (single-writer rule). The id is durable: replacing a failed compute reuses the same
   `endpoint_id` (new instance, same record).
4. **Branch.** Fork an existing branch at a chosen point — the default branch or another
   branch — producing a *new* branch (with its own branch/`timeline_id` under the same
   project) and database linked to their parent, and, optionally, a first endpoint on the
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
branch's timeline. (The endpoint *record* and its `endpoint_id` persist across the swap; it is the
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
safekeeper; WAL lands in a shared store (`safekeeper.wals`) with no term guard on the
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

A project's id *is* the storage layer's `tenant_id` and a branch's id *is* its
`timeline_id` — the storage layer's 128-bit ids, carried here verbatim as `uuid`; the
rename to project/branch stops at the controller boundary. All foreign keys are
`RESTRICT` (the PG default) unless noted: teardown is top-down through tasks, never via
`ON DELETE CASCADE` surprises.

**Ownership & access**

```sql
CREATE TABLE app_user (                        -- "user" is reserved in PG
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    is_admin    boolean NOT NULL DEFAULT false  -- admins skip all grants
);

-- A storage pool maps roughly to a Meko datapack. Any user can create one and is
-- recorded as its owner; others reach it only via a user_pool_grant (a "share").
CREATE TABLE storage_pool (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name           text NOT NULL UNIQUE,
    owner_user_id  uuid NOT NULL REFERENCES app_user(id)
);

CREATE TABLE project (                         -- a "database": one project, one default branch
    id               uuid PRIMARY KEY,   -- the storage-layer tenant_id
    storage_pool_id  uuid NOT NULL REFERENCES storage_pool(id),
    name             text NOT NULL,
    UNIQUE (storage_pool_id, name)             -- name unique within its pool
);

CREATE TABLE user_pool_grant (                 -- the "share": create/see projects in a pool
    user_id          uuid NOT NULL REFERENCES app_user(id),
    storage_pool_id  uuid NOT NULL REFERENCES storage_pool(id),
    PRIMARY KEY (user_id, storage_pool_id)
);

CREATE TYPE grant_role AS ENUM ('read_only', 'read_write');
CREATE TABLE user_project_grant (              -- per-project access; revoke = drop the row
    user_id     uuid NOT NULL REFERENCES app_user(id),
    project_id  uuid NOT NULL REFERENCES project(id),
    role        grant_role NOT NULL,
    PRIMARY KEY (user_id, project_id)
);
```

**Physical**

```sql
-- A branch *is* a timeline (the two are synonyms); there is no separate
-- timelines table. Identity is the (project_id, branch_id) pair — the storage
-- layer's (tenant_id, timeline_id).
CREATE TABLE branch (
    project_id          uuid NOT NULL REFERENCES project(id),  -- = storage tenant_id
    branch_id           uuid NOT NULL,                         -- = storage timeline_id
    name                text NOT NULL,
    pg_version          int  NOT NULL, -- Is PG Version needed?
    is_default          boolean NOT NULL DEFAULT false,  -- the project's default (root) branch

    -- Branching
    parent_branch_id    uuid,                  -- fork ancestry; NULL for the default branch
    fork_lsn            pg_lsn,                -- the point the branch was taken at

    PRIMARY KEY (project_id, branch_id),
    UNIQUE (project_id, name),                 -- name unique within its project
    FOREIGN KEY (project_id, parent_branch_id)    -- a fork parent stays in-project
        REFERENCES branch (project_id, branch_id)
);

-- Exactly one default branch per project (created with the project); the rest are forks.
CREATE UNIQUE INDEX one_default_per_project ON branch (project_id) WHERE is_default;

CREATE TYPE endpoint_role  AS ENUM ('read_write', 'read_only');

-- Durable record, ephemeral instance. Addressed by id; endpoints have no name.
CREATE TABLE endpoint (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),

    -- the spec: complete enough to recreate the endpoint from scratch
    project_id      uuid NOT NULL,
    branch_id       uuid NOT NULL,
    role            endpoint_role  NOT NULL,

    -- Access to the endpoint
    address         TEXT,
    
    config          jsonb, -- The config used to create an endpoint

    FOREIGN KEY (project_id, branch_id)        -- project-scoped binding
        REFERENCES branch (project_id, branch_id)
);

-- The single-writer rule, enforced in the schema rather than only in code.
CREATE UNIQUE INDEX one_writer_per_branch
    ON endpoint (project_id, branch_id)
    WHERE role = 'read_write';
```

**Operational**

The task log is the write-ahead structure from §5: one `task` row per operation (the
user-visible, API-monitorable unit) plus its ordered `task_step` rows — the steps it will
run, written before execution and marked complete as it goes.

```sql
CREATE TABLE task (                            -- one per operation; status is exposed via API
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    kind         text        NOT NULL,         -- 'create_project', 'branch', 'scale', ...
    status       text        NOT NULL DEFAULT 'pending',
    params       jsonb       NOT NULL,         -- the originating request
    error        text,                          -- set when status = 'failed'
    owner        text,                          -- which instance holds it (crash recovery)
    lease_expiry timestamptz                    -- lease expiry => another instance may reclaim
);

CREATE TABLE task_step (                        -- the write-ahead steps of one task
    task_id    uuid        NOT NULL REFERENCES task(id),
    ordinal    int         NOT NULL,            -- execution order within the task
    kind       text        NOT NULL,            -- which re-usable step
    status     text        NOT NULL DEFAULT 'pending',
    args       jsonb       NOT NULL,            -- inputs, written when the task is planned
    result     jsonb,                            -- outputs, written when the step succeeds
    PRIMARY KEY (task_id, ordinal)              -- steps are ordered, unique within a task
);
```

The load-bearing rules are in the DDL above, not just in API checks: branch references
are **project-scoped composite FKs** (cross-project use or forks are unrepresentable), the
**single-writer** partial unique index, **one default branch per project**,
**scoped-not-global names**, and **restrict (not cascade) deletes** across the resource
hierarchy.

## What this doc intentionally leaves out

- UX (CLI/console)
- the cross foundation agent layer
- billing/quotas
- end-user API and authentication
- Meko integration: datapacks as storage pools, Meko users gaining access, and updating
the Meko API and MCP servers