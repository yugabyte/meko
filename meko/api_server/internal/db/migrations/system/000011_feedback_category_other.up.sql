/*
 * Converge meko_system.feedback_category on the capitalised 'Other'
 * label used by the FE and the api_server validator.
 *
 * Environments diverged after PR #193 (commit 7a5f3698a5, 2026-05-07)
 * edited the enum literal in the original 000002_meko_feedback
 * migration from 'Other' to 'other' in place. Long-lived environments
 * (dev, prod) that ran the migration before the edit still carry
 * 'Other'; CI, fresh clusters, and any dev box that ran migrate up
 * against the current .up.sql carry 'other'. The FE ships 'Other';
 * this migration makes the DB agree wherever it doesn't yet.
 *
 * Guarded on pg_enum so it's idempotent — the RENAME only runs on
 * environments that still have 'other' (fresh installs / CI). On dev
 * and prod, which already have 'Other', the block short-circuits.
 * ALTER TYPE ... RENAME VALUE carries existing rows through
 * automatically, so no data touch-up is needed.
 */

DO $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM   pg_enum        e
        JOIN   pg_type        t ON t.oid = e.enumtypid
        JOIN   pg_namespace   n ON n.oid = t.typnamespace
        WHERE  n.nspname   = 'meko_system'
          AND  t.typname   = 'feedback_category'
          AND  e.enumlabel = 'other'
    ) THEN
        ALTER TYPE meko_system.feedback_category RENAME VALUE 'other' TO 'Other';
    END IF;
END
$$;
