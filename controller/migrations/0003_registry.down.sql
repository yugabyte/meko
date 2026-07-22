-- 0003 down: drop the registry schema.

DROP TABLE IF EXISTS registry.endpoints;
DROP TABLE IF EXISTS registry.pending_timeline_inits;
DROP TABLE IF EXISTS registry.timelines;
DROP TABLE IF EXISTS registry.branches;
DROP SCHEMA IF EXISTS registry;
