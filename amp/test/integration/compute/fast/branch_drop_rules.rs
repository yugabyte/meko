//! Branch drop rules through the CLI and controller:
//!
//! - a branch with a live child cannot be dropped;
//! - a drop tombstones the branch row instead of deleting it;
//! - a fork off a dropped branch is refused, leaving no rows in either table.
//!
//! The fork ↔ parent-drop race is covered by
//! controller/internal/core/pg_fork_drop_race_test.go.
//!
//! Uses its own project, not `main`, because it drops the branches it creates.

use std::time::Duration;

use anyhow::bail;
use tokio::time::sleep;

use crate::cluster::{Cluster, step};
use crate::db;

const PARENT: &str = "branch_drop_parent";
const CHILD: &str = "branch_drop_child";
const ORPHAN: &str = "branch_drop_orphan";
const ENDPOINT: &str = "ep_branch_drop";

/// Whether the branch is still live (row present, not tombstoned). The storage
/// layer's flat branch id is the dashless rendering of the row's uuid.
async fn live(yb: &str, branch_id: &str) -> anyhow::Result<i64> {
    db::query_i64(
        yb,
        &format!(
            "SELECT count(*) FROM amp_registry.branches \
             WHERE replace(branch_id::text, '-', '') = '{branch_id}' AND dropped_at IS NULL"
        ),
    )
    .await
}

/// Rows for a branch id regardless of tombstone state — a drop must leave the row
/// behind, so this stays 1 where `live` goes to 0.
async fn rows_for(yb: &str, branch_id: &str) -> anyhow::Result<i64> {
    db::query_i64(
        yb,
        &format!(
            "SELECT count(*) FROM amp_registry.branches \
             WHERE replace(branch_id::text, '-', '') = '{branch_id}'"
        ),
    )
    .await
}

/// Rows for a branch NAME. Only for a branch whose creation was refused, where
/// there is no id to address it by. A project's default branch is named `main`,
/// not after the project, so never use this for one.
async fn named_rows(yb: &str, name: &str) -> anyhow::Result<i64> {
    db::query_i64(
        yb,
        &format!("SELECT count(*) FROM amp_registry.branches WHERE name = '{name}'"),
    )
    .await
}

/// The ancestor's children in the storage layer, where a fork's second row lands.
async fn child_state_rows(yb: &str, ancestor: &str) -> anyhow::Result<i64> {
    db::query_i64(
        yb,
        &format!(
            "SELECT count(*) FROM amp_storage.branch_state WHERE ancestor_branch_id = '{ancestor}'"
        ),
    )
    .await
}

pub async fn run(c: &Cluster) -> anyhow::Result<()> {
    let yb = db::yb_conn(c.yb_host(), c.yb_port(), c.db_name());
    // N must keep compute_port + N inside this run's reserved block: the compute lane
    // is base+700 and the block ends at base+900, so N <= 200. N = 300 lands on
    // base+1000 — the next block's base, which bring-up never probed for freeness. CI
    // failed there: postgres could not bind port 12000 (base 11000), five retries.
    let pg_port = c.compute_port() + 200;

    // The parent needs an endpoint to be forkable at all: `branch create` reads
    // the default fork point from the safekeeper co-located with the parent's
    // writer, and refuses an unmaterialized ancestor.
    step("Create an isolated parent branch + endpoint");
    let parent_id = c.project_create(PARENT)?;
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

    step("Fork a child off the live parent");
    let child_id = c.branch_create_named(CHILD, &parent_id)?;
    if child_state_rows(&yb, &parent_id).await? != 1 {
        bail!("accepted fork did not register a branch_state row under the parent");
    }

    step("A parent with a live child cannot be dropped");
    let (ok, out) = c.yb_try(&["branch", "drop", "--branch-id", &parent_id])?;
    if ok {
        bail!("dropping a parent with a live child succeeded:\n{out}");
    }
    if !out.contains("child") {
        bail!("refusal does not name the child branches as the reason:\n{out}");
    }
    if live(&yb, &parent_id).await? != 1 {
        bail!("refused drop tombstoned the parent anyway");
    }

    step("With the child dropped, the parent drops too");
    c.yb(&["branch", "drop", "--branch-id", &child_id])?;
    if live(&yb, &child_id).await? != 0 {
        bail!("dropped child is still live");
    }
    // Retires the parent's endpoint on the way — a branch with endpoint records
    // cannot be tombstoned.
    c.yb(&["branch", "drop", "--branch-id", &parent_id])?;
    if live(&yb, &parent_id).await? != 0 {
        bail!("dropped parent is still live");
    }
    // Tombstoned, not deleted: a branch id is never reused, and the branch's pages
    // and WAL outlive the drop until GC reclaims them (#435).
    if rows_for(&yb, &parent_id).await? != 1 || rows_for(&yb, &child_id).await? != 1 {
        bail!("a dropped branch's row was deleted rather than tombstoned");
    }

    step("A fork off the dropped parent is refused and leaves no rows");
    let states_before = child_state_rows(&yb, &parent_id).await?;
    let (created, out) = c.branch_create_at(ORPHAN, &parent_id, "0/0")?;
    if let Some(id) = created {
        bail!("fork off a dropped parent was accepted as {id}:\n{out}");
    }
    if named_rows(&yb, ORPHAN).await? != 0 {
        bail!("refused fork left an amp_registry.branches row behind");
    }
    if child_state_rows(&yb, &parent_id).await? != states_before {
        bail!("refused fork left an amp_storage.branch_state row behind");
    }

    Ok(())
}
