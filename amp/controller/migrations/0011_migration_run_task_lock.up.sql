-- 0011: first-writer-wins for start_migration_run.
--
-- The twin of tasks_one_active_resize_per_endpoint and
-- tasks_one_active_ext_add_per_branch (both 0001), keyed on the migration id
-- (target_ref). The assess, migrate, and end handlers check for an active
-- migration_run before planning, but the launcher writes that row only once the
-- worker runs it, so two concurrent requests could both pass the check. This
-- index admits at most one non-terminal launcher per migration; a concurrent one
-- is ErrMigrationRunInProgress, which the API returns as 409 run_in_progress.
--
-- Same self-releasing shape: a terminal status drops the row out of the
-- predicate. By then the launcher has either marked its run running (which the
-- handler precheck sees) or failed it (which admits a retry).
--
-- The predicate names one kind, so it stays kind-DISJOINT from the other locks,
-- which is what keeps taskLockViolation's structural classification exact.
CREATE UNIQUE INDEX IF NOT EXISTS tasks_one_active_migration_run_per_migration
    ON tasks (target_ref)
    WHERE kind = 'start_migration_run' AND status IN ('pending', 'running');
