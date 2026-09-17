# AGENTS.md

This file provides guidance to AI coding agents when working with code in this repository.

## Prerequisites — verify before doing anything else

An **authenticated GitHub CLI (`gh`)** is required. Nearly everything around the
code — PRs, reviews, CI runs and logs, labels, rulesets, issues — lives on
GitHub, and this repo is private, so unauthenticated access silently returns the
wrong thing rather than failing.

**Check at the start of a session**, before planning or editing:

```bash
gh auth status          # must report a logged-in account with a valid token
```

### If it's missing, stop and set it up

Do not work around it. Specifically, do **not**:

- fetch GitHub URLs with a web fetcher instead of `gh` — a private repo returns
  the wrong content or nothing, and you cannot tell which from the response;
- ask the user to paste CI output or issue bodies in by hand.

`gh auth login` is interactive, so the user must run it themselves:

```bash
gh auth login          # choose GitHub.com → HTTPS → authenticate in browser
```

In Claude Code, have them prefix it with `!` (`! gh auth login`) so it runs in
the session and the output lands in the conversation. On a remote dev server the
browser will not open by itself — if a URL is printed, the user opens it locally.

Re-check with `gh auth status` before continuing. Do not assume success because
the user said they did it.

## Repository Layout

Meko is a polyglot monorepo. Each top-level directory is an independently-built component; they share only the YugabyteDB schema and the MCP/HTTP contracts between them.

| Path | Language | What it is |
|------|----------|-----------|
| `cli_app/` | Go (cobra + bubbletea TUI) | The `meko` CLI binary — entry point for end users. Talks to `api_server`. |
| `api_server/` | Go (net/http + Swagger) | REST API on `:25433`. Owns datapacks/agents/knowledge-bases, Cognito auth, Langfuse provisioning, S3 uploads, RAG indexing. Migrations are a **separate invocation** (`-migrate-only`), not part of serving — see Database below. |
| `inference_gateway/` | Python 3.11+ (FastAPI + LangChain) | Playground chat backend on `:8088`. Wires LLM (OpenAI / Bedrock) → Meko MCP server for memory + conversations. |
| `meko-provisioner/` | Go (Kubebuilder operator) | K8s operator that reconciles `MekoUserAccount` CRs into per-tenant namespaces (MCP server, RAG worker, Langfuse). See `meko-provisioner/AGENTS.md` for kubebuilder rules. |
| `meko_ui/` | TypeScript (React 19 + Vite + Tailwind + Drizzle) + Node Express server | Web UI; Express server in `meko_ui/server/` proxies to `api_server`. |
| `system_agent/` | Python (LangChain + mem0 + Streamlit) | Standalone agent + Streamlit playground; uses pgvector + BM25 over YugabyteDB. |
| `docs/` | Hugo | Public documentation site. |
| `jenkins/` | Groovy + bash | The image build pipeline: builds every meko component image on ephemeral AWS builders and pushes it to `quay.io/yugabyte/meko-*`. See `jenkins/README.md`. |
| `deploy/helm/meko/` | Helm | One flat chart deploying the whole platform — every component above plus langfuse — toggled per component and rendered for EKS or GKE from the same templates via values presets. See `deploy/helm/meko/README.md`. |

`ycp/` is **not** part of this tree — it is the shared control-plane surface between
meko and amp and lives at the repository root. See the root `AGENTS.md`.

The CLI talks to `api_server`, which provisions tenant infra via `meko-provisioner` and writes Langfuse audit data. The UI's Playground talks to `inference_gateway`, which forwards memory/conversation calls through MCP. Everything ultimately lands in YugabyteDB (default `localhost:5433`).

## Build & Run

```bash
# Build all Go binaries into build/latest/ (cli, api, provisioner)
./build.sh

# Build a subset, with lint
./build.sh cli api --lint

# Clean
./build.sh --clean
```

Per-component dev loops:

```bash
# CLI
(cd cli_app && go build -o meko . && ./meko --help)

# API server (runs migrations + starts on :25433)
(cd api_server && go run . -db-host 127.0.0.1 -db-port 5433)
# Migrations only:
(cd api_server && go run . -migrate-only)

# Inference gateway (Python 3.11+)
(cd inference_gateway && pip install -e ".[dev]" && python -m inference_gateway --llm-provider openai)

# UI (dev server with proxy to api_server)
(cd meko_ui && npm install && npm run dev)        # full server
(cd meko_ui && npm run dev:client)                # vite-only on :3010

# Provisioner (against current kubeconfig)
(cd meko-provisioner && make run)
```

## Tests

```bash
# api_server — unit tests are mocked; integration tests need YugabyteDB on localhost:5433
# See the destructive-tests warning below before running the whole tree.
(cd api_server && go test ./... -v)
(cd api_server && go test ./internal/auth/ -run TestValidateAccessToken_Valid -v)   # single test
(cd api_server && go test ./... -v -count=1)                                          # bypass cache

# cli_app
(cd cli_app && go test ./...)

# inference_gateway — all upstream calls are mocked, no creds needed
(cd inference_gateway && pytest)

# meko-provisioner — uses envtest (bundles its own apiserver + etcd)
(cd meko-provisioner && make test)

# UI typecheck
(cd meko_ui && npm run check)

# Helm chart — needs the helm-unittest plugin
# (helm plugin install https://github.com/helm-unittest/helm-unittest)
helm unittest deploy/helm/meko
```

### `api_server/internal/handlers` tests delete your local data

`go test ./internal/handlers/` (and therefore `go test ./...` in `api_server`)
**empties meko_system tables in whatever database is listening on
localhost:5433** — `datapacks`, `knowledge_bases`, `agents`, `agent_access`,
`agent_access_permissions`, `datapack_invites` and the langfuse mappings. Every
test calls `setupTestServer` → `cleanupDatapacks`, which runs unconditional
`DELETE`s, and the connection string is a hardcoded constant (`testConnStr` in
`health_handler_test.go`), so pointing the suite elsewhere means editing the
source. There is no prompt and no dry run.

If your local stack is up, that database *is* your dev data. Stop it, or accept
losing every datapack, before running the suite. S3 objects and the `dist_rag`
schema are untouched, so uploaded files and their chunks survive — but the
datapack rows that reference them do not, and the `agents` rows are gone with
no way to reconstruct them.

Recovering means re-inserting datapack rows with their **original** UUIDs, which
can be read back out of `dist_rag.sources` (each `source_uri` is
`s3://<bucket>/<user_id>/<datapack_id>/`). Same id, same S3 prefix, and
`CreateKB` re-adopts the orphaned dist_rag source by URI. Datapack *names* are
not recoverable.

**Linting the chart needs a values preset.** It is preset-driven, so bare
defaults trip an install-time guard (`ingress.tls.acmCertArn required for
acm-arn mode`) and `helm lint` then reports a misleading YAML parse error
against an unrelated template. Compose a cloud preset with an env preset, the
mode preset last:

```bash
cd deploy/helm/meko
helm lint . -f values-aws.yaml -f values-dev.yaml                     # EKS
helm lint . -f values-gcp.yaml -f values-dev.yaml -f values-byoc.yaml # BYOC data plane
helm lint . -f values-gcp.yaml -f values-dev.yaml -f values-ycp.yaml  # YCP control plane
```

## Lint

```bash
# Go — run per-module (each has its own go.mod)
(cd cli_app          && golangci-lint run ./...)
(cd api_server       && golangci-lint run ./...)
(cd meko-provisioner && GOTOOLCHAIN=go1.25.8 make lint)

# Python (system_agent)
(cd system_agent && mypy . && pycodestyle .)
```

`.golangci.yml` at repo root governs `cli_app` and `api_server`; `meko-provisioner` has its own. Both are **golangci-lint v2** configs (`version: "2"` on line 1) — v1 binaries cannot read them, and v2 needs the action at v7+. CI and the provisioner Makefile both pin **v2.8.0**; bump them together.

Install a matching binary with the repo's Go, not the tool's — `go install` otherwise builds it with golangci-lint's own older toolchain and it refuses to lint a `go 1.25` module:

```bash
GOTOOLCHAIN=go1.25.0 go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.8.0
```

Note `formatters.settings.goimports.local-prefixes: [meko]` — local imports go in their own group (in v2, `goimports` is a *formatter*, not a linter). Line limits: **120 for Go, 100 elsewhere**. Tabs for Go, 4 spaces for Python, 2 spaces for YAML/shell (see `.editorconfig`).

## Database

YugabyteDB YSQL (PostgreSQL-wire-compatible) on `:5433`. Tenant data lives under `meko_system.*` (registries) plus per-datapack databases (`account_id_<datapack-name>`).

**Migrations are a separate run, not part of serving.** `api_server` migrates only when `-migrate-only` (or one of the `-only-*-migrate` scoping flags) is passed; started without one it serves against whatever schema is already there. So every deployment needs its own migrate step — both the YCP control plane and the data-plane stack use an init container for it. Skip it and the api-server comes up healthy, answers `/api/v1/health` with 200, and fails every real request with `relation "meko_system.users" does not exist`.

```bash
(cd api_server && go run . -migrate-only)          # both sets: meko_system + data-plane
(cd api_server && go run . -only-system-migrate)   # one set; implies -migrate-only
```

Reset system tables for a clean slate:

```bash
./bin/ysqlsh -U yugabyte -d yugabyte -c "TRUNCATE meko_system.agent_knowledge_base, meko_system.agent_registry, meko_system.datapack_knowledge_base_registry, meko_system.datapack_registry, meko_system.account_registry CASCADE;"
```

## Architecture Notes

- **Three Go modules.** `cli_app/`, `api_server/`, and `meko-provisioner/` each have their own `go.mod` (all `go 1.25.x`). `meko-provisioner` is pinned to Go 1.25.8 via `GOTOOLCHAIN` in `build.sh` for its lint target; `cli_app` and `api_server` declare `go 1.25.0`.
- **Datapack = tenant unit.** A datapack is one PostgreSQL database + one MCP server endpoint + one Langfuse project. `api_server` creates all three transactionally when `POST /api/v1/datapacks` succeeds; deletion cascades via the provisioner's finalizer.
- **MCP is the agent contract.** Agents (CrewAI, LangChain, etc.) and the inference gateway all reach memory/conversation/RAG via the per-datapack MCP server. Auth is a bearer token passed in each request (never via env).
- **Cognito is optional.** When `--cognito-*` flags are unset, `/api/v1/datapacks` accepts unauthenticated calls. Production always sets them; `/success` is the OAuth2 callback.
- **Langfuse split.** `api_server` uses Langfuse's admin API for provisioning *and* connects directly to Langfuse Postgres (read-only) for observability endpoints. Without `--langfuse-db-*`, those endpoints return 503.
- **The mount point is configurable.** `--base-path` / `MEKO_YCP_BASE_PATH` moves every route under a URL prefix (console, `/api/v1`, swagger, the OAuth2 callback) for deployments fronted at a subpath of a shared domain — YCP at `https://devcloud.yugabyte.com/ycp/`. Unset (the default) serves at the domain root, unchanged. It must be a plain path prefix, not a URL: anything else fails startup rather than mounting somewhere unreachable. `GET /api/v1/health` answers at the domain root either way, since container probes bypass the proxy that supplies the prefix.
- **The BYOC data plane is installed by its chart; the control plane declares what runs *inside* it.** `POST /api/v1/environments` takes a *name*, a *kind* (`meko` | `amp`), and the *api_key_id* of the YBM API key the agent authenticates with. It seeds no desired state: the workloads — YugabyteDB with the `mage`/`pg_dist_rag` extensions plus `api-server`, `mcp-server`, `rag-worker`, `inference-gateway` and `ui` — come from the `meko` chart, which `GET /api/v1/environments/{id}` renders an install command for, with the stack's per-component image tags pinned from `api_server/internal/service/dataplane_release.go`. `POST /api/v1/environments/{id}/desired` replaces what the environment declares; nothing is merged, and a withdrawn resource is garbage-collected. Because omission is deletion, the write refuses a body that does not say what it means: `resources` is required (an absent key is a 400, not an empty set), unknown top-level fields are rejected rather than dropped, and an explicit `"resources": []` — which withdraws the whole tree — needs `"allow_empty": true` beside it. Registration is unaffected: `POST /api/v1/environments` with `"desired": []` still means "declare nothing" on its own. Kinds are constrained by the environment's kind — `agent` and `datapack` for meko, `storage-pool`, `cluster`, `branch` and `endpoint` for amp — and anything else is a 400 at declare time. Specs carry Secret *names*, never values. See `ycp/DEPLOY.md` for the field reference.
- **A fleet route reads the environment before it reads anything else.** Every per-environment route on the `fleet` group (`/environments/{id}/metrics`, `/environments/{id}/tasks`) resolves the environment against the calling account *first* and then scopes the read by that resolved row, never by anything in the query string — which is what makes the account header an authorization check rather than a lookup hint, and why another account's environment is a 404. Register new ones on the `fleet` group so they inherit the admin-secret + account middleware instead of wiring their own.
- **An amp environment's task history is relayed, not queried.** There is no inbound path to a BYOC data plane, so the reconcile agent reads the amp controller's task log as a change feed (`GET /tasks?updated_since=`) and reports the delta on its status heartbeat; `meko_system.environment_tasks` accumulates it and `GET /api/v1/environments/{id}/tasks` serves it. The control plane is the durable copy — the agent forgets what it relayed on restart and nothing can re-read the log for it — so that table's retention is the console's history depth. A `kind: meko` environment has no task log and returns an empty page. See `ycp/ARCHITECTURE.md` and `amp/controller/architecture/ac-tasks.md` §4.1.
- **No streaming yet.** `inference_gateway` returns full LLM replies in one response; session history is in-memory (lost on restart, not shared across instances).

## Auto-Generated Files (do not edit)

In `meko-provisioner/`: `config/crd/bases/*.yaml`, `config/rbac/role.yaml`, `**/zz_generated.*.go`, `PROJECT`. Regenerate with `make manifests generate` after editing `*_types.go` or kubebuilder markers.

In `api_server/`: Swagger comments at the top of `main.go` are the source for generated docs; don't hand-edit anything in a generated `docs/` if one appears.

## Per-component guides

Component-specific conventions (folder layout, naming, data/API patterns, best
practices) live in each component's own `AGENTS.md` — read the one for the code
you're touching:

- `meko_ui/AGENTS.md` — web app (React/Vite/Tailwind/shadcn/TanStack Query).
- `meko_admin/AGENTS.md` — operator console (SPA + Express BFF that proxies to api_server).
- `api_server/AGENTS.md` — Go REST API (handler → service → repository layering).
- `meko-provisioner/AGENTS.md` — kubebuilder operator.

## Prose discipline

Comment the *why* when it isn't obvious; keep docs and PR descriptions to what the
reader needs and must act on. Padding costs reviewers real time and rots — the full
rule, and the reasoning, is in
[`AGENTS.md > Prose discipline`](../AGENTS.md#prose-discipline--write-for-the-reader-not-for-volume);
the `commit` skill re-checks it at commit time.

## Branch naming

Branches are **`<username>/<kebab-topic>`** — `<username>` is your short handle
(`hsunder`, `anijhawan`, `rajmaddy`) — or **`feature/<kebab-topic>`**. Lead the
topic with a ticket / issue number when the work tracks one:

```
hsunder/174-ingest-perf-hardening
feature/270-free-tier-quota
anijhawan/app-no-cognito
```

Conventional-commit type prefixes are **not** branch prefixes: `fix/`, `feat/`,
`bug/`, `chore/`, `docs/`, `test/`, `refactor/`, `release/` are all rejected, as
are bare names with no prefix at all (`change-qs`). Branches on the remote that
predate this rule don't set precedent.

Enforcement comes from two places, and they cover different halves of the rule:

- **The remote.** The `branch-names-require-prefix` ruleset restricts creation on
  `refs/heads/*`; GitHub's `*` doesn't span `/`, so a bare name (`fix-wal`) is
  rejected at push time while `hsunder/topic` is not. This only requires *a*
  prefix — it can't tell `fix/` from a username.
- **Locally.** `.claude/hooks/check-branch-name.sh` blocks a non-conforming
  `git push` before it runs, and is what enforces the part the ruleset can't:
  the prefix must be a username or `feature`.

Check the name before the *first* push: renaming a published branch means
deleting the remote branch and re-pushing, which breaks any open PR.

The hook has a test suite — run it after touching the parser:

```bash
bash .claude/hooks/check-branch-name.test.sh
```

## Agent skills (`.claude/skills/`)

Pointers only — each skill is the authority on its own subject, so read the
relevant one rather than assuming; nothing here restates their contents.

| Skill | Read it before |
|---|---|
| `commit` | any commit — it gates the companions (docs, tests, generated files, setup) that must travel in the same commit, the prose trim, and the subject format |
| `git-push` | pushing a branch, or refreshing an existing PR |
| `open-pr` | opening a new PR — title convention and the body sections |
| `merge-pr` | merging |
| `create-amp-issue` | filing or updating a GitHub issue for **AMP** work (`amp_*` label + `AMP:` milestone) |
| `resolve-pr-comments` | closing out a review — resolving addressed threads, and 👍/👎 on bot findings |

Those are the ones that apply to meko work. Two further skills — `amp-mcp-tools` and
`integration-before-corruption-claim` — apply only when editing `amp/**`; the root
[`AGENTS.md`](../AGENTS.md) carries the complete index.

`.claude/settings.json` wires the branch-name hook (see Branch naming above).
