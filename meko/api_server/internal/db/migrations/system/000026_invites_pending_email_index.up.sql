/*
A share-link join looks for a pending invite sent to the joiner's email, matched
case-insensitively because invites store the address as typed. The existing
idx_invites_datapack_email_pending is on the bare column, so lower() can't use
it and the lookup read every pending invite on the datapack, under the link lock.

Alone in its file for the same reason as 000021.
*/
CREATE INDEX IF NOT EXISTS idx_invites_datapack_lower_email_pending
    ON meko_system.datapack_invites (datapack_id HASH, lower(recipient_email) ASC)
    WHERE status = 'pending';
