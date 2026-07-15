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
//! `list_versions` and `time_travel_recover` are intentionally unsupported;
//! yb-light has no need for them and any caller hitting these paths is a bug.

use std::collections::{HashMap, HashSet};
use std::num::NonZeroU32;
use std::sync::Arc;
use std::time::{Duration, SystemTime};

use anyhow::{Context, anyhow, bail};
use bytes::Bytes;
use futures::TryStreamExt;
use futures::stream::Stream;
// YB smart-driver fork; aliased so the type names below stay
// untouched. See pageserver/src/page_store.rs for the rationale.
use tokio_util::sync::CancellationToken;
use yb_tokio_postgres as tokio_postgres;
use yb_tokio_postgres::types::ToSql;

use ysql_client::YbConnConfig;

/// The YB table the endpoint_storage blob store lives in. Hardcoded — the
/// connection config carries no table name (every yb-light component owns
/// a fixed table).
const BLOBS_TABLE: &str = "blobs";
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
    ysql_fpu_client: Arc<crate::YsqlClient>,
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
    pub async fn new(config: &YbConnConfig, timeout: Duration) -> anyhow::Result<Self> {
        // YsqlClient owns reconnect logic, runs `SET yb_enable_upsert_mode`
        // on every reopen, and dispatches sslmode + sslrootcert internally.
        // Same factory used by the safekeeper's WAL-mirror connection so
        // both honour the same TLS rules.
        // `set_fast_path_upsert = true`: the blob INSERT path (see
        // `put_chunked_with_etag` below) is plain `INSERT INTO {table}
        // VALUES ...` without `ON CONFLICT`, and re-PUTs of the same
        // blob key rely on YB's upsert mode to skip the
        // read-before-write check. Without the GUC, the second PUT
        // would PK-conflict.
        let ysql_fpu_client =
            ysql_client::YsqlClient::from_yb_config(config, "pageserver:yb_store", true)
                .await
                .with_context(|| {
                    format!(
                        "failed to connect to YugabyteDB at {} as user `{}` (sslmode={:?})",
                        config.host_ports, config.user, config.sslmode
                    )
                })?;

        Ok(Self {
            ysql_fpu_client: Arc::new(ysql_fpu_client),
            table_name: BLOBS_TABLE.to_string(),
            prefix: None,
            timeout,
            concurrency_limiter: ConcurrencyLimiter::new(
                crate::DEFAULT_REMOTE_STORAGE_CONCURRENCY_LIMIT,
            ),
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
        // caller's prefix already had `branches/<branch_id>/` stripped) or
        // include a relative path. Match on the file-name portion only.
        let basename = key_body.rsplit('/').next().unwrap_or(key_body);
        basename == "tenant-manifest.json"
    }

    /// Extract `branch_id` from a `blobs` key by scanning the path for the
    /// first Neon-id-shaped token. Returns an empty string when the path
    /// doesn't carry one (e.g. future generic blobs that follow no branch
    /// convention).
    ///
    /// A Neon id is 32 hex characters. The function is heuristic — it returns
    /// the first such token it finds, in path order — which is sufficient for
    /// every key the pageserver and safekeeper write today and benign (empty)
    /// for any future key that doesn't follow the convention.
    fn extract_neon_id(key: &str) -> String {
        for part in key.split('/') {
            if Self::looks_like_neon_id(part) {
                return part.to_string();
            }
        }
        String::new()
    }

    /// Map a full key to the three storage columns:
    ///   `(namespace, branch_id, key_body)`
    ///
    /// - `namespace`: `"pageserver"` / `"safekeeper"`. Derived from
    ///   `YbStoreConfig::prefix`.
    /// - `branch_id`: the first Neon-id-shaped token in the path (empty
    ///   when absent).
    /// - `key_body`: the path *after the configured prefix is stripped*,
    ///   with these segments **dropped**:
    ///     - the literal `branches` marker,
    ///     - the branch_id value itself.
    ///       What's left is the trailing discriminator the object would have
    ///       relative to its `(namespace, branch_id)` bucket —
    ///       e.g. `000000-FFF__layer-1`, `tenant-manifest-1.json`,
    ///       `safekeeper-control`, or `000000010000000000000005`.
    ///
    /// [`reconstruct_key`] is the inverse — it knows the per-namespace
    /// layout convention and reassembles the original path.
    fn parse_key(&self, full_key: &str) -> (&'static str, String, String) {
        let cfg_prefix = self.prefix.as_deref().unwrap_or("");
        let rest = full_key.strip_prefix(cfg_prefix).unwrap_or(full_key);
        let namespace: &'static str =
            if cfg_prefix.contains("safekeeper") || full_key.starts_with("safekeeper") {
                "safekeeper"
            } else {
                "pageserver"
            };
        let branch_id = Self::extract_neon_id(rest);

        let key_body: String = rest
            .split('/')
            .filter(|seg| *seg != "branches" && (branch_id.is_empty() || *seg != branch_id))
            .collect::<Vec<_>>()
            .join("/");

        (namespace, branch_id, key_body)
    }

    /// Inverse of [`parse_key`]: rebuild the full key from
    /// `(namespace, branch_id, key_body)` using the per-namespace layout
    /// convention.
    ///
    /// Layout reference:
    /// - `pageserver`, branch known: `<prefix>branches/<branch_id>/<body>`
    /// - `pageserver`, no branch:    `<prefix><body>` (generic blob)
    /// - `safekeeper`, body is `safekeeper-control`: `<prefix>safekeeper-control/<branch_id>`
    /// - `safekeeper`, anything else (WAL backup):   `<prefix><branch_id>/<body>`
    fn reconstruct_key(&self, namespace: &str, branch_id: &str, key_body: &str) -> String {
        let cfg_prefix = self.prefix.as_deref().unwrap_or("");
        let body = if namespace == "safekeeper" {
            if key_body == "safekeeper-control" {
                if branch_id.is_empty() {
                    "safekeeper-control".to_string()
                } else {
                    format!("safekeeper-control/{branch_id}")
                }
            } else if key_body.is_empty() {
                branch_id.to_string()
            } else {
                format!("{branch_id}/{key_body}")
            }
        } else if branch_id.is_empty() {
            // pageserver-namespace blob with no branch — generic blob, no
            // `branches/` segment to synthesize.
            key_body.to_string()
        } else if key_body.is_empty() {
            format!("branches/{branch_id}")
        } else {
            format!("branches/{branch_id}/{key_body}")
        };
        format!("{cfg_prefix}{body}")
    }

    fn looks_like_neon_id(s: &str) -> bool {
        // bare 32-hex-char branch id
        s.len() == 32 && s.chars().all(|c| c.is_ascii_hexdigit())
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
        // contains a branch id (the common case — pageserver lists by
        // `branches/<branch_id>/…`), we filter on that column directly so
        // the query hits a single hash tablet.
        let (namespace, branch_id, raw_key_body_prefix) = self.parse_key(&full_prefix);
        let (mut key_body_prefix, requested_gen) = Self::split_key_generation(&raw_key_body_prefix);

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
        // `((branch_id, key) HASH, namespace, chunk_idx)`,
        // so equality on `key` resolves to a single tablet; LIKE forces
        // a full-cluster scan. Filename-shaped callers
        // (`index_part.json`, `tenant-manifest.json` after the prefix
        // rewrite above) take the fast path; directory-shaped callers
        // (`branches/X/`, anything ending in `/`) fall back
        // to LIKE.
        let key_body_is_filename = !key_body_prefix.is_empty()
            && key_body_prefix
                .rsplit('/')
                .next()
                .is_some_and(|seg| seg.contains('.'));

        let mut where_clauses: Vec<String> =
            vec!["namespace = $1::text::blob_namespace".to_string()];
        let mut bind_params: Vec<Box<dyn ToSql + Sync + Send>> = vec![Box::new(namespace)];
        if !branch_id.is_empty() {
            where_clauses.push(format!("branch_id = ${}", bind_params.len() + 1));
            bind_params.push(Box::new(branch_id.clone()));
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
            "SELECT branch_id, key, generation, size_bytes, last_modified_us \
             FROM {} WHERE {} \
             ORDER BY branch_id, key, generation",
            quote_ident(&self.table_name),
            where_clauses.join(" AND "),
        );
        if let Some(mk) = max_keys {
            let limit_idx = bind_params.len() + 1;
            sql += &format!(" LIMIT ${limit_idx}");
            bind_params.push(Box::new(mk.get() as i64));
        }

        let rows = {
            let client = self
                .ysql_fpu_client
                .current()
                .await
                .map_err(DownloadError::Other)?;
            let fut = client.query_raw(&sql, bind_params);
            let row_stream = self.with_timeout_download(fut, cancel).await?;
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
                    let row_branch: String = row.get("branch_id");
                    let row_key_body: String = row.get("key");
                    let row_generation: Option<i32> = row.get("generation");
                    let size_bytes: i64 = row.get("size_bytes");
                    let last_modified_us: i64 = row.get("last_modified_us");
                    // Re-attach the generation marker so callers see the
                    // full original path (`index_part.json-00000005`,
                    // `tenant-manifest-00000005.json`, etc.).
                    let key_with_gen = Self::rejoin_key_generation(&row_key_body, row_generation);
                    let full_key = self.reconstruct_key(namespace, &row_branch, &key_with_gen);
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
                // If the caller's prefix does not end in `/`, treat its parent
                // directory as the "group root" so that sibling keys still
                // group correctly (the same convention S3 uses).
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
                    let row_branch: String = row.get("branch_id");
                    let row_key_body: String = row.get("key");
                    let row_generation: Option<i32> = row.get("generation");
                    let size_bytes: i64 = row.get("size_bytes");
                    let last_modified_us: i64 = row.get("last_modified_us");

                    let key_with_gen = Self::rejoin_key_generation(&row_key_body, row_generation);
                    let full_key = self.reconstruct_key(namespace, &row_branch, &key_with_gen);
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
        let (namespace, branch_id, raw_key_body) = self.parse_key(&db_key);
        let (key_body, requested_gen) = Self::split_key_generation(&raw_key_body);
        // chunk_idx=0 always exists for any object (including zero-byte ones),
        // and carries the denormalized size/etag/timestamp — one-row lookup.
        // Generation is now a separate column, so we match it explicitly
        // (`NULL` for genless files, exact value otherwise) to preserve
        // the caller's gen-specific semantics.
        let sql = format!(
            "SELECT size_bytes, last_modified_us FROM {} \
             WHERE branch_id = $1 \
               AND namespace = $2::text::blob_namespace AND key = $3 \
               AND generation IS NOT DISTINCT FROM $4 AND chunk_idx = 0",
            quote_ident(&self.table_name)
        );
        let params: [&(dyn ToSql + Sync); 4] = [&branch_id, &namespace, &key_body, &requested_gen];
        let row_opt = self
            .with_timeout_download(self.ysql_fpu_client.query_opt(&sql, &params), cancel)
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

        let chunks: Vec<Bytes> = from.try_collect().await.context("read upload stream")?;
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
            Some(StorageMetadata(m)) => {
                Some(serde_json::to_string(&m).context("serialize storage metadata")?)
            }
            None => None,
        };

        // Delete any prior rows for this key first — this handles overwrite +
        // chunk-count shrinking (the upsert GUC handles same-PK conflict but
        // wouldn't remove rows for keys that no longer exist in the new write).
        let table = quote_ident(&self.table_name);
        let (namespace, branch_id, raw_key_body) = self.parse_key(&db_key);
        // Strip the `-<8hex>` generation suffix from the key body — it's
        // stored as a dedicated column on the row, not as part of the
        // key. Genless files (heatmap, initdb) keep their full key and
        // get `generation = NULL`.
        let (key_body, generation) = Self::split_key_generation(&raw_key_body);
        let delete_sql = format!(
            "DELETE FROM {table} \
             WHERE branch_id = $1 \
               AND namespace = $2::text::blob_namespace AND key = $3"
        );
        {
            let params: [&(dyn ToSql + Sync); 3] = [&branch_id, &namespace, &key_body];
            self.with_timeout(self.ysql_fpu_client.execute(&delete_sql, &params), cancel)
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
             (branch_id, namespace, key, generation, chunk_idx, data, \
              size_bytes, etag, last_modified_us, metadata_json) \
             VALUES ($1, $2::text::blob_namespace, $3, $4, $5, $6, $7, $8, $9, $10)"
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

        let client = self.ysql_fpu_client.current().await?;
        let pipeline_fut = async {
            let futures = row_chunks.into_iter().map(|(idx, chunk)| {
                let params: Vec<Box<dyn ToSql + Sync + Send>> = vec![
                    Box::new(branch_id.clone()),
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

        let (namespace, branch_id, raw_key_body) = self.parse_key(&db_key);
        let (key_body, requested_gen) = Self::split_key_generation(&raw_key_body);

        // Phase 1: cheap single-row lookup to get the denormalized metadata
        // (size_bytes / etag / last_modified_us / metadata_json) sitting on
        // chunk_idx = 0. Lets us honor If-None-Match and compute the byte
        // range without ever loading bulk data into memory.
        // `generation IS NOT DISTINCT FROM $4` makes the match strict even
        // when the requested gen is NULL (genless file).
        let head_sql = format!(
            "SELECT size_bytes, etag, last_modified_us, metadata_json \
             FROM {} WHERE branch_id = $1 \
               AND namespace = $2::text::blob_namespace AND key = $3 \
               AND generation IS NOT DISTINCT FROM $4 AND chunk_idx = 0",
            quote_ident(&self.table_name)
        );
        let head_params: [&(dyn ToSql + Sync); 4] =
            [&branch_id, &namespace, &key_body, &requested_gen];
        let head_opt = self
            .with_timeout_download(
                self.ysql_fpu_client.query_opt(&head_sql, &head_params),
                cancel,
            )
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
            .ysql_fpu_client
            .current()
            .await
            .map_err(DownloadError::Other)?;
        let table = self.table_name.clone();
        let stream_sql = format!(
            "SELECT chunk_idx, data FROM {} \
             WHERE branch_id = $1 \
               AND namespace = $2::text::blob_namespace AND key = $3 \
               AND generation IS NOT DISTINCT FROM $4 \
               AND chunk_idx >= $5 AND chunk_idx <= $6 \
             ORDER BY chunk_idx",
            quote_ident(&table)
        );
        let timeout = self.timeout;
        let stream_branch = branch_id.clone();
        let stream_namespace = namespace;
        let stream_key = key_body.clone();
        let stream_gen = requested_gen;

        // Open the row stream first (so any query error fails the
        // `download()` call cleanly with `DownloadError::Other` rather than
        // surfacing as an io::Error mid-stream after the caller has already
        // started writing to disk).
        let params: Vec<Box<dyn ToSql + Sync + Send>> = vec![
            Box::new(stream_branch),
            Box::new(stream_namespace),
            Box::new(stream_key),
            Box::new(stream_gen),
            Box::new(start_chunk),
            Box::new(end_chunk),
        ];
        let row_stream =
            match tokio::time::timeout(timeout, client.query_raw(&stream_sql, params)).await {
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
        let (namespace, branch_id, raw_key_body) = self.parse_key(&db_key);
        let (key_body, requested_gen) = Self::split_key_generation(&raw_key_body);
        let sql = format!(
            "DELETE FROM {} \
             WHERE branch_id = $1 \
               AND namespace = $2::text::blob_namespace AND key = $3 \
               AND generation IS NOT DISTINCT FROM $4",
            quote_ident(&self.table_name)
        );
        let params: [&(dyn ToSql + Sync); 4] = [&branch_id, &namespace, &key_body, &requested_gen];
        self.with_timeout(self.ysql_fpu_client.execute(&sql, &params), cancel)
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
             WHERE branch_id = $1 \
               AND namespace = $2::text::blob_namespace AND key = $3 \
               AND generation IS NOT DISTINCT FROM $4",
            quote_ident(&self.table_name)
        );

        // Each key has its own (namespace, branch, key_body) tuple, so we
        // fan out one DELETE per object pipelined via `execute_raw` +
        // `try_join_all`. Same wire-level batching trick as `upload`; each
        // DELETE hits one hash tablet directly.
        let parsed: Vec<(&'static str, String, String, Option<i32>)> = paths
            .iter()
            .map(|p| {
                let (ns, branch, raw_body) = self.parse_key(&self.full_key(p));
                let (body, generation) = Self::split_key_generation(&raw_body);
                (ns, branch, body, generation)
            })
            .collect();

        let client = self.ysql_fpu_client.current().await?;
        let pipeline_fut = async {
            let futures = parsed
                .into_iter()
                .map(|(ns, branch, key_body, generation)| {
                    let params: Vec<Box<dyn ToSql + Sync + Send>> = vec![
                        Box::new(branch),
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
        let (from_ns, from_branch, from_raw_body) = self.parse_key(&from_key);
        let (to_ns, to_branch, to_raw_body) = self.parse_key(&to_key);
        let (from_body, from_gen) = Self::split_key_generation(&from_raw_body);
        let (to_body, to_gen) = Self::split_key_generation(&to_raw_body);
        let table = quote_ident(&self.table_name);
        // Wipe the destination first so a copy that shrinks chunk count is
        // clean, then copy all source chunk rows over. Generation matches
        // are explicit; a copy at a different gen leaves the existing
        // gen's row in place.
        let delete_dst = format!(
            "DELETE FROM {table} \
             WHERE branch_id = $1 \
               AND namespace = $2::text::blob_namespace AND key = $3 \
               AND generation IS NOT DISTINCT FROM $4"
        );
        {
            let params: [&(dyn ToSql + Sync); 4] = [&to_branch, &to_ns, &to_body, &to_gen];
            self.with_timeout(self.ysql_fpu_client.execute(&delete_dst, &params), cancel)
                .await
                .with_context(|| format!("YbStore copy: pre-delete `{to_key}`"))?;
        }
        // Re-stamp the destination's generation from `to_gen` rather than
        // copying the source's generation — the caller's `to` path is
        // the truth.
        let insert_sql = format!(
            "INSERT INTO {table} \
             (branch_id, namespace, key, generation, chunk_idx, data, \
              size_bytes, etag, last_modified_us, metadata_json) \
             SELECT $5, $6::text::blob_namespace, $7, $8, chunk_idx, data, \
                    size_bytes, etag, \
                    (extract(epoch from now()) * 1000000)::bigint, metadata_json \
             FROM {table} \
             WHERE branch_id = $1 \
               AND namespace = $2::text::blob_namespace AND key = $3 \
               AND generation IS NOT DISTINCT FROM $4"
        );
        let params: [&(dyn ToSql + Sync); 8] = [
            &from_branch,
            &from_ns,
            &from_body,
            &from_gen,
            &to_branch,
            &to_ns,
            &to_body,
            &to_gen,
        ];
        let affected = self
            .with_timeout(self.ysql_fpu_client.execute(&insert_sql, &params), cancel)
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

    /// Real-shape branch id: bare 32-hex.
    const B_LEGACY: &str = "5b36a8400fc88e7eeec19878ef7fbe30";

    #[test]
    fn extract_neon_id_pageserver_branch_path() {
        let b = YbStore::extract_neon_id(&format!("branches/{B_LEGACY}/index_part.json-00000005"));
        assert_eq!(b, B_LEGACY, "branch_id from a pageserver branch path");
    }

    #[test]
    fn extract_neon_id_safekeeper_wal_path() {
        // WAL-backup layout: `<branch_id>/<segno>` — just the branch id
        // in path order.
        let b = YbStore::extract_neon_id(&format!("{B_LEGACY}/000000010000000000000005"));
        assert_eq!(b, B_LEGACY);
    }

    #[test]
    fn extract_neon_id_no_id_present() {
        let b = YbStore::extract_neon_id("random/path/no-ids-here");
        assert_eq!(b, "");
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
        let layer = "000000000000000000000000-FFFFFFFFFFFFFFFFFFFFFFFF__0000000001620E40-0000000001621E40-v1-00000005";
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
