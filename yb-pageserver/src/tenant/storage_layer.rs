//! Common types referenced by yb-pageserver's surviving signatures.
//!
//! Most of the layer-file machinery is gone in yb-mode. What remains
//! is the `LayerName` parsing surface used by `index_part.json` and
//! the HTTP layer-admin endpoints, plus a few vestigial markers
//! (`IoConcurrency`, `ValuesReconstructState`) kept so external
//! call-sites don't have to churn.

mod layer_name;

use std::ops::Range;

use bytes::Bytes;
pub use layer_name::{DeltaLayerName, ImageLayerName, LayerName};
use pageserver_api::config::GetVectoredConcurrentIo;
use utils::lsn::Lsn;
use utils::sync::gate::GateGuard;
use wal_decoder::models::record::NeonWalRecord;

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

#[derive(Clone, Copy, strum_macros::EnumString)]
pub(crate) enum LayerAccessStatsReset {
    NoReset,
    AllStats,
}

/// Range wrapping newtype, which uses display to render Debug.
pub(super) struct RangeDisplayDebug<'a, T: std::fmt::Display>(pub(super) &'a Range<T>);

impl<T: std::fmt::Display> std::fmt::Debug for RangeDisplayDebug<'_, T> {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "{}..{}", self.0.start, self.0.end)
    }
}
