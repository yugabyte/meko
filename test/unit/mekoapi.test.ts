import { describe, expect, it } from 'vitest';
import { MekoApi, MekoApiError, looksLikeMekoApi } from '../../src/server/mekoapi.js';
import { fakeMekoApi } from '../helpers.js';

const ok = (data: unknown) => ({ status: 200, body: { data } });

describe('MekoApi', () => {
  it('sends the key as a bearer token and JSON bodies to the share routes', async () => {
    const f = fakeMekoApi();
    f.handle(() => ok({ status: 'pending', invite_id: 'inv-1' }));
    await f.api.share('dp 1', 'bob@example.com', 'contributor');
    await f.api.resend('dp 1', 'inv-1');
    await f.api.revoke('dp 1', 'inv-1');
    await f.api.inviteAction('inv-2', 'accept', 'u-self');
    await f.api.inviteAction('inv-3', 'decline', null);
    expect(f.calls.map((c) => [c.method, c.path, c.body])).toEqual([
      ['POST', '/datapacks/dp%201/share', { recipient_email: 'bob@example.com', grant: 'contributor' }],
      ['POST', '/datapacks/dp%201/invite/inv-1/resend', undefined],
      ['DELETE', '/datapacks/dp%201/invite/inv-1', undefined],
      ['POST', '/datapacks/invite-action', { invite_id: 'inv-2', user_action: 'accept', recipient_user_id: 'u-self' }],
      ['POST', '/datapacks/invite-action', { invite_id: 'inv-3', user_action: 'decline' }],
    ]);
    expect(f.calls.every((c) => c.auth === 'Bearer test-key')).toBe(true);
  });

  it("passes on Meko's error message", async () => {
    const f = fakeMekoApi();
    f.handle(() => ({ status: 409, body: { error: { code: 'CONFLICT', message: 'bob@example.com is already a member' } } }));
    const e = await f.api.share('dp1', 'bob@example.com', 'viewer').catch((x: unknown) => x);
    expect(e).toBeInstanceOf(MekoApiError);
    expect(e).toMatchObject({ status: 409, code: 'CONFLICT', message: 'bob@example.com is already a member' });
  });

  it('passes on a refused key as an error', async () => {
    const f = fakeMekoApi();
    await expect(f.api.incomingShares()).rejects.toMatchObject({ status: 401, code: 'UNAUTHORIZED', message: 'invalid or expired token' });
    f.handle(() => ok([]));
    expect(await f.api.incomingShares()).toEqual([]);
  });

  it('says so when asked to share with no API URL', async () => {
    const api = new MekoApi(() => ({ baseUrl: null, apiKey: 'k' }));
    await expect(api.share('d', 'a@b.c', 'viewer')).rejects.toThrow(/API URL/);
  });

  it('reports an unreachable Meko and a timeout', async () => {
    const refused = new MekoApi(() => ({ baseUrl: 'https://meko.test/api/v1', apiKey: 'k' }), (async () => {
      throw new TypeError('fetch failed');
    }) as typeof fetch);
    await expect(refused.incomingShares()).rejects.toThrow(/Couldn't reach Meko's API at https:\/\/meko.test\/api\/v1: fetch failed/);
    const slow = new MekoApi(() => ({ baseUrl: 'https://meko.test/api/v1', apiKey: 'k' }), (async () => {
      throw Object.assign(new Error('aborted'), { name: 'TimeoutError' });
    }) as typeof fetch);
    await expect(slow.incomingShares()).rejects.toThrow(/timed out/);
  });

  it('parses incoming shares and drops incomplete rows', async () => {
    const f = fakeMekoApi();
    f.handle(() =>
      ok([
        { invite_id: 'i1', owner_user_id: 'u1', owner_name: 'Alice Ng', owner_email: 'alice@example.com', grant: 'Contributor', datapack_id: 'd1', datapack_name: 'adlc/proj/a/b' },
        { invite_id: '', datapack_id: 'd2', datapack_name: 'x' },
      ]),
    );
    expect(await f.api.incomingShares()).toEqual([
      { inviteId: 'i1', datapackId: 'd1', datapackName: 'adlc/proj/a/b', grant: 'contributor', ownerName: 'Alice Ng', ownerEmail: 'alice@example.com' },
    ]);
  });

  it('lists members (owner first) and pending invites, which contributors may not see', async () => {
    const f = fakeMekoApi();
    let forbid = false;
    f.handle((c) => {
      if (c.path.endsWith('/member-list')) {
        return ok({
          members: [
            { user_id: 'u2', is_owner: false, user_name: 'Carol', user_email_id: 'carol@example.com', grant: 'viewer' },
            { user_id: 'u1', is_owner: true, user_name: 'Alice', user_email_id: 'alice@example.com', grant: 'maintainer' },
          ],
        });
      }
      if (forbid) return { status: 403, body: { error: { code: 'FORBIDDEN', message: 'owner or maintainer only' } } };
      return ok({ pending_invites: [{ invite_id: 'i9', grant: 'contributor', recipient_email_id: 'dan@example.com' }] });
    });
    expect(await f.api.people('d1')).toEqual({
      members: [
        { name: 'Alice', email: 'alice@example.com', grant: 'owner' },
        { name: 'Carol', email: 'carol@example.com', grant: 'viewer' },
      ],
      invites: [{ inviteId: 'i9', email: 'dan@example.com', grant: 'contributor' }],
    });
    forbid = true;
    expect((await f.api.people('d1')).invites).toEqual([]);
  });

  it("recognises Meko's API by its envelope, asking without the key", async () => {
    const seen: { url: string; auth: unknown }[] = [];
    const answer = (status: number, body: string) => (async (url: string | URL | Request, init?: RequestInit) => {
      seen.push({ url: String(url), auth: (init?.headers as Record<string, string> | undefined)?.Authorization });
      return new Response(body, { status });
    }) as typeof fetch;
    expect(await looksLikeMekoApi('https://meko.test/api/v1/', answer(401, JSON.stringify({ error: { code: 'UNAUTHORIZED', message: 'no token' } })))).toEqual({ ok: true });
    expect(seen).toEqual([{ url: 'https://meko.test/api/v1/datapacks/incoming-shares', auth: undefined }]);
    expect(await looksLikeMekoApi('https://x.test', answer(404, '<html>not found</html>'))).toEqual({ ok: false, error: "it answered 404, not like Meko's API" });
    // Plenty of APIs answer `{ "error": "…" }`; Meko's error is an object with a code.
    expect(await looksLikeMekoApi('https://x.test', answer(404, JSON.stringify({ error: 'Not Found' })))).toEqual({ ok: false, error: "it answered 404, not like Meko's API" });
    const down = (async () => {
      throw new TypeError('fetch failed');
    }) as typeof fetch;
    expect(await looksLikeMekoApi('https://x.test', down)).toEqual({ ok: false, error: "couldn't reach it (fetch failed)" });
  });
});
