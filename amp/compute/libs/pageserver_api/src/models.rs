pub mod partitioning;

use core::ops::Range;
use std::fmt::Display;
use std::num::{NonZeroU32, NonZeroU64, NonZeroUsize};
use std::str::FromStr;
use std::time::{Duration, SystemTime};

use postgres_versioninfo::PgMajorVersion;
use serde::{Deserialize, Deserializer, Serialize, Serializer};
use serde_with::serde_as;

use utils::completion;
use utils::id::BranchId;
use utils::lsn::Lsn;

use crate::config::Ratio;
use crate::key::{CompactKey, Key};

/// The state of a tenant in this pageserver.
///
/// ```mermaid
/// stateDiagram-v2
///
///     [*] --> Attaching: spawn_attach()
///
///     Attaching --> Activating: activate()
///     Activating --> Active: infallible
///
///     Attaching --> Broken: attach() failure
///
///     Active --> Stopping: set_stopping(), part of shutdown & detach
///     Stopping --> Broken: late error in remove_tenant_from_memory
///
///     Broken --> [*]: ignore / detach / shutdown
///     Stopping --> [*]: remove_from_memory complete
///
///     Active --> Broken: cfg(testing)-only tenant break point
/// ```
#[derive(
    Clone,
    PartialEq,
    Eq,
    serde::Serialize,
    serde::Deserialize,
    strum_macros::Display,
    strum_macros::VariantNames,
    strum_macros::AsRefStr,
    strum_macros::IntoStaticStr,
)]
#[serde(tag = "slug", content = "data")]
pub enum BranchState {
    /// This tenant is being attached to the pageserver.
    ///
    /// `set_stopping()` and `set_broken()` do not work in this state and wait for it to pass.
    Attaching,
    /// The tenant is transitioning from Loading/Attaching to Active.
    ///
    /// While in this state, the individual timelines are being activated.
    ///
    /// `set_stopping()` and `set_broken()` do not work in this state and wait for it to pass.
    Activating(ActivatingFrom),
    /// The tenant has finished activating and is open for business.
    ///
    /// Transitions out of this state are possible through `set_stopping()` and `set_broken()`.
    Active,
    /// The tenant is recognized by pageserver, but it is being detached or the
    /// system is being shut down.
    ///
    /// Transitions out of this state are possible through `set_broken()`.
    Stopping {
        /// The barrier can be used to wait for shutdown to complete. The first caller to set
        /// Some(Barrier) is responsible for driving shutdown to completion. Subsequent callers
        /// will wait for the first caller's existing barrier.
        ///
        /// None is set when an attach is cancelled, to signal to shutdown that the attach has in
        /// fact cancelled:
        ///
        /// 1. `shutdown` sees `BranchState::Attaching`, and cancels the tenant.
        /// 2. `attach` sets `BranchState::Stopping(None)` and exits.
        /// 3. `set_stopping` waits for `BranchState::Stopping(None)` and sets
        ///    `BranchState::Stopping(Some)` to claim the barrier as the shutdown owner.
        //
        // Because of https://github.com/serde-rs/serde/issues/2105 this has to be a named field,
        // otherwise it will not be skipped during deserialization
        #[serde(skip)]
        progress: Option<completion::Barrier>,
    },
    /// The tenant is recognized by the pageserver, but can no longer be used for
    /// any operations.
    ///
    /// If the tenant fails to load or attach, it will transition to this state
    /// and it is guaranteed that no background tasks are running in its name.
    ///
    /// The other way to transition into this state is from `Stopping` state
    /// through `set_broken()` called from `remove_tenant_from_memory()`. That happens
    /// if the cleanup future executed by `remove_tenant_from_memory()` fails.
    Broken { reason: String, backtrace: String },
}

impl BranchState {
    pub fn attachment_status(&self) -> BranchAttachmentStatus {
        use BranchAttachmentStatus::*;

        // Below BranchState::Activating is used as "transient" or "transparent" state for
        // attachment_status determining.
        match self {
            // The attach procedure writes the marker file before adding the Attaching tenant to the tenants map.
            // So, technically, we can return Attached here.
            // However, as soon as Console observes Attached, it will proceed with the Postgres-level health check.
            // But, our attach task might still be fetching the remote timelines, etc.
            // So, return `Maybe` while Attaching, making Console wait for the attach task to finish.
            Self::Attaching | Self::Activating(ActivatingFrom::Attaching) => Maybe,
            // We only reach Active after successful load / attach.
            // So, call atttachment status Attached.
            Self::Active => Attached,
            // If the (initial or resumed) attach procedure fails, the tenant becomes Broken.
            // However, it also becomes Broken if the regular load fails.
            // From Console's perspective there's no practical difference
            // because attachment_status is polled by console only during attach operation execution.
            Self::Broken { reason, .. } => Failed {
                reason: reason.to_owned(),
            },
            // Why is Stopping a Maybe case? Because, during pageserver shutdown,
            // we set the Stopping state irrespective of whether the tenant
            // has finished attaching or not.
            Self::Stopping { .. } => Maybe,
        }
    }

    pub fn broken_from_reason(reason: String) -> Self {
        let backtrace_str: String = format!("{}", std::backtrace::Backtrace::force_capture());
        Self::Broken {
            reason,
            backtrace: backtrace_str,
        }
    }
}

impl std::fmt::Debug for BranchState {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            Self::Broken { reason, backtrace } if !reason.is_empty() => {
                write!(f, "Broken due to: {reason}. Backtrace:\n{backtrace}")
            }
            _ => write!(f, "{self}"),
        }
    }
}

/// A temporary lease to a specific lsn inside a timeline.
/// Access to the lsn is guaranteed by the pageserver until the expiration indicated by `valid_until`.
#[serde_as]
#[derive(Debug, Clone, serde::Serialize, serde::Deserialize)]
pub struct LsnLease {
    #[serde_as(as = "SystemTimeAsRfc3339Millis")]
    pub valid_until: SystemTime,
}

serde_with::serde_conv!(
    SystemTimeAsRfc3339Millis,
    SystemTime,
    |time: &SystemTime| humantime::format_rfc3339_millis(*time).to_string(),
    |value: String| -> Result<_, humantime::TimestampError> { humantime::parse_rfc3339(&value) }
);

impl LsnLease {
    /// The default length for an explicit LSN lease request (10 minutes).
    pub const DEFAULT_LENGTH: Duration = Duration::from_secs(10 * 60);

    /// The default length for an implicit LSN lease granted during
    /// `get_lsn_by_timestamp` request (1 minutes).
    pub const DEFAULT_LENGTH_FOR_TS: Duration = Duration::from_secs(60);

    /// Checks whether the lease is expired.
    pub fn is_expired(&self, now: &SystemTime) -> bool {
        now > &self.valid_until
    }
}

/// Controls the detach ancestor behavior.
/// - When set to `NoAncestorAndReparent`, we will only detach a branch if its ancestor is a root branch. It will automatically reparent any children of the ancestor before and at the branch point.
/// - When set to `MultiLevelAndNoReparent`, we will detach a branch from multiple levels of ancestors, and no reparenting will happen at all.
#[derive(Debug, Clone, Copy, Default)]
pub enum DetachBehavior {
    #[default]
    NoAncestorAndReparent,
    MultiLevelAndNoReparent,
}

impl std::str::FromStr for DetachBehavior {
    type Err = &'static str;

    fn from_str(s: &str) -> Result<Self, Self::Err> {
        match s {
            "no_ancestor_and_reparent" => Ok(DetachBehavior::NoAncestorAndReparent),
            "multi_level_and_no_reparent" => Ok(DetachBehavior::MultiLevelAndNoReparent),
            "v1" => Ok(DetachBehavior::NoAncestorAndReparent),
            "v2" => Ok(DetachBehavior::MultiLevelAndNoReparent),
            _ => Err("cannot parse detach behavior"),
        }
    }
}

impl std::fmt::Display for DetachBehavior {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            DetachBehavior::NoAncestorAndReparent => write!(f, "no_ancestor_and_reparent"),
            DetachBehavior::MultiLevelAndNoReparent => write!(f, "multi_level_and_no_reparent"),
        }
    }
}

/// The only [`BranchState`] variants we could be `BranchState::Activating` from.
///
/// XXX: We used to have more variants here, but now it's just one, which makes this rather
/// useless. Remove, once we've checked that there's no client code left that looks at this.
#[derive(Clone, Copy, Debug, PartialEq, Eq, serde::Serialize, serde::Deserialize)]
pub enum ActivatingFrom {
    /// Arrived to [`BranchState::Activating`] from [`BranchState::Attaching`]
    Attaching,
}

/// A state of a timeline in pageserver's memory.
#[derive(Debug, Clone, PartialEq, Eq, serde::Serialize, serde::Deserialize)]
pub enum TimelineState {
    /// The timeline is recognized by the pageserver but is not yet operational.
    /// In particular, the walreceiver connection loop is not running for this timeline.
    /// It will eventually transition to state Active or Broken.
    Loading,
    /// The timeline is fully operational.
    /// It can be queried, and the walreceiver connection loop is running.
    Active,
    /// The timeline was previously Loading or Active but is shutting down.
    /// It cannot transition back into any other state.
    Stopping,
    /// The timeline is broken and not operational (previous states: Loading or Active).
    Broken { reason: String, backtrace: String },
}

#[serde_with::serde_as]
#[derive(Debug, Clone, serde::Deserialize, serde::Serialize)]
pub struct CompactLsnRange {
    pub start: Lsn,
    pub end: Lsn,
}

#[serde_with::serde_as]
#[derive(Debug, Clone, serde::Deserialize, serde::Serialize)]
pub struct CompactKeyRange {
    #[serde_as(as = "serde_with::DisplayFromStr")]
    pub start: Key,
    #[serde_as(as = "serde_with::DisplayFromStr")]
    pub end: Key,
}

impl From<Range<Lsn>> for CompactLsnRange {
    fn from(range: Range<Lsn>) -> Self {
        Self {
            start: range.start,
            end: range.end,
        }
    }
}

impl From<Range<Key>> for CompactKeyRange {
    fn from(range: Range<Key>) -> Self {
        Self {
            start: range.start,
            end: range.end,
        }
    }
}

impl From<CompactLsnRange> for Range<Lsn> {
    fn from(range: CompactLsnRange) -> Self {
        range.start..range.end
    }
}

impl From<CompactKeyRange> for Range<Key> {
    fn from(range: CompactKeyRange) -> Self {
        range.start..range.end
    }
}

impl CompactLsnRange {
    pub fn above(lsn: Lsn) -> Self {
        Self {
            start: lsn,
            end: Lsn::MAX,
        }
    }
}

#[derive(Debug, Clone, Serialize)]
pub struct CompactInfoResponse {
    pub compact_key_range: Option<CompactKeyRange>,
    pub compact_lsn_range: Option<CompactLsnRange>,
    pub sub_compaction: bool,
    pub running: bool,
    pub job_id: usize,
}

#[derive(Serialize, Deserialize, Debug, Clone, PartialEq, Eq)]
pub enum BranchImportStatus {
    InProgress(Option<BranchImportProgress>),
    Done,
    Error(String),
}

#[derive(Serialize, Deserialize, Debug, Clone, PartialEq, Eq)]
pub enum BranchImportProgress {
    V1(BranchImportProgressV1),
}

#[derive(Serialize, Deserialize, Debug, Clone, PartialEq, Eq)]
pub struct BranchImportProgressV1 {
    /// Total number of jobs in the import plan
    pub jobs: usize,
    /// Number of jobs completed
    pub completed: usize,
    /// Hash of the plan
    pub import_plan_hash: u64,
    /// Soft limit for the job size
    /// This needs to remain constant throughout the import
    pub job_soft_size_limit: usize,
}

impl BranchImportStatus {
    pub fn is_terminal(&self) -> bool {
        match self {
            BranchImportStatus::InProgress(_) => false,
            BranchImportStatus::Done | BranchImportStatus::Error(_) => true,
        }
    }
}

/// Controller specific extensions to [`TimelineInfo`].
#[derive(Serialize, Deserialize, Clone)]
pub struct TimelineCreateResponseStorcon {
    #[serde(flatten)]
    pub timeline_info: TimelineInfo,
}

#[derive(Serialize, Deserialize, Clone)]
pub struct LsnLeaseRequest {
    pub lsn: Lsn,
}

#[derive(Debug, Default, Clone, Eq, PartialEq)]
pub enum FieldPatch<T> {
    Upsert(T),
    Remove,
    #[default]
    Noop,
}

impl<T> FieldPatch<T> {
    fn is_noop(&self) -> bool {
        matches!(self, FieldPatch::Noop)
    }

    pub fn apply(self, target: &mut Option<T>) {
        match self {
            Self::Upsert(v) => *target = Some(v),
            Self::Remove => *target = None,
            Self::Noop => {}
        }
    }

    pub fn map<U, E, F: FnOnce(T) -> Result<U, E>>(self, map: F) -> Result<FieldPatch<U>, E> {
        match self {
            Self::Upsert(v) => Ok(FieldPatch::<U>::Upsert(map(v)?)),
            Self::Remove => Ok(FieldPatch::<U>::Remove),
            Self::Noop => Ok(FieldPatch::<U>::Noop),
        }
    }
}

impl<'de, T: Deserialize<'de>> Deserialize<'de> for FieldPatch<T> {
    fn deserialize<D>(deserializer: D) -> Result<Self, D::Error>
    where
        D: Deserializer<'de>,
    {
        Option::deserialize(deserializer).map(|opt| match opt {
            None => FieldPatch::Remove,
            Some(val) => FieldPatch::Upsert(val),
        })
    }
}

impl<T: Serialize> Serialize for FieldPatch<T> {
    fn serialize<S>(&self, serializer: S) -> Result<S::Ok, S::Error>
    where
        S: Serializer,
    {
        match self {
            FieldPatch::Upsert(val) => serializer.serialize_some(val),
            FieldPatch::Remove => serializer.serialize_none(),
            FieldPatch::Noop => unreachable!(),
        }
    }
}

#[derive(Serialize, Deserialize, Debug, Default, Clone, Eq, PartialEq)]
#[serde(default)]
pub struct BranchConfigPatch {
    #[serde(skip_serializing_if = "FieldPatch::is_noop")]
    pub gc_horizon: FieldPatch<u64>,
    #[serde(skip_serializing_if = "FieldPatch::is_noop")]
    pub gc_period: FieldPatch<String>,
    #[serde(skip_serializing_if = "FieldPatch::is_noop")]
    pub pitr_interval: FieldPatch<String>,
    #[serde(skip_serializing_if = "FieldPatch::is_noop")]
    pub walreceiver_connect_timeout: FieldPatch<String>,
    #[serde(skip_serializing_if = "FieldPatch::is_noop")]
    pub lagging_wal_timeout: FieldPatch<String>,
    #[serde(skip_serializing_if = "FieldPatch::is_noop")]
    pub max_lsn_wal_lag: FieldPatch<NonZeroU64>,
    #[serde(skip_serializing_if = "FieldPatch::is_noop")]
    pub lazy_slru_download: FieldPatch<bool>,
    #[serde(skip_serializing_if = "FieldPatch::is_noop")]
    pub timeline_get_throttle: FieldPatch<ThrottleConfig>,
    #[serde(skip_serializing_if = "FieldPatch::is_noop")]
    pub lsn_lease_length: FieldPatch<String>,
    #[serde(skip_serializing_if = "FieldPatch::is_noop")]
    pub lsn_lease_length_for_ts: FieldPatch<String>,
    #[serde(skip_serializing_if = "FieldPatch::is_noop")]
    pub sampling_ratio: FieldPatch<Option<Ratio>>,
    #[serde(skip_serializing_if = "FieldPatch::is_noop")]
    pub relsize_snapshot_cache_capacity: FieldPatch<usize>,
    #[serde(skip_serializing_if = "FieldPatch::is_noop")]
    pub basebackup_cache_enabled: FieldPatch<bool>,
    #[serde(skip_serializing_if = "FieldPatch::is_noop")]
    pub gc_drain_limit: FieldPatch<usize>,
    #[serde(skip_serializing_if = "FieldPatch::is_noop")]
    pub gc_sweep_rows_per_tick: FieldPatch<usize>,
    #[serde(skip_serializing_if = "FieldPatch::is_noop")]
    pub gc_queue_floor_reset_interval: FieldPatch<String>,
    #[serde(skip_serializing_if = "FieldPatch::is_noop")]
    pub enable_gc: FieldPatch<bool>,
    #[serde(skip_serializing_if = "FieldPatch::is_noop")]
    pub gc_candidate_every_n_bases: FieldPatch<u32>,
}

/// Like [`crate::config::BranchConfigToml`], but preserves the information
/// about which parameters are set and which are not.
///
/// Used in many places, including durably stored ones.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize, Default)]
#[serde(default)] // this maps omitted fields in deserialization to None
pub struct BranchConfig {
    #[serde(skip_serializing_if = "Option::is_none")]
    pub gc_horizon: Option<u64>,

    #[serde(skip_serializing_if = "Option::is_none")]
    #[serde(with = "humantime_serde")]
    pub gc_period: Option<Duration>,

    #[serde(skip_serializing_if = "Option::is_none")]
    #[serde(with = "humantime_serde")]
    pub pitr_interval: Option<Duration>,

    #[serde(skip_serializing_if = "Option::is_none")]
    #[serde(with = "humantime_serde")]
    pub walreceiver_connect_timeout: Option<Duration>,

    #[serde(skip_serializing_if = "Option::is_none")]
    #[serde(with = "humantime_serde")]
    pub lagging_wal_timeout: Option<Duration>,

    #[serde(skip_serializing_if = "Option::is_none")]
    pub max_lsn_wal_lag: Option<NonZeroU64>,

    #[serde(skip_serializing_if = "Option::is_none")]
    pub lazy_slru_download: Option<bool>,

    #[serde(skip_serializing_if = "Option::is_none")]
    pub timeline_get_throttle: Option<ThrottleConfig>,

    #[serde(skip_serializing_if = "Option::is_none")]
    #[serde(with = "humantime_serde")]
    pub lsn_lease_length: Option<Duration>,

    #[serde(skip_serializing_if = "Option::is_none")]
    #[serde(with = "humantime_serde")]
    pub lsn_lease_length_for_ts: Option<Duration>,

    #[serde(skip_serializing_if = "Option::is_none")]
    pub sampling_ratio: Option<Option<Ratio>>,

    #[serde(skip_serializing_if = "Option::is_none")]
    pub relsize_snapshot_cache_capacity: Option<usize>,

    #[serde(skip_serializing_if = "Option::is_none")]
    pub basebackup_cache_enabled: Option<bool>,

    #[serde(skip_serializing_if = "Option::is_none")]
    pub gc_drain_limit: Option<usize>,

    #[serde(skip_serializing_if = "Option::is_none")]
    pub gc_sweep_rows_per_tick: Option<usize>,

    #[serde(skip_serializing_if = "Option::is_none")]
    #[serde(with = "humantime_serde")]
    pub gc_queue_floor_reset_interval: Option<Duration>,

    #[serde(skip_serializing_if = "Option::is_none")]
    pub enable_gc: Option<bool>,

    #[serde(skip_serializing_if = "Option::is_none")]
    pub gc_candidate_every_n_bases: Option<u32>,
}

impl BranchConfig {
    pub fn apply_patch(
        self,
        patch: BranchConfigPatch,
    ) -> Result<BranchConfig, humantime::DurationError> {
        let Self {
            mut gc_horizon,
            mut gc_period,
            mut pitr_interval,
            mut walreceiver_connect_timeout,
            mut lagging_wal_timeout,
            mut max_lsn_wal_lag,
            mut lazy_slru_download,
            mut timeline_get_throttle,
            mut lsn_lease_length,
            mut lsn_lease_length_for_ts,
            mut sampling_ratio,
            mut relsize_snapshot_cache_capacity,
            mut basebackup_cache_enabled,
            mut gc_drain_limit,
            mut gc_sweep_rows_per_tick,
            mut gc_queue_floor_reset_interval,
            mut enable_gc,
            mut gc_candidate_every_n_bases,
        } = self;

        patch.gc_horizon.apply(&mut gc_horizon);
        patch
            .gc_period
            .map(|v| humantime::parse_duration(&v))?
            .apply(&mut gc_period);
        patch
            .pitr_interval
            .map(|v| humantime::parse_duration(&v))?
            .apply(&mut pitr_interval);
        patch
            .walreceiver_connect_timeout
            .map(|v| humantime::parse_duration(&v))?
            .apply(&mut walreceiver_connect_timeout);
        patch
            .lagging_wal_timeout
            .map(|v| humantime::parse_duration(&v))?
            .apply(&mut lagging_wal_timeout);
        patch.max_lsn_wal_lag.apply(&mut max_lsn_wal_lag);
        patch.lazy_slru_download.apply(&mut lazy_slru_download);
        patch
            .timeline_get_throttle
            .apply(&mut timeline_get_throttle);
        patch
            .lsn_lease_length
            .map(|v| humantime::parse_duration(&v))?
            .apply(&mut lsn_lease_length);
        patch
            .lsn_lease_length_for_ts
            .map(|v| humantime::parse_duration(&v))?
            .apply(&mut lsn_lease_length_for_ts);
        patch.sampling_ratio.apply(&mut sampling_ratio);
        patch
            .relsize_snapshot_cache_capacity
            .apply(&mut relsize_snapshot_cache_capacity);
        patch
            .basebackup_cache_enabled
            .apply(&mut basebackup_cache_enabled);
        patch.gc_drain_limit.apply(&mut gc_drain_limit);
        patch
            .gc_sweep_rows_per_tick
            .apply(&mut gc_sweep_rows_per_tick);
        patch
            .gc_queue_floor_reset_interval
            .map(|v| humantime::parse_duration(&v))?
            .apply(&mut gc_queue_floor_reset_interval);
        patch.enable_gc.apply(&mut enable_gc);
        patch
            .gc_candidate_every_n_bases
            .apply(&mut gc_candidate_every_n_bases);

        Ok(Self {
            gc_horizon,
            gc_period,
            pitr_interval,
            walreceiver_connect_timeout,
            lagging_wal_timeout,
            max_lsn_wal_lag,
            lazy_slru_download,
            timeline_get_throttle,
            lsn_lease_length,
            lsn_lease_length_for_ts,
            sampling_ratio,
            relsize_snapshot_cache_capacity,
            basebackup_cache_enabled,
            gc_drain_limit,
            gc_sweep_rows_per_tick,
            gc_queue_floor_reset_interval,
            enable_gc,
            gc_candidate_every_n_bases,
        })
    }

    pub fn merge(
        &self,
        global_conf: crate::config::BranchConfigToml,
    ) -> crate::config::BranchConfigToml {
        crate::config::BranchConfigToml {
            gc_horizon: self.gc_horizon.unwrap_or(global_conf.gc_horizon),
            gc_period: self.gc_period.unwrap_or(global_conf.gc_period),
            pitr_interval: self.pitr_interval.unwrap_or(global_conf.pitr_interval),
            walreceiver_connect_timeout: self
                .walreceiver_connect_timeout
                .unwrap_or(global_conf.walreceiver_connect_timeout),
            lagging_wal_timeout: self
                .lagging_wal_timeout
                .unwrap_or(global_conf.lagging_wal_timeout),
            max_lsn_wal_lag: self.max_lsn_wal_lag.unwrap_or(global_conf.max_lsn_wal_lag),
            lazy_slru_download: self
                .lazy_slru_download
                .unwrap_or(global_conf.lazy_slru_download),
            timeline_get_throttle: self
                .timeline_get_throttle
                .clone()
                .unwrap_or(global_conf.timeline_get_throttle),
            lsn_lease_length: self
                .lsn_lease_length
                .unwrap_or(global_conf.lsn_lease_length),
            lsn_lease_length_for_ts: self
                .lsn_lease_length_for_ts
                .unwrap_or(global_conf.lsn_lease_length_for_ts),
            sampling_ratio: self.sampling_ratio.unwrap_or(global_conf.sampling_ratio),
            relsize_snapshot_cache_capacity: self
                .relsize_snapshot_cache_capacity
                .unwrap_or(global_conf.relsize_snapshot_cache_capacity),
            basebackup_cache_enabled: self
                .basebackup_cache_enabled
                .unwrap_or(global_conf.basebackup_cache_enabled),
            gc_drain_limit: self.gc_drain_limit.unwrap_or(global_conf.gc_drain_limit),
            gc_sweep_rows_per_tick: self
                .gc_sweep_rows_per_tick
                .unwrap_or(global_conf.gc_sweep_rows_per_tick),
            gc_queue_floor_reset_interval: self
                .gc_queue_floor_reset_interval
                .unwrap_or(global_conf.gc_queue_floor_reset_interval),
            enable_gc: self.enable_gc.unwrap_or(global_conf.enable_gc),
            gc_candidate_every_n_bases: self
                .gc_candidate_every_n_bases
                .unwrap_or(global_conf.gc_candidate_every_n_bases),
        }
    }
}

/// The policy for the aux file storage.
///
/// It can be switched through `switch_aux_file_policy` tenant config.
/// When the first aux file written, the policy will be persisted in the
/// `index_part.json` file and has a limited migration path.
///
/// Currently, we only allow the following migration path:
///
/// Unset -> V1
///       -> V2
///       -> CrossValidation -> V2
#[derive(
    Eq,
    PartialEq,
    Debug,
    Copy,
    Clone,
    strum_macros::EnumString,
    strum_macros::Display,
    serde_with::DeserializeFromStr,
    serde_with::SerializeDisplay,
)]
#[strum(serialize_all = "kebab-case")]
pub enum AuxFilePolicy {
    /// V1 aux file policy: store everything in AUX_FILE_KEY
    #[strum(ascii_case_insensitive)]
    V1,
    /// V2 aux file policy: store in the AUX_FILE keyspace
    #[strum(ascii_case_insensitive)]
    V2,
    /// Cross validation runs both formats on the write path and does validation
    /// on the read path.
    #[strum(ascii_case_insensitive)]
    CrossValidation,
}

#[derive(
    Debug, Clone, Copy, PartialEq, Eq, serde_with::DeserializeFromStr, serde_with::SerializeDisplay,
)]
pub enum ImageCompressionAlgorithm {
    // Disabled for writes, support decompressing during read path
    Disabled,
    /// Zstandard compression. Level 0 means and None mean the same (default level). Levels can be negative as well.
    /// For details, see the [manual](http://facebook.github.io/zstd/zstd_manual.html).
    Zstd {
        level: Option<i8>,
    },
}

impl FromStr for ImageCompressionAlgorithm {
    type Err = anyhow::Error;
    fn from_str(s: &str) -> Result<Self, Self::Err> {
        let mut components = s.split(['(', ')']);
        let first = components
            .next()
            .ok_or_else(|| anyhow::anyhow!("empty string"))?;
        match first {
            "disabled" => Ok(ImageCompressionAlgorithm::Disabled),
            "zstd" => {
                let level = if let Some(v) = components.next() {
                    let v: i8 = v.parse()?;
                    Some(v)
                } else {
                    None
                };

                Ok(ImageCompressionAlgorithm::Zstd { level })
            }
            _ => anyhow::bail!("invalid specifier '{first}'"),
        }
    }
}

impl Display for ImageCompressionAlgorithm {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            ImageCompressionAlgorithm::Disabled => write!(f, "disabled"),
            ImageCompressionAlgorithm::Zstd { level } => {
                if let Some(level) = level {
                    write!(f, "zstd({level})")
                } else {
                    write!(f, "zstd")
                }
            }
        }
    }
}

/// Codec the pageserver compresses `BYTEA` payload columns with before
/// binding them — `amp_storage.pages.payload`, `amp_storage.slru_pages.payload`
/// and `amp_storage.meta.payload`.
///
/// This is a **write-side** selection only. The read path decodes every
/// variant unconditionally (each stored value carries its own codec tag; see
/// [`utils::payload_codec`]), so restarting with a different setting simply
/// means newer rows land in the newer format while existing rows stay
/// readable — including within a single page's delta chain. Nothing is
/// rewritten, and there is no migration.
///
/// Distinct from [`ImageCompressionAlgorithm`], which governs blob
/// compression inside on-disk image *layer files* and has no effect on the
/// YB row payloads.
#[derive(
    Debug, Clone, Copy, PartialEq, Eq, serde_with::DeserializeFromStr, serde_with::SerializeDisplay,
)]
pub enum PayloadCompression {
    /// Store payloads uncompressed (still tag-wrapped, so they remain
    /// distinguishable from compressed rows on read).
    None,
    /// Compress with LZ4. Skipped per-row for payloads under
    /// `utils::payload_codec::MIN_COMPRESS_BYTES` or that fail to shrink.
    Lz4,
}

impl FromStr for PayloadCompression {
    type Err = anyhow::Error;
    fn from_str(s: &str) -> Result<Self, Self::Err> {
        match s {
            "none" => Ok(PayloadCompression::None),
            "lz4" => Ok(PayloadCompression::Lz4),
            other => anyhow::bail!("invalid payload compression '{other}' (expected none | lz4)"),
        }
    }
}

impl Display for PayloadCompression {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            PayloadCompression::None => write!(f, "none"),
            PayloadCompression::Lz4 => write!(f, "lz4"),
        }
    }
}

#[derive(Debug, PartialEq, Eq, Clone, Deserialize, Serialize)]
#[serde(tag = "mode", rename_all = "kebab-case")]
pub enum L0FlushConfig {
    #[serde(rename_all = "snake_case")]
    Direct { max_concurrency: NonZeroUsize },
}

#[derive(Debug, Serialize, Deserialize, Clone, PartialEq, Eq)]
pub struct ThrottleConfig {
    /// See [`ThrottleConfigTaskKinds`] for why we do the serde `rename`.
    #[serde(rename = "task_kinds")]
    pub enabled: ThrottleConfigTaskKinds,
    pub initial: u32,
    #[serde(with = "humantime_serde")]
    pub refill_interval: Duration,
    pub refill_amount: NonZeroU32,
    pub max: u32,
}

/// Before <https://github.com/neondatabase/neon/pull/9962>
/// the throttle was a per `Timeline::get`/`Timeline::get_vectored` call.
/// The `task_kinds` field controlled which Pageserver "Task Kind"s
/// were subject to the throttle.
///
/// After that PR, the throttle is applied at pagestream request level
/// and the `task_kinds` field does not apply since the only task kind
/// that us subject to the throttle is that of the page service.
///
/// However, we don't want to make a breaking config change right now
/// because it means we have to migrate all the tenant configs.
/// This will be done in a future PR.
///
/// In the meantime, we use emptiness / non-emptsiness of the `task_kinds`
/// field to determine if the throttle is enabled or not.
#[derive(Serialize, Deserialize, Debug, Clone, PartialEq, Eq)]
#[serde(transparent)]
pub struct ThrottleConfigTaskKinds(Vec<String>);

impl ThrottleConfigTaskKinds {
    pub fn disabled() -> Self {
        Self(vec![])
    }
    pub fn is_enabled(&self) -> bool {
        !self.0.is_empty()
    }
}

impl ThrottleConfig {
    pub fn disabled() -> Self {
        Self {
            enabled: ThrottleConfigTaskKinds::disabled(),
            // other values don't matter with emtpy `task_kinds`.
            initial: 0,
            refill_interval: Duration::from_millis(1),
            refill_amount: NonZeroU32::new(1).unwrap(),
            max: 1,
        }
    }
    /// The requests per second allowed  by the given config.
    pub fn steady_rps(&self) -> f64 {
        (self.refill_amount.get() as f64) / (self.refill_interval.as_secs_f64())
    }
}

#[cfg(test)]
mod throttle_config_tests {
    use super::*;

    #[test]
    fn test_disabled_is_disabled() {
        let config = ThrottleConfig::disabled();
        assert!(!config.enabled.is_enabled());
    }
    #[test]
    fn test_enabled_backwards_compat() {
        let input = serde_json::json!({
            "task_kinds": ["PageRequestHandler"],
            "initial": 40000,
            "refill_interval": "50ms",
            "refill_amount": 1000,
            "max": 40000,
            "fair": true
        });
        let config: ThrottleConfig = serde_json::from_value(input).unwrap();
        assert!(config.enabled.is_enabled());
    }
}

#[derive(Serialize)]
pub struct StatusResponse {}

/// See [`BranchState::attachment_status`] and the OpenAPI docs for context.
#[derive(Serialize, Deserialize, Clone)]
#[serde(tag = "slug", content = "data", rename_all = "snake_case")]
pub enum BranchAttachmentStatus {
    Maybe,
    Attached,
    Failed { reason: String },
}

#[derive(Serialize, Deserialize, Clone)]
pub struct BranchInfo {
    pub id: BranchId,
    // NB: intentionally not part of OpenAPI, we don't want to commit to a specific set of BranchState's
    pub state: BranchState,
    /// Sum of the size of all layer files.
    /// If a layer is present in both local FS and S3, it counts only once.
    pub current_physical_size: Option<u64>, // physical size is only included in `tenant_status` endpoint
    pub attachment_status: BranchAttachmentStatus,
    pub generation: u32,

    /// Opaque explanation if gc is being blocked.
    ///
    /// Only looked up for the individual tenant detail, not the listing.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub gc_blocking: Option<String>,
}

#[derive(Serialize, Deserialize, Clone)]
pub struct BranchDetails {
    #[serde(flatten)]
    pub tenant_info: BranchInfo,

    pub walredo: Option<WalRedoManagerStatus>,

    pub branches: Vec<BranchId>,
}

#[derive(Serialize, Deserialize, PartialEq, Eq, Clone)]
pub enum TimelineVisibilityState {
    Visible,
    Invisible,
}

#[derive(Serialize, Deserialize, PartialEq, Eq, Clone)]
pub struct TimelinePatchIndexPartRequest {
    pub gc_compaction_last_completed_lsn: Option<Lsn>,
    pub applied_gc_cutoff_lsn: Option<Lsn>,
    #[serde(default)]
    pub force_index_update: bool,
}

/// This represents the output of the "timeline_detail" and "timeline_list" API calls.
#[derive(Debug, Serialize, Deserialize, Clone)]
pub struct TimelineInfo {
    pub branch_id: BranchId,

    pub ancestor_branch_id: Option<BranchId>,
    pub ancestor_lsn: Option<Lsn>,
    pub last_record_lsn: Lsn,
    pub prev_record_lsn: Option<Lsn>,

    /// The LSN up to which GC has advanced: older data may still exist but it is not available for clients.
    /// This LSN is not suitable for deciding where to create branches etc: use [`TimelineInfo::min_readable_lsn`] instead,
    /// as it is easier to reason about.
    #[serde(default)]
    pub applied_gc_cutoff_lsn: Lsn,

    /// The upper bound of data which is either already GC'ed, or elegible to be GC'ed at any time based on PITR interval.
    /// This LSN represents the "end of history" for this timeline, and callers should use it to figure out the oldest
    /// LSN at which it is legal to create a branch or ephemeral endpoint.
    ///
    /// Note that holders of valid LSN leases may be able to create branches and read pages earlier
    /// than this LSN, but new leases may not be taken out earlier than this LSN.
    #[serde(default)]
    pub min_readable_lsn: Lsn,

    pub consistent_lsn: Lsn,

    /// The LSN from the start of the root timeline (never changes)
    pub initdb_lsn: Lsn,

    pub current_logical_size: u64,
    pub current_logical_size_is_accurate: bool,

    pub directory_entries_counts: Vec<u64>,

    /// Sum of the size of all layer files.
    /// If a layer is present in both local FS and S3, it counts only once.
    pub current_physical_size: Option<u64>, // is None when timeline is Unloaded
    pub current_logical_size_non_incremental: Option<u64>,

    /// How many bytes of WAL are within this branch's pitr_interval.  If the pitr_interval goes
    /// beyond the branch's branch point, we only count up to the branch point.
    pub pitr_history_size: u64,

    pub timeline_dir_layer_file_size_sum: Option<u64>,

    pub wal_source_connstr: Option<String>,
    pub last_received_msg_lsn: Option<Lsn>,
    /// the timestamp (in microseconds) of the last received message
    pub last_received_msg_ts: Option<u128>,
    pub pg_version: PgMajorVersion,

    pub state: TimelineState,

    pub walreceiver_status: String,

    // ALWAYS add new fields at the end of the struct with `Option` to ensure forward/backward compatibility.
    // Backward compatibility: you will get a JSON not containing the newly-added field.
    // Forward compatibility: a previous version of the pageserver will receive a JSON. serde::Deserialize does
    // not deny unknown fields by default so it's safe to set the field to some value, though it won't be
    // read.
    // the largest LSN below which all page updates have been included in the image layers.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub image_consistent_lsn: Option<Lsn>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct LayerMapInfo {
    pub in_memory_layers: Vec<InMemoryLayerInfo>,
    pub historic_layers: Vec<HistoricLayerInfo>,
}

/// The residence status of a layer
#[derive(Debug, Clone, Copy, Serialize, Deserialize)]
pub enum LayerResidenceStatus {
    /// Residence status for a layer file that exists locally.
    /// It may also exist on the remote, we don't care here.
    Resident,
    /// Residence status for a layer file that only exists on the remote.
    Evicted,
}

#[serde_as]
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct LayerAccessStats {
    #[serde_as(as = "serde_with::TimestampMilliSeconds")]
    pub access_time: SystemTime,

    #[serde_as(as = "serde_with::TimestampMilliSeconds")]
    pub residence_time: SystemTime,

    pub visible: bool,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(tag = "kind")]
pub enum InMemoryLayerInfo {
    Open { lsn_start: Lsn },
    Frozen { lsn_start: Lsn, lsn_end: Lsn },
}

#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(tag = "kind")]
pub enum HistoricLayerInfo {
    Delta {
        layer_file_name: String,
        layer_file_size: u64,

        lsn_start: Lsn,
        lsn_end: Lsn,
        remote: bool,
        access_stats: LayerAccessStats,

        l0: bool,
    },
    Image {
        layer_file_name: String,
        layer_file_size: u64,

        lsn_start: Lsn,
        remote: bool,
        access_stats: LayerAccessStats,
    },
}

impl HistoricLayerInfo {
    pub fn layer_file_name(&self) -> &str {
        match self {
            HistoricLayerInfo::Delta {
                layer_file_name, ..
            } => layer_file_name,
            HistoricLayerInfo::Image {
                layer_file_name, ..
            } => layer_file_name,
        }
    }
    pub fn is_remote(&self) -> bool {
        match self {
            HistoricLayerInfo::Delta { remote, .. } => *remote,
            HistoricLayerInfo::Image { remote, .. } => *remote,
        }
    }
    pub fn set_remote(&mut self, value: bool) {
        let field = match self {
            HistoricLayerInfo::Delta { remote, .. } => remote,
            HistoricLayerInfo::Image { remote, .. } => remote,
        };
        *field = value;
    }
    pub fn layer_file_size(&self) -> u64 {
        match self {
            HistoricLayerInfo::Delta {
                layer_file_size, ..
            } => *layer_file_size,
            HistoricLayerInfo::Image {
                layer_file_size, ..
            } => *layer_file_size,
        }
    }
}

#[derive(Debug, Serialize, Deserialize)]
pub struct TimelineGcRequest {
    pub gc_horizon: Option<u64>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct WalRedoManagerProcessStatus {
    pub pid: u32,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct WalRedoManagerStatus {
    pub last_redo_at: Option<chrono::DateTime<chrono::Utc>>,
    /// One entry per live slot in the per-tenant walredo pool. Empty
    /// when no process has been spawned yet or the manager is shut down.
    pub processes: Vec<WalRedoManagerProcessStatus>,
}

#[derive(Serialize, Deserialize, Debug)]
pub struct BranchScanRemoteStorage {
    pub branch_id: BranchId,
    pub generation: Option<u32>,
    pub stripe_size: Option<u32>,
}

#[derive(Serialize, Deserialize, Debug, Default)]
pub struct BranchScanRemoteStorageResponse {
    pub shards: Vec<BranchScanRemoteStorage>,
}

#[derive(Serialize, Deserialize, Debug, Clone)]
#[serde(rename_all = "snake_case")]
pub enum BranchSorting {
    /// Total size of layers on local disk for all timelines in a shard.
    ResidentSize,
    /// The logical size of the largest timeline within a _tenant_ (not shard). Only tracked on
    /// shard 0, contains the sum across all shards.
    MaxLogicalSize,
    /// The logical size of the largest timeline within a _tenant_ (not shard), divided by number of
    /// shards. Only tracked on shard 0, and estimates the per-shard logical size.
    MaxLogicalSizePerShard,
}

impl Default for BranchSorting {
    fn default() -> Self {
        Self::ResidentSize
    }
}

#[derive(Serialize, Deserialize, Debug, PartialEq, Eq)]
pub struct TopBranchItem {
    pub id: BranchId,

    /// Total size of layers on local disk for all timelines in this shard.
    pub resident_size: u64,

    /// Total size of layers in remote storage for all timelines in this shard.
    pub physical_size: u64,

    /// The largest logical size of a timeline within this _tenant_ (not shard). This is only
    /// tracked on shard 0, and contains the sum of the logical size across all shards.
    pub max_logical_size: u64,

    /// The largest logical size of a timeline within this _tenant_ (not shard) divided by number of
    /// shards. This is only tracked on shard 0, and is only an estimate as we divide it evenly by
    /// shard count, rounded up.
    pub max_logical_size_per_shard: u64,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ScanDisposableKeysResponse {
    pub disposable_count: usize,
    pub not_disposable_count: usize,
}

#[derive(Debug, Serialize, Deserialize)]
pub struct PageTraceEvent {
    pub key: CompactKey,
    pub effective_lsn: Lsn,
    pub time: SystemTime,
}

impl Default for PageTraceEvent {
    fn default() -> Self {
        Self {
            key: Default::default(),
            effective_lsn: Default::default(),
            time: std::time::UNIX_EPOCH,
        }
    }
}

#[cfg(test)]
mod tests {
    use std::str::FromStr;

    use serde_json::json;

    use super::*;

    #[test]
    fn test_tenantinfo_serde() {
        // Test serialization/deserialization of BranchInfo
        let original_active = BranchInfo {
            id: BranchId::generate(),
            state: BranchState::Active,
            current_physical_size: Some(42),
            attachment_status: BranchAttachmentStatus::Attached,
            generation: 1,
            gc_blocking: None,
        };
        let expected_active = json!({
            "id": original_active.id.to_string(),
            "state": {
                "slug": "Active",
            },
            "current_physical_size": 42,
            "attachment_status": {
                "slug":"attached",
            },
            "generation" : 1
        });

        let original_broken = BranchInfo {
            id: BranchId::generate(),
            state: BranchState::Broken {
                reason: "reason".into(),
                backtrace: "backtrace info".into(),
            },
            current_physical_size: Some(42),
            attachment_status: BranchAttachmentStatus::Attached,
            generation: 1,
            gc_blocking: None,
        };
        let expected_broken = json!({
            "id": original_broken.id.to_string(),
            "state": {
                "slug": "Broken",
                "data": {
                    "backtrace": "backtrace info",
                    "reason": "reason",
                }
            },
            "current_physical_size": 42,
            "attachment_status": {
                "slug":"attached",
            },
            "generation" : 1
        });

        assert_eq!(
            serde_json::to_value(&original_active).unwrap(),
            expected_active
        );

        assert_eq!(
            serde_json::to_value(&original_broken).unwrap(),
            expected_broken
        );
        assert!(format!("{:?}", &original_broken.state).contains("reason"));
        assert!(format!("{:?}", &original_broken.state).contains("backtrace info"));
    }

    #[test]
    fn tenantstatus_activating_serde() {
        let states = [BranchState::Activating(ActivatingFrom::Attaching)];
        let expected = "[{\"slug\":\"Activating\",\"data\":\"Attaching\"}]";

        let actual = serde_json::to_string(&states).unwrap();

        assert_eq!(actual, expected);

        let parsed = serde_json::from_str::<Vec<BranchState>>(&actual).unwrap();

        assert_eq!(states.as_slice(), &parsed);
    }

    #[test]
    fn tenantstatus_activating_strum() {
        // tests added, because we use these for metrics
        let examples = [
            (line!(), BranchState::Attaching, "Attaching"),
            (
                line!(),
                BranchState::Activating(ActivatingFrom::Attaching),
                "Activating",
            ),
            (line!(), BranchState::Active, "Active"),
            (
                line!(),
                BranchState::Stopping { progress: None },
                "Stopping",
            ),
            (
                line!(),
                BranchState::Stopping {
                    progress: Some(completion::Barrier::default()),
                },
                "Stopping",
            ),
            (
                line!(),
                BranchState::Broken {
                    reason: "Example".into(),
                    backtrace: "Looooong backtrace".into(),
                },
                "Broken",
            ),
        ];

        for (line, rendered, expected) in examples {
            let actual: &'static str = rendered.into();
            assert_eq!(actual, expected, "example on {line}");
        }
    }

    #[test]
    fn test_image_compression_algorithm_parsing() {
        use ImageCompressionAlgorithm::*;
        let cases = [
            ("disabled", Disabled),
            ("zstd", Zstd { level: None }),
            ("zstd(18)", Zstd { level: Some(18) }),
            ("zstd(-3)", Zstd { level: Some(-3) }),
        ];

        for (display, expected) in cases {
            assert_eq!(
                ImageCompressionAlgorithm::from_str(display).unwrap(),
                expected,
                "parsing works"
            );
            assert_eq!(format!("{expected}"), display, "Display FromStr roundtrip");

            let ser = serde_json::to_string(&expected).expect("serialization");
            assert_eq!(
                serde_json::from_str::<ImageCompressionAlgorithm>(&ser).unwrap(),
                expected,
                "serde roundtrip"
            );

            assert_eq!(
                serde_json::Value::String(display.to_string()),
                serde_json::to_value(expected).unwrap(),
                "Display is the serde serialization"
            );
        }
    }

    #[test]
    fn test_payload_compression_parsing() {
        use PayloadCompression::*;
        for (display, expected) in [("none", None), ("lz4", Lz4)] {
            assert_eq!(
                PayloadCompression::from_str(display).unwrap(),
                expected,
                "parsing works"
            );
            assert_eq!(format!("{expected}"), display, "Display FromStr roundtrip");

            let ser = serde_json::to_string(&expected).expect("serialization");
            assert_eq!(
                serde_json::from_str::<PayloadCompression>(&ser).unwrap(),
                expected,
                "serde roundtrip"
            );
            assert_eq!(
                serde_json::Value::String(display.to_string()),
                serde_json::to_value(expected).unwrap(),
                "Display is the serde serialization"
            );
        }

        // Unknown settings must fail loudly at config load rather than
        // silently degrading to a default: an operator who typos `lz4` wants
        // a startup error, not uncompressed writes.
        assert!(PayloadCompression::from_str("zstd").is_err());
        assert!(PayloadCompression::from_str("LZ4").is_err());
        assert!(PayloadCompression::from_str("").is_err());
    }

    #[test]
    fn test_branch_config_patch_serde_and_apply() {
        let config = BranchConfigPatch {
            gc_horizon: FieldPatch::Remove,
            pitr_interval: FieldPatch::Upsert("30 days".to_string()),
            ..BranchConfigPatch::default()
        };

        let json = serde_json::to_string(&config).unwrap();
        let expected = r#"{"gc_horizon":null,"pitr_interval":"30 days"}"#;
        assert_eq!(json, expected);

        let decoded: BranchConfigPatch = serde_json::from_str(&json).unwrap();
        assert_eq!(decoded, config);

        // Now apply the patch to a config to demonstrate semantics
        let base = BranchConfig {
            gc_horizon: Some(100),
            pitr_interval: Some(Duration::from_secs(7 * 24 * 3600)),
            ..Default::default()
        };

        let expected = BranchConfig {
            gc_horizon: None,
            pitr_interval: Some(Duration::from_secs(30 * 24 * 3600)),
            ..Default::default()
        };

        let patched = base.apply_patch(decoded).unwrap();
        assert_eq!(patched, expected);
    }
}
