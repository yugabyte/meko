-- 0012: a machine-readable class for a failed task's error.
--
-- A create planned as a task reports a refusal (a second read_write endpoint on a
-- branch) as a failed task rather than a synchronous 409, and `error` is free
-- text. A caller classifying the failure needs the class, not a string match.
-- NULL when the task did not fail or its cause has no code.
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS error_code text;
