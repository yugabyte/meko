//! Reproducer / regression guard for the TPC-C safekeeper wedge
//! (findings: tpcc-safekeeper-wedge-2026-07-06).
//!
//! Under sustained write load, `safekeeper.wals` tablet splits ("Tablet
//! not running (CREATING)") plus walproposer reconnect churn wedged the
//! safekeeper permanently: the timeline manager stuck in
//! `state_snapshot` on the shared_state RwLock, residence-guard
//! timeouts on every new WAL receiver, `flush_lsn` frozen, every commit
//! hung on SyncRep.
//!
//! This scenario recreates the trigger deliberately: a WAL-heavy insert
//! loop, manual tablet splits via yb-admin (best-effort; skipped loudly
//! when the binary isn't available), and periodic immediate compute
//! restarts to force vote/elected handshakes under load. A probe
//! commit must land within [`PROBE_TIMEOUT`] after every cycle; if it
//! doesn't, we SIGUSR2 the safekeeper (full multi-runtime task dump
//! into its log), collect the wedge signature, and fail with the
//! evidence.

use std::path::PathBuf;
use std::process::Command;
use std::time::Duration;

use anyhow::{Context, bail};

use crate::cluster::{Cluster, step};
use crate::tee_println;
use crate::db;

const CYCLES: usize = 10;
const PROBE_TIMEOUT: Duration = Duration::from_secs(90);

pub async fn run(c: &Cluster) -> anyhow::Result<()> {
    let conninfo = db::compute_conn(c.compute_port());

    step("Seed WAL-heavy load table");
    tokio::time::timeout(
        PROBE_TIMEOUT,
        db::batch(
            &conninfo,
            "DROP TABLE IF EXISTS wedge_load; \
             CREATE TABLE wedge_load (a BIGINT, b TEXT);",
        ),
    )
    .await
    .map_err(|_| wedge_evidence(c, 0))??;

    let admin = YbAdmin::discover(c).await;
    match &admin {
        Some(a) => tee_println!(
            "yb-admin: {} (wals tableid {})",
            a.bin.display(),
            a.table_id
        ),
        None => tee_println!(
            "WARNING: yb-admin unavailable (set YB_ADMIN_BIN) — running churn without \
             tablet splits; the split half of the trigger is NOT exercised"
        ),
    }

    for cycle in 0..CYCLES {
        step(&format!(
            "Cycle {}/{CYCLES}: split + write burst + churn + probe",
            cycle + 1
        ));

        // Kick tablet splits while the burst runs (real trigger: the WAL
        // table splitting under write volume).
        if let Some(a) = &admin {
            a.flush_and_split();
        }

        // WAL-heavy burst: ~10 x 2000 rows x ~400B. Each statement is
        // under the watchdog too — the original wedge froze the FIRST
        // commit of the run, long before the end-of-cycle probe.
        for _ in 0..10 {
            tokio::time::timeout(
                PROBE_TIMEOUT,
                db::batch(
                    &conninfo,
                    "INSERT INTO wedge_load SELECT g, repeat('x', 400) \
                     FROM generate_series(1, 2000) g",
                ),
            )
            .await
            .map_err(|_| wedge_evidence(c, cycle))?
            .context("burst insert")?;
        }

        // Reconnect churn every other cycle: an immediate compute restart
        // forces a fresh walproposer (vote + elected handshake) while the
        // safekeeper's consumer may still be mid-flush from the burst.
        if cycle % 2 == 1 {
            c.yb(&["endpoint", "stop", "main", "--mode", "immediate"])?;
            c.yb(&["endpoint", "start", "main"])?;
        }

        // Watchdog probe: a commit must land within PROBE_TIMEOUT. On the
        // wedge, walproposer reconnects forever and every commit hangs.
        let probe = tokio::time::timeout(
            PROBE_TIMEOUT,
            db::batch(&conninfo, "INSERT INTO wedge_load VALUES (-1, 'probe')"),
        )
        .await;
        match probe {
            Ok(Ok(())) => {}
            Ok(Err(e)) => {
                // Transient connect errors right after a restart are fine;
                // give it one more try inside the same budget.
                tee_println!("probe error ({e:#}); retrying once");
                tokio::time::timeout(
                    PROBE_TIMEOUT,
                    db::batch(&conninfo, "INSERT INTO wedge_load VALUES (-1, 'probe')"),
                )
                .await
                .map_err(|_| wedge_evidence(c, cycle))?
                .context("probe insert (retry)")?;
            }
            Err(_) => return Err(wedge_evidence(c, cycle)),
        }
    }

    step("Final integrity check");
    let rows = db::query_i64(&conninfo, "SELECT count(*) FROM wedge_load").await?;
    if rows < (CYCLES as i64) * 10 * 2000 {
        bail!("lost rows: expected >= {}, got {rows}", CYCLES * 10 * 2000);
    }
    db::batch(&conninfo, "DROP TABLE wedge_load").await?;
    Ok(())
}

/// Commit stalled past the watchdog: capture the safekeeper's task dump
/// (SIGUSR2 — all runtimes) and the wedge signature from its log, and
/// return the evidence as the test failure.
fn wedge_evidence(c: &Cluster, cycle: usize) -> anyhow::Error {
    let sk_dir = c.repo_dir().join("endpoints/main/safekeeper");
    let pid = std::fs::read_to_string(sk_dir.join("safekeeper.pid"))
        .ok()
        .and_then(|s| s.trim().parse::<i32>().ok());
    if let Some(pid) = pid {
        let _ = Command::new("kill").args(["-USR2", &pid.to_string()]).status();
        std::thread::sleep(Duration::from_secs(3));
    }
    let log = newest_log(&sk_dir);
    let mut evidence = String::new();
    if let Some(log) = log {
        if let Ok(text) = std::fs::read_to_string(&log) {
            let guard_timeouts = text.matches("timeout acquiring in wal_residence_guard").count();
            evidence.push_str(&format!(
                "sk log {}: {} residence-guard timeouts\n",
                log.display(),
                guard_timeouts
            ));
            if let Some(idx) = text.rfind("TOKIO TASK DUMP (SIGUSR2) BEGIN") {
                evidence.push_str("--- task dump (all runtimes) ---\n");
                evidence.push_str(&text[idx..]);
            } else {
                // Fall back to the last log lines.
                let tail: Vec<&str> = text.lines().rev().take(40).collect();
                for line in tail.iter().rev() {
                    evidence.push_str(line);
                    evidence.push('\n');
                }
            }
        }
    }
    anyhow::anyhow!(
        "SAFEKEEPER WEDGE: probe commit did not land within {PROBE_TIMEOUT:?} \
         (cycle {}). {evidence}",
        cycle + 1
    )
}

fn newest_log(dir: &std::path::Path) -> Option<PathBuf> {
    let mut logs: Vec<(std::time::SystemTime, PathBuf)> = std::fs::read_dir(dir)
        .ok()?
        .flatten()
        .filter(|e| e.path().extension().is_some_and(|x| x == "log"))
        .filter_map(|e| Some((e.metadata().ok()?.modified().ok()?, e.path())))
        .collect();
    logs.sort();
    logs.pop().map(|(_, p)| p)
}

/// Best-effort yb-admin driver for splitting the run's `safekeeper.wals`
/// tablets.
struct YbAdmin {
    bin: PathBuf,
    masters: String,
    table_id: String,
}

impl YbAdmin {
    async fn discover(c: &Cluster) -> Option<YbAdmin> {
        let bin = std::env::var("YB_ADMIN_BIN")
            .map(PathBuf::from)
            .ok()
            .filter(|p| p.exists())
            .or_else(which_yb_admin)?;
        let masters = std::env::var("YB_MASTER_ADDRS")
            .ok()
            .filter(|v| !v.is_empty())
            .unwrap_or_else(|| format!("{}:7100", c.yb_host()));

        // DocDB table id of this run's safekeeper.wals (GetPgsqlTableId
        // encoding, plain 32-hex — yb-admin's `tableid.` selector).
        let yb_conn = db::yb_conn(c.yb_host(), c.yb_port(), c.db_name());
        let db_oid = db::query_i64(
            &yb_conn,
            "SELECT oid::bigint FROM pg_database WHERE datname = current_database()",
        )
        .await
        .ok()?;
        let table_oid = db::query_i64(
            &yb_conn,
            "SELECT c.oid::bigint FROM pg_class c \
             JOIN pg_namespace n ON n.oid = c.relnamespace \
             WHERE n.nspname = 'safekeeper' AND c.relname = 'wals'",
        )
        .await
        .ok()?;
        let table_id = format!(
            "{:08x}000030008000{:012x}",
            db_oid as u32, table_oid as u32
        );
        Some(YbAdmin {
            bin,
            masters,
            table_id,
        })
    }

    /// Flush + split every current tablet of the wals table. Best-effort:
    /// split requests can fail transiently (already splitting, too small);
    /// the point is sustained split churn, not per-call success.
    fn flush_and_split(&self) {
        let sel = format!("tableid.{}", self.table_id);
        let _ = self.admin(&["flush_table", &sel, "10"]);
        let Some(out) = self.admin(&["list_tablets", &sel, "0"]) else {
            return;
        };
        let mut kicked = 0;
        for line in out.lines().skip(1) {
            let Some(tablet_id) = line.split_whitespace().next() else {
                continue;
            };
            if tablet_id.len() == 32 && self.admin(&["split_tablet", tablet_id]).is_some() {
                kicked += 1;
            }
        }
        tee_println!("kicked {kicked} tablet split(s) on safekeeper.wals");
    }

    fn admin(&self, args: &[&str]) -> Option<String> {
        let out = Command::new(&self.bin)
            .arg("-master_addresses")
            .arg(&self.masters)
            .args(args)
            .output()
            .ok()?;
        out.status
            .success()
            .then(|| String::from_utf8_lossy(&out.stdout).into_owned())
    }
}

fn which_yb_admin() -> Option<PathBuf> {
    let out = Command::new("which").arg("yb-admin").output().ok()?;
    if out.status.success() {
        let p = PathBuf::from(String::from_utf8_lossy(&out.stdout).trim().to_string());
        if p.exists() {
            return Some(p);
        }
    }
    None
}
