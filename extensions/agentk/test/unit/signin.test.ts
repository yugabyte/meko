import { afterEach, describe, expect, it, vi } from 'vitest';
import { checkSignIn, mekoAuth, SIGN_IN_CHECK_MS } from '../../src/web/signin.js';

/** Each check a while after the last, so none is answered by an earlier one (checkSignIn's throttle). */
let clock = 0;
const later = () => (clock += 10 * SIGN_IN_CHECK_MS);

/** Meko's answer to GET /api/v1/auth/status: its envelope, or a status with no body, or no answer at all. */
function meko(reply: { data: Record<string, unknown> } | number | 'down') {
  const assign = vi.fn();
  vi.stubGlobal('location', { pathname: '/ext/agentk/p/a b/s/1', search: '?x=1', assign });
  vi.stubGlobal(
    'fetch',
    vi.fn(async (url: string) => {
      expect(url).toBe('/api/v1/auth/status');
      if (reply === 'down') throw new TypeError('Failed to fetch');
      if (typeof reply === 'number') return new Response('', { status: reply });
      return Response.json(reply);
    }),
  );
  return assign;
}

describe('signing in through Meko', () => {
  afterEach(() => vi.unstubAllGlobals());

  it('goes to Meko’s sign-in screen, back to this page after, once remote access has no sign-in', async () => {
    const assign = meko({ data: { access: 'remote', signed_in: false, username: null } });
    expect(await checkSignIn(later())).toBe(false);
    expect(assign).toHaveBeenCalledWith(`/signin?next=${encodeURIComponent('/ext/agentk/p/a b/s/1?x=1')}`);
  });

  it.each([
    ['remote access, still signed in', { data: { access: 'remote', signed_in: true } }],
    ['local access', { data: { access: 'local', signed_in: false } }],
    ['no Meko in front (npm run dev)', 404],
    ['Meko down', 'down'],
  ] as const)('keeps retrying with %s', async (_, reply) => {
    const assign = meko(reply);
    expect(await checkSignIn(later())).toBe(true);
    expect(assign).not.toHaveBeenCalled();
  });

  it('asks Meko at most every few seconds while it says this browser is signed in', async () => {
    meko({ data: { access: 'remote', signed_in: true } });
    const t = later();
    expect(await checkSignIn(t)).toBe(true);
    expect(await checkSignIn(t + SIGN_IN_CHECK_MS - 1)).toBe(true);
    expect(fetch).toHaveBeenCalledTimes(1);
    expect(await checkSignIn(t + SIGN_IN_CHECK_MS)).toBe(true);
    expect(fetch).toHaveBeenCalledTimes(2);
  });

  it.each([
    [{ data: { access: 'remote', signed_in: true } }, { access: 'remote', signedIn: true }],
    [{ data: { access: 'local', signed_in: false } }, { access: 'local', signedIn: false }],
    [{ data: { access: 'other' } }, null],
    [404, null],
  ] as const)('reads what Meko says about this browser (%j)', async (reply, auth) => {
    meko(reply);
    expect(await mekoAuth()).toEqual(auth);
  });
});
