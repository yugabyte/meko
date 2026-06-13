---
status: pending
severity: medium
area: storage_ctl / yb_light_local
opened: 2026-06-12
---

# GC the data behind dropped branches

`yb_light_local timeline drop` (and the controller's
`PUT /control/v1/tenant/:tenant_id/timeline/:timeline_id/drop`) tear down a
branch's *compute*: they stop the endpoint processes, deprovision the
co-located pageserver/safekeeper, delete the endpoint records, discard any
un-materialized init recipe, and mark the timeline **dropped** by setting
`timelines.deleted_at`. A dropped timeline no longer surfaces as a live branch
(`list_timeline_branch_names` filters `deleted_at IS NULL`) and is not brought
back on restart.

What it deliberately does **not** do: garbage-collect the durable data. After a
drop, two things linger:

1. The `storage_controller.timelines` row (now with `deleted_at` set).
2. The branch's pages/WAL in YugabyteDB. Note a child branch shares its
   ancestor's pre-branch data via lineage-aware reads (nothing is copied at
   branch time — see [29](29_pending-init-to-yb.md)), so GC must only reclaim
   data **not** reachable from any live timeline.

## Plan

Add a GC pass (periodic or triggered) that, for each timeline with
`deleted_at` set:

- Confirms no live timeline depends on its data (lineage check).
- Reclaims the unreachable pages/WAL.
- Hard-deletes the `timelines` row. The persistence hard-delete already gates
  on the marker: `delete_timeline` (`storage_controller/src/persistence.rs`)
  only removes rows where `deleted_at IS NOT NULL`.

## Notes

- Relates to the broader [7](7_gc.md) (general pageserver GC), which is
  currently deferred. This task is narrower: reclaiming **dropped-branch** data
  specifically.
- The drop path itself reuses `deleted_at` rather than introducing a new
  "dropped" state, so this GC can treat all `deleted_at`-marked timelines
  uniformly regardless of whether they came from `timeline drop` or the
  heavyweight `tenant_timeline_delete` flow.
