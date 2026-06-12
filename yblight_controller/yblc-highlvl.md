# yblight-controller (yblc) — PRD Summary

## The one-paragraph version

**yblight-controller (yblc) is the control plane for YB Light.** It owns the full
lifecycle of everything a customer cares about — tenants, databases (timelines),
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
the world — create a tenant, create or branch a timeline, add/remove/pause an
endpoint, scale — is a **control-plane mutation**, and the only way to perform one is
a Controller API call. Agents, the MCP server, future automation — they are all just
API consumers of the Controller. Neither the data nor the compute layer has the
knowledge or access needed to perform these operations; that capability lives only in
the control plane today.

**An MCP server ships with YBLC.** Alongside the REST API, YBLC exposes its control
plane as an MCP server so that AI agents can drive lifecycle operations — create a
tenant, create or branch a timeline, stand up or pause an endpoint, scale — through
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
  └── Tenant Pool        ≈ an "org"; created by an admin
        └── Tenant       a customer/team; the unit of timeline ownership and access
              └── Timeline (PG cluster)   a database + its data
                    └── Endpoint          a running Postgres serving that database
```

| To create… | You need (cumulatively)… | Notes |
|---|---|---|
| **Tenant pool** | **Admin** on the foundation | A pool is like an "org"; standing one up stays an admin action. |
| **Tenant** | Access to the pool | Tenants live within a pool. Access is then granted per tenant, so a pool's users don't automatically reach every tenant in it. |
| **Timeline (PG cluster)** | Write access to a tenant | Creating the timeline also stands up its first (write) endpoint by default. |
| **Endpoint** | An existing timeline | A *write* endpoint is only allowed if the timeline has no writer yet (single-writer rule); all others are read-only. |

Branching and merging are not resources — they are the action of creating a new timeline 
from an existing one or merging them together.

Creating tenants, timelines, and endpoints can all be driven through the **MCP server**.

Some notes on timelines and endpoints:

- **Timelines are tenant-scoped.** A timeline is identified by *(tenant, timeline)*.
- **One writer per database.** A second write endpoint cannot exist.

## 3. Main Workflows

Every one of these is a **Controller API call** — there is no other entry point — and
each runs as a **task**: a durable, idempotent, re-runnable workflow built from reusable
steps, so a crash mid-operation resumes rather than leaving a mess.

1. **Create tenant.** With access to the target pool, the Controller records a new tenant
   under it and assigns it a **`tenant_id`** — the identifier every resource below it is
   scoped by. Access to that tenant is then granted per user.
2. **Create timeline (PG cluster).** Under an existing tenant, the Controller creates a
   new timeline *scoped to that `tenant_id`* and assigns it its own **`timeline_id`** — a
   timeline is identified by the pair **(tenant_id, timeline_id)**, never globally. It then
   registers the timeline with the storage layer, creates the database record, and can
   stand up its first write endpoint.
3. **Create endpoint.** On an existing timeline — addressed by its **(tenant_id,
   timeline_id)** — the Controller assembles the endpoint *spec* (DB connection, timeline
   binding, read/write role, version, TLS, telemetry), assigns the endpoint its own unique
   **`endpoint_id`** (endpoints have no names; they are addressed by id), provisions the
   compute, and hands the spec to that endpoint's `compute_ctl`. A write endpoint is
   rejected if the timeline already has one (single-writer rule). The id is durable:
   replacing a failed compute reuses the same `endpoint_id` (new instance, same record).
4. **Branch.** Fork an existing timeline at a chosen point, producing a *new* timeline —
   with its own `timeline_id` under the same tenant — and database linked to their parent,
   and, optionally, a first endpoint on the branch. The pre-branch history is shared, not
   copied.
5. **Pause / resume.** A Controller API call targeting an endpoint. Pause suspends the
   compute while the endpoint record and its spec survive; resume recreates the running
   instance from that stored spec. Because endpoints are stateless, "resume" is just
   "recreate from the spec we already hold."

> **Resume nuance:** while an endpoint is paused, nothing is listening, so an incoming
> connection can't wake it — scale-up from zero is driven through the Controller API only.

## 4. The Controller as the writer authority

The single-writer rule — one writer per database — is a correctness requirement, not
just a convenience: two endpoints writing the same database at once would corrupt it.
The hard case is an endpoint that has gone **bad but is still running** — its process is
up and may still accept writes, yet the Controller has decided to replace it.

Two facts make this tractable: the Controller knows every endpoint and which one holds
the write role for each timeline, and the Controller is the only thing that creates
endpoints. A second writer can therefore only appear if the Controller itself creates
one so the whole problem reduces to the Controller ordering its own actions.

**Kill before create.** When the Controller replaces a write endpoint's compute, it fully
retires the old **instance** — and *confirms* it is gone — before a new instance takes
over the write role, so there is never a window in which two live computes write the same
timeline. (The endpoint *record* and its `endpoint_id` persist across the swap; it is the
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
> single-writer database this is better than the possibility of 2 writers.

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
running one and still finish the work. The framework rests on three properties:

- **Task state lives in the managed YBDB, not in the running process.** A task and its
  subtasks — which step it is on, which steps already finished, their results — are
  persisted as rows. A controller crash loses no progress, because the progress was
  never only in memory.
- **Subtasks are idempotent and checkpointed.** Each step records its own completion, so
  re-running a task skips the steps that already succeeded and resumes from the one that
  didn't. Replaying a half-finished operation converges to the same result instead of
  double-applying it.
- **A task is owned by one instance at a time, and ownership is recoverable.** Claiming a
  task is an atomic transition (pending → running), so two instances never run the same
  task at once. If the owner crashes, its task is left `running` with its checkpoints
  intact; another instance reclaims orphaned tasks (running → pending) and carries them
  to completion, skipping the steps the dead instance already finished.

## 6. State: a first look at the tables

Everything the Controller knows lives as rows in **its own schema in the managed
YugabyteDB universe**. The schema falls into three buckets: the **ownership/access**
hierarchy, the **physical** resources (timelines and endpoints), and the **operational**
task log. A sketch in PostgreSQL DDL follows — a first cut to show the shape; authoritative
DDL lives with the code and its migrations.

The task structure is left out of this high level document, and can be designed/implemented
later.

`tenant_id` and `timeline_id` are the storage layer's 128-bit ids, carried here as `uuid`.
All foreign keys are `RESTRICT` (the PG default) unless noted: teardown is top-down through
tasks, never via `ON DELETE CASCADE` surprises.

**Ownership & access**

```sql
CREATE TABLE tenant_pool (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name        text NOT NULL UNIQUE
);

CREATE TABLE tenant (
    id              uuid PRIMARY KEY,   -- the storage-layer tenant_id
    tenant_pool_id  uuid NOT NULL REFERENCES tenant_pool(id),
    name            text NOT NULL,
    UNIQUE (tenant_pool_id, name)              -- name unique within its pool
);

CREATE TABLE app_user (                        -- "user" is reserved in PG
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    is_admin    boolean NOT NULL DEFAULT false    -- admins skip all grants
);

CREATE TABLE user_pool_grant (                 -- create/see tenants in a pool
    user_id         uuid NOT NULL REFERENCES app_user(id),
    tenant_pool_id  uuid NOT NULL REFERENCES tenant_pool(id),
    PRIMARY KEY (user_id, tenant_pool_id)
);

CREATE TYPE grant_role AS ENUM ('read_only', 'read_write');
CREATE TABLE user_tenant_grant (               -- per-tenant access; revoke = drop the row
    user_id    uuid NOT NULL REFERENCES app_user(id),
    tenant_id  uuid NOT NULL REFERENCES tenant(id),
    role       grant_role NOT NULL,
    PRIMARY KEY (user_id, tenant_id)
);
```

**Physical**

```sql
-- A pg_cluster *is* a timeline (the two are synonyms); there is no separate
-- timelines table. Identity is the (tenant_id, timeline_id) pair.
CREATE TABLE pg_cluster (
    tenant_id           uuid NOT NULL REFERENCES tenant(id),
    timeline_id         uuid NOT NULL,
    name                text NOT NULL,
    pg_version          int  NOT NULL,
    
    -- Branching timelines
    parent_timeline_id  uuid,                  -- fork ancestry; NULL for a root timeline
    fork_lsn            pg_lsn,                -- the point the branch was taken at

    PRIMARY KEY (tenant_id, timeline_id),
    UNIQUE (tenant_id, name),                  -- name unique within its tenant
    FOREIGN KEY (tenant_id, parent_timeline_id)   -- a fork parent stays in-tenant
        REFERENCES pg_cluster (tenant_id, timeline_id)
);

CREATE TYPE endpoint_role  AS ENUM ('read_write', 'read_only');

-- Durable record, ephemeral instance. Addressed by id; endpoints have no name.
CREATE TABLE endpoint (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),

    -- the spec: complete enough to recreate the endpoint from scratch
    tenant_id       uuid NOT NULL,
    timeline_id     uuid NOT NULL,
    role            endpoint_role  NOT NULL,

    -- Access to the endpoint
    address         TEXT,

    -- Cert info as this needs rotation every 24~ hours
    tls_cert_ref    text,
    cert_issuer     text,
    cert_expiry     timestamptz,
    cert_rotation_state text,                  -- so cert rotation can run as a task

    FOREIGN KEY (tenant_id, timeline_id)       -- tenant-scoped binding
        REFERENCES pg_cluster (tenant_id, timeline_id)
);

-- The single-writer rule, enforced in the schema rather than only in code.
CREATE UNIQUE INDEX one_writer_per_timeline
    ON endpoint (tenant_id, timeline_id)
    WHERE role = 'read_write';
```

The load-bearing rules are in the DDL above, not just in API checks: timeline references
are **tenant-scoped composite FKs** (cross-tenant use or forks are unrepresentable), the
**single-writer** partial unique index, **scoped-not-global names**, and **restrict (not
cascade) deletes** across the resource hierarchy.

## What this doc intentionally leaves out

- UX (CLI/console)
- the cross foundation agent layer
- billing/quotas
- end-user API and authentication
- Meko integration: datapacks as tenants, Meko users gaining access, and updating meko api 
and mcp servers