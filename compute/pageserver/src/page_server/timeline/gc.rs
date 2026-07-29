//! PageServer GC: the periodic candidate drain (drop / shrink / trim)
//! plus its cutoff computation. `impl Timeline` here reaches
//! Timeline's private fields because `gc` is a descendant module of
//! `timeline`.

use super::*;
use pageserver_api::key::Key;

/// One tick's due candidates split by coarse operation (the drain's
/// action order + dequeue method): trim candidates keep their `(key,
/// gc_kind)` so `act_trims` dispatches to the right table; drop and shrink
/// keep the whole row (it carries its `gc_kind`). Produced by
/// [`Timeline::partition_by_kind`]; borrowed by [`Timeline::act`] then
/// [`Timeline::dequeue`].
struct DuePartition {
    trims: Vec<(Key, crate::page_store::GcKind)>,
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
    /// - **select**: rows with `floor < due_lsn <= applied_cutoff`,
    ///   oldest-first, capped at `gc_drain_limit` — the floor seeks the
    ///   scan past rows earlier ticks already handled;
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
    ///   next tick re-selects. Uncapped → the cutoff (everything
    ///   `<= cutoff` handled); capped → `last selected due_lsn − 1` (the
    ///   −1 keeps a limit-split tie group selectable).
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

        // Objects (rel forks / SLRU segments) with a materialise job
        // pending, snapshotted BEFORE acting so a job in flight during a
        // drop's delete is always caught: a busy-object drop is acted but
        // not dequeued (see `dequeue`), so a straggling image INSERT after
        // the delete can't leak. Paid only when drops are present.
        let objects_pending_materialise = if partition.drops.is_empty() {
            Default::default()
        } else {
            self.page_writer
                .as_ref()
                .map(|pw| pw.objects_with_pending_materialise())
                .unwrap_or_default()
        };

        // Cancel is checked once, up front: a mid-tick cancel just
        // leaves candidates queued (idempotent, re-drained next tick).
        if self.cancel.is_cancelled() {
            return Err(GcError::TimelineCancelled);
        }

        // Act (delete), then dequeue what was acted — strictly after every
        // action succeeded. An error before the dequeue returns via `?`
        // leaving the floor unchanged, so the next tick re-selects the same
        // batch. `objects_pending_materialise` was snapshotted before acting.
        let rows_deleted = self.act(store, &partition, applied_cutoff).await?;
        result.rows_deleted = rows_deleted;

        let dequeued = self
            .dequeue(
                store,
                &partition,
                &objects_pending_materialise,
                floor,
                applied_cutoff,
            )
            .await?;
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
        crate::metrics::GC_CANDIDATES_DEQUEUED.inc_by(dequeued);
        crate::metrics::GC_ROWS_DELETED.inc_by(result.rows_deleted);
        crate::metrics::GC_DRAIN_TICKS
            .with_label_values(&[if all_due_visited { "drained" } else { "capped" }])
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
        due: &DuePartition,
        applied_cutoff: Lsn,
    ) -> Result<u64, GcError> {
        let mut pages_deleted = self.act_drops(store, &due.drops).await?;
        pages_deleted += self.act_shrinks(store, &due.shrinks).await?;
        pages_deleted += self.act_trims(store, &due.trims, applied_cutoff).await?;
        Ok(pages_deleted)
    }

    /// Remove from `pending_gc` exactly what [`Self::act`] just reclaimed,
    /// returning the count. Trim keys range-sweep `(floor, cutoff]`;
    /// drop/shrink clear by their PK. A drop whose object is in
    /// `objects_pending_materialise` (snapshotted before acting) is left
    /// queued for a later tick — a straggling image INSERT after its delete
    /// would otherwise leak — and logged, not removed. Runs only after
    /// `act` succeeded (crash-safety: act-before-dequeue).
    async fn dequeue(
        &self,
        store: &Arc<crate::page_store::PageStore>,
        due: &DuePartition,
        objects_pending_materialise: &std::collections::HashSet<Key>,
        floor: Lsn,
        applied_cutoff: Lsn,
    ) -> Result<u64, GcError> {
        // Trims dequeue by a range sweep over their (key, gc_kind) pairs;
        // shrinks and non-busy drops by PK; a busy drop (in
        // objects_pending_materialise) stays queued.
        let mut drop_shrink_to_dequeue: Vec<crate::page_store::GcDropShrinkCandidateId> = due
            .shrinks
            .iter()
            .map(|r| (r.key, r.gc_kind, r.due_lsn))
            .collect();
        let mut deferred_dequeue_keys: Vec<Key> = Vec::new();
        for r in &due.drops {
            if objects_pending_materialise.contains(&r.key) {
                deferred_dequeue_keys.push(r.key);
            } else {
                drop_shrink_to_dequeue.push((r.key, r.gc_kind, r.due_lsn));
            }
        }
        // A busy object's drop is acted but left queued; it re-selects (and
        // re-logs) only on floor resets, so an object recurring here across
        // resets means its materialise job is wedged, pinning the drop's
        // reclamation. info! (not debug): the whole point is prod
        // visibility, and the volume is one line per object per reset.
        if !deferred_dequeue_keys.is_empty() {
            let sample = &deferred_dequeue_keys[..deferred_dequeue_keys.len().min(8)];
            tracing::info!(
                count = deferred_dequeue_keys.len(),
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
    /// (id-reuse-safe — a recreated object writes above it). Rel-fork and
    /// SLRU drops act per candidate, in parallel (distinct tablets, no
    /// ordering constraint). Dead meta keys batch per `due_lsn` into one
    /// `key = ANY` delete — a DROP DATABASE emits O(rels) of them (a
    /// sparse-reldir tombstone per rel + the dir/relmap keys) all at the
    /// drop's LSN — the same batched meta path `act_trims` uses. Returns
    /// rows deleted.
    async fn act_drops(
        &self,
        store: &Arc<crate::page_store::PageStore>,
        drop_rows: &[crate::page_store::PendingGcRow],
    ) -> Result<u64, GcError> {
        use crate::page_store::GcKind;
        use std::collections::HashMap;
        let branch_id = self.branch_id;

        // Split by table: rel-fork / SLRU drops act per candidate; DropMeta
        // keys group by `due_lsn` for one batched delete each. A non-drop
        // kind here is a partition bug — skip + alarm, don't misdelete.
        let mut fork_slru: Vec<crate::page_store::PendingGcRow> = Vec::new();
        let mut meta_by_due: HashMap<Lsn, Vec<Key>> = HashMap::new();
        for row in drop_rows {
            match row.gc_kind {
                GcKind::DropPages | GcKind::DropSlru => fork_slru.push(*row),
                GcKind::DropMeta => meta_by_due.entry(row.due_lsn).or_default().push(row.key),
                other => {
                    tracing::error!(gc_kind = ?other, key = %row.key, "act_drops: non-drop kind");
                    crate::metrics::CORRECTNESS_ERRORS
                        .with_label_values(&["gc_unknown_candidate_kind"])
                        .inc();
                }
            }
        }

        // TODO(perf): a DROP DATABASE emits O(rels) DropPages too; these could
        // batch like DropMeta above — one composite (spc,db,rel,fork) unnest
        // delete per due_lsn, plus batched rel-size sentinels. DropSlru is few.
        let mut deleted = self
            .run_parallel_actions(store, fork_slru.into_iter(), |store, row| async move {
                let (key, due) = (row.key, row.due_lsn);
                match row.gc_kind {
                    // rel fork: drop its pages + rel-size sentinel key.
                    GcKind::DropPages => {
                        store
                            .gc_act_drop_fork(branch_id, crate::page_store::rel_fork_of(&key), due)
                            .await
                    }
                    // SLRU segment: drop its slru_pages + its seg-size
                    // sentinel meta key (= this key).
                    GcKind::DropSlru => {
                        store
                            .gc_act_drop_slru(
                                branch_id,
                                crate::page_store::slru_seg_of(&key),
                                crate::page_store::key_to_bytes(&key),
                                due,
                            )
                            .await
                    }
                    // The split above routed only Drop{Pages,Slru} here.
                    _ => Ok(0),
                }
            })
            .await?;

        // Dead meta keys, one batched delete per `due_lsn`.
        for (due, keys) in meta_by_due {
            deleted += store
                .gc_act_drop_meta(branch_id, &keys, due)
                .await
                .map_err(GcError::Persistence)?;
        }
        Ok(deleted)
    }

    /// Shrink actions, one delete per candidate. Returns rows deleted.
    async fn act_shrinks(
        &self,
        store: &Arc<crate::page_store::PageStore>,
        shrink_rows: &[crate::page_store::PendingGcRow],
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
    /// `gc_act_shrink_fork`). Shrink is rel-only: a non-rel or
    /// missing-`relsize` candidate is skipped and alarmed, not acted, so
    /// the drain isn't wedged.
    fn shrink_targets(
        shrink_rows: &[crate::page_store::PendingGcRow],
    ) -> Vec<(crate::page_store::GcForkIdent, i64, Lsn)> {
        shrink_rows
            .iter()
            .filter_map(|r| {
                if !r.key.is_rel_size_key() {
                    tracing::error!(
                        key = %r.key,
                        due_lsn = %r.due_lsn,
                        "pending_gc shrink candidate is not a rel-size key; skipped"
                    );
                    crate::metrics::CORRECTNESS_ERRORS
                        .with_label_values(&["gc_shrink_non_rel"])
                        .inc();
                    return None;
                }
                match r.relsize {
                    Some(relsize) => {
                        Some((crate::page_store::rel_fork_of(&r.key), relsize, r.due_lsn))
                    }
                    None => {
                        tracing::error!(
                            key = %r.key,
                            due_lsn = %r.due_lsn,
                            "pending_gc shrink candidate without a stored relsize; skipped"
                        );
                        crate::metrics::CORRECTNESS_ERRORS
                            .with_label_values(&["gc_shrink_missing_relsize"])
                            .inc();
                        None
                    }
                }
            })
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
        // their object); keys are already deduped in partition_by_kind. An
        // unexpected kind here would be a partition bug — skip + alarm.
        let mut rel_by_fork: HashMap<crate::page_store::GcForkIdent, Vec<i64>> = HashMap::new();
        let mut slru_by_seg: HashMap<crate::page_store::GcSlruSeg, Vec<i64>> = HashMap::new();
        let mut meta_keys: Vec<Vec<u8>> = Vec::new();
        for (key, gc_kind) in trims {
            match *gc_kind {
                GcKind::TrimPages => rel_by_fork
                    .entry(crate::page_store::rel_fork_of(key))
                    .or_default()
                    .push(key.field6 as i64),
                GcKind::TrimSlru => slru_by_seg
                    .entry(crate::page_store::slru_seg_of(key))
                    .or_default()
                    .push(key.field6 as i64),
                GcKind::TrimMeta => meta_keys.push(crate::page_store::key_to_bytes(key)),
                other => {
                    tracing::error!(gc_kind = ?other, key = %key, "act_trims: non-trim kind");
                    crate::metrics::CORRECTNESS_ERRORS
                        .with_label_values(&["gc_unknown_candidate_kind"])
                        .inc();
                }
            }
        }

        let rel_deleted = self
            .run_parallel_actions(
                store,
                rel_by_fork.into_iter(),
                |store, (fork, blknums)| async move {
                    store
                        .gc_act_trim_blocks(branch_id, fork, &blknums, applied_cutoff)
                        .await
                },
            )
            .await?;

        let slru_deleted = self
            .run_parallel_actions(
                store,
                slru_by_seg.into_iter(),
                |store, (seg, blknums)| async move {
                    store
                        .gc_act_trim_slru(branch_id, seg, &blknums, applied_cutoff)
                        .await
                },
            )
            .await?;

        // Meta trim: one batched keep-newest over all meta keys.
        let meta_deleted = store
            .gc_act_trim_meta(branch_id, &meta_keys, applied_cutoff)
            .await
            .map_err(GcError::Persistence)?;

        Ok(rel_deleted + slru_deleted + meta_deleted)
    }

    /// Partition due candidates into the drain's coarse buckets — trim /
    /// drop / shrink — off `gc_kind`, which sets the drain's action order
    /// and dequeue method. A trim keeps its `(key, gc_kind)` so `act_trims`
    /// can dispatch to the right table (N due candidates for one key
    /// coalesce, since the trim anchors at the cutoff, not any candidate's
    /// due_lsn); drop and shrink keep the full row (it carries its kind).
    /// An unknown kind is skipped with a `correctness_errors_total` alarm
    /// and left on disk, never dequeued.
    ///
    /// `trims` is deduped by key here (a hot key has many due candidates),
    /// so neither the trim action nor the dequeue carries duplicate keys.
    ///
    /// Returns the split as a [`DuePartition`].
    fn partition_by_kind(due: Vec<crate::page_store::PendingGcRow>) -> DuePartition {
        use crate::page_store::CoarseOp;
        let mut trims = Vec::new();
        let mut drop_rows = Vec::new();
        let mut shrink_rows = Vec::new();
        for row in due {
            match row.gc_kind.coarse() {
                Some(CoarseOp::Trim) => trims.push((row.key, row.gc_kind)),
                Some(CoarseOp::Drop) => drop_rows.push(row),
                Some(CoarseOp::Shrink) => shrink_rows.push(row),
                None => {
                    tracing::error!(
                        gc_kind = ?row.gc_kind,
                        key = %row.key,
                        due_lsn = %row.due_lsn,
                        "pending_gc candidate of unknown kind; left on disk, never dequeued"
                    );
                    crate::metrics::CORRECTNESS_ERRORS
                        .with_label_values(&["gc_unknown_candidate_kind"])
                        .inc();
                }
            }
        }
        trims.sort_unstable();
        trims.dedup();
        DuePartition {
            trims,
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
    use crate::page_store::{GcKind, PendingGcRow};
    use pageserver_api::key::{rel_block_to_key, rel_size_to_key};
    use pageserver_api::reltag::RelTag;

    fn rel(relnode: u32) -> RelTag {
        RelTag {
            spcnode: 1663,
            dbnode: 5,
            relnode,
            forknum: 0,
        }
    }
    /// A trim candidate for a rel block.
    fn trim_row(relnode: u32, blknum: u32) -> PendingGcRow {
        PendingGcRow {
            key: rel_block_to_key(rel(relnode), blknum),
            gc_kind: GcKind::TrimPages,
            due_lsn: Lsn(100),
            relsize: None,
        }
    }
    /// A drop candidate — the fork's rel-size object key.
    fn drop_row(relnode: u32, due: u64) -> PendingGcRow {
        PendingGcRow {
            key: rel_size_to_key(rel(relnode)),
            gc_kind: GcKind::DropPages,
            due_lsn: Lsn(due),
            relsize: None,
        }
    }
    /// A shrink candidate — the fork's rel-size key + relsize.
    fn shrink_row(relnode: u32, relsize: i64, due: u64) -> PendingGcRow {
        PendingGcRow {
            key: rel_size_to_key(rel(relnode)),
            gc_kind: GcKind::ShrinkPages,
            due_lsn: Lsn(due),
            relsize: Some(relsize),
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
        let unknown = PendingGcRow {
            key: rel_block_to_key(rel(16400), 0),
            gc_kind: GcKind::Unknown(7), // no such kind
            due_lsn: Lsn(100),
            relsize: None,
        };
        let p = Timeline::partition_by_kind(vec![
            trim_row(16400, 0),
            drop_row(16400, 100),
            shrink_row(16400, 2, 100),
            unknown,
        ]);
        assert_eq!(p.trims.len(), 1);
        assert_eq!(p.drops.len(), 1);
        assert_eq!(p.shrinks.len(), 1);
        assert_eq!(alarms() - before, 1, "the unknown kind alarms once");
    }

    /// Each of the 7 kinds buckets by coarse operation (trim / drop /
    /// shrink), and a trim retains its `gc_kind` for table dispatch.
    #[test]
    fn partition_buckets_all_seven_kinds() {
        let row = |key: Key, gc_kind: GcKind| PendingGcRow {
            key,
            gc_kind,
            due_lsn: Lsn(100),
            relsize: None,
        };
        let k = rel_block_to_key(rel(16400), 0);
        let p = Timeline::partition_by_kind(vec![
            row(k, GcKind::TrimPages),
            row(k, GcKind::TrimSlru),
            row(k, GcKind::TrimMeta),
            row(k, GcKind::ShrinkPages),
            row(k, GcKind::DropPages),
            row(k, GcKind::DropSlru),
            row(k, GcKind::DropMeta),
        ]);
        // 3 TRIM_* → trims (deduped by key, but distinct kinds stay), 3
        // DROP_* → drops, 1 SHRINK → shrinks.
        let mut trim_kinds: Vec<GcKind> = p.trims.iter().map(|(_, kind)| *kind).collect();
        trim_kinds.sort_unstable();
        assert_eq!(
            trim_kinds,
            vec![GcKind::TrimPages, GcKind::TrimSlru, GcKind::TrimMeta]
        );
        assert_eq!(p.drops.len(), 3);
        assert_eq!(p.shrinks.len(), 1);
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
        let targets =
            Timeline::shrink_targets(&[shrink_row(16400, 2, 50), shrink_row(16400, 3, 80)]);
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
        let mut missing = shrink_row(16400, 0, 50);
        missing.relsize = None;
        let valid = shrink_row(16400, 4, 50);
        let before = alarms();
        let targets = Timeline::shrink_targets(&[missing, valid]);
        assert_eq!(
            targets,
            vec![((1663, 5, 16400, 0), 4, Lsn(50))],
            "the missing-relsize candidate is skipped; the valid one passes"
        );
        assert_eq!(alarms() - before, 1, "the skip alarms exactly once");
    }
}
