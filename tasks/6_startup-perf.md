---
status: pending
severity: medium
area: yb-pageserver
opened: 2026-05-15
---

# Phase 6 — Startup performance

Cold-start time-to-first-`getPage` is dominated by sequential setup
the layer-stack design baked in but yb-mode no longer needs.

## Items to investigate

- **(a)** Drop the `list_remote_timelines` blob-prefix scan entirely
  in yb-mode and source the timeline list from
  `pageserver.timeline_state` (one indexed query vs N blob LIST + GET
  round-trips).
- **(b)** Parallelise per-timeline attach (`load_remote_timeline` is
  already async but is called serially in the `sorted_timelines`
  loop).
- **(c)** Skip the local-disk dentry-purge in yb-mode (no layer files
  to reconcile).
- **(d)** Shorten the walreceiver catch-up from `disk_consistent_lsn`
  — currently a tenant restart re-applies the entire post-flush
  window even though most of it is cache-resident on the previous
  pageserver. Could be near-instant after Phase 4f's per-key LRU
  lands.

## Target

Sub-second time-to-first-`getPage` on a clean restart for a tenant
with one timeline + a few hundred MB of recent writes.
