//! Regression for the control-file-save retry storm (PR #239 review):
//! a FAILED periodic control-file save must not re-spawn every manager tick.
//!
//! Before the fix, `update_control_file_save` advanced the interval clock only
//! on a SUCCESSFUL save (`note_advisory_persisted`), so a failing save left the
//! persisted clock stale and the manager re-spawned the save on every loop
//! iteration (~300 ms) — hammering YB and flooding logs during exactly the
//! outage you don't want to pile onto. The fix gates the spawn on a
//! `cfile_last_attempt` clock advanced on every attempt (success OR failure),
//! bounding retries to one per `control_file_save_interval`.
//!
//! The save is made to fail deterministically with the `sk-cfile-save-fail`
//! failpoint (no real YB outage): restart the SK with a short save interval,
//! arm the failpoint, drive writes so `remote_consistent_lsn` keeps advancing
//! (so the save keeps firing), and require the number of failed-save log lines
//! over the window to stay at ~window/interval — not the per-tick storm.

use std::path::PathBuf;
use std::time::{Duration, Instant};

use anyhow::{Context, bail};

use crate::cluster::{Cluster, step};
use crate::db;
use crate::tee_println;

const SAVE_INTERVAL: &str = "2s";
const WINDOW: Duration = Duration::from_secs(15);
/// ~window/interval ≈ 8 legit retries. The pre-fix storm re-spawned on every
/// manager tick (dozens+). Generous ceiling the fix clears and the bug blows
/// past.
const MAX_FAILED_SAVES: usize = 12;
const MARKER: &str = "sk-cfile-save-fail: injected control-file save failure";

pub async fn run(c: &Cluster) -> anyhow::Result<()> {
    let conninfo = db::compute_conn(c.compute_port());
    let http = reqwest::Client::new();
    let failpoints_url = format!("http://127.0.0.1:{}/v1/failpoints", c.sk_http_port());

    step("Restart safekeeper with a short control-file save interval");
    c.yb_env(
        &["safekeeper", "restart"],
        &[("YB_SK_CONTROL_FILE_SAVE_INTERVAL", SAVE_INTERVAL)],
    )?;

    step("Seed a load table");
    db::batch(
        &conninfo,
        "DROP TABLE IF EXISTS cfile_storm; \
         CREATE TABLE cfile_storm (a BIGINT, b TEXT);",
    )
    .await?;

    // The SK log spans the whole suite; measure the delta, not the absolute.
    let baseline = sk_log_count(c, MARKER);

    step("Arm sk-cfile-save-fail (every control-file save fails)");
    let resp = http
        .put(&failpoints_url)
        .json(&serde_json::json!([{ "name": "sk-cfile-save-fail", "actions": "return" }]))
        .send()
        .await
        .context("PUT /v1/failpoints")?;
    if !resp.status().is_success() {
        bail!(
            "arming failpoint failed: {} {}",
            resp.status(),
            resp.text().await.unwrap_or_default()
        );
    }

    step("Drive writes so the failing save keeps firing for the window");
    let deadline = Instant::now() + WINDOW;
    while Instant::now() < deadline {
        db::batch(
            &conninfo,
            "INSERT INTO cfile_storm SELECT g, repeat('x', 400) FROM generate_series(1, 2000) g",
        )
        .await
        .context("driver insert")?;
        tokio::time::sleep(Duration::from_millis(500)).await;
    }

    step("Disarm the failpoint");
    let _ = http
        .put(&failpoints_url)
        .json(&serde_json::json!([{ "name": "sk-cfile-save-fail", "actions": "off" }]))
        .send()
        .await;

    let failed = sk_log_count(c, MARKER).saturating_sub(baseline);
    tee_println!(
        "failed control-file saves in {WINDOW:?} @ {SAVE_INTERVAL} interval: {failed}"
    );

    // Sanity: the failpoint must actually have fired, else the guard is vacuous.
    if failed == 0 {
        bail!(
            "sk-cfile-save-fail never fired — the save path didn't run (check the interval \
             override / rcl advance), so the retry-storm guard is untested"
        );
    }
    // The guard: a failing save retries at most ~once per interval, not per tick.
    if failed > MAX_FAILED_SAVES {
        bail!(
            "control-file save RETRY STORM: {failed} failed saves in {WINDOW:?} at a \
             {SAVE_INTERVAL} interval (ceiling {MAX_FAILED_SAVES}) — a failed save is \
             re-spawning faster than once per interval"
        );
    }
    tee_println!("  ok — failed saves bounded to ~once per interval (no retry storm)");

    step("Disarm + restore the default save interval");
    c.yb(&["safekeeper", "restart"])?;
    db::batch(&conninfo, "DROP TABLE IF EXISTS cfile_storm").await?;
    Ok(())
}

fn sk_log_count(c: &Cluster, needle: &str) -> usize {
    let sk_dir = c.repo_dir().join("endpoints/main/safekeeper");
    let Some(log) = newest_log(&sk_dir) else {
        return 0;
    };
    std::fs::read_to_string(&log)
        .map(|t| t.matches(needle).count())
        .unwrap_or(0)
}

fn newest_log(dir: &std::path::Path) -> Option<PathBuf> {
    let mut logs: Vec<(std::time::SystemTime, PathBuf)> = std::fs::read_dir(dir)
        .ok()?
        .flatten()
        .filter(|e| e.path().extension().is_some_and(|x| x == "log"))
        .filter_map(|e| Some((e.metadata().ok()?.modified().ok()?, e.path())))
        .collect();
    logs.sort();
    logs.pop().map(|(_, p)| p)
}
