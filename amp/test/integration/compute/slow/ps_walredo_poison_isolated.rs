//! Issue #482: a compute that disconnects mid-getpage poisons the pooled
//! walredo child its in-flight redo was using, and the caller that draws that
//! child next must absorb the poison instead of reporting corruption.
//!
//! `1*return(N)` on the `ps::walredo-stdout-armed` failpoint stalls one redo
//! inside the armed window, `kill -9` on the backend it belongs to drops the
//! future the way a disconnecting compute does, and reads then run until a
//! caller draws the poisoned child. That draw must come back as `retried`, the
//! scan must still return every row, and the correctness counter must stay at
//! 0. The harness's gates cover the rest: `MARKERS` catches the materialiser's
//! `ABORT`, `FATAL_MARKERS` the debug-build panic in `critical_timeline!`.
//!
//! The materialiser is off for this scenario: its redos otherwise take the
//! one-shot stall ahead of the backend's getpage.
//!
//! Budget exhaustion is out of scope here. It needs more consecutive poisoned
//! draws than `ReadPage`'s 2 retries, so more poisoned children than the local
//! cluster's `walredo_pool_size` of 1, and concurrent callers on a live
//! cluster each absorb one anyway. `pageserver/tests/walredo_poison.rs` drives
//! it directly at pool sizes 2 and 3.

use std::time::Duration;

use anyhow::{Context, bail, ensure};

use crate::cluster::{Cluster, step};
use crate::tee_println;
use crate::{db, page_api};

const N_ROWS: i64 = 50_000;
const FAILPOINT: &str = "ps::walredo-stdout-armed";
/// How long the stalled redo sits in the armed window. Only the kill has to
/// land inside it, and the failpoint fires once, so this is generous.
const STALL_MS: u64 = 30_000;
/// How long to wait for a redo to reach the armed window. Well under
/// `STALL_MS` so the kill lands while the redo is still there.
const PAUSE_WAIT: Duration = Duration::from_secs(20);
/// How long to wait for a later caller to draw one of the poisoned children.
const POISON_WAIT: Duration = Duration::from_secs(60);
/// How long to wait for the restarted compute to accept queries.
const COMPUTE_READY_WAIT: Duration = Duration::from_secs(60);
/// How long to wait for the killed backend's redo future to be dropped, which
/// is what poisons the child. Near-instant in practice.
const POISON_ARM_WAIT: Duration = Duration::from_secs(15);
/// The stall can still go to another backend's getpage, in which case killing
/// ours poisons nothing. Re-arm and retry.
const ATTEMPTS: usize = 3;
/// The stress read cache, so re-reads miss the PS cache, with every
/// materialiser trigger at 0, which disables it.
const PAGE_WRITER_KEYS: &str = "\
     image_chain_limit = 0\n\
     image_chain_limit_slru = 0\n\
     image_chain_payload_bytes_limit = 0\n\
     read_cache_bytes = 65536\n";

pub async fn run(c: &Cluster) -> anyhow::Result<()> {
    let conninfo = db::compute_conn(c.compute_port());

    step("seed a churned table so reads have delta chains to replay");
    db::batch(
        &conninfo,
        "DROP TABLE IF EXISTS ps_poison; \
         CREATE TABLE ps_poison (id int, v int) WITH (autovacuum_enabled = off);",
    )
    .await?;
    db::batch(
        &conninfo,
        &format!(
            "INSERT INTO ps_poison \
             SELECT g, (g * 2654435761) % 1000000 FROM generate_series(1, {N_ROWS}) g"
        ),
    )
    .await?;
    // Every UPDATE adds a delta to the pages it touches. With the materialiser
    // off (below) nothing folds them into images, so a cold read of these
    // pages has to reach walredo.
    for v in 1..=3 {
        db::batch(&conninfo, &format!("UPDATE ps_poison SET v = v + {v}")).await?;
    }

    let toml = c
        .repo_dir()
        .join("endpoints/main/pageserver/pageserver.toml");
    let original = std::fs::read_to_string(&toml)?;

    let result = run_inner(c, &conninfo, &original).await;

    // Best-effort cleanup, in an order that cannot mask the verdict: release
    // any thread still parked at the failpoint first, then drop the table
    // while the cluster is still on this scenario's config.
    let _ = set_failpoint(c, "off").await;
    let _ = db::batch(&conninfo, "DROP TABLE IF EXISTS ps_poison").await;
    result.and(page_api::restore_ps(c, &original))
}

async fn run_inner(c: &Cluster, conninfo: &str, original: &str) -> anyhow::Result<()> {
    step("pipelined PS with the materialiser off, cold compute, so reads reach walredo");
    page_api::restart_with_pipeline_depth_and_keys(c, original, 16, PAGE_WRITER_KEYS)?;
    c.yb_quiet(&["endpoint", "stop", "main", "--mode", "immediate"]);
    c.yb(&["endpoint", "start", "main"])?;
    wait_for_compute(conninfo, COMPUTE_READY_WAIT).await?;

    let retried_before = poison_count(c, "retried").await?;
    let exhausted_before = poison_count(c, "exhausted").await?;
    let skipped_before = poison_count(c, "materializer_skipped").await?;
    tee_println!(
        "  poison counters before: retried={retried_before} exhausted={exhausted_before} \
         materializer_skipped={skipped_before}"
    );

    let mut poisoned = false;
    for attempt in 1..=ATTEMPTS {
        step(&format!(
            "attempt {attempt}/{ATTEMPTS}: stall a redo in the armed window, then kill its \
             backend"
        ));
        if poison_once(c, conninfo, retried_before).await? {
            poisoned = true;
            break;
        }
        tee_println!("  no poisoned draw observed; re-arming");
    }
    ensure!(
        poisoned,
        "no walredo child was poisoned in {ATTEMPTS} attempts — the failpoint never held the \
         compute's own redo in the armed window, so this run proves nothing"
    );

    step("the poisoned pool still serves reads");
    // `kill -9` puts the postmaster through crash recovery, which refuses
    // connections. The loop above cannot have waited it out: it exits as soon
    // as `retried` moves, which need not wait for recovery to finish.
    wait_for_compute(conninfo, COMPUTE_READY_WAIT).await?;
    // A fresh backend, so nothing is answered from the killed one's caches.
    let count = db::query_i64(conninfo, "SELECT count(*) FROM ps_poison").await?;
    ensure!(
        count == N_ROWS,
        "post-poison scan returned {count} rows, want {N_ROWS}"
    );

    step("the poison was absorbed, not escalated");
    let exhausted = poison_count(c, "exhausted").await?;
    let skipped = poison_count(c, "materializer_skipped").await?;
    ensure!(
        exhausted == exhausted_before,
        "poison budget exhausted ({exhausted_before} -> {exhausted}) — one disconnect poisons \
         at most `walredo_pool_size` children, well inside a budget of 2x that"
    );
    ensure!(
        skipped == skipped_before,
        "the materialiser skipped {} job(s) — one disconnect should not exhaust its budget",
        skipped - skipped_before
    );
    // A poisoned draw that runs out of retries must not reach
    // `critical_timeline!` or the materialiser's `fatal_abort`. `final_verdict`
    // fails the run if this counter has any series at all.
    let correctness = c
        .metric_sum(c.ps_http_port(), "pageserver_yb_correctness_errors_total")
        .await?;
    ensure!(
        correctness == 0.0,
        "a poisoned walredo draw raised the correctness counter to {correctness} — a client \
         disconnect is not corruption"
    );
    tee_println!("  ok — read served, poison absorbed as `retried`, correctness counter at 0");
    Ok(())
}

/// Does this log line name backend `pid` as the stalled caller? The span field
/// is the whole pid, so a plain `contains` would let backend 12345 answer for
/// 1234 — the scenario would then kill a backend that is not in the armed
/// window and poison nothing.
fn names_backend(line: &str, pid: i32) -> bool {
    let needle = format!("application_name={pid}");
    line.match_indices(&needle).any(|(at, _)| {
        line[at + needle.len()..]
            .chars()
            .next()
            .is_none_or(|c| !c.is_ascii_digit())
    })
}

/// Wait for the redo sleeping in the armed window to finish.
///
/// Turning the failpoint off cannot wake a redo already sleeping, and with a
/// pool of 1 that child's stdout mutex is held for the rest of `STALL_MS`, so
/// every branch that gives up on a stall has to wait it out or the next
/// attempt queues behind it and burns its budget there.
async fn wait_out_stall(c: &Cluster, since: usize) -> anyhow::Result<()> {
    let done = format!("failpoint {FAILPOINT:?}: sleep done");
    let budget = Duration::from_millis(STALL_MS) + Duration::from_secs(5);
    if wait_for_log(c, since, &done, budget).await.is_none() {
        bail!(
            "a redo stalled at {FAILPOINT} and never logged `sleep done` within {}s; \
             the pool is wedged and later attempts would queue behind it",
            budget.as_secs()
        );
    }
    Ok(())
}

/// One poison attempt. Returns whether a later caller actually drew the
/// poisoned child, i.e. whether `retried` moved past `retried_before`.
async fn poison_once(c: &Cluster, conninfo: &str, retried_before: f64) -> anyhow::Result<bool> {
    // Opened before arming, so its startup catalog reads cannot take the
    // stall.
    let client = db::connect(conninfo).await?;
    let pid: i32 = client.query_one("SELECT pg_backend_pid()", &[]).await?.get(0);
    tee_println!("  throwaway backend pid = {pid}");

    let log_before = ps_log_len(c);
    set_failpoint(c, &format!("1*return({STALL_MS})")).await?;

    // A cold sequential scan at high IO concurrency: many getpages in flight,
    // and with image_chain_limit = 2 the pages behind them need a redo.
    let scan = tokio::spawn(async move {
        // Errors when we kill the backend, which is the point.
        let _ = client
            .batch_execute("SET effective_io_concurrency = 32; SELECT count(*) FROM ps_poison;")
            .await;
    });

    // `sleep_millis_async` logs this only when the failpoint is armed and hit,
    // so it means a redo is in the armed window right now.
    let sleeping = format!("failpoint {FAILPOINT:?}: sleeping");
    let Some(hit) = wait_for_log(c, log_before, &sleeping, PAUSE_WAIT).await else {
        tee_println!("  no redo reached the armed window; the scan never needed walredo");
        scan.abort();
        set_failpoint(c, "off").await?;
        return Ok(false);
    };

    // The page_service span carries the backend's pid as `application_name`; a
    // materialiser redo has no such span. On someone else's stall our getpage
    // is queued on that child's stdout mutex, not in the armed window, so
    // killing the backend would poison nothing.
    if !names_backend(&hit, pid) {
        tee_println!("  the stall went to another caller, not backend {pid}; waiting it out");
        scan.abort();
        set_failpoint(c, "off").await?;
        wait_out_stall(c, log_before).await?;
        // Aborting the task drops the client, not the backend. Its scan
        // resumes after the stall and would compete for the next attempt's.
        db::batch(
            conninfo,
            &format!("SELECT pg_terminate_backend({pid}, 30000)"),
        )
        .await?;
        return Ok(false);
    }

    step("kill the backend (immediate disconnect; the PS aborts the stalled redo)");
    let kill_mark = ps_log_len(c);
    let killed = std::process::Command::new("kill")
        .args(["-9", &pid.to_string()])
        .status()?
        .success();
    ensure!(killed, "failed to kill backend pid {pid}");
    scan.abort();

    // `utils::poison`'s guard warns `poisoning stdout` from `Drop` when it is
    // dropped still armed, so this line is the event the scenario exists to
    // produce. A disconnect that poisoned nothing then says so here rather
    // than a minute later as "no poisoned draw".
    if wait_for_log(c, kill_mark, "poisoning stdout", POISON_ARM_WAIT)
        .await
        .is_none()
    {
        tee_println!(
            "  killing backend {pid} did not poison a child within {}s",
            POISON_ARM_WAIT.as_secs()
        );
        set_failpoint(c, "off").await?;
        // Reaching here means a redo is still sleeping in the armed window, so
        // wait it out for the same reason the foreign-stall branch above does:
        // turning the failpoint off cannot wake it, and the next attempt would
        // queue behind the stdout mutex it still holds.
        wait_out_stall(c, kill_mark).await?;
        return Ok(false);
    }
    set_failpoint(c, "off").await?;

    step("drive reads until a caller draws the poisoned child");
    let deadline = std::time::Instant::now() + POISON_WAIT;
    while std::time::Instant::now() < deadline {
        // Each query is a fresh backend, so it re-reads through the PS.
        let _ = db::query_i64(conninfo, "SELECT count(*) FROM ps_poison").await;
        let retried = poison_count(c, "retried").await?;
        if retried > retried_before {
            tee_println!(
                "  {} poisoned draw(s) retried onto another child",
                retried - retried_before
            );
            return Ok(true);
        }
        tokio::time::sleep(Duration::from_millis(500)).await;
    }
    Ok(false)
}

/// `pageserver_wal_redo_poison_total` for one outcome. The series always
/// exists — `preinitialize_metrics` seeds every outcome at 0.
async fn poison_count(c: &Cluster, outcome: &str) -> anyhow::Result<f64> {
    c.metric_sum(
        c.ps_http_port(),
        &format!("pageserver_wal_redo_poison_total{{outcome=\"{outcome}\"}}"),
    )
    .await
    .with_context(|| format!("read poison counter for outcome={outcome}"))
}

/// Poll the freshly started compute until it answers a trivial query.
/// `endpoint start` returns before postgres is accepting connections.
async fn wait_for_compute(conninfo: &str, budget: Duration) -> anyhow::Result<()> {
    let deadline = std::time::Instant::now() + budget;
    loop {
        // `::int8` because `query_i64` decodes int8 and a bare 1 is int4.
        let err = match db::query_i64(conninfo, "SELECT 1::int8").await {
            Ok(_) => return Ok(()),
            Err(e) => e,
        };
        if std::time::Instant::now() >= deadline {
            bail!(
                "compute did not accept queries within {}s: {err:#}",
                budget.as_secs(),
            );
        }
        tokio::time::sleep(Duration::from_millis(250)).await;
    }
}

fn ps_log_len(c: &Cluster) -> usize {
    std::fs::read_to_string(c.ps_log())
        .map(|s| s.lines().count())
        .unwrap_or(0)
}

/// Poll the pageserver log for `needle` among the lines added since `before`,
/// returning the first line that matches.
async fn wait_for_log(
    c: &Cluster,
    before: usize,
    needle: &str,
    budget: Duration,
) -> Option<String> {
    let deadline = std::time::Instant::now() + budget;
    loop {
        if let Ok(content) = std::fs::read_to_string(c.ps_log())
            && let Some(hit) = content.lines().skip(before).find(|l| l.contains(needle))
        {
            return Some(hit.to_owned());
        }
        if std::time::Instant::now() >= deadline {
            return None;
        }
        tokio::time::sleep(Duration::from_millis(200)).await;
    }
}

async fn set_failpoint(c: &Cluster, actions: &str) -> anyhow::Result<()> {
    let url = format!("http://127.0.0.1:{}/v1/failpoints", c.ps_http_port());
    let body = serde_json::json!([{ "name": FAILPOINT, "actions": actions }]);
    let resp = c.daemon_client(c.ps_http_port())?.put(&url).json(&body).send().await?;
    ensure!(
        resp.status().is_success(),
        "PUT {FAILPOINT}={actions} -> {}",
        resp.status()
    );
    Ok(())
}
