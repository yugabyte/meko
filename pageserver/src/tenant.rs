//! Timeline repository implementation that keeps old data in layer files, and
//! the recent changes in ephemeral files.
//!
//! See tenant/*_layer.rs files. The functions here are responsible for locating
//! the correct layer for the get/put call, walking back the timeline branching
//! history as needed.
//!
//! The files are stored in the .yb_light/tenants/<tenant_id>/timelines/<timeline_id>
//! directory. See docs/pageserver-storage.md for how the files are managed.
//! In addition to the layer files, there is a metadata file in the same
//! directory that contains information about the timeline, in particular its
//! parent timeline, and the last LSN that has been written to disk.
//!

use std::collections::hash_map::Entry;
use std::collections::{BTreeMap, HashMap, HashSet};
use std::fmt::Debug;
use std::fs;
use std::sync::{Arc, Mutex, Weak};
use std::time::{Duration, Instant, SystemTime};

use anyhow::{Context, bail};
use arc_swap::ArcSwap;
use camino::{Utf8Path, Utf8PathBuf};
use futures::StreamExt;
use futures::stream::FuturesUnordered;
use itertools::Itertools as _;
use once_cell::sync::Lazy;
pub use pageserver_api::models::TenantState;
use pageserver_api::models::RelSizeMigration;
use pageserver_api::models::ShardIdentity;
use pageserver_api::models::TimelineState;
use postgres_ffi::PgMajorVersion;
use remote_storage::{DownloadError, GenericRemoteStorage, TimeoutOrCancel};
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

use self::metadata::TimelineMetadata;
use self::mgr::{GetActiveTenantError, GetTenantError};
use self::timeline::uninit::{TimelineCreateGuard, TimelineExclusionError, UninitializedTimeline};
use self::timeline::{GcCutoffs, TimelineResources};
use crate::basebackup_cache::BasebackupCache;
use crate::config::PageServerConf;
use crate::context;
use crate::context::RequestContextBuilder;
use crate::context::{DownloadBehavior, RequestContext};
use crate::metrics::{
    BROKEN_TENANTS_SET, CONCURRENT_INITDBS, INITDB_RUN_TIME, INITDB_SEMAPHORE_ACQUISITION_TIME,
    TENANT, TENANT_STATE_METRIC, remove_tenant_metrics,
};
use crate::task_mgr::TaskKind;
use crate::tenant::gc_result::GcResult;
use initdb_storage::{INITDB_PATH, remote_initdb_archive_path};

use crate::tenant::timeline::delete::DeleteTimelineFlow;
use crate::tenant::timeline::uninit::cleanup_timeline_directory;
use crate::walingest::WalLagCooldown;
use crate::walredo::{PostgresRedoManager, RedoAttemptType};
use crate::{InitializationOrder, TEMP_FILE_SUFFIX, import_datadir, span, task_mgr, walredo};

static INIT_DB_SEMAPHORE: Lazy<Semaphore> = Lazy::new(|| Semaphore::new(8));
use utils::crashsafe;
use utils::generation::Generation;
use utils::id::{TenantId, TimelineId};
use utils::lsn::Lsn;

pub mod metadata;
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

/// Parts of the `.yb_light/tenants/<tenant_id>/timelines/<timeline_id>` directory prefix.
pub const TIMELINES_SEGMENT_NAME: &str = "timelines";

/// References to shared objects that are passed into each tenant, such
/// as the shared remote storage client and process initialization state.
#[derive(Clone)]
pub struct TenantSharedResources {
    pub remote_storage: GenericRemoteStorage,
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


    fn is_gc_blocked_by_lsn_lease_deadline(&self) -> bool {
        self.lsn_lease_deadline
            .map(|d| tokio::time::Instant::now() < d)
            .unwrap_or(false)
    }
}
struct TimelinePreload {
    metadata: Result<TimelineMetadata, DownloadError>,
}

pub(crate) struct TenantPreload {
    /// Map from timeline ID to a possible timeline preload. It is None iff the timeline is offloaded according to the manifest.
    timelines: HashMap<TimelineId, Option<TimelinePreload>>,
}

/// Build a `TimelineMetadata` from a `pageserver.timeline_state` row.
/// In yb-mode the row *is* the timeline metadata; everything that
/// used to ride on `index_part.json` either lives in the row or is
/// dead weight.
fn synth_timeline_metadata_from_row(
    row: &crate::page_store::TimelineRow,
) -> anyhow::Result<TimelineMetadata> {
    let pg_major = match row.pg_version {
        14 => PgMajorVersion::PG14,
        15 => PgMajorVersion::PG15,
        16 => PgMajorVersion::PG16,
        17 => PgMajorVersion::PG17,
        v => anyhow::bail!("timeline_state.pg_version is {v}, not a supported PG major version"),
    };
    Ok(TimelineMetadata::new(
        row.consistent_lsn,
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
        row.initdb_lsn.unwrap_or(row.consistent_lsn),
        pg_major,
    ))
}

/// How to spawn the tenant. Single-timeline pageservers always attach
/// eagerly: the launcher's storage_controller polls for Active right
/// after spawn, and there is only one timeline to warm up.
pub(crate) enum SpawnMode {
    /// Activate as soon as possible
    Eager,
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

    id: TenantId,

    // The detailed sharding information, beyond the number/count in id
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
    /// **Lock order**: if acquiring both, acquire `timelines` before `timelines_creating`.
    timelines_creating: std::sync::Mutex<HashSet<TimelineId>>,

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
        write!(f, "{} ({})", self.id, self.current_state())
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
    #[allow(dead_code)]
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
    async fn timeline_init_and_sync(
        self: &Arc<Self>,
        timeline_id: TimelineId,
        resources: TimelineResources,
        metadata: TimelineMetadata,
        ancestor: Option<Arc<Timeline>>,
        cause: LoadTimelineCause,
        ctx: &RequestContext,
    ) -> anyhow::Result<TimelineInitAndSyncResult> {
        let tenant_id = self.id;

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

        // Build the ancestry lineage for lineage-aware reads. This is
        // the open-existing path, so the timeline_state row (and its
        // ancestor chain) is guaranteed to exist.
        let mut resources = resources;
        resources.lineage = Some(
            self.compute_page_store
                .load_lineage(tenant_id, timeline_id)
                .await
                .context("load timeline lineage")?,
        );

        let (timeline, _timeline_ctx) = self.create_timeline_struct(
            timeline_id,
            &metadata,
            ancestor.clone(),
            resources,
            idempotency.clone(),
            None, // rel_size_v2_status — always None in yb-mode
            None, // rel_size_migrated_at — always None in yb-mode
            ctx,
        )?;
        let consistent_lsn_seed = timeline.get_applied_lsn();

        if !consistent_lsn_seed.is_valid() {
            anyhow::bail!("Timeline {tenant_id}/{timeline_id} has invalid consistent_lsn seed");
        }

        assert_eq!(
            consistent_lsn_seed,
            metadata.disk_consistent_lsn(),
            "applied_lsn is seeded from the persisted consistent_lsn at construction"
        );

        // yb-mode: no upload queue to init. The PageWriter takes the
        // `remote_state.consistent_lsn` slot directly so flush success
        // advances the durable, gap-free watermark that feeds SK
        // feedback and restart-resume.
        if let Some(pw) = timeline.page_writer.as_ref() {
            pw.set_consistent_lsn_slot(timeline.remote_state.consistent_lsn.clone());
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
            remote_storage,
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

                let SpawnMode::Eager = mode;
                let preload = {
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
                        tenant_clone.activate(None, &ctx);
                    }
                    Err(e) => make_broken_or_stopping(&tenant_clone, anyhow::anyhow!(e)),
                }

                Ok(())
            }
            .instrument(tracing::info_span!(parent: None, "attach", tenant_id=%tenant_shard_id, gen=?generation)),
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

        // Single-timeline pageserver: load ONLY the configured
        // timeline's row. The tenant may have other rows in
        // `pageserver.timeline_state` (ancestors and branches served
        // by other pageserver processes) — those are reached through
        // lineage-aware PageStore reads, never hosted here.
        let tenant_id = self.id;
        let identity = self
            .conf
            .timeline_identity
            .context("preload requires a timeline identity in pageserver.toml")?;
        anyhow::ensure!(
            identity.tenant_id == tenant_id,
            "preload: tenant {} does not match configured identity {}",
            tenant_id,
            identity.tenant_id,
        );
        let row = tokio::select! {
            r = self.compute_page_store.get_timeline(tenant_id, identity.timeline_id) => r?,
            _ = cancel.cancelled() => anyhow::bail!("cancelled"),
        };
        let mut timelines = HashMap::with_capacity(1);
        match row {
            Some(row) => {
                if row.deleted_at.is_some() {
                    anyhow::bail!(
                        "timeline {} is marked deleted (deleted_at = {:?}); refusing to open",
                        row.timeline_id,
                        row.deleted_at,
                    );
                }
                info!(
                    timeline_id = %row.timeline_id,
                    consistent_lsn = %row.consistent_lsn,
                    prev_record_lsn = ?row.prev_record_lsn,
                    applied_gc_cutoff_lsn = %row.applied_gc_cutoff_lsn,
                    ancestor_lsn = ?row.ancestor_lsn,
                    initdb_lsn = ?row.initdb_lsn,
                    "preload: timeline LSNs read from pageserver.timeline_state"
                );
                let metadata = synth_timeline_metadata_from_row(&row)?;
                timelines.insert(
                    row.timeline_id,
                    Some(TimelinePreload {
                        metadata: Ok(metadata),
                    }),
                );
            }
            None => {
                // Row absent: attach() runs the [timeline_init]
                // create-if-missing path (bootstrap or branch).
                info!(
                    timeline_id = %identity.timeline_id,
                    "preload: no timeline_state row yet; will create at attach if configured"
                );
            }
        }
        Ok(TenantPreload { timelines })
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

        let mut timeline_ancestors = HashMap::new();
        for (timeline_id, preload) in preload.timelines {
            let Some(preload) = preload else { continue };
            // `preload.metadata` is synthesised from
            // `pageserver.timeline_state` in `Tenant::preload`, so it's
            // always `Ok(_)` — no remote blob download path remains in yb-mode.
            let metadata = preload
                .metadata
                .expect("yb-mode preload always builds an Ok TimelineMetadata");
            timeline_ancestors.insert(timeline_id, metadata);
        }

        // Create-if-missing: the configured timeline has no
        // `pageserver.timeline_state` row yet. Create it per the
        // `[timeline_init]` block of pageserver.toml — bootstrap runs
        // initdb + import (and inserts the row via finish_creation);
        // branch only writes a row pointing at the ancestor (the data
        // is reached via lineage-aware reads).
        let identity = self
            .conf
            .timeline_identity
            .context("attach requires a timeline identity in pageserver.toml")?;
        let mut bootstrapped_timeline = None;
        if !timeline_ancestors.contains_key(&identity.timeline_id) {
            use pageserver_api::config::TimelineInitConfig;
            match &self.conf.timeline_init {
                None => anyhow::bail!(
                    "timeline {} has no timeline_state row and no [timeline_init] is \
                     configured in pageserver.toml",
                    identity.timeline_id,
                ),
                Some(TimelineInitConfig::Bootstrap { pg_version }) => {
                    // Crash-retry idempotency: if a previous bootstrap
                    // attempt uploaded the initdb tar but crashed before
                    // the row insert, REUSE that tar. initdb output is
                    // not bit-deterministic, and the orphan page rows
                    // from the failed import must be overwritten with
                    // identical payloads on retry.
                    let initdb_blob = initdb_storage::remote_initdb_archive_path(
                        &self.id,
                        &identity.timeline_id,
                    );
                    let load_existing_initdb = match self
                        .remote_storage
                        .head_object(&initdb_blob, &self.cancel)
                        .await
                    {
                        Ok(_) => {
                            info!(
                                timeline_id = %identity.timeline_id,
                                "bootstrap: reusing initdb tar from a previous attempt"
                            );
                            Some(identity.timeline_id)
                        }
                        Err(DownloadError::NotFound) => None,
                        Err(e) => {
                            return Err(anyhow::anyhow!(e)
                                .context("probe initdb tar before bootstrap"));
                        }
                    };
                    info!(
                        timeline_id = %identity.timeline_id,
                        pg_version = %pg_version,
                        "bootstrapping configured timeline (initdb + import)"
                    );
                    // The nested span carries timeline_id: the timeline
                    // creation internals (UninitializedTimeline::write,
                    // flush loop spawn) debug-assert that both tenant_id
                    // and timeline_id are on the current span. The
                    // surrounding attach span only has tenant_id.
                    let result = self
                        .bootstrap_timeline(
                            identity.timeline_id,
                            *pg_version,
                            load_existing_initdb,
                            ctx,
                        )
                        .instrument(info_span!("bootstrap_timeline", timeline_id = %identity.timeline_id))
                        .await
                        .map_err(|e| anyhow::anyhow!("bootstrap configured timeline: {e}"))?;
                    // finish_creation inserted the timeline into
                    // self.timelines and wrote the timeline_state row;
                    // activation happens in TenantShard::activate.
                    let (CreateTimelineResult::Created(t)
                    | CreateTimelineResult::Idempotent(t)) = result;
                    bootstrapped_timeline = Some(t.timeline_id);
                }
                Some(TimelineInitConfig::Branch {
                    ancestor_timeline_id,
                    ancestor_lsn,
                }) => {
                    let generation_i32: i32 = self
                        .generation
                        .into()
                        .map(|g: u32| g as i32)
                        .unwrap_or(0);
                    info!(
                        timeline_id = %identity.timeline_id,
                        ancestor = %ancestor_timeline_id,
                        %ancestor_lsn,
                        "creating branch timeline row"
                    );
                    let row = self
                        .fpu_page_store
                        .create_branch_row(
                            self.id,
                            identity.timeline_id,
                            *ancestor_timeline_id,
                            *ancestor_lsn,
                            generation_i32,
                        )
                        .await
                        .context("create branch timeline row")?;
                    let metadata = synth_timeline_metadata_from_row(&row)?;
                    timeline_ancestors.insert(identity.timeline_id, metadata);
                }
            }
        }
        let _ = bootstrapped_timeline;

        // gc_blocking lived on index_part.json; yb-mode has no source for
        // it yet, so initialise empty.
        let gc_blocks: HashMap<TimelineId, enumset::EnumSet<gc_block::GcBlockingReason>> =
            HashMap::new();

        let sorted_timelines = tree_sort_timelines(timeline_ancestors, |m| m.ancestor_timeline())?;
        for (timeline_id, remote_metadata) in sorted_timelines {
            // TODO again handle early failure
            let effect = self
                .load_remote_timeline(
                    timeline_id,
                    remote_metadata,
                    self.build_timeline_resources(timeline_id),
                    LoadTimelineCause::Attach,
                    ctx,
                )
                .await
                .with_context(|| {
                    format!(
                        "failed to load remote timeline {} for tenant {}",
                        timeline_id, self.id
                    )
                })?;

            let TimelineInitAndSyncResult::ReadyToActivate = effect;
            // activation happens later, on Tenant::activate
        }

        // At this point we've initialized all timelines and are tracking them.
        // Now compute the layer visibility for all (not offloaded) timelines.
        let compute_visiblity_for = {
            let timelines_accessor = self.timelines.lock().unwrap();
            // Before activation, populate each Timeline's GcInfo with information about its children
            self.initialize_gc_info(&timelines_accessor, None);
            timelines_accessor.values().cloned().collect::<Vec<_>>()
        };

        // update_layer_visibility removed in phase 4d.2.
        let _ = compute_visiblity_for;

        // Resume-deletion path is dead in yb-mode: synth never produces
        // `MaybeDeletedIndexPart::Deleted`, so there's nothing to
        // restart here. If we want crash-safe deletion in yb-mode,
        // surface `timeline_state.deleted_at` at preload time instead.

        // yb-mode is the only path; we never wrote layer files, never
        // created per-timeline dentries, nothing to reconcile. The
        // tenant manifest was a remote-storage blob that gated
        // offloaded-timeline tracking; offload isn't supported here
        // so there's nothing to write.

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
    async fn load_remote_timeline(
        self: &Arc<Self>,
        timeline_id: TimelineId,
        remote_metadata: TimelineMetadata,
        resources: TimelineResources,
        cause: LoadTimelineCause,
        ctx: &RequestContext,
    ) -> anyhow::Result<TimelineInitAndSyncResult> {
        span::debug_assert_current_span_has_tenant_id();

        info!("loading timeline {}", timeline_id);

        // Single-timeline pageserver: a branch's ancestor is served by
        // a DIFFERENT pageserver process and is never hosted here. The
        // in-process Arc link is therefore best-effort (present only in
        // multi-timeline test setups); reads below the branch point go
        // through lineage-aware PageStore reads instead.
        let ancestor = remote_metadata
            .ancestor_timeline()
            .and_then(|ancestor_id| self.timelines.lock().unwrap().get(&ancestor_id).cloned());

        self.timeline_init_and_sync(
            timeline_id,
            resources,
            remote_metadata,
            ancestor,
            cause,
            ctx,
        )
        .await
    }

    #[allow(dead_code)]
    pub(crate) fn tenant_shard_id(&self) -> TenantId {
        self.id
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
                tenant_id: self.id,
                timeline_id,
            })?;

        if active_only && !timeline.is_active() {
            Err(GetTimelineError::NotActive {
                tenant_id: self.id,
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

    pub fn list_timeline_ids(&self) -> Vec<TimelineId> {
        self.timelines.lock().unwrap().keys().cloned().collect()
    }

    /// Helper for unit tests to create a timeline with some pre-loaded states.

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

        if self
            .tenant_conf
            .load()
            .is_gc_blocked_by_lsn_lease_deadline()
        {
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

    /// Changes tenant status to active, unless shutdown was already requested.
    ///
    /// `background_jobs_can_start` is an optional barrier set to a value during pageserver startup
    /// to delay background jobs. Background jobs can be started right away when None is given.
    fn activate(
        self: &Arc<Self>,
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
            debug!(tenant_id = %self.id, "Activating tenant");
            activating = true;
            // Continue outside the closure. We need to grab timelines.lock()
            // and we plan to turn it into a tokio::sync::Mutex in a future patch.
        });

        if activating {
            let timelines_accessor = self.timelines.lock().unwrap();
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
                    background_jobs_can_start,
                    &ctx.with_scope_timeline(timeline),
                );
                activated_timelines += 1;
            }

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
                    tenant_id = %self.id,
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
                    tracing::error!("TENANT TASK ENDED WITH UNEXPECTED JoinError: {je:?}");
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
        task_mgr::shutdown_tasks(None, Some(self.id), None).await;

        if let Some(walredo_mgr) = self.walredo_mgr.as_ref() {
            walredo_mgr.shutdown().await;
        }

        // Wait for any in-flight operations to complete
        self.gate.close().await;

        remove_tenant_metrics(&self.id);

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
                                GetActiveTenantError::NotFound(GetTenantError::ShardNotFound(self.id))
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

    #[allow(dead_code)]
    pub(crate) fn get_shard_identity(&self) -> ShardIdentity {
        self.shard_identity
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
        rel_size_v2_status: Option<RelSizeMigration>,
        rel_size_migrated_at: Option<Lsn>,
        ctx: &RequestContext,
    ) -> anyhow::Result<(Arc<Timeline>, RequestContext)> {
        let ancestor_id = new_metadata.ancestor_timeline();
        // If an in-process ancestor Arc is supplied it must match the
        // metadata; its ABSENCE is fine — branches read their ancestor
        // through lineage-aware PageStore reads, not an in-process link.
        if let Some(ancestor) = ancestor.as_ref() {
            anyhow::ensure!(
                ancestor_id == Some(ancestor.timeline_id),
                "Timeline's {new_timeline_id} ancestor {ancestor_id:?} does not match \
                 supplied ancestor {}",
                ancestor.timeline_id,
            );
        }
        let state = TimelineState::Loading;

        let pg_version = new_metadata.pg_version();

        let timeline = Timeline::new(
            self.conf,
            Arc::clone(&self.tenant_conf),
            new_metadata,
            ancestor,
            new_timeline_id,
            self.id,
            self.generation,
            self.shard_identity,
            self.walredo_mgr.clone(),
            resources,
            pg_version,
            state,
            self.attach_wal_lag_cooldown.clone(),
            create_idempotency,
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
        id: TenantId,
        remote_storage: GenericRemoteStorage,
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

            let tid = id.to_string();
            let set_key = &[tid.as_str()][..];

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
            id,
            shard_identity,
            generation: attached_conf.generation,
            conf,
            // using now here is good enough approximation to catch tenants with really long
            // activation times.
            constructed_at: Instant::now(),
            timelines: Mutex::new(HashMap::new()),
            timelines_creating: Mutex::new(HashSet::new()),
            gc_cs: tokio::sync::Mutex::new(()),
            walredo_mgr,
            remote_storage,
            state,
            activate_now_sem: tokio::sync::Semaphore::new(0),
            attach_wal_lag_cooldown: Arc::new(std::sync::OnceLock::new()),
            cancel: CancellationToken::default(),
            gate: Gate::default(),
            pagestream_throttle: Arc::new(throttle::Throttle::new(
                TenantShard::get_pagestream_throttle_config(conf, &attached_conf.tenant_conf),
            )),
            pagestream_throttle_metrics: Arc::new(
                crate::metrics::tenant_throttling::Pagestream::new(&id),
            ),
            tenant_conf: Arc::new(ArcSwap::from_pointee(attached_conf)),
            gc_block: Default::default(),
            basebackup_cache,
            compute_page_store,
            fpu_page_store,
        }
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
        restrict_to_timeline: Option<TimelineId>,
    ) {
        if restrict_to_timeline.is_none() {
            // This function must be called before activation: after activation timeline create/delete operations
            // might happen, and this function is not safe to run concurrently with those.
            assert!(!self.is_active());
        }

        // Scan all timelines. For each timeline, remember the timeline ID and
        // the branch point where it was created.
        let mut all_branchpoints: BTreeMap<TimelineId, Vec<(Lsn, TimelineId)>> = BTreeMap::new();
        timelines.iter().for_each(|(timeline_id, timeline_entry)| {
            if let Some(ancestor_timeline_id) = &timeline_entry.get_ancestor_timeline_id() {
                let ancestor_children = all_branchpoints.entry(*ancestor_timeline_id).or_default();
                ancestor_children.push((timeline_entry.get_ancestor_lsn(), *timeline_id));
            }
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
            let mut branchpoints: Vec<(Lsn, TimelineId)> = all_branchpoints
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
            Err(TimelineExclusionError::AlreadyExists { existing, arg }) => {
                {
                    let existing = &existing.create_idempotency;
                    let _span = info_span!("idempotency_check", ?existing, ?arg).entered();
                    debug!("timeline already exists");

                    match (existing, &arg) {
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
                initdb_storage::upload_initdb_dir(
                    &self.remote_storage,
                    &self.id,
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

        let timelines_path = self.conf.timelines_path(&self.id);
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
                let source_path =
                    &remote_initdb_archive_path(&self.id, &existing_initdb_timeline_id);
                let dest_path = &remote_initdb_archive_path(&self.id, &timeline_id);

                // if this fails, it will get retried by retried control plane requests
                self.remote_storage
                    .copy_object(source_path, dest_path, &self.cancel)
                    .await
                    .context("copy initdb tar")?;
            }
            let timeline_path = self.conf.timelines_path(&self.id);
            let (initdb_tar_zst_path, initdb_tar_zst) = initdb_storage::download_initdb_tar_zst(
                &timeline_path,
                &self.remote_storage,
                &self.id,
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

            let buf_read = BufReader::with_capacity(initdb_storage::BUFFER_SIZE, initdb_tar_zst);
            extract_zst_tarball(&pgdata_path, buf_read)
                .await
                .context("extract initdb tar")?;
        } else {
            // Init temporarily repo to get bootstrap data, this creates a directory in the `pgdata_path` path
            run_initdb(self.conf, &pgdata_path, pg_version, &self.cancel)
                .await
                .context("run initdb")?;

            // Upload the created data dir to S3
            self.upload_initdb(&timelines_path, &pgdata_path, &timeline_id)
                .await?;
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

        let tenant_shard_id = raw_timeline.owning_tenant.id;
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
            lineage: None,
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
        let tenant_shard_id = self.id;

        let resources = self.build_timeline_resources(new_timeline_id);
        let _ = (new_metadata, &rel_size_v2_status, rel_size_migrated_at);

        let (timeline_struct, timeline_ctx) = self
            .create_timeline_struct(
                new_timeline_id,
                new_metadata,
                ancestor,
                resources,
                create_guard.idempotency.clone(),
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
        let tenant_shard_id = self.id;

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

    /// Return the visible size of all timelines in this tenant.
    pub(crate) fn get_visible_size(&self) -> u64 {
        let timelines = self.timelines.lock().unwrap();
        timelines
            .values()
            .map(|t| t.metrics.visible_physical_size_gauge.get())
            .sum()
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
