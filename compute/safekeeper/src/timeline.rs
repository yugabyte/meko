//! This module implements Timeline lifecycle management and has all necessary code
//! to glue together SafeKeeper and all other background services.

use std::cmp::max;
use std::ops::{Deref, DerefMut};
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::time::Duration;

use anyhow::{Context, Result, anyhow, bail};
use camino::{Utf8Path, Utf8PathBuf};
use http_utils::error::ApiError;
use safekeeper_api::models::PeerInfo;
use tokio::fs::{self};
use tokio::sync::{RwLock, RwLockReadGuard, RwLockWriteGuard, watch};
use tokio::time::Instant;
use tokio_util::sync::CancellationToken;
use tracing::*;
use utils::id::{BranchId, NodeId};
use utils::lsn::Lsn;
use utils::sync::gate::Gate;

use crate::metrics::{
    FullTimelineInfo, MISC_OPERATION_SECONDS, WAL_STORAGE_LIMIT_ERRORS, WalStorageMetrics,
};

use crate::disk_usage::GLOBAL_DISK_LIMIT_EXCEEDED;
use crate::receive_wal::WalReceivers;
use crate::safekeeper::{AcceptorProposerMessage, ProposerAcceptorMessage, SafeKeeper};
use crate::send_wal::{WalSenders, WalSendersTimelineMetricValues};
use crate::state::{TimelineMemState, TimelinePersistentState, TimelineState};
use crate::timeline_guard::ResidenceGuard;
use crate::timeline_manager::{AtomicStatus, ManagerCtl};
use crate::timelines_set::TimelinesSet;
use crate::wal_storage::{Storage as wal_storage_iface, WalReader};
use crate::yb_wal_storage::YbWalConn;
use crate::yb_wal_storage::{self, YbWalStorage};
use crate::{SafeKeeperConf, control_file, debug_dump, timeline_manager};

/// Minimum subset of safekeeper timeline state needed by callers that
/// previously consumed `storage_broker::proto::SafekeeperTimelineInfo`.
#[derive(Debug, Clone)]
pub struct LocalSkInfo {
    pub flush_lsn: Lsn,
    pub commit_lsn: Lsn,
}

// vector-based node id -> peer state map with very limited functionality we
// need.
#[derive(Debug, Clone, Default)]
pub struct PeersInfo(pub Vec<PeerInfo>);

impl PeersInfo {
    #[allow(dead_code)]
    fn get(&mut self, id: NodeId) -> Option<&mut PeerInfo> {
        self.0.iter_mut().find(|p| p.sk_id == id)
    }

    #[allow(dead_code)]
    fn upsert(&mut self, p: &PeerInfo) {
        match self.get(p.sk_id) {
            Some(rp) => *rp = p.clone(),
            None => self.0.push(p.clone()),
        }
    }
}

pub type ReadGuardSharedState<'a> = RwLockReadGuard<'a, SharedState>;

/// WriteGuardSharedState is a wrapper around `RwLockWriteGuard<SharedState>` that
/// automatically updates `watch::Sender` channels with state on drop.
pub struct WriteGuardSharedState<'a> {
    tli: Arc<Timeline>,
    guard: RwLockWriteGuard<'a, SharedState>,
}

impl<'a> WriteGuardSharedState<'a> {
    fn new(tli: Arc<Timeline>, guard: RwLockWriteGuard<'a, SharedState>) -> Self {
        WriteGuardSharedState { tli, guard }
    }
}

impl Deref for WriteGuardSharedState<'_> {
    type Target = SharedState;

    fn deref(&self) -> &Self::Target {
        &self.guard
    }
}

impl DerefMut for WriteGuardSharedState<'_> {
    fn deref_mut(&mut self) -> &mut Self::Target {
        &mut self.guard
    }
}

impl Drop for WriteGuardSharedState<'_> {
    fn drop(&mut self) {
        // yb-light: commit_lsn = flush_lsn (single-SK, every flush is
        // committed). The watcher subscribers (walsenders, yb_wal) see
        // updates every time flush_lsn advances.
        let commit_lsn = self.guard.sk.flush_lsn();

        let _ = self.tli.commit_lsn_watch_tx.send_if_modified(|old| {
            if *old != commit_lsn {
                *old = commit_lsn;
                true
            } else {
                false
            }
        });

        // send notification about shared state update
        self.tli.shared_state_version_tx.send_modify(|old| {
            *old += 1;
        });
    }
}

/// This structure is stored in shared state and represents the state of the timeline.
///
/// Usually it holds SafeKeeper, but it also supports offloaded timeline state. In this
/// case, SafeKeeper is not available (because WAL is not present on disk) and all
/// operations can be done only with control file.
// `Loaded` dominates the size; the `Empty` transitional state is rare and short-lived,
// so boxing the common variant would cost more than the unused space.
#[allow(clippy::large_enum_variant)]
pub enum StateSK {
    Loaded(SafeKeeper<control_file::ControlFileStorage, yb_wal_storage::WalStore>),
    // Not used, required for moving between states.
    Empty,
}

impl StateSK {
    pub fn flush_lsn(&self) -> Lsn {
        match self {
            StateSK::Loaded(sk) => sk.wal_store.flush_lsn(),
            StateSK::Empty => unreachable!(),
        }
    }

    /// Watch receiver that fires as WAL becomes durable in YugabyteDB
    /// (see [`wal_storage::Storage::yb_durable_watch`]). `None` otherwise.
    pub fn yb_durable_watch(&self) -> Option<watch::Receiver<crate::yb_wal_storage::DurableState>> {
        match self {
            StateSK::Loaded(sk) => sk.wal_store.yb_durable_watch(),
            StateSK::Empty => None,
        }
    }

    /// Get a reference to the control file's timeline state.
    pub fn state(&self) -> &TimelineState<control_file::ControlFileStorage> {
        match self {
            StateSK::Loaded(sk) => &sk.state,
            StateSK::Empty => unreachable!(),
        }
    }

    pub fn state_mut(&mut self) -> &mut TimelineState<control_file::ControlFileStorage> {
        match self {
            StateSK::Loaded(sk) => &mut sk.state,
            StateSK::Empty => unreachable!(),
        }
    }

    /// Close open WAL files to release FDs.
    fn close_wal_store(&mut self) {
        if let StateSK::Loaded(sk) = self {
            sk.wal_store.close();
        }
    }

    /// Used for metrics only.
    pub fn wal_storage_metrics(&self) -> WalStorageMetrics {
        match self {
            StateSK::Loaded(sk) => sk.wal_store.get_metrics(),
            StateSK::Empty => unreachable!(),
        }
    }

    /// Returns WAL storage internal LSNs for debug dump.
    pub fn wal_storage_internal_state(&self) -> (Lsn, Lsn, Lsn, bool) {
        match self {
            StateSK::Loaded(sk) => sk.wal_store.internal_state(),
            StateSK::Empty => unreachable!(),
        }
    }

    /// Access to SafeKeeper object.
    pub fn safekeeper(
        &mut self,
    ) -> &mut SafeKeeper<control_file::ControlFileStorage, yb_wal_storage::WalStore> {
        match self {
            StateSK::Loaded(sk) => sk,
            StateSK::Empty => unreachable!(),
        }
    }
}

/// Shared state associated with database instance
pub struct SharedState {
    /// Safekeeper object
    pub(crate) sk: StateSK,
    /// In memory list containing state of peers sent in latest messages from them.
    pub(crate) peers_info: PeersInfo,
    // True value hinders old WAL removal; this is used by snapshotting. We
    // could make it a counter, but there is no need to.
    pub(crate) wal_removal_on_hold: bool,
}

impl SharedState {
    /// Creates a new SharedState.
    pub fn new(sk: StateSK) -> Self {
        Self {
            sk,
            peers_info: PeersInfo(vec![]),
            wal_removal_on_hold: false,
        }
    }

    /// Restore SharedState from control file. If file doesn't exist, bails out.
    pub async fn restore(
        conf: &SafeKeeperConf,
        branch_id: &BranchId,
        yb_wal: Option<&Arc<YbWalConn>>,
    ) -> Result<Self> {
        let yb_wal = yb_wal
            .cloned()
            .context("control-file restore requires yb_storage_conn")?;
        let control_store =
            control_file::ControlFileStorage::restore_new(yb_wal.system_client.clone(), branch_id)
                .await?;
        if control_store.server.wal_seg_size == 0 {
            bail!(TimelineError::UninitializedWalSegSize(*branch_id));
        }

        let wal_store = YbWalStorage::new(branch_id, &control_store, yb_wal.clone()).await?;
        let sk = StateSK::Loaded(SafeKeeper::new(
            TimelineState::new(control_store),
            wal_store,
            conf.my_id,
        )?);

        Ok(Self::new(sk))
    }

    pub(crate) fn get_wal_seg_size(&self) -> usize {
        self.sk.state().server.wal_seg_size as usize
    }

    fn get_safekeeper_info(
        &self,
        _branch_id: &BranchId,
        _conf: &SafeKeeperConf,
        _standby_apply_lsn: Lsn,
    ) -> LocalSkInfo {
        let flush_lsn = self.sk.flush_lsn();
        LocalSkInfo {
            flush_lsn,
            commit_lsn: flush_lsn,
        }
    }

    /// Get our latest view of alive peers status on the timeline.
    /// We pass our own info through the broker as well, so when we don't have connection
    /// to the broker returned vec is empty.
    pub(crate) fn get_peers(&self, heartbeat_timeout: Duration) -> Vec<PeerInfo> {
        let now = Instant::now();
        self.peers_info
            .0
            .iter()
            // Regard peer as absent if we haven't heard from it within heartbeat_timeout.
            .filter(|p| now.duration_since(p.ts) <= heartbeat_timeout)
            .cloned()
            .collect()
    }
}

#[derive(Debug, thiserror::Error)]
pub enum TimelineError {
    #[error("Timeline {0} was cancelled and cannot be used anymore")]
    Cancelled(BranchId),
    #[error("Timeline {0} was not found in global map")]
    NotFound(BranchId),
    #[error("Timeline {0} has been deleted")]
    Deleted(BranchId),
    #[error("Timeline {0} creation is in progress")]
    CreationInProgress(BranchId),
    #[error("Timeline {0} exists on disk, but wasn't loaded on startup")]
    Invalid(BranchId),
    #[error("Timeline {0} is already exists")]
    AlreadyExists(BranchId),
    #[error("Timeline {0} is not initialized, wal_seg_size is zero")]
    UninitializedWalSegSize(BranchId),
    #[error("Timeline {0} is not initialized, pg_version is unknown")]
    UninitialinzedPgVersion(BranchId),
}

// Convert to HTTP API error.
impl From<TimelineError> for ApiError {
    fn from(te: TimelineError) -> ApiError {
        match te {
            TimelineError::NotFound(branch_id) => {
                ApiError::NotFound(anyhow!("timeline {} not found", branch_id).into())
            }
            TimelineError::Deleted(branch_id) => {
                ApiError::NotFound(anyhow!("timeline {} deleted", branch_id).into())
            }
            _ => ApiError::InternalServerError(anyhow!("{}", te)),
        }
    }
}

// Convert to PG query error. NotFound / Deleted route to
// `QueryError::NotFound`, which logs at info!; the controller
// races PS-attach (which starts the walreceiver and IDENTIFY_SYSTEMs us)
// against `POST /v1/tenant/timeline` (which loads the timeline here),
// and the walreceiver retries cheaply once the timeline appears.
// Logging that retry-once race at error! makes a routine event look
// like a bug. Everything else is still a real error.
impl From<TimelineError> for postgres_backend::QueryError {
    fn from(te: TimelineError) -> postgres_backend::QueryError {
        match te {
            TimelineError::NotFound(_) | TimelineError::Deleted(_) => {
                postgres_backend::QueryError::NotFound(te.to_string().into())
            }
            _ => postgres_backend::QueryError::Other(anyhow!("{}", te)),
        }
    }
}

/// We run remote deletion in a background task, this is how it sends its results back.
type RemoteDeletionReceiver = tokio::sync::watch::Receiver<Option<anyhow::Result<()>>>;

/// Timeline struct manages lifecycle (creation, deletion, restore) of a safekeeper timeline.
/// It also holds SharedState and provides mutually exclusive access to it.
pub struct Timeline {
    pub branch_id: BranchId,

    /// Used to broadcast commit_lsn updates to all background jobs.
    commit_lsn_watch_tx: watch::Sender<Lsn>,
    commit_lsn_watch_rx: watch::Receiver<Lsn>,

    /// Broadcasts shared state updates.
    shared_state_version_tx: watch::Sender<usize>,
    shared_state_version_rx: watch::Receiver<usize>,

    /// Safekeeper and other state, that should remain consistent and
    /// synchronized with the disk. This is tokio mutex as we write WAL to disk
    /// while holding it, ensuring that consensus checks are in order.
    mutex: RwLock<SharedState>,
    walsenders: Arc<WalSenders>,
    walreceivers: Arc<WalReceivers>,
    timeline_dir: Utf8PathBuf,
    manager_ctl: ManagerCtl,
    conf: Arc<SafeKeeperConf>,

    pub(crate) yb_wal: Option<Arc<YbWalConn>>,

    remote_deletion: std::sync::Mutex<Option<RemoteDeletionReceiver>>,

    /// Hold this gate from code that depends on the Timeline's non-shut-down state.  While holding
    /// this gate, you must respect [`Timeline::cancel`]
    pub(crate) gate: Gate,

    /// Delete/cancel will trigger this, background tasks should drop out as soon as it fires
    pub(crate) cancel: CancellationToken,

    // timeline_manager controlled state
    /// True iff the timeline is doing useful work: has live computes,
    /// is mid-flush, or the pageserver's `remote_consistent_lsn` is
    /// behind `flush_lsn`. Drives `safekeeper_active_timelines` +
    /// the `safekeeper_timeline_active` gauge. Naming inherited from
    /// upstream Neon's "is this timeline in the broker active set"
    /// flag — yb-light has no broker, but the activity-tracking
    /// semantics are still meaningful.
    pub(crate) broker_active: AtomicBool,
    pub(crate) last_removed_segno: AtomicU64,
    pub(crate) mgr_status: AtomicStatus,
}

impl Timeline {
    /// Constructs a new timeline.
    pub fn new(
        branch_id: BranchId,
        timeline_dir: &Utf8Path,
        shared_state: SharedState,
        conf: Arc<SafeKeeperConf>,
        yb_wal: Option<Arc<YbWalConn>>,
    ) -> Arc<Self> {
        // yb-light: commit_lsn ≡ flush_lsn (single-SK).
        let (commit_lsn_watch_tx, commit_lsn_watch_rx) =
            watch::channel(shared_state.sk.flush_lsn());
        let (shared_state_version_tx, shared_state_version_rx) = watch::channel(0);

        let walreceivers = WalReceivers::new();

        Arc::new(Self {
            branch_id,
            timeline_dir: timeline_dir.to_owned(),
            commit_lsn_watch_tx,
            commit_lsn_watch_rx,
            shared_state_version_tx,
            shared_state_version_rx,
            mutex: RwLock::new(shared_state),
            walsenders: WalSenders::new(walreceivers.clone()),
            walreceivers,
            gate: Default::default(),
            cancel: CancellationToken::default(),
            remote_deletion: std::sync::Mutex::new(None),
            manager_ctl: ManagerCtl::new(),
            conf,
            broker_active: AtomicBool::new(false),
            last_removed_segno: AtomicU64::new(0),
            mgr_status: AtomicStatus::new(),
            yb_wal,
        })
    }

    /// Load existing timeline from disk.
    pub async fn load_timeline(
        conf: Arc<SafeKeeperConf>,
        branch_id: BranchId,
        yb_wal: Option<Arc<YbWalConn>>,
    ) -> Result<Arc<Timeline>> {
        use tracing::Instrument;
        let span = info_span!("load_timeline", branch_id = %branch_id);
        async move {
            let shared_state =
                SharedState::restore(conf.as_ref(), &branch_id, yb_wal.as_ref()).await?;
            let timeline_dir = get_timeline_dir(conf.as_ref(), &branch_id);

            Ok(Timeline::new(
                branch_id,
                &timeline_dir,
                shared_state,
                conf,
                yb_wal,
            ))
        }
        .instrument(span)
        .await
    }

    /// Bootstrap new or existing timeline starting background tasks.
    pub fn bootstrap(
        self: &Arc<Timeline>,
        _shared_state: &mut WriteGuardSharedState<'_>,
        conf: &SafeKeeperConf,
        broker_active_set: Arc<TimelinesSet>,
        yb_wal: Option<Arc<YbWalConn>>,
    ) {
        let (tx, rx) = self.manager_ctl.bootstrap_manager();

        let Ok(gate_guard) = self.gate.enter() else {
            // Init raced with shutdown
            return;
        };

        // Start manager task which will monitor timeline state and update
        // background tasks.
        tokio::spawn({
            let this = self.clone();
            let conf = conf.clone();
            async move {
                let _gate_guard = gate_guard;
                timeline_manager::main_task(
                    ManagerTimeline { tli: this },
                    conf,
                    broker_active_set,
                    tx,
                    rx,
                    yb_wal,
                )
                .await
            }
        });
    }

    /// Cancel the timeline, requesting background activity to stop. Closing
    /// the `self.gate` waits for that.
    pub fn cancel(&self) {
        info!("timeline {} shutting down", self.branch_id);
        self.cancel.cancel();
    }

    /// Background timeline activities (which hold Timeline::gate) will no
    /// longer run once this function completes. `Self::cancel` must have been
    /// already called.
    pub async fn close(&self) {
        assert!(self.cancel.is_cancelled());

        // Wait for any concurrent tasks to stop using this timeline, to avoid e.g. attempts
        // to read deleted files.
        self.gate.close().await;
    }

    /// Delete timeline from disk completely, by removing timeline directory.
    ///
    /// Also deletes WAL in s3. Might fail if e.g. s3 is unavailable, but
    /// deletion API endpoint is retriable.
    ///
    /// Timeline must be in shut-down state (i.e. call [`Self::close`] first)
    pub async fn delete(
        &self,
        shared_state: &mut WriteGuardSharedState<'_>,
        only_local: bool,
    ) -> Result<bool> {
        // Assert that [`Self::close`] was already called
        assert!(self.cancel.is_cancelled());
        assert!(self.gate.close_complete());

        info!("deleting timeline {} from disk", self.branch_id);

        // Close associated FDs. Nobody will be able to touch timeline data once
        // it is cancelled, so WAL storage won't be opened again.
        shared_state.sk.close_wal_store();

        if !only_local {
            self.remote_delete().await?;
        }

        let dir_existed = delete_dir(&self.timeline_dir).await?;
        Ok(dir_existed)
    }

    /// Delete timeline content from remote storage.  If the returned future is dropped,
    /// deletion will continue in the background.
    ///
    /// This function ordinarily spawns a task and stashes a result receiver into [`Self::remote_deletion`].  If
    /// deletion is already happening, it may simply wait for an existing task's result.
    ///
    /// Note: we concurrently delete remote storage data from multiple
    /// safekeepers. That's ok, s3 replies 200 if object doesn't exist and we
    /// do some retries anyway.
    async fn remote_delete(&self) -> Result<()> {
        // We will start a background task to do the deletion, so that it proceeds even if our
        // API request is dropped.  Future requests will see the existing deletion task and wait
        // for it to complete.
        let mut result_rx = {
            let mut remote_deletion_state = self.remote_deletion.lock().unwrap();
            let result_rx = if let Some(result_rx) = remote_deletion_state.as_ref() {
                if let Some(result) = result_rx.borrow().as_ref() {
                    if let Err(e) = result {
                        // A previous remote deletion failed: we will start a new one
                        tracing::error!("remote deletion failed, will retry ({e})");
                        None
                    } else {
                        // A previous remote deletion call already succeeded
                        return Ok(());
                    }
                } else {
                    // Remote deletion is still in flight
                    Some(result_rx.clone())
                }
            } else {
                // Remote deletion was not attempted yet, start it now.
                None
            };

            match result_rx {
                Some(result_rx) => result_rx,
                None => self.start_remote_delete(&mut remote_deletion_state),
            }
        };

        // Wait for a result
        let Ok(result) = result_rx.wait_for(|v| v.is_some()).await else {
            // Unexpected: sender should always send a result before dropping the channel, even if it has an error
            return Err(anyhow::anyhow!(
                "remote deletion task future was dropped without sending a result"
            ));
        };

        result
            .as_ref()
            .expect("We did a wait_for on this being Some above")
            .as_ref()
            .map(|_| ())
            .map_err(|e| anyhow::anyhow!("remote deletion failed: {e}"))
    }

    /// Spawn background task to do remote deletion, return a receiver for its outcome
    fn start_remote_delete(
        &self,
        guard: &mut std::sync::MutexGuard<Option<RemoteDeletionReceiver>>,
    ) -> RemoteDeletionReceiver {
        tracing::info!("starting remote deletion");
        let yb_wal = self.yb_wal.clone();
        let (result_tx, result_rx) = tokio::sync::watch::channel(None);
        let branch_id = self.branch_id;
        tokio::task::spawn(
            async move {
                let r = if let Some(yb_wal) = yb_wal {
                    control_file::YbControlFileStorage::delete(&yb_wal.system_client, &branch_id)
                        .await
                } else {
                    tracing::info!(
                        "skipping remote deletion because yb_storage_conn is not set; this effectively leaks the control-file row"
                    );
                    Ok(())
                };

                if let Err(e) = &r {
                    // Log error here in case nobody ever listens for our result (e.g. dropped API request)
                    tracing::error!("remote deletion failed: {e}");
                }

                // Ignore send results: it's legal for the Timeline to give up waiting for us.
                let _ = result_tx.send(Some(r));
            }
            .instrument(info_span!("remote_delete", branch_id = %self.branch_id)),
        );

        **guard = Some(result_rx.clone());

        result_rx
    }

    /// Returns if timeline is cancelled.
    pub fn is_cancelled(&self) -> bool {
        self.cancel.is_cancelled()
    }

    /// True when this safekeeper is running with `yb_storage_conn`, i.e.
    /// live-WAL writes go through `YbWalStorage`. Used by the WAL acceptor
    /// loop to pick a flush cadence tuned for YB's per-statement cost model,
    /// where batching pays off.
    pub fn is_yb_wal_mode(&self) -> bool {
        self.conf.is_yb_wal_mode()
    }

    /// Take a writing mutual exclusive lock on timeline shared_state.
    pub async fn write_shared_state(self: &Arc<Self>) -> WriteGuardSharedState<'_> {
        WriteGuardSharedState::new(self.clone(), self.mutex.write().await)
    }

    pub async fn read_shared_state(&self) -> ReadGuardSharedState {
        self.mutex.read().await
    }

    /// Returns commit_lsn watch channel.
    pub fn get_commit_lsn_watch_rx(&self) -> watch::Receiver<Lsn> {
        self.commit_lsn_watch_rx.clone()
    }

    /// Watch receiver that fires as more WAL becomes durable in YugabyteDB,
    /// so the acceptor can ack (`AppendResponse`) without blocking ingest on the
    /// YB round trip. `None` for local-FS.
    pub async fn yb_get_durable_watch(
        &self,
    ) -> Option<watch::Receiver<crate::yb_wal_storage::DurableState>> {
        self.read_shared_state().await.sk.yb_durable_watch()
    }

    /// On a durability advance, emit an `AppendResponse` carrying the new
    /// (durability-capped) flush_lsn so walproposer releases waiting commits.
    /// Takes the write guard so its `Drop` re-broadcasts `commit_lsn` to the
    /// local walsenders, and fills hot-standby feedback.
    pub async fn yb_emit_append_response(
        self: &Arc<Self>,
    ) -> Result<Option<AcceptorProposerMessage>> {
        if self.is_cancelled() {
            bail!(TimelineError::Cancelled(self.branch_id));
        }
        let resp = {
            let mut shared_state = self.write_shared_state().await;
            let mut resp = shared_state
                .sk
                .safekeeper()
                .yb_emit_append_response()
                .await?;
            resp.hs_feedback = self.walsenders.get_hotstandby().hs_feedback;
            resp
        };
        Ok(Some(AcceptorProposerMessage::AppendResponse(resp)))
    }

    /// Returns watch channel for SharedState update version.
    pub fn get_state_version_rx(&self) -> watch::Receiver<usize> {
        self.shared_state_version_rx.clone()
    }

    /// Returns wal_seg_size.
    pub async fn get_wal_seg_size(&self) -> usize {
        self.read_shared_state().await.get_wal_seg_size()
    }

    /// Returns state of the timeline.
    pub async fn get_state(&self) -> (TimelineMemState, TimelinePersistentState) {
        let state = self.read_shared_state().await;
        (
            state.sk.state().inmem.clone(),
            TimelinePersistentState::clone(state.sk.state()),
        )
    }

    /// Snapshot of local SK timeline state. Previously broadcast to the
    /// storage broker; retained for `wal_backup_partial` to capture
    /// pre-upload watermarks.
    pub async fn get_safekeeper_info(&self, conf: &SafeKeeperConf) -> LocalSkInfo {
        let standby_apply_lsn = self.walsenders.get_hotstandby().reply.apply_lsn;
        let shared_state = self.read_shared_state().await;
        shared_state.get_safekeeper_info(&self.branch_id, conf, standby_apply_lsn)
    }

    pub async fn get_peers(&self, conf: &SafeKeeperConf) -> Vec<PeerInfo> {
        let shared_state = self.read_shared_state().await;
        shared_state.get_peers(conf.heartbeat_timeout)
    }

    pub fn get_walsenders(&self) -> &Arc<WalSenders> {
        &self.walsenders
    }

    pub fn get_walreceivers(&self) -> &Arc<WalReceivers> {
        &self.walreceivers
    }

    /// Returns flush_lsn.
    pub async fn get_flush_lsn(&self) -> Lsn {
        self.read_shared_state().await.sk.flush_lsn()
    }

    /// Gather timeline data for metrics.
    pub async fn info_for_metrics(&self) -> Option<FullTimelineInfo> {
        if self.is_cancelled() {
            return None;
        }

        let WalSendersTimelineMetricValues {
            ps_feedback_counter,
            ps_corruption_detected,
            last_ps_feedback,
            interpreted_wal_reader_tasks,
        } = self.walsenders.info_for_metrics();

        let state = self.read_shared_state().await;
        Some(FullTimelineInfo {
            branch_id: self.branch_id,
            ps_feedback_count: ps_feedback_counter,
            ps_corruption_detected,
            last_ps_feedback,
            timeline_is_active: self.broker_active.load(Ordering::Relaxed),
            num_computes: self.walreceivers.get_num() as u32,
            last_removed_segno: self.last_removed_segno.load(Ordering::Relaxed),
            interpreted_wal_reader_tasks,
            mem_state: state.sk.state().inmem.clone(),
            persisted_state: TimelinePersistentState::clone(state.sk.state()),
            flush_lsn: state.sk.flush_lsn(),
            wal_storage: state.sk.wal_storage_metrics(),
        })
    }

    /// Returns in-memory timeline state to build a full debug dump.
    pub async fn memory_dump(&self) -> debug_dump::Memory {
        let state = self.read_shared_state().await;

        let (write_lsn, write_record_lsn, flush_lsn, file_open) =
            state.sk.wal_storage_internal_state();

        debug_dump::Memory {
            is_cancelled: self.is_cancelled(),
            peers_info_len: state.peers_info.0.len(),
            walsenders: self.walsenders.get_all_public(),
            active: self.broker_active.load(Ordering::Relaxed),
            num_computes: self.walreceivers.get_num() as u32,
            last_removed_segno: self.last_removed_segno.load(Ordering::Relaxed),
            mem_state: state.sk.state().inmem.clone(),
            mgr_status: self.mgr_status.get(),
            write_lsn,
            write_record_lsn,
            flush_lsn,
            file_open,
        }
    }

    /// Apply a function to the control file state and persist it.
    pub async fn map_control_file<T>(
        self: &Arc<Self>,
        f: impl FnOnce(&mut TimelinePersistentState) -> Result<T>,
    ) -> Result<T> {
        let mut state = self.write_shared_state().await;
        let mut persistent_state = state.sk.state_mut().start_change();
        // If f returns error, we abort the change and don't persist anything.
        let res = f(&mut persistent_state)?;
        // If persisting fails, we abort the change and return error.
        state
            .sk
            .state_mut()
            .finish_change(&persistent_state)
            .await?;
        Ok(res)
    }

    /// Guts of [`Self::wal_residence_guard`] and [`Self::try_wal_residence_guard`]
    async fn do_wal_residence_guard(
        self: &Arc<Self>,
        block: bool,
    ) -> Result<Option<WalResidentTimeline>> {
        let op_label = if block {
            "wal_residence_guard"
        } else {
            "try_wal_residence_guard"
        };

        if self.is_cancelled() {
            bail!(TimelineError::Cancelled(self.branch_id));
        }

        debug!("requesting WalResidentTimeline guard");
        let started_at = Instant::now();
        let status_before = self.mgr_status.get();

        // Wait 30 seconds for the guard to be acquired. It can time out if someone is
        // holding the lock (e.g. during `SafeKeeper::process_msg()`) or manager task
        // is stuck.
        let res = tokio::time::timeout_at(started_at + Duration::from_secs(30), async {
            if block {
                self.manager_ctl.wal_residence_guard().await.map(Some)
            } else {
                self.manager_ctl.try_wal_residence_guard().await
            }
        })
        .await;

        let guard = match res {
            Ok(Ok(guard)) => {
                let finished_at = Instant::now();
                let elapsed = finished_at - started_at;
                MISC_OPERATION_SECONDS
                    .with_label_values(&[op_label])
                    .observe(elapsed.as_secs_f64());

                guard
            }
            Ok(Err(e)) => {
                warn!(
                    "error acquiring in {op_label}, statuses {:?} => {:?}",
                    status_before,
                    self.mgr_status.get()
                );
                return Err(e);
            }
            Err(_) => {
                warn!(
                    "timeout acquiring in {op_label} guard, statuses {:?} => {:?}",
                    status_before,
                    self.mgr_status.get()
                );
                anyhow::bail!("timeout while acquiring WalResidentTimeline guard");
            }
        };

        Ok(guard.map(|g| WalResidentTimeline::new(self.clone(), g)))
    }

    /// Get the timeline guard for reading/writing WAL files.
    /// If WAL files are not present on disk (evicted), they will be automatically
    /// downloaded from remote storage. This is done in the manager task, which is
    /// responsible for issuing all guards.
    ///
    /// NB: don't use this function from timeline_manager, it will deadlock.
    /// NB: don't use this function while holding shared_state lock.
    pub async fn wal_residence_guard(self: &Arc<Self>) -> Result<WalResidentTimeline> {
        self.do_wal_residence_guard(true)
            .await
            .map(|m| m.expect("Always get Some in block=true mode"))
    }

    /// Get the timeline guard for reading/writing WAL files if the timeline is resident,
    /// else return None
    #[allow(dead_code)]
    pub(crate) async fn try_wal_residence_guard(
        self: &Arc<Self>,
    ) -> Result<Option<WalResidentTimeline>> {
        self.do_wal_residence_guard(false).await
    }

    pub async fn backup_partial_reset(self: &Arc<Self>) -> Result<Vec<String>> {
        self.manager_ctl.backup_partial_reset().await
    }
}

/// This is a guard that allows to read/write disk timeline state.
/// All tasks that are trying to read/write WAL from disk should use this guard.
pub struct WalResidentTimeline {
    pub tli: Arc<Timeline>,
    _guard: ResidenceGuard,
}

impl WalResidentTimeline {
    pub fn new(tli: Arc<Timeline>, _guard: ResidenceGuard) -> Self {
        WalResidentTimeline { tli, _guard }
    }
}

impl Deref for WalResidentTimeline {
    type Target = Arc<Timeline>;

    fn deref(&self) -> &Self::Target {
        &self.tli
    }
}

impl WalResidentTimeline {
    /// Returns true if walsender should stop sending WAL to pageserver. We
    /// terminate it if remote_consistent_lsn reached commit_lsn and there is no
    /// computes. While there might be nothing to stream already, we learn about
    /// remote_consistent_lsn update through replication feedback, and we want
    /// to stop pushing to the broker if pageserver is fully caughtup.
    pub async fn should_walsender_stop(&self, reported_remote_consistent_lsn: Lsn) -> bool {
        if self.is_cancelled() {
            return true;
        }
        let shared_state = self.read_shared_state().await;
        if self.walreceivers.get_num() == 0 {
            let flush_lsn = shared_state.sk.flush_lsn();
            return flush_lsn == Lsn(0) || reported_remote_consistent_lsn >= flush_lsn;
        }
        false
    }

    // Check if disk usage by WAL segment files for this timeline exceeds the configured limit.
    fn check_disk_usage(&self, shared_state_locked: &mut WriteGuardSharedState<'_>) -> Result<()> {
        // The disk usage is calculated based on the number of segments between `last_removed_segno`
        // and the current flush LSN segment number. `last_removed_segno` is advanced after
        // unneeded WAL files are physically removed from disk (see `update_wal_removal_end()`
        // in `timeline_manager.rs`).
        let max_timeline_disk_usage_bytes = self.conf.max_timeline_disk_usage_bytes;
        if max_timeline_disk_usage_bytes > 0 {
            let last_removed_segno = self.last_removed_segno.load(Ordering::Relaxed);
            let flush_lsn = shared_state_locked.sk.flush_lsn();
            let wal_seg_size = shared_state_locked.sk.state().server.wal_seg_size as u64;
            let current_segno = flush_lsn.segment_number(wal_seg_size as usize);

            let segno_count = current_segno - last_removed_segno;
            let disk_usage_bytes = segno_count * wal_seg_size;

            if disk_usage_bytes > max_timeline_disk_usage_bytes {
                WAL_STORAGE_LIMIT_ERRORS.inc();
                bail!(
                    "WAL storage utilization exceeds configured limit of {} bytes: current disk usage: {} bytes",
                    max_timeline_disk_usage_bytes,
                    disk_usage_bytes
                );
            }
        }

        if GLOBAL_DISK_LIMIT_EXCEEDED.load(Ordering::Relaxed) {
            bail!("Global disk usage exceeded limit");
        }

        Ok(())
    }

    /// Pass arrived message to the safekeeper.
    pub async fn process_msg(
        &self,
        msg: &ProposerAcceptorMessage,
    ) -> Result<Option<AcceptorProposerMessage>> {
        if self.is_cancelled() {
            bail!(TimelineError::Cancelled(self.branch_id));
        }

        let mut rmsg: Option<AcceptorProposerMessage>;
        {
            let mut shared_state = self.write_shared_state().await;
            // Errors from check_disk_usage() fail process_msg(), which propagates up and terminates the
            // entire WalAcceptor. Postgres will disconnect and keep retrying every second until it can
            // successfully propose WAL again.
            self.check_disk_usage(&mut shared_state)?;
            rmsg = shared_state.sk.safekeeper().process_msg(msg).await?;

            // if this is AppendResponse, fill in proper hot standby feedback.
            if let Some(AcceptorProposerMessage::AppendResponse(ref mut resp)) = rmsg {
                resp.hs_feedback = self.walsenders.get_hotstandby().hs_feedback;
            }
        }
        Ok(rmsg)
    }

    pub async fn get_walreader(
        &self,
        start_lsn: Lsn,
        flush_lsn: Arc<std::sync::atomic::AtomicU64>,
    ) -> Result<WalReader> {
        let (_, persisted_state) = self.get_state().await;

        WalReader::new(
            &self.branch_id,
            &persisted_state,
            start_lsn,
            self.yb_wal.clone(),
            flush_lsn,
        )
    }

    pub fn get_timeline_dir(&self) -> Utf8PathBuf {
        self.timeline_dir.clone()
    }

    /// Update in memory remote consistent lsn.
    pub async fn update_remote_consistent_lsn(&self, candidate: Lsn) {
        let mut shared_state = self.write_shared_state().await;
        shared_state.sk.state_mut().inmem.remote_consistent_lsn = max(
            shared_state.sk.state().inmem.remote_consistent_lsn,
            candidate,
        );
    }
}

/// This struct contains methods that are used by timeline manager task.
pub(crate) struct ManagerTimeline {
    pub(crate) tli: Arc<Timeline>,
}

impl Deref for ManagerTimeline {
    type Target = Arc<Timeline>;

    fn deref(&self) -> &Self::Target {
        &self.tli
    }
}

impl ManagerTimeline {
    #[allow(dead_code)]
    pub(crate) fn timeline_dir(&self) -> &Utf8PathBuf {
        &self.tli.timeline_dir
    }

    /// Update current manager state, useful for debugging manager deadlocks.
    pub(crate) fn set_status(&self, status: timeline_manager::Status) {
        self.mgr_status.store(status, Ordering::Relaxed);
    }
}

/// Deletes directory and it's contents. Returns false if directory does not exist.
pub async fn delete_dir(path: &Utf8PathBuf) -> Result<bool> {
    match fs::remove_dir_all(path).await {
        Ok(_) => Ok(true),
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => Ok(false),
        Err(e) => Err(e.into()),
    }
}

/// Get a path to the timeline directory. If you need to read WAL files from disk,
/// use WalResidentTimeline::get_timeline_dir instead. This function does not check
/// timeline eviction status and WAL files might not be present on disk.
///
/// The path scheme is now flat: `<workdir>/<branch_id>` (the enclosing
/// per-tenant directory was removed along with the tenant/timeline model).
pub fn get_timeline_dir(conf: &SafeKeeperConf, branch_id: &BranchId) -> Utf8PathBuf {
    conf.workdir.join(branch_id.to_string())
}
