//! Persisted types for the safekeeper's partial-segment-upload state.
//!
//! In upstream Neon the safekeeper periodically uploaded each
//! timeline's trailing (in-progress) WAL segment to S3 so that a
//! crashed SK didn't lose the last few hundred KB of WAL. In
//! yb-light WAL is in `yb_storage.wals` as soon as it arrives, so
//! there is no partial-segment upload work to do — the entire
//! `PartialBackup` task + its `main_task` + the S3 segment-name
//! helpers were deleted in Phase 9 Wave 1.
//!
//! The two types here (`PartialRemoteSegment`, `State`, plus the
//! `UploadStatus` enum) survive because `TimelinePersistentState`
//! still serializes a `partial_backup: State` field as part of the
//! control file format, and existing control files have it on disk
//! / in YB. Removing the field is gated on either a versioned
//! migration or a clean break of the on-disk format — out of scope
//! here. In practice the field stays at `State::default()`
//! (`segments: vec![]`) for the lifetime of any yb-light timeline.

use safekeeper_api::Term;
use serde::{Deserialize, Serialize};
use utils::lsn::Lsn;

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub enum UploadStatus {
    /// Upload is in progress. Garbage-collection-only.
    InProgress,
    /// Upload finished — at most one segment per timeline carries this.
    Uploaded,
    /// Deletion is in progress. Garbage-collection-only.
    Deleting,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub struct PartialRemoteSegment {
    pub status: UploadStatus,
    pub name: String,
    pub commit_lsn: Lsn,
    pub flush_lsn: Lsn,
    /// Last-log-term at upload time — same role as `term` on a vote
    /// record. Kept for backcompat with the on-disk format.
    pub term: Term,
}

/// On-disk record of which partial segments the SK has uploaded.
/// Always empty in yb-light. Don't drop the field without bumping
/// the control-file format version.
// NB: these structures are part of the control file. Don't change
// them without changing the control-file format version.
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Default)]
pub struct State {
    pub segments: Vec<PartialRemoteSegment>,
}
