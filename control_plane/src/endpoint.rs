//! Code to manage compute endpoints
//!
//! The durable registry of compute endpoints lives in the
//! `storage_controller.endpoints` table, not on the local filesystem. Each
//! endpoint's config (the [`EndpointConf`] struct) is serialized to JSON and
//! stored as the `spec` column of one row, keyed by endpoint id.
//! `yb_light_local` creates / lists / deletes endpoints by calling the storage
//! controller's `/control/v1/endpoint` API (see
//! [`crate::storage_controller::StorageController::endpoint_create`] and
//! friends); the controller is the single source of truth.
//!
//! The endpoint is managed by the `compute_ctl` binary. When an endpoint is
//! started, we launch `compute_ctl` It synchronizes the safekeepers, downloads
//! the basebackup from the pageserver to initialize the data directory, and
//! finally launches the PostgreSQL process. It watches the PostgreSQL process
//! until it exits.
//!
//! A live endpoint still needs a local working directory to hold runtime
//! artifacts of its `compute_ctl` / `postgres` processes. This directory is
//! created lazily on `start` (not at create time) and removed on `destroy`; it
//! is *not* the registry — losing it does not lose the endpoint, only its
//! current process state. The `postgresql.conf` passed to `compute_ctl` is
//! regenerated in memory from [`EndpointConf`] on each start, so it is never
//! persisted on its own.
//!
//! Working-directory contents:
//!
//! ```text
//! .yb_light/endpoints/main/
//!     compute.log               - log output of `compute_ctl` and `postgres`
//!     config.json               - passed to `compute_ctl`
//!     compute_ctl.pid           - pid of the `compute_ctl` process
//!     pgdata/
//!         postgresql.conf       - copy of postgresql.conf created by `compute_ctl`
//!         neon.signal
//!         zenith.signal         - copy of neon.signal, for backward compatibility
//!         <other PostgreSQL files>
//! ```
//!
use std::collections::BTreeMap;
use std::fmt::Display;
use std::net::{IpAddr, Ipv4Addr, SocketAddr, TcpListener, TcpStream};
use std::path::PathBuf;
use std::process::Command;
use std::str::FromStr;
use std::sync::Arc;
use std::time::{Duration, Instant};

use anyhow::{Context, Result, anyhow, bail};
use base64::Engine;
use base64::prelude::BASE64_URL_SAFE_NO_PAD;
use compute_api::requests::{
    COMPUTE_AUDIENCE, ComputeClaims, ComputeClaimsScope, ConfigurationRequest,
};
use compute_api::responses::{
    ComputeConfig, ComputeCtlConfig, ComputeStatus, ComputeStatusResponse, TerminateResponse,
    TlsConfig,
};
use compute_api::spec::{
    Cluster, ComputeAudit, ComputeFeature, ComputeMode, ComputeSpec, Database, PageserverProtocol,
    PageserverConnectionItem, PgIdent, RemoteExtSpec, Role,
};

// re-export these, because they're used in the reconfigure() function
pub use compute_api::spec::PageserverConnectionInfo;

use jsonwebtoken::jwk::{
    AlgorithmParameters, CommonParameters, EllipticCurve, Jwk, JwkSet, KeyAlgorithm, KeyOperations,
    OctetKeyPairParameters, OctetKeyPairType, PublicKeyUse,
};
use nix::sys::signal::{Signal, kill};
use pem::Pem;
use reqwest::header::CONTENT_TYPE;
use safekeeper_api::PgMajorVersion;
use safekeeper_api::membership::SafekeeperGeneration;
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use spki::der::Decode;
use spki::{SubjectPublicKeyInfo, SubjectPublicKeyInfoRef};
use tracing::debug;
use utils::id::{NodeId, TenantId, TimelineId};

use pageserver_api::config::DEFAULT_GRPC_LISTEN_PORT as DEFAULT_PAGESERVER_GRPC_PORT;
use pageserver_api::controller_api::ComputeEndpoint;
use postgres_connection::parse_host_port;

use crate::local_env::LocalEnv;
use crate::endpoint_storage::EndpointStorage;
use crate::pageserver::PageServerNode;
use crate::postgresql_conf::PostgresConf;
use crate::safekeeper::SafekeeperNode;
use crate::storage_controller::StorageController;

// contents of a endpoint.json file
#[derive(Serialize, Deserialize, PartialEq, Eq, Clone, Debug)]
pub struct EndpointConf {
    endpoint_id: String,
    tenant_id: TenantId,
    timeline_id: TimelineId,
    mode: ComputeMode,
    pg_port: u16,
    external_http_port: u16,
    internal_http_port: u16,
    pg_version: PgMajorVersion,
    grpc: bool,
    skip_pg_catalog_updates: bool,
    reconfigure_concurrency: usize,
    drop_subscriptions_before_start: bool,
    features: Vec<ComputeFeature>,
    cluster: Option<Cluster>,
    compute_ctl_config: ComputeCtlConfig,
    privileged_role_name: Option<String>,
    #[serde(default)]
    shared_buffers: Option<String>,
    #[serde(default)]
    lfc_size: Option<String>,
    #[serde(default)]
    pg_settings: Vec<(String, String)>,
}

//
// ComputeControlPlane
//
pub struct ComputeControlPlane {
    base_port: u16,

    // endpoint ID is the key
    pub endpoints: BTreeMap<String, Arc<Endpoint>>,

    env: LocalEnv,
}

impl ComputeControlPlane {
    /// Load the current set of endpoints from the storage controller's
    /// `endpoints` table (via its HTTP API). Replaces the previous scan of the
    /// `.yb_light/endpoints/` directory.
    pub async fn load(env: LocalEnv) -> Result<ComputeControlPlane> {
        let records = StorageController::from_env(&env)
            .endpoint_list()
            .await
            .context("listing endpoints from storage controller")?;

        let mut endpoints = BTreeMap::default();
        for record in records {
            let endpoint_id = record.endpoint_id.clone();
            let conf: EndpointConf = serde_json::from_value(record.spec)
                .with_context(|| format!("deserializing spec for endpoint {endpoint_id}"))?;
            let ep = Endpoint::from_conf(conf, &env);
            endpoints.insert(ep.endpoint_id.clone(), Arc::new(ep));
        }

        Ok(ComputeControlPlane {
            base_port: 55431,
            endpoints,
            env,
        })
    }

    fn next_candidate_port(&self) -> u16 {
        1 + self
            .endpoints
            .values()
            .map(|ep| {
                ep.pg_address
                    .port()
                    .max(ep.external_http_address.port())
                    .max(ep.internal_http_address.port())
            })
            .max()
            .unwrap_or(self.base_port)
    }

    /// Returns the first port >= `start` that can be bound on localhost.
    fn find_free_port(start: u16) -> u16 {
        let localhost = IpAddr::V4(Ipv4Addr::LOCALHOST);
        for port in start..=u16::MAX {
            if TcpListener::bind(SocketAddr::new(localhost, port)).is_ok() {
                return port;
            }
        }
        panic!("no free port found in range [{start}, 65535]");
    }

    /// Create a JSON Web Key Set. This ideally matches the way we create a JWKS
    /// from the production control plane.
    fn create_jwks_from_pem(pem: &Pem) -> Result<JwkSet> {
        let spki: SubjectPublicKeyInfoRef = SubjectPublicKeyInfo::from_der(pem.contents())?;
        let public_key = spki.subject_public_key.raw_bytes();

        let mut hasher = Sha256::new();
        hasher.update(public_key);
        let key_hash = hasher.finalize();

        Ok(JwkSet {
            keys: vec![Jwk {
                common: CommonParameters {
                    public_key_use: Some(PublicKeyUse::Signature),
                    key_operations: Some(vec![KeyOperations::Verify]),
                    key_algorithm: Some(KeyAlgorithm::EdDSA),
                    key_id: Some(BASE64_URL_SAFE_NO_PAD.encode(key_hash)),
                    x509_url: None::<String>,
                    x509_chain: None::<Vec<String>>,
                    x509_sha1_fingerprint: None::<String>,
                    x509_sha256_fingerprint: None::<String>,
                },
                algorithm: AlgorithmParameters::OctetKeyPair(OctetKeyPairParameters {
                    key_type: OctetKeyPairType::OctetKeyPair,
                    curve: EllipticCurve::Ed25519,
                    x: BASE64_URL_SAFE_NO_PAD.encode(public_key),
                }),
            }],
        })
    }

    #[allow(clippy::too_many_arguments)]
    pub async fn new_endpoint(
        &mut self,
        endpoint_id: &str,
        tenant_id: TenantId,
        timeline_id: TimelineId,
        pg_port: Option<u16>,
        external_http_port: Option<u16>,
        internal_http_port: Option<u16>,
        pg_version: PgMajorVersion,
        mode: ComputeMode,
        grpc: bool,
        skip_pg_catalog_updates: bool,
        drop_subscriptions_before_start: bool,
        privileged_role_name: Option<String>,
        shared_buffers: Option<String>,
        lfc_size: Option<String>,
        pg_settings: Vec<(String, String)>,
    ) -> Result<Arc<Endpoint>> {
        let pg_port = pg_port.unwrap_or_else(|| Self::find_free_port(self.next_candidate_port()));
        let external_http_port =
            external_http_port.unwrap_or_else(|| Self::find_free_port(pg_port + 1));
        let internal_http_port =
            internal_http_port.unwrap_or_else(|| Self::find_free_port(external_http_port + 1));
        let compute_ctl_config = ComputeCtlConfig {
            jwks: Self::create_jwks_from_pem(&self.env.read_public_key()?)?,
            tls: None::<TlsConfig>,
        };
        let conf = EndpointConf {
            endpoint_id: endpoint_id.to_string(),
            tenant_id,
            timeline_id,
            mode,
            external_http_port,
            internal_http_port,
            pg_port,
            pg_version,
            grpc,
            // We don't setup roles and databases in the spec locally, so we don't need to
            // do catalog updates. Catalog updates also include check availability
            // data creation. Yet, we have tests that check that size and db dump
            // before and after start are the same. So, skip catalog updates,
            // with this we basically test a case of waking up an idle compute, where
            // we also skip catalog updates in the cloud.
            skip_pg_catalog_updates,
            drop_subscriptions_before_start,
            reconfigure_concurrency: 1,
            features: vec![],
            cluster: None,
            compute_ctl_config,
            privileged_role_name,
            shared_buffers,
            lfc_size,
            pg_settings,
        };

        // Persist the endpoint in the storage controller's `endpoints` table.
        // The whole `EndpointConf` rides along as an opaque JSONB `spec`.
        let spec = serde_json::to_value(&conf).context("serializing endpoint config")?;
        StorageController::from_env(&self.env)
            .endpoint_create(ComputeEndpoint {
                endpoint_id: endpoint_id.to_string(),
                tenant_id: tenant_id.to_string(),
                timeline_id: timeline_id.to_string(),
                spec,
            })
            .await
            .context("creating endpoint in storage controller")?;

        let ep = Arc::new(Endpoint::from_conf(conf, &self.env));
        self.endpoints
            .insert(ep.endpoint_id.clone(), Arc::clone(&ep));

        Ok(ep)
    }

    pub fn check_conflicting_endpoints(
        &self,
        mode: ComputeMode,
        tenant_id: TenantId,
        timeline_id: TimelineId,
    ) -> Result<()> {
        if matches!(mode, ComputeMode::Primary) {
            // this check is not complete, as you could have a concurrent attempt at
            // creating another primary, both reading the state before checking it here,
            // but it's better than nothing.
            let mut duplicates = self.endpoints.iter().filter(|(_k, v)| {
                v.tenant_id == tenant_id
                    && v.timeline_id == timeline_id
                    && v.mode == mode
                    && v.status() != EndpointStatus::Stopped
            });

            if let Some((key, _)) = duplicates.next() {
                bail!(
                    "attempting to create a duplicate primary endpoint on tenant {tenant_id}, timeline {timeline_id}: endpoint {key:?} exists already. please don't do this, it is not supported."
                );
            }
        }
        Ok(())
    }
}

///////////////////////////////////////////////////////////////////////////////

pub struct Endpoint {
    /// used as the directory name
    endpoint_id: String,
    pub tenant_id: TenantId,
    pub timeline_id: TimelineId,
    pub mode: ComputeMode,
    /// If true, the endpoint should use gRPC to communicate with Pageservers.
    pub grpc: bool,

    // port and address of the Postgres server and `compute_ctl`'s HTTP APIs
    pub pg_address: SocketAddr,
    pub external_http_address: SocketAddr,
    pub internal_http_address: SocketAddr,

    // postgres major version in the format: 14, 15, etc.
    pg_version: PgMajorVersion,

    // These are not part of the endpoint as such, but the environment
    // the endpoint runs in.
    pub env: LocalEnv,

    // Optimizations
    skip_pg_catalog_updates: bool,

    drop_subscriptions_before_start: bool,
    reconfigure_concurrency: usize,
    // Feature flags
    features: Vec<ComputeFeature>,
    // Cluster settings
    cluster: Option<Cluster>,

    /// The compute_ctl config for the endpoint's compute.
    compute_ctl_config: ComputeCtlConfig,

    /// The name of the privileged role for the endpoint.
    privileged_role_name: Option<String>,

    /// Value of `shared_buffers` to write into the generated postgresql.conf.
    /// Defaults to `1MB` (matches upstream behaviour) when unset; raise it
    /// (e.g. `32GB`) for benchmarking on larger hosts.
    shared_buffers: Option<String>,

    /// If set, enables Neon's Local File Cache (LFC) on the compute by
    /// writing both `neon.max_file_cache_size` (postmaster, hard cap) and
    /// `neon.file_cache_size_limit` (SIGHUP, soft cap) to this value
    /// (e.g. `8GB`). Without this, LFC is disabled and every shared_buffers
    /// miss is a getpage RPC.
    lfc_size: Option<String>,

    /// Arbitrary `name = value` pairs to append to the generated
    /// postgresql.conf. Use for tuning knobs that don't have a dedicated
    /// CLI flag — e.g. `max_connections=200`, `effective_cache_size=96GB`.
    pg_settings: Vec<(String, String)>,
}

#[derive(PartialEq, Eq)]
pub enum EndpointStatus {
    Running,
    Stopped,
    Crashed,
    RunningNoPidfile,
}

impl Display for EndpointStatus {
    fn fmt(&self, writer: &mut std::fmt::Formatter) -> std::fmt::Result {
        writer.write_str(match self {
            Self::Running => "running",
            Self::Stopped => "stopped",
            Self::Crashed => "crashed",
            Self::RunningNoPidfile => "running, no pidfile",
        })
    }
}

#[derive(Default, Clone, Copy, clap::ValueEnum)]
pub enum EndpointTerminateMode {
    #[default]
    /// Use pg_ctl stop -m fast
    Fast,
    /// Use pg_ctl stop -m immediate
    Immediate,
    /// Use /terminate?mode=immediate
    ImmediateTerminate,
}

impl std::fmt::Display for EndpointTerminateMode {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(match &self {
            EndpointTerminateMode::Fast => "fast",
            EndpointTerminateMode::Immediate => "immediate",
            EndpointTerminateMode::ImmediateTerminate => "immediate-terminate",
        })
    }
}

pub struct EndpointStartArgs {
    pub auth_token: Option<String>,
    pub endpoint_storage_token: String,
    pub endpoint_storage_addr: String,
    pub safekeepers_generation: Option<SafekeeperGeneration>,
    pub safekeepers: Vec<NodeId>,
    pub pageserver_conninfo: PageserverConnectionInfo,
    pub remote_ext_base_url: Option<String>,
    pub create_test_user: bool,
    pub start_timeout: Duration,
    pub autoprewarm: bool,
    pub offload_lfc_interval_seconds: Option<std::num::NonZeroU64>,
    pub dev: bool,
}

impl Endpoint {
    /// Build an in-memory [`Endpoint`] from its persisted [`EndpointConf`]
    /// (loaded from the storage controller's `endpoints` table). Pure: derives
    /// the listen addresses from the persisted ports and touches no files.
    fn from_conf(conf: EndpointConf, env: &LocalEnv) -> Endpoint {
        debug!("deserialized endpoint conf: {:?}", conf);

        Endpoint {
            pg_address: SocketAddr::new(IpAddr::from(Ipv4Addr::LOCALHOST), conf.pg_port),
            external_http_address: SocketAddr::new(
                IpAddr::from(Ipv4Addr::UNSPECIFIED),
                conf.external_http_port,
            ),
            internal_http_address: SocketAddr::new(
                IpAddr::from(Ipv4Addr::LOCALHOST),
                conf.internal_http_port,
            ),
            endpoint_id: conf.endpoint_id,
            env: env.clone(),
            timeline_id: conf.timeline_id,
            mode: conf.mode,
            tenant_id: conf.tenant_id,
            pg_version: conf.pg_version,
            grpc: conf.grpc,
            skip_pg_catalog_updates: conf.skip_pg_catalog_updates,
            reconfigure_concurrency: conf.reconfigure_concurrency,
            drop_subscriptions_before_start: conf.drop_subscriptions_before_start,
            features: conf.features,
            cluster: conf.cluster,
            compute_ctl_config: conf.compute_ctl_config,
            privileged_role_name: conf.privileged_role_name,
            shared_buffers: conf.shared_buffers,
            lfc_size: conf.lfc_size,
            pg_settings: conf.pg_settings,
        }
    }

    fn create_endpoint_dir(&self) -> Result<()> {
        std::fs::create_dir_all(self.endpoint_path()).with_context(|| {
            format!(
                "could not create endpoint directory {}",
                self.endpoint_path().display()
            )
        })
    }

    // Generate postgresql.conf with default configuration
    fn setup_pg_conf(&self) -> Result<PostgresConf> {
        let mut conf = PostgresConf::new();
        conf.append("max_wal_senders", "10");
        conf.append("wal_log_hints", "off");
        conf.append("max_replication_slots", "10");
        conf.append("hot_standby", "on");
        // Set to 1MB by default to both exercise getPage requests/LFC, and still have enough
        // room for Postgres to operate. Everything smaller might be not enough for Postgres
        // under load, and can cause errors like 'no unpinned buffers available', see
        // <https://github.com/neondatabase/neon/issues/9956>.
        // Override via the `--shared-buffers` flag on `yb_light_local endpoint create` (or by
        // editing endpoint.json) — e.g. `32GB` on benchmarking hosts.
        conf.append(
            "shared_buffers",
            self.shared_buffers.as_deref().unwrap_or("1MB"),
        );
        // Postgres defaults to effective_io_concurrency=1, which does not exercise the pageserver's
        // batching logic.  Set this to 2 so that we exercise the code a bit without letting
        // individual tests do a lot of concurrent work on underpowered test machines
        conf.append("effective_io_concurrency", "2");
        conf.append("fsync", "off");
        // yb-light durability lives in YB rows, not in fsync'd PG pages — torn
        // pages can't survive recovery here, so disable full_page_writes to
        // shave the per-checkpoint WAL spike. Override via --pg-setting if a
        // workload genuinely needs it.
        conf.append("full_page_writes", "off");
        conf.append("max_connections", "100");
        conf.append("wal_level", "logical");
        // wal_sender_timeout is the maximum time to wait for WAL replication.
        // It also defines how often the walreceiver will send a feedback message to the wal sender.
        conf.append("wal_sender_timeout", "5s");
        conf.append("listen_addresses", &self.pg_address.ip().to_string());
        conf.append("port", &self.pg_address.port().to_string());
        conf.append("wal_keep_size", "0");
        // walproposer panics when basebackup is invalid, it is pointless to restart in this case.
        conf.append("restart_after_crash", "off");

        // Load the 'neon' extension
        conf.append("shared_preload_libraries", "neon");

        // When local TLS is off (the default), tell walproposer not to attempt a
        // TLS handshake against the safekeeper. The GUC is prepended verbatim to
        // the libpq conninfo walproposer builds per-safekeeper.
        // Flip `use_local_tls = true` in .yb_light/config to re-enable.
        if !self.env.use_local_tls {
            conf.append("neon.safekeeper_conninfo_options", "sslmode=disable");
        }

        // Neon Local File Cache: when --lfc-size=<v> was passed at create,
        // enable LFC by setting both the postmaster hard cap and the
        // SIGHUP-tunable soft cap to the same value. Without this, both
        // GUCs default to 0 and LFC is fully disabled.
        if let Some(lfc) = self.lfc_size.as_deref() {
            conf.append("neon.max_file_cache_size", lfc);
            conf.append("neon.file_cache_size_limit", lfc);
        }

        // User-supplied --pg-setting overrides (or extras). These come
        // last so they win over the defaults set above.
        for (k, v) in &self.pg_settings {
            conf.append(k, v);
        }

        conf.append_line("");
        // Replication-related configurations, such as WAL sending
        match &self.mode {
            ComputeMode::Primary => {
                // Configure backpressure
                // - Replication write lag depends on how fast the walreceiver can process incoming WAL.
                //   This lag determines latency of get_page_at_lsn. Speed of applying WAL is about 10MB/sec,
                //   so to avoid expiration of 1 minute timeout, this lag should not be larger than 600MB.
                //   Actually latency should be much smaller (better if < 1sec). But we assume that recently
                //   updates pages are not requested from pageserver.
                // - Replication flush lag depends on speed of persisting data by checkpointer (creation of
                //   delta/image layers) and advancing disk_consistent_lsn. Safekeepers are able to
                //   remove/archive WAL only beyond disk_consistent_lsn. Too large a lag can cause long
                //   recovery time (in case of pageserver crash) and disk space overflow at safekeepers.
                // - Replication apply lag depends on speed of uploading changes to S3 by uploader thread.
                //   To be able to restore database in case of pageserver node crash, safekeeper should not
                //   remove WAL beyond this point. Too large lag can cause space exhaustion in safekeepers
                //   (if they are not able to upload WAL to S3).
                conf.append("max_replication_write_lag", "15MB");
                conf.append("max_replication_flush_lag", "10GB");

                if !self.env.safekeepers.is_empty() {
                    // Configure Postgres to connect to the safekeepers
                    conf.append("synchronous_standby_names", "walproposer");

                    let safekeepers = self
                        .env
                        .safekeepers
                        .iter()
                        .map(|sk| format!("localhost:{}", sk.get_compute_port()))
                        .collect::<Vec<String>>()
                        .join(",");
                    conf.append("neon.safekeepers", &safekeepers);
                } else {
                    // We only use setup without safekeepers for tests,
                    // and don't care about data durability on pageserver,
                    // so set more relaxed synchronous_commit.
                    conf.append("synchronous_commit", "remote_write");

                    // Configure the node to stream WAL directly to the pageserver
                    // This isn't really a supported configuration, but can be useful for
                    // testing.
                    conf.append("synchronous_standby_names", "pageserver");
                }
            }
            ComputeMode::Static(lsn) => {
                conf.append("recovery_target_lsn", &lsn.to_string());
            }
            ComputeMode::Replica => {
                assert!(!self.env.safekeepers.is_empty());

                // TODO: use future host field from safekeeper spec
                // Pass the list of safekeepers to the replica so that it can connect to any of them,
                // whichever is available.
                let sk_ports = self
                    .env
                    .safekeepers
                    .iter()
                    .map(|x| x.get_compute_port().to_string())
                    .collect::<Vec<_>>()
                    .join(",");
                let sk_hosts = vec!["localhost"; self.env.safekeepers.len()].join(",");

                let connstr = format!(
                    "host={} port={} options='-c timeline_id={} tenant_id={}' application_name=replica replication=true",
                    sk_hosts,
                    sk_ports,
                    &self.timeline_id.to_string(),
                    &self.tenant_id.to_string(),
                );

                let slot_name = format!("repl_{}_", self.timeline_id);
                conf.append("primary_conninfo", connstr.as_str());
                conf.append("primary_slot_name", slot_name.as_str());
                conf.append("hot_standby", "on");
                // prefetching of blocks referenced in WAL doesn't make sense for us
                // Neon hot standby ignores pages that are not in the shared_buffers
                if self.pg_version >= PgMajorVersion::PG17 {
                    conf.append("recovery_prefetch", "off");
                }
            }
        }

        Ok(conf)
    }

    pub fn endpoint_path(&self) -> PathBuf {
        self.env.endpoints_path().join(&self.endpoint_id)
    }

    pub fn pgdata(&self) -> PathBuf {
        self.endpoint_path().join("pgdata")
    }

    pub fn status(&self) -> EndpointStatus {
        let timeout = Duration::from_millis(300);
        let has_pidfile = self.pgdata().join("postmaster.pid").exists();
        let can_connect = TcpStream::connect_timeout(&self.pg_address, timeout).is_ok();

        match (has_pidfile, can_connect) {
            (true, true) => EndpointStatus::Running,
            (false, false) => EndpointStatus::Stopped,
            (true, false) => EndpointStatus::Crashed,
            (false, true) => EndpointStatus::RunningNoPidfile,
        }
    }

    fn pg_ctl(&self, args: &[&str], auth_token: &Option<String>) -> Result<()> {
        let pg_ctl_path = self.env.pg_bin_dir(self.pg_version)?.join("pg_ctl");
        let mut cmd = Command::new(&pg_ctl_path);
        cmd.args(
            [
                &[
                    "-D",
                    self.pgdata().to_str().unwrap(),
                    "-w", //wait till pg_ctl actually does what was asked
                ],
                args,
            ]
            .concat(),
        )
        .env_clear()
        .env(
            "LD_LIBRARY_PATH",
            self.env.pg_lib_dir(self.pg_version)?.to_str().unwrap(),
        )
        .env(
            "DYLD_LIBRARY_PATH",
            self.env.pg_lib_dir(self.pg_version)?.to_str().unwrap(),
        );

        // Pass authentication token used for the connections to pageserver and safekeepers
        if let Some(token) = auth_token {
            cmd.env("NEON_AUTH_TOKEN", token);
        }

        let pg_ctl = cmd
            .output()
            .context(format!("{} failed", pg_ctl_path.display()))?;
        if !pg_ctl.status.success() {
            anyhow::bail!(
                "pg_ctl failed, exit code: {}, stdout: {}, stderr: {}",
                pg_ctl.status,
                String::from_utf8_lossy(&pg_ctl.stdout),
                String::from_utf8_lossy(&pg_ctl.stderr),
            );
        }

        Ok(())
    }

    fn wait_for_compute_ctl_to_exit(&self, send_sigterm: bool) -> Result<()> {
        // TODO use background_process::stop_process instead: https://github.com/neondatabase/neon/pull/6482
        let pidfile_path = self.endpoint_path().join("compute_ctl.pid");
        let pid: u32 = std::fs::read_to_string(pidfile_path)?.parse()?;
        let pid = nix::unistd::Pid::from_raw(pid as i32);
        if send_sigterm {
            kill(pid, Signal::SIGTERM).ok();
        }
        crate::background_process::wait_until_stopped("compute_ctl", pid)?;
        Ok(())
    }

    fn read_postgresql_conf(&self) -> Result<String> {
        // Regenerate the postgresql.conf from the endpoint config. It used to
        // be written to `endpoints/<id>/postgresql.conf` at create time and
        // slurped back here, but the endpoint config now lives in the storage
        // controller's `endpoints` table, so we build it in memory from the
        // same deterministic `setup_pg_conf` inputs. We include it in the spec
        // we pass to `compute_ctl`, which writes it to the postgresql.conf in
        // the data directory.
        Ok(self.setup_pg_conf()?.to_string())
    }

    /// Map safekeepers ids to the actual connection strings.
    fn build_safekeepers_connstrs(&self, sk_ids: Vec<NodeId>) -> Result<Vec<String>> {
        let mut safekeeper_connstrings = Vec::new();
        if self.mode == ComputeMode::Primary {
            for sk_id in sk_ids {
                let sk = self
                    .env
                    .safekeepers
                    .iter()
                    .find(|node| node.id == sk_id)
                    .ok_or_else(|| anyhow!("safekeeper {sk_id} does not exist"))?;
                safekeeper_connstrings.push(format!("127.0.0.1:{}", sk.get_compute_port()));
            }
        }
        Ok(safekeeper_connstrings)
    }

    /// Generate a JWT with the correct claims.
    pub fn generate_jwt(&self, scope: Option<ComputeClaimsScope>) -> Result<String> {
        self.env.generate_auth_token(&ComputeClaims {
            audience: match scope {
                Some(ComputeClaimsScope::Admin) => Some(vec![COMPUTE_AUDIENCE.to_owned()]),
                _ => None,
            },
            compute_id: match scope {
                Some(ComputeClaimsScope::Admin) => None,
                _ => Some(self.endpoint_id.clone()),
            },
            scope,
        })
    }

    pub async fn start(&self, args: EndpointStartArgs) -> Result<()> {
        if self.status() == EndpointStatus::Running {
            anyhow::bail!("The endpoint is already running");
        }

        // The endpoint registry lives in the storage controller's table, so the
        // local working directory is no longer created at create time — make
        // sure it exists before we write config.json / logs / the pidfile.
        self.create_endpoint_dir()?;

        let postgresql_conf = self.read_postgresql_conf()?;

        // We always start the compute node from scratch, so if the Postgres
        // data dir exists from a previous launch, remove it first.
        if self.pgdata().exists() {
            std::fs::remove_dir_all(self.pgdata())?;
        }

        let safekeeper_connstrings = self.build_safekeepers_connstrs(args.safekeepers)?;

        // check for file remote_extensions_spec.json
        // if it is present, read it and pass to compute_ctl
        let remote_extensions_spec_path = self.endpoint_path().join("remote_extensions_spec.json");
        let remote_extensions_spec = std::fs::File::open(remote_extensions_spec_path);
        let remote_extensions: Option<RemoteExtSpec>;

        if let Ok(spec_file) = remote_extensions_spec {
            remote_extensions = serde_json::from_reader(spec_file).ok();
        } else {
            remote_extensions = None;
        };

        // For the sake of backwards-compatibility, also fill in 'pageserver_connstring'
        //
        // XXX: I believe this is not really needed, except to make
        // test_forward_compatibility happy.
        //
        // Use a closure so that we can conviniently return None in the middle of the
        // loop.
        let pageserver_connstring: Option<String> = (|| {
            let mut connstrings = Vec::new();
            for pageserver in &args.pageserver_conninfo.pageservers {
                if let Some(libpq_url) = &pageserver.libpq_url {
                    connstrings.push(libpq_url.clone());
                } else {
                    return Ok::<_, anyhow::Error>(None);
                }
            }
            Ok(Some(connstrings.join(",")))
        })()?;

        // Create config file
        let config = {
            let mut spec = ComputeSpec {
                skip_pg_catalog_updates: self.skip_pg_catalog_updates,
                format_version: 1.0,
                operation_uuid: None,
                features: self.features.clone(),
                swap_size_bytes: None,
                disk_quota_bytes: None,
                disable_lfc_resizing: None,
                cluster: Cluster {
                    cluster_id: None, // project ID: not used
                    name: None,       // project name: not used
                    state: None,
                    roles: if args.create_test_user {
                        vec![Role {
                            name: PgIdent::from_str("test").unwrap(),
                            encrypted_password: None,
                            options: None,
                        }]
                    } else {
                        Vec::new()
                    },
                    databases: if args.create_test_user {
                        vec![Database {
                            name: PgIdent::from_str("neondb").unwrap(),
                            owner: PgIdent::from_str("test").unwrap(),
                            options: None,
                            restrict_conn: false,
                            invalid: false,
                        }]
                    } else {
                        Vec::new()
                    },
                    settings: None,
                    postgresql_conf: Some(postgresql_conf.clone()),
                },
                delta_operations: None,
                tenant_id: Some(self.tenant_id),
                timeline_id: Some(self.timeline_id),
                project_id: None,
                branch_id: None,
                endpoint_id: Some(self.endpoint_id.clone()),
                mode: self.mode,
                pageserver_connection_info: Some(args.pageserver_conninfo.clone()),
                pageserver_connstring,
                safekeepers_generation: args.safekeepers_generation.map(|g| g.into_inner()),
                safekeeper_connstrings,
                storage_auth_token: args.auth_token.clone(),
                remote_extensions,
                pgbouncer_settings: None,
                local_proxy_config: None,
                reconfigure_concurrency: self.reconfigure_concurrency,
                drop_subscriptions_before_start: self.drop_subscriptions_before_start,
                audit_log_level: ComputeAudit::Disabled,
                logs_export_host: None::<String>,
                endpoint_storage_addr: Some(args.endpoint_storage_addr),
                endpoint_storage_token: Some(args.endpoint_storage_token),
                autoprewarm: args.autoprewarm,
                offload_lfc_interval_seconds: args.offload_lfc_interval_seconds,
                suspend_timeout_seconds: -1, // Only used in yb_light_local.
                databricks_settings: None,
            };

            // this strange code is needed to support respec() in tests
            if self.cluster.is_some() {
                debug!("Cluster is already set in the endpoint spec, using it");
                spec.cluster = self.cluster.clone().unwrap();

                debug!("spec.cluster {:?}", spec.cluster);

                // fill missing fields again
                if args.create_test_user {
                    spec.cluster.roles.push(Role {
                        name: PgIdent::from_str("test").unwrap(),
                        encrypted_password: None,
                        options: None,
                    });
                    spec.cluster.databases.push(Database {
                        name: PgIdent::from_str("neondb").unwrap(),
                        owner: PgIdent::from_str("test").unwrap(),
                        options: None,
                        restrict_conn: false,
                        invalid: false,
                    });
                }
                spec.cluster.postgresql_conf = Some(postgresql_conf);
            }

            ComputeConfig {
                spec: Some(spec),
                compute_ctl_config: self.compute_ctl_config.clone(),
            }
        };

        let config_path = self.endpoint_path().join("config.json");
        std::fs::write(config_path, serde_json::to_string_pretty(&config)?)?;

        // Open log file. We'll redirect the stdout and stderr of `compute_ctl` to it.
        let logfile = std::fs::OpenOptions::new()
            .create(true)
            .append(true)
            .open(self.endpoint_path().join("compute.log"))?;

        // Launch compute_ctl
        let conn_str = self.connstr("cloud_admin", "postgres");
        println!("Starting postgres node at '{conn_str}'");
        if args.create_test_user {
            let conn_str = self.connstr("test", "neondb");
            println!("Also at '{conn_str}'");
        }
        let mut cmd = Command::new(self.env.yb_light_distrib_dir.join("yb-compute-ctl"));
        cmd.args([
            "--external-http-port",
            &self.external_http_address.port().to_string(),
        ])
        .args([
            "--internal-http-port",
            &self.internal_http_address.port().to_string(),
        ])
        .args(["--pgdata", self.pgdata().to_str().unwrap()])
        .args(["--connstr", &conn_str])
        .arg("--config")
        .arg(self.endpoint_path().join("config.json").as_os_str())
        .args([
            "--pgbin",
            self.env
                .pg_bin_dir(self.pg_version)?
                .join("postgres")
                .to_str()
                .unwrap(),
        ])
        // TODO: It would be nice if we generated compute IDs with the same
        // algorithm as the real control plane.
        .args(["--compute-id", &self.endpoint_id])
        .stdin(std::process::Stdio::null())
        .stderr(logfile.try_clone()?)
        .stdout(logfile);

        if let Some(remote_ext_base_url) = args.remote_ext_base_url {
            cmd.args(["--remote-ext-base-url", &remote_ext_base_url]);
        }

        if args.dev {
            cmd.arg("--dev");
        }

        if let Some(privileged_role_name) = self.privileged_role_name.clone() {
            cmd.args(["--privileged-role-name", &privileged_role_name]);
        }

        let child = cmd.spawn()?;
        // set up a scopeguard to kill & wait for the child in case we panic or bail below
        let child = scopeguard::guard(child, |mut child| {
            println!("SIGKILL & wait the started process");
            (|| {
                // TODO: use another signal that can be caught by the child so it can clean up any children it spawned
                child.kill().context("SIGKILL child")?;
                child.wait().context("wait() for child process")?;
                anyhow::Ok(())
            })()
            .with_context(|| format!("scopeguard kill&wait child {child:?}"))
            .unwrap();
        });

        // Write down the pid so we can wait for it when we want to stop
        // TODO use background_process::start_process instead: https://github.com/neondatabase/neon/pull/6482
        let pid = child.id();
        let pidfile_path = self.endpoint_path().join("compute_ctl.pid");
        std::fs::write(pidfile_path, pid.to_string())?;

        // Wait for it to start
        const ATTEMPT_INTERVAL: Duration = Duration::from_millis(100);
        let start_at = Instant::now();
        loop {
            match self.get_status().await {
                Ok(state) => {
                    match state.status {
                        ComputeStatus::Init => {
                            let timeout = args.start_timeout;
                            if Instant::now().duration_since(start_at) > timeout {
                                bail!(
                                    "compute startup timed out {:?}; still in Init state",
                                    timeout
                                );
                            }
                            // keep retrying
                        }
                        ComputeStatus::Running => {
                            // All good!
                            break;
                        }
                        ComputeStatus::Failed => {
                            bail!(
                                "compute startup failed: {}",
                                state
                                    .error
                                    .as_deref()
                                    .unwrap_or("<no error from compute_ctl>")
                            );
                        }
                        ComputeStatus::Empty
                        | ComputeStatus::ConfigurationPending
                        | ComputeStatus::Configuration
                        | ComputeStatus::TerminationPendingFast
                        | ComputeStatus::TerminationPendingImmediate
                        | ComputeStatus::Terminated
                        | ComputeStatus::RefreshConfigurationPending
                        | ComputeStatus::RefreshConfiguration => {
                            bail!("unexpected compute status: {:?}", state.status)
                        }
                    }
                }
                Err(e) => {
                    if Instant::now().duration_since(start_at) > args.start_timeout {
                        return Err(e).context(format!(
                            "timed out {:?} waiting to connect to compute_ctl HTTP",
                            args.start_timeout
                        ));
                    }
                }
            }
            tokio::time::sleep(ATTEMPT_INTERVAL).await;
        }

        // disarm the scopeguard, let the child outlive this function (and yb_light_local invoction)
        drop(scopeguard::ScopeGuard::into_inner(child));

        Ok(())
    }

    // Update the pageservers in the spec file of the endpoint. This is useful to test the spec refresh scenario.
    pub async fn update_pageservers_in_config(
        &self,
        pageserver_conninfo: &PageserverConnectionInfo,
    ) -> Result<()> {
        let config_path = self.endpoint_path().join("config.json");
        let mut config: ComputeConfig = {
            let file = std::fs::File::open(&config_path)?;
            serde_json::from_reader(file)?
        };

        let mut spec = config.spec.unwrap();
        spec.pageserver_connection_info = Some(pageserver_conninfo.clone());
        config.spec = Some(spec);

        let file = std::fs::File::create(&config_path)?;
        serde_json::to_writer_pretty(file, &config)?;

        Ok(())
    }

    // Call the /status HTTP API
    pub async fn get_status(&self) -> Result<ComputeStatusResponse> {
        let client = reqwest::Client::new();

        let response = client
            .request(
                reqwest::Method::GET,
                format!(
                    "http://{}:{}/status",
                    self.external_http_address.ip(),
                    self.external_http_address.port()
                ),
            )
            .bearer_auth(self.generate_jwt(None::<ComputeClaimsScope>)?)
            .send()
            .await?;

        // Interpret the response
        let status = response.status();
        if !(status.is_client_error() || status.is_server_error()) {
            Ok(response.json().await?)
        } else {
            // reqwest does not export its error construction utility functions, so let's craft the message ourselves
            let url = response.url().to_owned();
            let msg = match response.text().await {
                Ok(err_body) => format!("Error: {err_body}"),
                Err(_) => format!("Http error ({}) at {}.", status.as_u16(), url),
            };
            Err(anyhow::anyhow!(msg))
        }
    }

    pub async fn reconfigure(
        &self,
        pageserver_conninfo: Option<&PageserverConnectionInfo>,
        safekeepers: Option<Vec<NodeId>>,
        safekeeper_generation: Option<SafekeeperGeneration>,
    ) -> Result<()> {
        let (mut spec, compute_ctl_config) = {
            let config_path = self.endpoint_path().join("config.json");
            let file = std::fs::File::open(config_path)?;
            let config: ComputeConfig = serde_json::from_reader(file)?;

            (config.spec.unwrap(), config.compute_ctl_config)
        };

        let postgresql_conf = self.read_postgresql_conf()?;
        spec.cluster.postgresql_conf = Some(postgresql_conf);

        if let Some(pageserver_conninfo) = pageserver_conninfo {
            // If pageservers are provided, we need to ensure that they are not empty.
            // This is a requirement for the compute_ctl configuration.
            anyhow::ensure!(
                !pageserver_conninfo.pageservers.is_empty(),
                "no pageservers provided"
            );
            spec.pageserver_connection_info = Some(pageserver_conninfo.clone());
        }

        // If safekeepers are not specified, don't change them.
        if let Some(safekeepers) = safekeepers {
            let safekeeper_connstrings = self.build_safekeepers_connstrs(safekeepers)?;
            spec.safekeeper_connstrings = safekeeper_connstrings;
            if let Some(g) = safekeeper_generation {
                spec.safekeepers_generation = Some(g.into_inner());
            }
        }

        let client = reqwest::Client::builder()
            .timeout(Duration::from_secs(120))
            .build()
            .unwrap();
        let response = client
            .post(format!(
                "http://{}:{}/configure",
                self.external_http_address.ip(),
                self.external_http_address.port()
            ))
            .header(CONTENT_TYPE.as_str(), "application/json")
            .bearer_auth(self.generate_jwt(None::<ComputeClaimsScope>)?)
            .body(
                serde_json::to_string(&ConfigurationRequest {
                    spec,
                    compute_ctl_config,
                })
                .unwrap(),
            )
            .send()
            .await?;

        let status = response.status();
        if !(status.is_client_error() || status.is_server_error()) {
            Ok(())
        } else {
            let url = response.url().to_owned();
            let msg = match response.text().await {
                Ok(err_body) => format!("Error: {err_body}"),
                Err(_) => format!("Http error ({}) at {}.", status.as_u16(), url),
            };
            Err(anyhow::anyhow!(msg))
        }
    }

    pub async fn reconfigure_pageservers(
        &self,
        pageservers: &PageserverConnectionInfo,
    ) -> Result<()> {
        self.reconfigure(Some(pageservers), None, None).await
    }

    pub async fn reconfigure_safekeepers(
        &self,
        safekeepers: Vec<NodeId>,
        generation: SafekeeperGeneration,
    ) -> Result<()> {
        self.reconfigure(None, Some(safekeepers), Some(generation))
            .await
    }

    pub async fn stop(
        &self,
        mode: EndpointTerminateMode,
        destroy: bool,
    ) -> Result<TerminateResponse> {
        // An endpoint that was created but never started has no pgdata and
        // no compute to stop — skip straight to teardown (used by `endpoint
        // stop --destroy` for cleanup of a create-but-never-started
        // endpoint, e.g. a co-located pair provisioned at `endpoint create`).
        let started = self.pgdata().exists();

        // pg_ctl stop is fast but doesn't allow us to collect LSN. /terminate is
        // slow, and test runs time out. Solution: special mode "immediate-terminate"
        // which uses /terminate
        let response = if !started {
            TerminateResponse { lsn: None }
        } else if let EndpointTerminateMode::ImmediateTerminate = mode {
            let ip = self.external_http_address.ip();
            let port = self.external_http_address.port();
            let url = format!("http://{ip}:{port}/terminate?mode=immediate");
            let token = self.generate_jwt(Some(ComputeClaimsScope::Admin))?;
            let request = reqwest::Client::new().post(url).bearer_auth(token);
            let response = request.send().await.context("/terminate")?;
            let text = response.text().await.context("/terminate result")?;
            serde_json::from_str(&text).with_context(|| format!("deserializing {text}"))?
        } else {
            self.pg_ctl(&["-m", &mode.to_string(), "stop"], &None)?;
            TerminateResponse { lsn: None }
        };

        // Also wait for the compute_ctl process to die. It reaps the postgres
        // process tree (postmaster + any straggler backends) before exiting, so
        // by the time it is gone the client port is free for the next compute.
        //
        // If destroying or stop mode is immediate, send it SIGTERM before
        // waiting.
        if started {
            let send_sigterm = destroy || !matches!(mode, EndpointTerminateMode::Fast);
            self.wait_for_compute_ctl_to_exit(send_sigterm)?;
        }
        if destroy {
            println!(
                "Destroying postgres data directory '{}'",
                self.pgdata().to_str().unwrap()
            );
            // If this endpoint owns a co-located pageserver+safekeeper
            // (it was the first endpoint created for its timeline), tear
            // them down before removing the folder, and drop the
            // safekeeper from the persisted config. v1 semantics: stop the
            // processes and remove the dir — no cascade / refusal when a
            // sibling endpoint still references the timeline.
            self.deprovision_colocated_storage()?;
            // Remove the local working directory if it exists. Unlike before,
            // this is just runtime state — the endpoint's durable record is the
            // row we delete below, not this directory.
            if self.endpoint_path().exists() {
                std::fs::remove_dir_all(self.endpoint_path())?;
            }
            // Drop the endpoint from the storage controller's `endpoints` table.
            StorageController::from_env(&self.env)
                .endpoint_delete(&self.endpoint_id)
                .await
                .context("deleting endpoint from storage controller")?;
        }
        Ok(response)
    }

    /// Stop the pageserver, safekeeper and endpoint_storage co-located
    /// under this endpoint's folder (if any) and drop the safekeeper and
    /// endpoint_storage from the persisted config. The pageserver leaves no
    /// config trace — it is discovered from its `pageserver/` subdir, which
    /// the caller removes with the folder.
    fn deprovision_colocated_storage(&self) -> Result<()> {
        let endpoint_path = self.endpoint_path();

        // Pageserver: the one whose datadir lives under this endpoint.
        for ps_conf in &self.env.pageservers {
            if ps_conf.data_dir.starts_with(&endpoint_path) {
                let pageserver = PageServerNode::from_env(&self.env, ps_conf);
                if let Err(e) = pageserver.stop(true) {
                    eprintln!("pageserver {} stop failed: {e:#}", ps_conf.id);
                }
            }
        }

        // Safekeeper(s): the ones owned by this endpoint.
        let owned_sk_ids: Vec<_> = self
            .env
            .safekeepers
            .iter()
            .filter(|sk| sk.endpoint_id == self.endpoint_id)
            .map(|sk| sk.id)
            .collect();
        for sk_conf in self
            .env
            .safekeepers
            .iter()
            .filter(|sk| owned_sk_ids.contains(&sk.id))
        {
            let safekeeper = SafekeeperNode::from_env(&self.env, sk_conf);
            if let Err(e) = safekeeper.stop(true) {
                eprintln!("safekeeper {} stop failed: {e:#}", sk_conf.id);
            }
        }

        // endpoint_storage: this endpoint's own instance.
        let owns_endpoint_storage = self
            .env
            .endpoint_storages
            .iter()
            .any(|es| es.endpoint_id == self.endpoint_id);
        for es_conf in self
            .env
            .endpoint_storages
            .iter()
            .filter(|es| es.endpoint_id == self.endpoint_id)
        {
            let storage = EndpointStorage::from_conf(&self.env, es_conf);
            if let Err(e) = storage.stop(true) {
                eprintln!("endpoint_storage for endpoint {} stop failed: {e:#}", self.endpoint_id);
            }
        }

        if !owned_sk_ids.is_empty() || owns_endpoint_storage {
            let mut env = self.env.clone();
            env.safekeepers
                .retain(|sk| !owned_sk_ids.contains(&sk.id));
            env.endpoint_storages
                .retain(|es| es.endpoint_id != self.endpoint_id);
            env.persist_config()
                .context("persist config after dropping co-located storage")?;
        }

        Ok(())
    }

    pub async fn refresh_configuration(&self) -> Result<()> {
        let client = reqwest::Client::builder()
            .timeout(Duration::from_secs(30))
            .build()
            .unwrap();
        let response = client
            .post(format!(
                "http://{}:{}/refresh_configuration",
                self.internal_http_address.ip(),
                self.internal_http_address.port()
            ))
            .send()
            .await?;

        let status = response.status();
        if !(status.is_client_error() || status.is_server_error()) {
            Ok(())
        } else {
            let url = response.url().to_owned();
            let msg = match response.text().await {
                Ok(err_body) => format!("Error: {err_body}"),
                Err(_) => format!("Http error ({}) at {}.", status.as_u16(), url),
            };
            Err(anyhow::anyhow!(msg))
        }
    }

    pub fn connstr(&self, user: &str, db_name: &str) -> String {
        format!(
            "postgresql://{}@{}:{}/{}",
            user,
            self.pg_address.ip(),
            self.pg_address.port(),
            db_name
        )
    }
}

/// If caller is telling us what pageserver to use, this is not a tenant which is
/// fully managed by storage controller, therefore not sharded.
pub fn local_pageserver_conf_to_conn_info(
    conf: &crate::local_env::PageServerConf,
    use_tls: bool,
) -> Result<PageserverConnectionInfo> {
    let libpq_url = {
        let (host, port) = parse_host_port(&conf.listen_pg_addr)?;
        let port = port.unwrap_or(5432);
        let sslmode = if use_tls { "" } else { "?sslmode=disable" };
        Some(format!("postgres://no_user@{host}:{port}{sslmode}"))
    };
    let grpc_url = if let Some(grpc_addr) = &conf.listen_grpc_addr {
        let (host, port) = parse_host_port(grpc_addr)?;
        let port = port.unwrap_or(DEFAULT_PAGESERVER_GRPC_PORT);
        Some(format!("grpc://no_user@{host}:{port}"))
    } else {
        None
    };
    let ps_conninfo = PageserverConnectionItem {
        id: Some(conf.id),
        libpq_url,
        grpc_url,
    };

    Ok(PageserverConnectionInfo {
        pageservers: vec![ps_conninfo],
        prefer_protocol: PageserverProtocol::default(),
    })
}
