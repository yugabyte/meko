---
status: in_progress
severity: highest
area: yb-pageserver
opened: 2026-06-11
---

# "Catch me if you can" walredo corruption on long delta chains

Walredo PANICs during page reconstruction over long delta chains,
fail-stopping the PS. Reproducible on `d9641d272` and `b8aa3fac4`
against W10 (1000-warehouse) TPC-C state; not reproduced on
`dde78d810`. Named for how it vanishes the moment you build the
instrumented binary to chase it — the newer build's materializer
drains the dangerous chains into stable image layers, after which even
the old binary runs clean on the same data.

Live source of truth (mirrored verbatim below as of 2026-06-11):
`ec2-user@172.151.20.28:/home/ec2-user/runbooks/walredo-corruption.md`
(key `~/.yugabyte/yb-15-aws-portal-1-key.pem`). Re-pull it for the
latest run state; this task is the checked-in snapshot.

Related: [task 27 — PS cache-walk regression](27_ps-cache-walk-regression.md)
(the `cache_ms` blow-up below is a *separate* slowdown on the same
release line, not this corruption); [task 23 — SK kill-9 WAL hole](23_sk-kill9-wal-hole-flake.md)
(the "YB WAL hole" below is also *not* this corruption).

---

# yb-neon walredo corruption — collected evidence

Last updated: 2026-06-11 (run #6 in progress).

## TL;DR

Walredo PANIC during PS materializer "legacy compaction" on long delta chains,
reproducible on builds `d9641d272` and `b8aa3fac4` against W10 (1000-warehouse)
TPC-C state. **Not reproduced** on instrumented build `dde78d810` across >3 h
cumulative under-load runtime. The PS correctly SIGABRTs on walredo failure per
the redo-failure-is-corruption invariant; no data loss because YB has the
records that the PANIC fired on — they just can't be replayed.

Six older `yb-pageserver` SIGABRT coredumps exist (`coredumpctl list`):
`582e7e4d8`, three on `93c8c29c6`, one on `d9641d272`, two on `b8aa3fac4`.
Whether all six are the same root cause is not certain — the three documented
in detail below share the same materializer-compaction trigger and walredo
PANIC shape; the four older ones (`582e7e4d8` + `93c8c29c6` × 3) just SIGABRT
without preserved log context.

## Crash 1 — `d9641d272` @ 2026-06-08 13:26:09 UTC

Caught mid-execute on the very first W10 corruption-hunt run.

```
2026-06-08T13:26:05.299368Z ERROR wal-redo-postgres-stderr{pid=1417242 ...}:
  received output output=seccomp: bad syscall 186
2026-06-08T13:26:05.300028Z ERROR page_service_conn_main{...}:
  process_query{tenant_id=1d3d54f61c0f20a663964cbf96ffa5ef
                timeline_id=25fe008abdbd26704ee0a6b84af25cd1}:
  handle_pagerequests:request:handle_get_page_request{
    request_id=4302337459880576
    rel=1663/16384/16649 blkno=39662
    req_lsn=FFFFFFFF/FFFFFFFF not_modified_since_lsn=25/50D07958
  }:handle_get_page_at_lsn_request_batched:
  error applying 203 WAL records 7/FE2AC558..7/FE61B448 (13696 bytes)
  to key 000000067F00004000000041090000009AEE during read page,
  from base image with LSN 7/FE0CC830 to reconstruct page image
  at LSN 25/514EE590 n_attempts=0: apply_wal_records

2026-06-08T13:26:05.300424Z ERROR wal-redo-postgres-stderr{pid=1417248 ...}:
  2026-06-08 13:26:05.300 GMT [1417248] PANIC:  failed to add new item
  CONTEXT:  WAL redo at 7/FE2AC558 for Btree/INSERT_LEAF: off: 207

2026-06-08T13:26:05.302263Z ERROR ...handle_get_page_at_lsn_request_batched:
  CRITICAL: [tenant_shard_id, timeline_id] walredo failure during
  page reconstruction: apply_wal_records

2026-06-08T13:26:05.302324Z ERROR ...: error reading relation or page version:
  Read error: reconstruct a page image: apply_wal_records: read walredo stdout:
  early eof
```

| | |
|---|---|
| key | `000000067F00004000000041090000009AEE` (rel `1663/16384/16649` blkno `39662`) |
| chain_len | 204 |
| base_img_lsn | `7/FE0CC830` |
| chain top WAL | `7/FE61B448` |
| materializer target | `25/514EE590` |
| WAL bytes | 13,696 |
| trigger | `read page` (compute-driven `get_page` for `req_lsn=FFFFFFFF/FFFFFFFF`) |
| walredo PANIC | `failed to add new item` on `Btree/INSERT_LEAF off: 207` |

Coredump: `Mon 2026-06-08 13:26:09 UTC pid=1416913 SIGABRT missing` (file gone).
`build_tag: 2026-06-07-d9641d272` commit subject unknown locally.

## Crash 2 — `b8aa3fac4` @ 2026-06-09 10:56:45 UTC

Caught during the immediate-post-W10-load materializer wave (no execute had
started).

```
2026-06-09T10:56:43.731381Z ERROR wal-redo-postgres-stderr{pid=3250339 ...}:
  2026-06-09 10:56:43.731 GMT [3250339] PANIC:  neon_rm_redo: invalid max offset number
  CONTEXT:  WAL redo at 22/439671F8 for neon/INSERT: off: 23, flags: 0x08

2026-06-09T10:56:43.731190Z ERROR ...image_materializer:
  error applying 21 WAL records 22/439671F8..22/43A8BB58 (2709 bytes)
  to key 000000067F000040000000401E00003884ED during legacy compaction,
  from base image with LSN 22/426FE620 to reconstruct page image
  at LSN 22/43A8BB58 n_attempts=0: apply_wal_records
Caused by:
  0: read walredo stdout
  1: early eof

2026-06-09T10:56:43.732076Z ERROR ...:image_materializer:
  ABORT: MATERIALIZER REDO ERROR
  tenant=1c2771481296bc21bd229837d640c437
  timeline=4bf9b49477cc95982d13faea2ef167d7
  target_lsn=22/43A8BB58
  key=b"\0\0\0\x06\x7f\0\0@\0\0\0@\x1e\0\08\x84\xed"
  chain=[... 22 entries ending with base image at 22/426FE620 ...]
```

| | |
|---|---|
| key | `000000067F000040000000401E00003884ED` |
| chain_len | 22 |
| base_img_lsn | `22/426FE620` |
| chain top WAL | `22/43A8BB58` |
| WAL bytes | 2,709 |
| trigger | `legacy compaction` (materializer, NOT compute-driven) |
| walredo PANIC | `neon_rm_redo: invalid max offset number` on `neon/INSERT off: 23 flags: 0x08` |

This is the **`neon` rmgr** (Neon's custom WAL resource manager for materializer
operations), not standard PostgreSQL btree. Different PANIC text but same
"item-offset wrong" failure shape.

`build_tag: 2026-06-08-b8aa3fac4` commit subject:
**"pageserver: Arc<PageRow> through chain reads + flush"** ← prime suspect
for both the crash and the post-crash cache_ms regression (see below).

## Crash 3 — `b8aa3fac4` @ 2026-06-10 13:34:55 UTC

Identical signature to crash 2, on a different tenant. Hit at **+41 s** after
MEASURE start in a W10 execute on a freshly loaded 1000-wh dataset.

```
2026-06-10T13:34:54.080536Z ERROR wal-redo-postgres-stderr{pid=964924 ...}:
  PANIC:  neon_rm_redo: invalid max offset number
2026-06-10T13:34:54.081379Z ERROR wal-redo-postgres-stderr{pid=964932 ...}:
  PANIC:  neon_rm_redo: invalid max offset number
2026-06-10T13:34:54.082012Z ERROR request{method=POST path=/v1/tenant/.../timeline ...}:
  timeline_create:image_materializer:
  ABORT: MATERIALIZER REDO ERROR
  tenant=52b37d78ae200f18a698798650ced5e5
  timeline=5538aef757d0bdff215af822f3d0069c
  target_lsn=22/75CDB1F8
  key=b"\0\0\0\x06\x7f\0\0@\0\0\0@\x1e\0\08\xb2\x90"
  chain_len=29  base_img_lsn=Some(22/75C7CE00)
  chain=[
    (lsn=22/75CDB1F8 is_image=0 will_init=0 pl=169),
    (lsn=22/75CDA500 is_image=0 will_init=0 pl=169),
    (lsn=22/75CDA3D8 is_image=0 will_init=0 pl=169),
    (lsn=22/75CDA2B0 is_image=0 will_init=0 pl=169),
    (lsn=22/75CD6520 is_image=0 will_init=0 pl=169),
    (lsn=22/75CD41E8 is_image=0 will_init=0 pl=169),
    (lsn=22/75CD40C0 is_image=0 will_init=0 pl=169),
    (lsn=22/75CD3F80 is_image=0 will_init=0 pl=169),
    (lsn=22/75CD3E58 is_image=0 will_init=0 pl=169),
    ... 18 more 169-byte deltas ...
    (lsn=22/75C7CE00 is_image=1 will_init=1 pl=8192)
  ]
  apply_wal_records: read walredo stdout: early eof
```

| | |
|---|---|
| chain_len | 29 |
| base_img_lsn | `22/75C7CE00` |
| chain top WAL | `22/75CDB1F8` |
| trigger | `legacy compaction` initiated by `timeline_create` POST during execute warm-up |
| walredo PANIC | same as crash 2 |

Coredump: `Wed 2026-06-10 13:34:55 UTC pid=963887 SIGABRT truncated 10.4M`
(`coredumpctl info 963887` still works).

## Pattern summary

| | Crash 1 | Crash 2 | Crash 3 |
|---|---|---|---|
| Build | d9641d272 | b8aa3fac4 | b8aa3fac4 |
| Date | 2026-06-08 | 2026-06-09 | 2026-06-10 |
| Trigger | read-driven (`get_page`) | materializer (post-load) | materializer (during execute) |
| rmgr | `Btree` | `neon` | `neon` |
| Walredo PANIC | "failed to add new item" | "invalid max offset number" | "invalid max offset number" |
| chain_len | 204 | 22 | 29 |
| All-zero deltas of size 77 stuck in a row | yes | no | no |
| pl=169 deltas in chain | n/a | mostly | almost all |

All three: PS fail-stop is correct (`feedback_redo_failure_is_corruption`). All
three: walredo postgres process couldn't get its PANIC text past seccomp
(`seccomp: bad syscall 186` = `gettid` from the abort path). Only an early
WARNING (`specified item offset is too large`) leaked through to PS log; the
full PANIC message died with the process. The "page-delta hole" / "page hole"
phrasing comes from the visible symptom: PS reports a redo failure on a delta
that can't be applied — looks like a hole in the chain even though all the
bytes are there.

## Reproduction recipe (b8aa3fac4 against W10)

1. Clean & deploy on b8aa3fac4, default config (SB=8GB, PS+SK sf=4.0, LFC=off).
2. `tpcc-run.sh ybLight4 W10 create load` — 8 h with 16-20 loader threads. PS
   survives the load.
3. PS will keep running materializer activity afterward for hours; no crash.
4. Kick off any tpcc execute (`tpcc-run.sh ybLight4 W10 execute`).
   The materializer compacts freshly-loaded WAL chains under the demand for
   `get_page` from compute → PANIC within ~41 s.
   Both successful repros (crash 2: 6 min after load; crash 3: 41 s into
   execute) fired during this first post-load materializer wave.

## What is NOT the corruption (often confused)

### "YB WAL hole" — SK chunk-stitching regression in dde78d810
```
ERROR: WalReader::read failed at start=22/1745ECE0
       YB WAL hole at segno 8727 offset 4582624
       (neither cache nor `wals` has a covering chunk)
```
The chunks ARE in YB (484 contiguous chunks covering the segment). The new SK
in `dde78d810` requires ONE chunk to cover the entire requested read range; old
SK in `b8aa3fac4` stitched multiple. Commit subject:
**"safekeeper: drop the lookback window in fetch_chunks_from_yb"** — that's the
commit that removed the multi-chunk stitching. Mitigation: revert to old SK,
or have new SK write fewer/larger chunks going forward.

### WAL page-padding deadlock
- PS `last_record_lsn = 22/2D045FF0` (end of last record)
- SK `flush_lsn      = 22/2D046000` (end of WAL page incl. 16 B padding)
- gap = start of an incomplete WAL record:
  `length=0x3b(59) prev_lsn=22/2D045FB0`
- `handle_basebackup_request{lsn=22/2D046000}` does `wait_lsn(22/2D046000)`
  which never resolves: PS won't reach that LSN without compute writing a new
  record; compute can't start without basebackup.
- Resolved by `UPDATE public.wals SET chunk_size=8176, data=substring(data FOR 8176)
                 WHERE … segno=8749 AND chunk_offset=278528`
  (truncates the 16-byte incomplete record header). SK rehydrates
  `flush_lsn=22/2D045FF0` on restart.

### cache_ms regression on b8aa3fac4 / dde78d810
Not the corruption, but a separate slowdown in the same release line.
`pageserver_smgr_query_seconds` mean = 87 ms; `perf tag="scan_range"` per-stage:
- Early in run: `total≈yb_ms`, `cache_ms=0`
- Mid-run: `total≈400 ms  yb_ms≈10 ms  cache_ms≈380 ms`

Tracks `pageserver_yb_pagewriter_delta_counts_entries` growing (≈4.5 M / 360 MB
in-memory delta map). Per-get_page cache walk appears O(N) over delta entries.
Pre-Arc<PageRow> builds got 78 % tpmC; current best is 58 % (run #5, post-LFC
warm-up). [Tracked separately as task 27.]

## Status — dde78d810 corruption hunt

`dde78d810` adds:
- `YB_PS_TRACE_REL_BLK=1663/16384/16649/0/*` env var → `target=yb_ps_trace`
  log lines covering each put_pages stage. `decode` without `put_pages_ok` =
  silent drop before YB; `grep 'CRITICAL: put_pages chunk'` = silent drop by YB.
- (Probably also fixes the redo failure but commit subject is SK-side; PS-side
  fix may be in an earlier commit on the same branch.)

| Run | Build mix | Duration | tpmC | PANICs | trace events |
|---|---|---|---|---|---|
| Pre-fix W10 #1 | all b8aa3fac4 | 41 s → crash | n/a | 1 | n/a |
| Pre-fix W10 #2 | all b8aa3fac4 | 41 s → crash | n/a | 1 | n/a |
| Post-fix #1 | dde78d810 PS + b8aa3fac4 rest, LFC off | 1 h 02 m | 899.83 | 0 | 0 |
| Post-fix #2 | same | 1 h 02 m | 824.73 | 0 | 0 |
| Post-fix #3 | dde78d810 PS + b8aa3fac4 rest, LFC=50GB | 1 h 02 m | 2,552.12 | 0 | 0 |
| Post-fix #4 | same | 1 h 02 m | 2,980.78 | 0 | 0 |
| Post-fix #5 | same | 1 h 02 m | 7,439.95 | 0 | 0 |
| OLD smoke | b8aa3fac4 PS (after dde78d810 drained the danger chain) | 10 m | 905.8 | 0 | 0 |

Cumulative dde78d810 PS uptime under W10 execute load: **>3 h 18 m, 0 panics, 0
trace-events, 0 `put_pages CRITICAL`**. The old binary now also runs cleanly
against the same data — because the new build's materializer succeeded in
flushing the dangerous chains into stable image layers.

## Artifacts on disk

- `/home/ec2-user/ps-stack.log` — current PS log (truncated each PS restart).
  Crash-3 log block was preserved in this document before truncation.
- `coredumpctl list` SIGABRT entries on `yb-pageserver`:
  - 2026-06-06 13:50 / 15:29 / 15:43 / 16:17 — older builds, file gone
  - 2026-06-08 13:26 — `d9641d272`, file gone
  - 2026-06-09 10:56 — `b8aa3fac4`, file gone
  - 2026-06-10 13:34 — `b8aa3fac4`, **file truncated 10.4 MB still present**
- yb_storage snapshot (taken by user, location not recorded here) — preserves
  the 1000-wh corruption-trigger state, restorable to redo the experiment.

## Open questions

- Is the PS-side fix on `dde78d810` itself, or earlier on the branch? The
  commit subject is SK-side ("drop the lookback window"); the corruption fix
  must be in another commit between `b8aa3fac4` and `dde78d810`.
- Are the four 2026-06-06 SIGABRTs on `582e7e4d8` / `93c8c29c6` the same root
  cause? File contents gone; only `coredumpctl info` metadata remains.
- Is `cache_ms` blow-up from the Arc<PageRow> change a bug or a tuning issue?
  78 % → 58 % regression. Worth bisecting between `582e7e4d8` and `b8aa3fac4`.
