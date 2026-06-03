---
status: done
severity: low
area: yb-pageserver
opened: 2026-05-15
closed: 2026-06-03
---

# Phase 6 — Startup performance

Cold-start time-to-first-`getPage` was dominated by sequential setup
the layer-stack design baked in but yb-mode no longer needs.

## Resolution

- **(a)** `Tenant::preload` is now a single
  `PageStore::list_timelines(tenant_id)` SQL query. No controller
  LIST, no per-timeline `download_index_file`, no manifest fetch.
  `synth_index_part_from_row` builds an empty `IndexPart` per row
  (commit `8e9601df3`).
- **(c)** `clean_up_timelines` and the per-timeline
  `create_dir_all(timeline_path)` deleted entirely — yb-mode never
  writes layer files, so there were no dentries to create or purge
  (commit `8e9601df3`).
- **(b)** dropped — sibling-parallel attach isn't worth the
  complexity; the per-timeline work is cheap and tenants with many
  parallel branches aren't the common case.
- **(d)** dropped — the original framing
  (`last_applied_lsn` separate from `disk_consistent_lsn`) was
  muddled. The two are the same in our design: nothing is durable
  until flush, so the only correct restart point IS the flush
  watermark. The real lever for shrinking restart replay is the
  per-key LRU lazy-replay angle, which lives elsewhere.
