-- Drop Apache AGE graphs (these create their own schemas)
SET search_path = mag_catalog, public;
SELECT * FROM mag_catalog.drop_graph('meko_promoted_graph', true);
SELECT * FROM mag_catalog.drop_graph('meko_multi_tenant_graph', true);

-- Drop the entire datapacks schema. CASCADE removes all tables, indexes,
-- enum types, and any dependent objects.
DROP SCHEMA IF EXISTS datapacks CASCADE;
