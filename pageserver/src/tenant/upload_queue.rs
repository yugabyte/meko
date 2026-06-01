use std::collections::VecDeque;
use std::fmt::Debug;
use std::sync::Arc;
use std::sync::atomic::AtomicU32;

use chrono::NaiveDateTime;
use tracing::info;
use utils::lsn::{AtomicLsn, Lsn};

use crate::tenant::metadata::TimelineMetadata;
use crate::tenant::remote_timeline_client::index::IndexPart;

// Uninitialized is much smaller than Initialized, but this is harmless
// in practice: only one upload queue exists per timeline.
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

/// Queued and in-progress upload operations.
///
/// yb-pageserver only uploads index parts (no layer files, no per-file
/// deletes), so the queue is highly degenerate: at most one operation
/// is ever in flight, and Barrier / Shutdown / UploadMetadata all
/// fully block each other. We keep the queue shape (FIFO of ops + a
/// slot for the in-flight task) to preserve barrier / shutdown
/// semantics, but the prior reordering / `can_bypass` machinery is
/// gone.
pub struct UploadQueueInitialized {
    /// The next uploaded `index_part.json`; assumed to be dirty.
    ///
    /// Don't read this directly except for layer-metadata updates —
    /// add a projected field instead.
    pub(crate) dirty: IndexPart,

    /// The latest remote-persisted `IndexPart`.
    ///
    /// Each completed metadata upload updates this. The second item
    /// is the task ID that last wrote it, so we never overwrite a
    /// newer value with an older one.
    pub(crate) clean: (IndexPart, Option<u64>),

    /// Only updated after the generation has been validated with the
    /// control plane (skipped when the timeline's generation is None).
    pub(crate) visible_remote_consistent_lsn: Arc<AtomicLsn>,

    /// Monotonic ID assigned to each spawned task. Used for tracing
    /// and for clean-update ordering above.
    pub(crate) task_counter: u64,

    /// Currently in-progress upload task. At most one exists because
    /// every `UploadOp` blocks every other one.
    pub inprogress_task: Option<Arc<UploadTask>>,

    /// Operations queued for later execution.
    pub queued_operations: VecDeque<UploadOp>,

    /// Set to true once `UploadOp::Shutdown` has been observed at the
    /// head of the queue.
    pub(crate) shutting_down: bool,

    /// Permitless semaphore on which any number of
    /// `RemoteTimelineClient::shutdown` futures can park until one of
    /// them stops the queue. Closed by `launch_queued_tasks` when it
    /// encounters `UploadOp::Shutdown`.
    pub(crate) shutdown_ready: Arc<tokio::sync::Semaphore>,
}

impl UploadQueueInitialized {
    pub(super) fn no_pending_work(&self) -> bool {
        self.inprogress_task.is_none() && self.queued_operations.is_empty()
    }

    pub(super) fn get_last_remote_consistent_lsn_visible(&self) -> Lsn {
        self.visible_remote_consistent_lsn.load()
    }

    pub(super) fn get_last_remote_consistent_lsn_projected(&self) -> Option<Lsn> {
        let lsn = self.clean.0.metadata.disk_consistent_lsn();
        self.clean.1.map(|_| lsn)
    }

    /// Pop the next ready op from the queue, plus any back-to-back
    /// `UploadMetadata` operations to coalesce with it.
    ///
    /// Returns `None` when the queue is empty, a task is already in
    /// flight, or the head is `Shutdown` (which is left at the head
    /// to block everything; `shutdown_ready` is closed as a signal).
    pub fn next_ready(&mut self) -> Option<(UploadOp, Vec<UploadOp>)> {
        if self.inprogress_task.is_some() {
            return None;
        }
        let head = self.queued_operations.front()?;
        if matches!(head, UploadOp::Shutdown) {
            self.shutdown_ready.close();
            return None;
        }

        let mut op = self.queued_operations.pop_front().expect("checked above");
        let mut coalesced_ops = Vec::new();
        if matches!(op, UploadOp::UploadMetadata { .. }) {
            while let Some(UploadOp::UploadMetadata { .. }) = self.queued_operations.front() {
                coalesced_ops.push(op);
                op = self
                    .queued_operations
                    .pop_front()
                    .expect("checked via front()");
            }
        }
        Some((op, coalesced_ops))
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
            dirty: index_part.clone(),
            clean: (index_part, None),
            visible_remote_consistent_lsn: Arc::new(AtomicLsn::new(0)),
            task_counter: 0,
            inprogress_task: None,
            queued_operations: VecDeque::new(),
            shutting_down: false,
            shutdown_ready: Arc::new(tokio::sync::Semaphore::new(0)),
        };

        *self = UploadQueue::Initialized(state);
        Ok(self.initialized_mut().expect("we just set it"))
    }

    pub fn initialize_with_current_remote_index_part(
        &mut self,
        index_part: &IndexPart,
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
            dirty: index_part.clone(),
            clean: (index_part.clone(), None),
            visible_remote_consistent_lsn: Arc::new(
                index_part.metadata.disk_consistent_lsn().into(),
            ),
            task_counter: 0,
            inprogress_task: None,
            queued_operations: VecDeque::new(),
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

/// An in-progress upload task.
#[derive(Debug)]
pub struct UploadTask {
    /// Unique ID. Mirrors `UploadQueueInitialized::task_counter`.
    pub task_id: u64,
    /// Number of retries attempted by `perform_upload_task`.
    pub retries: AtomicU32,
    /// The upload operation.
    pub op: UploadOp,
    /// Back-to-back `UploadMetadata`s that were coalesced into this
    /// task at scheduling time (see
    /// [`UploadQueueInitialized::next_ready`]).
    pub coalesced_ops: Vec<UploadOp>,
}

#[derive(Clone, Debug)]
pub enum UploadOp {
    /// Upload an `index_part.json` file.
    UploadMetadata {
        /// The next [`UploadQueueInitialized::clean`] after this
        /// upload succeeds.
        uploaded: Box<IndexPart>,
    },

    /// Barrier. The channel is closed once the barrier is reached.
    Barrier(tokio::sync::watch::Sender<()>),

    /// Shutdown: blocks every subsequent operation. Otherwise the
    /// same as a Barrier.
    Shutdown,
}

impl std::fmt::Display for UploadOp {
    fn fmt(&self, f: &mut std::fmt::Formatter) -> std::fmt::Result {
        match self {
            UploadOp::UploadMetadata { uploaded, .. } => {
                write!(
                    f,
                    "UploadMetadata(lsn: {})",
                    uploaded.metadata.disk_consistent_lsn()
                )
            }
            UploadOp::Barrier(_) => write!(f, "Barrier"),
            UploadOp::Shutdown => write!(f, "Shutdown"),
        }
    }
}
