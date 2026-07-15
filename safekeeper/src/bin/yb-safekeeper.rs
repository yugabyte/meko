//
// Main entry point for the safekeeper executable
//
use std::sync::Arc;
use std::time::{Duration, Instant};

use anyhow::{Context, Result};
use camino::Utf8PathBuf;
use clap::Parser;
use futures::future::BoxFuture;
use futures::stream::FuturesUnordered;
use futures::{FutureExt, StreamExt};
use http_utils::tls_certs::ReloadingCertificateResolver;
use metrics::set_build_info_metric;
use safekeeper::yb_wal_storage::YbWalConn;
use safekeeper::{
    BACKGROUND_RUNTIME, GlobalTimelines, HTTP_RUNTIME, SafeKeeperConf, WAL_SERVICE_RUNTIME, http,
    wal_service,
};
use safekeeper_api::config::SafekeeperConfigToml;
use sd_notify::NotifyState;
use tokio::runtime::Handle;
use tokio::signal::unix::{SignalKind, signal};
use tokio::task::JoinError;
use tracing::*;
use utils::auth::{JwtAuth, Scope, SwappableJwtAuth};
use utils::logging::{self, LogFormat, SecretString};
use utils::metrics_collector::{METRICS_COLLECTION_INTERVAL, METRICS_COLLECTOR};
use utils::{pid_file, project_build_tag, project_git_version, tcp_listener};

use safekeeper::disk_usage::{
    GLOBAL_DISK_LIMIT_EXCEEDED, get_filesystem_capacity, get_filesystem_usage,
};
use safekeeper::metrics::GLOBAL_DISK_UTIL_CHECK_SECONDS;
use std::sync::atomic::Ordering;

#[global_allocator]
static GLOBAL: tikv_jemallocator::Jemalloc = tikv_jemallocator::Jemalloc;

/// Configure jemalloc — same tuning as `yb-pageserver`:
///
/// - `prof:true,prof_active:true,lg_prof_sample:21` — heap profile,
///   sample every 2 MiB (~3% allocator overhead).
/// - `background_thread:true` — spawn jemalloc's purge thread so
///   dirty/muzzy decay runs even when the application is quiescent.
///   Default-off; safekeeper does mostly bounded buffering but during
///   long quiet stretches between WAL bursts it still benefits from
///   returning arena slack to the OS.
/// - `dirty_decay_ms:1000`, `muzzy_decay_ms:0` — aggressive decay so
///   RSS tracks load instead of high-water-marking after each spike.
#[allow(non_upper_case_globals)]
#[unsafe(export_name = "malloc_conf")]
pub static malloc_conf: &[u8] =
    b"prof:true,prof_active:true,lg_prof_sample:21,background_thread:true,dirty_decay_ms:1000,muzzy_decay_ms:0\0";

const PID_FILE_NAME: &str = "safekeeper.pid";

project_git_version!(GIT_VERSION);
project_build_tag!(BUILD_TAG);

const FEATURES: &[&str] = &[
    #[cfg(feature = "testing")]
    "testing",
];

fn version() -> String {
    format!(
        "{GIT_VERSION} failpoints: {}, features: {:?}",
        fail::has_failpoints(),
        FEATURES,
    )
}

const ABOUT: &str = r#"
A fleet of safekeepers is responsible for reliably storing WAL received from
compute, passing it through consensus (mitigating potential computes brain
split), and serving the hardened part further downstream to pageserver(s).
"#;

#[derive(Parser)]
#[command(name = "Neon safekeeper", version = GIT_VERSION, about = ABOUT, long_about = None)]
struct Args {
    /// Path to the safekeeper data directory. Holds `safekeeper.toml` (all
    /// other configuration), logs, and the pidfile.
    #[arg(short = 'D', long, default_value = "./")]
    datadir: Utf8PathBuf,
    /// Initialize and exit (legacy no-op probe; configuration now lives in
    /// `safekeeper.toml`).
    #[arg(long)]
    init: bool,
    /// Dump control file at path specified by this argument and exit.
    #[arg(long)]
    dump_control_file: Option<Utf8PathBuf>,
}

#[tokio::main(flavor = "current_thread")]
async fn main() -> anyhow::Result<()> {
    let args = <Args as clap::FromArgMatches>::from_arg_matches(
        &<Args as clap::CommandFactory>::command()
            .version(version())
            .get_matches(),
    )?;

    // Suspended-task dump on SIGUSR2 (`kill -USR2 <pid>`). See `utils::tokio_dump`.
    // Register EVERY runtime: the WAL acceptors (the usual suspects in a
    // wedge — they take the timeline shared_state write lock) live on
    // WAL_SERVICE_RUNTIME, not the main runtime, and a dump that misses
    // them shows a blocked reader with an invisible holder.
    utils::tokio_dump::spawn_sigusr2_dumper(vec![
        ("safekeeper".to_string(), tokio::runtime::Handle::current()),
        (
            "wal_service".to_string(),
            WAL_SERVICE_RUNTIME.handle().clone(),
        ),
        ("http".to_string(), HTTP_RUNTIME.handle().clone()),
        (
            "background".to_string(),
            BACKGROUND_RUNTIME.handle().clone(),
        ),
    ]);

    if let Some(addr) = args.dump_control_file {
        anyhow::bail!(
            "--dump-control-file is unsupported: local-FS control files were removed. \
             Read the blob from remote storage at safekeeper-control/<branch_id> \
             (input was {addr})"
        );
    }

    let workdir = args.datadir.canonicalize_utf8().with_context(|| {
        format!(
            "Failed to get the absolute path for input workdir {:?}",
            args.datadir
        )
    })?;

    // All real configuration lives in `<workdir>/safekeeper.toml`, written by
    // the launcher. On `--init` the file may not exist yet (it's a legacy
    // no-op probe), so fall back to defaults just to log and exit.
    let config_path = workdir.join("safekeeper.toml");
    let cfg = match std::fs::read_to_string(&config_path) {
        Ok(s) => SafekeeperConfigToml::from_toml_str(&s)
            .with_context(|| format!("parse {config_path}"))?,
        Err(e) if e.kind() == std::io::ErrorKind::NotFound && args.init => {
            SafekeeperConfigToml::default()
        }
        Err(e) => return Err(anyhow::Error::from(e).context(format!("read {config_path}"))),
    };

    // important to keep the order of:
    // 1. init logging
    // 2. tracing panic hook
    // 3. sentry
    logging::init(
        LogFormat::from_config(&cfg.log_format)?,
        logging::TracingErrorLayerEnablement::Disabled,
        logging::Output::Stdout,
    )?;
    logging::replace_panic_hook_with_tracing_panic_hook().forget();
    info!("version: {GIT_VERSION}");
    info!("buld_tag: {BUILD_TAG}");

    // Change into the data directory.
    std::env::set_current_dir(&workdir)?;

    // Prevent running multiple safekeepers on the same directory
    let lock_file_path = workdir.join(PID_FILE_NAME);
    let lock_file =
        pid_file::claim_for_current_process(&lock_file_path).context("claim pid file")?;
    info!("claimed pid file at {lock_file_path:?}");
    // ensure that the lock file is held even if the main thread of the process is panics
    // we need to release the lock file only when the current process is gone
    std::mem::forget(lock_file);

    let id = cfg.id;
    info!("safekeeper ID {}", id);
    if args.init {
        return Ok(());
    }

    // An empty path string means "auth disabled" for that surface.
    let key_path = |p: &Option<String>| {
        p.as_deref()
            .filter(|s| !s.is_empty())
            .map(Utf8PathBuf::from)
    };
    let load_jwt = |path: &Utf8PathBuf| -> anyhow::Result<JwtAuth> {
        JwtAuth::from_key_path(path).context("failed to load the auth key")
    };

    let pg_auth = match key_path(&cfg.pg_auth_public_key_path) {
        None => {
            info!("pg auth is disabled");
            None
        }
        Some(path) => {
            info!("loading pg auth JWT key from {path}");
            Some(Arc::new(load_jwt(&path)?))
        }
    };
    let pg_tenant_only_auth = match key_path(&cfg.pg_tenant_only_auth_public_key_path) {
        None => {
            info!("pg tenant only auth is disabled");
            None
        }
        Some(path) => {
            info!("loading pg tenant only auth JWT key from {path}");
            Some(Arc::new(load_jwt(&path)?))
        }
    };
    let http_auth = match key_path(&cfg.http_auth_public_key_path) {
        None => {
            info!("http auth is disabled");
            None
        }
        Some(path) => {
            info!("loading http auth JWT key(s) from {path}");
            Some(Arc::new(SwappableJwtAuth::new(load_jwt(&path)?)))
        }
    };

    // Load JWT auth token to connect to other safekeepers for pull_timeline.
    let sk_auth_token = if let Some(auth_token_path) = key_path(&cfg.auth_token_path) {
        info!("loading JWT token for authentication with safekeepers from {auth_token_path}");
        let auth_token = tokio::fs::read_to_string(&auth_token_path).await?;
        Some(SecretString::from(auth_token.trim().to_owned()))
    } else {
        info!("no JWT token for authentication with safekeepers detected");
        None
    };

    let ssl_ca_certs = match key_path(&cfg.ssl_ca_file) {
        Some(ssl_ca_file) => {
            tracing::info!("Using ssl root CA file: {ssl_ca_file:?}");
            let buf = tokio::fs::read(&ssl_ca_file).await?;
            pem::parse_many(&buf)?
                .into_iter()
                .filter(|pem| pem.tag() == "CERTIFICATE")
                .collect()
        }
        None => Vec::new(),
    };

    let yb_storage_conn = match cfg.yb_storage_conn.as_deref() {
        Some(s) => {
            Some(parse_yb_conn_config(s).context("parse yb_storage_conn from safekeeper.toml")?)
        }
        None => None,
    };

    let conf = Arc::new(SafeKeeperConf {
        workdir,
        my_id: id,
        listen_pg_addr: cfg.listen_pg,
        listen_pg_addr_tenant_only: cfg.listen_pg_tenant_only,
        listen_http_addr: cfg.listen_http,
        listen_https_addr: cfg.listen_https,
        advertise_pg_addr: cfg.advertise_pg,
        availability_zone: cfg.availability_zone,
        heartbeat_timeout: cfg.heartbeat_timeout,
        yb_storage_conn,
        branch_id: cfg.branch_id,
        wal_cache_size_mib: cfg.wal_cache_size_mib.unwrap_or_else(|| {
            safekeeper::scale_count(
                safekeeper::defaults::DEFAULT_WAL_CACHE_SIZE_MIB,
                cfg.scale_factor,
            )
        }),
        max_timeline_disk_usage_bytes: cfg.max_timeline_disk_usage_bytes,
        pg_auth,
        pg_tenant_only_auth,
        http_auth,
        sk_auth_token,
        current_thread_runtime: cfg.current_thread_runtime,
        control_file_save_interval: cfg.control_file_save_interval,
        wal_reader_fanout: cfg.wal_reader_fanout,
        max_delta_for_fanout: cfg.max_delta_for_fanout,
        ssl_key_file: Utf8PathBuf::from(cfg.ssl_key_file),
        ssl_cert_file: Utf8PathBuf::from(cfg.ssl_cert_file),
        ssl_cert_reload_period: cfg.ssl_cert_reload_period,
        ssl_ca_certs,
        use_https_safekeeper_api: cfg.use_https_safekeeper_api,
        enable_tls_wal_service_api: cfg.enable_tls_wal_service_api,
        force_metric_collection_on_scrape: cfg.force_metric_collection_on_scrape,
        global_disk_check_interval: cfg.global_disk_check_interval,
        max_global_disk_usage_ratio: cfg.max_global_disk_usage_ratio,
    });

    start_safekeeper(conf).await
}

/// Result of joining any of main tasks: upper error means task failed to
/// complete, e.g. panicked, inner is error produced by task itself.
type JoinTaskRes = Result<anyhow::Result<()>, JoinError>;

async fn start_safekeeper(conf: Arc<SafeKeeperConf>) -> Result<()> {
    info!("starting safekeeper WAL service on {}", conf.listen_pg_addr);
    let pg_listener = tcp_listener::bind(conf.listen_pg_addr.clone()).map_err(|e| {
        error!("failed to bind to address {}: {}", conf.listen_pg_addr, e);
        e
    })?;

    let pg_listener_tenant_only =
        if let Some(listen_pg_addr_tenant_only) = &conf.listen_pg_addr_tenant_only {
            info!(
                "starting safekeeper tenant scoped WAL service on {}",
                listen_pg_addr_tenant_only
            );
            let listener = tcp_listener::bind(listen_pg_addr_tenant_only.clone()).map_err(|e| {
                error!(
                    "failed to bind to address {}: {}",
                    listen_pg_addr_tenant_only, e
                );
                e
            })?;
            Some(listener)
        } else {
            None
        };

    info!(
        "starting safekeeper HTTP service on {}",
        conf.listen_http_addr
    );
    let http_listener = tcp_listener::bind(conf.listen_http_addr.clone()).map_err(|e| {
        error!("failed to bind to address {}: {}", conf.listen_http_addr, e);
        e
    })?;

    let https_listener = match conf.listen_https_addr.as_ref() {
        Some(listen_https_addr) => {
            info!("starting safekeeper HTTPS service on {}", listen_https_addr);
            Some(tcp_listener::bind(listen_https_addr).map_err(|e| {
                error!("failed to bind to address {}: {}", listen_https_addr, e);
                e
            })?)
        }
        None => None,
    };

    let yb_wal = YbWalConn::from_conf(&conf).await?;

    let global_timelines = Arc::new(GlobalTimelines::new(conf.clone(), yb_wal.clone()));

    // Register metrics collector for active timelines. It's important to do this
    // after daemonizing, otherwise process collector will be upset.
    let timeline_collector = safekeeper::metrics::TimelineCollector::new(global_timelines.clone());
    metrics::register_internal(Box::new(timeline_collector))?;

    // Keep handles to main tasks to die if any of them disappears.
    let mut tasks_handles: FuturesUnordered<BoxFuture<(String, JoinTaskRes)>> =
        FuturesUnordered::new();

    // Start wal backup launcher before loading timelines as we'll notify it
    // through the channel about timelines which need offloading, not draining
    // the channel would cause deadlock.
    let current_thread_rt = conf
        .current_thread_runtime
        .then(|| Handle::try_current().expect("no runtime in main"));

    // Load all timelines from disk to memory.
    global_timelines.init().await?;

    // Run everything in current thread rt, if asked.
    if conf.current_thread_runtime {
        info!("running in current thread runtime");
    }

    let tls_server_config = if conf.listen_https_addr.is_some() || conf.enable_tls_wal_service_api {
        let ssl_key_file = conf.ssl_key_file.clone();
        let ssl_cert_file = conf.ssl_cert_file.clone();
        let ssl_cert_reload_period = conf.ssl_cert_reload_period;

        // Create resolver in BACKGROUND_RUNTIME, so the background certificate reloading
        // task is run in this runtime.
        let cert_resolver = current_thread_rt
            .as_ref()
            .unwrap_or_else(|| BACKGROUND_RUNTIME.handle())
            .spawn(async move {
                ReloadingCertificateResolver::new(
                    "main",
                    &ssl_key_file,
                    &ssl_cert_file,
                    ssl_cert_reload_period,
                )
                .await
            })
            .await??;

        let config = rustls::ServerConfig::builder()
            .with_no_client_auth()
            .with_cert_resolver(cert_resolver);

        Some(Arc::new(config))
    } else {
        None
    };

    let wal_service_handle = current_thread_rt
        .as_ref()
        .unwrap_or_else(|| WAL_SERVICE_RUNTIME.handle())
        .spawn(wal_service::task_main(
            conf.clone(),
            pg_listener,
            Scope::SafekeeperData,
            conf.enable_tls_wal_service_api
                .then(|| tls_server_config.clone())
                .flatten(),
            global_timelines.clone(),
        ))
        // wrap with task name for error reporting
        .map(|res| ("WAL service main".to_owned(), res));
    tasks_handles.push(Box::pin(wal_service_handle));

    let global_timelines_ = global_timelines.clone();
    let timeline_housekeeping_handle = current_thread_rt
        .as_ref()
        .unwrap_or_else(|| WAL_SERVICE_RUNTIME.handle())
        .spawn(async move {
            const TOMBSTONE_TTL: Duration = Duration::from_secs(3600 * 24);
            loop {
                tokio::time::sleep(TOMBSTONE_TTL).await;
                global_timelines_.housekeeping(&TOMBSTONE_TTL);
            }
        })
        .map(|res| ("Timeline map housekeeping".to_owned(), res));
    tasks_handles.push(Box::pin(timeline_housekeeping_handle));

    // Spawn global disk usage watcher task, if a global disk usage limit is specified.
    let interval = conf.global_disk_check_interval;
    let data_dir = conf.workdir.clone();
    // Use the safekeeper data directory to compute filesystem capacity. This only runs once on startup, so
    // there is little point to continue if we can't have the proper protections in place.
    let fs_capacity_bytes = get_filesystem_capacity(data_dir.as_std_path())
        .expect("Failed to get filesystem capacity for data directory");
    let limit: u64 = (conf.max_global_disk_usage_ratio * fs_capacity_bytes as f64) as u64;
    if limit > 0 {
        let disk_usage_watch_handle = BACKGROUND_RUNTIME
            .handle()
            .spawn(async move {
                // Use Tokio interval to preserve fixed cadence between filesystem utilization checks
                let mut ticker = tokio::time::interval(interval);
                ticker.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Delay);

                loop {
                    ticker.tick().await;
                    let data_dir_clone = data_dir.clone();
                    let check_start = Instant::now();

                    let usage = tokio::task::spawn_blocking(move || {
                        get_filesystem_usage(data_dir_clone.as_std_path())
                    })
                    .await
                    .unwrap_or(0);

                    let elapsed = check_start.elapsed().as_secs_f64();
                    GLOBAL_DISK_UTIL_CHECK_SECONDS.observe(elapsed);
                    if usage > limit {
                        warn!(
                            "Global disk usage exceeded limit. Usage: {} bytes, limit: {} bytes",
                            usage, limit
                        );
                    }
                    GLOBAL_DISK_LIMIT_EXCEEDED.store(usage > limit, Ordering::Relaxed);
                }
            })
            .map(|res| ("Global disk usage watcher".to_string(), res));
        tasks_handles.push(Box::pin(disk_usage_watch_handle));
    }
    if let Some(pg_listener_tenant_only) = pg_listener_tenant_only {
        let wal_service_handle = current_thread_rt
            .as_ref()
            .unwrap_or_else(|| WAL_SERVICE_RUNTIME.handle())
            .spawn(wal_service::task_main(
                conf.clone(),
                pg_listener_tenant_only,
                Scope::Tenant,
                conf.enable_tls_wal_service_api
                    .then(|| tls_server_config.clone())
                    .flatten(),
                global_timelines.clone(),
            ))
            // wrap with task name for error reporting
            .map(|res| ("WAL service tenant only main".to_owned(), res));
        tasks_handles.push(Box::pin(wal_service_handle));
    }

    let http_handle = current_thread_rt
        .as_ref()
        .unwrap_or_else(|| HTTP_RUNTIME.handle())
        .spawn(http::task_main_http(
            conf.clone(),
            http_listener,
            global_timelines.clone(),
        ))
        .map(|res| ("HTTP service main".to_owned(), res));
    tasks_handles.push(Box::pin(http_handle));

    if let Some(https_listener) = https_listener {
        let https_handle = current_thread_rt
            .as_ref()
            .unwrap_or_else(|| HTTP_RUNTIME.handle())
            .spawn(http::task_main_https(
                conf.clone(),
                https_listener,
                tls_server_config.expect("tls_server_config is set earlier if https is enabled"),
                global_timelines.clone(),
            ))
            .map(|res| ("HTTPS service main".to_owned(), res));
        tasks_handles.push(Box::pin(https_handle));
    }

    if conf.force_metric_collection_on_scrape {
        let metrics_handle = current_thread_rt
            .as_ref()
            .unwrap_or_else(|| BACKGROUND_RUNTIME.handle())
            .spawn(async move {
                let mut interval: tokio::time::Interval =
                    tokio::time::interval(METRICS_COLLECTION_INTERVAL);
                loop {
                    interval.tick().await;
                    tokio::task::spawn_blocking(|| {
                        METRICS_COLLECTOR.run_once(true);
                    });
                }
            })
            .map(|res| ("broker main".to_owned(), res));
        tasks_handles.push(Box::pin(metrics_handle));
    }

    set_build_info_metric(GIT_VERSION, BUILD_TAG);

    // TODO: update tokio-stream, convert to real async Stream with
    // SignalStream, map it to obtain missing signal name, combine streams into
    // single stream we can easily sit on.
    let mut sigquit_stream = signal(SignalKind::quit())?;
    let mut sigint_stream = signal(SignalKind::interrupt())?;
    let mut sigterm_stream = signal(SignalKind::terminate())?;

    // Notify systemd that we are ready. This is important as currently loading
    // timelines takes significant time (~30s in busy regions).
    if let Err(e) = sd_notify::notify(true, &[NotifyState::Ready]) {
        warn!("systemd notify failed: {:?}", e);
    }

    tokio::select! {
        Some((task_name, res)) = tasks_handles.next()=> {
            error!("{} task failed: {:?}, exiting", task_name, res);
            std::process::exit(1);
        }
        // On any shutdown signal, log receival and exit. Additionally, handling
        // SIGQUIT prevents coredump.
        _ = sigquit_stream.recv() => info!("received SIGQUIT, terminating"),
        _ = sigint_stream.recv() => info!("received SIGINT, terminating"),
        _ = sigterm_stream.recv() => info!("received SIGTERM, terminating")

    };
    std::process::exit(0);
}

fn parse_yb_conn_config(s: &str) -> anyhow::Result<ysql_client::YbConnConfig> {
    // The shared YB connection config (host_ports, user, dbname, ...) as a
    // TOML inline table — identical to the `[*.yb]` blocks the other
    // components consume.
    Ok(utils::toml_edit_ext::deserialize_item(&s.parse()?)?)
}

#[test]
fn verify_cli() {
    use clap::CommandFactory;
    Args::command().debug_assert()
}
