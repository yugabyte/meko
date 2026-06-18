//! This module contains global `(tenant_id, timeline_id)` -> `Arc<Timeline>` mapping.
//! All timelines should always be present in this map, this is done by loading them
//! all from the disk on startup and keeping them in memory.

use std::collections::HashMap;
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

use anyhow::{Context, Result, bail};
use camino::Utf8PathBuf;
use camino_tempfile::Utf8TempDir;
use safekeeper_api::ServerInfo;
use safekeeper_api::membership::{Configuration, SafekeeperGeneration};
use safekeeper_api::models::{SafekeeperUtilization, TimelineDeleteResult};
use tracing::*;
use utils::id::{NodeId, TenantId, TenantTimelineId};
use utils::lsn::Lsn;

use crate::state::TimelinePersistentState;
use crate::timeline::{Timeline, TimelineError, delete_dir, get_tenant_dir, get_timeline_dir};
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
    /// This process serves a single (tenant, timeline) (see
    /// [`SafeKeeperConf::yb_ttid`]), so the registry is one slot, not a map.
    timeline: Option<(TenantTimelineId, GlobalMapTimeline)>,

    /// A tombstone indicates this timeline used to exist has been deleted. These are used to prevent
    /// on-demand timeline creation from recreating deleted timelines. This is only soft-enforced, as
    /// this slot is dropped on restart.
    /// The timeline might also be locally deleted (excluded) via safekeeper migration algorithm. In that case,
    /// the tombsone contains the corresponding safekeeper generation. The pull_timeline requests with
    /// higher generation ignore such tombstones and can recreate the timeline.
    timeline_tombstone: Option<(TenantTimelineId, TimelineTombstone)>,
    /// A tombstone indicates that the tenant used to exist has been deleted.
    /// These are created only by tenant_delete requests. They are always valid regardless of the
    /// request generation.
    /// This is only soft-enforced, as this slot is dropped on restart.
    tenant_tombstone: Option<(TenantId, Instant)>,

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

    /// Borrow the slot's entry if it is for `ttid`.
    fn lookup(&self, ttid: &TenantTimelineId) -> Option<&GlobalMapTimeline> {
        self.timeline
            .as_ref()
            .filter(|(k, _)| k == ttid)
            .map(|(_, v)| v)
    }

    /// Get timeline from the map. Returns error if timeline doesn't exist or
    /// creation is in progress.
    fn get(&self, ttid: &TenantTimelineId) -> Result<Arc<Timeline>, TimelineError> {
        match self.lookup(ttid).cloned() {
            Some(GlobalMapTimeline::Timeline(tli)) => Ok(tli),
            Some(GlobalMapTimeline::CreationInProgress) => {
                Err(TimelineError::CreationInProgress(*ttid))
            }
            None => {
                if self.has_tombstone(ttid, None) {
                    Err(TimelineError::Deleted(*ttid))
                } else {
                    Err(TimelineError::NotFound(*ttid))
                }
            }
        }
    }

    fn has_timeline_tombstone(
        &self,
        ttid: &TenantTimelineId,
        generation: Option<SafekeeperGeneration>,
    ) -> bool {
        let tombstone = self
            .timeline_tombstone
            .as_ref()
            .filter(|(k, _)| k == ttid)
            .map(|(_, t)| t);
        if let Some(generation) = generation {
            tombstone.is_some_and(|t| t.is_valid(generation))
        } else {
            tombstone.is_some()
        }
    }

    fn has_tenant_tombstone(&self, tenant_id: &TenantId) -> bool {
        self.tenant_tombstone
            .as_ref()
            .is_some_and(|(k, _)| k == tenant_id)
    }

    /// Check if the state has a tenant or a timeline tombstone.
    /// If `generation` is provided, check only for timeline tombsotnes with same or higher generation.
    /// If `generation` is `None`, check for any timeline tombstone.
    /// Tenant tombstones are checked regardless of the generation.
    fn has_tombstone(
        &self,
        ttid: &TenantTimelineId,
        generation: Option<SafekeeperGeneration>,
    ) -> bool {
        self.has_timeline_tombstone(ttid, generation) || self.has_tenant_tombstone(&ttid.tenant_id)
    }

    /// Removes timeline tombstone for the given timeline ID.
    /// Returns `true` if there have been actual changes.
    fn remove_timeline_tombstone(&mut self, ttid: &TenantTimelineId) -> bool {
        if self
            .timeline_tombstone
            .as_ref()
            .is_some_and(|(k, _)| k == ttid)
        {
            self.timeline_tombstone = None;
            true
        } else {
            false
        }
    }

    fn delete(&mut self, ttid: TenantTimelineId, generation: Option<SafekeeperGeneration>) {
        if self.timeline.as_ref().is_some_and(|(k, _)| *k == ttid) {
            self.timeline = None;
        }
        self.timeline_tombstone = Some((ttid, TimelineTombstone::new(generation)));
    }

    fn add_tenant_tombstone(&mut self, tenant_id: TenantId) {
        self.tenant_tombstone = Some((tenant_id, Instant::now()));
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
                tenant_tombstone: None,
                conf,
                broker_active_set: Arc::new(TimelinesSet::default()),
                yb_wal,
            }),
        }
    }

    /// Inject dependencies needed for the timeline constructors and load all timelines to memory.
    /// yb-light's safekeeper persists state in `safekeeper.timeline_state`,
    /// not on the local filesystem, so the on-disk workdir is only used
    /// for logs and tmp dirs and has no tenant subdirs to scan.
    pub async fn init(&self) -> Result<()> {
        let (yb_wal, ttid) = {
            let state = self.state.lock().unwrap();
            (state.yb_wal.clone(), state.conf.yb_ttid()?)
        };
        let yb_wal = yb_wal.context("safekeeper init requires yb_storage_conn")?;

        // `safekeeper.timeline_state` is shared across the whole cluster, so
        // load ONLY our own timeline — never every row. Its row is absent on
        // a fresh bootstrap (the timeline is created on demand by the first
        // walproposer handshake); load it here only if it already exists.
        let ttids = control_file::YbControlFileStorage::list_all_ttids(&yb_wal.system_client)
            .await
            .context("enumerate safekeeper.timeline_state on init")?;
        if ttids.contains(&ttid) {
            self.load_one_timeline(ttid).await;
        }
        info!(
            "loaded {} timelines from safekeeper.timeline_state",
            self.state.lock().unwrap().timeline.is_some() as usize
        );
        Ok(())
    }

    /// Load a single timeline by ttid and register it in the global map.
    /// Errors are logged but not propagated — a single corrupt timeline
    /// shouldn't take down the whole safekeeper at startup.
    async fn load_one_timeline(&self, ttid: TenantTimelineId) {
        let (conf, broker_active_set, yb_wal) = {
            let state = self.state.lock().unwrap();
            state.get_dependencies()
        };
        match Timeline::load_timeline(conf.clone(), ttid, yb_wal.clone()).await {
            Ok(tli) => {
                let mut shared_state = tli.write_shared_state().await;
                self.state.lock().unwrap().timeline =
                    Some((ttid, GlobalMapTimeline::Timeline(tli.clone())));
                tli.bootstrap(&mut shared_state, &conf, broker_active_set, yb_wal);
            }
            Err(e) => error!("failed to load timeline {ttid}, reason: {e:?}"),
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
        ttid: TenantTimelineId,
        mconf: Configuration,
        server_info: ServerInfo,
        start_lsn: Lsn,
    ) -> Result<Arc<Timeline>> {
        let generation = Some(mconf.generation);

        let (conf, _, yb_wal) = {
            let state = self.state.lock().unwrap();
            if let Ok(timeline) = state.get(&ttid) {
                // Timeline already exists, return it.
                return Ok(timeline);
            }

            if state.has_tombstone(&ttid, generation) {
                anyhow::bail!(TimelineError::Deleted(ttid));
            }

            state.get_dependencies()
        };

        info!("creating new timeline {}", ttid);

        // Do on disk initialization in tmp dir.
        let (_tmp_dir, tmp_dir_path) = create_temp_timeline_dir(&conf, ttid).await?;

        // TODO: currently we create only cfile. It would be reasonable to
        // immediately initialize first WAL segment as well.
        let state = TimelinePersistentState::new(&ttid, mconf, server_info, start_lsn)?;
        let yb_wal = yb_wal
            .clone()
            .context("control-file create requires yb_storage_conn")?;
        control_file::ControlFileStorage::create_new(yb_wal.fpu_client.clone(), &ttid, state)
            .await?;
        let timeline = self
            .load_temp_timeline(ttid, &tmp_dir_path, generation)
            .await?;
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
        ttid: TenantTimelineId,
        tmp_path: &Utf8PathBuf,
        generation: Option<SafekeeperGeneration>,
    ) -> Result<Arc<Timeline>> {
        // Check for existence and mark that we're creating it.
        let (conf, broker_active_set, yb_wal) = {
            let mut state = self.state.lock().unwrap();
            match state.lookup(&ttid) {
                Some(GlobalMapTimeline::CreationInProgress) => {
                    bail!(TimelineError::CreationInProgress(ttid));
                }
                Some(GlobalMapTimeline::Timeline(_)) => {
                    bail!(TimelineError::AlreadyExists(ttid));
                }
                _ => {}
            }

            if state.has_tombstone(&ttid, generation) {
                // If the timeline is deleted, we refuse to recreate it.
                // This is a safeguard against accidentally overwriting a timeline that was deleted
                // by concurrent request.
                anyhow::bail!(TimelineError::Deleted(ttid));
            }

            // We might have an outdated tombstone with the older generation.
            // Remove it unconditionally.
            state.remove_timeline_tombstone(&ttid);

            state.timeline = Some((ttid, GlobalMapTimeline::CreationInProgress));
            state.get_dependencies()
        };

        // Do the actual move and reflect the result in the map.
        match GlobalTimelines::install_temp_timeline(ttid, tmp_path, conf.clone(), yb_wal.clone())
            .await
        {
            Ok(timeline) => {
                let mut timeline_shared_state = timeline.write_shared_state().await;
                let mut state = self.state.lock().unwrap();
                assert!(matches!(
                    state.lookup(&ttid),
                    Some(GlobalMapTimeline::CreationInProgress)
                ));

                state.timeline = Some((ttid, GlobalMapTimeline::Timeline(timeline.clone())));
                drop(state);
                timeline.bootstrap(&mut timeline_shared_state, &conf, broker_active_set, yb_wal);
                drop(timeline_shared_state);
                Ok(timeline)
            }
            Err(e) => {
                // Init failed, remove the marker from the map
                let mut state = self.state.lock().unwrap();
                assert!(matches!(
                    state.lookup(&ttid),
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
        ttid: TenantTimelineId,
        _tmp_path: &Utf8PathBuf,
        conf: Arc<SafeKeeperConf>,
        yb_wal: Option<Arc<YbWalConn>>,
    ) -> Result<Arc<Timeline>> {
        Timeline::load_timeline(conf, ttid, yb_wal).await
    }

    /// Get a timeline from the global map. If it's not present, it doesn't exist on disk,
    /// or was corrupted and couldn't be loaded on startup. Returned timeline is always valid,
    /// i.e. loaded in memory and not cancelled.
    pub(crate) fn get(&self, ttid: TenantTimelineId) -> Result<Arc<Timeline>, TimelineError> {
        let tli_res = {
            let state = self.state.lock().unwrap();
            state.get(&ttid)
        };
        match tli_res {
            Ok(tli) => {
                if tli.is_cancelled() {
                    return Err(TimelineError::Cancelled(ttid));
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

    /// Returns all timelines belonging to a given tenant. Used for deleting all timelines of a tenant,
    /// and that's why it can return cancelled timelines, to retry deleting them.
    fn get_all_for_tenant(&self, tenant_id: TenantId) -> Vec<Arc<Timeline>> {
        let global_lock = self.state.lock().unwrap();
        global_lock
            .timeline
            .iter()
            .filter_map(|(_, t)| match t {
                GlobalMapTimeline::Timeline(t) => Some(t.clone()),
                _ => None,
            })
            .filter(|t| t.ttid.tenant_id == tenant_id)
            .collect()
    }

    /// Delete timeline, only locally on this node or globally (also cleaning
    /// remote storage WAL), depending on `action` value.
    pub(crate) async fn delete_or_exclude(
        &self,
        ttid: &TenantTimelineId,
        action: DeleteOrExclude,
    ) -> Result<TimelineDeleteResult> {
        let tli_res = {
            let state = self.state.lock().unwrap();

            if state.has_timeline_tombstone(ttid, None) {
                // Presence of a tombstone guarantees that a previous deletion has completed and there is no work to do.
                info!("Timeline {ttid} was already deleted");
                return Ok(TimelineDeleteResult { dir_existed: false });
            }

            state.get(ttid)
        };

        let result = match tli_res {
            Ok(timeline) => {
                info!("deleting timeline {}, action={:?}", ttid, action);

                timeline.cancel();
                timeline.close().await;

                info!("timeline {ttid} shut down for deletion");

                let mut shared_state = timeline.write_shared_state().await;

                let only_local = matches!(action, DeleteOrExclude::DeleteLocal);
                let dir_existed = timeline.delete(&mut shared_state, only_local).await?;

                Ok(TimelineDeleteResult { dir_existed })
            }
            Err(_) => {
                // Timeline not in memory, but it may still exist on disk in broken state.
                let dir_path = get_timeline_dir(self.state.lock().unwrap().conf.as_ref(), ttid);
                let dir_existed = delete_dir(&dir_path).await?;

                Ok(TimelineDeleteResult { dir_existed })
            }
        };

        // Finalize deletion, by dropping Timeline objects and storing smaller tombstones.
        self.state.lock().unwrap().delete(*ttid, None);

        result
    }

    /// Deactivates and deletes all timelines for the tenant. Returns map of all timelines which
    /// the tenant had, `true` if a timeline was active. There may be a race if new timelines are
    /// created simultaneously. In that case the function will return error and the caller should
    /// retry tenant deletion again later.
    ///
    /// If only_local, doesn't remove WAL segments in remote storage.
    pub async fn delete_all_for_tenant(
        &self,
        tenant_id: &TenantId,
        action: DeleteOrExclude,
    ) -> Result<HashMap<TenantTimelineId, TimelineDeleteResult>> {
        info!("deleting all timelines for tenant {}", tenant_id);

        // Adding a tombstone before getting the timelines to prevent new timeline additions
        self.state.lock().unwrap().add_tenant_tombstone(*tenant_id);

        let to_delete = self.get_all_for_tenant(*tenant_id);

        let mut err = None;

        let mut deleted = HashMap::new();
        for tli in &to_delete {
            match self.delete_or_exclude(&tli.ttid, action.clone()).await {
                Ok(result) => {
                    deleted.insert(tli.ttid, result);
                }
                Err(e) => {
                    error!("failed to delete timeline {}: {}", tli.ttid, e);
                    // Save error to return later.
                    err = Some(e);
                }
            }
        }

        // If there was an error, return it.
        if let Some(e) = err {
            return Err(e);
        }

        // There may be broken timelines on disk, so delete the whole tenant dir as well.
        // Note that we could concurrently create new timelines while we were deleting them,
        // so the directory may be not empty. In this case timelines will have bad state
        // and timeline background jobs can panic.
        let tenant_dir = get_tenant_dir(self.state.lock().unwrap().conf.as_ref(), tenant_id);
        delete_dir(&tenant_dir).await?;

        Ok(deleted)
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
        if state
            .tenant_tombstone
            .as_ref()
            .is_some_and(|(_, v)| now.duration_since(*v) >= *tombstone_ttl)
        {
            state.tenant_tombstone = None;
        }
    }

    pub fn get_sk_id(&self) -> NodeId {
        self.state.lock().unwrap().conf.my_id
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
    ttid: TenantTimelineId,
) -> Result<(Utf8TempDir, Utf8PathBuf)> {
    let temp_base = conf.workdir.join("tmp");

    tokio::fs::create_dir_all(&temp_base).await?;

    let tli_dir = camino_tempfile::Builder::new()
        .suffix("_temptli")
        .prefix(&format!("{}_{}_", ttid.tenant_id, ttid.timeline_id))
        .tempdir_in(temp_base)?;

    let tli_dir_path = tli_dir.path().to_path_buf();

    Ok((tli_dir, tli_dir_path))
}

/// Validate a freshly-created temp timeline by reading its control-file
/// blob back from remote storage and checking generation bounds.
/// Returns `(commit_lsn, flush_lsn)` which in yb-light both collapse to
/// `timeline_start_lsn` at creation time.
#[allow(dead_code)]
pub async fn validate_temp_timeline(
    _conf: &SafeKeeperConf,
    ttid: TenantTimelineId,
    _path: &Utf8PathBuf,
    generation: Option<SafekeeperGeneration>,
    yb_wal: Option<&Arc<YbWalConn>>,
) -> Result<(Lsn, Lsn)> {
    let yb_wal = yb_wal
        .cloned()
        .context("control-file validate requires yb_storage_conn")?;
    let control_store =
        control_file::ControlFileStorage::restore_new(yb_wal.system_client.clone(), &ttid).await?;
    if control_store.server.wal_seg_size == 0 {
        bail!("wal_seg_size is not set");
    }

    if let Some(generation) = generation {
        if control_store.mconf.generation > generation {
            bail!(
                "tmp timeline generation {} is higher than expected {generation}",
                control_store.mconf.generation
            );
        }
    }

    let lsn = control_store.timeline_start_lsn;
    Ok((lsn, lsn))
}

/// A tombstone for a deleted timeline.
/// The generation is passed with "exclude" request and stored in the tombstone.
/// We ignore the tombstone if the request generation is higher than
/// the tombstone generation.
/// If the tombstone doesn't have a generation, it's considered permanent,
/// e.g. after "delete" request.
struct TimelineTombstone {
    timestamp: Instant,
    generation: Option<SafekeeperGeneration>,
}

impl TimelineTombstone {
    fn new(generation: Option<SafekeeperGeneration>) -> Self {
        TimelineTombstone {
            timestamp: Instant::now(),
            generation,
        }
    }

    /// Check if the timeline is still valid for the given generation.
    fn is_valid(&self, generation: SafekeeperGeneration) -> bool {
        self.generation.is_none_or(|g| g >= generation)
    }
}
