-- Revert 0003: drop the task list indexes.

DROP INDEX IF EXISTS task_by_status;
DROP INDEX IF EXISTS task_by_project;
DROP INDEX IF EXISTS task_by_created_at;
