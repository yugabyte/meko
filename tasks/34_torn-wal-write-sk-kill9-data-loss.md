---
status: pending
severity: highest
area: yb-pageserver / safekeeper
opened: 2026-06-11
---

# Torn WAL write: SK kill-9 recovery loses acked committed rows + trips `durable_walk_unflushed_row`

Found by the new e2e reproducer
`test/e2e/src/scenarios/fast/torn_wal_write.rs` (scenario A): SIGKILL the
safekeeper mid-flush while a `INSERT id=i; COMMIT;` loop runs (each commit
synchronously durable), restart the SK, let the surviving compute's
walproposer reconnect, then read durable state from a fresh basebackup.

## Symptom

Two distinct corruption signals, both from the crash-recovery path:

1. **Intermittent loss of acked committed rows.** The counter table comes
   back non-contiguous: across four runs the read-back was
   `count=677 max=2117`, `692/2132`, `802/2260` (loss), then `1452/1452`
   (clean) — timing-dependent. When it loses, the survivors are a
   contiguous *suffix* (`min(id)=1459, max=2260, count=802`): the *early*
   committed rows are gone, later ones intact. Each lost row's `COMMIT`
   had returned to the client (synchronous), so this is loss of
   acknowledged data, not an expected tail truncation.

2. **`pageserver_yb_correctness_errors_total{kind="durable_walk_unflushed_row"}`**
   fires (14 in one run) at the suite Final verdict — the PS cache-only
   fast path saw an unflushed row at/below an LSN it believed durable
   (`unified_pages.rs::lookup_chain_durable_if_complete`). Per that
   function's own doc this is either the benign attach-replay race or a
   real flush-horizon violation ([task 28](28_walredo-corruption-catch-me-if-you-can.md)
   bug-B family). The restart-heavy torn-write test trips it where the
   ordinary restart scenarios (`restart_correctness`,
   `restart_wal_replay_yb`) leave it at zero, which points past benign.

## Forensics (one captured failure)

- `public.wals` for the timeline started at `chunk_offset=5506224`
  (≈ LSN `0/15404B0`), **not 0** — the early WAL had already been trimmed
  from `wals`.
- SK restart log: `YbWalStorage::new — rehydrated write_lsn=0/162BFF0,
  cfile_timeline_start_lsn=0/15404B0`; the manager then reports
  `remote_consistent_lsn=0/15404B0`. The SK's earliest *available* WAL is
  `0/15404B0`, so it can no longer serve anything below it.
- `pageserver.pages` held all 126 heap blocks (0..125) for the relation,
  but `heap_page_items(get_raw_page('torn_sk', 0))` returned **0 physical
  tuples** (block 100 had its 18). The early heap pages are physically
  empty — the inserts for ids 1..1458 were never durably materialised,
  yet their WAL was trimmed.
- After the SK restart the PS reconnected reporting
  `remote_consistent_lsn=0/0`, then streamed only `[0/15404B0 ..
  0/176A688]` (ids 1459+). It never re-applied `[.. 0/15404B0]` because
  the SK could no longer serve it → the early inserts are gone for good.

## Hypothesis

The pageserver advances `remote_consistent_lsn` (the SK's `wals`-trim
signal, via `remove_wal::calc_horizon_lsn = min(cfile_remote_consistent_lsn,
flush_lsn)`) ahead of durably persisting those pages to `pageserver.pages`.
On a live PS this is masked — the un-persisted pages stay in the in-memory
cache, so reads succeed. The SK kill-9 forces the PS to re-stream from the
SK's trimmed start, exposing that the early pages were never durable.

Distinct from [task 24](24_early-clog-slru-deltas-lost.md): that was SLRU /
CLOG *visibility* loss (tuples physically present but read as aborted);
here the heap tuples are physically **absent**. Related to
[task 23](23_sk-kill9-wal-hole-flake.md) (SK kill-9 "YB WAL hole" — fixed
the loud walreceiver-wedge path; this is the silent-loss sibling) and
[task 28](28_walredo-corruption-catch-me-if-you-can.md) (flush-horizon
violations surfaced by the same `durable_walk_unflushed_row` counter).

## Repro

`test/e2e/run.sh torn_wal_write` (fast). Scenario A's contiguity assertion
is timing-flaky; the `durable_walk_unflushed_row` Final-verdict counter is
the more reliable catch. `-n 5` to hit the contiguity hole directly.
Scenarios B (compute kill-9 mid-commit) and C (both killed together)
currently come back contiguous — the loss is specific to the SK-only
crash with a surviving compute.

## Where to look

- PS `remote_consistent_lsn` / `disk_consistent_lsn` advancement vs the YB
  page flush: `pageserver/src/page_writer.rs` flush horizon,
  `pageserver/src/unified_pages.rs` `flush_horizon_lsn`. The trim signal
  must not advance past what is durably in `pageserver.pages`.
- SK `safekeeper/src/remove_wal.rs::calc_horizon_lsn` — what drives the
  `wals` trim, and whether the PS feedback it trusts is the *durable*
  (YB-persisted) LSN or merely the applied/received LSN.
- `safekeeper/src/yb_wal_storage.rs` `YbWalStorage::new` rehydration +
  `delete_after` at restart.
