/**
 * Who gets in is Meko's call: it serves AgentK under BASE. A Meko from before it dropped its login
 * signs people in, with remote access; a newer one has no login, and always says signed in. These
 * call Meko's own routes, root-absolute rather than under BASE. Under `npm run dev` there's no Meko,
 * so they fail: then a refused request is shown as an error and retried as before. Signing out, on
 * a Meko with a login, is on its account page (Settings › General links to it), not in AgentK.
 */

/** What Meko says about this browser (`GET /api/v1/auth/status`). */
export interface MekoAuth {
  /** `remote`: Meko takes sign-ins from other devices; `local`: it opens on this computer with none. */
  access: 'local' | 'remote';
  signedIn: boolean;
  /** The login's username: only a Meko from before it dropped its login has one; else null. */
  username: string | null;
}

/** Close codes a WebSocket gets when it was refused (no sign-in, or another site's page): it waits for checkSignIn before retrying. */
export const WS_UNAUTHORIZED: readonly number[] = [4401, 1008];

/**
 * The close code the browser reports when Meko refuses an upgrade (401) or closes a signed-out
 * session's sockets, and also for a dropped network: the socket retries as usual, and checks the
 * sign-in alongside (throttled), which goes to Meko's sign-in screen only when it's gone.
 */
export const WS_ABNORMAL = 1006;

/** Meko's sign-in state, or null when Meko can't say (not running behind it). */
export async function mekoAuth(): Promise<MekoAuth | null> {
  try {
    const res = await fetch('/api/v1/auth/status', { credentials: 'same-origin' });
    if (!res.ok) return null;
    const body = (await res.json()) as { data?: { access?: unknown; signed_in?: unknown; username?: unknown } } | null;
    const access = body?.data?.access;
    if (access !== 'local' && access !== 'remote') return null;
    const username = body?.data?.username;
    return { access, signedIn: body?.data?.signed_in === true, username: typeof username === 'string' && username ? username : null };
  } catch {
    return null;
  }
}

/** Meko's sign-in screen, coming back to `page` (a path and query) after. */
export function signInUrl(page: string): string {
  return `/signin?next=${encodeURIComponent(page)}`;
}

let checking: Promise<boolean> | null = null;
let checkedAt = 0;
/** A check that found this browser signed in stands this long, so a socket that keeps closing (a dropped network) asks at most this often. */
export const SIGN_IN_CHECK_MS = 5000;

/**
 * After AgentK's API answered 401, or a WebSocket closed with a WS_UNAUTHORIZED code: Meko may have
 * refused the request. Where Meko has a login and this browser isn't signed in any more, goes
 * to its sign-in screen (back to this page after) and resolves false. Else resolves true, so the
 * caller keeps retrying.
 */
export function checkSignIn(now: number = Date.now()): Promise<boolean> {
  if (!checking && now - checkedAt < SIGN_IN_CHECK_MS) return Promise.resolve(true);
  checking ??= mekoAuth()
    .then((auth) => {
      if (auth?.access !== 'remote' || auth.signedIn) {
        checkedAt = now;
        return true;
      }
      location.assign(signInUrl(location.pathname + location.search));
      return false;
    })
    .finally(() => {
      checking = null;
    });
  return checking;
}
