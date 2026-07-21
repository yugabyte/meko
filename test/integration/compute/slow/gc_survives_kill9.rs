//! GC state is durable: candidates live in YB (`pending_gc`), not in
//! pageserver memory. On a dedicated branch, SIGKILL its pageserver
//! with a fresh backlog queued, restart it, and the drain must RESUME —
//! the queue drains to ~empty, `pages` reclaims below its pre-crash
//! peak — with reads byte-identical to before the crash and no
//! corruption markers.

use std::time::Duration;

use anyhow::{Context, bail};
use tokio::time::sleep;

use crate::cluster::{Cluster, step, tee_line};
use crate::db;

pub async fn run(c: &Cluster) -> anyhow::Result<()> {
    let branch_pg_port = c.compute_port() + 190;
    let yb = db::yb_conn(c.yb_host(), c.yb_port(), c.db_name());

    step("Dedicated branch + endpoint");
    let b = c.branch_create_named("gc_kill", "main")?;
    let ps_id = c.endpoint_create_on_branch("ep_gc_kill", "gc_kill", branch_pg_port)?;
    let conn = db::compute_conn(branch_pg_port);
    let ps_pid = "endpoints/ep_gc_kill/pageserver/pageserver.pid";
    let pages = format!("SELECT count(*) FROM pageserver.pages WHERE branch_id = '{b}'");
    let queue = format!("SELECT count(*) FROM pageserver.pending_gc WHERE branch_id = '{b}'");
    let id = ps_id.to_string();

    step("Seed + churn to queue a backlog, capture the pre-crash checksum");
    db::batch(
        &conn,
        "DROP TABLE IF EXISTS gc_kill;
         CREATE TABLE gc_kill (id INT PRIMARY KEY, payload TEXT NOT NULL);
         INSERT INTO gc_kill SELECT g, md5(g::text) FROM generate_series(1, 1000) g;",
    )
    .await?;
    for i in 0..20 {
        db::batch(&conn, &format!("UPDATE gc_kill SET payload = md5(payload || '{i}')")).await?;
    }
    db::batch(&conn, "CHECKPOINT").await?;
    let want_sum = db::query_text(
        &conn,
        "SELECT md5(string_agg(payload, ',' ORDER BY id)) FROM gc_kill",
    )
    .await?;
    let peak_pages = db::query_i64(&yb, &pages).await?;

    step(&format!("SIGKILL the branch pageserver (node {ps_id}) mid-drain"));
    kill9(c, ps_pid, "pageserver")?;

    step("Restart the branch pageserver and recycle its compute");
    c.yb_quiet(&["pageserver", "stop", "--id", &id, "-m", "immediate"]);
    c.yb(&["pageserver", "start", "--id", &id])?;
    c.yb_quiet(&["endpoint", "stop", "ep_gc_kill", "--mode", "immediate"]);
    c.yb(&["endpoint", "start", "ep_gc_kill"])?;
    let deadline = std::time::Instant::now() + Duration::from_secs(60);
    loop {
        if db::query_i64(&conn, "SELECT 1::bigint").await.is_ok() {
            break;
        }
        if std::time::Instant::now() > deadline {
            bail!("compute never served reads after pageserver kill/restart");
        }
        sleep(Duration::from_secs(1)).await;
    }

    step("Drain resumes after restart (queue → ~0, pages fall below the pre-crash peak)");
    let mut pages_now = peak_pages;
    let mut q = i64::MAX;
    for _ in 0..60 {
        sleep(Duration::from_secs(1)).await;
        pages_now = db::query_i64(&yb, &pages).await?;
        q = db::query_i64(&yb, &queue).await?;
        if q < 100 && pages_now < peak_pages {
            break;
        }
    }
    if q >= 100 || pages_now >= peak_pages {
        bail!("GC did not resume after restart: queue={q} peak={peak_pages} now={pages_now}");
    }

    step("Reads are byte-identical to before the crash");
    let got_sum = db::query_text(
        &conn,
        "SELECT md5(string_agg(payload, ',' ORDER BY id)) FROM gc_kill",
    )
    .await;
    let _ = c.yb(&["branch", "drop", "--branch-id", &b]);
    c.yb_quiet(&["pageserver", "stop", "--id", &id, "-m", "immediate"]);
    if got_sum? != want_sum {
        bail!("checksum changed across kill/restart+GC");
    }
    Ok(())
}

/// SIGKILL the process whose pidfile is at `rel` under the run dir.
fn kill9(c: &Cluster, rel: &str, what: &str) -> anyhow::Result<()> {
    let pidfile = c.repo_dir().join(rel);
    let contents = std::fs::read_to_string(&pidfile)
        .with_context(|| format!("reading {what} pidfile {}", pidfile.display()))?;
    let pid = contents.lines().next().unwrap_or("").trim();
    if pid.is_empty() {
        bail!("{what} pidfile {} is empty", pidfile.display());
    }
    let status = std::process::Command::new("kill")
        .args(["-9", pid])
        .status()
        .with_context(|| format!("kill -9 {pid} ({what})"))?;
    if !status.success() {
        bail!("kill -9 {pid} ({what}) exited with {status}");
    }
    tee_line(&format!("  killed {what} pid {pid} (SIGKILL)"));
    Ok(())
}
