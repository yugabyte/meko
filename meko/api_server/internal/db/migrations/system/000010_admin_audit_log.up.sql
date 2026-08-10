/*
 * Admin action audit log.
 *
 * Append-only record of privileged admin-console actions, so every change made
 * through the admin API is attributable to a specific operator: "who did what,
 * to whom, and when". The motivating use is user tier changes (free/standard/pro
 * upgrades + downgrades) — without this we can see that a user's tier changed
 * (users.updated_at) but not who changed it or from what. The generic
 * action/target/details shape covers any future admin action (e.g.
 * amp-registration edits) with no schema change.
 *
 * actor_email is the human-readable operator; actor_uuid is their stable id
 * (Cognito sub) and is null for automated sources (Stripe, AWS Marketplace).
 * source records where the change originated. details carries the
 * action-specific payload, e.g. {"old_tier":"free","new_tier":"standard"}.
 *
 * Never on a hot path; written in the same transaction as the mutation it
 * records so the log and the change can't diverge.
 */
CREATE TABLE IF NOT EXISTS meko_system.admin_audit_log (
    id          UUID        DEFAULT gen_random_uuid(),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),

    actor_email TEXT        NOT NULL,   -- operator who performed the action
    actor_uuid  TEXT,                   -- operator's stable id (Cognito sub); null for automated sources
    source      TEXT        NOT NULL DEFAULT 'admin_console', -- admin_console | stripe | aws_marketplace

    action      TEXT        NOT NULL,   -- verb.noun, e.g. 'user.tier.update'
    target_type TEXT        NOT NULL,   -- e.g. 'user'
    target_id   TEXT        NOT NULL,   -- the affected resource id (e.g. user_id)
    details     JSONB       NOT NULL DEFAULT '{}'::jsonb,     -- action-specific payload

    PRIMARY KEY (id HASH)
);

-- History for one resource ("who touched user X"), newest first.
CREATE INDEX IF NOT EXISTS idx_admin_audit_target
    ON meko_system.admin_audit_log (target_type, target_id, created_at DESC);
-- Everything one operator did.
CREATE INDEX IF NOT EXISTS idx_admin_audit_actor
    ON meko_system.admin_audit_log (actor_email, created_at DESC);
-- All events of one action type.
CREATE INDEX IF NOT EXISTS idx_admin_audit_action
    ON meko_system.admin_audit_log (action, created_at DESC);
