-- Initial schema for storage_controller.
--
-- Migrations run with `search_path = storage_controller, public`
-- (see `set_search_path` in persistence.rs), so unqualified objects
-- land in `storage_controller`. The schema itself is created by
-- `ensure_schema_exists` before any migration runs.

-- Diesel helper functions used by `updated_at` triggers. Kept for
-- forward compatibility even though no current table has an
-- `updated_at` column.
CREATE OR REPLACE FUNCTION diesel_manage_updated_at(_tbl regclass) RETURNS VOID AS $$
BEGIN
    EXECUTE format('CREATE TRIGGER set_updated_at BEFORE UPDATE ON %s
                    FOR EACH ROW EXECUTE PROCEDURE diesel_set_updated_at()', _tbl);
END;
$$ LANGUAGE plpgsql;

CREATE OR REPLACE FUNCTION diesel_set_updated_at() RETURNS trigger AS $$
BEGIN
    IF (
        NEW IS DISTINCT FROM OLD AND
        NEW.updated_at IS NOT DISTINCT FROM OLD.updated_at
    ) THEN
        NEW.updated_at := current_timestamp;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- Pageserver shard placement and configuration.
CREATE TABLE tenant_shards (
    tenant_id              VARCHAR NOT NULL,
    shard_number           INTEGER NOT NULL,
    shard_count            INTEGER NOT NULL,
    shard_stripe_size      INTEGER NOT NULL,
    -- NULL once the shard has been detached / never attached.
    generation             INTEGER,
    generation_pageserver  BIGINT,
    placement_policy       VARCHAR NOT NULL,
    splitting              SMALLINT NOT NULL,
    -- JSON-encoded TenantConfig; opaque to the database.
    config                 TEXT NOT NULL,
    scheduling_policy      VARCHAR NOT NULL DEFAULT '"Active"',
    preferred_az_id        VARCHAR,
    PRIMARY KEY (tenant_id, shard_number, shard_count)
);

CREATE INDEX tenant_shards_tenant_id ON tenant_shards (tenant_id);

-- Pageserver registry.
CREATE TABLE nodes (
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
CREATE TABLE safekeepers (
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

-- Per-shard metadata-scrubber health summary; FK cascade so a shard
-- removal cleans up its row.
CREATE TABLE metadata_health (
    tenant_id        VARCHAR NOT NULL,
    shard_number     INTEGER NOT NULL,
    shard_count      INTEGER NOT NULL,
    healthy          BOOLEAN NOT NULL DEFAULT TRUE,
    last_scrubbed_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, shard_number, shard_count),
    FOREIGN KEY (tenant_id, shard_number, shard_count)
        REFERENCES tenant_shards (tenant_id, shard_number, shard_count)
        ON DELETE CASCADE
);

-- Storage-controller leader-election bookkeeping.
CREATE TABLE controllers (
    address    VARCHAR NOT NULL,
    started_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (address, started_at)
);

-- Per-timeline safekeeper set + generation tracking.
CREATE TABLE timelines (
    tenant_id                   VARCHAR NOT NULL,
    timeline_id                 VARCHAR NOT NULL,
    start_lsn                   pg_lsn NOT NULL,
    generation                  INTEGER NOT NULL,
    sk_set                      BIGINT[] NOT NULL,
    new_sk_set                  BIGINT[],
    cplane_notified_generation  INTEGER NOT NULL,
    deleted_at                  TIMESTAMPTZ,
    sk_set_notified_generation  INTEGER NOT NULL DEFAULT 1,
    PRIMARY KEY (tenant_id, timeline_id)
);

-- Queue of pending generation-bump ops per (timeline, safekeeper).
CREATE TABLE safekeeper_timeline_pending_ops (
    sk_id       BIGINT NOT NULL,
    tenant_id   VARCHAR NOT NULL,
    timeline_id VARCHAR NOT NULL,
    generation  INTEGER NOT NULL,
    op_kind     VARCHAR NOT NULL,
    PRIMARY KEY (tenant_id, timeline_id, sk_id)
);

-- Per-timeline import progress, shard-keyed JSON.
CREATE TABLE timeline_imports (
    tenant_id      VARCHAR NOT NULL,
    timeline_id    VARCHAR NOT NULL,
    shard_statuses JSONB NOT NULL,
    PRIMARY KEY (tenant_id, timeline_id)
);

-- Hadron-specific safekeeper registry (separate from upstream
-- `safekeepers` for the hadron-only fast path).
CREATE TABLE hadron_safekeepers (
    sk_node_id       BIGINT NOT NULL,
    listen_http_addr VARCHAR NOT NULL,
    listen_http_port INTEGER NOT NULL,
    listen_pg_addr   VARCHAR NOT NULL,
    listen_pg_port   INTEGER NOT NULL,
    PRIMARY KEY (sk_node_id)
);

-- Timeline → safekeeper assignment, hadron-side.
CREATE TABLE hadron_timeline_safekeepers (
    timeline_id        VARCHAR NOT NULL,
    sk_node_id         BIGINT NOT NULL,
    legacy_endpoint_id UUID,
    PRIMARY KEY (timeline_id, sk_node_id)
);
