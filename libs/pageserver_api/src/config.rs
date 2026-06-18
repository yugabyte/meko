use camino::Utf8PathBuf;

#[cfg(test)]
mod tests;

use const_format::formatcp;
use utils::serde_percent::Percent;
pub const DEFAULT_PG_LISTEN_PORT: u16 = 64000;
pub const DEFAULT_PG_LISTEN_ADDR: &str = formatcp!("127.0.0.1:{DEFAULT_PG_LISTEN_PORT}");
pub const DEFAULT_HTTP_LISTEN_PORT: u16 = 9898;
pub const DEFAULT_HTTP_LISTEN_ADDR: &str = formatcp!("127.0.0.1:{DEFAULT_HTTP_LISTEN_PORT}");
// TODO: gRPC is disabled by default for now, but the port is used in yb_light_local.
pub const DEFAULT_GRPC_LISTEN_PORT: u16 = 51051; // storage-broker already uses 50051

use std::collections::HashMap;
use std::fmt::Display;
use std::num::{NonZeroU64, NonZeroUsize};
use std::str::FromStr;
use std::time::Duration;

use postgres_backend::AuthType;
use postgres_versioninfo::PgMajorVersion;
use remote_storage::RemoteStorageConfig;
use serde_with::serde_as;
use utils::id::{TenantId, TimelineId};
use utils::logging::LogFormat;
use utils::lsn::Lsn;

use crate::models::{ImageCompressionAlgorithm, LsnLease};

/// Default value for the `scale_factor` config knob. 1.0 means "leave
/// every scale-derived default at its historical value".
pub const DEFAULT_SCALE_FACTOR: f64 = 1.0;

fn default_scale_factor() -> f64 {
    DEFAULT_SCALE_FACTOR
}

/// Multiply a count-like base default by `scale_factor`, rounded and
/// clamped to at least 1. Use for pool sizes, worker counts, queue
/// depths — anything where 0 would deadlock the system.
pub fn scale_count(base: usize, scale_factor: f64) -> usize {
    let scaled = (base as f64 * scale_factor).round();
    if scaled < 1.0 { 1 } else { scaled as usize }
}

/// Multiply a byte-budget base default by `scale_factor`, rounded and
/// clamped to at least 1 byte. Use for cache sizes, buffer caps,
/// memory budgets.
pub fn scale_bytes(base: u64, scale_factor: f64) -> u64 {
    let scaled = (base as f64 * scale_factor).round();
    if scaled < 1.0 { 1 } else { scaled as u64 }
}

// Certain metadata (e.g. externally-addressable name, AZ) is delivered
// as a separate structure.  This information is not needed by the pageserver
// itself, it is only used for registering the pageserver with the control
// plane and/or storage controller.
#[derive(PartialEq, Eq, Debug, serde::Serialize, serde::Deserialize)]
pub struct NodeMetadata {
    #[serde(rename = "host")]
    pub postgres_host: String,
    #[serde(rename = "port")]
    pub postgres_port: u16,
    pub grpc_host: Option<String>,
    pub grpc_port: Option<u16>,
    pub http_host: String,
    pub http_port: u16,
    pub https_port: Option<u16>,

    // Deployment tools may write fields to the metadata file beyond what we
    // use in this type: this type intentionally only names fields that require.
    #[serde(flatten)]
    pub other: HashMap<String, serde_json::Value>,
}

impl Display for NodeMetadata {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(
            f,
            "postgresql://{}:{} ",
            self.postgres_host, self.postgres_port
        )?;
        if let Some(grpc_host) = &self.grpc_host {
            let grpc_port = self.grpc_port.unwrap_or_default();
            write!(f, "grpc://{grpc_host}:{grpc_port} ")?;
        }
        write!(f, "http://{}:{} ", self.http_host, self.http_port)?;
        write!(f, "other:{:?}", self.other)?;
        Ok(())
    }
}

/// `pageserver.toml`
///
/// We use serde derive with `#[serde(default)]` to generate a deserializer
/// that fills in the default values for each config field.
///
/// If there cannot be a static default value because we need to make runtime
/// checks to determine the default, make it an `Option` (which defaults to None).
/// The runtime check should be done in the consuming crate, i.e., `pageserver`.
///
/// Unknown fields are silently ignored during deserialization.
/// The alternative, which we used in the past, was to set `deny_unknown_fields`,
/// which fails deserialization, and hence pageserver startup, if there is an unknown field.
/// The reason we don't do that anymore is that it complicates
/// usage of config fields for feature flagging, which we commonly do for
/// region-by-region rollouts.
/// The complications mainly arise because the `pageserver.toml` contents on a
/// prod server have a separate lifecycle from the pageserver binary.
/// For instance, `pageserver.toml` contents today are defined in the internal
/// infra repo, and thus introducing a new config field to pageserver and
/// rolling it out to prod servers are separate commits in separate repos
/// that can't be made or rolled back atomically.
/// Rollbacks in particular pose a risk with deny_unknown_fields because
/// the old pageserver binary may reject a new config field, resulting in
/// an outage unless the person doing the pageserver rollback remembers
/// to also revert the commit that added the config field in to the
/// `pageserver.toml` templates in the internal infra repo.
/// (A pre-deploy config check would eliminate this risk during rollbacks,
///  cf [here](https://github.com/neondatabase/cloud/issues/24349).)
/// In addition to this compatibility problem during emergency rollbacks,
/// deny_unknown_fields adds further complications when decomissioning a feature
/// flag: with deny_unknown_fields, we can't remove a flag from the [`ConfigToml`]
/// until all prod servers' `pageserver.toml` files have been updated to a version
/// that doesn't specify the flag. Otherwise new software would fail to start up.
/// This adds the requirement for an intermediate step where the new config field
/// is accepted but ignored, prolonging the decomissioning process by an entire
/// release cycle.
/// By contrast  with unknown fields silently ignored, decomissioning a feature
/// flag is a one-step process: we can skip the intermediate step and straight
/// remove the field from the [`ConfigToml`]. We leave the field in the
/// `pageserver.toml` files on prod servers until we reach certainty that we
/// will not roll back to old software whose behavior was dependent on config.
/// Then we can remove the field from the templates in the internal infra repo.
/// This process is [documented internally](
/// https://docs.neon.build/storage/pageserver_configuration.html).
///
/// yb-light note: although `ConfigToml` itself still ignores unknown fields at
/// the serde layer (this struct has no `deny_unknown_fields`), the yb-light
/// `yb-pageserver` binary treats any ignored field as fatal and refuses to
/// start (see `bin/yb-pageserver.rs`, which diffs the on-disk TOML against the
/// re-serialized config). The upstream rollout/rollback rationale above does
/// not apply here: yb-light's `pageserver.toml` is launcher-generated against
/// the same build, so a stray key is an operator mistake, not a fleet-skew
/// feature flag. Consequence: do not add a field with a value-dependent
/// `skip_serializing_if` anywhere reachable from `ConfigToml` — it would
/// round-trip-drop at its default value and be misreported as unknown.
///
/// Note that above relaxed compatbility for the config format does NOT APPLY
/// TO THE STORAGE FORMAT. As general guidance, when introducing storage format
/// changes, ensure that the potential rollback target version will be compatible
/// with the new format. This must hold regardless of what flags are set in in the `pageserver.toml`:
/// any format version that exists in an environment must be compatible with the software that runs there.
/// Use a pageserver.toml flag only to gate whether software _writes_ the new format.
/// For more compatibility considerations, refer to [internal docs](
/// https://docs.neon.build/storage/compat.html?highlight=compat#format-versions--compatibility)
#[serde_as]
#[derive(Clone, Debug, serde::Deserialize, serde::Serialize)]
#[serde(default)]
pub struct ConfigToml {
    /// Single-timeline identity: the one tenant this pageserver
    /// process serves for its entire lifetime. Set by the launcher
    /// (neon_local / storage_controller) before spawning the process.
    /// All three of `tenant_id` / `timeline_id` / `generation` must be
    /// present together (validated in `PageServerConf::parse_and_validate`).
    #[serde(skip_serializing_if = "Option::is_none")]
    pub tenant_id: Option<TenantId>,
    /// The one timeline this process serves. See [`Self::tenant_id`].
    #[serde(skip_serializing_if = "Option::is_none")]
    pub timeline_id: Option<TimelineId>,
    /// Generation number for this attachment. The launcher bumps the
    /// generation in `storage_controller.tenants` on every spawn and
    /// writes the fresh value here; the pageserver CAS-installs it on
    /// the `pageserver.timeline_state` row at startup
    /// (`cas_bump_generation_on_attach`), which fences out any stale
    /// process still holding an older generation.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub generation: Option<u32>,
    /// How to create the timeline if its `pageserver.timeline_state`
    /// row does not exist yet. Consulted ONLY when the row is absent;
    /// an existing row is always opened as-is (which makes restarts
    /// trivially idempotent). Absent row + absent `[timeline_init]`
    /// is a fatal startup error.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub timeline_init: Option<TimelineInitConfig>,

    // types mapped 1:1 into the runtime PageServerConfig type
    pub listen_pg_addr: String,
    pub listen_http_addr: String,
    pub listen_https_addr: Option<String>,
    pub listen_grpc_addr: Option<String>,
    pub ssl_key_file: Utf8PathBuf,
    pub ssl_cert_file: Utf8PathBuf,
    #[serde(with = "humantime_serde")]
    pub ssl_cert_reload_period: Duration,
    pub ssl_ca_file: Option<Utf8PathBuf>,
    #[serde(with = "humantime_serde")]
    pub wait_lsn_timeout: Duration,
    #[serde(with = "humantime_serde")]
    pub wal_redo_timeout: Duration,
    pub superuser: String,
    pub locale: String,
    pub page_cache_size: usize,
    pub pg_distrib_dir: Option<Utf8PathBuf>,
    #[serde_as(as = "serde_with::DisplayFromStr")]
    pub http_auth_type: AuthType,
    #[serde_as(as = "serde_with::DisplayFromStr")]
    pub pg_auth_type: AuthType,
    pub grpc_auth_type: AuthType,
    pub auth_validation_public_key_path: Option<Utf8PathBuf>,
    pub yb_storage_conn: Option<RemoteStorageConfig>,
    pub tenant_config: TenantConfigToml,
    /// libpq connection string of the single safekeeper, e.g.
    /// `host=127.0.0.1 port=5484`. The walreceiver opens its WAL
    /// streaming connection against this address.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub safekeeper_pg_addr: Option<String>,
    #[serde_as(as = "serde_with::DisplayFromStr")]
    pub log_format: LogFormat,
    #[serde(with = "humantime_serde")]
    pub synthetic_size_calculation_interval: Duration,
    pub disk_usage_based_eviction: DiskUsageEvictionTaskConfig,
    pub test_remote_failures: u64,
    pub test_remote_failures_probability: u64,
    pub ondemand_download_behavior_treat_error_as_warn: bool,
    #[serde(with = "humantime_serde")]
    pub background_task_maximum_delay: Duration,
    pub control_plane_api: Option<reqwest::Url>,
    pub control_plane_api_token: Option<String>,
    /// Unstable feature: subject to change or removal without notice.
    /// See <https://github.com/neondatabase/neon/pull/9218>.
    pub import_pgdata_upcall_api: Option<reqwest::Url>,
    /// Unstable feature: subject to change or removal without notice.
    /// See <https://github.com/neondatabase/neon/pull/9218>.
    pub import_pgdata_upcall_api_token: Option<String>,
    /// Unstable feature: subject to change or removal without notice.
    /// See <https://github.com/neondatabase/neon/pull/9218>.
    pub import_pgdata_aws_endpoint_url: Option<reqwest::Url>,
    pub heatmap_upload_concurrency: usize,
    pub secondary_download_concurrency: usize,
    pub ingest_batch_size: u64,
    pub max_vectored_read_bytes: MaxVectoredReadBytes,
    pub max_get_vectored_keys: MaxGetVectoredKeys,
    pub image_compression: ImageCompressionAlgorithm,
    pub timeline_offloading: bool,
    pub ephemeral_bytes_per_memory_kb: usize,
    pub l0_flush: Option<crate::models::L0FlushConfig>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub no_sync: Option<bool>,
    pub page_service_pipelining: PageServicePipeliningConfig,
    pub get_vectored_concurrent_io: GetVectoredConcurrentIo,
    pub enable_read_path_debugging: Option<bool>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub validate_wal_contiguity: Option<bool>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub load_previous_heatmap: Option<bool>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub generate_unarchival_heatmap: Option<bool>,
    pub tracing: Option<Tracing>,
    pub enable_tls_page_service_api: bool,
    pub dev_mode: bool,
    pub timeline_import_config: TimelineImportConfig,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub basebackup_cache_config: Option<BasebackupCacheConfig>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub image_layer_generation_large_timeline_threshold: Option<u64>,
    pub force_metric_collection_on_scrape: bool,

    /// Maximum permitted size of a single on-disk layer file before
    /// `batch_split_writer` rolls to a new file. Originally a hard
    /// 4.5 GB constant (the legacy single-PUT S3 upload cap; any
    /// modern S3 client streams larger objects via multipart). Exposed
    /// here so operators on stores without that cap (YB, GCS, local
    /// disk, multipart-S3) can lift the per-file ceiling without
    /// recompiling. Counted in bytes; default 4_500_000_000 preserves
    /// the historical value.
    pub s3_upload_limit_bytes: u64,

    /// yb-pageserver row-per-page writer tunables. The upstream
    /// `pageserver` binary ignores these — they only take effect when
    /// `yb_storage_conn.kind = "YbStore"` and the binary is
    /// `yb-pageserver`.
    #[serde(default)]
    pub yb_page_writer: YbPageWriterConfigToml,

    /// Global multiplier applied to scale-derived defaults across the
    /// pageserver: YSQL connection pool sizes, tokio worker threads,
    /// in-flight materialiser concurrency, `YbPageWriterConfig` memory
    /// budgets, etc. 1.0 preserves the historical sizing; 0.25 fits a
    /// small dev box; values >1 scale up for beefy hosts. Fields that
    /// are explicitly set in `pageserver.toml` always win over the
    /// scaled default.
    #[serde(default = "default_scale_factor")]
    pub scale_factor: f64,
}

/// On-disk shape of `[timeline_init]` in `pageserver.toml`: how to
/// create the configured timeline when its `pageserver.timeline_state`
/// row does not exist yet. See [`ConfigToml::timeline_init`].
///
/// ```toml
/// [timeline_init]
/// mode       = "bootstrap"
/// pg_version = 17
/// ```
/// or
/// ```toml
/// [timeline_init]
/// mode                 = "branch"
/// ancestor_timeline_id = "77ffde89f43d4ca08ef0a0ed1f089eaa"
/// ancestor_lsn         = "0/169AD58"
/// ```
#[serde_as]
#[derive(Debug, Clone, PartialEq, Eq, serde::Serialize, serde::Deserialize)]
#[serde(tag = "mode", rename_all = "snake_case")]
pub enum TimelineInitConfig {
    /// Run initdb and import the resulting pgdata as the timeline's
    /// initial page set.
    Bootstrap { pg_version: PgMajorVersion },
    /// Create a branch: write a `timeline_state` row pointing at the
    /// ancestor at `ancestor_lsn`. Pre-branch pages stay under the
    /// ancestor's timeline_id and are reached via lineage-aware reads.
    Branch {
        ancestor_timeline_id: TimelineId,
        #[serde_as(as = "serde_with::DisplayFromStr")]
        ancestor_lsn: Lsn,
    },
}

/// On-disk shape of `[yb_page_writer]` in `pageserver.toml`. Every
/// field is optional so we can tell "absent" (apply the scaled
/// default) apart from "explicitly set" (use the toml value verbatim,
/// `scale_factor` does not touch it). Resolved into [`YbPageWriterConfig`]
/// at startup by [`YbPageWriterConfigToml::resolve`].
#[derive(Debug, Clone, Default, PartialEq, Eq, serde::Serialize, serde::Deserialize)]
#[serde(default)]
pub struct YbPageWriterConfigToml {
    #[serde(
        with = "humantime_serde::option",
        skip_serializing_if = "Option::is_none"
    )]
    pub flush_interval: Option<Duration>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub flush_size_bytes: Option<usize>,
    #[serde(
        with = "humantime_serde::option",
        skip_serializing_if = "Option::is_none"
    )]
    pub flush_poll_interval: Option<Duration>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub image_chain_limit: Option<u32>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub image_chain_limit_slru: Option<u32>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub image_chain_payload_bytes_limit: Option<u32>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub materialize_queue_capacity: Option<usize>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub read_cache_bytes: Option<usize>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub backpressure_bytes: Option<usize>,
    /// Maximum entries in the in-process LRU cache for `pageserver.meta`
    /// point reads. 0 disables the cache. Default 8192.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub meta_cache_entries: Option<usize>,
}

#[derive(Debug, Clone, PartialEq, Eq, serde::Serialize)]
pub struct YbPageWriterConfig {
    /// Flush the in-RAM buffer to `pageserver.pages` after this much
    /// wall-clock time since the buffer opened, even if it hasn't hit
    /// the size threshold. Default 5 seconds.
    #[serde(with = "humantime_serde")]
    pub flush_interval: Duration,

    /// Flush the in-RAM buffer when its estimated size crosses this.
    /// Default 25 MiB (scaled by `scale_factor`).
    pub flush_size_bytes: usize,

    /// Period at which the background flusher polls `should_flush()`.
    /// Default 500 ms.
    #[serde(with = "humantime_serde")]
    pub flush_poll_interval: Duration,

    /// When a relation-page key accumulates this many consecutive
    /// non-image, non-`will_init` deltas, queue it for the background
    /// image materialiser. The materialiser walredos the chain at the
    /// triggering LSN and INSERTs a fresh `is_image=TRUE, will_init=TRUE`
    /// row, so subsequent reads land on the fast-path CTE seek instead
    /// of walking the chain back to the latest `will_init` delta.
    /// Set to 0 to disable this count-based trigger (the
    /// `image_chain_payload_bytes_limit` trigger remains in effect).
    /// Setting both to 0 disables the materialiser entirely. Default 20.
    pub image_chain_limit: u32,

    /// Like `image_chain_limit`, but for SLRU (clog / multixact) block
    /// keys. The clog page takes one tiny delta per transaction commit,
    /// so its chain grows orders of magnitude faster than a relation
    /// page's — but each delta is only a couple of bytes, so a much
    /// higher threshold keeps reconstruction (and basebackup, which
    /// rebuilds the active clog page) bounded without materialising
    /// trivially often. Without it SLRU pages were never materialised
    /// and basebackup time scaled with commit volume (chains reached
    /// the full ~32 K deltas/page). 0 disables the SLRU trigger.
    /// Default 1000 (caps clog reconstruction at ~1/30th of a full
    /// page). The payload-bytes trigger does not apply to SLRU.
    pub image_chain_limit_slru: u32,

    /// Materialiser trigger by combined-delta-payload size: when the
    /// sum of non-anchor delta payloads since the last anchor for a
    /// key exceeds this many bytes, dispatch the materialiser even
    /// if `image_chain_limit` hasn't been reached. Catches the case
    /// of a few large deltas (heap rewrites, near-full pages) that
    /// already represent a lot of walredo work to apply on read.
    /// Set to 0 to disable. Default 4 KiB.
    pub image_chain_payload_bytes_limit: u32,

    /// Bound on in-flight materialisation jobs. The write-side trigger
    /// awaits send (backpressures the walreceiver if the materialiser
    /// can't keep up — preferred over silent drops, which leak deep
    /// chains into the steady state). The read-side trigger uses
    /// `try_send` (drop OK — the next read of the same key re-evaluates).
    ///
    /// Each slot now carries a captured durable chain (see
    /// `MaterializeJob.chain` in `page_writer.rs`), so per-slot RAM is
    /// ~11 KB rather than ~100 B — but payload `Bytes` are
    /// refcount-shared with `UnifiedPages`, so marginal heap is
    /// dominated by chain entries the cache evicts while the job
    /// is in flight (small-MB scale).
    ///
    /// Sized for **burst absorption**, not sustained overload: at the
    /// materialiser's ~150 K rows/s INSERT-side drain rate (pool=4 +
    /// `MAX_INFLIGHT_INSERTS=4`), 2000 slots drain in ~13 ms. So
    /// 2000 only matters if the walreceiver enqueues >1000 jobs in
    /// <7 ms — the steady-state bottleneck (if any) is walredo or
    /// pool-slot contention, not the queue depth. Default 2000.
    pub materialize_queue_capacity: usize,

    /// Soft cap on the in-RAM `UnifiedPages` cache (pre-flush buffer +
    /// post-flush LRU-ish cache). When `total_bytes` exceeds this,
    /// durable entries are evicted FIFO. Pre-flush rows are pinned
    /// regardless, so the cache can transiently exceed this when the
    /// walreceiver is way ahead of the consumer. Default 1 GiB.
    pub read_cache_bytes: usize,

    /// Hard ceiling on `unflushed_bytes + bytes_in_aux_buffers` before
    /// `PageWriter::put` blocks waiting on a flush. Bounds RSS growth
    /// when YB INSERTs fall behind the walreceiver: ingest stalls
    /// here, the safekeeper holds the WAL, and the pageserver heap
    /// stays small. 0 disables backpressure (legacy unbounded
    /// growth). Should be ≥ `flush_size_bytes` so the size-based
    /// flush trigger fires first; recommended 2× `flush_size_bytes`.
    /// Default 50 MiB.
    pub backpressure_bytes: usize,

    /// Maximum entries in the in-process LRU cache for `pageserver.meta`
    /// point reads. Writes populate the cache (write-through) so a read
    /// at any LSN ≥ the write LSN hits the cache without a YB round-trip.
    /// 0 disables the cache. Default 8192.
    pub meta_cache_entries: usize,
}

impl YbPageWriterConfig {
    /// Base defaults at `scale_factor = 1.0`. Used by tests and as
    /// the input to [`YbPageWriterConfigToml::resolve`].
    pub fn unscaled_defaults() -> Self {
        Self {
            flush_interval: Duration::from_secs(5),
            flush_size_bytes: 25 * 1024 * 1024,
            flush_poll_interval: Duration::from_millis(500),
            image_chain_limit: 20,
            image_chain_limit_slru: 1000,
            image_chain_payload_bytes_limit: 4096,
            materialize_queue_capacity: 2000,
            read_cache_bytes: 1024 * 1024 * 1024,
            backpressure_bytes: 50 * 1024 * 1024,
            meta_cache_entries: 8192,
        }
    }
}

impl Default for YbPageWriterConfig {
    fn default() -> Self {
        Self::unscaled_defaults()
    }
}

impl YbPageWriterConfigToml {
    /// Fill in unset fields with scale-aware defaults. Time/threshold
    /// fields use the base default verbatim; memory and queue budgets
    /// are multiplied by `scale_factor`.
    pub fn resolve(&self, scale_factor: f64) -> YbPageWriterConfig {
        let base = YbPageWriterConfig::unscaled_defaults();
        YbPageWriterConfig {
            flush_interval: self.flush_interval.unwrap_or(base.flush_interval),
            flush_size_bytes: self
                .flush_size_bytes
                .unwrap_or_else(|| scale_count(base.flush_size_bytes, scale_factor)),
            flush_poll_interval: self.flush_poll_interval.unwrap_or(base.flush_poll_interval),
            image_chain_limit: self.image_chain_limit.unwrap_or(base.image_chain_limit),
            image_chain_limit_slru: self
                .image_chain_limit_slru
                .unwrap_or(base.image_chain_limit_slru),
            image_chain_payload_bytes_limit: self
                .image_chain_payload_bytes_limit
                .unwrap_or(base.image_chain_payload_bytes_limit),
            materialize_queue_capacity: self
                .materialize_queue_capacity
                .unwrap_or_else(|| scale_count(base.materialize_queue_capacity, scale_factor)),
            read_cache_bytes: self
                .read_cache_bytes
                .unwrap_or_else(|| scale_count(base.read_cache_bytes, scale_factor)),
            backpressure_bytes: self
                .backpressure_bytes
                .unwrap_or_else(|| scale_count(base.backpressure_bytes, scale_factor)),
            meta_cache_entries: self.meta_cache_entries.unwrap_or(base.meta_cache_entries),
        }
    }
}

#[derive(Debug, Clone, PartialEq, Eq, serde::Serialize, serde::Deserialize)]
#[serde(default)]
pub struct DiskUsageEvictionTaskConfig {
    pub max_usage_pct: utils::serde_percent::Percent,
    pub min_avail_bytes: u64,
    #[serde(with = "humantime_serde")]
    pub period: Duration,
    #[cfg(feature = "testing")]
    pub mock_statvfs: Option<statvfs::mock::Behavior>,
    /// Select sorting for evicted layers
    #[serde(default)]
    pub eviction_order: EvictionOrder,
    pub enabled: bool,
}

impl Default for DiskUsageEvictionTaskConfig {
    fn default() -> Self {
        Self {
            max_usage_pct: Percent::new(80).unwrap(),
            min_avail_bytes: 2_000_000_000,
            period: Duration::from_secs(60),
            #[cfg(feature = "testing")]
            mock_statvfs: None,
            eviction_order: EvictionOrder::default(),
            enabled: true,
        }
    }
}

#[derive(Debug, Clone, PartialEq, Eq, serde::Serialize, serde::Deserialize)]
#[serde(tag = "mode", rename_all = "kebab-case")]
pub enum PageServicePipeliningConfig {
    Serial,
    Pipelined(PageServicePipeliningConfigPipelined),
}
#[derive(Debug, Clone, PartialEq, Eq, serde::Serialize, serde::Deserialize)]
pub struct PageServicePipeliningConfigPipelined {
    /// Failed config parsing and validation if larger than `max_get_vectored_keys`.
    pub max_batch_size: NonZeroUsize,
    pub execution: PageServiceProtocolPipelinedExecutionStrategy,
    // The default below is such that new versions of the software can start
    // with the old configuration.
    #[serde(default)]
    pub batching: PageServiceProtocolPipelinedBatchingStrategy,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, serde::Serialize, serde::Deserialize)]
#[serde(rename_all = "kebab-case")]
pub enum PageServiceProtocolPipelinedExecutionStrategy {
    ConcurrentFutures,
    Tasks,
}

#[derive(Default, Debug, Clone, Copy, PartialEq, Eq, serde::Serialize, serde::Deserialize)]
#[serde(rename_all = "kebab-case")]
pub enum PageServiceProtocolPipelinedBatchingStrategy {
    /// All get page requests in a batch will be at the same LSN
    #[default]
    UniformLsn,
    /// Get page requests in a batch may be at different LSN
    ///
    /// One key cannot be present more than once at different LSNs in
    /// the same batch.
    ScatteredLsn,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, serde::Serialize, serde::Deserialize)]
#[serde(tag = "mode", rename_all = "kebab-case")]
pub enum GetVectoredConcurrentIo {
    /// The read path is fully sequential: layers are visited
    /// one after the other and IOs are issued and waited upon
    /// from the same task that traverses the layers.
    Sequential,
    /// The read path still traverses layers sequentially, and
    /// index blocks will be read into the PS PageCache from
    /// that task, with waiting.
    /// But data IOs are dispatched and waited upon from a sidecar
    /// task so that the traversing task can continue to traverse
    /// layers while the IOs are in flight.
    /// If the PS PageCache miss rate is low, this improves
    /// throughput dramatically.
    SidecarTask,
}

#[derive(Debug, Copy, Clone, PartialEq, Eq, serde::Serialize, serde::Deserialize)]
pub struct Ratio {
    pub numerator: usize,
    pub denominator: usize,
}

#[derive(Debug, Clone, PartialEq, Eq, serde::Serialize, serde::Deserialize)]
pub struct OtelExporterConfig {
    pub endpoint: String,
    pub protocol: OtelExporterProtocol,
    #[serde(with = "humantime_serde")]
    pub timeout: Duration,
}

#[derive(Debug, Copy, Clone, PartialEq, Eq, serde::Serialize, serde::Deserialize)]
#[serde(rename_all = "kebab-case")]
pub enum OtelExporterProtocol {
    Grpc,
    HttpBinary,
    HttpJson,
}

#[derive(Debug, Clone, PartialEq, Eq, serde::Serialize, serde::Deserialize)]
pub struct Tracing {
    pub sampling_ratio: Ratio,
    pub export_config: OtelExporterConfig,
}

impl From<&OtelExporterConfig> for tracing_utils::ExportConfig {
    fn from(val: &OtelExporterConfig) -> Self {
        tracing_utils::ExportConfig {
            endpoint: Some(val.endpoint.clone()),
            protocol: val.protocol.into(),
            timeout: Some(val.timeout),
        }
    }
}

impl From<OtelExporterProtocol> for tracing_utils::Protocol {
    fn from(val: OtelExporterProtocol) -> Self {
        match val {
            OtelExporterProtocol::Grpc => tracing_utils::Protocol::Grpc,
            OtelExporterProtocol::HttpJson => tracing_utils::Protocol::HttpJson,
            OtelExporterProtocol::HttpBinary => tracing_utils::Protocol::HttpBinary,
        }
    }
}

#[derive(Debug, Clone, PartialEq, Eq, serde::Serialize, serde::Deserialize)]
pub struct TimelineImportConfig {
    pub import_job_concurrency: NonZeroUsize,
    pub import_job_soft_size_limit: NonZeroUsize,
    pub import_job_checkpoint_threshold: NonZeroUsize,
    /// Max size of the remote storage partial read done by any job
    pub import_job_max_byte_range_size: NonZeroUsize,
}

#[derive(Debug, Clone, PartialEq, Eq, serde::Serialize, serde::Deserialize)]
#[serde(default)]
pub struct BasebackupCacheConfig {
    #[serde(with = "humantime_serde")]
    pub cleanup_period: Duration,
    /// Maximum total size of basebackup cache entries on disk in bytes.
    /// The cache may slightly exceed this limit because we do not know
    /// the exact size of the cache entry untill it's written to disk.
    pub max_total_size_bytes: u64,
    // TODO(diko): support max_entry_size_bytes.
    // pub max_entry_size_bytes: u64,
    pub max_size_entries: usize,
    /// Size of the channel used to send prepare requests to the basebackup cache worker.
    /// If exceeded, new prepare requests will be dropped.
    pub prepare_channel_size: usize,
}

impl Default for BasebackupCacheConfig {
    fn default() -> Self {
        Self {
            cleanup_period: Duration::from_secs(60),
            max_total_size_bytes: 1024 * 1024 * 1024, // 1 GiB
            // max_entry_size_bytes: 16 * 1024 * 1024,   // 16 MiB
            max_size_entries: 10000,
            prepare_channel_size: 100,
        }
    }
}

pub mod statvfs {
    pub mod mock {
        #[derive(Debug, Clone, PartialEq, Eq, serde::Serialize, serde::Deserialize)]
        #[serde(tag = "type")]
        pub enum Behavior {
            Success {
                blocksize: u64,
                total_blocks: u64,
                name_filter: Option<utils::serde_regex::Regex>,
            },
            #[cfg(feature = "testing")]
            Failure { mocked_error: MockedError },
        }

        #[cfg(feature = "testing")]
        #[derive(Debug, Clone, Copy, PartialEq, Eq, serde::Serialize, serde::Deserialize)]
        #[allow(clippy::upper_case_acronyms)]
        pub enum MockedError {
            EIO,
        }

        #[cfg(feature = "testing")]
        impl From<MockedError> for nix::Error {
            fn from(e: MockedError) -> Self {
                match e {
                    MockedError::EIO => nix::Error::EIO,
                }
            }
        }
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, serde::Serialize, serde::Deserialize)]
#[serde(tag = "type", content = "args")]
pub enum EvictionOrder {
    RelativeAccessed {
        highest_layer_count_loses_first: bool,
    },
}

impl Default for EvictionOrder {
    fn default() -> Self {
        Self::RelativeAccessed {
            highest_layer_count_loses_first: true,
        }
    }
}

#[derive(Copy, Clone, Debug, PartialEq, Eq, serde::Serialize, serde::Deserialize)]
#[serde(transparent)]
pub struct MaxVectoredReadBytes(pub NonZeroUsize);

#[derive(Copy, Clone, Debug, PartialEq, Eq, serde::Serialize, serde::Deserialize)]
#[serde(transparent)]
pub struct MaxGetVectoredKeys(NonZeroUsize);

impl MaxGetVectoredKeys {
    pub fn get(&self) -> usize {
        self.0.get()
    }
}

pub mod defaults {
    use crate::models::ImageCompressionAlgorithm;

    pub const DEFAULT_WAIT_LSN_TIMEOUT: &str = "300 s";
    pub const DEFAULT_WAL_REDO_TIMEOUT: &str = "60 s";

    pub const DEFAULT_SUPERUSER: &str = "cloud_admin";
    pub const DEFAULT_LOCALE: &str = if cfg!(target_os = "macos") {
        "C"
    } else {
        "C.UTF-8"
    };

    pub const DEFAULT_PAGE_CACHE_SIZE: usize = 8192;

    pub const DEFAULT_LOG_FORMAT: &str = "plain";



    pub const DEFAULT_SYNTHETIC_SIZE_CALCULATION_INTERVAL: &str = "10 min";
    pub const DEFAULT_BACKGROUND_TASK_MAXIMUM_DELAY: &str = "10s";

    pub const DEFAULT_HEATMAP_UPLOAD_CONCURRENCY: usize = 8;
    pub const DEFAULT_SECONDARY_DOWNLOAD_CONCURRENCY: usize = 1;

    pub const DEFAULT_INGEST_BATCH_SIZE: u64 = 100;

    /// Soft limit for the maximum size of a vectored read.
    pub const DEFAULT_MAX_VECTORED_READ_BYTES: usize = 130 * 1024;

    pub const DEFAULT_MAX_GET_VECTORED_KEYS: usize = 32;

    pub const DEFAULT_IMAGE_COMPRESSION: ImageCompressionAlgorithm =
        ImageCompressionAlgorithm::Zstd { level: Some(1) };

    pub const DEFAULT_EPHEMERAL_BYTES_PER_MEMORY_KB: usize = 0;

    #[cfg(feature = "io-align-4k")]
    pub const DEFAULT_IO_BUFFER_ALIGNMENT: usize = 4096;
    #[cfg(all(feature = "io-align-512", not(feature = "io-align-4k")))]
    pub const DEFAULT_IO_BUFFER_ALIGNMENT: usize = 512;
    #[cfg(not(any(feature = "io-align-512", feature = "io-align-4k")))]
    pub const DEFAULT_IO_BUFFER_ALIGNMENT: usize = 512;

    pub const DEFAULT_SSL_KEY_FILE: &str = "server.key";
    pub const DEFAULT_SSL_CERT_FILE: &str = "server.crt";
}

impl Default for ConfigToml {
    fn default() -> Self {
        use defaults::*;

        Self {
            tenant_id: None,
            timeline_id: None,
            generation: None,
            timeline_init: None,
            listen_pg_addr: (DEFAULT_PG_LISTEN_ADDR.to_string()),
            listen_http_addr: (DEFAULT_HTTP_LISTEN_ADDR.to_string()),
            listen_https_addr: (None),
            listen_grpc_addr: None,
            ssl_key_file: Utf8PathBuf::from(DEFAULT_SSL_KEY_FILE),
            ssl_cert_file: Utf8PathBuf::from(DEFAULT_SSL_CERT_FILE),
            ssl_cert_reload_period: Duration::from_secs(60),
            ssl_ca_file: None,
            wait_lsn_timeout: (humantime::parse_duration(DEFAULT_WAIT_LSN_TIMEOUT)
                .expect("cannot parse default wait lsn timeout")),
            wal_redo_timeout: (humantime::parse_duration(DEFAULT_WAL_REDO_TIMEOUT)
                .expect("cannot parse default wal redo timeout")),
            superuser: (DEFAULT_SUPERUSER.to_string()),
            locale: DEFAULT_LOCALE.to_string(),
            page_cache_size: (DEFAULT_PAGE_CACHE_SIZE),
            pg_distrib_dir: None,
            http_auth_type: (AuthType::Trust),
            pg_auth_type: (AuthType::Trust),
            grpc_auth_type: (AuthType::Trust),
            auth_validation_public_key_path: (None),
            yb_storage_conn: None,
            safekeeper_pg_addr: None,
            log_format: (LogFormat::from_str(DEFAULT_LOG_FORMAT).unwrap()),

            synthetic_size_calculation_interval: (humantime::parse_duration(
                DEFAULT_SYNTHETIC_SIZE_CALCULATION_INTERVAL,
            )
            .expect("cannot parse default synthetic size calculation interval")),

            disk_usage_based_eviction: DiskUsageEvictionTaskConfig::default(),

            test_remote_failures: (0),
            test_remote_failures_probability: (100),

            ondemand_download_behavior_treat_error_as_warn: (false),

            background_task_maximum_delay: (humantime::parse_duration(
                DEFAULT_BACKGROUND_TASK_MAXIMUM_DELAY,
            )
            .unwrap()),

            control_plane_api: (None),
            control_plane_api_token: (None),

            import_pgdata_upcall_api: (None),
            import_pgdata_upcall_api_token: (None),
            import_pgdata_aws_endpoint_url: (None),

            heatmap_upload_concurrency: (DEFAULT_HEATMAP_UPLOAD_CONCURRENCY),
            secondary_download_concurrency: (DEFAULT_SECONDARY_DOWNLOAD_CONCURRENCY),

            ingest_batch_size: (DEFAULT_INGEST_BATCH_SIZE),

            max_vectored_read_bytes: (MaxVectoredReadBytes(
                NonZeroUsize::new(DEFAULT_MAX_VECTORED_READ_BYTES).unwrap(),
            )),
            max_get_vectored_keys: (MaxGetVectoredKeys(
                NonZeroUsize::new(DEFAULT_MAX_GET_VECTORED_KEYS).unwrap(),
            )),
            image_compression: (DEFAULT_IMAGE_COMPRESSION),
            timeline_offloading: true,
            ephemeral_bytes_per_memory_kb: (DEFAULT_EPHEMERAL_BYTES_PER_MEMORY_KB),
            l0_flush: None,
            tenant_config: TenantConfigToml::default(),
            no_sync: None,
            page_service_pipelining: PageServicePipeliningConfig::Pipelined(
                PageServicePipeliningConfigPipelined {
                    max_batch_size: NonZeroUsize::new(32).unwrap(),
                    execution: PageServiceProtocolPipelinedExecutionStrategy::ConcurrentFutures,
                    batching: PageServiceProtocolPipelinedBatchingStrategy::ScatteredLsn,
                },
            ),
            get_vectored_concurrent_io: GetVectoredConcurrentIo::SidecarTask,
            enable_read_path_debugging: if cfg!(feature = "testing") {
                Some(true)
            } else {
                None
            },
            validate_wal_contiguity: None,
            load_previous_heatmap: None,
            generate_unarchival_heatmap: None,
            tracing: None,
            enable_tls_page_service_api: false,
            dev_mode: false,
            timeline_import_config: TimelineImportConfig {
                import_job_concurrency: NonZeroUsize::new(32).unwrap(),
                import_job_soft_size_limit: NonZeroUsize::new(256 * 1024 * 1024).unwrap(),
                import_job_checkpoint_threshold: NonZeroUsize::new(32).unwrap(),
                import_job_max_byte_range_size: NonZeroUsize::new(4 * 1024 * 1024).unwrap(),
            },
            basebackup_cache_config: None,
            image_layer_generation_large_timeline_threshold: Some(2 * 1024 * 1024 * 1024),
            force_metric_collection_on_scrape: true,
            s3_upload_limit_bytes: 4_500_000_000,
            yb_page_writer: YbPageWriterConfigToml::default(),
            scale_factor: DEFAULT_SCALE_FACTOR,
        }
    }
}

/// Tenant-level configuration values, used for various purposes.
#[derive(Debug, Clone, PartialEq, Eq, serde::Serialize, serde::Deserialize)]
#[serde(default)]
pub struct TenantConfigToml {
    /// How much WAL history to retain for branching / replicas at an
    /// older LSN. Page versions older than this become eligible for
    /// garbage collection.
    pub gc_horizon: u64,
    /// Interval at which the background GC loop runs. `Duration::ZERO`
    /// disables automatic GC.
    #[serde(with = "humantime_serde")]
    pub gc_period: Duration,
    /// Time-based PITR retention. Page versions older than this are
    /// eligible for garbage collection.
    #[serde(with = "humantime_serde")]
    pub pitr_interval: Duration,
    /// Maximum time to wait while opening a connection to a
    /// safekeeper to receive WAL.
    #[serde(with = "humantime_serde")]
    pub walreceiver_connect_timeout: Duration,
    /// Considers safekeepers stalled after no WAL updates for longer
    /// than this; a stalled safekeeper is swapped for a newer one.
    #[serde(with = "humantime_serde")]
    pub lagging_wal_timeout: Duration,
    /// Considers safekeepers lagging when their WAL is behind another
    /// safekeeper by more than this; a lagging safekeeper is
    /// swapped after `lagging_wal_timeout` elapses without progress.
    pub max_lsn_wal_lag: NonZeroU64,
    /// If true SLRU segments are downloaded on demand; if false they
    /// are included in basebackup.
    pub lazy_slru_download: bool,
    /// Per-tenant pagestream getpage throttle config.
    pub timeline_get_throttle: crate::models::ThrottleConfig,
    /// LSN-lease length for explicit lease requests.
    #[serde(with = "humantime_serde")]
    pub lsn_lease_length: Duration,
    /// LSN-lease length for implicit `get_lsn_by_timestamp` leases.
    #[serde(with = "humantime_serde")]
    pub lsn_lease_length_for_ts: Duration,
    /// Enable rel_size_v2 for this tenant. Once on, the tenant
    /// persists this; the migration is one-way.
    pub rel_size_v2_enabled: bool,
    /// Sampling ratio for trace-collected page requests; `Some(None)`
    /// disables, `Some(Some(r))` enables with ratio `r`.
    pub sampling_ratio: Option<Ratio>,
    /// Capacity of the in-memory relsize snapshot cache.
    pub relsize_snapshot_cache_capacity: usize,
    /// Enable the basebackup cache for this tenant.
    pub basebackup_cache_enabled: bool,
}

pub mod tenant_conf_defaults {
    pub const DEFAULT_GC_HORIZON: u64 = 64 * 1024 * 1024;
    // Large DEFAULT_GC_PERIOD is fine as long as PITR_INTERVAL is larger.
    pub const DEFAULT_GC_PERIOD: &str = "1 hr";
    pub const DEFAULT_PITR_INTERVAL: &str = "7 days";
    pub const DEFAULT_WALRECEIVER_CONNECT_TIMEOUT: &str = "10 seconds";
    pub const DEFAULT_WALRECEIVER_LAGGING_WAL_TIMEOUT: &str = "10 seconds";
    // The default limit on WAL lag should be set to avoid causing
    // disconnects under high throughput: with broker stats updated
    // ~1/s, 1 GiB suffices for throughputs up to ~1 GiB/s per timeline.
    pub const DEFAULT_MAX_WALRECEIVER_LSN_WAL_LAG: u64 = 1024 * 1024 * 1024;
    pub const DEFAULT_RELSIZE_SNAPSHOT_CACHE_CAPACITY: usize = 1000;
}

impl Default for TenantConfigToml {
    fn default() -> Self {
        use tenant_conf_defaults::*;
        Self {
            gc_horizon: DEFAULT_GC_HORIZON,
            gc_period: humantime::parse_duration(DEFAULT_GC_PERIOD)
                .expect("cannot parse default gc period"),
            pitr_interval: humantime::parse_duration(DEFAULT_PITR_INTERVAL)
                .expect("cannot parse default PITR interval"),
            walreceiver_connect_timeout: humantime::parse_duration(
                DEFAULT_WALRECEIVER_CONNECT_TIMEOUT,
            )
            .expect("cannot parse default walreceiver connect timeout"),
            lagging_wal_timeout: humantime::parse_duration(DEFAULT_WALRECEIVER_LAGGING_WAL_TIMEOUT)
                .expect("cannot parse default walreceiver lagging wal timeout"),
            max_lsn_wal_lag: NonZeroU64::new(DEFAULT_MAX_WALRECEIVER_LSN_WAL_LAG)
                .expect("cannot parse default max walreceiver Lsn wal lag"),
            lazy_slru_download: false,
            timeline_get_throttle: crate::models::ThrottleConfig::disabled(),
            lsn_lease_length: LsnLease::DEFAULT_LENGTH,
            lsn_lease_length_for_ts: LsnLease::DEFAULT_LENGTH_FOR_TS,
            rel_size_v2_enabled: false,
            sampling_ratio: None,
            relsize_snapshot_cache_capacity: DEFAULT_RELSIZE_SNAPSHOT_CACHE_CAPACITY,
            basebackup_cache_enabled: false,
        }
    }
}
