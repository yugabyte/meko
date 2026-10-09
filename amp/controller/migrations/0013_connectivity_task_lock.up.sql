-- 0013: first-writer-wins for update_endpoint_connectivity.
--
-- The twin of tasks_one_active_resize_per_endpoint (0001), keyed on the endpoint
-- id (target_ref). Each task replaces the endpoint's whole allowlist, so two in
-- flight would land in whichever order the worker claimed them; a concurrent one
-- is ErrConnectivityInProgress, which the API returns as 409.
--
-- The predicate names one kind, so it stays kind-DISJOINT from the other locks,
-- which is what keeps taskLockViolation's structural classification exact.
CREATE UNIQUE INDEX IF NOT EXISTS tasks_one_active_connectivity_per_endpoint
    ON tasks (target_ref)
    WHERE kind = 'update_endpoint_connectivity' AND status IN ('pending', 'running');
