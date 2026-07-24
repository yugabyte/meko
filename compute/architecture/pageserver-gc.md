# Pageserver GC

The pageserver stores each page version as its own row in
`pageserver.pages` (schema: [`yugabyte.md`](yugabyte.md)). GC reclaims the
rows no read can reach any more — superseded page versions, blocks past a
truncation, and the pages of a dropped relfilenode — by `DELETE`ing them.
It is pure space reclamation: it never changes what a correct read
returns.

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
truncation, a relfilenode drop — as a row in `pageserver.pending_gc` (a
sibling queue table), tagged with the LSN at which that event's garbage
becomes collectable. A periodic **drain** then acts on each record once
the GC cutoff has advanced past that LSN. A queue row's `blknum` column
doubles as the candidate **kind**:

| `blknum` | kind | recorded when | becomes collectable |
| --- | --- | --- | --- |
| `>= 0` | **trim** | a block's every-n-th new base (`is_image`/`will_init`); `due_lsn` = the newest counted base's LSN | that block's versions below its newest base, at/under the cutoff |
| `-1` | **shrink** | a relation is truncated at `due_lsn` | blocks at/above the post-truncation size |
| `-2` | **drop** | a relfilenode is dropped (`DROP TABLE`/index, or every rel of a `DROP DATABASE`) at `due_lsn` | all of the fork's pages `<= due_lsn` |

`due_lsn` is the LSN at which the garbage became collectable; a candidate
is actionable only once the cutoff reaches its `due_lsn`.

## Candidate emission

Candidates split by **durability requirement**, not by producer:

- **Trim candidates** (self-healing): both producers — the ingest
  window flush (each `will_init` row it lands) and the materialiser
  (one per rel image it writes) — feed one shared in-RAM buffer on the
  `PageWriter` (`GcTrimCandidateEmitter`, `pageserver/src/gc_trim_candidates.rs`),
  flushed by its own loop at 500
  distinct blocks or 5 s, whichever first (same-block candidates fold
  to the latest `due_lsn`). **Best-effort by design**: a crash or
  failed flush loses at most one buffer, healed by the block's next
  base — trims are cumulative, so the next candidate's greedy-to-cutoff
  trim re-covers the lost span. Only a block whose final-ever base fell
  in a lost buffer leaks (bounded; reclaimable by a future backstop
  sweep). The flusher shuts down after both producers stop, with a
  final flush, so a clean shutdown loses nothing.
- **Shrink and drop events** (one-shot — a dropped fork gets no future
  base to re-emit a candidate) stay **window-durable** on the ingest
  path: buffered alongside the window's rows and flushed with them, so
  a buffer lost to a crash implies `consistent_lsn` didn't advance and
  the producing record (`put_rel_truncation`, `put_rel_drops`, per-rel
  under `drop_dbdir` for `DROP DATABASE`) replays and re-emits.

Best-effort is safe for trim because no single trim candidate is
load-bearing (the block's next base heals it), and both producers
record a candidate only after its base is durable — candidate flushed
⇒ base flushed already.
Window durability wasn't buying ingest trim a re-emission guarantee
anyway: the sampling counter below is in-memory and resets on restart,
so a replayed window may emit at a different base or not at all.

`gc_candidate_every_n_bases` (default 10) samples both trim producers
through one shared per-block counter: one trim candidate per n bases per
block, stamped `due_lsn = max LSN among its n counted bases`. The
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
sweep by block-range instead of naming exact PKs (see Drain step 5): a
base landing after a tick's trim anchor is clamped above the cutoff, so
the `<= cutoff` sweep only removes a candidate whose garbage the trim
didn't collect in the rare clamp race (above), where the block's next
base re-covers it.

## Drain

Each `gc_period` tick, `Timeline::gc_iteration` (`page_server/timeline/gc.rs`)
runs:

1. **Cutoff.** Compute `min(retention window, live child fork points,
   live leases, consistent_lsn)`, floored at the branch's own ancestor
   LSN. Persist `applied_gc_cutoff_lsn = max(old, new)` to
   `timeline_state` (generation-fenced, monotone), then publish it to the
   in-memory `Rcu<Lsn>`.
2. **Await readers.** Wait out in-flight reads still holding a guard on
   the *old* cutoff before deleting anything they might need. Reads never
   wait for GC; GC waits for reads.
3. **Select.** `select_due_pending_gc`: up to `gc_drain_limit`
   (default 10 000) candidates with
   `floor < due_lsn <= cutoff`, oldest-first, one bounded range scan per
   placement bucket. The **floor** (`GcDrainProgress::floor`) marks how
   far the drain has progressed: every candidate at or below it has been
   handled — dequeued, acted-but-requeued (a busy-fork drop), or parked
   as unknown-kind — so the scan seeks past those rows' YB delete
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
   floor un-dequeued: a busy-fork drop still waiting for its fork to go
   idle, and a rare clamp-race trim straggler (a candidate the `due_lsn`
   clamp couldn't lift above a concurrently advancing cutoff). Without
   it, on a pageserver that never restarts those would linger
   indefinitely; the periodic reset bounds that to the interval. It does
   **not** catch a leak with *no* queue row (e.g. an emitter candidate buffer
   lost to a crash before flushing); that class waits on a future
   full rel-directory sweep (see the design doc's full-sweep alternative).
4. **Act**, ordered **drop → shrink → trim** (coarser first — a dropped
   fork moots its own shrink/trim candidates):
   - **drop** (`gc_drop_fork`): delete the fork's `pages` rows
     `<= due_lsn` (bounded by the drop LSN, so a later reuse of the
     relfilenode keeps its newer pages) plus the fork's rel-size meta key.
   - **shrink** (`gc_shrink_fork`): read the rel's size as of the cutoff,
     delete `pages` with `blknum >= size AND lsn <= cutoff`
     (re-extension-safe).
   - **trim** (`gc_trim_blocks`): per block keep the newest base
     `<= cutoff` (the anchor) and everything above; delete below. Trims
     across forks run in parallel (`GC_ACTION_PARALLELISM`, default 2,
     scaled) — distinct forks are distinct `pages` hash keys / tablets,
     no ordering constraint.

   All drops are acted regardless of fork state; the busy-fork ones are
   just held back from the **dequeue** (step 5), not from the delete.
   All actions land before step 5 — that phase barrier is the
   crash-safety contract.
5. **Dequeue**, strictly after the actions succeed: a candidate leaves
   the queue only once its garbage is gone, and idempotent actions make
   action-before-dequeue crash-safe (a crash in between re-runs the tick
   into a no-op). One block-scoped range delete (`dequeue_pending_gc`)
   handles every kind: for each visited block — a trim block, or a
   shrink (-1) / drop (-2) sentinel — it deletes that block's candidates
   `<= cutoff`. Cleaner than naming exact PKs, and safe for the same
   reason across kinds: the tick acted each visited block to the cutoff,
   so every candidate `<= cutoff` there is already handled, and the
   insert clamp keeps anything that must outlive the tick (a not-yet-due
   re-drop, a base landing after a trim anchor) above the cutoff and out
   of range. Keyed on exact blknum, so the kinds never sweep each other.
   - **busy-fork drops**: *not* passed to the dequeue. The drop's pages
     are already deleted; leaving the row queued lets a later tick re-act
     it and sweep any image a straggling materialise job inserts (at a
     folded LSN below the drop) after the delete. A dropped fork gets no
     new jobs, so the rescan (or the fork going idle) converges it. This
     is the one image-leak race the eager delete could open, closed by
     the re-act; shrink and trim don't need it (a late image on a live
     fork is reclaimed by that block/fork's next visit).
6. **Advance the floor**, only now — after act and dequeue both
   succeeded, so a failed tick leaves it and the failed batch
   re-selects. An **uncapped** tick (everything due handled) advances
   the floor to the cutoff; a **capped** tick only to `last selected
   due_lsn − 1`, since the limit can split that group's ties and the −1
   keeps them selectable. Unchanged on an empty select. A busy-fork drop
   and an unknown-kind row (alarmed via `correctness_errors_total`) don't
   hold the floor back — both stay on disk below it, swept by a later
   tick or the rescan.

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
| `gc_candidate_every_n_bases` | `10` | Trim-candidate sampling: one per n bases per block. Read once at activation. |
| `gc_drain_limit` | `10 000` | Max candidates a tick selects/acts on; bounds per-tick memory and delete burst. Read fresh each tick. |
| `gc_period` | — | Drain tick interval. |
| `gc_horizon` / `pitr_interval` | — | Retention window feeding the cutoff. |

## Metrics

Prometheus, from `pageserver/src/metrics.rs`. (Listed here rather than in
[`pageserver-metrics.md`](pageserver-metrics.md) so the GC story is
one doc.)

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `pageserver_yb_gc_drain_lag_bytes` | UIntGauge | — | Residual `cutoff − due.last().due_lsn` on a budget-capped tick (the due span the tick couldn't reach); 0 when it drained everything due. 0 baseline while keeping up; grows once inserts outrun the drain — **the backlog/starvation alarm**. NB: during a backlog the periodic floor reset reads artificially high for one sample (it scans from the head, so `due.last()` is older); alert on a rate/smoothed value, not a raw sample. |
| `pageserver_yb_gc_applied_cutoff_lsn` | UIntGauge | — | Current GC cutoff (`applied_gc_cutoff_lsn`), per tick. `pageserver_last_record_lsn − this` = retained-history window. Frozen while the queue grows ⇒ cutoff pinned (lease/child/`consistent_lsn`), not a slow drain. |
| `pageserver_yb_gc_candidates_emitted_total` | IntCounter | `path` (`ingest`/`materialiser`/`drop`/`shrink`) | Candidates emitted, by producer path. For trim this counts sampler firings: one per `gc_candidate_every_n_bases` bases per block — but if a block fires more than once within one emitter flush window, those firings fold into a single `pending_gc` row, so inserted rows ≤ emitted. Drop/shrink events unsampled, one row each. `rate()` = emission rate, an upper bound on the insert rate. |
| `pageserver_yb_gc_candidates_read_total` | IntCounter | — | Rows the drain's `pending_gc` select returned per tick (pre-routing). `== gc_drain_limit` ⇒ capped; `≫ removed` ⇒ read but not cleared. |
| `pageserver_yb_gc_candidates_dequeued_total` | IntCounter | — | Rows deleted from `pending_gc` by the dequeue: each visited block's whole `(floor, cutoff]` candidate set, so ≥ the tick's acted rows (collapses a block's duplicates/stragglers), ≈ `read` at steady state. `rate(emitted) − rate(dequeued)` = queue growth. |
| `pageserver_yb_gc_pages_rows_deleted_total` | IntCounter | — | `pageserver.pages` rows deleted by the drain (trim / drop / shrink). `rate()` = reclamation throughput. |
| `pageserver_yb_gc_drain_ticks_total` | IntCounter | `outcome` (`drained`/`capped`/`empty`/`lease_grace`/`disabled`/`error`) | One increment per drain-loop iteration, by how it ended. `rate(capped)/rate(all)` = keep-up alarm; `lease_grace`/`disabled` moving = GC silently gated off; `rate(all)` = tick cadence. |

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
- Candidates drained but `pages_rows_deleted ≈ 0` → the cutoff isn't
  advancing past them; it's pinned (below).

**History window too large / cutoff pinned.** The cutoff is
`min(retention, live lease, child fork point, consistent_lsn)`, floored at
the branch's ancestor LSN. Find the floor:
- **Lease** — a compute holding an lsn lease (long query, stuck
  basebackup, a client that never released) pins it at its read LSN; find
  and clear the holder.
- **Child branch** — a live child pins its parent at the fork point. NB: a
  *dropped* child still pins today (its `timeline_state` isn't marked
  deleted on drop), so a stale dropped branch leaks a parent pin.
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
- `SELECT count(*) FROM pageserver.pending_gc WHERE branch_id = '…'` —
  queue depth.
- `applied_gc_cutoff_lsn` in `pageserver.timeline_state` — the current
  cutoff.
- The drain log line `gc drain complete … candidates_dequeued=…
  pages_rows_deleted=…`.
