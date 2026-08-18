//! The write fence, end to end: take the endpoint's lease away from it and
//! the endpoint must stop writing.
//!
//! Everything else in this suite depends on the fence *granting* — every
//! endpoint here holds a lease and every write is checked against it — so the
//! happy path is covered a hundred times over. Nothing covered the path the
//! fence exists for. A fence that has quietly stopped refusing looks exactly
//! like a fence that is working, right up until two endpoints write the same
//! branch.
//!
//! The lease is stolen in SQL rather than by standing up a second supervisor:
//! `token_acquire` grants unconditionally to the current holder (that is what
//! makes renewal cheap), so overwriting `client_id` is precisely what a real
//! takeover does to the loser, and it needs no second process to schedule.

use std::time::Duration;

use anyhow::{Context, bail};
use tokio::time::sleep;

use crate::cluster::{Cluster, step};
use crate::db;

/// How long the thief holds the branch for.
///
/// Longer than two things, for two reasons. Longer than the 5s renewal interval,
/// so at least one renewal is certainly refused even if a loaded runner delays
/// the renewal thread — that refusal is the point of the scenario. And longer
/// than the 15s TTL, so the supervisor's held token runs out while the thief
/// still holds the row; a stolen lease that expired first would be handed
/// straight back and nothing would ever stop writing.
const STOLEN_LEASE_SECS: i64 = 30;

/// The supervisor renews every 5s against a 15s TTL, so the refusal lands
/// within one interval. Allow several: the renewal thread's clock and ours are
/// not the same clock.
const LAPSE_BUDGET: Duration = Duration::from_secs(45);

/// Reclaiming waits out [`STOLEN_LEASE_SECS`] and then brings three services
/// and a compute back up, so it gets considerably longer.
const RECLAIM_BUDGET: Duration = Duration::from_secs(180);

/// Whoever the lease is handed to. Nothing renews it, so it expires on its own
/// — which is exactly what a real second endpoint disappearing looks like, and
/// is how the supervisor gets its branch back.
const THIEF: &str = "fence-lapse-test";

pub async fn run(c: &Cluster) -> anyhow::Result<()> {
    // Unique across the whole suite, not just this half of it. Endpoints are
    // not torn down between scenarios — `prep_for_next_test` only touches
    // `main` — so a port two scenarios share is a later `endpoint create` that
    // fails on a port the earlier one is still bound to. Unique-by-construction
    // rather than cleanup at the end, because a scenario that bails midway
    // never reaches its cleanup and would leave the same trap behind.
    let branch_pg_port = c.compute_port() + 300;
    let yb = db::yb_conn(c.yb_host(), c.yb_port(), c.db_name());

    step("Dedicated branch + endpoint");
    let b = c.branch_create_named("fence_lapse", c.branch())?;
    c.endpoint_create_on_branch("ep_fence_lapse", &b, branch_pg_port)?;
    let conn = db::compute_conn(branch_pg_port);

    step("The endpoint holds its lease and writes");
    db::batch(
        &conn,
        "CREATE TABLE fenced (id INT PRIMARY KEY, payload TEXT NOT NULL);
         INSERT INTO fenced SELECT g, md5(g::text) FROM generate_series(1, 500) g;
         CHECKPOINT;",
    )
    .await?;
    let before = db::query_i64(&conn, "SELECT count(*) FROM fenced").await?;
    if before != 500 {
        bail!("setup did not commit: {before} rows");
    }

    // Find the row rather than assuming how the id is spelled: the resource is
    // `branch:<BRANCH_ID>` as the supervisor was handed it, and the CLI's id and
    // the env var have not always agreed on dashes.
    let resource = db::query_text_opt(
        &yb,
        &format!(
            "SELECT resource_id FROM amp_registry.resource_token \
             WHERE replace(resource_id, '-', '') = replace('branch:{b}', '-', '')"
        ),
    )
    .await?
    .with_context(|| {
        format!("no lease row for branch {b}: this endpoint is not holding a write lease")
    })?;
    let holder = db::query_text(
        &yb,
        &format!("SELECT client_id FROM amp_registry.resource_token WHERE resource_id = '{resource}'"),
    )
    .await?;
    if holder.is_empty() || holder == THIEF {
        bail!("lease for {resource} is held by {holder:?}, not by the endpoint's supervisor");
    }
    println!("      lease held by {holder}");

    step("Take the lease away");
    // Someone else's name, with a deadline far enough out that the supervisor's
    // next renewal is certainly refused — not expired, and not its row any more
    // — which is the answer a real second endpoint's takeover leaves behind.
    //
    // The deadline is also what the supervisor is told to wait before trying
    // again (`token_acquire` returns `retry_after_ms`), and it sleeps that long,
    // so it doubles as how long reclaiming takes. Deleting the row early does
    // NOT shorten that: the supervisor is already asleep.
    db::batch(
        &yb,
        &format!(
            "UPDATE amp_registry.resource_token \
             SET client_id = '{THIEF}', \
                 token = (EXTRACT(EPOCH FROM now()) * 1000000)::bigint \
                         + {STOLEN_LEASE_SECS} * 1000000, \
                 granted_us = (EXTRACT(EPOCH FROM now()) * 1000000)::bigint \
             WHERE resource_id = '{resource}'"
        ),
    )
    .await?;

    step("The endpoint stops writing");
    let deadline = tokio::time::Instant::now() + LAPSE_BUDGET;
    let mut last = String::from("writes still succeeded");
    loop {
        if tokio::time::Instant::now() >= deadline {
            bail!("the endpoint kept writing {LAPSE_BUDGET:?} after losing its lease: {last}");
        }
        sleep(Duration::from_secs(1)).await;
        // A write, not a read: reads off a stale endpoint are not what the
        // fence is for, and the pageserver serves them until it is stopped.
        match db::batch(&conn, "INSERT INTO fenced VALUES (100001, 'after-the-lapse')").await {
            Err(e) => {
                last = format!("{e:#}");
                break;
            }
            Ok(()) => {
                // Undo it so the row count below still means something if the
                // very next attempt is the one that fails.
                let _ = db::batch(&conn, "DELETE FROM fenced WHERE id = 100001").await;
            }
        }
    }
    println!("      writes stopped: {last}");

    step("It takes the branch back once the stolen lease expires");
    // Nothing renews the thief's lease, so it lapses and the supervisor — which
    // has been waiting exactly that long — acquires and restarts the endpoint.
    // A fence that stops an endpoint *permanently* fails here, which is as much
    // a bug as one that never stops it.
    let deadline = tokio::time::Instant::now() + RECLAIM_BUDGET;
    loop {
        if tokio::time::Instant::now() >= deadline {
            bail!("the endpoint did not come back within {RECLAIM_BUDGET:?} of the stolen lease expiring");
        }
        sleep(Duration::from_secs(2)).await;
        if let Ok(n) = db::query_i64(&conn, "SELECT count(*) FROM fenced").await {
            if n != before {
                bail!("came back with {n} rows, not the {before} committed before the lapse");
            }
            break;
        }
    }
    Ok(())
}
