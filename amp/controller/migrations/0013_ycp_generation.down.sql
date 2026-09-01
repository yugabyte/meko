-- Reverse 0013.
ALTER TABLE tasks DROP COLUMN IF EXISTS ycp_generation;
ALTER TABLE tasks DROP COLUMN IF EXISTS ycp_resource_id;
ALTER TABLE tasks DROP COLUMN IF EXISTS ycp_resource;

ALTER TABLE amp_registry.branches  DROP COLUMN IF EXISTS ycp_generation;
ALTER TABLE amp_registry.endpoints DROP COLUMN IF EXISTS ycp_generation;
