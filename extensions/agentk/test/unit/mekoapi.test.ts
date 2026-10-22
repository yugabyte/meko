import { createHash } from 'node:crypto';
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

  it('asks Meko for a presigned upload, posts the file there without the key, and reads how its indexing goes', async () => {
    const f = fakeMekoApi();
    const storage = 'https://bucket.s3.test/';
    f.handle((c) => c.path.endsWith('/upload-url')
      ? ok({ url: storage, fields: { key: 'u/dp/h/notes.md', policy: 'p', 'x-amz-checksum-sha256': 'c' }, key: 'u/dp/h/notes.md', already_uploaded: false })
      : c.path === storage ? { status: 204 }
      : ok({ document_key: 'u/dp/h/notes.md', state: 'Indexing', document_status: 'PROCESSING' }));
    const bytes = Buffer.from('# Hi\n');
    const up = await f.api.requestUpload('dp 1', 'notes.md', bytes);
    expect(up).toEqual({ url: storage, fields: [['key', 'u/dp/h/notes.md'], ['policy', 'p'], ['x-amz-checksum-sha256', 'c']], key: 'u/dp/h/notes.md', alreadyUploaded: false });
    await f.api.postPresigned(up, 'notes.md', bytes);
    expect(await f.api.documentStatus('dp 1', 'u/dp/h/notes.md')).toBe('indexing');
    const [ask, post, status] = f.calls;
    expect([ask.method, ask.path, ask.auth, ask.type]).toEqual(['POST', '/datapacks/dp%201/knowledge-bases/upload-url', 'Bearer test-key', 'application/json']);
    expect(ask.body).toEqual({ filename: 'notes.md', size: 5, content_hash: createHash('sha256').update(bytes).digest('hex'), client: 'agent-k-local' });
    expect([post.method, post.path, post.auth]).toEqual(['POST', storage, null]);
    expect([...post.form!.keys()]).toEqual(['key', 'policy', 'x-amz-checksum-sha256', 'file']);
    const file = post.form?.get('file') as File;
    expect([file.name, await file.text()]).toEqual(['notes.md', '# Hi\n']);
    expect(status.path).toBe('/datapacks/dp%201/knowledge-bases/document-status?document_key=u%2Fdp%2Fh%2Fnotes.md');
  });

  it('reads an upload Meko already has, and refuses a reply it can\'t use', async () => {
    const f = fakeMekoApi();
    f.handle(() => ok({ url: '', key: 'u/dp/h/old.md', already_uploaded: true, existing_filename: 'old.md' }));
    expect(await f.api.requestUpload('dp', 'old.md', Buffer.from('o'))).toEqual({ url: '', fields: [], key: 'u/dp/h/old.md', alreadyUploaded: true });
    f.handle(() => ok({ url: 'https://bucket.s3.test/', key: 'k', already_uploaded: false }));
    await expect(f.api.requestUpload('dp', 'a.md', Buffer.from('a'))).rejects.toMatchObject({ code: 'BAD_REPLY' });
    f.handle(() => ok({ url: 'https://bucket.s3.test/', fields: {}, already_uploaded: false }));
    await expect(f.api.requestUpload('dp', 'a.md', Buffer.from('a'))).rejects.toMatchObject({ code: 'BAD_REPLY' });
  });

  it("posts only over https, unless Meko's own API is on http, and words the storage's refusal without its URL", async () => {
    const up = (url: string) => ({ url, fields: [['policy', 'p'] as [string, string]], key: 'k', alreadyUploaded: false });
    const f = fakeMekoApi();
    await expect(f.api.postPresigned(up('http://minio.test/b?X-Amz-Signature=s'), 'a.md', Buffer.from('a'))).rejects.toMatchObject({ code: 'BAD_REPLY' });
    await expect(f.api.postPresigned(up('not a url'), 'a.md', Buffer.from('a'))).rejects.toMatchObject({ code: 'BAD_REPLY' });
    expect(f.calls).toEqual([]);
    const local = fakeMekoApi('http://localhost:8080/api/v1');
    local.handle(() => ({ status: 204 }));
    await local.api.postPresigned(up('http://minio.test/b'), 'a.md', Buffer.from('a'));
    expect(local.calls.map((c) => c.path)).toEqual(['http://minio.test/b']);

    f.handle(() => ({ status: 400, body: '<?xml version="1.0"?><Error><Code>EntityTooLarge</Code><Message>Your proposed upload exceeds the maximum allowed size</Message></Error>' }));
    const e = await f.api.postPresigned(up('https://bucket.s3.test/?sig=s'), 'a.md', Buffer.from('a')).catch((x: unknown) => x);
    expect(e).toMatchObject({ status: 400, code: 'STORAGE_EntityTooLarge', message: 'Your proposed upload exceeds the maximum allowed size' });
    f.handle(() => ({ status: 502, body: 'Bad gateway' }));
    await expect(f.api.postPresigned(up('https://bucket.s3.test/'), 'a.md', Buffer.from('a'))).rejects.toMatchObject({ status: 502, code: 'STORAGE_HTTP_502' });
    const down = new MekoApi(() => ({ baseUrl: 'https://meko.test/api/v1', apiKey: 'k' }), (async () => {
      throw new TypeError('fetch failed');
    }) as typeof fetch);
    const gone = await down.postPresigned(up('https://bucket.s3.test/x?sig=s'), 'a.md', Buffer.from('a')).catch((x: unknown) => x);
    expect(gone).toMatchObject({ code: 'UNREACHABLE', message: "Couldn't reach Meko's file storage at bucket.s3.test: fetch failed" });
  });

  it("reads a share link, changes it one step a call, and joins by a link's token", async () => {
    const f = fakeMekoApi();
    f.handle((c) => c.path.startsWith('/share/')
      ? ok({ datapack_id: 'd1', grant: 'Contributor', already_member: false })
      : ok({ share_link: { status: 'active', token: 't 1', grant: 'contributor', redemption_count: 3, members_at_link_role: 2 } }));
    expect(await f.api.shareLink('dp 1')).toEqual({ active: true, token: 't 1', grant: 'contributor', joined: 3, atLinkRole: 2 });
    await f.api.setShareLink('dp 1', { reset: true });
    expect(await f.api.redeemShareLink('t/1')).toEqual({ datapackId: 'd1', grant: 'contributor', alreadyMember: false });
    expect(f.calls.map((c) => [c.method, c.path, c.body])).toEqual([
      ['GET', '/datapacks/dp%201/share-link', undefined],
      ['PUT', '/datapacks/dp%201/share-link', { reset: true }],
      ['POST', '/share/t%2F1/redeem', undefined],
    ]);
    // An answer of another shape means Meko's API changed: an error, not an empty link.
    f.handle(() => ok({}));
    await expect(f.api.shareLink('d')).rejects.toMatchObject({ code: 'BAD_REPLY' });
    f.handle(() => ok({ share_link: { status: 'active', token: 't', grant: 'maintainer' } }));
    await expect(f.api.shareLink('d')).rejects.toMatchObject({ code: 'BAD_REPLY' });
    await expect(f.api.redeemShareLink('t')).rejects.toMatchObject({ code: 'BAD_REPLY' });
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
