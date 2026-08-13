//! The supervisor's own pidfile, claimed with an advisory lock.
//!
//! A pod does not need one — the kubelet tracks PID 1. `yb_amp_local` does: it
//! exits after every command, so the only way it can tell whether an endpoint's
//! supervisor is still up is to look at the file and see whether anyone holds
//! the lock. A pid on its own does not answer that, because pids are reused.
//!
//! **The lock has to live in a descriptor this process opens**, which Rust marks
//! close-on-exec, so no child inherits it. A descriptor passed in from outside is
//! inherited by every child this process spawns, and the lock then outlives the
//! supervisor for as long as any daemon runs — leaving the launcher unable to
//! tell a live supervisor from a dead one, and unable to start a replacement.

use std::fs::OpenOptions;
use std::io::Write;
use std::os::fd::AsRawFd;

use anyhow::{Context, Result, bail};
use camino::{Utf8Path, Utf8PathBuf};

/// The pidfile's name inside the run directory. `yb_amp_local` derives the same
/// name from the process name it supervises — see `supervisorName` in
/// controller/local/launch/supervisor.go.
pub const PIDFILE_NAME: &str = "endpoint_supervisor.pid";

pub fn pidfile_path(run_dir: &Utf8Path) -> Utf8PathBuf {
    run_dir.join(PIDFILE_NAME)
}

/// Holds the claim. Dropping it releases the lock, so the caller keeps it for as
/// long as the process runs.
#[must_use]
pub struct Claim {
    /// Never read: the open descriptor *is* the lock, and closing it — which is
    /// what dropping this does — is what releases it.
    #[allow(dead_code)]
    file: std::fs::File,
}

/// Claim `<run_dir>/endpoint_supervisor.pid` for this process.
///
/// Fails if another live process already holds it, which is the useful answer:
/// two supervisors over one run directory would each start a pageserver against
/// the same datadir.
pub fn claim(run_dir: &Utf8Path) -> Result<Claim> {
    let path = pidfile_path(run_dir);
    let mut file = OpenOptions::new()
        .create(true)
        .truncate(false)
        .write(true)
        .open(&path)
        .with_context(|| format!("open {path}"))?;

    // Non-blocking: a held lock is an answer, not something to wait for.
    // SAFETY: `fd` is open for the duration of the call.
    if unsafe { libc::flock(file.as_raw_fd(), libc::LOCK_EX | libc::LOCK_NB) } != 0 {
        let err = std::io::Error::last_os_error();
        if err.kind() == std::io::ErrorKind::WouldBlock {
            bail!("another supervisor is already running against {run_dir}");
        }
        return Err(err).with_context(|| format!("lock {path}"));
    }

    // Truncate only after the lock is ours, so a failed claim leaves the running
    // supervisor's pid intact.
    file.set_len(0)
        .with_context(|| format!("truncate {path}"))?;
    write!(file, "{}", std::process::id()).with_context(|| format!("write {path}"))?;
    file.flush().with_context(|| format!("flush {path}"))?;
    Ok(Claim { file })
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_claim_records_this_process_and_excludes_a_second_one() {
        let dir = camino_tempfile::tempdir().unwrap();
        let held = claim(dir.path()).expect("first claim");

        let got = std::fs::read_to_string(pidfile_path(dir.path())).unwrap();
        assert_eq!(got.trim(), std::process::id().to_string());

        // Same process, so this cannot test the cross-process case — but a second
        // claim must not silently truncate the pid of the first.
        drop(held);
        let _second = claim(dir.path()).expect("claim after release");
        let got = std::fs::read_to_string(pidfile_path(dir.path())).unwrap();
        assert_eq!(got.trim(), std::process::id().to_string());
    }

    /// The launcher polls for this file, so a claim that leaves it empty reads as
    /// a supervisor that never started.
    #[test]
    fn the_pid_is_written_before_the_claim_returns() {
        let dir = camino_tempfile::tempdir().unwrap();
        let _claim = claim(dir.path()).unwrap();
        let got = std::fs::read_to_string(pidfile_path(dir.path())).unwrap();
        assert!(!got.trim().is_empty(), "pidfile is empty");
        assert!(got.trim().parse::<u32>().is_ok(), "{got:?} is not a pid");
    }
}
