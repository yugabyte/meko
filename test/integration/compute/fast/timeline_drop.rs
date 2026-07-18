//! `branch drop`: tear down a branch and all of its endpoints. Branches off
//! `main`, brings up an endpoint on the branch, then drops the branch and
//! verifies the durable cleanup: the endpoint's compute is gone (local dir
//! removed), its record is deleted from the controller, and the
//! branch is marked dropped (`deleted_at` set) so it no longer counts as a
//! live branch and isn't brought back on restart. The underlying branch
//! data is intentionally left for a later GC pass (see #36).

use anyhow::bail;

use crate::cluster::{Cluster, step};
use crate::db;

const ENDPOINT_ID: &str = "ep_drop";
const BRANCH_NAME: &str = "drop_me";
// Explicit, known branch id so we can address the branch in `branch drop`
// and query its registry row directly.
const BRANCH_ID: &str = "d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0";

pub async fn run(c: &Cluster) -> anyhow::Result<()> {
    // Branch endpoint sits in the compute lane just above `main`.
    let branch_pg_port = c.compute_port() + 101;
    let main = db::compute_conn(c.compute_port());
    let branch = db::compute_conn(branch_pg_port);
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

    step("Create branch with an explicit branch id");
    c.yb(&[
        "branch", "create", "--branch-name", BRANCH_NAME, "--ancestor-branch-name", "main",
        "--branch-id", BRANCH_ID,
    ])?;

    step("Create + start an endpoint on the branch");
    c.yb(&[
        "endpoint", "create", ENDPOINT_ID, "--branch-id", BRANCH_ID, "--branch-name", BRANCH_NAME,
        "--pg-version", "18", "--pg-port", &branch_pg_port.to_string(),
    ])?;
    c.yb(&["endpoint", "start", ENDPOINT_ID])?;

    step("Branch endpoint serves the seeded data via lineage");
    let count = db::query_i64(&branch, "SELECT count(*) FROM drop_seed").await?;
    if count != 100 {
        bail!("branch sees {count} rows, want 100");
    }

    step("Pre-drop: the branch is a live branch with an endpoint record");
    let live = live_branch_rows(&yb).await?;
    if live != 1 {
        bail!("pre-drop live branch rows = {live}, want 1");
    }
    let eps = endpoint_rows(&yb).await?;
    if eps < 1 {
        bail!("pre-drop endpoint rows = {eps}, want >= 1");
    }

    step("Drop the branch and all its endpoints");
    c.yb(&["branch", "drop", "--branch-id", BRANCH_ID])?;

    step("Post-drop: the endpoint's local dir is gone");
    let ep_dir = c.repo_dir().join("endpoints").join(ENDPOINT_ID);
    if ep_dir.exists() {
        bail!("endpoint dir {} still exists after drop", ep_dir.display());
    }

    step("Post-drop: the endpoint record is deleted from the controller");
    let eps = endpoint_rows(&yb).await?;
    if eps != 0 {
        bail!("post-drop endpoint rows = {eps}, want 0");
    }

    step("Post-drop: the branch is marked dropped, not live");
    let live = live_branch_rows(&yb).await?;
    if live != 0 {
        bail!("post-drop live branch rows = {live}, want 0 (branch should be marked dropped)");
    }
    let dropped = db::query_i64(
        &yb,
        &format!(
            "SELECT count(*) FROM registry.timelines \
             WHERE branch_id = '{BRANCH_ID}' AND deleted_at IS NOT NULL"
        ),
    )
    .await?;
    if dropped != 1 {
        bail!("post-drop dropped branch rows = {dropped}, want 1");
    }

    step("Post-drop: main is unaffected");
    let main_count = db::query_i64(&main, "SELECT count(*) FROM drop_seed").await?;
    if main_count != 100 {
        bail!("main sees {main_count} rows after drop, want 100");
    }

    Ok(())
}

/// Count of *live* (not-dropped) `timelines` rows for our branch.
async fn live_branch_rows(yb: &str) -> anyhow::Result<i64> {
    db::query_i64(
        yb,
        &format!(
            "SELECT count(*) FROM registry.timelines \
             WHERE branch_id = '{BRANCH_ID}' AND deleted_at IS NULL"
        ),
    )
    .await
}

/// Count of endpoint records pinned to our branch.
async fn endpoint_rows(yb: &str) -> anyhow::Result<i64> {
    db::query_i64(
        yb,
        &format!(
            "SELECT count(*) FROM registry.endpoints WHERE branch_id = '{BRANCH_ID}'"
        ),
    )
    .await
}
