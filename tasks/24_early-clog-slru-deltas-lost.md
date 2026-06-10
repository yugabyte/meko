---
status: pending
severity: highest
area: yb-pageserver
opened: 2026-06-10
---

# Early-window CLOG (SLRU) deltas never persist to `pageserver.slru_pages`

**Pre-existing** (reproduced on `78bcaf44d`, before task 22) — exposed
by the branch-endpoint work, which is the first consumer to read CLOG
from the pageserver at an old LSN.

## Symptom

Commits made in roughly the first minute / first ~240 KB of WAL after
`tenant create` never get their `ClogSetCommitted` SLRU deltas
persisted. The `pageserver.slru_pages` chain for clog blk 0 is:

```
image  @ initdb_lsn      (bootstrap import of pg_xact/0000)
<nothing for all compute-startup + early user commits>
delta  @ ~initdb+0x3B000 (first survivor; later commits all persist)
```

Verified identically on the pre-task-22 baseline (one cluster: image
@0x1540438, first delta @0x1579ed0, seed commits at 0x154e8c8–0x1555a30
have no deltas) and on task-22 HEAD (image @0x1540438, first delta
@0x157bf20). New commits persist fine (live-probed: +1 row per commit).
Rel-page deltas from the SAME transactions persist at their original
LSNs, so ingestion ran — only the SLRU rows are missing.

## Impact

A FRESH compute (basebackup) reading those xids' tuples without hint
bits sees them as ABORTED → committed data invisible. Masked on the
main timeline because long-running computes have local pg_xact and
recycled computes get heap pages whose hint bits have since reached
the pageserver. A BRANCH at a pre-hole LSN (task 22 topology) is fully
exposed: `relation does not exist` for tables created in the window
(pg_class tuples invisible).

Repro: `tenant create` → `endpoint start` → create table + insert +
checkpoint → `timeline branch` → endpoint on branch → table invisible.
`test/e2e/fast/branch-endpoint.sh` works around it with
`VACUUM FREEZE` on the seed table (remove that when this is fixed).

## Where to look

- `walingest.rs::ingest_xact_record` puts `ClogSetCommitted`
  unconditionally → `PageWriter::put` routes `is_slru_block_key` to
  the aux buffer → `flush()` snapshots and drains it. Code reads
  clean; the loss must be flow-specific to the first window.
- The cutoff is at a consistent WAL OFFSET (~initdb + 0x3B000) in two
  unrelated clusters — suggests a boundary effect (first walreceiver
  catch-up batch? interpreted-WAL-reader chunking? something replayed
  from `consistent_lsn` after the bootstrap freeze_and_flush?).
- Also check the benign-looking bootstrap WARN: `advance_consistent_lsn
  failed: no timeline row … called before timeline_create wrote the
  row` — a flush in that window may drain aux buffers whose rows are
  then never re-sent.
