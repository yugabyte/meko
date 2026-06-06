//! Safekeeper remote-storage plumbing.
//!
//! In upstream Neon this module was an S3 WAL-segment offloader: a
//! per-timeline background task that watched `flush_lsn`, picked
//! completed 16 MiB segments off local disk, and uploaded them to
//! S3 / GCS / Azure for durability beyond the SK quorum. In yb-light
//! WAL is durable as soon as it lands in `yb_storage.wals` (YB Raft
//! replication, RF=3), so the S3 offloader has nothing to do.
//!
//! What survives:
//!
//! - `WalBackup` — a small handle that holds the
//!   [`GenericRemoteStorage`] used by the control-file storage layer
//!   plus the YB connection used by the live-WAL mirror. Despite the
//!   name (kept to minimise churn against upstream-Neon source maps),
//!   it does no "backup" work — it's effectively a typed bag of
//!   remote-side handles the SK shares across timelines.
//! - [`delete_timeline`] — recursively delete a timeline's remote
//!   storage objects (control file + any legacy WAL blobs). Called
//!   from `Timeline`'s remote-deletion path.
//! - [`remote_timeline_path`] — `<tenant>/<timeline>` path builder
//!   shared by control-file storage and deletion.
//!
//! Everything else — the upload task (`WalBackupTask`, `update_task`,
//! `backup_task_main`), the segment math (`Segment`, `get_segments`),
//! the peer-leader election (`hadron_determine_offloader`,
//! `determine_offloader`), the partial-segment helpers
//! (`backup_partial_segment`, `copy_partial_segment`), the S3 WAL
//! reader (`read_object`), and `copy_s3_segments` — was Wave 1 dead
//! code and is gone.

use std::num::NonZeroU32;
use std::sync::Arc;

use anyhow::{Context, Result};
use camino::Utf8Path;
use remote_storage::{GenericRemoteStorage, ListingMode, RemotePath};
use tokio_util::sync::CancellationToken;
use tracing::info;
use utils::backoff;
use utils::id::TenantTimelineId;
use utils::pausable_failpoint;

use crate::SafeKeeperConf;
use crate::yb_wal_storage::YbWalConn;

/// Handle to the safekeeper's remote-storage backend (control file
/// blobs) plus the live-WAL YB mirror connection. One instance is
/// constructed at boot and shared across timelines.
pub struct WalBackup {
    /// `GenericRemoteStorage` used by the per-timeline control-file
    /// blob path (see `control_file::ControlFileStorage`). Will go
    /// away once the SK control file moves into YB columns (task 19).
    storage: Option<Arc<GenericRemoteStorage>>,
    /// Live-WAL YB mirror connection. `None` when `--wal-yb-config`
    /// isn't set (which yb-light shouldn't be running without).
    yb_wal: Option<Arc<YbWalConn>>,
}

impl WalBackup {
    /// Construct from the SK config. Both backends are optional:
    ///
    /// - `remote_storage` → `Option<GenericRemoteStorage>` used by the
    ///   control-file storage. Required for any timeline that
    ///   persists its control file remotely (i.e. all of them today;
    ///   task 19 moves the control file into YB columns and removes
    ///   this dependency).
    /// - `wal_yb_config` → `Option<YsqlClient>` for the live-WAL
    ///   mirror. Required in yb-light's normal mode (the SK writes
    ///   each WAL chunk into `yb_storage.wals` as it arrives).
    pub async fn new(conf: &SafeKeeperConf) -> Result<Self> {
        let yb_wal = match &conf.wal_yb_config {
            Some(yb) => {
                // Same factory the pageserver's YbStore uses — honors
                // sslmode + sslrootcert + password_env consistently.
                // `set_fast_path_upsert = true`: this client serves
                // WAL chunk INSERTs (`yb_wal_storage::insert_wal`)
                // and WAL GC (`truncate_wal`), both of which benefit
                // from skipping YB's read-before-write check.
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

        let storage = match conf.remote_storage.as_ref() {
            Some(config) => Some(Arc::new(GenericRemoteStorage::from_config(config).await?)),
            None => None,
        };

        Ok(Self { storage, yb_wal })
    }

    pub fn get_storage(&self) -> Option<Arc<GenericRemoteStorage>> {
        self.storage.clone()
    }

    /// Live-WAL YB mirror, if configured. `None` when
    /// `--wal-yb-config` isn't set.
    pub fn yb_wal(&self) -> Option<Arc<YbWalConn>> {
        self.yb_wal.clone()
    }
}

/// Recursively delete every remote-storage object under
/// `<tenant>/<timeline>/`. Called from `Timeline`'s remote-deletion
/// path when a timeline is deleted.
pub async fn delete_timeline(
    storage: &GenericRemoteStorage,
    ttid: &TenantTimelineId,
) -> Result<()> {
    let remote_path = remote_timeline_path(ttid)?;

    // see DEFAULT_MAX_KEYS_PER_LIST_RESPONSE
    let batch_size: NonZeroU32 = NonZeroU32::new(1000).unwrap();

    pausable_failpoint!("sk-delete-timeline-remote-pause");
    fail::fail_point!("sk-delete-timeline-remote", |_| {
        Err(anyhow::anyhow!("failpoint: sk-delete-timeline-remote"))
    });

    let cancel = CancellationToken::new(); // not really used
    backoff::retry(
        || async {
            // Do list-delete in batch_size batches to make progress
            // even if there are many files.
            loop {
                let files = storage
                    .list(
                        Some(&remote_path),
                        ListingMode::NoDelimiter,
                        Some(batch_size),
                        &cancel,
                    )
                    .await?
                    .keys
                    .into_iter()
                    .map(|o| o.key)
                    .collect::<Vec<_>>();
                if files.is_empty() {
                    return Ok(());
                }
                info!(
                    "deleting batch of {} WAL segments [{}-{}]",
                    files.len(),
                    files.first().unwrap().object_name().unwrap_or(""),
                    files.last().unwrap().object_name().unwrap_or("")
                );
                storage.delete_objects(&files, &cancel).await?;
            }
        },
        |_| false,
        3,
        10,
        "executing WAL segments deletion batch",
        &cancel,
    )
    .await
    .ok_or_else(|| anyhow::anyhow!("canceled"))
    .and_then(|x| x)?;

    Ok(())
}

pub fn remote_timeline_path(ttid: &TenantTimelineId) -> Result<RemotePath> {
    RemotePath::new(&Utf8Path::new(&ttid.tenant_id.to_string()).join(ttid.timeline_id.to_string()))
}
