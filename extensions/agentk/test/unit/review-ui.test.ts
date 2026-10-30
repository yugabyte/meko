import { describe, expect, it } from 'vitest';
import { REVIEW_LEFT_CODE, type CodeReviewDetail, type CodeReviewFinding, type CodeReviewSummary } from '../../src/shared/codereview';
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
  const f = (key: string, round: number, sweep: number) => ({ key, round, sweep }) as CodeReviewFinding;

  it('is one unnamed group for a one-round, one-sweep review', () => {
    expect(findingGroups([f('a', 1, 1), f('b', 1, 1)])).toEqual([{ key: '1:1', label: 'Findings', findings: [f('a', 1, 1), f('b', 1, 1)] }]);
  });

  it('groups by round, then sweep, naming only what had more than one', () => {
    const groups = findingGroups([f('c', 2, 1), f('a', 1, 2), f('b', 1, 1)]);
    expect(groups.map((g) => [g.label, g.findings.map((x) => x.key)])).toEqual([
      ['Round 1, sweep 1', ['b']],
      ['Round 1, sweep 2', ['a']],
      ['Round 2, sweep 1', ['c']],
    ]);
  });
});

describe('findings panel while the review runs', () => {
  const review = (state: CodeReviewSummary['state']) => ({ state }) as CodeReviewSummary;
  const detail = (major: number) => ({ review: { counts: { blocker: 0, major, minor: 0 } } }) as CodeReviewDetail;

  it.each([
    ['preparing', true],
    ['sweeping', true],
    ['scoring', true],
    // Waiting on the task's PR description, before round 1.
    ['describing', true],
    // The round's findings are final while the task fixes them.
    ['fixing', false],
    ['done', false],
    ['failed', false],
    ['cancelled', false],
  ] as const)('reads Reviewing in %s: %s', (state, reviewing) => {
    expect(findingsReviewing(review(state))).toBe(reviewing);
  });

  it('reads nothing without a review', () => {
    expect(findingsReviewing(null)).toBe(false);
    expect(findingsCountsText(null, null)).toBeNull();
  });

  it('leaves the counts out of the header while it reads Reviewing', () => {
    expect(findingsCountsText(review('sweeping'), detail(0))).toBeNull();
    expect(findingsCountsText(review('describing'), detail(2))).toBeNull();
  });

  it('shows the counts once the review stops running, no findings included', () => {
    expect(findingsCountsText(review('done'), detail(0))).toBe('no findings');
    expect(findingsCountsText(review('fixing'), detail(2))).toBe('2 major');
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
