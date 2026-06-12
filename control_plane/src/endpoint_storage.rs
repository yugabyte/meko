use crate::background_process::{self, start_process, stop_process};
use crate::local_env::{EndpointStorageConf, LocalEnv};
use anyhow::{Context, Result};
use camino::Utf8PathBuf;
use std::io::Write;
use std::net::SocketAddr;
use std::str::FromStr;
use std::time::Duration;

pub const ENDPOINT_STORAGE_DEFAULT_ADDR: SocketAddr =
    SocketAddr::new(std::net::IpAddr::V4(std::net::Ipv4Addr::LOCALHOST), 9993);

pub struct EndpointStorage {
    pub bin: Utf8PathBuf,
    pub data_dir: Utf8PathBuf,
    pub pemfile: Utf8PathBuf,
    pub addr: SocketAddr,
    /// Verbatim JSON object for the `storage_kind` block. None → fall
    /// back to a YbStore default derived from `storage_controller.database_url`
    /// (colocates with the `blobs` table that the pageserver uses).
    pub remote_storage_json: Option<String>,
    /// Fallback connection info used when `remote_storage_json` is `None`:
    /// the `storage_controller.database_url` from the outer LocalEnv,
    /// from which we derive the default YbStore JSON.
    pub storage_controller_database_url: Option<String>,
}

impl EndpointStorage {
    /// Build the runtime handle for the per-endpoint instance described by
    /// `conf` (its `listen_addr` and owning `endpoint_id`); the binary,
    /// pemfile and default-store database URL come from the shared `env`.
    pub fn from_conf(env: &LocalEnv, conf: &EndpointStorageConf) -> EndpointStorage {
        EndpointStorage {
            bin: Utf8PathBuf::from_path_buf(env.endpoint_storage_bin()).unwrap(),
            data_dir: Utf8PathBuf::from_path_buf(
                env.endpoint_storage_data_dir(&conf.endpoint_id),
            )
            .unwrap(),
            pemfile: Utf8PathBuf::from_path_buf(env.get_public_key_path()).unwrap(),
            addr: conf.listen_addr,
            remote_storage_json: conf.remote_storage_json.clone(),
            storage_controller_database_url: env.storage_controller.database_url.clone(),
        }
    }

    fn config_path(&self) -> Utf8PathBuf {
        self.data_dir.join("endpoint_storage.json")
    }

    fn listen_addr(&self) -> Utf8PathBuf {
        format!("{}:{}", self.addr.ip(), self.addr.port()).into()
    }

    /// Build the default YbStore JSON from `storage_controller.database_url`.
    /// Mirrors what the pageserver/safekeeper get for their `remote_storage`:
    /// same `yb_storage` DB and `blobs` table (endpoint_storage colocates
    /// with the pageserver). Requires `storage_controller.database_url` to
    /// be configured — without it we have no host/port/user to point at,
    /// and there's no other backend to fall back to.
    fn default_yb_store_json(&self) -> Result<String> {
        let url = self.storage_controller_database_url.as_deref().context(
            "endpoint_storage.remote_storage_json is unset and storage_controller.database_url \
             is also unset; cannot derive a default YbStore config. Set one of the two.",
        )?;
        let parsed = tokio_postgres::Config::from_str(url)
            .context("parsing storage_controller.database_url for endpoint_storage default")?;
        let host = parsed
            .get_hosts()
            .first()
            .and_then(|h| match h {
                tokio_postgres::config::Host::Tcp(h) => Some(h.clone()),
                #[cfg(unix)]
                tokio_postgres::config::Host::Unix(_) => None,
            })
            .context("storage_controller.database_url must include a TCP host")?;
        let port = parsed.get_ports().first().copied().unwrap_or(5433);
        let user = parsed
            .get_user()
            .context("storage_controller.database_url must include a user")?
            .to_string();
        let dbname = parsed.get_dbname().unwrap_or("yb_storage").to_string();
        let json = serde_json::json!({
            "type": "YbStore",
            "host": host,
            "port": port,
            "user": user,
            "dbname": dbname,
            "table_name": "blobs",
        });
        Ok(serde_json::to_string(&json)?)
    }

    pub fn init(&self) -> Result<()> {
        println!("Initializing object storage in {:?}", self.data_dir);

        // The endpoint_storage service deserializes the top-level config
        // via `#[serde(flatten)] storage_kind: TypedRemoteStorageKind`,
        // so we just inline the JSON object the user supplied (or the
        // default YbStore block we derive from storage_controller.database_url)
        // into the outer config.
        let storage_kind = match &self.remote_storage_json {
            Some(json) => json.clone(),
            None => self.default_yb_store_json()?,
        };

        let cfg = serde_json::json!({
            "listen": self.listen_addr(),
            "pemfile": self.pemfile.clone(),
        });
        let cfg_obj = cfg.as_object().unwrap().clone();
        let storage_obj: serde_json::Map<String, serde_json::Value> = serde_json::from_str(
            &storage_kind,
        )
        .context("parsing remote_storage_json: must be a JSON object with a `type` field")?;
        let mut merged: serde_json::Map<String, serde_json::Value> = cfg_obj;
        merged.extend(storage_obj);

        std::fs::create_dir_all(self.config_path().parent().unwrap())?;
        std::fs::write(
            self.config_path(),
            serde_json::to_string(&serde_json::Value::Object(merged))?,
        )
        .context("write object storage config")?;
        Ok(())
    }

    pub async fn start(&self, retry_timeout: &Duration) -> Result<()> {
        println!("Starting endpoint_storage at {}", self.listen_addr());
        std::io::stdout().flush().context("flush stdout")?;

        let process_status_check = || async {
            let res = reqwest::Client::new().get(format!("http://{}/metrics", self.listen_addr()));
            match res.send().await {
                Ok(res) => Ok(res.status().is_success()),
                Err(_) => Ok(false),
            }
        };

        let res = start_process(
            "endpoint_storage",
            &self.data_dir.clone().into_std_path_buf(),
            &self.bin.clone().into_std_path_buf(),
            vec![self.config_path().to_string()],
            vec![("RUST_LOG".into(), "debug".into())],
            background_process::InitialPidFile::Create(self.pid_file()),
            retry_timeout,
            process_status_check,
        )
        .await;
        if res.is_err() {
            eprintln!("Logs:\n{}", std::fs::read_to_string(self.log_file())?);
        }

        res
    }

    pub fn stop(&self, immediate: bool) -> anyhow::Result<()> {
        stop_process(immediate, "endpoint_storage", &self.pid_file())
    }

    fn log_file(&self) -> Utf8PathBuf {
        self.data_dir.join("endpoint_storage.log")
    }

    fn pid_file(&self) -> Utf8PathBuf {
        self.data_dir.join("endpoint_storage.pid")
    }
}
