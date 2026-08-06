# `endpoint_storage` HTTP API

Source: `compute/endpoint_storage/src/app.rs` (router + handlers),
`compute/endpoint_storage/src/lib.rs` (path extraction and sanitization),
`compute/endpoint_storage/src/main.rs` (binary, config, listen addr),
`compute/endpoint_storage/src/openapi_spec.yml` (hand-maintained OpenAPI).

`endpoint_storage` is a small axum service that stores per-endpoint blobs (LFC
prewarm state, and any other per-endpoint auxiliary data) as objects in a
YB-backed blob store. Durability comes from YugabyteDB: the service itself is
stateless, writing every object through the `remote_storage` `YbStore` backend
into the `blobs` YB table (see [`remote-storage.md`](remote-storage.md)), so an
instance can be killed and replaced without losing anything. `compute_ctl` is the
only caller of the blob routes (LFC prewarm/offload —
`compute/compute_tools/src/compute_prewarm.rs`); `yb_amp_local` polls `/metrics`
as its readiness gate after spawning an instance
(`controller/local/launch/nodes.go`).

In the dev cluster one instance runs per endpoint: `yb_amp_local` provisions an
instance alongside each endpoint, writing its config and datadir under
`endpoints/<endpoint_id>/endpoint_storage`
(`controller/local/launch/endpoint.go`, `controller/local/config/localenv.go`).

## Binary and configuration

The crate builds the `yb-endpoint-storage` binary (`Cargo.toml [[bin]]`; the lib
crate stays `endpoint_storage`). It takes a single JSON config, either as a
positional file path or inline via `--config=<inline-json>` (the k8s helm mode) —
the two are mutually exclusive and one is required (`main.rs`). The config struct
rejects unknown fields:

| Field | Required | Default | Meaning |
|---|---|---|---|
| `listen` | no | `0.0.0.0:51243` | `SocketAddr` to bind |
| `yb_storage_conn` | yes | — | `TypedRemoteStorageKind` (tagged `type: "YbStore"`) — the YB connection backing the blobs |
| `max_upload_file_limit` | no | `100 MiB` (`100*1024*1024`) | max accepted `PUT` body size |

The service builds its backend with
`GenericRemoteStorage::from_storage_kind(yb_storage_conn)`. `TypedRemoteStorageKind`
has exactly one variant, `YbStore`, carrying a `ysql_client::YbConnConfig`
connection block (`compute/libs/remote_storage/src/config.rs`); there is no
S3/Azure/GCS/LocalFs path. The blobs table name is hardcoded in `yb_store`
(`BLOBS_TABLE = "blobs"`) — the config carries connection info only.

Shutdown is graceful: a signal handler cancels a `CancellationToken` that both
stops the axum server and aborts in-flight storage retries. `SIGUSR2` dumps
suspended tokio tasks (`utils::tokio_dump`).

### Listen addresses in practice

Two different defaults exist, depending on launch path:

- **The binary itself** defaults to `0.0.0.0:51243` when `listen` is omitted from
  the config (`main.rs`).
- **`yb_amp_local`** always writes an explicit `listen`. It templates each
  instance off the default addr `127.0.0.1:9993`
  (`controller/local/config/config.go`, mirroring the Rust
  `ENDPOINT_STORAGE_DEFAULT_ADDR`) and offsets the port per endpoint, writing
  `endpoint_storage.json` as `{"listen", "yb_storage_conn"}` via
  `controller/local/cfg/endpointstorage.go`. The `yb_storage_conn` block is
  derived from the controller's own `[*.yb]` connection (the same YugabyteDB the
  pageserver and safekeeper persist into), with the `type: "YbStore"`
  discriminator injected.

The address each compute should use arrives in its spec
(`endpoint_storage_addr`, or the `yb_amp.endpoint_storage_addr` GUC fallback).

## Access

The route governs access: an object lives under `{branch_id}/{endpoint_id}/`,
and `normalize_key` rejects any key that would climb out of that prefix (see
Routes below). The service listens on the endpoint's own loopback address and
serves that endpoint's pod, so its blast radius is the trust boundary. Any
endpoint of a branch can read another's objects, which is what
readonly→readwrite promotion prewarm relies on (`/lfc/prewarm?from_endpoint=`).

Exposing this service beyond a single endpoint's pod requires authenticating
requests first.

## Routes

Defined in `app()` (`app.rs`):

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/{branch_id}/{endpoint_id}/{*path}` | Download an object; streams `application/octet-stream` |
| `PUT` | `/{branch_id}/{endpoint_id}/{*path}` | Upload an object (overwrites if present) |
| `GET` | `/metrics` | Prometheus text exposition (see [`endpoint-storage-metrics.md`](endpoint-storage-metrics.md)) |
| `GET` | `/status` | Health check; always `200 OK` |

Notes:

- The `{*path}` wildcard captures the rest of the URL as the object key. It is
  sanitized by `normalize_key` (`lib.rs`): the key is path-cleaned, a leading `/`
  is stripped, and any key resolving to `..`, `.`, or `/` is rejected with `400` —
  preventing traversal outside the `{branch_id}/{endpoint_id}` prefix.
- The storage path (the `RemotePath` handed to `YbStore`) is
  `{branch_id}/{endpoint_id}/{normalized-key}`.
- `compute_ctl` stores LFC state under the key `lfc_state`, i.e. at
  `/{branch_id}/{endpoint_id}/lfc_state`.
- `/metrics` is implemented in code but is **not** listed in `openapi_spec.yml`;
  the spec documents `/status` and the object routes only.

## Handler behavior

Both storage handlers wrap the underlying `RemoteStorage` call in
`utils::backoff::retry` with `WARN_THRESHOLD = 3` and `MAX_RETRIES = 10`, bailing
early on the graceful-shutdown cancellation token.

- **`GET` / `get`**: `storage.download(...)` and stream the body.
  `DownloadError::NotFound` becomes `404 Not Found` (logged at info — a missing
  key is the caller's condition to handle, e.g. first-ever prewarm); other errors
  become `500`.
- **`PUT` / `set`**: rejects bodies larger than `max_upload_file_limit` with
  `400`, then buffers the whole body in memory and `storage.upload(...)`s it as a
  single stream. `remote_storage` has no multipart upload, and a streamed HTTP
  body has no guaranteed length up front, so the tradeoff is an in-memory copy
  with the size cap as the guard (comment in `app.rs`). Overwrites any existing
  object. Returns `200`.

