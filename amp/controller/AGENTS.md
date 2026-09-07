# Agent guide — amp controller

`controller` is the YB Amp control plane (the "amp controller" /
`amp-controller`): the control plane for a single placement group. Start
with the design docs in [`architecture/`](architecture/):

- [`architecture/ac-highlvl.md`](architecture/ac-highlvl.md) — PRD summary:
  the resource ladder, the core workflows, and the state tables.
- [`architecture/amp-controller.md`](architecture/amp-controller.md) —
  high-level design & guiding principles (API-first, the task framework,
  managed services, deployment).
- [`architecture/ac-tasks.md`](architecture/ac-tasks.md) — the write-ahead task
  framework's design and **step catalog** (kinds, args, idempotency contract, the
  task plans that compose them) — what `amp-controller.md` §8 defers to.
- [`architecture/ac-locking.md`](architecture/ac-locking.md) — the two locks,
  both `pkg/pglease` leases: `task:<id>` so two instances cannot execute one task,
  `endpoint:<id>` so two blocking tasks cannot touch one endpoint. Claiming,
  reclaim after a crash, and why losing a lease exits the process.
- [`architecture/ac-endpoint-upgrade.md`](architecture/ac-endpoint-upgrade.md) —
  **proposed** (#809): moving deployed endpoints onto a target version. A parent
  task that owns rows in a new `subtasks` table and runs them itself, so one fleet
  roll holds one task lease rather than one per endpoint. Not built yet.
- [`architecture/ac-apis.md`](architecture/ac-apis.md) — the REST API: the
  OpenAPI v3 spec as source of truth, code generation, the served explorer, and
  how the API is tested through the generated client.
- [`architecture/ac-k8s-deploy.md`](architecture/ac-k8s-deploy.md) — how the
  controller deploys endpoints (the Helm-backed Kubernetes deployer and its
  Docker sibling behind `deploy.Deployer`, the synchronous deploy/retire path,
  the `--deploy-driver` selector, the recipe/bootstrap model, internal-LoadBalancer
  endpoints, and the current deployment assumptions).
- [`architecture/controller-cluster-create.md`](architecture/controller-cluster-create.md) —
  creating a cluster, patterned on `yb_amp_local`: declare a recipe at create,
  materialize the storage layer at the first endpoint.
- [`architecture/ac-mcp.md`](architecture/ac-mcp.md) — the MCP server
  ([`mcp/`](mcp/)): a thin FastMCP wrapper exposing the REST API as 18 MCP tools
  for AI agents, its transports (stdio/http), config, and deployment as its own
  pod in the `yb-amp-controller` chart.
- [`architecture/ac-metrics.md`](architecture/ac-metrics.md) — the metrics
  pipeline (scrape Prometheus → OTel Collector → store): the controller's
  `/metrics` endpoint and the `amp_controller_*` series it exposes
  ([`internal/metrics`](internal/metrics/metrics.go)), plus the Collector and
  compute `postgres_exporter` follow-on steps.
- [`architecture/ac-logs.md`](architecture/ac-logs.md) — the log pipeline
  (container stdout → filelog daemonset → OTLP → VictoriaLogs → the
  `amp — Endpoint logs` dashboard): why it tails `/var/log/pods` instead of
  exporting in-process, the collector's receiver/processor anatomy, and how its
  labels line up with the metrics ones.

This guide is scoped to `controller/`. The repo-wide guide is the top-level
[`AGENTS.md`](../AGENTS.md); where they overlap, the rules here win for code
under `controller/`.

## Conventions

- **Log through `internal/log`, not `log/slog` directly.** Get a logger from
  `log.New` (the root logger, built from config) or `log.FromContext` (a
  request/task-scoped logger) — never `slog.New`, a hand-built `slog.Handler`,
  or `slog.Default()` as your source. Stamp scoped loggers onto the context with
  `log.IntoContext`, and use the canonical attribute-key constants
  (`log.KeyTaskID`, `log.KeyClusterID`, `log.KeyRequestID`, …) so records stay
  consistent and queryable. You'll still use the `slog.Attr` / `slog.Level`
  value types and `*slog.Logger` is what `internal/log` hands back — that's
  expected; the rule is about where the logger *comes from*, so every component
  shares one configuration, one format, and one set of keys.

- **`pkg/` is importable, `internal/` is not.** Non-test code under
  [`pkg/`](pkg/) is a library: it must not import `internal/`, must not depend on
  controller wiring, and should be usable on its own. Put anything that is part of
  the controller proper in `internal/`. Test files may reach for `internal/`
  helpers — they are not part of an importer's dependency graph, so a live-database
  test using `internal/db` costs a consumer nothing. `pkg/pglease` is the first of
  these and currently has no in-tree caller.

  **Logging is the one carve-out from the rule above**, and it cuts the other way:
  a `pkg/` library cannot reach `internal/log`, and it must **not** fall back to
  `slog.Default()` either — a library that logs through a process-wide global
  reports through whatever its importer happened to install, which is the same
  inconsistency that rule exists to prevent, only harder to see. Take a
  `*slog.Logger` from the caller and default to `slog.New(slog.DiscardHandler)`:
  silence is honest, and a caller that wants the records passes one in.

- **Keep `controller/architecture/*.md` in sync with the code.** Any change under
  `controller/` that alters behavior, the API surface, the database schema/migrations,
  or the task model must update the matching architecture doc in the *same*
  change. A code change with no doc change usually means the doc is now stale —
  treat fixing it as part of the work, not a follow-up.

## Build / test

From the repo root: `make controller`, `make controller-vet`, `make controller-test`. Or directly:
`go -C controller build ./...`, `go -C controller vet ./...`, `go -C controller test ./...`.

The MCP server under [`mcp/`](mcp/) is a separate **Python/`uv`** project (not
part of the Go module): test it with `uv run --project controller/mcp pytest`.

## API code generation

The REST API is **spec-driven**: `internal/api/openapi_spec.yaml` is the source
of truth, and the models, the echo server interface, and the Go client are
generated into `internal/api/api.gen.go`. After editing the spec, run
`make controller-generate` (`go -C controller generate ./...`) and commit the regenerated file.
Don't hand-edit `api.gen.go`. The controllers (`internal/api/handlers.go`)
implement the generated `StrictServerInterface`, so a spec change that drops or
reshapes an operation fails the build until the handlers match. Keep
`architecture/ac-apis.md` in sync with the spec.

**The spec has a second consumer outside this module.** `ycp/internal/ampclient/`
generates the YCP reconcile agent's client from a vendored **copy** of it, because ycp
is a separate Go module that must not cross-import this one. So editing the spec means
also running `ycp/internal/ampclient/sync-spec.sh` and committing its result **in the
same PR** — a unit test and the `meko_ycp` CI job fail otherwise. A stale copy compiles
and passes everything, so nothing else catches it; it once sat two features behind.
