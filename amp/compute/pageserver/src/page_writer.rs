//! `PageWriter` — buffered write path between `walingest` and `PageStore`.
//!
//! Pure passthrough: every relation-page entry from walingest becomes
//! one row in `amp_storage.pages`, untouched. WAL records flow in as
//! `is_image=FALSE`; FPI records flow in as `is_image=TRUE`. The
//! `will_init` flag comes straight off the `Value` — we don't synthesise
//! anchors at write time.
//!
//! Chain length is not a write-time invariant any more — a background
//! image materialiser (separate module) periodically scans
//! `amp_storage.pages` for keys whose delta chain has grown long and
//! drops a fresh image row at the latest LSN. Reads stay correct without
//! it (`PageStore::read_chain` walks back to the latest will_init delta
//! when no image exists); the background task is pure optimisation that
//! keeps reads fast.
//!
//! Buffering: rows accumulate in RAM until a flush trigger (age or
//! bytes) fires, then drain to `amp_storage.pages` in one INSERT.

use std::collections::HashMap;
use std::hash::{Hash, Hasher};
use std::sync::Arc;
use std::sync::atomic::{AtomicU64, AtomicUsize, Ordering};
use std::time::{Duration, Instant};

use hashlink::LruCache;

use anyhow::Result;
use bytes::Bytes;
use dashmap::{DashMap, DashSet};
use pageserver_api::key::{KEY_SIZE, Key};
use parking_lot::Mutex as PlMutex;
use tokio_util::sync::CancellationToken;
use tracing::{Instrument, info, info_span};
use utils::bin_ser::BeSer;
use utils::id::BranchId;
use utils::lsn::Lsn;
use utils::simple_rcu::Rcu;
use wal_decoder::models::record::NeonWalRecord;
use wal_decoder::models::value::Value;
use wal_decoder::serialized_batch::{SerializedValueBatch, ValueMeta};

use pageserver_api::reltag::SlruKind;

use crate::gc_trim_candidates::{GcTrimCandidateEmitter, TrimBase};
use crate::page_store::{
    LineageSegment, MetaRow, PageRow, PageStore, PendingGcRow, SlruPageRow, merge_page_chain,
};

/// Flush triggers (whichever fires first). Override via
/// `YbPageWriterConfig` in `pageserver.toml`.
#[derive(Clone, Debug)]
pub struct FlushPolicy {
    pub max_age: Duration,
    pub max_bytes: usize,
    /// Hard ceiling on `unflushed_bytes + bytes_in_aux_buffers` before
    /// `put()` blocks waiting on a flush. 0 disables backpressure —
    /// `put()` always returns immediately and the buffer can grow
    /// without bound when YB falls behind. Set above `max_bytes` so
    /// the size-based flush trigger fires first; recommended ≥ 2×
    /// `max_bytes`.
    pub backpressure_bytes: usize,
    /// Number of parallel workers used to deserialize + build page rows in
    /// `put_serialized`. 1 = serial. See `YbPageWriterConfig::ingest_parallelism`.
    pub ingest_parallelism: usize,
    /// Minimum batch size (entries) before parallel apply kicks in. See
    /// `YbPageWriterConfig::ingest_parallelism_min_batch`.
    pub ingest_parallelism_min_batch: usize,
    /// See `YbPageWriterConfig::release_backpressure_per_chunk`.
    pub release_backpressure_per_chunk: bool,
}

impl Default for FlushPolicy {
    fn default() -> Self {
        Self {
            max_age: Duration::from_secs(5),
            max_bytes: 12 * 1024 * 1024,
            backpressure_bytes: 25 * 1024 * 1024,
            ingest_parallelism: 1,
            ingest_parallelism_min_batch: 256,
            release_backpressure_per_chunk: true,
        }
    }
}

/// A request to walredo the chain for `key` at `target_lsn` and INSERT
/// a fresh image row. Produced by `PageWriter::put` when a key's delta
/// chain crosses `image_chain_limit`, consumed by the background image
/// materialiser task. Materialisation is best-effort: dropped jobs (on
/// channel-full or task error) just mean the chain stays long for one
/// more cycle.
#[derive(Debug, Clone)]
pub struct MaterializeJob {
    pub key: Key,
    /// A relation-page job targets a buffered delta, flushed or not: the
    /// crossing delta when `put()` enqueues it, the key's newest on a retry.
    /// Post-flush, cold-residue and read-side jobs stay at or below
    /// `flush_horizon_lsn`, which SLRU jobs require: an SLRU chain is
    /// read from YB alone.
    pub target_lsn: Lsn,
}

/// Trigger criteria for the image materialiser. `None` disables the
/// counter logic in `PageWriter::put`; mostly useful for tests and the
/// upstream-style PhysicalStorage-only mode that doesn't need a
/// materialiser.
///
/// Materialisation fires on **whichever threshold lands first**:
/// `count_limit` deltas accumulated since the last anchor, OR
/// `payload_bytes_limit` of combined delta payload bytes accumulated
/// since the last anchor. Reasoning: small WAL records (single-bit
/// `ClearVisibilityMapFlags`, 1-byte payloads) accumulate slowly
/// and waste materialiser cycles when count-only; large deltas
/// (near-full pages) leave reads doing many records' worth of
/// walredo before the count trigger fires. Either zero disables the
/// corresponding trigger (both zero → materialiser fully disabled,
/// equivalent to `image_chain_limit = 0`).
#[derive(Clone)]
pub struct MaterializerHandle {
    /// Latched true once the walreceiver's first post-attach catch-up
    /// completes (see `Timeline::mark_replay_caught_up`). While false,
    /// no materialize job may be dispatched: a fold that runs while the
    /// attach replay window is being re-ingested can anchor on state
    /// that predates deltas it has not seen, and the resulting image is
    /// stamped above its own content — a durable lie every later read
    /// anchors on (#240: a clog image at the basebackup LSN was missing
    /// the final ~30 durable commits' bits).
    pub replay_caught_up: tokio::sync::watch::Receiver<bool>,
    pub count_limit: u32,
    /// Per-key delta threshold for SLRU (clog/multixact) block keys.
    /// SLRU keys ignore `count_limit`/`payload_bytes_limit` and use this
    /// instead — see `YbPageWriterConfig::image_chain_limit_slru`.
    pub slru_count_limit: u32,
    pub payload_bytes_limit: u32,
    pub tx: tokio::sync::mpsc::Sender<MaterializeJob>,
    /// Keys with a job currently queued or being processed. The write
    /// trigger refuses to enqueue (and refuses to clear the per-key
    /// delta counter) when a key is already in flight — this prevents
    /// the "hot page modified 100 times during materialiser stall"
    /// case from queueing 5 jobs at LSNs (20, 40, 60, 80, 100). Instead
    /// the counter stays at >= threshold across the stall and the next
    /// delta after the in-flight job completes fires a single fresh
    /// job at the *current* LSN — strictly better for read latency
    /// (image is closer to last_record_lsn) and avoids 4 wasted
    /// walredo cycles.
    ///
    /// Cleared by the materialiser after each batch completes
    /// (success OR failure — on failure the next trigger needs to be
    /// able to fire). Task panics leak entries; pageserver restart
    /// clears all timeline state including this set.
    pub in_flight: Arc<std::sync::Mutex<std::collections::HashSet<Key>>>,
}

impl MaterializerHandle {
    /// False while the attach replay window is still being re-ingested;
    /// no fold may be dispatched (see `replay_caught_up`).
    pub fn folds_allowed(&self) -> bool {
        *self.replay_caught_up.borrow()
    }
}

/// Running per-key delta tally — count, payload bytes, and the LSN
/// of the most recent delta seen for this key. See
/// `PageWriter::delta_counts`.
///
/// `last_lsn` lets the post-flush trigger dispatch keys that crossed
/// the threshold in a PAST flush (so they aren't in the current
/// flush's snapshot). If `last_lsn ≤ flush_horizon_lsn` the row is
/// durable in YB and we can safely use `last_lsn` as the materialise
/// target. Without this, cold-key residue accumulates in
/// `delta_counts` forever: keys that hit the threshold then went
/// quiet are never re-visited by the snapshot-matching dispatch,
/// causing delta_counts to grow unbounded (the 8.4M-entries pathology
/// we observed under sustained mixed read/write workloads).
#[derive(Clone, Copy, Default)]
struct DeltaTally {
    count: u32,
    payload_bytes: u32,
    last_lsn: Lsn,
}

impl DeltaTally {
    /// Whether this tally has reached either materialize threshold. A
    /// limit of 0 disables that dimension.
    fn hits(&self, count_limit: u32, payload_bytes_limit: u32) -> bool {
        (count_limit > 0 && self.count >= count_limit)
            || (payload_bytes_limit > 0 && self.payload_bytes >= payload_bytes_limit)
    }
}

/// Top-level write buffer + flusher for one timeline.
///
/// Two `PageStore` handles, each on its own YSQL connection:
/// - `compute_page_store` serves compute-facing reads
///   (`scan_range` / `read_chain`). Idle on cache hit.
/// - `hot_path_page_store` serves walreceiver writes (`flush*`) and
///   the image materialiser's cache-miss read
///   (`read_chain_for_materialize`). Materialiser image writes also go via
///   this connection (held directly by `ImageMaterializer`).
///
/// Compute getPage stays isolated from internal load — a long flush
/// or a materialiser walredo-and-INSERT can't queue behind a compute
/// read and vice versa.
pub struct PageWriter {
    branch_id: BranchId,
    /// Ordered ancestry segments, nearest-first (`lineage[0]` is this
    /// timeline at `Lsn::MAX`). All PageStore reads go through the
    /// lineage-aware variants so a branch serves pre-branch-point
    /// data from the ancestor's rows. Writes always target
    /// `branch_id` — never an ancestor. Set once before the writer
    /// is shared (see [`Self::set_lineage`]); defaults to the trivial
    /// single-segment lineage.
    lineage: Vec<LineageSegment>,
    compute_page_store: Arc<PageStore>,
    hot_path_page_store: Arc<PageStore>,
    policy: FlushPolicy,
    /// Per-key page cache + pre-flush buffer. Internally thread-safe
    /// — every method takes `&self`. Per-key state lives in a lock-free
    /// ordered `SkipMap` (one `Mutex<KeyState>` per key); aggregate
    /// counters are atomics; the eviction LRU and flush FIFO each have
    /// their own small `parking_lot::Mutex`. Concurrent getpage reads go
    /// straight to the per-key mutexes and never serialize on a global
    /// lock — that was the bottleneck the per-key state was introduced
    /// to fix (writer-starvation when the materialiser waited behind
    /// many concurrent read holders on a single `RwLock<WriterState>`).
    pages: crate::unified_pages::UnifiedPages,
    /// Aux (non-rel) flush buffers + the "any unflushed work?"
    /// timestamp. Touched by put/flush plus the rare meta/SLRU reads
    /// that blend buffered with persisted. Hot path doesn't touch
    /// this. The flush side drains by COUNT (`drain(0..slru_count)`)
    /// in a critical section separate from the snapshot, so it is
    /// only sound when at most one `flush()` runs at a time — see
    /// `flush_serialize`.
    aux: PlMutex<AuxBuffers>,
    /// Total rows ever appended to the buffer (rel + slru + meta).
    /// Reported in `WriterStats`; not load-bearing.
    rows_written: AtomicU64,
    /// Total single-key `scan_meta_range` calls served from the in-process
    /// LRU cache without a YB round-trip. Reported in `WriterStats`.
    meta_cache_hits: AtomicU64,
    /// Per-key tally of consecutive non-image, non-`will_init` deltas
    /// since the last anchor for that key. Both fields advance on
    /// each non-anchor `put`; either hitting its
    /// `MaterializerHandle` threshold dispatches a job and removes
    /// the entry.
    delta_counts: DashMap<Key, DeltaTally>,
    /// Keys over their materialize threshold whose job is not enqueued yet:
    /// SLRU keys until the next flush, relation keys whose enqueue failed at
    /// the crossing until the flusher's tick retries them. Lets those
    /// paths skip a walk of all of `delta_counts` (#21). Every reader
    /// re-checks the tally, so a stale member is harmless.
    keys_to_materialize: DashSet<Key>,
    materializer: Option<MaterializerHandle>,
    /// GC trim-candidate collection: the shared emitter (sampling + buffering + flush)
    /// both producers feed (ingest window flush, materialiser
    /// `commit_phase`). `Some` iff collection is on. Best-effort — see
    /// [`GcTrimCandidateEmitter`].
    gc_trim_candidate_emitter: Option<Arc<GcTrimCandidateEmitter>>,
    /// Highest LSN successfully reported as durable to
    /// `amp_storage.branch_state.consistent_lsn`. Advances only
    /// after `try_write_rows` succeeds AND the subsequent
    /// `advance_consistent_lsn` succeeds. Initialised to 0 at
    /// construction; attach seeds it from the persisted row.
    consistent_lsn_watermark: std::sync::atomic::AtomicU64,
    /// Highest LSN actually durabilised in `amp_storage.pages` /
    /// `slru_pages` / `meta`. Advances on every successful flush,
    /// even when the throttle skips the YB UPDATE on
    /// `consistent_lsn`. Used by `force_upsert_consistent_lsn_if_advanced`
    /// at shutdown to persist any tail the throttle deferred.
    consistent_lsn_observed: std::sync::atomic::AtomicU64,
    /// Wall-clock timestamp of the most recent successful
    /// `advance_consistent_lsn`. Used to throttle the YB UPDATE
    /// to one per `policy.max_age` window even if the size-based
    /// flush trigger fires faster than that (a burst of rel-page
    /// writes can flush every few hundred ms; persisting the row
    /// that often would be wasted YB traffic — SK truncation is
    /// driven by this value but bounded by `max_age` is plenty
    /// fine-grained).
    ///
    /// `Mutex<Option<Instant>>` rather than atomic because the
    /// contention is nil (only the flusher writes it) and `Instant`
    /// isn't atomically representable. `None` means "no upsert yet";
    /// the first flush always writes regardless of elapsed time.
    last_consistent_lsn_upsert_at: std::sync::Mutex<Option<Instant>>,
    /// `(last_record_lsn → prev_record_lsn)` for WAL records the
    /// walreceiver has finish_write'd but PageWriter hasn't yet
    /// persisted via `advance_consistent_lsn`. Populated by
    /// `Timeline::finish_write` (which knows both values atomically
    /// from `RecordLsn::cnt_advance`). The flush path looks up the
    /// prev for the max flushed lsn and prunes entries ≤ that lsn
    /// (those records are now durable).
    ///
    /// When the lookup misses (rare race: a record's pages landed in
    /// the buffer before its `finish_write` ran, and a flush fired
    /// between the two), we persist `dc_lsn` with `prev = NULL`
    /// — same as before this tracker existed; the walreceiver
    /// re-derives prev on restart.
    record_lsn_history: std::sync::Mutex<std::collections::BTreeMap<Lsn, Lsn>>,
    /// `Arc<AtomicLsn>` of the timeline's `consistent_lsn` — the value
    /// the walreceiver loop forwards to the safekeeper in
    /// `PageserverFeedback`, which drives
    /// safekeeper WAL truncation. Bumped on every successful
    /// `advance_consistent_lsn`. Set lazily after `init_upload_queue` runs
    /// (PageWriter is constructed before the upload queue is initialized), via
    /// [`Self::set_consistent_lsn_slot`]. If unset at flush
    /// time (smoke tests, init-race), the publish is a no-op; the
    /// next flush retries.
    consistent_lsn_slot: std::sync::OnceLock<Arc<utils::lsn::AtomicLsn>>,
    /// Pulsed by `notify_waiters()` at the end of every successful
    /// `flush()`. `put()` blocks on this when the buffer exceeds
    /// `policy.backpressure_bytes`.
    flush_completed: tokio::sync::Notify,
    /// Pulsed when an SLRU key crosses its materialise threshold (its
    /// fold reads YB only, so the flusher flushes before enqueuing the job),
    /// and by a `put()` that starts waiting on backpressure. Relation keys
    /// are enqueued from `put()` itself. `notify_one` coalesces a burst, and
    /// `put()` never blocks on it. An `Arc` so the flusher task can wait on
    /// it without holding a strong ref to the writer between ticks.
    flusher_wakeup: Arc<tokio::sync::Notify>,
    /// Total puts that had to wait at least once on `flush_completed`
    /// because the buffer was at `policy.backpressure_bytes`. Exposed
    /// for telemetry / smoke tests.
    backpressure_waits: std::sync::atomic::AtomicU64,
    /// Highest LSN every buffered row at or below which is in YB.
    /// Advanced only when `mark_flushed` runs (after every chunk of a
    /// flush has committed). Bounds the post-flush and cold-residue
    /// enqueue targets, which SLRU folds need: an SLRU chain is read
    /// from YB alone.
    flush_horizon_lsn: std::sync::atomic::AtomicU64,
    /// Serializes the whole of `flush()`. The periodic flusher
    /// (`spawn_flusher`) and every `flush` caller
    /// (checkpoints, basebackup, bootstrap import-finish, HTTP, the
    /// shutdown drain) all invoke `flush()` from independent tasks.
    /// `flush()` snapshots the aux buffer in one critical section and
    /// drains it by count (`drain(0..slru_count)`) in a later one; if
    /// two flushes overlap, the second's count was computed against a
    /// buffer the first already drained, so its `drain` removes fresh
    /// rows that were never written to YB — a silent SLRU/meta loss.
    /// The rel FIFO drains the same way: `mark_flushed` drops the front
    /// entries its snapshot claimed, by count, and `FlushClaim` hands
    /// every claim back on failure. Holding this across the entire
    /// flush makes snapshot+drain atomic with respect to other
    /// flushes. `put()` never takes it, so ingest is not serialized
    /// against flush.
    flush_serialize: tokio::sync::Mutex<()>,
    /// Rel bytes of the in-flight flush whose chunk has landed in YB; they
    /// still count in `pages.unflushed_bytes()` until the flush marks them
    /// (a row an image promoted mid-flush already left it, so the discount
    /// can run ahead by that row until the mark). `await_backpressure`
    /// discounts them when `policy.release_backpressure_per_chunk` is set.
    landed_unmarked_bytes: AtomicUsize,
    /// In-process LRU cache for `amp_storage.meta` point reads, keyed by
    /// raw key bytes. `None` when disabled.
    ///
    /// Invariant: when an entry is present, it holds the highest-LSN
    /// `MetaRow` seen for that key since it was last evicted. `put()`
    /// only overwrites an existing entry when the incoming LSN is strictly
    /// higher, so an idempotent re-write of the same (key, lsn) leaves the
    /// cached entry untouched.
    ///
    /// Reads: a single-key hit at `cached.lsn ≤ target_lsn` is returned
    /// immediately — no YB round-trip. This is correct because:
    ///   1. `PageWriter` is the sole writer for its timeline; any newer
    ///      write would have overwritten this cache entry.
    ///   2. WAL ordering guarantees writes for a key arrive with strictly
    ///      increasing LSNs, so an evict-then-older-write scenario cannot
    ///      occur within a single `PageWriter` lifetime.
    ///
    /// Cache misses (key never written here, or evicted by LRU) fall
    /// through to the normal YB + aux-buffer blend, which also covers
    /// ancestor timeline segments.
    meta_cache: Option<PlMutex<LruCache<Bytes, MetaRow>>>,
    #[cfg(test)]
    scan_pause: PlMutex<Option<Arc<ScanPause>>>,
    /// Test hook: the next flush fails once its rel rows have landed.
    #[cfg(test)]
    fail_write_after_rel: std::sync::atomic::AtomicBool,
}

/// Test hook: holds a multi-block scan between its cache read and its YB read.
#[cfg(test)]
#[derive(Default)]
struct ScanPause {
    reached: tokio::sync::Notify,
    resume: tokio::sync::Notify,
}

/// A flush's claim on the unflushed rows, from its snapshot until its mark.
/// Settled unmarked — the write failed, or the `flush()` future was dropped
/// mid-way — it hands the claimed rows back for the next flush and stops
/// discounting their landed bytes. It is dropped before `flush_serialize`'s
/// guard, so no other snapshot can interleave.
struct FlushClaim<'a> {
    writer: &'a PageWriter,
    /// Rel bytes of this flush whose chunk has landed.
    landed: AtomicUsize,
    settled: bool,
}

impl FlushClaim<'_> {
    /// `marked`: the rows were marked durable, which already took them out of
    /// `unflushed_bytes`. Otherwise they go back to the unclaimed FIFO.
    fn settle(&mut self, marked: bool) {
        if self.settled {
            return;
        }
        self.settled = true;
        let w = self.writer;
        if !marked {
            w.pages.release_claims();
        }
        let landed = self.landed.load(Ordering::Relaxed);
        let _ = w
            .landed_unmarked_bytes
            .fetch_update(Ordering::Relaxed, Ordering::Relaxed, |v| {
                Some(v.saturating_sub(landed))
            });
        if !marked {
            // The released rows count against backpressure again.
            w.flush_completed.notify_waiters();
        }
    }
}

impl Drop for FlushClaim<'_> {
    fn drop(&mut self) {
        self.settle(false);
    }
}

/// Non-rel flush buffers, kept under a small `parking_lot::Mutex`.
/// Put/flush are the only producers; meta/SLRU reads briefly snapshot
/// here too. Holds are O(rows-in-this-flush), which is tiny.
struct AuxBuffers {
    slru_buffer: Vec<SlruPageRow>,
    meta_buffer: Vec<MetaRow>,
    /// GC drop/shrink events (`GcKind::Drop*` / `ShrinkPages`). One-shot, so they
    /// ride the window flush for its durability: a lost buffer implies
    /// the producing record replays and re-emits. Trim candidates go
    /// through [`GcTrimCandidateEmitter`] instead.
    gc_event_buffer: Vec<PendingGcRow>,
    /// Bytes pending durability for the aux buffers — SLRU + meta +
    /// pending_gc events. Rel-page unflushed bytes live on
    /// `pages.unflushed_bytes()`. Drives the non-rel side of the
    /// size-based flush trigger.
    bytes_in_aux_buffers: usize,
    /// Start of the current flush window. Drives the age-based flush
    /// trigger; `None` when the buffer is empty, and restarted by a flush
    /// that leaves rows behind.
    opened_at: Option<Instant>,
}

#[derive(Debug, Clone, Default)]
pub struct WriterStats {
    pub rows_written: u64,
    pub bytes_in_buffer: usize,
    pub buffered_rows: usize,
    pub buffered_slru_rows: usize,
    pub buffered_meta_rows: usize,
    /// Number of `scan_meta_range` single-key calls that returned a
    /// result directly from the in-process LRU cache, skipping YB.
    pub meta_cache_hits: u64,
}

impl PageWriter {
    // Wide signature: constructor wires up all collaborators in one place.
    #[allow(clippy::too_many_arguments)]
    pub fn new(
        branch_id: BranchId,
        compute_page_store: Arc<PageStore>,
        hot_path_page_store: Arc<PageStore>,
        policy: FlushPolicy,
        read_cache_bytes: usize,
        meta_cache_entries: usize,
    ) -> Self {
        Self {
            branch_id,
            lineage: LineageSegment::single(branch_id),
            compute_page_store,
            hot_path_page_store,
            policy,
            pages: crate::unified_pages::UnifiedPages::new(read_cache_bytes),
            aux: PlMutex::new(AuxBuffers {
                slru_buffer: Vec::new(),
                meta_buffer: Vec::new(),
                gc_event_buffer: Vec::new(),
                bytes_in_aux_buffers: 0,
                opened_at: None,
            }),
            gc_trim_candidate_emitter: None,
            rows_written: AtomicU64::new(0),
            meta_cache_hits: AtomicU64::new(0),
            delta_counts: DashMap::new(),
            keys_to_materialize: DashSet::new(),
            materializer: None,
            consistent_lsn_watermark: std::sync::atomic::AtomicU64::new(0),
            consistent_lsn_observed: std::sync::atomic::AtomicU64::new(0),
            last_consistent_lsn_upsert_at: std::sync::Mutex::new(None),
            record_lsn_history: std::sync::Mutex::new(std::collections::BTreeMap::new()),
            consistent_lsn_slot: std::sync::OnceLock::new(),
            flush_completed: tokio::sync::Notify::new(),
            flusher_wakeup: Arc::new(tokio::sync::Notify::new()),
            backpressure_waits: std::sync::atomic::AtomicU64::new(0),
            flush_horizon_lsn: std::sync::atomic::AtomicU64::new(0),
            flush_serialize: tokio::sync::Mutex::new(()),
            landed_unmarked_bytes: AtomicUsize::new(0),
            meta_cache: (meta_cache_entries > 0)
                .then(|| PlMutex::new(LruCache::new(meta_cache_entries))),
            #[cfg(test)]
            scan_pause: PlMutex::new(None),
            #[cfg(test)]
            fail_write_after_rel: std::sync::atomic::AtomicBool::new(false),
        }
    }

    #[cfg(test)]
    async fn pause_between_cache_and_yb(&self) {
        let pause = self.scan_pause.lock().clone();
        if let Some(p) = pause {
            p.reached.notify_one();
            p.resume.notified().await;
        }
    }

    /// Seed the in-memory `consistent_lsn` watermark from the
    /// persisted timeline row at attach time. Without this, the first
    /// post-restart flush would upsert a smaller LSN than what's
    /// already in YB (a no-op write but wasted round-trip). Idempotent
    /// — uses `fetch_max` so a later flush that already advanced the
    /// watermark isn't rolled back.
    pub fn seed_consistent_lsn_watermark(&self, lsn: utils::lsn::Lsn) {
        use std::sync::atomic::Ordering::Relaxed;
        let prev = self.consistent_lsn_watermark.load(Relaxed);
        if lsn.0 > prev {
            // Single writer at attach; no race with the flush loop yet.
            self.consistent_lsn_watermark.store(lsn.0, Relaxed);
        }
    }

    /// Borrow the internal-traffic `PageStore`. Used by
    /// `Timeline::write_timeline_row_to_yb` (Phase 5b) and other
    /// callers that need to issue side-effects (the timeline-row upsert)
    /// on the same connection the flush path uses.
    pub fn hot_path_page_store(&self) -> &Arc<PageStore> {
        &self.hot_path_page_store
    }

    /// Bypass-throttle dc_lsn upsert for shutdown. The periodic flush
    /// path throttles the YB UPDATE on the timeline row to one per
    /// `policy.max_age`; on a clean shutdown we may have skipped the
    /// last few flushes' worth of dc_lsn advances. Catch them up by
    /// upserting `consistent_lsn_observed` if it exceeds the watermark.
    /// No-op if the watermark is already at or past the observed
    /// value.
    pub async fn force_upsert_consistent_lsn_if_advanced(&self) -> Result<()> {
        use std::sync::atomic::Ordering::Relaxed;
        let observed = self.consistent_lsn_observed.load(Relaxed);
        let watermark = self.consistent_lsn_watermark.load(Relaxed);
        if observed <= watermark {
            return Ok(());
        }
        let lsn = utils::lsn::Lsn(observed);
        // Pair dc_lsn with the prev_record_lsn the walreceiver
        // noted for this record. `None` falls through to a NULL
        // column — the walreceiver re-derives prev on restart.
        let prev = self.take_prev_at_or_below(lsn).map(|(_, p)| p);
        self.hot_path_page_store
            .advance_consistent_lsn(self.branch_id, lsn, prev)
            .await?;
        self.consistent_lsn_watermark.store(observed, Relaxed);
        *self.last_consistent_lsn_upsert_at.lock().unwrap() = Some(Instant::now());
        self.publish_consistent_lsn(lsn);
        Ok(())
    }

    /// Install the `Arc<AtomicLsn>` slot the walreceiver forwards to
    /// the safekeeper. Called once after `RemoteTimelineClient::init_upload_queue`
    /// materialises the slot. Safe to call concurrently; idempotent —
    /// the OnceLock rejects a second `set` silently.
    pub fn set_consistent_lsn_slot(&self, slot: Arc<utils::lsn::AtomicLsn>) {
        let _ = self.consistent_lsn_slot.set(slot);
    }

    /// Bump the safekeeper-truncation slot. Called from the gen-gated
    /// flush success path; the value is already durable in YB
    /// (`amp_storage.branch_state.consistent_lsn`) so the
    /// advertise can't lie. No-op if the slot hasn't been installed
    /// yet (init race / smoke tests).
    fn publish_consistent_lsn(&self, lsn: utils::lsn::Lsn) {
        if let Some(slot) = self.consistent_lsn_slot.get() {
            slot.fetch_max(lsn);
        }
    }

    /// Install the image-materialiser handle. Counter logic in `put`
    /// is a no-op until this is called. Must be called before any
    /// `put` traffic for that traffic to be observed.
    /// Install the lineage built by [`PageStore::load_lineage`].
    /// Must be called before the writer is shared (same wiring window
    /// as [`Self::set_materializer`]); `lineage[0]` must be this
    /// writer's own timeline.
    pub fn set_lineage(&mut self, lineage: Vec<LineageSegment>) {
        assert!(
            lineage.first().map(|s| s.branch_id) == Some(self.branch_id),
            "lineage[0] must be the writer's own timeline ({}), got {:?}",
            self.branch_id,
            lineage.first(),
        );
        self.lineage = lineage;
    }

    pub fn set_materializer(&mut self, handle: MaterializerHandle) {
        self.materializer = Some(handle);
    }

    /// Enable GC trim-candidate collection: creates the shared
    /// [`GcTrimCandidateEmitter`] (one candidate per `every_n_bases`
    /// bases per block; zero clamps to 1). Mirrors the
    /// `gc_candidate_every_n_bases` tenant-config knob.
    pub fn set_gc_candidate_every_n(
        &mut self,
        every_n_bases: u32,
        applied_gc_cutoff: Arc<Rcu<Lsn>>,
    ) {
        self.gc_trim_candidate_emitter = Some(Arc::new(GcTrimCandidateEmitter::new(
            self.branch_id,
            Arc::clone(&self.hot_path_page_store),
            applied_gc_cutoff,
            every_n_bases,
        )));
    }

    /// Record a producer's base writes for GC trim — rel / SLRU bases or
    /// `meta` puts; the row type ([`TrimBase`]) decides what a base is and
    /// its kind (sampling + metric + buffering, see
    /// [`GcTrimCandidateEmitter::record_bases`]). No-op when collection is
    /// off.
    pub(crate) fn record_gc_trim_bases<'a, R: TrimBase + 'a>(
        &self,
        rows: impl IntoIterator<Item = &'a R>,
        path: crate::metrics::GcEmitPath,
    ) {
        if let Some(emitter) = &self.gc_trim_candidate_emitter {
            emitter.record_bases(rows, path);
        }
    }

    /// Spawn the emitter's flusher (`None` when collection is off).
    /// Shut it down only after BOTH producers have stopped — the
    /// cancel path does the final flush that covers the tail.
    pub fn spawn_gc_trim_flusher(&self) -> Option<FlushTask> {
        let emitter = Arc::clone(self.gc_trim_candidate_emitter.as_ref()?);
        let cancel = CancellationToken::new();
        let handle = tokio::spawn(
            emitter
                .flush_loop(cancel.clone())
                .instrument(info_span!("pending_gc_flusher", branch_id = %self.branch_id)),
        );
        Some(FlushTask { handle, cancel })
    }

    /// Whether GC is on for this writer — the `enable_gc` master
    /// switch, frozen at activation as the emitter's presence
    /// (wired via [`Self::set_gc_candidate_every_n`]).
    pub(crate) fn gc_enabled(&self) -> bool {
        self.gc_trim_candidate_emitter.is_some()
    }

    /// Buffer drop/shrink event candidates (`GcKind::Drop*` / `ShrinkPages`)
    /// for the next window flush; no-op when GC is off. Unsampled —
    /// sampling a one-shot event away would lose it permanently.
    pub(crate) fn push_gc_drop_shrink_events(&self, rows: &[PendingGcRow]) {
        if !self.gc_enabled() || rows.is_empty() {
            return;
        }
        for row in rows {
            let path = match row.gc_kind {
                crate::page_store::GcKind::ShrinkPages => crate::metrics::GcEmitPath::Shrink,
                crate::page_store::GcKind::DropSlru
                | crate::page_store::GcKind::DropMeta
                | crate::page_store::GcKind::DropDb
                | crate::page_store::GcKind::DropRel => crate::metrics::GcEmitPath::Drop,
                // Exhaustive, no wildcard: a new kind must be classified here
                // at compile time rather than mislabelled in release and
                // panicking in debug.
                other @ (crate::page_store::GcKind::TrimPages
                | crate::page_store::GcKind::TrimSlru
                | crate::page_store::GcKind::TrimMeta
                | crate::page_store::GcKind::Unknown(_)) => {
                    debug_assert!(
                        false,
                        "trim candidates go through the emitter, got {other:?}"
                    );
                    crate::metrics::GcEmitPath::Drop
                }
            };
            crate::metrics::GC_CANDIDATES_EMITTED
                .with_label_values(&[path.into()])
                .inc();
        }
        let mut aux = self.aux.lock();
        if aux.opened_at.is_none() {
            aux.opened_at = Some(Instant::now());
        }
        for row in rows {
            aux.bytes_in_aux_buffers += std::mem::size_of::<PendingGcRow>();
            aux.gc_event_buffer.push(*row);
        }
    }

    /// Objects (fork / segment) that could still receive a materialised
    /// image at an old (folded) LSN, so a drop of one must be left queued.
    /// Two sources: `in_flight` (dispatched-but-not-landed jobs) and
    /// `keys_to_materialize` (threshold-crossed keys a later flush may dispatch).
    /// Rel blocks map to their fork, SLRU blocks to their segment; anything
    /// else is filtered out. Sub-threshold `delta_counts` are excluded —
    /// they can't dispatch without new deltas and would pin the drop forever.
    pub(crate) fn objects_with_pending_materialise(&self) -> std::collections::HashSet<Key> {
        use crate::page_store::materialise_object_key;
        let Some(materializer) = self.materializer.as_ref() else {
            return Default::default();
        };
        let mut keys: std::collections::HashSet<Key> = self
            .keys_to_materialize
            .iter()
            .filter_map(|k| materialise_object_key(&k))
            .collect();
        keys.extend(
            materializer
                .in_flight
                .lock()
                .unwrap()
                .iter()
                .filter_map(materialise_object_key),
        );
        keys
    }

    pub fn branch_id(&self) -> BranchId {
        self.branch_id
    }

    /// Append a batch of entries to the in-RAM buffer. Does not flush.
    /// Routes every entry into one of the three row-store tables:
    /// - relation-page keys → `amp_storage.pages`
    /// - SLRU block keys   → `amp_storage.slru_pages`
    /// - everything else   → `amp_storage.meta` (full-overwrite)
    ///
    /// Meta entries must arrive as `Value::Image` — they're
    /// full-overwrite by convention. A `Value::WalRecord` here would
    /// signal an unexpected walredo dependency on a non-page key and is
    /// an error.
    pub async fn put(&self, entries: &[(Key, Lsn, Value)]) -> Result<()> {
        if entries.is_empty() {
            return Ok(());
        }

        self.await_backpressure().await;

        let mut rel_rows = Vec::with_capacity(entries.len());
        let mut slru_rows = Vec::new();
        let mut meta_rows = Vec::new();
        let mut slru_mat = Vec::new();
        for (key, lsn, value) in entries {
            if key.is_rel_block_key() {
                rel_rows.push(build_page_row(*key, *lsn, value)?);
            } else if key.is_slru_block_key() {
                slru_rows.push(build_slru_page_row(*key, *lsn, value)?);
                slru_mat.push((*key, *lsn, slru_resets_chain(value)));
            } else {
                meta_rows.push(build_meta_row(*key, *lsn, value)?);
            }
        }
        self.push_rows(rel_rows, slru_rows, meta_rows, slru_mat)
    }

    /// Apply a serialized WAL value batch. With `ingest_parallelism > 1`
    /// (and a large enough batch) the work is partitioned by page key
    /// across blocking workers, each of which deserializes, builds, AND
    /// pushes its keys' rows. Distinct keys live in independent per-key
    /// DashMap shards so they apply concurrently; every row for a given key
    /// lands on one worker, preserving that key's chain / ascending-LSN
    /// order.
    ///
    /// This advances no visibility watermark: `put_batch` bumps
    /// `applied_lsn` only after this returns, and reads filter each key's
    /// chain by their requested LSN — so a partially-applied batch is never
    /// observable, even across the parallel workers.
    pub async fn put_serialized(self: &Arc<Self>, batch: SerializedValueBatch) -> Result<()> {
        let len = batch.metadata.len();
        if len == 0 {
            return Ok(());
        }

        self.await_backpressure().await;

        // Below this size the spawn_blocking hand-off costs more than the
        // serial apply saves.
        let parallelism = self.policy.ingest_parallelism;
        if parallelism > 1 && len >= self.policy.ingest_parallelism_min_batch {
            // Partition entry indices by page key so the same key is always
            // applied by one worker (its chain order + materialiser tally
            // stay race-free); distinct keys hit distinct shards and apply
            // in parallel.
            let batch = Arc::new(batch);
            let mut buckets: Vec<Vec<usize>> = vec![Vec::new(); parallelism];
            for (i, meta) in batch.metadata.iter().enumerate() {
                if matches!(meta, ValueMeta::Serialized(_)) {
                    let mut hasher = std::collections::hash_map::DefaultHasher::new();
                    meta.key().hash(&mut hasher);
                    let bucket = (hasher.finish() % parallelism as u64) as usize;
                    buckets[bucket].push(i);
                }
            }
            let mut handles = Vec::new();
            for indices in buckets {
                if indices.is_empty() {
                    continue;
                }
                let this = Arc::clone(self);
                let batch = Arc::clone(&batch);
                handles.push(tokio::task::spawn_blocking(move || {
                    this.apply_indices(&batch, &indices)
                }));
            }
            for handle in handles {
                handle
                    .await
                    .map_err(|e| anyhow::anyhow!("ingest apply task failed: {e}"))??;
            }
            Ok(())
        } else {
            let (rel_rows, slru_rows, meta_rows, slru_mat) =
                build_rows_from_metas(&batch, batch.metadata.iter())?;
            self.push_rows(rel_rows, slru_rows, meta_rows, slru_mat)
        }
    }

    /// Deserialize, build, and push the page rows for `indices` — a
    /// key-partition of the batch (every row for any key it touches is in
    /// this slice, in ascending-LSN batch order). Pushes go to the per-key
    /// shards, so concurrent workers on disjoint keys don't contend.
    fn apply_indices(&self, batch: &SerializedValueBatch, indices: &[usize]) -> Result<()> {
        let (rel_rows, slru_rows, meta_rows, slru_mat) =
            build_rows_from_metas(batch, indices.iter().map(|&i| &batch.metadata[i]))?;
        self.push_rows(rel_rows, slru_rows, meta_rows, slru_mat)
    }

    /// Backpressure: if the buffer is already at or above the configured
    /// hard cap, block until a flush drains it below the cap. The
    /// walreceiver is the only producer that hits this path, so stalling
    /// here pushes the slowdown all the way up to safekeeper ack — which
    /// is exactly what we want when YB INSERTs can't keep up. Without
    /// this, the buffer grows unboundedly under CPU pressure (pre-flush
    /// rows are pinned in `UnifiedPages` and `evict_to_capacity` can't
    /// touch them).
    ///
    /// `notified()` is registered before the size check (via `enable()`)
    /// to close the race where a flush completes between our check and
    /// our wait.
    async fn await_backpressure(&self) {
        if self.policy.backpressure_bytes > 0 {
            let mut waited = false;
            loop {
                let notified = self.flush_completed.notified();
                tokio::pin!(notified);
                notified.as_mut().enable();

                // Rows whose chunk already landed in YB don't count: they
                // only wait for their flush to mark them (zero unless
                // `release_backpressure_per_chunk`). They still hold memory,
                // and a flush that fails after landing them hands them back to
                // be written again, so each failed retry could admit another
                // cap's worth: the undiscounted total stops ingest at twice
                // the cap, which a flush that succeeds never reaches.
                let cap = self.policy.backpressure_bytes;
                let held = self.pages.unflushed_bytes() + self.aux.lock().bytes_in_aux_buffers;
                let pending =
                    held.saturating_sub(self.landed_unmarked_bytes.load(Ordering::Relaxed));
                if pending < cap && held < cap.saturating_mul(2) {
                    break;
                }
                if !waited {
                    waited = true;
                    self.backpressure_waits
                        .fetch_add(1, std::sync::atomic::Ordering::Relaxed);
                    crate::metrics::PAGEWRITER_BACKPRESSURE_WAITS
                        .with_label_values(&["flush"])
                        .inc();
                    tracing::debug!(
                        backpressure_bytes = self.policy.backpressure_bytes,
                        "PageWriter::put backpressuring on flush",
                    );
                    // Flush now rather than at the next tick: a buffer this full is over the
                    // size trigger, which any wakeup flushes.
                    self.flusher_wakeup.notify_one();
                }
                notified.await;
            }
        }
    }

    /// Push pre-built rows into the in-RAM buffers and update the
    /// materialiser counters. Always serial and in caller order — this is
    /// the stage that must NOT be parallelized (per-key DashMap pushes are
    /// lock-free, but the materialiser bookkeeping and aux buffers assume
    /// ordered, single-threaded application).
    fn push_rows(
        &self,
        rel_rows: Vec<PageRow>,
        slru_rows: Vec<SlruPageRow>,
        meta_rows: Vec<MetaRow>,
        slru_mat: Vec<(Key, Lsn, bool)>,
    ) -> Result<()> {
        if rel_rows.is_empty() && slru_rows.is_empty() && meta_rows.is_empty() {
            return Ok(());
        }
        // Accumulate per-key delta counters here. A relation key's
        // crossing enqueues its job from here without blocking; an SLRU
        // key's wakes the flusher (`record_delta_for_materialize`).
        let rel_limits = self
            .materializer
            .as_ref()
            .map(|materializer| (materializer.count_limit, materializer.payload_bytes_limit))
            .filter(|(c, b)| *c > 0 || *b > 0);
        // Rel rows: per-key DashMap shards; no global lock.
        for row in rel_rows {
            let key = row.key;
            let lsn = row.lsn;
            let resets_chain = row.is_image || row.will_init;
            let payload_len = row.payload.len() as u32;
            pageserver_api::write_trace::trace_stage("put", &key, lsn);
            self.pages.push(row);
            self.rows_written.fetch_add(1, Ordering::Relaxed);
            if let Some((count_limit, payload_bytes_limit)) = rel_limits {
                self.record_delta_for_materialize(
                    &key,
                    lsn,
                    resets_chain,
                    payload_len,
                    count_limit,
                    payload_bytes_limit,
                );
            }
        }
        // SLRU (clog/multixact) rows feed the *same* materialise trigger,
        // but with the dedicated SLRU threshold. The clog page takes one
        // tiny delta per transaction commit, so without this its chain
        // grows to a full page (~32 K deltas) and every basebackup
        // re-walks it to rebuild the page (the slow phase we observed).
        // The payload trigger is disabled — clog deltas are a couple of
        // bytes. Tallied from `slru_mat` (key, lsn, resets_chain) built
        // alongside the rows, because the built `slru_rows` drop the `Key`.
        if let Some(slru_count_limit) = self
            .materializer
            .as_ref()
            .map(|materializer| materializer.slru_count_limit)
            .filter(|&c| c > 0)
        {
            for (key, lsn, resets_chain) in &slru_mat {
                self.record_delta_for_materialize(key, *lsn, *resets_chain, 0, slru_count_limit, 0);
            }
        }
        // Aux (slru + meta) buffers under one small mutex. Holds are
        // O(rows-in-this-batch); read paths blend buffered with
        // persisted but only briefly snapshot here too.
        if !slru_rows.is_empty() || !meta_rows.is_empty() {
            {
                let mut aux = self.aux.lock();
                if aux.opened_at.is_none() {
                    aux.opened_at = Some(Instant::now());
                }
                for row in slru_rows {
                    aux.bytes_in_aux_buffers += slru_row_size(&row);
                    aux.slru_buffer.push(row);
                    self.rows_written.fetch_add(1, Ordering::Relaxed);
                }
                for row in &meta_rows {
                    aux.bytes_in_aux_buffers += meta_row_size(row);
                    aux.meta_buffer.push(row.clone());
                    self.rows_written.fetch_add(1, Ordering::Relaxed);
                }
            }
            // Write-through: populate the meta cache outside the aux lock so
            // reads hitting the cache never block on an aux write.
            if let Some(cache) = &self.meta_cache {
                let mut cache = cache.lock();
                for row in meta_rows {
                    match cache.get(&row.key) {
                        Some(existing) if existing.lsn >= row.lsn => {}
                        _ => {
                            cache.insert(row.key.clone(), row);
                        }
                    }
                }
            }
        } else {
            // Even when only rel rows arrived we still need to arm
            // the age-based flush timer.
            let mut aux = self.aux.lock();
            if aux.opened_at.is_none() {
                aux.opened_at = Some(Instant::now());
            }
        }

        Ok(())
    }

    /// Enqueue a batch of SLRU materialise jobs from `flush()`, waiting for
    /// room in the materialiser's channel. Caller must have already reserved
    /// each job's key in `in_flight`.
    async fn enqueue_slru_materialize_jobs(&self, to_materialize: Vec<MaterializeJob>) {
        if to_materialize.is_empty() {
            return;
        }
        let Some(materializer) = self.materializer.as_ref() else {
            return;
        };
        let dispatched = to_materialize.len();
        let queue_cap = materializer.tx.max_capacity();
        let queue_remaining_before = materializer.tx.capacity();
        let mut waited_for_capacity = false;
        for job in to_materialize {
            // Blocking send: backpressure the walreceiver if the
            // materialiser can't keep up. Silent try_send drops were
            // causing deep chains to accumulate during write bursts
            // and stay deep forever once ingest went idle (no later
            // put would re-trigger after the counter cleared).
            // Receiver lives as long as the timeline, so SendError
            // here means timeline shutdown — undo the in_flight
            // reservation for this and remaining jobs so a later
            // restart's trigger isn't blocked.
            if !waited_for_capacity && materializer.tx.capacity() == 0 {
                waited_for_capacity = true;
                crate::metrics::PAGEWRITER_BACKPRESSURE_WAITS
                    .with_label_values(&["materializer"])
                    .inc();
            }
            let key_for_rollback = job.key;
            if let Err(e) = materializer.tx.send(job).await {
                tracing::debug!("materialiser send dropped (receiver closed): {e}");
                materializer
                    .in_flight
                    .lock()
                    .unwrap()
                    .remove(&key_for_rollback);
                break;
            }
        }
        crate::metrics::MATERIALIZER_QUEUE_DEPTH.set(
            materializer
                .tx
                .max_capacity()
                .saturating_sub(materializer.tx.capacity()) as u64,
        );
        let queue_remaining_after = materializer.tx.capacity();
        if waited_for_capacity || queue_remaining_after < queue_cap / 2 {
            info!(
                tag = "materialise_dispatch",
                dispatched = dispatched,
                queue_cap = queue_cap,
                queue_remaining_before = queue_remaining_before,
                queue_remaining_after = queue_remaining_after,
                waited_for_capacity = waited_for_capacity,
                "perf",
            );
        }
    }

    /// Advance the per-key delta tally and act on a threshold crossing: a
    /// relation key's job is enqueued at once, at the crossing delta, and only
    /// a failed enqueue lands in `keys_to_materialize` for the flusher's tick to retry.
    /// An SLRU key lands in `keys_to_materialize` and wakes the flusher, since its
    /// fold reads YB only. An anchor (`resets_chain` — image or
    /// `will_init`) drops the key instead, since its chain no longer needs
    /// collapsing.
    fn record_delta_for_materialize(
        &self,
        key: &Key,
        lsn: Lsn,
        resets_chain: bool,
        payload_len: u32,
        count_limit: u32,
        payload_bytes_limit: u32,
    ) {
        if resets_chain {
            self.delta_counts.remove(key);
            self.keys_to_materialize.remove(key);
            return;
        }
        let (crossed, last_lsn) = {
            let mut tally = self.delta_counts.entry(*key).or_default();
            let was_hit = tally.hits(count_limit, payload_bytes_limit);
            tally.count = tally.count.saturating_add(1);
            tally.payload_bytes = tally.payload_bytes.saturating_add(payload_len);
            if lsn > tally.last_lsn {
                tally.last_lsn = lsn;
            }
            // Crossed iff this put pushed the tally from below the
            // threshold to at/above it. (Shard guard drops here, before
            // `in_flight` or the ready-set: never nested.)
            (
                tally.hits(count_limit, payload_bytes_limit) && !was_hit,
                tally.last_lsn,
            )
        };
        if !crossed {
            return;
        }
        if key.is_slru_block_key() {
            self.keys_to_materialize.insert(*key);
            self.flusher_wakeup.notify_one();
        } else if !self.try_enqueue_rel_materialize_job(*key, last_lsn) {
            self.keys_to_materialize.insert(*key);
        }
    }

    /// The materialiser, unless there is none or folds are held off: during
    /// attach replay a fold can stamp an image above content it never saw
    /// (#240).
    fn materializer_if_folds_allowed(&self) -> Option<&MaterializerHandle> {
        self.materializer
            .as_ref()
            .filter(|materializer| materializer.folds_allowed())
    }

    /// Enqueue a relation key's materialise job without waiting, and restart its
    /// tally. False when folds are held off, the key is already being
    /// folded, or the materialiser channel is full.
    fn try_enqueue_rel_materialize_job(&self, key: Key, target_lsn: Lsn) -> bool {
        let Some(materializer) = self.materializer_if_folds_allowed() else {
            return false;
        };
        {
            let mut in_flight = materializer.in_flight.lock().unwrap();
            if !in_flight.insert(key) {
                return false;
            }
            if materializer
                .tx
                .try_send(MaterializeJob { key, target_lsn })
                .is_err()
            {
                in_flight.remove(&key);
                return false;
            }
        }
        self.delta_counts.remove(&key);
        crate::metrics::MATERIALIZER_QUEUE_DEPTH.set(
            materializer
                .tx
                .max_capacity()
                .saturating_sub(materializer.tx.capacity()) as u64,
        );
        true
    }

    /// Retry the relation keys whose enqueue failed at the crossing, at
    /// their newest buffered delta. Runs on the flusher's tick, so a key
    /// that stops receiving writes still gets folded.
    fn retry_enqueue_rel_materialize_jobs(&self) {
        let Some(materializer) = self.materializer_if_folds_allowed() else {
            return;
        };
        let pending: Vec<Key> = self
            .keys_to_materialize
            .iter()
            .map(|k| *k.key())
            .filter(|k| !k.is_slru_block_key())
            .collect();
        for key in pending {
            if materializer.tx.capacity() == 0 {
                break;
            }
            // A put can reset the key between the walk and this lookup.
            let tally = self.delta_counts.get(&key).map(|t| *t);
            match tally {
                Some(t) if t.hits(materializer.count_limit, materializer.payload_bytes_limit) => {
                    if self.try_enqueue_rel_materialize_job(key, t.last_lsn) {
                        self.keys_to_materialize.remove(&key);
                    }
                }
                _ => {
                    self.keys_to_materialize.remove(&key);
                }
            }
        }
    }

    /// Whether an SLRU page, not already being materialized, waits for a
    /// flush to enqueue its job. The flusher flushes on a wakeup when it
    /// does. Keys are collected
    /// before `in_flight` is locked, matching the enqueue paths' order
    /// (`keys_to_materialize` shard, then `in_flight`).
    fn slru_materialize_jobs_pending_flush(&self) -> bool {
        // While folds are held off, a flush would enqueue nothing.
        let Some(materializer) = self.materializer_if_folds_allowed() else {
            return false;
        };
        let slru: Vec<Key> = self
            .keys_to_materialize
            .iter()
            .filter(|k| k.key().is_slru_block_key())
            .map(|k| *k.key())
            .collect();
        if slru.is_empty() {
            return false;
        }
        let in_flight = materializer.in_flight.lock().unwrap();
        slru.iter().any(|k| !in_flight.contains(k))
    }

    /// Take a materialise job for each SLRU crossing, at or below `flush_horizon_lsn`:
    /// an SLRU fold reads YB alone, so its target must be durable. A tally
    /// whose newest delta is above the horizon is imaged at the horizon when
    /// `clamp_to_horizon` (after a flush: the active CLOG/multixact page
    /// races ahead of it, #115) and left for a later flush otherwise (the
    /// nothing-to-flush path, which drains cold residue once compute goes
    /// quiet). Relation keys are enqueued from `put()`
    /// ([`Self::record_delta_for_materialize`]).
    /// A taken key is reserved in `in_flight` and leaves both maps, so the
    /// caller must enqueue its job; stale hints leave only `keys_to_materialize`.
    fn take_slru_materialize_jobs(&self, clamp_to_horizon: bool) -> Vec<MaterializeJob> {
        let Some(materializer) = self.materializer_if_folds_allowed() else {
            return Vec::new();
        };
        if materializer.slru_count_limit == 0 {
            return Vec::new();
        }
        let horizon = Lsn(self.flush_horizon_lsn.load(Ordering::Relaxed));
        if horizon == Lsn(0) {
            return Vec::new();
        }
        let mut jobs = Vec::new();
        let mut stale: Vec<Key> = Vec::new();
        for entry in self.keys_to_materialize.iter() {
            let key = entry.key();
            if !key.is_slru_block_key() {
                continue;
            }
            let Some(tally) = self.delta_counts.get(key).map(|t| *t) else {
                stale.push(*key);
                continue;
            };
            if !tally.hits(materializer.slru_count_limit, 0) {
                stale.push(*key);
                continue;
            }
            let target_lsn = if tally.last_lsn == Lsn(0) {
                continue;
            } else if tally.last_lsn <= horizon {
                tally.last_lsn
            } else if clamp_to_horizon {
                horizon
            } else {
                continue;
            };
            if materializer.in_flight.lock().unwrap().insert(*key) {
                jobs.push(MaterializeJob {
                    key: *key,
                    target_lsn,
                });
            }
        }
        for job in &jobs {
            self.delta_counts.remove(&job.key);
            self.keys_to_materialize.remove(&job.key);
        }
        for k in &stale {
            self.keys_to_materialize.remove(k);
        }
        jobs
    }

    /// Range scan: return all page rows in `range` at LSN ≤
    /// `target_lsn`, blending the in-RAM UnifiedPages (both pre-flush
    /// and post-flush cache) with PageStore's persisted rows. Each
    /// per-key chain is LSN-descending, deduplicated by LSN, and
    /// truncated at the first `will_init=true` row.
    pub async fn scan_range(
        &self,
        range: std::ops::Range<Key>,
        target_lsn: Lsn,
    ) -> Result<std::collections::HashMap<Key, Vec<Arc<PageRow>>>> {
        let t0 = Instant::now();
        // GetPage hot path: a 1-key vectored read arrives as
        // `[k..k.next())`. Dispatch to `read_chain`, which holds the
        // cache-only fast path; on a hit we skip YB entirely.
        if crate::page_store::single_block_range(&range) {
            let chain = self.read_chain(range.start, target_lsn).await?;
            let mut out = std::collections::HashMap::with_capacity(1);
            if !chain.is_empty() {
                out.insert(range.start, chain);
            }
            return Ok(out);
        }
        // Cache before YB: a row leaves the cache only once it is in YB.
        let cached: std::collections::HashMap<Key, Vec<Arc<PageRow>>> =
            self.pages
                .lookup_range(&range.start, &range.end, target_lsn);
        let t_cache = t0.elapsed();
        #[cfg(test)]
        self.pause_between_cache_and_yb().await;
        let t_yb_start = Instant::now();
        let persisted = self
            .compute_page_store
            .scan_range_lineage(&self.lineage, range.clone(), target_lsn)
            .await?;
        let t_yb = t_yb_start.elapsed();
        let persisted_rows: usize = persisted.values().map(|v| v.len()).sum();
        let cached_rows: usize = cached.values().map(|v| v.len()).sum();
        let t_merge_start = Instant::now();
        let mut out: std::collections::HashMap<Key, Vec<Arc<PageRow>>> =
            std::collections::HashMap::with_capacity(persisted.len() + cached.len());
        for (k, chain) in persisted {
            out.insert(k, chain);
        }
        for (k, chain) in cached {
            out.entry(k).or_default().extend(chain);
        }
        self.finalize_blended_chains(&mut out, target_lsn);
        let t_merge = t_merge_start.elapsed();
        self.log_scan_perf(
            "scan_range",
            t0,
            t_yb,
            t_cache,
            t_merge,
            persisted_rows,
            cached_rows,
            out.len(),
        );
        Ok(out)
    }

    /// Emit the shared `scan_*` perf log when the op was slow (≥5 ms). `tag`
    /// distinguishes the caller (`scan_range` / `scan_scattered_set`); the
    /// scattered fold size lives in the `pageserver_scattered_set_keys_global`
    /// metric rather than a log field.
    #[allow(clippy::too_many_arguments)]
    fn log_scan_perf(
        &self,
        tag: &'static str,
        t0: Instant,
        t_yb: Duration,
        t_cache: Duration,
        t_merge: Duration,
        yb_rows: usize,
        cache_rows: usize,
        keys_out: usize,
    ) {
        let total = t0.elapsed();
        if total.as_millis() < 5 {
            return;
        }
        info!(
            tag,
            total_ms = total.as_millis() as u64,
            yb_ms = t_yb.as_millis() as u64,
            cache_ms = t_cache.as_millis() as u64,
            merge_ms = t_merge.as_millis() as u64,
            yb_rows,
            cache_rows,
            keys_out,
            "perf",
        );
    }

    /// Multi-key ("scattered") variant of [`Self::scan_range`]: fetch
    /// chains for a SET of same-fork relation-page keys in one batched read
    /// per lineage segment (`PageStore::scan_scattered_set_lineage` — one
    /// thin-lane read per block, all riding one `Perform`, paged until every
    /// block reaches its anchor) instead of a round trip per block, then
    /// blend with the in-RAM UnifiedPages exactly as `scan_range` does.
    /// Correctness (will_init anchor, dedup, trim, read-side materialize
    /// trigger) is identical to `scan_range`; only the batching differs.
    /// Caller must pass same-fork keys.
    pub async fn scan_scattered_set(
        &self,
        keys: &[Key],
        target_lsn: Lsn,
    ) -> Result<std::collections::HashMap<Key, Vec<Arc<PageRow>>>> {
        if keys.is_empty() {
            return Ok(std::collections::HashMap::new());
        }
        let t0 = Instant::now();
        // Cache before YB, as in `scan_range`.
        let cached: Vec<(Key, Vec<Arc<PageRow>>)> = keys
            .iter()
            .map(|k| (*k, self.pages.lookup_chain(k, target_lsn)))
            .collect();
        let t_cache = t0.elapsed();
        #[cfg(test)]
        self.pause_between_cache_and_yb().await;
        let t_yb_start = Instant::now();
        let persisted = self
            .compute_page_store
            .scan_scattered_set_lineage(&self.lineage, keys, target_lsn)
            .await?;
        let t_yb = t_yb_start.elapsed();
        let mut out: std::collections::HashMap<Key, Vec<Arc<PageRow>>> =
            std::collections::HashMap::with_capacity(keys.len());
        let persisted_rows: usize = persisted.values().map(|v| v.len()).sum();
        for (k, chain) in persisted {
            out.insert(k, chain);
        }
        let mut cached_rows = 0usize;
        for (k, chain) in cached {
            if !chain.is_empty() {
                cached_rows += chain.len();
                out.entry(k).or_default().extend(chain);
            }
        }
        let t_merge_start = Instant::now();
        self.finalize_blended_chains(&mut out, target_lsn);
        let t_merge = t_merge_start.elapsed();
        self.log_scan_perf(
            "scan_scattered_set",
            t0,
            t_yb,
            t_cache,
            t_merge,
            persisted_rows,
            cached_rows,
            out.len(),
        );
        Ok(out)
    }

    /// Finalize each key's blended chain ([`merge_page_chain`]), then fire the per-key
    /// read-side materialize trigger (a chain longer than the write-side
    /// `count_limit` means the write trigger lost a job — PS crash, queue
    /// overflow — so enqueue a fresh materialise to self-heal the next read).
    /// Shared tail of [`Self::scan_range`] and [`Self::scan_scattered_set`].
    fn finalize_blended_chains(
        &self,
        out: &mut std::collections::HashMap<Key, Vec<Arc<PageRow>>>,
        target_lsn: Lsn,
    ) {
        for (key, chain) in out.iter_mut() {
            merge_page_chain(chain);
            let crossed = self.chain_crossed_segment(chain);
            self.maybe_dispatch_read_side_materialize(*key, target_lsn, chain.len(), crossed);
        }
    }

    /// Read the chain for `key` at `target_lsn`, blending UnifiedPages
    /// (pre-flush + post-flush cache) with PageStore.
    /// Read-side fallback for the write-trigger gap. When a read returns
    /// a chain longer than the write-side `count_limit`, the page either
    /// never got an image written (PS crash dropped the queued job, or
    /// a long-ago dispatch's image got stranded behind subsequent
    /// deltas faster than the materialiser could catch up). Enqueue a
    /// fresh materialise job here so the slow read self-heals.
    fn maybe_dispatch_read_side_materialize(
        &self,
        key: Key,
        target_lsn: Lsn,
        chain_len: usize,
        crossed_segment: bool,
    ) {
        let Some(materializer) = self.materializer.as_ref() else {
            return;
        };
        // Capped at the flush horizon. Conservative: folds can read
        // unflushed rows, so the cap is not needed for correctness.
        let horizon = Lsn(self
            .flush_horizon_lsn
            .load(std::sync::atomic::Ordering::Relaxed));
        if horizon.0 == 0 {
            return;
        }
        // Materialise at the read's LSN, capped at the horizon. A historical
        // read folds an image at its own LSN — useful, because future reads
        // of the same historical key (static endpoints, branches) anchor on
        // it. What makes installing a below-head image SAFE is the install
        // guard in `UnifiedPages::install_image_if_extends`: it only seeds
        // the image into the cache when the key is already present with a
        // complete suffix above the image's LSN, so the image can never
        // become a stale mid-chain anchor missing YB-only rows. See that
        // method for the full argument.
        let bounded_target = std::cmp::min(target_lsn, horizon);
        // A chain that crossed into an ancestor segment dispatches
        // regardless of length: materialising an image into the OWN
        // timeline copies the key up, so the branch converges to
        // single-tablet reads after one slow read per key instead of
        // after `count_limit` more deltas.
        let effective_len = if crossed_segment {
            usize::MAX
        } else {
            chain_len
        };
        if materializer.folds_allowed()
            && try_dispatch_read_side(
                key,
                bounded_target,
                effective_len,
                materializer.count_limit,
                &materializer.in_flight,
                &materializer.tx,
            )
        {
            crate::metrics::MATERIALIZER_READ_SIDE_DISPATCHES.inc();
        }
    }

    /// True when `chain`'s tail reaches into an ancestor lineage
    /// segment — i.e. the read had to leave this timeline's own rows.
    /// (A copy-up image installed at an LSN ≤ the branch point can
    /// false-positive here; the materialiser's "already an image"
    /// check makes the redundant dispatch a cheap no-op.)
    fn chain_crossed_segment(&self, chain: &[Arc<PageRow>]) -> bool {
        self.lineage.len() > 1
            && chain
                .last()
                .is_some_and(|r| r.lsn <= self.lineage[1].cap_lsn)
    }

    /// Whether a cache-only (own-timeline) chain is self-sufficient
    /// under lineage: its anchor must sit at or above the first ancestor's cap
    /// (`LineageSegment::shadowed_by`), otherwise ancestor rows in
    /// (anchor_lsn, cap] may exist that the cache cannot know about.
    fn cache_anchor_covers_lineage(&self, chain: &[Arc<PageRow>]) -> bool {
        match self.lineage.get(1) {
            None => true,
            Some(first_ancestor) => first_ancestor.shadowed_by(chain),
        }
    }

    /// The chain for `key` at `target_lsn`, and whether the cache alone
    /// served it. Shared by `read_chain` and `read_chain_for_materialize`,
    /// which differ only in the store a cache miss reads and the metric
    /// `source` label.
    async fn blend_cache_and_persisted_chain(
        &self,
        store: &PageStore,
        source: &'static str,
        key: Key,
        target_lsn: Lsn,
    ) -> Result<(Vec<Arc<PageRow>>, bool)> {
        // Cache-only fast path: if the cache holds a complete chain
        // (image-anchor reachable from target_lsn, no eviction in
        // between), serve from RAM and skip YB entirely. This is the
        // common case for hot pages after the materialiser has run.
        // Lock-free against put/flush/materialiser-install for
        // different keys — only the key's DashMap shard.
        //
        // Lineage caveat: the cache holds only OWN-timeline rows. A
        // cached anchor BELOW the branch cap (a materialiser copy-up
        // installed at its chain-head LSN) does not cover ancestor
        // deltas in (anchor_lsn, cap] — fall through to the lineage
        // walk in that case.
        if let Some(chain) = self.pages.lookup_chain_if_complete(&key, target_lsn) {
            if self.cache_anchor_covers_lineage(&chain) {
                crate::metrics::PAGE_READS
                    .with_label_values(&[source, "fast_cache"])
                    .inc();
                return Ok((chain, true));
            }
        }
        crate::metrics::PAGE_READS
            .with_label_values(&[source, "yb_blend"])
            .inc();
        let cached: Vec<Arc<PageRow>> = self.pages.lookup_chain(&key, target_lsn);
        let persisted = store
            .read_chain_lineage(&self.lineage, &key_to_bytes(key), target_lsn)
            .await?;
        let mut merged = cached;
        merged.extend(persisted);
        // Cached rows first, so a tied pair of bases keeps the cached one,
        // which replays to the same page.
        merge_page_chain(&mut merged);
        Ok((merged, false))
    }

    pub async fn read_chain(&self, key: Key, target_lsn: Lsn) -> Result<Vec<Arc<PageRow>>> {
        let (chain, from_cache) = self
            .blend_cache_and_persisted_chain(&self.compute_page_store, "getpage", key, target_lsn)
            .await?;
        if !from_cache {
            let crossed = self.chain_crossed_segment(&chain);
            self.maybe_dispatch_read_side_materialize(key, target_lsn, chain.len(), crossed);
        }
        Ok(chain)
    }

    /// The materialiser's chain read: `read_chain` on the internal
    /// connection, refusing an anchorless chain.
    ///
    /// Unflushed rows are folded too. An image built from them is safe
    /// even if the process crashes before they flush: YB sorts the image
    /// ahead of a same-LSN delta (PK `lsn DESC, will_init DESC`), so the
    /// replayed delta is never applied on top of it, and the rows below
    /// the image that are lost with the buffer are re-ingested before
    /// any read, branch or GC cutoff can reach their LSNs. A mid-flush
    /// partial YB view cannot leave a hole either: every row of an
    /// unflushed chunk is still in the (pinned) cache.
    pub async fn read_chain_for_materialize(
        &self,
        key: Key,
        target_lsn: Lsn,
    ) -> Result<Vec<Arc<PageRow>>> {
        // A cache miss reads on the internal conn (the one the materialiser
        // writes on), so a long walredo-and-INSERT can't stall compute.
        let (merged, _) = self
            .blend_cache_and_persisted_chain(
                &self.hot_path_page_store,
                "materializer",
                key,
                target_lsn,
            )
            .await?;
        if merged.last().is_some_and(|r| !r.will_init) {
            // No `will_init` anchor in the merged cache+YB chain ≤
            // target_lsn (#57 part 2). `parse_chain` would set
            // `base_img=None` and walredo would fold these deltas onto a
            // ZERO page; the materialiser would then install that
            // wrong/zero image, making the
            // corruption durable — surfacing later as a heap hole /
            // orphan catalog row / "unexpected zero page". A complete
            // chain must always reach a will_init anchor; its
            // absence means the page's init record is missing upstream.
            // Refuse loudly rather than fabricate a zero-based page: a
            // hard error stops the materialiser job (logged + skipped by
            // `redo_one`) instead of silently amplifying the loss.
            let key_bytes = key_to_bytes(key);
            return Err(unanchored_chain_error(
                "materialize_unanchored_chain",
                "read_chain_for_materialize",
                target_lsn,
                merged.len(),
                merged.last().map(|r| r.lsn),
                merged.first().map(|r| r.lsn),
                format_args!("key={key_bytes:?}"),
            ));
        }
        Ok(merged)
    }

    /// SLRU twin of [`Self::read_chain_for_materialize`]: the durable chain for one
    /// `(kind, segno, blknum)` block at `target_lsn`. For the materialiser.
    ///
    /// Lineage-aware because it must be. A branch only ever stamps SETBIT
    /// deltas onto the clog page active at its fork, whose `will_init` anchor
    /// sits under the ancestor's `branch_id`; an own-timeline read returns
    /// that chain anchorless, walredo rejects it for having no base image,
    /// and the materialiser's `Err` arm fatal-aborts the process (#420).
    ///
    /// Durable-only: no `aux.slru_buffer` blend (those are exactly the
    /// unflushed rows) and no cache blend (SLRU has no `self.pages`
    /// counterpart), so the store alone is the durable view.
    pub async fn read_slru_chain_durable(
        &self,
        kind: SlruKind,
        segno: u32,
        blknum: u32,
        target_lsn: Lsn,
    ) -> Result<Vec<SlruPageRow>> {
        // Always `yb_blend` — no SLRU cache means no `fast_cache` counterpart.
        // Own `source` so these don't dilute the rel cache-hit ratio.
        crate::metrics::PAGE_READS
            .with_label_values(&["materializer_slru", "yb_blend"])
            .inc();
        let chain = self
            .hot_path_page_store
            .read_slru_chain_lineage(&self.lineage, kind, segno, blknum, target_lsn)
            .await?;
        // Both halves matter: an empty chain is a legitimate no-op, and the
        // anchor scan is the only check that the walk reached one —
        // `read_slru_chain_lineage` truncates at the anchor only when
        // `lineage.len() > 1`.
        if !chain.is_empty() && !chain.iter().any(|r| r.will_init) {
            return Err(unanchored_chain_error(
                "materialize_unanchored_slru_chain",
                "read_slru_chain_durable",
                target_lsn,
                chain.len(),
                chain.last().map(|r| r.lsn),
                chain.first().map(|r| r.lsn),
                format_args!(
                    "kind={kind:?} segno={segno} blknum={blknum} \
                     lineage_segments={}",
                    self.lineage.len()
                ),
            ));
        }
        Ok(chain)
    }

    /// Install a freshly-materialised image into the cache. Call AFTER
    /// `PageStore::put_pages([image])` succeeds. Drops the deltas the
    /// image supersedes for that key (durable, lsn ≤ image.lsn, stopping
    /// at the first unflushed row) and indexes the new image as a durable
    /// cache entry; subsequent `read_chain` / `read_chain_for_materialize`
    /// find the image first and
    /// don't walk the chain back.
    ///
    /// The check-trim-insert is delegated to
    /// `UnifiedPages::install_image_if_extends`, which runs it atomically
    /// under the key's shard mutex and *skips* the install unless it keeps
    /// the cache a complete suffix (key present, image.lsn ≥ the lowest
    /// cached lsn). A skip is correct — the image is already durable in YB,
    /// so reads just fall through to the YB blend. See that method for why
    /// a below-head install would otherwise be a silent lost-update.
    pub async fn install_materialized_image(&self, image: Arc<PageRow>) {
        // No outer lock — `pages` is internally sharded. The call below
        // only touches the image's key shard plus (on a successful install)
        // the LRU and evict. Concurrent reads on other keys do not block;
        // reads on THIS key see a brief shard-lock blip.
        let t_start = Instant::now();
        let installed = self.pages.install_image_if_extends(image);
        let t_install = t_start.elapsed();
        // Only re-check capacity when we actually added bytes.
        let t_evict_start = Instant::now();
        if installed {
            self.pages.evict_to_capacity();
        }
        let t_evict = t_evict_start.elapsed();
        let total = t_start.elapsed();
        if total.as_millis() >= 5 {
            info!(
                tag = "install_image",
                total_ms = total.as_millis() as u64,
                install_us = t_install.as_micros() as u64,
                evict_us = t_evict.as_micros() as u64,
                installed,
                "perf",
            );
        }
    }

    /// Drain the buffer into one `INSERT` against `PageStore`. No-op if
    /// the buffer is empty. Resets the policy timer.
    pub async fn flush(&self) -> Result<()> {
        // Exclusive across all flushers: the aux-buffer snapshot and
        // the later count-based `drain(0..slru_count)` must see the
        // same buffer, or an overlapping flush silently drops fresh
        // SLRU/meta rows. See the `flush_serialize` field doc.
        let _flush_guard = self.flush_serialize.lock().await;
        // Capture the ingest frontier BEFORE snapshotting: the highest
        // record noted by `finish_write` (`note_record_lsn`). A noted
        // record's rows — rel, SLRU and meta — are all in the buffers
        // already, because `DatadirModification::commit` puts the whole
        // batch before calling `finish_write`. The snapshots below are
        // therefore guaranteed to contain EVERY unflushed row at
        // lsn ≤ frontier. Rows from records still mid-put can be
        // captured only partially (sharded-map iteration races the
        // put), so no durability watermark may be derived from them:
        // computing `flush_horizon_lsn` / `consistent_lsn` from
        // max(snapshot lsns) is how #22's bug B lost rows — the
        // checkpoint passed a row that had missed the snapshot, and
        // replay never re-ingested it.
        let ingest_frontier: Option<Lsn> = self
            .record_lsn_history
            .lock()
            .ok()
            .and_then(|m| m.keys().next_back().copied());
        // Snapshot. UnifiedPages snapshots the unflushed prefix
        // without removing it (rows stay visible to concurrent reads
        // during the YB round-trip — critical for walreceiver's RMW
        // on rel-size sentinels). After the INSERT succeeds we flip
        // those rows from unflushed → durable in one FIFO pass.
        let rel_snapshot = self.pages.snapshot_unflushed(None);
        // Hand the YB INSERT the same `Arc<PageRow>`s the cache slots
        // hold — just a refcount bump per row, no PageRow copy. The
        // separate `Arc<PageEntry>` snapshot stays alive so the
        // subsequent `mark_flushed` can flip durable on the exact
        // entries we wrote.
        let rel_rows: Vec<Arc<PageRow>> = rel_snapshot.iter().map(|e| e.row_arc()).collect();
        for row in &rel_rows {
            pageserver_api::write_trace::trace_stage("snapshot", &row.key, row.lsn);
        }
        let (slru_rows, meta_rows, pending_gc_rows) = {
            let aux = self.aux.lock();
            (
                aux.slru_buffer.clone(),
                aux.meta_buffer.clone(),
                aux.gc_event_buffer.clone(),
            )
        };
        let rel_count = rel_rows.len();
        let slru_count = slru_rows.len();
        let meta_count = meta_rows.len();
        let pending_gc_count = pending_gc_rows.len();
        if rel_count == 0 && slru_count == 0 && meta_count == 0 && pending_gc_count == 0 {
            // Nothing to flush. SLRU residue (crossed in a past flush,
            // not enqueued yet — e.g. blocked by in_flight that has
            // since cleared) can still drain here. Without this pass,
            // delta_counts grows monotonically as workloads wind down —
            // the residue would only get re-examined on a flush that has
            // rows, which never comes once compute is idle.
            //
            // The snapshot may still have claimed slots an image already
            // promoted; marking it drains them.
            self.pages.mark_flushed(&rel_snapshot);
            let to_dispatch = self.take_slru_materialize_jobs(false);
            self.enqueue_slru_materialize_jobs(to_dispatch).await;
            return Ok(());
        }
        let mut claim = FlushClaim {
            writer: self,
            landed: AtomicUsize::new(0),
            settled: false,
        };

        // INSERT to YB outside any lock so concurrent puts and reads
        // aren't blocked by the round-trip. With
        // `release_backpressure_per_chunk`, each landed chunk stops counting
        // against `backpressure_bytes` now rather than at the mark.
        let release_per_chunk = self.policy.release_backpressure_per_chunk;
        let landed = |chunk: &[Arc<PageRow>]| {
            if !release_per_chunk {
                return;
            }
            let bytes: usize = chunk
                .iter()
                .map(|r| crate::unified_pages::row_size(r))
                .sum();
            claim.landed.fetch_add(bytes, Ordering::Relaxed);
            self.landed_unmarked_bytes
                .fetch_add(bytes, Ordering::Relaxed);
            self.flush_completed.notify_waiters();
        };
        let res = self
            .try_write_rows(&rel_rows, &slru_rows, &meta_rows, &pending_gc_rows, &landed)
            .await;

        if res.is_ok() {
            // GC trim candidates for this window's bases, recorded only
            // now that the rows are durable: candidate implies its base
            // landed. Rel + SLRU bases and meta puts each feed the sampler.
            self.record_gc_trim_bases(
                rel_rows.iter().map(|r| r.as_ref()),
                crate::metrics::GcEmitPath::Ingest,
            );
            self.record_gc_trim_bases(&slru_rows, crate::metrics::GcEmitPath::Ingest);
            self.record_gc_trim_bases(&meta_rows, crate::metrics::GcEmitPath::Ingest);
            // Everything at lsn ≤ ingest_frontier is now durable: rows
            // ≤ frontier were either already durable or in the
            // snapshots we just wrote (see the frontier capture above).
            // The frontier — NOT the max lsn of the flushed rows — is
            // the new candidate for `flush_horizon_lsn` and
            // `consistent_lsn`: the snapshot can contain a row at
            // lsn X while missing a concurrently-put row at lsn < X,
            // so max(snapshot) is not a durability frontier.
            // Promote rel rows in-place: flip durable on the exact
            // `Arc<PageEntry>`s we captured in the snapshot, so a slot a
            // materialiser image overwrote at the same LSN is never
            // confused with a fresh row. The FIFO drains the front
            // entries this snapshot claimed, which `flush_serialize`
            // keeps exact.
            self.pages.mark_flushed(&rel_snapshot);
            claim.settle(true);
            self.pages.evict_to_capacity();
            // Advance the materialiser's safe-to-read-from-YB watermark
            // to the ingest frontier. Until this point, YB contained a
            // partial view of this flush's chunks (buffer_unordered
            // dispatch order ≠ commit order) and the materialiser
            // would have seen holes in the chain. After mark_flushed
            // every row ≤ frontier is durable in both cache and YB —
            // rows above the frontier may still have unflushed
            // same-record siblings, so the horizon must not pass it
            // (the pre-frontier horizon was the source of the
            // transient walredo failures / unified_pages durability
            // invariant panics in #22).
            if let Some(frontier) = ingest_frontier {
                let cur = self.flush_horizon_lsn.load(Ordering::Relaxed);
                if frontier.0 > cur {
                    self.flush_horizon_lsn.store(frontier.0, Ordering::Relaxed);
                }
                // NB (#57 part 2): the rel FIFO may legitimately hold
                // an unflushed row at lsn ≤ `frontier` here, and that is
                // benign — not a data-losing over-advance. On a walreceiver
                // reconnect the stream rewinds to the durable
                // `consistent_lsn` and replays the committed gap; rows in
                // that gap that were already flushed (hence evicted/trimmed
                // from the cache) get re-`put` as FRESH unflushed slots at
                // their original low LSNs. Such a row sits below the frontier
                // in the FIFO yet is already durable in YB from the first
                // pass, so advancing past it loses nothing — the next flush
                // re-writes it (idempotent) and clears the FIFO entry. The
                // genuine catalog/heap loss under crash recovery is a
                // separate, still-open issue (a committed page write missing
                // from YB); it does not manifest as an unflushed FIFO row
                // here, so this site makes no durability assertion.
            }
            // SLRU crossings are enqueued once this flush has advanced the durable
            // horizon; relation crossings are enqueued from `put()`.
            let to_materialize = self.take_slru_materialize_jobs(true);
            // Aux buffers drain under their small mutex.
            {
                let mut aux = self.aux.lock();
                let aux_drop_bytes: usize = aux
                    .slru_buffer
                    .iter()
                    .take(slru_count)
                    .map(slru_row_size)
                    .sum::<usize>()
                    + aux
                        .meta_buffer
                        .iter()
                        .take(meta_count)
                        .map(meta_row_size)
                        .sum::<usize>();
                aux.bytes_in_aux_buffers = aux.bytes_in_aux_buffers.saturating_sub(
                    aux_drop_bytes + pending_gc_count * std::mem::size_of::<PendingGcRow>(),
                );
                aux.slru_buffer.drain(0..slru_count);
                aux.meta_buffer.drain(0..meta_count);
                aux.gc_event_buffer.drain(0..pending_gc_count);
                // NB (#57 part 2): as on the rel FIFO above, a
                // meta/SLRU row ≤ frontier remaining after the drain is the
                // benign reconnect-replay re-push of an already-durable row,
                // not a data-losing over-advance — see the rel-path comment
                // for the full rationale.
                if self.pages.unflushed_count() == 0
                    && aux.slru_buffer.is_empty()
                    && aux.meta_buffer.is_empty()
                    && aux.gc_event_buffer.is_empty()
                {
                    aux.opened_at = None;
                } else {
                    aux.opened_at = Some(Instant::now());
                }
            }
            // Wake `put()` callers blocked on backpressure before the SLRU
            // enqueue, which can wait on a full materialiser queue.
            self.flush_completed.notify_waiters();
            self.enqueue_slru_materialize_jobs(to_materialize).await;

            // Outside the writer lock: persist the dc_lsn advance to
            // `amp_storage.branch_state` if (a) it actually moved AND
            // (b) at least `policy.max_age` has elapsed since the last
            // upsert. The throttle decouples the YB UPDATE rate from
            // the page-data flush rate — a burst of size-triggered
            // flushes (every few hundred ms under load) only writes
            // the row once per `max_age` window. Safe because the
            // safekeeper's WAL truncation is bounded by the persisted
            // value; falling at most `max_age` behind means SK keeps
            // an extra `max_age` worth of WAL (5s by default — trivial).
            // Order matters: pages are already in YB by this point, so
            // a missed UPDATE just means the next flush retries; SK
            // still has the WAL because the timeline row's persisted
            // dc_lsn hasn't moved.
            if let Some(new_lsn) = ingest_frontier.map(|l| l.0) {
                use std::sync::atomic::Ordering::Relaxed;
                // Always advance `consistent_lsn_observed` — that's the
                // "everything ≤ this is durable in YB pages/slru/meta"
                // tracker. The throttle below only gates the YB UPDATE
                // on the timeline row, not the underlying durability.
                self.consistent_lsn_observed.fetch_max(new_lsn, Relaxed);
                let prev = self.consistent_lsn_watermark.load(Relaxed);
                let elapsed_ok = {
                    let guard = self.last_consistent_lsn_upsert_at.lock().unwrap();
                    match *guard {
                        None => true, // first upsert always goes through
                        Some(prev_at) => prev_at.elapsed() >= self.policy.max_age,
                    }
                };
                if new_lsn > prev && elapsed_ok {
                    let lsn = utils::lsn::Lsn(new_lsn);
                    // Best-effort pairing — see force_upsert_consistent_lsn_if_advanced.
                    let prev_record_lsn = self.take_prev_at_or_below(lsn).map(|(_, p)| p);
                    match self
                        .hot_path_page_store
                        .advance_consistent_lsn(self.branch_id, lsn, prev_record_lsn)
                        .await
                    {
                        Ok(()) => {
                            self.consistent_lsn_watermark.store(new_lsn, Relaxed);
                            *self.last_consistent_lsn_upsert_at.lock().unwrap() =
                                Some(Instant::now());
                            // Publish the just-advanced LSN as the
                            // safekeeper-truncation bound. The walreceiver
                            // loop forwards this slot to the safekeeper
                            // in PageserverFeedback.consistent_lsn,
                            // bounding `truncate_lsn` / `peer_horizon_lsn`.
                            // The UPDATE above is the durability commit
                            // point — this publish only echoes a fact
                            // that's already in YB.
                            self.publish_consistent_lsn(lsn);
                        }
                        Err(e) => {
                            // Transient YB / network error. Pages are
                            // already in YB, only the timeline row
                            // lags. Next flush retries; SK keeps a
                            // little more WAL until we catch up.
                            tracing::warn!(
                                branch_id = %self.branch_id,
                                new_lsn = %lsn,
                                "advance_consistent_lsn failed: {e:#}"
                            );
                        }
                    }
                }
            }
        }
        // On error, leave buffers untouched — next periodic flush retries.
        // With `yb_enable_upsert_mode=true` (set per-connection in
        // YsqlClient), partial-chunk-success retries don't PK-conflict.
        if res.is_err() {
            claim.settle(false);
        }
        res
    }

    async fn try_write_rows(
        &self,
        rel_rows: &[Arc<PageRow>],
        slru_rows: &[SlruPageRow],
        meta_rows: &[MetaRow],
        pending_gc_rows: &[PendingGcRow],
        rel_landed: &(dyn Fn(&[Arc<PageRow>]) + Send + Sync),
    ) -> Result<()> {
        // All flush writes go via the internal conn. Compute getPage
        // never queues behind a 12 MiB INSERT.
        if !rel_rows.is_empty() {
            self.hot_path_page_store
                .put_pages_reporting(self.branch_id, rel_rows, rel_landed)
                .await?;
        }
        #[cfg(test)]
        if self
            .fail_write_after_rel
            .swap(false, std::sync::atomic::Ordering::Relaxed)
        {
            anyhow::bail!("injected write failure after the rel rows landed");
        }
        if !slru_rows.is_empty() {
            self.hot_path_page_store
                .put_slru_pages(self.branch_id, slru_rows)
                .await?;
        }
        if !meta_rows.is_empty() {
            self.hot_path_page_store
                .put_meta(self.branch_id, meta_rows)
                .await?;
        }
        if !pending_gc_rows.is_empty() {
            self.hot_path_page_store
                .gc_enqueue_candidates(self.branch_id, pending_gc_rows)
                .await?;
        }
        Ok(())
    }

    /// `max_bytes`, or `backpressure_bytes` when that is lower, so a `put()`
    /// blocked on backpressure never waits for the age trigger.
    fn flush_size_trigger(&self) -> usize {
        match self.policy.backpressure_bytes {
            0 => self.policy.max_bytes,
            bp => self.policy.max_bytes.min(bp),
        }
    }

    pub async fn should_flush(&self) -> bool {
        let aux = self.aux.lock();
        self.pages.unflushed_bytes() + aux.bytes_in_aux_buffers >= self.flush_size_trigger()
            || aux
                .opened_at
                .is_some_and(|at| at.elapsed() >= self.policy.max_age)
    }

    pub async fn stats(&self) -> WriterStats {
        let (slru_len, meta_len, aux_bytes) = {
            let aux = self.aux.lock();
            (
                aux.slru_buffer.len(),
                aux.meta_buffer.len(),
                aux.bytes_in_aux_buffers,
            )
        };
        let bytes_in_buffer = self.pages.unflushed_bytes() + aux_bytes;
        // Side-effect: refresh the global gauges so a periodic
        // Prometheus scrape reflects the current depth. We sample
        // here rather than at every put/flush to keep the update off
        // the hot ingest path; the next scrape after a flush picks
        // up the change.
        let delta_counts_entries = self.delta_counts.len() as u64;
        let by_key_entries = self.pages.by_key_entries() as u64;
        crate::metrics::PAGEWRITER_UNFLUSHED_BYTES.set(bytes_in_buffer as u64);
        crate::metrics::PAGEWRITER_DELTA_COUNTS_ENTRIES.set(delta_counts_entries);
        crate::metrics::PAGEWRITER_DELTA_COUNTS_APPROX_BYTES
            .set(delta_counts_entries * crate::metrics::DELTA_COUNTS_BYTES_PER_ENTRY);
        crate::metrics::UNIFIED_PAGES_BY_KEY_ENTRIES.set(by_key_entries);
        WriterStats {
            rows_written: self.rows_written.load(Ordering::Relaxed),
            bytes_in_buffer,
            buffered_rows: self.pages.unflushed_count(),
            buffered_slru_rows: slru_len,
            buffered_meta_rows: meta_len,
            meta_cache_hits: self.meta_cache_hits.load(Ordering::Relaxed),
        }
    }

    /// Number of entries currently in the meta LRU cache. Returns `None`
    /// if the cache is disabled (`meta_cache_entries = 0`).
    pub fn meta_cache_len(&self) -> Option<usize> {
        self.meta_cache.as_ref().map(|c| c.lock().len())
    }

    /// Total `put()` calls that had to wait on backpressure since
    /// startup. Telemetry only.
    pub fn backpressure_wait_count(&self) -> u64 {
        self.backpressure_waits
            .load(std::sync::atomic::Ordering::Relaxed)
    }

    /// Record that the walreceiver has applied a record ending at
    /// `last_record_lsn`, whose predecessor record ended at
    /// `prev_record_lsn`. The flush path looks this up when it
    /// persists `consistent_lsn` so the matching
    /// `prev_record_lsn` column gets a non-NULL value on restart.
    ///
    /// Called from `Timeline::finish_write` after each WAL record
    /// is applied.
    pub fn note_record_lsn(&self, last_record_lsn: Lsn, prev_record_lsn: Lsn) {
        if let Ok(mut m) = self.record_lsn_history.lock() {
            m.insert(last_record_lsn, prev_record_lsn);
            // Cap the map size at a generous bound so a stuck flush
            // doesn't grow it unboundedly. We prune from below in
            // `take_prev_at_or_below`; this is just defense in depth.
            const HARD_CAP: usize = 16_384;
            if m.len() > HARD_CAP {
                let prune_to = m.len() - HARD_CAP / 2;
                let cutoff = *m.keys().nth(prune_to).expect("len > HARD_CAP/2");
                m.retain(|&k, _| k >= cutoff);
            }
        }
    }

    /// Pull the `prev_record_lsn` paired with the highest noted
    /// `last_record_lsn ≤ flushed_lsn`, and remove that entry plus
    /// all older ones from the history (those records are durable
    /// now). Returns the matched `(last, prev)` pair, or `None` if
    /// the buffer holds no notes at or below `flushed_lsn` —
    /// typically because pages landed before their record's
    /// `finish_write` ran.
    fn take_prev_at_or_below(&self, flushed_lsn: Lsn) -> Option<(Lsn, Lsn)> {
        let mut m = self.record_lsn_history.lock().ok()?;
        let (&last, &prev) = m.range(..=flushed_lsn).next_back()?;
        m.retain(|&k, _| k > last);
        Some((last, prev))
    }

    /// Range scan over meta keys. Returns the latest version per key.
    pub async fn scan_meta_range(
        &self,
        range: std::ops::Range<Key>,
        target_lsn: Lsn,
    ) -> Result<std::collections::HashMap<bytes::Bytes, MetaRow>> {
        // Single-key fast path: check the in-process LRU cache before
        // issuing a YB round-trip. A hit means this timeline has written
        // this key; the cached row is always the most recently written
        // version (write-through invariant from `put()`). It's valid as
        // the answer whenever cached.lsn ≤ target_lsn — any older row
        // in YB would be shadowed by this one.
        if range.end == range.start.next() {
            if let Some(cache) = &self.meta_cache {
                let mut key_bytes = vec![0u8; KEY_SIZE];
                range.start.write_to_byte_slice(&mut key_bytes);
                let key_bytes = Bytes::from(key_bytes);
                if let Some(row) = cache.lock().get(&key_bytes).cloned() {
                    if row.lsn <= target_lsn {
                        self.meta_cache_hits.fetch_add(1, Ordering::Relaxed);
                        let mut out = HashMap::with_capacity(1);
                        out.insert(row.key.clone(), row);
                        return Ok(out);
                    }
                }
            }
        }

        // Sample the in-RAM aux rows BEFORE the YB round-trip, not
        // after. `flush()` writes a meta row to YB (`try_write_rows`)
        // and only THEN drains it from `aux.meta_buffer` — so a row is
        // never absent from both sources at one instant. But readers
        // must sample them in that same order: aux first, then YB.
        // Sampling YB first (aux second) opens a window where, between
        // the two reads, a concurrent flush commits the row to YB and
        // drains it from aux — so the stale YB read misses it and the
        // post-drain aux read misses it too. The row falls through the
        // crack and `get_rel_size` lags a freshly-ingested extend: the
        // beyond-EOF check then serves a zeroed page for a block that
        // exists (#141). Reading aux first closes the window — if a row
        // has left aux it is already durable in YB (the drain strictly
        // follows the commit), and if it is still in aux we capture it
        // directly. This matches `read_slru_chain` below, which already
        // reads aux before PageStore. The merge keeps the highest LSN
        // per key, so YB still wins where it holds a newer version.
        //
        // Tie-break: on EQUAL LSN, durable YB wins (aux wins only when
        // STRICTLY newer). On a walreceiver reconnect the WAL stream
        // rewinds and re-pushes already-durable rows back into aux at
        // their original LSNs (see `flush()`'s #57-part-2 note). Such a
        // re-pushed meta row can carry a stale payload at the SAME LSN
        // as the durable row — letting it win would regress a rel-size
        // back to a smaller value mid-recovery and serve a zero page
        // for a block that exists (observed as torn_wal_write mode-A
        // data loss). Preferring the durable row on ties avoids that
        // while still closing the #141 window, where the missing row is
        // STRICTLY newer in aux than the stale YB row.
        let mut out: HashMap<Bytes, MetaRow> = {
            let aux = self.aux.lock();
            let mut acc: HashMap<Bytes, MetaRow> = HashMap::new();
            for row in aux.meta_buffer.iter() {
                if row.key.len() != KEY_SIZE || row.lsn > target_lsn {
                    continue;
                }
                let k = Key::from_slice(&row.key);
                if k < range.start || k >= range.end {
                    continue;
                }
                match acc.get(&row.key) {
                    Some(existing) if existing.lsn >= row.lsn => continue,
                    _ => {
                        acc.insert(row.key.clone(), row.clone());
                    }
                }
            }
            acc
        };
        let yb = self
            .compute_page_store
            .scan_meta_range_lineage(&self.lineage, range.clone(), target_lsn)
            .await?;
        for (k, v) in yb {
            match out.get(&k) {
                // aux wins only when STRICTLY newer; YB (durable) wins ties.
                Some(existing) if existing.lsn > v.lsn => continue,
                _ => {
                    out.insert(k, v);
                }
            }
        }
        Ok(out)
    }

    /// The SLRU buffer's rows for `blocks` of one `(kind, segno)` at or
    /// below `target_lsn`: what a read must blend with the store, since the
    /// buffer holds rows not yet flushed.
    fn buffered_slru_rows(
        &self,
        kind: SlruKind,
        segno: u32,
        blocks: std::ops::Range<u32>,
        target_lsn: Lsn,
    ) -> Vec<SlruPageRow> {
        let aux = self.aux.lock();
        aux.slru_buffer
            .iter()
            .filter(|r| {
                r.kind == kind
                    && r.segno == segno
                    && blocks.contains(&r.blknum)
                    && r.lsn <= target_lsn
            })
            .cloned()
            .collect()
    }

    /// Per-block SLRU chain read, blending the in-RAM SLRU buffer with
    /// PageStore. Used by the timeline's vectored read for SLRU keys.
    pub async fn read_slru_chain(
        &self,
        kind: SlruKind,
        segno: u32,
        blknum: u32,
        target_lsn: Lsn,
    ) -> Result<Vec<SlruPageRow>> {
        let mut chain = self.buffered_slru_rows(kind, segno, blknum..blknum + 1, target_lsn);
        let persisted = self
            .compute_page_store
            .read_slru_chain_lineage(&self.lineage, kind, segno, blknum, target_lsn)
            .await?;
        chain.extend(persisted);
        crate::page_store::merge_slru_chain(&mut chain, crate::page_store::SlruScan::ToAnchor);
        Ok(chain)
    }

    /// Range scan over an SLRU segment, blending buffered + persisted.
    /// `blknum_range` lives within one `(kind, segno)`.
    pub async fn scan_slru_range(
        &self,
        kind: SlruKind,
        segno: u32,
        blknum_range: std::ops::Range<u32>,
        target_lsn: Lsn,
        mode: crate::page_store::SlruScan,
    ) -> Result<std::collections::HashMap<u32, Vec<SlruPageRow>>> {
        // Buffer before YB, as in `read_slru_chain`.
        let buffered = self.buffered_slru_rows(kind, segno, blknum_range.clone(), target_lsn);
        #[cfg(test)]
        self.pause_between_cache_and_yb().await;
        let mut out = self
            .compute_page_store
            .scan_slru_range_lineage(&self.lineage, kind, segno, blknum_range, target_lsn, mode)
            .await?;
        for row in buffered {
            out.entry(row.blknum).or_default().push(row);
        }
        for chain in out.values_mut() {
            crate::page_store::merge_slru_chain(chain, mode);
        }
        Ok(out)
    }
}

/// Handle for a periodic-flush background task. Cancel the token to
/// stop; the task does one final `flush()` before returning.
pub struct FlushTask {
    pub handle: tokio::task::JoinHandle<()>,
    pub cancel: CancellationToken,
}

impl FlushTask {
    pub async fn shutdown(self) {
        self.cancel.cancel();
        let _ = self.handle.await;
    }
}

impl PageWriter {
    pub fn spawn_flusher(self: &Arc<Self>, tick: Duration) -> FlushTask {
        let cancel = CancellationToken::new();
        let cancel_for_task = cancel.clone();
        let weak = Arc::downgrade(self);
        // Cloned out so the task can wait on a wakeup without upgrading
        // `weak` (which would pin the writer across the whole tick and
        // defeat drop-based shutdown).
        let wakeup = Arc::clone(&self.flusher_wakeup);
        let handle = tokio::spawn(async move {
            // An interval, not a per-iteration sleep, so passes keep a steady
            // cadence however often wakeups come.
            let mut ticker = tokio::time::interval(tick.max(Duration::from_millis(1)));
            ticker.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Delay);
            loop {
                tokio::select! {
                    _ = cancel_for_task.cancelled() => break,
                    _ = ticker.tick() => {}
                    _ = wakeup.notified() => {}
                }
                let Some(w) = weak.upgrade() else { break };
                // Every pass, tick or wakeup: a wakeup only makes the pass
                // come sooner. `stats()` refreshes the unflushed-bytes gauge
                // so a /metrics scrape sees the buffer depth even when there
                // is nothing to flush.
                let _ = w.stats().await;
                w.retry_enqueue_rel_materialize_jobs();
                let flush_due = w.slru_materialize_jobs_pending_flush() || w.should_flush().await;
                // Lets an integration test keep folded images above unflushed deltas.
                #[cfg(feature = "testing")]
                let flush_due =
                    flush_due && fail::eval("ps::page-writer::hold-flusher", |_| ()).is_none();
                if flush_due && let Err(e) = w.flush().await {
                    crate::metrics::CORRECTNESS_ERRORS
                        .with_label_values(&["pagewriter_flush"])
                        .inc();
                    tracing::error!(
                        "PAGEWRITER PERIODIC FLUSH FAILED — rows may not have \
                         reached YB; walreceiver will retry on resume but \
                         consistent_lsn may stall: {e:?}"
                    );
                }
            }
            if let Some(w) = weak.upgrade() {
                if let Err(e) = w.flush().await {
                    crate::metrics::CORRECTNESS_ERRORS
                        .with_label_values(&["pagewriter_flush"])
                        .inc();
                    tracing::error!(
                        "PAGEWRITER FINAL FLUSH FAILED — last unflushed window \
                         lost: {e:?}"
                    );
                }
                // Force-persist any post-throttle dc_lsn the periodic
                // flushes have been deferring. Without this, a clean
                // shutdown could leave the persisted dc_lsn up to
                // `policy.max_age` behind the actual durable LSN —
                // safe for crash recovery (walreceiver re-applies the
                // gap, idempotent INSERTs) but wasteful on restart
                // and noisy in logs.
                if let Err(e) = w.force_upsert_consistent_lsn_if_advanced().await {
                    crate::metrics::CORRECTNESS_ERRORS
                        .with_label_values(&["pagewriter_flush"])
                        .inc();
                    tracing::error!(
                        "PAGEWRITER FINAL DC_LSN UPSERT FAILED — persisted \
                         consistent_lsn is stale on disk; next attach \
                         will redo extra WAL: {e:?}"
                    );
                }
            }
        });
        FlushTask { handle, cancel }
    }
}

/// Convert the upstream-shaped `SerializedValueBatch` (one batch per
/// digested WAL record) into the `(Key, LSN, Value)` triples
/// [`PageWriter::put`] consumes. Skips `ValueMeta::Observed` entries.
/// Whether a SLRU value resets the delta chain (image / will-init anchor).
fn slru_resets_chain(value: &Value) -> bool {
    match value {
        Value::Image(_) => true,
        Value::WalRecord(rec) => rec.will_init(),
    }
}

/// (rel rows, slru rows, meta rows, slru materialiser tuples).
type BuiltRows = (
    Vec<PageRow>,
    Vec<SlruPageRow>,
    Vec<MetaRow>,
    Vec<(Key, Lsn, bool)>,
);

/// Deserialize values and build page rows for the given `metas` (a subset
/// of `batch.metadata`). Pure CPU and self-contained, so it is safe to run
/// on a blocking worker over an `Arc<SerializedValueBatch>`. The `slru_mat`
/// tuples carry the `(key, lsn, resets_chain)` the materialiser needs,
/// since the built `SlruPageRow`s drop the key.
fn build_rows_from_metas<'a>(
    batch: &'a SerializedValueBatch,
    metas: impl Iterator<Item = &'a ValueMeta>,
) -> Result<BuiltRows> {
    let mut rel_rows = Vec::new();
    let mut slru_rows = Vec::new();
    let mut meta_rows = Vec::new();
    let mut slru_mat = Vec::new();
    for meta in metas {
        let ValueMeta::Serialized(ser) = meta else {
            continue;
        };
        let start = ser.batch_offset as usize;
        let end = start
            .checked_add(ser.len)
            .ok_or_else(|| anyhow::anyhow!("ValueMeta offset overflow"))?;
        if end > batch.raw.len() {
            anyhow::bail!(
                "ValueMeta range {start}..{end} exceeds raw buffer length {}",
                batch.raw.len()
            );
        }
        let value = Value::des(&batch.raw[start..end])?;
        let key = Key::from_compact(ser.key);
        let lsn = ser.lsn;
        if key.is_rel_block_key() {
            rel_rows.push(build_page_row(key, lsn, &value)?);
        } else if key.is_slru_block_key() {
            slru_rows.push(build_slru_page_row(key, lsn, &value)?);
            slru_mat.push((key, lsn, slru_resets_chain(&value)));
        } else {
            meta_rows.push(build_meta_row(key, lsn, &value)?);
        }
    }
    Ok((rel_rows, slru_rows, meta_rows, slru_mat))
}

pub fn extract_triples(batch: &SerializedValueBatch) -> Result<Vec<(Key, Lsn, Value)>> {
    let mut out = Vec::with_capacity(batch.len);
    for meta in &batch.metadata {
        let ValueMeta::Serialized(ser) = meta else {
            continue;
        };
        let start = ser.batch_offset as usize;
        let end = start
            .checked_add(ser.len)
            .ok_or_else(|| anyhow::anyhow!("ValueMeta offset overflow"))?;
        if end > batch.raw.len() {
            anyhow::bail!(
                "ValueMeta range {start}..{end} exceeds raw buffer length {}",
                batch.raw.len()
            );
        }
        let value = Value::des(&batch.raw[start..end])?;
        let key = Key::from_compact(ser.key);
        out.push((key, ser.lsn, value));
    }
    Ok(out)
}

fn key_to_bytes(key: Key) -> Bytes {
    let mut buf = vec![0u8; KEY_SIZE];
    key.write_to_byte_slice(&mut buf);
    Bytes::from(buf)
}

/// Bump `correctness_errors{metric_label}` and build the anchorless-chain
/// error. Shared by `read_chain_for_materialize` and `read_slru_chain_durable` so the
/// two refusals can't drift; each call site documents why it refuses.
/// `subject` carries the caller's key-identifying fields.
// Every argument is a distinct diagnostic field.
#[allow(clippy::too_many_arguments)]
fn unanchored_chain_error(
    metric_label: &str,
    op: &str,
    target_lsn: Lsn,
    chain_len: usize,
    lo: Option<Lsn>,
    hi: Option<Lsn>,
    subject: std::fmt::Arguments<'_>,
) -> anyhow::Error {
    crate::metrics::CORRECTNESS_ERRORS
        .with_label_values(&[metric_label])
        .inc();
    anyhow::anyhow!(
        "{op}: no will_init anchor in chain ≤ target_lsn={target_lsn} \
         ({subject}, chain_len={chain_len}, lsn lo={lo:?} hi={hi:?}) — the page's \
         init record is missing; refusing to materialise a base-less page"
    )
}

/// Write-side round-trip check on every freshly-serialised
/// `NeonWalRecord`. Off by default; flip the env var
/// `YB_PS_VERIFY_WAL_ROUNDTRIP=1` to enable.
///
/// When on, the moment after `rec.ser()` produces bytes we call
/// `NeonWalRecord::des(&bytes)` and compare back to the original. If
/// they don't match, the serializer is at fault — corruption happens
/// before the bytes ever leave the process. If this check stays clean
/// while reads in YB still see `deserialize error`, the corruption is
/// downstream (network, YB BYTEA storage, or read-path decode).
///
/// Kept behind an env flag so the hot-path cost (a second bincode
/// pass + an Eq compare per WAL record) is only paid during
/// investigations.
fn verify_walrecord_roundtrip_enabled() -> bool {
    static ON: std::sync::OnceLock<bool> = std::sync::OnceLock::new();
    *ON.get_or_init(|| {
        std::env::var("YB_PS_VERIFY_WAL_ROUNDTRIP")
            .map(|v| matches!(v.as_str(), "1" | "true" | "yes"))
            .unwrap_or(false)
    })
}

/// Translate a walingest `Value` into a `PageRow`. Images become
/// `is_image=TRUE, will_init=TRUE`. WAL records become `is_image=FALSE`
/// and carry the record's own `will_init` flag — that's the only signal
/// the read side has to identify a chain anchor when no image exists.
fn build_page_row(key: Key, lsn: Lsn, value: &Value) -> Result<PageRow> {
    let (is_image, will_init, payload) = match value {
        Value::Image(b) => (true, true, b.clone()),
        Value::WalRecord(rec) => {
            let will_init = rec.will_init();
            let bytes = Bytes::from(rec.ser()?);
            if verify_walrecord_roundtrip_enabled() {
                match NeonWalRecord::des(&bytes) {
                    Ok(decoded) if &decoded == rec => {}
                    Ok(decoded) => {
                        // Our own serializer is wrong — writing this
                        // byte sequence to YB persists input that the
                        // read path will mis-decode. Abort before the
                        // bytes leave the process.
                        crate::metrics::CORRECTNESS_ERRORS
                            .with_label_values(&["write_path_roundtrip"])
                            .inc();
                        utils::abort::fatal_abort(format_args!(
                            "WRITE-PATH ROUNDTRIP MISMATCH key={} lsn={} \
                             payload_len={}: orig={:?} decoded={:?}",
                            key,
                            lsn,
                            bytes.len(),
                            rec,
                            decoded,
                        ));
                    }
                    Err(e) => {
                        // A roundtrip failure on bytes we just produced
                        // means the serializer can write values it
                        // cannot read. Abort.
                        crate::metrics::CORRECTNESS_ERRORS
                            .with_label_values(&["write_path_roundtrip"])
                            .inc();
                        utils::abort::fatal_abort(format_args!(
                            "WRITE-PATH ROUNDTRIP DECODE FAILED key={} lsn={} \
                             payload_len={}: {:#}",
                            key,
                            lsn,
                            bytes.len(),
                            e,
                        ));
                    }
                }
            }
            (false, will_init, bytes)
        }
    };
    Ok(PageRow {
        key,
        lsn,
        is_image,
        will_init,
        payload,
    })
}

/// SLRU equivalent of `build_page_row`. `key` must be an SLRU block key
/// (`Key::is_slru_block_key`); the caller is expected to have already
/// filtered.
fn build_slru_page_row(key: Key, lsn: Lsn, value: &Value) -> Result<SlruPageRow> {
    let (kind, segno, blknum) = key.to_slru_block()?;
    let (is_image, will_init, payload) = match value {
        Value::Image(b) => (true, true, b.clone()),
        Value::WalRecord(rec) => {
            let will_init = rec.will_init();
            let bytes = Bytes::from(rec.ser()?);
            (false, will_init, bytes)
        }
    };
    Ok(SlruPageRow {
        kind,
        segno,
        blknum,
        lsn,
        is_image,
        will_init,
        payload,
    })
}

fn slru_row_size(r: &SlruPageRow) -> usize {
    r.payload.len() + 32
}

/// Convert a meta-key Value into a MetaRow. Meta keys are
/// full-overwrite, so we only accept `Value::Image`; a WalRecord on a
/// non-rel-non-slru key would mean walredo is expected for a meta key,
/// which the row-store design doesn't support.
fn build_meta_row(key: Key, lsn: Lsn, value: &Value) -> Result<MetaRow> {
    let payload = match value {
        Value::Image(b) => b.clone(),
        Value::WalRecord(rec) => {
            // Some walingest paths still emit Value::WalRecord for
            // small meta deltas (e.g. relation-size sentinel updates
            // that carry the new size in the record). Serialize and
            // store as-is — meta reads return the row that's there;
            // higher layers are responsible for interpreting the
            // payload. The expectation is that these arrive with
            // will_init=true (the record fully replaces the key's
            // value at this LSN) — log if it isn't.
            if !rec.will_init() {
                tracing::warn!(
                    "PageWriter: meta key {key} lsn={lsn} received a non-init WalRecord; \
                     stored as opaque payload"
                );
            }
            Bytes::from(rec.ser()?)
        }
    };
    Ok(MetaRow {
        key: key_to_bytes(key),
        lsn,
        payload,
    })
}

fn meta_row_size(r: &MetaRow) -> usize {
    r.payload.len() + r.key.len() + 16
}

/// Pure decision for read-side materialise dispatch. Returns `true` if
/// a `MaterializeJob` was successfully enqueued (and the caller should
/// bump the dispatch counter).
///
/// Encapsulated as a free function so it can be unit-tested with a
/// synthetic channel + HashSet, no PageWriter / YB setup needed.
///
/// Invariants:
///   - `count_limit == 0` ⇒ no dispatch (materialiser disabled).
///   - `chain_len <= count_limit` ⇒ no dispatch (short enough; write
///     trigger will handle it).
///   - Key already in `in_flight` ⇒ no dispatch (a materialise job for
///     this key is already enqueued or being processed).
///   - `try_send` failure (queue full or receiver dropped) ⇒ the
///     `in_flight` reservation is rolled back so a later trigger can
///     retry — otherwise the key would be permanently blocked from
///     re-dispatch.
fn try_dispatch_read_side(
    key: Key,
    target_lsn: Lsn,
    chain_len: usize,
    count_limit: u32,
    in_flight: &std::sync::Mutex<std::collections::HashSet<Key>>,
    tx: &tokio::sync::mpsc::Sender<MaterializeJob>,
) -> bool {
    if count_limit == 0 || chain_len <= count_limit as usize {
        return false;
    }
    let inserted = in_flight.lock().unwrap().insert(key);
    if !inserted {
        return false;
    }
    if tx.try_send(MaterializeJob { key, target_lsn }).is_err() {
        in_flight.lock().unwrap().remove(&key);
        return false;
    }
    true
}

#[cfg(test)]
mod tests {
    use super::*;

    use bytes::Bytes;
    use remote_storage::YbConnConfig;
    use utils::id::BranchId;
    use wal_decoder::models::record::NeonWalRecord;

    use crate::page_store::{PageStore, PageStoreConfig};

    impl PageWriter {
        /// Test-only deterministic flush of the trim-candidate emitter.
        async fn flush_gc_trim_candidates(&self) {
            if let Some(emitter) = &self.gc_trim_candidate_emitter {
                emitter.flush().await;
            }
        }
    }

    fn test_yb_config() -> YbConnConfig {
        let host = std::env::var("YB_TEST_HOST").unwrap_or_else(|_| "127.0.0.1".to_string());
        let port = std::env::var("YB_TEST_PORT").unwrap_or_else(|_| "5433".to_string());
        YbConnConfig {
            host_ports: format!("{host}:{port}").parse().unwrap(),
            user: std::env::var("YB_TEST_USER").unwrap_or_else(|_| "yugabyte".to_string()),
            password_env: None,
            dbname: std::env::var("YB_TEST_DB").unwrap_or_else(|_| "yb_storage".to_string()),
            sslmode: std::env::var("YB_TEST_SSLMODE").ok(),
            sslrootcert: std::env::var("YB_TEST_SSLROOTCERT").ok(),
            sslcert: std::env::var("YB_TEST_SSLCERT").ok(),
            sslkey: std::env::var("YB_TEST_SSLKEY").ok(),
            disable_transactional_writes: false,
            tserver_rpc_addrs: Some(
                std::env::var("YB_TEST_TSERVER_RPC")
                    .unwrap_or_else(|_| format!("{host}:9100"))
                    .parse()
                    .unwrap(),
            ),
            tsclient_read_sessions: None,
            tsclient_write_sessions: None,
            tsclient_sessions_per_conn: None,
            connect_timeout_ms: ysql_client::DEFAULT_CONNECT_TIMEOUT_MS,
            statement_timeout_ms: ysql_client::DEFAULT_STATEMENT_TIMEOUT_MS,
            tcp_user_timeout_ms: 0,
            connection_lifetime_ms: ysql_client::DEFAULT_CONNECTION_LIFETIME_MS,
        }
    }

    /// Shared-schema test scaffolding — see the matching comment in
    /// `page_store.rs::tests::SHARED_STORE`. All tests in this binary
    /// share one schema, isolated by `rand_tenant()`/`rand_timeline()`.
    /// Per-test schema DDL was costing 5–10 s of YB tablet
    /// coordination apiece; sharing amortizes that to once per binary.
    static SHARED_STORE: tokio::sync::OnceCell<Arc<PageStore>> = tokio::sync::OnceCell::const_new();
    const PROD_SCHEMA: &str = "amp_storage";

    async fn make_store(_suffix: &str) -> (Arc<PageStore>, String) {
        let store = SHARED_STORE
            .get_or_init(|| async {
                let cfg = PageStoreConfig {
                    schema: PROD_SCHEMA.to_string(),
                    ..PageStoreConfig::new(test_yb_config())
                };
                let store = PageStore::connect(cfg, "pw_test", true)
                    .await
                    .expect("connect to local YB; run `yb_amp_local init && start` first to migrate the schema");
                Arc::new(store)
            })
            .await
            .clone();
        (store, PROD_SCHEMA.to_string())
    }

    async fn drop_schema(_store: &PageStore, _schema: &str) {
        // No-op: shared production schema, owned by the diesel
        // migration. Per-test isolation comes from random
        // tenant/timeline IDs.
    }

    fn k(n: u32) -> Key {
        Key {
            field1: 0,
            field2: 1663,
            field3: 5,
            field4: 16384,
            field5: 0,
            field6: n,
        }
    }

    fn image() -> Value {
        Value::Image(Bytes::from_static(b"natural-image-bytes"))
    }

    fn delta() -> Value {
        Value::WalRecord(NeonWalRecord::ClearVisibilityMapFlags {
            new_heap_blkno: Some(1),
            old_heap_blkno: None,
            flags: 0x01,
        })
    }

    fn writer(store: Arc<PageStore>, policy: FlushPolicy) -> (PageWriter, BranchId) {
        let branch = BranchId::generate();
        (
            // Tests don't exercise the connection split — pass the
            // same PageStore arc as both compute + internal.
            PageWriter::new(branch, store.clone(), store, policy, 64 * 1024 * 1024, 0),
            branch,
        )
    }

    /// A `PageWriter` with the meta LRU cache enabled and capacity
    /// `entries`. Pass `entries = 1` to force eviction on the second key.
    fn writer_with_cache(
        store: Arc<PageStore>,
        policy: FlushPolicy,
        entries: usize,
    ) -> (PageWriter, BranchId) {
        let t = BranchId::generate();
        (
            PageWriter::new(t, store.clone(), store, policy, 64 * 1024 * 1024, entries),
            t,
        )
    }

    /// A metadata-range key (field1 = 0x60) with a unique discriminator.
    fn mk(n: u32) -> Key {
        Key {
            field1: 0x60,
            field2: 0,
            field3: 0,
            field4: 0,
            field5: 0,
            field6: n,
        }
    }

    /// What the `writer_with_materializer*` fixtures hand back: the writer,
    /// its tenant/timeline ids, the job receiver, and the shared `in_flight`
    /// set.
    type MaterializerFixture = (
        PageWriter,
        BranchId,
        tokio::sync::mpsc::Receiver<MaterializeJob>,
        Arc<std::sync::Mutex<std::collections::HashSet<Key>>>,
    );

    /// `writer` plus an installed materializer handle. Returns the
    /// receiver so tests can pull queued jobs AND the `in_flight`
    /// Arc so tests can simulate the materialiser's per-batch
    /// cleanup (production: `ImageMaterializer::materialize_batch`
    /// removes each batched key from `in_flight` after the INSERT;
    /// tests do the equivalent inline after pulling a job).
    fn writer_with_materializer(
        store: Arc<PageStore>,
        policy: FlushPolicy,
        limit: u32,
    ) -> MaterializerFixture {
        // Default fixture: count-only trigger (payload-bytes disabled)
        // so existing tests stay deterministic on count-based thresholds.
        writer_with_materializer_thresholds(store, policy, limit, 0, 0)
    }

    /// Like `writer_with_materializer` but lets tests configure both
    /// `count_limit` and `payload_bytes_limit` independently. Use this
    /// when exercising the payload-bytes trigger branch or the
    /// either-fires-first ("hybrid") semantics from Phase 4i (commit
    /// `895b7eca5`).
    fn writer_with_materializer_thresholds(
        store: Arc<PageStore>,
        policy: FlushPolicy,
        count_limit: u32,
        payload_bytes_limit: u32,
        slru_count_limit: u32,
    ) -> MaterializerFixture {
        let t = BranchId::generate();
        let mut w = PageWriter::new(t, store.clone(), store, policy, 64 * 1024 * 1024, 0);
        let (tx, rx) = tokio::sync::mpsc::channel(64);
        let in_flight = Arc::new(std::sync::Mutex::new(std::collections::HashSet::new()));
        // Tests exercise steady-state dispatch: open the attach
        // quarantine (`true`) so folds are allowed.
        let (_caught_up_tx, caught_up_rx) = tokio::sync::watch::channel(true);
        w.set_materializer(MaterializerHandle {
            replay_caught_up: caught_up_rx,
            count_limit,
            slru_count_limit,
            payload_bytes_limit,
            tx,
            in_flight: in_flight.clone(),
        });
        (w, t, rx, in_flight)
    }

    /// A non-`will_init` delta (the normal "modify existing page"
    /// shape). `ClearVisibilityMapFlags` is convenient because it has
    /// `will_init() == false` regardless of fields.
    fn delta_non_init() -> Value {
        Value::WalRecord(NeonWalRecord::ClearVisibilityMapFlags {
            new_heap_blkno: Some(1),
            old_heap_blkno: None,
            flags: 0x01,
        })
    }

    /// A `will_init=true` delta.
    fn delta_will_init() -> Value {
        Value::WalRecord(NeonWalRecord::Postgres {
            will_init: true,
            rec: Bytes::from_static(b"init-rec"),
        })
    }

    #[test]
    fn extract_triples_round_trips_through_serialized_batch() {
        let values_in: Vec<(Key, Lsn, Value)> = vec![
            (k(1), Lsn(100), image()),
            (
                k(2),
                Lsn(200),
                Value::WalRecord(NeonWalRecord::Postgres {
                    will_init: true,
                    rec: Bytes::from_static(b"abc"),
                }),
            ),
            (k(1), Lsn(300), delta()),
        ];
        let triples_in: Vec<(pageserver_api::key::CompactKey, Lsn, usize, Value)> = values_in
            .iter()
            .map(|(k, l, v)| {
                (
                    k.to_compact(),
                    *l,
                    v.serialized_size().expect("serialized_size") as usize,
                    v.clone(),
                )
            })
            .collect();
        let batch = SerializedValueBatch::from_values(triples_in);
        let extracted = extract_triples(&batch).expect("extract_triples");
        assert_eq!(extracted.len(), values_in.len());
        for ((k_in, lsn_in, val_in), (k_out, lsn_out, val_out)) in
            values_in.iter().zip(extracted.iter())
        {
            assert_eq!(k_out, k_in);
            assert_eq!(lsn_out, lsn_in);
            assert_eq!(val_out, val_in);
        }
    }

    #[tokio::test]
    async fn empty_flush_is_noop() {
        let (store, schema) = make_store("empty_flush").await;
        let (w, _t) = writer(store.clone(), FlushPolicy::default());
        w.flush().await.unwrap();
        let stats = w.stats().await;
        assert_eq!(stats.buffered_rows, 0);
        drop_schema(&store, &schema).await;
    }

    #[tokio::test]
    async fn image_passes_through_untouched() {
        let (store, schema) = make_store("image_pt").await;
        let (w, t) = writer(store.clone(), FlushPolicy::default());
        w.put(&[(k(1), Lsn(10), image())]).await.unwrap();
        let stats = w.stats().await;
        assert_eq!(stats.rows_written, 1);
        assert_eq!(stats.buffered_rows, 1);
        w.flush().await.unwrap();
        let chain = store
            .read_chain(t, &key_to_bytes(k(1)), Lsn(100))
            .await
            .unwrap();
        assert_eq!(chain.len(), 1);
        assert!(chain[0].is_image);
        assert!(chain[0].will_init);
        assert_eq!(chain[0].payload.as_ref(), b"natural-image-bytes");
        drop_schema(&store, &schema).await;
    }

    #[tokio::test]
    async fn delta_passes_through_untouched() {
        // No write-time materialisation: a plain delta arriving first
        // is stored as-is; reads find it via the slow path.
        let (store, schema) = make_store("delta_pt").await;
        let (w, t) = writer(store.clone(), FlushPolicy::default());
        let init = Value::WalRecord(NeonWalRecord::Postgres {
            will_init: true,
            rec: Bytes::from_static(b"init"),
        });
        w.put(&[
            (k(1), Lsn(10), init),
            (k(1), Lsn(20), delta()),
            (k(1), Lsn(30), delta()),
        ])
        .await
        .unwrap();
        let stats = w.stats().await;
        assert_eq!(stats.rows_written, 3);
        w.flush().await.unwrap();
        let chain = store
            .read_chain(t, &key_to_bytes(k(1)), Lsn(100))
            .await
            .unwrap();
        assert_eq!(chain.len(), 3);
        // LSN-descending; anchor at the bottom.
        assert_eq!(chain[0].lsn, Lsn(30));
        assert_eq!(chain[2].lsn, Lsn(10));
        assert!(chain[2].will_init);
        assert!(!chain[2].is_image);
        drop_schema(&store, &schema).await;
    }

    /// #57 part 2: deterministic repro of the materialiser's
    /// anchorless-fold hazard. A key whose `will_init` anchor is absent
    /// from YB (its init record was lost upstream) leaves only non-init
    /// deltas. `read_chain_for_materialize` must NOT hand that chain to the
    /// materialiser: `parse_chain` would set `base_img=None` and walredo
    /// would fold the deltas onto a ZERO page, then `install + trim`
    /// would make that wrong/zero page durable — the "unexpected zero
    /// page" / heap-hole / orphan-catalog corruption. The fix errors;
    /// `redo_one` logs it and skips the page (leaving the deltas in
    /// place) rather than materialising a zero-based image.
    #[tokio::test]
    async fn read_chain_for_materialize_refuses_anchorless_chain() {
        let (store, schema) = make_store("anchorless").await;
        let (w, t) = writer(store.clone(), FlushPolicy::default());
        // Deltas only, no will_init anchor anywhere ≤ target.
        w.put(&[(k(1), Lsn(20), delta()), (k(1), Lsn(30), delta())])
            .await
            .unwrap();
        w.flush().await.unwrap();

        // Sanity: the raw store chain is genuinely anchorless (no
        // will_init), which is the precondition the fix must catch.
        let raw = store
            .read_chain(t, &key_to_bytes(k(1)), Lsn(100))
            .await
            .unwrap();
        assert!(!raw.is_empty(), "deltas should be durable");
        assert!(
            !raw.iter().any(|r| r.will_init),
            "precondition: chain has no will_init anchor"
        );

        // The materialiser-facing read must refuse the anchorless chain.
        let res = w.read_chain_for_materialize(k(1), Lsn(100)).await;
        assert!(
            res.is_err(),
            "read_chain_for_materialize must error on an anchorless chain (would zero-base \
             the page); got Ok with {} rows",
            res.map(|c| c.len()).unwrap_or(0),
        );
        drop_schema(&store, &schema).await;
    }

    fn clog_row(lsn: u64, is_image: bool, will_init: bool, payload: &[u8]) -> SlruPageRow {
        SlruPageRow {
            kind: SlruKind::Clog,
            segno: 0,
            blknum: 7,
            lsn: Lsn(lsn),
            is_image,
            will_init,
            payload: Bytes::copy_from_slice(payload),
        }
    }

    /// #420: the materialiser's SLRU read must resolve a base that lives on
    /// the ANCESTOR. A branch only ever stamps `SETBIT` deltas onto the clog
    /// page that was active at the fork — that page's `will_init` ZEROPAGE
    /// anchor predates the branch and sits under the ancestor's `branch_id`.
    /// The pre-fix code read branch-locally, got an anchorless chain, and
    /// walredo's "no base image" error hit the materialiser's Err arm, which
    /// fatal-aborts the pageserver.
    #[tokio::test]
    async fn read_slru_chain_durable_resolves_ancestor_base() {
        let (store, schema) = make_store("slru_lineage").await;
        let (mut w, child) = writer(store.clone(), FlushPolicy::default());
        let anc = BranchId::generate();

        // Ancestor holds the anchor (the ZEROPAGE-derived base image); the
        // branch holds deltas only, which is the whole shape of the bug.
        store
            .put_slru_pages(anc, &[clog_row(10, true, true, b"anc-base")])
            .await
            .unwrap();
        store
            .put_slru_pages(
                child,
                &[
                    clog_row(30, false, false, b"d30"),
                    clog_row(40, false, false, b"d40"),
                ],
            )
            .await
            .unwrap();

        // Precondition: read branch-locally — exactly what the old code did —
        // and confirm it is genuinely anchorless. If this ever starts finding
        // an anchor, the test below has stopped covering #420.
        let local = store
            .read_slru_chain(child, SlruKind::Clog, 0, 7, Lsn(100))
            .await
            .unwrap();
        assert_eq!(local.len(), 2, "branch-local read sees only its own deltas");
        assert!(
            !local.iter().any(|r| r.will_init),
            "precondition: the branch-local chain has no anchor (that IS #420)"
        );

        w.set_lineage(vec![
            LineageSegment {
                branch_id: child,
                cap_lsn: Lsn::MAX,
            },
            LineageSegment {
                branch_id: anc,
                cap_lsn: Lsn(25),
            },
        ]);

        let chain = w
            .read_slru_chain_durable(SlruKind::Clog, 0, 7, Lsn(100))
            .await
            .expect("lineage walk must find the ancestor's anchor, not refuse");
        assert_eq!(
            chain.iter().map(|r| r.lsn.0).collect::<Vec<_>>(),
            vec![40, 30, 10],
            "LSN-descending, ancestor anchor stitched in at the tail"
        );
        let anchor = chain.last().unwrap();
        assert!(
            anchor.will_init && anchor.is_image,
            "the chain must terminate on the ancestor's base image so walredo has a base"
        );
        drop_schema(&store, &schema).await;
    }

    /// The SLRU twin of `read_chain_for_materialize_refuses_anchorless_chain`: when
    /// the anchor is missing from the branch AND every ancestor, refuse rather
    /// than let walredo fold `SETBIT` deltas onto a zero clog page (which
    /// would read back as "every transaction aborted"). Refusing here means
    /// `redo_one` logs and skips this one page; reaching walredo instead would
    /// kill the process.
    #[tokio::test]
    async fn read_slru_chain_durable_refuses_anchorless_chain() {
        let (store, schema) = make_store("slru_anchorless").await;
        let (w, child) = writer(store.clone(), FlushPolicy::default());
        store
            .put_slru_pages(
                child,
                &[
                    clog_row(30, false, false, b"d30"),
                    clog_row(40, false, false, b"d40"),
                ],
            )
            .await
            .unwrap();

        // Default single-segment lineage: no ancestor to rescue this chain.
        let res = w
            .read_slru_chain_durable(SlruKind::Clog, 0, 7, Lsn(100))
            .await;
        assert!(
            res.is_err(),
            "must error on an anchorless SLRU chain (would zero-base the clog page); \
             got Ok with {} rows",
            res.map(|c| c.len()).unwrap_or(0),
        );

        // An empty chain is a legitimate no-op, NOT an error — `redo_one`
        // maps it to "nothing to materialise yet". Guards the `!is_empty()`
        // half of the refusal condition.
        let empty = w
            .read_slru_chain_durable(SlruKind::Clog, 0, 999, Lsn(100))
            .await
            .expect("a block with no rows at all must not be treated as corruption");
        assert!(empty.is_empty());
        drop_schema(&store, &schema).await;
    }

    /// #1598: a batch mixing LSNs reads each key at its own LSN, so P's image
    /// at 300 (above P's 220, at Q's 300) cannot hide the image at 200. P and R
    /// share a group of non-adjacent blocks, so it takes the scattered-set read.
    #[tokio::test]
    async fn scattered_query_reads_each_key_at_its_own_lsn() {
        use crate::keyspace::KeySpace;
        use crate::page_server::timeline::{VersionedKeySpaceQuery, read_chains};

        let (store, schema) = make_store("scattered_lsn").await;
        let (w, _t) = writer(store.clone(), FlushPolicy::default());
        let (p, q, r) = (k(1), k(2), k(4));
        w.put(&[
            (r, Lsn(100), image()),
            (p, Lsn(200), image()),
            (p, Lsn(250), delta()),
            (p, Lsn(300), image()),
            (q, Lsn(300), image()),
            (r, Lsn(300), image()),
        ])
        .await
        .unwrap();
        w.flush().await.unwrap();

        let query = VersionedKeySpaceQuery::scattered(vec![
            (Lsn(300), KeySpace::single(q..q.next())),
            (
                Lsn(220),
                KeySpace {
                    ranges: vec![p..p.next(), r..r.next()],
                },
            ),
        ]);
        let (chains, _) = read_chains(&w, &query).await.unwrap();
        let (p_lsn, p_chain) = &chains[&p];
        assert_eq!(*p_lsn, Lsn(220));
        assert_eq!(p_chain.len(), 1);
        assert!(p_chain[0].is_image);
        assert_eq!(p_chain[0].lsn, Lsn(200));
        let (r_lsn, r_chain) = &chains[&r];
        assert_eq!(*r_lsn, Lsn(220));
        assert_eq!(r_chain.len(), 1);
        assert_eq!(r_chain[0].lsn, Lsn(100));
        let (q_lsn, q_chain) = &chains[&q];
        assert_eq!(*q_lsn, Lsn(300));
        assert_eq!(q_chain[0].lsn, Lsn(300));
        drop_schema(&store, &schema).await;
    }

    #[tokio::test]
    async fn long_chain_without_image_round_trips() {
        // 100 deltas after a will_init delta — no image, no truncation.
        // read_chain returns the full chain up to the anchor.
        let (store, schema) = make_store("long_chain").await;
        let (w, t) = writer(store.clone(), FlushPolicy::default());
        let init = Value::WalRecord(NeonWalRecord::Postgres {
            will_init: true,
            rec: Bytes::from_static(b"init"),
        });
        let mut entries: Vec<(Key, Lsn, Value)> = vec![(k(1), Lsn(10), init)];
        for i in 1..=100 {
            entries.push((k(1), Lsn(10 + 10 * i), delta()));
        }
        w.put(&entries).await.unwrap();
        w.flush().await.unwrap();
        let chain = store
            .read_chain(t, &key_to_bytes(k(1)), Lsn(2000))
            .await
            .unwrap();
        assert_eq!(chain.len(), 101);
        assert_eq!(chain[0].lsn, Lsn(1010));
        assert!(chain.last().unwrap().will_init);
        drop_schema(&store, &schema).await;
    }

    #[tokio::test]
    async fn routes_keys_to_three_buffers() {
        // Each Key kind lands in its own buffer:
        //   rel-page  → pages buffer
        //   slru-block → slru buffer
        //   anything else → meta buffer
        let (store, schema) = make_store("route_three").await;
        let (w, t) = writer(store.clone(), FlushPolicy::default());

        let meta_key = Key {
            field1: 0x60,
            field2: 0,
            field3: 0,
            field4: 0,
            field5: 0,
            field6: 0,
        };
        let slru_key =
            pageserver_api::key::slru_block_to_key(pageserver_api::reltag::SlruKind::Clog, 7, 42);

        w.put(&[
            (k(1), Lsn(10), image()),
            (slru_key, Lsn(10), image()),
            (meta_key, Lsn(10), image()),
        ])
        .await
        .unwrap();
        let stats = w.stats().await;
        assert_eq!(stats.rows_written, 3);
        assert_eq!(stats.buffered_rows, 1, "rel buffer");
        assert_eq!(stats.buffered_slru_rows, 1, "slru buffer");
        assert_eq!(stats.buffered_meta_rows, 1, "meta buffer");
        w.flush().await.unwrap();
        // Meta round-trips its payload bytes.
        let mut meta_key_bytes = vec![0u8; pageserver_api::key::KEY_SIZE];
        meta_key.write_to_byte_slice(&mut meta_key_bytes);
        let got = store
            .read_meta(t, &meta_key_bytes, Lsn(100))
            .await
            .unwrap()
            .expect("meta row");
        assert_eq!(got.payload.as_ref(), b"natural-image-bytes");
        drop_schema(&store, &schema).await;
    }

    /// Two concurrent `flush()` calls with a put between them: each drains
    /// only the SLRU/meta rows it wrote, so none is dropped. Multi-threaded,
    /// so the spawned flush runs alongside the second put.
    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn concurrent_flushes_lose_no_aux_rows() {
        let (store, schema) = make_store("concurrent_aux").await;
        let (w, t) = writer(store.clone(), FlushPolicy::default());
        let w = Arc::new(w);
        let meta_key = |n: u32| Key {
            field1: 0x60,
            field2: n,
            field3: 0,
            field4: 0,
            field5: 0,
            field6: 0,
        };
        let slru_key = |blk: u32| {
            pageserver_api::key::slru_block_to_key(pageserver_api::reltag::SlruKind::Clog, 7, blk)
        };
        w.put(&[
            (k(1), Lsn(10), image()),
            (slru_key(1), Lsn(10), image()),
            (meta_key(1), Lsn(10), image()),
        ])
        .await
        .unwrap();
        let first = {
            let w = Arc::clone(&w);
            tokio::spawn(async move { w.flush().await })
        };
        w.put(&[
            (k(2), Lsn(20), image()),
            (slru_key(2), Lsn(20), image()),
            (meta_key(2), Lsn(20), image()),
        ])
        .await
        .unwrap();
        let second = w.flush();
        let (first, second) = tokio::join!(first, second);
        first.unwrap().unwrap();
        second.unwrap();

        let stats = w.stats().await;
        assert_eq!(stats.buffered_rows, 0);
        assert_eq!(stats.buffered_slru_rows, 0);
        assert_eq!(stats.buffered_meta_rows, 0);
        for n in [1, 2] {
            let mut kb = vec![0u8; pageserver_api::key::KEY_SIZE];
            meta_key(n).write_to_byte_slice(&mut kb);
            let got = store.read_meta(t, &kb, Lsn(100)).await.unwrap();
            assert!(got.is_some(), "meta row {n} reached YB");
        }
        drop_schema(&store, &schema).await;
    }

    /// A `flush()` dropped mid-way (a caller's timeout) must hand its claimed
    /// rows back: the next flush completes and writes everything.
    #[tokio::test]
    async fn cancelled_flush_does_not_strand_its_rows() {
        let (store, schema) = make_store("cancelled_flush").await;
        let (w, _t) = writer(store.clone(), FlushPolicy::default());
        // Cut flushes short at a spread of points: waiting for the lock,
        // during the write, after the mark.
        for (i, micros) in [1u64, 50, 500, 5_000, 50_000].into_iter().enumerate() {
            let lsn = Lsn(10 * (i as u64 + 1));
            w.put(&[(k(i as u32 + 1), lsn, image())]).await.unwrap();
            let _ = tokio::time::timeout(Duration::from_micros(micros), w.flush()).await;
        }
        tokio::time::timeout(Duration::from_secs(60), w.flush())
            .await
            .expect("flush after a cancelled one hung")
            .unwrap();
        assert_eq!(
            w.stats().await.buffered_rows,
            0,
            "every claimed row was rewritten"
        );
        drop_schema(&store, &schema).await;
    }

    /// A flush whose write fails after its rel chunks landed hands every row
    /// back, counted against backpressure again, and the next flush writes
    /// the rel, SLRU and meta rows.
    #[tokio::test]
    async fn failed_flush_hands_its_rows_back() {
        let (store, schema) = make_store("failed_flush").await;
        let (w, t) = writer(store.clone(), FlushPolicy::default());
        let meta_key = Key {
            field1: 0x60,
            field2: 1,
            field3: 0,
            field4: 0,
            field5: 0,
            field6: 0,
        };
        let slru_key =
            pageserver_api::key::slru_block_to_key(pageserver_api::reltag::SlruKind::Clog, 7, 1);
        w.put(&[
            (k(1), Lsn(10), image()),
            (k(2), Lsn(10), image()),
            (slru_key, Lsn(10), image()),
            (meta_key, Lsn(10), image()),
        ])
        .await
        .unwrap();
        let unflushed = w.pages.unflushed_bytes();
        let rel_key = |n: u32| {
            let mut kb = vec![0u8; pageserver_api::key::KEY_SIZE];
            k(n).write_to_byte_slice(&mut kb);
            kb
        };

        w.fail_write_after_rel.store(true, Ordering::Relaxed);
        w.flush().await.expect_err("injected write failure");
        assert!(
            !store
                .read_chain(t, &rel_key(1), Lsn(100))
                .await
                .unwrap()
                .is_empty(),
            "the rel rows landed before the failure"
        );
        let stats = w.stats().await;
        assert_eq!(stats.buffered_rows, 2, "rel rows handed back");
        assert_eq!(stats.buffered_slru_rows, 1);
        assert_eq!(stats.buffered_meta_rows, 1);
        assert_eq!(w.pages.unflushed_bytes(), unflushed);
        assert_eq!(
            w.landed_unmarked_bytes.load(Ordering::Relaxed),
            0,
            "the landed rel rows count against backpressure again"
        );

        w.flush().await.unwrap();
        let stats = w.stats().await;
        assert_eq!(stats.buffered_rows, 0);
        assert_eq!(stats.buffered_slru_rows, 0);
        assert_eq!(stats.buffered_meta_rows, 0);
        for n in [1, 2] {
            let got = store.read_chain(t, &rel_key(n), Lsn(100)).await.unwrap();
            assert_eq!(got.len(), 1, "rel row {n} in YB");
        }
        let mut kb = vec![0u8; pageserver_api::key::KEY_SIZE];
        meta_key.write_to_byte_slice(&mut kb);
        assert!(store.read_meta(t, &kb, Lsn(100)).await.unwrap().is_some());
        drop_schema(&store, &schema).await;
    }

    /// Landed rows stop counting against `backpressure_bytes`, but `put`
    /// still blocks once the buffer holds twice the cap, however much of it
    /// has landed: a run of failing flushes can't grow it without bound.
    #[tokio::test]
    async fn backpressure_stops_at_twice_the_cap() {
        let (store, schema) = make_store("bp_ceiling").await;
        let cap = 1000;
        let policy = FlushPolicy {
            backpressure_bytes: cap,
            ..FlushPolicy::default()
        };
        let (w, _t) = writer(store.clone(), policy);
        // `Err` when the put is still blocked in backpressure.
        let put = |n: u32| {
            let w = &w;
            async move {
                let rows = [(k(n), Lsn(10), image())];
                tokio::time::timeout(Duration::from_millis(500), w.put(&rows))
                    .await
                    .map(|r| r.unwrap())
            }
        };

        let mut n = 0;
        while w.pages.unflushed_bytes() < cap {
            n += 1;
            put(n).await.expect("below the cap");
        }
        n += 1;
        assert!(put(n).await.is_err(), "blocks at the cap");

        // As if a flush had landed every buffered row and not yet marked them.
        w.landed_unmarked_bytes
            .store(w.pages.unflushed_bytes(), Ordering::Relaxed);
        while w.pages.unflushed_bytes() < 2 * cap {
            put(n).await.expect("landed rows don't count");
            n += 1;
        }
        assert!(put(n).await.is_err(), "blocks at twice the cap");

        w.landed_unmarked_bytes.store(0, Ordering::Relaxed);
        w.flush().await.unwrap();
        put(n).await.expect("the flush drained the buffer");
        drop_schema(&store, &schema).await;
    }

    #[tokio::test]
    async fn flush_emits_a_single_insert_round_trip() {
        let (store, schema) = make_store("flush_one_rt").await;
        let (w, t) = writer(store.clone(), FlushPolicy::default());
        let entries: Vec<(Key, Lsn, Value)> = (0..50).map(|i| (k(i), Lsn(10), image())).collect();
        w.put(&entries).await.unwrap();
        assert_eq!(w.stats().await.buffered_rows, 50);
        w.flush().await.unwrap();
        assert_eq!(w.stats().await.buffered_rows, 0);
        // Sample-read one of them.
        let chain = store
            .read_chain(t, &key_to_bytes(k(0)), Lsn(100))
            .await
            .unwrap();
        assert_eq!(chain.len(), 1);
        assert!(chain[0].is_image);
        drop_schema(&store, &schema).await;
    }

    #[tokio::test]
    async fn should_flush_on_size() {
        let (store, schema) = make_store("flush_size").await;
        let policy = FlushPolicy {
            max_age: Duration::from_secs(3600),
            max_bytes: 200,
            backpressure_bytes: 0,
            ingest_parallelism: 1,
            ingest_parallelism_min_batch: 256,
            ..FlushPolicy::default()
        };
        let (w, _t) = writer(store.clone(), policy);
        assert!(!w.should_flush().await);
        for i in 0..4 {
            w.put(&[(k(i), Lsn(10), image())]).await.unwrap();
        }
        assert!(w.should_flush().await, "stats: {:?}", w.stats().await);
        drop_schema(&store, &schema).await;
    }

    #[tokio::test]
    async fn should_flush_on_age() {
        let (store, schema) = make_store("flush_age").await;
        let policy = FlushPolicy {
            max_age: Duration::from_millis(100),
            max_bytes: 1_000_000_000,
            backpressure_bytes: 0,
            ingest_parallelism: 1,
            ingest_parallelism_min_batch: 256,
            ..FlushPolicy::default()
        };
        let (w, _t) = writer(store.clone(), policy);
        w.put(&[(k(1), Lsn(10), image())]).await.unwrap();
        assert!(!w.should_flush().await);
        tokio::time::sleep(Duration::from_millis(150)).await;
        assert!(w.should_flush().await);
        w.flush().await.unwrap();
        assert!(!w.should_flush().await);
        drop_schema(&store, &schema).await;
    }

    /// #1679: a row flushed and evicted while a multi-block scan is in
    /// flight must still be in its result.
    async fn scan_misses_no_row_flushed_mid_scan(scattered: bool) {
        let (store, schema) = make_store("scan_mid_flush").await;
        let branch = BranchId::generate();
        // A 1-byte cache evicts every unpinned key on each flush.
        let w = Arc::new(PageWriter::new(
            branch,
            store.clone(),
            store.clone(),
            FlushPolicy::default(),
            1,
            0,
        ));
        w.put(&[(k(7), Lsn(100), image()), (k(8), Lsn(100), image())])
            .await
            .unwrap();
        w.flush().await.unwrap();
        w.put(&[(k(7), Lsn(110), delta())]).await.unwrap();

        let pause = Arc::new(ScanPause::default());
        *w.scan_pause.lock() = Some(pause.clone());
        let reader = {
            let w = w.clone();
            tokio::spawn(async move {
                if scattered {
                    w.scan_scattered_set(&[k(7), k(8)], Lsn(120)).await
                } else {
                    w.scan_range(k(7)..k(9), Lsn(120)).await
                }
            })
        };
        pause.reached.notified().await;
        w.flush().await.unwrap();
        assert!(
            w.pages.lookup_chain(&k(7), Lsn(120)).is_empty(),
            "the flush should have evicted k(7) from the cache"
        );
        pause.resume.notify_one();

        let out = reader.await.unwrap().unwrap();
        let lsns: Vec<Lsn> = out[&k(7)].iter().map(|r| r.lsn).collect();
        assert_eq!(lsns, vec![Lsn(110), Lsn(100)]);
        drop_schema(&store, &schema).await;
    }

    #[tokio::test]
    async fn scan_range_misses_no_row_flushed_mid_scan() {
        scan_misses_no_row_flushed_mid_scan(false).await;
    }

    #[tokio::test]
    async fn scan_scattered_set_misses_no_row_flushed_mid_scan() {
        scan_misses_no_row_flushed_mid_scan(true).await;
    }

    /// #1679: the SLRU twin of `scan_misses_no_row_flushed_mid_scan`, where
    /// the flush drains the row from the SLRU buffer.
    #[tokio::test]
    async fn scan_slru_range_misses_no_row_flushed_mid_scan() {
        use pageserver_api::key::slru_block_to_key;
        use pageserver_api::reltag::SlruKind;
        let (store, schema) = make_store("slru_scan_mid_flush").await;
        let (w, _t) = writer(store.clone(), FlushPolicy::default());
        let w = Arc::new(w);
        let b0 = slru_block_to_key(SlruKind::Clog, 7, 0);
        let b1 = slru_block_to_key(SlruKind::Clog, 7, 1);
        w.put(&[(b0, Lsn(100), image()), (b1, Lsn(100), image())])
            .await
            .unwrap();
        w.flush().await.unwrap();
        w.put(&[(b0, Lsn(110), delta_non_init())]).await.unwrap();

        let pause = Arc::new(ScanPause::default());
        *w.scan_pause.lock() = Some(pause.clone());
        let reader = {
            let w = w.clone();
            tokio::spawn(async move {
                w.scan_slru_range(
                    SlruKind::Clog,
                    7,
                    0..2,
                    Lsn(120),
                    crate::page_store::SlruScan::ToAnchor,
                )
                .await
            })
        };
        pause.reached.notified().await;
        w.flush().await.unwrap();
        assert_eq!(w.stats().await.buffered_slru_rows, 0);
        pause.resume.notify_one();

        let out = reader.await.unwrap().unwrap();
        let lsns: Vec<Lsn> = out[&0].iter().map(|r| r.lsn).collect();
        assert_eq!(lsns, vec![Lsn(110), Lsn(100)]);
        drop_schema(&store, &schema).await;
    }

    #[tokio::test]
    async fn flush_leaving_rows_restarts_the_age_clock() {
        let (store, schema) = make_store("flush_age_reset").await;
        let policy = FlushPolicy {
            max_age: Duration::from_millis(100),
            max_bytes: 1_000_000_000,
            backpressure_bytes: 0,
            ingest_parallelism: 1,
            ingest_parallelism_min_batch: 256,
            ..FlushPolicy::default()
        };
        let (w, _t) = writer(store.clone(), policy);
        w.put(&[(k(1), Lsn(10), image())]).await.unwrap();
        tokio::time::sleep(Duration::from_millis(150)).await;
        let (flushed, put) = tokio::join!(w.flush(), async {
            tokio::task::yield_now().await;
            w.put(&[(k(2), Lsn(20), image())]).await
        });
        flushed.unwrap();
        put.unwrap();
        assert_eq!(w.stats().await.buffered_rows, 1);
        assert!(!w.should_flush().await);
        drop_schema(&store, &schema).await;
    }

    #[tokio::test]
    async fn read_chain_blends_buffered_and_persisted() {
        let (store, schema) = make_store("blend").await;
        let (w, _t) = writer(store.clone(), FlushPolicy::default());
        let first_batch: Vec<(Key, Lsn, Value)> = (0..5)
            .map(|i| {
                (
                    k(1),
                    Lsn(10 + 10 * i),
                    if i == 0 { image() } else { delta() },
                )
            })
            .collect();
        w.put(&first_batch).await.unwrap();
        w.flush().await.unwrap();
        let second_batch: Vec<(Key, Lsn, Value)> =
            (5..8).map(|i| (k(1), Lsn(10 + 10 * i), delta())).collect();
        w.put(&second_batch).await.unwrap();
        assert!(w.stats().await.buffered_rows > 0);
        let chain = w.read_chain(k(1), Lsn(1000)).await.unwrap();
        // image at 10 + 7 deltas at 20..80.
        assert_eq!(chain.len(), 8);
        let mut prev = u64::MAX;
        for row in &chain {
            assert!(row.lsn.0 < prev);
            prev = row.lsn.0;
        }
        assert_eq!(chain.last().unwrap().lsn, Lsn(10));
        assert!(chain.last().unwrap().will_init);
        drop_schema(&store, &schema).await;
    }

    #[tokio::test]
    async fn read_chain_truncates_at_will_init_anchor() {
        let (store, schema) = make_store("trunc_at_anchor").await;
        let (w, _t) = writer(store.clone(), FlushPolicy::default());
        w.put(&[(k(1), Lsn(10), image())]).await.unwrap();
        for i in 1..=3 {
            w.put(&[(k(1), Lsn(10 + 10 * i), delta())]).await.unwrap();
        }
        let init = Value::WalRecord(NeonWalRecord::Postgres {
            will_init: true,
            rec: Bytes::from_static(b"init"),
        });
        w.put(&[(k(1), Lsn(50), init)]).await.unwrap();
        for i in 6..=8 {
            w.put(&[(k(1), Lsn(10 + 10 * i), delta())]).await.unwrap();
        }
        w.flush().await.unwrap();
        let chain = w.read_chain(k(1), Lsn(1000)).await.unwrap();
        // 4 rows: deltas at 90/80/70, anchor at 50.
        assert_eq!(chain.len(), 4);
        assert_eq!(chain.first().unwrap().lsn, Lsn(90));
        assert_eq!(chain.last().unwrap().lsn, Lsn(50));
        assert!(chain.last().unwrap().will_init);
        drop_schema(&store, &schema).await;
    }

    #[tokio::test]
    async fn periodic_flusher_drains_on_age_trigger() {
        let (store, schema) = make_store("flusher_age").await;
        let t = BranchId::generate();
        let policy = FlushPolicy {
            max_age: Duration::from_millis(40),
            max_bytes: 1 << 30,
            backpressure_bytes: 0,
            ingest_parallelism: 1,
            ingest_parallelism_min_batch: 256,
            ..FlushPolicy::default()
        };
        let w = Arc::new(PageWriter::new(
            t,
            store.clone(),
            store.clone(),
            policy,
            64 * 1024 * 1024,
            0,
        ));
        let task = w.spawn_flusher(Duration::from_millis(15));
        w.put(&[(k(1), Lsn(10), image())]).await.unwrap();
        // Give the flusher long enough to drain on the common path (40ms
        // age + 15ms tick), then poll rather than asserting after a single
        // fixed sleep. The behavior under test is "the periodic flusher
        // eventually drains on the age trigger"; the *latency* of that drain
        // depends on the YB INSERT round-trip, which under full-suite
        // parallelism (shared connection, busy runtime) can exceed any fixed
        // budget — that one-shot assert is what made this flaky. Wait for
        // buffered_rows to reach 0 with a generous ceiling.
        tokio::time::sleep(Duration::from_millis(50)).await;
        let start = std::time::Instant::now();
        loop {
            let stats = w.stats().await;
            if stats.buffered_rows == 0 {
                break;
            }
            assert!(
                start.elapsed() < Duration::from_secs(10),
                "periodic flusher did not drain on age within 10s: {stats:?}"
            );
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
        let chain = store
            .read_chain(t, &key_to_bytes(k(1)), Lsn(100))
            .await
            .unwrap();
        assert_eq!(chain.len(), 1);
        task.shutdown().await;
        drop_schema(&store, &schema).await;
    }

    /// A `put` blocked on backpressure wakes the flusher, whose size trigger
    /// is clamped to the cap, so it is released long before the age trigger.
    #[tokio::test]
    async fn backpressured_put_is_released_before_the_age_trigger() {
        let (store, schema) = make_store("flusher_backpressure").await;
        let policy = FlushPolicy {
            max_age: Duration::from_secs(3600),
            max_bytes: 1 << 30,
            backpressure_bytes: 1,
            ingest_parallelism: 1,
            ingest_parallelism_min_batch: 256,
            release_backpressure_per_chunk: true,
        };
        let (w, _t) = writer(store.clone(), policy);
        let w = Arc::new(w);
        let task = w.spawn_flusher(Duration::from_secs(3600));
        // Let the interval's immediate first tick pass on an empty buffer.
        tokio::time::sleep(Duration::from_millis(100)).await;
        w.put(&[(k(1), Lsn(10), image())]).await.unwrap();
        tokio::time::timeout(Duration::from_secs(10), w.put(&[(k(2), Lsn(20), image())]))
            .await
            .expect("put still blocked on backpressure after 10s")
            .unwrap();
        assert_eq!(w.backpressure_waits.load(Ordering::Relaxed), 1);
        task.shutdown().await;
        drop_schema(&store, &schema).await;
    }

    #[tokio::test]
    async fn periodic_flusher_does_final_flush_on_shutdown() {
        let (store, schema) = make_store("flusher_final").await;
        let t = BranchId::generate();
        let policy = FlushPolicy {
            max_age: Duration::from_secs(3600),
            max_bytes: 1 << 30,
            backpressure_bytes: 0,
            ingest_parallelism: 1,
            ingest_parallelism_min_batch: 256,
            ..FlushPolicy::default()
        };
        let w = Arc::new(PageWriter::new(
            t,
            store.clone(),
            store.clone(),
            policy,
            64 * 1024 * 1024,
            0,
        ));
        let task = w.spawn_flusher(Duration::from_millis(15));
        w.put(&[(k(1), Lsn(10), image())]).await.unwrap();
        tokio::time::sleep(Duration::from_millis(80)).await;
        assert!(w.stats().await.buffered_rows > 0);
        task.shutdown().await;
        assert_eq!(w.stats().await.buffered_rows, 0);
        let chain = store
            .read_chain(t, &key_to_bytes(k(1)), Lsn(100))
            .await
            .unwrap();
        assert_eq!(chain.len(), 1);
        drop_schema(&store, &schema).await;
    }

    /// Regression test for the flush race fixed in 8a685a4dd.
    ///
    /// Before the fix, PageWriter::flush() did `mem::take` on the
    /// in-memory buffer up front, then issued the YB INSERT outside
    /// the lock. During the round-trip the just-buffered rows were in
    /// NEITHER the buffer NOR YB. Any concurrent read in that window
    /// returned no data — walreceiver's read-modify-write on rel-size
    /// fork events then hit MissingKey, propagated corruption to compute,
    /// and we'd lose the workload.
    ///
    /// Now flush clones the buffer, INSERTs from the clone, and drains
    /// the originals only on success. Reads keep seeing the rows
    /// throughout the round-trip.
    ///
    /// We exercise the window by spawning concurrent flush + read
    /// tasks via tokio::join!. With a real YB round-trip the read
    /// races into the flush window most of the time; an occasional
    /// "read fires before flush even starts" outcome is also fine for
    /// this assertion (the read still finds the row in the un-drained
    /// buffer). The bug pattern would manifest as the read seeing an
    /// empty result while the buffer is in flight.
    #[tokio::test]
    async fn read_during_in_flight_flush_sees_buffered_rows() {
        let (store, schema) = make_store("flush_race").await;
        let (w, _t) = writer(store.clone(), FlushPolicy::default());
        let w = Arc::new(w);

        // Seed enough rows that the flush has a non-trivial round-trip
        // (multiple chunks against YB) and a read fired concurrently
        // has a real chance to land mid-flush.
        let mut entries = Vec::with_capacity(800);
        for i in 0u32..800 {
            entries.push((k(i), Lsn(10 + i as u64), image()));
        }
        w.put(&entries).await.unwrap();
        assert_eq!(w.stats().await.buffered_rows, 800);

        let w_flush = w.clone();
        let w_read = w.clone();
        let target_key = k(500);
        let (flush_res, read_res) = tokio::join!(
            tokio::spawn(async move { w_flush.flush().await }),
            tokio::spawn(async move { w_read.read_chain(target_key, Lsn(10_000)).await }),
        );
        flush_res.unwrap().unwrap();
        let chain = read_res.unwrap().unwrap();
        assert!(
            !chain.is_empty(),
            "concurrent read during flush returned empty — flush race regressed"
        );
        assert_eq!(chain[0].lsn, Lsn(10 + 500));
        // Sanity: after flush completes, the buffer is empty and the
        // row is persisted.
        assert_eq!(w.stats().await.buffered_rows, 0);
        drop_schema(&store, &schema).await;
    }

    #[tokio::test]
    async fn passthrough_payload_is_serialised_wal_record() {
        let (store, schema) = make_store("ser_rt").await;
        let (w, t) = writer(store.clone(), FlushPolicy::default());
        let rec = NeonWalRecord::ClearVisibilityMapFlags {
            new_heap_blkno: Some(42),
            old_heap_blkno: Some(43),
            flags: 0x07,
        };
        w.put(&[(k(1), Lsn(10), image())]).await.unwrap();
        w.put(&[(k(1), Lsn(20), Value::WalRecord(rec.clone()))])
            .await
            .unwrap();
        w.flush().await.unwrap();
        let chain = store
            .read_chain(t, &key_to_bytes(k(1)), Lsn(100))
            .await
            .unwrap();
        assert_eq!(chain.len(), 2);
        let delta_row = chain.iter().find(|r| !r.is_image).expect("one delta row");
        assert!(!delta_row.will_init);
        let decoded = NeonWalRecord::des(&delta_row.payload).expect("ser/des round-trip");
        assert_eq!(decoded, rec);
        drop_schema(&store, &schema).await;
    }

    /// After `limit` consecutive non-init deltas on one key, exactly
    /// one materialise job lands on the queue. The counter then
    /// resets, so the *next* delta only re-trips after another full
    /// `limit` deltas (verified at the end).
    #[tokio::test]
    async fn delta_threshold_dispatches_one_job() {
        let (store, schema) = make_store("mat_threshold").await;
        const LIMIT: u32 = 10;
        let (w, _t, mut rx, in_flight) =
            writer_with_materializer(store.clone(), FlushPolicy::default(), LIMIT);

        // (LIMIT - 1) deltas — the counter sits below threshold.
        for i in 0..(LIMIT - 1) {
            w.put(&[(k(1), Lsn(10 + i as u64), delta_non_init())])
                .await
                .unwrap();
        }
        w.flush().await.unwrap();
        assert!(rx.try_recv().is_err(), "premature dispatch below threshold");

        // LIMITth delta crosses: exactly one job, at that delta's LSN.
        w.put(&[(k(1), Lsn(10 + LIMIT as u64), delta_non_init())])
            .await
            .unwrap();
        w.flush().await.unwrap();
        let job = rx
            .try_recv()
            .expect("dispatched after flush hits threshold");
        assert_eq!(job.target_lsn, Lsn(10 + LIMIT as u64));
        assert_eq!(job.key, k(1));
        // Simulate the materialiser finishing this batch.
        in_flight.lock().unwrap().remove(&job.key);

        // Counter reset: another (LIMIT-1) deltas + flush must not
        // re-dispatch.
        for i in 0..(LIMIT - 1) {
            w.put(&[(k(1), Lsn(100 + i as u64), delta_non_init())])
                .await
                .unwrap();
        }
        w.flush().await.unwrap();
        assert!(rx.try_recv().is_err(), "dispatch before second threshold");
        w.put(&[(k(1), Lsn(200), delta_non_init())]).await.unwrap();
        w.flush().await.unwrap();
        let _ = rx.try_recv().expect("dispatch on second threshold");

        drop_schema(&store, &schema).await;
    }

    /// The GC drain's deferral input. `objects_with_pending_materialise`
    /// must report a fork while a job is pending (`in_flight`, from
    /// dispatch until the batch completes) AND while a
    /// threshold-crossed key sits undispatched in `keys_to_materialize` (cold
    /// residue a later flush may still dispatch) — and go quiet once
    /// both clear. Guards the drop deferral against the
    /// residue-dispatch race: a drop event acted while either
    /// population held its fork could race an image INSERT at an old
    /// (folded) LSN.
    #[tokio::test]
    async fn pending_materialise_forks_cover_inflight_and_residue() {
        let (store, schema) = make_store("mat_forks").await;
        const LIMIT: u32 = 10;
        let (w, _t, mut rx, in_flight) =
            writer_with_materializer(store.clone(), FlushPolicy::default(), LIMIT);
        // The busy set holds each object's sentinel key (blknum → MAX) —
        // the same key a drop candidate carries. k(2)'s object is its
        // rel-size sentinel.
        let fork = Key {
            field6: 0xFFFF_FFFF,
            ..k(2)
        };

        assert!(
            w.objects_with_pending_materialise().is_empty(),
            "quiescent writer reports no busy objects"
        );

        // Cross the threshold: job enqueued → in_flight.
        for i in 0..LIMIT {
            w.put(&[(k(2), Lsn(10 + i as u64), delta_non_init())])
                .await
                .unwrap();
        }
        w.flush().await.unwrap();
        let job1 = rx.try_recv().expect("dispatched at threshold");
        assert!(
            w.objects_with_pending_materialise().contains(&fork),
            "queued/running job must mark its fork busy"
        );

        // Cross again while job1 is still in flight: the re-crossed key
        // is blocked from dispatch and parks in keys_to_materialize (residue).
        for i in 0..LIMIT {
            w.put(&[(k(2), Lsn(100 + i as u64), delta_non_init())])
                .await
                .unwrap();
        }
        assert!(rx.try_recv().is_err(), "in_flight blocks the re-dispatch");

        // job1's batch completes — but the residue must keep the fork
        // busy: the next retry can still enqueue it.
        in_flight.lock().unwrap().remove(&job1.key);
        assert!(
            w.objects_with_pending_materialise().contains(&fork),
            "undispatched keys_to_materialize residue must keep the fork busy"
        );

        w.retry_enqueue_rel_materialize_jobs();
        let job2 = rx.try_recv().expect("residue dispatches once unblocked");
        assert!(w.objects_with_pending_materialise().contains(&fork));

        // Second batch completes: nothing pending anywhere → free.
        in_flight.lock().unwrap().remove(&job2.key);
        assert!(
            !w.objects_with_pending_materialise().contains(&fork),
            "fork frees once in_flight and residue are both clear"
        );

        // SLRU jobs are materialised too (clog / multixact), so an
        // in-flight SLRU block must mark its SEGMENT busy (by its seg-size
        // sentinel) — otherwise a DropSlru racing that job would leak a
        // resurrected image.
        let slru_block =
            pageserver_api::key::slru_block_to_key(pageserver_api::reltag::SlruKind::Clog, 7, 3);
        let slru_seg = pageserver_api::key::slru_segment_size_to_key(
            pageserver_api::reltag::SlruKind::Clog,
            7,
        );
        in_flight.lock().unwrap().insert(slru_block);
        assert!(
            w.objects_with_pending_materialise().contains(&slru_seg),
            "an in-flight SLRU block marks its segment (seg-size sentinel) busy"
        );
        in_flight.lock().unwrap().remove(&slru_block);

        drop_schema(&store, &schema).await;
    }

    /// SLRU (clog) block keys use the dedicated `slru_count_limit`, not
    /// the relation-page `count_limit`. After `SLRU_LIMIT` non-init
    /// deltas on one clog block the post-flush trigger dispatches exactly
    /// one materialise job, and a relation key is left untouched when its
    /// own limits are 0. Guards the clog-chain growth that made basebackup
    /// scale with commit volume (the page was never materialised before).
    #[tokio::test]
    async fn slru_delta_threshold_dispatches_one_job() {
        use pageserver_api::key::slru_block_to_key;
        use pageserver_api::reltag::SlruKind;
        let (store, schema) = make_store("mat_slru_threshold").await;
        const SLRU_LIMIT: u32 = 8;
        // Rel limits 0 so *only* the SLRU trigger can fire.
        let (w, _t, mut rx, in_flight) = writer_with_materializer_thresholds(
            store.clone(),
            FlushPolicy::default(),
            0,
            0,
            SLRU_LIMIT,
        );
        let sk = slru_block_to_key(SlruKind::Clog, 0, 0);

        // SLRU keys never join the rel flush snapshot, so they dispatch
        // only via the cold-residue path — which is gated on the durable
        // `flush_horizon_lsn`. In the real cluster the walreceiver advances
        // that continuously as it ingests; here we note record LSNs to
        // simulate it (mirrors `flush_horizon_advances_to_ingest_frontier`).

        // (SLRU_LIMIT - 1) deltas + flush — below threshold, no dispatch.
        for i in 0..(SLRU_LIMIT - 1) {
            w.put(&[(sk, Lsn(10 + i as u64), delta_non_init())])
                .await
                .unwrap();
        }
        w.note_record_lsn(Lsn(10 + SLRU_LIMIT as u64), Lsn(10));
        w.flush().await.unwrap();
        assert!(
            rx.try_recv().is_err(),
            "premature SLRU dispatch below threshold"
        );

        // The threshold-crossing delta + flush dispatches exactly one job
        // for this clog block at its latest (now-durable) LSN.
        w.put(&[(sk, Lsn(10 + SLRU_LIMIT as u64), delta_non_init())])
            .await
            .unwrap();
        w.flush().await.unwrap();
        let job = rx
            .try_recv()
            .expect("SLRU dispatch after flush hits threshold");
        assert_eq!(job.key, sk);
        assert_eq!(job.target_lsn, Lsn(10 + SLRU_LIMIT as u64));
        in_flight.lock().unwrap().remove(&job.key);

        // A relation key, with rel limits at 0, must NOT dispatch — the
        // SLRU threshold is independent of and not applied to rel keys.
        for i in 0..(SLRU_LIMIT + 5) {
            w.put(&[(k(1), Lsn(300 + i as u64), delta_non_init())])
                .await
                .unwrap();
        }
        w.flush().await.unwrap();
        assert!(
            rx.try_recv().is_err(),
            "rel key dispatched despite rel limits = 0"
        );

        drop_schema(&store, &schema).await;
    }

    /// An image row resets the per-key counter — the chain is anchored
    /// again, so post-image deltas count from zero.
    #[tokio::test]
    async fn image_resets_chain_counter() {
        let (store, schema) = make_store("mat_image_reset").await;
        const LIMIT: u32 = 10;
        let (w, _t, mut rx, _in_flight) =
            writer_with_materializer(store.clone(), FlushPolicy::default(), LIMIT);

        // 5 deltas, then an image, then 9 deltas + flush — must NOT
        // dispatch (counter reset by the image, then only 9 deltas
        // accumulated again).
        for i in 0..5 {
            w.put(&[(k(1), Lsn(10 + i), delta_non_init())])
                .await
                .unwrap();
        }
        w.put(&[(k(1), Lsn(50), image())]).await.unwrap();
        for i in 0..(LIMIT - 1) {
            w.put(&[(k(1), Lsn(100 + i as u64), delta_non_init())])
                .await
                .unwrap();
        }
        w.flush().await.unwrap();
        assert!(
            rx.try_recv().is_err(),
            "image should have reset the counter"
        );
        // 10th post-image delta + flush: now dispatch.
        w.put(&[(k(1), Lsn(200), delta_non_init())]).await.unwrap();
        w.flush().await.unwrap();
        let job = rx.try_recv().expect("dispatch after 10 post-image deltas");
        assert_eq!(job.target_lsn, Lsn(200));

        drop_schema(&store, &schema).await;
    }

    /// A key whose enqueue failed at the crossing and is then reset by an
    /// anchor must NOT be retried: the anchor clears both the tally and
    /// the ready-set entry.
    #[tokio::test]
    async fn anchor_after_crossing_cancels_dispatch() {
        let (store, schema) = make_store("mat_cross_then_reset").await;
        const LIMIT: u32 = 3;
        let (w, _t, mut rx, in_flight) =
            writer_with_materializer(store.clone(), FlushPolicy::default(), LIMIT);

        // Cross while the key is being folded — the enqueue fails and k(1)
        // enters the ready set.
        in_flight.lock().unwrap().insert(k(1));
        for i in 0..LIMIT {
            w.put(&[(k(1), Lsn(10 + i as u64), delta_non_init())])
                .await
                .unwrap();
        }
        assert!(w.keys_to_materialize.contains(&k(1)));
        // Anchor at a higher LSN resets the chain before the retry.
        w.put(&[(k(1), Lsn(100), image())]).await.unwrap();
        in_flight.lock().unwrap().remove(&k(1));
        w.retry_enqueue_rel_materialize_jobs();
        assert!(
            rx.try_recv().is_err(),
            "anchor after crossing must cancel the pending enqueue",
        );

        drop_schema(&store, &schema).await;
    }

    /// Hybrid materialiser threshold — payload-bytes branch.
    ///
    /// Regression for commit `895b7eca5` (Phase 4i: count OR payload
    /// bytes). The existing threshold tests all set
    /// `payload_bytes_limit = 0` (disabled) and only exercise the
    /// count branch. This pins the bytes branch:
    ///
    /// - With count disabled (`count_limit = 0`) and
    ///   `payload_bytes_limit` set to exactly 3 deltas' worth of
    ///   payload, exactly 3 non-init deltas must dispatch.
    /// - Fewer-than-3 must not dispatch; the 3rd is the one that fires.
    /// - After the in-flight slot clears the counter resets to 0 —
    ///   accumulating another 3 deltas re-fires.
    #[tokio::test]
    async fn payload_bytes_threshold_dispatches_on_byte_budget() {
        let (store, schema) = make_store("mat_bytes_threshold").await;

        // Measure the serialised size of a single non-init delta —
        // can't hardcode it because the wire format is internal.
        let one_delta_bytes = match delta_non_init() {
            Value::WalRecord(rec) => rec.ser().expect("ser delta").len() as u32,
            _ => panic!("delta_non_init must be a WalRecord"),
        };
        let limit = one_delta_bytes * 3; // exactly 3 deltas trigger.

        let (w, _t, mut rx, in_flight) =
            writer_with_materializer_thresholds(store.clone(), FlushPolicy::default(), 0, limit, 0);

        w.put(&[(k(1), Lsn(10), delta_non_init())]).await.unwrap();
        w.put(&[(k(1), Lsn(20), delta_non_init())]).await.unwrap();
        w.flush().await.unwrap();
        assert!(
            rx.try_recv().is_err(),
            "must not dispatch before payload-bytes threshold is reached"
        );
        // 3rd delta + flush puts cumulative bytes at exactly the limit.
        w.put(&[(k(1), Lsn(30), delta_non_init())]).await.unwrap();
        w.flush().await.unwrap();
        let job = rx
            .try_recv()
            .expect("dispatched on hitting payload-bytes threshold");
        assert_eq!(job.target_lsn, Lsn(30));

        in_flight.lock().unwrap().remove(&job.key);

        // Counter reset on dispatch — another 2 deltas + flush must
        // NOT dispatch.
        w.put(&[(k(1), Lsn(40), delta_non_init())]).await.unwrap();
        w.put(&[(k(1), Lsn(50), delta_non_init())]).await.unwrap();
        w.flush().await.unwrap();
        assert!(
            rx.try_recv().is_err(),
            "counter must reset after dispatch; tally restarts at 0"
        );
        w.put(&[(k(1), Lsn(60), delta_non_init())]).await.unwrap();
        w.flush().await.unwrap();
        let _ = rx
            .try_recv()
            .expect("dispatch on second payload-bytes threshold");

        drop_schema(&store, &schema).await;
    }

    /// Hybrid materialiser threshold — whichever lands first dispatches.
    ///
    /// Regression for commit `895b7eca5`: with BOTH `count_limit` and
    /// `payload_bytes_limit` set, the trigger that fills first wins.
    /// Here the bytes budget is generous (10 deltas' worth) but the
    /// count is tight (3); count must win.
    #[tokio::test]
    async fn hybrid_threshold_count_wins_when_count_fills_first() {
        let (store, schema) = make_store("mat_hybrid_count_wins").await;

        let one_delta_bytes = match delta_non_init() {
            Value::WalRecord(rec) => rec.ser().expect("ser delta").len() as u32,
            _ => panic!("delta_non_init must be a WalRecord"),
        };
        // count=3 hits before bytes=10*one_delta.
        let (w, _t, mut rx, _in_flight) = writer_with_materializer_thresholds(
            store.clone(),
            FlushPolicy::default(),
            3,
            one_delta_bytes * 10,
            0,
        );

        w.put(&[(k(1), Lsn(10), delta_non_init())]).await.unwrap();
        w.put(&[(k(1), Lsn(20), delta_non_init())]).await.unwrap();
        w.flush().await.unwrap();
        assert!(rx.try_recv().is_err());
        w.put(&[(k(1), Lsn(30), delta_non_init())]).await.unwrap();
        w.flush().await.unwrap();
        let _ = rx.try_recv().expect("count threshold fires first");

        drop_schema(&store, &schema).await;
    }

    /// Write-side trigger respects the `in_flight` dedup: while the
    /// materialiser is still processing a key's job, additional
    /// threshold-crossings for that key MUST NOT enqueue duplicates,
    /// and the counter must stay at-or-above threshold so the first
    /// post-completion delta immediately re-fires (at the current
    /// LSN, not stale intermediate ones).
    ///
    /// Regression for commit `2f6544321` (in-flight dedup, no
    /// stale-LSN duplicates from write-side triggers). The read-side
    /// dedup has its own coverage in `try_dispatch_read_side`'s tests;
    /// this is the matching write-side coverage.
    #[tokio::test]
    async fn write_side_dedup_blocks_redispatch_while_in_flight() {
        let (store, schema) = make_store("mat_write_dedup").await;
        const LIMIT: u32 = 5;
        let (w, _t, mut rx, in_flight) =
            writer_with_materializer(store.clone(), FlushPolicy::default(), LIMIT);

        // First threshold: enqueue.
        for i in 0..LIMIT {
            w.put(&[(k(1), Lsn(10 + i as u64), delta_non_init())])
                .await
                .unwrap();
        }
        w.flush().await.unwrap();
        let first = rx.try_recv().expect("first threshold dispatches");
        assert_eq!(first.target_lsn, Lsn(10 + (LIMIT - 1) as u64));
        assert!(
            in_flight.lock().unwrap().contains(&first.key),
            "in_flight must hold the key while the job is outstanding"
        );

        // While the key is in_flight, later crossings must NOT enqueue
        // duplicates for it.
        for i in 0..(LIMIT * 3) {
            w.put(&[(k(1), Lsn(100 + i as u64), delta_non_init())])
                .await
                .unwrap();
        }
        w.flush().await.unwrap();
        assert!(
            rx.try_recv().is_err(),
            "no duplicate dispatch while key is in_flight"
        );

        // Materialiser completes — remove the in_flight reservation.
        // The retry re-fires at the key's newest LSN (counter still ≥
        // threshold from the dedup-blocked attempt).
        in_flight.lock().unwrap().remove(&first.key);
        w.put(&[(k(1), Lsn(500), delta_non_init())]).await.unwrap();
        w.retry_enqueue_rel_materialize_jobs();
        let second = rx.try_recv().expect("re-fires after in_flight release");
        assert_eq!(
            second.target_lsn,
            Lsn(500),
            "re-enqueue must target the newest LSN for this key"
        );

        drop_schema(&store, &schema).await;
    }

    /// A `will_init=true` delta is also a chain anchor and resets the
    /// counter (same semantics as an image — the row says "future reads
    /// can start the chain here").
    #[tokio::test]
    async fn will_init_delta_resets_chain_counter() {
        let (store, schema) = make_store("mat_winit_reset").await;
        const LIMIT: u32 = 5;
        let (w, _t, mut rx, _in_flight) =
            writer_with_materializer(store.clone(), FlushPolicy::default(), LIMIT);

        for i in 0..3 {
            w.put(&[(k(1), Lsn(10 + i), delta_non_init())])
                .await
                .unwrap();
        }
        w.put(&[(k(1), Lsn(50), delta_will_init())]).await.unwrap();
        for i in 0..(LIMIT - 1) {
            w.put(&[(k(1), Lsn(100 + i as u64), delta_non_init())])
                .await
                .unwrap();
        }
        w.flush().await.unwrap();
        assert!(
            rx.try_recv().is_err(),
            "will_init should have reset the counter"
        );
        w.put(&[(k(1), Lsn(200), delta_non_init())]).await.unwrap();
        w.flush().await.unwrap();
        let _ = rx.try_recv().expect("dispatch after will_init + LIMIT");

        drop_schema(&store, &schema).await;
    }

    /// Counters are per-key — interleaving doesn't cross-pollinate.
    #[tokio::test]
    async fn per_key_counters_are_independent() {
        let (store, schema) = make_store("mat_per_key").await;
        const LIMIT: u32 = 4;
        let (w, _t, mut rx, _in_flight) =
            writer_with_materializer(store.clone(), FlushPolicy::default(), LIMIT);

        // Alternate keys k(1) and k(2). After 4 deltas total each +
        // flush, the post-flush trigger should fire one job per key.
        for i in 0..LIMIT {
            w.put(&[(k(1), Lsn(10 + i as u64), delta_non_init())])
                .await
                .unwrap();
            w.put(&[(k(2), Lsn(100 + i as u64), delta_non_init())])
                .await
                .unwrap();
        }
        w.flush().await.unwrap();
        let mut seen_keys = std::collections::HashSet::new();
        for _ in 0..2 {
            let job = rx.try_recv().expect("two dispatches expected");
            seen_keys.insert(job.key);
        }
        assert_eq!(seen_keys.len(), 2, "one dispatch per key");
        assert!(rx.try_recv().is_err(), "no extra dispatches");

        drop_schema(&store, &schema).await;
    }

    /// `put()` enqueues a relation key's job at the crossing, without a
    /// flush. Later crossings while that job is in flight stay pending.
    #[tokio::test]
    async fn put_enqueues_at_the_crossing() {
        let (store, schema) = make_store("mat_put_enqueues").await;
        const LIMIT: u32 = 10;
        let (w, _t, mut rx, _in_flight) =
            writer_with_materializer(store.clone(), FlushPolicy::default(), LIMIT);

        for i in 0..(LIMIT * 10) as u64 {
            w.put(&[(k(1), Lsn(10 + i), delta_non_init())])
                .await
                .unwrap();
        }
        let job = rx.try_recv().expect("the crossing put enqueued");
        assert_eq!(job.target_lsn, Lsn(10 + LIMIT as u64 - 1));
        assert!(rx.try_recv().is_err(), "in_flight blocks later crossings");
        assert!(
            w.keys_to_materialize.contains(&k(1)),
            "a blocked crossing stays pending"
        );

        drop_schema(&store, &schema).await;
    }

    /// With the flusher running, a relation-page crossing is enqueued at
    /// the crossing delta, and the buffer is not flushed.
    #[tokio::test]
    async fn rel_crossing_enqueues_without_a_flush() {
        let (store, schema) = make_store("mat_rel_crossing").await;
        const LIMIT: u32 = 5;
        let policy = FlushPolicy {
            max_age: Duration::from_secs(3600),
            max_bytes: 1 << 30,
            ..FlushPolicy::default()
        };
        let (w, _t, mut rx, _in_flight) = writer_with_materializer(store.clone(), policy, LIMIT);
        let w = Arc::new(w);
        let task = w.spawn_flusher(Duration::from_secs(3600));
        for i in 0..LIMIT as u64 {
            w.put(&[(k(1), Lsn(10 + i), delta_non_init())])
                .await
                .unwrap();
        }
        let job = tokio::time::timeout(Duration::from_secs(10), rx.recv())
            .await
            .expect("crossing enqueued without a flush")
            .expect("materialiser channel open");
        assert_eq!(job.key, k(1));
        assert_eq!(job.target_lsn, Lsn(10 + LIMIT as u64 - 1));
        assert_eq!(
            w.stats().await.buffered_rows,
            LIMIT as usize,
            "a relation-page crossing must not flush"
        );
        task.shutdown().await;
        drop_schema(&store, &schema).await;
    }

    /// A full materialiser queue never blocks `put()`: crossings it has no
    /// room for stay pending and go out on a retry once a slot frees up.
    #[tokio::test]
    async fn rel_crossing_enqueue_never_blocks_on_a_full_queue() {
        let (store, schema) = make_store("mat_rel_crossing_full").await;
        const LIMIT: u32 = 2;
        let (w, _t, mut rx, in_flight) =
            writer_with_materializer(store.clone(), FlushPolicy::default(), LIMIT);
        let queue = rx.max_capacity();
        for key in 0..=queue as u32 {
            for i in 0..LIMIT as u64 {
                w.put(&[(k(key + 1), Lsn(10 + i), delta_non_init())])
                    .await
                    .unwrap();
            }
        }
        assert_eq!(rx.len(), queue, "enqueue fills the queue and returns");
        assert_eq!(
            w.keys_to_materialize.len(),
            1,
            "the crossing with no room stays pending"
        );
        let job = rx.recv().await.unwrap();
        in_flight.lock().unwrap().remove(&job.key);
        w.retry_enqueue_rel_materialize_jobs();
        assert!(
            w.keys_to_materialize.is_empty(),
            "enqueued once a slot freed up"
        );
        drop_schema(&store, &schema).await;
    }

    /// An SLRU crossing still forces a flush: its fold reads YB only, so
    /// the job can be enqueued only once the deltas are durable.
    #[tokio::test]
    async fn slru_crossing_forces_a_flush() {
        use pageserver_api::key::slru_block_to_key;
        use pageserver_api::reltag::SlruKind;
        let (store, schema) = make_store("mat_slru_crossing").await;
        const SLRU_LIMIT: u32 = 4;
        let policy = FlushPolicy {
            max_age: Duration::from_secs(3600),
            max_bytes: 1 << 30,
            ..FlushPolicy::default()
        };
        let (w, _t, mut rx, _in_flight) =
            writer_with_materializer_thresholds(store.clone(), policy, 0, 0, SLRU_LIMIT);
        let w = Arc::new(w);
        let task = w.spawn_flusher(Duration::from_secs(3600));
        let sk = slru_block_to_key(SlruKind::Clog, 0, 0);
        // Noted before the crossing put, so the forced flush can advance
        // the horizon the SLRU enqueue is bounded by.
        w.note_record_lsn(Lsn(10 + SLRU_LIMIT as u64), Lsn(10));
        for i in 0..SLRU_LIMIT as u64 {
            w.put(&[(sk, Lsn(10 + i), delta_non_init())]).await.unwrap();
        }
        let job = tokio::time::timeout(Duration::from_secs(10), rx.recv())
            .await
            .expect("SLRU crossing enqueued after a forced flush")
            .expect("materialiser channel open");
        assert_eq!(job.key, sk);
        assert_eq!(
            w.stats().await.buffered_slru_rows,
            0,
            "the crossing flushed"
        );
        task.shutdown().await;
        drop_schema(&store, &schema).await;
    }

    /// The materialiser's read folds unflushed rows, anchored on a
    /// flushed base.
    #[tokio::test]
    async fn read_chain_for_materialize_includes_unflushed_rows() {
        let (store, schema) = make_store("mat_read_unflushed").await;
        let (w, _t) = writer(store.clone(), FlushPolicy::default());
        w.put(&[(k(1), Lsn(10), image())]).await.unwrap();
        w.flush().await.unwrap();
        w.put(&[(k(1), Lsn(20), delta_non_init())]).await.unwrap();
        w.put(&[(k(1), Lsn(30), delta_non_init())]).await.unwrap();
        let chain = w.read_chain_for_materialize(k(1), Lsn(30)).await.unwrap();
        let lsns: Vec<Lsn> = chain.iter().map(|r| r.lsn).collect();
        assert_eq!(lsns, vec![Lsn(30), Lsn(20), Lsn(10)]);
        drop_schema(&store, &schema).await;
    }

    /// `flush_horizon_lsn` advances atomically inside
    /// `mark_flushed` to `max(snapshot.lsn)`. The post-flush and
    /// cold-residue enqueues bound their targets by it.
    #[tokio::test]
    async fn flush_horizon_advances_to_ingest_frontier() {
        let (store, schema) = make_store("mat_horizon").await;
        const LIMIT: u32 = 10;
        let (w, _t, _rx, _in_flight) =
            writer_with_materializer(store.clone(), FlushPolicy::default(), LIMIT);

        assert_eq!(
            w.flush_horizon_lsn
                .load(std::sync::atomic::Ordering::Relaxed),
            0,
            "horizon starts at zero before any flush",
        );

        // Push three rows at increasing LSNs across two keys, then
        // note the record at 30 as fully applied (walreceiver's
        // finish_write). After the flush, horizon = the noted
        // frontier, 30.
        w.put(&[(k(1), Lsn(10), delta_non_init())]).await.unwrap();
        w.put(&[(k(2), Lsn(20), delta_non_init())]).await.unwrap();
        w.put(&[(k(1), Lsn(30), delta_non_init())]).await.unwrap();
        w.note_record_lsn(Lsn(30), Lsn(20));
        w.flush().await.unwrap();
        assert_eq!(
            w.flush_horizon_lsn
                .load(std::sync::atomic::Ordering::Relaxed),
            30,
            "horizon advances to the noted ingest frontier after mark_flushed",
        );

        // A second flush with a later noted record monotonically
        // extends the horizon.
        w.put(&[(k(1), Lsn(40), delta_non_init())]).await.unwrap();
        w.note_record_lsn(Lsn(40), Lsn(30));
        w.flush().await.unwrap();
        assert_eq!(
            w.flush_horizon_lsn
                .load(std::sync::atomic::Ordering::Relaxed),
            40,
        );

        drop_schema(&store, &schema).await;
    }

    /// Bug-B regression (#22): rows at LSNs beyond the noted
    /// ingest frontier may belong to records whose sibling rows are
    /// still being put — the flush snapshot can capture them while
    /// missing a lower-LSN sibling. Neither `flush_horizon_lsn` nor
    /// the consistent_lsn may pass the frontier, even though such
    /// rows are in the flush we just wrote. Pre-fix, both watermarks
    /// advanced to max(snapshot.lsn) and a crash in that window lost
    /// the missed sibling permanently (replay started above it).
    #[tokio::test]
    async fn watermarks_capped_at_ingest_frontier() {
        let (store, schema) = make_store("frontier_cap").await;
        let (w, t) = writer(store.clone(), FlushPolicy::default());
        seed_branch_state_row(&store, t).await;

        let slot = Arc::new(utils::lsn::AtomicLsn::new(0));
        w.set_consistent_lsn_slot(slot.clone());

        // Record at 100 fully applied (noted); a row of the record at
        // 200 is in the buffer but the record is NOT noted — its
        // siblings may still be mid-put.
        w.put(&[(k(1), Lsn(100), image())]).await.unwrap();
        w.note_record_lsn(Lsn(100), Lsn(90));
        w.put(&[(k(2), Lsn(200), image())]).await.unwrap();
        w.flush().await.unwrap();

        assert_eq!(
            w.flush_horizon_lsn
                .load(std::sync::atomic::Ordering::Relaxed),
            100,
            "horizon must stop at the ingest frontier, not max(snapshot.lsn)",
        );
        assert_eq!(
            slot.load(),
            Lsn(100),
            "consistent_lsn must stop at the ingest frontier, not max(snapshot.lsn)",
        );

        // Once the record at 200 finishes applying, the next flush
        // moves the horizon up to it.
        w.note_record_lsn(Lsn(200), Lsn(100));
        w.put(&[(k(3), Lsn(210), image())]).await.unwrap();
        w.flush().await.unwrap();
        assert_eq!(
            w.flush_horizon_lsn
                .load(std::sync::atomic::Ordering::Relaxed),
            200,
        );

        drop_schema(&store, &schema).await;
    }

    /// A relation-page crossing enqueues its job at the crossing delta's LSN, and
    /// the deltas after it start the next tally.
    #[tokio::test]
    async fn crossing_enqueues_at_the_crossing_delta() {
        let (store, schema) = make_store("mat_target_lsn").await;
        const LIMIT: u32 = 3;
        let (w, _t, mut rx, _in_flight) =
            writer_with_materializer(store.clone(), FlushPolicy::default(), LIMIT);

        for lsn in [10u64, 20, 30, 40, 50] {
            w.put(&[(k(1), Lsn(lsn), delta_non_init())]).await.unwrap();
        }
        let job = rx.try_recv().expect("crossing enqueued");
        assert_eq!(job.target_lsn, Lsn(30));
        assert!(
            rx.try_recv().is_err(),
            "40 and 50 are below the next crossing"
        );

        drop_schema(&store, &schema).await;
    }

    /// Cold-key residue: an SLRU tally that crossed the threshold in a
    /// past flush must still be enqueued on a later flush with nothing to
    /// write, at `last_lsn` from the tally (≤ `flush_horizon_lsn` after the
    /// prior flush). Without this, `delta_counts` grows unbounded as the
    /// workload winds down (the 8.4M-entries pathology we observed at rest).
    #[tokio::test]
    async fn cold_key_residue_dispatches_at_last_lsn() {
        use pageserver_api::key::slru_block_to_key;
        use pageserver_api::reltag::SlruKind;
        let (store, schema) = make_store("mat_cold_residue").await;
        const LIMIT: u32 = 3;
        let (w, _t, mut rx, in_flight) =
            writer_with_materializer_thresholds(store.clone(), FlushPolicy::default(), 0, 0, LIMIT);

        // Block dispatch by pre-loading in_flight with this key; the
        // first flush will see the threshold but skip dispatch. The
        // tally stays in delta_counts.
        let key_bytes = slru_block_to_key(SlruKind::Clog, 0, 0);
        in_flight.lock().unwrap().insert(key_bytes);

        // Cross the threshold. After flush, last_lsn = 30 (max put
        // LSN) and that LSN is durable.
        for lsn in [10u64, 20, 30] {
            w.put(&[(key_bytes, Lsn(lsn), delta_non_init())])
                .await
                .unwrap();
        }
        w.note_record_lsn(Lsn(30), Lsn(20));
        w.flush().await.unwrap();
        assert!(
            rx.try_recv().is_err(),
            "in_flight block should suppress the first dispatch",
        );

        // Job completes. Clear in_flight as the materialiser would.
        in_flight.lock().unwrap().remove(&key_bytes);

        // Second flush: no new puts → snapshot is empty for k(1).
        // The cold-residue fallback dispatches at last_lsn=30.
        w.flush().await.unwrap();
        let job = rx
            .try_recv()
            .expect("cold-residue dispatch must fire on the second flush");
        assert_eq!(job.key, key_bytes);
        assert_eq!(
            job.target_lsn,
            Lsn(30),
            "cold-residue dispatch should use tally.last_lsn",
        );

        drop_schema(&store, &schema).await;
    }

    /// `image_chain_limit = 0` disables the counter entirely — no
    /// dispatches no matter how many deltas land.
    #[tokio::test]
    async fn limit_zero_disables_dispatch() {
        let (store, schema) = make_store("mat_disabled").await;
        let (w, _t, mut rx, _) = writer_with_materializer(store.clone(), FlushPolicy::default(), 0);

        for i in 0..50 {
            w.put(&[(k(1), Lsn(10 + i), delta_non_init())])
                .await
                .unwrap();
        }
        assert!(rx.try_recv().is_err(), "limit=0 must not dispatch");

        drop_schema(&store, &schema).await;
    }

    /// Helper: populate a `branch_state` row so `PageWriter::flush` has
    /// somewhere to land its `advance_consistent_lsn` UPDATE.
    async fn seed_branch_state_row(store: &PageStore, branch: BranchId) {
        // The controller provisions the row at branch create; tests stand in
        // for it before the pageserver fills in its own column group. That
        // provisioning is what sets `pg_version` — the row passed to
        // update_branch_initial_state carries it only to match, since that
        // call does not write it.
        store
            .provision_branch_row_for_test(branch, 180_002)
            .await
            .unwrap();
        store
            .update_branch_initial_state(&crate::page_store::BranchStateRow {
                branch_id: branch,
                consistent_lsn: Lsn(0),
                applied_gc_cutoff_lsn: Lsn(0),
                ancestor_branch_id: None,
                ancestor_lsn: None,
                pg_version: 180_002,
                initdb_lsn: None,
                prev_record_lsn: None,
            })
            .await
            .unwrap();
    }

    /// `consistent_lsn` slot must stay at its seed value through any
    /// number of `put()` calls. It only advances inside the flush
    /// success path, AFTER `try_write_rows` has landed the rows in YB
    /// AND the `advance_consistent_lsn` UPSERT has updated
    /// `branch_state`. Wiring a buffer-write to the slot would
    /// reintroduce the 2026-06 gap-on-crash bug: SK feedback /
    /// restart-resume would see an LSN whose rows aren't actually in
    /// `amp_storage.pages`.
    #[tokio::test]
    async fn put_alone_does_not_advance_consistent_lsn_slot() {
        let (store, schema) = make_store("clsn_slot_no_advance_on_put").await;
        let (w, t) = writer(store.clone(), FlushPolicy::default());
        seed_branch_state_row(&store, t).await;

        let slot = Arc::new(utils::lsn::AtomicLsn::new(0));
        w.set_consistent_lsn_slot(slot.clone());
        assert_eq!(slot.load(), Lsn(0), "slot starts at the constructor seed");

        // Three puts at ascending LSNs — exactly what walreceiver does
        // between flushes. The slot must NOT move; only the in-RAM
        // buffer is being touched.
        w.put(&[(k(1), Lsn(0x100), image())]).await.unwrap();
        w.put(&[(k(2), Lsn(0x200), image())]).await.unwrap();
        w.put(&[(k(3), Lsn(0x300), image())]).await.unwrap();
        w.note_record_lsn(Lsn(0x300), Lsn(0x200));
        assert_eq!(
            slot.load(),
            Lsn(0),
            "consistent_lsn slot must not advance on put (or note) alone"
        );

        // After a real flush, the slot reaches the max successfully
        // flushed LSN. That contract is exercised by
        // `flush_publishes_consistent_lsn`; the cross-check
        // here is just that put→flush ordering is the only path that
        // can move the slot.
        w.flush().await.unwrap();
        assert_eq!(
            slot.load(),
            Lsn(0x300),
            "slot advances exactly once: in the flush success path"
        );

        drop_schema(&store, &schema).await;
    }

    /// `PageWriter::flush` advances the `consistent_lsn`
    /// slot installed via `set_consistent_lsn_slot` whenever
    /// the `advance_consistent_lsn` UPDATE
    /// succeeds. Walreceiver feedback reads that slot to drive
    /// safekeeper WAL truncation — regression on the publish path
    /// would silently freeze WAL retention.
    #[tokio::test]
    async fn flush_publishes_consistent_lsn() {
        let (store, schema) = make_store("rclsn_publish").await;
        let (w, t) = writer(store.clone(), FlushPolicy::default());
        seed_branch_state_row(&store, t).await;

        let slot = Arc::new(utils::lsn::AtomicLsn::new(0));
        w.set_consistent_lsn_slot(slot.clone());

        // Slot is zero before any flush.
        assert_eq!(slot.load(), Lsn(0));

        // One image at Lsn(42), record noted as applied. After flush
        // the slot must reflect the noted frontier.
        w.put(&[(k(1), Lsn(42), image())]).await.unwrap();
        w.note_record_lsn(Lsn(42), Lsn(40));
        w.flush().await.unwrap();
        assert_eq!(
            slot.load(),
            Lsn(42),
            "slot must advance to the max flushed LSN"
        );

        // Multiple later rows in a single flush — slot reaches the max.
        let init = Value::WalRecord(NeonWalRecord::Postgres {
            will_init: true,
            rec: Bytes::from_static(b"init"),
        });
        w.put(&[
            (k(2), Lsn(50), init.clone()),
            (k(2), Lsn(60), delta()),
            (k(3), Lsn(75), init),
        ])
        .await
        .unwrap();
        w.note_record_lsn(Lsn(75), Lsn(60));
        // The throttle has `max_age` since the last successful upsert.
        // Default `max_age` is 5s; the first flush set the watermark,
        // and we're calling flush() again before the window elapses,
        // so the gated UPDATE is intentionally skipped → slot stays.
        w.flush().await.unwrap();
        assert_eq!(
            slot.load(),
            Lsn(42),
            "throttle prevents back-to-back UPDATE; slot retains prior value"
        );

        drop_schema(&store, &schema).await;
    }

    /// `note_record_lsn` records (last → prev) pairs; the next
    /// lookup at or below a given flushed lsn returns the matching
    /// prev and prunes the rest. Pure map mechanics.
    #[tokio::test]
    async fn note_record_lsn_basic_lookup_and_prune() {
        let (store, schema) = make_store("rl_hist_basic").await;
        let (w, _t) = writer(store.clone(), FlushPolicy::default());

        w.note_record_lsn(Lsn(0x100), Lsn(0x080));
        w.note_record_lsn(Lsn(0x200), Lsn(0x100));
        w.note_record_lsn(Lsn(0x300), Lsn(0x200));

        // Lookup at the exact `last` returns its prev.
        assert_eq!(
            w.take_prev_at_or_below(Lsn(0x200)),
            Some((Lsn(0x200), Lsn(0x100)))
        );
        // Older entries (≤ 0x200) are pruned; 0x100 is gone.
        assert_eq!(w.take_prev_at_or_below(Lsn(0x100)), None);
        // 0x300 survives.
        assert_eq!(
            w.take_prev_at_or_below(Lsn(0x300)),
            Some((Lsn(0x300), Lsn(0x200)))
        );
        // Map now empty.
        assert_eq!(w.take_prev_at_or_below(Lsn(0x400)), None);

        drop_schema(&store, &schema).await;
    }

    /// `take_prev_at_or_below(L)` picks the highest noted entry ≤ L,
    /// not the exact L. Models the race where pages flush at an lsn
    /// the walreceiver has noted but a later (uncommitted) record
    /// sits above.
    #[tokio::test]
    async fn note_record_lsn_picks_highest_at_or_below() {
        let (store, schema) = make_store("rl_hist_range").await;
        let (w, _t) = writer(store.clone(), FlushPolicy::default());

        w.note_record_lsn(Lsn(0x100), Lsn(0x080));
        w.note_record_lsn(Lsn(0x300), Lsn(0x100));

        // Query at 0x250 — no entry at exactly that lsn; pick the
        // highest noted ≤ 0x250, which is 0x100.
        assert_eq!(
            w.take_prev_at_or_below(Lsn(0x250)),
            Some((Lsn(0x100), Lsn(0x080)))
        );
        // 0x300 still there.
        assert_eq!(
            w.take_prev_at_or_below(Lsn(0x300)),
            Some((Lsn(0x300), Lsn(0x100)))
        );

        drop_schema(&store, &schema).await;
    }

    /// End-to-end: walreceiver-style flow notes record_lsn after a
    /// put + flush, the throttled `advance_consistent_lsn` runs,
    /// and the persisted `branch_state.prev_record_lsn` reflects
    /// the noted prev. Restart correctness depends on this column
    /// being non-NULL; the previous shape never wrote it.
    #[tokio::test]
    async fn flush_persists_prev_record_lsn_when_noted() {
        let (store, schema) = make_store("flush_persists_prev").await;
        let (w, t) = writer(store.clone(), FlushPolicy::default());
        seed_branch_state_row(&store, t).await;

        // Simulate `Timeline::finish_write(Lsn(42))` for the record
        // whose pages we're about to flush.
        w.note_record_lsn(Lsn(42), Lsn(40));

        w.put(&[(k(1), Lsn(42), image())]).await.unwrap();
        w.flush().await.unwrap();

        let row = store.get_branch_state(t).await.unwrap().unwrap();
        assert_eq!(row.consistent_lsn, Lsn(42));
        assert_eq!(
            row.prev_record_lsn,
            Some(Lsn(40)),
            "prev_record_lsn must be persisted alongside consistent_lsn"
        );

        drop_schema(&store, &schema).await;
    }

    /// A crash after a fold wrote an image from unflushed deltas leaves the
    /// image in YB above `consistent_lsn` and its deltas lost. Replay must
    /// rebuild every LSN below the image from the deltas, never the image.
    #[tokio::test]
    async fn replay_under_an_image_folded_from_lost_deltas() {
        let (store, schema) = make_store("replay_under_image").await;
        let (w, t) = writer(store.clone(), FlushPolicy::default());
        seed_branch_state_row(&store, t).await;
        let key = k(1);
        let shape = |chain: &[Arc<PageRow>]| -> Vec<(u64, bool)> {
            chain.iter().map(|r| (r.lsn.0, r.is_image)).collect()
        };

        w.put(&[
            (key, Lsn(100), image()),
            (key, Lsn(104), delta()),
            (key, Lsn(108), delta()),
        ])
        .await
        .unwrap();
        w.note_record_lsn(Lsn(108), Lsn(104));
        w.flush().await.unwrap();

        for lsn in [112, 114, 116, 118] {
            w.put(&[(key, Lsn(lsn), delta())]).await.unwrap();
        }
        let img = Arc::new(PageRow {
            key,
            lsn: Lsn(118),
            is_image: true,
            will_init: true,
            payload: Bytes::from_static(b"folded-image"),
        });
        store.put_pages(t, &[img]).await.unwrap();
        drop(w);

        let w = PageWriter::new(
            t,
            store.clone(),
            store.clone(),
            FlushPolicy::default(),
            64 * 1024 * 1024,
            0,
        );
        let row = store.get_branch_state(t).await.unwrap().unwrap();
        assert_eq!(row.consistent_lsn, Lsn(108));
        let persisted = store
            .read_chain(t, &key_to_bytes(key), Lsn(118))
            .await
            .unwrap();
        assert_eq!(shape(&persisted), [(118, true)]);

        let mut expected = vec![(108, false), (104, false), (100, true)];
        for lsn in [112u64, 114, 116] {
            w.put(&[(key, Lsn(lsn), delta())]).await.unwrap();
            expected.insert(0, (lsn, false));
            for read_at in [lsn, lsn + 1] {
                let chain = w.read_chain(key, Lsn(read_at)).await.unwrap();
                assert_eq!(shape(&chain), expected, "read at {read_at}");
            }
        }

        w.put(&[(key, Lsn(118), delta())]).await.unwrap();
        let chain = w.read_chain(key, Lsn(118)).await.unwrap();
        assert_eq!(shape(&chain), [(118, true)]);
        let chain = w.read_chain_for_materialize(key, Lsn(118)).await.unwrap();
        assert_eq!(
            shape(&chain),
            [(118, true)],
            "a re-enqueued job stops at the image"
        );

        w.put(&[(key, Lsn(120), delta())]).await.unwrap();
        let chain = w.read_chain(key, Lsn(120)).await.unwrap();
        assert_eq!(shape(&chain), [(120, false), (118, true)]);

        drop_schema(&store, &schema).await;
    }

    /// When `note_record_lsn` hasn't run at all (rows landed in the
    /// buffer but no record has finished applying), there is no
    /// durability frontier and the flush must not advance the
    /// checkpoint: rows in the snapshot may have same-record siblings
    /// that are still being put, and a checkpoint past them is how
    /// #22's bug B lost records across a crash.
    #[tokio::test]
    async fn flush_without_noted_record_does_not_advance_consistent_lsn() {
        let (store, schema) = make_store("flush_null_prev").await;
        let (w, t) = writer(store.clone(), FlushPolicy::default());
        seed_branch_state_row(&store, t).await;
        let seeded = store.get_branch_state(t).await.unwrap().unwrap();

        // Deliberately skip note_record_lsn.
        w.put(&[(k(1), Lsn(42), image())]).await.unwrap();
        w.flush().await.unwrap();

        let row = store.get_branch_state(t).await.unwrap().unwrap();
        assert_eq!(
            row.consistent_lsn, seeded.consistent_lsn,
            "no noted record => no frontier => checkpoint must not move"
        );
        assert_eq!(row.prev_record_lsn, seeded.prev_record_lsn);

        drop_schema(&store, &schema).await;
    }

    /// Once a non-NULL `prev_record_lsn` has been persisted, a
    /// subsequent throttled `advance_consistent_lsn` whose
    /// caller couldn't resolve a fresh pairing must NOT clobber the
    /// column with NULL. The SQL uses `COALESCE($5, prev_record_lsn)`
    /// to preserve the prior value in that case.
    #[tokio::test]
    async fn flush_with_no_pairing_preserves_persisted_prev() {
        let (store, schema) = make_store("flush_preserve_prev").await;
        let (_w, t) = writer(store.clone(), FlushPolicy::default());
        seed_branch_state_row(&store, t).await;

        // First advance with a real prev.
        store
            .advance_consistent_lsn(t, Lsn(0x200), Some(Lsn(0x180)))
            .await
            .unwrap();
        let row = store.get_branch_state(t).await.unwrap().unwrap();
        assert_eq!(row.prev_record_lsn, Some(Lsn(0x180)));

        // Second advance with no pairing (None). dc_lsn moves forward
        // but prev_record_lsn must retain its prior value.
        store
            .advance_consistent_lsn(t, Lsn(0x300), None)
            .await
            .unwrap();
        let row = store.get_branch_state(t).await.unwrap().unwrap();
        assert_eq!(row.consistent_lsn, Lsn(0x300));
        assert_eq!(
            row.prev_record_lsn,
            Some(Lsn(0x180)),
            "NULL-prev advance must not clobber the persisted value"
        );

        drop_schema(&store, &schema).await;
    }

    /// Documents the contract the `pgdatadir_mapping::put_rel_creation`
    /// idempotency check leans on: within a `policy.max_age` window
    /// of back-to-back flushes, `amp_storage.pages` rows are durable
    /// at LSN X while `amp_storage.branch_state.consistent_lsn`
    /// still reads X' < X. This lag is intentional — the throttle
    /// keeps a single hot table row from absorbing one UPSERT per
    /// flush — but it means a `kill -9` mid-window leaves the next
    /// PS lifetime resuming WAL replay from X'. Re-applying records
    /// whose effects are already in YB would panic walreceiver with
    /// `RelationAlreadyExists` on the first CREATE in (X', X];
    /// `put_rel_creation` returns early when the rel is already in
    /// the keyspace to keep replay safe over this window. See the
    /// integration `restart_wal_replay` after `restart_wal_replay_yb`
    /// in `run.sh all` for the end-to-end repro.
    #[tokio::test]
    async fn consistent_lsn_lag_under_throttle_is_expected() {
        use std::sync::atomic::Ordering::Relaxed;

        let (store, schema) = make_store("clsn_lag_expected").await;
        let (w, t) = writer(store.clone(), FlushPolicy::default());
        seed_branch_state_row(&store, t).await;

        // First flush: the first upsert always goes through.
        w.put(&[(k(1), Lsn(0x100), image())]).await.unwrap();
        w.note_record_lsn(Lsn(0x100), Lsn(0x80));
        w.flush().await.unwrap();
        let row = store.get_branch_state(t).await.unwrap().unwrap();
        assert_eq!(row.consistent_lsn, Lsn(0x100));

        // Second flush within `policy.max_age`: pages durable, the
        // branch_state UPDATE is throttled.
        w.put(&[(k(2), Lsn(0x200), image())]).await.unwrap();
        w.note_record_lsn(Lsn(0x200), Lsn(0x100));
        w.flush().await.unwrap();

        // Page row IS durable in `pages`.
        let chain = store
            .read_chain(t, &key_to_bytes(k(2)), Lsn(0x300))
            .await
            .unwrap();
        let max_page_lsn = chain.iter().map(|r| r.lsn).max().unwrap_or(Lsn(0));
        assert_eq!(
            max_page_lsn,
            Lsn(0x200),
            "page row at 0x200 is durable in amp_storage.pages"
        );

        // In-memory tracker knows the true LSN.
        let observed = w.consistent_lsn_observed.load(Relaxed);
        assert_eq!(Lsn(observed), Lsn(0x200));

        // Persisted column stays at the first flush's LSN — by
        // design. The replay window between this LSN and the actual
        // max durable LSN is what `put_rel_creation`'s idempotency
        // check protects against.
        let row = store.get_branch_state(t).await.unwrap().unwrap();
        assert_eq!(
            row.consistent_lsn,
            Lsn(0x100),
            "throttle keeps consistent_lsn at the prior upsert; \
             amp_storage.pages can be ahead within the window"
        );

        drop_schema(&store, &schema).await;
    }

    // TODO: end-to-end WAL GC test. Spin up safekeeper + pageserver,
    // ingest WAL, advance dc_lsn on PS, verify safekeeper deletes wals
    // rows ≤ that LSN. Needs a multi-binary harness; tracked as
    // follow-up to phase 5d.

    // Unit tests for the read-side dispatch decision. No YB, no
    // PageWriter — just the pure free function `try_dispatch_read_side`
    // exercised against a synthetic channel + HashSet. Sub-millisecond.
    mod read_side_dispatch {
        use super::*;
        use std::collections::HashSet;
        use std::sync::Mutex;

        fn setup() -> (
            Mutex<HashSet<Key>>,
            tokio::sync::mpsc::Sender<MaterializeJob>,
            tokio::sync::mpsc::Receiver<MaterializeJob>,
        ) {
            let (tx, rx) = tokio::sync::mpsc::channel::<MaterializeJob>(8);
            (Mutex::new(HashSet::new()), tx, rx)
        }

        fn kb() -> Key {
            k(1)
        }

        #[test]
        fn dispatches_when_chain_exceeds_count_limit() {
            let (in_flight, tx, mut rx) = setup();
            let dispatched = try_dispatch_read_side(kb(), Lsn(100), 21, 20, &in_flight, &tx);
            assert!(dispatched);
            let job = rx.try_recv().unwrap();
            assert_eq!(job.key, kb());
            assert_eq!(job.target_lsn, Lsn(100));
            assert!(in_flight.lock().unwrap().contains(&kb()));
        }

        #[test]
        fn skips_when_chain_at_or_below_limit() {
            let (in_flight, tx, mut rx) = setup();
            for chain_len in 0..=20 {
                assert!(!try_dispatch_read_side(
                    kb(),
                    Lsn(100),
                    chain_len,
                    20,
                    &in_flight,
                    &tx
                ));
            }
            assert!(rx.try_recv().is_err());
            assert!(in_flight.lock().unwrap().is_empty());
        }

        #[test]
        fn skips_when_count_limit_zero_disabled() {
            let (in_flight, tx, mut rx) = setup();
            assert!(!try_dispatch_read_side(
                kb(),
                Lsn(100),
                9999,
                0,
                &in_flight,
                &tx
            ));
            assert!(rx.try_recv().is_err());
            assert!(in_flight.lock().unwrap().is_empty());
        }

        #[test]
        fn skips_when_key_already_in_flight() {
            let (in_flight, tx, mut rx) = setup();
            in_flight.lock().unwrap().insert(kb());
            assert!(!try_dispatch_read_side(
                kb(),
                Lsn(100),
                50,
                20,
                &in_flight,
                &tx
            ));
            assert!(rx.try_recv().is_err());
            // Pre-existing in_flight entry must remain.
            assert!(in_flight.lock().unwrap().contains(&kb()));
        }

        #[test]
        fn rolls_back_in_flight_when_queue_full() {
            // Channel capacity 1, fill it, then try to dispatch.
            let (tx, mut rx) = tokio::sync::mpsc::channel::<MaterializeJob>(1);
            tx.try_send(MaterializeJob {
                key: k(99),
                target_lsn: Lsn(1),
            })
            .unwrap();
            let in_flight = Mutex::new(HashSet::new());
            // Dispatch should fail and NOT leave the key in in_flight.
            assert!(!try_dispatch_read_side(
                kb(),
                Lsn(100),
                50,
                20,
                &in_flight,
                &tx
            ));
            assert!(
                !in_flight.lock().unwrap().contains(&kb()),
                "in_flight must be rolled back so a later read can retry"
            );
            // The pre-filled job is still there; only it should drain.
            let pre = rx.try_recv().unwrap();
            assert_eq!(pre.key, k(99));
            assert!(rx.try_recv().is_err());
        }
    }

    // ── meta cache unit tests ────────────────────────────────────────────────

    /// After `put()` the cache is populated; after `flush()` the
    /// `meta_buffer` is drained but the cache retains the entry.
    #[tokio::test]
    async fn meta_cache_populated_by_put_and_survives_flush() {
        let (store, schema) = make_store("mc_populated").await;
        let (w, _t) = writer_with_cache(store.clone(), FlushPolicy::default(), 16);

        assert_eq!(w.meta_cache_len(), Some(0));
        w.put(&[(mk(0), Lsn(10), image())]).await.unwrap();
        assert_eq!(w.meta_cache_len(), Some(1), "put should populate cache");

        w.flush().await.unwrap();
        assert_eq!(
            w.stats().await.buffered_meta_rows,
            0,
            "flush must drain meta_buffer"
        );
        assert_eq!(w.meta_cache_len(), Some(1), "cache survives flush");
        drop_schema(&store, &schema).await;
    }

    /// A scan immediately after `flush()` (meta_buffer empty) is served
    /// from the in-process cache, not from YB.
    #[tokio::test]
    async fn meta_cache_serves_after_buffer_drain() {
        let (store, schema) = make_store("mc_after_drain").await;
        let (w, _t) = writer_with_cache(store.clone(), FlushPolicy::default(), 16);
        let payload = Bytes::from_static(b"cache-served-payload");

        w.put(&[(mk(1), Lsn(10), Value::Image(payload.clone()))])
            .await
            .unwrap();
        w.flush().await.unwrap();
        assert_eq!(w.stats().await.buffered_meta_rows, 0);

        let hits_before = w.stats().await.meta_cache_hits;
        let result = w
            .scan_meta_range(mk(1)..mk(1).next(), Lsn(100))
            .await
            .unwrap();
        assert_eq!(
            w.stats().await.meta_cache_hits,
            hits_before + 1,
            "should be a cache hit"
        );
        let row = result.into_values().next().expect("row present");
        assert_eq!(row.payload, payload);
        drop_schema(&store, &schema).await;
    }

    /// A second write for the same key at a higher LSN replaces the cache
    /// entry; a subsequent scan returns the newer payload.
    #[tokio::test]
    async fn meta_cache_newer_write_supersedes() {
        let (store, schema) = make_store("mc_newer_wins").await;
        let (w, _t) = writer_with_cache(store.clone(), FlushPolicy::default(), 16);

        w.put(&[(mk(2), Lsn(10), Value::Image(Bytes::from_static(b"old")))])
            .await
            .unwrap();
        w.put(&[(mk(2), Lsn(20), Value::Image(Bytes::from_static(b"new")))])
            .await
            .unwrap();
        assert_eq!(w.meta_cache_len(), Some(1), "same key → single entry");

        w.flush().await.unwrap();
        let result = w
            .scan_meta_range(mk(2)..mk(2).next(), Lsn(100))
            .await
            .unwrap();
        let row = result.into_values().next().expect("row present");
        assert_eq!(row.payload, Bytes::from_static(b"new"), "latest write wins");
        drop_schema(&store, &schema).await;
    }

    /// Requesting a `target_lsn` that is below the cached entry's LSN
    /// must skip the cache and fall through to YB (which may have an
    /// older row or return nothing).
    #[tokio::test]
    async fn meta_cache_skips_for_older_target_lsn() {
        let (store, schema) = make_store("mc_old_lsn").await;
        let (w, _t) = writer_with_cache(store.clone(), FlushPolicy::default(), 16);

        w.put(&[(mk(3), Lsn(100), Value::Image(Bytes::from_static(b"v100")))])
            .await
            .unwrap();
        w.flush().await.unwrap();

        let hits_before = w.stats().await.meta_cache_hits;
        // target_lsn=50 is before the cached row at LSN=100 — must miss.
        let result = w
            .scan_meta_range(mk(3)..mk(3).next(), Lsn(50))
            .await
            .unwrap();
        assert_eq!(
            w.stats().await.meta_cache_hits,
            hits_before,
            "cache must not be used when cached.lsn > target_lsn"
        );
        assert!(result.is_empty(), "no row at LSN ≤ 50");
        drop_schema(&store, &schema).await;
    }

    /// When the cache is at capacity and a new key evicts an existing one,
    /// a read for the evicted key falls through to YB and still returns the
    /// correct value.
    #[tokio::test]
    async fn meta_cache_eviction_falls_back_to_yb() {
        let (store, schema) = make_store("mc_evict").await;
        // Capacity 1 → writing a second key evicts the first.
        let (w, _t) = writer_with_cache(store.clone(), FlushPolicy::default(), 1);

        w.put(&[(mk(4), Lsn(10), Value::Image(Bytes::from_static(b"key4")))])
            .await
            .unwrap();
        assert_eq!(w.meta_cache_len(), Some(1));

        // Write a second key — evicts mk(4) from the size-1 cache.
        w.put(&[(mk(5), Lsn(20), Value::Image(Bytes::from_static(b"key5")))])
            .await
            .unwrap();
        assert_eq!(w.meta_cache_len(), Some(1));

        w.flush().await.unwrap();

        // mk(4) was evicted — read must go to YB.
        let hits_before = w.stats().await.meta_cache_hits;
        let r4 = w
            .scan_meta_range(mk(4)..mk(4).next(), Lsn(100))
            .await
            .unwrap();
        assert_eq!(
            w.stats().await.meta_cache_hits,
            hits_before,
            "mk(4) evicted — no cache hit"
        );
        assert_eq!(
            r4.into_values().next().map(|r| r.payload),
            Some(Bytes::from_static(b"key4")),
            "YB must serve the evicted key"
        );

        // mk(5) is still cached.
        let hits_before = w.stats().await.meta_cache_hits;
        let r5 = w
            .scan_meta_range(mk(5)..mk(5).next(), Lsn(100))
            .await
            .unwrap();
        assert_eq!(
            w.stats().await.meta_cache_hits,
            hits_before + 1,
            "mk(5) must hit cache"
        );
        assert_eq!(
            r5.into_values().next().map(|r| r.payload),
            Some(Bytes::from_static(b"key5"))
        );
        drop_schema(&store, &schema).await;
    }

    /// With `meta_cache_entries = 0` (disabled), writes and reads still
    /// produce the correct result through YB.
    #[tokio::test]
    async fn meta_cache_disabled_is_transparent() {
        let (store, schema) = make_store("mc_disabled").await;
        // `writer()` uses 0 entries — cache disabled.
        let (w, _t) = writer(store.clone(), FlushPolicy::default());

        assert_eq!(w.meta_cache_len(), None, "cache must be None when disabled");
        w.put(&[(
            mk(6),
            Lsn(10),
            Value::Image(Bytes::from_static(b"no-cache")),
        )])
        .await
        .unwrap();
        w.flush().await.unwrap();

        let result = w
            .scan_meta_range(mk(6)..mk(6).next(), Lsn(100))
            .await
            .unwrap();
        assert_eq!(w.stats().await.meta_cache_hits, 0, "no hits when disabled");
        let row = result.into_values().next().expect("row from YB");
        assert_eq!(row.payload, Bytes::from_static(b"no-cache"));
        drop_schema(&store, &schema).await;
    }

    // ---- parallel apply (ingest_parallelism) ----------------------

    /// Build a `(CompactKey, Lsn, serialized_size, Value)` entry with the
    /// correct serialized size for `SerializedValueBatch::from_values`.
    fn ser_entry(
        key: Key,
        lsn: Lsn,
        val: Value,
    ) -> (pageserver_api::key::CompactKey, Lsn, usize, Value) {
        let size = val.serialized_size().expect("serialized_size") as usize;
        (key.to_compact(), lsn, size, val)
    }

    /// A distinct image payload per tag, so per-key comparisons are meaningful.
    fn img(tag: &str) -> Value {
        Value::Image(Bytes::from(tag.as_bytes().to_vec()))
    }

    /// Read a key's chain (newest-first) from the in-RAM buffer, reduced to
    /// its comparable fields. The batches below give every key an image
    /// anchor, so the chain is complete in the buffer (no YB read needed).
    async fn chain_fields(w: &PageWriter, key: Key) -> Vec<(Lsn, bool, bool, Bytes)> {
        w.read_chain(key, Lsn(u64::MAX))
            .await
            .unwrap()
            .into_iter()
            .map(|r| (r.lsn, r.is_image, r.will_init, r.payload.clone()))
            .collect()
    }

    /// Parallel apply (`ingest_parallelism > 1`) must produce byte-identical
    /// per-key chains to the serial path: partition-by-key + concurrent push
    /// must not drop, duplicate, reorder, or cross-contaminate any version.
    #[tokio::test]
    async fn put_serialized_parallel_matches_serial() {
        let (store, schema) = make_store("ppush_parity").await;

        // Enough keys that the parallel path actually fans across workers.
        // Each key: an image anchor + two deltas (in ascending LSN, as the
        // batch validator requires per key).
        let n_keys = 400u32;
        let build = || {
            let mut entries = Vec::new();
            for kk in 0..n_keys {
                entries.push(ser_entry(k(kk), Lsn(10), img(&format!("img-{kk}"))));
                entries.push(ser_entry(k(kk), Lsn(20), delta()));
                entries.push(ser_entry(k(kk), Lsn(30), delta()));
            }
            SerializedValueBatch::from_values(entries)
        };

        let serial_policy = FlushPolicy {
            ingest_parallelism: 1,
            ..FlushPolicy::default()
        };
        let (ws, _ts) = writer(store.clone(), serial_policy);
        let ws = Arc::new(ws);
        ws.put_serialized(build()).await.unwrap();

        let parallel_policy = FlushPolicy {
            ingest_parallelism: 4,
            ingest_parallelism_min_batch: 1, // force the parallel path
            ..FlushPolicy::default()
        };
        let (wp, _tp) = writer(store.clone(), parallel_policy);
        let wp = Arc::new(wp);
        wp.put_serialized(build()).await.unwrap();

        for kk in 0..n_keys {
            let s = chain_fields(&ws, k(kk)).await;
            let p = chain_fields(&wp, k(kk)).await;
            assert_eq!(s.len(), 3, "key {kk}: expected image + 2 deltas serially");
            assert_eq!(p, s, "key {kk}: parallel chain differs from serial");
        }

        drop_schema(&store, &schema).await;
    }

    /// Many versions of ONE key in a parallel batch must stay in correct LSN
    /// order — the by-key partition guarantees a key is owned by a single
    /// worker, so its chain is never interleaved/reordered.
    #[tokio::test]
    async fn put_serialized_parallel_preserves_per_key_lsn_order() {
        let (store, schema) = make_store("ppush_order").await;

        let hot = k(999_999);
        let build = || {
            let mut entries = Vec::new();
            // Padding keys so the batch crosses into the parallel path.
            for kk in 0..400u32 {
                entries.push(ser_entry(k(kk), Lsn(10), img(&format!("o-{kk}"))));
            }
            // The hot key: image anchor + 20 deltas at increasing LSNs.
            entries.push(ser_entry(hot, Lsn(100), img("hot-base")));
            for i in 1..=20u64 {
                entries.push(ser_entry(hot, Lsn(100 + i * 10), delta()));
            }
            SerializedValueBatch::from_values(entries)
        };

        let parallel_policy = FlushPolicy {
            ingest_parallelism: 4,
            ingest_parallelism_min_batch: 1,
            ..FlushPolicy::default()
        };
        let (wp, _tp) = writer(store.clone(), parallel_policy);
        let wp = Arc::new(wp);
        wp.put_serialized(build()).await.unwrap();

        let chain = chain_fields(&wp, hot).await;
        assert_eq!(chain.len(), 21, "expected image + 20 deltas");
        let lsns: Vec<u64> = chain.iter().map(|(l, _, _, _)| l.0).collect();
        let mut descending = lsns.clone();
        descending.sort_unstable_by(|a, b| b.cmp(a));
        assert_eq!(lsns, descending, "chain not in descending LSN order");
        assert_eq!(
            *lsns.last().unwrap(),
            100,
            "oldest entry must be the anchor LSN"
        );
        assert!(
            chain.last().unwrap().1,
            "oldest entry must be the image anchor"
        );

        drop_schema(&store, &schema).await;
    }

    /// GC candidate collection at the ingest hook. Scenarios:
    ///
    /// 1. Collection disabled (no `set_gc_candidate_every_n`): base writes
    ///    produce no `pending_gc` rows.
    /// 2. n = 1: every base — image or plain `Value::Image` — buffers
    ///    one candidate; plain deltas buffer none. Candidates go to the
    ///    shared emitter, NOT the window flush: nothing is in YB
    ///    even after `flush()`, only after the emitter's own flush,
    ///    and same-block candidates fold to the latest due_lsn.
    /// 3. n = 3: only every third base OF A GIVEN BLOCK emits, and the
    ///    per-block counters are independent — an interleaved block
    ///    that only ever reaches 2 bases emits nothing (the doc's
    ///    bounded-leak trade for sampling).
    #[tokio::test]
    async fn gc_candidates_every_n_bases() {
        let all_pending = |store: Arc<PageStore>, branch: BranchId| async move {
            let mut rows = store
                .gc_select_due_candidates(branch, Lsn(0), Lsn(i64::MAX as u64), 100_000)
                .await
                .unwrap()
                .rows;
            rows.sort_by_key(|r| (r.key.field6, r.due_lsn));
            rows.iter()
                .map(|r| (r.key.field6 as i64, r.due_lsn.0))
                .collect::<Vec<_>>()
        };
        let cutoff = || Arc::new(Rcu::new(Lsn(0)));

        let (store, schema) = make_store("gc_cand").await;

        // 1. Disabled: bases leave no candidates behind.
        let (w, branch) = writer(store.clone(), FlushPolicy::default());
        w.put(&[(k(1), Lsn(10), image())]).await.unwrap();
        w.flush().await.unwrap();
        assert!(
            all_pending(store.clone(), branch).await.is_empty(),
            "disabled collection must insert nothing"
        );

        // 2. n = 1: every base, only bases; decoupled from the window
        // flush; same-block candidates fold to the latest due_lsn.
        let (mut w, branch) = writer(store.clone(), FlushPolicy::default());
        w.set_gc_candidate_every_n(1, cutoff());
        let w = Arc::new(w);
        w.put(&[
            (k(1), Lsn(10), image()),
            (k(1), Lsn(20), delta()), // plain delta: no candidate
            (k(1), Lsn(30), image()),
            (k(2), Lsn(15), image()),
        ])
        .await
        .unwrap();
        w.flush().await.unwrap();
        assert!(
            all_pending(store.clone(), branch).await.is_empty(),
            "the window flush must not carry trim candidates"
        );
        w.flush_gc_trim_candidates().await;
        assert_eq!(
            all_pending(store.clone(), branch).await,
            vec![(1, 30), (2, 15)],
            "one candidate per base, none for the delta; block 1's two \
             candidates fold to the latest due_lsn"
        );

        // 3. n = 3: per-block counting; the 2-base block never emits.
        let (mut w, branch) = writer(store.clone(), FlushPolicy::default());
        w.set_gc_candidate_every_n(3, cutoff());
        let w = Arc::new(w);
        let mut entries: Vec<(Key, Lsn, Value)> = Vec::new();
        // block 5: 7 bases at lsns 10..=70 → emits at the 3rd and 6th,
        // folded to the 6th's due_lsn in the buffer.
        for i in 1..=7u64 {
            entries.push((k(5), Lsn(i * 10), image()));
        }
        // block 6 interleaved: 2 bases → below n, never emits.
        entries.push((k(6), Lsn(15), image()));
        entries.push((k(6), Lsn(25), image()));
        w.put(&entries).await.unwrap();
        w.flush().await.unwrap();
        w.flush_gc_trim_candidates().await;
        assert_eq!(
            all_pending(store.clone(), branch).await,
            vec![(5, 60)],
            "every 3rd base per block, folded to the latest; independent \
             per-block counters"
        );

        // 4. Out-of-order counting: a materialiser image at its fold
        // point (lsn 10) is the n-th base after an ingest base at lsn
        // 100 was counted. The candidate must be stamped at the counted
        // MAX (100) — stamping the n-th base's own lsn would spend the
        // candidate before the garbage under the lsn-100 base is
        // collectable.
        let (mut w, branch) = writer(store.clone(), FlushPolicy::default());
        w.set_gc_candidate_every_n(2, cutoff());
        let rows = [
            build_page_row(k(9), Lsn(100), &image()).unwrap(),
            build_page_row(k(9), Lsn(10), &image()).unwrap(),
        ];
        w.record_gc_trim_bases(rows.iter(), crate::metrics::GcEmitPath::Ingest);
        w.flush_gc_trim_candidates().await;
        assert_eq!(
            all_pending(store.clone(), branch).await,
            vec![(9, 100)],
            "an n-th base with a lower lsn stamps the counted max"
        );

        drop_schema(&store, &schema).await;
    }
}
