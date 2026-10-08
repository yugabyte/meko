# PR review guide — meko

**Read [`REVIEW.md`](../REVIEW.md) at the repository root first.** It carries the
size gates, the review budget, the reporting rules, the model-selection rule and the
**upgrade-safety check** — all of which apply to every review in this repo. This
file is only the meko-specific layer: the traps and checks particular to this
codebase.

## 1. Area-specific concerns

Meko is a polyglot monorepo; each top-level directory is built independently and
they share only the YugabyteDB schema and the MCP/HTTP contracts between them.
Weight these by what the diff touches, and read the component's own `AGENTS.md`
for the full conventions.

**api_server** (`api_server/` — Go REST API on `:25433`). The dominant concerns
are layering and access control:

- **Layering is not optional.** Handlers parse/validate and call exactly one
  service (`ok()` / `apiError()`, no SQL, no business logic); services hold the
  logic and own transactions; **all SQL lives in `repository/`** and returns
  models, never echo/http types. A query in a handler, or business logic in a
  repo, is a finding.
- **Every net-new route MUST be under an auth-enforcing group** — never an
  unauthenticated public route. Check the scope is the tightest that fits:
  `RequireDatapackOwnership` (owner-only, all mutations) vs
  `RequireDatapackAccess` (owner + shared, reads); admin endpoints under `/admin`
  behind `RequireAdminSecret`; service-to-service under `/internal` behind
  `RequireInternalSecret`. An unguarded route slipping in by omission is a
  security finding, not a nit.
- **Admin logic stays isolated.** Anything meko_admin consumes lives in
  `internal/service/admin` + `internal/repository/admin` + `handlers/admin_*.go`
  — don't surface admin data on a user group, and don't have meko_admin hit user
  routes.
- **Right DB, right pool.** Control-plane tables (`meko_system.*`, `SystemDB`) vs
  data-plane (`datapacks.*`, `dist_rag.*`, `DB`) — a table on the wrong side or a
  query on the wrong pool is a bug.
- **Secrets and feature gates are flags/env, never hardcoded**, and unset secrets
  must degrade gracefully (log + 503 / disabled feature), not crash.
- **Its clients run other versions.** The Deployment rolls replica by replica, `meko`
  CLIs in the field lag the server, BYOC data planes run whatever release the customer
  installed, and agents call the MCP tools directly. Route, request/response and MCP
  tool-schema changes are additive. A removed or renamed field, route or enum value is
  a break, even when every client in this repo was updated in the same PR.

**Database & migrations** (`api_server/internal/db/migrations/{system,data}`).
Migrations run on every boot:

- They must be idempotent and land in the correct `system` vs `data` folder.
- **They run while the previous release is still serving.** The migrate step is an
  init container on every api_server pod, built from that pod's own image
  (`deploy/helm/meko/templates/api-server/deployment.yaml`). The first new pod migrates
  while the old replicas already running keep taking traffic. Expand, then contract:
  add in one release, drop or rename only once no supported release reads the old
  shape. A `DROP COLUMN` or rename in the same PR that stops using the old shape is a
  finding, and so is a new `NOT NULL` column without a default.
- **Every new migration file raises the downgrade floor.** The init container runs
  `m.Up()` (`api_server/internal/db/connect.go`), which fails on a database at a version
  its image doesn't embed. After a rollback, and for any old pod recreated mid-rollout
  (a drain, an eviction), the pod stays stuck in Init. Additive SQL doesn't change
  that, so the PR must state the floor.
- **Enums can't drop values in place** — the pattern is create a `_v2` type,
  rebind columns, drop the old, rename (see `000008_standard_tier`). A raw
  `ALTER TYPE ... DROP` is wrong.
- YugabyteDB is PostgreSQL-wire-compatible but not identical — for schema/index
  choices (sharding, primary-key layout, `SPLIT INTO`, transaction retries)
  weigh the YSQL guidance, not vanilla Postgres habits.

**meko_ui / meko_admin** (`meko_ui/`, `meko_admin/` — React 19 + Vite + Tailwind
v4 + shadcn + TanStack Query, each with an Express server/BFF):

- **All HTTP goes through `src/lib/api/`** — never `fetch`/`axios` from a
  component. Server state is TanStack Query with stable query keys and
  `invalidateQueries` on mutation success; server data must not live in
  `useState`.
- **Styling uses the semantic CSS-variable tokens** (`bg-card`,
  `text-muted-foreground`, `bg-primary`, …) merged with `cn()` — never raw hex,
  and both light and dark must work.
- **Feature flags come from `runtimeEnv`**, not hardcoded, and route strings come
  from `app/routes/paths.ts`, never inline literals.
- **meko_admin BFF rule:** prefer proxying to an api_server `/admin/*` endpoint
  over running SQL in `server/db.ts` — the local pg pool is a drift hazard (it
  caused the `amp_registration` "relation does not exist" bug). Also confirm
  `queryClient.clear()` fires on both login and logout — query keys aren't
  per-user, so skipping it leaks the previous admin's cached data.

**meko-provisioner** (`meko-provisioner/` — Kubebuilder operator). Reconciliation
correctness and generated-file hygiene:

- **Never hand-edit auto-generated files** — `config/crd/bases/*.yaml`,
  `config/rbac/role.yaml`, `config/webhook/manifests.yaml`, `**/zz_generated.*.go`,
  `PROJECT`. Changes to `*_types.go` or kubebuilder markers must be accompanied by
  regenerated manifests (`make manifests generate`); a marker change without the
  regen is stale.
- **Reconciliation must be idempotent**, re-fetch before update to avoid
  conflicts, set owner references for GC, and use finalizers to clean up external
  resources (namespaces, buckets, DNS). Watch secondary resources via `.Owns()` /
  `.Watches()`, not bare `RequeueAfter`.
- Don't delete `// +kubebuilder:scaffold:*` markers or relocate scaffolded files.
- **CRD changes must accept the CRs already stored.** A new required field, a
  tightened validation or a renamed field rejects or drops existing objects on the next
  update. `api_server` writes these CRs and rolls independently of the operator, so each
  side has to tolerate the other's previous version.
- Pinned to Go 1.25.8 via `GOTOOLCHAIN` for lint.

**inference_gateway** (`inference_gateway/` — Python 3.11+, FastAPI + LangChain).
Playground chat backend; forwards memory/conversation calls through MCP:

- Memory/conversation/RAG must go through the per-datapack MCP server with a
  bearer token passed per request — **never** auth via env.
- Session history is in-memory (lost on restart, not shared across instances) and
  there's no streaming — a change assuming persistence or streamed responses is
  wrong unless it adds the machinery.
- Upstream calls are mocked in tests; `pytest` needs no creds.

**cli_app** (`cli_app/` — Go, cobra + bubbletea TUI). The user-facing `meko`
binary; talks only to `api_server`. Keep API vocabulary at the client boundary
and match the surrounding cobra/bubbletea patterns.

## 2. Companion updates travel in the same PR

Confirm the diff didn't leave these stale:

- The relevant component `AGENTS.md` / `docs/` when behavior or a convention
  changed.
- Tests covering the new/changed behavior (api_server unit tests are mocked;
  integration tests need YugabyteDB on `localhost:5433`).
- Regenerated provisioner manifests when `*_types.go` or markers changed
  (`make manifests generate`).
- Swagger doc comments on api_server handlers when a route's shape changed — they
  are the source for generated docs.
- On UI changes, that visible behavior was verified on a running server (no
  in-editor preview).

## 3. Hygiene — meko-specific

The repo-wide rules (no agent attribution, what the body must explain, full URLs) are
in the root [`REVIEW.md`](../REVIEW.md). On top of those:

- **Title carries the tracking ticket** — e.g.
  `[MEKO-123] meko_ui: make Create Cluster pg_version selectable`. A Jira key from
  any project is fine (`[AMP-55]`, `[CLOUDGA-37256]`, …), as is `[#<issue>]` for a
  GitHub issue; don't ask for a `MEKO-` ticket instead. The tag is required (the
  `PR title` check fails an untagged PR): when there is no ticket, ask the author to
  file one, not to add a placeholder like `[MEKO-xxx]`. Do flag an invented or
  guessed number.
- Body includes the `open-pr` sections that apply: Summary, Files changed, Testing,
  Config/env vars, plus Upgrade safety, Math/derivation or Deploy notes when relevant.
- Lint/typecheck clean for the touched module: Go — `go build ./...`,
  `go vet ./...`, `golangci-lint run ./...` (`goimports.local-prefixes: meko`,
  tabs, 120-col); web — `npm run check`; Python — `mypy` + `pycodestyle`.
