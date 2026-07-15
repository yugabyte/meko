use std::collections::HashMap;
use std::path::PathBuf;
use std::str::FromStr;
use std::time::Duration;

use clap::{Parser, Subcommand};
use pageserver_api::controller_api::{
    BranchCreateRequest, BranchDescribeResponse, BranchMigrateRequest, BranchMigrateResponse,
    BranchPolicyRequest, MigrationConfig, NodeAvailabilityWrapper, NodeConfigureRequest,
    NodeDescribeResponse, NodeRegisterRequest, NodeShardResponse, PlacementPolicy,
    ShardSchedulingPolicy,
};
use pageserver_api::models::{BranchConfig, BranchConfigPatchRequest, BranchConfigRequest};
use pageserver_client::mgmt_api::{self};
use reqwest::{Certificate, Method, StatusCode, Url};
use storage_controller_client::control_api::Client;
use utils::id::{BranchId, NodeId};

#[derive(Subcommand, Debug)]
enum Command {
    /// Register a pageserver with the storage controller.  This shouldn't usually be necessary,
    /// since pageservers auto-register when they start up
    NodeRegister {
        #[arg(long)]
        node_id: NodeId,

        #[arg(long)]
        listen_pg_addr: String,
        #[arg(long)]
        listen_pg_port: u16,
        #[arg(long)]
        listen_grpc_addr: Option<String>,
        #[arg(long)]
        listen_grpc_port: Option<u16>,

        #[arg(long)]
        listen_http_addr: String,
        #[arg(long)]
        listen_http_port: u16,
        #[arg(long)]
        listen_https_port: Option<u16>,
    },

    /// Modify a node's configuration in the storage controller
    NodeConfigure {
        #[arg(long)]
        node_id: NodeId,

        /// Availability is usually auto-detected based on heartbeats.  Set 'offline' here to
        /// manually mark a node offline
        #[arg(long)]
        availability: Option<NodeAvailabilityArg>,
    },
    /// Exists for backup usage and will be removed in future.
    /// Use [`Command::NodeStartDelete`] instead, if possible.
    NodeDelete {
        #[arg(long)]
        node_id: NodeId,
    },
    /// Start deletion of the specified pageserver.
    NodeStartDelete {
        #[arg(long)]
        node_id: NodeId,
        /// When `force` is true, skip waiting for branches to prewarm during migration.
        /// This can significantly speed up node deletion since prewarming all branches
        /// can take considerable time, but may result in slower initial access to
        /// migrated branches until they warm up naturally.
        #[arg(long)]
        force: bool,
    },
    /// Cancel deletion of the specified pageserver and wait for `timeout`
    /// for the operation to be canceled. May be retried.
    NodeCancelDelete {
        #[arg(long)]
        node_id: NodeId,
        #[arg(long)]
        timeout: humantime::Duration,
    },
    /// Delete a tombstone of node from the storage controller.
    /// This is used when we want to allow the node to be re-registered.
    NodeDeleteTombstone {
        #[arg(long)]
        node_id: NodeId,
    },
    /// Modify a branch's policies in the storage controller
    BranchPolicy {
        #[arg(long)]
        branch_id: BranchId,
        /// Placement policy controls whether a branch is `detached`, has only a secondary location (`secondary`),
        /// or is in the normal attached state with N secondary locations (`attached:N`)
        #[arg(long)]
        placement: Option<PlacementPolicyArg>,
        /// Scheduling policy enables pausing the controller's scheduling activity involving this branch.  `active` is normal,
        /// `essential` disables optimization scheduling changes, `pause` disables all scheduling changes, and `stop` prevents
        /// all reconciliation activity including for scheduling changes already made.  `pause` and `stop` can make a branch
        /// unavailable, and are only for use in emergencies.
        #[arg(long)]
        scheduling: Option<ShardSchedulingPolicyArg>,
    },
    /// List nodes known to the storage controller
    Nodes {},
    /// List soft deleted nodes known to the storage controller
    NodeTombstones {},
    /// List branches known to the storage controller
    Branches {
        /// If this field is set, it will list the branches on a specific node
        node_id: Option<NodeId>,
    },
    /// Create a new branch in the storage controller, and by extension on pageservers.
    BranchCreate {
        #[arg(long)]
        branch_id: BranchId,
    },
    /// Delete a branch in the storage controller, and by extension on pageservers.
    BranchDelete {
        #[arg(long)]
        branch_id: BranchId,
    },
    /// Migrate the attached location for a branch to a specific pageserver.
    BranchMigrate {
        #[arg(long)]
        branch_id: BranchId,
        #[arg(long)]
        node: NodeId,
        #[arg(long, default_value_t = true, action = clap::ArgAction::Set)]
        prewarm: bool,
        #[arg(long, default_value_t = false, action = clap::ArgAction::Set)]
        override_scheduler: bool,
    },
    /// Watch the location of a branch evolve, e.g. while expecting it to migrate
    BranchWatch {
        #[arg(long)]
        branch_id: BranchId,
    },
    /// Migrate the secondary location for a branch to a specific pageserver.
    BranchMigrateSecondary {
        #[arg(long)]
        branch_id: BranchId,
        #[arg(long)]
        node: NodeId,
    },
    /// Cancel any ongoing reconciliation for this branch
    BranchCancelReconcile {
        #[arg(long)]
        branch_id: BranchId,
    },
    /// Set the pageserver branch configuration of a branch: this is the configuration structure
    /// that is passed through to pageservers, and does not affect storage controller behavior.
    /// Any previous branch configs are overwritten.
    SetBranchConfig {
        #[arg(long)]
        branch_id: BranchId,
        #[arg(long)]
        config: String,
    },
    /// Patch the pageserver branch configuration of a branch. Any fields with null values in the
    /// provided JSON are unset from the branch config and all fields with non-null values are set.
    /// Unspecified fields are not changed.
    PatchBranchConfig {
        #[arg(long)]
        branch_id: BranchId,
        #[arg(long)]
        config: String,
    },
    /// Print details about a particular branch, including all its shards' states.
    BranchDescribe {
        #[arg(long)]
        branch_id: BranchId,
    },
    /// Uncleanly drop a branch from the storage controller: this doesn't delete anything from pageservers. Appropriate
    /// if you e.g. used `branch-warmup` by mistake on a branch ID that doesn't really exist, or is in some other region.
    BranchDrop {
        #[arg(long)]
        branch_id: BranchId,
        #[arg(long)]
        unclean: bool,
    },
    NodeDrop {
        #[arg(long)]
        node_id: NodeId,
        #[arg(long)]
        unclean: bool,
    },
    /// Downloads any missing heatmap layers for all shard for a given branch
    DownloadHeatmapLayers {
        /// Branch ID. The operation is performed on all shards of the branch.
        #[arg(long)]
        branch_id: BranchId,
        /// Optional: Maximum download concurrency (default is 16)
        #[arg(long)]
        concurrency: Option<usize>,
    },
}

#[derive(Parser)]
#[command(
    author,
    version,
    about,
    long_about = "CLI for Storage Controller Support/Debug"
)]
#[command(arg_required_else_help(true))]
struct Cli {
    #[arg(long)]
    /// URL to storage controller.  e.g. http://127.0.0.1:1234 when using `yb_light_local`
    api: Url,

    #[arg(long)]
    /// JWT token for authenticating with storage controller.  Depending on the API used, this
    /// should have either `pageserverapi` or `admin` scopes: for convenience, you should mint
    /// a token with both scopes to use with this tool.
    jwt: Option<String>,

    #[arg(long)]
    /// Trusted root CA certificates to use in https APIs.
    ssl_ca_file: Option<PathBuf>,

    #[command(subcommand)]
    command: Command,
}

#[derive(Debug, Clone)]
struct PlacementPolicyArg(PlacementPolicy);

impl FromStr for PlacementPolicyArg {
    type Err = anyhow::Error;

    fn from_str(s: &str) -> Result<Self, Self::Err> {
        match s {
            "detached" => Ok(Self(PlacementPolicy::Detached)),
            "secondary" => Ok(Self(PlacementPolicy::Secondary)),
            _ if s.starts_with("attached:") => {
                let mut splitter = s.split(':');
                let _prefix = splitter.next().unwrap();
                match splitter.next().and_then(|s| s.parse::<usize>().ok()) {
                    Some(n) => Ok(Self(PlacementPolicy::Attached(n))),
                    None => Err(anyhow::anyhow!(
                        "Invalid format '{s}', a valid example is 'attached:1'"
                    )),
                }
            }
            _ => Err(anyhow::anyhow!(
                "Unknown placement policy '{s}', try detached,secondary,attached:<n>"
            )),
        }
    }
}

#[derive(Debug, Clone)]
struct ShardSchedulingPolicyArg(ShardSchedulingPolicy);

impl FromStr for ShardSchedulingPolicyArg {
    type Err = anyhow::Error;

    fn from_str(s: &str) -> Result<Self, Self::Err> {
        match s {
            "active" => Ok(Self(ShardSchedulingPolicy::Active)),
            "essential" => Ok(Self(ShardSchedulingPolicy::Essential)),
            "pause" => Ok(Self(ShardSchedulingPolicy::Pause)),
            "stop" => Ok(Self(ShardSchedulingPolicy::Stop)),
            _ => Err(anyhow::anyhow!(
                "Unknown scheduling policy '{s}', try active,essential,pause,stop"
            )),
        }
    }
}

#[derive(Debug, Clone)]
struct NodeAvailabilityArg(NodeAvailabilityWrapper);

impl FromStr for NodeAvailabilityArg {
    type Err = anyhow::Error;

    fn from_str(s: &str) -> Result<Self, Self::Err> {
        match s {
            "active" => Ok(Self(NodeAvailabilityWrapper::Active)),
            "offline" => Ok(Self(NodeAvailabilityWrapper::Offline)),
            _ => Err(anyhow::anyhow!("Unknown availability state '{s}'")),
        }
    }
}

#[tokio::main]
async fn main() -> anyhow::Result<()> {
    let cli = Cli::parse();

    let ssl_ca_certs = match &cli.ssl_ca_file {
        Some(ssl_ca_file) => {
            let buf = tokio::fs::read(ssl_ca_file).await?;
            Certificate::from_pem_bundle(&buf)?
        }
        None => Vec::new(),
    };

    let mut http_client = reqwest::Client::builder();
    for ssl_ca_cert in ssl_ca_certs {
        http_client = http_client.add_root_certificate(ssl_ca_cert);
    }
    let http_client = http_client.build()?;

    let storcon_client = Client::new(http_client.clone(), cli.api.clone(), cli.jwt.clone());

    let mut trimmed = cli.api.to_string();
    trimmed.pop();
    let vps_client = mgmt_api::Client::new(http_client.clone(), trimmed, cli.jwt.as_deref());

    match cli.command {
        Command::NodeRegister {
            node_id,
            listen_pg_addr,
            listen_pg_port,
            listen_grpc_addr,
            listen_grpc_port,
            listen_http_addr,
            listen_http_port,
            listen_https_port,
        } => {
            storcon_client
                .dispatch::<_, ()>(
                    Method::POST,
                    "control/v1/node".to_string(),
                    Some(NodeRegisterRequest {
                        node_id,
                        listen_pg_addr,
                        listen_pg_port,
                        listen_grpc_addr,
                        listen_grpc_port,
                        listen_http_addr,
                        listen_http_port,
                        listen_https_port,
                        node_ip_addr: None,
                    }),
                )
                .await?;
        }
        Command::BranchCreate { branch_id } => {
            storcon_client
                .dispatch::<_, ()>(
                    Method::POST,
                    "v1/branch".to_string(),
                    Some(BranchCreateRequest {
                        new_branch_id: branch_id,
                        generation: None,
                        placement_policy: Some(PlacementPolicy::Attached(1)),
                        config: BranchConfig::default(),
                    }),
                )
                .await?;
        }
        Command::BranchDelete { branch_id } => {
            let status = vps_client.branch_delete(branch_id).await?;
            tracing::info!("Delete status: {}", status);
        }
        Command::Nodes {} => {
            let mut resp = storcon_client
                .dispatch::<(), Vec<NodeDescribeResponse>>(
                    Method::GET,
                    "control/v1/node".to_string(),
                    None,
                )
                .await?;

            resp.sort_by(|a, b| a.listen_http_addr.cmp(&b.listen_http_addr));

            let mut table = comfy_table::Table::new();
            table.set_header(["Id", "Hostname", "Availability"]);
            for node in resp {
                table.add_row([
                    format!("{}", node.id),
                    node.listen_http_addr,
                    format!("{:?}", node.availability),
                ]);
            }
            println!("{table}");
        }
        Command::NodeConfigure {
            node_id,
            availability,
        } => {
            let req = NodeConfigureRequest {
                node_id,
                availability: availability.map(|a| a.0),
            };
            storcon_client
                .dispatch::<_, ()>(
                    Method::PUT,
                    format!("control/v1/node/{node_id}/config"),
                    Some(req),
                )
                .await?;
        }
        Command::Branches {
            node_id: Some(node_id),
        } => {
            let describe_response = storcon_client
                .dispatch::<(), NodeShardResponse>(
                    Method::GET,
                    format!("control/v1/node/{node_id}/shards"),
                    None,
                )
                .await?;
            let shards = describe_response.shards;
            let mut table = comfy_table::Table::new();
            table.set_header([
                "Branch",
                "Intended Primary/Secondary",
                "Observed Primary/Secondary",
            ]);
            for shard in shards {
                table.add_row([
                    format!("{}", shard.branch_id),
                    match shard.is_intended_secondary {
                        None => "".to_string(),
                        Some(true) => "Secondary".to_string(),
                        Some(false) => "Primary".to_string(),
                    },
                    match shard.is_observed_secondary {
                        None => "".to_string(),
                        Some(true) => "Secondary".to_string(),
                        Some(false) => "Primary".to_string(),
                    },
                ]);
            }
            println!("{table}");
        }
        Command::Branches { node_id: None } => {
            // Set up output formatting
            let mut table = comfy_table::Table::new();
            table.set_header(["BranchId", "Placement", "Scheduling"]);

            // Pagination loop over listing API
            let mut start_after = None;
            const LIMIT: usize = 1000;
            loop {
                let path = match start_after {
                    None => format!("control/v1/branch?limit={LIMIT}"),
                    Some(start_after) => {
                        format!("control/v1/branch?limit={LIMIT}&start_after={start_after}")
                    }
                };

                let resp = storcon_client
                    .dispatch::<(), Vec<BranchDescribeResponse>>(Method::GET, path, None)
                    .await?;

                if resp.is_empty() {
                    // End of data reached
                    break;
                }

                // Give some visual feedback while we're building up the table (comfy_table doesn't have
                // streaming output)
                if resp.len() >= LIMIT {
                    eprint!(".");
                }

                start_after = Some(resp.last().unwrap().branch_id);

                for branch in resp {
                    let shard_zero = branch.shards.into_iter().next().unwrap();
                    table.add_row([
                        format!("{}", branch.branch_id),
                        format!("{:?}", branch.policy),
                        format!("{:?}", shard_zero.scheduling_policy),
                    ]);
                }
            }

            // Terminate progress dots
            if table.row_count() > LIMIT {
                eprint!("");
            }

            println!("{table}");
        }
        Command::BranchPolicy {
            branch_id,
            placement,
            scheduling,
        } => {
            let req = BranchPolicyRequest {
                scheduling: scheduling.map(|s| s.0),
                placement: placement.map(|p| p.0),
            };
            storcon_client
                .dispatch::<_, ()>(
                    Method::PUT,
                    format!("control/v1/branch/{branch_id}/policy"),
                    Some(req),
                )
                .await?;
        }
        Command::BranchMigrate {
            branch_id,
            node,
            prewarm,
            override_scheduler,
        } => {
            let migration_config = MigrationConfig {
                prewarm,
                override_scheduler,
                ..Default::default()
            };

            let req = BranchMigrateRequest {
                node_id: node,
                origin_node_id: None,
                migration_config,
            };

            match storcon_client
                .dispatch::<BranchMigrateRequest, BranchMigrateResponse>(
                    Method::PUT,
                    format!("control/v1/branch/{branch_id}/migrate"),
                    Some(req),
                )
                .await
            {
                Err(mgmt_api::Error::ApiError(StatusCode::PRECONDITION_FAILED, msg)) => {
                    anyhow::bail!(
                        "Migration to {node} rejected, may require `--force` ({}) ",
                        msg
                    );
                }
                Err(e) => return Err(e.into()),
                Ok(_) => {}
            }

            watch_branch(storcon_client, branch_id, Some(node)).await?;
        }
        Command::BranchWatch { branch_id } => {
            watch_branch(storcon_client, branch_id, None).await?;
        }
        Command::BranchMigrateSecondary { branch_id, node } => {
            let req = BranchMigrateRequest {
                node_id: node,
                origin_node_id: None,
                migration_config: MigrationConfig::default(),
            };

            storcon_client
                .dispatch::<BranchMigrateRequest, BranchMigrateResponse>(
                    Method::PUT,
                    format!("control/v1/branch/{branch_id}/migrate_secondary"),
                    Some(req),
                )
                .await?;
        }
        Command::BranchCancelReconcile { branch_id } => {
            storcon_client
                .dispatch::<(), ()>(
                    Method::PUT,
                    format!("control/v1/branch/{branch_id}/cancel_reconcile"),
                    None,
                )
                .await?;
        }
        Command::SetBranchConfig { branch_id, config } => {
            let branch_conf = serde_json::from_str(&config)?;

            vps_client
                .set_branch_config(&BranchConfigRequest {
                    branch_id,
                    config: branch_conf,
                })
                .await?;
        }
        Command::PatchBranchConfig { branch_id, config } => {
            let branch_conf = serde_json::from_str(&config)?;

            vps_client
                .patch_branch_config(&BranchConfigPatchRequest {
                    branch_id,
                    config: branch_conf,
                })
                .await?;
        }
        Command::BranchDescribe { branch_id } => {
            let BranchDescribeResponse {
                branch_id,
                shards,
                policy,
                config,
            } = storcon_client
                .dispatch::<(), BranchDescribeResponse>(
                    Method::GET,
                    format!("control/v1/branch/{branch_id}"),
                    None,
                )
                .await?;

            let nodes = storcon_client
                .dispatch::<(), Vec<NodeDescribeResponse>>(
                    Method::GET,
                    "control/v1/node".to_string(),
                    None,
                )
                .await?;
            let nodes = nodes
                .into_iter()
                .map(|n| (n.id, n))
                .collect::<HashMap<_, _>>();

            println!("Branch {branch_id}");
            let mut table = comfy_table::Table::new();
            table.add_row(["Policy", &format!("{policy:?}")]);
            table.add_row(["Config", &serde_json::to_string_pretty(&config).unwrap()]);
            println!("{table}");
            println!("Shards:");
            let mut table = comfy_table::Table::new();
            table.set_header(["Branch", "Attached", "Secondary", "Last error", "status"]);
            for shard in shards {
                let secondary = shard
                    .node_secondary
                    .iter()
                    .map(|n| format!("{n}"))
                    .collect::<Vec<_>>()
                    .join(",");

                let mut status_parts = Vec::new();
                if shard.is_reconciling {
                    status_parts.push("reconciling");
                }

                if shard.is_pending_compute_notification {
                    status_parts.push("pending_compute");
                }

                if shard.is_splitting {
                    status_parts.push("splitting");
                }
                let status = status_parts.join(",");

                let attached_node = shard
                    .node_attached
                    .as_ref()
                    .map(|id| nodes.get(id).expect("Shard references nonexistent node"));

                table.add_row([
                    format!("{}", shard.branch_id),
                    attached_node
                        .map(|n| format!("{} ({})", n.listen_http_addr, n.id))
                        .unwrap_or(String::new()),
                    secondary,
                    shard.last_error,
                    status,
                ]);
            }
            println!("{table}");
        }
        Command::BranchDrop { branch_id, unclean } => {
            if !unclean {
                anyhow::bail!(
                    "This command is not a branch deletion, and uncleanly drops all controller state for the branch.  If you know what you're doing, add `--unclean` to proceed."
                )
            }
            storcon_client
                .dispatch::<(), ()>(
                    Method::POST,
                    format!("debug/v1/branch/{branch_id}/drop"),
                    None,
                )
                .await?;
        }
        Command::NodeDrop { node_id, unclean } => {
            if !unclean {
                anyhow::bail!(
                    "This command is not a clean node decommission, and uncleanly drops all controller state for the node, without checking if any branches still refer to it.  If you know what you're doing, add `--unclean` to proceed."
                )
            }
            storcon_client
                .dispatch::<(), ()>(Method::POST, format!("debug/v1/node/{node_id}/drop"), None)
                .await?;
        }
        Command::NodeDelete { node_id } => {
            eprintln!("Warning: This command is obsolete and will be removed in a future version");
            eprintln!("Use `NodeStartDelete` instead, if possible");
            storcon_client
                .dispatch::<(), ()>(Method::DELETE, format!("control/v1/node/{node_id}"), None)
                .await?;
        }
        Command::NodeStartDelete { node_id, force } => {
            let query = if force {
                format!("control/v1/node/{node_id}/delete?force=true")
            } else {
                format!("control/v1/node/{node_id}/delete")
            };
            storcon_client
                .dispatch::<(), ()>(Method::PUT, query, None)
                .await?;
            println!("Delete started for {node_id}");
        }
        Command::NodeCancelDelete {
            node_id,
            timeout: _,
        } => {
            storcon_client
                .dispatch::<(), ()>(
                    Method::DELETE,
                    format!("control/v1/node/{node_id}/delete"),
                    None,
                )
                .await?;

            println!("Delete was cancelled for node {node_id}.");
        }
        Command::NodeDeleteTombstone { node_id } => {
            storcon_client
                .dispatch::<(), ()>(
                    Method::DELETE,
                    format!("debug/v1/tombstone/{node_id}"),
                    None,
                )
                .await?;
        }
        Command::NodeTombstones {} => {
            let mut resp = storcon_client
                .dispatch::<(), Vec<NodeDescribeResponse>>(
                    Method::GET,
                    "debug/v1/tombstone".to_string(),
                    None,
                )
                .await?;

            resp.sort_by(|a, b| a.listen_http_addr.cmp(&b.listen_http_addr));

            let mut table = comfy_table::Table::new();
            table.set_header(["Id", "Hostname", "Availability"]);
            for node in resp {
                table.add_row([
                    format!("{}", node.id),
                    node.listen_http_addr,
                    format!("{:?}", node.availability),
                ]);
            }
            println!("{table}");
        }
        Command::DownloadHeatmapLayers {
            branch_id,
            concurrency,
        } => {
            let mut path = format!("v1/branch/{branch_id}/download_heatmap_layers",);

            if let Some(c) = concurrency {
                path = format!("{path}?concurrency={c}");
            }

            storcon_client
                .dispatch::<(), ()>(Method::POST, path, None)
                .await?;
        }
    }

    Ok(())
}

static WATCH_INTERVAL: Duration = Duration::from_secs(5);

async fn watch_branch(
    storcon_client: Client,
    branch_id: BranchId,
    until_migrated_to: Option<NodeId>,
) -> anyhow::Result<()> {
    if let Some(until_migrated_to) = until_migrated_to {
        println!("Waiting for branch {branch_id} to be migrated to node {until_migrated_to}");
    }

    loop {
        let desc = storcon_client
            .dispatch::<(), BranchDescribeResponse>(
                Method::GET,
                format!("control/v1/branch/{branch_id}"),
                None,
            )
            .await?;

        // Output the current state of the branch
        let shard = desc
            .shards
            .iter()
            .find(|s| s.branch_id == branch_id)
            .ok_or(anyhow::anyhow!("Branch not found"))?;
        let summary = format!(
            "attached: {} secondary: {} {}",
            shard
                .node_attached
                .map(|n| format!("{n}"))
                .unwrap_or("none".to_string()),
            shard
                .node_secondary
                .iter()
                .map(|n| n.to_string())
                .collect::<Vec<_>>()
                .join(","),
            if shard.is_reconciling {
                "(reconciler active)"
            } else {
                "(reconciler idle)"
            }
        );
        println!("{summary}");

        // Maybe drop out if we finished migration
        if let Some(until_migrated_to) = until_migrated_to {
            if shard.node_attached == Some(until_migrated_to) && !shard.is_reconciling {
                println!("Branch {branch_id} is now on node {until_migrated_to}");
                break;
            }
        }

        tokio::time::sleep(WATCH_INTERVAL).await;
    }
    Ok(())
}
