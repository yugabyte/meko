//! Reproducer / regression guard for the second 2026-07 safekeeper
//! wedge: the timeline manager persisted the control file to YB while
//! holding the timeline shared_state write lock, inside the same loop
//! that services WalResidentTimeline guard requests. A slow
//! `safekeeper.timeline_state` UPDATE therefore froze WAL appends
//! (appends need that lock) AND starved guard issuance (30s guard
//! timeout, manager status stuck at `UpdateControlFile`), wedging every
//! commit until the write returned.
//!
//! The stall is made deterministic with the `sk-cfile-save` failpoint
//! (async sleep in the save path) instead of a saturated YB cluster:
//! restart the SK with a 1s save interval, arm the failpoint, drive
//! writes so `remote_consistent_lsn` advances (making the save fire),
//! then require every probe commit — including one across a compute
//! restart, which needs a fresh residence guard mid-stall — to land
//! within [`PROBE_TIMEOUT`] while the save path is stalled.

use std::path::PathBuf;
use std::process::Command;
use std::time::Duration;

use anyhow::{Context, bail};

use crate::cluster::{Cluster, step};
use crate::db;
use crate::tee_println;

/// How long the failpoint stalls each control-file save. Must exceed
/// the residence-guard acquire timeout (30s) plus probe margin so a
/// regression reliably trips the probes.
const SAVE_STALL: Duration = Duration::from_secs(120);
/// A probe commit must land within this budget at every point of the
/// scenario. Shorter than [`SAVE_STALL`] so a manager blocked on the
/// stalled save cannot ride it out.
const PROBE_TIMEOUT: Duration = Duration::from_secs(45);

pub async fn run(c: &Cluster) -> anyhow::Result<()> {
    let conninfo = db::compute_conn(c.compute_port());
    let http = reqwest::Client::new();
    let failpoints_url = format!("http://127.0.0.1:{}/v1/failpoints", c.sk_http_port());

    // The safekeeper log spans the whole suite run; assert on the
    // delta, not the absolute count.
    let guard_timeouts_baseline = sk_log_count(c, "timeout acquiring in wal_residence_guard");

    step("Restart safekeeper with a 1s control-file save interval");
    c.yb_env(
        &["safekeeper", "restart"],
        &[("YB_SK_CONTROL_FILE_SAVE_INTERVAL", "1s")],
    )?;

    step("Seed load table and confirm commits flow");
    tokio::time::timeout(
        PROBE_TIMEOUT,
        db::batch(
            &conninfo,
            "DROP TABLE IF EXISTS cfile_stall; \
             CREATE TABLE cfile_stall (a BIGINT, b TEXT);",
        ),
    )
    .await
    .map_err(|_| stall_evidence(c, "seed"))??;

    step("Arm the sk-cfile-save failpoint");
    let resp = http
        .put(&failpoints_url)
        .json(&serde_json::json!([{
            "name": "sk-cfile-save",
            "actions": format!("return({})", SAVE_STALL.as_millis()),
        }]))
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

    // Advance remote_consistent_lsn (writes -> PS ingest -> feedback)
    // so the manager has unflushed in-memory state and the 1s-interval
    // save fires into the armed failpoint.
    step("Drive writes until the save path hits the failpoint");
    let deadline = tokio::time::Instant::now() + Duration::from_secs(60);
    loop {
        tokio::time::timeout(
            PROBE_TIMEOUT,
            db::batch(
                &conninfo,
                "INSERT INTO cfile_stall SELECT g, repeat('x', 400) \
                 FROM generate_series(1, 2000) g",
            ),
        )
        .await
        .map_err(|_| stall_evidence(c, "pre-stall insert"))??;

        if sk_log_contains(c, "failpoint \"sk-cfile-save\": sleeping") {
            tee_println!("control-file save is stalled in the failpoint");
            break;
        }
        if tokio::time::Instant::now() > deadline {
            bail!(
                "sk-cfile-save failpoint never hit within 60s — save path \
                 did not fire (check the 1s interval override plumbing)"
            );
        }
        tokio::time::sleep(Duration::from_secs(1)).await;
    }

    // The regression window: the manager is inside the stalled save for
    // SAVE_STALL. Pre-fix it held the shared_state write lock there, so
    // the first probe froze; post-fix the save runs in a background
    // task and everything below proceeds unimpeded.
    step("Probe commits during the stall");
    for i in 0..6 {
        tokio::time::timeout(
            PROBE_TIMEOUT,
            db::batch(&conninfo, "INSERT INTO cfile_stall VALUES (-1, 'probe')"),
        )
        .await
        .map_err(|_| stall_evidence(c, &format!("stall probe {i}")))?
        .context("stall probe insert")?;
        tokio::time::sleep(Duration::from_secs(2)).await;
    }

    step("Compute restart mid-stall (fresh walproposer needs a residence guard)");
    c.yb(&["endpoint", "stop", "main", "--mode", "immediate"])?;
    c.yb(&["endpoint", "start", "main"])?;
    let probe = tokio::time::timeout(
        PROBE_TIMEOUT,
        db::batch(&conninfo, "INSERT INTO cfile_stall VALUES (-2, 'probe')"),
    )
    .await;
    match probe {
        Ok(Ok(())) => {}
        Ok(Err(e)) => {
            // Transient connect errors right after a restart are fine;
            // one more try inside the same budget.
            tee_println!("post-restart probe error ({e:#}); retrying once");
            tokio::time::timeout(
                PROBE_TIMEOUT,
                db::batch(&conninfo, "INSERT INTO cfile_stall VALUES (-2, 'probe')"),
            )
            .await
            .map_err(|_| stall_evidence(c, "post-restart probe (retry)"))?
            .context("post-restart probe insert (retry)")?;
        }
        Err(_) => return Err(stall_evidence(c, "post-restart probe")),
    }

    step("Disarm failpoint, restore default save interval");
    let _ = http
        .put(&failpoints_url)
        .json(&serde_json::json!([{ "name": "sk-cfile-save", "actions": "off" }]))
        .send()
        .await;
    // Plain restart rewrites safekeeper.toml with the default interval.
    c.yb(&["safekeeper", "restart"])?;

    step("Final integrity check");
    let expected = db::query_i64(&conninfo, "SELECT count(*) FROM cfile_stall").await?;
    if expected < 2000 {
        bail!("lost rows: got {expected}");
    }
    let guard_timeouts = sk_log_count(c, "timeout acquiring in wal_residence_guard")
        .saturating_sub(guard_timeouts_baseline);
    if guard_timeouts > 0 {
        bail!("safekeeper logged {guard_timeouts} residence-guard timeout(s) during the stall");
    }
    db::batch(&conninfo, "DROP TABLE cfile_stall").await?;
    Ok(())
}

/// A probe stalled: capture the safekeeper task dump (SIGUSR2, all
/// runtimes) and the wedge signature from its log as the test failure.
fn stall_evidence(c: &Cluster, phase: &str) -> anyhow::Error {
    let sk_dir = c.repo_dir().join("endpoints/main/safekeeper");
    let pid = std::fs::read_to_string(sk_dir.join("safekeeper.pid"))
        .ok()
        .and_then(|s| s.trim().parse::<i32>().ok());
    if let Some(pid) = pid {
        let _ = Command::new("kill").args(["-USR2", &pid.to_string()]).status();
        std::thread::sleep(Duration::from_secs(3));
    }
    let mut evidence = String::new();
    if let Some(log) = newest_log(&sk_dir) {
        if let Ok(text) = std::fs::read_to_string(&log) {
            let guard_timeouts = text.matches("timeout acquiring in wal_residence_guard").count();
            evidence.push_str(&format!(
                "sk log {}: {} residence-guard timeouts\n",
                log.display(),
                guard_timeouts
            ));
            let tail: Vec<&str> = text.lines().rev().take(30).collect();
            for line in tail.iter().rev() {
                evidence.push_str(line);
                evidence.push('\n');
            }
        }
    }
    anyhow::anyhow!(
        "SAFEKEEPER WEDGE: commit did not land within {PROBE_TIMEOUT:?} \
         while the control-file save was stalled ({phase}). {evidence}"
    )
}

fn sk_log_contains(c: &Cluster, needle: &str) -> bool {
    sk_log_count(c, needle) > 0
}

fn sk_log_count(c: &Cluster, needle: &str) -> usize {
    let sk_dir = c.repo_dir().join("endpoints/main/safekeeper");
    let Some(log) = newest_log(&sk_dir) else {
        return 0;
    };
    std::fs::read_to_string(&log)
        .map(|text| text.matches(needle).count())
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
