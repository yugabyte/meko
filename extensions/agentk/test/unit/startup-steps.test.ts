import { describe, expect, it } from 'vitest';
import { HARNESS_IDS, type HarnessId } from '../../src/shared/harnesses.js';
import { startupSteps } from '../../src/shared/stages.js';
import type { Session, StartupState } from '../../src/shared/types.js';

function session(o: { harness?: string; kind?: Session['kind']; startup?: Partial<StartupState> | null } = {}) {
  const startup = o.startup === null ? null : { step: 'launch' as const, stepSince: 0, startedAt: 0, resumed: false, hasPrompt: true, ...o.startup };
  return { harness: (o.harness ?? 'claude') as HarnessId, kind: o.kind ?? 'plan', startup };
}

const launch = (s: ReturnType<typeof session>) => startupSteps(s).find((x) => x.step === 'launch')?.label;

describe("the Starting view's steps", () => {
  it.each([
    ['claude', 'Starting Claude Code'],
    ['opencode', 'Starting OpenCode'],
    ['kiro', 'Starting Kiro'],
  ])('name the agent a %s session starts', (harness, label) => {
    expect(launch(session({ harness }))).toBe(label);
  });

  it('say Claude Code only for Claude Code', () => {
    for (const harness of HARNESS_IDS.filter((h) => h !== 'claude')) {
      expect(startupSteps(session({ harness })).map((s) => s.label).join(' ')).not.toContain('Claude');
    }
  });

  it('read an unknown agent as Claude Code, as everywhere else', () => {
    expect(launch(session({ harness: 'newer-agent' }))).toBe('Starting Claude Code');
  });

  it('list every step for a new task with a first prompt', () => {
    expect(startupSteps(session()).map((s) => s.step)).toEqual(['worktree', 'launch', 'loading', 'prompt']);
    expect(startupSteps(session({ startup: null })).map((s) => s.step)).toEqual(['worktree', 'launch', 'loading', 'prompt']);
  });

  it('leave out the worktree on a resume, and the prompt when there is none', () => {
    expect(startupSteps(session({ startup: { resumed: true, hasPrompt: false } })).map((s) => s.step)).toEqual(['launch', 'loading']);
    expect(startupSteps(session({ startup: { hasPrompt: false } })).map((s) => s.step)).toEqual(['worktree', 'launch', 'loading']);
  });

  it("say a review fetches the PR in its worktree step", () => {
    expect(startupSteps(session({ kind: 'review-pr' }))[0].label).toBe('Fetching the PR and its design, and preparing the worktree');
    expect(startupSteps(session())[0].label).toBe('Updating the base branch and preparing the worktree');
  });
});
