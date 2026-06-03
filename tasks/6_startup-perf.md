---
status: pending
severity: low
area: yb-pageserver
opened: 2026-05-15
---

# Phase 6 — Startup performance

Cold-start time-to-first-`getPage` is dominated by sequential setup
the layer-stack design baked in but yb-mode no longer needs.

## Target

Sub-second time-to-first-`getPage` on a clean restart for a tenant
with one timeline + a few hundred MB of recent writes.

## Done

- **(a)** `Tenant::preload` short-circuits to a single
  `PageStore::list_timelines(tenant_id)` query. No controller LIST,
  no per-timeline `download_index_file`, no manifest fetch.
  `synth_index_part_from_row` builds an empty `IndexPart` per row.
- **(c)** `clean_up_timelines` and the per-timeline
  `create_dir_all(timeline_path)` deleted entirely — yb-mode never
  writes layer files, so there are no dentries to create or purge.

## Left

### (b) Parallelise per-timeline attach

`load_remote_timeline` is async but called serially in the
`sorted_timelines` loop. Ancestors must finish before descendants,
but siblings at the same depth can run concurrently.

**Caveat:** the loop body's real work is `Timeline::new` + map
insert — already fast. In the yb-light common case (1 timeline per
tenant) wall-clock savings are zero; benefit only shows for
ancestor-branched tenants.

**Defer** until profiling on a real workload says the serial
attach loop is the bottleneck.

## Suggested next step

Profile a fresh restart against a busy tenant. If we're already
sub-second, close this task — (b) isn't worth implementing without
measurement justifying it.
