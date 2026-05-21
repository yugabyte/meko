# Compute

The compute is a vanilla Postgres 17.5 backend (built from `vendor/postgres-v17`)
with Neon's smgr / extension patches applied. **This component is unchanged in
yb-neon** — we did not modify Postgres or `compute_ctl`. It's here for
completeness so a reader has the full picture.

## Inherited from Neon

### Storage manager hooks (`NEON_SMGR`)

Postgres delegates relation file I/O to a pluggable `smgr` (storage manager).
Neon ships a custom one — loaded as part of the `neon` extension
(`shared_preload_libraries=neon`) — that intercepts:

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
a quorum ACK before unblocking the commit. The yb-neon dev cluster runs one
safekeeper so "quorum" is just that single ACK.

### LFC (Local File Cache)

Neon-specific on-disk page cache that backs `shared_buffers`. Sits between the
compute's buffer pool and the pageserver. Knobs:

- `neon.file_cache_size_limit` — max size (default 1 GB)
- `neon.max_file_cache_size` — hard cap
- Setting both to `0` disables LFC entirely; every buffer miss goes straight
  to the pageserver via getpage RPC.

### Compute spec / `compute_ctl`

`compute_ctl` reads `endpoints/<id>/config.json`, generates `postgresql.conf`,
starts Postgres, runs the post-start catalog migrations, and supervises it.
None of this changes between upstream Neon and yb-neon.

### Endpoint lifecycle

Endpoints are managed via `neon_local endpoint create|start|stop|reconfigure`.
The endpoint's `pgdata` directory holds:

- `postgresql.conf`, `pg_hba.conf`, `pg_ident.conf` — generated from spec.
- `PG_VERSION`, `global/pg_control`, system catalog files — pulled from
  pageserver via a basebackup-over-libpq when the endpoint first starts.
- `pg_wal/` — WAL segments produced *here* before they're shipped to the
  safekeeper. Ephemeral by design — destroyed when the endpoint is dropped.
- `pg_xact/`, `pg_subtrans/`, `pg_multixact/` — SLRU files. Touched by PG,
  but also reconstructed by the pageserver as part of basebackup.
- User-table heap and index files — mostly *not present*. Reads go through
  `NEON_SMGR` → pageserver.

## Changes in yb-neon

**None.** This component is unchanged.

## Implications of yb-neon for the compute

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

- Postgres source: `vendor/postgres-v17/` (git submodule, Neon's patched fork).
- `compute_ctl`: `compute_tools/`.
- Neon's PG extension (`neon.so`, smgr hooks): in the vendored postgres tree.
- Endpoint creation in the dev wrapper: `control_plane/src/endpoint.rs`.
