//! `PageStore` — YugabyteDB-backed `(Key, LSN) → Value` row store.
//!
//! This is yb-pageserver's replacement for the upstream layer-file machinery
//! (delta/image layers + on-disk B-trees + L0/L1 compaction). Every page
//! version is one row in `pageserver.pages`; per-timeline bookkeeping that
//! upstream split across `metadata`, `index_part.json`, and
//! `tenant-manifest.json` collapses into one `pageserver.timelines` row.
//!
//! The read path is bounded by the *10-delta image rule* enforced at write
//! time: at most 9 deltas between any two image rows for the same key, so
//! `SELECT … ORDER BY lsn DESC LIMIT 10` always yields a chain that
//! contains an image. The 10-delta rule lives in the caller, not here —
//! `PageStore` is a thin row store.
//!
//! Connection management piggybacks on `remote_storage::YsqlClient`: same
//! reconnect-on-close semantics + the `SET yb_enable_upsert_mode = true`
//! GUC, so a plain `INSERT` skips DocDB's read-before-write check.

use std::collections::HashMap;
use std::sync::Arc;

use anyhow::{Context, Result};
use bytes::Bytes;
use remote_storage::{YbStoreConfig, YsqlClient};
use tokio_postgres::types::ToSql;
use utils::id::{TenantId, TimelineId};
use utils::lsn::Lsn;

/// One row in `pageserver.pages`.
///
/// `payload` is the raw bytes: an 8 KiB Postgres page when `is_image`,
/// otherwise a serialised `NeonWalRecord` (whatever the caller wants —
/// `PageStore` doesn't inspect it). `will_init` is the cached value of
/// `Value::will_init()` so reads can stop walking back without a decode.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct PageRow {
    pub key: Bytes,
    pub lsn: Lsn,
    pub is_image: bool,
    pub will_init: bool,
    pub payload: Bytes,
}

/// One row in `pageserver.timelines`. Replaces the upstream split of
/// `metadata`, `index_part.json`, and `tenant-manifest.json` with a
/// single transactionally-updatable record.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct TimelineRow {
    pub tenant_id: TenantId,
    pub timeline_id: TimelineId,
    pub last_record_lsn: Lsn,
    pub disk_consistent_lsn: Lsn,
    pub applied_gc_cutoff_lsn: Lsn,
    pub ancestor_timeline_id: Option<TimelineId>,
    pub ancestor_lsn: Option<Lsn>,
    pub current_generation: i32,
    pub pg_version: u32,
}

/// Default schema name. Tests override to isolate from production rows.
pub const DEFAULT_SCHEMA: &str = "pageserver";

/// Connection + addressing for a `PageStore`. `yb` carries host/port/TLS/etc.,
/// `schema` controls which schema the page/timeline tables live in (defaults
/// to `pageserver` — tests pass a unique value for isolation).
#[derive(Debug, Clone)]
pub struct PageStoreConfig {
    pub yb: YbStoreConfig,
    pub schema: String,
}

impl PageStoreConfig {
    pub fn new(yb: YbStoreConfig) -> Self {
        Self {
            yb,
            schema: DEFAULT_SCHEMA.to_string(),
        }
    }
}

/// Thin row-store wrapper around `pageserver.pages` and
/// `pageserver.timelines`. Cheap to clone: holds an `Arc` to the underlying
/// reconnecting client.
#[derive(Clone)]
pub struct PageStore {
    client: Arc<YsqlClient>,
    schema: Arc<str>,
}

impl PageStore {
    /// Open a connection to YugabyteDB using `config.yb`. Eager — surfaces
    /// auth / DNS / TLS errors at construction.
    pub async fn connect(config: PageStoreConfig, application_name: &str) -> Result<Self> {
        let client = YsqlClient::from_yb_config(&config.yb, application_name)
            .await
            .context("opening PageStore connection")?;
        Ok(Self {
            client: Arc::new(client),
            schema: Arc::from(config.schema),
        })
    }

    /// Idempotent CREATE SCHEMA + CREATE TABLE for our two tables.
    ///
    /// `control_plane`'s `ensure_external_database` runs the same DDL at
    /// cluster bootstrap; this method exists so tests can spin up an
    /// isolated schema without going through the control-plane.
    pub async fn ensure_schema(&self) -> Result<()> {
        let create_schema = format!("CREATE SCHEMA IF NOT EXISTS \"{}\"", self.schema);
        self.client
            .execute(&create_schema, &[])
            .await
            .with_context(|| format!("creating schema {}", self.schema))?;

        let create_pages = format!(
            "CREATE TABLE IF NOT EXISTS \"{}\".pages (
                tenant_id    TEXT    NOT NULL,
                timeline_id  TEXT    NOT NULL,
                key          BYTEA   NOT NULL,
                lsn          BIGINT  NOT NULL,
                is_image     BOOLEAN NOT NULL,
                will_init    BOOLEAN NOT NULL,
                payload      BYTEA   NOT NULL,
                PRIMARY KEY ((tenant_id, timeline_id, key) HASH, lsn DESC)
            )",
            self.schema
        );
        self.client
            .execute(&create_pages, &[])
            .await
            .context("creating pages table")?;

        let create_timelines = format!(
            "CREATE TABLE IF NOT EXISTS \"{}\".timelines (
                tenant_id              TEXT    NOT NULL,
                timeline_id            TEXT    NOT NULL,
                last_record_lsn        BIGINT  NOT NULL,
                disk_consistent_lsn    BIGINT  NOT NULL,
                applied_gc_cutoff_lsn  BIGINT  NOT NULL,
                ancestor_timeline_id   TEXT,
                ancestor_lsn           BIGINT,
                current_generation     INT     NOT NULL,
                pg_version             INT     NOT NULL,
                PRIMARY KEY ((tenant_id) HASH, timeline_id)
            )",
            self.schema
        );
        self.client
            .execute(&create_timelines, &[])
            .await
            .context("creating timelines table")?;
        Ok(())
    }

    // ------------------- pages ---------------------------------------------

    /// Insert a batch of page rows. The caller controls the 10-delta image
    /// rule — `PageStore` does not reinterpret `(is_image, will_init,
    /// payload)`.
    ///
    /// Implementation: one multi-row `INSERT … VALUES (…),(…),…` per call.
    /// YB's `rpc_max_message_size` defaults to 256 MiB; callers are
    /// expected to keep batches well below that (the flush layer in the
    /// pageserver caps at 100 MB by default).
    pub async fn put_pages(
        &self,
        tenant: TenantId,
        timeline: TimelineId,
        rows: &[PageRow],
    ) -> Result<()> {
        if rows.is_empty() {
            return Ok(());
        }
        let tenant_s = tenant.to_string();
        let timeline_s = timeline.to_string();

        let mut sql = format!(
            "INSERT INTO \"{}\".pages \
             (tenant_id, timeline_id, key, lsn, is_image, will_init, payload) VALUES ",
            self.schema
        );
        let mut params: Vec<Box<dyn ToSql + Sync + Send>> = Vec::with_capacity(rows.len() * 7);
        for (i, row) in rows.iter().enumerate() {
            if i > 0 {
                sql.push(',');
            }
            let base = i * 7 + 1;
            sql.push_str(&format!(
                "(${}, ${}, ${}, ${}, ${}, ${}, ${})",
                base,
                base + 1,
                base + 2,
                base + 3,
                base + 4,
                base + 5,
                base + 6,
            ));
            params.push(Box::new(tenant_s.clone()));
            params.push(Box::new(timeline_s.clone()));
            params.push(Box::new(row.key.to_vec()));
            params.push(Box::new(row.lsn.0 as i64));
            params.push(Box::new(row.is_image));
            params.push(Box::new(row.will_init));
            params.push(Box::new(row.payload.to_vec()));
        }
        let params_ref: Vec<&(dyn ToSql + Sync)> =
            params.iter().map(|p| p.as_ref() as &(dyn ToSql + Sync)).collect();
        self.client
            .execute(&sql, &params_ref)
            .await
            .with_context(|| {
                format!(
                    "put_pages tenant={tenant} timeline={timeline} batch_rows={}",
                    rows.len()
                )
            })?;
        Ok(())
    }

    /// Read the most recent `limit` rows for `key` at LSN ≤ `target_lsn`,
    /// ordered `lsn` descending. With the 10-delta image rule, `limit = 10`
    /// is sufficient to guarantee an image is in the returned chain.
    pub async fn read_chain(
        &self,
        tenant: TenantId,
        timeline: TimelineId,
        key: &[u8],
        target_lsn: Lsn,
        limit: usize,
    ) -> Result<Vec<PageRow>> {
        let sql = format!(
            "SELECT key, lsn, is_image, will_init, payload \
             FROM \"{}\".pages \
             WHERE tenant_id = $1 AND timeline_id = $2 AND key = $3 AND lsn <= $4 \
             ORDER BY lsn DESC LIMIT $5",
            self.schema
        );
        let key_owned = key.to_vec();
        let rows = self
            .client
            .query(
                &sql,
                &[
                    &tenant.to_string(),
                    &timeline.to_string(),
                    &key_owned,
                    &(target_lsn.0 as i64),
                    &(limit as i64),
                ],
            )
            .await
            .context("read_chain")?;
        rows.into_iter().map(row_to_page).collect()
    }

    /// Vectored read: independent `read_chain` for each key, returned as a
    /// map keyed by the original key bytes. Each chain is a fresh round-trip
    /// for now; a follow-up will fold this into one round-trip via
    /// `UNNEST`-driven joins once the read path needs it.
    pub async fn read_chains(
        &self,
        tenant: TenantId,
        timeline: TimelineId,
        keys: &[Bytes],
        target_lsn: Lsn,
        limit: usize,
    ) -> Result<HashMap<Bytes, Vec<PageRow>>> {
        let mut out = HashMap::with_capacity(keys.len());
        for key in keys {
            let chain = self
                .read_chain(tenant, timeline, key, target_lsn, limit)
                .await?;
            out.insert(key.clone(), chain);
        }
        Ok(out)
    }

    /// Drop every page row for `(tenant, timeline)`. Currently only used by
    /// tests; production GC lands separately.
    pub async fn delete_all_pages(
        &self,
        tenant: TenantId,
        timeline: TimelineId,
    ) -> Result<()> {
        let sql = format!(
            "DELETE FROM \"{}\".pages WHERE tenant_id = $1 AND timeline_id = $2",
            self.schema
        );
        self.client
            .execute(&sql, &[&tenant.to_string(), &timeline.to_string()])
            .await
            .context("delete_all_pages")?;
        Ok(())
    }

    // ------------------- timelines ----------------------------------------

    /// Upsert (write-or-replace) the full state row for a timeline.
    pub async fn upsert_timeline(&self, row: &TimelineRow) -> Result<()> {
        let sql = format!(
            "INSERT INTO \"{}\".timelines \
             (tenant_id, timeline_id, last_record_lsn, disk_consistent_lsn, \
              applied_gc_cutoff_lsn, ancestor_timeline_id, ancestor_lsn, \
              current_generation, pg_version) \
             VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) \
             ON CONFLICT (tenant_id, timeline_id) DO UPDATE SET \
                last_record_lsn       = EXCLUDED.last_record_lsn, \
                disk_consistent_lsn   = EXCLUDED.disk_consistent_lsn, \
                applied_gc_cutoff_lsn = EXCLUDED.applied_gc_cutoff_lsn, \
                ancestor_timeline_id  = EXCLUDED.ancestor_timeline_id, \
                ancestor_lsn          = EXCLUDED.ancestor_lsn, \
                current_generation    = EXCLUDED.current_generation, \
                pg_version            = EXCLUDED.pg_version",
            self.schema
        );
        self.client
            .execute(
                &sql,
                &[
                    &row.tenant_id.to_string(),
                    &row.timeline_id.to_string(),
                    &(row.last_record_lsn.0 as i64),
                    &(row.disk_consistent_lsn.0 as i64),
                    &(row.applied_gc_cutoff_lsn.0 as i64),
                    &row.ancestor_timeline_id.map(|id| id.to_string()),
                    &row.ancestor_lsn.map(|l| l.0 as i64),
                    &row.current_generation,
                    &(row.pg_version as i32),
                ],
            )
            .await
            .context("upsert_timeline")?;
        Ok(())
    }

    pub async fn get_timeline(
        &self,
        tenant: TenantId,
        timeline: TimelineId,
    ) -> Result<Option<TimelineRow>> {
        let sql = format!(
            "SELECT tenant_id, timeline_id, last_record_lsn, disk_consistent_lsn, \
                    applied_gc_cutoff_lsn, ancestor_timeline_id, ancestor_lsn, \
                    current_generation, pg_version \
             FROM \"{}\".timelines \
             WHERE tenant_id = $1 AND timeline_id = $2",
            self.schema
        );
        let row = self
            .client
            .query_opt(&sql, &[&tenant.to_string(), &timeline.to_string()])
            .await
            .context("get_timeline")?;
        row.map(row_to_timeline).transpose()
    }

    pub async fn list_timelines(&self, tenant: TenantId) -> Result<Vec<TimelineRow>> {
        let sql = format!(
            "SELECT tenant_id, timeline_id, last_record_lsn, disk_consistent_lsn, \
                    applied_gc_cutoff_lsn, ancestor_timeline_id, ancestor_lsn, \
                    current_generation, pg_version \
             FROM \"{}\".timelines \
             WHERE tenant_id = $1",
            self.schema
        );
        let rows = self
            .client
            .query(&sql, &[&tenant.to_string()])
            .await
            .context("list_timelines")?;
        rows.into_iter().map(row_to_timeline).collect()
    }

    pub async fn delete_timeline(
        &self,
        tenant: TenantId,
        timeline: TimelineId,
    ) -> Result<()> {
        let sql = format!(
            "DELETE FROM \"{}\".timelines WHERE tenant_id = $1 AND timeline_id = $2",
            self.schema
        );
        self.client
            .execute(&sql, &[&tenant.to_string(), &timeline.to_string()])
            .await
            .context("delete_timeline")?;
        Ok(())
    }

    /// Atomically increment `current_generation` and return the new value.
    /// The bump fences any older pageserver that still thinks it owns the
    /// timeline: its next gen-checked write hits a row with a newer gen and
    /// aborts.
    ///
    /// Fails if no timeline row exists — call `upsert_timeline` first.
    pub async fn bump_generation(
        &self,
        tenant: TenantId,
        timeline: TimelineId,
    ) -> Result<i32> {
        let sql = format!(
            "UPDATE \"{}\".timelines \
             SET current_generation = current_generation + 1 \
             WHERE tenant_id = $1 AND timeline_id = $2 \
             RETURNING current_generation",
            self.schema
        );
        let row = self
            .client
            .query_opt(&sql, &[&tenant.to_string(), &timeline.to_string()])
            .await
            .context("bump_generation")?
            .with_context(|| format!("no timeline row for {tenant}/{timeline}"))?;
        Ok(row.get::<_, i32>(0))
    }

    pub async fn read_generation(
        &self,
        tenant: TenantId,
        timeline: TimelineId,
    ) -> Result<Option<i32>> {
        let sql = format!(
            "SELECT current_generation FROM \"{}\".timelines \
             WHERE tenant_id = $1 AND timeline_id = $2",
            self.schema
        );
        let row = self
            .client
            .query_opt(&sql, &[&tenant.to_string(), &timeline.to_string()])
            .await
            .context("read_generation")?;
        Ok(row.map(|r| r.get::<_, i32>(0)))
    }

    /// Test-only escape hatch — runs raw SQL on the underlying client.
    /// Used by integration tests to create / drop per-test schemas. Not
    /// for production callers.
    #[doc(hidden)]
    pub async fn test_only_execute(&self, sql: &str) -> Result<u64> {
        self.client.execute(sql, &[]).await
    }
}

fn row_to_page(r: tokio_postgres::Row) -> Result<PageRow> {
    let key: Vec<u8> = r.try_get(0)?;
    let lsn: i64 = r.try_get(1)?;
    let is_image: bool = r.try_get(2)?;
    let will_init: bool = r.try_get(3)?;
    let payload: Vec<u8> = r.try_get(4)?;
    Ok(PageRow {
        key: Bytes::from(key),
        lsn: Lsn(lsn as u64),
        is_image,
        will_init,
        payload: Bytes::from(payload),
    })
}

fn row_to_timeline(r: tokio_postgres::Row) -> Result<TimelineRow> {
    let tenant_s: String = r.try_get(0)?;
    let timeline_s: String = r.try_get(1)?;
    let last_record_lsn: i64 = r.try_get(2)?;
    let disk_consistent_lsn: i64 = r.try_get(3)?;
    let applied_gc_cutoff_lsn: i64 = r.try_get(4)?;
    let ancestor_tl: Option<String> = r.try_get(5)?;
    let ancestor_lsn: Option<i64> = r.try_get(6)?;
    let current_generation: i32 = r.try_get(7)?;
    let pg_version: i32 = r.try_get(8)?;
    Ok(TimelineRow {
        tenant_id: tenant_s.parse().context("parsing tenant_id")?,
        timeline_id: timeline_s.parse().context("parsing timeline_id")?,
        last_record_lsn: Lsn(last_record_lsn as u64),
        disk_consistent_lsn: Lsn(disk_consistent_lsn as u64),
        applied_gc_cutoff_lsn: Lsn(applied_gc_cutoff_lsn as u64),
        ancestor_timeline_id: ancestor_tl
            .map(|s| s.parse::<TimelineId>())
            .transpose()
            .context("parsing ancestor_timeline_id")?,
        ancestor_lsn: ancestor_lsn.map(|l| Lsn(l as u64)),
        current_generation,
        pg_version: pg_version as u32,
    })
}

#[cfg(test)]
mod tests {
    use super::*;

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

    /// Build a `PageStore` against a per-test schema name so concurrent
    /// tests don't trample each other. The schema is created and dropped
    /// inside the test.
    async fn make_store(suffix: &str) -> (PageStore, String) {
        let schema = format!("pageserver_test_{suffix}");
        // Drop first in case a prior failed run left it behind.
        {
            let cfg = PageStoreConfig {
                yb: test_yb_config(),
                schema: schema.clone(),
            };
            let store = PageStore::connect(cfg, "page_store_test_setup")
                .await
                .expect("connect to local YB");
            store
                .client
                .execute(&format!("DROP SCHEMA IF EXISTS \"{schema}\" CASCADE"), &[])
                .await
                .expect("drop pre-existing schema");
        }
        let cfg = PageStoreConfig {
            yb: test_yb_config(),
            schema: schema.clone(),
        };
        let store = PageStore::connect(cfg, "page_store_test")
            .await
            .expect("connect to local YB");
        store.ensure_schema().await.expect("ensure_schema");
        (store, schema)
    }

    async fn drop_schema(store: &PageStore, schema: &str) {
        let _ = store
            .client
            .execute(&format!("DROP SCHEMA IF EXISTS \"{schema}\" CASCADE"), &[])
            .await;
    }

    fn rand_tenant() -> TenantId {
        TenantId::generate()
    }

    fn rand_timeline() -> TimelineId {
        TimelineId::generate()
    }

    fn page_row(key: &[u8], lsn: u64, is_image: bool, will_init: bool, payload: &[u8]) -> PageRow {
        PageRow {
            key: Bytes::copy_from_slice(key),
            lsn: Lsn(lsn),
            is_image,
            will_init,
            payload: Bytes::copy_from_slice(payload),
        }
    }

    #[tokio::test]
    async fn ensure_schema_is_idempotent() {
        let (store, schema) = make_store("idem").await;
        for _ in 0..3 {
            store.ensure_schema().await.expect("second ensure_schema");
        }
        drop_schema(&store, &schema).await;
    }

    #[tokio::test]
    async fn put_and_read_chain_round_trip() {
        let (store, schema) = make_store("round_trip").await;
        let tenant = rand_tenant();
        let timeline = rand_timeline();
        let key = b"\x00abcdefghijklmnopq";
        assert_eq!(key.len(), 18);

        // Write image at lsn=10, deltas at 20 and 30.
        let rows = vec![
            page_row(key, 10, true, true, b"image-bytes"),
            page_row(key, 20, false, false, b"delta-A"),
            page_row(key, 30, false, false, b"delta-B"),
        ];
        store.put_pages(tenant, timeline, &rows).await.unwrap();

        // Read at lsn=100, limit=10 → all three rows, descending.
        let got = store
            .read_chain(tenant, timeline, key, Lsn(100), 10)
            .await
            .unwrap();
        assert_eq!(got.len(), 3);
        assert_eq!(got[0].lsn, Lsn(30));
        assert_eq!(got[1].lsn, Lsn(20));
        assert_eq!(got[2].lsn, Lsn(10));
        assert!(got[2].is_image);
        assert!(got[2].will_init);
        assert_eq!(got[0].payload.as_ref(), b"delta-B");
        assert_eq!(got[2].payload.as_ref(), b"image-bytes");

        drop_schema(&store, &schema).await;
    }

    #[tokio::test]
    async fn read_chain_respects_target_lsn_and_limit() {
        let (store, schema) = make_store("limit").await;
        let tenant = rand_tenant();
        let timeline = rand_timeline();
        let key = b"\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x01";

        // Image + 12 deltas at lsns 10, 20, ..., 130.
        let mut rows = vec![page_row(key, 10, true, true, b"img")];
        for i in 1..=12 {
            rows.push(page_row(key, 10 + 10 * i, false, false, &[b'd', i as u8]));
        }
        store.put_pages(tenant, timeline, &rows).await.unwrap();

        // limit=10 → 10 rows total.
        let got = store
            .read_chain(tenant, timeline, key, Lsn(10_000), 10)
            .await
            .unwrap();
        assert_eq!(got.len(), 10);
        assert_eq!(got.first().unwrap().lsn, Lsn(130));
        assert_eq!(got.last().unwrap().lsn, Lsn(40));

        // target_lsn=50 → only rows at 10, 20, 30, 40, 50 (5 rows).
        let got = store
            .read_chain(tenant, timeline, key, Lsn(50), 10)
            .await
            .unwrap();
        assert_eq!(got.len(), 5);
        assert_eq!(got[0].lsn, Lsn(50));
        assert_eq!(got[4].lsn, Lsn(10));

        // target_lsn=5 → empty (nothing ≤ 5).
        let got = store
            .read_chain(tenant, timeline, key, Lsn(5), 10)
            .await
            .unwrap();
        assert!(got.is_empty());

        drop_schema(&store, &schema).await;
    }

    #[tokio::test]
    async fn read_chain_isolates_by_tenant_and_timeline() {
        let (store, schema) = make_store("isolation").await;
        let key = b"\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x02";

        let t1 = rand_tenant();
        let t2 = rand_tenant();
        let tl1 = rand_timeline();
        let tl2 = rand_timeline();

        store
            .put_pages(t1, tl1, &[page_row(key, 10, true, true, b"t1tl1")])
            .await
            .unwrap();
        store
            .put_pages(t2, tl1, &[page_row(key, 10, true, true, b"t2tl1")])
            .await
            .unwrap();
        store
            .put_pages(t1, tl2, &[page_row(key, 10, true, true, b"t1tl2")])
            .await
            .unwrap();

        assert_eq!(
            store.read_chain(t1, tl1, key, Lsn(100), 5).await.unwrap()[0]
                .payload
                .as_ref(),
            b"t1tl1"
        );
        assert_eq!(
            store.read_chain(t2, tl1, key, Lsn(100), 5).await.unwrap()[0]
                .payload
                .as_ref(),
            b"t2tl1"
        );
        assert_eq!(
            store.read_chain(t1, tl2, key, Lsn(100), 5).await.unwrap()[0]
                .payload
                .as_ref(),
            b"t1tl2"
        );

        drop_schema(&store, &schema).await;
    }

    #[tokio::test]
    async fn read_chain_returns_empty_for_unknown_key() {
        let (store, schema) = make_store("unknown_key").await;
        let tenant = rand_tenant();
        let timeline = rand_timeline();
        let got = store
            .read_chain(tenant, timeline, b"unknown-key-bytes!", Lsn(100), 10)
            .await
            .unwrap();
        assert!(got.is_empty());
        drop_schema(&store, &schema).await;
    }

    #[tokio::test]
    async fn read_chains_vectored() {
        let (store, schema) = make_store("vectored").await;
        let tenant = rand_tenant();
        let timeline = rand_timeline();
        let k1: &[u8] = b"\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\xa1";
        let k2: &[u8] = b"\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\xa2";

        store
            .put_pages(
                tenant,
                timeline,
                &[
                    page_row(k1, 10, true, true, b"k1-img"),
                    page_row(k1, 20, false, false, b"k1-delta"),
                    page_row(k2, 15, true, true, b"k2-img"),
                ],
            )
            .await
            .unwrap();

        let got = store
            .read_chains(
                tenant,
                timeline,
                &[Bytes::copy_from_slice(k1), Bytes::copy_from_slice(k2)],
                Lsn(100),
                10,
            )
            .await
            .unwrap();
        assert_eq!(got.len(), 2);
        let k1_chain = got.get(&Bytes::copy_from_slice(k1)).unwrap();
        let k2_chain = got.get(&Bytes::copy_from_slice(k2)).unwrap();
        assert_eq!(k1_chain.len(), 2);
        assert_eq!(k1_chain[0].lsn, Lsn(20));
        assert_eq!(k2_chain.len(), 1);
        assert_eq!(k2_chain[0].payload.as_ref(), b"k2-img");
        drop_schema(&store, &schema).await;
    }

    #[tokio::test]
    async fn delete_all_pages_clears_one_timeline_only() {
        let (store, schema) = make_store("delete_all").await;
        let tenant = rand_tenant();
        let tl1 = rand_timeline();
        let tl2 = rand_timeline();
        let key = b"\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\xb0";

        store
            .put_pages(tenant, tl1, &[page_row(key, 1, true, true, b"a")])
            .await
            .unwrap();
        store
            .put_pages(tenant, tl2, &[page_row(key, 1, true, true, b"b")])
            .await
            .unwrap();

        store.delete_all_pages(tenant, tl1).await.unwrap();
        assert!(
            store
                .read_chain(tenant, tl1, key, Lsn(100), 5)
                .await
                .unwrap()
                .is_empty()
        );
        assert_eq!(
            store.read_chain(tenant, tl2, key, Lsn(100), 5).await.unwrap()[0]
                .payload
                .as_ref(),
            b"b"
        );
        drop_schema(&store, &schema).await;
    }

    fn sample_timeline_row(t: TenantId, tl: TimelineId) -> TimelineRow {
        TimelineRow {
            tenant_id: t,
            timeline_id: tl,
            last_record_lsn: Lsn(100),
            disk_consistent_lsn: Lsn(90),
            applied_gc_cutoff_lsn: Lsn(50),
            ancestor_timeline_id: None,
            ancestor_lsn: None,
            current_generation: 1,
            pg_version: 17,
        }
    }

    #[tokio::test]
    async fn upsert_and_get_timeline_round_trip() {
        let (store, schema) = make_store("tl_round_trip").await;
        let tenant = rand_tenant();
        let timeline = rand_timeline();
        let row = sample_timeline_row(tenant, timeline);
        store.upsert_timeline(&row).await.unwrap();
        let got = store.get_timeline(tenant, timeline).await.unwrap().unwrap();
        assert_eq!(got, row);

        // Second upsert with different fields overwrites.
        let updated = TimelineRow {
            last_record_lsn: Lsn(200),
            disk_consistent_lsn: Lsn(190),
            ..row
        };
        store.upsert_timeline(&updated).await.unwrap();
        let got = store.get_timeline(tenant, timeline).await.unwrap().unwrap();
        assert_eq!(got, updated);

        drop_schema(&store, &schema).await;
    }

    #[tokio::test]
    async fn get_timeline_returns_none_for_unknown() {
        let (store, schema) = make_store("tl_none").await;
        let got = store
            .get_timeline(rand_tenant(), rand_timeline())
            .await
            .unwrap();
        assert!(got.is_none());
        drop_schema(&store, &schema).await;
    }

    #[tokio::test]
    async fn list_timelines_per_tenant() {
        let (store, schema) = make_store("tl_list").await;
        let t1 = rand_tenant();
        let t2 = rand_tenant();
        let tl_a = rand_timeline();
        let tl_b = rand_timeline();
        let tl_c = rand_timeline();
        store
            .upsert_timeline(&sample_timeline_row(t1, tl_a))
            .await
            .unwrap();
        store
            .upsert_timeline(&sample_timeline_row(t1, tl_b))
            .await
            .unwrap();
        store
            .upsert_timeline(&sample_timeline_row(t2, tl_c))
            .await
            .unwrap();

        let mut got = store.list_timelines(t1).await.unwrap();
        got.sort_by_key(|r| r.timeline_id);
        let mut want = vec![tl_a, tl_b];
        want.sort();
        assert_eq!(got.iter().map(|r| r.timeline_id).collect::<Vec<_>>(), want);

        let only_t2 = store.list_timelines(t2).await.unwrap();
        assert_eq!(only_t2.len(), 1);
        assert_eq!(only_t2[0].timeline_id, tl_c);

        drop_schema(&store, &schema).await;
    }

    #[tokio::test]
    async fn delete_timeline_removes_row() {
        let (store, schema) = make_store("tl_delete").await;
        let tenant = rand_tenant();
        let timeline = rand_timeline();
        store
            .upsert_timeline(&sample_timeline_row(tenant, timeline))
            .await
            .unwrap();
        store.delete_timeline(tenant, timeline).await.unwrap();
        assert!(store.get_timeline(tenant, timeline).await.unwrap().is_none());
        drop_schema(&store, &schema).await;
    }

    #[tokio::test]
    async fn bump_generation_is_monotonic() {
        let (store, schema) = make_store("gen").await;
        let tenant = rand_tenant();
        let timeline = rand_timeline();
        store
            .upsert_timeline(&sample_timeline_row(tenant, timeline))
            .await
            .unwrap();
        let g0 = store.read_generation(tenant, timeline).await.unwrap();
        assert_eq!(g0, Some(1));
        for expected in 2..=5 {
            let g = store.bump_generation(tenant, timeline).await.unwrap();
            assert_eq!(g, expected);
        }
        let g_read = store.read_generation(tenant, timeline).await.unwrap();
        assert_eq!(g_read, Some(5));

        drop_schema(&store, &schema).await;
    }

    #[tokio::test]
    async fn bump_generation_fails_without_row() {
        let (store, schema) = make_store("gen_missing").await;
        let res = store
            .bump_generation(rand_tenant(), rand_timeline())
            .await;
        assert!(res.is_err());
        drop_schema(&store, &schema).await;
    }

    #[tokio::test]
    async fn ancestor_round_trip() {
        let (store, schema) = make_store("ancestor").await;
        let tenant = rand_tenant();
        let timeline = rand_timeline();
        let ancestor = rand_timeline();
        let row = TimelineRow {
            ancestor_timeline_id: Some(ancestor),
            ancestor_lsn: Some(Lsn(0x1234_5678_9abc_def0)),
            ..sample_timeline_row(tenant, timeline)
        };
        store.upsert_timeline(&row).await.unwrap();
        let got = store.get_timeline(tenant, timeline).await.unwrap().unwrap();
        assert_eq!(got.ancestor_timeline_id, Some(ancestor));
        assert_eq!(got.ancestor_lsn, Some(Lsn(0x1234_5678_9abc_def0)));
        drop_schema(&store, &schema).await;
    }

    #[tokio::test]
    async fn high_lsn_round_trip() {
        // u64 LSNs near 2^63 must round-trip cleanly through BIGINT.
        let (store, schema) = make_store("high_lsn").await;
        let tenant = rand_tenant();
        let timeline = rand_timeline();
        let key = b"\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\xff";
        let high = 0x7fff_ffff_ffff_fff0u64;
        store
            .put_pages(tenant, timeline, &[page_row(key, high, true, true, b"x")])
            .await
            .unwrap();
        let got = store
            .read_chain(tenant, timeline, key, Lsn(u64::MAX >> 1), 5)
            .await
            .unwrap();
        assert_eq!(got.len(), 1);
        assert_eq!(got[0].lsn.0, high);
        drop_schema(&store, &schema).await;
    }
}
