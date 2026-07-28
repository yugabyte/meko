//! Utils for dumping full state of the safekeeper.

use std::path::PathBuf;
use std::sync::Arc;

use anyhow::Result;
use chrono::{DateTime, Utc};
use postgres_ffi::XLogSegNo;
use safekeeper_api::models::WalSenderState;
use serde::{Deserialize, Serialize};
use utils::id::BranchId;
use utils::lsn::Lsn;

use crate::state::{TimelineMemState, TimelinePersistentState};
use crate::{GlobalTimelines, SafeKeeperConf, timeline_manager};

/// Various filters that influence the resulting JSON output.
#[derive(Debug, Serialize, Deserialize, Clone)]
pub struct Args {
    /// Dump all available safekeeper state. False by default.
    pub dump_all: bool,

    /// Dump control_file content. Uses value of `dump_all` by default.
    pub dump_control_file: bool,

    /// Dump in-memory state. Uses value of `dump_all` by default.
    pub dump_memory: bool,

    /// Filter timelines by branch_id.
    pub branch_id: Option<BranchId>,
}

/// Response for debug dump request.
#[derive(Debug, Serialize)]
pub struct Response {
    pub start_time: DateTime<Utc>,
    pub finish_time: DateTime<Utc>,
    pub timelines: Vec<TimelineDumpSer>,
    pub timelines_count: usize,
    pub config: Config,
}

pub struct TimelineDumpSer {
    pub tli: Arc<crate::timeline::Timeline>,
    pub args: Args,
    pub runtime: Arc<tokio::runtime::Runtime>,
}

impl std::fmt::Debug for TimelineDumpSer {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("TimelineDumpSer")
            .field("tli", &self.tli.branch_id)
            .field("args", &self.args)
            .finish()
    }
}

impl Serialize for TimelineDumpSer {
    fn serialize<S>(&self, serializer: S) -> std::result::Result<S::Ok, S::Error>
    where
        S: serde::Serializer,
    {
        let dump = self
            .runtime
            .block_on(build_from_tli_dump(&self.tli, &self.args));
        dump.serialize(serializer)
    }
}

async fn build_from_tli_dump(timeline: &Arc<crate::timeline::Timeline>, args: &Args) -> Timeline {
    let control_file = if args.dump_control_file {
        let state = timeline.get_state().await.1;
        Some(state)
    } else {
        None
    };

    let memory = if args.dump_memory {
        Some(timeline.memory_dump().await)
    } else {
        None
    };

    Timeline {
        branch_id: timeline.branch_id,
        control_file,
        memory,
    }
}

/// Safekeeper configuration.
#[derive(Debug, Serialize, Deserialize)]
pub struct Config {
    pub workdir: PathBuf,
    pub listen_pg_addr: String,
    pub listen_http_addr: String,
}

#[derive(Debug, Serialize, Deserialize)]
pub struct Timeline {
    pub branch_id: BranchId,
    pub control_file: Option<TimelinePersistentState>,
    pub memory: Option<Memory>,
}

#[derive(Debug, Serialize, Deserialize)]
pub struct Memory {
    pub is_cancelled: bool,
    pub peers_info_len: usize,
    pub walsenders: Vec<WalSenderState>,
    pub active: bool,
    pub num_computes: u32,
    pub last_removed_segno: XLogSegNo,
    pub mem_state: TimelineMemState,
    pub mgr_status: timeline_manager::Status,

    // WalBookkeeping state.
    pub write_lsn: Lsn,
    pub write_record_lsn: Lsn,
    pub flush_lsn: Lsn,
    pub file_open: bool,
}

/// Build debug dump response, using the provided [`Args`] filters.
pub async fn build(args: Args, global_timelines: Arc<GlobalTimelines>) -> Result<Response> {
    let start_time = Utc::now();
    let timelines_count = global_timelines.timelines_count();
    let config = global_timelines.get_global_config();

    let ptrs_snapshot = if let Some(branch_id) = args.branch_id {
        // If branch_id is specified, we can just get the timeline directly,
        // without taking a snapshot of the whole list.
        if let Ok(tli) = global_timelines.get(branch_id) {
            vec![tli]
        } else {
            vec![]
        }
    } else {
        // Otherwise, take a snapshot of the whole list.
        global_timelines.get_all()
    };

    let mut timelines = Vec::new();
    let runtime = Arc::new(
        tokio::runtime::Builder::new_current_thread()
            .build()
            .unwrap(),
    );
    for tli in ptrs_snapshot {
        let branch_id = tli.branch_id;
        if let Some(filter_branch_id) = args.branch_id {
            if filter_branch_id != branch_id {
                continue;
            }
        }

        timelines.push(TimelineDumpSer {
            tli,
            args: args.clone(),
            runtime: runtime.clone(),
        });
    }

    // Tokio forbids to drop runtime in async context, so this is a stupid way
    // to drop it in non async context.
    tokio::task::spawn_blocking(move || {
        let _r = runtime;
    })
    .await?;

    Ok(Response {
        start_time,
        finish_time: Utc::now(),
        timelines,
        timelines_count,
        config: build_config(config),
    })
}

/// Converts SafeKeeperConf to Config, filtering out the fields that are not
/// supposed to be exposed.
fn build_config(config: Arc<SafeKeeperConf>) -> Config {
    Config {
        workdir: config.workdir.clone().into(),
        listen_pg_addr: config.listen_pg_addr.clone(),
        listen_http_addr: config.listen_http_addr.clone(),
    }
}
