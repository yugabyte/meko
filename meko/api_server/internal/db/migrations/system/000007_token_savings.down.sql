-- Drop the token savings tables. CASCADE removes the log table's indexes
-- along with their parent tables.
DROP TABLE IF EXISTS meko_system.token_savings_summary CASCADE;
DROP TABLE IF EXISTS meko_system.token_savings_log CASCADE;
