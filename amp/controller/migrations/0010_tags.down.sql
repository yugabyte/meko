-- Reverse 0010. Dropping the columns removes nothing from a running release:
-- the labels stay on its objects until the endpoint is next redeployed.

ALTER TABLE amp_registry.endpoints DROP COLUMN IF EXISTS tags;
ALTER TABLE amp_registry.clusters DROP COLUMN IF EXISTS tags;
