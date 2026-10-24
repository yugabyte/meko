import { afterEach, describe, expect, it, vi } from 'vitest';
import { canSignOut, checkSignIn, mekoAuth } from '../../src/web/signin.js';

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
    expect(await checkSignIn()).toBe(false);
    expect(assign).toHaveBeenCalledWith(`/signin?next=${encodeURIComponent('/ext/agentk/p/a b/s/1?x=1')}`);
  });

  it.each([
    ['remote access, still signed in', { data: { access: 'remote', signed_in: true } }],
    ['local access', { data: { access: 'local', signed_in: false } }],
    ['no Meko in front (npm run dev)', 404],
    ['Meko down', 'down'],
  ] as const)('keeps retrying with %s', async (_, reply) => {
    const assign = meko(reply);
    expect(await checkSignIn()).toBe(true);
    expect(assign).not.toHaveBeenCalled();
  });

  it.each([
    [{ data: { access: 'remote', signed_in: true } }, true],
    [{ data: { access: 'remote', signed_in: false } }, false],
    [{ data: { access: 'local', signed_in: false } }, false],
    [404, false],
  ] as const)('offers Sign out only where Meko signed this browser in (%j)', async (reply, offered) => {
    meko(reply);
    expect(canSignOut(await mekoAuth())).toBe(offered);
  });
});
