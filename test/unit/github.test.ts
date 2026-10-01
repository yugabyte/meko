import { describe, expect, it } from 'vitest';
import { isRepoName, parsePrInput, pickPr, prFromUrl, type PrRef } from '../../src/server/github.js';

const pr = (number: number, state: PrRef['state'], createdAt: string, isCrossRepository = false): PrRef =>
  ({ number, url: `https://github.com/acme/demo/pull/${number}`, state, createdAt, isCrossRepository });

describe('finding the PR a session opened', () => {
  const since = Date.parse('2026-09-01T12:00:00Z');

  it('ignores an older PR from a branch of the same name', () => {
    expect(pickPr([pr(3, 'MERGED', '2026-05-01T09:00:00Z')], since)).toBeNull();
    expect(pickPr([pr(3, 'MERGED', '2026-05-01T09:00:00Z'), pr(9, 'OPEN', '2026-09-01T12:30:00Z')], since)?.number).toBe(9);
  });

  it('prefers an open PR, then the newest', () => {
    expect(pickPr([pr(9, 'OPEN', '2026-09-01T12:30:00Z'), pr(10, 'CLOSED', '2026-09-01T13:00:00Z')], since)?.number).toBe(9);
    expect(pickPr([pr(9, 'CLOSED', '2026-09-01T12:30:00Z'), pr(10, 'CLOSED', '2026-09-01T13:00:00Z')], since)?.number).toBe(10);
  });

  it('takes an older open PR only for a branch that existed before the session', () => {
    const old = [pr(3, 'OPEN', '2026-05-01T09:00:00Z'), pr(2, 'MERGED', '2026-04-01T09:00:00Z')];
    expect(pickPr(old, since)).toBeNull();
    expect(pickPr(old, since, true)?.number).toBe(3);
    expect(pickPr([pr(2, 'MERGED', '2026-04-01T09:00:00Z')], since, true)).toBeNull();
    // A fork's PR from a branch of the same name isn't this branch's.
    expect(pickPr([pr(4, 'OPEN', '2026-05-01T09:00:00Z', true)], since, true)).toBeNull();
    expect(pickPr([{ ...pr(4, 'OPEN', '2026-05-01T09:00:00Z'), isCrossRepository: undefined }], since, true)).toBeNull();
  });

  it('allows for a little clock skew', () => {
    expect(pickPr([pr(9, 'OPEN', '2026-09-01T11:59:30Z')], since)?.number).toBe(9);
    expect(pickPr([], since)).toBeNull();
  });
});

describe('a PR the user types', () => {
  it('reads a number, owner/name#number and a PR link', () => {
    expect(parsePrInput('21')).toEqual({ repo: null, number: 21 });
    expect(parsePrInput(' #21 ')).toEqual({ repo: null, number: 21 });
    expect(parsePrInput('acme/demo#21')).toEqual({ repo: 'acme/demo', number: 21 });
    expect(parsePrInput('https://github.com/acme/demo/pull/21')).toEqual({ repo: 'acme/demo', number: 21 });
    expect(parsePrInput('github.com/acme/my.repo/pull/21/files#diff-1')).toEqual({ repo: 'acme/my.repo', number: 21 });
    expect(prFromUrl('https://github.com/acme/demo/pull/7')).toEqual({ repo: 'acme/demo', number: 7 });
  });

  it('refuses anything else', () => {
    for (const bad of ['', '0', '#0', 'pr 21', 'acme/demo', 'https://gitlab.com/acme/demo/pull/2', 'https://github.com/acme/demo/issues/2', '../x#3', 'a/b/c#1']) {
      expect(parsePrInput(bad), bad).toBeNull();
    }
    expect(prFromUrl(null)).toBeNull();
    expect(prFromUrl('https://github.com/acme/demo/pull/x')).toBeNull();
    expect(isRepoName('acme/demo')).toBe(true);
    for (const bad of ['acme', 'acme/demo/x', '../demo', 'acme/..', 'ac me/demo', 7]) expect(isRepoName(bad), String(bad)).toBe(false);
  });
});
