# amp — the YB Light control plane

`amp` is the **amp controller** (the `yblight-controller` / `yblc`): the control
plane for a single placement group. It owns the lifecycle of storage pools,
tenants, PG clusters/timelines, and endpoints, and exposes all of it through a
REST API. Its only state store is the placement group's YBDB universe, reached
over YSQL.

Design docs (read these first):

- [`yblc-highlvl.md`](architecture/yblc-highlvl.md) — PRD summary: the resource ladder, the
  core workflows, and the state tables.
- [`yblight-controller.md`](architecture/yblight-controller.md) — high-level design and
  guiding principles (API-first, task framework, managed services, deployment).

It started as the **base framework** ([#84](https://github.com/yugabyte/yb-light/issues/84)):
a runnable skeleton — `slog` logging, the write-ahead task framework, and the
YBDB connection + initial migrations. On top of it, the **initial APIs**
([#92](https://github.com/yugabyte/yb-light/issues/92)) add the OpenAPI v3-driven
REST surface (users, storage pools, timelines) below. The workflow execution
that backs the timeline calls (create-database, branch, scale) lands on the task
framework later.

## Layout

```
amp/
  cmd/amp/            entrypoint (serve | migrate | version)
  internal/config/    configuration (env + flags)
  internal/log/       slog logger + context helpers
  internal/db/        pgxpool connection + golang-migrate runner
  internal/api/       REST API: openapi_spec.yaml (source of truth) + generated
                      code (api.gen.go) + controllers, echo wiring, Swagger UI
  internal/core/      domain layer: Backend interface + YBDB and in-memory impls
  internal/task/      write-ahead task framework (types, store, registry, worker)
  internal/process/   subprocess supervision (spawn, restart with backoff, clean stop)
  internal/storagecontroller/  managed storage controller: subprocess spec + timeline client
  migrations/         embedded raw-SQL migrations (golang-migrate)
```

## Build / test

From the repo root (`make amp`, `make amp-test`, `make amp-vet`) or directly:

```sh
go -C amp build ./...
go -C amp vet ./...
go -C amp test ./...
```

The amp module is **pure Go**: `make amp-test` needs no Postgres headers, no Rust
toolchain, and no build-tools image — the storage-controller integration test
runs against a fake binary, so supervision/restart are covered locally. Building
the real bundle (`make amp-bundle`, which compiles `storage_controller`) is the
only part that needs the wider toolchain — the `vendor/postgres-v17` submodule
plus Rust + C + `clang`/`libclang`; it installs the PG v17 headers itself. See
[`deploy/amp`](../deploy/amp/README.md) for the bundle/container details.

## Run

The controller refuses to start without a database URL (its universe connection
is required configuration). Point it at any PostgreSQL-wire endpoint — a real
YugabyteDB universe in production, or a local `yugabyted` for dev:

```sh
# apply migrations only (e.g. an init container / pre-deploy step)
amp migrate --database-url postgres://yugabyte@127.0.0.1:5433/yblight_controller

# run the API server + task worker (migrations run on startup too)
amp serve   --database-url postgres://yugabyte@127.0.0.1:5433/yblight_controller \
            --listen :8080
```

Control-plane tables live in their own schema (`--db-schema`, default `amp`),
isolated from tenant data. Configuration is read from flags or the matching
environment variables (`AMP_DATABASE_URL`, `AMP_LISTEN`, `AMP_DB_SCHEMA`,
`AMP_LOG_LEVEL`, `AMP_LOG_FORMAT`, `AMP_TASK_POLL_INTERVAL`, `AMP_TASK_LEASE_TTL`,
`AMP_SIGNING_KEY_PATH`); flags win.

## API

The REST API is **OpenAPI v3-driven**: `internal/api/openapi_spec.yaml` is the
source of truth, and `oapi-codegen` generates the models, the echo server
interface the controllers implement, and the Go client the tests drive (run
`make amp-generate` after editing the spec). The controller also serves the spec
and an interactive explorer. See
[`architecture/yblc-apis.md`](architecture/yblc-apis.md) for the full design.

## Managed services

The controller runs the **storage controller** (`yblight-controller.md` §11.1) as
a subprocess it fully owns: it spawns the binary, streams its logs, restarts it
with backoff if it exits, and stops it cleanly on shutdown. This is off by
default (the base skeleton runs without the binary present); enable it with
`--storage-controller`:

```sh
amp serve --database-url postgres://yugabyte@127.0.0.1:5433/yblight_controller \
          --storage-controller \
          --storage-controller-bin /path/to/storage_controller \
          --storage-controller-listen 127.0.0.1:50051 \
          --storage-controller-dev
```

| flag | env | default | meaning |
|---|---|---|---|
| `--storage-controller` | `AMP_STORAGE_CONTROLLER` | `false` | run the storage controller as a managed subprocess |
| `--storage-controller-bin` | `AMP_STORAGE_CONTROLLER_BIN` | `storage_controller` | path to the binary |
| `--storage-controller-listen` | `AMP_STORAGE_CONTROLLER_LISTEN` | `127.0.0.1:50051` | host:port it serves on |
| `--storage-controller-database-url` | `AMP_STORAGE_CONTROLLER_DATABASE_URL` | `--database-url` | universe connection (defaults to the controller's) |
| `--storage-controller-dev` | `AMP_STORAGE_CONTROLLER_DEV` | `false` | run it in `--dev` mode (no auth; dev/local only) |
| `--storage-controller-start-timeout` | `AMP_STORAGE_CONTROLLER_START_TIMEOUT` | `30s` | how long to wait for `/ready` |
| `--storage-controller-stop-timeout` | `AMP_STORAGE_CONTROLLER_STOP_TIMEOUT` | `10s` | grace after SIGTERM before SIGKILL |

The controller is the storage controller's only caller; it drives timeline
management (create/delete/describe) through the internal client in
`internal/storagecontroller`.

A bare `--storage-controller-bin` (the default, `storage_controller`) is
resolved next to the `amp` binary first — the **bundle** layout — then `PATH`;
an explicit path is used verbatim. `make amp-bundle` produces that layout in
`target/amp-bundle/`, and the [`deploy/amp`](../deploy/amp/README.md) container
ships amp + storage controller together, so amp finds it with no configuration.

## Endpoints (stubs)

- `GET /healthz` — liveness
- `GET /readyz` — readiness (pings the DB)
- `GET /api/v1/openapi.yaml` — the OpenAPI document
- `GET /api/v1/docs` — Swagger UI (interactive explorer, assets embedded)
- `POST /api/v1/users`, `GET /api/v1/users/:userId` — users (no auth, #92)
- `POST|GET /api/v1/users/:userId/storage-pools`,
  `GET …/storage-pools/:poolId` — storage pools, owner-scoped (no sharing)
- `POST …/storage-pools/:poolId/timelines` — create a timeline: creates a tenant
  (a "database") + its main timeline and returns `(tenant_id, timeline_id)` (`201`)
- `DELETE …/timelines/:timelineId`, `POST …/timelines/:timelineId/branch` —
  delete / branch a timeline; **logged only** (`202`) until the task framework lands
- `GET /api/v1/tasks/:taskId` — task status (a real read path)
