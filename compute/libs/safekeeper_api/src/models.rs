//! Types used in safekeeper http API. Many of them are also reused internally.

use std::net::SocketAddr;

use postgres_ffi_types::TimestampTz;
use postgres_versioninfo::PgVersionId;
use serde::{Deserialize, Serialize};
use tokio::time::Instant;
use utils::id::{BranchId, NodeId};
use utils::lsn::Lsn;
use utils::pageserver_feedback::PageserverFeedback;

use crate::ServerInfo;

#[derive(Debug, Serialize, Deserialize)]
pub struct SafekeeperStatus {}

#[derive(Serialize, Deserialize, Clone)]
pub struct TimelineCreateRequest {
    pub branch_id: BranchId,
    pub pg_version: PgVersionId,
    pub system_id: Option<u64>,
    // By default WAL_SEGMENT_SIZE
    pub wal_seg_size: Option<u32>,
    pub start_lsn: Lsn,
    // Normal creation should omit this field (start_lsn initializes all LSNs).
    // However, we allow specifying custom value higher than start_lsn for
    // manual recovery case, see test_s3_wal_replay.
    pub commit_lsn: Option<Lsn>,
}

/// Things safekeeper should know about timeline state on peers.
/// Used as both model and internally.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct PeerInfo {
    pub sk_id: NodeId,
    /// LSN of the last record.
    pub flush_lsn: Lsn,
    pub commit_lsn: Lsn,
    /// Since which LSN safekeeper has WAL.
    pub local_start_lsn: Lsn,
    /// When info was received. Serde annotations are not very useful but make
    /// the code compile -- we don't rely on this field externally.
    #[serde(skip)]
    #[serde(default = "Instant::now")]
    pub ts: Instant,
    pub pg_connstr: String,
    pub http_connstr: String,
    pub https_connstr: Option<String>,
}

pub type FullTransactionId = u64;

/// Hot standby feedback received from replica
#[derive(Debug, Clone, Copy, Serialize, Deserialize)]
pub struct HotStandbyFeedback {
    pub ts: TimestampTz,
    pub xmin: FullTransactionId,
    pub catalog_xmin: FullTransactionId,
}

pub const INVALID_FULL_TRANSACTION_ID: FullTransactionId = 0;

impl HotStandbyFeedback {
    pub fn empty() -> HotStandbyFeedback {
        HotStandbyFeedback {
            ts: 0,
            xmin: 0,
            catalog_xmin: 0,
        }
    }
}

/// Standby status update
#[derive(Debug, Clone, Copy, Serialize, Deserialize)]
pub struct StandbyReply {
    pub write_lsn: Lsn, // The location of the last WAL byte + 1 received and written to disk in the standby.
    pub flush_lsn: Lsn, // The location of the last WAL byte + 1 flushed to disk in the standby.
    pub apply_lsn: Lsn, // The location of the last WAL byte + 1 applied in the standby.
    pub reply_ts: TimestampTz, // The client's system clock at the time of transmission, as microseconds since midnight on 2000-01-01.
    pub reply_requested: bool,
}

impl StandbyReply {
    pub fn empty() -> Self {
        StandbyReply {
            write_lsn: Lsn::INVALID,
            flush_lsn: Lsn::INVALID,
            apply_lsn: Lsn::INVALID,
            reply_ts: 0,
            reply_requested: false,
        }
    }
}

#[derive(Debug, Clone, Copy, Serialize, Deserialize)]
pub struct StandbyFeedback {
    pub reply: StandbyReply,
    pub hs_feedback: HotStandbyFeedback,
}

impl StandbyFeedback {
    pub fn empty() -> Self {
        StandbyFeedback {
            reply: StandbyReply::empty(),
            hs_feedback: HotStandbyFeedback::empty(),
        }
    }
}

/// Receiver is either pageserver or regular standby, which have different
/// feedbacks.
/// Used as both model and internally.
#[derive(Debug, Clone, Copy, Serialize, Deserialize)]
pub enum ReplicationFeedback {
    Pageserver(PageserverFeedback),
    Standby(StandbyFeedback),
}

/// Uniquely identifies a WAL service connection. Logged in spans for
/// observability.
pub type ConnectionId = u32;

/// Per-WAL-sender state, serialized in admin API responses.
///
/// Upstream Neon had a parallel `Vanilla` variant for the now-removed
/// libpq physical-replication ingest path (see PostgresClientProtocol).
/// After lane N only `Interpreted` remains; kept as a single-variant
/// enum to preserve the JSON shape API consumers expect.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub enum WalSenderState {
    Interpreted(InterpretedWalSenderState),
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct InterpretedWalSenderState {
    pub branch_id: BranchId,
    pub addr: SocketAddr,
    pub conn_id: ConnectionId,
    // postgres application_name
    pub appname: Option<String>,
    pub feedback: ReplicationFeedback,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct WalReceiverState {
    /// None means it is recovery initiated by us (this safekeeper).
    pub conn_id: Option<ConnectionId>,
    pub status: WalReceiverStatus,
}

/// Walreceiver status. Currently only whether it is still connecting or
/// has started receiving the stream, but it is easy to add more if needed.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub enum WalReceiverStatus {
    #[serde(alias = "Voting")]
    Connecting,
    Streaming,
}

/// Info about timeline on safekeeper ready for reporting.
#[derive(Debug, Serialize, Deserialize)]
pub struct TimelineStatus {
    pub branch_id: BranchId,
    pub pg_info: ServerInfo,
    pub flush_lsn: Lsn,
    pub timeline_start_lsn: Lsn,
    pub local_start_lsn: Lsn,
    pub commit_lsn: Lsn,
    pub remote_consistent_lsn: Lsn,
    pub peers: Vec<PeerInfo>,
    pub walsenders: Vec<WalSenderState>,
    pub walreceivers: Vec<WalReceiverState>,
}

#[derive(Clone, Copy, Serialize, Deserialize)]
pub struct TimelineDeleteResult {
    pub dir_existed: bool,
}

pub type TenantDeleteResult = std::collections::HashMap<String, TimelineDeleteResult>;

#[derive(Debug, Clone, Deserialize, Serialize)]
pub struct SafekeeperUtilization {
    pub timeline_count: u64,
}
