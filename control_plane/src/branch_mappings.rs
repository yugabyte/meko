//! Branch mappings for convenience

use std::collections::HashMap;
use std::fs;
use std::path::Path;

use anyhow::{bail, Context};
use serde::{Deserialize, Serialize};

use utils::id::BranchId;

/// Keep human-readable aliases in memory (and persist them to config XXX), to hide branch hex strings from the user.
#[derive(PartialEq, Eq, Clone, Debug, Default, Serialize, Deserialize)]
#[serde(default, deny_unknown_fields)]
pub struct BranchMappings {
    /// Default branch ID to use with the 'yb_light_local' command line utility, when
    /// --branch_id is not explicitly specified. This comes from the branches.
    pub default_branch_id: Option<BranchId>,

    /// A branch name maps to exactly one branch id (the flat branch model
    /// has no tenant axis, so the old per-tenant `Vec` collapses to a single
    /// `BranchId`).
    pub mappings: HashMap<String, BranchId>,
}

impl BranchMappings {
    pub fn register_branch_mapping(
        &mut self,
        branch_name: String,
        branch_id: BranchId,
    ) -> anyhow::Result<()> {
        match self.mappings.get(&branch_name) {
            Some(existing) if existing == &branch_id => Ok(()),
            Some(existing) => bail!(
                "branch '{branch_name}' is already mapped to branch {existing}, cannot map to another branch {branch_id}"
            ),
            None => {
                self.mappings.insert(branch_name, branch_id);
                Ok(())
            }
        }
    }

    pub fn get_branch_timeline_id(&self, branch_name: &str) -> Option<BranchId> {
        // If it looks like a branch ID, return it as it is
        if let Ok(branch_id) = branch_name.parse::<BranchId>() {
            return Some(branch_id);
        }

        self.mappings.get(branch_name).copied()
    }

    pub fn timeline_name_mappings(&self) -> HashMap<BranchId, String> {
        self.mappings
            .iter()
            .map(|(name, branch_id)| (*branch_id, name.clone()))
            .collect()
    }

    pub fn persist(&self, path: &Path) -> anyhow::Result<()> {
        let content = &toml::to_string_pretty(self)?;
        fs::write(path, content).with_context(|| {
            format!(
                "Failed to write branch information into path '{}'",
                path.display()
            )
        })
    }

    pub fn load(path: &Path) -> anyhow::Result<BranchMappings> {
        let branches_file_contents = fs::read_to_string(path)?;
        Ok(toml::from_str(branches_file_contents.as_str())?)
    }
}
