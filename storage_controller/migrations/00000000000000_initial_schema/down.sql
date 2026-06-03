-- Inverse of up.sql — drop in reverse-dependency order so FK
-- references unwind cleanly.

DROP TABLE IF EXISTS hadron_timeline_safekeepers;
DROP TABLE IF EXISTS hadron_safekeepers;
DROP TABLE IF EXISTS timeline_imports;
DROP TABLE IF EXISTS safekeeper_timeline_pending_ops;
DROP TABLE IF EXISTS timelines;
DROP TABLE IF EXISTS controllers;
DROP TABLE IF EXISTS metadata_health;
DROP TABLE IF EXISTS safekeepers;
DROP TABLE IF EXISTS nodes;
DROP TABLE IF EXISTS tenant_shards;

DROP FUNCTION IF EXISTS diesel_set_updated_at();
DROP FUNCTION IF EXISTS diesel_manage_updated_at(_tbl regclass);
