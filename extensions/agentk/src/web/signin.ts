/**
 * Signing in is Meko's: it serves AgentK under BASE and decides who gets in. These call Meko's own
 * routes, root-absolute rather than under BASE. Under `npm run dev` there's no Meko, so they fail:
 * then there's no Sign out, and a refused request is shown as an error and retried as before.
 */

/** What Meko says about this browser (`GET /api/v1/auth/status`). */
export interface MekoAuth {
  /** `remote`: Meko takes sign-ins from other devices; `local`: it opens on this computer with none. */
  access: 'local' | 'remote';
  signedIn: boolean;
}

/** Close codes a WebSocket gets when Meko refused it (no sign-in, or one that has gone). */
export const WS_UNAUTHORIZED: readonly number[] = [4401, 1008];

/** Meko's sign-in state, or null when Meko can't say (not running behind it). */
export async function mekoAuth(): Promise<MekoAuth | null> {
  try {
    const res = await fetch('/api/v1/auth/status', { credentials: 'same-origin' });
    if (!res.ok) return null;
    const body = (await res.json()) as { data?: { access?: unknown; signed_in?: unknown } } | null;
    const access = body?.data?.access;
    if (access !== 'local' && access !== 'remote') return null;
    return { access, signedIn: body?.data?.signed_in === true };
  } catch {
    return null;
  }
}

/** Sign out is offered only where Meko signed this browser in. */
export function canSignOut(auth: MekoAuth | null): boolean {
  return auth?.access === 'remote' && auth.signedIn;
}

/** Signs out of Meko (and so AgentK), then shows Meko's sign-in screen. */
export async function signOut(): Promise<void> {
  try {
    await fetch('/api/v1/auth/logout', {
      method: 'POST',
      credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: '{}',
    });
  } catch {
    // The sign-in screen still shows whether the session is gone.
  }
  location.assign('/signin');
}

/** Meko's sign-in screen, coming back to `page` (a path and query) after. */
export function signInUrl(page: string): string {
  return `/signin?next=${encodeURIComponent(page)}`;
}

let checking: Promise<boolean> | null = null;

/**
 * After AgentK's API answered 401, or a WebSocket closed with a WS_UNAUTHORIZED code: Meko refused
 * the request. Where Meko signs people in and this browser isn't signed in any more, goes to its
 * sign-in screen (back to this page after) and resolves false. Else resolves true, so the caller
 * keeps retrying.
 */
export function checkSignIn(): Promise<boolean> {
  checking ??= mekoAuth()
    .then((auth) => {
      if (auth?.access !== 'remote' || auth.signedIn) return true;
      location.assign(signInUrl(location.pathname + location.search));
      return false;
    })
    .finally(() => {
      checking = null;
    });
  return checking;
}
