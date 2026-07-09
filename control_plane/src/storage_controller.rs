use anyhow::Context as _;
use std::ffi::OsStr;
use std::fs;
use std::path::PathBuf;
use std::process::ExitStatus;
use std::str::FromStr;
use std::sync::OnceLock;
use std::time::{Duration, Instant};

use crate::background_process;
use crate::local_env::{LocalEnv, YbLightStorageControllerConf};

// The storage controller's metadata DB lives in YB; talk to it
// through the YB smart-driver fork so `build_yb_rustls_connector`
// (which returns a `yb_postgres_openssl::MakeTlsConnector`) lines up
// with the Config / Client / Error types this file uses below. The
// rest of control_plane keeps using the Neon fork's `tokio_postgres`.
use camino::{Utf8Path, Utf8PathBuf};
use nix::unistd::Pid;
use pageserver_api::controller_api::{
    BranchCreateRequest, BranchCreateResponse, ComputeEndpoint, NodeConfigureRequest,
    NodeDescribeResponse, NodeRegisterRequest, PendingTimelineInit, PendingTimelineInitRequest,
    SetTimelineBranchNameRequest, TimelineBranchName,
};
use pageserver_api::models::BranchConfig;
use pageserver_client::mgmt_api::ResponseErrorMessageExt;
use pem::Pem;
use postgres_backend::AuthType;
use reqwest::{Method, Response};
use safekeeper_api::PgMajorVersion;
use serde::de::DeserializeOwned;
use serde::{Deserialize, Serialize};
use tokio::process::Command;
use tracing::instrument;
use url::Url;
use utils::auth::{Claims, Scope, encode_from_key_file};
use utils::id::{BranchId, NodeId};
use whoami::username;
use yb_tokio_postgres as tokio_postgres;

pub struct StorageController {
    env: LocalEnv,
    private_key: Option<Pem>,
    public_key: Option<Pem>,
    client: reqwest::Client,
    config: YbLightStorageControllerConf,

    // The listen port is learned when starting the storage controller,
    // hence the use of OnceLock to init it at the right time.
    listen_port: OnceLock<u16>,
}

const COMMAND: &str = "storage_controller";

const STORAGE_CONTROLLER_POSTGRES_VERSION: PgMajorVersion = PgMajorVersion::PG18;

// All three subsystems (storage_controller, pageserver YbStore for blobs,
// safekeeper for wals) share `yb_storage` on the YB cluster by default.
// The controller's tables live in a dedicated `storage_controller` schema
// inside this DB (see `storage_controller::persistence::SC_SCHEMA`); blobs
// sit in `public`, and the safekeeper's wals in its `safekeeper` schema —
// all in the same DB.
const DB_NAME: &str = "yb_storage";

pub struct YbLightStorageControllerStartArgs {
    pub instance_id: u8,
    pub base_port: Option<u16>,
    pub start_timeout: humantime::Duration,
}

impl YbLightStorageControllerStartArgs {
    pub fn with_default_instance_id(start_timeout: humantime::Duration) -> Self {
        Self {
            instance_id: 1,
            base_port: None,
            start_timeout,
        }
    }
}

pub struct YbLightStorageControllerStopArgs {
    pub instance_id: u8,
    pub immediate: bool,
}

impl YbLightStorageControllerStopArgs {
    pub fn with_default_instance_id(immediate: bool) -> Self {
        Self {
            instance_id: 1,
            immediate,
        }
    }
}

#[derive(Serialize, Deserialize)]
pub struct AttachHookRequest {
    pub branch_id: BranchId,
    pub node_id: Option<NodeId>,
    pub generation_override: Option<i32>, // only new branches
    pub config: Option<BranchConfig>,     // only new branches
}

#[derive(Serialize, Deserialize)]
pub struct AttachHookResponse {
    #[serde(rename = "gen")]
    pub generation: Option<u32>,
}

#[derive(Serialize, Deserialize)]
pub struct InspectRequest {
    pub branch_id: BranchId,
}

#[derive(Serialize, Deserialize)]
pub struct InspectResponse {
    pub attachment: Option<(u32, NodeId)>,
}

impl StorageController {
    pub fn from_env(env: &LocalEnv) -> Self {
        // Assume all pageservers have symmetric auth configuration: this service
        // expects to use one JWT token to talk to all of them. On a fresh repo
        // no pageserver is provisioned yet, so fall back to the init template
        // (and to Trust if there isn't even a template).
        let http_auth_type = env
            .pageservers
            .first()
            .map(|p| p.http_auth_type)
            .or_else(|| env.pageserver_templates.first().map(|t| t.http_auth_type))
            .unwrap_or(AuthType::Trust);
        let (private_key, public_key) = match http_auth_type {
            AuthType::Trust => (None, None),
            AuthType::YbLightJWT => {
                let private_key_path = env.get_private_key_path();
                let private_key =
                    pem::parse(fs::read(private_key_path).expect("failed to read private key"))
                        .expect("failed to parse PEM file");

                // If pageserver auth is enabled, this implicitly enables auth for this service,
                // using the same credentials.
                let public_key_path =
                    camino::Utf8PathBuf::try_from(env.base_data_dir.join("auth_public_key.pem"))
                        .unwrap();

                // This service takes keys as a string rather than as a path to a file/dir: read the key into memory.
                let public_key = if std::fs::metadata(&public_key_path)
                    .expect("Can't stat public key")
                    .is_dir()
                {
                    // Our config may specify a directory: this is for the pageserver's ability to handle multiple
                    // keys.  We only use one key at a time, so, arbitrarily load the first one in the directory.
                    let mut dir =
                        std::fs::read_dir(&public_key_path).expect("Can't readdir public key path");
                    let dent = dir
                        .next()
                        .expect("Empty key dir")
                        .expect("Error reading key dir");

                    pem::parse(std::fs::read_to_string(dent.path()).expect("Can't read public key"))
                        .expect("Failed to parse PEM file")
                } else {
                    pem::parse(
                        std::fs::read_to_string(&public_key_path).expect("Can't read public key"),
                    )
                    .expect("Failed to parse PEM file")
                };
                (Some(private_key), Some(public_key))
            }
        };

        Self {
            env: env.clone(),
            private_key,
            public_key,
            client: env.create_http_client(),
            config: env.storage_controller.clone(),
            listen_port: OnceLock::default(),
        }
    }

    fn storage_controller_instance_dir(&self, instance_id: u8) -> PathBuf {
        self.env
            .base_data_dir
            .join(format!("storage_controller_{instance_id}"))
    }

    fn pid_file(&self, instance_id: u8) -> Utf8PathBuf {
        Utf8PathBuf::from_path_buf(
            self.storage_controller_instance_dir(instance_id)
                .join("storage_controller.pid"),
        )
        .expect("non-Unicode path")
    }

    /// Find the directory containing postgres subdirectories, such `bin` and `lib`
    ///
    /// This usually uses STORAGE_CONTROLLER_POSTGRES_VERSION of postgres, but will fall back
    /// to other versions if that one isn't found.  Some automated tests create circumstances
    /// where only one version is available in pg_distrib_dir, such as `test_remote_extensions`.
    async fn get_pg_dir(&self, dir_name: &str) -> anyhow::Result<Utf8PathBuf> {
        const PREFER_VERSIONS: [PgMajorVersion; 1] = [STORAGE_CONTROLLER_POSTGRES_VERSION];

        for v in PREFER_VERSIONS {
            let path = Utf8PathBuf::from_path_buf(self.env.pg_dir(v, dir_name)?).unwrap();
            if tokio::fs::try_exists(&path).await? {
                return Ok(path);
            }
        }

        // Fall through
        anyhow::bail!(
            "Postgres directory '{}' not found in {}",
            dir_name,
            self.env.pg_distrib_dir.display(),
        );
    }

    pub async fn get_pg_bin_dir(&self) -> anyhow::Result<Utf8PathBuf> {
        self.get_pg_dir("bin").await
    }

    pub async fn get_pg_lib_dir(&self) -> anyhow::Result<Utf8PathBuf> {
        self.get_pg_dir("lib").await
    }

    /// Readiness check for our postgres process
    async fn pg_isready(&self, pg_bin_dir: &Utf8Path, postgres_port: u16) -> anyhow::Result<bool> {
        let bin_path = pg_bin_dir.join("pg_isready");
        let args = [
            "-h",
            "localhost",
            "-U",
            &username(),
            "-d",
            DB_NAME,
            "-p",
            &format!("{postgres_port}"),
        ];
        let pg_lib_dir = self.get_pg_lib_dir().await.unwrap();
        let envs = [
            ("LD_LIBRARY_PATH".to_owned(), pg_lib_dir.to_string()),
            ("DYLD_LIBRARY_PATH".to_owned(), pg_lib_dir.to_string()),
        ];
        let exitcode = Command::new(bin_path)
            .args(args)
            .envs(envs)
            .spawn()?
            .wait()
            .await?;

        Ok(exitcode.success())
    }

    /// Create our database if it doesn't exist
    ///
    /// This function is equivalent to the `diesel setup` command in the diesel CLI.  We implement
    /// the same steps by hand to avoid imposing a dependency on installing diesel-cli for developers
    /// who just want to run `cargo yb_light_local` without knowing about diesel.
    ///
    /// Returns the database url
    pub async fn setup_database(&self, postgres_port: u16) -> anyhow::Result<String> {
        let database_url = format!(
            "postgresql://{}@localhost:{}/{DB_NAME}",
            &username(),
            postgres_port
        );

        let pg_bin_dir = self.get_pg_bin_dir().await?;
        let createdb_path = pg_bin_dir.join("createdb");
        let pg_lib_dir = self.get_pg_lib_dir().await.unwrap();
        let envs = [
            ("LD_LIBRARY_PATH".to_owned(), pg_lib_dir.to_string()),
            ("DYLD_LIBRARY_PATH".to_owned(), pg_lib_dir.to_string()),
        ];
        let output = Command::new(&createdb_path)
            .args([
                "-h",
                "localhost",
                "-p",
                &format!("{postgres_port}"),
                "-U",
                &username(),
                "-O",
                &username(),
                DB_NAME,
            ])
            .envs(envs)
            .output()
            .await
            .expect("Failed to spawn createdb");

        if !output.status.success() {
            let stderr = String::from_utf8(output.stderr).expect("Non-UTF8 output from createdb");
            if stderr.contains("already exists") {
                tracing::info!("Database {DB_NAME} already exists");
            } else {
                anyhow::bail!("createdb failed with status {}: {stderr}", output.status);
            }
        }

        Ok(database_url)
    }

    /// Provision the external (YugabyteDB) cluster before the rest of
    /// yb_light_local starts: ensure both the `storage_controller` database
    /// (Diesel migrations populate the tables) and the `yb_storage`
    /// database with its `blob_namespace` type + `blobs` / `wals` tables
    /// (operator schema that pageserver+safekeeper `YbStore` writes to)
    /// exist. All steps are idempotent.
    ///
    /// We connect to YugabyteDB's `yugabyte` admin database with the same
    /// host/user/credentials as the controller URL to issue
    /// `CREATE DATABASE`.
    async fn ensure_external_database(&self, yb: &ysql_client::YbConnConfig) -> anyhow::Result<()> {
        let url = yb
            .to_database_url()
            .context("building database URL from storage_controller.yb")?;
        let target = tokio_postgres::Config::from_str(&url)
            .context("parsing storage_controller.yb database URL")?;
        let controller_db = yb.dbname.clone();

        // The controller's tables live in the `storage_controller`
        // schema inside `yb_storage`; `blobs`/`wals` and the
        // `pageserver.*` page-store tables share the same DB. We log
        // if the operator pointed the URL elsewhere — that still
        // works, but they lose the colocation invariant.
        //
        // The schema itself (every table + ENUM + index) is created
        // by storage_controller's diesel migrations once the binary
        // is up; here we only ensure the database container exists
        // so the controller can connect to it.
        if controller_db != "yb_storage" {
            tracing::warn!(
                "storage_controller.yb names db {controller_db:?} \
                 (not the default `yb_storage`); blobs/wals colocation \
                 assumes yb_storage — override deliberately or update the config"
            );
        }
        Self::create_database(&target, &controller_db).await?;
        if controller_db != "yb_storage" {
            Self::create_database(&target, "yb_storage").await?;
        }
        Ok(())
    }

    /// Open a one-shot connection to an external (YugabyteDB) cluster,
    /// honouring the parsed `Config`'s SSL mode. Used by the YB
    /// provisioning helpers (`create_database`).
    /// Returns the `Client` plus the spawned connection-task handle.
    ///
    /// When the URL has `sslmode=disable` (or no sslmode) we use `NoTls`;
    /// any non-`Disable` mode uses the rustls connector from
    /// `remote_storage` with the AcceptAll verifier — same TLS posture
    /// the storage_controller binary itself uses.
    async fn connect_external(
        config: &tokio_postgres::Config,
        what: &str,
    ) -> anyhow::Result<(
        tokio_postgres::Client,
        tokio::task::JoinHandle<Result<(), tokio_postgres::Error>>,
    )> {
        use tokio_postgres::config::SslMode;
        let use_tls = !matches!(config.get_ssl_mode(), SslMode::Disable);
        if use_tls {
            // No sslrootcert is exposed via the URL parse, so use AcceptAll.
            let tls = remote_storage::build_yb_rustls_connector(None)
                .context("building rustls connector for storage controller DB URL")?;
            let (client, conn) = config
                .connect(tls)
                .await
                .with_context(|| format!("connecting to {what} (TLS)"))?;
            let handle = tokio::spawn(conn);
            Ok((client, handle))
        } else {
            let (client, conn) = config
                .connect(tokio_postgres::NoTls)
                .await
                .with_context(|| format!("connecting to {what}"))?;
            let handle = tokio::spawn(conn);
            Ok((client, handle))
        }
    }

    /// `CREATE DATABASE <name>` against the `yugabyte` admin DB on the
    /// cluster described by `base`. Treats "already exists" as success.
    async fn create_database(base: &tokio_postgres::Config, name: &str) -> anyhow::Result<()> {
        let mut admin = base.clone();
        admin.dbname("yugabyte");
        let (client, conn_task) =
            Self::connect_external(&admin, "YB admin database 'yugabyte'").await?;

        let create_sql = format!("CREATE DATABASE \"{}\"", name.replace('"', "\"\""));
        match client.batch_execute(&create_sql).await {
            Ok(()) => {
                println!("Created database '{name}'");
            }
            Err(e) => {
                let is_dup = e.as_db_error().is_some_and(|d| {
                    d.code() == &tokio_postgres::error::SqlState::DUPLICATE_DATABASE
                });
                if !is_dup {
                    return Err(
                        anyhow::Error::from(e).context(format!("creating database '{name}'"))
                    );
                }
            }
        }
        drop(client);
        conn_task.await??;
        Ok(())
    }

    pub async fn connect_to_database(
        &self,
        postgres_port: u16,
    ) -> anyhow::Result<(
        tokio_postgres::Client,
        tokio_postgres::Connection<tokio_postgres::Socket, tokio_postgres::tls::NoTlsStream>,
    )> {
        tokio_postgres::Config::new()
            .host("localhost")
            .port(postgres_port)
            // The user is the ambient operating system user name.
            // That is an impurity which we want to fix in => TODO https://github.com/neondatabase/neon/issues/8400
            //
            // Until we get there, use the ambient operating system user name.
            // Recent tokio-postgres versions default to this if the user isn't specified.
            // But tokio-postgres fork doesn't have this upstream commit:
            // https://github.com/sfackler/rust-postgres/commit/cb609be758f3fb5af537f04b584a2ee0cebd5e79
            // => we should rebase our fork => TODO https://github.com/neondatabase/neon/issues/8399
            .user(&username())
            .dbname(DB_NAME)
            .connect(tokio_postgres::NoTls)
            .await
            .map_err(anyhow::Error::new)
    }

    /// Wrapper for the pg_ctl binary, which we spawn as a short-lived subprocess when starting and stopping postgres
    async fn pg_ctl<I, S>(&self, args: I) -> ExitStatus
    where
        I: IntoIterator<Item = S>,
        S: AsRef<OsStr>,
    {
        let pg_bin_dir = self.get_pg_bin_dir().await.unwrap();
        let bin_path = pg_bin_dir.join("pg_ctl");

        let pg_lib_dir = self.get_pg_lib_dir().await.unwrap();
        let envs = [
            ("LD_LIBRARY_PATH".to_owned(), pg_lib_dir.to_string()),
            ("DYLD_LIBRARY_PATH".to_owned(), pg_lib_dir.to_string()),
        ];

        Command::new(bin_path)
            .args(args)
            .envs(envs)
            .spawn()
            .expect("Failed to spawn pg_ctl, binary_missing?")
            .wait()
            .await
            .expect("Failed to wait for pg_ctl termination")
    }

    /// Idempotent: ensure the external `yb_storage` database exists and
    /// has the pageserver/safekeeper schema in place. Callers must run
    /// this **before** spawning pageserver / safekeeper / endpoint_storage
    /// since those bind to `yb_storage` immediately on start. Without
    /// this barrier, a fresh `yb_light_local start` races storage_controller's
    /// own database provisioning against the pageserver's first connect
    /// and the pageserver crashes with `database "yb_storage" does not
    /// exist`. See #49.
    pub async fn prepare_database_if_external(&self) -> anyhow::Result<()> {
        if let Some(yb) = self.config.yb.clone() {
            self.ensure_external_database(&yb).await?;
        }
        Ok(())
    }

    pub async fn start(&self, start_args: YbLightStorageControllerStartArgs) -> anyhow::Result<()> {
        let instance_dir = self.storage_controller_instance_dir(start_args.instance_id);
        if let Err(err) = tokio::fs::create_dir(&instance_dir).await {
            if err.kind() != std::io::ErrorKind::AlreadyExists {
                panic!("Failed to create instance dir {instance_dir:?}");
            }
        }

        if self.env.use_local_tls {
            self.env.generate_ssl_cert(
                &instance_dir.join("server.crt"),
                &instance_dir.join("server.key"),
            )?;
        }

        let listen_url = &self.env.control_plane_api;

        let scheme = listen_url.scheme();
        let host = listen_url.host_str().unwrap();

        // If `yb` is set in YbLightStorageControllerConf, the operator
        // is pointing the controller at an externally-managed Postgres (in
        // yb-light: a YugabyteDB cluster). In that case we skip the local-PG
        // bootstrap entirely.
        let external_yb = self.config.yb.clone();

        let (listen_port, postgres_port) = match (start_args.base_port, &external_yb) {
            (Some(base_port), Some(yb)) => {
                // HA / multi-instance mode against an external DB: derive
                // postgres_port from the first YB endpoint so the
                // connect_to_database() path below (used only for the startup
                // script) still works when external_yb is None at some sites.
                // The port here is otherwise unused on the external path.
                let port = yb
                    .host_ports
                    .0
                    .first()
                    .map(|hp| hp.port)
                    .unwrap_or(5433);
                (base_port, port)
            }
            (Some(_), None) => {
                anyhow::bail!("--base-port requires YbLightStorageControllerConf::yb");
            }
            (None, _) => {
                let port = listen_url.port().unwrap();
                (port, port + 1)
            }
        };

        self.listen_port
            .set(listen_port)
            .expect("StorageController::listen_port is only set here");

        // Needed below to set LD_LIBRARY_PATH for the storage_controller
        // process itself (it links against libpq).
        let pg_lib_dir = self.get_pg_lib_dir().await?;

        // Against an external database (e.g. YugabyteDB), make sure the
        // target database itself exists before pointing the controller at
        // it. The controller's Diesel migrations create the schema inside
        // the DB, but they don't create the DB container.
        if let Some(yb) = &external_yb {
            self.ensure_external_database(yb).await?;
        }

        // Local-PG bootstrap, skipped when an external database is configured.
        let pg_started = if external_yb.is_none() {
            self.is_postgres_running().await?
        } else {
            true
        };

        if !pg_started {
            // Start a vanilla Postgres process used by the storage controller for persistence.
            let pg_data_path = Utf8PathBuf::from_path_buf(self.env.base_data_dir.clone())
                .unwrap()
                .join("storage_controller_db");
            let pg_bin_dir = self.get_pg_bin_dir().await?;
            let pg_log_path = pg_data_path.join("postgres.log");

            if !tokio::fs::try_exists(&pg_data_path).await? {
                let initdb_args = [
                    "--pgdata",
                    pg_data_path.as_ref(),
                    "--username",
                    &username(),
                    "--no-sync",
                    "--no-instructions",
                ];
                tracing::info!(
                    "Initializing storage controller database with args: {:?}",
                    initdb_args
                );

                // Initialize empty database
                let initdb_path = pg_bin_dir.join("initdb");
                let mut child = Command::new(&initdb_path)
                    .envs(vec![
                        ("LD_LIBRARY_PATH".to_owned(), pg_lib_dir.to_string()),
                        ("DYLD_LIBRARY_PATH".to_owned(), pg_lib_dir.to_string()),
                    ])
                    .args(initdb_args)
                    .spawn()
                    .expect("Failed to spawn initdb");
                let status = child.wait().await?;
                if !status.success() {
                    anyhow::bail!("initdb failed with status {status}");
                }
            };

            // Write a minimal config file:
            // - Specify the port, since this is chosen dynamically
            // - Switch off fsync, since we're running on lightweight test environments and when e.g. scale testing
            //   the storage controller we don't want a slow local disk to interfere with that.
            //
            // NB: it's important that we rewrite this file on each start command so we propagate changes
            // from `LocalEnv`'s config file (`.yb_light/config`).
            tokio::fs::write(
                &pg_data_path.join("postgresql.conf"),
                format!("port = {postgres_port}\nfsync=off\n"),
            )
            .await?;

            println!("Starting storage controller database...");
            let db_start_args = [
                "-w",
                "-D",
                pg_data_path.as_ref(),
                "-l",
                pg_log_path.as_ref(),
                "-U",
                &username(),
                "start",
            ];
            tracing::info!(
                "Starting storage controller database with args: {:?}",
                db_start_args
            );

            let db_start_status = self.pg_ctl(db_start_args).await;
            let start_timeout: Duration = start_args.start_timeout.into();
            let db_start_deadline = Instant::now() + start_timeout;
            if !db_start_status.success() {
                return Err(anyhow::anyhow!(
                    "Failed to start postgres {}",
                    db_start_status.code().unwrap()
                ));
            }

            loop {
                if Instant::now() > db_start_deadline {
                    return Err(anyhow::anyhow!("Timed out waiting for postgres to start"));
                }

                match self.pg_isready(&pg_bin_dir, postgres_port).await {
                    Ok(true) => {
                        tracing::info!("storage controller postgres is now ready");
                        break;
                    }
                    Ok(false) => {
                        tokio::time::sleep(Duration::from_millis(100)).await;
                    }
                    Err(e) => {
                        tracing::warn!("Failed to check postgres status: {e}")
                    }
                }
            }

            self.setup_database(postgres_port).await?;
        }

        // The connection is passed to the storage_controller binary as either
        // the structured `--yb-config` block (external YB; the binary builds
        // its diesel URL from it) or a `--database-url` URL (local-PG
        // bootstrap). Built just before the arg vector below.
        let local_database_url = format!("postgresql://localhost:{postgres_port}/{DB_NAME}");

        // We support running a startup SQL script to fiddle with the database before we launch storcon.
        // This is used by the test suite — only meaningful against the local-PG bootstrap path.
        if external_yb.is_none() {
            let startup_script_path = self
                .env
                .base_data_dir
                .join("storage_controller_db.startup.sql");
            let startup_script = match tokio::fs::read_to_string(&startup_script_path).await {
                Ok(script) => {
                    tokio::fs::remove_file(startup_script_path).await?;
                    script
                }
                Err(e) => {
                    if e.kind() == std::io::ErrorKind::NotFound {
                        // always run some startup script so that this code path doesn't bit rot
                        "BEGIN; COMMIT;".to_string()
                    } else {
                        anyhow::bail!("Failed to read startup script: {e}")
                    }
                }
            };
            let (mut client, conn) = self.connect_to_database(postgres_port).await?;
            let conn = tokio::spawn(conn);
            let tx = client.build_transaction();
            let tx = tx.start().await?;
            tx.batch_execute(&startup_script).await?;
            tx.commit().await?;
            drop(client);
            conn.await??;
        }

        let addr = format!("{host}:{listen_port}");
        // External YB → pass the shared `[*.yb]` block as `--yb-config`;
        // local-PG bootstrap → pass the diesel URL as `--database-url`.
        let (db_flag, db_value) = match &external_yb {
            Some(yb) => ("--yb-config", yb.to_inline_toml()),
            None => ("--database-url", local_database_url),
        };
        let mut args = vec![
            "--dev",
            db_flag,
            &db_value,
            "--max-offline-interval",
            &humantime::Duration::from(self.config.max_offline).to_string(),
            "--max-warming-up-interval",
            &humantime::Duration::from(self.config.max_warming_up).to_string(),
            "--heartbeat-interval",
            &humantime::Duration::from(self.config.heartbeat_interval).to_string(),
        ]
        .into_iter()
        .map(|s| s.to_string())
        .collect::<Vec<_>>();

        match scheme {
            "http" => args.extend(["--listen".to_string(), addr]),
            "https" => args.extend(["--listen-https".to_string(), addr]),
            _ => {
                panic!("Unexpected url scheme in control_plane_api: {scheme}");
            }
        }

        if self.config.use_https_pageserver_api {
            args.push("--use-https-pageserver-api".to_string());
        }

        if self.config.use_https_safekeeper_api {
            args.push("--use-https-safekeeper-api".to_string());
        }

        if let Some(ssl_ca_file) = self.env.ssl_ca_cert_path() {
            args.push(format!("--ssl-ca-file={}", ssl_ca_file.to_str().unwrap()));
        }

        if let Some(private_key) = &self.private_key {
            let claims = Claims::new(None, Scope::PageServerApi);
            let jwt_token =
                encode_from_key_file(&claims, private_key).expect("failed to generate jwt token");
            args.push(format!("--jwt-token={jwt_token}"));

            let claims = Claims::new(None, Scope::SafekeeperData);
            let jwt_token =
                encode_from_key_file(&claims, private_key).expect("failed to generate jwt token");
            args.push(format!("--safekeeper-jwt-token={jwt_token}"));
        }

        if let Some(public_key) = &self.public_key {
            args.push(format!("--public-key=\"{public_key}\""));
        }

        if let Some(control_plane_hooks_api) = &self.env.control_plane_hooks_api {
            args.push(format!("--control-plane-url={control_plane_hooks_api}"));
        }

        // Safekeepers aren't provisioned until `endpoint create`, so on a
        // fresh repo `env.safekeepers` is empty; fall back to the init
        // templates for the auth check and the per-timeline count.
        let auth_enabled = self
            .env
            .safekeepers
            .iter()
            .chain(self.env.safekeeper_templates.iter())
            .any(|sk| sk.auth_enabled);
        if auth_enabled && self.private_key.is_none() {
            anyhow::bail!("Safekeeper set up for auth but no private key specified");
        }

        if self.config.timelines_onto_safekeepers {
            args.push("--timelines-onto-safekeepers".to_string());
        }

        // Per-timeline safekeeper count: how many safekeepers each new
        // timeline is placed on. In yb-light this is the size of the init
        // safekeeper template set (one safekeeper per timeline), not the
        // running total across all provisioned timelines.
        let timeline_sk_count = self
            .config
            .timeline_safekeeper_count
            .unwrap_or(self.env.safekeeper_templates.len());
        // yb_light_local is used in test environments where we often have less than 3 safekeepers.
        if self.config.timeline_safekeeper_count.is_some() || timeline_sk_count < 3 {
            args.push(format!("--timeline-safekeeper-count={timeline_sk_count}"));
        }

        let envs = vec![
            ("LD_LIBRARY_PATH".to_owned(), pg_lib_dir.to_string()),
            ("DYLD_LIBRARY_PATH".to_owned(), pg_lib_dir.to_string()),
        ];

        println!("Starting storage controller at {scheme}://{host}:{listen_port}");

        background_process::start_process(
            COMMAND,
            &instance_dir,
            &self.env.storage_controller_bin(),
            args,
            envs,
            background_process::InitialPidFile::Create(self.pid_file(start_args.instance_id)),
            &start_args.start_timeout,
            || async {
                match self.ready().await {
                    Ok(_) => Ok(true),
                    Err(_) => Ok(false),
                }
            },
        )
        .await?;

        Ok(())
    }

    pub async fn stop(&self, stop_args: YbLightStorageControllerStopArgs) -> anyhow::Result<()> {
        background_process::stop_process(
            stop_args.immediate,
            COMMAND,
            &self.pid_file(stop_args.instance_id),
        )?;

        let storcon_instances = self.env.storage_controller_instances().await?;
        for (instance_id, instanced_dir_path) in storcon_instances {
            if instance_id == stop_args.instance_id {
                continue;
            }

            let pid_file = instanced_dir_path.join("storage_controller.pid");
            let pid = tokio::fs::read_to_string(&pid_file)
                .await
                .map_err(|err| {
                    anyhow::anyhow!("Failed to read storcon pid file at {pid_file:?}: {err}")
                })?
                .parse::<i32>()
                .expect("pid is valid i32");

            let other_proc_alive = !background_process::process_has_stopped(Pid::from_raw(pid))?;
            if other_proc_alive {
                // There is another storage controller instance running, so we return
                // and leave the database running.
                return Ok(());
            }
        }

        // When pointed at an external database (e.g. YugabyteDB), yb_light_local
        // never started a local Postgres, so there's nothing to stop here.
        if self.config.yb.is_some() {
            return Ok(());
        }

        let pg_data_path = self.env.base_data_dir.join("storage_controller_db");

        println!("Stopping storage controller database...");
        let pg_stop_args = ["-D", &pg_data_path.to_string_lossy(), "stop"];
        let stop_status = self.pg_ctl(pg_stop_args).await;
        if !stop_status.success() {
            match self.is_postgres_running().await {
                Ok(false) => {
                    println!("Storage controller database is already stopped");
                    return Ok(());
                }
                Ok(true) => {
                    anyhow::bail!("Failed to stop storage controller database");
                }
                Err(err) => {
                    anyhow::bail!("Failed to stop storage controller database: {err}");
                }
            }
        }

        Ok(())
    }

    async fn is_postgres_running(&self) -> anyhow::Result<bool> {
        let pg_data_path = self.env.base_data_dir.join("storage_controller_db");

        let pg_status_args = ["-D", &pg_data_path.to_string_lossy(), "status"];
        let status_exitcode = self.pg_ctl(pg_status_args).await;

        // pg_ctl status returns this exit code if postgres is not running: in this case it is
        // fine that stop failed.  Otherwise it is an error that stop failed.
        const PG_STATUS_NOT_RUNNING: i32 = 3;
        const PG_NO_DATA_DIR: i32 = 4;
        const PG_STATUS_RUNNING: i32 = 0;
        match status_exitcode.code() {
            Some(PG_STATUS_NOT_RUNNING) => Ok(false),
            Some(PG_NO_DATA_DIR) => Ok(false),
            Some(PG_STATUS_RUNNING) => Ok(true),
            Some(code) => Err(anyhow::anyhow!(
                "pg_ctl status returned unexpected status code: {:?}",
                code
            )),
            None => Err(anyhow::anyhow!("pg_ctl status returned no status code")),
        }
    }

    fn get_claims_for_path(path: &str) -> anyhow::Result<Option<Claims>> {
        let category = match path.find('/') {
            Some(idx) => &path[..idx],
            None => path,
        };

        match category {
            "status" | "ready" => Ok(None),
            "control" | "debug" => Ok(Some(Claims::new(None, Scope::Admin))),
            "v1" => Ok(Some(Claims::new(None, Scope::PageServerApi))),
            _ => Err(anyhow::anyhow!("Failed to determine claims for {}", path)),
        }
    }

    /// Simple HTTP request wrapper for calling into storage controller
    async fn dispatch<RQ, RS>(
        &self,
        method: reqwest::Method,
        path: String,
        body: Option<RQ>,
    ) -> anyhow::Result<RS>
    where
        RQ: Serialize + Sized,
        RS: DeserializeOwned + Sized,
    {
        let response = self.dispatch_inner(method, path, body).await?;
        Ok(response
            .json()
            .await
            .map_err(pageserver_client::mgmt_api::Error::ReceiveBody)?)
    }

    /// Simple HTTP request wrapper for calling into storage controller
    async fn dispatch_inner<RQ>(
        &self,
        method: reqwest::Method,
        path: String,
        body: Option<RQ>,
    ) -> anyhow::Result<Response>
    where
        RQ: Serialize + Sized,
    {
        // In the special case of the `storage_controller start` subcommand, we wish
        // to use the API endpoint of the newly started storage controller in order
        // to pass the readiness check. In this scenario [`Self::listen_port`] will
        // be set (see [`Self::start`]).
        //
        // Otherwise, we infer the storage controller api endpoint from the configured
        // control plane API.
        let port = if let Some(port) = self.listen_port.get() {
            *port
        } else {
            self.env.control_plane_api.port().unwrap()
        };

        // The configured URL has the /upcall path prefix for pageservers to use: we will strip that out
        // for general purpose API access.
        let url = Url::from_str(&format!(
            "{}://{}:{port}/{path}",
            self.env.control_plane_api.scheme(),
            self.env.control_plane_api.host_str().unwrap(),
        ))
        .unwrap();

        let mut builder = self.client.request(method, url);
        if let Some(body) = body {
            builder = builder.json(&body)
        }
        if let Some(private_key) = &self.private_key {
            println!("Getting claims for path {path}");
            if let Some(required_claims) = Self::get_claims_for_path(&path)? {
                println!("Got claims {required_claims:?} for path {path}");
                let jwt_token = encode_from_key_file(&required_claims, private_key)?;
                builder = builder.header(
                    reqwest::header::AUTHORIZATION,
                    format!("Bearer {jwt_token}"),
                );
            }
        }

        let response = builder.send().await?;
        let response = response.error_from_body().await?;

        Ok(response)
    }

    /// Call into the attach_hook API, for use before handing out attachments to pageservers
    #[instrument(skip(self))]
    pub async fn attach_hook(
        &self,
        branch_id: BranchId,
        pageserver_id: NodeId,
    ) -> anyhow::Result<Option<u32>> {
        let request = AttachHookRequest {
            branch_id,
            node_id: Some(pageserver_id),
            generation_override: None,
            config: None,
        };

        let response = self
            .dispatch::<_, AttachHookResponse>(
                Method::POST,
                "debug/v1/attach-hook".to_string(),
                Some(request),
            )
            .await?;

        Ok(response.generation)
    }

    #[instrument(skip(self))]
    pub async fn inspect(
        &self,
        branch_id: BranchId,
    ) -> anyhow::Result<Option<(u32, NodeId)>> {
        let request = InspectRequest { branch_id };

        let response = self
            .dispatch::<_, InspectResponse>(
                Method::POST,
                "debug/v1/inspect".to_string(),
                Some(request),
            )
            .await?;

        Ok(response.attachment)
    }

    #[instrument(skip(self))]
    pub async fn branch_create(
        &self,
        req: BranchCreateRequest,
    ) -> anyhow::Result<BranchCreateResponse> {
        self.dispatch(Method::POST, "v1/branch".to_string(), Some(req))
            .await
    }

    #[instrument(skip(self))]
    pub async fn branch_import(&self, branch_id: BranchId) -> anyhow::Result<BranchCreateResponse> {
        self.dispatch::<(), BranchCreateResponse>(
            Method::POST,
            format!("debug/v1/branch/{branch_id}/import"),
            None,
        )
        .await
    }

    #[instrument(skip_all, fields(node_id=%req.node_id))]
    pub async fn node_register(&self, req: NodeRegisterRequest) -> anyhow::Result<()> {
        self.dispatch::<_, ()>(Method::POST, "control/v1/node".to_string(), Some(req))
            .await
    }

    #[instrument(skip_all, fields(node_id=%req.node_id))]
    pub async fn node_configure(&self, req: NodeConfigureRequest) -> anyhow::Result<()> {
        self.dispatch::<_, ()>(
            Method::PUT,
            format!("control/v1/node/{}/config", req.node_id),
            Some(req),
        )
        .await
    }

    pub async fn node_list(&self) -> anyhow::Result<Vec<NodeDescribeResponse>> {
        self.dispatch::<(), Vec<NodeDescribeResponse>>(
            Method::GET,
            "control/v1/node".to_string(),
            None,
        )
        .await
    }

    #[instrument(skip(self))]
    pub async fn ready(&self) -> anyhow::Result<()> {
        self.dispatch::<(), ()>(Method::GET, "ready".to_string(), None)
            .await
    }

    /// Register a launcher-created branch in the storage
    /// controller's `timelines` table.
    #[instrument(skip(self))]
    pub async fn timeline_register(
        &self,
        branch_id: BranchId,
        req: pageserver_api::controller_api::TimelineRegisterRequest,
    ) -> anyhow::Result<()> {
        self.dispatch::<_, ()>(
            Method::POST,
            format!("control/v1/branch/{branch_id}/register"),
            Some(req),
        )
        .await
    }

    pub async fn set_timeline_branch_name(
        &self,
        branch_id: BranchId,
        branch_name: Option<String>,
    ) -> anyhow::Result<()> {
        self.dispatch::<_, ()>(
            Method::PUT,
            format!("control/v1/branch/{branch_id}/branch_name"),
            Some(SetTimelineBranchNameRequest { branch_name }),
        )
        .await
    }

    pub async fn list_timeline_branch_names(&self) -> anyhow::Result<Vec<TimelineBranchName>> {
        self.dispatch::<(), _>(
            Method::GET,
            "control/v1/timeline_branch_names".to_string(),
            None,
        )
        .await
    }

    /// Record a pending timeline init recipe (`branch create` allocates the
    /// id before the PS+SK exist). Idempotent on the branch_id key.
    pub async fn record_pending_timeline_init(
        &self,
        branch_id: BranchId,
        req: PendingTimelineInitRequest,
    ) -> anyhow::Result<()> {
        self.dispatch::<_, ()>(
            Method::POST,
            format!("control/v1/branch/{branch_id}/pending_init"),
            Some(req),
        )
        .await
    }

    /// List the pending timeline inits recorded for a branch. Used by
    /// `endpoint create` to resolve an unprovisioned branch and read its
    /// materialization recipe.
    pub async fn list_pending_timeline_inits(
        &self,
    ) -> anyhow::Result<Vec<PendingTimelineInit>> {
        self.dispatch::<(), _>(
            Method::GET,
            "control/v1/pending_timeline_inits".to_string(),
            None,
        )
        .await
    }

    /// Delete a pending timeline init after its PS+SK have been materialized.
    pub async fn delete_pending_timeline_init(
        &self,
        branch_id: BranchId,
    ) -> anyhow::Result<()> {
        self.dispatch::<(), ()>(
            Method::DELETE,
            format!("control/v1/branch/{branch_id}/pending_init"),
            None,
        )
        .await
    }

    /// Drop a branch: delete all of the branch's endpoint records, discard
    /// any un-materialized init recipe, and mark the branch dropped. The
    /// caller (`yb_light_local timeline drop`) stops the compute processes
    /// first. Idempotent.
    pub async fn timeline_drop(
        &self,
        branch_id: BranchId,
    ) -> anyhow::Result<()> {
        self.dispatch::<(), ()>(
            Method::PUT,
            format!("control/v1/branch/{branch_id}/drop"),
            None,
        )
        .await
    }

    /// Persist a compute endpoint record. Backs `ComputeControlPlane::new_endpoint`,
    /// replacing the old `endpoint.json` file write.
    #[instrument(skip(self, req), fields(endpoint_id = %req.endpoint_id))]
    pub async fn endpoint_create(&self, req: ComputeEndpoint) -> anyhow::Result<()> {
        self.dispatch::<_, ()>(Method::POST, "control/v1/endpoint".to_string(), Some(req))
            .await
    }

    /// List all persisted compute endpoint records. Backs
    /// `ComputeControlPlane::load`, replacing the old `read_dir(endpoints/)` scan.
    pub async fn endpoint_list(&self) -> anyhow::Result<Vec<ComputeEndpoint>> {
        self.dispatch::<(), Vec<ComputeEndpoint>>(
            Method::GET,
            "control/v1/endpoint".to_string(),
            None,
        )
        .await
    }

    /// Delete a persisted compute endpoint record. Backs the `destroy` path of
    /// `Endpoint::stop`, replacing the old `remove_dir_all(endpoint dir)`.
    #[instrument(skip(self))]
    pub async fn endpoint_delete(&self, endpoint_id: &str) -> anyhow::Result<()> {
        self.dispatch::<(), ()>(
            Method::DELETE,
            format!("control/v1/endpoint/{endpoint_id}"),
            None,
        )
        .await
    }
}
