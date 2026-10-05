//! Branch-drop GC end to end: dropping a branch eventually reclaims its data.
//!
//! The controller's reaper (`controller/internal/branchgc`) owns this flow, not
//! the pageserver — a dropped branch's pageserver is terminated with it, so
//! nothing would be left to drain a queued candidate. The reaper waits out
//! `--branch-gc-retention`, deletes the branch's rows from every `amp_storage`
//! table, and stamps `gc_completed_at` on the branch row (which is KEPT: a branch
//! id is never reused).
//!
//! The suite runs the controller with `AMP_BRANCH_GC_RETENTION=0s` and a seconds-
//! scale tick (see `cluster.rs`), so what would be a day-long wait in production
//! happens inside a test.
//!
//! What this pins that the Go tests cannot: the rows being reclaimed are the ones
//! a REAL pageserver and safekeeper wrote, in every table they use, and `main`
//! — whose rows share those same six-tablet `pages` / `wals` splits — keeps
//! serving throughout.

use std::time::Duration;

use anyhow::bail;
use tokio::time::sleep;

use crate::cluster::{Cluster, step};
use crate::db;
use crate::tee_println;

const ENDPOINT_ID: &str = "ep_gc_reap";
const BRANCH_NAME: &str = "reap_me";

/// How long to wait for the reap. Generous against a slow CI box: the reaper
/// ticks every few seconds, and one tick's row budget covers a test-sized branch
/// many times over.
const REAP_TIMEOUT: Duration = Duration::from_secs(90);

/// The `amp_storage` tables a branch has rows in — the reaper's whole delete plan.
/// Every one of them is branch-keyed by the branch id.
const TABLES: [&str; 7] = [
    "branch_state",
    "pages",
    "wals",
    "pending_gc",
    "blobs",
    "slru_pages",
    "meta",
];

/// Rows a branch owns in one `amp_storage` table. Used only on a failure path and
/// in the pre-drop wait — the polling loops below ask for the total in one query.
async fn rows_in(yb: &str, table: &str, branch_id: &str) -> anyhow::Result<i64> {
    db::query_i64(
        yb,
        &format!("SELECT count(*) FROM amp_storage.{table} WHERE branch_id = '{branch_id}'"),
    )
    .await
}

/// A `count(*)` subquery per table in the plan, as SQL text.
fn per_table_counts(branch_id: &str) -> Vec<String> {
    TABLES
        .iter()
        .map(|t| format!("(SELECT count(*) FROM amp_storage.{t} WHERE branch_id = '{branch_id}')"))
        .collect()
}

/// Rows the branch owns across every table in the plan, plus whether the reaper has
/// marked it reclaimed — both in ONE query, because this is polled every couple of
/// seconds and `db::query_i64` opens a fresh connection per call (seven tables
/// polled separately is seven connections and seven cold catalog caches per tick).
async fn reap_progress(yb: &str, branch_id: &str) -> anyhow::Result<(i64, i64)> {
    let sql = format!(
        "SELECT {sum} AS rows_left, \
                (SELECT count(*) FROM amp_registry.branches \
                  WHERE branch_id = '{branch_id}' \
                    AND dropped_at IS NOT NULL AND gc_completed_at IS NOT NULL) AS marked",
        sum = per_table_counts(branch_id).join(" + "),
    );
    let row = db::connect(yb).await?.query_one(&sql, &[]).await?;
    Ok((row.get::<_, i64>(0), row.get::<_, i64>(1)))
}

/// Dump the per-table breakdown into the run log — the failure path's detail.
async fn dump_tables(yb: &str, branch_id: &str, note: &str) -> anyhow::Result<()> {
    for table in TABLES {
        let n = rows_in(yb, table, branch_id).await?;
        tee_println!("  amp_storage.{table}: {n} rows {note}");
    }
    Ok(())
}

pub async fn run(c: &Cluster) -> anyhow::Result<()> {
    let branch_pg_port = c.endpoint_port();
    let main = db::compute_conn(c.compute_port());
    let branch_conn = db::compute_conn(branch_pg_port);
    let yb = db::yb_conn(c.yb_host(), c.yb_port(), c.db_name());

    step("Seed main, then branch off it");
    db::batch(
        &main,
        "DROP TABLE IF EXISTS reap_seed;
         CREATE TABLE reap_seed (id INT PRIMARY KEY, payload TEXT NOT NULL);
         INSERT INTO reap_seed SELECT g, 'main-' || md5(g::text) FROM generate_series(1, 200) g;
         CHECKPOINT;",
    )
    .await?;
    let want_main = db::query_text(
        &main,
        "SELECT md5(string_agg(payload, ',' ORDER BY id)) FROM reap_seed",
    )
    .await?;
    let branch_id = c.branch_create_named(BRANCH_NAME, c.branch())?;

    step("Write on the branch so it owns rows of its own");
    c.endpoint_create_on_branch(ENDPOINT_ID, &branch_id, branch_pg_port)?;
    db::batch(
        &branch_conn,
        "CREATE TABLE reap_branch (id INT PRIMARY KEY, payload TEXT NOT NULL);
         INSERT INTO reap_branch SELECT g, 'branch-' || md5(g::text) FROM generate_series(1, 2000) g;
         UPDATE reap_branch SET payload = 'churn-' || payload;
         CHECKPOINT;",
    )
    .await?;

    // The branch's pages/meta land as its pageserver ingests, and its WAL as the
    // safekeeper mirrors — both asynchronous, so wait for them rather than assuming.
    step("Pre-drop: the branch owns rows the reaper will have to reclaim");
    // least(...) > 0 means every one of them has landed. `branch_state` is keyed on
    // branch_id alone, so its count is 0 or 1.
    let ready = format!(
        "SELECT least({})",
        ["branch_state", "pages", "wals", "meta"]
            .iter()
            .map(|t| format!(
                "(SELECT count(*) FROM amp_storage.{t} WHERE branch_id = '{branch_id}')"
            ))
            .collect::<Vec<_>>()
            .join(", "),
    );
    let mut before = 0;
    for _ in 0..30 {
        if db::query_i64(&yb, &ready).await? > 0 {
            before = reap_progress(&yb, &branch_id).await?.0;
            break;
        }
        sleep(Duration::from_secs(1)).await;
    }
    if before == 0 {
        dump_tables(&yb, &branch_id, "").await?;
        bail!("the branch never got rows in pages/wals/meta/branch_state; nothing to reclaim");
    }
    tee_println!(
        "  branch {branch_id} owns {before} rows across {} tables",
        TABLES.len()
    );

    step("Drop the branch (stops its endpoint, tombstones the row)");
    c.yb(&["branch", "drop", "--branch-id", &branch_id])?;

    step("The controller's reaper deletes every one of those rows and marks the branch");
    // One loop for both halves: the rows going to zero and `gc_completed_at` being
    // stamped are one reap, and the marker is written after the last delete.
    let mut left = before;
    let mut marked = 0;
    let deadline = std::time::Instant::now() + REAP_TIMEOUT;
    while std::time::Instant::now() < deadline {
        (left, marked) = reap_progress(&yb, &branch_id).await?;
        if left == 0 && marked == 1 {
            break;
        }
        sleep(Duration::from_secs(2)).await;
    }
    if left != 0 {
        dump_tables(&yb, &branch_id, "left").await?;
        bail!("{left} of the dropped branch's {before} rows survive after {REAP_TIMEOUT:?}");
    }
    if marked != 1 {
        bail!(
            "the branch's rows are gone but its row is missing or unmarked (gc_completed_at rows = {marked})"
        );
    }

    step("main is untouched: same data, and it still owns its own rows");
    let got_main = db::query_text(
        &main,
        "SELECT md5(string_agg(payload, ',' ORDER BY id)) FROM reap_seed",
    )
    .await?;
    if got_main != want_main {
        bail!("main's data changed across a branch reap: {got_main} != {want_main}");
    }
    let main_pages = rows_in(&yb, "pages", c.branch()).await?;
    let main_state = rows_in(&yb, "branch_state", c.branch()).await?;
    if main_pages == 0 || main_state != 1 {
        bail!("the reap took main's rows too (pages = {main_pages}, branch_state = {main_state})");
    }

    Ok(())
}
