---
status: done
severity: medium
area: storage_ctl
opened: 2026-06-02
closed: 2026-06-02
---

# Collapse storage_ctl Diesel migrations into one initial schema

## Resolution

`storage_controller/migrations/` is now a single
`00000000000000_initial_schema/{up,down}.sql` that emits the final
schema (10 tables + 1 index + 1 FK + the two diesel helper
functions). The 24 historical per-step migrations were deleted.

Measured impact: fresh `yb_light_local init + start` against an empty
`yb_storage` went from ~3 min (12 dots of `pageserver has not started
yet, continuing to wait` during the 25-step replay) to **16s**
end-to-end on the same dev box.

The Diesel framework wiring (`embed_migrations!`, `__diesel_schema_migrations`,
`run_pending_migrations`) is unchanged — future schema changes still
add timestamped migrations as new directories alongside the initial
one.

---

(Below: original analysis at filing time.)

## Motivation

`storage_controller/migrations/` carries 25 Diesel migrations
inherited from upstream Neon — every schema evolution from
`2024-01-07_create_tenant_shards` through `2025-03-18_timeline_imports`.
Each one runs sequentially at every fresh startup, which against YB
contributes meaningfully to the Phase 4k wedge
(`[1_phase-4k-storage_ctl-startup-wedge.md](1_phase-4k-storage_ctl-startup-wedge.md)`):
~60 s of `CREATE TABLE` / `ALTER TABLE` / `DROP COLUMN` /
`CREATE INDEX` churn before the controller answers HTTP.

We are a fresh product. No external operator runs a 2024-vintage
schema we need to migrate forward from. The full chain is dead
weight — replay history nobody will ever traverse.

## Sketch

- Squash all 25 migrations into a single `00000000000000_initial_schema`
  that emits the *final* DDL (`CREATE TABLE`s in their current shape,
  indexes, NOT NULL constraints — what you'd get if you `pg_dump
  --schema-only` the populated DB after the existing chain runs).
- Drop the per-step files. Keep the Diesel framework wiring so future
  schema changes can still add timestamped migrations.
- Update any test that asserts a specific migration is present.

## Why we can do this safely

- No deployed environments exist that ran an earlier migration tip
  and would need to skip-ahead.
- Diesel records migrations by filename in
  `__diesel_schema_migrations`; a brand-new DB sees only one row and
  is indistinguishable from a fully-migrated old DB at the SQL level.

## Notes

- Cuts ~50–60 s off every fresh `yb_light_local start` against YB and
  removes most of the surface area for the
  `ensure_yb_storage_schema` sibling-wedge documented in
  `1_phase-4k-storage_ctl-startup-wedge.md`.
- Worth doing once the YB-fork branch stops chasing upstream Neon —
  rebasing/squashing migrations against an actively-moving upstream
  is busywork. Defer until the fork is decoupled.
