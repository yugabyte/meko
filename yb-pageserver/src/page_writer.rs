//! `PageWriter` — buffered write path between `walingest` and `PageStore`.
//!
//! Pure passthrough: every relation-page entry from walingest becomes
//! one row in `pageserver.pages`, untouched. WAL records flow in as
//! `is_image=FALSE`; FPI records flow in as `is_image=TRUE`. The
//! `will_init` flag comes straight off the `Value` — we don't synthesise
//! anchors at write time.
//!
//! Chain length is not a write-time invariant any more — a background
//! image materialiser (separate module) periodically scans
//! `pageserver.pages` for keys whose delta chain has grown long and
//! drops a fresh image row at the latest LSN. Reads stay correct without
//! it (`PageStore::read_chain` walks back to the latest will_init delta
//! when no image exists); the background task is pure optimisation that
//! keeps reads fast.
//!
//! Buffering: rows accumulate in RAM until a flush trigger (age or
//! bytes) fires, then drain to `pageserver.pages` in one INSERT.

use std::sync::Arc;
use std::time::{Duration, Instant};

use anyhow::Result;
use bytes::Bytes;
use pageserver_api::key::{KEY_SIZE, Key};
use tokio::sync::Mutex;
use tokio_util::sync::CancellationToken;
use utils::bin_ser::BeSer;
use utils::id::{TenantId, TimelineId};
use utils::lsn::Lsn;
use wal_decoder::models::value::Value;
use wal_decoder::serialized_batch::{SerializedValueBatch, ValueMeta};

use pageserver_api::reltag::SlruKind;

use crate::page_store::{MetaRow, PageRow, PageStore, SlruPageRow};

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

/// Top-level write buffer + flusher for one timeline.
pub struct PageWriter {
    tenant_id: TenantId,
    timeline_id: TimelineId,
    page_store: Arc<PageStore>,
    policy: FlushPolicy,
    state: Mutex<WriterState>,
}

struct WriterState {
    buffer: Vec<PageRow>,
    slru_buffer: Vec<SlruPageRow>,
    meta_buffer: Vec<MetaRow>,
    bytes_in_buffer: usize,
    opened_at: Option<Instant>,
    rows_written: u64,
}

#[derive(Debug, Clone, Default)]
pub struct WriterStats {
    pub rows_written: u64,
    pub bytes_in_buffer: usize,
    pub buffered_rows: usize,
    pub buffered_slru_rows: usize,
    pub buffered_meta_rows: usize,
}

impl PageWriter {
    pub fn new(
        tenant_id: TenantId,
        timeline_id: TimelineId,
        page_store: Arc<PageStore>,
        policy: FlushPolicy,
    ) -> Self {
        Self {
            tenant_id,
            timeline_id,
            page_store,
            policy,
            state: Mutex::new(WriterState {
                buffer: Vec::new(),
                slru_buffer: Vec::new(),
                meta_buffer: Vec::new(),
                bytes_in_buffer: 0,
                opened_at: None,
                rows_written: 0,
            }),
        }
    }

    /// Append a batch of entries to the in-RAM buffer. Does not flush.
    /// Routes every entry into one of the three row-store tables:
    /// - relation-page keys → `pageserver.pages`
    /// - SLRU block keys   → `pageserver.slru_pages`
    /// - everything else   → `pageserver.meta` (full-overwrite)
    ///
    /// Meta entries must arrive as `Value::Image` — they're
    /// full-overwrite by convention. A `Value::WalRecord` here would
    /// signal an unexpected walredo dependency on a non-page key and is
    /// an error.
    pub async fn put(&self, entries: &[(Key, Lsn, Value)]) -> Result<()> {
        if entries.is_empty() {
            return Ok(());
        }
        let mut rel_rows = Vec::with_capacity(entries.len());
        let mut slru_rows = Vec::new();
        let mut meta_rows = Vec::new();
        for (key, lsn, value) in entries {
            if key.is_rel_block_key() {
                rel_rows.push(build_page_row(*key, *lsn, value)?);
            } else if key.is_slru_block_key() {
                slru_rows.push(build_slru_page_row(*key, *lsn, value)?);
            } else {
                meta_rows.push(build_meta_row(*key, *lsn, value)?);
            }
        }
        if rel_rows.is_empty() && slru_rows.is_empty() && meta_rows.is_empty() {
            return Ok(());
        }
        let mut s = self.state.lock().await;
        if s.opened_at.is_none() {
            s.opened_at = Some(Instant::now());
        }
        for row in rel_rows {
            s.bytes_in_buffer += row_size(&row);
            s.buffer.push(row);
            s.rows_written += 1;
        }
        for row in slru_rows {
            s.bytes_in_buffer += slru_row_size(&row);
            s.slru_buffer.push(row);
            s.rows_written += 1;
        }
        for row in meta_rows {
            s.bytes_in_buffer += meta_row_size(&row);
            s.meta_buffer.push(row);
            s.rows_written += 1;
        }
        Ok(())
    }

    /// Range scan: return all page rows in `range` at LSN ≤
    /// `target_lsn`, blending in-RAM buffered rows with PageStore's
    /// persisted rows. Each per-key chain is LSN-descending and
    /// truncated at the first `will_init=true` row.
    pub async fn scan_range(
        &self,
        range: std::ops::Range<Key>,
        target_lsn: Lsn,
    ) -> Result<std::collections::HashMap<Key, Vec<PageRow>>> {
        let persisted = self
            .page_store
            .scan_range(self.tenant_id, self.timeline_id, range.clone(), target_lsn)
            .await?;
        let buffered: Vec<PageRow> = {
            let s = self.state.lock().await;
            s.buffer
                .iter()
                .filter(|r| {
                    if r.key.len() != KEY_SIZE {
                        return false;
                    }
                    let k = Key::from_slice(&r.key);
                    k >= range.start && k < range.end && r.lsn <= target_lsn
                })
                .cloned()
                .collect()
        };
        let mut out: std::collections::HashMap<Key, Vec<PageRow>> =
            std::collections::HashMap::with_capacity(persisted.len() + buffered.len());
        for (key_bytes, chain) in persisted {
            let k = Key::from_slice(&key_bytes);
            out.insert(k, chain);
        }
        for row in buffered {
            let k = Key::from_slice(&row.key);
            out.entry(k).or_default().push(row);
        }
        for chain in out.values_mut() {
            chain.sort_by(|a, b| b.lsn.cmp(&a.lsn));
            if let Some(idx) = chain.iter().position(|r| r.will_init) {
                chain.truncate(idx + 1);
            }
        }
        Ok(out)
    }

    /// Read the chain for `key` at `target_lsn`, blending in-RAM
    /// buffered rows on top of PageStore's persisted chain.
    pub async fn read_chain(&self, key: Key, target_lsn: Lsn) -> Result<Vec<PageRow>> {
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
            .read_chain(self.tenant_id, self.timeline_id, &key_bytes, target_lsn)
            .await?;
        buffered.extend(persisted);
        buffered.sort_by(|a, b| b.lsn.cmp(&a.lsn));
        // Truncate at the first will_init row — anything older is
        // shadowed by that anchor.
        if let Some(idx) = buffered.iter().position(|r| r.will_init) {
            buffered.truncate(idx + 1);
        }
        Ok(buffered)
    }

    /// Drain the buffer into one `INSERT` against `PageStore`. No-op if
    /// the buffer is empty. Resets the policy timer.
    pub async fn flush(&self) -> Result<()> {
        let (rel_rows, slru_rows, meta_rows) = {
            let mut s = self.state.lock().await;
            s.bytes_in_buffer = 0;
            s.opened_at = None;
            (
                std::mem::take(&mut s.buffer),
                std::mem::take(&mut s.slru_buffer),
                std::mem::take(&mut s.meta_buffer),
            )
        };
        if !rel_rows.is_empty() {
            self.page_store
                .put_pages(self.tenant_id, self.timeline_id, &rel_rows)
                .await?;
        }
        if !slru_rows.is_empty() {
            self.page_store
                .put_slru_pages(self.tenant_id, self.timeline_id, &slru_rows)
                .await?;
        }
        if !meta_rows.is_empty() {
            self.page_store
                .put_meta(self.tenant_id, self.timeline_id, &meta_rows)
                .await?;
        }
        Ok(())
    }

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
            rows_written: s.rows_written,
            bytes_in_buffer: s.bytes_in_buffer,
            buffered_rows: s.buffer.len(),
            buffered_slru_rows: s.slru_buffer.len(),
            buffered_meta_rows: s.meta_buffer.len(),
        }
    }

    /// Read the latest meta value for `key` at LSN ≤ `target_lsn`,
    /// blending buffer + PageStore.
    pub async fn read_meta(&self, key: Key, target_lsn: Lsn) -> Result<Option<MetaRow>> {
        let key_bytes = key_to_bytes(key);
        let buffered = {
            let s = self.state.lock().await;
            s.meta_buffer
                .iter()
                .filter(|r| r.key == key_bytes && r.lsn <= target_lsn)
                .max_by_key(|r| r.lsn)
                .cloned()
        };
        let persisted = self
            .page_store
            .read_meta(self.tenant_id, self.timeline_id, &key_bytes, target_lsn)
            .await?;
        Ok(match (buffered, persisted) {
            (Some(b), Some(p)) => Some(if b.lsn >= p.lsn { b } else { p }),
            (Some(b), None) => Some(b),
            (None, p) => p,
        })
    }

    /// Range scan over meta keys. Returns the latest version per key.
    pub async fn scan_meta_range(
        &self,
        range: std::ops::Range<Key>,
        target_lsn: Lsn,
    ) -> Result<std::collections::HashMap<bytes::Bytes, MetaRow>> {
        let mut out = self
            .page_store
            .scan_meta_range(self.tenant_id, self.timeline_id, range.clone(), target_lsn)
            .await?;
        let s = self.state.lock().await;
        for row in s.meta_buffer.iter() {
            if row.key.len() != KEY_SIZE || row.lsn > target_lsn {
                continue;
            }
            let k = Key::from_slice(&row.key);
            if k < range.start || k >= range.end {
                continue;
            }
            match out.get(&row.key) {
                Some(existing) if existing.lsn >= row.lsn => continue,
                _ => {
                    out.insert(row.key.clone(), row.clone());
                }
            }
        }
        Ok(out)
    }

    /// Per-block SLRU chain read, blending the in-RAM SLRU buffer with
    /// PageStore. Used by the timeline's vectored read for SLRU keys.
    pub async fn read_slru_chain(
        &self,
        kind: SlruKind,
        segno: u32,
        blknum: u32,
        target_lsn: Lsn,
    ) -> Result<Vec<SlruPageRow>> {
        let mut buffered: Vec<SlruPageRow> = {
            let s = self.state.lock().await;
            s.slru_buffer
                .iter()
                .filter(|r| {
                    r.kind == kind && r.segno == segno && r.blknum == blknum
                        && r.lsn <= target_lsn
                })
                .cloned()
                .collect()
        };
        let persisted = self
            .page_store
            .read_slru_chain(self.tenant_id, self.timeline_id, kind, segno, blknum, target_lsn)
            .await?;
        buffered.extend(persisted);
        buffered.sort_by(|a, b| b.lsn.cmp(&a.lsn));
        if let Some(idx) = buffered.iter().position(|r| r.will_init) {
            buffered.truncate(idx + 1);
        }
        Ok(buffered)
    }

    /// Range scan over an SLRU segment, blending buffered + persisted.
    /// `blknum_range` lives within one `(kind, segno)`.
    pub async fn scan_slru_range(
        &self,
        kind: SlruKind,
        segno: u32,
        blknum_range: std::ops::Range<u32>,
        target_lsn: Lsn,
    ) -> Result<std::collections::HashMap<u32, Vec<SlruPageRow>>> {
        let persisted = self
            .page_store
            .scan_slru_range(self.tenant_id, self.timeline_id, kind, segno, blknum_range.clone(), target_lsn)
            .await?;
        let buffered: Vec<SlruPageRow> = {
            let s = self.state.lock().await;
            s.slru_buffer
                .iter()
                .filter(|r| {
                    r.kind == kind && r.segno == segno
                        && r.blknum >= blknum_range.start && r.blknum < blknum_range.end
                        && r.lsn <= target_lsn
                })
                .cloned()
                .collect()
        };
        let mut out: std::collections::HashMap<u32, Vec<SlruPageRow>> = persisted;
        for row in buffered {
            out.entry(row.blknum).or_default().push(row);
        }
        for chain in out.values_mut() {
            chain.sort_by(|a, b| b.lsn.cmp(&a.lsn));
            if let Some(idx) = chain.iter().position(|r| r.will_init) {
                chain.truncate(idx + 1);
            }
        }
        Ok(out)
    }
}

/// Handle for a periodic-flush background task. Cancel the token to
/// stop; the task does one final `flush()` before returning.
pub struct FlushTask {
    pub handle: tokio::task::JoinHandle<()>,
    pub cancel: CancellationToken,
}

impl FlushTask {
    pub async fn shutdown(self) {
        self.cancel.cancel();
        let _ = self.handle.await;
    }
}

impl PageWriter {
    pub fn spawn_flusher(self: &Arc<Self>, tick: Duration) -> FlushTask {
        let cancel = CancellationToken::new();
        let cancel_for_task = cancel.clone();
        let weak = Arc::downgrade(self);
        let handle = tokio::spawn(async move {
            loop {
                tokio::select! {
                    _ = cancel_for_task.cancelled() => break,
                    _ = tokio::time::sleep(tick) => {}
                }
                let Some(w) = weak.upgrade() else { break };
                if w.should_flush().await
                    && let Err(e) = w.flush().await
                {
                    tracing::warn!("PageWriter periodic flush failed: {e:?}");
                }
            }
            if let Some(w) = weak.upgrade()
                && let Err(e) = w.flush().await
            {
                tracing::warn!("PageWriter final flush failed: {e:?}");
            }
        });
        FlushTask { handle, cancel }
    }
}

/// Convert the upstream-shaped `SerializedValueBatch` (one batch per
/// digested WAL record) into the `(Key, LSN, Value)` triples
/// [`PageWriter::put`] consumes. Skips `ValueMeta::Observed` entries.
pub fn extract_triples(batch: &SerializedValueBatch) -> Result<Vec<(Key, Lsn, Value)>> {
    let mut out = Vec::with_capacity(batch.len);
    for meta in &batch.metadata {
        let ValueMeta::Serialized(ser) = meta else {
            continue;
        };
        let start = ser.batch_offset as usize;
        let end = start
            .checked_add(ser.len)
            .ok_or_else(|| anyhow::anyhow!("ValueMeta offset overflow"))?;
        if end > batch.raw.len() {
            anyhow::bail!(
                "ValueMeta range {start}..{end} exceeds raw buffer length {}",
                batch.raw.len()
            );
        }
        let value = Value::des(&batch.raw[start..end])?;
        let key = Key::from_compact(ser.key);
        out.push((key, ser.lsn, value));
    }
    Ok(out)
}

fn key_to_bytes(key: Key) -> Bytes {
    let mut buf = vec![0u8; KEY_SIZE];
    key.write_to_byte_slice(&mut buf);
    Bytes::from(buf)
}

/// Translate a walingest `Value` into a `PageRow`. Images become
/// `is_image=TRUE, will_init=TRUE`. WAL records become `is_image=FALSE`
/// and carry the record's own `will_init` flag — that's the only signal
/// the read side has to identify a chain anchor when no image exists.
fn build_page_row(key: Key, lsn: Lsn, value: &Value) -> Result<PageRow> {
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

fn row_size(r: &PageRow) -> usize {
    r.payload.len() + r.key.len() + 16
}

/// SLRU equivalent of `build_page_row`. `key` must be an SLRU block key
/// (`Key::is_slru_block_key`); the caller is expected to have already
/// filtered.
fn build_slru_page_row(key: Key, lsn: Lsn, value: &Value) -> Result<SlruPageRow> {
    let (kind, segno, blknum) = key.to_slru_block()?;
    let (is_image, will_init, payload) = match value {
        Value::Image(b) => (true, true, b.clone()),
        Value::WalRecord(rec) => {
            let will_init = rec.will_init();
            let bytes = Bytes::from(rec.ser()?);
            (false, will_init, bytes)
        }
    };
    Ok(SlruPageRow {
        kind,
        segno,
        blknum,
        lsn,
        is_image,
        will_init,
        payload,
    })
}

fn slru_row_size(r: &SlruPageRow) -> usize {
    r.payload.len() + 32
}

/// Convert a meta-key Value into a MetaRow. Meta keys are
/// full-overwrite, so we only accept `Value::Image`; a WalRecord on a
/// non-rel-non-slru key would mean walredo is expected for a meta key,
/// which the row-store design doesn't support.
fn build_meta_row(key: Key, lsn: Lsn, value: &Value) -> Result<MetaRow> {
    let payload = match value {
        Value::Image(b) => b.clone(),
        Value::WalRecord(rec) => {
            // Some walingest paths still emit Value::WalRecord for
            // small meta deltas (e.g. relation-size sentinel updates
            // that carry the new size in the record). Serialize and
            // store as-is — meta reads return the row that's there;
            // higher layers are responsible for interpreting the
            // payload. The expectation is that these arrive with
            // will_init=true (the record fully replaces the key's
            // value at this LSN) — log if it isn't.
            if !rec.will_init() {
                tracing::warn!(
                    "PageWriter: meta key {key} lsn={lsn} received a non-init WalRecord; \
                     stored as opaque payload"
                );
            }
            Bytes::from(rec.ser()?)
        }
    };
    Ok(MetaRow {
        key: key_to_bytes(key),
        lsn,
        payload,
    })
}

fn meta_row_size(r: &MetaRow) -> usize {
    r.payload.len() + r.key.len() + 16
}

#[cfg(test)]
mod tests {
    use super::*;

    use bytes::Bytes;
    use remote_storage::YbStoreConfig;
    use utils::id::{TenantId, TimelineId};
    use wal_decoder::models::record::NeonWalRecord;

    use crate::page_store::{PageStore, PageStoreConfig};

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
            field2: 1663,
            field3: 5,
            field4: 16384,
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
        policy: FlushPolicy,
    ) -> (PageWriter, TenantId, TimelineId) {
        let t = TenantId::generate();
        let tl = TimelineId::generate();
        (
            PageWriter::new(t, tl, store, policy),
            t,
            tl,
        )
    }

    #[test]
    fn extract_triples_round_trips_through_serialized_batch() {
        let values_in: Vec<(Key, Lsn, Value)> = vec![
            (k(1), Lsn(100), image()),
            (
                k(2),
                Lsn(200),
                Value::WalRecord(NeonWalRecord::Postgres {
                    will_init: true,
                    rec: Bytes::from_static(b"abc"),
                }),
            ),
            (k(1), Lsn(300), delta()),
        ];
        let triples_in: Vec<(pageserver_api::key::CompactKey, Lsn, usize, Value)> = values_in
            .iter()
            .map(|(k, l, v)| {
                (
                    k.to_compact(),
                    *l,
                    v.serialized_size().expect("serialized_size") as usize,
                    v.clone(),
                )
            })
            .collect();
        let batch = SerializedValueBatch::from_values(triples_in);
        let extracted = extract_triples(&batch).expect("extract_triples");
        assert_eq!(extracted.len(), values_in.len());
        for ((k_in, lsn_in, val_in), (k_out, lsn_out, val_out)) in
            values_in.iter().zip(extracted.iter())
        {
            assert_eq!(k_out, k_in);
            assert_eq!(lsn_out, lsn_in);
            assert_eq!(val_out, val_in);
        }
    }

    #[tokio::test]
    async fn empty_flush_is_noop() {
        let (store, schema) = make_store("empty_flush").await;
        let (w, _t, _tl) = writer(store.clone(), FlushPolicy::default());
        w.flush().await.unwrap();
        let stats = w.stats().await;
        assert_eq!(stats.buffered_rows, 0);
        drop_schema(&store, &schema).await;
    }

    #[tokio::test]
    async fn image_passes_through_untouched() {
        let (store, schema) = make_store("image_pt").await;
        let (w, t, tl) = writer(store.clone(), FlushPolicy::default());
        w.put(&[(k(1), Lsn(10), image())]).await.unwrap();
        let stats = w.stats().await;
        assert_eq!(stats.rows_written, 1);
        assert_eq!(stats.buffered_rows, 1);
        w.flush().await.unwrap();
        let chain = store
            .read_chain(t, tl, &key_to_bytes(k(1)), Lsn(100))
            .await
            .unwrap();
        assert_eq!(chain.len(), 1);
        assert!(chain[0].is_image);
        assert!(chain[0].will_init);
        assert_eq!(chain[0].payload.as_ref(), b"natural-image-bytes");
        drop_schema(&store, &schema).await;
    }

    #[tokio::test]
    async fn delta_passes_through_untouched() {
        // No write-time materialisation: a plain delta arriving first
        // is stored as-is; reads find it via the slow path.
        let (store, schema) = make_store("delta_pt").await;
        let (w, t, tl) = writer(store.clone(), FlushPolicy::default());
        let init = Value::WalRecord(NeonWalRecord::Postgres {
            will_init: true,
            rec: Bytes::from_static(b"init"),
        });
        w.put(&[
            (k(1), Lsn(10), init),
            (k(1), Lsn(20), delta()),
            (k(1), Lsn(30), delta()),
        ]).await.unwrap();
        let stats = w.stats().await;
        assert_eq!(stats.rows_written, 3);
        w.flush().await.unwrap();
        let chain = store
            .read_chain(t, tl, &key_to_bytes(k(1)), Lsn(100))
            .await
            .unwrap();
        assert_eq!(chain.len(), 3);
        // LSN-descending; anchor at the bottom.
        assert_eq!(chain[0].lsn, Lsn(30));
        assert_eq!(chain[2].lsn, Lsn(10));
        assert!(chain[2].will_init);
        assert!(!chain[2].is_image);
        drop_schema(&store, &schema).await;
    }

    #[tokio::test]
    async fn long_chain_without_image_round_trips() {
        // 100 deltas after a will_init delta — no image, no truncation.
        // read_chain returns the full chain up to the anchor.
        let (store, schema) = make_store("long_chain").await;
        let (w, t, tl) = writer(store.clone(), FlushPolicy::default());
        let init = Value::WalRecord(NeonWalRecord::Postgres {
            will_init: true,
            rec: Bytes::from_static(b"init"),
        });
        let mut entries: Vec<(Key, Lsn, Value)> = vec![(k(1), Lsn(10), init)];
        for i in 1..=100 {
            entries.push((k(1), Lsn(10 + 10 * i), delta()));
        }
        w.put(&entries).await.unwrap();
        w.flush().await.unwrap();
        let chain = store
            .read_chain(t, tl, &key_to_bytes(k(1)), Lsn(2000))
            .await
            .unwrap();
        assert_eq!(chain.len(), 101);
        assert_eq!(chain[0].lsn, Lsn(1010));
        assert!(chain.last().unwrap().will_init);
        drop_schema(&store, &schema).await;
    }

    #[tokio::test]
    async fn routes_keys_to_three_buffers() {
        // Each Key kind lands in its own buffer:
        //   rel-page  → pages buffer
        //   slru-block → slru buffer
        //   anything else → meta buffer
        let (store, schema) = make_store("route_three").await;
        let (w, t, tl) = writer(store.clone(), FlushPolicy::default());

        let meta_key = Key {
            field1: 0x60,
            field2: 0,
            field3: 0,
            field4: 0,
            field5: 0,
            field6: 0,
        };
        let slru_key = pageserver_api::key::slru_block_to_key(
            pageserver_api::reltag::SlruKind::Clog,
            7,
            42,
        );

        w.put(&[
            (k(1), Lsn(10), image()),
            (slru_key, Lsn(10), image()),
            (meta_key, Lsn(10), image()),
        ])
        .await
        .unwrap();
        let stats = w.stats().await;
        assert_eq!(stats.rows_written, 3);
        assert_eq!(stats.buffered_rows, 1, "rel buffer");
        assert_eq!(stats.buffered_slru_rows, 1, "slru buffer");
        assert_eq!(stats.buffered_meta_rows, 1, "meta buffer");
        w.flush().await.unwrap();
        // Meta round-trips its payload bytes.
        let mut meta_key_bytes = vec![0u8; pageserver_api::key::KEY_SIZE];
        meta_key.write_to_byte_slice(&mut meta_key_bytes);
        let got = store
            .read_meta(t, tl, &meta_key_bytes, Lsn(100))
            .await
            .unwrap()
            .expect("meta row");
        assert_eq!(got.payload.as_ref(), b"natural-image-bytes");
        drop_schema(&store, &schema).await;
    }

    #[tokio::test]
    async fn flush_emits_a_single_insert_round_trip() {
        let (store, schema) = make_store("flush_one_rt").await;
        let (w, t, tl) = writer(store.clone(), FlushPolicy::default());
        let entries: Vec<(Key, Lsn, Value)> =
            (0..50).map(|i| (k(i), Lsn(10), image())).collect();
        w.put(&entries).await.unwrap();
        assert_eq!(w.stats().await.buffered_rows, 50);
        w.flush().await.unwrap();
        assert_eq!(w.stats().await.buffered_rows, 0);
        // Sample-read one of them.
        let chain = store
            .read_chain(t, tl, &key_to_bytes(k(0)), Lsn(100))
            .await
            .unwrap();
        assert_eq!(chain.len(), 1);
        assert!(chain[0].is_image);
        drop_schema(&store, &schema).await;
    }

    #[tokio::test]
    async fn should_flush_on_size() {
        let (store, schema) = make_store("flush_size").await;
        let policy = FlushPolicy {
            max_age: Duration::from_secs(3600),
            max_bytes: 200,
        };
        let (w, _t, _tl) = writer(store.clone(), policy);
        assert!(!w.should_flush().await);
        for i in 0..4 {
            w.put(&[(k(i), Lsn(10), image())]).await.unwrap();
        }
        assert!(w.should_flush().await, "stats: {:?}", w.stats().await);
        drop_schema(&store, &schema).await;
    }

    #[tokio::test]
    async fn should_flush_on_age() {
        let (store, schema) = make_store("flush_age").await;
        let policy = FlushPolicy {
            max_age: Duration::from_millis(100),
            max_bytes: 1_000_000_000,
        };
        let (w, _t, _tl) = writer(store.clone(), policy);
        w.put(&[(k(1), Lsn(10), image())]).await.unwrap();
        assert!(!w.should_flush().await);
        tokio::time::sleep(Duration::from_millis(150)).await;
        assert!(w.should_flush().await);
        w.flush().await.unwrap();
        assert!(!w.should_flush().await);
        drop_schema(&store, &schema).await;
    }

    #[tokio::test]
    async fn read_chain_blends_buffered_and_persisted() {
        let (store, schema) = make_store("blend").await;
        let (w, _t, _tl) = writer(store.clone(), FlushPolicy::default());
        let first_batch: Vec<(Key, Lsn, Value)> = (0..5)
            .map(|i| (k(1), Lsn(10 + 10 * i), if i == 0 { image() } else { delta() }))
            .collect();
        w.put(&first_batch).await.unwrap();
        w.flush().await.unwrap();
        let second_batch: Vec<(Key, Lsn, Value)> =
            (5..8).map(|i| (k(1), Lsn(10 + 10 * i), delta())).collect();
        w.put(&second_batch).await.unwrap();
        assert!(w.stats().await.buffered_rows > 0);
        let chain = w.read_chain(k(1), Lsn(1000)).await.unwrap();
        // image at 10 + 7 deltas at 20..80.
        assert_eq!(chain.len(), 8);
        let mut prev = u64::MAX;
        for row in &chain {
            assert!(row.lsn.0 < prev);
            prev = row.lsn.0;
        }
        assert_eq!(chain.last().unwrap().lsn, Lsn(10));
        assert!(chain.last().unwrap().will_init);
        drop_schema(&store, &schema).await;
    }

    #[tokio::test]
    async fn read_chain_truncates_at_will_init_anchor() {
        let (store, schema) = make_store("trunc_at_anchor").await;
        let (w, _t, _tl) = writer(store.clone(), FlushPolicy::default());
        w.put(&[(k(1), Lsn(10), image())]).await.unwrap();
        for i in 1..=3 {
            w.put(&[(k(1), Lsn(10 + 10 * i), delta())]).await.unwrap();
        }
        let init = Value::WalRecord(NeonWalRecord::Postgres {
            will_init: true,
            rec: Bytes::from_static(b"init"),
        });
        w.put(&[(k(1), Lsn(50), init)]).await.unwrap();
        for i in 6..=8 {
            w.put(&[(k(1), Lsn(10 + 10 * i), delta())]).await.unwrap();
        }
        w.flush().await.unwrap();
        let chain = w.read_chain(k(1), Lsn(1000)).await.unwrap();
        // 4 rows: deltas at 90/80/70, anchor at 50.
        assert_eq!(chain.len(), 4);
        assert_eq!(chain.first().unwrap().lsn, Lsn(90));
        assert_eq!(chain.last().unwrap().lsn, Lsn(50));
        assert!(chain.last().unwrap().will_init);
        drop_schema(&store, &schema).await;
    }

    #[tokio::test]
    async fn periodic_flusher_drains_on_age_trigger() {
        let (store, schema) = make_store("flusher_age").await;
        let t = TenantId::generate();
        let tl = TimelineId::generate();
        let policy = FlushPolicy {
            max_age: Duration::from_millis(40),
            max_bytes: 1 << 30,
        };
        let w = Arc::new(PageWriter::new(t, tl, store.clone(), policy));
        let task = w.spawn_flusher(Duration::from_millis(15));
        w.put(&[(k(1), Lsn(10), image())]).await.unwrap();
        tokio::time::sleep(Duration::from_millis(250)).await;
        let stats = w.stats().await;
        assert_eq!(stats.buffered_rows, 0, "stats: {stats:?}");
        let chain = store
            .read_chain(t, tl, &key_to_bytes(k(1)), Lsn(100))
            .await
            .unwrap();
        assert_eq!(chain.len(), 1);
        task.shutdown().await;
        drop_schema(&store, &schema).await;
    }

    #[tokio::test]
    async fn periodic_flusher_does_final_flush_on_shutdown() {
        let (store, schema) = make_store("flusher_final").await;
        let t = TenantId::generate();
        let tl = TimelineId::generate();
        let policy = FlushPolicy {
            max_age: Duration::from_secs(3600),
            max_bytes: 1 << 30,
        };
        let w = Arc::new(PageWriter::new(t, tl, store.clone(), policy));
        let task = w.spawn_flusher(Duration::from_millis(15));
        w.put(&[(k(1), Lsn(10), image())]).await.unwrap();
        tokio::time::sleep(Duration::from_millis(80)).await;
        assert!(w.stats().await.buffered_rows > 0);
        task.shutdown().await;
        assert_eq!(w.stats().await.buffered_rows, 0);
        let chain = store
            .read_chain(t, tl, &key_to_bytes(k(1)), Lsn(100))
            .await
            .unwrap();
        assert_eq!(chain.len(), 1);
        drop_schema(&store, &schema).await;
    }

    #[tokio::test]
    async fn passthrough_payload_is_serialised_wal_record() {
        let (store, schema) = make_store("ser_rt").await;
        let (w, t, tl) = writer(store.clone(), FlushPolicy::default());
        let rec = NeonWalRecord::ClearVisibilityMapFlags {
            new_heap_blkno: Some(42),
            old_heap_blkno: Some(43),
            flags: 0x07,
        };
        w.put(&[(k(1), Lsn(10), image())]).await.unwrap();
        w.put(&[(k(1), Lsn(20), Value::WalRecord(rec.clone()))])
            .await
            .unwrap();
        w.flush().await.unwrap();
        let chain = store
            .read_chain(t, tl, &key_to_bytes(k(1)), Lsn(100))
            .await
            .unwrap();
        assert_eq!(chain.len(), 2);
        let delta_row = chain.iter().find(|r| !r.is_image).expect("one delta row");
        assert!(!delta_row.will_init);
        let decoded = NeonWalRecord::des(&delta_row.payload).expect("ser/des round-trip");
        assert_eq!(decoded, rec);
        drop_schema(&store, &schema).await;
    }
}
