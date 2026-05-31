//!
//! The layer map tracks what layers exist in a timeline.
//!
//! When the timeline is first accessed, the server lists of all layer files
//! in the timelines/<timeline_id> directory, and populates this map with
//! ImageLayer and DeltaLayer structs corresponding to each file. When the first
//! new WAL record is received, we create an InMemoryLayer to hold the incoming
//! records. Now and then, in the checkpoint() function, the in-memory layer is
//! are frozen, and it is split up into new image and delta layers and the
//! corresponding files are written to disk.
//!
//! Design overview:
//!
//! The `search` method of the layer map is on the read critical path, so we've
//! built an efficient data structure for fast reads, stored in `LayerMap::historic`.
//! Other read methods are less critical but still impact performance of background tasks.
//!
//! This data structure relies on a persistent/immutable binary search tree. See the
//! following lecture for an introduction <https://www.youtube.com/watch?v=WqCWghETNDc&t=581s>
//! Summary: A persistent/immutable BST (and persistent data structures in general) allows
//! you to modify the tree in such a way that each modification creates a new "version"
//! of the tree. When you modify it, you get a new version, but all previous versions are
//! still accessible too. So if someone is still holding a reference to an older version,
//! they continue to see the tree as it was then. The persistent BST stores all the
//! different versions in an efficient way.
//!
//! Our persistent BST maintains a map of which layer file "covers" each key. It has only
//! one dimension, the key. See `layer_coverage.rs`. We use the persistent/immutable property
//! to handle the LSN dimension.
//!
//! To build the layer map, we insert each layer to the persistent BST in LSN.start order,
//! starting from the oldest one. After each insertion, we grab a reference to that "version"
//! of the tree, and store it in another tree, a BtreeMap keyed by the LSN. See
//! `historic_layer_coverage.rs`.
//!
//! To search for a particular key-LSN pair, you first look up the right "version" in the
//! BTreeMap. Then you search that version of the BST with the key.
//!
//! The persistent BST keeps all the versions, but there is no way to change the old versions
//! afterwards. We can add layers as long as they have larger LSNs than any previous layer in
//! the map, but if we need to remove a layer, or insert anything with an older LSN, we need
//! to throw away most of the persistent BST and build a new one, starting from the oldest
//! LSN. See [`LayerMap::flush_updates()`].
//!

mod historic_layer_coverage;
mod layer_coverage;

use std::collections::{BTreeMap, HashMap, VecDeque};
use std::iter::Peekable;
use std::ops::Range;
use std::sync::Arc;
use std::time::Instant;

use anyhow::Result;
use historic_layer_coverage::BufferedHistoricLayerCoverage;
pub use historic_layer_coverage::LayerKey;
use pageserver_api::key::Key;
use pageserver_api::keyspace::{KeySpace, KeySpaceAccum};
use range_set_blaze::{CheckSortedDisjoint, RangeSetBlaze};
use tokio::sync::watch;
use utils::lsn::Lsn;

use super::storage_layer::{LayerVisibilityHint, PersistentLayerDesc};
use crate::context::RequestContext;
use crate::tenant::storage_layer::{InMemoryLayer, ReadableLayerWeak};

///
/// LayerMap tracks what layers exist on a timeline.
///
pub struct LayerMap {
    //
    // 'open_layer' holds the current InMemoryLayer that is accepting new
    // records. If it is None, 'next_open_layer_at' will be set instead, indicating
    // where the start LSN of the next InMemoryLayer that is to be created.
    //
    pub open_layer: Option<Arc<InMemoryLayer>>,
    pub next_open_layer_at: Option<Lsn>,

    ///
    /// Frozen layers, if any. Frozen layers are in-memory layers that
    /// are no longer added to, but haven't been written out to disk
    /// yet. They contain WAL older than the current 'open_layer' or
    /// 'next_open_layer_at', but newer than any historic layer.
    /// The frozen layers are in order from oldest to newest, so that
    /// the newest one is in the 'back' of the VecDeque, and the oldest
    /// in the 'front'.
    ///
    pub frozen_layers: VecDeque<Arc<InMemoryLayer>>,

    /// Index of the historic layers optimized for search
    historic: BufferedHistoricLayerCoverage<Arc<PersistentLayerDesc>>,

    /// L0 layers have key range Key::MIN..Key::MAX, and locating them using R-Tree search is very inefficient.
    /// So L0 layers are held in l0_delta_layers vector, in addition to the R-tree.
    ///
    /// NB: make sure to notify `watch_l0_deltas` on changes.
    l0_delta_layers: Vec<Arc<PersistentLayerDesc>>,

    /// Notifies about L0 delta layer changes, sending the current number of L0 layers.
    watch_l0_deltas: watch::Sender<usize>,
}

impl Default for LayerMap {
    fn default() -> Self {
        Self {
            open_layer: Default::default(),
            next_open_layer_at: Default::default(),
            frozen_layers: Default::default(),
            historic: Default::default(),
            l0_delta_layers: Default::default(),
            watch_l0_deltas: watch::channel(0).0,
        }
    }
}

/// The primary update API for the layer map.
///
/// Batching historic layer insertions and removals is good for
/// performance and this struct helps us do that correctly.
#[must_use]
pub struct BatchedUpdates<'a> {
    // While we hold this exclusive reference to the layer map the type checker
    // will prevent us from accidentally reading any unflushed updates.
    layer_map: &'a mut LayerMap,
}

/// Provide ability to batch more updates while hiding the read
/// API so we don't accidentally read without flushing.
impl BatchedUpdates<'_> {
    ///
    /// Insert an on-disk layer.
    ///
    // TODO remove the `layer` argument when `mapping` is refactored out of `LayerMap`
    pub fn insert_historic(&mut self, layer_desc: PersistentLayerDesc) {
        self.layer_map.insert_historic_noflush(layer_desc)
    }

    ///
    /// Remove an on-disk layer from the map.
    ///
    /// This should be called when the corresponding file on disk has been deleted.
    ///
    pub fn remove_historic(&mut self, layer_desc: &PersistentLayerDesc) {
        self.layer_map.remove_historic_noflush(layer_desc)
    }

    // We will flush on drop anyway, but this method makes it
    // more explicit that there is some work being done.
    /// Apply all updates
    pub fn flush(self) {
        // Flush happens on drop
    }
}

// Ideally the flush() method should be called explicitly for more
// controlled execution. But if we forget we'd rather flush on drop
// than panic later or read without flushing.
//
// TODO maybe warn if flush hasn't explicitly been called
impl Drop for BatchedUpdates<'_> {
    fn drop(&mut self) {
        self.layer_map.flush_updates();
    }
}

/// Return value of LayerMap::search
#[derive(Eq, PartialEq, Debug, Hash)]
pub struct SearchResult {
    pub layer: ReadableLayerWeak,
    pub lsn_floor: Lsn,
}

/// Return value of [`LayerMap::range_search`]
///
/// Contains a mapping from a layer description to a keyspace
/// accumulator that contains all the keys which intersect the layer
/// from the original search space.
#[derive(Debug)]
pub struct RangeSearchResult {
    pub found: HashMap<SearchResult, KeySpaceAccum>,
}

impl RangeSearchResult {
    fn new() -> Self {
        Self {
            found: HashMap::new(),
        }
    }

    fn map_to_in_memory_layer(
        in_memory_layer: Option<InMemoryLayerDesc>,
        range: Range<Key>,
    ) -> RangeSearchResult {
        match in_memory_layer {
            Some(inmem) => {
                let search_result = SearchResult {
                    lsn_floor: inmem.get_lsn_range().start,
                    layer: ReadableLayerWeak::InMemoryLayer(inmem),
                };

                let mut accum = KeySpaceAccum::new();
                accum.add_range(range);
                RangeSearchResult {
                    found: HashMap::from([(search_result, accum)]),
                }
            }
            None => RangeSearchResult::new(),
        }
    }
}

/// Collector for results of range search queries on the LayerMap.
/// It should be provided with two iterators for the delta and image coverage
/// that contain all the changes for layers which intersect the range.
struct RangeSearchCollector<Iter>
where
    Iter: Iterator<Item = (i128, Option<Arc<PersistentLayerDesc>>)>,
{
    in_memory_layer: Option<InMemoryLayerDesc>,
    delta_coverage: Peekable<Iter>,
    image_coverage: Peekable<Iter>,
    key_range: Range<Key>,
    end_lsn: Lsn,

    current_delta: Option<Arc<PersistentLayerDesc>>,
    current_image: Option<Arc<PersistentLayerDesc>>,

    result: RangeSearchResult,
}

#[derive(Debug)]
enum NextLayerType {
    Delta(i128),
    Image(i128),
    Both(i128),
}

impl NextLayerType {
    fn next_change_at_key(&self) -> Key {
        match self {
            NextLayerType::Delta(at) => Key::from_i128(*at),
            NextLayerType::Image(at) => Key::from_i128(*at),
            NextLayerType::Both(at) => Key::from_i128(*at),
        }
    }
}

impl<Iter> RangeSearchCollector<Iter>
where
    Iter: Iterator<Item = (i128, Option<Arc<PersistentLayerDesc>>)>,
{
    fn new(
        key_range: Range<Key>,
        end_lsn: Lsn,
        in_memory_layer: Option<InMemoryLayerDesc>,
        delta_coverage: Iter,
        image_coverage: Iter,
    ) -> Self {
        Self {
            in_memory_layer,
            delta_coverage: delta_coverage.peekable(),
            image_coverage: image_coverage.peekable(),
            key_range,
            end_lsn,
            current_delta: None,
            current_image: None,
            result: RangeSearchResult::new(),
        }
    }

    /// Run the collector. Collection is implemented via a two pointer algorithm.
    /// One pointer tracks the start of the current range and the other tracks
    /// the beginning of the next range which will overlap with the next change
    /// in coverage across both image and delta.
    fn collect(mut self) -> RangeSearchResult {
        let next_layer_type = self.choose_next_layer_type();
        let mut current_range_start = match next_layer_type {
            None => {
                // No changes for the range
                self.pad_range(self.key_range.clone());
                return self.result;
            }
            Some(layer_type) if self.key_range.end <= layer_type.next_change_at_key() => {
                // Changes only after the end of the range
                self.pad_range(self.key_range.clone());
                return self.result;
            }
            Some(layer_type) => {
                // Changes for the range exist.
                let coverage_start = layer_type.next_change_at_key();
                let range_before = self.key_range.start..coverage_start;
                self.pad_range(range_before);

                self.advance(&layer_type);
                coverage_start
            }
        };

        while current_range_start < self.key_range.end {
            let next_layer_type = self.choose_next_layer_type();
            match next_layer_type {
                Some(t) => {
                    let current_range_end = t.next_change_at_key();
                    self.add_range(current_range_start..current_range_end);
                    current_range_start = current_range_end;

                    self.advance(&t);
                }
                None => {
                    self.add_range(current_range_start..self.key_range.end);
                    current_range_start = self.key_range.end;
                }
            }
        }

        self.result
    }

    /// Map a range which does not intersect any persistent layers to
    /// the in-memory layer candidate.
    fn pad_range(&mut self, key_range: Range<Key>) {
        if !key_range.is_empty() {
            if let Some(ref inmem) = self.in_memory_layer {
                let search_result = SearchResult {
                    layer: ReadableLayerWeak::InMemoryLayer(inmem.clone()),
                    lsn_floor: inmem.get_lsn_range().start,
                };

                self.result
                    .found
                    .entry(search_result)
                    .or_default()
                    .add_range(key_range);
            }
        }
    }

    /// Select the appropiate layer for the given range and update
    /// the collector.
    fn add_range(&mut self, covered_range: Range<Key>) {
        let selected = LayerMap::select_layer(
            self.current_delta.clone(),
            self.current_image.clone(),
            self.in_memory_layer.clone(),
            self.end_lsn,
        );

        match selected {
            Some(search_result) => self
                .result
                .found
                .entry(search_result)
                .or_default()
                .add_range(covered_range),
            None => self.pad_range(covered_range),
        }
    }

    /// Move to the next coverage change.
    fn advance(&mut self, layer_type: &NextLayerType) {
        match layer_type {
            NextLayerType::Delta(_) => {
                let (_, layer) = self.delta_coverage.next().unwrap();
                self.current_delta = layer;
            }
            NextLayerType::Image(_) => {
                let (_, layer) = self.image_coverage.next().unwrap();
                self.current_image = layer;
            }
            NextLayerType::Both(_) => {
                let (_, image_layer) = self.image_coverage.next().unwrap();
                let (_, delta_layer) = self.delta_coverage.next().unwrap();

                self.current_image = image_layer;
                self.current_delta = delta_layer;
            }
        }
    }

    /// Pick the next coverage change: the one at the lesser key or both if they're alligned.
    fn choose_next_layer_type(&mut self) -> Option<NextLayerType> {
        let next_delta_at = self.delta_coverage.peek().map(|(key, _)| key);
        let next_image_at = self.image_coverage.peek().map(|(key, _)| key);

        match (next_delta_at, next_image_at) {
            (None, None) => None,
            (Some(next_delta_at), None) => Some(NextLayerType::Delta(*next_delta_at)),
            (None, Some(next_image_at)) => Some(NextLayerType::Image(*next_image_at)),
            (Some(next_delta_at), Some(next_image_at)) if next_image_at < next_delta_at => {
                Some(NextLayerType::Image(*next_image_at))
            }
            (Some(next_delta_at), Some(next_image_at)) if next_delta_at < next_image_at => {
                Some(NextLayerType::Delta(*next_delta_at))
            }
            (Some(next_delta_at), Some(_)) => Some(NextLayerType::Both(*next_delta_at)),
        }
    }
}

#[derive(Debug, PartialEq, Eq, Clone, Hash)]
pub struct InMemoryLayerDesc {
    handle: InMemoryLayerHandle,
    lsn_range: Range<Lsn>,
}

impl InMemoryLayerDesc {
    pub(crate) fn get_lsn_range(&self) -> Range<Lsn> {
        self.lsn_range.clone()
    }
}

#[derive(Debug, PartialEq, Eq, Clone, Hash)]
enum InMemoryLayerHandle {
    Open,
    Frozen(usize),
}

impl LayerMap {
    ///
    /// Find the latest layer (by lsn.end) that covers the given
    /// 'key', with lsn.start < 'end_lsn'.
    ///
    /// The caller of this function is the page reconstruction
    /// algorithm looking for the next relevant delta layer, or
    /// the terminal image layer. The caller will pass the lsn_floor
    /// value as end_lsn in the next call to search.
    ///
    /// If there's an image layer exactly below the given end_lsn,
    /// search should return that layer regardless if there are
    /// overlapping deltas.
    ///
    /// If the latest layer is a delta and there is an overlapping
    /// image with it below, the lsn_floor returned should be right
    /// above that image so we don't skip it in the search. Otherwise
    /// the lsn_floor returned should be the bottom of the delta layer
    /// because we should make as much progress down the lsn axis
    /// as possible. It's fine if this way we skip some overlapping
    /// deltas, because the delta we returned would contain the same
    /// wal content.
    ///
    /// TODO: This API is convoluted and inefficient. If the caller
    /// makes N search calls, we'll end up finding the same latest
    /// image layer N times. We should either cache the latest image
    /// layer result, or simplify the api to `get_latest_image` and
    /// `get_latest_delta`, and only call `get_latest_image` once.
    ///
    pub fn search(&self, key: Key, end_lsn: Lsn) -> Option<SearchResult> {
        let in_memory_layer = self.search_in_memory_layer(end_lsn);

        let version = match self.historic.get().unwrap().get_version(end_lsn.0 - 1) {
            Some(version) => version,
            None => {
                return in_memory_layer.map(|desc| SearchResult {
                    lsn_floor: desc.get_lsn_range().start,
                    layer: ReadableLayerWeak::InMemoryLayer(desc),
                });
            }
        };

        let latest_delta = version.delta_coverage.query(key.to_i128());
        let latest_image = version.image_coverage.query(key.to_i128());

        Self::select_layer(latest_delta, latest_image, in_memory_layer, end_lsn)
    }

    /// Select a layer from three potential candidates (in-memory, delta and image layer).
    /// The candidates represent the first layer of each type which intersect a key range.
    ///
    /// Layer types have an in implicit priority (image > delta > in-memory). For instance,
    /// if we have the option of reading an LSN range from both an image and a delta, we
    /// should read from the image.
    fn select_layer(
        delta_layer: Option<Arc<PersistentLayerDesc>>,
        image_layer: Option<Arc<PersistentLayerDesc>>,
        in_memory_layer: Option<InMemoryLayerDesc>,
        end_lsn: Lsn,
    ) -> Option<SearchResult> {
        assert!(delta_layer.as_ref().is_none_or(|l| l.is_delta()));
        assert!(image_layer.as_ref().is_none_or(|l| !l.is_delta()));

        match (delta_layer, image_layer, in_memory_layer) {
            (None, None, None) => None,
            (None, Some(image), None) => {
                let lsn_floor = image.get_lsn_range().start;
                Some(SearchResult {
                    layer: ReadableLayerWeak::PersistentLayer(image),
                    lsn_floor,
                })
            }
            (Some(delta), None, None) => {
                let lsn_floor = delta.get_lsn_range().start;
                Some(SearchResult {
                    layer: ReadableLayerWeak::PersistentLayer(delta),
                    lsn_floor,
                })
            }
            (Some(delta), Some(image), None) => {
                let img_lsn = image.get_lsn_range().start;
                let image_is_newer = image.get_lsn_range().end >= delta.get_lsn_range().end;
                let image_exact_match = img_lsn + 1 == end_lsn;
                if image_is_newer || image_exact_match {
                    Some(SearchResult {
                        layer: ReadableLayerWeak::PersistentLayer(image),
                        lsn_floor: img_lsn,
                    })
                } else {
                    // If the delta overlaps with the image in the LSN dimension, do a partial
                    // up to the image layer.
                    let lsn_floor =
                        std::cmp::max(delta.get_lsn_range().start, image.get_lsn_range().start + 1);
                    Some(SearchResult {
                        layer: ReadableLayerWeak::PersistentLayer(delta),
                        lsn_floor,
                    })
                }
            }
            (None, None, Some(inmem)) => {
                let lsn_floor = inmem.get_lsn_range().start;
                Some(SearchResult {
                    layer: ReadableLayerWeak::InMemoryLayer(inmem),
                    lsn_floor,
                })
            }
            (None, Some(image), Some(inmem)) => {
                // If the in-memory layer overlaps with the image in the LSN dimension, do a partial
                // up to the image layer.
                let img_lsn = image.get_lsn_range().start;
                let image_is_newer = image.get_lsn_range().end >= inmem.get_lsn_range().end;
                let image_exact_match = img_lsn + 1 == end_lsn;
                if image_is_newer || image_exact_match {
                    Some(SearchResult {
                        layer: ReadableLayerWeak::PersistentLayer(image),
                        lsn_floor: img_lsn,
                    })
                } else {
                    let lsn_floor =
                        std::cmp::max(inmem.get_lsn_range().start, image.get_lsn_range().start + 1);
                    Some(SearchResult {
                        layer: ReadableLayerWeak::InMemoryLayer(inmem),
                        lsn_floor,
                    })
                }
            }
            (Some(delta), None, Some(inmem)) => {
                // Overlaps between delta and in-memory layers are not a valid
                // state, but we handle them here for completeness.
                let delta_end = delta.get_lsn_range().end;
                let delta_is_newer = delta_end >= inmem.get_lsn_range().end;
                let delta_exact_match = delta_end == end_lsn;
                if delta_is_newer || delta_exact_match {
                    Some(SearchResult {
                        lsn_floor: delta.get_lsn_range().start,
                        layer: ReadableLayerWeak::PersistentLayer(delta),
                    })
                } else {
                    // If the in-memory layer overlaps with the delta in the LSN dimension, do a partial
                    // up to the delta layer.
                    let lsn_floor =
                        std::cmp::max(inmem.get_lsn_range().start, delta.get_lsn_range().end);
                    Some(SearchResult {
                        layer: ReadableLayerWeak::InMemoryLayer(inmem),
                        lsn_floor,
                    })
                }
            }
            (Some(delta), Some(image), Some(inmem)) => {
                // Determine the preferred persistent layer without taking the in-memory layer
                // into consideration.
                let persistent_res =
                    Self::select_layer(Some(delta.clone()), Some(image.clone()), None, end_lsn)
                        .unwrap();
                let persistent_l = match persistent_res.layer {
                    ReadableLayerWeak::PersistentLayer(l) => l,
                    ReadableLayerWeak::InMemoryLayer(_) => unreachable!(),
                };

                // Now handle the in-memory layer overlaps.
                let inmem_res = if persistent_l.is_delta() {
                    Self::select_layer(Some(persistent_l), None, Some(inmem.clone()), end_lsn)
                        .unwrap()
                } else {
                    Self::select_layer(None, Some(persistent_l), Some(inmem.clone()), end_lsn)
                        .unwrap()
                };

                Some(SearchResult {
                    layer: inmem_res.layer,
                    // Use the more restrictive LSN floor
                    lsn_floor: std::cmp::max(persistent_res.lsn_floor, inmem_res.lsn_floor),
                })
            }
        }
    }

    pub fn range_search(&self, key_range: Range<Key>, end_lsn: Lsn) -> RangeSearchResult {
        let in_memory_layer = self.search_in_memory_layer(end_lsn);

        let version = match self.historic.get().unwrap().get_version(end_lsn.0 - 1) {
            Some(version) => version,
            None => {
                return RangeSearchResult::map_to_in_memory_layer(in_memory_layer, key_range);
            }
        };

        let raw_range = key_range.start.to_i128()..key_range.end.to_i128();
        let delta_changes = version.delta_coverage.range_overlaps(&raw_range);
        let image_changes = version.image_coverage.range_overlaps(&raw_range);

        let collector = RangeSearchCollector::new(
            key_range,
            end_lsn,
            in_memory_layer,
            delta_changes,
            image_changes,
        );
        collector.collect()
    }

    /// Start a batch of updates, applied on drop
    pub fn batch_update(&mut self) -> BatchedUpdates<'_> {
        BatchedUpdates { layer_map: self }
    }

    ///
    /// Insert an on-disk layer
    ///
    /// Helper function for BatchedUpdates::insert_historic
    ///
    /// TODO(chi): remove L generic so that we do not need to pass layer object.
    pub(self) fn insert_historic_noflush(&mut self, layer_desc: PersistentLayerDesc) {
        // TODO: See #3869, resulting #4088, attempted fix and repro #4094

        if Self::is_l0(&layer_desc.key_range, layer_desc.is_delta) {
            self.l0_delta_layers.push(layer_desc.clone().into());
            self.watch_l0_deltas
                .send_replace(self.l0_delta_layers.len());
        }

        self.historic.insert(
            historic_layer_coverage::LayerKey::from(&layer_desc),
            layer_desc.into(),
        );
    }

    ///
    /// Remove an on-disk layer from the map.
    ///
    /// Helper function for BatchedUpdates::remove_historic
    ///
    pub fn remove_historic_noflush(&mut self, layer_desc: &PersistentLayerDesc) {
        self.historic
            .remove(historic_layer_coverage::LayerKey::from(layer_desc));
        let layer_key = layer_desc.key();
        if Self::is_l0(&layer_desc.key_range, layer_desc.is_delta) {
            let len_before = self.l0_delta_layers.len();
            let mut l0_delta_layers = std::mem::take(&mut self.l0_delta_layers);
            l0_delta_layers.retain(|other| other.key() != layer_key);
            self.l0_delta_layers = l0_delta_layers;
            self.watch_l0_deltas
                .send_replace(self.l0_delta_layers.len());
            // this assertion is related to use of Arc::ptr_eq in Self::compare_arced_layers,
            // there's a chance that the comparison fails at runtime due to it comparing (pointer,
            // vtable) pairs.
            assert_eq!(
                self.l0_delta_layers.len(),
                len_before - 1,
                "failed to locate removed historic layer from l0_delta_layers"
            );
        }
    }

    /// Helper function for BatchedUpdates::drop.
    pub(self) fn flush_updates(&mut self) {
        self.historic.rebuild();
    }

    /// Is there a newer image layer for given key- and LSN-range? Or a set
    /// of image layers within the specified lsn range that cover the entire
    /// specified key range?
    ///
    /// This is used for garbage collection, to determine if an old layer can
    /// be deleted.
    pub fn image_layer_exists(&self, key: &Range<Key>, lsn: &Range<Lsn>) -> bool {
        if key.is_empty() {
            // Vacuously true. There's a newer image for all 0 of the kerys in the range.
            return true;
        }

        let version = match self.historic.get().unwrap().get_version(lsn.end.0 - 1) {
            Some(v) => v,
            None => return false,
        };

        let start = key.start.to_i128();
        let end = key.end.to_i128();

        let layer_covers = |layer: Option<Arc<PersistentLayerDesc>>| match layer {
            Some(layer) => layer.get_lsn_range().start >= lsn.start,
            None => false,
        };

        // Check the start is covered
        if !layer_covers(version.image_coverage.query(start)) {
            return false;
        }

        // Check after all changes of coverage
        for (_, change_val) in version.image_coverage.range(start..end) {
            if !layer_covers(change_val) {
                return false;
            }
        }

        true
    }

    pub fn iter_historic_layers(&self) -> impl ExactSizeIterator<Item = Arc<PersistentLayerDesc>> {
        self.historic.iter()
    }

    /// Get a ref counted pointer for the first in memory layer that matches the provided predicate.
    pub(crate) fn search_in_memory_layer(&self, below: Lsn) -> Option<InMemoryLayerDesc> {
        let is_below = |l: &Arc<InMemoryLayer>| {
            let start_lsn = l.get_lsn_range().start;
            below > start_lsn
        };

        if let Some(open) = &self.open_layer {
            if is_below(open) {
                return Some(InMemoryLayerDesc {
                    handle: InMemoryLayerHandle::Open,
                    lsn_range: open.get_lsn_range(),
                });
            }
        }

        self.frozen_layers
            .iter()
            .enumerate()
            .rfind(|(_idx, l)| is_below(l))
            .map(|(idx, l)| InMemoryLayerDesc {
                handle: InMemoryLayerHandle::Frozen(idx),
                lsn_range: l.get_lsn_range(),
            })
    }

    ///
    /// Divide the whole given range of keys into sub-ranges based on the latest
    /// image layer that covers each range at the specified lsn (inclusive).
    /// This is used when creating  new image layers.
    pub fn image_coverage(
        &self,
        key_range: &Range<Key>,
        lsn: Lsn,
    ) -> Vec<(Range<Key>, Option<Arc<PersistentLayerDesc>>)> {
        let version = match self.historic.get().unwrap().get_version(lsn.0) {
            Some(v) => v,
            None => return vec![],
        };

        let start = key_range.start.to_i128();
        let end = key_range.end.to_i128();

        // Initialize loop variables
        let mut coverage: Vec<(Range<Key>, Option<Arc<PersistentLayerDesc>>)> = vec![];
        let mut current_key = start;
        let mut current_val = version.image_coverage.query(start);

        // Loop through the change events and push intervals
        for (change_key, change_val) in version.image_coverage.range(start..end) {
            let kr = Key::from_i128(current_key)..Key::from_i128(change_key);
            coverage.push((kr, current_val.take()));
            current_key = change_key;
            current_val.clone_from(&change_val);
        }

        // Add the final interval
        let kr = Key::from_i128(current_key)..Key::from_i128(end);
        coverage.push((kr, current_val.take()));

        coverage
    }

    /// Check if the key range resembles that of an L0 layer.
    pub fn is_l0(key_range: &Range<Key>, is_delta_layer: bool) -> bool {
        is_delta_layer && key_range == &(Key::MIN..Key::MAX)
    }

    /// This function determines which layers are counted in `count_deltas`:
    /// layers that should count towards deciding whether or not to reimage
    /// a certain partition range.
    ///
    /// There are two kinds of layers we currently consider reimage-worthy:
    ///
    /// Case 1: Non-L0 layers are currently reimage-worthy by default.
    /// TODO Some of these layers are very sparse and cover the entire key
    ///      range. Replacing 256MB of data (or less!) with terabytes of
    ///      images doesn't seem wise. We need a better heuristic, possibly
    ///      based on some of these factors:
    ///      a) whether this layer has any wal in this partition range
    ///      b) the size of the layer
    ///      c) the number of images needed to cover it
    ///      d) the estimated time until we'll have to reimage over it for GC
    ///
    /// Case 2: Since L0 layers by definition cover the entire key space, we consider
    /// them reimage-worthy only when the entire key space can be covered by very few
    /// images (currently 1).
    /// TODO The optimal number should probably be slightly higher than 1, but to
    ///      implement that we need to plumb a lot more context into this function
    ///      than just the current partition_range.
    pub fn is_reimage_worthy(layer: &PersistentLayerDesc, partition_range: &Range<Key>) -> bool {
        // Case 1
        if !Self::is_l0(&layer.key_range, layer.is_delta) {
            return true;
        }

        // Case 2
        if partition_range == &(Key::MIN..Key::MAX) {
            return true;
        }

        false
    }

    /// Count the height of the tallest stack of reimage-worthy deltas
    /// in this 2d region.
    ///
    /// If `limit` is provided we don't try to count above that number.
    ///
    /// This number is used to compute the largest number of deltas that
    /// we'll need to visit for any page reconstruction in this region.
    /// We use this heuristic to decide whether to create an image layer.
    pub fn count_deltas(&self, key: &Range<Key>, lsn: &Range<Lsn>, limit: Option<usize>) -> usize {
        // We get the delta coverage of the region, and for each part of the coverage
        // we recurse right underneath the delta. The recursion depth is limited by
        // the largest result this function could return, which is in practice between
        // 3 and 10 (since we usually try to create an image when the number gets larger).

        if lsn.is_empty() || key.is_empty() || limit == Some(0) {
            return 0;
        }

        let version = match self.historic.get().unwrap().get_version(lsn.end.0 - 1) {
            Some(v) => v,
            None => return 0,
        };

        let start = key.start.to_i128();
        let end = key.end.to_i128();

        // Initialize loop variables
        let mut max_stacked_deltas = 0;
        let mut current_key = start;
        let mut current_val = version.delta_coverage.query(start);

        // Loop through the delta coverage and recurse on each part
        for (change_key, change_val) in version.delta_coverage.range(start..end) {
            // If there's a relevant delta in this part, add 1 and recurse down
            if let Some(val) = &current_val {
                if val.get_lsn_range().end > lsn.start {
                    let kr = Key::from_i128(current_key)..Key::from_i128(change_key);
                    let lr = lsn.start..val.get_lsn_range().start;
                    if !kr.is_empty() {
                        let base_count = Self::is_reimage_worthy(val, key) as usize;
                        let new_limit = limit.map(|l| l - base_count);
                        let max_stacked_deltas_underneath = self.count_deltas(&kr, &lr, new_limit);
                        max_stacked_deltas = std::cmp::max(
                            max_stacked_deltas,
                            base_count + max_stacked_deltas_underneath,
                        );
                    }
                }
            }

            current_key = change_key;
            current_val.clone_from(&change_val);
        }

        // Consider the last part
        if let Some(val) = &current_val {
            if val.get_lsn_range().end > lsn.start {
                let kr = Key::from_i128(current_key)..Key::from_i128(end);
                let lr = lsn.start..val.get_lsn_range().start;

                if !kr.is_empty() {
                    let base_count = Self::is_reimage_worthy(val, key) as usize;
                    let new_limit = limit.map(|l| l - base_count);
                    let max_stacked_deltas_underneath = self.count_deltas(&kr, &lr, new_limit);
                    max_stacked_deltas = std::cmp::max(
                        max_stacked_deltas,
                        base_count + max_stacked_deltas_underneath,
                    );
                }
            }
        }

        max_stacked_deltas
    }

    /* BEGIN_HADRON */
    /**
     * Compute the image consistent LSN, the largest LSN below which all pages have been redone successfully.
     * It works by first finding the latest image layers and store them into a map. Then for each delta layer,
     * find all overlapping image layers in order to potentially increase the image LSN in case there are gaps
     * (e.g., if an image is created at LSN 100 but the delta layer spans LSN [150, 200], then we can increase
     * image LSN to 150 because there is no WAL record in between).
     * Finally, the image consistent LSN is computed by taking the minimum of all image layers.
     */
    pub fn compute_image_consistent_lsn(&self, disk_consistent_lsn: Lsn) -> Lsn {
        struct ImageLayerInfo {
            // creation LSN of the image layer
            image_lsn: Lsn,
            // the current minimum LSN of newer delta layers with overlapping key ranges
            min_delta_lsn: Lsn,
        }
        let started_at = Instant::now();

        let min_l0_deltas_lsn = {
            let l0_deltas = self.level0_deltas();
            l0_deltas
                .iter()
                .map(|layer| layer.get_lsn_range().start)
                .min()
                .unwrap_or(disk_consistent_lsn)
        };
        let global_key_range = Key::MIN..Key::MAX;

        // step 1: collect all most recent image layers into a map
        // map: end key to image_layer_info
        let mut image_map: BTreeMap<Key, ImageLayerInfo> = BTreeMap::new();
        for (img_range, img) in self.image_coverage(&global_key_range, disk_consistent_lsn) {
            let img_lsn = img.map(|layer| layer.get_lsn_range().end).unwrap_or(Lsn(0));
            image_map.insert(
                img_range.end,
                ImageLayerInfo {
                    image_lsn: img_lsn,
                    min_delta_lsn: min_l0_deltas_lsn,
                },
            );
        }

        // step 2: go through all delta layers, and update the image layer info with overlapping
        // key ranges
        for layer in self.historic.iter() {
            if !layer.is_delta {
                continue;
            }
            let delta_key_range = layer.get_key_range();
            let delta_lsn_range = layer.get_lsn_range();
            for (img_end_key, img_info) in image_map.range_mut(delta_key_range.start..Key::MAX) {
                debug_assert!(img_end_key >= &delta_key_range.start);
                if delta_lsn_range.end > img_info.image_lsn {
                    // the delta layer includes WAL records after the image
                    // it's possibel that the delta layer's start LSN < image LSN, which will be simply ignored by step 3
                    img_info.min_delta_lsn =
                        std::cmp::min(img_info.min_delta_lsn, delta_lsn_range.start);
                }
                if img_end_key >= &delta_key_range.end {
                    // we have fully processed all overlapping image layers
                    break;
                }
            }
        }

        // step 3, go through all image layers and find the image consistent LSN
        let mut img_consistent_lsn = min_l0_deltas_lsn.checked_sub(Lsn(1)).unwrap();
        let mut prev_key = Key::MIN;
        for (img_key, img_info) in image_map {
            tracing::debug!(
                "Image layer {:?}:{} has min delta lsn {}",
                Range {
                    start: prev_key,
                    end: img_key,
                },
                img_info.image_lsn,
                img_info.min_delta_lsn,
            );
            let image_lsn = std::cmp::max(
                img_info.image_lsn,
                img_info.min_delta_lsn.checked_sub(Lsn(1)).unwrap_or(Lsn(0)),
            );
            img_consistent_lsn = std::cmp::min(img_consistent_lsn, image_lsn);
            prev_key = img_key;
        }
        tracing::info!(
            "computed image_consistent_lsn {} for disk_consistent_lsn {} in {}ms. Processed {} layrs in total.",
            img_consistent_lsn,
            disk_consistent_lsn,
            started_at.elapsed().as_millis(),
            self.historic.len()
        );
        img_consistent_lsn
    }

    /* END_HADRON */

    /// Return all L0 delta layers
    pub fn level0_deltas(&self) -> &Vec<Arc<PersistentLayerDesc>> {
        &self.l0_delta_layers
    }

    /// Subscribes to L0 delta layer changes, sending the current number of L0 delta layers.
    pub fn watch_level0_deltas(&self) -> watch::Receiver<usize> {
        self.watch_l0_deltas.subscribe()
    }

    /// debugging function to print out the contents of the layer map
    #[allow(unused)]
    pub async fn dump(&self, verbose: bool, ctx: &RequestContext) -> Result<()> {
        println!("Begin dump LayerMap");

        println!("open_layer:");
        if let Some(open_layer) = &self.open_layer {
            open_layer.dump(verbose, ctx).await?;
        }

        println!("frozen_layers:");
        for frozen_layer in self.frozen_layers.iter() {
            frozen_layer.dump(verbose, ctx).await?;
        }

        println!("historic_layers:");
        for desc in self.iter_historic_layers() {
            desc.dump();
        }
        println!("End dump LayerMap");
        Ok(())
    }

    /// `read_points` represent the tip of a timeline and any branch points, i.e. the places
    /// where we expect to serve reads.
    ///
    /// This function is O(N) and should be called infrequently.  The caller is responsible for
    /// looking up and updating the Layer objects for these layer descriptors.
    pub fn get_visibility(
        &self,
        mut read_points: Vec<Lsn>,
    ) -> (
        Vec<(Arc<PersistentLayerDesc>, LayerVisibilityHint)>,
        KeySpace,
    ) {
        // This is like a KeySpace, but this type is intended for efficient unions with image layer ranges, whereas
        // KeySpace is intended to be composed statically and iterated over.
        struct KeyShadow {
            // Map of range start to range end
            inner: RangeSetBlaze<i128>,
        }

        impl KeyShadow {
            fn new() -> Self {
                Self {
                    inner: Default::default(),
                }
            }

            fn contains(&self, range: Range<Key>) -> bool {
                let range_incl = range.start.to_i128()..=range.end.to_i128() - 1;
                self.inner.is_superset(&RangeSetBlaze::from_sorted_disjoint(
                    CheckSortedDisjoint::from([range_incl]),
                ))
            }

            /// Add the input range to the keys covered by self.
            ///
            /// Return true if inserting this range covered some keys that were previously not covered
            fn cover(&mut self, insert: Range<Key>) -> bool {
                let range_incl = insert.start.to_i128()..=insert.end.to_i128() - 1;
                self.inner.ranges_insert(range_incl)
            }

            fn reset(&mut self) {
                self.inner = Default::default();
            }

            fn to_keyspace(&self) -> KeySpace {
                let mut accum = KeySpaceAccum::new();
                for range_incl in self.inner.ranges() {
                    let range = Range {
                        start: Key::from_i128(*range_incl.start()),
                        end: Key::from_i128(range_incl.end() + 1),
                    };
                    accum.add_range(range)
                }

                accum.to_keyspace()
            }
        }

        // The 'shadow' will be updated as we sweep through the layers: an image layer subtracts from the shadow,
        // and a ReadPoint
        read_points.sort_by_key(|rp| rp.0);
        let mut shadow = KeyShadow::new();

        // We will interleave all our read points and layers into a sorted collection
        enum Item {
            ReadPoint { lsn: Lsn },
            Layer(Arc<PersistentLayerDesc>),
        }

        let mut items = Vec::with_capacity(self.historic.len() + read_points.len());
        items.extend(self.iter_historic_layers().map(Item::Layer));
        items.extend(
            read_points
                .into_iter()
                .map(|rp| Item::ReadPoint { lsn: rp }),
        );

        // Ordering: we want to iterate like this:
        // 1. Highest LSNs first
        // 2. Consider images before deltas if they end at the same LSNs (images cover deltas)
        // 3. Consider ReadPoints before image layers if they're at the same LSN (readpoints make that image visible)
        items.sort_by_key(|item| {
            std::cmp::Reverse(match item {
                Item::Layer(layer) => {
                    if layer.is_delta() {
                        (Lsn(layer.get_lsn_range().end.0 - 1), 0)
                    } else {
                        (layer.image_layer_lsn(), 1)
                    }
                }
                Item::ReadPoint { lsn } => (*lsn, 2),
            })
        });

        let mut results = Vec::with_capacity(self.historic.len());

        let mut maybe_covered_deltas: Vec<Arc<PersistentLayerDesc>> = Vec::new();

        for item in items {
            let (reached_lsn, is_readpoint) = match &item {
                Item::ReadPoint { lsn } => (lsn, true),
                Item::Layer(layer) => (&layer.lsn_range.start, false),
            };
            maybe_covered_deltas.retain(|d| {
                if *reached_lsn >= d.lsn_range.start && is_readpoint {
                    // We encountered a readpoint within the delta layer: it is visible

                    results.push((d.clone(), LayerVisibilityHint::Visible));
                    false
                } else if *reached_lsn < d.lsn_range.start {
                    // We passed the layer's range without encountering a read point: it is not visible
                    results.push((d.clone(), LayerVisibilityHint::Covered));
                    false
                } else {
                    // We're still in the delta layer: continue iterating
                    true
                }
            });

            match item {
                Item::ReadPoint { lsn: _lsn } => {
                    // TODO: propagate the child timeline's shadow from their own run of this function, so that we don't have
                    // to assume that the whole key range is visible at the branch point.
                    shadow.reset();
                }
                Item::Layer(layer) => {
                    let visibility = if layer.is_delta() {
                        if shadow.contains(layer.get_key_range()) {
                            // If a layer isn't visible based on current state, we must defer deciding whether
                            // it is truly not visible until we have advanced past the delta's range: we might
                            // encounter another branch point within this delta layer's LSN range.
                            maybe_covered_deltas.push(layer);
                            continue;
                        } else {
                            LayerVisibilityHint::Visible
                        }
                    } else {
                        let modified = shadow.cover(layer.get_key_range());
                        if modified {
                            // An image layer in a region which wasn't fully covered yet: this layer is visible, but layers below it will be covered
                            LayerVisibilityHint::Visible
                        } else {
                            // An image layer in a region that was already covered
                            LayerVisibilityHint::Covered
                        }
                    };

                    results.push((layer, visibility));
                }
            }
        }

        // Drain any remaining maybe_covered deltas
        results.extend(
            maybe_covered_deltas
                .into_iter()
                .map(|d| (d, LayerVisibilityHint::Covered)),
        );

        (results, shadow.to_keyspace())
    }
}

