//! The sustained-outage half of #770, complementing the fast suite's
//! single-failure test. The contract under a sustained flush failure is:
//!
//! - A synchronous commit STALLS — and that is correct, not asserted against.
//!   The backend parks on SyncRep, and a dropped safekeeper connection does
//!   not error a SyncRep waiter; the walproposer reconnects and the wait
//!   continues. (First cut of this test asserted the commit errors inside the
//!   fail-fast bound; it hung for the full budget, which is how this doc
//!   learned what the signal does and does not buy.)
//! - The commit is never LOST: it completes once flushes succeed again.
//! - The safekeeper never wedges around it: the fail-fast signal keeps
//!   `wait_for_durable` callers from camping on the timeline write lock, so
//!   the manager keeps serving residence guards throughout — zero guard
//!   timeouts is the observable.
//! - Recovery is autonomous: no restart, `Failed` clears on the next
//!   successful batch (`DurableState::Failed` is "currently failing", not
//!   terminal).
//!
//! Each wrong implementation fails a distinct assertion: an exiting consumer
//! (original #770) never completes the stalled commit; one that retries but
//! never signals leaves waiters camping on the lock (guard timeouts); a
//! signal that never clears fails the recovery half.

use std::time::Duration;

use anyhow::{Context, bail};

use crate::cluster::{Cluster, step};
use crate::db;
use crate::tee_println;

/// How long the flush keeps failing. Longer than the consumer's fail-fast
/// bound (`CONSUMER_RETRY_SIGNAL_AFTER`, 20s) plus margin, so the signalled
/// window is what the assertions observe.
const OUTAGE: Duration = Duration::from_secs(60);
/// After the failpoint is disarmed, the stalled commit and a fresh one must
/// both land within this budget — the batch retries at most 5s apart, so
/// recovery is one backoff step plus the flush itself.
const RECOVERY_TIMEOUT: Duration = Duration::from_secs(60);

pub async fn run(c: &Cluster) -> anyhow::Result<()> {
    let conninfo = db::compute_conn(c.compute_port());
    let http = reqwest::Client::new();
    let failpoints_url = format!("http://127.0.0.1:{}/v1/failpoints", c.sk_http_port());

    step("Seed load table and confirm commits flow");
    tokio::time::timeout(
        Duration::from_secs(45),
        db::batch(
            &conninfo,
            "DROP TABLE IF EXISTS wal_failfast; \
             CREATE TABLE wal_failfast (a BIGINT, b TEXT);",
        ),
    )
    .await
    .context("seed timed out")??;

    // `return` with no count: every flush fails until disarmed.
    step("Arm sk-wal-direct-flush-fail for a sustained outage");
    let resp = http
        .put(&failpoints_url)
        .json(&serde_json::json!([{
            "name": "sk-wal-direct-flush-fail",
            "actions": "return",
        }]))
        .send()
        .await
        .context("PUT /v1/failpoints")?;
    if !resp.status().is_success() {
        bail!("arming failpoint failed: {}", resp.status());
    }
    let guard_timeouts_baseline = sk_log_count(c, "timeout acquiring in wal_residence_guard");
    let disarm_at = tokio::time::Instant::now() + OUTAGE;

    // The commit stalls on SyncRep for the whole outage — that is synchronous
    // semantics, not the bug — so it runs in the background. What must NOT
    // happen is losing it: it has to complete once flushes succeed again.
    step("Start a commit that will stall on SyncRep for the whole outage");
    let stalled_conninfo = conninfo.clone();
    let stalled = tokio::spawn(async move {
        db::batch(
            &stalled_conninfo,
            "INSERT INTO wal_failfast VALUES (1, 'during-outage')",
        )
        .await
    });

    // Ride out the outage watching the two things that distinguish a stall
    // from a wedge: the fail-fast signal fires (waiters are not camping on
    // the timeline write lock), and the manager keeps serving guards.
    step("During the outage: signal fires, manager stays responsive");
    let mut signal_seen = false;
    while tokio::time::Instant::now() < disarm_at {
        signal_seen = signal_seen || sk_log_contains(c, "failing waiters fast while retrying");
        tokio::time::sleep(Duration::from_secs(5)).await;
    }
    if stalled.is_finished() {
        // It cannot have committed durably while every flush fails; a finish
        // here is either a lost-commit bug or the failpoint not covering the
        // write path.
        let res = stalled.await;
        bail!(
            "the stalled commit finished DURING the outage ({res:?}) — either the \
             failpoint is not covering the write path or a commit was acked \
             without durability"
        );
    }
    if !signal_seen {
        bail!(
            "the consumer never published its fail-fast signal during a {OUTAGE:?} \
             outage — waiters were left camping on `wait_for_durable`'s full bound"
        );
    }
    let guard_timeouts = sk_log_count(c, "timeout acquiring in wal_residence_guard")
        .saturating_sub(guard_timeouts_baseline);
    if guard_timeouts > 0 {
        bail!(
            "{guard_timeouts} residence-guard timeout(s) during the outage — a \
             durability wait is camping on the timeline write lock despite the \
             fail-fast signal"
        );
    }
    tee_println!("outage rode out: signal fired, zero guard timeouts");

    step("Disarm the failpoint");
    let _ = http
        .put(&failpoints_url)
        .json(&serde_json::json!([{ "name": "sk-wal-direct-flush-fail", "actions": "off" }]))
        .send()
        .await;

    // Recovery must be autonomous — no safekeeper restart, no endpoint
    // bounce — and must release the stalled commit, not just fresh ones.
    step("The stalled commit completes once flushes succeed");
    match tokio::time::timeout(RECOVERY_TIMEOUT, stalled).await {
        Ok(Ok(Ok(()))) => tee_println!("stalled commit landed after the outage"),
        Ok(Ok(Err(e))) => bail!("stalled commit errored instead of completing: {e:#}"),
        Ok(Err(join)) => bail!("stalled commit task panicked: {join}"),
        Err(_) => bail!(
            "the stalled commit did not complete within {RECOVERY_TIMEOUT:?} of the \
             outage ending — a parked SyncRep waiter was lost, or \
             `DurableState::Failed` is still terminal (#770)"
        ),
    }

    step("Fresh commits land too");
    tokio::time::timeout(
        RECOVERY_TIMEOUT,
        db::batch(&conninfo, "INSERT INTO wal_failfast VALUES (2, 'recovered')"),
    )
    .await
    .map_err(|_| anyhow::anyhow!("no fresh commit within {RECOVERY_TIMEOUT:?} of recovery"))?
    .context("recovery insert")?;
    tee_println!("timeline recovered without a restart");

    step("Final integrity check");
    let rows = db::query_i64(&conninfo, "SELECT count(*) FROM wal_failfast WHERE a IN (1,2)")
        .await?;
    if rows != 2 {
        bail!("expected both the stalled and the fresh row, got {rows} of 2");
    }
    let retries = sk_log_count(c, "YB WAL consumer batch failed, retrying");
    if retries == 0 {
        bail!("no batch retries logged across a {OUTAGE:?} outage — wrong path");
    }
    db::batch(&conninfo, "DROP TABLE wal_failfast").await?;
    Ok(())
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

fn newest_log(dir: &std::path::Path) -> Option<std::path::PathBuf> {
    let mut logs: Vec<(std::time::SystemTime, std::path::PathBuf)> = std::fs::read_dir(dir)
        .ok()?
        .flatten()
        .filter(|e| e.path().extension().is_some_and(|x| x == "log"))
        .filter_map(|e| Some((e.metadata().ok()?.modified().ok()?, e.path())))
        .collect();
    logs.sort();
    logs.pop().map(|(_, p)| p)
}
