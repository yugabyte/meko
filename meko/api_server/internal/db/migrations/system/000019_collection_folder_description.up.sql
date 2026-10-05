/*
 * A folder gets a description.
 *
 * Nullable and additive: every existing folder stays valid with nothing
 * written, so this needs no backfill and is safe to apply ahead of the code
 * that reads it.
 *
 * Worth the column rather than another key in a JSONB blob because search is
 * the point. Once a collection holds enough folders that its tree does not
 * fit on a screen, "which folder do I put this in" is answered by what the
 * folder is FOR, not by its name, and a plain column is what a later
 * knowledge or memory search can filter and rank on.
 */

ALTER TABLE meko_system.collection_folders
    ADD COLUMN IF NOT EXISTS folder_description VARCHAR;
