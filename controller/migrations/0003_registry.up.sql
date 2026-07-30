-- 0003: the `amp_registry` schema — the controller's storage-layer bookkeeping:
-- the branch and endpoint registries behind the HTTP surface the controller
-- serves to yb_amp_local and the integration harness.
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
-- The row is the branch's CREATION RECORD — `branch_name`, `pg_version`,
-- `ancestor_branch_id`, `start_lsn` — written once by RecordBranch at
-- branch-create time and read back by the launcher to materialize the branch's
-- pageserver + safekeeper (a NULL `ancestor_branch_id` means bootstrap, a set
-- one means fork that ancestor at `start_lsn`).
--
-- There is deliberately NO generation here. Fencing is the pageserver's own
-- concern: it claims a generation at startup by incrementing
-- `amp_storage.branch_state.current_generation` and taking what that statement
-- returns, so the counter lives in the same row as the writes it fences and
-- there is no second copy to keep in step.
--
-- A dropped branch is TOMBSTONED, not deleted: `dropped_at` is stamped and the
-- row stays, because a branch id is never reused and the record of what the id
-- was is worth keeping. Every reader that answers "is this branch live?" —
-- `ListBranches`, `ListBranchNames` — filters `dropped_at IS NULL`.
--
-- The branch's data-plane state (consistent LSN, WAL start, the daemons' own
-- column groups) lives in `amp_storage.branch_state`, keyed by the same id.
CREATE TABLE IF NOT EXISTS amp_registry.branches (
    branch_id              VARCHAR NOT NULL,
    branch_name            VARCHAR,
    pg_version             INTEGER,
    ancestor_branch_id     VARCHAR,
    start_lsn              pg_lsn,
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
