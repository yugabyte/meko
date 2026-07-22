//! PageServer GC: the periodic candidate drain (drop / shrink / trim)
//! plus its cutoff computation. `impl Timeline` here reaches
//! Timeline's private fields because `gc` is a descendant module of
//! `timeline`.

use super::*;

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
        let store = self.gc_page_store.clone();

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
    /// - **select**: rows with `due_lsn <= applied_cutoff`,
    ///   oldest-first, capped at `gc_drain_limit`;
    /// - **act**: drop → shrink → trim, coarsest-first (a dropped fork
    ///   moots its own shrink/trim work), forks parallel within a kind;
    /// - **dequeue**, only after every action succeeded: with
    ///   idempotent actions that ordering is crash-safe without a
    ///   cross-table transaction — a crash in between re-drains the
    ///   same candidates into a no-op.
    async fn drain_due_candidates(
        &self,
        store: &Arc<crate::page_store::PageStore>,
        applied_cutoff: Lsn,
        result: &mut GcResult,
    ) -> Result<(), GcError> {
        // TODO(gc_completed_lsn): pass the persisted drain watermark as
        // a lower bound so this scan seeks past drained-candidate
        // tombstones instead of skimming them every tick.
        let drain_limit = self.get_gc_drain_limit();
        let due = store
            .select_due_pending_gc(self.branch_id, applied_cutoff, drain_limit)
            .await
            .map_err(GcError::Persistence)?;
        crate::metrics::GC_APPLIED_CUTOFF_LSN.set(applied_cutoff.0);
        // Under the limit ⇒ every due candidate was returned, so the
        // dequeue may range-sweep instead of naming blocks.
        let all_due_visited = due.len() < drain_limit;
        // Residual drain lag: the due span a capped tick couldn't
        // reach; 0 when everything due was drained (incl. empty
        // queue — the gauge must reset or it freezes at a stale
        // value).
        crate::metrics::GC_DRAIN_LAG_BYTES.set(match due.last() {
            Some(r) if !all_due_visited => applied_cutoff.0.saturating_sub(r.due_lsn.0),
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

        let (trim_blocks_by_fork, trim_rows, drop_rows, shrink_rows) = Self::partition_by_kind(due);

        let drop_rows = self.defer_busy_drop_events(drop_rows);

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
        // above leaves everything queued for the next tick. Trim rows:
        // collapse-on-visit / sweep — see `dequeue_gc_trim_rows` for the
        // two modes + safety bounds. Drop/shrink events: by exact
        // PK, never a cutoff-range sweep — a drop only covers <= its
        // due_lsn, so a same-fork drop/shrink event still queued above the
        // cutoff must survive to its own tick.
        let mut removed = store
            .dequeue_gc_trim_rows(self.branch_id, &trim_rows, applied_cutoff, all_due_visited)
            .await
            .map_err(GcError::Persistence)?;
        let mut drop_shrink_events = drop_rows;
        drop_shrink_events.extend(shrink_rows);
        removed += store
            .dequeue_gc_drop_shrink_events(self.branch_id, &drop_shrink_events)
            .await
            .map_err(GcError::Persistence)?;
        result.candidates_drained = (trim_rows.len() + drop_shrink_events.len()) as u64;
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

    /// Hold back drop events whose fork has a materialise job pending
    /// (in flight or queued). The race, for block B of a fork:
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
    /// Returns the actionable drops; deferred ones are neither acted
    /// nor dequeued (exact-PK), so they re-try next tick and converge
    /// (a dropped fork gets no new jobs). The busy-fork snapshot is
    /// paid only on the rare tick that has drop events.
    fn defer_busy_drop_events(
        &self,
        drop_rows: Vec<crate::page_store::PendingGcRow>,
    ) -> Vec<crate::page_store::PendingGcRow> {
        if drop_rows.is_empty() {
            return drop_rows;
        }
        let busy = self
            .page_writer
            .as_ref()
            .map(|pw| pw.forks_with_pending_materialise())
            .unwrap_or_default();
        let (drop_act, drop_deferred): (Vec<_>, Vec<_>) = drop_rows
            .into_iter()
            .partition(|r| !busy.contains(&(r.spcnode, r.dbnode, r.relnode, r.forknum)));
        if !drop_deferred.is_empty() {
            crate::metrics::GC_DROP_DEFERRED.inc_by(drop_deferred.len() as u64);
            info!(
                drop_deferred = drop_deferred.len(),
                "gc: deferred drop events whose fork has a pending materialise job"
            );
        }
        drop_act
    }

    /// Partition due candidates by kind (`blknum` doubles as the kind —
    /// see `GC_KIND_DROP` / `GC_KIND_SHRINK`). Trim rows (`blknum >= 0`)
    /// group by fork — N candidates for one block coalesce into one
    /// visit, lossless because the trim anchors at the cutoff, not any
    /// candidate's due_lsn (see `gc_trim_blocks`). An unknown negative
    /// kind (a newer version's row after a rollback, or corruption) is
    /// skipped with a `correctness_errors_total` alarm.
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
                        "pending_gc candidate of unknown kind; leaving it queued"
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
    /// know (a newer version's row after a rollback, or corruption).
    /// The partition must skip + alarm it — never panic on table
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
