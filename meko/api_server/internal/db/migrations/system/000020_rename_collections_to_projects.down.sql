/* Reverse of the up: Projects back to Collections, in reverse order. */

ALTER TABLE meko_system.projects        RENAME CONSTRAINT uq_projects_user_name   TO uq_collections_user_name;
ALTER TABLE meko_system.project_folders RENAME CONSTRAINT uq_project_folders_name TO uq_collection_folders_name;

ALTER INDEX IF EXISTS meko_system.projects_pkey                RENAME TO collections_pkey;
ALTER INDEX IF EXISTS meko_system.project_folders_pkey         RENAME TO collection_folders_pkey;
ALTER INDEX IF EXISTS meko_system.idx_projects_user_id         RENAME TO idx_collections_user_id;
ALTER INDEX IF EXISTS meko_system.idx_project_folders_project  RENAME TO idx_collection_folders_collection;
ALTER INDEX IF EXISTS meko_system.idx_placements_project       RENAME TO idx_placements_collection;

ALTER TABLE meko_system.datapack_placements RENAME COLUMN project_id TO collection_id;
ALTER TABLE meko_system.project_folders     RENAME COLUMN project_id TO collection_id;

ALTER TABLE meko_system.projects RENAME COLUMN project_details TO collection_details;
ALTER TABLE meko_system.projects RENAME COLUMN project_name    TO collection_name;
ALTER TABLE meko_system.projects RENAME COLUMN project_id      TO collection_id;

ALTER TABLE meko_system.project_folders RENAME TO collection_folders;
ALTER TABLE meko_system.projects        RENAME TO collections;
