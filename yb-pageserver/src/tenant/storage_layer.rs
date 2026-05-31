//! Common traits and structs for layers.
//!
//! In yb-pageserver mode this module is mostly vestigial — there are
//! no on-disk layer files and no LayerMap to populate. We keep just
//! enough scaffolding for the surviving signatures (basebackup,
//! pgdatadir_mapping, walredo, layer_map) to compile.

pub mod inmemory_layer;
pub(crate) mod layer;
mod layer_desc;
mod layer_name;

use std::ops::Range;
use std::sync::Arc;

use bytes::Bytes;
pub use inmemory_layer::InMemoryLayer;
pub(crate) use layer::Layer;
pub use layer_desc::{PersistentLayerDesc, PersistentLayerKey};
pub use layer_name::{DeltaLayerName, ImageLayerName, LayerName};
use pageserver_api::config::GetVectoredConcurrentIo;
use utils::lsn::Lsn;
use utils::sync::gate::GateGuard;
use wal_decoder::models::record::NeonWalRecord;

use super::layer_map::InMemoryLayerDesc;

pub fn range_overlaps<T>(a: &Range<T>, b: &Range<T>) -> bool
where
    T: PartialOrd<T>,
{
    if a.start < b.start {
        a.end > b.start
    } else {
        b.end > a.start
    }
}

/// Walredo reconstruction state — the accumulator for image + WAL
/// records on the read path. Still used by `Timeline::reconstruct_value`.
#[derive(Debug, Default, Clone)]
pub(crate) struct ValueReconstructState {
    pub(crate) records: Vec<(Lsn, NeonWalRecord)>,
    pub(crate) img: Option<(Lsn, Bytes)>,
}

impl ValueReconstructState {
    pub fn num_deltas(&self) -> usize {
        match self.img {
            Some(_) => self.records.len(),
            None => self.records.len() - 1,
        }
    }
}

/// yb-mode: vestige. The legacy `get_vectored` traversal used this to
/// accumulate per-key reconstruct buffers. `Timeline::get_vectored_impl`
/// ignores it entirely — the row-store read path runs through
/// `PageWriter::scan_range`. Kept as a unit type so external callers
/// (basebackup, pgdatadir_mapping) can keep their existing signatures.
pub struct ValuesReconstructState;

impl ValuesReconstructState {
    pub fn new(_io_concurrency: IoConcurrency) -> Self {
        Self
    }
}

/// yb-mode: vestige. The legacy parallel-IO sidecar task is gone —
/// layer-file reads are gone. `IoConcurrency` is now a no-op marker
/// passed through `get_vectored`-style entry points.
#[derive(Debug, Clone)]
pub enum IoConcurrency {
    Sequential,
}

pub(crate) enum SelectedIoConcurrency {
    Sequential,
}

impl IoConcurrency {
    pub(crate) fn sequential() -> Self {
        Self::Sequential
    }

    pub fn spawn_from_conf(
        _conf: GetVectoredConcurrentIo,
        _gate_guard: GateGuard,
    ) -> IoConcurrency {
        IoConcurrency::Sequential
    }

    pub(crate) fn spawn(_io_concurrency: SelectedIoConcurrency) -> Self {
        IoConcurrency::Sequential
    }

    #[cfg(test)]
    pub(crate) fn spawn_for_test() -> impl std::ops::DerefMut<Target = Self> {
        struct Wrapper(IoConcurrency);
        impl std::ops::Deref for Wrapper {
            type Target = IoConcurrency;
            fn deref(&self) -> &Self::Target {
                &self.0
            }
        }
        impl std::ops::DerefMut for Wrapper {
            fn deref_mut(&mut self) -> &mut Self::Target {
                &mut self.0
            }
        }
        Wrapper(IoConcurrency::Sequential)
    }
}

#[derive(Debug, PartialEq, Eq, Hash)]
pub enum ReadableLayerWeak {
    PersistentLayer(Arc<PersistentLayerDesc>),
    InMemoryLayer(InMemoryLayerDesc),
}

/// Visibility hint for layers. In yb-mode there are no layers so the
/// hint is effectively dead, but it still appears in `LayerMap`
/// signatures.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum LayerVisibilityHint {
    Visible,
    Covered,
}

#[derive(Clone, Copy, strum_macros::EnumString)]
pub(crate) enum LayerAccessStatsReset {
    NoReset,
    AllStats,
}

/// Get a layer descriptor from a layer.
pub(crate) trait AsLayerDesc {
    fn layer_desc(&self) -> &PersistentLayerDesc;
}

/// Range wrapping newtype, which uses display to render Debug.
pub(crate) struct RangeDisplayDebug<'a, T: std::fmt::Display>(pub(crate) &'a Range<T>);

impl<T: std::fmt::Display> std::fmt::Debug for RangeDisplayDebug<'_, T> {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "{}..{}", self.0.start, self.0.end)
    }
}
