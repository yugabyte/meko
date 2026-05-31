//! yb-pageserver: vestigial layer-file machinery.
//!
//! In yb-mode there are no on-disk layer files: writes go straight
//! through `PageWriter` into the row store, reads come back the same
//! way, and the upload queue's `UploadLayer` variant is never
//! constructed. The `Layer` / `ResidentLayer` types still appear in
//! signatures throughout the layer-map / upload-queue / HTTP code
//! paths, so we keep them as compile-only shells.
//!
//! Every method that used to touch the filesystem (download, evict,
//! delete, get_or_maybe_download, dump-of-contents) is gone. What
//! remains is the bare minimum the callers reach for while iterating
//! over the (always empty) layer collections.

use std::sync::Arc;

use camino::{Utf8Path, Utf8PathBuf};
use pageserver_api::models::HistoricLayerInfo;
use tracing::warn;
use utils::generation::Generation;

use super::{AsLayerDesc, LayerAccessStatsReset, PersistentLayerDesc};
use crate::context::RequestContext;
use crate::tenant::remote_timeline_client::LayerFileMetadata;

/// A Layer is what the layer-map code shuffles around. In yb-mode it
/// is never constructed at runtime — all the maps are empty.
#[derive(Clone)]
pub(crate) struct Layer(Arc<LayerInner>);

struct LayerInner {
    desc: PersistentLayerDesc,
    path: Utf8PathBuf,
    generation: Generation,
}

impl std::fmt::Display for Layer {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(
            f,
            "{}{}",
            self.layer_desc().short_id(),
            self.0.generation.get_suffix()
        )
    }
}

impl std::fmt::Debug for Layer {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "{self}")
    }
}

impl AsLayerDesc for Layer {
    fn layer_desc(&self) -> &PersistentLayerDesc {
        &self.0.desc
    }
}

impl PartialEq for Layer {
    fn eq(&self, other: &Self) -> bool {
        Arc::as_ptr(&self.0) == Arc::as_ptr(&other.0)
    }
}

impl Layer {
    pub(crate) fn is_likely_resident(&self) -> bool {
        false
    }

    pub(crate) fn delete_on_drop(&self) {
        // yb-mode: never constructed, so this drop-time hook never
        // fires. Kept so the layer-manager's gc cleanup path compiles.
    }

    pub(crate) fn local_path(&self) -> &Utf8Path {
        &self.0.path
    }

    pub(crate) fn info(&self, _reset: LayerAccessStatsReset) -> HistoricLayerInfo {
        // yb-mode: layers are never constructed, so this is unreachable.
        // If a caller ever does reach here it points at a real regression.
        warn!("Layer::info called in yb-mode; this should be unreachable");
        unreachable!("yb-pageserver never constructs Layer at runtime");
    }

    pub(crate) async fn dump(&self, _verbose: bool, _ctx: &RequestContext) -> anyhow::Result<()> {
        Ok(())
    }

    /// Waits until this layer has been dropped. In yb-mode it returns
    /// immediately: layers are never constructed so nothing is ever
    /// waiting to be dropped.
    #[cfg(any(feature = "testing", test))]
    pub(crate) fn wait_drop(&self) -> impl std::future::Future<Output = ()> + 'static {
        async {}
    }
}

/// Eviction-guard view of a Layer. Never constructed in yb-mode.
#[derive(Clone)]
pub(crate) struct ResidentLayer {
    owner: Layer,
}

impl std::fmt::Display for ResidentLayer {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "{}", self.owner)
    }
}

impl std::fmt::Debug for ResidentLayer {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "{}", self.owner)
    }
}

impl AsLayerDesc for ResidentLayer {
    fn layer_desc(&self) -> &PersistentLayerDesc {
        self.owner.layer_desc()
    }
}

impl AsRef<Layer> for ResidentLayer {
    fn as_ref(&self) -> &Layer {
        &self.owner
    }
}

impl From<ResidentLayer> for Layer {
    fn from(value: ResidentLayer) -> Self {
        value.owner
    }
}

impl ResidentLayer {
    pub(crate) fn local_path(&self) -> &Utf8Path {
        self.owner.local_path()
    }

    pub(crate) fn metadata(&self) -> LayerFileMetadata {
        // yb-mode: ResidentLayer is never constructed at runtime.
        warn!("ResidentLayer::metadata called in yb-mode; should be unreachable");
        unreachable!("yb-pageserver never constructs ResidentLayer at runtime");
    }
}

#[derive(Debug, thiserror::Error)]
pub(crate) enum EvictionError {
    #[error("layer not found")]
    NotFound,
    #[error("layer is being downloaded")]
    Downloaded,
    #[error("eviction timed out")]
    Timeout,
}

/// Errors surfaced by the HTTP `/download_layer` admin endpoint. In
/// yb-mode that endpoint is a no-op (no layers to download), so these
/// variants are unreachable at runtime but still referenced in match
/// arms.
#[derive(Debug, thiserror::Error)]
pub(crate) enum DownloadError {
    #[error("timeline shutdown")]
    TimelineShutdown,
    #[error("download cancelled")]
    DownloadCancelled,
}
