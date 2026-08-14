//! Reproducer for #765: a stalled YB WAL consumer leaves the safekeeper in
//! a retry loop that cannot make progress.
//!
//! `wait_for_durable` runs inside `process_msg`, which holds the timeline
//! shared_state WRITE lock, and it is reached from `flush_wal`
//! (`handle_elected`) and `truncate_wal` (ProposerElected). It bails at
//! `YB_DURABLE_WAIT_MAX` (60s) and drops the connection so the lock is
//! released — but the walproposer then reconnects, and the reconnect's
//! ProposerElected re-enters the SAME wait, for the same target, against
//! the same unmoved watermark. That is the wedge: not one blocked call but
//! a cycle that re-arms itself, which is why the reported endpoint sat at
//! zero WAL progress across 42 minutes and 229 reconnects.
//!
//! In production the stall is a YB tablet-split storm making the batch
//! INSERT take minutes. Here it is the `sk-yb-wal-flush` failpoint (async
//! sleep before the batch write, so the durability watermark stands
//! still), which reproduces the loop without a saturated cluster.
//!
//! Two things this deliberately does NOT assert, both learned from running
//! it:
//!
//! - **Commits freezing during the stall is not the bug.** `flush_lsn()` is
//!   capped at `durable_lsn`, so a frozen watermark means the safekeeper
//!   correctly declines to ack and `synchronous_commit` parks the backend.
//!   A synchronous system whose store stopped accepting writes is supposed
//!   to block. Asserting on it fails for a reason that is not #765.
//! - **A reconnect is the trigger, not a consequence.** Steady streaming
//!   (`write_wal`, `yb_flush(Notify)`) never waits, so a stall alone never
//!   enters the lock-held path; the fixture has to force a reconnect. This
//!   is why the guard-timeout counter can read zero here while production
//!   logged 205 — that symptom additionally needs concurrent guard demand
//!   the single-endpoint fixture does not generate. It is checked, not
//!   relied on; the load-bearing assertion is recovery.
//!
//! Sibling of `sk_cfile_stall`: same class of write-lock starvation,
//! different holder — there the control-file save, here the durability
//! wait.

use std::path::PathBuf;
use std::process::Command;
use std::time::Duration;

use anyhow::{Context, bail};

use crate::cluster::{Cluster, step};
use crate::db;
use crate::tee_println;

/// How long the failpoint stalls each consumer batch.
///
/// Longer than `YB_DURABLE_WAIT_MAX` (60s) so a durability wait entered
/// during the stall is guaranteed to time out at least once — but kept
/// short, because disarming the failpoint does NOT wake a consumer already
/// inside its sleep. Anything longer than the recovery budget below and
/// the test measures its own sleep rather than the timeline's ability to
/// come back.
const FLUSH_STALL: Duration = Duration::from_secs(150);
/// Budget for the setup commits, before any stall is armed.
const PROBE_TIMEOUT: Duration = Duration::from_secs(45);
/// How long to watch for guard starvation once a reconnect has driven the
/// safekeeper into a lock-held durability wait.
///
/// Long enough to span a full `YB_DURABLE_WAIT_MAX` (60s) wait and the
/// reconnect that follows it, so the retry loop turns over at least once
/// inside the window; bounded by [`FLUSH_STALL`] so what it observes is
/// the stall, not the consumer waking up.
const GUARD_WATCH: Duration = Duration::from_secs(75);
/// After the failpoint is disarmed the timeline must serve a commit again
/// within this budget. Sized to absorb the tail of an in-flight
/// [`FLUSH_STALL`] sleep plus the consumer's drain: disarming stops future
/// sleeps but cannot cut short the one already running, and a budget that
/// expires inside it would report a wedge that is really just the fixture.
/// The reported endpoint never recovered at all — that is the bug.
const RECOVERY_TIMEOUT: Duration = Duration::from_secs(240);
/// Rows per driving insert. Enough chunks to fill the consumer's bounded
/// channel (`CHUNK_CHANNEL_CAPACITY`), so the backpressure path is
/// exercised alongside the durability wait.
const DRIVE_ROWS: i64 = 20000;

pub async fn run(c: &Cluster) -> anyhow::Result<()> {
    let conninfo = db::compute_conn(c.compute_port());
    let http = reqwest::Client::new();
    let failpoints_url = format!("http://127.0.0.1:{}/v1/failpoints", c.sk_http_port());

    // The safekeeper log spans the whole suite run; assert on the delta.
    let guard_timeouts_baseline = sk_log_count(c, "timeout acquiring in wal_residence_guard");

    step("Seed load table and confirm commits flow");
    tokio::time::timeout(
        PROBE_TIMEOUT,
        db::batch(
            &conninfo,
            "DROP TABLE IF EXISTS yb_durable_stall; \
             CREATE TABLE yb_durable_stall (a BIGINT, b TEXT);",
        ),
    )
    .await
    .map_err(|_| stall_evidence(c, "seed", PROBE_TIMEOUT))??;

    step("Arm the sk-yb-wal-flush failpoint");
    let resp = http
        .put(&failpoints_url)
        .json(&serde_json::json!([{
            "name": "sk-yb-wal-flush",
            "actions": format!("return({})", FLUSH_STALL.as_millis()),
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

    // One insert is enough to hand the consumer a batch; it stalls on the
    // first one. This insert is itself expected to block once the channel
    // fills, so it gets its own budget and a stall is not fatal here —
    // the probes below are the assertion.
    step("Drive writes until the consumer is stalled in the failpoint");
    let drive = tokio::time::timeout(
        PROBE_TIMEOUT,
        db::batch(
            &conninfo,
            &format!(
                "INSERT INTO yb_durable_stall SELECT g, repeat('x', 400) \
                 FROM generate_series(1, {DRIVE_ROWS}) g"
            ),
        ),
    )
    .await;
    match drive {
        Ok(r) => r.context("driving insert")?,
        Err(_) => tee_println!("driving insert is itself blocked — consumer backpressure is in"),
    }
    if !sk_log_contains(c, "failpoint \"sk-yb-wal-flush\": sleeping") {
        bail!(
            "sk-yb-wal-flush failpoint never hit — the consumer never reached \
             the batch write (check the failpoint plumbing and that the SK was \
             built with --features testing)"
        );
    }
    tee_println!("YB WAL consumer is stalled in the failpoint");

    // Commits freezing here is NOT the bug and is not asserted on: with the
    // watermark frozen, `flush_lsn()` (capped at `durable_lsn`) cannot
    // advance, so the safekeeper correctly declines to ack and
    // `synchronous_commit` parks the backend. A synchronous system whose
    // store has stopped accepting writes is supposed to block.
    //
    // The bug is that this state does not stay confined to throughput, and
    // does not end when the stall does. Forcing a walproposer reconnect is
    // what makes the difference: reconnecting issues ProposerElected, whose
    // `truncate_wal` calls `wait_for_durable` INSIDE `process_msg` — so from
    // here the timeline write lock is held across the wait, the manager's
    // read starves behind it, and guard requests begin timing out. Steady
    // streaming never enters that wait (`write_wal` and `yb_flush(Notify)`
    // do not block), which is why a reconnect is the trigger rather than a
    // consequence.
    step("Force a walproposer reconnect while the consumer is stalled");
    c.yb(&["endpoint", "stop", "main", "--mode", "immediate"])?;
    // The compute's startup sync-safekeepers is itself a `flush_wal` →
    // `wait_for_durable`, so `start` may not return while the stall holds.
    // Its exit status is not the signal; the safekeeper's behaviour is.
    let started = c.yb(&["endpoint", "start", "main"]);
    if let Err(e) = &started {
        tee_println!("endpoint start did not complete during the stall ({e:#}) — expected");
    }

    // A guard request only starves if it arrives WHILE the write lock is
    // held, and the lock is held only for the duration of a durability
    // wait. Waiting for the wait's first 10s progress log and only then
    // forcing a fresh guard request is what makes the collision
    // deterministic — issued before the wait starts, or after it bails at
    // 60s, the request is served immediately and nothing is observed.
    // Restarting the pageserver is the lever: its WAL sender has to take a
    // new `WalResidentTimeline` guard when it reconnects.
    step("Wait until a durability wait is in flight");
    let waits_before = sk_log_count(c, "still waiting for YB WAL durability");
    let wait_deadline = tokio::time::Instant::now() + Duration::from_secs(45);
    let mut wait_in_flight = false;
    while tokio::time::Instant::now() < wait_deadline {
        if sk_log_count(c, "still waiting for YB WAL durability") > waits_before {
            wait_in_flight = true;
            break;
        }
        tokio::time::sleep(Duration::from_secs(2)).await;
    }
    if !wait_in_flight {
        bail!(
            "no durability wait started within 45s of the reconnect — the reconnect \
             did not reach `truncate_wal`/`flush_wal`, so the write lock is not held \
             and this fixture cannot observe guard starvation"
        );
    }
    tee_println!("a durability wait is in flight; the timeline write lock is held");

    // It takes more than one request. The manager's loop is
    //
    //     loop { state_snapshot().await; ...; select! { ... manager_rx.recv() } }
    //
    // and `state_snapshot()` is where it takes the shared_state READ lock
    // (`timeline_manager.rs`). A request that lands while the loop is parked
    // in the `select!` is answered straight away — the guard path itself
    // never touches shared_state. Only afterwards does the loop come round
    // and block in `state_snapshot()` behind the writer, and every request
    // from then on waits out its full 30s. So the FIRST guard request is
    // always served and the SECOND is the one that starves, which is why a
    // single collision observes nothing and production — with many
    // concurrent senders/receivers — logged 205 timeouts. The manager status
    // it reports while stuck (`StateSnapshot => StateSnapshot`) is the same
    // one in the issue.
    step("Force repeated guard requests into the held write lock");
    for i in 0..3 {
        c.yb_quiet(&["pageserver", "stop", "main", "-m", "immediate"]);
        if let Err(e) = c.yb(&["pageserver", "start", "main"]) {
            tee_println!("pageserver start {i} did not complete during the stall ({e:#})");
        }
        tokio::time::sleep(Duration::from_secs(3)).await;
    }

    step("Watch for guard starvation while the stall holds");
    let watch_deadline = tokio::time::Instant::now() + GUARD_WATCH;
    let mut guard_timeouts = 0usize;
    while tokio::time::Instant::now() < watch_deadline {
        guard_timeouts = sk_log_count(c, "timeout acquiring in wal_residence_guard")
            .saturating_sub(guard_timeouts_baseline);
        if guard_timeouts > 0 {
            break;
        }
        tokio::time::sleep(Duration::from_secs(5)).await;
    }
    let durable_waits = sk_log_count(c, "still waiting for YB WAL durability");
    let wait_timeouts = sk_log_count(c, "YB WAL durability wait timed out");
    let reconnects = sk_log_count(c, "received ProposerElected");
    tee_println!(
        "during the stall: {guard_timeouts} residence-guard timeouts, \
         {durable_waits} durability-wait progress logs, {wait_timeouts} durability-wait \
         timeouts, {reconnects} ProposerElected"
    );

    // Let the consumer drain, then require the timeline to come back. This
    // is the assertion that separates "slow while YB is slow" from #765:
    // the reported endpoint never recovered, 42 minutes and 229 reconnects
    // in, because every reconnect re-entered the same lock-held wait.
    step("Disarm the failpoint and require recovery");
    let _ = http
        .put(&failpoints_url)
        .json(&serde_json::json!([{ "name": "sk-yb-wal-flush", "actions": "off" }]))
        .send()
        .await;
    // A compute whose start was cut short by the stall needs another one
    // before it can serve; a stop/start pair is idempotent enough to retry.
    let _ = c.yb(&["endpoint", "stop", "main", "--mode", "immediate"]);
    c.yb(&["endpoint", "start", "main"])
        .context("endpoint start after the stall was lifted")?;
    tokio::time::timeout(
        RECOVERY_TIMEOUT,
        db::batch(&conninfo, "INSERT INTO yb_durable_stall VALUES (-2, 'recovered')"),
    )
    .await
    .map_err(|_| stall_evidence(c, "post-stall recovery", RECOVERY_TIMEOUT))?
    .context("post-stall recovery insert")?;
    tee_println!("timeline recovered after the stall was lifted");

    step("Final integrity check");
    let probes = db::query_i64(&conninfo, "SELECT count(*) FROM yb_durable_stall WHERE a < 0")
        .await?;
    if probes < 1 {
        bail!("recovery probe row did not survive: got {probes}");
    }
    let guard_timeouts = sk_log_count(c, "timeout acquiring in wal_residence_guard")
        .saturating_sub(guard_timeouts_baseline);
    if guard_timeouts > 0 {
        bail!(
            "safekeeper logged {guard_timeouts} residence-guard timeout(s) — a stalled \
             consumer starved the manager, so a durability wait is still being held \
             across the timeline write lock (#765)"
        );
    }
    db::batch(&conninfo, "DROP TABLE yb_durable_stall").await?;
    Ok(())
}

/// A probe stalled: capture the safekeeper task dump (SIGUSR2, all
/// runtimes) and the wedge signature from its log as the test failure.
fn stall_evidence(c: &Cluster, phase: &str, budget: Duration) -> anyhow::Error {
    let sk_dir = c.repo_dir().join("endpoints/main/safekeeper");
    let pid = std::fs::read_to_string(sk_dir.join("safekeeper.pid"))
        .ok()
        .and_then(|s| s.trim().parse::<i32>().ok());
    if let Some(pid) = pid {
        let _ = Command::new("kill")
            .args(["-USR2", &pid.to_string()])
            .status();
        std::thread::sleep(Duration::from_secs(3));
    }
    let mut evidence = String::new();
    if let Some(log) = newest_log(&sk_dir) {
        if let Ok(text) = std::fs::read_to_string(&log) {
            // The issue's counter table: guard starvation, the durability
            // wait that causes it, and the walproposer churn it produces.
            evidence.push_str(&format!(
                "sk log {}: {} residence-guard timeouts, {} durability-wait timeouts, \
                 {} ProposerElected (walproposer reconnects)\n",
                log.display(),
                text.matches("timeout acquiring in wal_residence_guard").count(),
                text.matches("YB WAL durability wait timed out").count(),
                text.matches("received ProposerElected").count(),
            ));
            let tail: Vec<&str> = text.lines().rev().take(30).collect();
            for line in tail.iter().rev() {
                evidence.push_str(line);
                evidence.push('\n');
            }
        }
    }
    anyhow::anyhow!(
        "SAFEKEEPER WEDGE (#765): commit did not land within {budget:?} \
         while the YB WAL consumer was stalled ({phase}). {evidence}"
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
