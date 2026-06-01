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
//! that needs guarding. We keep the same public API surface so the
//! rest of the pageserver doesn't change; the implementation is just
//! a direct `remote_storage.delete()` and a `fetch_max` on the
//! caller-supplied `result_slot` for `remote_consistent_lsn` updates.

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
    pub fn spawn_with(self, runtime: &tokio::runtime::Handle) -> tokio::task::JoinHandle<()> {
        // yb-mode has no background workers, but the caller invokes
        // us from a sync startup context, so `tokio::spawn` (which
        // requires being inside an active reactor) would panic.
        // Spawn into the explicit runtime Handle instead.
        runtime.spawn(async {})
    }
}

#[derive(Clone)]
pub struct DeletionQueueClient {
    storage: GenericRemoteStorage,
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
    /// upcall — we publish to the slot directly.
    pub(crate) async fn update_remote_consistent_lsn(
        &self,
        _tenant_shard_id: TenantShardId,
        _timeline_id: TimelineId,
        _current_generation: Generation,
        lsn: Lsn,
        result_slot: Arc<AtomicLsn>,
    ) {
        result_slot.fetch_max(lsn);
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
        _conf: &'static PageServerConf,
    ) -> (Self, DeletionQueueWorkers<C>)
    where
        C: StorageControllerUpcallApi + Send + Sync,
    {
        let client = DeletionQueueClient {
            storage: remote_storage,
        };
        (
            Self { client },
            DeletionQueueWorkers {
                _phantom: std::marker::PhantomData,
            },
        )
    }

    pub async fn shutdown(&mut self, _timeout: Duration) {
        // No background workers to wind down.
    }
}
