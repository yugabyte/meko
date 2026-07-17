//! Code to manage safekeepers
//!
//! In the local test environment, each safekeeper is co-located under the
//! endpoint that owns it:
//!
//! ```text
//!   .yb_light/endpoints/<endpoint_id>/safekeeper
//! ```
use std::error::Error as _;
use std::io::Write;
use std::path::PathBuf;
use std::time::Duration;
use std::{io, result};

use anyhow::Context;
use camino::Utf8PathBuf;
use postgres_connection::PgConnectionConfig;
use safekeeper_api::config::SafekeeperConfigToml;
use safekeeper_api::models::TimelineCreateRequest;
use safekeeper_client::mgmt_api;
use thiserror::Error;
use utils::auth::{Claims, Scope};
use utils::id::NodeId;

use crate::background_process;
use crate::local_env::{LocalEnv, SafekeeperConf};

#[derive(Error, Debug)]
pub enum SafekeeperHttpError {
    #[error("request error: {0}{}", .0.source().map(|e| format!(": {e}")).unwrap_or_default())]
    Transport(#[from] reqwest::Error),

    #[error("Error: {0}")]
    Response(String),
}

type Result<T> = result::Result<T, SafekeeperHttpError>;

fn err_from_client_err(err: mgmt_api::Error) -> SafekeeperHttpError {
    use mgmt_api::Error::*;
    match err {
        ApiError(_, str) => SafekeeperHttpError::Response(str),
        Cancelled => SafekeeperHttpError::Response("Cancelled".to_owned()),
        ReceiveBody(err) => SafekeeperHttpError::Transport(err),
        ReceiveErrorBody(err) => SafekeeperHttpError::Response(err),
        Timeout(str) => SafekeeperHttpError::Response(format!("timeout: {str}")),
    }
}

//
// Control routines for safekeeper.
//
// Used in CLI and tests.
//
#[derive(Debug)]
pub struct SafekeeperNode {
    pub id: NodeId,

    pub conf: SafekeeperConf,

    pub pg_connection_config: PgConnectionConfig,
    pub env: LocalEnv,
    pub http_client: mgmt_api::Client,
    pub listen_addr: String,
}

impl SafekeeperNode {
    pub fn from_env(env: &LocalEnv, conf: &SafekeeperConf) -> SafekeeperNode {
        let listen_addr = if let Some(ref listen_addr) = conf.listen_addr {
            listen_addr.clone()
        } else {
            "127.0.0.1".to_string()
        };
        let jwt = None;
        let http_base_url = format!("http://{}:{}", listen_addr, conf.http_port);
        SafekeeperNode {
            id: conf.id,
            conf: conf.clone(),
            pg_connection_config: Self::safekeeper_connection_config(&listen_addr, conf.pg_port),
            env: env.clone(),
            http_client: mgmt_api::Client::new(env.create_http_client(), http_base_url, jwt),
            listen_addr,
        }
    }

    /// Construct libpq connection string for connecting to this safekeeper.
    fn safekeeper_connection_config(addr: &str, port: u16) -> PgConnectionConfig {
        PgConnectionConfig::new_host_port(url::Host::parse(addr).unwrap(), port)
    }

    pub fn datadir_path(&self) -> PathBuf {
        self.env.safekeeper_data_dir(&self.conf.endpoint_id)
    }

    pub fn pid_file(&self) -> Utf8PathBuf {
        Utf8PathBuf::from_path_buf(self.datadir_path().join("safekeeper.pid"))
            .expect("non-Unicode path")
    }

    /// Initializes a safekeeper node by creating all necessary files,
    /// e.g. SSL certificates and JWT token file.
    pub fn initialize(&self) -> anyhow::Result<()> {
        if self.env.use_local_tls {
            self.env.generate_ssl_cert(
                &self.datadir_path().join("server.crt"),
                &self.datadir_path().join("server.key"),
            )?;
        }

        // Generate a token file for authentication with other safekeepers
        if self.conf.auth_enabled {
            let token = self
                .env
                .generate_auth_token(&Claims::new(None, Scope::SafekeeperData))?;

            let token_path = self.datadir_path().join("peer_jwt_token");
            std::fs::write(token_path, token)?;
        }

        Ok(())
    }

    pub async fn start(
        &self,
        extra_opts: &[String],
        retry_timeout: &Duration,
    ) -> anyhow::Result<()> {
        println!(
            "Starting safekeeper at '{}' in '{}', retrying for {:?}",
            self.pg_connection_config.raw_address(),
            self.datadir_path().display(),
            retry_timeout,
        );
        io::stdout().flush().unwrap();

        let id = self.id;
        let datadir = self.datadir_path();
        let datadir_str = datadir
            .to_str()
            .with_context(|| {
                format!("Datadir path {datadir:?} cannot be represented as a unicode string")
            })?
            .to_owned();

        // All configuration is written to `<datadir>/safekeeper.toml`, which
        // the process reads on start — the safekeeper's analogue of
        // `pageserver.toml`. Only `-D` (plus any test overrides) stays on the
        // CLI. `backup_threads` has no toml field: `wal_backup` is removed in
        // yb-light, so it is always `None`.
        let auth_key = if self.conf.auth_enabled {
            Some(
                self.env
                    .base_data_dir
                    .join("auth_public_key.pem")
                    .to_str()
                    .context("auth key path is not unicode")?
                    .to_owned(),
            )
        } else {
            None
        };
        let auth_token_path = if self.conf.auth_enabled {
            Some(
                datadir
                    .join("peer_jwt_token")
                    .to_str()
                    .context("token path is not unicode")?
                    .to_owned(),
            )
        } else {
            None
        };
        let ssl_ca_file = match self.env.ssl_ca_cert_path() {
            Some(p) => Some(p.to_str().context("ssl ca path is not unicode")?.to_owned()),
            None => None,
        };

        // Test-only override of the periodic control-file save cadence
        // (humantime format, e.g. "1s"). The integration cfile-stall scenario
        // shrinks it so the save path fires within the test budget.
        let control_file_save_interval = match std::env::var("YB_SK_CONTROL_FILE_SAVE_INTERVAL") {
            Ok(s) => humantime::parse_duration(&s)
                .with_context(|| format!("parse YB_SK_CONTROL_FILE_SAVE_INTERVAL={s:?}"))?,
            Err(_) => SafekeeperConfigToml::default().control_file_save_interval,
        };

        let toml_cfg = SafekeeperConfigToml {
            id,
            branch_id: self.conf.branch_id,
            control_file_save_interval,
            listen_pg: format!("{}:{}", self.listen_addr, self.conf.pg_port),
            listen_pg_tenant_only: self
                .conf
                .pg_tenant_only_port
                .map(|p| format!("{}:{}", self.listen_addr, p)),
            listen_http: format!("{}:{}", self.listen_addr, self.conf.http_port),
            listen_https: self
                .conf
                .https_port
                .map(|p| format!("{}:{}", self.listen_addr, p)),
            availability_zone: Some(format!("sk-{id}")),
            yb_storage_conn: self.conf.yb_storage_conn.clone(),
            wal_cache_size_mib: self.conf.wal_cache_size_mib.map(|s| s as usize),
            pg_auth_public_key_path: auth_key.clone(),
            pg_tenant_only_auth_public_key_path: auth_key.clone(),
            http_auth_public_key_path: auth_key,
            auth_token_path,
            ssl_ca_file,
            ..Default::default()
        };
        std::fs::create_dir_all(&datadir)
            .with_context(|| format!("create safekeeper datadir {datadir:?}"))?;
        std::fs::write(
            datadir.join("safekeeper.toml"),
            toml_cfg
                .to_toml_string()
                .context("serialize safekeeper.toml")?,
        )
        .context("write safekeeper.toml")?;

        let mut args = vec!["-D".to_owned(), datadir_str];
        args.extend_from_slice(extra_opts);

        let env_variables = Vec::new();
        background_process::start_process(
            &format!("safekeeper-{id}"),
            &datadir,
            &self.env.safekeeper_bin(),
            &args,
            env_variables,
            background_process::InitialPidFile::Expect(self.pid_file()),
            retry_timeout,
            || async {
                match self.check_status().await {
                    Ok(()) => Ok(true),
                    Err(SafekeeperHttpError::Transport(_)) => Ok(false),
                    Err(e) => Err(anyhow::anyhow!("Failed to check node status: {e}")),
                }
            },
        )
        .await
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
        background_process::stop_process(
            immediate,
            &format!("safekeeper {}", self.id),
            &self.pid_file(),
        )
    }

    pub async fn check_status(&self) -> Result<()> {
        self.http_client
            .status()
            .await
            .map_err(err_from_client_err)?;
        Ok(())
    }

    pub async fn create_timeline(&self, req: &TimelineCreateRequest) -> Result<()> {
        self.http_client
            .create_timeline(req)
            .await
            .map_err(err_from_client_err)?;
        Ok(())
    }
}
