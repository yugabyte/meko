//! `yb-endpoint-supervisor` — PID 1 of the endpoint pod.
//!
//! An endpoint pod runs three processes: the safekeeper, the
//! pageserver, and `compute_ctl` with Postgres. This binary renders their config
//! and supervises them.
//!
//! # Modules
//!
//! - [`resources`] — what the pod has: the container's cgroup CPU and memory
//!   limits, and the size of its data volume. Every derived setting is sized from
//!   these, and an input that cannot be determined stays `None` so the caller
//!   leaves the setting alone rather than guessing.
//! - [`pg_settings`] — the Postgres GUCs derived from those resources.
//!   `shared_buffers` takes half the memory; the LFC is bounded by the data
//!   volume. Every other knob is a postmaster-context ceiling plus a live value
//!   beneath it, so the live value can be retuned with a reload.
//! - [`config`] — rendering `pageserver.toml` and `safekeeper.toml`, with
//!   operator overrides merged in. Keys are merged by name, so a key cannot be
//!   emitted twice, and each document is type-checked against the struct its
//!   daemon parses.
//! - [`registry`] — the operator overrides for this endpoint, read out of
//!   `amp_registry`. A failed read is a warning, not a boot failure.
//! - [`retry`] — bounded retry with backoff. Because an error out of this process
//!   is a pod restart, a dependency that is briefly unavailable is retried here
//!   rather than escalated.
//! - [`supervise`] — the PID 1 behaviour: ordered, health-gated startup, restart
//!   with a rolling budget, and reverse-order shutdown.
//! - [`boot`] — the shape of the pod: ports, paths, the base config keys, the
//!   three services, and waiting for YugabyteDB before starting them.
//! - [`spec`] — the compute spec.
//! - [`remote_auth`] — the `cloud_admin` password, without which only loopback
//!   clients can authenticate. Its success line is the readiness signal the
//!   controller's Docker deployer waits for.
//!
//! # Startup order
//!
//! The sequence in [`run`] is load-bearing, not incidental:
//!
//! 1. Signal handlers, so a pod delete during a slow startup still stops cleanly.
//! 2. Resources and the GUCs derived from them.
//! 3. **Wait for YugabyteDB.** Everything below needs it, and the two things that
//!    have their own retry budget are not sized to absorb an outage — see
//!    [`boot::YB_WAIT_TIMEOUT`].
//! 4. The registry overrides, *after* the wait, so a transient outage does not
//!    silently cost the endpoint its operator settings.
//! 5. Render and write every config file, before any child starts. A render that
//!    fails is a bad config, so it must fail here rather than crash-loop a daemon.
//! 6. Start the children in dependency order, then supervise them.

use std::collections::BTreeMap;

use anyhow::{Context, Result};
use camino::{Utf8Path, Utf8PathBuf};

pub mod boot;
pub mod config;
pub mod coordinate;
pub mod pg_settings;
pub mod registry;
pub mod remote_auth;
pub mod resources;
pub mod retry;
pub mod spec;
pub mod supervise;

use resources::{Cgroup, Resources};
use supervise::{Outcome, Policy, Supervisor};

fn env_opt(name: &str) -> Option<String> {
    std::env::var(name).ok().filter(|v| !v.is_empty())
}

fn env_or(name: &str, default: &str) -> String {
    env_opt(name).unwrap_or_else(|| default.to_string())
}

/// A port from the environment, falling back to the default the chart, the
/// daemons and the compute all agree on. A malformed value is an error rather
/// than a silent fallback: booting on a different port than the operator asked
/// for produces a pod that looks healthy and is unreachable.
fn env_port(name: &str, default: u16) -> Result<u16> {
    match env_opt(name) {
        None => Ok(default),
        Some(v) => v
            .parse()
            .with_context(|| format!("{name}={v:?} is not a port number")),
    }
}

/// A boolean the chart may render as `true`/`false`; anything else is false, so a
/// blank ConfigMap value means "off" rather than a boot failure.
fn env_bool(name: &str) -> bool {
    env_opt(name).is_some_and(|v| matches!(v.trim().to_ascii_lowercase().as_str(), "true" | "1"))
}

/// An optional count of seconds. A malformed value is an error: silently not
/// offloading is the kind of thing nobody notices until the cache is cold.
fn env_u64(name: &str) -> Result<Option<u64>> {
    match env_opt(name) {
        None => Ok(None),
        Some(v) => v
            .parse()
            .map(Some)
            .with_context(|| format!("{name}={v:?} is not a number of seconds")),
    }
}

/// The environment contract, as the chart's ConfigMap and Secret deliver it.
struct Env {
    layout: boot::Layout,
    yb_host_ports: String,
    yb_user: String,
    yb_dbname: String,
    yb_sslmode: Option<String>,
    yb_sslrootcert: Option<String>,
    yb_password_env: Option<String>,
    endpoint_id: Option<String>,
    compute_password: String,
    /// Load the previous LFC state on compute startup.
    autoprewarm: bool,
    /// Save the LFC state this often. `None` means never.
    offload_lfc_interval_seconds: Option<u64>,
    shared_preload_libraries: String,
    /// Extension paths and the like, computed by the chart from the ImageVolumes
    /// it mounted. Not derivable here and not in the database.
    chart_settings: BTreeMap<String, String>,
}

impl Env {
    fn read() -> Result<Self> {
        let run_dir: Utf8PathBuf = env_or("RUN_DIR", "/data").into();
        let bin_dir: Utf8PathBuf = env_or("BIN_DIR", "/usr/local/bin").into();
        let pg_distrib_dir: Utf8PathBuf = env_or("PG_DISTRIB_DIR", "/usr/local").into();

        // The controller has to reach in, so its presence is what widens the
        // daemons' listen address beyond loopback.
        let bind_host = match env_opt("CONTROLLER_URL") {
            Some(_) => "0.0.0.0",
            None => boot::LISTEN_HOST,
        };

        let branch_id = env_opt("BRANCH_ID").context("BRANCH_ID is required")?;

        let layout = boot::Layout {
            run_dir,
            bin_dir,
            pg_distrib_dir,
            branch_id,
            compute_port: env_port("COMPUTE_PORT", 55433)?,
            pageserver_pg_port: env_port("PAGESERVER_PG_PORT", 64000)?,
            pageserver_http_port: env_port("PAGESERVER_HTTP_PORT", 9898)?,
            pageserver_grpc_port: env_port("PAGESERVER_GRPC_PORT", 51051)?,
            safekeeper_pg_port: env_port("SAFEKEEPER_PG_PORT", 5454)?,
            safekeeper_http_port: env_port("SAFEKEEPER_HTTP_PORT", 7676)?,
            compute_ctl_http_port: env_port("COMPUTE_CTL_HTTP_PORT", 3080)?,
            pg_version_major: env_or("ENDPOINT_PG_VERSION", "18")
                .parse()
                .context("ENDPOINT_PG_VERSION is not a major version number")?,
            bind_host: bind_host.to_string(),
            control_plane_api: env_or("CONTROL_PLANE_API", "http://localhost:6666"),
        };

        // A malformed id means the overrides cannot be read, which is a warning
        // rather than a failure — but say so, since the pod will look fine while
        // quietly ignoring the operator's settings.
        let endpoint_id = match env_opt("ENDPOINT_ID") {
            None => None,
            Some(raw) => match registry::valid_endpoint_id(&raw) {
                Some(id) => Some(id),
                None => {
                    tracing::warn!(
                        endpoint_id = %raw,
                        "ENDPOINT_ID is not a UUID; per-endpoint overrides will not be read"
                    );
                    None
                }
            },
        };

        Ok(Self {
            layout,
            yb_host_ports: env_or("YB_HOST_PORTS", "127.0.0.1:5433"),
            yb_user: env_or("YB_USER", "yugabyte"),
            yb_dbname: env_or("YB_DBNAME", "yb_storage"),
            yb_sslmode: env_opt("YB_SSLMODE"),
            yb_sslrootcert: env_opt("YB_SSLROOTCERT"),
            yb_password_env: env_opt("YB_PASSWORD_ENV"),
            endpoint_id,
            compute_password: env_or("COMPUTE_PASSWORD", "cloud_admin"),
            autoprewarm: env_bool("AUTOPREWARM"),
            offload_lfc_interval_seconds: env_u64("OFFLOAD_LFC_INTERVAL_SECONDS")?,
            shared_preload_libraries: env_or("SHARED_PRELOAD_LIBRARIES", "yb_amp"),
            chart_settings: chart_settings()?,
        })
    }

    fn yb_conn(&self) -> toml_edit::Value {
        boot::yb_conn_inline(
            &self.yb_host_ports,
            &self.yb_user,
            &self.yb_dbname,
            self.yb_sslmode.as_deref(),
            self.yb_sslrootcert.as_deref(),
            self.yb_password_env.as_deref(),
        )
    }

    /// The same connection as a JSON object, which is the shape
    /// `compute_ctl` parses: `remote_storage::TypedRemoteStorageKind` is an
    /// externally tagged enum (`#[serde(tag = "type")]`), so the discriminator is
    /// a sibling of the connection fields, not a wrapper around them.
    fn yb_conn_typed_json(&self) -> serde_json::Value {
        let mut map = serde_json::Map::new();
        map.insert("type".into(), "YbStore".into());
        map.insert("host_ports".into(), self.yb_host_ports.clone().into());
        map.insert("user".into(), self.yb_user.clone().into());
        map.insert("dbname".into(), self.yb_dbname.clone().into());
        for (k, v) in [
            ("password_env", &self.yb_password_env),
            ("sslmode", &self.yb_sslmode),
            ("sslrootcert", &self.yb_sslrootcert),
        ] {
            if let Some(v) = v {
                map.insert(k.into(), v.clone().into());
            }
        }
        serde_json::Value::Object(map)
    }

    /// Host and port for the readiness wait, taken from the first entry of the
    /// comma-separated `host:port` list.
    fn yb_probe_target(&self) -> Result<(String, u16)> {
        let first = self
            .yb_host_ports
            .split(',')
            .next()
            .unwrap_or_default()
            .trim();
        let (host, port) = first
            .rsplit_once(':')
            .with_context(|| format!("YB_HOST_PORTS entry {first:?} is not host:port"))?;
        let port = port
            .parse()
            .with_context(|| format!("YB_HOST_PORTS entry {first:?} has a bad port"))?;
        Ok((host.to_string(), port))
    }
}

/// `COMPUTE_EXTRA_SETTINGS_JSON`, a flat `{name: value}` map the chart renders.
fn chart_settings() -> Result<BTreeMap<String, String>> {
    let raw = env_or("COMPUTE_EXTRA_SETTINGS_JSON", "{}");
    let parsed: BTreeMap<String, serde_json::Value> =
        serde_json::from_str(&raw).context("COMPUTE_EXTRA_SETTINGS_JSON is not a JSON object")?;
    Ok(parsed
        .into_iter()
        .map(|(k, v)| {
            let s = match v {
                serde_json::Value::String(s) => s,
                other => other.to_string(),
            };
            (k, s)
        })
        .collect())
}

fn main() -> std::process::ExitCode {
    init_logging();
    match run() {
        Ok(code) => code,
        Err(e) => {
            // `{e:#}` so the whole context chain lands in the pod's log; this is
            // the only place a startup failure is explained.
            tracing::error!("supervisor failed: {e:#}");
            std::process::ExitCode::FAILURE
        }
    }
}

fn run() -> Result<std::process::ExitCode> {
    // First, so a delete arriving mid-startup is honoured rather than killing this
    // process with its children already running.
    supervise::install_signal_handlers()?;

    let env = Env::read()?;
    let l = &env.layout;

    let cgroup_root: Utf8PathBuf = env_or("CGROUP_ROOT", "/sys/fs/cgroup").into();
    let res = Resources::resolve(
        &Cgroup::new(cgroup_root),
        env_opt("ENDPOINT_CPU").as_deref(),
        env_opt("ENDPOINT_MEMORY").as_deref(),
        env_opt("ENDPOINT_DATA_VOLUME_SIZE").as_deref(),
        resources::statvfs_total_bytes(&l.run_dir),
    );
    tracing::info!(
        memory_bytes = res.memory_bytes,
        vcpu = res.vcpu,
        data_volume_bytes = res.data_volume_bytes,
        "resolved endpoint resources"
    );

    let (settings, skipped) = pg_settings::derive(&res);
    for s in &settings {
        tracing::info!(name = %s.name, value = %s.value, "derived");
    }
    if skipped.memory_unknown {
        tracing::warn!("endpoint memory unknown; shared_buffers left at the Postgres default");
    }
    if skipped.cpu_unknown {
        tracing::warn!("endpoint CPU unknown; parallel-worker GUCs left at their defaults");
    }
    if skipped.volume_unknown {
        tracing::warn!("data volume budget unknown; the LFC stays disabled");
    }

    // The daemons and the compute load the bundled Postgres libraries.
    prepend_ld_library_path(&l.pg_lib());

    for dir in [l.run_dir.clone(), l.ps_datadir(), l.sk_datadir()] {
        std::fs::create_dir_all(&dir).with_context(|| format!("create {dir}"))?;
    }

    // Before the registry read and before any child: everything below needs YB,
    // and neither the read's retry budget nor the restart budget is sized to
    // absorb an outage.
    let (yb_host, yb_port) = env.yb_probe_target()?;
    boot::wait_for_yb(
        &yb_host,
        yb_port,
        boot::YB_WAIT_TIMEOUT,
        boot::YB_WAIT_INTERVAL,
    );
    if supervise::shutdown_requested() {
        tracing::info!("shutdown requested during startup; nothing was started");
        return Ok(std::process::ExitCode::SUCCESS);
    }

    let overrides = fetch_overrides(&env);

    // Every config file is written before anything starts: a render failure is a
    // bad config, and failing here beats crash-looping a daemon against it.
    let scale = res
        .vcpu
        .map(|v| ("scale_factor", toml_edit::Value::from(v)));
    if scale.is_none() {
        tracing::warn!("endpoint CPU unknown; leaving the daemons' scale_factor at their default");
    }

    let mut ps_base = boot::pageserver_base(l, env.yb_conn());
    ps_base.extend(scale.clone());
    let ps_toml = config::render(config::Daemon::Pageserver, ps_base, &overrides.pageserver)
        .context("rendering pageserver.toml")?;
    write(&l.ps_datadir().join("pageserver.toml"), &ps_toml)?;

    // The safekeeper's yb_storage_conn is a string-encoded inline table rather
    // than a table, which is the shape its config type expects.
    let mut sk_base = boot::safekeeper_base(l, env.yb_conn().to_string());
    sk_base.extend(scale);
    let sk_toml = config::render(config::Daemon::Safekeeper, sk_base, &overrides.safekeeper)
        .context("rendering safekeeper.toml")?;
    write(&l.sk_datadir().join("safekeeper.toml"), &sk_toml)?;

    let compute_spec = spec::render_compute_spec(
        &spec::ComputeSpecParams {
            compute_port: l.compute_port,
            branch_id: &l.branch_id,
            safekeeper_addr: &l.safekeeper_addr(),
            pageserver_connstring: &l.pageserver_connstring(),
            shared_preload_libraries: &env.shared_preload_libraries,
            cloud_admin_password_md5: &spec::md5_role_password(
                &env.compute_password,
                "cloud_admin",
            ),
            endpoint_id: env.endpoint_id.as_deref(),
            yb_storage_conn: env.yb_conn_typed_json(),
            autoprewarm: env.autoprewarm,
            offload_lfc_interval_seconds: env.offload_lfc_interval_seconds,
        },
        &settings,
        &env.chart_settings,
        &overrides.pg,
    );
    write(
        &l.run_dir.join("compute_spec.json"),
        &serde_json::to_string(&compute_spec)?,
    )?;

    // The compute starts last, after the pageserver/safekeeper handshake: the
    // safekeeper only learns about the branch over its HTTP API, and the LSN to
    // create it at comes from the pageserver once its branch is Active.
    let mut services = boot::services(l);
    let compute = services
        .pop()
        .context("boot::services must include a compute")?;
    anyhow::ensure!(
        compute.name == "compute_ctl",
        "expected compute_ctl last in boot::services, got {}",
        compute.name
    );

    let mut sup = Supervisor::new(Policy::from_env());
    for svc in services {
        sup.add(svc);
    }
    sup.start_all()?;

    // Wrapped so a failure — or a SIGTERM landing mid-handshake — stops the two
    // daemons that are already up. Returning straight out would leave them running
    // against datadirs nobody is closing, which is the situation the signal
    // handlers exist to prevent, and `Supervisor` does not reap on drop.
    let handshake = (|| -> Result<String> {
        let start_lsn = coordinate::wait_branch_active(
            boot::LISTEN_HOST,
            l.pageserver_http_port,
            &l.branch_id,
            coordinate::BRANCH_ACTIVE_TIMEOUT,
            coordinate::BRANCH_ACTIVE_INTERVAL,
        )?;
        coordinate::create_safekeeper_branch(
            boot::LISTEN_HOST,
            l.safekeeper_http_port,
            &l.branch_id,
            l.pg_version_major,
            &start_lsn,
        )?;
        Ok(start_lsn)
    })();
    if let Err(e) = handshake {
        sup.stop_all();
        return Err(e).context("the pageserver/safekeeper handshake");
    }

    sup.add(compute);
    // Skips the two that are already running.
    sup.start_all()?;

    // On its own thread: this waits for Postgres to answer queries, and blocking
    // here would stop the monitor loop from restarting a sibling that crashed
    // meanwhile. The controller's Docker deployer treats the line it logs as the
    // endpoint's readiness signal — see remote_auth::READY_MARKER.
    remote_auth::spawn(
        &l.pg_bin().join("psql"),
        l.compute_port,
        &env.compute_password,
    );

    tracing::info!("all services are up");

    match sup.run(std::time::Duration::from_millis(500))? {
        Outcome::Shutdown => {
            tracing::info!("stopped on signal");
            Ok(std::process::ExitCode::SUCCESS)
        }
        // Non-zero so the kubelet recreates the pod: a service that cannot stay
        // up is not something this process can fix by restarting it again.
        Outcome::BudgetExceeded { service, restarts } => {
            tracing::error!(service, restarts, "exiting so the pod is recreated");
            Ok(std::process::ExitCode::FAILURE)
        }
    }
}

/// Reads the operator overrides. A failure is a warning: an endpoint that cannot
/// reach the registry must still start on its derived defaults, since refusing to
/// boot because an override *might* exist would turn a registry blip into an
/// outage.
fn fetch_overrides(env: &Env) -> registry::Overrides {
    let Some(id) = &env.endpoint_id else {
        tracing::info!("no ENDPOINT_ID; running on derived defaults");
        return registry::Overrides::default();
    };
    let Ok((host, port)) = env.yb_probe_target() else {
        return registry::Overrides::default();
    };
    let target = registry::PsqlTarget {
        psql: env.layout.pg_bin().join("psql"),
        host,
        port: port.to_string(),
        user: env.yb_user.clone(),
        dbname: env.yb_dbname.clone(),
        password_env: env.yb_password_env.clone(),
    };
    match registry::fetch(&target, id) {
        Ok(Some(o)) => {
            tracing::info!(
                pg = o.pg.len(),
                pageserver = o.pageserver.len(),
                safekeeper = o.safekeeper.len(),
                "read overrides from amp_registry"
            );
            o
        }
        Ok(None) => {
            tracing::info!(endpoint_id = %id, "no registry row; running on derived defaults");
            registry::Overrides::default()
        }
        Err(e) => {
            tracing::warn!("could not read overrides, running on derived defaults: {e:#}");
            registry::Overrides::default()
        }
    }
}

fn write(path: &Utf8Path, contents: &str) -> Result<()> {
    std::fs::write(path, contents).with_context(|| format!("write {path}"))?;
    tracing::info!(%path, bytes = contents.len(), "wrote");
    Ok(())
}

/// Puts `dir` ahead of any inherited `LD_LIBRARY_PATH`, which the children
/// inherit in turn.
fn prepend_ld_library_path(dir: &Utf8Path) {
    let existing = std::env::var("LD_LIBRARY_PATH").unwrap_or_default();
    let joined = if existing.is_empty() {
        dir.to_string()
    } else {
        format!("{dir}:{existing}")
    };
    // SAFETY: single-threaded at this point — no children are spawned and the
    // tracing subscriber does not read the environment.
    unsafe { std::env::set_var("LD_LIBRARY_PATH", joined) };
}

/// Registers a stdout subscriber, without which every `tracing::` call in this
/// binary is silently dropped.
///
/// `RUST_LOG` is honoured so a pod can be turned up to debug without a rebuild;
/// the default is `info`, matching what the other daemons log at.
fn init_logging() {
    let filter = tracing_subscriber::EnvFilter::try_from_default_env()
        .unwrap_or_else(|_| tracing_subscriber::EnvFilter::new("info"));
    tracing_subscriber::fmt()
        .with_env_filter(filter)
        .with_writer(std::io::stdout)
        .init();
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_port_falls_back_to_its_default_and_rejects_junk() {
        // Not set: the default the chart and the daemons agree on.
        assert_eq!(env_port("SUPERVISOR_TEST_ABSENT_PORT", 9898).unwrap(), 9898);
        // A bad value must fail rather than silently bind somewhere else.
        // SAFETY: single-threaded test, and the name is unique to this test.
        unsafe { std::env::set_var("SUPERVISOR_TEST_BAD_PORT", "not-a-port") };
        assert!(env_port("SUPERVISOR_TEST_BAD_PORT", 9898).is_err());
        unsafe { std::env::remove_var("SUPERVISOR_TEST_BAD_PORT") };
    }

    /// The wait needs one host:port, but the connection string may carry several.
    #[test]
    fn the_probe_target_is_the_first_host_port() {
        let mut env = test_env();
        env.yb_host_ports = "a.example:5433,b.example:5434".into();
        assert_eq!(
            env.yb_probe_target().unwrap(),
            ("a.example".to_string(), 5433)
        );
        // IPv6 needs the *last* colon to be the separator.
        env.yb_host_ports = "[::1]:5433".into();
        assert_eq!(env.yb_probe_target().unwrap(), ("[::1]".to_string(), 5433));

        env.yb_host_ports = "no-port".into();
        assert!(env.yb_probe_target().is_err());
    }

    /// The chart's settings arrive as JSON, and a number must survive as the
    /// string a GUC takes rather than becoming `Some(5)` or being dropped.
    #[test]
    fn chart_settings_stringify_scalars() {
        // SAFETY: single-threaded test.
        unsafe {
            std::env::set_var(
                "COMPUTE_EXTRA_SETTINGS_JSON",
                r#"{"a":"one","b":2,"c":true}"#,
            )
        };
        let got = chart_settings().unwrap();
        unsafe { std::env::remove_var("COMPUTE_EXTRA_SETTINGS_JSON") };
        assert_eq!(got.get("a").unwrap(), "one");
        assert_eq!(got.get("b").unwrap(), "2");
        assert_eq!(got.get("c").unwrap(), "true");
    }

    #[test]
    fn malformed_chart_settings_are_an_error_not_an_empty_map() {
        // SAFETY: single-threaded test.
        unsafe { std::env::set_var("COMPUTE_EXTRA_SETTINGS_JSON", "not json") };
        let got = chart_settings();
        unsafe { std::env::remove_var("COMPUTE_EXTRA_SETTINGS_JSON") };
        assert!(got.is_err());
    }

    #[test]
    fn ld_library_path_keeps_what_it_inherited_behind_the_bundled_lib() {
        // SAFETY: single-threaded test.
        unsafe { std::env::set_var("LD_LIBRARY_PATH", "/inherited") };
        prepend_ld_library_path(Utf8Path::new("/usr/local/lib"));
        assert_eq!(
            std::env::var("LD_LIBRARY_PATH").unwrap(),
            "/usr/local/lib:/inherited"
        );
        unsafe { std::env::remove_var("LD_LIBRARY_PATH") };
    }

    fn test_env() -> Env {
        Env {
            layout: boot::Layout {
                run_dir: "/data".into(),
                bin_dir: "/usr/local/bin".into(),
                pg_distrib_dir: "/usr/local".into(),
                branch_id: "9a054903242f46bca0c9cc6b91a37e4c".into(),
                compute_port: 55433,
                pageserver_pg_port: 64000,
                pageserver_http_port: 9898,
                pageserver_grpc_port: 51051,
                safekeeper_pg_port: 5454,
                safekeeper_http_port: 7676,
                compute_ctl_http_port: 3080,
                pg_version_major: 18,
                bind_host: "127.0.0.1".into(),
                control_plane_api: "http://localhost:6666".into(),
            },
            yb_host_ports: "127.0.0.1:5433".into(),
            yb_user: "yugabyte".into(),
            yb_dbname: "yb_storage".into(),
            yb_sslmode: None,
            yb_sslrootcert: None,
            yb_password_env: None,
            endpoint_id: None,
            compute_password: "cloud_admin".into(),
            autoprewarm: false,
            offload_lfc_interval_seconds: None,
            shared_preload_libraries: "yb_amp".into(),
            chart_settings: BTreeMap::new(),
        }
    }

    /// Without an endpoint id there is nothing to look up, and the supervisor
    /// must run on derived defaults rather than treating it as a failure.
    #[test]
    fn no_endpoint_id_yields_empty_overrides() {
        let got = fetch_overrides(&test_env());
        assert_eq!(got, registry::Overrides::default());
    }

    /// `compute_ctl` parses `yb_storage_conn` as `TypedRemoteStorageKind`, an
    /// externally tagged enum. Deserializing into the real type is the only way to
    /// know the shape is right — a JSON string and a nested object both look
    /// plausible and neither one loads.
    #[test]
    fn the_specs_storage_connection_deserializes_as_its_own_type() {
        let mut env = test_env();
        env.yb_sslmode = Some("require".into());
        env.yb_password_env = Some("YB_PASSWORD".into());
        let conn = env.yb_conn_typed_json();
        let json = serde_json::to_string(&conn).unwrap();
        serde_json::from_str::<remote_storage::TypedRemoteStorageKind>(&json)
            .unwrap_or_else(|e| panic!("compute_ctl could not parse {json}: {e}"));
    }

    /// The optional connection fields are omitted rather than emitted as null,
    /// which `YbConnConfig` would reject.
    #[test]
    fn absent_ssl_fields_are_omitted_from_the_connection() {
        let s = test_env().yb_conn_typed_json().to_string();
        assert!(s.contains(r#""type":"YbStore""#), "{s}");
        assert!(!s.contains("sslmode"), "{s}");
        assert!(!s.contains("null"), "{s}");
    }
}
