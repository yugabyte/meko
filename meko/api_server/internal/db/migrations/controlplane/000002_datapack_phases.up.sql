-- Per-datapack phases as the agent last reported them.
--
-- Until now a status report's datapack_phases were reduced to a count
-- (environments.datapacks), which threw away the only explanation of a state the
-- console shows constantly: an environment sits in PROVISIONING or DEGRADED because
-- one datapack is not Active, while every component reads healthy. The operator saw
-- "everything green, state PROVISIONING" and had no way to find out why without
-- reading agent logs in a cluster they may not have access to.
--
-- Shape mirrors components: a JSONB array written whole on each report, not a child
-- table. These are a snapshot of what the agent observes, replaced every tick and
-- never queried across environments — the rows a child table would give us would be
-- churn with no reader.
ALTER TABLE meko_system.environments
    ADD COLUMN IF NOT EXISTS datapack_phases JSONB NOT NULL DEFAULT '[]'::jsonb;
