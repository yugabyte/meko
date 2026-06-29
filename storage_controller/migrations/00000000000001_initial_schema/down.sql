-- Inverse of up.sql — drop in reverse-dependency order so FK
-- references unwind cleanly.

-- safekeeper.* and its schema.
DROP TABLE IF EXISTS safekeeper.wals;
DROP TABLE IF EXISTS safekeeper.timeline_state;
DROP SCHEMA IF EXISTS safekeeper;

-- pageserver.* and its schema.
DROP TABLE IF EXISTS pageserver.timeline_state;
DROP TABLE IF EXISTS pageserver.tenant_config;
DROP TABLE IF EXISTS pageserver.meta;
DROP TABLE IF EXISTS pageserver.slru_pages;
DROP TABLE IF EXISTS pageserver.pages;
DROP SCHEMA IF EXISTS pageserver;

-- public.* blob store.
DROP TABLE IF EXISTS public.blobs;
DROP TYPE IF EXISTS public.blob_namespace;

-- storage_controller.* tables.
DROP TABLE IF EXISTS storage_controller.endpoints;
DROP TABLE IF EXISTS storage_controller.timeline_imports;
DROP TABLE IF EXISTS storage_controller.pending_timeline_inits;
DROP TABLE IF EXISTS storage_controller.timelines;
DROP TABLE IF EXISTS storage_controller.controllers;
DROP TABLE IF EXISTS storage_controller.metadata_health;
DROP TABLE IF EXISTS storage_controller.nodes;
DROP TABLE IF EXISTS storage_controller.branches;
