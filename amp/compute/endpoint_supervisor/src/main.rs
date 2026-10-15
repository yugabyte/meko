//! `endpoint-supervisor` — PID 1 of the endpoint pod.
//!
//! An endpoint pod runs three processes: the safekeeper, the
//! pageserver, and `compute_ctl` with Postgres. This binary renders their config
//! and supervises them.
//!
//! `PRERENDERED_CONFIG` says how much of the config a caller brought; see
//! [`Prerender`]. A pod brings none. `yb_amp_local` sets `daemons`: it writes the
//! two daemon tomls, which describe per-endpoint data directories and ports it
//! found by searching for free ones, and the supervisor renders the compute spec
//! as it does everywhere else — so a local compute gets the same invariants and
//! the same `amp_registry` settings a deployed one does. `true` keeps the old
//! all-three behaviour for a caller that has not been taught otherwise.
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
//! 2. **Wait for YugabyteDB.** Everything below needs it, and the two things that
//!    have their own retry budget are not sized to absorb an outage — see
//!    [`boot::YB_WAIT_TIMEOUT`].
//! 3. [`render_config`]: the resources, the GUCs derived from them, the registry
//!    overrides (*after* the wait, so a transient outage does not silently cost
//!    the endpoint its operator settings), and every config file — all before any
//!    child starts, because a render that fails is a bad config and must fail here
//!    rather than crash-loop a daemon.
//! 4. Start the children in dependency order, then supervise them.

use std::collections::BTreeMap;

use anyhow::{Context, Result};
use camino::{Utf8Path, Utf8PathBuf};

pub mod boot;
pub mod compute_tls;
pub mod config;
pub mod control;
pub mod coordinate;
pub mod fence;
pub mod guc;
pub mod http;
pub mod identity;
pub mod lfc_proxy;
pub mod pg_settings;
pub mod pidfile;
pub mod protected_keys;
pub mod reconfigure;
pub mod registry;
pub mod remote_auth;
pub mod resources;
pub mod retry;
pub mod spec;
pub mod startup;
pub mod supervise;

use resources::{Cgroup, Resources};
use supervise::{Outcome, Policy, Supervisor};

/// What a compute preloads unless `BASE_SHARED_PRELOAD_LIBRARIES` overrides it: the
/// storage extension it cannot run without, plus `pg_stat_statements`, which feeds
/// the postgres_exporter statement collector and compute_ctl's `/insights`,
/// `pgsentinel`, which samples active session history, and `auto_explain`, which
/// logs nothing until `auto_explain.log_min_duration` is set. The first three can
/// only be loaded at postmaster start; loading auto_explain there makes its
/// settings apply on a reload, to sessions already open too.
///
/// The base list lives here, not in the chart, because a name whose library is
/// missing stops the postmaster: only the image knows which libraries it ships,
/// and the controller renders one chart against endpoint images of any version
/// (#1572).
const DEFAULT_SHARED_PRELOAD_LIBRARIES: &str = "yb_amp,pg_stat_statements,pgsentinel,auto_explain";

/// `shared_preload_libraries`: the base list (the image's own, unless
/// overridden) followed by the preload extensions the chart mounted, which
/// arrive separately so that naming one does not replace the base.
///
/// `legacy` is `SHARED_PRELOAD_LIBRARIES`, the whole list, which is all a
/// supervisor older than #1572 reads. A newer chart still sets it for those
/// images, to a list every image ships, so it is ignored whenever the chart
/// also sent `base` or `extra`. Alone, it is an older chart's full list.
fn shared_preload_libraries(
    legacy: Option<&str>,
    base: Option<&str>,
    extra: Option<&str>,
) -> String {
    let list = if base.is_none() && extra.is_none() {
        legacy.unwrap_or(DEFAULT_SHARED_PRELOAD_LIBRARIES)
    } else {
        base.unwrap_or(DEFAULT_SHARED_PRELOAD_LIBRARIES)
    };
    let mut libs: Vec<&str> = Vec::new();
    let names = list.split(',').chain(extra.unwrap_or_default().split(','));
    for name in names.map(str::trim).filter(|n| !n.is_empty()) {
        if !libs.contains(&name) {
            libs.push(name);
        }
    }
    libs.join(",")
}

pub(crate) fn env_opt(name: &str) -> Option<String> {
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
    env_bool_or(name, false)
}

/// Same, but for a knob whose safe state is *on*: an unset or blank value keeps
/// the default rather than silently disabling it. Used for the write fence,
/// where "off" means an unfenced endpoint and must be asked for explicitly.
pub fn env_bool_or(name: &str, default: bool) -> bool {
    match env_opt(name) {
        None => default,
        Some(v) => matches!(v.trim().to_ascii_lowercase().as_str(), "true" | "1"),
    }
}

/// An optional count of seconds. A malformed value is an error: silently not
/// offloading is the kind of thing nobody notices until the cache is cold.
pub(crate) fn env_u64(name: &str) -> Result<Option<u64>> {
    match env_opt(name) {
        None => Ok(None),
        Some(v) => v
            .parse()
            .map(Some)
            .with_context(|| format!("{name}={v:?} is not a number of seconds")),
    }
}

/// The environment contract, as the chart's ConfigMap and Secret deliver it.
pub struct Env {
    layout: boot::Layout,
    yb_host_ports: String,
    yb_user: String,
    yb_dbname: String,
    yb_sslmode: Option<String>,
    yb_sslrootcert: Option<String>,
    yb_sslcert: Option<String>,
    yb_sslkey: Option<String>,
    yb_password_env: Option<String>,
    /// The mounted server certificate the compute presents to clients. Both or
    /// neither: a cert with no key configures nothing, so
    /// [`Self::compute_tls`] treats a half-set pair as unset and says so.
    compute_ssl_cert_file: Option<String>,
    compute_ssl_key_file: Option<String>,
    endpoint_id: Option<String>,
    /// The verifier for the compute default user's password, as the control
    /// plane computed it. `None` — unset or empty — means the role's password is
    /// left exactly as the catalog has it; see [`remote_auth`] for why that is
    /// not the same as a default.
    compute_password: Option<String>,
    /// Load the previous LFC state on compute startup.
    autoprewarm: bool,
    /// Save the LFC state this often. `None` means never.
    offload_lfc_interval_seconds: Option<u64>,
    shared_preload_libraries: String,
    /// Extension paths and the like, computed by the chart from the ImageVolumes
    /// it mounted. Not derivable here and not in the database.
    chart_settings: BTreeMap<String, String>,
    /// `compute_ctl`'s bearer-token key set. See
    /// [`spec::ComputeSpecParams::compute_ctl_jwks`].
    compute_ctl_jwks: serde_json::Value,
    /// Which config files the caller wrote itself. See [`Prerender`].
    prerender: Prerender,
}

/// How much of the config a caller brought with it.
///
/// A pod brings none: the supervisor renders all three files from the
/// environment and the registry. `yb_amp_local` brings the two daemon tomls,
/// because their content comes from an environment this process cannot see —
/// per-endpoint data directories under the launcher's tree and ports it found by
/// searching for free ones.
///
/// It does **not** bring the compute spec. The settings in it are either
/// invariants of a yb-amp compute or derived from the registry, both of which
/// this process is the one that knows; a launcher that rendered its own copy kept
/// a second list of the same settings that drifted from this one (https://github.com/yugabyte/meko/issues/1050).
/// What is genuinely local goes through `COMPUTE_EXTRA_SETTINGS_JSON`, the same
/// layer the chart uses for the extension paths only it can compute.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Prerender {
    /// Nothing: render all three. The pod.
    Nothing,
    /// The two daemon tomls, but not the compute spec. `yb_amp_local`.
    Daemons,
    /// All three. A caller that wants this process purely as a supervisor.
    Everything,
}

impl Prerender {
    /// `PRERENDERED_CONFIG`: unset or false for [`Self::Nothing`], `daemons` for
    /// [`Self::Daemons`], anything else truthy for [`Self::Everything`].
    ///
    /// `true` keeps meaning all three, so a caller that has not been taught the
    /// compute spec is still correct rather than silently having it rewritten.
    fn from_env() -> Self {
        match env_opt("PRERENDERED_CONFIG").as_deref().map(str::trim) {
            Some(v) if v.eq_ignore_ascii_case("daemons") => Self::Daemons,
            _ if env_bool("PRERENDERED_CONFIG") => Self::Everything,
            _ => Self::Nothing,
        }
    }

    /// Whether a caller is driving this at all, which is also what decides
    /// whether each child gets its own log file.
    fn caller_driven(self) -> bool {
        self != Self::Nothing
    }

    fn brings_compute_spec(self) -> bool {
        self == Self::Everything
    }
}

/// `--run-dir <path>`, the one command-line argument this binary takes.
///
/// The pod passes nothing and the environment says everything, which is what a
/// ConfigMap is for. `yb_amp_local` passes it because several supervisors share
/// a host: it puts the endpoint's directory in the process's argv, so `ps` says
/// which endpoint a supervisor belongs to — and so tools that reap a cluster by
/// path anchor (`pkill -f <run dir>`, which is how the integration suite tears
/// one down) match the supervisor along with the daemons it started. Without it
/// the supervisor survives the sweep and restarts everything just killed.
fn run_dir_from_args() -> Result<Option<Utf8PathBuf>> {
    let args: Vec<String> = std::env::args().skip(1).collect();
    parse_run_dir(&args.iter().map(String::as_str).collect::<Vec<_>>())
}

fn parse_run_dir(args: &[&str]) -> Result<Option<Utf8PathBuf>> {
    let mut args = args.iter();
    let mut found = None;
    while let Some(arg) = args.next() {
        let value = match arg.strip_prefix("--run-dir") {
            Some("") => (*args.next().context("--run-dir needs a path")?).to_string(),
            Some(rest) => rest
                .strip_prefix('=')
                .with_context(|| format!("expected --run-dir=<path>, got {arg:?}"))?
                .to_string(),
            None => anyhow::bail!("unexpected argument {arg:?}; only --run-dir is accepted"),
        };
        found = Some(Utf8PathBuf::from(value));
    }
    Ok(found)
}

impl Env {
    fn read() -> Result<Self> {
        let run_dir: Utf8PathBuf = match run_dir_from_args()? {
            Some(dir) => dir,
            None => env_or("RUN_DIR", "/data").into(),
        };
        let bin_dir: Utf8PathBuf = env_or("BIN_DIR", "/usr/local/bin").into();
        let pg_distrib_dir: Utf8PathBuf = env_or("PG_DISTRIB_DIR", "/usr/local").into();

        // The controller has to reach in, so its presence is what widens the
        // daemons' listen address beyond loopback.
        let bind_host = match env_opt("CONTROLLER_URL") {
            Some(_) => "0.0.0.0",
            None => boot::LISTEN_HOST,
        };

        let branch_id = env_opt("BRANCH_ID").context("BRANCH_ID is required")?;

        // What the caller brought, which is also what says whether each daemon's
        // log is read on its own.
        let prerender = Prerender::from_env();

        let compute_ctl_http_port = env_port("COMPUTE_CTL_HTTP_PORT", 3080)?;
        let safekeeper_pg_port = env_port("SAFEKEEPER_PG_PORT", 5454)?;

        let layout = boot::Layout {
            run_dir,
            bin_dir,
            pg_distrib_dir,
            branch_id,
            compute_port: env_port("COMPUTE_PORT", 55433)?,
            pageserver_pg_port: env_port("PAGESERVER_PG_PORT", 64000)?,
            pageserver_http_port: env_port("PAGESERVER_HTTP_PORT", 9898)?,
            pageserver_grpc_port: env_port("PAGESERVER_GRPC_PORT", 51051)?,
            safekeeper_pg_port,
            // A pod's compute and pageserver reach the same port; `yb_amp_local`
            // can put a tenant-only one in front of the compute.
            safekeeper_compute_port: env_port("SAFEKEEPER_COMPUTE_PORT", safekeeper_pg_port)?,
            safekeeper_http_port: env_port("SAFEKEEPER_HTTP_PORT", 7676)?,
            compute_ctl_http_port,
            // The pod allocates the pair contiguously; `yb_amp_local` takes each
            // from the first free port it finds, so it passes both.
            compute_ctl_internal_http_port: env_port(
                "COMPUTE_CTL_INTERNAL_HTTP_PORT",
                compute_ctl_http_port + 1,
            )?,
            pg_version_major: env_or("ENDPOINT_PG_VERSION", "18")
                .parse()
                .context("ENDPOINT_PG_VERSION is not a major version number")?,
            bind_host: bind_host.to_string(),
            control_plane_api: env_or("CONTROL_PLANE_API", "http://localhost:6666"),
            compute_id: env_or("COMPUTE_ID", boot::COMPUTE_ID),
            privileged_role_name: env_opt("PRIVILEGED_ROLE_NAME"),
            log_files: prerender.caller_driven(),
            identities: identity::Identities::from_env()?,
            // The chart's fixed port in a pod; a launcher sharing a host allocates
            // one per endpoint, and one that allocated none keeps the old path.
            lfc_state_port: match env_opt("LFC_STATE_PORT") {
                _ if prerender.brings_compute_spec() => None,
                None if prerender == Prerender::Nothing => Some(9896),
                None => None,
                Some(_) => Some(env_port("LFC_STATE_PORT", 9896)?),
            },
        };
        anyhow::ensure!(
            layout.identities.is_none() || layout.lfc_state_port.is_some(),
            "the user split needs the LFC state server (LFC_STATE_PORT), or the compute \
             would need a YugabyteDB credential for its prewarm state"
        );

        // A malformed id means the overrides cannot be read, which is a warning
        // rather than a failure — but say so, since the pod will look fine while
        // quietly ignoring the operator's settings.
        let endpoint_id = match env_opt("ENDPOINT_ID") {
            None => None,
            Some(raw) => match well_formed_endpoint_id(&raw) {
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
            yb_sslcert: env_opt("YB_SSLCERT"),
            yb_sslkey: env_opt("YB_SSLKEY"),
            yb_password_env: env_opt("YB_PASSWORD_ENV"),
            compute_ssl_cert_file: env_opt("COMPUTE_SSL_CERT_FILE"),
            compute_ssl_key_file: env_opt("COMPUTE_SSL_KEY_FILE"),
            endpoint_id,
            compute_password: env_opt("COMPUTE_PASSWORD"),
            autoprewarm: env_bool("AUTOPREWARM"),
            offload_lfc_interval_seconds: env_u64("OFFLOAD_LFC_INTERVAL_SECONDS")?,
            shared_preload_libraries: shared_preload_libraries(
                env_opt("SHARED_PRELOAD_LIBRARIES").as_deref(),
                env_opt("BASE_SHARED_PRELOAD_LIBRARIES").as_deref(),
                env_opt("EXTRA_SHARED_PRELOAD_LIBRARIES").as_deref(),
            ),
            chart_settings: chart_settings()?,
            compute_ctl_jwks: compute_ctl_jwks()?,
            prerender,
        })
    }

    /// The compute's server certificate, or `None` for a plaintext compute.
    ///
    /// A half-configured pair is `None` rather than an error: the rest of the
    /// endpoint is fine, and refusing to start a database because one ConfigMap
    /// key is missing is the wrong trade. It is warned about, because the pod
    /// would otherwise look healthy while quietly serving no TLS.
    pub(crate) fn compute_tls(&self) -> Option<spec::ComputeTls<'_>> {
        match (
            self.compute_ssl_cert_file.as_deref(),
            self.compute_ssl_key_file.as_deref(),
        ) {
            (Some(cert_file), Some(key_file)) => Some(spec::ComputeTls {
                cert_file,
                key_file,
            }),
            (None, None) => None,
            _ => {
                tracing::warn!(
                    "COMPUTE_SSL_CERT_FILE and COMPUTE_SSL_KEY_FILE must both be set; \
                     the compute will not offer TLS"
                );
                None
            }
        }
    }

    fn yb_conn(&self) -> toml_edit::Value {
        boot::yb_conn_inline(
            &self.yb_host_ports,
            &self.yb_user,
            &self.yb_dbname,
            boot::YbTls {
                sslmode: self.yb_sslmode.as_deref(),
                sslrootcert: self.yb_sslrootcert.as_deref(),
                sslcert: self.yb_sslcert.as_deref(),
                sslkey: self.yb_sslkey.as_deref(),
            },
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
            ("sslcert", &self.yb_sslcert),
            ("sslkey", &self.yb_sslkey),
        ] {
            if let Some(v) = v {
                map.insert(k.into(), v.clone().into());
            }
        }
        serde_json::Value::Object(map)
    }

    /// The same connection as the typed config the store client takes. Built
    /// from the JSON above rather than restated, so the two cannot drift; the
    /// `type` tag is the storage-kind discriminator, which `YbConnConfig` does
    /// not carry (and rejects, since it denies unknown fields).
    fn yb_conn_config(&self) -> Result<ysql_client::YbConnConfig> {
        let mut v = self.yb_conn_typed_json();
        v.as_object_mut()
            .expect("yb_conn_typed_json builds an object")
            .remove("type");
        serde_json::from_value(v).context("building the YB connection for the write fence")
    }

    /// Where to serve the settings API, or `None` when no port is configured.
    ///
    /// Opt-in rather than defaulted: this is the one listener in the pod that
    /// exists for the control plane rather than for the endpoint's own traffic,
    /// so a deployment that has no controller calling it should not have it
    /// open. A port that will not parse is a warning and no listener, on the
    /// same reasoning as the rest of this surface — a typo in a ConfigMap must
    /// not be the reason a database will not start.
    fn settings_api_addr(&self) -> Option<std::net::SocketAddr> {
        let raw = env_opt("SUPERVISOR_HTTP_PORT")?;
        match raw.trim().parse::<u16>() {
            Ok(port) if port > 0 => Some(std::net::SocketAddr::from((
                std::net::Ipv4Addr::UNSPECIFIED,
                port,
            ))),
            _ => {
                tracing::warn!(
                    port = %raw,
                    "SUPERVISOR_HTTP_PORT is not a port; the settings API will not be served"
                );
                None
            }
        }
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

/// `COMPUTE_CTL_JWKS_JSON`, the `{"keys": [...]}` object `compute_ctl` validates
/// bearer tokens against. Empty by default, which is the pod's and closes every
/// external authenticated route — see [`spec::ComputeSpecParams::compute_ctl_jwks`].
fn compute_ctl_jwks() -> Result<serde_json::Value> {
    let raw = env_or("COMPUTE_CTL_JWKS_JSON", r#"{"keys": []}"#);
    let parsed: serde_json::Value =
        serde_json::from_str(&raw).context("COMPUTE_CTL_JWKS_JSON is not JSON")?;
    anyhow::ensure!(
        parsed.get("keys").is_some_and(|k| k.is_array()),
        "COMPUTE_CTL_JWKS_JSON must be an object with a \"keys\" array"
    );
    Ok(parsed)
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
    if std::env::args().nth(1).as_deref() == Some(lfc_proxy::SUBCOMMAND) {
        let Err(e) = lfc_proxy::run_from_env();
        tracing::error!("LFC state service failed: {e:#}");
        return std::process::ExitCode::FAILURE;
    }
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
    let mut bring_up_start = std::time::Instant::now();

    let env = std::sync::Arc::new(Env::read()?);
    let l = &env.layout;
    // Before anything renders a daemon config or builds the HTTP client: both
    // capture it.
    let secret = match env.prerender {
        Prerender::Nothing => Some(coordinate::generate_control_secret()?),
        Prerender::Daemons | Prerender::Everything => prerendered_control_secret(l)?,
    };
    if secret.is_none() {
        // A caller's config from before the secret existed. The daemons then
        // accept a write from anything that reaches their port.
        anyhow::ensure!(
            l.identities.is_none(),
            "the user split needs http_control_secret in the caller's pageserver.toml and \
             safekeeper.toml; without it the compute could change the daemons' state"
        );
        tracing::warn!(
            "the caller's daemon config sets no http_control_secret; the pageserver and \
             safekeeper accept state-changing requests from anything on their port"
        );
    }
    coordinate::set_control_secret(secret)?;
    let fence_cfg = fence::Config::from_env(&l.branch_id)?;
    if !fence_cfg.required {
        tracing::warn!(
            "REQUIRE_WRITE_TOKEN is off: this endpoint runs unfenced, and nothing stops a \
             second endpoint writing the same branch"
        );
    }

    std::fs::create_dir_all(&l.run_dir).with_context(|| format!("create {}", l.run_dir))?;
    // Before anything is started, and held for the life of the process: it is
    // both this supervisor's claim on the run directory and how `yb_amp_local`
    // tells a live supervisor from a stale one.
    let _pidfile = pidfile::claim(&l.run_dir)?;

    // The daemons and the compute load the bundled Postgres libraries.
    prepend_ld_library_path(&l.pg_lib());
    // An externalized extension may ship the system libraries its .so links
    // (the CloudNativePG PostGIS image bundles geos/proj/gdal), which live only
    // in that ImageVolume. Appended, not prepended: the endpoint image's own
    // libraries must keep winning.
    if let Some(dirs) = env_opt("EXTENSION_LIBRARY_PATH") {
        append_ld_library_path(&dirs);
    }

    for dir in [l.run_dir.clone(), l.ps_datadir(), l.sk_datadir()] {
        std::fs::create_dir_all(&dir).with_context(|| format!("create {dir}"))?;
    }
    if let Some(ids) = l.identities {
        boot::prepare_split_dirs(l, &ids)?;
    }

    // Before the registry read and before any child: everything below needs YB,
    // and neither the read's retry budget nor the restart budget is sized to
    // absorb an outage.
    let (yb_host, yb_port) = env.yb_probe_target()?;
    let yb_wait_start = std::time::Instant::now();
    if boot::wait_for_yb(
        &yb_host,
        yb_port,
        boot::YB_WAIT_TIMEOUT,
        boot::YB_WAIT_INTERVAL,
    ) == boot::Ready::Reachable
    {
        startup::phase("yb_wait", yb_wait_start.elapsed());
    }
    if supervise::shutdown_requested() {
        tracing::info!("shutdown requested during startup; nothing was started");
        return Ok(std::process::ExitCode::SUCCESS);
    }

    // The caller's files must be there before anything reads them, whichever
    // subset it promised.
    require_prerendered(l, env.prerender)?;
    if env.prerender != Prerender::Everything {
        // Tolerant on the boot path: `fetch_overrides` warns and returns
        // defaults, because an endpoint that cannot reach the registry must
        // still start. `reload_config` refuses instead — see there.
        //
        // With `Prerender::Daemons` this renders the compute spec only; see
        // [`render_config`].
        startup::timed("render", || {
            let overrides = fetch_overrides(&env);
            render_config(&env, fence_cfg.required, &overrides)
        })?;
    }

    // Written by each lease's remote-auth run, served by the settings API.
    let remote_auth = std::sync::Arc::new(remote_auth::Status::default());

    // Nothing starts before the endpoint holds its branch's write lease, and
    // losing it takes every child down. A pod that cannot get the lease is a pod
    // waiting its turn, so this loops rather than failing — it wins the lease,
    // serves under it, and on loss goes back to waiting.
    // REQUIRE_WRITE_TOKEN=false is the escape hatch for a fencing bug, so it
    // must not depend on any of the fencing machinery — including the token
    // store, whose brokenness may be exactly why it was set. With fencing off,
    // nothing here is connected to, acquired, renewed, or released.
    // Once for the process, not once per lease: the settings it serves belong to
    // the endpoint, which outlives any lease it happens to be holding. Not fatal
    // — an endpoint serves fine without it, and refusing to run because a port is
    // taken would trade a working database for a convenience. Only when a port is
    // configured, which the chart and `yb_amp_local` both do. It is served on a
    // prerendered endpoint too. On `Everything` every call is the refusal in
    // `reload_handler`, which the caller learns from an answer rather than from a
    // connection refused; on `Daemons` the compute spec is this process's to
    // re-render, so the call does what it says.
    if let Some(addr) = env.settings_api_addr() {
        let state = std::sync::Arc::new(http::State::new(
            env.clone(),
            fence_cfg.required,
            remote_auth.clone(),
        ));
        match http::spawn(addr, state) {
            Ok(bound) => tracing::info!(%bound, "settings API bound"),
            Err(e) => tracing::warn!("settings API unavailable: {e:#}"),
        }
    }

    // Same reasoning, and the same scope: the certificate is the endpoint's, so
    // one watcher for the process outlives every lease it serves under.
    if let Some(tls) = env.compute_tls() {
        compute_tls::spawn(
            tls.cert_file,
            tls.key_file,
            l.compute_port,
            l.staged_compute_tls(),
        );
    }

    let mut fence = if fence_cfg.required {
        Some(fence::Fence::connect(
            fence_cfg.clone(),
            &env.yb_conn_config()?,
            &l.branch_id,
            vec![
                (
                    "safekeeper",
                    format!(
                        "http://{}:{}/v1/write_token",
                        boot::LISTEN_HOST,
                        l.safekeeper_http_port
                    ),
                ),
                (
                    "pageserver",
                    format!(
                        "http://{}:{}/v1/write_token",
                        boot::LISTEN_HOST,
                        l.pageserver_http_port
                    ),
                ),
            ],
        )?)
    } else {
        None
    };

    loop {
        if let Some(f) = fence.as_mut() {
            let lease_start = std::time::Instant::now();
            let acquired = f.acquire_blocking();
            if acquired {
                startup::phase("lease", lease_start.elapsed());
            } else {
                tracing::info!("shutdown requested while waiting for the write token");
                // After a lease lost to a late renewal, the store still names this
                // client with a fresh deadline (`Fence::renew`); release it so a
                // replacement is not left waiting out the TTL. A no-op when no
                // token was ever held.
                f.release();
                return Ok(std::process::ExitCode::SUCCESS);
            }
        }
        let (outcome, returned) = serve_one_lease(
            &env,
            fence_cfg.required,
            fence,
            &remote_auth,
            bring_up_start,
        )?;
        fence = returned;
        match outcome {
            Outcome::Shutdown => {
                tracing::info!("stopped on signal");
                if let Some(f) = fence.as_mut() {
                    f.release();
                }
                return Ok(std::process::ExitCode::SUCCESS);
            }
            // Non-zero so the kubelet recreates the pod: a service that cannot
            // stay up is not something this process can fix by restarting it
            // again.
            Outcome::BudgetExceeded { service, restarts } => {
                tracing::error!(service, restarts, "exiting so the pod is recreated");
                if let Some(f) = fence.as_mut() {
                    f.release();
                }
                return Ok(std::process::ExitCode::FAILURE);
            }
            // Only reachable with a fence: `fence::lost` is raised by its
            // renewal thread.
            Outcome::FenceLost => {
                bring_up_start = std::time::Instant::now();
                tracing::warn!(
                    "every service stopped; waiting for the write token to bring the \
                     endpoint back up"
                );
            }
        }
    }
}

/// Brings the endpoint up under one lease and supervises it until that lease
/// ends, the process is asked to stop, or a service exhausts its restart budget.
///
/// Called once per lease, so everything it does has to be idempotent across
/// restarts: the configs are already written, the safekeeper's branch create
/// tolerates its own 409, and the pageserver reads its init mode off a row that
/// exists by then.
fn serve_one_lease(
    env: &std::sync::Arc<Env>,
    fence_required: bool,
    fence: Option<fence::Fence>,
    remote_auth: &std::sync::Arc<remote_auth::Status>,
    bring_up_start: std::time::Instant,
) -> Result<(Outcome, Option<fence::Fence>)> {
    let l = &env.layout;

    // The compute starts last, after the pageserver/safekeeper handshake: the
    // safekeeper only learns about the branch over its HTTP API, and the LSN to
    // create it at comes from the pageserver once its branch is Active.
    let mut services = boot::services(l);
    let compute = services
        .pop()
        .context("boot::services must include a compute")?;
    anyhow::ensure!(
        compute.name == boot::COMPUTE_SERVICE,
        "expected compute_ctl last in boot::services, got {}",
        compute.name
    );

    let mut sup = Supervisor::new(Policy::from_env());
    for svc in services {
        sup.add(svc);
    }
    // Before the compute, whose spec points at it.
    if let Some(svc) = boot::lfc_state_service(
        l,
        env.endpoint_id.as_deref(),
        &env.yb_conn_typed_json().to_string(),
    ) {
        sup.add(svc);
    }
    // Renewing runs for the whole bring-up, not just once it is over. Starting a
    // pageserver on a fresh branch runs initdb and imports the initial page set,
    // which takes longer than one lease — so the token a child is seeded with at
    // spawn expires mid-import unless something keeps renewing it, and each
    // renewal is what carries a fresh token to the daemons as they come up.
    //
    // Losing the lease here is not a half-built endpoint torn down for no
    // reason: it means another endpoint owns the branch, and this one has to
    // stop at whatever stage it reached.
    let renewals = fence.map(fence::spawn_renewals);

    // Every fallible step runs inside this closure so there is exactly one way
    // out of the function, and the fence always comes back through it. An error
    // that returned directly would leave the renewal thread holding the fence —
    // renewing a lease for daemons that are gone, unreleased until the process
    // exits and the TTL runs out on its own.
    let outcome_res = (|| -> Result<Outcome> {
        sup.start_all()?;
        // Both daemons get the live token as soon as they are up, rather than
        // waiting out a renewal tick holding whatever they were seeded with.
        fence::push_current();

        // Wrapped so a failure — or a SIGTERM landing mid-handshake — stops the
        // two daemons that are already up. Returning straight out would leave
        // them running against datadirs nobody is closing, which is the
        // situation the signal handlers exist to prevent, and `Supervisor` does
        // not reap on drop.
        let handshake = startup::timed("handshake", || -> Result<String> {
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
                coordinate::BRANCH_CREATE_TIMEOUT,
                coordinate::BRANCH_CREATE_INTERVAL,
            )?;
            Ok(start_lsn)
        });
        if let Err(e) = handshake {
            sup.stop_all();
            return Err(e).context("the pageserver/safekeeper handshake");
        }

        // Only on this first spawn: a compute the monitor restarts later is not
        // waiting on any of the supervisor's boot.
        sup.add(compute.env_once(
            startup::BOOT_MS_ENV,
            &bring_up_start.elapsed().as_millis().to_string(),
        ));
        // Skips the two that are already running.
        sup.start_all()?;
        startup::STARTUP_SECONDS.observe(bring_up_start.elapsed().as_secs_f64());

        // On its own thread: this waits for Postgres to answer queries, and
        // blocking here would stop the monitor loop from restarting a sibling
        // that crashed meanwhile. The controller's Docker deployer treats the
        // line it logs as the endpoint's readiness signal — see
        // remote_auth::READY_MARKER — and the controller polls its outcome over
        // `GET /v1/remote-auth`.
        //
        // Skipped when the caller rendered the config: the password exists so a
        // client outside the pod can authenticate, and a `yb_amp_local` endpoint
        // is reached over loopback, by a spec that already says how its roles
        // are set up. The route then answers `pending`, and nothing asks it.
        if !env.prerender.caller_driven() {
            remote_auth::spawn(
                &l.pg_bin().join("psql"),
                l.compute_port,
                env.compute_password.as_deref(),
                remote_auth.clone(),
            );
        }

        // The control socket goes up last, once every service exists, so a
        // caller cannot ask about one that has not been added yet. Sharing the
        // supervisor means the monitor loop takes the lock a pass at a time —
        // see `control`.
        let sup: control::Shared = std::sync::Arc::new(std::sync::Mutex::new(sup));
        // The settings API restarts through this one. Published per lease
        // because it is rebuilt per lease, and retired when serving ends so a
        // reload between leases says there is nothing running rather than
        // restarting into a supervisor that has stopped.
        http::set_supervisor(Some(sup.clone()));
        // Not fatal, at either step: the endpoint serves fine without a control
        // surface, and taking it down over one would trade a working database
        // for a missing convenience. `thread::spawn` panics when the process is
        // out of threads, which is exactly the moment that trade is worst.
        match control::bind(&l.run_dir).and_then(|(listener, generation)| {
            let sup = sup.clone();
            // The control socket re-renders the compute spec before starting the
            // compute, so it needs the same environment the boot render used.
            let env_for_control = env.clone();
            let fence_for_control = fence_required;
            std::thread::Builder::new()
                .name("control".into())
                .spawn(move || {
                    control::serve(
                        listener,
                        sup,
                        generation,
                        env_for_control,
                        fence_for_control,
                    )
                })
                .context("spawning the control thread")
        }) {
            Ok(_handle) => {}
            Err(e) => tracing::warn!("control socket unavailable: {e:#}"),
        }

        tracing::info!("all services are up");

        const POLL: std::time::Duration = std::time::Duration::from_millis(500);
        control::run_shared(&sup, POLL)
    })();

    // Serving is over, so there is no supervisor to restart through until the
    // next lease publishes one. A reload in the gap says so rather than acting
    // on a supervisor that has stopped.
    http::set_supervisor(None);

    // The serving is over, however it ended; nothing else stops the renewal
    // thread when the daemons died for a spent restart budget — the process is
    // not shutting down and the lease is not lost, so it would renew forever
    // and this join would never return.
    fence::stop_renewals();
    let mut fence = renewals
        .map(|handle| {
            handle
                .join()
                .map_err(|_| anyhow::anyhow!("the write-token renewal thread panicked"))
        })
        .transpose()?;

    match outcome_res {
        Ok(outcome) => Ok((outcome, fence)),
        Err(e) => {
            // The caller never sees the fence on this path, so the lease is
            // handed back here — without this, a replacement waits out the TTL
            // for a lease whose holder is already exiting.
            if let Some(f) = fence.as_mut() {
                f.release();
            }
            Err(e)
        }
    }
}

/// Sizes the endpoint from its cgroup and writes every config file: both daemons'
/// TOML and the compute spec.
///
/// All of it happens before anything starts, because a render failure is a bad
/// config — failing here beats crash-looping a daemon against it.
/// What a render changed on disk.
///
/// The daemons read their config once, at start, so "changed" is the same
/// question as "does this service have to be restarted to be running what the
/// registry says". Reporting it is the whole value of re-rendering at runtime:
/// re-rendering is cheap, restarting a pageserver is not.
#[derive(Debug, Default, Clone, PartialEq, Eq, serde::Serialize)]
pub struct Rendered {
    pub pageserver: bool,
    pub safekeeper: bool,
    pub compute: bool,
    /// The Postgres settings whose effective value moved, when the compute spec
    /// changed. What to do about them is [`reconfigure::decide`]'s to say — it
    /// needs the running server to classify them.
    pub changed_settings: Vec<String>,
    /// Settings that were stored and deliberately not written, because Postgres
    /// would refuse the config file that contained them, each with why. See
    /// [`crate::guc`].
    pub refused_settings: Vec<guc::NotApplied>,
}

/// Re-read the registry and re-render, for a caller that is not booting.
///
/// The difference from the boot path is what a failed read means. At boot,
/// carrying on with derived defaults is right: the endpoint has nothing else to
/// start from, and an override that cannot be read is worth less than a database
/// that will not come up. Here it is the opposite — the real settings are
/// already on disk and already running, so rendering defaults over them would
/// discard an operator's configuration because of a transient YB error, and
/// report all three services as needing a restart onto it.
pub fn reload_config(env: &Env, fence_required: bool) -> Result<Rendered> {
    let overrides = match env.endpoint_id.as_deref() {
        // Nothing to look up, so nothing to lose: same as boot.
        None => registry::Overrides::default(),
        Some(id) => {
            let yb = env
                .yb_conn_config()
                .context("building the registry connection")?;
            registry::fetch(&yb, id)
                .context("re-reading the registry")?
                .unwrap_or_default()
        }
    };
    render_config(env, fence_required, &overrides)
}

/// The certificate settings an operator asked for and will not get.
///
/// Only when the compute actually has a certificate: with no mount there is
/// nothing rendered after their settings, so `ssl = off` from an operator is an
/// ordinary setting that applies like any other.
pub(crate) fn compute_tls_refusals(
    env: &Env,
    operator: &std::collections::BTreeMap<String, String>,
) -> Vec<guc::NotApplied> {
    if env.compute_tls().is_none() {
        return Vec::new();
    }
    spec::COMPUTE_TLS_OWNED
        .iter()
        .filter(|name| operator.contains_key(**name))
        .map(|name| guc::NotApplied {
            name: (*name).to_string(),
            reason: guc::COMPUTE_TLS_OWNED.to_string(),
        })
        .collect()
}

pub fn render_config(
    env: &Env,
    fence_required: bool,
    overrides: &registry::Overrides,
) -> Result<Rendered> {
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

    // Asked before the render, not after: a name or value Postgres refuses would
    // take the whole config file down with it, and at create there is no server
    // to have asked instead.
    let screened = guc::screen(
        &l.pg_bin(),
        &l.run_dir,
        l.identities.map(|i| i.compute),
        &overrides.pg,
    );
    for r in &screened.refused {
        tracing::warn!(
            name = %r.name,
            reason = %r.reason,
            "stored but not rendered"
        );
    }

    // The chart's own settings get the same treatment. They used to be safe by
    // construction — extension_control_path and dynamic_library_path are stock
    // PG18 — but the chart now also renders fork-only GUCs for a registered
    // extension (yb_amp_abi_extra_exempt_libraries). An endpoint pinned to an
    // image whose postgres predates such a GUC would otherwise be handed a name
    // it refuses, and Postgres discards the WHOLE config file over one bad
    // name: the endpoint then dies at its next restart, far from the change
    // that caused it. Dropping the one setting instead costs that extension its
    // waiver and leaves everything else in force, which is the recoverable
    // failure.
    let screened_chart = guc::screen(
        &l.pg_bin(),
        &l.run_dir,
        l.identities.map(|i| i.compute),
        &env.chart_settings,
    );
    for r in &screened_chart.refused {
        tracing::warn!(
            name = %r.name,
            reason = %r.reason,
            "chart setting not rendered: this compute's postgres does not accept it"
        );
    }

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

    // The branch's lease timings, before the fence takes a token with them.
    // `render_config` runs ahead of the acquire, so this is the one read that
    // already has them — the alternative is a second registry round trip on the
    // boot path to fetch what we are holding.
    match fence::apply_overrides(&overrides.write_token) {
        Ok(true) => {
            let (ttl, renew) = fence::timings();
            tracing::info!(
                ttl_ms = ttl.as_millis() as u64,
                renew_ms = renew.as_millis() as u64,
                "write token: branch settings applied"
            );
        }
        Ok(false) => {}
        // Not fatal, for the same reason the rest of this read is not: an
        // endpoint that cannot use the branch's settings must still come up on
        // the ones it was started with.
        Err(e) => tracing::warn!("write token: ignoring the branch's lease settings: {e:#}"),
    }

    // Only the daemon tomls take it, so it is not computed when they are the
    // caller's — and neither is the warning, which would be noise about a file
    // this process is not writing.
    let scale = res
        .vcpu
        .map(|v| ("scale_factor", toml_edit::Value::from(v)));
    if scale.is_none() && env.prerender == Prerender::Nothing {
        tracing::warn!("endpoint CPU unknown; leaving the daemons' scale_factor at their default");
    }

    // Skipped when the caller brought the daemon tomls: theirs describe per-endpoint
    // data directories and ports this process cannot see, and rendering over them
    // would replace a working local config with one built from pod defaults.
    //
    // Any daemon override stored for this endpoint is therefore not applied, and
    // is named back to the caller rather than dropped — a reload that answered
    // 200 with nothing restarted would otherwise read as "your edit took".
    let mut refused_daemon: Vec<guc::NotApplied> = Vec::new();
    if env.prerender != Prerender::Nothing {
        for (daemon, keys) in [
            ("pageserver", &overrides.pageserver),
            ("safekeeper", &overrides.safekeeper),
        ] {
            refused_daemon.extend(keys.keys().map(|k| guc::NotApplied {
                name: format!("{daemon}.{k}"),
                reason: guc::CALLERS_DAEMON_CONFIG.to_string(),
            }));
        }
    }
    let (pageserver, safekeeper) = if env.prerender == Prerender::Nothing {
        let mut ps_base = boot::pageserver_base(l, env.yb_conn(), fence_required);
        ps_base.extend(scale.clone());
        if let Some(secret) = coordinate::control_secret() {
            ps_base.push(("http_control_secret", secret.into()));
        }
        let ps_toml = config::render(config::Daemon::Pageserver, ps_base, &overrides.pageserver)
            .context("rendering pageserver.toml")?;
        let pageserver = write(&l.ps_datadir().join("pageserver.toml"), &ps_toml)?;

        // The safekeeper's yb_storage_conn is a string-encoded inline table rather
        // than a table, which is the shape its config type expects.
        let mut sk_base = boot::safekeeper_base(l, env.yb_conn().to_string(), fence_required);
        sk_base.extend(scale);
        if let Some(secret) = coordinate::control_secret() {
            sk_base.push(("http_control_secret", secret.into()));
        }
        let sk_toml = config::render(config::Daemon::Safekeeper, sk_base, &overrides.safekeeper)
            .context("rendering safekeeper.toml")?;
        let safekeeper = write(&l.sk_datadir().join("safekeeper.toml"), &sk_toml)?;
        (pageserver, safekeeper)
    } else {
        (false, false)
    };

    let staged_tls = l.staged_compute_tls();
    let compute_spec = spec::render_compute_spec(
        &spec::ComputeSpecParams {
            compute_port: l.compute_port,
            branch_id: &l.branch_id,
            safekeeper_addr: &l.safekeeper_compute_addr(),
            pageserver_connstring: &l.pageserver_connstring(),
            shared_preload_libraries: &env.shared_preload_libraries,
            endpoint_id: env.endpoint_id.as_deref(),
            // The compute gets the LFC state server instead of the storage
            // connection whenever one runs, so it holds no YugabyteDB credential.
            yb_storage_conn: match l.lfc_state_url() {
                Some(_) => serde_json::Value::Null,
                None => env.yb_conn_typed_json(),
            },
            lfc_state_url: l.lfc_state_url(),
            autoprewarm: env.autoprewarm,
            offload_lfc_interval_seconds: env.offload_lfc_interval_seconds,
            compute_ctl_jwks: env.compute_ctl_jwks.clone(),
            compute_tls: match (&staged_tls, env.compute_tls()) {
                (Some(to), Some(_)) => Some(spec::ComputeTls {
                    cert_file: to.cert_file.as_str(),
                    key_file: to.key_file.as_str(),
                }),
                (_, mounted) => mounted,
            },
        },
        &settings,
        &screened_chart.keep,
        &screened.keep,
    );
    let rendered_spec = serde_json::to_string(&compute_spec)?;
    let (compute, previous) = write_returning_previous(&l.compute_spec(), &rendered_spec)?;
    // Only meaningful against a spec that was already there: at boot there is
    // nothing to diff, and nothing running to reload either.
    // No previous spec to diff, or one that will not parse: every setting counts
    // as changed. An empty list would mean "nothing moved", and nothing moved is
    // reported as applied — which is the one answer that must not come out of
    // "what moved could not be determined".
    let changed_settings = match previous
        .as_deref()
        .map(serde_json::from_str::<serde_json::Value>)
    {
        Some(Ok(old)) => reconfigure::changed_settings(&old, &compute_spec),
        _ => reconfigure::settings_of(&compute_spec)
            .into_keys()
            .collect(),
    };
    Ok(Rendered {
        pageserver,
        safekeeper,
        compute,
        changed_settings,
        // The chart's refusals belong here too, not just in the log. Without
        // them a chart GUC the probe rejected is reported nowhere the settings
        // API can see; worse, on the next render its name has vanished from the
        // previous compute_spec.json, so it lands in changed_settings, reaches
        // classify_settings as "inapplicable", and slips past the filter whose
        // whole job is to keep a render-refused name from being re-reported
        // under that generic reason.
        refused_settings: screened
            .refused
            .into_iter()
            .chain(screened_chart.refused)
            .chain(refused_daemon)
            // The certificate settings render after the operator's, so one of
            // theirs is stored and then beaten. Say so: the probe passes it
            // (a path parses), nothing moves, and an unreported edit that did
            // nothing is the failure `not_applied` was added to stop.
            .chain(compute_tls_refusals(env, &overrides.pg))
            .collect(),
    })
}

/// Checks that the caller really did write the files it said it would.
///
/// Cheap, and it converts the whole failure mode of `PRERENDERED_CONFIG` into one
/// message. Without it a missing file surfaces as a daemon that exits on a
/// default config it silently invented, or a `compute_ctl` that panics on a spec
/// it cannot open — neither of which names the file that is missing.
fn require_prerendered(l: &boot::Layout, mode: Prerender) -> Result<()> {
    let mut paths = match mode {
        Prerender::Nothing => return Ok(()),
        Prerender::Daemons | Prerender::Everything => vec![
            l.ps_datadir().join("pageserver.toml"),
            l.sk_datadir().join("safekeeper.toml"),
        ],
    };
    if mode.brings_compute_spec() {
        paths.push(l.compute_spec());
    }
    for path in paths {
        anyhow::ensure!(
            path.exists(),
            "PRERENDERED_CONFIG is set but {path} does not exist"
        );
    }
    match mode {
        Prerender::Everything => {
            tracing::info!("running on the caller's config; rendering nothing")
        }
        _ => tracing::info!("running on the caller's daemon config; rendering the compute spec"),
    }
    Ok(())
}

/// The secret the caller rendered into both daemon tomls, which this process
/// has to send. A missing file is left to [`require_prerendered`] to report.
fn prerendered_control_secret(l: &boot::Layout) -> Result<Option<String>> {
    let read = |path: Utf8PathBuf| -> Result<Option<String>> {
        let Ok(text) = std::fs::read_to_string(&path) else {
            return Ok(None);
        };
        let doc: toml_edit::DocumentMut =
            text.parse().with_context(|| format!("parsing {path}"))?;
        Ok(doc
            .get("http_control_secret")
            .and_then(|v| v.as_str())
            .map(String::from))
    };
    let ps = read(l.ps_datadir().join("pageserver.toml"))?;
    let sk = read(l.sk_datadir().join("safekeeper.toml"))?;
    anyhow::ensure!(
        ps == sk,
        "pageserver.toml and safekeeper.toml disagree on http_control_secret; \
         one secret has to serve both"
    );
    Ok(ps)
}

/// Reads the operator overrides. A failure is a warning: an endpoint that cannot
/// reach the registry must still start on its derived defaults, since refusing to
/// A well-formed endpoint id, or `None`.
///
/// Not about SQL any more — the registry read binds the id as a parameter, so
/// nothing it contains can reach the statement. This is about the **compute
/// spec**: `env.endpoint_id` is also what `compute_ctl` keys its LFC prewarm and
/// offload blobs by (see `spec::Params::endpoint_id`). An id that is absent
/// disables prewarm, which is a defined state; an arbitrary string becomes a
/// blob key, which is not. Checking it here keeps both consumers seeing only
/// ids the control plane could have issued.
fn well_formed_endpoint_id(raw: &str) -> Option<String> {
    let s = raw.trim();
    let shape = s.len() == 36
        && s.as_bytes().iter().enumerate().all(|(i, b)| {
            if matches!(i, 8 | 13 | 18 | 23) {
                *b == b'-'
            } else {
                b.is_ascii_hexdigit()
            }
        });
    shape.then(|| s.to_string())
}

/// boot because an override *might* exist would turn a registry blip into an
/// outage.
fn fetch_overrides(env: &Env) -> registry::Overrides {
    let Some(id) = &env.endpoint_id else {
        tracing::info!("no ENDPOINT_ID; running on derived defaults");
        return registry::Overrides::default();
    };
    let yb = match env.yb_conn_config() {
        Ok(yb) => yb,
        Err(e) => {
            tracing::warn!("could not build the registry connection: {e:#}");
            return registry::Overrides::default();
        }
    };
    match registry::fetch(&yb, id) {
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

/// Write the file, and say whether that changed it.
///
/// Compared rather than assumed: at runtime the answer decides whether a daemon
/// has to be restarted, and re-rendering the same bytes is the common case —
/// most of a config comes from the environment and the resources, not from the
/// registry row someone just edited.
fn write(path: &Utf8Path, contents: &str) -> Result<bool> {
    Ok(write_returning_previous(path, contents)?.0)
}

/// Write, and hand back what was there — the compute path diffs the two to
/// decide whether the change needs a restart or only a reload.
fn write_returning_previous(path: &Utf8Path, contents: &str) -> Result<(bool, Option<String>)> {
    let previous = std::fs::read_to_string(path).ok();
    let changed = match &previous {
        Some(existing) => existing != contents,
        // No file yet, or unreadable: writing it is a change either way.
        None => true,
    };
    // Written beside and renamed over, rather than truncated in place. At boot
    // nothing else is reading these; at runtime a re-render races the monitor
    // loop restarting a daemon, and a daemon that reads a half-written config
    // fails on a parse error that names nothing about why. `rename` within a
    // directory is atomic, so a concurrent reader sees one version or the other.
    let tmp = Utf8PathBuf::from(format!("{path}.tmp"));
    std::fs::write(&tmp, contents).with_context(|| format!("write {tmp}"))?;
    std::fs::rename(&tmp, path).with_context(|| format!("rename {tmp} -> {path}"))?;
    tracing::info!(%path, bytes = contents.len(), changed, "wrote");
    Ok((changed, previous))
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

/// Puts `dirs` (a colon-separated list) behind the current `LD_LIBRARY_PATH`,
/// which the children inherit in turn.
fn append_ld_library_path(dirs: &str) {
    let existing = std::env::var("LD_LIBRARY_PATH").unwrap_or_default();
    let joined = if existing.is_empty() {
        dirs.to_string()
    } else {
        format!("{existing}:{dirs}")
    };
    tracing::info!(%dirs, "extension library dirs appended to LD_LIBRARY_PATH");
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

    #[test]
    fn preload_extensions_extend_the_base_rather_than_replace_it() {
        assert_eq!(
            shared_preload_libraries(None, None, None),
            DEFAULT_SHARED_PRELOAD_LIBRARIES
        );
        // The chart's fallback for older supervisors does not shrink the base.
        assert_eq!(
            shared_preload_libraries(Some("yb_amp,pg_cron"), None, Some("pg_cron")),
            "yb_amp,pg_stat_statements,pgsentinel,auto_explain,pg_cron"
        );
        // An override replaces the base, and the extensions still go after it.
        assert_eq!(
            shared_preload_libraries(
                Some("yb_amp,pg_documentdb_core,pg_documentdb"),
                Some("yb_amp"),
                Some("pg_documentdb_core,pg_documentdb")
            ),
            "yb_amp,pg_documentdb_core,pg_documentdb"
        );
        // A name in both is loaded once.
        assert_eq!(
            shared_preload_libraries(None, Some("yb_amp, pg_cron"), Some("pg_cron,")),
            "yb_amp,pg_cron"
        );
        // An older chart sends only the whole list.
        assert_eq!(
            shared_preload_libraries(Some("yb_amp,pg_stat_statements"), None, None),
            "yb_amp,pg_stat_statements"
        );
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

    pub(crate) fn test_env() -> Env {
        Env {
            layout: boot::Layout {
                run_dir: "/data".into(),
                bin_dir: "/usr/local/bin".into(),
                pg_distrib_dir: "/usr/local".into(),
                branch_id: "9a054903-242f-46bc-a0c9-cc6b91a37e4c".into(),
                compute_port: 55433,
                pageserver_pg_port: 64000,
                pageserver_http_port: 9898,
                pageserver_grpc_port: 51051,
                safekeeper_pg_port: 5454,
                safekeeper_compute_port: 5454,
                safekeeper_http_port: 7676,
                compute_ctl_http_port: 3080,
                compute_ctl_internal_http_port: 3081,
                pg_version_major: 18,
                bind_host: "127.0.0.1".into(),
                control_plane_api: "http://localhost:6666".into(),
                compute_id: boot::COMPUTE_ID.into(),
                privileged_role_name: None,
                log_files: false,
                identities: None,
                lfc_state_port: Some(9896),
            },
            yb_host_ports: "127.0.0.1:5433".into(),
            yb_user: "yugabyte".into(),
            yb_dbname: "yb_storage".into(),
            yb_sslmode: None,
            yb_sslrootcert: None,
            yb_sslcert: None,
            yb_sslkey: None,
            yb_password_env: None,
            compute_ssl_cert_file: None,
            compute_ssl_key_file: None,
            endpoint_id: None,
            compute_password: Some("cloud_admin".into()),
            autoprewarm: false,
            offload_lfc_interval_seconds: None,
            shared_preload_libraries: "yb_amp".into(),
            chart_settings: BTreeMap::new(),
            prerender: Prerender::Nothing,
            compute_ctl_jwks: serde_json::json!({"keys": []}),
        }
    }

    /// The id reaches the compute spec as well as the registry read, and
    /// `compute_ctl` keys its LFC blobs by it. An id that is absent disables
    /// prewarm; an arbitrary string would become a blob key instead.
    #[test]
    fn the_prerendered_secret_is_read_from_both_tomls() {
        let dir = camino_tempfile::tempdir().unwrap();
        let mut l = test_env().layout;
        l.run_dir = dir.path().to_path_buf();
        std::fs::create_dir_all(l.ps_datadir()).unwrap();
        std::fs::create_dir_all(l.sk_datadir()).unwrap();
        let write = |ps: &str, sk: &str| {
            std::fs::write(l.ps_datadir().join("pageserver.toml"), ps).unwrap();
            std::fs::write(l.sk_datadir().join("safekeeper.toml"), sk).unwrap();
        };
        write("id = 1\n", "id = 1\n");
        assert_eq!(prerendered_control_secret(&l).unwrap(), None);
        write(
            "http_control_secret = \"s\"\n",
            "http_control_secret = \"s\"\n",
        );
        assert_eq!(
            prerendered_control_secret(&l).unwrap().as_deref(),
            Some("s")
        );
        write("http_control_secret = \"s\"\n", "id = 1\n");
        assert!(prerendered_control_secret(&l).is_err());
    }

    #[test]
    fn only_a_uuid_becomes_an_endpoint_id() {
        const ID: &str = "3f2b1c4e-0000-4000-8000-000000000001";
        assert_eq!(well_formed_endpoint_id(ID).as_deref(), Some(ID));
        assert_eq!(
            well_formed_endpoint_id(&format!("  {ID}  ")).as_deref(),
            Some(ID)
        );
        for bad in [
            "",
            "not-a-uuid",
            "'; DROP TABLE amp_registry.endpoints; --",
            "3f2b1c4e-0000-4000-8000-00000000000", // too short
            "3f2b1c4e-0000-4000-8000-0000000000011", // too long
            "3f2b1c4e_0000_4000_8000_000000000001", // wrong separators
            "zzzzzzzz-0000-4000-8000-000000000001", // non-hex
        ] {
            assert_eq!(well_formed_endpoint_id(bad), None, "accepted {bad:?}");
        }
    }

    /// The settings API is the one listener in this pod that exists for the
    /// control plane rather than the endpoint's own traffic, so it is served
    /// only where something calls it — the chart emits the port only when a
    /// controller URL is set.
    #[test]
    fn the_settings_api_is_served_only_when_a_port_is_configured() {
        let saved = std::env::var("SUPERVISOR_HTTP_PORT").ok();
        let env = test_env();
        // SAFETY: single-threaded test; this var is read by nothing else.
        unsafe { std::env::remove_var("SUPERVISOR_HTTP_PORT") };
        assert_eq!(env.settings_api_addr(), None, "unset must serve nothing");

        unsafe { std::env::set_var("SUPERVISOR_HTTP_PORT", "9899") };
        assert_eq!(env.settings_api_addr().unwrap().port(), 9899);

        // A typo in a ConfigMap must not be the reason a database will not
        // start, so these are warnings and no listener rather than failures.
        for bad in ["", "0", "no", "70000", "-1"] {
            unsafe { std::env::set_var("SUPERVISOR_HTTP_PORT", bad) };
            assert_eq!(env.settings_api_addr(), None, "accepted {bad:?}");
        }

        unsafe {
            match saved {
                Some(v) => std::env::set_var("SUPERVISOR_HTTP_PORT", v),
                None => std::env::remove_var("SUPERVISOR_HTTP_PORT"),
            }
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

    /// The launcher passes the run directory on the command line so the
    /// endpoint's path is in the supervisor's argv — `ps` says which endpoint it
    /// belongs to, and a teardown that reaps by path anchor (`pkill -f <run
    /// dir>`) matches it instead of leaving it to restart what it just killed.
    #[test]
    fn the_run_directory_can_come_from_the_command_line() {
        // The pod passes nothing; the environment decides.
        assert_eq!(parse_run_dir(&[]).unwrap(), None);
        assert_eq!(
            parse_run_dir(&["--run-dir", "/repo/endpoints/main"]).unwrap(),
            Some(Utf8PathBuf::from("/repo/endpoints/main"))
        );
        assert_eq!(
            parse_run_dir(&["--run-dir=/repo/endpoints/main"]).unwrap(),
            Some(Utf8PathBuf::from("/repo/endpoints/main"))
        );
        // A typo must not boot an endpoint against /data.
        assert!(parse_run_dir(&["--run-dir"]).is_err());
        assert!(parse_run_dir(&["--runn-dir=/x"]).is_err());
        assert!(parse_run_dir(&["/repo"]).is_err());
    }

    /// `PRERENDERED_CONFIG` promises three files exist. A missing one has to be
    /// named here: further down it becomes a daemon exiting on an invented
    /// default, or a `compute_ctl` panic on a spec it cannot open.
    #[test]
    fn prerendered_config_names_the_file_the_caller_did_not_write() {
        let dir = camino_tempfile::tempdir().unwrap();
        let mut l = test_env().layout;
        l.run_dir = dir.path().to_path_buf();

        std::fs::create_dir_all(l.ps_datadir()).unwrap();
        std::fs::create_dir_all(l.sk_datadir()).unwrap();

        let err = require_prerendered(&l, Prerender::Everything)
            .unwrap_err()
            .to_string();
        assert!(err.contains("pageserver.toml"), "{err}");

        std::fs::write(l.ps_datadir().join("pageserver.toml"), "").unwrap();
        std::fs::write(l.sk_datadir().join("safekeeper.toml"), "").unwrap();
        let err = require_prerendered(&l, Prerender::Everything)
            .unwrap_err()
            .to_string();
        assert!(err.contains("compute_spec.json"), "{err}");

        std::fs::write(l.compute_spec(), "{}").unwrap();
        require_prerendered(&l, Prerender::Everything).expect("all three present");
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
