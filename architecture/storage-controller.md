# Storage controller

The storage controller is the **cluster control plane**. It tracks which
tenants live on which pageservers, which safekeepers hold which timelines,
generation numbers, scheduling policies, and so on. It is *not* on the
data path — neither WAL nor pages flow through it.

In yb-light, the storage controller's relational state has been moved into
YugabyteDB. **Zero lines of storage-controller code were changed** for
this — the controller speaks Postgres wire protocol via Diesel and
YugabyteDB is wire-compatible with Postgres. SC tables live in the
`storage_controller` schema inside the shared `yb_storage` database
(single DB, no separate `storage_controller` DB); bootstrap is a
fresh-schema apply of the collapsed Diesel migration, not a
`pg_dump | psql`. See `control_plane/src/storage_controller.rs` (the
`DB_NAME = "yb_storage"` constant) and `storage_controller/src/persistence.rs`
(`SC_SCHEMA = "storage_controller"`, applied via `search_path=storage_controller,public`).

## What it persists

A single collapsed migration (`storage_controller/migrations/00000000000001_initial_schema/up.sql`)
creates the 8 storage_controller tables below plus the pageserver, safekeeper,
and public schemas (blobs / wals / blob_namespace, pageserver.pages /
slru_pages / meta / tenant_config / timeline_state, safekeeper.timeline_state)
in the same `yb_storage` DB.

| Table | What it tracks |
|---|---|
| `tenants` | Each tenant → pageserver assignment, generation, scheduling policy, splitting state, opaque config JSON |
| `nodes` | Pageserver identities: node_id, listen addrs (PG/HTTP/HTTPS/gRPC), lifecycle, scheduling policy |
| `safekeepers` | Safekeeper identities: id, region_id, version, host/port (PG + HTTP/HTTPS), AZ, scheduling policy |
| `timelines` | Per-timeline cluster-side bookkeeping: start LSN, generation, safekeeper set, deleted_at, branch_name |
| `safekeeper_timeline_pending_ops` | Pending control operations on a (safekeeper, timeline) pair — used to recover after a controller restart |
| `metadata_health` | Per-shard health markers (last successful scrub timestamps) |
| `timeline_imports` | In-progress imports (e.g., from pg_dump) |
| `controllers` | Multi-controller leader election rows (only one controller is leader at a time) |

The controller also writes to `pageserver.tenant_config` (in the same DB,
created by the same collapsed migration) on tenant create / config update
— task 17 moved `TenantConfig` out of pageserver-local state into YB, so
this is now the authoritative per-tenant config table the SC owns.

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

### Diverges from Neon

The safekeeper reconciler path is no longer upstream-parity:

- **Storage broker removed** (task 20): SC no longer pushes membership
  through `storage_broker`; safekeepers learn membership over plain HTTP.
- **SK control-file in YB** (task 19): per-timeline safekeeper state lives
  in `safekeeper.timeline_state` (one row per timeline) instead of an
  on-disk control file blob, so SC reconciles against DB rows rather than
  the legacy filesystem path.
- **Membership-switch HTTP surface stripped** (commit `1a9ca7ec1`): the
  multi-SK membership-change endpoints / state-machine were removed.
- **Multi-SK reconciler simplified**: with `--timeline-safekeeper-count 1`
  the default and no membership churn, the reconciler is a much-reduced
  subset of the upstream code path.

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

## Changes in yb-light

### 1. Use YugabyteDB instead of a local Postgres v16

Upstream Neon (and `neon_local` in particular) launches a local Postgres
v16 server alongside the controller and uses it as the controller's DB.
yb-light replaces this with the same YB cluster that holds everything else.

**There is no Rust code change for this.** The controller's
`--database-url` flag accepts any Postgres-compatible URI; pointing it at
`postgresql://yugabyte@127.0.0.1:5433/yb_storage` is sufficient. (The
SC binary in fact rejects any other DB name — see the validation in
`control_plane/src/storage_controller.rs` around the `DB_NAME` constant.)

What we do at runtime:

1. `CREATE DATABASE yb_storage` on YB (if not present).
2. Spawn the `storage_controller` binary with
   `--database-url postgresql://yugabyte@127.0.0.1:5433/yb_storage`.
   It then runs the collapsed Diesel migration, which creates the
   `storage_controller`, `pageserver`, `safekeeper`, and `public` schemas
   and all their tables.

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
(none)
```

`neon_local` looks at `storage_controller.database_url` in `.neon/config`:
when it's set (e.g. to a YugabyteDB URL — local or remote), the wrapper
**skips the entire local-Postgres bootstrap** (`initdb`, `pg_ctl start`,
the startup-script transaction) and passes the URL straight through to
the controller as `--database-url`. `neon_local stop` likewise skips
`pg_ctl stop`. No `storage_controller_db/` directory is created.

Before pointing the controller at the URL, neon_local **connects to
YB's `yugabyte` admin database** on the same host with the same
credentials and provisions the one DB yb-light needs:

- `CREATE DATABASE yb_storage` — the single shared DB. On the
  controller's first start, the collapsed Diesel migration
  (`storage_controller/migrations/00000000000001_initial_schema/up.sql`)
  creates everything inside it: the `storage_controller.*` tables, the
  `pageserver.*` tables (pages, slru_pages, meta, tenant_config,
  timeline_state), `safekeeper.timeline_state`, and the `public.*`
  blob-store (`blob_namespace` ENUM + `blobs` + `wals`). See
  [`yugabyte.md`](yugabyte.md) for the blob/WAL schemas.

The step is idempotent. A fresh YB cluster needs no operator DDL beyond
`CREATE DATABASE yb_storage`, which neon_local issues.

Set it via the init TOML (`neon_local init --config <file>`):

```toml
[storage_controller]
database_url = "postgresql://yugabyte@127.0.0.1:5433/yb_storage"
# or against a remote YB cluster:
# database_url = "postgresql://yugabyte@yb-host.example.com:5433/yb_storage"
# TLS against a YBA-managed universe:
# database_url = "postgresql://yugabyte:PW@yb-host.example.com:5433/yb_storage?sslmode=require"
```

The controller itself still links against `libpq`, so the Postgres v17
libs must exist locally — but no Postgres *server* runs.

### TLS-aware admin connections

The neon_local wrapper's auto-provisioning connections (`create_database`
on the `yugabyte` admin DB, `ensure_yb_storage_schema` on `yb_storage`)
are routed through a small `connect_external` helper in
`control_plane/src/storage_controller.rs` that inspects the parsed
`tokio_postgres::Config::get_ssl_mode()`:

- `Disable` → `tokio_postgres::NoTls` (local-PG style).
- Anything else → the shared `remote_storage::build_yb_rustls_connector(None)`
  with the AcceptAll verifier. Same TLS posture the SC binary itself
  uses for its main `--database-url` connection.

So a `database_url` like
`postgresql://yugabyte:PW@yb-host:5433/yb_storage?sslmode=require`
brings up cleanly against a TLS-only YB universe with no proxy. The
SC binary's own connection (via `establish_connection_rustls` in
`storage_controller/src/persistence.rs`) was always TLS-capable; this
fix removes the corresponding `NoTls` hardcode in the *provisioning*
path that runs once at first start.

## Run / configure

```
storage_controller \
    --dev \
    --database-url 'postgresql://yugabyte@127.0.0.1:5433/yb_storage' \
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
