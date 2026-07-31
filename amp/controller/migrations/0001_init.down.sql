-- Reverse of 0001_init.up.sql, in dependency order (children before parents).

DROP TABLE IF EXISTS task_step;
DROP TABLE IF EXISTS task;
DROP TYPE  IF EXISTS step_status;
DROP TYPE  IF EXISTS task_status;

DROP TABLE IF EXISTS endpoint;
DROP TYPE  IF EXISTS endpoint_role;

DROP TABLE IF EXISTS branch;

DROP TABLE IF EXISTS user_project_grant;
DROP TYPE  IF EXISTS grant_role;
DROP TABLE IF EXISTS user_pool_grant;
DROP TABLE IF EXISTS project;
DROP TABLE IF EXISTS storage_pool;
DROP TABLE IF EXISTS app_user;
