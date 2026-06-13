---
status: pending
severity: high
area: yb-pageserver
opened: 2026-06-13
---

# Walreceiver wedges re-ingesting SMGR_TRUNCATE_FSM during recovery

## Symptom

During crash recovery the walreceiver permanently wedges on one record,
panicking on every retry and never advancing `consistent_lsn`:

```
panic{thread=walreceiver worker location=.../walreceiver_connection.rs:460}:
  could not ingest record at 0/2B14EC8
Caused by:
  could not find data for key
  [..0003600001ffffffff..] (rel-size, FSM fork, relnode 221184),
  original HWM LSN 0/2B14EC7
```

All panics are at the SAME LSN (timeline stuck), so the watermark never
moves past it — a hard wedge, not a transient. Surfaces in
`test/e2e torn_wal_write` around iteration 4 (now reachable since the
task-34 basebackup corruption was fixed; earlier runs died sooner).

## Root cause (diagnosed, not yet fixed)

`walingest.rs` `SMGR_TRUNCATE_FSM` handling calls
`get_relsize(modification, fsm_rel)` (walingest.rs ~686), which does
`get_rel_exists` then `get_rel_size`. The panic means the two disagree:
`get_rel_exists(FSM)` returns `true` but `get_rel_size(FSM)` returns
`MissingKey` at the resume HWM.

Durable-state forensics (live repro cluster, db
`yb_storage_e2e_4164774_761765178`, timeline `d725f00a…`):
- FSM data AND rel-size keys for relnode 221184 have **min durable LSN
  `0x2b14ec8`** (the wedge record) — nothing earlier was lost.
- `consistent_lsn = 0x2b132e8` sits BETWEEN when the FSM was registered in
  the rel-directory (≤ resume point, so `get_rel_exists` = true) and when
  its rel-size key was first written (`0x2b14ec8`, above resume point, so
  `get_rel_size` = MissingKey). On replay from `0x2b132e8` the directory
  says the FSM exists but its size key isn't durable yet → wedge.

So the FSM rel-directory entry and the FSM rel-size key are durably
inconsistent relative to the gap-free `consistent_lsn` watermark. Same
"recovery read robustness" family as task 34 but a distinct path
(walreceiver ingest, not basebackup). NOT caused by task 34's
`flush_for_basebackup` fix (no over-advance — `consistent_lsn` is correctly
below the FSM creation; different code path).

## Fix direction (for sign-off)

Likely: `SMGR_TRUNCATE_FSM` (and probably `_VM`) ingest should treat a
`get_rel_size` `MissingKey` as "nothing to truncate" — matching the
existing `unwrap_or(0)` intent at the call site — rather than letting it
propagate and wedge the walreceiver. Confirm `get_relsize` should map the
exists=true/size=missing case to `None`. Deeper question worth checking:
why the rel-directory entry becomes durable before the rel-size key (and
whether the watermark should not advance into that gap).

## Repro

`test/e2e/run.sh -n 10 --no-fail-clean torn_wal_write` (needs the task-34
fix present so the run reaches iter ~4). Look for
`could not ingest record … could not find data for key … (FSM)` in the
pageserver log and a stuck `consistent_lsn`.
