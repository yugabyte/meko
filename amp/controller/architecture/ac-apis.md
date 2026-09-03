# ac — REST API

**Status:** Draft
**Last updated:** 2026-08-27

How the amp controller's REST API is defined, generated, served, and tested.
The guiding principle is amp-controller.md §3 (**API-first**, versioned in
the URL path). The initial surface landed in
[#92](https://github.com/yugabyte/amp/issues/92);
[#109](https://github.com/yugabyte/amp/issues/109) then promoted storage
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
  clusters, branches, and endpoints, with create forms for pools and clusters and a
  per-branch **branch** form (with an optional fork-LSN), and it shows a
  branch's lineage (which branch it forked from, and at what LSN). It does the
  owner filtering client-side (the list endpoints aren't owner-scoped yet). No
  build step and no server-side rendering, so it can't drift from the spec.

The home page lists and creates users; selecting one drills into that user's
storage pools, clusters, branches, and endpoints:

| Users (home) | User detail: pools → clusters → branches → endpoints |
|---|---|
| ![Users list and create](screenshots/admin-console-users.png) | ![Storage pools, clusters, branches, and endpoints for a user](screenshots/admin-console-pools.png) |

## The surface

Everything is under `/api/v1`. The resource ladder is **storage pool → cluster →
branch → endpoint** (the user is not in the path). The error envelope is `{ "error": {
"code", "message" } }` for every non-2xx response. Every operation that touches
the state store also declares `503` (see Layering below).

**Mutations are asynchronous tasks.** The six world-changing operations —
`createCluster`, `createBranch`, `createEndpoint`, `deleteCluster`,
`deleteBranch`, `deleteEndpoint` — no longer do the work inline. Each runs cheap
synchronous validation, then plans a write-ahead **task** (amp-controller.md §5,
§8; [`ac-tasks.md`](ac-tasks.md)) and returns `202 Accepted` with the `Task`
JSON in the body and a `Location: /api/v1/tasks/{id}` header. The client then
**polls `getTask`** until `status` is `succeeded` (done) or `failed` (read the
`error` field), and reads the resulting ids/`address` back with the relevant
`GET`/list. Because the worker does the work, a name or single-writer conflict
that used to be a synchronous `409` on create is now a task that ends
`status == "failed"` — `409` no longer occurs on these creates. Only cheap,
local checks stay synchronous: `400` (malformed body / bad parameter), `404`
(unknown pool/cluster/branch/endpoint), and `501` (endpoint deploy disabled).
`createUser` and `createStoragePool` are **not** tasks — they remain synchronous
`201` returning the resource.

| Method & path | Operation | Notes |
|---|---|---|
| `POST /users` | createUser | Create a user. No auth. |
| `GET /users` | listUsers | Paginated. |
| `GET /users/{userId}` | getUser | |
| `POST /storage-pools` | createStoragePool | Top-level; the owner is named in the body (`owner_user_id`). |
| `GET /storage-pools` | listStoragePools | Paginated; lists **every** pool (no owner filter yet). |
| `GET /storage-pools/{poolId}` | getStoragePool | By id; not owner-scoped. |
| `PATCH /storage-pools/{poolId}` | updateStoragePool | Rename. Any pool, any time — nothing addresses a pool by name. `409` if another live pool holds the name. |
| `DELETE /storage-pools/{poolId}` | deleteStoragePool | **Synchronous** → `204`; an empty pool owns no compute, so there is no task. `409` while a live cluster remains. Already-deleted clusters do not count: they are tombstoned, and blocking on a tombstone would strand the pool forever. The pool is itself tombstoned (its clusters' FKs still point at it) and drops out of `listStoragePools`; the partial index `storage_pools_name_live` frees its name at once. |
| `POST /storage-pools/{poolId}/clusters` | createCluster | **Async** → `202` + `Task` + `Location: /api/v1/tasks/{id}`. Plans a task that creates a cluster + its default branch and records the controller pending-init recipe (keyed by `branch_id`). Its writer is sized and floored exactly as `createEndpoint` sizes one. Pass `with_writer: true` (optionally `resources` = cpu/memory, validated as `createEndpoint` validates them — a `memory` without an explicit Mi-or-larger unit is a `400`, as is a pair below the memory-per-vCPU floor) to also create and deploy the default branch's read_write endpoint in the **same** task — a one-call "create cluster"; the default (`with_writer: false`) deploys no endpoint (compute is a separate resource). Optional `extensions` (catalog names, e.g. `pgvector`) enables externalized extensions on the default branch — validated against the catalog (unknown → `400`; any with no catalog / Docker driver → `400`) and mounted by every endpoint on that branch, inherited by a fork ([`compute/architecture/extensions.md`](../../compute/architecture/extensions.md)); add more to a branch later with `addBranchExtensions`. Poll `getTask`, then `getCluster`/`listClusters` to read the ids/`default_branch`/`extensions`. Cheap validation is still synchronous: `400` (bad body / unsupported `pg_version`) / `404` (unknown pool). A duplicate cluster name no longer `409`s — it surfaces as a `failed` task. |
| `GET /storage-pools/{poolId}/clusters` | listClusters | Paginated; the pool's clusters, each with its `default_branch`. Scoped to usable clusters by default — failed-creation clusters (orphans left when a `create_cluster` task failed mid-provisioning) are excluded from both the page and its total; pass `include_failed=true` (e.g. from an operator's fleet view) to also surface them so they can be seen and deleted. `404` if the pool is unknown. |
| `GET …/clusters/{clusterId}` | getCluster | One cluster (with its `default_branch` and the default branch's `extensions`). `404` if the pool/cluster is unknown. |
| `DELETE …/clusters/{clusterId}` | deleteCluster | **Async cascade** → `202` + `Task` + `Location`. Plans a `delete_cluster` task that retires every endpoint in the cluster, then drops its branches and the cluster row. Cheap pre-check: `404` if the pool/cluster is unknown. |
| `POST …/clusters/{clusterId}/branches` | createBranch | **Async** → `202` + `Task` + `Location`. Plans a task that forks a parent branch (default: the cluster's default branch; override with `parent_branch_id` to branch off another branch) into a new branch under the same cluster, recording the controller recipe with the ancestor + fork LSN set (default: the parent's latest LSN; override with `fork_point_lsn`). Does **not** deploy an endpoint. Poll `getTask`, then `listBranches`/`getBranch` to read the branch. Cheap validation is synchronous: `400` (bad body / bad or unresolvable `fork_point_lsn`) / `404` (unknown pool/cluster/parent). A duplicate branch name surfaces as a `failed` task, not a `409`. |
| `GET …/clusters/{clusterId}/branches` | listBranches | Paginated; the cluster's branches (default branch + forks). `404` if the pool/cluster is unknown. |
| `GET …/branches/{branchId}` | getBranch | One branch, including its own externalized-extension set (`extensions` — seeded at create on the default branch, inherited by a fork, extended per branch). `404` if the pool/cluster/branch is unknown. |
| `POST …/branches/{branchId}/extensions` | addBranchExtensions | **Async** → `202` + `Task` + `Location`. Plans a task that unions the requested catalog names into **this branch's** externalized-extension set (idempotent) and **redeploys every existing endpoint on the branch** to mount the new set. Validated against the catalog (unknown → `400`; any with no catalog / Docker driver → `400`). Poll `getTask`, then `getBranch` to read the new `extensions`. Optional `ycp_generation` — the reconcile agent's desired-state generation, stamped on the branch when the task completes and echoed as the branch's `ycp_generation`; other callers omit it. **First-writer-wins**: while an add for this branch is in flight, a concurrent one is `409`. `404` if the pool/cluster/branch is unknown. See [`compute/architecture/extensions.md`](../../compute/architecture/extensions.md). |
| `DELETE …/branches/{branchId}` | deleteBranch | **Async cascade** → `202` + `Task` + `Location`. Plans a `delete_branch` task that retires the branch's endpoints, then drops the branch row. Cheap pre-checks: `404` if the pool/cluster/branch is unknown; `409` if the branch has child branches (the parent FK is `RESTRICT`, so the delete would be doomed). |
| `POST …/branches/{branchId}/endpoints` | createEndpoint | **Async** → `202` + `Task` + `Location`. Plans a task that records the endpoint, deploys it, and records its `address`. Poll `getTask`, then `listEndpoints`/`getEndpoint` to read the id + `address`. Optional `disk_type` (`network` default = the configured storage class; `local` not yet supported → `400`). Optional `resources` (`cpu`/`memory` k8s quantities) applied as request == limit → Guaranteed QoS; `memory` needs an explicit Mi-or-larger unit (a bare `4` is four *bytes*) → `400`. Defaults to 2 cores, with memory following CPU at the control plane's memory-per-vCPU ratio (`--deploy-memory-gib-per-vcpu`, 8 GiB/core by default → 16Gi), so naming only `cpu` is enough. That ratio is also a floor: less memory per core is a `400` naming the minimum for the CPU asked for. Externalized extensions are opted into **per branch** (seeded at `createCluster`, extended with `addBranchExtensions`, inherited by a fork), so this endpoint mounts its branch's set automatically — it takes no `extensions` field ([`compute/architecture/extensions.md`](../../compute/architecture/extensions.md)). Optional `pageserver_settings` / `safekeeper_settings` — per-endpoint overrides for the two storage daemons, flat `{key: scalar}` maps whose keys may be **dotted paths** (`tenant_config.enable_gc`, which is how the pageserver's GC knobs are reached at all). Validated against a key set generated from the daemons' own Rust config types, so an unknown key, a wrong type, a non-scalar value or a protected identity/wiring key is a `400`. Applied by the supervisor from the endpoint's row at pod start, not carried on the deploy spec. `501` if `--deploy` is off; `404` if the pool/cluster/branch is unknown. A second `read_write` endpoint (single-writer) surfaces as a `failed` task, not a `409`. |
| `GET …/branches/{branchId}/endpoints` | listEndpoints | Paginated; the branch's endpoints. `404` if the pool/cluster/branch is unknown. |
| `GET …/endpoints/{endpointId}` | getEndpoint | One endpoint (its `address` is set once deployed; `resources` reports the current cpu/memory read from `config`; `version` is what it is recorded as running — the endpoint image tag today — and is absent when unknown, which is every endpoint predating version tracking). |
| `PATCH …/branches/{branchId}/settings` | updateBranchSettings | **Sync** → `200`. Replaces the branch-wide settings named in the body (`pg_settings`, `write_token_settings`) and leaves the rest alone — an omitted field is "do not touch", `{}` clears. `write_token_settings` is the branch's write lease (`ttl_ms`, `renew_seconds`, which must be under it); on the branch because the lease is one row per branch and every endpoint on it contends for that row. The row is the durable answer; the controller then asks each of the branch's endpoints to re-read it. Each endpoint applies what it can in place and restarts only what it must: `reloaded` lists services that took the change while running, `restarted` those bounced, and `restart_forced_by` names the Postgres setting that forced one (Postgres fixes some settings at server start; a storage daemon's config has no reload path at all). A setting Postgres will not take — a name it does not know, a value it cannot parse — is refused before anything is stored: the branch's `read_write` endpoint is asked first, and a `400` names each one with why, storing nothing from the request. With no writer to ask, or one that cannot be reached, the write proceeds and such a setting comes back in `not_applied` as `{name, reason}` instead, never written to the config file (one that contains it does not load, so the compute would not start). Nothing is restarted for a setting that is merely inert, like a compiled-in one. A write that renders identical config does nothing. Why an endpoint could not be told comes back in `unreachable`. `404` if the pool/cluster/branch is unknown. |
| `PATCH …/endpoints/{endpointId}/settings` | updateEndpointSettings | **Sync** → `200`. The same, for one endpoint's `pg_settings` / `pageserver_settings` / `safekeeper_settings`. Postgres settings usually reload; the daemons' always restart theirs. Daemon settings are validated exactly as `createEndpoint` validates them — an unknown key, wrong type, non-scalar value or protected key is a `400`, because a key the daemon does not know aborts its startup. `404` if the endpoint is unknown. |
| `POST …/endpoints/{endpointId}/resize` | resizeEndpoint | **Async** → `202` + `Task` + `Location`. Plans a `resize_endpoint` task that persists the new `resources` (cpu/memory, request == limit → Guaranteed QoS) and resizes the running pod **in place** (k8s `resize` subresource; the Deployment template is deliberately not patched — that would trigger a rollout). An omitted dimension keeps its current value. The task result reports the in-place outcome (`completed`/`pending`/`infeasible`/`unsupported`). **First-writer-wins**: a concurrent resize of the same endpoint gets `409` while one is in flight. The floor applies to the shape the resize would *leave* the endpoint at, not to what the request named, so growing `cpu` alone is refused with a `400` naming the minimum memory for those cores. An endpoint already below the floor — everything created before the rule — is held to "no worse" instead: a resize that does not reduce the memory each core has is allowed, including a shrink toward the floor, and one that lowers it is a `400`. An endpoint whose stored size cannot be read is refused rather than compared. Optional `ycp_generation`, as on `addBranchExtensions`. `400` on a malformed quantity; `501` if `--deploy` is off; `404` if the endpoint is unknown. |
| `DELETE …/endpoints/{endpointId}` | deleteEndpoint | **Async** → `202` + `Task` + `Location`. Plans a `delete_endpoint` task that retires the running instance and drops the row. `501` if `--deploy` is off; `404` if the endpoint is unknown. |
| `POST /endpoint-upgrades` | upgradeEndpoints | **Async fan-out** → `202` + `Task` + `Location`. Plans an `upgrade_endpoints` task that moves every live endpoint — or the `endpoint_ids` subset — onto `version` (the endpoint image tag today), `max_parallel` at a time (default 4, `1..50`). One parent task owns an `upgrade_endpoint` child per endpoint and runs them itself, so a roll of N endpoints is ONE row in `GET /tasks`; drill in with `listTaskSubtasks`. The task result carries one line per endpoint (`upgraded`/`skipped`/`failed`/`unattempted`). Fail-fast: the first child failure stops new children starting, in-flight ones finish, and the task fails — but a child refused its endpoint lock is `unattempted`, not a failure. **One roll at a time**: a second roll planned while an `upgrade_endpoints` task is non-terminal gets a `409` — a query over `tasks` at plan time, with the no-wait `fleet:endpoints` lease deciding a race that beats it (that roll fails naming the roll that won). Whole-fleet rolls skip endpoints already recorded at the target; an endpoint named in `endpoint_ids` is never skipped, which is how a partial roll is re-run. Only the request is validated: an unknown endpoint id is a `skipped` line, not a `404`. `400` on a missing `version` or an out-of-range `max_parallel`; `409` while a roll is in flight; `501` if `--deploy` is off. Top-level because the operation is fleet-scoped. |
| `GET /tasks` | listTasks | Paginated; write-ahead tasks newest-first (`created_at` DESC), with optional `status`, `kind`, and `cluster_id` filters. |
| `GET /tasks/{taskId}` | getTask | Status of a write-ahead task (`pending`/`running`/`succeeded`/`failed`; `error` set on failure; `result` carries the final step's outcome, e.g. a resize's `in_place_resize`). `ycp_generation` is the desired-state generation the task applies, for one planned from YCP desired state — a `failed` task has **not** advanced it. |
| `GET /tasks/{taskId}/subtasks` | listTaskSubtasks | The children a fan-out task owns — one per endpoint for a roll — each with its own `kind` (a **step** kind), `status`, `args`, `result` and `error`. Oldest first, not paginated. This is where a roll's per-endpoint detail lives, and the only account a FAILED roll leaves (a failed step records no result). `404` if the task is unknown; `[]` for a task that fanned out to none. |

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

### Clusters & branches: create, branch, and delete are all tasks

**`createCluster` plans a task and returns `202`.** After validating the body
(`400` on a missing name or an unsupported `pg_version`) and confirming the pool
exists (a synchronous `404`, preserving the pool-not-found contract), the handler
plans a `create_cluster` task (`task.Service.CreateCluster`) and returns `202`
with the `Task` and the `Location` header. The worker then, in idempotent steps:
1. creates the cluster (a "database", unique name within the pool) together with
   its **default branch** (`branch`, `is_default`); and
2. records the controller's **pending-init recipe** (keyed by the flat
   storage-layer `branch_id`) when a controller is configured — the branch is
   declared, not yet materialized.

Because that work is now on the task, a **duplicate cluster name** is no longer a
synchronous `409`: it surfaces when the `create_cluster` step's name insert
fails, ending the task `status == "failed"`. The client polls `getTask`, then
reads the cluster (and its embedded `default_branch`, whose `branch_id` *is* the
storage-layer `branch_id`) back with `getCluster`/`listClusters`. The task
does **not** eagerly materialize the branch on the controller or deploy
an endpoint: compute is a separate resource (below), and the first endpoint
deployed on the branch materializes it (the recipe is then dropped). See
[`controller-cluster-create.md`](controller-cluster-create.md) and
[`ac-tasks.md`](ac-tasks.md).

Compute is a **separate resource**: a caller `POST`s an endpoint on the branch
(below), which the controller deploys via a task. Bootstrapping a fresh
branch's storage (initdb + the storage schema) is still out-of-band — the deploy
path attaches an existing storage-layer branch (Mode B), it does not create one.

**`createBranch` forks an existing branch (#130), as a task.** After validating
the body and resolving the cluster, the parent branch, and the fork point
synchronously — a missing pool/cluster/parent is `404`, and a malformed or
unresolvable `fork_point_lsn` is `400` — the handler plans a `branch` task
(`task.Service.Branch`) and returns `202` + `Task` + `Location`. The new branch
is a non-default `branch` under the **same cluster**, inheriting the parent's
`pg_version` and recording its fork ancestry (`parent_branch_id`, `fork_lsn`).
Externalized extensions are chosen at the cluster level but stored per branch, so
a fork inherits its ancestor's set at fork time (copied onto its own row); its
endpoints resolve against that set, readable on `getBranch` (`extensions`).
The fork point defaults to the parent's durable WAL end — the `flush_lsn` of the
safekeeper co-located with the parent's `read_write` endpoint, read directly from
that node (`node.Client.TimelineFlushLSN`) **before** the task is planned; a
caller can pin an earlier point with `fork_point_lsn`. The worker then forks the
branch and records the controller's **pending-init recipe**, this time
with the ancestor and `start_lsn` set, and the branch is materialized lazily by
the first endpoint deployed on it (the pageserver clones from the ancestor at
`fork_lsn` instead of running `initdb`). A **duplicate branch name** within the
cluster surfaces as a `failed` task, not a `409`; the client polls `getTask`,
then reads the branch with `listBranches`/`getBranch`. *(Reading the ancestor's
own node is routing-correct where the controller's per-branch `describe`
is not — it routes to whichever pageserver attached the branch last and misses
the parent branch once the branch has multiple endpoints, #133; `flush_lsn` also
never lags durable WAL the way a pageserver's `last_record_lsn` can, #130. The
parent must have a `read_write` endpoint to resolve a default — otherwise pass
`fork_point_lsn`.)*

A parent whose safekeeper never answers is **`503`**, not `400` (#724): the fork
is well-formed and retrying is the caller's correct reaction, which
`invalid_request` tells it not to do. Nothing is planned before the fork point
resolves, so that retry is safe. A safekeeper that *does* answer — 404, no
`flush_lsn` — is a verdict, and stays `400`.

**`deleteCluster` and `deleteBranch` are real cascade-delete tasks.** Each runs a
cheap pre-check that the target exists (`404` via `core.Backend` if not);
`deleteBranch` additionally rejects a branch with child branches up front (`409`,
since the parent FK is `RESTRICT` and the task would be doomed). Each then
plans a task — `task.Service.DeleteCluster` / `DeleteBranch` — and returns `202`
+ `Task` + `Location`. The worker's single, self-contained idempotent step
(`delete_cluster` / `delete_branch`) enumerates the cluster's (resp. branch's)
endpoints at execution time, retires each running instance and drops its record,
then drops the branch(es) and the cluster row. The cascade order is
**endpoints → branch(es) → cluster**: the FKs are `RESTRICT` (amp-controller.md
§10.2), so children are removed before their parents — and `delete_cluster`
nulls the branches' in-cluster parent pointers before dropping them to sidestep
the self-FK ordering. The client polls `getTask` for the terminal status; the
outcomes are `202` (accepted), `400`/`404`/`409` (synchronous validation; `409`
only on `deleteBranch` with child forks), or a `failed` task. See [`ac-tasks.md`](ac-tasks.md) §3.

**`listClusters`, `listBranches`, and `listEndpoints` are read-only and
paginated.** `listClusters` returns the pool's clusters (each with its
`default_branch`), scoped to usable clusters unless `include_failed=true` is
passed to also surface failed-creation orphans; `listBranches` returns a cluster's branches (default branch +
forks); `listEndpoints` returns a branch's endpoints. The branch is addressed by
`(poolId, clusterId, branchId)`, so the backend first resolves the branch within
the cluster — `branch_id` is a generated UUID, unique within a cluster — and a
pool, cluster, or branch that doesn't exist is a `404`.

### Endpoints are deployed by a task (#108)

`createEndpoint` resolves the branch within the cluster via
`core.Backend.GetBranch` (`(poolId, clusterId, branchId)`: a missing link is
`404`), validates the role and `disk_type`, resolves the pod's CPU + memory
reservation (the optional `resources: { cpu, memory }` block: **2 cores** by
default, with memory following CPU at the control plane's memory-per-vCPU ratio
— 8 GiB per core unless `--deploy-memory-gib-per-vcpu` says otherwise — and a
pair below that ratio refused with a `400`. `memory` must also carry an explicit
Mi-or-larger unit, because Kubernetes reads a bare `4` as four *bytes* and an
endpoint sized that way can never be resized in place afterwards — meko#492,
whose migration `0009` also normalized the values already stored), then plans a
`create_endpoint` task
(`task.Service.CreateEndpoint`) and returns `202` + `Task` + `Location`. The
`cpu`/`memory` are Kubernetes resource quantities the chart renders as **both a
request and a limit** on the endpoint container, so the pod is scheduled with the
**Guaranteed** QoS class. The
worker's `create_endpoint` step records the `endpoint` row first (so the
single-writer rule is enforced before a deploy is spent), deploys it via the
runtime-agnostic [`deploy.Deployer`](../internal/deploy/) (Docker today,
Kubernetes via Helm), records the resolved `address`, and best-effort drops the
controller's pending-init recipe (the endpoint has now materialized the
branch). A branch admits at most one `read_write` endpoint — the schema's
`endpoints_one_writer_per_branch` partial unique index — so a second writer now surfaces as
a **`failed` task** rather than a synchronous `409`. `deleteEndpoint` runs the
same cheap `404` pre-check, then plans a `delete_endpoint` task that retires the
running instance and drops the row; it returns `202` (not the old `204`). Both
refuse with `501` when the deployer is disabled (`--deploy` off). The client
polls `getTask`, then reads the deployed endpoint's `address` with
`getEndpoint`/`listEndpoints`. The Kubernetes deploy mechanism (the Helm release
per endpoint, Mode-B attach) is described in
[`ac-k8s-deploy.md`](ac-k8s-deploy.md); the step catalog and the worker are in
[`ac-tasks.md`](ac-tasks.md).

## Layering

```
internal/api/    HTTP: generated contract (api.gen.go) + controllers (handlers.go),
                 echo wiring (server.go), error envelope (errors.go), docs (docs.go)
        │  implements StrictServerInterface, calls ↓
internal/core/   Domain: Backend interface + PgBackend (YBDB) + MemBackend (tests).
                 ErrNotFound → 404, ErrConflict → 409.
internal/task/   Task framework: Service (plan tasks), Store, and the worker that
                 runs the idempotent steps. Mutation handlers plan tasks here;
                 getTask/listTasks read status back (ac-tasks.md).
internal/deploy/ Kubernetes endpoint provisioner; deploy/retire driven by the
                 task steps (ac-k8s-deploy.md).
```

The `core.Backend` seam is what lets the whole HTTP layer be tested without a
database: `MemBackend` mirrors `PgBackend`'s semantics (owner-existence checks,
globally-unique pool names, top-level + paginated listing, cluster create, and
in-cluster-unique branch names). The synchronous read paths and the cheap
pre-checks on the mutation handlers go through it directly; the mutations
themselves go through `task.Service`, and the world-changing work runs in the
worker's steps (against the task framework's `StateStore` port, `ac-tasks.md`
§5). A controller that returns an unexpected (non-`echo.HTTPError`) error yields
a `500` rendered in the same `Error` envelope — documented per operation in the
spec so the generated client gets a typed handle for it.

**An unreachable state store is a `503`, not that `500` (#886)**, so a tserver
restart does not read to a caller as a controller bug. `errorHandler` classifies
the escaping error with `db.Unavailable`: a failed connection attempt, a
connection that died under a statement, or a SQLSTATE in class `08` (plus the
class-57 shutdown codes and `53300`) is the store going away; a syntax,
constraint, permission, or authentication error is a verdict on the statement and
stays the controller's fault. This is close to but not the same test `/readyz`
runs — a bare `Ping` where any failure means unhealthy — so the two can disagree
at the edges, and `/readyz` does not predict what a request returned.

Replayability is a second question, answered by `db.SafeToRetry`. A **read**
always may be replayed. A **mutation** only when the failure provably preceded
the statement reaching the store *and* nothing earlier in the request had already
written: past a commit or an insert, the store marks what follows with
`db.ErrOutcomeUnknown` (`db.AfterWrite`), which forces the `500`. Without that,
a connection failure on the read-back after a durable insert would answer `503`
on work that already exists, and the retry would plan it a second time under a
fresh id. Every `503`, this one and `createBranch`'s unresolvable fork point,
carries `Retry-After`.

## Testing

Both in `internal/api/*_test.go`, driving the **generated client** against a real
`httptest` server with the backend stubbed by `MemBackend` and the in-memory
`task.MemStore`:

- **Controller tests** assert routing, path/body binding, status codes, and
  response shapes for every operation. For the six async mutations they check the
  cheap synchronous contract — `202` with the right `Task` `kind` and a `Location`
  header (and the synchronous `400`/`404`/`501` validations) — without running a
  worker; the freshly-planned task is still `pending`.
- **Worker-driven tests** then exercise the full async path. A `drainTasks`
  helper runs a real `task.Worker` — wired with `ProductionRegistry` over the
  same store, backend, and `Provisioning` the server uses — until every task
  reaches a terminal status, so the test can poll `getTask` to `succeeded`/
  `failed` and then read the materialized cluster/branch/endpoint (its ids and
  `address`) back through the API exactly as a client would. This covers
  create-cluster, branch, create/delete-endpoint, and the delete-cluster/
  delete-branch cascades (a duplicate name or single-writer collision asserted as
  a `failed` task).
- **Storage-pool flow test** runs the synchronous create → list → get workflow,
  asserting the top-level behaviour: listing returns every pool, and a pool is
  fetchable by id regardless of its creator.
- **List tests** (`lists_test.go`) cover the paginated lists: the `limit`/`offset`
  contract and clamping, `total` (all matches) vs `count` (this page), and the
  parent `404`s on the nested cluster/branch/endpoint lists.

`TestServesSpecAndExplorer` covers the spec/Swagger-UI/health endpoints, and
`TestServesUI` covers the admin console served at `/`.
