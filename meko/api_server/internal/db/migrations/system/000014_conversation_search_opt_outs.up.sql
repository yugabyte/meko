/*
 * Per-datapack opt-out from the conversation-history search cache.
 *
 * Conversation-search embedding is enabled by default for every datapack
 * (subject to the deployment-wide MEKO_ENABLE_CONVERSATION_SEARCH flag) —
 * a datapack owner can only opt OUT, never opt in explicitly. Presence of a
 * row here means "do not embed this datapack's future turns"; absence means
 * enabled. This keeps the common case (no row) free of a default-value
 * migration whenever a new datapack is created.
 */
CREATE TABLE IF NOT EXISTS meko_system.conversation_search_opt_outs (
    datapack_id  UUID        NOT NULL REFERENCES meko_system.datapacks(datapack_id) ON DELETE CASCADE,
    opted_out_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    opted_out_by TEXT,
    PRIMARY KEY (datapack_id HASH)
);
