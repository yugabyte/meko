//! Helpers to do common higher-level tasks with the [`Client`].

use std::sync::Arc;

use utils::id::BranchId;

use super::Client;

/// Retrieve a list of all of the pageserver's branches.
pub async fn get_pageserver_branches(api_client: &Arc<Client>) -> anyhow::Result<Vec<BranchId>> {
    let branches = api_client
        .list_branches()
        .await?
        .into_iter()
        .map(|bi| bi.id)
        .collect();
    Ok(branches)
}
