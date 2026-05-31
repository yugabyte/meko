//! An ImageLayer represents an image or a snapshot of a key-range at
//! one particular LSN.
//!
//! It contains an image of all key-value pairs in its key-range. Any key
//! that falls into the image layer's range but does not exist in the layer,
//! does not exist.
//!
//! An image layer is stored in a file on disk. The file is stored in
//! timelines/<timeline_id> directory.  Currently, there are no
//! subdirectories, and each image layer file is named like this:
//!
//! ```text
//!    <key start>-<key end>__<LSN>
//! ```
//!
//! For example:
//!
//! ```text
//!    000000067F000032BE0000400000000070B6-000000067F000032BE0000400000000080B6__00000000346BC568
//! ```
//!
//! Every image layer file consists of three parts: "summary",
//! "index", and "values".  The summary is a fixed size header at the
//! beginning of the file, and it contains basic information about the
//! layer, and offsets to the other parts. The "index" is a B-tree,
//! mapping from Key to an offset in the "values" part.  The
//! actual page images are stored in the "values" part.
use std::fs::File;
use std::ops::Range;
use std::os::unix::prelude::FileExt;
use std::str::FromStr;
use std::sync::Arc;
use std::sync::atomic::AtomicU64;

use anyhow::{Context, Result, bail, ensure};
use bytes::Bytes;
use camino::{Utf8Path, Utf8PathBuf};
use hex;
use pageserver_api::config::MaxVectoredReadBytes;
use pageserver_api::key::{KEY_SIZE, Key};
use pageserver_api::shard::TenantShardId;
use serde::{Deserialize, Serialize};
use tokio::sync::OnceCell;
use tokio_util::sync::CancellationToken;
use tracing::*;
use utils::bin_ser::BeSer;
use utils::bin_ser::SerializeError;
use utils::id::{TenantId, TimelineId};
use utils::lsn::Lsn;

use super::errors::PutError;
use super::layer_name::ImageLayerName;
use super::{
    AsLayerDesc, LayerName, PersistentLayerDesc,
};
use crate::config::PageServerConf;
use crate::context::RequestContext;
use crate::page_cache::{self, FileId, PAGE_SZ};
use crate::tenant::blob_io::BlobWriter;
use crate::tenant::block_io::{BlockBuf, FileBlockReader};
use crate::tenant::disk_btree::{
    DiskBtreeBuilder, DiskBtreeReader, VisitDirection,
};
use crate::virtual_file::TempVirtualFile;
use crate::virtual_file::owned_buffers_io::io_buf_ext::IoBufExt;
use crate::virtual_file::owned_buffers_io::write::{Buffer, BufferedWriterShutdownMode};
use crate::virtual_file::{self, IoBuffer, IoBufferMut, MaybeFatalIo, VirtualFile};
use crate::{IMAGE_FILE_MAGIC, STORAGE_FORMAT_VERSION, TEMP_FILE_SUFFIX};

///
/// Header stored in the beginning of the file
///
/// After this comes the 'values' part, starting on block 1. After that,
/// the 'index' starts at the block indicated by 'index_start_blk'
///
#[derive(Debug, Serialize, Deserialize, PartialEq, Eq)]
pub struct Summary {
    /// Magic value to identify this as a neon image file. Always IMAGE_FILE_MAGIC.
    pub magic: u16,
    pub format_version: u16,

    pub tenant_id: TenantId,
    pub timeline_id: TimelineId,
    pub key_range: Range<Key>,
    pub lsn: Lsn,

    /// Block number where the 'index' part of the file begins.
    pub index_start_blk: u32,
    /// Block within the 'index', where the B-tree root page is stored
    pub index_root_blk: u32,
    // the 'values' part starts after the summary header, on block 1.
}

impl From<&ImageLayer> for Summary {
    fn from(layer: &ImageLayer) -> Self {
        Self::expected(
            layer.desc.tenant_shard_id.tenant_id,
            layer.desc.timeline_id,
            layer.desc.key_range.clone(),
            layer.lsn,
        )
    }
}

impl Summary {
    /// Serializes the summary header into an aligned buffer of lenth `PAGE_SZ`.
    pub fn ser_into_page(&self) -> Result<IoBuffer, SerializeError> {
        let mut buf = IoBufferMut::with_capacity(PAGE_SZ);
        Self::ser_into(self, &mut buf)?;
        // Pad zeroes to the buffer so the length is a multiple of the alignment.
        buf.extend_with(0, buf.capacity() - buf.len());
        Ok(buf.freeze())
    }

    pub(super) fn expected(
        tenant_id: TenantId,
        timeline_id: TimelineId,
        key_range: Range<Key>,
        lsn: Lsn,
    ) -> Self {
        Self {
            magic: IMAGE_FILE_MAGIC,
            format_version: STORAGE_FORMAT_VERSION,
            tenant_id,
            timeline_id,
            key_range,
            lsn,

            index_start_blk: 0,
            index_root_blk: 0,
        }
    }
}

/// This is used only from `pagectl`. Within pageserver, all layers are
/// [`crate::tenant::storage_layer::Layer`], which can hold an [`ImageLayerInner`].
pub struct ImageLayer {
    path: Utf8PathBuf,
    pub desc: PersistentLayerDesc,
    // This entry contains an image of all pages as of this LSN, should be the same as desc.lsn
    pub lsn: Lsn,
    inner: OnceCell<ImageLayerInner>,
}

impl std::fmt::Debug for ImageLayer {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        use super::RangeDisplayDebug;

        f.debug_struct("ImageLayer")
            .field("key_range", &RangeDisplayDebug(&self.desc.key_range))
            .field("file_size", &self.desc.file_size)
            .field("lsn", &self.lsn)
            .field("inner", &self.inner)
            .finish()
    }
}

/// ImageLayer is the in-memory data structure associated with an on-disk image
/// file.
pub struct ImageLayerInner {
    // values copied from summary
    index_start_blk: u32,
    index_root_blk: u32,

    file: Arc<VirtualFile>,
    file_id: FileId,
}

impl std::fmt::Debug for ImageLayerInner {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("ImageLayerInner")
            .field("index_start_blk", &self.index_start_blk)
            .field("index_root_blk", &self.index_root_blk)
            .finish()
    }
}

impl ImageLayerInner {
    pub(super) async fn dump(&self, ctx: &RequestContext) -> anyhow::Result<()> {
        let block_reader = FileBlockReader::new(&self.file, self.file_id);
        let tree_reader = DiskBtreeReader::<_, KEY_SIZE>::new(
            self.index_start_blk,
            self.index_root_blk,
            block_reader,
        );

        tree_reader.dump(ctx).await?;

        tree_reader
            .visit(
                &[0u8; KEY_SIZE],
                VisitDirection::Forwards,
                |key, value| {
                    println!("key: {} offset {}", hex::encode(key), value);
                    true
                },
                ctx,
            )
            .await?;

        Ok(())
    }
}

/// Boilerplate to implement the Layer trait, always use layer_desc for persistent layers.
impl std::fmt::Display for ImageLayer {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "{}", self.layer_desc().short_id())
    }
}

impl AsLayerDesc for ImageLayer {
    fn layer_desc(&self) -> &PersistentLayerDesc {
        &self.desc
    }
}

impl ImageLayer {
    pub async fn dump(&self, verbose: bool, ctx: &RequestContext) -> Result<()> {
        self.desc.dump();

        if !verbose {
            return Ok(());
        }

        let inner = self.load(ctx).await?;

        inner.dump(ctx).await?;

        Ok(())
    }

    fn temp_path_for(
        conf: &PageServerConf,
        timeline_id: TimelineId,
        tenant_shard_id: TenantShardId,
        fname: &ImageLayerName,
    ) -> Utf8PathBuf {
        // TempVirtualFile requires us to never reuse a filename while an old
        // instance of TempVirtualFile created with that filename is not done dropping yet.
        // So, we use a monotonic counter to disambiguate the filenames.
        static NEXT_TEMP_DISAMBIGUATOR: AtomicU64 = AtomicU64::new(1);
        let filename_disambiguator =
            NEXT_TEMP_DISAMBIGUATOR.fetch_add(1, std::sync::atomic::Ordering::Relaxed);

        conf.timeline_path(&tenant_shard_id, &timeline_id)
            .join(format!(
                "{fname}.{filename_disambiguator:x}.{TEMP_FILE_SUFFIX}"
            ))
    }

    ///
    /// Open the underlying file and read the metadata into memory, if it's
    /// not loaded already.
    ///
    async fn load(&self, ctx: &RequestContext) -> Result<&ImageLayerInner> {
        self.inner
            .get_or_try_init(|| self.load_inner(ctx))
            .await
            .with_context(|| format!("Failed to load image layer {}", self.path()))
    }

    async fn load_inner(&self, ctx: &RequestContext) -> Result<ImageLayerInner> {
        let path = self.path();

        let loaded =
            ImageLayerInner::load(&path, self.desc.image_layer_lsn(), None, None, ctx).await?;

        // not production code
        let actual_layer_name = LayerName::from_str(path.file_name().unwrap()).unwrap();
        let expected_layer_name = self.layer_desc().layer_name();

        if actual_layer_name != expected_layer_name {
            println!("warning: filename does not match what is expected from in-file summary");
            println!("actual: {:?}", actual_layer_name.to_string());
            println!("expected: {:?}", expected_layer_name.to_string());
        }

        Ok(loaded)
    }

    /// Create an ImageLayer struct representing an existing file on disk.
    ///
    /// This variant is only used for debugging purposes, by the 'pagectl' binary.
    pub fn new_for_path(path: &Utf8Path, file: File) -> Result<ImageLayer> {
        let mut summary_buf = vec![0; PAGE_SZ];
        file.read_exact_at(&mut summary_buf, 0)?;
        let summary = Summary::des_prefix(&summary_buf)?;
        let metadata = file
            .metadata()
            .context("get file metadata to determine size")?;

        // This function is never used for constructing layers in a running pageserver,
        // so it does not need an accurate TenantShardId.
        let tenant_shard_id = TenantShardId::unsharded(summary.tenant_id);

        Ok(ImageLayer {
            path: path.to_path_buf(),
            desc: PersistentLayerDesc::new_img(
                tenant_shard_id,
                summary.timeline_id,
                summary.key_range,
                summary.lsn,
                metadata.len(),
            ), // Now we assume image layer ALWAYS covers the full range. This may change in the future.
            lsn: summary.lsn,
            inner: OnceCell::new(),
        })
    }

    fn path(&self) -> Utf8PathBuf {
        self.path.clone()
    }
}

#[derive(thiserror::Error, Debug)]
pub enum RewriteSummaryError {
    #[error("magic mismatch")]
    MagicMismatch,
    #[error(transparent)]
    Other(#[from] anyhow::Error),
}

impl From<std::io::Error> for RewriteSummaryError {
    fn from(e: std::io::Error) -> Self {
        Self::Other(anyhow::anyhow!(e))
    }
}

impl ImageLayer {
    pub async fn rewrite_summary<F>(
        path: &Utf8Path,
        rewrite: F,
        ctx: &RequestContext,
    ) -> Result<(), RewriteSummaryError>
    where
        F: Fn(Summary) -> Summary,
    {
        let file = VirtualFile::open_with_options_v2(
            path,
            virtual_file::OpenOptions::new().read(true).write(true),
            ctx,
        )
        .await
        .with_context(|| format!("Failed to open file '{path}'"))?;
        let file_id = page_cache::next_file_id();
        let block_reader = FileBlockReader::new(&file, file_id);
        let summary_blk = block_reader.read_blk(0, ctx).await?;
        let actual_summary = Summary::des_prefix(summary_blk.as_ref()).context("deserialize")?;
        if actual_summary.magic != IMAGE_FILE_MAGIC {
            return Err(RewriteSummaryError::MagicMismatch);
        }

        let new_summary = rewrite(actual_summary);

        let buf = new_summary.ser_into_page().context("serialize")?;
        let (_buf, res) = file.write_all_at(buf.slice_len(), 0, ctx).await;
        res?;
        Ok(())
    }
}

impl ImageLayerInner {
    pub(super) async fn load(
        path: &Utf8Path,
        _lsn: Lsn,
        summary: Option<Summary>,
        _max_vectored_read_bytes: Option<MaxVectoredReadBytes>,
        ctx: &RequestContext,
    ) -> anyhow::Result<Self> {
        let file = Arc::new(
            VirtualFile::open_v2(path, ctx)
                .await
                .context("open layer file")?,
        );
        let file_id = page_cache::next_file_id();
        let block_reader = FileBlockReader::new(&file, file_id);
        let summary_blk = block_reader
            .read_blk(0, ctx)
            .await
            .context("read first block")?;

        // length is the only way how this could fail, so it's not actually likely at all unless
        // read_blk returns wrong sized block.
        //
        // TODO: confirm and make this into assertion
        let actual_summary =
            Summary::des_prefix(summary_blk.as_ref()).context("deserialize first block")?;

        if let Some(mut expected_summary) = summary {
            // production code path
            expected_summary.index_start_blk = actual_summary.index_start_blk;
            expected_summary.index_root_blk = actual_summary.index_root_blk;
            // mask out the timeline_id, but still require the layers to be from the same tenant
            expected_summary.timeline_id = actual_summary.timeline_id;

            if actual_summary != expected_summary {
                bail!(
                    "in-file summary does not match expected summary. actual = {:?} expected = {:?}",
                    actual_summary,
                    expected_summary
                );
            }
        }

        Ok(ImageLayerInner {
            index_start_blk: actual_summary.index_start_blk,
            index_root_blk: actual_summary.index_root_blk,
            file,
            file_id,
        })
    }

    // Look up the keys in the provided keyspace and update
    // the reconstruct state with whatever is found.

}

/// A builder object for constructing a new image layer.
///
/// Usage:
///
/// 1. Create the ImageLayerWriter by calling ImageLayerWriter::new(...)
///
/// 2. Write the contents by calling `put_page_image` for every key-value
///    pair in the key range.
///
/// 3. Call `finish`.
///
struct ImageLayerWriterInner {
    conf: &'static PageServerConf,
    path: Utf8PathBuf,
    timeline_id: TimelineId,
    tenant_shard_id: TenantShardId,
    key_range: Range<Key>,
    lsn: Lsn,

    // Total uncompressed bytes passed into put_image
    uncompressed_bytes: u64,

    // Like `uncompressed_bytes`,
    // but only of images we might consider for compression
    uncompressed_bytes_eligible: u64,

    // Like `uncompressed_bytes`, but only of images
    // where we have chosen their compressed form
    uncompressed_bytes_chosen: u64,

    // Number of keys in the layer.
    num_keys: usize,

    blob_writer: BlobWriter<TempVirtualFile>,
    tree: DiskBtreeBuilder<BlockBuf, KEY_SIZE>,

    #[cfg(feature = "testing")]
    last_written_key: Key,
}

impl ImageLayerWriterInner {
    ///
    /// Start building a new image layer.
    ///
    #[allow(clippy::too_many_arguments)]
    async fn new(
        conf: &'static PageServerConf,
        timeline_id: TimelineId,
        tenant_shard_id: TenantShardId,
        key_range: &Range<Key>,
        lsn: Lsn,
        gate: &utils::sync::gate::Gate,
        cancel: CancellationToken,
        ctx: &RequestContext,
    ) -> anyhow::Result<Self> {
        // Create the file initially with a temporary filename.
        // We'll atomically rename it to the final name when we're done.
        let path = ImageLayer::temp_path_for(
            conf,
            timeline_id,
            tenant_shard_id,
            &ImageLayerName {
                key_range: key_range.clone(),
                lsn,
            },
        );
        trace!("creating image layer {}", path);
        let file = TempVirtualFile::new(
            VirtualFile::open_with_options_v2(
                &path,
                virtual_file::OpenOptions::new()
                    .create_new(true)
                    .write(true),
                ctx,
            )
            .await?,
            gate.enter()?,
        );

        // Start at `PAGE_SZ` to make room for the header block.
        let blob_writer = BlobWriter::new(
            file,
            PAGE_SZ as u64,
            gate,
            cancel,
            ctx,
            info_span!(parent: None, "image_layer_writer_flush_task", tenant_id=%tenant_shard_id.tenant_id, shard_id=%tenant_shard_id.shard_slug(), timeline_id=%timeline_id, path = %path),
        )?;

        // Initialize the b-tree index builder
        let block_buf = BlockBuf::new();
        let tree_builder = DiskBtreeBuilder::new(block_buf);

        let writer = Self {
            conf,
            path,
            timeline_id,
            tenant_shard_id,
            key_range: key_range.clone(),
            lsn,
            tree: tree_builder,
            blob_writer,
            uncompressed_bytes: 0,
            uncompressed_bytes_eligible: 0,
            uncompressed_bytes_chosen: 0,
            num_keys: 0,
            #[cfg(feature = "testing")]
            last_written_key: Key::MIN,
        };

        Ok(writer)
    }

    ///
    /// Write next value to the file.
    ///
    /// The page versions must be appended in blknum order.
    ///
    async fn put_image(
        &mut self,
        key: Key,
        img: Bytes,
        ctx: &RequestContext,
    ) -> Result<(), PutError> {
        if !self.key_range.contains(&key) {
            return Err(PutError::Other(anyhow::anyhow!(
                "key {:?} not in range {:?}",
                key,
                self.key_range
            )));
        }
        let compression = self.conf.image_compression;
        let uncompressed_len = img.len() as u64;
        self.uncompressed_bytes += uncompressed_len;
        self.num_keys += 1;
        let (_img, res) = self
            .blob_writer
            .write_blob_maybe_compressed(img.slice_len(), ctx, compression)
            .await;
        // TODO: re-use the buffer for `img` further upstack
        let (off, compression_info) = res.map_err(PutError::WriteBlob)?;
        if compression_info.compressed_size.is_some() {
            // The image has been considered for compression at least
            self.uncompressed_bytes_eligible += uncompressed_len;
        }
        if compression_info.written_compressed {
            // The image has been compressed
            self.uncompressed_bytes_chosen += uncompressed_len;
        }

        let mut keybuf: [u8; KEY_SIZE] = [0u8; KEY_SIZE];
        key.write_to_byte_slice(&mut keybuf);
        self.tree
            .append(&keybuf, off)
            .map_err(anyhow::Error::new)
            .map_err(PutError::Other)?;

        #[cfg(feature = "testing")]
        {
            self.last_written_key = key;
        }

        Ok(())
    }

    ///
    /// Write the next image to the file, as a raw blob header and data.
    ///
    /// The page versions must be appended in blknum order.
    ///
    async fn put_image_raw(
        &mut self,
        key: Key,
        raw_with_header: Bytes,
        ctx: &RequestContext,
    ) -> anyhow::Result<()> {
        ensure!(self.key_range.contains(&key));

        // NB: we don't update the (un)compressed metrics, since we can't determine them without
        // decompressing the image. This seems okay.
        self.num_keys += 1;

        let (_, res) = self
            .blob_writer
            .write_blob_raw(raw_with_header.slice_len(), ctx)
            .await;
        let offset = res?;

        let mut keybuf: [u8; KEY_SIZE] = [0u8; KEY_SIZE];
        key.write_to_byte_slice(&mut keybuf);
        self.tree.append(&keybuf, offset)?;

        #[cfg(feature = "testing")]
        {
            self.last_written_key = key;
        }

        Ok(())
    }

    ///
    /// Finish writing the image layer.
    ///
    async fn finish(
        self,
        ctx: &RequestContext,
        end_key: Option<Key>,
    ) -> anyhow::Result<(PersistentLayerDesc, Utf8PathBuf)> {
        let index_start_blk = self.blob_writer.size().div_ceil(PAGE_SZ as u64) as u32;

        // Calculate compression ratio
        let compressed_size = self.blob_writer.size() - PAGE_SZ as u64; // Subtract PAGE_SZ for header
        crate::metrics::COMPRESSION_IMAGE_INPUT_BYTES.inc_by(self.uncompressed_bytes);
        crate::metrics::COMPRESSION_IMAGE_INPUT_BYTES_CONSIDERED
            .inc_by(self.uncompressed_bytes_eligible);
        crate::metrics::COMPRESSION_IMAGE_INPUT_BYTES_CHOSEN.inc_by(self.uncompressed_bytes_chosen);

        // NB: filter() may pass through raw pages from a different layer, without looking at
        // whether these are compressed or not. We don't track metrics for these, so avoid
        // increasing `COMPRESSION_IMAGE_OUTPUT_BYTES` in this case too.
        if self.uncompressed_bytes > 0 {
            crate::metrics::COMPRESSION_IMAGE_OUTPUT_BYTES.inc_by(compressed_size);
        };

        let file = self
            .blob_writer
            .shutdown(
                BufferedWriterShutdownMode::ZeroPadToNextMultiple(PAGE_SZ),
                ctx,
            )
            .await?;

        // Write out the index
        let mut offset = index_start_blk as u64 * PAGE_SZ as u64;
        let (index_root_blk, block_buf) = self.tree.finish()?;

        // TODO(yuchen): https://github.com/neondatabase/neon/issues/10092
        // Should we just replace BlockBuf::blocks with one big buffer?
        for buf in block_buf.blocks {
            let (_buf, res) = file.write_all_at(buf.slice_len(), offset, ctx).await;
            res?;
            offset += PAGE_SZ as u64;
        }

        let final_key_range = if let Some(end_key) = end_key {
            self.key_range.start..end_key
        } else {
            self.key_range.clone()
        };

        // Fill in the summary on blk 0
        let summary = Summary {
            magic: IMAGE_FILE_MAGIC,
            format_version: STORAGE_FORMAT_VERSION,
            tenant_id: self.tenant_shard_id.tenant_id,
            timeline_id: self.timeline_id,
            key_range: final_key_range.clone(),
            lsn: self.lsn,
            index_start_blk,
            index_root_blk,
        };

        // Writes summary at the first block (offset 0).
        let buf = summary.ser_into_page()?;
        let (_buf, res) = file.write_all_at(buf.slice_len(), 0, ctx).await;
        res?;

        let metadata = file
            .metadata()
            .await
            .context("get metadata to determine file size")?;

        let desc = PersistentLayerDesc::new_img(
            self.tenant_shard_id,
            self.timeline_id,
            final_key_range,
            self.lsn,
            metadata.len(),
        );

        #[cfg(feature = "testing")]
        if let Some(end_key) = end_key {
            assert!(
                self.last_written_key < end_key,
                "written key violates end_key range"
            );
        }

        // Note: Because we open the file in write-only mode, we cannot
        // reuse the same VirtualFile for reading later. That's why we don't
        // set inner.file here. The first read will have to re-open it.

        // fsync the file
        file.sync_all()
            .await
            .maybe_fatal_err("image_layer sync_all")?;

        trace!("created image layer {}", self.path);

        // The gate guard stored in `destination_file` is dropped. Callers (e.g.. flush loop or compaction)
        // keep the gate open also, so that it's safe for them to rename the file to its final destination.
        file.disarm_into_inner();

        Ok((desc, self.path))
    }
}

/// A builder object for constructing a new image layer.
///
/// Usage:
///
/// 1. Create the ImageLayerWriter by calling ImageLayerWriter::new(...)
///
/// 2. Write the contents by calling `put_page_image` for every key-value
///    pair in the key range.
///
/// 3. Call `finish`.
///
/// # Note
///
/// As described in <https://github.com/neondatabase/neon/issues/2650>, it's
/// possible for the writer to drop before `finish` is actually called. So this
/// could lead to odd temporary files in the directory, exhausting file system.
/// This structure wraps `ImageLayerWriterInner` and also contains `Drop`
/// implementation that cleans up the temporary file in failure. It's not
/// possible to do this directly in `ImageLayerWriterInner` since `finish` moves
/// out some fields, making it impossible to implement `Drop`.
///
#[must_use]
pub struct ImageLayerWriter {
    inner: Option<ImageLayerWriterInner>,
}

impl ImageLayerWriter {
    ///
    /// Start building a new image layer.
    ///
    #[allow(clippy::too_many_arguments)]
    pub async fn new(
        conf: &'static PageServerConf,
        timeline_id: TimelineId,
        tenant_shard_id: TenantShardId,
        key_range: &Range<Key>,
        lsn: Lsn,
        gate: &utils::sync::gate::Gate,
        cancel: CancellationToken,
        ctx: &RequestContext,
    ) -> anyhow::Result<ImageLayerWriter> {
        Ok(Self {
            inner: Some(
                ImageLayerWriterInner::new(
                    conf,
                    timeline_id,
                    tenant_shard_id,
                    key_range,
                    lsn,
                    gate,
                    cancel,
                    ctx,
                )
                .await?,
            ),
        })
    }

    ///
    /// Write next value to the file.
    ///
    /// The page versions must be appended in blknum order.
    ///
    pub async fn put_image(
        &mut self,
        key: Key,
        img: Bytes,
        ctx: &RequestContext,
    ) -> Result<(), PutError> {
        self.inner.as_mut().unwrap().put_image(key, img, ctx).await
    }

    ///
    /// Write the next value to the file, as a raw header and data. This allows passing through a
    /// raw, potentially compressed image from a different layer file without recompressing it.
    ///
    /// The page versions must be appended in blknum order.
    ///
    pub async fn put_image_raw(
        &mut self,
        key: Key,
        raw_with_header: Bytes,
        ctx: &RequestContext,
    ) -> anyhow::Result<()> {
        self.inner
            .as_mut()
            .unwrap()
            .put_image_raw(key, raw_with_header, ctx)
            .await
    }

    ///
    /// Finish writing the image layer.
    ///
    pub(crate) async fn finish(
        mut self,
        ctx: &RequestContext,
    ) -> anyhow::Result<(PersistentLayerDesc, Utf8PathBuf)> {
        self.inner.take().unwrap().finish(ctx, None).await
    }

}

