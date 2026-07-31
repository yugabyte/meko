#![deny(unsafe_code)]
#![deny(clippy::undocumented_unsafe_blocks)]
use const_format::formatcp;
use pq_proto::SystemId;
use serde::{Deserialize, Serialize};

/// On-disk safekeeper configuration (`safekeeper.toml`).
pub mod config;
/// Public API types
pub mod models;

pub use postgres_versioninfo::{PgMajorVersion, PgVersionId};

/// Information about Postgres. Safekeeper gets it once and then verifies all
/// further connections from computes match. Note: it is a part of sk control
/// file.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct ServerInfo {
    /// Postgres server version
    pub pg_version: PgVersionId,
    pub system_id: SystemId,
    pub wal_seg_size: u32,
}

pub const DEFAULT_PG_LISTEN_PORT: u16 = 5454;
pub const DEFAULT_PG_LISTEN_ADDR: &str = formatcp!("127.0.0.1:{DEFAULT_PG_LISTEN_PORT}");

pub const DEFAULT_HTTP_LISTEN_PORT: u16 = 7676;
pub const DEFAULT_HTTP_LISTEN_ADDR: &str = formatcp!("127.0.0.1:{DEFAULT_HTTP_LISTEN_PORT}");
