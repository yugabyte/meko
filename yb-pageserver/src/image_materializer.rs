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
use tracing::{Instrument, debug, info, info_span, warn};
use utils::id::{TenantId, TimelineId};
use utils::lsn::Lsn;
use wal_decoder::models::record::NeonWalRecord;

use crate::page_store::{PageRow, PageStore};
use crate::page_writer::{MaterializeJob, PageWriter};
use crate::tenant::WalRedoManager;
use crate::walredo::RedoAttemptType;

/// Drain up to this many queued `MaterializeJob`s per worker iteration
/// so their image INSERTs land in a single binary-COPY round-trip.
/// Sized to keep the COPY round-trip dominated by network/disk and
/// not by per-row PG parameter overhead; capped to avoid stalling
/// the queue if walredo is slow.
const BATCH_DRAIN_LIMIT: usize = 64;

/// Maximum concurrent `redo_one` futures in flight within a single
/// `materialize_batch`. Each future does an SQL `read_chain_durable`
/// (against `internal_page_store` — its own connection), a CPU
/// WAL-record decode, and a walredo subprocess RPC.
///
/// The walredo subprocess serialises its own apply phase via stdin /
/// stdout mutexes — true CPU parallelism in walredo would need a
/// subprocess pool, which is a separate change to `PostgresRedoManager`.
/// But the SQL read and the WAL decode CAN run in parallel, and walredo
/// pipelines stdin-write of req N+1 with stdout-read of req N inside
/// the subprocess. So 8-way fan-out captures most of the win at low
/// risk; raising further hits diminishing returns until walredo itself
/// is parallelised.
const REDO_CONCURRENCY: usize = 8;

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
    internal_page_store: Arc<PageStore>,
    walredo_mgr: Arc<WalRedoManager>,
    pg_version: PgMajorVersion,
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
    pub fn spawn(
        tenant_id: TenantId,
        timeline_id: TimelineId,
        page_writer: Arc<PageWriter>,
        internal_page_store: Arc<PageStore>,
        walredo_mgr: Arc<WalRedoManager>,
        pg_version: PgMajorVersion,
        mut rx: mpsc::Receiver<MaterializeJob>,
    ) -> MaterializerTask {
        let cancel = CancellationToken::new();
        let cancel_for_task = cancel.clone();
        let me = Arc::new(Self {
            tenant_id,
            timeline_id,
            page_writer,
            internal_page_store,
            walredo_mgr,
            pg_version,
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
        let handle = tokio::spawn(async move {
            loop {
                tokio::select! {
                    biased;
                    _ = cancel_for_task.cancelled() => break,
                    first = rx.recv() => {
                        let Some(first) = first else { break }; // PageWriter dropped
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
                        if let Err(e) = me.materialize_batch(&batch).await {
                            warn!(
                                tenant_id = %me.tenant_id,
                                timeline_id = %me.timeline_id,
                                batch_size = batch.len(),
                                "image materialisation batch failed: {e:#}"
                            );
                        }
                    }
                }
            }
            info!(
                tenant_id = %me.tenant_id,
                timeline_id = %me.timeline_id,
                "image materialiser exited",
            );
        }.instrument(span));
        MaterializerTask { handle, cancel }
    }

    /// Materialise a batch of jobs end-to-end: read every chain,
    /// walredo each, INSERT all resulting images in one round-trip on
    /// the dedicated connection, then install each into the in-RAM
    /// cache. Errors from individual jobs are logged and skipped —
    /// one bad chain shouldn't drop the whole batch. Errors from the
    /// INSERT itself propagate (caller logs + the next trigger
    /// retries).
    ///
    /// The redo phase runs up to `REDO_CONCURRENCY` `redo_one` futures
    /// in parallel via `FuturesUnordered`. Each future reads its chain
    /// from `internal_page_store` (own YSQL connection — no contention
    /// with the walreceiver write path), decodes records, and calls
    /// walredo. The SQL reads and WAL decodes parallelise cleanly;
    /// walredo serialises its apply but pipelines IPC, so concurrent
    /// callers amortise the stdin-write / stdout-read latency. Order
    /// of completion is irrelevant since each result is independent
    /// and they're all merged into `images` for the single bulk INSERT.
    async fn materialize_batch(&self, jobs: &[MaterializeJob]) -> Result<()> {
        use futures::StreamExt;
        use futures::future::{BoxFuture, FutureExt};
        use futures::stream::FuturesUnordered;

        let t0 = std::time::Instant::now();
        let mut images: Vec<PageRow> = Vec::with_capacity(jobs.len());
        let mut total_chain_rows = 0usize;
        let mut total_records = 0usize;

        // Boxed futures so prime + refill calls produce the same
        // FuturesUnordered item type. Each future borrows `self` and the
        // `MaterializeJob` slice from `jobs` — both outlive the await.
        type RedoFut<'a> = BoxFuture<
            'a,
            (
                &'a MaterializeJob,
                Result<Option<(PageRow, usize, usize)>>,
            ),
        >;
        fn spawn_redo<'a>(me: &'a ImageMaterializer, job: &'a MaterializeJob) -> RedoFut<'a> {
            async move {
                let res = me.redo_one(job).await;
                (job, res)
            }
            .boxed()
        }

        let mut in_flight: FuturesUnordered<RedoFut<'_>> = FuturesUnordered::new();
        let mut iter = jobs.iter();
        // Prime up to REDO_CONCURRENCY futures.
        for _ in 0..REDO_CONCURRENCY {
            if let Some(job) = iter.next() {
                in_flight.push(spawn_redo(self, job));
            } else {
                break;
            }
        }
        // As each completes, refill from the iterator until exhausted.
        while let Some((job, res)) = in_flight.next().await {
            match res {
                Ok(Some((image_row, chain_len, n_records))) => {
                    total_chain_rows += chain_len;
                    total_records += n_records;
                    images.push(image_row);
                }
                Ok(None) => {} // nothing-to-do (no chain / already image-anchored)
                Err(e) => warn!(
                    tenant_id = %self.tenant_id,
                    timeline_id = %self.timeline_id,
                    target_lsn = %job.target_lsn,
                    "skip job in batch: {e:#}"
                ),
            }
            if let Some(next_job) = iter.next() {
                in_flight.push(spawn_redo(self, next_job));
            }
        }
        if images.is_empty() {
            return Ok(());
        }
        let t_redo = t0.elapsed();

        // Parameterised batched INSERT, parallel chunks, same path
        // the walreceiver flush uses. With ≤64 image rows per batch
        // the whole thing typically lands in a single chunk; if it
        // overflows, put_pages's `buffer_unordered(4)` pipelines the
        // chunks on the dedicated connection.
        let t_insert_start = std::time::Instant::now();
        self.internal_page_store
            .put_pages(self.tenant_id, self.timeline_id, &images)
            .await
            .context("put_pages")?;
        let t_insert = t_insert_start.elapsed();

        // 2) Install each image into the cache (one writer lock per
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

        let total = t0.elapsed();
        if total.as_millis() >= 5 {
            info!(
                tag = "materialise_batch",
                total_ms = total.as_millis() as u64,
                redo_ms = t_redo.as_millis() as u64,
                insert_ms = t_insert.as_millis() as u64,
                install_ms = t_install.as_millis() as u64,
                batch_size = jobs.len(),
                images = images.len(),
                chain_rows = total_chain_rows,
                records = total_records,
                "perf",
            );
        }
        debug!(
            tenant_id = %self.tenant_id,
            timeline_id = %self.timeline_id,
            batch_size = jobs.len(),
            images = images.len(),
            "materialised image batch",
        );
        Ok(())
    }

    /// Per-job read + walredo. Returns:
    /// - `Some((image_row, chain_len, n_records))` to enqueue for COPY,
    /// - `None` to drop the job (nothing durable yet, or already image-anchored),
    /// - `Err` to log and skip.
    async fn redo_one(
        &self,
        job: &MaterializeJob,
    ) -> Result<Option<(PageRow, usize, usize)>> {
        // Durable-only read: combines the post-flush cache with YB,
        // EXCLUDES the pre-flush buffer. If we built an image from
        // pre-flush rows, a crash + WAL replay would re-apply those
        // deltas on top of the (already-INSERTed) image → double-apply.
        let chain = self
            .page_writer
            .read_chain_durable(Key::from_slice(&job.key), job.target_lsn)
            .await
            .context("read_chain_durable")?;
        if chain.is_empty() {
            return Ok(None);
        }
        if let Some(head) = chain.first()
            && head.is_image
            && head.lsn >= job.target_lsn
        {
            return Ok(None);
        }

        let (base_img, records) = parse_chain(&chain)?;
        if records.is_empty() && base_img.is_none() {
            return Ok(None);
        }

        let actual_lsn = chain.first().expect("chain non-empty").lsn;
        let key = Key::from_slice(&job.key);
        let chain_len = chain.len();
        let n_records = chain_len.saturating_sub(if base_img.is_some() { 1 } else { 0 });
        let image_bytes = if records.is_empty() {
            base_img
                .expect("checked records empty + base_img some")
                .1
        } else {
            self.walredo_mgr
                .request_redo(
                    key,
                    actual_lsn,
                    base_img,
                    records,
                    self.pg_version,
                    RedoAttemptType::LegacyCompaction,
                )
                .await
                .context("request_redo")?
        };
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
/// Mirrors `tenant::timeline::chain_to_value_reconstruct_state` but
/// returns the tuple directly so we don't pull in the read-path's
/// `ValueReconstructState` (which lives in `storage_layer`, slated for
/// removal in Phase 4d).
fn parse_chain(chain: &[PageRow]) -> Result<(Option<(Lsn, Bytes)>, Vec<(Lsn, NeonWalRecord)>)> {
    use utils::bin_ser::BeSer;

    if chain.is_empty() {
        return Ok((None, Vec::new()));
    }
    let anchor_idx = chain.len() - 1;
    let anchor = &chain[anchor_idx];

    let (base_img, records_slice) = if anchor.is_image {
        (Some((anchor.lsn, anchor.payload.clone())), &chain[..anchor_idx])
    } else {
        (None, &chain[..])
    };

    let mut records = Vec::with_capacity(records_slice.len());
    for row in records_slice {
        let rec = NeonWalRecord::des(&row.payload)
            .with_context(|| format!("decode NeonWalRecord at lsn={}", row.lsn))?;
        records.push((row.lsn, rec));
    }
    // `chain` came in LSN-descending; walredo applies records in
    // vector order and expects LSN-ascending so each delta sees the
    // page state BEFORE it (cf. `Timeline::reconstruct_value`, which
    // does the same reverse before its own walredo call).
    records.reverse();
    Ok((base_img, records))
}
