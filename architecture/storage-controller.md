# Storage controller

The storage controller is the **cluster control plane**. It tracks which
tenants live on which pageservers, which safekeepers hold which timelines,
generation numbers, scheduling policies, and so on. It is *not* on the
data path — neither WAL nor pages flow through it.

In yb-neon, the storage controller's relational state has been moved into
YugabyteDB. **Zero lines of storage-controller code were changed** for
this — the controller speaks Postgres wire protocol to its DB via Diesel,
and YugabyteDB is wire-compatible with Postgres. The migration was a
`createdb` + `pg_dump | psql` and a `--database-url` change.

## Inherited from Neon (unchanged)

### What it persists

11 Diesel-managed tables (schema in `storage_controller/migrations/`):

| Table | What it tracks |
|---|---|
| `tenant_shards` | Each (tenant, shard) → pageserver assignment, generation, scheduling policy, splitting state, opaque config JSON |
| `nodes` | Pageserver identities: node_id, listen addrs (PG/HTTP/HTTPS/gRPC), availability zone, lifecycle, scheduling policy |
| `safekeepers` | Safekeeper identities: id, host/port (PG + HTTP/HTTPS), AZ, scheduling policy |
| `timelines` | Per-timeline cluster-side bookkeeping: start LSN, generation, safekeeper set, ancestor info |
| `hadron_safekeepers` | (HCC variant) parallel safekeeper registry |
| `hadron_timeline_safekeepers` | (HCC variant) parallel timeline→safekeeper mapping |
| `safekeeper_timeline_pending_ops` | Pending control operations on a (safekeeper, timeline) pair — used to recover after a controller restart |
| `metadata_health` | Per-shard health markers (last successful scrub timestamps) |
| `timeline_imports` | In-progress imports (e.g., from pg_dump) |
| `controllers` | Multi-controller leader election rows (only one controller is leader at a time) |
| `__diesel_schema_migrations` | Diesel's own migration log |

### What it does

- **Tenant placement**: picks a pageserver for a new tenant; reschedules
  on node loss (heartbeat timeout).
- **Tenant attach / detach**: tells the pageserver via its HTTP API to
  attach a tenant at a specific generation.
- **Safekeeper assignment**: picks the safekeeper set for a new timeline.
- **Compute notification**: tells the compute (via `--use-local-compute-notifications`
  or via the control-plane API) when its pageserver assignment changes.
- **Heartbeats**: pings each pageserver + safekeeper on `--heartbeat-interval`
  to detect liveness.

### Wire protocol

The controller exposes an HTTP API on `--listen` for:

- Pageservers and safekeepers to "upcall" (re-attach, register, post heartbeat).
- `neon_local` / external operators to manage tenants and shards.
- The compute to look up its current pageserver address.

### Code layout

- `storage_controller/src/` — the controller binary.
- `storage_controller/migrations/` — Diesel SQL migrations.
- `control_plane/src/storage_controller.rs` — the dev wrapper (`neon_local`)
  that starts the controller and (in upstream Neon) initdb's a local PG
  for it.

## Changes in yb-neon

### 1. Use YugabyteDB instead of a local Postgres v16

Upstream Neon (and `neon_local` in particular) launches a local Postgres
v16 server alongside the controller and uses it as the controller's DB.
yb-neon replaces this with the same YB cluster that holds everything else.

**There is no Rust code change for this.** The controller's
`--database-url` flag accepts any Postgres-compatible URI; pointing it at
`postgresql://yugabyte@127.0.0.1:5433/storage_controller` is sufficient.

What we did at runtime:

1. `CREATE DATABASE storage_controller` on YB.
2. `pg_dump` the existing local-PG schema + data, `psql` it into YB.
3. Re-spawn the `storage_controller` binary with
   `--database-url postgresql://yugabyte@127.0.0.1:5433/storage_controller`.
4. Stopped the orphaned local PG v16.

Diesel migrations + every SELECT/INSERT/UPDATE work unchanged because YB's
YSQL frontend is Postgres-protocol-compatible.

### 2. Implications

- **Same migrations**: when an upstream commit adds a new Diesel migration,
  it Just Works on YB. No translation step.
- **Same Rust client code**: `diesel`, `tokio_postgres`, prepared statements
  — all unchanged.
- **YB-flavored transactions**: YB exposes `READ COMMITTED` by default on
  our cluster (`--yb_enable_read_committed_isolation=true`). The controller
  doesn't rely on `REPEATABLE READ` or `SERIALIZABLE` semantics beyond what
  YB provides, so no surprises.

## Local FS footprint in YB-backed mode

```
<workdir>/storage_controller_db/    (empty - PG v16 is no longer used)
```

The dev wrapper still creates a `storage_controller_db/` directory because
upstream `control_plane/src/storage_controller.rs` does an `initdb` there
during `neon_local start`. With our setup we run the controller binary
directly (bypassing the wrapper's PG-spawn step), so that directory stays
empty. A cleanup PR could remove the wrapper's initdb step when YB is
configured; for now it's harmless overhead.

## Run / configure

```
storage_controller \
    --dev \
    --database-url 'postgresql://yugabyte@127.0.0.1:5433/storage_controller' \
    --max-offline-interval     10s \
    --max-warming-up-interval  30s \
    --heartbeat-interval       1s \
    --address-for-peers        http://127.0.0.1:1244/ \
    --listen                   127.0.0.1:1244 \
    --use-local-compute-notifications \
    --neon-local-repo-dir      <workdir> \
    --timelines-onto-safekeepers \
    --timeline-safekeeper-count 1
```

## Performance characteristics

The controller's traffic is low: a handful of heartbeats + a few attach
RPCs per minute in steady state. Latency to YB is irrelevant; the DB sees
single-digit QPS in normal operation.
