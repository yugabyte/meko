-- Meko Local's first schema. Migrations are append-only: never edit one that has shipped.

CREATE TABLE settings (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
) STRICT;

CREATE TABLE datapacks (
  id          TEXT PRIMARY KEY,           -- a UUID, as in prod
  name        TEXT NOT NULL UNIQUE,
  description TEXT NOT NULL DEFAULT '',
  created_at  TEXT NOT NULL,              -- RFC 3339, UTC
  updated_at  TEXT NOT NULL
) STRICT;
