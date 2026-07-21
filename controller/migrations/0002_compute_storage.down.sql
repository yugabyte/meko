-- 0002 down: drop the compute-facing (data-plane) schemas of `yb_storage`.

DROP TABLE IF EXISTS safekeeper.wals;
DROP TABLE IF EXISTS safekeeper.timeline_state;
DROP SCHEMA IF EXISTS safekeeper;

DROP TABLE IF EXISTS pageserver.pending_gc;
DROP TABLE IF EXISTS pageserver.timeline_state;
DROP TABLE IF EXISTS pageserver.meta;
DROP TABLE IF EXISTS pageserver.slru_pages;
DROP TABLE IF EXISTS pageserver.pages;
DROP SCHEMA IF EXISTS pageserver;

DROP TABLE IF EXISTS public.blobs;
DROP TYPE IF EXISTS public.blob_namespace;
