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
| `api_server/` | Go (net/http + Swagger) | REST API on `:25433`. Owns datapacks/agents/knowledge-bases, Cognito auth, Langfuse provisioning, S3 uploads, RAG indexing. Runs migrations on boot. |
| `inference_gateway/` | Python 3.11+ (FastAPI + LangChain) | Playground chat backend on `:8088`. Wires LLM (OpenAI / Bedrock) → Meko MCP server for memory + conversations. |
| `meko-provisioner/` | Go (Kubebuilder operator) | K8s operator that reconciles `MekoUserAccount` CRs into per-tenant namespaces (MCP server, RAG worker, Langfuse). See `meko-provisioner/AGENTS.md` for kubebuilder rules. |
| `meko_ui/` | TypeScript (React 19 + Vite + Tailwind + Drizzle) + Node Express server | Web UI; Express server in `meko_ui/server/` proxies to `api_server`. |
| `system_agent/` | Python (LangChain + mem0 + Streamlit) | Standalone agent + Streamlit playground; uses pgvector + BM25 over YugabyteDB. |
| `ycp/` | Go (net/http, own `go.mod`) | The BYOC **data plane** for YCP (Yugabyte Control Plane): the pull agent (`cmd/agent`), the per-datapack MCP server (`cmd/dpmcp`), the wire types both sides share (`internal/api`), and the kind e2e. The control plane itself is `api_server --control-plane-mode`. See `ycp/ARCHITECTURE.md` and `ycp/DEPLOY.md`. |
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

# ycp — unit tests need nothing running
(cd ycp && go test ./...)
# build + deploy end-to-end: builds the api-server and agent images, stands the
# control plane up on a throwaway kind cluster, and asserts register -> enroll ->
# READY -> restart-without-re-enrolling. Needs kind + docker or podman.
(cd ycp && ./test/e2e/run.sh)

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
| `commit` | any commit — it gates the companions (docs, tests, generated files, setup) that must travel in the same commit, and the subject format |
| `git-push` | pushing a branch, or refreshing an existing PR |
| `open-pr` | opening a new PR — title convention and the body sections |
| `merge-pr` | merging |
| `create-amp-issue` | filing or updating a GitHub issue for **AMP** work (`amp_*` label + `AMP:` milestone) |

`.claude/settings.json` wires the branch-name hook (see Branch naming above).
