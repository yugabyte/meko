use std::collections::HashMap;
use std::mem::ManuallyDrop;
use std::ops::{Deref, DerefMut};
use std::time::Duration;

use anyhow::Context;
use utils::lsn::Lsn;

use super::TimelineWriterState;
use crate::tenant::layer_map::LayerMap;
use crate::tenant::storage_layer::{
    AsLayerDesc, Layer, PersistentLayerDesc,
    PersistentLayerKey,
};

/// Warn if the lock was held for longer than this threshold.
/// It's very generous and we should bring this value down over time.
const LAYER_MANAGER_LOCK_WARN_THRESHOLD: Duration = Duration::from_secs(5);
const LAYER_MANAGER_LOCK_READ_WARN_THRESHOLD: Duration = Duration::from_secs(30);

/// Describes the operation that is holding the layer manager lock
#[derive(Debug, Clone, Copy, strum_macros::Display)]
#[strum(serialize_all = "kebab_case")]
pub(crate) enum LayerManagerLockHolder {
    GetLayerMapInfo,
    Init,
    LoadLayerMap,
    Shutdown,
}

/// Wrapper for the layer manager that tracks the amount of time during which
/// it was held under read or write lock
#[derive(Default)]
pub(crate) struct LockedLayerManager {
    locked: tokio::sync::RwLock<LayerManager>,
}

pub(crate) struct LayerManagerReadGuard<'a> {
    guard: ManuallyDrop<tokio::sync::RwLockReadGuard<'a, LayerManager>>,
    acquired_at: std::time::Instant,
    holder: LayerManagerLockHolder,
}

pub(crate) struct LayerManagerWriteGuard<'a> {
    guard: ManuallyDrop<tokio::sync::RwLockWriteGuard<'a, LayerManager>>,
    acquired_at: std::time::Instant,
    holder: LayerManagerLockHolder,
}

impl Drop for LayerManagerReadGuard<'_> {
    fn drop(&mut self) {
        // Drop the lock first, before potentially warning if it was held for too long.
        // SAFETY: ManuallyDrop in Drop implementation
        unsafe { ManuallyDrop::drop(&mut self.guard) };

        let held_for = self.acquired_at.elapsed();
        if held_for >= LAYER_MANAGER_LOCK_READ_WARN_THRESHOLD {
            tracing::warn!(
                holder=%self.holder,
                "Layer manager read lock held for {}s",
                held_for.as_secs_f64(),
            );
        }
    }
}

impl Drop for LayerManagerWriteGuard<'_> {
    fn drop(&mut self) {
        // Drop the lock first, before potentially warning if it was held for too long.
        // SAFETY: ManuallyDrop in Drop implementation
        unsafe { ManuallyDrop::drop(&mut self.guard) };

        let held_for = self.acquired_at.elapsed();
        if held_for >= LAYER_MANAGER_LOCK_WARN_THRESHOLD {
            tracing::warn!(
                holder=%self.holder,
                "Layer manager write lock held for {}s",
                held_for.as_secs_f64(),
            );
        }
    }
}

impl Deref for LayerManagerReadGuard<'_> {
    type Target = LayerManager;

    fn deref(&self) -> &Self::Target {
        self.guard.deref()
    }
}

impl Deref for LayerManagerWriteGuard<'_> {
    type Target = LayerManager;

    fn deref(&self) -> &Self::Target {
        self.guard.deref()
    }
}

impl DerefMut for LayerManagerWriteGuard<'_> {
    fn deref_mut(&mut self) -> &mut Self::Target {
        self.guard.deref_mut()
    }
}

impl LockedLayerManager {
    pub(crate) async fn read(&self, holder: LayerManagerLockHolder) -> LayerManagerReadGuard {
        let guard = ManuallyDrop::new(self.locked.read().await);
        LayerManagerReadGuard {
            guard,
            acquired_at: std::time::Instant::now(),
            holder,
        }
    }

    pub(crate) async fn write(&self, holder: LayerManagerLockHolder) -> LayerManagerWriteGuard {
        let guard = ManuallyDrop::new(self.locked.write().await);
        LayerManagerWriteGuard {
            guard,
            acquired_at: std::time::Instant::now(),
            holder,
        }
    }

    pub(crate) fn try_write(
        &self,
        holder: LayerManagerLockHolder,
    ) -> Result<LayerManagerWriteGuard, tokio::sync::TryLockError> {
        let guard = ManuallyDrop::new(self.locked.try_write()?);

        Ok(LayerManagerWriteGuard {
            guard,
            acquired_at: std::time::Instant::now(),
            holder,
        })
    }
}

/// Provides semantic APIs to manipulate the layer map.
pub(crate) enum LayerManager {
    /// Open as in not shutdown layer manager; we still have in-memory layers and we can manipulate
    /// the layers.
    Open(OpenLayerManager),
    /// Shutdown layer manager where there are no more in-memory layers and persistent layers are
    /// read-only.
    Closed {
        layers: HashMap<PersistentLayerKey, Layer>,
    },
}

impl Default for LayerManager {
    fn default() -> Self {
        LayerManager::Open(OpenLayerManager::default())
    }
}

impl LayerManager {
    pub(crate) fn get_from_key(&self, key: &PersistentLayerKey) -> Layer {
        // The assumption for the `expect()` is that all code maintains the following invariant:
        // A layer's descriptor is present in the LayerMap => the LayerFileManager contains a layer for the descriptor.
        self.try_get_from_key(key)
            .with_context(|| format!("get layer from key: {key}"))
            .expect("not found")
            .clone()
    }

    pub(crate) fn try_get_from_key(&self, key: &PersistentLayerKey) -> Option<&Layer> {
        self.layers().get(key)
    }

    pub(crate) fn get_from_desc(&self, desc: &PersistentLayerDesc) -> Layer {
        self.get_from_key(&desc.key())
    }

    /// Get an immutable reference to the layer map.
    ///
    /// We expect users only to be able to get an immutable layer map. If users want to make modifications,
    /// they should use the below semantic APIs. This design makes us step closer to immutable storage state.
    pub(crate) fn layer_map(&self) -> Result<&LayerMap, Shutdown> {
        use LayerManager::*;
        match self {
            Open(OpenLayerManager { layer_map, .. }) => Ok(layer_map),
            Closed { .. } => Err(Shutdown),
        }
    }

    pub(crate) fn open_mut(&mut self) -> Result<&mut OpenLayerManager, Shutdown> {
        use LayerManager::*;

        match self {
            Open(open) => Ok(open),
            Closed { .. } => Err(Shutdown),
        }
    }

    /// LayerManager shutdown. The in-memory layers do cleanup on drop, so we must drop them in
    /// order to allow shutdown to complete.
    ///
    /// If there was a want to flush in-memory layers, it must have happened earlier.
    pub(crate) fn shutdown(&mut self, writer_state: &mut Option<TimelineWriterState>) {
        use LayerManager::*;
        match self {
            Open(OpenLayerManager {
                layer_map,
                layer_fmgr: LayerFileManager(hashmap),
            }) => {
                // NB: no need to decrement layer metrics; metrics are removed on timeline shutdown.
                let open = layer_map.open_layer.take();
                let frozen = layer_map.frozen_layers.len();
                let taken_writer_state = writer_state.take();
                tracing::info!(open = open.is_some(), frozen, "dropped inmemory layers");
                let layers = std::mem::take(hashmap);
                *self = Closed { layers };
                assert_eq!(open.is_some(), taken_writer_state.is_some());
            }
            Closed { .. } => {
                tracing::debug!("ignoring multiple shutdowns on layer manager")
            }
        }
    }

    /// Sum up the historic layer sizes
    pub(crate) fn layer_size_sum(&self) -> u64 {
        self.layers()
            .values()
            .map(|l| l.layer_desc().file_size)
            .sum()
    }

    pub(crate) fn likely_resident_layers(&self) -> impl Iterator<Item = &'_ Layer> + '_ {
        self.layers().values().filter(|l| l.is_likely_resident())
    }

    fn layers(&self) -> &HashMap<PersistentLayerKey, Layer> {
        use LayerManager::*;
        match self {
            Open(OpenLayerManager { layer_fmgr, .. }) => &layer_fmgr.0,
            Closed { layers } => layers,
        }
    }
}

#[derive(Default)]
pub(crate) struct OpenLayerManager {
    layer_map: LayerMap,
    layer_fmgr: LayerFileManager<Layer>,
}

impl std::fmt::Debug for OpenLayerManager {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("OpenLayerManager")
            .field("layer_count", &self.layer_fmgr.0.len())
            .finish()
    }
}

#[derive(Debug, thiserror::Error)]
#[error("layer manager has been shutdown")]
pub(crate) struct Shutdown;

impl OpenLayerManager {
    /// Initialize when creating a new timeline, called in `init_empty_layer_map`.
    pub(crate) fn initialize_empty(&mut self, next_open_layer_at: Lsn) {
        self.layer_map.next_open_layer_at = Some(next_open_layer_at);
    }


}

pub(crate) struct LayerFileManager<T>(HashMap<PersistentLayerKey, T>);

impl<T> Default for LayerFileManager<T> {
    fn default() -> Self {
        Self(HashMap::default())
    }
}

