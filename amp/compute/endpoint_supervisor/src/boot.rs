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
    pub safekeeper_http_port: u16,
    /// `compute_ctl`'s own HTTP surface. Its internal port is this plus one.
    pub compute_ctl_http_port: u16,
    /// Postgres major version. The install is version-qualified inside
    /// `pg_distrib_dir`, so this is needed to find `bin/` and `lib/`.
    pub pg_version_major: u32,
    /// `0.0.0.0` when the controller must reach in, loopback otherwise.
    pub bind_host: String,
    pub control_plane_api: String,
}

/// Loopback is what the compute and the supervisor's own probes use, even when
/// the daemons bind `0.0.0.0` — a wildcard listener accepts it too.
pub const LISTEN_HOST: &str = "127.0.0.1";

/// The identity `compute_ctl` reports itself under. One compute per pod, so a
/// fixed name is enough — but the flag is required, not optional.
pub const COMPUTE_ID: &str = "yb-amp-endpoint";

impl Layout {
    pub fn ps_datadir(&self) -> Utf8PathBuf {
        self.run_dir.join("pageserver")
    }
    pub fn sk_datadir(&self) -> Utf8PathBuf {
        self.run_dir.join("safekeeper")
    }
    pub fn pgdata(&self) -> Utf8PathBuf {
        self.run_dir.join("pgdata")
    }
    /// The Postgres binaries: `<pg_distrib_dir>/v<major>/bin`.
    ///
    /// Version-qualified, because that is how the image lays the install out —
    /// `pg_distrib_dir` itself holds one directory per major version. Flattening
    /// this to `<pg_distrib_dir>/bin` finds nothing, and the failure is indirect:
    /// `compute_ctl` panics on a missing `pg_config` and the registry read cannot
    /// find `psql`.
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
    pub fn safekeeper_addr(&self) -> String {
        format!("{LISTEN_HOST}:{}", self.safekeeper_pg_port)
    }
}

/// The YB connection as the inline toml table both daemons expect.
pub fn yb_conn_inline(
    host_ports: &str,
    user: &str,
    dbname: &str,
    sslmode: Option<&str>,
    sslrootcert: Option<&str>,
    password_env: Option<&str>,
) -> Value {
    let mut t = InlineTable::new();
    t.insert("host_ports", host_ports.into());
    t.insert("user", user.into());
    t.insert("dbname", dbname.into());
    for (k, v) in [
        ("password_env", password_env),
        ("sslmode", sslmode),
        ("sslrootcert", sslrootcert),
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
pub fn pageserver_base(l: &Layout, yb_conn: Value) -> Vec<(&'static str, Value)> {
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
    ]
}

/// The base `safekeeper.toml` keys. Its `yb_storage_conn` is a *string*-encoded
/// inline table rather than a table, which is the shape its config type expects.
pub fn safekeeper_base(l: &Layout, yb_conn_str: String) -> Vec<(&'static str, Value)> {
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
    vec![
        Service::new(
            "safekeeper",
            vec![
                l.bin_dir.join("yb-safekeeper").to_string(),
                "-D".into(),
                l.sk_datadir().to_string(),
            ],
        )
        .cwd(l.sk_datadir())
        .health(status_probe(l.safekeeper_http_port, "/v1/status")),
        Service::new(
            "pageserver",
            vec![
                l.bin_dir.join("yb-pageserver").to_string(),
                "-D".into(),
                l.ps_datadir().to_string(),
            ],
        )
        .cwd(l.ps_datadir())
        .health(status_probe(l.pageserver_http_port, "/v1/status")),
        // Every flag here is one `yb-compute-ctl` actually declares. The spec file
        // is `--config`; `--compute-id` is a required `String` with no default, so
        // omitting it fails clap parsing and the process exits before it does
        // anything. `--dev` skips the VM-specific operations that have no meaning
        // in a container.
        Service::new(
            "compute_ctl",
            vec![
                l.bin_dir.join("yb-compute-ctl").to_string(),
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
                COMPUTE_ID.into(),
                "--config".into(),
                l.run_dir.join("compute_spec.json").to_string(),
                "--external-http-port".into(),
                l.compute_ctl_http_port.to_string(),
                "--internal-http-port".into(),
                (l.compute_ctl_http_port + 1).to_string(),
                "--dev".into(),
            ],
        )
        .cwd(l.run_dir.clone())
        .health(tcp_probe(l.compute_port)),
    ]
}

#[cfg(test)]
mod tests {
    use super::*;

    fn layout() -> Layout {
        Layout {
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
            bind_host: "0.0.0.0".into(),
            control_plane_api: "http://localhost:6666".into(),
        }
    }

    /// The order is the dependency chain; a reshuffle here breaks compute startup.
    #[test]
    fn services_start_in_dependency_order() {
        let s = services(&layout());
        let names: Vec<&str> = s.iter().map(|x| x.name.as_str()).collect();
        assert_eq!(names, ["safekeeper", "pageserver", "compute_ctl"]);
    }

    /// The image lays Postgres out under `<pg_distrib_dir>/v<major>`, so every
    /// path derived from it must carry the version. Flattening it to
    /// `<pg_distrib_dir>/bin` produced a pod that crash-looped: `compute_ctl`
    /// panicked on a missing `pg_config` and the registry read could not find
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

    /// Every flag in the compute's argv must be one `yb-compute-ctl` declares, and
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

    /// The daemons bind the public address, but the compute and the probes reach
    /// them over loopback — which a wildcard listener also accepts.
    #[test]
    fn daemons_bind_public_while_internal_wiring_stays_loopback() {
        let l = layout();
        let base = pageserver_base(&l, Value::from("x"));
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
        let v = yb_conn_inline("h:5433", "yugabyte", "yb_storage", None, None, None);
        let s = v.to_string();
        assert!(s.contains("host_ports = \"h:5433\""), "{s}");
        assert!(!s.contains("sslmode"), "{s}");
        let v = yb_conn_inline(
            "h:5433",
            "yugabyte",
            "yb_storage",
            Some("require"),
            Some("/ca.crt"),
            Some("YB_PASSWORD"),
        );
        let s = v.to_string();
        assert!(s.contains("sslmode = \"require\""), "{s}");
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
