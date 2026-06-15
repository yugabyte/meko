//! `PageWriter` — buffered write path between `walingest` and `PageStore`.
//!
//! Pure passthrough: every relation-page entry from walingest becomes
//! one row in `pageserver.pages`, untouched. WAL records flow in as
//! `is_image=FALSE`; FPI records flow in as `is_image=TRUE`. The
//! `will_init` flag comes straight off the `Value` — we don't synthesise
//! anchors at write time.
//!
//! Chain length is not a write-time invariant any more — a background
//! image materialiser (separate module) periodically scans
//! `pageserver.pages` for keys whose delta chain has grown long and
//! drops a fresh image row at the latest LSN. Reads stay correct without
//! it (`PageStore::read_chain` walks back to the latest will_init delta
//! when no image exists); the background task is pure optimisation that
//! keeps reads fast.
//!
//! Buffering: rows accumulate in RAM until a flush trigger (age or
//! bytes) fires, then drain to `pageserver.pages` in one INSERT.

use std::collections::HashMap;
use std::sync::Arc;
use std::sync::atomic::{AtomicU64, Ordering};
use std::time::{Duration, Instant};

use hashlink::LruCache;

use anyhow::Result;
use bytes::Bytes;
use dashmap::{DashMap, DashSet};
use pageserver_api::key::{KEY_SIZE, Key};
use parking_lot::Mutex as PlMutex;
use tokio_util::sync::CancellationToken;
use tracing::info;
use utils::bin_ser::BeSer;
use utils::id::{TenantId, TimelineId};
use utils::lsn::Lsn;
use wal_decoder::models::record::NeonWalRecord;
use wal_decoder::models::value::Value;
use wal_decoder::serialized_batch::{SerializedValueBatch, ValueMeta};

use pageserver_api::reltag::SlruKind;

use crate::page_store::{
    LineageSegment, MetaRow, PageRow, PageStore, SlruPageRow, dedup_page_chain_by_lsn,
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
}

impl Default for FlushPolicy {
    fn default() -> Self {
        Self {
            max_age: Duration::from_secs(5),
            max_bytes: 25 * 1024 * 1024,
            backpressure_bytes: 50 * 1024 * 1024,
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
    /// Always durable by construction. The post-flush trigger uses
    /// `max(snapshot.lsn for key)` (every snapshot row is in YB AND
    /// marked durable in cache by the time the trigger fires); the
    /// read-side trigger uses the read's `target_lsn` which is the
    /// caller's responsibility to bound at a durable LSN.
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
/// - `fpu_page_store` serves walreceiver writes (`flush*`) and
///   the image materialiser's cache-miss durable read
///   (`read_chain_durable`). Materialiser image writes also go via
///   this connection (held directly by `ImageMaterializer`).
///
/// Compute getPage stays isolated from internal load — a long flush
/// or a materialiser walredo-and-INSERT can't queue behind a compute
/// read and vice versa.
pub struct PageWriter {
    tenant_id: TenantId,
    timeline_id: TimelineId,
    /// Ordered ancestry segments, nearest-first (`lineage[0]` is this
    /// timeline at `Lsn::MAX`). All PageStore reads go through the
    /// lineage-aware variants so a branch serves pre-branch-point
    /// data from the ancestor's rows. Writes always target
    /// `timeline_id` — never an ancestor. Set once before the writer
    /// is shared (see [`Self::set_lineage`]); defaults to the trivial
    /// single-segment lineage.
    lineage: Vec<LineageSegment>,
    /// Storage-controller-assigned generation for this attach. Carried
    /// through to `advance_consistent_lsn` so the persisting UPDATE
    /// can fail-fast if a higher generation has taken over.
    generation: utils::generation::Generation,
    compute_page_store: Arc<PageStore>,
    fpu_page_store: Arc<PageStore>,
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
    /// the entry. Shared with the flush path (post-flush trigger),
    /// which walks the map looking for keys that crossed the
    /// threshold during the flushed-window.
    delta_counts: DashMap<Key, DeltaTally>,
    /// Keys whose `delta_counts` tally has *crossed* a materialize
    /// threshold and are awaiting dispatch — the put loop inserts a key
    /// here on the transition into "hit", so the flush trigger walks
    /// only this set instead of sweeping all of `delta_counts`. Without
    /// it, every flush iterated the whole map (O(all cached keys)) to
    /// find the few threshold-crossers — the write-path twin of the
    /// `lookup_range` O(N) cache walk (#21). Treated as an
    /// over-approximation: the flush walk re-checks the tally and drops
    /// stale entries, so a spurious member is harmless.
    delta_ready: DashSet<Key>,
    materializer: Option<MaterializerHandle>,
    /// Highest LSN successfully reported as durable to
    /// `pageserver.timeline_state.consistent_lsn`. Advances only
    /// after `try_write_rows` succeeds AND the subsequent
    /// `advance_consistent_lsn` succeeds. Initialised to 0 at
    /// construction; attach seeds it from the persisted row.
    consistent_lsn_watermark: std::sync::atomic::AtomicU64,
    /// Highest LSN actually durabilised in `pageserver.pages` /
    /// `slru_pages` / `meta`. Advances on every successful flush,
    /// even when the throttle skips the YB UPDATE on
    /// `disk_consistent_lsn`. Used by `force_upsert_consistent_lsn_if_advanced`
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
    /// `Arc<AtomicLsn>` of the timeline's `visible_remote_consistent_lsn`
    /// — the value the walreceiver loop forwards to the safekeeper as
    /// `remote_consistent_lsn` in `PageserverFeedback`, which drives
    /// safekeeper WAL truncation. Bumped on every successful
    /// generation-gated `advance_consistent_lsn`. Set lazily
    /// after `init_upload_queue` runs (PageWriter is constructed
    /// before the upload queue is initialized), via
    /// [`Self::set_consistent_lsn_slot`]. If unset at flush
    /// time (smoke tests, init-race), the publish is a no-op; the
    /// next flush retries.
    consistent_lsn_slot: std::sync::OnceLock<Arc<utils::lsn::AtomicLsn>>,
    /// Pulsed by `notify_waiters()` at the end of every successful
    /// `flush()`. `put()` blocks on this when the buffer exceeds
    /// `policy.backpressure_bytes`.
    flush_completed: tokio::sync::Notify,
    /// Total puts that had to wait at least once on `flush_completed`
    /// because the buffer was at `policy.backpressure_bytes`. Exposed
    /// for telemetry / smoke tests.
    backpressure_waits: std::sync::atomic::AtomicU64,
    /// Upper bound on rel-page LSNs the materialiser is allowed to
    /// consult in YB. Advanced only when `mark_flushed` runs
    /// (after every chunk of a flush has committed). Materialiser
    /// reads cap their `target_lsn` at this value so a mid-flush
    /// partial-YB-view can never feed walredo a chain with a hole
    /// — see `read_chain_durable` for the use site.
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
    /// (Rel rows are immune: they drain by entry identity via
    /// `mark_flushed`, not by count.) Holding this across the entire
    /// flush makes snapshot+drain atomic with respect to other
    /// flushes. `put()` never takes it, so ingest is not serialized
    /// against flush.
    flush_serialize: tokio::sync::Mutex<()>,
    /// In-process LRU cache for `pageserver.meta` point reads, keyed by
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
}

/// Non-rel flush buffers, kept under a small `parking_lot::Mutex`.
/// Put/flush are the only producers; meta/SLRU reads briefly snapshot
/// here too. Holds are O(rows-in-this-flush), which is tiny.
struct AuxBuffers {
    slru_buffer: Vec<SlruPageRow>,
    meta_buffer: Vec<MetaRow>,
    /// Bytes pending durability for SLRU + meta only. Rel-page
    /// unflushed bytes live on `pages.unflushed_bytes()`. Drives the
    /// non-rel side of the size-based flush trigger.
    bytes_in_aux_buffers: usize,
    /// Wall-clock of the first row appended after the most-recent
    /// flush. Drives the age-based flush trigger; reset to `None`
    /// when the buffer is empty.
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
    pub fn new(
        tenant_id: TenantId,
        timeline_id: TimelineId,
        generation: utils::generation::Generation,
        compute_page_store: Arc<PageStore>,
        fpu_page_store: Arc<PageStore>,
        policy: FlushPolicy,
        read_cache_bytes: usize,
        meta_cache_entries: usize,
    ) -> Self {
        Self {
            tenant_id,
            timeline_id,
            lineage: LineageSegment::single(timeline_id),
            generation,
            compute_page_store,
            fpu_page_store,
            policy,
            pages: crate::unified_pages::UnifiedPages::new(read_cache_bytes),
            aux: PlMutex::new(AuxBuffers {
                slru_buffer: Vec::new(),
                meta_buffer: Vec::new(),
                bytes_in_aux_buffers: 0,
                opened_at: None,
            }),
            rows_written: AtomicU64::new(0),
            meta_cache_hits: AtomicU64::new(0),
            delta_counts: DashMap::new(),
            delta_ready: DashSet::new(),
            materializer: None,
            consistent_lsn_watermark: std::sync::atomic::AtomicU64::new(0),
            consistent_lsn_observed: std::sync::atomic::AtomicU64::new(0),
            last_consistent_lsn_upsert_at: std::sync::Mutex::new(None),
            record_lsn_history: std::sync::Mutex::new(std::collections::BTreeMap::new()),
            consistent_lsn_slot: std::sync::OnceLock::new(),
            flush_completed: tokio::sync::Notify::new(),
            backpressure_waits: std::sync::atomic::AtomicU64::new(0),
            flush_horizon_lsn: std::sync::atomic::AtomicU64::new(0),
            flush_serialize: tokio::sync::Mutex::new(()),
            meta_cache: (meta_cache_entries > 0)
                .then(|| PlMutex::new(LruCache::new(meta_cache_entries))),
        }
    }

    /// Seed the in-memory `disk_consistent_lsn` watermark from the
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
    /// callers that need to issue side-effects (timeline-row upsert,
    /// generation bump) on the same connection the flush path uses.
    pub fn fpu_page_store(&self) -> &Arc<PageStore> {
        &self.fpu_page_store
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
        match self
            .fpu_page_store
            .advance_consistent_lsn(
                self.tenant_id,
                self.timeline_id,
                self.generation,
                lsn,
                prev,
            )
            .await?
        {
            crate::page_store::AdvanceConsistentLsnOutcome::Updated => {
                self.consistent_lsn_watermark.store(observed, Relaxed);
                *self.last_consistent_lsn_upsert_at.lock().unwrap() = Some(Instant::now());
                self.publish_consistent_lsn(lsn);
            }
            crate::page_store::AdvanceConsistentLsnOutcome::StaleGeneration { live_generation } => {
                // Shutdown path: another PS owns the timeline. Don't
                // trigger a global shutdown here (we're already in
                // one), but surface the fact so the operator can see
                // why the shutdown-time catch-up failed.
                tracing::warn!(
                    tenant_id = %self.tenant_id,
                    timeline_id = %self.timeline_id,
                    our_generation = ?self.generation,
                    live_generation,
                    "force_upsert_consistent_lsn_if_advanced fenced by newer generation — \
                     leaving dc_lsn at previous watermark"
                );
            }
        }
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
    /// (`pageserver.timeline_state.consistent_lsn`) so the
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
            lineage.first().map(|s| s.timeline_id) == Some(self.timeline_id),
            "lineage[0] must be the writer's own timeline ({}), got {:?}",
            self.timeline_id,
            lineage.first(),
        );
        self.lineage = lineage;
    }

    pub fn set_materializer(&mut self, handle: MaterializerHandle) {
        self.materializer = Some(handle);
    }

    pub fn tenant_id(&self) -> TenantId {
        self.tenant_id
    }

    pub fn timeline_id(&self) -> TimelineId {
        self.timeline_id
    }

    /// Append a batch of entries to the in-RAM buffer. Does not flush.
    /// Routes every entry into one of the three row-store tables:
    /// - relation-page keys → `pageserver.pages`
    /// - SLRU block keys   → `pageserver.slru_pages`
    /// - everything else   → `pageserver.meta` (full-overwrite)
    ///
    /// Meta entries must arrive as `Value::Image` — they're
    /// full-overwrite by convention. A `Value::WalRecord` here would
    /// signal an unexpected walredo dependency on a non-page key and is
    /// an error.
    pub async fn put(&self, entries: &[(Key, Lsn, Value)]) -> Result<()> {
        if entries.is_empty() {
            return Ok(());
        }

        // Backpressure: if the buffer is already at or above the
        // configured hard cap, block until a flush drains it below
        // the cap. The walreceiver is the only producer that hits
        // this path, so stalling it here pushes the slowdown all the
        // way up to safekeeper ack — which is exactly what we want
        // when YB INSERTs can't keep up. Without this, the buffer
        // grows unboundedly under CPU pressure (pre-flush rows are
        // pinned in `UnifiedPages` and `evict_to_capacity` can't
        // touch them).
        //
        // `notified()` is registered before the size check (via
        // `enable()`) to close the race where a flush completes
        // between our check and our wait.
        if self.policy.backpressure_bytes > 0 {
            let mut waited = false;
            loop {
                let notified = self.flush_completed.notified();
                tokio::pin!(notified);
                notified.as_mut().enable();

                let pending = self.pages.unflushed_bytes() + self.aux.lock().bytes_in_aux_buffers;
                if pending < self.policy.backpressure_bytes {
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
                }
                notified.await;
            }
        }

        let mut rel_rows = Vec::with_capacity(entries.len());
        let mut slru_rows = Vec::new();
        let mut meta_rows = Vec::new();
        for (key, lsn, value) in entries {
            if key.is_rel_block_key() {
                rel_rows.push(build_page_row(*key, *lsn, value)?);
            } else if key.is_slru_block_key() {
                slru_rows.push(build_slru_page_row(*key, *lsn, value)?);
            } else {
                meta_rows.push(build_meta_row(*key, *lsn, value)?);
            }
        }
        if rel_rows.is_empty() && slru_rows.is_empty() && meta_rows.is_empty() {
            return Ok(());
        }
        // Accumulate per-key delta counters here; the materialiser
        // trigger fires from `flush()` (after `mark_flushed`
        // advances `flush_horizon_lsn`), not from this put loop. That
        // way every job's `target_lsn` is durable by construction —
        // see `flush()` for the trigger site.
        let rel_limits = self
            .materializer
            .as_ref()
            .map(|m| (m.count_limit, m.payload_bytes_limit))
            .filter(|(c, b)| *c > 0 || *b > 0);
        // Rel rows: per-key DashMap shards; no global lock.
        for row in rel_rows {
            let key = row.key.clone();
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
        // bytes. Tallied straight off `entries` because the built
        // `slru_rows` drop the `Key`; `will_init` mirrors
        // `build_slru_page_row` (image / ZEROPAGE anchors the chain).
        if let Some(slru_count_limit) = self
            .materializer
            .as_ref()
            .map(|m| m.slru_count_limit)
            .filter(|&c| c > 0)
        {
            for (key, lsn, value) in entries {
                if !key.is_slru_block_key() {
                    continue;
                }
                let resets_chain = match value {
                    Value::Image(_) => true,
                    Value::WalRecord(rec) => rec.will_init(),
                };
                self.record_delta_for_materialize(key, *lsn, resets_chain, 0, slru_count_limit, 0);
            }
        }
        // Aux (slru + meta) buffers under one small mutex. Holds
        // are O(rows-in-this-batch); read paths blend buffered with
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

    /// Send a batch of materialise jobs to the worker. Extracted so
    /// the post-flush trigger (`flush()`) and read-side dispatch can
    /// share the same backpressure + metrics path. Caller must have
    /// already reserved each job's key in `in_flight`.
    async fn dispatch_materialize_jobs(&self, to_materialize: Vec<MaterializeJob>) {
        if to_materialize.is_empty() {
            return;
        }
        let Some(m) = self.materializer.as_ref() else {
            return;
        };
        let dispatched = to_materialize.len();
        let queue_cap = m.tx.max_capacity();
        let queue_remaining_before = m.tx.capacity();
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
            if !waited_for_capacity && m.tx.capacity() == 0 {
                waited_for_capacity = true;
                crate::metrics::PAGEWRITER_BACKPRESSURE_WAITS
                    .with_label_values(&["materializer"])
                    .inc();
            }
            let key_for_rollback = job.key.clone();
            if let Err(e) = m.tx.send(job).await {
                tracing::debug!("materialiser send dropped (receiver closed): {e}");
                m.in_flight.lock().unwrap().remove(&key_for_rollback);
                break;
            }
        }
        crate::metrics::MATERIALIZER_QUEUE_DEPTH
            .set(m.tx.max_capacity().saturating_sub(m.tx.capacity()) as u64);
        let queue_remaining_after = m.tx.capacity();
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

    /// Per-key materialise thresholds. SLRU (clog/multixact) block keys
    /// use the dedicated `slru_count_limit` with the payload trigger
    /// disabled (clog deltas are tiny); every other key uses the
    /// relation-page count + payload limits.
    fn materialize_limits(m: &MaterializerHandle, key: &Key) -> (u32, u32) {
        if key.is_slru_block_key() {
            (m.slru_count_limit, 0)
        } else {
            (m.count_limit, m.payload_bytes_limit)
        }
    }

    /// Advance the per-key delta tally and record a threshold crossing in
    /// `delta_ready`. An anchor (`resets_chain` — image or `will_init`)
    /// drops the key instead, since its chain no longer needs collapsing.
    /// Shared by the rel and SLRU put paths so both stay in lock-step with
    /// `collect_cold_residue_jobs`'s re-check.
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
            self.delta_ready.remove(key);
            return;
        }
        let crossed = {
            let mut tally = self.delta_counts.entry(key.clone()).or_default();
            let was_hit = tally.hits(count_limit, payload_bytes_limit);
            tally.count = tally.count.saturating_add(1);
            tally.payload_bytes = tally.payload_bytes.saturating_add(payload_len);
            if lsn > tally.last_lsn {
                tally.last_lsn = lsn;
            }
            // Crossed iff this put pushed the tally from below the
            // threshold to at/above it — record once per crossing so the
            // flush trigger can find this key without sweeping the whole
            // map. (Shard guard drops here, before the ready-set insert:
            // shard lock → DashSet, never nested.)
            tally.hits(count_limit, payload_bytes_limit) && !was_hit
        };
        if crossed {
            self.delta_ready.insert(key.clone());
        }
    }

    /// Walk `delta_counts` for keys whose tally crossed threshold but
    /// have no current-flush snapshot row, dispatching them at their
    /// recorded `last_lsn`. Safe only when `last_lsn ≤
    /// flush_horizon_lsn` (durable in YB by construction). Called
    /// from both flush()'s normal trigger path and its
    /// nothing-to-flush short-circuit, so cold residue still drains
    /// when compute goes quiet. Removes dispatched keys from
    /// `delta_counts` so subsequent flushes don't redispatch them.
    fn collect_cold_residue_jobs(&self) -> Vec<MaterializeJob> {
        let Some(m) = self.materializer.as_ref() else {
            return Vec::new();
        };
        if m.count_limit == 0 && m.payload_bytes_limit == 0 && m.slru_count_limit == 0 {
            return Vec::new();
        }
        let horizon = Lsn(self.flush_horizon_lsn.load(Ordering::Relaxed));
        if horizon == Lsn(0) {
            // No flush has ever completed — nothing is durable yet,
            // so cold-residue dispatch would point at non-durable
            // LSNs. Skip.
            return Vec::new();
        }
        let mut to_dispatch = Vec::new();
        let mut dispatched: Vec<Key> = Vec::new();
        let mut stale: Vec<Key> = Vec::new();
        // Walk only the keys that crossed a threshold (the ready set),
        // not all of `delta_counts`. The ready set is a hint: re-check the
        // live tally and drop any entry that was since reset or dispatched.
        for entry in self.delta_ready.iter() {
            let key = entry.key();
            let Some(tally) = self.delta_counts.get(key).map(|t| *t) else {
                // Tally gone (anchor reset it) — stale hint.
                stale.push(key.clone());
                continue;
            };
            let (count_limit, payload_bytes_limit) = Self::materialize_limits(m, key);
            if !tally.hits(count_limit, payload_bytes_limit) {
                stale.push(key.clone());
                continue;
            }
            if tally.last_lsn == Lsn(0) || tally.last_lsn > horizon {
                // Crossed but not yet durable — keep queued for a later
                // flush once the horizon advances.
                continue;
            }
            let inserted = m.in_flight.lock().unwrap().insert(key.clone());
            if inserted {
                dispatched.push(key.clone());
                to_dispatch.push(MaterializeJob {
                    key: key.clone(),
                    target_lsn: tally.last_lsn,
                });
            }
        }
        // Dispatched keys leave both maps; stale hints leave only the ready
        // set (their tally, if any, keeps accumulating).
        for k in &dispatched {
            self.delta_counts.remove(k);
            self.delta_ready.remove(k);
        }
        for k in &stale {
            self.delta_ready.remove(k);
        }
        to_dispatch
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
        if range.end.field1 == range.start.field1
            && range.end.field2 == range.start.field2
            && range.end.field3 == range.start.field3
            && range.end.field4 == range.start.field4
            && range.end.field5 == range.start.field5
            && range.end.field6 == range.start.field6.wrapping_add(1)
        {
            let chain = self.read_chain(range.start, target_lsn).await?;
            let mut out = std::collections::HashMap::with_capacity(1);
            if !chain.is_empty() {
                out.insert(range.start, chain);
            }
            return Ok(out);
        }
        let persisted = self
            .compute_page_store
            .scan_range_lineage(self.tenant_id, &self.lineage, range.clone(), target_lsn)
            .await?;
        let t_yb = t0.elapsed();
        let t_lock_start = Instant::now();
        let cached: std::collections::HashMap<Key, Vec<Arc<PageRow>>> = self
            .pages
            .lookup_range(&range.start, &range.end, target_lsn);
        let t_cache = t_lock_start.elapsed();
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
        for (key, chain) in out.iter_mut() {
            // Descending LSN; on ties prefer the image (see `read_chain`
            // for the rationale).
            chain.sort_by(|a, b| b.lsn.cmp(&a.lsn).then_with(|| b.is_image.cmp(&a.is_image)));
            dedup_page_chain_by_lsn(chain);
            if let Some(idx) = chain.iter().position(|r| r.will_init) {
                chain.truncate(idx + 1);
            }
            // Per-key read-side trigger: a chain longer than
            // count_limit means the write trigger lost a job (PS
            // crash, queue overflow). Enqueue a fresh materialise so
            // the next read of the same key is fast.
            let crossed = self.chain_crossed_segment(chain);
            self.maybe_dispatch_read_side_materialize(*key, target_lsn, chain.len(), crossed);
        }
        let t_merge = t_merge_start.elapsed();
        let total = t0.elapsed();
        if total.as_millis() >= 5 {
            info!(
                tag = "scan_range",
                total_ms = total.as_millis() as u64,
                yb_ms = t_yb.as_millis() as u64,
                cache_ms = t_cache.as_millis() as u64,
                merge_ms = t_merge.as_millis() as u64,
                yb_rows = persisted_rows,
                cache_rows = cached_rows,
                keys_out = out.len(),
                "perf",
            );
        }
        Ok(out)
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
        let Some(m) = self.materializer.as_ref() else {
            return;
        };
        // The materialiser's target_lsn must be durable. The read's
        // target_lsn can sit above the horizon (compute observing its
        // own pre-flush writes). Cap at the horizon so the resulting
        // job won't ask the materialiser to read YB above the
        // partial-flush window. If horizon is 0 (no flush yet) skip
        // the dispatch — nothing to materialize against.
        let horizon = Lsn(self
            .flush_horizon_lsn
            .load(std::sync::atomic::Ordering::Relaxed));
        if horizon.0 == 0 {
            return;
        }
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
        if try_dispatch_read_side(
            key,
            bounded_target,
            effective_len,
            m.count_limit,
            &m.in_flight,
            &m.tx,
        ) {
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
    /// under lineage: its anchor must sit at or above the branch cap,
    /// otherwise ancestor rows in (anchor_lsn, cap] may exist that the
    /// cache cannot know about.
    fn cache_anchor_covers_lineage(&self, chain: &[Arc<PageRow>]) -> bool {
        if self.lineage.len() <= 1 {
            return true;
        }
        chain
            .last()
            .is_some_and(|r| r.will_init && r.lsn >= self.lineage[1].cap_lsn)
    }

    pub async fn read_chain(&self, key: Key, target_lsn: Lsn) -> Result<Vec<Arc<PageRow>>> {
        let key_bytes = key_to_bytes(key);
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
                return Ok(chain);
            }
        }
        let cached: Vec<Arc<PageRow>> = self.pages.lookup_chain(&key, target_lsn);
        let persisted = self
            .compute_page_store
            .read_chain_lineage(self.tenant_id, &self.lineage, &key_bytes, target_lsn)
            .await?;
        let mut merged = cached;
        merged.extend(persisted);
        // Descending LSN; on ties (materialiser INSERTed an image at
        // the same LSN as an existing delta), the image must sort
        // first so dedup keeps it and discards the now-superseded
        // delta. Otherwise the chain can lose its anchor.
        merged.sort_by(|a, b| b.lsn.cmp(&a.lsn).then_with(|| b.is_image.cmp(&a.is_image)));
        dedup_page_chain_by_lsn(&mut merged);
        if let Some(idx) = merged.iter().position(|r| r.will_init) {
            merged.truncate(idx + 1);
        }
        let crossed = self.chain_crossed_segment(&merged);
        self.maybe_dispatch_read_side_materialize(key, target_lsn, merged.len(), crossed);
        Ok(merged)
    }

    /// Read the chain for `key` at `target_lsn`, using only durable
    /// sources (post-flush cache + PageStore). Pre-flush rows are
    /// EXCLUDED. The image materialiser uses this so its image is
    /// built only from data that survives a process crash.
    pub async fn read_chain_durable(
        &self,
        key: Key,
        target_lsn: Lsn,
    ) -> Result<Vec<Arc<PageRow>>> {
        // `target_lsn` is the caller's contract: it must be ≤
        // `flush_horizon_lsn` so a YB read above the horizon can't
        // surface a mid-flush partial-chunk view. The post-flush
        // materialiser trigger guarantees this by construction
        // (it sets `target_lsn` to the snapshot's max, which the
        // horizon was just advanced to). The read-side dispatch
        // path must bound its own `target_lsn` similarly.
        let key_bytes = key_to_bytes(key);
        // Cache-only fast path (durable-only variant for the
        // materialiser): same completeness criterion as `read_chain`,
        // including the lineage caveat on sub-cap anchors.
        if let Some(chain) = self
            .pages
            .lookup_chain_durable_if_complete(&key, target_lsn)
        {
            if self.cache_anchor_covers_lineage(&chain) {
                return Ok(chain);
            }
        }
        let cached: Vec<Arc<PageRow>> = self.pages.lookup_chain_durable(&key, target_lsn);
        // Materialiser-side cache miss → the internal conn (same one
        // the materialiser's write path uses). Keeping this off
        // `compute_page_store` is the whole point of the split: a
        // long materialiser walredo-and-INSERT can't stall compute.
        let persisted = self
            .fpu_page_store
            .read_chain_lineage(self.tenant_id, &self.lineage, &key_bytes, target_lsn)
            .await?;
        let mut merged = cached;
        merged.extend(persisted);
        // Descending LSN; on ties (materialiser INSERTed an image at
        // the same LSN as an existing delta), the image must sort
        // first so dedup keeps it and discards the now-superseded
        // delta. Otherwise the chain can lose its anchor.
        merged.sort_by(|a, b| b.lsn.cmp(&a.lsn).then_with(|| b.is_image.cmp(&a.is_image)));
        dedup_page_chain_by_lsn(&mut merged);
        if let Some(idx) = merged.iter().position(|r| r.will_init) {
            merged.truncate(idx + 1);
        } else if !merged.is_empty() {
            // No `will_init` anchor in the merged cache+YB chain ≤
            // target_lsn (#57 part 2). `parse_chain` would set
            // `base_img=None` and walredo would fold these deltas onto a
            // ZERO page; the materialiser would then `install` +
            // `trim_key_up_to` that wrong/zero image, making the
            // corruption durable — surfacing later as a heap hole /
            // orphan catalog row / "unexpected zero page". A complete
            // durable chain must always reach a will_init anchor; its
            // absence means the page's init record is missing upstream.
            // Refuse loudly rather than fabricate a zero-based page: a
            // hard error stops the materialiser job (logged + skipped by
            // `redo_one`) instead of silently amplifying the loss.
            crate::metrics::CORRECTNESS_ERRORS
                .with_label_values(&["materialize_unanchored_chain"])
                .inc();
            let lo = merged.last().map(|r| r.lsn);
            let hi = merged.first().map(|r| r.lsn);
            anyhow::bail!(
                "read_chain_durable: no will_init anchor in chain ≤ target_lsn={target_lsn} \
                 (chain_len={}, lsn lo={lo:?} hi={hi:?}, key={key_bytes:?}) — the page's \
                 init record is missing; refusing to materialise a zero-based page",
                merged.len(),
            );
        }
        Ok(merged)
    }

    /// Install a freshly-materialised image into the cache. Call AFTER
    /// `PageStore::put_pages([image])` succeeds. Drops the deltas the
    /// image supersedes for that key (durable, lsn ≤ image.lsn) and
    /// indexes the new image as a durable cache entry; subsequent
    /// `read_chain` / `read_chain_durable` find the image first and
    /// don't walk the chain back.
    pub async fn install_materialized_image(&self, image: Arc<PageRow>) {
        // No outer lock — `pages` is internally sharded. The three
        // calls below only touch the image's key shard (trim, push)
        // plus the LRU (small parking_lot::Mutex held for ns) plus
        // evict (LRU walk + per-shard removes). Concurrent reads on
        // other keys do not block install; reads on THIS key see a
        // brief shard-lock blip while trim/push run.
        let t_start = Instant::now();
        let key = image.key.clone();
        let lsn = image.lsn;
        let t_trim_start = Instant::now();
        self.pages.trim_key_up_to(&key, lsn);
        let t_trim = t_trim_start.elapsed();
        let t_push_start = Instant::now();
        self.pages.push_durable_arc(image);
        let t_push = t_push_start.elapsed();
        let t_evict_start = Instant::now();
        self.pages.evict_to_capacity();
        let t_evict = t_evict_start.elapsed();
        let total = t_start.elapsed();
        if total.as_millis() >= 5 {
            info!(
                tag = "install_image",
                total_ms = total.as_millis() as u64,
                trim_us = t_trim.as_micros() as u64,
                push_us = t_push.as_micros() as u64,
                evict_us = t_evict.as_micros() as u64,
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
        // computing `flush_horizon_lsn` / `disk_consistent_lsn` from
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
        let rel_rows: Vec<Arc<PageRow>> =
            rel_snapshot.iter().map(|e| e.row_arc()).collect();
        for row in &rel_rows {
            pageserver_api::write_trace::trace_stage("snapshot", &row.key, row.lsn);
        }
        let (slru_rows, meta_rows) = {
            let aux = self.aux.lock();
            (aux.slru_buffer.clone(), aux.meta_buffer.clone())
        };
        let rel_count = rel_rows.len();
        let slru_count = slru_rows.len();
        let meta_count = meta_rows.len();
        if rel_count == 0 && slru_count == 0 && meta_count == 0 {
            // Nothing to flush. Cold-key residue (tallies whose
            // threshold-crossing happened in past flushes but
            // haven't dispatched — e.g. blocked by in_flight that
            // has since cleared) can still drain here. Without this
            // pass, delta_counts grows monotonically as workloads
            // wind down — the residue would only get re-examined on
            // a flush that has rows, which never comes once compute
            // is idle.
            let to_dispatch = self.collect_cold_residue_jobs();
            self.dispatch_materialize_jobs(to_dispatch).await;
            return Ok(());
        }

        // INSERT to YB outside any lock so concurrent puts and reads
        // aren't blocked by the round-trip.
        let res = self.try_write_rows(&rel_rows, &slru_rows, &meta_rows).await;

        if res.is_ok() {
            // Everything at lsn ≤ ingest_frontier is now durable: rows
            // ≤ frontier were either already durable or in the
            // snapshots we just wrote (see the frontier capture above).
            // The frontier — NOT the max lsn of the flushed rows — is
            // the new candidate for `flush_horizon_lsn` and
            // `disk_consistent_lsn`: the snapshot can contain a row at
            // lsn X while missing a concurrently-put row at lsn < X,
            // so max(snapshot) is not a durability frontier.
            // Promote rel rows in-place: flip durable on the exact
            // `Arc<PageEntry>`s we captured in the snapshot. No
            // by-(key, lsn) lookup, so a materialiser race that
            // overwrote a same-LSN slot can't make us over-pop the
            // FIFO and steal a fresh row from the next batch.
            self.pages.mark_flushed(&rel_snapshot);
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
            // Materialiser trigger fires here, not from `put`. The
            // trigger LSN is the just-promoted max per key in this
            // snapshot — durable by construction (every row in
            // `rel_rows` is now in YB AND marked durable in cache).
            // Walking `delta_counts` finds keys whose count or
            // payload-byte tally crossed `image_chain_limit` /
            // `image_chain_payload_bytes_limit`; for each, find the
            // max LSN seen in this snapshot and enqueue one job.
            // Keys present in the current snapshot dispatch at their
            // per-snapshot max LSN; cold-key residue (threshold-crossed
            // in a past flush, no current-snapshot row) dispatches at
            // its tally's `last_lsn` — guaranteed durable when
            // `last_lsn ≤ flush_horizon_lsn`, which we've just
            // advanced. Without this fallback, cold residue
            // accumulates in `delta_counts` forever and the periodic
            // flush walk grows O(forever) instead of O(active keys).
            let mut to_materialize: Vec<MaterializeJob> = Vec::new();
            if let Some(m) = self.materializer.as_ref()
                && (m.count_limit > 0 || m.payload_bytes_limit > 0 || m.slru_count_limit > 0)
            {
                let mut max_lsn_per_key: HashMap<Key, Lsn> = HashMap::new();
                for row in &rel_rows {
                    let entry = max_lsn_per_key.entry(row.key.clone()).or_insert(Lsn(0));
                    if row.lsn > *entry {
                        *entry = row.lsn;
                    }
                }
                let horizon = Lsn(self.flush_horizon_lsn.load(Ordering::Relaxed));
                let mut dispatched: Vec<Key> = Vec::new();
                let mut stale: Vec<Key> = Vec::new();
                // Only the threshold-crossers (ready set) can produce a job —
                // walk it instead of sweeping all of `delta_counts`. Keys that
                // crossed during this flush's puts are already in the set (the
                // put loop records the crossing). The ready set is a hint:
                // re-check the live tally and drop stale entries.
                for entry in self.delta_ready.iter() {
                    let key = entry.key();
                    let Some(tally) = self.delta_counts.get(key).map(|t| *t) else {
                        stale.push(key.clone());
                        continue;
                    };
                    let (count_limit, payload_bytes_limit) = Self::materialize_limits(m, key);
                    if !tally.hits(count_limit, payload_bytes_limit) {
                        stale.push(key.clone());
                        continue;
                    }
                    let target_lsn = match max_lsn_per_key.get(key) {
                        Some(&lsn) => lsn,
                        None => {
                            // Cold residue. Dispatch at `last_lsn` iff
                            // it's durable; otherwise the only delta
                            // is still buffered (just-bumped pre-flush
                            // by an unflushed put after the snapshot)
                            // and we leave it for the next flush. The
                            // `Lsn(0)` guard handles tallies created
                            // before the `last_lsn` field existed in
                            // case of in-place upgrades.
                            if tally.last_lsn == Lsn(0) || tally.last_lsn > horizon {
                                continue;
                            }
                            tally.last_lsn
                        }
                    };
                    let inserted = m.in_flight.lock().unwrap().insert(key.clone());
                    if inserted {
                        dispatched.push(key.clone());
                        to_materialize.push(MaterializeJob {
                            key: key.clone(),
                            target_lsn,
                        });
                    }
                }
                for k in &dispatched {
                    self.delta_counts.remove(k);
                    self.delta_ready.remove(k);
                }
                for k in &stale {
                    self.delta_ready.remove(k);
                }
            }
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
                aux.bytes_in_aux_buffers = aux.bytes_in_aux_buffers.saturating_sub(aux_drop_bytes);
                aux.slru_buffer.drain(0..slru_count);
                aux.meta_buffer.drain(0..meta_count);
                // NB (#57 part 2): as on the rel FIFO above, a
                // meta/SLRU row ≤ frontier remaining after the drain is the
                // benign reconnect-replay re-push of an already-durable row,
                // not a data-losing over-advance — see the rel-path comment
                // for the full rationale.
                if self.pages.unflushed_count() == 0
                    && aux.slru_buffer.is_empty()
                    && aux.meta_buffer.is_empty()
                {
                    aux.opened_at = None;
                }
            }
            self.dispatch_materialize_jobs(to_materialize).await;
            // Wake any `put()` callers blocked on backpressure. Done
            // after the state lock is released so the woken tasks can
            // re-take it immediately.
            self.flush_completed.notify_waiters();

            // Outside the writer lock: persist the dc_lsn advance to
            // `pageserver.timeline_state` if (a) it actually moved AND
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
                        .fpu_page_store
                        .advance_consistent_lsn(
                            self.tenant_id,
                            self.timeline_id,
                            self.generation,
                            lsn,
                            prev_record_lsn,
                        )
                        .await
                    {
                        Ok(crate::page_store::AdvanceConsistentLsnOutcome::Updated) => {
                            self.consistent_lsn_watermark.store(new_lsn, Relaxed);
                            *self.last_consistent_lsn_upsert_at.lock().unwrap() = Some(Instant::now());
                            // Publish the just-advanced LSN as the
                            // safekeeper-truncation bound. The walreceiver
                            // loop forwards this slot to the safekeeper
                            // in PageserverFeedback.remote_consistent_lsn,
                            // bounding `truncate_lsn` / `peer_horizon_lsn`.
                            // The gen-fenced UPDATE above is the
                            // durability commit point — this publish
                            // only echoes a fact that's already in YB.
                            self.publish_consistent_lsn(lsn);
                        }
                        Ok(crate::page_store::AdvanceConsistentLsnOutcome::StaleGeneration {
                            live_generation,
                        }) => {
                            // Fatal: a higher-generation pageserver has
                            // taken over. Forward progress is
                            // impossible — the dc_lsn UPDATE is the
                            // gen fence on the data write side; once
                            // we lose it any subsequent writes are
                            // either redundant (live PS will overwrite)
                            // or actively wrong (stale dc_lsn would
                            // shrink the safekeeper truncation
                            // horizon). See architecture/
                            // yb-pageserver.md "Generation fencing".
                            crate::task_mgr::shutdown_for_stale_generation(
                                self.tenant_id,
                                self.timeline_id,
                                self.generation,
                                live_generation,
                            );
                        }
                        Err(e) => {
                            // Transient YB / network error. Pages are
                            // already in YB, only the timeline row
                            // lags. Next flush retries; SK keeps a
                            // little more WAL until we catch up.
                            tracing::warn!(
                                tenant_id = %self.tenant_id,
                                timeline_id = %self.timeline_id,
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
        res
    }

    async fn try_write_rows(
        &self,
        rel_rows: &[Arc<PageRow>],
        slru_rows: &[SlruPageRow],
        meta_rows: &[MetaRow],
    ) -> Result<()> {
        // All flush writes go via the internal conn. Compute getPage
        // never queues behind a 25 MiB INSERT.
        if !rel_rows.is_empty() {
            self.fpu_page_store
                .put_pages(self.tenant_id, self.timeline_id, rel_rows)
                .await?;
        }
        if !slru_rows.is_empty() {
            self.fpu_page_store
                .put_slru_pages(self.tenant_id, self.timeline_id, slru_rows)
                .await?;
        }
        if !meta_rows.is_empty() {
            self.fpu_page_store
                .put_meta(self.tenant_id, self.timeline_id, meta_rows)
                .await?;
        }
        Ok(())
    }

    pub async fn should_flush(&self) -> bool {
        let aux = self.aux.lock();
        let unflushed_bytes = self.pages.unflushed_bytes() + aux.bytes_in_aux_buffers;
        if unflushed_bytes >= self.policy.max_bytes {
            return true;
        }
        if let Some(at) = aux.opened_at
            && at.elapsed() >= self.policy.max_age
        {
            return true;
        }
        false
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
    /// persists `disk_consistent_lsn` so the matching
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

        let mut out = self
            .compute_page_store
            .scan_meta_range_lineage(self.tenant_id, &self.lineage, range.clone(), target_lsn)
            .await?;
        let aux = self.aux.lock();
        for row in aux.meta_buffer.iter() {
            if row.key.len() != KEY_SIZE || row.lsn > target_lsn {
                continue;
            }
            let k = Key::from_slice(&row.key);
            if k < range.start || k >= range.end {
                continue;
            }
            match out.get(&row.key) {
                Some(existing) if existing.lsn >= row.lsn => continue,
                _ => {
                    out.insert(row.key.clone(), row.clone());
                }
            }
        }
        Ok(out)
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
        let mut buffered: Vec<SlruPageRow> = {
            let aux = self.aux.lock();
            aux.slru_buffer
                .iter()
                .filter(|r| {
                    r.kind == kind && r.segno == segno && r.blknum == blknum && r.lsn <= target_lsn
                })
                .cloned()
                .collect()
        };
        let persisted = self
            .compute_page_store
            .read_slru_chain_lineage(
                self.tenant_id,
                &self.lineage,
                kind,
                segno,
                blknum,
                target_lsn,
            )
            .await?;
        buffered.extend(persisted);
        buffered.sort_by(|a, b| b.lsn.cmp(&a.lsn));
        if let Some(idx) = buffered.iter().position(|r| r.will_init) {
            buffered.truncate(idx + 1);
        }
        Ok(buffered)
    }

    /// Range scan over an SLRU segment, blending buffered + persisted.
    /// `blknum_range` lives within one `(kind, segno)`.
    pub async fn scan_slru_range(
        &self,
        kind: SlruKind,
        segno: u32,
        blknum_range: std::ops::Range<u32>,
        target_lsn: Lsn,
    ) -> Result<std::collections::HashMap<u32, Vec<SlruPageRow>>> {
        let persisted = self
            .compute_page_store
            .scan_slru_range_lineage(
                self.tenant_id,
                &self.lineage,
                kind,
                segno,
                blknum_range.clone(),
                target_lsn,
            )
            .await?;
        let buffered: Vec<SlruPageRow> = {
            let aux = self.aux.lock();
            aux.slru_buffer
                .iter()
                .filter(|r| {
                    r.kind == kind
                        && r.segno == segno
                        && r.blknum >= blknum_range.start
                        && r.blknum < blknum_range.end
                        && r.lsn <= target_lsn
                })
                .cloned()
                .collect()
        };
        let mut out: std::collections::HashMap<u32, Vec<SlruPageRow>> = persisted;
        for row in buffered {
            out.entry(row.blknum).or_default().push(row);
        }
        for chain in out.values_mut() {
            chain.sort_by(|a, b| b.lsn.cmp(&a.lsn));
            if let Some(idx) = chain.iter().position(|r| r.will_init) {
                chain.truncate(idx + 1);
            }
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
        let handle = tokio::spawn(async move {
            loop {
                tokio::select! {
                    _ = cancel_for_task.cancelled() => break,
                    _ = tokio::time::sleep(tick) => {}
                }
                let Some(w) = weak.upgrade() else { break };
                // `stats()` refreshes the unflushed-bytes Prometheus
                // gauge as a side-effect. Done every tick so a
                // /metrics scrape reflects the current buffer depth
                // even when there's nothing to flush.
                let _ = w.stats().await;
                if w.should_flush().await
                    && let Err(e) = w.flush().await
                {
                    crate::metrics::CORRECTNESS_ERRORS
                        .with_label_values(&["pagewriter_flush"])
                        .inc();
                    tracing::error!(
                        "PAGEWRITER PERIODIC FLUSH FAILED — rows may not have \
                         reached YB; walreceiver will retry on resume but \
                         disk_consistent_lsn may stall: {e:?}"
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
                         disk_consistent_lsn is stale on disk; next attach \
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
    if tx
        .try_send(MaterializeJob {
            key,
            target_lsn,
        })
        .is_err()
    {
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
    use utils::id::{TenantId, TimelineId};
    use wal_decoder::models::record::NeonWalRecord;

    use crate::page_store::{PageStore, PageStoreConfig};

    fn test_yb_config() -> YbConnConfig {
        let host = std::env::var("YB_TEST_HOST").unwrap_or_else(|_| "127.0.0.1".to_string());
        let port = std::env::var("YB_TEST_PORT").unwrap_or_else(|_| "5433".to_string());
        YbConnConfig {
            host_ports: format!("{host}:{port}").parse().unwrap(),
            user: std::env::var("YB_TEST_USER").unwrap_or_else(|_| "yugabyte".to_string()),
            password_env: None,
            dbname: std::env::var("YB_TEST_DB").unwrap_or_else(|_| "yb_storage".to_string()),
            sslmode: None,
            sslrootcert: None,
        }
    }

    /// Shared-schema test scaffolding — see the matching comment in
    /// `page_store.rs::tests::SHARED_STORE`. All tests in this binary
    /// share one schema, isolated by `rand_tenant()`/`rand_timeline()`.
    /// Per-test schema DDL was costing 5–10 s of YB tablet
    /// coordination apiece; sharing amortizes that to once per binary.
    static SHARED_STORE: tokio::sync::OnceCell<Arc<PageStore>> = tokio::sync::OnceCell::const_new();
    const PROD_SCHEMA: &str = "pageserver";

    async fn make_store(_suffix: &str) -> (Arc<PageStore>, String) {
        let store = SHARED_STORE
            .get_or_init(|| async {
                let cfg = PageStoreConfig {
                    yb: test_yb_config(),
                    schema: PROD_SCHEMA.to_string(),
                    scale_factor: pageserver_api::config::DEFAULT_SCALE_FACTOR,
                };
                let store = PageStore::connect(cfg, "pw_test", true)
                    .await
                    .expect("connect to local YB; run `yb_light_local init && start` first to migrate the schema");
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

    fn writer(store: Arc<PageStore>, policy: FlushPolicy) -> (PageWriter, TenantId, TimelineId) {
        let t = TenantId::generate();
        let tl = TimelineId::generate();
        (
            // Tests don't exercise the connection split — pass the
            // same PageStore arc as both compute + internal.
            PageWriter::new(
                t,
                tl,
                utils::generation::Generation::new(1),
                store.clone(),
                store,
                policy,
                64 * 1024 * 1024,
                0,
            ),
            t,
            tl,
        )
    }

    /// A `PageWriter` with the meta LRU cache enabled and capacity
    /// `entries`. Pass `entries = 1` to force eviction on the second key.
    fn writer_with_cache(
        store: Arc<PageStore>,
        policy: FlushPolicy,
        entries: usize,
    ) -> (PageWriter, TenantId, TimelineId) {
        let t = TenantId::generate();
        let tl = TimelineId::generate();
        (
            PageWriter::new(
                t,
                tl,
                utils::generation::Generation::new(1),
                store.clone(),
                store,
                policy,
                64 * 1024 * 1024,
                entries,
            ),
            t,
            tl,
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
    ) -> (
        PageWriter,
        TenantId,
        TimelineId,
        tokio::sync::mpsc::Receiver<MaterializeJob>,
        Arc<std::sync::Mutex<std::collections::HashSet<Key>>>,
    ) {
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
    ) -> (
        PageWriter,
        TenantId,
        TimelineId,
        tokio::sync::mpsc::Receiver<MaterializeJob>,
        Arc<std::sync::Mutex<std::collections::HashSet<Key>>>,
    ) {
        let t = TenantId::generate();
        let tl = TimelineId::generate();
        let mut w = PageWriter::new(
            t,
            tl,
            utils::generation::Generation::new(1),
            store.clone(),
            store,
            policy,
            64 * 1024 * 1024,
            0,
        );
        let (tx, rx) = tokio::sync::mpsc::channel(64);
        let in_flight = Arc::new(std::sync::Mutex::new(std::collections::HashSet::new()));
        w.set_materializer(MaterializerHandle {
            count_limit,
            slru_count_limit,
            payload_bytes_limit,
            tx,
            in_flight: in_flight.clone(),
        });
        (w, t, tl, rx, in_flight)
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
        let (w, _t, _tl) = writer(store.clone(), FlushPolicy::default());
        w.flush().await.unwrap();
        let stats = w.stats().await;
        assert_eq!(stats.buffered_rows, 0);
        drop_schema(&store, &schema).await;
    }

    #[tokio::test]
    async fn image_passes_through_untouched() {
        let (store, schema) = make_store("image_pt").await;
        let (w, t, tl) = writer(store.clone(), FlushPolicy::default());
        w.put(&[(k(1), Lsn(10), image())]).await.unwrap();
        let stats = w.stats().await;
        assert_eq!(stats.rows_written, 1);
        assert_eq!(stats.buffered_rows, 1);
        w.flush().await.unwrap();
        let chain = store
            .read_chain(t, tl, &key_to_bytes(k(1)), Lsn(100))
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
        let (w, t, tl) = writer(store.clone(), FlushPolicy::default());
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
            .read_chain(t, tl, &key_to_bytes(k(1)), Lsn(100))
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
    /// deltas. `read_chain_durable` must NOT hand that chain to the
    /// materialiser: `parse_chain` would set `base_img=None` and walredo
    /// would fold the deltas onto a ZERO page, then `install + trim`
    /// would make that wrong/zero page durable — the "unexpected zero
    /// page" / heap-hole / orphan-catalog corruption. The fix errors;
    /// `redo_one` logs it and skips the page (leaving the deltas in
    /// place) rather than materialising a zero-based image.
    #[tokio::test]
    async fn read_chain_durable_refuses_anchorless_chain() {
        let (store, schema) = make_store("anchorless").await;
        let (w, t, tl) = writer(store.clone(), FlushPolicy::default());
        // Deltas only, no will_init anchor anywhere ≤ target.
        w.put(&[(k(1), Lsn(20), delta()), (k(1), Lsn(30), delta())])
            .await
            .unwrap();
        w.flush().await.unwrap();

        // Sanity: the raw store chain is genuinely anchorless (no
        // will_init), which is the precondition the fix must catch.
        let raw = store
            .read_chain(t, tl, &key_to_bytes(k(1)), Lsn(100))
            .await
            .unwrap();
        assert!(!raw.is_empty(), "deltas should be durable");
        assert!(
            !raw.iter().any(|r| r.will_init),
            "precondition: chain has no will_init anchor"
        );

        // The materialiser-facing read must refuse the anchorless chain.
        let res = w.read_chain_durable(k(1), Lsn(100)).await;
        assert!(
            res.is_err(),
            "read_chain_durable must error on an anchorless chain (would zero-base \
             the page); got Ok with {} rows",
            res.map(|c| c.len()).unwrap_or(0),
        );
        drop_schema(&store, &schema).await;
    }

    #[tokio::test]
    async fn long_chain_without_image_round_trips() {
        // 100 deltas after a will_init delta — no image, no truncation.
        // read_chain returns the full chain up to the anchor.
        let (store, schema) = make_store("long_chain").await;
        let (w, t, tl) = writer(store.clone(), FlushPolicy::default());
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
            .read_chain(t, tl, &key_to_bytes(k(1)), Lsn(2000))
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
        let (w, t, tl) = writer(store.clone(), FlushPolicy::default());

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
            .read_meta(t, tl, &meta_key_bytes, Lsn(100))
            .await
            .unwrap()
            .expect("meta row");
        assert_eq!(got.payload.as_ref(), b"natural-image-bytes");
        drop_schema(&store, &schema).await;
    }

    #[tokio::test]
    async fn flush_emits_a_single_insert_round_trip() {
        let (store, schema) = make_store("flush_one_rt").await;
        let (w, t, tl) = writer(store.clone(), FlushPolicy::default());
        let entries: Vec<(Key, Lsn, Value)> = (0..50).map(|i| (k(i), Lsn(10), image())).collect();
        w.put(&entries).await.unwrap();
        assert_eq!(w.stats().await.buffered_rows, 50);
        w.flush().await.unwrap();
        assert_eq!(w.stats().await.buffered_rows, 0);
        // Sample-read one of them.
        let chain = store
            .read_chain(t, tl, &key_to_bytes(k(0)), Lsn(100))
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
        };
        let (w, _t, _tl) = writer(store.clone(), policy);
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
        };
        let (w, _t, _tl) = writer(store.clone(), policy);
        w.put(&[(k(1), Lsn(10), image())]).await.unwrap();
        assert!(!w.should_flush().await);
        tokio::time::sleep(Duration::from_millis(150)).await;
        assert!(w.should_flush().await);
        w.flush().await.unwrap();
        assert!(!w.should_flush().await);
        drop_schema(&store, &schema).await;
    }

    #[tokio::test]
    async fn read_chain_blends_buffered_and_persisted() {
        let (store, schema) = make_store("blend").await;
        let (w, _t, _tl) = writer(store.clone(), FlushPolicy::default());
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
        let (w, _t, _tl) = writer(store.clone(), FlushPolicy::default());
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
        let t = TenantId::generate();
        let tl = TimelineId::generate();
        let policy = FlushPolicy {
            max_age: Duration::from_millis(40),
            max_bytes: 1 << 30,
            backpressure_bytes: 0,
        };
        let w = Arc::new(PageWriter::new(
            t,
            tl,
            utils::generation::Generation::new(1),
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
            .read_chain(t, tl, &key_to_bytes(k(1)), Lsn(100))
            .await
            .unwrap();
        assert_eq!(chain.len(), 1);
        task.shutdown().await;
        drop_schema(&store, &schema).await;
    }

    #[tokio::test]
    async fn periodic_flusher_does_final_flush_on_shutdown() {
        let (store, schema) = make_store("flusher_final").await;
        let t = TenantId::generate();
        let tl = TimelineId::generate();
        let policy = FlushPolicy {
            max_age: Duration::from_secs(3600),
            max_bytes: 1 << 30,
            backpressure_bytes: 0,
        };
        let w = Arc::new(PageWriter::new(
            t,
            tl,
            utils::generation::Generation::new(1),
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
            .read_chain(t, tl, &key_to_bytes(k(1)), Lsn(100))
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
    /// sentinels then hit MissingKey, propagated corruption to compute,
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
        let (w, _t, _tl) = writer(store.clone(), FlushPolicy::default());
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
        let (w, t, tl) = writer(store.clone(), FlushPolicy::default());
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
            .read_chain(t, tl, &key_to_bytes(k(1)), Lsn(100))
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
        let (w, _t, _tl, mut rx, in_flight) =
            writer_with_materializer(store.clone(), FlushPolicy::default(), LIMIT);

        // (LIMIT - 1) deltas + a flush — the counter sits below
        // threshold, so the post-flush trigger doesn't fire yet.
        for i in 0..(LIMIT - 1) {
            w.put(&[(k(1), Lsn(10 + i as u64), delta_non_init())])
                .await
                .unwrap();
        }
        w.flush().await.unwrap();
        assert!(rx.try_recv().is_err(), "premature dispatch below threshold");

        // LIMITth delta + flush: the post-flush trigger sees count ≥
        // limit and enqueues exactly one job at the snapshot's max
        // LSN for this key.
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
        let (w, _t, _tl, mut rx, in_flight) = writer_with_materializer_thresholds(
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
        let (w, _t, _tl, mut rx, _in_flight) =
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

    /// A key that crosses the threshold (entering the ready set) and is
    /// then reset by an anchor *before any flush* must NOT dispatch: the
    /// anchor clears both the tally and the ready-set hint. Guards the
    /// ready-set ↔ `delta_counts` sync introduced with the per-flush
    /// walk optimization (#21) — the trigger walks only the ready set, so
    /// a crossing that's since been cancelled must not linger there.
    #[tokio::test]
    async fn anchor_after_crossing_cancels_dispatch() {
        let (store, schema) = make_store("mat_cross_then_reset").await;
        const LIMIT: u32 = 3;
        let (w, _t, _tl, mut rx, _in_flight) =
            writer_with_materializer(store.clone(), FlushPolicy::default(), LIMIT);

        // Cross the threshold — k(1) enters the ready set.
        for i in 0..LIMIT {
            w.put(&[(k(1), Lsn(10 + i as u64), delta_non_init())])
                .await
                .unwrap();
        }
        // Anchor at a higher LSN resets the chain before the flush fires.
        w.put(&[(k(1), Lsn(100), image())]).await.unwrap();
        w.note_record_lsn(Lsn(100), Lsn(99));
        w.flush().await.unwrap();
        assert!(
            rx.try_recv().is_err(),
            "anchor after crossing must cancel the queued dispatch",
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

        let (w, _t, _tl, mut rx, in_flight) =
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
        let (w, _t, _tl, mut rx, _in_flight) = writer_with_materializer_thresholds(
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
        let (w, _t, _tl, mut rx, in_flight) =
            writer_with_materializer(store.clone(), FlushPolicy::default(), LIMIT);

        // First threshold + flush: dispatch.
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

        // While the key is in_flight, subsequent flushes must NOT
        // enqueue duplicates for it.
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
        // The next push + flush re-fires at the current LSN (counter
        // still ≥ threshold from the dedup-blocked attempt).
        in_flight.lock().unwrap().remove(&first.key);
        w.put(&[(k(1), Lsn(500), delta_non_init())]).await.unwrap();
        w.flush().await.unwrap();
        let second = rx.try_recv().expect("re-fires after in_flight release");
        assert_eq!(
            second.target_lsn,
            Lsn(500),
            "re-dispatch must target the latest flushed LSN for this key"
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
        let (w, _t, _tl, mut rx, _in_flight) =
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
        let (w, _t, _tl, mut rx, _in_flight) =
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

    /// The post-flush trigger refactor (Phase 4.d follow-on): even
    /// when the per-key tally is well past the threshold, no
    /// MaterializeJob fires until a flush completes. Regression
    /// guard for the put-time trigger semantics we replaced —
    /// nothing should sneak through `put()`.
    #[tokio::test]
    async fn dispatch_does_not_fire_without_flush() {
        let (store, schema) = make_store("mat_no_dispatch_without_flush").await;
        const LIMIT: u32 = 10;
        let (w, _t, _tl, mut rx, _in_flight) =
            writer_with_materializer(store.clone(), FlushPolicy::default(), LIMIT);

        // Push 10× the threshold without ever flushing. Pre-refactor
        // the put-time trigger would have dispatched as soon as the
        // counter crossed LIMIT; post-refactor nothing should land
        // in the channel until `flush()` runs.
        for i in 0..(LIMIT * 10) as u64 {
            w.put(&[(k(1), Lsn(10 + i), delta_non_init())])
                .await
                .unwrap();
        }
        assert!(
            rx.try_recv().is_err(),
            "no dispatch should happen until a flush completes",
        );

        // The pending flush only fires after we explicitly flush.
        w.flush().await.unwrap();
        let job = rx
            .try_recv()
            .expect("flush completes → post-flush trigger fires");
        // target_lsn is the max LSN in the snapshot, not the LSN of
        // some interior row that "crossed the threshold". With
        // pushes 10..(10 + 100), max = 109.
        assert_eq!(job.target_lsn, Lsn(10 + (LIMIT * 10 - 1) as u64));

        drop_schema(&store, &schema).await;
    }

    /// `flush_horizon_lsn` advances atomically inside
    /// `mark_flushed` to `max(snapshot.lsn)`. The post-flush
    /// materialiser trigger relies on this — every job has
    /// `target_lsn ≤ horizon` by construction, which is what makes
    /// `read_chain_durable` safe.
    #[tokio::test]
    async fn flush_horizon_advances_to_ingest_frontier() {
        let (store, schema) = make_store("mat_horizon").await;
        const LIMIT: u32 = 10;
        let (w, _t, _tl, _rx, _in_flight) =
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
        let (w, t, tl) = writer(store.clone(), FlushPolicy::default());
        seed_timeline_row(&store, t, tl).await;

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

    /// When a single key has multiple deltas in one snapshot, the
    /// dispatched `target_lsn` is the max LSN for that key — not the
    /// LSN of whichever delta nominally crossed the threshold.
    /// Pinning this matters because `read_chain_durable(key,
    /// target_lsn)` walks the chain up to `target_lsn`, so the
    /// materialised image's LSN equals what we put here.
    #[tokio::test]
    async fn flush_trigger_target_lsn_is_per_key_max() {
        let (store, schema) = make_store("mat_target_lsn").await;
        const LIMIT: u32 = 3;
        let (w, _t, _tl, mut rx, _in_flight) =
            writer_with_materializer(store.clone(), FlushPolicy::default(), LIMIT);

        // 5 deltas for k(1), all in the same snapshot. LIMIT=3
        // crosses with delta #3 at lsn 30, but the snapshot includes
        // up to lsn 50.
        for (i, lsn) in [10u64, 20, 30, 40, 50].iter().enumerate() {
            let _ = i;
            w.put(&[(k(1), Lsn(*lsn), delta_non_init())]).await.unwrap();
        }
        w.flush().await.unwrap();

        let job = rx.try_recv().expect("trigger fires post-flush");
        assert_eq!(
            job.target_lsn,
            Lsn(50),
            "target_lsn must be max LSN-for-key in the snapshot, not the \
             threshold-crossing delta's LSN",
        );

        drop_schema(&store, &schema).await;
    }

    /// Cold-key residue: a tally that crossed the threshold in a
    /// past flush but isn't in the current snapshot (no new put) must
    /// still dispatch — at `last_lsn` from the tally, since
    /// `last_lsn ≤ flush_horizon_lsn` after the prior flush. Without
    /// this, `delta_counts` grows unbounded as the workload winds
    /// down (the 8.4M-entries pathology we observed at rest).
    #[tokio::test]
    async fn cold_key_residue_dispatches_at_last_lsn() {
        let (store, schema) = make_store("mat_cold_residue").await;
        const LIMIT: u32 = 3;
        let (w, _t, _tl, mut rx, in_flight) =
            writer_with_materializer(store.clone(), FlushPolicy::default(), LIMIT);

        // Block dispatch by pre-loading in_flight with this key; the
        // first flush will see the threshold but skip dispatch. The
        // tally stays in delta_counts.
        let key_bytes = k(1);
        in_flight.lock().unwrap().insert(key_bytes);

        // Cross the threshold. After flush, last_lsn = 30 (max put
        // LSN) and that LSN is durable.
        for lsn in [10u64, 20, 30] {
            w.put(&[(k(1), Lsn(lsn), delta_non_init())]).await.unwrap();
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
        let (w, _t, _tl, mut rx, _) =
            writer_with_materializer(store.clone(), FlushPolicy::default(), 0);

        for i in 0..50 {
            w.put(&[(k(1), Lsn(10 + i), delta_non_init())])
                .await
                .unwrap();
        }
        assert!(rx.try_recv().is_err(), "limit=0 must not dispatch");

        drop_schema(&store, &schema).await;
    }

    /// Helper: insert a `timeline_state` row with `current_generation = 1`
    /// matching the generation used by the `writer` helper. Required
    /// for any test that wants `PageWriter::flush` to land its
    /// generation-gated `advance_consistent_lsn` UPDATE.
    async fn seed_timeline_row(store: &PageStore, tenant: TenantId, timeline: TimelineId) {
        store
            .insert_timeline_at_creation(&crate::page_store::TimelineRow {
                tenant_id: tenant,
                timeline_id: timeline,
                consistent_lsn: Lsn(0),
                applied_gc_cutoff_lsn: Lsn(0),
                ancestor_timeline_id: None,
                ancestor_lsn: None,
                current_generation: 1,
                pg_version: 17,
                initdb_lsn: None,
                prev_record_lsn: None,
                archived_at: None,
                deleted_at: None,
                rel_size_migration: None,
                rel_size_migrated_at: None,
                lineage: Default::default(),
            })
            .await
            .unwrap();
    }

    /// `consistent_lsn` slot must stay at its seed value through any
    /// number of `put()` calls. It only advances inside the flush
    /// success path, AFTER `try_write_rows` has landed the rows in YB
    /// AND the gen-fenced `advance_consistent_lsn` UPSERT has updated
    /// `timeline_state`. Wiring a buffer-write to the slot would
    /// reintroduce the gap-on-crash bug (`findings-ps-wal-replay-
    /// restart-bug-2026-06-04`): SK feedback / restart-resume would
    /// see an LSN whose rows aren't actually in `pageserver.pages`.
    #[tokio::test]
    async fn put_alone_does_not_advance_consistent_lsn_slot() {
        let (store, schema) = make_store("clsn_slot_no_advance_on_put").await;
        let (w, t, tl) = writer(store.clone(), FlushPolicy::default());
        seed_timeline_row(&store, t, tl).await;

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
        // `flush_publishes_remote_consistent_lsn`; the cross-check
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
    /// the generation-gated `advance_consistent_lsn` UPDATE
    /// succeeds. Walreceiver feedback reads that slot to drive
    /// safekeeper WAL truncation — regression on the publish path
    /// would silently freeze WAL retention.
    #[tokio::test]
    async fn flush_publishes_remote_consistent_lsn() {
        let (store, schema) = make_store("rclsn_publish").await;
        let (w, t, tl) = writer(store.clone(), FlushPolicy::default());
        seed_timeline_row(&store, t, tl).await;

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
        let (w, _t, _tl) = writer(store.clone(), FlushPolicy::default());

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
        let (w, _t, _tl) = writer(store.clone(), FlushPolicy::default());

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
    /// and the persisted `timeline_state.prev_record_lsn` reflects
    /// the noted prev. Restart correctness depends on this column
    /// being non-NULL; the previous shape never wrote it.
    #[tokio::test]
    async fn flush_persists_prev_record_lsn_when_noted() {
        let (store, schema) = make_store("flush_persists_prev").await;
        let (w, t, tl) = writer(store.clone(), FlushPolicy::default());
        seed_timeline_row(&store, t, tl).await;

        // Simulate `Timeline::finish_write(Lsn(42))` for the record
        // whose pages we're about to flush.
        w.note_record_lsn(Lsn(42), Lsn(40));

        w.put(&[(k(1), Lsn(42), image())]).await.unwrap();
        w.flush().await.unwrap();

        let row = store.get_timeline(t, tl).await.unwrap().unwrap();
        assert_eq!(row.consistent_lsn, Lsn(42));
        assert_eq!(
            row.prev_record_lsn,
            Some(Lsn(40)),
            "prev_record_lsn must be persisted alongside disk_consistent_lsn"
        );

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
        let (w, t, tl) = writer(store.clone(), FlushPolicy::default());
        seed_timeline_row(&store, t, tl).await;
        let seeded = store.get_timeline(t, tl).await.unwrap().unwrap();

        // Deliberately skip note_record_lsn.
        w.put(&[(k(1), Lsn(42), image())]).await.unwrap();
        w.flush().await.unwrap();

        let row = store.get_timeline(t, tl).await.unwrap().unwrap();
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
        use utils::generation::Generation;
        let (store, schema) = make_store("flush_preserve_prev").await;
        let (_w, t, tl) = writer(store.clone(), FlushPolicy::default());
        seed_timeline_row(&store, t, tl).await;

        // First advance with a real prev.
        store
            .advance_consistent_lsn(t, tl, Generation::new(1), Lsn(0x200), Some(Lsn(0x180)))
            .await
            .unwrap();
        let row = store.get_timeline(t, tl).await.unwrap().unwrap();
        assert_eq!(row.prev_record_lsn, Some(Lsn(0x180)));

        // Second advance with no pairing (None). dc_lsn moves forward
        // but prev_record_lsn must retain its prior value.
        store
            .advance_consistent_lsn(t, tl, Generation::new(1), Lsn(0x300), None)
            .await
            .unwrap();
        let row = store.get_timeline(t, tl).await.unwrap().unwrap();
        assert_eq!(row.consistent_lsn, Lsn(0x300));
        assert_eq!(
            row.prev_record_lsn,
            Some(Lsn(0x180)),
            "NULL-prev advance must not clobber the persisted value"
        );

        drop_schema(&store, &schema).await;
    }

    /// When the persisted `current_generation` has moved past ours
    /// (another pageserver took over), `PageWriter::flush` calls
    /// `task_mgr::shutdown_for_stale_generation` which cancels the
    /// global shutdown token. The whole process is expected to exit.
    /// Here we just verify the cancellation lands.
    #[tokio::test]
    async fn stale_gen_triggers_shutdown_request() {
        use tokio_util::sync::CancellationToken;

        let (store, schema) = make_store("stale_gen_shutdown").await;
        let (w, t, tl) = writer(store.clone(), FlushPolicy::default());
        seed_timeline_row(&store, t, tl).await;
        // Bump the persisted generation past the writer's gen=1.
        store.bump_generation(t, tl).await.unwrap(); // now 2
        store.bump_generation(t, tl).await.unwrap(); // now 3

        // Install a fresh global shutdown token so we can observe
        // cancellation. The OnceLock `init` no-ops on a re-set, so use
        // a child token of the existing global if it's already
        // initialized in this test binary's process; otherwise install
        // one.
        let token = CancellationToken::new();
        // Best-effort install. If another test already initialized
        // the global, this set is a no-op and we'll fall through to
        // the existing token, which we don't have a handle to — so
        // the assertion below would not fire. To make the test
        // hermetic, plumb a per-PageWriter override would be cleaner;
        // skipping that here to keep the change focused. If the
        // OnceLock rejects, we observe the side effect (warn log)
        // rather than the cancellation.
        crate::task_mgr::init_global_shutdown_token(token.clone());

        // Put + flush at our (now-stale) gen=1. The gen-gated UPDATE
        // returns `StaleGeneration` and the flush path calls
        // `shutdown_for_stale_generation`.
        w.put(&[(k(1), Lsn(10), image())]).await.unwrap();
        w.note_record_lsn(Lsn(10), Lsn(8));
        w.flush().await.unwrap();

        // Either the token we installed got cancelled (init won the
        // race) or a previously-installed token got cancelled
        // (someone else's). The first case is what we assert; the
        // second is a known non-hermetic outcome from cargo test's
        // shared process.
        assert!(
            token.is_cancelled(),
            "stale-gen detection must cancel the global shutdown token"
        );

        drop_schema(&store, &schema).await;
    }

    /// Documents the contract the `pgdatadir_mapping::put_rel_creation`
    /// idempotency check leans on: within a `policy.max_age` window
    /// of back-to-back flushes, `pageserver.pages` rows are durable
    /// at LSN X while `pageserver.timeline_state.consistent_lsn`
    /// still reads X' < X. This lag is intentional — the throttle
    /// keeps a single hot table row from absorbing one UPSERT per
    /// flush — but it means a `kill -9` mid-window leaves the next
    /// PS lifetime resuming WAL replay from X'. Re-applying records
    /// whose effects are already in YB would panic walreceiver with
    /// `RelationAlreadyExists` on the first CREATE in (X', X];
    /// `put_rel_creation` returns early when the rel is already in
    /// the keyspace to keep replay safe over this window. See the
    /// e2e `restart_wal_replay` after `restart_wal_replay_yb`
    /// in `run.sh all` for the end-to-end repro.
    #[tokio::test]
    async fn consistent_lsn_lag_under_throttle_is_expected() {
        use std::sync::atomic::Ordering::Relaxed;

        let (store, schema) = make_store("clsn_lag_expected").await;
        let (w, t, tl) = writer(store.clone(), FlushPolicy::default());
        seed_timeline_row(&store, t, tl).await;

        // First flush: the first upsert always goes through.
        w.put(&[(k(1), Lsn(0x100), image())]).await.unwrap();
        w.note_record_lsn(Lsn(0x100), Lsn(0x80));
        w.flush().await.unwrap();
        let row = store.get_timeline(t, tl).await.unwrap().unwrap();
        assert_eq!(row.consistent_lsn, Lsn(0x100));

        // Second flush within `policy.max_age`: pages durable, the
        // timeline_state UPDATE is throttled.
        w.put(&[(k(2), Lsn(0x200), image())]).await.unwrap();
        w.note_record_lsn(Lsn(0x200), Lsn(0x100));
        w.flush().await.unwrap();

        // Page row IS durable in `pages`.
        let chain = store
            .read_chain(t, tl, &key_to_bytes(k(2)), Lsn(0x300))
            .await
            .unwrap();
        let max_page_lsn = chain.iter().map(|r| r.lsn).max().unwrap_or(Lsn(0));
        assert_eq!(
            max_page_lsn,
            Lsn(0x200),
            "page row at 0x200 is durable in pageserver.pages"
        );

        // In-memory tracker knows the true LSN.
        let observed = w.consistent_lsn_observed.load(Relaxed);
        assert_eq!(Lsn(observed), Lsn(0x200));

        // Persisted column stays at the first flush's LSN — by
        // design. The replay window between this LSN and the actual
        // max durable LSN is what `put_rel_creation`'s idempotency
        // check protects against.
        let row = store.get_timeline(t, tl).await.unwrap().unwrap();
        assert_eq!(
            row.consistent_lsn,
            Lsn(0x100),
            "throttle keeps consistent_lsn at the prior upsert; \
             pageserver.pages can be ahead within the window"
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
        let (w, _t, _tl) = writer_with_cache(store.clone(), FlushPolicy::default(), 16);

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
        let (w, _t, _tl) = writer_with_cache(store.clone(), FlushPolicy::default(), 16);
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
        assert_eq!(w.stats().await.meta_cache_hits, hits_before + 1, "should be a cache hit");
        let row = result.into_values().next().expect("row present");
        assert_eq!(row.payload, payload);
        drop_schema(&store, &schema).await;
    }

    /// A second write for the same key at a higher LSN replaces the cache
    /// entry; a subsequent scan returns the newer payload.
    #[tokio::test]
    async fn meta_cache_newer_write_supersedes() {
        let (store, schema) = make_store("mc_newer_wins").await;
        let (w, _t, _tl) = writer_with_cache(store.clone(), FlushPolicy::default(), 16);

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
        let (w, _t, _tl) = writer_with_cache(store.clone(), FlushPolicy::default(), 16);

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
            w.stats().await.meta_cache_hits, hits_before,
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
        let (w, _t, _tl) = writer_with_cache(store.clone(), FlushPolicy::default(), 1);

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
        assert_eq!(w.stats().await.meta_cache_hits, hits_before, "mk(4) evicted — no cache hit");
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
        assert_eq!(w.stats().await.meta_cache_hits, hits_before + 1, "mk(5) must hit cache");
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
        let (w, _t, _tl) = writer(store.clone(), FlushPolicy::default());

        assert_eq!(w.meta_cache_len(), None, "cache must be None when disabled");
        w.put(&[(mk(6), Lsn(10), Value::Image(Bytes::from_static(b"no-cache")))])
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
}
