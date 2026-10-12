//! Assembling the pod: what to render, and what to start in what order.
//!
//! Everything that decides the shape of the endpoint — the ports, the paths, the
//! three services and their health probes. Separate from `main` so it is testable
//! without spawning anything.

use std::time::Duration;

use camino::Utf8PathBuf;
use toml_edit::{InlineTable, Value};

use crate::supervise::{HealthCheck, Service};

/// The ports and identities the pod is wired with. The compute, the daemons and
/// the chart all agree on these values, so a change here needs a matching change
/// in the chart.
#[derive(Debug, Clone)]
pub struct Layout {
    pub run_dir: Utf8PathBuf,
    pub bin_dir: Utf8PathBuf,
    pub pg_distrib_dir: Utf8PathBuf,
    pub branch_id: String,
    pub compute_port: u16,
    pub pageserver_pg_port: u16,
    pub pageserver_http_port: u16,
    pub pageserver_grpc_port: u16,
    pub safekeeper_pg_port: u16,
    /// The safekeeper port the **compute** connects to, which the pageserver's
    /// does not have to equal: `yb_amp_local` can configure a tenant-only pg
    /// port, and that is the one walproposer must use. Defaults to
    /// [`Self::safekeeper_pg_port`], which is what a pod has.
    pub safekeeper_compute_port: u16,
    pub safekeeper_http_port: u16,
    /// `compute_ctl`'s own HTTP surface.
    pub compute_ctl_http_port: u16,
    /// `compute_ctl`'s internal HTTP surface. In the pod this is
    /// [`Self::compute_ctl_http_port`] plus one; `yb_amp_local` allocates the two
    /// independently, so it is carried rather than derived.
    pub compute_ctl_internal_http_port: u16,
    /// Postgres major version. The install is version-qualified inside
    /// `pg_distrib_dir`, so this is needed to find `bin/` and `lib/`.
    pub pg_version_major: u32,
    /// `0.0.0.0` when the controller must reach in, loopback otherwise.
    pub bind_host: String,
    pub control_plane_api: String,
    /// The identity `compute_ctl` reports itself under. One compute per pod, so
    /// [`COMPUTE_ID`] is enough there; `yb_amp_local` runs many on one host and
    /// passes the endpoint id.
    pub compute_id: String,
    /// `--privileged-role-name`, when the caller wants one. Absent in the pod.
    pub privileged_role_name: Option<String>,
    /// Give each child its own log file beside its datadir instead of letting it
    /// inherit this process's stdout.
    ///
    /// A pod wants one interleaved stream, which is what `kubectl logs` shows.
    /// `yb_amp_local` wants `endpoints/<id>/pageserver/pageserver.log`, because
    /// several endpoints share a host and each daemon's log is read on its own.
    pub log_files: bool,
    /// Set when the storage processes and the compute run as different users.
    pub identities: Option<crate::identity::Identities>,
    /// Loopback port of the LFC state service, used only with the users split.
    /// Where the LFC state server listens. `None` when the supervisor does not
    /// render the compute spec, or a launcher allocated no port for it; the
    /// compute then reaches YugabyteDB itself.
    pub lfc_state_port: Option<u16>,
}

/// Loopback is what the compute and the supervisor's own probes use, even when
/// the daemons bind `0.0.0.0` — a wildcard listener accepts it too.
pub const LISTEN_HOST: &str = "127.0.0.1";

/// The compute's service name, as the control socket and the supervisor's own
/// service list both spell it.
pub const COMPUTE_SERVICE: &str = "compute_ctl";

/// The default identity `compute_ctl` reports itself under. One compute per pod,
/// so a fixed name is enough — but the flag is required, not optional.
pub const COMPUTE_ID: &str = "yb-amp-endpoint";

impl Layout {
    pub fn ps_datadir(&self) -> Utf8PathBuf {
        self.run_dir.join("pageserver")
    }
    pub fn sk_datadir(&self) -> Utf8PathBuf {
        self.run_dir.join("safekeeper")
    }
    /// Under the compute user's own directory when the users are split:
    /// `compute_ctl` deletes and recreates pgdata, which needs write access to
    /// its parent, and the run directory stays the supervisor's.
    pub fn pgdata(&self) -> Utf8PathBuf {
        match self.identities {
            Some(_) => self.compute_dir().join("pgdata"),
            None => self.run_dir.join("pgdata"),
        }
    }
    pub fn compute_dir(&self) -> Utf8PathBuf {
        self.run_dir.join("compute")
    }
    /// Where `compute_ctl` reads and writes its LFC state, through the storage
    /// side, so it needs no YugabyteDB credential of its own.
    pub fn lfc_state_url(&self) -> Option<String> {
        self.lfc_state_port
            .map(|port| format!("http://{LISTEN_HOST}:{port}"))
    }
    /// Where the compute reads its server certificate and key when it runs as
    /// its own user and cannot read the mount.
    pub fn staged_compute_tls(&self) -> Option<crate::compute_tls::Staged> {
        self.identities.map(|ids| crate::compute_tls::Staged {
            cert_file: self.compute_dir().join("server.crt"),
            key_file: self.compute_dir().join("server.key"),
            owner: ids.compute,
        })
    }
    /// The compute spec `compute_ctl` loads — written by this process in the pod,
    /// and by the caller when the config is pre-rendered.
    pub fn compute_spec(&self) -> Utf8PathBuf {
        self.run_dir.join("compute_spec.json")
    }
    /// The Postgres binaries: `<pg_distrib_dir>/v<major>/bin`.
    ///
    /// Version-qualified, because that is how the image lays the install out —
    /// `pg_distrib_dir` itself holds one directory per major version. Flattening
    /// this to `<pg_distrib_dir>/bin` finds nothing, and the failure is indirect:
    /// `compute_ctl` panics on a missing `pg_config`, and `remote_auth` cannot
    /// find the `psql` it sets the compute's password with.
    pub fn pg_bin(&self) -> Utf8PathBuf {
        self.pg_distrib_dir
            .join(format!("v{}", self.pg_version_major))
            .join("bin")
    }

    /// The matching `lib/`, which the daemons and the compute load Postgres from.
    pub fn pg_lib(&self) -> Utf8PathBuf {
        self.pg_distrib_dir
            .join(format!("v{}", self.pg_version_major))
            .join("lib")
    }

    pub fn pageserver_connstring(&self) -> String {
        format!("host={LISTEN_HOST} port={}", self.pageserver_pg_port)
    }
    /// Where the pageserver streams WAL from — the safekeeper's pg port.
    pub fn safekeeper_addr(&self) -> String {
        format!("{LISTEN_HOST}:{}", self.safekeeper_pg_port)
    }
    /// What `yb_amp.safekeepers` names, which walproposer connects to. Separate
    /// from [`Self::safekeeper_addr`] because a tenant-only port serves the
    /// compute while the pageserver keeps using the pg port.
    pub fn safekeeper_compute_addr(&self) -> String {
        format!("{LISTEN_HOST}:{}", self.safekeeper_compute_port)
    }
}

/// The TLS half of the YB connection block, as libpq names the keys.
#[derive(Clone, Copy, Debug, Default)]
pub struct YbTls<'a> {
    pub sslmode: Option<&'a str>,
    pub sslrootcert: Option<&'a str>,
    pub sslcert: Option<&'a str>,
    pub sslkey: Option<&'a str>,
}

/// The YB connection as the inline toml table both daemons expect.
pub fn yb_conn_inline(
    host_ports: &str,
    user: &str,
    dbname: &str,
    tls: YbTls<'_>,
    password_env: Option<&str>,
) -> Value {
    let mut t = InlineTable::new();
    t.insert("host_ports", host_ports.into());
    t.insert("user", user.into());
    t.insert("dbname", dbname.into());
    for (k, v) in [
        ("password_env", password_env),
        ("sslmode", tls.sslmode),
        ("sslrootcert", tls.sslrootcert),
        ("sslcert", tls.sslcert),
        ("sslkey", tls.sslkey),
    ] {
        if let Some(v) = v {
            t.insert(k, v.into());
        }
    }
    Value::InlineTable(t)
}

/// The base `pageserver.toml` keys, before overrides are merged over them.
///
/// How the branch's timeline is created is deliberately absent: the controller
/// seeds `amp_storage.branch_state` at branch-create time and the pageserver
/// reads its init mode off that row.
pub fn pageserver_base(
    l: &Layout,
    yb_conn: Value,
    require_write_token: bool,
) -> Vec<(&'static str, Value)> {
    vec![
        ("pg_distrib_dir", l.pg_distrib_dir.as_str().into()),
        (
            "listen_pg_addr",
            format!("{}:{}", l.bind_host, l.pageserver_pg_port).into(),
        ),
        (
            "listen_http_addr",
            format!("{}:{}", l.bind_host, l.pageserver_http_port).into(),
        ),
        (
            "listen_grpc_addr",
            format!("{}:{}", l.bind_host, l.pageserver_grpc_port).into(),
        ),
        // The local safekeeper is always reached over loopback inside the pod,
        // whatever the public bind address is.
        ("safekeeper_pg_addr", l.safekeeper_addr().into()),
        ("control_plane_api", l.control_plane_api.clone().into()),
        ("yb_storage_conn", yb_conn),
        ("branch_id", l.branch_id.clone().into()),
        ("require_write_token", require_write_token.into()),
    ]
}

/// The base `safekeeper.toml` keys. Its `yb_storage_conn` is a *string*-encoded
/// inline table rather than a table, which is the shape its config type expects.
pub fn safekeeper_base(
    l: &Layout,
    yb_conn_str: String,
    require_write_token: bool,
) -> Vec<(&'static str, Value)> {
    vec![
        ("branch_id", l.branch_id.clone().into()),
        (
            "listen_pg",
            format!("{}:{}", l.bind_host, l.safekeeper_pg_port).into(),
        ),
        (
            "listen_http",
            format!("{}:{}", l.bind_host, l.safekeeper_http_port).into(),
        ),
        ("yb_storage_conn", yb_conn_str.into()),
        ("require_write_token", require_write_token.into()),
    ]
}

/// A probe that succeeds once the service answers `path` with a 2xx.
///
/// Every HTTP service in the pod gets one of these rather than a TCP connect: the
/// port binds before the server can serve, so connect-only would let the next
/// service start against a dependency that is not ready — and for the pageserver
/// it would report ready before its branch exists at all.
///
/// The path is per-service; the safekeeper and pageserver both serve
/// `/v1/status`.
pub fn status_probe(port: u16, path: &'static str) -> HealthCheck {
    Box::new(move || crate::coordinate::http_ok(LISTEN_HOST, port, path))
}

/// A probe that succeeds once something is listening. Used for the compute, whose
/// port speaks the Postgres wire protocol rather than HTTP.
pub fn tcp_probe(port: u16) -> HealthCheck {
    Box::new(move || {
        let addr = format!("{LISTEN_HOST}:{port}");
        addr.parse()
            .ok()
            .and_then(|a| std::net::TcpStream::connect_timeout(&a, Duration::from_millis(500)).ok())
            .is_some()
    })
}

/// How long to wait for YugabyteDB before starting the daemons anyway.
///
/// This is the pod's whole tolerance for YB being unavailable **at startup**, and
/// it has to absorb the outage on its own: once the daemons are running, a
/// yb_storage failure is handled by each daemon (the safekeeper fails the
/// connection and lets the walproposer reconnect), but at startup the pageserver
/// exits when it cannot read `amp_storage.branch_state`, and six exits inside the
/// restart window take the pod down in 15–25 s. Waiting is the only thing between
/// a brief YB outage and pod recycling, so the default is generous — minutes, to
/// cover a rolling restart or a leader election, not seconds.
pub const YB_WAIT_TIMEOUT: Duration = Duration::from_secs(300);

/// Poll interval for the wait. Cheap: one TCP connect per tick.
pub const YB_WAIT_INTERVAL: Duration = Duration::from_secs(1);

/// Blocks until YugabyteDB accepts a TCP connection, or `timeout` expires.
///
/// Call this **before** the registry override read and before starting any
/// daemon. Both have their own bounded retry, and neither budget is meant to
/// absorb a full outage: reading the overrides against a dead YB just burns its
/// attempts and boots the endpoint on derived defaults, silently dropping the
/// operator's settings.
///
/// Every daemon's first act is to open a `yb_storage` connection. Starting them
/// against an unreachable YB makes each crash on connect, and six crashes inside
/// the restart window exit the pod in well under a minute — so a YB rolling
/// restart turns into pod recycling instead of a wait. Waiting here absorbs it.
///
/// A timeout is **not** an error: the caller starts the daemons anyway, because
/// their own retry may still succeed and refusing to start is strictly worse than
/// letting them try. The return value says which happened, for the log.
pub fn wait_for_yb(host: &str, port: u16, timeout: Duration, interval: Duration) -> Ready {
    let deadline = std::time::Instant::now() + timeout;
    let mut waited = false;
    loop {
        if crate::supervise::shutdown_requested() {
            return Ready::ShuttingDown;
        }
        if probe_addr(host, port) {
            if waited {
                tracing::info!(host, port, "YugabyteDB is reachable");
            }
            return Ready::Reachable;
        }
        if std::time::Instant::now() >= deadline {
            tracing::warn!(
                host,
                port,
                timeout_secs = timeout.as_secs(),
                "YugabyteDB still unreachable; starting the daemons to let their own retry run"
            );
            return Ready::TimedOut;
        }
        if !waited {
            tracing::info!(host, port, "waiting for YugabyteDB");
            waited = true;
        }
        std::thread::sleep(interval);
    }
}

/// Outcome of [`wait_for_yb`].
#[derive(Debug, PartialEq, Eq)]
pub enum Ready {
    Reachable,
    /// The wait expired. The caller proceeds; the daemons retry on their own.
    TimedOut,
    ShuttingDown,
}

/// A connect attempt against an arbitrary host, which may be a name rather than
/// an address, so it resolves rather than parsing.
fn probe_addr(host: &str, port: u16) -> bool {
    use std::net::ToSocketAddrs;
    let Ok(addrs) = (host, port).to_socket_addrs() else {
        return false;
    };
    addrs
        .into_iter()
        .any(|a| std::net::TcpStream::connect_timeout(&a, Duration::from_millis(500)).is_ok())
}

/// The inherited variables a compute running as its own user must not see: every
/// `YB_*` connection setting, the password variable the connection names, and
/// the `cloud_admin` verifier, which only the supervisor applies.
///
/// A denylist rather than an allowlist because the chart passes externalized
/// extensions' data directories (`PROJ_DATA` and the like) under names this
/// process cannot know in advance.
pub fn storage_only_env(names: impl Iterator<Item = String>) -> Vec<String> {
    let password_env = crate::env_opt("YB_PASSWORD_ENV").unwrap_or_else(|| "YB_PASSWORD".into());
    names
        .filter(|k| k.starts_with("YB_") || k == "COMPUTE_PASSWORD" || *k == password_env)
        .collect()
}

/// Gives each user its directories before any child starts.
///
/// Owned by root with the user's group and mode `0770`, not owned by the user:
/// the supervisor keeps only `SETUID`, `SETGID`, `CHOWN` and `KILL`, so without
/// `DAC_OVERRIDE` it could not write the daemons' tomls or the compute's staged
/// certificate into a directory another uid owns `0700`. The children get in
/// through their primary group (supplementary groups are cleared), and neither
/// side can enter the other's directory.
pub fn prepare_split_dirs(l: &Layout, ids: &crate::identity::Identities) -> anyhow::Result<()> {
    use std::os::unix::fs::{PermissionsExt, chown};

    use anyhow::Context;
    std::fs::create_dir_all(l.compute_dir())
        .with_context(|| format!("create {}", l.compute_dir()))?;
    for (dir, who) in [
        (l.ps_datadir(), ids.storage),
        (l.sk_datadir(), ids.storage),
        (l.compute_dir(), ids.compute),
    ] {
        chown(&dir, Some(0), Some(who.gid)).with_context(|| format!("chown {dir}"))?;
        std::fs::set_permissions(&dir, std::fs::Permissions::from_mode(0o770))
            .with_context(|| format!("chmod {dir}"))?;
    }
    // Root's and closed to both children: the settings probe and the staged
    // certificate rely on nothing under it but their own directories being
    // writable by them.
    chown(&l.run_dir, Some(0), Some(0)).with_context(|| format!("chown {}", l.run_dir))?;
    std::fs::set_permissions(&l.run_dir, std::fs::Permissions::from_mode(0o755))
        .with_context(|| format!("chmod {}", l.run_dir))?;
    Ok(())
}

/// The compute's argv.
///
/// Every flag here is one `compute-ctl` actually declares. The spec file is
/// `--config`; `--compute-id` is a required `String` with no default, so omitting
/// it fails clap parsing and the process exits before it does anything. `--dev`
/// skips the VM-specific operations (killing pgbouncer and local_proxy on the way
/// down) — neither exists in a container nor under `yb_amp_local`.
pub fn compute_ctl_argv(l: &Layout) -> Vec<String> {
    let mut argv = vec![
        l.bin_dir.join("compute-ctl").to_string(),
        "--pgdata".into(),
        l.pgdata().to_string(),
        "--connstr".into(),
        format!(
            "postgresql://cloud_admin@{LISTEN_HOST}:{}/postgres",
            l.compute_port
        ),
        "--pgbin".into(),
        l.pg_bin().join("postgres").to_string(),
        "--compute-id".into(),
        l.compute_id.clone(),
        "--config".into(),
        l.compute_spec().to_string(),
        "--external-http-port".into(),
        l.compute_ctl_http_port.to_string(),
        "--internal-http-port".into(),
        l.compute_ctl_internal_http_port.to_string(),
        "--dev".into(),
    ];
    if let Some(role) = &l.privileged_role_name {
        argv.push("--privileged-role-name".into());
        argv.push(role.clone());
    }
    argv
}

/// The three services, in the order they must start.
///
/// The order is a real dependency chain, not a preference: `compute_ctl`'s first
/// act is to fetch a basebackup from the pageserver, and the pageserver streams
/// WAL from the safekeeper.
///
/// `compute_ctl` is deliberately **last**, because the pageserver/safekeeper
/// handshake in [`crate::coordinate`] has to run between the pageserver coming up
/// and the compute starting. The caller starts the first two, coordinates, then
/// starts the compute.
pub fn services(l: &Layout) -> Vec<Service> {
    // Each daemon's log, at the path `yb_amp_local` and the integration suite
    // already read. `None` in a pod, where one interleaved stream is what
    // `kubectl logs` shows.
    let log = |dir: Utf8PathBuf, name: &str| l.log_files.then(|| dir.join(format!("{name}.log")));
    vec![
        Service::new(
            "safekeeper",
            vec![
                l.bin_dir.join("safekeeper").to_string(),
                "-D".into(),
                l.sk_datadir().to_string(),
            ],
        )
        .cwd(l.sk_datadir())
        .run_as(l.identities.map(|i| i.storage))
        .health(status_probe(l.safekeeper_http_port, "/v1/status"))
        .logging_to(log(l.sk_datadir(), "safekeeper")),
        Service::new(
            "pageserver",
            vec![
                l.bin_dir.join("pageserver").to_string(),
                "-D".into(),
                l.ps_datadir().to_string(),
            ],
        )
        .cwd(l.ps_datadir())
        .run_as(l.identities.map(|i| i.storage))
        .health(status_probe(l.pageserver_http_port, "/v1/status"))
        .logging_to(log(l.ps_datadir(), "pageserver")),
        Service::new(COMPUTE_SERVICE, compute_ctl_argv(l))
            .cwd(match l.identities {
                Some(_) => l.compute_dir(),
                None => l.run_dir.clone(),
            })
            .run_as(l.identities.map(|i| i.compute))
            .without_env(match l.identities {
                Some(_) => storage_only_env(std::env::vars().map(|(k, _)| k)),
                None => Vec::new(),
            })
            .health(tcp_probe(l.compute_port))
            .logging_to(log(l.run_dir.clone(), "compute")),
    ]
}

/// The LFC state server ([`crate::lfc_proxy`]): how the compute reaches its
/// prewarm state without a YugabyteDB credential. Runs as the storage user when
/// the users are split, with the storage credentials it inherits.
pub fn lfc_state_service(
    l: &Layout,
    endpoint_id: Option<&str>,
    storage_json: &str,
) -> Option<Service> {
    let port = l.lfc_state_port?;
    let mut svc = Service::new(
        LFC_STATE_SERVICE,
        vec![
            l.bin_dir.join("endpoint-supervisor").to_string(),
            crate::lfc_proxy::SUBCOMMAND.into(),
        ],
    )
    .cwd(l.ps_datadir())
    .run_as(l.identities.map(|i| i.storage))
    .env("LFC_STATE_PORT", &port.to_string())
    .env("LFC_STATE_BRANCH_ID", &l.branch_id)
    .env("LFC_STATE_STORAGE", storage_json)
    .health(tcp_probe(port));
    if let Some(id) = endpoint_id {
        svc = svc.env("LFC_STATE_ENDPOINT_ID", id);
    }
    Some(svc)
}

pub const LFC_STATE_SERVICE: &str = "lfc_state";

#[cfg(test)]
mod tests {
    use super::*;

    fn layout() -> Layout {
        Layout {
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
            bind_host: "0.0.0.0".into(),
            control_plane_api: "http://localhost:6666".into(),
            compute_id: COMPUTE_ID.into(),
            privileged_role_name: None,
            log_files: false,
            identities: None,
            lfc_state_port: Some(9896),
        }
    }

    /// The compute must not inherit anything that reaches the foundation.
    #[test]
    fn the_compute_loses_every_foundation_variable() {
        let names = [
            "YB_PASSWORD",
            "YB_SSLKEY",
            "YB_HOST_PORTS",
            "COMPUTE_PASSWORD",
            "PROJ_DATA",
            "PATH",
        ]
        .map(String::from);
        let mut gone = storage_only_env(names.into_iter());
        gone.sort();
        assert_eq!(
            gone,
            [
                "COMPUTE_PASSWORD",
                "YB_HOST_PORTS",
                "YB_PASSWORD",
                "YB_SSLKEY"
            ]
        );
    }

    #[test]
    fn split_mode_moves_pgdata_under_the_compute_dir() {
        let mut l = layout();
        assert_eq!(l.pgdata(), "/data/pgdata");
        l.identities = Some(crate::identity::Identities {
            storage: crate::identity::RunAs {
                uid: 1000,
                gid: 1000,
            },
            compute: crate::identity::RunAs {
                uid: 1001,
                gid: 1001,
            },
        });
        assert_eq!(l.pgdata(), "/data/compute/pgdata");
    }

    /// The order is the dependency chain; a reshuffle here breaks compute startup.
    #[test]
    fn services_start_in_dependency_order() {
        let s = services(&layout());
        let names: Vec<&str> = s.iter().map(|x| x.name.as_str()).collect();
        assert_eq!(names, ["safekeeper", "pageserver", "compute_ctl"]);
    }

    /// With the users split, each child runs as its own user from its own
    /// directory, and only the compute loses the storage variables.
    #[test]
    fn a_split_runs_each_child_as_its_user() {
        use crate::identity::{Identities, RunAs};
        let ids = Identities {
            storage: RunAs {
                uid: 1000,
                gid: 1000,
            },
            compute: RunAs {
                uid: 1001,
                gid: 1001,
            },
        };
        let mut l = layout();
        l.identities = Some(ids);
        let s = services(&l);
        let get = |name: &str| {
            s.iter()
                .find(|x| x.name == name)
                .unwrap()
                .identity_for_test()
        };

        for (name, dir) in [
            ("safekeeper", l.sk_datadir()),
            ("pageserver", l.ps_datadir()),
        ] {
            let (run_as, cwd, removed) = get(name);
            assert_eq!(run_as, Some(ids.storage), "{name}");
            assert_eq!(cwd.unwrap(), dir.as_std_path(), "{name}");
            assert!(removed.is_empty(), "{name} lost {removed:?}");
        }

        let (run_as, cwd, removed) = get(COMPUTE_SERVICE);
        assert_eq!(run_as, Some(ids.compute));
        assert_eq!(cwd.unwrap(), l.compute_dir().as_std_path());
        assert_eq!(
            removed,
            storage_only_env(std::env::vars().map(|(k, _)| k)).as_slice()
        );
        let argv = s
            .iter()
            .find(|x| x.name == COMPUTE_SERVICE)
            .unwrap()
            .argv_for_test();
        let pgdata = &argv[argv.iter().position(|a| a == "--pgdata").unwrap() + 1];
        assert_eq!(pgdata, l.compute_dir().join("pgdata").as_str());
    }

    /// The LFC state server runs whenever it has a port, as the storage user
    /// when the users are split.
    #[test]
    fn the_lfc_state_server_runs_as_the_storage_user() {
        use crate::identity::{Identities, RunAs};
        let mut l = layout();
        l.lfc_state_port = None;
        assert!(lfc_state_service(&l, Some("ep"), "{}").is_none());
        l.lfc_state_port = Some(9896);
        let unsplit = lfc_state_service(&l, Some("ep"), "{}").unwrap();
        assert_eq!(unsplit.identity_for_test().0, None);
        let ids = Identities {
            storage: RunAs {
                uid: 1000,
                gid: 1000,
            },
            compute: RunAs {
                uid: 1001,
                gid: 1001,
            },
        };
        l.identities = Some(ids);
        let svc = lfc_state_service(&l, Some("ep"), "{}").unwrap();
        assert_eq!(svc.identity_for_test().0, Some(ids.storage));
        assert_eq!(
            svc.argv_for_test(),
            [
                "/usr/local/bin/endpoint-supervisor",
                crate::lfc_proxy::SUBCOMMAND
            ]
        );
    }

    /// Unsplit, every child inherits this process's identity and environment.
    #[test]
    fn unsplit_children_inherit_everything() {
        for svc in services(&layout()) {
            let (run_as, _, removed) = svc.identity_for_test();
            assert_eq!(run_as, None, "{}", svc.name);
            assert!(removed.is_empty(), "{}", svc.name);
        }
    }

    /// The image lays Postgres out under `<pg_distrib_dir>/v<major>`, so every
    /// path derived from it must carry the version. Flattening it to
    /// `<pg_distrib_dir>/bin` produced a pod that crash-looped: `compute_ctl`
    /// panicked on a missing `pg_config`, and `remote_auth` could not find
    /// `psql`. Neither failure names the path, so this is pinned here.
    #[test]
    fn the_postgres_paths_are_version_qualified() {
        let l = layout();
        assert_eq!(l.pg_bin(), "/usr/local/v18/bin");
        assert_eq!(l.pg_lib(), "/usr/local/v18/lib");
        // The daemons' own `pg_distrib_dir` stays the *parent*: the pageserver
        // appends the version itself.
        assert_eq!(l.pg_distrib_dir, "/usr/local");

        let s = services(&l);
        let compute = s.iter().find(|x| x.name == "compute_ctl").unwrap();
        let argv = compute.argv_for_test();
        let pgbin = argv[argv.iter().position(|a| a == "--pgbin").unwrap() + 1].clone();
        assert_eq!(pgbin, "/usr/local/v18/bin/postgres", "{argv:?}");
    }

    /// Every flag in the compute's argv must be one `compute-ctl` declares, and
    /// the required ones must be present. A wrong flag name or a missing
    /// `--compute-id` fails clap parsing, so the process exits before it does any
    /// work and the pod crash-loops — invisible to a test that only checks the
    /// service list.
    #[test]
    fn the_compute_argv_matches_the_flags_compute_ctl_declares() {
        let l = layout();
        let s = services(&l);
        let compute = s.iter().find(|x| x.name == "compute_ctl").unwrap();
        let argv = compute.argv_for_test();

        // The spec file is --config. --spec-path does not exist.
        assert!(argv.contains(&"--config".to_string()), "{argv:?}");
        assert!(!argv.iter().any(|a| a == "--spec-path"), "{argv:?}");
        assert_eq!(
            argv[argv.iter().position(|a| a == "--config").unwrap() + 1],
            "/data/compute_spec.json"
        );

        // Required with no default: omitting it is a parse failure.
        assert!(argv.contains(&"--compute-id".to_string()), "{argv:?}");
        assert_eq!(
            argv[argv.iter().position(|a| a == "--compute-id").unwrap() + 1],
            COMPUTE_ID
        );

        // The internal port is the external one plus one.
        let ext = argv
            .iter()
            .position(|a| a == "--external-http-port")
            .map(|i| argv[i + 1].clone())
            .expect("--external-http-port");
        let int = argv
            .iter()
            .position(|a| a == "--internal-http-port")
            .map(|i| argv[i + 1].clone())
            .expect("--internal-http-port");
        assert_eq!(ext, l.compute_ctl_http_port.to_string());
        assert_eq!(
            int.parse::<u16>().unwrap(),
            l.compute_ctl_http_port + 1,
            "internal must be external+1"
        );

        assert!(argv.contains(&"--dev".to_string()), "{argv:?}");
    }

    /// `yb_amp_local` and the integration suite read each daemon's log at a fixed
    /// path — `endpoints/<id>/pageserver/pageserver.log` and its siblings. A pod
    /// wants one interleaved stream instead, so the paths are only attached when
    /// the caller asked for them.
    #[test]
    fn per_service_logs_land_where_the_launcher_reads_them() {
        let mut l = layout();
        l.run_dir = "/repo/endpoints/main".into();
        assert!(
            services(&l).iter().all(|s| s.log_file_for_test().is_none()),
            "a pod must keep inheriting stdout"
        );

        l.log_files = true;
        let got: Vec<_> = services(&l)
            .iter()
            .map(|s| {
                (
                    s.name.clone(),
                    s.log_file_for_test().map(|p| p.display().to_string()),
                )
            })
            .collect();
        assert_eq!(
            got,
            vec![
                (
                    "safekeeper".to_string(),
                    Some("/repo/endpoints/main/safekeeper/safekeeper.log".to_string())
                ),
                (
                    "pageserver".to_string(),
                    Some("/repo/endpoints/main/pageserver/pageserver.log".to_string())
                ),
                (
                    "compute_ctl".to_string(),
                    Some("/repo/endpoints/main/compute.log".to_string())
                ),
            ]
        );
    }

    /// `yb_amp_local` runs many endpoints on one host: each compute needs its own
    /// identity, and its two HTTP ports come from a free-port search rather than
    /// being contiguous. Deriving either one would collide.
    #[test]
    fn the_caller_can_name_the_compute_and_place_its_ports() {
        let mut l = layout();
        l.compute_id = "ep_main".into();
        l.compute_ctl_internal_http_port = 47119;
        l.privileged_role_name = Some("yb_amp_admin".into());

        let argv = compute_ctl_argv(&l);
        let flag = |name: &str| {
            argv.iter()
                .position(|a| a == name)
                .map(|i| argv[i + 1].clone())
        };
        assert_eq!(flag("--compute-id").as_deref(), Some("ep_main"));
        assert_eq!(flag("--internal-http-port").as_deref(), Some("47119"));
        assert_eq!(flag("--external-http-port").as_deref(), Some("3080"));
        assert_eq!(
            flag("--privileged-role-name").as_deref(),
            Some("yb_amp_admin")
        );
    }

    /// The pod passes no privileged role, and an empty flag is not the same as an
    /// absent one — `compute-ctl` would take the empty string as the role name.
    #[test]
    fn no_privileged_role_means_no_flag() {
        let argv = compute_ctl_argv(&layout());
        assert!(
            !argv.iter().any(|a| a == "--privileged-role-name"),
            "{argv:?}"
        );
    }

    /// The daemons bind the public address, but the compute and the probes reach
    /// them over loopback — which a wildcard listener also accepts.
    #[test]
    fn daemons_bind_public_while_internal_wiring_stays_loopback() {
        let l = layout();
        let base = pageserver_base(&l, Value::from("x"), true);
        let get = |k: &str| {
            base.iter()
                .find(|(n, _)| *n == k)
                .map(|(_, v)| v.to_string().trim().trim_matches('"').to_string())
                .unwrap()
        };
        assert_eq!(get("listen_pg_addr"), "0.0.0.0:64000");
        assert_eq!(get("safekeeper_pg_addr"), "127.0.0.1:5454");
        assert_eq!(l.pageserver_connstring(), "host=127.0.0.1 port=64000");
    }

    #[test]
    fn the_yb_conn_table_omits_absent_optionals() {
        let v = yb_conn_inline("h:5433", "yugabyte", "yb_storage", YbTls::default(), None);
        let s = v.to_string();
        assert!(s.contains("host_ports = \"h:5433\""), "{s}");
        assert!(!s.contains("ssl"), "{s}");
        let v = yb_conn_inline(
            "h:5433",
            "amp_storage",
            "yb_storage",
            YbTls {
                sslmode: Some("verify-full"),
                sslrootcert: Some("/ca.crt"),
                sslcert: Some("/tls/tls.crt"),
                sslkey: Some("/tls/tls.key"),
            },
            Some("YB_PASSWORD"),
        );
        let s = v.to_string();
        assert!(s.contains("sslmode = \"verify-full\""), "{s}");
        assert!(s.contains("sslcert = \"/tls/tls.crt\""), "{s}");
        assert!(s.contains("sslkey = \"/tls/tls.key\""), "{s}");
        assert!(s.contains("password_env = \"YB_PASSWORD\""), "{s}");
    }

    #[test]
    fn paths_hang_off_the_run_dir() {
        let l = layout();
        assert_eq!(l.pgdata(), "/data/pgdata");
        assert_eq!(l.ps_datadir(), "/data/pageserver");
        assert_eq!(l.sk_datadir(), "/data/safekeeper");
    }

    /// A probe against a port nothing listens on must report unhealthy rather
    /// than hanging, or the health gate would never time out.
    #[test]
    fn a_probe_on_a_dead_port_is_false() {
        // Port 1 is privileged and unbound in any sane test environment.
        assert!(!tcp_probe(1)());
    }

    /// An unreachable YB must time out and hand control back, not block forever
    /// and not report reachable — the caller starts the daemons either way.
    #[test]
    fn waiting_for_an_unreachable_yb_times_out_and_returns() {
        let started = std::time::Instant::now();
        let got = wait_for_yb(
            "127.0.0.1",
            1,
            Duration::from_millis(150),
            Duration::from_millis(10),
        );
        assert_eq!(got, Ready::TimedOut);
        assert!(
            started.elapsed() < Duration::from_secs(5),
            "took {:?}",
            started.elapsed()
        );
    }

    /// The happy path returns as soon as something accepts, without waiting out
    /// the timeout.
    #[test]
    fn waiting_for_a_reachable_yb_returns_immediately() {
        let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
        let port = listener.local_addr().unwrap().port();
        let started = std::time::Instant::now();
        let got = wait_for_yb(
            "127.0.0.1",
            port,
            Duration::from_secs(30),
            Duration::from_millis(10),
        );
        assert_eq!(got, Ready::Reachable);
        assert!(
            started.elapsed() < Duration::from_secs(5),
            "took {:?}",
            started.elapsed()
        );
    }

    /// A hostname that does not resolve must be treated as unreachable rather
    /// than panicking on an address parse.
    #[test]
    fn an_unresolvable_host_is_not_reachable() {
        assert!(!probe_addr("no-such-host.invalid", 5433));
    }

    /// The startup tolerance for YB being down. The wait is the only thing
    /// absorbing it: the restart budget takes the pod down in 15–25 s once the
    /// daemons start failing, so this must be comfortably larger than any outage
    /// we intend to ride out. Pinned so shortening it is a deliberate act.
    #[test]
    fn the_yb_wait_absorbs_a_realistic_outage() {
        // A rolling restart or leader election, with margin.
        assert!(
            YB_WAIT_TIMEOUT >= Duration::from_secs(120),
            "YB_WAIT_TIMEOUT is {YB_WAIT_TIMEOUT:?}, too short to ride out a restart"
        );
        // The poll must be frequent enough that recovery is prompt, not rounded
        // up to a large fraction of the outage.
        assert!(YB_WAIT_INTERVAL <= Duration::from_secs(2));
    }
}
