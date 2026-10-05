//! A SIGTERM to compute_ctl must still give Postgres a *fast* shutdown (#898).
//!
//! This is the supervisor's ordinary stop, and `forward_termination_signal` sends
//! the postmaster SIGINT for it specifically so a shutdown checkpoint is written —
//! what lets a read replica get its running-xacts list without scanning CLOG.
//! With `PR_SET_PDEATHSIG` in place, compute_ctl's own exit is now a SIGQUIT for
//! the postmaster, so returning from the handler the moment the SIGINT is sent
//! escalates that fast shutdown to an immediate one and the checkpoint is never
//! written. `handle_exit_signal` therefore waits for Postgres first.
//!
//! Postgres names both in its log, and `pmdie` only skips the escalation message
//! if it is *already* in immediate shutdown, so the discriminator is exact:
//!   - `received fast shutdown request` must appear — the SIGINT arrived.
//!   - `received immediate shutdown request` must NOT — nothing overtook it.
//!
//! Without the wait the second line appears every time, microseconds after the
//! first.
//!
//! The window read is bounded by the postmaster's death, so a restart the
//! supervisor starts afterwards cannot contribute to it.
//!
//! Own cluster and endpoint: this stops the compute out from under itself, and
//! `prep_for_next_test` only recycles `main`. Destroyed on the way out.

use std::io::{Read, Seek, SeekFrom};
use std::path::Path;
use std::time::{Duration, Instant};

use anyhow::{Context, bail};
use tokio::time::sleep;

use crate::cluster::{Cluster, parent_of, pid_alive, pid_in, signal, step};
use crate::db;
use crate::tee_println;

const CLUSTER: &str = "cc_sigterm";
const ENDPOINT_ID: &str = "ep_cc_sigterm";

/// compute_ctl waits 8 s for the fast shutdown; this only has to outlast that
/// plus the postmaster's own exit.
const SHUTDOWN_TIMEOUT: Duration = Duration::from_secs(30);

/// Read `log` from `offset` to its current end.
fn tail_from(log: &Path, offset: u64) -> anyhow::Result<String> {
    let mut f = std::fs::File::open(log).with_context(|| format!("opening {}", log.display()))?;
    f.seek(SeekFrom::Start(offset))
        .with_context(|| format!("seeking {}", log.display()))?;
    let mut text = String::new();
    f.read_to_string(&mut text)
        .with_context(|| format!("reading {}", log.display()))?;
    Ok(text)
}

pub async fn run(c: &Cluster) -> anyhow::Result<()> {
    let pg_port = c.endpoint_port();

    step("Own cluster + endpoint (this one's compute_ctl is stopped under it)");
    let branch_id = c.cluster_create(CLUSTER)?;
    c.endpoint_create_on_branch(ENDPOINT_ID, &branch_id, pg_port)?;
    let conn = db::compute_conn(pg_port);
    let endpoint_dir = c.repo_dir().join("endpoints").join(ENDPOINT_ID);
    let log = endpoint_dir.join("compute.log");

    step("Write something, so the shutdown has a checkpoint to write");
    db::batch(
        &conn,
        "DROP TABLE IF EXISTS cc_sigterm;
         CREATE TABLE cc_sigterm (id INT PRIMARY KEY);
         INSERT INTO cc_sigterm VALUES (1);",
    )
    .await?;

    let outcome = drains_on_sigterm(&endpoint_dir, &log).await;
    c.yb_quiet(&["endpoint", "stop", ENDPOINT_ID, "--destroy"]);
    c.yb_quiet(&["pageserver", "stop", ENDPOINT_ID, "-m", "immediate"]);
    outcome
}

async fn drains_on_sigterm(endpoint_dir: &Path, log: &Path) -> anyhow::Result<()> {
    let pm = pid_in(&endpoint_dir.join("pgdata/postmaster.pid"))?;
    let cc = parent_of(pm)?;
    if cc <= 1 {
        bail!("postmaster {pm} has ppid {cc}: no compute_ctl to signal");
    }
    // Everything the shutdown logs lands after this point.
    let offset = std::fs::metadata(log)
        .with_context(|| format!("statting {}", log.display()))?
        .len();

    step("SIGTERM compute_ctl — the stop a pod gets");
    signal(cc, "TERM", "compute_ctl")?;

    step("Postgres is allowed to finish the fast shutdown before compute_ctl goes");
    let deadline = Instant::now() + SHUTDOWN_TIMEOUT;
    while pid_alive(pm) {
        if Instant::now() >= deadline {
            bail!("postmaster {pm} still alive {SHUTDOWN_TIMEOUT:?} after compute_ctl SIGTERM");
        }
        sleep(Duration::from_millis(200)).await;
    }
    let tail = tail_from(log, offset)?;

    if !tail.contains("received fast shutdown request") {
        bail!(
            "postgres never logged a fast shutdown after compute_ctl was SIGTERMed: \
             `forward_termination_signal` is the only thing that sends it SIGINT"
        );
    }
    if tail.contains("received immediate shutdown request") {
        bail!(
            "the fast shutdown was escalated to an immediate one: compute_ctl exited before \
             postgres finished, and the kernel's parent-death SIGQUIT overtook it, so no \
             shutdown checkpoint was written (#898)"
        );
    }
    tee_println!("  postmaster {pm} took the fast shutdown, uninterrupted");

    Ok(())
}
