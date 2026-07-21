//! This module contains global `branch_id` -> `Arc<Timeline>` mapping.
//! All timelines should always be present in this map, this is done by loading them
//! all from the disk on startup and keeping them in memory.

use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

use anyhow::{Context, Result, bail};
use camino::Utf8PathBuf;
use camino_tempfile::Utf8TempDir;
use safekeeper_api::ServerInfo;
use safekeeper_api::models::{SafekeeperUtilization, TimelineDeleteResult};
use tracing::*;
use utils::id::BranchId;
use utils::lsn::Lsn;

use crate::state::TimelinePersistentState;
use crate::timeline::{Timeline, TimelineError, delete_dir, get_timeline_dir};
use crate::timelines_set::TimelinesSet;
use crate::yb_wal_storage::YbWalConn;
use crate::{SafeKeeperConf, control_file};

// Timeline entry in the global map: either a ready timeline, or mark that it is
// being created.
#[derive(Clone)]
enum GlobalMapTimeline {
    CreationInProgress,
    Timeline(Arc<Timeline>),
}

struct GlobalTimelinesState {
    /// This process serves a single branch (see
    /// [`SafeKeeperConf::yb_branch_id`]), so the registry is one slot, not a map.
    timeline: Option<(BranchId, GlobalMapTimeline)>,

    /// A tombstone indicates this timeline used to exist and has been deleted. These are used to
    /// prevent on-demand timeline creation from recreating deleted timelines. This is only
    /// soft-enforced, as this slot is dropped on restart.
    timeline_tombstone: Option<(BranchId, TimelineTombstone)>,

    conf: Arc<SafeKeeperConf>,
    broker_active_set: Arc<TimelinesSet>,
    yb_wal: Option<Arc<YbWalConn>>,
}

impl GlobalTimelinesState {
    /// Get dependencies for a timeline constructor.
    fn get_dependencies(
        &self,
    ) -> (
        Arc<SafeKeeperConf>,
        Arc<TimelinesSet>,
        Option<Arc<YbWalConn>>,
    ) {
        (
            self.conf.clone(),
            self.broker_active_set.clone(),
            self.yb_wal.clone(),
        )
    }

    /// Borrow the slot's entry if it is for `branch_id`.
    fn lookup(&self, branch_id: &BranchId) -> Option<&GlobalMapTimeline> {
        self.timeline
            .as_ref()
            .filter(|(k, _)| k == branch_id)
            .map(|(_, v)| v)
    }

    /// Get timeline from the map. Returns error if timeline doesn't exist or
    /// creation is in progress.
    fn get(&self, branch_id: &BranchId) -> Result<Arc<Timeline>, TimelineError> {
        match self.lookup(branch_id).cloned() {
            Some(GlobalMapTimeline::Timeline(tli)) => Ok(tli),
            Some(GlobalMapTimeline::CreationInProgress) => {
                Err(TimelineError::CreationInProgress(*branch_id))
            }
            None => {
                if self.has_tombstone(branch_id) {
                    Err(TimelineError::Deleted(*branch_id))
                } else {
                    Err(TimelineError::NotFound(*branch_id))
                }
            }
        }
    }

    /// Check if the state has a timeline tombstone for `branch_id`.
    fn has_tombstone(&self, branch_id: &BranchId) -> bool {
        self.timeline_tombstone
            .as_ref()
            .is_some_and(|(k, _)| k == branch_id)
    }

    /// Removes timeline tombstone for the given branch ID.
    /// Returns `true` if there have been actual changes.
    fn remove_timeline_tombstone(&mut self, branch_id: &BranchId) -> bool {
        if self
            .timeline_tombstone
            .as_ref()
            .is_some_and(|(k, _)| k == branch_id)
        {
            self.timeline_tombstone = None;
            true
        } else {
            false
        }
    }

    fn delete(&mut self, branch_id: BranchId) {
        if self.timeline.as_ref().is_some_and(|(k, _)| *k == branch_id) {
            self.timeline = None;
        }
        self.timeline_tombstone = Some((branch_id, TimelineTombstone::new()));
    }
}

/// A struct used to manage access to the global timelines map.
pub struct GlobalTimelines {
    state: Mutex<GlobalTimelinesState>,
}

impl GlobalTimelines {
    /// Create a new instance of the global timelines map.
    pub fn new(conf: Arc<SafeKeeperConf>, yb_wal: Option<Arc<YbWalConn>>) -> Self {
        Self {
            state: Mutex::new(GlobalTimelinesState {
                timeline: None,
                timeline_tombstone: None,
                conf,
                broker_active_set: Arc::new(TimelinesSet::default()),
                yb_wal,
            }),
        }
    }

    /// Inject dependencies needed for the timeline constructors and load all timelines to memory.
    /// yb-amp's safekeeper persists state in `safekeeper.timeline_state`,
    /// not on the local filesystem, so the on-disk workdir is only used
    /// for logs and tmp dirs and has no tenant subdirs to scan.
    pub async fn init(&self) -> Result<()> {
        let (yb_wal, branch_id) = {
            let state = self.state.lock().unwrap();
            (state.yb_wal.clone(), state.conf.yb_branch_id()?)
        };
        let yb_wal = yb_wal.context("safekeeper init requires yb_storage_conn")?;

        // `safekeeper.timeline_state` is shared across the whole cluster, so
        // load ONLY our own branch — never every row. Its row is absent on
        // a fresh bootstrap (the branch is created on demand by the first
        // walproposer handshake); load it here only if it already exists.
        let branch_ids =
            control_file::YbControlFileStorage::list_all_branch_ids(&yb_wal.system_client)
                .await
                .context("enumerate safekeeper.timeline_state on init")?;
        if branch_ids.contains(&branch_id) {
            self.load_one_timeline(branch_id).await;
        }
        info!(
            "loaded {} timelines from safekeeper.timeline_state",
            self.state.lock().unwrap().timeline.is_some() as usize
        );
        Ok(())
    }

    /// Load a single timeline by branch_id and register it in the global map.
    /// Errors are logged but not propagated — a single corrupt timeline
    /// shouldn't take down the whole safekeeper at startup.
    async fn load_one_timeline(&self, branch_id: BranchId) {
        let (conf, broker_active_set, yb_wal) = {
            let state = self.state.lock().unwrap();
            state.get_dependencies()
        };
        match Timeline::load_timeline(conf.clone(), branch_id, yb_wal.clone()).await {
            Ok(tli) => {
                let mut shared_state = tli.write_shared_state().await;
                self.state.lock().unwrap().timeline =
                    Some((branch_id, GlobalMapTimeline::Timeline(tli.clone())));
                tli.bootstrap(&mut shared_state, &conf, broker_active_set, yb_wal);
            }
            Err(e) => error!("failed to load timeline {branch_id}, reason: {e:?}"),
        }
    }

    /// Get the number of timelines in the map.
    pub fn timelines_count(&self) -> usize {
        self.state.lock().unwrap().timeline.is_some() as usize
    }

    /// Get the global safekeeper config.
    pub fn get_global_config(&self) -> Arc<SafeKeeperConf> {
        self.state.lock().unwrap().conf.clone()
    }

    pub fn get_global_broker_active_set(&self) -> Arc<TimelinesSet> {
        self.state.lock().unwrap().broker_active_set.clone()
    }

    /// Create a new timeline with the given id. If the timeline already exists, returns
    /// an existing timeline.
    pub(crate) async fn create(
        &self,
        branch_id: BranchId,
        server_info: ServerInfo,
        start_lsn: Lsn,
    ) -> Result<Arc<Timeline>> {
        let (conf, _, yb_wal) = {
            let state = self.state.lock().unwrap();
            if let Ok(timeline) = state.get(&branch_id) {
                // Timeline already exists, return it.
                return Ok(timeline);
            }

            if state.has_tombstone(&branch_id) {
                anyhow::bail!(TimelineError::Deleted(branch_id));
            }

            state.get_dependencies()
        };

        info!("creating new timeline {}", branch_id);

        // Do on disk initialization in tmp dir.
        let (_tmp_dir, tmp_dir_path) = create_temp_timeline_dir(&conf, branch_id).await?;

        // TODO: currently we create only cfile. It would be reasonable to
        // immediately initialize first WAL segment as well.
        let state = TimelinePersistentState::new(&branch_id, server_info, start_lsn)?;
        let yb_wal = yb_wal
            .clone()
            .context("control-file create requires yb_storage_conn")?;
        control_file::ControlFileStorage::create_new(yb_wal.fpu_client.clone(), &branch_id, state)
            .await?;
        let timeline = self.load_temp_timeline(branch_id, &tmp_dir_path).await?;
        Ok(timeline)
    }

    /// Move timeline from a temp directory to the main storage, and load it to
    /// the global map. Creating timeline in this way ensures atomicity: rename
    /// is atomic, so either move of the whole datadir succeeds or it doesn't,
    /// but corrupted data dir shouldn't be possible.
    ///
    /// We'd like to avoid holding map lock while doing IO, so it's a 3 step
    /// process:
    /// 1) check the global map that timeline doesn't exist and mark that we're
    ///    creating it;
    /// 2) move the directory and load the timeline
    /// 3) take lock again and insert the timeline into the global map.
    pub async fn load_temp_timeline(
        &self,
        branch_id: BranchId,
        tmp_path: &Utf8PathBuf,
    ) -> Result<Arc<Timeline>> {
        // Check for existence and mark that we're creating it.
        let (conf, broker_active_set, yb_wal) = {
            let mut state = self.state.lock().unwrap();
            match state.lookup(&branch_id) {
                Some(GlobalMapTimeline::CreationInProgress) => {
                    bail!(TimelineError::CreationInProgress(branch_id));
                }
                Some(GlobalMapTimeline::Timeline(_)) => {
                    bail!(TimelineError::AlreadyExists(branch_id));
                }
                _ => {}
            }

            if state.has_tombstone(&branch_id) {
                // If the timeline is deleted, we refuse to recreate it.
                // This is a safeguard against accidentally overwriting a timeline that was deleted
                // by concurrent request.
                anyhow::bail!(TimelineError::Deleted(branch_id));
            }

            state.remove_timeline_tombstone(&branch_id);

            state.timeline = Some((branch_id, GlobalMapTimeline::CreationInProgress));
            state.get_dependencies()
        };

        // Do the actual move and reflect the result in the map.
        match GlobalTimelines::install_temp_timeline(
            branch_id,
            tmp_path,
            conf.clone(),
            yb_wal.clone(),
        )
        .await
        {
            Ok(timeline) => {
                let mut timeline_shared_state = timeline.write_shared_state().await;
                let mut state = self.state.lock().unwrap();
                assert!(matches!(
                    state.lookup(&branch_id),
                    Some(GlobalMapTimeline::CreationInProgress)
                ));

                state.timeline = Some((branch_id, GlobalMapTimeline::Timeline(timeline.clone())));
                drop(state);
                timeline.bootstrap(&mut timeline_shared_state, &conf, broker_active_set, yb_wal);
                drop(timeline_shared_state);
                Ok(timeline)
            }
            Err(e) => {
                // Init failed, remove the marker from the map
                let mut state = self.state.lock().unwrap();
                assert!(matches!(
                    state.lookup(&branch_id),
                    Some(GlobalMapTimeline::CreationInProgress)
                ));
                state.timeline = None;
                Err(e)
            }
        }
    }

    /// Main part of load_temp_timeline: load the timeline from remote
    /// storage. The `tmp_path` argument is unused in YB mode — the
    /// control-file blob already lives in `GenericRemoteStorage`, so
    /// there's nothing to move locally.
    async fn install_temp_timeline(
        branch_id: BranchId,
        _tmp_path: &Utf8PathBuf,
        conf: Arc<SafeKeeperConf>,
        yb_wal: Option<Arc<YbWalConn>>,
    ) -> Result<Arc<Timeline>> {
        Timeline::load_timeline(conf, branch_id, yb_wal).await
    }

    /// Get a timeline from the global map. If it's not present, it doesn't exist on disk,
    /// or was corrupted and couldn't be loaded on startup. Returned timeline is always valid,
    /// i.e. loaded in memory and not cancelled.
    pub(crate) fn get(&self, branch_id: BranchId) -> Result<Arc<Timeline>, TimelineError> {
        let tli_res = {
            let state = self.state.lock().unwrap();
            state.get(&branch_id)
        };
        match tli_res {
            Ok(tli) => {
                if tli.is_cancelled() {
                    return Err(TimelineError::Cancelled(branch_id));
                }
                Ok(tli)
            }
            _ => tli_res,
        }
    }

    /// Returns all timelines. This is used for background timeline processes.
    pub fn get_all(&self) -> Vec<Arc<Timeline>> {
        let global_lock = self.state.lock().unwrap();
        global_lock
            .timeline
            .iter()
            .filter_map(|(_, t)| match t {
                GlobalMapTimeline::Timeline(t) if !t.is_cancelled() => Some(t.clone()),
                _ => None,
            })
            .collect()
    }

    /// Returns statistics about timeline counts
    pub fn get_timeline_counts(&self) -> SafekeeperUtilization {
        let global_lock = self.state.lock().unwrap();
        let timeline_count = global_lock
            .timeline
            .iter()
            .filter(|(_, t)| match t {
                GlobalMapTimeline::CreationInProgress => false,
                GlobalMapTimeline::Timeline(t) => !t.is_cancelled(),
            })
            .count() as u64;
        SafekeeperUtilization { timeline_count }
    }

    /// Delete timeline, only locally on this node or globally (also cleaning
    /// remote storage WAL), depending on `action` value.
    pub(crate) async fn delete_or_exclude(
        &self,
        branch_id: &BranchId,
        action: DeleteOrExclude,
    ) -> Result<TimelineDeleteResult> {
        let tli_res = {
            let state = self.state.lock().unwrap();

            if state.has_tombstone(branch_id) {
                // Presence of a tombstone guarantees that a previous deletion has completed and there is no work to do.
                info!("Timeline {branch_id} was already deleted");
                return Ok(TimelineDeleteResult { dir_existed: false });
            }

            state.get(branch_id)
        };

        let result = match tli_res {
            Ok(timeline) => {
                info!("deleting timeline {}, action={:?}", branch_id, action);

                timeline.cancel();
                timeline.close().await;

                info!("timeline {branch_id} shut down for deletion");

                let mut shared_state = timeline.write_shared_state().await;

                let only_local = matches!(action, DeleteOrExclude::DeleteLocal);
                let dir_existed = timeline.delete(&mut shared_state, only_local).await?;

                Ok(TimelineDeleteResult { dir_existed })
            }
            Err(_) => {
                // Timeline not in memory, but it may still exist on disk in broken state.
                let dir_path =
                    get_timeline_dir(self.state.lock().unwrap().conf.as_ref(), branch_id);
                let dir_existed = delete_dir(&dir_path).await?;

                Ok(TimelineDeleteResult { dir_existed })
            }
        };

        // Finalize deletion, by dropping Timeline objects and storing smaller tombstones.
        self.state.lock().unwrap().delete(*branch_id);

        result
    }

    pub fn housekeeping(&self, tombstone_ttl: &Duration) {
        let mut state = self.state.lock().unwrap();

        // We keep tombstones long enough to have a good chance of preventing rogue computes from re-creating deleted
        // timelines.  If a compute kept running for longer than this TTL (or across a safekeeper restart) then they
        // may recreate a deleted timeline.
        let now = Instant::now();
        if state
            .timeline_tombstone
            .as_ref()
            .is_some_and(|(_, v)| now.duration_since(v.timestamp) >= *tombstone_ttl)
        {
            state.timeline_tombstone = None;
        }
    }
}

/// Action for delete_or_exclude.
#[derive(Clone, Debug)]
pub enum DeleteOrExclude {
    /// Delete timeline globally.
    Delete,
    /// Delete timeline only locally (don't touch remote storage / YB).
    DeleteLocal,
}

/// Create temp directory for a new timeline. It needs to be located on the same
/// filesystem as the rest of the timelines. It will be automatically deleted when
/// Utf8TempDir goes out of scope.
pub async fn create_temp_timeline_dir(
    conf: &SafeKeeperConf,
    branch_id: BranchId,
) -> Result<(Utf8TempDir, Utf8PathBuf)> {
    let temp_base = conf.workdir.join("tmp");

    tokio::fs::create_dir_all(&temp_base).await?;

    let tli_dir = camino_tempfile::Builder::new()
        .suffix("_temptli")
        .prefix(&format!("{branch_id}_"))
        .tempdir_in(temp_base)?;

    let tli_dir_path = tli_dir.path().to_path_buf();

    Ok((tli_dir, tli_dir_path))
}

/// A tombstone for a deleted timeline. Held only in memory (dropped on
/// restart) to stop on-demand re-creation of a just-deleted timeline.
struct TimelineTombstone {
    timestamp: Instant,
}

impl TimelineTombstone {
    fn new() -> Self {
        TimelineTombstone {
            timestamp: Instant::now(),
        }
    }
}
