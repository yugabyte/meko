//! compute_ctl must take its data directory back from a postmaster that is still
//! running in it, instead of rebuilding it underneath one (#898).
//!
//! `PR_SET_PDEATHSIG` (see `compute_ctl_kill9_reaps_postgres`) stops the orphan
//! being created; the reclaim is the defence behind it, and also covers something
//! with no orphan in it at all — `create_pgdata` deleting the files under a live
//! postmaster, which frees neither the port it holds nor the pidfile that
//! identifies it, leaving the retry loop to spend five attempts on basebackups
//! that cannot fix a port conflict.
//!
//! Two things about the setup, both of which cost a red run to learn:
//!
//! It does not go through `yb_amp_local endpoint start`, which wipes pgdata itself
//! before spawning the compute (`startCompute`) — the pidfile would be gone before
//! compute_ctl could read it. Instead it takes the production path: SIGKILL
//! compute_ctl and let the *supervisor* restart it, as a pod does.
//!
//! And the supervisor is SIGSTOPped across the kill. Left running, it has the
//! replacement compute_ctl through `create_pgdata` about half a second before the
//! SIGQUITed postmaster has finished dying, so the state under test does not exist
//! yet when the code that looks for it looks. Thawed as soon as the stand-in is in
//! place, well inside the 15 s write-lease TTL the supervisor renews.
//!
//! The stand-in is deliberately not a postgres: the deathsig is what makes a real
//! orphan unproducible, and a second postmaster on a live branch would be a second
//! walproposer — a different fault. compute_ctl acts on the state, and the state
//! is three facts: a live process, its cwd in the data directory (`ChangeToDataDir`
//! in `postmaster.c`), and the port bound.
//!
//! Each assertion is load-bearing: the endpoint serves again (without the reclaim
//! it never does); the stand-in went on SIGQUIT (our own child, so the status is
//! exact — still running would mean pgdata was rebuilt under a live owner); and
//! the seeded row reads back on a new pid, so a free port alone is not enough.
//!
//! Own cluster and endpoint, destroyed on the way out: `prep_for_next_test` only
//! recycles `main`.
use std::io::{BufRead, BufReader};
use std::path::Path;
use std::process::{Child, Command, Stdio};
use std::time::{Duration, Instant};

use anyhow::{Context, bail};
use tokio::time::sleep;

use crate::cluster::{Cluster, parent_of, pid_in, signal, step};
use crate::db;
use crate::tee_println;

const CLUSTER: &str = "cc_orphan";
const ENDPOINT_ID: &str = "ep_cc_orphan";

/// Postgres' immediate-shutdown signal, and the one the reclaim sends first.
const SIGQUIT: i32 = 3;

/// How long the stand-in keeps trying for the port. It only has to cover the
/// kernel delivering the deathsig and postgres' immediate shutdown.
const GRAB_TIMEOUT_SECS: u32 = 60;

/// Restart budget: the supervisor notices compute_ctl is gone, restarts it, and
/// compute_ctl reclaims the port and takes a fresh basebackup before postgres
/// accepts connections.
const RECOVER_TIMEOUT: Duration = Duration::from_secs(90);

/// A process that owns `pgdata` the way a postmaster does — cwd in the data
/// directory — and grabs the client port the moment it comes free. Spawned
/// *before* the kill and polling, so the port is never free long enough for the
/// replacement compute to take it.
///
/// The wildcard bind is not laziness about `listen_addresses`: on Linux wildcard
/// and specific binds of one port collide either way round.
fn spawn_stand_in(pgdata: &Path, port: u16) -> anyhow::Result<Child> {
    let grab = "\
import signal, socket, sys, time

# An inherited SIG_IGN would outlive the reclaim's SIGQUIT; a postmaster resets it.
signal.signal(signal.SIGQUIT, signal.SIG_DFL)
port, deadline = int(sys.argv[1]), time.time() + int(sys.argv[2])
while True:
    s = socket.socket()
    s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    try:
        s.bind(('0.0.0.0', port))
        s.listen(1)
        break
    except OSError:
        s.close()
        if time.time() > deadline:
            print('timeout', flush=True)
            sys.exit(1)
        time.sleep(0.02)
print('bound', flush=True)
time.sleep(600)
";
    Command::new("python3")
        .args([
            "-c",
            grab,
            &port.to_string(),
            &GRAB_TIMEOUT_SECS.to_string(),
        ])
        .current_dir(pgdata)
        .stdout(Stdio::piped())
        .spawn()
        .context("spawning the stand-in postmaster")
}

/// Block until the stand-in has the port, then make it the owner of record.
fn claim_pgdata(stand_in: &mut Child, pgdata: &Path) -> anyhow::Result<()> {
    let stdout = stand_in.stdout.take().expect("stdout is piped");
    let mut line = String::new();
    BufReader::new(stdout)
        .read_line(&mut line)
        .context("reading the stand-in's ready line")?;
    if line.trim() != "bound" {
        bail!("the stand-in never got the port (it said {line:?})");
    }

    // The pidfile postgres would have left, overwriting the killed compute's,
    // which names a pid that is now gone. Only the first line matters.
    std::fs::write(
        pgdata.join("postmaster.pid"),
        format!("{}\n{}\n", stand_in.id(), pgdata.display()),
    )
    .context("writing the stand-in's postmaster.pid")
}

pub async fn run(c: &Cluster) -> anyhow::Result<()> {
    // Offsets are unique across the fast suite: an endpoint that outlives its
    // scenario would take the port with it.
    let pg_port = c.compute_port() + 125;

    step("Own cluster + endpoint (this one gets killed and squatted on)");
    let branch_id = c.cluster_create(CLUSTER)?;
    c.endpoint_create_on_branch(ENDPOINT_ID, &branch_id, pg_port)?;
    let conn = db::compute_conn(pg_port);
    let pgdata = c
        .repo_dir()
        .join("endpoints")
        .join(ENDPOINT_ID)
        .join("pgdata");

    step("Seed a row, so the reclaimed compute has to serve real data");
    db::batch(
        &conn,
        "DROP TABLE IF EXISTS cc_orphan;
         CREATE TABLE cc_orphan (id INT PRIMARY KEY);
         INSERT INTO cc_orphan VALUES (1);",
    )
    .await?;

    let endpoint_dir = c.repo_dir().join("endpoints").join(ENDPOINT_ID);
    let sup_pid = pid_in(&endpoint_dir.join("endpoint_supervisor.pid"))?;
    let mut stand_in = spawn_stand_in(&pgdata, pg_port)?;
    let outcome = reclaim_and_serve(&conn, &pgdata, &mut stand_in, pg_port, sup_pid).await;

    // On every path: the stand-in must not outlive the scenario (it holds a
    // port), and the supervisor must not be left frozen (its write lease would
    // lapse and nothing would restart anything).
    let _ = signal(sup_pid, "CONT", "the supervisor");
    let _ = stand_in.kill();
    let _ = stand_in.wait();
    c.yb_quiet(&["endpoint", "stop", ENDPOINT_ID, "--destroy"]);
    c.yb_quiet(&["pageserver", "stop", ENDPOINT_ID, "-m", "immediate"]);
    outcome
}

/// The kill, the squat and the assertions, split out so the stand-in is cleaned
/// up on every path.
async fn reclaim_and_serve(
    conn: &str,
    pgdata: &Path,
    stand_in: &mut Child,
    pg_port: u16,
    sup_pid: i32,
) -> anyhow::Result<()> {
    let stand_in_pid = stand_in.id() as i32;

    step("Freeze the supervisor, so the replacement compute_ctl waits for the setup");
    signal(sup_pid, "STOP", "the supervisor")?;

    step("SIGKILL compute_ctl — the kernel takes its postmaster down with it");
    let pm_before = pid_in(&pgdata.join("postmaster.pid"))?;
    let cc_pid = parent_of(pm_before)?;
    if cc_pid <= 1 {
        bail!("postmaster {pm_before} has ppid {cc_pid}: no compute_ctl to kill");
    }
    signal(cc_pid, "9", "compute_ctl")?;

    step("The stand-in takes the freed port and the data directory with it");
    claim_pgdata(stand_in, pgdata)?;
    tee_println!("  stand-in pid {stand_in_pid} owns pgdata and holds port {pg_port}");

    step("Thaw the supervisor: its replacement compute_ctl must reclaim both");
    signal(sup_pid, "CONT", "the supervisor")?;
    let deadline = Instant::now() + RECOVER_TIMEOUT;
    loop {
        // Query, not a port probe: the stand-in accepts a TCP connect and never
        // speaks postgres, and the basebackup has to land before this row reads.
        if let Ok(1) = db::query_i64(conn, "SELECT count(*) FROM cc_orphan").await {
            break;
        }
        if Instant::now() >= deadline {
            bail!(
                "the endpoint did not serve again within {RECOVER_TIMEOUT:?}: compute_ctl \
                 could not take port {pg_port} back from the process owning its pgdata (#898)"
            );
        }
        sleep(Duration::from_secs(1)).await;
    }

    step("The owner was shut down, and asked rather than killed outright");
    let status = stand_in
        .try_wait()
        .context("waiting on the stand-in")?
        .context("the stand-in is still running: compute_ctl rebuilt pgdata under a live owner")?;
    // Our own child, so this is exact.
    match std::os::unix::process::ExitStatusExt::signal(&status) {
        Some(SIGQUIT) => {}
        other => bail!("the stand-in went on signal {other:?}, want SIGQUIT ({SIGQUIT})"),
    }

    let pm_after = pid_in(&pgdata.join("postmaster.pid"))?;
    if pm_after == stand_in_pid || pm_after == pm_before {
        bail!("postmaster.pid names {pm_after}: no compute of its own ever started");
    }
    tee_println!("  serving again on postmaster pid {pm_after}");

    Ok(())
}
