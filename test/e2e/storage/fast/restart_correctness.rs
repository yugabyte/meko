//! prev_record_lsn restart fix: write data, gracefully stop the
//! endpoint then the pageserver, confirm `prev_record_lsn` was persisted
//! (non-NULL) in `pageserver.timeline_state`, restart, and verify the
//! data reads back intact. Without the fix the column stays NULL forever
//! (or is read and discarded), leaving the walreceiver to guess `prev`
//! on restart — historically a corruption-class incident.

use anyhow::bail;
use regex::Regex;

use crate::cluster::{Cluster, MARKERS, step};
use crate::tee_println;
use crate::db;

pub async fn run(c: &Cluster) -> anyhow::Result<()> {
    let main = db::compute_conn(c.compute_port());
    let yb = db::yb_conn(c.yb_host(), c.yb_port(), c.db_name());
    let tid = c.tid();

    step("Write a small workload (table + inserts + updates)");
    db::batch(
        &main,
        "DROP TABLE IF EXISTS restart_check;
         CREATE TABLE restart_check (id INT PRIMARY KEY, val TEXT NOT NULL);
         INSERT INTO restart_check (id, val)
           SELECT g, 'row-' || g FROM generate_series(1, 200) g;
         UPDATE restart_check SET val = 'updated-' || id WHERE id <= 50;
         CHECKPOINT;",
    )
    .await?;

    step("Stop endpoint, then pageserver (graceful — should force_upsert pending dc_lsn)");
    c.yb(&["endpoint", "stop", "main"])?;
    c.yb(&["pageserver", "stop"])?;

    step("Confirm prev_record_lsn was persisted (non-NULL after PS shutdown)");
    let prev = db::query_text_opt(
        &yb,
        &format!(
            "SELECT prev_record_lsn::text FROM pageserver.timeline_state WHERE tenant_id = '{tid}'"
        ),
    )
    .await?;
    match prev.as_deref() {
        Some(v) if !v.is_empty() => tee_println!("  prev_record_lsn persisted as: {v}"),
        _ => bail!("prev_record_lsn was not persisted (column is NULL after graceful shutdown)"),
    }

    step("Restart pageserver");
    c.yb(&["pageserver", "start"])?;

    step("Restart endpoint and verify data is intact");
    c.yb(&["endpoint", "start", "main"])?;
    let total = db::query_i64(&main, "SELECT count(*) FROM restart_check").await?;
    let updated =
        db::query_i64(&main, "SELECT count(*) FROM restart_check WHERE val LIKE 'updated-%'").await?;
    if total != 200 || updated != 50 {
        bail!("post-restart counts wrong: total={total} updated={updated} (expected 200 / 50)");
    }
    tee_println!("  ok  (total={total}, updated={updated})");

    step("Scan pageserver log for corruption markers introduced by the restart");
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
