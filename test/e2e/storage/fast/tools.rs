//! Smoke-test the operator diagnostic tools end-to-end against the live
//! (no-TLS) cluster: `yb-dump-wals`, `yb-dump-pages` (list + decode), and
//! `yb-reset-consistent-lsn`. This exercises the shared `YbConnArgs`
//! connection path (the no-TLS arm) and each tool's basic behaviour, so a
//! regression in a tool — or in the shared connection handling that used to
//! drift between them — fails CI here instead of only surfacing on a remote
//! TLS cluster during an incident.

use anyhow::bail;

use crate::cluster::{Cluster, step};
use crate::{db, tee_println};

const ENDPOINT_ID: &str = "ep_tools";

pub async fn run(c: &Cluster) -> anyhow::Result<()> {
    // Provision a dedicated branch for this test rather than reusing the
    // shared `main` branch. The fast suite runs branch scenarios first,
    // which leave several (and dropped) branches behind — so addressing this
    // test's data by a fresh, dedicated branch id keeps the lookups below
    // unambiguous: the data we load here is the data we read.
    let tools_port = c.compute_port() + 120;
    step("create a dedicated branch + endpoint for this test");
    let branch = c.branch_create()?;
    if branch.is_empty() {
        bail!("branch create failed");
    }
    c.yb(&[
        "endpoint", "create", ENDPOINT_ID, "--branch-id", &branch, "--branch-name", "main",
        "--pg-version", "17", "--pg-port", &tools_port.to_string(),
    ])?;
    c.yb(&["endpoint", "start", ENDPOINT_ID])?;

    let compute = db::compute_conn(tools_port);
    step("seed a table so pageserver.pages + safekeeper.wals have data");
    db::batch(
        &compute,
        "DROP TABLE IF EXISTS tools_probe;\n\
         CREATE TABLE tools_probe (id int primary key, v text);\n\
         INSERT INTO tools_probe SELECT g, 'r'||g FROM generate_series(1,200) g;\n\
         CHECKPOINT;",
    )
    .await?;

    let yb = db::yb_conn(c.yb_host(), c.yb_port(), c.db_name());

    step("confirm the branch's timeline_state row exists");
    let row = db::query_text(
        &yb,
        &format!("SELECT branch_id FROM pageserver.timeline_state WHERE branch_id='{branch}' LIMIT 1"),
    )
    .await?;
    if row.is_empty() {
        bail!("no timeline_state row for branch {branch}");
    }

    step("yb-dump-wals --summary");
    let wals = c.run_tool("yb-dump-wals", &["--branch", &branch, "--summary"])?;
    if !wals.contains("segno") {
        bail!("yb-dump-wals --summary missing 'segno' header:\n{wals}");
    }

    step("pick an existing (rel, fork, blk) from pageserver.pages");
    // `pageserver.pages` is populated by the asynchronous pageserver→YB
    // flush, which lags the CHECKPOINT above, so the row may not be there
    // the instant we look. Poll until it lands rather than one-shot query.
    let key_sql = format!(
        "SELECT spcnode||' '||dbnode||' '||relnode||' '||forknum||' '||blknum \
         FROM pageserver.pages WHERE branch_id='{branch}' LIMIT 1"
    );
    let deadline = std::time::Instant::now() + std::time::Duration::from_secs(30);
    let key = loop {
        if let Some(k) = db::query_text_opt(&yb, &key_sql).await? {
            break k;
        }
        if std::time::Instant::now() > deadline {
            bail!(
                "no rows in pageserver.pages for branch {branch} \
                 within 30s — the flush never landed"
            );
        }
        tokio::time::sleep(std::time::Duration::from_millis(500)).await;
    };
    let f: Vec<&str> = key.split_whitespace().collect();
    if f.len() != 5 {
        bail!("expected 5 key columns from pageserver.pages, got {key:?}");
    }
    let (spc, dbn, rel, fork, blk) = (f[0], f[1], f[2], f[3], f[4]);

    step("yb-dump-pages (list)");
    let list = c.run_tool(
        "yb-dump-pages",
        &[
            "--branch", &branch, "--spc", spc, "--db", dbn, "--rel", rel,
            "--fork", fork, "--blknum", blk,
        ],
    )?;
    if !list.contains("blknum") || !list.contains("lsn") {
        bail!("yb-dump-pages list missing expected header:\n{list}");
    }

    step("yb-dump-pages --decode (chain decode + gap check)");
    let dec = c.run_tool(
        "yb-dump-pages",
        &[
            "--decode", "--branch", &branch, "--spc", spc, "--db", dbn,
            "--rel", rel, "--fork", fork, "--blknum", blk,
        ],
    )?;
    if !dec.contains("rows for rel") || !(dec.contains("offnum gaps") || dec.contains("GAP")) {
        bail!("yb-dump-pages --decode produced unexpected output:\n{dec}");
    }

    step("yb-reset-consistent-lsn (dry run — read-only)");
    let cur = db::query_text(
        &yb,
        &format!(
            "SELECT to_hex((consistent_lsn>>32)::int8)||'/'||to_hex((consistent_lsn & x'ffffffff'::bigint)::int8) \
             FROM pageserver.timeline_state WHERE branch_id='{branch}'"
        ),
    )
    .await?;
    // Whether it finds a boundary depends on WAL coverage (a tiny fresh
    // cluster may not have a chunk covering the anchor page). What we assert
    // here is that it CONNECTED and reached the WAL-decode stage — i.e. the
    // shared YbConnArgs no-TLS path works for this tool too. A connection
    // failure would carry the `connecting to yb_storage` context instead.
    let (ok, out, err) = c.run_tool_capture(
        "yb-reset-consistent-lsn",
        &["--branch", &branch, "--target-lsn", &cur],
    )?;
    if err.contains("connecting to yb_storage") {
        bail!("yb-reset-consistent-lsn failed to connect:\n{err}");
    }
    let reached_wal = ok || err.contains("WAL") || out.contains("WAL");
    if !reached_wal {
        bail!("yb-reset-consistent-lsn neither succeeded nor reached the WAL stage:\nstdout={out}\nstderr={err}");
    }

    tee_println!(
        "  yb-dump-wals + yb-dump-pages (list & decode) + yb-reset-consistent-lsn \
         all ran against the no-TLS cluster"
    );

    step("cleanup: deprovision this test's endpoint");
    c.yb_quiet(&["endpoint", "stop", ENDPOINT_ID, "--destroy"]);

    Ok(())
}
