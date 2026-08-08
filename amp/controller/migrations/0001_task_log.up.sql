-- 0001: the `controller` schema — the write-ahead task log
-- (controller/architecture/amp-controller.md §8).
--
-- This is the controller's own operational machinery: how it gets work done, not
-- a resource a user ever addresses by name. That is why it sits in a schema of
-- its own, apart from the resource model (0002) and the data plane (0003).
--
-- This is the schema --db-schema names (default `controller`), so it is the one
-- the migrator's search_path and the runtime pool point at, and its objects are
-- written unqualified. The other two schemas are fixed names and are always
-- referenced schema-qualified.
--
-- It is provisioned FIRST so a fresh database has somewhere to record work from
-- the moment the controller starts. Nothing here references the resource model:
-- `tasks.cluster_id` is deliberately FK-less, because a task may create its own
-- target.

CREATE TYPE task_status AS ENUM ('pending', 'running', 'succeeded', 'failed');
CREATE TYPE step_status AS ENUM ('pending', 'running', 'succeeded', 'failed');

-- One row per initiated operation; the user-visible, API-monitorable unit. It
-- carries its steps as an ordered list of step ids (step_ids); the steps
-- themselves live in task_steps.
CREATE TABLE tasks (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    kind         text        NOT NULL,  -- e.g. 'create_cluster', 'branch', 'create_endpoint'
    status       task_status NOT NULL DEFAULT 'pending',
    cluster_id   uuid,                   -- target resource (for API monitoring); a
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
-- has its own id (referenced by tasks.step_ids, which fixes execution order); the
-- task_id backref keeps "the steps of this task" a cheap, indexed lookup.
CREATE TABLE task_steps (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    task_id    uuid        NOT NULL REFERENCES tasks (id),
    kind       text        NOT NULL,  -- which re-usable step
    status     step_status NOT NULL DEFAULT 'pending',
    args       jsonb       NOT NULL DEFAULT '{}'::jsonb,  -- inputs, written when planned
    result     jsonb,                  -- outputs, written when the step succeeds
    error      text,
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX task_steps_by_task ON task_steps (task_id);

-- The claim/resume query only ever scans unfinished tasks.
CREATE INDEX tasks_runnable ON tasks (status) WHERE status IN ('pending', 'running');

-- Indexes backing the task list endpoint (GET /api/v1/tasks): the list is ordered
-- by created_at DESC and optionally filtered by cluster_id and status.
CREATE INDEX tasks_by_created_at ON tasks (created_at);
CREATE INDEX tasks_by_cluster ON tasks (cluster_id);
CREATE INDEX tasks_by_status ON tasks (status);

-- First-writer-wins lock for endpoint resize.
--
-- A resize (`resize_endpoint` task) should hold its endpoint until it finishes:
-- while one is in flight, a second concurrent resize of the same endpoint must be
-- rejected (409) rather than racing it to a nondeterministic final size. This
-- partial unique index enforces exactly that — at most one non-terminal
-- `resize_endpoint` task per endpoint (`target_ref` is the endpoint id). The
-- second concurrent INSERT violates it and the store surfaces a conflict.
--
-- The lock is self-releasing: when the in-flight task reaches a terminal status
-- (`succeeded`/`failed`) it drops out of the index's predicate, so the next
-- resize is admitted. A crashed task is reclaimed via its lease and eventually
-- reaches terminal too, so the lock cannot wedge permanently.
CREATE UNIQUE INDEX tasks_one_active_resize_per_endpoint
    ON tasks (target_ref)
    WHERE kind = 'resize_endpoint' AND status IN ('pending', 'running');
