//! Types in this file are for pageserver's upward-facing API calls to the storage controller,
//! required for acquiring and validating tenant generation numbers.
//!
//! See docs/rfcs/025-generation-numbers.md

use serde::{Deserialize, Serialize};
use utils::generation::Generation;
use utils::id::{NodeId, TimelineId};

use utils::id::{ShardStripeSize, TenantId};

use crate::controller_api::NodeRegisterRequest;
use crate::models::ShardImportStatus;

/// Upcall message sent by the pageserver to the configured `control_plane_api` on
/// startup.
#[derive(Serialize, Deserialize)]
pub struct ReAttachRequest {
    pub node_id: NodeId,

    /// Optional inline self-registration: this is useful with the storage controller,
    /// if the node already has a node_id set.
    #[serde(skip_serializing_if = "Option::is_none", default)]
    pub register: Option<NodeRegisterRequest>,

    /// Optional flag to indicate whether the node is starting with an empty local disk.
    /// Will be set to true if the node couldn't find any local tenant data on startup, could be
    /// due to the node starting for the first time or due to a local SSD failure/disk wipe event.
    /// The flag may be used by the storage controller to update its observed state of the world
    /// to make sure that it sends explicit location_config calls to the node following the
    /// re-attach request.
    pub empty_local_disk: Option<bool>,
}

#[derive(Serialize, Deserialize, Debug)]
pub struct ReAttachResponseTenant {
    pub id: TenantId,
    /// `Some(g)` → re-attach in generation `g`; `None` → not attached.
    pub r#gen: Option<u32>,
    pub stripe_size: ShardStripeSize,
}
#[derive(Serialize, Deserialize)]
pub struct ReAttachResponse {
    pub tenants: Vec<ReAttachResponseTenant>,
}

#[derive(Serialize, Deserialize)]
pub struct ValidateRequestTenant {
    pub id: TenantId,
    pub r#gen: u32,
}

#[derive(Serialize, Deserialize)]
pub struct ValidateRequest {
    pub tenants: Vec<ValidateRequestTenant>,
}

#[derive(Serialize, Deserialize)]
pub struct ValidateResponse {
    pub tenants: Vec<ValidateResponseTenant>,
}

#[derive(Serialize, Deserialize)]
pub struct ValidateResponseTenant {
    pub id: TenantId,
    pub valid: bool,
}

#[derive(Serialize, Deserialize)]
pub struct TimelineImportStatusRequest {
    pub tenant_shard_id: TenantId,
    pub timeline_id: TimelineId,
    pub generation: Generation,
}

#[derive(Serialize, Deserialize)]
pub struct PutTimelineImportStatusRequest {
    pub tenant_shard_id: TenantId,
    pub timeline_id: TimelineId,
    pub status: ShardImportStatus,
    pub generation: Generation,
}
