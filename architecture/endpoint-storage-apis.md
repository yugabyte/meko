# `endpoint_storage` HTTP API

Source: `endpoint_storage/src/app.rs` (router + handlers),
`endpoint_storage/src/lib.rs` (auth, path/claim extraction),
`endpoint_storage/src/main.rs` (binary, config, listen addr),
`endpoint_storage/src/claims.rs` (JWT claim shapes),
`endpoint_storage/src/openapi_spec.yml` (hand-maintained OpenAPI).

`endpoint_storage` is a small axum service that stores per-endpoint blobs
(LFC prewarm caches, etc.) as objects in the YB-backed object store. It is
used by compute (`compute_ctl`) and the control plane to upload/download
LFC prewarm data; it deploys either standalone or baked into the compute
image for large computes (`main.rs:1-4`). In yb-light one instance runs per
endpoint — `control_plane` co-locates an instance with each endpoint and
stamps it a per-endpoint listen address (`control_plane/src/endpoint_storage.rs`).

## Binary and configuration

The binary takes a single JSON config, either as a positional file path or
via `--config=<inline-json>` (k8s helm mode) — `main.rs:21-61`. The config
struct (`main.rs:30-41`):

| Field | Required | Default | Meaning |
|---|---|---|---|
| `listen` | no | `0.0.0.0:51243` | `SocketAddr` to bind |
| `pemfile` | yes | — | path to the Ed25519 public-key PEM used to verify JWTs |
| `yb_storage_conn` | yes | — | `TypedRemoteStorageKind` (tagged `type: "YbStore"`) — the YB connection backing the blobs |
| `max_upload_file_limit` | no | `100 MiB` (`100*1024*1024`) | max accepted `PUT` body size |

The service builds its backend with
`GenericRemoteStorage::from_storage_kind(yb_storage_conn)`
(`main.rs:71-72`). `TypedRemoteStorageKind` has only one variant, `YbStore`,
carrying a `ysql_client::YbConnConfig` connection block
(`libs/remote_storage/src/config.rs:82-89`). The `blobs` table name is
hardcoded in `yb_store` — the config carries connection info only. See
[`remote-storage.md`](remote-storage.md) for the `YbStore` backend and the
`public.blobs` schema.

### Default listen addresses

Two different defaults exist, depending on launch path:

- **The binary itself** defaults to `0.0.0.0:51243` when `listen` is omitted
  from the config (`main.rs:17-19`).
- **`control_plane`** (`yb_light_local`) does not rely on that default. It
  templates each instance off `ENDPOINT_STORAGE_DEFAULT_ADDR` =
  `127.0.0.1:9993` (`control_plane/src/endpoint_storage.rs:9-10`,
  `control_plane/src/local_env.rs:262`) and offsets the port per endpoint /
  per port-base (`yb_light_local.rs:1045`, `:1091`), writing an explicit
  `listen` into each instance's `endpoint_storage.json`
  (`endpoint_storage.rs:90-94`).

## Authentication

Every object route and every prefix-delete route requires a
`Authorization: Bearer <JWT>` header. The token is an EdDSA (Ed25519) JWT
verified against the `pemfile` public key (`lib.rs:21-38`,
`VALIDATION_ALGO = EdDSA`). The decoded claims must match the route, else
the request is rejected.

Two claim shapes (`claims.rs`):

- `EndpointStorageClaims { branch_id, endpoint_id, exp }` —
  used by `compute_ctl` for object GET/PUT/DELETE.
- `DeletePrefixClaims { branch_id, endpoint_id: Option, exp }`
  — used by the control plane for prefix deletes (branch / endpoint
  scope, with the trailing endpoint level `None` for a whole-branch delete).

Authorization checks (`lib.rs:140-227`):

- For object routes, the claim's `(branch_id, endpoint_id, exp)`
  must equal the route's. **Exception**: for `GET`, the `endpoint_id` is
  taken from the *claim* rather than the path, so a read-only replica can
  read another endpoint's data during readonly→readwrite prewarming
  (`lib.rs:161-165`).
- For prefix deletes, the claim's `(branch_id, endpoint_id, exp)`
  must equal the route's, with the `Option` endpoint level matching exactly.

A failed claim/route match returns `401 Unauthorized` (`lib.rs:120-123`); a
malformed token or missing header returns `400 Bad Request`.

`/status` and `/metrics` are unauthenticated.

## Routes

Defined in `app()` (`app.rs:12-27`):

| Method | Path | Auth | Purpose |
|---|---|---|---|
| `GET` | `/{branch_id}/{endpoint_id}/{*path}` | JWT | Download an object; streams `application/octet-stream` |
| `PUT` | `/{branch_id}/{endpoint_id}/{*path}` | JWT | Upload an object (overwrites if present) |
| `DELETE` | `/{branch_id}/{endpoint_id}/{*path}` | JWT | Delete one object |
| `DELETE` | `/{branch_id}/{endpoint_id}` | JWT | Delete all objects under an endpoint prefix |
| `DELETE` | `/{branch_id}` | JWT | Delete all objects under a branch prefix |
| `GET` | `/metrics` | none | Prometheus text exposition (see [`endpoint-storage-metrics.md`](endpoint-storage-metrics.md)) |
| `GET` | `/status` | none | Health check; always `200 OK` |

Notes:

- The `{*path}` segment is a wildcard capturing the rest of the URL as the
  object key. It is sanitized by `normalize_key` (`lib.rs:40-49`): the key
  is path-cleaned, leading `/` stripped, and any key resolving to `..`,
  `.`, or `/` is rejected with `400` — preventing path traversal outside
  the `{branch_id}/{endpoint_id}` prefix.
- The object's storage path (the `RemotePath` handed to `YbStore`) is
  `{branch_id}/{endpoint_id}/{normalized-key}`
  (`lib.rs:102-115`). The prefix-delete path drops the absent trailing
  endpoint level (`lib.rs:186-197`).
- `/metrics` is implemented in code (`app.rs:28`, `:41-46`) but is **not**
  listed in `openapi_spec.yml`; the spec documents `/status` and the
  object/prefix routes only.

## Handler behavior

All four storage handlers (`app.rs:48-151`) wrap the underlying
`RemoteStorage` call in `utils::backoff::retry` with `WARN_THRESHOLD = 3`
and `MAX_RETRIES = 10` (`app.rs:38-39`), bailing on cancellation
(graceful-shutdown token).

- **`GET` / `get`** (`app.rs:48-78`): `storage.download(...)`, streams the
  body. A `DownloadError::NotFound` becomes `404 Not Found` (logged at info,
  "not an issue of _this_ service"); other errors become `500`.
- **`PUT` / `set`** (`app.rs:86-117`): rejects bodies larger than
  `max_upload_file_limit` with `400`, then buffers the body in memory and
  `storage.upload(...)`s it as a single stream. (remote_storage has no
  multipart upload, so the body is read fully into memory with the size cap
  as the guard — see the comment at `app.rs:80-85`.) Overwrites any existing
  object. Returns `200`.
- **`DELETE` (object) / `delete`** (`app.rs:119-134`): `storage.delete(...)`.
  Returns `200` (delete of a missing object is not an error).
- **`DELETE` (prefix) / `delete_prefix`** (`app.rs:136-151`):
  `storage.delete_prefix(...)`. Returns `200`.

## yb-light specifics

- The only storage backend is `YbStore`; objects are rows in the
  `public.blobs` YB table (`libs/remote_storage/src/yb_store.rs`,
  [`remote-storage.md`](remote-storage.md)). There is no S3/Azure/GCS/LocalFs
  path.
- One instance per endpoint, co-located with the endpoint, addressed via the
  control_plane-stamped per-endpoint listen address rather than a single
  shared service.
- In `control_plane`, when `endpoint_storage.remote_storage_json` is unset,
  the `yb_storage_conn` block is derived from `storage_controller.yb` (the
  same `[*.yb]` connection the pageserver/safekeeper use), with a `type:
  "YbStore"` discriminator injected (`endpoint_storage.rs:53-99`).
