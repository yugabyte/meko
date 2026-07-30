/*
 * Artifact store for agent-generated files.
 *
 * Content-addressed blob store scoped to a datapack. Each artifact is
 * identified by the SHA-256 hex digest of its bytes, making duplicate
 * uploads idempotent — inserting the same bytes twice returns the
 * existing row without writing a new one.
 *
 * Storage tier:
 *   size_bytes < 1 MB  → inline_content (BYTEA), s3_key NULL
 *   size_bytes >= 1 MB → s3_key set, inline_content NULL
 *
 * Expiry (soft-delete only — no hard deletes):
 *   expired_at is set by the GC job when last_accessed_at falls
 *   outside the tier TTL (free: 30 days; pro: never). Expired artifacts
 *   return 410 from artifact_get but remain recoverable by an admin.
 */
CREATE TABLE IF NOT EXISTS datapacks.artifacts (
    id               UUID        NOT NULL DEFAULT gen_random_uuid(),
    datapack_id      UUID        NOT NULL,
    content_hash     TEXT        NOT NULL,  -- SHA-256 hex of file bytes
    filename         TEXT        NOT NULL,
    content_type     TEXT        NOT NULL,
    size_bytes       BIGINT      NOT NULL,
    inline_content   BYTEA,                 -- set when size_bytes < 1 MB
    s3_key           TEXT,                  -- set when size_bytes >= 1 MB
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_accessed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expired_at       TIMESTAMPTZ,           -- NULL = live; non-NULL = soft-expired
    PRIMARY KEY (datapack_id HASH, content_hash ASC)
) SPLIT INTO 12 TABLETS;

CREATE INDEX IF NOT EXISTS idx_artifacts_datapack_expired
    ON datapacks.artifacts (datapack_id, expired_at, last_accessed_at);
