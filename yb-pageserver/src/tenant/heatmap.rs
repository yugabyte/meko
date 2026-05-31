use std::collections::HashMap;
use std::time::SystemTime;

use serde::{Deserialize, Serialize};
use serde_with::{DisplayFromStr, TimestampSeconds, serde_as};
use utils::generation::Generation;
use utils::id::TimelineId;

use crate::tenant::remote_timeline_client::index::LayerFileMetadata;
use crate::tenant::storage_layer::LayerName;

#[derive(Serialize, Deserialize)]
pub(crate) struct HeatMapTenant {
    /// Generation of the attached location that uploaded the heatmap: this is not required
    /// for correctness, but acts as a hint to secondary locations in order to detect thrashing
    /// in the unlikely event that two attached locations are both uploading conflicting heatmaps.
    pub(crate) generation: Generation,

    pub(crate) timelines: Vec<HeatMapTimeline>,

    /// Uploaders provide their own upload period in the heatmap, as a hint to downloaders
    /// of how frequently it is worthwhile to check for updates.
    ///
    /// This is optional for backward compat, and because we sometimes might upload
    /// a heatmap explicitly via API for a tenant that has no periodic upload configured.
    #[serde(default)]
    pub(crate) upload_period_ms: Option<u128>,
}

impl HeatMapTenant {
    pub(crate) fn into_timelines_index(self) -> HashMap<TimelineId, HeatMapTimeline> {
        self.timelines
            .into_iter()
            .map(|htl| (htl.timeline_id, htl))
            .collect()
    }
}

#[serde_as]
#[derive(Serialize, Deserialize, Clone)]
pub(crate) struct HeatMapTimeline {
    #[serde_as(as = "DisplayFromStr")]
    pub(crate) timeline_id: TimelineId,

    layers: Vec<HeatMapLayer>,
}

#[serde_as]
#[derive(Serialize, Deserialize, Clone)]
pub(crate) struct HeatMapLayer {
    pub(crate) name: LayerName,
    pub(crate) metadata: LayerFileMetadata,

    #[serde_as(as = "TimestampSeconds<i64>")]
    pub(crate) access_time: SystemTime,

    #[serde(default)]
    pub(crate) cold: bool,
}

impl HeatMapLayer {
    pub(crate) fn new(
        name: LayerName,
        metadata: LayerFileMetadata,
        access_time: SystemTime,
        cold: bool,
    ) -> Self {
        Self {
            name,
            metadata,
            access_time,
            cold,
        }
    }
}

impl HeatMapTimeline {
    pub(crate) fn new(timeline_id: TimelineId, layers: Vec<HeatMapLayer>) -> Self {
        Self {
            timeline_id,
            layers,
        }
    }

    pub(crate) fn into_hot_layers(self) -> impl Iterator<Item = HeatMapLayer> {
        self.layers.into_iter().filter(|l| !l.cold)
    }

    pub(crate) fn hot_layers(&self) -> impl Iterator<Item = &HeatMapLayer> {
        self.layers.iter().filter(|l| !l.cold)
    }

    pub(crate) fn all_layers(&self) -> impl Iterator<Item = &HeatMapLayer> {
        self.layers.iter()
    }
}
