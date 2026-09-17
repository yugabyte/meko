-- Only the constraint comes off. The backfill above it is not reversible: the
-- states it replaced were '' or garbage, and there is nothing to restore them to.
ALTER TABLE meko_system.environments
    DROP CONSTRAINT IF EXISTS environments_state_check;
