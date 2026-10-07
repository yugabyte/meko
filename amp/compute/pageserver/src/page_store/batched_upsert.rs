//! The one write path behind `PageStore`'s batched writers — `put_pages`,
//! `put_slru_pages`, `put_meta` and `gc_enqueue_candidates`. Each writer
//! supplies only its [`UpsertLane`] and how one of its rows becomes an
//! [`UpsertRow`]; gating, chunking, fencing and metrics live here.

use std::sync::Arc;

use anyhow::{Context, Result};
use utils::id::BranchId;
use yb_thin_client::{TableHandle, TsClient, UpsertRow, WriteDeadline};

use super::{META_ROWS_PER_INSERT, PageStore, ROWS_PER_INSERT, ThinClientTables, server_fenced};

/// How many of a lane's chunks are in flight at once.
pub(super) enum Fanout {
    /// This base, multiplied by `scale_factor`.
    Scaled(usize),
    /// One at a time.
    Serial,
}

/// The fixed shape of one table's batched write.
pub(super) struct UpsertLane {
    /// The `YB_STORE_OP_SECONDS` op label, and the prefix of a chunk's error.
    pub op: &'static str,
    /// What the write gate and the fence name in their logs.
    pub what: &'static str,
    pub table: fn(&ThinClientTables) -> &Arc<TableHandle>,
    pub rows_per_chunk: usize,
    pub fanout: Fanout,
}

pub(super) const PAGES_LANE: UpsertLane = UpsertLane {
    op: "put_pages",
    what: "write to amp_storage.pages",
    table: |p| &p.pages,
    rows_per_chunk: ROWS_PER_INSERT,
    fanout: Fanout::Scaled(2),
};

pub(super) const SLRU_PAGES_LANE: UpsertLane = UpsertLane {
    op: "put_slru_pages",
    what: "write to amp_storage.slru_pages",
    table: |p| &p.slru_pages,
    rows_per_chunk: ROWS_PER_INSERT,
    fanout: Fanout::Scaled(1),
};

pub(super) const META_LANE: UpsertLane = UpsertLane {
    op: "put_meta",
    what: "write to amp_storage.meta",
    table: |p| &p.meta,
    rows_per_chunk: META_ROWS_PER_INSERT,
    fanout: Fanout::Scaled(2),
};

/// Serial: candidate volume is a rounding error next to the page writes it
/// trails.
pub(super) const PENDING_GC_LANE: UpsertLane = UpsertLane {
    op: "gc_enqueue_candidates",
    what: "write to amp_storage.pending_gc",
    table: |p| &p.pending_gc,
    rows_per_chunk: ROWS_PER_INSERT,
    fanout: Fanout::Serial,
};

impl PageStore {
    /// Upsert `rows` into `lane`'s table, `to_upsert` turning each row (and its
    /// index in `rows`) into its upsert. `on_chunk_ok` sees each chunk's rows
    /// once that chunk has landed.
    ///
    /// Every row is built before the first chunk is sent, so a row
    /// `to_upsert` rejects fails the call before anything reaches YB.
    pub(super) async fn upsert_batched<R: Sync>(
        &self,
        lane: &UpsertLane,
        branch_id: BranchId,
        rows: &[R],
        to_upsert: impl FnMut(&Arc<TableHandle>, usize, &R) -> Result<UpsertRow>,
        on_chunk_ok: impl Fn(&[R]),
    ) -> Result<()> {
        if rows.is_empty() {
            return Ok(());
        }
        self.write_gate(lane.what);
        let p = self.thinclient.current().await;
        // Building the rows, payload encoding included, is part of the op.
        let started = std::time::Instant::now();
        let upserts = build_upserts(&p, lane, rows, to_upsert)?;
        let parallelism = match lane.fanout {
            Fanout::Scaled(base) => pageserver_api::config::scale_count(base, self.scale_factor),
            Fanout::Serial => 1,
        };
        let chunks: Vec<_> = rows
            .chunks(lane.rows_per_chunk)
            .zip(upserts.chunks(lane.rows_per_chunk))
            .collect();

        // Fenced per chunk, not per batch (#1737). A flush grows with the
        // endpoint (768 MiB at 64 vCPU) while the lease window does not, and
        // fenced whole, a flush that needs longer than the window is refused at
        // its deadline and resubmitted whole, forever. Upserts are idempotent,
        // so a chunk resubmitted alone is safe.
        let (client, on_chunk_ok) = (&p.client, &on_chunk_ok);
        utils::write_fence::global()
            .write_fenced_each(
                lane.what,
                server_fenced,
                chunks,
                parallelism,
                |&(rows, upserts), deadline| async move {
                    upsert_chunk(client, lane, branch_id, upserts, deadline.into()).await?;
                    on_chunk_ok(rows);
                    Ok(())
                },
            )
            .await?;
        crate::metrics::YB_STORE_OP_SECONDS
            .with_label_values(&[lane.op, "yb_thin_client"])
            .observe(started.elapsed().as_secs_f64());
        Ok(())
    }
}

/// `rows` as the upserts [`PageStore::upsert_batched`] sends, in order.
pub(super) fn build_upserts<R>(
    p: &ThinClientTables,
    lane: &UpsertLane,
    rows: &[R],
    mut to_upsert: impl FnMut(&Arc<TableHandle>, usize, &R) -> Result<UpsertRow>,
) -> Result<Vec<UpsertRow>> {
    let table = (lane.table)(p);
    rows.iter()
        .enumerate()
        .map(|(i, row)| to_upsert(table, i, row))
        .collect()
}

/// Write one chunk under `deadline`: the write [`PageStore::upsert_batched`]
/// fences.
pub(super) async fn upsert_chunk(
    client: &TsClient,
    lane: &UpsertLane,
    branch_id: BranchId,
    upserts: &[UpsertRow],
    deadline: WriteDeadline,
) -> Result<()> {
    // Full retry budget (inside `upsert`): this lane has no YSQL fallback, so
    // it rides out transient failures itself. There is no per-attempt session
    // rotation — the shim owns the session pool and the tserver does the
    // routing. A terminal failure fails the write.
    client.upsert(upserts, deadline).await.with_context(|| {
        format!(
            "{}(yb_thin_client) branch={branch_id} chunk_rows={}",
            lane.op,
            upserts.len()
        )
    })
}
