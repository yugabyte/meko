use std::collections::HashMap;
use std::sync::Arc;
use std::time::Instant;

use pageserver_api::models::{BranchConfig, BranchConfigRequest};
use utils::id::BranchId;
use utils::lsn::Lsn;

/// Ingest aux files into the pageserver.
#[derive(clap::Parser)]
pub(crate) struct Args {
    #[clap(long, default_value = "http://localhost:9898")]
    mgmt_api_endpoint: String,
    #[clap(long, default_value = "postgres://postgres@localhost:64000")]
    page_service_connstring: String,
    #[clap(long)]
    pageserver_jwt: Option<String>,

    targets: Option<Vec<BranchId>>,
}

pub(crate) fn main(args: Args) -> anyhow::Result<()> {
    let rt = tokio::runtime::Builder::new_multi_thread()
        .enable_all()
        .build()
        .unwrap();

    let main_task = rt.spawn(main_impl(args));
    rt.block_on(main_task).unwrap()
}

async fn main_impl(args: Args) -> anyhow::Result<()> {
    let args: &'static Args = Box::leak(Box::new(args));

    let mgmt_api_client = Arc::new(pageserver_client::mgmt_api::Client::new(
        reqwest::Client::new(), // TODO: support ssl_ca_file for https APIs in pagebench.
        args.mgmt_api_endpoint.clone(),
        args.pageserver_jwt.as_deref(),
    ));

    // discover targets
    let branches: Vec<BranchId> = crate::util::cli::targets::discover(
        &mgmt_api_client,
        crate::util::cli::targets::Spec {
            limit_to_first_n_targets: None,
            targets: {
                if let Some(targets) = &args.targets {
                    if targets.len() != 1 {
                        anyhow::bail!("must specify exactly one target");
                    }
                    Some(targets.clone())
                } else {
                    None
                }
            },
        },
    )
    .await?;

    let branch_id = branches[0];

    println!("operating on branch {branch_id}");

    mgmt_api_client
        .set_branch_config(&BranchConfigRequest {
            branch_id,
            config: BranchConfig::default(),
        })
        .await?;

    for batch in 0..100 {
        let items = (0..100)
            .map(|id| {
                (
                    format!("pg_logical/mappings/{batch:03}.{id:03}"),
                    format!("{id:08}"),
                )
            })
            .collect::<HashMap<_, _>>();
        let file_cnt = items.len();
        mgmt_api_client
            .ingest_aux_files(branch_id, items)
            .await?;
        println!("ingested {file_cnt} files");
    }

    for _ in 0..100 {
        let start = Instant::now();
        let files = mgmt_api_client
            .list_aux_files(branch_id, Lsn(Lsn::MAX.0 - 1))
            .await?;
        println!(
            "{} files found in {}s",
            files.len(),
            start.elapsed().as_secs_f64()
        );
    }

    anyhow::Ok(())
}
