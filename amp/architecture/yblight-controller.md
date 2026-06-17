# yblight-controller — High-Level Design & Guiding Principles

**Status:** Draft
**Last updated:** 2026-06-17

This is the guiding-principles document for yblight-controller, the control plane for YB Light.
It is intentionally not a feature design doc — individual features (task framework,
API schemas, cert management, etc.) will get their own docs in `designs/` and should
stay consistent with the principles laid out here.

## 1. What yblight-controller is

yblight-controller is the control plane for a **single placement group**. It lives within the
placement group it manages and owns the full lifecycle of everything inside it:
tenancy (storage pools, tenants), PG clusters and their timelines, and the endpoints
that serve traffic. It provisions capacity ahead of demand, drives YBA for YBDB
universe management, and exposes an API for all of it.

The placement group is yblight-controller's deployment boundary: one placement group, one yblight-controller.
Later, higher-level **agents** will manage fleets of control planes (and therefore
fleets of placement groups) — that layer is explicitly outside yblight-controller's scope.
yblight-controller's API is what such an agent will consume, which is one more reason to be
API-first (§3).

yblight-controller does **not** serve data-plane traffic. Endpoints are self-contained YB Light
instances; yblight-controller creates, configures, tracks, and retires them.

## 2. Why a standalone service (and not YBA)

- **yblight-controller is self-built and standalone, not a YBA fork.** It runs alongside YBA;
  YBA continues to manage the YBDB universes, and yblight-controller drives YBA via its REST API.
- **The tenancy model is the core reason a fork was rejected.** yblight-controller owns tenants,
  service instances, and instance→universe pinning. Multiple tenants share one
  universe, which inverts YBA's Customer-owns-Universe model — retrofitting that
  inversion into YBA would fight its data model everywhere.
- **VM provisioning reuses ybops** — the existing ybops PEX artifact, repackaged and
  consumed as a pinned vendored dependency, driven via the `ybcloud.py` CLI and paired
  with node-agent for in-guest operations. ybops covers VMs only; pod-based deployments
  get their own Helm chart/operator.

## 3. Guiding principles

1. **API-first.** Every capability is exposed through the yblight-controller API before any UX
   exists. UX (CLI, console) is a consumer of the API, never a side channel. We are
   deliberately not designing UX right now. The API is a basic REST API, versioned in
   the URL path (e.g. `/api/v1/...`).
2. **yblight-controller stays simple to run.** yblight-controller itself must run equally well on Kubernetes
   or a VM. To keep that true, its external dependencies are limited to:
   - the placement group's **YBDB universe**, reached over the PG wire protocol
     (YSQL) — this is yblight-controller's state store; it does not run a database of its own, and
   - optionally, the **YBA REST API**.

   The universe's connection details (a YSQL connection string) are always provided
   directly as deployment configuration, in both modes (§9). YBA access, when
   configured, adds universe management on top — it is never part of the path to
   the state store.

   It must not require cluster-admin powers, cloud-provider credentials in its core
   path, or anything else that would tie it to one runtime. Runtime-specific logic
   (e.g. creating pods vs. VMs) lives behind a provisioning interface, not in the core.
3. **Multi-runtime behind a provisioning interface.** Runtime-specific endpoint
   provisioning lives behind the `deploy.Deployer` interface (§5.6), never in the
   core. The first *implemented* runtime is **Docker** (one container per endpoint —
   used by local/dev and the e2e); **Kubernetes** (one pod per endpoint) is the
   production target and the next implementation. The abstractions (deployer
   interface, endpoint spec, task framework) are designed so a new runtime — VMs,
   another cloud — is a new `Deployer`, not a change to the model.
4. **The control plane enforces invariants; it doesn't just hope the data plane does.**
   Even where YB Light enforces a rule internally (e.g. a single writer per timeline),
   yblight-controller independently enforces it at the API layer so users can never get into a bad
   position. Defense in depth, with yblight-controller as the first line.
5. **All mutations go through the task framework.** Any action that changes the world
   — create a database, branch a timeline, add an endpoint — is a task composed of
   re-usable, idempotent **steps** (§8). Nothing mutates state out-of-band.
6. **yblight-controller is the source of truth for intent; the world is the source of truth for
   reality.** yblight-controller records what *should* exist; reconciliation against what actually
   exists (in k8s, YBA, and the endpoints themselves) detects drift — and repairs it
   by triggering tasks (§8), never through a separate mutation path.
7. **Leverage, don't reimplement.** YBA manages YBDB. ybops handles VM provisioning.
   compute_ctl manages services inside an endpoint. The existing storage controller
   keeps handling storage-layer fencing and registry duties (§11). yblight-controller
   orchestrates these — it does not duplicate them.

## 4. Resource model

The hierarchy:

```
Placement Group ──── YBDB Universe (managed by YBA)
  └── Storage Pools (0+)       ≈ a Meko datapack; created by any user, shared to others
        └── Tenants (0+)       each is a "database": a tenant + its one main PG cluster
              └── PG Clusters   the main (root) cluster + its branches (0+ forks)
                    └── Endpoints (0+)
```

Most levels may be empty: a fresh placement group has no storage pools yet, a fresh
pool no tenants, a cluster no endpoints (§4.4). The tenant is the exception — it is
created together with its main PG cluster (§6), so a tenant always has at least that
one cluster. Empty is both a valid resting state and what top-down teardown (§10.2)
passes through.

A yblight-controller instance manages exactly **one** placement group (§1), so the placement
group is the implicit root of everything in yblight-controller's API and database — not one row
among many.

### 4.1 Logical constructs

These exist only in yblight-controller's state store; they have no physical footprint of their own.

- **Placement group** — top-level unit, and the scope of a yblight-controller deployment. Each
  placement group has exactly one YBDB universe (managed by a YBA; one YBA can manage
  multiple universes) and holds zero or more storage pools.
- **Storage pool** — a grouping of tenants, modeled to map roughly onto a **Meko
  datapack**. It is *not* an "org" — the org level above pools is currently undefined
  (§12). Any user can create a storage pool and becomes its **owner**; a user who did
  not create it reaches it only if the pool is **shared** with them (a pool grant,
  §10.1). A user can own, or be shared into, many pools.
- **Tenant** — a **database**: the unit a client creates, scoped to one storage pool. A
  tenant maps ~1:1 to its **main PG cluster** — creating a database creates the tenant
  and that main cluster together (§6) — and every branch of that cluster lives under the
  same tenant (§4.3). The tenant is the unit of timeline ownership (§4.3) and the finest
  grain of user access (§10.1); that timeline scoping is what isolates tenants from each
  other structurally, with access granted per tenant.

### 4.2 Physical constructs

These correspond to real data or real running processes, and yblight-controller tracks each one.

- **YBDB universe** — the shared storage layer for a placement group; managed by YBA,
  driven by yblight-controller through YBA's API.
- **PG cluster** — a logical Postgres database backed by physical data in YBDB; that
  data is the cluster's timeline, exactly one per cluster (§4.3). Each tenant has one
  **main** (root) cluster, created with the tenant; every other cluster under the tenant
  is a **branch**, produced by forking another cluster's timeline (§4.3).
- **Endpoint** — a self-contained YB Light instance serving a PG cluster; runs on a
  pod (or, later, a VM). Endpoints are what users actually connect to, so the API
  must return endpoint connection details (host, port, TLS state) to users.
  Endpoints are **stateless** (§5.3): "physical" here means they consume hardware,
  not that they hold durable data.

### 4.3 Timelines

A PG cluster's data is its **timeline** — the two are synonyms for one entity,
viewed from two angles: we say "timeline" when talking about the data, and "PG
cluster" when talking about the compute serving it. Timelines are uniquely identified
by the pair **(tenant_id, timeline_id)** — a timeline cannot be named, resolved, or
attached to outside its owning tenant. This scoping is structural: every API and internal
interface that touches a timeline takes the tenant as part of the key, so cross-tenant
timeline use is unrepresentable, not merely forbidden.

Forking a timeline produces a new timeline (same tenant) and a new PG cluster bound
to it; the parent/child relationship is tracked so branch ancestry is queryable.

### 4.4 Endpoint cardinality and the single-writer invariant

- A PG cluster has **zero or more** endpoints. Zero is a valid, supported state —
  a cluster's data lives in YBDB regardless of whether anything is serving it
  (this is what enables scale-to-zero).
- **Scale-up from zero is API-driven only.** An endpoint must be created through
  the yblight-controller API before anything can connect to it; with zero endpoints there is
  nothing listening, so a "wake on connect" model is impossible by construction —
  there is nothing to receive the connection.
- **At most one endpoint per PG cluster may be a write endpoint.** All others are
  read-only. yblight-controller enforces this at the API layer (per principle 4): a request that
  would create a second writer, or flip a reader to writer while a writer exists, is
  rejected before anything touches the data plane — regardless of whether YB Light
  would also have refused it.

### 4.5 Capacity headroom

Headroom — the spare capacity kept on hand so new endpoints and PG clusters never
block on provisioning — is modeled at the **placement group level**: one pool of
spare capacity per placement group, not per storage pool or tenant. Subdividing it
between storage pools is a later refinement, possible if and when it's needed,
precisely because we start with the coarser unit.

This works because endpoints are stateless (§5.3) and carry no hardware affinity:
an endpoint may be scheduled next to endpoints of other tenants — including tenants
in other storage pools. Isolation between tenants is enforced by timeline scoping
(§4.3), with user access granted per tenant (§10.1) — not by dedicated
hardware.

## 5. Service topology

Every process in a running placement group belongs to exactly one of two tiers:

- **Placement-group-global services** (§5.1) — one of each per placement group,
  deployed once and shared by every endpoint: yblight-controller itself, the storage
  controller, and endpoint_storage. This is the control-plane tier; none of it serves
  data-plane traffic.
- **Per-endpoint services** (§5.2) — the YB Light stack that makes up a single
  endpoint, deployed once per endpoint: compute/pg, safekeeper, pageserver,
  compute_ctl, and an otel collector. This is the data-plane tier.

```
Placement group
├── Global services (one of each, §5.1)
│   ├── yblight-controller    — the control plane (this doc)
│   ├── storage controller    — storage-layer registry + generation fencing (§11.1)
│   └── endpoint_storage      — endpoint-scoped scratch store (§11.5)
└── Endpoints (0+)            — one self-contained blob each (§5.4)
    └── per endpoint (§5.2):
        ├── compute / pg      — the Postgres users connect to
        ├── safekeeper        — WAL durability
        ├── pageserver        — page materialization/serving
        ├── compute_ctl       — supervises the above; controller's single contact
        └── otel collector    — metrics
```

The split is along statefulness. The global tier persists to the universe over YSQL
and fronts the whole placement group; the per-endpoint tier is stateless (§5.3) and
disposable — all of an endpoint's durable data lives in the universe, not on the
endpoint. yblight-controller is the only caller of the global services' internal APIs
and the only thing that talks to each endpoint's compute_ctl.

### 5.1 Placement-group-global services

Exactly one of each runs per placement group (§1):

- **yblight-controller** — the control plane this document describes. The deployment
  boundary is one placement group, one controller (§1); it owns the lifecycle of
  everything below it and exposes the API (§3) that an agent layer (§12) will consume.
- **storage controller** — the existing `storage_controller/` crate, reused as an
  internal storage-layer registry and the generation-fencing authority the pageserver
  and safekeeper binaries already speak to. It runs *under* yblight-controller as a
  managed service (§11.1): never yblight-controller's state store, never exposed to
  users.
- **endpoint_storage** — YB Light's small standalone store for endpoint-scoped scratch
  data (LFC prewarm/offload state today). Also a managed service (§11.5); its contents
  are performance state, not correctness state.

All three persist to the placement group's universe over YSQL, so the global tier adds
no external dependency beyond the universe itself (principle 2). The storage controller
and endpoint_storage are *managed* by yblight-controller — it deploys, supervises, and
fronts them; §11 covers what that posture means, what of the storage controller is
reused versus left inert, and the open questions.

### 5.2 Per-endpoint services

Each endpoint is a self-contained YB Light instance running a handful of services:

1. **pg / compute** — the Postgres compute layer users connect to.
2. **safekeeper** — WAL durability.
3. **pageserver** — page materialization/serving.

These services are managed by **compute_ctl**, which is yblight-controller's single point of
contact inside an endpoint: yblight-controller talks to compute_ctl; compute_ctl manages the
service processes. yblight-controller does not manage the individual services directly.

In addition, every endpoint runs an **OpenTelemetry (otel) collector** for metrics
collection. The otel configuration is part of the endpoint spec that yblight-controller owns and
distributes — observability is provisioned with the endpoint, not bolted on after.

### 5.3 Endpoints are stateless

All durable data lives in YBDB. An endpoint's access to that data is purely
**configuration** — the YBDB connection in its spec, plus its (tenant_id, timeline_id)
binding. Nothing on the endpoint's node or pod is precious: if an endpoint goes away,
creating a new one with the same spec — on a completely different node, pod, or even
runtime — serves exactly the same data.

Statelessness is a property of **YB Light's own design**, owned and guaranteed by
YB Light, not something yblight-controller provides or enforces — the control plane assumes
it. (The safekeeper is actively being updated to fully deliver this guarantee.)

This property does a lot of work, and the rest of the design leans on it:

- **Replacement is the recovery primitive.** A failed endpoint is not repaired; it is
  recreated. There is no data to rescue, migrate, or resync.
- **Scheduling is unconstrained by data locality.** Endpoints can be placed wherever
  capacity exists; nothing pins them to particular hardware.
- **Scale-to-zero falls out for free.** Removing every endpoint from a PG cluster
  loses nothing (§4.4); recreating one later picks the data back up.
- **Upgrades can be replace, not mutate** (§7): bring up a new endpoint at the new
  version, cut over, retire the old one.

The corollary obligation for yblight-controller: the endpoint **spec** is the durable artifact.
Everything needed to recreate an endpoint from nothing — YBDB connection config,
timeline binding, role, version, TLS/cert references, otel config — must live in
yblight-controller's database, never only on the endpoint itself.

Per-endpoint configuration that yblight-controller owns and tracks includes (non-exhaustive):

- the YBDB connection config through which the endpoint accesses its data;
- which PG cluster / timeline it serves, and whether it is the write endpoint;
- TLS state (enabled/disabled) and the certificates backing it (§7);
- software version (for upgrades, §7);
- otel configuration;
- the placement group's endpoint_storage address and the endpoint-scoped access
  token for it (§11.5).

### 5.4 Endpoints are self-contained blobs

An endpoint is packaged and deployed as a single, self-contained software blob —
compute, safekeeper, pageserver, compute_ctl, and the otel collector travel together.
On **Docker** (the first implemented runtime, §5.6) this is **one container** — the
single-container endpoint image (`deploy/endpoint`), whose processes are brought up
and supervised by a small `supervisor.py` under tini. On **Kubernetes** the same
blob is **one pod per endpoint**; on VMs, a handful of processes on the host.

The unit of deployment, scheduling, replacement, and upgrade is the whole endpoint
— never an individual service inside one.

### 5.5 Delivering the endpoint spec to compute_ctl

yblight-controller is the source of every endpoint's spec. It assembles the spec from its
state store (§5.3, §10) — YBDB connection, the `(tenant_id, timeline_id)` binding, role,
version, TLS/cert references, otel config, the endpoint_storage address + scoped token
(§11.5) — and hands it to that endpoint's compute_ctl, the single point of contact (§5.2).
compute_ctl accepts a spec by three transports that all carry the **same payload** (one
config object: the spec plus the TLS material and the JWKS used to authenticate its own
API); they differ only in delivery and trust, not content:

- **File** — write the spec to disk and point compute_ctl at it. This is what
  `yb_light_local` does today; it has no auth and no live-update path, so it suits dev,
  not the controller.
- **Push** — `POST /configure` on the endpoint's compute_ctl API. The controller drives;
  compute_ctl reacts.
- **Hosted pull** — the controller *serves* the spec endpoint that compute_ctl fetches
  from at boot (and re-fetches on a refresh), keyed by compute id, with an
  `Empty`/`Attached` handshake so a compute can come up before its spec is assigned.

**Posture: push by default; hosting the pull API is an optional, additive capability.**
Delivering or changing a spec is a mutation, so it is a task (§8) like every other —
idempotent, durable, re-runnable — and the push model maps onto that cleanly: the
controller decides, issues `POST /configure`, and reconciles (principle 6) against the
observed status. Hosting the pull endpoint additionally lets endpoints boot *specless*
and wait for assignment, which pairs with capacity headroom (§4.5) and scale-up from a
pool of pre-warmed computes — the `Empty` state is exactly that "exists but unassigned"
signal. Whichever transport is used, this is an **internal** seam: API-first (§3) governs
the controller's own user-facing surface, not the compute_ctl wire it speaks — the same
stance taken for the managed services in §11.

Both directions run on the control-plane signing key (§9.4): the controller signs the
JWTs compute_ctl verifies on inbound `/configure`, and, if it hosts the pull endpoint, it
verifies the token compute_ctl presents when fetching. The exact endpoint shape and the
push-vs-pull choice per runtime are deferred to the controller's API design doc; what is
fixed here is that the spec is one artifact the controller owns and can deliver by any of
these routes without reshaping it. See
[`compute_ctl.md`](../architecture/compute_ctl.md) for the compute-side mechanics.

### 5.6 Provisioning endpoints: the `deploy.Deployer` interface

Launching the endpoint blob (§5.4) on a concrete runtime is the one runtime-specific
step (principle 3), so it sits behind a single Go interface, `deploy.Deployer`
(`amp/internal/deploy`):

```go
CreateEndpoint(ctx, EndpointSpec) (Endpoint, error)  // launch, wait until serving, return host:port
DeleteEndpoint(ctx, id) error
```

The create-database workflow (§6) calls it; nothing else in `core` or the API
handlers knows how an endpoint is launched. A new runtime is a new implementation of
this interface, not a change to the workflow.

- **Docker (implemented).** `DockerDeployer` runs the single-container endpoint image
  as one container, over the Docker daemon socket. The controller and the endpoints it
  launches share a **user-defined Docker network**, so the storage controller (§11.1)
  reaches an endpoint's pageserver/safekeeper **by container name** (Docker DNS) — not
  a juggled host port. Only the compute's Postgres port is published to the host, and
  the controller returns that `host:port` as the endpoint's address; endpoints reach
  the YBDB universe via the host gateway. This is the runtime exercised by the
  amp-on-Docker e2e (`test/e2e/run-amp.sh`).
- **Kubernetes (next).** A `K8sDeployer` implements the same interface against the
  cluster API — a pod per endpoint, a Service for the address — reusing the
  `deploy/helm/yb-light-endpoint` chart. VMs (via ybops) follow the same shape.

The endpoint spec carried in `EndpointSpec` (tenant/timeline ids, PG version, the YBDB
connection, the storage-controller URL, bootstrap flag) is delivered to the endpoint as
environment today; §5.5's `compute_ctl` transports are the longer-term refinement.

## 6. Core workflows

These are the initial user-facing workflows. Each is intended to be a task built from
re-usable steps (§8); none is a special case.

1. **Create a storage pool** — any user can create a pool and becomes its owner;
   reaching a pool created by someone else requires a share (a pool grant, §10.1).
2. **Create a database** — the primary entry point. In one task this creates the
   **tenant**, its **main PG cluster** (a fresh timeline), and optionally that cluster's
   first (write) endpoint, all under a storage pool the caller can reach. Tenants are
   never created on their own — a database *is* a tenant plus its main cluster. Access
   to the new tenant is then granted per tenant (§10.1).
3. **Branch a database (fork a timeline)** — fork an existing cluster's timeline at a
   point, creating a new branch cluster and timeline linked to its parent, under the
   same tenant (§4.3).
4. **Add/remove endpoints on a PG cluster** — scale endpoints up and down, including
   down to **zero**. Adding a write endpoint is only valid when none exists; removing
   the write endpoint leaves a read-only (or empty) cluster.

Implied operator-facing workflows (capacity headroom, node/pool provisioning, universe
creation through YBA) follow the same pattern but are not user-facing.

**Implemented today (create-database).** `POST .../timelines` runs workflow 2 end to
end inside the controller: it creates the tenant + main timeline (state store),
registers the tenant with the managed storage controller (§11.1), and provisions the
write endpoint through the deployer (§5.6), returning the endpoint's connection
address. It currently runs **synchronously** (the call blocks while the endpoint comes
up) rather than through the write-ahead task framework — moving it onto a task
(principle 5, §8) so it is async, durable, and resumable is the planned next step. See
[`yblc-apis.md`](yblc-apis.md) for the API shape.

## 7. Longer-term requirements (designed for now, built later)

These are not in the first milestone, but the model above must not paint us into a
corner on any of them:

- **Endpoint upgrades** — endpoints carry a tracked software version; upgrade is a
  task that rolls an endpoint (or a cluster's endpoints) to a new version. Because
  endpoints are stateless (§5.3), the default strategy is replace-and-cut-over rather
  than in-place mutation. For read-only endpoints this is seamless. For the write
  endpoint, the single-writer invariant (§4.4) means the old and new endpoints can
  never both be writers: cut-over is demote-old-then-promote-new, which implies a
  brief write-unavailability window. If the task is interrupted between the two
  steps, the cluster is left writer-less until the task resumes (§8) — the invariant
  is never violated; availability is the trade-off.
- **TLS enable/disable per endpoint** — TLS is a per-endpoint property that can be
  toggled through the API.
- **Certificate tracking** — yblight-controller tracks the certs backing each TLS-enabled
  endpoint (issuer, expiry, rotation state) so rotation can be driven as a task.
  Rotation of these client-facing TLS credentials is a requirement, not an
  option: connections *to* endpoints must survive cert turnover.
- **Internal token rotation** — the machine-plane JWTs yblight-controller mints (§9.4)
  expire and must be rotatable without recreating endpoints — the
  endpoint_storage access token (§11.5) in particular. Like certificate
  rotation above, this is a requirement of the model; the mechanism is
  deliberately not designed yet.
- **Multi-cloud, multi-runtime** — endpoints on VMs (via ybops + node-agent) and on
  Kubernetes in multiple clouds, behind the same provisioning interface.

## 8. Task framework

Every mutation (principle 5, §6, §7) runs as a **task**. The guarantees we require:

- **Idempotency is the contract.** Every step must be safely re-runnable: a task can be
  retried after failure, process restart, or partial execution, and converge to the same
  result. Steps check current state before acting ("create if absent", "ensure configured
  as X") rather than assuming a clean slate.
- **Tasks are composed of re-usable steps.** "Create endpoint" appears inside
  create-database, branch, and scale-up flows as the same step, not three copies.
- **All workflows in §6 and §7 are tasks — and so are reconciliation repairs.** When
  reconciliation detects drift (principle 6), the repair is a task like any other. There
  is no second code path for mutations.
- **Tasks are durable and observable.** Task state lives in yblight-controller's state
  store and survives restarts; in-flight tasks resume or retry rather than vanish. Because
  task state is in the universe rather than in any one process, a crashed controller
  doesn't strand its in-flight work: another instance can reclaim and finish it, which is
  what makes the control plane highly available rather than a single point of failure.

We implement these guarantees directly on the state store — there is no external
durable-execution runtime. The mechanism is a write-ahead task log.

### 8.1 A write-ahead task log

The framework is **write-ahead**: when a task starts, it records the ordered list of steps
it intends to run *before* it runs any of them, then walks that list, marking each step as
it completes. The plan is durable intent; execution catches up to it. This is the same
discipline as the storage layer's WAL — log what you will do, then do it — applied to
control-plane operations.

Two record types, both rows in yblight-controller's schema (§10):

- **`task`** — one row per initiated operation (create database, branch, scale, a
  reconciliation repair). This is the **user-visible** unit: the API exposes task status
  so a caller can monitor or poll a long-running operation. It carries the task kind, a
  coarse status, the resource it targets, the originating request, and (on failure) the
  error.
- **`task_step`** — the ordered steps of one task, one row each. A step records its
  position in the sequence (`ordinal`), which re-usable step it is (`kind`), its own
  status, its input arguments, and its result. These rows **are** the write-ahead log:
  written when the task is planned, updated as execution progresses.

Execution is then a simple loop: take the lowest-`ordinal` step that is not yet
`succeeded`, run it, record its result and status, repeat until the list is exhausted or a
step fails. On crash and reclaim (§8.2), the new owner runs the *same* loop — completed
steps are skipped because their status already says `succeeded`, so work resumes at the
first unfinished step instead of from the top.

A sketch of the DDL (authoritative DDL lives with the code and its migrations):

```sql
CREATE TYPE task_status AS ENUM ('pending', 'running', 'succeeded', 'failed');
CREATE TYPE step_status AS ENUM ('pending', 'running', 'succeeded', 'failed');

CREATE TABLE task (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    kind         text        NOT NULL,        -- e.g. 'create_database', 'branch', 'scale'
    status       task_status NOT NULL DEFAULT 'pending',
    tenant_id    uuid,                         -- the resource the task is about, for API
    target_ref   text,                         --   monitoring; nullable (a task may create
                                               --   its own target, absent when planned)
    params       jsonb       NOT NULL,        -- the request that created the task
    error        text,                         -- set when status = 'failed'
    owner        text,                         -- which instance holds the task (§8.2)
    lease_expiry timestamptz,                  -- ownership lease; expiry => reclaimable
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE task_step (
    task_id    uuid        NOT NULL REFERENCES task(id),
    ordinal    int         NOT NULL,          -- execution order within the task
    kind       text        NOT NULL,          -- which re-usable step (e.g. 'create_endpoint')
    status     step_status NOT NULL DEFAULT 'pending',
    args       jsonb       NOT NULL,          -- inputs, written when the task is planned
    result     jsonb,                          -- outputs, written when the step succeeds
    error      text,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (task_id, ordinal)            -- steps are ordered, unique within a task
);

-- the claim/resume query only ever scans unfinished tasks
CREATE INDEX task_runnable ON task (status) WHERE status IN ('pending', 'running');
```

### 8.2 Ownership and recovery

A task is owned by one instance at a time. Claiming a task is an atomic transition
(`pending` → `running`, stamping `owner` and `lease_expiry`), so two instances never run
the same task concurrently. If the owner crashes, its lease expires; another instance then
reclaims the task — re-stamping `owner` and resuming from the first non-`succeeded` step
(§8.1). Because every step is idempotent, replaying the step that was in flight when the
owner died converges rather than double-applying.

The catalog of step kinds — which steps exist and their exact arguments — belongs to the
task framework's own design doc. What is fixed here is the shape: a user-visible `task`
plus its ordered, idempotent `task_step` rows, planned write-ahead and executed by a
resume-safe loop under a lease.

## 9. Deployment configuration

The universe's **YSQL connection details are required configuration** in every
deployment: yblight-controller is always given a connection string to its universe directly,
and refuses to start without one. Connection details are static — if the universe's
topology changes, whoever manages the deployment must update the connection string.

YBA access is **optional** and strictly additive. It is never part of the path to
the state store; with or without it, the controller reaches YBDB the same way.

### 9.1 YBA mode

yblight-controller is given a YBA address, an API token, and the customer/universe
UUIDs that scope it:

- **Universe management, not connectivity.** YBA access enables the operations that
  genuinely require YBA — universe management (§2). The controller does not use it
  to reach the state store, and keeping connectivity independent of YBA also means
  a YBA outage cannot keep the controller from its own state. The configured access
  is the YBA API in general, not a single purpose — other uses may be added later.
- **Scoped by customer and universe UUID.** yblight-controller is bound to a single
  customer UUID and universe UUID, and that boundary is enforced from both sides:
  the API token it is given should carry the least privilege YBA can express
  (scoped to that customer), and the controller itself refuses to address any
  universe other than its configured UUID — even if the token would allow more.
  A compromised or misconfigured controller must not be able to reach other
  customers' universes through YBA.

### 9.2 Direct mode (no YBA)

When YBA access is not provided, connectivity is unchanged — the controller runs
from the same configured connection string.

- The state store layout, task framework, and API behavior are identical in both
  modes. Code above the connection layer must not know or care which mode it is
  running in.
- Anything that genuinely requires YBA — universe operations (§2) — is unavailable.
  The universe is assumed to be provisioned and managed by something else, and the
  controller treats it as given.

This is the natural mode for development and testing without a YBA, and for running
against a universe whose management lives outside the controller's view.

### 9.3 Bootstrapping

Standing up a placement group is **runbook-driven** — operator-run for now, with the
agent layer as a candidate to automate it later. The runbook (a deliverable of this
project) covers both connection modes:

- **YBDB via yugabyted** — for direct mode, start YBDB with `yugabyted`, covering
  both Docker and standalone-on-a-VM deployment.
- **YBA** — for YBA mode, stand up YBA and its universe following the public YBA
  documentation: <https://docs.yugabyte.com/stable/yugabyte-platform/>. We do not
  duplicate YBA install/operations docs in this project.

Once the database is reachable (either way), yblight-controller's first boot creates
its control-plane database and schema (§10) and the placement group is ready for
storage pools.

### 9.4 Control-plane signing key

yblight-controller is the sole issuer of the machine-plane tokens: the JWTs verified
by compute_ctl, endpoint_storage (§11.5), and the storage controller (§11.1),
and the data-plane tokens where pageserver/safekeeper auth is enabled. The
private signing key is **deployment configuration, provided as a file path** —
the same shape on every runtime: a file on a VM's disk, a mounted Secret on
Kubernetes. yblight-controller will start without it, generate its own key, and 
store that on the file system. This will be useful for testing - though should
not be used in production

Private key material never lives in the state store. The state store records
public halves and lifecycle metadata only; verifiers receive public keys
through what they already consume — the JWKS embedded in each endpoint spec
(§5.3) and the managed services' own configuration. User-facing API
authentication is a separate, deferred concern (§12) and must not share this
key.

## 10. State tracking

yblight-controller does not run its own database. Its state store is the **placement group's YBDB
universe**, accessed over YSQL — the same universe whose PG clusters it manages. Two
consequences worth stating up front:

- **Isolation.** yblight-controller's control-plane tables live in their own database within the
  universe, with RBAC keeping them strictly apart from tenant data. Tenants must have
  no path to control-plane state.
- **Coupling.** The control plane's availability is tied to its universe's
  availability. Within a single placement group this coupling is mostly tolerable —
  if the universe is down, the data plane is down anyway — but it means yblight-controller cannot
  be the thing that repairs a fully-unavailable universe. That responsibility sits
  with YBA (or whatever manages the universe in direct mode, §9.2) and, later, the
  higher-level agent layer.

The state store tracks both halves of the model:

- **Logical:** storage pools (each with an owner), tenants, and users — users are either
  **admin** (placement-group-wide, all pools and tenants) or non-admin. A non-admin
  reaches a pool they created (as owner) or one **shared** with them via a **pool grant**,
  and reaches a tenant through **per-tenant grants** (read-only or read-write access to
  specific tenants), see §10.1 — plus the RBAC bindings at each level.
- **Physical:** PG clusters and their timelines (which is the tenant's main cluster, plus
  fork ancestry), and endpoints (timeline binding and r/w vs. read-only role, exposed
  ports/address, placement, version, TLS/cert state, otel config). Endpoint records must
  be complete enough to recreate the endpoint from scratch (§5.3).
- **Operational:** tasks and their ordered task steps (§8), with enough state to resume
  them.

Records carry intent (desired state) distinctly from observed state, so
reconciliation has something to reconcile against.

### 10.1 Entities

```
user ──< user_pool_grant >── storage_pool ──< tenant ──< pg_cluster (timeline) ──< endpoint
 │           (a "share")          ▲             │         one main cluster + its branches
 │       owner_user_id ───────────┘             │
 └──< user_tenant_grant >───────────────────────┘   (role: read_only | read_write)

admin users skip both grants; a pool's owner needs no share for that pool
```

- **storage_pool / tenant** — the ownership hierarchy. A storage pool maps roughly to a
  **Meko datapack** and carries an **owner** (`owner_user_id`, the user who created it);
  it is *not* an "org" (that level is undefined for now, §12). Names are unique within
  their parent scope (pool within placement group, tenant within pool), not globally, so
  tenants can't collide with or probe each other's names.
- **pg_cluster** — carries its timeline (the two are synonyms, §4.3), keyed by the
  composite **(tenant_id, timeline_id)**; plus name (unique within its tenant), an
  `is_main` flag (exactly one main cluster per tenant, created with the tenant, §6; the
  rest are branches), fork ancestry (parent timeline + fork point), and lifecycle state.
  There is no separate timelines table.
- **endpoint** — durable **record**, ephemeral **instance**. The record is what users
  address through the API ("add/remove endpoint"), by id — endpoints have no names.
  The record carries the spec: timeline
  binding, `read_write` | `read_only` role, version, TLS/cert ref, otel config. Its
  runtime columns — state, address, exposed ports, runtime ref (pod/VM handle),
  last-seen — are observed data. Replacing a failed instance keeps the endpoint id
  (new pod, same record); scale-to-zero deletes records, not just instances.
  Address + ports + TLS state form the connection details returned to users.
- **user / user_pool_grant / user_tenant_grant** — users are either **admin** (all
  pools and tenants, no grants needed) or non-admin. Any user may **create** a storage
  pool and is recorded as its **owner**, which needs no grant. To reach a pool they did
  not create, a non-admin needs a **pool grant** (the "share"), keyed
  `(user_id, storage_pool_id)`, that lets them create and see tenants in it; and
  **tenant grants**, keyed `(user_id, tenant_id)` with a `read_only` | `read_write` role,
  that govern access to specific tenants. Effective access is the union of the tenant
  grants. Revocation is dropping the relevant row; a non-admin with zero grants (and no
  pools of their own) is valid and can access nothing. The tenant is the finest grain of
  user access (decided).
- The **placement group has no table** — a controller instance *is* one placement
  group (§4); its identity and connection-mode configuration are deployment config,
  not rows.

### 10.2 Invariants the schema enforces

Per principle 4, the load-bearing rules are expressed in the schema itself, not only
in API-layer validation (full DDL lives with the code and its migrations):

1. **Timelines are tenant-scoped structurally.** Every timeline reference — endpoint
   binding, fork parent — is a composite foreign key carrying `tenant_id`, so
   cross-tenant timeline use (including cross-tenant forks) is unrepresentable.
2. **Single writer per PG cluster.** A partial unique index —
   `UNIQUE (tenant_id, timeline_id) WHERE role = 'read_write'` — so two write
   endpoints cannot exist no matter what code path tries.
3. **One main cluster per tenant.** A partial unique index on `pg_cluster` —
   `UNIQUE (tenant_id) WHERE is_main` — so a tenant cannot acquire a second main
   cluster; every other cluster under it is a branch.
4. **Access is explicit rows.** Pool grants key on `(user_id, storage_pool_id)` and
   tenant grants on `(user_id, tenant_id)`; a grant at either level either exists or it
   doesn't. A pool's `owner_user_id` is access in its own right and needs no grant row.
5. **Deletes are restricted, not cascaded.** Removing a pool with tenants, a tenant
   with clusters, a cluster with endpoints, or a cluster whose timeline has fork
   children is a constraint violation; teardown happens top-down through tasks,
   never via `ON DELETE CASCADE` surprises.

## 11. Managed services: the storage controller and endpoint_storage

yblight-controller is not the only control-plane process in a placement group: two
existing yb-light services ride along — the **storage controller** and
**endpoint_storage**, the other two global services (§5.1). Both run *under*
yblight-controller as **managed services**:
yblight-controller deploys them (one of each per placement group), supervises their
lifecycle — start, health, upgrade, through tasks like every other mutation —
and fronts them completely. Their APIs are internal; endpoints and
yblight-controller itself are their only callers, and API-first (§3) applies to
yblight-controller's surface, not theirs. Because they ship inside the controller's
own deployment, they add no new external dependency (principle 2): like
yblight-controller, they persist to the universe over YSQL.

### 11.1 The storage controller

yb-light already ships a storage controller (the `storage_controller/` crate): an
HTTP service persisting to the universe over YSQL, speaking a protocol the
pageserver and safekeeper binaries already implement. Today it is driven by
`yb_light_local`. yblight-controller takes the same posture, with a tighter scope.

**Decision: reuse it as an internal storage-layer registry — never as
yblight-controller's state store, and never exposed to users.** This is principle 7
applied: the hard, proven part is the fencing/registry protocol the data-plane
binaries already speak, and we do not rewrite it.

**How the controller runs it ([#94](https://github.com/yugabyte/yb-light/issues/94)).**
yblight-controller owns the storage controller as a child process it fully
manages. A generic process supervisor (`amp/internal/process`) starts the
binary, streams its stdout/stderr into the controller's structured log,
restarts it with capped exponential backoff if it exits on its own, and stops
it cleanly on shutdown — SIGTERM first, escalating to SIGKILL after a grace
period. The supervisor's `Manager` runs any number of managed processes; the
storage controller is the first. The storage-controller specifics — the binary
path, the `--listen` / `--database-url` / `--dev` arguments, and the `/ready`
readiness probe — live in `amp/internal/storagecontroller`, which also holds
the thin internal HTTP client yblight-controller uses to drive timeline
management (create/delete/describe over the storage controller's
`/v1/tenant/{tenant}/timeline` surface). The manager runs alongside the API
server and the task worker in the `serve` loop, gated on `--storage-controller`;
when disabled (the default for the base skeleton, so the controller still runs
without the binary present) the controller serves without it. Driving the
lifecycle *through tasks* (§8) — start and upgrade as durable, idempotent
operations rather than a process spawned at boot — is the next step; #94 lays
the supervision foundation it builds on.

**Packaging.** Because yblight-controller owns the storage controller's
lifecycle, the two ship together: `make amp-bundle` lays the `amp` and
`storage_controller` binaries side by side in one directory, and the
`deploy/amp` container does the same under `/opt/yb-light-amp`. yblight-controller
resolves a bare storage-controller binary name against its *own* directory
first, so the bundled binary is found with no path configuration (falling back
to `PATH`); an explicit path is honored verbatim. This keeps "one deployment, no
new external dependency" (§11) literally true — the managed service travels
inside the controller's own image.

### 11.2 What is reused, what is not

| API surface | What it does | Used by yb-light today | yblight-controller posture |
|---|---|---|---|
| Upcalls (`/upcall/v1/*`) | Generation fencing: `re-attach`/`validate` at pageserver boot | Yes — every pageserver start | **Reuse** — boot-critical (§11.4) |
| Node registry (`/control/v1/node*`) | Register/describe/configure pageservers | Yes | **Reuse** |
| Safekeeper registry (`/control/v1/safekeeper*`) | Upsert, list, scheduling policy | Yes | **Reuse** |
| Timeline bookkeeping | Timeline create/register/delete, branch-name mappings | Yes | **Reuse** |
| Generation debug ops (`attach-hook`, `inspect`) | Issue/inspect generations | Yes | **Reuse** (internal/ops only) |
| Compute-endpoint registry (`/control/v1/endpoint*`) | Opaque-spec endpoint rows | Yes — built as a shim for `yb_light_local` | **Not reused** — superseded by yblight-controller's endpoint records (§10.1); an opaque spec cannot carry the schema-enforced invariants of §10.2 |
| Tenant placement/sharding, policies, time travel, pageserver passthrough | Upstream multi-tenant placement machinery | Dormant | **Not used** — dead weight in the per-timeline topology |

### 11.3 Constraints that make reuse safe

- **Passive registry, not a second reconciler.** Per principles 5 and 6,
  yblight-controller is the only mutation driver and the only reconciler. The storage
  controller's autonomous behaviors (heartbeat-driven state transitions,
  scheduling/placement) must be audited and disabled or rendered inert.
- **yblight-controller's records never live in it.** Tenancy, PG clusters, endpoints,
  and tasks live in yblight-controller's own schema (§10). The storage controller keeps
  only storage-layer bookkeeping: generations, node and safekeeper rows,
  timeline rows.
- **Replaceable by construction.** Nothing in yblight-controller may grow roots into it
  beyond the upcall/registry contract, so folding the live slice into
  yblight-controller later stays a contained change.

Generations remain load-bearing in our topology: replacement is the recovery
primitive (§5.3), so an old and a new pageserver for the same timeline can
briefly coexist during a replace — generation fencing is what keeps the stale
one from corrupting storage state.

### 11.4 Open questions (for the storage-controller design doc in `designs/`)

- **Who serves the upcall API.** The pageserver inside an endpoint must reach
  `/upcall/v1/*` at boot to get fenced. Either the shared storage controller
  serves it — and endpoint blobs gain a second boot-critical dependency beyond
  compute_ctl (§5.2) — or yblight-controller absorbs the small upcall surface itself and
  the separate service is retired. The second option collapses back to a single
  service and a single source of truth; it must be costed before the first
  milestone hardens the dependency.
- **Scale shape.** One pageserver per timeline means node count tracks endpoint
  count, with register/deregister churn at create/destroy and scale-to-zero —
  inverted from upstream's assumption of a small fleet of long-lived
  pageservers. Heartbeating load and registry growth need validation.

### 11.5 endpoint_storage

The second managed service. endpoint_storage is yb-light's small standalone
store for endpoint-scoped scratch data (today: LFC prewarm/offload state), and
it stays a standalone app — one instance per placement group, persisting to the
universe like everything else (by default colocated in the same blob storage
the pageserver uses).

It is not optional plumbing: every compute spec yblight-controller builds carries the
endpoint_storage address plus an access token scoped to that endpoint's
(tenant, timeline, endpoint), so provisioning an endpoint implies a reachable
endpoint_storage. yblight-controller owns both halves: it runs the service, and it
mints the scoped tokens, signed with the control-plane key (§9.4); their
rotation is a stated requirement (§7), not yet designed.

Its contents are performance state, not correctness state — losing them costs
cache warmth, nothing else — so it inherits the replacement-is-recovery posture
(§5.3) unconditionally.

## 12. Out of scope (for now)

- **UX** — CLI/console design. API only.
- **Multi-placement-group management** — the higher-level "agent" layer that manages
  fleets of control planes. yblight-controller's job is to expose an API good enough for that
  layer to build on.
- **An org layer above storage pools** — storage pools map roughly to Meko datapacks
  (§4.1) with no grouping above them; what an "org" is, and how pools roll up into one,
  is undefined for now.
- **Task framework step catalog** — the framework's shape is fixed here (§8); the
  inventory of step kinds and their arguments is a separate design doc.
- **VM runtime** — designed for, not built; Kubernetes first.
- **Billing/metering, quotas** — will build on the tenancy model later.
- **API authentication** — the user/grant model (§10.1) is designed; how callers
  authenticate as users is not.
- **Timeline data GC** — deleting a PG cluster removes control-plane records;
  reclaiming the timeline's physical data in YBDB is a future design.
- **Data-plane credential isolation** — pageservers and safekeepers across all
  tenants currently share one set of universe credentials on the shared storage
  tables; tenant-scoped data-plane credentials (or row-level security) are a
  future design.

