/*
Shared datapacks in the datapack list start from the recipient's accepted
invites. datapack_invites had no index on recipient_user_id, so every list read
the whole table.

Alone in its file for the same reason as 000021.
*/
CREATE INDEX IF NOT EXISTS idx_invites_recipient_status
    ON meko_system.datapack_invites (recipient_user_id HASH, status ASC, datapack_id ASC);
