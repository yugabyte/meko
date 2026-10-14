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
  A helper that gates a handler on something failing (an access check, a
  malformed request) cannot signal that failure via `apiError`'s own return
  value -- it's `nil` on any successful write, regardless of status. Use
  `writeAndFail(c, status, code, msg)` (`handlers/response.go`), which writes
  the response and returns a real, checkable error, or return `bool` the way
  `bindJSON`/`resolveAgent` do -- never propagate `apiError`'s own result as
  if it were one.
  For a failure that wraps an internal call (an upstream HTTP request, the DB),
  use `internalError(c, err, action)` (`handlers/response.go`) instead of
  `apiError(c, 500, "INTERNAL_ERROR", err.Error())`: it logs `err` server-side
  and returns a generic message, so Go's `*url.Error` (which embeds the internal
  URL) and raw SQL never reach the client (MEKO-812). Keep `apiError` with its
  own message for input validation, where the message is meant for the caller.
- **Services** hold logic and call repos + clients. External-system vocabulary
  stays at the edge — translate a third party's terms to Meko's at the service
  boundary rather than leaking them inward. (The AMP client used to be the
  example: Meko's `cluster` became AMP's `project` at the `AMPService`→client
  boundary. AMP renamed the resource to `cluster` in #671, so that hop is now
  `cluster` end to end and there's no term to translate.)
- **Repos** own every query — parameterized, wrap errors with `%w`, translate
  `sql.ErrNoRows` to a domain sentinel (e.g. `ErrUserNotFound`).

## Wiring & config

- Everything is constructed once in `serviceRegistry` / `buildServiceRegistry`
  (`internal/api/router.go`) in dependency order and shared. New service? add a
  field, build it there, register its routes in the right auth group.
- **Register routes on `root`, never on the bare `echo.Echo`.** `NewRouter` builds
  `root := e.Group(basePath)` and everything hangs off it, so `--base-path=/ycp`
  moves the whole surface at once. A route added straight to `e` silently stays at
  the domain root — and also escapes the group's middleware, which is how the
  health alias briefly lost its rate limiter. The alias is the one deliberate
  exception, and it is handed the limiter explicitly.
- **Config = flags in `main.go` with an `os.Getenv`/`envOrDefault` fallback**
  (mirror the existing ones). Secrets are always env/flag, never hardcoded
  (`YBA_API_TOKEN`, `ADMIN_API_KEY`, `--langfuse-admin-api-key`). Feature gates
  too (`--show-pg-amp`). Degrade gracefully when a secret is unset (log + 503 /
  disabled feature) rather than crashing.

## Access control (required for every new endpoint)

Every net-new route MUST be registered under an auth-enforcing group/middleware
— never add an unauthenticated public route. Enforcing *who can call it* is part
of adding the endpoint, not a follow-up. Pick the tightest scope:

- **User routes** — `authGroup` / `datapackGroup` behind `RequireAuth` (an ID
  token from whichever identity provider is on — Cognito or a generic OIDC
  issuer, both behind `auth.TokenValidator` — or a PAT). Datapack-scoped routes add `RequireDatapackOwnership`
  (owner-only — use for any mutation) or `RequireDatapackAccess` (owner + shared
  — reads).
- **Admin (meko_admin) routes** — the `/admin` group behind `RequireAdminSecret`
  (`X-Meko-Admin-Secret`). **Any endpoint meko_admin consumes goes here**, and
  its logic/SQL live in the isolated `internal/service/admin` +
  `internal/repository/admin` packages (handlers as `internal/handlers/admin_*.go`).
  Don't surface admin/console data on a user group, and don't have meko_admin
  hit user routes. **Every internal amp API goes here too**, meaning any route customers
  must not reach through YBM. YCP's `/admin/environments/{id}/desired/*` routes are
  built this way. They reuse `EnvironmentService`
  (`{service,handlers}/environment_admin_*.go`, a name the `meko_ycp_cp` CI filter
  matches) and are registered only in control-plane mode. The route is half of the
  rule: the public routes must also refuse and hide the desired-state kind it writes.
  The checklist is in [`ycp/ARCHITECTURE.md`](../../ycp/ARCHITECTURE.md#an-internal-amp-api-goes-on-the-admin-tier).
- **Service-to-service** — the `/internal` group behind `RequireInternalSecret`
  (e.g. meko-mcp-server → api_server).

If a route genuinely needs no auth (rare), make it a deliberate, documented
choice — call it out in the PR; never let an unguarded route slip in by omission.

## Database

- Two logical DBs: **`meko_system.*`** control plane (`SystemDB`) vs the
  **data plane** (`datapacks.*`, `dist_rag.*`, `DB`). Put a table on the right
  side and use the matching pool. Admin metrics span both (+ Langfuse) via the
  `admin` repo.
- Migrations live in `internal/db/migrations/{system,data,controlplane}` and run
  on startup. Enums can't drop values in place — swap to a fresh type (see
  `000008_standard_tier`: create `_v2`, rebind columns, drop old, rename).
- **A migration that writes rows is the only statement in its file**, and DDL never
  shares a file with DML: golang-migrate `Exec`s a migration whole, so the simple
  query protocol runs it as one implicit transaction, and YSQL cannot retry a
  statement inside such a batch (yugabyte-db#21833). A conflict with a live writer
  aborts it and leaves the version dirty, failing every later boot until someone
  resets that set's version row by hand — each set has its own table, and only the
  control-plane one lives in `meko_system` (`getPostgresConfig`). Alone in its file
  the same statement is retried transparently. `TestBackfillMigrationsAreSingleStatement`
  enforces it — nothing else would, because such a batch applies cleanly on a quiet
  database and on the plain Postgres the `controlplane` set meets in CI
  (CLOUDGA-36984).
- A new migration's version must extend main's current highest by exactly +1
  in its directory, and ship both an `up.sql` and a `down.sql` — CI's
  `meko_migration_versions` job blocks a PR that doesn't. Check locally before
  pushing: `go run ./internal/db/migrations/checkmigrations origin/main`.
- Classify a unique violation by SQLSTATE `23505` plus **which statement raised
  it**, never by `pq.Error.Constraint`: PostgreSQL fills that field, YSQL does not
  (it only formats the index name into the message), so name-matching passes every
  test and turns the 409 into a 500 in production — CLOUDGA-36978, and
  <https://github.com/yugabyte/amp/issues/400> before it. Where the statement
  alone doesn't identify the index, pin the table's unique-index inventory in a
  test, as `internal/repository/environment_unique_indexes_test.go` does.
- `meko_system.runtime_config` holds operator-tunable values that a service must
  pick up without a restart (today: `memory_search_min_score`, consumed by
  meko-mcp-server). One row per key, shared by every datapack on this database.
  Flags and env vars stay the source of truth for boot-time
  settings: pool sizes, feature gates, URLs. Values are `VARCHAR`; each consumer
  parses, validates, and falls back (MCP falls back to the
  `MEMORY_SEARCH_MIN_SCORE` env default). Seed new keys in a system migration.
  The operator `UPDATE` recipe is in `USAGE.md`.

## Conventions

- `.golangci.yml` (repo root, golangci-lint **v2** format) governs this module:
  **`formatters.settings.goimports.local-prefixes: [meko]`** (local imports in
  their own group), tabs, **120-col** lines. Run `golangci-lint run ./...` with a
  v2.8.0 binary — see the root `AGENTS.md > Lint` for the install line, since the
  obvious `go install` produces one that refuses to lint a `go 1.25` module.
- Structured logging via `slog` with key/value pairs.
- Naming: exported `PascalCase`; constructors `New<Type>`; `…Repo` / `…Service` /
  `…Handler` suffixes; swagger doc comments on handlers are the source for
  generated docs.

## Checks

`go build ./...`, `go vet ./...`, and `golangci-lint run ./...`. Unit tests are
mocked (`go test ./...`); integration tests need YugabyteDB on `localhost:5433`.
The datapack list integration test (`repository/datapack_list_integration_test.go`)
instead needs a disposable YugabyteDB named by `MEKO_LIST_TEST_DSN`, and skips
without it; it applies the system migrations there.
