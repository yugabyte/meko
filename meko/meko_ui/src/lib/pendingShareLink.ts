// Remembers the share link a signed-out visitor chose to sign in or sign up for, for
// the sign-in paths that lose `?return_to=` (Google, LinkedIn, OIDC land on "/").

import { shareLinkPath } from "@/app/routes/paths";
import { getSessionItem, removeSessionItem, setSessionItem } from "@/lib/browserStorage";

export const PENDING_SHARE_TOKEN_KEY = "meko:pending_share_token";

/** Long enough for sign-up with an emailed code; short enough that an
 *  abandoned attempt can't send a later sign-in in this tab to the link. */
export const PENDING_SHARE_TTL_MS = 30 * 60 * 1000;

/** Saved only when the visitor starts sign-in or sign-up from the link's page. */
export function savePendingShareToken(token: string, now = Date.now()): void {
  setSessionItem(PENDING_SHARE_TOKEN_KEY, JSON.stringify({ token, savedAt: now }));
}

/** The `/share/:token` path to resume, or "" when none is pending or it expired. */
export function pendingSharePath(now = Date.now()): string {
  const raw = getSessionItem(PENDING_SHARE_TOKEN_KEY);
  if (!raw) return "";
  try {
    const { token, savedAt } = JSON.parse(raw) as { token?: string; savedAt?: number };
    if (token && typeof savedAt === "number" && now - savedAt < PENDING_SHARE_TTL_MS) {
      return shareLinkPath(token);
    }
  } catch {
    // Unreadable: treat as none.
  }
  clearPendingShareToken();
  return "";
}

/** Called when the link was joined or found dead, and on sign-out. */
export function clearPendingShareToken(): void {
  removeSessionItem(PENDING_SHARE_TOKEN_KEY);
}
