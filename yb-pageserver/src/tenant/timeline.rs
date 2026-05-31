pub mod delete;
pub(crate) mod handle;
pub(crate) mod import_pgdata;
pub mod layer_manager;
pub(crate) mod logical_size;
pub mod uninit;
mod walreceiver;

use hashlink::LruCache;
use std::array;
use std::collections::btree_map::Entry;
use std::collections::{BTreeMap, HashMap};
use std::ops::{ControlFlow, Deref, Range};
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering as AtomicOrdering};
use std::sync::{Arc, Mutex, OnceLock, RwLock, Weak};
use std::time::{Duration, Instant, SystemTime};

use anyhow::{Context, Result, anyhow, bail, ensure};
use arc_swap::{ArcSwap, ArcSwapOption};
use bytes::Bytes;
use chrono::{DateTime, Utc};
use handle::ShardTimelineId;
use layer_manager::{
    LayerManagerLockHolder, LockedLayerManager,
};

use once_cell::sync::Lazy;
use pageserver_api::config::tenant_conf_defaults::DEFAULT_PITR_INTERVAL;
use pageserver_api::key::{
    Key, METADATA_KEY_BEGIN_PREFIX, METADATA_KEY_END_PREFIX,
};
use pageserver_api::models::{
    DownloadRemoteLayersTaskInfo, DownloadRemoteLayersTaskSpawnRequest, LayerMapInfo, LsnLease, PageTraceEvent, RelSizeMigration,
    TimelineState,
};
use pageserver_api::reltag::{BlockNumber, RelTag};
use pageserver_api::shard::{ShardIdentity, ShardIndex, ShardNumber, TenantShardId};
use postgres_connection::PgConnectionConfig;
use postgres_ffi::v14::xlog_utils;
use postgres_ffi::{PgMajorVersion, WAL_SEGMENT_SIZE, to_pg_timestamp};
use rand::Rng;
use remote_storage::DownloadError;
use storage_broker::BrokerClientChannel;
use tokio::sync::mpsc::Sender;
use tokio::sync::watch;
use tokio_util::sync::CancellationToken;
use tracing::*;
use utils::generation::Generation;
use utils::id::TimelineId;
use utils::logging::{MonitorSlowFutureCallback, monitor_slow_future};
use utils::lsn::{AtomicLsn, Lsn, RecordLsn};
use utils::postgres_client::PostgresClientProtocol;
use utils::seqwait::SeqWait;
use utils::simple_rcu::{Rcu, RcuReadGuard};
use utils::sync::gate::{Gate, GateGuard};
use utils::{completion, critical_timeline, pausable_failpoint};
use wal_decoder::serialized_batch::{SerializedValueBatch, ValueMeta};

use self::delete::DeleteTimelineFlow;
use self::logical_size::LogicalSize;
use self::walreceiver::{WalReceiver, WalReceiverConf};
use super::remote_timeline_client::RemoteTimelineClient;
use super::remote_timeline_client::index::GcCompactionState;
use super::{
    AttachedTenantConf, GcError, MaybeOffloaded,
    debug_assert_current_span_has_tenant_and_timeline_id,
};
use crate::aux_file::AuxFileSizeEstimator;
use crate::basebackup_cache::BasebackupCache;
use crate::config::PageServerConf;
use crate::context::{
    DownloadBehavior, RequestContext,
};
use crate::keyspace::KeySpace;
use crate::metrics::{
    DELTAS_PER_READ_GLOBAL, ScanLatencyOngoingRecording, TimelineMetrics,
};
use crate::page_service::TenantManagerTypes;
use crate::pgdatadir_mapping::{
    CalculateLogicalSizeError, DirectoryKind, LsnForTimestamp, MetricsUpdate,
};
use crate::task_mgr::TaskKind;
use crate::tenant::gc_result::GcResult;
use crate::tenant::metadata::TimelineMetadata;
use crate::tenant::storage_layer::{
    IoConcurrency, Layer, LayerAccessStatsReset, LayerName,
    ValueReconstructState, ValuesReconstructState,
};
use crate::tenant::tasks::BackgroundLoopKind;
use crate::tenant::timeline::logical_size::CurrentLogicalSize;
use crate::walingest::WalLagCooldown;
use crate::walredo::RedoAttemptType;
use crate::{task_mgr, walredo};


/// The outward-facing resources required to build a Timeline
pub struct TimelineResources {
    pub remote_client: RemoteTimelineClient,
    pub pagestream_throttle: Arc<crate::tenant::throttle::Throttle>,
    pub pagestream_throttle_metrics: Arc<crate::metrics::tenant_throttling::Pagestream>,
    pub basebackup_cache: Arc<BasebackupCache>,
    /// Compute-reads `PageStore` (see
    /// `TenantSharedResources::compute_page_store`). `Some` enables
    /// yb-pageserver mode for this timeline.
    pub compute_page_store: Option<Arc<crate::page_store::PageStore>>,
    /// Internal-traffic `PageStore` (see
    /// `TenantSharedResources::internal_page_store`).
    pub internal_page_store: Option<Arc<crate::page_store::PageStore>>,
}

pub struct Timeline {
    pub(crate) conf: &'static PageServerConf,
    tenant_conf: Arc<ArcSwap<AttachedTenantConf>>,

    myself: Weak<Self>,

    pub(crate) tenant_shard_id: TenantShardId,
    pub timeline_id: TimelineId,

    /// The generation of the tenant that instantiated us: this is used for safety when writing remote objects.
    /// Never changes for the lifetime of this [`Timeline`] object.
    ///
    /// This duplicates the generation stored in LocationConf, but that structure is mutable:
    /// this copy enforces the invariant that generatio doesn't change during a Tenant's lifetime.
    pub(crate) generation: Generation,

    /// The detailed sharding information from our parent Tenant.  This enables us to map keys
    /// to shards, and is constant through the lifetime of this Timeline.
    shard_identity: ShardIdentity,

    pub pg_version: PgMajorVersion,

    /// The tuple has two elements.
    /// 1. `LayerFileManager` keeps track of the various physical representations of the layer files (inmem, local, remote).
    /// 2. `LayerMap`, the acceleration data structure for `get_reconstruct_data`.
    ///
    /// `LayerMap` maps out the `(PAGE,LSN) / (KEY,LSN)` space, which is composed of `(KeyRange, LsnRange)` rectangles.
    /// We describe these rectangles through the `PersistentLayerDesc` struct.
    ///
    /// When we want to reconstruct a page, we first find the `PersistentLayerDesc`'s that we need for page reconstruction,
    /// using `LayerMap`. Then, we use `LayerFileManager` to get the `PersistentLayer`'s that correspond to the
    /// `PersistentLayerDesc`'s.
    ///
    /// Hence, it's important to keep things coherent. The `LayerFileManager` must always have an entry for all
    /// `PersistentLayerDesc`'s in the `LayerMap`. If it doesn't, `LayerFileManager::get_from_desc` will panic at
    /// runtime, e.g., during page reconstruction.
    ///
    /// In the future, we'll be able to split up the tuple of LayerMap and `LayerFileManager`,
    /// so that e.g. on-demand-download/eviction, and layer spreading, can operate just on `LayerFileManager`.
    pub(crate) layers: LockedLayerManager,

    pub(crate) standby_horizon: AtomicLsn,

    // WAL redo manager. `None` only for broken tenants.
    walredo_mgr: Option<Arc<super::WalRedoManager>>,

    /// Remote storage client.
    /// See [`remote_timeline_client`](super::remote_timeline_client) module comment for details.
    pub(crate) remote_client: Arc<RemoteTimelineClient>,

    /// yb-pageserver row-per-page writer. `Some` enables dual-write at
    /// `TimelineWriter::put_batch`: every `(Key, LSN, Value)` digested
    /// by walingest is also forwarded to `PageStore` via the 10-delta
    /// rule. `None` is upstream layer-file-only behaviour.
    pub(crate) page_writer: Option<Arc<crate::page_writer::PageWriter>>,

    /// Handle for the page_writer periodic flusher. Taken and
    /// `shutdown()`-ed during `Timeline::shutdown` so buffered rows are
    /// drained to PageStore before the pageserver exits.
    page_writer_flush_task: std::sync::Mutex<Option<crate::page_writer::FlushTask>>,

    /// Handle for the background image materialiser task, when the
    /// timeline has both a `page_writer` and a walredo manager. Taken
    /// and `shutdown()`-ed during `Timeline::shutdown`. `None` when
    /// materialisation is disabled or the timeline is running in
    /// upstream-only mode.
    image_materializer_task:
        std::sync::Mutex<Option<crate::image_materializer::MaterializerTask>>,

    // What page versions do we hold in the repository? If we get a
    // request > last_record_lsn, we need to wait until we receive all
    // the WAL up to the request. The SeqWait provides functions for
    // that. TODO: If we get a request for an old LSN, such that the
    // versions have already been garbage collected away, we should
    // throw an error, but we don't track that currently.
    //
    // last_record_lsn.load().last points to the end of last processed WAL record.
    //
    // We also remember the starting point of the previous record in
    // 'last_record_lsn.load().prev'. It's used to set the xl_prev pointer of the
    // first WAL record when the node is started up. But here, we just
    // keep track of it.
    last_record_lsn: SeqWait<RecordLsn, Lsn>,

    // All WAL records have been processed and stored durably on files on
    // local disk, up to this LSN. On crash and restart, we need to re-process
    // the WAL starting from this point.
    //
    // Some later WAL records might have been processed and also flushed to disk
    // already, so don't be surprised to see some, but there's no guarantee on
    // them yet.
    disk_consistent_lsn: AtomicLsn,

    // Parent timeline that this timeline was branched from, and the LSN
    // of the branch point.
    ancestor_timeline: Option<Arc<Timeline>>,
    ancestor_lsn: Lsn,

    // The LSN of gc-compaction that was last applied to this timeline.
    gc_compaction_state: ArcSwapOption<GcCompactionState>,

    pub(crate) metrics: Arc<TimelineMetrics>,

    // `Timeline` doesn't write these metrics itself, but it manages the lifetime.  Code
    // in `crate::page_service` writes these metrics.
    pub(crate) query_metrics: crate::metrics::SmgrQueryTimePerTimeline,

    directory_metrics_inited: [AtomicBool; DirectoryKind::KINDS_NUM],
    directory_metrics: [AtomicU64; DirectoryKind::KINDS_NUM],

    /// Ensures layers aren't frozen by checkpointer between
    /// [`Timeline::get_layer_for_write`] and layer reads.
    /// Locked automatically by [`TimelineWriter`] and checkpointer.
    /// Must always be acquired before the layer map/individual layer lock
    /// to avoid deadlock.
    ///
    /// The state is cleared upon freezing.
    write_lock: tokio::sync::Mutex<Option<TimelineWriterState>>,

    // The LSN at which we have executed GC: whereas [`Self::gc_info`] records the LSN at which
    // we _intend_ to GC (i.e. the PITR cutoff), this LSN records where we actually last did it.
    // Because PITR interval is mutable, it's possible for this LSN to be earlier or later than
    // the planned GC cutoff.
    pub applied_gc_cutoff_lsn: Rcu<Lsn>,

    // List of child timelines and their branch points. This is needed to avoid
    // garbage collecting data that is still needed by the child timelines.
    pub(crate) gc_info: std::sync::RwLock<GcInfo>,

    // It may change across major versions so for simplicity
    // keep it after running initdb for a timeline.
    // It is needed in checks when we want to error on some operations
    // when they are requested for pre-initdb lsn.
    // It can be unified with latest_gc_cutoff_lsn under some "first_valid_lsn",
    // though let's keep them both for better error visibility.
    pub initdb_lsn: Lsn,

    /// Current logical size of the "datadir", at the last LSN.
    current_logical_size: LogicalSize,

    /// Information about the last processed message by the WAL receiver,
    /// or None if WAL receiver has not received anything for this timeline
    /// yet.
    pub last_received_wal: Mutex<Option<WalReceiverInfo>>,
    pub walreceiver: Mutex<Option<WalReceiver>>,

    /// Relation size cache
    pub(crate) rel_size_latest_cache: RwLock<HashMap<RelTag, (Lsn, BlockNumber)>>,
    pub(crate) rel_size_snapshot_cache: Mutex<LruCache<(Lsn, RelTag), BlockNumber>>,

    download_all_remote_layers_task_info: RwLock<Option<DownloadRemoteLayersTaskInfo>>,

    state: watch::Sender<TimelineState>,

    /// Prevent two tasks from deleting the timeline at the same time. If held, the
    /// timeline is being deleted. If 'true', the timeline has already been deleted.
    pub delete_progress: TimelineDeleteProgress,

    /// Gate to prevent shutdown completing while I/O is still happening to this timeline's data
    pub(crate) gate: Gate,

    /// Cancellation token scoped to this timeline: anything doing long-running work relating
    /// to the timeline should drop out when this token fires.
    pub(crate) cancel: CancellationToken,

    /// Make sure we only have one running compaction at a time in tests.
    ///
    /// Must only be taken in two places:
    /// - [`Timeline::compact`] (this file)
    /// - [`delete::delete_local_timeline_directory`]
    ///
    /// Timeline deletion will acquire both compaction and gc locks in whatever order.
    compaction_lock: tokio::sync::Mutex<()>,

    /// Begin Hadron: If true, the pageserver has likely detected data corruption in the timeline.
    /// We need to feed this information back to the Safekeeper and postgres for them to take the
    /// appropriate action.
    corruption_detected: AtomicBool,

    /// Make sure we only have one running gc at a time.
    ///
    /// Must only be taken in two places:
    /// - [`Timeline::gc`] (this file)
    /// - [`delete::delete_local_timeline_directory`]
    ///
    /// Timeline deletion will acquire both compaction and gc locks in whatever order.
    gc_lock: tokio::sync::Mutex<()>,

    /// Cloned from [`super::TenantShard::pagestream_throttle`] on construction.
    pub(crate) pagestream_throttle: Arc<crate::tenant::throttle::Throttle>,

    /// Size estimator for aux file v2
    pub(crate) aux_file_size_estimator: AuxFileSizeEstimator,

    pub(crate) handles: handle::PerTimelineState<TenantManagerTypes>,

    pub(crate) attach_wal_lag_cooldown: Arc<OnceLock<WalLagCooldown>>,

    /// Cf. [`crate::tenant::CreateTimelineIdempotency`].
    pub(crate) create_idempotency: crate::tenant::CreateTimelineIdempotency,

    /// If Some, collects GetPage metadata for an ongoing PageTrace.
    pub(crate) page_trace: ArcSwapOption<Sender<PageTraceEvent>>,

    pub(crate) rel_size_v2_status: ArcSwap<(Option<RelSizeMigration>, Option<Lsn>)>,

    wait_lsn_log_slow: tokio::sync::Semaphore,

    /// A channel to send async requests to prepare a basebackup for the basebackup cache.
    basebackup_cache: Arc<BasebackupCache>,

    /// Basebackup will collect the count and store it here. Used for reldirv2 rollout.
    pub(crate) db_rel_count: ArcSwapOption<(usize, usize)>,
}

pub type TimelineDeleteProgress = Arc<tokio::sync::Mutex<DeleteTimelineFlow>>;

pub struct WalReceiverInfo {
    pub wal_source_connconf: PgConnectionConfig,
    pub last_received_msg_lsn: Lsn,
    pub last_received_msg_ts: u128,
}

/// Information about how much history needs to be retained, needed by
/// Garbage Collection.
#[derive(Default)]
pub(crate) struct GcInfo {
    /// Specific LSNs that are needed.
    ///
    /// Currently, this includes all points where child branches have
    /// been forked off from. In the future, could also include
    /// explicit user-defined snapshot points.
    pub(crate) retain_lsns: Vec<(Lsn, TimelineId, MaybeOffloaded)>,

    /// The cutoff coordinates, which are combined by selecting the minimum.
    pub(crate) cutoffs: GcCutoffs,

    /// Leases granted to particular LSNs.
    pub(crate) leases: BTreeMap<Lsn, LsnLease>,

    /// Whether our branch point is within our ancestor's PITR interval (for cost estimation)
    pub(crate) within_ancestor_pitr: bool,
}

impl GcInfo {
    pub(crate) fn min_cutoff(&self) -> Lsn {
        self.cutoffs.select_min()
    }

    pub(super) fn insert_child(
        &mut self,
        child_id: TimelineId,
        child_lsn: Lsn,
        is_offloaded: MaybeOffloaded,
    ) {
        self.retain_lsns.push((child_lsn, child_id, is_offloaded));
        self.retain_lsns.sort_by_key(|i| i.0);
    }

    pub(super) fn remove_child_maybe_offloaded(
        &mut self,
        child_id: TimelineId,
        maybe_offloaded: MaybeOffloaded,
    ) -> bool {
        // Remove at most one element. Needed for correctness if there is two live `Timeline` objects referencing
        // the same timeline. Shouldn't but maybe can occur when Arc's live longer than intended.
        let mut removed = false;
        self.retain_lsns.retain(|i| {
            if removed {
                return true;
            }
            let remove = i.1 == child_id && i.2 == maybe_offloaded;
            removed |= remove;
            !remove
        });
        removed
    }

    pub(super) fn remove_child_not_offloaded(&mut self, child_id: TimelineId) -> bool {
        self.remove_child_maybe_offloaded(child_id, MaybeOffloaded::No)
    }

    pub(super) fn remove_child_offloaded(&mut self, child_id: TimelineId) -> bool {
        self.remove_child_maybe_offloaded(child_id, MaybeOffloaded::Yes)
    }
    pub(crate) fn lsn_covered_by_lease(&self, lsn: Lsn) -> bool {
        self.leases.contains_key(&lsn)
    }
}

/// The `GcInfo` component describing which Lsns need to be retained.  Functionally, this
/// is a single number (the oldest LSN which we must retain), but it internally distinguishes
/// between time-based and space-based retention for observability and consumption metrics purposes.
#[derive(Clone, Debug, Default)]
pub(crate) struct GcCutoffs {
    /// Calculated from the [`pageserver_api::models::TenantConfig::gc_horizon`], this LSN indicates how much
    /// history we must keep to retain a specified number of bytes of WAL.
    pub(crate) space: Lsn,

    /// Calculated from [`pageserver_api::models::TenantConfig::pitr_interval`], this LSN indicates
    /// how much history we must keep to enable reading back at least the PITR interval duration.
    ///
    /// None indicates that the PITR cutoff has not been computed. A PITR interval of 0 will yield
    /// Some(last_record_lsn).
    pub(crate) time: Option<Lsn>,
}

impl GcCutoffs {
    fn select_min(&self) -> Lsn {
        // NB: if we haven't computed the PITR cutoff yet, we can't GC anything.
        self.space.min(self.time.unwrap_or_default())
    }
}

/// An error happened in a get() operation.
#[derive(thiserror::Error, Debug)]
pub(crate) enum PageReconstructError {
    #[error(transparent)]
    Other(anyhow::Error),

    #[error("Ancestor LSN wait error: {0}")]
    AncestorLsnTimeout(WaitLsnError),

    #[error("timeline shutting down")]
    Cancelled,

    /// An error happened replaying WAL records
    #[error(transparent)]
    WalRedo(anyhow::Error),

    #[error("{0}")]
    MissingKey(Box<MissingKeyError>),
}

impl PageReconstructError {
    #[allow(dead_code)] // we use the is_cancel + into_anyhow pattern in quite a few places, this one will follow soon enough
    pub(crate) fn into_anyhow(self) -> anyhow::Error {
        match self {
            PageReconstructError::Other(e) => e,
            PageReconstructError::AncestorLsnTimeout(e) => e.into_anyhow(),
            PageReconstructError::Cancelled => anyhow::Error::new(self),
            PageReconstructError::WalRedo(e) => e,
            PageReconstructError::MissingKey(_) => anyhow::Error::new(self),
        }
    }
}

impl From<anyhow::Error> for PageReconstructError {
    fn from(value: anyhow::Error) -> Self {
        // with walingest.rs many PageReconstructError are wrapped in as anyhow::Error
        match value.downcast::<PageReconstructError>() {
            Ok(pre) => pre,
            Err(other) => PageReconstructError::Other(other),
        }
    }
}

impl From<utils::bin_ser::DeserializeError> for PageReconstructError {
    fn from(value: utils::bin_ser::DeserializeError) -> Self {
        PageReconstructError::Other(anyhow::Error::new(value).context("deserialization failure"))
    }
}

impl From<layer_manager::Shutdown> for PageReconstructError {
    fn from(_: layer_manager::Shutdown) -> Self {
        PageReconstructError::Cancelled
    }
}

impl GetVectoredError {
    #[cfg(test)]
    pub(crate) fn is_missing_key_error(&self) -> bool {
        matches!(self, Self::MissingKey(_))
    }
}

impl From<layer_manager::Shutdown> for GetVectoredError {
    fn from(_: layer_manager::Shutdown) -> Self {
        GetVectoredError::Cancelled
    }
}

#[derive(thiserror::Error)]
pub struct MissingKeyError {
    keyspace: KeySpace,
    shard: ShardNumber,
    query: Option<VersionedKeySpaceQuery>,
    // This is largest request LSN from the get page request batch
    original_hwm_lsn: Lsn,
    ancestor_lsn: Option<Lsn>,
    backtrace: Option<std::backtrace::Backtrace>,
}

impl std::fmt::Debug for MissingKeyError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "{self}")
    }
}

impl std::fmt::Display for MissingKeyError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(
            f,
            "could not find data for key {} (shard {:?}), original HWM LSN {}",
            self.keyspace, self.shard, self.original_hwm_lsn
        )?;

        if let Some(ref ancestor_lsn) = self.ancestor_lsn {
            write!(f, ", ancestor {ancestor_lsn}")?;
        }

        if let Some(ref query) = self.query {
            write!(f, ", query {query}")?;
        }

        if let Some(ref backtrace) = self.backtrace {
            write!(f, "\n{backtrace}")?;
        }

        Ok(())
    }
}

#[derive(thiserror::Error, Debug)]
pub(crate) enum CreateImageLayersError {
    #[error("timeline shutting down")]
    Cancelled,

    #[error("read failed")]
    GetVectoredError(#[source] GetVectoredError),

    #[error("reconstruction failed")]
    PageReconstructError(#[source] PageReconstructError),

    #[error(transparent)]
    Other(anyhow::Error),
}

impl From<layer_manager::Shutdown> for CreateImageLayersError {
    fn from(_: layer_manager::Shutdown) -> Self {
        CreateImageLayersError::Cancelled
    }
}

#[derive(thiserror::Error, Debug, Clone)]
pub(crate) enum FlushLayerError {
    /// Timeline cancellation token was cancelled
    #[error("timeline shutting down")]
    Cancelled,

    // Arc<> the following non-clonable error types: we must be Clone-able because the flush error is propagated from the flush
    // loop via a watch channel, where we can only borrow it.
    #[error("create image layers (shared)")]
    CreateImageLayersError(Arc<CreateImageLayersError>),

    #[error("other (shared)")]
    Other(#[from] Arc<anyhow::Error>),
}

impl From<layer_manager::Shutdown> for FlushLayerError {
    fn from(_: layer_manager::Shutdown) -> Self {
        FlushLayerError::Cancelled
    }
}

#[derive(thiserror::Error, Debug)]
pub enum GetVectoredError {
    #[error("timeline shutting down")]
    Cancelled,

    #[error("requested too many keys: {0} > {1}")]
    Oversized(u64, u64),

    #[error("requested at invalid LSN: {0}")]
    InvalidLsn(Lsn),

    #[error("requested key not found: {0}")]
    MissingKey(Box<MissingKeyError>),

    #[error("ancestry walk")]
    GetReadyAncestorError(#[source] GetReadyAncestorError),

    #[error(transparent)]
    Other(#[from] anyhow::Error),
}

impl From<GetReadyAncestorError> for GetVectoredError {
    fn from(value: GetReadyAncestorError) -> Self {
        use GetReadyAncestorError::*;
        match value {
            Cancelled => GetVectoredError::Cancelled,
            AncestorLsnTimeout(_) | BadState { .. } => {
                GetVectoredError::GetReadyAncestorError(value)
            }
        }
    }
}

#[derive(thiserror::Error, Debug)]
pub enum GetReadyAncestorError {
    #[error("ancestor LSN wait error")]
    AncestorLsnTimeout(#[from] WaitLsnError),

    #[error("bad state on timeline {timeline_id}: {state:?}")]
    BadState {
        timeline_id: TimelineId,
        state: TimelineState,
    },

    #[error("cancelled")]
    Cancelled,
}

#[derive(Clone, Copy)]
pub enum LogicalSizeCalculationCause {
    Initial,
}

pub enum GetLogicalSizePriority {
    User,
    Background,
}

#[derive(Debug, enumset::EnumSetType)]
pub(crate) enum CompactFlags {
    ForceRepartition,
    ForceImageLayerCreation,
    ForceL0Compaction,
    OnlyL0Compaction,
    EnhancedGcBottomMostCompaction,
    DryRun,
    /// Makes image compaction yield if there's pending L0 compaction. This should always be used in
    /// the background compaction task, since we want to aggressively compact down L0 to bound
    /// read amplification.
    ///
    /// It only makes sense to use this when `compaction_l0_first` is enabled (such that we yield to
    /// an L0 compaction pass), and without `OnlyL0Compaction` (L0 compaction shouldn't yield for L0
    /// compaction).
    YieldForL0,
}

#[derive(Debug, Clone, serde::Deserialize)]
pub(crate) struct MarkInvisibleRequest {
    #[serde(default)]
    pub is_visible: Option<bool>,
}

impl std::fmt::Debug for Timeline {
    fn fmt(&self, f: &mut std::fmt::Formatter) -> std::fmt::Result {
        write!(f, "Timeline<{}>", self.timeline_id)
    }
}

#[derive(thiserror::Error, Debug, Clone)]
pub enum WaitLsnError {
    // Called on a timeline which is shutting down
    #[error("Shutdown")]
    Shutdown,

    // Called on an timeline not in active state or shutting down
    #[error("Bad timeline state: {0:?}")]
    BadState(TimelineState),

    // Timeout expired while waiting for LSN to catch up with goal.
    #[error("{0}")]
    Timeout(String),
}

impl WaitLsnError {
    pub(crate) fn is_cancel(&self) -> bool {
        match self {
            WaitLsnError::Shutdown => true,
            WaitLsnError::BadState(timeline_state) => match timeline_state {
                TimelineState::Loading => false,
                TimelineState::Active => false,
                TimelineState::Stopping => true,
                TimelineState::Broken { .. } => false,
            },
            WaitLsnError::Timeout(_) => false,
        }
    }
    pub(crate) fn into_anyhow(self) -> anyhow::Error {
        match self {
            WaitLsnError::Shutdown => anyhow::Error::new(self),
            WaitLsnError::BadState(_) => anyhow::Error::new(self),
            WaitLsnError::Timeout(_) => anyhow::Error::new(self),
        }
    }
}

impl From<WaitLsnError> for tonic::Status {
    fn from(err: WaitLsnError) -> Self {
        use tonic::Code;
        let code = if err.is_cancel() {
            Code::Unavailable
        } else {
            Code::Internal
        };
        tonic::Status::new(code, err.to_string())
    }
}

// The impls below achieve cancellation mapping for errors.
// Perhaps there's a way of achieving this with less cruft.

impl From<CreateImageLayersError> for FlushLayerError {
    fn from(e: CreateImageLayersError) -> Self {
        match e {
            CreateImageLayersError::Cancelled => FlushLayerError::Cancelled,
            any => FlushLayerError::CreateImageLayersError(Arc::new(any)),
        }
    }
}

impl From<PageReconstructError> for CreateImageLayersError {
    fn from(e: PageReconstructError) -> Self {
        match e {
            PageReconstructError::Cancelled => CreateImageLayersError::Cancelled,
            _ => CreateImageLayersError::PageReconstructError(e),
        }
    }
}

impl From<GetVectoredError> for CreateImageLayersError {
    fn from(e: GetVectoredError) -> Self {
        match e {
            GetVectoredError::Cancelled => CreateImageLayersError::Cancelled,
            _ => CreateImageLayersError::GetVectoredError(e),
        }
    }
}

impl From<GetVectoredError> for PageReconstructError {
    fn from(e: GetVectoredError) -> Self {
        match e {
            GetVectoredError::Cancelled => PageReconstructError::Cancelled,
            GetVectoredError::InvalidLsn(_) => PageReconstructError::Other(anyhow!("Invalid LSN")),
            err @ GetVectoredError::Oversized(_, _) => PageReconstructError::Other(err.into()),
            GetVectoredError::MissingKey(err) => PageReconstructError::MissingKey(err),
            GetVectoredError::GetReadyAncestorError(err) => PageReconstructError::from(err),
            GetVectoredError::Other(err) => PageReconstructError::Other(err),
        }
    }
}

impl From<GetReadyAncestorError> for PageReconstructError {
    fn from(e: GetReadyAncestorError) -> Self {
        use GetReadyAncestorError::*;
        match e {
            AncestorLsnTimeout(wait_err) => PageReconstructError::AncestorLsnTimeout(wait_err),
            bad_state @ BadState { .. } => PageReconstructError::Other(anyhow::anyhow!(bad_state)),
            Cancelled => PageReconstructError::Cancelled,
        }
    }
}

pub(crate) enum WaitLsnTimeout {
    Custom(Duration),
    // Use the [`PageServerConf::wait_lsn_timeout`] default
    Default,
}

pub(crate) enum WaitLsnWaiter {
    Tenant,
    PageService,
    HttpEndpoint,
    BaseBackupCache,
}

/// Argument to [`Timeline::shutdown`].
#[derive(Debug, Clone, Copy)]
pub(crate) enum ShutdownMode {
    /// Graceful shutdown, may do a lot of I/O as we flush any open layers to disk. This method can
    /// take multiple seconds for a busy timeline.
    ///
    /// While we are flushing, we continue to accept read I/O for LSNs ingested before
    /// the call to [`Timeline::shutdown`].
    FreezeAndFlush,
    /// Only flush the layers to the remote storage without freezing any open layers. Flush the deletion
    /// queue. This is the mode used by ancestor detach and any other operations that reloads a tenant
    /// but not increasing the generation number. Note that this mode cannot be used at tenant shutdown,
    /// as flushing the deletion queue at that time will cause shutdown-in-progress errors.
    Reload,
    /// Shut down immediately, without waiting for any open layers to flush.
    Hard,
}

/// Public interface functions
impl Timeline {
    /// Get the LSN where this branch was created
    pub(crate) fn get_ancestor_lsn(&self) -> Lsn {
        self.ancestor_lsn
    }

    /// Get the ancestor's timeline id
    pub(crate) fn get_ancestor_timeline_id(&self) -> Option<TimelineId> {
        self.ancestor_timeline
            .as_ref()
            .map(|ancestor| ancestor.timeline_id)
    }

    /// Get the ancestor timeline
    pub(crate) fn ancestor_timeline(&self) -> Option<&Arc<Timeline>> {
        self.ancestor_timeline.as_ref()
    }

    /// Get the bytes written since the PITR cutoff on this branch, and
    /// whether this branch's ancestor_lsn is within its parent's PITR.
    pub(crate) fn get_pitr_history_stats(&self) -> (u64, bool) {
        // TODO: for backwards compatibility, we return the full history back to 0 when the PITR
        // cutoff has not yet been initialized. This should return None instead, but this is exposed
        // in external HTTP APIs and callers may not handle a null value.
        let gc_info = self.gc_info.read().unwrap();
        let history = self
            .get_last_record_lsn()
            .checked_sub(gc_info.cutoffs.time.unwrap_or_default())
            .unwrap_or_default()
            .0;
        (history, gc_info.within_ancestor_pitr)
    }

    /// Read timeline's GC cutoff: this is the LSN at which GC has started to happen
    pub(crate) fn get_applied_gc_cutoff_lsn(&self) -> RcuReadGuard<Lsn> {
        self.applied_gc_cutoff_lsn.read()
    }

    /// Read timeline's planned GC cutoff: this is the logical end of history that users are allowed
    /// to read (based on configured PITR), even if physically we have more history. Returns None
    /// if the PITR cutoff has not yet been initialized.
    pub(crate) fn get_gc_cutoff_lsn(&self) -> Option<Lsn> {
        self.gc_info.read().unwrap().cutoffs.time
    }

    /// Look up given page version.
    ///
    /// If a remote layer file is needed, it is downloaded as part of this
    /// call.
    ///
    /// This method enforces [`Self::pagestream_throttle`] internally.
    ///
    /// NOTE: It is considered an error to 'get' a key that doesn't exist. The
    /// abstraction above this needs to store suitable metadata to track what
    /// data exists with what keys, in separate metadata entries. If a
    /// non-existent key is requested, we may incorrectly return a value from
    /// an ancestor branch, for example, or waste a lot of cycles chasing the
    /// non-existing key.
    ///
    /// # Cancel-Safety
    ///
    /// This method is cancellation-safe.
    #[inline(always)]
    pub(crate) async fn get(
        &self,
        key: Key,
        lsn: Lsn,
        ctx: &RequestContext,
    ) -> Result<Bytes, PageReconstructError> {
        if !lsn.is_valid() {
            return Err(PageReconstructError::Other(anyhow::anyhow!("Invalid LSN")));
        }

        // This check is debug-only because of the cost of hashing, and because it's a double-check: we
        // already checked the key against the shard_identity when looking up the Timeline from
        // page_service.
        debug_assert!(!self.shard_identity.is_key_disposable(&key));

        let mut reconstruct_state = ValuesReconstructState::new(IoConcurrency::sequential());

        let query = VersionedKeySpaceQuery::uniform(KeySpace::single(key..key.next()), lsn);

        let vectored_res = self
            .get_vectored_impl(query, &mut reconstruct_state, ctx)
            .await;

        let key_value = vectored_res?.pop_first();
        match key_value {
            Some((got_key, value)) => {
                if got_key != key {
                    error!(
                        "Expected {}, but singular vectored get returned {}",
                        key, got_key
                    );
                    Err(PageReconstructError::Other(anyhow!(
                        "Singular vectored get returned wrong key"
                    )))
                } else {
                    value
                }
            }
            None => Err(PageReconstructError::MissingKey(Box::new(
                MissingKeyError {
                    keyspace: KeySpace::single(key..key.next()),
                    shard: self.shard_identity.get_shard_number(&key),
                    original_hwm_lsn: lsn,
                    ancestor_lsn: None,
                    backtrace: None,
                    query: None,
                },
            ))),
        }
    }

    /// Look up multiple page versions at a given LSN
    ///
    /// This naive implementation will be replaced with a more efficient one
    /// which actually vectorizes the read path.
    ///
    /// NB: the read path must be cancellation-safe. The Tonic gRPC service will drop the future
    /// if the client goes away (e.g. due to timeout or cancellation).
    pub(crate) async fn get_vectored(
        &self,
        query: VersionedKeySpaceQuery,
        io_concurrency: super::storage_layer::IoConcurrency,
        ctx: &RequestContext,
    ) -> Result<BTreeMap<Key, Result<Bytes, PageReconstructError>>, GetVectoredError> {
        let total_keyspace = query.total_keyspace();

        let key_count = total_keyspace.total_raw_size();
        if key_count > self.conf.max_get_vectored_keys.get() {
            return Err(GetVectoredError::Oversized(
                key_count as u64,
                self.conf.max_get_vectored_keys.get() as u64,
            ));
        }

        for range in &total_keyspace.ranges {
            let mut key = range.start;
            while key != range.end {
                assert!(!self.shard_identity.is_key_disposable(&key));
                key = key.next();
            }
        }

        trace!(
            "get vectored query {} from task kind {:?}",
            query,
            ctx.task_kind(),
        );

        let start = crate::metrics::GET_VECTORED_LATENCY
            .for_task_kind(ctx.task_kind())
            .map(|metric| (metric, Instant::now()));

        let res = self
            .get_vectored_impl(query, &mut ValuesReconstructState::new(io_concurrency), ctx)
            .await;

        if let Some((metric, start)) = start {
            let elapsed = start.elapsed();
            metric.observe(elapsed.as_secs_f64());
        }

        res
    }

    /// Scan the keyspace and return all existing key-values in the keyspace. This currently uses vectored
    /// get underlying. Normal vectored get would throw an error when a key in the keyspace is not found
    /// during the search, but for the scan interface, it returns all existing key-value pairs, and does
    /// not expect each single key in the key space will be found. The semantics is closer to the RocksDB
    /// scan iterator interface. We could optimize this interface later to avoid some checks in the vectored
    /// get path to maintain and split the probing and to-be-probe keyspace. We also need to ensure that
    /// the scan operation will not cause OOM in the future.
    pub(crate) async fn scan(
        &self,
        keyspace: KeySpace,
        lsn: Lsn,
        ctx: &RequestContext,
        io_concurrency: super::storage_layer::IoConcurrency,
    ) -> Result<BTreeMap<Key, Result<Bytes, PageReconstructError>>, GetVectoredError> {
        if !lsn.is_valid() {
            return Err(GetVectoredError::InvalidLsn(lsn));
        }

        trace!(
            "key-value scan request for {:?}@{} from task kind {:?}",
            keyspace,
            lsn,
            ctx.task_kind()
        );

        // We should generalize this into Keyspace::contains in the future.
        for range in &keyspace.ranges {
            if range.start.field1 < METADATA_KEY_BEGIN_PREFIX
                || range.end.field1 > METADATA_KEY_END_PREFIX
            {
                return Err(GetVectoredError::Other(anyhow::anyhow!(
                    "only metadata keyspace can be scanned"
                )));
            }
        }

        let start = crate::metrics::SCAN_LATENCY
            .for_task_kind(ctx.task_kind())
            .map(ScanLatencyOngoingRecording::start_recording);

        let query = VersionedKeySpaceQuery::uniform(keyspace, lsn);

        let vectored_res = self
            .get_vectored_impl(query, &mut ValuesReconstructState::new(io_concurrency), ctx)
            .await;

        if let Some(recording) = start {
            recording.observe();
        }

        vectored_res
    }

    pub(super) async fn get_vectored_impl(
        &self,
        query: VersionedKeySpaceQuery,
        _reconstruct_state: &mut ValuesReconstructState,
        ctx: &RequestContext,
    ) -> Result<BTreeMap<Key, Result<Bytes, PageReconstructError>>, GetVectoredError> {
        if query.is_empty() {
            return Ok(BTreeMap::default());
        }

        // yb-pageserver read path: every range goes through the row
        // store. Inside `get_vectored_via_page_writer` we classify each
        // range by its start key:
        //   - rel-page  → pageserver.pages    (chain + walredo)
        //   - slru-block → pageserver.slru_pages (chain + walredo)
        //   - else      → pageserver.meta     (single payload, no walredo)
        //
        // Production yb-pageserver always has `page_writer = Some(_)`;
        // the legacy LayerMap traversal that used to fall through here
        // is gone (Phase 4d). If page_writer is None at this point the
        // tenant is misconfigured — surface that as a clear error
        // rather than a panic from `expect`.
        let Some(page_writer) = self.page_writer.clone() else {
            return Err(GetVectoredError::Other(anyhow::anyhow!(
                "Timeline has no PageWriter — yb-pageserver mode requires both \
                 compute_page_store and internal_page_store to be configured"
            )));
        };
        let redo_attempt_type = if ctx.task_kind() == TaskKind::Compaction {
            RedoAttemptType::LegacyCompaction
        } else {
            RedoAttemptType::ReadPage
        };
        self.get_vectored_via_page_writer(page_writer, query, redo_attempt_type, ctx)
            .await
    }

    /// yb-pageserver row-per-page read path. Replaces the LayerMap
    /// traversal: per requested key at its target LSN, read the chain
    /// from `PageWriter::scan_range` (which blends in-RAM buffered
    /// rows with `pageserver.pages`), decode into walredo's
    /// `ValueReconstructState` shape, drive `reconstruct_value` to
    /// produce the final 8 KiB page.
    ///
    /// Each chain is LSN-descending and pre-truncated at the latest
    /// `will_init=true` anchor (image or init-delta). Chain length is
    /// bounded by the background image materialiser, not at write time,
    /// so a cold key may produce a long chain — correctness is
    /// guaranteed; performance scales with how recently the bg task
    /// last touched the key.
    async fn get_vectored_via_page_writer(
        &self,
        page_writer: Arc<crate::page_writer::PageWriter>,
        query: VersionedKeySpaceQuery,
        redo_attempt_type: RedoAttemptType,
        _ctx: &RequestContext,
    ) -> Result<BTreeMap<Key, Result<Bytes, PageReconstructError>>, GetVectoredError> {
        let total_keyspace = query.total_keyspace();
        let target_lsn = query.high_watermark_lsn()?;

        let mut chains_by_key: std::collections::HashMap<Key, Vec<crate::page_store::PageRow>> =
            std::collections::HashMap::new();
        // Meta keys go straight into the results map — no walredo.
        let mut meta_results: BTreeMap<Key, Result<Bytes, PageReconstructError>> = BTreeMap::new();

        for range in &total_keyspace.ranges {
            if range.start.is_rel_block_key() {
                let scan = page_writer
                    .scan_range(range.clone(), target_lsn)
                    .await
                    .map_err(|e| {
                        GetVectoredError::Other(anyhow!("PageStore scan_range failed: {e:#}"))
                    })?;
                for (key, chain) in scan {
                    chains_by_key.entry(key).or_default().extend(chain);
                }
            } else if range.start.is_slru_block_key() {
                // SLRU range — always stays within one (kind, segno).
                let (kind, segno, blk_lo) = range.start.to_slru_block().map_err(|e| {
                    GetVectoredError::Other(anyhow!("SLRU range start decode: {e:#}"))
                })?;
                let blk_hi = range.end.field6;
                let scan = page_writer
                    .scan_slru_range(kind, segno, blk_lo..blk_hi, target_lsn)
                    .await
                    .map_err(|e| {
                        GetVectoredError::Other(anyhow!("SLRU scan_range failed: {e:#}"))
                    })?;
                for (blknum, chain) in scan {
                    let key = pageserver_api::key::slru_block_to_key(kind, segno, blknum);
                    let converted: Vec<crate::page_store::PageRow> = chain
                        .into_iter()
                        .map(|sr| crate::page_store::PageRow {
                            key: slru_key_bytes(&sr),
                            lsn: sr.lsn,
                            is_image: sr.is_image,
                            will_init: sr.will_init,
                            payload: sr.payload,
                        })
                        .collect();
                    chains_by_key.entry(key).or_default().extend(converted);
                }
            } else {
                // Meta range: latest payload per key, no walredo.
                let scan = page_writer
                    .scan_meta_range(range.clone(), target_lsn)
                    .await
                    .map_err(|e| {
                        GetVectoredError::Other(anyhow!("meta scan_range failed: {e:#}"))
                    })?;
                for (_key_bytes, row) in scan {
                    if row.key.len() == pageserver_api::key::KEY_SIZE {
                        let k = Key::from_slice(&row.key);
                        meta_results.insert(k, Ok(row.payload));
                    }
                }
            }
        }

        // For each key, drive walredo in parallel.
        let futs = futures::stream::FuturesUnordered::new();
        for (key, chain) in chains_by_key {
            let target_lsn = query.map_key_to_lsn(&key);
            // Filter chain rows to those at lsn ≤ target_lsn (in case
            // the caller-asked LSN differs across keys); the chain is
            // already LSN-descending so we just truncate.
            let mut chain: Vec<_> =
                chain.into_iter().filter(|r| r.lsn <= target_lsn).collect();
            // Truncate at the first will_init=true row — older rows are
            // shadowed by that anchor.
            if let Some(idx) = chain.iter().position(|r| r.will_init) {
                chain.truncate(idx + 1);
            }
            let walredo_self = self.myself.upgrade().expect("&self method holds the arc");
            futs.push(async move {
                let data = match chain_to_value_reconstruct_state(&chain) {
                    Ok(d) => d,
                    Err(e) => return (key, Err(PageReconstructError::from(e))),
                };
                DELTAS_PER_READ_GLOBAL.observe(data.num_deltas() as f64);
                let res = walredo_self
                    .reconstruct_value(key, target_lsn, data, redo_attempt_type)
                    .await;
                (key, res)
            });
        }

        use futures::StreamExt;
        let mut results: BTreeMap<Key, Result<Bytes, PageReconstructError>> = meta_results;
        futures::pin_mut!(futs);
        while let Some((key, res)) = futs.next().await {
            results.insert(key, res);
        }
        Ok(results)
    }

    /// Get last or prev record separately. Same as get_last_record_rlsn().last/prev.
    pub(crate) fn get_last_record_lsn(&self) -> Lsn {
        self.last_record_lsn.load().last
    }

    pub(crate) fn get_prev_record_lsn(&self) -> Lsn {
        self.last_record_lsn.load().prev
    }

    /// Atomically get both last and prev.
    pub(crate) fn get_last_record_rlsn(&self) -> RecordLsn {
        self.last_record_lsn.load()
    }

    /// Subscribe to callers of wait_lsn(). The value of the channel is None if there are no
    /// wait_lsn() calls in progress, and Some(Lsn) if there is an active waiter for wait_lsn().
    pub(crate) fn subscribe_for_wait_lsn_updates(&self) -> watch::Receiver<Option<Lsn>> {
        self.last_record_lsn.status_receiver()
    }

    pub(crate) fn get_disk_consistent_lsn(&self) -> Lsn {
        self.disk_consistent_lsn.load()
    }

    /// Phase 5b: write the full timeline row into `pageserver.timelines`
    /// once at creation time, so the per-flush `advance_disk_consistent_lsn`
    /// UPDATE has a row to target. Also seeds `PageWriter`'s in-memory
    /// `dc_lsn_watermark` from the just-written `disk_consistent_lsn` —
    /// otherwise the first flush would redundantly UPDATE to a value
    /// already on disk.
    ///
    /// No-op when `page_writer` is None (upstream pageserver mode). The
    /// upsert uses `internal_page_store` so the bootstrap I/O doesn't
    /// land on the compute-reads connection.
    pub(crate) async fn write_timeline_row_to_yb(&self) -> anyhow::Result<()> {
        let Some(pw) = self.page_writer.as_ref() else {
            return Ok(());
        };
        let dc_lsn = self.get_disk_consistent_lsn();
        let gc_cutoff = *self.get_applied_gc_cutoff_lsn();
        let ancestor_id = self.get_ancestor_timeline_id();
        let ancestor_lsn = if ancestor_id.is_some() {
            Some(self.get_ancestor_lsn())
        } else {
            None
        };
        let row = crate::page_store::TimelineRow {
            tenant_id: self.tenant_shard_id.tenant_id,
            timeline_id: self.timeline_id,
            disk_consistent_lsn: dc_lsn,
            applied_gc_cutoff_lsn: gc_cutoff,
            ancestor_timeline_id: ancestor_id,
            ancestor_lsn,
            // Generation::into() returns None for `Generation::None`; in
            // attached state we always have a Valid generation from the
            // storage controller's most recent attach.
            current_generation: self.generation.into().unwrap_or(0) as i32,
            pg_version: self.pg_version.major_version_num(),
        };
        pw.internal_page_store()
            .upsert_timeline(&row)
            .await
            .context("upsert_timeline at creation")?;
        pw.seed_dc_lsn_watermark(dc_lsn);
        Ok(())
    }

    /// remote_consistent_lsn from the perspective of the tenant's current generation,
    /// not validated with control plane yet.
    /// See [`Self::get_remote_consistent_lsn_visible`].
    pub(crate) fn get_remote_consistent_lsn_projected(&self) -> Option<Lsn> {
        self.remote_client.remote_consistent_lsn_projected()
    }

    /// remote_consistent_lsn which the tenant is guaranteed not to go backward from,
    /// i.e. a value of remote_consistent_lsn_projected which has undergone
    /// generation validation in the deletion queue.
    pub(crate) fn get_remote_consistent_lsn_visible(&self) -> Option<Lsn> {
        self.remote_client.remote_consistent_lsn_visible()
    }

    /// The sum of the file size of all historic layers in the layer map.
    /// This method makes no distinction between local and remote layers.
    /// Hence, the result **does not represent local filesystem usage**.
    pub(crate) async fn layer_size_sum(&self) -> u64 {
        let guard = self
            .layers
            .read(LayerManagerLockHolder::GetLayerMapInfo)
            .await;
        guard.layer_size_sum()
    }

    pub(crate) fn get_directory_metrics(&self) -> [u64; DirectoryKind::KINDS_NUM] {
        array::from_fn(|idx| self.directory_metrics[idx].load(AtomicOrdering::Relaxed))
    }

    ///
    /// Wait until WAL has been received and processed up to this LSN.
    ///
    /// You should call this before any of the other get_* or list_* functions. Calling
    /// those functions with an LSN that has been processed yet is an error.
    ///
    pub(crate) async fn wait_lsn(
        &self,
        lsn: Lsn,
        who_is_waiting: WaitLsnWaiter,
        timeout: WaitLsnTimeout,
        ctx: &RequestContext, /* Prepare for use by cancellation */
    ) -> Result<(), WaitLsnError> {
        let state = self.current_state();
        if self.cancel.is_cancelled() || matches!(state, TimelineState::Stopping) {
            return Err(WaitLsnError::Shutdown);
        } else if !matches!(state, TimelineState::Active) {
            return Err(WaitLsnError::BadState(state));
        }

        if cfg!(debug_assertions) {
            match ctx.task_kind() {
                TaskKind::WalReceiverManager
                | TaskKind::WalReceiverConnectionHandler
                | TaskKind::WalReceiverConnectionPoller => {
                    match who_is_waiting {
                        WaitLsnWaiter::Tenant
                        | WaitLsnWaiter::PageService
                        | WaitLsnWaiter::HttpEndpoint
                        | WaitLsnWaiter::BaseBackupCache => unreachable!(
                            "tenant or page_service context are not expected to have task kind {:?}",
                            ctx.task_kind()
                        ),
                    }
                }
                _ => {}
            }
        }

        let timeout = match timeout {
            WaitLsnTimeout::Custom(t) => t,
            WaitLsnTimeout::Default => self.conf.wait_lsn_timeout,
        };

        let timer = crate::metrics::WAIT_LSN_TIME.start_timer();
        let start_finish_counterpair_guard = self.metrics.wait_lsn_start_finish_counterpair.guard();

        let wait_for_timeout = self.last_record_lsn.wait_for_timeout(lsn, timeout);
        let wait_for_timeout = std::pin::pin!(wait_for_timeout);
        // Use threshold of 1 because even 1 second of wait for ingest is very much abnormal.
        let log_slow_threshold = Duration::from_secs(1);
        // Use period of 10 to avoid flooding logs during an outage that affects all timelines.
        let log_slow_period = Duration::from_secs(10);
        let mut logging_permit = None;
        let wait_for_timeout = monitor_slow_future(
            log_slow_threshold,
            log_slow_period,
            wait_for_timeout,
            |MonitorSlowFutureCallback {
                 ready,
                 is_slow,
                 elapsed_total,
                 elapsed_since_last_callback,
             }| {
                self.metrics
                    .wait_lsn_in_progress_micros
                    .inc_by(u64::try_from(elapsed_since_last_callback.as_micros()).unwrap());
                if !is_slow {
                    return;
                }
                // It's slow, see if we should log it.
                // (We limit the logging to one per invocation per timeline to avoid excessive
                // logging during an extended broker / networking outage that affects all timelines.)
                if logging_permit.is_none() {
                    logging_permit = self.wait_lsn_log_slow.try_acquire().ok();
                }
                if logging_permit.is_none() {
                    return;
                }
                // We log it.
                if ready {
                    info!(
                        "slow wait_lsn completed after {:.3}s",
                        elapsed_total.as_secs_f64()
                    );
                } else {
                    info!(
                        "slow wait_lsn still running for {:.3}s",
                        elapsed_total.as_secs_f64()
                    );
                }
            },
        );
        let res = wait_for_timeout.await;
        // don't count the time spent waiting for lock below, and also in walreceiver.status(), towards the wait_lsn_time_histo
        drop(logging_permit);
        drop(start_finish_counterpair_guard);
        drop(timer);
        match res {
            Ok(()) => Ok(()),
            Err(e) => {
                use utils::seqwait::SeqWaitError::*;
                match e {
                    Shutdown => Err(WaitLsnError::Shutdown),
                    Timeout => {
                        let walreceiver_status = self.walreceiver_status();
                        Err(WaitLsnError::Timeout(format!(
                            "Timed out while waiting for WAL record at LSN {} to arrive, last_record_lsn {} disk consistent LSN={}, WalReceiver status: {}",
                            lsn,
                            self.get_last_record_lsn(),
                            self.get_disk_consistent_lsn(),
                            walreceiver_status,
                        )))
                    }
                }
            }
        }
    }

    pub(crate) fn walreceiver_status(&self) -> String {
        match &*self.walreceiver.lock().unwrap() {
            None => "stopping or stopped".to_string(),
            Some(walreceiver) => match walreceiver.status() {
                Some(status) => status.to_human_readable_string(),
                None => "Not active".to_string(),
            },
        }
    }

    /// Check that it is valid to request operations with that lsn.
    pub(crate) fn check_lsn_is_in_scope(
        &self,
        lsn: Lsn,
        latest_gc_cutoff_lsn: &RcuReadGuard<Lsn>,
    ) -> anyhow::Result<()> {
        ensure!(
            lsn >= **latest_gc_cutoff_lsn,
            "LSN {} is earlier than latest GC cutoff {} (we might've already garbage collected needed data)",
            lsn,
            **latest_gc_cutoff_lsn,
        );
        Ok(())
    }

    /// Initializes an LSN lease. The function will return an error if the requested LSN is less than the `latest_gc_cutoff_lsn`.
    pub(crate) fn init_lsn_lease(
        &self,
        lsn: Lsn,
        length: Duration,
        ctx: &RequestContext,
    ) -> anyhow::Result<LsnLease> {
        self.make_lsn_lease(lsn, length, true, ctx)
    }

    /// Renews a lease at a particular LSN. The requested LSN is not validated against the `latest_gc_cutoff_lsn` when we are in the grace period.
    pub(crate) fn renew_lsn_lease(
        &self,
        lsn: Lsn,
        length: Duration,
        ctx: &RequestContext,
    ) -> anyhow::Result<LsnLease> {
        self.make_lsn_lease(lsn, length, false, ctx)
    }

    /// Obtains a temporary lease blocking garbage collection for the given LSN.
    ///
    /// If we are in `AttachedSingle` mode and is not blocked by the lsn lease deadline, this function will error
    /// if the requesting LSN is less than the `latest_gc_cutoff_lsn` and there is no existing request present.
    ///
    /// If there is an existing lease in the map, the lease will be renewed only if the request extends the lease.
    /// The returned lease is therefore the maximum between the existing lease and the requesting lease.
    fn make_lsn_lease(
        &self,
        lsn: Lsn,
        length: Duration,
        init: bool,
        _ctx: &RequestContext,
    ) -> anyhow::Result<LsnLease> {
        let lease = {
            // Normalize the requested LSN to be aligned, and move to the first record
            // if it points to the beginning of the page (header).
            let lsn = xlog_utils::normalize_lsn(lsn, WAL_SEGMENT_SIZE);

            let mut gc_info = self.gc_info.write().unwrap();
            let planned_cutoff = gc_info.min_cutoff();

            let valid_until = SystemTime::now() + length;

            let entry = gc_info.leases.entry(lsn);

            match entry {
                Entry::Occupied(mut occupied) => {
                    let existing_lease = occupied.get_mut();
                    if valid_until > existing_lease.valid_until {
                        existing_lease.valid_until = valid_until;
                        let dt: DateTime<Utc> = valid_until.into();
                        info!("lease extended to {}", dt);
                    } else {
                        let dt: DateTime<Utc> = existing_lease.valid_until.into();
                        info!("existing lease covers greater length, valid until {}", dt);
                    }

                    existing_lease.clone()
                }
                Entry::Vacant(vacant) => {
                    // Never allow a lease to be requested for an LSN below the applied GC cutoff. The data could have been deleted.
                    let latest_gc_cutoff_lsn = self.get_applied_gc_cutoff_lsn();
                    if lsn < *latest_gc_cutoff_lsn {
                        bail!(
                            "tried to request an lsn lease for an lsn below the latest gc cutoff. requested at {} gc cutoff {}",
                            lsn,
                            *latest_gc_cutoff_lsn
                        );
                    }

                    // We allow create lease for those below the planned gc cutoff if we are still within the grace period
                    // of GC blocking.
                    let validate = {
                        let conf = self.tenant_conf.load();
                        !conf.is_gc_blocked_by_lsn_lease_deadline()
                    };

                    // Do not allow initial lease creation to be below the planned gc cutoff. The client (compute_ctl) determines
                    // whether it is a initial lease creation or a renewal.
                    if (init || validate) && lsn < planned_cutoff {
                        bail!(
                            "tried to request an lsn lease for an lsn below the planned gc cutoff. requested at {} planned gc cutoff {}",
                            lsn,
                            planned_cutoff
                        );
                    }

                    let dt: DateTime<Utc> = valid_until.into();
                    info!("lease created, valid until {}", dt);
                    vacant.insert(LsnLease { valid_until }).clone()
                }
            }
        };

        Ok(lease)
    }

    /// yb-mode: equivalent to `freeze_and_flush` (drain `PageWriter`
    /// row buffer into YB). The `u64` return is a legacy flush-token
    /// from the Neon flush_loop machinery — always 0 in yb-mode.
    #[instrument(skip(self), fields(tenant_id=%self.tenant_shard_id.tenant_id, shard_id=%self.tenant_shard_id.shard_slug(), timeline_id=%self.timeline_id))]
    pub(crate) async fn freeze(&self) -> Result<u64, FlushLayerError> {
        self.freeze_and_flush0().await?;
        Ok(0)
    }

    /// Freeze and flush the open in-memory layer, waiting for it to be written to disk.
    #[instrument(skip(self), fields(tenant_id=%self.tenant_shard_id.tenant_id, shard_id=%self.tenant_shard_id.shard_slug(), timeline_id=%self.timeline_id))]
    /// yb-pageserver durability checkpoint: drain the `PageWriter`
    /// row-INSERT buffer into YugabyteDB.
    ///
    /// Replaces the original Neon freeze-inmem-layer → flush-to-disk
    /// pipeline (which produced delta + image layer files). In yb-mode
    /// the equivalent of "make pending writes durable" is to flush the
    /// pending row buffer; YB's Raft replication takes it from there.
    /// No-op if there's no `PageWriter` (a yb-pageserver running with
    /// no compute_page_store + internal_page_store wouldn't accept
    /// writes in the first place).
    pub(crate) async fn freeze_and_flush(&self) -> Result<(), FlushLayerError> {
        self.freeze_and_flush0().await
    }

    pub(crate) async fn freeze_and_flush0(&self) -> Result<(), FlushLayerError> {
        if let Some(pw) = &self.page_writer {
            pw.flush()
                .await
                .map_err(|e| FlushLayerError::Other(Arc::new(e)))?;
        }
        Ok(())
    }

    /// yb-mode: no-op. In Neon this background task closes open
    /// ephemeral InMemoryLayers when idle / oversized. yb-pageserver
    /// never opens an InMemoryLayer (writes go straight into
    /// `PageWriter`'s row buffer and through to YB), so there is
    /// nothing to roll. The `PageWriter` has its own size + age
    /// triggers (`should_flush_*`) and a dedicated background flush.
    pub(super) async fn maybe_freeze_ephemeral_layer(&self) {}

    /// Test-only no-op stub left after Phase 4d.2 deleted compaction.rs.
    /// Tests under tenant.rs still call `.compact()` to drive
    /// freeze+flush+compact sequences; with no layer files there's
    /// nothing to compact, so this just returns Done.
    #[cfg(test)]
    pub(crate) async fn compact(
        self: &std::sync::Arc<Self>,
        _cancel: &CancellationToken,
        _flags: enumset::EnumSet<CompactFlags>,
        _ctx: &RequestContext,
    ) -> Result<(), anyhow::Error> {
        Ok(())
    }

    /// Mutate the timeline with a [`TimelineWriter`].
    pub(crate) async fn writer(&self) -> TimelineWriter<'_> {
        TimelineWriter {
            tl: self,
            _write_guard: self.write_lock.lock().await,
        }
    }

    pub(crate) fn activate(
        self: &Arc<Self>,
        parent: Arc<crate::tenant::TenantShard>,
        broker_client: BrokerClientChannel,
        background_jobs_can_start: Option<&completion::Barrier>,
        ctx: &RequestContext,
    ) {
        if self.tenant_shard_id.is_shard_zero() {
            // Logical size is only maintained accurately on shard zero.
            self.spawn_initial_logical_size_computation_task(ctx);
        }
        self.launch_wal_receiver(ctx, broker_client);
        self.set_state(TimelineState::Active);
        // Per-timeline eviction task deleted in Phase 4d.2 — no layer
        // files to evict in the YB row-store world.
        let _ = (parent, background_jobs_can_start);
    }

    /// After this function returns, there are no timeline-scoped tasks are left running.
    ///
    /// The preferred pattern for is:
    /// - in any spawned tasks, keep Timeline::guard open + Timeline::cancel / child token
    /// - if early shutdown (not just cancellation) of a sub-tree of tasks is required,
    ///   go the extra mile and keep track of JoinHandles
    /// - Keep track of JoinHandles using a passed-down `Arc<Mutex<Option<JoinSet>>>` or similar,
    ///   instead of spawning directly on a runtime. It is a more composable / testable pattern.
    ///
    /// For legacy reasons, we still have multiple tasks spawned using
    /// `task_mgr::spawn(X, Some(tenant_id), Some(timeline_id))`.
    /// We refer to these as "timeline-scoped task_mgr tasks".
    /// Some of these tasks are already sensitive to Timeline::cancel while others are
    /// not sensitive to Timeline::cancel and instead respect [`task_mgr::shutdown_token`]
    /// or [`task_mgr::shutdown_watcher`].
    /// We want to gradually convert the code base away from these.
    ///
    /// Here is an inventory of timeline-scoped task_mgr tasks that are still sensitive to
    /// `task_mgr::shutdown_{token,watcher}` (there are also tenant-scoped and global-scoped
    /// ones that aren't mentioned here):
    /// - [`TaskKind::TimelineDeletionWorker`]
    ///    - NB: also used for tenant deletion
    /// - [`TaskKind::RemoteUploadTask`]`
    /// - [`TaskKind::InitialLogicalSizeCalculation`]
    /// - [`TaskKind::DownloadAllRemoteLayers`] (can we get rid of it?)
    // Inventory of timeline-scoped task_mgr tasks that use spawn but aren't sensitive:
    /// - [`TaskKind::Eviction`]
    /// - [`TaskKind::LayerFlushTask`]
    /// - [`TaskKind::OndemandLogicalSizeCalculation`]
    /// - [`TaskKind::GarbageCollector`] (immediate_gc is timeline-scoped)
    pub(crate) async fn shutdown(&self, mode: ShutdownMode) {
        debug_assert_current_span_has_tenant_and_timeline_id();

        // Regardless of whether we're going to try_freeze_and_flush
        // cancel walreceiver to stop ingesting more data asap.
        //
        // Note that we're accepting a race condition here where we may
        // do the final flush below, before walreceiver observes the
        // cancellation and exits.
        // This means we may open a new InMemoryLayer after the final flush below.
        // Flush loop is also still running for a short while, so, in theory, it
        // could also make its way into the upload queue.
        //
        // If we wait for the shutdown of the walreceiver before moving on to the
        // flush, then that would be avoided. But we don't do it because the
        // walreceiver entertains reads internally, which means that it possibly
        // depends on the download of layers. Layer download is only sensitive to
        // the cancellation of the entire timeline, so cancelling the walreceiver
        // will have no effect on the individual get requests.
        // This would cause problems when there is a lot of ongoing downloads or
        // there is S3 unavailabilities, i.e. detach, deletion, etc would hang,
        // and we can't deallocate resources of the timeline, etc.
        let walreceiver = self.walreceiver.lock().unwrap().take();
        tracing::debug!(
            is_some = walreceiver.is_some(),
            "Waiting for WalReceiverManager..."
        );
        if let Some(walreceiver) = walreceiver {
            walreceiver.cancel().await;
        }
        // ... and inform any waiters for newer LSNs that there won't be any.
        self.last_record_lsn.shutdown();

        // Stop the page_writer's periodic flusher and drain any
        // remaining buffered rows. After walreceiver has cancelled, no
        // new put_batch calls will arrive, so the buffer can be flushed
        // to completion. We do this before the layer-stack flush below
        // so a failure here doesn't deadlock the rest of shutdown.
        let flush_task = self.page_writer_flush_task.lock().unwrap().take();
        if let Some(task) = flush_task {
            task.shutdown().await;
        }
        if let Some(pw) = self.page_writer.as_ref()
            && let Err(e) = pw.flush().await
        {
            tracing::warn!("page_writer final flush failed during shutdown: {e:?}");
        }

        // Stop the image materialiser AFTER the final flush so any
        // jobs queued during the flush get a chance to drain. The
        // shutdown is best-effort — pending jobs at cancel time are
        // simply dropped; the next pageserver start sees longer
        // chains and re-enqueues organically.
        let mat_task = self.image_materializer_task.lock().unwrap().take();
        if let Some(task) = mat_task {
            task.shutdown().await;
        }

        if let ShutdownMode::FreezeAndFlush = mode {
            let do_flush = if let Some((open, frozen)) = self
                .layers
                .read(LayerManagerLockHolder::Shutdown)
                .await
                .layer_map()
                .map(|lm| (lm.open_layer.is_some(), lm.frozen_layers.len()))
                .ok()
                .filter(|(open, frozen)| *open || *frozen > 0)
            {
                if self.remote_client.is_archived() == Some(true) {
                    // No point flushing on shutdown for an archived timeline: it is not important
                    // to have it nice and fresh after our restart, and trying to flush here might
                    // race with trying to offload it (which also stops the flush loop)
                    false
                } else {
                    tracing::info!(?open, frozen, "flushing and freezing on shutdown");
                    true
                }
            } else {
                // this is double-shutdown, it'll be a no-op
                true
            };

            // we shut down walreceiver above, so, we won't add anything more
            // to the InMemoryLayer; freeze it and wait for all frozen layers
            // to reach the disk & upload queue, then shut the upload queue and
            // wait for it to drain.
            if do_flush {
                match self.freeze_and_flush().await {
                    Ok(_) => {
                        // drain the upload queue
                        // if we did not wait for completion here, it might be our shutdown process
                        // didn't wait for remote uploads to complete at all, as new tasks can forever
                        // be spawned.
                        //
                        // what is problematic is the shutting down of RemoteTimelineClient, because
                        // obviously it does not make sense to stop while we wait for it, but what
                        // about corner cases like s3 suddenly hanging up?
                        self.remote_client.shutdown().await;
                    }
                    Err(FlushLayerError::Cancelled) => {
                        // this is likely the second shutdown, ignore silently.
                        // TODO: this can be removed once https://github.com/neondatabase/neon/issues/5080
                        debug_assert!(self.cancel.is_cancelled());
                    }
                    Err(e) => {
                        // Non-fatal.  Shutdown is infallible.  Failures to flush just mean that
                        // we have some extra WAL replay to do next time the timeline starts.
                        warn!("failed to freeze and flush: {e:#}");
                    }
                }

                // `self.remote_client.shutdown().await` above should have already flushed everything from the queue, but
                // we also do a final check here to ensure that the queue is empty.
                if !self.remote_client.no_pending_work() {
                    warn!(
                        "still have pending work in remote upload queue, but continuing shutting down anyways"
                    );
                }
            }
        }

        if let ShutdownMode::Reload = mode {
            // drain the upload queue
            self.remote_client.shutdown().await;
            if !self.remote_client.no_pending_work() {
                warn!(
                    "still have pending work in remote upload queue, but continuing shutting down anyways"
                );
            }
        }

        // Signal any subscribers to our cancellation token to drop out
        tracing::debug!("Cancelling CancellationToken");
        self.cancel.cancel();

        // Ensure Prevent new page service requests from starting.
        self.handles.shutdown();

        // Transition the remote_client into a state where it's only useful for timeline deletion.
        // (The deletion use case is why we can't just hook up remote_client to Self::cancel).)
        self.remote_client.stop();

        // As documented in remote_client.stop()'s doc comment, it's our responsibility
        // to shut down the upload queue tasks.
        // TODO: fix that, task management should be encapsulated inside remote_client.
        task_mgr::shutdown_tasks(
            Some(TaskKind::RemoteUploadTask),
            Some(self.tenant_shard_id),
            Some(self.timeline_id),
        )
        .await;

        // TODO: work toward making this a no-op. See this function's doc comment for more context.
        tracing::debug!("Waiting for tasks...");
        task_mgr::shutdown_tasks(None, Some(self.tenant_shard_id), Some(self.timeline_id)).await;

        {
            // Allow any remaining in-memory layers to do cleanup -- until that, they hold the gate
            // open.
            let mut write_guard = self.write_lock.lock().await;
            self.layers
                .write(LayerManagerLockHolder::Shutdown)
                .await
                .shutdown(&mut write_guard);
        }

        // Finally wait until any gate-holders are complete.
        //
        // TODO: once above shutdown_tasks is a no-op, we can close the gate before calling shutdown_tasks
        // and use a TBD variant of shutdown_tasks that asserts that there were no tasks left.
        self.gate.close().await;

        self.metrics.shutdown();
    }

    pub(crate) fn set_state(&self, new_state: TimelineState) {
        match (self.current_state(), new_state) {
            (equal_state_1, equal_state_2) if equal_state_1 == equal_state_2 => {
                info!("Ignoring new state, equal to the existing one: {equal_state_2:?}");
            }
            (st, TimelineState::Loading) => {
                error!("ignoring transition from {st:?} into Loading state");
            }
            (TimelineState::Broken { .. }, new_state) => {
                error!("Ignoring state update {new_state:?} for broken timeline");
            }
            (TimelineState::Stopping, TimelineState::Active) => {
                error!("Not activating a Stopping timeline");
            }
            (_, new_state) => {
                self.state.send_replace(new_state);
            }
        }
    }

    pub(crate) fn set_broken(&self, reason: String) {
        let backtrace_str: String = format!("{}", std::backtrace::Backtrace::force_capture());
        let broken_state = TimelineState::Broken {
            reason,
            backtrace: backtrace_str,
        };
        self.set_state(broken_state);

        // Although the Broken state is not equivalent to shutdown() (shutdown will be called
        // later when this tenant is detach or the process shuts down), firing the cancellation token
        // here avoids the need for other tasks to watch for the Broken state explicitly.
        self.cancel.cancel();
    }

    pub(crate) fn current_state(&self) -> TimelineState {
        self.state.borrow().clone()
    }

    pub(crate) fn is_broken(&self) -> bool {
        matches!(&*self.state.borrow(), TimelineState::Broken { .. })
    }

    pub(crate) fn is_active(&self) -> bool {
        self.current_state() == TimelineState::Active
    }

    pub(crate) fn is_archived(&self) -> Option<bool> {
        self.remote_client.is_archived()
    }

    pub(crate) fn is_stopping(&self) -> bool {
        self.current_state() == TimelineState::Stopping
    }

    pub(crate) fn subscribe_for_state_updates(&self) -> watch::Receiver<TimelineState> {
        self.state.subscribe()
    }

    pub(crate) async fn wait_to_become_active(
        &self,
        _ctx: &RequestContext, // Prepare for use by cancellation
    ) -> Result<(), TimelineState> {
        let mut receiver = self.state.subscribe();
        loop {
            let current_state = receiver.borrow().clone();
            match current_state {
                TimelineState::Loading => {
                    receiver
                        .changed()
                        .await
                        .expect("holding a reference to self");
                }
                TimelineState::Active => {
                    return Ok(());
                }
                TimelineState::Broken { .. } | TimelineState::Stopping => {
                    // There's no chance the timeline can transition back into ::Active
                    return Err(current_state);
                }
            }
        }
    }

    pub(crate) async fn layer_map_info(
        &self,
        reset: LayerAccessStatsReset,
    ) -> Result<LayerMapInfo, layer_manager::Shutdown> {
        let guard = self
            .layers
            .read(LayerManagerLockHolder::GetLayerMapInfo)
            .await;
        let layer_map = guard.layer_map()?;
        let mut in_memory_layers = Vec::with_capacity(layer_map.frozen_layers.len() + 1);
        if let Some(open_layer) = &layer_map.open_layer {
            in_memory_layers.push(open_layer.info());
        }
        for frozen_layer in &layer_map.frozen_layers {
            in_memory_layers.push(frozen_layer.info());
        }

        let historic_layers = layer_map
            .iter_historic_layers()
            .map(|desc| guard.get_from_desc(&desc).info(reset))
            .collect();

        Ok(LayerMapInfo {
            in_memory_layers,
            historic_layers,
        })
    }

    pub(crate) async fn download_layer(
        &self,
        _layer_file_name: &LayerName,
        _ctx: &RequestContext,
    ) -> anyhow::Result<Option<bool>> {
        // yb-mode: no layer files. The HTTP admin endpoint that calls
        // this is effectively a no-op; return None ("not found").
        Ok(None)
    }

    /// yb-mode: no layer files to evict. Always returns Ok(None).
    pub(crate) async fn evict_layer(
        &self,
        _layer_file_name: &LayerName,
    ) -> anyhow::Result<Option<bool>> {
        Ok(None)
    }


    pub(crate) fn is_basebackup_cache_enabled(&self) -> bool {
        let tenant_conf = self.tenant_conf.load();
        tenant_conf
            .tenant_conf
            .basebackup_cache_enabled
            .unwrap_or(self.conf.default_tenant_conf.basebackup_cache_enabled)
    }

    /// Try to get a basebackup from the on-disk cache.
    pub(crate) async fn get_cached_basebackup(&self, lsn: Lsn) -> Option<tokio::fs::File> {
        self.basebackup_cache
            .get(self.tenant_shard_id.tenant_id, self.timeline_id, lsn)
            .await
    }

    /// Convenience method to attempt fetching a basebackup for the timeline if enabled and safe for
    /// the given request parameters.
    ///
    /// TODO: consider moving this onto GrpcPageServiceHandler once the libpq handler is gone.
    pub async fn get_cached_basebackup_if_enabled(
        &self,
        lsn: Option<Lsn>,
        prev_lsn: Option<Lsn>,
        full: bool,
        replica: bool,
        gzip: bool,
    ) -> Option<tokio::fs::File> {
        if !self.is_basebackup_cache_enabled() || !self.basebackup_cache.is_enabled() {
            return None;
        }
        // We have to know which LSN to fetch the basebackup for.
        let lsn = lsn?;
        // We only cache gzipped, non-full basebackups for primary computes with automatic prev_lsn.
        if prev_lsn.is_some() || full || replica || !gzip {
            return None;
        }
        self.get_cached_basebackup(lsn).await
    }

    /// Prepare basebackup for the given LSN and store it in the basebackup cache.
    /// The method is asynchronous and returns immediately.
    /// The actual basebackup preparation is performed in the background
    /// by the basebackup cache on a best-effort basis.
    pub(crate) fn prepare_basebackup(&self, lsn: Lsn) {
        if !self.is_basebackup_cache_enabled() {
            return;
        }
        if !self.tenant_shard_id.is_shard_zero() {
            // In theory we should never get here, but just in case check it.
            // Preparing basebackup doesn't make sense for shards other than shard zero.
            return;
        }
        if !self.is_active() {
            // May happen during initial timeline creation.
            // Such timeline is not in the global timeline map yet,
            // so basebackup cache will not be able to find it.
            // TODO(diko): We can prepare such timelines in finish_creation().
            return;
        }

        self.basebackup_cache
            .send_prepare(self.tenant_shard_id, self.timeline_id, lsn);
    }
}

// Private functions
impl Timeline {
    pub(crate) fn get_lsn_lease_length(&self) -> Duration {
        let tenant_conf = self.tenant_conf.load();
        tenant_conf
            .tenant_conf
            .lsn_lease_length
            .unwrap_or(self.conf.default_tenant_conf.lsn_lease_length)
    }

    pub(crate) fn get_lsn_lease_length_for_ts(&self) -> Duration {
        let tenant_conf = self.tenant_conf.load();
        tenant_conf
            .tenant_conf
            .lsn_lease_length_for_ts
            .unwrap_or(self.conf.default_tenant_conf.lsn_lease_length_for_ts)
    }

    pub(crate) fn is_gc_blocked_by_lsn_lease_deadline(&self) -> bool {
        let tenant_conf = self.tenant_conf.load();
        tenant_conf.is_gc_blocked_by_lsn_lease_deadline()
    }

    pub(crate) fn get_lazy_slru_download(&self) -> bool {
        let tenant_conf = self.tenant_conf.load();
        tenant_conf
            .tenant_conf
            .lazy_slru_download
            .unwrap_or(self.conf.default_tenant_conf.lazy_slru_download)
    }

    /// Checks if a get page request should get perf tracing
    ///
    /// The configuration priority is: tenant config override, default tenant config,
    /// pageserver config.
    pub(crate) fn is_get_page_request_sampled(&self) -> bool {
        let tenant_conf = self.tenant_conf.load();
        let ratio = tenant_conf
            .tenant_conf
            .sampling_ratio
            .flatten()
            .or(self.conf.default_tenant_conf.sampling_ratio)
            .or(self.conf.tracing.as_ref().map(|t| t.sampling_ratio));

        match ratio {
            Some(r) => {
                if r.numerator == 0 {
                    false
                } else {
                    rand::rng().random_range(0..r.denominator) < r.numerator
                }
            }
            None => false,
        }
    }




    /// Returns `true` if the rel_size_v2 config is enabled. NOTE: the write path and read path
    /// should look at `get_rel_size_v2_status()` to get the actual status of the timeline. It is
    /// possible that the index part persists the state while the config doesn't get persisted.
    pub(crate) fn get_rel_size_v2_enabled(&self) -> bool {
        let tenant_conf = self.tenant_conf.load();
        tenant_conf
            .tenant_conf
            .rel_size_v2_enabled
            .unwrap_or(self.conf.default_tenant_conf.rel_size_v2_enabled)
    }

    pub(crate) fn get_rel_size_v2_status(&self) -> (RelSizeMigration, Option<Lsn>) {
        let (status, migrated_at) = self.rel_size_v2_status.load().as_ref().clone();
        (status.unwrap_or(RelSizeMigration::Legacy), migrated_at)
    }

    pub fn get_compaction_l0_first(&self) -> bool {
        let tenant_conf = self.tenant_conf.load().tenant_conf.clone();
        tenant_conf
            .compaction_l0_first
            .unwrap_or(self.conf.default_tenant_conf.compaction_l0_first)
    }

    pub fn get_compaction_l0_semaphore(&self) -> bool {
        let tenant_conf = self.tenant_conf.load().tenant_conf.clone();
        tenant_conf
            .compaction_l0_semaphore
            .unwrap_or(self.conf.default_tenant_conf.compaction_l0_semaphore)
    }




    // HADRON
    pub fn get_compaction_shard_ancestor(&self) -> bool {
        let tenant_conf = self.tenant_conf.load();
        tenant_conf
            .tenant_conf
            .compaction_shard_ancestor
            .unwrap_or(self.conf.default_tenant_conf.compaction_shard_ancestor)
    }

    fn get_evictions_low_residence_duration_metric_threshold(
        tenant_conf: &pageserver_api::models::TenantConfig,
        default_tenant_conf: &pageserver_api::config::TenantConfigToml,
    ) -> Duration {
        tenant_conf
            .evictions_low_residence_duration_metric_threshold
            .unwrap_or(default_tenant_conf.evictions_low_residence_duration_metric_threshold)
    }




    pub(super) fn tenant_conf_updated(&self, new_conf: &AttachedTenantConf) {
        // NB: Most tenant conf options are read by background loops, so,
        // changes will automatically be picked up.

        // The threshold is embedded in the metric. So, we need to update it.
        {
            let new_threshold = Self::get_evictions_low_residence_duration_metric_threshold(
                &new_conf.tenant_conf,
                &self.conf.default_tenant_conf,
            );

            let tenant_id_str = self.tenant_shard_id.tenant_id.to_string();
            let shard_id_str = format!("{}", self.tenant_shard_id.shard_slug());

            let timeline_id_str = self.timeline_id.to_string();

            self.remote_client.update_config(&new_conf.location);

            let mut rel_size_cache = self.rel_size_snapshot_cache.lock().unwrap();
            if let Some(new_capacity) = new_conf.tenant_conf.relsize_snapshot_cache_capacity {
                if new_capacity != rel_size_cache.capacity() {
                    rel_size_cache.set_capacity(new_capacity);
                }
            }

            self.metrics
                .evictions_with_low_residence_duration
                .write()
                .unwrap()
                .change_threshold(
                    &tenant_id_str,
                    &shard_id_str,
                    &timeline_id_str,
                    new_threshold,
                );
        }
    }

    /// Open a Timeline handle.
    ///
    /// Loads the metadata for the timeline into memory, but not the layer map.
    #[allow(clippy::too_many_arguments)]
    pub(super) fn new(
        conf: &'static PageServerConf,
        tenant_conf: Arc<ArcSwap<AttachedTenantConf>>,
        metadata: &TimelineMetadata,
        ancestor: Option<Arc<Timeline>>,
        timeline_id: TimelineId,
        tenant_shard_id: TenantShardId,
        generation: Generation,
        shard_identity: ShardIdentity,
        walredo_mgr: Option<Arc<super::WalRedoManager>>,
        resources: TimelineResources,
        pg_version: PgMajorVersion,
        state: TimelineState,
        attach_wal_lag_cooldown: Arc<OnceLock<WalLagCooldown>>,
        create_idempotency: crate::tenant::CreateTimelineIdempotency,
        gc_compaction_state: Option<GcCompactionState>,
        rel_size_v2_status: Option<RelSizeMigration>,
        rel_size_migrated_at: Option<Lsn>,
        cancel: CancellationToken,
    ) -> Arc<Self> {
        let disk_consistent_lsn = metadata.disk_consistent_lsn();
        let (state, _) = watch::channel(state);

        let evictions_low_residence_duration_metric_threshold = {
            let loaded_tenant_conf = tenant_conf.load();
            Self::get_evictions_low_residence_duration_metric_threshold(
                &loaded_tenant_conf.tenant_conf,
                &conf.default_tenant_conf,
            )
        };

        if let Some(ancestor) = &ancestor {
            let mut ancestor_gc_info = ancestor.gc_info.write().unwrap();
            // If we construct an explicit timeline object, it's obviously not offloaded
            let is_offloaded = MaybeOffloaded::No;
            ancestor_gc_info.insert_child(timeline_id, metadata.ancestor_lsn(), is_offloaded);
        }

        let relsize_snapshot_cache_capacity = {
            let loaded_tenant_conf = tenant_conf.load();
            loaded_tenant_conf
                .tenant_conf
                .relsize_snapshot_cache_capacity
                .unwrap_or(conf.default_tenant_conf.relsize_snapshot_cache_capacity)
        };

        // Build the row-per-page writer if both PageStores are
        // configured (yb-pageserver mode). Upstream pageserver builds
        // a Timeline with both = None. PageWriter routes reads through
        // `compute_page_store` and writes through `internal_page_store`
        // so compute getPage stays off the walreceiver/materialiser
        // write conn.
        let (page_writer, page_writer_flush_task, image_materializer_task) =
            match (
                resources.compute_page_store.as_ref(),
                resources.internal_page_store.as_ref(),
            ) {
                (Some(compute_ps), Some(internal_ps)) => {
                    let pw_cfg = &conf.yb_page_writer;
                    let mut writer = crate::page_writer::PageWriter::new(
                        tenant_shard_id.tenant_id,
                        timeline_id,
                        compute_ps.clone(),
                        internal_ps.clone(),
                        crate::page_writer::FlushPolicy {
                            max_age: pw_cfg.flush_interval,
                            max_bytes: pw_cfg.flush_size_bytes,
                        },
                        pw_cfg.read_cache_bytes,
                    );

                    // Materialiser channel: established BEFORE we wrap
                    // the writer in an Arc so we can set_materializer
                    // on `&mut writer`. The spawn happens AFTER the
                    // Arc exists because the materialiser needs to
                    // hold its own Arc<PageWriter>.
                    // Either-threshold trigger: count OR payload bytes,
                    // whichever lands first. Both zero disables.
                    let mat_enabled = (pw_cfg.image_chain_limit > 0
                        || pw_cfg.image_chain_payload_bytes_limit > 0)
                        && walredo_mgr.is_some();
                    let mat_channel = if mat_enabled {
                        let (tx, rx) = tokio::sync::mpsc::channel(
                            pw_cfg.materialize_queue_capacity.max(1),
                        );
                        let in_flight = std::sync::Arc::new(std::sync::Mutex::new(
                            std::collections::HashSet::new(),
                        ));
                        writer.set_materializer(crate::page_writer::MaterializerHandle {
                            count_limit: pw_cfg.image_chain_limit,
                            payload_bytes_limit: pw_cfg.image_chain_payload_bytes_limit,
                            tx,
                            in_flight: in_flight.clone(),
                        });
                        Some((rx, in_flight))
                    } else {
                        None
                    };

                    let writer = Arc::new(writer);
                    let flush_task = writer.spawn_flusher(pw_cfg.flush_poll_interval);

                    let materializer_task = match (mat_channel, walredo_mgr.clone()) {
                        (Some((rx, in_flight)), Some(wr)) => {
                            let pg_major = postgres_ffi::PgMajorVersion::try_from(pg_version)
                                .expect("invalid pg_version in timeline init");
                            Some(crate::image_materializer::ImageMaterializer::spawn(
                                tenant_shard_id.tenant_id,
                                timeline_id,
                                writer.clone(),
                                internal_ps.clone(),
                                wr,
                                pg_major,
                                rx,
                                in_flight,
                            ))
                        }
                        _ => None,
                    };

                    (Some(writer), Some(flush_task), materializer_task)
                }
                _ => (None, None, None),
            };

        Arc::new_cyclic(|myself| {
            let metrics = Arc::new(TimelineMetrics::new(
                &tenant_shard_id,
                &timeline_id,
                crate::metrics::EvictionsWithLowResidenceDurationBuilder::new(
                    "mtime",
                    evictions_low_residence_duration_metric_threshold,
                ),
            ));
            let aux_file_metrics = metrics.aux_file_size_gauge.clone();

            let result = Timeline {
                conf,
                tenant_conf,
                myself: myself.clone(),
                timeline_id,
                tenant_shard_id,
                generation,
                shard_identity,
                pg_version,
                layers: Default::default(),

                walredo_mgr,
                walreceiver: Mutex::new(None),

                remote_client: Arc::new(resources.remote_client),

                page_writer: page_writer.clone(),
                page_writer_flush_task: std::sync::Mutex::new(page_writer_flush_task),
                image_materializer_task: std::sync::Mutex::new(image_materializer_task),

                // initialize in-memory 'last_record_lsn' from 'disk_consistent_lsn'.
                last_record_lsn: SeqWait::new(RecordLsn {
                    last: disk_consistent_lsn,
                    prev: metadata.prev_record_lsn().unwrap_or(Lsn(0)),
                }),
                disk_consistent_lsn: AtomicLsn::new(disk_consistent_lsn.0),

                gc_compaction_state: ArcSwapOption::from_pointee(gc_compaction_state),

                ancestor_timeline: ancestor,
                ancestor_lsn: metadata.ancestor_lsn(),

                metrics,

                query_metrics: crate::metrics::SmgrQueryTimePerTimeline::new(
                    &tenant_shard_id,
                    &timeline_id,
                    resources.pagestream_throttle_metrics,
                ),

                directory_metrics: array::from_fn(|_| AtomicU64::new(0)),
                directory_metrics_inited: array::from_fn(|_| AtomicBool::new(false)),

                write_lock: tokio::sync::Mutex::new(None),

                gc_info: std::sync::RwLock::new(GcInfo::default()),

                applied_gc_cutoff_lsn: Rcu::new(metadata.latest_gc_cutoff_lsn()),
                initdb_lsn: metadata.initdb_lsn(),

                current_logical_size: if disk_consistent_lsn.is_valid() {
                    // we're creating timeline data with some layer files existing locally,
                    // need to recalculate timeline's logical size based on data in the layers.
                    LogicalSize::deferred_initial(disk_consistent_lsn)
                } else {
                    // we're creating timeline data without any layers existing locally,
                    // initial logical size is 0.
                    LogicalSize::empty_initial()
                },

                last_received_wal: Mutex::new(None),
                rel_size_latest_cache: RwLock::new(HashMap::new()),
                rel_size_snapshot_cache: Mutex::new(LruCache::new(relsize_snapshot_cache_capacity)),

                download_all_remote_layers_task_info: RwLock::new(None),

                state,

                delete_progress: TimelineDeleteProgress::default(),

                cancel,
                gate: Gate::default(),

                compaction_lock: tokio::sync::Mutex::default(),
                corruption_detected: AtomicBool::default(),
                gc_lock: tokio::sync::Mutex::default(),

                standby_horizon: AtomicLsn::new(0),

                pagestream_throttle: resources.pagestream_throttle,

                aux_file_size_estimator: AuxFileSizeEstimator::new(aux_file_metrics),

                handles: Default::default(),

                attach_wal_lag_cooldown,

                create_idempotency,

                page_trace: Default::default(),

                rel_size_v2_status: ArcSwap::from_pointee((
                    rel_size_v2_status,
                    rel_size_migrated_at,
                )),

                wait_lsn_log_slow: tokio::sync::Semaphore::new(1),

                basebackup_cache: resources.basebackup_cache,

                db_rel_count: ArcSwapOption::from_pointee(None),
            };

            result
                .metrics
                .last_record_lsn_gauge
                .set(disk_consistent_lsn.0 as i64);
            result
        })
    }

    /// yb-mode: no-op. The legacy Neon flush_loop only runs to
    /// freeze InMemoryLayers into image / delta layer files; ybLight
    /// never opens an InMemoryLayer (writes route through PageWriter
    /// into YB rows), so spawning the loop is wasted. Kept as a stub
    /// so callers in tenant.rs / uninit.rs don't have to change.
    pub(super) fn maybe_spawn_flush_loop(self: &Arc<Self>) {}

    pub(crate) fn update_gc_compaction_state(
        &self,
        gc_compaction_state: GcCompactionState,
    ) -> anyhow::Result<()> {
        self.gc_compaction_state
            .store(Some(Arc::new(gc_compaction_state.clone())));
        self.remote_client
            .schedule_index_upload_for_gc_compaction_state_update(gc_compaction_state)
    }

    pub(crate) fn update_rel_size_v2_status(
        &self,
        rel_size_v2_status: RelSizeMigration,
        rel_size_migrated_at: Option<Lsn>,
    ) -> anyhow::Result<()> {
        self.rel_size_v2_status.store(Arc::new((
            Some(rel_size_v2_status.clone()),
            rel_size_migrated_at,
        )));
        self.remote_client
            .schedule_index_upload_for_rel_size_v2_status_update(
                rel_size_v2_status,
                rel_size_migrated_at,
            )
    }

    /// Creates and starts the wal receiver.
    ///
    /// This function is expected to be called at most once per Timeline's lifecycle
    /// when the timeline is activated.
    fn launch_wal_receiver(
        self: &Arc<Self>,
        ctx: &RequestContext,
        broker_client: BrokerClientChannel,
    ) {
        info!(
            "launching WAL receiver for timeline {} of tenant {}",
            self.timeline_id, self.tenant_shard_id
        );

        let tenant_conf = self.tenant_conf.load();
        let wal_connect_timeout = tenant_conf
            .tenant_conf
            .walreceiver_connect_timeout
            .unwrap_or(self.conf.default_tenant_conf.walreceiver_connect_timeout);
        let lagging_wal_timeout = tenant_conf
            .tenant_conf
            .lagging_wal_timeout
            .unwrap_or(self.conf.default_tenant_conf.lagging_wal_timeout);
        let max_lsn_wal_lag = tenant_conf
            .tenant_conf
            .max_lsn_wal_lag
            .unwrap_or(self.conf.default_tenant_conf.max_lsn_wal_lag);

        let mut guard = self.walreceiver.lock().unwrap();
        assert!(
            guard.is_none(),
            "multiple launches / re-launches of WAL receiver are not supported"
        );

        let protocol = PostgresClientProtocol::Interpreted {
            format: utils::postgres_client::InterpretedFormat::Protobuf,
            compression: Some(utils::postgres_client::Compression::Zstd { level: 1 }),
        };

        *guard = Some(WalReceiver::start(
            Arc::clone(self),
            WalReceiverConf {
                protocol,
                wal_connect_timeout,
                lagging_wal_timeout,
                max_lsn_wal_lag,
                auth_token: crate::config::SAFEKEEPER_AUTH_TOKEN.get().cloned(),
                availability_zone: self.conf.availability_zone.clone(),
                ingest_batch_size: self.conf.ingest_batch_size,
                validate_wal_contiguity: self.conf.validate_wal_contiguity,
            },
            broker_client,
            ctx,
        ));
    }

    /// Initialize with an empty layer map. Used when creating a new timeline.
    pub(super) fn init_empty_layer_map(&self, start_lsn: Lsn) {
        let mut layers = self.layers.try_write(LayerManagerLockHolder::Init).expect(
            "in the context where we call this function, no other task has access to the object",
        );
        layers
            .open_mut()
            .expect("in this context the LayerManager must still be open")
            .initialize_empty(Lsn(start_lsn.0));
    }

    /// Retrieve current logical size of the timeline.
    ///
    /// The size could be lagging behind the actual number, in case
    /// the initial size calculation has not been run (gets triggered on the first size access).
    ///
    /// return size and boolean flag that shows if the size is exact
    pub(crate) fn get_current_logical_size(
        self: &Arc<Self>,
        priority: GetLogicalSizePriority,
        ctx: &RequestContext,
    ) -> logical_size::CurrentLogicalSize {
        if !self.tenant_shard_id.is_shard_zero() {
            // Logical size is only accurately maintained on shard zero: when called elsewhere, for example
            // when HTTP API is serving a GET for timeline zero, return zero
            return logical_size::CurrentLogicalSize::Approximate(logical_size::Approximate::zero());
        }

        let current_size = self.current_logical_size.current_size();
        debug!("Current size: {current_size:?}");

        match (current_size.accuracy(), priority) {
            (logical_size::Accuracy::Exact, _) => (), // nothing to do
            (logical_size::Accuracy::Approximate, GetLogicalSizePriority::Background) => {
                // background task will eventually deliver an exact value, we're in no rush
            }
            (logical_size::Accuracy::Approximate, GetLogicalSizePriority::User) => {
                // background task is not ready, but user is asking for it now;
                // => make the background task skip the line
                // (The alternative would be to calculate the size here, but,
                //  it can actually take a long time if the user has a lot of rels.
                //  And we'll inevitable need it again; So, let the background task do the work.)
                match self
                    .current_logical_size
                    .cancel_wait_for_background_loop_concurrency_limit_semaphore
                    .get()
                {
                    Some(cancel) => cancel.cancel(),
                    None => {
                        match self.current_state() {
                            TimelineState::Broken { .. } | TimelineState::Stopping => {
                                // Can happen when timeline detail endpoint is used when deletion is ongoing (or its broken).
                                // Don't make noise.
                            }
                            TimelineState::Loading => {
                                // Import does not return an activated timeline.
                                info!(
                                    "discarding priority boost for logical size calculation because timeline is not yet active"
                                );
                            }
                            TimelineState::Active => {
                                // activation should be setting the once cell
                                warn!(
                                    "unexpected: cancel_wait_for_background_loop_concurrency_limit_semaphore not set, priority-boosting of logical size calculation will not work"
                                );
                                debug_assert!(false);
                            }
                        }
                    }
                }
            }
        }

        if let CurrentLogicalSize::Approximate(_) = &current_size {
            if ctx.task_kind() == TaskKind::WalReceiverConnectionHandler {
                let first = self
                    .current_logical_size
                    .did_return_approximate_to_walreceiver
                    .compare_exchange(
                        false,
                        true,
                        AtomicOrdering::Relaxed,
                        AtomicOrdering::Relaxed,
                    )
                    .is_ok();
                if first {
                    crate::metrics::initial_logical_size::TIMELINES_WHERE_WALRECEIVER_GOT_APPROXIMATE_SIZE.inc();
                }
            }
        }

        current_size
    }

    fn spawn_initial_logical_size_computation_task(self: &Arc<Self>, ctx: &RequestContext) {
        let Some(initial_part_end) = self.current_logical_size.initial_part_end else {
            // nothing to do for freshly created timelines;
            assert_eq!(
                self.current_logical_size.current_size().accuracy(),
                logical_size::Accuracy::Exact,
            );
            self.current_logical_size.initialized.add_permits(1);
            return;
        };

        let cancel_wait_for_background_loop_concurrency_limit_semaphore = CancellationToken::new();
        let token = cancel_wait_for_background_loop_concurrency_limit_semaphore.clone();
        self.current_logical_size
            .cancel_wait_for_background_loop_concurrency_limit_semaphore.set(token)
            .expect("initial logical size calculation task must be spawned exactly once per Timeline object");

        let self_clone = Arc::clone(self);
        let background_ctx = ctx.detached_child(
            TaskKind::InitialLogicalSizeCalculation,
            DownloadBehavior::Download,
        );
        task_mgr::spawn(
            task_mgr::BACKGROUND_RUNTIME.handle(),
            task_mgr::TaskKind::InitialLogicalSizeCalculation,
            self.tenant_shard_id,
            Some(self.timeline_id),
            "initial size calculation",
            // NB: don't log errors here, task_mgr will do that.
            async move {
                self_clone
                    .initial_logical_size_calculation_task(
                        initial_part_end,
                        cancel_wait_for_background_loop_concurrency_limit_semaphore,
                        background_ctx,
                    )
                    .await;
                Ok(())
            }
            .instrument(info_span!(parent: None, "initial_size_calculation", tenant_id=%self.tenant_shard_id.tenant_id, shard_id=%self.tenant_shard_id.shard_slug(), timeline_id=%self.timeline_id)),
        );
    }

    /// # Cancellation
    ///
    /// This method is sensitive to `Timeline::cancel`.
    ///
    /// It is _not_ sensitive to task_mgr::shutdown_token().
    ///
    /// # Cancel-Safety
    ///
    /// It does Timeline IO, hence this should be polled to completion because
    /// we could be leaving in-flight IOs behind, which is safe, but annoying
    /// to reason about.
    async fn initial_logical_size_calculation_task(
        self: Arc<Self>,
        initial_part_end: Lsn,
        skip_concurrency_limiter: CancellationToken,
        background_ctx: RequestContext,
    ) {
        scopeguard::defer! {
            // Irrespective of the outcome of this operation, we should unblock anyone waiting for it.
            self.current_logical_size.initialized.add_permits(1);
        }

        let try_once = |attempt: usize| {
            let background_ctx = &background_ctx;
            let self_ref = &self;
            let skip_concurrency_limiter = &skip_concurrency_limiter;
            async move {
                let wait_for_permit = super::tasks::acquire_concurrency_permit(
                    BackgroundLoopKind::InitialLogicalSizeCalculation,
                    background_ctx,
                );

                use crate::metrics::initial_logical_size::StartCircumstances;
                let (_maybe_permit, circumstances) = tokio::select! {
                    permit = wait_for_permit => {
                        (Some(permit), StartCircumstances::AfterBackgroundTasksRateLimit)
                    }
                    _ = self_ref.cancel.cancelled() => {
                        return Err(CalculateLogicalSizeError::Cancelled);
                    }
                    () = skip_concurrency_limiter.cancelled() => {
                        // Some action that is part of a end user interaction requested logical size
                        // => break out of the rate limit
                        // TODO: ideally we'd not run on BackgroundRuntime but the requester's runtime;
                        // but then again what happens if they cancel; also, we should just be using
                        // one runtime across the entire process, so, let's leave this for now.
                        (None, StartCircumstances::SkippedConcurrencyLimiter)
                    }
                };

                let metrics_guard = if attempt == 1 {
                    crate::metrics::initial_logical_size::START_CALCULATION.first(circumstances)
                } else {
                    crate::metrics::initial_logical_size::START_CALCULATION.retry(circumstances)
                };

                let io_concurrency = IoConcurrency::spawn_from_conf(
                    self_ref.conf.get_vectored_concurrent_io,
                    self_ref
                        .gate
                        .enter()
                        .map_err(|_| CalculateLogicalSizeError::Cancelled)?,
                );

                let calculated_size = self_ref
                    .logical_size_calculation_task(
                        initial_part_end,
                        LogicalSizeCalculationCause::Initial,
                        background_ctx,
                    )
                    .await?;

                self_ref
                    .trigger_aux_file_size_computation(
                        initial_part_end,
                        background_ctx,
                        io_concurrency,
                    )
                    .await?;

                // TODO: add aux file size to logical size

                Ok((calculated_size, metrics_guard))
            }
        };

        let retrying = async {
            let mut attempt = 0;
            loop {
                attempt += 1;

                match try_once(attempt).await {
                    Ok(res) => return ControlFlow::Continue(res),
                    Err(CalculateLogicalSizeError::Cancelled) => return ControlFlow::Break(()),
                    Err(
                        e @ (CalculateLogicalSizeError::Decode(_)
                        | CalculateLogicalSizeError::PageRead(_)),
                    ) => {
                        warn!(attempt, "initial size calculation failed: {e:?}");
                        // exponential back-off doesn't make sense at these long intervals;
                        // use fixed retry interval with generous jitter instead
                        let sleep_duration = Duration::from_secs(
                            u64::try_from(
                                // 1hour base
                                (60_i64 * 60_i64)
                                    // 10min jitter
                                    + rand::rng().random_range(-10 * 60..10 * 60),
                            )
                            .expect("10min < 1hour"),
                        );
                        tokio::select! {
                            _ = tokio::time::sleep(sleep_duration) => {}
                            _ = self.cancel.cancelled() => return ControlFlow::Break(()),
                        }
                    }
                }
            }
        };

        let (calculated_size, metrics_guard) = match retrying.await {
            ControlFlow::Continue(calculated_size) => calculated_size,
            ControlFlow::Break(()) => return,
        };

        // we cannot query current_logical_size.current_size() to know the current
        // *negative* value, only truncated to u64.
        let added = self
            .current_logical_size
            .size_added_after_initial
            .load(AtomicOrdering::Relaxed);

        let sum = calculated_size.saturating_add_signed(added);

        // set the gauge value before it can be set in `update_current_logical_size`.
        self.metrics.current_logical_size_gauge.set(sum);

        self.current_logical_size
            .initial_logical_size
            .set((calculated_size, metrics_guard.calculation_result_saved()))
            .ok()
            .expect("only this task sets it");
    }


    #[instrument(skip_all)]
    async fn logical_size_calculation_task(
        self: &Arc<Self>,
        lsn: Lsn,
        cause: LogicalSizeCalculationCause,
        ctx: &RequestContext,
    ) -> Result<u64, CalculateLogicalSizeError> {
        crate::span::debug_assert_current_span_has_tenant_and_timeline_id();
        // We should never be calculating logical sizes on shard !=0, because these shards do not have
        // accurate relation sizes, and they do not emit consumption metrics.
        debug_assert!(self.tenant_shard_id.is_shard_zero());

        let guard = self
            .gate
            .enter()
            .map_err(|_| CalculateLogicalSizeError::Cancelled)?;

        self.calculate_logical_size(lsn, cause, &guard, ctx).await
    }

    /// Calculate the logical size of the database at the latest LSN.
    ///
    /// NOTE: counted incrementally, includes ancestors. This can be a slow operation,
    /// especially if we need to download remote layers.
    async fn calculate_logical_size(
        &self,
        up_to_lsn: Lsn,
        cause: LogicalSizeCalculationCause,
        _guard: &GateGuard,
        ctx: &RequestContext,
    ) -> Result<u64, CalculateLogicalSizeError> {
        info!(
            "Calculating logical size for timeline {} at {}",
            self.timeline_id, up_to_lsn
        );

        if let Err(()) = pausable_failpoint!("timeline-calculate-logical-size-pause", &self.cancel)
        {
            return Err(CalculateLogicalSizeError::Cancelled);
        }

        // See if we've already done the work for initial size calculation.
        // This is a short-cut for timelines that are mostly unused.
        if let Some(size) = self.current_logical_size.initialized_size(up_to_lsn) {
            return Ok(size);
        }
        let storage_time_metrics = match cause {
            LogicalSizeCalculationCause::Initial => &self.metrics.logical_size_histo,
        };
        let timer = storage_time_metrics.start_timer();
        let logical_size = self
            .get_current_logical_size_non_incremental(up_to_lsn, ctx)
            .await?;
        debug!("calculated logical size: {logical_size}");
        timer.stop_and_record();
        Ok(logical_size)
    }

    /// Update current logical size, adding `delta' to the old value.
    fn update_current_logical_size(&self, delta: i64) {
        let logical_size = &self.current_logical_size;
        logical_size.increment_size(delta);

        // Also set the value in the prometheus gauge. Note that
        // there is a race condition here: if this is is called by two
        // threads concurrently, the prometheus gauge might be set to
        // one value while current_logical_size is set to the
        // other.
        match logical_size.current_size() {
            CurrentLogicalSize::Exact(ref new_current_size) => self
                .metrics
                .current_logical_size_gauge
                .set(new_current_size.into()),
            CurrentLogicalSize::Approximate(_) => {
                // don't update the gauge yet, this allows us not to update the gauge back and
                // forth between the initial size calculation task.
            }
        }
    }

    pub(crate) fn update_directory_entries_count(&self, kind: DirectoryKind, count: MetricsUpdate) {
        // TODO: this directory metrics is not correct -- we could have multiple reldirs in the system
        // for each of the database, but we only store one value, and therefore each pgdirmodification
        // would overwrite the previous value if they modify different databases.

        match count {
            MetricsUpdate::Set(count) => {
                self.directory_metrics[kind.offset()].store(count, AtomicOrdering::Relaxed);
                self.directory_metrics_inited[kind.offset()].store(true, AtomicOrdering::Relaxed);
            }
            MetricsUpdate::Add(count) => {
                // TODO: these operations are not atomic; but we only have one writer to the metrics, so
                // it's fine.
                if self.directory_metrics_inited[kind.offset()].load(AtomicOrdering::Relaxed) {
                    // The metrics has been initialized with `MetricsUpdate::Set` before, so we can add/sub
                    // the value reliably.
                    self.directory_metrics[kind.offset()].fetch_add(count, AtomicOrdering::Relaxed);
                }
                // Otherwise, ignore this update
            }
            MetricsUpdate::Sub(count) => {
                // TODO: these operations are not atomic; but we only have one writer to the metrics, so
                // it's fine.
                if self.directory_metrics_inited[kind.offset()].load(AtomicOrdering::Relaxed) {
                    // The metrics has been initialized with `MetricsUpdate::Set` before.
                    // The operation could overflow so we need to normalize the value.
                    let prev_val =
                        self.directory_metrics[kind.offset()].load(AtomicOrdering::Relaxed);
                    let res = prev_val.saturating_sub(count);
                    self.directory_metrics[kind.offset()].store(res, AtomicOrdering::Relaxed);
                }
                // Otherwise, ignore this update
            }
        };

        // TODO: remove this, there's no place in the code that updates this aux metrics.
        let aux_metric =
            self.directory_metrics[DirectoryKind::AuxFiles.offset()].load(AtomicOrdering::Relaxed);

        let sum_of_entries = self
            .directory_metrics
            .iter()
            .map(|v| v.load(AtomicOrdering::Relaxed))
            .sum();
        // Set a high general threshold and a lower threshold for the auxiliary files,
        // as we can have large numbers of relations in the db directory.
        const SUM_THRESHOLD: u64 = 5000;
        const AUX_THRESHOLD: u64 = 1000;
        if sum_of_entries >= SUM_THRESHOLD || aux_metric >= AUX_THRESHOLD {
            self.metrics
                .directory_entries_count_gauge
                .set(sum_of_entries);
        } else if let Some(metric) = Lazy::get(&self.metrics.directory_entries_count_gauge) {
            metric.set(sum_of_entries);
        }
    }


    /// Returns true if the given lsn is or was an ancestor branchpoint.
    pub(crate) fn is_ancestor_lsn(&self, lsn: Lsn) -> bool {
        // upon timeline detach, we set the ancestor_lsn to Lsn::INVALID and the store the original
        // branchpoint in the value in IndexPart::lineage
        self.ancestor_lsn == lsn
            || (self.ancestor_lsn == Lsn::INVALID
                && self.remote_client.is_previous_ancestor_lsn(lsn))
    }
}

#[derive(Clone)]
/// Type representing a query in the ([`Lsn`], [`Key`]) space.
/// In other words, a set of segments in a 2D space.
///
/// This representation has the advatange of avoiding hash map
/// allocations for uniform queries.
pub(crate) enum VersionedKeySpaceQuery {
    /// Variant for queries at a single [`Lsn`]
    Uniform { keyspace: KeySpace, lsn: Lsn },
    /// Variant for queries at multiple [`Lsn`]s
    Scattered {
        keyspaces_at_lsn: Vec<(Lsn, KeySpace)>,
    },
}

impl VersionedKeySpaceQuery {
    pub(crate) fn uniform(keyspace: KeySpace, lsn: Lsn) -> Self {
        Self::Uniform { keyspace, lsn }
    }

    pub(crate) fn scattered(keyspaces_at_lsn: Vec<(Lsn, KeySpace)>) -> Self {
        Self::Scattered { keyspaces_at_lsn }
    }

    /// Returns the most recent (largest) LSN included in the query.
    /// If any of the LSNs included in the query are invalid, returns
    /// an error instead.
    fn high_watermark_lsn(&self) -> Result<Lsn, GetVectoredError> {
        match self {
            Self::Uniform { lsn, .. } => {
                if !lsn.is_valid() {
                    return Err(GetVectoredError::InvalidLsn(*lsn));
                }

                Ok(*lsn)
            }
            Self::Scattered { keyspaces_at_lsn } => {
                let mut max_lsn = None;
                for (lsn, _keyspace) in keyspaces_at_lsn.iter() {
                    if !lsn.is_valid() {
                        return Err(GetVectoredError::InvalidLsn(*lsn));
                    }
                    max_lsn = std::cmp::max(max_lsn, Some(lsn));
                }

                if let Some(computed) = max_lsn {
                    Ok(*computed)
                } else {
                    Err(GetVectoredError::Other(anyhow!("empty input")))
                }
            }
        }
    }

    /// Returns the total keyspace being queried: the result of projecting
    /// everything in the key dimensions onto the key axis.
    fn total_keyspace(&self) -> KeySpace {
        match self {
            Self::Uniform { keyspace, .. } => keyspace.clone(),
            Self::Scattered { keyspaces_at_lsn } => keyspaces_at_lsn
                .iter()
                .map(|(_lsn, keyspace)| keyspace)
                .fold(KeySpace::default(), |mut acc, v| {
                    acc.merge(v);
                    acc
                }),
        }
    }

    /// Returns LSN for a specific key.
    ///
    /// Invariant: requested key must be part of [`Self::total_keyspace`]
    pub(super) fn map_key_to_lsn(&self, key: &Key) -> Lsn {
        match self {
            Self::Uniform { lsn, .. } => *lsn,
            Self::Scattered { keyspaces_at_lsn } => {
                keyspaces_at_lsn
                    .iter()
                    .find(|(_lsn, keyspace)| keyspace.contains(key))
                    .expect("Returned key was requested")
                    .0
            }
        }
    }

    fn is_empty(&self) -> bool {
        match self {
            Self::Uniform { keyspace, .. } => keyspace.is_empty(),
            Self::Scattered { keyspaces_at_lsn } => keyspaces_at_lsn
                .iter()
                .all(|(_lsn, keyspace)| keyspace.is_empty()),
        }
    }
}

impl std::fmt::Display for VersionedKeySpaceQuery {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "[")?;

        match self {
            VersionedKeySpaceQuery::Uniform { keyspace, lsn } => {
                write!(f, "{keyspace} @ {lsn}")?;
            }
            VersionedKeySpaceQuery::Scattered { keyspaces_at_lsn } => {
                for (lsn, keyspace) in keyspaces_at_lsn.iter() {
                    write!(f, "{keyspace} @ {lsn},")?;
                }
            }
        }

        write!(f, "]")
    }
}

impl Timeline {
    pub(crate) fn get_shard_identity(&self) -> &ShardIdentity {
        &self.shard_identity
    }

    #[inline(always)]
    pub(crate) fn shard_timeline_id(&self) -> ShardTimelineId {
        ShardTimelineId {
            shard_index: ShardIndex {
                shard_number: self.shard_identity.number,
                shard_count: self.shard_identity.count,
            },
            timeline_id: self.timeline_id,
        }
    }

    pub(crate) fn finish_write(&self, new_lsn: Lsn) {
        assert!(new_lsn.is_aligned());

        self.metrics.last_record_lsn_gauge.set(new_lsn.0 as i64);
        self.last_record_lsn.advance(new_lsn);
    }

    pub(crate) async fn preserve_initdb_archive(&self) -> anyhow::Result<()> {
        self.remote_client
            .preserve_initdb_archive(
                &self.tenant_shard_id.tenant_id,
                &self.timeline_id,
                &self.cancel,
            )
            .await
    }

    /// Wait until the background initial logical size calculation is complete, or
    /// this Timeline is shut down.  Calling this function will cause the initial
    /// logical size calculation to skip waiting for the background jobs barrier.
    pub(crate) async fn await_initial_logical_size(self: Arc<Self>) {
        if !self.shard_identity.is_shard_zero() {
            // We don't populate logical size on shard >0: skip waiting for it.
            return;
        }

        if self.remote_client.is_deleting() {
            // The timeline was created in a deletion-resume state, we don't expect logical size to be populated
            return;
        }

        if self.current_logical_size.current_size().is_exact() {
            // root timelines are initialized with exact count, but never start the background
            // calculation
            return;
        }

        if self.cancel.is_cancelled() {
            // We already requested stopping the tenant, so we cannot wait for the logical size
            // calculation to complete given the task might have been already cancelled.
            return;
        }

        if let Some(await_bg_cancel) = self
            .current_logical_size
            .cancel_wait_for_background_loop_concurrency_limit_semaphore
            .get()
        {
            await_bg_cancel.cancel();
        } else {
            // We should not wait if we were not able to explicitly instruct
            // the logical size cancellation to skip the concurrency limit semaphore.
            // TODO: this is an unexpected case.  We should restructure so that it
            // can't happen.
            tracing::warn!(
                "await_initial_logical_size: can't get semaphore cancel token, skipping"
            );
            debug_assert!(false);
        }

        tokio::select!(
            _ = self.current_logical_size.initialized.acquire() => {},
            _ = self.cancel.cancelled() => {}
        )
    }

}

impl Drop for Timeline {
    fn drop(&mut self) {
        if let Some(ancestor) = &self.ancestor_timeline {
            // This lock should never be poisoned, but in case it is we do a .map() instead of
            // an unwrap(), to avoid panicking in a destructor and thereby aborting the process.
            if let Ok(mut gc_info) = ancestor.gc_info.write() {
                if !gc_info.remove_child_not_offloaded(self.timeline_id) {
                    tracing::error!(tenant_id = %self.tenant_shard_id.tenant_id, shard_id = %self.tenant_shard_id.shard_slug(), timeline_id = %self.timeline_id,
                        "Couldn't remove retain_lsn entry from timeline's parent on drop: already removed");
                }
            }
        }
        info!(
            "Timeline {} for tenant {} is being dropped",
            self.timeline_id, self.tenant_shard_id.tenant_id
        );
    }
}

/// Descriptor for a delta layer used in testing infra. The start/end key/lsn range of the
/// delta layer might be different from the min/max key/lsn in the delta layer. Therefore,
/// the layer descriptor requires the user to provide the ranges, which should cover all
/// keys specified in the `data` field.

impl Timeline {
    async fn find_gc_time_cutoff(
        &self,
        now: SystemTime,
        pitr: Duration,
        cancel: &CancellationToken,
        ctx: &RequestContext,
    ) -> Result<Option<Lsn>, PageReconstructError> {
        debug_assert_current_span_has_tenant_and_timeline_id();
        if self.shard_identity.is_shard_zero() {
            // Shard Zero has SLRU data and can calculate the PITR time -> LSN mapping itself
            let time_range = if pitr == Duration::ZERO {
                humantime::parse_duration(DEFAULT_PITR_INTERVAL).expect("constant is invalid")
            } else {
                pitr
            };

            // If PITR is so large or `now` is so small that this underflows, we will retain no history (highly unexpected case)
            let time_cutoff = now.checked_sub(time_range).unwrap_or(now);
            let timestamp = to_pg_timestamp(time_cutoff);

            let time_cutoff = match self.find_lsn_for_timestamp(timestamp, cancel, ctx).await? {
                LsnForTimestamp::Present(lsn) => Some(lsn),
                LsnForTimestamp::Future(lsn) => {
                    // The timestamp is in the future. That sounds impossible,
                    // but what it really means is that there hasn't been
                    // any commits since the cutoff timestamp.
                    //
                    // In this case we should use the LSN of the most recent commit,
                    // which is implicitly the last LSN in the log.
                    debug!("future({})", lsn);
                    Some(self.get_last_record_lsn())
                }
                LsnForTimestamp::Past(lsn) => {
                    debug!("past({})", lsn);
                    None
                }
                LsnForTimestamp::NoData(lsn) => {
                    debug!("nodata({})", lsn);
                    None
                }
            };
            Ok(time_cutoff)
        } else {
            // Shards other than shard zero cannot do timestamp->lsn lookups, and must instead learn their GC cutoff
            // from shard zero's index.  The index doesn't explicitly tell us the time cutoff, but we may assume that
            // the point up to which shard zero's last_gc_cutoff has advanced will either be the time cutoff, or a
            // space cutoff that we would also have respected ourselves.
            match self
                .remote_client
                .download_foreign_index(ShardNumber(0), cancel)
                .await
            {
                Ok((index_part, index_generation, _index_mtime)) => {
                    tracing::info!(
                        "GC loaded shard zero metadata (gen {index_generation:?}): latest_gc_cutoff_lsn: {}",
                        index_part.metadata.latest_gc_cutoff_lsn()
                    );
                    Ok(Some(index_part.metadata.latest_gc_cutoff_lsn()))
                }
                Err(DownloadError::NotFound) => {
                    // This is unexpected, because during timeline creations shard zero persists to remote
                    // storage before other shards are called, and during timeline deletion non-zeroth shards are
                    // deleted before the zeroth one.  However, it should be harmless: if we somehow end up in this
                    // state, then shard zero should _eventually_ write an index when it GCs.
                    tracing::warn!("GC couldn't find shard zero's index for timeline");
                    Ok(None)
                }
                Err(e) => {
                    // TODO: this function should return a different error type than page reconstruct error
                    Err(PageReconstructError::Other(anyhow::anyhow!(e)))
                }
            }

            // TODO: after reading shard zero's GC cutoff, we should validate its generation with the storage
            // controller.  Otherwise, it is possible that we see the GC cutoff go backwards while shard zero
            // is going through a migration if we read the old location's index and it has GC'd ahead of the
            // new location.  This is legal in principle, but problematic in practice because it might result
            // in a timeline creation succeeding on shard zero ('s new location) but then failing on other shards
            // because they have GC'd past the branch point.
        }
    }

    /// Find the Lsns above which layer files need to be retained on
    /// garbage collection.
    ///
    /// We calculate two cutoffs, one based on time and one based on WAL size.  `pitr`
    /// controls the time cutoff (or ZERO to disable time-based retention), and `space_cutoff` controls
    /// the space-based retention.
    ///
    /// This function doesn't simply to calculate time & space based retention: it treats time-based
    /// retention as authoritative if enabled, and falls back to space-based retention if calculating
    /// the LSN for a time point isn't possible.  Therefore the GcCutoffs::horizon in the response might
    /// be different to the `space_cutoff` input.  Callers should treat the min() of the two cutoffs
    /// in the response as the GC cutoff point for the timeline.
    #[instrument(skip_all, fields(timeline_id=%self.timeline_id))]
    pub(super) async fn find_gc_cutoffs(
        &self,
        now: SystemTime,
        space_cutoff: Lsn,
        pitr: Duration,
        cancel: &CancellationToken,
        ctx: &RequestContext,
    ) -> Result<GcCutoffs, PageReconstructError> {
        let _timer = self
            .metrics
            .find_gc_cutoffs_histo
            .start_timer()
            .record_on_drop();

        pausable_failpoint!("Timeline::find_gc_cutoffs-pausable");

        if cfg!(test) && pitr == Duration::ZERO {
            // Unit tests which specify zero PITR interval expect to avoid doing any I/O for timestamp lookup
            return Ok(GcCutoffs {
                time: Some(self.get_last_record_lsn()),
                space: space_cutoff,
            });
        }

        // Calculate a time-based limit on how much to retain:
        // - if PITR interval is set, then this is our cutoff.
        // - if PITR interval is not set, then we do a lookup
        //   based on DEFAULT_PITR_INTERVAL, so that size-based retention does not result in keeping history around permanently on idle databases.
        let time_cutoff = self.find_gc_time_cutoff(now, pitr, cancel, ctx).await?;

        Ok(match (pitr, time_cutoff) {
            (Duration::ZERO, Some(time_cutoff)) => {
                // PITR is not set. Retain the size-based limit, or the default time retention,
                // whichever requires less data.
                GcCutoffs {
                    time: Some(self.get_last_record_lsn()),
                    space: std::cmp::max(time_cutoff, space_cutoff),
                }
            }
            (Duration::ZERO, None) => {
                // PITR is not set, and time lookup failed
                GcCutoffs {
                    time: Some(self.get_last_record_lsn()),
                    space: space_cutoff,
                }
            }
            (_, None) => {
                // PITR interval is set & we didn't look up a timestamp successfully.  Conservatively assume PITR
                // cannot advance beyond what was already GC'd, and respect space-based retention
                GcCutoffs {
                    time: Some(*self.get_applied_gc_cutoff_lsn()),
                    space: space_cutoff,
                }
            }
            (_, Some(time_cutoff)) => {
                // PITR interval is set and we looked up timestamp successfully.  Ignore
                // size based retention and make time cutoff authoritative
                GcCutoffs {
                    time: Some(time_cutoff),
                    space: time_cutoff,
                }
            }
        })
    }
    /// yb-mode: GC of layer files is meaningless; old page versions
    /// are pruned inside the row store by `PageWriter`. Returning an
    /// empty result preserves the gc_loop's telemetry shape.
    pub(super) async fn gc(&self) -> Result<GcResult, GcError> {
        Ok(GcResult::default())
    }

    /// Reconstruct a value, using the given base image and WAL records in 'data'.
    pub(crate) async fn reconstruct_value(
        &self,
        key: Key,
        request_lsn: Lsn,
        mut data: ValueReconstructState,
        redo_attempt_type: RedoAttemptType,
    ) -> Result<Bytes, PageReconstructError> {
        // Perform WAL redo if needed
        data.records.reverse();

        let fire_critical_error = match redo_attempt_type {
            RedoAttemptType::ReadPage => true,
            RedoAttemptType::LegacyCompaction => true,
            RedoAttemptType::GcCompaction => false,
        };

        // If we have a page image, and no WAL, we're all set
        if data.records.is_empty() {
            if let Some((img_lsn, img)) = &data.img {
                trace!(
                    "found page image for key {} at {}, no WAL redo required, req LSN {}",
                    key, img_lsn, request_lsn,
                );
                Ok(img.clone())
            } else {
                Err(PageReconstructError::from(anyhow!(
                    "base image for {key} at {request_lsn} not found"
                )))
            }
        } else {
            // We need to do WAL redo.
            //
            // If we don't have a base image, then the oldest WAL record better initialize
            // the page
            if data.img.is_none() && !data.records.first().unwrap().1.will_init() {
                Err(PageReconstructError::from(anyhow!(
                    "Base image for {} at {} not found, but got {} WAL records",
                    key,
                    request_lsn,
                    data.records.len()
                )))
            } else {
                if data.img.is_some() {
                    trace!(
                        "found {} WAL records and a base image for {} at {}, performing WAL redo",
                        data.records.len(),
                        key,
                        request_lsn
                    );
                } else {
                    trace!(
                        "found {} WAL records that will init the page for {} at {}, performing WAL redo",
                        data.records.len(),
                        key,
                        request_lsn
                    );
                };
                let res = self
                    .walredo_mgr
                    .as_ref()
                    .context("timeline has no walredo manager")
                    .map_err(PageReconstructError::WalRedo)?
                    .request_redo(
                        key,
                        request_lsn,
                        data.img,
                        data.records,
                        self.pg_version,
                        redo_attempt_type,
                    )
                    .await;
                let img = match res {
                    Ok(img) => img,
                    Err(walredo::Error::Cancelled) => return Err(PageReconstructError::Cancelled),
                    Err(walredo::Error::Other(err)) => {
                        if fire_critical_error {
                            critical_timeline!(
                                self.tenant_shard_id,
                                self.timeline_id,
                                Some(&self.corruption_detected),
                                "walredo failure during page reconstruction: {err:?}"
                            );
                        }
                        return Err(PageReconstructError::WalRedo(
                            err.context("reconstruct a page image"),
                        ));
                    }
                };
                Ok(img)
            }
        }
    }

    pub(crate) async fn spawn_download_all_remote_layers(
        self: Arc<Self>,
        request: DownloadRemoteLayersTaskSpawnRequest,
        ctx: &RequestContext,
    ) -> Result<DownloadRemoteLayersTaskInfo, DownloadRemoteLayersTaskInfo> {
        use pageserver_api::models::DownloadRemoteLayersTaskState;

        // this is not really needed anymore; it has tests which really check the return value from
        // http api. it would be better not to maintain this anymore.

        let mut status_guard = self.download_all_remote_layers_task_info.write().unwrap();
        if let Some(st) = &*status_guard {
            match &st.state {
                DownloadRemoteLayersTaskState::Running => {
                    return Err(st.clone());
                }
                DownloadRemoteLayersTaskState::ShutDown
                | DownloadRemoteLayersTaskState::Completed => {
                    *status_guard = None;
                }
            }
        }

        let self_clone = Arc::clone(&self);
        let task_ctx = ctx.detached_child(
            TaskKind::DownloadAllRemoteLayers,
            DownloadBehavior::Download,
        );
        let task_id = task_mgr::spawn(
            task_mgr::BACKGROUND_RUNTIME.handle(),
            task_mgr::TaskKind::DownloadAllRemoteLayers,
            self.tenant_shard_id,
            Some(self.timeline_id),
            "download all remote layers task",
            async move {
                self_clone.download_all_remote_layers(request, &task_ctx).await;
                let mut status_guard = self_clone.download_all_remote_layers_task_info.write().unwrap();
                 match &mut *status_guard {
                    None => {
                        warn!("tasks status is supposed to be Some(), since we are running");
                    }
                    Some(st) => {
                        let exp_task_id = format!("{}", task_mgr::current_task_id().unwrap());
                        if st.task_id != exp_task_id {
                            warn!("task id changed while we were still running, expecting {} but have {}", exp_task_id, st.task_id);
                        } else {
                            st.state = DownloadRemoteLayersTaskState::Completed;
                        }
                    }
                };
                Ok(())
            }
            .instrument(info_span!(parent: None, "download_all_remote_layers", tenant_id = %self.tenant_shard_id.tenant_id, shard_id = %self.tenant_shard_id.shard_slug(), timeline_id = %self.timeline_id))
        );

        let initial_info = DownloadRemoteLayersTaskInfo {
            task_id: format!("{task_id}"),
            state: DownloadRemoteLayersTaskState::Running,
            total_layer_count: 0,
            successful_download_count: 0,
            failed_download_count: 0,
        };
        *status_guard = Some(initial_info.clone());

        Ok(initial_info)
    }

    async fn download_all_remote_layers(
        self: &Arc<Self>,
        _request: DownloadRemoteLayersTaskSpawnRequest,
        _ctx: &RequestContext,
    ) {
        use pageserver_api::models::DownloadRemoteLayersTaskState;
        // yb-mode: no layer files to download. Mark immediately complete.
        let mut st = self.download_all_remote_layers_task_info.write().unwrap();
        if let Some(st) = st.as_mut() {
            st.state = DownloadRemoteLayersTaskState::Completed;
        }
    }

    pub(crate) fn get_download_all_remote_layers_task_info(
        &self,
    ) -> Option<DownloadRemoteLayersTaskInfo> {
        self.download_all_remote_layers_task_info
            .read()
            .unwrap()
            .clone()
    }

    /* BEGIN_HADRON */
    pub(crate) async fn compute_image_consistent_lsn(&self) -> anyhow::Result<Lsn> {
        let guard = self
            .layers
            .read(LayerManagerLockHolder::ComputeImageConsistentLsn)
            .await;
        let layer_map = guard.layer_map()?;
        let disk_consistent_lsn = self.get_disk_consistent_lsn();

        Ok(layer_map.compute_image_consistent_lsn(disk_consistent_lsn))
    }
    /* END_HADRON */
}

impl Timeline {
    pub(crate) fn get_shard_index(&self) -> ShardIndex {
        ShardIndex {
            shard_number: self.tenant_shard_id.shard_number,
            shard_count: self.tenant_shard_id.shard_count,
        }
    }

    /// Persistently blocks gc for `Manual` reason.
    ///
    /// Returns true if no such block existed before, false otherwise.
    pub(crate) async fn block_gc(&self, tenant: &super::TenantShard) -> anyhow::Result<bool> {
        use crate::tenant::remote_timeline_client::index::GcBlockingReason;
        assert_eq!(self.tenant_shard_id, tenant.tenant_shard_id);
        tenant.gc_block.insert(self, GcBlockingReason::Manual).await
    }

    /// Persistently unblocks gc for `Manual` reason.
    pub(crate) async fn unblock_gc(&self, tenant: &super::TenantShard) -> anyhow::Result<()> {
        use crate::tenant::remote_timeline_client::index::GcBlockingReason;
        assert_eq!(self.tenant_shard_id, tenant.tenant_shard_id);
        tenant.gc_block.remove(self, GcBlockingReason::Manual).await
    }

    #[cfg(test)]
    pub(super) fn force_advance_lsn(self: &Arc<Timeline>, new_lsn: Lsn) {
        self.last_record_lsn.advance(new_lsn);
    }

    #[cfg(test)]
    pub(super) fn force_set_disk_consistent_lsn(&self, new_value: Lsn) {
        self.disk_consistent_lsn.store(new_value);
    }

}

/// yb-mode placeholder. The original Neon TimelineWriter tracked the
/// open InMemoryLayer + size + last-freeze hints to drive the
/// roll-layer trigger heuristic. ybLight has no InMemoryLayer
/// (writes go through PageWriter → YB rows), so the state is empty.
/// Kept as a unit struct so the `Mutex<Option<TimelineWriterState>>`
/// surface that the TimelineWriter wrapper holds doesn't have to
/// change.
pub(crate) struct TimelineWriterState;

/// Various functions to mutate the timeline.
// TODO Currently, Deref is used to allow easy access to read methods from this trait.
// This is probably considered a bad practice in Rust and should be fixed eventually,
// but will cause large code changes.
pub(crate) struct TimelineWriter<'a> {
    tl: &'a Timeline,
    _write_guard: tokio::sync::MutexGuard<'a, Option<TimelineWriterState>>,
}

impl Deref for TimelineWriter<'_> {
    type Target = Timeline;

    fn deref(&self) -> &Self::Target {
        self.tl
    }
}

impl TimelineWriter<'_> {

    /// Put a batch of keys at the specified Lsns.
    pub(crate) async fn put_batch(
        &mut self,
        batch: SerializedValueBatch,
        _ctx: &RequestContext,
    ) -> anyhow::Result<()> {
        if !batch.has_data() {
            return Ok(());
        }

        // In debug builds, assert that we don't write any keys that don't belong to this shard.
        if cfg!(debug_assertions) {
            for metadata in &batch.metadata {
                if let ValueMeta::Serialized(metadata) = metadata {
                    let key = Key::from_compact(metadata.key);
                    assert!(
                        self.shard_identity.is_key_local(&key)
                            || self.shard_identity.is_key_global(&key),
                        "key {key} does not belong on shard {}",
                        self.shard_identity.shard_index()
                    );
                }
            }
        }

        let batch_max_lsn = batch.max_lsn;

        // yb-mode write path: every entry routes into PageWriter →
        // pageserver.{pages, slru_pages, meta} rows. The legacy
        // layer-file write branch is gone (Phase 4d): a timeline
        // without a PageWriter has nowhere to put the data.
        let page_writer = self.tl.page_writer.as_ref().ok_or_else(|| {
            anyhow::anyhow!(
                "Timeline has no PageWriter — yb-pageserver mode requires both \
                 compute_page_store and internal_page_store to be configured"
            )
        })?;
        let triples = crate::page_writer::extract_triples(&batch)?;
        if !triples.is_empty() {
            page_writer.put(&triples).await?;
        }
        // Advance disk_consistent_lsn so callers that depend on it
        // (bootstrap finalisation, basebackup gate) see the row-store
        // writes as durable. PageWriter's flush is periodic; treating
        // the in-memory buffer as "durable enough" matches the
        // layer-stack's behaviour where data is visible to
        // disk_consistent_lsn before reaching remote storage.
        self.tl.disk_consistent_lsn.fetch_max(batch_max_lsn);
        // yb-mode: the legacy TimelineWriterState (prev_lsn, max_lsn,
        // open_layer) has no writers anymore — those fields fed the
        // deleted roll-layer trigger heuristic.
        let _ = batch_max_lsn;
        Ok(())
    }

    pub(crate) async fn delete_batch(
        &mut self,
        _batch: &[(Range<Key>, Lsn)],
        _ctx: &RequestContext,
    ) -> anyhow::Result<()> {
        // yb-mode: deletions of relations / SLRU segments are not
        // expressed as tombstone keys in the row store; the WAL
        // record itself (TRUNCATE / DROP RELATION) drives the
        // appropriate INSERTs into pageserver.pages via PageWriter.
        // The layer-file tombstone path that this hook fed into is
        // gone (Phase 4d).
        Ok(())
    }

    /// Track the end of the latest digested WAL record.
    /// Remember the (end of) last valid WAL record remembered in the timeline.
    ///
    /// Call this after you have finished writing all the WAL up to 'lsn'.
    ///
    /// 'lsn' must be aligned. This wakes up any wait_lsn() callers waiting for
    /// the 'lsn' or anything older. The previous last record LSN is stored alongside
    /// the latest and can be read.
    pub(crate) fn finish_write(&self, new_lsn: Lsn) {
        self.tl.finish_write(new_lsn);
    }

    pub(crate) fn update_current_logical_size(&self, delta: i64) {
        self.tl.update_current_logical_size(delta)
    }
}

// We need TimelineWriter to be send in upcoming conversion of
// Timeline::layers to tokio::sync::RwLock.
#[test]
fn is_send() {
    fn _assert_send<T: Send>() {}
    _assert_send::<TimelineWriter<'_>>();
}


/// Encode an SLRU block row's identity as the 18-byte BYTEA key the
/// `PageRow.key` field expects. Used by the SLRU branch of
/// `get_vectored_via_page_writer` to convert an `SlruPageRow` into the
/// uniform `PageRow` shape that walredo + ValueReconstructState expect.
fn slru_key_bytes(row: &crate::page_store::SlruPageRow) -> bytes::Bytes {
    use pageserver_api::key::{KEY_SIZE, slru_block_to_key};
    let key = slru_block_to_key(row.kind, row.segno, row.blknum);
    let mut buf = vec![0u8; KEY_SIZE];
    key.write_to_byte_slice(&mut buf);
    bytes::Bytes::from(buf)
}

/// Decode a `PageWriter::read_chain` result into walredo's expected
/// `ValueReconstructState` shape.
///
/// `chain` is sorted **LSN descending** (newest first) and truncated at
/// the first `will_init=true` row (the chain anchor). For walredo we
/// produce:
/// - `img`: the anchor row's payload if `is_image=true`. Otherwise the
///   anchor is a `will_init` delta and goes into `records` like the
///   rest — walredo handles "first record initialises the page".
/// - `records`: every other row in the chain, decoded as
///   `NeonWalRecord`. Kept in descending-LSN order; `reconstruct_value`
///   reverses to ascending internally before applying.
fn chain_to_value_reconstruct_state(
    chain: &[crate::page_store::PageRow],
) -> anyhow::Result<crate::tenant::storage_layer::ValueReconstructState> {
    use crate::tenant::storage_layer::ValueReconstructState;
    use utils::bin_ser::BeSer;
    use wal_decoder::models::record::NeonWalRecord;

    let mut state = ValueReconstructState::default();
    if chain.is_empty() {
        return Ok(state);
    }
    // The anchor (oldest row) is at chain.last() because `chain` is
    // LSN-descending.
    let anchor_idx = chain.len() - 1;
    let anchor = &chain[anchor_idx];

    let records_slice: &[crate::page_store::PageRow] = if anchor.is_image {
        // Image anchor: hand it to walredo as base_img, deltas above
        // become records.
        state.img = Some((anchor.lsn, anchor.payload.clone()));
        &chain[..anchor_idx]
    } else {
        // will_init delta anchor: include it as the first (oldest)
        // record. No base_img.
        &chain[..]
    };

    state.records.reserve(records_slice.len());
    for row in records_slice {
        // Already in descending order.
        let rec = NeonWalRecord::des(&row.payload).map_err(|e| {
            anyhow::anyhow!("failed to decode NeonWalRecord at lsn={}: {e:#}", row.lsn)
        })?;
        state.records.push((row.lsn, rec));
    }
    Ok(state)
}

