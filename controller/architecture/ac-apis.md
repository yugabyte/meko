# ac — REST API

**Status:** Draft
**Last updated:** 2026-06-17

How the amp controller's REST API is defined, generated, served, and tested.
The guiding principle is amp-controller.md §3 (**API-first**, versioned in
the URL path). The initial surface landed in
[#92](https://github.com/yugabyte/yb-light/issues/92);
[#109](https://github.com/yugabyte/yb-light/issues/109) then promoted storage
pools to top-level resources and added paginated list endpoints, which is the
shape described here.

## Spec-driven, OpenAPI v3

The API is **defined by an OpenAPI v3 document**, not by the Go code:
[`controller/internal/api/openapi_spec.yaml`](../internal/api/openapi_spec.yaml) is the
single source of truth. From it, [`oapi-codegen`](https://github.com/oapi-codegen/oapi-codegen)
generates `controller/internal/api/api.gen.go`, which contains:

- the request/response **models** (`User`, `StoragePool`, `Error`, …);
- the **echo strict-server** interface (`StrictServerInterface`) the controllers
  implement, plus the router that binds requests to it; and
- a **Go client** (`ClientWithResponses`) — used by the tests to exercise the
  API exactly as a real consumer would.

Because the controllers implement a *generated* interface, the wire shape cannot
drift from the spec: change the YAML, regenerate, and the compiler points at
every handler that no longer matches.

```
openapi_spec.yaml ──oapi-codegen──> api.gen.go ──┬── StrictServerInterface  (handlers.go implements)
   (source of truth)                             ├── models                 (request/response types)
                                                  └── ClientWithResponses    (tests drive this)
```

Regenerate with `make controller-generate` (wraps `go -C controller generate ./...`). The
generated file is checked in. The codegen config is
[`oapi-codegen.yaml`](../internal/api/oapi-codegen.yaml); the tool is pinned as a
Go tool dependency in `controller/go.mod`, so generation needs no preinstalled binary.

The spec is linted with the rest of the repo's specs by `make lint-openapi-spec`
(redocly). Auth is deferred (below), so the `security-defined` warnings are
expected.

## Served on the API server

The controller hosts both the document and an interactive explorer off the same
server (`internal/api/docs.go`):

- `GET /api/v1/openapi.yaml` — the embedded spec (`go:embed`).
- `GET /api/v1/docs` — **Swagger UI**, the interactive explorer, with its assets
  embedded in the binary (`swaggest/swgui`), so it works with no network access.
- `GET /` — the **admin console** (`internal/api/ui.go`): a single static
  HTML+CSS+vanilla-JS page (`internal/api/ui/index.html`), embedded with
  `go:embed`. It is a pure client of the API below — the home page lists and
  creates users, and selecting a user shows that user's storage pools and their
  timelines and endpoints, with create forms for pools and timelines and a
  per-timeline **branch** form (with an optional fork-LSN), and it shows a
  branch's lineage (which timeline it forked from, and at what LSN). It does the
  owner filtering client-side (the list endpoints aren't owner-scoped yet). No
  build step and no server-side rendering, so it can't drift from the spec.

The home page lists and creates users; selecting one drills into that user's
storage pools, timelines, and endpoints:

| Users (home) | User detail: pools → timelines → endpoints |
|---|---|
| ![Users list and create](screenshots/admin-console-users.png) | ![Storage pools, timelines, and endpoints for a user](screenshots/admin-console-pools.png) |

## The surface

Everything is under `/api/v1`. The resource ladder is **storage pool → timeline
→ endpoint** (the user is not in the path). The error envelope is `{ "error": {
"code", "message" } }` for every non-2xx response.

| Method & path | Operation | Notes |
|---|---|---|
| `POST /users` | createUser | Create a user. No auth. |
| `GET /users` | listUsers | Paginated. |
| `GET /users/{userId}` | getUser | |
| `POST /storage-pools` | createStoragePool | Top-level; the owner is named in the body (`owner_user_id`). |
| `GET /storage-pools` | listStoragePools | Paginated; lists **every** pool (no owner filter yet). |
| `GET /storage-pools/{poolId}` | getStoragePool | By id; not owner-scoped. |
| `POST /storage-pools/{poolId}/timelines` | createTimeline | Creates a tenant + its main timeline and records the storage-controller pending-init recipe; returns `(tenant_id, timeline_id)`. Does **not** deploy an endpoint. `201` / `404` / `409`. |
| `GET /storage-pools/{poolId}/timelines` | listTimelines | Paginated; the pool's timelines (all tenants). `404` if the pool is unknown. |
| `DELETE …/timelines/{timelineId}` | deleteTimeline | **Logged only** → `202`. |
| `POST …/timelines/{timelineId}/branch` | branchTimeline | Forks the parent into a new (non-main) timeline under the same tenant, recording the storage-controller recipe with the ancestor + fork LSN set (default: the parent's latest LSN; override with `fork_point_lsn`). Returns the branch `{tenant_id, timeline_id, parent_timeline_id, fork_lsn}`. Does **not** deploy an endpoint. `201` / `404` / `409`. |
| `POST …/timelines/{timelineId}/endpoints` | createEndpoint | Creates the endpoint and **synchronously deploys** it → `201 {endpoint}` (its `address` set). `501` if `--deploy` is off. |
| `GET …/timelines/{timelineId}/endpoints` | listEndpoints | Paginated; the timeline's endpoints. `404` if the pool/timeline is unknown. |
| `GET …/endpoints/{endpointId}` | getEndpoint | One endpoint (its `address` is set once deployed). |
| `DELETE …/endpoints/{endpointId}` | deleteEndpoint | **Synchronously retires** the endpoint → `204`. `501` if `--deploy` is off. |
| `GET /tasks/{taskId}` | getTask | Status of a write-ahead task. |

### Pagination

Every list endpoint takes `limit` and `offset` query parameters and returns the
items array alongside `total` (the count of **all** matching items, ignoring
`limit`/`offset`) and `count` (the number of items in **this** response). `limit`
defaults to 100 and is clamped to `1..1000`; `offset` defaults to 0 and is
clamped to `>= 0` — out-of-range pagination is clamped, never a `400`. The
clamping lives in the controllers (`pageParams`); the backend applies the
resulting `LIMIT`/`OFFSET` and a separate `COUNT(*)` for `total`.

### Users and identity (no auth yet)

#92 calls for "a basic notion of users without authentication." A user is just
an identity (`app_user`); there is no credential. Storage pools used to be
addressed under `/users/{userId}`, but a pool will eventually be acted on by
more than its creator (pool grants), so #109 made pools top-level: the acting
user is named where it's actually needed — `owner_user_id` on pool create. The
user/grant model (amp-controller.md §10.1) is designed but authentication is
a separate, deferred concern (§12). When auth lands, the owner on create moves
to an authenticated principal; the resource model does not change.

Because there is no auth, `createUser` also lets a caller mint an **admin**
(`is_admin: true`) freely. That is intentional for this milestone, not a hole to
fix here — admin gating arrives with authentication.

### Storage pools

Pools are **top-level** resources, addressed by their own id. `createStoragePool`
names the owner in the body (`owner_user_id`, `404` if that user doesn't exist);
the new pool's `owner_user_id` records it. Pool names are unique across the
placement group. Pool sharing (pool grants) is still deferred, but listing is no
longer owner-scoped: `listStoragePools` returns **every** pool (an owner filter
can come with grants), and `getStoragePool` resolves a pool by id regardless of
who created it.

### Timelines: create + branch persist the resource and its recipe; delete is logged-only

**`createTimeline` records the control-plane resource and the pending-init
recipe.** It:
1. creates a tenant (a "database", unique name within the pool, else `409`)
   together with its main timeline (`pg_cluster`, `is_main`) — one transaction,
   `core.Backend.CreateTimeline`; the pool is top-level, so an unknown pool
   surfaces as `404` (the `tenant.storage_pool_id` foreign key); and
2. records the storage controller's **pending-init recipe**
   (`storagecontroller.Client.RecordPendingTimelineInit`) when a controller is
   configured — the timeline is declared, not yet materialized.

It returns the storage-layer ids `(tenant_id, timeline_id)`. It does **not**
eagerly create the tenant on the controller or deploy an endpoint: compute is a
separate resource (below), and the first endpoint deployed on the timeline
materializes it (the recipe is then dropped). See
[`controller-timeline-create.md`](controller-timeline-create.md).

Compute is a **separate resource**: a caller `POST`s an endpoint on the timeline
(below), which the controller deploys synchronously. Bootstrapping a fresh
tenant's storage (initdb + the storage schema) is still out-of-band — the deploy
path attaches an existing tenant (Mode B), it does not create one.

**`branchTimeline` forks an existing timeline (#130).** The parent is the
timeline in the path; the branch is a new, non-main `pg_cluster` under the
**same tenant**, inheriting the parent's `pg_version` and recording its fork
ancestry (`parent_timeline_id`, `fork_lsn`) — `core.Backend.BranchTimeline`. The
fork point defaults to the parent's latest LSN, read from the storage controller
(`GetTimeline`'s `last_record_lsn`); a caller can pin an earlier point with
`fork_point_lsn`. Like create, it then records the storage controller's
**pending-init recipe**, this time with the ancestor and `start_lsn` set, and the
branch is materialized lazily by the first endpoint deployed on it (the
pageserver clones from the ancestor at `fork_lsn` instead of running `initdb`).
It returns the branch timeline (`201`); a missing pool/parent `404`s and a
duplicate name within the tenant `409`s. *(`last_record_lsn` is the pageserver's
ingested position, which can lag the safekeeper's durable `flush_lsn` that
`yb_light_local` branches at; the controller doesn't expose `flush_lsn`, so an
exact point needs `fork_point_lsn`.)*

**`deleteTimeline` is still logged-only.** It is meant to run through the
write-ahead task framework (amp-controller.md §5, §8), whose step catalog
does not exist yet, so per #92 it **only logs** the request and returns
`202 Accepted`. It bypasses `core.Backend` and doesn't validate the timeline
(that validation belongs to the task steps that will replace the logging); no
`task` row is written, and `getTask` is wired to the store for when it is. Its
only outcomes are `202`, `400`, and `500`.

**`listTimelines` and `listEndpoints` are read-only and paginated.**
`listTimelines` returns the pool's timelines across all its tenants (joining
`tenant` → `pg_cluster` on `storage_pool_id`); `listEndpoints` returns a
timeline's endpoints. The timeline is addressed by `(poolId, timelineId)`, so the
backend first resolves the timeline within the pool — `timeline_id` is a
generated UUID, unique within a pool — and a pool or timeline that doesn't exist
is a `404`.

### Endpoints are deployed synchronously (#108)

`createEndpoint` resolves the timeline within the pool via
`core.Backend.GetTimeline` (`(poolId, timelineId)`, top-level: a missing link is
`404`), inserts the `endpoint` row (recorded first, so the single-writer rule is
enforced before a deploy is spent), then **synchronously deploys** it via the
runtime-agnostic [`deploy.Deployer`](../internal/deploy/) (Docker today,
Kubernetes via Helm), records the resolved `address`
(`core.Backend.SetEndpointAddress`), and best-effort drops the storage
controller's pending-init recipe (the endpoint has now materialized the
timeline). It returns `201` with the deployed endpoint. If the deploy fails the
row is removed (compensation). A timeline admits at most one `read_write`
endpoint — the schema's `one_writer_per_timeline` partial unique index — which
surfaces as `409`. `deleteEndpoint` synchronously retires the endpoint (remove
the running instance + drop the row) and returns `204`. Both refuse with `501`
when the deployer is disabled (`--deploy` off). The Kubernetes deploy mechanism
(the Helm release per endpoint, Mode-B attach) is described in
[`ac-k8s-deploy.md`](ac-k8s-deploy.md); moving the deploy onto the
write-ahead task framework (async + `getTask` polling) is a follow-up.

## Layering

```
internal/api/    HTTP: generated contract (api.gen.go) + controllers (handlers.go),
                 echo wiring (server.go), error envelope (errors.go), docs (docs.go)
        │  implements StrictServerInterface, calls ↓
internal/core/   Domain: Backend interface + PgBackend (YBDB) + MemBackend (tests).
                 ErrNotFound → 404, ErrConflict → 409.
internal/task/   Task store, read by getTask.
internal/deploy/ Kubernetes endpoint provisioner; synchronous deploy/retire (ac-k8s-deploy.md).
```

The `core.Backend` seam is what lets the whole HTTP layer be tested without a
database: `MemBackend` mirrors `PgBackend`'s semantics (owner-existence checks,
globally-unique pool names, top-level + paginated listing, tenant create, and
in-tenant-unique branch names). The user, storage-pool, timeline (create +
branch), and endpoint controllers go through it; the logged-only delete-timeline
controller does not (above). A controller
that returns an unexpected (non-`echo.HTTPError`) error yields a `500` rendered
in the same `Error` envelope — documented per operation in the spec so the
generated client gets a typed handle for it.

## Testing

Two layers, both in `internal/api/*_test.go`:

- **Controller tests** drive the **generated client** against a real `httptest`
  server with the backend stubbed by `MemBackend` and a fake task store. They
  assert routing, path/body binding, status codes, and response shapes for every
  operation — validating the controllers against the generated contract.
- **Storage-pool flow test** runs the create → list → get workflow, asserting the
  top-level behaviour: listing returns every pool, and a pool is fetchable by id
  regardless of its creator.
- **List tests** (`lists_test.go`) cover the paginated lists: the `limit`/`offset`
  contract and clamping, `total` (all matches) vs `count` (this page), and the
  parent `404`s on the nested timeline/endpoint lists.

`TestServesSpecAndExplorer` covers the spec/Swagger-UI/health endpoints, and
`TestServesUI` covers the admin console served at `/`.
