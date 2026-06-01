use chrono::NaiveDateTime;
use serde::{Deserialize, Serialize};
use utils::id::TimelineId;
use utils::lsn::Lsn;
use utils::shard::ShardStripeSize;

/// Tenant shard manifest, stored in remote storage. Contains offloaded timelines and other tenant
/// shard-wide information that must be persisted in remote storage.
///
/// The manifest is always updated on tenant attach, and as needed.
#[derive(Clone, Debug, Serialize, Deserialize, PartialEq, Eq)]
pub struct TenantManifest {
    /// The manifest version. Incremented on manifest format changes, even non-breaking ones.
    /// Manifests must generally always be backwards and forwards compatible for one release, to
    /// allow release rollbacks.
    pub version: usize,

    /// This tenant's stripe size. This is only advisory, and used to recover tenant data from
    /// remote storage. The autoritative source is the storage controller. If None, assume the
    /// original default value of 32768 blocks (256 MB).
    #[serde(skip_serializing_if = "Option::is_none")]
    pub stripe_size: Option<ShardStripeSize>,

    /// The list of offloaded timelines together with enough information
    /// to not have to actually load them.
    ///
    /// Note: the timelines mentioned in this list might be deleted, i.e.
    /// we don't hold an invariant that the references aren't dangling.
    /// Existence of index-part.json is the actual indicator of timeline existence.
    #[serde(default)]
    pub offloaded_timelines: Vec<OffloadedTimelineManifest>,
}

/// The remote level representation of an offloaded timeline.
///
/// Very similar to [`pageserver_api::models::OffloadedTimelineInfo`],
/// but the two datastructures serve different needs, this is for a persistent disk format
/// that must be backwards compatible, while the other is only for informative purposes.
#[derive(Clone, Debug, Serialize, Deserialize, Copy, PartialEq, Eq)]
pub struct OffloadedTimelineManifest {
    pub timeline_id: TimelineId,
    /// Whether the timeline has a parent it has been branched off from or not
    pub ancestor_timeline_id: Option<TimelineId>,
    /// Whether to retain the branch lsn at the ancestor or not
    pub ancestor_retain_lsn: Option<Lsn>,
    /// The time point when the timeline was archived
    pub archived_at: NaiveDateTime,
}

/// The newest manifest version. This should be incremented on changes, even non-breaking ones. We
/// do not use deny_unknown_fields, so new fields are not breaking.
///
/// 1: initial version
/// 2: +stripe_size
///
/// When adding new versions, also add a parse_vX test case below.
pub const LATEST_TENANT_MANIFEST_VERSION: usize = 2;

impl TenantManifest {
    /// Returns true if the manifests are equal, ignoring the version number. This avoids
    /// re-uploading all manifests just because the version number is bumped.
    pub fn eq_ignoring_version(&self, other: &Self) -> bool {
        // Fast path: if the version is equal, just compare directly.
        if self.version == other.version {
            return self == other;
        }

        // We could alternatively just clone and modify the version here.
        let Self {
            version: _, // ignore version
            stripe_size,
            offloaded_timelines,
        } = self;

        stripe_size == &other.stripe_size && offloaded_timelines == &other.offloaded_timelines
    }

    /// Decodes a manifest from JSON.
    pub fn from_json_bytes(bytes: &[u8]) -> Result<Self, serde_json::Error> {
        serde_json::from_slice(bytes)
    }

    /// Encodes a manifest as JSON.
    pub fn to_json_bytes(&self) -> serde_json::Result<Vec<u8>> {
        serde_json::to_vec(self)
    }
}

