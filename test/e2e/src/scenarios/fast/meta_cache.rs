//! Meta-cache correctness: verify that reads of metadata keys (reldir,
//! relsize, aux-file keys) return the right data throughout a workload
//! and survive a pageserver restart (cold cache → YB fallback path).
//!
//! The scenario is read-heavy on metadata: it creates many tables
//! (one reldir + relsize entry each), queries them repeatedly (each
//! GetPage triggers relsize lookups), then restarts the pageserver so
//! the in-process LRU is cold and every meta read must come from YB.

use anyhow::bail;
use regex::Regex;

use crate::cluster::{Cluster, MARKERS, step};
use crate::db;
use crate::tee_println;

const N_TABLES: usize = 30;

pub async fn run(c: &Cluster) -> anyhow::Result<()> {
    let main = db::compute_conn(c.compute_port());

    step("Create tables and populate with data (exercises meta write path)");
    // Build a single batch statement: CREATE + INSERT for each table.
    let mut ddl = String::from("SET client_min_messages = WARNING;\n");
    for i in 0..N_TABLES {
        ddl.push_str(&format!(
            "DROP TABLE IF EXISTS mc_t{i};\n\
             CREATE TABLE mc_t{i} (id INT PRIMARY KEY, v TEXT NOT NULL);\n\
             INSERT INTO mc_t{i} SELECT g, 'row-'||g FROM generate_series(1,50) g;\n"
        ));
    }
    ddl.push_str("CHECKPOINT;");
    db::batch(&main, &ddl).await?;

    step("Read from every table (exercises meta read / cache-hit path)");
    for i in 0..N_TABLES {
        let n = db::query_i64(&main, &format!("SELECT count(*) FROM mc_t{i}")).await?;
        if n != 50 {
            bail!("mc_t{i}: expected 50 rows before restart, got {n}");
        }
    }
    tee_println!("  {N_TABLES} tables × 50 rows verified before restart");

    step("Stop pageserver (flushes in-flight WAL, then cold-restarts the cache)");
    c.yb(&["endpoint", "stop", "main"])?;
    c.yb(&["pageserver", "stop"])?;
    c.yb(&["pageserver", "start"])?;
    c.yb(&["endpoint", "start", "main"])?;

    step("Re-read every table with a cold meta cache (exercises YB fallback path)");
    for i in 0..N_TABLES {
        let n = db::query_i64(&main, &format!("SELECT count(*) FROM mc_t{i}")).await?;
        if n != 50 {
            bail!("mc_t{i}: expected 50 rows after restart, got {n}");
        }
    }
    tee_println!("  {N_TABLES} tables × 50 rows verified after restart");

    step("Update and re-read (exercises cache overwrite / recency path)");
    for i in 0..N_TABLES {
        db::batch(
            &main,
            &format!("UPDATE mc_t{i} SET v = 'updated' WHERE id <= 10;"),
        )
        .await?;
    }
    for i in 0..N_TABLES {
        let updated =
            db::query_i64(&main, &format!("SELECT count(*) FROM mc_t{i} WHERE v = 'updated'"))
                .await?;
        if updated != 10 {
            bail!("mc_t{i}: expected 10 updated rows, got {updated}");
        }
    }
    tee_println!("  {N_TABLES} tables × 10 updated rows verified");

    step("Scan pageserver log for corruption markers");
    let markers = Regex::new(MARKERS).expect("static regex");
    let log = std::fs::read_to_string(c.ps_log()).unwrap_or_default();
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
