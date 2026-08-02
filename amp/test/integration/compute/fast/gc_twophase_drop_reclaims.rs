//! Two-phase-commit meta drop reclamation: `PREPARE TRANSACTION` writes a
//! two-phase state file — a `meta` key (`twophase_file_key`, field1=0x02)
//! — and `COMMIT PREPARED` resolves it (`drop_twophase_file`), queuing a
//! `gc_kind = drop_meta` candidate. The drain then deletes that key's rows
//! `<= due_lsn`. Asserts the prepared xid's two-phase meta row appears on
//! PREPARE and drains to zero after COMMIT PREPARED + GC. Dedicated branch.

use std::time::Duration;

use anyhow::bail;
use tokio::time::sleep;

use crate::cluster::{Cluster, step};
use crate::db;

pub async fn run(c: &Cluster) -> anyhow::Result<()> {
    let branch_pg_port = c.compute_port() + 280;
    let yb = db::yb_conn(c.yb_host(), c.yb_port(), c.db_name());

    step("Dedicated branch + endpoint");
    let b = c.branch_create_named("gc_tp", c.branch())?;
    // Inlined (not endpoint_create_on_branch) to pass --pg-setting:
    // PREPARE TRANSACTION needs max_prepared_transactions > 0 (compute
    // defaults to 0); without it the PREPARE below errors before GC runs.
    c.yb(&[
        "endpoint",
        "create",
        "ep_gc_tp",
        "--branch-id",
        &b,
        "--pg-version",
        "18",
        "--pg-port",
        &branch_pg_port.to_string(),
        "--pg-setting",
        "max_prepared_transactions=5",
    ])?;
    c.yb(&["endpoint", "start", "ep_gc_tp"])?;
    let conn = db::compute_conn(branch_pg_port);

    step("PREPARE TRANSACTION — writes a two-phase state file to meta");
    db::batch(&conn, "CREATE TABLE gc_tp (id INT PRIMARY KEY)").await?;
    // Explicit block so PREPARE TRANSACTION terminates it (a bare
    // multi-statement batch is an implicit txn, which PREPARE can't end).
    db::batch(
        &conn,
        "BEGIN; INSERT INTO gc_tp VALUES (1); PREPARE TRANSACTION 'gc_tp';",
    )
    .await?;
    db::batch(&conn, "CHECKPOINT").await?;
    // The prepared xid — gone from pg_prepared_xacts after COMMIT PREPARED,
    // so capture it now. On a fresh cluster xid < 2^32, so the two-phase key
    // is field4=0 field5=0 field6=xid (see `twophase_file_key`).
    let xid = db::query_i64(
        &conn,
        "SELECT transaction::text::bigint FROM pg_prepared_xacts WHERE gid = 'gc_tp'",
    )
    .await?;

    // That exact two-phase key's rows in `meta` (field1=0x02 is the
    // two-phase keyspace; the decode's meta branch prints the fields).
    let tp_rows = format!(
        "SELECT count(*) FROM amp_storage.meta \
         WHERE branch_id = '{b}' \
           AND amp_storage.gc_key_decode(key) = 'meta f1=2 f2=0 f3=0 f4=0 f5=0 f6={xid}'"
    );
    // The PREPARE record is ingested asynchronously (compute → WAL →
    // pageserver), so poll for the key rather than racing the ingest.
    let mut present = 0;
    for _ in 0..20 {
        present = db::query_i64(&yb, &tp_rows).await?;
        if present >= 1 {
            break;
        }
        sleep(Duration::from_secs(1)).await;
    }
    if present < 1 {
        bail!("PREPARE TRANSACTION did not land a two-phase key (xid {xid}) in meta");
    }

    step("COMMIT PREPARED, then churn so the cutoff passes the drop");
    db::batch(&conn, "COMMIT PREPARED 'gc_tp'").await?;
    let mut left = i64::MAX;
    for i in 0..40 {
        db::batch(
            &conn,
            &format!(
                "CREATE TABLE IF NOT EXISTS gc_tp_churn (id INT PRIMARY KEY, n INT);
                 INSERT INTO gc_tp_churn VALUES ({i}, {i});
                 CHECKPOINT;"
            ),
        )
        .await?;
        sleep(Duration::from_secs(1)).await;
        left = db::query_i64(&yb, &tp_rows).await?;
        if left == 0 {
            break;
        }
    }

    let _ = c.yb(&["branch", "drop", "--branch-id", &b]);
    c.yb_quiet(&["pageserver", "stop", "ep_gc_tp", "-m", "immediate"]);
    if left != 0 {
        bail!("two-phase meta key (xid {xid}) not reclaimed after COMMIT PREPARED: {left} rows left");
    }
    Ok(())
}
