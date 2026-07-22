//! Reads stay correct while the GC drain advances the cutoff and trims
//! superseded versions underneath them.
//!
//! `race_read` is seeded then churned with 12 in-place UPDATEs to stack
//! deep per-block version chains, then FROZEN (never written again) — so
//! its checksum is a fixed invariant while its old (sub-anchor) versions
//! are trimmed. The cutoff is advanced by writing a *separate*
//! `race_churn` table (which bumps last_record_lsn), so the drain keeps
//! trimming race_read's old versions while the reads run.
//!
//! Cases:
//!   - IN-FLIGHT: concurrent cold readers loop over race_read (each a
//!     fresh frontier getPage, holding the RCU cutoff guard) while the
//!     drain trims. Every read must return the frozen checksum. Had GC
//!     trimmed a version a read still needed, the read would FAIL (its
//!     base/anchor gone) — or, less likely, return a wrong checksum; the
//!     reader asserts both (a query error and a mismatch).
//!   - BELOW-CUTOFF: a fresh getPage AT the old lsn x, now below the
//!     advanced cutoff, is cleanly rejected — its version was trimmed
//!     (base_x proved it was readable before the cutoff moved).
//!   - NEW INCOMING: a fresh frontier read once the drain has reclaimed
//!     still returns the frozen checksum.
//!
//! Between them these cover the drain's wait-for-in-flight-readers path,
//! post-trim rejection below the cutoff, and post-trim correctness.
//! TODO(gc_race_precise): also cover a getPage AT an old lsn x with the
//! cutoff crossing x mid-read (failpoint-paused — the precise RCU
//! necessity). Attempted and reverted: forcing the cross fights the
//! lease/consistent_lsn floor (which pins the cutoff at ~x), so it's
//! flaky; the RCU guard is belt-and-suspenders behind that floor.

use std::sync::Arc;
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::time::Duration;

use anyhow::{Context, bail};
use tokio::time::sleep;
use utils::lsn::Lsn;

use crate::cluster::{Cluster, step};
use crate::{db, page_api, tee_println};

pub async fn run(c: &Cluster) -> anyhow::Result<()> {
    let branch_pg_port = c.compute_port() + 210;
    let yb = db::yb_conn(c.yb_host(), c.yb_port(), c.db_name());

    step("Dedicated branch + endpoint");
    let b = c.branch_create_named("gc_race", "main")?;
    c.endpoint_create_on_branch("ep_gc_race", "gc_race", branch_pg_port)?;
    let conn = db::compute_conn(branch_pg_port);
    let read_pages = format!("SELECT count(*) FROM pageserver.pages WHERE branch_id = '{b}'");

    step("Seed race_read + stack deep version chains, capture the stable checksum");
    db::batch(
        &conn,
        // yb_amp_test_utils provides clear_buffer_cache() for the cold reads below.
        "CREATE EXTENSION IF NOT EXISTS yb_amp_test_utils;
         DROP TABLE IF EXISTS race_read;
         CREATE TABLE race_read (id INT PRIMARY KEY, payload TEXT NOT NULL);
         INSERT INTO race_read SELECT g, md5(g::text) FROM generate_series(1, 1500) g;",
    )
    .await?;
    // Case 1 & 2 anchor: an early durable lsn `x` where race_read block 0
    // is readable. Address the BRANCH endpoint's OWN pageserver (one PS
    // per endpoint) via its gRPC connstring.
    db::batch(&conn, "CHECKPOINT").await?;
    let x_lsn: Lsn = db::query_text(&conn, "SELECT pg_current_wal_lsn()::text")
        .await?
        .trim()
        .parse()
        .context("parse x_lsn")?;
    let dbnode = db::query_i64(
        &conn,
        "SELECT oid::int8 FROM pg_database WHERE datname = current_database()",
    )
    .await? as u32;
    let relnode =
        db::query_i64(&conn, "SELECT pg_relation_filenode('race_read')::int8").await? as u32;
    let branch_hex = db::query_text(&conn, "SHOW yb_amp.branch_id")
        .await?
        .trim()
        .to_string();
    let connstr = db::query_text(&conn, "SHOW yb_amp.pageserver_connstring").await?;
    let branch_client = page_api::client_at_connstring(c, &branch_hex, &connstr)?;
    let rel = page_api::heap_rel(dbnode, relnode);
    // Baseline block 0 @ x (retry while the fresh gRPC attach settles). A
    // later read @ x that's rejected then proves it's the cutoff, not a
    // missing page.
    let mut base_x = None;
    for attempt in 0..15u64 {
        match page_api::get_page(&branch_client, rel, 0, x_lsn, x_lsn, attempt + 1).await {
            Ok(p) => {
                base_x = Some(p);
                break;
            }
            Err(_) => sleep(Duration::from_millis(700)).await,
        }
    }
    let base_x = base_x.context("baseline getPage @ x never succeeded")?;
    anyhow::ensure!(base_x.len() == 8192, "baseline page @ x not 8KiB");
    for i in 0..12 {
        db::batch(&conn, &format!("UPDATE race_read SET payload = md5(payload || '{i}')")).await?;
    }
    db::batch(&conn, "CHECKPOINT").await?;
    // race_read is now FINAL — never written again — so its checksum is
    // a fixed invariant while its sub-anchor versions are trimmed.
    let want_sum = db::query_text(
        &conn,
        "SELECT md5(string_agg(payload, ',' ORDER BY id)) FROM race_read",
    )
    .await?;
    let peak = db::query_i64(&yb, &read_pages).await?;

    // A separate churn table: writing it advances last_record_lsn (and
    // thus the GC cutoff) DURING the read phase, so the drain keeps
    // advancing the cutoff and trimming race_read's old versions while
    // the readers run — that is what makes the reads in-flight over a
    // moving cutoff.
    db::batch(
        &conn,
        "DROP TABLE IF EXISTS race_churn;
         CREATE TABLE race_churn (id INT PRIMARY KEY, payload TEXT NOT NULL);
         INSERT INTO race_churn SELECT g, md5(g::text) FROM generate_series(1, 200) g;",
    )
    .await?;

    step("Concurrent cold readers race the drain — every read must stay byte-correct");
    let stop = Arc::new(AtomicBool::new(false));
    let reads_done = Arc::new(AtomicU64::new(0));
    let mut readers = Vec::new();
    for _ in 0..4 {
        let conn_r = conn.clone();
        let want = want_sum.clone();
        let stop_r = stop.clone();
        let counter = reads_done.clone();
        readers.push(tokio::spawn(async move {
            while !stop_r.load(Ordering::Relaxed) {
                // Cold read: drop the compute buffer cache so the SELECT
                // pulls heap pages via getPage (holding the RCU cutoff
                // guard) — an in-flight read over the store being trimmed.
                db::batch(&conn_r, "SELECT clear_buffer_cache();").await?;
                let got = db::query_text(
                    &conn_r,
                    "SELECT md5(string_agg(payload, ',' ORDER BY id)) FROM race_read",
                )
                .await?;
                if got != want {
                    bail!(
                        "concurrent read saw a wrong checksum — GC trimmed a version a read still needed"
                    );
                }
                counter.fetch_add(1, Ordering::Relaxed);
            }
            Ok::<(), anyhow::Error>(())
        }));
    }

    // Advance the cutoff while the readers run, until the drain reclaims
    // race_read's garbage (pages drop below peak) — i.e. trimming
    // overlaps the in-flight reads.
    let mut reclaimed = false;
    'churn: for i in 0..60 {
        db::batch(&conn, &format!("UPDATE race_churn SET payload = md5(payload || '{i}')")).await?;
        db::batch(&conn, "CHECKPOINT").await?;
        for _ in 0..3 {
            sleep(Duration::from_millis(500)).await;
            if db::query_i64(&yb, &read_pages).await? < peak {
                reclaimed = true;
                break 'churn;
            }
        }
    }
    // Keep the readers racing a little past the reclaim point.
    sleep(Duration::from_secs(2)).await;
    stop.store(true, Ordering::Relaxed);

    let mut reader_err: Option<anyhow::Error> = None;
    for r in readers {
        if let Err(e) = r.await.expect("reader task panicked") {
            reader_err = Some(e);
        }
    }

    step("Case 2: a new getPage @ x — now below the advanced cutoff — is cleanly rejected");
    let case2_rejected = page_api::get_page(&branch_client, rel, 0, x_lsn, x_lsn, 10_000)
        .await
        .is_err();

    step("New incoming read after the drain has trimmed");
    db::batch(&conn, "SELECT clear_buffer_cache();").await?;
    let new_incoming = db::query_text(
        &conn,
        "SELECT md5(string_agg(payload, ',' ORDER BY id)) FROM race_read",
    )
    .await;

    let total_reads = reads_done.load(Ordering::Relaxed);
    let _ = c.yb(&["branch", "drop", "--branch-id", &b]);
    c.yb_quiet(&["pageserver", "stop", "ep_gc_race", "-m", "immediate"]);

    if let Some(e) = reader_err {
        return Err(e);
    }
    if !reclaimed {
        bail!("drain did not reclaim race_read during the read race (peak={peak})");
    }
    if !case2_rejected {
        bail!("getPage @ {x_lsn} is below the cutoff but was not rejected (case 2)");
    }
    if new_incoming? != want_sum {
        bail!("new-incoming cold read after GC saw a wrong checksum");
    }
    tee_println!(
        "  race clean: {total_reads} concurrent cold reads correct while GC reclaimed race_read"
    );
    Ok(())
}
