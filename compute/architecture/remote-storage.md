# `RemoteStorage` and the `YbStore` backend

`RemoteStorage` is the CRUD-style abstraction the pageserver (and the
`endpoint_storage` service) use to read and write durable byte-blobs to
"somewhere that is not the local disk". The trait lives in
[`compute/libs/remote_storage/src/lib.rs`](../libs/remote_storage/src/lib.rs);
its one production implementation, `YbStore`, keeps every object as one or
more rows in a YugabyteDB table reached over the Postgres wire protocol
([`yb_store.rs`](../libs/remote_storage/src/yb_store.rs)). A second
implementation, `UnreliableWrapper` (`simulate_failures.rs`), wraps any
backend to inject faults for tests.

Because YugabyteDB's YSQL layer is wire-compatible with Postgres, the same
`YbStore` code runs unchanged against a plain Postgres server for local
development.

## The `RemoteStorage` trait

`RemoteStorage: Send + Sync + 'static` (`lib.rs:333`) is a small CRUD surface
over an object store:

| Method | Purpose |
|---|---|
| `list_streaming` | List objects under a prefix, yielded as a `Stream<Item = Result<Listing, _>>`. |
| `list` | Default method: drains `list_streaming` into a single `Listing`. |
| `list_versions` | Enumerate object versions. |
| `head_object` | Fetch size / mtime of one object without its bytes. |
| `upload` | Stream `data_size_bytes` of `Bytes` into an object, with optional metadata. |
| `download` | Open a `Download` whose `download_stream` yields the object bytes, honoring an optional byte range and `If-None-Match` ETag. |
| `delete` / `delete_objects` | Delete one / many objects. |
| `max_keys_per_delete` | Hint for how many keys a single `delete_objects` call should batch. |
| `delete_prefix` | Default method: `list_streaming` + `delete_objects` over everything under a prefix. |
| `copy` | Server-side copy of one object to a new path. |
| `time_travel_recover` | Roll a prefix back to a point in time. |

Supporting types, all in `lib.rs`:

- **`RemotePath`** — a *relative* UTF-8 path (`Utf8PathBuf`) that the caller
  constructs. `RemotePath::new` rejects absolute paths. It is the object's
  identity; the backend decides how to persist it.
- **`ListingMode`** — `NoDelimiter` (return every matching key in `keys`) or
  `WithDelimiter` (group by the next `/` and return the group roots in
  `prefixes`, matching S3 `ListObjectsV2` semantics).
- **`DownloadOpts`** — `etag` (If-None-Match), `byte_start` / `byte_end`
  bounds, `version_id`, and a `DownloadKind` size hint. `byte_range()`
  normalizes the bounds into an inclusive-start / exclusive-end pair.
- **`Download`** — a `download_stream: DownloadStream` plus `last_modified`,
  `etag`, and optional `metadata`. `DownloadStream` is a pinned boxed
  `Stream<Item = std::io::Result<Bytes>>`, shaped for `tokio::io::copy_buf`.
- **`Etag`** — an opaque owned `String` newtype returned with downloads and
  heads.
- **`StorageMetadata`** — an immutable `HashMap<String, String>` of arbitrary
  key/value pairs stored alongside an object.
- **`DownloadError`** — carries the `NotFound`, `Unmodified`, `Timeout`, and
  `Cancelled` cases the backend distinguishes.

### `GenericRemoteStorage`

The rest of the codebase holds a single concrete
`GenericRemoteStorage` enum rather than a trait object (`lib.rs:531`):

```rust
pub enum GenericRemoteStorage<Other: Clone = Arc<UnreliableWrapper>> {
    Unreliable(Other),
    YbStore(Arc<YbStore>),
}
```

Every method is dispatched by matching both arms, so adding a variant is a
`grep`-able refactor. `GenericRemoteStorage::from_config(&RemoteStorageConfig)`
(`lib.rs:710`) deserializes the config and builds the concrete backend;
`bucket_name()` returns the backing table name (`"blobs"`) for `YbStore`.

## `YbStore`

`YbStore` (`yb_store.rs:79`) implements every trait method against one
YugabyteDB table. Its fields:

- `ysql_fpu_client: Arc<YsqlClient>` — an auto-reconnecting pool wrapper
  around `tokio_postgres::Client`.
- `table_name: String` — set to the hardcoded `BLOBS_TABLE = "blobs"`
  (`yb_store.rs:59`). The connection config carries no table name; the table
  is fixed per component.
- `prefix: Option<String>` — an optional key prefix, normalized to end in
  `/`. `YbStore::new` sets it to `None`, so `full_key()` uses each caller's
  `RemotePath` verbatim as the database key.
- `timeout: Duration` and a `ConcurrencyLimiter`.

`tokio_postgres` is imported as an alias of the YB smart-driver crate
(`yb_tokio_postgres as tokio_postgres`), so the type names in the file are
ordinary Postgres-client types.

### The `blobs` table

`YbStore` does not create its table. The operator provisions it — the
compute-facing schema is created by controller migration
[`0002_compute_storage.up.sql`](../../controller/migrations/0002_compute_storage.up.sql),
which the column names in `yb_store.rs` are hardcoded against:

```sql
CREATE TYPE public.blob_namespace AS ENUM ('pageserver', 'safekeeper');

CREATE TABLE public.blobs (
    branch_id        TEXT                  NOT NULL,  -- 32-hex branch id, "" when none
    namespace        public.blob_namespace NOT NULL,  -- 'pageserver' or 'safekeeper'
    key              TEXT                  NOT NULL,  -- discriminating filename, generation stripped
    generation       INT,                             -- attach generation; NULL for genless files
    chunk_idx        INT                   NOT NULL,
    data             BYTEA                 NOT NULL,
    size_bytes       BIGINT                NOT NULL,  -- total object size, duplicated per chunk
    etag             TEXT                  NOT NULL,  -- total-object MD5 etag, duplicated per chunk
    last_modified_us BIGINT                NOT NULL,  -- microseconds since Unix epoch
    metadata_json    TEXT,                            -- serialized StorageMetadata, or NULL
    PRIMARY KEY ((branch_id, key) HASH, namespace, chunk_idx)
);
```

Design points and the constraints behind them:

- **`(branch_id, key)` is the hash part of the primary key.** Hashing on the
  pair distributes rows across DocDB tablets at per-object granularity, so a
  branch with many objects does not concentrate on one hot tablet, and an
  equality predicate on both columns resolves to a single-tablet point
  lookup. Every point operation (`head_object`, `download`'s metadata read
  and its chunk-range scan, `delete`, `upload`'s pre-delete, `copy`) supplies
  `branch_id` and `key` and therefore hits exactly one tablet.
- **`generation` is its own column, not part of `key`.** Keeping the
  generation out of the hashed key means `WHERE key = '<filename>'` is the
  dominant, LIKE-free query shape; different generations of the same logical
  file share a tablet; and genless files (heatmap, initdb archives) sit
  naturally with `generation IS NULL`. Matches on read use
  `generation IS NOT DISTINCT FROM $N` so a `NULL` generation participates in
  equality the way callers expect.
- **`size_bytes`, `etag`, `last_modified_us`, and `metadata_json` are
  denormalized** — copied onto every chunk row for the same object — so a
  `head_object` (and `download`'s phase-1 metadata read) is a single-row read
  against `chunk_idx = 0`, which always exists for any object including a
  zero-byte one. `metadata_json` is `TEXT` (a serialized JSON string) so no
  extra `tokio_postgres` type features are required.
- **`namespace` is a `blob_namespace` ENUM.** `tokio_postgres` cannot bind a
  Rust `&str` directly to a custom enum type, so every statement casts the
  bound text server-side with `$n::text::blob_namespace`.

### Mapping an object path to a row

An object's `RemotePath` is decomposed into the `(namespace, branch_id,
key_body)` storage columns by `YbStore::parse_key` (`yb_store.rs:296`), and
the generation marker is peeled off `key_body` by `split_key_generation`. The
two are exactly invertible so a stored row reconstructs the caller's original
path — the round-trip-lossless property that `list` and delete-by-prefix rely
on.

**`parse_key(full_key) -> (namespace, branch_id, key_body)`:**

- `namespace` is `"safekeeper"` when the configured prefix or the key path
  contains `safekeeper`, otherwise `"pageserver"`.
- `branch_id` is the first branch-id-shaped token found in path order, via
  `extract_neon_id` / `looks_like_neon_id`: a token of **exactly 32
  hexadecimal characters**. The scan is heuristic but covers every key the
  pageserver writes; when no such token exists (a generic blob) it yields the
  empty string.
- `key_body` is the path with the configured prefix stripped and the literal
  `branches` segment and the `branch_id` value itself dropped, the remainder
  re-joined with `/`. What is left is the object's discriminator relative to
  its `(namespace, branch_id)` bucket — e.g. `index_part.json-00000005`,
  `tenant-manifest-00000001.json`, or a layer-file name.

**`split_key_generation(key_body) -> (body, Option<i32>)`** (`yb_store.rs:184`)
strips a `-<8 hex>` generation marker in either of the two shapes the
pageserver emits:

- **Trailing** — `index_part.json-00000005`, or a layer file ending in
  `-v1-00000005`: the marker sits at the very end.
- **Pre-extension** — `tenant-manifest-00000001.json`: the marker sits just
  before the final `.<ext>`.

The parse is conservative: it requires an explicit `-` immediately before an
8-character all-hex token, parsed with `i32::from_str_radix(_, 16)`. A file
whose name merely happens to end in `-<8 non-hex>` (e.g. `something-zzzzzzzz`)
is left intact with generation `None`, as are genless files.

**`rejoin_key_generation(body, gen)`** (`yb_store.rs:232`) is the inverse: it
formats the marker back as `-{:08x}` and re-inserts it at the correct
position. Because both `index_part.json-<gen>` and
`tenant-manifest-<gen>.json` collapse to a `*.json` body, the pre-extension
placement cannot be inferred from the body alone, so
`uses_pre_extension_generation` special-cases the one filename that uses it —
`tenant-manifest.json`. New pre-extension file types would need to be added
there.

**`reconstruct_key(namespace, branch_id, key_body)`** (`yb_store.rs:325`)
rebuilds the full path from the stored columns using the per-namespace layout:

| namespace | branch_id | key_body | reconstructed path |
|---|---|---|---|
| `pageserver` | non-empty | non-empty | `<prefix>branches/<branch_id>/<body>` |
| `pageserver` | non-empty | empty | `<prefix>branches/<branch_id>` |
| `pageserver` | empty | — | `<prefix><body>` (generic blob) |
| `safekeeper` | any | `safekeeper-control` | `<prefix>safekeeper-control/<branch_id>` |
| `safekeeper` | non-empty | empty | `<prefix><branch_id>` |
| `safekeeper` | non-empty | other | `<prefix><branch_id>/<body>` |

`list` runs the full round trip: it reads back `(branch_id, key, generation)`,
calls `rejoin_key_generation` to restore the marker, then `reconstruct_key`
to restore the path a caller originally uploaded.

### Chunking

`YB_CHUNK_SIZE = 100 * 1024 * 1024` (100 MiB, `yb_store.rs:77`). Each object is
split into rows of at most this many bytes, one row per `chunk_idx` in order.
The cap keeps every single `data` `BYTEA` well under YugabyteDB's default
`rpc_max_message_size` of 256 MiB — a single value larger than that limit is
rejected at the RPC layer. 100 MiB keeps the chunk count low for the objects
the pageserver actually writes: a typical layer file is one chunk, and an
upper-bound ~290 MiB layer is three. A zero-byte object is stored as a single
empty chunk at `chunk_idx = 0`.

### Uploads (`upload`)

1. Acquire a write concurrency permit, then drain the input
   `Stream<Item = io::Result<Bytes>>` into one in-memory buffer and validate
   its length against `data_size_bytes` (short read is an error; a long read
   is truncated).
2. Compute the object's MD5 ETag over the whole buffer (`compute_etag`), and
   serialize any `StorageMetadata` to JSON.
3. `parse_key` + `split_key_generation` derive the row columns.
4. **Pre-delete**: `DELETE FROM blobs WHERE branch_id = $1 AND namespace = $2
   AND key = $3` (no generation predicate). This removes every prior row for
   the object — handling both plain overwrites and the case where the new
   object has fewer chunks than the old one, and superseding any older
   generation of the same file so only one generation persists per key body.
5. **Insert**: one plain `INSERT` per chunk. The chunk rows are materialized
   up front so each future owns its parameters, then all inserts are issued
   via `client.execute_raw(..)` and driven concurrently with
   `futures::future::try_join_all`. `tokio_postgres` multiplexes the
   Bind/Execute frames over the single connection without serial round-trip
   waits, so N chunk inserts cost roughly one round trip's latency.

The inserts are plain `INSERT`s with no `ON CONFLICT`. They rely on the
session GUC `yb_enable_upsert_mode = true` — set by `YsqlClient` on every
(re)connect because `YbStore::new` requests a fast-path-upsert client — to
skip DocDB's read-before-write check; re-inserting the same primary key
overwrites rather than conflicting. The whole insert wave and the pre-delete
are each wrapped in `with_timeout`, which races the operation against the
configured timeout and the cancellation token.

### Downloads (`download`)

`download` streams object bytes without ever holding more than one chunk
resident, in two phases:

- **Phase 1 — metadata.** A single-row read of `size_bytes`, `etag`,
  `last_modified_us`, and `metadata_json` from `chunk_idx = 0` (with the
  generation matched via `IS NOT DISTINCT FROM`). Zero rows returns
  `DownloadError::NotFound`. If `DownloadOpts::etag` (If-None-Match) equals the
  stored ETag, it returns `DownloadError::Unmodified`. The requested byte range
  from `byte_range()` is clamped to the object size; an empty clamped range
  yields an empty download stream.
- **Phase 2 — chunk streaming.** Because chunks are a fixed 100 MiB (except
  possibly the last), the code computes which `chunk_idx` values overlap the
  requested `[start_byte, end_byte)` range and how much to trim off the first
  and last chunk. It opens a `tokio_postgres` `RowStream` with
  `SELECT chunk_idx, data ... WHERE chunk_idx >= $5 AND chunk_idx <= $6
  ORDER BY chunk_idx`. The row stream is opened *before* the `Download` is
  returned, so a query error fails the `download()` call cleanly rather than
  surfacing mid-stream after the caller has begun writing to disk. Each row is
  wrapped with `Bytes::from(data)` (zero-copy) and sliced to its trim bounds
  via `async_stream::stream!`, pulling bytes off the wire as the consumer
  polls.

Streaming reads are not auto-retried — a `RowStream` is not replayable — so
`download` grabs the currently-live client and lets the next call reconnect if
the connection has since dropped.

### Listing (`list` / `list_streaming`)

`list_streaming` wraps a single `list` call in `futures::stream::once`.
`list` builds its query from the parsed prefix:

- It always filters `namespace = $1::text::blob_namespace` and
  `chunk_idx = 0` (the latter dedupes the chunk rows back to one row per
  object). When a `branch_id` is extractable from the prefix it filters on
  that column too, pinning the scan to the branch's tablet.
- **Fast path vs. scan.** If the prefix's terminal segment looks like a
  filename (contains a `.`), it filters `key = $n` — a single-tablet hash
  lookup — and, when the prefix named a specific generation, matches that
  generation as well. Otherwise it filters `key LIKE $n ESCAPE '\\'` with the
  literal fragment escaped by `escape_like`, which is a wider scan. The
  pageserver's `tenant-manifest` prefix (which arrives without an extension)
  is rewritten to `tenant-manifest.json` first so it takes the equality fast
  path rather than a LIKE scan.
- `ORDER BY branch_id, key, generation`, with an optional `LIMIT` bound to
  `max_keys`.

Results are rebuilt into `RemotePath`s via `rejoin_key_generation` +
`reconstruct_key`. In `NoDelimiter` mode every matching row becomes a
`ListingObject`. In `WithDelimiter` mode the reconstructed keys are made
relative to the listing prefix (treating a non-slash-terminated prefix's
parent directory as the group root, the S3 convention), then split on the next
`/`: the leading segment becomes an entry in `prefixes`, and keys with no
further `/` become `keys`.

### `head_object`

A single-row read of `size_bytes` and `last_modified_us` from `chunk_idx = 0`,
matching `branch_id`, `namespace`, `key`, and `generation`. Missing row is
`DownloadError::NotFound`.

### `delete`, `delete_objects`, `max_keys_per_delete`

`delete` issues one `DELETE ... WHERE branch_id = $1 AND namespace = $2 AND
key = $3 AND generation IS NOT DISTINCT FROM $4`, removing all chunk rows for
that object. `delete_objects` parses each path into its own tuple and fans out
one such `DELETE` per object, pipelined via `execute_raw` + `try_join_all` (the
same wire-batching trick as `upload`), so each delete hits its own tablet.
`max_keys_per_delete` returns `YB_MAX_KEYS_PER_DELETE = 1000` — a batching hint
only; the SQL layer accepts larger arrays. An empty `paths` slice is a no-op.

### `copy`

`copy` is a fully server-side operation: it first deletes the destination
(scoped to `to`'s generation) so a copy that shrinks the chunk count stays
clean, then runs a single `INSERT ... SELECT` that reads every source chunk
row and re-stamps it with the destination's `(branch_id, namespace, key,
generation)` and a server-side `(extract(epoch from now()) * 1000000)::bigint`
timestamp. The destination generation comes from the caller's `to` path, not
the source. If the `INSERT ... SELECT` affects zero rows the source did not
exist and `copy` errors.

### Unsupported operations

- `list_versions` returns `DownloadError::Other("list_versions is not
  supported by YbStore")`.
- `time_travel_recover` returns `TimeTravelError::Unimplemented`.

These are deliberate: yb-amp keeps exactly one row per logical file (new
generations overwrite old ones; ACID isolation prevents split-brain), so there
are no historical versions to enumerate or roll back to, and any caller
reaching these paths is a bug.

### Concurrency, timeouts, cancellation

- **Concurrency.** A `ConcurrencyLimiter` holds two semaphores (read and
  write) each sized to `DEFAULT_REMOTE_STORAGE_CONCURRENCY_LIMIT = 100`. Every
  method acquires a permit for its `RequestKind` (`Get`/`List`/`Head` are
  reads; `Put`/`Delete`/`Copy` are writes) before touching the database, and
  the acquire itself races the cancellation token.
- **Timeouts.** `with_timeout` / `with_timeout_download` wrap each operation's
  future in `tokio::select!` against `tokio::time::sleep(self.timeout)` and
  `cancel.cancelled()`, mapping the losers to `TimeoutOrCancel` /
  `DownloadError::{Timeout, Cancelled}`. The timeout is
  `RemoteStorageConfig::timeout` (default 120 s).

`last_modified_us` values are converted to `SystemTime` by
`system_time_from_micros`, which handles negative (pre-epoch) values.

## Configuration

`RemoteStorageConfig` (`config.rs:10`) holds the backend `storage`, a
`timeout` (default `DEFAULT_TIMEOUT = 120s`), and a `small_timeout` (default
`DEFAULT_SMALL_TIMEOUT = 30s`). `small_timeout` is retained for compatibility
with existing config files; `YbStore` does not consume it. `timeout` is always
serialized (no conditional skip) because the pageserver rejects a config whose
re-serialized form is missing a key it expects.

`storage` is a `RemoteStorageKind`, an `#[serde(untagged)]` enum whose only
variant is `YbStore(YbConnConfig)` (`config.rs:63`). `TypedRemoteStorageKind`
is the `#[serde(tag = "type")]` sibling used by services such as
`endpoint_storage` that load their backend from a JSON file at startup.
`RemoteStorageConfig::from_toml` / `from_toml_str` parse the pageserver's TOML.

The connection block is the shared `ysql_client::YbConnConfig` (re-exported as
`remote_storage::YbConnConfig`), with `#[serde(deny_unknown_fields)]`:

```rust
pub struct YbConnConfig {
    pub host_ports: HostPorts,             // CSV "host:port[,host:port,...]"; port defaults to 5433
    pub user: String,                      // default "yugabyte"
    pub dbname: String,                    // default "yb_storage"
    pub password_env: Option<String>,      // name of env var holding the password
    pub sslmode: Option<String>,           // None / "disable" => NoTls; else rustls
    pub sslrootcert: Option<String>,       // PEM CA bundle for verify-ca / verify-full
    pub disable_transactional_writes: bool,// write-connection GUC; default false
    pub hot_path: HotPath,                 // tsclient (default) vs ysql for hot paths
    pub tserver_rpc_addrs: Option<HostPorts>,
}
```

It carries no table name (each component hardcodes its own; `YbStore` uses
`blobs`) and no prefix or concurrency limit. Because `RemoteStorageKind` is
untagged with a single variant, any TOML with `host_ports` deserializes to
`YbStore`.

### TLS

Selected by `sslmode` and dispatched inside `YsqlClient`:

- `None` or `"disable"` → plain TCP (`tokio_postgres::NoTls`).
- any other value with no `sslrootcert` → rustls with an accept-all verifier
  (certificate-chain errors are logged, not fatal); suitable for
  YBA-managed universes whose per-universe self-signed CA is not in the host
  trust store.
- a strict mode (`verify-ca` / `verify-full`) with `sslrootcert` → rustls
  verifying against the supplied PEM CA bundle.

### Connection handling (`YsqlClient`)

`YbStore::new` builds its client with
`ysql_client::YsqlClient::from_yb_config(config, "pageserver:yb_store", true)`.
`YsqlClient` is the auto-reconnecting wrapper around `tokio_postgres::Client`;
the trailing `true` requests a fast-path-upsert connection, which runs
`SET yb_enable_upsert_mode` on every (re)open — the GUC the `upload` insert
path depends on. `YbStore` reaches the underlying client through
`current()` (for raw `query_raw` / `execute_raw` streaming) and through the
`execute` / `query_opt` retry helpers for one-shot statements. SQL identifiers
built into statements are double-quote-escaped by `quote_ident` as
defense-in-depth.

### Sample TOML

For the pageserver:

```toml
yb_storage_conn = {
    host_ports = "127.0.0.1:5433",
    user = "yugabyte",
    dbname = "yb_storage"
}
```

Against a TLS-fronted YBA-managed universe:

```toml
{ host_ports = 'yb-host:5433', user = 'yugabyte', password_env = 'YB_STORAGE_PASSWORD', dbname = 'yb_storage', sslmode = 'require' }
```

## Smoke test

[`compute/libs/remote_storage/examples/yb_smoke.rs`](../libs/remote_storage/examples/yb_smoke.rs)
is a standalone binary that drives upload → head → download → list → copy →
range download → delete against a live YB (or Postgres) instance, writing to
the hardcoded `blobs` table. Run:

```
cargo run -p remote_storage --example yb_smoke -- \
    --host-ports 127.0.0.1:5433 --user yugabyte --dbname yb_storage
```

It exits 0 on success and 1 on failure — a quick way to catch regressions
before exercising the pageserver end to end.
