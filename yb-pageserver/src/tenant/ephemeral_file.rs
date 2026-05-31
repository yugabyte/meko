//! Implementation of append-only file data structure
//! used to keep in-memory layers spilled on disk.

use std::io;
use std::sync::Arc;
use std::sync::atomic::{AtomicU64, Ordering};

use camino::Utf8PathBuf;
use num_traits::Num;
use pageserver_api::shard::TenantShardId;
use tokio_epoll_uring::{BoundedBuf, Slice};
use tokio_util::sync::CancellationToken;
use tracing::{error, info_span};
use utils::id::TimelineId;
use utils::sync::gate::GateGuard;

use crate::assert_u64_eq_usize::{U64IsUsize, UsizeIsU64};
use crate::config::PageServerConf;
use crate::context::RequestContext;
use crate::page_cache;
use crate::tenant::storage_layer::inmemory_layer::GlobalResourceUnits;
use crate::virtual_file::owned_buffers_io::io_buf_aligned::IoBufAlignedMut;
use crate::virtual_file::owned_buffers_io::slice::SliceMutExt;
use crate::virtual_file::owned_buffers_io::write::{Buffer, FlushTaskError};
use crate::virtual_file::{self, IoBufferMut, TempVirtualFile, VirtualFile, owned_buffers_io};

use self::owned_buffers_io::write::OwnedAsyncWriter;

pub struct EphemeralFile {
    _tenant_shard_id: TenantShardId,
    _timeline_id: TimelineId,
    page_cache_file_id: page_cache::FileId,
    file: TempVirtualFileCoOwnedByEphemeralFileAndBufferedWriter,

    buffered_writer: tokio::sync::RwLock<BufferedWriter>,

    bytes_written: AtomicU64,

    resource_units: std::sync::Mutex<GlobalResourceUnits>,
}

type BufferedWriter = owned_buffers_io::write::BufferedWriter<
    IoBufferMut,
    TempVirtualFileCoOwnedByEphemeralFileAndBufferedWriter,
>;

/// A TempVirtualFile that is co-owned by the [`EphemeralFile`]` and [`BufferedWriter`].
///
/// (Actually [`BufferedWriter`] internally is just a client to a background flush task.
/// The co-ownership is between [`EphemeralFile`] and that flush task.)
///
/// Co-ownership allows us to serve reads for data that has already been flushed by the [`BufferedWriter`].
#[derive(Debug, Clone)]
struct TempVirtualFileCoOwnedByEphemeralFileAndBufferedWriter {
    inner: Arc<TempVirtualFile>,
}

const TAIL_SZ: usize = 64 * 1024;

impl EphemeralFile {
    pub async fn create(
        conf: &PageServerConf,
        tenant_shard_id: TenantShardId,
        timeline_id: TimelineId,
        gate: &utils::sync::gate::Gate,
        cancel: &CancellationToken,
        ctx: &RequestContext,
    ) -> anyhow::Result<EphemeralFile> {
        // TempVirtualFile requires us to never reuse a filename while an old
        // instance of TempVirtualFile created with that filename is not done dropping yet.
        // So, we use a monotonic counter to disambiguate the filenames.
        static NEXT_TEMP_DISAMBIGUATOR: AtomicU64 = AtomicU64::new(1);
        let filename_disambiguator =
            NEXT_TEMP_DISAMBIGUATOR.fetch_add(1, std::sync::atomic::Ordering::Relaxed);

        let filename = conf
            .timeline_path(&tenant_shard_id, &timeline_id)
            .join(Utf8PathBuf::from(format!(
                "ephemeral-{filename_disambiguator}"
            )));

        let file = TempVirtualFileCoOwnedByEphemeralFileAndBufferedWriter::new(
            VirtualFile::open_with_options_v2(
                &filename,
                virtual_file::OpenOptions::new()
                    .create_new(true)
                    .read(true)
                    .write(true),
                ctx,
            )
            .await?,
            gate.enter()?,
        );

        let page_cache_file_id = page_cache::next_file_id(); // XXX get rid, we're not page-caching anymore

        Ok(EphemeralFile {
            _tenant_shard_id: tenant_shard_id,
            _timeline_id: timeline_id,
            page_cache_file_id,
            file: file.clone(),
            buffered_writer: tokio::sync::RwLock::new(BufferedWriter::new(
                file,
                0,
                || IoBufferMut::with_capacity(TAIL_SZ),
                gate.enter()?,
                cancel.child_token(),
                ctx,
                info_span!(parent: None, "ephemeral_file_buffered_writer", tenant_id=%tenant_shard_id.tenant_id, shard_id=%tenant_shard_id.shard_slug(), timeline_id=%timeline_id, path = %filename),
            )),
            bytes_written: AtomicU64::new(0),
            resource_units: std::sync::Mutex::new(GlobalResourceUnits::new()),
        })
    }
}

impl TempVirtualFileCoOwnedByEphemeralFileAndBufferedWriter {
    fn new(file: VirtualFile, gate_guard: GateGuard) -> Self {
        Self {
            inner: Arc::new(TempVirtualFile::new(file, gate_guard)),
        }
    }
}

impl OwnedAsyncWriter for TempVirtualFileCoOwnedByEphemeralFileAndBufferedWriter {
    fn write_all_at<Buf: owned_buffers_io::io_buf_aligned::IoBufAligned + Send>(
        &self,
        buf: owned_buffers_io::io_buf_ext::FullSlice<Buf>,
        offset: u64,
        ctx: &RequestContext,
    ) -> impl std::future::Future<
        Output = (
            owned_buffers_io::io_buf_ext::FullSlice<Buf>,
            std::io::Result<()>,
        ),
    > + Send {
        self.inner.write_all_at(buf, offset, ctx)
    }

    fn set_len(
        &self,
        len: u64,
        ctx: &RequestContext,
    ) -> impl Future<Output = std::io::Result<()>> + Send {
        self.inner.set_len(len, ctx)
    }
}

impl std::ops::Deref for TempVirtualFileCoOwnedByEphemeralFileAndBufferedWriter {
    type Target = VirtualFile;

    fn deref(&self) -> &Self::Target {
        &self.inner
    }
}

#[derive(Debug, thiserror::Error)]
pub(crate) enum EphemeralFileWriteError {
    #[error("cancelled")]
    Cancelled,
}

impl EphemeralFile {
    pub(crate) fn len(&self) -> u64 {
        // TODO(vlad): The value returned here is not always correct if
        // we have more than one concurrent writer. Writes are always
        // sequenced, but we could grab the buffered writer lock if we wanted
        // to.
        self.bytes_written.load(Ordering::Acquire)
    }

    pub(crate) fn page_cache_file_id(&self) -> page_cache::FileId {
        self.page_cache_file_id
    }

    /// Returns the offset at which the first byte of the input was written, for use
    /// in constructing indices over the written value.
    ///
    /// Panics if the write is short because there's no way we can recover from that.
    /// TODO: make upstack handle this as an error.
    pub(crate) async fn write_raw(
        &self,
        srcbuf: &[u8],
        ctx: &RequestContext,
    ) -> Result<u64, EphemeralFileWriteError> {
        let (pos, control) = self.write_raw_controlled(srcbuf, ctx).await?;
        if let Some(control) = control {
            control.release().await;
        }
        Ok(pos)
    }

    async fn write_raw_controlled(
        &self,
        srcbuf: &[u8],
        ctx: &RequestContext,
    ) -> Result<(u64, Option<owned_buffers_io::write::FlushControl>), EphemeralFileWriteError> {
        let mut writer = self.buffered_writer.write().await;

        let (nwritten, control) = writer
            .write_buffered_borrowed_controlled(srcbuf, ctx)
            .await
            .map_err(|e| match e {
                FlushTaskError::Cancelled => EphemeralFileWriteError::Cancelled,
            })?;
        assert_eq!(
            nwritten,
            srcbuf.len(),
            "buffered writer has no short writes"
        );

        // There's no realistic risk of overflow here. We won't have exabytes sized files on disk.
        let pos = self
            .bytes_written
            .fetch_add(srcbuf.len().into_u64(), Ordering::AcqRel);

        let mut resource_units = self.resource_units.lock().unwrap();
        resource_units.maybe_publish_size(self.bytes_written.load(Ordering::Relaxed));

        Ok((pos, control))
    }
}

