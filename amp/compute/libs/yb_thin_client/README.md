# yb_thin_client

A thin, safe Rust client to a **yb-tserver's `PgClientService.Perform`**, used
on the storage hot paths (pageserver page reads/writes, safekeeper WAL
reads/writes). The tserver does the tablet routing — exactly as it does for
pggate — so there is **no metacache, batcher, or tablet invoker** on this side.

All the YugabyteDB C++ lives behind a small C ABI in a prebuilt shim
(`libyb_thin_client.so`, a YugabyteDB build target); this crate is a thin
marshaller over that ABI. No RPC framing, session management, routing,
partition hashing, or row decoding happens in Rust — the shim does all of it
with YB's own code.

## Architecture

```
   pageserver / safekeeper (Rust)
        │  TsClient::{read, upsert, open_table, connect}
   ┌────▼───────────────────────────────┐
   │ yb_thin_client (this crate)         │  marshal args → C structs,
   │  src/client.rs  safe API            │  bridge async callbacks → oneshot,
   │  src/ffi.rs     extern "C" (unsafe) │  map cells → Value/Row
   └────┬───────────────────────────────┘
        │  shim/yb_thin_client.h  (the C ABI)
   ┌────▼───────────────────────────────┐
   │ libyb_thin_client.so  (YB C++)      │  PgClientServiceProxy, PartitionSchema,
   │                                     │  pg_doc_data decode — YB's own code
   └────┬───────────────────────────────┘
        │  PgClientService.Perform (RPC, port 9100)
     yb-tserver  ── routes each op to the right tablet
```

The design principle is **minimise RPCs**: the read primitive is a *batch of
ops that ride one `Perform`* (one RPC, one session, one snapshot). A single
read is just a one-op batch; writes are the same shape.

## Rust API (`src/client.rs`)

- `TsClient::connect(&TsClientConfig) -> Arc<TsClient>` — connect to tserver RPC
  endpoints and open a session pool (blocking; startup only).
- `client.open_table(db_oid, table_oid) -> Arc<TableHandle>` — resolve a table's
  id + schema (blocking; startup health check). `TableHandle::col()` /
  `num_hash_columns()` / `num_key_columns()` resolve columns by name.
- **`client.read(&[ReadOp], read_time_ht) -> ReadBatch`** — the read primitive:
  N read ops as the ops of one `Perform`; `batch.ops[i]` corresponds to
  `ops[i]`, `batch.used_read_time_ht` is the one snapshot. `read_page` and
  `read_scan` are RPC-free sugar over it (the latter pages a whole scan pinned
  to the first page's snapshot).
- **`client.upsert(&[UpsertRow])`** — a batch of upserts as one write `Perform`;
  rows may span tables.
- Builders: `RangeRead::new(table, hash_values).cond(...).targets(...).limit(...)`
  and `UpsertRow::new(table, key_values, &[(col, val)])` resolve column names to
  ids against the `TableHandle` (RPC-free). `BindValue` is the input value type.
- Reads come back **decoded**: the shim decodes each page (YB's `pg_doc_data`
  codec + the table's column types) into typed cells; this crate maps them onto
  a `Value` enum (`Null|Bool|I16|I32|I64|Text|Bytes`) and `Row = Vec<Value>`.
  `Value::as_i64()/as_bytes()/…` return `Result`.
- Errors: `YbError` mirrors the shim's status codes (`Invalid`, `Network`,
  `TryAgain`, `ReadRestart`, `Schema`, `Other`); `ReadRestart` means restart the
  whole batch on a fresh read point.

## Session / connection pool

`TsClientConfig` carries `read_sessions` / `write_sessions` /
`sessions_per_conn`. A single PgClientService session serialises its Performs
server-side, so concurrency comes from many sessions; the shim packs
`sessions_per_conn` sessions onto each connection and opens
`ceil((read+write)/per_conn)` connections. It is the *connections* that spread
load across tserver nodes (behind a ClusterIP VIP, each connection may land on
a different node). A field of 0 selects the shim default (read 4, write 1,
per-conn 4).

## The C ABI (`shim/yb_thin_client.h`)

Vendored verbatim from the shim release; `src/ffi.rs` mirrors it 1:1. The
surface: `ybthin_client_create`/`_destroy`, `ybthin_table_open`/`_close`/
`ybthin_columns_free`, `ybthin_read_async` (batch read → `ybthin_read_result`
of per-op decoded `ybthin_cell[]`), `ybthin_upsert_batch_async`,
`ybthin_read_result_free`, `ybthin_string_free`. Status codes:
`OK/INVALID/NETWORK/TRY_AGAIN/READ_RESTART/SCHEMA/OTHER`. Callbacks may fire on
a YB reactor thread, so the Rust side only signals a `oneshot` and returns.

## Build / packaging

The shim is built in the YugabyteDB tree as a **self-contained, full-LTO
`libyb_thin_client.so`** (only stock glibc `NEEDED`) and published as a
**public, versioned, per-arch tarball**:

```
https://s3.us-west-2.amazonaws.com/downloads.yugabyte.com/yb_thin_client/\
  yb_thin_client-<version>-<commit>-release-linux-<arch>.tar.gz
   → lib/libyb_thin_client.so   include/yb_thin_client.h   version.txt
```

The exact build is pinned so the linked shim always matches the vendored ABI:

- `shim/VERSION` — release version (checked against the tarball's `version.txt`
  by `build.rs`).
- `shim/COMMIT` — shim commit (selects the tarball).
- `shim/yb_thin_client.h` — the ABI header, vendored verbatim; `src/ffi.rs`
  mirrors it.

`build.rs` resolves the shim in two steps:

1. **`YB_THIN_CLIENT_LIB_DIR`**, when set — an explicit directory holding
   `libyb_thin_client.so`. CI (`pr-build.yml`, `on-demand-checks.yml`) and both
   storage Dockerfiles set it, the latter to the `/opt/yb_thin_client/lib` they
   stage the shim into, so image builds never fetch from `build.rs`. Set but
   missing the `.so` is a hard error, not a fall-through — you asked for *that*
   shim.
2. **Otherwise the pinned shim in the repo's gitignored `.shim-cache/`**, keyed
   by version + commit + arch. Already populated ⇒ reused untouched; a miss runs
   `scripts/fetch-yb-thin-client.sh` (plain `curl`, no credentials) exactly once.
   So a plain `cargo build` needs no setup.

Nothing else is consulted. The old chain also tried `YB_BUILD_ROOT`,
`YB_SOURCE_ROOT/build/latest/lib` and a hardcoded `$HOME/code/...` YB build dir
— which link a *developer's* shim, not the pinned one, and since a raw build tree
ships no `version.txt` the ABI guard silently downgraded to a warning. A missing
`version.txt` is now fatal. **There is no stub fallback** — a build that can't
find the `.so` fails loudly.

The linked binaries carry a `DT_NEEDED` for `libyb_thin_client.so` **and a
`DT_RUNPATH` to the pinned shim**, so they load it unaided — a bare
`cargo test -p safekeeper`, or a hand-launched `yb_amp_local`, needs no
environment setup.

A dependency's `build.rs` genuinely cannot set link args for a downstream
binary, so the rpath is not inherited: instead this crate declares
`links = "yb_thin_client"` and publishes its resolved directory as
`cargo:lib-dir=…`. Cargo forwards that to each direct dependent's build script
as `DEP_YB_THIN_CLIENT_LIB_DIR`, and `compute/pageserver/build.rs` and
`compute/safekeeper/build.rs` each emit their own
`-Wl,--enable-new-dtags -Wl,-rpath,<dir>`. Adding another crate that links the
shim means adding the same three-line `build.rs`.

`--enable-new-dtags` matters: the default `DT_RPATH` is searched *before*
`LD_LIBRARY_PATH`, which would let a developer's baked-in path shadow a
deploy image's. `DT_RUNPATH` is searched *after*, so an explicitly set
`LD_LIBRARY_PATH` always wins — which is how the deploy images (shim at
`/opt/yb_thin_client/lib`), CI, and `run.sh` keep control.

To move to a newer shim: rebuild + publish the tarball, bump `shim/COMMIT`
(and `shim/VERSION` if it changed), and re-vendor `shim/yb_thin_client.h`.

## Hot-path integration

This is the only transport for the page and WAL hot paths — there is no YSQL
alternative. The pageserver opens
`PageStore::thinclient` (`pages`/`slru_pages`/`meta`) and the safekeeper opens
`YbThinClient` (`amp_storage.wals`), and the safekeeper's multi-segment WAL fetch
rides one `read(&ops)` Perform. Cold paths
(DDL, GC, rehydration, control file) always use YSQL. See
[`../../architecture/yugabyte.md`](../../architecture/yugabyte.md),
[`pageserver.md`](../../architecture/pageserver.md), and
[`safekeeper.md`](../../architecture/safekeeper.md).

## Testing

`tests/live.rs` is an `#[ignore]`d smoke test against a running YugabyteDB + the
real shim (connect → open_table → a cross-key write batch → a 2-op read batch):

```sh
cargo test -p yb_thin_client --test live -- --ignored --nocapture
```

(No environment setup: the build finds the shim on its own, and this crate's
own test targets get the rpath directly from its `build.rs`.)
