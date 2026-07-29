-- 0003: the `amp_registry` schema — the controller's storage-layer bookkeeping:
-- generation authority (the fencing counter) plus the branch and endpoint
-- registries behind the HTTP surface the controller serves to yb_amp_local and
-- the integration harness.
--
-- Kept in its own `amp_registry` schema so the flat-storage model (32-hex branch_id
-- VARCHAR) stays separate from the controller product model (project/branch/
-- endpoint by UUID in `controller`). Every statement is IF-NOT-EXISTS
-- (idempotent). Objects are schema-qualified (the migrator's search_path is
-- `controller`).

CREATE SCHEMA IF NOT EXISTS amp_registry;

-- One row per branch: everything the controller and the launcher need to know
-- about a branch, from the moment its id is allocated.
--
-- yb-mode doesn't shard branches across pageservers (YB tablet-shards the
-- keyspace itself), so this is keyed only by `branch_id`.
--
-- Two groups of columns, with different writers:
--
--   * The FENCING group — `generation` + `placement_policy` — is owned by the
--     attach-hook. The generation counter is the fencing primitive: monotonic
--     across the branch's lifecycle (deliberately PRESERVED on detach, so a
--     later reattach still fences a pageserver from a prior generation) and
--     NULL only until the branch's first attach. Because detach keeps the
--     counter, `placement_policy` is what actually distinguishes detached from
--     attached; `Inspect` reads the pair.
--
--   * The CREATION group — `branch_name`, `pg_version`, `ancestor_branch_id`,
--     `start_lsn` — is written once by RecordBranch at branch-create time and
--     is what the launcher reads back to materialize the branch's pageserver +
--     safekeeper (a NULL `ancestor_branch_id` means bootstrap, a set one means
--     fork that ancestor at `start_lsn`).
--
-- The creation group is nullable because the attach-hook keeps a safety-net
-- INSERT for a branch that somehow reaches attach without having been created,
-- and that path has only a branch id to go on. `pg_version IS NOT NULL` is
-- therefore the predicate for "this branch was properly created", the same
-- shape as the presence predicates on `amp_storage.branch_state`.
--
-- A dropped branch is TOMBSTONED, not deleted: `dropped_at` is stamped and the
-- row stays. A branch id is never reused, so keeping the row keeps the
-- generation counter (and so the fencing history) intact, and leaves a record of
-- what the id was. Every reader that answers "is this branch live?" —
-- `Inspect`, `ListBranches`, `ListBranchNames` — filters `dropped_at IS NULL`.
--
-- The branch's data-plane state (consistent LSN, WAL start, the daemons' own
-- column groups) lives in `amp_storage.branch_state`, keyed by the same id.
CREATE TABLE IF NOT EXISTS amp_registry.branches (
    branch_id              VARCHAR NOT NULL,
    branch_name            VARCHAR,
    pg_version             INTEGER,
    ancestor_branch_id     VARCHAR,
    start_lsn              pg_lsn,
    generation             INTEGER,
    placement_policy       VARCHAR NOT NULL,
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    dropped_at             TIMESTAMPTZ,
    PRIMARY KEY (branch_id)
);

-- Durable registry of compute endpoints managed by yb_amp_local, keyed by
-- endpoint id. `spec` holds the endpoint config as opaque JSONB; the controller
-- never interprets it (it round-trips through yb_amp_local's serde model).
CREATE TABLE IF NOT EXISTS amp_registry.endpoints (
    endpoint_id  VARCHAR NOT NULL,
    branch_id    VARCHAR NOT NULL,
    spec         JSONB NOT NULL,
    PRIMARY KEY (endpoint_id)
);
