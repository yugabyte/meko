//! Types for the pageserver's upward-facing API calls to the storage controller
//! (the timeline-import status upcalls).

use serde::{Deserialize, Serialize};
use utils::generation::Generation;
use utils::id::BranchId;

use crate::models::BranchImportStatus;

#[derive(Serialize, Deserialize)]
pub struct TimelineImportStatusRequest {
    pub branch_id: BranchId,
    pub generation: Generation,
}

#[derive(Serialize, Deserialize)]
pub struct PutTimelineImportStatusRequest {
    pub branch_id: BranchId,
    pub status: BranchImportStatus,
    pub generation: Generation,
}
