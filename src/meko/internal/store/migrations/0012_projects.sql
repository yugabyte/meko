-- Projects: a two-level tree, project then folder, that files this Meko's datapacks, as Meko Cloud's
-- projects do (docs/api.md, "Projects"). A datapack is filed in at most one folder; deleting a
-- project or folder unfiles its datapacks and never deletes them.
-- Migrations are append-only: never edit one that has shipped.

CREATE TABLE projects (
  id          TEXT PRIMARY KEY,                  -- a UUID, as in Meko Cloud
  name        TEXT NOT NULL UNIQUE,
  description TEXT NOT NULL DEFAULT '',
  theme_key   TEXT NOT NULL,                     -- the layout its first folders came from
  created_at  TEXT NOT NULL,                     -- RFC 3339, UTC
  updated_at  TEXT NOT NULL
) STRICT;

CREATE TABLE project_folders (
  id          TEXT PRIMARY KEY,
  project_id  TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  name        TEXT NOT NULL,
  description TEXT NOT NULL DEFAULT '',
  created_at  TEXT NOT NULL,
  updated_at  TEXT NOT NULL,
  UNIQUE (project_id, name)
) STRICT;

-- Where a datapack is filed: one row at most, keyed by the datapack. project_id repeats the folder's
-- so a project's datapacks are one index scan.
CREATE TABLE datapack_placements (
  datapack_id TEXT PRIMARY KEY REFERENCES datapacks(id) ON DELETE CASCADE,
  project_id  TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  folder_id   TEXT NOT NULL REFERENCES project_folders(id) ON DELETE CASCADE,
  placed_at   TEXT NOT NULL,                     -- when it was filed here (a move resets it)
  updated_at  TEXT NOT NULL
) STRICT;
CREATE INDEX datapack_placements_project ON datapack_placements(project_id, folder_id);
