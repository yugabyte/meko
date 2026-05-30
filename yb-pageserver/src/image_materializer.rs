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
use tracing::{debug, info, warn};
use utils::id::{TenantId, TimelineId};
use utils::lsn::Lsn;
use wal_decoder::models::record::NeonWalRecord;

use crate::page_store::PageRow;
use crate::page_writer::{MaterializeJob, PageWriter};
use crate::tenant::WalRedoManager;
use crate::walredo::RedoAttemptType;

/// Background materialiser for one timeline.
pub struct ImageMaterializer {
    tenant_id: TenantId,
    timeline_id: TimelineId,
    page_writer: Arc<PageWriter>,
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
            walredo_mgr,
            pg_version,
        });
        let handle = tokio::spawn(async move {
            loop {
                tokio::select! {
                    biased;
                    _ = cancel_for_task.cancelled() => break,
                    job = rx.recv() => {
                        let Some(job) = job else { break }; // PageWriter dropped
                        if let Err(e) = me.materialize(&job).await {
                            warn!(
                                tenant_id = %me.tenant_id,
                                timeline_id = %me.timeline_id,
                                target_lsn = %job.target_lsn,
                                "image materialisation failed: {e:#}"
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
        });
        MaterializerTask { handle, cancel }
    }

    async fn materialize(&self, job: &MaterializeJob) -> Result<()> {
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
            // Nothing durable to anchor on — the triggering deltas
            // are still in the pre-flush buffer. Next post-flush
            // enqueue will pick this up.
            return Ok(());
        }
        // Skip if the chain is already image-anchored at or past the
        // target LSN (concurrent / duplicate enqueue).
        if let Some(head) = chain.first()
            && head.is_image
            && head.lsn >= job.target_lsn
        {
            return Ok(());
        }

        let (base_img, records) = parse_chain(&chain)?;
        if records.is_empty() && base_img.is_none() {
            return Ok(()); // nothing to redo
        }

        // Use the highest LSN actually present in our durable chain
        // as the image LSN, not the originally-enqueued target_lsn.
        // If the chain stops short of target_lsn (because some
        // triggering deltas aren't yet durable), we still make
        // useful progress and the next trigger picks up the rest.
        let actual_lsn = chain.first().expect("chain non-empty").lsn;
        let key = Key::from_slice(&job.key);
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

        let image_row = PageRow {
            key: job.key.clone(),
            lsn: actual_lsn,
            is_image: true,
            will_init: true,
            payload: image_bytes,
        };
        // 1) INSERT to YB first so the durable copy lands before
        // we mutate the in-RAM cache.
        self.page_writer
            .page_store()
            .put_pages(self.tenant_id, self.timeline_id, &[image_row.clone()])
            .await
            .context("put_pages")?;
        // 2) Trim cache for this key up to actual_lsn (drops the
        // deltas the image just superseded) and insert the new image
        // as a durable cache entry.
        self.page_writer
            .install_materialized_image(image_row)
            .await;
        debug!(
            tenant_id = %self.tenant_id,
            timeline_id = %self.timeline_id,
            target_lsn = %job.target_lsn,
            actual_lsn = %actual_lsn,
            chain_len = chain.len(),
            "materialised image",
        );
        Ok(())
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
