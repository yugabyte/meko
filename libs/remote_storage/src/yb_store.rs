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
    /// Auto-reconnecting wrapper around `tokio_postgres::Client`. See
    /// [`crate::YsqlClient`] for the reconnect/retry semantics.
    client: Arc<crate::YsqlClient>,
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
        // YsqlClient owns reconnect logic, runs `SET yb_enable_upsert_mode`
        // on every reopen, and dispatches sslmode + sslrootcert internally.
        // Same factory used by the safekeeper's WAL-mirror connection so
        // both honour the same TLS rules.
        let client = crate::YsqlClient::from_yb_config(config, "pageserver:yb_store")
            .await
            .with_context(|| {
                format!(
                    "failed to connect to YugabyteDB at {}:{} as user `{}` (sslmode={:?})",
                    config.host, config.port, config.user, config.sslmode
                )
            })?;

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

    /// Fast path for "what timelines does this tenant own?" — straight
    /// `SELECT timeline_id FROM storage_controller.timelines WHERE
    /// tenant_id = $1`. The pageserver previously implemented this by
    /// `LIST`ing the `tenants/<T>/timelines/` prefix in `blobs` and
    /// parsing the result, which on YB fanned out across every
    /// (tenant, timeline) hash tablet and returned every layer row.
    ///
    /// The `timelines` table is owned by storage_controller's schema in
    /// the same DB, so we can hit it with the YbStore connection — a
    /// single PK-hash tablet lookup.
    ///
    /// Returns the bare timeline IDs as strings; the caller parses them
    /// (we keep this side decoupled from `utils::id::TimelineId` to
    /// avoid a new dep).
    pub async fn list_timelines_for_tenant_via_controller(
        &self,
        tenant_id: &str,
    ) -> anyhow::Result<Vec<String>> {
        let sql = "SELECT timeline_id FROM storage_controller.timelines \
                   WHERE tenant_id = $1 AND deleted_at IS NULL";
        let client = self.client.current().await?;
        let rows = client.query(sql, &[&tenant_id]).await?;
        Ok(rows.into_iter().map(|r| r.get::<_, String>("timeline_id")).collect())
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

    /// Strip a `-<8 hex>` generation marker from a blob key body and
    /// return the key without it plus the parsed generation. Two layouts
    /// the pageserver emits today:
    ///
    /// - **Trailing** (`<name>-<gen>`): `index_part.json-00000005`,
    ///   `<layer-name>-v1-00000005`. Suffix sits at the very end.
    /// - **Pre-extension** (`<name>-<gen>.<ext>`): `tenant-manifest-00000001.json`.
    ///   Suffix sits just before the last `.<ext>`.
    ///
    /// Genless files (heatmap, initdb archives, key bodies without any
    /// `-<8hex>` segment in either position) pass through unchanged with
    /// `Generation = None`.
    ///
    /// The check is conservative: we require an explicit `-` preceding
    /// the 8-hex token. False positives on file names that happen to
    /// contain `-<8hex>` somewhere else aren't possible by construction
    /// (layer names use `__` as their internal separator, not `-` before
    /// 8-hex tokens).
    fn split_key_generation(key_body: &str) -> (String, Option<i32>) {
        // Helper: try to parse the 8-char window ending at `end_exclusive`
        // as `-<8hex>`. Returns the parsed generation if it does.
        fn check(buf: &[u8], end_exclusive: usize) -> Option<i32> {
            if end_exclusive < 9 || buf[end_exclusive - 9] != b'-' {
                return None;
            }
            let suffix = std::str::from_utf8(&buf[end_exclusive - 8..end_exclusive]).ok()?;
            if !suffix.chars().all(|c| c.is_ascii_hexdigit()) {
                return None;
            }
            i32::from_str_radix(suffix, 16).ok()
        }

        let bytes = key_body.as_bytes();
        // Pattern 1: trailing `-<8hex>` (index_part.json, layer files).
        if let Some(g) = check(bytes, bytes.len()) {
            return (key_body[..bytes.len() - 9].to_string(), Some(g));
        }
        // Pattern 2: `-<8hex>` immediately before the final extension dot
        // (tenant-manifest-<gen>.json). Use `rfind` to pick the LAST dot
        // so multi-dot names still work.
        if let Some(dot_idx) = key_body.rfind('.') {
            if let Some(g) = check(bytes, dot_idx) {
                let mut body = String::with_capacity(key_body.len() - 9);
                body.push_str(&key_body[..dot_idx - 9]);
                body.push_str(&key_body[dot_idx..]);
                return (body, Some(g));
            }
        }
        (key_body.to_string(), None)
    }

    /// Inverse of [`split_key_generation`]: re-insert the generation
    /// marker into `key_body` so a stored `(key, generation)` pair can
    /// be surfaced back to the caller in the original path format.
    ///
    /// Two emit shapes the pageserver uses today:
    /// - `tenant-manifest` is the one path-builder that puts the
    ///   generation BEFORE the extension (`tenant-manifest-<gen>.json`).
    /// - Everything else (`index_part.json`, layer files) puts it at
    ///   the very end of the key.
    ///
    /// Split-rejoin can't perfectly invert from the body alone — both
    /// `index_part.json-<gen>` and `tenant-manifest-<gen>.json` produce
    /// `<x>.json` bodies — so we special-case the tenant-manifest name
    /// here. New file types with pre-extension generations need to be
    /// added to this list.
    fn rejoin_key_generation(key_body: &str, generation: Option<i32>) -> String {
        let Some(g) = generation else {
            return key_body.to_string();
        };
        let suffix = format!("-{:08x}", g as u32);
        if Self::uses_pre_extension_generation(key_body) {
            if let Some(dot_idx) = key_body.rfind('.') {
                let mut out = String::with_capacity(key_body.len() + suffix.len());
                out.push_str(&key_body[..dot_idx]);
                out.push_str(&suffix);
                out.push_str(&key_body[dot_idx..]);
                return out;
            }
        }
        format!("{key_body}{suffix}")
    }

    /// Whether `key_body` (already gen-stripped) corresponds to a file
    /// shape that emits its generation BEFORE the extension. Today only
    /// the tenant-manifest fits.
    fn uses_pre_extension_generation(key_body: &str) -> bool {
        // The key body can be just `tenant-manifest.json` (when the
        // caller's prefix already had `tenants/<T>/` stripped) or include
        // a relative path. Match on the file-name portion only.
        let basename = key_body.rsplit('/').next().unwrap_or(key_body);
        basename == "tenant-manifest.json"
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
        // sharded TenantShardId: `<32hex>-<4hex>` = 37 chars, one dash.
        // Emitted by `TenantShardId::Display` (libs/utils/src/shard.rs)
        // for any tenant with `shard_count != 0`. The previous form
        // accepted here (32+1+4+1+4 = 42 chars, two dashes) doesn't
        // exist in the codebase — leaving sharded tenants
        // unrecognised by `extract_neon_ids`, which then mistook the
        // following timeline_id for the tenant_id and produced a
        // tenant-wide LIKE scan in `YbStore::list`.
        if s.len() == 32 + 1 + 4 && s.as_bytes()[32] == b'-' {
            let core = &s[..32];
            let slug = &s[33..];
            return core.chars().all(|c| c.is_ascii_hexdigit())
                && slug.chars().all(|c| c.is_ascii_hexdigit());
        }
        false
    }

    async fn with_timeout<F, T, E>(&self, fut: F, cancel: &CancellationToken) -> anyhow::Result<T>
    where
        F: std::future::Future<Output = Result<T, E>>,
        E: Into<anyhow::Error>,
    {
        tokio::select! {
            res = fut => res.map_err(Into::into),
            _ = tokio::time::sleep(self.timeout) => Err(TimeoutOrCancel::Timeout.into()),
            _ = cancel.cancelled() => Err(TimeoutOrCancel::Cancel.into()),
        }
    }

    async fn with_timeout_download<F, T, E>(
        &self,
        fut: F,
        cancel: &CancellationToken,
    ) -> Result<T, DownloadError>
    where
        F: std::future::Future<Output = Result<T, E>>,
        E: Into<anyhow::Error>,
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
        let (namespace, tenant_id, timeline_id, raw_key_body_prefix) =
            self.parse_key(&full_prefix);
        let (mut key_body_prefix, requested_gen) =
            Self::split_key_generation(&raw_key_body_prefix);

        // Pageserver's `remote_tenant_manifest_prefix` returns
        // `…/tenant-manifest` (no extension) because S3/Azure LIST is
        // literal-prefix and the stored keys are
        // `tenant-manifest-<gen>.json`. For YB the storage-layer split
        // gives us `tenant-manifest.json` as the canonical body, so
        // rewrite the prefix here to match — turns the LIKE branch
        // below into an equality lookup on a known filename.
        if key_body_prefix
            .rsplit('/')
            .next()
            .map(|seg| seg == "tenant-manifest")
            .unwrap_or(false)
        {
            key_body_prefix.push_str(".json");
        }

        // Decide equality vs LIKE based on whether the prefix looks like
        // a full filename (has a `.<ext>` in its terminal segment) or a
        // directory/partial prefix. PK is
        // `((tenant_id, timeline_id, key) HASH, namespace, chunk_idx)`,
        // so equality on `key` resolves to a single tablet; LIKE forces
        // a full-cluster scan. Filename-shaped callers
        // (`index_part.json`, `tenant-manifest.json` after the prefix
        // rewrite above) take the fast path; directory-shaped callers
        // (`tenants/X/timelines/`, anything ending in `/`) fall back
        // to LIKE.
        let key_body_is_filename = !key_body_prefix.is_empty()
            && key_body_prefix
                .rsplit('/')
                .next()
                .is_some_and(|seg| seg.contains('.'));

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
        } else if !key_body_prefix.is_empty() && !key_body_prefix.starts_with("timelines/") {
            // Tenant-scoped objects (`tenant-manifest`, `heatmap`) are
            // stored under `(tenant_id, timeline_id='')`. Explicit
            // `timeline_id = ''` keeps the planner on those rows only
            // (matters for the LIKE branch — equality already lands on
            // one tablet by hash).
            where_clauses.push(format!("timeline_id = ${}", bind_params.len() + 1));
            bind_params.push(Box::new(String::new()));
        }
        if key_body_is_filename {
            // Hash-tablet localised lookup. If the caller named a
            // specific generation, match it; otherwise return whatever
            // generation is stored for that key.
            where_clauses.push(format!("key = ${}", bind_params.len() + 1));
            bind_params.push(Box::new(key_body_prefix.clone()));
            if requested_gen.is_some() {
                where_clauses.push(format!(
                    "generation IS NOT DISTINCT FROM ${}",
                    bind_params.len() + 1
                ));
                bind_params.push(Box::new(requested_gen));
            }
        } else {
            let like_pattern = format!("{}%", escape_like(&key_body_prefix));
            where_clauses.push(format!("key LIKE ${} ESCAPE '\\'", bind_params.len() + 1));
            bind_params.push(Box::new(like_pattern));
        }
        where_clauses.push("chunk_idx = 0".to_string());

        let mut sql = format!(
            "SELECT tenant_id, timeline_id, key, generation, size_bytes, last_modified_us \
             FROM {} WHERE {} \
             ORDER BY tenant_id, timeline_id, key, generation",
            quote_ident(&self.table_name),
            where_clauses.join(" AND "),
        );
        if let Some(mk) = max_keys {
            let limit_idx = bind_params.len() + 1;
            sql += &format!(" LIMIT ${limit_idx}");
            bind_params.push(Box::new(mk.get() as i64));
        }

        let rows = {
            let client = self.client.current().await.map_err(DownloadError::Other)?;
            let fut = client.query_raw(&sql, bind_params);
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
                    let row_generation: Option<i32> = row.get("generation");
                    let size_bytes: i64 = row.get("size_bytes");
                    let last_modified_us: i64 = row.get("last_modified_us");
                    // Re-attach the generation marker so callers see the
                    // full original path (`index_part.json-00000005`,
                    // `tenant-manifest-00000005.json`, etc.).
                    let key_with_gen =
                        Self::rejoin_key_generation(&row_key_body, row_generation);
                    let full_key = self.reconstruct_key(namespace, &row_tenant, &row_timeline, &key_with_gen);
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
                    let row_generation: Option<i32> = row.get("generation");
                    let size_bytes: i64 = row.get("size_bytes");
                    let last_modified_us: i64 = row.get("last_modified_us");

                    let key_with_gen =
                        Self::rejoin_key_generation(&row_key_body, row_generation);
                    let full_key = self.reconstruct_key(namespace, &row_tenant, &row_timeline, &key_with_gen);
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
        let (namespace, tenant_id, timeline_id, raw_key_body) = self.parse_key(&db_key);
        let (key_body, requested_gen) = Self::split_key_generation(&raw_key_body);
        // chunk_idx=0 always exists for any object (including zero-byte ones),
        // and carries the denormalized size/etag/timestamp — one-row lookup.
        // Generation is now a separate column, so we match it explicitly
        // (`NULL` for genless files, exact value otherwise) to preserve
        // the caller's gen-specific semantics.
        let sql = format!(
            "SELECT size_bytes, last_modified_us FROM {} \
             WHERE tenant_id = $1 AND timeline_id = $2 \
               AND namespace = $3::text::blob_namespace AND key = $4 \
               AND generation IS NOT DISTINCT FROM $5 AND chunk_idx = 0",
            quote_ident(&self.table_name)
        );
        let params: [&(dyn ToSql + Sync); 5] =
            [&tenant_id, &timeline_id, &namespace, &key_body, &requested_gen];
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
        let (namespace, tenant_id, timeline_id, raw_key_body) = self.parse_key(&db_key);
        // Strip the `-<8hex>` generation suffix from the key body — it's
        // stored as a dedicated column on the row, not as part of the
        // key. Genless files (heatmap, initdb) keep their full key and
        // get `generation = NULL`.
        let (key_body, generation) = Self::split_key_generation(&raw_key_body);
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
             (tenant_id, timeline_id, namespace, key, generation, chunk_idx, data, \
              size_bytes, etag, last_modified_us, metadata_json) \
             VALUES ($1, $2, $3::text::blob_namespace, $4, $5, $6, $7, $8, $9, $10, $11)"
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

        let client = self.client.current().await?;
        let pipeline_fut = async {
            let futures = row_chunks.into_iter().map(|(idx, chunk)| {
                let params: Vec<Box<dyn ToSql + Sync + Send>> = vec![
                    Box::new(tenant_id.clone()),
                    Box::new(timeline_id.clone()),
                    Box::new(namespace),
                    Box::new(key_body.clone()),
                    Box::new(generation),
                    Box::new(idx),
                    Box::new(chunk),
                    Box::new(size),
                    Box::new(etag.clone()),
                    Box::new(now_us),
                    Box::new(metadata_json.clone()),
                ];
                client.execute_raw(&insert_sql, params)
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

        let (namespace, tenant_id, timeline_id, raw_key_body) = self.parse_key(&db_key);
        let (key_body, requested_gen) = Self::split_key_generation(&raw_key_body);

        // Phase 1: cheap single-row lookup to get the denormalized metadata
        // (size_bytes / etag / last_modified_us / metadata_json) sitting on
        // chunk_idx = 0. Lets us honor If-None-Match and compute the byte
        // range without ever loading bulk data into memory.
        // `generation IS NOT DISTINCT FROM $5` makes the match strict even
        // when the requested gen is NULL (genless file).
        let head_sql = format!(
            "SELECT size_bytes, etag, last_modified_us, metadata_json \
             FROM {} WHERE tenant_id = $1 AND timeline_id = $2 \
               AND namespace = $3::text::blob_namespace AND key = $4 \
               AND generation IS NOT DISTINCT FROM $5 AND chunk_idx = 0",
            quote_ident(&self.table_name)
        );
        let head_params: [&(dyn ToSql + Sync); 5] =
            [&tenant_id, &timeline_id, &namespace, &key_body, &requested_gen];
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

        // Streaming reads can't be auto-retried (the row stream isn't
        // replayable), so we just grab the current live client; the next
        // download() call will reconnect if this one's been dropped since.
        let client = self
            .client
            .current()
            .await
            .map_err(DownloadError::Other)?;
        let table = self.table_name.clone();
        let stream_sql = format!(
            "SELECT chunk_idx, data FROM {} \
             WHERE tenant_id = $1 AND timeline_id = $2 \
               AND namespace = $3::text::blob_namespace AND key = $4 \
               AND generation IS NOT DISTINCT FROM $5 \
               AND chunk_idx >= $6 AND chunk_idx <= $7 \
             ORDER BY chunk_idx",
            quote_ident(&table)
        );
        let timeout = self.timeout;
        let stream_tenant = tenant_id.clone();
        let stream_timeline = timeline_id.clone();
        let stream_namespace = namespace;
        let stream_key = key_body.clone();
        let stream_gen = requested_gen;

        // Open the row stream first (so any query error fails the
        // `download()` call cleanly with `DownloadError::Other` rather than
        // surfacing as an io::Error mid-stream after the caller has already
        // started writing to disk).
        let params: Vec<Box<dyn ToSql + Sync + Send>> = vec![
            Box::new(stream_tenant),
            Box::new(stream_timeline),
            Box::new(stream_namespace),
            Box::new(stream_key),
            Box::new(stream_gen),
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
        let (namespace, tenant_id, timeline_id, raw_key_body) = self.parse_key(&db_key);
        let (key_body, requested_gen) = Self::split_key_generation(&raw_key_body);
        let sql = format!(
            "DELETE FROM {} \
             WHERE tenant_id = $1 AND timeline_id = $2 \
               AND namespace = $3::text::blob_namespace AND key = $4 \
               AND generation IS NOT DISTINCT FROM $5",
            quote_ident(&self.table_name)
        );
        let params: [&(dyn ToSql + Sync); 5] =
            [&tenant_id, &timeline_id, &namespace, &key_body, &requested_gen];
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
               AND namespace = $3::text::blob_namespace AND key = $4 \
               AND generation IS NOT DISTINCT FROM $5",
            quote_ident(&self.table_name)
        );

        // Each key has its own (namespace, tenant, timeline, key_body)
        // tuple, so we fan out one DELETE per object pipelined via
        // `execute_raw` + `try_join_all`. Same wire-level batching trick
        // as `upload`; each DELETE hits one hash tablet directly.
        let parsed: Vec<(&'static str, String, String, String, Option<i32>)> = paths
            .iter()
            .map(|p| {
                let (ns, tenant, timeline, raw_body) = self.parse_key(&self.full_key(p));
                let (body, generation) = Self::split_key_generation(&raw_body);
                (ns, tenant, timeline, body, generation)
            })
            .collect();

        let client = self.client.current().await?;
        let pipeline_fut = async {
            let futures = parsed
                .into_iter()
                .map(|(ns, tenant, timeline, key_body, generation)| {
                    let params: Vec<Box<dyn ToSql + Sync + Send>> = vec![
                        Box::new(tenant),
                        Box::new(timeline),
                        Box::new(ns),
                        Box::new(key_body),
                        Box::new(generation),
                    ];
                    client.execute_raw(&sql, params)
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
        let (from_ns, from_tenant, from_timeline, from_raw_body) = self.parse_key(&from_key);
        let (to_ns, to_tenant, to_timeline, to_raw_body) = self.parse_key(&to_key);
        let (from_body, from_gen) = Self::split_key_generation(&from_raw_body);
        let (to_body, to_gen) = Self::split_key_generation(&to_raw_body);
        let table = quote_ident(&self.table_name);
        // Wipe the destination first so a copy that shrinks chunk count is
        // clean, then copy all source chunk rows over. Generation matches
        // are explicit; a copy at a different gen leaves the existing
        // gen's row in place.
        let delete_dst = format!(
            "DELETE FROM {table} \
             WHERE tenant_id = $1 AND timeline_id = $2 \
               AND namespace = $3::text::blob_namespace AND key = $4 \
               AND generation IS NOT DISTINCT FROM $5"
        );
        {
            let params: [&(dyn ToSql + Sync); 5] =
                [&to_tenant, &to_timeline, &to_ns, &to_body, &to_gen];
            self.with_timeout(self.client.execute(&delete_dst, &params), cancel)
                .await
                .with_context(|| format!("YbStore copy: pre-delete `{to_key}`"))?;
        }
        // Re-stamp the destination's generation from `to_gen` rather than
        // copying the source's generation — the caller's `to` path is
        // the truth.
        let insert_sql = format!(
            "INSERT INTO {table} \
             (tenant_id, timeline_id, namespace, key, generation, chunk_idx, data, \
              size_bytes, etag, last_modified_us, metadata_json) \
             SELECT $6, $7, $8::text::blob_namespace, $9, $10, chunk_idx, data, \
                    size_bytes, etag, \
                    (extract(epoch from now()) * 1000000)::bigint, metadata_json \
             FROM {table} \
             WHERE tenant_id = $1 AND timeline_id = $2 \
               AND namespace = $3::text::blob_namespace AND key = $4 \
               AND generation IS NOT DISTINCT FROM $5"
        );
        let params: [&(dyn ToSql + Sync); 10] = [
            &from_tenant,
            &from_timeline,
            &from_ns,
            &from_body,
            &from_gen,
            &to_tenant,
            &to_timeline,
            &to_ns,
            &to_body,
            &to_gen,
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

#[cfg(test)]
mod tests {
    use super::*;

    /// Real-shape unsharded TenantShardId: bare 32-hex.
    const T_LEGACY: &str = "5b36a8400fc88e7eeec19878ef7fbe30";

    /// Real-shape sharded TenantShardId: `<32hex>-<4hex>` (37 chars,
    /// one dash). Format emitted by `TenantShardId::Display` whenever
    /// `shard_count != 0` — i.e., for every non-legacy tenant.
    const T_SHARDED: &str = "5b36a8400fc88e7eeec19878ef7fbe30-0001";

    /// Real-shape TimelineId.
    const TL: &str = "364e1d848370d9609ba474ebd8612b90";

    #[test]
    fn extract_neon_ids_legacy_tenant_with_timeline() {
        let (t, tl) = YbStore::extract_neon_ids(
            &format!("tenants/{T_LEGACY}/timelines/{TL}/index_part.json-00000005"),
        );
        assert_eq!(t, T_LEGACY, "tenant_id for legacy unsharded tenant");
        assert_eq!(tl, TL, "timeline_id alongside legacy tenant");
    }

    #[test]
    fn extract_neon_ids_legacy_tenant_only() {
        let (t, tl) =
            YbStore::extract_neon_ids(&format!("tenants/{T_LEGACY}/tenant-manifest-1.json"));
        assert_eq!(t, T_LEGACY);
        assert_eq!(tl, "");
    }

    /// Regression: sharded TSID (`<32hex>-<4hex>`) was rejected by the
    /// old `looks_like_neon_id` (which expected a *two*-dash 42-char
    /// shape that no codepath actually emits). Result: the parser
    /// skipped the TSID, landed on the timeline_id by mistake, and
    /// left timeline_id empty — producing a tenant-wide LIKE scan
    /// instead of a single-tablet hash lookup.
    #[test]
    fn extract_neon_ids_sharded_tenant_with_timeline() {
        let (t, tl) = YbStore::extract_neon_ids(
            &format!("tenants/{T_SHARDED}/timelines/{TL}/index_part.json-00000005"),
        );
        assert_eq!(t, T_SHARDED, "tenant_id for sharded tenant (37-char form)");
        assert_eq!(tl, TL, "timeline_id alongside sharded tenant");
    }

    #[test]
    fn extract_neon_ids_sharded_tenant_only() {
        let (t, tl) =
            YbStore::extract_neon_ids(&format!("tenants/{T_SHARDED}/tenant-manifest-1.json"));
        assert_eq!(t, T_SHARDED);
        assert_eq!(tl, "");
    }

    #[test]
    fn extract_neon_ids_no_ids_present() {
        let (t, tl) = YbStore::extract_neon_ids("random/path/no-ids-here");
        assert_eq!(t, "");
        assert_eq!(tl, "");
    }

    #[test]
    fn extract_neon_ids_safekeeper_wal_path() {
        // WAL-backup layout: `<tenant>/<timeline>/<segno>` — no
        // `tenants/timelines` markers, just the ids in path order.
        let (t, tl) = YbStore::extract_neon_ids(
            &format!("{T_LEGACY}/{TL}/000000010000000000000005"),
        );
        assert_eq!(t, T_LEGACY);
        assert_eq!(tl, TL);
    }

    // ---- split_key_generation / rejoin_key_generation -------------

    #[test]
    fn split_trailing_index_part() {
        let (body, g) = YbStore::split_key_generation("index_part.json-00000005");
        assert_eq!(body, "index_part.json");
        assert_eq!(g, Some(5));
    }

    #[test]
    fn split_pre_extension_tenant_manifest() {
        // The shape that triggered the user's question: gen sits before
        // the `.json` extension, not at the end of the key.
        let (body, g) = YbStore::split_key_generation("tenant-manifest-00000001.json");
        assert_eq!(body, "tenant-manifest.json");
        assert_eq!(g, Some(1));
    }

    #[test]
    fn split_layer_v1_suffix() {
        // Layer files end in `-v1-<gen>` — trailing-suffix shape.
        let layer =
            "000000000000000000000000-FFFFFFFFFFFFFFFFFFFFFFFF__0000000001620E40-0000000001621E40-v1-00000005";
        let (body, g) = YbStore::split_key_generation(layer);
        assert_eq!(g, Some(5));
        assert!(body.ends_with("-v1"));
        assert!(!body.ends_with("-00000005"));
    }

    #[test]
    fn split_heatmap_no_gen() {
        let (body, g) = YbStore::split_key_generation("heatmap-v1.json");
        assert_eq!(body, "heatmap-v1.json");
        assert_eq!(g, None);
    }

    #[test]
    fn split_initdb_no_gen() {
        let (body, g) = YbStore::split_key_generation("initdb.tar.zst");
        assert_eq!(body, "initdb.tar.zst");
        assert_eq!(g, None);
    }

    #[test]
    fn split_non_hex_tail_not_a_generation() {
        // `-zzzzzzzz` is 8 chars but non-hex — must NOT be parsed as gen.
        let (body, g) = YbStore::split_key_generation("something-zzzzzzzz");
        assert_eq!(body, "something-zzzzzzzz");
        assert_eq!(g, None);
    }

    #[test]
    fn rejoin_round_trip_trailing() {
        let (body, g) = YbStore::split_key_generation("index_part.json-00000005");
        assert_eq!(
            YbStore::rejoin_key_generation(&body, g),
            "index_part.json-00000005"
        );
    }

    #[test]
    fn rejoin_round_trip_pre_extension() {
        let (body, g) = YbStore::split_key_generation("tenant-manifest-00000001.json");
        assert_eq!(
            YbStore::rejoin_key_generation(&body, g),
            "tenant-manifest-00000001.json"
        );
    }

    #[test]
    fn rejoin_no_gen_passthrough() {
        assert_eq!(
            YbStore::rejoin_key_generation("heatmap-v1.json", None),
            "heatmap-v1.json"
        );
    }
}

