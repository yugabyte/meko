import { describe, expect, it } from 'vitest';
import type { HarnessId } from '../../src/shared/harnesses.js';
import {
  defaultTarget, kindOf, moveBlockedIn, moveEffect, moveError, moveTargets, movesStep, offersPrompt, promptByDefault, skippedSteps,
  startupSteps,
} from '../../src/shared/stages.js';
import type { MoveTarget, Session, Stage, StartupState } from '../../src/shared/types.js';

describe('stage moves', () => {
  it('moves a task, old type names included, to any stage but Merge & Close', () => {
    for (const k of ['explore', 'plan', 'quick', 'task'] as const) {
      expect(kindOf(k)).toBe('task');
      expect(moveTargets(k)).toEqual(['explore', 'design', 'code', 'review']);
    }
    expect(kindOf('review-pr')).toBe('review-pr');
    expect(moveError({ kind: 'quick', stage: 'code' }, 'explore')).toBeNull();
  });

  it('refuses moving out of Merge & Close and Done only', () => {
    const all: Stage[] = ['explore', 'design', 'code', 'pr', 'review', 'merge', 'done'];
    expect(all.filter((s) => moveBlockedIn(s))).toEqual(['merge', 'done']);
  });

  it('accepts every stage but the one the task is in', () => {
    for (const from of ['explore', 'design', 'code', 'review'] as MoveTarget[]) {
      for (const to of ['explore', 'design', 'code', 'review'] as MoveTarget[]) {
        expect(moveError({ kind: 'task', stage: from }, to), `${from} → ${to}`).toEqual(from === to ? expect.any(String) : null);
      }
    }
    expect(moveError({ kind: 'task', stage: 'design' }, 'design')).toBe('Nothing to change: this session is already in Design.');
    // The PR stage is already PR & Review.
    expect(moveError({ kind: 'task', stage: 'pr' }, 'review')).toBe('Nothing to change: this session is already in PR & Review.');
    expect(moveError({ kind: 'task', stage: 'pr' }, 'code')).toBeNull();
  });

  it('opens the move dialog on the next step, or back on Code from PR & Review', () => {
    expect(defaultTarget('explore')).toBe('design');
    expect(defaultTarget('design')).toBe('code');
    expect(defaultTarget('code')).toBe('review');
    expect(defaultTarget('pr')).toBe('code');
    expect(defaultTarget('review')).toBe('code');
  });

  it('offers and defaults the new stage prompt by move', () => {
    expect(movesStep('pr', 'review')).toBe(false);
    expect(offersPrompt('code', 'review')).toBe(false);
    expect(offersPrompt('code', 'code')).toBe(false);
    expect(promptByDefault('code', 'design')).toBe(true);
    expect(promptByDefault('review', 'design')).toBe(true);
    expect(promptByDefault('explore', 'design')).toBe(true);
    expect(promptByDefault('explore', 'code')).toBe(true);
    expect(promptByDefault('design', 'code')).toBe(true);
    expect(promptByDefault('review', 'code')).toBe(false);
    expect(promptByDefault('pr', 'code')).toBe(false);
    expect(promptByDefault('code', 'explore')).toBe(false);
    expect(offersPrompt('code', 'explore')).toBe(true);
  });

  it('says what a move does', () => {
    expect(moveEffect({ stage: 'code', prNumber: null }, 'design')).toBe(
      'AgentK designs what this session has worked out so far, and blocks edits to the repo until you approve a design.');
    expect(moveEffect({ stage: 'design', prNumber: null }, 'code')).toBe('Skips design approval. AgentK implements what this session has worked out so far.');
    expect(moveEffect({ stage: 'explore', prNumber: null }, 'code')).toBe('AgentK implements what this session has worked out so far.');
    expect(moveEffect({ stage: 'explore', prNumber: null }, 'code', true)).toBe('AgentK implements the task you typed.');
    expect(moveEffect({ stage: 'review', prNumber: 12 }, 'code')).toBe(
      'The session keeps its worktree, branch and conversation. PR #12 stays open. AgentK stops handling its reviews and CI until you come back to PR & Review.');
    expect(moveEffect({ stage: 'code', prNumber: null }, 'explore')).toBe('AgentK runs no design, PR or merge while the session is in Explore.');
    expect(moveEffect({ stage: 'explore', prNumber: null }, 'review')).toMatch(/^AgentK picks up the branch's open PR/);
  });

  it('keeps a Review a PR session in its one stage', () => {
    expect(moveTargets('review-pr')).toEqual([]);
    expect(moveBlockedIn('reviewing-pr')).toBe("A Review a PR session can't be moved to another stage.");
    expect(moveError({ kind: 'review-pr', stage: 'reviewing-pr' }, 'code')).toBe("A Review a PR session can't be moved to another stage.");
  });

  it('marks steps the session never went through as skipped', () => {
    // Started at Code: Explore and Design were never entered.
    expect(skippedSteps('task', 'review', ['code', 'pr', 'review'])).toEqual(['explore', 'design']);
    // Started at Design.
    expect(skippedSteps('task', 'code', ['design', 'code'])).toEqual(['explore']);
    // Started at Explore, then Code.
    expect(skippedSteps('task', 'code', ['explore', 'code'])).toEqual(['design']);
    expect(skippedSteps('task', 'pr', ['explore', 'design', 'code', 'pr'])).toEqual([]);
    // Moved back to Explore from Design: nothing before it.
    expect(skippedSteps('task', 'explore', ['design', 'explore'])).toEqual([]);
    expect(skippedSteps('task', 'done', ['code', 'pr', 'review', 'merge', 'done'])).toEqual(['explore', 'design']);
  });
});

describe("the Starting view's steps", () => {
  const session = (o: { harness?: string; kind?: Session['kind']; startup?: Partial<StartupState> | null } = {}) => ({
    harness: (o.harness ?? 'claude') as HarnessId,
    kind: o.kind ?? 'plan',
    startup: o.startup === null ? null : { step: 'launch' as const, stepSince: 0, startedAt: 0, resumed: false, hasPrompt: true, ...o.startup },
  });

  it.each([
    ['claude', 'Starting Claude Code'],
    ['opencode', 'Starting OpenCode'],
    ['kiro', 'Starting Kiro'],
    // An unknown agent (a newer install's) reads as Claude Code, as everywhere else.
    ['newer-agent', 'Starting Claude Code'],
  ])('name the agent a %s session starts', (harness, label) => {
    expect(startupSteps(session({ harness })).find((s) => s.step === 'launch')?.label).toBe(label);
  });

  it('leave out the worktree on a resume, and the prompt when there is none', () => {
    expect(startupSteps(session()).map((s) => s.step)).toEqual(['worktree', 'launch', 'loading', 'prompt']);
    expect(startupSteps(session({ startup: null })).map((s) => s.step)).toEqual(['worktree', 'launch', 'loading', 'prompt']);
    expect(startupSteps(session({ startup: { resumed: true, hasPrompt: false } })).map((s) => s.step)).toEqual(['launch', 'loading']);
    expect(startupSteps(session({ startup: { hasPrompt: false } })).map((s) => s.step)).toEqual(['worktree', 'launch', 'loading']);
  });

  it('say a review fetches the PR in its worktree step', () => {
    expect(startupSteps(session({ kind: 'review-pr' }))[0].label).toBe('Fetching the PR and its design, and preparing the worktree');
    expect(startupSteps(session())[0].label).toBe('Updating the base branch and preparing the worktree');
  });
});
