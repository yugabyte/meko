import { describe, expect, it } from 'vitest';
import { knowledgeName, uploadError } from '../../src/server/knowledge.js';
import { MekoApiError } from '../../src/server/mekoapi.js';
import type { KnowledgeUpload } from '../../src/shared/types.js';
import { mekoAsk, mekoPlan, mekoSummary, type MekoResult } from '../../src/web/knowledge.js';

describe('knowledge-base files', () => {
  it.each([
    ['notes.md', 'notes.md'],
    [' Spec.PDF ', 'Spec.PDF'],
    ['data.json', 'data.json'],
    ['page.htm', 'page.htm'],
    ['readme.TEXT', 'readme.TEXT'],
    ['pic.png', 400],
    ['archive.zip', 400],
    ['md', 400],
    ['.md', 400],
    ['notes.', 400],
    ['../notes.md', 400],
    ['dir/notes.md', 400],
    ['dir\\notes.md', 400],
    ['', 400],
    [undefined, 400],
  ])('takes the name %j: %s', (name, want) => {
    if (typeof want === 'string') expect(knowledgeName(name)).toBe(want);
    else expect(() => knowledgeName(name)).toThrow(expect.objectContaining({ status: want }));
  });

  it.each([
    [new MekoApiError(403, 'FORBIDDEN', 'forbidden'), 403, /only its owner and maintainers can/],
    [new MekoApiError(401, 'UNAUTHORIZED', 'invalid or expired token'), 502, /refused AgentK's key/],
    [new MekoApiError(403, 'INSUFFICIENT_SCOPE', 'needs write'), 502, /refused AgentK's key/],
    [new MekoApiError(404, 'HTTP_404', 'Meko answered 404.'), 502, /doesn't take uploads from AgentK yet\. Add the file from Meko's web app/],
    [new MekoApiError(404, 'NOT_FOUND', 'datapack not found'), 502, /^Meko couldn't find adlc\/proj\/r\/p: datapack not found\. Check the project's datapack/],
    [new MekoApiError(500, 'INTERNAL', 'boom.'), 502, /^Meko couldn't add the file: boom\. Try again in a minute/],
    [new MekoApiError(413, 'FILE_TOO_LARGE', 'Free plans take files up to 5 MB'), 413, /^Free plans take files up to 5 MB$/],
    [new MekoApiError(400, 'INVALID_FILE', 'content does not match .pdf'), 400, /^content does not match .pdf$/],
    [new MekoApiError(0, 'UNREACHABLE', "Couldn't reach Meko's API at x: fetch failed"), 502, /Couldn't reach Meko's API/],
  ])('words Meko refusing an upload (%s)', (e, status, message) => {
    const h = uploadError(e, 'adlc/proj/r/p');
    expect(h.status).toBe(status);
    expect(h.message).toMatch(message);
  });

  it('splits held files into those Upload to Meko takes and those it skips, and asks before sending', () => {
    const held = [{ name: 'notes.md' }, { name: '' }, { name: 'spec.PDF' }, { name: 'x.zip' }];
    const plan = mekoPlan(held);
    expect([plan.upload.map((h) => h.name), plan.skip.map((h) => h.name)]).toEqual([['notes.md', 'spec.PDF'], ['', 'x.zip']]);
    const ask = mekoAsk(plan, 'adlc/proj/r/p');
    expect(ask).toMatch(/^Upload notes\.md and spec\.PDF to adlc\/proj\/r\/p\? Everyone with access/);
    expect(ask).toMatch(/It skips a pasted image and x\.zip:/);
    expect(mekoAsk(mekoPlan([{ name: 'a.md' }]), 'dp')).not.toMatch(/skips/);
  });

  it('sums up an Upload to Meko: what was added, what Meko had, what failed and why', () => {
    const up = (name: string, status: KnowledgeUpload['status']): MekoResult =>
      ({ name, upload: { filename: name, documentKey: 'k', datapackName: 'dp', status } });
    expect(mekoSummary([up('a.md', 'processing'), up('b.md', 'uploaded'), up('c.md', 'already_processed'), { name: 'd.pdf', error: 'Meko said no.' }]))
      .toBe('a.md and b.md added to the knowledge base. Meko is indexing 1 of them. c.md was already there. Couldn’t add d.pdf: Meko said no.');
    expect(mekoSummary([up('a.md', 'processing')])).toBe('a.md added to the knowledge base. Meko is indexing it.');
  });
});
