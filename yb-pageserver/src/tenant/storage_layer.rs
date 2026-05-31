//! Common traits and structs for layers

pub mod inmemory_layer;
pub(crate) mod layer;
mod layer_desc;
mod layer_name;

use std::ops::Range;
use std::pin::Pin;
use std::sync::Arc;
use std::sync::atomic::AtomicUsize;
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use bytes::Bytes;
use futures::StreamExt;
use futures::stream::FuturesUnordered;
pub use inmemory_layer::InMemoryLayer;
pub(crate) use layer::{EvictionError, Layer, ResidentLayer};
pub use layer_desc::{PersistentLayerDesc, PersistentLayerKey};
pub use layer_name::{DeltaLayerName, ImageLayerName, LayerName};
use pageserver_api::config::GetVectoredConcurrentIo;
use tracing::{Instrument, trace};
use utils::lsn::Lsn;
use utils::sync::gate::GateGuard;
use wal_decoder::models::record::NeonWalRecord;

use super::layer_map::InMemoryLayerDesc;
use crate::context::PerfInstrumentFutureExt;

pub fn range_overlaps<T>(a: &Range<T>, b: &Range<T>) -> bool
where
    T: PartialOrd<T>,
{
    if a.start < b.start {
        a.end > b.start
    } else {
        b.end > a.start
    }
}

/// Struct used to communicate across calls to 'get_value_reconstruct_data'.
///
/// Before first call, you can fill in 'page_img' if you have an older cached
/// version of the page available. That can save work in
/// 'get_value_reconstruct_data', as it can stop searching for page versions
/// when all the WAL records going back to the cached image have been collected.
///
/// When get_value_reconstruct_data returns Complete, 'img' is set to an image
/// of the page, or the oldest WAL record in 'records' is a will_init-type
/// record that initializes the page without requiring a previous image.
///
/// If 'get_page_reconstruct_data' returns Continue, some 'records' may have
/// been collected, but there are more records outside the current layer. Pass
/// the same ValueReconstructState struct in the next 'get_value_reconstruct_data'
/// call, to collect more records.
///
#[derive(Debug, Default, Clone)]
pub(crate) struct ValueReconstructState {
    pub(crate) records: Vec<(Lsn, NeonWalRecord)>,
    pub(crate) img: Option<(Lsn, Bytes)>,
}

impl ValueReconstructState {
    /// Returns the number of page deltas applied to the page image.
    pub fn num_deltas(&self) -> usize {
        match self.img {
            Some(_) => self.records.len(),
            None => self.records.len() - 1, // omit will_init record
        }
    }
}











/// yb-mode: vestige. The legacy `get_vectored` traversal used this
/// to accumulate per-key reconstruct buffers. yb-pageserver's
/// `Timeline::get_vectored_impl` ignores it entirely (the row-store
/// read path runs through `PageWriter::scan_range`); we keep the
/// struct as a unit type so external callers (basebackup,
/// pgdatadir_mapping) can keep their existing signatures.
pub struct ValuesReconstructState;

/// The level of IO concurrency to be used on the read path
///
/// The desired end state is that we always do parallel IO.
/// This struct and the dispatching in the impl will be removed once
/// we've built enough confidence.
pub enum IoConcurrency {
    Sequential,
    SidecarTask {
        task_id: usize,
        ios_tx: tokio::sync::mpsc::UnboundedSender<IoFuture>,
    },
}

type IoFuture = Pin<Box<dyn Send + Future<Output = ()>>>;

pub(crate) enum SelectedIoConcurrency {
    Sequential,
    SidecarTask(GateGuard),
}

impl std::fmt::Debug for IoConcurrency {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            IoConcurrency::Sequential => write!(f, "Sequential"),
            IoConcurrency::SidecarTask { .. } => write!(f, "SidecarTask"),
        }
    }
}

impl std::fmt::Debug for SelectedIoConcurrency {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            SelectedIoConcurrency::Sequential => write!(f, "Sequential"),
            SelectedIoConcurrency::SidecarTask(_) => write!(f, "SidecarTask"),
        }
    }
}

impl IoConcurrency {
    /// Force sequential IO. This is a temporary workaround until we have
    /// moved plumbing-through-the-call-stack
    /// of IoConcurrency into `RequestContextq.
    ///
    /// DO NOT USE for new code.
    ///
    /// Tracking issue: <https://github.com/neondatabase/neon/issues/10460>.
    pub(crate) fn sequential() -> Self {
        Self::spawn(SelectedIoConcurrency::Sequential)
    }

    pub fn spawn_from_conf(conf: GetVectoredConcurrentIo, gate_guard: GateGuard) -> IoConcurrency {
        let selected = match conf {
            GetVectoredConcurrentIo::Sequential => SelectedIoConcurrency::Sequential,
            GetVectoredConcurrentIo::SidecarTask => SelectedIoConcurrency::SidecarTask(gate_guard),
        };
        Self::spawn(selected)
    }

    pub(crate) fn spawn(io_concurrency: SelectedIoConcurrency) -> Self {
        match io_concurrency {
            SelectedIoConcurrency::Sequential => IoConcurrency::Sequential,
            SelectedIoConcurrency::SidecarTask(gate_guard) => {
                let (ios_tx, ios_rx) = tokio::sync::mpsc::unbounded_channel();
                static TASK_ID: AtomicUsize = AtomicUsize::new(0);
                let task_id = TASK_ID.fetch_add(1, std::sync::atomic::Ordering::Relaxed);
                // TODO: enrich the span with more context (tenant,shard,timeline) + (basebackup|pagestream|...)
                let span =
                    tracing::info_span!(parent: None, "IoConcurrency_sidecar", task_id = task_id);
                trace!(task_id, "spawning sidecar task");
                tokio::spawn(async move {
                    trace!("start");
                    scopeguard::defer!{ trace!("end") };
                    type IosRx = tokio::sync::mpsc::UnboundedReceiver<IoFuture>;
                    enum State {
                        Waiting {
                            // invariant: is_empty(), but we recycle the allocation
                            empty_futures: FuturesUnordered<IoFuture>,
                            ios_rx: IosRx,
                        },
                        Executing {
                            futures: FuturesUnordered<IoFuture>,
                            ios_rx: IosRx,
                        },
                        ShuttingDown {
                            futures: FuturesUnordered<IoFuture>,
                        },
                    }
                    let mut state = State::Waiting {
                        empty_futures: FuturesUnordered::new(),
                        ios_rx,
                    };
                    loop {
                        match state {
                            State::Waiting {
                                empty_futures,
                                mut ios_rx,
                            } => {
                                assert!(empty_futures.is_empty());
                                tokio::select! {
                                    fut = ios_rx.recv() => {
                                        if let Some(fut) = fut {
                                            trace!("received new io future");
                                            empty_futures.push(fut);
                                            state = State::Executing { futures: empty_futures, ios_rx };
                                        } else {
                                            state = State::ShuttingDown { futures: empty_futures }
                                        }
                                    }
                                }
                            }
                            State::Executing {
                                mut futures,
                                mut ios_rx,
                            } => {
                                tokio::select! {
                                    res = futures.next() => {
                                        trace!("io future completed");
                                        assert!(res.is_some());
                                        if futures.is_empty() {
                                            state = State::Waiting { empty_futures: futures, ios_rx};
                                        } else {
                                            state = State::Executing { futures, ios_rx };
                                        }
                                    }
                                    fut = ios_rx.recv() => {
                                        if let Some(fut) = fut {
                                            trace!("received new io future");
                                            futures.push(fut);
                                            state =  State::Executing { futures, ios_rx};
                                        } else {
                                            state = State::ShuttingDown { futures };
                                        }
                                    }
                                }
                            }
                            State::ShuttingDown {
                                mut futures,
                            } => {
                                trace!("shutting down");
                                while let Some(()) = futures.next().await {
                                    trace!("io future completed (shutdown)");
                                    // drain
                                }
                                trace!("shutdown complete");
                                break;
                            }
                        }
                    }
                    drop(gate_guard); // drop it right before we exit
                }.instrument(span));
                IoConcurrency::SidecarTask { task_id, ios_tx }
            }
        }
    }

    #[cfg(test)]
    pub(crate) fn spawn_for_test() -> impl std::ops::DerefMut<Target = Self> {
        use std::ops::{Deref, DerefMut};

        use tracing::info;
        use utils::sync::gate::Gate;

        // Spawn needs a Gate, give it one.
        struct Wrapper {
            inner: IoConcurrency,
            #[allow(dead_code)]
            gate: Box<Gate>,
        }
        impl Deref for Wrapper {
            type Target = IoConcurrency;

            fn deref(&self) -> &Self::Target {
                &self.inner
            }
        }
        impl DerefMut for Wrapper {
            fn deref_mut(&mut self) -> &mut Self::Target {
                &mut self.inner
            }
        }
        let gate = Box::new(Gate::default());

        // The default behavior when running Rust unit tests without any further
        // flags is to use the new behavior.
        // The CI uses the following environment variable to unit test both old
        // and new behavior.
        // NB: the Python regression & perf tests take the `else` branch
        // below and have their own defaults management.
        let selected = {
            // The pageserver_api::config type is unsuitable because it's internally tagged.
            #[derive(serde::Deserialize)]
            #[serde(rename_all = "kebab-case")]
            enum TestOverride {
                Sequential,
                SidecarTask,
            }
            use once_cell::sync::Lazy;
            static TEST_OVERRIDE: Lazy<TestOverride> = Lazy::new(|| {
                utils::env::var_serde_json_string(
                    "NEON_PAGESERVER_UNIT_TEST_GET_VECTORED_CONCURRENT_IO",
                )
                .unwrap_or(TestOverride::SidecarTask)
            });

            match *TEST_OVERRIDE {
                TestOverride::Sequential => SelectedIoConcurrency::Sequential,
                TestOverride::SidecarTask => {
                    SelectedIoConcurrency::SidecarTask(gate.enter().expect("just created it"))
                }
            }
        };

        info!(?selected, "get_vectored_concurrent_io test");

        Wrapper {
            inner: Self::spawn(selected),
            gate,
        }
    }
}

impl Clone for IoConcurrency {
    fn clone(&self) -> Self {
        match self {
            IoConcurrency::Sequential => IoConcurrency::Sequential,
            IoConcurrency::SidecarTask { task_id, ios_tx } => IoConcurrency::SidecarTask {
                task_id: *task_id,
                ios_tx: ios_tx.clone(),
            },
        }
    }
}

impl ValuesReconstructState {
    pub fn new(_io_concurrency: IoConcurrency) -> Self {
        Self
    }
}

#[derive(Debug, PartialEq, Eq, Hash)]
pub enum ReadableLayerWeak {
    PersistentLayer(Arc<PersistentLayerDesc>),
    InMemoryLayer(InMemoryLayerDesc),
}

/// Layers contain a hint indicating whether they are likely to be used for reads.
///
/// This is a hint rather than an authoritative value, so that we do not have to update it synchronously
/// when changing the visibility of layers (for example when creating a branch that makes some previously
/// covered layers visible).  It should be used for cache management but not for correctness-critical checks.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum LayerVisibilityHint {
    /// A Visible layer might be read while serving a read, because there is not an image layer between it
    /// and a readable LSN (the tip of the branch or a child's branch point)
    Visible,
    /// A Covered layer probably won't be read right now, but _can_ be read in future if someone creates
    /// a branch or ephemeral endpoint at an LSN below the layer that covers this.
    Covered,
}

pub(crate) struct LayerAccessStats(std::sync::atomic::AtomicU64);

#[derive(Clone, Copy, strum_macros::EnumString)]
pub(crate) enum LayerAccessStatsReset {
    NoReset,
    AllStats,
}

impl Default for LayerAccessStats {
    fn default() -> Self {
        // Default value is to assume resident since creation time, and visible.
        let (_mask, mut value) = Self::to_low_res_timestamp(Self::RTIME_SHIFT, SystemTime::now());
        value |= 0x1 << Self::VISIBILITY_SHIFT;

        Self(std::sync::atomic::AtomicU64::new(value))
    }
}

// Efficient store of two very-low-resolution timestamps and some bits.  Used for storing last access time and
// last residence change time.
impl LayerAccessStats {
    // How many high bits to drop from a u32 timestamp?
    // - Only storing up to a u32 timestamp will work fine until 2038 (if this code is still in use
    //   after that, this software has been very successful!)
    // - Dropping the top bit is implicitly safe because unix timestamps are meant to be
    // stored in an i32, so they never used it.
    // - Dropping the next two bits is safe because this code is only running on systems in
    // years >= 2024, and these bits have been 1 since 2021
    //
    // Therefore we may store only 28 bits for a timestamp with one second resolution.  We do
    // this truncation to make space for some flags in the high bits of our u64.
    const TS_DROP_HIGH_BITS: u32 = u32::count_ones(Self::TS_ONES) + 1;
    const TS_MASK: u32 = 0x1f_ff_ff_ff;
    const TS_ONES: u32 = 0x60_00_00_00;

    const ATIME_SHIFT: u32 = 0;
    const RTIME_SHIFT: u32 = 32 - Self::TS_DROP_HIGH_BITS;
    const VISIBILITY_SHIFT: u32 = 64 - 2 * Self::TS_DROP_HIGH_BITS;

    fn write_bits(&self, mask: u64, value: u64) -> u64 {
        self.0
            .fetch_update(
                // TODO: decide what orderings are correct
                std::sync::atomic::Ordering::Relaxed,
                std::sync::atomic::Ordering::Relaxed,
                |v| Some((v & !mask) | (value & mask)),
            )
            .expect("Inner function is infallible")
    }

    fn to_low_res_timestamp(shift: u32, time: SystemTime) -> (u64, u64) {
        // Drop the low three bits of the timestamp, for an ~8s accuracy
        let timestamp = time.duration_since(UNIX_EPOCH).unwrap().as_secs() & (Self::TS_MASK as u64);

        ((Self::TS_MASK as u64) << shift, timestamp << shift)
    }

    fn read_low_res_timestamp(&self, shift: u32) -> Option<SystemTime> {
        let read = self.0.load(std::sync::atomic::Ordering::Relaxed);

        let ts_bits = (read & ((Self::TS_MASK as u64) << shift)) >> shift;
        if ts_bits == 0 {
            None
        } else {
            Some(UNIX_EPOCH + Duration::from_secs(ts_bits | (Self::TS_ONES as u64)))
        }
    }

    /// Record a change in layer residency.
    ///
    /// Recording the event must happen while holding the layer map lock to
    /// ensure that latest-activity-threshold-based layer eviction (eviction_task.rs)
    /// can do an "imitate access" to this layer, before it observes `now-latest_activity() > threshold`.
    ///
    /// If we instead recorded the residence event with a timestamp from before grabbing the layer map lock,
    /// the following race could happen:
    ///
    /// - Compact: Write out an L1 layer from several L0 layers. This records residence event LayerCreate with the current timestamp.
    /// - Eviction: imitate access logical size calculation. This accesses the L0 layers because the L1 layer is not yet in the layer map.
    /// - Compact: Grab layer map lock, add the new L1 to layer map and remove the L0s, release layer map lock.
    /// - Eviction: observes the new L1 layer whose only activity timestamp is the LayerCreate event.
    pub(crate) fn record_residence_event_at(&self, now: SystemTime) {
        let (mask, value) = Self::to_low_res_timestamp(Self::RTIME_SHIFT, now);
        self.write_bits(mask, value);
    }

    pub(crate) fn record_residence_event(&self) {
        self.record_residence_event_at(SystemTime::now())
    }

    fn as_api_model(
        &self,
        reset: LayerAccessStatsReset,
    ) -> pageserver_api::models::LayerAccessStats {
        let ret = pageserver_api::models::LayerAccessStats {
            access_time: self
                .read_low_res_timestamp(Self::ATIME_SHIFT)
                .unwrap_or(UNIX_EPOCH),
            residence_time: self
                .read_low_res_timestamp(Self::RTIME_SHIFT)
                .unwrap_or(UNIX_EPOCH),
            visible: matches!(self.visibility(), LayerVisibilityHint::Visible),
        };
        match reset {
            LayerAccessStatsReset::NoReset => {}
            LayerAccessStatsReset::AllStats => {
                self.write_bits((Self::TS_MASK as u64) << Self::ATIME_SHIFT, 0x0);
                self.write_bits((Self::TS_MASK as u64) << Self::RTIME_SHIFT, 0x0);
            }
        }
        ret
    }

    /// Whether this layer has been accessed (excluding in [`AccessStatsBehavior::Skip`]).
    ///
    /// This indicates whether the layer has been used for some purpose that would motivate
    /// us to keep it on disk, such as for serving a getpage request.
    fn accessed(&self) -> bool {
        // Consider it accessed if the most recent access is more recent than
        // the most recent change in residence status.
        match (
            self.read_low_res_timestamp(Self::ATIME_SHIFT),
            self.read_low_res_timestamp(Self::RTIME_SHIFT),
        ) {
            (None, _) => false,
            (Some(_), None) => true,
            (Some(a), Some(r)) => a >= r,
        }
    }

    /// Helper for extracting the visibility hint from the literal value of our inner u64
    fn decode_visibility(&self, bits: u64) -> LayerVisibilityHint {
        match (bits >> Self::VISIBILITY_SHIFT) & 0x1 {
            1 => LayerVisibilityHint::Visible,
            0 => LayerVisibilityHint::Covered,
            _ => unreachable!(),
        }
    }

    pub(crate) fn visibility(&self) -> LayerVisibilityHint {
        let read = self.0.load(std::sync::atomic::Ordering::Relaxed);
        self.decode_visibility(read)
    }
}

/// Get a layer descriptor from a layer.
pub(crate) trait AsLayerDesc {
    /// Get the layer descriptor.
    fn layer_desc(&self) -> &PersistentLayerDesc;
}

pub mod tests {
    use pageserver_api::shard::TenantShardId;
    use utils::id::TimelineId;

    use super::*;

    impl From<DeltaLayerName> for PersistentLayerDesc {
        fn from(value: DeltaLayerName) -> Self {
            PersistentLayerDesc::new_delta(
                TenantShardId::from([0; 18]),
                TimelineId::from_array([0; 16]),
                value.key_range,
                value.lsn_range,
                233,
            )
        }
    }

    impl From<ImageLayerName> for PersistentLayerDesc {
        fn from(value: ImageLayerName) -> Self {
            PersistentLayerDesc::new_img(
                TenantShardId::from([0; 18]),
                TimelineId::from_array([0; 16]),
                value.key_range,
                value.lsn,
                233,
            )
        }
    }

    impl From<LayerName> for PersistentLayerDesc {
        fn from(value: LayerName) -> Self {
            match value {
                LayerName::Delta(d) => Self::from(d),
                LayerName::Image(i) => Self::from(i),
            }
        }
    }
}

/// Range wrapping newtype, which uses display to render Debug.
///
/// Useful with `Key`, which has too verbose `{:?}` for printing multiple layers.
struct RangeDisplayDebug<'a, T: std::fmt::Display>(&'a Range<T>);

impl<T: std::fmt::Display> std::fmt::Debug for RangeDisplayDebug<'_, T> {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "{}..{}", self.0.start, self.0.end)
    }
}

