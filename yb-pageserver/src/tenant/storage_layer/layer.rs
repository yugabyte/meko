//! yb-pageserver: vestigial layer-file machinery.
//!
//! In yb-mode there are no on-disk layer files: writes go straight
//! through `PageWriter` into the row store, reads come back the same
//! way. The `Layer` / `ResidentLayer` types still appear in
//! `LayerMap` / `LayerManager` signatures, so we keep them as
//! compile-only shells. Neither is ever constructed at runtime.

use std::sync::Arc;

use camino::{Utf8Path, Utf8PathBuf};
use pageserver_api::models::HistoricLayerInfo;
use tracing::warn;
use utils::generation::Generation;

use super::{AsLayerDesc, LayerAccessStatsReset, PersistentLayerDesc};
use crate::context::RequestContext;

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

    pub(crate) fn local_path(&self) -> &Utf8Path {
        &self.0.path
    }

    pub(crate) fn info(&self, _reset: LayerAccessStatsReset) -> HistoricLayerInfo {
        warn!("Layer::info called in yb-mode; this should be unreachable");
        unreachable!("yb-pageserver never constructs Layer at runtime");
    }

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

#[allow(dead_code)]
pub(crate) async fn dump_unused(_ctx: &RequestContext) {
    // placeholder so `RequestContext` import isn't ever lost in future
    // edits; the only purpose of this module now is to keep the type
    // shapes alive.
}
