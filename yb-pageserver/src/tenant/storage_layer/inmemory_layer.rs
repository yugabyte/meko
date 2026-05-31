//! yb-pageserver: vestigial `InMemoryLayer` type.
//!
//! The runtime ephemeral-file write path is gone. In yb-mode incoming
//! WAL routes straight through `PageWriter` into the row store and an
//! `InMemoryLayer` is never constructed. The type itself still appears
//! in `LayerMap` / `ReadableLayerWeak` signatures, so we keep a tiny
//! compile-only shell.

use std::fmt::Write;
use std::ops::Range;
use std::sync::OnceLock;

use anyhow::Result;
use pageserver_api::models::InMemoryLayerInfo;
use utils::id::TimelineId;
use utils::lsn::Lsn;

use crate::context::RequestContext;

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

/// yb-mode: GLOBAL_RESOURCES used to track ephemeral-file dirty bytes;
/// in yb-mode there are no ephemeral files. The static is still set
/// from `tenant::mgr` at startup, so we keep a no-op carrier.
pub(crate) struct GlobalResources {
    pub(crate) max_dirty_bytes: std::sync::atomic::AtomicU64,
}

pub(crate) static GLOBAL_RESOURCES: GlobalResources = GlobalResources {
    max_dirty_bytes: std::sync::atomic::AtomicU64::new(0),
};
