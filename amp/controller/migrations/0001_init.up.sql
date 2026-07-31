-- amp controller initial schema.
--
-- Implements the design in controller/architecture/ac-highlvl.md §6 (resource ladder
-- + tables) and controller/architecture/amp-controller.md §8 (write-ahead task log)
-- and §10 (state). The
-- load-bearing invariants (§10.2) are expressed here in the schema, not only in
-- API checks: project-scoped composite FKs, the single-writer partial unique
-- index, one-default-per-project, and RESTRICT (never CASCADE) deletes.
--
-- The user-facing resource ladder is storage pool -> project -> branch ->
-- endpoint. The storage layer is flat: a branch's branch_id IS the storage-layer
-- BranchId (there is no enclosing tenant), so the controller keeps it verbatim.
-- A project is purely a control-plane grouping ("a database") that owns a set of
-- branches; it does NOT map to any storage object. The (project_id, branch_id)
-- composite keys below are the *product* data model — project-scoped lineage and
-- the single-writer rule — not a storage identity: only branch_id reaches the
-- storage layer.
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

-- A project is a "database": one project, one default branch. id is a
-- control-plane id only (no storage object maps to a project anymore).
CREATE TABLE project (
    id               uuid PRIMARY KEY,
    storage_pool_id  uuid        NOT NULL REFERENCES storage_pool (id),
    name             text        NOT NULL,
    created_at       timestamptz NOT NULL DEFAULT now(),
    UNIQUE (storage_pool_id, name)  -- name unique within its pool
);

-- The "share": lets a non-owner create and see projects in a pool.
CREATE TABLE user_pool_grant (
    user_id          uuid NOT NULL REFERENCES app_user (id),
    storage_pool_id  uuid NOT NULL REFERENCES storage_pool (id),
    PRIMARY KEY (user_id, storage_pool_id)
);

CREATE TYPE grant_role AS ENUM ('read_only', 'read_write');

-- Per-project access; revoke = drop the row.
CREATE TABLE user_project_grant (
    user_id     uuid       NOT NULL REFERENCES app_user (id),
    project_id  uuid       NOT NULL REFERENCES project (id),
    role        grant_role NOT NULL,
    PRIMARY KEY (user_id, project_id)
);

-- Physical -------------------------------------------------------------------

-- A branch is a PG cluster. branch_id is the storage-layer BranchId verbatim
-- (the flat storage identity). The product identity is the (project_id,
-- branch_id) pair, which scopes lineage to a project; the storage layer keys on
-- branch_id alone.
CREATE TABLE branch (
    project_id          uuid        NOT NULL REFERENCES project (id),
    branch_id           uuid        NOT NULL,
    name                text        NOT NULL,
    pg_version          int         NOT NULL,
    is_default          boolean     NOT NULL DEFAULT false,  -- the project's root branch

    parent_branch_id    uuid,        -- fork ancestry; NULL for the default branch
    fork_lsn            pg_lsn,      -- the point the branch was taken at

    created_at          timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (project_id, branch_id),
    UNIQUE (project_id, name),       -- name unique within its project
    FOREIGN KEY (project_id, parent_branch_id)  -- a fork parent stays in-project
        REFERENCES branch (project_id, branch_id)
);

-- Exactly one default branch per project; the rest are branches off it.
CREATE UNIQUE INDEX one_default_per_project ON branch (project_id) WHERE is_default;

CREATE TYPE endpoint_role AS ENUM ('read_write', 'read_only');

-- Durable record, ephemeral instance. Addressed by id; endpoints have no name.
CREATE TABLE endpoint (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),

    project_id   uuid          NOT NULL,
    branch_id    uuid          NOT NULL,
    role         endpoint_role NOT NULL,

    address         text,      -- connection details returned to users
    tailnet_address text,      -- optional second address: the Tailscale (tailnet)
                               --   URL, populated alongside the LoadBalancer address
                               --   when the endpoint is tailnet-exposed; NULL otherwise
    config       jsonb,        -- the spec used to (re)create the endpoint

    created_at   timestamptz   NOT NULL DEFAULT now(),

    FOREIGN KEY (project_id, branch_id)  -- project-scoped binding
        REFERENCES branch (project_id, branch_id)
);

-- The single-writer rule, enforced in the schema rather than only in code.
CREATE UNIQUE INDEX one_writer_per_branch
    ON endpoint (project_id, branch_id)
    WHERE role = 'read_write';

-- Operational (write-ahead task log, amp-controller.md §8) ---------------

CREATE TYPE task_status AS ENUM ('pending', 'running', 'succeeded', 'failed');
CREATE TYPE step_status AS ENUM ('pending', 'running', 'succeeded', 'failed');

-- One row per initiated operation; the user-visible, API-monitorable unit. It
-- carries its steps as an ordered list of step ids (step_ids); the steps
-- themselves live in task_step.
CREATE TABLE task (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    kind         text        NOT NULL,  -- e.g. 'create_project', 'branch', 'create_endpoint'
    status       task_status NOT NULL DEFAULT 'pending',
    project_id   uuid,                   -- target resource (for API monitoring); a
                                         --   task may create its own target, so no FK
    target_ref   text,
    params       jsonb       NOT NULL DEFAULT '{}'::jsonb,  -- the originating request
    step_ids     uuid[]      NOT NULL DEFAULT '{}',         -- steps in execution order
    error        text,                   -- set when status = 'failed'
    owner        text,                   -- which instance holds the task (§8.2)
    lease_expiry timestamptz,            -- ownership lease; expiry => reclaimable
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);

-- The idempotent steps of one task; written write-ahead, marked as they run. Each
-- has its own id (referenced by task.step_ids, which fixes execution order); the
-- task_id backref keeps "the steps of this task" a cheap, indexed lookup.
CREATE TABLE task_step (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    task_id    uuid        NOT NULL REFERENCES task (id),
    kind       text        NOT NULL,  -- which re-usable step
    status     step_status NOT NULL DEFAULT 'pending',
    args       jsonb       NOT NULL DEFAULT '{}'::jsonb,  -- inputs, written when planned
    result     jsonb,                  -- outputs, written when the step succeeds
    error      text,
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX task_step_by_task ON task_step (task_id);

-- The claim/resume query only ever scans unfinished tasks.
CREATE INDEX task_runnable ON task (status) WHERE status IN ('pending', 'running');

-- Indexes backing the task list endpoint (GET /api/v1/tasks): the list is ordered
-- by created_at DESC and optionally filtered by project_id and status.
CREATE INDEX task_by_created_at ON task (created_at);
CREATE INDEX task_by_project ON task (project_id);
CREATE INDEX task_by_status ON task (status);
