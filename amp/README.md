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
  migrations/         embedded raw-SQL migrations (golang-migrate)
```

## Build / test

From the repo root (`make amp`, `make amp-test`, `make amp-vet`) or directly:

```sh
go -C amp build ./...
go -C amp vet ./...
go -C amp test ./...
```

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

- `GET /healthz` — liveness
- `GET /readyz` — readiness (pings the DB)
- `GET /api/v1/openapi.yaml` — the OpenAPI document
- `GET /api/v1/docs` — Swagger UI (interactive explorer, assets embedded)
- `POST /api/v1/users`, `GET /api/v1/users/:userId` — users (no auth, #92)
- `POST|GET /api/v1/users/:userId/storage-pools`,
  `GET …/storage-pools/:poolId` — storage pools, owner-scoped (no sharing)
- `POST|DELETE …/storage-pools/:poolId/timelines[/:timelineId]`,
  `POST …/timelines/:timelineId/branch` — timelines; **logged only** (`202`)
  until the task framework lands
- `GET /api/v1/tasks/:taskId` — task status (a real read path)
