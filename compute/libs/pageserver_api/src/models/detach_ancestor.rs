use std::collections::HashSet;

use utils::id::BranchId;

#[derive(Debug, Default, PartialEq, serde::Serialize, serde::Deserialize)]
pub struct AncestorDetached {
    pub reparented_branches: HashSet<BranchId>,
}
