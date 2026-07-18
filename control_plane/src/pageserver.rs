//! Code to manage pageservers
//!
//! In the local test environment, each pageserver is co-located under the
//! endpoint that owns it:
//!
//! ```text
//!   .yb_light/endpoints/<endpoint_id>/pageserver
//! ```
//!
use std::collections::HashMap;
use std::io;
use std::io::Write;
use std::num::NonZeroU64;
use std::path::PathBuf;
use std::str::FromStr;
use std::time::Duration;

use anyhow::{Context, bail};
use camino::Utf8PathBuf;
use pageserver_api::config::{
    DEFAULT_GRPC_LISTEN_PORT, DEFAULT_HTTP_LISTEN_PORT, TimelineInitConfig,
};
use pageserver_api::controller_api::NodeRegisterRequest;
use pageserver_api::models::{self, TimelineInfo};
use pageserver_client::mgmt_api;
use postgres_backend::AuthType;
use postgres_connection::{PgConnectionConfig, parse_host_port};
use utils::auth::{Claims, Scope};
use utils::id::{BranchId, NodeId};
use utils::lsn::Lsn;

use crate::background_process;
use crate::local_env::{LocalEnv, PageServerConf, YbLightLocalInitPageserverConf};

//
// Control routines for pageserver.
//
// Used in CLI and tests.
//
#[derive(Debug)]
pub struct PageServerNode {
    pub pg_connection_config: PgConnectionConfig,
    pub conf: PageServerConf,
    pub env: LocalEnv,
    pub http_client: mgmt_api::Client,
}

impl PageServerNode {
    pub fn from_env(env: &LocalEnv, conf: &PageServerConf) -> PageServerNode {
        let (host, port) =
            parse_host_port(&conf.listen_pg_addr).expect("Unable to parse listen_pg_addr");
        let port = port.unwrap_or(5432);

        let endpoint = if env.controller.use_https_pageserver_api {
            format!(
                "https://{}",
                conf.listen_https_addr.as_ref().expect(
                    "listen https address should be specified if use_https_pageserver_api is on"
                )
            )
        } else {
            format!("http://{}", conf.listen_http_addr)
        };

        Self {
            pg_connection_config: PgConnectionConfig::new_host_port(host, port),
            conf: conf.clone(),
            env: env.clone(),
            http_client: mgmt_api::Client::new(
                env.create_http_client(),
                endpoint,
                {
                    match conf.http_auth_type {
                        AuthType::Trust => None,
                        AuthType::YbLightJWT => Some(
                            env.generate_auth_token(&Claims::new(None, Scope::PageServerApi))
                                .unwrap(),
                        ),
                    }
                }
                .as_deref(),
            ),
        }
    }

    fn pageserver_make_identity_toml(&self, node_id: NodeId) -> toml_edit::DocumentMut {
        toml_edit::DocumentMut::from_str(&format!("id={node_id}")).unwrap()
    }

    fn pageserver_init_make_toml(
        &self,
        conf: YbLightLocalInitPageserverConf,
        safekeeper_pg_addr: &str,
    ) -> anyhow::Result<toml_edit::DocumentMut> {
        // `data_dir` is a runtime-only field (where the toml lives), not
        // derived from the init conf, so exclude it from the equality check.
        let mut expected = PageServerConf::from(&conf);
        expected.data_dir = self.conf.data_dir.clone();
        assert_eq!(
            &expected, &self.conf,
            "during yb_light_local init, we derive the runtime state of ps conf (self.conf) from the --config flag fully"
        );

        // TODO(christian): instead of what we do here, create a pageserver_api::config::ConfigToml (PR #7656)

        // FIXME: the paths should be shell-escaped to handle paths with spaces, quotas etc.
        let pg_distrib_dir_param = format!(
            "pg_distrib_dir='{}'",
            self.env.pg_distrib_dir_raw().display()
        );

        // The pageserver streams WAL from the safekeeper co-located with
        // its timeline (caller-supplied), not `env.safekeepers.first()` —
        // in the co-located topology the global-first safekeeper belongs to
        // a different timeline's pair once more than one pair exists.
        let safekeeper_pg_addr_param = format!("safekeeper_pg_addr='{safekeeper_pg_addr}'");

        let mut overrides = vec![pg_distrib_dir_param, safekeeper_pg_addr_param];

        overrides.push(format!(
            "control_plane_api='{}'",
            self.env.control_plane_api.as_str()
        ));

        // Controller uses the same auth as pageserver: if JWT is enabled
        // for us, we will also need it to talk to them.
        if matches!(conf.http_auth_type, AuthType::YbLightJWT) {
            let jwt_token = self
                .env
                .generate_auth_token(&Claims::new(None, Scope::GenerationsApi))
                .unwrap();
            overrides.push(format!("control_plane_api_token='{jwt_token}'"));
        }

        // `conf.yb_storage_conn` is the supported way to set this — written
        // verbatim to avoid the `#[serde(flatten)] other` mangling that
        // turns inline tables into nested `other = { yb_storage_conn = "..." }`
        // blocks pageserver ignores. Legacy callers that stuff it into
        // `other` still work. LocalFs was dropped, so a YbStore connection is
        // mandatory — bail out hard if it isn't set.
        if let Some(rs) = &conf.yb_storage_conn {
            overrides.push(format!("yb_storage_conn={rs}"));
        } else if !conf.other.contains_key("yb_storage_conn") {
            anyhow::bail!(
                "pageserver yb_storage_conn must be configured (LocalFs was removed; \
                 set YbLightLocalInitPageserverConf::yb_storage_conn to a YbStore inline table)"
            );
        }

        if [conf.http_auth_type, conf.pg_auth_type, conf.grpc_auth_type]
            .contains(&AuthType::YbLightJWT)
        {
            // Keys are generated in the toplevel repo dir, pageservers' workdirs
            // are one level below that, so refer to keys with ../
            overrides.push("auth_validation_public_key_path='../auth_public_key.pem'".to_owned());
        }

        if let Some(ssl_ca_file) = self.env.ssl_ca_cert_path() {
            overrides.push(format!("ssl_ca_file='{}'", ssl_ca_file.to_str().unwrap()));
        }

        // Apply the user-provided overrides
        overrides.push({
            let mut doc =
                toml_edit::ser::to_document(&conf).expect("we deserialized this from toml earlier");
            // `id` is written out to `identity.toml` instead of `pageserver.toml`
            doc.remove("id").expect("it's part of the struct");
            // Already emitted verbatim above; serializing the Option<String>
            // here would write `yb_storage_conn = "{ host_ports = ... }"` (a
            // string, not a table), which pageserver rejects.
            doc.remove("yb_storage_conn");
            doc.to_string()
        });

        // Turn `overrides` into a toml document.
        // TODO: above code is legacy code, it should be refactored to use toml_edit directly.
        let mut config_toml = toml_edit::DocumentMut::new();
        for fragment_str in overrides {
            let fragment = toml_edit::DocumentMut::from_str(&fragment_str)
                .expect("all fragments in `overrides` are valid toml documents, this function controls that");
            for (key, item) in fragment.iter() {
                config_toml.insert(key, item.clone());
            }
        }
        Ok(config_toml)
    }

    /// Initializes a pageserver node by creating its config with the overrides provided.
    /// `safekeeper_pg_addr` is the `host:port` of the safekeeper co-located with this
    /// pageserver's timeline, baked into `pageserver.toml` as the WAL source.
    pub fn initialize(
        &self,
        conf: YbLightLocalInitPageserverConf,
        safekeeper_pg_addr: &str,
    ) -> anyhow::Result<()> {
        self.pageserver_init(conf, safekeeper_pg_addr)
            .with_context(|| format!("Failed to run init for pageserver node {}", self.conf.id))
    }

    pub fn repo_path(&self) -> PathBuf {
        self.conf.data_dir.clone()
    }

    /// The pid file is created by the pageserver process, with its pid stored inside.
    /// Other pageservers cannot lock the same file and overwrite it for as long as the current
    /// pageserver runs. (Unless someone removes the file manually; never do that!)
    fn pid_file(&self) -> Utf8PathBuf {
        Utf8PathBuf::from_path_buf(self.repo_path().join("pageserver.pid"))
            .expect("non-Unicode path")
    }

    pub async fn start(&self, retry_timeout: &Duration) -> anyhow::Result<()> {
        // Single-timeline pageserver: every process start is an
        // attach, so bump the generation fence first. A stale process
        // still holding the old generation gets fenced out by the
        // CAS on the pageserver.timeline_state row.
        self.register_and_attach().await?;
        self.start_node(retry_timeout).await
    }

    /// Read the single-branch identity baked into this pageserver's
    /// `pageserver.toml`, if any. `None` means the launcher hasn't
    /// assigned a branch yet (fresh repo, pre-`branch create`).
    pub fn read_timeline_identity(&self) -> anyhow::Result<Option<BranchId>> {
        let config_file_path = self.repo_path().join("pageserver.toml");
        let contents = std::fs::read_to_string(&config_file_path)
            .with_context(|| format!("read {config_file_path:?}"))?;
        let doc = contents
            .parse::<toml_edit::DocumentMut>()
            .with_context(|| format!("parse {config_file_path:?}"))?;
        let get_str = |key: &str| doc.get(key).and_then(|item| item.as_str());
        match get_str("branch_id") {
            Some(branch) => Ok(Some(
                branch
                    .parse()
                    .context("parse branch_id in pageserver.toml")?,
            )),
            None => Ok(None),
        }
    }

    /// Bake the single-branch identity into `pageserver.toml`
    /// (read-modify-write; overwrites any previous identity).
    /// `generation` is just a placeholder here — [`Self::start`] bumps
    /// it via the controller on every spawn.
    pub fn set_timeline_identity(
        &self,
        branch_id: BranchId,
        generation: u32,
        timeline_init: Option<&TimelineInitConfig>,
    ) -> anyhow::Result<()> {
        let mut fragment = toml_edit::DocumentMut::new();
        fragment.insert("branch_id", toml_edit::value(branch_id.to_string()));
        fragment.insert("generation", toml_edit::value(generation as i64));
        if let Some(init) = timeline_init {
            let init_doc =
                toml_edit::ser::to_document(init).context("serialize [timeline_init]")?;
            fragment.insert(
                "timeline_init",
                toml_edit::Item::Table(init_doc.as_table().clone()),
            );
        }
        self.merge_config_fragment(fragment)
    }

    /// Provision a NEW pageserver datadir by cloning `template`'s
    /// `pageserver.toml` and patching the per-node bits (listen
    /// addresses, safekeeper address). The timeline identity is NOT
    /// set here — the caller bakes it via
    /// [`Self::set_timeline_identity`] before starting the process.
    pub fn initialize_from_template(
        &self,
        template: &PageServerNode,
        safekeeper_pg_addr: &str,
    ) -> anyhow::Result<()> {
        let datadir = self.repo_path();
        std::fs::create_dir_all(&datadir)
            .with_context(|| format!("create pageserver datadir {datadir:?}"))?;
        let template_toml_path = template.repo_path().join("pageserver.toml");
        let template_toml = std::fs::read_to_string(&template_toml_path)
            .with_context(|| format!("read template {template_toml_path:?}"))?;
        let mut doc = template_toml
            .parse::<toml_edit::DocumentMut>()
            .with_context(|| format!("parse template {template_toml_path:?}"))?;

        doc.insert(
            "listen_pg_addr",
            toml_edit::value(self.conf.listen_pg_addr.clone()),
        );
        doc.insert(
            "listen_http_addr",
            toml_edit::value(self.conf.listen_http_addr.clone()),
        );
        match &self.conf.listen_grpc_addr {
            Some(addr) => {
                doc.insert("listen_grpc_addr", toml_edit::value(addr.clone()));
            }
            None => {
                doc.remove("listen_grpc_addr");
            }
        }
        match &self.conf.listen_https_addr {
            Some(addr) => {
                doc.insert("listen_https_addr", toml_edit::value(addr.clone()));
            }
            None => {
                doc.remove("listen_https_addr");
            }
        }
        doc.insert(
            "safekeeper_pg_addr",
            toml_edit::value(safekeeper_pg_addr.to_string()),
        );
        // Strip any identity inherited from the template: this node
        // serves its own branch, assigned by the caller.
        for key in ["branch_id", "generation", "timeline_init"] {
            doc.remove(key);
        }

        let config_file_path = datadir.join("pageserver.toml");
        let mut config_file = std::fs::OpenOptions::new()
            .create_new(true)
            .write(true)
            .open(&config_file_path)
            .with_context(|| format!("open pageserver toml for write: {config_file_path:?}"))?;
        config_file
            .write_all(doc.to_string().as_bytes())
            .context("write pageserver toml")?;
        drop(config_file);

        let identity_file_path = datadir.join("identity.toml");
        let identity_toml = self.pageserver_make_identity_toml(self.conf.id);
        std::fs::write(&identity_file_path, identity_toml.to_string())
            .with_context(|| format!("write identity toml: {identity_file_path:?}"))?;

        if self.env.use_local_tls {
            self.env.generate_ssl_cert(
                datadir.join("server.crt").as_path(),
                datadir.join("server.key").as_path(),
            )?;
        }

        Ok(())
    }

    /// Merge top-level keys of `fragment` into `pageserver.toml`,
    /// overwriting existing values.
    fn merge_config_fragment(&self, fragment: toml_edit::DocumentMut) -> anyhow::Result<()> {
        let config_file_path = self.repo_path().join("pageserver.toml");
        let contents = std::fs::read_to_string(&config_file_path)
            .with_context(|| format!("read {config_file_path:?}"))?;
        let mut doc = contents
            .parse::<toml_edit::DocumentMut>()
            .with_context(|| format!("parse {config_file_path:?}"))?;
        for (key, item) in fragment.iter() {
            doc.insert(key, item.clone());
        }
        std::fs::write(&config_file_path, doc.to_string())
            .with_context(|| format!("write {config_file_path:?}"))?;
        Ok(())
    }

    fn node_register_request(&self) -> NodeRegisterRequest {
        let (_, http_port) =
            parse_host_port(&self.conf.listen_http_addr).expect("Unable to parse listen_http_addr");
        let http_port = http_port.unwrap_or(DEFAULT_HTTP_LISTEN_PORT);
        let https_port = self.conf.listen_https_addr.as_ref().map(|https_addr| {
            let (_, https_port) =
                parse_host_port(https_addr).expect("Unable to parse listen_https_addr");
            https_port.unwrap_or(9899)
        });
        let (mut grpc_host, mut grpc_port) = (None, None);
        if let Some(grpc_addr) = &self.conf.listen_grpc_addr {
            let (_, port) = parse_host_port(grpc_addr).expect("Unable to parse listen_grpc_addr");
            grpc_host = Some("localhost".to_string());
            grpc_port = Some(port.unwrap_or(DEFAULT_GRPC_LISTEN_PORT));
        }
        NodeRegisterRequest {
            node_id: self.conf.id,
            listen_pg_addr: "localhost".to_string(),
            listen_pg_port: self.pg_connection_config.port(),
            listen_grpc_addr: grpc_host,
            listen_grpc_port: grpc_port,
            listen_http_addr: "localhost".to_string(),
            listen_http_port: http_port,
            listen_https_port: https_port,
            node_ip_addr: None,
        }
    }

    /// If an identity is baked into `pageserver.toml`: register this
    /// node with the controller (idempotent), bump the
    /// branch's generation against this node via the attach-hook API,
    /// and persist the fresh generation back into the toml so the
    /// next process spawn picks it up. No-op without an identity.
    ///
    /// Returns the granted generation, or `None` if no identity is
    /// configured.
    pub async fn register_and_attach(&self) -> anyhow::Result<Option<u32>> {
        let Some(branch_id) = self.read_timeline_identity()? else {
            return Ok(None);
        };
        let controller = crate::controller::Controller::from_env(&self.env);
        controller
            .node_register(self.node_register_request())
            .await
            .context("register pageserver node with controller")?;
        let generation = controller
            .attach_hook(branch_id, self.conf.id)
            .await?
            .context("attach_hook did not return a generation")?;
        let mut fragment = toml_edit::DocumentMut::new();
        fragment.insert("generation", toml_edit::value(generation as i64));
        self.merge_config_fragment(fragment)?;
        println!(
            "pageserver {} serves {branch_id} at generation {generation}",
            self.conf.id
        );
        Ok(Some(generation))
    }

    fn pageserver_init(
        &self,
        conf: YbLightLocalInitPageserverConf,
        safekeeper_pg_addr: &str,
    ) -> anyhow::Result<()> {
        let datadir = self.repo_path();
        let node_id = self.conf.id;
        println!(
            "Initializing pageserver node {} at '{}' in {:?}",
            node_id,
            self.pg_connection_config.raw_address(),
            datadir
        );
        io::stdout().flush()?;

        let config = self
            .pageserver_init_make_toml(conf, safekeeper_pg_addr)
            .context("make pageserver toml")?;
        let config_file_path = datadir.join("pageserver.toml");
        let mut config_file = std::fs::OpenOptions::new()
            .create_new(true)
            .write(true)
            .open(&config_file_path)
            .with_context(|| format!("open pageserver toml for write: {config_file_path:?}"))?;
        config_file
            .write_all(config.to_string().as_bytes())
            .context("write pageserver toml")?;
        drop(config_file);

        let identity_file_path = datadir.join("identity.toml");
        let mut identity_file = std::fs::OpenOptions::new()
            .create_new(true)
            .write(true)
            .open(&identity_file_path)
            .with_context(|| format!("open identity toml for write: {identity_file_path:?}"))?;
        let identity_toml = self.pageserver_make_identity_toml(node_id);
        identity_file
            .write_all(identity_toml.to_string().as_bytes())
            .context("write identity toml")?;
        drop(identity_toml);

        if self.env.use_local_tls {
            self.env.generate_ssl_cert(
                datadir.join("server.crt").as_path(),
                datadir.join("server.key").as_path(),
            )?;
        }

        // TODO: invoke a TBD config-check command to validate that pageserver will start with the written config

        // Write metadata file, used by pageserver on startup to register itself with
        // the controller
        let metadata_path = datadir.join("metadata.json");

        let http_host = "localhost".to_string();
        let (_, http_port) =
            parse_host_port(&self.conf.listen_http_addr).expect("Unable to parse listen_http_addr");
        let http_port = http_port.unwrap_or(DEFAULT_HTTP_LISTEN_PORT);

        let https_port = match self.conf.listen_https_addr.as_ref() {
            Some(https_addr) => {
                let (_https_host, https_port) =
                    parse_host_port(https_addr).expect("Unable to parse listen_https_addr");
                Some(https_port.unwrap_or(9899))
            }
            None => None,
        };

        let (mut grpc_host, mut grpc_port) = (None, None);
        if let Some(grpc_addr) = &self.conf.listen_grpc_addr {
            let (_, port) = parse_host_port(grpc_addr).expect("Unable to parse listen_grpc_addr");
            grpc_host = Some("localhost".to_string());
            grpc_port = Some(port.unwrap_or(DEFAULT_GRPC_LISTEN_PORT));
        }

        // Intentionally hand-craft JSON: this acts as an implicit format compat test
        // in case the pageserver-side structure is edited, and reflects the real life
        // situation: the metadata is written by some other script.
        std::fs::write(
            metadata_path,
            serde_json::to_vec(&pageserver_api::config::NodeMetadata {
                postgres_host: "localhost".to_string(),
                postgres_port: self.pg_connection_config.port(),
                grpc_host,
                grpc_port,
                http_host,
                http_port,
                https_port,
                other: HashMap::new(),
            })
            .unwrap(),
        )
        .expect("Failed to write metadata file");

        Ok(())
    }

    async fn start_node(&self, retry_timeout: &Duration) -> anyhow::Result<()> {
        // TODO: using a thread here because start_process() is not async but we need to call check_status()
        let datadir = self.repo_path();
        println!(
            "Starting pageserver node {} at '{}' in {:?}, retrying for {:?}",
            self.conf.id,
            self.pg_connection_config.raw_address(),
            datadir,
            retry_timeout
        );
        io::stdout().flush().context("flush stdout")?;

        let datadir_path_str = datadir.to_str().with_context(|| {
            format!(
                "Cannot start pageserver node {} in path that has no string representation: {:?}",
                self.conf.id, datadir,
            )
        })?;
        let args = vec!["-D", datadir_path_str];

        background_process::start_process(
            "pageserver",
            &datadir,
            &self.env.pageserver_bin(),
            args,
            self.pageserver_env_variables()?,
            background_process::InitialPidFile::Expect(self.pid_file()),
            retry_timeout,
            || async {
                let st = self.check_status().await;
                match st {
                    Ok(()) => Ok(true),
                    Err(mgmt_api::Error::ReceiveBody(_)) => Ok(false),
                    Err(e) => Err(anyhow::anyhow!("Failed to check node status: {e}")),
                }
            },
        )
        .await?;

        Ok(())
    }

    fn pageserver_env_variables(&self) -> anyhow::Result<Vec<(String, String)>> {
        // FIXME: why is this tied to pageserver's auth type? Whether or not the safekeeper
        // needs a token, and how to generate that token, seems independent to whether
        // the pageserver requires a token in incoming requests.
        Ok(if self.conf.http_auth_type != AuthType::Trust {
            // Generate a token to connect from the pageserver to a safekeeper
            let token = self
                .env
                .generate_auth_token(&Claims::new(None, Scope::SafekeeperData))?;
            vec![("YBAMP_AUTH_TOKEN".to_owned(), token)]
        } else {
            Vec::new()
        })
    }

    ///
    /// Stop the server.
    ///
    /// If 'immediate' is true, we use SIGQUIT, killing the process immediately.
    /// Otherwise we use SIGTERM, triggering a clean shutdown
    ///
    /// If the server is not running, returns success
    ///
    pub fn stop(&self, immediate: bool) -> anyhow::Result<()> {
        background_process::stop_process(immediate, "pageserver", &self.pid_file())
    }

    pub async fn check_status(&self) -> mgmt_api::Result<()> {
        self.http_client.status().await
    }

    pub fn parse_config(mut settings: HashMap<&str, &str>) -> anyhow::Result<models::BranchConfig> {
        let result = models::BranchConfig {
            gc_horizon: settings
                .remove("gc_horizon")
                .map(|x| x.parse::<u64>())
                .transpose()
                .context("Failed to parse 'gc_horizon' as an integer")?,
            gc_period: settings
                .remove("gc_period")
                .map(humantime::parse_duration)
                .transpose()
                .context("Failed to parse 'gc_period' as duration")?,
            pitr_interval: settings
                .remove("pitr_interval")
                .map(humantime::parse_duration)
                .transpose()
                .context("Failed to parse 'pitr_interval' as duration")?,
            walreceiver_connect_timeout: settings
                .remove("walreceiver_connect_timeout")
                .map(humantime::parse_duration)
                .transpose()
                .context("Failed to parse 'walreceiver_connect_timeout' as duration")?,
            lagging_wal_timeout: settings
                .remove("lagging_wal_timeout")
                .map(humantime::parse_duration)
                .transpose()
                .context("Failed to parse 'lagging_wal_timeout' as duration")?,
            max_lsn_wal_lag: settings
                .remove("max_lsn_wal_lag")
                .map(|x| x.parse::<NonZeroU64>())
                .transpose()
                .context("Failed to parse 'max_lsn_wal_lag' as non zero integer")?,
            lazy_slru_download: settings
                .remove("lazy_slru_download")
                .map(|x| x.parse::<bool>())
                .transpose()
                .context("Failed to parse 'lazy_slru_download' as bool")?,
            timeline_get_throttle: settings
                .remove("timeline_get_throttle")
                .map(serde_json::from_str)
                .transpose()
                .context("parse `timeline_get_throttle` from json")?,
            lsn_lease_length: settings
                .remove("lsn_lease_length")
                .map(humantime::parse_duration)
                .transpose()
                .context("Failed to parse 'lsn_lease_length' as duration")?,
            lsn_lease_length_for_ts: settings
                .remove("lsn_lease_length_for_ts")
                .map(humantime::parse_duration)
                .transpose()
                .context("Failed to parse 'lsn_lease_length_for_ts' as duration")?,
            rel_size_v2_enabled: settings
                .remove("rel_size_v2_enabled")
                .map(|x| x.parse::<bool>())
                .transpose()
                .context("Failed to parse 'rel_size_v2_enabled' as bool")?,
            sampling_ratio: settings
                .remove("sampling_ratio")
                .map(serde_json::from_str)
                .transpose()
                .context("Failed to parse 'sampling_ratio'")?,
            relsize_snapshot_cache_capacity: settings
                .remove("relsize_snapshot_cache_capacity")
                .map(|x| x.parse::<usize>())
                .transpose()
                .context("Failed to parse 'relsize_snapshot_cache_capacity' as integer")?,
            basebackup_cache_enabled: settings
                .remove("basebackup_cache_enabled")
                .map(|x| x.parse::<bool>())
                .transpose()
                .context("Failed to parse 'basebackup_cache_enabled' as bool")?,
        };
        if !settings.is_empty() {
            bail!("Unrecognized tenant settings: {settings:?}")
        } else {
            Ok(result)
        }
    }

    /// List the branches this pageserver serves. In the per-endpoint
    /// topology a pageserver serves exactly one branch (the identity baked
    /// into its `pageserver.toml`), so this returns at most one
    /// [`TimelineInfo`], fetched via [`Self::timeline_info`].
    pub async fn timeline_list(&self) -> anyhow::Result<Vec<TimelineInfo>> {
        let Some(branch_id) = self.read_timeline_identity()? else {
            return Ok(Vec::new());
        };
        let info = self
            .timeline_info(branch_id, mgmt_api::ForceAwaitLogicalSize::No)
            .await?;
        Ok(vec![info])
    }

    pub async fn timeline_info(
        &self,
        branch_id: BranchId,
        force_await_logical_size: mgmt_api::ForceAwaitLogicalSize,
    ) -> anyhow::Result<TimelineInfo> {
        let timeline_info = self
            .http_client
            .timeline_info(branch_id, force_await_logical_size)
            .await?;
        Ok(timeline_info)
    }

    /// Flush the branch's pages to YB, advancing `consistent_lsn`. With
    /// `wait_lsn`, the pageserver waits for ingest to reach that LSN first.
    pub async fn flush(&self, branch_id: BranchId, wait_lsn: Option<Lsn>) -> anyhow::Result<()> {
        self.http_client.branch_flush(branch_id, wait_lsn).await?;
        Ok(())
    }
}
