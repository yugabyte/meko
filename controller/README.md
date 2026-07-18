# controller — the YB Amp control plane

`controller` is the **amp controller** (the `amp-controller` / `ac`): the control
plane for a single placement group. It owns the lifecycle of storage pools,
projects, PG clusters/branches, and endpoints, and exposes all of it through a
REST API. Its only state store is the placement group's YBDB universe, reached
over YSQL.

Design docs (read these first):

- [`ac-highlvl.md`](architecture/ac-highlvl.md) — PRD summary: the resource ladder, the
  core workflows, and the state tables.
- [`amp-controller.md`](architecture/amp-controller.md) — high-level design and
  guiding principles (API-first, task framework, managed services, deployment).
- [`ac-k8s-deploy.md`](architecture/ac-k8s-deploy.md) — deploying endpoints: the
  Helm-backed Kubernetes deployer and its Docker sibling, the synchronous
  deploy/retire path, and the `--deploy-driver` selector.
- [`controller-project-create.md`](architecture/controller-project-create.md) — creating a
  project (patterned on `yb_amp_local`): declare a recipe at create, materialize
  the storage layer at the first endpoint.
- [`ac-mcp.md`](architecture/ac-mcp.md) — the MCP server (`controller/mcp/`): a
  FastMCP wrapper that exposes the REST API as MCP tools for AI agents.

It started as the **base framework** ([#84](https://github.com/yugabyte/amp/issues/84)):
a runnable skeleton — `slog` logging, the write-ahead task framework, and the
YBDB connection + initial migrations. On top of it, the **initial APIs**
([#92](https://github.com/yugabyte/amp/issues/92)) add the OpenAPI v3-driven
REST surface (users, storage pools, projects, branches) below. The **Kubernetes endpoint
deploy** ([#108](https://github.com/yugabyte/amp/issues/108)) adds endpoints
as a resource: `createEndpoint` synchronously shells out to the configured driver
— Docker by default, or Helm on Kubernetes via `--deploy-driver kubernetes` — to
bring up one pod/container per endpoint (see
[`ac-k8s-deploy.md`](architecture/ac-k8s-deploy.md)). Moving the deploy onto
the write-ahead task framework, along with the remaining project/branch calls
(create-project, branch, scale), lands later.

## Layout

```
controller/
  cmd/controller/            entrypoint (serve | migrate | version)
  internal/config/    configuration (env + flags)
  internal/log/       slog logger + context helpers
  internal/db/        pgxpool connection + golang-migrate runner
  internal/api/       REST API: openapi_spec.yaml (source of truth) + generated
                      code (api.gen.go) + controllers, echo wiring, Swagger UI
  internal/core/      domain layer: Backend interface + YBDB and in-memory impls
  internal/task/      write-ahead task framework (types, store, registry, worker)
  internal/process/   subprocess supervision (spawn, restart with backoff, clean stop)
  internal/registry/          the storage-layer control surface (generation authority + registries)
  internal/helm/      one-shot helm CLI wrapper (upgrade --install / uninstall)
  internal/deploy/    endpoint provisioner: Kubernetes/Helm or Docker (synchronous deploy/retire)
  migrations/         embedded raw-SQL migrations (golang-migrate)
  mcp/                MCP server (Python/FastMCP, uv): wraps the REST API as MCP tools (ac-mcp.md)
```

## Build / test

From the repo root (`make controller`, `make controller-test`, `make controller-vet`) or directly:

```sh
go -C controller build ./...
go -C controller vet ./...
go -C controller test ./...
```

The controller module is **pure Go**: `make controller-test` needs no Postgres headers, no Rust
toolchain, and no build-tools image — the controller integration test
runs against a fake binary, so supervision/restart are covered locally. Building
the real bundle (`make controller-bundle`, which compiles `controller`) is the
only part that needs the wider toolchain — the `compute/vendor/postgres-v18` submodule
plus Rust + C + `clang`/`libclang`; it installs the PG v18 headers itself. See
[`deploy/controller`](../deploy/controller/README.md) for the bundle/container details.

## Run

The controller refuses to start without a database URL (its universe connection
is required configuration). Point it at any PostgreSQL-wire endpoint — a real
YugabyteDB universe in production, or a local `yugabyted` for dev:

```sh
# apply migrations only (e.g. an init container / pre-deploy step)
controller migrate --database-url postgres://yugabyte@127.0.0.1:5433/amp_controller

# run the API server + task worker (migrations run on startup too)
controller serve   --database-url postgres://yugabyte@127.0.0.1:5433/amp_controller \
            --listen :8080
```

Control-plane tables live in their own schema (`--db-schema`, default `controller`),
isolated from project data. Configuration is read from flags or the matching
environment variables (`AMP_DATABASE_URL`, `AMP_LISTEN`, `AMP_DB_SCHEMA`,
`AMP_LOG_LEVEL`, `AMP_LOG_FORMAT`, `AMP_TASK_POLL_INTERVAL`, `AMP_TASK_LEASE_TTL`,
`AMP_SIGNING_KEY_PATH`); flags win.

## API

The REST API is **OpenAPI v3-driven**: `internal/api/openapi_spec.yaml` is the
source of truth, and `oapi-codegen` generates the models, the echo server
interface the controllers implement, and the Go client the tests drive (run
`make controller-generate` after editing the spec). The controller also serves the spec
and an interactive explorer, plus an embedded **admin console** at `/` (open the
listen address, e.g. `http://localhost:8080/`): a static page to manage users
and, per user, their storage pools, projects, branches, and endpoints. See
[`architecture/ac-apis.md`](architecture/ac-apis.md) for the full design.

## Storage-layer control surface

The controller owns cluster control for the storage layer: **generation authority**
(the attach-hook fencing counter), the pageserver-node / branch / endpoint
**registries**, and **provisioning** of the shared `yb_storage` database (all its
schemas) on startup. It serves these as an HTTP surface (`/control/v1/...`,
`/debug/v1/attach-hook`, `/ready`) on the same listener as the versioned `/api/v1`
product API; the endpoint pods and the `yb_amp_local` dev harness drive it. State
lives in the `registry` schema of `yb_storage`. See
[`architecture/sc-integration.md`](architecture/sc-integration.md) for the design.

`make controller-bundle` builds the (Go-only) `controller` binary into
`target/controller-bundle/`, which the [`deploy/controller`](../deploy/controller/README.md)
container ships.

## MCP server

[`mcp/`](mcp/) is a standalone **MCP server** that exposes the REST API to AI
agents as [Model Context Protocol](https://modelcontextprotocol.io) tools. It is
a thin, stateless [FastMCP](https://github.com/jlowin/fastmcp) wrapper (Python,
built with `uv`) that calls the controller over HTTP — no DB, no auth of its own.
It registers **18 tools** (one per REST operation; `create_user`/`list_users` are
deliberately omitted, and `owner_user_id` is a required per-call argument), runs
over `stdio` (local/desktop clients) or `http` (in-cluster), and is deployed as
its own pod via the `yb-amp-controller` Helm chart (`mcp.enabled`). See
[`architecture/ac-mcp.md`](architecture/ac-mcp.md) for the design and
[`mcp/README.md`](mcp/README.md) for running it. Build/test with `uv`:

```sh
uv run --project controller/mcp pytest
```

## Endpoints (stubs)

- `GET /healthz` — liveness
- `GET /readyz` — readiness (pings the DB)
- `GET /api/v1/openapi.yaml` — the OpenAPI document
- `GET /api/v1/docs` — Swagger UI (interactive explorer, assets embedded)
- `POST /api/v1/users`, `GET /api/v1/users/:userId` — users (no auth, #92)
- `GET /api/v1/users` — list users (paginated)
- `POST|GET /api/v1/storage-pools`, `GET …/storage-pools/:poolId` — storage pools,
  top-level (#109): create names the owner in the body (`owner_user_id`); list
  returns every pool; get is by id (not owner-scoped)
- `POST|GET …/storage-pools/:poolId/projects` — create a project (creates a project
  (a "database") + its default branch and returns a `Project` with its embedded
  `default_branch`, `201`); list the pool's projects (paginated)
- `GET …/projects/:projectId`, `DELETE …/projects/:projectId` — get a project /
  delete it (**logged only**, `202`, until the task framework lands)
- `POST|GET …/projects/:projectId/branches` — branch (fork) an existing branch into a
  new branch (`201`); list the project's branches (paginated)
- `GET …/branches/:branchId`, `DELETE …/branches/:branchId` — get a branch /
  delete it (**logged only**, `202`, until the task framework lands)
- `GET …/branches/:branchId/endpoints` — list the branch's endpoints (paginated)
- `GET /api/v1/tasks/:taskId` — task status (a real read path)

List endpoints take `limit`/`offset` query params and return `total` (all matches)
and `count` (this page); see [`architecture/ac-apis.md`](architecture/ac-apis.md).
