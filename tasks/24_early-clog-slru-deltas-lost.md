---
status: done
severity: highest
area: yb-pageserver
opened: 2026-06-10
closed: 2026-06-10
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
`test/e2e/fast/branch-endpoint.sh` is the regression check (it seeds
without `VACUUM FREEZE`, so the branch reads the seed xids' CLOG from
the pageserver).

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

## Root cause

`PageWriter::flush()` (`pageserver/src/page_writer.rs`) had no
serialization. The periodic flusher (`spawn_flusher`) and every
`freeze_and_flush` caller — checkpoint, basebackup, bootstrap
import-finish, HTTP, shutdown — call `pw.flush()` from independent
tasks. `flush()` snapshots the aux (SLRU + meta) buffer in one critical
section and drains it **by count** (`drain(0..slru_count)`) in a later
one. When two flushes overlap, the second's `slru_count` was computed
against a buffer the first already drained, so its `drain` removes
fresh, never-written rows → silent loss. Rel-page rows are immune
because they drain by entry **identity** (`mark_flushed(&rel_snapshot)`)
— exactly the "rel persists, SLRU lost" asymmetry. Overlap spikes at
startup (the import-finish `freeze_and_flush` and the compute's initial
checkpoint race the periodic flusher), so only the first window loses
deltas; later commits flush serially and persist. SLRU is the visible
victim because SLRU rows are append-only — lost meta rows get masked by
later full-overwrite rows.

## Fix

Added `flush_serialize: tokio::sync::Mutex<()>`; `flush()` holds it
across the whole flush so snapshot + count-based drain are atomic w.r.t.
other flushes. `put()` never takes it (ingest is not serialized against
flush). Verified: `test/e2e/fast/branch-endpoint.sh` passes with the
`VACUUM FREEZE` workaround removed — a fresh branch now reads the seed
xids' CLOG correctly instead of seeing them aborted.
