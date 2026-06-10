//! Walredo read-path types. The original `storage_layer` module
//! housed the entire layer-file infrastructure (Layer, LayerMap,
//! ImageLayer, DeltaLayer, LayerName parser, etc.); after the
//! layer-stack removal only the `ValueReconstructState` accumulator
//! that walredo's `reconstruct_value` consumes remains here. The
//! `LayerName` parser went away with `index_part.json`'s
//! `layer_metadata` field (see commits in phase 4d).

use bytes::Bytes;
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
