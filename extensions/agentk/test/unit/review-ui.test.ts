import { describe, expect, it } from 'vitest';
import { DESCRIBE_INTERRUPTED, REVIEW_LEFT_CODE, type CodeReviewDetail, type CodeReviewFinding, type CodeReviewSummary } from '../../src/shared/codereview';
import type { Session } from '../../src/shared/types';
import { findingGroups, findingsCountsText, findingsReviewing, stageChipText, withOverride } from '../../src/web/util.js';

describe('stageChipText', () => {
  const review = (state: CodeReviewSummary['state'], detail: string | null) => ({ state, detail }) as CodeReviewSummary;
  it.each([
    ['code', null, null, 'Code'],
    ['code', 'describing', null, 'Review & PR'],
    ['code', 'sweeping', null, 'Review & PR'],
    ['code', 'fixing', null, 'Review & PR'],
    ['code', 'failed', 'no review pass reported', 'Review & PR'],
    ['code', 'cancelled', 'you cancelled it', 'Review & PR'],
    // Cancelled by leaving Code: it doesn't hold the next Open PR, which starts a new review.
    ['code', 'cancelled', REVIEW_LEFT_CODE, 'Code'],
    ['code', 'done', null, 'Code'],
    ['review', 'done', null, 'Review & PR'],
  ] as const)('in %s with a %s review (%s) reads %s', (stage, state, detail, text) => {
    expect(stageChipText({ stage, codeReview: state ? review(state, detail) : null } as Session)).toBe(text);
  });
});

describe('findingGroups', () => {
  const f = (key: string, round: number, sweep: number, kept = true, pending?: boolean) => ({ key, round, sweep, kept, pending }) as CodeReviewFinding;

  it('is one unnamed group for a one-round, one-sweep review', () => {
    expect(findingGroups([f('a', 1, 1), f('b', 1, 1)])).toEqual([
      { key: '1:1', label: 'Findings', findings: [f('a', 1, 1), f('b', 1, 1)], dismissed: [] },
    ]);
  });

  it('groups by round, then sweep, naming only what had more than one', () => {
    const groups = findingGroups([f('c', 2, 1), f('a', 1, 2), f('b', 1, 1)]);
    expect(groups.map((g) => [g.label, g.findings.map((x) => x.key)])).toEqual([
      ['Round 1, sweep 1', ['b']],
      ['Round 1, sweep 2', ['a']],
      ['Round 2, sweep 1', ['c']],
    ]);
  });

  it("puts each group's dropped findings under dismissed, apart from its kept ones and the running round's pending ones, in the order they came", () => {
    const groups = findingGroups([
      f('a', 1, 1, false), f('b', 1, 1), f('c', 1, 1, false), f('d', 1, 1), f('e', 2, 1, false),
      f('f', 3, 1, false, true), f('g', 3, 1, false, true),
    ]);
    expect(groups.map((g) => [g.label, g.findings.map((x) => x.key), g.dismissed.map((x) => x.key)])).toEqual([
      ['Round 1', ['b', 'd'], ['a', 'c']],
      ['Round 2', [], ['e']],
      ['Round 3', ['f', 'g'], []],
    ]);
  });
});

describe('findings panel while the review runs', () => {
  const review = (state: CodeReviewSummary['state'], detail: string | null = null) => ({ state, detail }) as CodeReviewSummary;

  it.each([
    ['preparing', null, true],
    ['sweeping', null, true],
    ['scoring', null, true],
    // The task writes its PR description, before round 1.
    ['describing', null, true],
    // Past an interrupted description turn, it waits on the user's Review now.
    ['describing', DESCRIBE_INTERRUPTED, false],
    // The round's findings are final while the task fixes them.
    ['fixing', null, false],
    ['done', null, false],
    ['failed', 'no review pass reported', false],
    ['cancelled', 'you cancelled it', false],
  ] as const)('reads Reviewing in %s (%s): %s', (state, detail, reviewing) => {
    expect(findingsReviewing(review(state, detail))).toBe(reviewing);
  });

  it.each([
    [null, null, null],
    ['sweeping', 0, null],
    ['done', 0, 'no findings'],
    ['fixing', 2, '2 major'],
  ] as const)('heads a %s review with %s major findings with counts %s', (state, major, text) => {
    const detail = major === null ? null : ({ review: { counts: { blocker: 0, major, minor: 0 } } }) as CodeReviewDetail;
    expect(findingsCountsText(state ? review(state) : null, detail)).toBe(text);
  });
});

describe('withOverride', () => {
  it.each([
    [{}, 'strategy', 'sirius-1.0', { strategy: 'sirius-1.0' }],
    [{ strategy: 'off', rounds: 2 }, 'strategy', '', { rounds: 2 }],
    [{ depth: 3 }, 'depth', '1', { depth: 1 }],
    [{ rounds: null, depth: 2 }, 'rounds', '0', { rounds: 0, depth: 2 }],
    [{}, 'strategy', 'nope', {}],
  ] as const)('%j with %s set to %j is %j', (value, key, raw, out) => {
    expect(withOverride(value, key, raw)).toEqual(out);
  });
});
