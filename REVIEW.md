# PR review guide

Use this when reviewing a pull request in this repo (human or agent). A good
review covers whatever the change actually touches — correctness, tests, docs,
operational impact, hygiene — so weight your attention by what's in the diff.
This guide is the meko-specific layer on top of reading the code: the traps and
checks particular to this codebase.

> **Report findings that matter.** Surface issues you're reasonably confident
> about and that carry real severity — don't report low-confidence guesses or
> low-severity nits. Tag each finding with its severity so the author can
> triage.
>
> **Keep the output reasonably sized.** Cover the findings, but write each one
> tersely — enough for the author to locate the issue and act on it (what's
> wrong, where, why it matters), and no more. Skip restating the diff, padding,
> and multi-paragraph explanations of one-line problems. Terse and actionable
> beats verbose.

> **Agents: pick the model by review complexity.** Run substantive reviews —
> anything touching correctness, concurrency, storage/schema, or cross-service
> seams — on an Opus-class model (e.g. Opus 4.8); review judgment is the
> priority there, not speed or cost. Simple reviews (docs, comments, config,
> small mechanical diffs) may downgrade to Sonnet. Never use Haiku, and don't
> use Fable unless explicitly requested.

## 1. Before you start — gates

**Oversized PRs — ask before proceeding.** Check the size first. If any
threshold is exceeded, **stop and ask the user for permission to proceed** — the
point is to avoid accidentally burning tokens on a review that big:

- more than **20 commits**, or
- more than **100 files** changed, or
- more than **5000 lines** changed (additions + deletions).

If they decline, suggest the PR author run the review with their local AI agent
instead.

**Cap the review's own budget.** Stop and report progress rather than pushing
past either limit:

- **Wall-clock: 10 minutes.**
- **Tokens: 1M (~$25 on Opus).**

On hitting a cap, say how far you got and what's left. These are hard limits, not
targets — don't exceed them.

## 2. Area-specific concerns

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

**Database & migrations** (`api_server/internal/db/migrations/{system,data}`).
Migrations run on every boot:

- They must be idempotent and land in the correct `system` vs `data` folder.
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

## 3. Companion updates travel in the same PR

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

## 4. Hygiene

- **Title carries the tracking ticket** — e.g.
  `[MEKO-123] meko_ui: make Create Cluster pg_version selectable`.
- Body follows the AGENTS.md PR template (Summary, Files changed, Testing,
  Config/env vars, and Math/derivation or Deploy notes when they apply); explains
  **what and why**, not just the diff. Link related PRs/issues with full URLs,
  never a bare `#123`.
- Lint/typecheck clean for the touched module: Go — `go build ./...`,
  `go vet ./...`, `golangci-lint run ./...` (`goimports.local-prefixes: meko`,
  tabs, 120-col); web — `npm run check`; Python — `mypy` + `pycodestyle`.
- **No agent/assistant attribution** in commits or the PR body — no "Generated
  with Claude Code" line, no `claude.ai/code` session link or id, and no
  `Co-Authored-By:` trailer. The PR and its commits are authored by the human.
