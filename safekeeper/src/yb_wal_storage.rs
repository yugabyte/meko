//! YugabyteDB-mirrored WAL storage.
//!
//! Wraps [`PhysicalStorage`] so that every WAL write is also persisted to a
//! YB table (`yb_storage.wals` by default). YB becomes the durable copy;
//! the local FS remains the read path (used by `WalReader`) and a hot cache
//! for re-reads of recent WAL.
//!
//! Phase 1 (this module): write-through mirror. `flush_wal()` doesn't return
//! until the corresponding bytes have been INSERT'd into YB.
//!
//! Phase 2 (future): make `WalReader` capable of fetching from YB, so the
//! local FS becomes truly optional.
//!
//! Schema (one row per `write_wal()` call within a segment; multiple rows
//! across a segment boundary):
//!
//! ```sql
//! CREATE TABLE wals (
//!     tenant_id        TEXT    NOT NULL,
//!     timeline_id      TEXT    NOT NULL,
//!     segno            BIGINT  NOT NULL,
//!     chunk_seq        INT     NOT NULL,   -- monotonic per (tenant, timeline, segno)
//!     chunk_offset     BIGINT  NOT NULL,   -- byte offset of `data` within the segment
//!     chunk_size       INT     NOT NULL,   -- redundant: length(data)
//!     data             BYTEA   NOT NULL,
//!     last_modified_us BIGINT  NOT NULL,
//!     PRIMARY KEY ((tenant_id, timeline_id) HASH, segno, chunk_seq)
//! );
//! ```
//!
//! Notes:
//! - `chunk_seq` is an in-memory monotonic counter per segment. On restart it
//!   is rehydrated by `SELECT max(chunk_seq) FROM wals WHERE ... GROUP BY segno`.
//! - `truncate_wal` deletes rows whose end offset (`chunk_offset + chunk_size`)
//!   lies entirely past the new end; rows that straddle the cut are left
//!   intact (the local-FS truncation is the source of truth for that case —
//!   the reader uses local FS, so a tiny over-keep in YB is harmless).
//! - `remove_up_to(segno)` deletes all rows with `segno < segno_up_to`.
//!
//! ## Batching (producer / consumer)
//!
//! `write_wal()` is the producer: it parses the incoming WAL byte range
//! into one `PendingChunk` per segment, stamps each with a monotonic
//! `chunk_seq` and an `end_lsn`, then pushes them onto a bounded `mpsc`
//! channel. It does **not** wait for YB.
//!
//! A background consumer task (spawned in `YbWalStorage::new`) drains the
//! channel greedily, builds one multi-row `INSERT ... VALUES (...), (...)`
//! per batch — up to [`FLUSH_ROWS_THRESHOLD`] rows or
//! [`FLUSH_BYTES_THRESHOLD`] bytes — runs it through [`YsqlClient`], and
//! publishes the highest end_lsn of each successful batch onto a
//! `tokio::sync::watch` watermark. This lets the producer keep pushing
//! the *next* batch while the previous one is in flight, which the
//! single-threaded design couldn't do (every `flush_pending` held
//! `&mut self` across the YB round-trip).
//!
//! `flush_wal()` is the durability sync point: it records the highest
//! end_lsn the producer has pushed (`submitted_max_lsn`) and awaits the
//! watermark catching up. The walproposer only treats data as durable
//! after a successful `flush_wal()`, so any crash between `write_wal()`
//! returning and the batch landing in YB is a no-op for the compute (no
//! commit ack was sent).
//!
//! `truncate_wal` waits the same way before issuing the DELETE, so a
//! pending chunk past `end_pos` cannot land after the truncate.
//!
//! ## Failure handling
//!
//! On a batch INSERT failure (after `YsqlClient`'s own auto-retry has
//! given up — e.g. persistent auth / schema / connectivity failure),
//! the consumer publishes [`DurableState::Failed`] onto the watermark
//! and exits. The producer's next `tx.send()` then fails (rx dropped),
//! `write_wal` returns the captured error, the walproposer reconnects,
//! and the safekeeper handles the rest via the consensus state machine.
//! `flush_wal` waiters see `Failed` via the watermark and propagate the
//! same error.
//!
//! `remove_up_to` does **not** wait — by construction it only deletes
//! segments well below the writer's current position, which the
//! channel never holds.

use std::collections::HashMap;
use std::future::Future;
use std::pin::Pin;
use std::sync::Arc;
use std::task::{Context as TaskContext, Poll};
use std::time::{SystemTime, UNIX_EPOCH};

use anyhow::{Context, Result};
use camino::Utf8Path;
use futures::future::{BoxFuture, FutureExt};
use postgres_ffi::XLogSegNo;
use tokio::io::{AsyncRead, ReadBuf};
use tokio::sync::{mpsc, watch};
use tokio_postgres::types::ToSql;
use utils::id::TenantTimelineId;
use utils::lsn::Lsn;

use crate::metrics::WalStorageMetrics;
use crate::state::TimelinePersistentState;
use crate::wal_storage::{PhysicalStorage, Storage};

// Re-export the auto-reconnecting Postgres client wrapper from the
// shared `remote_storage` crate (used by both the pageserver's
// `YbStore` and our YB-WAL mirror here). See `remote_storage::YsqlClient`
// for the reconnect / retry-once semantics.
pub use remote_storage::YsqlClient;

/// Flush the buffer when it accumulates this many chunks. Picked to amortize
/// the per-statement DocDB cost (single-row ≈ 0.6 ms; 50-row batch ≈ 2.5 ms
/// → ~12× fewer per-row ms).
const FLUSH_ROWS_THRESHOLD: usize = 50;

/// Flush the buffer when the total payload bytes cross this threshold,
/// even if `FLUSH_ROWS_THRESHOLD` hasn't been reached. Keeps memory bounded
/// and avoids ever building an INSERT that would push YB's
/// `rpc_max_message_size` (256 MiB default). 4 MiB leaves comfortable
/// headroom for the row metadata + protocol overhead.
const FLUSH_BYTES_THRESHOLD: usize = 4 * 1024 * 1024;

/// One row queued for the next batched INSERT. Carries `end_lsn` so the
/// consumer can publish a durability watermark after each batch.
struct PendingChunk {
    segno: u64,
    chunk_seq: i32,
    chunk_offset: i64,
    chunk_size: i32,
    data: Vec<u8>,
    /// LSN immediately after this chunk's last byte. The consumer uses
    /// the last chunk's `end_lsn` in a batch as the post-flush watermark.
    end_lsn: Lsn,
}

/// Capacity of the producer→consumer `mpsc` channel. Bounded so that a
/// stalled consumer back-pressures the producer (`write_wal` blocks on
/// `tx.send().await`) rather than growing memory unboundedly.
const CHUNK_CHANNEL_CAPACITY: usize = 1024;

/// Durability state published by the consumer onto the watch channel.
#[derive(Clone, Debug)]
enum DurableState {
    /// Every `PendingChunk` whose `end_lsn <= self.0` has been durably
    /// INSERT'd into YugabyteDB.
    Healthy(Lsn),
    /// The consumer task hit a non-recoverable error and stopped
    /// processing. The string is the rendered error chain for
    /// propagation up through `flush_wal` / `write_wal`.
    Failed(String),
}

pub struct YbWalStorage {
    inner: PhysicalStorage,
    /// Auto-reconnecting wrapper around `tokio_postgres::Client`. Used
    /// directly for the synchronous DELETE paths (`truncate_wal`,
    /// `remove_up_to`). The consumer task gets a clone of this `Arc` for
    /// its batched INSERTs.
    client: Arc<YsqlClient>,
    tenant_id: String,
    timeline_id: String,
    table_name: String,
    wal_seg_size: usize,
    /// Monotonic chunk counter per segno. Allocated by the producer
    /// (`write_wal`) before pushing onto the channel; consumer reads but
    /// doesn't mutate. Rehydrated from YB at construction.
    next_chunk_seq: HashMap<u64, i32>,
    /// Producer side of the chunk channel.
    tx: mpsc::Sender<PendingChunk>,
    /// Receiver side of the durability watermark.
    watermark_rx: watch::Receiver<DurableState>,
    /// Highest `end_lsn` the producer has pushed onto `tx`. `flush_wal`
    /// awaits the watermark catching up to this.
    submitted_max_lsn: Lsn,
    /// Kept so the consumer task isn't dropped immediately on
    /// `YbWalStorage` drop; closing `tx` is what signals shutdown.
    _consumer_handle: tokio::task::JoinHandle<()>,
}

impl YbWalStorage {
    pub async fn new(
        ttid: &TenantTimelineId,
        timeline_dir: &Utf8Path,
        state: &TimelinePersistentState,
        _no_sync: bool,
        client: Arc<YsqlClient>,
        table_name: String,
    ) -> Result<Self> {
        let wal_seg_size = state.server.wal_seg_size as usize;
        let tenant_id_str = ttid.tenant_id.to_string();
        let timeline_id_str = ttid.timeline_id.to_string();

        // Rehydrate `write_lsn` from YB rather than scanning local FS. Take
        // the highest `chunk_offset + chunk_size` across all segnos for this
        // (tenant, timeline) as the end-of-WAL position.
        let write_lsn = Self::load_write_lsn(
            &client,
            &table_name,
            &tenant_id_str,
            &timeline_id_str,
            wal_seg_size,
            state.commit_lsn,
        )
        .await?;

        // Construct the inner PhysicalStorage in `no_local_writes` mode so all
        // of its disk operations are no-ops. The struct still holds the
        // LSN tracker + WAL decoder, which we want to reuse.
        let inner = PhysicalStorage::new_no_local_writes(ttid, timeline_dir, state, write_lsn)?;

        // Producer/consumer plumbing. The watermark starts at
        // `write_lsn` so an immediately-following `flush_wal()` with
        // `submitted_max_lsn == write_lsn` returns at once (nothing to
        // wait for).
        let (tx, rx) = mpsc::channel::<PendingChunk>(CHUNK_CHANNEL_CAPACITY);
        let (watermark_tx, watermark_rx) =
            watch::channel(DurableState::Healthy(write_lsn));
        let consumer_handle = tokio::spawn(consumer_main(
            rx,
            client.clone(),
            table_name.clone(),
            tenant_id_str.clone(),
            timeline_id_str.clone(),
            watermark_tx,
        ));

        let mut me = YbWalStorage {
            inner,
            client,
            tenant_id: tenant_id_str,
            timeline_id: timeline_id_str,
            table_name,
            wal_seg_size,
            next_chunk_seq: HashMap::new(),
            tx,
            watermark_rx,
            submitted_max_lsn: write_lsn,
            _consumer_handle: consumer_handle,
        };

        // Clean any rows past write_lsn left over from a crash where we
        // INSERT'd but never returned to the caller. Goes direct via the
        // sync client (consumer hasn't been handed anything yet anyway).
        me.delete_after(write_lsn).await?;
        me.next_chunk_seq = Self::load_next_chunk_seq(
            &me.client,
            &me.table_name,
            &me.tenant_id,
            &me.timeline_id,
        )
        .await?;
        Ok(me)
    }

    /// Find the end of the persisted WAL by scanning the YB table for the
    /// highest end-byte across all segnos for this timeline. Falls back to
    /// `commit_lsn` when the table is empty (fresh timeline).
    async fn load_write_lsn(
        client: &YsqlClient,
        table: &str,
        tenant_id: &str,
        timeline_id: &str,
        wal_seg_size: usize,
        commit_lsn: Lsn,
    ) -> Result<Lsn> {
        let sql = format!(
            "SELECT segno, max(chunk_offset + chunk_size) AS end_off FROM {} \
             WHERE tenant_id = $1 AND timeline_id = $2 \
             GROUP BY segno ORDER BY segno DESC LIMIT 1",
            quote_ident(table)
        );
        let params: [&(dyn ToSql + Sync); 2] = [&tenant_id, &timeline_id];
        let row_opt = client
            .query_opt(&sql, &params)
            .await
            .context("YbWalStorage: rehydrate write_lsn from YB")?;
        let lsn = match row_opt {
            Some(row) => {
                let segno: i64 = row.get("segno");
                let end_off: i64 = row.get("end_off");
                Lsn((segno as u64) * (wal_seg_size as u64) + end_off as u64)
            }
            None => commit_lsn,
        };
        Ok(lsn)
    }

    async fn load_next_chunk_seq(
        client: &YsqlClient,
        table: &str,
        tenant_id: &str,
        timeline_id: &str,
    ) -> Result<HashMap<u64, i32>> {
        let sql = format!(
            "SELECT segno, max(chunk_seq) AS m FROM {} \
             WHERE tenant_id = $1 AND timeline_id = $2 GROUP BY segno",
            quote_ident(table)
        );
        let params: [&(dyn ToSql + Sync); 2] = [&tenant_id, &timeline_id];
        let rows = client
            .query(&sql, &params)
            .await
            .context("YbWalStorage: load chunk_seq state")?;
        let mut out = HashMap::new();
        for row in rows {
            let segno: i64 = row.get("segno");
            let max_seq: i32 = row.get("m");
            out.insert(segno as u64, max_seq + 1);
        }
        Ok(out)
    }

    fn allocate_seq(&mut self, segno: u64) -> i32 {
        let entry = self.next_chunk_seq.entry(segno).or_insert(0);
        let seq = *entry;
        *entry = seq + 1;
        seq
    }

    fn inner_pg_version_id(&self) -> postgres_versioninfo::PgVersionId {
        self.inner.pg_version_id()
    }

    fn inner_system_id(&self) -> u64 {
        self.inner.system_id()
    }

    /// Producer side: split the incoming WAL range at segment boundaries,
    /// stamp each piece with `chunk_seq` + `end_lsn`, and push onto the
    /// `mpsc` channel. The consumer task INSERTs them in batches; the
    /// caller does **not** wait for durability (use `flush_wal`).
    async fn upsert_writes(&mut self, mut startpos: Lsn, buf: &[u8]) -> Result<()> {
        if buf.is_empty() {
            return Ok(());
        }
        let mut remaining = buf;
        while !remaining.is_empty() {
            let segno = startpos.segment_number(self.wal_seg_size);
            let offset = startpos.segment_offset(self.wal_seg_size);
            let room_in_segment = self.wal_seg_size - offset;
            let take = remaining.len().min(room_in_segment);
            let end_lsn = Lsn(startpos.0 + take as u64);

            let pc = PendingChunk {
                segno,
                chunk_seq: self.allocate_seq(segno),
                chunk_offset: offset as i64,
                chunk_size: take as i32,
                data: remaining[..take].to_vec(),
                end_lsn,
            };

            // tx.send().await may block on backpressure (bounded channel).
            // The walproposer side stalls here until the consumer drains —
            // intended behaviour when YB can't keep up.
            if self.tx.send(pc).await.is_err() {
                // Consumer task exited. The watermark should carry the
                // captured error string.
                let state = self.watermark_rx.borrow().clone();
                match state {
                    DurableState::Failed(msg) => {
                        anyhow::bail!("YB WAL consumer task failed: {msg}");
                    }
                    DurableState::Healthy(_) => {
                        anyhow::bail!("YB WAL consumer task gone (channel closed)");
                    }
                }
            }
            // Advance the submitted watermark only after the chunk is
            // actually queued — otherwise a `flush_wal()` racing with a
            // failed `tx.send()` could wait on an LSN that no longer
            // corresponds to anything in flight.
            self.submitted_max_lsn = end_lsn;

            remaining = &remaining[take..];
            startpos = end_lsn;
        }
        Ok(())
    }

    /// Wait for the consumer to publish durability up to `target` LSN,
    /// or propagate a captured error if the consumer has failed.
    async fn wait_for_durable(&mut self, target: Lsn) -> Result<()> {
        let res = self
            .watermark_rx
            .wait_for(|state| match state {
                DurableState::Healthy(d) => *d >= target,
                DurableState::Failed(_) => true,
            })
            .await;
        let state = match res {
            Ok(guard) => guard.clone(),
            Err(_) => {
                anyhow::bail!("YB WAL consumer task gone (watermark channel closed)");
            }
        };
        match state {
            DurableState::Healthy(_) => Ok(()),
            DurableState::Failed(msg) => Err(anyhow::anyhow!(msg)),
        }
    }

    async fn delete_after(&mut self, end_pos: Lsn) -> Result<()> {
        // Drop any segment strictly past end_pos's segment, and any chunk whose
        // end byte lies past end_pos within the current segment.
        let segno_end = end_pos.segment_number(self.wal_seg_size) as i64;
        let offset_end = end_pos.segment_offset(self.wal_seg_size) as i64;
        let table = quote_ident(&self.table_name);
        let sql = format!(
            "DELETE FROM {table} \
             WHERE tenant_id = $1 AND timeline_id = $2 \
               AND ( segno > $3 \
                  OR (segno = $3 AND chunk_offset + chunk_size > $4) )"
        );
        let params: [&(dyn ToSql + Sync); 4] =
            [&self.tenant_id, &self.timeline_id, &segno_end, &offset_end];
        self.client
            .execute(&sql, &params)
            .await
            .context("YbWalStorage: truncate delete")?;
        // Reset in-memory chunk_seq state for affected segnos.
        self.next_chunk_seq.retain(|seg, _| (*seg as i64) < segno_end);
        if self.next_chunk_seq.contains_key(&(segno_end as u64)) {
            // After a partial-segment truncate we conservatively reload that
            // segment's seq from YB so chunk_seq stays monotonic.
            let cur = Self::load_next_chunk_seq(
                &self.client,
                &self.table_name,
                &self.tenant_id,
                &self.timeline_id,
            )
            .await?;
            self.next_chunk_seq = cur;
        }
        Ok(())
    }

    async fn delete_segments_up_to(&self, segno_up_to: XLogSegNo) -> Result<()> {
        let sql = format!(
            "DELETE FROM {} WHERE tenant_id = $1 AND timeline_id = $2 AND segno < $3",
            quote_ident(&self.table_name)
        );
        let segno_i: i64 = segno_up_to as i64;
        let params: [&(dyn ToSql + Sync); 3] =
            [&self.tenant_id, &self.timeline_id, &segno_i];
        self.client
            .execute(&sql, &params)
            .await
            .context("YbWalStorage: remove_up_to delete")?;
        Ok(())
    }
}

impl Storage for YbWalStorage {
    fn write_lsn(&self) -> Lsn {
        self.inner.write_lsn()
    }

    fn flush_lsn(&self) -> Lsn {
        self.inner.flush_lsn()
    }

    async fn initialize_first_segment(&mut self, init_lsn: Lsn) -> Result<()> {
        // Generate the standard PG segment header + zero padding, then push
        // it through the consumer just like a regular write. Wait for the
        // consumer to flush before returning so the 16 MiB header is
        // durable by the time the caller sees success. One-shot
        // (per-timeline init), not on the per-commit hot path.
        let segno = init_lsn.segment_number(self.wal_seg_size);
        let major_pg_version = postgres_ffi::PgMajorVersion::try_from(self.inner_pg_version_id())
            .map_err(|e| anyhow::anyhow!("invalid pg version: {e:?}"))?;
        let wal_seg = postgres_ffi::generate_wal_segment(
            segno,
            self.inner_system_id(),
            major_pg_version,
            init_lsn,
        )?;
        let end_lsn = Lsn(init_lsn.0 + wal_seg.len() as u64);
        let pc = PendingChunk {
            segno,
            chunk_seq: self.allocate_seq(segno),
            chunk_offset: 0,
            chunk_size: wal_seg.len() as i32,
            data: wal_seg.to_vec(),
            end_lsn,
        };
        self.tx
            .send(pc)
            .await
            .map_err(|_| anyhow::anyhow!("YB WAL consumer task gone"))?;
        self.submitted_max_lsn = end_lsn;
        self.wait_for_durable(end_lsn).await?;
        self.inner.initialize_first_segment(init_lsn).await
    }

    async fn write_wal(&mut self, startpos: Lsn, buf: &[u8]) -> Result<()> {
        // Push chunks onto the consumer's channel. Durability is not
        // observed yet — the caller must call `flush_wal` to wait for it.
        self.upsert_writes(startpos, buf).await?;
        self.inner.write_wal(startpos, buf).await
    }

    async fn truncate_wal(&mut self, end_pos: Lsn) -> Result<()> {
        // Drain everything the consumer has in flight first. Otherwise a
        // buffered chunk for an LSN past `end_pos` could be flushed after
        // the DELETE and silently resurrect data the walproposer intended
        // to discard.
        self.wait_for_durable(self.submitted_max_lsn).await?;
        self.delete_after(end_pos).await?;
        self.inner.truncate_wal(end_pos).await
    }

    async fn flush_wal(&mut self) -> Result<()> {
        // Durability sync point — wait for the consumer's watermark to
        // catch up to the highest LSN write_wal has pushed, then advance
        // the inner LSN watermark. The walproposer treats this as the
        // durable-commit boundary, so the flush MUST complete (or return
        // an error) before this returns.
        self.wait_for_durable(self.submitted_max_lsn).await?;
        self.inner.flush_wal().await
    }

    fn remove_up_to(&self, segno_up_to: XLogSegNo) -> BoxFuture<'static, anyhow::Result<()>> {
        let inner_fut = self.inner.remove_up_to(segno_up_to);
        let client = self.client.clone();
        let table = self.table_name.clone();
        let tenant_id = self.tenant_id.clone();
        let timeline_id = self.timeline_id.clone();
        async move {
            inner_fut.await?;
            let sql = format!(
                "DELETE FROM {} WHERE tenant_id = $1 AND timeline_id = $2 AND segno < $3",
                quote_ident(&table)
            );
            let segno_i: i64 = segno_up_to as i64;
            let params: [&(dyn ToSql + Sync); 3] = [&tenant_id, &timeline_id, &segno_i];
            client
                .execute(&sql, &params)
                .await
                .context("YbWalStorage: remove_up_to delete")?;
            Ok(())
        }
        .boxed()
    }

    fn close(&mut self) {
        self.inner.close();
    }

    fn get_metrics(&self) -> WalStorageMetrics {
        self.inner.get_metrics()
    }
}

fn quote_ident(ident: &str) -> String {
    let escaped = ident.replace('"', "\"\"");
    format!("\"{escaped}\"")
}

/// Consumer task body. Drains `rx` greedily into batches (≤
/// [`FLUSH_ROWS_THRESHOLD`] rows / ≤ [`FLUSH_BYTES_THRESHOLD`] bytes),
/// INSERTs each batch via `YsqlClient`, and publishes the highest
/// `end_lsn` of the batch onto `watermark_tx` as `DurableState::Healthy`.
///
/// On a batch failure, publishes `DurableState::Failed` and exits. The
/// producer (`upsert_writes`) detects the closed `rx` on its next
/// `tx.send().await` and propagates the captured error string upward.
///
/// Exits cleanly when `tx` is dropped (channel closes from the
/// producer side, `rx.recv()` returns `None`).
async fn consumer_main(
    mut rx: mpsc::Receiver<PendingChunk>,
    client: Arc<YsqlClient>,
    table_name: String,
    tenant_id: String,
    timeline_id: String,
    watermark_tx: watch::Sender<DurableState>,
) {
    loop {
        // Block until at least one chunk arrives.
        let first = match rx.recv().await {
            Some(c) => c,
            None => return,
        };
        let mut batch: Vec<PendingChunk> = Vec::with_capacity(FLUSH_ROWS_THRESHOLD);
        let mut batch_bytes = first.chunk_size as usize;
        batch.push(first);

        // Greedily drain more without yielding, until threshold or empty.
        while batch.len() < FLUSH_ROWS_THRESHOLD && batch_bytes < FLUSH_BYTES_THRESHOLD {
            match rx.try_recv() {
                Ok(c) => {
                    batch_bytes += c.chunk_size as usize;
                    batch.push(c);
                }
                Err(_) => break,
            }
        }

        let highest_lsn = batch
            .last()
            .map(|c| c.end_lsn)
            .expect("batch is non-empty");

        match flush_batch(&client, &table_name, &tenant_id, &timeline_id, &batch).await {
            Ok(()) => {
                // Publish watermark. Failure here means all receivers have
                // dropped — only happens during shutdown — so ignore.
                let _ = watermark_tx.send(DurableState::Healthy(highest_lsn));
            }
            Err(e) => {
                let msg = format!("{e:#}");
                tracing::error!(
                    rows = batch.len(),
                    bytes = batch_bytes,
                    "YB WAL consumer batch failed: {msg}"
                );
                let _ = watermark_tx.send(DurableState::Failed(msg));
                // Exit. The producer's next tx.send() will fail and
                // propagate the error up to the walproposer-facing path.
                return;
            }
        }
    }
}

/// Build and execute one multi-row `INSERT ... VALUES (...), (...)`
/// for the entire batch. Returns the error chain on failure (the
/// statement is atomic — all rows commit or none).
async fn flush_batch(
    client: &YsqlClient,
    table_name: &str,
    tenant_id: &str,
    timeline_id: &str,
    batch: &[PendingChunk],
) -> Result<()> {
    if batch.is_empty() {
        return Ok(());
    }
    let n = batch.len();
    let now_us: i64 = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_micros() as i64)
        .unwrap_or(0);

    let mut sql = String::with_capacity(128 + n * 56);
    sql.push_str("INSERT INTO ");
    sql.push_str(&quote_ident(table_name));
    sql.push_str(
        " (tenant_id, timeline_id, segno, chunk_seq, chunk_offset, \
          chunk_size, data, last_modified_us) VALUES ",
    );
    for i in 0..n {
        if i > 0 {
            sql.push_str(", ");
        }
        let b = i * 8;
        use std::fmt::Write as _;
        let _ = write!(
            sql,
            "(${},${},${},${},${},${},${},${})",
            b + 1,
            b + 2,
            b + 3,
            b + 4,
            b + 5,
            b + 6,
            b + 7,
            b + 8
        );
    }

    let segno_is: Vec<i64> = batch.iter().map(|p| p.segno as i64).collect();
    let seqs: Vec<i32> = batch.iter().map(|p| p.chunk_seq).collect();
    let offset_is: Vec<i64> = batch.iter().map(|p| p.chunk_offset).collect();
    let size_is: Vec<i32> = batch.iter().map(|p| p.chunk_size).collect();

    let mut params: Vec<&(dyn ToSql + Sync)> = Vec::with_capacity(n * 8);
    for i in 0..n {
        params.push(&tenant_id);
        params.push(&timeline_id);
        params.push(&segno_is[i]);
        params.push(&seqs[i]);
        params.push(&offset_is[i]);
        params.push(&size_is[i]);
        params.push(&batch[i].data);
        params.push(&now_us);
    }

    client
        .execute(&sql, &params)
        .await
        .with_context(|| format!("YB WAL batched INSERT ({n} rows)"))?;
    Ok(())
}

/// Fetch a WAL segment from `yb_storage.wals`, reassemble all chunks in
/// `chunk_seq` order (so later writes at the same offset overwrite earlier
/// ones, matching local-FS semantics), and return an `AsyncRead` positioned at
/// `start_xlogoff`.
///
/// Returns `Ok(None)` when the segment has no rows in YB (caller should fall
/// through to the next source). Returns `Ok(Some(reader))` on success.
pub async fn read_segment_from_yb(
    client: &YsqlClient,
    table: &str,
    ttid: &TenantTimelineId,
    segno: XLogSegNo,
    wal_seg_size: usize,
    start_xlogoff: usize,
) -> Result<Option<Pin<Box<dyn AsyncRead + Send + Sync>>>> {
    let sql = format!(
        "SELECT chunk_offset, chunk_size, data FROM {} \
         WHERE tenant_id = $1 AND timeline_id = $2 AND segno = $3 \
         ORDER BY chunk_seq",
        quote_ident(table)
    );
    let tenant_id_str = ttid.tenant_id.to_string();
    let timeline_id_str = ttid.timeline_id.to_string();
    let segno_i: i64 = segno as i64;
    let params: [&(dyn ToSql + Sync); 3] = [&tenant_id_str, &timeline_id_str, &segno_i];
    let rows = client
        .query(&sql, &params)
        .await
        .context("YB WAL read: query chunks")?;
    if rows.is_empty() {
        return Ok(None);
    }

    // Reassemble into a flat segment buffer. Later chunk_seqs over the same
    // byte range win (last-write semantics).
    let mut segment = vec![0u8; wal_seg_size];
    for row in &rows {
        let off: i64 = row.get("chunk_offset");
        let size: i32 = row.get("chunk_size");
        let data: Vec<u8> = row.get("data");
        let off = off as usize;
        let size = size as usize;
        if off + size > wal_seg_size {
            anyhow::bail!(
                "YB WAL chunk extends past segment: off={off} size={size} > seg_size={wal_seg_size}"
            );
        }
        segment[off..off + size].copy_from_slice(&data);
    }
    Ok(Some(Box::pin(VecAsyncRead {
        buf: segment,
        pos: start_xlogoff,
    })))
}

/// Minimal `AsyncRead` over an owned `Vec<u8>` cursor. Used to return a
/// segment reassembled from YB.
pub struct VecAsyncRead {
    buf: Vec<u8>,
    pos: usize,
}

impl AsyncRead for VecAsyncRead {
    fn poll_read(
        mut self: Pin<&mut Self>,
        _cx: &mut TaskContext<'_>,
        dst: &mut ReadBuf<'_>,
    ) -> Poll<std::io::Result<()>> {
        let remaining = self.buf.len().saturating_sub(self.pos);
        let n = remaining.min(dst.remaining());
        if n > 0 {
            dst.put_slice(&self.buf[self.pos..self.pos + n]);
            self.pos += n;
        }
        Poll::Ready(Ok(()))
    }
}

/// Dispatcher across WAL-storage backends. Mirrors `ControlFileStorage`.
pub enum WalStore {
    /// Local-FS only (existing behavior).
    Physical(PhysicalStorage),
    /// Local FS + synchronous mirror to YB.
    Yb(YbWalStorage),
}

impl WalStore {
    /// Forwarded passthrough for `PhysicalStorage::internal_state`. Used by
    /// debug-dump / metrics endpoints.
    pub fn internal_state(&self) -> (Lsn, Lsn, Lsn, bool) {
        match self {
            WalStore::Physical(s) => s.internal_state(),
            WalStore::Yb(s) => s.inner.internal_state(),
        }
    }
}

impl Storage for WalStore {
    fn write_lsn(&self) -> Lsn {
        match self {
            WalStore::Physical(s) => s.write_lsn(),
            WalStore::Yb(s) => s.write_lsn(),
        }
    }

    fn flush_lsn(&self) -> Lsn {
        match self {
            WalStore::Physical(s) => s.flush_lsn(),
            WalStore::Yb(s) => s.flush_lsn(),
        }
    }

    async fn initialize_first_segment(&mut self, init_lsn: Lsn) -> Result<()> {
        match self {
            WalStore::Physical(s) => s.initialize_first_segment(init_lsn).await,
            WalStore::Yb(s) => s.initialize_first_segment(init_lsn).await,
        }
    }

    async fn write_wal(&mut self, startpos: Lsn, buf: &[u8]) -> Result<()> {
        match self {
            WalStore::Physical(s) => s.write_wal(startpos, buf).await,
            WalStore::Yb(s) => s.write_wal(startpos, buf).await,
        }
    }

    async fn truncate_wal(&mut self, end_pos: Lsn) -> Result<()> {
        match self {
            WalStore::Physical(s) => s.truncate_wal(end_pos).await,
            WalStore::Yb(s) => s.truncate_wal(end_pos).await,
        }
    }

    async fn flush_wal(&mut self) -> Result<()> {
        match self {
            WalStore::Physical(s) => s.flush_wal().await,
            WalStore::Yb(s) => s.flush_wal().await,
        }
    }

    fn remove_up_to(&self, segno_up_to: XLogSegNo) -> BoxFuture<'static, anyhow::Result<()>> {
        match self {
            WalStore::Physical(s) => s.remove_up_to(segno_up_to),
            WalStore::Yb(s) => s.remove_up_to(segno_up_to),
        }
    }

    fn close(&mut self) {
        match self {
            WalStore::Physical(s) => s.close(),
            WalStore::Yb(s) => s.close(),
        }
    }

    fn get_metrics(&self) -> WalStorageMetrics {
        match self {
            WalStore::Physical(s) => s.get_metrics(),
            WalStore::Yb(s) => s.get_metrics(),
        }
    }
}
