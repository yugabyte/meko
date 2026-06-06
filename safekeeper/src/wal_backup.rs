//! Safekeeper YB-mirror connection handle.
//!
//! In upstream Neon this module was an S3 WAL-segment offloader.
//! yb-light durable WAL lives in `yb_storage.wals` (YB Raft, RF=3),
//! so the offloader has nothing to do. Phase 9 deleted the upload
//! task, the partial-segment uploader, the peer-leader election,
//! the segment math, and the S3 WAL reader; task 19 deleted the
//! `GenericRemoteStorage` plumbing that used to back the SK
//! control file (now a row in `safekeeper.timeline_state`).
//!
//! What survives:
//!
//! - `WalBackup` — a thin shell wrapping `Option<Arc<YbWalConn>>`.
//!   Kept (rather than collapsing to the inner type) because the
//!   timeline / manager plumbing is typed against `Arc<WalBackup>`
//!   and the shell makes that rename trivial when the time comes.
//!   No "backup" work happens here despite the name.

use std::sync::Arc;

use anyhow::{Context, Result};

use crate::SafeKeeperConf;
use crate::yb_wal_storage::YbWalConn;

/// Holds the safekeeper's shared YB connection. One instance is
/// constructed at boot and shared across timelines via
/// `Arc<WalBackup>`.
pub struct WalBackup {
    /// Live-WAL YB mirror connection. `None` when `--wal-yb-config`
    /// isn't set (which yb-light shouldn't be running without).
    yb_wal: Option<Arc<YbWalConn>>,
}

impl WalBackup {
    /// Construct from the SK config. `wal_yb_config` is required for
    /// yb-light's normal mode; without it the SK has neither a
    /// place to write WAL nor a place to write its control file.
    pub async fn new(conf: &SafeKeeperConf) -> Result<Self> {
        let yb_wal = match &conf.wal_yb_config {
            Some(yb) => {
                // Same factory the pageserver's YbStore uses — honors
                // sslmode + sslrootcert + password_env consistently.
                // `set_fast_path_upsert = true`: this client serves
                // WAL chunk INSERTs, WAL GC, and (since task 19) the
                // control-file UPSERT — all idempotent /
                // last-writer-wins, so YB's read-before-write check
                // adds nothing.
                let client = ysql_client::YsqlClient::from_yb_config(
                    &yb.conn_config(),
                    "safekeeper:yb_wal_mirror",
                    true,
                )
                .await
                .with_context(|| {
                    format!(
                        "connect WAL mirror YB at {}:{} (sslmode={:?})",
                        yb.host, yb.port, yb.sslmode
                    )
                })?;
                let cache = if conf.wal_cache_size_mib > 0 {
                    Some(Arc::new(std::sync::Mutex::new(
                        crate::wal_cache::WalCache::new(conf.wal_cache_size_mib * 1024 * 1024),
                    )))
                } else {
                    None
                };
                Some(Arc::new(YbWalConn {
                    client: Arc::new(client),
                    table: yb.table_name.clone(),
                    cache,
                }))
            }
            None => None,
        };
        Ok(Self { yb_wal })
    }

    /// Live-WAL YB mirror, if configured. `None` when
    /// `--wal-yb-config` isn't set.
    pub fn yb_wal(&self) -> Option<Arc<YbWalConn>> {
        self.yb_wal.clone()
    }
}
