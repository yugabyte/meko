//! #420: the image materialiser must fold a branch's clog (SLRU) chain whose
//! base image lives on the PARENT.
//!
//! A branch inherits the parent's xid counter, so the `pg_xact` page active at
//! the fork already has its `will_init` base under the parent's `branch_id`.
//! Branching copies nothing, so every commit on the branch appends a SETBIT
//! delta to that page and the branch never writes a base of its own. Once the
//! chain crosses `image_chain_limit_slru` (1000) the materialiser folds it —
//! and the fold needs that parent-side base.
//!
//! Before the fix the SLRU read was branch-local, so the fold saw deltas with
//! no base, walredo refused the base-less request, and the materialiser (which
//! treats a walredo failure as corruption) `fatal_abort`ed the pageserver. This
//! test failed there with `could not connect to pageserver`.
//!
//! Deliberately GC-independent, unlike `gc_slru_trim_reclaims`: materialisation
//! runs whether or not GC is on, and the abort did too — so asserting the
//! reclamation of trimmed rows would leave the GC-off configuration uncovered.
//! This asserts the positive artifact instead: an `is_image` row appears on the
//! BRANCH for the block whose only base is on the parent.

use std::time::Duration;

use anyhow::{Context, bail};
use tokio::time::sleep;

use crate::cluster::{Cluster, step};
use crate::db;

/// Kept well under `image_chain_limit_slru` (1000) so no materialise job can
/// fire yet. This window is what lets the test prove its own premise: the
/// branch holds clog deltas and no base of its own.
const WARMUP_COMMITS: i64 = 50;

/// Past the 1000-delta trigger with margin, so the fold is certain to be
/// dispatched for the hot block.
const TRIGGER_COMMITS: i64 = 1500;

pub async fn run(c: &Cluster) -> anyhow::Result<()> {
    let branch_pg_port = c.endpoint_port();
    let yb = db::yb_conn(c.yb_host(), c.yb_port(), c.db_name());

    step("Branch off main + endpoint");
    let b = c.branch_create_named("mat_st", c.branch())?;
    c.endpoint_create_on_branch("ep_mat_st", &b, branch_pg_port)?;
    let conn = db::compute_conn(branch_pg_port);

    let parent = db::query_text(
        &yb,
        &format!(
            "SELECT ancestor_branch_id::text FROM amp_storage.branch_state \
             WHERE branch_id = '{b}'"
        ),
    )
    .await
    .context("reading the branch's ancestor_branch_id")?;

    step("Commit a few txns so the branch accrues clog deltas but no base of its own");
    // A server-side loop that COMMITs per row: one clog SETBIT delta each.
    // (COMMIT can't run inside the implicit txn of a multi-statement batch, so
    // the procedure body and the CALL go in separate batches.)
    db::batch(&conn, "CREATE TABLE mat_st (id BIGINT)").await?;
    db::batch(
        &conn,
        "CREATE PROCEDURE mat_st_churn(n INT) LANGUAGE plpgsql AS $$
         BEGIN FOR i IN 1..n LOOP INSERT INTO mat_st VALUES (i); COMMIT; END LOOP; END $$;",
    )
    .await?;
    db::batch(&conn, &format!("CALL mat_st_churn({WARMUP_COMMITS})")).await?;
    db::batch(&conn, "CHECKPOINT").await?;

    // The clog block this branch is actually writing to (kind=0 = Clog).
    //
    // Poll for it. The compute's CHECKPOINT bounds the WAL, but the pageserver
    // ingests and flushes to YB on its own schedule, so these rows land a moment
    // later. Waiting is safe for the premise below: the warmup is far short of
    // the 1000-delta trigger and no new deltas accrue while we wait, so the
    // chain cannot grow into a fold.
    let hot_blknum = format!(
        // `blknum::bigint`: the column is SMALLINT, and COALESCE with an
        // integer literal would otherwise hand back an int4.
        "SELECT COALESCE((SELECT blknum::bigint FROM amp_storage.slru_pages \
           WHERE branch_id = '{b}' AND kind = 0 \
           GROUP BY blknum ORDER BY count(*) DESC LIMIT 1), -1)"
    );
    let mut hot = -1;
    for _ in 0..30 {
        hot = db::query_i64(&yb, &hot_blknum).await?;
        if hot >= 0 {
            break;
        }
        db::batch(&conn, "CHECKPOINT").await?;
        sleep(Duration::from_secs(1)).await;
    }
    if hot < 0 {
        bail!(
            "no clog rows reached amp_storage.slru_pages for this branch within 30s of \
             {WARMUP_COMMITS} committed txns — either the commits never touched pg_xact \
             or the pageserver never flushed them, so this test proves nothing"
        );
    }
    let on_branch = |pred: &str| {
        format!(
            "SELECT count(*) FROM amp_storage.slru_pages \
             WHERE branch_id = '{b}' AND kind = 0 AND blknum = {hot} AND {pred}"
        )
    };

    // The premise, asserted rather than assumed. All three must hold for this
    // to be exercising #420 at all:
    //   1. the branch has deltas on the hot block,
    //   2. the branch has NO anchor of its own for it,
    //   3. the parent does.
    // (2) and (3) together are the bug's exact shape: a fold of this block can
    // only succeed by reading across into the parent. If (2) or (3) stops
    // holding the premise is gone, so those bail as INCONCLUSIVE — not as a
    // regression in the fix.
    let branch_anchors = db::query_i64(&yb, &on_branch("will_init")).await?;
    if branch_anchors != 0 {
        bail!(
            "INCONCLUSIVE: branch already has its own base for clog block {hot} \
             ({branch_anchors} will_init rows), so a branch-local fold would have \
             succeeded and #420 is not being exercised — the churn most likely \
             crossed a clog page boundary and zeroed a fresh page locally"
        );
    }
    let deltas = db::query_i64(&yb, &on_branch("NOT will_init")).await?;
    if deltas == 0 {
        bail!("branch holds no clog deltas on block {hot}; nothing for a fold to collapse");
    }
    let parent_anchors = db::query_i64(
        &yb,
        &format!(
            "SELECT count(*) FROM amp_storage.slru_pages \
             WHERE branch_id = '{parent}' AND kind = 0 AND blknum = {hot} AND will_init"
        ),
    )
    .await?;
    if parent_anchors == 0 {
        bail!(
            "INCONCLUSIVE: neither branch nor parent {parent} holds a base for clog \
             block {hot}, so there is no parent-side base for the fold to find"
        );
    }

    step("Churn past the materialise threshold: the fold must resolve the parent's base");
    // Pre-fix this is where the pageserver dies, so name the failure rather
    // than let it surface as a bare connection error.
    let bug = format!(
        "pre-#420 symptom: the materialiser fatal-aborts folding clog block {hot} on \
         this branch, because its base image is on parent {parent} and the SLRU read \
         only looked at the branch (branch had {deltas} deltas, 0 anchors)"
    );
    db::batch(&conn, &format!("CALL mat_st_churn({TRIGGER_COMMITS})"))
        .await
        .with_context(|| format!("churn past the materialise threshold failed — {bug}"))?;
    db::batch(&conn, "CHECKPOINT")
        .await
        .with_context(|| format!("checkpoint after the churn failed — {bug}"))?;

    // The artifact that proves the fold succeeded: an is_image row on THIS
    // branch for a block whose only base was on the parent. Materialisation is
    // asynchronous, so poll; each round checkpoints to keep the flush horizon
    // (which bounds the fold's target LSN) moving.
    let mut images = 0;
    for _ in 0..40 {
        images = db::query_i64(&yb, &on_branch("is_image"))
            .await
            .with_context(|| format!("polling for the branch's clog image — {bug}"))?;
        if images > 0 {
            break;
        }
        db::batch(&conn, "CHECKPOINT")
            .await
            .with_context(|| format!("checkpoint while polling failed — {bug}"))?;
        sleep(Duration::from_secs(1)).await;
    }

    // The pageserver must still be serving: a read that goes through it, with
    // the row count the churn should have produced. Pre-fix the process is gone
    // by now and this cannot answer at all.
    let rows = db::query_i64(&conn, "SELECT count(*) FROM mat_st")
        .await
        .with_context(|| format!("reading back the churned table failed — {bug}"))?;

    let _ = c.yb(&["branch", "drop", "--branch-id", &b]);
    c.yb_quiet(&["pageserver", "stop", "ep_mat_st", "-m", "immediate"]);

    if images == 0 {
        bail!(
            "materialiser never imaged clog block {hot} on the branch after \
             {TRIGGER_COMMITS} more commits (>{} delta trigger). Either the fold is not \
             being dispatched, or it is failing to resolve the base image that lives on \
             parent {parent} (#420)",
            1000,
        );
    }
    let expected = WARMUP_COMMITS + TRIGGER_COMMITS;
    if rows != expected {
        bail!("expected {expected} rows in mat_st after the churn, read back {rows}");
    }
    Ok(())
}
