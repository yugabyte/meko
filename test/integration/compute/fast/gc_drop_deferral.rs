//! Drop-vs-materialise deferral, end to end. A failpoint holds a
//! materialise job in flight (its key pinned in `in_flight`) while
//! the table is DROPPED and the cutoff passes the drop. The drop is
//! eager-acted (its pages deleted) but its pending_gc row is DEFERRED
//! (not dequeued) while the fork is busy — observed as pages=0 with the
//! queue row still present. Once the job lands — fork quiet — a later
//! tick re-acts and reclaims everything, the job's back-dated straggler
//! image included. Guards the exact race the deferral exists for;
//! without the re-act that image would leak forever (a drop's fork is
//! otherwise never revisited).

use std::time::Duration;

use anyhow::bail;
use tokio::time::sleep;

use crate::cluster::{Cluster, step};
use crate::db;

/// How long the failpoint holds the materialise job (ms). Must
/// comfortably exceed the flush period (~5 s) + the cutoff's travel
/// time past the drop (~2 s at the suite's 1 s gc_period), so the
/// deferral is observed while the job is still held.
const HOLD_MS: u32 = 20_000;

pub async fn run(c: &Cluster) -> anyhow::Result<()> {
    let branch_pg_port = c.compute_port() + 250;
    let yb = db::yb_conn(c.yb_host(), c.yb_port(), c.db_name());

    step("Dedicated branch + endpoint");
    let b = c.branch_create_named("gc_dm", "main")?;
    c.endpoint_create_on_branch("ep_gc_dm", "gc_dm", branch_pg_port)?;
    let conn = db::compute_conn(branch_pg_port);
    let http = c.endpoint_ps_http_port("ep_gc_dm")?;

    step("Arm the materialise-hold failpoint on the branch pageserver");
    set_failpoint(http, &format!("return({HOLD_MS})")).await?;

    step("Cross the image-chain threshold on one block (held job)");
    db::batch(
        &conn,
        "CREATE TABLE gc_dm (id INT PRIMARY KEY, payload TEXT NOT NULL);
         INSERT INTO gc_dm VALUES (1, md5('seed'));",
    )
    .await?;
    // 40 HOT updates of one row = one block accumulating >
    // image_chain_limit (20) deltas; the post-flush trigger dispatches
    // a materialise job for it, which the failpoint then holds — the
    // fork stays in `in_flight` for the whole hold.
    for i in 0..40 {
        db::batch(&conn, &format!("UPDATE gc_dm SET payload = md5('{i}')")).await?;
    }
    db::batch(&conn, "CHECKPOINT").await?;
    let relnode = db::query_i64(&conn, "SELECT pg_relation_filenode('gc_dm')::bigint").await?;
    let rel_pages = format!(
        "SELECT count(*) FROM pageserver.pages WHERE branch_id = '{b}' AND relnode = {relnode}"
    );
    let rel_queue = format!(
        "SELECT count(*) FROM pageserver.pending_gc WHERE branch_id = '{b}' AND relnode = {relnode}"
    );
    // Wait past a flush period so the job is dispatched (and held).
    sleep(Duration::from_secs(8)).await;
    if db::query_i64(&yb, &rel_pages).await? == 0 {
        bail!("seeded table produced no stored pages");
    }

    step("DROP the table — the drop must be acted but DEFERRED (queued) while the job is held");
    db::batch(&conn, "DROP TABLE gc_dm").await?;
    db::batch(&conn, "CREATE TABLE gc_dm_churn (id INT PRIMARY KEY, n INT)").await?;
    // Deferral signature, no metric needed: the drop is eager-acted (its
    // pages deleted) but its pending_gc row is NOT dequeued while the
    // fork is busy — so pages reach 0 while the queue row lingers.
    // Acted-and-dequeued (idle fork) would drain the queue to 0;
    // not-yet-due would leave pages > 0.
    let mut deferred = false;
    for i in 0..20 {
        db::batch(
            &conn,
            &format!("INSERT INTO gc_dm_churn VALUES ({i}, {i}); CHECKPOINT;"),
        )
        .await?;
        sleep(Duration::from_secs(1)).await;
        if db::query_i64(&yb, &rel_pages).await? == 0 && db::query_i64(&yb, &rel_queue).await? > 0 {
            deferred = true;
            break;
        }
    }
    if !deferred {
        let _ = set_failpoint(http, "off").await;
        bail!("drop was not deferred — expected pages=0 with the queue row still present while the job was held");
    }

    step("Job lands → fork quiet → drop acts → everything reclaimed");
    let _ = set_failpoint(http, "off").await; // new jobs run normally; held ones wake on their timer
    let (mut pages, mut queue) = (i64::MAX, i64::MAX);
    for i in 0..40 {
        db::batch(
            &conn,
            &format!("INSERT INTO gc_dm_churn VALUES (1000 + {i}, {i}); CHECKPOINT;"),
        )
        .await?;
        sleep(Duration::from_secs(1)).await;
        pages = db::query_i64(&yb, &rel_pages).await?;
        queue = db::query_i64(&yb, &rel_queue).await?;
        if pages == 0 && queue == 0 {
            break;
        }
    }
    let _ = c.yb(&["branch", "drop", "--branch-id", &b]);
    c.yb_quiet(&["pageserver", "stop", "ep_gc_dm", "-m", "immediate"]);
    if pages != 0 || queue != 0 {
        bail!(
            "deferred drop did not fully reclaim: pages_left={pages} queue_left={queue}"
        );
    }
    Ok(())
}

/// Configure the `ps::materialise-redo-one` failpoint on the branch's
/// pageserver. `actions` is a fail-crate action string (`"return(20000)"`
/// holds each job that long; `"off"` clears).
async fn set_failpoint(http_port: u16, actions: &str) -> anyhow::Result<()> {
    let url = format!("http://127.0.0.1:{http_port}/v1/failpoints");
    reqwest::Client::new()
        .put(&url)
        .json(&serde_json::json!([{ "name": "ps::materialise-redo-one", "actions": actions }]))
        .send()
        .await?
        .error_for_status()?;
    Ok(())
}
