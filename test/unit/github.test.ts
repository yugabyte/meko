import { describe, expect, it } from 'vitest';
import { pickPr, type PrRef } from '../../src/server/github.js';

const pr = (number: number, state: PrRef['state'], createdAt: string): PrRef =>
  ({ number, url: `https://github.com/acme/demo/pull/${number}`, state, createdAt });

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

  it('allows for a little clock skew', () => {
    expect(pickPr([pr(9, 'OPEN', '2026-09-01T11:59:30Z')], since)?.number).toBe(9);
    expect(pickPr([], since)).toBeNull();
  });
});
