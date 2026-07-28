-- 0003 down: drop the amp_registry schema.

DROP TABLE IF EXISTS amp_registry.endpoints;
DROP TABLE IF EXISTS amp_registry.pending_timeline_inits;
DROP TABLE IF EXISTS amp_registry.timelines;
DROP TABLE IF EXISTS amp_registry.branches;
DROP SCHEMA IF EXISTS amp_registry;
