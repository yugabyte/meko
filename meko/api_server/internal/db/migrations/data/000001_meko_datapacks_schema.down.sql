-- Graph schemas exist only on installs that applied the earlier version of the up
-- migration, which created them with MAGE. Dropped here without needing mag_catalog,
-- so a down on a database without MAGE still works.
DROP SCHEMA IF EXISTS meko_promoted_graph CASCADE;
DROP SCHEMA IF EXISTS meko_multi_tenant_graph CASCADE;

-- Drop the entire datapacks schema. CASCADE removes all tables, indexes,
-- enum types, and any dependent objects.
DROP SCHEMA IF EXISTS datapacks CASCADE;
