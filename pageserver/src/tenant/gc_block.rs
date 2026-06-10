use std::collections::HashMap;
use std::sync::Arc;

use utils::id::TimelineId;

#[derive(Debug, enumset::EnumSetType, serde::Serialize, serde::Deserialize)]
#[enumset(serialize_repr = "list")]
pub(crate) enum GcBlockingReason {
    Manual,
    DetachAncestor,
}

type Storage = HashMap<TimelineId, enumset::EnumSet<GcBlockingReason>>;

/// GcBlock provides persistent (per-timeline) gc blocking.
#[derive(Default)]
pub(crate) struct GcBlock {
    /// The timelines which have current reasons to block gc.
    ///
    /// LOCK ORDER: this is held locked while scheduling the next index_part update. This is done
    /// to keep the this field up to date with RemoteTimelineClient `upload_queue.dirty`.
    reasons: std::sync::Mutex<Storage>,

    /// GC background task or manually run `Tenant::gc_iteration` holds a lock on this.
    ///
    /// Do not add any more features taking and forbidding taking this lock. It should be
    /// `tokio::sync::Notify`, but that is rarely used. On the other side, [`GcBlock::insert`]
    /// synchronizes with gc attempts by locking and unlocking this mutex.
    blocking: Arc<tokio::sync::Mutex<()>>,
}

impl GcBlock {
    /// Start another gc iteration.
    ///
    /// Returns a guard to be held for the duration of gc iteration to allow synchronizing with
    /// it's ending, or if not currently possible, a value describing the reasons why not.
    ///
    /// Cancellation safe.
    pub(super) async fn start(&self) -> Result<Guard, BlockingReasons> {
        let reasons = {
            let g = self.reasons.lock().unwrap();

            // TODO: the assumption is that this method gets called periodically. in prod, we use 1h, in
            // tests, we use everything. we should warn if the gc has been consecutively blocked
            // for more than 1h (within single tenant session?).
            BlockingReasons::clean_and_summarize(g)
        };

        if let Some(reasons) = reasons {
            Err(reasons)
        } else {
            Ok(Guard {
                _inner: self.blocking.clone().lock_owned().await,
            })
        }
    }

    pub(crate) fn before_delete(&self, timeline_id: &super::TimelineId) {
        let unblocked = {
            let mut g = self.reasons.lock().unwrap();
            if g.is_empty() {
                return;
            }

            g.remove(timeline_id);

            BlockingReasons::clean_and_summarize(g).is_none()
        };

        if unblocked {
            tracing::info!("gc is now unblocked following deletion");
        }
    }

    /// Initialize with the non-deleted timelines of this tenant.
    pub(crate) fn set_scanned(&self, scanned: Storage) {
        let mut g = self.reasons.lock().unwrap();
        assert!(g.is_empty());
        g.extend(scanned.into_iter().filter(|(_, v)| !v.is_empty()));

        if let Some(reasons) = BlockingReasons::clean_and_summarize(g) {
            tracing::info!(summary=?reasons, "initialized with gc blocked");
        }
    }
}

pub(crate) struct Guard {
    _inner: tokio::sync::OwnedMutexGuard<()>,
}

#[derive(Debug)]
pub(crate) struct BlockingReasons {
    timelines: usize,
    reasons: enumset::EnumSet<GcBlockingReason>,
}

impl std::fmt::Display for BlockingReasons {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(
            f,
            "{} timelines block for {:?}",
            self.timelines, self.reasons
        )
    }
}

impl BlockingReasons {
    fn clean_and_summarize(mut g: std::sync::MutexGuard<'_, Storage>) -> Option<Self> {
        let mut reasons = enumset::EnumSet::empty();
        g.retain(|_key, value| {
            reasons = reasons.union(*value);
            !value.is_empty()
        });
        if !g.is_empty() {
            Some(BlockingReasons {
                timelines: g.len(),
                reasons,
            })
        } else {
            None
        }
    }

}
