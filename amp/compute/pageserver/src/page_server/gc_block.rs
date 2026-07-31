use std::sync::Arc;

#[derive(Debug, enumset::EnumSetType, serde::Serialize, serde::Deserialize)]
#[enumset(serialize_repr = "list")]
pub(crate) enum GcBlockingReason {
    Manual,
    DetachAncestor,
}

/// GcBlock provides gc blocking for the timeline.
#[derive(Default)]
pub(crate) struct GcBlock {
    /// Reasons currently blocking gc for the timeline; empty means gc is
    /// allowed to run.
    ///
    /// LOCK ORDER: this is held locked while scheduling the next index_part
    /// update, to keep it consistent with RemoteTimelineClient
    /// `upload_queue.dirty`.
    reasons: std::sync::Mutex<enumset::EnumSet<GcBlockingReason>>,

    /// GC background task or manually run `Tenant::gc_iteration` holds a lock
    /// on this.
    ///
    /// Do not add any more features taking and forbidding taking this lock. It
    /// should be `tokio::sync::Notify`, but that is rarely used. On the other
    /// side, gc-block mutations synchronize with gc attempts by locking and
    /// unlocking this mutex.
    blocking: Arc<tokio::sync::Mutex<()>>,
}

impl GcBlock {
    /// Start another gc iteration.
    ///
    /// Returns a guard to be held for the duration of gc iteration to allow
    /// synchronizing with its ending, or if not currently possible, a value
    /// describing the reasons why not.
    ///
    /// Cancellation safe.
    pub(super) async fn start(&self) -> Result<Guard, BlockingReasons> {
        let reasons = *self.reasons.lock().unwrap();
        if reasons.is_empty() {
            Ok(Guard {
                _inner: self.blocking.clone().lock_owned().await,
            })
        } else {
            Err(BlockingReasons { reasons })
        }
    }

    /// Initialize with the timeline's gc-blocking reasons (empty if none).
    pub(crate) fn set_scanned(&self, scanned: enumset::EnumSet<GcBlockingReason>) {
        let mut g = self.reasons.lock().unwrap();
        assert!(g.is_empty());
        *g = scanned;
        if !scanned.is_empty() {
            tracing::info!(summary=?BlockingReasons { reasons: scanned }, "initialized with gc blocked");
        }
    }
}

pub(crate) struct Guard {
    _inner: tokio::sync::OwnedMutexGuard<()>,
}

#[derive(Debug)]
pub(crate) struct BlockingReasons {
    reasons: enumset::EnumSet<GcBlockingReason>,
}

impl std::fmt::Display for BlockingReasons {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "timeline blocks gc for {:?}", self.reasons)
    }
}
