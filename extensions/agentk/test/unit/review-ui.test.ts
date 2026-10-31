import { describe, expect, it } from 'vitest';
import { REVIEW_LEFT_CODE, type CodeReviewFinding, type CodeReviewSummary } from '../../src/shared/codereview';
import type { Session } from '../../src/shared/types';
import { findingGroups, stageChipText, withOverride } from '../../src/web/util.js';

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

  it("puts each group's dropped findings under dismissed, apart from its kept ones, both in the order they came", () => {
    const groups = findingGroups([f('a', 1, 1, false), f('b', 1, 1), f('c', 1, 1, false), f('d', 1, 1), f('e', 2, 1, false)]);
    expect(groups.map((g) => [g.label, g.findings.map((x) => x.key), g.dismissed.map((x) => x.key)])).toEqual([
      ['Round 1', ['b', 'd'], ['a', 'c']],
      ['Round 2', [], ['e']],
    ]);
  });

  it("keeps the running round's candidates, not yet kept or dropped, out of dismissed", () => {
    const groups = findingGroups([f('a', 1, 1), f('b', 1, 1, false), f('c', 2, 1, false, true), f('d', 2, 1, false, true)]);
    expect(groups.map((g) => [g.label, g.findings.map((x) => x.key), g.dismissed.map((x) => x.key)])).toEqual([
      ['Round 1', ['a'], ['b']],
      ['Round 2', ['c', 'd'], []],
    ]);
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
