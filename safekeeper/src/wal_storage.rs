//! WAL bookkeeping + reader.
//!
//! Historically this module wrote WAL to a local pg_wal-style directory.
//! yb-light keeps WAL only in YB (`safekeeper.wals`); `PhysicalStorage` is
//! now a pure bookkeeping helper used by `YbWalStorage` to track LSN
//! watermarks and feed the `WalStreamDecoder`. `WalReader` continues to
//! serve WAL bytes to the WAL sender — locally from the in-memory
//! `WalCache`, otherwise from YB.

use std::cmp::min;
use std::future::Future;
use std::pin::Pin;

use anyhow::{Result, bail};
use bytes::Bytes;
use futures::future::BoxFuture;
use postgres_ffi::waldecoder::WalStreamDecoder;
use postgres_ffi::XLogSegNo;
use postgres_versioninfo::{PgMajorVersion, PgVersionId};
use pq_proto::SystemId;
use std::sync::Arc;
use tokio::io::{AsyncRead, AsyncReadExt};
use tracing::*;
use utils::id::BranchId;
use utils::lsn::Lsn;

use crate::metrics::{WAL_STORAGE_OPERATION_SECONDS, WalStorageMetrics};
use crate::state::TimelinePersistentState;
use crate::yb_wal_storage::YbWalConn;

pub trait Storage {
    // Last written LSN.
    fn write_lsn(&self) -> Lsn;
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

    /// Durably store WAL on disk, up to the last written WAL record.
    fn flush_wal(&mut self) -> impl Future<Output = Result<()>> + Send;

    /// Flush the consumer's queue, advancing the local inner watermark; the
    /// mode selects how far to go (see [`crate::yb_wal_storage::YbFlush`]).
    /// Local-FS has no async durability, so the default delegates to the
    /// synchronous [`Self::flush_wal`].
    fn yb_flush(
        &mut self,
        mode: crate::yb_wal_storage::YbFlush,
    ) -> impl Future<Output = Result<()>> + Send {
        let _ = mode;
        self.flush_wal()
    }

    /// Watch receiver that fires as more WAL becomes durable in YugabyteDB,
    /// so the acceptor can ack the moment durability advances. `None` for
    /// local-FS (`flush_wal` is synchronous).
    fn yb_durable_watch(
        &self,
    ) -> Option<tokio::sync::watch::Receiver<crate::yb_wal_storage::DurableState>> {
        None
    }

    /// Remove all segments <= given segno. Returns function doing that as we
    /// want to perform it without timeline lock.
    fn remove_up_to(&self, segno_up_to: XLogSegNo) -> BoxFuture<'static, anyhow::Result<()>>;

    /// Release resources associated with the storage -- technically, close FDs.
    /// Currently we don't remove timelines until restart (#3146), so need to
    /// spare descriptors. This would be useful for temporary tli detach as
    /// well.
    fn close(&mut self) {}

    /// Get metrics for this timeline.
    fn get_metrics(&self) -> WalStorageMetrics;
}

/// In-memory bookkeeping for the WAL stream: write/flush watermarks plus
/// the `WalStreamDecoder` used to find WAL-record boundaries. No
/// filesystem interaction — `YbWalStorage` owns durability via YB.
///
/// Relationship of LSNs:
/// `write_lsn` >= `write_record_lsn` >= `flush_record_lsn`
pub struct PhysicalStorage {
    metrics: WalStorageMetrics,

    /// Size of WAL segment in bytes.
    wal_seg_size: usize,
    pg_version: PgVersionId,
    system_id: u64,

    /// Possibly in the middle of a WAL record.
    write_lsn: Lsn,

    /// LSN of the last fully-written WAL record.
    write_record_lsn: Lsn,

    /// Flush watermark. May be in the middle of a record.
    flush_lsn: Lsn,

    /// LSN of the last fully-flushed WAL record.
    flush_record_lsn: Lsn,

    /// Used to detect WAL-record boundaries inside the byte stream.
    decoder: WalStreamDecoder,
}

impl PhysicalStorage {
    /// Build the bookkeeping helper. `write_lsn` is supplied by the caller
    /// (`YbWalStorage` rehydrates it from `safekeeper.wals`); all watermarks
    /// start there.
    pub fn new(
        branch_id: &BranchId,
        state: &TimelinePersistentState,
        write_lsn: Lsn,
    ) -> Result<PhysicalStorage> {
        let wal_seg_size = state.server.wal_seg_size as usize;
        debug!(
            "initialized WAL bookkeeping for branch {}, write_lsn={}",
            branch_id, write_lsn,
        );
        Ok(PhysicalStorage {
            metrics: WalStorageMetrics::default(),
            wal_seg_size,
            pg_version: state.server.pg_version,
            system_id: state.server.system_id,
            write_lsn,
            write_record_lsn: write_lsn,
            flush_lsn: write_lsn,
            flush_record_lsn: write_lsn,
            decoder: WalStreamDecoder::new(
                write_lsn,
                PgMajorVersion::try_from(state.server.pg_version).unwrap(),
            ),
        })
    }

    /// Read-only access to the PG version stored on this instance.
    pub fn pg_version_id(&self) -> PgVersionId {
        self.pg_version
    }

    /// Read-only access to the system identifier carried on the segment header.
    pub fn system_id(&self) -> u64 {
        self.system_id
    }

    /// Get all known state of the storage.
    pub fn internal_state(&self) -> (Lsn, Lsn, Lsn, bool) {
        (
            self.write_lsn,
            self.write_record_lsn,
            self.flush_record_lsn,
            false,
        )
    }
}

impl Storage for PhysicalStorage {
    fn write_lsn(&self) -> Lsn {
        self.write_lsn
    }

    #[allow(clippy::misnamed_getters)]
    fn flush_lsn(&self) -> Lsn {
        self.flush_record_lsn
    }

    async fn initialize_first_segment(&mut self, init_lsn: Lsn) -> Result<()> {
        let _timer = WAL_STORAGE_OPERATION_SECONDS
            .with_label_values(&["initialize_first_segment"])
            .start_timer();
        // YB-backed `YbWalStorage` initializes the segment-header bytes in
        // YB; nothing to do at the bookkeeping layer beyond the trace.
        let segno = init_lsn.segment_number(self.wal_seg_size);
        info!(
            "initialize_first_segment: segno {} at lsn {} (YB-backed)",
            segno, init_lsn
        );
        Ok(())
    }

    /// Advance bookkeeping for `buf` bytes starting at `startpos`. Durability is
    /// the caller's responsibility (`YbWalStorage` INSERTs into `safekeeper.wals`).
    async fn write_wal(&mut self, startpos: Lsn, buf: &[u8]) -> Result<()> {
        if self.write_lsn > startpos {
            bail!(
                "write_wal rewrites WAL written before, write_lsn={}, startpos={}",
                self.write_lsn,
                startpos
            );
        }
        if self.write_lsn < startpos && self.write_lsn != Lsn(0) {
            bail!(
                "write_wal creates gap in written WAL, write_lsn={}, startpos={}",
                self.write_lsn,
                startpos
            );
        }

        self.write_lsn = startpos + buf.len() as u64;
        // No per-segment fsync barrier without a local FS; keep `flush_lsn`
        // aligned with `write_lsn` directly.
        self.flush_lsn = self.write_lsn;
        self.metrics.observe_write_bytes(buf.len());

        if self.decoder.available() != startpos {
            info!(
                "restart decoder from {} to {}",
                self.decoder.available(),
                startpos
            );
            let pg_version = self.decoder.pg_version;
            self.decoder = WalStreamDecoder::new(startpos, pg_version);
        }
        self.decoder.feed_bytes(buf);

        if self.write_record_lsn <= self.flush_lsn {
            self.flush_record_lsn = self.write_record_lsn;
        }
        while let Some((lsn, _rec)) = self.decoder.poll_decode()? {
            self.write_record_lsn = lsn;
            if lsn <= self.flush_lsn {
                self.flush_record_lsn = lsn;
            }
        }

        Ok(())
    }

    async fn flush_wal(&mut self) -> Result<()> {
        // YB-only mode: WAL is durable in YB after write_wal. Just advance
        // the in-memory flush watermarks.
        self.flush_lsn = self.write_lsn;
        self.flush_record_lsn = self.write_record_lsn;
        Ok(())
    }

    /// Reset bookkeeping watermarks to `end_pos`. Caller
    /// (`YbWalStorage::truncate_wal`) has already deleted the YB rows past
    /// `end_pos` before we get here.
    async fn truncate_wal(&mut self, end_pos: Lsn) -> Result<()> {
        let _timer = WAL_STORAGE_OPERATION_SECONDS
            .with_label_values(&["truncate_wal"])
            .start_timer();

        if self.write_record_lsn != Lsn(0) && end_pos > self.write_record_lsn {
            bail!(
                "truncate_wal called on non-written WAL, write_record_lsn={}, end_pos={}",
                self.write_record_lsn,
                end_pos
            );
        }

        if end_pos == self.write_lsn && end_pos == self.flush_record_lsn {
            return Ok(());
        }

        self.write_lsn = end_pos;
        self.flush_lsn = end_pos;
        self.write_record_lsn = end_pos;
        self.flush_record_lsn = end_pos;
        info!("truncated WAL to {}", end_pos);
        Ok(())
    }

    fn remove_up_to(&self, _segno_up_to: XLogSegNo) -> BoxFuture<'static, anyhow::Result<()>> {
        Box::pin(async move { Ok(()) })
    }

    fn close(&mut self) {}

    fn get_metrics(&self) -> WalStorageMetrics {
        self.metrics.clone()
    }
}

pub struct WalReader {
    branch_id: BranchId,
    wal_seg_size: usize,
    pos: Lsn,
    wal_segment: Option<Pin<Box<dyn AsyncRead + Send + Sync>>>,

    // WAL is read from YB (`safekeeper.wals`); yb-light writes no local files.
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
        if state.server.wal_seg_size == 0 || state.local_start_lsn == Lsn(0) {
            bail!("state uninitialized, no data to read");
        }

        // TODO: Upgrade to bail!() once we know this couldn't possibly happen
        if state.timeline_start_lsn == Lsn(0) {
            warn!("timeline_start_lsn uninitialized before initializing wal reader");
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
    /// WAL lives only in YB (`safekeeper.wals`) — yb-light writes no local
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
                &self.branch_id,
                segno,
                self.wal_seg_size,
                xlogoff,
                self.flush_lsn.clone(),
            )));
        }

        bail!("WAL segment is not found (no YB WAL connection for this reader)")
    }
}
