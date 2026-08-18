//! `branch drop`: tear down a branch and all of its endpoints. Branches off
//! `main`, brings up an endpoint on the branch, then drops the branch and
//! verifies the durable cleanup: the endpoint's compute is gone (local dir
//! removed), its record is deleted from the controller, and the
//! branch is marked dropped (`dropped_at` set) so it no longer counts as a
//! live branch and isn't brought back on restart. The underlying branch data is
//! reclaimed asynchronously, by the controller's branch-drop reaper a retention
//! window later — that is `gc_branch_drop_reclaims`, not this test, which asserts
//! only what the drop itself does.

use anyhow::bail;

use crate::cluster::{Cluster, step};
use crate::db;

const ENDPOINT_ID: &str = "ep_drop";
const BRANCH_NAME: &str = "drop_me";

pub async fn run(c: &Cluster) -> anyhow::Result<()> {
    // Branch endpoint sits in the compute lane just above `main`.
    let branch_pg_port = c.compute_port() + 101;
    let main = db::compute_conn(c.compute_port());
    let branch_conn = db::compute_conn(branch_pg_port);
    let yb = db::yb_conn(c.yb_host(), c.yb_port(), c.db_name());

    step("Seed data on main");
    db::batch(
        &main,
        "DROP TABLE IF EXISTS drop_seed;
         CREATE TABLE drop_seed (id INT PRIMARY KEY);
         INSERT INTO drop_seed SELECT g FROM generate_series(1, 100) g;
         CHECKPOINT;",
    )
    .await?;

    // The controller assigns branch ids, so scrape the one it created rather than
    // pinning a literal.
    step("Create a branch off main");
    let branch_id = c.branch_create_named(BRANCH_NAME, c.branch())?;

    step("Create + start an endpoint on the branch");
    c.yb(&[
        "endpoint", "create", ENDPOINT_ID, "--branch-id", &branch_id,
        "--pg-version", "18", "--pg-port", &branch_pg_port.to_string(),
    ])?;
    c.yb(&["endpoint", "start", ENDPOINT_ID])?;

    step("Branch endpoint serves the seeded data via lineage");
    let count = db::query_i64(&branch_conn, "SELECT count(*) FROM drop_seed").await?;
    if count != 100 {
        bail!("branch sees {count} rows, want 100");
    }

    step("Pre-drop: the branch is a live branch with an endpoint record");
    let live = live_branch_rows(&yb, &branch_id).await?;
    if live != 1 {
        bail!("pre-drop live branch rows = {live}, want 1");
    }
    let eps = endpoint_rows(&yb, &branch_id).await?;
    if eps < 1 {
        bail!("pre-drop endpoint rows = {eps}, want >= 1");
    }

    step("Drop the branch and all its endpoints");
    c.yb(&["branch", "drop", "--branch-id", &branch_id])?;

    step("Post-drop: the endpoint's local dir is gone");
    let ep_dir = c.repo_dir().join("endpoints").join(ENDPOINT_ID);
    if ep_dir.exists() {
        bail!("endpoint dir {} still exists after drop", ep_dir.display());
    }

    step("Post-drop: the endpoint record is deleted from the controller");
    let eps = endpoint_rows(&yb, &branch_id).await?;
    if eps != 0 {
        bail!("post-drop endpoint rows = {eps}, want 0");
    }

    step("Post-drop: the branch is tombstoned, not live");
    let live = live_branch_rows(&yb, &branch_id).await?;
    if live != 0 {
        bail!("post-drop live branch rows = {live}, want 0 (branch should be tombstoned)");
    }
    let dropped = db::query_i64(
        &yb,
        &format!(
            "SELECT count(*) FROM amp_registry.branches \
             WHERE branch_id = '{branch_id}' \
               AND dropped_at IS NOT NULL"
        ),
    )
    .await?;
    if dropped != 1 {
        bail!("post-drop tombstoned branch rows = {dropped}, want 1");
    }

    step("Post-drop: main is unaffected");
    let main_count = db::query_i64(&main, "SELECT count(*) FROM drop_seed").await?;
    if main_count != 100 {
        bail!("main sees {main_count} rows after drop, want 100");
    }

    Ok(())
}

/// Count of *live* (not-tombstoned) `amp_registry.branches` rows for our branch.
///
/// One branch table serves both the product API and the storage-layer control
/// surface, and a BranchId is the uuid it is keyed by — one spelling on both.
async fn live_branch_rows(yb: &str, branch_id: &str) -> anyhow::Result<i64> {
    db::query_i64(
        yb,
        &format!(
            "SELECT count(*) FROM amp_registry.branches \
             WHERE branch_id = '{branch_id}' \
               AND dropped_at IS NULL"
        ),
    )
    .await
}

/// Count of endpoint records pinned to our branch.
async fn endpoint_rows(yb: &str, branch_id: &str) -> anyhow::Result<i64> {
    db::query_i64(
        yb,
        &format!(
            "SELECT count(*) FROM amp_registry.endpoints \
             WHERE branch_id = '{branch_id}'"
        ),
    )
    .await
}
