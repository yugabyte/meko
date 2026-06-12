# yblight-controller (yblc) — PRD Summary

## The one-paragraph version

**yblight-controller (yblc) is the control plane for YB Light.** It owns the full
lifecycle of everything a customer cares about — tenants, databases (timelines),
the read/write compute "endpoints" that serve them, and the branching between them —
and exposes all of it through a single REST API. It keeps its own state in YBDB
it manages and reuses YB Light's existing storage components rather than 
reimplementing them. YBLC will also be able to integrate with YBA if we want to 
add the ability to scale the universe directly from YBLight Controller.

## 1. Who talks to what: agents, MCP, the Controller, and the database

There are two planes, and the rule is different for each.

**Control plane — always through the Controller.** Anything that changes the world
(create a tenant, create or branch a timeline, add/remove/pause an endpoint) is a
Controller API call. Agents, the MCP server, future automation — they are all just
API consumers of the Controller. 

**Data plane — straight to the endpoint.** To actually run SQL, a client connects 
directly to an endpoint over the normal Postgres wire protocol. The Controller is 
not in this path and never proxies query traffic; the controller can be called 
to get the connection string of an endpoint.

> **Rule of thumb:** *Change the shape of the world → Controller API. Read/write rows
> in a database → connect to the endpoint.* An MCP server typically does both: it issues
> control requests to the Controller, and opens direct SQL connections to endpoints for
> data.

```
  Agents / MCP / automation
        │  (control: create, branch, scale, pause)
        ▼
   ┌─────────────────┐      drives       ┌──────────────┐
   │  YBLC           │ ───────────────▶ │ storage layer │  (internal only —
   │   (the control  │   compute_ctl,    │  + computes   │   never called by
   │     plane)      │   storage ctlr    └──────────────┘   agents directly)
   └─────────────────┘
        ▲
        │  (data: SQL) ── apps/agents connect DIRECTLY to ▶  [ endpoint = Postgres ]
```

## 2. One Controller, or many?

**One Controller per placement group.** A placement group is one YugabyteDB universe 
plus everything running on top of it; the Controller lives *inside* the placement 
group it manages and is the single authority for it.

- A deployment with **N placement groups has N controllers**, each independent and
  authoritative only for its own placement group.
- Coordinating *across* controllers (managing a fleet of placement groups) is the job
  of a future project and not currently in scope of YBLight Controller

## 3. The resource ladder: what each "create" step requires

Resources form a strict hierarchy; you cannot create a level without the level beneath
it. Requirements are cumulative going down the table.

```
Placement Group  (= one YugabyteDB universe, one Controller)
  └── Tenant Pool        ≈ an "org"; the unit of user access
        └── Tenant       a customer/team; the unit of timeline ownership
              └── PG Cluster / Timeline   a database + its data
                    └── Endpoint          a running Postgres serving that database
```

| To create / do… | You need (cumulatively)… | Notes |
|---|---|---|
| **Tenant pool** | **Admin** on the placement group | Pools are how non-admins get any access at all, so creating one is an admin action. |
| **Tenant** | RBAC access to the target pool + the pool exists | Access is currently granted at the *pool* level, not per tenant. |
| **Timeline / PG cluster** | A tenant to create the timeline in | Creating the database also by default creates its first endpoint. |
| **Endpoint** | an existing timeline | A *write* endpoint is only allowed if the database has no writer yet (single-writer rule); all others are read-only. |
| **Branch** (fork a timeline) | an existing parent timeline (same tenant) + a fork point | Produces a new timeline + a new database, with parent/child ancestry tracked. |
| **Pause / resume** an endpoint | an existing endpoint | Pause suspends the running compute; resume brings it back. Resume is an explicit API call — there is no "wake on connect" planned today.  |

Two structural guarantees worth calling out, because they are enforced in the schema
itself, not just in code:

- **Timelines are tenant-scoped.** A timeline is identified by *(tenant, timeline)* and
  is unrepresentable outside its tenant.
- **One writer per database.** A second write endpoint cannot exist no matter what code
  path tries to create it.

## 4. The five operations, step by step

All five are implemented as **tasks** — durable, idempotent, re-runnable workflows built
from reusable steps — so a crash mid-operation resumes rather than leaving a mess.

1. **Create tenant** — caller has pool access → Controller records the tenant under the
   pool. No per-tenant access setup (access is pool-grained).
2. **Create timeline (PG cluster)** — under an existing tenant, the Controller creates a
   new timeline, registers it with the storage layer, creates the database record, and
   (optionally) stands up its first write endpoint.
3. **Create endpoint** — on an existing database, the Controller assembles the endpoint
   *spec* (DB connection, timeline binding, read/write role, version, TLS, telemetry),
   provisions the compute, and hands the spec to that endpoint's `compute_ctl`. A write
   endpoint is rejected if one already exists.
4. **Branch** — fork an existing timeline at a chosen point. This creates a *new*
   timeline plus a new database linked to its parent; optionally stands up an endpoint on it.
5. **Pause / resume** — an API call against an endpoint. Pause suspends the compute (the
   endpoint *record* and its spec survive); resume recreates the running instance from
   that stored spec. Because endpoints are stateless, "resume" is just "recreate from the
   spec we already hold."

> **Important nuance for the resume case:** with an endpoint paused/removed, **nothing is
> listening**, so a connection can't wake it. Scale-up from zero is API-driven only.

## 5. YBLC access is required for control operations

Every lifecycle operation — create a tenant, create or branch a timeline, add/remove an
endpoint, pause/resume, scale — is a **control-plane mutation**, and the only way to
perform one is through the YBLC API. 

Neither the data nor compute layer will have knowledge or access to the functionality
needed to perform these control operations, this lives only in the control plane today.

## What this doc intentionally leaves out

UX (CLI/console), the cross-placement-group agent layer, billing/quotas, end-user API
authentication, and physical data GC are all deferred. The full
`yblight-controller.md` covers the complete model, the
managed-services posture, and the open questions.