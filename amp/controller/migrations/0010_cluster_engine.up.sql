-- The engine a cluster was provisioned as. An engine is a named bundle of
-- externalized extensions (compute/architecture/extensions.md): `aidb` seeds
-- pgvector + age + pg_cron, `oracle` seeds hexabridge, and `postgres` seeds
-- nothing. The controller resolves the bundle at create and seeds it onto the
-- default branch's `extensions`, so this column records what the cluster was
-- created as, not a constraint the branch is held to.
--
-- Extensions are add-only (there is no removal API), so an engine's bundle stays
-- a subset of the branch's set for the cluster's whole life — a stored engine
-- cannot become a lie. Pre-existing clusters were all plain Postgres.
-- IF NOT EXISTS keeps the step re-runnable: migrations follow the same
-- idempotency contract as every other YB write, and on YugabyteDB an ADD COLUMN
-- that adds nothing does not bump the catalog version, so the guard is free
-- (0005_endpoint_daemon_settings documents the same reasoning).
ALTER TABLE amp_registry.clusters
    ADD COLUMN IF NOT EXISTS engine text NOT NULL DEFAULT 'postgres';
