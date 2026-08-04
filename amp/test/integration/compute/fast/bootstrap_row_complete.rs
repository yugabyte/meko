//! #552: a bootstrapped branch's `amp_storage.branch_state` row must carry
//! each daemon's whole column group, and say so via that daemon's explicit
//! `*_initialized` flag.
//!
//! `update_branch_initial_state` is the only writer of `initdb_lsn`,
//! `pageserver_initialized`, and used to be
//! scoped `WHERE consistent_lsn IS NULL`. On the bootstrap path that predicate
//! is already false by the time it runs — `claim_generation` satisfies the gen
//! fence before the branch is opened, and the initdb import's flush lands
//! `consistent_lsn` via `advance_consistent_lsn` — so the statement matched
//! zero rows and the whole creation write was silently dropped.
//!
//! `applied_gc_cutoff_lsn` is deliberately NOT asserted here: the harness runs
//! with `enable_gc = true`, so a GC tick stamps that column within a second and
//! would mask the bug. `initdb_lsn` has no second writer, so it stays NULL
//! for the life of the branch and is the honest signal.
//!
//! This asserts against the shared `main` branch, which the harness bootstraps
//! at cluster setup — the same code path every branch takes. The safekeeper's
//! flag is covered too: its group is filled on the first walproposer handshake,
//! which has happened by the time any scenario runs.

use anyhow::bail;

use crate::cluster::{Cluster, step};
use crate::db;
use crate::tee_println;

/// Columns that must be non-NULL once the branch is up.
const POPULATED: &[&str] = &["initdb_lsn", "consistent_lsn", "current_generation"];

/// Each daemon's explicit "my group exists" discriminator. These are booleans,
/// so "present" is not the assertion — `false` is a perfectly non-NULL value
/// and means the creation write never landed.
const FLAGS: &[&str] = &["pageserver_initialized", "safekeeper_initialized"];

pub async fn run(c: &Cluster) -> anyhow::Result<()> {
    let yb = db::yb_conn(c.yb_host(), c.yb_port(), c.db_name());
    let branch = c.branch();

    step("Each daemon's initialized flag must be true");
    for flag in FLAGS {
        let got = db::query_text_opt(
            &yb,
            &format!(
                "SELECT {flag}::text FROM amp_storage.branch_state \
                 WHERE branch_id = '{branch}'"
            ),
        )
        .await?;
        match got.as_deref() {
            Some("true") => tee_println!("  {flag} = true"),
            other => bail!(
                "branch_state.{flag} is {} on a live branch — that daemon's \
                 creation write never landed (#552)",
                other.unwrap_or("NULL")
            ),
        }
    }

    step("The pageserver's column group must be populated");
    for column in POPULATED {
        let got = db::query_text_opt(
            &yb,
            &format!(
                "SELECT {column}::text FROM amp_storage.branch_state \
                 WHERE branch_id = '{branch}'"
            ),
        )
        .await?;
        match got.as_deref() {
            Some(v) if !v.is_empty() => tee_println!("  {column} = {v}"),
            _ => bail!(
                "branch_state.{column} is NULL on a bootstrapped branch — the \
                 creation write was dropped (#552)"
            ),
        }
    }

    Ok(())
}
