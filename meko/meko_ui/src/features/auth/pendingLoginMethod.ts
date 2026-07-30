// Carries the chosen auth method across a redirect-based sign-in. Google OAuth
// leaves the SPA, so we don't know the user's identity at click time — the
// button stashes the method here, and useAuthBootstrap consumes it once the
// session is verified on the post-redirect boot, firing `user_signed_in` then
// with the real email + user_id (and never recording a cancelled sign-in).
import { getLocalItem, removeLocalItem, setLocalItem } from "@/lib/browserStorage";

const PENDING_LOGIN_METHOD_KEY = "meko:pending-login-method";

export function setPendingLoginMethod(method: string): void {
  setLocalItem(PENDING_LOGIN_METHOD_KEY, method);
}

export function clearPendingLoginMethod(): void {
  removeLocalItem(PENDING_LOGIN_METHOD_KEY);
}

/** Read and clear in one shot (consume-once) so a stale marker can't linger
 *  past the boot that immediately follows the redirect. */
export function consumePendingLoginMethod(): string | null {
  const value = getLocalItem(PENDING_LOGIN_METHOD_KEY);
  if (value != null) removeLocalItem(PENDING_LOGIN_METHOD_KEY);
  return value;
}
