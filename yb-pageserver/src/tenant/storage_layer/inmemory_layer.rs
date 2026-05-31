//! An in-memory layer stores recently received key-value pairs.
//!
//! In yb-pageserver the legacy ephemeral-file write path is gone:
//! incoming WAL routes straight through PageWriter into the row store,
//! and this module's only remaining purpose is to keep the
//! `InMemoryLayer` type alive so `LayerMap`/`ReadableLayerWeak` still
//! compile. The struct itself is no longer constructed at runtime.
use std::fmt::Write;
use std::ops::Range;
use std::sync::OnceLock;
use std::sync::atomic::AtomicU64;

use anyhow::Result;
use pageserver_api::models::InMemoryLayerInfo;
use utils::id::TimelineId;
use utils::lsn::Lsn;

use crate::assert_u64_eq_usize::u64_to_usize;
use crate::context::RequestContext;
use crate::page_cache;


#[derive(Debug, PartialEq, Eq, Clone, Copy, Hash)]
pub(crate) struct InMemoryLayerFileId(page_cache::FileId);

pub struct InMemoryLayer {
    timeline_id: TimelineId,

    /// This layer contains all the changes from 'start_lsn'. The
    /// start is inclusive.
    start_lsn: Lsn,

    /// Frozen layers have an exclusive end LSN.
    /// Writes are only allowed when this is `None`.
    pub(crate) end_lsn: OnceLock<Lsn>,
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

/// Index-entry layout used by `validate_checkpoint_distance` callers
/// (config validation). The runtime use of this packed encoding is gone
/// in yb-mode, but the static check is still wired up.
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


/// Global cap on how much dirty in-memory data is tolerated before
/// freezing. yb-mode never opens an InMemoryLayer, but
/// `tenant::mgr` still writes the configured cap into this static at
/// startup; keep it as a no-op carrier.
pub(crate) struct GlobalResources {
    pub(crate) max_dirty_bytes: AtomicU64,
}


pub(crate) static GLOBAL_RESOURCES: GlobalResources = GlobalResources {
    max_dirty_bytes: AtomicU64::new(0),
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

