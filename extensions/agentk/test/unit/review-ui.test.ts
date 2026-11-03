import { describe, expect, it } from 'vitest';
import { DESCRIBE_INTERRUPTED, REVIEW_LEFT_CODE, type CodeReviewDetail, type CodeReviewFinding, type CodeReviewSummary } from '../../src/shared/codereview';
import type { Session } from '../../src/shared/types';
import { canRunReview, findingGroups, findingsCountsText, findingsReviewing, stageChipText, withOverride } from '../../src/web/util.js';

describe('stageChipText', () => {
  const review = (state: CodeReviewSummary['state'], detail: string | null, onDemand = false) => ({ state, detail, onDemand }) as CodeReviewSummary;
  it.each([
    ['code', null, null, 'Code'],
    ['code', 'describing', null, 'Review & PR'],
    ['code', 'sweeping', null, 'Review & PR'],
    ['code', 'triage', null, 'Review & PR'],
    ['code', 'fixing', null, 'Review & PR'],
    ['code', 'failed', 'no review pass reported', 'Review & PR'],
    ['code', 'cancelled', 'you cancelled it', 'Review & PR'],
    // Cancelled by leaving Code: it doesn't hold the next Open PR, which starts a new review.
    ['code', 'cancelled', REVIEW_LEFT_CODE, 'Code'],
    ['code', 'done', null, 'Code'],
    ['review', 'done', null, 'Review & PR'],
    // One the user ran holds nothing: the task stays in Code with its Open PR.
    ['code', 'sweeping', 'on demand', 'Code'],
    ['code', 'failed', 'on demand', 'Code'],
  ] as const)('in %s with a %s review (%s) reads %s', (stage, state, detail, text) => {
    const onDemand = detail === 'on demand';
    expect(stageChipText({ stage, codeReview: state ? review(state, onDemand ? null : detail, onDemand) : null } as Session)).toBe(text);
  });
});

describe('canRunReview', () => {
  const s = (over: Partial<Session>) => ({ kind: 'task', stage: 'code', prNumber: null, codeReview: null, status: 'idle', busy: false, ...over }) as Session;
  const review = (state: CodeReviewSummary['state']) => ({ state, detail: null }) as CodeReviewSummary;
  it.each([
    ['a task in Code', s({}), { offered: true, ready: true, wait: null }],
    ['mid-turn', s({ status: 'working' }), { offered: true, ready: false, wait: "After AgentK's turn" }],
    ['waiting on the user', s({ status: 'needs_input' }), { offered: true, ready: false, wait: 'Answer AgentK in the terminal first' }],
    ['exited', s({ status: 'exited' }), { offered: true, ready: false, wait: 'Resume the session first' }],
    ['past a finished review', s({ codeReview: review('done') }), { offered: true, ready: true, wait: null }],
    ['while one runs', s({ codeReview: review('sweeping') }), { offered: false, ready: false, wait: null }],
    ['while its findings wait on you', s({ codeReview: review('triage') }), { offered: false, ready: false, wait: null }],
    ['while one waits for fixes', s({ codeReview: review('fixing') }), { offered: false, ready: false, wait: null }],
    ['in Review & PR with its PR', s({ stage: 'review', prNumber: 7 }), { offered: true, ready: true, wait: null }],
    ['in Review & PR before the PR is found', s({ stage: 'review' }), { offered: false, ready: false, wait: null }],
    ['in Design', s({ stage: 'design' }), { offered: false, ready: false, wait: null }],
    ['on a Review a PR session', s({ kind: 'review-pr', stage: 'reviewing-pr' }), { offered: false, ready: false, wait: null }],
  ] as const)('%s: %j', (_, session, want) => {
    expect(canRunReview(session)).toEqual(want);
  });
});

describe('findingGroups', () => {
  const f = (key: string, round: number, sweep: number, kept = true, pending?: boolean, dismissed?: boolean) =>
    ({ key, round, sweep, kept, pending, ...(dismissed ? { you: { dismissed, note: '' } } : {}) }) as CodeReviewFinding;

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

  it("puts each group's dropped findings, and the ones the user dismissed, under dismissed, apart from its kept ones and the running round's pending ones, in the order they came", () => {
    const groups = findingGroups([
      f('a', 1, 1, false), f('b', 1, 1), f('c', 1, 1, false), f('d', 1, 1), f('e', 2, 1, false), f('h', 2, 1, true, false, true), f('i', 2, 1),
      f('f', 3, 1, false, true), f('g', 3, 1, false, true),
    ]);
    expect(groups.map((g) => [g.label, g.findings.map((x) => x.key), g.dismissed.map((x) => x.key)])).toEqual([
      ['Round 1', ['b', 'd'], ['a', 'c']],
      ['Round 2', ['i'], ['e', 'h']],
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
    // The round's findings are final while they wait on the user, and while the task fixes them.
    ['triage', null, false],
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
    [{ depth: 2 }, 'check', 'no', { depth: 2, check: false }],
    [{ check: false }, 'check', '', {}],
  ] as const)('%j with %s set to %j is %j', (value, key, raw, out) => {
    expect(withOverride(value, key, raw)).toEqual(out);
  });
});
