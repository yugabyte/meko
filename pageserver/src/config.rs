//! Functions for handling page server configuration options
//!
//! Configuration options can be set in the pageserver.toml configuration
//! file, or on the command line.
//! See also `settings.md` for better description on every parameter.

pub mod ignored_fields;

use std::env;
use std::num::NonZeroUsize;
use std::sync::Arc;
use std::time::Duration;

use anyhow::{Context, ensure};
use camino::{Utf8Path, Utf8PathBuf};
use once_cell::sync::OnceCell;
use pageserver_api::config::{
    DiskUsageEvictionTaskConfig, MaxGetVectoredKeys, MaxVectoredReadBytes,
    PageServicePipeliningConfig, PageServicePipeliningConfigPipelined,
};
use pageserver_api::models::ImageCompressionAlgorithm;
use pem::Pem;
use postgres_backend::AuthType;
use postgres_ffi::PgMajorVersion;
use remote_storage::{RemotePath, RemoteStorageConfig};
use reqwest::Url;
use utils::generation::Generation;
use utils::id::{NodeId, TenantId, TimelineId};
use utils::logging::{LogFormat, SecretString};

use crate::page_server::{TENANTS_SEGMENT_NAME, TIMELINES_SEGMENT_NAME};

/// Global state of pageserver.
///
/// It's mostly immutable configuration, but some semaphores and the
/// like crept in over time and the name stuck.
///
/// Instantiated by deserializing `pageserver.toml` into  [`pageserver_api::config::ConfigToml`]
/// and passing that to [`PageServerConf::parse_and_validate`].
///
/// # Adding a New Field
///
/// 1. Add the field to `pageserver_api::config::ConfigToml`.
/// 2. Fix compiler errors (exhaustive destructuring will guide you).
///
/// For fields that require additional validation or filling in of defaults at runtime,
/// check for examples in the [`PageServerConf::parse_and_validate`] method.
/// The single (tenant, timeline) a pageserver process serves for its
/// entire lifetime, plus the generation granted by the launcher for
/// this attachment. Parsed from the `tenant_id` / `timeline_id` /
/// `generation` fields of `pageserver.toml` (all-or-nothing).
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct TimelineIdentity {
    pub tenant_id: TenantId,
    pub timeline_id: TimelineId,
    pub generation: Generation,
}

#[derive(Debug, Clone)]
pub struct PageServerConf {
    // Identifier of that particular pageserver so e g safekeepers
    // can safely distinguish different pageservers
    pub id: NodeId,

    /// yb-mode single-timeline identity. `None` only during the
    /// transition while the control plane still attaches tenants
    /// dynamically; once the single-timeline startup lands this is
    /// required and startup fails without it.
    pub timeline_identity: Option<TimelineIdentity>,

    /// How to create the timeline when its `pageserver.timeline_state`
    /// row is absent at startup. See
    /// [`pageserver_api::config::TimelineInitConfig`].
    pub timeline_init: Option<pageserver_api::config::TimelineInitConfig>,

    /// Example (default): 127.0.0.1:64000
    pub listen_pg_addr: String,
    /// Example (default): 127.0.0.1:9898
    pub listen_http_addr: String,
    /// Example: 127.0.0.1:9899
    pub listen_https_addr: Option<String>,
    /// If set, expose a gRPC API on this address.
    /// Example: 127.0.0.1:51051
    ///
    /// EXPERIMENTAL: this protocol is unstable and under active development.
    pub listen_grpc_addr: Option<String>,

    /// Path to a file with certificate's private key for https and gRPC API.
    /// Default: server.key
    pub ssl_key_file: Utf8PathBuf,
    /// Path to a file with a X509 certificate for https and gRPC API.
    /// Default: server.crt
    pub ssl_cert_file: Utf8PathBuf,
    /// Period to reload certificate and private key from files.
    /// Default: 60s.
    pub ssl_cert_reload_period: Duration,
    /// Trusted root CA certificates to use in https APIs in PEM format.
    pub ssl_ca_certs: Vec<Pem>,

    // Timeout when waiting for WAL receiver to catch up to an LSN given in a GetPage@LSN call.
    pub wait_lsn_timeout: Duration,
    // How long to wait for WAL redo to complete.
    pub wal_redo_timeout: Duration,

    /// Per-tenant walredo subprocess pool size. Base default 2,
    /// multiplied by `scale_factor`. Each slot is a separate
    /// long-lived postgres-walredo process; requests round-robin across
    /// them so a single tenant can replay multiple records in parallel.
    pub walredo_pool_size: NonZeroUsize,

    pub superuser: String,
    pub locale: String,

    // Repository directory, relative to current working directory.
    // Normally, the page server changes the current working directory
    // to the repository, and 'workdir' is always '.'. But we don't do
    // that during unit testing, because the current directory is global
    // to the process but different unit tests work on different
    // repositories.
    pub workdir: Utf8PathBuf,

    pub pg_distrib_dir: Utf8PathBuf,

    // Authentication
    /// authentication method for the HTTP mgmt API
    pub http_auth_type: AuthType,
    /// authentication method for libpq connections from compute
    pub pg_auth_type: AuthType,
    /// authentication method for gRPC connections from compute
    pub grpc_auth_type: AuthType,
    /// Path to a file or directory containing public key(s) for verifying JWT tokens.
    /// Used for both mgmt and compute auth, if enabled.
    pub auth_validation_public_key_path: Option<Utf8PathBuf>,

    pub remote_storage_config: Option<RemoteStorageConfig>,

    pub default_tenant_conf: pageserver_api::config::TenantConfigToml,

    /// libpq connection string of the single safekeeper that the
    /// walreceiver streams WAL from, e.g. `host=127.0.0.1 port=5484`.
    pub safekeeper_pg_addr: Option<String>,

    pub log_format: LogFormat,

    pub synthetic_size_calculation_interval: Duration,

    pub disk_usage_based_eviction: DiskUsageEvictionTaskConfig,

    // The number of allowed failures in remote storage operations.
    pub test_remote_failures: u64,
    // The probability of failure in remote storage operations. Only works when test_remote_failures > 1.
    // Use 100 for 100% failure, 0 for no failure.
    pub test_remote_failures_probability: u64,

    pub ondemand_download_behavior_treat_error_as_warn: bool,

    /// How long will background tasks be delayed at most after initial load of tenants.
    ///
    /// Our largest initialization completions are in the range of 100-200s, so perhaps 10s works
    /// as we now isolate initial loading, initial logical size calculation and background tasks.
    /// Smaller nodes will have background tasks "not running" for this long unless every timeline
    /// has it's initial logical size calculated. Not running background tasks for some seconds is
    /// not terrible.
    pub background_task_maximum_delay: Duration,

    pub control_plane_api: Url,

    /// JWT token for use with the control plane API.
    pub control_plane_api_token: Option<SecretString>,

    /// Maximum number of WAL records to be ingested and committed at the same time
    pub ingest_batch_size: u64,

    pub max_vectored_read_bytes: MaxVectoredReadBytes,

    /// Maximum number of keys to be read in a single get_vectored call.
    pub max_get_vectored_keys: MaxGetVectoredKeys,

    pub image_compression: ImageCompressionAlgorithm,

    /// Whether to offload archived timelines automatically
    pub timeline_offloading: bool,

    /// How many bytes of ephemeral layer content will we allow per kilobyte of RAM.  When this
    /// is exceeded, we start proactively closing ephemeral layers to limit the total amount
    /// of ephemeral data.
    ///
    /// Setting this to zero disables limits on total ephemeral layer size.
    pub ephemeral_bytes_per_memory_kb: usize,

    /// Optionally disable disk syncs (unsafe!)
    pub no_sync: bool,

    pub page_service_pipelining: pageserver_api::config::PageServicePipeliningConfig,

    pub get_vectored_concurrent_io: pageserver_api::config::GetVectoredConcurrentIo,

    /// Enable read path debugging. If enabled, read key errors will print a backtrace of the layer
    /// files read.
    pub enable_read_path_debugging: bool,

    /// Interpreted protocol feature: if enabled, validate that the logical WAL received from
    /// safekeepers does not have gaps.
    pub validate_wal_contiguity: bool,

    pub tracing: Option<pageserver_api::config::Tracing>,

    /// Enable TLS in page service API.
    /// Does not force TLS: the client negotiates TLS usage during the handshake.
    /// Uses key and certificate from ssl_key_file/ssl_cert_file.
    pub enable_tls_page_service_api: bool,

    /// Run in development mode, which disables certain safety checks
    /// such as authentication requirements for HTTP and PostgreSQL APIs.
    /// This is insecure and should only be used in development environments.
    pub dev_mode: bool,

    pub timeline_import_config: pageserver_api::config::TimelineImportConfig,

    pub basebackup_cache_config: Option<pageserver_api::config::BasebackupCacheConfig>,

    /// Defines what is a big tenant for the purpose of image layer generation.
    /// See Timeline::should_check_if_image_layers_required
    pub image_layer_generation_large_timeline_threshold: Option<u64>,

    /// Controls whether to collect all metrics on each scrape or to return potentially stale
    /// results.
    pub force_metric_collection_on_scrape: bool,

    /// Maximum permitted size of a single on-disk layer file.
    /// See `pageserver_api::config::ConfigToml::s3_upload_limit_bytes`.
    pub s3_upload_limit_bytes: u64,

    /// yb-pageserver row-per-page writer tunables.
    pub yb_page_writer: pageserver_api::config::YbPageWriterConfig,

    /// See `pageserver_api::config::ConfigToml::scale_factor`. The
    /// resolved (post-toml-merge) value is stashed here so binary
    /// startup can multiply count-shaped constants — pool sizes,
    /// PARALLELISM, `REDO_CONCURRENCY`, etc. — by it on the fly.
    pub scale_factor: f64,
}

/// Token for authentication to safekeepers
///
/// We do not want to store this in a PageServerConf because the latter may be logged
/// and/or serialized at a whim, while the token is secret. Currently this token is the
/// same for accessing all tenants/timelines, but may become per-tenant/per-timeline in
/// the future, more tokens and auth may arrive for storage broker, completely changing the logic.
/// Hence, we resort to a global variable for now instead of passing the token from the
/// startup code to the connection code through a dozen layers.
pub static SAFEKEEPER_AUTH_TOKEN: OnceCell<Arc<String>> = OnceCell::new();

impl PageServerConf {
    //
    // Repository paths, relative to workdir.
    //

    pub fn tenants_path(&self) -> Utf8PathBuf {
        self.workdir.join(TENANTS_SEGMENT_NAME)
    }

    pub fn deletion_prefix(&self) -> Utf8PathBuf {
        self.workdir.join("deletion")
    }

    pub fn metadata_path(&self) -> Utf8PathBuf {
        self.workdir.join("metadata.json")
    }

    pub fn basebackup_cache_dir(&self) -> Utf8PathBuf {
        self.workdir.join("basebackup_cache")
    }

    pub fn deletion_list_path(&self, sequence: u64) -> Utf8PathBuf {
        // Encode a version in the filename, so that if we ever switch away from JSON we can
        // increment this.
        const VERSION: u8 = 1;

        self.deletion_prefix()
            .join(format!("{sequence:016x}-{VERSION:02x}.list"))
    }

    pub fn deletion_header_path(&self) -> Utf8PathBuf {
        // Encode a version in the filename, so that if we ever switch away from JSON we can
        // increment this.
        const VERSION: u8 = 1;

        self.deletion_prefix().join(format!("header-{VERSION:02x}"))
    }

    pub fn tenant_path(&self, tenant_shard_id: &TenantId) -> Utf8PathBuf {
        self.tenants_path().join(tenant_shard_id.to_string())
    }

    pub fn timelines_path(&self, tenant_shard_id: &TenantId) -> Utf8PathBuf {
        self.tenant_path(tenant_shard_id)
            .join(TIMELINES_SEGMENT_NAME)
    }

    pub fn timeline_path(
        &self,
        tenant_shard_id: &TenantId,
        timeline_id: &TimelineId,
    ) -> Utf8PathBuf {
        self.timelines_path(tenant_shard_id)
            .join(timeline_id.to_string())
    }

    /// Turns storage remote path of a file into its local path.
    pub fn local_path(&self, remote_path: &RemotePath) -> Utf8PathBuf {
        remote_path.with_base(&self.workdir)
    }

    //
    // Postgres distribution paths
    //
    pub fn pg_distrib_dir(&self, pg_version: PgMajorVersion) -> anyhow::Result<Utf8PathBuf> {
        let path = self.pg_distrib_dir.clone();

        Ok(path.join(pg_version.v_str()))
    }

    pub fn pg_bin_dir(&self, pg_version: PgMajorVersion) -> anyhow::Result<Utf8PathBuf> {
        Ok(self.pg_distrib_dir(pg_version)?.join("bin"))
    }
    pub fn pg_lib_dir(&self, pg_version: PgMajorVersion) -> anyhow::Result<Utf8PathBuf> {
        Ok(self.pg_distrib_dir(pg_version)?.join("lib"))
    }

    /// Parse a configuration file (pageserver.toml) into a PageServerConf struct,
    /// validating the input and failing on errors.
    ///
    /// This leaves any options not present in the file in the built-in defaults.
    pub fn parse_and_validate(
        id: NodeId,
        config_toml: pageserver_api::config::ConfigToml,
        workdir: &Utf8Path,
    ) -> anyhow::Result<Self> {
        let pageserver_api::config::ConfigToml {
            tenant_id,
            timeline_id,
            generation,
            timeline_init,
            listen_pg_addr,
            listen_http_addr,
            listen_https_addr,
            listen_grpc_addr,
            ssl_key_file,
            ssl_cert_file,
            ssl_cert_reload_period,
            ssl_ca_file,
            wait_lsn_timeout,
            wal_redo_timeout,
            superuser,
            locale,
            page_cache_size: _,
            pg_distrib_dir,
            http_auth_type,
            pg_auth_type,
            grpc_auth_type,
            auth_validation_public_key_path,
            remote_storage,
            safekeeper_pg_addr,
            log_format,
            synthetic_size_calculation_interval,
            disk_usage_based_eviction,
            test_remote_failures,
            test_remote_failures_probability,
            ondemand_download_behavior_treat_error_as_warn,
            background_task_maximum_delay,
            control_plane_api,
            control_plane_api_token,
            import_pgdata_upcall_api: _,
            import_pgdata_upcall_api_token: _,
            import_pgdata_aws_endpoint_url: _,
            heatmap_upload_concurrency: _,
            secondary_download_concurrency: _,
            ingest_batch_size,
            max_vectored_read_bytes,
            max_get_vectored_keys,
            image_compression,
            timeline_offloading,
            ephemeral_bytes_per_memory_kb,
            l0_flush: _,
            tenant_config,
            no_sync,
            page_service_pipelining,
            get_vectored_concurrent_io,
            enable_read_path_debugging,
            validate_wal_contiguity,
            load_previous_heatmap: _,
            generate_unarchival_heatmap: _,
            tracing,
            enable_tls_page_service_api,
            dev_mode,
            timeline_import_config,
            basebackup_cache_config,
            image_layer_generation_large_timeline_threshold,
            force_metric_collection_on_scrape,
            s3_upload_limit_bytes,
            yb_page_writer,
            scale_factor,
        } = config_toml;

        let timeline_identity = match (tenant_id, timeline_id, generation) {
            (Some(tenant_id), Some(timeline_id), Some(generation)) => Some(TimelineIdentity {
                tenant_id,
                timeline_id,
                generation: Generation::new(generation),
            }),
            (None, None, None) => None,
            _ => anyhow::bail!(
                "tenant_id, timeline_id and generation must be set together in pageserver.toml \
                 (got tenant_id: {}, timeline_id: {}, generation: {})",
                tenant_id.is_some(),
                timeline_id.is_some(),
                generation.is_some(),
            ),
        };
        if timeline_init.is_some() && timeline_identity.is_none() {
            anyhow::bail!("[timeline_init] requires tenant_id/timeline_id/generation to be set");
        }

        let yb_page_writer = yb_page_writer.resolve(scale_factor);
        let walredo_pool_size =
            NonZeroUsize::new(pageserver_api::config::scale_count(2, scale_factor))
                .expect("scale_count clamps to >= 1");

        let mut conf = PageServerConf {
            // ------------------------------------------------------------
            // fields that are already fully validated by the ConfigToml Deserialize impl
            // ------------------------------------------------------------
            timeline_identity,
            timeline_init,
            listen_pg_addr,
            listen_http_addr,
            listen_https_addr,
            listen_grpc_addr,
            ssl_key_file,
            ssl_cert_file,
            ssl_cert_reload_period,
            wait_lsn_timeout,
            wal_redo_timeout,
            walredo_pool_size,
            superuser,
            locale,
            http_auth_type,
            pg_auth_type,
            grpc_auth_type,
            auth_validation_public_key_path,
            remote_storage_config: remote_storage,
            safekeeper_pg_addr,
            log_format,
            synthetic_size_calculation_interval,
            disk_usage_based_eviction,
            test_remote_failures,
            test_remote_failures_probability,
            ondemand_download_behavior_treat_error_as_warn,
            background_task_maximum_delay,
            control_plane_api: control_plane_api
                .ok_or_else(|| anyhow::anyhow!("`control_plane_api` must be set"))?,
            ingest_batch_size,
            max_vectored_read_bytes,
            max_get_vectored_keys,
            image_compression,
            timeline_offloading,
            ephemeral_bytes_per_memory_kb,
            page_service_pipelining,
            get_vectored_concurrent_io,
            tracing,
            enable_tls_page_service_api,
            dev_mode,
            timeline_import_config,
            basebackup_cache_config,
            image_layer_generation_large_timeline_threshold,
            force_metric_collection_on_scrape,
            s3_upload_limit_bytes,
            yb_page_writer,
            scale_factor,

            // ------------------------------------------------------------
            // fields that require additional validation or custom handling
            // ------------------------------------------------------------
            workdir: workdir.to_owned(),
            pg_distrib_dir: pg_distrib_dir.unwrap_or_else(|| {
                std::env::current_dir()
                    .expect("current_dir() failed")
                    .try_into()
                    .expect("current_dir() is not a valid Utf8Path")
            }),
            control_plane_api_token: control_plane_api_token.map(SecretString::from),
            id,
            default_tenant_conf: tenant_config,
            no_sync: no_sync.unwrap_or(false),
            enable_read_path_debugging: enable_read_path_debugging.unwrap_or(false),
            validate_wal_contiguity: validate_wal_contiguity.unwrap_or(false),
            ssl_ca_certs: match ssl_ca_file {
                Some(ssl_ca_file) => {
                    let buf = std::fs::read(ssl_ca_file)?;
                    pem::parse_many(&buf)?
                        .into_iter()
                        .filter(|pem| pem.tag() == "CERTIFICATE")
                        .collect()
                }
                None => Vec::new(),
            },
        };

        // ------------------------------------------------------------
        // custom validation code that covers more than one field in isolation
        // ------------------------------------------------------------

        if [conf.http_auth_type, conf.pg_auth_type, conf.grpc_auth_type]
            .contains(&AuthType::NeonJWT)
        {
            let auth_validation_public_key_path = conf
                .auth_validation_public_key_path
                .get_or_insert_with(|| workdir.join("auth_public_key.pem"));
            ensure!(
                auth_validation_public_key_path.exists(),
                format!(
                    "Can't find auth_validation_public_key at '{auth_validation_public_key_path}'",
                )
            );
        }

        if let Some(tracing_config) = conf.tracing.as_ref() {
            let ratio = &tracing_config.sampling_ratio;
            ensure!(
                ratio.denominator != 0 && ratio.denominator >= ratio.numerator,
                format!(
                    "Invalid sampling ratio: {}/{}",
                    ratio.numerator, ratio.denominator
                )
            );

            let url = Url::parse(&tracing_config.export_config.endpoint)
                .map_err(anyhow::Error::msg)
                .with_context(|| {
                    format!(
                        "tracing endpoint URL is invalid : {}",
                        tracing_config.export_config.endpoint
                    )
                })?;

            ensure!(
                url.scheme() == "http" || url.scheme() == "https",
                format!(
                    "tracing endpoint URL must start with http:// or https://: {}",
                    tracing_config.export_config.endpoint
                )
            );
        }

        if let PageServicePipeliningConfig::Pipelined(PageServicePipeliningConfigPipelined {
            max_batch_size,
            ..
        }) = conf.page_service_pipelining
        {
            if max_batch_size.get() > conf.max_get_vectored_keys.get() {
                return Err(anyhow::anyhow!(
                    "`max_batch_size` ({max_batch_size}) must be less than or equal to `max_get_vectored_keys` ({})",
                    conf.max_get_vectored_keys.get()
                ));
            }
        };

        Ok(conf)
    }

    #[cfg(test)]
    pub fn test_repo_dir(test_name: &str) -> Utf8PathBuf {
        let test_output_dir = std::env::var("TEST_OUTPUT").unwrap_or("../tmp_check".into());

        let test_id = uuid::Uuid::new_v4();
        Utf8PathBuf::from(format!("{test_output_dir}/test_{test_name}_{test_id}"))
    }

    pub fn dummy_conf(repo_dir: Utf8PathBuf) -> Self {
        let pg_distrib_dir = Utf8PathBuf::from(env!("CARGO_MANIFEST_DIR")).join("../pg_install");

        let mut config_toml = pageserver_api::config::ConfigToml {
            wait_lsn_timeout: Duration::from_secs(60),
            wal_redo_timeout: Duration::from_secs(60),
            pg_distrib_dir: Some(pg_distrib_dir),
            synthetic_size_calculation_interval: Duration::from_secs(60),
            background_task_maximum_delay: Duration::ZERO,
            control_plane_api: Some(Url::parse("http://localhost:6666").unwrap()),
            ..Default::default()
        };

        // Test authors tend to forget about the default 10min initial lease deadline
        // when writing tests, which turns their immediate gc requests via mgmt API
        // into no-ops. Override the binary default here, such that there is no initial
        // lease deadline by default in tests. Tests that care can always override it
        // themselves.
        // Cf https://databricks.atlassian.net/browse/LKB-92?focusedCommentId=6722329
        config_toml.tenant_config.lsn_lease_length = Duration::from_secs(0);

        PageServerConf::parse_and_validate(NodeId(0), config_toml, &repo_dir).unwrap()
    }
}

#[derive(serde::Deserialize, serde::Serialize)]
pub struct PageserverIdentity {
    pub id: NodeId,
}

#[cfg(test)]
mod tests {

    use std::str::FromStr;
    use std::time::Duration;

    use camino::Utf8PathBuf;
    use pageserver_api::config::{DiskUsageEvictionTaskConfig, EvictionOrder};
    use rstest::rstest;
    use utils::{id::NodeId, serde_percent::Percent};

    use super::PageServerConf;

    #[test]
    fn test_minimal_config_toml_is_valid() {
        // The minimal valid config for running a pageserver:
        // - control_plane_api is mandatory, as pageservers cannot run in isolation
        // - we use Default impl of everything else in this situation
        let input = r#"
            control_plane_api = "http://localhost:6666"
        "#;
        let config_toml = toml_edit::de::from_str::<pageserver_api::config::ConfigToml>(input)
            .expect("empty config is valid");
        let workdir = Utf8PathBuf::from("/nonexistent");
        PageServerConf::parse_and_validate(NodeId(0), config_toml, &workdir)
            .expect("parse_and_validate");
    }

    #[test]
    fn test_timeline_identity_round_trip() {
        let input = r#"
            control_plane_api = "http://localhost:6666"
            tenant_id   = "c8178f5ed40cf94a9b3a23b4f9e1f966"
            timeline_id = "77ffde89f43d4ca08ef0a0ed1f089eaa"
            generation  = 7

            [timeline_init]
            mode       = "bootstrap"
            pg_version = 17
        "#;
        let config_toml = toml_edit::de::from_str::<pageserver_api::config::ConfigToml>(input)
            .expect("identity config is valid");
        let workdir = Utf8PathBuf::from("/nonexistent");
        let conf = PageServerConf::parse_and_validate(NodeId(0), config_toml.clone(), &workdir)
            .expect("parse_and_validate");
        let ident = conf.timeline_identity.expect("identity parsed");
        assert_eq!(
            ident.tenant_id.to_string(),
            "c8178f5ed40cf94a9b3a23b4f9e1f966"
        );
        assert_eq!(
            ident.timeline_id.to_string(),
            "77ffde89f43d4ca08ef0a0ed1f089eaa"
        );
        assert_eq!(ident.generation, utils::generation::Generation::new(7));
        assert!(matches!(
            conf.timeline_init,
            Some(pageserver_api::config::TimelineInitConfig::Bootstrap { pg_version })
                if pg_version.major_version_num() == 17
        ));

        // serialize → reparse → same identity fields survive the round trip
        let serialized = toml_edit::ser::to_string_pretty(&config_toml).expect("serializes");
        let reparsed = toml_edit::de::from_str::<pageserver_api::config::ConfigToml>(&serialized)
            .expect("round-trips");
        let conf2 = PageServerConf::parse_and_validate(NodeId(0), reparsed, &workdir)
            .expect("parse_and_validate round trip");
        assert_eq!(conf2.timeline_identity, conf.timeline_identity);
        assert_eq!(conf2.timeline_init, conf.timeline_init);
    }

    #[test]
    fn test_timeline_init_branch_parses_lsn() {
        let input = r#"
            control_plane_api = "http://localhost:6666"
            tenant_id   = "c8178f5ed40cf94a9b3a23b4f9e1f966"
            timeline_id = "11ffde89f43d4ca08ef0a0ed1f089eaa"
            generation  = 1

            [timeline_init]
            mode                 = "branch"
            ancestor_timeline_id = "77ffde89f43d4ca08ef0a0ed1f089eaa"
            ancestor_lsn         = "0/169AD58"
        "#;
        let config_toml = toml_edit::de::from_str::<pageserver_api::config::ConfigToml>(input)
            .expect("branch config is valid");
        let workdir = Utf8PathBuf::from("/nonexistent");
        let conf = PageServerConf::parse_and_validate(NodeId(0), config_toml, &workdir)
            .expect("parse_and_validate");
        match conf.timeline_init {
            Some(pageserver_api::config::TimelineInitConfig::Branch {
                ancestor_timeline_id,
                ancestor_lsn,
            }) => {
                assert_eq!(
                    ancestor_timeline_id.to_string(),
                    "77ffde89f43d4ca08ef0a0ed1f089eaa"
                );
                assert_eq!(ancestor_lsn, utils::lsn::Lsn::from_str("0/169AD58").unwrap());
            }
            other => panic!("expected branch init, got {other:?}"),
        }
    }

    #[test]
    fn test_partial_timeline_identity_is_rejected() {
        // tenant_id without timeline_id/generation must fail validation
        let input = r#"
            control_plane_api = "http://localhost:6666"
            tenant_id = "c8178f5ed40cf94a9b3a23b4f9e1f966"
        "#;
        let config_toml = toml_edit::de::from_str::<pageserver_api::config::ConfigToml>(input)
            .expect("fields parse");
        let workdir = Utf8PathBuf::from("/nonexistent");
        PageServerConf::parse_and_validate(NodeId(0), config_toml, &workdir)
            .expect_err("partial identity must be rejected");

        // [timeline_init] without identity must fail validation
        let input = r#"
            control_plane_api = "http://localhost:6666"

            [timeline_init]
            mode       = "bootstrap"
            pg_version = 17
        "#;
        let config_toml = toml_edit::de::from_str::<pageserver_api::config::ConfigToml>(input)
            .expect("fields parse");
        PageServerConf::parse_and_validate(NodeId(0), config_toml, &workdir)
            .expect_err("timeline_init without identity must be rejected");
    }

    #[test]
    fn test_config_tracing_endpoint_is_invalid() {
        let input = r#"
            control_plane_api = "http://localhost:6666"

            [tracing]

            sampling_ratio = { numerator = 1, denominator = 0 }

            [tracing.export_config]
            endpoint = "localhost:4317"
            protocol = "http-binary"
            timeout = "1ms"
        "#;
        let config_toml = toml_edit::de::from_str::<pageserver_api::config::ConfigToml>(input)
            .expect("config has valid fields");
        let workdir = Utf8PathBuf::from("/nonexistent");
        PageServerConf::parse_and_validate(NodeId(0), config_toml, &workdir)
            .expect_err("parse_and_validate should fail for endpoint without scheme");
    }

    #[rstest]
    #[case(32, 32, true)]
    #[case(64, 32, false)]
    #[case(64, 64, true)]
    #[case(128, 128, true)]
    fn test_config_max_batch_size_is_valid(
        #[case] max_batch_size: usize,
        #[case] max_get_vectored_keys: usize,
        #[case] is_valid: bool,
    ) {
        let input = format!(
            r#"
            control_plane_api = "http://localhost:6666"
            max_get_vectored_keys = {max_get_vectored_keys}
            page_service_pipelining = {{ mode="pipelined", execution="concurrent-futures", max_batch_size={max_batch_size}, batching="uniform-lsn" }}
        "#,
        );
        let config_toml = toml_edit::de::from_str::<pageserver_api::config::ConfigToml>(&input)
            .expect("config has valid fields");
        let workdir = Utf8PathBuf::from("/nonexistent");
        let result = PageServerConf::parse_and_validate(NodeId(0), config_toml, &workdir);
        assert_eq!(result.is_ok(), is_valid);
    }

    #[rstest]
    #[
        case::omit_the_whole_config(
            DiskUsageEvictionTaskConfig {
                max_usage_pct: Percent::new(80).unwrap(),
                min_avail_bytes: 2_000_000_000,
                period: Duration::from_secs(60),
                eviction_order: Default::default(),
                #[cfg(feature = "testing")]
                mock_statvfs: None,
                enabled: true,
            },
        r#"
            control_plane_api = "http://localhost:6666"
        "#,
    )]
    #[
        case::omit_enabled_field(
            DiskUsageEvictionTaskConfig {
                max_usage_pct: Percent::new(80).unwrap(),
                min_avail_bytes: 1_000_000_000,
                period: Duration::from_secs(60),
                eviction_order: EvictionOrder::RelativeAccessed {
                    highest_layer_count_loses_first: true,
                },
                #[cfg(feature = "testing")]
                mock_statvfs: None,
                enabled: true,
            },
        r#"
            control_plane_api = "http://localhost:6666"
            disk_usage_based_eviction = { max_usage_pct = 80, min_avail_bytes = 1000000000, period = "60s" }
        "#,
    )]
    #[case::disabled(
        DiskUsageEvictionTaskConfig {
            max_usage_pct: Percent::new(80).unwrap(),
            min_avail_bytes: 2_000_000_000,
            period: Duration::from_secs(60),
            eviction_order: EvictionOrder::RelativeAccessed {
                highest_layer_count_loses_first: true,
            },
            #[cfg(feature = "testing")]
            mock_statvfs: None,
            enabled: false,
        },
        r#"
            control_plane_api = "http://localhost:6666"
            disk_usage_based_eviction = { enabled = false }
        "#
    )]
    fn test_config_disk_usage_based_eviction_is_valid(
        #[case] expected_disk_usage_based_eviction: DiskUsageEvictionTaskConfig,
        #[case] input: &str,
    ) {
        let config_toml = toml_edit::de::from_str::<pageserver_api::config::ConfigToml>(input)
            .expect("disk_usage_based_eviction is valid");
        let workdir = Utf8PathBuf::from("/nonexistent");
        let config = PageServerConf::parse_and_validate(NodeId(0), config_toml, &workdir).unwrap();
        let disk_usage_based_eviction = config.disk_usage_based_eviction;
        assert_eq!(
            expected_disk_usage_based_eviction,
            disk_usage_based_eviction
        );
    }
}
