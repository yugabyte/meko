//! Common types referenced by yb-pageserver's surviving signatures.
//!
//! Most of the layer-file machinery is gone in yb-mode. What remains
//! is the `LayerName` parsing surface used by `index_part.json`,
//! plus the walredo `ValueReconstructState` accumulator the read path
//! still feeds into walredo.

mod layer_name;

use std::ops::Range;

use bytes::Bytes;
pub use layer_name::{DeltaLayerName, ImageLayerName, LayerName};
use utils::lsn::Lsn;
use wal_decoder::models::record::NeonWalRecord;

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

/// Range wrapping newtype, which uses display to render Debug.
pub(super) struct RangeDisplayDebug<'a, T: std::fmt::Display>(pub(super) &'a Range<T>);

impl<T: std::fmt::Display> std::fmt::Debug for RangeDisplayDebug<'_, T> {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "{}..{}", self.0.start, self.0.end)
    }
}
