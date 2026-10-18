import { describe, expect, it } from 'vitest';
import type { ReviewingPr, Session } from '../../src/shared/types.js';
import { headerApproves, mergeGate, openPrGate, queuedText, updateShowing } from '../../src/web/util.js';

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
