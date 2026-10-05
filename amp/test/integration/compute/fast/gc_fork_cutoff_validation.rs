//! Fork-LSN validation against the ancestor's GC cutoff, end to end
//! through the real stack (CLI → controller `RecordBranch` → YB): a
//! fork below the ancestor's advanced cutoff is rejected at branch
//! creation and leaves no rows behind; a fork at the durable head
//! succeeds. Companion: `gc_branch_pins_parent` covers that an accepted
//! child keeps the parent's fork-point history readable.
//!
//! Runs against its OWN root branch, not `main`: children other
//! scenarios fork off `main` pin its cutoff for the rest of the suite
//! run (a dropped child's `branch_state` row keeps pinning), so `main`'s
//! cutoff can never pass a fresh LSN here.

use std::time::Duration;

use anyhow::bail;
use tokio::time::sleep;

use crate::cluster::{Cluster, step};
use crate::db;

const PARENT: &str = "gc_fork_parent";
const ENDPOINT: &str = "ep_gc_fork";
/// The below-cutoff fork the controller must refuse. A const because the
/// assertion reads its name back out of `amp_registry`.
const DOOMED: &str = "gc_fork_doomed";

/// `X/Y` pg_lsn text → u64.
fn lsn_u64(text: &str) -> anyhow::Result<u64> {
    let (hi, lo) = text
        .trim()
        .split_once('/')
        .ok_or_else(|| anyhow::anyhow!("bad lsn {text}"))?;
    Ok((u64::from_str_radix(hi, 16)? << 32) | u64::from_str_radix(lo, 16)?)
}

pub async fn run(c: &Cluster) -> anyhow::Result<()> {
    let yb = db::yb_conn(c.yb_host(), c.yb_port(), c.db_name());
    let pg_port = c.endpoint_port();

    step("Create an isolated parent branch + endpoint");
    let parent_id = c.cluster_create(PARENT)?;
    c.endpoint_create_on_branch(ENDPOINT, &parent_id, pg_port)?;
    let parent = db::compute_conn(pg_port);
    let mut ready = false;
    for _ in 0..60 {
        if db::query_i64(&parent, "SELECT 1::bigint").await.is_ok() {
            ready = true;
            break;
        }
        sleep(Duration::from_secs(1)).await;
    }
    if !ready {
        bail!("parent endpoint never became ready");
    }

    step("Seed the parent, capture the doomed fork point (pre-churn LSN)");
    db::batch(
        &parent,
        "CREATE TABLE gc_fork_val (id INT PRIMARY KEY, v TEXT NOT NULL);
         INSERT INTO gc_fork_val SELECT g, md5(g::text) FROM generate_series(1, 200) g;
         CHECKPOINT;",
    )
    .await?;
    let doomed = db::query_text(&parent, "SELECT pg_current_wal_flush_lsn()::text").await?;
    let doomed_u64 = lsn_u64(&doomed)?;

    step("Advance the parent past the fork point, then let the cutoff pass it");
    // Bounded churn — pushing the head is cheap; what gates the cutoff
    // is `consistent_lsn` (pageserver ingest), which on a slow runner
    // lags churn indefinitely. So churn a fixed amount, drain ingest to
    // the tip (`flush?lsn=<tip>` blocks until consistent_lsn reaches
    // it), and then only POLL: the next 1s GC tick can publish past the
    // fork point.
    for _ in 0..8 {
        db::batch(&parent, "UPDATE gc_fork_val SET v = md5(v); CHECKPOINT;").await?;
    }
    let tip = db::query_text(&parent, "SELECT pg_current_wal_flush_lsn()::text").await?;
    let flush_url = format!(
        "http://127.0.0.1:{}/v1/branch/{parent_id}/flush?lsn={tip}",
        c.endpoint_ps_http_port(ENDPOINT)?,
    );
    let resp = reqwest::Client::new().put(&flush_url).send().await?;
    if !resp.status().is_success() {
        bail!("pageserver flush returned {}", resp.status());
    }
    let cutoff_sql = format!(
        "SELECT COALESCE(applied_gc_cutoff_lsn, 0) FROM amp_storage.branch_state \
         WHERE branch_id = '{parent_id}'"
    );
    let mut cutoff = 0i64;
    for _ in 0..30 {
        cutoff = db::query_i64(&yb, &cutoff_sql).await?;
        if cutoff as u64 > doomed_u64 {
            break;
        }
        sleep(Duration::from_secs(1)).await;
    }
    if cutoff as u64 <= doomed_u64 {
        bail!("parent's cutoff ({cutoff}) never passed the fork point {doomed} — GC not running?");
    }

    step("A fork below the cutoff is rejected and leaves no rows");
    let children_sql = format!(
        "SELECT count(*) FROM amp_storage.branch_state WHERE ancestor_branch_id = '{parent_id}'"
    );
    let children_before = db::query_i64(&yb, &children_sql).await?;
    let (created, out) = c.branch_create_at(DOOMED, &parent_id, &doomed)?;
    if let Some(id) = created {
        bail!("fork at {doomed} (below cutoff {cutoff}) was accepted as {id}:\n{out}");
    }
    if !out.contains("GC cutoff") {
        bail!("rejection does not carry the cutoff reason:\n{out}");
    }
    if db::query_i64(&yb, &children_sql).await? != children_before {
        bail!("rejected fork left a branch_state row behind");
    }
    let reg = db::query_text_opt(
        &yb,
        &format!(
            "SELECT branch_id::text FROM amp_registry.branches \
             WHERE name = '{}' AND dropped_at IS NULL",
            c.iteration_name(DOOMED)
        ),
    )
    .await?;
    if reg.is_some() {
        bail!("rejected fork left an amp_registry.branches row behind");
    }

    step("A fork at the durable head succeeds and registers its row");
    let ok_branch = c.branch_create_named("gc_fork_ok", &parent_id)?;
    let row = db::query_text_opt(
        &yb,
        &format!(
            "SELECT ancestor_lsn::text FROM amp_storage.branch_state \
             WHERE branch_id = '{ok_branch}'"
        ),
    )
    .await?;
    match row {
        Some(lsn) if lsn.parse::<i64>().is_ok_and(|l| l as u64 > doomed_u64) => {}
        other => bail!("accepted fork's branch_state row is wrong: {other:?}"),
    }

    // Teardown: stop this scenario's endpoint so later scenarios see the
    // usual single-compute cluster.
    c.yb(&["endpoint", "stop", ENDPOINT])?;
    Ok(())
}
