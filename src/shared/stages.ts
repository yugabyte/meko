import { KIND_STAGES, STAGE_LABELS, type MoveTarget, type SessionKind, type Stage, type StoredKind } from './types.js';

/** A session's kind as stored and sent (`Session.kind`), as the rules use it: `task` or `review-pr`. */
export function kindOf(kind: StoredKind | SessionKind): SessionKind {
  return kind === 'review-pr' ? 'review-pr' : 'task';
}

/** The stages a session can be moved to by hand, in stage bar order. */
export const MOVE_TARGETS: MoveTarget[] = ['explore', 'design', 'code', 'review'];

/** The stage bar's step for a stage: opening the PR is part of PR & Review. */
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

/** Where the move dialog starts unless a stage is picked: the next step on, or back to Code from PR & Review. */
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

/** True when the move changes the stage bar's step (from the PR stage, PR & Review doesn't). */
export function movesStep(from: Stage, to: MoveTarget): boolean {
  return stepOf(from) !== to;
}

/**
 * Whether a move offers to send the new stage's prompt. PR & Review decides by itself (the PR
 * prompt goes out only when there's work to push).
 */
export function offersPrompt(from: Stage, to: MoveTarget): boolean {
  return to !== 'review' && movesStep(from, to);
}

/** Whether a move sends the new stage's prompt when the request doesn't say. */
export function promptByDefault(from: Stage, to: MoveTarget): boolean {
  if (!offersPrompt(from, to)) return false;
  if (to === 'design') return true;
  // Back from PR & Review, the user usually has something specific in mind: they type it.
  return to === 'code' && (from === 'explore' || from === 'design');
}

/** What a move changes, in plain words, for the move dialog. */
export function moveEffect(from: { stage: Stage; prNumber: number | null }, to: MoveTarget): string {
  const out: string[] = [];
  // The move's prompt carries on from the conversation, not the session's first prompt.
  if (to === 'design') out.push('AgentK designs what this session has worked out so far, and blocks edits to the repo until you approve a design.');
  else if (to === 'code' && promptByDefault(from.stage, to)) {
    out.push(`${from.stage === 'design' ? 'Skips design approval. ' : ''}AgentK implements what this session has worked out so far.`);
  } else if (to === 'review') out.push("AgentK picks up the branch's open PR, or commits, pushes and opens one.");
  else if (to === 'explore') out.push('AgentK runs no design, PR or merge while the session is in Explore.');
  else out.push('The session keeps its worktree, branch and conversation.');
  if (from.prNumber && (from.stage === 'pr' || from.stage === 'review')) {
    out.push(`PR #${from.prNumber} stays open. AgentK stops handling its reviews and CI until you come back to PR & Review.`);
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
