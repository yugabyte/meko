# Pageserver GC

The pageserver stores each version as its own row — relation blocks in
`amp_storage.pages`, clog/multixact blocks in `amp_storage.slru_pages`, and
everything else (rel-size sentinels, directory listings, control /
checkpoint, aux, two-phase) in `amp_storage.meta` (schema:
[`yugabyte.md`](yugabyte.md)). GC reclaims the rows no read can reach any
more — superseded versions, blocks past a truncation, and the rows of a
retired object (a dropped relfilenode, a truncated SLRU segment, a dropped
database's keys) — by `DELETE`ing them. It is pure space reclamation: it
never changes what a correct read returns.

This is the pageserver's **page** GC. It is separate from **WAL GC** (the
safekeeper trimming WAL segments once they are durably ingested and past
retention) — a different component with its own config; the two don't
interact beyond `consistent_lsn` bounding the GC cutoff (below).

Removing dead tuples / defragmenting within a page is ordinary Postgres
`VACUUM` in the compute (emits WAL, ingested like any write) — orthogonal
to this.

GC is **default off**; a branch turns it on with `enable_gc` — see
[Config](#config).

## Model

Finding garbage after the fact is expensive, so instead the write path
**records each garbage-producing event** as it happens — a base write, a
truncation, an object drop — as a row in `amp_storage.pending_gc` (a
sibling queue table), tagged with the LSN at which that event's garbage
becomes collectable. A periodic **drain** then acts on each record once
the GC cutoff has advanced past that LSN. A candidate names its garbage by
an encoded `Key` (the same 18-byte layout `meta` uses — a rel block, an
SLRU block, or a whole object) plus a `gc_kind` that names both the
**operation** and its **target table**, so the drain dispatches on
`gc_kind` alone: each kind maps 1:1 to an act function and the key is never
re-inspected for routing. The seven kinds are `{trim, drop}` ×
`{pages, slru, meta}` plus `shrink` (rel `pages` only) — the emitter, which
knows the table it wrote, stamps the kind; the key stays pure identity.

| operation | key | recorded when | becomes collectable |
| --- | --- | --- | --- |
| **trim** (`TrimPages`/`TrimSlru`/`TrimMeta`) | the block (rel / SLRU) or meta key | a block's every-n-th new base (`is_image`/`will_init`), or a meta put; `due_lsn` = the newest counted base's LSN | that key's versions below its newest base `<= cutoff` |
| **shrink** (`ShrinkPages`, rel-only) | the rel-size key | a relation is truncated at `due_lsn` | blocks at/above the post-truncation size |
| **drop** (`DropPages`/`DropSlru`/`DropMeta`) | the retired object's key | a relfilenode drop, an SLRU segment truncation, each dead key of a `DROP DATABASE`, or a two-phase resolve, at `due_lsn` | all of the object's rows `<= due_lsn` |

`due_lsn` is the LSN at which the garbage became collectable; a candidate
is actionable only once the cutoff reaches its `due_lsn`. A `gc_kind` the
drain doesn't recognise is alarmed and left queued. The
`gc_key_decode(bytea)` SQL function unpacks a `key` for inspection.

## Candidate emission

Candidates split by **durability requirement**, not by producer:

- **Trim candidates** (self-healing): both producers — the ingest
  window flush and the materialiser — sample their base writes into one
  shared in-RAM buffer on the `PageWriter` (`GcTrimCandidateEmitter`,
  `pageserver/src/gc_trim_candidates.rs`): `will_init` rows of `pages` /
  `slru_pages`, and every `meta` put (meta is full-overwrite — no
  `will_init`, so each put is a base). Flushed by its own loop at 500
  distinct keys or 5 s, whichever first (same-key candidates fold
  to the latest `due_lsn`). **Best-effort by design**: a crash or
  failed flush loses at most one buffer, healed by the block's next
  base — trims are cumulative, so the next candidate's greedy-to-cutoff
  trim re-covers the lost span. Only a block whose final-ever base fell
  in a lost buffer leaks (bounded; reclaimable by a future backstop
  sweep). The flusher shuts down after both producers stop, with a
  final flush, so a clean shutdown loses nothing.
- **Shrink and drop events** (one-shot — a retired object gets no future
  base to re-emit a candidate) stay **window-durable** on the ingest
  path: buffered alongside the window's rows and flushed with them, so
  a buffer lost to a crash implies `consistent_lsn` didn't advance and
  the producing record replays and re-emits — `put_rel_truncation`
  (shrink), and the drops from `put_rel_drops`, `drop_slru_segment`,
  `drop_twophase_file`, and each dead key under `drop_dbdir`
  (`DROP DATABASE`).

Best-effort is safe for trim because no single trim candidate is
load-bearing (the block's next base heals it), and both producers
record a candidate only after its base is durable — candidate flushed
⇒ base flushed already.
Window durability wasn't buying ingest trim a re-emission guarantee
anyway: the sampling counter below is in-memory and resets on restart,
so a replayed window may emit at a different base or not at all.

`gc_candidate_every_n_bases` (default 10) samples both trim producers
through one shared per-key counter: one trim candidate per n bases per
key, stamped `due_lsn = max LSN among its n counted bases`. The
max matters only cross-producer — a materialiser image lands at its
fold point, below bases ingest has since counted, and stamping the
n-th base's own LSN would make the drain visit (and dequeue) the
candidate before the counted garbage is collectable, orphaning it
until the block's next candidate. `n>1` trades queue volume for up to `n−1` bases of
garbage retained on a block that goes cold mid-cycle (the next
candidate's trim still collapses the skipped cycles). **Sampling is
trim-only** — shrink and drop events are always recorded.

**Insert invariant: no candidate lands at `due_lsn <=` the current
cutoff**, so GC (which acts at `due_lsn <= cutoff`) never races a fresh
insert and a later tick always selects it. Drop/shrink events satisfy
it structurally — they flush with their window, so at insert time
`due_lsn > consistent_lsn >= cutoff`. A trim candidate can sit in the
emitter past that point — a materialiser image is stamped at an old
LSN (the folded delta's), and an ingest candidate can sit in the
candidate buffer while the cutoff advances past its just-landed base
— so the
emitter's flush clamps each row to
`due_lsn = max(due_lsn, applied_gc_cutoff_lsn + 1)` — safe for a trim
candidate, which only sets when the block is next visited (the trim is
greedy to the cutoff regardless). The flush reads the cutoff without
locking it across the INSERT, so a concurrent tick can advance the
cutoff past the clamped value and skip that one row; it waits for the
periodic floor reset (or a restart), and the block's next base
re-covers the garbage. This invariant is also what lets the dequeue
sweep by block-range instead of naming exact PKs (see the dequeue step): a
base landing after a tick's trim anchor is clamped above the cutoff, so
the `<= cutoff` sweep only removes a candidate whose garbage the trim
didn't collect in the rare clamp race (above), where the block's next
base re-covers it.

## Drain

Each `gc_period` tick, `Timeline::gc_iteration` (`page_server/timeline/gc.rs`)
runs:

1. **Cutoff.** Compute `min(retention window, live leases,
   consistent_lsn)`, floored at the branch's own ancestor LSN. Persist
   `applied_gc_cutoff_lsn = max(old, new)` to `branch_state`
   (generation-fenced, monotone), then publish it to the in-memory
   `Rcu<Lsn>`.

   **Child fork points** pin the cutoff too, but they are folded in only
   *inside the persist UPDATE itself*, which clamps to `min(ancestor_lsn)`
   over every child row still present. A deleted child can still be read
   until branch-deletion GC cleans it up (for instance, to restore it
   within the PITR interval); only removing its `branch_state` row —
   that GC's last step (no such flow yet — see
   [#362](https://github.com/yugabyte/amp/issues/362)) — releases the
   pin.
   Reading the child min separately (a pre-read folded into the computed
   min) would be the fork ↔ cutoff race: a fork committing between the
   read and the write gets cut past.
   Instead the two sides interlock on the parent's `branch_state` row —
   fork registration (controller `RecordBranch`) inserts the child row
   and, in the same transaction, runs a conditional write on the parent's
   row (`WHERE applied_gc_cutoff_lsn <= fork_lsn`, self-assignment SET).
   Both sides write the parent row, so they serialize instead of
   write-skewing: fork-first ⇒ the publish's clamp sees the child and
   holds at its fork point; publish-first ⇒ the fork's condition sees the
   advanced cutoff and the registration rolls back (rejected). A plain
   read on either side would not conflict and both could commit on stale
   views of each other.

   The same shape closes the fork ↔ **drop** race, one row over. A drop
   tombstones the parent's `amp_registry.branches` row while checking
   `NOT EXISTS (live children)`, and the fork's liveness check was a read
   of that row — so both could commit and leave a tombstoned parent with
   a live child, whose pre-fork history lives under the *parent's*
   `branch_id` and would then be reclaimed out from under a live branch.
   So `RecordBranch`'s liveness check is a conditional write of that row too
   (`WHERE dropped_at IS NULL`, self-assignment SET), which is what makes the
   two conflict. A row lock would not: it orders the drop after the fork but
   never refreshes its snapshot, so under repeatable read the drop's
   `NOT EXISTS (live children)` still sees no child and tombstones the parent
   — silently, with no serialization error. No lock mode helps, because the
   child does not exist yet when the fork checks, and a phantom cannot be
   locked.
2. **Await readers.** Wait out in-flight reads still holding a guard on
   the *old* cutoff before deleting anything they might need. Reads never
   wait for GC; GC waits for reads.
3. **Select.** `gc_select_due_candidates`: up to `gc_drain_limit`
   (default 10 000) candidates with
   `floor < due_lsn <= cutoff`, oldest-first, one bounded range scan per
   placement bucket. The **floor** (`GcDrainProgress::floor`) marks how
   far the drain has progressed: every candidate at or below it has been
   handled — dequeued, acted-but-requeued (a busy-object drop), or parked
   (an unknown kind, a malformed key) — so the scan seeks past those rows' YB delete
   tombstones instead of re-skimming them from the queue head every tick.
   It is in-memory only — 0 after a restart, so the first tick scans
   from the head. Both the select and the dequeue use it as the lower
   bound of the tick's `(floor, cutoff]` window, but it isn't a
   correctness bound: too low costs a redundant scan, too high defers
   rows to a later tick or the floor reset.

   **Periodic floor reset.** Every `gc_queue_floor_reset_interval`
   (default 2 h) a tick forces the select floor to 0, so the scan starts
   from the queue head (still bounded by the cutoff). The interval is
   wall-clock, so the sweep cadence holds regardless of the tick rate
   (`gc_period`). This re-selects the two things that sit *below* the
   floor un-dequeued: a busy-object drop still waiting for its object to go
   idle, and a rare clamp-race trim straggler (a candidate the `due_lsn`
   clamp couldn't lift above a concurrently advancing cutoff). Without
   it, on a pageserver that never restarts those would linger
   indefinitely; the periodic reset bounds that to the interval. It does
   **not** catch a leak with *no* queue row (e.g. an emitter candidate buffer
   lost to a crash before flushing); that class waits on a future
   full rel-directory sweep (see the design doc's full-sweep alternative).
4. **Validate & partition** (`classify_candidate` → `partition_by_kind`).
   Every selected row is checked against its `gc_kind` before it can reach
   an act function, because the act paths take the object identity from the
   `key` *blind* (`rel_fork_of` / `slru_seg_of` just read Key fields) — a
   key whose shape doesn't match its kind would delete a wrong, live
   object. So each kind requires its keyspace's shape: rel block
   (`TrimPages`) or rel-size sentinel (`ShrinkPages` / `DropPages`), SLRU
   block (`TrimSlru`) or segment-size sentinel (`DropSlru`), any `meta`-table
   key (`TrimMeta`). A shrink additionally needs a `relsize` in `BlockNumber`
   range — `gc_act_shrink_fork` deletes `blknum >= relsize`, so a negative one
   would match every block and delete the whole fork.

   `DropMeta` is the one kind with an **allowlist** rather than a keyspace
   test, because trim keeps one version and drop keeps none: `gc_act_trim_meta`
   keeps the newest version `<= cutoff`, while `gc_act_drop_meta` deletes
   `lsn <= due_lsn` outright. A drop is therefore only correct on a key whose
   object is *dead*, and nearly every `meta` key is live state for something —
   a size sentinel (owned by `Drop{Pages,Slru}`, which delete it with the
   object), a dbdir, the twophase *directory* (whose key is
   `twophase_file_key(0)`). So `DropMeta` accepts exactly the four shapes its
   emitters produce — relmap, rel-dir, sparse-reldir entry, twophase file —
   and adding an emitter means adding its shape. `TrimMeta` needs no
   allowlist: every `meta` row is self-contained (an image, or a `will_init`
   record that fully replaces the key), so keeping the newest `<= cutoff`
   cannot lose readable state for any key.

   A refused row alarms on `correctness_errors_total` under its own label
   (the log line carries the row), enters no bucket, and is **left queued** —
   never dequeued. That holds for every fault, including ones that look like
   plain corruption, because acting and dequeueing are one decision here: the
   garbage the row names is leaked either way, so purging the row would buy
   only silence, and would forfeit the chance for a fixed or newer binary to
   drain it. An unknown `gc_kind` is the clearest case (a newer binary
   probably wrote it, and roll-forward reclaims it), but a shape or `relsize`
   fault has the same rollback explanation.

   The cost is that the alarm re-fires on each floor reset while the row
   sits there — which is the intent; a stuck row should stay lit rather than
   bump a counter once. A refused row also costs one drain slot per reset,
   bounded: a capped tick still advances the floor past it. To clear rows
   judged unrecoverable, `DELETE` them from `pending_gc`.

   A wrong-length `key` bytea is caught even earlier, at decode in the
   select: there's no `Key` to classify, and none to build a dequeue
   predicate from either, so it is dropped from the batch and left queued.
   It still counts toward the tick's limit and floor anchor
   (`GcSelectedCandidates`), so one corrupt row can't strand the rows above it.
5. **Act**, ordered **drop → shrink → trim** (coarser first — a dropped
   object moots its own shrink/trim candidates), each dispatched by its
   `gc_kind` to the matching act function:
   - **drop** — delete the object's rows `<= due_lsn` (bounded by the drop
     LSN, so a later reuse — relfilenode, wrapped-around segno, recreated
     database — keeps its newer rows): a rel fork (`gc_act_drop_fork`:
     `pages` + the rel-size meta key), an SLRU segment (`gc_act_drop_slru`:
     `slru_pages` + the segment-size meta key), or dead meta keys
     (`gc_act_drop_meta`).
   - **shrink** (`gc_act_shrink_fork`, rel-only): delete `pages` with
     `blknum >= relsize AND lsn <= due_lsn`, using the `relsize`
     (truncation target) carried on the candidate — no meta read. The
     `lsn <= due_lsn` bound keeps a re-extended block, whose new base sits
     above the truncation. (SLRU truncation is segment-granular, so it
     drops rather than shrinks; `meta` isn't shrunk.)
   - **trim** — keep the newest base `<= cutoff` (the anchor) and
     everything above, delete below: rel blocks (`gc_act_trim_blocks`) and
     SLRU blocks (`gc_act_trim_slru`, single-tablet — the branch hash key
     plus a `(kind, segno)` range prefix); `meta` has no `will_init`/chain,
     so `gc_act_trim_meta` just keeps the newest version `<= cutoff` per
     key. Trims across objects run in parallel (`GC_ACTION_PARALLELISM`,
     default 2, scaled) — distinct objects are distinct keys / tablets,
     no ordering constraint.

   Every act on `pages` must bind `bucket_id`, the leading column of that
   table's all-range key (see [`pageserver.md`](pageserver.md) "Why the
   bucket"), or the DELETE loses its leading bound and scans the table. So
   each one fans out over buckets rather than issuing a single statement:
   `gc_act_trim_blocks` groups its candidate blknums by `page_bucket_id` and
   issues one bucket-pinned DELETE per group (a block's anchor is computed
   within its own bucket, which is where all of that block's rows live, so the
   per-block semantics are unchanged), while `gc_act_drop_fork` and
   `gc_act_shrink_fork` cover a whole fork — length unknown at that point —
   and so loop over all `PAGE_BUCKETS`. Each returns the summed row count.
   `slru_pages` and `meta` need no fan-out: `branch_id` is their whole hash
   key, so binding the branch (which every act does) already bounds the
   statement to one tablet.

   All drops are acted regardless of busy state; the busy-object ones are
   just held back from the **dequeue**, not from the delete.
   All actions land before it — that phase barrier is the
   crash-safety contract.
6. **Dequeue**, strictly after the actions succeed: a candidate leaves
   the queue only once its garbage is gone, and idempotent actions make
   action-before-dequeue crash-safe (a crash in between re-runs the tick
   into a no-op). Two paths, each matching its kind's delete bound:
   - **trim** (`gc_dequeue_trim_candidates`): a block-scoped range delete over
     `(floor, cutoff]`. The tick acted each visited block *to the cutoff*,
     so every candidate `<= cutoff` there is handled — the sweep coalesces
     a hot block's duplicate candidates into one delete. (The insert clamp
     keeps a base landing after the anchor above the cutoff, out of range.)
   - **drop / shrink** (`gc_dequeue_drop_shrink_candidates`): delete by exact
     `(ident, due_lsn)` — one row per acted candidate, no window. They act
     per-candidate at `<= due_lsn`, so only the acted candidate may clear;
     an un-acted sibling (e.g. a capped-out later re-drop) keeps its
     distinct `due_lsn` and stays queued for its own tick.
   - **busy-object drops** (a rel fork or SLRU segment with a materialise
     job in flight): *not* passed to the dequeue. The drop's rows are
     already deleted; leaving the row queued lets a later tick re-act
     it and sweep any image a straggling materialise job inserts (at a
     folded LSN below the drop) after the delete. A dropped object gets no
     new jobs, so the rescan (or the object going idle) converges it. This
     is the one image-leak race the eager delete could open, closed by
     the re-act; shrink and trim don't need it (a late image on a live
     object is reclaimed by that object's next visit).
7. **Advance the floor** (`next_drain_floor`), only now — after act and
   dequeue both succeeded, so a failed tick leaves it and the failed batch
   re-selects. An **uncapped** tick (everything due handled) advances the
   floor to the cutoff. A **capped** tick normally advances only to `max
   selected due_lsn − 1`, since the limit can split that group's ties and the
   −1 keeps them selectable. Unchanged on an empty select. The rows that stay
   queued below the floor — a busy-object drop, a refused candidate,
   a malformed key — don't hold it back; they're swept by a later tick or the
   rescan.

   **Except** when a capped tick dequeued *nothing*: then it advances to `max
   selected due_lsn` instead, stepping over that tie group. Holding at
   `max − 1` only makes progress if the tick cleared something; a tie group of
   `>= gc_drain_limit` rows that all stay queued (every one refused, or every
   one a busy-object drop) would otherwise re-select identically forever — and
   because it fills the limit at the *oldest* `due_lsn`, nothing above it is
   ever reached, stalling the branch's whole queue rather than just those rows.
   Groups do get that large: a DROP DATABASE emits one candidate per rel at the
   drop's LSN (those clear normally — the stall needs rows that don't).

   Stepping over is not a full recovery. Any row at that `due_lsn` the limit
   didn't return is stuck behind the group too, and a floor reset won't reach
   it — the reset re-scans from the head, but the group refills the batch and
   the select has no tiebreaker within a `due_lsn`, so the same subset comes
   back. Those rows wait until the blocking ones are cleared (drained by a
   newer binary, or `DELETE`d). What stepping over buys is that the *rest* of
   the queue keeps draining instead of the whole branch wedging.

The whole drain (select, actions, dequeue) runs on a dedicated
**`yb-pageserver:gc` connection pool** sized to the action width, so a
backlogged drain never contends with the ingest/materialiser lanes or
compute reads. Candidate *inserts* stay on the fpu pool — they need its
upsert mode for replay dedup, and their volume is a rounding error.

## Config

All GC knobs are **per-branch** (tenant config, patchable via
`BranchConfigPatch`). Under the 1 pageserver : 1 branch topology,
per-branch is also per-instance today.

| Knob | Default | Effect |
| --- | --- | --- |
| `enable_gc` | `false` | Master switch — off ⇒ no candidates emitted ⇒ nothing to reclaim. Read once at timeline activation, so a runtime patch applies on the next PS restart. |
| `gc_candidate_every_n_bases` | `10` | Trim-candidate sampling: one per n bases per key. Read once at activation. |
| `gc_drain_limit` | `10 000` | Max candidates a tick selects/acts on; bounds per-tick memory and delete burst. Read fresh each tick. |
| `gc_period` | — | Drain tick interval. |
| `gc_horizon` / `pitr_interval` | — | Retention window feeding the cutoff. |

## Metrics

Prometheus, from `pageserver/src/metrics.rs`. (Listed here rather than in
[`pageserver-metrics.md`](pageserver-metrics.md) so the GC story is
one doc.)

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `pageserver_yb_gc_drain_lag_bytes` | UIntGauge | — | Residual `cutoff − max selected due_lsn` on a budget-capped tick (the due span the tick couldn't reach); 0 when it drained everything due. 0 baseline while keeping up; grows once inserts outrun the drain — **the backlog/starvation alarm**. NB: during a backlog the periodic floor reset reads artificially high for one sample (it scans from the head, so `due.last()` is older); alert on a rate/smoothed value, not a raw sample. |
| `pageserver_yb_gc_applied_cutoff_lsn` | UIntGauge | — | Current GC cutoff (`applied_gc_cutoff_lsn`), per tick. `pageserver_last_record_lsn − this` = retained-history window. Frozen while the queue grows ⇒ cutoff pinned (lease/child/`consistent_lsn`), not a slow drain. |
| `pageserver_yb_gc_candidates_emitted_total` | IntCounter | `path` (`ingest`/`materialiser`/`drop`/`shrink`) | Candidates emitted, by producer path. For trim this counts sampler firings: one per `gc_candidate_every_n_bases` bases per key — but if a key fires more than once within one emitter flush window, those firings fold into a single `pending_gc` row, so inserted rows ≤ emitted. Drop/shrink events unsampled, one row each. `rate()` = emission rate, an upper bound on the insert rate. |
| `pageserver_yb_gc_candidates_read_total` | IntCounter | — | Rows the drain's `pending_gc` select returned per tick (pre-routing). `== gc_drain_limit` ⇒ capped; `≫ removed` ⇒ read but not cleared. |
| `pageserver_yb_gc_candidates_dequeued_total` | IntCounter | — | Rows deleted from `pending_gc` by the dequeue: trim range-sweeps a visited key's whole `(floor, cutoff]` set (collapses duplicates), drop/shrink clear exactly the acted candidates. ≈ `read` at steady state. `rate(emitted) − rate(dequeued)` = queue growth. |
| `pageserver_yb_gc_rows_deleted_total` | IntCounter | — | Rows deleted by the drain across `amp_storage.pages`, `slru_pages`, and `meta` (trim / drop / shrink). `rate()` = reclamation throughput. |
| `pageserver_yb_gc_drain_ticks_total` | IntCounter | `outcome` (`drained`/`capped`/`empty`/`lease_grace`/`disabled`/`error`) | One increment per drain-loop iteration, by how it ended. `rate(capped)/rate(all)` = keep-up alarm; `lease_grace`/`disabled` moving = GC silently gated off; `rate(all)` = tick cadence. |

The drain also owns five `kind` values of
`pageserver_yb_correctness_errors_total` (the rest are in
[`pageserver-metrics.md`](pageserver-metrics.md)). All five mean "the drain
refused to act on a queue row", and **none of them dequeues it**, so
each **re-fires on every floor reset** until the row is fixed, drained by a
newer binary, or `DELETE`d by hand. Read a sustained rate as one stuck row, not
a stream of new ones — the `error!` line carries the row.

| `kind` | Meaning |
| --- | --- |
| `gc_candidate_key_shape_mismatch` | The row's `key` shape doesn't match the keyspace its `gc_kind` acts on (e.g. `DropPages` carrying a rel *block* key, not the fork's rel-size key). The act paths decode the object identity from the key blind, so acting could delete a live object. |
| `gc_shrink_missing_relsize` | A `ShrinkPages` candidate with no stored `relsize` — nothing to truncate at. |
| `gc_shrink_relsize_out_of_range` | A `ShrinkPages` `relsize` that isn't a block number (negative, or `>= u32::MAX`). A negative one would match every block (`blknum >= relsize`) and delete the whole fork. |
| `gc_unknown_candidate_kind` | A `gc_kind` this binary doesn't know, most likely written by a newer one — it reclaims the row on roll-forward. |
| `gc_malformed_key` | The `key` bytea isn't the fixed 18-byte `Key` length. Refused at decode in the select rather than panicking; there's no valid key to dequeue it by either. |

## Troubleshooting

Three checks localize most GC escalations (metrics above):

1. **Is GC running?** `enable_gc` on for the branch and
   `rate(gc_drain_ticks_total) > 0`. `outcome=disabled` climbing ⇒ gated
   off; `lease_grace` ⇒ skipped during fresh-attach grace; `error` ⇒ the
   drain is erroring.
2. **Is the queue keeping up?** `rate(candidates_emitted) −
   rate(candidates_dequeued) > 0` ⇒ the drain is falling behind; confirm
   with `gc_drain_lag_bytes > 0` and rising `outcome=capped`.
3. **Is the cutoff advancing?** `pageserver_last_record_lsn −
   gc_applied_cutoff_lsn` is the retained-history window. Frozen while the
   queue grows ⇒ the cutoff is pinned.

**Storage keeps growing / pages not reclaimed.**
- Drain capped (`gc_drain_lag_bytes > 0`, `capped` ticks) → raise
  `gc_drain_limit` or shorten `gc_period`; the drain has its own pool, so
  a bigger budget won't starve ingest or reads.
- `candidates_emitted` flat → `enable_gc` unset, or
  `gc_candidate_every_n_bases` too high (both read at timeline
  activation — a live change needs a PS restart).
- Candidates drained but `rows_deleted ≈ 0` → the cutoff isn't
  advancing past them; it's pinned (below).

**History window too large / cutoff pinned.** The cutoff is
`min(retention, live lease, child fork point, consistent_lsn)`, floored at
the branch's ancestor LSN. Find the floor:
- **Lease** — a compute holding an lsn lease (long query, stuck
  basebackup, a client that never released) pins it at its read LSN; find
  and clear the holder.
- **Child branch** — every child `branch_state` row pins its parent at
  its fork point, `deleted_at` set or not (a deleted child can still be
  read until branch-deletion GC removes its row — until that flow
  exists, a dropped branch pins its parent indefinitely).
- **`consistent_lsn`** — ingestion lagging (safekeeper/walreceiver behind)
  holds the cutoff back; not a GC problem.
- **Retention** — a large `gc_horizon` / `pitr_interval` is intentional;
  confirm the branch's config.

**Reads erroring during reclamation.** Usually *not* GC over-trim: GC only
deletes at/under the cutoff and waits out in-flight readers (RCU), so a
genuine over-trim shows a needed base/anchor *below* the cutoff missing.
If the failing read's base + records are *above* `gc_applied_cutoff_lsn`,
GC never touched them — look at ingest / walredo. Any
`correctness_errors_total` increment is a corruption signal regardless of
cause.

**Direct probes.**
- `SELECT count(*) FROM amp_storage.pending_gc WHERE branch_id = '…'` —
  queue depth.
- `SELECT gc_kind, amp_storage.gc_key_decode(key), due_lsn FROM
  amp_storage.pending_gc WHERE branch_id = '…'` — the queued candidates,
  decoded.
- `applied_gc_cutoff_lsn` in `amp_storage.branch_state` — the current
  cutoff.
- The drain log line `gc drain complete … candidates_dequeued=…
  rows_deleted=…`.
