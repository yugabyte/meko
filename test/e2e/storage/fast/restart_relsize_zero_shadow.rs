//! #22 regression: rel-size cache pin during WAL replay must not
//! zero-shadow live blocks. Insert-heavy load extends a relation while
//! the PS flushes rows to YB continuously; an immediate PS stop leaves
//! the persisted checkpoint behind YB's durable horizon, so the restart
//! replays WAL it already flushed. Pre-fix, `put_rel_extend` never
//! advances the rel-size cache during that replay (storage already
//! knows every size it proposes), the cache stays pinned at the
//! replay-start size, and every subsequent record re-zero-fills
//! `[pinned_size, blkno)` — stamping all-zero page images at LSNs
//! *above* the blocks' live deltas. Detection: any all-zero image row
//! whose LSN is greater than the oldest row of its block is shadowing
//! data (a legitimate gap-fill zero is always the block's first row).

use std::time::{Duration, Instant};

use anyhow::bail;
use regex::Regex;

use crate::cluster::{Cluster, MARKERS, step};
use crate::tee_println;
use crate::db;

pub async fn run(c: &Cluster) -> anyhow::Result<()> {
    let main = db::compute_conn(c.compute_port());
    let yb = db::yb_conn(c.yb_host(), c.yb_port(), c.db_name());
    let branch = c.branch();
    // The PS log accumulates across iterations of this scenario (the
    // in-scenario restart does not truncate it); remember where it
    // starts so the marker scan below only judges this iteration.
    let log_start = std::fs::metadata(c.ps_log()).map(|m| m.len() as usize).unwrap_or(0);

    step("Create an insert-only table with wide rows (fast extension)");
    db::batch(
        &main,
        "DROP TABLE IF EXISTS zero_shadow;
         CREATE TABLE zero_shadow (id BIGSERIAL PRIMARY KEY, pad TEXT NOT NULL)
             WITH (autovacuum_enabled = false);",
    )
    .await?;
    let dbnode = db::query_text(&main, "SELECT oid::text FROM pg_database WHERE datname = current_database()").await?;
    let relnode = db::query_text(&main, "SELECT pg_relation_filenode('zero_shadow')::text").await?;
    tee_println!("zero_shadow dbnode={dbnode} relnode={relnode}");

    // Outlive the PageWriter's dc_lsn upsert throttle (5s default) so
    // the last persisted checkpoint is guaranteed to lag the rows the
    // flush loop keeps pushing into YB — that lag IS the replay window.
    step("Extend the relation continuously for ~7s");
    let deadline = Instant::now() + Duration::from_secs(7);
    let mut batches = 0u32;
    while Instant::now() < deadline {
        db::batch(
            &main,
            "INSERT INTO zero_shadow (pad)
               SELECT repeat('x', 1000) FROM generate_series(1, 500);",
        )
        .await?;
        batches += 1;
    }
    let nblocks =
        db::query_i64(&main, "SELECT pg_relation_size('zero_shadow') / 8192").await?;
    tee_println!("{batches} insert batches, relation at {nblocks} blocks");
    if nblocks < 50 {
        bail!("workload too small to exercise extension ({nblocks} blocks < 50)");
    }

    // A true SIGKILL, not `stop -m immediate`: the immediate-stop path
    // still runs a shutdown flush + dc_lsn upsert, which closes the
    // replay window and lets the restart skip replay entirely. The
    // production crashes were SIGABRT/OOM-kill — no shutdown handler —
    // so that's what we simulate.
    step("SIGKILL pageserver (checkpoint stays behind YB rows)");
    let pidfile = c.repo_dir().join("endpoints/main/pageserver/pageserver.pid");
    let pid = std::fs::read_to_string(&pidfile)?.trim().to_string();
    let status = std::process::Command::new("kill").args(["-9", &pid]).status()?;
    if !status.success() {
        bail!("kill -9 {pid} failed");
    }
    tokio::time::sleep(Duration::from_secs(1)).await;

    step("Restart pageserver (replays the flushed window)");
    c.yb(&["pageserver", "start"])?;
    tokio::time::sleep(Duration::from_secs(3)).await;

    step("Drive post-restart inserts (flushes any replay-window writes)");
    for _ in 0..4 {
        db::batch(
            &main,
            "INSERT INTO zero_shadow (pad)
               SELECT repeat('x', 1000) FROM generate_series(1, 500);",
        )
        .await?;
    }
    db::batch(&main, "CHECKPOINT;").await?;
    tokio::time::sleep(Duration::from_secs(3)).await;

    step("Assert: no all-zero image above a block's first row (yb_storage)");
    // Branch-wide, not just the test table: index/toast/catalog blocks
    // corrupt the same way (the first repro of bug B was on the pkey
    // index block while a table-scoped assertion passed).
    //
    // A legitimate gap-fill zero page is written when the block first
    // comes into existence, so it is the block's oldest row. Any
    // all-zero image with an older sibling row is shadowing live data.
    let shadowing = db::query_i64(
        &yb,
        &format!(
            "SELECT count(*) FROM pageserver.pages p
             WHERE p.branch_id = '{branch}' AND p.is_image
               AND p.payload = decode(repeat('00', 8192), 'hex')
               AND p.lsn > (SELECT min(q.lsn) FROM pageserver.pages q
                            WHERE q.branch_id = p.branch_id
                              AND q.spcnode = p.spcnode
                              AND q.dbnode = p.dbnode
                              AND q.relnode = p.relnode
                              AND q.forknum = p.forknum
                              AND q.blknum = p.blknum)"
        ),
    )
    .await?;
    let dup_zeros = db::query_i64(
        &yb,
        &format!(
            "SELECT count(*) FROM (
                 SELECT spcnode, dbnode, relnode, forknum, blknum
                 FROM pageserver.pages
                 WHERE branch_id = '{branch}' AND is_image
                   AND payload = decode(repeat('00', 8192), 'hex')
                 GROUP BY 1, 2, 3, 4, 5 HAVING count(*) > 1
             ) d"
        ),
    )
    .await?;
    if shadowing > 0 || dup_zeros > 0 {
        bail!(
            "zero-image shadowing detected: {shadowing} zero image(s) above live rows, \
             {dup_zeros} block(s) with duplicate zero images (#22 regression)"
        );
    }
    tee_println!("  ok — no shadowing zero images, no duplicate zeros");

    step("Verify the data reads back intact through the restarted PS");
    let rows = db::query_i64(&main, "SELECT count(*) FROM zero_shadow").await?;
    let expected = (batches as i64 + 4) * 500;
    if rows != expected {
        bail!("row count mismatch after restart: {rows} != {expected}");
    }
    tee_println!("  ok — {rows} rows intact");

    step("Scan pageserver log for corruption markers (this iteration only)");
    let full_log = std::fs::read_to_string(c.ps_log()).unwrap_or_default();
    let log = full_log.get(log_start..).unwrap_or(full_log.as_str());
    let markers = Regex::new(MARKERS).expect("static regex");
    let hits: Vec<&str> = log.lines().filter(|l| markers.is_match(l)).collect();
    if !hits.is_empty() {
        eprintln!("FAIL — {} corruption marker(s) in pageserver.log", hits.len());
        for l in hits.iter().take(5) {
            eprintln!("{l}");
        }
        bail!("corruption markers in pageserver.log");
    }
    tee_println!("  pageserver log clean");

    Ok(())
}
