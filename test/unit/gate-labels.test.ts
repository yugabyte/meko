import { describe, expect, it } from 'vitest';
import type { Session } from '../../src/shared/types.js';
import { mergeGate, openPrGate, queuedText } from '../../src/web/util.js';

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
