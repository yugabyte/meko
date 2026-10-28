import { describe, expect, it } from 'vitest';
import type { LearnProgress, ReviewingPr, Session } from '../../src/shared/types.js';
import { closingText, designGate, elapsedText, headerApproves, learnStepText, mergeGate, openPrGate, queuedText, reviewTarget, updateShowing } from '../../src/web/util.js';
import { closeStep } from '../../src/server/store.js';

const session = (over: Partial<Session> = {}) =>
  ({
    status: 'idle', busy: false, midTurn: false, promptsHeld: 0, queued: null,
    canMerge: false, mergeQueueable: false, mergeBlockers: [], ...over,
  }) as Session;

describe('openPrGate', () => {
  it('opens the PR between turns', () => {
    expect(openPrGate(session())).toEqual({ label: 'Open PR', reason: null, title: 'Commit, push and open a pull request' });
  });

  it('queues it for the turn end while AgentK works, Claude starts, or Claude asks mid-turn', () => {
    for (const over of [{ status: 'working' }, { status: 'starting' }, { status: 'needs_input', midTurn: true }] as Partial<Session>[]) {
      expect(openPrGate(session(over))).toMatchObject({ label: 'Open PR after this turn', reason: null });
    }
  });

  it('is closed when Claude exited, waits on the user with no turn behind it, or holds prompts', () => {
    expect(openPrGate(session({ status: 'exited' }))).toMatchObject({ label: 'Open PR', reason: 'Claude has exited. Resume the session first.' });
    expect(openPrGate(session({ status: 'needs_input' })).reason).toBe('AgentK is waiting on you in the terminal. Answer it first.');
    expect(openPrGate(session({ promptsHeld: 1 })).reason).toMatch(/prompts from you/);
  });
});

describe('mergeGate', () => {
  it('merges now when nothing blocks it', () => {
    expect(mergeGate(session({ canMerge: true }))).toMatchObject({ label: 'Merge & Close', reason: null });
  });

  it('offers Merge when ready while only blockers that clear by themselves are left', () => {
    const gate = mergeGate(session({ mergeQueueable: true, mergeBlockers: ['CI is still running.', 'Waiting for the AgentK review.'] }));
    expect(gate).toMatchObject({ label: 'Merge when ready', reason: null });
    expect(gate.title).toContain('CI is still running.\nWaiting for the AgentK review.');
  });

  it('is closed, with the blockers, otherwise', () => {
    expect(mergeGate(session({ mergeBlockers: ['CI is failing.', 'CI is still running.'] }))).toEqual({
      label: 'Merge & Close', reason: 'CI is failing.\nCI is still running.', title: 'CI is failing.\nCI is still running.',
    });
  });
});

describe('queuedText', () => {
  it('says what a queued gate waits for', () => {
    expect(queuedText(session())).toBeNull();
    expect(queuedText(session({ queued: 'open-pr' }))).toBe('✓ Open PR queued: goes when the turn ends');
    expect(queuedText(session({ queued: 'merge', mergeBlockers: ['CI is still running.'] }))).toBe('✓ Merge queued: waiting on CI');
    expect(queuedText(session({ queued: 'merge', mergeBlockers: ['Waiting for the AgentK review.'] }))).toBe('✓ Merge queued: waiting on the AgentK review');
    expect(queuedText(session({ queued: 'merge', mergeBlockers: ['AgentK is still working.', 'CI is still running.'] }))).toBe('✓ Merge queued: waiting on AgentK');
  });

  it.each([
    [{ queued: 'open-pr' }, '✓ Run to merge: opens the PR when this turn ends'],
    [{ queued: 'merge', mergeBlockers: ['CI is failing.'] }, '✓ Run to merge: waiting on a CI fix'],
    [{ queued: 'merge', mergeBlockers: ['Changes were requested.'] }, '✓ Run to merge: waiting on you'],
    [{ queued: 'merge', mergeQueueable: true, mergeBlockers: ['AgentK is still working.'] }, '✓ Run to merge: waiting on AgentK'],
  ] as [Partial<Session>, string][])('says what Run to merge waits for: %o', (over, text) => {
    expect(queuedText(session({ autoRun: true, ...over }))).toBe(text);
  });
});

describe('headerApproves', () => {
  const design = (open: number, over: Partial<Session> = {}) =>
    session({ stage: 'design', desired: 'running', hasDesign: true, designQuestions: { open }, ...over });
  it.each<[string, Session, 'shown' | 'hidden' | 'none', boolean, boolean?]>([
    ['approves a design with no open questions', design(0), 'none', true],
    ['takes the user to the open questions', design(2), 'hidden', true],
    ['leaves it to the footer while the open questions show', design(2), 'shown', false],
    ['leaves it out while the open questions can’t be shown', design(2), 'none', false],
    // A change to one of AgentK's decisions goes with the questions, like an answer.
    ['takes the user to a change to a decision', design(0), 'hidden', true, true],
    ['leaves a change to a decision to the footer while the questions show', design(0), 'shown', false, true],
    ['leaves it out while a change to a decision can’t be shown', design(0), 'none', false, true],
    ['shows nothing before there is a design', design(0, { hasDesign: false }), 'hidden', false],
    ['shows nothing past Design', design(0, { stage: 'code' }), 'hidden', false],
    ['shows nothing for an archived session', design(0, { desired: 'archived' }), 'hidden', false],
  ])('%s', (_, s, questions, shown, changing = false) => {
    expect(headerApproves(s, questions, changing)).toBe(shown);
  });
});

describe('designGate', () => {
  const design = (open: number, over: Partial<Session> = {}) =>
    session({ stage: 'design', desired: 'running', hasDesign: true, designQuestions: { open }, ...over });
  it('reads Design Review and only opens the design, so mid-turn doesn’t hold it up', () => {
    expect(designGate(design(2, { status: 'working' }))).toEqual({
      label: 'Design Review',
      reason: null,
      title: 'Review the design and answer its open questions, then approve it there',
    });
    expect(designGate(design(0))).toEqual({ label: 'Design Review', reason: null, title: 'Review the design, then approve it there' });
  });
  it('waits for a design', () => {
    const reason = 'AgentK hasn’t proposed a design yet.';
    expect(designGate(design(0, { hasDesign: false }))).toEqual({ label: 'Design Review', reason, title: reason });
  });
});

describe('reviewTarget', () => {
  it.each<[string, number, boolean, boolean, 'questions' | 'design' | 'approve']>([
    ['opens the questions while some are open', 2, false, false, 'questions'],
    ['opens the questions while a change to a decision waits', 0, true, false, 'questions'],
    ['opens the design with nothing to answer', 0, false, false, 'design'],
    // Otherwise the click would change nothing: the panel brings up its approve button.
    ['brings up the approve button with the panel open', 2, false, true, 'approve'],
    ['brings up the approve button with the panel open and nothing to answer', 0, false, true, 'approve'],
  ])('%s', (_, open, changing, panelOpen, target) => {
    expect(reviewTarget(open, changing, panelOpen)).toBe(target);
  });
});

describe('updateShowing', () => {
  const pr = (over: Partial<ReviewingPr> = {}) => ({ reviewSha: 'a', headSha: 'b', designUnread: false, ...over }) as ReviewingPr;
  it.each([
    ['not while the state is from before the update', pr(), pr(), false],
    ['once the commit checked out moved', pr(), pr({ reviewSha: 'b' }), true],
    ['once the head moved (the PR had no new commits after all)', pr(), pr({ headSha: 'a' }), true],
    ['once the design is read', pr({ designUnread: true }), pr(), true],
    ['once the session is gone', pr(), null, true],
  ] as const)('%s', (_what, before, now, showing) => {
    expect(updateShowing(before, now)).toBe(showing);
  });
});

describe('the progress of a Discard or Merge & Close', () => {
  const at = (step: LearnProgress['step'], facts = 0, done = 0): LearnProgress => ({ step, facts, done, startedAt: 0 });

  it.each([
    [at('search', 1), 'Looking up 1 fact in Meko'],
    [at('save', 4, 0), 'Saving 1 of 4 to Meko'],
    // Past the last fact, before the step closes.
    [at('save', 4, 4), 'Saving 4 of 4 to Meko'],
  ])('says what the learn step is doing (%o)', (p, text) => {
    expect(learnStepText(p)).toBe(text);
  });

  it.each([
    ['merge', null, 'Waiting for GitHub to merge PR #7'],
    ['learn', at('check', 2), 'Merged. Checking 2 facts against Meko'],
    // A state from before the step's first push.
    ['learn', null, 'Merged. Saving learnings'],
    [null, null, 'Merging and closing'],
  ] as [Session['closeStep'], LearnProgress | null, string][])('says where Merge & Close is (%s)', (closeStep, learnProgress, text) => {
    expect(closingText({ closeStep, learnProgress, prNumber: 7 })).toBe(text);
  });

  it('tells the steps of Merge & Close apart, and none outside it', () => {
    const step = (over: Partial<{ learning: boolean; merged: boolean; inTurn: boolean }>) => closeStep('merge', { learning: false, merged: false, inTurn: false, ...over });
    // Not merged yet, even while asking GitHub (busy) or with Save learnings running.
    expect(step({ inTurn: true, learning: true })).toBe('merge');
    expect(step({ merged: true, inTurn: true })).toBe('turn');
    // Merged with a learn step running: the close's own, or Save learnings' still going. A typed one is a turn too.
    expect(step({ merged: true, learning: true, inTurn: true })).toBe('learn');
    expect(step({ merged: true })).toBe('cleanup');
    expect(closeStep('review', { learning: true, merged: true, inTurn: false })).toBeNull();
  });

  it('counts the time in seconds, then minutes', () => {
    expect([-5, 59_999, 125_000].map(elapsedText)).toEqual(['0s', '59s', '2m 05s']);
  });
});
