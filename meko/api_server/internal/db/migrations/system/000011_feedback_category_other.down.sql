/*
 * Intentionally a no-op.
 *
 * The up-migration renames 'other' → 'Other' where the lowercase label
 * still exists (fresh installs / CI). A symmetric down would rename
 * 'Other' → 'other' unconditionally — but 'Other' is exactly what
 * dev / prod already had before this migration existed, so running the
 * down against those environments would rewrite their enum back to
 * lowercase and re-introduce the bug this PR fixes.
 *
 * The enum's current state carries no signal that distinguishes
 *   "the up-migration renamed 'other' to 'Other' here"
 * from
 *   "'Other' was always here; the up-migration was a skip".
 * Any guard we can write on pg_enum alone triggers in both cases, so a
 * conditional revert is unsafe on the environments most likely to have
 * a rollback run against them.
 *
 * Migrator round-trip tooling accepts a clean no-op down, so leave the
 * file empty of effect. If a genuine revert is ever needed on a fresh
 * install, do it by hand:
 *
 *   ALTER TYPE meko_system.feedback_category RENAME VALUE 'Other' TO 'other';
 */
SELECT 1 WHERE FALSE;
