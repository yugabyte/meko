//! An in-memory layer stores recently received key-value pairs.
//!
//! The "in-memory" part of the name is a bit misleading: the actual page versions are
//! held in an ephemeral file, not in memory. The metadata for each page version, i.e.
//! its position in the file, is kept in memory, though.
//!
use std::cmp::Ordering;
use std::collections::{BTreeMap, HashMap};
use std::fmt::Write;
use std::ops::Range;
use std::sync::atomic::{AtomicU64, AtomicUsize, Ordering as AtomicOrdering};
use std::sync::{Arc, OnceLock};
use std::time::Instant;

use anyhow::Result;
use camino::Utf8PathBuf;
use pageserver_api::key::{CompactKey, Key};
use pageserver_api::keyspace::KeySpace;
use pageserver_api::models::InMemoryLayerInfo;
use pageserver_api::shard::TenantShardId;
use tokio::sync::RwLock;
use tokio_util::sync::CancellationToken;
use tracing::*;
use utils::id::TimelineId;
use utils::lsn::Lsn;
use utils::vec_map::VecMap;
use wal_decoder::serialized_batch::{SerializedValueBatch, SerializedValueMeta, ValueMeta};

use super::{DeltaLayerWriter, PersistentLayerDesc, ValuesReconstructState};
use crate::assert_u64_eq_usize::{U64IsUsize, UsizeIsU64, u64_to_usize};
use crate::config::PageServerConf;
use crate::context::{PageContentKind, RequestContext, RequestContextBuilder};
// avoid binding to Write (conflicts with std::io::Write)
// while being able to use std::fmt::Write's methods
use crate::metrics::TIMELINE_EPHEMERAL_BYTES;
use crate::tenant::timeline::GetVectoredError;
use crate::virtual_file::owned_buffers_io::io_buf_ext::IoBufExt;
use crate::{l0_flush, page_cache};


#[derive(Debug, PartialEq, Eq, Clone, Copy, Hash)]
pub(crate) struct InMemoryLayerFileId(page_cache::FileId);

pub struct InMemoryLayer {
    conf: &'static PageServerConf,
    tenant_shard_id: TenantShardId,
    timeline_id: TimelineId,
    file_id: InMemoryLayerFileId,

    /// This layer contains all the changes from 'start_lsn'. The
    /// start is inclusive.
    start_lsn: Lsn,

    /// Frozen layers have an exclusive end LSN.
    /// Writes are only allowed when this is `None`.
    pub(crate) end_lsn: OnceLock<Lsn>,

    /// Used for traversal path. Cached representation of the in-memory layer after frozen.
    frozen_local_path_str: OnceLock<Arc<str>>,

    opened_at: Instant,

    /// All versions of all pages in the layer are kept here. Indexed
    /// by block number and LSN. The [`IndexEntry`] is an offset into the
    /// ephemeral file where the page version is stored.
    ///
    /// We use a separate lock for the index to reduce the critical section
    /// during which reads cannot be planned.
    ///
    /// Note that the file backing [`InMemoryLayer::file`] is append-only,
    /// so it is not necessary to hold a lock on the index while reading or writing from the file.
    /// In particular:
    /// 1. It is safe to read and release [`InMemoryLayer::index`] before reading from [`InMemoryLayer::file`].
    /// 2. It is safe to write to [`InMemoryLayer::file`] before locking and updating [`InMemoryLayer::index`].
    index: RwLock<BTreeMap<CompactKey, VecMap<Lsn, IndexEntry>>>,

    estimated_in_mem_size: AtomicU64,
}

impl std::fmt::Debug for InMemoryLayer {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("InMemoryLayer")
            .field("start_lsn", &self.start_lsn)
            .field("end_lsn", &self.end_lsn)
            .finish()
    }
}

/// Support the same max blob length as blob_io, because ultimately
/// all the InMemoryLayer contents end up being written into a delta layer,
/// using the [`crate::tenant::blob_io`].
const MAX_SUPPORTED_BLOB_LEN: usize = crate::tenant::blob_io::MAX_SUPPORTED_BLOB_LEN;
const MAX_SUPPORTED_BLOB_LEN_BITS: usize = {
    let trailing_ones = MAX_SUPPORTED_BLOB_LEN.trailing_ones() as usize;
    let leading_zeroes = MAX_SUPPORTED_BLOB_LEN.leading_zeros() as usize;
    assert!(trailing_ones + leading_zeroes == std::mem::size_of::<usize>() * 8);
    trailing_ones
};

/// See [`InMemoryLayer::index`].
///
/// For memory efficiency, the data is packed into a u64.
///
/// Layout:
/// - 1 bit: `will_init`
/// - [`MAX_SUPPORTED_BLOB_LEN_BITS`][]: `len`
/// - [`MAX_SUPPORTED_POS_BITS`](IndexEntry::MAX_SUPPORTED_POS_BITS): `pos`
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct IndexEntry(u64);

impl IndexEntry {
    /// See [`Self::MAX_SUPPORTED_POS`].
    const MAX_SUPPORTED_POS_BITS: usize = {
        let remainder = 64 - 1 - MAX_SUPPORTED_BLOB_LEN_BITS;
        if remainder < 32 {
            panic!("pos can be u32 as per type system, support that");
        }
        remainder
    };
    /// The maximum supported blob offset that can be represented by [`Self`].
    /// See also [`Self::validate_checkpoint_distance`].
    const MAX_SUPPORTED_POS: usize = (1 << Self::MAX_SUPPORTED_POS_BITS) - 1;



    /// See [`Self::new`].
    pub(crate) const fn validate_checkpoint_distance(
        checkpoint_distance: u64,
    ) -> Result<(), &'static str> {
        if checkpoint_distance > Self::MAX_SUPPORTED_POS as u64 {
            return Err("exceeds the maximum supported value");
        }
        let res = u64_to_usize(checkpoint_distance).checked_add(MAX_SUPPORTED_BLOB_LEN);
        if res.is_none() {
            return Err(
                "checkpoint distance + max supported blob len overflows in-memory addition",
            );
        }

        // NB: it is ok for the result of the addition to be larger than MAX_SUPPORTED_POS

        Ok(())
    }

    const _ASSERT_DEFAULT_CHECKPOINT_DISTANCE_IS_VALID: () = {
        let res = Self::validate_checkpoint_distance(
            pageserver_api::config::tenant_conf_defaults::DEFAULT_CHECKPOINT_DISTANCE,
        );
        if res.is_err() {
            panic!("default checkpoint distance is valid")
        }
    };
}


/// State shared by all in-memory (ephemeral) layers.  Updated infrequently during background ticks in Timeline,
/// to minimize contention.
///
/// This global state is used to implement behaviors that require a global view of the system, e.g.
/// rolling layers proactively to limit the total amount of dirty data.
pub(crate) struct GlobalResources {
    // Limit on how high dirty_bytes may grow before we start freezing layers to reduce it.
    // Zero means unlimited.
    pub(crate) max_dirty_bytes: AtomicU64,
    // How many bytes are in all EphemeralFile objects
    dirty_bytes: AtomicU64,
    // How many layers are contributing to dirty_bytes
    dirty_layers: AtomicUsize,
}


pub(crate) static GLOBAL_RESOURCES: GlobalResources = GlobalResources {
    max_dirty_bytes: AtomicU64::new(0),
    dirty_bytes: AtomicU64::new(0),
    dirty_layers: AtomicUsize::new(0),
};

impl InMemoryLayer {


    pub(crate) fn info(&self) -> InMemoryLayerInfo {
        let lsn_start = self.start_lsn;

        if let Some(&lsn_end) = self.end_lsn.get() {
            InMemoryLayerInfo::Frozen { lsn_start, lsn_end }
        } else {
            InMemoryLayerInfo::Open { lsn_start }
        }
    }



    pub(crate) fn end_lsn_or_max(&self) -> Lsn {
        self.end_lsn.get().copied().unwrap_or(Lsn::MAX)
    }

    pub(crate) fn get_lsn_range(&self) -> Range<Lsn> {
        self.start_lsn..self.end_lsn_or_max()
    }

    /// debugging function to print out the contents of the layer
    ///
    /// this is likely completly unused
    pub async fn dump(&self, _verbose: bool, _ctx: &RequestContext) -> Result<()> {
        let end_str = self.end_lsn_or_max();

        println!(
            "----- in-memory layer for tli {} LSNs {}-{} ----",
            self.timeline_id, self.start_lsn, end_str,
        );

        Ok(())
    }

}

fn inmem_layer_display(mut f: impl Write, start_lsn: Lsn, end_lsn: Lsn) -> std::fmt::Result {
    write!(f, "inmem-{:016X}-{:016X}", start_lsn.0, end_lsn.0)
}

impl std::fmt::Display for InMemoryLayer {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        let end_lsn = self.end_lsn_or_max();
        inmem_layer_display(f, self.start_lsn, end_lsn)
    }
}

