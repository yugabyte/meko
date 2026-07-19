-- 0003: the `registry` schema — the controller's storage-layer bookkeeping:
-- generation authority (the fencing counter) plus the node / branch / endpoint /
-- pending-init registries behind the HTTP surface the controller serves to
-- yb_amp_local and the integration harness.
--
-- Kept in its own `registry` schema so the flat-storage model (32-hex branch_id
-- VARCHAR) stays separate from the controller product model (project/branch/
-- endpoint by UUID in `controller`). Every statement is IF-NOT-EXISTS
-- (idempotent). Objects are schema-qualified (the migrator's search_path is
-- `controller`).

CREATE SCHEMA IF NOT EXISTS registry;

-- Per-branch placement + generation record. yb-mode doesn't shard branches
-- across pageservers (YB tablet-shards the keyspace itself), so this is keyed
-- only by `branch_id`. The generation counter (bumped by attach-hook) is the
-- fencing primitive; it is monotonic across the branch's lifecycle (preserved on
-- detach) and NULL only until the branch's first attach.
CREATE TABLE IF NOT EXISTS registry.branches (
    branch_id              VARCHAR NOT NULL,
    generation             INTEGER,
    generation_pageserver  BIGINT,
    placement_policy       VARCHAR NOT NULL,
    config                 JSONB NOT NULL,
    scheduling_policy      VARCHAR NOT NULL DEFAULT '"Active"',
    PRIMARY KEY (branch_id)
);

-- Pageserver registry (bookkeeping/observability; nothing gates on it).
CREATE TABLE IF NOT EXISTS registry.nodes (
    node_id              BIGINT NOT NULL,
    listen_http_addr     VARCHAR NOT NULL,
    listen_http_port     INTEGER NOT NULL,
    listen_pg_addr       VARCHAR NOT NULL,
    listen_pg_port       INTEGER NOT NULL,
    listen_https_port    INTEGER,
    lifecycle            VARCHAR NOT NULL DEFAULT 'active',
    listen_grpc_addr     VARCHAR,
    listen_grpc_port     INTEGER,
    PRIMARY KEY (node_id)
);

-- Per-branch WAL start LSN + generation tracking. (The branch's single
-- co-located safekeeper is owned by the launcher, not tracked here.)
CREATE TABLE IF NOT EXISTS registry.timelines (
    branch_id                   VARCHAR NOT NULL,
    start_lsn                   pg_lsn NOT NULL,
    generation                  INTEGER NOT NULL,
    deleted_at                  TIMESTAMPTZ,
    branch_name                 VARCHAR,
    PRIMARY KEY (branch_id)
);

-- Init recipe for a branch whose id + branch name are allocated (at
-- `branch create`) but whose pageserver + safekeeper aren't provisioned
-- yet. `endpoint create` reads it back to materialize the branch, then
-- deletes the row. A NULL ancestor_branch_id marks a Bootstrap recipe
-- (initdb + import on first start); a non-NULL one marks a Branch off
-- that ancestor at start_lsn.
CREATE TABLE IF NOT EXISTS registry.pending_timeline_inits (
    branch_id                   VARCHAR NOT NULL,
    branch_name                 VARCHAR NOT NULL,
    pg_version                  INTEGER NOT NULL,
    ancestor_branch_id          VARCHAR,
    start_lsn                   pg_lsn NOT NULL,
    PRIMARY KEY (branch_id)
);

-- Durable registry of compute endpoints managed by yb_amp_local, keyed by
-- endpoint id. `spec` holds the endpoint config as opaque JSONB; the controller
-- never interprets it (it round-trips through yb_amp_local's serde model).
CREATE TABLE IF NOT EXISTS registry.endpoints (
    endpoint_id  VARCHAR NOT NULL,
    branch_id    VARCHAR NOT NULL,
    spec         JSONB NOT NULL,
    PRIMARY KEY (endpoint_id)
);
