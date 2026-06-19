//! Co-located pageserver/safekeeper topology: a branch gets a NEW PS+SK
//! pair provisioned at `endpoint create`, and the branch endpoint serves
//! pre-branch-point data via lineage reads (nothing copied) plus
//! post-branch writes isolated from the ancestor in both directions —
//! and survives a pageserver restart.

use anyhow::bail;

use crate::cluster::{Cluster, step};
use crate::db;

pub async fn run(c: &Cluster) -> anyhow::Result<()> {
    // Branch endpoint sits in the compute lane just above `main`, so the
    // whole cluster stays inside its picked port block.
    let branch_pg_port = c.compute_port() + 100;
    let main = db::compute_conn(c.compute_port());
    let branch = db::compute_conn(branch_pg_port);
    let tid = c.tid();

    step("Seed pre-branch data on main");
    // The branch compute reads these seed transactions' CLOG (SLRU)
    // state straight from the pageserver, so this also covers
    // early-window CLOG-delta persistence.
    db::batch(
        &main,
        "DROP TABLE IF EXISTS branch_seed;
         CREATE TABLE branch_seed (id INT PRIMARY KEY, val TEXT NOT NULL);
         INSERT INTO branch_seed SELECT g, 'pre-branch-' || g FROM generate_series(1, 1000) g;
         CHECKPOINT;",
    )
    .await?;
    let pre_count = db::query_i64(&main, "SELECT count(*) FROM branch_seed").await?;
    if pre_count != 1000 {
        bail!("seed count {pre_count} != 1000");
    }

    step("Create branch b1 (records the branch point; PS/SK come up at endpoint create)");
    c.yb(&["timeline", "branch", "--tenant-id", tid, "--branch-name", "b1"])?;

    step("Write post-branch data on main (must NOT be visible on b1)");
    db::batch(
        &main,
        "INSERT INTO branch_seed SELECT g, 'post-branch-main-' || g FROM generate_series(1001, 1100) g",
    )
    .await?;

    step("Create + start endpoint on b1 (provisions pageserver 2 + safekeeper 2)");
    c.yb(&[
        "endpoint", "create", "ep_b1", "--tenant-id", tid, "--branch-name", "b1",
        "--pg-version", "17", "--pg-port", &branch_pg_port.to_string(),
    ])?;
    c.yb(&["endpoint", "start", "ep_b1"])?;

    step("Branch sees pre-branch data via lineage reads");
    let b1_pre = db::query_i64(
        &branch,
        "SELECT count(*) FROM branch_seed WHERE val LIKE 'pre-branch-%'",
    )
    .await?;
    if b1_pre != 1000 {
        bail!("branch sees {b1_pre} pre-branch rows, want 1000");
    }

    step("Branch does NOT see main's post-branch writes");
    let b1_post_main = db::query_i64(
        &branch,
        "SELECT count(*) FROM branch_seed WHERE val LIKE 'post-branch-main-%'",
    )
    .await?;
    if b1_post_main != 0 {
        bail!("branch sees {b1_post_main} of main's post-branch rows, want 0");
    }

    step("Branch writes are isolated from main");
    db::batch(
        &branch,
        "INSERT INTO branch_seed SELECT g, 'post-branch-b1-' || g FROM generate_series(2001, 2100) g",
    )
    .await?;
    let b1_own = db::query_i64(
        &branch,
        "SELECT count(*) FROM branch_seed WHERE val LIKE 'post-branch-b1-%'",
    )
    .await?;
    if b1_own != 100 {
        bail!("branch sees {b1_own} of its own rows, want 100");
    }
    let main_b1 = db::query_i64(
        &main,
        "SELECT count(*) FROM branch_seed WHERE val LIKE 'post-branch-b1-%'",
    )
    .await?;
    if main_b1 != 0 {
        bail!("main sees {main_b1} branch rows, want 0");
    }
    let main_total = db::query_i64(&main, "SELECT count(*) FROM branch_seed").await?;
    if main_total != 1100 {
        bail!("main total {main_total}, want 1100");
    }

    step("Branch survives a pageserver restart (open-existing + lineage rebuild)");
    c.yb(&["pageserver", "stop", "--id", "2"])?;
    c.yb(&["pageserver", "start", "--id", "2"])?;
    let after_restart = db::query_i64(&branch, "SELECT count(*) FROM branch_seed").await?;
    if after_restart != 1100 {
        bail!("branch sees {after_restart} rows after PS restart, want 1100 (1000 pre + 100 own)");
    }

    step("Cleanup: stop branch endpoint (branch PS/SK keep running; suite teardown stops them)");
    c.yb_quiet(&["endpoint", "stop", "ep_b1", "--mode", "immediate"]);

    Ok(())
}
