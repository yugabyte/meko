-- The owner's login for the web listener with remote access, and its browser sessions. It has
-- AgentK's shape (its owner and auth_sessions tables), so the login Meko imports from AgentK is a
-- straight copy. The outstanding setup link and the last TOTP step accepted are in settings.
-- Migrations are append-only: never edit one that has shipped.

-- The one owner. pw_scrypt is scrypt$N$r$p$salt$hash; totp_pending is a secret being enrolled.
CREATE TABLE owner (
  id           INTEGER PRIMARY KEY CHECK (id = 1),
  username     TEXT NOT NULL,
  pw_scrypt    TEXT NOT NULL,
  totp_secret  TEXT,
  totp_pending TEXT
) STRICT;

-- A browser signed in: only the cookie's SHA-256 is kept.
CREATE TABLE auth_sessions (
  token_sha256 TEXT PRIMARY KEY,
  created_at   TEXT NOT NULL,            -- RFC 3339, UTC
  expires_at   TEXT NOT NULL,
  user_agent   TEXT NOT NULL DEFAULT '',
  ip           TEXT NOT NULL DEFAULT ''
) STRICT;
