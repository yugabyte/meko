//! Branch data inheritance: a branch — and a branch-of-a-branch — reads
//! its ancestor's committed rows via lineage-aware reads against YB,
//! with nothing copied at branch time. Checks value-level inheritance
//! (exact row + whole-table checksum), multi-level lineage (a grandchild
//! sees data from BOTH ancestors), and ancestor immutability.

use anyhow::bail;

use crate::cluster::{Cluster, step};
use crate::db;

pub async fn run(c: &Cluster) -> anyhow::Result<()> {
    // Two branch endpoints in the compute lane, above `main`.
    let b1_port = c.compute_port() + 158;
    let b2_port = c.compute_port() + 168;
    let main = db::compute_conn(c.compute_port());
    let b1 = db::compute_conn(b1_port);
    let b2 = db::compute_conn(b2_port);

    step("Seed ancestor data on main (known values + whole-table checksum)");
    db::batch(
        &main,
        "DROP TABLE IF EXISTS lineage;
         CREATE TABLE lineage (id INT PRIMARY KEY, payload TEXT NOT NULL);
         INSERT INTO lineage SELECT g, md5(g::text) FROM generate_series(1, 2000) g;
         CHECKPOINT;",
    )
    .await?;
    let anc_sum = db::query_text(
        &main,
        "SELECT md5(string_agg(payload, ',' ORDER BY id)) FROM lineage",
    )
    .await?;
    let anc_42 = db::query_text(&main, "SELECT payload FROM lineage WHERE id = 42").await?;
    if anc_sum.is_empty() {
        bail!("could not compute ancestor checksum");
    }

    step("Branch anc_b1 off main, provision + start its endpoint");
    c.yb(&["branch", "create", "--branch-name", "anc_b1", "--ancestor-branch-name", "main"])?;
    c.yb(&[
        "endpoint", "create", "ep_anc_b1", "--branch-name", "anc_b1",
        "--pg-version", "18", "--pg-port", &b1_port.to_string(),
    ])?;
    c.yb(&["endpoint", "start", "ep_anc_b1"])?;

    step("anc_b1 inherits the ancestor's rows — exact count, value, and checksum");
    let b1_cnt = db::query_i64(&b1, "SELECT count(*) FROM lineage").await?;
    if b1_cnt != 2000 {
        bail!("anc_b1 sees {b1_cnt} ancestor rows, want 2000");
    }
    let b1_42 = db::query_text(&b1, "SELECT payload FROM lineage WHERE id = 42").await?;
    if b1_42 != anc_42 {
        bail!("anc_b1 row 42 payload '{b1_42}' != ancestor '{anc_42}'");
    }
    let b1_sum = db::query_text(
        &b1,
        "SELECT md5(string_agg(payload, ',' ORDER BY id)) FROM lineage",
    )
    .await?;
    if b1_sum != anc_sum {
        bail!("anc_b1 whole-table checksum mismatch vs ancestor");
    }

    step("Write more on anc_b1, then branch anc_b2 off it (two-level lineage)");
    db::batch(
        &b1,
        "INSERT INTO lineage SELECT g, 'b1-' || g FROM generate_series(3001, 3100) g;
         CHECKPOINT;",
    )
    .await?;
    c.yb(&[
        "branch", "create", "--branch-name", "anc_b2",
        "--ancestor-branch-name", "anc_b1",
    ])?;
    c.yb(&[
        "endpoint", "create", "ep_anc_b2", "--branch-name", "anc_b2",
        "--pg-version", "18", "--pg-port", &b2_port.to_string(),
    ])?;
    c.yb(&["endpoint", "start", "ep_anc_b2"])?;

    step("anc_b2 inherits from BOTH ancestors (main's 2000 + anc_b1's 100)");
    let b2_cnt = db::query_i64(&b2, "SELECT count(*) FROM lineage").await?;
    if b2_cnt != 2100 {
        bail!("anc_b2 sees {b2_cnt} rows, want 2100 (2000 from main + 100 from anc_b1)");
    }
    let b2_42 = db::query_text(&b2, "SELECT payload FROM lineage WHERE id = 42").await?;
    if b2_42 != anc_42 {
        bail!("anc_b2 row 42 payload '{b2_42}' != original ancestor '{anc_42}'");
    }
    let b2_b1 = db::query_i64(&b2, "SELECT count(*) FROM lineage WHERE payload LIKE 'b1-%'").await?;
    if b2_b1 != 100 {
        bail!("anc_b2 sees {b2_b1} of anc_b1's rows, want 100");
    }

    step("Ancestors are immutable w.r.t. their descendants' writes");
    let main_cnt = db::query_i64(&main, "SELECT count(*) FROM lineage").await?;
    if main_cnt != 2000 {
        bail!("main (root) count changed to {main_cnt}, want 2000");
    }
    let b1_final = db::query_i64(&b1, "SELECT count(*) FROM lineage").await?;
    if b1_final != 2100 {
        bail!("anc_b1 count {b1_final}, want 2100 (2000 inherited + 100 own)");
    }

    step("Cleanup: deprovision branch endpoints");
    c.yb_quiet(&["endpoint", "stop", "ep_anc_b2", "--destroy"]);
    c.yb_quiet(&["endpoint", "stop", "ep_anc_b1", "--destroy"]);

    Ok(())
}
