# AGENTS.md

This file provides guidance to AI coding agents when working with code in this repository.

## Repository Layout

Meko is a polyglot monorepo. Each top-level directory is an independently-built component; they share only the YugabyteDB schema and the MCP/HTTP contracts between them.

| Path | Language | What it is |
|------|----------|-----------|
| `cli_app/` | Go (cobra + bubbletea TUI) | The `meko` CLI binary — entry point for end users. Talks to `api_server`. |
| `api_server/` | Go (net/http + Swagger) | REST API on `:25433`. Owns datapacks/agents/knowledge-bases, Cognito auth, Langfuse provisioning, S3 uploads, RAG indexing. Runs migrations on boot. |
| `inference_gateway/` | Python 3.11+ (FastAPI + LangChain) | Playground chat backend on `:8088`. Wires LLM (OpenAI / Bedrock) → Meko MCP server for memory + conversations. |
| `meko-provisioner/` | Go (Kubebuilder operator) | K8s operator that reconciles `MekoUserAccount` CRs into per-tenant namespaces (MCP server, RAG worker, Langfuse). See `meko-provisioner/AGENTS.md` for kubebuilder rules. |
| `meko_ui/` | TypeScript (React 19 + Vite + Tailwind + Drizzle) + Node Express server | Web UI; Express server in `meko_ui/server/` proxies to `api_server`. |
| `system_agent/` | Python (LangChain + mem0 + Streamlit) | Standalone agent + Streamlit playground; uses pgvector + BM25 over YugabyteDB. |
| `docs/` | Hugo | Public documentation site. |

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

`.golangci.yml` at repo root governs `cli_app` and `api_server`. Note `goimports.local-prefixes: meko` — local imports go in their own group. Line limits: **120 for Go, 100 elsewhere**. Tabs for Go, 4 spaces for Python, 2 spaces for YAML/shell (see `.editorconfig`).

## Database

YugabyteDB YSQL (PostgreSQL-wire-compatible) on `:5433`. The `api_server` runs migrations on every startup against the system schema. Tenant data lives under `meko_system.*` (registries) plus per-datapack databases (`account_id_<datapack-name>`).

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

## Pull requests

**Title** must carry the tracking ticket / JIRA number so the change is traceable
— e.g. `[MEKO-123] meko_ui: make Create Cluster pg_version selectable`.

Write PR descriptions with these sections (drop one only when it truly doesn't
apply, and say so):

- **Summary** — 1–3 sentences: what changed and the user-facing effect, not the diff.
- **Files changed** — one bullet per file, 1–2 lines each (group by component when the PR spans several).
- **Testing** — what you ran (`go build`/`go vet`/`golangci-lint`, `npm run check`) and what still needs manual verification.
- **Config / env vars** — any new/required env var or flag, where it's set, and the default/degraded behavior when unset.
- **Math / derivation** — only when the PR computes something: the formula, where the numbers come from, a tiny worked example, and what's measured vs modeled.
- **Deploy notes** — only when runtime setup changes: migrations, required env, ordering.

Keep it crisp; link related PRs/issues with full URLs, never bare `#123`. See
the Git Commit Guidelines below (no AI attribution in commits or PR descriptions).

## Git Commit Guidelines

- Do not mention any AI assistant in commit messages, PR descriptions, or code comments.
- Do not include `Co-Authored-By` lines referencing AI assistants.
- Write commit messages as if authored entirely by the developer.
