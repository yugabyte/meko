/*
 * Collections are called Projects.
 *
 * A rename rather than an edit of 000018: that migration has shipped, so the
 * tables exist and only a new migration can move them. Renames are metadata
 * only -- no rows are rewritten and no data moves.
 *
 * NOT upgrade safe, deliberately. A build from before this migration queries
 * meko_system.collections, which stops existing the moment this runs, so an
 * old pod serving alongside a new one fails. Taken knowingly because the
 * feature is flag-gated and is not live in production. Like any migration it
 * also raises the downgrade floor.
 *
 * The flag is renamed with it: MEKO_ENABLE_COLLECTIONS / --enable-collections
 * become MEKO_ENABLE_PROJECTS / --enable-projects, so deploy config that sets
 * the old name turns the feature OFF rather than erroring.
 */

ALTER TABLE meko_system.collections        RENAME TO projects;
ALTER TABLE meko_system.collection_folders RENAME TO project_folders;

ALTER TABLE meko_system.projects RENAME COLUMN collection_id      TO project_id;
ALTER TABLE meko_system.projects RENAME COLUMN collection_name    TO project_name;
ALTER TABLE meko_system.projects RENAME COLUMN collection_details TO project_details;

ALTER TABLE meko_system.project_folders    RENAME COLUMN collection_id TO project_id;
ALTER TABLE meko_system.datapack_placements RENAME COLUMN collection_id TO project_id;

-- Renaming a table leaves its indexes and constraints under the old name, so
-- each one is moved explicitly.
ALTER INDEX IF EXISTS meko_system.collections_pkey                 RENAME TO projects_pkey;
ALTER INDEX IF EXISTS meko_system.collection_folders_pkey          RENAME TO project_folders_pkey;
ALTER INDEX IF EXISTS meko_system.idx_collections_user_id          RENAME TO idx_projects_user_id;
ALTER INDEX IF EXISTS meko_system.idx_collection_folders_collection RENAME TO idx_project_folders_project;
ALTER INDEX IF EXISTS meko_system.idx_placements_collection        RENAME TO idx_placements_project;

ALTER TABLE meko_system.projects        RENAME CONSTRAINT uq_collections_user_name   TO uq_projects_user_name;
ALTER TABLE meko_system.project_folders RENAME CONSTRAINT uq_collection_folders_name TO uq_project_folders_name;
