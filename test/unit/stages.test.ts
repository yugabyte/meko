import { describe, expect, it } from 'vitest';
import {
  defaultStage, moveBlockedIn, moveEffect, moveError, moveTargets, movesStep, offersPrompt, promptByDefault, skippedSteps, stepOf,
} from '../../src/shared/stages.js';
import { KIND_STAGES, SESSION_KINDS, STAGE_LABELS, type MoveTarget, type Stage } from '../../src/shared/types.js';

describe('stage moves', () => {
  it('shows the PR stage as part of PR & Review', () => {
    expect(stepOf('pr')).toBe('review');
    expect(stepOf('code')).toBe('code');
    expect(STAGE_LABELS.pr).toBe('PR & Review');
    expect(STAGE_LABELS.review).toBe('PR & Review');
  });

  it('moves to the stages each type has, never Merge & Close', () => {
    expect(moveTargets('explore')).toEqual(['explore']);
    expect(moveTargets('plan')).toEqual(['design', 'code', 'review']);
    expect(moveTargets('quick')).toEqual(['code', 'review']);
  });

  it('refuses moving out of Merge & Close and Done only', () => {
    const all: Stage[] = ['explore', 'design', 'code', 'pr', 'review', 'merge', 'done'];
    expect(all.filter((s) => moveBlockedIn(s))).toEqual(['merge', 'done']);
  });

  it('accepts exactly the pairs a type has, except staying where it is', () => {
    const from = { kind: 'quick' as const, stage: 'code' as Stage };
    for (const kind of SESSION_KINDS) {
      for (const stage of ['explore', 'design', 'code', 'review'] as MoveTarget[]) {
        const valid = (KIND_STAGES[kind] as Stage[]).includes(stage) && !(kind === 'quick' && stage === 'code');
        expect(moveError(from, { kind, stage }), `${kind}/${stage}`).toEqual(valid ? null : expect.any(String));
      }
    }
    expect(moveError({ kind: 'plan', stage: 'design' }, { kind: 'quick', stage: 'design' })).toBe('Quick Code sessions have no Design stage.');
    // The PR stage is already PR & Review; changing the type there is fine.
    expect(moveError({ kind: 'quick', stage: 'pr' }, { kind: 'quick', stage: 'review' })).toBe('Nothing to change: this Quick Code session is already in PR & Review.');
    expect(moveError({ kind: 'quick', stage: 'pr' }, { kind: 'plan', stage: 'review' })).toBeNull();
  });

  it('lands on the current step when the new type has it, else on its first', () => {
    expect(defaultStage('plan', 'code')).toBe('code');
    expect(defaultStage('plan', 'pr')).toBe('review');
    expect(defaultStage('plan', 'explore')).toBe('design');
    expect(defaultStage('quick', 'design')).toBe('code');
    expect(defaultStage('explore', 'review')).toBe('explore');
  });

  it('offers and defaults the new stage prompt by move', () => {
    expect(movesStep('pr', 'review')).toBe(false);
    expect(offersPrompt('code', 'review')).toBe(false);
    expect(offersPrompt('code', 'code')).toBe(false);
    expect(promptByDefault('code', 'design')).toBe(true);
    expect(promptByDefault('review', 'design')).toBe(true);
    expect(promptByDefault('explore', 'code')).toBe(true);
    expect(promptByDefault('design', 'code')).toBe(true);
    expect(promptByDefault('review', 'code')).toBe(false);
    expect(promptByDefault('pr', 'code')).toBe(false);
    expect(promptByDefault('code', 'explore')).toBe(false);
    expect(offersPrompt('code', 'explore')).toBe(true);
  });

  it('says what a move does', () => {
    expect(moveEffect({ stage: 'code', prNumber: null }, 'design')).toBe('AgentK blocks edits to the repo until you approve a design.');
    expect(moveEffect({ stage: 'design', prNumber: null }, 'code')).toBe('Skips design approval.');
    expect(moveEffect({ stage: 'review', prNumber: 12 }, 'code')).toBe(
      'The session keeps its worktree, branch and conversation. PR #12 stays open. AgentK stops handling its reviews and CI until you come back to PR & Review.');
    expect(moveEffect({ stage: 'pr', prNumber: null }, 'review')).toBe('Only the type changes. The session stays in PR & Review.');
    expect(moveEffect({ stage: 'explore', prNumber: null }, 'review')).toMatch(/^AgentK picks up the branch's open PR/);
  });

  it('marks steps the session never went through as skipped', () => {
    expect(skippedSteps('plan', 'code', ['code'])).toEqual(['design']);
    expect(skippedSteps('plan', 'code', ['design', 'code'])).toEqual([]);
    expect(skippedSteps('quick', 'review', ['explore', 'pr', 'review'])).toEqual(['code']);
    expect(skippedSteps('quick', 'pr', ['code', 'pr'])).toEqual([]);
    expect(skippedSteps('explore', 'explore', ['explore'])).toEqual([]);
    expect(skippedSteps('plan', 'done', ['code', 'pr', 'review', 'merge', 'done'])).toEqual(['design']);
  });
});
