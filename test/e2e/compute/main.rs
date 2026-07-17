//! End-to-end test driver. Brings up one yb_light_local cluster against
//! an ephemeral YB database, then runs the requested tests in a fixed
//! order, scanning the pageserver log for corruption markers after each.
//!
//! All tests share a single cluster + `main` branch + endpoint to
//! minimize setup cost; between tests the cluster is recycled (any
//! component the prior test left down is restarted, a wedged pageserver
//! is force-restarted, and the compute endpoint is recreated for a clean
//! compute state). The `main` branch persists across tests, so tests use
//! unique table names.
//!
//! This is intentionally a binary, not a `cargo test` target: the
//! shared-cluster, fixed-order, gate-after-each, whole-selection-on-one-
//! cluster model (unit, then fast, then slow) can't be expressed with the
//! parallel/unordered default test harness. `test/e2e/compute/run.sh`
//! wraps it.
//!
//! Usage:
//!   e2e_tests                  # unit then fast (the default gate)
//!   e2e_tests unit             # only the live-DB `cargo test` modules
//!                              # (page_store, page_writer, yb_wal_storage)
//!                              # pointed at this run's cluster
//!   e2e_tests fast             # only the fast suite
//!   e2e_tests slow             # only the slow suite
//!   e2e_tests all              # unit, fast, then slow
//!   e2e_tests <name>           # one test by name, e.g. `branch_ancestor`
//!                              # or `smoke` (substring match also works)
//!   e2e_tests -n <N> [sel]     # repeat the selection N times and print a
//!                              # cumulative pass/fail count. Implies
//!                              # run-all (failures are tallied, not fatal)
//!                              # — useful for flakiness hunting, e.g.
//!                              # `e2e_tests -n 20 restart_wal_replay_yb`.

mod cluster;
pub(crate) use e2e_common::db;
mod page_api;
mod psql;
mod scenarios;
mod sk_api;
mod workload;

use std::future::Future;
use std::path::PathBuf;
use std::pin::Pin;
use std::process::ExitCode;

use cluster::Cluster;

#[derive(Clone, Copy, PartialEq)]
enum Category {
    Fast,
    Slow,
}

impl Category {
    fn dir(self) -> &'static str {
        match self {
            Category::Fast => "fast",
            Category::Slow => "slow",
        }
    }
}

/// One run-order entry: the live-DB `cargo test` modules, or a whole
/// scenario category.
#[derive(Clone, Copy)]
enum Selection {
    Unit,
    Category(Category),
}

enum Mode {
    /// Run selections in order. Default is `unit` then `fast`; `all` is
    /// `unit`, `fast`, then `slow`.
    Selections(Vec<Selection>),
    /// Run only tests whose name matches (exact or substring).
    Filter(String),
}

/// One executable test: a scenario future or a `.sql` golden workload.
enum Item<'a> {
    Scenario(Pin<Box<dyn Future<Output = anyhow::Result<()>> + 'a>>),
    Sql(PathBuf),
}

#[derive(Default)]
struct Tally {
    passed: u64,
    failed: u64,
}

struct Args {
    mode: Mode,
    /// Number of times to repeat the selection.
    repeat: u32,
    /// `true` (default) aborts on the first failure; `-n` flips it to
    /// run-all-and-tally.
    fail_fast: bool,
}

#[tokio::main]
async fn main() -> ExitCode {
    let args = match parse_args() {
        Ok(a) => a,
        Err(msg) => {
            eprintln!("{msg}");
            eprintln!("usage: e2e_tests [-n <count>] [unit|fast|slow|all|<test-name>]");
            return ExitCode::from(2);
        }
    };

    match run(args).await {
        Ok(()) => ExitCode::SUCCESS,
        Err(e) => {
            eprintln!("\nE2E FAILED: {e:#}");
            ExitCode::FAILURE
        }
    }
}

fn parse_args() -> Result<Args, String> {
    let mut repeat: u32 = 1;
    let mut fail_fast = true;
    let mut selector: Option<String> = None;

    let mut it = std::env::args().skip(1);
    while let Some(arg) = it.next() {
        let count = if arg == "-n" {
            Some(it.next().ok_or_else(|| "-n requires a count".to_string())?)
        } else {
            // `-n=5` or `-n5`
            arg.strip_prefix("-n=")
                .or_else(|| arg.strip_prefix("-n"))
                .map(str::to_string)
        };

        if let Some(count) = count {
            repeat = count
                .parse::<u32>()
                .ok()
                .filter(|&n| n >= 1)
                .ok_or_else(|| format!("invalid -n count: {count:?}"))?;
            fail_fast = false; // -n opts into run-all + tally
        } else if selector.is_none() {
            selector = Some(arg);
        } else {
            return Err(format!("unexpected extra argument: {arg:?}"));
        }
    }

    let mode = match selector.as_deref() {
        None => Mode::Selections(vec![Selection::Unit, Selection::Category(Category::Fast)]),
        Some("unit") => Mode::Selections(vec![Selection::Unit]),
        Some("fast") => Mode::Selections(vec![Selection::Category(Category::Fast)]),
        Some("slow") => Mode::Selections(vec![Selection::Category(Category::Slow)]),
        Some("all") => Mode::Selections(vec![
            Selection::Unit,
            Selection::Category(Category::Fast),
            Selection::Category(Category::Slow),
        ]),
        Some(name) => Mode::Filter(name.to_string()),
    };

    Ok(Args {
        mode,
        repeat,
        fail_fast,
    })
}

async fn run(args: Args) -> anyhow::Result<()> {
    let Args {
        mode,
        repeat,
        mut fail_fast,
    } = args;
    // #57: stop at the FIRST failing iteration while still running
    // multiple iterations for churn. `-n` normally disables fail_fast
    // (run-all + tally); this env flips it back so the failing
    // iteration's cluster state is preserved uncontaminated (pair with
    // --no-fail-stop to keep the cluster live for forensics).
    if std::env::var("E2E_STOP_ON_FAIL").is_ok() {
        fail_fast = true;
    }
    let mut cluster = Cluster::setup().await?;

    let mut tally = Tally::default();
    for iter in 1..=repeat {
        if repeat > 1 {
            cluster::step(&format!("iteration {iter}/{repeat}"));
        }
        // Rebuild the item list each iteration: scenario futures are
        // one-shot, so they can't be reused across repeats.
        let items = collect_items(&cluster, &mode)?;
        for (name, item) in items {
            let res = match item {
                Item::Scenario(fut) => cluster.run_scenario(&name, fut).await,
                Item::Sql(path) => cluster.run_sql(&name, &path).await,
            };
            match res {
                Ok(()) => tally.passed += 1,
                Err(e) => {
                    tally.failed += 1;
                    eprintln!("FAIL — {name}: {e:#}");
                    if fail_fast {
                        return Err(e);
                    }
                }
            }
        }
    }

    if !fail_fast {
        cluster::step(&format!(
            "cumulative: {} passed, {} failed (over {repeat} iteration(s))",
            tally.passed, tally.failed
        ));
    }

    cluster.final_verdict().await?;

    if tally.failed > 0 {
        anyhow::bail!("{} test execution(s) failed", tally.failed);
    }
    cluster.mark_success();
    Ok(())
}

/// Build the ordered list of tests to run for `mode`, in run order:
/// scenarios (lexical) then `.sql` workloads (lexical), per category.
/// Scenarios come from the build-time registry; `.sql` workloads are
/// discovered at runtime.
fn collect_items<'a>(c: &'a Cluster, mode: &Mode) -> anyhow::Result<Vec<(String, Item<'a>)>> {
    let mut items: Vec<(String, Item<'a>)> = Vec::new();

    match mode {
        Mode::Selections(sels) => {
            for &sel in sels {
                match sel {
                    Selection::Unit => items.push((
                        "live_db_unit_tests".to_string(),
                        Item::Scenario(Box::pin(unit_tests(c))),
                    )),
                    Selection::Category(cat) => push_category(c, cat, &mut items),
                }
            }
        }
        Mode::Filter(query) => {
            let matches = |name: &str| name == query || name.contains(query);
            push_category(c, Category::Fast, &mut items);
            push_category(c, Category::Slow, &mut items);
            items.retain(|(name, _)| matches(name));
            if items.is_empty() {
                anyhow::bail!(
                    "no test matched {query:?}; available: {}",
                    all_test_names(c).join(", ")
                );
            }
        }
    }

    // `E2E_SKIP=<substr>[,<substr>...]` drops matching tests from the run
    // (e.g. a known-flaky one). Reported, never silent.
    if let Ok(skip) = std::env::var("E2E_SKIP") {
        let pats: Vec<&str> = skip
            .split(',')
            .map(str::trim)
            .filter(|s| !s.is_empty())
            .collect();
        if !pats.is_empty() {
            let mut skipped: Vec<String> = Vec::new();
            items.retain(|(name, _)| {
                if pats.iter().any(|p| name.contains(p)) {
                    skipped.push(name.clone());
                    false
                } else {
                    true
                }
            });
            if !skipped.is_empty() {
                eprintln!(
                    "E2E_SKIP: skipping {} test(s): {}",
                    skipped.len(),
                    skipped.join(", ")
                );
            }
        }
    }
    Ok(items)
}

/// Append a category's tests (scenarios then `.sql` workloads) to `items`.
fn push_category<'a>(c: &'a Cluster, cat: Category, items: &mut Vec<(String, Item<'a>)>) {
    let scenarios = match cat {
        Category::Fast => scenarios::fast(c),
        Category::Slow => scenarios::slow(c),
    };
    for (name, fut) in scenarios {
        items.push((name.to_string(), Item::Scenario(fut)));
    }
    for name in sql_names(cat) {
        let path = sql_path(cat, &name);
        items.push((name, Item::Sql(path)));
    }
    // Surface disabled workloads (`<name>.sql.disabled`) so they're
    // visibly skipped, never silently dropped.
    let disabled = disabled_sql_names(cat);
    if !disabled.is_empty() {
        eprintln!(
            "disabled (skipped) {} workload(s) in {}/: {}",
            disabled.len(),
            cat.dir(),
            disabled.join(", ")
        );
    }
}

/// Run the live-DB unit tests against this run's cluster. Two suites, both
/// needing a real YB (which CI otherwise lacks) — the ephemeral database this
/// cluster's storage_controller just migrated is that YB, isolated per run:
///
/// 1. The pageserver/safekeeper module tests (`page_store::tests`,
///    `page_writer::tests`, `yb_wal_storage::tests`) — assert against the
///    production `pageserver.*`/`safekeeper.*` schema over YSQL.
/// 2. The `yb_tsclient` live integration suite (`tests/live.rs`, all
///    `#[ignore]` "requires a live YugabyteDB") — exercises the tserver-RPC
///    client (PgClientService on port 9100) and creates its own tables.
async fn unit_tests(c: &Cluster) -> anyhow::Result<()> {
    // pageserver/safekeeper live-DB module tests (YSQL, migrated schema).
    // `pageserver/testing` matches the feature set run.sh built the cluster
    // binaries (and thus the pageserver lib) with, for artifact reuse.
    // Trailing `::` keeps the filters to the modules' own tests.
    let mut ps_sk = cargo_test_cmd();
    ps_sk.args([
        "-p",
        "pageserver",
        "-p",
        "safekeeper",
        // Match the exact feature set run.sh built the cluster binaries with
        // (both testing features) so cargo reuses those artifacts instead of
        // recompiling safekeeper under a different feature set.
        "--features",
        "pageserver/testing,safekeeper/testing",
        "--lib",
        "--",
        "page_store::tests::",
        "page_writer::tests::",
        "yb_wal_storage::tests::",
    ]);
    ps_sk
        .env("YB_TEST_HOST", c.yb_host())
        .env("YB_TEST_PORT", c.yb_port().to_string())
        .env("YB_TEST_DB", c.db_name());
    run_cargo_test(ps_sk, "pageserver/safekeeper unit tests").await?;

    // yb_tsclient live integration suite (tserver RPC on :9100; creates its
    // own tables via YSQL in this run's ephemeral db). `--ignored` runs the
    // otherwise-skipped live tests; the 3-endpoint spread test self-skips
    // against a single-node cluster.
    let mut tsclient = cargo_test_cmd();
    tsclient.args(["-p", "yb_tsclient", "--test", "live", "--", "--ignored"]);
    tsclient
        .env("YB_LIVE_TSERVERS", format!("{}:9100", c.yb_host()))
        .env("YB_LIVE_YSQL", format!("{}:{}", c.yb_host(), c.yb_port()))
        .env("YB_LIVE_DB", c.db_name());
    run_cargo_test(tsclient, "yb_tsclient live tests").await
}

/// A `cargo test` command matching this harness's own build profile, so the
/// artifacts run.sh just built are reused instead of recompiled under the
/// other profile.
fn cargo_test_cmd() -> tokio::process::Command {
    let mut cmd = tokio::process::Command::new("cargo");
    cmd.arg("test");
    if !cfg!(debug_assertions) {
        cmd.arg("--release");
    }
    cmd
}

/// Spawn a prepared `cargo test` command, stream its output through
/// `tee_line` (each line lands in the run log AND counts as progress, so a
/// quiet stretch — test-binary compile, a slow test — doesn't trip
/// run_scenario's no-progress backstop), and fail if it exits non-zero.
async fn run_cargo_test(mut cmd: tokio::process::Command, label: &str) -> anyhow::Result<()> {
    use anyhow::Context as _;
    use tokio::io::{AsyncBufReadExt, AsyncRead, BufReader};

    cmd.stdout(std::process::Stdio::piped())
        .stderr(std::process::Stdio::piped());
    let mut child = cmd.spawn().with_context(|| format!("spawn {label}"))?;

    async fn forward(stream: impl AsyncRead + Unpin) -> std::io::Result<()> {
        let mut lines = BufReader::new(stream).lines();
        while let Some(line) = lines.next_line().await? {
            cluster::tee_line(&line);
        }
        Ok(())
    }
    let stdout = child.stdout.take().expect("stdout piped");
    let stderr = child.stderr.take().expect("stderr piped");
    let (status, out_res, err_res) = tokio::join!(child.wait(), forward(stdout), forward(stderr));
    let status = status.with_context(|| format!("wait for {label}"))?;
    out_res.with_context(|| format!("read {label} stdout"))?;
    err_res.with_context(|| format!("read {label} stderr"))?;
    anyhow::ensure!(status.success(), "{label}: cargo test exited with {status}");
    Ok(())
}

/// Every test name, for the "no match" error message.
fn all_test_names(c: &Cluster) -> Vec<String> {
    scenarios::fast(c)
        .into_iter()
        .chain(scenarios::slow(c))
        .map(|(name, _)| name.to_string())
        .chain(sql_names(Category::Fast))
        .chain(sql_names(Category::Slow))
        .collect()
}

/// The category directory next to this crate's manifest.
fn category_dir(cat: Category) -> PathBuf {
    PathBuf::from(env!("CARGO_MANIFEST_DIR")).join(cat.dir())
}

/// `.sql` workload names (file stems) in a category, lexically sorted.
/// Discovered at runtime, so adding a workload needs no recompile.
fn sql_names(cat: Category) -> Vec<String> {
    let mut names: Vec<String> = std::fs::read_dir(category_dir(cat))
        .into_iter()
        .flatten()
        .filter_map(Result::ok)
        .filter_map(|entry| {
            let path = entry.path();
            if path.extension().and_then(|e| e.to_str()) == Some("sql") {
                path.file_stem().and_then(|s| s.to_str()).map(String::from)
            } else {
                None
            }
        })
        .collect();
    names.sort();
    names
}

/// Resolve a `.sql` workload path (`<category>/<name>.sql`).
fn sql_path(cat: Category, name: &str) -> PathBuf {
    category_dir(cat).join(format!("{name}.sql"))
}

/// Names of disabled `.sql` workloads in a category — files named
/// `<name>.sql.disabled`. The runner never executes these; they're
/// reported as skipped so a disabled test stays visible. Re-enable by
/// stripping the `.disabled` suffix (from both `.sql` and `.out`).
fn disabled_sql_names(cat: Category) -> Vec<String> {
    let mut names: Vec<String> = std::fs::read_dir(category_dir(cat))
        .into_iter()
        .flatten()
        .filter_map(Result::ok)
        .filter_map(|entry| {
            entry
                .file_name()
                .to_str()
                .and_then(|n| n.strip_suffix(".sql.disabled"))
                .map(String::from)
        })
        .collect();
    names.sort();
    names
}
