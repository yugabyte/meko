use std::collections::{HashMap, HashSet, VecDeque};
use std::fmt::Debug;
use std::sync::Arc;
use std::sync::atomic::AtomicU32;

use chrono::NaiveDateTime;
use once_cell::sync::Lazy;
use tracing::info;
use utils::generation::Generation;
use utils::lsn::{AtomicLsn, Lsn};

use super::remote_timeline_client::is_same_remote_layer_path;
use super::storage_layer::{AsLayerDesc as _, LayerName, ResidentLayer};
use crate::tenant::metadata::TimelineMetadata;
use crate::tenant::remote_timeline_client::index::{IndexPart, LayerFileMetadata};

/// Kill switch for upload queue reordering in case it causes problems.
/// TODO: remove this once we have confidence in it.
static DISABLE_UPLOAD_QUEUE_REORDERING: Lazy<bool> =
    Lazy::new(|| std::env::var("DISABLE_UPLOAD_QUEUE_REORDERING").as_deref() == Ok("true"));

/// Kill switch for index upload coalescing in case it causes problems.
/// TODO: remove this once we have confidence in it.
static DISABLE_UPLOAD_QUEUE_INDEX_COALESCING: Lazy<bool> =
    Lazy::new(|| std::env::var("DISABLE_UPLOAD_QUEUE_INDEX_COALESCING").as_deref() == Ok("true"));

// clippy warns that Uninitialized is much smaller than Initialized, which wastes
// memory for Uninitialized variants. Doesn't matter in practice, there are not
// that many upload queues in a running pageserver, and most of them are initialized
// anyway.
#[allow(clippy::large_enum_variant)]
pub enum UploadQueue {
    Uninitialized,
    Initialized(UploadQueueInitialized),
    Stopped(UploadQueueStopped),
}

impl UploadQueue {
    pub fn as_str(&self) -> &'static str {
        match self {
            UploadQueue::Uninitialized => "Uninitialized",
            UploadQueue::Initialized(_) => "Initialized",
            UploadQueue::Stopped(_) => "Stopped",
        }
    }
}

#[derive(Copy, Clone, PartialEq, Eq, Hash, Debug)]
pub enum OpType {
    MayReorder,
    FlushDeletion,
}

/// This keeps track of queued and in-progress tasks.
pub struct UploadQueueInitialized {
    /// Maximum number of inprogress tasks to schedule. 0 is no limit.
    pub(crate) inprogress_limit: usize,

    /// Counter to assign task IDs
    pub(crate) task_counter: u64,

    /// The next uploaded index_part.json; assumed to be dirty.
    ///
    /// Should not be read, directly except for layer file updates. Instead you should add a
    /// projected field.
    pub(crate) dirty: IndexPart,

    /// The latest remote persisted IndexPart.
    ///
    /// Each completed metadata upload will update this. The second item is the task_id which last
    /// updated the value, used to ensure we never store an older value over a newer one.
    pub(crate) clean: (IndexPart, Option<u64>),

    /// How many file uploads or deletions been scheduled, since the
    /// last (scheduling of) metadata index upload?
    pub(crate) latest_files_changes_since_metadata_upload_scheduled: u64,

    /// The Lsn is only updated after our generation has been validated with
    /// the control plane (unlesss a timeline's generation is None, in which case
    /// we skip validation)
    pub(crate) visible_remote_consistent_lsn: Arc<AtomicLsn>,

    /// Tasks that are currently in-progress. In-progress means that a tokio Task
    /// has been launched for it. An in-progress task can be busy uploading, but it can
    /// also be waiting on the `concurrency_limiter` Semaphore in S3Bucket, or it can
    /// be waiting for retry in `exponential_backoff`.
    pub inprogress_tasks: HashMap<u64, Arc<UploadTask>>,

    /// Queued operations that have not been launched yet. They might depend on previous
    /// tasks to finish. For example, metadata upload cannot be performed before all
    /// preceding layer file uploads have completed.
    pub queued_operations: VecDeque<UploadOp>,

    /// Files which have been unlinked but not yet had scheduled a deletion for. Only kept around
    /// for error logging.
    ///
    /// Putting this behind a testing feature to catch problems in tests, but assuming we could have a
    /// bug causing leaks, then it's better to not leave this enabled for production builds.
    #[cfg(feature = "testing")]
    pub(crate) dangling_files: HashMap<LayerName, Generation>,

    /// Ensure we order file operations correctly.
    pub(crate) recently_deleted: HashSet<(LayerName, Generation)>,

    /// Deletions that are blocked by the tenant configuration
    pub(crate) blocked_deletions: Vec<Delete>,

    /// Set to true when we have inserted the `UploadOp::Shutdown` into the `inprogress_tasks`.
    pub(crate) shutting_down: bool,

    /// Permitless semaphore on which any number of `RemoteTimelineClient::shutdown` futures can
    /// wait on until one of them stops the queue. The semaphore is closed when
    /// `RemoteTimelineClient::launch_queued_tasks` encounters `UploadOp::Shutdown`.
    pub(crate) shutdown_ready: Arc<tokio::sync::Semaphore>,
}

impl UploadQueueInitialized {
    pub(super) fn no_pending_work(&self) -> bool {
        self.inprogress_tasks.is_empty() && self.queued_operations.is_empty()
    }

    pub(super) fn get_last_remote_consistent_lsn_visible(&self) -> Lsn {
        self.visible_remote_consistent_lsn.load()
    }

    pub(super) fn get_last_remote_consistent_lsn_projected(&self) -> Option<Lsn> {
        let lsn = self.clean.0.metadata.disk_consistent_lsn();
        self.clean.1.map(|_| lsn)
    }

    /// Returns and removes the next ready operation from the queue, if any. This isn't necessarily
    /// the first operation in the queue, to avoid head-of-line blocking -- an operation can jump
    /// the queue if it doesn't conflict with operations ahead of it.
    ///
    /// Also returns any operations that were coalesced into this one, e.g. multiple index uploads.
    ///
    /// None may be returned even if the queue isn't empty, if no operations are ready yet.
    ///
    /// NB: this is quadratic, but queues are expected to be small, and bounded by inprogress_limit.
    pub fn next_ready(&mut self) -> Option<(UploadOp, Vec<UploadOp>)> {
        // If inprogress_tasks is already at limit, don't schedule anything more.
        if self.inprogress_limit > 0 && self.inprogress_tasks.len() >= self.inprogress_limit {
            return None;
        }

        for (i, candidate) in self.queued_operations.iter().enumerate() {
            // If this candidate is ready, go for it. Otherwise, try the next one.
            if self.is_ready(i) {
                // Shutdown operations are left at the head of the queue, to prevent further
                // operations from starting. Signal that we're ready to shut down.
                if matches!(candidate, UploadOp::Shutdown) {
                    assert!(self.inprogress_tasks.is_empty(), "shutdown with tasks");
                    assert_eq!(i, 0, "shutdown not at head of queue");
                    self.shutdown_ready.close();
                    return None;
                }

                let mut op = self.queued_operations.remove(i).expect("i can't disappear");

                // Coalesce any back-to-back index uploads by only uploading the newest one that's
                // ready. This typically happens with layer/index/layer/index/... sequences, where
                // the layers bypass the indexes, leaving the indexes queued.
                //
                // If other operations are interleaved between index uploads we don't try to
                // coalesce them, since we may as well update the index concurrently with them.
                // This keeps the index fresh and avoids starvation.
                //
                // NB: we assume that all uploaded indexes have the same remote path. This
                // is true at the time of writing: the path only depends on the tenant,
                // timeline and generation, all of which are static for a timeline instance.
                // Otherwise, we must be careful not to coalesce different paths.
                let mut coalesced_ops = Vec::new();
                if matches!(op, UploadOp::UploadMetadata { .. }) {
                    while let Some(UploadOp::UploadMetadata { .. }) = self.queued_operations.get(i)
                    {
                        if *DISABLE_UPLOAD_QUEUE_INDEX_COALESCING {
                            break;
                        }
                        if !self.is_ready(i) {
                            break;
                        }
                        coalesced_ops.push(op);
                        op = self.queued_operations.remove(i).expect("i can't disappear");
                    }
                }

                return Some((op, coalesced_ops));
            }

            // Nothing can bypass a barrier or shutdown. If it wasn't scheduled above, give up.
            if matches!(candidate, UploadOp::Barrier(_) | UploadOp::Shutdown) {
                return None;
            }

            // If upload queue reordering is disabled, bail out after the first operation.
            if *DISABLE_UPLOAD_QUEUE_REORDERING {
                return None;
            }
        }
        None
    }

    /// Returns true if the queued operation at the given position is ready to be uploaded, i.e. if
    /// it doesn't conflict with any in-progress or queued operations ahead of it. Operations are
    /// allowed to skip the queue when it's safe to do so, to increase parallelism.
    ///
    /// The position must be valid for the queue size.
    fn is_ready(&self, pos: usize) -> bool {
        let candidate = self.queued_operations.get(pos).expect("invalid position");
        self
            // Look at in-progress operations, in random order.
            .inprogress_tasks
            .values()
            .map(|task| &task.op)
            // Then queued operations ahead of the candidate, front-to-back.
            .chain(self.queued_operations.iter().take(pos))
            // Keep track of the active index ahead of each operation. This is used to ensure that
            // an upload doesn't skip the queue too far, such that it modifies a layer that's
            // referenced by an active index.
            //
            // It's okay that in-progress operations are emitted in random order above, since at
            // most one of them can be an index upload (enforced by can_bypass).
            .scan(&self.clean.0, |next_active_index, op| {
                let active_index = *next_active_index;
                if let UploadOp::UploadMetadata { uploaded } = op {
                    *next_active_index = uploaded; // stash index for next operation after this
                }
                Some((op, active_index))
            })
            // Check if the candidate can bypass all of them.
            .all(|(op, active_index)| candidate.can_bypass(op, active_index))
    }

    /// Returns the number of in-progress deletion operations.
    #[cfg(test)]
    pub(crate) fn num_inprogress_deletions(&self) -> usize {
        self.inprogress_tasks
            .iter()
            .filter(|(_, t)| matches!(t.op, UploadOp::Delete(_)))
            .count()
    }

    /// Returns the number of in-progress layer uploads.
    #[cfg(test)]
    pub(crate) fn num_inprogress_layer_uploads(&self) -> usize {
        self.inprogress_tasks
            .iter()
            .filter(|(_, t)| matches!(t.op, UploadOp::UploadLayer(_, _, _)))
            .count()
    }

    /// Test helper that schedules all ready operations into inprogress_tasks, and returns
    /// references to them.
    ///
    /// TODO: the corresponding production logic should be moved from RemoteTimelineClient into
    /// UploadQueue, so we can use the same code path.
    #[cfg(test)]
    fn schedule_ready(&mut self) -> Vec<Arc<UploadTask>> {
        let mut tasks = Vec::new();
        // NB: schedule operations one by one, to handle conflicts with inprogress_tasks.
        while let Some((op, coalesced_ops)) = self.next_ready() {
            self.task_counter += 1;
            let task = Arc::new(UploadTask {
                task_id: self.task_counter,
                op,
                coalesced_ops,
                retries: 0.into(),
            });
            self.inprogress_tasks.insert(task.task_id, task.clone());
            tasks.push(task);
        }
        tasks
    }

    /// Test helper that marks an operation as completed, removing it from inprogress_tasks.
    ///
    /// TODO: the corresponding production logic should be moved from RemoteTimelineClient into
    /// UploadQueue, so we can use the same code path.
    #[cfg(test)]
    fn complete(&mut self, task_id: u64) {
        let Some(task) = self.inprogress_tasks.remove(&task_id) else {
            return;
        };
        // Update the clean index on uploads.
        if let UploadOp::UploadMetadata { ref uploaded } = task.op {
            if task.task_id > self.clean.1.unwrap_or_default() {
                self.clean = (*uploaded.clone(), Some(task.task_id));
            }
        }
    }
}

#[derive(Clone, Copy)]
pub(super) enum SetDeletedFlagProgress {
    NotRunning,
    InProgress(NaiveDateTime),
    Successful(NaiveDateTime),
}

pub struct UploadQueueStoppedDeletable {
    pub(super) upload_queue_for_deletion: UploadQueueInitialized,
    pub(super) deleted_at: SetDeletedFlagProgress,
}

#[allow(clippy::large_enum_variant, reason = "TODO")]
pub enum UploadQueueStopped {
    Deletable(UploadQueueStoppedDeletable),
    Uninitialized,
}

#[derive(thiserror::Error, Debug)]
pub enum NotInitialized {
    #[error("queue is in state Uninitialized")]
    Uninitialized,
    #[error("queue is in state Stopped")]
    Stopped,
    #[error("queue is shutting down")]
    ShuttingDown,
}

impl NotInitialized {
    pub(crate) fn is_stopping(&self) -> bool {
        use NotInitialized::*;
        match self {
            Uninitialized => false,
            Stopped => true,
            ShuttingDown => true,
        }
    }
}

impl UploadQueue {
    pub fn initialize_empty_remote(
        &mut self,
        metadata: &TimelineMetadata,
        inprogress_limit: usize,
    ) -> anyhow::Result<&mut UploadQueueInitialized> {
        match self {
            UploadQueue::Uninitialized => (),
            UploadQueue::Initialized(_) | UploadQueue::Stopped(_) => {
                anyhow::bail!("already initialized, state {}", self.as_str())
            }
        }

        info!("initializing upload queue for empty remote");

        let index_part = IndexPart::empty(metadata.clone());

        let state = UploadQueueInitialized {
            inprogress_limit,
            dirty: index_part.clone(),
            clean: (index_part, None),
            latest_files_changes_since_metadata_upload_scheduled: 0,
            visible_remote_consistent_lsn: Arc::new(AtomicLsn::new(0)),
            // what follows are boring default initializations
            task_counter: 0,
            inprogress_tasks: HashMap::new(),
            queued_operations: VecDeque::new(),
            #[cfg(feature = "testing")]
            dangling_files: HashMap::new(),
            recently_deleted: HashSet::new(),
            blocked_deletions: Vec::new(),
            shutting_down: false,
            shutdown_ready: Arc::new(tokio::sync::Semaphore::new(0)),
        };

        *self = UploadQueue::Initialized(state);
        Ok(self.initialized_mut().expect("we just set it"))
    }

    pub fn initialize_with_current_remote_index_part(
        &mut self,
        index_part: &IndexPart,
        inprogress_limit: usize,
    ) -> anyhow::Result<&mut UploadQueueInitialized> {
        match self {
            UploadQueue::Uninitialized => (),
            UploadQueue::Initialized(_) | UploadQueue::Stopped(_) => {
                anyhow::bail!("already initialized, state {}", self.as_str())
            }
        }

        info!(
            "initializing upload queue with remote index_part.disk_consistent_lsn: {}",
            index_part.metadata.disk_consistent_lsn()
        );

        let state = UploadQueueInitialized {
            inprogress_limit,
            dirty: index_part.clone(),
            clean: (index_part.clone(), None),
            latest_files_changes_since_metadata_upload_scheduled: 0,
            visible_remote_consistent_lsn: Arc::new(
                index_part.metadata.disk_consistent_lsn().into(),
            ),
            // what follows are boring default initializations
            task_counter: 0,
            inprogress_tasks: HashMap::new(),
            queued_operations: VecDeque::new(),
            #[cfg(feature = "testing")]
            dangling_files: HashMap::new(),
            recently_deleted: HashSet::new(),
            blocked_deletions: Vec::new(),
            shutting_down: false,
            shutdown_ready: Arc::new(tokio::sync::Semaphore::new(0)),
        };

        *self = UploadQueue::Initialized(state);
        Ok(self.initialized_mut().expect("we just set it"))
    }

    pub fn initialized_mut(&mut self) -> Result<&mut UploadQueueInitialized, NotInitialized> {
        use UploadQueue::*;
        match self {
            Uninitialized => Err(NotInitialized::Uninitialized),
            Initialized(x) => {
                if x.shutting_down {
                    Err(NotInitialized::ShuttingDown)
                } else {
                    Ok(x)
                }
            }
            Stopped(_) => Err(NotInitialized::Stopped),
        }
    }

    pub(crate) fn stopped_mut(&mut self) -> anyhow::Result<&mut UploadQueueStoppedDeletable> {
        match self {
            UploadQueue::Initialized(_) | UploadQueue::Uninitialized => {
                anyhow::bail!("queue is in state {}", self.as_str())
            }
            UploadQueue::Stopped(UploadQueueStopped::Uninitialized) => {
                anyhow::bail!("queue is in state Stopped(Uninitialized)")
            }
            UploadQueue::Stopped(UploadQueueStopped::Deletable(deletable)) => Ok(deletable),
        }
    }
}

/// An in-progress upload or delete task.
#[derive(Debug)]
pub struct UploadTask {
    /// Unique ID of this task. Used as the key in `inprogress_tasks` above.
    pub task_id: u64,
    /// Number of task retries.
    pub retries: AtomicU32,
    /// The upload operation.
    pub op: UploadOp,
    /// Any upload operations that were coalesced into this operation. This typically happens with
    /// back-to-back index uploads, see `UploadQueueInitialized::next_ready()`.
    pub coalesced_ops: Vec<UploadOp>,
}

/// A deletion of some layers within the lifetime of a timeline.  This is not used
/// for timeline deletion, which skips this queue and goes directly to DeletionQueue.
#[derive(Debug, Clone)]
pub struct Delete {
    pub layers: Vec<(LayerName, LayerFileMetadata)>,
}

#[derive(Clone, Debug)]
pub enum UploadOp {
    /// Upload a layer file. The last field indicates the last operation for thie file.
    UploadLayer(ResidentLayer, LayerFileMetadata, Option<OpType>),

    /// Upload a index_part.json file
    UploadMetadata {
        /// The next [`UploadQueueInitialized::clean`] after this upload succeeds.
        uploaded: Box<IndexPart>,
    },

    /// Delete layer files
    Delete(Delete),

    /// Barrier. When the barrier operation is reached, the channel is closed.
    Barrier(tokio::sync::watch::Sender<()>),

    /// Shutdown; upon encountering this operation no new operations will be spawned, otherwise
    /// this is the same as a Barrier.
    Shutdown,
}

impl std::fmt::Display for UploadOp {
    fn fmt(&self, f: &mut std::fmt::Formatter) -> std::fmt::Result {
        match self {
            UploadOp::UploadLayer(layer, metadata, mode) => {
                write!(
                    f,
                    "UploadLayer({}, size={:?}, gen={:?}, mode={:?})",
                    layer, metadata.file_size, metadata.generation, mode
                )
            }
            UploadOp::UploadMetadata { uploaded, .. } => {
                write!(
                    f,
                    "UploadMetadata(lsn: {})",
                    uploaded.metadata.disk_consistent_lsn()
                )
            }
            UploadOp::Delete(delete) => {
                write!(f, "Delete({} layers)", delete.layers.len())
            }
            UploadOp::Barrier(_) => write!(f, "Barrier"),
            UploadOp::Shutdown => write!(f, "Shutdown"),
        }
    }
}

impl UploadOp {
    /// Returns true if self can bypass other, i.e. if the operations don't conflict. index is the
    /// active index when other would be uploaded -- if we allow self to bypass other, this would
    /// be the active index when self is uploaded.
    pub fn can_bypass(&self, other: &UploadOp, index: &IndexPart) -> bool {
        match (self, other) {
            // Nothing can bypass a barrier or shutdown, and it can't bypass anything.
            (UploadOp::Barrier(_), _) | (_, UploadOp::Barrier(_)) => false,
            (UploadOp::Shutdown, _) | (_, UploadOp::Shutdown) => false,

            // Uploads and deletes can bypass each other unless they're for the same file.
            (UploadOp::UploadLayer(a, ameta, _), UploadOp::UploadLayer(b, bmeta, _)) => {
                let aname = &a.layer_desc().layer_name();
                let bname = &b.layer_desc().layer_name();
                !is_same_remote_layer_path(aname, ameta, bname, bmeta)
            }
            (UploadOp::UploadLayer(u, umeta, _), UploadOp::Delete(d))
            | (UploadOp::Delete(d), UploadOp::UploadLayer(u, umeta, _)) => {
                d.layers.iter().all(|(dname, dmeta)| {
                    !is_same_remote_layer_path(&u.layer_desc().layer_name(), umeta, dname, dmeta)
                })
            }

            // Deletes are idempotent and can always bypass each other.
            (UploadOp::Delete(_), UploadOp::Delete(_)) => true,

            // Uploads and deletes can bypass an index upload as long as neither the uploaded index
            // nor the active index below it references the file. A layer can't be modified or
            // deleted while referenced by an index.
            //
            // Similarly, index uploads can bypass uploads and deletes as long as neither the
            // uploaded index nor the active index references the file (the latter would be
            // incorrect use by the caller).
            (UploadOp::UploadLayer(u, umeta, _), UploadOp::UploadMetadata { uploaded: i })
            | (UploadOp::UploadMetadata { uploaded: i }, UploadOp::UploadLayer(u, umeta, _)) => {
                let uname = u.layer_desc().layer_name();
                !i.references(&uname, umeta) && !index.references(&uname, umeta)
            }
            (UploadOp::Delete(d), UploadOp::UploadMetadata { uploaded: i })
            | (UploadOp::UploadMetadata { uploaded: i }, UploadOp::Delete(d)) => {
                d.layers.iter().all(|(dname, dmeta)| {
                    !i.references(dname, dmeta) && !index.references(dname, dmeta)
                })
            }

            // Indexes can never bypass each other. They can coalesce though, and
            // `UploadQueue::next_ready()` currently does this when possible.
            (UploadOp::UploadMetadata { .. }, UploadOp::UploadMetadata { .. }) => false,
        }
    }
}

