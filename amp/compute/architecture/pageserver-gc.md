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

## Branch-drop GC

Everything below reclaims garbage *within a live branch*. Reclaiming a **dropped
branch** — all of its rows, in every table — is a separate flow, owned by the
controller (`controller/internal/branchgc`), and this is the whole of it:

1. The drop **tombstones** the branch (`amp_registry.branches.dropped_at`) and
   writes nothing else. The reaper stamps `reap_after = dropped_at +
   --branch-gc-retention` on the next tick that sees it (default 24 h, matching the
   default `pitr_interval`, so a drop is no more destructive than the retention the
   branch already ran with).
2. Once that deadline passes, the reaper deletes the branch's rows from
   `branch_state`, `pages`, `wals`, `pending_gc`, `blobs`, `slru_pages`, and `meta`
   — skipping the branch entirely if a live child fork or an endpoint still
   references it.
3. It stamps `gc_completed_at`. The branch row itself stays: a branch id is never
   reused, so "reclaimed" has to stay distinguishable from "never existed".

**Why not the drain below.** A dropped branch's pageserver is terminated with the
branch, so a candidate queued into `pending_gc` would never be dequeued — nothing
is left running to drain it. The controller is the only component that outlives the
branch, and `pending_gc` is one of the tables it deletes rather than a queue it
uses.

`branch_state` is deleted **first**, because that row is what pins an ancestor's
cutoff at this branch's fork point (see [Drain](#drain) step 1) — so a dropped
child stops holding its parent's cutoff one retention window after the drop, not
never.

Reaping a large branch is a write flood aimed at the same six `pages` / `wals`
tablets live branches ingest into, so every batch is bounded — a key-only SELECT
that yields `ybctid`s, then one `DELETE ... WHERE ybctid = ANY(...)`, resuming from
a cursor so it never re-walks the tombstones it just wrote — and every tick has a
row budget, so a big branch drains over many ticks.
Cadence, pacing knobs and the operator's triage query:
[`controller/architecture/sc-integration.md`](../../controller/architecture/sc-integration.md#branch-drop-gc)
and [`runbooks/controller_runbook.md`](../../runbooks/controller_runbook.md).
Un-drop/restore is **not** built — the window is a safety margin, not a recovery
window.

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
re-inspected for routing. The eight kinds are `{trim, drop}` ×
`{pages, slru, meta}` plus `shrink` (rel `pages` only) and the two prefix
sweeps, `drop_db` and `drop_rel` (a whole dropped database or relation, both
tables) — the emitter, which
knows the table it wrote, stamps the kind; the key stays pure identity.

| operation | key | recorded when | becomes collectable |
| --- | --- | --- | --- |
| **trim** (`TrimPages`/`TrimSlru`/`TrimMeta`) | the block (rel / SLRU) or meta key | a block's every-n-th new base (`is_image`/`will_init`), or a meta put; `due_lsn` = the newest counted base's LSN | that key's versions below its newest base `<= cutoff` |
| **shrink** (`ShrinkPages`, rel-only) | the rel-size key | a relation is truncated at `due_lsn` | blocks at/above the post-truncation size |
| **drop** (`DropSlru`/`DropMeta`) | the retired object's key | an SLRU segment truncation or a two-phase resolve, at `due_lsn` | all of the object's rows `<= due_lsn` |
| **drop_db** (`DropDb`) | the database's relmap key — its `(spcnode, dbnode)` identity | a `DROP DATABASE` at `due_lsn` | every `pages` row and every `meta` key of that database, `<= due_lsn` |
| **drop_rel** (`DropRel`) | the relation's main-fork size key — its `(spcnode, dbnode, relnode)` identity | a `DROP TABLE` at `due_lsn`, one candidate per relfilenode | every fork's `pages` rows and size row, `<= due_lsn`; the sparse rel-directory tombstones stay |

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
  `drop_twophase_file`, and the one db-wide candidate `drop_dbdir`
  queues (`DROP DATABASE`).

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

Each tick, `Timeline::gc_iteration` (`page_server/timeline/gc.rs`) runs:

1. **Cutoff.** Compute `min(retention window, live leases,
   consistent_lsn)`, floored at the branch's own ancestor LSN. Persist
   `applied_gc_cutoff_lsn = max(old, new)` to `branch_state`
   (monotone), then publish it to the in-memory
   `Rcu<Lsn>`.

   The `consistent_lsn` term is load-bearing for **correctness**, not just
   keep-up. GC deletes history below the cutoff, and the WAL ingest path reads
   below its own apply position with no GC guard — so the cutoff must not pass
   that position. The matching ingest-side rule (records at or below
   `consistent_lsn` are skipped, so ingest never reads under the cutoff) is in
   [pageserver.md](pageserver.md#replay-correctness-rules); dropping either
   half lets a replay read reclaimed history
   ([#588](https://github.com/yugabyte/meko/issues/588)).

   **Child fork points** pin the cutoff too, but they are folded in only
   *inside the persist UPDATE itself*, which clamps to `min(ancestor_lsn)`
   over every child row still present. A dropped child can still be read
   until branch-drop GC cleans it up (for instance, to restore it
   within the PITR interval); removing its `branch_state` row is what
   releases the pin, and that is the FIRST thing the controller's reaper
   deletes (`controller/internal/branchgc`, see
   [controller/architecture/sc-integration.md](../../controller/architecture/sc-integration.md#branch-drop-gc))
   — so a parent's cutoff starts moving again one retention window after
   the child is dropped, not never.
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
   views of each other. Whichever side loses aborts with a serialization
   failure, and the controller's task worker re-runs the step on it
   (bounded, jittered) rather than failing the operation: nothing landed,
   so the re-run either wins or reads the winner's effect and rejects for
   a real reason. GC is not the only other writer of these rows — another
   fork off the ancestor, a drop of it, and an endpoint being created on it
   all contend here too — so the retry is what lets any of those proceed
   concurrently rather than failing each other.

   The same shape closes the fork ↔ **drop** race, one row over. A drop
   tombstones the parent's `amp_registry.branches` row while checking
   `NOT EXISTS (live children)`, and the fork's liveness check was a read
   of that row — so both could commit and leave a tombstoned parent with
   a live child, whose pre-fork history lives under the *parent's*
   `branch_id` and would then be reclaimed out from under a live branch.
   So `RecordBranch`'s liveness check is a conditional write of that row too
   (`WHERE dropped_at IS NULL`, self-assignment SET), which is what makes the
   two conflict. A row lock serializes it too under read committed, but the drop
   would then need `FOR NO KEY UPDATE` as a statement of its own ahead of its
   guarded `UPDATE` — one extra round trip, where the fork side is a wash
   (`FOR SHARE` in place of the write). And under repeatable read the drop would
   not see the newly inserted fork at all: its read time is fixed when it asks for
   the lock, before the fork commits.
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
   (`TrimPages`) or rel-size sentinel (`ShrinkPages` / `DropRel`), SLRU
   block (`TrimSlru`) or segment-size sentinel (`DropSlru`), any `meta`-table
   key (`TrimMeta`), the relmap key (`DropDb` — the act takes only
   `(spcnode, dbnode)` from the key and sweeps that whole database, so a
   rel-size key would name a *live* database and delete every row of it). A
   shrink additionally needs a `relsize` in `BlockNumber`
   range — `gc_act_shrink_fork` deletes `blknum >= relsize`, so a negative one
   would match every block and delete the whole fork.

   `DropMeta` is the one kind with an **allowlist** rather than a keyspace
   test, because trim keeps one version and drop keeps none: `gc_act_trim_meta`
   keeps the newest version `<= cutoff`, while `gc_act_drop_meta` deletes
   `lsn <= due_lsn` outright. A drop is therefore only correct on a key whose
   object is *dead*, and nearly every `meta` key is live state for something —
   a size sentinel (owned by `DropRel`/`DropSlru`, which delete it with the
   object), a dbdir, the twophase *directory* (whose key is
   `twophase_file_key(0)`). So `DropMeta` accepts a fixed allowlist, currently
   the one shape it emits — a twophase file — and adding an emitter means adding
   its shape. Every accepted shape is blast radius for a mis-emitted candidate,
   so the list holds nothing speculative: the relmap and sparse-reldir shapes
   went with the per-fork `DROP DATABASE` emitter (one `DropDb` candidate now
   sweeps those keys by range), and the rel-dir shape with the v1 keyspace.
   `TrimMeta` needs no
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
     LSN, so a later reuse — wrapped-around segno, recreated relfilenode or
     database — keeps its newer rows): an SLRU segment (`gc_act_drop_slru`:
     `slru_pages` + the segment-size meta key), or dead meta keys
     (`gc_act_drop_meta`). A dropped relation is a prefix sweep, below.
   - **drop_db / drop_rel** (`gc_act_sweep`) — a whole dropped database or
     relation, as a range sweep rather than a per-fork fan-out. The two differ
     only in how many key columns are bound (`GcSweepScope`): a relation binds
     `relnode` as well, so its forks are one contiguous `pages` range per
     bucket just as a database's relations are, and it sweeps one `meta` range
     (the forks' size rows) rather than two. A relation's sparse rel-directory
     entries are NOT swept — a `Removed` marker shadows an ancestor's `Exists`
     under copy-on-write, so only a `DROP DATABASE`, which removes the DbDir
     entry and shadows the whole range from readers, may delete those. The key layout is what makes that a
     seek: `(branch_id, spcnode, dbnode)` is a `pages` key prefix under a bound
     `bucket_id`, and a rel key encodes spcnode/dbnode ahead of relnode, so the
     database is one contiguous range per bucket and per `meta` prefix — six
     seeks plus two, against six *per fork* before. Sweeping `meta` by range
     rather than by list is also what clears the sparse-reldir tombstones
     earlier `DROP TABLE`s left.

     A database is unbounded in size, so this is the first act to be
     **budgeted and resumable**: `gc_sweep_rows_per_tick` caps the rows a
     tick deletes, and each batch is a key-only SELECT yielding `ybctid`s then
     one `DELETE ... WHERE ybctid = ANY(...)`, resuming from an in-memory
     cursor (`GcDrainProgress::sweep_cursors`) so a batch never re-walks the
     tombstones the last one wrote — the controller's branch-drop reaper in
     miniature. Losing the cursors to a restart costs a re-scan of those
     tombstones, not a re-delete. A candidate whose range still holds rows is
     not dequeued, and resumes on the next tick from memory rather than at the
     floor reset (below). (`ShrinkPages` is *not* budgeted — one unbounded DELETE per bucket over the
     truncated suffix — and cannot be coalesced with its siblings either, since
     each fork carries its own truncation length.)

     `pages` is swept **round-robin over the buckets**, one batch each per
     pass, because `bucket_id` leads that table's key and pre-splits it into
     six tablets: a bucket at a time would aim a whole tick's deletes at one
     Raft group — one live ingest is writing to, for every branch — while the
     other five idle. The batch is *derived*, a sixth of the budget and capped
     at `GC_SWEEP_MAX_ROWS_PER_DELETE`, so one pass is a tick's work and
     pacing stays the only knob. Passes repeat until the budget is spent or
     every bucket reports empty, so a database whose rows all land in one
     bucket still spends its whole budget. The `meta` ranges stay sequential —
     a branch's `meta` is one tablet, so there is nothing to spread.
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
     default 1, scaled) — distinct objects are distinct keys / tablets,
     no ordering constraint.

   Every act on `pages` must bind `bucket_id`, the leading column of that
   table's all-range key (see [`pageserver.md`](pageserver.md) "Why the
   bucket"), or the DELETE loses its leading bound and scans the table. So
   each one fans out over buckets rather than issuing a single statement:
   `gc_act_trim_blocks` groups its candidate blknums by `page_bucket_id` and
   issues one bucket-pinned DELETE per group (a block's anchor is computed
   within its own bucket, which is where all of that block's rows live, so the
   per-block semantics are unchanged), while `gc_act_shrink_fork` covers a
   fork's whole truncated suffix — length unknown at that point — and so loops
   over all `PAGE_BUCKETS`. Each returns the summed row count.
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
     job in flight; for a `DropDb`, *any* fork of the database): *not*
     passed to the dequeue. The drop's rows are
     already deleted; leaving the row queued lets a later tick re-act
     it and sweep any image a straggling materialise job inserts (at a
     folded LSN below the drop) after the delete. A dropped object gets no
     new jobs, so the rescan (or the object going idle) converges it. This
     is the one image-leak race the eager delete could open, closed by
     the re-act; shrink and trim don't need it (a late image on a live
     object is reclaimed by that object's next visit).
   - **budget-capped sweeps** (a `drop_db` whose range still holds rows): also
     left queued, but re-found from memory rather than by the select — see
     [Resuming a capped sweep](#resuming-a-capped-sweep).
7. **Advance the floor** (`next_drain_floor`), only now — after act and
   dequeue both succeeded, so a failed tick leaves it and the failed batch
   re-selects. An **uncapped** tick (everything due handled) advances the
   floor to the cutoff. A **capped** tick normally advances only to `max
   selected due_lsn − 1`, since the limit can split that group's ties and the
   −1 keeps them selectable. Unchanged on an empty select. The rows that stay
   queued below the floor — a busy-object drop, a refused candidate,
   a malformed key, a capped sweep — don't hold it back; they're swept by a
   later tick or the rescan.

   **Except** when a capped tick dequeued *nothing*: then it advances to `max
   selected due_lsn` instead, stepping over that tie group. Holding at
   `max − 1` only makes progress if the tick cleared something; a tie group of
   `>= gc_drain_limit` rows that all stay queued (every one refused, or every
   one a busy-object drop) would otherwise re-select identically forever — and
   because it fills the limit at the *oldest* `due_lsn`, nothing above it is
   ever reached, stalling the branch's whole queue rather than just those rows.
   Groups do get that large: the emitter flushes up to 500 keys at one
   `due_lsn` (those clear normally — the stall needs rows that don't).

   Stepping over is not a full recovery. Any row at that `due_lsn` the limit
   didn't return is stuck behind the group too, and a floor reset won't reach
   it — the reset re-scans from the head, but the group refills the batch and
   the select has no tiebreaker within a `due_lsn`, so the same subset comes
   back. Those rows wait until the blocking ones are cleared (drained by a
   newer binary, or `DELETE`d). What stepping over buys is that the *rest* of
   the queue keeps draining instead of the whole branch wedging.

### Resuming a capped sweep

A `drop_db` whose range outlasts the tick's row budget stays queued, and the
NEXT tick resumes it — but not by re-selecting it. The tick reads the
unfinished candidates straight out of `GcDrainProgress::sweep_cursors` and adds
them to the rows the select returned (`Timeline::with_resuming`), so the floor
advances to the cutoff as usual.

Making the *select* re-find it instead — holding the floor just below its
`due_lsn` — is the obvious design and it degrades: the candidate's `due_lsn` is
frozen at the drop while the cutoff keeps advancing, so the window widens for
the whole sweep and every trim candidate the branch dequeues inside it leaves a
tombstone exactly where the next tick's scan starts. A 100 GB database is
~427 ticks; at 500 candidates/s of ordinary emission that is ~1M rows dequeued
inside the pinned window, re-walked every 5 s. The floor exists to skip
precisely those tombstones.

Nothing is lost when the cursors are: they are in-memory only, and a restart
resets the floor to 0, so the first tick after one re-selects the row from the
queue head and starts the sweep over. A candidate the select *also* returns (a
floor-reset tick scans from the head) is deduped by
`(key, gc_kind, due_lsn)` — acting twice would re-issue the sweep, and
dequeueing twice would delete a row already gone.

**Only a candidate with ranges left resumes this way.** A busy-object drop is
also left queued, but it waits for the floor reset: it depends on an external
event with no progress guarantee, so retrying it every tick would re-act a
wedged object forever, and — because an injected candidate keeps the tick
reporting `capped` — would hold the whole branch's drain at the backlog cadence
for as long as the job stays wedged. A capped sweep is the opposite: guaranteed
to advance every tick.

### The re-check pass

A sweep resumes from a cursor that only moves forward, so the usual protection
for a busy drop — leave it queued, re-act it later, and catch whatever the
straggling materialise job inserted — does not work: re-acting resumes *past*
the straggler. The unbudgeted acts have no such hole, because each visit deletes
the whole object again.

So a tick that finds any fork inside the sweep's scope awaiting a materialise
job sets `needs_recheck` on its cursor, and `is_reclaimed` — the dequeue's gate —
stays false while it is set. On reaching the end of its ranges the sweep rewinds
once and clears the flag, owing exactly one more full pass. Rewinding when the
flag is *set* would instead discard a long sweep every time a fork happened to be
busy; the pass itself is one seek per `pages` bucket and per `meta` range over
ranges that are already empty, since the deletes do not repeat.

That "any fork inside the scope" test is a lookup, not a scan:
`PendingMaterialise` indexes the tick's in-flight jobs by `(spcnode, dbnode)` and
`(spcnode, dbnode, relnode)`, which is what a scope's key range admits — a
relation drop is the common case, so scanning the whole in-flight set per
candidate would be quadratic in a `DROP SCHEMA`.

Without it the leak is silent and permanent: busy at one tick, idle at the tick
that finishes, dequeued with an image stranded behind the cursors — and a dropped
object's keys are never named by a future record, so nothing would ever emit a
candidate for them again.

The whole drain (select, actions, dequeue) runs on a dedicated
**`pageserver:gc` connection pool** sized to the action width, so a
backlogged drain never contends with the ingest/materialiser lanes or
compute reads. Candidate *inserts* take whichever lane `hot_path` selects,
alongside the page writes they trail: under the default they are
PGSQL_UPSERT batches on the direct-RPC lane
(`gc_enqueue_candidates_thin`), and under `hot_path = "ysql"` they run on
the fpu pool, whose upsert mode gives them the same replay dedup. Either
way their volume is a rounding error next to the page writes. The drain
itself has no thin lane — select, actions and dequeue are YSQL only.

`bucket_id` is why the lane split is safe here: it is placement only and
**writer-computed** (`page_store::gc_bucket_id`), deliberately not a
generated column, so both lanes derive it identically from the key.
Unlike `pages`, `pending_gc` carries no bucket CHECK constraint, so the
direct-RPC lane bypasses no enforcement. A lane that bucketed differently
would write rows the drain's per-bucket fan-out never looks at — an
invisible candidate, not a lost page — which is what
`thin_lane_gc_enqueue_matches_ysql_drain` pins down by enqueuing on the
lane and draining over YSQL.

### Tick cadence

`gc_period` is the base, not the period: the loop (`gc_loop`,
`page_server/tasks.rs`) picks the next sleep from the queue state the tick
left (`GcResult::outcome`, whose `label()` is also the
`gc_drain_ticks_total` label), so an idle branch stops paying a full
per-bucket fan-out every 30s and a backlogged one stops being pinned at
`gc_drain_limit` per 30s.

| Queue the tick left (`gc_drain_ticks_total` label) | Next sleep |
| --- | --- |
| Caught up — everything due was acted (`drained`) | `gc_period` (30s) |
| Backlog remains — `gc_drain_limit` hit, or a sweep stopped on `gc_sweep_rows_per_tick` (`capped`) | `gc_period / 6` (5s), never under 1s |
| Nothing due (`empty`) | double the previous sleep, up to `8 x gc_period` (4min) |

The first working tick resets to the base. A tick that reached
`gc_iteration` and skipped the drain there (`test_enable_gc_drain` off, lease
grace) backs off like an idle one; the loop-level disable (`gc_period == 0` or
`gc_horizon == 0`) instead keeps its own fixed 10 s re-check, so the
`outcome=disabled` label covers two cadences — 10 s if GC is switched off, up
to 4 min if only the drain is. Errors keep
their own exponential retry. The backoff state lives in the loop and the first
tick still jitters (`sleep_random`), so a fleet restarted together doesn't
resynchronise its selects on the shared `pending_gc` tablets.

A candidate that comes due inside a backed-off window waits up to the ceiling
before it is reclaimed — GC is throughput-bound, not latency-bound. The other
end of that trade: the drain's sustained ceiling is `gc_drain_limit` (or
`gc_sweep_rows_per_tick`) per *floor*, so a backlog drains 6x faster than the
base cadence suggests.

## Config

All GC knobs are **per-branch** (tenant config, patchable via
`BranchConfigPatch`). Under the 1 pageserver : 1 branch topology,
per-branch is also per-instance today.

| Knob | Default | Effect |
| --- | --- | --- |
| `enable_gc` | `false` | Master switch — off ⇒ no candidates emitted ⇒ nothing to reclaim. Read once at timeline activation, so a runtime patch applies on the next PS restart. |
| `gc_candidate_every_n_bases` | `10` | Trim-candidate sampling: one per n bases per key. Read once at activation. |
| `gc_drain_limit` | `10 000` | Max candidates a tick selects/acts on; bounds per-tick memory and delete burst. Read fresh each tick. |
| `gc_sweep_rows_per_tick` | `30 000` | Rows a tick deletes across its prefix sweeps, shared by all of them. `gc_drain_limit` counts candidates, and a dropped database or relation is one, so this is what bounds their burst; a larger object drains over many ticks. The default is `PAGE_BUCKETS x GC_SWEEP_MAX_ROWS_PER_DELETE`, so one tick is one pass of one bounded DELETE per bucket (~6 000 rows/s at the backlog cadence). Read fresh each tick. |
| `gc_period` | 30 s | **Base** drain tick interval; the loop adapts around it (5 s floor / 4 min ceiling — see [Tick cadence](#tick-cadence)). `0` disables GC. |
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
| `pageserver_yb_gc_drain_ticks_total` | IntCounter | `outcome` (`drained`/`capped`/`empty`/`lease_grace`/`disabled`/`error`) | One increment per drain-loop iteration, by how it ended. `rate(capped)/rate(all)` = keep-up alarm; `lease_grace`/`disabled` moving = GC silently gated off; `rate(all)` = tick cadence — which is adaptive, so a low rate is idle backoff, not necessarily a stall. |

The drain also owns five `kind` values of
`pageserver_yb_correctness_errors_total` (the rest are in
[`pageserver-metrics.md`](pageserver-metrics.md)). All five mean "the drain
refused to act on a queue row", and **none of them dequeues it**, so
each **re-fires on every floor reset** until the row is fixed, drained by a
newer binary, or `DELETE`d by hand. Read a sustained rate as one stuck row, not
a stream of new ones — the `error!` line carries the row.

| `kind` | Meaning |
| --- | --- |
| `gc_candidate_key_shape_mismatch` | The row's `key` shape doesn't match the keyspace its `gc_kind` acts on (e.g. `DropRel` carrying a rel *block* key, not the relation's rel-size key). The act paths decode the object identity from the key blind, so acting could delete a live object. |
| `gc_shrink_missing_relsize` | A `ShrinkPages` candidate with no stored `relsize` — nothing to truncate at. |
| `gc_shrink_relsize_out_of_range` | A `ShrinkPages` `relsize` that isn't a block number (negative, or `>= u32::MAX`). A negative one would match every block (`blknum >= relsize`) and delete the whole fork. |
| `gc_unknown_candidate_kind` | A `gc_kind` this binary doesn't know, most likely written by a newer one — it reclaims the row on roll-forward. |
| `gc_malformed_key` | The `key` bytea isn't the fixed 18-byte `Key` length. Refused at decode in the select rather than panicking; there's no valid key to dequeue it by either. |

## Troubleshooting

Three checks localize most GC escalations (metrics above):

1. **Is GC running?** `enable_gc` on for the branch and
   `rate(gc_drain_ticks_total) > 0`. `outcome=disabled` climbing ⇒ gated
   off; `lease_grace` ⇒ skipped during fresh-attach grace; `error` ⇒ the
   drain is erroring. A *low* tick rate is not by itself a stall: a branch
   with nothing due backs off to one tick per 4 min, so read the rate together
   with `outcome=empty` climbing.
2. **Is the queue keeping up?** `rate(candidates_emitted) −
   rate(candidates_dequeued) > 0` ⇒ the drain is falling behind; confirm
   with `gc_drain_lag_bytes > 0` and rising `outcome=capped`.
3. **Is the cutoff advancing?** `pageserver_last_record_lsn −
   gc_applied_cutoff_lsn` is the retained-history window. Frozen while the
   queue grows ⇒ the cutoff is pinned. Expect `outcome=empty` ticks at the
   4 min ceiling while it is: nothing below a pinned cutoff is due, so the
   drain idles however much the branch churns, and reclaim resumes one tick
   after the pin lifts.

**Storage keeps growing / pages not reclaimed.**
- Drain capped (`gc_drain_lag_bytes > 0`, `capped` ticks) → the loop is
  already ticking at the 5 s floor; raise `gc_drain_limit` (or shorten
  `gc_period`, which lowers the floor with it, down to a 1 s clamp). The drain
  has its own pool, so a bigger budget won't starve ingest or reads.
- `candidates_emitted` flat → `enable_gc` unset, or
  `gc_candidate_every_n_bases` too high (both read at timeline
  activation — a live change needs a PS restart).
- Candidates drained but `rows_deleted ≈ 0` → the cutoff isn't
  advancing past them; it's pinned (below).
- A dropped database or relation still holding space → its sweep is paced by
  `gc_sweep_rows_per_tick`; raise the knob to reclaim faster. A repeated
  `gc: sweep owes a re-check pass` for the same object instead means a
  materialise job is wedged, and the sweep is waiting for a floor reset.

**History window too large / cutoff pinned.** The cutoff is
`min(retention, live lease, child fork point, consistent_lsn)`, floored at
the branch's ancestor LSN. Find the floor:
- **Lease** — a compute holding an lsn lease (long query, stuck
  basebackup, a client that never released) pins it at its read LSN; find
  and clear the holder.
- **Child branch** — every child `branch_state` row pins its parent at
  its fork point. The pin is the row's presence, so a dropped child keeps
  pinning its parent until the controller's branch-drop reaper deletes the
  row, one `--branch-gc-retention` window later; past that, check the
  reaper is running and not skipping the branch.
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
