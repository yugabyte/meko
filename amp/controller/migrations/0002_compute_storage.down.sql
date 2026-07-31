-- 0002 down: drop the compute-facing (data-plane) schema of `yb_storage`.

DROP TABLE IF EXISTS amp_storage.wals;
DROP FUNCTION IF EXISTS amp_storage.gc_key_decode(BYTEA);
DROP TABLE IF EXISTS amp_storage.pending_gc;
DROP TABLE IF EXISTS amp_storage.branch_state;
DROP TABLE IF EXISTS amp_storage.meta;
DROP TABLE IF EXISTS amp_storage.slru_pages;
DROP TABLE IF EXISTS amp_storage.pages;
DROP TABLE IF EXISTS amp_storage.blobs;
DROP TYPE  IF EXISTS amp_storage.blob_namespace;
DROP SCHEMA IF EXISTS amp_storage;
