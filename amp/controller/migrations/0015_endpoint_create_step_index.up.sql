-- 0015: find the task that created an endpoint.
--
-- Every endpoint is created by a create_endpoint STEP, inside a create_endpoint,
-- create_cluster or branch task, and the step's args are the only place that names
-- the endpoint for all three (target_ref carries it only for the first). The
-- endpoint list reads that link on every request, and the task log is never
-- trimmed, so the lookup is indexed rather than a scan of every step.
CREATE INDEX IF NOT EXISTS task_steps_create_endpoint
    ON task_steps ((args->>'endpoint_id'))
    WHERE kind = 'create_endpoint';
