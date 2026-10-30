import { HARNESS_CAPS, harnessOf } from './harnesses.js';
import {
  KIND_STAGES, STAGE_LABELS, type MoveTarget, type Session, type SessionKind, type Stage, type StartupStep, type StoredKind,
} from './types.js';

/** A session's kind as stored and sent (`Session.kind`), as the rules use it: `task`, `review-pr` or `code-review`. */
export function kindOf(kind: StoredKind | SessionKind): SessionKind {
  return kind === 'review-pr' || kind === 'code-review' ? kind : 'task';
}

/**
 * The steps the Starting view lists for a session, naming the agent it runs on: no worktree step
 * when it resumes, and no prompt step when it has no first prompt.
 */
export function startupSteps(s: Pick<Session, 'harness' | 'kind' | 'startup'>): { step: StartupStep; label: string }[] {
  const st = s.startup;
  const steps: { step: StartupStep; label: string }[] = [];
  if (!st?.resumed) {
    steps.push({
      step: 'worktree',
      label: kindOf(s.kind) === 'review-pr'
        ? 'Fetching the PR and its design, and preparing the worktree'
        : 'Updating the base branch and preparing the worktree',
    });
  }
  steps.push(
    { step: 'launch', label: `Starting ${HARNESS_CAPS[harnessOf(s.harness)].label}` },
    { step: 'loading', label: 'Loading plugins, MCP servers and hooks' },
  );
  if (!st || st.hasPrompt) steps.push({ step: 'prompt', label: 'Sending your task' });
  return steps;
}

/** The stages a session can be moved to by hand, in stage bar order. */
export const MOVE_TARGETS: MoveTarget[] = ['explore', 'design', 'code', 'review'];

/** The stage bar's step for a stage: opening the PR is part of Review & PR. */
export function stepOf(stage: Stage): Stage {
  return stage === 'pr' ? 'review' : stage;
}

/** Why a session in this stage can't be moved by hand, or null when it can. */
export function moveBlockedIn(stage: Stage): string | null {
  if (stage === 'reviewing-pr') return "A Review a PR session can't be moved to another stage.";
  if (stage === 'merge') return "This session is merging and closing, so it can't be moved.";
  if (stage === 'done') return "This session is done, so it can't be moved.";
  return null;
}

/** The stages a session of this kind can be moved to. */
export function moveTargets(kind: StoredKind | SessionKind): MoveTarget[] {
  return MOVE_TARGETS.filter((t) => (KIND_STAGES[kindOf(kind)] as Stage[]).includes(t));
}

/** Where the move dialog starts unless a stage is picked: the next step on, or back to Code from Review & PR. */
export function defaultTarget(current: Stage): MoveTarget {
  const step = stepOf(current);
  if (step === 'explore') return 'design';
  if (step === 'design') return 'code';
  if (step === 'code') return 'review';
  return 'code';
}

/** Why the session can't be moved to this stage from where it is, or null. */
export function moveError(from: { kind: StoredKind | SessionKind; stage: Stage }, to: MoveTarget): string | null {
  if (!moveTargets(from.kind).includes(to)) return moveBlockedIn('reviewing-pr');
  if (stepOf(from.stage) === to) return `Nothing to change: this session is already in ${STAGE_LABELS[to]}.`;
  return null;
}

/** True when the move changes the stage bar's step (from the PR stage, Review & PR doesn't). */
export function movesStep(from: Stage, to: MoveTarget): boolean {
  return stepOf(from) !== to;
}

/**
 * Whether a move offers to send the new stage's prompt. Review & PR decides by itself (the PR
 * prompt goes out only when there's work to push).
 */
export function offersPrompt(from: Stage, to: MoveTarget): boolean {
  return to !== 'review' && movesStep(from, to);
}

/** Whether a move sends the new stage's prompt when the request doesn't say. */
export function promptByDefault(from: Stage, to: MoveTarget): boolean {
  if (!offersPrompt(from, to)) return false;
  if (to === 'design') return true;
  // Back from Review & PR, the user usually has something specific in mind: they type it.
  return to === 'code' && (from === 'explore' || from === 'design');
}

/**
 * What a move changes, in plain words, for the move dialog. `task`: the user typed a task for a
 * session without one, which the stage's prompt then names; otherwise the move's prompt carries on
 * from the conversation, not the session's first prompt.
 */
export function moveEffect(from: { stage: Stage; prNumber: number | null }, to: MoveTarget, task = false): string {
  const out: string[] = [];
  const work = task ? 'the task you typed' : 'what this session has worked out so far';
  if (to === 'design') out.push(`AgentK designs ${work}, and blocks edits to the repo until you approve a design.`);
  else if (to === 'code' && promptByDefault(from.stage, to)) {
    out.push(`${from.stage === 'design' ? 'Skips design approval. ' : ''}AgentK implements ${work}.`);
  } else if (to === 'review') out.push("AgentK picks up the branch's open PR, or opens one: with the local review on, it reviews the change in Code first.");
  else if (to === 'explore') out.push('AgentK runs no design, PR or merge while the session is in Explore.');
  else out.push('The session keeps its worktree, branch and conversation.');
  if (from.prNumber && (from.stage === 'pr' || from.stage === 'review')) {
    out.push(`PR #${from.prNumber} stays open. AgentK stops handling its reviews and CI until you come back to Review & PR.`);
  }
  return out.join(' ');
}

/** Steps before the current one that the session never entered (the stage bar shows them as skipped). */
export function skippedSteps(kind: SessionKind, stage: Stage, visited: Stage[]): Stage[] {
  const steps: Stage[] = KIND_STAGES[kind];
  const seen = new Set(visited.map(stepOf));
  const idx = stage === 'done' ? steps.length : steps.indexOf(stepOf(stage));
  return steps.slice(0, Math.max(idx, 0)).filter((s) => !seen.has(s));
}
