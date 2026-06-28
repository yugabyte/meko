//! Types in this file are for pageserver's upward-facing API calls to the storage controller,
//! required for acquiring and validating branch generation numbers.
//!
//! See docs/rfcs/025-generation-numbers.md

use serde::{Deserialize, Serialize};
use utils::generation::Generation;
use utils::id::{BranchId, NodeId};

use crate::controller_api::NodeRegisterRequest;
use crate::models::BranchImportStatus;

/// Upcall message sent by the pageserver to the configured `control_plane_api` on
/// startup.
#[derive(Serialize, Deserialize)]
pub struct ReAttachRequest {
    pub node_id: NodeId,

    /// Optional inline self-registration: this is useful with the storage controller,
    /// if the node already has a node_id set.
    #[serde(skip_serializing_if = "Option::is_none", default)]
    pub register: Option<NodeRegisterRequest>,
}

#[derive(Serialize, Deserialize, Debug)]
pub struct ReAttachResponseBranch {
    pub id: BranchId,
    /// `Some(g)` → re-attach in generation `g`; `None` → not attached.
    pub r#gen: Option<u32>,
}
#[derive(Serialize, Deserialize)]
pub struct ReAttachResponse {
    pub branches: Vec<ReAttachResponseBranch>,
}

#[derive(Serialize, Deserialize)]
pub struct ValidateRequestBranch {
    pub id: BranchId,
    pub r#gen: u32,
}

#[derive(Serialize, Deserialize)]
pub struct ValidateRequest {
    pub branches: Vec<ValidateRequestBranch>,
}

#[derive(Serialize, Deserialize)]
pub struct ValidateResponse {
    pub branches: Vec<ValidateResponseBranch>,
}

#[derive(Serialize, Deserialize)]
pub struct ValidateResponseBranch {
    pub id: BranchId,
    pub valid: bool,
}

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
