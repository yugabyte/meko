-- Restore public connect
GRANT CONNECT ON DATABASE yugabyte TO PUBLIC;
GRANT CONNECT ON DATABASE postgres TO PUBLIC;

-- Drop the entire meko_system schema. CASCADE removes all tables, indexes,
-- enum types, and any dependent objects.
DROP SCHEMA IF EXISTS meko_system CASCADE;
