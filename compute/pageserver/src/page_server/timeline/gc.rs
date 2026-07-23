//! PageServer GC: the periodic candidate drain (drop / shrink / trim)
//! plus its cutoff computation. `impl Timeline` here reaches
//! Timeline's private fields because `gc` is a descendant module of
//! `timeline`.

use super::*;

/// Every Nth drain tick ignores the floor and rescans from the queue
/// head (floor 0). At the default 30s `gc_period` this is ~2 hours. See
/// [`Timeline::drain_due_candidates`] for why.
const GC_FULL_RESCAN_EVERY_N_TICKS: u64 = 240;

impl Timeline {
    /// PageServer GC. One iteration:
    /// - **compute & publish** the cutoff (generation-fenced,
    ///   monotone);
    /// - **wait** out in-flight reads holding the old cutoff;
    /// - **drain** due `pending_gc` candidates — drop dead forks,
    ///   shrink truncated ones, trim each listed block down to its
    ///   anchor — then dequeue (crash-safety contract on
    ///   [`Self::drain_due_candidates`]).
    ///
    /// Caller (`gc_iteration`) has already gated on
    /// `test_enable_gc_drain` and the lsn-lease deadline grace.
    pub(crate) async fn gc(&self) -> Result<GcResult, GcError> {
        let mut result = GcResult::default();
        // No writer yet ⇒ timeline not fully wired; nothing to GC.
        if self.page_writer.is_none() {
            return Ok(result);
        }
        // The whole drain runs on the dedicated GC pool (see
        // `TenantSharedResources::gc_page_store`).
        let store = self.gc_drain.page_store.clone();

        let cutoff = self.compute_gc_cutoff(&store).await?;
        let applied_cutoff = self.publish_and_await_readers(&store, cutoff).await?;
        self.drain_due_candidates(&store, applied_cutoff, &mut result)
            .await?;
        Ok(result)
    }

    /// This drain's target cutoff: the min over the retention floor,
    /// live leases, child fork points (from timeline_state — children
    /// are other processes), and consistent_lsn (post-crash replay
    /// reads below it; the cutoff must never pass it), then floored at
    /// our own fork point.
    async fn compute_gc_cutoff(
        &self,
        store: &Arc<crate::page_store::PageStore>,
    ) -> Result<Lsn, GcError> {
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
        let children_min = store
            .min_children_fork_lsn(self.branch_id)
            .await
            .map_err(GcError::Persistence)?;
        let mut cutoff = retention_min;
        if let Some(lease) = lease_min {
            cutoff = cutoff.min(lease);
        }
        if let Some(child) = children_min {
            cutoff = cutoff.min(child);
        }
        cutoff = cutoff.min(self.get_consistent_lsn());
        cutoff = cutoff.max(self.get_ancestor_lsn());
        Ok(cutoff)
    }

    /// Publish `cutoff` (generation-fenced, monotone), then advance the
    /// in-memory floor and wait out every read admitted under the old
    /// one (RCU grace). Returns the authoritative `applied` floor.
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
        use crate::page_store::PublishGcCutoffOutcome;
        let applied = match store
            .publish_gc_cutoff(self.branch_id, self.generation, cutoff)
            .await
            .map_err(GcError::Persistence)?
        {
            PublishGcCutoffOutcome::Published { applied } => applied,
            PublishGcCutoffOutcome::StaleGeneration { live_generation } => {
                return Err(GcError::StaleGeneration { live_generation });
            }
        };
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
    /// - **select**: rows with
    ///   `floor < due_lsn <= applied_cutoff`, oldest-first, capped at
    ///   `gc_drain_limit` — the floor (`completed_lsn`) seeks the scan
    ///   past the tombstones of rows earlier ticks already drained;
    /// - **act**: drop → shrink → trim, coarsest-first (a dropped fork
    ///   moots its own shrink/trim work), forks parallel within a kind;
    ///   drop events whose fork has a materialise job pending are held
    ///   in the in-memory `deferred_drops` list instead (the floor
    ///   will pass them, so re-selection can't be their retry path);
    /// - **dequeue**, only after every action succeeded: with
    ///   idempotent actions that ordering is crash-safe without a
    ///   cross-table transaction — a crash in between re-drains the
    ///   same candidates into a no-op;
    /// - **advance the floor**, strictly after the dequeue: an error
    ///   before this point returns via `?` leaving the floor unchanged,
    ///   so the next tick selects the same rows again. An uncapped tick
    ///   advances to the cutoff (everything `<= cutoff` was handled);
    ///   a capped one to `last selected due_lsn − 1` (the −1 keeps a
    ///   limit-split tie group selectable). Unchanged on an empty
    ///   select. Deferred rows don't hold it back — the list owns them;
    ///   nor does an unknown-kind row (left on disk, never dequeued).
    ///
    /// Every `GC_FULL_RESCAN_EVERY_N_TICKS`th tick ignores the floor
    /// (lower bound 0), so the scan starts from the queue head, to
    /// reclaim a candidate the materialiser clamp left stranded below
    /// the floor (see `PendingGcBuffer`).
    async fn drain_due_candidates(
        &self,
        store: &Arc<crate::page_store::PageStore>,
        applied_cutoff: Lsn,
        result: &mut GcResult,
    ) -> Result<(), GcError> {
        let drain_limit = self.get_gc_drain_limit();
        // One lock (a std Mutex can't be held across the awaits below):
        // bump the tick counter, pick the floor, snapshot the deferred
        // list.
        let (floor, deferred_snapshot) = {
            let mut progress = self.gc_drain.progress.lock().unwrap();
            let full_rescan = progress.tick_count % GC_FULL_RESCAN_EVERY_N_TICKS == 0;
            progress.tick_count += 1;
            let floor = if full_rescan {
                Lsn(0)
            } else {
                progress.completed_lsn
            };
            (floor, progress.deferred_drops.clone())
        };
        let due = store
            .select_due_pending_gc(self.branch_id, floor, applied_cutoff, drain_limit)
            .await
            .map_err(GcError::Persistence)?;
        crate::metrics::GC_APPLIED_CUTOFF_LSN.set(applied_cutoff.0);
        // Under the limit ⇒ every due candidate was returned — the
        // tick fully drains the due span (drained vs capped label).
        let all_due_visited = due.len() < drain_limit;
        // Residual drain lag: the due span a capped tick couldn't
        // reach; 0 when everything due was drained (incl. empty
        // queue — the gauge must reset or it freezes at a stale
        // value).
        crate::metrics::GC_DRAIN_LAG_BYTES.set(match due.last() {
            Some(r) if !all_due_visited => applied_cutoff.0.saturating_sub(r.due_lsn.0),
            _ => 0,
        });
        let next_floor = Self::next_floor(
            due.last().map(|r| r.due_lsn),
            all_due_visited,
            applied_cutoff,
        );
        // All selected rows, before trim routing.
        crate::metrics::GC_CANDIDATES_READ.inc_by(due.len() as u64);

        let select_was_empty = due.is_empty();
        let (trim_blocks_by_fork, trim_rows, drop_rows, shrink_rows) = Self::partition_by_kind(due);

        // Merge this tick's drop events with the deferred snapshot
        // (taken at the top; written back only after the whole tick
        // succeeds, so a failed tick can't lose a deferred row the floor
        // already passed) and hold back the busy forks.
        let busy = if drop_rows.is_empty() && deferred_snapshot.is_empty() {
            Default::default()
        } else {
            // The busy-fork snapshot is paid only on the rare tick
            // that has drop events in play.
            self.page_writer
                .as_ref()
                .map(|pw| pw.forks_with_pending_materialise())
                .unwrap_or_default()
        };
        let (drop_rows, drop_deferred) =
            Self::partition_drops_by_busy_fork(deferred_snapshot, drop_rows, &busy);
        if !drop_deferred.is_empty() {
            crate::metrics::GC_DROP_DEFERRED.inc_by(drop_deferred.len() as u64);
            info!(
                drop_deferred = drop_deferred.len(),
                "gc: drop events held deferred — fork has a pending materialise job"
            );
        }
        if select_was_empty && drop_rows.is_empty() {
            crate::metrics::GC_DRAIN_TICKS
                .with_label_values(&["empty"])
                .inc();
            return Ok(());
        }

        // Cancel is checked once, up front: a mid-tick cancel just
        // leaves candidates queued (idempotent, re-drained next tick).
        if self.cancel.is_cancelled() {
            return Err(GcError::TimelineCancelled);
        }
        let parallelism = pageserver_api::config::scale_count(
            crate::page_store::GC_ACTION_PARALLELISM,
            self.conf.scale_factor,
        );

        // Actions run coarsest-first — drop (-2) → shrink (-1) → trim
        // (>= 0): a dropped fork moots its own shrink/trim work. Each
        // act helper is a phase barrier: every delete of a kind lands
        // before the next kind starts, and all of them before the
        // dequeue below. First error aborts the tick pre-dequeue —
        // in-flight siblings may still complete server-side, which is
        // harmless (idempotent actions, candidates stay queued).
        let mut pages_deleted = self.act_drops(store, &drop_rows, parallelism).await?;
        pages_deleted += self
            .act_shrinks(store, &shrink_rows, applied_cutoff, parallelism)
            .await?;
        pages_deleted += self
            .act_trims(store, trim_blocks_by_fork, applied_cutoff, parallelism)
            .await?;
        result.pages_rows_deleted = pages_deleted;

        // Dequeue strictly after every action succeeded: a candidate
        // may only leave the queue once its garbage is gone. Failure
        // above leaves everything queued for the next tick. Exactly
        // the acted rows go — deferred drop events are not in `acted`
        // and stay on disk (see `dequeue_pending_gc_rows` for why every
        // kind dequeues by exact PK).
        let mut acted = trim_rows;
        acted.extend(drop_rows);
        acted.extend(shrink_rows);
        let removed = store
            .dequeue_pending_gc_rows(self.branch_id, &acted)
            .await
            .map_err(GcError::Persistence)?;
        // Commit only after the dequeue's `?`: a failed tick must leave
        // the floor so its batch re-selects. `max` so the periodic
        // rescan tick's older batch can't lower it.
        {
            let mut progress = self.gc_drain.progress.lock().unwrap();
            progress.deferred_drops = drop_deferred;
            if let Some(next_floor) = next_floor {
                progress.completed_lsn = progress.completed_lsn.max(next_floor);
            }
        }
        result.candidates_drained = acted.len() as u64;
        crate::metrics::GC_CANDIDATES_REMOVED.inc_by(removed);
        crate::metrics::GC_PAGES_ROWS_DELETED.inc_by(result.pages_rows_deleted);
        crate::metrics::GC_DRAIN_TICKS
            .with_label_values(&[if all_due_visited { "drained" } else { "capped" }])
            .inc();

        info!(
            applied_gc_cutoff = %applied_cutoff,
            candidates_drained = result.candidates_drained,
            pages_rows_deleted = result.pages_rows_deleted,
            "gc drain complete"
        );
        Ok(())
    }

    /// Drop actions, forks in parallel (distinct tablets, no ordering
    /// constraint): delete every page of the fork with
    /// `lsn <= due_lsn` (relnode-reuse safe), deduped per fork by
    /// [`Self::drop_targets`]. Returns pages rows deleted.
    async fn act_drops(
        &self,
        store: &Arc<crate::page_store::PageStore>,
        drop_rows: &[crate::page_store::PendingGcRow],
        parallelism: usize,
    ) -> Result<u64, GcError> {
        use futures::{StreamExt as _, TryStreamExt as _};
        let targets = Self::drop_targets(drop_rows);
        let deleted: Vec<u64> = futures::stream::iter(targets.into_iter().map(
            |((spcnode, dbnode, relnode, forknum), due_lsn)| {
                let store = store.clone();
                let branch_id = self.branch_id;
                async move {
                    store
                        .gc_drop_fork(branch_id, spcnode, dbnode, relnode, forknum, due_lsn)
                        .await
                }
            },
        ))
        .buffer_unordered(parallelism)
        .try_collect()
        .await
        .map_err(GcError::Persistence)?;
        Ok(deleted.iter().sum())
    }

    /// Dedup same-tick drop events per fork, keeping the MAX due_lsn:
    /// a re-drop's larger bound supersedes the smaller, so one DELETE
    /// covers every selected drop event. MAX is load-bearing — every
    /// event dequeues after the tick, so a DELETE bounded at the
    /// smaller due_lsn would leak the later incarnation's rows between
    /// the two bounds forever. (Owned targets so the action's stream
    /// futures borrow nothing — `Send` across buffer_unordered.)
    fn drop_targets(
        drop_rows: &[crate::page_store::PendingGcRow],
    ) -> std::collections::HashMap<crate::page_store::GcForkIdent, Lsn> {
        let mut targets: std::collections::HashMap<crate::page_store::GcForkIdent, Lsn> =
            std::collections::HashMap::new();
        for r in drop_rows {
            let due = targets
                .entry((r.spcnode, r.dbnode, r.relnode, r.forknum))
                .or_insert(r.due_lsn);
            *due = (*due).max(r.due_lsn);
        }
        targets
    }

    /// Shrink actions, forks in parallel: delete the fork's pages with
    /// `blknum >=` its size as of `applied_cutoff`. Deduped per fork —
    /// same-fork shrink events in one tick would repeat the same
    /// cutoff-anchored work. Returns pages rows deleted.
    async fn act_shrinks(
        &self,
        store: &Arc<crate::page_store::PageStore>,
        shrink_rows: &[crate::page_store::PendingGcRow],
        applied_cutoff: Lsn,
        parallelism: usize,
    ) -> Result<u64, GcError> {
        use futures::{StreamExt as _, TryStreamExt as _};
        let mut targets: Vec<crate::page_store::GcForkIdent> = shrink_rows
            .iter()
            .map(|r| (r.spcnode, r.dbnode, r.relnode, r.forknum))
            .collect();
        targets.sort_unstable();
        targets.dedup();
        let deleted: Vec<u64> = futures::stream::iter(targets.into_iter().map(
            |(spcnode, dbnode, relnode, forknum)| {
                let store = store.clone();
                let branch_id = self.branch_id;
                async move {
                    store
                        .gc_shrink_fork(
                            branch_id,
                            spcnode,
                            dbnode,
                            relnode,
                            forknum,
                            applied_cutoff,
                        )
                        .await
                }
            },
        ))
        .buffer_unordered(parallelism)
        .try_collect()
        .await
        .map_err(GcError::Persistence)?;
        Ok(deleted.iter().sum())
    }

    /// Trim actions, forks in parallel: per listed block keep the
    /// anchor (newest base `<= applied_cutoff`) and everything above
    /// it, delete below. Returns pages rows deleted.
    async fn act_trims(
        &self,
        store: &Arc<crate::page_store::PageStore>,
        trim_blocks_by_fork: std::collections::HashMap<crate::page_store::GcForkIdent, Vec<i64>>,
        applied_cutoff: Lsn,
        parallelism: usize,
    ) -> Result<u64, GcError> {
        use futures::{StreamExt as _, TryStreamExt as _};
        let deleted: Vec<u64> = futures::stream::iter(trim_blocks_by_fork.into_iter().map(
            |((spcnode, dbnode, relnode, forknum), mut blknums)| {
                let store = store.clone();
                let branch_id = self.branch_id;
                async move {
                    blknums.sort_unstable();
                    blknums.dedup();
                    store
                        .gc_trim_blocks(
                            branch_id,
                            spcnode,
                            dbnode,
                            relnode,
                            forknum,
                            &blknums,
                            applied_cutoff,
                        )
                        .await
                }
            },
        ))
        .buffer_unordered(parallelism)
        .try_collect()
        .await
        .map_err(GcError::Persistence)?;
        Ok(deleted.iter().sum())
    }

    /// Merge the previously-deferred drops with this tick's newly-selected
    /// ones and split them by whether their fork has a materialise job
    /// pending (in flight or queued): idle → act now, busy → keep
    /// deferred. The race the deferral guards, for block B of a fork:
    ///
    ///   delta@100         triggers a (long-running) materialise job for B
    ///   drop@110          queues a drop event (due_lsn 110)
    ///   tick, cutoff 120  acts the drop: DELETEs B's pages <= 110, dequeues it
    ///   job finishes      INSERTs image@100 + a trim candidate (due_lsn 100)
    ///   tick, cutoff 130  trims B: keeps image@100 as the anchor
    ///
    /// image@100 now leaks forever — trim retains it as B's anchor, and
    /// the drop that would delete it (<= 110) is already dequeued. So
    /// don't act a drop event while its fork has a job in flight.
    ///
    /// The same race exists for shrink, but there it is left
    /// unhandled: the leaked image is bounded (at most one per tail
    /// block with a job in flight at the shrink) and invisible to
    /// reads, and any subsequent truncation, drop, or re-extension
    /// deletes it. Deferring shrinks would instead starve a hot fork's
    /// tail reclaim (churn keeps the fork busy). Trim needs no
    /// deferral either: a late image is an ordinary base on a live
    /// block — the block's next trim visit anchors on it or deletes
    /// it.
    ///
    /// A deferred event's retry path is the in-memory list, not
    /// re-selection: the floor may pass its due_lsn the moment the
    /// tick succeeds. Until then the select can still return it, so
    /// the merge dedups by exact PK. Deferral converges — a dropped
    /// fork gets no new jobs.
    ///
    /// Returns (actionable, still deferred).
    fn partition_drops_by_busy_fork(
        previously_deferred: Vec<crate::page_store::PendingGcRow>,
        newly_selected: Vec<crate::page_store::PendingGcRow>,
        busy: &std::collections::HashSet<crate::page_store::GcForkIdent>,
    ) -> (
        Vec<crate::page_store::PendingGcRow>,
        Vec<crate::page_store::PendingGcRow>,
    ) {
        let mut seen = std::collections::HashSet::new();
        let mut act = Vec::new();
        let mut deferred_out = Vec::new();
        for r in previously_deferred.into_iter().chain(newly_selected) {
            if !seen.insert((
                r.bucket_id,
                r.due_lsn,
                r.spcnode,
                r.dbnode,
                r.relnode,
                r.forknum,
                r.blknum,
            )) {
                continue;
            }
            if busy.contains(&(r.spcnode, r.dbnode, r.relnode, r.forknum)) {
                deferred_out.push(r);
            } else {
                act.push(r);
            }
        }
        (act, deferred_out)
    }

    /// Where the select floor advances to after a successful tick, from
    /// the last selected `due_lsn` and whether the tick was uncapped
    /// (returned every due row). `None` leaves the floor unchanged.
    ///
    /// - empty select → `None`;
    /// - uncapped → the cutoff: everything `due_lsn <= cutoff` was
    ///   handled and the insert invariant keeps new candidates above the
    ///   cutoff, so jumping there skips the boundary tie group and the
    ///   not-dequeued deferred / unknown-kind rows next tick;
    /// - capped → `last − 1`: only rows strictly below the last are
    ///   guaranteed returned (the limit can split its tie group), so the
    ///   −1 keeps that group selectable.
    fn next_floor(last_due: Option<Lsn>, all_due_visited: bool, cutoff: Lsn) -> Option<Lsn> {
        match last_due {
            None => None,
            Some(_) if all_due_visited => Some(cutoff),
            Some(due) => Some(Lsn(due.0.saturating_sub(1))),
        }
    }

    /// Partition due candidates by kind (`blknum` doubles as the kind —
    /// see `GC_KIND_DROP` / `GC_KIND_SHRINK`). Trim rows (`blknum >= 0`)
    /// group by fork — N candidates for one block coalesce into one
    /// visit, lossless because the trim anchors at the cutoff, not any
    /// candidate's due_lsn (see `gc_trim_blocks`). An unknown negative
    /// kind is skipped with a `correctness_errors_total` alarm and left
    /// on disk, never dequeued.
    ///
    /// Returns (trim blocks per fork, trim rows, drop rows, shrink rows).
    #[allow(clippy::type_complexity)]
    fn partition_by_kind(
        due: Vec<crate::page_store::PendingGcRow>,
    ) -> (
        std::collections::HashMap<crate::page_store::GcForkIdent, Vec<i64>>,
        Vec<crate::page_store::PendingGcRow>,
        Vec<crate::page_store::PendingGcRow>,
        Vec<crate::page_store::PendingGcRow>,
    ) {
        let mut trim_blocks_by_fork: std::collections::HashMap<
            crate::page_store::GcForkIdent,
            Vec<i64>,
        > = std::collections::HashMap::new();
        let mut trim_rows = Vec::with_capacity(due.len());
        let mut drop_rows = Vec::new();
        let mut shrink_rows = Vec::new();
        for row in due {
            match row.blknum {
                b if b >= 0 => {
                    trim_blocks_by_fork
                        .entry((row.spcnode, row.dbnode, row.relnode, row.forknum))
                        .or_default()
                        .push(b);
                    trim_rows.push(row);
                }
                crate::page_store::GC_KIND_DROP => drop_rows.push(row),
                crate::page_store::GC_KIND_SHRINK => shrink_rows.push(row),
                other => {
                    tracing::error!(
                        blknum = other,
                        due_lsn = %row.due_lsn,
                        "pending_gc candidate of unknown kind; left on disk, never dequeued"
                    );
                    crate::metrics::CORRECTNESS_ERRORS
                        .with_label_values(&["gc_unknown_candidate_kind"])
                        .inc();
                }
            }
        }
        (trim_blocks_by_fork, trim_rows, drop_rows, shrink_rows)
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
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::page_store::{GC_KIND_DROP, GC_KIND_SHRINK, PendingGcRow};

    fn row(blknum: i64) -> PendingGcRow {
        PendingGcRow {
            bucket_id: 0,
            spcnode: 1663,
            dbnode: 5,
            relnode: 16400,
            forknum: 0,
            blknum,
            due_lsn: Lsn(100),
        }
    }

    /// Scenario: the queue holds a row of a kind this version doesn't
    /// know. The partition must skip + alarm it — never panic on table
    /// contents — while still routing the known kinds.
    #[test]
    fn partition_skips_and_alarms_unknown_kind() {
        let alarms = || {
            crate::metrics::CORRECTNESS_ERRORS
                .with_label_values(&["gc_unknown_candidate_kind"])
                .get()
        };
        let before = alarms();
        let (trim_blocks, trim_rows, drop_rows, shrink_rows) = Timeline::partition_by_kind(vec![
            row(7),
            row(GC_KIND_DROP),
            row(GC_KIND_SHRINK),
            row(-3),
        ]);
        assert_eq!(trim_rows.len(), 1);
        assert_eq!(trim_blocks.values().flatten().count(), 1);
        assert_eq!(drop_rows.len(), 1);
        assert_eq!(shrink_rows.len(), 1);
        assert_eq!(alarms() - before, 1, "the unknown kind alarms once");
    }

    /// The floor jumps to the cutoff on an uncapped tick (everything due
    /// handled), holds at `last − 1` on a capped one (its tie group may
    /// be split), and is left unchanged on an empty select.
    #[test]
    fn next_floor_uncapped_jumps_to_cutoff_capped_holds_at_last() {
        let cutoff = Lsn(100);
        assert_eq!(
            Timeline::next_floor(None, true, cutoff),
            None,
            "empty select"
        );
        assert_eq!(
            Timeline::next_floor(Some(Lsn(80)), true, cutoff),
            Some(Lsn(100)),
            "uncapped → cutoff"
        );
        assert_eq!(
            Timeline::next_floor(Some(Lsn(80)), false, cutoff),
            Some(Lsn(79)),
            "capped → last − 1"
        );
    }

    /// Scenario: a drop event was deferred last tick (fork busy) and —
    /// the floor not yet past its due_lsn — the select returns it
    /// again this tick. The merge must dedup the two copies (acting a
    /// drop twice is idempotent, but dequeueing a PK twice would
    /// under-report and double-count metrics), route rows of busy
    /// forks to the deferred list, and release a row whose fork went
    /// quiet.
    #[test]
    fn partition_drops_by_busy_fork_dedups() {
        use crate::page_store::GC_KIND_DROP;
        let at = |relnode: i64, due: u64| {
            let mut r = row(GC_KIND_DROP);
            r.relnode = relnode;
            r.due_lsn = Lsn(due);
            r
        };
        let busy_fork = at(16400, 110); // fork 16400 still busy
        let freed = at(16401, 120); // was deferred; fork now quiet
        let fresh = at(16402, 130); // newly selected, quiet fork
        let busy: std::collections::HashSet<_> = [(1663, 5, 16400, 0i16)].into();

        let (act, deferred) = Timeline::partition_drops_by_busy_fork(
            vec![busy_fork, freed],
            vec![busy_fork, fresh], // busy_fork re-selected: must dedup
            &busy,
        );
        let rels = |v: &[PendingGcRow]| v.iter().map(|r| r.relnode).collect::<Vec<_>>();
        assert_eq!(rels(&act), vec![16401, 16402], "freed + fresh act");
        assert_eq!(
            rels(&deferred),
            vec![16400],
            "busy fork stays deferred, once"
        );
    }

    /// Scenario: same fork, two distinct drop events (relnode reuse —
    /// different due_lsns). Both are real rows; the PK dedup must NOT
    /// collapse them (each needs its own dequeue), and both follow the
    /// fork's busy verdict together.
    #[test]
    fn partition_drops_by_busy_fork_keeps_distinct_events() {
        use crate::page_store::GC_KIND_DROP;
        let at = |due: u64| {
            let mut r = row(GC_KIND_DROP);
            r.due_lsn = Lsn(due);
            r
        };
        let (act, deferred) = Timeline::partition_drops_by_busy_fork(
            vec![at(110)],
            vec![at(150)],
            &Default::default(),
        );
        assert_eq!(act.len(), 2, "distinct due_lsns are distinct rows");
        assert!(deferred.is_empty());
    }

    /// Scenario: relnode reused and re-dropped within one tick — two
    /// drop events for the same fork. The dedup must keep the MAX
    /// due_lsn regardless of row order: both events dequeue after the
    /// tick, so a DELETE bounded at the smaller due_lsn would leak the
    /// later incarnation's rows between the two bounds forever.
    #[test]
    fn drop_targets_dedup_keeps_max_due_lsn() {
        let at = |blknum: i64, relnode: i64, due: u64| {
            let mut r = row(blknum);
            r.relnode = relnode;
            r.due_lsn = Lsn(due);
            r
        };
        let early = at(GC_KIND_DROP, 16400, 110);
        let late = at(GC_KIND_DROP, 16400, 150);
        let other = at(GC_KIND_DROP, 16499, 120);
        for rows in [
            [early, late, other],
            [late, early, other], // insertion order must not matter
        ] {
            let targets = Timeline::drop_targets(&rows);
            assert_eq!(targets.len(), 2);
            assert_eq!(targets[&(1663, 5, 16400, 0)], Lsn(150));
            assert_eq!(targets[&(1663, 5, 16499, 0)], Lsn(120));
        }
    }
}
