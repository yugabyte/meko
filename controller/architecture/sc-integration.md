# Storage-layer control (in the controller)

The Go `controller/` owns cluster control for the storage layer, **in-process** —
there is no separate control-plane binary. This doc describes what it owns, the
schema it uses, and the HTTP surface it serves.

## What it owns

- **Generation authority** — the per-branch fencing counter. `attach-hook` bumps
  it (`UPDATE registry.branches SET generation = generation + 1,
  generation_pageserver = $node RETURNING generation`) and hands the value to the
  attaching pageserver; the value is baked into `pageserver.toml`. It is monotonic
  across a branch's lifecycle (preserved on detach) so a stale pageserver from a
  prior generation is fenced. `inspect` reads it back.
- **Registries** — pageserver **nodes** (bookkeeping/observability; nothing gates
  on it), **branch** records + branch-name mappings, the **pending-init** recipe
  (a branch declared but not yet materialized), and the compute **endpoint**
  registry (`yb_amp_local`'s durable endpoint records).
- **Provisioning** — the whole `yb_storage` database. The controller's
  golang-migrate migrations create `public.blobs`+enum, all `pageserver.*` and
  `safekeeper.*` tables (the schemas the compute daemons bind at startup — they
  create no schema of their own), and the `registry` schema below.

## Schema

State lives in the **`registry` schema** of the shared `yb_storage` DB, provisioned
by `controller/migrations/0006_registry.up.sql`. Five branch-keyed tables:
`branches` (generation authority + placement), `nodes`, `timelines` (start_lsn +
branch_name), `pending_timeline_inits`, `endpoints`. It is kept separate from the
controller **product** model (`project`/`branch`/`endpoint` by UUID, in the
`controller` schema) — the registry keys on the flat 32-hex `branch_id`.

Dropped vs upstream: the heartbeat/availability loop, the scheduler, tenant
placement/sharding, the `/v1` pageserver passthrough, timeline imports, and
metadata health — none are needed in the per-branch topology.

## HTTP surface

The controller serves the surface on its **API listener** (`:8080`), alongside the
versioned `/api/v1` product API, implemented in
`controller/internal/registry` (store + handlers, routed in `handlers.go`):

| Method + path | Purpose |
|---|---|
| `GET /ready` | readiness (DB reachable + migrations applied) |
| `POST/GET /control/v1/node`, `PUT /control/v1/node/{id}/config` | node registry |
| `POST /debug/v1/attach-hook`, `POST /debug/v1/inspect` | generation authority |
| `POST /debug/v1/branch/{id}/import` | attach an existing branch |
| `POST /control/v1/branch/{id}/register`, `PUT .../branch_name`, `PUT .../drop` | branch bookkeeping |
| `GET /control/v1/timeline_branch_names` | branch-name listing |
| `POST/DELETE /control/v1/branch/{id}/pending_init`, `GET /control/v1/pending_timeline_inits` | pending-init recipes |
| `POST/GET /control/v1/endpoint`, `DELETE /control/v1/endpoint/{id}` | endpoint registry |

The wire shapes match `pageserver_api::controller_api` and the launcher client
DTOs in `control_plane/src/controller.rs`. In dev/local the surface is unauthenticated
(the whole controller is, today — auth is deferred, #189).

## Callers

- **`yb_amp_local`** (the dev harness, `control_plane/`) launches the `controller`
  binary in place of the old subprocess and drives this surface over HTTP via its
  `Controller` client (`control_plane/src/controller.rs`) — node register + attach
  when starting a pageserver, branch register/drop, pending-init record/list/delete,
  endpoint create/list/delete. The integration harness (`test/integration/compute/`)
  exercises the same path.
- **Endpoint pods** (Kubernetes/Docker deploy) reach the surface at the controller's
  own URL (`AMP_DEPLOY_CONTROLLER_URL`) to register their pageserver and take a
  generation from the attach-hook.
- **The controller's own task framework** records/clears pending-init recipes through
  the same store **in-process** (not over HTTP) — the `RecipeStore` port in
  `controller/internal/task` is satisfied by an in-process adapter over the registry
  store.
