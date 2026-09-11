//! SKIPPED IN CI via `INTEGRATION_SKIP` in `.github/workflows/amp-pr-build.yml`.
//! It still runs locally.
//!
//! TODO(#1091): remove that skip. The `WARMUP` below is a fixed 6 s, which no
//! longer reliably leaves the in-memory position ahead of the durable frontier
//! at kill time, so all `ROUNDS` miss and the run fails with "the race was not
//! exercised" — the setup, not the assertion. The fix is to wait for that
//! condition rather than guess at it with wall clock.
//! https://github.com/yugabyte/meko/issues/1091
//!
//! GC cutoff vs walreceiver replay (#588). Sequence:
//!
//!   consistent_lsn = 100; pageserver applied to 180 in memory, flush in flight
//!   safekeeper killed, pageserver stays up
//!   reconnect resumes at 100 and re-applies 100→180 over live state
//!   flush lands: consistent_lsn = 180, cutoff follows, GC deletes below 180
//!   replay at 150 reads at 149 — below the cutoff, row gone, walreceiver panics
//!
//! Rules: `compute/architecture/pageserver.md` § Replay correctness rules.
//!
//! Required to reproduce: the safekeeper dies with the pageserver up (killing the
//! pageserver discards the in-memory position that rewinds); SIGKILL, since
//! `-m immediate` is SIGQUIT and runs the shutdown drain, leaving an empty replay
//! window; the suite's `pitr_interval=0s` + small `gc_horizon` (`cluster.rs`).
//!
//! Asserts the skip counter rises, since a run that never opened a replay window
//! would otherwise pass. Own amp cluster: a fork of `main` pins main's cutoff for the
//! rest of the run.

use std::time::{Duration, Instant};

use anyhow::bail;
use tokio::time::sleep;

use crate::cluster::{Cluster, step};
use crate::db;
use crate::psql;
use crate::tee_println;

const CLUSTER: &str = "catchup_race";
const ENDPOINT_ID: &str = "ep_catchup_race";
const TABLE: &str = "catchup_race_counter";

/// Non-zero means a connection rewound below the durable frontier.
const SKIP_METRIC: &str = "pageserver_yb_wal_ingest_records_skipped_durable";

/// Kill attempts. The pre-kill flush has to land after the reconnect; in practice
/// that happens in the first or second round.
const ROUNDS: usize = 4;

/// Writer runtime before the kill, so the in-memory position leads the durable
/// frontier (the page writer flushes at 12 MB / 5 s).
const WARMUP: Duration = Duration::from_secs(6);

/// Per-round failure timeout; a round ends as soon as the counter rises.
const ROUND_TIMEOUT: Duration = Duration::from_secs(45);

pub async fn run(c: &Cluster) -> anyhow::Result<()> {
    // Offsets must stay <= 200: the compute lane is base+700 and the run's reserved
    // block ends at base+900, so anything above that is unprobed and can be in use.
    let pg_port = c.compute_port() + 110;

    step("Create an own amp cluster (a fork of `main` would pin main's cutoff)");
    let branch_id = c.cluster_create(CLUSTER)?;
    c.endpoint_create_on_branch(ENDPOINT_ID, &branch_id, pg_port)?;
    let compute = db::compute_conn(pg_port);
    let ps_http = c.endpoint_ps_http_port(ENDPOINT_ID)?;
    let sk_pid = c
        .repo_dir()
        .join("endpoints")
        .join(ENDPOINT_ID)
        .join("safekeeper/safekeeper.pid");

    step("Seed the counter table + a commit-per-row fill procedure");
    // ON CONFLICT: a later round's writer would collide with earlier rows.
    db::batch(
        &compute,
        &format!(
            "CREATE TABLE {TABLE} (id BIGINT PRIMARY KEY, pad TEXT NOT NULL)
                 WITH (autovacuum_enabled = false);
             CREATE OR REPLACE PROCEDURE {TABLE}_fill(iters INT) AS $$
             BEGIN
                 FOR i IN 1..iters LOOP
                     INSERT INTO {TABLE} (id, pad) VALUES (i, repeat('x', 400))
                         ON CONFLICT DO NOTHING;
                     COMMIT;
                 END LOOP;
             END;
             $$ LANGUAGE plpgsql;
             CHECKPOINT;"
        ),
    )
    .await?;

    let mut engaged = 0.0;
    for round in 0..ROUNDS {
        step(&format!(
            "round {round}: hammer commit-per-row, then SIGKILL the safekeeper mid-flush"
        ));
        let mut bg = psql::spawn_bg(pg_port, &format!("CALL {TABLE}_fill(100000000);"))?;
        sleep(WARMUP).await;
        tee_println!(
            "  round {round}: durable frontier {}",
            c.consistent_lsn(&branch_id).await?.unwrap_or(0)
        );

        // Pageserver stays up: its in-memory position, ahead of the durable
        // frontier, is what the reconnect rewinds from.
        c.kill9(&sk_pid, "safekeeper")?;

        let _ = bg.kill();
        let _ = bg.wait();

        step(&format!(
            "round {round}: restart the safekeeper — the pageserver reconnects and rewinds"
        ));
        c.yb_quiet(&["safekeeper", "stop", ENDPOINT_ID, "-m", "immediate"]);
        c.yb(&["safekeeper", "start", ENDPOINT_ID])?;

        // Only now, with a safekeeper back: psql died but the backend did not (`CALL
        // fill(...)` returns nothing, so it never sees the closed socket and would
        // commit for the rest of the run). This round-trip can block for as long as
        // the branch has no safekeeper, so it must not sit between the kill and the
        // restart — that gap is what the race needs kept narrow.
        let _ = db::batch(
            &compute,
            &format!(
                "SELECT pg_terminate_backend(pid) FROM pg_stat_activity \
                  WHERE query LIKE 'CALL {TABLE}_fill%' AND pid <> pg_backend_pid()"
            ),
        )
        .await;

        // Poll for the counter; a panic is caught by the fatal-marker watcher.
        let deadline = Instant::now() + ROUND_TIMEOUT;
        while Instant::now() < deadline {
            engaged = c.metric_sum(ps_http, SKIP_METRIC).await.unwrap_or(0.0);
            if engaged > 0.0 {
                break;
            }
            sleep(Duration::from_millis(500)).await;
        }
        if engaged > 0.0 {
            tee_println!(
                "  round {round}: guard engaged — {engaged} already-durable record(s) skipped"
            );
            break;
        }
        tee_println!("  round {round}: no rewind below the frontier this time");
    }

    // Committed rows must be a contiguous prefix 1..k: the fill procedure commits
    // per row in ascending id, so count(*) == max(id) unless a record went missing.
    // Read after an endpoint restart so the answer comes from the store, not the
    // compute's buffers. This is the counterweight to the counter assertion above —
    // a skip that dropped WAL which was NOT durable would also raise the counter,
    // and only this notices.
    let survived = if engaged > 0.0 {
        step("Restart the compute and check the committed rows are a contiguous prefix");
        c.yb_quiet(&["endpoint", "stop", ENDPOINT_ID, "--mode", "immediate"]);
        c.yb(&["endpoint", "start", ENDPOINT_ID])?;
        Some(
            db::query_i64(
                &compute,
                &format!("SELECT count(*) - COALESCE(max(id), 0) FROM {TABLE}"),
            )
            .await?,
        )
    } else {
        None
    };

    // Before any bail: a live branch pins its cutoff for the rest of the run, and a
    // live endpoint's log keeps being scanned by the fatal-marker watcher.
    step("Tear down");
    c.yb_quiet(&["endpoint", "stop", ENDPOINT_ID, "--mode", "immediate"]);
    let _ = c.yb(&["branch", "drop", "--branch-id", &branch_id]);
    c.yb_quiet(&["pageserver", "stop", ENDPOINT_ID, "-m", "immediate"]);
    c.yb_quiet(&["safekeeper", "stop", ENDPOINT_ID, "-m", "immediate"]);

    match survived {
        None => bail!(
            "no round rewound below the durable frontier after {ROUNDS} kills \
             ({SKIP_METRIC} stayed 0): the race was not exercised"
        ),
        Some(0) => Ok(()),
        Some(gap) => bail!(
            "committed rows are not a contiguous prefix: count(*) - max(id) = {gap} \
             — a record that was not durable got skipped"
        ),
    }
}

