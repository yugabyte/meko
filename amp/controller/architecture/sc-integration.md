# Storage-layer control (in the controller)

The Go `controller/` owns cluster control for the storage layer, **in-process** —
there is no separate control-plane binary. This doc describes what it owns, the
schema it uses, and the HTTP surface it serves.

## What it owns

- ~~Generation authority~~ — **no longer the controller's.** The per-branch
  fencing counter lives in `amp_storage.branch_state.current_generation`, and the
  pageserver claims its own at startup (`UPDATE ... SET current_generation =
  COALESCE(current_generation, 0) + 1 RETURNING current_generation`). Keeping the
  counter in the same row as the writes it fences means there is no second copy to
  keep in step, and no round trip to issue one.
- **Branch and endpoint bookkeeping** — branch records + name mappings, seeding
  the per-branch state a pageserver needs to attach, and the launcher's durable
  endpoint records. Not a registry of its own: these are the control plane's own
  `branch` and `endpoint` rows, read and written in the storage layer's rendering.
- **Provisioning** — the whole `yb_storage` database. The controller's
  golang-migrate migrations create the whole `amp_storage` schema — blobs+enum,
  the page-store tables, the WAL mirror, and both per-branch state tables (the
  schema the compute daemons bind at startup — they create none of their own) —
  plus the control-plane schemas below.

## Schema

There is **one** `branch` table and **one** `endpoint` table, in the
`amp_registry` schema of the shared `yb_storage` DB, provisioned by
`controller/migrations/0002_resource_model.up.sql`. The product API and this surface are two
renderings of those same rows, not two copies:

| | product API | this surface |
|---|---|---|
| branch key | `(project_id uuid, branch_id uuid)` | flat 32-lowercase-hex `branch_id` |
| endpoint key | `id uuid` | caller-chosen handle in `endpoint.name` |

The hex form is `replace(branch_id::text, '-', '')` — the same 128 bits, so the
translation is lossless and total and there is nothing to keep in step. A branch
dropped through either surface is the same row with the same `dropped_at` stamp.

Two companion schemas are fixed rather than configurable: `controller` holds the
write-ahead task log, and `amp_storage` holds the data plane. `--db-schema`
(default `amp_registry`) relocates only the resource model.

Because the rows are project-scoped, so is this surface — see the routes below.
`yb_amp_local` provisions its user/pool/project through `/api/v1` at startup.

Dropped vs upstream: the heartbeat/availability loop, the scheduler, tenant
placement/sharding, the `/v1` pageserver passthrough, timeline imports, and
metadata health — none are needed in the per-branch topology.

## HTTP surface

The controller serves the surface on its **API listener** (`:8080`), alongside the
versioned `/api/v1` product API, implemented in
`controller/internal/registry` (store + handlers, routed in `handlers.go`):

| Method + path | Purpose |
|---|---|
| `GET /ready` | readiness (DB reachable + migrations applied) — the one unscoped route, so it answers before a project exists |
| `PUT /control/v1/projects/{p}/branch/{id}/branch_name`, `PUT .../drop` | branch rename / tombstone |
| `GET /control/v1/projects/{p}/timeline_branch_names` | branch-name listing |
| `POST /control/v1/projects/{p}/branch/{id}/record`, `GET /control/v1/projects/{p}/branches` | declare a branch + seed its `amp_storage.branch_state`; list live branches |
| `POST/GET /control/v1/projects/{p}/endpoint`, `DELETE /control/v1/projects/{p}/endpoint/{id}` | launcher endpoint records |

The wire shapes match the launcher client DTOs in `controller/local/client`. In
dev/local the surface is unauthenticated
(the whole controller is, today — auth is deferred, #189).

## Callers

- **`yb_amp_local`** (the dev harness, `controller/local/`) launches the `controller`
  binary in place of the old subprocess and drives this surface over HTTP via its
  controller client (`controller/local/client`) — attach when starting a
  pageserver, branch register/drop, pending-init record/list/delete,
  endpoint create/list/delete. The integration harness (`test/integration/compute/`)
  exercises the same path.
- **Endpoint pods** (Kubernetes/Docker deploy) make **no** calls to this surface.
  They are handed `BRANCH_ID` and `CONTROLLER_URL` and need neither: the branch's
  `amp_storage.branch_state` row is seeded before the pod starts, and the
  pageserver claims its own generation. (`deploy/endpoint/supervisor.py` has no
  `/control/v1` client.)
- **The controller's own task framework** seeds branch state through the same store
  **in-process** (not over HTTP) — the `RecipeStore` port in
  `controller/internal/task` is satisfied by an in-process adapter, and it passes
  the project id along with the branch id because the row it writes is
  project-scoped.

## Why the generation is minted per spawn (not injected at deploy)

The generation is claimed on the **pageserver-spawn path**: the pageserver process
increments the counter itself at boot and takes what the statement returns, before
opening the branch. This is deliberate and load-bearing for fencing — the generation
must advance on **every** pageserver incarnation so that a restarted or rescheduled
pod fences its predecessor. Kubernetes restarts-in-place and node-failure reschedules
happen **outside** the controller's deploy flow (kubelet drives them), so the process
claiming a fresh generation on every boot is exactly what keeps those safe: a
lingering old instance holds a lower generation and its next generation-checked write
aborts (see [pageserver.md](../../compute/architecture/pageserver.md)
§ "Generation fencing").

Rejected alternative (closed #206 / #207 / #212 / #213): have amp mint the generation once
at deploy time and inject it into the pod (a `GENERATION` env value). A kubelet restart
would then **reuse** the injected generation and fail to fence the still-live old
instance — a split-brain / corruption risk. Claiming at process start has the same
per-incarnation property the controller's attach-hook had, without the round trip or
the second copy of the counter. For the same reason, pageserver-node registration stays pod-driven rather than
moving in-process, and there is no liveness heartbeater (nothing consumes node
availability — the `nodes` registry is bookkeeping only).
