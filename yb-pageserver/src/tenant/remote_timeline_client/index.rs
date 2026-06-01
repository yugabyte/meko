//! In-memory index to track the tenant files on the remote storage.
//!
//! Able to restore itself from the storage index parts, that are located in every timeline's remote directory and contain all data about
//! remote timeline layers and its metadata.

use std::collections::HashMap;

use chrono::NaiveDateTime;
use pageserver_api::models::AuxFilePolicy;
use pageserver_api::models::RelSizeMigration;
use pageserver_api::shard::ShardIndex;
use serde::{Deserialize, Serialize};
use utils::id::TimelineId;
use utils::lsn::Lsn;

use super::is_same_remote_layer_path;
use crate::tenant::Generation;
use crate::tenant::metadata::TimelineMetadata;
use crate::tenant::storage_layer::LayerName;

/// In-memory representation of an `index_part.json` file
///
/// Contains the data about all files in the timeline, present remotely and its metadata.
///
/// This type needs to be backwards and forwards compatible. When changing the fields,
/// remember to add a test case for the changed version.
#[derive(Debug, PartialEq, Eq, Clone, Serialize, Deserialize)]
pub struct IndexPart {
    /// Debugging aid describing the version of this type.
    #[serde(default)]
    version: usize,

    #[serde(default)]
    #[serde(skip_serializing_if = "Option::is_none")]
    pub deleted_at: Option<NaiveDateTime>,

    #[serde(default)]
    #[serde(skip_serializing_if = "Option::is_none")]
    pub archived_at: Option<NaiveDateTime>,

    /// Layer filenames and metadata. For an index persisted in remote storage, all layers must
    /// exist in remote storage.
    pub layer_metadata: HashMap<LayerName, LayerFileMetadata>,

    /// Because of the trouble of eyeballing the legacy "metadata" field, we copied the
    /// "disk_consistent_lsn" out. After version 7 this is no longer needed, but the name cannot be
    /// reused.
    pub(super) disk_consistent_lsn: Lsn,

    // TODO: rename as "metadata" next week, keep the alias = "metadata_bytes", bump version Adding
    // the "alias = metadata" was forgotten in #7693, so we have to use "rewrite = metadata_bytes"
    // for backwards compatibility.
    #[serde(
        rename = "metadata_bytes",
        alias = "metadata",
        with = "crate::tenant::metadata::modern_serde"
    )]
    pub metadata: TimelineMetadata,

    #[serde(default)]
    pub(crate) lineage: Lineage,

    #[serde(skip_serializing_if = "Option::is_none", default)]
    pub(crate) gc_blocking: Option<GcBlocking>,

    /// Describes the kind of aux files stored in the timeline.
    ///
    /// The value is modified during file ingestion when the latest wanted value communicated via tenant config is applied if it is acceptable.
    /// A V1 setting after V2 files have been committed is not accepted.
    ///
    /// None means no aux files have been written to the storage before the point
    /// when this flag is introduced.
    ///
    /// This flag is not used any more as all tenants have been transitioned to the new aux file policy.
    #[serde(skip_serializing_if = "Option::is_none", default)]
    pub(crate) last_aux_file_policy: Option<AuxFilePolicy>,

    #[serde(skip_serializing_if = "Option::is_none", default)]
    pub(crate) rel_size_migration: Option<RelSizeMigration>,

    /// Not used anymore -- kept here for backwards compatibility. Merged into the `gc_compaction` field.
    #[serde(skip_serializing_if = "Option::is_none", default)]
    l2_lsn: Option<Lsn>,

    /// State for the garbage-collecting compaction pass.
    ///
    /// Garbage-collecting compaction (gc-compaction) prunes `Value`s that are outside
    /// the PITR window and not needed by child timelines.
    ///
    /// A commonly used synonym for this compaction pass is
    /// "bottommost-compaction"  because the affected LSN range
    /// is the "bottom" of the (key,lsn) map.
    ///
    /// Gc-compaction is a quite expensive operation; that's why we use
    /// trigger condition.
    /// This field here holds the state pertaining to that trigger condition
    /// and (in future) to the progress of the gc-compaction, so that it's
    /// resumable across restarts & migrations.
    ///
    /// Note that the underlying algorithm is _also_ called `gc-compaction`
    /// in most places & design docs; but in fact it is more flexible than
    /// just the specific use case here; it needs a new name.
    #[serde(skip_serializing_if = "Option::is_none", default)]
    pub(crate) gc_compaction: Option<GcCompactionState>,

    /// The timestamp when the timeline was marked invisible in synthetic size calculations.
    #[serde(skip_serializing_if = "Option::is_none", default)]
    pub(crate) marked_invisible_at: Option<NaiveDateTime>,

    /// The LSN at which we started the rel size migration. Accesses below this LSN should be
    /// processed with the v1 read path. Usually this LSN should be set together with `rel_size_migration`.
    #[serde(skip_serializing_if = "Option::is_none", default)]
    pub(crate) rel_size_migrated_at: Option<Lsn>,
}

#[derive(Debug, PartialEq, Eq, Clone, Serialize, Deserialize)]
pub struct GcCompactionState {
    /// The upper bound of the last completed garbage-collecting compaction, aka. L2 LSN.
    pub(crate) last_completed_lsn: Lsn,
}

impl IndexPart {
    /// When adding or modifying any parts of `IndexPart`, increment the version so that it can be
    /// used to understand later versions.
    ///
    /// Version is currently informative only.
    /// Version history
    /// - 2: added `deleted_at`
    /// - 3: no longer deserialize `timeline_layers` (serialized format is the same, but timeline_layers
    ///   is always generated from the keys of `layer_metadata`)
    /// - 4: timeline_layers is fully removed.
    /// - 5: lineage was added
    /// - 6: last_aux_file_policy is added.
    /// - 7: metadata_bytes is no longer written, but still read
    /// - 8: added `archived_at`
    /// - 9: +gc_blocking
    /// - 10: +import_pgdata
    /// - 11: +rel_size_migration
    /// - 12: +l2_lsn
    /// - 13: +gc_compaction
    /// - 14: +marked_invisible_at
    /// - 15: +rel_size_migrated_at
    const LATEST_VERSION: usize = 15;

    // Versions we may see when reading from a bucket.
    pub const KNOWN_VERSIONS: &'static [usize] =
        &[1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15];

    pub const FILE_NAME: &'static str = "index_part.json";

    pub fn empty(metadata: TimelineMetadata) -> Self {
        IndexPart {
            version: Self::LATEST_VERSION,
            layer_metadata: Default::default(),
            disk_consistent_lsn: metadata.disk_consistent_lsn(),
            metadata,
            deleted_at: None,
            archived_at: None,
            lineage: Default::default(),
            gc_blocking: None,
            last_aux_file_policy: None,
            rel_size_migration: None,
            l2_lsn: None,
            gc_compaction: None,
            marked_invisible_at: None,
            rel_size_migrated_at: None,
        }
    }

    pub fn version(&self) -> usize {
        self.version
    }

    /// If you want this under normal operations, read it from self.metadata:
    /// this method is just for the scrubber to use when validating an index.
    pub fn duplicated_disk_consistent_lsn(&self) -> Lsn {
        self.disk_consistent_lsn
    }

    pub fn from_json_bytes(bytes: &[u8]) -> Result<Self, serde_json::Error> {
        serde_json::from_slice::<IndexPart>(bytes)
    }

    pub fn to_json_bytes(&self) -> serde_json::Result<Vec<u8>> {
        serde_json::to_vec(self)
    }

    #[cfg(test)]
    pub(crate) fn example() -> Self {
        Self::empty(TimelineMetadata::example())
    }

    /// Returns true if the index contains a reference to the given layer (i.e. file path).
    ///
    /// TODO: there should be a variant of LayerName for the physical remote path that contains
    /// information about the shard and generation, to avoid passing in metadata.
    pub fn references(&self, name: &LayerName, metadata: &LayerFileMetadata) -> bool {
        let Some(index_metadata) = self.layer_metadata.get(name) else {
            return false;
        };
        is_same_remote_layer_path(name, metadata, name, index_metadata)
    }

    /// Check for invariants in the index: this is useful when uploading an index to ensure that if
    /// we encounter a bug, we do not persist buggy metadata.
    pub(crate) fn validate(&self) -> Result<(), String> {
        if self.metadata.ancestor_timeline().is_none()
            && self.layer_metadata.is_empty()
        {
            // Unless we're in the middle of a raw pgdata import, or this is a child timeline,the index must
            // always have at least one layer.
            return Err("Index has no ancestor and no layers".to_string());
        }

        Ok(())
    }
}

/// Metadata gathered for each of the layer files.
///
/// Fields have to be `Option`s because remote [`IndexPart`]'s can be from different version, which
/// might have less or more metadata depending if upgrading or rolling back an upgrade.
#[derive(Debug, Clone, PartialEq, Eq, PartialOrd, Ord, Serialize, Deserialize)]
pub struct LayerFileMetadata {
    pub file_size: u64,

    #[serde(default = "Generation::none")]
    #[serde(skip_serializing_if = "Generation::is_none")]
    pub generation: Generation,

    #[serde(default = "ShardIndex::unsharded")]
    #[serde(skip_serializing_if = "ShardIndex::is_unsharded")]
    pub shard: ShardIndex,
}

impl LayerFileMetadata {
    pub fn new(file_size: u64, generation: Generation, shard: ShardIndex) -> Self {
        LayerFileMetadata {
            file_size,
            generation,
            shard,
        }
    }
    /// Helper to get both generation and file size in a tuple
    pub fn generation_file_size(&self) -> (Generation, u64) {
        (self.generation, self.file_size)
    }
}

/// Limited history of earlier ancestors.
///
/// A timeline can have more than 1 earlier ancestor, in the rare case that it was repeatedly
/// reparented by having an later timeline be detached from it's ancestor.
#[derive(Debug, PartialEq, Eq, Clone, Serialize, Deserialize, Default)]
pub(crate) struct Lineage {
    /// Has the `reparenting_history` been truncated to [`Lineage::REMEMBER_AT_MOST`].
    #[serde(skip_serializing_if = "is_false", default)]
    reparenting_history_truncated: bool,

    /// Earlier ancestors, truncated when [`Self::reparenting_history_truncated`]
    ///
    /// These are stored in case we want to support WAL based DR on the timeline. There can be many
    /// of these and at most one [`Self::original_ancestor`]. There cannot be more reparentings
    /// after [`Self::original_ancestor`] has been set.
    #[serde(skip_serializing_if = "Vec::is_empty", default)]
    reparenting_history: Vec<TimelineId>,

    /// The ancestor from which this timeline has been detached from and when.
    ///
    /// If you are adding support for detaching from a hierarchy, consider changing the ancestry
    /// into a `Vec<(TimelineId, Lsn)>` to be a path instead.
    // FIXME: this is insufficient even for path of two timelines for future wal recovery
    // purposes:
    //
    // assuming a "old main" which has received most of the WAL, and has a branch "new main",
    // starting a bit before "old main" last_record_lsn. the current version works fine,
    // because we will know to replay wal and branch at the recorded Lsn to do wal recovery.
    //
    // then assuming "new main" would similarly receive a branch right before its last_record_lsn,
    // "new new main". the current implementation would just store ("new main", ancestor_lsn, _)
    // here. however, we cannot recover from WAL using only that information, we would need the
    // whole ancestry here:
    //
    // ```json
    // [
    //   ["old main", ancestor_lsn("new main"), _],
    //   ["new main", ancestor_lsn("new new main"), _]
    // ]
    // ```
    #[serde(skip_serializing_if = "Option::is_none", default)]
    original_ancestor: Option<(TimelineId, Lsn, NaiveDateTime)>,
}

fn is_false(b: &bool) -> bool {
    !b
}

impl Lineage {
    /// The queried lsn is most likely the basebackup lsn, and this answers question "is it allowed
    /// to start a read/write primary at this lsn".
    pub(crate) fn is_previous_ancestor_lsn(&self, lsn: Lsn) -> bool {
        self.original_ancestor
            .is_some_and(|(_, ancestor_lsn, _)| ancestor_lsn == lsn)
    }

    /// Returns true if the timeline originally had an ancestor, and no longer has one.
    pub(crate) fn is_detached_from_ancestor(&self) -> bool {
        self.original_ancestor.is_some()
    }
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub(crate) struct GcBlocking {
    pub(crate) started_at: NaiveDateTime,
    pub(crate) reasons: enumset::EnumSet<GcBlockingReason>,
}

#[derive(Debug, enumset::EnumSetType, serde::Serialize, serde::Deserialize)]
#[enumset(serialize_repr = "list")]
pub(crate) enum GcBlockingReason {
    Manual,
    DetachAncestor,
}

impl GcBlocking {
    pub(super) fn started_now_for(reason: GcBlockingReason) -> Self {
        GcBlocking {
            started_at: chrono::Utc::now().naive_utc(),
            reasons: enumset::EnumSet::only(reason),
        }
    }

    /// Returns true if the given reason is one of the reasons why the gc is blocked.
    pub(crate) fn blocked_by(&self, reason: GcBlockingReason) -> bool {
        self.reasons.contains(reason)
    }

    /// Returns a version of self with the given reason.
    pub(super) fn with_reason(&self, reason: GcBlockingReason) -> Self {
        assert!(!self.blocked_by(reason));
        let mut reasons = self.reasons;
        reasons.insert(reason);

        Self {
            started_at: self.started_at,
            reasons,
        }
    }

    /// Returns a version of self without the given reason. Assumption is that if
    /// there are no more reasons, we can unblock the gc by returning `None`.
    pub(super) fn without_reason(&self, reason: GcBlockingReason) -> Option<Self> {
        assert!(self.blocked_by(reason));

        if self.reasons.len() == 1 {
            None
        } else {
            let mut reasons = self.reasons;
            assert!(reasons.remove(reason));
            assert!(!reasons.is_empty());

            Some(Self {
                started_at: self.started_at,
                reasons,
            })
        }
    }
}

