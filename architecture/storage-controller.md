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
creates the storage_controller tables below plus the pageserver, safekeeper,
and public schemas (blobs / wals / blob_namespace, pageserver.pages /
slru_pages / meta / timeline_state, safekeeper.timeline_state)
in the same `yb_storage` DB.

| Table | What it tracks |
|---|---|
| `tenants` | Each tenant → pageserver assignment, generation, scheduling policy, splitting state, opaque config JSON |
| `nodes` | Pageserver identities: node_id, listen addrs (PG/HTTP/HTTPS/gRPC), lifecycle, scheduling policy |
| `safekeepers` | Safekeeper identities: id, region_id, version, host/port (PG + HTTP/HTTPS), AZ, scheduling policy |
| `timelines` | Per-timeline cluster-side bookkeeping: start LSN, generation, safekeeper set, deleted_at, branch_name. A non-NULL `deleted_at` marks a timeline as deleting/dropped — it's filtered out of the live branch listing (`/control/v1/timeline_branch_names`) and left for a later data GC (`timeline drop`; see #38) |
| `pending_timeline_inits` | Init recipe for a timeline whose ids + branch name are allocated (at `tenant create` / `timeline branch`) but whose PS+SK aren't provisioned yet: branch_name, pg_version, ancestor_timeline_id (NULL ⇒ Bootstrap, non-NULL ⇒ Branch), start_lsn. `endpoint create` reads it to materialize the pair, then deletes the row; driven by the `/control/v1/.../pending_init` + `/control/v1/tenant/:id/pending_timeline_inits` APIs |
| `metadata_health` | Per-tenant health markers (last successful scrub timestamps) |
| `timeline_imports` | In-progress imports (e.g., from pg_dump) |
| `endpoints` | Compute endpoints managed by `yb_light_local`: endpoint_id, tenant_id, timeline_id, and the opaque `EndpointConf` JSON (`spec`). Replaces the old per-endpoint `.yb_light/endpoints/<id>/endpoint.json` folder registry; driven by the `/control/v1/endpoint` API. |

### What it does

- **Generation authority**: owns `tenants.generation`. The launcher
  (neon_local today) bumps it through the attach-hook API on every
  pageserver spawn and bakes the value into that pageserver's
  `pageserver.toml` — see "Single-timeline process model" in
  [`yb-pageserver.md`](yb-pageserver.md). The controller no longer
  pushes attachments to pageservers.
- **Timeline observation**: on `tenant_timeline_create` it polls the
  attached pageserver's `timeline_detail` until the timeline (created
  by the pageserver itself from its `[timeline_init]` config) reports
  Active, then runs the safekeeper leg with the observed
  `last_record_lsn`.
- **Safekeeper assignment**: picks the safekeeper set for a new timeline.
- **Heartbeats**: pings each pageserver + safekeeper on `--heartbeat-interval`
  to detect liveness.

### Diverges from Neon

The pageserver-facing control path is launcher-managed (#16):

- **Pageserver reconciler deleted wholesale** (#16): the
  `Reconciler`, background reconcile loop, schedule optimiser, node
  drain/fill/delete operations, tenant migration APIs,
  multi-controller leadership (leader election, step-down,
  request forwarding) and the chaos injector are all gone — about
  8,400 lines. Pageserver attachment is decided at spawn time by the
  launcher (identity + generation in `pageserver.toml`); there is
  nothing to converge. The scheduler survives only to pick the intent
  node at tenant create; the heartbeater survives for liveness.
- **No pageserver re-attach**: the pageserver no longer calls
  `/upcall/v1/re-attach` (its `controller_upcall_client` is deleted).
  Node registration + generation bumps arrive via the launcher
  (`node_register` + attach-hook). The upcall handlers still exist
  server-side but are unused by the yb pageserver.
- **Timeline creation observes, not commands**:
  `tenant_timeline_create_pageservers` polls `timeline_detail` instead
  of POSTing a create request.

The safekeeper reconciler path is no longer upstream-parity:

- **Storage broker removed** (#14): SC no longer pushes membership
  through `storage_broker`; safekeepers learn membership over plain HTTP.
- **SK control-file in YB** (#10): per-timeline safekeeper state lives
  in `safekeeper.timeline_state` (one row per timeline) instead of an
  on-disk control file blob, so SC reconciles against DB rows rather than
  the legacy filesystem path.
- **Membership-switch HTTP surface stripped** (commit `1a9ca7ec1`): the
  multi-SK membership-change endpoints / state-machine were removed.
- **Background safekeeper reconciler removed**: the persistent
  `safekeeper_timeline_pending_ops` queue and its per-SK reconciler task
  (`safekeeper_reconciler.rs`) are gone. With one compute / one SK / one PS,
  timeline and tenant deletions are issued to the safekeeper synchronously
  (inline, with retries) and the `timelines` row is removed on success.

### Wire protocol

The controller exposes an HTTP API on `--listen` for:

- The launcher (`yb_light_local`) to register pageserver nodes
  (`/control/v1/node`) and bump generations (`/debug/v1/attach-hook`)
  before each pageserver spawn.
- `yb_light_local` / external operators to manage tenants, timelines,
  and compute endpoints (`/control/v1/endpoint`). Endpoints resolve
  their pageserver/safekeepers from per-timeline config in
  `.yb_light/config` + `pageserver.toml`, not from the controller
  (`tenant_locate` survives as a debug API only).
- Dropping a branch and all its endpoints
  (`PUT /control/v1/tenant/:tenant_id/timeline/:timeline_id/drop`, behind
  `yb_light_local timeline drop`): deletes the timeline's `endpoints` rows,
  discards any `pending_timeline_inits` recipe, and marks the timeline
  `deleted_at` so it stops surfacing as a live branch and isn't restarted.
  Idempotent; the underlying PS/SK data is left for a later GC pass.
- Legacy upcall handlers (re-attach / validate) remain but the yb
  pageserver no longer calls them.

### Code layout

- `storage_controller/src/` — the controller binary.
- `storage_controller/migrations/` — Diesel SQL migrations.
- `control_plane/src/storage_controller.rs` — the dev wrapper (`yb_light_local`)
  that starts the controller and (in upstream Neon) initdb's a local PG
  for it.

## Changes in yb-light

### 1. Use YugabyteDB instead of a local Postgres v16

Upstream Neon (and `yb_light_local` in particular) launches a local Postgres
v16 server alongside the controller and uses it as the controller's DB.
yb-light replaces this with the same YB cluster that holds everything else.

**This added a small Rust code change.** The controller gained a
`--yb-config` flag that takes the same shared `YbConnConfig` block as the
other components (the `[storage_controller.yb]` block); it builds its own
diesel/libpq connection URL internally via `YbConnConfig::to_database_url()`.
yb-light passes this block instead of a pre-formed URI. (The SC binary
still rejects any DB name other than `yb_storage` — see the validation in
`control_plane/src/storage_controller.rs` around the `DB_NAME` constant.)
The legacy `--database-url` / `DATABASE_URL` inputs remain only as a
fallback for the local-Postgres bootstrap path.

What we do at runtime:

1. `CREATE DATABASE yb_storage` on YB (if not present).
2. Spawn the `storage_controller` binary with
   `--yb-config "{ host_ports = '127.0.0.1:5433', user = 'yugabyte', dbname = 'yb_storage' }"`.
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

`yb_light_local` looks at the `[storage_controller.yb]` block in
`.yb_light/config`: when it's set (pointing at a YugabyteDB cluster —
local or remote), the wrapper **skips the entire local-Postgres
bootstrap** (`initdb`, `pg_ctl start`, the startup-script transaction)
and passes the block straight through to the controller as
`--yb-config <inline TOML>`. `yb_light_local stop` likewise skips
`pg_ctl stop`. No `storage_controller_db/` directory is created.

Before pointing the controller at the URL, yb_light_local **connects to
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
`CREATE DATABASE yb_storage`, which yb_light_local issues.

Set it via the init TOML (`yb_light_local init --config <file>`):

```toml
[storage_controller.yb]
host_ports = "127.0.0.1:5433"
user = "yugabyte"
dbname = "yb_storage"
# password_env = "YB_STORAGE_PASSWORD"
# sslmode = "require"
# against a remote YB cluster:
# host_ports = "yb-host.example.com:5433"
# TLS against a YBA-managed universe:
# host_ports = "yb-host.example.com:5433"
# sslmode = "require"
# password_env = "YB_STORAGE_PASSWORD"
```

This `[storage_controller.yb]` block is the same shared YB connection
block (`YbConnConfig`) used by the pageserver `yb_storage_conn`, the
safekeeper `yb_storage_conn`, and endpoint_storage — identical field set.

The controller itself still links against `libpq`, so the Postgres v17
libs must exist locally — but no Postgres *server* runs.

### TLS-aware admin connections

The yb_light_local wrapper's auto-provisioning connections (`create_database`
on the `yugabyte` admin DB, `ensure_yb_storage_schema` on `yb_storage`)
are routed through a small `connect_external` helper in
`control_plane/src/storage_controller.rs` that inspects the parsed
`tokio_postgres::Config::get_ssl_mode()`:

- `Disable` → `tokio_postgres::NoTls` (local-PG style).
- Anything else → the shared `remote_storage::build_yb_rustls_connector(None)`
  with the AcceptAll verifier. Same TLS posture the SC binary itself
  uses for its main YB connection.

So a `[storage_controller.yb]` block with `host_ports = "yb-host:5433"`,
`sslmode = "require"`, `password_env = "..."` brings up cleanly against a
TLS-only YB universe with no proxy. The SC binary now receives this block
as `--yb-config <inline TOML>` and builds its diesel/libpq connection URL
internally (via `YbConnConfig::to_database_url()`), rather than being
handed a `--database-url` URI; its own connection (via
`establish_connection_rustls` in `storage_controller/src/persistence.rs`)
was always TLS-capable. This fix removes the corresponding `NoTls`
hardcode in the *provisioning* path that runs once at first start.

`--database-url` / `DATABASE_URL` still exist as a fallback for the
local-Postgres bootstrap path, but the yb-light path no longer uses them.

## Run / configure

```
storage_controller \
    --dev \
    --yb-config "{ host_ports = '127.0.0.1:5433', user = 'yugabyte', dbname = 'yb_storage' }" \
    --max-offline-interval     10s \
    --max-warming-up-interval  30s \
    --heartbeat-interval       1s \
    --address-for-peers        http://127.0.0.1:1244/ \
    --listen                   127.0.0.1:1244 \
    --use-local-compute-notifications \
    --yb-light-local-repo-dir      <workdir> \
    --timelines-onto-safekeepers \
    --timeline-safekeeper-count 1
```

## Performance characteristics

The controller's traffic is low: a handful of heartbeats + a few attach
RPCs per minute in steady state. Latency to YB is irrelevant; the DB sees
single-digit QPS in normal operation.
