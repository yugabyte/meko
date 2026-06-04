-- Inverse of up.sql — drop in reverse-dependency order so FK
-- references unwind cleanly.

-- pageserver.* and its schema.
DROP TABLE IF EXISTS pageserver.timeline_state;
DROP TABLE IF EXISTS pageserver.tenant_config;
DROP TABLE IF EXISTS pageserver.meta;
DROP TABLE IF EXISTS pageserver.slru_pages;
DROP TABLE IF EXISTS pageserver.pages;
DROP SCHEMA IF EXISTS pageserver;

-- public.* blob/wal store.
DROP TABLE IF EXISTS public.wals;
DROP TABLE IF EXISTS public.blobs;
DROP TYPE IF EXISTS public.blob_namespace;

-- storage_controller.* tables.
DROP TABLE IF EXISTS storage_controller.hadron_timeline_safekeepers;
DROP TABLE IF EXISTS storage_controller.hadron_safekeepers;
DROP TABLE IF EXISTS storage_controller.timeline_imports;
DROP TABLE IF EXISTS storage_controller.safekeeper_timeline_pending_ops;
DROP TABLE IF EXISTS storage_controller.timelines;
DROP TABLE IF EXISTS storage_controller.controllers;
DROP TABLE IF EXISTS storage_controller.metadata_health;
DROP TABLE IF EXISTS storage_controller.safekeepers;
DROP TABLE IF EXISTS storage_controller.nodes;
DROP TABLE IF EXISTS storage_controller.tenant_shards;
