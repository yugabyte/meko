-- Reverse of 0001_task_log.up.sql, in dependency order (children before parents).
--
-- The schema itself is left in place: it is the migrator's search_path schema and
-- owns schema_migrations, so dropping it would take the version table with it.

DROP TABLE IF EXISTS task_steps;
DROP TABLE IF EXISTS tasks;
DROP TYPE  IF EXISTS step_status;
DROP TYPE  IF EXISTS task_status;
