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
use tokio_postgres::types::ToSql;
use utils::id::TenantTimelineId;
use utils::lsn::Lsn;

use crate::metrics::WalStorageMetrics;
use crate::state::TimelinePersistentState;
use crate::wal_storage::{PhysicalStorage, Storage};

pub struct YbWalStorage {
    inner: PhysicalStorage,
    client: Arc<tokio_postgres::Client>,
    tenant_id: String,
    timeline_id: String,
    table_name: String,
    wal_seg_size: usize,
    /// Monotonic chunk counter per segno. Rehydrated from YB at construction.
    next_chunk_seq: HashMap<u64, i32>,
}

impl YbWalStorage {
    pub async fn new(
        ttid: &TenantTimelineId,
        timeline_dir: &Utf8Path,
        state: &TimelinePersistentState,
        no_sync: bool,
        client: Arc<tokio_postgres::Client>,
        table_name: String,
    ) -> Result<Self> {
        let inner = PhysicalStorage::new(ttid, timeline_dir, state, no_sync)?;
        let wal_seg_size = state.server.wal_seg_size as usize;

        let mut me = YbWalStorage {
            inner,
            client,
            tenant_id: ttid.tenant_id.to_string(),
            timeline_id: ttid.timeline_id.to_string(),
            table_name,
            wal_seg_size,
            next_chunk_seq: HashMap::new(),
        };

        // Reconcile YB with the local-FS view of where WAL ends. The inner
        // `PhysicalStorage::new` ran `find_end_of_wal` against local disk and
        // set its `flush_lsn` to that point. Any YB chunk whose end offset
        // lies past that LSN is stale (left over from a previous run that
        // crashed before its writes hit a durable Raft commit). Drop them.
        let end_of_wal = me.inner.flush_lsn();
        me.delete_after(end_of_wal).await?;
        me.next_chunk_seq = Self::load_next_chunk_seq(
            &me.client,
            &me.table_name,
            &me.tenant_id,
            &me.timeline_id,
        )
        .await?;
        Ok(me)
    }

    async fn load_next_chunk_seq(
        client: &tokio_postgres::Client,
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

    /// Split a write at segment boundaries and INSERT one row per segment-spanning
    /// piece. Each row records `chunk_offset` (byte position within the segment)
    /// and a monotonic `chunk_seq`.
    async fn upsert_writes(&mut self, mut startpos: Lsn, buf: &[u8]) -> Result<()> {
        if buf.is_empty() {
            return Ok(());
        }
        let now_us: i64 = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .map(|d| d.as_micros() as i64)
            .unwrap_or(0);

        let insert_sql = format!(
            "INSERT INTO {} (tenant_id, timeline_id, segno, chunk_seq, chunk_offset, \
                             chunk_size, data, last_modified_us) \
             VALUES ($1, $2, $3, $4, $5, $6, $7, $8)",
            quote_ident(&self.table_name)
        );

        let mut remaining = buf;
        while !remaining.is_empty() {
            let segno = startpos.segment_number(self.wal_seg_size);
            let offset = startpos.segment_offset(self.wal_seg_size);
            let room_in_segment = self.wal_seg_size - offset;
            let take = remaining.len().min(room_in_segment);
            let chunk: Vec<u8> = remaining[..take].to_vec();

            let segno_i: i64 = segno as i64;
            let seq: i32 = self.allocate_seq(segno);
            let offset_i: i64 = offset as i64;
            let size_i: i32 = take as i32;

            let params: [&(dyn ToSql + Sync); 8] = [
                &self.tenant_id,
                &self.timeline_id,
                &segno_i,
                &seq,
                &offset_i,
                &size_i,
                &chunk,
                &now_us,
            ];
            self.client
                .execute(&insert_sql, &params)
                .await
                .context("YbWalStorage: insert WAL chunk")?;

            remaining = &remaining[take..];
            startpos = Lsn(startpos.0 + take as u64);
        }
        Ok(())
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

    fn initialize_first_segment(
        &mut self,
        init_lsn: Lsn,
    ) -> impl Future<Output = Result<()>> + Send {
        self.inner.initialize_first_segment(init_lsn)
    }

    async fn write_wal(&mut self, startpos: Lsn, buf: &[u8]) -> Result<()> {
        // Push to YB first so a torn local write never reports success without
        // YB having the bytes. (If YB fails, local write doesn't happen.)
        self.upsert_writes(startpos, buf).await?;
        self.inner.write_wal(startpos, buf).await
    }

    async fn truncate_wal(&mut self, end_pos: Lsn) -> Result<()> {
        self.delete_after(end_pos).await?;
        self.inner.truncate_wal(end_pos).await
    }

    async fn flush_wal(&mut self) -> Result<()> {
        // YB writes are already durable (each write_wal awaited the INSERT).
        // Still call inner.flush_wal to fsync local-FS shadow (used by reader).
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

/// Fetch a WAL segment from `yb_storage.wals`, reassemble all chunks in
/// `chunk_seq` order (so later writes at the same offset overwrite earlier
/// ones, matching local-FS semantics), and return an `AsyncRead` positioned at
/// `start_xlogoff`.
///
/// Returns `Ok(None)` when the segment has no rows in YB (caller should fall
/// through to the next source). Returns `Ok(Some(reader))` on success.
pub async fn read_segment_from_yb(
    client: &tokio_postgres::Client,
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
