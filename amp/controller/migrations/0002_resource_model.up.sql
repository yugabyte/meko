-- 0002: the `amp_registry` schema — the control plane's resource model.
--
-- Implements the design in controller/architecture/ac-highlvl.md §6 (resource
-- ladder + tables) and controller/architecture/amp-controller.md §10 (state).
-- The load-bearing invariants (§10.2) are expressed here in the schema, not only
-- in API checks: the single-writer partial unique index, one-default-per-cluster,
-- and RESTRICT (never CASCADE) deletes.
--
-- The user-facing resource ladder is storage pool -> cluster -> branch ->
-- endpoint. A cluster is purely a control-plane grouping ("a database") that owns
-- a set of branches; it does NOT map to any storage object.
--
-- ONE branch table and ONE endpoint table, reached through one API (/api/v1).
-- The storage layer renders the same branch id flat (32-hex) rather than as a
-- uuid, but that is a rendering of one identity, not a second registry.
--
-- Keys are flat. A branch's `branch_id` IS the storage layer's `BranchId`, a
-- 128-bit id carried here verbatim as `uuid` and globally unique on its own, so
-- it is the whole primary key; the storage layer is flat and has no enclosing
-- tenant. An endpoint keys on its own `id` and reaches its cluster through its
-- branch.
--
-- Every object here is SCHEMA-QUALIFIED. `amp_registry` is a fixed name, not a
-- deployment knob — the same as `amp_storage` — so the DDL says so outright
-- rather than inheriting whatever search_path it happens to run under. That
-- search_path belongs to the controller's own schema (--db-schema, default
-- `controller`, where the task log and schema_migrations live), which is why the
-- Go statements over these tables are schema-qualified too (db.ResourceSchema).

CREATE SCHEMA IF NOT EXISTS amp_registry;

-- Ownership & access ---------------------------------------------------------

CREATE TABLE amp_registry.users (                           -- "user" is reserved in PG; "users" is not
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    is_admin    boolean     NOT NULL DEFAULT false,  -- admins skip all grants
    created_at  timestamptz NOT NULL DEFAULT now()
);

-- A storage pool maps roughly to a Meko datapack. Any user can create one and
-- is recorded as its owner; others reach it only via a user_pool_grants row.
CREATE TABLE amp_registry.storage_pools (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name           text        NOT NULL UNIQUE,
    owner_user_id  uuid        NOT NULL REFERENCES amp_registry.users (id),
    created_at     timestamptz NOT NULL DEFAULT now()
);

-- A cluster is a "database": one cluster, one default branch. id is a
-- control-plane id only (no storage object maps to a cluster anymore).
--
-- Tombstoned, not deleted (see `branches` below for the full rationale): a
-- cluster owns branches, branches are tombstoned, and the branch -> cluster FK is
-- RESTRICT, so the cluster row has to outlive them.
CREATE TABLE amp_registry.clusters (
    id               uuid PRIMARY KEY,
    storage_pool_id  uuid        NOT NULL REFERENCES amp_registry.storage_pools (id),
    name             text        NOT NULL,
    created_at       timestamptz NOT NULL DEFAULT now(),
    dropped_at       timestamptz
);

-- Name unique within its pool, among LIVE clusters: dropping a cluster frees its
-- name for reuse (unlike a branch id, a cluster name is not an identity the
-- storage layer ever saw).
CREATE UNIQUE INDEX clusters_name_per_pool ON amp_registry.clusters (storage_pool_id, name)
    WHERE dropped_at IS NULL;

-- The "share": lets a non-owner create and see clusters in a pool.
CREATE TABLE amp_registry.user_pool_grants (
    user_id          uuid NOT NULL REFERENCES amp_registry.users (id),
    storage_pool_id  uuid NOT NULL REFERENCES amp_registry.storage_pools (id),
    PRIMARY KEY (user_id, storage_pool_id)
);

CREATE TYPE amp_registry.grant_role AS ENUM ('read_only', 'read_write');

-- Per-cluster access; revoke = drop the row.
CREATE TABLE amp_registry.user_cluster_grants (
    user_id     uuid       NOT NULL REFERENCES amp_registry.users (id),
    cluster_id  uuid       NOT NULL REFERENCES amp_registry.clusters (id),
    role        amp_registry.grant_role NOT NULL,
    PRIMARY KEY (user_id, cluster_id)
);

-- Physical -------------------------------------------------------------------

-- A branch is one logical Postgres database's data — what Postgres itself calls a
-- cluster. `branch_id` is the storage-layer BranchId verbatim
-- (the flat storage identity) and is globally unique, so it stands alone as the
-- primary key; the storage layer addresses it as `replace(branch_id::text,'-','')`.
-- `cluster_id` is an ordinary scoping column, not part of the identity.
--
-- This is also the branch's CREATION RECORD — `name`, `pg_version`,
-- `parent_branch_id`, `fork_lsn` — written at branch-create time and read back
-- by yb_amp_local to materialize the branch's pageserver + safekeeper (a NULL
-- parent means bootstrap, a set one means fork that ancestor at `fork_lsn`).
--
-- NB: with a single-column key the fork FK can no longer also assert that a
-- parent is in the SAME cluster — that was a side effect of the old composite
-- key. Lineage staying in-cluster is now an application invariant (the API
-- resolves a parent within the cluster before forking), not a schema one.
--
-- There is deliberately NO generation here. Fencing is the pageserver's own
-- concern: it claims a generation at startup by incrementing
-- `amp_storage.branch_state.current_generation`, so the counter lives in the same
-- row as the writes it fences and there is no second copy to keep in step.
--
-- A dropped branch is TOMBSTONED, not deleted: `dropped_at` is stamped and the
-- row stays, because a branch id is never reused, the record of what the id was
-- is worth keeping, and the branch's page/WAL data in amp_storage outlives the
-- drop (left for a later GC pass, #36). Every reader that answers "is this branch
-- live?" filters `dropped_at IS NULL`.
--
-- The branch's data-plane state (consistent LSN, WAL start, the daemons' own
-- column groups) lives in `amp_storage.branch_state`, keyed by the same id.
CREATE TABLE amp_registry.branches (
    branch_id           uuid        PRIMARY KEY,
    cluster_id          uuid        NOT NULL REFERENCES amp_registry.clusters (id),
    name                text        NOT NULL,
    pg_version          int         NOT NULL,
    is_default          boolean     NOT NULL DEFAULT false,  -- the cluster's root branch

    parent_branch_id    uuid        REFERENCES amp_registry.branches (branch_id),  -- fork ancestry; NULL for a root
    fork_lsn            pg_lsn,     -- the point the branch was taken at

    -- The branch's opted-in externalized extension set: the names, beyond the
    -- always-mounted catalog defaults, that endpoints on this branch mount as
    -- ImageVolumes (compute/architecture/extensions.md). It lives on the branch
    -- identity record so every branch-creation path inherits it — RecordBranch
    -- copies the ancestor's set into a fork's row at create time, and both a fork
    -- and a new read-replica endpoint pick it up. The create_endpoint opt-in
    -- unions names in; defaults to the empty set.
    extensions          jsonb       NOT NULL DEFAULT '[]'::jsonb,

    -- Extra PostgreSQL settings applied to every compute on this branch, as a
    -- flat {name: value} object. Like `extensions` it lives on the branch
    -- identity record so a fork inherits it: RecordBranch copies the ancestor's
    -- object into the new row. An endpoint's own `pg_settings` merge over these,
    -- so this is the baseline, not the last word.
    pg_settings         jsonb       NOT NULL DEFAULT '{}'::jsonb,

    created_at          timestamptz NOT NULL DEFAULT now(),
    dropped_at          timestamptz -- tombstone; a branch id is never reused
);

-- "The branches of this cluster" is the list endpoint, the storage-layer
-- surface's every read, and the cluster-delete cascade. It is the hottest access
-- path on the table and the primary key no longer serves it, so it gets an index
-- of its own — the partial unique index below cannot stand in, since it covers
-- only live rows and leads with a second column.
CREATE INDEX branches_by_cluster ON amp_registry.branches (cluster_id);

-- Name unique within its cluster, and exactly one default branch per cluster —
-- both among LIVE branches, so a drop frees the name and lets a replacement
-- default be designated. Partial because the rows stay behind forever.
CREATE UNIQUE INDEX branches_name_per_cluster ON amp_registry.branches (cluster_id, name)
    WHERE dropped_at IS NULL;
CREATE UNIQUE INDEX branches_one_default_per_cluster ON amp_registry.branches (cluster_id)
    WHERE is_default AND dropped_at IS NULL;

-- Fork lineage: "the children of this branch" gates a branch delete.
CREATE INDEX branches_by_parent ON amp_registry.branches (parent_branch_id);

CREATE TYPE amp_registry.endpoint_role AS ENUM ('read_write', 'read_only');

-- Durable record, ephemeral instance.
--
-- An endpoint belongs to a branch and reaches its cluster through it — there is
-- no cluster_id column, so the two can never disagree.
--
-- Every endpoint is created the same way, through the product API, whether the
-- instance it names is a pod the controller deploys or a local process
-- yb_amp_local spawns (deploy.LocalDeployer). What differs is only how much of
-- the row gets filled in: a local compute has no controller-assigned address.
CREATE TABLE amp_registry.endpoints (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),

    branch_id    uuid          NOT NULL REFERENCES amp_registry.branches (branch_id),
    role         amp_registry.endpoint_role NOT NULL,

    -- A caller-chosen handle, like every other resource in this schema has. It
    -- is optional: an endpoint is always addressable by `id`, and a caller that
    -- does not care can leave it unset.
    name         text,

    address         text,      -- connection details returned to users
    tailnet_address text,      -- optional second address: the Tailscale (tailnet)
                               --   URL, populated alongside the LoadBalancer address
                               --   when the endpoint is tailnet-exposed; NULL otherwise

    -- The spec used to (re)create the endpoint: `cpu`, `memory`, `extensions`
    -- and friends, written by the create_endpoint step. It also carries the
    -- caller's `extended` object — configuration the control plane stores and
    -- hands to the deployer WITHOUT interpreting (for a locally-deployed
    -- compute, the ports it binds). Nested under one key so a passthrough field
    -- can never collide with a product one.
    config       jsonb,

    -- This endpoint's own extra PostgreSQL settings, a flat {name: value}
    -- object. The effective set for the compute is the branch's merged with
    -- this one, and this one wins on a key both define — branch-wide policy with
    -- a per-endpoint override, which is what lets one read replica run with a
    -- different work_mem than its writer.
    pg_settings  jsonb         NOT NULL DEFAULT '{}'::jsonb,

    -- The same idea for the two storage daemons that run in the endpoint's pod.
    -- Each is a flat {key: value} object of top-level toml keys, e.g.
    --   {"scale_factor": 8, "wal_cache_size_mib": 512}
    -- written verbatim into pageserver.toml / safekeeper.toml by
    -- the endpoint supervisor, so they beat every resource-derived default
    -- it computes — the same precedence an explicitly-set toml value already has
    -- over a scale_factor-derived one inside the pageserver.
    --
    -- Two columns rather than one blob: the daemons have separate config surfaces
    -- and separate reload semantics (a scale_factor change restarts the
    -- pageserver, a wal_cache_size_mib change restarts the safekeeper), so the
    -- diff that decides what to restart is per-daemon.
    pageserver_settings jsonb  NOT NULL DEFAULT '{}'::jsonb,
    safekeeper_settings jsonb  NOT NULL DEFAULT '{}'::jsonb,

    created_at   timestamptz   NOT NULL DEFAULT now()
);

-- The single-writer rule, enforced in the schema rather than only in code. It
-- covers the launcher's endpoints too, which is why yb_amp_local no longer needs
-- its own duplicate-primary check to be the only thing standing in the way.
CREATE UNIQUE INDEX endpoints_one_writer_per_branch
    ON amp_registry.endpoints (branch_id)
    WHERE role = 'read_write';

-- A name identifies an endpoint within its branch, the way a branch name does
-- within its cluster. Not global: two branches may each have a "primary", which
-- is the natural thing to call the writer on each of them. NULLs are distinct in
-- a unique index, so any number of unnamed endpoints coexist.
CREATE UNIQUE INDEX endpoints_name_per_branch
    ON amp_registry.endpoints (branch_id, name);

-- "The endpoints of this branch" is the cascade-delete enumeration and the list
-- endpoint's filter. one_writer_per_branch covers only read_write rows, so
-- read-only endpoints need an index of their own.
CREATE INDEX endpoints_by_branch ON amp_registry.endpoints (branch_id);
