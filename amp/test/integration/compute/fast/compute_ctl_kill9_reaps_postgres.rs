//! An abrupt compute_ctl death must not leave its postmaster behind (#898).
//!
//! `wait_postgres` reaps the postmaster's process group only on the graceful
//! path. SIGKILL skips it, and before the fix the postmaster kept the client port
//! bound: the replacement compute_ctl lost the bind, spent all five startup
//! attempts on fresh basebackups — which cannot fix a port conflict — and exited,
//! looping until the orphan happened to die. Measured on a k8s endpoint: 20-25 s
//! of downtime in four of five kills, and 530 s in the fifth, which never
//! recovered inside the 300 s observation cap. The fix is `PR_SET_PDEATHSIG`
//! (SIGQUIT) on the postgres spawn.
//!
//! Both assertions are load-bearing:
//!   - the *old* postmaster pid is gone shortly after the kill — the fix itself,
//!     and red without it.
//!   - the endpoint serves again on a *different* pid, so the test cannot pass by
//!     everything staying down.
//!
//! Own cluster and endpoint, destroyed on the way out: the kill recycles this
//! compute, and `prep_for_next_test` only recycles `main`, so anything left
//! serving would hold its port and its daemons against every later scenario.

use std::time::{Duration, Instant};

use anyhow::bail;
use tokio::time::sleep;

use crate::cluster::{Cluster, pid_alive, parent_of, pid_in, signal, step};
use crate::db;
use crate::tee_println;

const CLUSTER: &str = "cc_kill9";
const ENDPOINT_ID: &str = "ep_cc_kill9";

/// The deathsig is delivered by the kernel at once; this only has to cover
/// postgres' own immediate-shutdown path. Generous so a loaded CI box does not
/// flake, still far below the 20 s+ the unfixed orphan survived.
const REAP_TIMEOUT: Duration = Duration::from_secs(15);

/// Restart budget: the supervisor notices, restarts compute_ctl, and compute_ctl
/// takes a fresh basebackup before postgres accepts connections.
const RECOVER_TIMEOUT: Duration = Duration::from_secs(90);

pub async fn run(c: &Cluster) -> anyhow::Result<()> {
    let pg_port = c.endpoint_port();

    step("Own cluster + endpoint (the kill recycles this compute)");
    let branch_id = c.cluster_create(CLUSTER)?;
    c.endpoint_create_on_branch(ENDPOINT_ID, &branch_id, pg_port)?;
    let conn = db::compute_conn(pg_port);
    let pgdata = c
        .repo_dir()
        .join("endpoints")
        .join(ENDPOINT_ID)
        .join("pgdata");

    step("Confirm it serves, and note the pids we are about to disturb");
    db::batch(
        &conn,
        "DROP TABLE IF EXISTS cc_kill9;
         CREATE TABLE cc_kill9 (id INT PRIMARY KEY);
         INSERT INTO cc_kill9 VALUES (1);",
    )
    .await?;
    let pm_before = pid_in(&pgdata.join("postmaster.pid"))?;
    let cc_pid = parent_of(pm_before)?;
    tee_println!("  postmaster pid {pm_before}, compute_ctl pid {cc_pid}");

    // If the postmaster's parent were already init, compute_ctl would have died
    // before we started and the kill below would prove nothing.
    if cc_pid <= 1 {
        bail!("postmaster {pm_before} has ppid {cc_pid}: no compute_ctl to kill");
    }
    if !pid_alive(cc_pid) {
        bail!("compute_ctl {cc_pid} is not running before the kill");
    }

    step("SIGKILL compute_ctl — no handler runs, so only the kernel can reap postgres");
    signal(cc_pid, "9", "compute_ctl")?;

    step("The orphaned postmaster must die on its own (this is the fix)");
    let deadline = Instant::now() + REAP_TIMEOUT;
    while pid_alive(pm_before) {
        if Instant::now() >= deadline {
            bail!(
                "postmaster {pm_before} still alive {:?} after compute_ctl was SIGKILLed: it was \
                 orphaned instead of being signalled, and it is holding port {pg_port} against \
                 the replacement compute_ctl (#898)",
                REAP_TIMEOUT
            );
        }
        sleep(Duration::from_millis(200)).await;
    }
    tee_println!("  postmaster {pm_before} reaped with compute_ctl");

    step("The endpoint comes back, on a new postmaster");
    let deadline = Instant::now() + RECOVER_TIMEOUT;
    loop {
        // Query rather than just probing the port: a fresh basebackup has to
        // land before this row is readable again.
        if let Ok(n) = db::query_i64(&conn, "SELECT count(*) FROM cc_kill9").await {
            if n == 1 {
                break;
            }
        }
        if Instant::now() >= deadline {
            bail!("endpoint did not serve again within {RECOVER_TIMEOUT:?} of the kill");
        }
        sleep(Duration::from_secs(1)).await;
    }

    let pm_after = pid_in(&pgdata.join("postmaster.pid"))?;
    if pm_after == pm_before {
        bail!("postmaster pid {pm_after} is unchanged: the compute never actually restarted");
    }
    tee_println!("  serving again on postmaster pid {pm_after}");

    // Give the port and the co-located daemons back: this scenario ends with its
    // endpoint deliberately up, so nothing else would.
    c.yb_quiet(&["endpoint", "stop", ENDPOINT_ID, "--destroy"]);
    c.yb_quiet(&["pageserver", "stop", ENDPOINT_ID, "-m", "immediate"]);

    Ok(())
}
