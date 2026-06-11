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

Last updated: 2026-06-11 (night). Root cause found AND fixed for the
zero-image shadowing (bug A): crash-replay rel-size cache pin → gap-fill
writes zero-page images over live tail blocks (see "ROOT CAUSE FOUND").
Fixes landed: unconditional cache advance (`rel_extend_plan`) + exclusive
pre-record reads (`get_committed_before` / `Version::at(lsn-1)`); e2e
regression `restart_relsize_zero_shadow` green 10/10 under SIGKILL.
Still open: bug B (single-record loss when the checkpoint passes an
unflushed row — reproduced once by the e2e scenario), a retry-healed
transient walredo failure in the same family, and the W10 data-repair
pass for existing zero-image landmines.

## Update 2026-06-10 — two recorded hypotheses falsified by git

Two load-bearing claims in this doc are wrong; verified by pure ancestry,
no repro needed:

1. **"The PS-side fix is in some commit between `b8aa3fac4` and `dde78d810`"
   — FALSE.** That range is exactly three commits, none a PS logic fix:
   `dde78d810` (SK lookback window), `79a39e206` (SK batched range-fetch),
   `26718c7a4` (PS *env-gated trace* + an unconditional row-drop *detector*
   that logs `CRITICAL` but changes no write behavior). There is no code fix.
   The corruption "vanished" only because the newer build's materializer
   drained the dangerous chains into stable image layers — the data state
   changed, not the code. **The bug is therefore still latent in `main`:**
   both `b8aa3fac4` and `dde78d810` are ancestors of HEAD and the suspect
   code is unchanged at HEAD. The clean >3 h runs are a property of the
   drained dataset, not evidence of a fix.

2. **"`b8aa3fac4` Arc<PageRow> is the prime suspect for the crash" —
   EXONERATED.** Crash 1 ran on `d9641d272` (committed 2026-06-07), which is
   *older* than `b8aa3fac4` (2026-06-08) and does **not** contain the
   Arc<PageRow> change (`git merge-base --is-ancestor b8aa3fac4 d9641d272` →
   false). A change introduced after a crash cannot have caused it. The Arc
   diff is also a mechanical `PageRow → Arc<PageRow>` wrap of read-only rows
   (rows are immutable; `payload: Bytes` is already Arc-shared) — no
   aliasing/mutation that could corrupt chain bytes. Arc may still own the
   *cache_ms* regression (task 27); it does not own the corruption.

**Refocus:** root cause is common to `d9641d272` *and* `b8aa3fac4` — the
build-independent delta-chain assembly handing walredo a chain that decodes
to a bad item offset (`invalid max offset number` / `failed to add new item`).
Next: audit the chain merge/sort/dedup/truncate path (`PageWriter::read_chain`
+ `lookup_chain*` + `PageStore::read_chain`/`scan_range`) for an ordering or
boundary error that survives across both builds, rather than bisecting the
Arc commit.

## Update 2026-06-11 — run #6 "repro" is a FALSE POSITIVE (env failure, not corruption)

The saved crash log
(`ps-stack.log.crash-20260611-072330-rel16414-blk4135568.gz`) is **not** the
corruption. Every `apply_wal_records` failure in it is the walredo postgres
child failing to *exec*:

```
wal-redo-postgres-stderr{pid=584906 ... pg_version=PG17}: received output
  output=.../builds/2026-06-09-dde78d810/pg_install/v17/bin/postgres:
  error while loading shared libraries: libicui18n.so.60:
  cannot open shared object file: No such file or directory
```

All 9 walredo launch attempts died at load time → `write to walredo stdin:
Broken pipe` / `read walredo stdout: early eof` → `ABORT: MATERIALIZER REDO
ERROR` → correct fail-stop, wrong reason. No walredo PANIC anywhere in the
log; the chain in the ABORT (`chain_len=6`, blk `3F1A90`=4135568) was never
actually fed to a working redo process.

Mechanism: `pageserver/src/walredo/process.rs` spawns walredo with
`env_clear()` + `LD_LIBRARY_PATH=<pg_lib_dir>` (`pg_install/v17/lib`) only.
The bundle ships `libssl/libcrypto/libicu*` in the bundle-top-level `lib/`
(reachable by PS itself via neon_local's child-env allowlist) but
`pg_install/v17/lib` had none of them. The `libssl.so.1.1`/`libcrypto.so.1.1`
symlinks in `pg_install/v17/lib` were created **2026-06-11 07:02:08** — nine
seconds before the 07:02:17 SIGABRT — i.e. mid-firefight, one missing lib at
a time, and `libicu*` was the next one in line at 07:20.

Consequences:

- Today's SIGABRT series (06:58:20, 07:02:17, 07:20:25) is this env
  crash-loop. The 04:22:34 and 05:31:43 cores predate the 07:02 symlinks, so
  walredo could not have loaded then either (no system libicu60 on the box,
  `pg_install` was switched to the new bundle's) — presumed same cause, but
  their log text is gone (PS log truncates per restart). The 05:31 core is
  fully present (32.5 M) if anyone wants certainty.
- The earlier (06-10) clean 3 h runs must have run walredo from a different
  `pg_distrib_dir` — this bundle's `pg_install/v17` could never exec postgres
  before 07:02 today.
- The trace instrumentation itself **works**: 151 K `write_trace stage="put"`
  lines for rel 16414 in the 9 s the PS was up. Once the env is fixed, a real
  repro will have full put-side coverage.

Fix on the box (matches the [bundle-runtime-libs memory], do for every
deployed bundle):

```
cd /home/ec2-user/yb-light/builds/2026-06-09-dde78d810
ln -s $PWD/lib/libicudata.so.60 $PWD/lib/libicui18n.so.60 $PWD/lib/libicuuc.so.60 \
      pg_install/v17/lib/
```

Durable fix: ship script should symlink/copy all runtime libs into
`pg_install/*/lib/`, since walredo's `env_clear()` spawn only sees
`pg_lib_dir`. Then re-run the repro recipe — run #6 has not actually tested
anything yet.

## Update 2026-06-11 (later) — ROOT CAUSE FOUND: replay-window zero-image shadowing

Crash 4 (REAL, 07:41:17, log `ps-stack.log.crash-20260611-074120-recovered.gz`)
finally preserved the full PANIC context:

```
PANIC:  neon_rm_redo: invalid lp
CONTEXT: WAL redo at 29/764FB078 for neon/HOT_UPDATE:
         old_xmax: 10375787, old_off: 35, ..., new_off: 58
key=000000067F000040000000400700000000FE  (rel 16391 blk 254)
chain_len=44  base_img_lsn=29/74ABCCA8  during legacy compaction (attach gen=0f)
```

Forensics were done directly against `pageserver.pages` in YB
(decode scripts in `/tmp/claude/task28/` on the dev box: `decode_chain.py`,
`lp35_history.py`, `window_decode.py`, `sim.py` — the last one replays
stored deltas image-to-image with PG redo semantics and diffs line-pointer
tables; generalize into `tools/` later).

### Root cause A — rel-size cache pin during crash-replay → zero-image bursts

The dominant mechanism, fully evidenced on rel 16414 (insert-only tail
growth) blk 4135568 and neighbors:

- Blk 4135568 has **6,269 images — every one an all-zero page — vs 69 real
  deltas**. All zero images sit in `29/774F8738..29/78331088`, today's
  crash-loop replay window. Neighboring tail blocks: 4135400 → 17,277 zero
  images; 4135560 → 6,645; 4135600 → 4,400; 4135650 → 2,119 (monotonically
  decreasing with block number). Low blocks (1M/2M/3M/4M) are clean (1–3
  images).
- Max zero-image LSN = `29/782E8040` for most blocks = the prior instance's
  flush horizon; bursts stop exactly there.

Mechanism (all in `pgdatadir_mapping.rs` / `walingest.rs`):

1. yb-light flushes page rows to YB continuously, but the replay start
   point (checkpoint) lags. After a crash, **WAL replay re-runs below YB's
   durable horizon** — storage already contains rows (incl. `rel_size_to_key`
   values) at LSNs the replay is about to re-visit. This anomaly cannot
   happen in upstream Neon (layer files are a consistent snapshot at
   `disk_consistent_lsn`); it is a yb-light-specific design gap.
2. During replay, `put_rel_extend` reads the size key LSN-versioned
   (`self.get(size_key)` at the record's LSN) and sees the *first pass's own
   write* — `old_size == nblocks`, never less — so the
   `if nblocks > old_size` guard makes it a permanent no-op and
   **`set_cached_rel_size` is never called**.
3. The rel-size *cache* (`rel_size_latest_cache`) therefore stays pinned at
   the replay-start size. `handle_rel_extend` / `ingest_batch` →
   `create_relation_if_required` keep getting the pinned value, treat every
   record beyond it as an extension, and `find_gaps`/gap-fill writes
   **`ZERO_PAGE` images for the entire [pinned_size, current_blk) range at
   the current record's LSN — on every single record** (zero images for one
   block land ~250 bytes of WAL apart, matching per-record cadence).
4. The zero images land *above* the first pass's real deltas, shadowing
   them: reads return an empty page (**silent data loss** — the data is
   still in YB below the zero image, just unreachable), and any block with
   non-`will_init` deltas above its last zero image PANICs walredo at the
   next materialization/read: `invalid max offset number` /
   `failed to add new item` / `invalid lp` — exactly crashes 1–4's shapes.
5. Once replay passes the prior flush horizon, `put_rel_extend` fires
   again, the cache unsticks, bursts stop — the corruption window closes
   and only the landmines remain. Materializer activity then gradually
   "drains" them → **"catch me if you can"**: why crashes cluster right
   after load/attach/restart and vanish under an instrumented rebuild.

Side effect: the millions of junk zero-image rows inflate the PageWriter
delta map — likely a contributor to the task-27 `cache_ms` O(N) walk
(4.5 M entries / 360 MB observed there).

### Root cause B (secondary, suspected) — single-record loss across crashes

Image-to-image lp simulation over full block histories found rare
single-record losses *not* explained by zero images:

- rel 16391 blk 254: a HOT_UPDATE placing a tuple at lp 35 in
  `(29/6B946C38, 29/71DC5148)` is missing from the store — yet the stored
  prune at `29/71DC5148` redirects `7 → 35` (proving the primary had it)
  and crash 4's failing record updates *from* lp 35. 258 image intervals
  checked, this is one of 2 inconsistencies; the other two known cases:
  lp 37 (epoch 25, same block) and blk 300 lp 5 (epoch 27) — image
  *contains* the missing record's effect, so the record reached the
  materializer but never YB.
- All cases sit near PS crash days (coredumps 06-06 .. 06-11). Suspected:
  checkpoint/`last_record_lsn` persisted ahead of a lost pages flush, so
  the final replay pass starts above the record's LSN and never re-puts it
  (same family as tasks 23/24 flush-ordering bugs). Not yet proven —
  needs the flush/checkpoint ordering audit.

### Fix directions

1. **Unpin the cache**: `put_rel_extend` must update
   `set_cached_rel_size` even when `nblocks <= old_size` (cache
   `max(old_size, nblocks)`), and/or gap-fill must compute `old_nblocks`
   against the *latest* durable size, never an LSN-versioned read taken
   mid-replay.
2. **Make replay idempotent for gap-fill**: never write zero images for
   blocks that already have rows at or below the current LSN (existence
   check, or suppress gap-fill entirely while `lsn <= durable_horizon`).
3. **Checkpoint ordering**: replay start must never exceed what the pages
   flush durably covers (root cause B).
4. **Data repair**: shadowed deltas still exist below the bogus zero
   images. A repair pass can delete zero images that have older real rows
   for the same key (a legit creation-time gap zero never does), restoring
   reads. Needed before trusting this dataset again.

### Update 2026-06-11 (evening) — fix A landed in tree; e2e scenario reproduces bug B in 30 s

Fix A is implemented: `put_rel_extend` now always advances the rel-size
cache via the pure helper `rel_extend_plan` (`pgdatadir_mapping.rs`),
with unit tests pinning the contract
(`rel_extend_replay_advances_cache_without_write`). New e2e scenario
`test/e2e/src/scenarios/fast/restart_relsize_zero_shadow.rs`:
insert-heavy extension for 7 s → `pageserver stop -m immediate` →
restart → assert no all-zero image newer than its block's oldest row.

First run of the scenario (fix A applied) **reproduced the production
corruption locally in ~30 s**: attach materializer died with
`PANIC: failed to add new item` (crash-1 signature) on the pkey-index
block (db=5 rel=16391 blk=3). Forensics on the preserved run DB
(`yb_storage_e2e_2641792_131610426`, run dir `run_1781190176_2641792`):

- Replay restarted from checkpoint `0/25C7DE0`.
- blk 3's btree-insert deltas arrive every ~0x70000 of WAL:
  `…2449D48, 24B9E18, 2529F20, <MISSING ~2599F20>, 260A0A8…` —
  **exactly one record lost, just below the checkpoint**. Its flush was
  lost by the immediate stop while the checkpoint had already advanced
  past it → replay never re-ingested it → chain hole → walredo PANIC.
  **This is bug B, deterministically reproduced.**
- The same lost window also dropped a size-key write: the first replay
  extension record (same lsn `0/260A0A8`) re-zero-filled once, leaving
  exactly one shadowing zero image (blk 39) — fix A removed the
  thousand-image flood; this single burst is bug B residue, as
  predicted in "Fix directions".

Suspected bug-B mechanism (to verify next): `disk_consistent_lsn` is
advanced to `max(lsn)` of the rows a flush happened to write, but
durability requires the lsn below which *everything* (rel + slru + meta
buffers) is flushed — a row at a lower lsn that missed the flush
snapshot is silently passed over by the checkpoint and lost on crash.
Fix B = compute the checkpoint as a true frontier (min over unflushed,
across all three buffers), then re-run the scenario until clean.

Note for the scenario: extend the zero-image assertion to all relnodes
(the first run's assertion was scoped to the table and missed the
index-block crash — the corruption-marker log scan caught it instead).

### Update 2026-06-11 (night) — L-1 committed reads land; SIGKILL scenario 5/5 green

The principled fix for bug A is in: ingest's "what was the relation
like *before* this record" questions now read committed state strictly
below the record's LSN (`DatadirModification::get_committed_before`;
`create_relation_if_required` exists+size via `Version::at(lsn-1)` with
explicit latest-cache seeding; `put_rel_extend` old-size read). On the
first pass this is identical to the inclusive read; during replay it
stops the record from seeing its own prior-pass effects, so replay
re-takes first-pass decisions and gap-fill re-writes the same zeros at
the same LSNs (idempotent upserts). The `rel_extend_plan` cache-advance
stays as belt-and-braces (no longer load-bearing; the no-grow branch is
still legitimately reachable from `import_datadir.rs:196`, so it must
not assert).

Scenario hardening: true SIGKILL via pidfile (`stop -m immediate` runs
a shutdown flush that closes the replay window — both bugs need the
kill-9 path), tenant-wide zero-image assertions, per-iteration PS log
scan (the log accumulates across iterations; a whole-file scan
re-reports old markers).

Verdict matrix:

| Build | Kill | Result |
|---|---|---|
| HEAD | immediate stop | pass (window closed by shutdown flush) |
| HEAD | SIGKILL | FAIL — panic-loop `unified_pages.rs:441` + `pgdatadir_mapping.rs:467` |
| fix A only | immediate stop | FAIL once — bug-B missing record (race) |
| fix A + L-1 | SIGKILL ×5 + ×5 | **10/10 of 11 clean**; one iteration hit a *transient* `failed to add new item` walredo PANIC that healed on retry (no ABORT, data intact) |

The transient is the remaining lead: a chain assembled while rows were
only partially visible/durable — same family as the
`lookup_chain_durable_if_complete saw an unflushed row` invariant and
bug B. Next: the flush/checkpoint/visibility ordering audit in
`page_writer.rs`/`unified_pages.rs`, using this scenario with `-n` as
the repro harness.

### Minor tooling notes

- `tools/yb-dump-pages` displays rows in non-numeric LSN order: the
  hex-rendered text is aliased `AS lsn`, so `ORDER BY lsn` binds to the
  alias and sorts lexicographically. Confused the first half hour of
  forensics. Filed as [task 32](32_yb-dump-pages-lsn-sort.md).
- Delta payload format (for future tooling): 13-byte BE-bincode header
  (`NeonWalRecord::Postgres` variant u32 + `will_init` u8 + len u64) +
  raw `XLogRecord` bytes. Image payload = raw 8192-byte page.

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
**"pageserver: Arc<PageRow> through chain reads + flush"** ← still suspected
for the post-crash cache_ms regression (task 27), but **exonerated for the
crash** — crash 1 predates this commit on `d9641d272` (see Update 2026-06-10).

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

- ~~Is the PS-side fix on `dde78d810` itself, or earlier on the branch?~~
  ANSWERED (see Update 2026-06-10): there is **no** PS-side fix in
  `b8aa3fac4..dde78d810`. The corruption is latent in `main`; the clean runs
  reflect a drained dataset, not a code fix.
- Are the four 2026-06-06 SIGABRTs on `582e7e4d8` / `93c8c29c6` the same root
  cause? File contents gone; only `coredumpctl info` metadata remains.
- Is `cache_ms` blow-up from the Arc<PageRow> change a bug or a tuning issue?
  78 % → 58 % regression. Worth bisecting between `582e7e4d8` and `b8aa3fac4`.
