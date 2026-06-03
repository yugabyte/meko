---
status: pending
severity: high
area: yb-pageserver
opened: 2026-06-02
---

# Data-modeling review for the YB-backed pageserver tables

## Motivation

`pageserver.pages`, `pageserver.slru_pages`, `pageserver.meta`, and
`pageserver.timeline_state` were defined as the minimum schema
needed to port Neon's key/LSN-keyed value store onto YugabyteDB.
They work, but the shape was driven by "match what Neon's
`KeySpace + LSN` API expects" rather than by "what reads and writes
YB does well." Worth a structured review now that we have:

- A clear materializer/read-path access pattern (anchor-CTE + delta
  walk; range scans across blknum within a relation).
- A clear write-path access pattern (high-fanout INSERTs from
  walreceiver flushes, batched and parallelised by chunk).
- Real failure modes (Phase 4.k schema-bootstrap wedge, the
  `is_image=TRUE` anchor index that misses `will_init` deltas, the
  mixed-key index column situation in `pages`).
- Operational signals worth tabulating (tablet hotspotting under
  walreceiver bursts, large-payload BYTEA storage cost).

## Scope

A non-exhaustive sketch of what a focused review should look at —
each item is a candidate; some will turn out to be fine as-is.

### `pageserver.pages`

- **Partition key.** Today: `(tenant_id, timeline_id, spcnode, dbnode,
  relnode, forknum)` HASH. Per-relation hashing — every key in one
  relation lands on the same tablet. Hot-table writes (CREATE TABLE
  + INSERT-heavy) localise to one tablet's leader. Should we
  split by adding `blknum / N` to the hash?
- **Clustering.** Today: `(blknum, is_image, lsn DESC)`. The
  `is_image` column is in the cluster key because the anchor CTE
  filters on `is_image=TRUE`; but the chain walk needs the latest
  `will_init=TRUE` row, which can be an FPI delta
  (`is_image=FALSE, will_init=TRUE`). Either revisit the anchor
  query to use `will_init` instead, or carry both in the cluster
  key.
- **`will_init` column.** Could be folded into a single
  `anchor_kind` enum: `none | image | fpi-init`. Saves a bit of
  storage; clarifies intent.
- **Payload size distribution.** The 85-byte typical delta vs the
  occasional 8 KiB FPI — does YB handle this efficiently as
  `bytea` in the same column, or is there a better layout
  (`is_image = TRUE` rows in a sibling table)?

### `pageserver.slru_pages`

Same shape as `pages` but with SLRU-specific columns. If `pages`
moves, this should move in lockstep.

### `pageserver.meta`

Full-overwrite blobs; no walredo. Currently keyed by
`(tenant_id, timeline_id, key, lsn)` with `lsn DESC` clustering.
Read pattern is always "latest at or below target_lsn"
(`LIMIT 1`). Could we drop the LSN-DESC clustering and instead
maintain a `latest_lsn` per `(tenant, timeline, key)` row pair
to avoid the range scan? Storage trade-off vs read-latency
trade-off.

### `pageserver.timeline_state`

One row per timeline, generation-fenced UPDATEs. Currently
indexed by `(tenant_id, timeline_id)`. The Phase 4.k wedge stems
from `storage_controller`'s migrations running against the same
DB; this task should consider whether `pageserver.*` and
`storage_controller.*` should share a database at all, or live in
separate ones for clean lifecycle.

### Cross-table

- Are tablet counts right? Default YB tablet split policies may
  produce too few (hot tablets under burst) or too many (per-row
  metadata overhead).
- TTL / GC. No data lifecycle policy today; pages accumulate
  forever (task 7 is the cron-prune placeholder). What does a YB-
  native retention look like — `TIMESTAMP` columns + periodic
  delete, or row-version GC?

## Plan

This task is the **scoping** of the review, not the
implementation. Deliverable:

1. Walk every column of every table, document what it's for and
   what access patterns hit it.
2. Profile real workloads (the e2e workload is a starting point)
   and capture per-table read/write hotness, tablet distribution,
   payload-size distribution.
3. Produce a proposal listing concrete schema changes worth
   making, with size-of-prize estimates.
4. Each accepted proposal becomes its own task — this one stays
   high-priority until the proposal lands.

## Notes

- Reference write-up on the two hot `pageserver.pages` read shapes
  and the anchor/will_init clustering trade-off:
  [pages CTE optimization](14_ref_pages_cte_optimization.md).
- Cross-link to [4.k storage_ctl startup wedge](4.k_neon-local-start-wedge.md)
  and [10 Diesel migrations collapse](10_collapse-storage_ctl-diesel-migrations.md);
  the schema-bootstrap pain points are part of the data-modeling
  conversation.
- `architecture/yugabyte.md` is the current schema reference;
  any output from this review updates it.
- Touch lightly until **task 12 (e2e test infra)** is solid —
  schema changes need fast end-to-end validation to land safely.
