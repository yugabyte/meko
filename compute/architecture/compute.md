# Compute

The compute is a vanilla Postgres 18.2 backend (built from `compute/vendor/postgres-v18`)
with Neon's smgr / extension patches applied. The Postgres source tree itself
is unmodified; `compute_ctl` is upstream Neon. The `yb_amp` extension at
`compute/pgxn/yb_amp/` lives in this repo and has had a small number of wire-protocol
edits (see "Changes in yb-amp" below).

## Inherited from Neon

### Storage manager hooks (`NEON_SMGR`)

Postgres delegates relation file I/O to a pluggable `smgr` (storage manager).
Neon ships a custom one — loaded as part of the `yb_amp` extension
(`shared_preload_libraries=yb_amp`) — that intercepts:

- `smgrread()` for any relation page that's not in shared_buffers / LFC:
  issues a **getpage RPC** to the pageserver instead of reading a `.dat` file.
- `smgrwrite()`: the dirty page is *not* written to a local file. The WAL
  record produced by the modification is what makes it durable, via the
  walproposer → safekeeper path.
- `smgrextend()`, `smgrnblocks()`, `smgrexists()`, etc.: all redirected to the
  pageserver, which is the source of truth for relfile contents and sizes.

So the *user-table* `base/<dboid>/<relfilenode>` files that vanilla PG
materializes on disk are mostly absent on a Neon compute — the pageserver
reconstructs them on demand.

### `walproposer`

Neon's WAL push side. Replaces `pg_basebackup`/`walsender` for getting WAL out
of the compute: pushes each WAL batch to the configured safekeepers, waits for
a quorum ACK before unblocking the commit. yb-amp is hard-wired single-SK;
walproposer's multi-SK code paths in `compute/pgxn/yb_amp/walproposer.c`
(`n_safekeepers`, `MAX_SAFEKEEPERS`, etc.) are vestigial and unexercised.

### LFC (Local File Cache)

Neon-specific on-disk page cache that backs `shared_buffers`. Sits between the
compute's buffer pool and the pageserver. Knobs:

- `yb_amp.max_file_cache_size` — startup cap (`PGC_POSTMASTER`). Default `0`,
  which disables LFC entirely. Operators must set this to a non-zero value
  at postmaster start to enable the cache.
- `yb_amp.file_cache_size_limit` — live-tunable soft limit (`PGC_SIGHUP`).
  Default `0`. Capped above by `max_file_cache_size`.
- When both are `0` (the default), every buffer miss goes straight to the
  pageserver via getpage RPC.

### Compute spec / `compute_ctl`

`compute_ctl` reads `endpoints/<id>/config.json`, generates `postgresql.conf`,
starts Postgres, runs the post-start catalog migrations, and supervises it.
None of this changes between upstream Neon and yb-amp.

### Endpoint lifecycle

Endpoints are managed via `yb_amp_local endpoint create|start|stop|reconfigure`.
The durable registry of endpoints lives in the controller's `endpoints`
table (column `spec` holds the serialized `EndpointConf`), not on local disk;
`yb_amp_local` drives it through the `/control/v1/endpoint` API. The local
`.yb_amp/endpoints/<id>/` directory is only runtime working state
(`config.json`, logs, the pidfile, and `pgdata`), created on `start` and removed
on `destroy`. The endpoint's `pgdata` directory holds:

- `postgresql.conf`, `pg_hba.conf`, `pg_ident.conf` — generated from spec.
- `PG_VERSION`, `global/pg_control`, system catalog files — pulled from
  pageserver via basebackup over gRPC (default for new endpoints) or libpq
  (legacy connstrings). `compute/compute_tools/src/compute.rs` picks the path based
  on the connection string scheme.
- `pg_wal/` — WAL segments produced *here* before they're shipped to the
  safekeeper. Ephemeral by design — destroyed when the endpoint is dropped.
- `pg_xact/`, `pg_subtrans/`, `pg_multixact/` — SLRU files. Touched by PG,
  but also reconstructed by the pageserver as part of basebackup.
- User-table heap and index files — mostly *not present*. Reads go through
  `NEON_SMGR` → pageserver.

## Changes in yb-amp

The Postgres source (`compute/vendor/postgres-v18/`) and `compute/compute_tools/` are
untouched. The `yb_amp` extension at `compute/pgxn/yb_amp/` has a small number of
yb-amp-specific edits:

- `compute/pgxn/yb_amp/walproposer.c` / `walproposer.h` — v2 wire protocol stripped
  (commit `045fce42d`). Only v3 remains on the wire.
- `compute/pgxn/yb_amp/libpagestore.c` — `pageserver_response_disconnect_timeout`
  lowered 150 s → 30 s (`e296d43df`).
- Small unrelated touches: `databricks_metrics_shared` NULL-check
  (`bc2a2e8e7`), walredo seccomp filter (`7a85e8aff`).

The user-facing wire protocols (getpage RPC, libpq replication) are
otherwise identical to upstream Neon.

## Implications of yb-amp for the compute

The compute doesn't know YugabyteDB exists. The pageserver and safekeeper it
talks to happen to be YB-backed underneath; the wire protocols
(getpage RPC, libpq replication) are identical. The only operationally
visible difference vs upstream Neon is:

- Higher `flush_wal()` latency on the safekeeper side → slightly higher
  commit latency.
- Higher first-page-read latency after a pageserver cold-start (layer fetches
  go to YB instead of S3 / local FS).

Steady-state — once warm — performance is indistinguishable from a Neon
deployment using a local-FS or S3 backend.

## Where the code lives

- Postgres source: `compute/vendor/postgres-v18/` (git submodule, Neon's patched fork).
- `compute_ctl`: `compute/compute_tools/` — the per-endpoint supervisor. Its lifecycle,
  spec, and HTTP control API have their own page: [`compute_ctl.md`](compute_ctl.md).
- Neon's PG extension (`yb_amp.so`, smgr hooks, walproposer): `compute/pgxn/yb_amp/`
  (top-level in this repo, NOT in the vendored postgres tree).
- Endpoint creation in the dev wrapper: `control_plane/src/endpoint.rs`.
