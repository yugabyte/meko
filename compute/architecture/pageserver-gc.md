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
| `>= 0` | **trim** | a block gets a new base (`is_image`/`will_init`) at `due_lsn` | that block's versions below its newest base, at/under the cutoff |
| `-1` | **shrink** | a relation is truncated at `due_lsn` | blocks at/above the post-truncation size |
| `-2` | **drop** | a relfilenode is dropped (`DROP TABLE`/index, or every rel of a `DROP DATABASE`) at `due_lsn` | all of the fork's pages `<= due_lsn` |

`due_lsn` is the LSN at which the garbage became collectable; a candidate
is actionable only once the cutoff reaches its `due_lsn`.

## Candidate emission

Two writers record **trim** candidates:

- **Ingest** (`PageWriter` flush): a `will_init` delta buffers a
  candidate, flushed durably in the same window as the rows that
  produced it. A lost candidate implies `consistent_lsn` didn't advance,
  so its window (base included) replays — harmless, and trims are
  cumulative so the block's next candidate re-covers the span.
- **Materialiser**: records a candidate for each rel image it writes,
  into an in-RAM buffer (`PendingGcBuffer`) flushed at 500 distinct
  blocks or 5 s, whichever first (same-block candidates fold to the
  latest `due_lsn`). **Best-effort by design**: a crash or failed flush
  loses at most one buffer, healed by the block's next materialisation.
  Only a block whose final-ever base fell in a lost buffer leaks
  (bounded; reclaimable by a future backstop sweep).

`gc_candidate_every_n_bases` (default 10) samples both paths through one
shared per-block counter: one trim candidate per n bases per block. `n>1`
trades queue volume for up to `n−1` bases of garbage retained on a block
that goes cold mid-cycle (the next candidate's trim still collapses the
skipped cycles). **Sampling is trim-only** — shrink and drop are discrete
one-shot events and are always recorded, at the truncation / drop that
causes them (`put_rel_truncation`, `put_rel_drops`, and per-rel under
`drop_dbdir` for `DROP DATABASE`). These events ride the ingest
writer's candidate buffer, so they share its durability contract: flushed
with the window, and a buffer lost to a crash implies `consistent_lsn`
didn't advance, so the producing record replays and re-emits.

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
   (default 10 000) candidates with `due_lsn <= cutoff`, oldest-first,
   one bounded range scan per placement bucket.
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

   Drop first consults the writer's pending-materialise set (jobs
   queued or in flight, plus threshold-crossed keys awaiting dispatch):
   a drop event whose fork appears there is **deferred** — left queued
   for a later tick — because a pending job back-dates its image INSERT
   to the folded delta's LSN, so it could re-insert one folded image
   per straggling block right after the DELETE — rows no future
   candidate revisits on a dropped fork. A dropped fork gets no new
   jobs or tallies, so deferral converges within a tick or two.

   The same race exists for shrink, but there it is left unhandled:
   the leaked image is bounded (at most one per tail block with a job
   in flight at the shrink) and invisible to reads, and any subsequent
   truncation, drop, or re-extension deletes it. Deferring shrinks
   would instead starve a hot fork's tail reclaim, since steady churn
   keeps the fork in the pending-materialise set. Trim needs no
   deferral: a late image is an ordinary base on a live block — the
   block's next trim visit anchors on it or deletes it.

   All actions land before step 5 — that phase barrier is the
   crash-safety contract.
5. **Dequeue**, strictly after the actions succeed: a candidate leaves
   the queue only once its garbage is gone, and idempotent actions make
   action-before-dequeue crash-safe without a cross-table transaction (a
   crash in between re-runs the tick into a no-op). Trim rows dequeue
   **collapse-on-visit** — a visited block's other trim candidates
   `<= cutoff` are already-covered and go too (an under-cap tick sweeps
   `due_lsn <= cutoff AND blknum >= 0` per bucket). Drop/shrink events
   dequeue by **exact PK**, never by cutoff range — a drop only covers
   `<= its due_lsn`.

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
| `pageserver_yb_gc_drain_lag_bytes` | UIntGauge | — | Residual `cutoff − due.last().due_lsn` on a budget-capped tick (the due span the tick couldn't reach); 0 when it drained everything due. 0 baseline while keeping up; grows once inserts outrun the drain — **the backlog/starvation alarm**. |
| `pageserver_yb_gc_applied_cutoff_lsn` | UIntGauge | — | Current GC cutoff (`applied_gc_cutoff_lsn`), per tick. `pageserver_last_record_lsn − this` = retained-history window. Frozen while the queue grows ⇒ cutoff pinned (lease/child/`consistent_lsn`), not a slow drain. |
| `pageserver_yb_gc_candidates_emitted_total` | IntCounter | `path` (`ingest`/`materialiser`/`drop`/`shrink`) | Candidates emitted into `pending_gc`: trim by producer path (post `gc_candidate_every_n_bases` sampling), drop/shrink events unsampled. `rate()` = candidate insert rate. |
| `pageserver_yb_gc_candidates_read_total` | IntCounter | — | Rows the drain's `pending_gc` select returned per tick (pre-routing). `== gc_drain_limit` ⇒ capped; `≫ removed` ⇒ read but not cleared. |
| `pageserver_yb_gc_candidates_removed_total` | IntCounter | — | Rows deleted from `pending_gc` by the dequeue (summed DELETE rowcounts; exceeds `read` under collapse). `rate(emitted) − rate(removed)` = queue growth. |
| `pageserver_yb_gc_pages_rows_deleted_total` | IntCounter | — | `pageserver.pages` rows deleted by the drain (trim / drop / shrink). `rate()` = reclamation throughput. |
| `pageserver_yb_gc_drop_deferred_total` | IntCounter | — | Drop events a tick left queued because their fork had a materialise job pending. Occasional increments are normal; sustained growth means a wedged job is pinning that fork's reclamation. |
| `pageserver_yb_gc_drain_ticks_total` | IntCounter | `outcome` (`drained`/`capped`/`empty`/`lease_grace`/`disabled`/`error`) | One increment per drain-loop iteration, by how it ended. `rate(capped)/rate(all)` = keep-up alarm; `lease_grace`/`disabled` moving = GC silently gated off; `rate(all)` = tick cadence. |

## Troubleshooting

Three checks localize most GC escalations (metrics above):

1. **Is GC running?** `enable_gc` on for the branch and
   `rate(gc_drain_ticks_total) > 0`. `outcome=disabled` climbing ⇒ gated
   off; `lease_grace` ⇒ skipped during fresh-attach grace; `error` ⇒ the
   drain is erroring.
2. **Is the queue keeping up?** `rate(candidates_emitted) −
   rate(candidates_removed) > 0` ⇒ the drain is falling behind; confirm
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
- The drain log line `gc drain complete … candidates_drained=…
  pages_rows_deleted=…`.
