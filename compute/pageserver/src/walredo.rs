//!
//! WAL redo. This service runs PostgreSQL in a special wal_redo mode
//! to apply given WAL records over an old page image and return new
//! page image.
//!
//! We rely on Postgres to perform WAL redo for us. We launch a
//! postgres process in special "wal redo" mode that's similar to
//! single-user mode. We then pass the previous page image, if any,
//! and all the WAL records we want to apply, to the postgres
//! process. Then we get the page image back. Communication with the
//! postgres process happens via stdin/stdout
//!
//! See pgxn/yb_amp_walredo/walredoproc.c for the other side of
//! this communication.
//!
//! The Postgres process is assumed to be secure against malicious WAL
//! records. It achieves it by dropping privileges before replaying
//! any WAL records, so that even if an attacker hijacks the Postgres
//! process, he cannot escape out of it.

/// Process lifecycle and abstracction for the IPC protocol.
mod process;

/// Code to apply [`NeonWalRecord`]s.
pub(crate) mod apply_neon;

use std::future::Future;
use std::sync::Arc;
use std::sync::atomic::{AtomicUsize, Ordering};
use std::time::{Duration, Instant};

use anyhow::Context;
use bytes::{Bytes, BytesMut};
use pageserver_api::key::Key;
use pageserver_api::models::{WalRedoManagerProcessStatus, WalRedoManagerStatus};
use postgres_ffi::PgMajorVersion;
use tracing::*;
use utils::id::BranchId;
use utils::lsn::Lsn;
use utils::sync::gate::GateError;
use utils::sync::heavier_once_cell;
use wal_decoder::models::record::NeonWalRecord;

use crate::config::PageServerConf;
use crate::metrics::{
    WAL_REDO_BYTES_HISTOGRAM, WAL_REDO_PROCESS_LAUNCH_DURATION_HISTOGRAM,
    WAL_REDO_RECORDS_HISTOGRAM, WAL_REDO_TIME,
};

/// The real implementation that uses a Postgres process to
/// perform WAL replay.
///
/// Per-tenant pool of walredo subprocesses. A single redo request
/// flows through one slot's process at a time (each subprocess is
/// stdin/stdout-bound), but the per-tenant cap on in-flight redo is
/// `walredo_pool_size` rather than 1. Slots are picked round-robin via
/// [`Self::next_slot`].
pub struct PostgresRedoManager {
    branch_id: BranchId,
    conf: &'static PageServerConf,
    last_redo_at: std::sync::Mutex<Option<Instant>>,
    /// One [`heavier_once_cell`] per pool slot. Each cell:
    ///
    /// 1. coalesces the lazy spawning of its walredo process
    ///    ([`ProcessOnceCell::Spawned`]).
    /// 2. is poisoned to [`ProcessOnceCell::ManagerShutDown`] on
    ///    [`Self::shutdown`] so no new process is spawned.
    ///
    /// Requests use the once-cell only to coalesce launch; they hold
    /// the process via `Arc::clone` so a faulting slot can be evicted
    /// (`take_and_deinit`) without disturbing other concurrent users
    /// of the same `Arc<Process>`.
    redo_pool: Vec<heavier_once_cell::OnceCell<ProcessOnceCell>>,

    /// Round-robin slot picker. Wraps modulo `redo_pool.len()`.
    next_slot: AtomicUsize,

    /// Gate that is entered when launching a walredo process and held open
    /// until the process has been `kill()`ed and `wait()`ed upon.
    ///
    /// Manager shutdown waits for this gate to close after setting every
    /// slot to [`ProcessOnceCell::ManagerShutDown`]. The gate tracks
    /// _processes_, not requests — a process may outlive any single
    /// request because we keep it around until quiesce and because the
    /// `Arc` may be held by concurrent requests.
    launched_processes: utils::sync::gate::Gate,
}

/// See [`PostgresRedoManager::redo_process`].
enum ProcessOnceCell {
    Spawned(Arc<Process>),
    ManagerShutDown,
}

struct Process {
    process: process::WalRedoProcess,
    /// This field is last in this struct so the guard gets dropped _after_ [`Self::process`].
    /// (Reminder: dropping [`Self::process`] synchronously sends SIGKILL and then `wait()`s for it to exit).
    _launched_processes_guard: utils::sync::gate::GateGuard,
}

impl std::ops::Deref for Process {
    type Target = process::WalRedoProcess;

    fn deref(&self) -> &Self::Target {
        &self.process
    }
}

#[derive(Debug, thiserror::Error)]
pub enum Error {
    #[error("cancelled")]
    Cancelled,
    #[error(transparent)]
    Other(#[from] anyhow::Error),
}

macro_rules! bail {
    ($($arg:tt)*) => {
        return Err($crate::walredo::Error::Other(::anyhow::anyhow!($($arg)*)));
    }
}

#[derive(Debug, Clone, Copy)]
pub enum RedoAttemptType {
    /// Used for the read path. Will fire critical errors and retry twice if failure.
    ReadPage,
    // Used for legacy compaction (only used in image compaction). Will fire critical errors and retry once if failure.
    LegacyCompaction,
    // Used for gc compaction. Will not fire critical errors and not retry.
    GcCompaction,
}

impl std::fmt::Display for RedoAttemptType {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            RedoAttemptType::ReadPage => write!(f, "read page"),
            RedoAttemptType::LegacyCompaction => write!(f, "legacy compaction"),
            RedoAttemptType::GcCompaction => write!(f, "gc compaction"),
        }
    }
}

///
/// Public interface of WAL redo manager
///
impl PostgresRedoManager {
    ///
    /// Request the WAL redo manager to apply some WAL records
    ///
    /// The WAL redo is handled by a separate thread, so this just sends a request
    /// to the thread and waits for response.
    ///
    /// # Cancel-Safety
    ///
    /// This method is cancellation-safe.
    pub async fn request_redo(
        &self,
        key: Key,
        lsn: Lsn,
        base_img: Option<(Lsn, Bytes)>,
        records: Vec<(Lsn, NeonWalRecord)>,
        pg_version: PgMajorVersion,
        redo_attempt_type: RedoAttemptType,
    ) -> Result<Bytes, Error> {
        if records.is_empty() {
            bail!("invalid WAL redo request with no records");
        }

        let max_retry_attempts = match redo_attempt_type {
            RedoAttemptType::ReadPage => 2,
            RedoAttemptType::LegacyCompaction => 1,
            RedoAttemptType::GcCompaction => 0,
        };

        let base_img_lsn = base_img.as_ref().map(|p| p.0).unwrap_or(Lsn::INVALID);
        let mut img = base_img.map(|p| p.1);
        let mut batch_neon = apply_neon::can_apply_in_neon(&records[0].1);
        let mut batch_start = 0;
        for (i, record) in records.iter().enumerate().skip(1) {
            let rec_neon = apply_neon::can_apply_in_neon(&record.1);

            if rec_neon != batch_neon {
                let result = if batch_neon {
                    self.apply_batch_neon(key, lsn, img, &records[batch_start..i])
                } else {
                    self.apply_batch_postgres(
                        key,
                        lsn,
                        img,
                        base_img_lsn,
                        &records[batch_start..i],
                        self.conf.wal_redo_timeout,
                        pg_version,
                        max_retry_attempts,
                        redo_attempt_type,
                    )
                    .await
                };
                img = Some(result?);

                batch_neon = rec_neon;
                batch_start = i;
            }
        }
        // last batch
        if batch_neon {
            self.apply_batch_neon(key, lsn, img, &records[batch_start..])
        } else {
            self.apply_batch_postgres(
                key,
                lsn,
                img,
                base_img_lsn,
                &records[batch_start..],
                self.conf.wal_redo_timeout,
                pg_version,
                max_retry_attempts,
                redo_attempt_type,
            )
            .await
        }
    }

    /// Do a ping request-response roundtrip.
    ///
    /// Not used in production, but by Rust benchmarks.
    ///
    /// # Cancel-Safety
    ///
    /// This method is cancellation-safe.
    pub async fn ping(&self, pg_version: PgMajorVersion) -> Result<(), Error> {
        self.do_with_walredo_process(pg_version, |proc| async move {
            proc.ping(Duration::from_secs(1))
                .await
                .map_err(Error::Other)
        })
        .await
    }

    pub fn status(&self) -> WalRedoManagerStatus {
        WalRedoManagerStatus {
            last_redo_at: {
                let at = *self.last_redo_at.lock().unwrap();
                at.and_then(|at| {
                    let age = at.elapsed();
                    // map any chrono errors silently to None here
                    chrono::Utc::now().checked_sub_signed(chrono::Duration::from_std(age).ok()?)
                })
            },
            processes: self
                .redo_pool
                .iter()
                .filter_map(|slot| {
                    slot.get().and_then(|p| match &*p {
                        ProcessOnceCell::Spawned(p) => {
                            Some(WalRedoManagerProcessStatus { pid: p.id() })
                        }
                        ProcessOnceCell::ManagerShutDown => None,
                    })
                })
                .collect(),
        }
    }
}

impl PostgresRedoManager {
    ///
    /// Create a new PostgresRedoManager.
    ///
    pub fn new(conf: &'static PageServerConf, branch_id: BranchId) -> PostgresRedoManager {
        // The actual processes are launched lazily, on first request to each slot.
        let pool_size = conf.walredo_pool_size.get();
        let redo_pool = (0..pool_size)
            .map(|_| heavier_once_cell::OnceCell::default())
            .collect();
        PostgresRedoManager {
            branch_id,
            conf,
            last_redo_at: std::sync::Mutex::default(),
            redo_pool,
            next_slot: AtomicUsize::new(0),
            launched_processes: utils::sync::gate::Gate::default(),
        }
    }

    /// Shut down the WAL redo manager.
    ///
    /// Returns `true` if this call was the one that initiated shutdown.
    /// `true` may be observed by no caller if the first caller stops polling.
    ///
    /// After this future completes
    /// - no redo process is running
    /// - no new redo process will be spawned
    /// - redo requests that need walredo process will fail with [`Error::Cancelled`]
    /// - [`apply_neon`]-only redo requests may still work, but this may change in the future
    ///
    /// # Cancel-Safety
    ///
    /// This method is cancellation-safe.
    pub async fn shutdown(&self) -> bool {
        // Poison every slot so no slot can spawn a new process. Track
        // whether *any* slot transitioned (i.e. we initiated shutdown).
        let mut any_transitioned = false;
        for slot in &self.redo_pool {
            let maybe_permit = match slot.get_or_init_detached().await {
                Ok(guard) => {
                    if matches!(&*guard, ProcessOnceCell::ManagerShutDown) {
                        None
                    } else {
                        let (proc, permit) = guard.take_and_deinit();
                        drop(proc); // drops the Arc; refcount may not be zero yet
                        Some(permit)
                    }
                }
                Err(permit) => Some(permit),
            };
            if let Some(permit) = maybe_permit {
                slot.set(ProcessOnceCell::ManagerShutDown, permit);
                any_transitioned = true;
            }
        }
        // wait for ongoing requests to drain and the refcounts of all Arc<WalRedoProcess> that
        // we ever launched to drop to zero, which when it happens synchronously kill()s & wait()s
        // for the underlying process.
        self.launched_processes.close().await;
        any_transitioned
    }

    /// This type doesn't have its own background task to check for idleness: we
    /// rely on our owner calling this function periodically in its own housekeeping
    /// loops.
    pub(crate) fn maybe_quiesce(&self, idle_timeout: Duration) {
        if let Ok(g) = self.last_redo_at.try_lock() {
            if let Some(last_redo_at) = *g {
                if last_redo_at.elapsed() >= idle_timeout {
                    drop(g);
                    for slot in &self.redo_pool {
                        drop(slot.get().map(|guard| guard.take_and_deinit()));
                    }
                }
            }
        }
    }

    /// # Cancel-Safety
    ///
    /// This method is cancel-safe iff `closure` is cancel-safe.
    /// Pick a pool slot in round-robin order. Wraps modulo
    /// `redo_pool.len()`; safe to call concurrently.
    fn pick_slot(&self) -> usize {
        let idx = self.next_slot.fetch_add(1, Ordering::Relaxed);
        idx % self.redo_pool.len()
    }

    async fn do_with_walredo_process<
        F: FnOnce(Arc<Process>) -> Fut,
        Fut: Future<Output = Result<O, Error>>,
        O,
    >(
        &self,
        pg_version: PgMajorVersion,
        closure: F,
    ) -> Result<O, Error> {
        let slot_idx = self.pick_slot();
        let slot = &self.redo_pool[slot_idx];
        let proc: Arc<Process> = match slot.get_or_init_detached().await {
            Ok(guard) => match &*guard {
                ProcessOnceCell::Spawned(proc) => Arc::clone(proc),
                ProcessOnceCell::ManagerShutDown => {
                    return Err(Error::Cancelled);
                }
            },
            Err(permit) => {
                let start = Instant::now();
                // acquire guard before spawning process, so that we don't spawn new processes
                // if the gate is already closed.
                let _launched_processes_guard = match self.launched_processes.enter() {
                    Ok(guard) => guard,
                    Err(GateError::GateClosed) => unreachable!(
                        "shutdown sets every once cell to `ManagerShutDown` state before closing the gate"
                    ),
                };
                let proc = Arc::new(Process {
                    process: process::WalRedoProcess::launch(self.conf, self.branch_id, pg_version)
                        .context("launch walredo process")?,
                    _launched_processes_guard,
                });
                let duration = start.elapsed();
                WAL_REDO_PROCESS_LAUNCH_DURATION_HISTOGRAM.observe(duration.as_secs_f64());
                info!(
                    elapsed_ms = duration.as_millis(),
                    pid = proc.id(),
                    slot = slot_idx,
                    "launched walredo process"
                );
                slot.set(ProcessOnceCell::Spawned(Arc::clone(&proc)), permit);
                proc
            }
        };

        // async closures are unstable, would support &Process
        let result = closure(proc.clone()).await;

        if result.is_err() {
            // Take the faulty process out of its slot. Other concurrent
            // tasks may still hold the same `Arc<Process>`; the last one
            // to drop it will SIGKILL/wait the subprocess.
            //
            // NB: drop blocks on wait(). If walredo always errors
            // immediately, this caps runaway respawn at
            // $num_runtimes * $num_executor_threads.
            if let Some(guard) = slot.get() {
                match &*guard {
                    ProcessOnceCell::ManagerShutDown => {}
                    ProcessOnceCell::Spawned(guard_proc) => {
                        if Arc::ptr_eq(&proc, guard_proc) {
                            // First to observe the error — take it out of rotation.
                            guard.take_and_deinit();
                        } else {
                            // Another task already evicted this slot and put a fresh
                            // process there. Do nothing — our view is stale.
                        }
                    }
                }
            }
            // The last task to drop `proc` runs the blocking wait().
            drop(proc);
        }

        result
    }

    ///
    /// Process one request for WAL redo using wal-redo postgres
    ///
    /// # Cancel-Safety
    ///
    /// Cancellation safe.
    #[allow(clippy::too_many_arguments)]
    async fn apply_batch_postgres(
        &self,
        key: Key,
        lsn: Lsn,
        base_img: Option<Bytes>,
        base_img_lsn: Lsn,
        records: &[(Lsn, NeonWalRecord)],
        wal_redo_timeout: Duration,
        pg_version: PgMajorVersion,
        max_retry_attempts: u32,
        redo_attempt_type: RedoAttemptType,
    ) -> Result<Bytes, Error> {
        *(self.last_redo_at.lock().unwrap()) = Some(Instant::now());

        let (rel, blknum) = key.to_rel_block().context("invalid record")?;
        let mut n_attempts = 0u32;
        loop {
            let base_img = &base_img;
            let closure = |proc: Arc<Process>| async move {
                let started_at = std::time::Instant::now();

                // Relational WAL records are applied using wal-redo-postgres
                let result = proc
                    .apply_wal_records(rel, blknum, base_img, records, wal_redo_timeout)
                    .await
                    .context("apply_wal_records");

                let duration = started_at.elapsed();

                let len = records.len();
                let nbytes = records.iter().fold(0, |acumulator, record| {
                    acumulator
                        + match &record.1 {
                            NeonWalRecord::Postgres { rec, .. } => rec.len(),
                            _ => unreachable!("Only PostgreSQL records are accepted in this batch"),
                        }
                });

                WAL_REDO_TIME.observe(duration.as_secs_f64());
                WAL_REDO_RECORDS_HISTOGRAM.observe(len as f64);
                WAL_REDO_BYTES_HISTOGRAM.observe(nbytes as f64);

                debug!(
                    "postgres applied {} WAL records ({} bytes) in {} us to reconstruct page image at LSN {}",
                    len,
                    nbytes,
                    duration.as_micros(),
                    lsn
                );

                if let Err(e) = result.as_ref() {
                    macro_rules! message {
                        ($level:tt) => {
                            $level!(
                                "error applying {} WAL records {}..{} ({} bytes) to key {} during {}, from base image with LSN {} to reconstruct page image at LSN {} n_attempts={}: {:?}",
                                records.len(),
                                records.first().map(|p| p.0).unwrap_or(Lsn(0)),
                                records.last().map(|p| p.0).unwrap_or(Lsn(0)),
                                nbytes,
                                key,
                                redo_attempt_type,
                                base_img_lsn,
                                lsn,
                                n_attempts,
                                e,
                            )
                        }
                    }
                    match redo_attempt_type {
                        RedoAttemptType::ReadPage => message!(error),
                        RedoAttemptType::LegacyCompaction => message!(error),
                        RedoAttemptType::GcCompaction => message!(warn),
                    }
                }

                result.map_err(Error::Other)
            };
            let result = self.do_with_walredo_process(pg_version, closure).await;

            if result.is_ok() && n_attempts != 0 {
                info!(n_attempts, "retried walredo succeeded");
            }
            n_attempts += 1;
            if n_attempts > max_retry_attempts || result.is_ok() {
                return result;
            }
        }
    }

    ///
    /// Process a batch of WAL records using bespoken Neon code.
    ///
    fn apply_batch_neon(
        &self,
        key: Key,
        lsn: Lsn,
        base_img: Option<Bytes>,
        records: &[(Lsn, NeonWalRecord)],
    ) -> Result<Bytes, Error> {
        let start_time = Instant::now();

        let mut page = BytesMut::new();
        if let Some(fpi) = base_img {
            // If full-page image is provided, then use it...
            page.extend_from_slice(&fpi[..]);
        } else {
            // All the current WAL record types that we can handle require a base image.
            bail!("invalid neon WAL redo request with no base image");
        }

        // Apply all the WAL records in the batch
        for (record_lsn, record) in records.iter() {
            self.apply_record_neon(key, &mut page, *record_lsn, record)?;
        }
        // Success!
        let duration = start_time.elapsed();
        // FIXME: using the same metric here creates a bimodal distribution by default, and because
        // there could be multiple batch sizes this would be N+1 modal.
        WAL_REDO_TIME.observe(duration.as_secs_f64());

        debug!(
            "neon applied {} WAL records in {} us to reconstruct page image at LSN {}",
            records.len(),
            duration.as_micros(),
            lsn
        );

        Ok(page.freeze())
    }

    fn apply_record_neon(
        &self,
        key: Key,
        page: &mut BytesMut,
        record_lsn: Lsn,
        record: &NeonWalRecord,
    ) -> anyhow::Result<()> {
        apply_neon::apply_in_neon(record, record_lsn, key, page)?;

        Ok(())
    }
}
