//! Background image materialiser for `pageserver.pages`.
//!
//! When `PageWriter::put` observes a key crossing
//! `YbPageWriterConfig::image_chain_limit` consecutive non-image,
//! non-`will_init` deltas, it pushes a [`MaterializeJob`] onto a
//! bounded mpsc. This task drains the queue: per job it reads the
//! current chain from `PageStore`, walredos to a fresh page image at
//! the triggering LSN, and INSERTs the result as a new image row
//! (`is_image=TRUE, will_init=TRUE`). Subsequent reads at LSN ≥ target
//! land on the fast-path image-anchored CTE instead of walking back
//! to the latest `will_init` delta.
//!
//! Why a background task and not at-write materialisation: walredo is
//! per-key and synchronous (subprocess RPC). Doing it on the
//! walreceiver's hot path would serialise WAL ingest. The
//! buffer-blended read path already serves correct pages without an
//! image; materialisation is pure read-latency optimisation.
//!
//! Failure / restart behaviour: on walredo or YB INSERT error the job
//! is logged and dropped — the next delta past the (now-reset)
//! counter re-enqueues. After a pageserver restart, the in-RAM
//! counters start at zero; restart-survivor keys with long chains
//! keep reading via the slow path until 10 more deltas trigger
//! materialisation. A periodic backstop scan could close this gap
//! later (not v1).

use std::sync::Arc;

use anyhow::{Context, Result};
use bytes::Bytes;
use pageserver_api::key::Key;
use postgres_ffi::PgMajorVersion;
use tokio::sync::mpsc;
use tokio_util::sync::CancellationToken;
use tracing::{Instrument, debug, info, info_span};
use utils::id::{TenantId, TimelineId};
use utils::lsn::Lsn;
use wal_decoder::models::record::NeonWalRecord;

use crate::page_store::{PageRow, PageStore};
use crate::page_writer::{MaterializeJob, PageWriter};
use crate::page_server::WalRedoManager;
use crate::walredo::RedoAttemptType;

/// Drain up to this many queued `MaterializeJob`s per worker iteration
/// so their image INSERTs land in a single binary-COPY round-trip.
/// Sized to keep the COPY round-trip dominated by network/disk and
/// not by per-row PG parameter overhead; capped to avoid stalling
/// the queue if walredo is slow.
///
/// Raised once write-side triggers started carrying their durable
/// chain inline (see `MaterializeJob.chain`): redo no longer blocks on
/// a YB `read_chain_durable` round-trip per job, so a batch of N is
/// CPU + walproposer-lib bound at `BASE_REDO_CONCURRENCY` parallelism
/// (scaled by `scale_factor`) and can clear in tens to hundreds of ms
/// even at the upper end. Larger batches amortise the per-row PG
/// protocol overhead and (with `BASE_MAX_INFLIGHT_INSERTS`) let batch
/// N's INSERT overlap fully with batch N+1's redo.
const BATCH_DRAIN_LIMIT: usize = 500;

/// Maximum number of pipelined INSERT futures that can be in flight
/// concurrently on the materialiser task. The redo phase of batch N+1
/// runs on the main task while a previously-redone batch's INSERT
/// drains in a spawned future; this cap bounds how many spawned
/// INSERT futures can be outstanding at once.
///
/// At 4: each materialiser `put_pages` carries ≤500 rows (one
/// `ROWS_PER_INSERT` chunk) and therefore uses exactly one slot of
/// the internal `PageStore` pool. With `INTERNAL_POOL_SIZE = 4` over
/// in `yb-pageserver.rs`, 4 in-flight INSERTs map to 4 distinct YB
/// backends and lift the materialiser's INSERT-side throughput from
/// ~38 K rows/s (1 conn at ~13 ms / 490-row batch) toward ~150 K rows/s.
/// Raises peak in-flight RAM by ~8 MB (4 × 500 × 8 KB image) over
/// the prior 2-deep pipeline; small versus the win when the
/// materialise queue is being driven hard enough to backpressure
/// the walreceiver. Scaled by `PageServerConf::scale_factor`.
const BASE_MAX_INFLIGHT_INSERTS: usize = 4;

/// Maximum concurrent `redo_one` futures in flight within a single
/// `materialize_batch`. Each future does an SQL `read_chain_durable`
/// (against `fpu_page_store` — its own connection), a CPU
/// WAL-record decode, and a walredo subprocess RPC.
///
/// The walredo subprocess serialises its own apply phase via stdin /
/// stdout mutexes — true CPU parallelism in walredo would need a
/// subprocess pool, which is a separate change to `PostgresRedoManager`.
/// But the SQL read and the WAL decode CAN run in parallel, and walredo
/// pipelines stdin-write of req N+1 with stdout-read of req N inside
/// the subprocess. So 8-way fan-out captures most of the win at low
/// risk; raising further hits diminishing returns until walredo itself
/// is parallelised. Scaled by `PageServerConf::scale_factor`.
const BASE_REDO_CONCURRENCY: usize = 8;

/// Output of [`ImageMaterializer::redo_phase`] — everything
/// [`ImageMaterializer::commit_phase`] needs to bulk-INSERT a batch's
/// rebuilt images into YB and install them into the in-RAM cache.
struct RedoOutcome {
    images: Vec<std::sync::Arc<PageRow>>,
    /// Bytes already added to `MATERIALIZER_INFLIGHT_BYTES` during
    /// redo. `commit_phase` is responsible for decrementing this on
    /// every exit path — the gauge then reflects bytes-still-held
    /// through the whole commit, not just the redo phase.
    held_image_bytes: u64,
    t_redo: std::time::Duration,
    total_chain_rows: usize,
    total_records: usize,
}

/// Background materialiser for one timeline.
pub struct ImageMaterializer {
    tenant_id: TenantId,
    timeline_id: TimelineId,
    page_writer: Arc<PageWriter>,
    /// Internal-traffic `PageStore` (own YSQL connection, shared
    /// with the walreceiver's `PageWriter::flush` write path and
    /// the materialiser's own cache-miss `read_chain_durable`).
    /// Keeps every materialiser write off the compute getPage
    /// connection — that's the isolation we wanted.
    fpu_page_store: Arc<PageStore>,
    walredo_mgr: Arc<WalRedoManager>,
    pg_version: PgMajorVersion,
    /// Shared with the write trigger in `PageWriter::put` — see
    /// `MaterializerHandle::in_flight`. After each batch (success or
    /// failure) the materialiser removes every key it pulled from the
    /// queue so the next delta past the per-key threshold can re-fire
    /// at the current LSN.
    in_flight: Arc<std::sync::Mutex<std::collections::HashSet<bytes::Bytes>>>,
    /// `BASE_REDO_CONCURRENCY` scaled by `PageServerConf::scale_factor`.
    redo_concurrency: usize,
    /// `BASE_MAX_INFLIGHT_INSERTS` scaled by `PageServerConf::scale_factor`.
    max_inflight_inserts: usize,
}

/// Handle returned by [`ImageMaterializer::spawn`]. Drop to leak; call
/// `shutdown` for a clean stop.
pub struct MaterializerTask {
    pub handle: tokio::task::JoinHandle<()>,
    pub cancel: CancellationToken,
}

impl MaterializerTask {
    pub async fn shutdown(self) {
        self.cancel.cancel();
        let _ = self.handle.await;
    }
}

impl ImageMaterializer {
    pub(crate) fn spawn(
        tenant_id: TenantId,
        timeline_id: TimelineId,
        page_writer: Arc<PageWriter>,
        fpu_page_store: Arc<PageStore>,
        walredo_mgr: Arc<WalRedoManager>,
        pg_version: PgMajorVersion,
        mut rx: mpsc::Receiver<MaterializeJob>,
        in_flight: Arc<std::sync::Mutex<std::collections::HashSet<bytes::Bytes>>>,
        scale_factor: f64,
    ) -> MaterializerTask {
        let cancel = CancellationToken::new();
        let cancel_for_task = cancel.clone();
        let redo_concurrency =
            pageserver_api::config::scale_count(BASE_REDO_CONCURRENCY, scale_factor);
        let max_inflight_inserts =
            pageserver_api::config::scale_count(BASE_MAX_INFLIGHT_INSERTS, scale_factor);
        let me = Arc::new(Self {
            tenant_id,
            timeline_id,
            page_writer,
            fpu_page_store,
            walredo_mgr,
            pg_version,
            in_flight,
            redo_concurrency,
            max_inflight_inserts,
        });
        // Walredo's `debug_assert_current_span_has_tenant_id` requires
        // both `tenant_id` and `shard_id` fields on the active span;
        // we run with a single-shard hardcoded layout, so emit "0000".
        let span = info_span!(
            "image_materializer",
            tenant_id = %tenant_id,
            shard_id = "0000",
            timeline_id = %timeline_id,
        );
        let handle = tokio::spawn(
            async move {
                // Helper: refresh the queue-depth gauge from the receiver's
                // current state. Called after every recv/try_recv and after
                // each batch completes so the gauge reflects live drain
                // rather than the last write-side dispatch snapshot.
                let sample_queue_depth = |rx: &tokio::sync::mpsc::Receiver<MaterializeJob>| {
                    crate::metrics::MATERIALIZER_QUEUE_DEPTH.set(rx.len() as u64);
                };
                // In-flight INSERT futures, oldest at the front. Bounded
                // to `max_inflight_inserts` — when full, await the front
                // before spawning a new one. Lets batch N+1's redo run
                // concurrently with batch N's INSERT round-trip while
                // keeping YB-side write fan-out bounded.
                let max_inflight_inserts = me.max_inflight_inserts;
                let mut inflight_inserts: std::collections::VecDeque<tokio::task::JoinHandle<()>> =
                    std::collections::VecDeque::with_capacity(max_inflight_inserts);
                loop {
                    tokio::select! {
                        biased;
                        _ = cancel_for_task.cancelled() => break,
                        first = rx.recv() => {
                            let Some(first) = first else { break }; // PageWriter dropped
                            sample_queue_depth(&rx);
                            // Drain up to BATCH_DRAIN_LIMIT-1 more jobs
                            // non-blockingly so we can fold them into one
                            // walredo loop + one COPY round-trip.
                            let mut batch = Vec::with_capacity(BATCH_DRAIN_LIMIT);
                            batch.push(first);
                            while batch.len() < BATCH_DRAIN_LIMIT {
                                match rx.try_recv() {
                                    Ok(j) => batch.push(j),
                                    Err(_) => break,
                                }
                            }
                            sample_queue_depth(&rx);
                            // Phase 1 (synchronous on this task): drive the
                            // batch's chain reads + walredo. Errors only
                            // come from setup; per-job redo errors are
                            // logged + skipped inside.
                            let outcome = match me.redo_phase(&batch).await {
                                Ok(o) => o,
                                Err(e) => {
                                    crate::metrics::CORRECTNESS_ERRORS
                                        .with_label_values(&["materializer_setup"])
                                        .inc();
                                    tracing::error!(
                                        tenant_id = %me.tenant_id,
                                        timeline_id = %me.timeline_id,
                                        batch_size = batch.len(),
                                        "MATERIALIZER REDO-PHASE SETUP ERROR — \
                                         entire batch un-materialised: {e:#}"
                                    );
                                    // Release in_flight for these keys so
                                    // the write trigger can re-dispatch
                                    // them on the next delta.
                                    let mut guard = me.in_flight.lock().unwrap();
                                    for job in &batch {
                                        guard.remove(&job.key);
                                    }
                                    continue;
                                }
                            };
                            // Phase 2 (pipelined): hand off the INSERT +
                            // cache install + in_flight release to a
                            // spawned future. The next iteration's redo
                            // starts immediately. If we're already at
                            // `max_inflight_inserts`, await the oldest first.
                            while inflight_inserts.len() >= max_inflight_inserts {
                                if let Some(h) = inflight_inserts.pop_front() {
                                    let _ = h.await;
                                }
                            }
                            let me_for_commit = Arc::clone(&me);
                            let h = tokio::spawn(
                                async move {
                                    me_for_commit.commit_phase(batch, outcome).await;
                                }
                                .in_current_span(),
                            );
                            inflight_inserts.push_back(h);
                            sample_queue_depth(&rx);
                        }
                    }
                }
                // Best-effort: drain any still-running INSERTs so the
                // last `max_inflight_inserts - 1` batches' images make it
                // to YB before the task exits. Without this, a clean
                // shutdown could leave a redone-but-not-INSERTed batch
                // in RAM that disappears when the task drops.
                while let Some(h) = inflight_inserts.pop_front() {
                    let _ = h.await;
                }
                // Best-effort reset on exit so a dead task doesn't leave
                // the gauge frozen at a stale value.
                crate::metrics::MATERIALIZER_QUEUE_DEPTH.set(0);
                info!(
                    tenant_id = %me.tenant_id,
                    timeline_id = %me.timeline_id,
                    "image materialiser exited",
                );
            }
            .instrument(span),
        );
        MaterializerTask { handle, cancel }
    }

    /// Phase 1 of the pipelined materialise step: run chain reads
    /// + walredo for every job in the batch. Returns a [`RedoOutcome`]
    /// ready to be handed to [`commit_phase`], which runs the bulk
    /// INSERT + cache install asynchronously.
    ///
    /// Per-job redo errors are logged and skipped — one bad chain
    /// shouldn't drop the whole batch. The function itself currently
    /// can't fail (the inner loop only logs); the `Result` is kept so
    /// the call site can grow setup-time errors without changing the
    /// signature.
    ///
    /// Redo runs up to `self.redo_concurrency` `redo_one` futures in
    /// parallel via `FuturesUnordered`. Each future reads its chain
    /// from `fpu_page_store` (own YSQL connection — no contention
    /// with the walreceiver write path), decodes records, and calls
    /// walredo. The SQL reads and WAL decodes parallelise cleanly;
    /// walredo serialises its apply but pipelines IPC, so concurrent
    /// callers amortise the stdin-write / stdout-read latency.
    async fn redo_phase(&self, jobs: &[MaterializeJob]) -> Result<RedoOutcome> {
        use futures::StreamExt;
        use futures::future::{BoxFuture, FutureExt};
        use futures::stream::FuturesUnordered;

        let t0 = std::time::Instant::now();
        let mut images: Vec<std::sync::Arc<PageRow>> = Vec::with_capacity(jobs.len());
        let mut total_chain_rows = 0usize;
        let mut total_records = 0usize;
        // Bytes held in `images` so far. Owned by the resulting
        // `RedoOutcome` so `commit_phase` can decrement the gauge
        // exactly when the bytes are actually freed (after the INSERT
        // future completes, not when this fn returns).
        let mut held_image_bytes: u64 = 0;

        // Boxed futures so prime + refill calls produce the same
        // FuturesUnordered item type. Each future borrows `self` and the
        // `MaterializeJob` slice from `jobs` — both outlive the await.
        type RedoFut<'a> =
            BoxFuture<'a, (&'a MaterializeJob, Result<Option<(PageRow, usize, usize)>>)>;
        fn spawn_redo<'a>(me: &'a ImageMaterializer, job: &'a MaterializeJob) -> RedoFut<'a> {
            async move {
                let res = me.redo_one(job).await;
                (job, res)
            }
            .boxed()
        }

        let mut in_flight: FuturesUnordered<RedoFut<'_>> = FuturesUnordered::new();
        let mut iter = jobs.iter();
        for _ in 0..self.redo_concurrency {
            if let Some(job) = iter.next() {
                in_flight.push(spawn_redo(self, job));
            } else {
                break;
            }
        }
        while let Some((job, res)) = in_flight.next().await {
            match res {
                Ok(Some((image_row, chain_len, n_records))) => {
                    total_chain_rows += chain_len;
                    total_records += n_records;
                    let b = image_row.payload.len() as u64;
                    crate::metrics::MATERIALIZER_INFLIGHT_BYTES.add(b);
                    held_image_bytes += b;
                    images.push(std::sync::Arc::new(image_row));
                }
                Ok(None) => {} // nothing-to-do (no chain / already image-anchored)
                Err(e) => {
                    // A walredo failure means the chain we assembled
                    // does not legally produce a page (PG's walredo
                    // PANIC'd, or the chain has missing/extra deltas
                    // relative to the base image). Continuing risks
                    // silently persisting a bad image; abort instead
                    // so the operator sees the corruption signal
                    // immediately.
                    crate::metrics::CORRECTNESS_ERRORS
                        .with_label_values(&["materializer_redo"])
                        .inc();
                    utils::abort::fatal_abort(format_args!(
                        "MATERIALIZER REDO ERROR tenant={} timeline={} \
                         target_lsn={} key={:?}: {:#}",
                        self.tenant_id,
                        self.timeline_id,
                        job.target_lsn,
                        bytes::Bytes::copy_from_slice(&job.key),
                        e,
                    ));
                }
            }
            if let Some(next_job) = iter.next() {
                in_flight.push(spawn_redo(self, next_job));
            }
        }
        Ok(RedoOutcome {
            images,
            held_image_bytes,
            t_redo: t0.elapsed(),
            total_chain_rows,
            total_records,
        })
    }

    /// Phase 2 of the pipelined materialise step: bulk INSERT the
    /// images + install each into the in-RAM cache + release the
    /// per-key `in_flight` guards + decrement the inflight-bytes gauge.
    ///
    /// Spawned by the materialiser's outer loop so that the next
    /// batch's [`redo_phase`] can run concurrently with this INSERT.
    /// At most `self.max_inflight_inserts` instances of this future
    /// are alive at once.
    ///
    /// `in_flight` and the gauge are always released, regardless of
    /// success or failure of `put_pages`, so a transient YB error
    /// doesn't leak the per-key trigger guard or the byte count.
    async fn commit_phase(self: Arc<Self>, batch: Vec<MaterializeJob>, outcome: RedoOutcome) {
        // Release the inflight-bytes gauge on every exit path. The
        // bytes themselves are genuinely freed when `outcome.images`
        // drops at the end of this fn (or in `commit_phase_inner`'s
        // drop on error). The scopeguard fires whether or not the
        // INSERT succeeded.
        let held = outcome.held_image_bytes;
        let _bytes_guard = scopeguard::guard((), |_| {
            if held > 0 {
                crate::metrics::MATERIALIZER_INFLIGHT_BYTES.sub(held);
            }
        });
        let result = self.commit_phase_inner(&batch, outcome).await;
        // Release per-key in_flight regardless of success/failure so
        // the write trigger can re-fire on the next delta. On failure
        // the next trigger will re-fire after the per-key counter
        // accumulates another threshold's worth of deltas; on success
        // the trigger fires immediately on the next delta (the counter
        // sat ≥ threshold during the in-flight stall).
        {
            let mut guard = self.in_flight.lock().unwrap();
            for job in &batch {
                guard.remove(&job.key);
            }
        }
        if let Err(e) = result {
            crate::metrics::CORRECTNESS_ERRORS
                .with_label_values(&["materializer_commit"])
                .inc();
            tracing::error!(
                tenant_id = %self.tenant_id,
                timeline_id = %self.timeline_id,
                batch_size = batch.len(),
                "MATERIALIZER COMMIT ERROR — batch images NOT written to YB; \
                 those keys stay un-trimmed in cache and may build deep chains \
                 until the next successful materialisation: {e:#}"
            );
        }
    }

    async fn commit_phase_inner(
        &self,
        batch: &[MaterializeJob],
        outcome: RedoOutcome,
    ) -> Result<()> {
        let RedoOutcome {
            images,
            held_image_bytes: _,
            t_redo,
            total_chain_rows,
            total_records,
        } = outcome;
        if images.is_empty() {
            return Ok(());
        }
        // Parameterised batched INSERT, parallel chunks, same path
        // the walreceiver flush uses. With ≤64 image rows per batch
        // the whole thing typically lands in a single chunk; if it
        // overflows, put_pages's `buffer_unordered(4)` pipelines the
        // chunks on the dedicated connection.
        let t_insert_start = std::time::Instant::now();
        self.fpu_page_store
            .put_pages(self.tenant_id, self.timeline_id, &images)
            .await
            .context("put_pages")?;
        let t_insert = t_insert_start.elapsed();

        // Install each image into the cache (one writer lock per
        // image — short and bounded; we don't merge them into one
        // critical section because that would block reads for the
        // whole batch's worth of trim/push work).
        let t_install_start = std::time::Instant::now();
        for image_row in &images {
            self.page_writer
                .install_materialized_image(image_row.clone())
                .await;
        }
        let t_install = t_install_start.elapsed();

        let total = t_redo + t_insert + t_install;
        if total.as_millis() >= 5 {
            info!(
                tag = "materialise_batch",
                total_ms = total.as_millis() as u64,
                redo_ms = t_redo.as_millis() as u64,
                insert_ms = t_insert.as_millis() as u64,
                install_ms = t_install.as_millis() as u64,
                batch_size = batch.len(),
                images = images.len(),
                chain_rows = total_chain_rows,
                records = total_records,
                "perf",
            );
        }
        debug!(
            tenant_id = %self.tenant_id,
            timeline_id = %self.timeline_id,
            batch_size = batch.len(),
            images = images.len(),
            "materialised image batch",
        );
        Ok(())
    }

    /// Per-job read + walredo. Returns:
    /// - `Some((image_row, chain_len, n_records))` to enqueue for COPY,
    /// - `None` to drop the job (nothing durable yet, or already image-anchored),
    /// - `Err` to log and skip.
    async fn redo_one(&self, job: &MaterializeJob) -> Result<Option<(PageRow, usize, usize)>> {
        // target_lsn is durable by construction (post-flush trigger
        // uses max snapshot LSN per key; read-side trigger uses the
        // caller's bound). Fetch via `read_chain_durable` — the cache
        // fast path handles the hot case and YB fallback handles
        // post-eviction or read-side dispatch.
        let chain = self
            .page_writer
            .read_chain_durable(Key::from_slice(&job.key), job.target_lsn)
            .await
            .context("read_chain_durable")?;
        tracing::debug!(
            tenant_id = %self.tenant_id,
            timeline_id = %self.timeline_id,
            target_lsn = %job.target_lsn,
            key = ?bytes::Bytes::copy_from_slice(&job.key),
            chain_len = chain.len(),
            "materialize_chain"
        );
        if chain.is_empty() {
            return Ok(None);
        }
        if let Some(head) = chain.first()
            && head.is_image
            && head.lsn >= job.target_lsn
        {
            return Ok(None);
        }

        // Track chain bytes held in RAM for the duration of walredo.
        // The MATERIALIZER_INFLIGHT_BYTES gauge sums these across the
        // `redo_concurrency` in-flight chains so an operator can see the
        // transient pool that `read_cache_bytes` does NOT cap.
        let chain_bytes: u64 = chain.iter().map(|r| r.payload.len() as u64).sum();
        crate::metrics::MATERIALIZER_INFLIGHT_BYTES.add(chain_bytes);
        let chain_guard = scopeguard::guard(chain_bytes, |b| {
            crate::metrics::MATERIALIZER_INFLIGHT_BYTES.sub(b);
        });

        let (base_img, records) = parse_chain(&chain)?;
        if records.is_empty() && base_img.is_none() {
            return Ok(None);
        }

        let actual_lsn = chain.first().expect("chain non-empty").lsn;
        let key = Key::from_slice(&job.key);
        let chain_len = chain.len();
        let n_records = chain_len.saturating_sub(if base_img.is_some() { 1 } else { 0 });
        let image_bytes = if records.is_empty() {
            base_img.expect("checked records empty + base_img some").1
        } else {
            let chain_dump: Vec<String> = chain
                .iter()
                .map(|r| {
                    format!(
                        "(lsn={} is_image={} will_init={} pl={})",
                        r.lsn,
                        r.is_image as u8,
                        r.will_init as u8,
                        r.payload.len()
                    )
                })
                .collect();
            self.walredo_mgr
                .request_redo(
                    key,
                    actual_lsn,
                    base_img.clone(),
                    records,
                    self.pg_version,
                    RedoAttemptType::LegacyCompaction,
                )
                .await
                .with_context(|| {
                    format!(
                        "request_redo actual_lsn={actual_lsn} \
                         chain_len={chain_len} base_img_lsn={:?} chain=[{}]",
                        base_img.as_ref().map(|(l, _)| *l),
                        chain_dump.join(",")
                    )
                })?
        };
        drop(chain_guard);
        Ok(Some((
            PageRow {
                key: job.key.clone(),
                lsn: actual_lsn,
                is_image: true,
                will_init: true,
                payload: image_bytes,
            },
            chain_len,
            n_records,
        )))
    }
}

/// Turn an LSN-descending chain (oldest at `chain.last()`) into the
/// `(base_img, records)` pair `WalRedoManager::request_redo` expects.
/// Mirrors `page_server::timeline::chain_to_value_reconstruct_state` but
/// returns the tuple directly so we don't pull in the read-path's
/// `ValueReconstructState` (which lives in `storage_layer`, slated for
/// removal in Phase 4d).
fn parse_chain(
    chain: &[std::sync::Arc<PageRow>],
) -> Result<(Option<(Lsn, Bytes)>, Vec<(Lsn, NeonWalRecord)>)> {
    use utils::bin_ser::BeSer;

    if chain.is_empty() {
        return Ok((None, Vec::new()));
    }
    let anchor_idx = chain.len() - 1;
    let anchor = &chain[anchor_idx];

    let (base_img, records_slice) = if anchor.is_image {
        (
            Some((anchor.lsn, anchor.payload.clone())),
            &chain[..anchor_idx],
        )
    } else {
        (None, &chain[..])
    };

    let mut records = Vec::with_capacity(records_slice.len());
    for row in records_slice {
        if row.is_image {
            anyhow::bail!(
                "image row at lsn={} found above the chain anchor (anchor_lsn={}); \
                 parse_chain expects exactly one anchor at the tail. \
                 Upstream chain construction is feeding mixed image+delta rows.",
                row.lsn,
                anchor.lsn,
            );
        }
        let rec = NeonWalRecord::des(&row.payload).with_context(|| {
            // Include payload length + a short hex prefix so on-disk
            // corruption is diagnosable from the log alone (the
            // bincode error message itself often just says
            // "io error: ... unexpected end of file" or similar).
            let prefix = row
                .payload
                .iter()
                .take(16)
                .map(|b| format!("{b:02x}"))
                .collect::<String>();
            format!(
                "decode NeonWalRecord at lsn={} (payload_len={}, prefix={prefix}..)",
                row.lsn,
                row.payload.len(),
            )
        })?;
        records.push((row.lsn, rec));
    }
    // `chain` came in LSN-descending; walredo applies records in
    // vector order and expects LSN-ascending so each delta sees the
    // page state BEFORE it (cf. `Timeline::reconstruct_value`, which
    // does the same reverse before its own walredo call).
    records.reverse();
    Ok((base_img, records))
}
