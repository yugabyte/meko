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

use std::sync::Arc;
use std::time::{Duration, Instant};

use anyhow::Result;
use bytes::Bytes;
use tracing::info;
use pageserver_api::key::{KEY_SIZE, Key};
use tokio::sync::RwLock;
use tokio_util::sync::CancellationToken;
use utils::bin_ser::BeSer;
use utils::id::{TenantId, TimelineId};
use utils::lsn::Lsn;
use wal_decoder::models::record::NeonWalRecord;
use wal_decoder::models::value::Value;
use wal_decoder::serialized_batch::{SerializedValueBatch, ValueMeta};

use pageserver_api::reltag::SlruKind;

use crate::page_store::{MetaRow, PageRow, PageStore, SlruPageRow};

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
    pub key: bytes::Bytes,
    pub target_lsn: Lsn,
    /// Durable chain captured at trigger time, LSN-descending, when the
    /// write-side trigger snapshots the cache while holding the writer
    /// lock. The materialiser uses this directly when it's
    /// `Some`-with-anchor, skipping the cache fast path and YB fallback
    /// entirely — the chain is by-construction complete (no other
    /// writer could have raced, no evictor had a chance to mutate it).
    ///
    /// `None` for the read-side trigger and shutdown trigger (no lock
    /// held when those fire); also `None` if the captured durable
    /// subset had no `will_init` row at trigger time (a freshly
    /// inserted page whose anchor is still pre-flush). The materialiser
    /// falls back to `read_chain_durable` in those cases.
    ///
    /// Payload `Bytes` here share their backing with the cache copies;
    /// holding the chain in the job only extends those allocations'
    /// lifetime — payload data is not duplicated.
    pub chain: Option<Vec<crate::page_store::PageRow>>,
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
    pub in_flight: Arc<std::sync::Mutex<std::collections::HashSet<bytes::Bytes>>>,
}

/// Running per-key delta tally — both count and payload bytes since
/// the last anchor. See `WriterState::delta_counts`.
#[derive(Clone, Copy, Default)]
struct DeltaTally {
    count: u32,
    payload_bytes: u32,
}

/// Top-level write buffer + flusher for one timeline.
///
/// Two `PageStore` handles, each on its own YSQL connection:
/// - `compute_page_store` serves compute-facing reads
///   (`scan_range` / `read_chain`). Idle on cache hit.
/// - `internal_page_store` serves walreceiver writes (`flush*`) and
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
    /// Storage-controller-assigned generation for this attach. Carried
    /// through to `advance_disk_consistent_lsn` so the persisting UPDATE
    /// can fail-fast if a higher generation has taken over.
    generation: utils::generation::Generation,
    compute_page_store: Arc<PageStore>,
    internal_page_store: Arc<PageStore>,
    policy: FlushPolicy,
    state: RwLock<WriterState>,
    materializer: Option<MaterializerHandle>,
    /// Highest LSN successfully reported as durable to
    /// `pageserver.timeline_state.disk_consistent_lsn`. Advances only
    /// after `try_write_rows` succeeds AND the subsequent
    /// `advance_disk_consistent_lsn` succeeds. Initialised to 0 at
    /// construction; attach seeds it from the persisted row.
    dc_lsn_watermark: std::sync::atomic::AtomicU64,
    /// Highest LSN actually durabilised in `pageserver.pages` /
    /// `slru_pages` / `meta`. Advances on every successful flush,
    /// even when the throttle skips the YB UPDATE on
    /// `disk_consistent_lsn`. Used by `force_upsert_dc_lsn_if_advanced`
    /// at shutdown to persist any tail the throttle deferred.
    dc_lsn_observed: std::sync::atomic::AtomicU64,
    /// Wall-clock timestamp of the most recent successful
    /// `advance_disk_consistent_lsn`. Used to throttle the YB UPDATE
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
    last_dc_lsn_upsert_at: std::sync::Mutex<Option<Instant>>,
    /// `Arc<AtomicLsn>` of the timeline's `visible_remote_consistent_lsn`
    /// — the value the walreceiver loop forwards to the safekeeper as
    /// `remote_consistent_lsn` in `PageserverFeedback`, which drives
    /// safekeeper WAL truncation. Bumped on every successful
    /// generation-gated `advance_disk_consistent_lsn`. Set lazily
    /// after `init_upload_queue` runs (PageWriter is constructed
    /// before the upload queue is initialized), via
    /// [`Self::set_remote_consistent_lsn_slot`]. If unset at flush
    /// time (smoke tests, init-race), the publish is a no-op; the
    /// next flush retries.
    remote_consistent_lsn_slot: std::sync::OnceLock<Arc<utils::lsn::AtomicLsn>>,
    /// Pulsed by `notify_waiters()` at the end of every successful
    /// `flush()`. `put()` blocks on this when the buffer exceeds
    /// `policy.backpressure_bytes`.
    flush_completed: tokio::sync::Notify,
    /// Total puts that had to wait at least once on `flush_completed`
    /// because the buffer was at `policy.backpressure_bytes`. Exposed
    /// for telemetry / smoke tests.
    backpressure_waits: std::sync::atomic::AtomicU64,
}

struct WriterState {
    /// Unified pre-flush buffer + post-flush LRU-ish cache for rel
    /// pages. See [`crate::unified_pages::UnifiedPages`].
    pages: crate::unified_pages::UnifiedPages,
    slru_buffer: Vec<SlruPageRow>,
    meta_buffer: Vec<MetaRow>,
    /// Bytes pending durability for SLRU + meta only. Rel-page
    /// unflushed bytes live on `pages.unflushed_bytes()`. Drives the
    /// non-rel side of the size-based flush trigger.
    bytes_in_aux_buffers: usize,
    opened_at: Option<Instant>,
    rows_written: u64,
    /// Per-key tally of consecutive non-image, non-`will_init` deltas
    /// since the last anchor for that key. Both fields advance on
    /// each non-anchor `put`; either hitting its
    /// `MaterializerHandle` threshold dispatches a job and removes
    /// the entry. Lives under the same RwLock as the buffers so the
    /// threshold check is atomic with the row push.
    delta_counts: std::collections::HashMap<bytes::Bytes, DeltaTally>,
}

#[derive(Debug, Clone, Default)]
pub struct WriterStats {
    pub rows_written: u64,
    pub bytes_in_buffer: usize,
    pub buffered_rows: usize,
    pub buffered_slru_rows: usize,
    pub buffered_meta_rows: usize,
}

impl PageWriter {
    pub fn new(
        tenant_id: TenantId,
        timeline_id: TimelineId,
        generation: utils::generation::Generation,
        compute_page_store: Arc<PageStore>,
        internal_page_store: Arc<PageStore>,
        policy: FlushPolicy,
        read_cache_bytes: usize,
    ) -> Self {
        Self {
            tenant_id,
            timeline_id,
            generation,
            compute_page_store,
            internal_page_store,
            policy,
            state: RwLock::new(WriterState {
                pages: crate::unified_pages::UnifiedPages::new(read_cache_bytes),
                slru_buffer: Vec::new(),
                meta_buffer: Vec::new(),
                bytes_in_aux_buffers: 0,
                opened_at: None,
                rows_written: 0,
                delta_counts: std::collections::HashMap::new(),
            }),
            materializer: None,
            dc_lsn_watermark: std::sync::atomic::AtomicU64::new(0),
            dc_lsn_observed: std::sync::atomic::AtomicU64::new(0),
            last_dc_lsn_upsert_at: std::sync::Mutex::new(None),
            remote_consistent_lsn_slot: std::sync::OnceLock::new(),
            flush_completed: tokio::sync::Notify::new(),
            backpressure_waits: std::sync::atomic::AtomicU64::new(0),
        }
    }

    /// Seed the in-memory `disk_consistent_lsn` watermark from the
    /// persisted timeline row at attach time. Without this, the first
    /// post-restart flush would upsert a smaller LSN than what's
    /// already in YB (a no-op write but wasted round-trip). Idempotent
    /// — uses `fetch_max` so a later flush that already advanced the
    /// watermark isn't rolled back.
    pub fn seed_dc_lsn_watermark(&self, lsn: utils::lsn::Lsn) {
        use std::sync::atomic::Ordering::Relaxed;
        let prev = self.dc_lsn_watermark.load(Relaxed);
        if lsn.0 > prev {
            // Single writer at attach; no race with the flush loop yet.
            self.dc_lsn_watermark.store(lsn.0, Relaxed);
        }
    }

    /// Borrow the internal-traffic `PageStore`. Used by
    /// `Timeline::write_timeline_row_to_yb` (Phase 5b) and other
    /// callers that need to issue side-effects (timeline-row upsert,
    /// generation bump) on the same connection the flush path uses.
    pub fn internal_page_store(&self) -> &Arc<PageStore> {
        &self.internal_page_store
    }

    /// Bypass-throttle dc_lsn upsert for shutdown. The periodic flush
    /// path throttles the YB UPDATE on the timeline row to one per
    /// `policy.max_age`; on a clean shutdown we may have skipped the
    /// last few flushes' worth of dc_lsn advances. Catch them up by
    /// upserting `dc_lsn_observed` if it exceeds the watermark.
    /// No-op if the watermark is already at or past the observed
    /// value.
    pub async fn force_upsert_dc_lsn_if_advanced(&self) -> Result<()> {
        use std::sync::atomic::Ordering::Relaxed;
        let observed = self.dc_lsn_observed.load(Relaxed);
        let watermark = self.dc_lsn_watermark.load(Relaxed);
        if observed <= watermark {
            return Ok(());
        }
        let lsn = utils::lsn::Lsn(observed);
        match self
            .internal_page_store
            .advance_disk_consistent_lsn(self.tenant_id, self.timeline_id, self.generation, lsn)
            .await?
        {
            crate::page_store::AdvanceDcLsnOutcome::Updated => {
                self.dc_lsn_watermark.store(observed, Relaxed);
                *self.last_dc_lsn_upsert_at.lock().unwrap() = Some(Instant::now());
                self.publish_remote_consistent_lsn(lsn);
            }
            crate::page_store::AdvanceDcLsnOutcome::StaleGeneration { live_generation } => {
                // Shutdown path: another PS owns the timeline. Don't
                // trigger a global shutdown here (we're already in
                // one), but surface the fact so the operator can see
                // why the shutdown-time catch-up failed.
                tracing::warn!(
                    tenant_id = %self.tenant_id,
                    timeline_id = %self.timeline_id,
                    our_generation = ?self.generation,
                    live_generation,
                    "force_upsert_dc_lsn_if_advanced fenced by newer generation — \
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
    pub fn set_remote_consistent_lsn_slot(&self, slot: Arc<utils::lsn::AtomicLsn>) {
        let _ = self.remote_consistent_lsn_slot.set(slot);
    }

    /// Bump the safekeeper-truncation slot. Called from the gen-gated
    /// flush success path; the value is already durable in YB
    /// (`pageserver.timeline_state.disk_consistent_lsn`) so the
    /// advertise can't lie. No-op if the slot hasn't been installed
    /// yet (init race / smoke tests).
    fn publish_remote_consistent_lsn(&self, lsn: utils::lsn::Lsn) {
        if let Some(slot) = self.remote_consistent_lsn_slot.get() {
            slot.fetch_max(lsn);
        }
    }

    /// Install the image-materialiser handle. Counter logic in `put`
    /// is a no-op until this is called. Must be called before any
    /// `put` traffic for that traffic to be observed.
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

                {
                    let s = self.state.read().await;
                    let pending = s.pages.unflushed_bytes() + s.bytes_in_aux_buffers;
                    if pending < self.policy.backpressure_bytes {
                        break;
                    }
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
        // Collect materialisation jobs while holding the lock; dispatch
        // them after release so the tx.send (channel-full case) can't
        // block another writer waiting on `state`.
        let trigger = self
            .materializer
            .as_ref()
            .map(|m| (m.count_limit, m.payload_bytes_limit, m.in_flight.clone()));
        let mut to_materialize: Vec<MaterializeJob> = Vec::new();
        {
            let mut s = self.state.write().await;
            if s.opened_at.is_none() {
                s.opened_at = Some(Instant::now());
            }
            for row in rel_rows {
                let key = row.key.clone();
                let lsn = row.lsn;
                let resets_chain = row.is_image || row.will_init;
                let payload_len = row.payload.len() as u32;
                s.pages.push(row);
                s.rows_written += 1;

                if resets_chain {
                    s.delta_counts.remove(&key);
                } else if let Some((count_limit, bytes_limit, in_flight)) = trigger.as_ref()
                    && (*count_limit > 0 || *bytes_limit > 0)
                {
                    let tally = s.delta_counts.entry(key.clone()).or_default();
                    tally.count = tally.count.saturating_add(1);
                    tally.payload_bytes = tally.payload_bytes.saturating_add(payload_len);
                    // Whichever threshold lands first dispatches. A
                    // disabled threshold (zero) is treated as "infinity"
                    // so it never alone fires.
                    let count_hit = *count_limit > 0 && tally.count >= *count_limit;
                    let bytes_hit = *bytes_limit > 0 && tally.payload_bytes >= *bytes_limit;
                    if count_hit || bytes_hit {
                        // Only enqueue + reset the counter if this key
                        // doesn't already have a job in flight. If it
                        // does, leave the counter at >= threshold so the
                        // first delta past the in-flight job's completion
                        // immediately re-fires — at the *current* LSN,
                        // not stale intermediate LSNs we'd waste walredo
                        // cycles on.
                        let inserted = in_flight.lock().unwrap().insert(key.clone());
                        if inserted {
                            s.delta_counts.remove(&key);
                            // Snapshot the durable chain while the
                            // writer lock is still held — no evictor
                            // or other put can race here, so the chain
                            // is by-construction complete. Lets the
                            // materialiser skip its cache fast path and
                            // (when an anchor is present) the YB
                            // fallback. `lookup_chain_durable` clones
                            // PageRows; payload Bytes refcount-bump
                            // rather than duplicate. See `MaterializeJob`
                            // for when this is `None`.
                            let chain = s.pages.lookup_chain_durable(&key, lsn);
                            let chain = chain_with_anchor(chain);
                            to_materialize.push(MaterializeJob {
                                key,
                                target_lsn: lsn,
                                chain,
                            });
                        }
                    }
                }
            }
            for row in slru_rows {
                s.bytes_in_aux_buffers += slru_row_size(&row);
                s.slru_buffer.push(row);
                s.rows_written += 1;
            }
            for row in meta_rows {
                s.bytes_in_aux_buffers += meta_row_size(&row);
                s.meta_buffer.push(row);
                s.rows_written += 1;
            }
        }

        if !to_materialize.is_empty()
            && let Some(m) = self.materializer.as_ref()
        {
            let dispatched = to_materialize.len();
            let queue_cap = m.tx.max_capacity();
            let queue_remaining_before = m.tx.capacity();
            let mut waited_for_capacity = false;
            for job in to_materialize {
                // Blocking send: backpressure the walreceiver if the
                // materialiser can't keep up. Silent try_send drops
                // were causing deep chains to accumulate during write
                // bursts and stay deep forever once ingest went idle
                // (no later put would re-trigger after the counter
                // cleared). Receiver lives as long as the timeline, so
                // SendError here means timeline shutdown — undo the
                // in_flight reservation for this and remaining jobs so
                // a later restart's trigger isn't blocked.
                if !waited_for_capacity && m.tx.capacity() == 0 {
                    waited_for_capacity = true;
                    crate::metrics::PAGEWRITER_BACKPRESSURE_WAITS
                        .with_label_values(&["materializer"])
                        .inc();
                }
                let key_for_rollback = job.key.clone();
                if let Err(e) = m.tx.send(job).await {
                    tracing::debug!(
                        "materialiser send dropped (receiver closed): {e}"
                    );
                    m.in_flight.lock().unwrap().remove(&key_for_rollback);
                    break;
                }
            }
            // Sample queue depth gauge after dispatch. Captures the
            // post-burst level; reads between bursts will tail toward
            // 0 as the materialiser drains.
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
        Ok(())
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
    ) -> Result<std::collections::HashMap<Key, Vec<PageRow>>> {
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
            .scan_range(self.tenant_id, self.timeline_id, range.clone(), target_lsn)
            .await?;
        let t_yb = t0.elapsed();
        let start_bytes = key_to_bytes(range.start);
        let end_bytes = key_to_bytes(range.end);
        let t_lock_start = Instant::now();
        let cached: std::collections::HashMap<bytes::Bytes, Vec<PageRow>> = {
            let s = self.state.read().await;
            s.pages.lookup_range(&start_bytes, &end_bytes, target_lsn)
        };
        let t_cache = t_lock_start.elapsed();
        let persisted_rows: usize = persisted.values().map(|v| v.len()).sum();
        let cached_rows: usize = cached.values().map(|v| v.len()).sum();
        let t_merge_start = Instant::now();
        let mut out: std::collections::HashMap<Key, Vec<PageRow>> =
            std::collections::HashMap::with_capacity(persisted.len() + cached.len());
        for (key_bytes, chain) in persisted {
            let k = Key::from_slice(&key_bytes);
            out.insert(k, chain);
        }
        for (key_bytes, chain) in cached {
            let k = Key::from_slice(&key_bytes);
            out.entry(k).or_default().extend(chain);
        }
        for (key, chain) in out.iter_mut() {
            // Descending LSN; on ties prefer the image (see `read_chain`
            // for the rationale).
            chain.sort_by(|a, b| {
                b.lsn
                    .cmp(&a.lsn)
                    .then_with(|| b.is_image.cmp(&a.is_image))
            });
            chain.dedup_by(|a, b| a.lsn == b.lsn);
            if let Some(idx) = chain.iter().position(|r| r.will_init) {
                chain.truncate(idx + 1);
            }
            // Per-key read-side trigger: a chain longer than
            // count_limit means the write trigger lost a job (PS
            // crash, queue overflow). Enqueue a fresh materialise so
            // the next read of the same key is fast.
            let key_bytes = key_to_bytes(*key);
            self.maybe_dispatch_read_side_materialize(&key_bytes, target_lsn, chain.len());
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
        key_bytes: &bytes::Bytes,
        target_lsn: Lsn,
        chain_len: usize,
    ) {
        let Some(m) = self.materializer.as_ref() else {
            return;
        };
        if try_dispatch_read_side(
            key_bytes,
            target_lsn,
            chain_len,
            m.count_limit,
            &m.in_flight,
            &m.tx,
        ) {
            crate::metrics::MATERIALIZER_READ_SIDE_DISPATCHES.inc();
        }
    }

    pub async fn read_chain(&self, key: Key, target_lsn: Lsn) -> Result<Vec<PageRow>> {
        let key_bytes = key_to_bytes(key);
        // Cache-only fast path: if the cache holds a complete chain
        // (image-anchor reachable from target_lsn, no eviction in
        // between), serve from RAM and skip YB entirely. This is the
        // common case for hot pages after the materialiser has run.
        {
            let s = self.state.read().await;
            if let Some(chain) = s.pages.lookup_chain_if_complete(&key_bytes, target_lsn) {
                return Ok(chain);
            }
        }
        let cached: Vec<PageRow> = {
            let s = self.state.read().await;
            s.pages.lookup_chain(&key_bytes, target_lsn)
        };
        let persisted = self
            .compute_page_store
            .read_chain(self.tenant_id, self.timeline_id, &key_bytes, target_lsn)
            .await?;
        let mut merged = cached;
        merged.extend(persisted);
        // Descending LSN; on ties (materialiser INSERTed an image at
        // the same LSN as an existing delta), the image must sort
        // first so dedup keeps it and discards the now-superseded
        // delta. Otherwise the chain can lose its anchor.
        merged.sort_by(|a, b| {
            b.lsn
                .cmp(&a.lsn)
                .then_with(|| b.is_image.cmp(&a.is_image))
        });
        merged.dedup_by(|a, b| a.lsn == b.lsn);
        if let Some(idx) = merged.iter().position(|r| r.will_init) {
            merged.truncate(idx + 1);
        }
        self.maybe_dispatch_read_side_materialize(&key_bytes, target_lsn, merged.len());
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
    ) -> Result<Vec<PageRow>> {
        let key_bytes = key_to_bytes(key);
        // Cache-only fast path (durable-only variant for the
        // materialiser): same completeness criterion as `read_chain`,
        // but restricted to durable rows.
        {
            let s = self.state.read().await;
            if let Some(chain) =
                s.pages.lookup_chain_durable_if_complete(&key_bytes, target_lsn)
            {
                return Ok(chain);
            }
        }
        let cached: Vec<PageRow> = {
            let s = self.state.read().await;
            s.pages.lookup_chain_durable(&key_bytes, target_lsn)
        };
        // Materialiser-side cache miss → the internal conn (same one
        // the materialiser's write path uses). Keeping this off
        // `compute_page_store` is the whole point of the split: a
        // long materialiser walredo-and-INSERT can't stall compute.
        let persisted = self
            .internal_page_store
            .read_chain(self.tenant_id, self.timeline_id, &key_bytes, target_lsn)
            .await?;
        let mut merged = cached;
        merged.extend(persisted);
        // Descending LSN; on ties (materialiser INSERTed an image at
        // the same LSN as an existing delta), the image must sort
        // first so dedup keeps it and discards the now-superseded
        // delta. Otherwise the chain can lose its anchor.
        merged.sort_by(|a, b| {
            b.lsn
                .cmp(&a.lsn)
                .then_with(|| b.is_image.cmp(&a.is_image))
        });
        merged.dedup_by(|a, b| a.lsn == b.lsn);
        if let Some(idx) = merged.iter().position(|r| r.will_init) {
            merged.truncate(idx + 1);
        }
        Ok(merged)
    }

    /// Install a freshly-materialised image into the cache. Call AFTER
    /// `PageStore::put_pages([image])` succeeds. Drops the deltas the
    /// image supersedes for that key (durable, lsn ≤ image.lsn) and
    /// indexes the new image as a durable cache entry; subsequent
    /// `read_chain` / `read_chain_durable` find the image first and
    /// don't walk the chain back.
    pub async fn install_materialized_image(&self, image: PageRow) {
        let t_acquire = Instant::now();
        let mut s = self.state.write().await;
        let t_held_start = Instant::now();
        let key = image.key.clone();
        let lsn = image.lsn;
        let t_trim_start = Instant::now();
        s.pages.trim_key_up_to(&key, lsn);
        let t_trim = t_trim_start.elapsed();
        let t_push_start = Instant::now();
        s.pages.push_durable(image);
        let t_push = t_push_start.elapsed();
        let t_evict_start = Instant::now();
        s.pages.evict_to_capacity();
        let t_evict = t_evict_start.elapsed();
        let t_held = t_held_start.elapsed();
        let t_acq = t_held_start.duration_since(t_acquire);
        if t_held.as_millis() >= 2 || t_acq.as_millis() >= 5 {
            info!(
                tag = "install_image",
                acquire_ms = t_acq.as_millis() as u64,
                held_ms = t_held.as_millis() as u64,
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
        // Snapshot under the lock. UnifiedPages snapshots the unflushed
        // prefix without removing it (rows stay visible to concurrent
        // reads during the YB round-trip — critical for walreceiver's
        // RMW on rel-size sentinels). After the INSERT succeeds we
        // flip those rows from unflushed → durable in one cursor pass.
        let (rel_count, slru_count, meta_count, rel_rows, slru_rows, meta_rows) = {
            let s = self.state.read().await;
            let rel_rows = s.pages.snapshot_unflushed(None);
            (
                rel_rows.len(),
                s.slru_buffer.len(),
                s.meta_buffer.len(),
                rel_rows,
                s.slru_buffer.clone(),
                s.meta_buffer.clone(),
            )
        };
        if rel_count == 0 && slru_count == 0 && meta_count == 0 {
            return Ok(());
        }

        // INSERT to YB outside the lock so concurrent puts and reads
        // aren't blocked by the round-trip.
        let res = self.try_write_rows(&rel_rows, &slru_rows, &meta_rows).await;

        if res.is_ok() {
            // Compute the max LSN we just durabilised across all three
            // tables — that's the new candidate for
            // `disk_consistent_lsn`. We compute it from the snapshot
            // we just flushed; the buffer is unchanged at this point.
            let max_flushed_lsn = rel_rows
                .iter()
                .map(|r| r.lsn.0)
                .chain(slru_rows.iter().map(|r| r.lsn.0))
                .chain(meta_rows.iter().map(|r| r.lsn.0))
                .max();
            {
                let mut s = self.state.write().await;
                // Promote rel rows in-place (no copy), then opportunistically
                // shrink the cache if it's over capacity.
                s.pages.mark_first_n_flushed(rel_count);
                s.pages.evict_to_capacity();
                // Aux buffers drain as before.
                let aux_drop_bytes: usize = s
                    .slru_buffer
                    .iter()
                    .take(slru_count)
                    .map(slru_row_size)
                    .sum::<usize>()
                    + s.meta_buffer
                        .iter()
                        .take(meta_count)
                        .map(meta_row_size)
                        .sum::<usize>();
                s.bytes_in_aux_buffers = s.bytes_in_aux_buffers.saturating_sub(aux_drop_bytes);
                s.slru_buffer.drain(0..slru_count);
                s.meta_buffer.drain(0..meta_count);
                if s.pages.unflushed_count() == 0
                    && s.slru_buffer.is_empty()
                    && s.meta_buffer.is_empty()
                {
                    s.opened_at = None;
                }
            }
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
            if let Some(new_lsn) = max_flushed_lsn {
                use std::sync::atomic::Ordering::Relaxed;
                // Always advance `dc_lsn_observed` — that's the
                // "max durable in YB pages/slru/meta" tracker. The
                // throttle below only gates the YB UPDATE on the
                // timeline row, not the underlying durability.
                self.dc_lsn_observed.fetch_max(new_lsn, Relaxed);
                let prev = self.dc_lsn_watermark.load(Relaxed);
                let elapsed_ok = {
                    let guard = self.last_dc_lsn_upsert_at.lock().unwrap();
                    match *guard {
                        None => true, // first upsert always goes through
                        Some(prev_at) => prev_at.elapsed() >= self.policy.max_age,
                    }
                };
                if new_lsn > prev && elapsed_ok {
                    let lsn = utils::lsn::Lsn(new_lsn);
                    match self
                        .internal_page_store
                        .advance_disk_consistent_lsn(
                            self.tenant_id,
                            self.timeline_id,
                            self.generation,
                            lsn,
                        )
                        .await
                    {
                        Ok(crate::page_store::AdvanceDcLsnOutcome::Updated) => {
                            self.dc_lsn_watermark.store(new_lsn, Relaxed);
                            *self.last_dc_lsn_upsert_at.lock().unwrap() = Some(Instant::now());
                            // Publish the just-advanced LSN as the
                            // safekeeper-truncation bound. The walreceiver
                            // loop forwards this slot to the safekeeper
                            // in PageserverFeedback.remote_consistent_lsn,
                            // bounding `truncate_lsn` / `peer_horizon_lsn`.
                            // The gen-fenced UPDATE above is the
                            // durability commit point — this publish
                            // only echoes a fact that's already in YB.
                            self.publish_remote_consistent_lsn(lsn);
                        }
                        Ok(crate::page_store::AdvanceDcLsnOutcome::StaleGeneration {
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
                                "advance_disk_consistent_lsn failed: {e:#}"
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
        rel_rows: &[PageRow],
        slru_rows: &[SlruPageRow],
        meta_rows: &[MetaRow],
    ) -> Result<()> {
        // All flush writes go via the internal conn. Compute getPage
        // never queues behind a 25 MiB INSERT.
        if !rel_rows.is_empty() {
            self.internal_page_store
                .put_pages(self.tenant_id, self.timeline_id, rel_rows)
                .await?;
        }
        if !slru_rows.is_empty() {
            self.internal_page_store
                .put_slru_pages(self.tenant_id, self.timeline_id, slru_rows)
                .await?;
        }
        if !meta_rows.is_empty() {
            self.internal_page_store
                .put_meta(self.tenant_id, self.timeline_id, meta_rows)
                .await?;
        }
        Ok(())
    }

    pub async fn should_flush(&self) -> bool {
        let s = self.state.read().await;
        let unflushed_bytes = s.pages.unflushed_bytes() + s.bytes_in_aux_buffers;
        if unflushed_bytes >= self.policy.max_bytes {
            return true;
        }
        if let Some(at) = s.opened_at
            && at.elapsed() >= self.policy.max_age
        {
            return true;
        }
        false
    }

    pub async fn stats(&self) -> WriterStats {
        let s = self.state.read().await;
        let bytes_in_buffer = s.pages.unflushed_bytes() + s.bytes_in_aux_buffers;
        // Side-effect: refresh the global gauges so a periodic
        // Prometheus scrape reflects the current depth. We sample
        // here rather than at every put/flush to keep the update off
        // the hot ingest path; the next scrape after a flush picks
        // up the change.
        let delta_counts_entries = s.delta_counts.len() as u64;
        let by_key_entries = s.pages.by_key_entries() as u64;
        crate::metrics::PAGEWRITER_UNFLUSHED_BYTES.set(bytes_in_buffer as u64);
        crate::metrics::PAGEWRITER_DELTA_COUNTS_ENTRIES.set(delta_counts_entries);
        crate::metrics::PAGEWRITER_DELTA_COUNTS_APPROX_BYTES
            .set(delta_counts_entries * crate::metrics::DELTA_COUNTS_BYTES_PER_ENTRY);
        crate::metrics::UNIFIED_PAGES_BY_KEY_ENTRIES.set(by_key_entries);
        WriterStats {
            rows_written: s.rows_written,
            bytes_in_buffer,
            buffered_rows: s.pages.unflushed_count(),
            buffered_slru_rows: s.slru_buffer.len(),
            buffered_meta_rows: s.meta_buffer.len(),
        }
    }

    /// Total `put()` calls that had to wait on backpressure since
    /// startup. Telemetry only.
    pub fn backpressure_wait_count(&self) -> u64 {
        self.backpressure_waits
            .load(std::sync::atomic::Ordering::Relaxed)
    }

    /// Read the latest meta value for `key` at LSN ≤ `target_lsn`,
    /// blending buffer + PageStore.
    pub async fn read_meta(&self, key: Key, target_lsn: Lsn) -> Result<Option<MetaRow>> {
        let key_bytes = key_to_bytes(key);
        let buffered = {
            let s = self.state.read().await;
            s.meta_buffer
                .iter()
                .filter(|r| r.key == key_bytes && r.lsn <= target_lsn)
                .max_by_key(|r| r.lsn)
                .cloned()
        };
        let persisted = self
            .compute_page_store
            .read_meta(self.tenant_id, self.timeline_id, &key_bytes, target_lsn)
            .await?;
        Ok(match (buffered, persisted) {
            (Some(b), Some(p)) => Some(if b.lsn >= p.lsn { b } else { p }),
            (Some(b), None) => Some(b),
            (None, p) => p,
        })
    }

    /// Range scan over meta keys. Returns the latest version per key.
    pub async fn scan_meta_range(
        &self,
        range: std::ops::Range<Key>,
        target_lsn: Lsn,
    ) -> Result<std::collections::HashMap<bytes::Bytes, MetaRow>> {
        let mut out = self
            .compute_page_store
            .scan_meta_range(self.tenant_id, self.timeline_id, range.clone(), target_lsn)
            .await?;
        let s = self.state.read().await;
        for row in s.meta_buffer.iter() {
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
            let s = self.state.read().await;
            s.slru_buffer
                .iter()
                .filter(|r| {
                    r.kind == kind && r.segno == segno && r.blknum == blknum
                        && r.lsn <= target_lsn
                })
                .cloned()
                .collect()
        };
        let persisted = self
            .compute_page_store
            .read_slru_chain(self.tenant_id, self.timeline_id, kind, segno, blknum, target_lsn)
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
            .scan_slru_range(self.tenant_id, self.timeline_id, kind, segno, blknum_range.clone(), target_lsn)
            .await?;
        let buffered: Vec<SlruPageRow> = {
            let s = self.state.read().await;
            s.slru_buffer
                .iter()
                .filter(|r| {
                    r.kind == kind && r.segno == segno
                        && r.blknum >= blknum_range.start && r.blknum < blknum_range.end
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
                if let Err(e) = w.force_upsert_dc_lsn_if_advanced().await {
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

/// Truncate the LSN-descending chain at the LATEST `will_init` row
/// (image or init-delta) and return the prefix, or `None` if no
/// anchor exists. Used at write-side trigger time so the captured
/// snapshot matches the shape `parse_chain` expects: exactly one
/// anchor at the tail, with only deltas above it. Without truncation
/// an older image sitting beneath a newer anchor would be fed to
/// `NeonWalRecord::des` and surface as a "deserialize error" at
/// materialisation time.
fn chain_with_anchor(mut chain: Vec<PageRow>) -> Option<Vec<PageRow>> {
    let idx = chain.iter().position(|r| r.will_init)?;
    chain.truncate(idx + 1);
    Some(chain)
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
                        crate::metrics::CORRECTNESS_ERRORS
                            .with_label_values(&["write_path_roundtrip"])
                            .inc();
                        tracing::error!(
                            key = %key,
                            %lsn,
                            payload_len = bytes.len(),
                            "WRITE-PATH ROUNDTRIP MISMATCH: ser→des decoded to \
                             a different NeonWalRecord. Serializer is the \
                             corruption source. orig={:?} decoded={:?}",
                            rec,
                            decoded,
                        );
                    }
                    Err(e) => {
                        crate::metrics::CORRECTNESS_ERRORS
                            .with_label_values(&["write_path_roundtrip"])
                            .inc();
                        tracing::error!(
                            key = %key,
                            %lsn,
                            payload_len = bytes.len(),
                            "WRITE-PATH ROUNDTRIP DECODE FAILED on freshly \
                             serialised bytes — bincode mismatch in our own \
                             code: {e:#}"
                        );
                    }
                }
            }
            (false, will_init, bytes)
        }
    };
    Ok(PageRow {
        key: key_to_bytes(key),
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
    key_bytes: &bytes::Bytes,
    target_lsn: Lsn,
    chain_len: usize,
    count_limit: u32,
    in_flight: &std::sync::Mutex<std::collections::HashSet<bytes::Bytes>>,
    tx: &tokio::sync::mpsc::Sender<MaterializeJob>,
) -> bool {
    if count_limit == 0 || chain_len <= count_limit as usize {
        return false;
    }
    let inserted = in_flight.lock().unwrap().insert(key_bytes.clone());
    if !inserted {
        return false;
    }
    if tx
        .try_send(MaterializeJob {
            key: key_bytes.clone(),
            target_lsn,
            // Read-side trigger doesn't hold the writer lock, so no
            // race-free snapshot is available; materialiser falls
            // back to `read_chain_durable` for these.
            chain: None,
        })
        .is_err()
    {
        in_flight.lock().unwrap().remove(key_bytes);
        return false;
    }
    true
}

#[cfg(test)]
mod tests {
    use super::*;

    use bytes::Bytes;
    use remote_storage::YbStoreConfig;
    use utils::id::{TenantId, TimelineId};
    use wal_decoder::models::record::NeonWalRecord;

    use crate::page_store::{PageStore, PageStoreConfig};

    fn test_yb_config() -> YbStoreConfig {
        YbStoreConfig {
            host: std::env::var("YB_TEST_HOST").unwrap_or_else(|_| "127.0.0.1".to_string()),
            port: std::env::var("YB_TEST_PORT")
                .ok()
                .and_then(|s| s.parse().ok())
                .unwrap_or(5433),
            user: std::env::var("YB_TEST_USER").unwrap_or_else(|_| "yugabyte".to_string()),
            password_env: None,
            dbname: std::env::var("YB_TEST_DB").unwrap_or_else(|_| "yb_storage".to_string()),
            table_name: "pages".to_string(),
            prefix: Some("test/".to_string()),
            sslmode: None,
            sslrootcert: None,
            concurrency_limit: std::num::NonZeroUsize::new(8).unwrap(),
        }
    }

    /// Shared-schema test scaffolding — see the matching comment in
    /// `page_store.rs::tests::SHARED_STORE`. All tests in this binary
    /// share one schema, isolated by `rand_tenant()`/`rand_timeline()`.
    /// Per-test schema DDL was costing 5–10 s of YB tablet
    /// coordination apiece; sharing amortizes that to once per binary.
    static SHARED_STORE: tokio::sync::OnceCell<Arc<PageStore>> = tokio::sync::OnceCell::const_new();
    const SHARED_SCHEMA: &str = "pw_test_shared";

    async fn make_store(_suffix: &str) -> (Arc<PageStore>, String) {
        let store = SHARED_STORE
            .get_or_init(|| async {
                let cfg = PageStoreConfig {
                    yb: test_yb_config(),
                    schema: SHARED_SCHEMA.to_string(),
                };
                let pre = PageStore::connect(cfg.clone(), "pw_test_setup")
                    .await
                    .expect("connect to local YB");
                let _ = pre
                    .test_only_execute(&format!(
                        "DROP SCHEMA IF EXISTS \"{SHARED_SCHEMA}\" CASCADE"
                    ))
                    .await;
                let store = PageStore::connect(cfg, "pw_test")
                    .await
                    .expect("connect to local YB");
                store.ensure_schema().await.expect("ensure_schema");
                Arc::new(store)
            })
            .await
            .clone();
        (store, SHARED_SCHEMA.to_string())
    }

    async fn drop_schema(_store: &PageStore, _schema: &str) {
        // No-op: shared schema; cleanup happens at the start of the
        // next test-binary invocation via the OnceCell init.
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

    fn writer(
        store: Arc<PageStore>,
        policy: FlushPolicy,
    ) -> (PageWriter, TenantId, TimelineId) {
        let t = TenantId::generate();
        let tl = TimelineId::generate();
        (
            // Tests don't exercise the connection split — pass the
            // same PageStore arc as both compute + internal.
            PageWriter::new(t, tl, utils::generation::Generation::new(1), store.clone(), store, policy, 64 * 1024 * 1024),
            t,
            tl,
        )
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
        Arc<std::sync::Mutex<std::collections::HashSet<bytes::Bytes>>>,
    ) {
        // Default fixture: count-only trigger (payload-bytes disabled)
        // so existing tests stay deterministic on count-based thresholds.
        writer_with_materializer_thresholds(store, policy, limit, 0)
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
    ) -> (
        PageWriter,
        TenantId,
        TimelineId,
        tokio::sync::mpsc::Receiver<MaterializeJob>,
        Arc<std::sync::Mutex<std::collections::HashSet<bytes::Bytes>>>,
    ) {
        let t = TenantId::generate();
        let tl = TimelineId::generate();
        let mut w = PageWriter::new(t, tl, utils::generation::Generation::new(1), store.clone(), store, policy, 64 * 1024 * 1024);
        let (tx, rx) = tokio::sync::mpsc::channel(64);
        let in_flight = Arc::new(std::sync::Mutex::new(std::collections::HashSet::new()));
        w.set_materializer(MaterializerHandle {
            count_limit,
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

    fn row(lsn: u64, is_image: bool, will_init: bool) -> PageRow {
        PageRow {
            key: key_to_bytes(k(1)),
            lsn: Lsn(lsn),
            is_image,
            will_init,
            payload: Bytes::from_static(b"dummy"),
        }
    }

    /// `chain_with_anchor` must truncate at the LATEST `will_init` row so
    /// `parse_chain` only ever sees one anchor at the tail. Regression for
    /// the corruption-repro on 2026-06-02 where an older image sitting
    /// beneath a newer image in a captured chain reached `parse_chain` and
    /// was fed to `NeonWalRecord::des` as if it were a delta.
    #[test]
    fn chain_with_anchor_truncates_at_latest_will_init() {
        // LSN-desc, two anchors: keep the newest, drop everything below it.
        let chain = vec![
            row(500, false, false),
            row(400, true, true),   // newest anchor (post-truncation tail)
            row(300, false, false),
            row(200, true, true),   // older anchor — must not leak through
            row(100, false, false),
        ];
        let truncated = chain_with_anchor(chain).expect("anchor present");
        assert_eq!(
            truncated.iter().map(|r| r.lsn.0).collect::<Vec<_>>(),
            vec![500, 400],
        );
        assert!(truncated.last().unwrap().will_init);
    }

    #[test]
    fn chain_with_anchor_none_when_no_anchor() {
        let chain = vec![row(500, false, false), row(400, false, false)];
        assert!(chain_with_anchor(chain).is_none());
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
        ]).await.unwrap();
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
        let slru_key = pageserver_api::key::slru_block_to_key(
            pageserver_api::reltag::SlruKind::Clog,
            7,
            42,
        );

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
        let entries: Vec<(Key, Lsn, Value)> =
            (0..50).map(|i| (k(i), Lsn(10), image())).collect();
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
            .map(|i| (k(1), Lsn(10 + 10 * i), if i == 0 { image() } else { delta() }))
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
        let w = Arc::new(PageWriter::new(t, tl, utils::generation::Generation::new(1), store.clone(), store.clone(), policy, 64 * 1024 * 1024));
        let task = w.spawn_flusher(Duration::from_millis(15));
        w.put(&[(k(1), Lsn(10), image())]).await.unwrap();
        tokio::time::sleep(Duration::from_millis(250)).await;
        let stats = w.stats().await;
        assert_eq!(stats.buffered_rows, 0, "stats: {stats:?}");
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
        let w = Arc::new(PageWriter::new(t, tl, utils::generation::Generation::new(1), store.clone(), store.clone(), policy, 64 * 1024 * 1024));
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

        // (LIMIT - 1) deltas: nothing queued yet.
        for i in 0..(LIMIT - 1) {
            w.put(&[(k(1), Lsn(10 + i as u64), delta_non_init())])
                .await
                .unwrap();
            assert!(
                rx.try_recv().is_err(),
                "premature dispatch after {} deltas",
                i + 1
            );
        }
        // LIMITth delta: dispatch.
        w.put(&[(k(1), Lsn(10 + LIMIT as u64), delta_non_init())])
            .await
            .unwrap();
        let job = rx
            .try_recv()
            .expect("dispatched on hitting threshold");
        assert_eq!(job.target_lsn, Lsn(10 + LIMIT as u64));
        assert_eq!(job.key.as_ref(), key_to_bytes(k(1)).as_ref());
        // Simulate the materialiser finishing this batch (production:
        // `ImageMaterializer::materialize_batch` does this after the
        // INSERT). Until this happens the in_flight dedup blocks
        // re-enqueue for the same key.
        in_flight.lock().unwrap().remove(&job.key);

        // Counter reset: another (LIMIT-1) deltas must not re-dispatch.
        for i in 0..(LIMIT - 1) {
            w.put(&[(k(1), Lsn(100 + i as u64), delta_non_init())])
                .await
                .unwrap();
        }
        assert!(rx.try_recv().is_err(), "dispatch before second threshold");
        w.put(&[(k(1), Lsn(200), delta_non_init())]).await.unwrap();
        let _ = rx.try_recv().expect("dispatch on second threshold");

        drop_schema(&store, &schema).await;
    }

    /// Write-side trigger captures the durable chain at dispatch time
    /// when the chain has a durable anchor.
    ///
    /// Regression for commit `e50423234` (captured chain at materialiser
    /// trigger): the materialiser used to re-read the chain via
    /// `read_chain_durable`, which forced a YB round-trip whenever the
    /// cache fast path bailed on `evicted_below`. Now the trigger
    /// snapshots the durable chain under the writer lock (where it is
    /// by construction complete) and embeds it in `MaterializeJob.chain`.
    #[tokio::test]
    async fn trigger_captures_durable_chain_with_anchor() {
        let (store, schema) = make_store("mat_capture_chain").await;
        const LIMIT: u32 = 10;
        let (w, _t, _tl, mut rx, _in_flight) =
            writer_with_materializer(store.clone(), FlushPolicy::default(), LIMIT);

        // Anchor + 9 deltas, then flush so all become durable.
        w.put(&[(k(1), Lsn(10), delta_will_init())]).await.unwrap();
        for i in 0..(LIMIT - 1) {
            w.put(&[(k(1), Lsn(20 + i as u64), delta_non_init())])
                .await
                .unwrap();
        }
        w.flush().await.unwrap();

        // The LIMIT-th post-anchor delta fires the trigger. At this
        // point the new delta is unflushed but the anchor + the
        // earlier 9 deltas are durable. The captured chain should hold
        // those 10 durable rows.
        w.put(&[(k(1), Lsn(100), delta_non_init())]).await.unwrap();
        let job = rx.try_recv().expect("dispatched on hitting threshold");
        let chain = job
            .chain
            .as_ref()
            .expect("captured chain populated when anchor is durable");

        // LSN-descending; the will_init anchor is at the end.
        let lsns: Vec<u64> = chain.iter().map(|r| r.lsn.0).collect();
        let lowest = *lsns.last().expect("non-empty chain");
        assert_eq!(lowest, 10, "anchor at lsn 10 must be in the captured chain");
        assert!(
            chain.last().unwrap().will_init,
            "the lowest captured row must be a will_init anchor"
        );
        // The just-pushed (unflushed) row must NOT appear in the
        // durable snapshot, even though target_lsn covers it.
        assert!(
            !lsns.contains(&100),
            "the pre-flush row pushed at trigger time must be excluded \
             from the durable snapshot — captured chain is durable-only"
        );

        drop_schema(&store, &schema).await;
    }

    /// Write-side trigger captures `chain: None` when the durable chain
    /// has no `will_init` anchor at trigger time — the materialiser
    /// can't act on an anchorless captured chain, and forcing it to
    /// would cost a snapshot allocation for nothing. Materialiser
    /// falls back to `read_chain_durable` in this case.
    ///
    /// Same regression as `trigger_captures_durable_chain_with_anchor`,
    /// covering the no-durable-anchor branch of the `chain_with_anchor`
    /// filter.
    #[tokio::test]
    async fn trigger_captures_none_when_anchor_still_pre_flush() {
        let (store, schema) = make_store("mat_capture_none").await;
        const LIMIT: u32 = 10;
        let (w, _t, _tl, mut rx, _in_flight) =
            writer_with_materializer(store.clone(), FlushPolicy::default(), LIMIT);

        // Anchor + 10 non-init deltas, no flush in between — every
        // row including the anchor stays in the pre-flush buffer.
        w.put(&[(k(1), Lsn(10), delta_will_init())]).await.unwrap();
        for i in 0..LIMIT {
            w.put(&[(k(1), Lsn(20 + i as u64), delta_non_init())])
                .await
                .unwrap();
        }

        let job = rx.try_recv().expect("dispatched on hitting threshold");
        assert!(
            job.chain.is_none(),
            "captured chain must be None when the will_init anchor is \
             still unflushed at trigger time"
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

        // 5 deltas, then an image, then 9 deltas — must NOT dispatch.
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
        assert!(
            rx.try_recv().is_err(),
            "image should have reset the counter"
        );
        // 10th post-image delta: now dispatch.
        w.put(&[(k(1), Lsn(200), delta_non_init())]).await.unwrap();
        let job = rx.try_recv().expect("dispatch after 10 post-image deltas");
        assert_eq!(job.target_lsn, Lsn(200));

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
            writer_with_materializer_thresholds(store.clone(), FlushPolicy::default(), 0, limit);

        w.put(&[(k(1), Lsn(10), delta_non_init())]).await.unwrap();
        w.put(&[(k(1), Lsn(20), delta_non_init())]).await.unwrap();
        assert!(
            rx.try_recv().is_err(),
            "must not dispatch before payload-bytes threshold is reached"
        );
        // 3rd delta puts cumulative bytes at exactly the limit.
        w.put(&[(k(1), Lsn(30), delta_non_init())]).await.unwrap();
        let job = rx
            .try_recv()
            .expect("dispatched on hitting payload-bytes threshold");
        assert_eq!(job.target_lsn, Lsn(30));

        // Simulate materialiser finishing so the next threshold can re-fire.
        in_flight.lock().unwrap().remove(&job.key);

        // Counter reset on dispatch — another 2 deltas must NOT
        // dispatch even though cumulative-since-creation is now 5
        // deltas (well past 3).
        w.put(&[(k(1), Lsn(40), delta_non_init())]).await.unwrap();
        w.put(&[(k(1), Lsn(50), delta_non_init())]).await.unwrap();
        assert!(
            rx.try_recv().is_err(),
            "counter must reset after dispatch; tally restarts at 0"
        );
        // 3rd delta past the previous trigger re-fires.
        w.put(&[(k(1), Lsn(60), delta_non_init())]).await.unwrap();
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
        );

        w.put(&[(k(1), Lsn(10), delta_non_init())]).await.unwrap();
        w.put(&[(k(1), Lsn(20), delta_non_init())]).await.unwrap();
        assert!(rx.try_recv().is_err());
        w.put(&[(k(1), Lsn(30), delta_non_init())]).await.unwrap();
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

        // First threshold: dispatch.
        for i in 0..LIMIT {
            w.put(&[(k(1), Lsn(10 + i as u64), delta_non_init())])
                .await
                .unwrap();
        }
        let first = rx.try_recv().expect("first threshold dispatches");
        assert_eq!(first.target_lsn, Lsn(10 + (LIMIT - 1) as u64));
        assert!(
            in_flight.lock().unwrap().contains(&first.key),
            "in_flight must hold the key while the job is outstanding"
        );

        // While the key is in_flight, additional threshold-crossings
        // for it must not enqueue duplicates. Push a generous run of
        // deltas — many multiples of LIMIT — and confirm no new
        // dispatch lands.
        for i in 0..(LIMIT * 3) {
            w.put(&[(k(1), Lsn(100 + i as u64), delta_non_init())])
                .await
                .unwrap();
        }
        assert!(
            rx.try_recv().is_err(),
            "no duplicate dispatch while key is in_flight"
        );

        // Materialiser completes — remove the in_flight reservation.
        // The next delta must re-fire immediately at the *current* LSN,
        // because the counter has been left at-or-above threshold since
        // the dedup-blocked dispatch.
        in_flight.lock().unwrap().remove(&first.key);
        w.put(&[(k(1), Lsn(500), delta_non_init())]).await.unwrap();
        let second = rx
            .try_recv()
            .expect("first delta after in_flight release immediately re-fires");
        assert_eq!(
            second.target_lsn,
            Lsn(500),
            "re-dispatch must target the *current* LSN, not a stale intermediate one"
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
        assert!(
            rx.try_recv().is_err(),
            "will_init should have reset the counter"
        );
        w.put(&[(k(1), Lsn(200), delta_non_init())]).await.unwrap();
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

        // Alternate keys k(1) and k(2). After 4 deltas total each, both
        // should have hit the threshold once.
        for i in 0..LIMIT {
            w.put(&[(k(1), Lsn(10 + i as u64), delta_non_init())])
                .await
                .unwrap();
            w.put(&[(k(2), Lsn(100 + i as u64), delta_non_init())])
                .await
                .unwrap();
        }
        let mut seen_keys = std::collections::HashSet::new();
        for _ in 0..2 {
            let job = rx.try_recv().expect("two dispatches expected");
            seen_keys.insert(job.key);
        }
        assert_eq!(seen_keys.len(), 2, "one dispatch per key");
        assert!(rx.try_recv().is_err(), "no extra dispatches");

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
        assert!(
            rx.try_recv().is_err(),
            "limit=0 must not dispatch"
        );

        drop_schema(&store, &schema).await;
    }

    /// Helper: insert a `timeline_state` row with `current_generation = 1`
    /// matching the generation used by the `writer` helper. Required
    /// for any test that wants `PageWriter::flush` to land its
    /// generation-gated `advance_disk_consistent_lsn` UPDATE.
    async fn seed_timeline_row(
        store: &PageStore,
        tenant: TenantId,
        timeline: TimelineId,
    ) {
        store
            .insert_timeline_at_creation(&crate::page_store::TimelineRow {
                tenant_id: tenant,
                timeline_id: timeline,
                disk_consistent_lsn: Lsn(0),
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

    /// `PageWriter::flush` advances the `visible_remote_consistent_lsn`
    /// slot installed via `set_remote_consistent_lsn_slot` whenever
    /// the generation-gated `advance_disk_consistent_lsn` UPDATE
    /// succeeds. Walreceiver feedback reads that slot to drive
    /// safekeeper WAL truncation — regression on the publish path
    /// would silently freeze WAL retention.
    #[tokio::test]
    async fn flush_publishes_remote_consistent_lsn() {
        let (store, schema) = make_store("rclsn_publish").await;
        let (w, t, tl) = writer(store.clone(), FlushPolicy::default());
        seed_timeline_row(&store, t, tl).await;

        let slot = Arc::new(utils::lsn::AtomicLsn::new(0));
        w.set_remote_consistent_lsn_slot(slot.clone());

        // Slot is zero before any flush.
        assert_eq!(slot.load(), Lsn(0));

        // One image at Lsn(42). After flush the slot must reflect it.
        w.put(&[(k(1), Lsn(42), image())]).await.unwrap();
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
            Mutex<HashSet<bytes::Bytes>>,
            tokio::sync::mpsc::Sender<MaterializeJob>,
            tokio::sync::mpsc::Receiver<MaterializeJob>,
        ) {
            let (tx, rx) = tokio::sync::mpsc::channel::<MaterializeJob>(8);
            (Mutex::new(HashSet::new()), tx, rx)
        }

        fn kb() -> bytes::Bytes {
            key_to_bytes(k(1))
        }

        #[test]
        fn dispatches_when_chain_exceeds_count_limit() {
            let (in_flight, tx, mut rx) = setup();
            let dispatched =
                try_dispatch_read_side(&kb(), Lsn(100), 21, 20, &in_flight, &tx);
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
                    &kb(), Lsn(100), chain_len, 20, &in_flight, &tx
                ));
            }
            assert!(rx.try_recv().is_err());
            assert!(in_flight.lock().unwrap().is_empty());
        }

        #[test]
        fn skips_when_count_limit_zero_disabled() {
            let (in_flight, tx, mut rx) = setup();
            assert!(!try_dispatch_read_side(
                &kb(), Lsn(100), 9999, 0, &in_flight, &tx
            ));
            assert!(rx.try_recv().is_err());
            assert!(in_flight.lock().unwrap().is_empty());
        }

        #[test]
        fn skips_when_key_already_in_flight() {
            let (in_flight, tx, mut rx) = setup();
            in_flight.lock().unwrap().insert(kb());
            assert!(!try_dispatch_read_side(
                &kb(), Lsn(100), 50, 20, &in_flight, &tx
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
                key: key_to_bytes(k(99)),
                target_lsn: Lsn(1),
                chain: None,
            })
            .unwrap();
            let in_flight = Mutex::new(HashSet::new());
            // Dispatch should fail and NOT leave the key in in_flight.
            assert!(!try_dispatch_read_side(
                &kb(), Lsn(100), 50, 20, &in_flight, &tx
            ));
            assert!(
                !in_flight.lock().unwrap().contains(&kb()),
                "in_flight must be rolled back so a later read can retry"
            );
            // The pre-filled job is still there; only it should drain.
            let pre = rx.try_recv().unwrap();
            assert_eq!(pre.key, key_to_bytes(k(99)));
            assert!(rx.try_recv().is_err());
        }
    }
}
