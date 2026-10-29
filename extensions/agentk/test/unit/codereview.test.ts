import { describe, expect, it } from 'vitest';
import {
  findMeAkReviewLine, meAkReviewLine, noCounts, parseMeAkReviewLine, readOverrides, resolveReview, reviewSegmentText, withMeAkReviewLine,
  type CodeReviewSummary, type SeverityCounts,
} from '../../src/shared/codereview.js';

const counts = (blocker: number, major: number, minor: number): SeverityCounts => ({ blocker, major, minor });

describe('the me_ak_review line', () => {
  it.each([
    [counts(0, 0, 0), 'me_ak_review:LGTM'],
    [counts(4, 4, 0), 'me_ak_review:4 blockers,4 major addressed'],
    [counts(1, 2, 3), 'me_ak_review:1 blocker,2 major,3 minor addressed'],
    [counts(0, 0, 2), 'me_ak_review:2 minor addressed'],
  ])('writes %j as %s and reads it back', (c, line) => {
    expect(meAkReviewLine(c)).toBe(line);
    expect(parseMeAkReviewLine(line)).toEqual(c);
  });

  it.each(['meko_design:x', 'me_ak_review:4 blokers addressed', 'me_ak_review:4 blockers', 'me_ak_review:0 major addressed'])(
    "doesn't read %s as one",
    (line) => expect(parseMeAkReviewLine(line)).toBeNull(),
  );

  it.each([
    ['Body.', 'Body.\n\nme_ak_review:LGTM'],
    ['', 'me_ak_review:LGTM'],
    ['Body.\nme_ak_review:1 minor addressed\nmeko_conversation_id:c', 'Body.\nme_ak_review:LGTM\nmeko_conversation_id:c'],
  ])('sets it in %j', (body, want) => {
    expect(withMeAkReviewLine(body, meAkReviewLine(noCounts()))).toBe(want);
    expect(findMeAkReviewLine(want)).toBe('me_ak_review:LGTM');
  });
});

describe('review settings', () => {
  const global = { strategy: 'sagittarius-3.0' as const, rounds: 0, depth: 1 };

  it('takes the session over the project over the global values, each on its own', () => {
    expect(resolveReview(global, { strategy: 'sirius-1.0', depth: 3 }, { depth: 2, rounds: null })).toEqual({ strategy: 'sirius-1.0', rounds: 0, depth: 2 });
  });

  it.each([
    [{ strategy: 'off', rounds: 9, depth: 0 }, { strategy: 'off', rounds: 5, depth: 1 }],
    [{ strategy: 'goliath-1.0', rounds: 1.5, depth: '3' }, {}],
    [null, {}],
  ])('reads %j from untrusted input as %j', (input, want) => {
    expect(readOverrides(input)).toEqual(want);
  });
});

describe("the review's part of a task's tab", () => {
  const r = (over: Partial<CodeReviewSummary>): CodeReviewSummary => ({
    id: 'r', sessionId: 's', strategy: 'sagittarius-3.0', state: 'sweeping', round: 1, rounds: 1, sweep: 1, depth: 1,
    passesDone: 2, passes: 5, scoresDone: 3, candidates: 9, counts: noCounts(), mekoCall: false, detail: null, ...over,
  });

  it.each([
    [r({}), '3/5'],
    [r({ passesDone: 5 }), '5/5'],
    [r({ state: 'scoring' }), '3/9'],
    [r({ state: 'done', counts: counts(2, 1, 4) }), '2·1'],
    [r({ state: 'done' }), '✓'],
    [r({ state: 'failed' }), '!'],
    [r({ state: 'cancelled' }), '!'],
  ])('shows %j as %s', (summary, text) => {
    expect(reviewSegmentText(summary)).toBe(text);
  });
});
