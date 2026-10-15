/*
 * Forgot-password requests, one row per request (MEKO-887).
 *
 * Two jobs, both keyed by the lowercased email:
 *
 *   - A per-email limit on POST /auth/forgot-password. The service counts this
 *     email's rows in the last hour before calling Cognito. The shared per-IP
 *     limiter alone lets one caller flood a single inbox from many addresses.
 *
 *   - Reset codes Meko issues itself, for an invitee who never finished first
 *     sign-in. Cognito refuses ForgotPassword while a user is in
 *     FORCE_CHANGE_PASSWORD, so for that user the service emails its own code
 *     and stores only its SHA-256 hash here. The password changes only when the
 *     code is presented (OWASP Forgot Password Cheat Sheet): code_hash,
 *     expires_at, used_at and attempts make the code short-lived, single-use
 *     and attempt-limited. The service reserves each guess with one
 *     conditional UPDATE on attempts, so concurrent guesses can't exceed the
 *     limit. code_hash is NULL for a request Cognito handled. Rows that never
 *     finish (rate-limited, unexpected Cognito status, attach failure) are
 *     deleted so they neither count toward the limit nor hide a live Meko code.
 *
 * PK ((email) HASH, id ASC): every per-email read and write stays on one
 * hash partition. The requested_at secondary index makes each insert and
 * delete a distributed write, and the hourly prune deletes across emails.
 *
 * Rows older than a day are deleted: the requesting email's on every new
 * request, and every email's at most once an hour per api_server pod. The
 * requested_at index keeps that global delete a range scan instead of a full
 * table scan.
 *
 * Additive and read by no earlier release, so it is safe to apply while old
 * pods serve. Like every new migration file it raises the downgrade floor.
 */
CREATE TABLE IF NOT EXISTS meko_system.password_reset_requests (
    id               UUID NOT NULL DEFAULT gen_random_uuid(),
    email            VARCHAR(320) NOT NULL,
    requested_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    code_hash        VARCHAR(128),
    expires_at       TIMESTAMPTZ,
    used_at          TIMESTAMPTZ,
    attempts         INT NOT NULL DEFAULT 0,
    PRIMARY KEY ((email) HASH, id ASC)
);

CREATE INDEX IF NOT EXISTS password_reset_requests_requested_at_idx
    ON meko_system.password_reset_requests (requested_at ASC);
