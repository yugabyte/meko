import { describe, expect, it } from 'vitest';
import { knowledgeName, uploadError } from '../../src/server/knowledge.js';
import { MekoApiError } from '../../src/server/mekoapi.js';
import { MAX_KB_UPLOAD_BYTES } from '../../src/shared/types.js';
import { batchSummary, knowledgeBatch, skippedNotice, sortDrop, type KnowledgeFileState } from '../../src/web/knowledge.js';

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

  it('keeps a batch\'s skipped files apart, and sums up what its uploads did', () => {
    const f = (name: string, size = 10) => ({ name, size });
    const b = knowledgeBatch([f('a.md'), f('empty.md', 0), f('big.pdf', MAX_KB_UPLOAD_BYTES + 1), f('b.md'), f('c.md'), f('d.md')]);
    expect([b.files.map((x) => x.name), b.skipped.map((x) => x.name)]).toEqual([['a.md', 'b.md', 'c.md', 'd.md'], ['empty.md', 'big.pdf']]);
    expect(batchSummary(b)).toBeNull();
    const states: KnowledgeFileState[] = [{ at: 'indexing' }, { at: 'added' }, { at: 'already' }, { at: 'failed', error: 'no' }];
    expect(batchSummary({ ...b, states })).toBe('2 files added to the knowledge base. Meko is indexing 1 of them. 1 file was already there. 1 file couldn’t be added.');
    expect(batchSummary({ ...b, states: [{ at: 'added' }, { at: 'waiting' }, { at: 'waiting' }, { at: 'waiting' }] })).toBe('a.md added to the knowledge base.');
  });
});
