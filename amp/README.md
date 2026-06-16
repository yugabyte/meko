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

This module is the **base framework** ([#84](https://github.com/yugabyte/yb-light/issues/84)):
a runnable skeleton — echo API stubs, `slog` logging, the write-ahead task
framework, and the YBDB connection + initial migrations. The real workflow logic
(create-database, branch, scale) lands on top of it later.

## Layout

```
amp/
  cmd/amp/            entrypoint (serve | migrate | version)
  internal/config/    configuration (env + flags)
  internal/log/       slog logger + context helpers
  internal/db/        pgxpool connection + golang-migrate runner
  internal/api/       echo server, middleware, stub handlers
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

## Endpoints (stubs)

- `GET /healthz` — liveness
- `GET /readyz` — readiness (pings the DB)
- `GET /api/v1/tasks/:id` — task status (a real read path)
- `POST /api/v1/storage-pools`, `/databases`, `/databases/:tenant/branches`,
  `/timelines/:tenant/:timeline/endpoints`, `DELETE /endpoints/:id` — the core
  workflows (§6), currently `501 Not Implemented` until the task step catalog
  exists.
