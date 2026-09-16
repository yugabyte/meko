//! Notices a renewed TLS file on disk.
//!
//! cert-manager renews a certificate ahead of expiry and the kubelet swaps
//! the mounted files; nothing tells the process. Both YugabyteDB lanes have
//! to notice, and they react differently — the YSQL lane (`ysql_client`)
//! bumps a generation so pooled slots reopen, the thin lane
//! (`yb_thin_client`) rebuilds its shim connection — but they must agree on
//! when a file counts as changed and how often to look. That agreement is
//! this crate; the reaction stays with each lane.
//!
//! Polling rather than inotify: the kubelet swaps a projected volume through
//! a symlink, which inotify on the file itself misses, and a minute of lag
//! against a renewal that runs days ahead of expiry costs nothing.

use std::sync::atomic::{AtomicU64, Ordering};
use std::time::Duration;

/// How many times a watch in this process has seen a watched file change and
/// asked its lane to reconnect.
///
/// The daemons report it on `/v1/status`. It is the only signal that separates
/// a reload from a rewrite: the certificate a status handler prints is re-read
/// from disk on every request, so it shows a renewal the instant cert-manager
/// writes it, whether or not any watcher was alive to act on it.
static RELOADS: AtomicU64 = AtomicU64::new(0);

pub fn reloads() -> u64 {
    RELOADS.load(Ordering::Acquire)
}

/// How often the files are re-read. `YB_TLS_RELOAD_POLL_SECS` overrides it,
/// for tests that renew a certificate and want to see the effect quickly.
pub const POLL_INTERVAL: Duration = Duration::from_secs(60);

pub fn poll_interval() -> Duration {
    std::env::var("YB_TLS_RELOAD_POLL_SECS")
        .ok()
        .and_then(|s| s.parse::<u64>().ok())
        .filter(|s| *s > 0)
        .map(Duration::from_secs)
        .unwrap_or(POLL_INTERVAL)
}

/// The last-seen contents of a set of files. They are small (a CA bundle, a
/// leaf certificate, a key), so the bytes themselves are the fingerprint.
pub struct FileSnapshot {
    paths: Vec<String>,
    contents: Vec<Option<Vec<u8>>>,
}

impl FileSnapshot {
    pub fn take(paths: Vec<String>) -> Self {
        let contents = paths.iter().map(|p| std::fs::read(p).ok()).collect();
        Self { paths, contents }
    }

    /// The paths whose contents differ from the last read. A file that cannot
    /// be read keeps its previous contents and is not reported: a transient
    /// read failure must not reconnect against files that have not moved. A
    /// file seen for the first time is a baseline, not a change.
    pub fn refresh(&mut self) -> Vec<String> {
        let mut changed = Vec::new();
        for (path, last) in self.paths.iter().zip(self.contents.iter_mut()) {
            match std::fs::read(path) {
                Ok(now) => {
                    if last.as_ref() != Some(&now) {
                        if last.is_some() {
                            changed.push(path.clone());
                        }
                        *last = Some(now);
                    }
                }
                Err(e) => tracing::debug!("TLS reload: could not read {path}: {e}"),
            }
        }
        changed
    }
}

/// Poll `paths` every `interval`, handing the rewritten ones to `on_change`.
///
/// `on_change` runs on every tick, with an empty slice when nothing moved,
/// and returns false to end the watch — how a lane says the pool it would
/// have reloaded is gone. Spawns nothing when there is nothing to watch.
///
/// The first tick lands at a random point inside the interval. A reload
/// forces a reconnect regardless of the connection's age, so it bypasses the
/// per-slot lifetime jitter that keeps a pool from recycling in lockstep;
/// without a phase offset here, every pool in a process — and every daemon
/// from one deploy, since cert-manager renews their certificates together —
/// would notice a fleet-wide renewal at the same instant and reconnect as one
/// storm. Spreading the trigger costs at most one interval of extra lag
/// against a renewal that runs days ahead of expiry.
pub fn watch<F>(paths: Vec<String>, interval: Duration, mut on_change: F)
where
    F: FnMut(&[String]) -> bool + Send + 'static,
{
    if paths.is_empty() {
        return;
    }
    // Baseline before spawning, not inside the task: the files as they are
    // when the pool is built are what a later poll is compared against. Taking
    // it in the task instead would adopt whatever a renewal wrote while that
    // task waited to be scheduled, and never report the change.
    let mut snapshot = FileSnapshot::take(paths);
    let phase = interval.mul_f64(rand::random::<f64>());
    tokio::spawn(async move {
        tokio::time::sleep(phase).await;
        loop {
            let changed = snapshot.refresh();
            if !changed.is_empty() {
                RELOADS.fetch_add(1, Ordering::AcqRel);
            }
            if !on_change(&changed) {
                return;
            }
            tokio::time::sleep(interval).await;
        }
    });
}

#[cfg(test)]
mod tests {
    use super::{FileSnapshot, watch};
    use std::sync::Arc;
    use std::sync::atomic::{AtomicUsize, Ordering};
    use std::time::Duration;

    fn tmp(name: &str, contents: &[u8]) -> String {
        let dir = std::env::temp_dir().join(format!("yb-tls-watch-{}", std::process::id()));
        std::fs::create_dir_all(&dir).unwrap();
        let path = dir.join(name);
        std::fs::write(&path, contents).unwrap();
        path.to_string_lossy().into_owned()
    }

    #[test]
    fn only_rewritten_files_are_reported() {
        let ca = tmp("ca.crt", b"ca-v1");
        let cert = tmp("tls.crt", b"cert-v1");
        let mut snap = FileSnapshot::take(vec![ca.clone(), cert.clone()]);
        assert!(snap.refresh().is_empty(), "nothing moved");

        std::fs::write(&cert, b"cert-v2").unwrap();
        assert_eq!(snap.refresh(), vec![cert]);
        assert!(snap.refresh().is_empty(), "reported once");

        std::fs::write(&ca, b"ca-v1").unwrap();
        assert!(snap.refresh().is_empty(), "same bytes is not a change");
    }

    #[test]
    fn an_unreadable_file_is_not_a_change() {
        let cert = tmp("gone.crt", b"cert-v1");
        let mut snap = FileSnapshot::take(vec![cert.clone()]);
        std::fs::remove_file(&cert).unwrap();
        assert!(snap.refresh().is_empty());
        std::fs::write(&cert, b"cert-v2").unwrap();
        assert_eq!(snap.refresh(), vec![cert]);
    }

    #[test]
    fn a_file_that_appears_later_is_not_reported_on_first_sight() {
        let path = std::env::temp_dir()
            .join(format!("yb-tls-watch-{}-absent.crt", std::process::id()))
            .to_string_lossy()
            .into_owned();
        let _ = std::fs::remove_file(&path);
        let mut snap = FileSnapshot::take(vec![path.clone()]);
        std::fs::write(&path, b"v1").unwrap();
        assert!(snap.refresh().is_empty(), "first contents are the baseline");
        std::fs::write(&path, b"v2").unwrap();
        assert_eq!(snap.refresh(), vec![path]);
    }

    /// Two watches on the same interval do not tick together: the reload
    /// bypasses the per-slot lifetime jitter, so this phase offset is what
    /// keeps a fleet-wide renewal from reconnecting everything at once.
    #[tokio::test]
    async fn watches_do_not_all_start_on_the_same_phase() {
        let cert = tmp("phase.crt", b"v1");
        let interval = Duration::from_millis(400);
        let firsts: Arc<std::sync::Mutex<Vec<Duration>>> = Arc::new(std::sync::Mutex::new(vec![]));
        let start = std::time::Instant::now();
        for _ in 0..16 {
            let (f, c) = (firsts.clone(), cert.clone());
            let mut seen = false;
            watch(vec![c], interval, move |_| {
                if !seen {
                    seen = true;
                    f.lock().unwrap().push(start.elapsed());
                }
                false
            });
        }
        tokio::time::sleep(interval + Duration::from_millis(100)).await;
        let got = firsts.lock().unwrap().clone();
        assert_eq!(got.len(), 16, "every watch ticked once");
        let spread = got
            .iter()
            .max()
            .unwrap()
            .saturating_sub(*got.iter().min().unwrap());
        assert!(
            spread > interval / 4,
            "first ticks landed within {spread:?} of each other, want them spread over {interval:?}"
        );
    }

    #[tokio::test]
    async fn the_watch_ends_when_on_change_says_so() {
        let cert = tmp("ending.crt", b"v1");
        let ticks = Arc::new(AtomicUsize::new(0));
        let seen = ticks.clone();
        watch(vec![cert], Duration::from_millis(5), move |_| {
            seen.fetch_add(1, Ordering::AcqRel) < 1
        });
        tokio::time::sleep(Duration::from_millis(300)).await;
        assert_eq!(ticks.load(Ordering::Acquire), 2, "one tick past the stop");
    }
}
