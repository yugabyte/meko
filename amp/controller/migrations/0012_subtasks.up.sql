-- 0012: `subtasks` — the children a fan-out parent task owns and runs itself
-- (controller/architecture/ac-endpoint-upgrade.md §5).
--
-- A fleet roll is one parent task that records a row here per endpoint and runs
-- them in-process. They are deliberately NOT rows in `tasks`. The claim loop reads
-- `tasks`, so a child there could be claimed by another instance while its parent
-- was already running it, and neither the endpoint lock nor the owner fence would
-- catch the double-run: both runs compute the same lease owner, which pglease
-- grants unconditionally to the current holder, and both name the same process in
-- `tasks.owner` (§5.2). A separate table makes that unrepresentable instead of
-- resting it on a WHERE clause. `tasks` is untouched — no query change, and a roll
-- of N endpoints still adds exactly one row to GET /tasks.
--
-- A subtask is ONE registered step with durable status, not a task with a step
-- list, so there is no `subtask_steps`: the step machinery exists to resume a
-- multi-step task at the right step, and a fan-out child gets nothing from it. A
-- child that ever needs two phases wants to be a real task. Hence `kind` is a STEP
-- kind — the same Registry key `task_steps.kind` uses — and `status` is 0001's
-- `step_status`, not `task_status`.
--
-- Two columns carry the design's invariants. `owner` is the PARENT's owner,
-- stamped when the child starts, so every child write is fenced on the same thing
-- every `tasks` write is, and a reclaimed parent re-stamps its children to fence
-- out the process it took over from. And `id` is derived from
-- (parent_id, kind, args) rather than defaulted, so a parent replaying its step
-- converges on the child it already created; the primary key is what enforces that.

CREATE TABLE IF NOT EXISTS subtasks (
    -- No DEFAULT, unlike `tasks` and `task_steps`: the id is the whole idempotency
    -- mechanism, so an insert that omits it must fail loudly rather than be handed a
    -- random one and silently lose replay convergence.
    id         uuid        PRIMARY KEY,
    parent_id  uuid        NOT NULL REFERENCES tasks (id),
    kind       text        NOT NULL,  -- a STEP kind: the Registry key, not a task kind
    status     step_status NOT NULL DEFAULT 'pending',
    args       jsonb       NOT NULL DEFAULT '{}'::jsonb,  -- inputs, written when created
    result     jsonb,                  -- outputs, written when the child succeeds
    error      text,
    owner      text,                   -- the parent's owner, for the write fence
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- "the children of this parent" is the only bulk lookup: the parent's replay
-- reconciliation, and GET /tasks/{taskId}/subtasks.
CREATE INDEX IF NOT EXISTS subtasks_by_parent ON subtasks (parent_id);
