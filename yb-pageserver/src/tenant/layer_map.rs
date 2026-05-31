//! yb-pageserver: vestigial layer-map.
//!
//! The legacy LayerMap was an R-tree-indexed persistent BST for fast
//! `(key, LSN)` → layer lookups across image / delta layer files. In
//! yb-mode there are no layer files: reads route through PageWriter
//! into the row store. We keep the type shapes alive for the few
//! call-sites that still iterate over the (always empty) map.

use std::collections::VecDeque;
use std::ops::Range;
use std::sync::Arc;

use pageserver_api::key::Key;
use utils::lsn::Lsn;

use super::storage_layer::{InMemoryLayer, PersistentLayerDesc};

/// LayerMap tracks what layers exist on a timeline. In yb-mode the
/// map is always empty.
pub struct LayerMap {
    /// `open_layer` holds the current `InMemoryLayer` accepting new
    /// records. Always `None` in yb-mode.
    pub open_layer: Option<Arc<InMemoryLayer>>,
    /// Start LSN of the next `InMemoryLayer` to be created. Set by
    /// `init_empty_layer_map`.
    pub next_open_layer_at: Option<Lsn>,
    /// Frozen in-memory layers waiting to be flushed. Always empty in
    /// yb-mode.
    pub frozen_layers: VecDeque<Arc<InMemoryLayer>>,
}

impl Default for LayerMap {
    fn default() -> Self {
        Self {
            open_layer: None,
            next_open_layer_at: None,
            frozen_layers: VecDeque::new(),
        }
    }
}

impl LayerMap {
    /// Iterate over the historic (on-disk) layers covered by this
    /// map. In yb-mode this is always an empty iterator.
    pub fn iter_historic_layers(&self) -> std::iter::Empty<Arc<PersistentLayerDesc>> {
        std::iter::empty()
    }

    /// True when the layer's key range spans the entire keyspace and
    /// it's a delta — i.e. it would have been an L0 layer in the
    /// legacy stack. Kept as a static helper for metrics labelling.
    pub fn is_l0(_key_range: &Range<Key>, _is_delta: bool) -> bool {
        false
    }
}

/// Descriptor for an in-memory layer entry inside a `ReadableLayerWeak`.
/// Never constructed in yb-mode.
#[derive(Debug, PartialEq, Eq, Clone, Hash)]
pub struct InMemoryLayerDesc {
    lsn_range: Range<Lsn>,
}

impl InMemoryLayerDesc {
    pub(crate) fn get_lsn_range(&self) -> Range<Lsn> {
        self.lsn_range.clone()
    }
}
