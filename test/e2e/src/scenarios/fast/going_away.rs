//! page_service "GoingAway" frame: keep a pagestream connection busy,
//! gracefully stop the pageserver, and verify it emitted the GoingAway
//! frame during shutdown and that the stop did not hang. The assertion
//! is on the PS side (the load-bearing half of the protocol); idle
//! compute backends may not drain the frame, so we don't assert
//! compute-side receipt here.
//!
//! Doubles as the regression guard for the torn-tail decode panic on the
//! safekeeper's interpreted-WAL (SK→PS) path: a walproposer election
//! truncates WAL and the next term rewrites forward from the truncation
//! point, and the safekeeper must not serve *stale cached* WAL bytes past
//! that point (it used to — `truncate_wal` dropped the YB rows but left
//! the in-memory `WalCache`, so a fresh SK→PS sender decoded an
//! inconsistent page and tripped `critical_timeline!`). That window only
//! opens once the WAL has grown past several 8 KiB pages with
//! page-spanning records, so we seed wide rows and keep an active writer
//! in flight across the stop — otherwise (small WAL) the scenario never
//! exercises it. The final SK-log scan asserts the panic did not fire.

use std::time::{Duration, Instant};

use anyhow::bail;

use crate::cluster::{Cluster, step};
use crate::tee_println;
use crate::{db, psql};

/// Wide enough that a single row's WAL record spans an 8 KiB page
/// boundary, so a truncation can land mid-record — the precondition for
/// the stale-continuation-page bug.
const WIDE_BYTES: usize = 7_000;

pub async fn run(c: &Cluster) -> anyhow::Result<()> {
    let main = db::compute_conn(c.compute_port());

    step("Seed wide rows so WAL spans many pages (the torn-tail precondition)");
    db::batch(
        &main,
        &format!(
            "DROP TABLE IF EXISTS going_away_seed;
             CREATE TABLE going_away_seed (id BIGINT PRIMARY KEY, val TEXT NOT NULL);
             INSERT INTO going_away_seed SELECT g, repeat('x', {WIDE_BYTES})
               FROM generate_series(1, 3000) g;
             CHECKPOINT;"
        ),
    )
    .await?;

    // Two background loops: a reader keeps a pagestream connection busy
    // (so the pageserver actually has a peer to send the GoingAway frame
    // to), and a writer streams wide rows so WAL is actively flowing —
    // and a mid-record truncation is possible — when the pageserver
    // stops.
    step("Start background read + write loops on the compute");
    let mut bg_read = psql::spawn_bg(
        c.compute_port(),
        "SELECT pg_sleep(0.05), count(*) FROM going_away_seed; \
         SELECT generate_series(1, 1000) AS i, pg_sleep(0.05);",
    )?;
    let mut writer_sql = String::new();
    for batch in 0..20 {
        let lo = 1_000_000 + batch * 100 + 1;
        let hi = 1_000_000 + batch * 100 + 100;
        writer_sql.push_str(&format!(
            "INSERT INTO going_away_seed SELECT g, repeat('w', {WIDE_BYTES}) \
               FROM generate_series({lo}, {hi}) g; SELECT pg_sleep(0.05); "
        ));
    }
    let mut bg_write = psql::spawn_bg(c.compute_port(), &writer_sql)?;
    tokio::time::sleep(Duration::from_secs(1)).await;

    step("Gracefully stop the pageserver");
    let t0 = Instant::now();
    c.yb(&["pageserver", "stop"])?;
    let stop_secs = t0.elapsed().as_secs_f64();
    tee_println!("  pageserver stop took {stop_secs:.2}s");

    // Kill the background loops; we don't care about their outcome.
    let _ = bg_read.kill();
    let _ = bg_read.wait();
    let _ = bg_write.kill();
    let _ = bg_write.wait();

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

    step("Confirm the safekeeper did not serve a stale WAL page across a truncation");
    // The torn-tail bug surfaces as the interpreted-WAL reader panicking on
    // an inconsistent page; `critical_timeline!` logs it before wedging.
    // (The harness fatal-marker watchdog also catches this mid-run — this
    // is an explicit, self-documenting backstop.)
    let sk_log = c
        .repo_dir()
        .join("endpoints/main/safekeeper/safekeeper-1.log");
    let sk = std::fs::read_to_string(&sk_log).unwrap_or_default();
    let hits: Vec<&str> = sk
        .lines()
        .filter(|l| {
            l.contains("invalid xlog page header")
                || l.contains("interpreted wal reader: failed to decode")
        })
        .collect();
    if !hits.is_empty() {
        eprintln!("FAIL — {} torn-tail decode line(s) in safekeeper log", hits.len());
        for l in hits.iter().take(5) {
            eprintln!("{l}");
        }
        bail!("safekeeper served a stale WAL page across a truncation (interpreted-WAL decode error)");
    }
    tee_println!("  ok  — safekeeper log clean of torn-tail decode errors");

    Ok(())
}
