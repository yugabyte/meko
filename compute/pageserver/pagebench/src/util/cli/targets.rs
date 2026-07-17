use std::sync::Arc;

use pageserver_client::mgmt_api;
use tracing::info;
use utils::id::BranchId;

pub(crate) struct Spec {
    pub(crate) limit_to_first_n_targets: Option<usize>,
    pub(crate) targets: Option<Vec<BranchId>>,
}

pub(crate) async fn discover(
    api_client: &Arc<mgmt_api::Client>,
    spec: Spec,
) -> anyhow::Result<Vec<BranchId>> {
    let mut branches = if let Some(targets) = spec.targets {
        targets
    } else {
        mgmt_api::util::get_pageserver_branches(api_client).await?
    };

    if let Some(limit) = spec.limit_to_first_n_targets {
        branches.sort(); // for determinism
        branches.truncate(limit);
        if branches.len() < limit {
            anyhow::bail!("pageserver has less than limit_to_first_n_targets={limit} branches");
        }
    }

    info!("branches:\n{:?}", branches);
    info!("number of branches:\n{:?}", branches.len());

    Ok(branches)
}
