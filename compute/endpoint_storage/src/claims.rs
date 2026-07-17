use serde::{Deserialize, Serialize};
use std::fmt::Display;
use utils::id::{BranchId, EndpointId};

/// Claims to add, remove, or retrieve endpoint data. Used by compute_ctl
#[derive(Deserialize, Serialize, PartialEq)]
pub struct EndpointStorageClaims {
    pub branch_id: BranchId,
    pub endpoint_id: EndpointId,
    pub exp: u64,
}

/// Claims to remove branch or endpoint data. Used by control plane
#[derive(Deserialize, Serialize, PartialEq)]
pub struct DeletePrefixClaims {
    pub branch_id: BranchId,
    /// None when the whole branch is deleted
    pub endpoint_id: Option<EndpointId>,
    pub exp: u64,
}

impl Display for EndpointStorageClaims {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(
            f,
            "EndpointClaims(branch_id={} endpoint_id={} exp={})",
            self.branch_id, self.endpoint_id, self.exp
        )
    }
}

impl Display for DeletePrefixClaims {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(
            f,
            "DeletePrefixClaims(branch_id={} endpoint_id={}, exp={})",
            self.branch_id,
            self.endpoint_id
                .as_ref()
                .map(ToString::to_string)
                .unwrap_or("".to_string()),
            self.exp
        )
    }
}
