# `RemoteStorage` and the `YbStore` backend

`RemoteStorage` is Neon's abstraction over object stores. It's the single
seam through which the pageserver (and now the safekeeper) writes durable
bytes to "somewhere not the local disk". yb-neon adds one new backend
(`YbStore`) to the existing four.

## The trait (inherited from Neon)

`libs/remote_storage/src/lib.rs:331`:

```rust
pub trait RemoteStorage: Send + Sync + 'static {
    fn list_streaming(&self, prefix: Option<&RemotePath>, mode: ListingMode, max_keys: Option<NonZeroU32>, cancel: &CancellationToken) -> impl Stream<Item = Result<Listing, DownloadError>> + Send;
    async fn list(...) -> Result<Listing, DownloadError> { ... default-impl on top of list_streaming ... }
    async fn list_versions(...) -> Result<VersionListing, DownloadError>;
    async fn head_object(&self, key: &RemotePath, cancel: &CancellationToken) -> Result<ListingObject, DownloadError>;
    async fn upload(&self, from: impl Stream<Item = std::io::Result<Bytes>> + Send + Sync + 'static, data_size_bytes: usize, to: &RemotePath, metadata: Option<StorageMetadata>, cancel: &CancellationToken) -> anyhow::Result<()>;
    async fn download(&self, from: &RemotePath, opts: &DownloadOpts, cancel: &CancellationToken) -> Result<Download, DownloadError>;
    async fn delete(&self, path: &RemotePath, cancel: &CancellationToken) -> anyhow::Result<()>;
    async fn delete_objects(&self, paths: &[RemotePath], cancel: &CancellationToken) -> anyhow::Result<()>;
    fn max_keys_per_delete(&self) -> usize;
    async fn delete_prefix(&self, prefix: &RemotePath, cancel: &CancellationToken) -> anyhow::Result<()> { ... default-impl on top of list + delete_objects ... }
    async fn copy(&self, from: &RemotePath, to: &RemotePath, cancel: &CancellationToken) -> anyhow::Result<()>;
    async fn time_travel_recover(...) -> Result<(), TimeTravelError>;
}
```

`GenericRemoteStorage` is an enum that dispatches across all backends so the
rest of the codebase can hold a single concrete type:

```rust
pub enum GenericRemoteStorage<Other: Clone = Arc<UnreliableWrapper>> {
    LocalFs(LocalFs),
    AwsS3(Arc<S3Bucket>),
    AzureBlob(Arc<AzureBlobStorage>),
    Unreliable(Other),
    GCS(Arc<GCSBucket>),
    YbStore(Arc<YbStore>),  // <-- new in yb-neon
}
```

Construction goes through `GenericRemoteStorage::from_config(&RemoteStorageConfig)`
which deserializes a TOML and picks the variant.

## YbStore: design (new in yb-neon)

Source: `libs/remote_storage/src/yb_store.rs`. Implements every `RemoteStorage`
method against a YugabyteDB table reached via `tokio_postgres`.

### Schema (operator-provisioned)

```sql
CREATE TABLE <table_name> (
    tenant_id        TEXT    NOT NULL,    -- Neon tenant UUID, "" if not extractable
    timeline_id      TEXT    NOT NULL,    -- Neon timeline UUID, "" for tenant-scope-only blobs
    key              TEXT    NOT NULL,    -- full RemoteStorage key (with configured prefix)
    chunk_idx        INT     NOT NULL,
    data             BYTEA   NOT NULL,
    size_bytes       BIGINT  NOT NULL,    -- total object size, denormalized
    etag             TEXT    NOT NULL,    -- total-object etag, denormalized
    last_modified_us BIGINT  NOT NULL,    -- denormalized
    metadata_json    TEXT,
    PRIMARY KEY ((tenant_id, timeline_id) HASH, key, chunk_idx)
);
```

- One row per **chunk** of the object. `chunk_idx` is sequential 0, 1, 2…
- `size_bytes`, `etag`, `last_modified_us`, `metadata_json` are **denormalized**
  onto every chunk row so `head_object()` is a one-row read against
  `chunk_idx = 0`.
- `tenant_id` + `timeline_id` are **extracted at write time** by
  `YbStore::extract_neon_ids` — it scans the key's path segments for
  Neon-id-shaped tokens (32 hex chars, or 32 hex chars with a
  `-<count>-<num>` shard suffix). The first such token becomes
  `tenant_id`; the second becomes `timeline_id`. Tokens that don't
  appear in the path produce empty strings. The extractor is heuristic
  but covers every key the pageserver and safekeeper write today
  (`pageserver-yb/tenants/<id>/timelines/<id>/…`,
  `safekeeper-yb/<id>/<id>/…`, `safekeeper-yb/safekeeper-control/<id>/<id>`,
  `pageserver-yb/tenants/<id>/tenant-manifest-…`). For any future key
  that doesn't carry a tenant/timeline, both columns are empty strings
  — schema still accepts the row.
- The PK puts `(tenant_id, timeline_id)` in the hash partition so
  every point-lookup (`head_object`, `download`, `delete`, `upload`'s
  pre-DELETE, `copy`) goes to one tablet directly — no scans. Same
  shape as the `wals` table. `list` and `delete_objects` remain
  scan-style because they're prefix / multi-key by nature.

### Chunking

`YB_CHUNK_SIZE = 100 * 1024 * 1024` (100 MiB). Sized to stay well below YB's
default `rpc_max_message_size` (256 MiB). For typical pageserver layer
files (≤ 300 MiB), this gives 1–3 chunks per object.

The first version of `YbStore` tried single-row storage and was rejected by
YB with "Arena footprint exceeded warning threshold" + RPC timeout when the
pageserver tried to upload a 290 MiB layer. Chunking fixed it.

### Upload (`upload`)

1. Collect the input stream into a single in-memory buffer.
2. Compute MD5 etag.
3. `DELETE FROM <table> WHERE key = $1` — handles overwrite + shrinking
   chunk count.
4. Split into 100 MiB chunks; build a `Vec<Box<dyn ToSql + Sync + Send>>`
   per chunk so each future owns its params.
5. Issue N `client.execute_raw(insert_sql, params)` futures, drive concurrently
   via `futures::future::try_join_all`. `tokio_postgres` multiplexes the
   Bind/Execute frames over the connection — one round-trip wave instead of
   N serial waits.

The session is initialized with `SET yb_enable_upsert_mode = true`, so plain
INSERT skips DocDB's read-before-write check. Combined with the pre-DELETE,
overwrites are cheap.

### Download (`download`)

`SELECT chunk_idx, data, size_bytes, etag, last_modified_us, metadata_json
 FROM <table> WHERE key = $1 ORDER BY chunk_idx`. Concat the `data` bytea
columns into a `Vec<u8>`, slice to the requested byte range (`DownloadOpts::byte_start/byte_end`),
wrap in `Bytes` + `futures::stream::once` to satisfy the `Download` shape.
Returns metadata from the first row.

`DownloadError::NotFound` if zero rows.

### List (`list_streaming` / `list`)

`SELECT key, size_bytes, last_modified_us FROM <table>
 WHERE key LIKE $1 ESCAPE '\\' AND chunk_idx = 0 ORDER BY key`.

Filtering on `chunk_idx = 0` dedupes the chunked rows back to one row per
object. `ListingMode::WithDelimiter` groups by the next `/` after the prefix
and emits the prefixes; same logic as `LocalFs`.

### Head, copy, delete, delete_objects

Straightforward SQL. `copy` is `DELETE` of the destination + an `INSERT ...
SELECT` that copies every chunk row (using a server-side `now()` for
`last_modified_us`).

### Not supported

- `list_versions` returns `DownloadError::Other("not supported")`.
- `time_travel_recover` returns `TimeTravelError::Unimplemented`.

These match the posture of `LocalFs`.

## Config

The `YbStoreConfig` struct (`libs/remote_storage/src/config.rs`):

```rust
pub struct YbStoreConfig {
    pub host: String,
    pub port: u16,                       // default 5433
    pub user: String,
    pub password_env: Option<String>,    // optional env-var name
    pub dbname: String,
    pub table_name: String,
    pub prefix: Option<String>,          // namespacing prefix
    pub concurrency_limit: NonZeroUsize, // default 100
}
```

Selected via the existing `RemoteStorageKind` enum (untagged TOML), which
picks `YbStore` automatically when the TOML has `host`/`user`/`dbname`/
`table_name` keys (they don't clash with the other variants' field sets).

### Sample TOML

For the pageserver:

```toml
remote_storage = {
    host = "127.0.0.1",
    port = 5433,
    user = "yugabyte",
    dbname = "yb_storage",
    table_name = "blobs",
    prefix = "pageserver-yb/"
}
```

For the safekeeper's control-file + wal-backup storage (same DB, same table,
different prefix):

```toml
{host = '127.0.0.1', port = 5433, user = 'yugabyte', dbname = 'yb_storage', table_name = 'blobs', prefix = 'safekeeper-yb/'}
```

For the safekeeper's live-WAL mirror (different table — `wals`, with a
different schema; see [`safekeeper.md`](safekeeper.md)):

```toml
{host = '127.0.0.1', port = 5433, user = 'yugabyte', dbname = 'yb_storage', table_name = 'wals'}
```

## Smoke test

`libs/remote_storage/examples/yb_smoke.rs` is a small binary that exercises
upload → head → download → list (delimiter + no-delimiter) → copy → range
download → delete → delete_objects against a live YB instance. Run:

```
cargo run --example yb_smoke -- \
    --host 127.0.0.1 --port 5433 --user yugabyte \
    --dbname yb_storage --table blobs
```

Exits 0 on success. Useful for catching regressions before driving the
pageserver / safekeeper through it.

## Inherited behavior worth knowing

- `GenericRemoteStorage` is dispatch-by-enum, not trait object. All match
  arms must enumerate `YbStore` — adding a new variant is a `grep`-able
  refactor.
- `RemotePath` is rooted at the prefix configured on the backend, not at
  the system root. Within `YbStore` the actual key stored is
  `<prefix><relative-path>`.
- `Cancellation tokens` propagate through every method. The `YbStore`
  impl uses `tokio::select!` on `cancel.cancelled()` to bail out of
  in-flight queries.
- Timeouts: `RemoteStorageConfig::timeout` (default 120 s) wraps every
  per-operation future.

## Performance notes

- **Throughput**: one INSERT per 100 MiB chunk, pipelined. For a 290 MiB
  upload, that's 3 INSERTs in one round-trip wave. Wire-level bandwidth
  bounded by your link to YB.
- **Latency**: `head_object` is a single-row indexed lookup (`(key, 0)` PK
  hit). `list` with `chunk_idx = 0` filter is a hash-shard prefix scan;
  efficient on YB.
- **Storage overhead**: ~70 B/row for the denormalized metadata columns
  (`size_bytes`/`etag`/`last_modified_us`/`metadata_json`). For typical
  multi-MB objects, overhead is <0.001%.

See `../hari-neon/benchmarks/neon-vs-yb-vs-vanilla.md` for cold-cache /
warm-cache benchmark numbers vs LocalFs.
