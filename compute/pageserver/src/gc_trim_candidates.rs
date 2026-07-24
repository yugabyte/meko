//! GC **trim**-candidate emission: every-n sampling + a shared
//! candidate buffer.
//!
//! Both producers — the ingest window flush and the
//! materialiser's `commit_phase` — record their base writes with one
//! [`GcTrimCandidateEmitter`], owned by the `PageWriter`: it samples them
//! through the shared per-block counter, buffers the survivors, and
//! flushes to `pageserver.pending_gc` on its own loop. Best-effort by
//! design: trims self-heal (the block's next base re-covers a lost
//! span), so no candidate here needs the window-durable path.
//! Drop/shrink events do — they stay on the `PageWriter`'s window
//! flush.
//!
//! The drain side (cutoff + select + act + dequeue) lives in
//! `page_server/timeline/gc.rs`; the story is in
//! `architecture/pageserver-gc.md`.

use std::sync::Arc;
use std::time::Duration;

use dashmap::DashMap;
use pageserver_api::key::Key;
use tokio_util::sync::CancellationToken;
use tracing::info;
use utils::id::BranchId;
use utils::lsn::Lsn;
use utils::simple_rcu::Rcu;

use crate::page_store::{GcBlockIdent, PageRow, PageStore, PendingGcRow};

/// Flush the GC trim-candidate buffer at this many distinct blocks
/// (= one full `ROWS_PER_INSERT` chunk) ...
const GC_TRIM_CANDIDATE_FLUSH_ROWS: usize = 500;

/// ... or after this long, whichever first. The row cap — not this —
/// bounds crash loss.
const GC_TRIM_CANDIDATE_FLUSH_INTERVAL: Duration = Duration::from_secs(5);

/// Build `pending_gc` rows from buffered candidates, clamping each
/// due_lsn to `cutoff + 1` (see [`GcTrimCandidateEmitter::flush`]
/// for why). Split out from the flush so the clamp is unit-testable
/// without a `PageStore`.
fn clamp_candidate_rows(
    entries: impl IntoIterator<Item = (GcBlockIdent, Lsn)>,
    cutoff: Lsn,
) -> Vec<PendingGcRow> {
    let due_floor = Lsn(cutoff.0 + 1);
    entries
        .into_iter()
        .map(
            |((spcnode, dbnode, relnode, forknum, blknum), due_lsn)| PendingGcRow {
                spcnode,
                dbnode,
                relnode,
                forknum,
                blknum,
                due_lsn: due_lsn.max(due_floor),
            },
        )
        .collect()
}

/// The **best-effort** GC trim-candidate emission pipeline — samples
/// both producers' base writes (the ingest window flush and the
/// materialiser's `commit_phase`) through one every-n counter, folds
/// the survivors, and flushes them to `pending_gc` on its own loop. A
/// crash or failed flush loses up to one candidate buffer, by design —
/// trims are cumulative, so the block's next candidate re-covers the
/// garbage. Same-block candidates fold to the LATEST due_lsn (the safe
/// direction: a later trim covers an earlier one's garbage; folding to
/// the earliest would not).
///
/// Drop/shrink events do NOT come through here: they are one-shot (no
/// future base re-emits them), so they keep the window-durable path
/// (`page_writer::AuxBuffers::gc_event_buffer`).
pub(crate) struct GcTrimCandidateEmitter {
    branch_id: BranchId,
    page_store: Arc<PageStore>,
    /// Every-n sampling: one candidate per `every_n_bases` base writes
    /// per block (n=1: every base).
    every_n_bases: u32,
    /// Per-block sampling state — (bases counted since the last
    /// emitted candidate, max LSN among them); entries removed on
    /// emission. In-memory: resets
    /// on restart. Hot blocks self-clean on emission, but a block going
    /// cold after 1..n-1 bases leaks its entry — a slow creep under a
    /// large random-update workload, not strictly bounded.
    /// TODO(gc_base_counts_cap): periodic sweep or size cap if it ever
    /// grows to matter.
    base_counts: DashMap<Key, (u32, Lsn)>,
    /// The timeline's published GC cutoff, shared with
    /// `Timeline::applied_gc_cutoff_lsn`. Read (no guard held) at flush
    /// to clamp each candidate's due_lsn above the cutoff — see
    /// [`Self::flush`].
    applied_gc_cutoff: Arc<Rcu<Lsn>>,
    /// The candidate buffer: block identity -> latest due_lsn seen; drained
    /// by `flush`. Its loss (crash, failed flush) is the emitter's
    /// bounded best-effort exposure.
    candidate_buffer: std::sync::Mutex<std::collections::HashMap<GcBlockIdent, Lsn>>,
    /// Signalled when the buffer crosses `GC_TRIM_CANDIDATE_FLUSH_ROWS`.
    flush_ready: tokio::sync::Notify,
}

impl GcTrimCandidateEmitter {
    pub(crate) fn new(
        branch_id: BranchId,
        page_store: Arc<PageStore>,
        applied_gc_cutoff: Arc<Rcu<Lsn>>,
        every_n_bases: u32,
    ) -> Self {
        Self {
            branch_id,
            page_store,
            every_n_bases: every_n_bases.max(1),
            base_counts: DashMap::new(),
            applied_gc_cutoff,
            candidate_buffer: std::sync::Mutex::new(std::collections::HashMap::new()),
            flush_ready: tokio::sync::Notify::new(),
        }
    }

    /// Count one base write to `key`: every n-th counted base emits a
    /// candidate — returns its due_lsn (the max lsn among the n
    /// counted bases), `None` otherwise.
    fn increment_base_count(&self, key: Key, lsn: Lsn) -> Option<Lsn> {
        if self.every_n_bases <= 1 {
            return Some(lsn);
        }
        // Single lock acquisition for the increment + conditional
        // removal (an entry()+remove() pair would take the shard lock
        // twice and leave a TOCTOU gap between them).
        use dashmap::mapref::entry::Entry;
        match self.base_counts.entry(key) {
            Entry::Occupied(mut occupied) => {
                let (count, max_lsn) = occupied.get_mut();
                *count += 1;
                if lsn > *max_lsn {
                    *max_lsn = lsn;
                }
                if *count >= self.every_n_bases {
                    let due = *max_lsn;
                    occupied.remove();
                    Some(due)
                } else {
                    None
                }
            }
            Entry::Vacant(vacant) => {
                vacant.insert((1, lsn));
                None
            }
        }
    }

    /// Record a producer's base writes: sample each `will_init` row
    /// through the shared every-n counter; every n-th base folds a
    /// candidate into the buffer at the max lsn of its n counted bases
    /// (same-block folds to the latest). `path` labels the emission
    /// metric (`ingest` /
    /// `materialiser`). In-memory only — the YB INSERT is deferred to
    /// `flush`, so this adds no YB round-trip to either producer's hot
    /// path.
    pub(crate) fn record_bases<'a>(
        &self,
        rows: impl IntoIterator<Item = &'a PageRow>,
        path: &'static str,
    ) {
        let emitted: Vec<(GcBlockIdent, Lsn)> = rows
            .into_iter()
            .filter(|row| row.will_init)
            .filter_map(|row| {
                let due = self.increment_base_count(row.key, row.lsn)?;
                crate::metrics::GC_CANDIDATES_EMITTED
                    .with_label_values(&[path])
                    .inc();
                Some((
                    (
                        row.key.field2 as i64,
                        row.key.field3 as i64,
                        row.key.field4 as i64,
                        row.key.field5 as i16,
                        row.key.field6 as i64,
                    ),
                    due,
                ))
            })
            .collect();
        if emitted.is_empty() {
            return;
        }
        let mut guard = self.candidate_buffer.lock().unwrap();
        for (ident, lsn) in emitted {
            let due = guard.entry(ident).or_insert(lsn);
            if lsn > *due {
                *due = lsn;
            }
        }
        // Notify outside the lock so a woken flusher doesn't contend it.
        let should_notify = guard.len() >= GC_TRIM_CANDIDATE_FLUSH_ROWS;
        drop(guard);
        if should_notify {
            self.flush_ready.notify_one();
        }
    }

    /// Flush everything buffered. On error rows are dropped, not
    /// retried — best-effort by design (see the struct doc).
    ///
    /// Clamp each due_lsn up to `cutoff + 1` before insert, upholding the
    /// GC invariant that new candidates land above the cutoff (GC acts
    /// only at `due_lsn <= cutoff`), so a fresh candidate can't race an
    /// in-flight drain and is guaranteed to be picked up by a later tick.
    /// A candidate can sit here past that point — a materialiser image
    /// is stamped at its folded chain's LSN, and an ingest candidate can
    /// wait out a window flush plus a cutoff advance — hence the clamp.
    /// Raising due_lsn is safe: it only sets when the block is next
    /// visited — the trim deletes below the block's newest base as of
    /// the cutoff regardless.
    ///
    /// The cutoff is read without locking it across the insert, so a
    /// concurrent tick can advance it past the clamped value and skip the
    /// row. Bounded: it waits for the periodic floor reset (or a
    /// restart), and the block's next base re-covers the garbage. Locking
    /// across the insert would avoid it but stall every GC tick behind
    /// these flushes.
    pub(crate) async fn flush(&self) {
        let cutoff = Lsn(self.applied_gc_cutoff.read().0);
        let rows = {
            let mut guard = self.candidate_buffer.lock().unwrap();
            clamp_candidate_rows(guard.drain(), cutoff)
        };
        if rows.is_empty() {
            return;
        }
        let t0 = std::time::Instant::now();
        match self.page_store.put_pending_gc(self.branch_id, &rows).await {
            Ok(()) => {
                info!(
                    tag = "pending_gc_flush",
                    rows = rows.len(),
                    flush_ms = t0.elapsed().as_millis() as u64,
                    "perf",
                );
            }
            Err(e) => {
                tracing::error!(
                    branch_id = %self.branch_id,
                    dropped_rows = rows.len(),
                    "pending_gc flush failed — candidates dropped (best-effort by \
                     design; affected blocks stay untrimmed until their next \
                     base): {e:#}"
                );
            }
        }
    }

    /// Flusher loop: flush on the row-count signal or every
    /// `GC_TRIM_CANDIDATE_FLUSH_INTERVAL`, and once more on
    /// cancellation so a clean shutdown loses nothing.
    pub(crate) async fn flush_loop(self: Arc<Self>, cancel: CancellationToken) {
        loop {
            tokio::select! {
                _ = cancel.cancelled() => break,
                _ = self.flush_ready.notified() => {}
                _ = tokio::time::sleep(GC_TRIM_CANDIDATE_FLUSH_INTERVAL) => {}
            }
            self.flush().await;
        }
        self.flush().await;
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// The trim-buffer flush clamp lifts a candidate's due_lsn to
    /// `cutoff + 1` when it sits at or below the cutoff (a materialiser
    /// image stamped at an old folded LSN, or an ingest candidate that
    /// out-waited a cutoff advance), and leaves an above-cutoff one
    /// untouched.
    #[test]
    fn clamp_lifts_due_lsn_to_above_cutoff() {
        let cutoff = Lsn(100);
        let ident = |blk: i64| (1663i64, 5i64, 16400i64, 0i16, blk);
        let rows = clamp_candidate_rows(
            [
                (ident(7), Lsn(40)),  // below cutoff → lifted
                (ident(8), Lsn(100)), // at cutoff → lifted
                (ident(9), Lsn(500)), // above cutoff → unchanged
            ],
            cutoff,
        );
        let due = |blk: i64| rows.iter().find(|r| r.blknum == blk).unwrap().due_lsn;
        assert_eq!(due(7), Lsn(101), "below cutoff → cutoff+1");
        assert_eq!(due(8), Lsn(101), "at cutoff → cutoff+1");
        assert_eq!(due(9), Lsn(500), "above cutoff → unchanged");
    }
}
