# yblc — REST API

**Status:** Draft
**Last updated:** 2026-06-16

How the amp controller's REST API is defined, generated, served, and tested.
The guiding principle is yblight-controller.md §3 (**API-first**, versioned in
the URL path); this doc is the concrete shape of the initial surface
([#92](https://github.com/yugabyte/yb-light/issues/92)).

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

## The surface (#92)

Everything is under `/api/v1`. The error envelope is `{ "error": { "code",
"message" } }` for every non-2xx response.

| Method & path | Operation | Notes |
|---|---|---|
| `POST /users` | createUser | Create a user. No auth. |
| `GET /users/{userId}` | getUser | |
| `POST /users/{userId}/storage-pools` | createStoragePool | Caller (the addressed user) becomes the owner. |
| `GET /users/{userId}/storage-pools` | listStoragePools | The user's **owned** pools (no sharing). |
| `GET /users/{userId}/storage-pools/{poolId}` | getStoragePool | Owner-scoped; 404 if not owned. |
| `POST …/{poolId}/timelines` | createTimeline | Runs the full workflow: tenant + main timeline + storage-controller registration + compute endpoint; returns `(tenant_id, timeline_id)` and the endpoint's `address`. `201` / `404` / `409`. |
| `DELETE …/timelines/{timelineId}` | deleteTimeline | **Logged only** → `202`. |
| `POST …/timelines/{timelineId}/branch` | branchTimeline | **Logged only** → `202`. Branching is explicit. |
| `GET /tasks/{taskId}` | getTask | Status of a write-ahead task. |

### Users and identity (no auth yet)

#92 calls for "a basic notion of users without authentication." A user is just
an identity (`app_user`); a caller **acts as** a user by addressing resources
under `/users/{userId}` — there is no credential. The user/grant model
(yblight-controller.md §10.1) is designed but authentication is a separate,
deferred concern (§12). When auth lands, the acting user moves from the path to
an authenticated principal; the resource model does not change.

Because there is no auth, `createUser` also lets a caller mint an **admin**
(`is_admin: true`) freely. That is intentional for this milestone, not a hole to
fix here — admin gating arrives with authentication.

### Storage pools

Any user can create a pool and becomes its `owner_user_id`. Pool names are
unique across the placement group. **Pool sharing (pool grants) is out of scope
for #92**, so `listStoragePools` returns exactly the pools a user owns, and
`getStoragePool` is owner-scoped (another user's pool 404s rather than leaking
its existence).

### Timelines: create runs the full provisioning workflow; delete and branch are logged-only

**`createTimeline` runs the whole create-an-endpoint workflow, in amp.** It:
1. validates the pool (owner-scoped: `404` if not owned) and creates a tenant
   (a "database", unique name within the pool, else `409`) together with its main
   timeline (`pg_cluster`, `is_main`) — one transaction, `core.Backend.CreateTimeline`;
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
globally-unique pool names, owner-scoped listing, tenant create). The user,
storage-pool, and create-timeline controllers go through it; the delete/branch
timeline controllers do not (above). A controller
that returns an unexpected (non-`echo.HTTPError`) error yields a `500` rendered
in the same `Error` envelope — documented per operation in the spec so the
generated client gets a typed handle for it.

## Testing

Two layers, both in `internal/api/*_test.go`:

- **Controller tests** drive the **generated client** against a real `httptest`
  server with the backend stubbed by `MemBackend` and a fake task store. They
  assert routing, path/body binding, status codes, and response shapes for every
  operation — validating the controllers against the generated contract.
- **Storage-pool flow test** runs the create → list → get workflow a user drives,
  and asserts ownership scoping (a user sees only their pools; cross-user fetch
  404s) — the "no sharing" guarantee.

`TestServesSpecAndExplorer` covers the spec/Swagger-UI/health endpoints.
