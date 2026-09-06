//! PageServer GC: the periodic candidate drain (drop / shrink / trim)
//! plus its cutoff computation. `impl Timeline` here reaches
//! Timeline's private fields because `gc` is a descendant module of
//! `timeline`.

use super::*;
use pageserver_api::key::Key;

use crate::page_server::gc_result::GcTickOutcome;

/// One tick's due candidates split by coarse operation, which sets the act
/// order and the dequeue method. A candidate
/// [`Timeline::classify_candidate`] rejects enters no bucket at all.
struct PartitionedCandidates {
    trims: Vec<(Key, crate::page_store::GcKind)>,
    drops: Vec<crate::page_store::PendingGcRow>,
    shrinks: Vec<(crate::page_store::PendingGcRow, i64)>,
}

/// What one tick's [`Timeline::act`] did, and what it couldn't finish.
struct ActOutcome {
    rows_deleted: u64,
    /// Prefix sweeps that ran out of row budget with work left. Not
    /// dequeued — a later tick resumes them from their cursor.
    unfinished_sweeps: std::collections::HashSet<crate::page_store::GcCandidateId>,
}

/// The materialise jobs in flight when a tick started — snapshotted before
/// acting, so a job running during a drop's delete is always caught — indexed
/// for the one question a drop asks: could a job still insert an image inside
/// what I am about to reclaim?
///
/// The two sweep indexes are [`crate::page_store::GcSweepScope::rel_keyspace_range`]'s
/// membership test spelled out. A rel key is inside a `Relation`'s range
/// exactly when it carries that `(spcnode, dbnode, relnode)`, and inside a
/// `Database`'s exactly when it carries that `(spcnode, dbnode)` — the range
/// binds no column below the one its scope binds. So these answer the same
/// question as scanning the set against the range, without the scan.
#[derive(Default)]
struct PendingMaterialise {
    /// The objects' own sentinel keys, which is what a per-object drop names.
    objects: std::collections::HashSet<Key>,
    rels: std::collections::HashSet<(u32, u32, u32)>,
    dbs: std::collections::HashSet<(u32, u32)>,
}

impl PendingMaterialise {
    fn new(objects: std::collections::HashSet<Key>) -> Self {
        // Only rel keys: an SLRU sentinel shares neither range.
        let rels: std::collections::HashSet<(u32, u32, u32)> = objects
            .iter()
            .filter(|k| k.field1 == 0)
            .map(|k| (k.field2, k.field3, k.field4))
            .collect();
        // Derived from `rels`, not from `objects`: one entry per busy
        // relation, so the database index costs the busy relations, not the
        // whole set.
        let dbs = rels.iter().map(|(spc, db, _)| (*spc, *db)).collect();
        Self { objects, rels, dbs }
    }

    /// Whether a drop's target could still receive a materialised image at a
    /// folded LSN. Its *dequeue* is what this holds back — the drop is acted
    /// either way; dequeueing it while a job can still land inside the
    /// reclaimed range is the leak this exists to prevent.
    ///
    /// Matched per scope variant, not with a catch-all: a new scope answering
    /// as a database would be silently wrong here.
    fn defers_dequeue(
        &self,
        row: &crate::page_store::PendingGcRow,
        scope: Option<crate::page_store::GcSweepScope>,
    ) -> bool {
        use crate::page_store::GcSweepScope;
        match scope {
            Some(GcSweepScope::Relation((spcnode, dbnode), relnode)) => {
                self.rels
                    .contains(&(spcnode as u32, dbnode as u32, relnode as u32))
            }
            Some(GcSweepScope::Database((spcnode, dbnode))) => {
                self.dbs.contains(&(spcnode as u32, dbnode as u32))
            }
            None => self.objects.contains(&row.key),
        }
    }
}

/// A validated candidate: its bucket, plus whatever the validation parsed on
/// the way (so no later stage re-unwraps it).
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub(crate) enum CandidatePartition {
    Trim,
    Drop,
    Shrink { relsize: i64 },
}

/// Why the drain won't act on a due candidate. None of these dequeues the
/// row.
///
// TODO(gc): a `gc_purge_unactionable_candidates` tenant flag (default off) to
// dequeue the non-`UnknownKind` faults instead of leaving them queued.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub(crate) enum CandidateRejectionReason {
    UnknownKind,
    /// Shape doesn't match the kind, so the act path would blind-decode the
    /// wrong object.
    KeyShapeMismatch,
    ShrinkMissingRelsize,
    /// Not a valid block number.
    ShrinkRelsizeOutOfRange,
}

impl CandidateRejectionReason {
    /// `correctness_errors_total` labels.
    fn label(self) -> &'static str {
        match self {
            CandidateRejectionReason::UnknownKind => "gc_unknown_candidate_kind",
            CandidateRejectionReason::KeyShapeMismatch => "gc_candidate_key_shape_mismatch",
            CandidateRejectionReason::ShrinkMissingRelsize => "gc_shrink_missing_relsize",
            CandidateRejectionReason::ShrinkRelsizeOutOfRange => "gc_shrink_relsize_out_of_range",
        }
    }
}

impl Timeline {
    /// PageServer GC. One iteration:
    /// - **compute & publish** the cutoff (monotone, durable before any
    ///   delete);
    /// - **wait** out in-flight reads holding the old cutoff;
    /// - **drain** due `pending_gc` candidates — drop dead forks,
    ///   shrink truncated ones, trim each listed block down to its
    ///   anchor — then dequeue (crash-safety contract on
    ///   [`Self::drain_due_candidates`]).
    ///
    /// Caller (`gc_iteration`) has already gated on `enable_gc` and the
    /// lsn-lease deadline grace.
    pub(crate) async fn gc(&self) -> Result<GcResult, GcError> {
        let mut result = GcResult::default();
        // No writer yet ⇒ timeline not fully wired; nothing to GC.
        if self.page_writer.is_none() {
            return Ok(result);
        }
        // The whole drain runs on the dedicated GC pool (see
        // `TenantSharedResources::gc_page_store`).
        let store = self.gc_drain.page_store.clone();

        let cutoff = self.compute_gc_cutoff();
        let applied_cutoff = self.publish_and_await_readers(&store, cutoff).await?;
        self.drain_due_candidates(&store, applied_cutoff, &mut result)
            .await?;
        Ok(result)
    }

    /// This drain's target cutoff: the min over the retention floor,
    /// live leases, and consistent_lsn, then floored at our own fork
    /// point. Child fork points are deliberately absent — their pin is
    /// the clamp inside
    /// [`crate::page_store::PageStore::publish_gc_cutoff`].
    ///
    /// GC deletes below the returned cutoff, so no unguarded read may happen
    /// there. Compute getpage holds the `applied_gc_cutoff` RCU guard, so GC
    /// waits for it; WAL ingest skips already-durable records so it does not
    /// read below `consistent_lsn` (the pageserver's durable frontier), except
    /// for a one-record window where a flush raises the frontier past a record
    /// mid-apply (#588); materialiser folds tolerate a missing row.
    fn compute_gc_cutoff(&self) -> Lsn {
        let (retention_min, lease_min) = {
            let gc_info = self.gc_info.read().unwrap();
            let now = SystemTime::now();
            let lease_min = gc_info
                .leases
                .iter()
                .filter(|(_, lease)| !lease.is_expired(&now))
                .map(|(lsn, _)| *lsn)
                .next();
            (gc_info.min_cutoff(), lease_min)
        };
        let mut cutoff = retention_min;
        if let Some(lease) = lease_min {
            cutoff = cutoff.min(lease);
        }
        cutoff = cutoff.min(self.get_consistent_lsn());
        cutoff.max(self.get_ancestor_lsn())
    }

    /// Publish `cutoff` (monotone), then advance the in-memory floor and wait
    /// out every read admitted under the old one (RCU grace). Returns the
    /// authoritative `applied` floor.
    ///
    /// Durable BEFORE any delete: restart seeds the read floor from the
    /// persisted column, so deleting beyond it would re-admit reads into
    /// trimmed territory after a crash. GREATEST in the UPDATE keeps it
    /// monotone. The RCU wait ensures every read admitted under the old
    /// floor has finished before we delete anything it might need —
    /// reads never wait for GC; GC waits for reads.
    async fn publish_and_await_readers(
        &self,
        store: &Arc<crate::page_store::PageStore>,
        cutoff: Lsn,
    ) -> Result<Lsn, GcError> {
        let applied = store
            .publish_gc_cutoff(self.branch_id, cutoff)
            .await
            .map_err(GcError::Persistence)?;
        let waitlist = {
            let write_guard = self.applied_gc_cutoff_lsn.lock_for_write();
            if applied > *write_guard {
                Some(write_guard.store_and_unlock(applied))
            } else {
                None
            }
        };
        if let Some(waitlist) = waitlist {
            waitlist.wait().await;
        }
        Ok(applied)
    }

    /// Drain due `pending_gc` candidates at `applied_cutoff`:
    /// - **select**: rows with `floor < due_lsn <= applied_cutoff`,
    ///   oldest-first, capped at `gc_drain_limit` — the floor seeks the
    ///   scan past rows earlier ticks already handled;
    /// - **validate & partition**: every row is checked against its kind
    ///   ([`Self::classify_candidate`]) before it reaches an act helper, so
    ///   a corrupt row can't misdelete a live object;
    /// - **act**: drop → shrink → trim, coarsest-first (a dropped fork
    ///   moots its own shrink/trim work), forks parallel within a kind.
    ///   All drops are acted, busy or not;
    /// - **dequeue**, only after every action succeeded: trim blocks
    ///   range-sweep their candidates in `(floor, cutoff]` (duplicates
    ///   coalesce); drop/shrink clear by their PK `(ident, due_lsn)`. A
    ///   busy-object drop (a rel fork or SLRU segment with a materialise
    ///   job in flight) is acted but left queued so a later tick re-acts
    ///   it, sweeping any image a straggling job inserts after the delete
    ///   (a dropped object gets no new jobs, so a floor reset converges
    ///   it). Idempotent actions make act-before-dequeue crash-safe;
    /// - **advance the floor**, strictly after the dequeue: an error
    ///   before this point returns via `?` leaving it unchanged, so the
    ///   next tick re-selects. Where it lands — and why a capped tick that
    ///   cleared nothing lands somewhere else — is [`Self::next_drain_floor`].
    ///
    /// Every `gc_queue_floor_reset_interval` a tick resets the floor to
    /// 0 to sweep anything stranded below it — a clamp-race trim
    /// straggler or a still-queued busy-object drop.
    async fn drain_due_candidates(
        &self,
        store: &Arc<crate::page_store::PageStore>,
        applied_cutoff: Lsn,
        result: &mut GcResult,
    ) -> Result<(), GcError> {
        let drain_limit = self.get_gc_drain_limit();
        let (floor, is_reset) = self.pick_drain_floor();
        let selected = store
            .gc_select_due_candidates(self.branch_id, floor, applied_cutoff, drain_limit)
            .await
            .map_err(GcError::Persistence)?;
        crate::metrics::GC_APPLIED_CUTOFF_LSN.set(applied_cutoff.0);
        // Under the limit ⇒ every due candidate was returned (drained vs
        // capped label). Raw count, so a dropped malformed row still counts.
        let all_due_visited = selected.raw_count < drain_limit;
        // Residual drain lag: the due span a capped tick couldn't
        // reach; 0 when everything due was drained (incl. empty
        // queue — the gauge must reset or it freezes at a stale
        // value).
        crate::metrics::GC_DRAIN_LAG_BYTES.set(match selected.max_due_lsn {
            Some(max) if !all_due_visited => applied_cutoff.0.saturating_sub(max.0),
            _ => 0,
        });
        // All selected rows, before trim routing.
        crate::metrics::GC_CANDIDATES_READ.inc_by(selected.raw_count as u64);

        // Candidates whose act was budget-capped resume from their in-memory
        // cursor instead of being re-found by the select. That is what frees
        // the floor to advance past them: holding it at their `due_lsn` for
        // the whole sweep would make every tick re-walk the tombstones of
        // everything the sweep's own earlier ticks dequeued above it.
        let resuming = self.gc_drain.progress.lock().unwrap().unfinished_sweeps();
        if selected.max_due_lsn.is_none() && resuming.is_empty() {
            result.outcome = GcTickOutcome::NothingDue;
            crate::metrics::GC_DRAIN_TICKS
                .with_label_values(&[result.outcome.label()])
                .inc();
            // An empty reset tick found nothing stranded below the floor,
            // so the reset succeeded — stamp it (see pick_drain_floor).
            if is_reset {
                self.gc_drain.progress.lock().unwrap().last_floor_reset =
                    Some(std::time::Instant::now());
            }
            return Ok(());
        }
        let partition = Self::partition_by_kind(Self::with_resuming(selected.rows, &resuming));

        // Objects (rel forks / SLRU segments) with a materialise job
        // pending, snapshotted BEFORE acting so a job in flight during a
        // drop's delete is always caught: a busy-object drop is acted but
        // not dequeued (see `dequeue`), so a straggling image INSERT after
        // the delete can't leak. Paid only when drops are present.
        let pending_materialise = if partition.drops.is_empty() {
            PendingMaterialise::default()
        } else {
            PendingMaterialise::new(
                self.page_writer
                    .as_ref()
                    .map(|pw| pw.objects_with_pending_materialise())
                    .unwrap_or_default(),
            )
        };

        // Cancel is checked once, up front: a mid-tick cancel just
        // leaves candidates queued (idempotent, re-drained next tick).
        if self.cancel.is_cancelled() {
            return Err(GcError::TimelineCancelled);
        }

        // Act (delete), then dequeue what was acted — strictly after every
        // action succeeded. An error before the dequeue returns via `?`
        // leaving the floor unchanged, so the next tick re-selects the same
        // batch. `pending_materialise` was snapshotted before acting.
        let acted = self.act(store, &partition, applied_cutoff).await?;
        result.rows_deleted = acted.rows_deleted;

        let dequeued = self
            .dequeue(
                store,
                &partition,
                &pending_materialise,
                &acted.unfinished_sweeps,
                floor,
                applied_cutoff,
            )
            .await?;
        let next_floor = Self::next_drain_floor(
            all_due_visited,
            dequeued,
            // Nothing selected ⇒ the queue is drained as far as the select
            // can see, so the cutoff is where the floor belongs.
            selected.max_due_lsn.unwrap_or(applied_cutoff),
            applied_cutoff,
        );
        // Monotone `max` so a floor-reset tick's older batch (it scans
        // from 0) can't lower a higher floor. Stamp the reset only now the
        // dequeue succeeded, so a failed reset tick retries next tick
        // rather than burning an interval (see pick_drain_floor).
        let mut progress = self.gc_drain.progress.lock().unwrap();
        progress.floor = progress.floor.max(next_floor);
        if is_reset {
            progress.last_floor_reset = Some(std::time::Instant::now());
        }
        result.candidates_dequeued = dequeued;
        // An unfinished sweep is the same state `BacklogRemains` names — the
        // tick stopped on its own row budget with work left — even though it
        // visited everything due, so the loop keeps the short cadence instead
        // of pacing the sweep at one budget per base period.
        result.outcome = if all_due_visited && acted.unfinished_sweeps.is_empty() {
            GcTickOutcome::CaughtUp
        } else {
            GcTickOutcome::BacklogRemains
        };
        crate::metrics::GC_CANDIDATES_DEQUEUED.inc_by(dequeued);
        crate::metrics::GC_ROWS_DELETED.inc_by(result.rows_deleted);
        crate::metrics::GC_DRAIN_TICKS
            .with_label_values(&[result.outcome.label()])
            .inc();

        info!(
            applied_gc_cutoff = %applied_cutoff,
            candidates_dequeued = result.candidates_dequeued,
            rows_deleted = result.rows_deleted,
            "gc drain complete"
        );
        Ok(())
    }

    /// Act on every due candidate, coarsest-first — drop → shrink → trim,
    /// objects parallel within a kind: a dropped fork moots its own
    /// shrink/trim work. Each act helper is a phase barrier; a first error
    /// aborts before the caller dequeues (in-flight siblings may still land
    /// server-side — harmless, idempotent, they stay queued). All drops are
    /// acted, busy object or not. Returns rows deleted (across pages,
    /// slru_pages, and meta); the caller
    /// then [`Self::dequeue`]s what was acted.
    async fn act(
        &self,
        store: &Arc<crate::page_store::PageStore>,
        due: &PartitionedCandidates,
        applied_cutoff: Lsn,
    ) -> Result<ActOutcome, GcError> {
        let (mut rows_deleted, unfinished_sweeps) = self.act_drops(store, &due.drops).await?;
        rows_deleted += self.act_shrinks(store, &due.shrinks).await?;
        rows_deleted += self.act_trims(store, &due.trims, applied_cutoff).await?;
        Ok(ActOutcome {
            rows_deleted,
            unfinished_sweeps,
        })
    }

    /// Remove from `pending_gc` exactly what [`Self::act`] just reclaimed,
    /// returning the count. Trim keys range-sweep `(floor, cutoff]`;
    /// drop/shrink clear by their PK. A drop whose target could still receive
    /// a materialised image ([`PendingMaterialise::defers_dequeue`]) is left queued for
    /// a later tick — a straggling image INSERT after its delete would
    /// otherwise leak — and logged, not removed; so is a candidate in
    /// `unfinished_sweeps`, whose sweep ran out of row budget. Runs only
    /// after `act`
    /// succeeded (crash-safety: act-before-dequeue).
    async fn dequeue(
        &self,
        store: &Arc<crate::page_store::PageStore>,
        due: &PartitionedCandidates,
        pending_materialise: &PendingMaterialise,
        unfinished_sweeps: &std::collections::HashSet<crate::page_store::GcCandidateId>,
        floor: Lsn,
        applied_cutoff: Lsn,
    ) -> Result<u64, GcError> {
        // Trims dequeue by a range sweep over their (key, gc_kind) pairs;
        // shrinks and finished, idle drops by PK. A busy drop and a
        // budget-capped sweep stay queued.
        let mut drop_shrink_to_dequeue: Vec<crate::page_store::GcCandidateId> = due
            .shrinks
            .iter()
            .map(|(r, _)| (r.key, r.gc_kind, r.due_lsn))
            .collect();
        let mut drops_awaiting_materialise: Vec<Key> = Vec::new();
        for r in &due.drops {
            let id = (r.key, r.gc_kind, r.due_lsn);
            let sweep = crate::page_store::GcSweepCandidate::of(r);
            let dequeue_deferred = pending_materialise.defers_dequeue(r, sweep.map(|s| s.scope()));
            if dequeue_deferred {
                drops_awaiting_materialise.push(r.key);
                // A sweep's cursors may have moved past an object this tick
                // while a job could still insert an image behind them, so
                // re-acting from the cursor would not catch it. Owe a full
                // pass. The per-object kinds need nothing: their re-act covers
                // the whole object.
                if let Some(sweep) = sweep {
                    self.gc_drain
                        .progress
                        .lock()
                        .unwrap()
                        .require_recheck(sweep);
                }
            }
            if dequeue_deferred || unfinished_sweeps.contains(&id) {
                continue;
            }
            drop_shrink_to_dequeue.push(id);
        }
        // A busy object's drop is acted but left queued; it re-selects (and
        // re-logs) only on floor resets, so an object recurring here across
        // resets means its materialise job is wedged, pinning the drop's
        // reclamation. info! (not debug): the whole point is prod
        // visibility, and the volume is one line per object per reset.
        if !drops_awaiting_materialise.is_empty() {
            let sample = &drops_awaiting_materialise[..drops_awaiting_materialise.len().min(8)];
            tracing::info!(
                count = drops_awaiting_materialise.len(),
                objects = ?sample,
                "gc: busy-object drops left queued (materialise job in flight)"
            );
        }
        let removed_trim = store
            .gc_dequeue_trim_candidates(self.branch_id, &due.trims, floor, applied_cutoff)
            .await
            .map_err(GcError::Persistence)?;
        let removed_drop_shrink = store
            .gc_dequeue_drop_shrink_candidates(self.branch_id, &drop_shrink_to_dequeue)
            .await
            .map_err(GcError::Persistence)?;
        // A dequeued candidate has no cursor to resume from any more; only a
        // resumable kind ever has one, and forgetting a missing id is a no-op.
        self.gc_drain
            .progress
            .lock()
            .unwrap()
            .remove_cursors(&drop_shrink_to_dequeue);
        Ok(removed_trim + removed_drop_shrink)
    }

    /// Run `f` over each of `items` with bounded concurrency
    /// (`GC_ACTION_PARALLELISM` × tenant `scale_factor`) and sum the
    /// per-call deleted counts. Each item gets a fresh `store` clone handed
    /// to `f`, so the future it returns owns its captures (required to be
    /// `Send` across `buffer_unordered`).
    async fn run_parallel_actions<T, Fut>(
        &self,
        store: &Arc<crate::page_store::PageStore>,
        items: impl Iterator<Item = T>,
        f: impl Fn(Arc<crate::page_store::PageStore>, T) -> Fut,
    ) -> Result<u64, GcError>
    where
        Fut: std::future::Future<Output = anyhow::Result<u64>>,
    {
        use futures::{StreamExt as _, TryStreamExt as _};
        // Fan-out width: GC_ACTION_PARALLELISM scaled by the tenant
        // scale_factor (cheap; recomputed per fan-out).
        let parallelism = pageserver_api::config::scale_count(
            crate::page_store::GC_ACTION_PARALLELISM,
            self.conf.scale_factor,
        );
        let deleted: Vec<u64> = futures::stream::iter(items.map(|item| f(store.clone(), item)))
            .buffer_unordered(parallelism)
            .try_collect()
            .await
            .map_err(GcError::Persistence)?;
        Ok(deleted.into_iter().sum())
    }

    /// Drop actions, reclaiming each object's rows `<= due_lsn`
    /// (id-reuse-safe — a recreated object writes above it). SLRU drops act
    /// per candidate, in parallel (distinct tablets, no ordering constraint).
    /// Dead meta keys batch per `due_lsn` into one
    /// `key = ANY` delete — the same batched meta path `act_trims` uses.
    /// A dropped database or relation is a budgeted prefix sweep of its own
    /// ([`Self::act_drop_sweeps`]).
    async fn act_drops(
        &self,
        store: &Arc<crate::page_store::PageStore>,
        drop_rows: &[crate::page_store::PendingGcRow],
    ) -> Result<
        (
            u64,
            std::collections::HashSet<crate::page_store::GcCandidateId>,
        ),
        GcError,
    > {
        use crate::page_store::GcKind;
        use std::collections::HashMap;
        let branch_id = self.branch_id;

        // Split by table: SLRU drops act per candidate; DropMeta
        // keys group by `due_lsn` for one batched delete each; a whole
        // database or relation is its own budgeted prefix sweep.
        let mut slru_drops: Vec<crate::page_store::PendingGcRow> = Vec::new();
        let mut meta_by_due: HashMap<Lsn, Vec<Key>> = HashMap::new();
        let mut sweeps: Vec<crate::page_store::GcSweepCandidate> = Vec::new();
        for row in drop_rows {
            // Asked, not re-listed: `GcSweepCandidate::of` is what decides
            // which kinds sweep, so routing on it pairs each row with its
            // scope here and leaves the act nothing to re-derive or assert.
            if let Some(sweep) = crate::page_store::GcSweepCandidate::of(row) {
                sweeps.push(sweep);
                continue;
            }
            match row.gc_kind {
                GcKind::DropSlru => slru_drops.push(*row),
                GcKind::DropMeta => meta_by_due.entry(row.due_lsn).or_default().push(row.key),
                // partition_by_kind routes only drop kinds into this bucket.
                other => debug_assert!(false, "act_drops: non-drop kind {other:?}"),
            }
        }

        let mut deleted = self
            .run_parallel_actions(store, slru_drops.into_iter(), |store, row| async move {
                // An SLRU segment: its slru_pages rows plus its seg-size
                // sentinel meta key, which is this candidate's own key.
                store
                    .gc_act_drop_slru(
                        branch_id,
                        crate::page_store::slru_seg_of(&row.key),
                        crate::page_store::key_to_bytes(&row.key),
                        row.due_lsn,
                    )
                    .await
            })
            .await?;

        // Dead meta keys, one batched delete per `due_lsn`.
        for (due, keys) in meta_by_due {
            deleted += store
                .gc_act_drop_meta(branch_id, &keys, due)
                .await
                .map_err(GcError::Persistence)?;
        }

        let (swept, unfinished_sweeps) = self.act_drop_sweeps(store, &sweeps).await?;
        Ok((deleted + swept, unfinished_sweeps))
    }

    /// Prefix sweeps (`GcKind::DropDb`/`DropRel`), sequentially and under one
    /// shared per-tick row budget (`gc_sweep_rows_per_tick`): a dropped object
    /// is unbounded in size, and every sweep hits the same `pages` tablets, so
    /// running them in parallel would multiply the burst the budget exists to
    /// bound. Each candidate resumes from its own cursor
    /// (`GcDrainProgress::sweep_cursors`), and one whose range still has rows
    /// is reported back so the caller neither dequeues it nor lets the floor
    /// pass it.
    async fn act_drop_sweeps(
        &self,
        store: &Arc<crate::page_store::PageStore>,
        sweeps: &[crate::page_store::GcSweepCandidate],
    ) -> Result<
        (
            u64,
            std::collections::HashSet<crate::page_store::GcCandidateId>,
        ),
        GcError,
    > {
        let mut unfinished = std::collections::HashSet::new();
        if sweeps.is_empty() {
            return Ok((0, unfinished));
        }
        let mut budget = self.get_gc_sweep_rows_per_tick();
        let mut deleted = 0u64;
        for cand in sweeps.iter().copied() {
            let id = cand.id();
            if budget == 0 {
                // Not swept at all: an earlier candidate spent the budget.
                // Still needs a cursor, or `unfinished_sweeps` cannot see it
                // next tick and it waits for a floor reset instead — the floor
                // no longer holds for unfinished candidates. `ensure` and not
                // `store`: this candidate may be mid-sweep, and the order
                // sweeps are visited in shifts between ticks.
                self.gc_drain.progress.lock().unwrap().ensure_cursor(cand);
                unfinished.insert(id);
                continue;
            }
            let mut progress = self.gc_drain.progress.lock().unwrap().resume_cursor(cand);
            let acted = store
                .gc_act_sweep(self.branch_id, cand.due_lsn(), budget, &mut progress)
                .await;
            let reclaimed = progress.is_reclaimed();
            let owed_pass = progress.begin_owed_pass();
            // Store the cursor whatever the outcome: a failed sweep still
            // deleted whatever it got through, and re-reading from the head
            // would re-walk those tombstones.
            self.gc_drain
                .progress
                .lock()
                .unwrap()
                .store_cursor(id, progress);
            let rows = acted.map_err(GcError::Persistence)?;
            deleted += rows;
            budget = budget.saturating_sub(rows as usize);
            if !reclaimed {
                unfinished.insert(id);
            }
            // Only the owed pass is logged. A sweep merely spending its budget
            // is progress, reported every tick by `gc drain complete` and by
            // `gc_rows_deleted_total`; this one is rare and means a materialise
            // job may be wedged, deferring the object to the floor reset.
            if owed_pass {
                tracing::info!(
                    scope = ?cand.scope(),
                    due_lsn = %cand.due_lsn(),
                    rows_deleted = rows,
                    "gc: sweep owes a re-check pass (a fork was busy mid-sweep)"
                );
            }
        }
        Ok((deleted, unfinished))
    }

    /// Shrink actions, one delete per candidate. Returns rows deleted.
    async fn act_shrinks(
        &self,
        store: &Arc<crate::page_store::PageStore>,
        shrink_rows: &[(crate::page_store::PendingGcRow, i64)],
    ) -> Result<u64, GcError> {
        let targets = Self::shrink_targets(shrink_rows);
        let branch_id = self.branch_id;
        self.run_parallel_actions(
            store,
            targets.into_iter(),
            |store, (fork, relsize, due_lsn)| async move {
                store
                    .gc_act_shrink_fork(branch_id, fork, relsize, due_lsn)
                    .await
            },
        )
        .await
    }

    /// Build one shrink target per candidate, each with its own
    /// `(relsize, due_lsn)` — never coalesced per fork (that would reclaim
    /// a block re-extended between two truncations; see the trap on
    /// `gc_act_shrink_fork`). A total decode: the `relsize` is the one
    /// `classify_candidate` validated and the key is already known rel-size.
    fn shrink_targets(
        shrink_rows: &[(crate::page_store::PendingGcRow, i64)],
    ) -> Vec<(crate::page_store::GcForkIdent, i64, Lsn)> {
        shrink_rows
            .iter()
            .map(|(r, relsize)| (crate::page_store::rel_fork_of(&r.key), *relsize, r.due_lsn))
            .collect()
    }

    /// Trim actions: per rel fork / SLRU segment keep each block's anchor
    /// (newest base `<= applied_cutoff`) and everything above, delete
    /// below; per meta key keep the newest version `<= cutoff`. Objects
    /// run in parallel within each keyspace. Returns rows deleted.
    async fn act_trims(
        &self,
        store: &Arc<crate::page_store::PageStore>,
        trims: &[(Key, crate::page_store::GcKind)],
        applied_cutoff: Lsn,
    ) -> Result<u64, GcError> {
        use crate::page_store::GcKind;
        use std::collections::HashMap;
        let branch_id = self.branch_id;

        // Group by target table off the candidate's kind (block keys by
        // their object); keys are already deduped in partition_by_kind.
        // Keys, not blknums: the watermark map is keyed by `Key`, and a rel
        // block key's `field6` is its blknum, so the store's blknums derive
        // from these. `classify_candidate` has already checked the shape.
        let mut rel_keys_by_fork: HashMap<crate::page_store::GcForkIdent, Vec<Key>> =
            HashMap::new();
        let mut slru_keys_by_seg: HashMap<crate::page_store::GcSlruSeg, Vec<Key>> = HashMap::new();
        let mut meta_keys: Vec<Key> = Vec::new();
        for (key, gc_kind) in trims {
            match *gc_kind {
                GcKind::TrimPages => rel_keys_by_fork
                    .entry(crate::page_store::rel_fork_of(key))
                    .or_default()
                    .push(*key),
                GcKind::TrimSlru => slru_keys_by_seg
                    .entry(crate::page_store::slru_seg_of(key))
                    .or_default()
                    .push(*key),
                GcKind::TrimMeta => meta_keys.push(*key),
                // partition_by_kind routes only trim kinds into this bucket.
                other => debug_assert!(false, "act_trims: non-trim kind {other:?}"),
            }
        }

        let rel_deleted = self
            .run_parallel_actions(
                store,
                rel_keys_by_fork.into_iter(),
                |store, (fork, keys)| async move {
                    let watermarks = self.block_trim_watermarks(&keys);
                    let mut trimmed_to = Vec::new();
                    let deleted = store
                        .gc_act_trim_blocks(
                            branch_id,
                            fork,
                            &watermarks,
                            applied_cutoff,
                            &mut trimmed_to,
                        )
                        .await;
                    self.store_block_trim_watermarks(&keys[0], &trimmed_to);
                    deleted
                },
            )
            .await?;

        let slru_deleted = self
            .run_parallel_actions(
                store,
                slru_keys_by_seg.into_iter(),
                |store, (seg, keys)| async move {
                    let watermarks = self.block_trim_watermarks(&keys);
                    let mut trimmed_to = Vec::new();
                    let deleted = store
                        .gc_act_trim_slru(
                            branch_id,
                            seg,
                            &watermarks,
                            applied_cutoff,
                            &mut trimmed_to,
                        )
                        .await;
                    self.store_block_trim_watermarks(&keys[0], &trimmed_to);
                    deleted
                },
            )
            .await?;

        // Meta trim: one batched keep-newest over all meta keys. Its keys are
        // not block keys, so it drives the map directly rather than through the
        // two helpers above.
        let key_watermarks: Vec<(Key, Lsn)> = {
            let mut progress = self.gc_drain.progress.lock().unwrap();
            let watermarks = progress.trim_watermarks_of(&meta_keys);
            meta_keys.iter().copied().zip(watermarks).collect()
        };
        let mut meta_trimmed_to = Vec::new();
        let meta_deleted = store
            .gc_act_trim_meta(
                branch_id,
                &key_watermarks,
                applied_cutoff,
                &mut meta_trimmed_to,
            )
            .await
            .map_err(GcError::Persistence)?;
        self.gc_drain
            .progress
            .lock()
            .unwrap()
            .store_trim_watermarks(meta_trimmed_to);

        Ok(rel_deleted + slru_deleted + meta_deleted)
    }

    /// The watermarks for one object's blocks, paired with their blknums —
    /// what the two block trims bind. The map is keyed by `Key`; the store
    /// binds blknums, which are those keys' `field6`.
    fn block_trim_watermarks(&self, keys: &[Key]) -> Vec<(i64, Lsn)> {
        let mut progress = self.gc_drain.progress.lock().unwrap();
        let watermarks = progress.trim_watermarks_of(keys);
        keys.iter()
            .map(|k| k.field6 as i64)
            .zip(watermarks)
            .collect()
    }

    /// Store the anchors a block trim committed to, rebuilding each `Key` from
    /// `template` — any key of the same object, since `field6` is the only
    /// field a block key varies within one fork or SLRU segment. Rebuilding
    /// from the caller's own candidate avoids a second encoding of the rel and
    /// SLRU key layouts, which would have to move together.
    ///
    /// Called on the error path too: only committed anchors reach `trimmed_to`,
    /// and a failed statement's predecessors still made progress.
    fn store_block_trim_watermarks(&self, template: &Key, trimmed_to: &[(i64, Lsn)]) {
        self.gc_drain
            .progress
            .lock()
            .unwrap()
            .store_trim_watermarks(trimmed_to.iter().map(|&(blknum, anchor)| {
                (
                    Key {
                        field6: blknum as u32,
                        ..*template
                    },
                    anchor,
                )
            }));
    }

    /// The `meta` keys a `DropMeta` may name — one shape per emitter in
    /// `pgdatadir_mapping`, which is currently just `drop_twophase_file`. Add
    /// an emitter, add its shape.
    ///
    /// Trap: an allowlist, not "any meta-table key". Drop deletes every version
    /// `<= due_lsn`, so it is only correct on a dead object, and most meta keys
    /// are live state (the size sentinels, a dbdir, the twophase *directory*,
    /// whose key is `twophase_file_key(0)`). Every shape accepted here is blast
    /// radius for a mis-emitted candidate, so the list stays at what is
    /// actually emitted: the relmap and sparse-reldir shapes went with the
    /// per-fork `DROP DATABASE` emitter, and the rel-dir shape with the v1
    /// keyspace.
    fn is_droppable_meta_key(key: &Key) -> bool {
        key.is_twophase_file_key()
    }

    /// A shrink's truncation target, validated. `u32::MAX` is the rel-size
    /// sentinel's own `field6`, so the largest real target is one below it.
    fn validated_relsize(
        row: &crate::page_store::PendingGcRow,
    ) -> Result<i64, CandidateRejectionReason> {
        let relsize = row
            .relsize
            .ok_or(CandidateRejectionReason::ShrinkMissingRelsize)?;
        (0..u32::MAX as i64)
            .contains(&relsize)
            .then_some(relsize)
            .ok_or(CandidateRejectionReason::ShrinkRelsizeOutOfRange)
    }

    /// Validate one due candidate and classify it. The drain's sole
    /// row-validation point: one arm per kind, declaring the two things a kind
    /// implies — the key shape it must carry, and the bucket it acts in.
    pub(crate) fn classify_candidate(
        row: &crate::page_store::PendingGcRow,
    ) -> Result<CandidatePartition, CandidateRejectionReason> {
        use crate::page_store::GcKind;

        let shaped = |ok: bool| {
            ok.then_some(())
                .ok_or(CandidateRejectionReason::KeyShapeMismatch)
        };
        Ok(match row.gc_kind {
            GcKind::TrimPages => {
                shaped(row.key.is_rel_block_key())?;
                CandidatePartition::Trim
            }
            GcKind::TrimSlru => {
                shaped(row.key.is_slru_block_key())?;
                CandidatePartition::Trim
            }
            // Trim needs no allowlist, unlike DropMeta: it keeps the newest
            // version `<= cutoff`, and every `meta` row is self-contained (see
            // `build_meta_row`), so it can't lose readable state for any key.
            // This is also the exact domain — `MetaRow::trim_key` emits
            // whatever was written to the table.
            GcKind::TrimMeta => {
                shaped(row.key.is_meta_table_key())?;
                CandidatePartition::Trim
            }
            GcKind::DropSlru => {
                shaped(row.key.is_slru_segment_size_key())?;
                CandidatePartition::Drop
            }
            GcKind::DropMeta => {
                shaped(Self::is_droppable_meta_key(&row.key))?;
                CandidatePartition::Drop
            }
            // The database's relmap key — its `(spcnode, dbnode)` identity in
            // the rel keyspace, and one of the keys the sweep deletes.
            GcKind::DropDb => {
                shaped(row.key.is_relmap_file_key())?;
                CandidatePartition::Drop
            }
            // A relation's size sentinel. The act reads (spcnode, dbnode,
            // relnode) and sweeps every fork, so `forknum` is not consulted —
            // the emitter puts the main fork's there. `relnode != 0` is
            // load-bearing: relnode 0 is the database's own slot, so a sweep
            // scoped there spans the live relmap and rel-directory keys.
            // `ShrinkPages` needs no such check — it only touches `pages`,
            // where relnode 0 holds nothing.
            GcKind::DropRel => {
                shaped(row.key.is_rel_size_key() && row.key.field4 != 0)?;
                CandidatePartition::Drop
            }
            GcKind::ShrinkPages => {
                shaped(row.key.is_rel_size_key())?;
                CandidatePartition::Shrink {
                    relsize: Self::validated_relsize(row)?,
                }
            }
            GcKind::Unknown(_) => return Err(CandidateRejectionReason::UnknownKind),
        })
    }

    /// Add the candidates resuming from an in-memory cursor to the rows the
    /// select returned, skipping any the select already found — a floor-reset
    /// tick scans from the head and returns them too, and acting on one twice
    /// would re-issue its sweep and dequeue it twice.
    ///
    /// A resuming row carries no `relsize` (only `ShrinkPages` has one, and it
    /// is not resumable), so the id is the whole candidate.
    fn with_resuming(
        mut selected: Vec<crate::page_store::PendingGcRow>,
        resuming: &[crate::page_store::GcCandidateId],
    ) -> Vec<crate::page_store::PendingGcRow> {
        // Hashed, not scanned: `selected` runs to `gc_drain_limit` (10 000)
        // and one deferred sweep per dropped relation can resume, so the pair
        // is large x large in exactly the drop-heavy workload this serves.
        let already: std::collections::HashSet<crate::page_store::GcCandidateId> = selected
            .iter()
            .map(|r| (r.key, r.gc_kind, r.due_lsn))
            .collect();
        for id @ (key, gc_kind, due_lsn) in resuming {
            if already.contains(id) {
                continue;
            }
            selected.push(crate::page_store::PendingGcRow {
                key: *key,
                gc_kind: *gc_kind,
                due_lsn: *due_lsn,
                relsize: None,
            });
        }
        selected
    }

    /// Split due candidates into the [`PartitionedCandidates`] buckets, validating each
    /// via [`Self::classify_candidate`] so the act helpers can trust theirs. A
    /// refused row is alarmed and left queued (see [`CandidateRejectionReason`]).
    ///
    /// `trims` is deduped by key (a hot key has many due candidates), so
    /// neither the trim action nor the dequeue carries duplicates.
    fn partition_by_kind(due: Vec<crate::page_store::PendingGcRow>) -> PartitionedCandidates {
        let mut trims = Vec::new();
        let mut drops = Vec::new();
        let mut shrinks = Vec::new();
        for row in due {
            match Self::classify_candidate(&row) {
                Ok(CandidatePartition::Trim) => trims.push((row.key, row.gc_kind)),
                Ok(CandidatePartition::Drop) => drops.push(row),
                Ok(CandidatePartition::Shrink { relsize }) => shrinks.push((row, relsize)),
                Err(fault) => {
                    tracing::error!(
                        gc_kind = ?row.gc_kind,
                        key = %row.key,
                        due_lsn = %row.due_lsn,
                        relsize = ?row.relsize,
                        fault = ?fault,
                        "pending_gc candidate the drain won't act on; left queued"
                    );
                    crate::metrics::CORRECTNESS_ERRORS
                        .with_label_values(&[fault.label()])
                        .inc();
                }
            }
        }
        trims.sort_unstable();
        trims.dedup();
        PartitionedCandidates {
            trims,
            drops,
            shrinks,
        }
    }

    /// Where the select floor advances after a tick. `all_due_visited` → the
    /// cutoff (everything `<= cutoff` is handled; the insert clamp keeps new
    /// candidates above it). Otherwise → `max_due_lsn − 1`, keeping a
    /// limit-split tie group selectable next tick.
    ///
    /// Trap: `max_due_lsn − 1` can wedge the queue. When a tick clears nothing
    /// (every row refused, or every row a busy-object drop) the floor lands just
    /// below the batch, so the next tick re-selects it unchanged. That repeats
    /// forever if one `due_lsn` holds `gc_drain_limit` rows or more: the batch
    /// is then filled by that group alone, nothing above it is ever reached, and
    /// the branch's whole queue stalls — not just those rows. Groups do get that
    /// large; the emitter flushes up to 500 keys at one `due_lsn`.
    ///
    /// So a tick that cleared nothing advances to `max_due_lsn` instead, putting
    /// the group below the floor so the rest of the queue drains. Rows at that
    /// `due_lsn` the limit didn't return stay stuck behind it — a floor reset
    /// re-scans from the head, but the group refills the batch — until the
    /// blocking rows themselves are cleared. Nothing is lost either way; none of
    /// them was dequeued.
    ///
    /// A candidate whose act was budget-capped does NOT hold the floor: it
    /// resumes from its in-memory cursor ([`Self::with_resuming`]), so the
    /// select never has to re-find it.
    fn next_drain_floor(
        all_due_visited: bool,
        dequeued: u64,
        max_due_lsn: Lsn,
        applied_cutoff: Lsn,
    ) -> Lsn {
        match (all_due_visited, dequeued) {
            (true, _) => applied_cutoff,
            (false, 0) => max_due_lsn,
            (false, _) => Lsn(max_due_lsn.0.saturating_sub(1)),
        }
    }

    /// Per-tick cap on candidates the drain selects (tenant override →
    /// default). Sole caller: `drain_due_candidates`.
    fn get_gc_drain_limit(&self) -> usize {
        let tenant_conf = self.tenant_conf.load();
        tenant_conf
            .tenant_conf
            .gc_drain_limit
            .unwrap_or(self.conf.default_tenant_conf.gc_drain_limit)
    }

    /// Per-tick cap on rows the prefix sweeps delete, shared by every such
    /// candidate in the tick (tenant override → default). Sole caller:
    /// `act_drop_sweeps`.
    fn get_gc_sweep_rows_per_tick(&self) -> usize {
        let tenant_conf = self.tenant_conf.load();
        tenant_conf
            .tenant_conf
            .gc_sweep_rows_per_tick
            .unwrap_or(self.conf.default_tenant_conf.gc_sweep_rows_per_tick)
    }

    /// How often a tick resets the select floor to 0 (tenant override →
    /// default).
    fn get_gc_queue_floor_reset_interval(&self) -> std::time::Duration {
        let tenant_conf = self.tenant_conf.load();
        tenant_conf
            .tenant_conf
            .gc_queue_floor_reset_interval
            .unwrap_or(self.conf.default_tenant_conf.gc_queue_floor_reset_interval)
    }

    /// This tick's floor and whether it's a reset tick: `(Lsn(0), true)`
    /// once `gc_queue_floor_reset_interval` has elapsed since the last
    /// *successful* reset (scan the queue from the head), else
    /// `(seek-hint, false)`. Does NOT stamp `last_floor_reset` — the
    /// caller stamps it only after the tick commits, so a reset tick that
    /// errors (the failure-prone one: it skims the tombstone backlog)
    /// retries next tick instead of burning a whole interval. Sync —
    /// keeps the progress mutex off the drain's awaits.
    fn pick_drain_floor(&self) -> (Lsn, bool) {
        let interval = self.get_gc_queue_floor_reset_interval();
        let now = std::time::Instant::now();
        let progress = self.gc_drain.progress.lock().unwrap();
        if progress
            .last_floor_reset
            .is_none_or(|t| now.duration_since(t) >= interval)
        {
            (Lsn(0), true)
        } else {
            (progress.floor, false)
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::page_store::{GcKind, PendingGcRow};
    use pageserver_api::key::{
        DBDIR_KEY, TWOPHASEDIR_KEY, rel_block_to_key, rel_dir_to_key, rel_size_to_key,
        rel_tag_sparse_key, relmap_file_key, slru_block_to_key, slru_dir_to_key,
        slru_segment_size_to_key, twophase_file_key,
    };
    use pageserver_api::reltag::{RelTag, SlruKind};

    fn rel(relnode: u32) -> RelTag {
        RelTag {
            spcnode: 1663,
            dbnode: 5,
            relnode,
            forknum: 0,
        }
    }
    /// The key shape each `gc_kind` is required to carry — what a
    /// well-formed candidate of that kind looks like.
    fn well_formed_key(gc_kind: GcKind) -> Key {
        match gc_kind {
            GcKind::TrimPages => rel_block_to_key(rel(16400), 7),
            GcKind::ShrinkPages | GcKind::DropRel => rel_size_to_key(rel(16400)),
            GcKind::TrimSlru => slru_block_to_key(SlruKind::Clog, 3, 7),
            GcKind::DropSlru => slru_segment_size_to_key(SlruKind::Clog, 3),
            // One of DropMeta's four allowed shapes; also a valid TrimMeta
            // key, since every droppable meta key is trimmable.
            GcKind::TrimMeta | GcKind::DropMeta => twophase_file_key(42),
            GcKind::DropDb => relmap_file_key(1663, 5),
            GcKind::Unknown(_) => unreachable!("an unknown kind has no required shape"),
        }
    }
    /// A well-formed candidate of `gc_kind` (a shrink gets a valid relsize).
    fn row_of(gc_kind: GcKind) -> PendingGcRow {
        PendingGcRow {
            key: well_formed_key(gc_kind),
            gc_kind,
            due_lsn: Lsn(100),
            relsize: (gc_kind == GcKind::ShrinkPages).then_some(2),
        }
    }
    /// Count of `correctness_errors_total{kind=label}`.
    fn alarms(label: &str) -> u64 {
        crate::metrics::CORRECTNESS_ERRORS
            .with_label_values(&[label])
            .get()
    }
    /// A validated shrink bucket entry, as `partition_by_kind` would build it —
    /// the row and its relsize derived from one argument so the pair can't
    /// drift.
    fn shrink_entry(relsize: i64, due: u64) -> (PendingGcRow, i64) {
        (
            PendingGcRow {
                relsize: Some(relsize),
                due_lsn: Lsn(due),
                ..row_of(GcKind::ShrinkPages)
            },
            relsize,
        )
    }
    /// Scenario: the queue holds a row of a kind this version doesn't
    /// know. The partition must skip + alarm it — never panic on table
    /// contents — while still routing the known kinds. Entering no bucket
    /// is what leaves it queued: `act` never sees it and `dequeue` only
    /// clears what the buckets carry, so a newer binary still finds it.
    #[test]
    fn partition_parks_unknown_kind_without_dequeueing_it() {
        let before = alarms("gc_unknown_candidate_kind");
        let unknown = PendingGcRow {
            gc_kind: GcKind::Unknown(99), // no such kind
            ..row_of(GcKind::TrimPages)
        };
        let p = Timeline::partition_by_kind(vec![
            row_of(GcKind::TrimPages),
            row_of(GcKind::DropRel),
            row_of(GcKind::ShrinkPages),
            unknown,
        ]);
        assert_eq!(p.trims.len(), 1);
        assert_eq!(p.drops.len(), 1);
        assert_eq!(p.shrinks.len(), 1);
        assert_eq!(
            alarms("gc_unknown_candidate_kind") - before,
            1,
            "the unknown kind alarms once"
        );
    }

    /// Each of the 8 kinds, carrying its required key shape, buckets by
    /// coarse operation (trim / drop / shrink) — none rejected — and a trim
    /// retains its `gc_kind` for table dispatch.
    #[test]
    fn partition_buckets_all_eight_kinds() {
        let p = Timeline::partition_by_kind(
            [
                GcKind::TrimPages,
                GcKind::TrimSlru,
                GcKind::TrimMeta,
                GcKind::ShrinkPages,
                GcKind::DropRel,
                GcKind::DropSlru,
                GcKind::DropMeta,
                GcKind::DropDb,
            ]
            .map(row_of)
            .to_vec(),
        );
        // 3 TRIM_* → trims, 4 DROP_* → drops, 1 SHRINK → shrinks.
        let mut trim_kinds: Vec<GcKind> = p.trims.iter().map(|(_, kind)| *kind).collect();
        trim_kinds.sort_unstable();
        assert_eq!(
            trim_kinds,
            vec![GcKind::TrimPages, GcKind::TrimSlru, GcKind::TrimMeta]
        );
        assert_eq!(p.drops.len(), 4);
        assert_eq!(p.shrinks.len(), 1);
    }

    /// Every kind's key shape is enforced, because each act path decodes
    /// the object identity from the key *blind* — a `TrimPages` row holding
    /// a rel-*size* key would trim `field6 = u32::MAX` as a block number,
    /// and a `DropRel` row holding a rel-*block* key would take its blknum
    /// for a fork and drop a live relation wholesale. Every kind is rejected when handed a key from the
    /// wrong keyspace. (`partition_buckets_all_eight_kinds` pins the
    /// converse: each kind's *right* shape classifies.)
    #[test]
    fn classify_rejects_a_key_shape_that_mismatches_its_kind() {
        // Per kind: a key of the WRONG shape for it.
        let cases = [
            (GcKind::TrimPages, rel_size_to_key(rel(16400))),
            (GcKind::ShrinkPages, rel_block_to_key(rel(16400), 7)),
            (
                GcKind::TrimSlru,
                slru_segment_size_to_key(SlruKind::Clog, 3),
            ),
            (GcKind::DropSlru, slru_block_to_key(SlruKind::Clog, 3, 7)),
            (GcKind::TrimMeta, rel_block_to_key(rel(16400), 7)),
            (GcKind::DropMeta, rel_block_to_key(rel(16400), 7)),
            // A db drop names the database, not one of its relations. The act
            // reads only (spcnode, dbnode) from the key, so a rel-size key
            // would name the LIVE database that relation belongs to and
            // delete every row of it.
            (GcKind::DropDb, rel_size_to_key(rel(16400))),
            (GcKind::DropDb, DBDIR_KEY),
            // A rel drop names a relation: a block key would take its blknum
            // for a fork, and the relmap key has relnode 0.
            (GcKind::DropRel, rel_block_to_key(rel(16400), 7)),
            (GcKind::DropRel, relmap_file_key(1663, 5)),
            // Rel-size-shaped but at relnode 0, whose sweep range would cover
            // the database's live relmap and rel-directory keys.
            (GcKind::DropRel, rel_size_to_key(rel(0))),
        ];
        for (gc_kind, wrong_key) in cases {
            let row = PendingGcRow {
                key: wrong_key,
                ..row_of(gc_kind)
            };
            assert_eq!(
                Timeline::classify_candidate(&row),
                Err(CandidateRejectionReason::KeyShapeMismatch),
                "{gc_kind:?} must reject {wrong_key}"
            );
        }
    }

    /// A shrink's `relsize` is the truncation target `gc_act_shrink_fork`
    /// deletes from (`blknum >= relsize`), so it must be present AND a real
    /// block number. A **negative** relsize is the dangerous one: it would
    /// match every block and delete the whole fork.
    #[test]
    fn classify_rejects_a_shrink_relsize_it_cannot_trust() {
        let with_relsize = |relsize: Option<i64>| PendingGcRow {
            relsize,
            ..row_of(GcKind::ShrinkPages)
        };
        assert_eq!(
            Timeline::classify_candidate(&with_relsize(None)),
            Err(CandidateRejectionReason::ShrinkMissingRelsize),
        );
        for bad in [-1, i64::MIN, u32::MAX as i64, i64::MAX] {
            assert_eq!(
                Timeline::classify_candidate(&with_relsize(Some(bad))),
                Err(CandidateRejectionReason::ShrinkRelsizeOutOfRange),
                "relsize {bad} is not a block number",
            );
        }
        // Truncate-to-empty and the largest real fork are both valid.
        for ok in [0, u32::MAX as i64 - 1] {
            assert!(Timeline::classify_candidate(&with_relsize(Some(ok))).is_ok());
        }
    }

    /// `DropMeta` accepts exactly the shape its one emitter produces, and
    /// nothing else — a drop deletes *every* version `<= due_lsn`, so naming
    /// any other meta key destroys live state. `TrimMeta` accepts all of them
    /// plus the rest of the meta table: it keeps the newest version, which is
    /// safe on live state and is what `MetaRow::trim_key` actually feeds it.
    #[test]
    fn drop_meta_allows_only_its_emitters_shapes_trim_meta_allows_any() {
        let refused = Err(CandidateRejectionReason::KeyShapeMismatch);
        let cases = [
            // The one live DropMeta emitter in `pgdatadir_mapping`.
            (
                "twophase file",
                twophase_file_key(42),
                Ok(CandidatePartition::Drop),
            ),
            // Shapes with no emitter: a drop deletes every version, so an
            // unemitted shape is blast radius, not compatibility.
            ("relmap", relmap_file_key(1663, 5), refused),
            ("rel dir", rel_dir_to_key(1663, 5), refused),
            (
                "sparse reldir entry",
                rel_tag_sparse_key(1663, 5, 16400, 0),
                refused,
            ),
            // Live meta keys no emitter produces. Each would lose real data:
            // the size sentinels belong to Drop{Pages,Slru} (which delete them
            // with the object), and the dir keys are live state.
            ("rel size sentinel", rel_size_to_key(rel(16400)), refused),
            (
                "slru seg-size sentinel",
                slru_segment_size_to_key(SlruKind::Clog, 3),
                refused,
            ),
            ("dbdir", DBDIR_KEY, refused),
            ("twophase dir", TWOPHASEDIR_KEY, refused),
            ("slru dir", slru_dir_to_key(SlruKind::Clog), refused),
        ];
        for (what, key, expect_drop) in cases {
            let row = |gc_kind| PendingGcRow {
                key,
                ..row_of(gc_kind)
            };
            assert_eq!(
                Timeline::classify_candidate(&row(GcKind::DropMeta)),
                expect_drop,
                "DropMeta on the {what} key"
            );
            assert_eq!(
                Timeline::classify_candidate(&row(GcKind::TrimMeta)),
                Ok(CandidatePartition::Trim),
                "{what} is trimmable — trim keeps the newest version"
            );
        }
    }

    /// The floor must move even when a tick clears nothing, or a tie group of
    /// `>= gc_drain_limit` permanently-queued rows re-selects forever and
    /// starves everything above it (see `next_drain_floor`).
    #[test]
    fn next_drain_floor_steps_over_a_tick_that_cleared_nothing() {
        let (max_due, cutoff) = (Lsn(500), Lsn(900));
        // Capped and nothing cleared ⇒ step over the group, don't hold at 499.
        assert_eq!(
            Timeline::next_drain_floor(false, 0, max_due, cutoff),
            max_due,
            "a capped tick that dequeued nothing must not re-select itself"
        );
        // Capped but progress made ⇒ keep the split tie group selectable.
        assert_eq!(
            Timeline::next_drain_floor(false, 1, max_due, cutoff),
            Lsn(499)
        );
        // all_due_visited ⇒ the cutoff, cleared or not (left-queued rows wait
        // for a reset, or resume from their cursor).
        for dequeued in [0, 7] {
            assert_eq!(
                Timeline::next_drain_floor(true, dequeued, max_due, cutoff),
                cutoff
            );
        }
        // No underflow at the bottom of the range.
        assert_eq!(Timeline::next_drain_floor(false, 1, Lsn(0), cutoff), Lsn(0));
    }

    /// A candidate resuming from its cursor is added to the tick's rows, and a
    /// floor-reset tick that also selects it from the queue must not get it
    /// twice — acting on it twice re-issues its sweep, and dequeueing it twice
    /// is a second delete of a row already gone.
    #[test]
    fn resuming_candidates_merge_without_duplicating_a_selected_row() {
        let db = relmap_file_key(1663, 5);
        let queued = PendingGcRow {
            key: db,
            gc_kind: GcKind::DropDb,
            due_lsn: Lsn(100),
            relsize: None,
        };
        let other = row_of(GcKind::TrimPages);

        // The select already returned it: no duplicate.
        let merged =
            Timeline::with_resuming(vec![other, queued], &[(db, GcKind::DropDb, Lsn(100))]);
        assert_eq!(merged.len(), 2, "already-selected candidate not re-added");

        // The select missed it (the floor advanced past it): it is added back.
        let merged = Timeline::with_resuming(vec![other], &[(db, GcKind::DropDb, Lsn(100))]);
        assert_eq!(merged, vec![other, queued], "resumed from the cursor alone");

        // Same database, different drop LSN — a re-created and re-dropped
        // database — is a distinct candidate and must not dedup away.
        let merged = Timeline::with_resuming(vec![queued], &[(db, GcKind::DropDb, Lsn(200))]);
        assert_eq!(merged.len(), 2);
    }

    /// A db-wide drop covers every fork of its database, so ANY fork with a
    /// materialise job in flight has to hold its dequeue back — the image
    /// that job may still insert at a folded LSN lands inside the swept
    /// range. Another database's busy fork is irrelevant, and the per-object
    /// kinds keep naming their object outright.
    #[test]
    fn db_drop_defers_while_any_fork_of_its_database_awaits_materialise() {
        let db_drop = PendingGcRow {
            key: relmap_file_key(1663, 5),
            ..row_of(GcKind::DropDb)
        };
        let busy = |keys: &[Key]| PendingMaterialise::new(keys.iter().copied().collect());
        // Scoped the way the drain scopes it, so this covers the composition
        // and not just the predicate.
        let awaits = |row: &crate::page_store::PendingGcRow, pending: &PendingMaterialise| {
            pending.defers_dequeue(
                row,
                crate::page_store::GcSweepCandidate::of(row).map(|c| c.scope()),
            )
        };

        assert!(!awaits(&db_drop, &busy(&[])));
        assert!(
            awaits(&db_drop, &busy(&[rel_size_to_key(rel(16400))])),
            "a busy fork of this database defers the whole db drop"
        );
        let other_db = RelTag {
            dbnode: 6,
            ..rel(16400)
        };
        assert!(
            !awaits(&db_drop, &busy(&[rel_size_to_key(other_db)])),
            "another database's busy fork is not this drop's business"
        );

        // A relation drop covers its own forks and stops there.
        let rel_drop = PendingGcRow {
            key: rel_size_to_key(rel(16400)),
            ..row_of(GcKind::DropRel)
        };
        assert!(
            awaits(
                &rel_drop,
                &busy(&[rel_size_to_key(RelTag {
                    forknum: 2,
                    ..rel(16400)
                })])
            ),
            "a busy fork defers the whole relation's drop"
        );
        assert!(
            !awaits(&rel_drop, &busy(&[rel_size_to_key(rel(16401))])),
            "a sibling relation's busy fork is not this drop's business"
        );

        // A per-object drop still matches on its own key alone.
        let fork_drop = row_of(GcKind::DropSlru);
        assert!(awaits(&fork_drop, &busy(&[fork_drop.key])));
        assert!(!awaits(&fork_drop, &busy(&[rel_size_to_key(other_db)])));
    }

    /// A refused row enters no bucket at all — so `act` can't touch it and
    /// `dequeue` can't clear it, which is how it stays queued for a fixed or
    /// newer binary. It alarms once per occurrence (not once per key), and a
    /// well-formed sibling still drains normally.
    #[test]
    fn partition_leaves_refused_rows_queued() {
        let before = alarms("gc_candidate_key_shape_mismatch");
        let bad = PendingGcRow {
            key: rel_size_to_key(rel(16400)), // not a block key
            ..row_of(GcKind::TrimPages)
        };
        let bad_later = PendingGcRow {
            due_lsn: Lsn(200),
            ..bad
        };
        let p = Timeline::partition_by_kind(vec![bad, bad_later, row_of(GcKind::TrimPages)]);
        assert_eq!(
            p.trims,
            vec![(well_formed_key(GcKind::TrimPages), GcKind::TrimPages)],
            "only the well-formed trim is acted and dequeued"
        );
        assert!(p.drops.is_empty() && p.shrinks.is_empty());
        assert_eq!(alarms("gc_candidate_key_shape_mismatch") - before, 2);
    }

    /// Two shrink candidates on the SAME fork (two truncations) must stay
    /// two independent targets — never coalesced. Merging them into a
    /// wider `blknum` × higher `lsn` delete would reclaim a block
    /// re-extended between the truncations (the `gc_act_shrink_fork` trap):
    /// e.g. truncate-to-2@50 then truncate-to-3@80, with the block
    /// rewritten at lsn 60 in between — a merged `blk>=2 AND lsn<=80`
    /// delete would drop that live lsn-60 base.
    #[test]
    fn shrink_targets_not_merged_per_fork() {
        let targets = Timeline::shrink_targets(&[shrink_entry(2, 50), shrink_entry(3, 80)]);
        assert_eq!(
            targets,
            vec![
                ((1663, 5, 16400, 0), 2, Lsn(50)),
                ((1663, 5, 16400, 0), 3, Lsn(80)),
            ],
            "same-fork shrinks stay separate, each with its own (relsize, due_lsn)"
        );
    }
}
