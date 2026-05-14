// Shared constants + helpers for Meko personal access tokens (MCP PATs).
// Centralized here so the Settings UI, the Connect dialog, and the
// installer-command builder all reference the same values instead of
// drifting via inline literals.

import type { MekoToken } from "@/lib/mekoApi";

/**
 * Hard cap on tokens per user. Enforced server-side
 * (`ErrMCPPATCapReached` → 429); the UI mirrors it to disable the
 * "Generate token" CTA before the user submits a guaranteed-fail request.
 */
export const MEKO_TOKEN_MAX_COUNT = 10;

/** Inclusive bounds on the human-readable token name. Tightened from
 *  the server-side validator so users get instant feedback on the form. */
export const MEKO_TOKEN_NAME_MIN_LENGTH = 1;
export const MEKO_TOKEN_NAME_MAX_LENGTH = 64;

/**
 * Default token TTL in days when the UI doesn't expose a custom expiry
 * picker. The server clamps to its own max if this exceeds platform
 * policy; we just pass 365 to give the user a year by default.
 */
export const MEKO_TOKEN_DEFAULT_TTL_DAYS = 365;

/**
 * Placeholder substituted into the npx installer command in place of a
 * real token. The Connect dialog instructs the user to paste their own
 * saved token here; the literal value also doubles as something that's
 * visually obvious in a terminal so it can't be confused with a real key.
 */
export const MEKO_TOKEN_INSTALLER_PLACEHOLDER = "<paste-your-meko-token>" as const;

/** Derived runtime status of a token. */
export const MEKO_TOKEN_STATUS = {
  active: "active",
  expired: "expired",
  revoked: "revoked",
} as const;
export type MekoTokenStatus = (typeof MEKO_TOKEN_STATUS)[keyof typeof MEKO_TOKEN_STATUS];

/**
 * Derive token status from the record's `revoked_at` / `expires_at`
 * fields. Mirrors the server's `IsActive(now)` predicate so the UI can
 * label rows without a separate round-trip.
 */
export function mekoTokenStatus(token: MekoToken, now: Date = new Date()): MekoTokenStatus {
  if (token.revoked_at) return MEKO_TOKEN_STATUS.revoked;
  if (new Date(token.expires_at).getTime() <= now.getTime()) {
    return MEKO_TOKEN_STATUS.expired;
  }
  return MEKO_TOKEN_STATUS.active;
}

/** Convenience: tokens that can still authenticate (used by the Connect dialog). */
export function filterActiveMekoTokens(tokens: MekoToken[], now: Date = new Date()): MekoToken[] {
  return tokens.filter((t) => mekoTokenStatus(t, now) === MEKO_TOKEN_STATUS.active);
}

/** Filesystem-safe filename for the one-time token download. Strips
 *  characters Windows / macOS reject and falls back to `meko-token` when
 *  the name normalizes to empty. */
export function mekoTokenDownloadFilename(name: string): string {
  const safe = name
    .trim()
    .toLowerCase()
    .replace(/[^a-z0-9._-]+/g, "-")
    .replace(/^-+|-+$/g, "");
  return `meko-token-${safe || "token"}.txt`;
}
