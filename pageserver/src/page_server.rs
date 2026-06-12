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

use std::collections::{HashMap, HashSet};
use std::fmt::Debug;
use std::fs;
use std::sync::{Arc, Mutex, Weak};
use std::time::{Duration, Instant, SystemTime};

use anyhow::Context;
use arc_swap::ArcSwap;
use camino::{Utf8Path, Utf8PathBuf};
use once_cell::sync::Lazy;
pub use pageserver_api::models::TenantState;
use pageserver_api::models::RelSizeMigration;
use pageserver_api::models::ShardIdentity;
use pageserver_api::models::TimelineState;
use postgres_ffi::PgMajorVersion;
use remote_storage::{DownloadError, GenericRemoteStorage, TimeoutOrCancel};
use tokio::io::BufReader;
use tokio::sync::Semaphore;
use tokio_util::sync::CancellationToken;
use tracing::*;
use utils::crashsafe::path_with_suffix_extension;
use utils::sync::gate::{Gate, GateGuard};
use utils::zstd::{create_zst_tarball, extract_zst_tarball};
use utils::{backoff, failpoint_support, fs_ext, pausable_failpoint};

use self::metadata::TimelineMetadata;
use self::timeline::uninit::{TimelineCreateGuard, TimelineExclusionError, UninitializedTimeline};
use self::timeline::{GcCutoffs, TimelineResources};
use crate::basebackup_cache::BasebackupCache;
use crate::config::PageServerConf;
use crate::context;
use crate::context::RequestContextBuilder;
use crate::context::{DownloadBehavior, RequestContext};
use crate::metrics::{
    CONCURRENT_INITDBS, INITDB_RUN_TIME, INITDB_SEMAPHORE_ACQUISITION_TIME,
    TENANT, remove_tenant_metrics,
};
use crate::task_mgr::TaskKind;
use crate::page_server::gc_result::GcResult;
use initdb_storage::{INITDB_PATH, remote_initdb_archive_path};

use crate::page_server::timeline::uninit::cleanup_timeline_directory;
use crate::walingest::WalLagCooldown;
use crate::walredo::{PostgresRedoManager, RedoAttemptType};
use crate::{TEMP_FILE_SUFFIX, import_datadir, span, task_mgr, walredo};

static INIT_DB_SEMAPHORE: Lazy<Semaphore> = Lazy::new(|| Semaphore::new(8));
use utils::crashsafe;
use utils::generation::Generation;
use utils::id::{TenantId, TimelineId};
use utils::lsn::Lsn;

pub mod metadata;
pub mod storage_layer;

pub mod mgr;
pub mod tasks;

pub(crate) mod timeline;

mod gc_block;
mod gc_result;
pub(crate) mod throttle;

pub(crate) use timeline::{PageReconstructError, Timeline};

pub(crate) use crate::span::debug_assert_current_span_has_tenant_and_timeline_id;
// re-export for use in walreceiver
pub use crate::page_server::timeline::WalReceiverInfo;

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

/// The tenant's effective configuration: the `[tenant_config]` section
/// of pageserver.toml plus the launcher-assigned generation.
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
                + PageServer::get_lsn_lease_length_impl(conf, &tenant_conf),
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

/// The single-timeline pageserver: one process, one (tenant, timeline),
/// opened synchronously at startup before any listener accepts traffic.
pub struct PageServer {
    // Global pageserver config parameters
    pub conf: &'static PageServerConf,

    // Overridden tenant-specific config parameters.
    // We keep pageserver_api::models::TenantConfig sturct here to preserve the information
    // about parameters that are not set.
    // This is necessary to allow global config updates.
    tenant_conf: Arc<ArcSwap<AttachedTenantConf>>,

    id: TenantId,

    // The detailed sharding information, beyond the number/count in id
    shard_identity: ShardIdentity,

    /// The launcher-assigned generation, used to fence YB writes against
    /// stale pageservers. Does not change over the lifetime of the process.
    generation: Generation,

    /// THE timeline this process serves. Set exactly once during startup
    /// (open-existing or create-if-missing), before listeners start.
    timeline: std::sync::OnceLock<Arc<Timeline>>,

    /// Guards the bootstrap-time timeline creation (crash-retry runs it
    /// again on the same id). Held as a set for the `TimelineCreateGuard`
    /// release-on-drop protocol.
    timelines_creating: std::sync::Mutex<HashSet<TimelineId>>,

    // This mutex prevents creation of new timelines during GC.
    gc_cs: tokio::sync::Mutex<()>,
    walredo_mgr: Option<Arc<WalRedoManager>>,

    /// Provides access to timeline data sitting in the remote storage.
    pub(crate) remote_storage: GenericRemoteStorage,

    /// A channel to send async requests to prepare a basebackup for the basebackup cache.
    basebackup_cache: Arc<BasebackupCache>,

    /// Time it took for the tenant to activate. Zero if not active yet.
    attach_wal_lag_cooldown: Arc<std::sync::OnceLock<WalLagCooldown>>,

    // Cancellation token fires when we have entered shutdown().  This is a parent of
    // Timelines' cancellation token.
    pub(crate) cancel: CancellationToken,

    // Users of the PageServer such as the page service must take this Gate to avoid
    // trying to use a PageServer which is shutting down.
    pub(crate) gate: Gate,

    /// Throttle applied at the top of [`Timeline::get`].
    /// All [`PageServer::timelines`] of a given [`PageServer`] instance share the same [`throttle::Throttle`] instance.
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
impl std::fmt::Debug for PageServer {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "{}", self.id)
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

/// What is used to determine idempotency of a [`PageServer::create_timeline`] call in  [`PageServer::start_creating_timeline`] in  [`PageServer::start_creating_timeline`].
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

/// What is returned by [`PageServer::start_creating_timeline`].
#[must_use]
enum StartCreatingTimelineResult {
    CreateGuard(TimelineCreateGuard),
    Idempotent(Arc<Timeline>),
}

enum TimelineInitAndSyncResult {
    ReadyToActivate,
}

/// What is returned by [`PageServer::create_timeline`].
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

impl PageServer {
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

        // We should never try and load the same timeline twice during startup
        self.timeline
            .set(Arc::clone(&timeline))
            .unwrap_or_else(|_| {
                unreachable!("Timeline {tenant_id}/{timeline_id} already exists in the tenant slot")
            });
        timeline.maybe_spawn_flush_loop();

        // yb-pageserver: page data lives in pageserver.pages /
        // slru_pages / meta, never in layer files. A timeline with
        // no ancestor and no layer files is the normal post-restart
        // state.

        Ok(TimelineInitAndSyncResult::ReadyToActivate)
    }

    /// Open the configured (tenant, timeline) synchronously: preload the
    /// timeline_state row, attach (open-existing or create-if-missing per
    /// `[timeline_init]`), and activate. Called from startup BEFORE any
    /// listener accepts traffic; failure is fatal to the process.
    pub(crate) async fn open(
        conf: &'static PageServerConf,
        tenant_shard_id: TenantId,
        resources: TenantSharedResources,
        attached_conf: AttachedTenantConf,
        shard_identity: ShardIdentity,
        ctx: &RequestContext,
    ) -> anyhow::Result<Arc<PageServer>> {
        let wal_redo_manager =
            WalRedoManager::new(PostgresRedoManager::new(conf, tenant_shard_id))
                .map_err(|GlobalShutDown| anyhow::anyhow!("pageserver is shutting down"))?;

        let TenantSharedResources {
            remote_storage,
            basebackup_cache,
            compute_page_store,
            fpu_page_store,
        } = resources;

        let generation = attached_conf.generation;

        let tenant = Arc::new(PageServer::new(
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

        let ctx = ctx.detached_child(TaskKind::Attach, DownloadBehavior::Warn);
        let tenant_clone = Arc::clone(&tenant);
        async move {
            info!("Opening tenant");

            let preload = {
                let _preload_timer = TENANT.preload.start_timer();
                tenant_clone
                    .preload(&remote_storage, tenant_clone.cancel.clone())
                    .await?
            };

            let attach_start = std::time::Instant::now();
            {
                let _attach_timer = Some(TENANT.attach.start_timer());
                tenant_clone.attach(Some(preload), &ctx).await?;
            }
            let attach_duration = attach_start.elapsed();
            _ = tenant_clone
                .attach_wal_lag_cooldown
                .set(WalLagCooldown::new(attach_start, attach_duration));

            info!("attach finished, activating");
            tenant_clone.activate(&ctx);
            anyhow::Ok(())
        }
        .instrument(
            tracing::info_span!(parent: None, "attach", tenant_id=%tenant_shard_id, gen=?generation),
        )
        .await?;
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
        self: &Arc<PageServer>,
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
                    // activation happens in PageServer::activate.
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
                    // The launcher picks the branch LSN from the
                    // ancestor's last_record_lsn, which can run ahead
                    // of the ancestor's DURABLE consistent_lsn by up
                    // to one flush interval (5 s default). Retry the
                    // durability validation until it catches up;
                    // other validation failures are fatal.
                    let deadline =
                        tokio::time::Instant::now() + std::time::Duration::from_secs(60);
                    let row = loop {
                        match self
                            .fpu_page_store
                            .create_branch_row(
                                self.id,
                                identity.timeline_id,
                                *ancestor_timeline_id,
                                *ancestor_lsn,
                                generation_i32,
                            )
                            .await
                        {
                            Ok(row) => break row,
                            Err(e)
                                if format!("{e:#}").contains("not yet durable")
                                    && tokio::time::Instant::now() < deadline =>
                            {
                                info!("branch point not yet durable on ancestor, retrying: {e:#}");
                                tokio::time::sleep(std::time::Duration::from_millis(500)).await;
                            }
                            Err(e) => return Err(e.context("create branch timeline row")),
                        }
                    };
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

        // Single-timeline pageserver: at most one timeline loads here,
        // and a branch's ancestor is served by a DIFFERENT process —
        // no in-process ancestor ordering to respect.
        for (timeline_id, remote_metadata) in timeline_ancestors {
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

        // Before activation, seed the timeline's GcInfo. Branch children
        // live in other processes, so there are no in-process branchpoints
        // to track; cross-process ancestor pins are GC future work (task 7).
        self.initialize_gc_info();

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
        // a DIFFERENT pageserver process and is never hosted here; reads
        // below the branch point go through lineage-aware PageStore reads.
        self.timeline_init_and_sync(timeline_id, resources, remote_metadata, None, cause, ctx)
            .await
    }

    #[allow(dead_code)]
    pub(crate) fn tenant_shard_id(&self) -> TenantId {
        self.id
    }

    /// The timeline this process serves, if startup has set it.
    pub(crate) fn timeline(&self) -> Option<Arc<Timeline>> {
        self.timeline.get().cloned()
    }

    /// Get Timeline handle for given Neon timeline ID.
    /// This function is idempotent. It doesn't change internal state in any way.
    pub fn get_timeline(
        &self,
        timeline_id: TimelineId,
        active_only: bool,
    ) -> Result<Arc<Timeline>, GetTimelineError> {
        let timeline = self
            .timeline
            .get()
            .filter(|tl| tl.timeline_id == timeline_id)
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
        self.timeline().into_iter().collect()
    }

    pub fn list_timeline_ids(&self) -> Vec<TimelineId> {
        self.timeline
            .get()
            .map(|tl| tl.timeline_id)
            .into_iter()
            .collect()
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
        if self.cancel.is_cancelled() {
            return Ok(GcResult::default());
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
        if let Some(timeline) = self.timeline().filter(|tl| tl.is_active()) {
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

    pub fn generation(&self) -> Generation {
        self.generation
    }

    /// Activate the opened timeline: launch its walreceiver and the
    /// tenant background loops. Runs synchronously at the end of startup,
    /// before listeners accept traffic.
    fn activate(self: &Arc<Self>, ctx: &RequestContext) {
        span::debug_assert_current_span_has_tenant_id();

        // Spawn gc and housekeeping loops. The loops will shut themselves
        // down when they notice that the tenant is shutting down.
        tasks::start_background_loops(self);

        let timeline = self
            .timeline()
            .expect("activate runs after attach has set the timeline");
        timeline.activate(self.clone(), &ctx.with_scope_timeline(&timeline));
        info!(tenant_id = %self.id, "tenant active");
    }

    /// Shutdown the tenant and join all of the spawned tasks.
    ///
    /// Setting the timeline to Stopping locks out new requests, signals
    /// cancellation to the WAL receiver (joined below, so no new data
    /// arrives before the final flush) and the background loops.
    async fn shutdown(&self, shutdown_mode: timeline::ShutdownMode) {
        span::debug_assert_current_span_has_tenant_id();

        if let Some(timeline) = self.timeline().filter(|tl| !tl.is_broken()) {
            timeline.set_state(TimelineState::Stopping);

            let timeline_id = timeline.timeline_id;
            let span = tracing::info_span!("timeline_shutdown", %timeline_id, ?shutdown_mode);
            timeline.shutdown(shutdown_mode).instrument(span).await;
        }

        // We cancel the Tenant's cancellation token _after_ the timeline has shut down.  This permits
        // it to continue to do work during its shutdown method, e.g. flushing data.
        tracing::debug!("Cancelling CancellationToken");
        self.cancel.cancel();

        // shutdown all tenant and timeline tasks: gc, housekeeping, page service
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
    }

    #[allow(dead_code)]
    pub(crate) fn get_shard_identity(&self) -> ShardIdentity {
        self.shard_identity
    }

}

impl PageServer {
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

    fn get_pagestream_throttle_config(
        psconf: &'static PageServerConf,
        overrides: &pageserver_api::models::TenantConfig,
    ) -> throttle::Config {
        overrides
            .timeline_get_throttle
            .clone()
            .unwrap_or(psconf.default_tenant_conf.timeline_get_throttle.clone())
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

    /// [`PageServer::shutdown`] must be called before dropping the returned [`PageServer`] object
    /// to ensure proper cleanup of background tasks and metrics.
    //
    // Allow too_many_arguments because a constructor's argument list naturally grows with the
    // number of attributes in the struct: breaking these out into a builder wouldn't be helpful.
    #[allow(clippy::too_many_arguments)]
    fn new(
        conf: &'static PageServerConf,
        attached_conf: AttachedTenantConf,
        shard_identity: ShardIdentity,
        walredo_mgr: Option<Arc<WalRedoManager>>,
        id: TenantId,
        remote_storage: GenericRemoteStorage,
        basebackup_cache: Arc<BasebackupCache>,
        compute_page_store: Arc<crate::page_store::PageStore>,
        fpu_page_store: Arc<crate::page_store::PageStore>,
    ) -> PageServer {
        assert!(!attached_conf.generation.is_none());

        PageServer {
            id,
            shard_identity,
            generation: attached_conf.generation,
            conf,
            timeline: std::sync::OnceLock::new(),
            timelines_creating: Mutex::new(HashSet::new()),
            gc_cs: tokio::sync::Mutex::new(()),
            walredo_mgr,
            remote_storage,
            attach_wal_lag_cooldown: Arc::new(std::sync::OnceLock::new()),
            cancel: CancellationToken::default(),
            gate: Gate::default(),
            pagestream_throttle: Arc::new(throttle::Throttle::new(
                PageServer::get_pagestream_throttle_config(conf, &attached_conf.tenant_conf),
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
        // Note that we don't hold the `PageServer::gc_cs` lock here because we don't want to delay the
        // branch creation task, which requires the GC lock. A GC iteration can run concurrently
        // with branch creation.
        //
        // See comments in [`PageServer::branch_timeline`] for more information about why branch
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

    /// Seeds the timeline's `GcInfo`. Branch children live in other
    /// processes, so there are no in-process branchpoints; cross-process
    /// ancestor pins are GC future work (task 7). The PITR cutoff is set
    /// later, before GC, by [`Self::refresh_gc_info_internal`].
    fn initialize_gc_info(&self) {
        let horizon = self.get_gc_horizon();

        if let Some(timeline) = self.timeline() {
            let mut target = timeline.gc_info.write().unwrap();

            target.retain_lsns = Vec::new();

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
            .timeline()
            .filter(|tl| match target_timeline_id.as_ref() {
                Some(target) => &tl.timeline_id == target,
                None => true,
            })
            .into_iter()
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

        if self.cancel.is_cancelled() {
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
        async fn flush_timeline(_gate: GateGuard, timeline: Arc<Timeline>) -> anyhow::Result<()> {
            tracing::info!(timeline_id=%timeline.timeline_id, "Flushing...");
            timeline.freeze_and_flush().await?;
            // yb-mode: no asynchronous uploads to wait on; flush is
            // synchronous against YB.
            Ok(())
        }

        let Some(timeline) = self.timeline() else {
            return Ok(());
        };
        // Run the flush in a task holding the timeline's gate: this
        // means that if this function's future is cancelled, the Timeline shutdown
        // will still wait for any I/O in here to complete.
        let Ok(gate) = timeline.gate.enter() else {
            return Ok(());
        };
        let jh = tokio::task::spawn(async move { flush_timeline(gate, timeline).await });
        if let Err(e) = jh.await {
            if !e.is_cancelled() && !e.is_panic() {
                tracing::error!("unexpected join error: {e:?}");
            }
        }

        Ok(())
    }

    /// How much local disk space the timeline wants (initdb tarballs and
    /// basebackup cache only in yb-mode — page data lives in YB).
    pub(crate) fn local_storage_wanted(&self) -> u64 {
        self.timeline()
            .map(|t| t.metrics.visible_physical_size_gauge.get())
            .unwrap_or(0)
    }

    /// Return the visible size of the timeline.
    pub(crate) fn get_visible_size(&self) -> u64 {
        self.local_storage_wanted()
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
