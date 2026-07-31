//! Ported from Neon's test_createuser (old_neon_test_runner/regress/test_createuser.py).
//! A role created on `main` must be visible on a branch taken from it — i.e. the
//! shared catalog (`pg_authid`) and its relmapper change propagate across the
//! branch point. Create the role on `main`, checkpoint, branch off `main`, then
//! connect to the branch's endpoint *as that role*.

use anyhow::bail;

use crate::cluster::{Cluster, step};
use crate::db;

pub async fn run(c: &Cluster) -> anyhow::Result<()> {
    let main = db::compute_conn(c.compute_port());
    let b_port = c.compute_port() + 178;
    // Trust auth on the compute, so connecting *as* the role needs no password.
    let as_user = format!("host=127.0.0.1 port={b_port} user=integration_createuser dbname=postgres");

    step("Create a role on main (a shared-catalog / relmapper change) + checkpoint");
    db::batch(
        &main,
        "DROP ROLE IF EXISTS integration_createuser;
         CREATE USER integration_createuser PASSWORD 'integration_pw';
         CHECKPOINT;",
    )
    .await?;

    step("Branch off main (captures the new role); provision + start its endpoint");
    c.yb(&["branch", "create", "--branch-name", "cu_branch", "--ancestor-branch-name", "main"])?;
    c.yb(&[
        "endpoint", "create", "ep_cu_branch", "--branch-name", "cu_branch",
        "--pg-version", "18", "--pg-port", &b_port.to_string(),
    ])?;
    c.yb(&["endpoint", "start", "ep_cu_branch"])?;

    step("Connect to the branch endpoint AS the new role — it must exist there");
    let who = db::query_text(&as_user, "SELECT current_user").await?;
    if who != "integration_createuser" {
        bail!("connected as '{who}', want 'integration_createuser' (role did not propagate to the branch)");
    }

    step("Cleanup: deprovision the branch endpoint + drop the role on main");
    c.yb_quiet(&["endpoint", "stop", "ep_cu_branch", "--destroy"]);
    db::batch(&main, "DROP ROLE IF EXISTS integration_createuser;").await?;

    Ok(())
}
