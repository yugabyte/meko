import { describe, expect, it } from 'vitest';
import type { CodeReviewFinding, CodeReviewSummary } from '../../src/shared/codereview';
import type { Session } from '../../src/shared/types';
import { findingGroups, stageChipText, withOverride } from '../../src/web/util.js';

describe('stageChipText', () => {
  const review = (state: CodeReviewSummary['state']) => ({ state }) as CodeReviewSummary;
  it.each([
    ['code', null, 'Code'],
    ['code', 'sweeping', 'Review'],
    ['code', 'fixing', 'Review'],
    ['code', 'failed', 'Review'],
    ['code', 'done', 'Code'],
    ['review', 'done', 'PR & Review'],
  ] as const)('in %s with a %s review reads %s', (stage, state, text) => {
    expect(stageChipText({ stage, codeReview: state ? review(state) : null } as Session)).toBe(text);
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
