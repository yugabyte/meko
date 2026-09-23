/*
 * Collections and folders: a user-owned tree that organises datapacks.
 *
 * meko_system.datapacks is deliberately NOT touched: placement lives in its
 * own table so an existing datapack needs no backfill and no new column.
 *
 * Ownership is recorded in exactly ONE place, collections.user_id. A folder
 * inherits it through collection_id, and a placement through the same. Neither
 * carries a user of its own, because a second copy of that fact could only
 * ever agree with the first or be a bug.
 *
 * Sharing does not duplicate any of this. A recipient sees the OWNER's collection
 * read-only under "Shared collections", scoped to the datapacks they actually
 * have access to, rather than getting a copy in a tree of their own.
 *
 * Two consequences worth knowing:
 *   - No backfill. "Unfiled" is the absence of a placement row, so this
 *     migration ships empty and every existing datapack keeps behaving
 *     exactly as it does today until a user files it.
 *   - Turning the feature off leaves these tables unread rather than
 *     stranding data in datapacks.
 */

CREATE TABLE IF NOT EXISTS meko_system.collections (
    collection_id      UUID        DEFAULT gen_random_uuid(),
    user_id            UUID        NOT NULL,   -- owner of this tree
    collection_name    VARCHAR     NOT NULL,
    -- Theme is STAMPED, not linked: create copies the theme's folders into
    -- collection_folders and nothing reads the theme again. These two columns are
    -- provenance only, so a collection never drifts "out of sync" with a theme.
    theme_key          VARCHAR     NOT NULL,
    theme_version      VARCHAR     NOT NULL,
    collection_details JSONB,      -- description/summary, mirrors datapack_details
    created_at         TIMESTAMPTZ DEFAULT NOW(),
    updated_at         TIMESTAMPTZ DEFAULT NOW(),
    PRIMARY KEY (collection_id HASH),
    CONSTRAINT uq_collections_user_name UNIQUE (user_id, collection_name)
) SPLIT INTO 12 TABLETS;

-- Covering index for "list my collections": returns the name straight from the
-- index. Same shape as idx_datapacks_user_id, and UNIQUE for the same reason
-- ((user_id, collection_id) is one-to-one).
CREATE UNIQUE INDEX IF NOT EXISTS idx_collections_user_id
    ON meko_system.collections (user_id HASH, collection_id ASC) INCLUDE (collection_name);


/*
 * Folders inside a collection. ONE level: a collection holds folders, and folders
 * hold datapacks. Folders do not nest.
 *
 * That is the whole structure the product needs, and keeping it flat is why
 * a collection's tree is one index scan with no recursion, and why a path is
 * always exactly "collection/folder".
 */
CREATE TABLE IF NOT EXISTS meko_system.collection_folders (
    folder_id     UUID        DEFAULT gen_random_uuid(),
    collection_id UUID        NOT NULL
                              REFERENCES meko_system.collections(collection_id) ON DELETE CASCADE,
    folder_name   VARCHAR     NOT NULL,
    created_at    TIMESTAMPTZ DEFAULT NOW(),
    updated_at    TIMESTAMPTZ DEFAULT NOW(),
    PRIMARY KEY (folder_id HASH),
    -- Folder names are unique within their collection. A plain constraint, not the
    -- partial-index pair a nested tree would have needed.
    CONSTRAINT uq_collection_folders_name UNIQUE (collection_id, folder_name)
) SPLIT INTO 12 TABLETS;

-- One scan returns a collection's whole folder list, covering the name so the
-- read never touches the main table.
CREATE INDEX IF NOT EXISTS idx_collection_folders_collection
    ON meko_system.collection_folders (collection_id HASH, folder_id ASC)
    INCLUDE (folder_name);


/*
 * Where a datapack sits. EXACTLY ONE row per datapack: a datapack has a single
 * home, in a collection owned by the datapack's owner.
 *
 * Sharing does not copy this. A recipient sees the OWNER's collection, read-only,
 * under "Shared collections", scoped to the datapacks they actually have access
 * to. That is why there is no user_id here: the owner is already known from
 * collections.user_id, and a per-user placement would let the same datapack sit
 * in two different trees at once.
 *
 * collection_id is denormalised from the folder so a collection-wide rollup is one
 * index scan rather than a join through collection_folders.
 */
CREATE TABLE IF NOT EXISTS meko_system.datapack_placements (
    datapack_id   UUID        NOT NULL
                              REFERENCES meko_system.datapacks(datapack_id) ON DELETE CASCADE,
    collection_id UUID        NOT NULL
                              REFERENCES meko_system.collections(collection_id) ON DELETE CASCADE,
    folder_id     UUID        NOT NULL
                              REFERENCES meko_system.collection_folders(folder_id) ON DELETE CASCADE,
    placed_at     TIMESTAMPTZ DEFAULT NOW(),
    updated_at    TIMESTAMPTZ DEFAULT NOW(),
    PRIMARY KEY (datapack_id HASH)
) SPLIT INTO 12 TABLETS;

-- Contents of one folder.
CREATE INDEX IF NOT EXISTS idx_placements_folder
    ON meko_system.datapack_placements (folder_id HASH, datapack_id ASC);

-- Per-folder datapack counts for a whole collection in one scan. This is the
-- index behind the Collections page's counts: GROUP BY folder_id over a
-- collection_id prefix, no fan-out to the per-datapack databases.
CREATE INDEX IF NOT EXISTS idx_placements_collection
    ON meko_system.datapack_placements (collection_id HASH, folder_id ASC, datapack_id ASC);
