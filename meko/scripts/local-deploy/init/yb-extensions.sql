-- Bootstrap extensions on the local yugabyted instance.
-- Run against the default `yugabyte` database before any meko service
-- attempts to use them. mcp-server's mem0 client also calls
-- CREATE EXTENSION at runtime, but doing it up-front avoids races and
-- surfaces missing-extension errors at deploy time instead of first-use.

CREATE EXTENSION IF NOT EXISTS vector;
CREATE EXTENSION IF NOT EXISTS mage;
CREATE EXTENSION IF NOT EXISTS pg_dist_rag;
