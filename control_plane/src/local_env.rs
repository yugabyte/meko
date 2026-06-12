//! This module is responsible for locating and loading paths in a local setup.
//!
//! Now it also provides init method which acts like a stub for proper installation
//! script which will use local paths.

use std::collections::HashMap;
use std::net::SocketAddr;
use std::path::{Path, PathBuf};
use std::process::{Command, Stdio};
use std::time::Duration;
use std::{env, fs};

use anyhow::{Context, bail};
use clap::ValueEnum;
use pem::Pem;
use postgres_backend::AuthType;
use reqwest::{Certificate, Url};
use safekeeper_api::PgMajorVersion;
use serde::{Deserialize, Serialize};
use utils::auth::encode_from_key_file;
use utils::id::{NodeId, TenantId, TenantTimelineId, TimelineId};

use crate::endpoint_storage::ENDPOINT_STORAGE_DEFAULT_ADDR;

pub const DEFAULT_PG_VERSION: u32 = 17;

//
// This data structures represents yb_light_local CLI config
//
// It is deserialized from the .yb_light/config file, or the config file passed
// to 'yb_light_local init --config=<path>' option. See control_plane/simple.conf for
// an example.
//
#[derive(PartialEq, Clone, Debug)]
pub struct LocalEnv {
    // Base directory for all the nodes (the pageserver, safekeepers and
    // compute endpoints).
    //
    // This is not stored in the config file. Rather, this is the path where the
    // config file itself is. It is read from the YB_LIGHT_REPO_DIR env variable which
    // must be an absolute path. If the env var is not set, $PWD/.yb_light is used.
    pub base_data_dir: PathBuf,

    // Path to postgres distribution. It's expected that "bin", "include",
    // "lib", "share" from postgres distribution are there. If at some point
    // in time we will be able to run against vanilla postgres we may split that
    // to four separate paths and match OS-specific installation layout.
    pub pg_distrib_dir: PathBuf,

    // Path to pageserver binary.
    pub yb_light_distrib_dir: PathBuf,

    // Default tenant ID to use with the 'yb_light_local' command line utility, when
    // --tenant_id is not explicitly specified.
    pub default_tenant_id: Option<TenantId>,

    // used to issue tokens during e.g pg start
    pub private_key_path: PathBuf,
    /// Path to environment's public key
    pub public_key_path: PathBuf,

    // Configuration for the storage controller (1 per yb_light_local environment)
    pub storage_controller: YbLightStorageControllerConf,

    /// The *provisioned* pageservers, discovered by [`Self::load_config`]
    /// from the `pageserver.toml` under each `endpoints/<name>/pageserver/`.
    /// Empty on a fresh repo; pageservers are provisioned at
    /// `endpoint create`. The launcher allocates new ones from
    /// [`Self::pageserver_templates`].
    pub pageservers: Vec<PageServerConf>,

    /// The *provisioned* safekeepers, persisted in `.yb_light/config`.
    /// Empty on a fresh repo; a safekeeper is provisioned (datadir,
    /// `endpoint_id`, `timeline_id`) at `endpoint create`, allocated from
    /// [`Self::safekeeper_templates`].
    pub safekeepers: Vec<SafekeeperConf>,

    /// Init-time pageserver conf(s) from the `yb_light_local init` TOML.
    /// Not a running node — the template (ports, `remote_storage`, auth)
    /// that `endpoint create` clones for the first/main pageserver. The
    /// lowest-id template is the port base for all pageserver allocation.
    pub pageserver_templates: Vec<YbLightLocalInitPageserverConf>,

    /// Init-time safekeeper conf(s) from the `yb_light_local init` TOML.
    /// The template (ports, `wal_yb_config`, auth) that `endpoint create`
    /// clones for the first/main safekeeper.
    pub safekeeper_templates: Vec<SafekeeperConf>,

    /// Init-time endpoint_storage conf from the `yb_light_local init` TOML:
    /// the template (base `listen_addr`, `remote_storage_json`) that
    /// `endpoint create` clones for each endpoint's instance.
    pub endpoint_storage: EndpointStorageConf,

    /// Per-endpoint endpoint_storage instances, one per endpoint, each
    /// with its own `listen_addr` and a datadir under
    /// `endpoints/<endpoint_id>/endpoint_storage`. Starts empty on a fresh
    /// repo; populated as endpoints are created.
    pub endpoint_storages: Vec<EndpointStorageConf>,

    // Control plane upcall API for pageserver: if None, we will not run storage_controller  If set, this will
    // be propagated into each pageserver's configuration.
    pub control_plane_api: Url,

    // Control plane upcall APIs for storage controller.  If set, this will be propagated into the
    // storage controller's configuration.
    pub control_plane_hooks_api: Option<Url>,

    /// Flag to generate SSL certificates for components that need it.
    /// Also generates root CA certificate that is used to sign all other certificates.
    pub use_local_tls: bool,
}

/// On-disk state stored in `.yb_light/config`.
#[derive(PartialEq, Clone, Debug, Default, Serialize, Deserialize)]
#[serde(default, deny_unknown_fields)]
pub struct OnDiskConfig {
    pub pg_distrib_dir: PathBuf,
    pub yb_light_distrib_dir: PathBuf,
    pub default_tenant_id: Option<TenantId>,
    pub private_key_path: PathBuf,
    pub public_key_path: PathBuf,
    pub storage_controller: YbLightStorageControllerConf,
    #[serde(
        skip_serializing,
        deserialize_with = "fail_if_pageservers_field_specified"
    )]
    pub pageservers: Vec<PageServerConf>,
    pub safekeepers: Vec<SafekeeperConf>,
    pub pageserver_templates: Vec<YbLightLocalInitPageserverConf>,
    pub safekeeper_templates: Vec<SafekeeperConf>,
    pub endpoint_storage: EndpointStorageConf,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub endpoint_storages: Vec<EndpointStorageConf>,
    pub control_plane_api: Option<Url>,
    pub control_plane_hooks_api: Option<Url>,
    pub control_plane_compute_hook_api: Option<Url>,
    // Note: skip serializing because in compat tests old storage controller fails
    // to load new config file. May be removed after this field is in release branch.
    #[serde(skip_serializing_if = "std::ops::Not::not")]
    pub use_local_tls: bool,
}

fn fail_if_pageservers_field_specified<'de, D>(_: D) -> Result<Vec<PageServerConf>, D::Error>
where
    D: serde::Deserializer<'de>,
{
    Err(serde::de::Error::custom(
        "The 'pageservers' field is no longer used; pageserver.toml is now authoritative; \
         Please remove the `pageservers` from your .yb_light/config.",
    ))
}

/// The description of the yb_light_local env to be initialized by `yb_light_local init --config`.
#[derive(Clone, Debug, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct YbLightLocalInitConf {
    // TODO: do we need this? Seems unused
    pub pg_distrib_dir: Option<PathBuf>,
    // TODO: do we need this? Seems unused
    pub yb_light_distrib_dir: Option<PathBuf>,
    pub default_tenant_id: TenantId,
    pub storage_controller: Option<YbLightStorageControllerConf>,
    pub pageservers: Vec<YbLightLocalInitPageserverConf>,
    pub safekeepers: Vec<SafekeeperConf>,
    pub endpoint_storage: EndpointStorageConf,
    pub control_plane_api: Option<Url>,
    pub control_plane_hooks_api: Option<Url>,
    #[serde(default)]
    pub use_local_tls: bool,
}

#[derive(Serialize, Deserialize, PartialEq, Eq, Clone, Debug)]
#[serde(default)]
pub struct EndpointStorageConf {
    pub listen_addr: SocketAddr,
    /// If `Some`, written verbatim as the `storage_kind` block of
    /// `endpoint_storage.json`. Otherwise yb_light_local derives a default
    /// `YbStore` config from `storage_controller.database_url`,
    /// colocating endpoint_storage with the pageserver in the same
    /// `yb_storage.public.blobs` table. Value must be a JSON object
    /// including the `type` discriminator, e.g.
    /// `{"type": "YbStore", "host": "...", "user": "...", ...}`. Mirrors
    /// the way [`YbLightLocalInitPageserverConf::remote_storage`] feeds the
    /// pageserver's TOML inline table — opaque to yb_light_local, parsed
    /// by `TypedRemoteStorageKind` in the target service.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub remote_storage_json: Option<String>,
    /// The endpoint whose folder holds this instance's datadir
    /// (`endpoints/<endpoint_id>/endpoint_storage`). Empty for the
    /// init-time template; set at `endpoint create` when an instance is
    /// provisioned under its owning endpoint.
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub endpoint_id: String,
}

/// A part of storage controller's config the yb_light_local knows about.
#[derive(Serialize, Deserialize, PartialEq, Eq, Clone, Debug)]
#[serde(default)]
pub struct YbLightStorageControllerConf {
    /// Heartbeat timeout before marking a node offline
    #[serde(with = "humantime_serde")]
    pub max_offline: Duration,

    #[serde(with = "humantime_serde")]
    pub max_warming_up: Duration,


    /// Database connection string for the storage controller's persistence
    /// layer. When `Some`, yb_light_local skips the local Postgres bootstrap
    /// (initdb / pg_ctl / createdb / startup-script tx) and passes this URL
    /// straight through to the storage controller as `--database-url`. This
    /// is how yb-neon points the controller at YugabyteDB; it's also useful
    /// for running multiple controller instances against a shared DB.
    /// Example: `postgresql://yugabyte@127.0.0.1:5433/storage_controller`.
    pub database_url: Option<String>,

    #[serde(with = "humantime_serde")]
    pub heartbeat_interval: Duration,

    pub use_https_pageserver_api: bool,

    pub timelines_onto_safekeepers: bool,

    pub use_https_safekeeper_api: bool,

    pub use_local_compute_notifications: bool,

    pub timeline_safekeeper_count: Option<usize>,
}

impl YbLightStorageControllerConf {
    // Use a shorter pageserver unavailability interval than the default to speed up tests.
    const DEFAULT_MAX_OFFLINE_INTERVAL: std::time::Duration = std::time::Duration::from_secs(10);

    const DEFAULT_MAX_WARMING_UP_INTERVAL: std::time::Duration = std::time::Duration::from_secs(30);

    // Very tight heartbeat interval to speed up tests
    const DEFAULT_HEARTBEAT_INTERVAL: std::time::Duration = std::time::Duration::from_millis(1000);
}

impl Default for YbLightStorageControllerConf {
    fn default() -> Self {
        Self {
            max_offline: Self::DEFAULT_MAX_OFFLINE_INTERVAL,
            max_warming_up: Self::DEFAULT_MAX_WARMING_UP_INTERVAL,
            database_url: None,
            heartbeat_interval: Self::DEFAULT_HEARTBEAT_INTERVAL,
            use_https_pageserver_api: false,
            timelines_onto_safekeepers: true,
            use_https_safekeeper_api: false,
            use_local_compute_notifications: true,
            timeline_safekeeper_count: None,
        }
    }
}

impl Default for EndpointStorageConf {
    fn default() -> Self {
        Self {
            listen_addr: ENDPOINT_STORAGE_DEFAULT_ADDR,
            remote_storage_json: None,
            endpoint_id: String::new(),
        }
    }
}

// yb_light_local needs to know this subset of pageserver configuration.
// For legacy reasons, this information is duplicated from `pageserver.toml` into `.yb_light/config`.
// It can get stale if `pageserver.toml` is changed.
// TODO(christian): don't store this at all in `.yb_light/config`, always load it from `pageserver.toml`
#[derive(Serialize, Deserialize, PartialEq, Eq, Clone, Debug)]
#[serde(default, deny_unknown_fields)]
pub struct PageServerConf {
    pub id: NodeId,
    pub listen_pg_addr: String,
    pub listen_http_addr: String,
    pub listen_https_addr: Option<String>,
    pub listen_grpc_addr: Option<String>,
    pub pg_auth_type: AuthType,
    pub http_auth_type: AuthType,
    pub grpc_auth_type: AuthType,
    pub no_sync: bool,
    /// The single (tenant, timeline) this pageserver serves, read from
    /// the `tenant_id` / `timeline_id` keys of its `pageserver.toml`.
    /// `None` until the launcher assigns one (`tenant create` /
    /// `timeline branch`).
    pub tenant_id: Option<TenantId>,
    pub timeline_id: Option<TimelineId>,
    /// The pageserver's data directory (`endpoints/<name>/pageserver`),
    /// captured at discovery / allocation time. Not part of the on-disk
    /// config — derived from where the `pageserver.toml` was found, or set
    /// when the launcher allocates a new pageserver at `endpoint create`.
    #[serde(skip)]
    pub data_dir: PathBuf,
}

impl Default for PageServerConf {
    fn default() -> Self {
        Self {
            id: NodeId(0),
            listen_pg_addr: String::new(),
            listen_http_addr: String::new(),
            listen_https_addr: None,
            listen_grpc_addr: None,
            pg_auth_type: AuthType::Trust,
            http_auth_type: AuthType::Trust,
            grpc_auth_type: AuthType::Trust,
            no_sync: false,
            tenant_id: None,
            timeline_id: None,
            data_dir: PathBuf::new(),
        }
    }
}

/// The toml that can be passed to `yb_light_local init --config`.
/// This is a subset of the `pageserver.toml` configuration.
// TODO(christian): use pageserver_api::config::ConfigToml (PR #7656)
#[derive(Clone, Debug, PartialEq, serde::Deserialize, serde::Serialize)]
pub struct YbLightLocalInitPageserverConf {
    pub id: NodeId,
    pub listen_pg_addr: String,
    pub listen_http_addr: String,
    pub listen_https_addr: Option<String>,
    pub listen_grpc_addr: Option<String>,
    pub pg_auth_type: AuthType,
    pub http_auth_type: AuthType,
    pub grpc_auth_type: AuthType,
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    pub no_sync: bool,
    /// If `Some`, written verbatim into `pageserver.toml` as
    /// `remote_storage = <value>`. Value must be a TOML inline table for
    /// `YbStore` (e.g. `{ host = "172.151.17.132", port = 5433, ... }`).
    /// Required — pageserver init bails if neither this nor `other.remote_storage`
    /// is set. Bypasses the `#[serde(flatten)] other` round-trip that
    /// would otherwise mangle inline tables.
    #[serde(default)]
    pub remote_storage: Option<String>,
    #[serde(flatten)]
    pub other: HashMap<String, toml::Value>,
}

impl From<&YbLightLocalInitPageserverConf> for PageServerConf {
    fn from(conf: &YbLightLocalInitPageserverConf) -> Self {
        let YbLightLocalInitPageserverConf {
            id,
            listen_pg_addr,
            listen_http_addr,
            listen_https_addr,
            listen_grpc_addr,
            pg_auth_type,
            http_auth_type,
            grpc_auth_type,
            no_sync,
            remote_storage: _,
            other: _,
        } = conf;
        Self {
            id: *id,
            listen_pg_addr: listen_pg_addr.clone(),
            listen_http_addr: listen_http_addr.clone(),
            listen_https_addr: listen_https_addr.clone(),
            listen_grpc_addr: listen_grpc_addr.clone(),
            pg_auth_type: *pg_auth_type,
            grpc_auth_type: *grpc_auth_type,
            http_auth_type: *http_auth_type,
            no_sync: *no_sync,
            tenant_id: None,
            timeline_id: None,
            data_dir: PathBuf::new(),
        }
    }
}

#[derive(Serialize, Deserialize, PartialEq, Eq, Clone, Debug)]
#[serde(default)]
pub struct SafekeeperConf {
    pub id: NodeId,
    pub pg_port: u16,
    pub pg_tenant_only_port: Option<u16>,
    pub http_port: u16,
    pub https_port: Option<u16>,
    pub backup_threads: Option<u32>,
    pub auth_enabled: bool,
    pub listen_addr: Option<String>,
    /// If `Some`, mirrors every live WAL write into YugabyteDB via the
    /// `wals` table. Value is the full `YbStoreConfig` TOML inline string
    /// that the safekeeper accepts as `--wal-yb-config`. Single-safekeeper
    /// constraint applies (see architecture/safekeeper.md §5).
    pub wal_yb_config: Option<String>,
    /// Capacity (MiB) of the in-memory cache of recent WAL chunks used in
    /// YB-WAL mode. Maps to `--wal-cache-size-mib`. Ignored without
    /// `wal_yb_config`. `Some(0)` disables the cache; `None` lets the
    /// safekeeper apply its built-in default.
    pub wal_cache_size_mib: Option<u32>,
    /// The timeline this safekeeper is dedicated to (per-endpoint
    /// safekeeper topology). `None` for the legacy/main safekeeper
    /// that serves the initial timeline.
    pub timeline_id: Option<TimelineId>,
    /// The endpoint whose folder holds this safekeeper's datadir
    /// (`endpoints/<endpoint_id>/safekeeper`). Empty for an init-time
    /// template; set at `endpoint create` when the safekeeper is
    /// provisioned under its owning endpoint.
    pub endpoint_id: String,
}

impl Default for SafekeeperConf {
    fn default() -> Self {
        Self {
            id: NodeId(0),
            pg_port: 0,
            pg_tenant_only_port: None,
            http_port: 0,
            https_port: None,
            backup_threads: None,
            auth_enabled: false,
            listen_addr: None,
            wal_yb_config: None,
            wal_cache_size_mib: None,
            timeline_id: None,
            endpoint_id: String::new(),
        }
    }
}

#[derive(Clone, Copy)]
pub enum InitForceMode {
    MustNotExist,
    EmptyDirOk,
    RemoveAllContents,
}

impl ValueEnum for InitForceMode {
    fn value_variants<'a>() -> &'a [Self] {
        &[
            Self::MustNotExist,
            Self::EmptyDirOk,
            Self::RemoveAllContents,
        ]
    }

    fn to_possible_value(&self) -> Option<clap::builder::PossibleValue> {
        Some(clap::builder::PossibleValue::new(match self {
            InitForceMode::MustNotExist => "must-not-exist",
            InitForceMode::EmptyDirOk => "empty-dir-ok",
            InitForceMode::RemoveAllContents => "remove-all-contents",
        }))
    }
}

impl SafekeeperConf {
    /// Compute is served by port on which only tenant scoped tokens allowed, if
    /// it is configured.
    pub fn get_compute_port(&self) -> u16 {
        self.pg_tenant_only_port.unwrap_or(self.pg_port)
    }
}

impl LocalEnv {
    /// The pageserver dedicated to `timeline_id` (per-endpoint pageserver
    /// topology: one pageserver process per timeline, identity baked into
    /// its `pageserver.toml`).
    pub fn pageserver_conf_for_timeline(&self, timeline_id: TimelineId) -> Option<&PageServerConf> {
        self.pageservers
            .iter()
            .find(|ps| ps.timeline_id == Some(timeline_id))
    }

    /// The safekeepers serving `timeline_id`: the dedicated one if
    /// provisioned, otherwise the legacy set (safekeepers with no
    /// `timeline_id`, i.e. the main timeline's).
    pub fn safekeepers_for_timeline(&self, timeline_id: TimelineId) -> Vec<&SafekeeperConf> {
        let dedicated: Vec<&SafekeeperConf> = self
            .safekeepers
            .iter()
            .filter(|sk| sk.timeline_id == Some(timeline_id))
            .collect();
        if !dedicated.is_empty() {
            return dedicated;
        }
        self.safekeepers
            .iter()
            .filter(|sk| sk.timeline_id.is_none())
            .collect()
    }
}

impl LocalEnv {
    pub fn pg_distrib_dir_raw(&self) -> PathBuf {
        self.pg_distrib_dir.clone()
    }

    pub fn pg_distrib_dir(&self, pg_version: PgMajorVersion) -> anyhow::Result<PathBuf> {
        let path = self.pg_distrib_dir.clone();

        Ok(path.join(pg_version.v_str()))
    }

    pub fn pg_dir(&self, pg_version: PgMajorVersion, dir_name: &str) -> anyhow::Result<PathBuf> {
        Ok(self.pg_distrib_dir(pg_version)?.join(dir_name))
    }

    pub fn pg_bin_dir(&self, pg_version: PgMajorVersion) -> anyhow::Result<PathBuf> {
        self.pg_dir(pg_version, "bin")
    }

    pub fn pg_lib_dir(&self, pg_version: PgMajorVersion) -> anyhow::Result<PathBuf> {
        self.pg_dir(pg_version, "lib")
    }

    pub fn endpoint_storage_bin(&self) -> PathBuf {
        self.yb_light_distrib_dir.join("endpoint_storage")
    }

    pub fn pageserver_bin(&self) -> PathBuf {
        // The pageserver binary is `yb-pageserver` (process names
        // carry the yb- prefix; folder + crate are bare `pageserver`).
        self.yb_light_distrib_dir.join("yb-pageserver")
    }

    pub fn storage_controller_bin(&self) -> PathBuf {
        // Irrespective of configuration, storage controller binary is always
        // run from the same location as yb_light_local.  This means that for compatibility
        // tests that run old pageserver/safekeeper, they still run latest storage controller.
        let yb_light_local_bin_dir = env::current_exe().unwrap().parent().unwrap().to_owned();
        yb_light_local_bin_dir.join("storage_controller")
    }

    pub fn safekeeper_bin(&self) -> PathBuf {
        self.yb_light_distrib_dir.join("safekeeper")
    }

    pub fn endpoints_path(&self) -> PathBuf {
        self.base_data_dir.join("endpoints")
    }

    /// A pageserver's datadir, co-located under its owning endpoint:
    /// `endpoints/<endpoint_id>/pageserver`.
    pub fn pageserver_data_dir(&self, endpoint_id: &str) -> PathBuf {
        self.endpoints_path().join(endpoint_id).join("pageserver")
    }

    /// A safekeeper's datadir, co-located under its owning endpoint:
    /// `endpoints/<endpoint_id>/safekeeper`.
    pub fn safekeeper_data_dir(&self, endpoint_id: &str) -> PathBuf {
        self.endpoints_path().join(endpoint_id).join("safekeeper")
    }

    /// `endpoints/<endpoint_id>/endpoint_storage`.
    pub fn endpoint_storage_data_dir(&self, endpoint_id: &str) -> PathBuf {
        self.endpoints_path()
            .join(endpoint_id)
            .join("endpoint_storage")
    }

    pub fn get_pageserver_conf(&self, id: NodeId) -> anyhow::Result<&PageServerConf> {
        if let Some(conf) = self.pageservers.iter().find(|node| node.id == id) {
            Ok(conf)
        } else {
            let have_ids = self
                .pageservers
                .iter()
                .map(|node| format!("{}:{}", node.id, node.listen_http_addr))
                .collect::<Vec<_>>();
            let joined = have_ids.join(",");
            bail!("could not find pageserver {id}, have ids {joined}")
        }
    }

    pub fn ssl_ca_cert_path(&self) -> Option<PathBuf> {
        if self.use_local_tls {
            Some(self.base_data_dir.join("rootCA.crt"))
        } else {
            None
        }
    }

    pub fn ssl_ca_key_path(&self) -> Option<PathBuf> {
        if self.use_local_tls {
            Some(self.base_data_dir.join("rootCA.key"))
        } else {
            None
        }
    }

    pub fn generate_ssl_ca_cert(&self) -> anyhow::Result<()> {
        let cert_path = self.ssl_ca_cert_path().unwrap();
        let key_path = self.ssl_ca_key_path().unwrap();
        if !fs::exists(cert_path.as_path())? {
            generate_ssl_ca_cert(cert_path.as_path(), key_path.as_path())?;
        }
        Ok(())
    }

    pub fn generate_ssl_cert(&self, cert_path: &Path, key_path: &Path) -> anyhow::Result<()> {
        self.generate_ssl_ca_cert()?;
        generate_ssl_cert(
            cert_path,
            key_path,
            self.ssl_ca_cert_path().unwrap().as_path(),
            self.ssl_ca_key_path().unwrap().as_path(),
        )
    }

    /// Creates HTTP client with local SSL CA certificates.
    pub fn create_http_client(&self) -> reqwest::Client {
        let ssl_ca_certs = self.ssl_ca_cert_path().map(|ssl_ca_file| {
            let buf = std::fs::read(ssl_ca_file).expect("SSL CA file should exist");
            Certificate::from_pem_bundle(&buf).expect("SSL CA file should be valid")
        });

        let mut http_client = reqwest::Client::builder();
        for ssl_ca_cert in ssl_ca_certs.unwrap_or_default() {
            http_client = http_client.add_root_certificate(ssl_ca_cert);
        }

        http_client
            .build()
            .expect("HTTP client should construct with no error")
    }

    /// Inspect the base data directory and extract the instance id and instance directory path
    /// for all storage controller instances
    pub async fn storage_controller_instances(&self) -> std::io::Result<Vec<(u8, PathBuf)>> {
        let mut instances = Vec::default();

        let dir = std::fs::read_dir(self.base_data_dir.clone())?;
        for dentry in dir {
            let dentry = dentry?;
            let is_dir = dentry.metadata()?.is_dir();
            let filename = dentry.file_name().into_string().unwrap();
            let parsed_instance_id = match filename.strip_prefix("storage_controller_") {
                Some(suffix) => suffix.parse::<u8>().ok(),
                None => None,
            };

            let is_instance_dir = is_dir && parsed_instance_id.is_some();

            if !is_instance_dir {
                continue;
            }

            instances.push((
                parsed_instance_id.expect("Checked previously"),
                dentry.path(),
            ));
        }

        Ok(instances)
    }

    /// Persist a branch alias for the given timeline by calling into the storage controller.
    /// Idempotent if the alias already points at the same timeline; rejects re-pointing.
    pub async fn register_branch_mapping(
        &self,
        branch_name: String,
        tenant_id: TenantId,
        timeline_id: TimelineId,
    ) -> anyhow::Result<()> {
        let sc = crate::storage_controller::StorageController::from_env(self);
        let existing = sc.list_timeline_branch_names().await?;
        if let Some(entry) = existing.iter().find(|e| e.branch_name == branch_name) {
            if entry.tenant_id == tenant_id && entry.timeline_id == timeline_id {
                return Ok(());
            }
            if entry.tenant_id == tenant_id {
                bail!(
                    "branch '{branch_name}' is already mapped to timeline {}, cannot map to another timeline {timeline_id}",
                    entry.timeline_id
                );
            }
        }
        sc.set_timeline_branch_name(tenant_id, timeline_id, Some(branch_name))
            .await
    }

    pub async fn get_branch_timeline_id(
        &self,
        branch_name: &str,
        tenant_id: TenantId,
    ) -> anyhow::Result<Option<TimelineId>> {
        let sc = crate::storage_controller::StorageController::from_env(self);
        let entries = sc.list_timeline_branch_names().await?;
        Ok(entries
            .into_iter()
            .find(|e| e.branch_name == branch_name && e.tenant_id == tenant_id)
            .map(|e| e.timeline_id))
    }

    pub async fn timeline_name_mappings(&self) -> anyhow::Result<HashMap<TenantTimelineId, String>> {
        let sc = crate::storage_controller::StorageController::from_env(self);
        let entries = sc.list_timeline_branch_names().await?;
        Ok(entries
            .into_iter()
            .map(|e| (TenantTimelineId::new(e.tenant_id, e.timeline_id), e.branch_name))
            .collect())
    }

    ///  Construct `Self` from on-disk state.
    pub fn load_config(repopath: &Path) -> anyhow::Result<Self> {
        if !repopath.exists() {
            bail!(
                "Neon config is not found in {}. You need to run 'yb_light_local init' first",
                repopath.to_str().unwrap()
            );
        }

        // TODO: check that it looks like a neon repository

        // load and parse file
        let config_file_contents = fs::read_to_string(repopath.join("config"))?;
        let on_disk_config: OnDiskConfig = toml::from_str(config_file_contents.as_str())?;
        let mut env = {
            let OnDiskConfig {
                pg_distrib_dir,
                yb_light_distrib_dir,
                default_tenant_id,
                private_key_path,
                public_key_path,
                storage_controller,
                pageservers,
                safekeepers,
                pageserver_templates,
                safekeeper_templates,
                control_plane_api,
                control_plane_hooks_api,
                control_plane_compute_hook_api: _,
                use_local_tls,
                endpoint_storage,
                endpoint_storages,
            } = on_disk_config;
            LocalEnv {
                base_data_dir: repopath.to_owned(),
                pg_distrib_dir,
                yb_light_distrib_dir,
                default_tenant_id,
                private_key_path,
                public_key_path,
                storage_controller,
                pageservers,
                safekeepers,
                pageserver_templates,
                safekeeper_templates,
                control_plane_api: control_plane_api.unwrap(),
                control_plane_hooks_api,
                use_local_tls,
                endpoint_storage,
                endpoint_storages,
            }
        };

        // The source of truth for a pageserver's runtime config is the
        // `pageserver.toml` under its endpoint's `pageserver/` subdir.
        assert!(
            env.pageservers.is_empty(),
            "we ensure this during deserialization"
        );
        env.pageservers = Self::discover_pageservers(&env.endpoints_path())?;

        Ok(env)
    }

    /// Scan `endpoints/*/pageserver/` for provisioned pageservers,
    /// reconstructing each [`PageServerConf`] from its `pageserver.toml`
    /// (listen addrs, auth, identity) and `identity.toml` (node id). The
    /// `data_dir` is the discovered `endpoints/<name>/pageserver` path.
    /// Endpoint dirs without a `pageserver/` subdir are skipped.
    pub fn discover_pageservers(endpoints_path: &Path) -> anyhow::Result<Vec<PageServerConf>> {
        let mut pageservers = Vec::new();
        if !endpoints_path.exists() {
            return Ok(pageservers);
        }
        for res in std::fs::read_dir(endpoints_path).context("open endpoints dir")? {
            let endpoint_dentry = res?;
            if !endpoint_dentry
                .file_type()
                .context("determine file type")?
                .is_dir()
            {
                continue;
            }
            let data_dir = endpoint_dentry.path().join("pageserver");
            let config_toml_path = data_dir.join("pageserver.toml");
            if !config_toml_path.exists() {
                // Endpoint that doesn't own a pageserver (e.g. a second
                // endpoint on a branch already served elsewhere).
                continue;
            }
            // TODO(christian): use pageserver_api::config::ConfigToml (PR #7656)
            #[derive(serde::Serialize, serde::Deserialize)]
            // (allow unknown fields, unlike PageServerConf)
            struct PageserverConfigTomlSubset {
                listen_pg_addr: String,
                listen_http_addr: String,
                listen_https_addr: Option<String>,
                listen_grpc_addr: Option<String>,
                pg_auth_type: AuthType,
                http_auth_type: AuthType,
                grpc_auth_type: AuthType,
                #[serde(default)]
                no_sync: bool,
                #[serde(default)]
                tenant_id: Option<TenantId>,
                #[serde(default)]
                timeline_id: Option<TimelineId>,
            }
            let config_toml: PageserverConfigTomlSubset = toml_edit::de::from_str(
                &std::fs::read_to_string(&config_toml_path)
                    .with_context(|| format!("read {config_toml_path:?}"))?,
            )
            .context("parse pageserver.toml")?;
            let identity_toml_path = data_dir.join("identity.toml");
            #[derive(serde::Serialize, serde::Deserialize)]
            struct IdentityTomlSubset {
                id: NodeId,
            }
            let identity_toml: IdentityTomlSubset = toml_edit::de::from_str(
                &std::fs::read_to_string(&identity_toml_path)
                    .with_context(|| format!("read {identity_toml_path:?}"))?,
            )
            .context("parse identity.toml")?;
            let PageserverConfigTomlSubset {
                listen_pg_addr,
                listen_http_addr,
                listen_https_addr,
                listen_grpc_addr,
                pg_auth_type,
                http_auth_type,
                grpc_auth_type,
                no_sync,
                tenant_id,
                timeline_id,
            } = config_toml;
            pageservers.push(PageServerConf {
                id: identity_toml.id,
                listen_pg_addr,
                listen_http_addr,
                listen_https_addr,
                listen_grpc_addr,
                pg_auth_type,
                http_auth_type,
                grpc_auth_type,
                no_sync,
                tenant_id,
                timeline_id,
                data_dir,
            });
        }
        Ok(pageservers)
    }

    pub fn persist_config(&self) -> anyhow::Result<()> {
        Self::persist_config_impl(
            &self.base_data_dir,
            &OnDiskConfig {
                pg_distrib_dir: self.pg_distrib_dir.clone(),
                yb_light_distrib_dir: self.yb_light_distrib_dir.clone(),
                default_tenant_id: self.default_tenant_id,
                private_key_path: self.private_key_path.clone(),
                public_key_path: self.public_key_path.clone(),
                storage_controller: self.storage_controller.clone(),
                pageservers: vec![], // it's skip_serializing anyway
                safekeepers: self.safekeepers.clone(),
                pageserver_templates: self.pageserver_templates.clone(),
                safekeeper_templates: self.safekeeper_templates.clone(),
                control_plane_api: Some(self.control_plane_api.clone()),
                control_plane_hooks_api: self.control_plane_hooks_api.clone(),
                control_plane_compute_hook_api: None,
                use_local_tls: self.use_local_tls,
                endpoint_storage: self.endpoint_storage.clone(),
                endpoint_storages: self.endpoint_storages.clone(),
            },
        )
    }

    pub fn persist_config_impl(base_path: &Path, config: &OnDiskConfig) -> anyhow::Result<()> {
        let conf_content = &toml::to_string_pretty(config)?;
        let target_config_path = base_path.join("config");
        fs::write(&target_config_path, conf_content).with_context(|| {
            format!(
                "Failed to write config file into path '{}'",
                target_config_path.display()
            )
        })
    }

    // this function is used only for testing purposes in CLI e g generate tokens during init
    pub fn generate_auth_token<S: Serialize>(&self, claims: &S) -> anyhow::Result<String> {
        let key = self.read_private_key()?;
        encode_from_key_file(claims, &key)
    }

    /// Get the path to the private key.
    pub fn get_private_key_path(&self) -> PathBuf {
        if self.private_key_path.is_absolute() {
            self.private_key_path.to_path_buf()
        } else {
            self.base_data_dir.join(&self.private_key_path)
        }
    }

    /// Get the path to the public key.
    pub fn get_public_key_path(&self) -> PathBuf {
        if self.public_key_path.is_absolute() {
            self.public_key_path.to_path_buf()
        } else {
            self.base_data_dir.join(&self.public_key_path)
        }
    }

    /// Read the contents of the private key file.
    pub fn read_private_key(&self) -> anyhow::Result<Pem> {
        let private_key_path = self.get_private_key_path();
        let pem = pem::parse(fs::read(private_key_path)?)?;
        Ok(pem)
    }

    /// Read the contents of the public key file.
    pub fn read_public_key(&self) -> anyhow::Result<Pem> {
        let public_key_path = self.get_public_key_path();
        let pem = pem::parse(fs::read(public_key_path)?)?;
        Ok(pem)
    }

    /// Materialize the [`YbLightLocalInitConf`] to disk. Called during [`yb_light_local init`].
    pub fn init(conf: YbLightLocalInitConf, force: &InitForceMode) -> anyhow::Result<()> {
        let base_path = base_path();
        assert_ne!(base_path, Path::new(""));
        let base_path = &base_path;

        // create base_path dir
        if base_path.exists() {
            match force {
                InitForceMode::MustNotExist => {
                    bail!(
                        "directory '{}' already exists. Perhaps already initialized?",
                        base_path.display()
                    );
                }
                InitForceMode::EmptyDirOk => {
                    if let Some(res) = std::fs::read_dir(base_path)?.next() {
                        res.context("check if directory is empty")?;
                        anyhow::bail!("directory not empty: {base_path:?}");
                    }
                }
                InitForceMode::RemoveAllContents => {
                    println!("removing all contents of '{}'", base_path.display());
                    // instead of directly calling `remove_dir_all`, we keep the original dir but removing
                    // all contents inside. This helps if the developer symbol links another directory (i.e.,
                    // S3 local SSD) to the `.yb_light` base directory.
                    for entry in std::fs::read_dir(base_path)? {
                        let entry = entry?;
                        let path = entry.path();
                        if path.is_dir() {
                            fs::remove_dir_all(&path)?;
                        } else {
                            fs::remove_file(&path)?;
                        }
                    }
                }
            }
        }
        if !base_path.exists() {
            fs::create_dir(base_path)?;
        }

        let YbLightLocalInitConf {
            pg_distrib_dir,
            yb_light_distrib_dir,
            default_tenant_id,
            storage_controller,
            pageservers,
            safekeepers,
            control_plane_api,
            use_local_tls,
            control_plane_hooks_api,
            endpoint_storage,
        } = conf;

        // Find postgres binaries.
        // Follow POSTGRES_DISTRIB_DIR if set, otherwise look in "pg_install".
        // Note that later in the code we assume, that distrib dirs follow the same pattern
        // for all postgres versions.
        let pg_distrib_dir = pg_distrib_dir.unwrap_or_else(|| {
            if let Some(postgres_bin) = env::var_os("POSTGRES_DISTRIB_DIR") {
                postgres_bin.into()
            } else {
                let cwd = env::current_dir().unwrap();
                cwd.join("pg_install")
            }
        });

        // Find neon binaries.
        let yb_light_distrib_dir = yb_light_distrib_dir
            .unwrap_or_else(|| env::current_exe().unwrap().parent().unwrap().to_owned());

        // Generate keypair for JWT.
        //
        // The keypair is only needed if authentication is enabled in any of the
        // components. For convenience, we generate the keypair even if authentication
        // is not enabled, so that you can easily enable it after the initialization
        // step.
        generate_auth_keys(
            base_path.join("auth_private_key.pem").as_path(),
            base_path.join("auth_public_key.pem").as_path(),
        )
        .context("generate auth keys")?;
        let private_key_path = PathBuf::from("auth_private_key.pem");
        let public_key_path = PathBuf::from("auth_public_key.pem");

        // No pageserver/safekeeper dirs are created at init: the
        // PS+SK pair for a timeline is provisioned under its owning
        // `endpoints/<name>/` folder at `endpoint create`. The init
        // confs are kept as templates that the launcher clones (ports,
        // `remote_storage`/`wal_yb_config`, auth) when it allocates a
        // pageserver/safekeeper. `pageservers`/`safekeepers` start empty
        // and are populated as endpoints are created.
        let env = LocalEnv {
            base_data_dir: base_path.clone(),
            pg_distrib_dir,
            yb_light_distrib_dir,
            default_tenant_id: Some(default_tenant_id),
            private_key_path,
            public_key_path,
            storage_controller: storage_controller.unwrap_or_default(),
            pageservers: Vec::new(),
            safekeepers: Vec::new(),
            pageserver_templates: pageservers,
            safekeeper_templates: safekeepers,
            control_plane_api: control_plane_api.unwrap(),
            control_plane_hooks_api,
            use_local_tls,
            endpoint_storage,
            endpoint_storages: Vec::new(),
        };

        if use_local_tls {
            env.generate_ssl_ca_cert()?;
        }

        // create endpoints dir
        fs::create_dir_all(env.endpoints_path())?;

        // No endpoint_storage instance is created at init: like the
        // PS+SK pair, one is provisioned under each `endpoints/<name>/`
        // folder at `endpoint create`. `endpoint_storage` is kept as the
        // template (base port, `remote_storage_json`) the launcher clones.

        env.persist_config()
    }
}

pub fn base_path() -> PathBuf {
    let path = match std::env::var_os("YB_LIGHT_REPO_DIR") {
        Some(val) => {
            let path = PathBuf::from(val);
            if !path.is_absolute() {
                // repeat the env var in the error because our default is always absolute
                panic!("YB_LIGHT_REPO_DIR must be an absolute path, got {path:?}");
            }
            path
        }
        None => {
            let pwd = std::env::current_dir()
                // technically this can fail but it's quite unlikeley
                .expect("determine current directory");
            let pwd_abs = pwd.canonicalize().expect("canonicalize current directory");
            pwd_abs.join(".yb_light")
        }
    };
    assert!(path.is_absolute());
    path
}

/// Generate a public/private key pair for JWT authentication
fn generate_auth_keys(private_key_path: &Path, public_key_path: &Path) -> anyhow::Result<()> {
    // Generate the key pair
    //
    // openssl genpkey -algorithm ed25519 -out auth_private_key.pem
    let keygen_output = Command::new("openssl")
        .arg("genpkey")
        .args(["-algorithm", "ed25519"])
        .args(["-out", private_key_path.to_str().unwrap()])
        .stdout(Stdio::null())
        .output()
        .context("failed to generate auth private key")?;
    if !keygen_output.status.success() {
        bail!(
            "openssl failed: '{}'",
            String::from_utf8_lossy(&keygen_output.stderr)
        );
    }

    // Extract the public key from the private key file
    //
    // openssl pkey -in auth_private_key.pem -pubout -out auth_public_key.pem
    let keygen_output = Command::new("openssl")
        .arg("pkey")
        .args(["-in", private_key_path.to_str().unwrap()])
        .arg("-pubout")
        .args(["-out", public_key_path.to_str().unwrap()])
        .output()
        .context("failed to extract public key from private key")?;
    if !keygen_output.status.success() {
        bail!(
            "openssl failed: '{}'",
            String::from_utf8_lossy(&keygen_output.stderr)
        );
    }

    Ok(())
}

fn generate_ssl_ca_cert(cert_path: &Path, key_path: &Path) -> anyhow::Result<()> {
    // openssl req -x509 -newkey ed25519 -nodes -subj "/CN=Neon Local CA" -days 36500 \
    // -out rootCA.crt -keyout rootCA.key
    let keygen_output = Command::new("openssl")
        .args([
            "req", "-x509", "-newkey", "ed25519", "-nodes", "-days", "36500",
        ])
        .args(["-subj", "/CN=Neon Local CA"])
        .args(["-out", cert_path.to_str().unwrap()])
        .args(["-keyout", key_path.to_str().unwrap()])
        .output()
        .context("failed to generate CA certificate")?;
    if !keygen_output.status.success() {
        bail!(
            "openssl failed: '{}'",
            String::from_utf8_lossy(&keygen_output.stderr)
        );
    }
    Ok(())
}

fn generate_ssl_cert(
    cert_path: &Path,
    key_path: &Path,
    ca_cert_path: &Path,
    ca_key_path: &Path,
) -> anyhow::Result<()> {
    // Generate Certificate Signing Request (CSR).
    let mut csr_path = cert_path.to_path_buf();
    csr_path.set_extension(".csr");

    // openssl req -new -nodes -newkey ed25519 -keyout server.key -out server.csr \
    // -subj "/CN=localhost" -addext "subjectAltName=DNS:localhost,IP:127.0.0.1"
    let keygen_output = Command::new("openssl")
        .args(["req", "-new", "-nodes"])
        .args(["-newkey", "ed25519"])
        .args(["-subj", "/CN=localhost"])
        .args(["-addext", "subjectAltName=DNS:localhost,IP:127.0.0.1"])
        .args(["-keyout", key_path.to_str().unwrap()])
        .args(["-out", csr_path.to_str().unwrap()])
        .output()
        .context("failed to generate CSR")?;
    if !keygen_output.status.success() {
        bail!(
            "openssl failed: '{}'",
            String::from_utf8_lossy(&keygen_output.stderr)
        );
    }

    // Sign CSR with CA key.
    //
    // openssl x509 -req -in server.csr -CA rootCA.crt -CAkey rootCA.key -CAcreateserial \
    // -out server.crt -days 36500 -copy_extensions copyall
    let keygen_output = Command::new("openssl")
        .args(["x509", "-req"])
        .args(["-in", csr_path.to_str().unwrap()])
        .args(["-CA", ca_cert_path.to_str().unwrap()])
        .args(["-CAkey", ca_key_path.to_str().unwrap()])
        .arg("-CAcreateserial")
        .args(["-out", cert_path.to_str().unwrap()])
        .args(["-days", "36500"])
        .args(["-copy_extensions", "copyall"])
        .output()
        .context("failed to sign CSR")?;
    if !keygen_output.status.success() {
        bail!(
            "openssl failed: '{}'",
            String::from_utf8_lossy(&keygen_output.stderr)
        );
    }

    // Remove CSR file as it's not needed anymore.
    fs::remove_file(csr_path)?;

    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use camino_tempfile::Utf8TempDir;

    fn minimal_env(base: PathBuf) -> LocalEnv {
        LocalEnv {
            base_data_dir: base,
            pg_distrib_dir: PathBuf::from("/pg"),
            yb_light_distrib_dir: PathBuf::from("/neon"),
            default_tenant_id: None,
            private_key_path: PathBuf::from("auth_private_key.pem"),
            public_key_path: PathBuf::from("auth_public_key.pem"),
            storage_controller: YbLightStorageControllerConf::default(),
            pageservers: Vec::new(),
            safekeepers: Vec::new(),
            pageserver_templates: Vec::new(),
            safekeeper_templates: Vec::new(),
            endpoint_storage: EndpointStorageConf::default(),
            endpoint_storages: Vec::new(),
            control_plane_api: "http://127.0.0.1:1234/upcall/v1/".parse().unwrap(),
            control_plane_hooks_api: None,
            use_local_tls: false,
        }
    }

    /// `pageserver.toml` / `identity.toml` for a provisioned pageserver
    /// under `endpoints/<name>/pageserver/`.
    fn write_pageserver_dir(endpoints: &Path, name: &str, id: u64, pg_port: u16) {
        let dir = endpoints.join(name).join("pageserver");
        fs::create_dir_all(&dir).unwrap();
        fs::write(
            dir.join("pageserver.toml"),
            format!(
                "listen_pg_addr = '127.0.0.1:{pg_port}'\n\
                 listen_http_addr = '127.0.0.1:{}'\n\
                 pg_auth_type = 'Trust'\n\
                 http_auth_type = 'Trust'\n\
                 grpc_auth_type = 'Trust'\n",
                pg_port + 1000
            ),
        )
        .unwrap();
        fs::write(dir.join("identity.toml"), format!("id = {id}\n")).unwrap();
    }

    #[test]
    fn data_dir_paths_are_endpoint_keyed() {
        let base = PathBuf::from("/tmp/repo");
        let env = minimal_env(base.clone());
        assert_eq!(
            env.pageserver_data_dir("main"),
            base.join("endpoints").join("main").join("pageserver")
        );
        assert_eq!(
            env.safekeeper_data_dir("ep_b1"),
            base.join("endpoints").join("ep_b1").join("safekeeper")
        );
        assert_eq!(
            env.endpoint_storage_data_dir("main"),
            base.join("endpoints").join("main").join("endpoint_storage")
        );
    }

    #[test]
    fn discover_pageservers_reconstructs_from_endpoint_dirs() {
        let tmp = Utf8TempDir::new().unwrap();
        let endpoints = tmp.path().as_std_path().join("endpoints");
        write_pageserver_dir(&endpoints, "main", 1, 64000);
        write_pageserver_dir(&endpoints, "ep_b1", 2, 64002);
        // An endpoint without a pageserver/ subdir is skipped.
        fs::create_dir_all(endpoints.join("ro").join("pgdata")).unwrap();

        let mut found = LocalEnv::discover_pageservers(&endpoints).unwrap();
        found.sort_by_key(|p| p.id.0);
        assert_eq!(found.len(), 2);

        assert_eq!(found[0].id, NodeId(1));
        assert_eq!(found[0].listen_pg_addr, "127.0.0.1:64000");
        assert_eq!(found[0].data_dir, endpoints.join("main").join("pageserver"));

        assert_eq!(found[1].id, NodeId(2));
        assert_eq!(found[1].data_dir, endpoints.join("ep_b1").join("pageserver"));
    }

    #[test]
    fn discover_pageservers_errors_on_malformed_toml() {
        let tmp = Utf8TempDir::new().unwrap();
        let endpoints = tmp.path().as_std_path().join("endpoints");
        let dir = endpoints.join("main").join("pageserver");
        fs::create_dir_all(&dir).unwrap();
        fs::write(dir.join("pageserver.toml"), "this is not = valid = toml").unwrap();
        fs::write(dir.join("identity.toml"), "id = 1\n").unwrap();
        assert!(LocalEnv::discover_pageservers(&endpoints).is_err());
    }

    #[test]
    fn discover_pageservers_empty_when_no_endpoints_dir() {
        let tmp = Utf8TempDir::new().unwrap();
        let missing = tmp.path().as_std_path().join("endpoints");
        assert!(LocalEnv::discover_pageservers(&missing).unwrap().is_empty());
    }
}
