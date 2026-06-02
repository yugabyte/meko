---
status: done
severity: medium
area: yb-pageserver
opened: 2026-05-15
---

# Phase 5 — Pageserver restart survives off `pageserver.timeline_state`

Runtime hookup of the per-timeline state row that replaces the
`index_part.json` blob. Four sub-phases:

## 5a — Flush-time dc_lsn UPDATE

`PageWriter` keeps an in-RAM `dc_lsn_watermark`; flush success path
issues a column-scoped `UPDATE timeline_state SET disk_consistent_lsn
= $1 WHERE pk` on the internal conn when the flush's max LSN exceeds
the watermark. In-memory monotonicity guard, no server-side
conditional.

## 5b — Row seeding at timeline creation

`UninitializedTimeline::finish_creation` calls
`Timeline::write_timeline_row_to_yb` (full-row upsert via
`internal_page_store` + `seed_dc_lsn_watermark`) BEFORE taking the
timelines mutex and before `maybe_spawn_flush_loop`. The row is
guaranteed to exist by the time the flusher starts hitting it, so
5a's UPDATE always lands.

## 5c — Attach falls back to timeline_state

When `attach`'s `index_part.json` fetch returns `NotFound`, the
`compute_page_store` path queries `get_timeline(tenant, timeline)`
and synthesises an `IndexPart::empty(metadata)` with
`TimelineMetadata` built from the row's columns.
`last_record_lsn` / `initdb_lsn` aren't persisted (re-derived on
walreceiver resume / stub-set to `disk_consistent_lsn`).

## 5d — Init-and-sync tolerances

`timeline_init_and_sync`'s "no ancestor + no layer files" sanity
check skips in yb-mode (that *is* the normal post-restart state);
`ensure_schema` swallows YB's "type X already exists" so a stale
orphan `pg_type` entry after `DROP DATABASE WITH (FORCE)` doesn't
break startup.

## End-to-end smoke

Cluster up → create tenant → INSERT 5k rows → flush → `dc_lsn`
22283440 → 23216088 in `pageserver.timeline_state` → stop+start
pageserver → `Phase 5c: synthesising IndexPart` fires → attach
completes (`activated_timelines=1`) → reads return correct values.

## Schema rename

`last_record_lsn` column removed from `pageserver.timeline_state`
(re-derived on resume); table renamed
`pageserver.timelines` → `pageserver.timeline_state` to avoid YB
type-name collision with `storage_controller.timelines`.
