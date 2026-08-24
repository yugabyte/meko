//! WAL storage trait + reader.
//!
//! Historically this module wrote WAL to a local pg_wal-style directory.
//! yb-amp keeps WAL only in YB (`amp_storage.wals`), so the only implementor of
//! [`Storage`] is `YbWalStorage`; the trait stays because `SafeKeeper` is generic
//! over it and its unit tests substitute a dummy. `WalReader` serves WAL bytes to
//! the WAL sender — locally from the in-memory `WalCache`, otherwise from YB.

use std::cmp::min;
use std::future::Future;
use std::pin::Pin;

use anyhow::{Result, bail};
use bytes::Bytes;
use futures::future::BoxFuture;
use postgres_ffi::XLogSegNo;
use postgres_versioninfo::PgMajorVersion;
use pq_proto::SystemId;
use std::sync::Arc;
use tokio::io::{AsyncRead, AsyncReadExt};
use utils::id::BranchId;
use utils::lsn::Lsn;

use crate::metrics::WalStorageMetrics;
use crate::state::TimelinePersistentState;
use crate::yb_wal_storage::YbWalConn;

pub trait Storage {
    /// End of the byte stream accepted so far; may sit mid-record.
    fn accepted_byte_end(&self) -> Lsn;
    /// LSN of last durably stored WAL record.
    fn flush_lsn(&self) -> Lsn;

    /// Initialize segment by creating proper long header at the beginning of
    /// the segment and short header at the page of given LSN. This is only used
    /// for timeline initialization because compute will stream data only since
    /// init_lsn. Other segment headers are included in compute stream.
    fn initialize_first_segment(
        &mut self,
        init_lsn: Lsn,
    ) -> impl Future<Output = Result<()>> + Send;

    /// Write piece of WAL from buf to disk, but not necessarily sync it.
    fn write_wal(&mut self, startpos: Lsn, buf: &[u8]) -> impl Future<Output = Result<()>> + Send;

    /// Truncate WAL at specified LSN, which must be the end of WAL record.
    fn truncate_wal(&mut self, end_pos: Lsn) -> impl Future<Output = Result<()>> + Send;

    /// Durably store WAL up to the last written WAL record.
    fn flush_wal(&mut self) -> impl Future<Output = Result<()>> + Send;

    /// Flush the consumer's queue; the mode selects how far to wait (see
    /// [`crate::yb_wal_storage::YbFlush`]). The default delegates to the
    /// synchronous [`Self::flush_wal`], for storage that carries no async
    /// durability of its own.
    fn yb_flush(
        &mut self,
        mode: crate::yb_wal_storage::YbFlush,
    ) -> impl Future<Output = Result<()>> + Send {
        let _ = mode;
        self.flush_wal()
    }

    /// Watch receiver that fires as more WAL becomes durable in YugabyteDB,
    /// so the acceptor can ack the moment durability advances. `None` when
    /// the implementation flushes synchronously and has nothing to watch.
    fn yb_durable_watch(
        &self,
    ) -> Option<tokio::sync::watch::Receiver<crate::yb_wal_storage::DurableState>> {
        None
    }

    /// Remove all segments <= given segno. Returns function doing that as we
    /// want to perform it without timeline lock.
    fn remove_up_to(&self, segno_up_to: XLogSegNo) -> BoxFuture<'static, anyhow::Result<()>>;

    /// Get metrics for this timeline.
    fn get_metrics(&self) -> WalStorageMetrics;
}

pub struct WalReader {
    branch_id: BranchId,
    wal_seg_size: usize,
    pos: Lsn,
    wal_segment: Option<Pin<Box<dyn AsyncRead + Send + Sync>>>,

    // WAL is read from YB (`amp_storage.wals`); yb-amp writes no local files.
    yb_wal: Option<Arc<YbWalConn>>,

    // Live flush (committed) frontier, handed to every `YbSegmentReader` this
    // reader opens so it never serves bytes that were uncommitted when fetched
    // (which a later walproposer term may rewrite — #139). The streaming WAL
    // reader keeps this advancing; a one-shot reader seeds it with its fixed
    // end bound.
    flush_lsn: Arc<std::sync::atomic::AtomicU64>,

    // We respond with zero-ed bytes before this Lsn as long as
    // pos is in the same segment as timeline_start_lsn.
    timeline_start_lsn: Lsn,
    // integer version number of PostgreSQL, e.g. 14; 15; 16
    pg_version: PgMajorVersion,
    system_id: SystemId,
    timeline_start_segment: Option<Bytes>,
}

impl WalReader {
    pub fn new(
        branch_id: &BranchId,
        state: &TimelinePersistentState,
        start_pos: Lsn,
        yb_wal: Option<Arc<YbWalConn>>,
        flush_lsn: Arc<std::sync::atomic::AtomicU64>,
    ) -> Result<Self> {
        if state.server.wal_seg_size == 0 || state.timeline_start_lsn == Lsn(0) {
            bail!("state uninitialized, no data to read");
        }

        if start_pos
            < state
                .timeline_start_lsn
                .segment_lsn(state.server.wal_seg_size as usize)
        {
            bail!(
                "Requested streaming from {}, which is before the start of the timeline {}, and also doesn't start at the first segment of that timeline",
                start_pos,
                state.timeline_start_lsn
            );
        }

        Ok(Self {
            branch_id: *branch_id,
            wal_seg_size: state.server.wal_seg_size as usize,
            pos: start_pos,
            wal_segment: None,
            flush_lsn,
            yb_wal,
            timeline_start_lsn: state.timeline_start_lsn,
            pg_version: PgMajorVersion::try_from(state.server.pg_version).unwrap(),
            system_id: state.server.system_id,
            timeline_start_segment: None,
        })
    }

    /// Read WAL at current position into provided buf, returns number of bytes
    /// read. It can be smaller than buf size only if segment boundary is
    /// reached.
    pub async fn read(&mut self, buf: &mut [u8]) -> Result<usize> {
        // If this timeline is new, we may not have a full segment yet, so
        // we pad the first bytes of the timeline's first WAL segment with 0s
        if self.pos < self.timeline_start_lsn {
            debug_assert_eq!(
                self.pos.segment_number(self.wal_seg_size),
                self.timeline_start_lsn.segment_number(self.wal_seg_size)
            );

            // All bytes after timeline_start_lsn are in WAL, but those before
            // are not, so we manually construct an empty segment for the bytes
            // not available in this timeline.
            if self.timeline_start_segment.is_none() {
                let it = postgres_ffi::generate_wal_segment(
                    self.timeline_start_lsn.segment_number(self.wal_seg_size),
                    self.system_id,
                    self.pg_version,
                    self.timeline_start_lsn,
                )?;
                self.timeline_start_segment = Some(it);
            }

            assert!(self.timeline_start_segment.is_some());
            let segment = self.timeline_start_segment.take().unwrap();

            let seg_bytes = &segment[..];

            // How much of the current segment have we already consumed?
            let pos_seg_offset = self.pos.segment_offset(self.wal_seg_size);

            // How many bytes may we consume in total?
            let tl_start_seg_offset = self.timeline_start_lsn.segment_offset(self.wal_seg_size);

            debug_assert!(seg_bytes.len() > pos_seg_offset);
            debug_assert!(seg_bytes.len() > tl_start_seg_offset);

            // Copy as many bytes as possible into the buffer
            let len = (tl_start_seg_offset - pos_seg_offset).min(buf.len());
            buf[0..len].copy_from_slice(&seg_bytes[pos_seg_offset..pos_seg_offset + len]);

            self.pos += len as u64;

            // If we're done with the segment, we can release it's memory.
            // However, if we're not yet done, store it so that we don't have to
            // construct the segment the next time this function is called.
            if self.pos < self.timeline_start_lsn {
                self.timeline_start_segment = Some(segment);
            }

            return Ok(len);
        }

        let mut wal_segment = match self.wal_segment.take() {
            Some(reader) => reader,
            None => self.open_segment().await?,
        };

        // How much to read and send in message? We cannot cross the WAL file
        // boundary, and we don't want send more than provided buffer.
        let xlogoff = self.pos.segment_offset(self.wal_seg_size);
        let send_size = min(buf.len(), self.wal_seg_size - xlogoff);

        // Read some data from the file.
        let buf = &mut buf[0..send_size];
        let send_size = wal_segment.read_exact(buf).await?;
        self.pos += send_size as u64;

        // Decide whether to reuse this file. If we don't set wal_segment here
        // a new reader will be opened next time.
        if self.pos.segment_offset(self.wal_seg_size) != 0 {
            self.wal_segment = Some(wal_segment);
        }

        Ok(send_size)
    }

    /// Open WAL segment at the current position of the reader.
    ///
    /// WAL lives only in YB (`amp_storage.wals`) — yb-amp writes no local
    /// segment files. `YbSegmentReader` serves the segment lazily, hitting
    /// the in-memory `WalCache` first and falling through to YB on a miss.
    async fn open_segment(&self) -> Result<Pin<Box<dyn AsyncRead + Send + Sync>>> {
        let xlogoff = self.pos.segment_offset(self.wal_seg_size);
        let segno = self.pos.segment_number(self.wal_seg_size);

        if let Some(yb_wal) = self.yb_wal.clone() {
            tracing::info!(
                "WalReader: serving segment {segno} (offset {xlogoff}) from YB table `{}` (cache {})",
                yb_wal.table,
                if yb_wal.cache.is_some() {
                    "enabled"
                } else {
                    "disabled"
                }
            );
            return Ok(Box::pin(crate::yb_wal_storage::YbSegmentReader::new(
                yb_wal,
                self.branch_id,
                segno,
                self.wal_seg_size,
                xlogoff,
                self.flush_lsn.clone(),
            )));
        }

        bail!("WAL segment is not found (no YB WAL connection for this reader)")
    }
}
