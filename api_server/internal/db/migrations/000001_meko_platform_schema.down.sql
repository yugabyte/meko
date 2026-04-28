-- Restore public connect
GRANT CONNECT ON DATABASE yugabyte TO PUBLIC;
GRANT CONNECT ON DATABASE postgres TO PUBLIC;

-- Drop langfuse mappings
DROP INDEX IF EXISTS meko_system.idx_langfuse_project_mapping_datapack_id;
DROP INDEX IF EXISTS meko_system.idx_langfuse_project_mapping_account;
DROP TABLE IF EXISTS meko_system.langfuse_project_mapping;
DROP TABLE IF EXISTS meko_system.langfuse_org_mapping;

-- Drop tier config
DROP TABLE IF EXISTS meko_system.tier_config;

-- Drop sharing tables
DROP TABLE IF EXISTS meko_system.datapack_invites;
DROP TYPE IF EXISTS meko_system.invite_status;

-- Drop core tables in reverse dependency order
DROP TABLE IF EXISTS meko_system.agent_access_permissions;
DROP TABLE IF EXISTS meko_system.agent_access;
DROP TABLE IF EXISTS meko_system.agents;
DROP TABLE IF EXISTS meko_system.knowledge_bases;
DROP TABLE IF EXISTS meko_system.datapacks;
DROP TABLE IF EXISTS meko_system.yb_universes;
DROP TABLE IF EXISTS meko_system.users;
DROP TABLE IF EXISTS meko_system.organization_registry;

-- Drop types and schema
DROP TYPE IF EXISTS meko_system.access_permission;
DROP TYPE IF EXISTS meko_system.access_object_type;
DROP TYPE IF EXISTS meko_system.account_type;
DROP SCHEMA IF EXISTS datapacks;
DROP SCHEMA IF EXISTS meko_system;
