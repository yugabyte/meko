# AGENTS.md — api_server (Go REST API)

Go + echo, layered handler → service → repository, on `:25433`. Owns
datapacks/agents/knowledge-bases, Cognito auth, Langfuse provisioning, S3, RAG
indexing, AMP orchestration, and the meko_admin endpoints. Runs migrations on
boot. Match the surrounding code; `go 1.25`.

## Layering (do not cross the layers)

```
internal/
  api/          # router + service-registry wiring (router.go, server.go)
  handlers/     # echo handlers: parse+validate input, call ONE service, return ok()/apiError(). No SQL, no business logic.
  service/      # business logic + orchestration. Calls repos and external clients. Owns transactions/coordination.
    admin/      # admin-only services, isolated so the user-facing services stay untouched
  repository/   # ALL SQL lives here. One repo per aggregate. Returns models, never echo/http types.
    admin/      # admin-only repos (span control-plane + data-plane + Langfuse)
  yba/          # standalone external client (YBA REST). External clients get their own package.
  models/       # shared structs
  db/migrations/{system,data}/   # goose-style, applied on boot
  auth/         # middleware (JWT/PAT, RequireDatapackOwnership/Access, RequireAdminSecret) + context getters
```

- **Handlers** never touch repos or SQL; they use `ok(c, status, data)` /
  `apiError(c, status, code, msg)` (see `handlers/response.go`) and pull HTTP
  request-scoped context via `auth.GetAccount(c)` / `auth.GetDatapack(c)`.
- **Services** hold logic and call repos + clients. External-system vocabulary
  stays at the edge: e.g. Meko-facing routes/handlers say `cluster`, and the
  `AMPService`→AMP client boundary is where it becomes an AMP `project` (the
  handler translates `:cluster_id` → the service's `projectID`). Keep that seam.
- **Repos** own every query — parameterized, wrap errors with `%w`, translate
  `sql.ErrNoRows` to a domain sentinel (e.g. `ErrUserNotFound`).

## Wiring & config

- Everything is constructed once in `serviceRegistry` / `buildServiceRegistry`
  (`internal/api/router.go`) in dependency order and shared. New service? add a
  field, build it there, register its routes in the right auth group.
- **Config = flags in `main.go` with an `os.Getenv`/`envOrDefault` fallback**
  (mirror the existing ones). Secrets are always env/flag, never hardcoded
  (`YBA_API_TOKEN`, `ADMIN_API_KEY`, `--langfuse-admin-api-key`). Feature gates
  too (`--show-pg-amp`). Degrade gracefully when a secret is unset (log + 503 /
  disabled feature) rather than crashing.

## Access control (required for every new endpoint)

Every net-new route MUST be registered under an auth-enforcing group/middleware
— never add an unauthenticated public route. Enforcing *who can call it* is part
of adding the endpoint, not a follow-up. Pick the tightest scope:

- **User routes** — `authGroup` / `datapackGroup` behind `RequireAuth` (Cognito
  JWT or a PAT). Datapack-scoped routes add `RequireDatapackOwnership`
  (owner-only — use for any mutation) or `RequireDatapackAccess` (owner + shared
  — reads).
- **Admin (meko_admin) routes** — the `/admin` group behind `RequireAdminSecret`
  (`X-Meko-Admin-Secret`). **Any endpoint meko_admin consumes goes here**, and
  its logic/SQL live in the isolated `internal/service/admin` +
  `internal/repository/admin` packages (handlers as `internal/handlers/admin_*.go`).
  Don't surface admin/console data on a user group, and don't have meko_admin
  hit user routes.
- **Service-to-service** — the `/internal` group behind `RequireInternalSecret`
  (e.g. meko-mcp-server → api_server).

If a route genuinely needs no auth (rare), make it a deliberate, documented
choice — call it out in the PR; never let an unguarded route slip in by omission.

## Database

- Two logical DBs: **`meko_system.*`** control plane (`SystemDB`) vs the
  **data plane** (`datapacks.*`, `dist_rag.*`, `DB`). Put a table on the right
  side and use the matching pool. Admin metrics span both (+ Langfuse) via the
  `admin` repo.
- Migrations live in `internal/db/migrations/{system,data}` and run on startup.
  Enums can't drop values in place — swap to a fresh type (see
  `000008_standard_tier`: create `_v2`, rebind columns, drop old, rename).

## Conventions

- `.golangci.yml` governs this module: **`goimports.local-prefixes: meko`**
  (local imports in their own group), tabs, **120-col** lines. Run
  `golangci-lint run ./...`.
- Structured logging via `slog` with key/value pairs.
- Naming: exported `PascalCase`; constructors `New<Type>`; `…Repo` / `…Service` /
  `…Handler` suffixes; swagger doc comments on handlers are the source for
  generated docs.

## Checks

`go build ./...`, `go vet ./...`, and `golangci-lint run ./...`. Unit tests are
mocked (`go test ./...`); integration tests need YugabyteDB on `localhost:5433`.
