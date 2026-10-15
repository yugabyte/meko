-- 0014: when a step's current attempt started.
--
-- updated_at is rewritten on every transition, so once a step finishes the time it
-- started is gone, and with it both the step's duration and the task's queue wait
-- (created_at to its first step's start). StartStep stamps this on every attempt,
-- so on a reclaimed step it is the attempt that ran last. NULL until a step first
-- runs, and on rows that predate this migration.
ALTER TABLE task_steps ADD COLUMN IF NOT EXISTS started_at timestamptz;
