//! page_service "GoingAway" frame: keep a pagestream connection busy,
//! gracefully stop the pageserver, and verify it emitted the GoingAway
//! frame during shutdown and that the stop did not hang. The assertion
//! is on the PS side (the load-bearing half of the protocol); idle
//! compute backends may not drain the frame, so we don't assert
//! compute-side receipt here.

use std::time::{Duration, Instant};

use anyhow::bail;

use crate::cluster::{Cluster, step};
use crate::tee_println;
use crate::{db, psql};

pub async fn run(c: &Cluster) -> anyhow::Result<()> {
    let main = db::compute_conn(c.compute_port());

    step("Seed a small table so the compute has something to read");
    db::batch(
        &main,
        "DROP TABLE IF EXISTS going_away_seed;
         CREATE TABLE going_away_seed (id INT PRIMARY KEY, val TEXT NOT NULL);
         INSERT INTO going_away_seed SELECT g, 'row-' || g FROM generate_series(1, 100) g;
         CHECKPOINT;",
    )
    .await?;

    step("Start a background read loop on the compute");
    let mut bg = psql::spawn_bg(
        c.compute_port(),
        "SELECT pg_sleep(0.05), count(*) FROM going_away_seed; \
         SELECT generate_series(1, 1000) AS i, pg_sleep(0.05);",
    )?;
    tokio::time::sleep(Duration::from_secs(1)).await;

    step("Gracefully stop the pageserver");
    let t0 = Instant::now();
    c.yb(&["pageserver", "stop"])?;
    let stop_secs = t0.elapsed().as_secs_f64();
    tee_println!("  pageserver stop took {stop_secs:.2}s");

    // Kill the background loop; we don't care about its outcome.
    let _ = bg.kill();
    let _ = bg.wait();

    step("Confirm the pageserver wrote GoingAway frames during shutdown");
    let ps_log = c.ps_log();
    let content = match std::fs::read_to_string(&ps_log) {
        Ok(c) => c,
        Err(_) => bail!("pageserver log not readable at {}", ps_log.display()),
    };
    let frames = content
        .lines()
        .filter(|l| l.contains("writing GoingAway frame on graceful shutdown"))
        .count();
    if frames >= 1 {
        tee_println!("  ok  — pageserver wrote {frames} GoingAway frame(s)");
    } else {
        eprintln!("FAIL — pageserver did not write any GoingAway frames");
        eprintln!("  (tail of pageserver log:)");
        for l in content.lines().rev().take(30).collect::<Vec<_>>().iter().rev() {
            eprintln!("{l}");
        }
        bail!("no GoingAway frames");
    }

    step("Confirm pageserver stop did not hang");
    // Generous 3s ceiling for host noise; the per-connection task exits
    // cleanly before the 1s shutdown budget with the frame in place.
    if stop_secs < 3.0 {
        tee_println!("  ok  — stop took {stop_secs:.2}s (< 3s)");
    } else {
        bail!("pageserver stop took {stop_secs:.2}s, expected < 3s");
    }

    Ok(())
}
