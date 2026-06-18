# yblc — REST API

**Status:** Draft
**Last updated:** 2026-06-17

How the amp controller's REST API is defined, generated, served, and tested.
The guiding principle is yblight-controller.md §3 (**API-first**, versioned in
the URL path). The initial surface landed in
[#92](https://github.com/yugabyte/yb-light/issues/92);
[#109](https://github.com/yugabyte/yb-light/issues/109) then promoted storage
pools to top-level resources and added paginated list endpoints, which is the
shape described here.

## Spec-driven, OpenAPI v3

The API is **defined by an OpenAPI v3 document**, not by the Go code:
[`amp/internal/api/openapi_spec.yaml`](../internal/api/openapi_spec.yaml) is the
single source of truth. From it, [`oapi-codegen`](https://github.com/oapi-codegen/oapi-codegen)
generates `amp/internal/api/api.gen.go`, which contains:

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

Regenerate with `make amp-generate` (wraps `go -C amp generate ./...`). The
generated file is checked in. The codegen config is
[`oapi-codegen.yaml`](../internal/api/oapi-codegen.yaml); the tool is pinned as a
Go tool dependency in `amp/go.mod`, so generation needs no preinstalled binary.

The spec is linted with the rest of the repo's specs by `make lint-openapi-spec`
(redocly). Auth is deferred (below), so the `security-defined` warnings are
expected.

## Served on the API server

The controller hosts both the document and an interactive explorer off the same
server (`internal/api/docs.go`):

- `GET /api/v1/openapi.yaml` — the embedded spec (`go:embed`).
- `GET /api/v1/docs` — **Swagger UI**, the interactive explorer, with its assets
  embedded in the binary (`swaggest/swgui`), so it works with no network access.

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
| `POST /storage-pools/{poolId}/timelines` | createTimeline | Runs the full workflow: tenant + main timeline + storage-controller registration + compute endpoint; returns `(tenant_id, timeline_id)` and the endpoint's `address`. `201` / `404` / `409`. |
| `GET /storage-pools/{poolId}/timelines` | listTimelines | Paginated; the pool's timelines (all tenants). `404` if the pool is unknown. |
| `DELETE …/timelines/{timelineId}` | deleteTimeline | **Logged only** → `202`. |
| `POST …/timelines/{timelineId}/branch` | branchTimeline | **Logged only** → `202`. Branching is explicit. |
| `GET …/timelines/{timelineId}/endpoints` | listEndpoints | Paginated; the timeline's endpoints. `404` if the pool/timeline is unknown. |
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
user/grant model (yblight-controller.md §10.1) is designed but authentication is
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

### Timelines: create runs the full provisioning workflow; delete and branch are logged-only

**`createTimeline` runs the whole create-an-endpoint workflow, in amp.** It:
1. creates a tenant (a "database", unique name within the pool, else `409`)
   together with its main timeline (`pg_cluster`, `is_main`) — one transaction,
   `core.Backend.CreateTimeline`; an unknown pool surfaces as `404` (the
   `tenant.storage_pool_id` foreign key);
2. registers the tenant with the managed storage controller
   (`storagecontroller.Client.CreateTenant` → `POST /v1/tenant`), when one is
   configured; and
3. provisions the compute endpoint via the runtime-agnostic
   [`deploy.Deployer`](../internal/deploy/) (Docker today, Kubernetes later),
   persists it (the `endpoint` table), and returns its connection `address` in the
   `Timeline.endpoint` field.

The endpoint it launches materializes the timeline (bootstrap initdb) and — in SC
mode — registers + attaches to the controller for its generation. The call is
synchronous (it blocks while the endpoint comes up); moving it onto the
write-ahead task framework (async + `getTask` polling) is a follow-up. amp's
`/readyz` gates on the managed controller so a caller can't race its startup.

**`deleteTimeline` and `branchTimeline` are still logged-only.** All such
mutations are meant to run through the write-ahead task framework
(yblight-controller.md §5, §8), whose step catalog does not exist yet, so per #92
these two **only log** the request and return `202 Accepted`. They bypass
`core.Backend` and don't validate the timeline (that validation belongs to the
task steps that will replace the logging); no `task` row is written, and
`getTask` is wired to the store for when they are. Their only outcomes are
`202`, `400`, and `500`.

**`listTimelines` and `listEndpoints` are read-only and paginated.**
`listTimelines` returns the pool's timelines across all its tenants (joining
`tenant` → `pg_cluster` on `storage_pool_id`); `listEndpoints` returns a
timeline's endpoints. The timeline is addressed by `(poolId, timelineId)`, so the
backend first resolves the timeline within the pool — `timeline_id` is a
generated UUID, unique within a pool — and a pool or timeline that doesn't exist
is a `404`.

## Layering

```
internal/api/    HTTP: generated contract (api.gen.go) + controllers (handlers.go),
                 echo wiring (server.go), error envelope (errors.go), docs (docs.go)
        │  implements StrictServerInterface, calls ↓
internal/core/   Domain: Backend interface + PgBackend (YBDB) + MemBackend (tests).
                 ErrNotFound → 404, ErrConflict → 409.
internal/task/   Task store, read by getTask.
```

The `core.Backend` seam is what lets the whole HTTP layer be tested without a
database: `MemBackend` mirrors `PgBackend`'s semantics (owner-existence checks,
globally-unique pool names, top-level + paginated listing, tenant create). The
user, storage-pool, timeline, and endpoint controllers go through it; the
delete/branch timeline controllers do not (above). A controller
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

`TestServesSpecAndExplorer` covers the spec/Swagger-UI/health endpoints.
