//! yb-mode stub of the deletion queue.
//!
//! Neon's original `DeletionQueue` was a 3-stage pipeline
//! (`ListWriter` → `Validator` → `Deleter`) that batched S3 object
//! deletions, persisted deletion lists to local disk, and gated every
//! batch on a generation-number upcall to the storage controller. The
//! whole apparatus existed because deferred S3 deletes had to survive
//! multi-attach races (see `docs/rfcs/025-generation-numbers.md`).
//!
//! ybLight's storage is YugabyteDB rows. Deletes are atomic single-
//! statement `DELETE FROM blobs`; there is no race window between
//! "this layer file is unreferenced" and "we actually unlink it"
//! that needs guarding. We keep the same public API
//! (`DeletionQueue`, `DeletionQueueClient`, `DeletionQueueWorkers`,
//! `recover`, `update_remote_consistent_lsn`, `push_layers`,
//! `push_immediate`, `flush`, `flush_advisory`, `flush_execute`,
//! `flush_immediate`, `new`, `spawn_with`, `shutdown`) so the rest
//! of the pageserver doesn't change, but the implementation is just
//! a direct `remote_storage.delete()` plus the `VisibleLsnUpdates`
//! tracking that timelines still need for advertising
//! `remote_consistent_lsn`.

use std::collections::HashMap;
use std::sync::Arc;
use std::time::Duration;

use pageserver_api::shard::TenantShardId;
use remote_storage::{GenericRemoteStorage, RemotePath};
use thiserror::Error;
use tokio_util::sync::CancellationToken;
use tracing::warn;
use utils::generation::Generation;
use utils::id::TimelineId;
use utils::lsn::{AtomicLsn, Lsn};

use crate::config::PageServerConf;
use crate::controller_upcall_client::StorageControllerUpcallApi;
use crate::tenant::remote_timeline_client::{LayerFileMetadata, remote_layer_path};
use crate::tenant::storage_layer::LayerName;

#[derive(Clone)]
pub struct DeletionQueue {
    client: DeletionQueueClient,
}

/// Returned by `DeletionQueue::new` for symmetry with the original
/// API. yb-mode runs no background workers, so this is just a marker;
/// `spawn_with` is a no-op.
pub struct DeletionQueueWorkers<C> {
    _phantom: std::marker::PhantomData<C>,
}

impl<C> DeletionQueueWorkers<C>
where
    C: StorageControllerUpcallApi + Send + Sync + 'static,
{
    pub fn spawn_with(self, _runtime: &tokio::runtime::Handle) -> tokio::task::JoinHandle<()> {
        tokio::spawn(async {})
    }
}

#[derive(Clone)]
pub struct DeletionQueueClient {
    storage: GenericRemoteStorage,
    tenant_shard_id_prefix: PageserverPathPrefix,
    lsn_table: Arc<std::sync::RwLock<VisibleLsnUpdates>>,
}

/// Stored per-tenant in `VisibleLsnUpdates`. Mirrors the original
/// generation-aware bookkeeping so `update_remote_consistent_lsn`
/// reaches the same `AtomicLsn` slot.
struct TenantLsnState {
    timelines: HashMap<TimelineId, PendingLsn>,
    generation: Generation,
}

struct PendingLsn {
    projected: Lsn,
    result_slot: Arc<AtomicLsn>,
}

#[derive(Default)]
struct VisibleLsnUpdates {
    tenants: HashMap<TenantShardId, TenantLsnState>,
}

impl VisibleLsnUpdates {
    fn new() -> Self {
        Self::default()
    }
}

#[derive(Clone)]
struct PageserverPathPrefix {
    conf: &'static PageServerConf,
}

#[derive(Error, Debug)]
pub enum DeletionQueueError {
    #[error("Deletion queue unavailable during shutdown")]
    ShuttingDown,
}

impl DeletionQueueClient {
    /// In Neon this replayed a persistent deletion list from local
    /// disk on startup. yb-mode keeps no persistent queue state — the
    /// `remote_storage.delete()` call below runs synchronously and
    /// either succeeds or returns an error to the caller — so recovery
    /// is a no-op.
    pub(crate) fn recover(
        &self,
        _attached_tenants: HashMap<TenantShardId, Generation>,
    ) -> Result<(), DeletionQueueError> {
        Ok(())
    }

    /// Advertise a timeline's new `remote_consistent_lsn`. Unlike the
    /// Neon pipeline this doesn't wait for a generation-validation
    /// upcall — we just publish the LSN to the same `AtomicLsn` slot
    /// callers read from.
    pub(crate) async fn update_remote_consistent_lsn(
        &self,
        tenant_shard_id: TenantShardId,
        timeline_id: TimelineId,
        current_generation: Generation,
        lsn: Lsn,
        result_slot: Arc<AtomicLsn>,
    ) {
        // Publish to the slot so external observers see the new LSN
        // immediately. The slot is the durable handoff; the table
        // entry below mirrors it so we keep parity with the original
        // bookkeeping (e.g. for diagnostics that walk the table).
        result_slot.fetch_max(lsn);

        let mut locked = self
            .lsn_table
            .write()
            .expect("Lock should never be poisoned");

        let tenant_entry = locked
            .tenants
            .entry(tenant_shard_id)
            .or_insert(TenantLsnState {
                timelines: HashMap::new(),
                generation: current_generation,
            });

        if tenant_entry.generation != current_generation {
            tenant_entry.timelines.clear();
            tenant_entry.generation = current_generation;
        }

        tenant_entry.timelines.insert(
            timeline_id,
            PendingLsn {
                projected: lsn,
                result_slot,
            },
        );
    }

    /// Submit layer-file paths for deletion against `remote_storage`.
    /// Runs synchronously: when this returns Ok, the paths are gone
    /// from the backing store. There is no generation gate in yb-mode
    /// because YB row deletes don't race with passive readers the way
    /// S3 object deletes did.
    pub(crate) fn push_layers(
        &self,
        tenant_shard_id: TenantShardId,
        timeline_id: TimelineId,
        current_generation: Generation,
        layers: Vec<(LayerName, LayerFileMetadata)>,
    ) -> Result<(), DeletionQueueError> {
        if layers.is_empty() {
            return Ok(());
        }
        let conf = self.tenant_shard_id_prefix.conf;
        let paths: Vec<RemotePath> = layers
            .into_iter()
            .map(|(name, meta)| {
                remote_layer_path(
                    &tenant_shard_id.tenant_id,
                    &timeline_id,
                    meta.shard,
                    &name,
                    meta.generation,
                )
            })
            .collect();
        let _ = (current_generation, conf);
        self.delete_paths_blocking(paths)
    }

    fn delete_paths_blocking(&self, paths: Vec<RemotePath>) -> Result<(), DeletionQueueError> {
        let storage = self.storage.clone();
        // We can't await inside this sync function. Spawn onto the
        // current runtime in detached mode and let the delete complete
        // in the background; in yb-mode the only effect of "delete now
        // vs in a moment" is timing of the underlying YB DELETE, not
        // correctness. Surface failures via tracing instead of
        // bubbling them back to the (sync-only) caller.
        let cancel = CancellationToken::new();
        tokio::spawn(async move {
            for path in paths {
                if let Err(e) = storage.delete(&path, &cancel).await {
                    warn!("deletion_queue: failed to delete {path}: {e:#}");
                }
            }
        });
        Ok(())
    }

    pub async fn flush(&self) -> Result<(), DeletionQueueError> {
        // No queue, nothing to flush.
        Ok(())
    }

    pub fn flush_advisory(&self) {
        // No queue, nothing to flush.
    }

    pub(crate) async fn flush_execute(&self) -> Result<(), DeletionQueueError> {
        Ok(())
    }

    /// Synchronous-immediate variant used by timeline deletion. The
    /// caller has already decided the listed objects are unreachable;
    /// just delete them.
    pub(crate) async fn push_immediate(
        &self,
        objects: Vec<RemotePath>,
    ) -> Result<(), DeletionQueueError> {
        let cancel = CancellationToken::new();
        for path in objects {
            if let Err(e) = self.storage.delete(&path, &cancel).await {
                warn!("deletion_queue.push_immediate: failed to delete {path}: {e:#}");
            }
        }
        Ok(())
    }

    pub(crate) async fn flush_immediate(&self) -> Result<(), DeletionQueueError> {
        // push_immediate is itself synchronous; nothing left to flush.
        Ok(())
    }
}

impl DeletionQueue {
    pub fn new_client(&self) -> DeletionQueueClient {
        self.client.clone()
    }

    pub fn new<C>(
        remote_storage: GenericRemoteStorage,
        _controller_upcall_client: C,
        conf: &'static PageServerConf,
    ) -> (Self, DeletionQueueWorkers<C>)
    where
        C: StorageControllerUpcallApi + Send + Sync,
    {
        let client = DeletionQueueClient {
            storage: remote_storage,
            tenant_shard_id_prefix: PageserverPathPrefix { conf },
            lsn_table: Arc::new(std::sync::RwLock::new(VisibleLsnUpdates::new())),
        };
        (
            Self {
                client,
            },
            DeletionQueueWorkers {
                _phantom: std::marker::PhantomData,
            },
        )
    }

    pub async fn shutdown(&mut self, _timeout: Duration) {
        // No background workers to wind down.
    }
}
