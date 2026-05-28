//! `PageWriter` — buffered write path between `walingest` and `PageStore`.
//!
//! Responsibilities:
//! - Apply the 10-delta image rule via [`ImageRule`].
//! - Buffer rows in RAM until a flush trigger (time or size).
//! - On `WriteAction::ForceImage`, call out to a [`Materializer`] to
//!   walredo the chain into an image at the same LSN.
//! - Flush the buffered rows to `PageStore`.
//!
//! Phase 2b deliverable: standalone module with full tests. Wiring into
//! `Timeline::TimelineWriter::put_batch` is a separate change (Phase 3).
//!
//! The [`Materializer`] trait is the seam that lets tests inject
//! deterministic mock walredo. Production wires this to a struct that
//! looks at the in-flight buffer + queries `PageStore::read_chain` for
//! the prior chain + calls `walredo::PostgresRedoManager::request_redo`.

use std::sync::Arc;
use std::time::{Duration, Instant};

use anyhow::Result;
use bytes::Bytes;
use futures::future::BoxFuture;
use pageserver_api::key::{KEY_SIZE, Key};
use tokio::sync::Mutex;
use utils::bin_ser::BeSer;
use utils::id::{TenantId, TimelineId};
use utils::lsn::Lsn;
use wal_decoder::models::value::Value;

use crate::image_rule::{IMAGE_CHAIN_LIMIT, ImageRule, WriteAction};
use crate::page_store::{PageRow, PageStore};

/// Defaults from the project plan: flush every 5 seconds or every 100 MiB,
/// whichever comes first.
#[derive(Clone, Debug)]
pub struct FlushPolicy {
    pub max_age: Duration,
    pub max_bytes: usize,
}

impl Default for FlushPolicy {
    fn default() -> Self {
        Self {
            max_age: Duration::from_secs(5),
            max_bytes: 100 * 1024 * 1024,
        }
    }
}

/// Build a single 8 KiB page snapshot for `key` at `target_lsn`.
///
/// Production wraps walredo: the chain of prior versions (an image at
/// the bottom, deltas above, descending LSN) plus the current entry
/// being replaced is enough to reconstruct the page.
///
/// `prior_chain` contains rows at LSN strictly less than `target_lsn`,
/// sorted LSN-descending, truncated at the first `will_init=true` row
/// (the anchor — guaranteed to be present by the 10-delta invariant).
/// `current` is the `Value` whose row is being replaced with an image
/// at `target_lsn`; it must be applied on top of the chain by the
/// materializer.
///
/// Tests inject a deterministic mock that ignores the chain and
/// returns a synthetic payload.
pub trait Materializer: Send + Sync {
    fn materialize<'a>(
        &'a self,
        key: Key,
        target_lsn: Lsn,
        prior_chain: Vec<PageRow>,
        current: &'a Value,
    ) -> BoxFuture<'a, Result<Bytes>>;
}

/// Top-level write buffer + flusher for one timeline.
pub struct PageWriter {
    tenant_id: TenantId,
    timeline_id: TimelineId,
    page_store: Arc<PageStore>,
    materializer: Arc<dyn Materializer>,
    policy: FlushPolicy,
    threshold: usize,
    state: Mutex<WriterState>,
}

struct WriterState {
    rule: ImageRule,
    buffer: Vec<PageRow>,
    bytes_in_buffer: usize,
    opened_at: Option<Instant>,
    materialised: u64,
    passed_through: u64,
}

#[derive(Debug, Clone, Default)]
pub struct WriterStats {
    pub materialised: u64,
    pub passed_through: u64,
    pub bytes_in_buffer: usize,
    pub buffered_rows: usize,
}

impl PageWriter {
    pub fn new(
        tenant_id: TenantId,
        timeline_id: TimelineId,
        page_store: Arc<PageStore>,
        materializer: Arc<dyn Materializer>,
        policy: FlushPolicy,
    ) -> Self {
        Self::with_threshold(
            tenant_id,
            timeline_id,
            page_store,
            materializer,
            policy,
            IMAGE_CHAIN_LIMIT,
        )
    }

    pub fn with_threshold(
        tenant_id: TenantId,
        timeline_id: TimelineId,
        page_store: Arc<PageStore>,
        materializer: Arc<dyn Materializer>,
        policy: FlushPolicy,
        image_threshold: usize,
    ) -> Self {
        Self {
            tenant_id,
            timeline_id,
            page_store,
            materializer,
            policy,
            threshold: image_threshold,
            state: Mutex::new(WriterState {
                rule: ImageRule::new(image_threshold),
                buffer: Vec::new(),
                bytes_in_buffer: 0,
                opened_at: None,
                materialised: 0,
                passed_through: 0,
            }),
        }
    }

    /// Apply the 10-delta rule to a batch of entries and append them to
    /// the in-RAM buffer. Does not flush.
    pub async fn put(&self, entries: &[(Key, Lsn, Value)]) -> Result<()> {
        for (key, lsn, value) in entries {
            // Lock-decide-unlock-await-relock so the materializer can be
            // slow without holding the writer mutex.
            let action = {
                let mut s = self.state.lock().await;
                if s.opened_at.is_none() {
                    s.opened_at = Some(Instant::now());
                }
                s.rule.decide(*key, value)
            };
            let row = match action {
                WriteAction::Passthrough => build_passthrough_row(*key, *lsn, value)?,
                WriteAction::ForceImage => {
                    // Gather the prior chain (rows at LSN < target_lsn).
                    // The new entry hasn't been buffered yet, so
                    // read_chain at lsn returns rows strictly < lsn
                    // (the unique-(key,lsn) WAL invariant prevents an
                    // exact match from existing already).
                    let prior_chain = self
                        .read_chain(*key, *lsn, IMAGE_CHAIN_LIMIT)
                        .await?;
                    let bytes = self
                        .materializer
                        .materialize(*key, *lsn, prior_chain, value)
                        .await?;
                    build_image_row(*key, *lsn, bytes)
                }
            };
            let row_size = row_size(&row);
            let mut s = self.state.lock().await;
            s.bytes_in_buffer += row_size;
            match action {
                WriteAction::Passthrough => s.passed_through += 1,
                WriteAction::ForceImage => s.materialised += 1,
            }
            s.buffer.push(row);
        }
        Ok(())
    }

    /// Read the chain for `key` at `target_lsn`, blending in-RAM
    /// buffered rows on top of PageStore's persisted chain.
    ///
    /// Phase 3 read path uses this so the getpage handler sees writes
    /// that haven't yet been flushed to YB. The result is sorted
    /// strictly LSN descending and contains at most `limit` rows.
    pub async fn read_chain(
        &self,
        key: Key,
        target_lsn: Lsn,
        limit: usize,
    ) -> Result<Vec<PageRow>> {
        let key_bytes = key_to_bytes(key);
        let mut buffered: Vec<PageRow> = {
            let s = self.state.lock().await;
            s.buffer
                .iter()
                .filter(|r| r.key == key_bytes && r.lsn <= target_lsn)
                .cloned()
                .collect()
        };
        let persisted = self
            .page_store
            .read_chain(self.tenant_id, self.timeline_id, &key_bytes, target_lsn, limit)
            .await?;
        buffered.extend(persisted);
        buffered.sort_by(|a, b| b.lsn.cmp(&a.lsn));
        // Stop at the first will_init row to honour the 10-delta read
        // contract — anything older is shadowed by the anchor.
        let mut cap = buffered.len();
        for (i, row) in buffered.iter().enumerate() {
            if row.will_init {
                cap = i + 1;
                break;
            }
        }
        buffered.truncate(cap.min(limit));
        Ok(buffered)
    }

    /// Drain the buffer into one `INSERT` against `PageStore`. No-op if
    /// the buffer is empty. Resets the policy timer.
    pub async fn flush(&self) -> Result<()> {
        let rows = {
            let mut s = self.state.lock().await;
            s.bytes_in_buffer = 0;
            s.opened_at = None;
            std::mem::take(&mut s.buffer)
        };
        if rows.is_empty() {
            return Ok(());
        }
        self.page_store
            .put_pages(self.tenant_id, self.timeline_id, &rows)
            .await
    }

    /// Returns true if either size or age trigger has fired.
    pub async fn should_flush(&self) -> bool {
        let s = self.state.lock().await;
        if s.bytes_in_buffer >= self.policy.max_bytes {
            return true;
        }
        if let Some(at) = s.opened_at
            && at.elapsed() >= self.policy.max_age
        {
            return true;
        }
        false
    }

    pub async fn stats(&self) -> WriterStats {
        let s = self.state.lock().await;
        WriterStats {
            materialised: s.materialised,
            passed_through: s.passed_through,
            bytes_in_buffer: s.bytes_in_buffer,
            buffered_rows: s.buffer.len(),
        }
    }

    pub fn threshold(&self) -> usize {
        self.threshold
    }
}

fn key_to_bytes(key: Key) -> Bytes {
    let mut buf = vec![0u8; KEY_SIZE];
    key.write_to_byte_slice(&mut buf);
    Bytes::from(buf)
}

fn build_passthrough_row(key: Key, lsn: Lsn, value: &Value) -> Result<PageRow> {
    let (is_image, will_init, payload) = match value {
        Value::Image(b) => (true, true, b.clone()),
        Value::WalRecord(rec) => {
            let will_init = rec.will_init();
            let bytes = Bytes::from(rec.ser()?);
            (false, will_init, bytes)
        }
    };
    Ok(PageRow {
        key: key_to_bytes(key),
        lsn,
        is_image,
        will_init,
        payload,
    })
}

fn build_image_row(key: Key, lsn: Lsn, payload: Bytes) -> PageRow {
    PageRow {
        key: key_to_bytes(key),
        lsn,
        is_image: true,
        will_init: true,
        payload,
    }
}

fn row_size(r: &PageRow) -> usize {
    // Approximate on-the-wire size. Doesn't have to be exact — used only
    // for the flush trigger.
    r.payload.len() + r.key.len() + 16
}

#[cfg(test)]
mod tests {
    use super::*;

    use std::sync::atomic::{AtomicU64, Ordering};

    use bytes::Bytes;
    use futures::FutureExt;
    use remote_storage::YbStoreConfig;
    use utils::id::{TenantId, TimelineId};
    use wal_decoder::models::record::NeonWalRecord;

    use crate::page_store::{PageStore, PageStoreConfig};

    // --- mock materializer -----------------------------------------------

    /// Deterministic mock: returns "image@{lsn}" as the materialised
    /// payload, and counts calls.
    struct MockMaterializer {
        calls: AtomicU64,
    }

    impl MockMaterializer {
        fn new() -> Self {
            Self {
                calls: AtomicU64::new(0),
            }
        }
        fn calls(&self) -> u64 {
            self.calls.load(Ordering::Relaxed)
        }
    }

    impl Materializer for MockMaterializer {
        fn materialize<'a>(
            &'a self,
            _key: Key,
            target_lsn: Lsn,
            _prior_chain: Vec<PageRow>,
            _current: &'a Value,
        ) -> BoxFuture<'a, Result<Bytes>> {
            self.calls.fetch_add(1, Ordering::Relaxed);
            async move { Ok(Bytes::from(format!("image@{}", target_lsn.0))) }.boxed()
        }
    }

    // --- env helpers (mirrored from page_store::tests so the writer
    //     tests don't pull in private items) -------------------------------

    fn test_yb_config() -> YbStoreConfig {
        YbStoreConfig {
            host: std::env::var("YB_TEST_HOST").unwrap_or_else(|_| "127.0.0.1".to_string()),
            port: std::env::var("YB_TEST_PORT")
                .ok()
                .and_then(|s| s.parse().ok())
                .unwrap_or(5433),
            user: std::env::var("YB_TEST_USER").unwrap_or_else(|_| "yugabyte".to_string()),
            password_env: None,
            dbname: std::env::var("YB_TEST_DB").unwrap_or_else(|_| "yb_storage".to_string()),
            table_name: "pages".to_string(),
            prefix: Some("test/".to_string()),
            sslmode: None,
            sslrootcert: None,
            concurrency_limit: std::num::NonZeroUsize::new(8).unwrap(),
        }
    }

    async fn make_store(suffix: &str) -> (Arc<PageStore>, String) {
        let schema = format!("pw_test_{suffix}");
        let cfg_for_drop = PageStoreConfig {
            yb: test_yb_config(),
            schema: schema.clone(),
        };
        let pre = PageStore::connect(cfg_for_drop, "pw_test_setup")
            .await
            .expect("connect to local YB");
        let drop_sql = format!("DROP SCHEMA IF EXISTS \"{schema}\" CASCADE");
        let _ = pre.test_only_execute(&drop_sql).await;
        let store = PageStore::connect(
            PageStoreConfig {
                yb: test_yb_config(),
                schema: schema.clone(),
            },
            "pw_test",
        )
        .await
        .expect("connect to local YB");
        store.ensure_schema().await.expect("ensure_schema");
        (Arc::new(store), schema)
    }

    async fn drop_schema(store: &PageStore, schema: &str) {
        let _ = store
            .test_only_execute(&format!("DROP SCHEMA IF EXISTS \"{schema}\" CASCADE"))
            .await;
    }

    fn k(n: u32) -> Key {
        Key {
            field1: 0,
            field2: 0,
            field3: 0,
            field4: 0,
            field5: 0,
            field6: n,
        }
    }

    fn image() -> Value {
        Value::Image(Bytes::from_static(b"natural-image-bytes"))
    }

    fn delta() -> Value {
        Value::WalRecord(NeonWalRecord::ClearVisibilityMapFlags {
            new_heap_blkno: Some(1),
            old_heap_blkno: None,
            flags: 0x01,
        })
    }

    fn writer(
        store: Arc<PageStore>,
        threshold: usize,
        policy: FlushPolicy,
    ) -> (PageWriter, Arc<MockMaterializer>, TenantId, TimelineId) {
        let m = Arc::new(MockMaterializer::new());
        let t = TenantId::generate();
        let tl = TimelineId::generate();
        (
            PageWriter::with_threshold(t, tl, store, m.clone() as Arc<dyn Materializer>, policy, threshold),
            m,
            t,
            tl,
        )
    }

    // --- tests ---------------------------------------------------------------

    #[tokio::test]
    async fn empty_flush_is_noop() {
        let (store, schema) = make_store("empty_flush").await;
        let (w, _m, _t, _tl) = writer(store.clone(), 10, FlushPolicy::default());
        w.flush().await.unwrap();
        let stats = w.stats().await;
        assert_eq!(stats.buffered_rows, 0);
        drop_schema(&store, &schema).await;
    }

    #[tokio::test]
    async fn first_image_passes_through_no_materialise() {
        let (store, schema) = make_store("first_image").await;
        let (w, m, t, tl) = writer(store.clone(), 10, FlushPolicy::default());
        w.put(&[(k(1), Lsn(10), image())]).await.unwrap();
        let stats = w.stats().await;
        assert_eq!(stats.passed_through, 1);
        assert_eq!(stats.materialised, 0);
        assert_eq!(m.calls(), 0);
        w.flush().await.unwrap();
        // Re-read from PageStore: one image row.
        let chain = store
            .read_chain(t, tl, &key_to_bytes(k(1)), Lsn(100), 10)
            .await
            .unwrap();
        assert_eq!(chain.len(), 1);
        assert!(chain[0].is_image);
        assert!(chain[0].will_init);
        assert_eq!(chain[0].payload.as_ref(), b"natural-image-bytes");
        drop_schema(&store, &schema).await;
    }

    #[tokio::test]
    async fn first_delta_triggers_materialise() {
        let (store, schema) = make_store("first_delta").await;
        let (w, m, t, tl) = writer(store.clone(), 10, FlushPolicy::default());
        w.put(&[(k(1), Lsn(10), delta())]).await.unwrap();
        assert_eq!(m.calls(), 1);
        w.flush().await.unwrap();
        let chain = store
            .read_chain(t, tl, &key_to_bytes(k(1)), Lsn(100), 10)
            .await
            .unwrap();
        assert_eq!(chain.len(), 1);
        assert!(chain[0].is_image);
        assert_eq!(chain[0].payload.as_ref(), b"image@10");
        drop_schema(&store, &schema).await;
    }

    #[tokio::test]
    async fn ten_deltas_after_image_force_one_materialise() {
        let (store, schema) = make_store("ten_deltas").await;
        let (w, m, t, tl) = writer(store.clone(), 10, FlushPolicy::default());
        let mut entries: Vec<(Key, Lsn, Value)> = vec![(k(1), Lsn(10), image())];
        // 10 plain deltas, LSNs 20, 30, ..., 110.
        for i in 1..=10 {
            entries.push((k(1), Lsn(10 + 10 * i), delta()));
        }
        w.put(&entries).await.unwrap();
        // Image + 9 deltas pass through; the 10th delta force-materialises.
        let stats = w.stats().await;
        assert_eq!(stats.materialised, 1);
        assert_eq!(stats.passed_through, 10);
        assert_eq!(m.calls(), 1);
        w.flush().await.unwrap();
        // Chain at LSN 1000: 11 rows total — but the *latest* 10 must
        // include an image. read_chain LIMIT 10 returns 10 most recent.
        let chain = store
            .read_chain(t, tl, &key_to_bytes(k(1)), Lsn(1000), 10)
            .await
            .unwrap();
        assert_eq!(chain.len(), 10);
        // Top row (highest LSN) is the materialised image.
        assert_eq!(chain[0].lsn, Lsn(110));
        assert!(chain[0].is_image);
        assert!(chain[0].will_init);
        assert_eq!(chain[0].payload.as_ref(), b"image@110");
        // Below it: 9 plain deltas at lsns 100, 90, ..., 20.
        for (i, row) in chain[1..].iter().enumerate() {
            assert_eq!(row.lsn, Lsn(100 - 10 * i as u64));
            assert!(!row.is_image);
            assert!(!row.will_init);
        }
        drop_schema(&store, &schema).await;
    }

    #[tokio::test]
    async fn limit_invariant_holds_for_long_workload() {
        // The core correctness property: after writing 100 deltas to one
        // key, ANY read with LIMIT 10 must include a will_init=true row.
        let (store, schema) = make_store("invariant").await;
        let (w, _m, t, tl) = writer(store.clone(), 10, FlushPolicy::default());
        let mut entries: Vec<(Key, Lsn, Value)> = vec![(k(1), Lsn(10), image())];
        for i in 1..=100 {
            entries.push((k(1), Lsn(10 + 10 * i), delta()));
        }
        w.put(&entries).await.unwrap();
        w.flush().await.unwrap();
        // Scan target_lsn values 10..=1010 in 10-step increments.
        for target in (10..=1010).step_by(10) {
            let chain = store
                .read_chain(t, tl, &key_to_bytes(k(1)), Lsn(target), 10)
                .await
                .unwrap();
            assert!(!chain.is_empty(), "target_lsn={target} empty chain");
            assert!(chain.len() <= 10);
            let has_anchor = chain.iter().any(|r| r.will_init);
            assert!(
                has_anchor,
                "target_lsn={target} chain lacks will_init=true row: {chain:?}"
            );
        }
        drop_schema(&store, &schema).await;
    }

    #[tokio::test]
    async fn flush_emits_a_single_insert_round_trip() {
        let (store, schema) = make_store("flush_one_rt").await;
        let (w, _m, t, tl) = writer(store.clone(), 10, FlushPolicy::default());
        let entries: Vec<(Key, Lsn, Value)> = (0..50)
            .map(|i| (k(i), Lsn(10), image()))
            .collect();
        w.put(&entries).await.unwrap();
        assert_eq!(w.stats().await.buffered_rows, 50);
        w.flush().await.unwrap();
        assert_eq!(w.stats().await.buffered_rows, 0);
        let listed = (0..50)
            .map(|i| key_to_bytes(k(i)))
            .collect::<Vec<_>>();
        let chains = store
            .read_chains(t, tl, &listed, Lsn(100), 10)
            .await
            .unwrap();
        assert_eq!(chains.len(), 50);
        for (_k, chain) in chains.iter() {
            assert_eq!(chain.len(), 1);
            assert!(chain[0].is_image);
        }
        drop_schema(&store, &schema).await;
    }

    #[tokio::test]
    async fn should_flush_on_size() {
        let (store, schema) = make_store("flush_size").await;
        let policy = FlushPolicy {
            max_age: Duration::from_secs(3600),
            max_bytes: 200, // tiny — one entry will trip it
        };
        let (w, _m, _t, _tl) = writer(store.clone(), 10, policy);
        assert!(!w.should_flush().await);
        // 1 image row, payload "natural-image-bytes" (19 B). Per row_size():
        // 19 (payload) + 18 (key bytes) + 16 (overhead) = 53 bytes.
        // Need >= 200 bytes -> 4 entries.
        for i in 0..4 {
            w.put(&[(k(i), Lsn(10), image())]).await.unwrap();
        }
        assert!(
            w.should_flush().await,
            "stats: {:?}",
            w.stats().await
        );
        drop_schema(&store, &schema).await;
    }

    #[tokio::test]
    async fn should_flush_on_age() {
        let (store, schema) = make_store("flush_age").await;
        let policy = FlushPolicy {
            max_age: Duration::from_millis(100),
            max_bytes: 1_000_000_000,
        };
        let (w, _m, _t, _tl) = writer(store.clone(), 10, policy);
        w.put(&[(k(1), Lsn(10), image())]).await.unwrap();
        assert!(!w.should_flush().await);
        tokio::time::sleep(Duration::from_millis(150)).await;
        assert!(w.should_flush().await);
        // After a flush, the age clock resets — should_flush is false.
        w.flush().await.unwrap();
        assert!(!w.should_flush().await);
        drop_schema(&store, &schema).await;
    }

    #[tokio::test]
    async fn keys_independent_in_materialisation_counting() {
        let (store, schema) = make_store("keys_indep").await;
        let (w, m, _t, _tl) = writer(store.clone(), 10, FlushPolicy::default());
        // Anchor 5 different keys with images.
        let entries: Vec<(Key, Lsn, Value)> = (0..5)
            .map(|i| (k(i), Lsn(10), image()))
            .collect();
        w.put(&entries).await.unwrap();
        assert_eq!(m.calls(), 0);
        // Now 9 deltas for k(0). No force-image yet.
        for i in 1..=9 {
            w.put(&[(k(0), Lsn(10 + 10 * i), delta())]).await.unwrap();
        }
        assert_eq!(m.calls(), 0);
        // 10th delta on k(0): force-image.
        w.put(&[(k(0), Lsn(110), delta())]).await.unwrap();
        assert_eq!(m.calls(), 1);
        // k(1) still has counter=0; one delta passes through.
        w.put(&[(k(1), Lsn(20), delta())]).await.unwrap();
        assert_eq!(m.calls(), 1);
        drop_schema(&store, &schema).await;
    }

    /// Recording mock: captures every (target_lsn, chain LSNs, current
    /// kind) the writer hands it. Used to prove the materializer contract.
    struct RecordingMaterializer {
        calls: tokio::sync::Mutex<Vec<RecordedCall>>,
    }
    #[derive(Debug, Clone)]
    struct RecordedCall {
        target_lsn: Lsn,
        chain_lsns: Vec<Lsn>,
        chain_has_anchor: bool,
        current_is_image: bool,
        current_will_init: bool,
    }
    impl RecordingMaterializer {
        fn new() -> Self {
            Self {
                calls: tokio::sync::Mutex::new(Vec::new()),
            }
        }
    }
    impl Materializer for RecordingMaterializer {
        fn materialize<'a>(
            &'a self,
            _key: Key,
            target_lsn: Lsn,
            prior_chain: Vec<PageRow>,
            current: &'a Value,
        ) -> BoxFuture<'a, Result<Bytes>> {
            let recorded = RecordedCall {
                target_lsn,
                chain_lsns: prior_chain.iter().map(|r| r.lsn).collect(),
                chain_has_anchor: prior_chain.iter().any(|r| r.will_init),
                current_is_image: current.is_image(),
                current_will_init: current.will_init(),
            };
            async move {
                self.calls.lock().await.push(recorded);
                Ok(Bytes::from(format!("img@{}", target_lsn.0)))
            }
            .boxed()
        }
    }

    #[tokio::test]
    async fn materializer_receives_prior_chain_and_current() {
        let (store, schema) = make_store("mat_contract").await;
        let m = Arc::new(RecordingMaterializer::new());
        let t = TenantId::generate();
        let tl = TimelineId::generate();
        let w = PageWriter::with_threshold(
            t,
            tl,
            store.clone(),
            m.clone() as Arc<dyn Materializer>,
            FlushPolicy::default(),
            10,
        );

        // Build a 10-row chain: image then 9 deltas; the 10th delta
        // should ForceImage and the mock should receive the 9-delta+image
        // chain plus the current delta value.
        let mut entries: Vec<(Key, Lsn, Value)> = vec![(k(1), Lsn(10), image())];
        for i in 1..=9 {
            entries.push((k(1), Lsn(10 + 10 * i), delta()));
        }
        w.put(&entries).await.unwrap();
        // No materialise yet — image + 9 deltas all pass through.
        assert_eq!(m.calls.lock().await.len(), 0);

        // 10th delta forces materialisation at LSN 110.
        w.put(&[(k(1), Lsn(110), delta())]).await.unwrap();
        let calls = m.calls.lock().await.clone();
        assert_eq!(calls.len(), 1);
        let c = &calls[0];
        assert_eq!(c.target_lsn, Lsn(110));
        // prior_chain is rows < 110: lsns 10, 20, ..., 100 (descending).
        // Truncated at the first will_init=true row (the image at LSN 10).
        // Should be 10 rows, ending at LSN 10.
        assert_eq!(c.chain_lsns.first(), Some(&Lsn(100)));
        assert_eq!(c.chain_lsns.last(), Some(&Lsn(10)));
        assert!(c.chain_has_anchor, "prior chain must include anchor");
        assert!(!c.current_is_image);
        assert!(!c.current_will_init);
        drop_schema(&store, &schema).await;
    }

    #[tokio::test]
    async fn read_chain_blends_buffered_and_persisted() {
        // Write 5 entries, flush, write 3 more (still buffered), then
        // read — should see 8 rows in the correct LSN-desc order.
        let (store, schema) = make_store("blend").await;
        let (w, _m, _t, _tl) = writer(store.clone(), 10, FlushPolicy::default());
        let first_batch: Vec<(Key, Lsn, Value)> = (0..5)
            .map(|i| (k(1), Lsn(10 + 10 * i), if i == 0 { image() } else { delta() }))
            .collect();
        w.put(&first_batch).await.unwrap();
        w.flush().await.unwrap();
        let second_batch: Vec<(Key, Lsn, Value)> = (5..8)
            .map(|i| (k(1), Lsn(10 + 10 * i), delta()))
            .collect();
        w.put(&second_batch).await.unwrap();
        assert!(w.stats().await.buffered_rows > 0);
        let chain = w.read_chain(k(1), Lsn(1000), 10).await.unwrap();
        // 1 image at LSN 10 + 7 deltas at LSNs 20, 30, ..., 80.
        assert_eq!(chain.len(), 8);
        let mut prev = u64::MAX;
        for row in &chain {
            assert!(row.lsn.0 < prev, "LSNs must descend; got {row:?}");
            prev = row.lsn.0;
        }
        // The anchor is at the bottom (LSN 10).
        assert_eq!(chain.last().unwrap().lsn, Lsn(10));
        assert!(chain.last().unwrap().will_init);
        drop_schema(&store, &schema).await;
    }

    #[tokio::test]
    async fn read_chain_truncates_at_will_init_anchor() {
        // After a will_init delta resets the chain, older rows must NOT
        // appear in the read result — the read path stops at the anchor.
        let (store, schema) = make_store("trunc_at_anchor").await;
        let (w, _m, _t, _tl) = writer(store.clone(), 10, FlushPolicy::default());
        w.put(&[(k(1), Lsn(10), image())]).await.unwrap();
        for i in 1..=3 {
            w.put(&[(k(1), Lsn(10 + 10 * i), delta())]).await.unwrap();
        }
        // A will_init delta at LSN 50 acts as a new anchor.
        let init = Value::WalRecord(NeonWalRecord::Postgres {
            will_init: true,
            rec: Bytes::from_static(b"init"),
        });
        w.put(&[(k(1), Lsn(50), init)]).await.unwrap();
        for i in 6..=8 {
            w.put(&[(k(1), Lsn(10 + 10 * i), delta())]).await.unwrap();
        }
        w.flush().await.unwrap();
        let chain = w.read_chain(k(1), Lsn(1000), 10).await.unwrap();
        // 4 rows: deltas at 90/80/70, anchor at 50. Older rows (40/30/20/10)
        // are shadowed by the anchor.
        assert_eq!(chain.len(), 4);
        assert_eq!(chain.first().unwrap().lsn, Lsn(90));
        assert_eq!(chain.last().unwrap().lsn, Lsn(50));
        assert!(chain.last().unwrap().will_init);
        assert!(chain.iter().all(|r| r.lsn >= Lsn(50)));
        drop_schema(&store, &schema).await;
    }

    #[tokio::test]
    async fn passthrough_payload_is_serialised_wal_record() {
        // Sanity: a passthrough delta stores `NeonWalRecord::ser()` bytes,
        // and `NeonWalRecord::des(payload)` round-trips back to the
        // original record.
        let (store, schema) = make_store("ser_rt").await;
        let (w, _m, t, tl) = writer(store.clone(), 10, FlushPolicy::default());
        let rec = NeonWalRecord::ClearVisibilityMapFlags {
            new_heap_blkno: Some(42),
            old_heap_blkno: Some(43),
            flags: 0x07,
        };
        // Anchor with an image so the next delta passes through.
        w.put(&[(k(1), Lsn(10), image())]).await.unwrap();
        w.put(&[(k(1), Lsn(20), Value::WalRecord(rec.clone()))])
            .await
            .unwrap();
        w.flush().await.unwrap();
        let chain = store
            .read_chain(t, tl, &key_to_bytes(k(1)), Lsn(100), 10)
            .await
            .unwrap();
        assert_eq!(chain.len(), 2);
        let delta_row = chain
            .iter()
            .find(|r| !r.is_image)
            .expect("one delta row");
        assert!(!delta_row.will_init);
        let decoded = NeonWalRecord::des(&delta_row.payload).expect("ser/des round-trip");
        assert_eq!(decoded, rec);
        drop_schema(&store, &schema).await;
    }
}
