# `RemoteStorage` and the `YbStore` backend

`RemoteStorage` is Neon's abstraction over object stores. In yb-light
it's the seam through which the pageserver writes durable bytes to
"somewhere not the local disk". The cloud backends (S3, Azure Blob, GCS)
were removed in lane E (commit `f26243aaa`) along with their SDK
dependencies and integration tests, and the dev-only LocalFs backend
was dropped subsequently — `YbStore` is now the only real backend,
alongside the `Unreliable` test wrapper. The safekeeper no longer uses
`RemoteStorage`: its `--remote-storage` flag was removed (#10) and
it now talks to YB directly via `YsqlClient` (see below).

## The trait (inherited from Neon)

`compute/libs/remote_storage/src/lib.rs:333`:

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
    Unreliable(Other),
    YbStore(Arc<YbStore>),
}
```

Construction goes through `GenericRemoteStorage::from_config(&RemoteStorageConfig)`
which deserializes a TOML and picks the variant. `RemoteStorageKind` is
now effectively a unit choice — its only non-test variant is
`YbStore(ysql_client::YbConnConfig)` (`config.rs:74-77`).

## YbStore: design (new in yb-light)

Source: `compute/libs/remote_storage/src/yb_store.rs`. Implements every `RemoteStorage`
method against a YugabyteDB table reached via `tokio_postgres`.

### Schema (operator-provisioned)

```sql
CREATE TYPE blob_namespace AS ENUM ('pageserver', 'safekeeper');

CREATE TABLE public.blobs (
    branch_id        TEXT                  NOT NULL,    -- yb-light branch id (32-hex), "" if not extractable
    namespace        public.blob_namespace NOT NULL,    -- 'pageserver' or 'safekeeper'
    key              TEXT                  NOT NULL,    -- discriminating filename, generation suffix stripped
    generation       INT,                               -- pageserver attach gen; NULL for genless files
    chunk_idx        INT                   NOT NULL,
    data             BYTEA                 NOT NULL,
    size_bytes       BIGINT                NOT NULL,
    etag             TEXT                  NOT NULL,
    last_modified_us BIGINT                NOT NULL,
    metadata_json    TEXT,
    PRIMARY KEY ((branch_id, key) HASH, namespace, chunk_idx)
);
```

Key in the HASH part distributes load across tablets at a per-key
granularity (avoiding a single hot tablet on big branches) and turns
`key = $x` lookups into one-tablet hash lookups. Generation lives in
its own column rather than appended to `key` so that:

- one row per logical file (old gens overwritten by new ones — ACID
  isolation prevents split-brain, no need to keep multiple versions),
- `WHERE key = '<full filename>'` is the dominant query shape (no LIKE),
- genless files (heatmap, initdb archives) sit naturally with
  `generation IS NULL`.

`YbStore::parse_key` splits each incoming `RemotePath` into the three
storage columns + the chunk index:

- **`namespace`**: derived from the blob's key/path (pageserver vs
  safekeeper), not from any config field — the shared connection block
  carries no prefix. Must match one of the variants of the
  `blob_namespace` ENUM. Stored
  via a `text::blob_namespace` cast in SQL (tokio_postgres can't bind
  Rust `&str` directly to a custom ENUM type, so the cast happens
  server-side).
- **`branch_id`**: first branch-id-shaped token
  in the path *after* stripping the leading path prefix (e.g.
  `pageserver-yb/`; it is part of the `RemotePath`, not a config field).
  A branch id is
  32 hex characters, optionally followed by `-<4hex>` for
  sharded ids. The extractor is heuristic but covers every key the
  pageserver and safekeeper write. Empty string when the branch is
  absent (future generic blobs may have none).
- **`key`**: the **discriminating filename only**. `parse_key` strips
  the leading path prefix *plus* the literal segment `branches`,
  *plus* the branch_id value itself.
  Then `split_key_generation` strips the trailing or pre-extension
  `-<8hex>` generation marker into the separate `generation` column —
  so a key like `index_part.json-00000005` lands as
  `key = 'index_part.json'` + `generation = 5`, and
  `tenant-manifest-00000001.json` as
  `key = 'tenant-manifest.json'` + `generation = 1`. Heatmap and initdb
  archives have no marker; they land with `generation = NULL`.
  `rejoin_key_generation` puts the marker back in the right
  position (trailing for most, pre-extension for `tenant-manifest`)
  when reconstructing paths for callers. The per-namespace layout
  convention lives in `reconstruct_key` — see below.
- **`generation`**: parsed off the path on insert; matched explicitly
  on read (`generation IS NOT DISTINCT FROM $N` so `NULL` participates
  in equality the way callers expect). Indexable but not part of
  the hash partition — different gens of the same logical file live
  on the same tablet (one row per key).
- **`chunk_idx`** + **`data`**: object split into ≤ 100 MiB chunks.

`reconstruct_key(namespace, branch_id, key_body)` rebuilds
the original `RemotePath` for `list` outputs by applying the layout:

| namespace | branch_id | layout |
|---|---|---|
| `pageserver` | non-empty | `<prefix>branches/<branch_id>/<body>` |
| `pageserver` | empty | `<prefix><body>` (generic blob) |

(`reconstruct_key` still has a dead `safekeeper` arm — `yb_store.rs:326-348`
— but no writer emits safekeeper rows anymore. The safekeeper's control
file now lives in typed columns in the `safekeeper.timeline_state` table,
not in `blobs` — commit `840e56aae`.)

Sample rows:

| namespace | branch_id | key |
|---|---|---|
| pageserver | 77ffde89…089eaa | `000000-FFF__layer-1` |
| pageserver | 77ffde89…089eaa | `index_part.json-00000001` |
| pageserver | 77ffde89…089eaa | `tenant-manifest-00000001.json` |

The PK puts `branch_id` in the hash partition so every
point-lookup (`head_object`, `download`'s metadata + streaming SELECT,
`delete`, `upload`'s pre-DELETE, `copy`'s pre-DELETE and INSERT…SELECT)
hits one tablet via hash routing. `list` constructs the LIKE pattern
in placeholder form and filters by branch_id when
extractable from the user-supplied prefix; `delete_objects` fans out
one DELETE per object, each pinned to its own tablet.

`size_bytes`, `etag`, `last_modified_us`, `metadata_json` are
**denormalized** onto every chunk row so `head_object()` is a one-row
read against `chunk_idx = 0`.

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

The shared `YbConnConfig` struct (`compute/libs/ysql_client/src/lib.rs`,
re-exported as `remote_storage::YbConnConfig`):

```rust
pub struct YbConnConfig {
    pub host_ports: HostPorts,           // CSV "host:port[,host:port,...]"; default port 5433
    pub user: String,                    // default "yugabyte"
    pub dbname: String,                  // default "yb_storage"
    pub password_env: Option<String>,    // name of env var holding the password
    pub sslmode: Option<String>,         // None / "disable" => NoTls, else rustls
    pub sslrootcert: Option<String>,     // path to PEM CA bundle (sslmode = verify-ca)
}
```

This is the single connection block shared by all four storage
components (pageserver `yb_storage_conn`, safekeeper `yb_storage_conn`,
endpoint_storage, storage_controller `--yb-config`). It carries no
`table_name` (each component hardcodes its own table) and no `prefix` or
`concurrency_limit`. Selected via the existing `RemoteStorageKind` enum
(untagged TOML), which picks `YbStore` automatically from the
`host_ports`/`user`/`dbname` keys (they don't clash with the other
variants' field sets).

### Connection handling — `YsqlClient`

`compute/libs/ysql_client/src/lib.rs` defines `YsqlClient`, the auto-reconnecting
wrapper around `tokio_postgres::Client` used by every yb-light component
that talks to YB over the Postgres wire (this crate's `YbStore`, the
safekeeper's `YbWalStorage`, the pageserver's `PageStore`). It was
extracted from `remote_storage` into its own crate in commit `f399c049e`
and is re-exported from `remote_storage::lib.rs:23` for compatibility.
yb_light_local's `ensure_external_database` does **not** go through
`YsqlClient` — it issues raw one-shot `tokio_postgres::Config::connect`
calls (`control_plane/src/storage_controller.rs:320-350`).

- Owns the `tokio_postgres::Config` plus a reconnect closure baked at
  construction time (`connect_no_tls` or `connect_rustls`).
- `YsqlClient` is **pooled**: every constructor takes a `pool_size >= 1`,
  opens that many independent connections eagerly, and each slot has its
  own mutex. `from_yb_config_pooled` is the standard entrypoint; the
  single-connection `from_yb_config` is sugar for `pool_size = 1`
  (`ysql_client/src/lib.rs:138-225`).
- Every `execute` / `query` / `query_opt` flows through `run_with_retries`
  (`ysql_client/src/lib.rs:341-470`), which classifies each
  `tokio_postgres::Error` as `ConnectionDrop` (socket died — force
  reconnect of the slot, dropping the prepared-statement cache),
  `Transient` (SQLSTATE `40001`/`40P01`/`25P03` or a known retryable
  `XX000` message), or `Fatal` (logic / schema errors, returned
  immediately). Retries use jittered backoff up to `MAX_TOTAL_ATTEMPTS`,
  with `ConnectionDrop`s separately capped at `MAX_CONNECTION_DROPS`
  (each one costs a TCP+TLS handshake). Idempotent SQL is a
  precondition.
- Prepared statements are cached per slot keyed by SQL text and
  invalidated when the slot reconnects (the `Statement` is bound to the
  dead client). Repeat calls with the same SQL skip `Parse`.
- Raw streaming access (`query_raw` / `execute_raw`) goes through
  `current_client_at(slot)` / `pick_slot` instead of the retry helper —
  these expose the underlying `Arc<tokio_postgres::Client>` for a single
  slot.
- The high-level factory `YsqlClient::from_yb_config(&YbConnConfig,
  application_name, set_fast_path_upsert)` does password_env lookup +
  sslmode/sslrootcert dispatch in one place. `YbStore::new` uses it;
  the safekeeper's `YbWalStorage::new` constructs three pooled
  `YsqlClient`s (FPU / read / system lanes) via the same factory
  (`compute/safekeeper/src/yb_wal_storage.rs:199-209`).

### TLS

- `sslmode = "disable"` (or unset) → plain TCP, `tokio_postgres::NoTls`.
- `sslmode = "require"` (or any non-`disable`) with no `sslrootcert`
  → rustls + AcceptAll verifier. Cert chain validation errors are
  logged at `info` level but ignored; suitable for YBA universes with
  per-universe self-signed CAs not in the host's trust store.
- `sslmode = "verify-ca"` with `sslrootcert = "/path/to/ca.pem"` →
  rustls verifying against the supplied PEM-encoded CA bundle.

`build_yb_rustls_connector(sslrootcert: Option<&str>)` (in
`compute/libs/ysql_client/src/lib.rs:679`, re-exported from
`remote_storage::lib.rs:23`) is the shared constructor; the
storage_controller wrapper in `control_plane` reuses it for its own
one-shot `CREATE DATABASE` / DDL connections (see
[`storage-controller.md`](storage-controller.md)). The rustls path was
introduced in commit `7a424639e`.

### Sample TOML

For the pageserver:

```toml
yb_storage_conn = {
    host_ports = "127.0.0.1:5433",
    user = "yugabyte",
    dbname = "yb_storage"
}
```

The safekeeper no longer takes `--remote-storage`. Its control file lives
in typed columns in `safekeeper.timeline_state` (commit `840e56aae`) and
its live-WAL mirror lives in `safekeeper.wals` (commit `7ffb8b4d6`). Both go
through `YsqlClient` directly — the safekeeper consumes a `YbConnConfig`
purely to address the YB cluster (the `safekeeper.wals` table is hardcoded),
but its writes do **not** flow through the `RemoteStorage` trait. See
[`safekeeper.md`](safekeeper.md).

With TLS against a YBA-managed universe:

```toml
{host_ports = 'yb-host:5433', user = 'yugabyte', password_env = 'YB_STORAGE_PASSWORD', dbname = 'yb_storage', sslmode = 'require'}
```

## Smoke test

`compute/libs/remote_storage/examples/yb_smoke.rs` is a small binary that exercises
upload → head → download → list (delimiter + no-delimiter) → copy → range
download → delete → delete_objects against a live YB instance. Run:

```
cargo run --example yb_smoke -- \
    --host-ports 127.0.0.1:5433 --user yugabyte \
    --dbname yb_storage --table blobs
```

Exits 0 on success. Useful for catching regressions before driving the
pageserver / safekeeper through it.

## Inherited behavior worth knowing

- `GenericRemoteStorage` is dispatch-by-enum, not trait object. All match
  arms must enumerate `YbStore` — adding a new variant is a `grep`-able
  refactor.
- `RemotePath` is a relative path the caller builds (rooted at a leading
  prefix segment such as `pageserver-yb/`, baked into the path — not a
  backend config field), not an absolute filesystem path. Within `YbStore`
  the path is split into the storage columns by `parse_key` rather than
  stored verbatim.
- `Cancellation tokens` propagate through every pageserver-side
  `RemoteStorage` method. The `YbStore` impl uses `tokio::select!` on
  `cancel.cancelled()` to bail out of in-flight queries.
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
