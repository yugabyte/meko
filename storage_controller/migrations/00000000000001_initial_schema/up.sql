-- Initial schema for the entire `yb_storage` database. Covers three
-- schemas, all created here:
--
-- - `storage_controller` — the controller's own tables.
-- - `public`             — `blobs` / `wals` blob-and-WAL store,
--   shared by pageserver and safekeeper.
-- - `pageserver`         — yb-pageserver page-store tables.

CREATE SCHEMA IF NOT EXISTS storage_controller;
CREATE SCHEMA IF NOT EXISTS pageserver;
CREATE SCHEMA IF NOT EXISTS safekeeper;

-- =====================================================================
-- storage_controller.* — controller's own tables.
-- =====================================================================

-- Per-tenant placement record. yb-mode doesn't shard tenants
-- across pageservers (YB tablet-shards the keyspace itself), so
-- this is keyed only by `tenant_id`.
CREATE TABLE storage_controller.tenant_shards (
    tenant_id              VARCHAR NOT NULL,
    -- NULL once the tenant has been detached / never attached.
    generation             INTEGER,
    generation_pageserver  BIGINT,
    placement_policy       VARCHAR NOT NULL,
    -- JSON-encoded TenantConfig; opaque to the database.
    config                 TEXT NOT NULL,
    scheduling_policy      VARCHAR NOT NULL DEFAULT '"Active"',
    preferred_az_id        VARCHAR,
    PRIMARY KEY (tenant_id)
);

-- Pageserver registry.
CREATE TABLE storage_controller.nodes (
    node_id              BIGINT NOT NULL,
    scheduling_policy    VARCHAR NOT NULL,
    listen_http_addr     VARCHAR NOT NULL,
    listen_http_port     INTEGER NOT NULL,
    listen_pg_addr       VARCHAR NOT NULL,
    listen_pg_port       INTEGER NOT NULL,
    availability_zone_id VARCHAR NOT NULL,
    listen_https_port    INTEGER,
    lifecycle            VARCHAR NOT NULL DEFAULT 'active',
    listen_grpc_addr     VARCHAR,
    listen_grpc_port     INTEGER,
    PRIMARY KEY (node_id)
);

-- Safekeeper registry.
CREATE TABLE storage_controller.safekeepers (
    id                   BIGINT NOT NULL,
    region_id            TEXT NOT NULL,
    version              BIGINT NOT NULL,
    host                 TEXT NOT NULL,
    port                 INTEGER NOT NULL,
    http_port            INTEGER NOT NULL,
    availability_zone_id TEXT NOT NULL,
    scheduling_policy    VARCHAR NOT NULL DEFAULT 'activating',
    https_port           INTEGER,
    PRIMARY KEY (id)
);

-- Per-tenant metadata-scrubber health summary; FK cascade so a
-- tenant removal cleans up its row.
CREATE TABLE storage_controller.metadata_health (
    tenant_id        VARCHAR NOT NULL,
    healthy          BOOLEAN NOT NULL DEFAULT TRUE,
    last_scrubbed_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id),
    FOREIGN KEY (tenant_id)
        REFERENCES storage_controller.tenant_shards (tenant_id)
        ON DELETE CASCADE
);

-- Storage-controller leader-election bookkeeping.
CREATE TABLE storage_controller.controllers (
    address    VARCHAR NOT NULL,
    started_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (address, started_at)
);

-- Per-timeline safekeeper set + generation tracking.
CREATE TABLE storage_controller.timelines (
    tenant_id                   VARCHAR NOT NULL,
    timeline_id                 VARCHAR NOT NULL,
    start_lsn                   pg_lsn NOT NULL,
    generation                  INTEGER NOT NULL,
    sk_set                      BIGINT[] NOT NULL,
    deleted_at                  TIMESTAMPTZ,
    PRIMARY KEY (tenant_id, timeline_id)
);

-- Queue of pending generation-bump ops per (timeline, safekeeper).
CREATE TABLE storage_controller.safekeeper_timeline_pending_ops (
    sk_id       BIGINT NOT NULL,
    tenant_id   VARCHAR NOT NULL,
    timeline_id VARCHAR NOT NULL,
    generation  INTEGER NOT NULL,
    op_kind     VARCHAR NOT NULL,
    PRIMARY KEY (tenant_id, timeline_id, sk_id)
);

-- Per-timeline import progress, shard-keyed JSON.
CREATE TABLE storage_controller.timeline_imports (
    tenant_id      VARCHAR NOT NULL,
    timeline_id    VARCHAR NOT NULL,
    shard_statuses JSONB NOT NULL,
    PRIMARY KEY (tenant_id, timeline_id)
);

-- Hadron-specific safekeeper registry (separate from upstream
-- `safekeepers` for the hadron-only fast path).
CREATE TABLE storage_controller.hadron_safekeepers (
    sk_node_id       BIGINT NOT NULL,
    listen_http_addr VARCHAR NOT NULL,
    listen_http_port INTEGER NOT NULL,
    listen_pg_addr   VARCHAR NOT NULL,
    listen_pg_port   INTEGER NOT NULL,
    PRIMARY KEY (sk_node_id)
);

-- Timeline → safekeeper assignment, hadron-side.
CREATE TABLE storage_controller.hadron_timeline_safekeepers (
    timeline_id        VARCHAR NOT NULL,
    sk_node_id         BIGINT NOT NULL,
    legacy_endpoint_id UUID,
    PRIMARY KEY (timeline_id, sk_node_id)
);

-- =====================================================================
-- public.* — blob namespace + blobs/wals storage tables.
--
-- Shared by pageserver and safekeeper. The `blob_namespace` ENUM
-- discriminates between which service a row belongs to.
-- =====================================================================

CREATE TYPE public.blob_namespace AS ENUM ('pageserver', 'safekeeper');

CREATE TABLE public.blobs (
    tenant_id        TEXT                  NOT NULL,
    timeline_id      TEXT                  NOT NULL,
    namespace        public.blob_namespace NOT NULL,
    key              TEXT                  NOT NULL,
    generation       INT,
    chunk_idx        INT                   NOT NULL,
    data             BYTEA                 NOT NULL,
    size_bytes       BIGINT                NOT NULL,
    etag             TEXT                  NOT NULL,
    last_modified_us BIGINT                NOT NULL,
    metadata_json    TEXT,
    PRIMARY KEY ((tenant_id, timeline_id, key) HASH, namespace, chunk_idx)
);

CREATE TABLE public.wals (
    tenant_id        TEXT    NOT NULL,
    timeline_id      TEXT    NOT NULL,
    segno            BIGINT  NOT NULL,
    chunk_offset     BIGINT  NOT NULL,
    chunk_size       INT     NOT NULL,
    data             BYTEA   NOT NULL,
    last_modified_us BIGINT  NOT NULL,
    -- PK is the chunk's natural identity within a segment. The
    -- writer never produces overlapping chunks (truncate_wal aligns
    -- the next write at exactly the truncation point), so the
    -- chunk_offset alone uniquely identifies a chunk and a same-PK
    -- INSERT is necessarily an idempotent retry of the same bytes.
    PRIMARY KEY ((tenant_id, timeline_id) HASH, segno, chunk_offset)
);

-- =====================================================================
-- pageserver.* — yb-pageserver page-store tables.
-- =====================================================================

-- Relation pages only: keys where `Key::is_rel_block_key()` holds
-- (field1 == 0x00, field4 != 0, field6 != 0xffffffff). Hash partition
-- on (tenant, timeline, spcnode, dbnode, relnode, forknum) keeps a
-- relation's whole block-set on one tablet; range scans within a
-- relfork are tablet-local. `is_image` sits between `blknum` and
-- `lsn` so an anchor lookup
-- (`AND is_image=TRUE ORDER BY lsn DESC LIMIT 1`) is a single seek.
CREATE TABLE pageserver.pages (
    tenant_id    TEXT     NOT NULL,
    timeline_id  TEXT     NOT NULL,
    spcnode      BIGINT   NOT NULL,
    dbnode       BIGINT   NOT NULL,
    relnode      BIGINT   NOT NULL,
    forknum      SMALLINT NOT NULL,
    blknum       BIGINT   NOT NULL,
    is_image     BOOLEAN  NOT NULL,
    lsn          BIGINT   NOT NULL,
    will_init    BOOLEAN  NOT NULL,
    payload      BYTEA    NOT NULL,
    PRIMARY KEY ((tenant_id, timeline_id, spcnode, dbnode, relnode, forknum) HASH,
                 blknum, lsn DESC, is_image)
);

-- SLRU pages — pg_xact, pg_multixact_offsets, pg_multixact_members.
-- `kind` matches `Key::field2` byte ordering: 0=Clog,
-- 1=MultiXactMembers, 2=MultiXactOffsets. Hash partition keeps a
-- whole SLRU segment on one tablet.
CREATE TABLE pageserver.slru_pages (
    tenant_id    TEXT     NOT NULL,
    timeline_id  TEXT     NOT NULL,
    kind         SMALLINT NOT NULL,
    segno        BIGINT   NOT NULL,
    blknum       BIGINT   NOT NULL,
    is_image     BOOLEAN  NOT NULL,
    lsn          BIGINT   NOT NULL,
    will_init    BOOLEAN  NOT NULL,
    payload      BYTEA    NOT NULL,
    PRIMARY KEY ((tenant_id, timeline_id, kind, segno) HASH,
                 blknum, lsn DESC, is_image)
);

-- Metadata blobs — everything that isn't a relation page or SLRU
-- block: relsize sentinels, directory listings (TenantDir/DbDir/
-- RelDir/SlruDir), aux files (zenith.signal, postgresql.auto.conf,
-- replication-slot state), two-phase commit files, replication
-- origins, SLRU segment-size sentinels. Full-overwrite semantics:
-- read = latest row ≤ target_lsn. No walredo, no chains.
CREATE TABLE pageserver.meta (
    tenant_id    TEXT     NOT NULL,
    timeline_id  TEXT     NOT NULL,
    key          BYTEA    NOT NULL,
    lsn          BIGINT   NOT NULL,
    payload      BYTEA    NOT NULL,
    PRIMARY KEY ((tenant_id, timeline_id, key) HASH, lsn DESC)
);

-- Per-tenant `TenantConfig` (pan-cluster tunables: GC horizon, PITR
-- interval, walreceiver timeouts, …). Authoritative copy lives here;
-- storage_controller writes on tenant create / config update and
-- notifies the pageserver via PUT location_config. The pageserver
-- re-reads from this table on every notification + on startup. No
-- separate per-pageserver-location storage of this struct — every
-- pageserver attaching the tenant sees identical config.
CREATE TABLE pageserver.tenant_config (
    tenant_id     TEXT        NOT NULL,
    config        JSONB       NOT NULL,
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY ((tenant_id) HASH)
);

-- Per-timeline state row — replaces upstream Neon's local
-- `metadata`, `index_part.json` layer list, and
-- `tenant-manifest.json` files with one durable row in YB.
-- Carries every load-bearing field from `index_part.json`: archive
-- / delete flags, rel-size schema-migration state, plus a JSON
-- `lineage` column for future ancestor-detach support (always
-- `Default` in yb-mode today).
CREATE TABLE pageserver.timeline_state (
    tenant_id              TEXT      NOT NULL,
    timeline_id            TEXT      NOT NULL,
    consistent_lsn         BIGINT    NOT NULL,
    applied_gc_cutoff_lsn  BIGINT    NOT NULL,
    ancestor_timeline_id   TEXT,
    ancestor_lsn           BIGINT,
    current_generation     INT       NOT NULL,
    pg_version             INT       NOT NULL,
    initdb_lsn             BIGINT,
    prev_record_lsn        BIGINT,
    archived_at            TIMESTAMP,
    deleted_at             TIMESTAMP,
    rel_size_migration     TEXT,
    rel_size_migrated_at   BIGINT,
    lineage                JSONB,
    PRIMARY KEY ((tenant_id) HASH, timeline_id)
);

-- =====================================================================
-- safekeeper.* — yb-safekeeper persistent state.
-- =====================================================================

-- Per-timeline safekeeper control-file row — replaces upstream
-- Neon's `safekeeper.control` local-FS blob (and yb-light's previous
-- `safekeeper-control/<tenant>/<timeline>` blob in GenericRemote-
-- Storage). One row per timeline, hash-partitioned on
-- `(tenant_id, timeline_id)` so the row lives on a single tablet.
--
-- Column choice: hot-update LSN scalars get their own BIGINT
-- columns so a control-file save is a column-scoped UPDATE rather
-- than a 400-byte blob rewrite. Slow-update structures (`mconf`,
-- `term_history`) go in JSONB so their internal Rust-side shape can
-- evolve without an SQL migration.
CREATE TABLE safekeeper.timeline_state (
    tenant_id              TEXT        NOT NULL,
    timeline_id            TEXT        NOT NULL,
    timeline_start_lsn     BIGINT      NOT NULL,
    local_start_lsn        BIGINT      NOT NULL,
    remote_consistent_lsn  BIGINT      NOT NULL,
    pg_version             INT         NOT NULL,
    system_id              BIGINT      NOT NULL,
    wal_seg_size           INT         NOT NULL,
    term                   BIGINT      NOT NULL,
    term_history           JSONB       NOT NULL,
    proposer_uuid          BYTEA       NOT NULL,
    mconf                  JSONB       NOT NULL,
    creation_ts            TIMESTAMPTZ NOT NULL,
    PRIMARY KEY ((tenant_id, timeline_id) HASH)
);
