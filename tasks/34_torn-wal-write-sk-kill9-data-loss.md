---
status: in_progress
severity: highest
area: yb-pageserver / safekeeper
opened: 2026-06-11
---

# Torn WAL write: crash-recovery loses acked committed data (heap rows, then catalog/index pages)

> **Scope note (2026-06-12):** part 1 — the SK-only-restart heap-row loss
> + `durable_walk_unflushed_row` — is **fixed and verified** (see "Fix
> (part 1)"). Part 2 — durable **torn catalog / torn index** corruption
> surfaced by repeated crash-recovery — is **open** (see "Part 2"). The
> two share a theme (committed page writes not surviving recovery) but
> have different mechanisms; keeping them in one task because they're the
> same reproducer family.

# Part 1 — SK kill-9 recovery loses acked committed rows + trips `durable_walk_unflushed_row`

Found by the new e2e reproducer
`test/e2e/src/scenarios/fast/torn_wal_write.rs` (scenario A): SIGKILL the
safekeeper mid-flush while a `INSERT id=i; COMMIT;` loop runs (each commit
synchronously durable), restart the SK, let the surviving compute's
walproposer reconnect, then read durable state from a fresh basebackup.

## Symptom

Two distinct corruption signals, both from the crash-recovery path:

1. **Loss of acked committed rows.** The counter table comes back
   non-contiguous: across runs the read-back was `count=677 max=2117`,
   `692/2132`, `802/2260`, and — worst seen — `count=4 max=1462`, with one
   clean `1452/1452` run (timing-dependent). When it loses, the survivors
   are a contiguous *suffix* (`min(id)=1459, max=2260, count=802`): the
   *early* committed rows are gone, later ones intact. Each lost row's
   `COMMIT` had returned to the client (synchronous), so this is loss of
   acknowledged data, not an expected tail truncation.

   **The loss is specific to crashing _only_ the safekeeper with a live
   compute** (matrix scenario A). The full
   `test/e2e/.../torn_wal_write.rs` matrix — singles {SK, compute, PS},
   pairs, and all-three — shows only the SK-alone mode losing data; every
   mode that *also* restarts the compute or the PS (D–G) recovers cleanly.
   That points at the surviving compute's walproposer resuming above the
   SK's trimmed start (it never re-sends WAL it believes already handed
   off), combined with the PS never having durably materialised it.

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

`test/e2e/run.sh torn_wal_write` (fast). Scenario A (SK alone) is the
repro and is mode 1 in the matrix, so the run stops there on failure
(the matrix now bails at the first losing mode). The contiguity
assertion (`count == max(id)`) catches it directly — the *amount* lost
varies run to run (debug repro `run_1781239715` lost everything below
the boundary: `count=1, max=1459`), but the failure itself is reliable.
The other matrix modes (B compute, C PS, D SK+PS, E SK+compute, F
PS+compute, G all three) come back contiguous.

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

## Root cause (CONFIRMED 2026-06-12)

The original hypothesis (PS trims `wals` ahead of durability) is **wrong /
inverted**. The actual mechanism, captured from a live debug repro
(`run_1781239715`, tenant `0c25b270…`, timeline `9d008d6a…`):

**The walreceiver re-ingests over still-unflushed cache slots on
reconnect, panicking `UnifiedPages::insert_arc_entry`.**

Sequence for scenario A:

1. The 3 s warmup is shorter than the flush/advance cadence, so the PS
   **never advances `consistent_lsn` past `initdb_lsn` (`0/15404B0` =
   `0/1540438`)**. Every workload row (ids 1..N) sits in the in-memory
   `UnifiedPages` cache as an **unflushed** entry; nothing is durable in
   `pageserver.pages` yet. (PS log: `consistent_lsn 0/15404B0 starting
   replication from 0/15404B0` throughout the window.)
2. SK is SIGKILLed and restarted. The committed WAL is still in `wals`
   (`min chunk_offset = 5506224 = 0/15404B0` is just the **timeline
   floor / initdb_lsn**, not evidence of over-trimming).
3. The PS does **not** restart — only its walreceiver connection drops.
   It reconnects and re-streams from its durable watermark
   (`get_consistent_lsn()` = `0/15404B0`) and re-ingests the record at
   `0/15406B8`.
4. That `(key, lsn)` is **already in the cache as an unflushed slot**
   from the pre-crash pass. `insert_arc_entry` (`unified_pages.rs:593`)
   asserts the overwritten slot was durable — it isn't — and **panics
   the walreceiver worker**:
   `insert_entry overwriting an unflushed slot at lsn=0/15406B8 …
   the row in the unflushed slot would be lost if it isn't also in YB`.
5. The panic kills the connection → reconnect from `0/15404B0` →
   same panic → **tight loop** (seen ~every 100–250 ms). The
   unflushed early rows are never flushed; a later re-attach rebuilds
   the timeline with a fresh empty cache seeded from the still-initial
   `consistent_lsn`, discarding them. **Result: ids 1..1458 are gone
   for good** (`torn_a` reads `count=1, max=1459` permanently, fully
   quiesced at gen 6 — not a delayed read).

The faulty assumption is in the `insert_arc_entry` comment itself:
*"walreceiver's LSNs are strictly monotonic within a PS lifetime, so it
never overlaps a same-LSN slot. **(On restart the cache is empty.)**"*
True for a **PS** restart; false when only the **SK** restarts and the
walreceiver reconnects against a non-empty cache.

Why only mode A: modes that also restart the PS (C/D/F/G) get a
genuinely empty cache on PS restart; mode B (compute only) doesn't force
the walreceiver to replay from `consistent_lsn` over live unflushed
rows. Only "SK alone, PS+compute live" hits the overlap.

Build-mode note: debug builds **panic** (the `debug_assert!`); release
builds compile the assert out and **silently overwrite** the unflushed
slot — same loss, no loop, no log. The `durable_walk_unflushed_row`
counter is a downstream symptom of the same non-durable-cache state, not
the primary signal.

### Fix direction (not yet implemented — needs design sign-off)

The re-streamed row is byte-identical to the buffered unflushed row, so
the overwrite is semantically a no-op. Options:
- Make `push()` idempotent against an existing **unflushed** same-`(key,
  lsn)` slot (keep the existing entry, don't panic) — narrowest fix.
- On walreceiver reconnect, reconcile/replay-skip records already
  present in the unflushed buffer.
- Have the PS resume from `max(consistent_lsn, highest-unflushed-lsn)`
  so it doesn't re-stream rows it already holds (careful: must not skip
  a genuine gap).

First option is the smallest and matches the invariant's intent; verify
against the full torn-write matrix + `restart_*` scenarios before
claiming fixed.

## Fix (part 1, landed 2026-06-12)

Implemented the narrowest option in `pageserver/src/unified_pages.rs`:
`push()` (walreceiver) is now **idempotent** against an existing
same-`(key, lsn)` slot. `insert_arc_entry` returns whether it added a
brand-new unflushed entry; on a duplicate it keeps the existing entry
untouched (whether unflushed-and-already-in-FIFO or already durable) and
`push()` skips the FIFO append, so there's no double-count, no
unflushed→durable revert, and no overwrite that could drop a
not-yet-durable row. `push_durable()` (materialiser) keeps its overwrite
semantics and its `debug_assert!` tripwire (now worded "push_durable
overwriting an unflushed slot").

Tests (`unified_pages` unit tests): replaced
`push_over_unflushed_panics_in_debug` with
`push_same_key_lsn_is_idempotent` (re-push while unflushed and re-push
after durable are both no-ops; FIFO/pin/bytes unchanged); updated
`push_durable_over_unflushed_panics_in_debug`'s expected message.

Verified:
- `cargo test -p pageserver --lib unified_pages` — 25 pass, incl. the new
  `push_same_key_lsn_is_idempotent` and
  `push_same_key_lsn_divergent_payload_panics_in_debug`.
- `test/e2e/run.sh torn_wal_write` — all 7 matrix modes PASS (A
  `count=1476 max=1476`, contiguous); Final verdict
  `durable_walk_unflushed_row` correctness counter at **0** (previously
  fired).
- Full `test/e2e/run.sh fast` — every scenario PASS (incl. `restart_*`),
  counter at 0. No restart-path regression.

A defensive `debug_assert!` in the skip path checks the replayed row is
byte-identical to the kept slot (deterministic-WAL assumption); a
mismatch panics in debug rather than silently keeping the wrong page.

# Part 2 — durable torn catalog / torn index under repeated crash-recovery (OPEN)

With part 1 fixed, mode A stopped failing fast, which let
`test/e2e/run.sh -n N torn_wal_write` stack multiple crash-recovery
cycles on one shared cluster (it reuses `torn_a..torn_g` across `-n`
iterations). That surfaced a *different*, durable corruption — not user
heap-row loss (the correctness counter stays 0), but **internally torn
system-catalog and index state**:

Observed in a captured `-n 5` run (`run_1781242577`, since torn down):

- **Torn catalog (orphan rowtype).** After several iterations,
  `torn_a` exists in `pg_type` (composite rowtype 147458 + array type
  147457), `pg_depend` (4 rows), and `pg_constraint`, but **not** in
  `pg_class` or `pg_attribute`. The next iteration's
  `CREATE TABLE torn_a` then fails `type "torn_a" already exists`.
  - **It is durable**, not transient replay state: recycling the
    compute (fresh basebackup at the PS's `consistent_lsn`) and
    re-querying still shows `pg_class=0, pg_type=1, pg_attribute=0,
    pg_depend=4`. So `pageserver.pages` itself holds a catalog where
    different catalog relations disagree.
  - **Cross-table casualty.** oid trace: the orphan `torn_a` (type
    147458) was created by **iter-4 mode A**, which *passed* its verify
    (`count=1417`). It was torn before iter-5 — i.e. during **iter-4
    mode B**, a *compute* crash + recycle that operates on `torn_b`,
    not `torn_a`. So an unrelated table's crash recovery corrupted
    `torn_a`'s catalog.

- **Torn index/heap.** iter-3 mode B failed `count=1322 but
  max(id)=1317`: the heap seqscan sees 5 rows the PK-index scan
  doesn't — index missing its tail entries while the heap has them, at
  the same `consistent_lsn`.

- A `fast`-suite run also saw a one-off `index "torn_a_pkey" contains
  unexpected zero page at block 5` in the post-recovery read.

Common theme: **pages that should all be durable at `consistent_lsn`
are not — some catalog/index pages are durable at an older version than
their sibling pages from the same committed transaction.** This is the
same "gap-free `consistent_lsn` frontier" invariant that
[task 28](28_walredo-corruption-catch-me-if-you-can.md) bug-B was about, now seen on
catalog + index relations rather than user heap.

## Part 2 — not yet root-caused

The part-1 `push()` fix does **not** address this; mode A passed 4/4
times it ran in the `-n 5` repro. The mechanism is unconfirmed and the
`-n` logs are too entangled (stacked crashes across 7 modes × N
iterations) to trust. Candidate directions to investigate with a
*single-crash* repro:

- `consistent_lsn`/`ingest_frontier` advancing past a record whose
  page set wasn't fully captured by the flush snapshot (task-28 bug-B
  family) — esp. multi-page catalog records (`CREATE TABLE` touches
  `pg_class` + `pg_attribute` + `pg_type` + `pg_depend` + `pg_constraint`
  in one record group).
- Compute recycle (`verify()` / mode B) basebackuping at
  `consistent_lsn` and the fresh primary diverging the WAL above it,
  dropping committed-but-not-yet-durable records.
- The materialiser overwriting a catalog/index page with a stale image.

### Repro hardening (done) + sharpened repro

The harness's `final_verdict` gate (`cluster.rs:461`) only checks the
`pageserver_yb_correctness_errors_total` *metric* — it is **blind to
catalog/index tears** (they don't touch that counter). That's why the
tear slipped past: a `-n` run prints `cumulative: N passed, M failed`
(scenario assertions) *and* `PASS — all workloads clean, counter at 0`
(metric gate) side by side — the second does **not** mean "no
corruption". So I added an explicit catalog-consistency assertion to
`torn_wal_write::verify()` (`assert_catalog_consistent`): orphan
composite rowtype (`pg_type typtype='c'` with no live `pg_class` at
`typrelid`) and the reverse (relation whose `reltype` is missing from
`pg_type`).

With that check, the repro is now **minimal and deterministic**:
`test/e2e/run.sh -n 2 torn_wal_write` →
- iteration 1: all 7 modes pass (catalog stays consistent).
- iteration 2 mode A (re-`CREATE TABLE torn_a`): passes.
- **iteration 2 mode B (compute SIGKILL + recycle): tears `torn_a`'s
  catalog** — `pg_class`/`pg_attribute` lose `torn_a` while
  `pg_type`/`pg_depend`/`pg_constraint` keep it. Mode B operates on
  `torn_b`, so this is a **cross-table** casualty of a *compute*-crash
  recovery, reproducible on the 2nd iteration (no deep stacking).

### Root cause (page-level, 2026-06-12)

Forensics on the freshly-torn `-n 2` cluster (`run_1781243940`,
tenant `f7c5fb9b…`, db oid 5, `pg_class` = relnode 1259):

- `pg_class` block 0 (the actively-appended catalog block) is durable
  only up to **`0/01c42148`**, while `timeline_state.consistent_lsn` is
  **`0/01d14ce8`** — the block lags the frontier.
- That durable block 0 contains `torn_b` (created *after* `torn_a`, in
  mode B) but **not** `torn_a` (created earlier, mode A). So `torn_a`'s
  `pg_class` block-0 write was **acked and `consistent_lsn` advanced
  past it, yet never durably materialized into block 0**. `pg_type`'s
  block happened to be durable with `torn_a`, hence the orphan rowtype.
- PS log: the timeline is **re-attached repeatedly** (gen 1→6; each
  compute recycle triggers a storage_controller re-attach). Every
  re-attach rebuilds the in-memory cache and **re-streams only from the
  durable `consistent_lsn`** (e.g. gen 6: `consistent_lsn=0/1AB1FB8 …
  starting replication from 0/1AB1FB8`). Records below the frontier are
  trusted as durable and never re-applied — so a write that was lost
  before it was flushed never comes back, and the stale durable page
  becomes permanent truth.

**Conclusion:** this is the **same class as part 1 / [task 28](28_walredo-corruption-catch-me-if-you-can.md)
bug-B** — a committed write lost while the `consistent_lsn` frontier
over-advances past it — but on a **different trigger path**:
compute-recycle → PS re-attach (not SK-restart → walreceiver reconnect).
The part-1 `push()` idempotency fix closed the re-ingest path; it does
**not** close this one. The defect is upstream, in whatever lets
`consistent_lsn` / `ingest_frontier` advance past a record whose page
write isn't yet durable in `pageserver.pages` (flush snapshot vs.
frontier in `page_writer.rs::flush`), exposed by the re-attach's
"re-stream from `consistent_lsn` only" resume.

### Instrumentation result (2026-06-12): flush-frontier ruled OUT

Added a tripwire in `page_writer.rs::flush` (counter
`consistent_lsn_over_advance` + `min_unflushed_lsn()` on `UnifiedPages`):
after `mark_flushed`, assert no **rel** row remains unflushed at
`lsn ≤ ingest_frontier` (the value `consistent_lsn` advances to). It
fired **0 times** across many `-n` crash-recovery cycles that produced
*every* Part-2 symptom (orphan catalog, `count>max` index skew, index
`unexpected zero page`, compute basebackup/walreceiver deadlock). So the
committed write is **not** lost at the rel-page flush frontier — that
hypothesis is wrong.

Remaining suspects (narrowed):
- **Meta/SLRU path** (`pageserver.meta` blobs, CLOG, etc.), which the
  rel-FIFO tripwire does *not* cover and which the flush comment already
  flags as fragile (count-based drain under overlapping flushes). Now
  also instrumented (same `consistent_lsn_over_advance` tripwire, meta
  variant). NB: the *observed* `pg_class` orphan is heap content (a rel
  page), so the meta probe is to rule the meta path in/out, not a
  confirmed cause.
- **Materialiser** folding an incomplete catalog-page chain into a stale
  image, then `trim_key_up_to` dropping the real deltas.
- **Basebackup/recycle divergence**: the fresh primary basebackups at
  `consistent_lsn` and re-streams above it. (If both flush tripwires stay
  silent through a tear, the loss is *post-flush* — here.)

Part-2 symptoms are **non-deterministic** (different mode/symptom each
run), so this is a multi-pronged investigation, not a single fix.

**Update 2 — over-advance-at-flush DISPROVEN; loss is POST-FLUSH.** With
both tripwires (rel FIFO + meta/SLRU) active, `-n` runs reproduced *every*
symptom — including **heap holes** (`count=27 < max=1359` mode A,
`count=482 < max=1454` mode E; the Part-1 lost-rows symptom recurring
under stacking) and catalog tears — while the tripwire stayed at **0
fires**. So at flush time the buffer *is* fully durable ≤ `consistent_lsn`;
the committed page is lost **after** a clean flush. That moves the prime
suspect to the **image materialiser**: it folds a key's chain into an
image (`read_chain_durable` → walredo) and then `trim_key_up_to` deletes
the folded deltas. If it ever folds an incomplete/wrong chain and trims,
the durable page is permanently corrupted — and it runs on every
re-attach (gen 1→6 under `-n`), matching the non-determinism and the
heap+catalog+index spread. Next probe: assert the materialiser's image
equals a full replay of the chain it trims (or never trims deltas it
didn't fold). NB: this also means the Part-1 `push()` fix is necessary
(it removed the re-ingest panic, and single/fresh runs pass) but the
heap-loss path is **not fully closed under stacked re-attach** — same
post-flush materialiser suspect.

### Deterministic repro + defensive fix (2026-06-12)

`read_chain`'s SQL floors the chain at the latest `is_image=TRUE` row,
else `lsn>=0` (full history); the Rust side trims at the first
`will_init`. So an **anchorless** result (no `will_init` anywhere ≤
`target_lsn`) means the page's init record is **genuinely missing from
YB** — a lost anchor. `read_chain_durable` returned that anchorless chain
to the materialiser, which `parse_chain`→`base_img=None`→walredo folds
onto a **zero page**, then `install`+`trim_key_up_to` makes it durable.
(The getpage `read_chain` path zero-bases the same way → that's the
compute-side "unexpected zero page" read error. One mechanism, both
paths.)

Deterministic repro (no e2e, no non-determinism):
`page_writer::tests::read_chain_durable_refuses_anchorless_chain` — put
deltas with no `will_init`, flush, call `read_chain_durable`. Confirmed
it **fails without the fix** (returns the 2-delta anchorless chain) and
**passes with it**; all 49 `page_writer` tests green.

Fix — loud-fail corruption **guards** (both paths that fold a chain):
- **Materialiser** (`read_chain_durable`): errors on an anchorless chain
  (counter `materialize_unanchored_chain`). `redo_one` **logs it and
  skips the page** (`Ok(None)`) — it does NOT hit the batch loop's
  `fatal_abort` arm (reserved for walredo failures). The deltas stay in
  place; a later trigger retries. So the materialiser can't amplify a
  lost anchor into a durable zero-based image.
- **getpage read path** (`chain_to_value_reconstruct_state`): errors on
  a plain-delta-anchored chain (counter `read_unanchored_chain`) instead
  of zero-basing. Previously the read path **silently** walredo'd
  anchorless deltas onto a zero page → silent missing rows for
  heap/catalog, and only btree's structural check surfaced it as
  "unexpected zero page". Now every such read fails loudly at the PS.

**Scope:** these are *corruption guards* — they turn a silent
lost-anchor into a loud error at the source, and stop the materialiser
making it durable. They do **not** fix the upstream **anchor loss**
itself (still unpinned — the post-flush durable loss). After them, the
`-n 2` repro's mode E surfaces as a PS getpage / `could not open
relation` error rather than a silent zero page; all non-torn `fast`
scenarios stay green (no read-path regression).

### Root-cause hypothesis (2026-06-12): generation-fence gap on the page-write path

Tracing *why* a durable page write disappears post-flush:

1. **Nothing prunes durable rows per-key.** The only DELETEs against
   `pages` are whole-timeline (`delete_all_pages`); the materialiser's
   `trim_key_up_to` is **cache-only**. So a row isn't *deleted* from YB.
2. **`read_chain` floors at the latest `is_image=TRUE` ≤ target**
   (`page_store.rs` SQL). So a **stale/wrong image hides the correct
   deltas beneath it** — no deletion needed; the truth is still in YB but
   shadowed. Matches the observed "block has an anchor image yet is
   missing a committed tuple", and a stale image at the wrong content =
   the lost-anchor / zero-page cases.
3. **`put_pages` is NOT generation-fenced.** It's an unconditional
   `INSERT INTO pages …` (`page_store.rs:365`). Only
   `advance_consistent_lsn` carries the `WHERE current_generation = $gen`
   fence. So page/**image** rows from a *superseded* generation can land.
4. **The materialiser drains in-flight image INSERTs on shutdown**
   (`image_materializer.rs:271` — `while let Some(h)=inflight.pop_front(){
   h.await }`), and shutdown fires at re-attach. So an old generation's
   folded images get committed via the un-fenced `put_pages` around the
   re-attach boundary.

**Hypothesis (RETRACTED):** "un-fenced `put_pages` lets a stale generation
write a masking image." Wrong — a materialiser image is
`walredo(chain ≤ lsn)`, and `(key, lsn)` content is **deterministic**, so
a stale generation writes a **byte-identical** image (and the write is an
idempotent upsert). An unfenced materialiser/flush write is therefore
**safe**; generation is not the vector. (The gen-fence asymmetry on
`put_pages` is real but benign for this reason.)

**Refined root direction.** Since (a) durable rows aren't deleted, (b)
all writes are idempotent/deterministic, the only way to durably corrupt
is a **hole**: a committed delta is *missing* from the durable chain, so
a later materialiser fold bakes a page that's missing it (and the image
then floors reads above the gap). A missing delta ≤ `consistent_lsn` is a
**flush over-advance** — `consistent_lsn`/`flush_horizon` advanced past a
record whose page write isn't durable. The rel-FIFO tripwire stayed
silent, but it can only see rows that *were buffered as unflushed*; it
**cannot** see a record that was noted (advancing `ingest_frontier`) yet
whose page write was never put into the buffer at all (or went missing
before the snapshot). That task-28-bug-B-class gap — `ingest_frontier`
covering a record whose pages aren't in the snapshot — is the next thing
to scrutinise in `PageWriter::flush` (frontier capture vs snapshot
contents) and `Timeline::finish_write`/`note_record_lsn` ordering.

### Deadlock root-caused + fixed (2026-06-12): over-strict idempotent-push assert

Observing the durable state (per the "add logs / query YB" approach) on a
deadlock run (`compute never served reads`) showed the real cause: the
basebackup was stuck in `wait_lsn` for the tail LSN while the walreceiver
**panic-looped** at `unified_pages.rs` — my own Part-1 idempotent-`push`
`debug_assert!` (byte-identity of a re-pushed row). The panic:

```
re-push at lsn=0/18E7B10 differs … existing=PageRow{is_image:true,will_init:true,…}
                                    new    =PageRow{is_image:false,will_init:false,…}
```

The materialiser installs an **image** at a record's max LSN, which
**collides** with that record's own **delta** LSN. When the walreceiver
re-streams the record (SK-only restart replay), `push` of the raw delta
lands on the existing image at the same LSN — legitimately different
content. The assert (which assumed byte-identity) fired → walreceiver
worker panicked → reconnect loop → ingest stalled → basebackup `wait_lsn`
deadlock → opaque `compute never served reads`. **Debug-only**: in
release the assert is compiled out and the skip already keeps the image
(correct). Fix: only assert byte-identity when the existing slot is a
**delta**; when it's an image, keep it (it supersedes the same-LSN delta,
the same tie-break `read_chain` uses). New unit test
`push_delta_over_materialised_image_keeps_image`. Verified: with the fix,
iteration 1 of `-n 4 torn_wal_write` passes **all 7 modes incl. mode E**
(previously deadlocked); the deadlock symptom is gone.

### Making caught panics easier to spot (observability)

A caught walreceiver-worker panic was near-invisible (silent reconnect
loop → downstream timeout, panic buried behind an 8 KB payload dump).
Added:
- `walreceiver.rs`: a caught worker panic bumps
  `correctness_errors{walreceiver_worker_panic}` so the suite's
  `final_verdict` metric gate (runs for every scenario) fails loudly.
- `torn_wal_write.rs`: `scan_for_panic` / `first_panic_line` greps the
  PS+SK logs for panics and names the panic in the failure — incl.
  enriching the `compute never served reads` timeout with the panic line.
- `unified_pages.rs`: the assert message now prints a compact
  `(is_image/will_init/len)` summary instead of dumping full page
  payloads.

### Catalog-orphan: durable per-key write loss (bisected 2026-06-12)

Caught a corrupted cluster (`-n 2 torn_wal_write` corrupts intermittently,
~1 in 2 post-deadlock-fix; `run … db yb_storage_e2e_3505065`) and probed
the durable state directly (the "query YB" approach):

- `torn_a` rel-oid = 73728. Its tuple is **physically absent from every
  `pg_class` block** (checked all blocks via
  `heap_page_items` + matching `t_data`'s leading oid bytes); `torn_b`
  (oid 24576) is present. So the row is **not present-but-invisible** —
  it's a **durable page-write loss**, NOT a clog/visibility issue (rules
  out the [task 24](24_early-clog-slru-deltas-lost.md) family).
- It is **per-key within one txn**: the same `CREATE TABLE torn_a`
  durably wrote **`pg_type`** (rowtype 73730 + array `_torn_a` 73729 both
  present) but its write to **`pg_class`** block 0 (relnode 1259) is
  gone. So one rel-page delta from a committed transaction survived and a
  sibling rel-page delta from the *same* transaction did not.
- YB `pageserver.pages` for `(relnode=1259, db=5, fork=0, blk=0)`: a
  materialiser **image at `0/01b7bac0`** (+ a same-LSN delta), older
  deltas below it, and a recent delta batch at `0/01c5xxxx` — with a
  **gap** where `torn_a`'s `pg_class` create delta should be. No delta
  carries it; walredo of the durable chain reproduces the
  compute's `torn_a`-less page exactly.

So: `consistent_lsn` advanced past `torn_a`'s `pg_class` block-0 write,
but that write was never durably in `pages`. The unflushed-row tripwire
can't see it (the row was never an unflushed entry — it was never
buffered, or dropped before the snapshot). All static write-path checks
(put always pushes; `commit` puts before `finish_write`/`note`; idempotent
skip keeps correct content; `put_pages` never logged a short affected
count; PK separates image/delta) pass — so the loss is a **race in the
put→note→flush(snapshot/frontier)→materialise pipeline** for one key, not
a single obviously-wrong line.

**Flush snapshot ruled OUT (2026-06-12).** Added a `snapshot_fifo_orphan`
probe in `snapshot_unflushed`: every FIFO pair must resolve to a live,
non-durable slot (under `flush_serialize` + the unflushed pin, anything
else means a pending write is dropped from the flush). It fired **0
times** in a run that *did* corrupt. Combined with `put_pages` never
logging a short affected-count, this means **anything `put` to the
PageWriter is persisted** — so `torn_a`'s `pg_class` delta was **never
`put`**. The loss is therefore **upstream of the PageWriter** (ingest /
WAL / recovery), not the flush snapshot/frontier.

**Next (upstream of the PageWriter):** the candidates are now
- **walingest / `DatadirModification::commit`** not putting the page for
  `torn_a`'s catalog insert (or putting it then it never reaching
  `put_batch`), or
- a **full-page-image overwrite** of `pg_class` blk 0 during mode-B
  recovery that captures a `torn_a`-less version, or
- **recycle / WAL divergence**: the mode-B fresh primary basebackups at
  `consistent_lsn` and re-streams, and the original committed
  `torn_a` WAL is overwritten/never re-applied.
Trace the `put` stage for `pg_class` blk 0 (`YB_PS_TRACE_REL_BLK=
1663/5/1259/0/0`, plumb the env to the PS) to see whether `torn_a`'s
create is ever `put` — present ⇒ lost downstream (contradiction, re-check);
absent ⇒ ingest/WAL/recovery. Mode B (compute crash + recycle) is the
trigger, so the recycle/divergence path is the strongest lead.

### Status: part 1 fixed; deadlock fixed (over-strict assert); anchorless read/materialiser guards + panic observability added; **catalog-orphan = confirmed durable per-key write loss (not visibility), still open** — a put→flush-snapshot/frontier race for one rel-page key

A correct fix is in the `consistent_lsn`/flush-frontier machinery and
must be designed + verified (not guessed) — advancing `consistent_lsn`
only once *every* page of *every* record ≤ frontier is confirmed durable
in `pageserver.pages`. The catalog-consistency gate
(`assert_catalog_consistent`) and the `-n 2` repro above make it
deterministic to validate a fix against. Also fold the catalog/index
checks into the permanent corruption gate (`final_verdict` only watches
the metric counter and is blind to these tears).

### TRACE PROOF (2026-06-12) — supersedes the "flush-frontier race" guess above

Plumbed `YB_PS_TRACE_REL_BLK` to the PS (`background_process.rs` env
passthrough) and hardened the harness so setup is reliable (`clean.sh`
now kills leaked computes from *any* `.e2e-tests` run dir + waits for
`COMPUTE_PORT` to free — a leaked compute/SC from a prior run was
tripping `endpoint create` with "duplicate primary endpoint", the
session-long setup flakiness). Captured a `pg_class`-direction orphan run
with the block-0 write-trace on (`1663/5/1259/0/0`; all `torn_*` rels
live in `pg_class` block 0, ctid `(0,x)`):

- **92 unique block-0 `put` LSNs → all 92 reached `durable` +
  `put_pages_ok`. "put but not durable" is empty.** Every block-0 write
  that was `put` got persisted; nothing dropped in
  flush/snapshot/`put_pages`.
- Yet `torn_a` is absent ⇒ **`torn_a`'s `pg_class` create was never
  `put`.** The page-write / flush / snapshot / materialiser subsystem is
  **fully exonerated** for this symptom (the earlier "flush over-advance
  / FIFO race" hypotheses are wrong).

> **RETRACTION (2026-06-12): this trace was blind to `torn_a` — the
> "exoneration" does NOT hold.** The trace pinned block 0 on the
> assumption that `torn_*` rows live in `pg_class` block 0. That is false:
> a freshly-`initdb`'d `pg_class` already holds ~400 system-catalog rows
> (~50 rows per 8 KB page → ~8+ blocks before any user DDL), and `-n N`
> runs only grow it. `torn_a`'s `pg_class` row almost certainly lands well
> past block 0, so "92/92 block-0 puts durable, `torn_a` absent" is
> trivially true and says nothing about whether `torn_a`'s create was put.
> It neither proves "never put" NOR exonerates the page-write/flush path.
> The over-advance hypothesis (consistent_lsn advancing past a record
> whose `pages` write never became durable — including via the
> `set_lsn(next_record_lsn)` advance on filtered/no-put records at
> `walreceiver_connection.rs:490`) is therefore still live. **Re-run the
> trace targeting the actual block `torn_a`'s `pg_class` row lands on (or
> trace the whole rel `1663/5/1259`, all blocks), not block 0.**

**Root: a WAL/recovery-stream divergence.** The committed
`CREATE TABLE torn_a` record was never applied by the pageserver.
Trigger is mode-B (compute crash + recycle); the loss alternates
per-record between the catalog rels (`pg_class` *or* `pg_type`, whichever
record's apply is missed). After the recycle the fresh primary
re-streams from a point that omits the original committed record
(fresh-primary epoch divergence / SK `wals` truncation-overwrite), so the
PS never (re-)applies it.

**Next:** trace the PS walreceiver `START_REPLICATION` range + SK `wals`
content across a mode-B recycle to find where the committed create LSN
drops out of the stream. Safekeeper / walproposer / recovery-epoch
investigation — a different subsystem from everything touched so far.

### ROOT CAUSE CAUGHT (2026-06-12): consistent_lsn over-advances past an unflushed rel row

The over-advance tripwire **fired** in a `-n 4` run (it's intermittent —
the earlier traced run just didn't hit it):

```
ERROR consistent_lsn over-advance: unflushed rel row at lsn=0/19094B8
  ≤ ingest_frontier=0/1988560 after mark_flushed — a committed page write
  is not durable but the frontier passed it
```

So `PageWriter::flush` advances `consistent_lsn` to `ingest_frontier`
while a rel row at a **lower** LSN is still unflushed (not durable). The
race is in the attach-replay re-stream: a flush captures a high
`ingest_frontier` while a lower-LSN row is buffered-but-not-in-this
-snapshot. On the next re-attach the PS resumes from the (over-advanced)
`consistent_lsn`, never re-applies the row at `0/19094B8`, and the
committed catalog/heap write is gone. This is THE catalog-orphan /
heap-hole root (the block-0 trace just happened to be a run that didn't
over-advance; the tripwire is the reliable catch).

**Fix:** never advance `consistent_lsn` to or past the smallest unflushed
row's LSN. Cap the advance at `min(ingest_frontier, <largest noted
record < min_unflushed_lsn>)`; if none, skip the advance this flush (the
unflushed row flushes in a later flush, then `consistent_lsn` catches
up). Verify: the over-advance tripwire stops firing and the
catalog/heap loss stops across many `-n` iterations.

(Other symptoms seen in the same chaotic attach — a transient
IDENTIFY_SYSTEM "not found" when the walreceiver beats the SK's timeline
load, and materialiser ERRORs — are startup races, separate from this
data-loss root.)

### CORRECTION (2026-06-12): the cap fix FAILED verification — reverted

The "cap `consistent_lsn` at `min_unflushed`" fix above was implemented
and verified, and it **does not stop the orphan**. Reverted via
`git checkout pageserver/src/page_writer.rs` (only the debug-gated
over-advance tripwire remains; the caps are gone).

Two attempts, both failed in `-n 8` repro runs:

1. Cap `consistent_lsn` at `min_unflushed` in the advance block → orphan
   persisted (loss signal still fired), over-advance tripwire still fired.
2. Also cap `flush_horizon` → orphan STILL persisted.

**Why it cannot work, reconciled with the trace proof:** the over-advance
tripwire detects a *put-but-unflushed* row ≤ frontier. The block-0 trace
proved the orphan record (`torn_a`'s `pg_class` create) was **never
`put`**. These are two DIFFERENT conditions:

- **Over-advance (#2):** a row that WAS `put` is still in the unflushed
  FIFO when `consistent_lsn` passes its LSN. `min_unflushed_lsn()` knows
  about it, so the cap can guard it. This is a real latent resume-loss
  bug — worth keeping the tripwire — but it is NOT the orphan.
- **Never-put (#1):** the committed `CREATE TABLE` record never reached
  `put` at all. It is not in the FIFO, so `min_unflushed_lsn()` cannot
  see it and **no cap on the advance can recover it.** Capping the
  over-advance is structurally incapable of fixing a never-put loss.

So #1 (the orphan) is upstream of ingest: a WAL/recovery-stream
divergence where the committed create record is never (re-)applied by the
PS after the mode-B compute-crash+recycle. The SK provably retains the
committed WAL — the hard `truncate_wal` assert in `handle_elected`
(`safekeeper.rs:913`, `start_streaming_at >= flush_lsn`) never fires, so
the SK never drops committed data. The miss is in how the PS walreceiver
resumes/applies the stream across the recycle, not in page-write/flush.

**Standing rule honored:** no unverified corruption fix is committed. The
caps were reverted rather than left in. Next step is the SK/walproposer
/walreceiver-resume investigation below — understand the mechanism before
touching corruption-critical code again.

### Next investigation: PS walreceiver resume across a mode-B recycle

Concrete questions to answer with targeted (debug-gated) instrumentation:

1. What LSN does the PS walreceiver pass to `START_REPLICATION` when it
   reconnects after the compute recycle? Is it ≥ the committed create
   record's LSN (so the SK legitimately never re-sends it)?
2. If yes: was the create record applied BEFORE the recycle and then lost
   (→ a flush/durability gap after all), or applied-but-to-a-stale
   timeline, or genuinely never applied because the walreceiver advanced
   `consistent_lsn` past it without applying? Distinguish via the
   per-record apply trace vs the resume LSN.
3. Does the recycle spin up a fresh walproposer epoch that re-streams from
   a `basebackup`/checkpoint that post-dates the create, leaving the PS to
   rely on a stream segment it already skipped?

### OVER-ADVANCE CAUGHT LIVE — but the MECHANISM is NOT yet proven (2026-06-12)

A `torn_wal_write -n 10 --no-fail-clean` run with `YB_PS_TRACE_REL_BLK=1663/5/1259/0/*`
(+ a new `decode` trace at `DatadirModification::put`) caught the over-
advance and a same-root `push_durable` panic in one mode-E attach. The
over-advance is real and reproducing. **BUT my first writeup over-claimed
the mechanism — see the CORRECTION below; the "how" is still open.**

Timeline from the PS log (`run_1781283488_3617073`):

```
consistent_lsn 0/239AE00, resume replication from 0/239AE00 (SK end 0/23E9EA8)
OVER-ADVANCE (rel):  unflushed 0/239B2C8 ≤ frontier 0/23E9C60   ← tripwire fired
OVER-ADVANCE (meta): unflushed 0/239B048 ≤ frontier 0/23E9C60   ← tripwire fired
WARN durable-walk:   unflushed 0/23B7398 ≤ target 0/23B7398 → YB fallback
PANIC push_durable:  over unflushed slot 0/23B7398   (unified_pages.rs:683)
```

Every offending LSN sits in the committed gap `[consistent_lsn, SK_end]`
the walreceiver re-streams right after attach. Mechanism:

1. `PageWriter::flush` captures `ingest_frontier = max(record_lsn_history)`
   = `0/23E9C60` (line 1099). The frontier comes from **noted records**,
   not from what's been `put`+snapshotted.
2. The documented contract (page_writer.rs:1086-1098) — "`commit` puts the
   whole batch before `note_record_lsn`, so the snapshot contains EVERY
   unflushed row ≤ frontier" — **is violated under attach-re-stream
   concurrency.** A row at `0/239B2C8` was `put` (it's in the FIFO) but
   missed the snapshot (the comment itself admits "sharded-map iteration
   races the put"), so after `mark_flushed` it is still unflushed yet
   ≤ frontier.
3. The flush advances the durable horizon to the **uncapped** frontier:
   `flush_horizon_lsn` (line 1172), `consistent_lsn_observed` **uncondit-
   ionally** (line 1363), and persists it via `advance_consistent_lsn`
   (line 1372, throttled) — and on shutdown `force_upsert_consistent_lsn_if_advanced`
   (line 415) re-persists `consistent_lsn_observed`.
4. The materializer trusts the over-advanced `flush_horizon`, targets
   `0/23B7398` (still unflushed) → `durable_walk` YB-fallback, then
   `push_durable` over the unflushed slot → **panic** (debug) / silent
   overwrite (release).
5. `get_consistent_lsn()` reads `remote_state.consistent_lsn` (advanced
   only after a successful persist). Once the over-advanced frontier is
   persisted (step 3) and the PS then restarts/re-attaches (the panic
   aborts the worker; any recycle suffices) **before a later flush writes
   the lagging rows**, the next resume starts PAST them → the committed
   catalog/heap rows are never re-applied → **permanent orphan.**

**Why the earlier `min_unflushed` cap failed:** it capped the persisted-
watermark candidate but (a) `consistent_lsn_observed` is advanced
unconditionally at line 1363 and (b) the shutdown `force_upsert` re-
persists that observed value — so an uncapped path leaked the over-
advanced frontier to disk anyway. (Also: the over-advance *tripwire*
checks the RAW frontier, so it keeps firing even with a correct cap — its
firing was misread as "cap didn't work".)

**Correct fix (proposed — needs OK before touching corruption-critical
code):** derive ONE safe watermark after `mark_flushed` —
`safe = min(ingest_frontier, min_unflushed_lsn().map(pred).unwrap_or(frontier))`
— and route ALL horizon advances through it: `flush_horizon_lsn`,
`consistent_lsn_observed`, the persisted `advance_consistent_lsn`, AND the
shutdown force-upsert. The lagging row flushes in a later flush, which then
advances the watermark — self-healing, no permanent loss, materializer
never sees an over-advanced horizon. Verify: tripwire may still fire
(detector of the raw race) but the orphan + push_durable panic stop across
`-n` iterations.

### CORRECTION 2 (2026-06-12): the over-advance MECHANISM above is NOT proven

Pushed back on the "snapshot races the put" story and it does not survive
a code read. Verified facts that make the over-advance look *impossible*
for a single in-order producer:

- `commit` does `put_batch().await` (synchronously lands the row in the
  buffer) **then** `finish_write → note_record_lsn` (`pgdatadir_mapping.rs:2689`
  then `:2706`; `timeline.rs:3054`). So put-before-note genuinely holds.
- `snapshot_unflushed(None)` captures the **entire** FIFO, not a prefix
  (`unified_pages.rs:550`), and has a `snapshot_fifo_orphan` probe.
- `record_lsn_history` is a fresh-empty per-`PageWriter` map at attach,
  and the failing run's PS log shows only ONE successful `START_REPLICATION`
  (from `0/239AE00`) before the over-advance — earlier connects were
  "connection refused" (noted nothing). No stale-high frontier carried in.

Under those rules `frontier=0/23E9C60` ⇒ every record ≤ it noted ⇒ every
page ≤ it already `put` ⇒ the `0/239B2C8` row was in the buffer before the
snapshot ⇒ snapshot captured it ⇒ durable after `mark_flushed`. The
over-advance should be impossible. Since the tripwire DID fire, a premise
is false. Remaining candidates (UNPROVEN):

1. **Evict/trim then re-push.** `mark_flushed` → `evict_to_capacity`
   (`page_writer.rs`), and the materializer `trim_key_up_to`. A durable
   low-LSN row evicted/trimmed, then re-touched by replay, re-enters the
   FIFO as fresh-unflushed at a LOW lsn. (The `lookup_chain_durable_if_complete`
   doc at `unified_pages.rs:449-461` already names this: "legacy compaction
   overlaps the walreceiver's replay, which re-puts rows already durable in
   YB as fresh unflushed cache entries.")
2. **A second producer** during attach churn breaks single-producer order.
3. **Aux (meta/SLRU) buffer isn't strictly LSN-ordered** — the meta
   over-advance fired in the same flush; the aux path uses a count-based
   drain against a clone.

**NOT re-proposing the cap until the mechanism is proven.** If it's
evict-then-re-push, a cap is the wrong fix.

### Instrumentation added (2026-06-12) to pin the mechanism

Uncommitted, debug-gated, diagnostic only:

- `decode` trace at `DatadirModification::put` (`pgdatadir_mapping.rs`) —
  earliest "walingest staged this page" signal.
- **FIFO provenance** (`unified_pages.rs`): the `unflushed_fifo` tuple now
  carries a global monotonic `push_seq` stamp; `push()` emits one
  `target=yb_fifo_prov stage=fifo_push key/lsn/seq` line per FRESH
  unflushed push (the enclosing tracing span names the producer —
  `connection{node_id}` vs `image_materializer` vs evict/replay).
- **Tripwire verdict** (`page_writer.rs:flush`): captures
  `snapshot_push_hwm = push_seq_hwm()` just before snapshotting; the
  over-advance error now logs `offending_seq`, `snapshot_push_hwm`, and
  `post_snapshot = offending_seq >= snapshot_push_hwm`.
  - `post_snapshot=true` ⇒ the row entered the FIFO during the YB round-
    trip at an lsn below the already-captured frontier (candidate 1/2 —
    non-monotonic / second producer). Grep `yb_fifo_prov seq=<offending_seq>`
    for the producer span.
  - `post_snapshot=false` ⇒ the row was present at snapshot time yet not
    promoted (snapshot/FIFO completeness defect).

Build green (`cargo build -p pageserver`). Next: re-run `torn_wal_write -n 10
--no-fail-clean` with `YB_PS_TRACE_REL_BLK=1663/5/1259/0/*` and read the
`post_snapshot` verdict + the producer span for `offending_seq`.

### RESOLUTION OF THE OVER-ADVANCE THREAD (2026-06-12): it is BENIGN — tripwires removed

The FIFO-provenance run settled it. Every over-advance the tripwire
caught was a **benign reconnect-replay re-push**, NOT data loss:

- The offending `(key, lsn)` (e.g. `…00c005…0001` @ `0/18D9968`) was
  pushed TWICE in one PS generation — seq `2846` then seq `5568` — both by
  the walreceiver (`connection{node_id=1}`), with `post_snapshot=true` on
  the second.
- Between the two pushes the log shows `walreceiver connection closed →
  starting replication from 0/18D9750` — a reconnect that rewound the
  stream to the durable `consistent_lsn` and replayed the committed gap.
- The row could only be re-`put` as a FRESH unflushed slot because it had
  been **evicted/trimmed after the first flush** — and eviction/trim only
  touch DURABLE rows. So the row was **already in YB** from the first
  pass. Advancing `consistent_lsn` past it loses nothing; the next flush
  re-writes it (idempotent) and clears the FIFO entry.

Therefore the over-advance is a cache-bookkeeping artifact, not a
durability bug. Both debug tripwires (rel FIFO + meta/SLRU) and the
`min_unflushed_lsn()` helper were **removed** and replaced with an
explanatory comment (`page_writer.rs:flush`). All the session's
provenance scaffolding (FIFO `push_seq`, `front_unflushed_provenance`,
`push_seq_hwm`, `fifo_push` trace, `decode` trace) was reverted —
diagnosis complete.

The proposed `min_unflushed`/frontier "cap" fix is therefore also
**withdrawn**: there was never a real over-advance loss to cap.

### LOCALIZED (2026-06-12): the corruption is the image materializer's fold+trim of pg_class block 0

Mode-A repro (`-n 6`, `YB_PS_TRACE_REL_BLK=1663/5/1259/0/*` + the
over-advance-loss detector + `decode`/`trim` traces) reproduced the
`torn_a` `pg_class` orphan (oid 73728) and pinned the mechanism:

- **Not over-advance**: the YB-point-read detector fired **0** times across
  9 reconnect windows where it provably ran.
- **Not a flush drop**: every traced pg_class `(key,lsn)` that reached
  `put` also reached `durable` (no put-without-durable).
- **All 42 `trim`s are on pg_class block 0** (every `torn_*` catalog row
  lives on block 0). YB shows block 0 is materializer-image-anchored
  (`is_image=t will_init=t` at lsn `0x1b643f8`); the trims are deltas
  ≤ that image.
- The compute's read of block 0 returns 26 rows with `torn_a` (oid 73728)
  **absent**; torn_a's create delta is ≤ the image (else it would survive
  as an untrimmed delta above it).

⟹ **The materializer folded a pg_class block-0 image that excluded
torn_a's committed row, then `trim_key_up_to` dropped the delta that
carried it — a durable committed row destroyed.** The walreceiver / flush
/ consistent_lsn paths are exonerated for this symptom.

### CLOG-WRITE TRACE (2026-06-12): clog flush durability is NOT the bug

Added an env-gated `yb_clog` trace (`YB_PS_TRACE_CLOG`) on the SLRU clog
write path: put → flush-snapshot → durable (put_slru_pages ok) + each
consistent_lsn persist. `-n 2` repro (orphan torn_a, create xid 16384):

- **0 clog block-0 writes were put-without-durable** (of ~19k). Every clog
  page write that happens reaches YB durably. ⟹ the commit's clog page is
  NOT dropped at flush; **SLRU/clog flush durability is exonerated.**
- This run's orphan create xid (16384) sits at the WAL floor (0/15404B0),
  in the first page pg_waldump can't decode (missing page header), so it
  wasn't re-traceable here — but the mechanism was fully decoded the prior
  run (torn_b xid 21505: COMMIT present + post-recovery PRUNE of its rows).

So the loss is NOT "clog write dropped at flush." Remaining candidates:
(a) the recovered compute's basebackup is served a clog folded/bounded
BELOW the commit (so it reads the create xid uncommitted and prunes before
the commit's clog is re-applied/served), or (b) the COMMIT's clog update
was never applied because its WAL was trimmed/not re-streamed on resume
(create xid at the floor + lagging consistent_lsn → SK trim).

Decisive next: capture the clog page the recovered compute READS at the
recovery basebackup (does it show the create xid committed?) — i.e. trace/
inspect the basebackup's served clog vs the durable clog at that LSN. That
distinguishes (a) basebackup-serving from (b) commit-not-applied, and is
the last fork before the fix.

### CLOG-LOSS VERIFIED (2026-06-12): the prune itself is the proof

Checked the durable clog SLRU for xid 21505 on the corrupted cluster: the
latest clog page-0 IMAGE has its bits = 0 (uncommitted), but the newest
durable versions are 24-byte SLRU set-bits DELTAS, so the folded value is
ambiguous from a raw read; and `txid_status` reads the COMPUTE's local
clog (which self-corrected after the prune), not the PS durable copy. So
"is the durable clog permanently holed" can't be read off cleanly — and it
doesn't matter:

**The PRUNE is the verification.** `PRUNE_ON_ACCESS` marks an insert-only
tuple (xmax=0) DEAD only when its `xmin` reads aborted/crashed in the
pruner's clog. pg_waldump shows the recovered compute (xid 23553,
post-recycle) pruned torn_b's rows whose xmin (21505) has a `COMMIT` record
in the WAL. ⟹ at prune time the recovered compute's clog (sourced from the
PS recovery basebackup) showed 21505 as NOT committed. The clog/
commit-visibility gap across the kill-recovery is confirmed; the prune
destroyed the rows irreversibly regardless of any later self-correction.

Fix territory (subtle — note the consistent_lsn cap already FAILED, so a
plain rel/aux min_unflushed cap is NOT sufficient): the clog SLRU commit
must be durable in the PS and reflected in the basebackup the recovered
compute reads BEFORE it prunes pg_class. Likely the SLRU/clog flush
durability + recovery ordering, OR the basebackup serving clog at a stale
LSN. Needs design, not another blind cap.

### ROOT CAUSE PROVEN via pg_waldump (2026-06-12): recovered compute prunes a COMMITTED txn's catalog rows because CLOG shows it uncommitted

Reconstructed the SK WAL (segno 1+2 chunks from `wals` → 16 MiB segment
files; fabricated seg1's offset-0 long header since WAL starts mid-page at
the initdb floor) and decoded with PG17 `pg_waldump`. For orphan torn_b
(oid 98304, create xid 21505):

- torn_b's CREATE (xid 21505) **COMMITTED**: `Transaction COMMIT` record at
  lsn `0/1E0E298` (names `relcache 98304` + toast/index oids); `txid_status(21505)
  = committed`. Its pg_class rows are at block-0 offsets **52,53,54,55**
  (the `INPLACE` catalog updates).
- A later `PRUNE_ON_ACCESS` (xid 23553, lsn `0/1EE73B8`, block 0) marks
  `dead: [...,52,53,55]  unused: [54]` — **exactly torn_b's offsets**,
  reclaiming the slots; a later CREATE reuses them.
- xid timeline confirms the recovery link: torn_b is **mode B's table**
  (mode B SIGKILLs the compute). create=21505 → fill → compute killed →
  recycled → the prune at 23553 is the RECYCLED compute.

⟹ **The recovered compute HOT-prunes a committed transaction's pg_class
rows because its CLOG shows that xid as NOT committed** (an insert-only
tuple is only pruned dead if its xmin reads aborted/crashed). torn_b's
pg_type row sits on a page not pruned in that window, so it survives →
orphan. ROOT = **CLOG / commit-visibility durability across the
kill-recovery** (the clog SLRU path — the aux buffer's count-based drain
that "can silently drop fresh SLRU/meta rows"; ties to the meta/SLRU
over-advance noted early). NOT the rel-page watermark or materialiser —
which is why those fixes did nothing.

Fix territory: ensure the commit's CLOG bit is durable in the PS and
correctly served to the recovered compute BEFORE it can prune pg_class
(SLRU/clog durability + recovery ordering), so prune never sees a
committed xact as aborted.

### CONVERGED (2026-06-12): orphan = compute HOT-prunes a committed-live catalog tuple

After falsifying both the materialiser and consistent_lsn, inspected a
corrupted cluster's `pg_class` with `pageinspect` (orphan = torn_b, oid
98304):

- torn_b's pg_class row is **absent from EVERY block** (full
  `heap_page_items` scan over all blocks, incl. dead/redirect — empty).
- Block 0 is the active insertion block: **48 `LP_DEAD` + 6 `LP_REDIRECT`**
  (heavy HOT churn), and high-oid inserts (106496+) have **reused
  reclaimed slots** there.

By elimination: the compute COMMITTED torn_b (its pg_type 98304 +
pg_attribute rows survive), so the compute's WAL contains torn_b's
pg_class INSERT. The storage path is faithful (materialiser +
consistent_lsn both exonerated by controlled experiments). So torn_b's row
existing-then-vanishing with its slot now holding a later object means
**the compute itself generated a PRUNE + slot-reuse over torn_b's
committed-live pg_class tuple** — i.e. the recovered compute **HOT-prunes a
committed-live catalog tuple as if dead** and reuses its line pointer.

⟹ Root is **compute-side (Postgres) HOT-prune / visibility under crash
recovery** (or the PS serving a page whose prune hints / `pd_prune_xid` /
xmin-committed bits mislead the compute's prune), NOT the durable
watermark or the materialiser. Consistent with why those fixes changed
nothing.

Decisive next: `pg_waldump` the pg_class WAL around a torn_X creation to
see `HEAP_INSERT` → (wrong) `HEAP2_PRUNE` → reuse, and the prune's
`snapshotConflictHorizon`/OldestXmin — does the recovered compute prune a
tuple whose creating xact is committed? Then the fix is in PG's
prune/visibility after a yb-light basebackup-recovery, or in the
xmin-commit hint bits the PS serves.

### CONSISTENT_LSN HYPOTHESIS FALSIFIED (2026-06-12) — 3 fixes, all 0/8

Implemented the full "capped + prompt" fix and verified it works
MECHANICALLY:
- `consistent_lsn` now advances and repeatedly catches up to the SK end
  (`clsn==sk` in the resume log), no longer pinned at the initdb floor.
- All persist sites (flush advance + `force_upsert`) re-cap at the current
  `min_unflushed` (rel FIFO + aux), via `safe_consistent_lsn()` — the
  over-advance-loss detector fires **0**.
- Catch-up flush fired 31× (`Timeline::flush_and_advance_consistent_lsn`).

Yet `torn_wal_write -n 8` = **0 passed, 8 failed**, corruption UNCHANGED
(orphan rowtype + missing rels). Across all three attempts (eager-uncapped,
per-flush cap, capped-everywhere) the result was 0/8 — WORSE than the 1/8
baseline (the aggressive catch-up flush adds a `compute never served reads`
deadlock).

⟹ **`consistent_lsn` lag / over-advance is NOT the root of the catalog
orphan.** Making the durable watermark track commits, and making it
provably safe, changes nothing. The line-pointer reuse (a committed
pg_class row's heap slot reused by a later object) happens regardless of
the watermark — so the recovered compute's view of pg_class block-0 free
space is wrong for a reason UNRELATED to durability lag.

ALL of these reverted (full `git checkout pageserver/src/`). New leading
suspect: the **recovered compute's free-space tracking (FSM fork) /
basebackup** — after recovery the compute reuses an occupied heap line
pointer, i.e. it believes the slot is free though the served heap page
holds the committed tuple. Investigate the FSM (fork 1) content the
basebackup serves vs the heap, and PG's slot-selection on INSERT after a
crash-recovery basebackup. Not the durable watermark.

### FAILED FIX (2026-06-12): catch-up flush + eager consistent_lsn persist made it WORSE — reverted

Attempt: on walreceiver catch-up (`streaming_lsn >= end_of_wal`), call a
new `Timeline::flush_and_advance_consistent_lsn()` = `pw.flush()` +
`pw.force_upsert_consistent_lsn_if_advanced()`, to make `consistent_lsn`
track commits instead of lagging at the 5s cadence.

Result: `-n 8` → **0 passed, 8 failed**, with NEW corruption beyond the
orphan — heap holes (`count=1018 but max=1485 — WAL replay left a hole`),
missing relations (`torn_c does not exist`), missing pg_attribute rows.
Strictly worse. Reverted both edits.

**Why it backfired (important):** `force_upsert_consistent_lsn_if_advanced`
persists `consistent_lsn_observed`, advanced UNCONDITIONALLY to each
flush's `ingest_frontier` (page_writer.rs:1363) — it carries the
OVER-ADVANCE (frontier past not-yet-durable rows). Persisting it EAGERLY
locks in a durable watermark past rows not in YB; a crash drops them. The
5s persist throttle was **accidentally mitigating** the over-advance by
delaying the persist until later flushes wrote the lagging rows.

**Revised fix design:** advancing `consistent_lsn` promptly requires it be
SAFE — a value where everything ≤ it is provably durable. Advance to
`min(frontier, pred(min_unflushed))`, persist only that, and route the
shutdown `force_upsert` + `consistent_lsn_observed` through the same cap.
(This is the earlier "cap" idea — now necessary, not wrong; it likely
"failed" before because the uncapped `observed`/`force_upsert` leaked.)
Two fix attempts have failed; align on the safe-advance design first.

### MATERIALISER EXONERATED + gap confirmed (2026-06-12, controlled experiment)

Disabled the materialiser by bumping BOTH fold triggers in
`pageserver_api::config` defaults (`image_chain_limit` 20 → 1e9,
`image_chain_payload_bytes_limit` 4096 → 1e9). A monitor confirmed the
materialiser was genuinely off during the run (`trim`+`fold` count stayed
**0** while `put` climbed 59→96→136 — ingest working, no folding).

Result: `torn_wal_write -n 8` = **1 passed, 7 failed** — the orphan
reproduces just as reliably with the materialiser OFF as ON (`-n 6` was
5/6 with it on). **⟹ the materialiser is NOT the cause.** The lp-41
overwrite lives in the raw compute deltas, not the fold. (The earlier
`-n 2` mat-off "2 passed" was small-sample luck.)

Gap confirmed directly on the preserved failed cluster:
- Slot reuse: torn_a's pg_class slot **(0,41)** now holds
  `pg_toast_81920_index` (oid 81924) — a LATER object reused torn_a's
  committed-row slot. torn_a (typrelid 73728, xid 16384) stays committed +
  live in pg_type (xmax 0).
- **`consistent_lsn` is pinned at the initdb floor `0/15404B0` while the
  SK races ahead** (`sk_end=0/162CFF0`), then lurches forward in big jumps
  each recovery cycle, always lagging `sk_end`. So the durable watermark
  badly lags committed WAL across the SK-kill recoveries — the PS
  re-streams `[0/15404B0 ..]` every reconnect. This is the Part-1-class
  non-advancement, and it is the enabling gap: with durability pinned near
  initdb across the churned recovery, pg_class block 0's line-pointer
  state diverges and a committed row's slot is reused.

Net: Part 2's catalog orphan is a **recovery/durability-lag (consistent_lsn
stuck at initdb) → heap line-pointer reuse over a committed pg_class row**,
NOT a materialiser or storage-fold bug. Fix territory: make `consistent_lsn`
advance during these SK-kill recovery cycles (so durability tracks commits
and the recovered compute's pg_class block 0 reflects all committed rows
before it resumes inserting).

### ROOT MECHANISM — line-pointer reuse over a committed tuple (2026-06-12, pageinspect-verified)

Decoded reliably with PG's own `pageinspect` (`heap_page_items` on both
the live folded page and the durable image bytes fetched from
`pageserver.pages` — no hand-rolled decoder):

- Durable **image** of pg_class block 0 (lsn `0x1b71348`): line pointer
  **lp 41** = torn_a's pg_class row — oid 73728, **xmin 16384, xmax 0
  (LIVE)**. `txid_status(16384)=committed`. So torn_a's pg_class row was
  committed AND durable.
- **Folded** (current) block 0: **lp 41 now holds oid 81924** (xmin
  18433) = **torn_b**; lp 48 = 81925 = torn_b_pkey. The line pointer was
  **reused**.

⟹ **torn_a's committed-live pg_class tuple was overwritten: a later
`CREATE TABLE torn_b` (iteration-2 mode B, xid 18433) reused heap line
pointer lp 41 that held torn_a's row.** pg_type/pg_attribute live on other
pages whose slots weren't reused, so they survive → orphan rowtype.

The PS stored everything faithfully (image with torn_a@lp41; a later delta
overwriting lp41 with torn_b) — which is why every storage-pipeline
detector (over-advance, flush, trim, fold, CLOG) was clean. The loss is a
**heap line-pointer reuse over a committed-live tuple across the mode-A
crash+recycle**: the recycled compute's view of pg_class block 0 did not
include torn_a (lp 41 looked free), so it inserted torn_b there. Root
points at the recycle **basebackup / consistent_lsn not reflecting
torn_a's committed create** at recovery time (a storage-visibility gap at
recycle), surfaced as a compute slot reuse. `yb-dump-wals --summary` shows
the SK WAL is gap-free (`0/15404b0..0/1d25be0`), so the WAL is intact — the
divergence is in what the recycled compute READ, not what the SK kept.

Next: determine consistent_lsn at the mode-A recycle vs torn_a's create
LSN — if the basebackup served block 0 below torn_a's create while
torn_a's create was already durable, that's the gap.

### CLEAN -n 2 REPRO (2026-06-12 late): torn CREATE, last pg_class write lost; needs churn

Reliable findings (from the compute's own catalog — no hand-decoding):

- `-n 1` PASSES all 7 modes; `-n 2` fails in iteration 2 (mode A passes,
  mode B's catalog check finds the orphan). **A single create does NOT
  reproduce — the orphan needs the repeated DROP+CREATE-of-torn_a churn
  across iterations** on the heavily-materialised pg_class block 0 + the
  SK kill.
- The orphan rowtype `torn_a` (pg_type oid 73730, **typrelid 73728**) has
  **xmin=16384, xmax=0** — committed by xid 16384, NOT deleted. Its 8
  `pg_attribute` rows (attrelid 73728) also survive. Only its `pg_class`
  relation row (oid 73728) is missing ⇒ **torn CREATE: the committed
  transaction's `pg_class` insert is lost while its `pg_type` /
  `pg_attribute` inserts survive.** Not a drop.
- torn_a (73728) is the **highest oid / last-created** rel; torn_c..g
  (32768..65536, iteration 1) survive on block 0. So it's the LAST
  pg_class block-0 write before the crash that's lost.

Methodology caveat: hand-decoding heap pages bit me twice (the `t_hoff`
offset; an image-vs-folded mixup) and `pageinspect get_raw_page` returns
empty on this compute, so the byte-level "where exactly does 73728
vanish" is NOT yet reliably pinned. The reliable signals (compute catalog;
the put→durable trace showing every pg_class put reached durable; the
over-advance / trim-race / CLOG detectors all clean) stand. Open: was
torn_a's pg_class insert never `put`, or put→durable then lost from the
folded read.

### (earlier, partly from a buggy decoder) CORRECTION + reopened (2026-06-12 late): decoder bug; image-vs-folded; CLOG fine

The walredo-I/O capture run reproduced the orphan, but two findings walk
back the "materialiser fold drops the tuple" conclusion below:

1. **Decoder bug.** `heap_page_oids`'s `t_hoff` offset was 18; correct is
   **22** (`t_choice`=12 + `t_ctid`=6 + `t_infomask2`=2 + `t_infomask`=2).
   So that run's `fold_image` `oids_in`/`oids_out` were garbage. Fixed in
   code; offset validated in Python against a live durable image.
2. **Image HAS the row; the folded read does NOT.** With the corrected
   offset the latest durable **image** of pg_class block 0 contains oid
   73728 (torn_a), but the compute's **folded** read (`pageinspect`,
   MVCC-correct) has NO tuple with oid 73728 on any block (439 live rels,
   none torn_a). So a delta ABOVE the final image removes 73728 in the
   folded view (page reinit/prune or delete) — not a materialiser fold.
3. **CLOG is fine.** `txid_status(16384) = committed`; the "CLOG/SLRU
   visibility loss" idea is dead.

Reopened hypotheses (need a clean repro to disambiguate):
- **Torn DROP**: torn_a's `pg_class` row legitimately deleted while its
  `pg_type` row's delete was LOST ⇒ orphan rowtype. (`-n` seed does
  `DROP TABLE IF EXISTS torn_a` each iteration; once an orphan exists it
  blocks DROP and cascades, muddying the accumulated state.)
- **Wrong reinit**: a will_init/FPI delta for block 0 from before torn_a's
  create re-applied above the image, reverting the page.

Next: re-run with the CORRECTED decoder and **`-n 1`** (single iteration —
avoids the orphan-cascade contamination) to capture the FIRST orphan
cleanly, and decode the exact post-image delta that makes oid 73728 vanish
from the folded page.

### (SUPERSEDED by the correction above) NARROWED to the walredo fold output (2026-06-12)

Added `fold_chain` (input-chain anchor + LSN list) and `fold_image`
(produced image LSN) traces to the materialiser; re-ran mode-A with the
pg_class trace. The pg_class-victim run (oid 73728) gave 4 chained folds
(each anchors on the previous fold's image) and 42 block-0 trims. Diffing
the **trimmed** LSNs against the **folded** LSNs:

- Every trimmed delta LSN ∈ a fold's input chain (comm -23 empty) ⇒ NO
  trim-race; nothing was trimmed that wasn't folded.
- The fold chains are clean: exactly one `will_init` anchor (the prior
  image) per fold, all other rows plain deltas ⇒ no early chain
  truncation. `parse_chain` is correct (base_img = anchor, deltas reversed
  to LSN-ascending, guards stray images).

⟹ torn_a's `pg_class` insert delta WAS fed to walredo (it's trimmed, and
every trimmed LSN was folded) yet the produced block-0 image lacks the
tuple. **The loss is in the fold's walredo output: an image missing a
committed tuple present in its input chain.** Everything upstream
(walreceiver, flush, consistent_lsn, chain assembly, trim boundary) is
exonerated.

Remaining probe: capture the losing fold's walredo INPUT (record
types/payloads) and OUTPUT page (tuple/oid set) for pg_class block 0, and
identify the fold where the live-tuple set first drops torn_a — i.e. does
walredo mis-apply torn_a's HEAP_INSERT, or does a later record in the same
chain remove it. Needs torn_a's exact create LSN (trace pg_type's CREATE
txn alongside) + page-tuple decoding of the fold I/O.

Superseded earlier sub-question (kept for history): WHY the fold's input chain excluded torn_a's delta —
either `read_chain_durable` returned an incomplete chain (truncated at a
will_init anchor above torn_a's delta, or the delta wasn't durable at fold
time), or an earlier bad fold already dropped it and this image inherited
the loss. Next: instrument the materializer fold to log its input-chain
LSNs vs the produced image LSN + the trim range, and catch a fold whose
input is missing a delta it then trims. `decode`=14 (bootstrap only) — the
interpreted-WAL walreceiver path does not go through
`DatadirModification::put`, so `put` (PageWriter) is the ingest signal here.

### Validation run (2026-06-12): noise gone, real corruption unchanged

`torn_wal_write -n 10 --no-fail-clean` after removing both tripwires +
relaxing the `push_durable` assert + the bookkeeping/FIFO fix:

- `push_durable overwriting` panics: **0** (was a per-attach loop before).
- `consistent_lsn over-advance` lines: **0**.
- `snapshot_fifo_orphan` probe: **0** — the FIFO-leak fix holds.
- Real corruption STILL reproduces (8/10 iterations), now as a clean data
  assertion instead of a debug abort:
  `FAIL — B compute: torn catalog — rowtype missing from pg_type: [torn_a]`
  (this run lost the `pg_type` row, oid 139265; prior run lost `pg_index`
  90117 — same class, lossy rel varies).

This VALIDATES the diagnosis: with both debug signals removed the
corruption still happens with zero panics / zero over-advance, proving
neither was the cause. The unit suites (`unified_pages` 26/26,
`page_writer` 49/49) pass. The real catalog tear (a committed catalog
page write from the last txn missing from durable YB after crash
recovery) remains OPEN.

### Status: Part 1 + deadlock + anchorless guards + harness fixes LANDED. The consistent_lsn over-advance is CONFIRMED BENIGN (reconnect-replay re-push of an already-durable row) — tripwires + provenance scaffolding removed, comment left in place. STILL OPEN and now cleanly separated: a REAL catalog/heap tear (e.g. `torn_a`'s `pg_index` rows 90117/90116 genuinely absent from durable YB while `consistent_lsn` sits above the highest durable page write) — a committed FIRST-TIME page write missing from YB, distinct from the benign over-advance. Mechanism for THAT loss is not yet pinned (was it ever `put`/flushed, or never ingested?). The debug-only `push_durable`-over-unflushed panic correlates with the failing runs and remains to be explained.

### DECISIVE NARROWING (2026-06-12 late): the orphan is a TRANSIENT STALE clog READ at recovery time — NOT durable loss, NOT consistent_lsn, NOT materializer

Run `run_1781307603_4037726` (`-n 6`, mode B orphan on `torn_a`) was left LIVE
(YB db `yb_storage_e2e_4037726_508707780`, PS pid 4040441, page-service
`127.0.0.1:64200`, `YB_PS_TRACE_CLOG=1`). The orphan first appears in
iteration 2 (iter 1 fully PASSED), so it needs churn/accumulation.

The `basebackup_clog` trace (clog page 0, committed xids in 16000..30000):
every recovery basebackup from `0/15404B0`..`0/1C40A88` served
`committed_count=0`, then `0/1D1FCD0` jumped to `2873` (xids 16384..19873).

Forensics on the LIVE durable state (querying `pageserver.slru_pages`
directly) settle every fork:

1. **Commit-bit deltas ARE durable.** Clog page 0 = ONE base image
   (`0/1540438`, the empty initdb page; the ONLY `will_init=true`/image row
   ever) + ~11.6k 24-byte `ClogSetCommitted` deltas. Delta wire format
   (big-endian): `tag=00000002 | vec_len(8B)=1 | xid(4B) | timestamp(8B)`.
   Monotonic xid↔LSN. xid **16384 commits at LSN `0/1B725B8`**; **1431**
   deltas with xid∈[16000,30000) sit at lsn ≤ `0/1C40A88`. So at backup_lsn
   `0/1C40A88` the basebackup SHOULD have served ~1431 committed — it served 0.
   ⟹ writes are NOT lost; the basebackup read dropped durable deltas.
2. **The read path is CORRECT on a quiescent system.** Replaying the *same*
   basebackup request now (`basebackup … 0/1C40A88` over port 64200) returns
   `committed_count=1431, lo=16384, hi=17814` — exactly the durable deltas.
   ⟹ the empty read was TRANSIENT, during active recovery, and has healed.
3. **Materializer is exonerated for clog.** `put()` only tallies REL rows in
   `delta_counts` (page_writer.rs:568/591); SLRU rows go straight to the aux
   buffer and are never materialized. The `chain_rows=3011` materialise batch
   logged just before the stale basebackup was rel pages, not clog.
4. **It is not simple temporal lag.** Log timeline around the stale backup:
   `0/1B725B8` delta written `durable` 23:41:25.110/.125; `consistent_lsn`
   persisted `0/1C40A88` at .164 (so clog ≤ `0/1C40A88` was provably durable);
   basebackup served `count=0` at .367 — ~250 ms AFTER the data was durable.
   No clog image, delete, or gen transition in that window.

⟹ **Root cause is a transient stale READ of clog page 0 during heavy
concurrent recovery (ingest + reconnect-replay + flush/drain + basebackup),
where the durable deltas exist but the served folded page lacks the bits.**
The recovering compute then HOT-prunes the "uncommitted"-per-clog but
present-per-heap catalog tuples (heap rel-pages survive — they drain by
identity), reuses the slots, and the rowtype is orphaned.

Note a reconnect-replay just before the stale backup: walreceiver restarted
from `consistent_lsn 0/1B69950` TWICE (puts of `0/1B725B8` at 23:41:20.122
and 23:41:23.428), then a flush snapshotted the whole clog run with
`frontier=0/1C40A88` at 23:41:24.837. The transient empty read happens in
this window — a candidate race between the flush's snapshot→durable→drain of
the aux SLRU buffer and the basebackup's durable+buffer blend/fold.

Next: a `read_chain` trace was added to `PageWriter::scan_slru_range` (clog
seg0 blk0: durable-row count, anchor/min/max LSN, buffered count, final
chain_len, target_lsn), env-gated on `YB_PS_TRACE_CLOG`. Rebuild + fresh
`-n` repro to catch the failing basebackup's assembled chain and tell a
SHORT durable query apart from a FULL chain that folds empty.

### REFRAMED (2026-06-12 late, run_1781309017_4056749): not clog — a STALE catalog HEAP PAGE; symptom is BIDIRECTIONAL

Fresh traced `-n 8` repro (clog `read_chain` trace live; db
`yb_storage_e2e_4056749_859009316`, PS pid 4060849 @ 64200 still LIVE). 2/8
passed, orphan from iter 4 mode B.

1. **Clog reads are CONSISTENT this run.** Aligning every `basebackup_clog`
   with its `read_chain`: `committed_count` turns nonzero exactly at the
   backup_lsn where xids ≥16000 first commit (`0/1C4E378`, count 1420); the
   chain's `max_lsn` equals the LSN of the last actual commit ≤ backup_lsn
   (e.g. `0/1FCAB60` reads max `0/1EEC3F8` because no page-0 commit happened
   in between — count correctly unchanged). NO stale clog read at the
   critical transition. So the previous run's clog `count=0`-at-`0/1C40A88`
   was an intermittent stale READ, not the universal mechanism.
2. **Symptom is BIDIRECTIONAL.** This run: `pg_class.torn_a` PRESENT
   (oid 139264, reltype 139266) but NO `pg_type` row for 139266 (rowtype
   lost). Previous run: rowtype present, pg_class relation lost. Opposite
   halves of the same `CREATE TABLE`.
3. **Pure clog-prune is IMPOSSIBLE for either run.** `CREATE TABLE` inserts
   the pg_class row AND the pg_type rowtype under the SAME xid/commit. A
   clog-visibility prune of that xid removes BOTH. One surviving means the
   pruning backend saw the xid COMMITTED — so the other tuple cannot be
   pruned as "uncommitted". (The pg_waldump "prune of a committed xid" in the
   earlier section is then a prune over a STALE page version, not a
   clog-uncommitted read.)

⟹ Unifying root: **the recovery basebackup serves a STALE version of one
catalog page — missing its last committed, durable update** — and WAL
replay from backup_lsn does not repair it (the relevant commit is ≤
backup_lsn). This is the SAME read-staleness class caught on clog in the
prior run, landing on a heap (pg_type) page here. The lossy half (pg_class
vs pg_type) varies run to run because it's whichever page's read goes stale.

Open: confirm directly that the durable PS `pages` row for the pg_type page
DID hold 139266 at recovery LSN (⇒ stale READ) vs never held it (⇒ lost
WRITE). Next experiment: add the `read_chain`-equivalent trace to the REL
read path (`PageWriter::scan_range`) and reproduce, checking whether the
failing basebackup's catalog-page read returns a SHORT/stale chain while the
delta is durable — mirroring the clog probe. `pageinspect` on the live
compute shows pg_type has 20 dead LPs (churn DROPs) — not conclusive on its
own.

### REL-READ TRACE built + run (2026-06-12 late): the basebackup carries NO heap pages — catalog pages reach the compute via GetPage during recovery

Added `trace_catalog_read_chain` (env `YB_PS_TRACE_CLOG`) to BOTH rel read
paths in `PageWriter` — `read_chain` (incl. the cache-only fast-path
early-return) and `scan_range` — logging assembled-chain stats (chain_len,
deltas, anchor, min/max LSN, target_lsn) for pg_class (1259), pg_type (1247),
and any user relation (relnode ≥ 16384 = the `torn_X` table + PK index).

Three traced `-n 8` repros (run dirs 1781309017 / 1781310330 / 1781310692 /
1781311197). Findings:

1. **The failure surface is stochastic**: same root shows up as a catalog
   tear (`rowtype missing from pg_type: torn_a`) OR a user-data hole
   (`count=N but max(id)=N-k — a committed record lost below a surviving
   one`). The `torn_X` table IS the counter table (`id BIGINT PRIMARY KEY,
   pad TEXT`), so its lost-row and its lost-rowtype are the same bug on
   different pages.
2. **ALL rel_read_chain traces are under `handle_get_page_request`; ZERO
   under `handle_basebackup_request`.** Root: the basebackup is NON-FULL
   (`basebackup.rs:466` only sends rels under `full_backup`), so it ships
   SLRU/clog + control file + aux files but NOT heap pages. The recovering
   compute reads catalog AND table heap pages from the PS via **GetPage on
   demand** during its own crash recovery. So the torn page is served through
   the GetPage read path, not the basebackup. (The clog stale-read in the
   prior run WAS basebackup-side because SLRU *is* shipped; heap is not.)

⟹ Refined: the stale/lost catalog (or table) HEAP page is exposed on the
**GetPage** path the recovering compute drives — `read_chain` /
`read_chain_cachefast` / `scan_range`. In mode B the PS is never killed, so
those reads hit the warm-cache fast path. Next: pin lost-WRITE vs stale-READ
by comparing, on the live repro cluster, the PS durable `pages` chain for the
pg_type page (does it hold the rowtype tuple at the compute's read LSN?)
against the compute's view, and locate the failing GetPage in the trace.

### NOT A LOST WRITE — the rowtype IS durable in the PS (2026-06-12, durable decode, run 1781311197)

Decoded pg_type (relnode 1247, fork 0) directly from the durable
`pageserver.pages` table on the live repro cluster (db
`yb_storage_e2e_4086554_896266490`, timeline `ef55e377…`). pg_type has 15
blocks; blocks 0–13 are untouched initdb images (LSN `0/1540438`); block 14
is the active tail (6 images + 56 deltas). Heap-decode (oid = first 4-byte
attr at `lp_off + t_hoff`, `t_hoff` at tuple byte 22) of the block-14
images:

| durable image LSN | has 139265 (array) | has 139266 (rowtype) | oid range |
|---|---|---|---|
| `0x1C47AF0` | no | no | 14453..81922 (pre-torn_a) |
| `0x20A2428` | no | no | 14453..122882 |
| **`0x225A620`** (latest) | **YES** | **YES** | 14453..**139266** |

⟹ **torn_a's rowtype 139266 reached durable YB.** This is NOT a lost write;
the data is safe in the PS. The corruption is introduced on the recovery
READ/REPLAY side, with the committed catalog tuple sitting durable in YB the
whole time. (The compute's *current* folded page omits 139266 only because
the `-n 8` cascade's later iterations DROP'd it and pushed oids to 147458 —
contamination, not the original loss.)

GetPage correlation (rel_read_chain on relnode=1247 blk=14): every read
carries a valid `anchor` (will_init image) and a chain whose `max_lsn`
tracks `target_lsn`; the anchor advances `0/1540438 → 0/1C47AF0 →
0/20A2428 → 0/225A620` as the materialiser installs images. No gross
stale/short read in steady state. 139266's insert lands right around
`0x225A620` (the image just above the last read that still lacks it).

⟹ Open (needs a STOP-AT-FIRST-FAIL repro — the `-n 8` cascade overwrote the
clean iter-4 state): does the recovering compute read block 14 (a) at a
recovery target BELOW 139266's insert and then NOT replay the insert (redo
starts above it per the basebackup checkpoint), leaving the lazily-served
page permanently without it; or (b) via some cache/relcache path that pins a
pre-139266 page? The PS durably has it; the question is purely why the
recovered compute's view omits it. The single-iteration clean repro +
control-file redo LSN vs 139266 insert LSN is the next decisive datum.

### CLEAN STOP-AT-FIRST-FAIL repro (2026-06-12, run 1781312444): basebackup serves a clog MISSING PAGE 1; the same request later serves it — transient stale SLRU read; SK rewind below page-1 creation implicated

Added `E2E_STOP_ON_FAIL` to the harness (`test/e2e/src/main.rs`): with `-n`
(churn) it re-enables `fail_fast` so the run halts at the FIRST failing
iteration; paired with `--no-fail-clean` the failing cluster is left LIVE
and UNCONTAMINATED (db `yb_storage_e2e_4099986_852668380`, timeline
`ba723e79…`, PS pid 4104974 @ 64200).

First failure was iter-3 **mode A** (SK kill) — the recovering compute
**won't start**:
```
FATAL: could not access status of transaction 33792
DETAIL: Could not read from file "pg_xact/0000" at offset 8192: read too few bytes.
LOG: aborting startup due to startup process failure
```
xid 33792 is on clog PAGE 1 (page 0 = xids 0..32767; 33792 ≥ 32768). The
basebackup served `pg_xact/0000` = **8192 B (page 0 only)** — page 1 missing.

Forensics on the live cluster:
- **Durable YB HAS clog page 1**: `slru_pages` kind=0 seg=0 has blknum=0
  (21506 rows) AND **blknum=1** (263 rows, will_init, first row LSN
  `0x221ba00`); consistent_lsn `0x22417f0`. So page 1 is durable below the
  backup_lsn.
- **The failing basebackup was at `0/22417F0`** (compute log: "starting with
  neon basebackup at LSN 0/22417F0" → immediate FATAL). Re-issuing the SAME
  request `basebackup … 0/22417F0` against the live PS now returns
  `pg_xact/0000` = **16384 B (pages 0+1)** — CORRECT. ⟹ transient stale read
  at recovery time, healed now. Identical pattern to the clog-commit-bit
  staleness, here on the clog SEGMENT SIZE / page count.
- **Path**: basebackup `get_slru_keyspace` → `get_slru_segment_size(Clog,0)`
  reads the meta key `slru_segment_size_to_key(Clog,0)` at backup_lsn
  (`pgdatadir_mapping.rs:881`). A stale value (1 not 2) truncates the served
  clog. Meta reads blend a write-through `meta_cache` single-key fast path +
  durable + aux buffer (`scan_meta_range`); the cache is updated on every
  higher-LSN put, so a plain stale cache shouldn't occur.
- **SK rewind smoking gun**: after the mode-A SK kill, the compute log shows
  `synced safekeepers lsn=0/217C4E0` — BELOW page-1 creation `0x221ba00`.
  The walproposer rewound the SK WAL to `0x217C4E0`, discarding the WAL that
  created clog page 1, *while the PS had already durably ingested page 1*.
  It then re-accumulated to `0/217CFC0` → `0/22417F0`; the basebackup at
  `0/22417F0` served the stale page-0-only clog.

⟹ Same root family confirmed end-to-end: the recovery basebackup's SLRU read
is transiently stale (commit-bits in one run, segment-size/page-1 here) —
the PS durably holds the data; the read at recovery time misses it; the same
read later returns it. Next: trace `get_slru_segment_size`(Clog) at
basebackup time (returned size, target_lsn, cache-vs-durable-vs-buffer) and
pin how the SK rewind to `0x217C4E0` interacts with the PS's already-durable
page-1 / size=2 meta to make the read return size=1 at `0/22417F0`.

### ROOT CAUSE PINNED (2026-06-12, run 1781313520, size+cache traces): it is NOT the size meta — block-1 CONTENT read lags its size-meta visibility

Added traces: `get_slru_segment_size`(Clog) logs the returned size + lsn;
`scan_meta_range` logs the clog size-key served from cache vs durable+buffer.
Full `-n 8` run; iter-3 mode A FATAL'd again on xid 33792 (clog page 1).

The size-meta hypothesis is **REFUTED**. At the FATAL compute's basebackup
LSN `0/22473A0`:
- `slru_segment_size`(Clog) returned **size=2** (correct — page 1's extend
  wrote size=2 at `0/220B828`, below `0/22473A0`), served from the
  write-through `meta_cache` (`row_lsn=0/220B828`). The size meta is NOT
  stale.
- Page-0 commit bits were fine (`basebackup_clog committed_count=10006`).
- **But two basebackups at the SAME `0/22473A0` disagree on block count**:
  one logged `Collected 3 SLRU blocks` (clog = page 0 only → `pg_xact/0000`
  = 8192 B → the FATAL'd compute), the other `Collected 4 SLRU blocks`
  (clog = pages 0+1 → correct).

⟹ The size meta says 2 (keyspace includes clog block 1), but the
`get_vectored` CONTENT read of block 1 transiently returned **nothing** — so
the tarball got only block 0. Block 1 IS durable (`slru_pages` blknum=1 first
row `0/220B828` ≤ backup_lsn). So the failure is a read-time consistency gap:
**block 1's content read lags its size-meta visibility at backup_lsn.** The
`meta_cache` retains `size=2` (write-through, survives flush drain), while the
SLRU block-1 row is momentarily invisible to `scan_slru_range`
(durable+`slru_buffer` blend) — most likely the post-snapshot/pre-durable (or
drained-but-not-yet-YB-visible) window of a concurrent flush during recovery.

This is the SAME mechanism as the clog-commit-bit staleness and the
catalog-heap staleness: **a basebackup/GetPage SLRU/page read at backup_lsn
transiently returns less than what is durable in YB.** The size-meta cache
just makes the clog-page-1 variant especially sharp (keyspace says 2, content
read yields 1).

⟹ Fix direction (for sign-off): make the basebackup's SLRU read internally
consistent — e.g. the per-block content read must see everything the size
meta (and durability watermark) claims at `backup_lsn`; or bound the
basebackup at an LSN where all SLRU blocks < size are provably durable; or
detect served-block-count < size and wait/retry. The unifying invariant to
restore: **a read at `backup_lsn` must reflect every write with LSN ≤
`backup_lsn` that the PS reports durable** — today the meta/size, SLRU-block,
and rel-page sub-reads can momentarily disagree during recovery.

### YB UPSERT MODE EXONERATED (2026-06-12, run 1781315427)

Tested the hypothesis that `yb_enable_upsert_mode=true` (the fpu write-path
fast path that skips read-before-write) causes the transient stale read.
Disabled it (`set_fast_path_upsert=false` on the fpu store) + added
`ON CONFLICT DO NOTHING` to the three put SQLs (`pages`/`slru_pages`/`meta`)
so the reconnect-replay re-push stays idempotent without upsert mode (a naive
disable alone hits `duplicate key violates *_pkey` and stalls the PS — the
affected-count check only logs, so it's harmless). Clean run: **0 flush
errors, but the corruption STILL reproduces identically** — iter-2 mode B
orphan rowtype, 1 passed / 7 failed (same as upsert-on). ⟹ Upsert mode is NOT
the cause; the staleness persists with standard transactional INSERTs.
Reverted all experiment edits. Pursuing fix direction 1.

### FIX LANDED + VERIFIED (2026-06-13, approach A): pre-flush the basebackup

Root cause (final): `handle_basebackup_request` `wait_lsn`s on the APPLIED
frontier (`last_record_lsn`), not the durable watermark (`consistent_lsn`).
When `backup_lsn > consistent_lsn` the served snapshot mixes durable YB with
not-yet-durable in-RAM buffer state and can read inconsistently during
recovery. Confirmed precisely: at the failing basebackup `0/22473A0`,
`consistent_lsn` was `0/216CDE8` < clog-page-1 LSN `0/220B828` < `backup_lsn`
`0/22473A0`; block 1 wasn't durable yet so its content read came back empty
while the (write-through cached) size meta read 2 → truncated `pg_xact` →
compute FATAL.

Fix (approach A):
- `Timeline::flush_for_basebackup()` (page_server/timeline.rs) — flush the
  PageWriter so every write ≤ the applied frontier (hence ≤ `backup_lsn`,
  already guaranteed ≤ applied by `wait_lsn`) is durable in YB.
- `handle_basebackup_request` (libpq) AND the gRPC `GetBaseBackup` handler
  call it after `wait_lsn`/`check_lsn_is_in_scope`, before reading the
  tarball.

Verification: `torn_wal_write -n 10` twice — **0 corruption signatures**
(no clog FATAL / torn catalog / orphan / WAL hole), where the bug previously
reproduced ~7/8 iterations.

### Port-bind race fixed (2026-06-13)

With the corruption gone, the only remaining `-n 10` failures were a
pre-existing infra race: during a fast compute recycle (`endpoint stop` →
`endpoint start`, or a `start` retry) a new compute_ctl could try to bind its
HTTP ports (55433/55434) while the predecessor was still releasing them, and
`compute_tools/src/http/server.rs` panicked on the first `EADDRINUSE`
(server.rs:211) → "endpoint start exited 1" → counted as data loss. 37 such
panics in one run. Fix: `Server::listener()` retries the bind on `AddrInUse`
for ~10s (50 × 200ms) before failing. Re-run: **0 bind panics** (was 37),
the retry absorbed the hand-off 18+ times, 0 corruption.

NOTE: diagnostic scaffolding still uncommitted and to be removed before the
fix commit — clog/rel/meta read traces (page_writer.rs, pgdatadir_mapping.rs,
basebackup.rs `basebackup_clog`), the `YB_PS_TRACE_CLOG` allowlist entry
(background_process.rs), and the `E2E_STOP_ON_FAIL` harness helper
(test/e2e/src/main.rs — arguably worth keeping). The two fixes above
(timeline.rs, page_service.rs, server.rs) are the keepers.

### (superseded notes below) The likely concrete mechanism (to confirm while implementing fix 1): the
basebackup serves at `backup_lsn` after `wait_lsn`, which waits on the
**applied frontier** (`last_record_lsn`), NOT the durable watermark
(`consistent_lsn`). When `backup_lsn > consistent_lsn`, the read blends
durable YB + in-RAM cache; during recovery a block at LSN ≤ `backup_lsn`
can be momentarily absent from BOTH (ingested-then-evicted/drained but its
flush not yet durable, or not re-populated after a walreceiver reconnect).
The two `0/22473A0` basebackups ("Collected 3" then "Collected 4") differ
precisely because block 1 became durable between them. ⟹ Fix 1 concretely:
ensure everything ≤ `backup_lsn` is durable before serving — either flush
the PageWriter up to `backup_lsn` in `handle_basebackup_request` after
`wait_lsn`, or clamp the basebackup read to `consistent_lsn`. Trade-off to
weigh: per-basebackup flush latency vs. serving a slightly older (durable)
LSN.
