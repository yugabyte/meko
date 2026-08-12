-- 0005: make sure the endpoint's pageserver/safekeeper override columns exist.
--
-- A repair, not a definition: `0002` creates both columns and remains their
-- definition. They were added to it after it had shipped, and golang-migrate will
-- not re-run an applied version, so databases past `0002` are missing them.
--
-- IF NOT EXISTS keeps it free where it is not needed: in YugabyteDB an ADD COLUMN
-- that adds nothing does not bump the catalog version, so a fresh database
-- performs no schema change here.

ALTER TABLE amp_registry.endpoints
    ADD COLUMN IF NOT EXISTS pageserver_settings jsonb NOT NULL DEFAULT '{}'::jsonb;

ALTER TABLE amp_registry.endpoints
    ADD COLUMN IF NOT EXISTS safekeeper_settings jsonb NOT NULL DEFAULT '{}'::jsonb;
