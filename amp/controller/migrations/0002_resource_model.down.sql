-- Reverse of 0002_resource_model.up.sql, in dependency order (children before
-- parents).
--
-- The schema goes too: unlike the controller's own schema it holds nothing but
-- the resource model, and it is not where schema_migrations lives.

DROP TABLE IF EXISTS amp_registry.endpoints;
DROP TYPE  IF EXISTS amp_registry.endpoint_role;

DROP TABLE IF EXISTS amp_registry.branches;

DROP TABLE IF EXISTS amp_registry.user_project_grants;
DROP TYPE  IF EXISTS amp_registry.grant_role;
DROP TABLE IF EXISTS amp_registry.user_pool_grants;
DROP TABLE IF EXISTS amp_registry.projects;
DROP TABLE IF EXISTS amp_registry.storage_pools;
DROP TABLE IF EXISTS amp_registry.users;
DROP SCHEMA IF EXISTS amp_registry;
