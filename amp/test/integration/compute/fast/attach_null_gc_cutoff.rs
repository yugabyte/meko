//! #531: a pageserver re-attaching to a branch whose `branch_state` row
//! has `consistent_lsn` set but `applied_gc_cutoff_lsn` still NULL used to
//! die during attach with
//!
//! ```text
//! Error: Failed to start pageserver
//! Caused by:
//!     0: error deserializing column 2: a Postgres value was `NULL`
//! ```
//!
//! and then crash-loop forever, because `get_branch_state`'s presence
//! predicate is only `consistent_lsn IS NOT NULL` while
//! `row_to_branch_state` read column 2 as a non-nullable `i64`. The row is
//! shared with the safekeeper and the controller seeds it with the whole
//! pageserver column group NULL, so "some of our columns are set" is a
//! reachable state that the deserializer must survive.
//!
//! The scenario forces exactly that row state on the shared `main` branch
//! while the pageserver is down, then restarts it: a branch with real data
//! whose cutoff column is NULL. Without the fix `pageserver start` never
//! goes healthy; with it the pageserver attaches, reads the branch back
//! intact, and treats the missing cutoff as "nothing has been GC'd yet".

use anyhow::{Context, bail};

use crate::cluster::{Cluster, step};
use crate::db;
use crate::tee_println;

pub async fn run(c: &Cluster) -> anyhow::Result<()> {
    let main = db::compute_conn(c.compute_port());
    let yb = db::yb_conn(c.yb_host(), c.yb_port(), c.db_name());
    let branch = c.branch();

    step("Write a workload so the branch has real page state to re-attach to");
    db::batch(
        &main,
        "DROP TABLE IF EXISTS null_cutoff_check;
         CREATE TABLE null_cutoff_check (id INT PRIMARY KEY, val TEXT NOT NULL);
         INSERT INTO null_cutoff_check (id, val)
           SELECT g, 'row-' || g FROM generate_series(1, 200) g;
         CHECKPOINT;",
    )
    .await?;

    step("Stop endpoint, then pageserver");
    c.yb(&["endpoint", "stop", "main"])?;
    c.yb(&["pageserver", "stop"])?;

    // Keep the pre-test value so the shared cluster is handed back to the
    // next scenario with the cutoff it had. The fix makes a NULL cutoff
    // *survivable*, not something the pageserver rewrites on attach, so
    // without this the column would stay NULL for the rest of the run.
    let cutoff_before = db::query_text_opt(
        &yb,
        &format!(
            "SELECT applied_gc_cutoff_lsn::text FROM amp_storage.branch_state \
             WHERE branch_id = '{branch}'"
        ),
    )
    .await?
    .context("main branch has no applied_gc_cutoff_lsn to save")?;
    tee_println!("  saved applied_gc_cutoff_lsn = {cutoff_before}");

    step("Force the #531 row state: consistent_lsn set, applied_gc_cutoff_lsn NULL");
    db::batch(
        &yb,
        &format!(
            "UPDATE amp_storage.branch_state SET applied_gc_cutoff_lsn = NULL \
             WHERE branch_id = '{branch}'"
        ),
    )
    .await?;

    // From here on the branch row is deliberately broken, so restore it
    // whatever happens — a bail with the column still NULL would wedge
    // every later scenario on the shared cluster behind the same
    // crash-loop this test is about.
    let verdict = attach_and_verify(c, &main).await;

    db::batch(
        &yb,
        &format!(
            "UPDATE amp_storage.branch_state SET applied_gc_cutoff_lsn = {cutoff_before} \
             WHERE branch_id = '{branch}'"
        ),
    )
    .await
    .context("restoring applied_gc_cutoff_lsn")?;

    verdict
}

async fn attach_and_verify(c: &Cluster, main: &str) -> anyhow::Result<()> {
    step("Restart pageserver — it must attach, not crash-loop on the NULL");
    c.yb_logged(&["pageserver", "start"])
        .context("pageserver failed to attach to a branch with a NULL applied_gc_cutoff_lsn (#531)")?;

    step("Restart endpoint and verify the branch reads back intact");
    c.yb(&["endpoint", "start", "main"])?;
    let total = db::query_i64(main, "SELECT count(*) FROM null_cutoff_check").await?;
    if total != 200 {
        bail!("post-attach row count wrong: {total} (expected 200)");
    }
    tee_println!("  ok  (total={total})");
    Ok(())
}
