/*
 * Link sharing: one shareable URL per datapack. Anyone signed in who opens it
 * while it is active joins the datapack with grant_role (viewer or
 * contributor). The token stays the same until an owner or maintainer resets
 * it; turning the link off stops new joins and leaves existing members alone.
 *
 * A join writes an accepted datapack_invites row (source = 'link'), the same
 * row every grant lookup already reads, so nothing that resolves a member's
 * role needs to know links exist.
 */
CREATE TABLE IF NOT EXISTS meko_system.datapack_share_links (
    datapack_id        UUID        NOT NULL REFERENCES meko_system.datapacks(datapack_id) ON DELETE CASCADE,
    token              VARCHAR(64) NOT NULL,
    grant_role         VARCHAR     NOT NULL DEFAULT 'viewer',  -- viewer | contributor
    status             VARCHAR     NOT NULL DEFAULT 'active',  -- active | off
    expires_at         TIMESTAMPTZ,                            -- NULL = never; not yet settable
    created_by_user_id UUID        NOT NULL,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (datapack_id HASH)
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_share_links_token
    ON meko_system.datapack_share_links (token);

-- Where an accepted member's role came from: 'email' (invite), 'link' (moves
-- with the link's role), or 'link_override' (a link member whose role an owner
-- then set by hand, which the link no longer changes). A revoked row marked
-- 'removed' is a member an owner removed, whom the link won't let back in.
-- Nullable so rows an older build inserts without it read as 'email'.
ALTER TABLE meko_system.datapack_invites
    ADD COLUMN IF NOT EXISTS source VARCHAR DEFAULT 'email';
