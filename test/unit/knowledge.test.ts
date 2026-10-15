import { describe, expect, it } from 'vitest';
import { knowledgeName, uploadError } from '../../src/server/knowledge.js';
import { MekoApiError } from '../../src/server/mekoapi.js';
import { skippedNotice, sortDrop } from '../../src/web/knowledge.js';

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
    [new MekoApiError(404, 'HTTP_404', 'Meko answered 404.'), 502, /doesn't take uploads from AgentK yet/],
    [new MekoApiError(404, 'NOT_FOUND', 'datapack not found'), 502, /^datapack not found$/],
    [new MekoApiError(413, 'FILE_TOO_LARGE', 'Free plans take files up to 5 MB'), 413, /^Free plans take files up to 5 MB$/],
    [new MekoApiError(400, 'INVALID_FILE', 'content does not match .pdf'), 400, /^content does not match .pdf$/],
    [new MekoApiError(0, 'UNREACHABLE', "Couldn't reach Meko's API at x: fetch failed"), 502, /Couldn't reach Meko's API/],
  ])('words Meko refusing an upload (%s)', (e, status, message) => {
    const h = uploadError(e, 'adlc/proj/r/p');
    expect(h.status).toBe(status);
    expect(h.message).toMatch(message);
  });

  it('sorts a drop into images for Claude, files for the knowledge base, and the rest', () => {
    const f = (name: string) => ({ name });
    const [png, md, pdf, zip] = [f('a.png'), f('notes.md'), f('spec.PDF'), f('x.zip')];
    expect(sortDrop([png, md, pdf, zip], [png])).toEqual({ images: [png], knowledge: [md, pdf], other: [zip] });
    expect(skippedNotice([zip])).toMatch(/^Skipped x\.zip\./);
    expect(skippedNotice([zip, zip])).toMatch(/^Skipped 2 files\./);
  });
});
