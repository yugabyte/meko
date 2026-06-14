//! The shared-cluster fixture. Owns the suite lifecycle: ephemeral-db
//! setup, `init`/`start`, one shared tenant + `main` endpoint, the
//! per-scenario cluster recycle, the corruption-marker log gate, and a
//! teardown that runs `clean.sh` but keeps the run's `*.log` files
//! (`--no-fail-clean` instead leaves a failed cluster up for postmortem).

use std::future::Future;
use std::io::Write;
use std::path::{Path, PathBuf};
use std::process::Command;
use std::sync::{Mutex, OnceLock};
use std::time::{Duration, Instant, SystemTime};

use anyhow::{Context, bail};
use regex::Regex;

static RUN_LOG: OnceLock<Mutex<std::fs::File>> = OnceLock::new();

/// Write `s` to stdout and, if initialized, to the run log file.
pub fn tee_line(s: &str) {
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
pub const FATAL_MARKERS: &str =
    r"interpreted wal reader: failed to decode|invalid xlog page header|panicked at|thread '[^']*' panicked";

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
    /// Free port-block base for this run (`init --port-base`), relocating
    /// the whole cluster off the defaults so concurrent runs (and a
    /// developer's local cluster) never collide. Auto-picked free, or
    /// pinned via `E2E_PORT_BASE`.
    port_base: u16,
    /// Compute port of the shared `main` endpoint.
    compute_port: u16,
    /// The shared tenant id, set by [`Cluster::setup`].
    tid: String,
    /// Regex form of [`MARKERS`].
    markers: Regex,
    /// Regex form of [`FATAL_MARKERS`] (polled across all cluster logs
    /// during a test so a wedge fails fast).
    fatal_markers: Regex,
    /// Set by [`Cluster::mark_success`] at the end of a fully-passing
    /// run. Drives the final-verdict messaging; teardown keeps logs
    /// either way (see `no_fail_clean` for the failure postmortem opt-in).
    success: bool,
    /// `--no-fail-clean` (env `E2E_NO_FAIL_CLEAN`): on failure, leave the
    /// cluster + db up for postmortem instead of the default
    /// tear-down-but-keep-logs.
    no_fail_clean: bool,
}

impl Cluster {
    pub fn tid(&self) -> &str {
        &self.tid
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
        let test_dir = env_or(&["TEST_DIR"], &repo.join(".e2e-tests").to_string_lossy());
        let test_dir = PathBuf::from(test_dir);

        // GC run dirs older than 7 days under .e2e-tests: a passing run
        // leaves a logs-only `run_<id>/`, a failed one its full dir.
        gc_old_runs(&test_dir);

        let run_id = run_id();
        let yb_light_repo_dir_env = std::env::var("YB_LIGHT_REPO_DIR").ok().filter(|v| !v.is_empty());
        let repo_dir = match &yb_light_repo_dir_env {
            Some(d) => PathBuf::from(d),
            None => test_dir.join(format!("run_{run_id}")),
        };
        std::fs::create_dir_all(&repo_dir)
            .with_context(|| format!("creating {}", repo_dir.display()))?;

        // Open the run log: inside repo_dir when YB_LIGHT_REPO_DIR was set,
        // otherwise a sibling of the run dir under test_dir.
        let log_dir = match &yb_light_repo_dir_env {
            Some(_) => repo_dir.clone(),
            None => test_dir.clone(),
        };
        let log_path = log_dir.join(format!("run_{run_id}.log"));
        if let Ok(f) = std::fs::File::create(&log_path) {
            let _ = RUN_LOG.set(Mutex::new(f));
            println!("run log: {}", log_path.display());
        }

        let yb_host = env_or(&["YB_HOST"], "127.0.0.1");
        let yb_port: u16 = env_or(&["YB_PORT"], "5433").parse().context("YB_PORT")?;

        let db_name = match std::env::var("YB_DBNAME") {
            Ok(d) if !d.is_empty() => d,
            _ => {
                let pid = std::process::id();
                let nanos = SystemTime::now()
                    .duration_since(SystemTime::UNIX_EPOCH)
                    .map(|d| d.subsec_nanos())
                    .unwrap_or(0);
                format!("yb_storage_e2e_{pid}_{nanos}")
            }
        };

        // Every run synthesizes its own config (`yb_light_local init
        // --yb-dbname <ephemeral> --port-base <free>`) and relocates the
        // whole cluster into a free port block, so runs are isolated by
        // construction and can execute concurrently. `main` lives in the
        // compute lane at base + COMPUTE_LANE. `E2E_PORT_BASE` pins the block;
        // otherwise a free one is auto-picked.
        let port_base = pick_port_base();
        let compute_port: u16 = port_base + COMPUTE_LANE;

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
            port_base,
            compute_port,
            tid: String::new(),
            markers: Regex::new(MARKERS).expect("static marker regex"),
            fatal_markers: Regex::new(FATAL_MARKERS).expect("static fatal-marker regex"),
            success: false,
            no_fail_clean: std::env::var("E2E_NO_FAIL_CLEAN").is_ok_and(|v| !v.is_empty()),
        };

        // clean leftover state, then init + start the stack.
        cluster.clean()?;

        step(&format!(
            "yb_light_local init + start (db={})",
            cluster.db_name
        ));
        let yb_port = cluster.yb_port.to_string();
        let port_base = cluster.port_base.to_string();
        cluster.yb(&[
            "init",
            "--yb-dbname",
            &cluster.db_name,
            "--yb-host",
            &cluster.yb_host,
            "--yb-port",
            &yb_port,
            // Relocate the whole cluster into a free port block so concurrent
            // runs (and a developer's local cluster) never collide.
            "--port-base",
            &port_base,
        ])?;
        cluster.yb(&["start"])?;

        step("tenant + endpoint");
        let tid = cluster.tenant_create()?;
        if tid.is_empty() {
            bail!("tenant create failed");
        }
        tee_println!("tenant: {tid}");
        cluster.tid = tid;

        // Pin `main` to `compute_port` (the compute lane, base +
        // COMPUTE_LANE) so the harness knows where to connect.
        let tid = cluster.tid.clone();
        let pg_port = cluster.compute_port.to_string();
        cluster.yb(&[
            "endpoint", "create", "main", "--tenant-id", &tid, "--branch-name", "main",
            "--pg-version", "17", "--pg-port", &pg_port,
        ])?;
        cluster.yb(&["endpoint", "start", "main"])?;

        Ok(cluster)
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
        let status = self
            .yb_command()
            .args(args)
            .status()
            .with_context(|| format!("spawning yb_light_local {}", args.join(" ")))?;
        if !status.success() {
            bail!("yb_light_local {} exited with {status}", args.join(" "));
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

    /// `tenant create --pg-version 17`, parsing the tenant id out of the
    /// "tenant <id> successfully created" stdout line.
    pub fn tenant_create(&self) -> anyhow::Result<String> {
        let out = self
            .yb_command()
            .args(["tenant", "create", "--pg-version", "17"])
            .output()
            .context("spawning yb_light_local tenant create")?;
        let text = format!(
            "{}{}",
            String::from_utf8_lossy(&out.stdout),
            String::from_utf8_lossy(&out.stderr)
        );
        let tid = text
            .lines()
            .find(|l| l.contains("successfully created"))
            .and_then(|l| l.split_whitespace().nth(1))
            .unwrap_or("")
            .to_string();
        Ok(tid)
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
    pub fn run_tool_capture(&self, bin: &str, extra: &[&str]) -> anyhow::Result<(bool, String, String)> {
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
    pub async fn run_scenario<Fut>(&self, label: &str, fut: Fut) -> anyhow::Result<()>
    where
        Fut: Future<Output = anyhow::Result<()>>,
    {
        self.prep_for_next_test()?;
        step(&format!("script: {label}"));
        let before = self.ps_log_lines();
        let t0 = Instant::now();
        // Run the scenario, but fail FAST if the cluster wedges: a
        // watchdog polls every cluster log for a fatal marker (e.g. the
        // safekeeper WAL-decode panic, #61) that would otherwise leave the
        // test hung in `wait_lsn`, and a wall-clock backstop catches any
        // other stall. Either beats waiting out a silent hang.
        let budget = Duration::from_secs(
            env_or(&["E2E_TEST_TIMEOUT"], "180").parse().unwrap_or(180),
        );
        tokio::select! {
            r = fut => r?,
            hit = self.watch_fatal() => {
                bail!("{label}: fatal marker in a cluster log while running (cluster wedged): {hit}");
            }
            _ = tokio::time::sleep(budget) => {
                bail!(
                    "{label}: timed out after {}s with no fatal marker — cluster wedged; logs under {}",
                    budget.as_secs(),
                    self.repo_dir.display()
                );
            }
        }
        self.corruption_gate(label, before)?;
        tee_println!("PASS — {label}  ({}s)", t0.elapsed().as_secs());
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
    /// golden (regenerate with `E2E_REGEN_EXPECTED=1`). Also runs the
    /// corruption gate.
    pub async fn run_sql(&self, label: &str, sql: &Path) -> anyhow::Result<()> {
        self.prep_for_next_test()?;
        step(&format!("workload: {label}"));
        let before = self.ps_log_lines();
        let t0 = Instant::now();
        crate::workload::run_sql_workload(self, label, sql)?;
        self.corruption_gate(label, before)?;
        tee_println!("PASS — {label}  ({}s)", t0.elapsed().as_secs());
        Ok(())
    }

    // ---- lifecycle internals ---------------------------------------

    /// Tear down any prior cluster state via `clean.sh`, with this run's
    /// repo dir + ephemeral db name in the env.
    fn clean(&self) -> anyhow::Result<()> {
        let status = Command::new("bash")
            .arg(self.repo.join("test/e2e/clean.sh"))
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
    fn teardown(&self) {
        let _ = Command::new("bash")
            .arg(self.repo.join("test/e2e/clean.sh"))
            .current_dir(&self.repo)
            .env("YB_LIGHT_REPO_DIR", &self.repo_dir)
            .env("YB_DBNAME", &self.db_name)
            .env("YB_HOST", &self.yb_host)
            .env("YB_PORT", self.yb_port.to_string())
            .env("COMPUTE_PORT", self.compute_port.to_string())
            .stdout(std::process::Stdio::null())
            .stderr(std::process::Stdio::null())
            .status();
    }

    /// Restart any cluster component the prior scenario left stopped,
    /// then recycle the compute, so each scenario starts from a clean
    /// compute state.
    pub fn prep_for_next_test(&self) -> anyhow::Result<()> {
        let r = &self.repo_dir;
        let sc = r.join("storage_controller_1/storage_controller.pid");
        let ps = r.join("endpoints/main/pageserver/pageserver.pid");
        let sk = r.join("endpoints/main/safekeeper/safekeeper.pid");
        let es = r.join("endpoints/main/endpoint_storage/endpoint_storage.pid");

        if !is_running(&sc) {
            step("restart storage_controller (prior test left it down)");
            self.yb(&["storage_controller", "start"])?;
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
            self.yb(&["endpoint_storage", "start"])?;
        }

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
        if !hit.is_empty() {
            eprintln!("FAIL — correctness counter non-zero:");
            for l in hit {
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
        if !self.success && self.no_fail_clean {
            // --no-fail-clean: leave the cluster + db up for postmortem.
            eprintln!();
            eprintln!("run FAILED — leaving cluster up for inspection:");
            eprintln!("  logs/dir: {}", self.repo_dir.display());
            eprintln!("  YB db:    {}", self.db_name);
            eprintln!(
                "  tear down manually with: YB_LIGHT_REPO_DIR={} YB_DBNAME={} test/e2e/clean.sh",
                self.repo_dir.display(),
                self.db_name
            );
        } else {
            // Default teardown for both a passing and a failing run: drop
            // the cluster + db but keep this run's `*.log` files.
            if !self.success {
                eprintln!();
                eprintln!(
                    "run FAILED — cluster torn down; logs kept at {}",
                    self.repo_dir.display()
                );
                eprintln!("  (re-run with --no-fail-clean to leave the cluster up for postmortem)");
            }
            self.teardown();
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

/// Pick a base for a free port block (base+0 .. base+900) so each run's
/// cluster coexists with concurrent runs and a developer's local cluster.
/// Probes candidate bases (one per service-family lane) and returns the
/// first all-free one; override/pin with `E2E_PORT_BASE`.
fn pick_port_base() -> u16 {
    if let Ok(b) = env_or(&["E2E_PORT_BASE"], "").parse::<u16>() {
        return b;
    }
    for base in (20000u16..=60000).step_by(1000) {
        if (0..=900).step_by(100).all(|d| port_free(base + d)) {
            return base;
        }
    }
    20000 // best-effort: the bind-probe is advisory anyway
}

/// Is `port` bindable on loopback right now? (Advisory free-port check.)
fn port_free(port: u16) -> bool {
    std::net::TcpListener::bind(("127.0.0.1", port)).is_ok()
}
