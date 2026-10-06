//! Registry for this process's single timeline.
//!
//! The safekeeper serves exactly one branch, fixed at startup by
//! `--branch-id` (see [`SafeKeeperConf::yb_branch_id`]), so this is a single
//! slot rather than a `branch_id -> Arc<Timeline>` map. Durable state lives in
//! YB, so the slot is populated by one keyed read at startup — or on demand by
//! the first walproposer handshake, for a branch whose row doesn't exist yet.

use std::sync::{Arc, Mutex};

use anyhow::{Context, Result, bail};
use safekeeper_api::ServerInfo;
use tracing::*;
use utils::id::BranchId;
use utils::lsn::Lsn;

use crate::state::TimelinePersistentState;
use crate::timeline::{Timeline, TimelineError};
use crate::yb_wal_storage::YbWalConn;
use crate::{SafeKeeperConf, control_file};

/// How long a create waits for the startup load before answering
/// [`TimelineError::StillLoading`] — a 503, which the caller re-sends inside its
/// own budget (`endpoint_supervisor`'s `coordinate.rs`).
///
/// It bounds one request, not the load. Every leg of the load is bounded in its
/// own right (`load_tail` is a fixed count of probes of at most `TAIL_SCAN_ROWS`
/// rows each, floored at the oldest segment GC keeps so they cross no deleted
/// keys, the hole scan decodes at most `MAX_PADDING_PROBES` gaps, each anchor
/// walk is capped at `MAX_ANCHOR_SCAN_BYTES`, the torn-tail cleanup at
/// `YB_TORN_TAIL_CLEANUP_MAX`), so this is not standing in for an unbounded step —
/// it decides how long one caller holds a connection before being told to ask
/// again, and holding it longer buys nothing a re-ask does not.
const LOAD_WAIT: std::time::Duration = std::time::Duration::from_secs(60);

// Timeline entry in the slot: either a ready timeline, or mark that it is
// being created.
#[derive(Clone)]
enum GlobalMapTimeline {
    CreationInProgress,
    Timeline(Arc<Timeline>),
}

struct GlobalTimelinesState {
    /// This process serves a single branch (see
    /// [`SafeKeeperConf::yb_branch_id`]), so the registry is one slot, not a
    /// map — and the slot needs no key, since the identity it would hold is
    /// already fixed by `conf`.
    timeline: Option<GlobalMapTimeline>,

    conf: Arc<SafeKeeperConf>,
    yb_wal: Option<Arc<YbWalConn>>,
}

impl GlobalTimelinesState {
    /// Get dependencies for a timeline constructor.
    fn get_dependencies(&self) -> (Arc<SafeKeeperConf>, Option<Arc<YbWalConn>>) {
        (self.conf.clone(), self.yb_wal.clone())
    }

    /// Whether `branch_id` is the branch this process serves. Requests naming
    /// any other branch are treated as not-found: `amp_storage.branch_state`
    /// is shared cluster-wide, so a misrouted request must not reach our slot.
    fn is_ours(&self, branch_id: &BranchId) -> bool {
        self.conf.branch_id.as_ref() == Some(branch_id)
    }

    /// Get the timeline. Returns error if it isn't ours, doesn't exist, or
    /// creation is in progress.
    fn get(&self, branch_id: &BranchId, loaded: bool) -> Result<Arc<Timeline>, TimelineError> {
        if !self.is_ours(branch_id) {
            return Err(TimelineError::NotFound(*branch_id));
        }
        match self.timeline.clone() {
            Some(GlobalMapTimeline::Timeline(tli)) => Ok(tli),
            Some(GlobalMapTimeline::CreationInProgress) => {
                Err(TimelineError::CreationInProgress(*branch_id))
            }
            // Before the startup load has run, an empty slot means "not read
            // yet". Saying "not found" then is a wrong answer rather than a slow
            // one, and callers treat it as final.
            None if !loaded => Err(TimelineError::StillLoading(*branch_id)),
            None => Err(TimelineError::NotFound(*branch_id)),
        }
    }
}

/// A struct used to manage access to the process's timeline slot.
pub struct GlobalTimelines {
    /// Whether [`GlobalTimelines::init`] has finished.
    ///
    /// The HTTP server starts before the load, so that a write fence can be
    /// pushed to before the load writes anything. Until the load is done, an
    /// empty slot means "not read yet", not "no such branch" — and answering a
    /// lookup with 404 in that window is a wrong answer, not a slow one.
    initialized: std::sync::atomic::AtomicBool,
    state: Mutex<GlobalTimelinesState>,
}

impl GlobalTimelines {
    /// Create a new instance of the timeline registry.
    pub fn new(conf: Arc<SafeKeeperConf>, yb_wal: Option<Arc<YbWalConn>>) -> Self {
        Self {
            initialized: std::sync::atomic::AtomicBool::new(false),
            state: Mutex::new(GlobalTimelinesState {
                timeline: None,
                conf,
                yb_wal,
            }),
        }
    }

    /// Inject dependencies needed for the timeline constructors and load this
    /// process's single branch into memory.
    /// yb-amp's safekeeper persists state in `amp_storage.branch_state`,
    /// so the on-disk workdir holds only the config, log, and pidfile —
    /// there is nothing on disk to scan.
    pub async fn init(&self) -> Result<()> {
        let (has_yb_wal, branch_id) = {
            let state = self.state.lock().unwrap();
            (state.yb_wal.is_some(), state.conf.yb_branch_id()?)
        };
        // Fail fast rather than letting the missing connection surface as a
        // per-timeline load error below.
        anyhow::ensure!(has_yb_wal, "safekeeper init requires yb_storage_conn");

        // We serve exactly one branch, and its id comes from our own config —
        // never from `amp_storage.branch_state`, which is shared across the
        // whole cluster. So this is a single keyed load of our own row, and
        // absence is normal: on a fresh bootstrap the row is written on the
        // first walproposer handshake.
        self.load_one_timeline(branch_id).await;
        info!(
            "loaded {} timelines from amp_storage.branch_state",
            self.state.lock().unwrap().timeline.is_some() as usize
        );
        self.initialized
            .store(true, std::sync::atomic::Ordering::Release);
        Ok(())
    }

    /// Whether the startup load has finished. Callers that would report a
    /// missing timeline have to check this first.
    pub fn is_initialized(&self) -> bool {
        self.initialized.load(std::sync::atomic::Ordering::Acquire)
    }

    /// Load a single timeline by branch_id and register it in the slot.
    /// A missing control-file row leaves the slot empty (the branch is created
    /// on demand by the first walproposer handshake). Errors are logged but
    /// not propagated — a corrupt timeline shouldn't take down the whole
    /// safekeeper at startup.
    async fn load_one_timeline(&self, branch_id: BranchId) {
        let (conf, yb_wal) = {
            let state = self.state.lock().unwrap();
            state.get_dependencies()
        };
        match Timeline::load_timeline(conf.clone(), branch_id, yb_wal.clone()).await {
            Ok(Some(tli)) => {
                let mut shared_state = tli.write_shared_state().await;
                self.state.lock().unwrap().timeline =
                    Some(GlobalMapTimeline::Timeline(tli.clone()));
                tli.bootstrap(&mut shared_state, &conf, yb_wal);
            }
            Ok(None) => {
                info!("no control-file row for {branch_id} yet; awaiting first handshake")
            }
            Err(e) => error!("failed to load timeline {branch_id}, reason: {e:?}"),
        }
    }

    /// Get the number of timelines in the slot.
    pub fn timelines_count(&self) -> usize {
        self.state.lock().unwrap().timeline.is_some() as usize
    }

    /// Get the global safekeeper config.
    pub fn get_global_config(&self) -> Arc<SafeKeeperConf> {
        self.state.lock().unwrap().conf.clone()
    }

    /// Create the timeline, writing its control-file row. If it already exists,
    /// returns the existing timeline.
    ///
    /// The slot is claimed with a `CreationInProgress` marker before the row is
    /// written, so a concurrent creator bails instead of racing us to write it.
    /// The marker is cleared on every failure path.
    pub(crate) async fn create(
        &self,
        branch_id: BranchId,
        server_info: ServerInfo,
        start_lsn: Lsn,
    ) -> Result<Arc<Timeline>> {
        // Wait out the startup load first. HTTP serves before [`Self::init`]
        // runs (so the write fence can be pushed to during it), which re-opens a
        // race the old ordering excluded structurally: a create that lands while
        // the load's read is in flight can have its freshly published timeline
        // overwritten by the load's own copy of the same row. Creation is a
        // once-per-branch event, so waiting a moment is the correct price.
        let load_deadline = tokio::time::Instant::now() + LOAD_WAIT;
        while !self.is_initialized() {
            if tokio::time::Instant::now() >= load_deadline {
                bail!(TimelineError::StillLoading(branch_id));
            }
            tokio::time::sleep(std::time::Duration::from_millis(20)).await;
        }

        let (conf, yb_wal) = {
            let mut state = self.state.lock().unwrap();
            if !state.is_ours(&branch_id) {
                bail!(
                    "refusing to create {branch_id}: this safekeeper serves {:?}",
                    state.conf.branch_id
                );
            }
            match state.timeline {
                // Timeline already exists, return it.
                Some(GlobalMapTimeline::Timeline(ref tli)) => return Ok(tli.clone()),
                Some(GlobalMapTimeline::CreationInProgress) => {
                    bail!(TimelineError::CreationInProgress(branch_id));
                }
                None => {}
            }
            state.timeline = Some(GlobalMapTimeline::CreationInProgress);
            state.get_dependencies()
        };

        info!("creating new timeline {}", branch_id);

        let res = self
            .create_inner(branch_id, server_info, start_lsn, &conf, yb_wal)
            .await;

        if res.is_err() {
            // Creation failed, release the slot so a retry can claim it.
            let mut state = self.state.lock().unwrap();
            debug_assert!(matches!(
                state.timeline,
                Some(GlobalMapTimeline::CreationInProgress)
            ));
            state.timeline = None;
        }
        res
    }

    /// Body of [`Self::create`], run with the slot already claimed. Writes the
    /// control-file row, then loads the timeline back from it and publishes it.
    async fn create_inner(
        &self,
        branch_id: BranchId,
        server_info: ServerInfo,
        start_lsn: Lsn,
        conf: &Arc<SafeKeeperConf>,
        yb_wal: Option<Arc<YbWalConn>>,
    ) -> Result<Arc<Timeline>> {
        let state = TimelinePersistentState::new(&branch_id, server_info, start_lsn)?;
        let yb_wal_conn = yb_wal
            .clone()
            .context("control-file create requires yb_storage_conn")?;
        control_file::ControlFileStorage::write_initial_state(
            yb_wal_conn.system_client.clone(),
            &branch_id,
            state,
        )
        .await?;

        let timeline = Timeline::load_timeline(conf.clone(), branch_id, yb_wal.clone())
            .await?
            .context("control-file row missing right after create")?;

        let mut timeline_shared_state = timeline.write_shared_state().await;
        self.state.lock().unwrap().timeline = Some(GlobalMapTimeline::Timeline(timeline.clone()));
        timeline.bootstrap(&mut timeline_shared_state, conf, yb_wal);
        drop(timeline_shared_state);

        Ok(timeline)
    }

    /// Get the timeline. Returned timeline is always valid, i.e. loaded in
    /// memory and not cancelled.
    pub(crate) fn get(&self, branch_id: BranchId) -> Result<Arc<Timeline>, TimelineError> {
        let tli = self
            .state
            .lock()
            .unwrap()
            .get(&branch_id, self.is_initialized())?;
        if tli.is_cancelled() {
            return Err(TimelineError::Cancelled(branch_id));
        }
        Ok(tli)
    }

    /// Returns the loaded timeline, if any. Shaped as a `Vec` because callers
    /// (metrics, debug dump) iterate it.
    pub fn get_all(&self) -> Vec<Arc<Timeline>> {
        let global_lock = self.state.lock().unwrap();
        global_lock
            .timeline
            .iter()
            .filter_map(|t| match t {
                GlobalMapTimeline::Timeline(t) if !t.is_cancelled() => Some(t.clone()),
                _ => None,
            })
            .collect()
    }
}
