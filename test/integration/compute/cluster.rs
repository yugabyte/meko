//! The shared-cluster fixture. Owns the suite lifecycle: ephemeral-db
//! setup, `init`/`start`, one shared `main` branch + endpoint, the
//! per-scenario cluster recycle, the corruption-marker log gate, and
//! teardown: a passing run is fully removed; a failed run stops the cluster
//! but keeps its `*.log` files (`--no-fail-stop` instead leaves the failed
//! cluster running for postmortem).

use std::future::Future;
use std::io::Write;
use std::path::{Path, PathBuf};
use std::process::Command;
use std::sync::{Mutex, OnceLock};
use std::time::{Duration, Instant, SystemTime};

use anyhow::{Context, bail};
use regex::Regex;

static RUN_LOG: OnceLock<Mutex<std::fs::File>> = OnceLock::new();

/// Wall-clock instant of the last harness progress signal. The
/// `run_scenario` watchdog treats a wedge as *no progress for `budget`*,
/// not *total runtime exceeds `budget`* — so a legitimately long scenario
/// (e.g. `torn_wal_write`'s full 6×7 crash matrix, which makes steady,
/// corruption-free progress over several minutes) isn't failed just for
/// taking a while, while a genuine hang (no step/output at all) still
/// trips fast. Every [`tee_line`] (step banners, PASS lines, scenario
/// `tee_println!`s) counts as progress.
static LAST_PROGRESS: OnceLock<Mutex<Instant>> = OnceLock::new();

fn progress_cell() -> &'static Mutex<Instant> {
    LAST_PROGRESS.get_or_init(|| Mutex::new(Instant::now()))
}

/// Record forward progress now. Called from [`tee_line`]; also call
/// directly to reset the wedge clock at a scenario boundary.
pub fn mark_progress() {
    *progress_cell().lock().unwrap() = Instant::now();
}

/// Time since the last [`mark_progress`].
fn since_progress() -> Duration {
    progress_cell().lock().unwrap().elapsed()
}

/// Write `s` to stdout and, if initialized, to the run log file.
pub fn tee_line(s: &str) {
    mark_progress();
    println!("{s}");
    if let Some(f) = RUN_LOG.get() {
        let _ = writeln!(f.lock().unwrap(), "{s}");
    }
}

/// Drop-in for `println!` that also writes to the run log.
#[macro_export]
macro_rules! tee_println {
    () => { $crate::cluster::tee_line("") };
    ($($arg:tt)*) => { $crate::cluster::tee_line(&format!($($arg)*)) };
}

/// Corruption markers scanned in the pageserver log after every
/// scenario. First hit aborts the run.
pub const MARKERS: &str = "MATERIALIZER REDO ERROR|ABORT|PANIC|early eof|specified item offset|image row at lsn|deserialize error|WRITE-PATH ROUNDTRIP";

/// Fatal markers polled across ALL cluster logs (pageserver + safekeeper)
/// *while a test runs*, so a wedge fails fast instead of hanging until a
/// timeout. The safekeeper's interpreted-WAL-reader decode panic
/// (`critical_timeline!`) is the prime case: it aborts the WAL sender, the
/// PS then loops forever in `wait_lsn` and the test silently stalls (#61).
pub const FATAL_MARKERS: &str = r"interpreted wal reader: failed to decode|invalid xlog page header|panicked at|thread '[^']*' panicked";

/// Print a step banner to stdout and the run log.
pub fn step(msg: &str) {
    tee_line("");
    tee_line(&format!("=== {msg} ==="));
}

pub struct Cluster {
    /// Path to the `yb_light_local` binary.
    yb_light: PathBuf,
    /// Repo root (`git rev-parse --show-toplevel`).
    repo: PathBuf,
    /// `YB_LIGHT_REPO_DIR` — this run's on-disk cluster directory.
    repo_dir: PathBuf,
    /// Ephemeral YugabyteDB database name for this run.
    db_name: String,
    yb_host: String,
    yb_port: u16,
    /// `YB_HOT_PATH` — hot-path transport for the run (`ysql` | `tsclient`),
    /// forwarded to `init --yb-hot-path` when set; unset runs the config
    /// default (tsclient).
    yb_hot_path: String,
    /// Free port-block base for this run (`init --port-base`), relocating
    /// the whole cluster off the defaults so concurrent runs (and a
    /// developer's local cluster) never collide. Auto-picked free, or
    /// pinned via `INTEGRATION_PORT_BASE`.
    port_base: u16,
    /// Compute port of the shared `main` endpoint.
    compute_port: u16,
    /// The shared `main` branch id, set by [`Cluster::setup`].
    branch: String,
    /// Regex form of [`MARKERS`].
    markers: Regex,
    /// Regex form of [`FATAL_MARKERS`] (polled across all cluster logs
    /// during a test so a wedge fails fast).
    fatal_markers: Regex,
    /// Set by [`Cluster::mark_success`] at the end of a fully-passing
    /// run. On success teardown removes the whole run dir; on failure it
    /// stops the cluster but keeps the run's `*.log` files (see
    /// `no_fail_stop` for the postmortem opt-in).
    success: bool,
    /// `--no-fail-stop` (env `INTEGRATION_NO_FAIL_STOP`): on failure, leave the
    /// cluster + db running for postmortem instead of the default
    /// stop-the-cluster-but-keep-logs.
    no_fail_stop: bool,
}

impl Cluster {
    /// The shared `main` branch id (as a hex string). This is the single
    /// flat-model identifier the former `(tenant_id, timeline_id)` pair
    /// collapsed to: it keys the branch's rows in every YB table and
    /// addresses it in CLI / HTTP / wire calls.
    pub fn branch(&self) -> &str {
        &self.branch
    }
    pub fn compute_port(&self) -> u16 {
        self.compute_port
    }
    /// HTTP port of pageserver node 1 (`port_base + 400`; node *i* adds
    /// *i*). Use this instead of hardcoding the old default 9989 — under
    /// the relocated port block that port is dead.
    pub fn ps_http_port(&self) -> u16 {
        self.port_base + 400
    }
    /// gRPC page_service port of pageserver node 1 (`port_base + 500`;
    /// node *i* adds *i*). This is the port the `direct_getpage` scenario
    /// connects to in order to issue GetPage@LSN directly to the PS.
    pub fn ps_grpc_port(&self) -> u16 {
        self.port_base + 500
    }
    /// Postgres replication port of safekeeper node 1 (`port_base + 100`).
    /// The `sk_wal_*` scenarios connect here to read WAL directly via the
    /// libpq replication protocol (`START_REPLICATION PHYSICAL`).
    pub fn sk_pg_port(&self) -> u16 {
        self.port_base + 100
    }
    /// HTTP port of safekeeper node 1 (`port_base + 200`). Scenarios use
    /// it for `/v1/failpoints` and the metrics endpoint.
    pub fn sk_http_port(&self) -> u16 {
        self.port_base + 200
    }
    /// The branch id of the shared `main` branch, as a hex string.
    /// Public wrapper over the PS branch listing for scenarios that need
    /// to address the branch directly (e.g. the gRPC page client).
    pub async fn branch_id(&self) -> anyhow::Result<String> {
        self.main_branch_id().await
    }
    pub fn repo_dir(&self) -> &Path {
        &self.repo_dir
    }
    pub fn db_name(&self) -> &str {
        &self.db_name
    }
    pub fn yb_host(&self) -> &str {
        &self.yb_host
    }
    pub fn yb_port(&self) -> u16 {
        self.yb_port
    }

    /// Build the fixture and bring the cluster up from scratch:
    /// `clean.sh`, `init`, `start`, then one shared tenant + `main`
    /// endpoint.
    pub async fn setup() -> anyhow::Result<Cluster> {
        let repo = git_toplevel()?;
        let test_dir = env_or(&["TEST_DIR"], &repo.join(".integration-tests").to_string_lossy());
        let test_dir = PathBuf::from(test_dir);

        // GC run dirs older than 7 days under .integration-tests: a passing run
        // leaves a logs-only `run_<id>/`, a failed one its full dir.
        gc_old_runs(&test_dir);

        let run_id = run_id();
        let yb_light_repo_dir_env = std::env::var("YB_LIGHT_REPO_DIR")
            .ok()
            .filter(|v| !v.is_empty());
        let repo_dir = match &yb_light_repo_dir_env {
            Some(d) => PathBuf::from(d),
            None => test_dir.join(format!("run_{run_id}")),
        };
        std::fs::create_dir_all(&repo_dir)
            .with_context(|| format!("creating {}", repo_dir.display()))?;

        // Open the run log as a SIBLING of the run dir under test_dir,
        // named after the run dir, so a teardown/`clean.sh` that wipes the
        // run dir never deletes the log (a failed run keeps its log for
        // postmortem). Holds whether the run dir was auto-derived or pinned
        // via YB_LIGHT_REPO_DIR (e.g. by run.sh, whose cleanup trap targets
        // this exact run).
        let run_dir_name = repo_dir
            .file_name()
            .map(|s| s.to_string_lossy().into_owned())
            .unwrap_or_else(|| format!("run_{run_id}"));
        let log_path = test_dir.join(format!("{run_dir_name}.log"));
        if let Ok(f) = std::fs::File::create(&log_path) {
            let _ = RUN_LOG.set(Mutex::new(f));
            println!("run log: {}", log_path.display());
        }

        let yb_host = env_or(&["YB_HOST"], "127.0.0.1");
        let yb_port: u16 = env_or(&["YB_PORT"], "5433").parse().context("YB_PORT")?;
        let yb_hot_path = env_or(&["YB_HOT_PATH"], "");

        let db_name = match std::env::var("YB_DBNAME") {
            Ok(d) if !d.is_empty() => d,
            _ => {
                let pid = std::process::id();
                let nanos = SystemTime::now()
                    .duration_since(SystemTime::UNIX_EPOCH)
                    .map(|d| d.subsec_nanos())
                    .unwrap_or(0);
                format!("yb_storage_integration_{pid}_{nanos}")
            }
        };

        // Every run synthesizes its own config (`yb_light_local init
        // --yb-dbname <ephemeral> --port-base <free>`) and relocates the
        // whole cluster into a free port block, so runs are isolated by
        // construction and can execute concurrently. `main` lives in the
        // compute lane at base + COMPUTE_LANE. `INTEGRATION_PORT_BASE` pins the block;
        // otherwise a free one is auto-picked.
        let yb_light = match std::env::var("YB_LIGHT") {
            Ok(p) if !p.is_empty() => PathBuf::from(p),
            _ => repo.join("target/debug/yb_light_local"),
        };
        if !yb_light.exists() {
            bail!(
                "yb_light_local binary not found at {}; run 'cargo build --bin yb_light_local'",
                yb_light.display()
            );
        }

        let mut cluster = Cluster {
            yb_light,
            repo,
            repo_dir,
            db_name,
            yb_host,
            yb_port,
            yb_hot_path,
            port_base: 0,
            compute_port: 0,
            branch: String::new(),
            markers: Regex::new(MARKERS).expect("static marker regex"),
            fatal_markers: Regex::new(FATAL_MARKERS).expect("static fatal-marker regex"),
            success: false,
            no_fail_stop: std::env::var("INTEGRATION_NO_FAIL_STOP").is_ok_and(|v| !v.is_empty()),
        };

        // Bring the stack up in a free, below-ephemeral port block. The
        // free-block probe is advisory and can lose a race to a concurrent
        // run that grabs the same block between probe and bind, so on a
        // bring-up failure we tear down and retry in a fresh block. This is
        // what lets several integration runs (and the dev cluster) coexist.
        let mut tried: Vec<u16> = Vec::new();
        let mut last_err: Option<anyhow::Error> = None;
        for attempt in 1..=PORT_BLOCK_ATTEMPTS {
            let port_base = pick_port_base(&tried);
            tried.push(port_base);
            cluster.port_base = port_base;
            cluster.compute_port = port_base + COMPUTE_LANE;
            cluster.branch = String::new();
            match cluster.bring_up() {
                Ok(()) => return Ok(cluster),
                Err(e) => {
                    tee_println!(
                        "cluster bring-up in port block {port_base} failed \
                         (attempt {attempt}/{PORT_BLOCK_ATTEMPTS}): {e:#}"
                    );
                    let _ = cluster.clean();
                    last_err = Some(e);
                }
            }
        }
        Err(last_err.unwrap_or_else(|| anyhow::anyhow!("no port block tried")))
            .context("cluster bring-up failed in every candidate port block")
    }

    /// `clean` + `init` + `start` + one shared tenant + the `main` endpoint,
    /// all in the cluster's currently-selected port block. Split out of
    /// [`Cluster::setup`] so a lost port-block race can be retried in a
    /// fresh block without rebuilding the whole fixture.
    fn bring_up(&mut self) -> anyhow::Result<()> {
        // clean leftover state, then init + start the stack.
        self.clean()?;

        step(&format!(
            "yb_light_local init + start (db={})",
            self.db_name
        ));
        let yb_port = self.yb_port.to_string();
        let port_base = self.port_base.to_string();
        let mut init_args = vec![
            "init",
            "--yb-dbname",
            &self.db_name,
            "--yb-host",
            &self.yb_host,
            "--yb-port",
            &yb_port,
            // Relocate the whole cluster into a free port block so concurrent
            // runs (and a developer's local cluster) never collide.
            "--port-base",
            &port_base,
        ];
        if !self.yb_hot_path.is_empty() {
            init_args.extend(["--yb-hot-path", &self.yb_hot_path]);
        }
        self.yb(&init_args)?;
        self.yb(&["start"])?;

        step("branch + endpoint");
        let branch = self.branch_create()?;
        if branch.is_empty() {
            bail!("branch create failed");
        }
        tee_println!("branch: {branch}");
        self.branch = branch;

        // Pin `main` to `compute_port` (the compute lane, base +
        // COMPUTE_LANE) so the harness knows where to connect.
        let branch = self.branch.clone();
        let pg_port = self.compute_port.to_string();
        self.yb(&[
            "endpoint",
            "create",
            "main",
            "--branch-id",
            &branch,
            "--branch-name",
            "main",
            "--pg-version",
            "18",
            "--pg-port",
            &pg_port,
        ])?;
        self.yb(&["endpoint", "start", "main"])?;
        Ok(())
    }

    /// Flip teardown to actually run `clean.sh`. Called once at the end
    /// of a fully-passing run.
    pub fn mark_success(&mut self) {
        self.success = true;
    }

    // ---- subprocess helpers ----------------------------------------

    /// Build a `yb_light_local` command with the env every subprocess
    /// needs: the repo dir and the WAL-roundtrip verifier flag.
    fn yb_command(&self) -> Command {
        let mut cmd = Command::new(&self.yb_light);
        cmd.current_dir(&self.repo)
            .env("YB_LIGHT_REPO_DIR", &self.repo_dir)
            .env(
                "YB_PS_VERIFY_WAL_ROUNDTRIP",
                env_or(&["YB_PS_VERIFY_WAL_ROUNDTRIP"], "1"),
            );
        cmd
    }

    /// Run a `yb_light_local` subcommand, inheriting stdio, and fail if
    /// it exits non-zero.
    pub fn yb(&self, args: &[&str]) -> anyhow::Result<()> {
        self.yb_env(args, &[])
    }

    /// Like [`Self::yb`], with extra environment variables set for the
    /// child only (e.g. daemon-config overrides the control plane reads
    /// when writing `safekeeper.toml` / `pageserver.toml`).
    pub fn yb_env(&self, args: &[&str], envs: &[(&str, &str)]) -> anyhow::Result<()> {
        let status = self
            .yb_command()
            .args(args)
            .envs(envs.iter().map(|(k, v)| (*k, *v)))
            .status()
            .with_context(|| format!("spawning yb_light_local {}", args.join(" ")))?;
        if !status.success() {
            bail!("yb_light_local {} exited with {status}", args.join(" "));
        }
        Ok(())
    }

    /// Run a `yb_light_local` subcommand, capturing its output and teeing
    /// every line into the run log. [`Self::yb`]'s inherited stdio reaches
    /// the console but never the run log file, so when a cleanup restart
    /// failed post-hoc forensics had nothing to go on (#241). Capture
    /// buffers until exit, so keep this to short commands (restarts,
    /// stop/start) — long ones would lose live output.
    pub fn yb_logged(&self, args: &[&str]) -> anyhow::Result<()> {
        let out = self
            .yb_command()
            .args(args)
            .output()
            .with_context(|| format!("spawning yb_light_local {}", args.join(" ")))?;
        for line in String::from_utf8_lossy(&out.stdout)
            .lines()
            .chain(String::from_utf8_lossy(&out.stderr).lines())
        {
            tee_line(&format!("  | {line}"));
        }
        if !out.status.success() {
            bail!(
                "yb_light_local {} exited with {}",
                args.join(" "),
                out.status
            );
        }
        Ok(())
    }

    /// Run a `yb_light_local` subcommand, ignoring its exit status (for
    /// best-effort cleanup calls).
    pub fn yb_quiet(&self, args: &[&str]) {
        let _ = self
            .yb_command()
            .args(args)
            .stdout(std::process::Stdio::null())
            .stderr(std::process::Stdio::null())
            .status();
    }

    /// Run a `yb_light_local` subcommand with a wall-clock timeout.
    /// Returns whether it completed successfully within the deadline.
    pub async fn yb_timeout(&self, args: &[&str], dur: Duration) -> anyhow::Result<bool> {
        let mut cmd = tokio::process::Command::new(&self.yb_light);
        cmd.current_dir(&self.repo)
            .env("YB_LIGHT_REPO_DIR", &self.repo_dir)
            .env(
                "YB_PS_VERIFY_WAL_ROUNDTRIP",
                env_or(&["YB_PS_VERIFY_WAL_ROUNDTRIP"], "1"),
            )
            .args(args)
            .stdout(std::process::Stdio::null());
        let mut child = cmd
            .spawn()
            .with_context(|| format!("spawning yb_light_local {}", args.join(" ")))?;
        match tokio::time::timeout(dur, child.wait()).await {
            Ok(status) => Ok(status?.success()),
            Err(_) => {
                let _ = child.start_kill();
                Ok(false)
            }
        }
    }

    /// `branch create --pg-version 18`, parsing the branch id out of the
    /// "branch <id> successfully created" stdout line.
    pub fn branch_create(&self) -> anyhow::Result<String> {
        let out = self
            .yb_command()
            .args(["branch", "create", "--pg-version", "18"])
            .output()
            .context("spawning yb_light_local branch create")?;
        let text = format!(
            "{}{}",
            String::from_utf8_lossy(&out.stdout),
            String::from_utf8_lossy(&out.stderr)
        );
        let branch = text
            .lines()
            .find(|l| l.contains("successfully created"))
            .and_then(|l| l.split_whitespace().nth(1))
            .unwrap_or("")
            .to_string();
        Ok(branch)
    }

    /// Run a diagnostic tool binary (built next to `yb_light_local` in
    /// `target/<profile>/`) with this run's YB connection flags + `extra`.
    /// Returns stdout; fails on a non-zero exit. Exercises the shared
    /// `YbConnArgs` no-TLS connection path end-to-end.
    pub fn run_tool(&self, bin: &str, extra: &[&str]) -> anyhow::Result<String> {
        let path = self
            .yb_light
            .parent()
            .map(|d| d.join(bin))
            .unwrap_or_else(|| PathBuf::from(bin));
        let host_ports = format!("{}:{}", self.yb_host, self.yb_port);
        let mut args: Vec<&str> = vec!["--host-ports", &host_ports, "--db-name", &self.db_name];
        args.extend_from_slice(extra);
        let out = Command::new(&path)
            .args(&args)
            .output()
            .with_context(|| format!("spawning {}", path.display()))?;
        if !out.status.success() {
            bail!(
                "{bin} {} failed (exit {}): {}",
                extra.join(" "),
                out.status,
                String::from_utf8_lossy(&out.stderr).trim()
            );
        }
        Ok(String::from_utf8_lossy(&out.stdout).into_owned())
    }

    /// Like [`Self::run_tool`] but tolerates a non-zero exit: returns
    /// `(success, stdout, stderr)`. For tools whose *success* depends on
    /// cluster data (e.g. WAL coverage) but whose *connection* is still
    /// worth asserting.
    pub fn run_tool_capture(
        &self,
        bin: &str,
        extra: &[&str],
    ) -> anyhow::Result<(bool, String, String)> {
        let path = self
            .yb_light
            .parent()
            .map(|d| d.join(bin))
            .unwrap_or_else(|| PathBuf::from(bin));
        let host_ports = format!("{}:{}", self.yb_host, self.yb_port);
        let mut args: Vec<&str> = vec!["--host-ports", &host_ports, "--db-name", &self.db_name];
        args.extend_from_slice(extra);
        let out = Command::new(&path)
            .args(&args)
            .output()
            .with_context(|| format!("spawning {}", path.display()))?;
        Ok((
            out.status.success(),
            String::from_utf8_lossy(&out.stdout).into_owned(),
            String::from_utf8_lossy(&out.stderr).into_owned(),
        ))
    }

    // ---- scenario / workload drivers -------------------------------

    /// Run one scenario: recycle the cluster, run the scenario future,
    /// then scan the pageserver log tail for corruption markers. `label`
    /// is used in progress/PASS messages. The future is lazy (async fns
    /// don't execute until awaited), so `prep` still runs before the
    /// scenario body even though the future is constructed at the call
    /// site.
    pub async fn run_scenario<Fut>(
        &self,
        label: &str,
        pos: (usize, usize),
        fut: Fut,
    ) -> anyhow::Result<()>
    where
        Fut: Future<Output = anyhow::Result<()>>,
    {
        self.prep_for_next_test().await?;
        step(&format!("script: {label}"));
        let before = self.ps_log_lines();
        let t0 = Instant::now();
        // Run the scenario, but fail FAST if the cluster wedges: a
        // watchdog polls every cluster log for a fatal marker (e.g. the
        // safekeeper WAL-decode panic, #61) that would otherwise leave the
        // test hung in `wait_lsn`, and a no-progress backstop catches any
        // other silent stall. Either beats waiting out a silent hang.
        //
        // The backstop measures time since the last harness output, NOT
        // total scenario runtime: a multi-iteration scenario like
        // `torn_wal_write` legitimately runs for several minutes, emitting
        // a step banner every few seconds while it crashes/recovers the
        // cluster ~dozens of times. Capping *total* runtime here failed
        // that scenario for merely being long (#71) even though it was
        // making steady, corruption-free progress; capping *quiet time*
        // still trips a real wedge (no step/output) within `budget`.
        let budget =
            Duration::from_secs(env_or(&["INTEGRATION_TEST_TIMEOUT"], "180").parse().unwrap_or(180));
        mark_progress();
        tokio::select! {
            r = fut => r?,
            hit = self.watch_fatal() => {
                bail!("{label}: fatal marker in a cluster log while running (cluster wedged): {hit}");
            }
            _ = async {
                loop {
                    tokio::time::sleep(Duration::from_secs(5)).await;
                    if since_progress() >= budget {
                        return;
                    }
                }
            } => {
                bail!(
                    "{label}: no progress for {}s with no fatal marker — cluster wedged; logs under {}",
                    budget.as_secs(),
                    self.repo_dir.display()
                );
            }
        }
        self.corruption_gate(label, before)?;
        self.wals_overlap_gate(label).await?;
        let (idx, total) = pos;
        tee_println!(
            "PASS [{:>8.3}s] ({idx}/{total}) {label}",
            t0.elapsed().as_secs_f64()
        );
        Ok(())
    }

    /// Poll every pageserver + safekeeper log under the run dir for a
    /// [`FATAL_MARKERS`] hit; resolves with the offending `path: line` the
    /// moment one appears. Used as a fail-fast arm in [`Self::run_scenario`].
    async fn watch_fatal(&self) -> String {
        loop {
            if let Some(hit) = self.scan_fatal() {
                return hit;
            }
            tokio::time::sleep(Duration::from_secs(2)).await;
        }
    }

    /// First [`FATAL_MARKERS`] line across all `endpoints/*/{pageserver,
    /// safekeeper}/*.log` under the run dir, prefixed with its file.
    fn scan_fatal(&self) -> Option<String> {
        let eps = std::fs::read_dir(self.repo_dir.join("endpoints")).ok()?;
        for ep in eps.flatten() {
            for sub in ["safekeeper", "pageserver"] {
                let Ok(rd) = std::fs::read_dir(ep.path().join(sub)) else {
                    continue;
                };
                for f in rd.flatten() {
                    let p = f.path();
                    if p.extension().is_none_or(|e| e != "log") {
                        continue;
                    }
                    let Ok(content) = std::fs::read_to_string(&p) else {
                        continue;
                    };
                    if let Some(line) = content.lines().find(|l| self.fatal_markers.is_match(l)) {
                        return Some(format!("{}: {}", p.display(), line));
                    }
                }
            }
        }
        None
    }

    /// Recycle the cluster, then run a `.sql` workload through `psql`,
    /// normalize its stdout, and diff against the checked-in `.out`
    /// golden (regenerate with `INTEGRATION_REGEN_EXPECTED=1`). Also runs the
    /// corruption gate.
    pub async fn run_sql(
        &self,
        label: &str,
        pos: (usize, usize),
        sql: &Path,
    ) -> anyhow::Result<()> {
        self.prep_for_next_test().await?;
        step(&format!("workload: {label}"));
        let before = self.ps_log_lines();
        let t0 = Instant::now();
        crate::workload::run_sql_workload(self, label, sql)?;
        self.corruption_gate(label, before)?;
        self.wals_overlap_gate(label).await?;
        let (idx, total) = pos;
        tee_println!(
            "PASS [{:>8.3}s] ({idx}/{total}) {label}",
            t0.elapsed().as_secs_f64()
        );
        Ok(())
    }

    // ---- lifecycle internals ---------------------------------------

    /// Tear down any prior cluster state via `clean.sh`, with this run's
    /// repo dir + ephemeral db name in the env.
    fn clean(&self) -> anyhow::Result<()> {
        let status = Command::new("bash")
            .arg(self.repo.join("test/integration/compute/clean.sh"))
            .current_dir(&self.repo)
            .env("YB_LIGHT_REPO_DIR", &self.repo_dir)
            .env("YB_DBNAME", &self.db_name)
            .env("YB_HOST", &self.yb_host)
            .env("YB_PORT", self.yb_port.to_string())
            .env("COMPUTE_PORT", self.compute_port.to_string())
            .status()
            .context("spawning clean.sh")?;
        if !status.success() {
            bail!("clean.sh exited with {status}");
        }
        Ok(())
    }

    /// Tear down the cluster + db.
    /// Best-effort — this is teardown. gc_old_runs reaps the run dir at
    /// 7 days.
    /// Stop the cluster and drop the ephemeral db via `clean.sh`. With
    /// `keep_logs`, clean.sh preserves this run's `*.log` files (and removes
    /// the rest); otherwise it removes the whole run dir.
    fn teardown(&self, keep_logs: bool) {
        let _ = Command::new("bash")
            .arg(self.repo.join("test/integration/compute/clean.sh"))
            .current_dir(&self.repo)
            .env("YB_LIGHT_REPO_DIR", &self.repo_dir)
            .env("YB_DBNAME", &self.db_name)
            .env("YB_HOST", &self.yb_host)
            .env("YB_PORT", self.yb_port.to_string())
            .env("COMPUTE_PORT", self.compute_port.to_string())
            .env("INTEGRATION_KEEP_LOGS", if keep_logs { "1" } else { "" })
            .stdout(std::process::Stdio::null())
            .stderr(std::process::Stdio::null())
            .status();
    }

    /// Discover the branch id of the shared `main` branch from the
    /// pageserver's branch listing.
    async fn main_branch_id(&self) -> anyhow::Result<String> {
        let url = format!(
            "http://127.0.0.1:{}/v1/branch/{}/timeline",
            self.ps_http_port(),
            self.branch
        );
        let body: serde_json::Value = reqwest::get(&url).await?.json().await?;
        body.as_array()
            .and_then(|a| a.first())
            .and_then(|t| t.get("branch_id"))
            .and_then(|v| v.as_str())
            .map(|s| s.to_string())
            .ok_or_else(|| {
                anyhow::anyhow!("no branch in pageserver listing for branch {}", self.branch)
            })
    }

    /// Before recycling, drive the pageserver to ingest + flush every WAL
    /// record the compute has produced, so its `consistent_lsn` reaches
    /// the tip. The post-recycle basebackup's `wait_lsn` then has ~no WAL
    /// to re-ingest.
    ///
    /// Without this, a heavy-commit scenario (e.g. `clog`) leaves the
    /// pageserver tens of MB behind the safekeeper — the compute commits
    /// faster than the PS ingests — and the next compute start drains
    /// that lag synchronously inside the basebackup, blowing the
    /// compute-startup budget (#115). Paying it here, with the compute
    /// still up, moves the cost out of the timed recycle.
    ///
    /// Best-effort: if the compute is already down (prior test stopped
    /// it) or the flush call fails, log and let the recycle proceed.
    async fn drain_ingest_before_recycle(&self) {
        match self.try_drain_ingest().await {
            Ok(Some(secs)) if secs >= 2 => {
                tee_println!("  (pre-recycle ingest drain: {secs}s)")
            }
            Ok(_) => {}
            Err(e) => tee_println!("  (pre-recycle ingest drain skipped: {e:#})"),
        }
    }

    /// Returns `Some(seconds)` for the drain, or `None` if there was
    /// nothing to do. Errors propagate to the best-effort wrapper.
    async fn try_drain_ingest(&self) -> anyhow::Result<Option<u64>> {
        // The compute's flushed WAL position is the tip the pageserver
        // must catch up to. A failure here (compute down) means no fresh
        // lag to drain, so the `?` short-circuit is the right behavior.
        let tip = crate::db::query_text(
            &crate::db::compute_conn(self.compute_port()),
            "select pg_current_wal_flush_lsn()::text",
        )
        .await?;
        let branch = self.main_branch_id().await?;
        // `flush?lsn=<tip>` waits for WAL ingest to reach `tip`, then
        // force_flushes — advancing `consistent_lsn` to the tip.
        let url = format!(
            "http://127.0.0.1:{}/v1/branch/{branch}/flush?lsn={tip}",
            self.ps_http_port(),
        );
        let t0 = Instant::now();
        let resp = reqwest::Client::new().put(&url).send().await?;
        if !resp.status().is_success() {
            bail!("pageserver flush returned {}", resp.status());
        }
        Ok(Some(t0.elapsed().as_secs()))
    }

    /// Restart any cluster component the prior scenario left stopped,
    /// then recycle the compute, so each scenario starts from a clean
    /// compute state.
    pub async fn prep_for_next_test(&self) -> anyhow::Result<()> {
        let r = &self.repo_dir;
        let sc = r.join("storage_controller_1/storage_controller.pid");
        let ps = r.join("endpoints/main/pageserver/pageserver.pid");
        let sk = r.join("endpoints/main/safekeeper/safekeeper.pid");
        let es = r.join("endpoints/main/endpoint_storage/endpoint_storage.pid");

        if !is_running(&sc) {
            step("restart storage_controller (prior test left it down)");
            self.yb(&["storage-controller", "start"])?;
        }
        if !is_running(&ps) {
            step("restart pageserver (prior test left it down)");
            self.yb(&["pageserver", "start"])?;
        }
        if !is_running(&sk) {
            step("restart safekeeper (prior test left it down)");
            self.yb(&["safekeeper", "start"])?;
        }
        if !is_running(&es) {
            step("restart endpoint_storage (prior test left it down)");
            self.yb(&["endpoint-storage", "start"])?;
        }

        // The pid check above can pass while the PS serves nothing — a
        // failed config-restore restart leaves a live-but-wedged process,
        // and every later test then fails confusingly on connection
        // refused (#241). Probe the management port and force one restart
        // if it won't accept.
        self.ensure_ps_serving().await?;

        // Drain the pageserver's ingest backlog while the compute is
        // still up, so the recycle below doesn't pay it inside the timed
        // basebackup `wait_lsn` (#115).
        self.drain_ingest_before_recycle().await;

        // Always recycle the compute (immediate stop: some scenarios
        // leave it mid-flight and a graceful stop blocks on the next
        // walproposer round-trip). `endpoint stop` is tolerant of an
        // already-gone postgres.
        let t0 = Instant::now();
        self.yb_quiet(&["endpoint", "stop", "main", "--mode", "immediate"]);
        self.yb(&["endpoint", "start", "main"])?;
        let secs = t0.elapsed().as_secs();
        if secs > 2 {
            tee_println!("  (endpoint recycle: {secs}s)");
        }
        Ok(())
    }

    /// Ensure the main pageserver accepts TCP on its management port,
    /// forcing one `pageserver restart` if it doesn't. Complements the
    /// pid-file liveness check in [`Self::prep_for_next_test`], which a
    /// live-but-wedged PS passes (#241).
    async fn ensure_ps_serving(&self) -> anyhow::Result<()> {
        if self.ps_accepts(Duration::from_secs(10)).await {
            return Ok(());
        }
        step("pageserver alive but not serving — forcing restart (prior test left it wedged)");
        self.yb_logged(&["pageserver", "restart"])?;
        if self.ps_accepts(Duration::from_secs(20)).await {
            return Ok(());
        }
        bail!(
            "pageserver still not accepting on 127.0.0.1:{} after a forced restart",
            self.ps_http_port()
        )
    }

    /// True once the PS management port accepts a TCP connection within
    /// `budget`, polling every 500ms.
    async fn ps_accepts(&self, budget: Duration) -> bool {
        let addr = format!("127.0.0.1:{}", self.ps_http_port());
        let t0 = Instant::now();
        loop {
            if let Ok(Ok(_)) = tokio::time::timeout(
                Duration::from_secs(1),
                tokio::net::TcpStream::connect(&addr),
            )
            .await
            {
                return true;
            }
            if t0.elapsed() >= budget {
                return false;
            }
            tokio::time::sleep(Duration::from_millis(500)).await;
        }
    }

    // ---- corruption gate -------------------------------------------

    /// Path to the shared `main` endpoint's pageserver log — the one the
    /// gate watches (branch pageservers write elsewhere, matching the
    /// shell which only scanned `main`'s log).
    pub fn ps_log(&self) -> PathBuf {
        self.repo_dir
            .join("endpoints/main/pageserver/pageserver.log")
    }

    fn ps_log_lines(&self) -> usize {
        std::fs::read_to_string(self.ps_log())
            .map(|s| s.lines().count())
            .unwrap_or(0)
    }

    /// Scan the pageserver log lines added since `before` for corruption
    /// markers; first hit fails the run. (Logs are append-only across
    /// restarts, so line counts only grow.)
    fn corruption_gate(&self, label: &str, before: usize) -> anyhow::Result<()> {
        let content = match std::fs::read_to_string(self.ps_log()) {
            Ok(c) => c,
            Err(_) => return Ok(()),
        };
        let lines: Vec<&str> = content.lines().collect();
        if lines.len() <= before {
            return Ok(());
        }
        let hits: Vec<&str> = lines[before..]
            .iter()
            .copied()
            .filter(|l| self.markers.is_match(l))
            .collect();
        if !hits.is_empty() {
            eprintln!(
                "FAIL — {} corruption marker(s) introduced by {label}",
                hits.len()
            );
            for l in hits.iter().take(5) {
                eprintln!("{l}");
            }
            bail!("corruption markers introduced by {label}");
        }
        Ok(())
    }

    /// Post-scenario invariant on the run's YB db: no two rows in
    /// `safekeeper.wals` within one `(branch_id, segno)` may have
    /// overlapping `[chunk_offset, chunk_offset + chunk_size)` ranges.
    /// The writer never produces overlapping chunks and everything
    /// downstream leans on it — `load_write_lsn`'s "highest offset ⇒
    /// highest end" rehydration, `truncate_wals_above`'s straddle math,
    /// and the segment reader's covering-chunk selection. Per-batch
    /// bucket stamping and the direct→YSQL fallback both replay the same
    /// coalesced runs, so any overlap here means a real write-path bug.
    ///
    /// One window-function query per scenario (lag over chunk_offset per
    /// partition); an empty table — early scenarios, or post-GC — is a
    /// trivial pass. Wired exactly like the corruption-marker gate: any
    /// violating pair fails the scenario, with the rows printed.
    async fn wals_overlap_gate(&self, label: &str) -> anyhow::Result<()> {
        let conninfo = crate::db::yb_conn(&self.yb_host, self.yb_port, &self.db_name);
        let client = crate::db::connect(&conninfo)
            .await
            .context("wals overlap gate: connecting to the run's YB db")?;
        let sql = "SELECT branch_id, segno, prev_off, prev_size, chunk_offset, chunk_size \
                   FROM (SELECT branch_id, segno, chunk_offset, chunk_size, \
                                lag(chunk_offset) OVER w AS prev_off, \
                                lag(chunk_size)  OVER w AS prev_size \
                         FROM safekeeper.wals \
                         WINDOW w AS (PARTITION BY branch_id, segno ORDER BY chunk_offset)) t \
                   WHERE prev_off + prev_size > chunk_offset";
        let rows = client
            .query(sql, &[])
            .await
            .context("wals overlap gate: overlap query")?;
        if rows.is_empty() {
            return Ok(());
        }
        eprintln!(
            "FAIL — {} overlapping safekeeper.wals row pair(s) after {label}",
            rows.len()
        );
        for r in rows.iter().take(5) {
            let branch: String = r.get("branch_id");
            let segno: i64 = r.get("segno");
            let prev_off: i64 = r.get("prev_off");
            let prev_size: i32 = r.get("prev_size");
            let off: i64 = r.get("chunk_offset");
            let size: i32 = r.get("chunk_size");
            eprintln!(
                "  branch {branch} segno {segno}: [{prev_off}, {}) overlaps [{off}, {})",
                prev_off + prev_size as i64,
                off + size as i64,
            );
        }
        bail!("overlapping safekeeper.wals chunk ranges introduced by {label}");
    }

    /// Final verdict: the `pageserver_yb_correctness_errors_total` metric
    /// must have no exported series (a series only appears once the
    /// counter is touched).
    pub async fn final_verdict(&self) -> anyhow::Result<()> {
        step("Final verdict");
        let body = reqwest::get(format!("http://127.0.0.1:{}/metrics", self.ps_http_port()))
            .await
            .and_then(|r| r.error_for_status())
            .ok();
        let counter = match body {
            Some(resp) => resp.text().await.unwrap_or_default(),
            None => String::new(),
        };
        let hit: Vec<&str> = counter
            .lines()
            .filter(|l| l.starts_with("pageserver_yb_correctness_errors_total{"))
            .collect();
        // `durable_walk_unflushed_row` is documented-benign during attach /
        // reconnect replay (#57): a scenario that restarts the safekeeper
        // (e.g. sk_wal_cold) makes the pageserver walreceiver reconnect and
        // replay over still-buffered rows, and the materialiser's durable
        // walk briefly sees those re-pushed-as-unflushed rows. It bails to
        // the YB read either way, so this kind is not corruption. Report it
        // (so a sustained rate stays visible) but don't fail on it alone;
        // every other kind remains fatal.
        let (benign, fatal): (Vec<&str>, Vec<&str>) = hit
            .iter()
            .partition(|l| l.contains("kind=\"durable_walk_unflushed_row\""));
        if !benign.is_empty() {
            for l in &benign {
                tee_println!("  (benign correctness counter — reconnect/attach replay #57: {l})");
            }
        }
        if !fatal.is_empty() {
            eprintln!("FAIL — correctness counter non-zero:");
            for l in fatal {
                eprintln!("{l}");
            }
            bail!("correctness counter non-zero");
        }
        tee_println!("PASS — all workloads clean, counter at 0");
        Ok(())
    }
}

impl Drop for Cluster {
    fn drop(&mut self) {
        if self.success {
            // Passing run: full teardown — drop the cluster + db + run dir.
            self.teardown(false);
        } else if self.no_fail_stop {
            // --no-fail-stop: leave the cluster + db running for postmortem.
            eprintln!();
            eprintln!("run FAILED — leaving cluster up for inspection:");
            eprintln!("  logs/dir: {}", self.repo_dir.display());
            eprintln!("  YB db:    {}", self.db_name);
            eprintln!(
                "  tear down manually with: YB_LIGHT_REPO_DIR={} YB_DBNAME={} test/integration/compute/clean.sh",
                self.repo_dir.display(),
                self.db_name
            );
        } else {
            // Default on failure: stop the cluster + drop the db, but keep
            // this run's `*.log` files for postmortem / CI artifact upload.
            eprintln!();
            eprintln!(
                "run FAILED — cluster stopped; logs kept at {}",
                self.repo_dir.display()
            );
            eprintln!("  (re-run with --no-fail-stop to leave the cluster up for postmortem)");
            self.teardown(true);
        }
    }
}

// ---- free helpers --------------------------------------------------

fn git_toplevel() -> anyhow::Result<PathBuf> {
    let out = Command::new("git")
        .args(["rev-parse", "--show-toplevel"])
        .output()
        .context("git rev-parse --show-toplevel")?;
    if !out.status.success() {
        bail!("not a git repository");
    }
    Ok(PathBuf::from(
        String::from_utf8_lossy(&out.stdout).trim().to_string(),
    ))
}

fn env_or(keys: &[&str], default: &str) -> String {
    for k in keys {
        if let Ok(v) = std::env::var(k) {
            if !v.is_empty() {
                return v;
            }
        }
    }
    default.to_string()
}

/// Is the process named in `pidfile` alive? (Linux `/proc/<pid>` check.)
/// Is the component that owns `pidfile` still running?
///
/// Each component (pageserver, safekeeper, endpoint_storage,
/// storage_controller) holds an exclusive advisory `flock` on its pidfile
/// for its entire lifetime (see `utils::pid_file` / `utils::lock_file`).
/// So liveness == "the lock is still held": we probe by trying to take it
/// non-blocking. If we get it, nobody holds it → not running; if it's
/// contended, a live process holds it → running.
///
/// This deliberately does NOT look at the pidfile's *content*: `pid_file`
/// documents the content as racy, and the storage controller in particular
/// is observed to leave its content empty while running fine — a
/// PID/`/proc` check (the previous implementation) both mis-fired on that
/// and didn't work off Linux at all.
fn is_running(pidfile: &Path) -> bool {
    use nix::fcntl::{Flock, FlockArg};

    let Ok(file) = std::fs::File::open(pidfile) else {
        return false; // no pidfile → never started, or already cleaned up
    };
    match Flock::lock(file, FlockArg::LockExclusiveNonblock) {
        // Acquired the lock → nobody was holding it → not running.
        // The guard drops here, releasing the lock immediately.
        Ok(_guard) => false,
        // Contended → a live process holds the lock → running.
        Err(_) => true,
    }
}

/// A run id derived from wall-clock seconds + pid — enough to keep
/// concurrent run dirs from colliding.
fn run_id() -> String {
    let secs = SystemTime::now()
        .duration_since(SystemTime::UNIX_EPOCH)
        .map(|d| d.as_secs())
        .unwrap_or(0);
    format!("{secs}_{}", std::process::id())
}

/// Reap `run_*` dirs/logs older than 7 days under `test_dir`.
fn gc_old_runs(test_dir: &Path) {
    let Ok(entries) = std::fs::read_dir(test_dir) else {
        return;
    };
    let cutoff = Duration::from_secs(7 * 24 * 3600);
    let now = SystemTime::now();
    for entry in entries.flatten() {
        let name = entry.file_name();
        let name = name.to_string_lossy();
        if !name.starts_with("run_") {
            continue;
        }
        let too_old = entry
            .metadata()
            .and_then(|m| m.modified())
            .ok()
            .and_then(|m| now.duration_since(m).ok())
            .map(|age| age > cutoff)
            .unwrap_or(false);
        if too_old {
            let p = entry.path();
            if p.is_dir() {
                let _ = std::fs::remove_dir_all(&p);
            } else {
                let _ = std::fs::remove_file(&p);
            }
        }
    }
}

/// Offset of the compute (`main`) lane from the cluster's port base. The
/// storage stack occupies base+0..base+600 (one 100-port lane per family;
/// see `yb_light_local init --port-base`); compute sits above it, and
/// scenario branch endpoints live just above `main` at `compute_port + N`.
const COMPUTE_LANE: u16 = 700;

/// How many distinct port blocks to try before giving up on bring-up.
/// Each retry picks a fresh block, so this also bounds how many concurrent
/// runs can lose the block race and still recover.
const PORT_BLOCK_ATTEMPTS: u16 = 6;

/// Pick a base for a free port block (base+0 .. base+900) so each run's
/// cluster coexists with concurrent runs and a developer's local cluster.
/// Probes candidate bases (one per service-family lane) and returns the
/// first all-free one; override/pin with `INTEGRATION_PORT_BASE`.
///
/// Two properties keep this robust under concurrency:
///   * The whole block stays **below the OS ephemeral port range**, so a
///     fixed cluster port can never clash with a transient outbound
///     connection's source port — that clash surfaces as a flaky
///     `tenant create` / `endpoint create` bind failure, not as an
///     obvious "address in use".
///   * The scan starts at a **PID-derived offset** (wrapping), so several
///     harness runs firing at once spread across the range instead of all
///     converging on the lowest free block and colliding.
fn pick_port_base(tried: &[u16]) -> u16 {
    if let Ok(b) = env_or(&["INTEGRATION_PORT_BASE"], "").parse::<u16>() {
        return b;
    }
    const LO: u16 = 10000;
    const STEP: u16 = 1000;
    const BLOCK: u16 = 900; // the block spans base+0 ..= base+900
    // Highest base whose entire block still sits below the ephemeral floor.
    let hi = ephemeral_floor().saturating_sub(BLOCK + STEP);
    let candidates: Vec<u16> = (LO..=hi)
        .step_by(STEP as usize)
        .filter(|b| !tried.contains(b))
        .collect();
    if candidates.is_empty() {
        return LO;
    }
    // Seed the scan from pid XOR wall-clock nanos so concurrent runs — and
    // successive retries within one run — start at different offsets rather
    // than all converging on the lowest free block.
    let seed = std::process::id() as u128
        ^ SystemTime::now()
            .duration_since(SystemTime::UNIX_EPOCH)
            .map(|d| d.as_nanos())
            .unwrap_or(0);
    let start = (seed % candidates.len() as u128) as usize;
    for i in 0..candidates.len() {
        let base = candidates[(start + i) % candidates.len()];
        if (0..=BLOCK).step_by(100).all(|d| port_free(base + d)) {
            return base;
        }
    }
    candidates[start] // best-effort: the bind-probe is advisory anyway
}

/// Low end of the OS ephemeral (dynamic) port range. Cluster ports must
/// stay below this so a fixed port can't collide with a transient
/// outbound connection's source port. Reads Linux's `ip_local_port_range`
/// (first field); falls back to the common 32768 default elsewhere.
fn ephemeral_floor() -> u16 {
    std::fs::read_to_string("/proc/sys/net/ipv4/ip_local_port_range")
        .ok()
        .and_then(|s| s.split_whitespace().next()?.parse::<u16>().ok())
        .filter(|&n| n > 10000)
        .unwrap_or(32768)
}

/// Is `port` bindable on loopback right now? (Advisory free-port check.)
fn port_free(port: u16) -> bool {
    std::net::TcpListener::bind(("127.0.0.1", port)).is_ok()
}
