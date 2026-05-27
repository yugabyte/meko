/*
 * MCP Personal Access Tokens (PATs).
 *
 * Users mint long-lived bearer tokens from the meko UI and paste them
 * into Cursor / Claude Desktop / any MCP client. The MCP server
 * validates each request by SHA-256-hashing the bearer and looking it
 * up here.
 *
 * PK design: ((user_id) HASH, id ASC).
 *   - List-by-user is a single-partition lookup.
 *   - Revoke is `WHERE id = $1 AND user_id = $2` -- exact PK hit, also
 *     prevents cross-user revocation since the partition key is the
 *     user_id.
 *
 * Secondary index on token_hash is the MCP server's hot path: one
 * unique-index lookup per request returns the row including user_id
 * and expires_at, so no further reads are needed.
 *
 * No `name` index: per-user filters are bounded by the 10-token cap
 * (enforced in the service layer), so a 10-row in-partition scan is
 * fine. App-level uniqueness check covers duplicate names.
 */
CREATE TABLE IF NOT EXISTS meko_system.mcp_personal_access_tokens (
    id            UUID NOT NULL DEFAULT gen_random_uuid(),
    user_id       UUID NOT NULL,                        -- meko_system.users(user_id)
    name          VARCHAR(100) NOT NULL,
    token_prefix  VARCHAR(20) NOT NULL,                 -- e.g. "mko_tkn_3KvL9X8z"
    token_hash    VARCHAR(128) NOT NULL,                -- SHA-256 hex of the plaintext token
    -- Permission set the token was minted with. Service layer enforces an
    -- allowlist (currently {"*", "read", "write"}). Default is full access
    -- so existing client code that does not specify scopes keeps working.
    scopes        TEXT[] NOT NULL DEFAULT ARRAY['*']::TEXT[],
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_used_at  TIMESTAMPTZ,
    -- expires_at is mandatory: every PAT must have a finite TTL so a leak
    -- has a hard upper bound. Service layer caps at MaxTTLDays (365).
    expires_at    TIMESTAMPTZ NOT NULL,
    revoked_at    TIMESTAMPTZ,
    PRIMARY KEY ((user_id) HASH, id ASC)
) SPLIT INTO 4 TABLETS;

CREATE UNIQUE INDEX IF NOT EXISTS mcp_pats_token_hash_idx
    ON meko_system.mcp_personal_access_tokens (token_hash);


/*
 * Append-only audit log for MCP PATs.
 *
 * Captures every create / revoke action from the api_server, plus
 * (eventually, from the MCP server) rate-limited events when a
 * revoked or expired token is presented. Used for incident response
 * ("how did this token leak?") and compliance.
 *
 * No FK on token_id: audit rows must outlive any future hard-delete
 * of the PATs table.
 *
 * PK design: ((token_id) HASH, created_at DESC, id ASC).
 *   - Primary forensic query is "everything that happened to this
 *     token, newest first" -- single-partition, naturally time-sorted.
 *   - If we later need a per-user view, add a secondary index on
 *     (user_id, created_at) then. Not needed today.
 */
CREATE TABLE IF NOT EXISTS meko_system.mcp_pat_audit_log (
    id                UUID NOT NULL DEFAULT gen_random_uuid(),
    token_id          UUID NOT NULL,
    user_id           UUID NOT NULL,                        -- denormalized owner
    action            VARCHAR(32) NOT NULL,                 -- created | revoked | expired_used | revoked_used
    actor_id          UUID NOT NULL,                        -- == user_id for self-service today
    actor_type        VARCHAR(16) NOT NULL DEFAULT 'user',  -- user | admin | system
    actor_ip          INET,                                 -- best-effort caller IP (XFF first hop, then RemoteAddr)
    actor_app_client  VARCHAR(500),                         -- broader than HTTP User-Agent: browser UA on user-driven rows, tool identifier (e.g. "Cursor/0.42.5 (mcp-client)") on validator-driven rows
    metadata          JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY ((token_id) HASH, created_at DESC, id ASC)
) SPLIT INTO 4 TABLETS;
