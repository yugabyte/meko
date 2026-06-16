# Agent guide — amp controller

`amp` is the YB Light control plane (the "amp controller" /
`yblight-controller`): the control plane for a single placement group. Start
with the design docs in [`architecture/`](architecture/):

- [`architecture/yblc-highlvl.md`](architecture/yblc-highlvl.md) — PRD summary:
  the resource ladder, the core workflows, and the state tables.
- [`architecture/yblight-controller.md`](architecture/yblight-controller.md) —
  high-level design & guiding principles (API-first, the task framework,
  managed services, deployment).

This guide is scoped to `amp/`. The repo-wide guide is the top-level
[`AGENTS.md`](../AGENTS.md); where they overlap, the rules here win for code
under `amp/`.

## Conventions

- **Log through `internal/log`, not `log/slog` directly.** Get a logger from
  `log.New` (the root logger, built from config) or `log.FromContext` (a
  request/task-scoped logger) — never `slog.New`, a hand-built `slog.Handler`,
  or `slog.Default()` as your source. Stamp scoped loggers onto the context with
  `log.IntoContext`, and use the canonical attribute-key constants
  (`log.KeyTaskID`, `log.KeyTenantID`, `log.KeyRequestID`, …) so records stay
  consistent and queryable. You'll still use the `slog.Attr` / `slog.Level`
  value types and `*slog.Logger` is what `internal/log` hands back — that's
  expected; the rule is about where the logger *comes from*, so every component
  shares one configuration, one format, and one set of keys.

- **Keep `amp/architecture/*.md` in sync with the code.** Any change under
  `amp/` that alters behavior, the API surface, the database schema/migrations,
  or the task model must update the matching architecture doc in the *same*
  change. A code change with no doc change usually means the doc is now stale —
  treat fixing it as part of the work, not a follow-up.

## Build / test

From the repo root: `make amp`, `make amp-vet`, `make amp-test`. Or directly:
`go -C amp build ./...`, `go -C amp vet ./...`, `go -C amp test ./...`.
