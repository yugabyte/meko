//! yb-amp row-store GC drain (Part 1): the periodic trim drain plus its
//! cutoff computation, added by the GC work. `impl Timeline` here reaches
//! Timeline's private fields because `gc` is a descendant module of
//! `timeline`.

use super::*;

impl Timeline {
    /// yb-amp row-store GC, Part 1 (trim) only in this cut.
    /// One iteration = compute & publish the cutoff (generation-fenced,
    /// monotone), wait out in-flight reads holding the old cutoff, then
    /// drain due `pending_gc` candidates: trim each block down to its
    /// anchor and dequeue. Ordering — action before dequeue — plus
    /// idempotent trims give crash safety without a cross-table
    /// transaction: a crash in between re-drains the same candidates
    /// into a no-op.
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
        let store = self.gc_page_store.clone();

        let cutoff = self.compute_gc_cutoff(&store).await?;
        let applied = self.publish_and_await_readers(&store, cutoff).await?;
        self.drain_due_candidates(&store, applied, &mut result)
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

    /// Drain due `pending_gc` candidates at `applied`: select
    /// oldest-first, trim each block down to the cutoff's anchor (forks
    /// in parallel), then dequeue. Ordering — action before dequeue —
    /// plus idempotent trims give crash safety without a cross-table
    /// transaction: a crash in between re-drains the same candidates
    /// into a no-op.
    async fn drain_due_candidates(
        &self,
        store: &Arc<crate::page_store::PageStore>,
        applied: Lsn,
        result: &mut GcResult,
    ) -> Result<(), GcError> {
        // TODO(gc_completed_lsn): pass the persisted drain watermark as
        // a lower bound so this scan seeks past drained-candidate
        // tombstones instead of skimming them every tick.
        let drain_limit = self.get_gc_drain_limit();
        let due = store
            .select_due_pending_gc(self.branch_id, applied, drain_limit)
            .await
            .map_err(GcError::Persistence)?;
        crate::metrics::GC_APPLIED_CUTOFF_LSN.set(applied.0);
        // Under the limit ⇒ every due candidate was returned, so the
        // dequeue may range-sweep instead of naming blocks.
        let all_due_visited = due.len() < drain_limit;
        // Residual drain lag: the due span a capped tick couldn't
        // reach; 0 when everything due was drained (incl. empty
        // queue — the gauge must reset or it freezes at a stale
        // value).
        crate::metrics::GC_DRAIN_LAG_BYTES.set(match due.last() {
            Some(r) if !all_due_visited => applied.0.saturating_sub(r.due_lsn.0),
            _ => 0,
        });
        if due.is_empty() {
            crate::metrics::GC_DRAIN_TICKS
                .with_label_values(&["empty"])
                .inc();
            return Ok(());
        }
        // All selected rows, before trim routing.
        crate::metrics::GC_CANDIDATES_READ.inc_by(due.len() as u64);

        let by_fork = Self::group_due_by_fork(&due);

        // Fork trims in parallel (forks = distinct tablets, no
        // ordering constraint). `try_collect` is the phase barrier:
        // every trim lands before the dequeue below. First error
        // aborts the tick pre-dequeue — dropped siblings may still
        // complete server-side, which is harmless (idempotent trims,
        // candidates stay queued). Cancel is checked once, up front.
        if self.cancel.is_cancelled() {
            return Err(GcError::TimelineCancelled);
        }
        let trim_parallelism = pageserver_api::config::scale_count(
            crate::page_store::GC_ACTION_PARALLELISM,
            self.conf.scale_factor,
        );
        use futures::{StreamExt as _, TryStreamExt as _};
        let deleted_per_fork: Vec<u64> = futures::stream::iter(by_fork.into_iter().map(
            |((spcnode, dbnode, relnode, forknum), mut blknums)| {
                let store = store.clone();
                let branch_id = self.branch_id;
                async move {
                    blknums.sort_unstable();
                    blknums.dedup();
                    store
                        .gc_trim_blocks(
                            branch_id, spcnode, dbnode, relnode, forknum, &blknums, applied,
                        )
                        .await
                }
            },
        ))
        .buffer_unordered(trim_parallelism)
        .try_collect()
        .await
        .map_err(GcError::Persistence)?;
        result.pages_rows_deleted = deleted_per_fork.iter().sum();

        // Dequeue strictly after every action succeeded: a candidate
        // may only leave the queue once its garbage is gone. Failure
        // above leaves everything queued for the next tick.
        // Collapse-on-visit: also clears the visited blocks' duplicate
        // candidates <= cutoff (and in sweep mode, the whole due range)
        // — see `dequeue_pending_gc` for the two modes + safety bounds.
        let removed = store
            .dequeue_pending_gc(self.branch_id, &due, applied, all_due_visited)
            .await
            .map_err(GcError::Persistence)?;
        result.candidates_drained = due.len() as u64;
        crate::metrics::GC_CANDIDATES_REMOVED.inc_by(removed);
        crate::metrics::GC_PAGES_ROWS_DELETED.inc_by(result.pages_rows_deleted);
        crate::metrics::GC_DRAIN_TICKS
            .with_label_values(&[if all_due_visited { "drained" } else { "capped" }])
            .inc();

        info!(
            applied_gc_cutoff = %applied,
            candidates_drained = result.candidates_drained,
            pages_rows_deleted = result.pages_rows_deleted,
            "gc drain complete"
        );
        Ok(())
    }

    /// Group due candidates by fork for trimming. N candidates for one
    /// block (one per base) coalesce into one visit, and the trim is
    /// greedy — it deletes to the CUTOFF's anchor, not any candidate's
    /// due_lsn — so re-based blocks are collected in full either way.
    ///
    /// Part 1 emits only trim candidates (`blknum >= 0`); the drop (`-2`)
    /// / shrink (`-1`) sentinels are a Part 2/3 concern. Their absence is
    /// a Part-1 invariant (this branch's own writer never emits them, and
    /// a stale-generation PS is fenced out at `publish_gc_cutoff` before
    /// it can drain), asserted here — this assert is where Part 2/3 will
    /// branch to route sentinels instead.
    fn group_due_by_fork(
        due: &[crate::page_store::PendingGcRow],
    ) -> std::collections::HashMap<crate::page_store::GcForkIdent, Vec<i64>> {
        let mut by_fork: std::collections::HashMap<crate::page_store::GcForkIdent, Vec<i64>> =
            std::collections::HashMap::new();
        for row in due {
            assert!(
                row.blknum >= 0,
                "Part 1 GC drain saw a drop/shrink sentinel (blknum={}); \
                 sentinel routing lands in Part 2/3",
                row.blknum
            );
            by_fork
                .entry((row.spcnode, row.dbnode, row.relnode, row.forknum))
                .or_default()
                .push(row.blknum);
        }
        by_fork
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
