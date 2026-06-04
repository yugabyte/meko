//! Timeline repository implementation that keeps old data in layer files, and
//! the recent changes in ephemeral files.
//!
//! See tenant/*_layer.rs files. The functions here are responsible for locating
//! the correct layer for the get/put call, walking back the timeline branching
//! history as needed.
//!
//! The files are stored in the .neon/tenants/<tenant_id>/timelines/<timeline_id>
//! directory. See docs/pageserver-storage.md for how the files are managed.
//! In addition to the layer files, there is a metadata file in the same
//! directory that contains information about the timeline, in particular its
//! parent timeline, and the last LSN that has been written to disk.
//!

use std::collections::hash_map::Entry;
use std::collections::{BTreeMap, HashMap, HashSet};
use std::fmt::{Debug, Display};
use std::future::Future;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex, Weak};
use std::time::{Duration, Instant, SystemTime};
use std::{fmt, fs};

use anyhow::{Context, bail};
use arc_swap::ArcSwap;
use camino::{Utf8Path, Utf8PathBuf};
use chrono::NaiveDateTime;
use futures::StreamExt;
use futures::stream::FuturesUnordered;
use itertools::Itertools as _;
use once_cell::sync::Lazy;
pub use pageserver_api::models::TenantState;
use pageserver_api::models::{self, RelSizeMigration};
use pageserver_api::models::{
    TimelineArchivalState, TimelineState, TopTenantShardItem,
    WalRedoManagerStatus,
};
use pageserver_api::shard::{ShardIdentity, ShardStripeSize};
use postgres_ffi::PgMajorVersion;
use remote_storage::{DownloadError, GenericRemoteStorage, TimeoutOrCancel};
use remote_timeline_client::index::GcCompactionState;
use remote_timeline_client::manifest::{
    LATEST_TENANT_MANIFEST_VERSION, OffloadedTimelineManifest, TenantManifest,
};
use remote_timeline_client::{FAILED_REMOTE_OP_RETRIES, FAILED_UPLOAD_WARN_THRESHOLD};
use storage_broker::BrokerClientChannel;
use tokio::io::BufReader;
use tokio::sync::{Semaphore, watch};
use tokio_util::sync::CancellationToken;
use tracing::*;
use utils::crashsafe::path_with_suffix_extension;
use utils::sync::gate::{Gate, GateGuard};
use utils::timeout::{TimeoutCancellableError, timeout_cancellable};
use utils::try_rcu::ArcSwapExt;
use utils::zstd::{create_zst_tarball, extract_zst_tarball};
use utils::{backoff, completion, failpoint_support, fs_ext, pausable_failpoint};

use self::config::LocationConf;
use self::metadata::TimelineMetadata;
use self::mgr::{GetActiveTenantError, GetTenantError};
use self::remote_timeline_client::upload::upload_tenant_manifest;
use self::remote_timeline_client::RemoteTimelineClient;
use self::timeline::uninit::{TimelineCreateGuard, TimelineExclusionError, UninitializedTimeline};
use self::timeline::{
    GcCutoffs, TimelineDeleteProgress, TimelineResources, WaitLsnError,
};
use crate::basebackup_cache::BasebackupCache;
use crate::config::PageServerConf;
use crate::context;
use crate::context::RequestContextBuilder;
use crate::context::{DownloadBehavior, RequestContext};
use crate::deletion_queue::{DeletionQueueClient, DeletionQueueError};
use crate::metrics::{
    BROKEN_TENANTS_SET, CONCURRENT_INITDBS,
    INITDB_RUN_TIME, INITDB_SEMAPHORE_ACQUISITION_TIME, TENANT, TENANT_OFFLOADED_TIMELINES,
    TENANT_STATE_METRIC, TIMELINE_STATE_METRIC,
    remove_tenant_metrics,
};
use crate::task_mgr::TaskKind;
use crate::tenant::gc_result::GcResult;
pub use crate::tenant::remote_timeline_client::index::IndexPart;
use crate::tenant::remote_timeline_client::{
    INITDB_PATH, MaybeDeletedIndexPart, remote_initdb_archive_path,
};

use crate::tenant::timeline::delete::DeleteTimelineFlow;
use crate::tenant::timeline::uninit::cleanup_timeline_directory;
use crate::virtual_file::VirtualFile;
use crate::walingest::WalLagCooldown;
use crate::walredo::{PostgresRedoManager, RedoAttemptType};
use crate::{InitializationOrder, TEMP_FILE_SUFFIX, import_datadir, span, task_mgr, walredo};

static INIT_DB_SEMAPHORE: Lazy<Semaphore> = Lazy::new(|| Semaphore::new(8));
use utils::crashsafe;
use utils::generation::Generation;
use utils::id::{TenantId, TimelineId};
use utils::lsn::{Lsn, RecordLsn};


pub mod metadata;
pub mod remote_timeline_client;
pub mod storage_layer;

pub mod config;
pub mod mgr;
pub mod tasks;

pub(crate) mod timeline;


mod gc_block;
mod gc_result;
pub(crate) mod throttle;

pub(crate) use timeline::{PageReconstructError, Timeline};

pub(crate) use crate::span::debug_assert_current_span_has_tenant_and_timeline_id;
// re-export for use in walreceiver
pub use crate::tenant::timeline::WalReceiverInfo;

/// The "tenants" part of `tenants/<tenant>/timelines...`
pub const TENANTS_SEGMENT_NAME: &str = "tenants";

/// Parts of the `.neon/tenants/<tenant_id>/timelines/<timeline_id>` directory prefix.
pub const TIMELINES_SEGMENT_NAME: &str = "timelines";

/// References to shared objects that are passed into each tenant, such
/// as the shared remote storage client and process initialization state.
#[derive(Clone)]
pub struct TenantSharedResources {
    pub broker_client: storage_broker::BrokerClientChannel,
    pub remote_storage: GenericRemoteStorage,
    pub deletion_queue_client: DeletionQueueClient,
    pub basebackup_cache: Arc<BasebackupCache>,
    /// Row-per-page YugabyteDB store dedicated to compute-facing
    /// reads (GetPage hot path: `PageWriter::scan_range` and
    /// `PageWriter::read_chain`). Paired with `fpu_page_store`.
    pub compute_page_store: Arc<crate::page_store::PageStore>,
    /// `PageStore` dedicated to internal traffic: walreceiver
    /// `PageWriter::flush` (parameterised batched INSERT), and the
    /// image materialiser (cache-miss `read_chain_durable` +
    /// `put_pages`). Owning its own YSQL connection keeps walapply
    /// writes and materialiser work off the compute getPage path so
    /// reads stay isolated from internal load.
    pub fpu_page_store: Arc<crate::page_store::PageStore>,
}

/// A [`TenantShard`] is really an _attached_ tenant.  The configuration
/// for an attached tenant is a subset of the [`LocationConf`], represented
/// in this struct.
#[derive(Clone)]
pub(super) struct AttachedTenantConf {
    tenant_conf: pageserver_api::models::TenantConfig,
    /// storage_controller-assigned generation used to fence YB writes.
    pub(super) generation: Generation,
    /// The deadline before which we are blocked from GC so that
    /// leases have a chance to be renewed.
    lsn_lease_deadline: Option<tokio::time::Instant>,
}

impl AttachedTenantConf {
    fn new(
        conf: &'static PageServerConf,
        tenant_conf: pageserver_api::models::TenantConfig,
        generation: Generation,
    ) -> Self {
        // Sets a deadline before which we cannot proceed to GC due to lsn lease.
        // The leases mapping is not persisted to disk; delaying GC by one
        // lease length gives the leases we granted before restart a chance
        // to renew when we run GC for the first time after attaching.
        let lsn_lease_deadline = Some(
            tokio::time::Instant::now()
                + TenantShard::get_lsn_lease_length_impl(conf, &tenant_conf),
        );

        Self {
            tenant_conf,
            generation,
            lsn_lease_deadline,
        }
    }

    fn try_from(
        conf: &'static PageServerConf,
        location_conf: LocationConf,
        tenant_conf: pageserver_api::models::TenantConfig,
    ) -> anyhow::Result<Self> {
        Ok(Self::new(conf, tenant_conf, location_conf.generation))
    }

    fn is_gc_blocked_by_lsn_lease_deadline(&self) -> bool {
        self.lsn_lease_deadline
            .map(|d| tokio::time::Instant::now() < d)
            .unwrap_or(false)
    }
}
struct TimelinePreload {
    index_part: Result<MaybeDeletedIndexPart, DownloadError>,
}

pub(crate) struct TenantPreload {
    /// The tenant manifest from remote storage, or None if no manifest was found.
    tenant_manifest: Option<TenantManifest>,
    /// Map from timeline ID to a possible timeline preload. It is None iff the timeline is offloaded according to the manifest.
    timelines: HashMap<TimelineId, Option<TimelinePreload>>,
}

/// Build a yb-mode `IndexPart` from the `pageserver.timeline_state` row.
/// Layer files don't exist, so the index is empty; the metadata fields
/// that used to live in `index_part.json` ride on the row directly.
fn synth_index_part_from_row(
    row: &crate::page_store::TimelineRow,
) -> anyhow::Result<MaybeDeletedIndexPart> {
    let pg_major = match row.pg_version {
        14 => PgMajorVersion::PG14,
        15 => PgMajorVersion::PG15,
        16 => PgMajorVersion::PG16,
        17 => PgMajorVersion::PG17,
        v => anyhow::bail!("timeline_state.pg_version is {v}, not a supported PG major version"),
    };
    let metadata = crate::tenant::metadata::TimelineMetadata::new(
        row.disk_consistent_lsn,
        // Persisted on flush; see PageWriter::take_prev_at_or_below.
        // NULL falls through as None and the walreceiver re-derives
        // prev from the SK stream.
        row.prev_record_lsn,
        row.ancestor_timeline_id,
        row.ancestor_lsn.unwrap_or(Lsn(0)),
        row.applied_gc_cutoff_lsn,
        // initdb_lsn falls back to disk_consistent_lsn — only consulted
        // by the bootstrap-time image-layer optimisation (dead code in
        // yb-mode) and consumption metrics.
        row.initdb_lsn.unwrap_or(row.disk_consistent_lsn),
        pg_major,
    );
    Ok(MaybeDeletedIndexPart::IndexPart(IndexPart::empty(metadata)))
}

/// When we spawn a tenant, there is a special mode for tenant creation that
/// avoids trying to read anything from remote storage.
pub(crate) enum SpawnMode {
    /// Activate as soon as possible
    Eager,
    /// Lazy activation in the background, with the option to skip the queue if the need comes up
    Lazy,
}

///
/// Tenant consists of multiple timelines. Keep them in a hash table.
///
pub struct TenantShard {
    // Global pageserver config parameters
    pub conf: &'static PageServerConf,

    /// The value creation timestamp, used to measure activation delay, see:
    /// <https://github.com/neondatabase/neon/issues/4025>
    constructed_at: Instant,

    state: watch::Sender<TenantState>,

    // Overridden tenant-specific config parameters.
    // We keep pageserver_api::models::TenantConfig sturct here to preserve the information
    // about parameters that are not set.
    // This is necessary to allow global config updates.
    tenant_conf: Arc<ArcSwap<AttachedTenantConf>>,

    tenant_shard_id: TenantId,

    // The detailed sharding information, beyond the number/count in tenant_shard_id
    shard_identity: ShardIdentity,

    /// The remote storage generation, used to protect S3 objects from split-brain.
    /// Does not change over the lifetime of the [`TenantShard`] object.
    ///
    /// This duplicates the generation stored in LocationConf, but that structure is mutable:
    /// this copy enforces the invariant that generatio doesn't change during a Tenant's lifetime.
    generation: Generation,

    timelines: Mutex<HashMap<TimelineId, Arc<Timeline>>>,

    /// During timeline creation, we first insert the TimelineId to the
    /// creating map, then `timelines`, then remove it from the creating map.
    /// **Lock order**: if acquiring all (or a subset), acquire them in order `timelines`, `timelines_offloaded`, `timelines_creating`
    timelines_creating: std::sync::Mutex<HashSet<TimelineId>>,

    /// Possibly offloaded and archived timelines
    /// **Lock order**: if acquiring all (or a subset), acquire them in order `timelines`, `timelines_offloaded`, `timelines_creating`
    timelines_offloaded: Mutex<HashMap<TimelineId, Arc<OffloadedTimeline>>>,

    /// The last tenant manifest known to be in remote storage. None if the manifest has not yet
    /// been either downloaded or uploaded. Always Some after tenant attach.
    ///
    /// Initially populated during tenant attach, updated via `maybe_upload_tenant_manifest`.
    ///
    /// Do not modify this directly. It is used to check whether a new manifest needs to be
    /// uploaded. The manifest is constructed in `build_tenant_manifest`, and uploaded via
    /// `maybe_upload_tenant_manifest`.
    remote_tenant_manifest: tokio::sync::Mutex<Option<TenantManifest>>,

    // This mutex prevents creation of new timelines during GC.
    // Adding yet another mutex (in addition to `timelines`) is needed because holding
    // `timelines` mutex during all GC iteration
    // may block for a long time `get_timeline`, `get_timelines_state`,... and other operations
    // with timelines, which in turn may cause dropping replication connection, expiration of wait_for_lsn
    // timeout...
    gc_cs: tokio::sync::Mutex<()>,
    walredo_mgr: Option<Arc<WalRedoManager>>,

    /// Provides access to timeline data sitting in the remote storage.
    pub(crate) remote_storage: GenericRemoteStorage,

    /// Access to global deletion queue for when this tenant wants to schedule a deletion.
    deletion_queue_client: DeletionQueueClient,

    /// A channel to send async requests to prepare a basebackup for the basebackup cache.
    basebackup_cache: Arc<BasebackupCache>,

    /// If the tenant is in Activating state, notify this to encourage it
    /// to proceed to Active as soon as possible, rather than waiting for lazy
    /// background warmup.
    pub(crate) activate_now_sem: tokio::sync::Semaphore,

    /// Time it took for the tenant to activate. Zero if not active yet.
    attach_wal_lag_cooldown: Arc<std::sync::OnceLock<WalLagCooldown>>,

    // Cancellation token fires when we have entered shutdown().  This is a parent of
    // Timelines' cancellation token.
    pub(crate) cancel: CancellationToken,

    // Users of the TenantShard such as the page service must take this Gate to avoid
    // trying to use a TenantShard which is shutting down.
    pub(crate) gate: Gate,

    /// Throttle applied at the top of [`Timeline::get`].
    /// All [`TenantShard::timelines`] of a given [`TenantShard`] instance share the same [`throttle::Throttle`] instance.
    pub(crate) pagestream_throttle: Arc<throttle::Throttle>,

    pub(crate) pagestream_throttle_metrics: Arc<crate::metrics::tenant_throttling::Pagestream>,

    /// An ongoing timeline detach concurrency limiter.
    /// `index_part.json` based gc blocking reason tracking.
    ///
    /// New gc iterations must start a new iteration by acquiring `GcBlock::start` before
    /// proceeding.
    pub(crate) gc_block: gc_block::GcBlock,

    /// Compute-reads `PageStore` (see
    /// `TenantSharedResources::compute_page_store`).
    pub(crate) compute_page_store: Arc<crate::page_store::PageStore>,
    /// Internal-traffic `PageStore` (see
    /// `TenantSharedResources::fpu_page_store`).
    pub(crate) fpu_page_store: Arc<crate::page_store::PageStore>,
}
impl std::fmt::Debug for TenantShard {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "{} ({})", self.tenant_shard_id, self.current_state())
    }
}

pub(crate) enum WalRedoManager {
    Prod(WalredoManagerId, PostgresRedoManager),
}

#[derive(thiserror::Error, Debug)]
#[error("pageserver is shutting down")]
pub(crate) struct GlobalShutDown;

impl WalRedoManager {
    pub(crate) fn new(mgr: PostgresRedoManager) -> Result<Arc<Self>, GlobalShutDown> {
        let id = WalredoManagerId::next();
        let arc = Arc::new(Self::Prod(id, mgr));
        let mut guard = WALREDO_MANAGERS.lock().unwrap();
        match &mut *guard {
            Some(map) => {
                map.insert(id, Arc::downgrade(&arc));
                Ok(arc)
            }
            None => Err(GlobalShutDown),
        }
    }
}

impl Drop for WalRedoManager {
    fn drop(&mut self) {
        let Self::Prod(id, _) = self;
        let mut guard = WALREDO_MANAGERS.lock().unwrap();
        if let Some(map) = &mut *guard {
            map.remove(id).expect("new() registers, drop() unregisters");
        }
    }
}

/// Global registry of all walredo managers so that [`crate::shutdown_pageserver`] can shut down
/// the walredo processes outside of the regular order.
///
/// This is necessary to work around a systemd bug where it freezes if there are
/// walredo processes left => <https://github.com/neondatabase/cloud/issues/11387>
#[allow(clippy::type_complexity)]
pub(crate) static WALREDO_MANAGERS: once_cell::sync::Lazy<
    Mutex<Option<HashMap<WalredoManagerId, Weak<WalRedoManager>>>>,
> = once_cell::sync::Lazy::new(|| Mutex::new(Some(HashMap::new())));
#[derive(PartialEq, Eq, Hash, Clone, Copy, Debug)]
pub(crate) struct WalredoManagerId(u64);
impl WalredoManagerId {
    pub fn next() -> Self {
        static NEXT: std::sync::atomic::AtomicU64 = std::sync::atomic::AtomicU64::new(1);
        let id = NEXT.fetch_add(1, std::sync::atomic::Ordering::Relaxed);
        if id == 0 {
            panic!(
                "WalredoManagerId::new() returned 0, indicating wraparound, risking it's no longer unique"
            );
        }
        Self(id)
    }
}

impl WalRedoManager {
    pub(crate) async fn shutdown(&self) -> bool {
        let Self::Prod(_, mgr) = self;
        mgr.shutdown().await
    }

    pub(crate) fn maybe_quiesce(&self, idle_timeout: Duration) {
        let Self::Prod(_, mgr) = self;
        mgr.maybe_quiesce(idle_timeout);
    }

    /// # Cancel-Safety
    ///
    /// This method is cancellation-safe.
    pub async fn request_redo(
        &self,
        key: pageserver_api::key::Key,
        lsn: Lsn,
        base_img: Option<(Lsn, bytes::Bytes)>,
        records: Vec<(Lsn, wal_decoder::models::record::NeonWalRecord)>,
        pg_version: PgMajorVersion,
        redo_attempt_type: RedoAttemptType,
    ) -> Result<bytes::Bytes, walredo::Error> {
        let Self::Prod(_, mgr) = self;
        mgr.request_redo(key, lsn, base_img, records, pg_version, redo_attempt_type)
            .await
    }

    pub(crate) fn status(&self) -> Option<WalRedoManagerStatus> {
        let WalRedoManager::Prod(_, m) = self;
        Some(m.status())
    }
}

/// A very lightweight memory representation of an offloaded timeline.
///
/// We need to store the list of offloaded timelines so that we can perform operations on them,
/// like unoffloading them, or (at a later date), decide to perform flattening.
/// This type has a much smaller memory impact than [`Timeline`], and thus we can store many
/// more offloaded timelines than we can manage ones that aren't.
pub struct OffloadedTimeline {
    pub tenant_shard_id: TenantId,
    pub timeline_id: TimelineId,
    pub ancestor_timeline_id: Option<TimelineId>,
    /// Whether to retain the branch lsn at the ancestor or not
    pub ancestor_retain_lsn: Option<Lsn>,

    /// When the timeline was archived.
    ///
    /// Present for future flattening deliberations.
    pub archived_at: NaiveDateTime,

    /// Prevent two tasks from deleting the timeline at the same time. If held, the
    /// timeline is being deleted. If 'true', the timeline has already been deleted.
    pub delete_progress: TimelineDeleteProgress,

    /// Part of the `OffloadedTimeline` object's lifecycle: this needs to be set before we drop it
    pub deleted_from_ancestor: AtomicBool,

    _metrics_guard: OffloadedTimelineMetricsGuard,
}

/// Increases the offloaded timeline count metric when created, and decreases when dropped.
struct OffloadedTimelineMetricsGuard;

impl OffloadedTimelineMetricsGuard {
    fn new() -> Self {
        TIMELINE_STATE_METRIC
            .with_label_values(&["offloaded"])
            .inc();
        Self
    }
}

impl Drop for OffloadedTimelineMetricsGuard {
    fn drop(&mut self) {
        TIMELINE_STATE_METRIC
            .with_label_values(&["offloaded"])
            .dec();
    }
}

impl OffloadedTimeline {
    /// Obtains an offloaded timeline from a given timeline object.
    ///
    fn from_manifest(tenant_shard_id: TenantId, manifest: &OffloadedTimelineManifest) -> Self {
        // We expect to reach this case in tenant loading, where the `retain_lsn` is populated in the parent's `gc_info`
        // by the `initialize_gc_info` function.
        let OffloadedTimelineManifest {
            timeline_id,
            ancestor_timeline_id,
            ancestor_retain_lsn,
            archived_at,
        } = *manifest;
        Self {
            tenant_shard_id,
            timeline_id,
            ancestor_timeline_id,
            ancestor_retain_lsn,
            archived_at,
            delete_progress: TimelineDeleteProgress::default(),
            deleted_from_ancestor: AtomicBool::new(false),
            _metrics_guard: OffloadedTimelineMetricsGuard::new(),
        }
    }
    fn manifest(&self) -> OffloadedTimelineManifest {
        let Self {
            timeline_id,
            ancestor_timeline_id,
            ancestor_retain_lsn,
            archived_at,
            ..
        } = self;
        OffloadedTimelineManifest {
            timeline_id: *timeline_id,
            ancestor_timeline_id: *ancestor_timeline_id,
            ancestor_retain_lsn: *ancestor_retain_lsn,
            archived_at: *archived_at,
        }
    }
    /// Delete this timeline's retain_lsn from its ancestor, if present in the given tenant
    fn delete_from_ancestor_with_timelines(
        &self,
        timelines: &std::sync::MutexGuard<'_, HashMap<TimelineId, Arc<Timeline>>>,
    ) {
        if let (Some(_retain_lsn), Some(ancestor_timeline_id)) =
            (self.ancestor_retain_lsn, self.ancestor_timeline_id)
        {
            if let Some((_, ancestor_timeline)) = timelines
                .iter()
                .find(|(tid, _tl)| **tid == ancestor_timeline_id)
            {
                let removal_happened = ancestor_timeline
                    .gc_info
                    .write()
                    .unwrap()
                    .remove_child_offloaded(self.timeline_id);
                if !removal_happened {
                    tracing::error!(tenant_id = %self.tenant_shard_id, shard_id = %self.tenant_shard_id.shard_slug(), timeline_id = %self.timeline_id,
                        "Couldn't remove retain_lsn entry from offloaded timeline's parent: already removed");
                }
            }
        }
        self.deleted_from_ancestor.store(true, Ordering::Release);
    }
    /// Call [`Self::delete_from_ancestor_with_timelines`] instead if possible.
    ///
    /// As the entire tenant is being dropped, don't bother deregistering the `retain_lsn` from the ancestor.
    fn defuse_for_tenant_drop(&self) {
        self.deleted_from_ancestor.store(true, Ordering::Release);
    }
}

impl fmt::Debug for OffloadedTimeline {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(f, "OffloadedTimeline<{}>", self.timeline_id)
    }
}

impl Drop for OffloadedTimeline {
    fn drop(&mut self) {
        if !self.deleted_from_ancestor.load(Ordering::Acquire) {
            tracing::warn!(
                "offloaded timeline {} was dropped without having cleaned it up at the ancestor",
                self.timeline_id
            );
        }
    }
}

#[derive(Copy, Clone, PartialEq, Eq, Hash, Debug)]
pub enum MaybeOffloaded {
    Yes,
    No,
}

#[derive(Clone, Debug)]
pub enum TimelineOrOffloaded {
    Timeline(Arc<Timeline>),
    Offloaded(Arc<OffloadedTimeline>),
}

impl TimelineOrOffloaded {
    pub fn arc_ref(&self) -> TimelineOrOffloadedArcRef<'_> {
        match self {
            TimelineOrOffloaded::Timeline(timeline) => {
                TimelineOrOffloadedArcRef::Timeline(timeline)
            }
            TimelineOrOffloaded::Offloaded(offloaded) => {
                TimelineOrOffloadedArcRef::Offloaded(offloaded)
            }
        }
    }
    pub fn tenant_shard_id(&self) -> TenantId {
        self.arc_ref().tenant_shard_id()
    }
    pub fn timeline_id(&self) -> TimelineId {
        self.arc_ref().timeline_id()
    }
    pub fn delete_progress(&self) -> &Arc<tokio::sync::Mutex<DeleteTimelineFlow>> {
        match self {
            TimelineOrOffloaded::Timeline(timeline) => &timeline.delete_progress,
            TimelineOrOffloaded::Offloaded(offloaded) => &offloaded.delete_progress,
        }
    }
    /// Returns true if this variant is a live `Timeline` (vs an
    /// offloaded shell). Used by the delete path to decide whether
    /// the timeline has in-memory state to shut down.
    fn is_live_timeline(&self) -> bool {
        matches!(self, TimelineOrOffloaded::Timeline(_))
    }
}

pub enum TimelineOrOffloadedArcRef<'a> {
    Timeline(&'a Arc<Timeline>),
    Offloaded(&'a Arc<OffloadedTimeline>),
}

impl TimelineOrOffloadedArcRef<'_> {
    pub fn tenant_shard_id(&self) -> TenantId {
        match self {
            TimelineOrOffloadedArcRef::Timeline(timeline) => timeline.tenant_shard_id,
            TimelineOrOffloadedArcRef::Offloaded(offloaded) => offloaded.tenant_shard_id,
        }
    }
    pub fn timeline_id(&self) -> TimelineId {
        match self {
            TimelineOrOffloadedArcRef::Timeline(timeline) => timeline.timeline_id,
            TimelineOrOffloadedArcRef::Offloaded(offloaded) => offloaded.timeline_id,
        }
    }
}

impl<'a> From<&'a Arc<Timeline>> for TimelineOrOffloadedArcRef<'a> {
    fn from(timeline: &'a Arc<Timeline>) -> Self {
        Self::Timeline(timeline)
    }
}

impl<'a> From<&'a Arc<OffloadedTimeline>> for TimelineOrOffloadedArcRef<'a> {
    fn from(timeline: &'a Arc<OffloadedTimeline>) -> Self {
        Self::Offloaded(timeline)
    }
}

#[derive(Debug, thiserror::Error, PartialEq, Eq)]
pub enum GetTimelineError {
    #[error("Timeline is shutting down")]
    ShuttingDown,
    #[error("Timeline {tenant_id}/{timeline_id} is not active, state: {state:?}")]
    NotActive {
        tenant_id: TenantId,
        timeline_id: TimelineId,
        state: TimelineState,
    },
    #[error("Timeline {tenant_id}/{timeline_id} was not found")]
    NotFound {
        tenant_id: TenantId,
        timeline_id: TimelineId,
    },
}

#[derive(Debug, thiserror::Error)]
pub enum LoadLocalTimelineError {
    #[error("FailedToLoad")]
    Load(#[source] anyhow::Error),
    #[error("FailedToResumeDeletion")]
    ResumeDeletion(#[source] anyhow::Error),
}

#[derive(thiserror::Error)]
pub enum DeleteTimelineError {
    #[error("NotFound")]
    NotFound,

    #[error("HasChildren")]
    HasChildren(Vec<TimelineId>),

    #[error("Timeline deletion is already in progress")]
    AlreadyInProgress(Arc<tokio::sync::Mutex<DeleteTimelineFlow>>),

    #[error("Cancelled")]
    Cancelled,

    #[error(transparent)]
    Other(#[from] anyhow::Error),
}

impl Debug for DeleteTimelineError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            Self::NotFound => write!(f, "NotFound"),
            Self::HasChildren(c) => f.debug_tuple("HasChildren").field(c).finish(),
            Self::AlreadyInProgress(_) => f.debug_tuple("AlreadyInProgress").finish(),
            Self::Cancelled => f.debug_tuple("Cancelled").finish(),
            Self::Other(e) => f.debug_tuple("Other").field(e).finish(),
        }
    }
}

#[derive(thiserror::Error)]
pub enum TimelineArchivalError {
    #[error("NotFound")]
    NotFound,

    #[error("Timeout")]
    Timeout,

    #[error("Cancelled")]
    Cancelled,

    #[error("ancestor is archived: {}", .0)]
    HasArchivedParent(TimelineId),

    #[error("HasUnarchivedChildren")]
    HasUnarchivedChildren(Vec<TimelineId>),

    #[error("Timeline archival is already in progress")]
    AlreadyInProgress,

    #[error(transparent)]
    Other(anyhow::Error),
}

#[derive(thiserror::Error, Debug)]
pub(crate) enum TenantManifestError {
    #[error("Remote storage error: {0}")]
    RemoteStorage(anyhow::Error),

    #[error("Cancelled")]
    Cancelled,
}

impl From<TenantManifestError> for TimelineArchivalError {
    fn from(e: TenantManifestError) -> Self {
        match e {
            TenantManifestError::RemoteStorage(e) => Self::Other(e),
            TenantManifestError::Cancelled => Self::Cancelled,
        }
    }
}

impl Debug for TimelineArchivalError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            Self::NotFound => write!(f, "NotFound"),
            Self::Timeout => write!(f, "Timeout"),
            Self::Cancelled => write!(f, "Cancelled"),
            Self::HasArchivedParent(p) => f.debug_tuple("HasArchivedParent").field(p).finish(),
            Self::HasUnarchivedChildren(c) => {
                f.debug_tuple("HasUnarchivedChildren").field(c).finish()
            }
            Self::AlreadyInProgress => f.debug_tuple("AlreadyInProgress").finish(),
            Self::Other(e) => f.debug_tuple("Other").field(e).finish(),
        }
    }
}

pub enum SetStoppingError {
    AlreadyStopping(completion::Barrier),
    Broken,
}

impl Debug for SetStoppingError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            Self::AlreadyStopping(_) => f.debug_tuple("AlreadyStopping").finish(),
            Self::Broken => write!(f, "Broken"),
        }
    }
}

/// Arguments to [`TenantShard::create_timeline`].
///
/// Not usable as an idempotency key for timeline creation because if [`CreateTimelineParamsBranch::ancestor_start_lsn`]
/// is `None`, the result of the timeline create call is not deterministic.
///
/// See [`CreateTimelineIdempotency`] for an idempotency key.
#[derive(Debug)]
pub(crate) enum CreateTimelineParams {
    Bootstrap(CreateTimelineParamsBootstrap),
    Branch(CreateTimelineParamsBranch),
}

#[derive(Debug)]
pub(crate) struct CreateTimelineParamsBootstrap {
    pub(crate) new_timeline_id: TimelineId,
    pub(crate) existing_initdb_timeline_id: Option<TimelineId>,
    pub(crate) pg_version: PgMajorVersion,
}

/// NB: See comment on [`CreateTimelineIdempotency::Branch`] for why there's no `pg_version` here.
#[derive(Debug)]
pub(crate) struct CreateTimelineParamsBranch {
    pub(crate) new_timeline_id: TimelineId,
    pub(crate) ancestor_timeline_id: TimelineId,
    pub(crate) ancestor_start_lsn: Option<Lsn>,
}

/// What is used to determine idempotency of a [`TenantShard::create_timeline`] call in  [`TenantShard::start_creating_timeline`] in  [`TenantShard::start_creating_timeline`].
///
/// Each [`Timeline`] object holds [`Self`] as an immutable property in [`Timeline::create_idempotency`].
///
/// We lower timeline creation requests to [`Self`], and then use [`PartialEq::eq`] to compare [`Timeline::create_idempotency`] with the request.
/// If they are equal, we return a reference to the existing timeline, otherwise it's an idempotency conflict.
///
/// There is special treatment for [`Self::FailWithConflict`] to always return an idempotency conflict.
/// It would be nice to have more advanced derive macros to make that special treatment declarative.
///
/// Notes:
/// - Unlike [`CreateTimelineParams`], ancestor LSN is fixed, so, branching will be at a deterministic LSN.
/// - We make some trade-offs though, e.g., [`CreateTimelineParamsBootstrap::existing_initdb_timeline_id`]
///   is not considered for idempotency. We can improve on this over time if we deem it necessary.
///
#[derive(Debug, Clone, PartialEq, Eq)]
pub(crate) enum CreateTimelineIdempotency {
    /// NB: special treatment, see comment in [`Self`].
    FailWithConflict,
    Bootstrap {
        pg_version: PgMajorVersion,
    },
    /// NB: branches always have the same `pg_version` as their ancestor.
    /// While [`pageserver_api::models::TimelineCreateRequestMode::Branch::pg_version`]
    /// exists as a field, and is set by cplane, it has always been ignored by pageserver when
    /// determining the child branch pg_version.
    Branch {
        ancestor_timeline_id: TimelineId,
        ancestor_start_lsn: Lsn,
    },
}

/// What is returned by [`TenantShard::start_creating_timeline`].
#[must_use]
enum StartCreatingTimelineResult {
    CreateGuard(TimelineCreateGuard),
    Idempotent(Arc<Timeline>),
}

enum TimelineInitAndSyncResult {
    ReadyToActivate,
}

/// What is returned by [`TenantShard::create_timeline`].
enum CreateTimelineResult {
    Created(Arc<Timeline>),
    Idempotent(Arc<Timeline>),
}

impl CreateTimelineResult {
    fn discriminant(&self) -> &'static str {
        match self {
            Self::Created(_) => "Created",
            Self::Idempotent(_) => "Idempotent",
        }
    }
}

#[derive(thiserror::Error, Debug)]
pub enum CreateTimelineError {
    #[error("creation of timeline with the given ID is in progress")]
    AlreadyCreating,
    #[error("timeline already exists with different parameters")]
    Conflict,
    #[error(transparent)]
    AncestorLsn(anyhow::Error),
    #[error("ancestor timeline is not active")]
    AncestorNotActive,
    #[error("ancestor timeline is archived")]
    AncestorArchived,
    #[error("tenant shutting down")]
    ShuttingDown,
    #[error(transparent)]
    Other(#[from] anyhow::Error),
}

#[derive(thiserror::Error, Debug)]
pub enum InitdbError {
    #[error("Operation was cancelled")]
    Cancelled,
    #[error(transparent)]
    Other(anyhow::Error),
    #[error(transparent)]
    Inner(postgres_initdb::Error),
}

#[allow(clippy::large_enum_variant, reason = "TODO")]
enum LoadTimelineCause {
    Attach,
    Unoffload,
}

#[derive(thiserror::Error, Debug)]
pub(crate) enum GcError {
    // The tenant is shutting down
    #[error("tenant shutting down")]
    TenantCancelled,

    // The tenant is shutting down
    #[error("timeline shutting down")]
    TimelineCancelled,

    // The tenant is in a state inelegible to run GC
    #[error("not active")]
    NotActive,

    // An error reading while calculating GC cutoffs
    #[error(transparent)]
    GcCutoffs(PageReconstructError),

    // If GC was invoked for a particular timeline, this error means it didn't exist
    #[error("timeline not found")]
    TimelineNotFound,
}

impl From<PageReconstructError> for GcError {
    fn from(value: PageReconstructError) -> Self {
        match value {
            PageReconstructError::Cancelled => Self::TimelineCancelled,
            other => Self::GcCutoffs(other),
        }
    }
}

#[derive(thiserror::Error, Debug)]
pub(crate) enum LoadConfigError {
    #[error("TOML deserialization error: '{0}'")]
    DeserializeToml(#[from] toml_edit::de::Error),

    #[error("Config not found at {0}")]
    NotFound(Utf8PathBuf),
}

impl TenantShard {
    /// Yet another helper for timeline initialization.
    ///
    /// - Initializes the Timeline struct and inserts it into the tenant's hash map
    /// - Scans the local timeline directory for layer files and builds the layer map
    /// - Downloads remote index file and adds remote files to the layer map
    /// - Schedules remote upload tasks for any files that are present locally but missing from remote storage.
    ///
    /// If the operation fails, the timeline is left in the tenant's hash map in Broken state. On success,
    /// it is marked as Active.
    #[allow(clippy::too_many_arguments)]
    async fn timeline_init_and_sync(
        self: &Arc<Self>,
        timeline_id: TimelineId,
        resources: TimelineResources,
        index_part: IndexPart,
        metadata: TimelineMetadata,
        ancestor: Option<Arc<Timeline>>,
        cause: LoadTimelineCause,
        ctx: &RequestContext,
    ) -> anyhow::Result<TimelineInitAndSyncResult> {
        let tenant_id = self.tenant_shard_id;

        let idempotency = if metadata.ancestor_timeline().is_none() {
            CreateTimelineIdempotency::Bootstrap {
                pg_version: metadata.pg_version(),
            }
        } else {
            CreateTimelineIdempotency::Branch {
                ancestor_timeline_id: metadata.ancestor_timeline().unwrap(),
                ancestor_start_lsn: metadata.ancestor_lsn(),
            }
        };

        let (timeline, _timeline_ctx) = self.create_timeline_struct(
            timeline_id,
            &metadata,
            ancestor.clone(),
            resources,
            idempotency.clone(),
            index_part.gc_compaction.clone(),
            index_part.rel_size_migration.clone(),
            index_part.rel_size_migrated_at,
            ctx,
        )?;
        let disk_consistent_lsn = timeline.get_disk_consistent_lsn();

        if !disk_consistent_lsn.is_valid() {
            anyhow::bail!("Timeline {tenant_id}/{timeline_id} has invalid disk_consistent_lsn");
        }

        assert_eq!(
            disk_consistent_lsn,
            metadata.disk_consistent_lsn(),
            "these are used interchangeably"
        );

        // yb-mode: no upload queue to init. The PageWriter takes the
        // `remote_state.rc_lsn` slot directly so flush success bumps
        // the safekeeper-truncation bound.
        let _ = &index_part;
        if let Some(pw) = timeline.page_writer.as_ref() {
            pw.set_remote_consistent_lsn_slot(timeline.remote_state.rc_lsn.clone());
        }

        // Eager push of the controller-issued generation to YB. The
        // storage controller bumps `tenant_shards.generation` on
        // every attach (including re-attach to the same PS after a
        // restart) but doesn't propagate the new value to YB itself
        // — this is where it lands. The flush path's
        // `WHERE current_generation = $our_gen` fence is strict, so
        // the invariant "YB column == our_gen at all times after
        // attach" must hold from the first flush onwards. This call
        // establishes that invariant. If a newer pageserver has
        // already bumped past us, the CAS errors out and the attach
        // is refused.
        timeline.bump_generation_on_attach().await?;

        let _ = cause;

        {
            let mut timelines_accessor = self.timelines.lock().unwrap();
            match timelines_accessor.entry(timeline_id) {
                // We should never try and load the same timeline twice during startup
                Entry::Occupied(_) => {
                    unreachable!(
                        "Timeline {tenant_id}/{timeline_id} already exists in the tenant map"
                    );
                }
                Entry::Vacant(v) => {
                    v.insert(Arc::clone(&timeline));
                    timeline.maybe_spawn_flush_loop();
                }
            }
        }

        // yb-pageserver: page data lives in pageserver.pages /
        // slru_pages / meta, never in layer files. A timeline with
        // no ancestor and no layer files is the normal post-restart
        // state.

        Ok(TimelineInitAndSyncResult::ReadyToActivate)
    }

    /// Attach a tenant that's available in cloud storage.
    ///
    /// This returns quickly, after just creating the in-memory object
    /// Tenant struct and launching a background task to download
    /// the remote index files.  On return, the tenant is most likely still in
    /// Attaching state, and it will become Active once the background task
    /// finishes. You can use wait_until_active() to wait for the task to
    /// complete.
    ///
    #[allow(clippy::too_many_arguments)]
    pub(crate) fn spawn(
        conf: &'static PageServerConf,
        tenant_shard_id: TenantId,
        resources: TenantSharedResources,
        attached_conf: AttachedTenantConf,
        shard_identity: ShardIdentity,
        init_order: Option<InitializationOrder>,
        mode: SpawnMode,
        ctx: &RequestContext,
    ) -> Result<Arc<TenantShard>, GlobalShutDown> {
        let wal_redo_manager =
            WalRedoManager::new(PostgresRedoManager::new(conf, tenant_shard_id))?;

        let TenantSharedResources {
            broker_client,
            remote_storage,
            deletion_queue_client,
            basebackup_cache,
            compute_page_store,
            fpu_page_store,
        } = resources;

        let generation = attached_conf.generation;

        let tenant = Arc::new(TenantShard::new(
            TenantState::Attaching,
            conf,
            attached_conf,
            shard_identity,
            Some(wal_redo_manager),
            tenant_shard_id,
            remote_storage.clone(),
            deletion_queue_client,
            basebackup_cache,
            compute_page_store,
            fpu_page_store,
        ));

        // The attach task will carry a GateGuard, so that shutdown() reliably waits for it to drop out if
        // we shut down while attaching.
        let attach_gate_guard = tenant
            .gate
            .enter()
            .expect("We just created the TenantShard: nothing else can have shut it down yet");

        // Do all the hard work in the background
        let tenant_clone = Arc::clone(&tenant);
        let ctx = ctx.detached_child(TaskKind::Attach, DownloadBehavior::Warn);
        task_mgr::spawn(
            &tokio::runtime::Handle::current(),
            TaskKind::Attach,
            tenant_shard_id,
            None,
            "attach tenant",
            async move {

                info!("Attaching tenant");

                let _gate_guard = attach_gate_guard;

                // Is this tenant being spawned as part of process startup?
                let starting_up = init_order.is_some();
                scopeguard::defer! {
                    if starting_up {
                        TENANT.startup_complete.inc();
                    }
                }

                fn make_broken_or_stopping(t: &TenantShard, err: anyhow::Error) {
                    t.state.send_modify(|state| match state {
                        // TODO: the old code alluded to DeleteTenantFlow sometimes setting
                        // TenantState::Stopping before we get here, but this may be outdated.
                        // Let's find out with a testing assertion. If this doesn't fire, and the
                        // logs don't show this happening in production, remove the Stopping cases.
                        TenantState::Stopping{..} if cfg!(any(test, feature = "testing")) => {
                            panic!("unexpected TenantState::Stopping during attach")
                        }
                        // If the tenant is cancelled, assume the error was caused by cancellation.
                        TenantState::Attaching if t.cancel.is_cancelled() => {
                            info!("attach cancelled, setting tenant state to Stopping: {err}");
                            // NB: progress None tells `set_stopping` that attach has cancelled.
                            *state = TenantState::Stopping { progress: None };
                        }
                        // According to the old code, DeleteTenantFlow may already have set this to
                        // Stopping. Retain its progress.
                        // TODO: there is no DeleteTenantFlow. Is this still needed? See above.
                        TenantState::Stopping { progress } if t.cancel.is_cancelled() => {
                            assert!(progress.is_some(), "concurrent attach cancellation");
                            info!("attach cancelled, already Stopping: {err}");
                        }
                        // Mark the tenant as broken.
                        TenantState::Attaching | TenantState::Stopping { .. } => {
                            error!("attach failed, setting tenant state to Broken (was {state}): {err:?}");
                            *state = TenantState::broken_from_reason(err.to_string())
                        }
                        // The attach task owns the tenant state until activated.
                        state => panic!("invalid tenant state {state} during attach: {err:?}"),
                    });
                }

                let mut init_order = init_order;
                // take the completion because initial tenant loading will complete when all of
                // these tasks complete.
                let _completion = init_order
                    .as_mut()
                    .and_then(|x| x.initial_tenant_load.take());
                let remote_load_completion = init_order
                    .as_mut()
                    .and_then(|x| x.initial_tenant_load_remote.take());

                enum AttachType<'a> {
                    /// We are attaching this tenant lazily in the background.
                    Warmup {
                        _permit: tokio::sync::SemaphorePermit<'a>,
                        during_startup: bool
                    },
                    /// We are attaching this tenant as soon as we can, because for example an
                    /// endpoint tried to access it.
                    OnDemand,
                    /// During normal operations after startup, we are attaching a tenant, and
                    /// eager attach was requested.
                    Normal,
                }

                let attach_type = if matches!(mode, SpawnMode::Lazy) {
                    // Before doing any I/O, wait for at least one of:
                    // - A client attempting to access to this tenant (on-demand loading)
                    // - A permit becoming available in the warmup semaphore (background warmup)

                    tokio::select!(
                        permit = tenant_clone.activate_now_sem.acquire() => {
                            let _ = permit.expect("activate_now_sem is never closed");
                            tracing::info!("Activating tenant (on-demand)");
                            AttachType::OnDemand
                        },
                        permit = conf.concurrent_tenant_warmup.inner().acquire() => {
                            let _permit = permit.expect("concurrent_tenant_warmup semaphore is never closed");
                            tracing::info!("Activating tenant (warmup)");
                            AttachType::Warmup {
                                _permit,
                                during_startup: init_order.is_some()
                            }
                        }
                        _ = tenant_clone.cancel.cancelled() => {
                            // This is safe, but should be pretty rare: it is interesting if a tenant
                            // stayed in Activating for such a long time that shutdown found it in
                            // that state.
                            tracing::info!(state=%tenant_clone.current_state(), "Tenant shut down before activation");
                            // Set the tenant to Stopping to signal `set_stopping` that we're done.
                            make_broken_or_stopping(&tenant_clone, anyhow::anyhow!("Shut down while Attaching"));
                            return Ok(());
                        },
                    )
                } else {
                    // SpawnMode::{Create,Eager} always cause jumping ahead of the
                    // concurrent_tenant_warmup queue
                    AttachType::Normal
                };

                let preload = match &mode {
                    SpawnMode::Eager | SpawnMode::Lazy => {
                        let _preload_timer = TENANT.preload.start_timer();
                        let res = tenant_clone
                            .preload(&remote_storage, task_mgr::shutdown_token())
                            .await;
                        match res {
                            Ok(p) => Some(p),
                            Err(e) => {
                                make_broken_or_stopping(&tenant_clone, anyhow::anyhow!(e));
                                return Ok(());
                            }
                        }
                    }

                };

                // Remote preload is complete.
                drop(remote_load_completion);


                // We will time the duration of the attach phase unless this is a creation (attach will do no work)
                let attach_start = std::time::Instant::now();
                let attached = {
                    let _attach_timer = Some(TENANT.attach.start_timer());
                    tenant_clone.attach(preload, &ctx).await
                };
                let attach_duration = attach_start.elapsed();
                _ = tenant_clone.attach_wal_lag_cooldown.set(WalLagCooldown::new(attach_start, attach_duration));

                match attached {
                    Ok(()) => {
                        info!("attach finished, activating");
                        tenant_clone.activate(broker_client, None, &ctx);
                    }
                    Err(e) => make_broken_or_stopping(&tenant_clone, anyhow::anyhow!(e)),
                }

                // If we are doing an opportunistic warmup attachment at startup, initialize
                // logical size at the same time.  This is better than starting a bunch of idle tenants
                // with cold caches and then coming back later to initialize their logical sizes.
                //
                // It also prevents the warmup proccess competing with the concurrency limit on
                // logical size calculations: if logical size calculation semaphore is saturated,
                // then warmup will wait for that before proceeding to the next tenant.
                if matches!(attach_type, AttachType::Warmup { during_startup: true, .. }) {
                    let mut futs: FuturesUnordered<_> = tenant_clone.timelines.lock().unwrap().values().cloned().map(|t| t.await_initial_logical_size()).collect();
                    tracing::info!("Waiting for initial logical sizes while warming up...");
                    while futs.next().await.is_some() {}
                    tracing::info!("Warm-up complete");
                }

                Ok(())
            }
            .instrument(tracing::info_span!(parent: None, "attach", tenant_id=%tenant_shard_id, shard_id=%tenant_shard_id.shard_slug(), gen=?generation)),
        );
        Ok(tenant)
    }

    #[instrument(skip_all)]
    pub(crate) async fn preload(
        self: &Arc<Self>,
        _remote_storage: &GenericRemoteStorage,
        cancel: CancellationToken,
    ) -> anyhow::Result<TenantPreload> {
        span::debug_assert_current_span_has_tenant_id();

        // The authoritative timeline set lives in
        // `pageserver.timeline_state`; one SQL fetch builds the full
        // preload. Offloaded timelines aren't supported in yb-mode, so
        // the tenant-manifest blob is also skipped.
        let tenant_id = self.tenant_shard_id;
        let rows = tokio::select! {
            r = self.compute_page_store.list_timelines(tenant_id) => r?,
            _ = cancel.cancelled() => anyhow::bail!("cancelled"),
        };
        info!("preload: {} timelines from pageserver.timeline_state", rows.len());
        let mut timelines = HashMap::with_capacity(rows.len());
        for row in rows {
            let index_part = synth_index_part_from_row(&row)?;
            timelines.insert(
                row.timeline_id,
                Some(TimelinePreload {
                    index_part: Ok(index_part),
                }),
            );
        }
        Ok(TenantPreload {
            tenant_manifest: None,
            timelines,
        })
    }

    ///
    /// Background task that downloads all data for a tenant and brings it to Active state.
    ///
    /// No background tasks are started as part of this routine.
    ///
    async fn attach(
        self: &Arc<TenantShard>,
        preload: Option<TenantPreload>,
        ctx: &RequestContext,
    ) -> anyhow::Result<()> {
        span::debug_assert_current_span_has_tenant_id();

        failpoint_support::sleep_millis_async!("before-attaching-tenant");

        let Some(preload) = preload else {
            anyhow::bail!(
                "local-only deployment is no longer supported, https://github.com/neondatabase/neon/issues/5624"
            );
        };

        let mut offloaded_timeline_ids = HashSet::new();
        let mut offloaded_timelines_list = Vec::new();
        if let Some(tenant_manifest) = &preload.tenant_manifest {
            for timeline_manifest in tenant_manifest.offloaded_timelines.iter() {
                let timeline_id = timeline_manifest.timeline_id;
                let offloaded_timeline =
                    OffloadedTimeline::from_manifest(self.tenant_shard_id, timeline_manifest);
                offloaded_timelines_list.push((timeline_id, Arc::new(offloaded_timeline)));
                offloaded_timeline_ids.insert(timeline_id);
            }
        }
        // Complete deletions for offloaded timeline id's from manifest.
        // The manifest will be uploaded later in this function.
        offloaded_timelines_list
            .retain(|(offloaded_id, offloaded)| {
                // Existence of a timeline is finally determined by the existence of an index-part.json in remote storage.
                // If there is dangling references in another location, they need to be cleaned up.
                let delete = !preload.timelines.contains_key(offloaded_id);
                if delete {
                    tracing::info!("Removing offloaded timeline {offloaded_id} from manifest as no remote prefix was found");
                    offloaded.defuse_for_tenant_drop();
                }
                !delete
        });

        let mut remote_index = HashMap::new();
        let mut timeline_ancestors = HashMap::new();
        for (timeline_id, preload) in preload.timelines {
            let Some(preload) = preload else { continue };
            // Offloaded timelines are tracked separately; preload only
            // surfaces non-offloaded ones.
            assert!(!offloaded_timeline_ids.contains(&timeline_id));
            // `preload.index_part` is synthesised from
            // `pageserver.timeline_state` in `Tenant::preload`, so it's
            // always `Ok(IndexPart(...))` — no remote blob download
            // path remains in yb-mode.
            let MaybeDeletedIndexPart::IndexPart(index_part) = preload
                .index_part
                .expect("yb-mode preload always builds an Ok IndexPart")
            else {
                unreachable!("synth_index_part_from_row never produces Deleted");
            };
            timeline_ancestors.insert(timeline_id, index_part.metadata.clone());
            remote_index.insert(timeline_id, index_part);
        }

        let mut gc_blocks = HashMap::new();

        let sorted_timelines = tree_sort_timelines(timeline_ancestors, |m| m.ancestor_timeline())?;
        for (timeline_id, remote_metadata) in sorted_timelines {
            let index_part = remote_index
                .remove(&timeline_id)
                .expect("just put it in above");

            if let Some(blocking) = index_part.gc_blocking.as_ref() {
                // could just filter these away, but it helps while testing
                anyhow::ensure!(
                    !blocking.reasons.is_empty(),
                    "index_part for {timeline_id} is malformed: it should not have gc blocking with zero reasons"
                );
                let prev = gc_blocks.insert(timeline_id, blocking.reasons);
                assert!(prev.is_none());
            }

            // TODO again handle early failure
            let effect = self
                .load_remote_timeline(
                    timeline_id,
                    index_part,
                    remote_metadata,
                    self.build_timeline_resources(timeline_id),
                    LoadTimelineCause::Attach,
                    ctx,
                )
                .await
                .with_context(|| {
                    format!(
                        "failed to load remote timeline {} for tenant {}",
                        timeline_id, self.tenant_shard_id
                    )
                })?;

            let TimelineInitAndSyncResult::ReadyToActivate = effect;
            // activation happens later, on Tenant::activate
        }

        // At this point we've initialized all timelines and are tracking them.
        // Now compute the layer visibility for all (not offloaded) timelines.
        let compute_visiblity_for = {
            let timelines_accessor = self.timelines.lock().unwrap();
            let mut timelines_offloaded_accessor = self.timelines_offloaded.lock().unwrap();

            timelines_offloaded_accessor.extend(offloaded_timelines_list.into_iter());

            // Before activation, populate each Timeline's GcInfo with information about its children
            self.initialize_gc_info(&timelines_accessor, &timelines_offloaded_accessor, None);

            timelines_accessor.values().cloned().collect::<Vec<_>>()
        };

        // update_layer_visibility removed in phase 4d.2.
        let _ = compute_visiblity_for;

        // Resume-deletion path is dead in yb-mode: synth never produces
        // `MaybeDeletedIndexPart::Deleted`, so there's nothing to
        // restart here. If we want crash-safe deletion in yb-mode,
        // surface `timeline_state.deleted_at` at preload time instead.

        // Stash the preloaded tenant manifest, and upload a new manifest if changed.
        //
        // NB: this must happen after the tenant is fully populated above. In particular the
        // offloaded timelines, which are included in the manifest.
        {
            let mut guard = self.remote_tenant_manifest.lock().await;
            assert!(guard.is_none(), "tenant manifest set before preload"); // first populated here
            *guard = preload.tenant_manifest;
        }
        self.maybe_upload_tenant_manifest().await?;

        // yb-mode is the only path; we never wrote layer files, never
        // created per-timeline dentries, nothing to reconcile.

        self.gc_block.set_scanned(gc_blocks);

        fail::fail_point!("attach-before-activate", |_| {
            anyhow::bail!("attach-before-activate");
        });
        failpoint_support::sleep_millis_async!("attach-before-activate-sleep", &self.cancel);

        info!("Done");

        Ok(())
    }

    /// Get sum of all remote timelines sizes
    ///
    /// This function relies on the index_part instead of listing the remote storage
    pub fn remote_size(&self) -> u64 {
        // yb-mode: no remote layer files; sum is always 0. The
        // caller (storage_ctl heatmap) tolerates this; tenant-size
        // reporting happens via timeline-row LSN ranges, not blob
        // bytes.
        let _ = self.list_timelines();
        0
    }

    #[instrument(skip_all, fields(timeline_id=%timeline_id))]
    #[allow(clippy::too_many_arguments)]
    async fn load_remote_timeline(
        self: &Arc<Self>,
        timeline_id: TimelineId,
        index_part: IndexPart,
        remote_metadata: TimelineMetadata,
        resources: TimelineResources,
        cause: LoadTimelineCause,
        ctx: &RequestContext,
    ) -> anyhow::Result<TimelineInitAndSyncResult> {
        span::debug_assert_current_span_has_tenant_id();

        info!("loading timeline {}", timeline_id);

        let ancestor = if let Some(ancestor_id) = remote_metadata.ancestor_timeline() {
            let timelines = self.timelines.lock().unwrap();
            Some(Arc::clone(timelines.get(&ancestor_id).ok_or_else(
                || {
                    anyhow::anyhow!(
                        "cannot find ancestor timeline {ancestor_id} for timeline {timeline_id}"
                    )
                },
            )?))
        } else {
            None
        };

        self.timeline_init_and_sync(
            timeline_id,
            resources,
            index_part,
            remote_metadata,
            ancestor,
            cause,
            ctx,
        )
        .await
    }

    fn build_timeline_client(
        &self,
        timeline_id: TimelineId,
        remote_storage: GenericRemoteStorage,
    ) -> RemoteTimelineClient {
        RemoteTimelineClient::new(
            remote_storage,
            self.tenant_shard_id,
            timeline_id,
            self.generation,
        )
    }

    fn load_timeline_metadata(
        self: &Arc<TenantShard>,
        timeline_id: TimelineId,
        remote_storage: GenericRemoteStorage,
        cancel: CancellationToken,
    ) -> impl Future<Output = TimelinePreload> + use<> {
        let client = self.build_timeline_client(timeline_id, remote_storage);
        async move {
            debug_assert_current_span_has_tenant_and_timeline_id();
            debug!("starting index part download");

            let index_part = client.download_index_file(&cancel).await;

            debug!("finished index part download");

            TimelinePreload { index_part }
        }
    }

    fn check_to_be_archived_has_no_unarchived_children(
        timeline_id: TimelineId,
        timelines: &std::sync::MutexGuard<'_, HashMap<TimelineId, Arc<Timeline>>>,
    ) -> Result<(), TimelineArchivalError> {
        let children: Vec<TimelineId> = timelines
            .iter()
            .filter_map(|(id, entry)| {
                if entry.get_ancestor_timeline_id() != Some(timeline_id) {
                    return None;
                }
                if entry.is_archived() == Some(true) {
                    return None;
                }
                Some(*id)
            })
            .collect();

        if !children.is_empty() {
            return Err(TimelineArchivalError::HasUnarchivedChildren(children));
        }
        Ok(())
    }

    fn check_ancestor_of_to_be_unarchived_is_not_archived(
        ancestor_timeline_id: TimelineId,
        timelines: &std::sync::MutexGuard<'_, HashMap<TimelineId, Arc<Timeline>>>,
        offloaded_timelines: &std::sync::MutexGuard<
            '_,
            HashMap<TimelineId, Arc<OffloadedTimeline>>,
        >,
    ) -> Result<(), TimelineArchivalError> {
        let has_archived_parent =
            if let Some(ancestor_timeline) = timelines.get(&ancestor_timeline_id) {
                ancestor_timeline.is_archived() == Some(true)
            } else if offloaded_timelines.contains_key(&ancestor_timeline_id) {
                true
            } else {
                error!("ancestor timeline {ancestor_timeline_id} not found");
                if cfg!(debug_assertions) {
                    panic!("ancestor timeline {ancestor_timeline_id} not found");
                }
                return Err(TimelineArchivalError::NotFound);
            };
        if has_archived_parent {
            return Err(TimelineArchivalError::HasArchivedParent(
                ancestor_timeline_id,
            ));
        }
        Ok(())
    }

    fn check_to_be_unarchived_timeline_has_no_archived_parent(
        timeline: &Arc<Timeline>,
    ) -> Result<(), TimelineArchivalError> {
        if let Some(ancestor_timeline) = timeline.ancestor_timeline() {
            if ancestor_timeline.is_archived() == Some(true) {
                return Err(TimelineArchivalError::HasArchivedParent(
                    ancestor_timeline.timeline_id,
                ));
            }
        }
        Ok(())
    }

    /// Loads the specified (offloaded) timeline from S3 and attaches it as a loaded timeline
    ///
    /// Counterpart to [`offload_timeline`].
    async fn unoffload_timeline(
        self: &Arc<Self>,
        timeline_id: TimelineId,
        broker_client: storage_broker::BrokerClientChannel,
        ctx: RequestContext,
    ) -> Result<Arc<Timeline>, TimelineArchivalError> {
        info!("unoffloading timeline");

        // We activate the timeline below manually, so this must be called on an active tenant.
        // We expect callers of this function to ensure this.
        match self.current_state() {
            TenantState::Activating { .. }
            | TenantState::Attaching
            | TenantState::Broken { .. } => {
                panic!("Timeline expected to be active")
            }
            TenantState::Stopping { .. } => return Err(TimelineArchivalError::Cancelled),
            TenantState::Active => {}
        }
        let cancel = self.cancel.clone();

        // Protect against concurrent attempts to use this TimelineId
        // We don't care much about idempotency, as it's ensured a layer above.
        let allow_offloaded = true;
        let _create_guard = self
            .create_timeline_create_guard(
                timeline_id,
                CreateTimelineIdempotency::FailWithConflict,
                allow_offloaded,
            )
            .map_err(|err| match err {
                TimelineExclusionError::AlreadyCreating => TimelineArchivalError::AlreadyInProgress,
                TimelineExclusionError::AlreadyExists { .. } => {
                    TimelineArchivalError::Other(anyhow::anyhow!("Timeline already exists"))
                }
                TimelineExclusionError::Other(e) => TimelineArchivalError::Other(e),
                TimelineExclusionError::ShuttingDown => TimelineArchivalError::Cancelled,
            })?;

        let timeline_preload = self
            .load_timeline_metadata(timeline_id, self.remote_storage.clone(), cancel.clone())
            .await;

        let index_part = match timeline_preload.index_part {
            Ok(index_part) => {
                debug!("remote index part exists for timeline {timeline_id}");
                index_part
            }
            Err(DownloadError::NotFound) => {
                error!(%timeline_id, "index_part not found on remote");
                return Err(TimelineArchivalError::NotFound);
            }
            Err(DownloadError::Cancelled) => return Err(TimelineArchivalError::Cancelled),
            Err(e) => {
                // Some (possibly ephemeral) error happened during index_part download.
                warn!(%timeline_id, "Failed to load index_part from remote storage, failed creation? ({e})");
                return Err(TimelineArchivalError::Other(
                    anyhow::Error::new(e).context("downloading index_part from remote storage"),
                ));
            }
        };
        let index_part = match index_part {
            MaybeDeletedIndexPart::IndexPart(index_part) => index_part,
            MaybeDeletedIndexPart::Deleted(_index_part) => {
                info!("timeline is deleted according to index_part.json");
                return Err(TimelineArchivalError::NotFound);
            }
        };
        let remote_metadata = index_part.metadata.clone();
        let timeline_resources = self.build_timeline_resources(timeline_id);
        self.load_remote_timeline(
            timeline_id,
            index_part,
            remote_metadata,
            timeline_resources,
            LoadTimelineCause::Unoffload,
            &ctx,
        )
        .await
        .with_context(|| {
            format!(
                "failed to load remote timeline {} for tenant {}",
                timeline_id, self.tenant_shard_id
            )
        })
        .map_err(TimelineArchivalError::Other)?;

        let timeline = {
            let timelines = self.timelines.lock().unwrap();
            let Some(timeline) = timelines.get(&timeline_id) else {
                warn!("timeline not available directly after attach");
                // This is not a panic because no locks are held between `load_remote_timeline`
                // which puts the timeline into timelines, and our look into the timeline map.
                return Err(TimelineArchivalError::Other(anyhow::anyhow!(
                    "timeline not available directly after attach"
                )));
            };
            let mut offloaded_timelines = self.timelines_offloaded.lock().unwrap();
            match offloaded_timelines.remove(&timeline_id) {
                Some(offloaded) => {
                    offloaded.delete_from_ancestor_with_timelines(&timelines);
                }
                None => warn!("timeline already removed from offloaded timelines"),
            }

            self.initialize_gc_info(&timelines, &offloaded_timelines, Some(timeline_id));

            Arc::clone(timeline)
        };

        // Upload new list of offloaded timelines to S3
        self.maybe_upload_tenant_manifest().await?;

        // Activate the timeline (if it makes sense)
        if !(timeline.is_broken() || timeline.is_stopping()) {
            let background_jobs_can_start = None;
            timeline.activate(
                self.clone(),
                broker_client.clone(),
                background_jobs_can_start,
                &ctx.with_scope_timeline(&timeline),
            );
        }

        info!("timeline unoffloading complete");
        Ok(timeline)
    }

    pub(crate) async fn apply_timeline_archival_config(
        self: &Arc<Self>,
        timeline_id: TimelineId,
        new_state: TimelineArchivalState,
        broker_client: storage_broker::BrokerClientChannel,
        ctx: RequestContext,
    ) -> Result<(), TimelineArchivalError> {
        info!("setting timeline archival config");
        // First part: figure out what is needed to do, and do validation
        let timeline_or_unarchive_offloaded = 'outer: {
            let timelines = self.timelines.lock().unwrap();

            let Some(timeline) = timelines.get(&timeline_id) else {
                let offloaded_timelines = self.timelines_offloaded.lock().unwrap();
                let Some(offloaded) = offloaded_timelines.get(&timeline_id) else {
                    return Err(TimelineArchivalError::NotFound);
                };
                if new_state == TimelineArchivalState::Archived {
                    // It's offloaded already, so nothing to do
                    return Ok(());
                }
                if let Some(ancestor_timeline_id) = offloaded.ancestor_timeline_id {
                    Self::check_ancestor_of_to_be_unarchived_is_not_archived(
                        ancestor_timeline_id,
                        &timelines,
                        &offloaded_timelines,
                    )?;
                }
                break 'outer None;
            };

            // Do some validation. We release the timelines lock below, so there is potential
            // for race conditions: these checks are more present to prevent misunderstandings of
            // the API's capabilities, instead of serving as the sole way to defend their invariants.
            match new_state {
                TimelineArchivalState::Unarchived => {
                    Self::check_to_be_unarchived_timeline_has_no_archived_parent(timeline)?
                }
                TimelineArchivalState::Archived => {
                    Self::check_to_be_archived_has_no_unarchived_children(timeline_id, &timelines)?
                }
            }
            Some(Arc::clone(timeline))
        };

        // Second part: unoffload timeline (if needed)
        let timeline = if let Some(timeline) = timeline_or_unarchive_offloaded {
            timeline
        } else {
            // Turn offloaded timeline into a non-offloaded one
            self.unoffload_timeline(timeline_id, broker_client, ctx)
                .await?
        };

        // Third part: persist the new archival state. yb-mode keeps
        // it in `remote_state.archived` only — there's no
        // index_part.json to upload, so the RTC schedule + the
        // wait_completion that used to gate this RPC are both gone.
        // Future restarts re-derive `archived` from
        // pageserver.timeline_state (TODO once we wire that column).
        timeline.remote_state.archived.store(
            matches!(new_state, TimelineArchivalState::Archived),
            std::sync::atomic::Ordering::Relaxed,
        );
        Ok(())
    }

    pub fn get_offloaded_timeline(
        &self,
        timeline_id: TimelineId,
    ) -> Result<Arc<OffloadedTimeline>, GetTimelineError> {
        self.timelines_offloaded
            .lock()
            .unwrap()
            .get(&timeline_id)
            .map(Arc::clone)
            .ok_or(GetTimelineError::NotFound {
                tenant_id: self.tenant_shard_id,
                timeline_id,
            })
    }

    pub(crate) fn tenant_shard_id(&self) -> TenantId {
        self.tenant_shard_id
    }

    /// Get Timeline handle for given Neon timeline ID.
    /// This function is idempotent. It doesn't change internal state in any way.
    pub fn get_timeline(
        &self,
        timeline_id: TimelineId,
        active_only: bool,
    ) -> Result<Arc<Timeline>, GetTimelineError> {
        let timelines_accessor = self.timelines.lock().unwrap();
        let timeline = timelines_accessor
            .get(&timeline_id)
            .ok_or(GetTimelineError::NotFound {
                tenant_id: self.tenant_shard_id,
                timeline_id,
            })?;

        if active_only && !timeline.is_active() {
            Err(GetTimelineError::NotActive {
                tenant_id: self.tenant_shard_id,
                timeline_id,
                state: timeline.current_state(),
            })
        } else {
            Ok(Arc::clone(timeline))
        }
    }

    /// Lists timelines the tenant contains.
    /// It's up to callers to omit certain timelines that are not considered ready for use.
    pub fn list_timelines(&self) -> Vec<Arc<Timeline>> {
        self.timelines
            .lock()
            .unwrap()
            .values()
            .map(Arc::clone)
            .collect()
    }


    /// Lists timelines the tenant manages, including offloaded ones.
    ///
    /// It's up to callers to omit certain timelines that are not considered ready for use.
    pub fn list_timelines_and_offloaded(
        &self,
    ) -> (Vec<Arc<Timeline>>, Vec<Arc<OffloadedTimeline>>) {
        let timelines = self
            .timelines
            .lock()
            .unwrap()
            .values()
            .map(Arc::clone)
            .collect();
        let offloaded = self
            .timelines_offloaded
            .lock()
            .unwrap()
            .values()
            .map(Arc::clone)
            .collect();
        (timelines, offloaded)
    }

    pub fn list_timeline_ids(&self) -> Vec<TimelineId> {
        self.timelines.lock().unwrap().keys().cloned().collect()
    }

    /// This is used by tests & import-from-basebackup.
    ///
    /// The returned [`UninitializedTimeline`] contains no data nor metadata and it is in
    /// a state that will fail [`TenantShard::load_remote_timeline`] because `disk_consistent_lsn=Lsn(0)`.
    ///
    /// The caller is responsible for getting the timeline into a state that will be accepted
    /// by [`TenantShard::load_remote_timeline`] / [`TenantShard::attach`].
    /// Then they may call [`UninitializedTimeline::finish_creation`] to add the timeline
    /// to the [`TenantShard::timelines`].
    ///
    /// Tests should use `TenantShard::create_test_timeline` to set up the minimum required metadata keys.
    pub(crate) async fn create_empty_timeline(
        self: &Arc<Self>,
        new_timeline_id: TimelineId,
        initdb_lsn: Lsn,
        pg_version: PgMajorVersion,
        ctx: &RequestContext,
    ) -> anyhow::Result<(UninitializedTimeline, RequestContext)> {
        anyhow::ensure!(
            self.is_active(),
            "Cannot create empty timelines on inactive tenant"
        );

        // Protect against concurrent attempts to use this TimelineId
        let create_guard = match self
            .start_creating_timeline(new_timeline_id, CreateTimelineIdempotency::FailWithConflict)
            .await?
        {
            StartCreatingTimelineResult::CreateGuard(guard) => guard,
            StartCreatingTimelineResult::Idempotent(_) => {
                unreachable!("FailWithConflict implies we get an error instead")
            }
        };

        let new_metadata = TimelineMetadata::new(
            // Initialize disk_consistent LSN to 0, The caller must import some data to
            // make it valid, before calling finish_creation()
            Lsn(0),
            None,
            None,
            Lsn(0),
            initdb_lsn,
            initdb_lsn,
            pg_version,
        );
        self.prepare_new_timeline(
            new_timeline_id,
            &new_metadata,
            create_guard,
            initdb_lsn,
            None,
            None,
            None,
            ctx,
        )
        .await
    }

    /// Helper for unit tests to create an empty timeline.
    ///
    /// The timeline is has state value `Active` but its background loops are not running.
    // This makes the various functions which anyhow::ensure! for Active state work in tests.
    // Our current tests don't need the background loops.
    #[cfg(test)]
    pub async fn create_test_timeline(
        self: &Arc<Self>,
        new_timeline_id: TimelineId,
        initdb_lsn: Lsn,
        pg_version: PgMajorVersion,
        ctx: &RequestContext,
    ) -> anyhow::Result<Arc<Timeline>> {
        let (uninit_tl, ctx) = self
            .create_empty_timeline(new_timeline_id, initdb_lsn, pg_version, ctx)
            .await?;
        let tline = uninit_tl.raw_timeline().expect("we just created it");
        assert_eq!(tline.get_last_record_lsn(), Lsn(0));

        // Setup minimum keys required for the timeline to be usable.
        let mut modification = tline.begin_modification(initdb_lsn);
        modification
            .init_empty_test_timeline()
            .context("init_empty_test_timeline")?;
        modification
            .commit(&ctx)
            .await
            .context("commit init_empty_test_timeline modification")?;

        // Flush to disk so that uninit_tl's check for valid disk_consistent_lsn passes.
        tline.maybe_spawn_flush_loop();
        tline.freeze_and_flush().await.context("freeze_and_flush")?;

        // yb-mode: `freeze_and_flush` already issued the YB INSERTs
        // synchronously; nothing to wait on.

        let tl = uninit_tl.finish_creation().await?;
        // The non-test code would call tl.activate() here.
        tl.set_state(TimelineState::Active);
        Ok(tl)
    }

    /// Helper for unit tests to create a timeline with some pre-loaded states.

    /// Create a new timeline.
    ///
    /// Returns the new timeline ID and reference to its Timeline object.
    ///
    /// If the caller specified the timeline ID to use (`new_timeline_id`), and timeline with
    /// the same timeline ID already exists, returns CreateTimelineError::AlreadyExists.
    #[allow(clippy::too_many_arguments)]
    pub(crate) async fn create_timeline(
        self: &Arc<TenantShard>,
        params: CreateTimelineParams,
        broker_client: storage_broker::BrokerClientChannel,
        ctx: &RequestContext,
    ) -> Result<Arc<Timeline>, CreateTimelineError> {
        if !self.is_active() {
            if matches!(self.current_state(), TenantState::Stopping { .. }) {
                return Err(CreateTimelineError::ShuttingDown);
            } else {
                return Err(CreateTimelineError::Other(anyhow::anyhow!(
                    "Cannot create timelines on inactive tenant"
                )));
            }
        }

        let _gate = self
            .gate
            .enter()
            .map_err(|_| CreateTimelineError::ShuttingDown)?;

        let result: CreateTimelineResult = match params {
            CreateTimelineParams::Bootstrap(CreateTimelineParamsBootstrap {
                new_timeline_id,
                existing_initdb_timeline_id,
                pg_version,
            }) => {
                self.bootstrap_timeline(
                    new_timeline_id,
                    pg_version,
                    existing_initdb_timeline_id,
                    ctx,
                )
                .await?
            }
            CreateTimelineParams::Branch(CreateTimelineParamsBranch {
                new_timeline_id,
                ancestor_timeline_id,
                mut ancestor_start_lsn,
            }) => {
                let ancestor_timeline = self
                    .get_timeline(ancestor_timeline_id, false)
                    .context("Cannot branch off the timeline that's not present in pageserver")?;

                // instead of waiting around, just deny the request because ancestor is not yet
                // ready for other purposes either.
                if !ancestor_timeline.is_active() {
                    return Err(CreateTimelineError::AncestorNotActive);
                }

                if ancestor_timeline.is_archived() == Some(true) {
                    info!("tried to branch archived timeline");
                    return Err(CreateTimelineError::AncestorArchived);
                }

                if let Some(lsn) = ancestor_start_lsn.as_mut() {
                    *lsn = lsn.align();

                    let ancestor_ancestor_lsn = ancestor_timeline.get_ancestor_lsn();
                    if ancestor_ancestor_lsn > *lsn {
                        // can we safely just branch from the ancestor instead?
                        return Err(CreateTimelineError::AncestorLsn(anyhow::anyhow!(
                            "invalid start lsn {} for ancestor timeline {}: less than timeline ancestor lsn {}",
                            lsn,
                            ancestor_timeline_id,
                            ancestor_ancestor_lsn,
                        )));
                    }

                    // Wait for the WAL to arrive and be processed on the parent branch up
                    // to the requested branch point. The repository code itself doesn't
                    // require it, but if we start to receive WAL on the new timeline,
                    // decoding the new WAL might need to look up previous pages, relation
                    // sizes etc. and that would get confused if the previous page versions
                    // are not in the repository yet.
                    ancestor_timeline
                        .wait_lsn(
                            *lsn,
                            timeline::WaitLsnWaiter::Tenant,
                            timeline::WaitLsnTimeout::Default,
                            ctx,
                        )
                        .await
                        .map_err(|e| match e {
                            e @ (WaitLsnError::Timeout(_) | WaitLsnError::BadState { .. }) => {
                                CreateTimelineError::AncestorLsn(anyhow::anyhow!(e))
                            }
                            WaitLsnError::Shutdown => CreateTimelineError::ShuttingDown,
                        })?;
                }

                self.branch_timeline(&ancestor_timeline, new_timeline_id, ancestor_start_lsn, ctx)
                    .await?
            }
        };

        // At this point we have dropped our guard on [`Self::timelines_creating`], and
        // the timeline is visible in [`Self::timelines`], but it is _not_ durable yet.  We must
        // not send a success to the caller until it is.  The same applies to idempotent retries.
        //
        // TODO: the timeline is already visible in [`Self::timelines`]; a caller could incorrectly
        // assume that, because they can see the timeline via API, that the creation is done and
        // that it is durable. Ideally, we would keep the timeline hidden (in [`Self::timelines_creating`])
        // until it is durable, e.g., by extending the time we hold the creation guard. This also
        // interacts with UninitializedTimeline and is generally a bit tricky.
        //
        // To re-emphasize: the only correct way to create a timeline is to repeat calling the
        // creation API until it returns success. Only then is durability guaranteed.
        // yb-mode: creation is synchronous (the timeline_state row is
        // upserted by `UninitializedTimeline::finish_creation` before
        // the lock is released). Nothing to wait on.
        info!(creation_result=%result.discriminant(), "timeline durable");

        // The creating task is responsible for activating the timeline.
        // We do this after `wait_completion()` so that we don't spin up tasks that start
        // doing stuff before the IndexPart is durable in S3, which is done by the previous section.
        let activated_timeline = match result {
            CreateTimelineResult::Created(timeline) => {
                timeline.activate(
                    self.clone(),
                    broker_client,
                    None,
                    &ctx.with_scope_timeline(&timeline),
                );
                timeline
            }
            CreateTimelineResult::Idempotent(timeline) => {
                info!(
                    "request was deemed idempotent, activation will be done by the creating task"
                );
                timeline
            }
        };

        Ok(activated_timeline)
    }

    pub(crate) async fn delete_timeline(
        self: Arc<Self>,
        timeline_id: TimelineId,
    ) -> Result<(), DeleteTimelineError> {
        DeleteTimelineFlow::run(&self, timeline_id).await?;

        Ok(())
    }

    /// perform one garbage collection iteration, removing old data files from disk.
    /// this function is periodically called by gc task.
    /// also it can be explicitly requested through page server api 'do_gc' command.
    ///
    /// `target_timeline_id` specifies the timeline to GC, or None for all.
    ///
    /// The `horizon` an `pitr` parameters determine how much WAL history needs to be retained.
    /// Also known as the retention period, or the GC cutoff point. `horizon` specifies
    /// the amount of history, as LSN difference from current latest LSN on each timeline.
    /// `pitr` specifies the same as a time difference from the current time. The effective
    /// GC cutoff point is determined conservatively by either `horizon` and `pitr`, whichever
    /// requires more history to be retained.
    //
    pub(crate) async fn gc_iteration(
        &self,
        target_timeline_id: Option<TimelineId>,
        horizon: u64,
        pitr: Duration,
        cancel: &CancellationToken,
        ctx: &RequestContext,
    ) -> Result<GcResult, GcError> {
        // Don't start doing work during shutdown
        if let TenantState::Stopping { .. } = self.current_state() {
            return Ok(GcResult::default());
        }

        // there is a global allowed_error for this
        if !self.is_active() {
            return Err(GcError::NotActive);
        }

        if self.tenant_conf.load().is_gc_blocked_by_lsn_lease_deadline() {
            info!("Skipping GC because lsn lease deadline is not reached");
            return Ok(GcResult::default());
        }

        let _guard = match self.gc_block.start().await {
            Ok(guard) => guard,
            Err(reasons) => {
                info!("Skipping GC: {reasons}");
                return Ok(GcResult::default());
            }
        };

        self.gc_iteration_internal(target_timeline_id, horizon, pitr, cancel, ctx)
            .await
    }

    /// Performs periodic housekeeping, via the tenant housekeeping background task.
    async fn housekeeping(&self) {
        // Freeze ephemeral layers on idle timelines so they don't sit
        // open indefinitely.
        let timelines = self
            .timelines
            .lock()
            .unwrap()
            .values()
            .filter(|tli| tli.is_active())
            .cloned()
            .collect_vec();
        for timeline in timelines {
            let span =
                info_span!("maybe_freeze_ephemeral_layer", timeline_id = %timeline.timeline_id);
            timeline
                .maybe_freeze_ephemeral_layer()
                .instrument(span)
                .await;
        }

        // Shut down walredo if idle.
        const WALREDO_IDLE_TIMEOUT: Duration = Duration::from_secs(180);
        if let Some(ref walredo_mgr) = self.walredo_mgr {
            walredo_mgr.maybe_quiesce(WALREDO_IDLE_TIMEOUT);
        }
    }

    pub fn timeline_has_no_attached_children(&self, timeline_id: TimelineId) -> bool {
        let timelines = self.timelines.lock().unwrap();
        !timelines
            .iter()
            .any(|(_id, tl)| tl.get_ancestor_timeline_id() == Some(timeline_id))
    }

    pub fn current_state(&self) -> TenantState {
        self.state.borrow().clone()
    }

    pub fn is_active(&self) -> bool {
        self.current_state() == TenantState::Active
    }

    pub fn generation(&self) -> Generation {
        self.generation
    }

    pub(crate) fn wal_redo_manager_status(&self) -> Option<WalRedoManagerStatus> {
        self.walredo_mgr.as_ref().and_then(|mgr| mgr.status())
    }

    /// Changes tenant status to active, unless shutdown was already requested.
    ///
    /// `background_jobs_can_start` is an optional barrier set to a value during pageserver startup
    /// to delay background jobs. Background jobs can be started right away when None is given.
    fn activate(
        self: &Arc<Self>,
        broker_client: BrokerClientChannel,
        background_jobs_can_start: Option<&completion::Barrier>,
        ctx: &RequestContext,
    ) {
        span::debug_assert_current_span_has_tenant_id();

        let mut activating = false;
        self.state.send_modify(|current_state| {
            use pageserver_api::models::ActivatingFrom;
            match &*current_state {
                TenantState::Activating(_) | TenantState::Active | TenantState::Broken { .. } | TenantState::Stopping { .. } => {
                    panic!("caller is responsible for calling activate() only on Loading / Attaching tenants, got {current_state:?}");
                }
                TenantState::Attaching => {
                    *current_state = TenantState::Activating(ActivatingFrom::Attaching);
                }
            }
            debug!(tenant_id = %self.tenant_shard_id, shard_id = %self.tenant_shard_id.shard_slug(), "Activating tenant");
            activating = true;
            // Continue outside the closure. We need to grab timelines.lock()
            // and we plan to turn it into a tokio::sync::Mutex in a future patch.
        });

        if activating {
            let timelines_accessor = self.timelines.lock().unwrap();
            let timelines_offloaded_accessor = self.timelines_offloaded.lock().unwrap();
            let timelines_to_activate = timelines_accessor
                .values()
                .filter(|timeline| !(timeline.is_broken() || timeline.is_stopping()));

            // Spawn gc and compaction loops. The loops will shut themselves
            // down when they notice that the tenant is inactive.
            tasks::start_background_loops(self, background_jobs_can_start);

            let mut activated_timelines = 0;

            for timeline in timelines_to_activate {
                timeline.activate(
                    self.clone(),
                    broker_client.clone(),
                    background_jobs_can_start,
                    &ctx.with_scope_timeline(timeline),
                );
                activated_timelines += 1;
            }

            let tid = self.tenant_shard_id.to_string();
            let shard_id = self.tenant_shard_id.shard_slug().to_string();
            let offloaded_timeline_count = timelines_offloaded_accessor.len();
            TENANT_OFFLOADED_TIMELINES
                .with_label_values(&[&tid, &shard_id])
                .set(offloaded_timeline_count as u64);

            self.state.send_modify(move |current_state| {
                assert!(
                    matches!(current_state, TenantState::Activating(_)),
                    "set_stopping and set_broken wait for us to leave Activating state",
                );
                *current_state = TenantState::Active;

                let elapsed = self.constructed_at.elapsed();
                let total_timelines = timelines_accessor.len();

                // log a lot of stuff, because some tenants sometimes suffer from user-visible
                // times to activate. see https://github.com/neondatabase/neon/issues/4025
                info!(
                    since_creation_millis = elapsed.as_millis(),
                    tenant_id = %self.tenant_shard_id,
                    shard_id = %self.tenant_shard_id.shard_slug(),
                    activated_timelines,
                    total_timelines,
                    post_state = <&'static str>::from(&*current_state),
                    "activation attempt finished"
                );

                TENANT.activation.observe(elapsed.as_secs_f64());
            });
        }
    }

    /// Shutdown the tenant and join all of the spawned tasks.
    ///
    /// The method caters for all use-cases:
    /// - pageserver shutdown (freeze_and_flush == true)
    /// - detach + ignore (freeze_and_flush == false)
    ///
    /// This will attempt to shutdown even if tenant is broken.
    ///
    /// `shutdown_progress` is a [`completion::Barrier`] for the shutdown initiated by this call.
    /// If the tenant is already shutting down, we return a clone of the first shutdown call's
    /// `Barrier` as an `Err`. This not-first caller can use the returned barrier to join with
    /// the ongoing shutdown.
    async fn shutdown(
        &self,
        shutdown_progress: completion::Barrier,
        shutdown_mode: timeline::ShutdownMode,
    ) -> Result<(), completion::Barrier> {
        span::debug_assert_current_span_has_tenant_id();

        // Set tenant (and its timlines) to Stoppping state.
        //
        // Since we can only transition into Stopping state after activation is complete,
        // run it in a JoinSet so all tenants have a chance to stop before we get SIGKILLed.
        //
        // Transitioning tenants to Stopping state has a couple of non-obvious side effects:
        // 1. Lock out any new requests to the tenants.
        // 2. Signal cancellation to WAL receivers (we wait on it below).
        // 3. Signal cancellation for other tenant background loops.
        // 4. ???
        //
        // The waiting for the cancellation is not done uniformly.
        // We certainly wait for WAL receivers to shut down.
        // That is necessary so that no new data comes in before the freeze_and_flush.
        // But the tenant background loops are joined-on in our caller.
        // It's mesed up.
        // we just ignore the failure to stop

        // If we're still attaching, fire the cancellation token early to drop out: this
        // will prevent us flushing, but ensures timely shutdown if some I/O during attach
        // is very slow.
        let shutdown_mode = if matches!(self.current_state(), TenantState::Attaching) {
            self.cancel.cancel();

            // Having fired our cancellation token, do not try and flush timelines: their cancellation tokens
            // are children of ours, so their flush loops will have shut down already
            timeline::ShutdownMode::Hard
        } else {
            shutdown_mode
        };

        match self.set_stopping(shutdown_progress).await {
            Ok(()) => {}
            Err(SetStoppingError::Broken) => {
                // assume that this is acceptable
            }
            Err(SetStoppingError::AlreadyStopping(other)) => {
                // give caller the option to wait for this this shutdown
                info!("Tenant::shutdown: AlreadyStopping");
                return Err(other);
            }
        };

        let mut js = tokio::task::JoinSet::new();
        {
            let timelines = self.timelines.lock().unwrap();
            timelines.values().for_each(|timeline| {
                let timeline = Arc::clone(timeline);
                let timeline_id = timeline.timeline_id;
                let span = tracing::info_span!("timeline_shutdown", %timeline_id, ?shutdown_mode);
                js.spawn(async move { timeline.shutdown(shutdown_mode).instrument(span).await });
            });
        }
        {
            let timelines_offloaded = self.timelines_offloaded.lock().unwrap();
            timelines_offloaded.values().for_each(|timeline| {
                timeline.defuse_for_tenant_drop();
            });
        }
        // test_long_timeline_create_then_tenant_delete is leaning on this message
        tracing::info!("Waiting for timelines...");
        while let Some(res) = js.join_next().await {
            match res {
                Ok(()) => {}
                Err(je) if je.is_cancelled() => unreachable!("no cancelling used"),
                Err(je) if je.is_panic() => {
                    // The panic itself was logged by the runtime's
                    // panic hook; count it so a flatline alert catches
                    // task deaths even if the log line was missed.
                    crate::metrics::CORRECTNESS_ERRORS
                        .with_label_values(&["tenant_task_panic"])
                        .inc();
                }
                Err(je) => {
                    crate::metrics::CORRECTNESS_ERRORS
                        .with_label_values(&["tenant_task_panic"])
                        .inc();
                    tracing::error!(
                        "TENANT TASK ENDED WITH UNEXPECTED JoinError: {je:?}"
                    );
                }
            }
        }

        // We cancel the Tenant's cancellation token _after_ the timelines have all shut down.  This permits
        // them to continue to do work during their shutdown methods, e.g. flushing data.
        tracing::debug!("Cancelling CancellationToken");
        self.cancel.cancel();

        // shutdown all tenant and timeline tasks: gc, compaction, page service
        // No new tasks will be started for this tenant because it's in `Stopping` state.
        //
        // this will additionally shutdown and await all timeline tasks.
        tracing::debug!("Waiting for tasks...");
        task_mgr::shutdown_tasks(None, Some(self.tenant_shard_id), None).await;

        if let Some(walredo_mgr) = self.walredo_mgr.as_ref() {
            walredo_mgr.shutdown().await;
        }

        // Wait for any in-flight operations to complete
        self.gate.close().await;

        remove_tenant_metrics(&self.tenant_shard_id);

        Ok(())
    }

    /// Change tenant status to Stopping, to mark that it is being shut down.
    ///
    /// This function waits for the tenant to become active if it isn't already, before transitioning it into Stopping state.
    ///
    /// This function is not cancel-safe!
    async fn set_stopping(&self, progress: completion::Barrier) -> Result<(), SetStoppingError> {
        let mut rx = self.state.subscribe();

        // cannot stop before we're done activating, so wait out until we're done activating
        rx.wait_for(|state| match state {
            TenantState::Activating(_) | TenantState::Attaching => {
                info!("waiting for {state} to turn Active|Broken|Stopping");
                false
            }
            TenantState::Active | TenantState::Broken { .. } | TenantState::Stopping { .. } => true,
        })
        .await
        .expect("cannot drop self.state while on a &self method");

        // we now know we're done activating, let's see whether this task is the winner to transition into Stopping
        let mut err = None;
        let stopping = self.state.send_if_modified(|current_state| match current_state {
            TenantState::Activating(_) | TenantState::Attaching => {
                unreachable!("we ensured above that we're done with activation, and, there is no re-activation")
            }
            TenantState::Active => {
                // FIXME: due to time-of-check vs time-of-use issues, it can happen that new timelines
                // are created after the transition to Stopping. That's harmless, as the Timelines
                // won't be accessible to anyone afterwards, because the Tenant is in Stopping state.
                *current_state = TenantState::Stopping { progress: Some(progress) };
                // Continue stopping outside the closure. We need to grab timelines.lock()
                // and we plan to turn it into a tokio::sync::Mutex in a future patch.
                true
            }
            TenantState::Stopping { progress: None } => {
                // An attach was cancelled, and the attach transitioned the tenant from Attaching to
                // Stopping(None) to let us know it exited. Register our progress and continue.
                *current_state = TenantState::Stopping { progress: Some(progress) };
                true
            }
            TenantState::Broken { reason, .. } => {
                info!(
                    "Cannot set tenant to Stopping state, it is in Broken state due to: {reason}"
                );
                err = Some(SetStoppingError::Broken);
                false
            }
            TenantState::Stopping { progress: Some(progress) } => {
                info!("Tenant is already in Stopping state");
                err = Some(SetStoppingError::AlreadyStopping(progress.clone()));
                false
            }
        });
        match (stopping, err) {
            (true, None) => {} // continue
            (false, Some(err)) => return Err(err),
            (true, Some(_)) => unreachable!(
                "send_if_modified closure must error out if not transitioning to Stopping"
            ),
            (false, None) => unreachable!(
                "send_if_modified closure must return true if transitioning to Stopping"
            ),
        }

        let timelines_accessor = self.timelines.lock().unwrap();
        let not_broken_timelines = timelines_accessor
            .values()
            .filter(|timeline| !timeline.is_broken());
        for timeline in not_broken_timelines {
            timeline.set_state(TimelineState::Stopping);
        }
        Ok(())
    }

    /// Method for tenant::mgr to transition us into Broken state in case of a late failure in
    /// `remove_tenant_from_memory`
    ///
    /// This function waits for the tenant to become active if it isn't already, before transitioning it into Stopping state.
    ///
    /// In tests, we also use this to set tenants to Broken state on purpose.
    pub(crate) async fn set_broken(&self, reason: String) {
        let mut rx = self.state.subscribe();

        // The load & attach routines own the tenant state until it has reached `Active`.
        // So, wait until it's done.
        rx.wait_for(|state| match state {
            TenantState::Activating(_) | TenantState::Attaching => {
                info!(
                    "waiting for {} to turn Active|Broken|Stopping",
                    <&'static str>::from(state)
                );
                false
            }
            TenantState::Active | TenantState::Broken { .. } | TenantState::Stopping { .. } => true,
        })
        .await
        .expect("cannot drop self.state while on a &self method");

        // we now know we're done activating, let's see whether this task is the winner to transition into Broken
        self.set_broken_no_wait(reason)
    }

    pub(crate) fn set_broken_no_wait(&self, reason: impl Display) {
        let reason = reason.to_string();
        self.state.send_modify(|current_state| {
            match *current_state {
                TenantState::Activating(_) | TenantState::Attaching => {
                    unreachable!("we ensured above that we're done with activation, and, there is no re-activation")
                }
                TenantState::Active => {
                    if cfg!(feature = "testing") {
                        warn!("Changing Active tenant to Broken state, reason: {}", reason);
                        *current_state = TenantState::broken_from_reason(reason);
                    } else {
                        unreachable!("not allowed to call set_broken on Active tenants in non-testing builds")
                    }
                }
                TenantState::Broken { .. } => {
                    warn!("Tenant is already in Broken state");
                }
                // This is the only "expected" path, any other path is a bug.
                TenantState::Stopping { .. } => {
                    warn!(
                        "Marking Stopping tenant as Broken state, reason: {}",
                        reason
                    );
                    *current_state = TenantState::broken_from_reason(reason);
                }
           }
        });
    }

    pub fn subscribe_for_state_updates(&self) -> watch::Receiver<TenantState> {
        self.state.subscribe()
    }

    /// The activate_now semaphore is initialized with zero units.  As soon as
    /// we add a unit, waiters will be able to acquire a unit and proceed.
    pub(crate) fn activate_now(&self) {
        self.activate_now_sem.add_permits(1);
    }

    pub(crate) async fn wait_to_become_active(
        &self,
        timeout: Duration,
    ) -> Result<(), GetActiveTenantError> {
        let mut receiver = self.state.subscribe();
        loop {
            let current_state = receiver.borrow_and_update().clone();
            match current_state {
                TenantState::Attaching | TenantState::Activating(_) => {
                    // in these states, there's a chance that we can reach ::Active
                    self.activate_now();
                    match timeout_cancellable(timeout, &self.cancel, receiver.changed()).await {
                        Ok(r) => {
                            r.map_err(
                            |_e: tokio::sync::watch::error::RecvError|
                                // Tenant existed but was dropped: report it as non-existent
                                GetActiveTenantError::NotFound(GetTenantError::ShardNotFound(self.tenant_shard_id))
                        )?
                        }
                        Err(TimeoutCancellableError::Cancelled) => {
                            return Err(GetActiveTenantError::Cancelled);
                        }
                        Err(TimeoutCancellableError::Timeout) => {
                            return Err(GetActiveTenantError::WaitForActiveTimeout {
                                latest_state: Some(self.current_state()),
                                wait_time: timeout,
                            });
                        }
                    }
                }
                TenantState::Active => {
                    return Ok(());
                }
                TenantState::Broken { reason, .. } => {
                    // This is fatal, and reported distinctly from the general case of "will never be active" because
                    // it's logically a 500 to external API users (broken is always a bug).
                    return Err(GetActiveTenantError::Broken(reason));
                }
                TenantState::Stopping { .. } => {
                    // There's no chance the tenant can transition back into ::Active
                    return Err(GetActiveTenantError::WillNotBecomeActive(current_state));
                }
            }
        }
    }

    /// For API access: generate a `LocationConfig` equivalent to the one
    /// that would be used to create this attachment. Not for hot paths.
    pub(crate) fn get_location_conf(&self) -> models::LocationConfig {
        models::LocationConfig {
            generation: self.generation.into(),
        }
    }

    pub(crate) fn get_shard_identity(&self) -> ShardIdentity {
        self.shard_identity
    }

    pub(crate) fn get_shard_stripe_size(&self) -> ShardStripeSize {
        self.shard_identity.stripe_size
    }

    pub(crate) fn get_generation(&self) -> Generation {
        self.generation
    }


    pub(crate) fn get_sizes(&self) -> TopTenantShardItem {
        let mut result = TopTenantShardItem {
            id: self.tenant_shard_id,
            resident_size: 0,
            physical_size: 0,
            max_logical_size: 0,
            max_logical_size_per_shard: 0,
        };

        for timeline in self.timelines.lock().unwrap().values() {
            result.resident_size += timeline.metrics.resident_physical_size_gauge.get();
            // yb-mode: remote physical size is always 0 (no S3
            // layer files).
            result.max_logical_size = std::cmp::max(
                result.max_logical_size,
                timeline.metrics.current_logical_size_gauge.get(),
            );
        }

        // yb-mode: always single-shard.
        result.max_logical_size_per_shard = result.max_logical_size;

        result
    }
}

/// Given a Vec of timelines and their ancestors (timeline_id, ancestor_id),
/// perform a topological sort, so that the parent of each timeline comes
/// before the children.
/// E extracts the ancestor from T
/// This allows for T to be different. It can be TimelineMetadata, can be Timeline itself, etc.
fn tree_sort_timelines<T, E>(
    timelines: HashMap<TimelineId, T>,
    extractor: E,
) -> anyhow::Result<Vec<(TimelineId, T)>>
where
    E: Fn(&T) -> Option<TimelineId>,
{
    let mut result = Vec::with_capacity(timelines.len());

    let mut now = Vec::with_capacity(timelines.len());
    // (ancestor, children)
    let mut later: HashMap<TimelineId, Vec<(TimelineId, T)>> =
        HashMap::with_capacity(timelines.len());

    for (timeline_id, value) in timelines {
        if let Some(ancestor_id) = extractor(&value) {
            let children = later.entry(ancestor_id).or_default();
            children.push((timeline_id, value));
        } else {
            now.push((timeline_id, value));
        }
    }

    while let Some((timeline_id, metadata)) = now.pop() {
        result.push((timeline_id, metadata));
        // All children of this can be loaded now
        if let Some(mut children) = later.remove(&timeline_id) {
            now.append(&mut children);
        }
    }

    // All timelines should be visited now. Unless there were timelines with missing ancestors.
    if !later.is_empty() {
        for (missing_id, orphan_ids) in later {
            for (orphan_id, _) in orphan_ids {
                error!(
                    "could not load timeline {orphan_id} because its ancestor timeline {missing_id} could not be loaded"
                );
            }
        }
        bail!("could not load tenant because some timelines are missing ancestors");
    }

    Ok(result)
}

impl TenantShard {
    pub fn tenant_specific_overrides(&self) -> pageserver_api::models::TenantConfig {
        self.tenant_conf.load().tenant_conf.clone()
    }

    pub fn effective_config(&self) -> pageserver_api::config::TenantConfigToml {
        self.tenant_specific_overrides()
            .merge(self.conf.default_tenant_conf.clone())
    }

    pub fn get_rel_size_v2_enabled(&self) -> bool {
        let tenant_conf = self.tenant_conf.load().tenant_conf.clone();
        tenant_conf
            .rel_size_v2_enabled
            .unwrap_or(self.conf.default_tenant_conf.rel_size_v2_enabled)
    }

    pub fn get_gc_horizon(&self) -> u64 {
        let tenant_conf = self.tenant_conf.load().tenant_conf.clone();
        tenant_conf
            .gc_horizon
            .unwrap_or(self.conf.default_tenant_conf.gc_horizon)
    }

    pub fn get_gc_period(&self) -> Duration {
        let tenant_conf = self.tenant_conf.load().tenant_conf.clone();
        tenant_conf
            .gc_period
            .unwrap_or(self.conf.default_tenant_conf.gc_period)
    }

    pub fn get_pitr_interval(&self) -> Duration {
        let tenant_conf = self.tenant_conf.load().tenant_conf.clone();
        tenant_conf
            .pitr_interval
            .unwrap_or(self.conf.default_tenant_conf.pitr_interval)
    }

    pub fn get_lsn_lease_length(&self) -> Duration {
        Self::get_lsn_lease_length_impl(self.conf, &self.tenant_conf.load().tenant_conf)
    }

    pub fn get_lsn_lease_length_impl(
        conf: &'static PageServerConf,
        tenant_conf: &pageserver_api::models::TenantConfig,
    ) -> Duration {
        tenant_conf
            .lsn_lease_length
            .unwrap_or(conf.default_tenant_conf.lsn_lease_length)
    }

    /// Generate an up-to-date TenantManifest based on the state of this Tenant.
    fn build_tenant_manifest(&self) -> TenantManifest {
        // Collect the offloaded timelines, and sort them for deterministic output.
        let offloaded_timelines = self
            .timelines_offloaded
            .lock()
            .unwrap()
            .values()
            .map(|tli| tli.manifest())
            .sorted_by_key(|m| m.timeline_id)
            .collect_vec();

        TenantManifest {
            version: LATEST_TENANT_MANIFEST_VERSION,
            stripe_size: Some(self.get_shard_stripe_size()),
            offloaded_timelines,
        }
    }

    pub fn update_tenant_config<
        F: Fn(
            pageserver_api::models::TenantConfig,
        ) -> anyhow::Result<pageserver_api::models::TenantConfig>,
    >(
        &self,
        update: F,
    ) -> anyhow::Result<pageserver_api::models::TenantConfig> {
        // Use read-copy-update in order to avoid overwriting the location config
        // state if this races with [`TenantShard::set_new_location_config`]. Note that
        // this race is not possible if both request types come from the storage
        // controller (as they should!) because an exclusive op lock is required
        // on the storage controller side.

        self.tenant_conf
            .try_rcu(|attached_conf| -> Result<_, anyhow::Error> {
                Ok(Arc::new(AttachedTenantConf {
                    tenant_conf: update(attached_conf.tenant_conf.clone())?,
                    generation: attached_conf.generation,
                    lsn_lease_deadline: attached_conf.lsn_lease_deadline,
                }))
            })?;

        let updated = self.tenant_conf.load();

        self.tenant_conf_updated(&updated.tenant_conf);
        // Don't hold self.timelines.lock() during the notifies.
        // There's no risk of deadlock right now, but there could be if we consolidate
        // mutexes in struct Timeline in the future.
        let timelines = self.list_timelines();
        for timeline in timelines {
            timeline.tenant_conf_updated(&updated);
        }

        Ok(updated.tenant_conf.clone())
    }

    pub(crate) fn set_new_location_config(&self, new_conf: AttachedTenantConf) {
        let new_tenant_conf = new_conf.tenant_conf.clone();

        self.tenant_conf.store(Arc::new(new_conf.clone()));

        self.tenant_conf_updated(&new_tenant_conf);
        // Don't hold self.timelines.lock() during the notifies.
        // There's no risk of deadlock right now, but there could be if we consolidate
        // mutexes in struct Timeline in the future.
        let timelines = self.list_timelines();
        for timeline in timelines {
            timeline.tenant_conf_updated(&new_conf);
        }
    }

    fn get_pagestream_throttle_config(
        psconf: &'static PageServerConf,
        overrides: &pageserver_api::models::TenantConfig,
    ) -> throttle::Config {
        overrides
            .timeline_get_throttle
            .clone()
            .unwrap_or(psconf.default_tenant_conf.timeline_get_throttle.clone())
    }

    pub(crate) fn tenant_conf_updated(&self, new_conf: &pageserver_api::models::TenantConfig) {
        let conf = Self::get_pagestream_throttle_config(self.conf, new_conf);
        self.pagestream_throttle.reconfigure(conf)
    }

    /// Helper function to create a new Timeline struct.
    ///
    /// The returned Timeline is in Loading state. The caller is responsible for
    /// initializing any on-disk state, and for inserting the Timeline to the 'timelines'
    /// map.
    ///
    /// `validate_ancestor == false` is used when a timeline is created for deletion
    /// and we might not have the ancestor present anymore which is fine for to be
    /// deleted timelines.
    #[allow(clippy::too_many_arguments)]
    fn create_timeline_struct(
        &self,
        new_timeline_id: TimelineId,
        new_metadata: &TimelineMetadata,
        ancestor: Option<Arc<Timeline>>,
        resources: TimelineResources,
        create_idempotency: CreateTimelineIdempotency,
        gc_compaction_state: Option<GcCompactionState>,
        rel_size_v2_status: Option<RelSizeMigration>,
        rel_size_migrated_at: Option<Lsn>,
        ctx: &RequestContext,
    ) -> anyhow::Result<(Arc<Timeline>, RequestContext)> {
        let ancestor_id = new_metadata.ancestor_timeline();
        anyhow::ensure!(
            ancestor_id == ancestor.as_ref().map(|t| t.timeline_id),
            "Timeline's {new_timeline_id} ancestor {ancestor_id:?} was not found"
        );
        let state = TimelineState::Loading;

        let pg_version = new_metadata.pg_version();

        let timeline = Timeline::new(
            self.conf,
            Arc::clone(&self.tenant_conf),
            new_metadata,
            ancestor,
            new_timeline_id,
            self.tenant_shard_id,
            self.generation,
            self.shard_identity,
            self.walredo_mgr.clone(),
            resources,
            pg_version,
            state,
            self.attach_wal_lag_cooldown.clone(),
            create_idempotency,
            gc_compaction_state,
            rel_size_v2_status,
            rel_size_migrated_at,
            self.cancel.child_token(),
        );

        let timeline_ctx = RequestContextBuilder::from(ctx)
            .scope(context::Scope::new_timeline(&timeline))
            .detached_child();

        Ok((timeline, timeline_ctx))
    }

    /// [`TenantShard::shutdown`] must be called before dropping the returned [`TenantShard`] object
    /// to ensure proper cleanup of background tasks and metrics.
    //
    // Allow too_many_arguments because a constructor's argument list naturally grows with the
    // number of attributes in the struct: breaking these out into a builder wouldn't be helpful.
    #[allow(clippy::too_many_arguments)]
    fn new(
        state: TenantState,
        conf: &'static PageServerConf,
        attached_conf: AttachedTenantConf,
        shard_identity: ShardIdentity,
        walredo_mgr: Option<Arc<WalRedoManager>>,
        tenant_shard_id: TenantId,
        remote_storage: GenericRemoteStorage,
        deletion_queue_client: DeletionQueueClient,
        basebackup_cache: Arc<BasebackupCache>,
        compute_page_store: Arc<crate::page_store::PageStore>,
        fpu_page_store: Arc<crate::page_store::PageStore>,
    ) -> TenantShard {
        assert!(!attached_conf.generation.is_none());

        let (state, mut rx) = watch::channel(state);

        tokio::spawn(async move {
            // reflect tenant state in metrics:
            // - global per tenant state: TENANT_STATE_METRIC
            // - "set" of broken tenants: BROKEN_TENANTS_SET
            //
            // set of broken tenants should not have zero counts so that it remains accessible for
            // alerting.

            let tid = tenant_shard_id.to_string();
            let shard_id = tenant_shard_id.shard_slug().to_string();
            let set_key = &[tid.as_str(), shard_id.as_str()][..];

            fn inspect_state(state: &TenantState) -> ([&'static str; 1], bool) {
                ([state.into()], matches!(state, TenantState::Broken { .. }))
            }

            let mut tuple = inspect_state(&rx.borrow_and_update());

            let is_broken = tuple.1;
            let mut counted_broken = if is_broken {
                // add the id to the set right away, there should not be any updates on the channel
                // after before tenant is removed, if ever
                BROKEN_TENANTS_SET.with_label_values(set_key).set(1);
                true
            } else {
                false
            };

            loop {
                let labels = &tuple.0;
                let current = TENANT_STATE_METRIC.with_label_values(labels);
                current.inc();

                if rx.changed().await.is_err() {
                    // tenant has been dropped
                    current.dec();
                    drop(BROKEN_TENANTS_SET.remove_label_values(set_key));
                    break;
                }

                current.dec();
                tuple = inspect_state(&rx.borrow_and_update());

                let is_broken = tuple.1;
                if is_broken && !counted_broken {
                    counted_broken = true;
                    // insert the tenant_id (back) into the set while avoiding needless counter
                    // access
                    BROKEN_TENANTS_SET.with_label_values(set_key).set(1);
                }
            }
        });

        TenantShard {
            tenant_shard_id,
            shard_identity,
            generation: attached_conf.generation,
            conf,
            // using now here is good enough approximation to catch tenants with really long
            // activation times.
            constructed_at: Instant::now(),
            timelines: Mutex::new(HashMap::new()),
            timelines_creating: Mutex::new(HashSet::new()),
            timelines_offloaded: Mutex::new(HashMap::new()),
            remote_tenant_manifest: Default::default(),
            gc_cs: tokio::sync::Mutex::new(()),
            walredo_mgr,
            remote_storage,
            deletion_queue_client,
            state,
            activate_now_sem: tokio::sync::Semaphore::new(0),
            attach_wal_lag_cooldown: Arc::new(std::sync::OnceLock::new()),
            cancel: CancellationToken::default(),
            gate: Gate::default(),
            pagestream_throttle: Arc::new(throttle::Throttle::new(
                TenantShard::get_pagestream_throttle_config(conf, &attached_conf.tenant_conf),
            )),
            pagestream_throttle_metrics: Arc::new(
                crate::metrics::tenant_throttling::Pagestream::new(&tenant_shard_id),
            ),
            tenant_conf: Arc::new(ArcSwap::from_pointee(attached_conf)),
            gc_block: Default::default(),
            basebackup_cache,
            compute_page_store,
            fpu_page_store,
        }
    }

    /// Locate and load config
    pub(super) fn load_tenant_config(
        conf: &'static PageServerConf,
        tenant_shard_id: &TenantId,
    ) -> Result<LocationConf, LoadConfigError> {
        let config_path = conf.tenant_location_config_path(tenant_shard_id);

        info!("loading tenant configuration from {config_path}");

        // load and parse file
        let config = fs::read_to_string(&config_path).map_err(|e| {
            match e.kind() {
                std::io::ErrorKind::NotFound => {
                    // The config should almost always exist for a tenant directory:
                    //  - When attaching a tenant, the config is the first thing we write
                    //  - When detaching a tenant, we atomically move the directory to a tmp location
                    //    before deleting contents.
                    //
                    // The very rare edge case that can result in a missing config is if we crash during attach
                    // between creating directory and writing config.  Callers should handle that as if the
                    // directory didn't exist.

                    LoadConfigError::NotFound(config_path)
                }
                _ => {
                    // No IO errors except NotFound are acceptable here: other kinds of error indicate local storage or permissions issues
                    // that we cannot cleanly recover
                    crate::virtual_file::on_fatal_io_error(&e, "Reading tenant config file")
                }
            }
        })?;

        Ok(toml_edit::de::from_str::<LocationConf>(&config)?)
    }

    /// Stores a tenant location config to disk.
    ///
    /// NB: make sure to call `ShardIdentity::assert_equal` before persisting a new config, to avoid
    /// changes to shard parameters that may result in data corruption.
    #[tracing::instrument(skip_all, fields(tenant_id=%tenant_shard_id, shard_id=%tenant_shard_id.shard_slug()))]
    pub(super) async fn persist_tenant_config(
        conf: &'static PageServerConf,
        tenant_shard_id: &TenantId,
        location_conf: &LocationConf,
    ) -> std::io::Result<()> {
        let config_path = conf.tenant_location_config_path(tenant_shard_id);

        Self::persist_tenant_config_at(tenant_shard_id, &config_path, location_conf).await
    }

    #[tracing::instrument(skip_all, fields(tenant_id=%tenant_shard_id, shard_id=%tenant_shard_id.shard_slug()))]
    pub(super) async fn persist_tenant_config_at(
        tenant_shard_id: &TenantId,
        config_path: &Utf8Path,
        location_conf: &LocationConf,
    ) -> std::io::Result<()> {
        debug!("persisting tenantconf to {config_path}");

        let mut conf_content = r#"# This file contains a specific per-tenant's config.
#  It is read in case of pageserver restart.
"#
        .to_string();

        fail::fail_point!("tenant-config-before-write", |_| {
            Err(std::io::Error::other("tenant-config-before-write"))
        });

        // Convert the config to a toml file.
        conf_content +=
            &toml_edit::ser::to_string_pretty(&location_conf).expect("Config serialization failed");

        let temp_path = path_with_suffix_extension(config_path, TEMP_FILE_SUFFIX);

        let conf_content = conf_content.into_bytes();
        VirtualFile::crashsafe_overwrite(config_path.to_owned(), temp_path, conf_content).await
    }

    //
    // How garbage collection works:
    //
    //                    +--bar------------->
    //                   /
    //             +----+-----foo---------------->
    //            /
    // ----main--+-------------------------->
    //                \
    //                 +-----baz-------->
    //
    //
    // 1. Grab 'gc_cs' mutex to prevent new timelines from being created while Timeline's
    //    `gc_infos` are being refreshed
    // 2. Scan collected timelines, and on each timeline, make note of the
    //    all the points where other timelines have been branched off.
    //    We will refrain from removing page versions at those LSNs.
    // 3. For each timeline, scan all layer files on the timeline.
    //    Remove all files for which a newer file exists and which
    //    don't cover any branch point LSNs.
    //
    // TODO:
    // - if a relation has a non-incremental persistent layer on a child branch, then we
    //   don't need to keep that in the parent anymore. But currently
    //   we do.
    async fn gc_iteration_internal(
        &self,
        target_timeline_id: Option<TimelineId>,
        horizon: u64,
        pitr: Duration,
        cancel: &CancellationToken,
        ctx: &RequestContext,
    ) -> Result<GcResult, GcError> {
        let mut totals: GcResult = Default::default();
        let now = Instant::now();

        let gc_timelines = self
            .refresh_gc_info_internal(target_timeline_id, horizon, pitr, cancel, ctx)
            .await?;

        failpoint_support::sleep_millis_async!("gc_iteration_internal_after_getting_gc_timelines");

        // If there is nothing to GC, we don't want any messages in the INFO log.
        if !gc_timelines.is_empty() {
            info!("{} timelines need GC", gc_timelines.len());
        } else {
            debug!("{} timelines need GC", gc_timelines.len());
        }

        // Perform GC for each timeline.
        //
        // Note that we don't hold the `TenantShard::gc_cs` lock here because we don't want to delay the
        // branch creation task, which requires the GC lock. A GC iteration can run concurrently
        // with branch creation.
        //
        // See comments in [`TenantShard::branch_timeline`] for more information about why branch
        // creation task can run concurrently with timeline's GC iteration.
        for timeline in gc_timelines {
            if cancel.is_cancelled() {
                // We were requested to shut down. Stop and return with the progress we
                // made.
                break;
            }
            let result = match timeline.gc().await {
                Err(GcError::TimelineCancelled) => {
                    if target_timeline_id.is_some() {
                        // If we were targetting this specific timeline, surface cancellation to caller
                        return Err(GcError::TimelineCancelled);
                    } else {
                        // A timeline may be shutting down independently of the tenant's lifecycle: we should
                        // skip past this and proceed to try GC on other timelines.
                        continue;
                    }
                }
                r => r?,
            };
            totals += result;
        }

        totals.elapsed = now.elapsed();
        Ok(totals)
    }

    /// Refreshes the Timeline::gc_info for all timelines, returning the
    /// Populate all Timelines' `GcInfo` with information about their children.  We do not set the
    /// PITR cutoffs here, because that requires I/O: this is done later, before GC, by [`Self::refresh_gc_info_internal`]
    ///
    /// Subsequently, parent-child relationships are updated incrementally inside [`Timeline::new`] and [`Timeline::drop`].
    fn initialize_gc_info(
        &self,
        timelines: &std::sync::MutexGuard<HashMap<TimelineId, Arc<Timeline>>>,
        timelines_offloaded: &std::sync::MutexGuard<HashMap<TimelineId, Arc<OffloadedTimeline>>>,
        restrict_to_timeline: Option<TimelineId>,
    ) {
        if restrict_to_timeline.is_none() {
            // This function must be called before activation: after activation timeline create/delete operations
            // might happen, and this function is not safe to run concurrently with those.
            assert!(!self.is_active());
        }

        // Scan all timelines. For each timeline, remember the timeline ID and
        // the branch point where it was created.
        let mut all_branchpoints: BTreeMap<TimelineId, Vec<(Lsn, TimelineId, MaybeOffloaded)>> =
            BTreeMap::new();
        timelines.iter().for_each(|(timeline_id, timeline_entry)| {
            if let Some(ancestor_timeline_id) = &timeline_entry.get_ancestor_timeline_id() {
                let ancestor_children = all_branchpoints.entry(*ancestor_timeline_id).or_default();
                ancestor_children.push((
                    timeline_entry.get_ancestor_lsn(),
                    *timeline_id,
                    MaybeOffloaded::No,
                ));
            }
        });
        timelines_offloaded
            .iter()
            .for_each(|(timeline_id, timeline_entry)| {
                let Some(ancestor_timeline_id) = &timeline_entry.ancestor_timeline_id else {
                    return;
                };
                let Some(retain_lsn) = timeline_entry.ancestor_retain_lsn else {
                    return;
                };
                let ancestor_children = all_branchpoints.entry(*ancestor_timeline_id).or_default();
                ancestor_children.push((retain_lsn, *timeline_id, MaybeOffloaded::Yes));
            });

        // The number of bytes we always keep, irrespective of PITR: this is a constant across timelines
        let horizon = self.get_gc_horizon();

        // Populate each timeline's GcInfo with information about its child branches
        let timelines_to_write = if let Some(timeline_id) = restrict_to_timeline {
            itertools::Either::Left(timelines.get(&timeline_id).into_iter())
        } else {
            itertools::Either::Right(timelines.values())
        };
        for timeline in timelines_to_write {
            let mut branchpoints: Vec<(Lsn, TimelineId, MaybeOffloaded)> = all_branchpoints
                .remove(&timeline.timeline_id)
                .unwrap_or_default();

            branchpoints.sort_by_key(|b| b.0);

            let mut target = timeline.gc_info.write().unwrap();

            target.retain_lsns = branchpoints;

            let space_cutoff = timeline
                .get_last_record_lsn()
                .checked_sub(horizon)
                .unwrap_or(Lsn(0));

            target.cutoffs = GcCutoffs {
                space: space_cutoff,
                time: None,
            };
        }
    }

    async fn refresh_gc_info_internal(
        &self,
        target_timeline_id: Option<TimelineId>,
        horizon: u64,
        pitr: Duration,
        cancel: &CancellationToken,
        ctx: &RequestContext,
    ) -> Result<Vec<Arc<Timeline>>, GcError> {
        // before taking the gc_cs lock, do the heavier weight finding of gc_cutoff points for
        // currently visible timelines.
        let timelines = self
            .timelines
            .lock()
            .unwrap()
            .values()
            .filter(|tl| match target_timeline_id.as_ref() {
                Some(target) => &tl.timeline_id == target,
                None => true,
            })
            .cloned()
            .collect::<Vec<_>>();

        if target_timeline_id.is_some() && timelines.is_empty() {
            // We were to act on a particular timeline and it wasn't found
            return Err(GcError::TimelineNotFound);
        }

        let mut gc_cutoffs: HashMap<TimelineId, GcCutoffs> =
            HashMap::with_capacity(timelines.len());

        // Ensures all timelines use the same start time when computing the time cutoff.
        let now_ts_for_pitr_calc = SystemTime::now();
        for timeline in timelines.iter() {
            let ctx = &ctx.with_scope_timeline(timeline);
            let cutoff = timeline
                .get_last_record_lsn()
                .checked_sub(horizon)
                .unwrap_or(Lsn(0));

            let cutoffs = timeline
                .find_gc_cutoffs(now_ts_for_pitr_calc, cutoff, pitr, cancel, ctx)
                .await?;
            let old = gc_cutoffs.insert(timeline.timeline_id, cutoffs);
            assert!(old.is_none());
        }

        if !self.is_active() || self.cancel.is_cancelled() {
            return Err(GcError::TenantCancelled);
        }

        // grab mutex to prevent new timelines from being created here; avoid doing long operations
        // because that will stall branch creation.
        let gc_cs = self.gc_cs.lock().await;

        // Ok, we now know all the branch points.
        // Update the GC information for each timeline.
        let mut gc_timelines = Vec::with_capacity(timelines.len());
        for timeline in timelines {
            // We filtered the timeline list above
            if let Some(target_timeline_id) = target_timeline_id {
                assert_eq!(target_timeline_id, timeline.timeline_id);
            }

            {
                let mut target = timeline.gc_info.write().unwrap();

                // Cull any expired leases
                let now = SystemTime::now();
                target.leases.retain(|_, lease| !lease.is_expired(&now));

                timeline
                    .metrics
                    .valid_lsn_lease_count_gauge
                    .set(target.leases.len() as u64);

                // Look up parent's PITR cutoff to update the child's knowledge of whether it is within parent's PITR
                if let Some(ancestor_id) = timeline.get_ancestor_timeline_id() {
                    if let Some(ancestor_gc_cutoffs) = gc_cutoffs.get(&ancestor_id) {
                        target.within_ancestor_pitr =
                            Some(timeline.get_ancestor_lsn()) >= ancestor_gc_cutoffs.time;
                    }
                }

                // Update metrics that depend on GC state
                timeline
                    .metrics
                    .archival_size
                    .set(if target.within_ancestor_pitr {
                        timeline.metrics.current_logical_size_gauge.get()
                    } else {
                        0
                    });
                if let Some(time_cutoff) = target.cutoffs.time {
                    timeline.metrics.pitr_history_size.set(
                        timeline
                            .get_last_record_lsn()
                            .checked_sub(time_cutoff)
                            .unwrap_or_default()
                            .0,
                    );
                }

                // Apply the cutoffs we found to the Timeline's GcInfo.  Why might we _not_ have cutoffs for a timeline?
                // - this timeline was created while we were finding cutoffs
                // - lsn for timestamp search fails for this timeline repeatedly
                if let Some(cutoffs) = gc_cutoffs.get(&timeline.timeline_id) {
                    let original_cutoffs = target.cutoffs.clone();
                    // GC cutoffs should never go back
                    target.cutoffs = GcCutoffs {
                        space: cutoffs.space.max(original_cutoffs.space),
                        time: cutoffs.time.max(original_cutoffs.time),
                    }
                }
            }

            gc_timelines.push(timeline);
        }
        drop(gc_cs);
        Ok(gc_timelines)
    }

    /// Branch an existing timeline.
    async fn branch_timeline(
        self: &Arc<Self>,
        src_timeline: &Arc<Timeline>,
        dst_id: TimelineId,
        start_lsn: Option<Lsn>,
        ctx: &RequestContext,
    ) -> Result<CreateTimelineResult, CreateTimelineError> {
        self.branch_timeline_impl(src_timeline, dst_id, start_lsn, ctx)
            .await
    }

    async fn branch_timeline_impl(
        self: &Arc<Self>,
        src_timeline: &Arc<Timeline>,
        dst_id: TimelineId,
        start_lsn: Option<Lsn>,
        ctx: &RequestContext,
    ) -> Result<CreateTimelineResult, CreateTimelineError> {
        let src_id = src_timeline.timeline_id;

        // We will validate our ancestor LSN in this function.  Acquire the GC lock so that
        // this check cannot race with GC, and the ancestor LSN is guaranteed to remain
        // valid while we are creating the branch.
        let _gc_cs = self.gc_cs.lock().await;

        // If no start LSN is specified, we branch the new timeline from the source timeline's last record LSN
        let start_lsn = start_lsn.unwrap_or_else(|| {
            let lsn = src_timeline.get_last_record_lsn();
            info!("branching timeline {dst_id} from timeline {src_id} at last record LSN: {lsn}");
            lsn
        });

        // we finally have determined the ancestor_start_lsn, so we can get claim exclusivity now
        let timeline_create_guard = match self
            .start_creating_timeline(
                dst_id,
                CreateTimelineIdempotency::Branch {
                    ancestor_timeline_id: src_timeline.timeline_id,
                    ancestor_start_lsn: start_lsn,
                },
            )
            .await?
        {
            StartCreatingTimelineResult::CreateGuard(guard) => guard,
            StartCreatingTimelineResult::Idempotent(timeline) => {
                return Ok(CreateTimelineResult::Idempotent(timeline));
            }
        };

        // Ensure that `start_lsn` is valid, i.e. the LSN is within the PITR
        // horizon on the source timeline
        //
        // We check it against both the planned GC cutoff stored in 'gc_info',
        // and the 'latest_gc_cutoff' of the last GC that was performed.  The
        // planned GC cutoff in 'gc_info' is normally larger than
        // 'applied_gc_cutoff_lsn', but beware of corner cases like if you just
        // changed the GC settings for the tenant to make the PITR window
        // larger, but some of the data was already removed by an earlier GC
        // iteration.

        // check against last actual 'latest_gc_cutoff' first
        let applied_gc_cutoff_lsn = src_timeline.get_applied_gc_cutoff_lsn();
        {
            let gc_info = src_timeline.gc_info.read().unwrap();
            let planned_cutoff = gc_info.min_cutoff();
            if gc_info.lsn_covered_by_lease(start_lsn) {
                tracing::info!(
                    "skipping comparison of {start_lsn} with gc cutoff {} and planned gc cutoff {planned_cutoff} due to lsn lease",
                    *applied_gc_cutoff_lsn
                );
            } else {
                src_timeline
                    .check_lsn_is_in_scope(start_lsn, &applied_gc_cutoff_lsn)
                    .context(format!(
                        "invalid branch start lsn: less than latest GC cutoff {}",
                        *applied_gc_cutoff_lsn,
                    ))
                    .map_err(CreateTimelineError::AncestorLsn)?;

                // and then the planned GC cutoff
                if start_lsn < planned_cutoff {
                    return Err(CreateTimelineError::AncestorLsn(anyhow::anyhow!(
                        "invalid branch start lsn: less than planned GC cutoff {planned_cutoff}"
                    )));
                }
            }
        }

        //
        // The branch point is valid, and we are still holding the 'gc_cs' lock
        // so that GC cannot advance the GC cutoff until we are finished.
        // Proceed with the branch creation.
        //

        // Determine prev-LSN for the new timeline. We can only determine it if
        // the timeline was branched at the current end of the source timeline.
        let RecordLsn {
            last: src_last,
            prev: src_prev,
        } = src_timeline.get_last_record_rlsn();
        let dst_prev = if src_last == start_lsn {
            Some(src_prev)
        } else {
            None
        };

        // Create the metadata file, noting the ancestor of the new timeline.
        // There is initially no data in it, but all the read-calls know to look
        // into the ancestor.
        let metadata = TimelineMetadata::new(
            start_lsn,
            dst_prev,
            Some(src_id),
            start_lsn,
            *src_timeline.applied_gc_cutoff_lsn.read(), // FIXME: should we hold onto this guard longer?
            src_timeline.initdb_lsn,
            src_timeline.pg_version,
        );

        let (rel_size_v2_status, rel_size_migrated_at) = src_timeline.get_rel_size_v2_status();
        let (uninitialized_timeline, _timeline_ctx) = self
            .prepare_new_timeline(
                dst_id,
                &metadata,
                timeline_create_guard,
                start_lsn + 1,
                Some(Arc::clone(src_timeline)),
                Some(rel_size_v2_status),
                rel_size_migrated_at,
                ctx,
            )
            .await?;

        let new_timeline = uninitialized_timeline.finish_creation().await?;

        // yb-mode: the metadata lives in `pageserver.timeline_state`,
        // not in an S3 `index_part.json`. `finish_creation` already
        // upserted the row; nothing to upload here.
        let _ = metadata;

        // Callers are responsible to wait for uploads to complete and for activating the timeline.

        Ok(CreateTimelineResult::Created(new_timeline))
    }

    /// Get exclusive access to the timeline ID for creation.
    ///
    /// Timeline-creating code paths must use this function before making changes
    /// to in-memory or persistent state.
    ///
    /// The `state` parameter is a description of the timeline creation operation
    /// we intend to perform.
    /// If the timeline was already created in the meantime, we check whether this
    /// request conflicts or is idempotent , based on `state`.
    async fn start_creating_timeline(
        self: &Arc<Self>,
        new_timeline_id: TimelineId,
        idempotency: CreateTimelineIdempotency,
    ) -> Result<StartCreatingTimelineResult, CreateTimelineError> {
        let allow_offloaded = false;
        match self.create_timeline_create_guard(new_timeline_id, idempotency, allow_offloaded) {
            Ok(create_guard) => {
                pausable_failpoint!("timeline-creation-after-uninit");
                Ok(StartCreatingTimelineResult::CreateGuard(create_guard))
            }
            Err(TimelineExclusionError::ShuttingDown) => Err(CreateTimelineError::ShuttingDown),
            Err(TimelineExclusionError::AlreadyCreating) => {
                // Creation is in progress, we cannot create it again, and we cannot
                // check if this request matches the existing one, so caller must try
                // again later.
                Err(CreateTimelineError::AlreadyCreating)
            }
            Err(TimelineExclusionError::Other(e)) => Err(CreateTimelineError::Other(e)),
            Err(TimelineExclusionError::AlreadyExists {
                existing: TimelineOrOffloaded::Offloaded(_existing),
                ..
            }) => {
                info!("timeline already exists but is offloaded");
                Err(CreateTimelineError::Conflict)
            }
            Err(TimelineExclusionError::AlreadyExists {
                existing: TimelineOrOffloaded::Timeline(existing),
                arg,
            }) => {
                {
                    let existing = &existing.create_idempotency;
                    let _span = info_span!("idempotency_check", ?existing, ?arg).entered();
                    debug!("timeline already exists");

                    match (existing, &arg) {
                        // FailWithConflict => no idempotency check
                        (CreateTimelineIdempotency::FailWithConflict, _)
                        | (_, CreateTimelineIdempotency::FailWithConflict) => {
                            warn!("timeline already exists, failing request");
                            return Err(CreateTimelineError::Conflict);
                        }
                        // Idempotent <=> CreateTimelineIdempotency is identical
                        (x, y) if x == y => {
                            info!(
                                "timeline already exists and idempotency matches, succeeding request"
                            );
                            // fallthrough
                        }
                        (_, _) => {
                            warn!("idempotency conflict, failing request");
                            return Err(CreateTimelineError::Conflict);
                        }
                    }
                }

                Ok(StartCreatingTimelineResult::Idempotent(existing))
            }
        }
    }

    async fn upload_initdb(
        &self,
        timelines_path: &Utf8PathBuf,
        pgdata_path: &Utf8PathBuf,
        timeline_id: &TimelineId,
    ) -> anyhow::Result<()> {
        let temp_path = timelines_path.join(format!(
            "{INITDB_PATH}.upload-{timeline_id}.{TEMP_FILE_SUFFIX}"
        ));

        scopeguard::defer! {
            if let Err(e) = fs::remove_file(&temp_path) {
                error!("Failed to remove temporary initdb archive '{temp_path}': {e}");
            }
        }

        let (pgdata_zstd, tar_zst_size) = create_zst_tarball(pgdata_path, &temp_path).await?;
        const INITDB_TAR_ZST_WARN_LIMIT: u64 = 2 * 1024 * 1024;
        if tar_zst_size > INITDB_TAR_ZST_WARN_LIMIT {
            warn!(
                "compressed {temp_path} size of {tar_zst_size} is above limit {INITDB_TAR_ZST_WARN_LIMIT}."
            );
        }

        pausable_failpoint!("before-initdb-upload");

        backoff::retry(
            || async {
                self::remote_timeline_client::upload_initdb_dir(
                    &self.remote_storage,
                    &self.tenant_shard_id,
                    timeline_id,
                    pgdata_zstd.try_clone().await?,
                    tar_zst_size,
                    &self.cancel,
                )
                .await
            },
            |_| false,
            3,
            u32::MAX,
            "persist_initdb_tar_zst",
            &self.cancel,
        )
        .await
        .ok_or_else(|| anyhow::Error::new(TimeoutOrCancel::Cancel))
        .and_then(|x| x)
    }

    /// - run initdb to init temporary instance and get bootstrap data
    /// - after initialization completes, tar up the temp dir and upload it to S3.
    async fn bootstrap_timeline(
        self: &Arc<Self>,
        timeline_id: TimelineId,
        pg_version: PgMajorVersion,
        load_existing_initdb: Option<TimelineId>,
        ctx: &RequestContext,
    ) -> Result<CreateTimelineResult, CreateTimelineError> {
        let timeline_create_guard = match self
            .start_creating_timeline(
                timeline_id,
                CreateTimelineIdempotency::Bootstrap { pg_version },
            )
            .await?
        {
            StartCreatingTimelineResult::CreateGuard(guard) => guard,
            StartCreatingTimelineResult::Idempotent(timeline) => {
                return Ok(CreateTimelineResult::Idempotent(timeline));
            }
        };

        // create a `tenant/{tenant_id}/timelines/basebackup-{timeline_id}.{TEMP_FILE_SUFFIX}/`
        // temporary directory for basebackup files for the given timeline.

        let timelines_path = self.conf.timelines_path(&self.tenant_shard_id);
        let pgdata_path = path_with_suffix_extension(
            timelines_path.join(format!("basebackup-{timeline_id}")),
            TEMP_FILE_SUFFIX,
        );

        // Remove whatever was left from the previous runs: safe because TimelineCreateGuard guarantees
        // we won't race with other creations or existent timelines with the same path.
        if pgdata_path.exists() {
            fs::remove_dir_all(&pgdata_path).with_context(|| {
                format!("Failed to remove already existing initdb directory: {pgdata_path}")
            })?;
            tracing::info!("removed previous attempt's temporary initdb directory '{pgdata_path}'");
        }

        // this new directory is very temporary, set to remove it immediately after bootstrap, we don't need it
        let pgdata_path_deferred = pgdata_path.clone();
        scopeguard::defer! {
            if let Err(e) = fs::remove_dir_all(&pgdata_path_deferred).or_else(fs_ext::ignore_not_found) {
                // this is unlikely, but we will remove the directory on pageserver restart or another bootstrap call
                error!("Failed to remove temporary initdb directory '{pgdata_path_deferred}': {e}");
            } else {
                tracing::info!("removed temporary initdb directory '{pgdata_path_deferred}'");
            }
        }
        if let Some(existing_initdb_timeline_id) = load_existing_initdb {
            if existing_initdb_timeline_id != timeline_id {
                let source_path = &remote_initdb_archive_path(
                    &self.tenant_shard_id,
                    &existing_initdb_timeline_id,
                );
                let dest_path =
                    &remote_initdb_archive_path(&self.tenant_shard_id, &timeline_id);

                // if this fails, it will get retried by retried control plane requests
                self.remote_storage
                    .copy_object(source_path, dest_path, &self.cancel)
                    .await
                    .context("copy initdb tar")?;
            }
            let (initdb_tar_zst_path, initdb_tar_zst) =
                self::remote_timeline_client::download_initdb_tar_zst(
                    self.conf,
                    &self.remote_storage,
                    &self.tenant_shard_id,
                    &existing_initdb_timeline_id,
                    &self.cancel,
                )
                .await
                .context("download initdb tar")?;

            scopeguard::defer! {
                if let Err(e) = fs::remove_file(&initdb_tar_zst_path) {
                    error!("Failed to remove temporary initdb archive '{initdb_tar_zst_path}': {e}");
                }
            }

            let buf_read =
                BufReader::with_capacity(remote_timeline_client::BUFFER_SIZE, initdb_tar_zst);
            extract_zst_tarball(&pgdata_path, buf_read)
                .await
                .context("extract initdb tar")?;
        } else {
            // Init temporarily repo to get bootstrap data, this creates a directory in the `pgdata_path` path
            run_initdb(self.conf, &pgdata_path, pg_version, &self.cancel)
                .await
                .context("run initdb")?;

            // Upload the created data dir to S3
            if self.tenant_shard_id().is_shard_zero() {
                self.upload_initdb(&timelines_path, &pgdata_path, &timeline_id)
                    .await?;
            }
        }
        let pgdata_lsn = import_datadir::get_lsn_from_controlfile(&pgdata_path)?.align();

        // Import the contents of the data directory at the initial checkpoint
        // LSN, and any WAL after that.
        // Initdb lsn will be equal to last_record_lsn which will be set after import.
        // Because we know it upfront avoid having an option or dummy zero value by passing it to the metadata.
        let new_metadata = TimelineMetadata::new(
            Lsn(0),
            None,
            None,
            Lsn(0),
            pgdata_lsn,
            pgdata_lsn,
            pg_version,
        );
        let (mut raw_timeline, timeline_ctx) = self
            .prepare_new_timeline(
                timeline_id,
                &new_metadata,
                timeline_create_guard,
                pgdata_lsn,
                None,
                None,
                None,
                ctx,
            )
            .await?;

        let tenant_shard_id = raw_timeline.owning_tenant.tenant_shard_id;
        raw_timeline
            .write(|unfinished_timeline| async move {
                import_datadir::import_timeline_from_postgres_datadir(
                    &unfinished_timeline,
                    &pgdata_path,
                    pgdata_lsn,
                    &timeline_ctx,
                )
                .await
                .with_context(|| {
                    format!(
                        "Failed to import pgdatadir for timeline {tenant_shard_id}/{timeline_id}"
                    )
                })?;

                fail::fail_point!("before-checkpoint-new-timeline", |_| {
                    Err(CreateTimelineError::Other(anyhow::anyhow!(
                        "failpoint before-checkpoint-new-timeline"
                    )))
                });

                Ok(())
            })
            .await?;

        // All done!
        let timeline = raw_timeline.finish_creation().await?;

        // Callers are responsible to wait for uploads to complete and for activating the timeline.

        Ok(CreateTimelineResult::Created(timeline))
    }

    /// Builds required resources for a new timeline.
    pub(crate) fn build_timeline_resources(&self, _timeline_id: TimelineId) -> TimelineResources {
        TimelineResources {
            pagestream_throttle: self.pagestream_throttle.clone(),
            pagestream_throttle_metrics: self.pagestream_throttle_metrics.clone(),
            basebackup_cache: self.basebackup_cache.clone(),
            compute_page_store: self.compute_page_store.clone(),
            fpu_page_store: self.fpu_page_store.clone(),
        }
    }

    /// Creates intermediate timeline structure and its files.
    ///
    /// An empty layer map is initialized, and new data and WAL can be imported starting
    /// at 'disk_consistent_lsn'. After any initial data has been imported, call
    /// `finish_creation` to insert the Timeline into the timelines map.
    #[allow(clippy::too_many_arguments)]
    async fn prepare_new_timeline<'a>(
        &'a self,
        new_timeline_id: TimelineId,
        new_metadata: &TimelineMetadata,
        create_guard: TimelineCreateGuard,
        start_lsn: Lsn,
        ancestor: Option<Arc<Timeline>>,
        rel_size_v2_status: Option<RelSizeMigration>,
        rel_size_migrated_at: Option<Lsn>,
        ctx: &RequestContext,
    ) -> anyhow::Result<(UninitializedTimeline<'a>, RequestContext)> {
        let tenant_shard_id = self.tenant_shard_id;

        let resources = self.build_timeline_resources(new_timeline_id);
        let _ = (new_metadata, &rel_size_v2_status, rel_size_migrated_at);

        let (timeline_struct, timeline_ctx) = self
            .create_timeline_struct(
                new_timeline_id,
                new_metadata,
                ancestor,
                resources,
                create_guard.idempotency.clone(),
                None,
                rel_size_v2_status,
                rel_size_migrated_at,
                ctx,
            )
            .context("Failed to create timeline data structure")?;

        timeline_struct.init_empty_layer_map(start_lsn);

        if let Err(e) = self
            .create_timeline_files(&create_guard.timeline_path)
            .await
        {
            error!(
                "Failed to create initial files for timeline {tenant_shard_id}/{new_timeline_id}, cleaning up: {e:?}"
            );
            cleanup_timeline_directory(create_guard);
            return Err(e);
        }

        debug!(
            "Successfully created initial files for timeline {tenant_shard_id}/{new_timeline_id}"
        );

        Ok((
            UninitializedTimeline::new(
                self,
                new_timeline_id,
                Some((timeline_struct, create_guard)),
            ),
            timeline_ctx,
        ))
    }

    async fn create_timeline_files(&self, timeline_path: &Utf8Path) -> anyhow::Result<()> {
        crashsafe::create_dir(timeline_path).context("Failed to create timeline directory")?;

        fail::fail_point!("after-timeline-dir-creation", |_| {
            anyhow::bail!("failpoint after-timeline-dir-creation");
        });

        Ok(())
    }

    /// Get a guard that provides exclusive access to the timeline directory, preventing
    /// concurrent attempts to create the same timeline.
    ///
    /// The `allow_offloaded` parameter controls whether to tolerate the existence of
    /// offloaded timelines or not.
    fn create_timeline_create_guard(
        self: &Arc<Self>,
        timeline_id: TimelineId,
        idempotency: CreateTimelineIdempotency,
        allow_offloaded: bool,
    ) -> Result<TimelineCreateGuard, TimelineExclusionError> {
        let tenant_shard_id = self.tenant_shard_id;

        let timeline_path = self.conf.timeline_path(&tenant_shard_id, &timeline_id);

        let create_guard = TimelineCreateGuard::new(
            self,
            timeline_id,
            timeline_path.clone(),
            idempotency,
            allow_offloaded,
        )?;

        // At this stage, we have got exclusive access to in-memory state for this timeline ID
        // for creation.
        // A timeline directory should never exist on disk already:
        // - a previous failed creation would have cleaned up after itself
        // - a pageserver restart would clean up timeline directories that don't have valid remote state
        //
        // Therefore it is an unexpected internal error to encounter a timeline directory already existing here,
        // this error may indicate a bug in cleanup on failed creations.
        if timeline_path.exists() {
            return Err(TimelineExclusionError::Other(anyhow::anyhow!(
                "Timeline directory already exists! This is a bug."
            )));
        }

        Ok(create_guard)
    }

    /// Flush any in-progress layers, schedule uploads, and wait for uploads to complete.
    ///
    /// This function can take a long time: callers should wrap it in a timeout if calling
    /// from an external API handler.
    ///
    /// Cancel-safety: cancelling this function may leave I/O running, but such I/O is
    /// still bounded by tenant/timeline shutdown.
    #[tracing::instrument(skip_all)]
    pub(crate) async fn flush_remote(&self) -> anyhow::Result<()> {
        let timelines = self.timelines.lock().unwrap().clone();

        async fn flush_timeline(_gate: GateGuard, timeline: Arc<Timeline>) -> anyhow::Result<()> {
            tracing::info!(timeline_id=%timeline.timeline_id, "Flushing...");
            timeline.freeze_and_flush().await?;
            // yb-mode: no asynchronous uploads to wait on; flush is
            // synchronous against YB.
            Ok(())
        }

        // We do not use a JoinSet for these tasks, because we don't want them to be
        // aborted when this function's future is cancelled: they should stay alive
        // holding their GateGuard until they complete, to ensure their I/Os complete
        // before Timeline shutdown completes.
        let mut results = FuturesUnordered::new();

        for (_timeline_id, timeline) in timelines {
            // Run each timeline's flush in a task holding the timeline's gate: this
            // means that if this function's future is cancelled, the Timeline shutdown
            // will still wait for any I/O in here to complete.
            let Ok(gate) = timeline.gate.enter() else {
                continue;
            };
            let jh = tokio::task::spawn(async move { flush_timeline(gate, timeline).await });
            results.push(jh);
        }

        while let Some(r) = results.next().await {
            if let Err(e) = r {
                if !e.is_cancelled() && !e.is_panic() {
                    tracing::error!("unexpected join error: {e:?}");
                }
            }
        }

        // The flushes we did above were just writes, but the TenantShard might have had
        // pending deletions as well from recent compaction/gc: we want to flush those
        // as well.  This requires flushing the global delete queue.  This is cheap
        // because it's typically a no-op.
        match self.deletion_queue_client.flush_execute().await {
            Ok(_) => {}
            Err(DeletionQueueError::ShuttingDown) => {}
        }

        Ok(())
    }

    /// How much local storage would this tenant like to have?  It can cope with
    /// less than this (via eviction and on-demand downloads), but this function enables
    /// the TenantShard to advertise how much storage it would prefer to have to provide fast I/O
    /// by keeping important things on local disk.
    ///
    /// This is a heuristic, not a guarantee: tenants that are long-idle will actually use less
    /// than they report here, due to layer eviction.  Tenants with many active branches may
    /// actually use more than they report here.
    pub(crate) fn local_storage_wanted(&self) -> u64 {
        let timelines = self.timelines.lock().unwrap();

        // Heuristic: we use the max() of the timelines' visible sizes, rather than the sum.  This
        // reflects the observation that on tenants with multiple large branches, typically only one
        // of them is used actively enough to occupy space on disk.
        timelines
            .values()
            .map(|t| t.metrics.visible_physical_size_gauge.get())
            .max()
            .unwrap_or(0)
    }

    /// HADRON
    /// Return the visible size of all timelines in this tenant.
    pub(crate) fn get_visible_size(&self) -> u64 {
        let timelines = self.timelines.lock().unwrap();
        timelines
            .values()
            .map(|t| t.metrics.visible_physical_size_gauge.get())
            .sum()
    }

    /// Builds a new tenant manifest, and uploads it if it differs from the last-known tenant
    /// manifest in `Self::remote_tenant_manifest`.
    ///
    /// TODO: instead of requiring callers to remember to call `maybe_upload_tenant_manifest` after
    /// changing any `TenantShard` state that's included in the manifest, consider making the manifest
    /// the authoritative source of data with an API that automatically uploads on changes. Revisit
    /// this when the manifest is more widely used and we have a better idea of the data model.
    pub(crate) async fn maybe_upload_tenant_manifest(&self) -> Result<(), TenantManifestError> {
        // Multiple tasks may call this function concurrently after mutating the TenantShard runtime
        // state, affecting the manifest generated by `build_tenant_manifest`. We use an async mutex
        // to serialize these callers. `eq_ignoring_version` acts as a slightly inefficient but
        // simple coalescing mechanism.
        let mut guard = tokio::select! {
            guard = self.remote_tenant_manifest.lock() => guard,
            _ = self.cancel.cancelled() => return Err(TenantManifestError::Cancelled),
        };

        // Build a new manifest.
        let manifest = self.build_tenant_manifest();

        // Check if the manifest has changed. We ignore the version number here, to avoid
        // uploading every manifest on version number bumps.
        if let Some(old) = guard.as_ref() {
            if manifest.eq_ignoring_version(old) {
                return Ok(());
            }
        }

        // Update metrics
        let tid = self.tenant_shard_id.to_string();
        let shard_id = self.tenant_shard_id.shard_slug().to_string();
        let set_key = &[tid.as_str(), shard_id.as_str()][..];
        TENANT_OFFLOADED_TIMELINES
            .with_label_values(set_key)
            .set(manifest.offloaded_timelines.len() as u64);

        // Upload the manifest. Remote storage does no retries internally, so retry here.
        match backoff::retry(
            || async {
                upload_tenant_manifest(
                    &self.remote_storage,
                    &self.tenant_shard_id,
                    self.generation,
                    &manifest,
                    &self.cancel,
                )
                .await
            },
            |_| self.cancel.is_cancelled(),
            FAILED_UPLOAD_WARN_THRESHOLD,
            FAILED_REMOTE_OP_RETRIES,
            "uploading tenant manifest",
            &self.cancel,
        )
        .await
        {
            None => Err(TenantManifestError::Cancelled),
            Some(Err(_)) if self.cancel.is_cancelled() => Err(TenantManifestError::Cancelled),
            Some(Err(e)) => Err(TenantManifestError::RemoteStorage(e)),
            Some(Ok(_)) => {
                // Store the successfully uploaded manifest, so that future callers can avoid
                // re-uploading the same thing.
                *guard = Some(manifest);

                Ok(())
            }
        }
    }
}

/// Create the cluster temporarily in 'initdbpath' directory inside the repository
/// to get bootstrap data for timeline initialization.
async fn run_initdb(
    conf: &'static PageServerConf,
    initdb_target_dir: &Utf8Path,
    pg_version: PgMajorVersion,
    cancel: &CancellationToken,
) -> Result<(), InitdbError> {
    let initdb_bin_path = conf
        .pg_bin_dir(pg_version)
        .map_err(InitdbError::Other)?
        .join("initdb");
    let initdb_lib_dir = conf.pg_lib_dir(pg_version).map_err(InitdbError::Other)?;
    info!(
        "running {} in {}, libdir: {}",
        initdb_bin_path, initdb_target_dir, initdb_lib_dir,
    );

    let _permit = {
        let _timer = INITDB_SEMAPHORE_ACQUISITION_TIME.start_timer();
        INIT_DB_SEMAPHORE.acquire().await
    };

    CONCURRENT_INITDBS.inc();
    scopeguard::defer! {
        CONCURRENT_INITDBS.dec();
    }

    let _timer = INITDB_RUN_TIME.start_timer();
    let res = postgres_initdb::do_run_initdb(postgres_initdb::RunInitdbArgs {
        superuser: &conf.superuser,
        locale: &conf.locale,
        initdb_bin: &initdb_bin_path,
        pg_version,
        library_search_path: &initdb_lib_dir,
        pgdata: initdb_target_dir,
    })
    .await
    .map_err(InitdbError::Inner);

    // This isn't true cancellation support, see above. Still return an error to
    // excercise the cancellation code path.
    if cancel.is_cancelled() {
        return Err(InitdbError::Cancelled);
    }

    res
}


