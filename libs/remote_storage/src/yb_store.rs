//! YugabyteDB-backed `RemoteStorage` implementation.
//!
//! Objects are stored as rows in a single user-managed table reached over the
//! Postgres wire protocol (`tokio_postgres`). Because YB is wire-compatible
//! with Postgres, the same code works against vanilla Postgres for local
//! development.
//!
//! The table is **not** created by this module; the operator is expected to
//! provision it with the schema below (column names are hard-coded):
//!
//! ```sql
//! CREATE TABLE <table_name> (
//!     key              TEXT    NOT NULL,
//!     chunk_idx        INT     NOT NULL,
//!     data             BYTEA   NOT NULL,
//!     size_bytes       BIGINT  NOT NULL,    -- total object size, duplicated per chunk
//!     etag             TEXT    NOT NULL,    -- total-object etag, duplicated per chunk
//!     last_modified_us BIGINT  NOT NULL,
//!     metadata_json    TEXT,
//!     PRIMARY KEY (key HASH, chunk_idx)
//! );
//! ```
//!
//! Each object is split into chunks of `YB_CHUNK_SIZE` bytes (100 MiB) and stored
//! as one row per chunk, ordered by `chunk_idx`. This works around YB's default
//! `rpc_max_message_size` (256 MiB) which can't accept a single bytea larger
//! than that. The `size_bytes` / `etag` / `last_modified_us` / `metadata_json`
//! columns are denormalized — copied onto every chunk row for the same key —
//! so a `head` lookup is a single-row read.
//!
//! `last_modified_us` is microseconds since the Unix epoch, written by the
//! server with `(extract(epoch from now()) * 1000000)::bigint`. Metadata is
//! kept as a serialized JSON string in `metadata_json`; the column is TEXT so
//! that no extra `tokio_postgres` features are required.
//!
//! `list_versions` and `time_travel_recover` are intentionally unsupported,
//! matching the posture of `LocalFs`.

use std::collections::{HashMap, HashSet};
use std::num::NonZeroU32;
use std::sync::Arc;
use std::time::{Duration, SystemTime};

use anyhow::{Context, anyhow, bail};
use bytes::Bytes;
use futures::TryStreamExt;
use futures::stream::Stream;
use tokio_postgres::types::ToSql;
use tokio_util::sync::CancellationToken;

use crate::config::YbStoreConfig;
use crate::error::{Cancelled, TimeoutOrCancel};
use crate::metrics::RequestKind;
use crate::{
    ConcurrencyLimiter, Download, DownloadError, DownloadOpts, DownloadStream, Etag, Listing,
    ListingMode, ListingObject, REMOTE_STORAGE_PREFIX_SEPARATOR, RemotePath, RemoteStorage,
    StorageMetadata, TimeTravelError, VersionListing,
};

/// Cap on the number of keys we advertise as deletable in a single
/// `delete_objects` call. The SQL layer can handle larger arrays, but this
/// matches what the other backends do.
const YB_MAX_KEYS_PER_DELETE: usize = 1000;

/// Size of each chunk row. Must stay below YB's `rpc_max_message_size`
/// default (256 MiB). 100 MiB keeps the chunk count low for typical
/// pageserver layer files (1 chunk for the common case, 3 chunks for an
/// upper-bound ~290 MiB layer).
const YB_CHUNK_SIZE: usize = 100 * 1024 * 1024;

pub struct YbStore {
    client: Arc<tokio_postgres::Client>,
    table_name: String,
    /// Configured prefix, normalized to always end in `/` when non-empty.
    prefix: Option<String>,
    timeout: Duration,
    concurrency_limiter: ConcurrencyLimiter,
}

impl std::fmt::Debug for YbStore {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("YbStore")
            .field("table_name", &self.table_name)
            .field("prefix", &self.prefix)
            .field("timeout", &self.timeout)
            .finish()
    }
}

impl YbStore {
    pub async fn new(config: &YbStoreConfig, timeout: Duration) -> anyhow::Result<Self> {
        let mut pg_config = tokio_postgres::Config::new();
        pg_config
            .host(&config.host)
            .port(config.port)
            .user(&config.user)
            .dbname(&config.dbname)
            .application_name("pageserver:yb_store");

        if let Some(env_name) = &config.password_env {
            let password = std::env::var(env_name).with_context(|| {
                format!("YbStore password env var `{env_name}` is not set")
            })?;
            pg_config.password(password);
        }

        let (client, connection) = pg_config
            .connect(tokio_postgres::NoTls)
            .await
            .with_context(|| {
                format!(
                    "failed to connect to YugabyteDB at {}:{} as user `{}`",
                    config.host, config.port, config.user
                )
            })?;

        tokio::spawn(async move {
            if let Err(e) = connection.await {
                tracing::warn!("YbStore connection ended with error: {e}");
            }
        });

        // Best-effort: enable YugabyteDB's per-session upsert mode so plain
        // `INSERT` skips DocDB's read-before-write conflict check. With this
        // set, a duplicate-PK insert silently overwrites instead of erroring,
        // which lets us drop `ON CONFLICT` (and avoid the round-trip + storage
        // engine read it forces). Vanilla Postgres doesn't have this GUC, so
        // an "unrecognized parameter" error here is expected and ignored.
        if let Err(e) = client
            .execute("SET yb_enable_upsert_mode = true", &[])
            .await
        {
            tracing::debug!(
                "YbStore: SET yb_enable_upsert_mode failed (likely vanilla Postgres): {e}"
            );
        }

        let prefix = config.prefix.as_ref().map(|p| {
            if p.is_empty() || p.ends_with(REMOTE_STORAGE_PREFIX_SEPARATOR) {
                p.clone()
            } else {
                format!("{p}/")
            }
        });

        Ok(Self {
            client: Arc::new(client),
            table_name: config.table_name.clone(),
            prefix,
            timeout,
            concurrency_limiter: ConcurrencyLimiter::new(config.concurrency_limit.get()),
        })
    }

    pub fn table_name(&self) -> &str {
        &self.table_name
    }

    fn full_key(&self, p: &RemotePath) -> String {
        match &self.prefix {
            Some(prefix) => format!("{prefix}{}", p.get_path().as_str()),
            None => p.get_path().as_str().to_string(),
        }
    }

    fn db_key_to_remote_path(&self, key: &str) -> anyhow::Result<RemotePath> {
        let stripped = match &self.prefix {
            Some(prefix) => key
                .strip_prefix(prefix.as_str())
                .ok_or_else(|| anyhow!("key `{key}` does not start with configured prefix"))?,
            None => key,
        };
        RemotePath::from_string(stripped)
    }

    fn full_prefix(&self, prefix: Option<&RemotePath>) -> String {
        let configured = self.prefix.as_deref().unwrap_or("");
        match prefix {
            Some(p) => format!("{configured}{}", p.get_path().as_str()),
            None => configured.to_string(),
        }
    }

    fn compute_etag(data: &[u8]) -> String {
        format!("{:x}", md5::compute(data))
    }

    /// Extract `(tenant_id, timeline_id)` from a `blobs` key by scanning
    /// the path for Neon-id-shaped tokens. Returns empty strings when the
    /// path doesn't carry the ids (e.g. tenant-scope-only blobs have an
    /// empty `timeline_id`; future generic blobs may have neither).
    ///
    /// A Neon id is 32 hex characters, optionally followed by `-<count>-<num>`
    /// for sharded tenants (e.g. `c8178f5e…1f966a-0004-0002`). The function
    /// is heuristic — it returns the first two such tokens it finds, in
    /// path order — which is sufficient for every key the pageserver and
    /// safekeeper write today and benign (both empty) for any future key
    /// that doesn't follow the convention.
    fn extract_neon_ids(key: &str) -> (String, String) {
        let mut found = Vec::with_capacity(2);
        for part in key.split('/') {
            if Self::looks_like_neon_id(part) {
                found.push(part.to_string());
                if found.len() == 2 {
                    break;
                }
            }
        }
        let tenant = found.first().cloned().unwrap_or_default();
        let timeline = found.get(1).cloned().unwrap_or_default();
        (tenant, timeline)
    }

    /// Map a full key to the four storage columns:
    ///   `(namespace, tenant_id, timeline_id, key_body)`
    ///
    /// - `namespace`: `"pageserver"` / `"safekeeper"`. Derived from
    ///   `YbStoreConfig::prefix`.
    /// - `tenant_id` / `timeline_id`: first two Neon-id-shaped tokens in
    ///   the path (empty when absent).
    /// - `key_body`: the path *after the configured prefix is stripped*,
    ///   with these segments **dropped**:
    ///     - the literal `tenants` and `timelines` markers,
    ///     - the tenant_id and timeline_id values themselves.
    ///   What's left is the trailing discriminator the object would have
    ///   relative to its `(namespace, tenant_id, timeline_id)` bucket —
    ///   e.g. `000000-FFF__layer-1`, `tenant-manifest-1.json`,
    ///   `safekeeper-control`, or `000000010000000000000005`.
    ///
    /// [`reconstruct_key`] is the inverse — it knows the per-namespace
    /// layout convention and reassembles the original path.
    fn parse_key(&self, full_key: &str) -> (&'static str, String, String, String) {
        let cfg_prefix = self.prefix.as_deref().unwrap_or("");
        let rest = full_key.strip_prefix(cfg_prefix).unwrap_or(full_key);
        let namespace: &'static str = if cfg_prefix.contains("safekeeper")
            || full_key.starts_with("safekeeper")
        {
            "safekeeper"
        } else {
            "pageserver"
        };
        let (tenant_id, timeline_id) = Self::extract_neon_ids(rest);

        let key_body: String = rest
            .split('/')
            .filter(|seg| {
                *seg != "tenants"
                    && *seg != "timelines"
                    && (tenant_id.is_empty() || *seg != tenant_id)
                    && (timeline_id.is_empty() || *seg != timeline_id)
            })
            .collect::<Vec<_>>()
            .join("/");

        (namespace, tenant_id, timeline_id, key_body)
    }

    /// Inverse of [`parse_key`]: rebuild the full key from
    /// `(namespace, tenant_id, timeline_id, key_body)` using the
    /// per-namespace layout convention.
    ///
    /// Layout reference:
    /// - `pageserver`, timeline known: `<prefix>tenants/<T>/timelines/<TL>/<body>`
    /// - `pageserver`, no timeline:    `<prefix>tenants/<T>/<body>`
    /// - `safekeeper`, body is `safekeeper-control`: `<prefix>safekeeper-control/<T>/<TL>`
    /// - `safekeeper`, anything else (WAL backup):   `<prefix><T>/<TL>/<body>`
    fn reconstruct_key(
        &self,
        namespace: &str,
        tenant_id: &str,
        timeline_id: &str,
        key_body: &str,
    ) -> String {
        let cfg_prefix = self.prefix.as_deref().unwrap_or("");
        let body = if namespace == "safekeeper" {
            if key_body == "safekeeper-control" {
                if timeline_id.is_empty() {
                    format!("safekeeper-control/{tenant_id}")
                } else {
                    format!("safekeeper-control/{tenant_id}/{timeline_id}")
                }
            } else if timeline_id.is_empty() {
                if key_body.is_empty() {
                    tenant_id.to_string()
                } else {
                    format!("{tenant_id}/{key_body}")
                }
            } else if key_body.is_empty() {
                format!("{tenant_id}/{timeline_id}")
            } else {
                format!("{tenant_id}/{timeline_id}/{key_body}")
            }
        } else if tenant_id.is_empty() {
            // pageserver-namespace blob with no tenant — unusual but we shouldn't
            // synthesize a `tenants/` segment if there's no value to fill in
            key_body.to_string()
        } else if timeline_id.is_empty() {
            if key_body.is_empty() {
                format!("tenants/{tenant_id}")
            } else {
                format!("tenants/{tenant_id}/{key_body}")
            }
        } else if key_body.is_empty() {
            format!("tenants/{tenant_id}/timelines/{timeline_id}")
        } else {
            format!("tenants/{tenant_id}/timelines/{timeline_id}/{key_body}")
        };
        format!("{cfg_prefix}{body}")
    }

    fn looks_like_neon_id(s: &str) -> bool {
        // bare 32-hex-char tenant/timeline id
        if s.len() == 32 && s.chars().all(|c| c.is_ascii_hexdigit()) {
            return true;
        }
        // sharded tenant id: 32-hex + "-XXXX-XXXX" (count + shard number, each 4 hex)
        if s.len() == 32 + 1 + 4 + 1 + 4 && s.as_bytes()[32] == b'-' && s.as_bytes()[37] == b'-' {
            let core = &s[..32];
            let count = &s[33..37];
            let num = &s[38..];
            if core.chars().all(|c| c.is_ascii_hexdigit())
                && count.chars().all(|c| c.is_ascii_hexdigit())
                && num.chars().all(|c| c.is_ascii_hexdigit())
            {
                return true;
            }
        }
        false
    }

    async fn with_timeout<F, T>(&self, fut: F, cancel: &CancellationToken) -> anyhow::Result<T>
    where
        F: std::future::Future<Output = Result<T, tokio_postgres::Error>>,
    {
        tokio::select! {
            res = fut => res.map_err(anyhow::Error::from),
            _ = tokio::time::sleep(self.timeout) => Err(TimeoutOrCancel::Timeout.into()),
            _ = cancel.cancelled() => Err(TimeoutOrCancel::Cancel.into()),
        }
    }

    async fn with_timeout_download<F, T>(
        &self,
        fut: F,
        cancel: &CancellationToken,
    ) -> Result<T, DownloadError>
    where
        F: std::future::Future<Output = Result<T, tokio_postgres::Error>>,
    {
        tokio::select! {
            res = fut => res.map_err(|e| DownloadError::Other(e.into())),
            _ = tokio::time::sleep(self.timeout) => Err(DownloadError::Timeout),
            _ = cancel.cancelled() => Err(DownloadError::Cancelled),
        }
    }

    async fn permit(
        &self,
        kind: RequestKind,
        cancel: &CancellationToken,
    ) -> Result<tokio::sync::SemaphorePermit<'_>, Cancelled> {
        let acquire = self.concurrency_limiter.acquire(kind);
        tokio::select! {
            permit = acquire => Ok(permit.expect("semaphore is never closed")),
            _ = cancel.cancelled() => Err(Cancelled),
        }
    }
}

fn system_time_from_micros(micros: i64) -> SystemTime {
    if micros >= 0 {
        SystemTime::UNIX_EPOCH + Duration::from_micros(micros as u64)
    } else {
        SystemTime::UNIX_EPOCH - Duration::from_micros((-micros) as u64)
    }
}

impl RemoteStorage for YbStore {
    fn list_streaming(
        &self,
        prefix: Option<&RemotePath>,
        mode: ListingMode,
        max_keys: Option<NonZeroU32>,
        cancel: &CancellationToken,
    ) -> impl Stream<Item = Result<Listing, DownloadError>> + Send {
        let listing = self.list(prefix, mode, max_keys, cancel);
        futures::stream::once(listing)
    }

    async fn list(
        &self,
        prefix: Option<&RemotePath>,
        mode: ListingMode,
        max_keys: Option<NonZeroU32>,
        cancel: &CancellationToken,
    ) -> Result<Listing, DownloadError> {
        let _permit = self
            .permit(RequestKind::List, cancel)
            .await
            .map_err(|_| DownloadError::Cancelled)?;

        let full_prefix = self.full_prefix(prefix);

        // Parse the prefix into the new column form. When the prefix
        // contains tenant/timeline ids (the common case — pageserver
        // lists by `tenants/<X>/timelines/<Y>/…`), we filter on those
        // columns directly so the query hits a single hash tablet.
        // The LIKE pattern is the **placeholder form** of the prefix
        // since stored keys carry `{T}` / `{TL}` placeholders.
        let (namespace, tenant_id, timeline_id, key_body_prefix) =
            self.parse_key(&full_prefix);
        let like_pattern = format!("{}%", escape_like(&key_body_prefix));

        let mut where_clauses: Vec<String> =
            vec!["namespace = $1::text::blob_namespace".to_string()];
        let mut bind_params: Vec<Box<dyn ToSql + Sync + Send>> =
            vec![Box::new(namespace)];
        if !tenant_id.is_empty() {
            where_clauses.push(format!("tenant_id = ${}", bind_params.len() + 1));
            bind_params.push(Box::new(tenant_id.clone()));
        }
        if !timeline_id.is_empty() {
            where_clauses.push(format!("timeline_id = ${}", bind_params.len() + 1));
            bind_params.push(Box::new(timeline_id.clone()));
        }
        let like_idx = bind_params.len() + 1;
        where_clauses.push(format!("key LIKE ${like_idx} ESCAPE '\\'"));
        bind_params.push(Box::new(like_pattern));
        where_clauses.push("chunk_idx = 0".to_string());

        let mut sql = format!(
            "SELECT tenant_id, timeline_id, key, size_bytes, last_modified_us \
             FROM {} WHERE {} \
             ORDER BY tenant_id, timeline_id, key",
            quote_ident(&self.table_name),
            where_clauses.join(" AND "),
        );
        if let Some(mk) = max_keys {
            let limit_idx = bind_params.len() + 1;
            sql += &format!(" LIMIT ${limit_idx}");
            bind_params.push(Box::new(mk.get() as i64));
        }

        let rows = {
            let fut = self.client.query_raw(&sql, bind_params);
            let row_stream = self
                .with_timeout_download(fut, cancel)
                .await?;
            use futures::StreamExt;
            let mut row_stream = std::pin::pin!(row_stream);
            let mut acc = Vec::new();
            while let Some(r) = row_stream.next().await {
                acc.push(r.map_err(|e| DownloadError::Other(e.into()))?);
            }
            acc
        };

        let mut result = Listing::default();

        match mode {
            ListingMode::NoDelimiter => {
                for row in rows {
                    let row_tenant: String = row.get("tenant_id");
                    let row_timeline: String = row.get("timeline_id");
                    let row_key_body: String = row.get("key");
                    let size_bytes: i64 = row.get("size_bytes");
                    let last_modified_us: i64 = row.get("last_modified_us");
                    let full_key = self.reconstruct_key(namespace, &row_tenant, &row_timeline, &row_key_body);
                    let remote_path = self
                        .db_key_to_remote_path(&full_key)
                        .map_err(DownloadError::Other)?;
                    result.keys.push(ListingObject {
                        key: remote_path,
                        last_modified: system_time_from_micros(last_modified_us),
                        size: size_bytes.max(0) as u64,
                    });
                }
            }
            ListingMode::WithDelimiter => {
                // Mirror `LocalFs`: if the caller's prefix does not end in
                // `/`, treat its parent directory as the "group root" so that
                // sibling keys still group correctly.
                let listing_prefix = match prefix {
                    Some(p) => {
                        let s = p.get_path().as_str().to_string();
                        if s.ends_with(REMOTE_STORAGE_PREFIX_SEPARATOR) || s.is_empty() {
                            full_prefix.clone()
                        } else {
                            let configured = self.prefix.as_deref().unwrap_or("");
                            match s.rfind(REMOTE_STORAGE_PREFIX_SEPARATOR) {
                                Some(i) => format!("{configured}{}", &s[..=i]),
                                None => configured.to_string(),
                            }
                        }
                    }
                    None => full_prefix.clone(),
                };

                let mut prefixes: HashSet<String> = HashSet::new();
                for row in rows {
                    let row_tenant: String = row.get("tenant_id");
                    let row_timeline: String = row.get("timeline_id");
                    let row_key_body: String = row.get("key");
                    let size_bytes: i64 = row.get("size_bytes");
                    let last_modified_us: i64 = row.get("last_modified_us");

                    let full_key = self.reconstruct_key(namespace, &row_tenant, &row_timeline, &row_key_body);
                    let relative = match full_key.strip_prefix(listing_prefix.as_str()) {
                        Some(r) => r.to_string(),
                        None => continue,
                    };

                    if let Some(i) = relative.find(REMOTE_STORAGE_PREFIX_SEPARATOR) {
                        prefixes.insert(relative[..i].to_string());
                    } else {
                        result.keys.push(ListingObject {
                            key: RemotePath::from_string(&relative)
                                .map_err(DownloadError::Other)?,
                            last_modified: system_time_from_micros(last_modified_us),
                            size: size_bytes.max(0) as u64,
                        });
                    }
                }

                result.prefixes = prefixes
                    .into_iter()
                    .map(|s| RemotePath::from_string(&s))
                    .collect::<anyhow::Result<Vec<_>>>()
                    .map_err(DownloadError::Other)?;
            }
        }

        Ok(result)
    }

    async fn list_versions(
        &self,
        _prefix: Option<&RemotePath>,
        _mode: ListingMode,
        _max_keys: Option<NonZeroU32>,
        _cancel: &CancellationToken,
    ) -> Result<VersionListing, DownloadError> {
        Err(DownloadError::Other(anyhow!(
            "list_versions is not supported by YbStore"
        )))
    }

    async fn head_object(
        &self,
        key: &RemotePath,
        cancel: &CancellationToken,
    ) -> Result<ListingObject, DownloadError> {
        let _permit = self
            .permit(RequestKind::Head, cancel)
            .await
            .map_err(|_| DownloadError::Cancelled)?;

        let db_key = self.full_key(key);
        let (namespace, tenant_id, timeline_id, key_body) = self.parse_key(&db_key);
        // chunk_idx=0 always exists for any object (including zero-byte ones),
        // and carries the denormalized size/etag/timestamp — one-row lookup.
        let sql = format!(
            "SELECT size_bytes, last_modified_us FROM {} \
             WHERE tenant_id = $1 AND timeline_id = $2 \
               AND namespace = $3::text::blob_namespace AND key = $4 AND chunk_idx = 0",
            quote_ident(&self.table_name)
        );
        let params: [&(dyn ToSql + Sync); 4] =
            [&tenant_id, &timeline_id, &namespace, &key_body];
        let row_opt = self
            .with_timeout_download(self.client.query_opt(&sql, &params), cancel)
            .await?;
        let row = row_opt.ok_or(DownloadError::NotFound)?;
        let size_bytes: i64 = row.get("size_bytes");
        let last_modified_us: i64 = row.get("last_modified_us");
        Ok(ListingObject {
            key: key.clone(),
            last_modified: system_time_from_micros(last_modified_us),
            size: size_bytes.max(0) as u64,
        })
    }

    async fn upload(
        &self,
        from: impl Stream<Item = std::io::Result<Bytes>> + Send + Sync + 'static,
        data_size_bytes: usize,
        to: &RemotePath,
        metadata: Option<StorageMetadata>,
        cancel: &CancellationToken,
    ) -> anyhow::Result<()> {
        let _permit = self.permit(RequestKind::Put, cancel).await?;

        let chunks: Vec<Bytes> = from
            .try_collect()
            .await
            .context("read upload stream")?;
        let mut buffer = Vec::with_capacity(data_size_bytes);
        for chunk in chunks {
            buffer.extend_from_slice(&chunk);
        }

        if buffer.len() < data_size_bytes {
            bail!(
                "upload stream shorter than expected: got {} bytes, expected {}",
                buffer.len(),
                data_size_bytes
            );
        }
        if buffer.len() > data_size_bytes {
            buffer.truncate(data_size_bytes);
        }

        let etag = Self::compute_etag(&buffer);
        let size: i64 = i64::try_from(buffer.len()).context("data size exceeds i64::MAX")?;
        let db_key = self.full_key(to);
        let metadata_json: Option<String> = match metadata {
            Some(StorageMetadata(m)) => Some(
                serde_json::to_string(&m).context("serialize storage metadata")?,
            ),
            None => None,
        };

        // Delete any prior rows for this key first — this handles overwrite +
        // chunk-count shrinking (the upsert GUC handles same-PK conflict but
        // wouldn't remove rows for keys that no longer exist in the new write).
        let table = quote_ident(&self.table_name);
        let (namespace, tenant_id, timeline_id, key_body) = self.parse_key(&db_key);
        let delete_sql = format!(
            "DELETE FROM {table} \
             WHERE tenant_id = $1 AND timeline_id = $2 \
               AND namespace = $3::text::blob_namespace AND key = $4"
        );
        {
            let params: [&(dyn ToSql + Sync); 4] =
                [&tenant_id, &timeline_id, &namespace, &key_body];
            self.with_timeout(self.client.execute(&delete_sql, &params), cancel)
                .await
                .with_context(|| format!("YbStore upload: pre-delete `{db_key}`"))?;
        }

        let now_us: i64 = SystemTime::now()
            .duration_since(SystemTime::UNIX_EPOCH)
            .map(|d| d.as_micros() as i64)
            .unwrap_or(0);

        // Plain INSERT; relies on `yb_enable_upsert_mode = true` (set at
        // session start) to skip the read-before-write check on YB. Each chunk
        // is its own statement so the wire-level message stays well below
        // `rpc_max_message_size`, but we fire them concurrently via
        // `execute_raw` + `try_join_all` — `tokio_postgres`'s Client
        // multiplexes all the Bind/Execute messages on the connection without
        // serial round-trip waits, so N inserts cost ~one round-trip's worth
        // of latency.
        let insert_sql = format!(
            "INSERT INTO {table} \
             (tenant_id, timeline_id, namespace, key, chunk_idx, data, size_bytes, \
              etag, last_modified_us, metadata_json) \
             VALUES ($1, $2, $3::text::blob_namespace, $4, $5, $6, $7, $8, $9, $10)"
        );

        // Materialize all chunk rows up front so each pipelined future can own
        // its params (no borrow-into-loop-local issues).
        let row_chunks: Vec<(i32, Vec<u8>)> = if buffer.is_empty() {
            vec![(0i32, Vec::new())]
        } else {
            buffer
                .chunks(YB_CHUNK_SIZE)
                .enumerate()
                .map(|(i, c)| (i as i32, c.to_vec()))
                .collect()
        };

        let pipeline_fut = async {
            let futures = row_chunks.into_iter().map(|(idx, chunk)| {
                let params: Vec<Box<dyn ToSql + Sync + Send>> = vec![
                    Box::new(tenant_id.clone()),
                    Box::new(timeline_id.clone()),
                    Box::new(namespace),
                    Box::new(key_body.clone()),
                    Box::new(idx),
                    Box::new(chunk),
                    Box::new(size),
                    Box::new(etag.clone()),
                    Box::new(now_us),
                    Box::new(metadata_json.clone()),
                ];
                self.client.execute_raw(&insert_sql, params)
            });
            futures::future::try_join_all(futures).await?;
            Ok::<_, tokio_postgres::Error>(())
        };

        self.with_timeout(pipeline_fut, cancel)
            .await
            .with_context(|| format!("YbStore upload of `{db_key}`"))?;
        Ok(())
    }

    async fn download(
        &self,
        from: &RemotePath,
        opts: &DownloadOpts,
        cancel: &CancellationToken,
    ) -> Result<Download, DownloadError> {
        let _permit = self
            .permit(RequestKind::Get, cancel)
            .await
            .map_err(|_| DownloadError::Cancelled)?;

        let db_key = self.full_key(from);

        let (namespace, tenant_id, timeline_id, key_body) = self.parse_key(&db_key);

        // Phase 1: cheap single-row lookup to get the denormalized metadata
        // (size_bytes / etag / last_modified_us / metadata_json) sitting on
        // chunk_idx = 0. Lets us honor If-None-Match and compute the byte
        // range without ever loading bulk data into memory.
        let head_sql = format!(
            "SELECT size_bytes, etag, last_modified_us, metadata_json \
             FROM {} WHERE tenant_id = $1 AND timeline_id = $2 \
               AND namespace = $3::text::blob_namespace AND key = $4 \
               AND chunk_idx = 0",
            quote_ident(&self.table_name)
        );
        let head_params: [&(dyn ToSql + Sync); 4] =
            [&tenant_id, &timeline_id, &namespace, &key_body];
        let head_opt = self
            .with_timeout_download(self.client.query_opt(&head_sql, &head_params), cancel)
            .await?;
        let head = head_opt.ok_or(DownloadError::NotFound)?;
        let total_size: i64 = head.get("size_bytes");
        let etag_str: String = head.get("etag");
        let last_modified_us: i64 = head.get("last_modified_us");
        let metadata_json: Option<String> = head.get("metadata_json");

        if let Some(if_none_match) = &opts.etag {
            if if_none_match.to_string() == etag_str {
                return Err(DownloadError::Unmodified);
            }
        }

        let metadata = match metadata_json {
            Some(s) => {
                let m: HashMap<String, String> =
                    serde_json::from_str(&s).map_err(|e| DownloadError::Other(e.into()))?;
                Some(StorageMetadata(m))
            }
            None => None,
        };

        let total_size_u64 = total_size.max(0) as u64;
        let (start_byte, end_byte) = match opts.byte_range() {
            None => (0u64, total_size_u64),
            Some((s, Some(e))) => (s.min(total_size_u64), e.min(total_size_u64)),
            Some((s, None)) => (s.min(total_size_u64), total_size_u64),
        };

        // Empty range (after clamping) — return an empty download stream.
        if start_byte >= end_byte {
            let empty: DownloadStream =
                Box::pin(futures::stream::empty::<std::io::Result<Bytes>>());
            return Ok(Download {
                download_stream: empty,
                last_modified: system_time_from_micros(last_modified_us),
                etag: Etag::from(etag_str),
                metadata,
            });
        }

        // Phase 2: stream chunks. Chunks are predictably sized
        // (`YB_CHUNK_SIZE` bytes each except possibly the last) so we can
        // compute which chunk_idx values overlap [start_byte, end_byte) and
        // how much to trim off the first and last.
        let chunk_size = YB_CHUNK_SIZE as u64;
        let start_chunk = (start_byte / chunk_size) as i32;
        // end is exclusive; compute the chunk holding the last byte
        let last_byte_inclusive = end_byte - 1;
        let end_chunk = (last_byte_inclusive / chunk_size) as i32;
        let start_off_in_first = (start_byte % chunk_size) as usize;
        let end_off_in_last = ((last_byte_inclusive % chunk_size) + 1) as usize;

        let client = self.client.clone();
        let table = self.table_name.clone();
        let stream_sql = format!(
            "SELECT chunk_idx, data FROM {} \
             WHERE tenant_id = $1 AND timeline_id = $2 \
               AND namespace = $3::text::blob_namespace AND key = $4 \
               AND chunk_idx >= $5 AND chunk_idx <= $6 \
             ORDER BY chunk_idx",
            quote_ident(&table)
        );
        let timeout = self.timeout;
        let stream_tenant = tenant_id.clone();
        let stream_timeline = timeline_id.clone();
        let stream_namespace = namespace;
        let stream_key = key_body.clone();

        // Open the row stream first (so any query error fails the
        // `download()` call cleanly with `DownloadError::Other` rather than
        // surfacing as an io::Error mid-stream after the caller has already
        // started writing to disk).
        let params: Vec<Box<dyn ToSql + Sync + Send>> = vec![
            Box::new(stream_tenant),
            Box::new(stream_timeline),
            Box::new(stream_namespace),
            Box::new(stream_key),
            Box::new(start_chunk),
            Box::new(end_chunk),
        ];
        let row_stream = match tokio::time::timeout(
            timeout,
            client.query_raw(&stream_sql, params),
        )
        .await
        {
            Ok(Ok(rs)) => rs,
            Ok(Err(e)) => return Err(DownloadError::Other(e.into())),
            Err(_) => return Err(DownloadError::Timeout),
        };

        // `row_stream` is a `tokio_postgres::RowStream` that pulls bytes off
        // the wire as we poll it — we never have more than one chunk's
        // `data` resident.
        let download_stream: DownloadStream = Box::pin(async_stream::stream! {
            let mut row_stream = std::pin::pin!(row_stream);
            while let Some(row_res) = futures::StreamExt::next(&mut row_stream).await {
                match row_res {
                    Ok(row) => {
                        let idx: i32 = row.get("chunk_idx");
                        let data: Vec<u8> = row.get("data");
                        let len = data.len();
                        let lo = if idx == start_chunk { start_off_in_first.min(len) } else { 0 };
                        let hi = if idx == end_chunk { end_off_in_last.min(len) } else { len };
                        if lo < hi {
                            // `Bytes::from(Vec<u8>)` is zero-copy; the slice
                            // is then a pointer + length adjustment.
                            yield Ok(Bytes::from(data).slice(lo..hi));
                        }
                    }
                    Err(e) => {
                        yield Err(std::io::Error::other(e));
                        return;
                    }
                }
            }
        });

        Ok(Download {
            download_stream,
            last_modified: system_time_from_micros(last_modified_us),
            etag: Etag::from(etag_str),
            metadata,
        })
    }

    async fn delete(&self, path: &RemotePath, cancel: &CancellationToken) -> anyhow::Result<()> {
        let _permit = self.permit(RequestKind::Delete, cancel).await?;
        let db_key = self.full_key(path);
        let (namespace, tenant_id, timeline_id, key_body) = self.parse_key(&db_key);
        let sql = format!(
            "DELETE FROM {} \
             WHERE tenant_id = $1 AND timeline_id = $2 \
               AND namespace = $3::text::blob_namespace AND key = $4",
            quote_ident(&self.table_name)
        );
        let params: [&(dyn ToSql + Sync); 4] =
            [&tenant_id, &timeline_id, &namespace, &key_body];
        self.with_timeout(self.client.execute(&sql, &params), cancel)
            .await
            .with_context(|| format!("YbStore delete of `{db_key}`"))?;
        Ok(())
    }

    async fn delete_objects(
        &self,
        paths: &[RemotePath],
        cancel: &CancellationToken,
    ) -> anyhow::Result<()> {
        if paths.is_empty() {
            return Ok(());
        }
        let _permit = self.permit(RequestKind::Delete, cancel).await?;
        let sql = format!(
            "DELETE FROM {} \
             WHERE tenant_id = $1 AND timeline_id = $2 \
               AND namespace = $3::text::blob_namespace AND key = $4",
            quote_ident(&self.table_name)
        );

        // Each key has its own (namespace, tenant, timeline, key_body)
        // tuple, so we fan out one DELETE per object pipelined via
        // `execute_raw` + `try_join_all`. Same wire-level batching trick
        // as `upload`; each DELETE hits one hash tablet directly.
        let parsed: Vec<(&'static str, String, String, String)> = paths
            .iter()
            .map(|p| self.parse_key(&self.full_key(p)))
            .collect();

        let pipeline_fut = async {
            let futures = parsed.into_iter().map(|(ns, tenant, timeline, key_body)| {
                let params: Vec<Box<dyn ToSql + Sync + Send>> = vec![
                    Box::new(tenant),
                    Box::new(timeline),
                    Box::new(ns),
                    Box::new(key_body),
                ];
                self.client.execute_raw(&sql, params)
            });
            futures::future::try_join_all(futures).await?;
            Ok::<_, tokio_postgres::Error>(())
        };
        self.with_timeout(pipeline_fut, cancel)
            .await
            .context("YbStore delete_objects")?;
        Ok(())
    }

    fn max_keys_per_delete(&self) -> usize {
        YB_MAX_KEYS_PER_DELETE
    }

    async fn copy(
        &self,
        from: &RemotePath,
        to: &RemotePath,
        cancel: &CancellationToken,
    ) -> anyhow::Result<()> {
        let _permit = self.permit(RequestKind::Copy, cancel).await?;
        let from_key = self.full_key(from);
        let to_key = self.full_key(to);
        let (from_ns, from_tenant, from_timeline, from_body) = self.parse_key(&from_key);
        let (to_ns, to_tenant, to_timeline, to_body) = self.parse_key(&to_key);
        let table = quote_ident(&self.table_name);
        // Wipe the destination first so a copy that shrinks chunk count is
        // clean, then copy all source chunk rows over.
        let delete_dst = format!(
            "DELETE FROM {table} \
             WHERE tenant_id = $1 AND timeline_id = $2 \
               AND namespace = $3::text::blob_namespace AND key = $4"
        );
        {
            let params: [&(dyn ToSql + Sync); 4] =
                [&to_tenant, &to_timeline, &to_ns, &to_body];
            self.with_timeout(self.client.execute(&delete_dst, &params), cancel)
                .await
                .with_context(|| format!("YbStore copy: pre-delete `{to_key}`"))?;
        }
        let insert_sql = format!(
            "INSERT INTO {table} \
             (tenant_id, timeline_id, namespace, key, chunk_idx, data, \
              size_bytes, etag, last_modified_us, metadata_json) \
             SELECT $5, $6, $7::text::blob_namespace, $8, chunk_idx, data, \
                    size_bytes, etag, \
                    (extract(epoch from now()) * 1000000)::bigint, metadata_json \
             FROM {table} \
             WHERE tenant_id = $1 AND timeline_id = $2 \
               AND namespace = $3::text::blob_namespace AND key = $4"
        );
        let params: [&(dyn ToSql + Sync); 8] = [
            &from_tenant,
            &from_timeline,
            &from_ns,
            &from_body,
            &to_tenant,
            &to_timeline,
            &to_ns,
            &to_body,
        ];
        let affected = self
            .with_timeout(self.client.execute(&insert_sql, &params), cancel)
            .await
            .with_context(|| format!("YbStore copy of `{from_key}` -> `{to_key}`"))?;
        if affected == 0 {
            bail!("YbStore copy: source key `{from_key}` not found");
        }
        Ok(())
    }

    async fn time_travel_recover(
        &self,
        _prefix: Option<&RemotePath>,
        _timestamp: SystemTime,
        _done_if_after: SystemTime,
        _cancel: &CancellationToken,
        _complexity_limit: Option<NonZeroU32>,
    ) -> Result<(), TimeTravelError> {
        Err(TimeTravelError::Unimplemented)
    }
}

/// Quote a SQL identifier with double quotes, escaping embedded double quotes.
/// The identifier comes from config so this is defense-in-depth.
fn quote_ident(ident: &str) -> String {
    let escaped = ident.replace('"', "\"\"");
    format!("\"{escaped}\"")
}

/// Escape `_` and `%` in a LIKE-pattern literal so that callers' raw key
/// fragments aren't interpreted as wildcards. The accompanying SQL uses
/// `ESCAPE '\\'`.
fn escape_like(s: &str) -> String {
    let mut out = String::with_capacity(s.len());
    for c in s.chars() {
        match c {
            '\\' | '%' | '_' => {
                out.push('\\');
                out.push(c);
            }
            _ => out.push(c),
        }
    }
    out
}
