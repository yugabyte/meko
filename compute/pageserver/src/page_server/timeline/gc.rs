//! PageServer GC: the periodic candidate drain (drop / shrink / trim)
//! plus its cutoff computation. `impl Timeline` here reaches
//! Timeline's private fields because `gc` is a descendant module of
//! `timeline`.

use super::*;

/// One tick's due candidates split by kind: trim blocks grouped by fork
/// (`blknum >= 0`) plus the drop (`-2`) and shrink (`-1`) sentinel rows.
/// Produced by [`Timeline::partition_by_kind`], consumed by
/// [`Timeline::act`].
struct DuePartition {
    trim_blocks_by_fork: std::collections::HashMap<crate::page_store::GcForkIdent, Vec<i64>>,
    drops: Vec<crate::page_store::PendingGcRow>,
    shrinks: Vec<crate::page_store::PendingGcRow>,
}

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
    /// live leases, child fork points (from pageserver_branch_state — children
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
    /// - **select**: rows with `floor < due_lsn <= applied_cutoff`,
    ///   oldest-first, capped at `gc_drain_limit` — the floor seeks the
    ///   scan past rows earlier ticks already handled;
    /// - **act**: drop → shrink → trim, coarsest-first (a dropped fork
    ///   moots its own shrink/trim work), forks parallel within a kind.
    ///   All drops are acted, busy fork or not;
    /// - **dequeue**, only after every action succeeded: trim blocks
    ///   range-sweep their candidates in `(floor, cutoff]` (duplicates
    ///   coalesce); drop/shrink clear by their PK `(ident, due_lsn)`. A
    ///   busy-fork drop is acted but left queued so a later tick re-acts
    ///   it, sweeping any image a straggling job inserts after the delete
    ///   (a dropped fork gets no new jobs, so a floor reset converges it).
    ///   Idempotent actions make act-before-dequeue crash-safe;
    /// - **advance the floor**, strictly after the dequeue: an error
    ///   before this point returns via `?` leaving it unchanged, so the
    ///   next tick re-selects. Uncapped → the cutoff (everything
    ///   `<= cutoff` handled); capped → `last selected due_lsn − 1` (the
    ///   −1 keeps a limit-split tie group selectable).
    ///
    /// Every `gc_queue_floor_reset_interval` a tick resets the floor to
    /// 0 to sweep anything stranded below it — a clamp-race trim
    /// straggler or a still-queued busy-fork drop.
    async fn drain_due_candidates(
        &self,
        store: &Arc<crate::page_store::PageStore>,
        applied_cutoff: Lsn,
        result: &mut GcResult,
    ) -> Result<(), GcError> {
        let drain_limit = self.get_gc_drain_limit();
        let (floor, is_reset) = self.pick_drain_floor();
        let due = store
            .gc_select_due_candidates(self.branch_id, floor, applied_cutoff, drain_limit)
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
        // All selected rows, before trim routing.
        crate::metrics::GC_CANDIDATES_READ.inc_by(due.len() as u64);

        let Some(last_due) = due.last().map(|r| r.due_lsn) else {
            crate::metrics::GC_DRAIN_TICKS
                .with_label_values(&["empty"])
                .inc();
            // An empty reset tick found nothing stranded below the floor,
            // so the reset succeeded — stamp it (see pick_drain_floor).
            if is_reset {
                self.gc_drain.progress.lock().unwrap().last_floor_reset =
                    Some(std::time::Instant::now());
            }
            return Ok(());
        };
        // Where the floor advances after this tick: uncapped → the cutoff
        // (everything `<= cutoff` handled; the insert clamp keeps new
        // candidates above it, so this skips the boundary tie group and
        // the not-dequeued deferred/unknown-kind rows next tick); capped →
        // `last − 1` (the limit can split the last tie group, so keep it
        // selectable).
        let next_floor = if all_due_visited {
            applied_cutoff
        } else {
            Lsn(last_due.0.saturating_sub(1))
        };
        let partition = Self::partition_by_kind(due);

        // Forks with a materialise job pending, snapshotted BEFORE acting
        // so a job in flight during a drop's delete is always caught: a
        // busy-fork drop is acted but not dequeued (see `act`), so a
        // straggling image INSERT after the delete can't leak. Paid only
        // when drops are present.
        let busy = if partition.drops.is_empty() {
            Default::default()
        } else {
            self.page_writer
                .as_ref()
                .map(|pw| pw.forks_with_pending_materialise())
                .unwrap_or_default()
        };

        // Cancel is checked once, up front: a mid-tick cancel just
        // leaves candidates queued (idempotent, re-drained next tick).
        if self.cancel.is_cancelled() {
            return Err(GcError::TimelineCancelled);
        }
        let parallelism = pageserver_api::config::scale_count(
            crate::page_store::GC_ACTION_PARALLELISM,
            self.conf.scale_factor,
        );

        let (pages_deleted, trim_to_dequeue, drop_shrink_to_dequeue) = self
            .act(store, partition, &busy, applied_cutoff, parallelism)
            .await?;
        result.pages_rows_deleted = pages_deleted;

        // Dequeue strictly after every action succeeded, then commit the
        // floor. An error before this point returns via `?` leaving the
        // floor unchanged, so the next tick re-selects the same batch.
        // Trim blocks range-sweep `(floor, cutoff]`; drop/shrink clear by
        // their PK `(ident, due_lsn)`.
        let removed_trim = store
            .gc_dequeue_trim_candidates(self.branch_id, &trim_to_dequeue, floor, applied_cutoff)
            .await
            .map_err(GcError::Persistence)?;
        let removed_drop_shrink = store
            .gc_dequeue_drop_shrink_candidates(self.branch_id, &drop_shrink_to_dequeue)
            .await
            .map_err(GcError::Persistence)?;
        let removed = removed_trim + removed_drop_shrink;
        // Monotone `max` so a floor-reset tick's older batch (it scans
        // from 0) can't lower a higher floor. Stamp the reset only now the
        // dequeue succeeded, so a failed reset tick retries next tick
        // rather than burning an interval (see pick_drain_floor).
        let mut progress = self.gc_drain.progress.lock().unwrap();
        progress.floor = progress.floor.max(next_floor);
        if is_reset {
            progress.last_floor_reset = Some(std::time::Instant::now());
        }
        result.candidates_dequeued = removed;
        crate::metrics::GC_CANDIDATES_DEQUEUED.inc_by(removed);
        crate::metrics::GC_PAGES_ROWS_DELETED.inc_by(result.pages_rows_deleted);
        crate::metrics::GC_DRAIN_TICKS
            .with_label_values(&[if all_due_visited { "drained" } else { "capped" }])
            .inc();

        info!(
            applied_gc_cutoff = %applied_cutoff,
            candidates_dequeued = result.candidates_dequeued,
            pages_rows_deleted = result.pages_rows_deleted,
            "gc drain complete"
        );
        Ok(())
    }

    /// Act on every due candidate, coarsest-first — drop (`-2`) → shrink
    /// (`-1`) → trim (`>= 0`), forks parallel within a kind: a dropped
    /// fork moots its own shrink/trim work. Each act helper is a phase
    /// barrier; a first error aborts before the caller dequeues (in-flight
    /// siblings may still land server-side — harmless, idempotent, they
    /// stay queued). All drops are acted, busy fork or not.
    ///
    /// Returns `(pages rows deleted, trim blocks to range-sweep, drop/shrink
    /// rows to dequeue by exact PK)` — tying the dequeue sets to what was
    /// acted. `busy` must be snapshotted by the caller before this runs.
    async fn act(
        &self,
        store: &Arc<crate::page_store::PageStore>,
        due: DuePartition,
        busy: &std::collections::HashSet<crate::page_store::GcForkIdent>,
        applied_cutoff: Lsn,
        parallelism: usize,
    ) -> Result<
        (
            u64,
            Vec<crate::page_store::GcBlockIdent>,
            Vec<crate::page_store::GcDropShrinkCandidateId>,
        ),
        GcError,
    > {
        let DuePartition {
            trim_blocks_by_fork,
            drops,
            shrinks,
        } = due;
        // Captured before act_trims consumes the map.
        let trim_blocks: Vec<crate::page_store::GcBlockIdent> = trim_blocks_by_fork
            .iter()
            .flat_map(|(&(s, d, r, f), blks)| blks.iter().map(move |&b| (s, d, r, f, b)))
            .collect();

        let mut pages_deleted = self.act_drops(store, &drops, parallelism).await?;
        pages_deleted += self.act_shrinks(store, &shrinks, parallelism).await?;
        pages_deleted += self
            .act_trims(store, trim_blocks_by_fork, applied_cutoff, parallelism)
            .await?;

        let (trim_to_dequeue, drop_shrink_to_dequeue, deferred_forks) =
            Self::get_dequeue_targets(trim_blocks, &shrinks, &drops, busy);
        // A busy fork's drop is acted but left queued; it re-selects (and
        // re-logs) only on floor resets, so a fork recurring here across
        // resets means its materialise job is wedged, pinning the drop's
        // reclamation. info! (not debug): the whole point is prod
        // visibility, and the volume is one line per fork per reset.
        if !deferred_forks.is_empty() {
            let sample = &deferred_forks[..deferred_forks.len().min(8)];
            tracing::info!(
                count = deferred_forks.len(),
                forks = ?sample,
                "gc: busy-fork drops left queued (materialise job in flight)"
            );
        }
        Ok((pages_deleted, trim_to_dequeue, drop_shrink_to_dequeue))
    }

    /// Split what [`Self::act`] cleared into its dequeue sets: trim blocks
    /// (range-swept over `(floor, cutoff]`) and drop/shrink candidates
    /// (by their PK). Busy-fork drops stay queued for a later tick; their
    /// forks are returned only for the breadcrumb log.
    fn get_dequeue_targets(
        trim_blocks: Vec<crate::page_store::GcBlockIdent>,
        shrink_rows: &[crate::page_store::PendingGcRow],
        drop_rows: &[crate::page_store::PendingGcRow],
        busy: &std::collections::HashSet<crate::page_store::GcForkIdent>,
    ) -> (
        Vec<crate::page_store::GcBlockIdent>,
        Vec<crate::page_store::GcDropShrinkCandidateId>,
        Vec<crate::page_store::GcForkIdent>,
    ) {
        let fork =
            |r: &crate::page_store::PendingGcRow| (r.spcnode, r.dbnode, r.relnode, r.forknum);
        let is_busy = |r: &crate::page_store::PendingGcRow| busy.contains(&fork(r));
        let candidate_id = |r: &crate::page_store::PendingGcRow| {
            (
                (r.spcnode, r.dbnode, r.relnode, r.forknum, r.blknum),
                r.due_lsn,
            )
        };
        let mut drop_shrink_targets: Vec<crate::page_store::GcDropShrinkCandidateId> =
            shrink_rows.iter().map(candidate_id).collect();
        drop_shrink_targets.extend(drop_rows.iter().filter(|r| !is_busy(r)).map(candidate_id));
        let deferred_forks = drop_rows.iter().filter(|r| is_busy(r)).map(fork).collect();
        (trim_blocks, drop_shrink_targets, deferred_forks)
    }

    /// Run `f` over each of `items` with `parallelism`-way concurrency and
    /// sum the per-call deleted counts. Each future must own its captures
    /// (so it's `Send` across `buffer_unordered`) — clone the `Arc` store
    /// inside the closure, before its `async move`.
    async fn run_parallel_actions<T, Fut>(
        items: impl Iterator<Item = T>,
        parallelism: usize,
        f: impl Fn(T) -> Fut,
    ) -> Result<u64, GcError>
    where
        Fut: std::future::Future<Output = anyhow::Result<u64>>,
    {
        use futures::{StreamExt as _, TryStreamExt as _};
        let deleted: Vec<u64> = futures::stream::iter(items.map(f))
            .buffer_unordered(parallelism)
            .try_collect()
            .await
            .map_err(GcError::Persistence)?;
        Ok(deleted.into_iter().sum())
    }

    /// Drop actions, one per candidate, forks in parallel (distinct
    /// tablets, no ordering constraint): delete every page of the fork
    /// with `lsn <= due_lsn` (relnode-reuse safe). Not deduped per fork —
    /// exact-PK dequeue clears only the acted candidate, so acting each
    /// one is correct; two same-fork drops in a tick just run two
    /// (subsumed) deletes. Returns pages rows deleted.
    async fn act_drops(
        &self,
        store: &Arc<crate::page_store::PageStore>,
        drop_rows: &[crate::page_store::PendingGcRow],
        parallelism: usize,
    ) -> Result<u64, GcError> {
        let targets: Vec<(crate::page_store::GcForkIdent, Lsn)> = drop_rows
            .iter()
            .map(|r| ((r.spcnode, r.dbnode, r.relnode, r.forknum), r.due_lsn))
            .collect();
        let branch_id = self.branch_id;
        Self::run_parallel_actions(
            targets.into_iter(),
            parallelism,
            |((spcnode, dbnode, relnode, forknum), due_lsn)| {
                let store = store.clone();
                async move {
                    store
                        .gc_act_drop_fork(branch_id, spcnode, dbnode, relnode, forknum, due_lsn)
                        .await
                }
            },
        )
        .await
    }

    /// Shrink actions, one delete per candidate. Returns rows deleted.
    async fn act_shrinks(
        &self,
        store: &Arc<crate::page_store::PageStore>,
        shrink_rows: &[crate::page_store::PendingGcRow],
        parallelism: usize,
    ) -> Result<u64, GcError> {
        let targets = Self::shrink_targets(shrink_rows);
        let branch_id = self.branch_id;
        Self::run_parallel_actions(
            targets.into_iter(),
            parallelism,
            |((spcnode, dbnode, relnode, forknum), relsize, due_lsn)| {
                let store = store.clone();
                async move {
                    store
                        .gc_act_shrink_fork(
                            branch_id, spcnode, dbnode, relnode, forknum, relsize, due_lsn,
                        )
                        .await
                }
            },
        )
        .await
    }

    /// Build one shrink target per candidate, each with its own
    /// `(relsize, due_lsn)` — never coalesced per fork (that would reclaim
    /// a block re-extended between two truncations; see the trap on
    /// `gc_act_shrink_fork`). A candidate missing its stored `relsize` is
    /// skipped and alarmed, not acted, so the drain isn't wedged.
    fn shrink_targets(
        shrink_rows: &[crate::page_store::PendingGcRow],
    ) -> Vec<(crate::page_store::GcForkIdent, i64, Lsn)> {
        shrink_rows
            .iter()
            .filter_map(|r| match r.relsize {
                Some(relsize) => Some((
                    (r.spcnode, r.dbnode, r.relnode, r.forknum),
                    relsize,
                    r.due_lsn,
                )),
                None => {
                    tracing::error!(
                        relnode = r.relnode,
                        forknum = r.forknum,
                        due_lsn = %r.due_lsn,
                        "pending_gc shrink candidate without a stored relsize; skipped"
                    );
                    crate::metrics::CORRECTNESS_ERRORS
                        .with_label_values(&["gc_shrink_missing_relsize"])
                        .inc();
                    None
                }
            })
            .collect()
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
        let branch_id = self.branch_id;
        Self::run_parallel_actions(
            trim_blocks_by_fork.into_iter(),
            parallelism,
            |((spcnode, dbnode, relnode, forknum), blknums)| {
                let store = store.clone();
                // blknums already sorted + deduped in partition_by_kind.
                async move {
                    store
                        .gc_act_trim_blocks(
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
        )
        .await
    }

    /// Partition due candidates by kind (`blknum` doubles as the kind —
    /// see `GC_KIND_DROP` / `GC_KIND_SHRINK`). Trim rows (`blknum >= 0`)
    /// group by fork — N candidates for one block coalesce into one
    /// visit, lossless because the trim anchors at the cutoff, not any
    /// candidate's due_lsn (see `gc_act_trim_blocks`). An unknown negative
    /// kind is skipped with a `correctness_errors_total` alarm and left
    /// on disk, never dequeued.
    ///
    /// Each fork's block list is deduped here (a hot block has many due
    /// candidates), so neither the trim action nor the flattened dequeue
    /// bind array carries duplicate blknums.
    ///
    /// Returns the split as a [`DuePartition`].
    fn partition_by_kind(due: Vec<crate::page_store::PendingGcRow>) -> DuePartition {
        let mut trim_blocks_by_fork: std::collections::HashMap<
            crate::page_store::GcForkIdent,
            Vec<i64>,
        > = std::collections::HashMap::new();
        let mut drop_rows = Vec::new();
        let mut shrink_rows = Vec::new();
        for row in due {
            match row.blknum {
                b if b >= 0 => {
                    trim_blocks_by_fork
                        .entry((row.spcnode, row.dbnode, row.relnode, row.forknum))
                        .or_default()
                        .push(b);
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
        for blknums in trim_blocks_by_fork.values_mut() {
            blknums.sort_unstable();
            blknums.dedup();
        }
        DuePartition {
            trim_blocks_by_fork,
            drops: drop_rows,
            shrinks: shrink_rows,
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
    use crate::page_store::{GC_KIND_DROP, GC_KIND_SHRINK, PendingGcRow};

    fn row(blknum: i64) -> PendingGcRow {
        PendingGcRow {
            spcnode: 1663,
            dbnode: 5,
            relnode: 16400,
            forknum: 0,
            blknum,
            due_lsn: Lsn(100),
            relsize: None,
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
        let p = Timeline::partition_by_kind(vec![
            row(7),
            row(GC_KIND_DROP),
            row(GC_KIND_SHRINK),
            row(-3),
        ]);
        assert_eq!(p.trim_blocks_by_fork.values().flatten().count(), 1);
        assert_eq!(p.drops.len(), 1);
        assert_eq!(p.shrinks.len(), 1);
        assert_eq!(alarms() - before, 1, "the unknown kind alarms once");
    }

    /// `get_dequeue_targets` splits the acted candidates into trim blocks
    /// (range-swept) and drop/shrink candidates (exact `(ident, due_lsn)`),
    /// excluding drops whose fork is busy (acted, left queued) and
    /// reporting those deferred forks.
    #[test]
    fn get_dequeue_targets_splits_trim_and_drop_shrink() {
        let mkrow = |relnode: i64, blknum: i64| PendingGcRow {
            spcnode: 1663,
            dbnode: 5,
            relnode,
            forknum: 0,
            blknum,
            due_lsn: Lsn(10),
            relsize: None,
        };
        let mut busy = std::collections::HashSet::new();
        busy.insert((1663, 5, 16400, 0)); // fork of the busy drop below

        let (trim_blocks, exact_rows, deferred_forks) = Timeline::get_dequeue_targets(
            vec![(1663, 5, 16403, 0, 7)],    // a visited trim block
            &[mkrow(16402, GC_KIND_SHRINK)], // shrink candidate
            &[mkrow(16400, GC_KIND_DROP), mkrow(16401, GC_KIND_DROP)], // busy, then idle
            &busy,
        );

        assert_eq!(trim_blocks, vec![(1663, 5, 16403, 0, 7)]);
        assert_eq!(
            exact_rows,
            vec![
                ((1663, 5, 16402, 0, GC_KIND_SHRINK), Lsn(10)), // shrink candidate
                ((1663, 5, 16401, 0, GC_KIND_DROP), Lsn(10)),   // idle drop; busy 16400 excluded
            ]
        );
        assert_eq!(
            deferred_forks,
            vec![(1663, 5, 16400, 0)],
            "the busy fork's drop is deferred and reported"
        );
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
        let cand = |relsize: i64, due: u64| PendingGcRow {
            spcnode: 1663,
            dbnode: 5,
            relnode: 16400,
            forknum: 0,
            blknum: GC_KIND_SHRINK,
            due_lsn: Lsn(due),
            relsize: Some(relsize),
        };
        let targets = Timeline::shrink_targets(&[cand(2, 50), cand(3, 80)]);
        assert_eq!(
            targets,
            vec![
                ((1663, 5, 16400, 0), 2, Lsn(50)),
                ((1663, 5, 16400, 0), 3, Lsn(80)),
            ],
            "same-fork shrinks stay separate, each with its own (relsize, due_lsn)"
        );
    }

    /// A shrink candidate with no stored `relsize` is a bug upstream: the
    /// builder drops it (never guessing a size) and alarms, so the drain
    /// keeps going rather than wedging. A valid candidate alongside it
    /// still passes through.
    #[test]
    fn shrink_targets_skip_and_alarm_on_missing_relsize() {
        let alarms = || {
            crate::metrics::CORRECTNESS_ERRORS
                .with_label_values(&["gc_shrink_missing_relsize"])
                .get()
        };
        let mk = |relsize: Option<i64>| PendingGcRow {
            spcnode: 1663,
            dbnode: 5,
            relnode: 16400,
            forknum: 0,
            blknum: GC_KIND_SHRINK,
            due_lsn: Lsn(50),
            relsize,
        };
        let before = alarms();
        let targets = Timeline::shrink_targets(&[mk(None), mk(Some(4))]);
        assert_eq!(
            targets,
            vec![((1663, 5, 16400, 0), 4, Lsn(50))],
            "the missing-relsize candidate is skipped; the valid one passes"
        );
        assert_eq!(alarms() - before, 1, "the skip alarms exactly once");
    }
}
