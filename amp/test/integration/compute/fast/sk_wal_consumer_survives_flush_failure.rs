//! Reproducer / regression guard for #770: a single failed WAL consumer batch
//! used to cost the endpoint all write availability, permanently.
//!
//! `consumer_main` returned on its first `flush_batch` error, publishing the
//! rendered error onto the durability `watch` channel. Nothing respawns that
//! task, and `watch` retains its last value, so `DurableState::Failed(msg)` was
//! an absorbing state: every later WAL push read that same captured `String`
//! and failed with it — `upsert_writes` on the closed channel,
//! `wait_for_durable` on the retained value — without ever calling YugabyteDB
//! again. The walproposer reconnected once per second forever, each attempt
//! dying on a replayed error in ~1.5 ms, until the safekeeper process was
//! restarted.
//!
//! On the reported endpoint that produced 8340 occurrences of one byte-identical
//! error string over 70 minutes with `flush_lsn` frozen, while all three
//! tservers logged nothing at all — no request ever reached the server.
//!
//! The failpoint fires **once** (`1*return`), which is the whole point: the
//! trigger is incidental and need not persist. Post-fix the consumer retries
//! the batch, the retry succeeds immediately, and the only visible cost is one
//! warning line. Pre-fix the endpoint never accepts another write.

use std::time::Duration;

use anyhow::{Context, bail};

use crate::cluster::{Cluster, step};
use crate::db;
use crate::tee_println;

/// A commit must land within this budget once the (single) injected failure
/// has been retried. Generous — post-fix the first retry is 100 ms away — so a
/// failure here means the timeline stopped accepting writes, not that it was
/// briefly slow.
const PROBE_TIMEOUT: Duration = Duration::from_secs(45);

pub async fn run(c: &Cluster) -> anyhow::Result<()> {
    let conninfo = db::compute_conn(c.compute_port());
    let http = reqwest::Client::new();
    let failpoints_url = format!("http://127.0.0.1:{}/v1/failpoints", c.sk_http_port());

    step("Seed load table and confirm commits flow");
    tokio::time::timeout(
        PROBE_TIMEOUT,
        db::batch(
            &conninfo,
            "DROP TABLE IF EXISTS wal_flush_fail; \
             CREATE TABLE wal_flush_fail (a BIGINT, b TEXT);",
        ),
    )
    .await
    .context("seed timed out")??;

    // `1*return` fires exactly once: the next consumer batch fails, every batch
    // after it is untouched.
    step("Arm sk-wal-direct-flush-fail for a SINGLE batch failure");
    let resp = http
        .put(&failpoints_url)
        .json(&serde_json::json!([{
            "name": "sk-wal-direct-flush-fail",
            "actions": "1*return",
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

    // This write's batch is the one that fails. Post-fix the consumer retries
    // and it lands; pre-fix the consumer exits here and never comes back.
    step("Write through the injected failure");
    tokio::time::timeout(
        PROBE_TIMEOUT,
        db::batch(
            &conninfo,
            "INSERT INTO wal_flush_fail SELECT g, repeat('x', 200) \
             FROM generate_series(1, 500) g",
        ),
    )
    .await
    .map_err(|_| wedge_evidence(c, "write through the injected failure"))?
    .context("insert through injected failure")?;

    if !sk_log_contains(c, "sk-wal-direct-flush-fail") {
        bail!(
            "the failpoint never fired — this test proves nothing unless the batch \
             actually failed. Check that the SK was built with --features testing."
        );
    }
    tee_println!("injected batch failure fired");

    // The regression: the consumer must still be alive. Pre-fix it had exited
    // and these commits would never land.
    step("Commits keep landing after the failure");
    for i in 0..3 {
        tokio::time::timeout(
            PROBE_TIMEOUT,
            db::batch(&conninfo, "INSERT INTO wal_flush_fail VALUES (-1, 'probe')"),
        )
        .await
        .map_err(|_| wedge_evidence(c, &format!("probe {i}")))?
        .context("probe insert")?;
    }

    step("Verify the batch was retried rather than abandoned");
    if !sk_log_contains(c, "YB WAL consumer batch failed, retrying") {
        bail!(
            "no consumer retry was logged — the batch failure did not go through the \
             retry path, so this run does not exercise the #770 fix"
        );
    }
    if sk_log_contains(c, "the producer is gone") {
        bail!("consumer exited on a batch failure while the producer was still live (#770)");
    }

    step("Final integrity check");
    let rows = db::query_i64(&conninfo, "SELECT count(*) FROM wal_flush_fail WHERE a > 0").await?;
    if rows != 500 {
        bail!("lost rows through the injected failure: got {rows} of 500");
    }
    let probes = db::query_i64(&conninfo, "SELECT count(*) FROM wal_flush_fail WHERE a < 0").await?;
    if probes != 3 {
        bail!("lost probe rows: got {probes} of 3");
    }
    db::batch(&conninfo, "DROP TABLE wal_flush_fail").await?;
    Ok(())
}

/// A write never landed: report it as the permanent-wedge signature, with the
/// counts that distinguish #770 (a replayed captured error, no YB traffic) from
/// an ordinary slow flush.
fn wedge_evidence(c: &Cluster, phase: &str) -> anyhow::Error {
    let replays = sk_log_count(c, "YB WAL consumer task failed");
    let retries = sk_log_count(c, "YB WAL consumer batch failed, retrying");
    anyhow::anyhow!(
        "WAL PUSH WEDGED (#770): no commit landed within {PROBE_TIMEOUT:?} after ONE \
         injected batch failure ({phase}). safekeeper log: {replays} replays of a \
         captured consumer error, {retries} batch retries. A single transient flush \
         failure must not end the timeline — the consumer has to retry, not exit."
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
