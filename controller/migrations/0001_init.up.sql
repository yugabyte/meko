-- amp controller initial schema.
--
-- Implements the design in controller/architecture/ac-highlvl.md §6 (resource ladder
-- + tables) and controller/architecture/amp-controller.md §8 (write-ahead task log)
-- and §10 (state). The
-- load-bearing invariants (§10.2) are expressed here in the schema, not only in
-- API checks: tenant-scoped composite FKs, the single-writer partial unique
-- index, one-main-per-tenant, and RESTRICT (never CASCADE) deletes.
--
-- Objects are unqualified and created in the search_path schema (controller).

-- Ownership & access ---------------------------------------------------------

CREATE TABLE app_user (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    is_admin    boolean     NOT NULL DEFAULT false,  -- admins skip all grants
    created_at  timestamptz NOT NULL DEFAULT now()
);

-- A storage pool maps roughly to a Meko datapack. Any user can create one and
-- is recorded as its owner; others reach it only via a user_pool_grant.
CREATE TABLE storage_pool (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name           text        NOT NULL UNIQUE,
    owner_user_id  uuid        NOT NULL REFERENCES app_user (id),
    created_at     timestamptz NOT NULL DEFAULT now()
);

-- A tenant is a "database": one tenant, one main timeline. id is the
-- storage-layer tenant_id.
CREATE TABLE tenant (
    id               uuid PRIMARY KEY,
    storage_pool_id  uuid        NOT NULL REFERENCES storage_pool (id),
    name             text        NOT NULL,
    created_at       timestamptz NOT NULL DEFAULT now(),
    UNIQUE (storage_pool_id, name)  -- name unique within its pool
);

-- The "share": lets a non-owner create and see tenants in a pool.
CREATE TABLE user_pool_grant (
    user_id          uuid NOT NULL REFERENCES app_user (id),
    storage_pool_id  uuid NOT NULL REFERENCES storage_pool (id),
    PRIMARY KEY (user_id, storage_pool_id)
);

CREATE TYPE grant_role AS ENUM ('read_only', 'read_write');

-- Per-tenant access; revoke = drop the row.
CREATE TABLE user_tenant_grant (
    user_id    uuid       NOT NULL REFERENCES app_user (id),
    tenant_id  uuid       NOT NULL REFERENCES tenant (id),
    role       grant_role NOT NULL,
    PRIMARY KEY (user_id, tenant_id)
);

-- Physical -------------------------------------------------------------------

-- A pg_cluster *is* a timeline; identity is the (tenant_id, timeline_id) pair.
CREATE TABLE pg_cluster (
    tenant_id           uuid        NOT NULL REFERENCES tenant (id),
    timeline_id         uuid        NOT NULL,
    name                text        NOT NULL,
    pg_version          int         NOT NULL,
    is_main             boolean     NOT NULL DEFAULT false,  -- the tenant's root cluster

    parent_timeline_id  uuid,        -- fork ancestry; NULL for a root timeline
    fork_lsn            pg_lsn,      -- the point the branch was taken at

    created_at          timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (tenant_id, timeline_id),
    UNIQUE (tenant_id, name),        -- name unique within its tenant
    FOREIGN KEY (tenant_id, parent_timeline_id)  -- a fork parent stays in-tenant
        REFERENCES pg_cluster (tenant_id, timeline_id)
);

-- Exactly one main cluster per tenant; the rest are branches.
CREATE UNIQUE INDEX one_main_per_tenant ON pg_cluster (tenant_id) WHERE is_main;

CREATE TYPE endpoint_role AS ENUM ('read_write', 'read_only');

-- Durable record, ephemeral instance. Addressed by id; endpoints have no name.
CREATE TABLE endpoint (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),

    tenant_id    uuid          NOT NULL,
    timeline_id  uuid          NOT NULL,
    role         endpoint_role NOT NULL,

    address      text,         -- connection details returned to users
    config       jsonb,        -- the spec used to (re)create the endpoint

    created_at   timestamptz   NOT NULL DEFAULT now(),

    FOREIGN KEY (tenant_id, timeline_id)  -- tenant-scoped binding
        REFERENCES pg_cluster (tenant_id, timeline_id)
);

-- The single-writer rule, enforced in the schema rather than only in code.
CREATE UNIQUE INDEX one_writer_per_timeline
    ON endpoint (tenant_id, timeline_id)
    WHERE role = 'read_write';

-- Operational (write-ahead task log, amp-controller.md §8) ---------------

CREATE TYPE task_status AS ENUM ('pending', 'running', 'succeeded', 'failed');
CREATE TYPE step_status AS ENUM ('pending', 'running', 'succeeded', 'failed');

-- One row per initiated operation; the user-visible, API-monitorable unit.
CREATE TABLE task (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    kind         text        NOT NULL,  -- e.g. 'create_database', 'branch', 'scale'
    status       task_status NOT NULL DEFAULT 'pending',
    tenant_id    uuid,                   -- target resource (for API monitoring); a
                                         --   task may create its own target, so no FK
    target_ref   text,
    params       jsonb       NOT NULL DEFAULT '{}'::jsonb,  -- the originating request
    error        text,                   -- set when status = 'failed'
    owner        text,                   -- which instance holds the task (§8.2)
    lease_expiry timestamptz,            -- ownership lease; expiry => reclaimable
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);

-- The ordered, idempotent steps of one task; written write-ahead, marked as run.
CREATE TABLE task_step (
    task_id    uuid        NOT NULL REFERENCES task (id),
    ordinal    int         NOT NULL,  -- execution order within the task
    kind       text        NOT NULL,  -- which re-usable step
    status     step_status NOT NULL DEFAULT 'pending',
    args       jsonb       NOT NULL DEFAULT '{}'::jsonb,  -- inputs, written when planned
    result     jsonb,                  -- outputs, written when the step succeeds
    error      text,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (task_id, ordinal)
);

-- The claim/resume query only ever scans unfinished tasks.
CREATE INDEX task_runnable ON task (status) WHERE status IN ('pending', 'running');
