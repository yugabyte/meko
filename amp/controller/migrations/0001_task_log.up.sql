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
    updated_at   timestamptz NOT NULL DEFAULT now(),

    -- The YCP desired-state update this task applies, so the worker can stamp the
    -- resource row on completion rather than each planner remembering to (meko#985;
    -- ac-ycp.md "Convergence status"). NULL means "not YCP-driven": every task an
    -- operator or the UI plans directly. `ycp_resource` is plain text validated in
    -- Go — the worker switches on it anyway, and one rejection site beats two.
    ycp_resource    text,        -- 'endpoint' | 'branch'
    ycp_resource_id uuid,
    ycp_generation  bigint,

    -- Cooperative cancellation. A timestamp rather than a fifth task_status value:
    -- a cancelled task ends `failed` carrying the cancel as its error, and this
    -- column is what distinguishes that from having genuinely broken.
    cancel_requested_at timestamptz
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

-- The same first-writer-wins lock for add_branch_extensions (`target_ref` is the
-- branch id). The YCP agent re-issues an update until the row confirms it, and the
-- extensions task redeploys every endpoint on the branch, so without this an agent
-- restart mid-pass would plan concurrent helm upgrades of one release.
--
-- The two predicates are kind-DISJOINT, which is what keeps taskLockViolation's
-- structural classification exact: a unique violation from Create's task INSERT
-- can only be the index for that task's own kind.
CREATE UNIQUE INDEX tasks_one_active_ext_add_per_branch
    ON tasks (target_ref)
    WHERE kind = 'add_branch_extensions' AND status IN ('pending', 'running');

-- The task change feed (GET /api/v1/tasks?updated_since=...), which the YCP agent
-- polls every reconcile tick for every environment and which orders by updated_at
-- rather than the default listing's created_at.
--
-- `updated_at ASC` is load-bearing: YSQL defaults a secondary index's leading
-- column to HASH, which serves neither the `updated_at >= $n` seek nor the ORDER BY
-- — both degrade to a filter over the whole task log plus a sort (measured on the
-- sibling branches_gc_due: HASH scanned 10k rows in 627 ms, ASC seeked 5 in 2 ms).
-- `id ASC` second so the tiebreak is served from the index too; tasks stamped by
-- the same now() do tie.
CREATE INDEX tasks_by_updated_at ON tasks (updated_at ASC, id ASC);
