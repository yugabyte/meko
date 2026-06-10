---
status: pending
severity: high
area: safekeeper
opened: 2026-06-10
---

# Flaky: SK kill-9 recovery hits "YB WAL hole" at the PS resume LSN

Observed once in `test/e2e/slow/restart-wal-replay.sh` (run
`20260609-235354`, during task 22 verification; passed on immediate
rerun and in subsequent runs).

Sequence: PS stopped immediate → 10 s WAL gap built on the SK → SK
stopped `-m immediate` → SK + PS restart → endpoint restart. The PS
walreceiver then failed repeatedly with:

```
WalReader::read failed at start=0/1758600 (chunk_end=0/1778000…)
YB WAL hole at segno 1 offset 7702016 (neither cache nor `wals` has a covering chunk)
```

`0/1758600` is exactly the PS's `consistent_lsn` (its stream resume
point). The endpoint's basebackup then deadlocked waiting for
`last_record_lsn` to advance, and the test timed out after 60 s.

Hypotheses to check (all in `safekeeper/src/yb_wal_storage.rs`):
- SK acks/commits WAL whose chunk hasn't been flushed to `public.wals`
  yet; `-m immediate` loses the cache tail. The PS (which already
  applied + page-flushed those records) resumes ABOVE the SK's durable
  WAL end and the read at the resume position finds no covering chunk.
  Recovery would require the compute's walproposer to re-send — but
  the compute is down, waiting on basebackup → deadlock.
- WAL truncation driven by `remote_consistent_lsn` removing the chunk
  that CONTAINS the resume LSN (boundary off-by-one at chunk
  granularity): `fetch_chunks_from_yb` treats "first chunk starts
  after pos" as a hole even when all data ≥ pos exists.

The `chunk_end=0/1778000 > start=0/1758600` detail in the error leans
toward the truncation-boundary hypothesis. Either way the failure is
in the SK WAL store + truncation feedback loop, which
`architecture/yb-pageserver.md` already lists as test debt.

Repro attempt: loop `test/e2e/run.sh slow` (or just the
restart-wal-replay script) — the window is the SK chunk-flush cadence
vs. the `-m immediate` kill.
