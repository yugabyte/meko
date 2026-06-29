-- 0002: give each task step a first-class id, and let the task carry its ordered
-- list of step ids (controller/architecture/ac-tasks.md §8). The task tables hold
-- no production data yet — no API path plans tasks (#113) — so this drops and
-- recreates them in the new shape rather than migrating rows. The task_status /
-- step_status enums from 0001 are reused unchanged.

DROP TABLE IF EXISTS task_step;
DROP TABLE IF EXISTS task;

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
