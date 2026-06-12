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

### Status: SK-restart heap loss fixed (part 1); deadlock fixed (over-strict assert); read/materialiser anchorless guards + panic observability added; **catalog-orphan data corruption (iter-2 mode B) still open** — the missing-delta hole / flush over-advance the unflushed-tripwire can't see

A correct fix is in the `consistent_lsn`/flush-frontier machinery and
must be designed + verified (not guessed) — advancing `consistent_lsn`
only once *every* page of *every* record ≤ frontier is confirmed durable
in `pageserver.pages`. The catalog-consistency gate
(`assert_catalog_consistent`) and the `-n 2` repro above make it
deterministic to validate a fix against. Also fold the catalog/index
checks into the permanent corruption gate (`final_verdict` only watches
the metric counter and is blind to these tears).
