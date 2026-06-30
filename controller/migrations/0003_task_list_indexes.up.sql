-- 0003: indexes backing the task list endpoint (GET /api/v1/tasks). The list is
-- ordered by created_at DESC and optionally filtered by project_id and status;
-- these indexes keep that scan cheap as the task log grows. (The task_runnable
-- partial index from 0001/0002 serves the claim path, not the list path.)

CREATE INDEX IF NOT EXISTS task_by_created_at ON task (created_at);
CREATE INDEX IF NOT EXISTS task_by_project ON task (project_id);
CREATE INDEX IF NOT EXISTS task_by_status ON task (status);
