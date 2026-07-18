//! The timeline manager task is responsible for managing the timeline's background tasks.
//!
//! It is spawned alongside each timeline and exits when the timeline is deleted.
//! It watches for changes in the timeline state and decides when to spawn or kill background tasks.
//! It also can manage some reactive state, like should the timeline be active for broker pushes or not.
//!
//! Be aware that you need to be extra careful with manager code, because it is not respawned on panic.
//! Also, if it will stuck in some branch, it will prevent any further progress in the timeline.

use std::sync::Arc;
use std::sync::atomic::AtomicUsize;
use std::time::Duration;

use futures::channel::oneshot;
use postgres_ffi::XLogSegNo;
use safekeeper_api::models::PeerInfo;
use serde::{Deserialize, Serialize};
use tokio::task::{JoinError, JoinHandle};
use tokio::time::Instant;
use tracing::{Instrument, debug, info, info_span, instrument, warn};
use utils::lsn::Lsn;

use crate::SafeKeeperConf;
use crate::control_file::{AdvisoryPersist, ControlFileStorage, Storage};
use crate::metrics::{MANAGER_ACTIVE_CHANGES, MANAGER_ITERATIONS_TOTAL, MISC_OPERATION_SECONDS};
use crate::remove_wal::calc_horizon_lsn;
use crate::send_wal::WalSenders;
use crate::state::TimelineState;
use crate::timeline::{ManagerTimeline, ReadGuardSharedState, StateSK, WalResidentTimeline};
use crate::timeline_guard::{AccessService, GuardId, ResidenceGuard};
use crate::timelines_set::{TimelineSetGuard, TimelinesSet};
use crate::yb_wal_storage::YbWalConn;

pub(crate) struct StateSnapshot {
    // inmem values
    pub(crate) remote_consistent_lsn: Lsn,

    // persistent control file values
    pub(crate) cfile_remote_consistent_lsn: Lsn,

    // latest state
    pub(crate) flush_lsn: Lsn,

    // misc
    pub(crate) cfile_last_persist_at: std::time::Instant,
    pub(crate) inmem_flush_pending: bool,
    pub(crate) wal_removal_on_hold: bool,
    #[allow(dead_code)]
    pub(crate) peers: Vec<PeerInfo>,
}

impl StateSnapshot {
    /// Create a new snapshot of the timeline state.
    fn new(read_guard: ReadGuardSharedState, heartbeat_timeout: Duration) -> Self {
        let state = read_guard.sk.state();
        Self {
            remote_consistent_lsn: state.inmem.remote_consistent_lsn,
            cfile_remote_consistent_lsn: state.remote_consistent_lsn,
            flush_lsn: read_guard.sk.flush_lsn(),
            cfile_last_persist_at: state.pers.last_persist_at(),
            inmem_flush_pending: Self::has_unflushed_inmem_state(state),
            wal_removal_on_hold: read_guard.wal_removal_on_hold,
            peers: read_guard.get_peers(heartbeat_timeout),
        }
    }

    fn has_unflushed_inmem_state(state: &TimelineState<ControlFileStorage>) -> bool {
        state.inmem.remote_consistent_lsn > state.remote_consistent_lsn
    }
}

/// Control how often the manager task should wake up to check updates.
/// There is no need to check for updates more often than this.
const REFRESH_INTERVAL: Duration = Duration::from_millis(300);

pub enum ManagerCtlMessage {
    /// Request to get a guard for WalResidentTimeline, with WAL files available locally.
    GuardRequest(tokio::sync::oneshot::Sender<anyhow::Result<ResidenceGuard>>),
    /// Get a guard for WalResidentTimeline if the timeline is not currently offloaded, else None
    TryGuardRequest(tokio::sync::oneshot::Sender<Option<ResidenceGuard>>),
    /// Request to drop the guard.
    GuardDrop(GuardId),
    /// Request to reset uploaded partial backup state.
    BackupPartialReset(oneshot::Sender<anyhow::Result<Vec<String>>>),
}

impl std::fmt::Debug for ManagerCtlMessage {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            ManagerCtlMessage::GuardRequest(_) => write!(f, "GuardRequest"),
            ManagerCtlMessage::TryGuardRequest(_) => write!(f, "TryGuardRequest"),
            ManagerCtlMessage::GuardDrop(id) => write!(f, "GuardDrop({id:?})"),
            ManagerCtlMessage::BackupPartialReset(_) => write!(f, "BackupPartialReset"),
        }
    }
}

pub struct ManagerCtl {
    manager_tx: tokio::sync::mpsc::UnboundedSender<ManagerCtlMessage>,

    // this is used to initialize manager, it will be moved out in bootstrap().
    init_manager_rx:
        std::sync::Mutex<Option<tokio::sync::mpsc::UnboundedReceiver<ManagerCtlMessage>>>,
}

impl Default for ManagerCtl {
    fn default() -> Self {
        Self::new()
    }
}

impl ManagerCtl {
    pub fn new() -> Self {
        let (tx, rx) = tokio::sync::mpsc::unbounded_channel();
        Self {
            manager_tx: tx,
            init_manager_rx: std::sync::Mutex::new(Some(rx)),
        }
    }

    /// Issue a new guard and wait for manager to prepare the timeline.
    /// Sends a message to the manager and waits for the response.
    /// Can be blocked indefinitely if the manager is stuck.
    pub async fn wal_residence_guard(&self) -> anyhow::Result<ResidenceGuard> {
        let (tx, rx) = tokio::sync::oneshot::channel();
        self.manager_tx.send(ManagerCtlMessage::GuardRequest(tx))?;

        // wait for the manager to respond with the guard
        rx.await
            .map_err(|e| anyhow::anyhow!("response read fail: {:?}", e))
            .and_then(std::convert::identity)
    }

    /// Issue a new guard if the timeline is currently not offloaded, else return None
    /// Sends a message to the manager and waits for the response.
    /// Can be blocked indefinitely if the manager is stuck.
    pub async fn try_wal_residence_guard(&self) -> anyhow::Result<Option<ResidenceGuard>> {
        let (tx, rx) = tokio::sync::oneshot::channel();
        self.manager_tx
            .send(ManagerCtlMessage::TryGuardRequest(tx))?;

        // wait for the manager to respond with the guard
        rx.await
            .map_err(|e| anyhow::anyhow!("response read fail: {:?}", e))
    }

    /// Request timeline manager to reset uploaded partial segment state and
    /// wait for the result.
    pub async fn backup_partial_reset(&self) -> anyhow::Result<Vec<String>> {
        let (tx, rx) = oneshot::channel();
        self.manager_tx
            .send(ManagerCtlMessage::BackupPartialReset(tx))
            .expect("manager task is not running");
        match rx.await {
            Ok(res) => res,
            Err(_) => anyhow::bail!("timeline manager is gone"),
        }
    }

    /// Must be called exactly once to bootstrap the manager.
    pub fn bootstrap_manager(
        &self,
    ) -> (
        tokio::sync::mpsc::UnboundedSender<ManagerCtlMessage>,
        tokio::sync::mpsc::UnboundedReceiver<ManagerCtlMessage>,
    ) {
        let rx = self
            .init_manager_rx
            .lock()
            .expect("mutex init_manager_rx poisoned")
            .take()
            .expect("manager already bootstrapped");

        (self.manager_tx.clone(), rx)
    }
}

pub(crate) struct Manager {
    // configuration & dependencies
    pub(crate) tli: ManagerTimeline,
    pub(crate) conf: SafeKeeperConf,
    pub(crate) wal_seg_size: usize,
    pub(crate) walsenders: Arc<WalSenders>,
    #[allow(dead_code)]
    pub(crate) yb_wal: Option<Arc<YbWalConn>>,

    // current state
    pub(crate) state_version_rx: tokio::sync::watch::Receiver<usize>,
    pub(crate) num_computes_rx: tokio::sync::watch::Receiver<usize>,
    pub(crate) tli_broker_active: TimelineSetGuard,
    pub(crate) last_removed_segno: XLogSegNo,

    // background tasks
    pub(crate) wal_removal_task: Option<JoinHandle<anyhow::Result<u64>>>,
    /// In-flight periodic control-file save (at most one). Runs WITHOUT
    /// the shared_state lock — holding that lock across a YB round trip
    /// froze WAL appends and starved residence-guard issuance (2026-07
    /// TPC-C wedge, `UpdateControlFile`). Resolves to the persisted
    /// `remote_consistent_lsn` on success, `None` on failure.
    pub(crate) cfile_save_task: Option<JoinHandle<Option<Lsn>>>,
    /// When the last periodic save was *attempted* (spawned), regardless of
    /// outcome. Gates the next spawn independently of the persisted clock: a
    /// FAILED save never advances the persisted watermark, so without this a
    /// failing save would re-spawn every manager tick (~300 ms) — a retry
    /// storm hammering YB during an outage. With it, a failed save retries at
    /// most once per `control_file_save_interval`. `None` = never attempted.
    cfile_last_attempt: Option<std::time::Instant>,
    /// Lock-free persist handle for the task above.
    cfile_persist: AdvisoryPersist,

    // misc
    pub(crate) access_service: AccessService,
}

/// This task gets spawned alongside each timeline and is responsible for managing the timeline's
/// background tasks.
/// Be careful, this task is not respawned on panic, so it should not panic.
#[instrument(name = "manager", skip_all, fields(branch_id = %tli.branch_id))]
pub async fn main_task(
    tli: ManagerTimeline,
    conf: SafeKeeperConf,
    broker_active_set: Arc<TimelinesSet>,
    manager_tx: tokio::sync::mpsc::UnboundedSender<ManagerCtlMessage>,
    mut manager_rx: tokio::sync::mpsc::UnboundedReceiver<ManagerCtlMessage>,
    yb_wal: Option<Arc<YbWalConn>>,
) {
    tli.set_status(Status::Started);

    let defer_tli = tli.tli.clone();
    scopeguard::defer! {
        if defer_tli.is_cancelled() {
            info!("manager task finished");
        } else {
            warn!("manager task finished prematurely");
        }
    };

    let mut mgr = Manager::new(tli, conf, broker_active_set, manager_tx, yb_wal).await;

    let _last_state = 'outer: loop {
        MANAGER_ITERATIONS_TOTAL.inc();

        mgr.set_status(Status::StateSnapshot);
        let state_snapshot = mgr.state_snapshot().await;

        let mut next_event: Option<Instant> = None;
        {
            let num_computes = *mgr.num_computes_rx.borrow();

            mgr.set_status(Status::UpdateBackup);
            let is_wal_backup_required = mgr.update_backup(num_computes, &state_snapshot).await;
            mgr.update_is_active(is_wal_backup_required, num_computes, &state_snapshot);

            mgr.set_status(Status::UpdateControlFile);
            mgr.update_control_file_save(&state_snapshot, &mut next_event);

            mgr.set_status(Status::UpdateWalRemoval);
            mgr.update_wal_removal(&state_snapshot).await;
        }

        mgr.set_status(Status::Wait);
        // wait until something changes. tx channels are stored under Arc, so they will not be
        // dropped until the manager task is finished.
        tokio::select! {
            _ = mgr.tli.cancel.cancelled() => {
                // timeline was deleted
                break 'outer state_snapshot;
            }
            _ = async {
                // don't wake up on every state change, but at most every REFRESH_INTERVAL
                tokio::time::sleep(REFRESH_INTERVAL).await;
                let _ = mgr.state_version_rx.changed().await;
            } => {
                // state was updated
            }
            _ = mgr.num_computes_rx.changed() => {
                // number of connected computes was updated
            }
            _ = sleep_until(&next_event) => {
                // we were waiting for some event (e.g. cfile save)
            }
            res = await_task_finish(mgr.wal_removal_task.as_mut()) => {
                // WAL removal task finished
                mgr.wal_removal_task = None;
                mgr.update_wal_removal_end(res);
            }
            res = await_task_finish(mgr.cfile_save_task.as_mut()) => {
                // control-file save task finished
                mgr.cfile_save_task = None;
                mgr.update_cfile_save_end(res).await;
            }

            msg = manager_rx.recv() => {
                mgr.set_status(Status::HandleMessage);
                mgr.handle_message(msg).await;
            }
        }
    };
    mgr.set_status(Status::Exiting);

    // remove timeline from the broker active set sooner, before waiting for background tasks
    mgr.tli_broker_active.set(false);

    if let Some(wal_removal_task) = &mut mgr.wal_removal_task {
        let res = wal_removal_task.await;
        mgr.update_wal_removal_end(res);
    }

    // The advisory save is safe to abandon: it's a monotonic UPDATE of a
    // row the timeline-delete flow removes anyway.
    if let Some(cfile_save_task) = &mgr.cfile_save_task {
        cfile_save_task.abort();
    }

    mgr.set_status(Status::Finished);
}

impl Manager {
    async fn new(
        tli: ManagerTimeline,
        conf: SafeKeeperConf,
        broker_active_set: Arc<TimelinesSet>,
        manager_tx: tokio::sync::mpsc::UnboundedSender<ManagerCtlMessage>,
        yb_wal: Option<Arc<YbWalConn>>,
    ) -> Manager {
        let cfile_persist = tli
            .read_shared_state()
            .await
            .sk
            .state()
            .pers
            .advisory_persist_handle();
        Manager {
            wal_seg_size: tli.get_wal_seg_size().await,
            walsenders: tli.get_walsenders().clone(),
            yb_wal,
            state_version_rx: tli.get_state_version_rx(),
            num_computes_rx: tli.get_walreceivers().get_num_rx(),
            tli_broker_active: broker_active_set.guard(tli.clone()),
            last_removed_segno: 0,
            wal_removal_task: None,
            cfile_save_task: None,
            cfile_last_attempt: None,
            cfile_persist,
            access_service: AccessService::new(manager_tx),
            tli,
            conf,
        }
    }

    fn set_status(&self, status: Status) {
        self.tli.set_status(status);
    }

    /// Get a WalResidentTimeline.
    /// Manager code must use this function instead of one from `Timeline`
    /// directly, because it will deadlock.
    ///
    /// This function is fallible because the guard may not be created if the timeline is
    /// shutting down.
    #[allow(dead_code)]
    pub(crate) fn wal_resident_timeline(&mut self) -> anyhow::Result<WalResidentTimeline> {
        let guard = self.access_service.create_guard(
            self.tli
                .gate
                .enter()
                .map_err(|_| anyhow::anyhow!("Timeline shutting down"))?,
        );
        Ok(WalResidentTimeline::new(self.tli.clone(), guard))
    }

    /// Get a snapshot of the timeline state.
    async fn state_snapshot(&self) -> StateSnapshot {
        let _timer = MISC_OPERATION_SECONDS
            .with_label_values(&["state_snapshot"])
            .start_timer();

        StateSnapshot::new(
            self.tli.read_shared_state().await,
            self.conf.heartbeat_timeout,
        )
    }

    /// No-op stub: the S3 WAL upload task was deleted in Phase 9.
    /// Returns `false` so `update_is_active` doesn't keep the
    /// timeline active solely for backup reasons. Kept as a function
    /// so the manager loop's call site doesn't need rewiring; the
    /// `wal_backup_active` flag on `Timeline` stays at its initial
    /// `false` because there is no backup task to track.
    async fn update_backup(&mut self, _num_computes: usize, _state: &StateSnapshot) -> bool {
        false
    }

    /// Update is_active flag and returns its value.
    fn update_is_active(
        &mut self,
        is_wal_backup_required: bool,
        num_computes: usize,
        state: &StateSnapshot,
    ) {
        // yb-amp: commit_lsn ≡ flush_lsn (single-SK quorum).
        let is_active = is_wal_backup_required
            || num_computes > 0
            || state.remote_consistent_lsn < state.flush_lsn;

        // update the broker timeline set
        if self.tli_broker_active.set(is_active) {
            // write log if state has changed
            info!(
                "timeline active={} now, remote_consistent_lsn={}, flush_lsn={}",
                is_active, state.remote_consistent_lsn, state.flush_lsn,
            );

            MANAGER_ACTIVE_CHANGES.inc();
        }

        // update the state in Arc<Timeline>
        self.tli
            .broker_active
            .store(is_active, std::sync::atomic::Ordering::Relaxed);
    }

    /// Kick off the periodic control-file save if needed; sets `next_event`
    /// when the save is due in the future.
    ///
    /// The save runs as a background task holding NO shared_state lock.
    /// This loop is the sole servicer of residence-guard requests, and
    /// WAL appends need the shared_state write lock — the previous
    /// in-loop, under-lock `flush()` meant one slow
    /// `safekeeper.timeline_state` UPDATE froze commits and timed out
    /// every new WAL receiver (2026-07 TPC-C wedge, manager status
    /// `UpdateControlFile`). Only `remote_consistent_lsn` needs this
    /// periodic persist; it goes through a monotonic single-column
    /// UPDATE that is safe to land concurrently with the full-row
    /// consensus persists (see [`AdvisoryPersist`]).
    fn update_control_file_save(
        &mut self,
        state: &StateSnapshot,
        next_event: &mut Option<Instant>,
    ) {
        if !state.inmem_flush_pending || self.cfile_save_task.is_some() {
            return;
        }

        let interval = self.conf.control_file_save_interval;
        // Gate on BOTH the persisted watermark AND the last *attempt*. A failed
        // save never advances the persisted clock, so gating on it alone would
        // re-spawn every manager tick (~300 ms) once the interval first elapses
        // — a retry storm on YB during an outage. The attempt clock, advanced
        // on every spawn (success or failure), bounds retries to one per
        // interval. (This is the async twin of the sync clock-reset guard in
        // control_file.rs.)
        let persist_due = state.cfile_last_persist_at.elapsed() > interval;
        let attempt_due = self
            .cfile_last_attempt
            .is_none_or(|t| t.elapsed() > interval);
        if persist_due && attempt_due {
            self.cfile_last_attempt = Some(std::time::Instant::now());
            let persist = self.cfile_persist.clone();
            let lsn = state.remote_consistent_lsn;
            let branch_id = self.tli.branch_id;
            self.cfile_save_task = Some(tokio::spawn(
                async move {
                    // Simulates the YB write stalling under load (integration
                    // sk_cfile_stall); configure with return(<millis>).
                    utils::failpoint_support::sleep_millis_async!("sk-cfile-save");
                    // Simulates the save FAILING (integration sk_cfile_save_retry_storm),
                    // to exercise the failure-retry gate without a real YB
                    // outage; configure with `return`.
                    fail::fail_point!("sk-cfile-save-fail", |_| {
                        warn!("sk-cfile-save-fail: injected control-file save failure");
                        None
                    });
                    match persist.persist_remote_consistent_lsn(lsn).await {
                        Ok(()) => Some(lsn),
                        Err(e) => {
                            warn!("failed to save control file: {:?}", e);
                            None
                        }
                    }
                }
                .instrument(info_span!("cfile save", branch_id=%branch_id)),
            ));
        } else {
            // Wake when whichever gate clears last has elapsed.
            let mut next = state.cfile_last_persist_at + interval;
            if let Some(t) = self.cfile_last_attempt {
                next = next.max(t + interval);
            }
            update_next_event(next_event, next.into());
        }
    }

    /// Fold a finished control-file save back into the in-memory row
    /// mirror (brief write lock, no I/O under it). A failed save was
    /// already logged by the task; the interval check retries it.
    async fn update_cfile_save_end(&self, res: Result<Option<Lsn>, JoinError>) {
        match res {
            Ok(Some(lsn)) => {
                let mut write_guard = self.tli.write_shared_state().await;
                write_guard.sk.state_mut().pers.note_advisory_persisted(lsn);
            }
            Ok(None) => {}
            Err(e) => warn!("control file save task failed: {:?}", e),
        }
    }

    /// Spawns WAL removal task if needed.
    async fn update_wal_removal(&mut self, state: &StateSnapshot) {
        if self.wal_removal_task.is_some() || state.wal_removal_on_hold {
            // WAL removal is already in progress or hold off
            return;
        }

        // Clamp the removal horizon to the slowest active walsender. WAL
        // lives in YB rows here (no local-disk WAL, no S3 fallback for
        // the pageserver to refetch from), so deleting bytes that an
        // active walsender still needs is a one-way data loss.
        let replication_horizon_lsn = self.walsenders.laggard_lsn();

        let removal_horizon_lsn = calc_horizon_lsn(state, replication_horizon_lsn);
        let removal_horizon_segno = removal_horizon_lsn
            .segment_number(self.wal_seg_size)
            .saturating_sub(1);

        if removal_horizon_segno > self.last_removed_segno {
            // we need to remove WAL
            let Ok(timeline_gate_guard) = self.tli.gate.enter() else {
                tracing::info!("Timeline shutdown, not spawning WAL removal task");
                return;
            };

            let remover = match self.tli.read_shared_state().await.sk {
                StateSK::Loaded(ref sk) => {
                    crate::wal_storage::Storage::remove_up_to(&sk.wal_store, removal_horizon_segno)
                }
                StateSK::Empty => unreachable!(),
            };

            self.wal_removal_task = Some(tokio::spawn(
                async move {
                    let _timeline_gate_guard = timeline_gate_guard;

                    remover.await?;
                    Ok(removal_horizon_segno)
                }
                .instrument(info_span!("WAL removal", branch_id=%self.tli.branch_id)),
            ));
        }
    }

    /// Update the state after WAL removal task finished.
    fn update_wal_removal_end(&mut self, res: Result<anyhow::Result<u64>, JoinError>) {
        let new_last_removed_segno = match res {
            Ok(Ok(segno)) => segno,
            Err(e) => {
                warn!("WAL removal task failed: {:?}", e);
                return;
            }
            Ok(Err(e)) => {
                warn!("WAL removal task failed: {:?}", e);
                return;
            }
        };

        self.last_removed_segno = new_last_removed_segno;
        // update the state in Arc<Timeline>
        self.tli
            .last_removed_segno
            .store(new_last_removed_segno, std::sync::atomic::Ordering::Relaxed);
    }

    /// Handle message arrived from ManagerCtl.
    async fn handle_message(&mut self, msg: Option<ManagerCtlMessage>) {
        debug!("received manager message: {:?}", msg);
        match msg {
            Some(ManagerCtlMessage::GuardRequest(tx)) => {
                let guard = match self.tli.gate.enter() {
                    Ok(gate_guard) => Ok(self.access_service.create_guard(gate_guard)),
                    Err(_) => Err(anyhow::anyhow!(
                        "timeline is shutting down, can't get a guard"
                    )),
                };

                if tx.send(guard).is_err() {
                    warn!("failed to reply with a guard, receiver dropped");
                }
            }
            Some(ManagerCtlMessage::TryGuardRequest(tx)) => {
                let result = match self.tli.gate.enter() {
                    Ok(gate_guard) => Some(self.access_service.create_guard(gate_guard)),
                    Err(_) => None,
                };

                if tx.send(result).is_err() {
                    warn!("failed to reply with a guard, receiver dropped");
                }
            }
            Some(ManagerCtlMessage::GuardDrop(guard_id)) => {
                self.access_service.drop_guard(guard_id);
            }
            Some(ManagerCtlMessage::BackupPartialReset(tx)) => {
                // Partial-backup S3 upload is gone in yb-amp; the
                // reset operation is a no-op. Return an empty segment
                // list so the HTTP route still returns a sensible 200.
                let _ = tx.send(Ok(Vec::new()));
            }
            None => {
                // can't happen, we're holding the sender
                unreachable!();
            }
        }
    }
}

// utility functions
async fn sleep_until(option: &Option<tokio::time::Instant>) {
    if let Some(timeout) = option {
        tokio::time::sleep_until(*timeout).await;
    } else {
        futures::future::pending::<()>().await;
    }
}

/// Future that resolves when the task is finished or never if the task is None.
///
/// Note: it accepts Option<&mut> instead of &mut Option<> because mapping the
/// option to get the latter is hard.
async fn await_task_finish<T>(option: Option<&mut JoinHandle<T>>) -> Result<T, JoinError> {
    if let Some(task) = option {
        task.await
    } else {
        futures::future::pending().await
    }
}

/// Update next_event if candidate is earlier.
fn update_next_event(next_event: &mut Option<Instant>, candidate: Instant) {
    if let Some(next) = next_event {
        if candidate < *next {
            *next = candidate;
        }
    } else {
        *next_event = Some(candidate);
    }
}

#[repr(usize)]
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
pub enum Status {
    NotStarted,
    Started,
    StateSnapshot,
    UpdateBackup,
    UpdateControlFile,
    UpdateWalRemoval,
    Wait,
    HandleMessage,
    Exiting,
    Finished,
}

/// AtomicStatus is a wrapper around AtomicUsize adapted for the Status enum.
pub struct AtomicStatus {
    inner: AtomicUsize,
}

impl Default for AtomicStatus {
    fn default() -> Self {
        Self::new()
    }
}

impl AtomicStatus {
    pub fn new() -> Self {
        AtomicStatus {
            inner: AtomicUsize::new(Status::NotStarted as usize),
        }
    }

    pub fn load(&self, order: std::sync::atomic::Ordering) -> Status {
        // Safety: This line of code uses `std::mem::transmute` to reinterpret the loaded value as `Status`.
        // It is safe to use `transmute` in this context because `Status` is a repr(usize) enum,
        // which means it has the same memory layout as usize.
        // However, it is important to ensure that the loaded value is a valid variant of `Status`,
        // otherwise, the behavior will be undefined.
        unsafe { std::mem::transmute(self.inner.load(order)) }
    }

    pub fn get(&self) -> Status {
        self.load(std::sync::atomic::Ordering::Relaxed)
    }

    pub fn store(&self, val: Status, order: std::sync::atomic::Ordering) {
        self.inner.store(val as usize, order);
    }
}
