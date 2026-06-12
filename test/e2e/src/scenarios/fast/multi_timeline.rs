//! Two independent timelines (each its own tenant + co-located PS+SK
//! pair) coexisting in the same shared YB db: storage isolation (a
//! same-named table on each holds only that timeline's rows) and
//! independent lifecycle (dropping one leaves the other intact and
//! serving). Doubles as coverage for concurrent pending Bootstrap inits
//! across tenants (task 29).

use anyhow::bail;

use crate::cluster::{Cluster, step};
use crate::tee_println;
use crate::db;

const A_PORT: u16 = 55570;
const B_PORT: u16 = 55580;

pub async fn run(c: &Cluster) -> anyhow::Result<()> {
    step("Create two independent timelines (tenant A, tenant B)");
    let ta = c.tenant_create()?;
    if ta.is_empty() {
        bail!("tenant A create failed");
    }
    let tb = c.tenant_create()?;
    if tb.is_empty() {
        bail!("tenant B create failed");
    }
    tee_println!("tenant A: {ta}");
    tee_println!("tenant B: {tb}");
    if ta == tb {
        bail!("two tenant creates returned the same id");
    }

    step("Provision + start an endpoint on each (one co-located PS+SK pair per timeline)");
    c.yb(&[
        "endpoint", "create", "ep_a", "--tenant-id", &ta, "--branch-name", "main",
        "--pg-version", "17", "--pg-port", &A_PORT.to_string(),
    ])?;
    c.yb(&["endpoint", "start", "ep_a"])?;
    c.yb(&[
        "endpoint", "create", "ep_b", "--tenant-id", &tb, "--branch-name", "main",
        "--pg-version", "17", "--pg-port", &B_PORT.to_string(),
    ])?;
    c.yb(&["endpoint", "start", "ep_b"])?;

    let a = db::compute_conn(A_PORT);
    let b = db::compute_conn(B_PORT);

    step("Write distinct data into the same-named table on each timeline");
    db::batch(
        &a,
        "DROP TABLE IF EXISTS island;
         CREATE TABLE island (id INT PRIMARY KEY, owner TEXT NOT NULL);
         INSERT INTO island SELECT g, 'A' FROM generate_series(1, 500) g;
         CHECKPOINT;",
    )
    .await?;
    db::batch(
        &b,
        "DROP TABLE IF EXISTS island;
         CREATE TABLE island (id INT PRIMARY KEY, owner TEXT NOT NULL);
         INSERT INTO island SELECT g, 'B' FROM generate_series(1, 700) g;
         CHECKPOINT;",
    )
    .await?;

    step("Each timeline sees only its own rows");
    let a_cnt = db::query_i64(&a, "SELECT count(*) FROM island").await?;
    if a_cnt != 500 {
        bail!("timeline A count {a_cnt}, want 500");
    }
    let a_foreign = db::query_i64(&a, "SELECT count(*) FROM island WHERE owner <> 'A'").await?;
    if a_foreign != 0 {
        bail!("timeline A sees {a_foreign} non-A rows, want 0");
    }
    let b_cnt = db::query_i64(&b, "SELECT count(*) FROM island").await?;
    if b_cnt != 700 {
        bail!("timeline B count {b_cnt}, want 700");
    }
    let b_foreign = db::query_i64(&b, "SELECT count(*) FROM island WHERE owner <> 'B'").await?;
    if b_foreign != 0 {
        bail!("timeline B sees {b_foreign} non-B rows, want 0");
    }

    step("Drop timeline A (deprovision its endpoint + co-located PS+SK pair)");
    c.yb(&["endpoint", "stop", "ep_a", "--destroy"])?;
    if c.repo_dir().join("endpoints/ep_a").exists() {
        bail!("endpoints/ep_a should be gone after --destroy");
    }

    step("Timeline B is unaffected by dropping A");
    let b_after = db::query_i64(&b, "SELECT count(*) FROM island").await?;
    if b_after != 700 {
        bail!("timeline B count {b_after} after dropping A, want 700");
    }
    let b_after_own = db::query_i64(&b, "SELECT count(*) FROM island WHERE owner = 'B'").await?;
    if b_after_own != 700 {
        bail!("timeline B owns {b_after_own} after dropping A, want 700");
    }

    step("Cleanup: deprovision timeline B");
    c.yb_quiet(&["endpoint", "stop", "ep_b", "--destroy"]);

    Ok(())
}
