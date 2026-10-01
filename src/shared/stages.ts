import { KIND_LABELS, KIND_STAGES, STAGE_LABELS, type MoveTarget, type SessionKind, type Stage } from './types.js';

/** The stages a session can be moved to by hand, in stage bar order. */
export const MOVE_TARGETS: MoveTarget[] = ['explore', 'design', 'code', 'review'];

/** The stage bar's step for a stage: opening the PR is part of PR & Review. */
export function stepOf(stage: Stage): Stage {
  return stage === 'pr' ? 'review' : stage;
}

/** Why a session in this stage can't be moved by hand, or null when it can. */
export function moveBlockedIn(stage: Stage): string | null {
  if (stage === 'reviewing-pr') return "A Review a PR session can't change type or stage.";
  if (stage === 'merge') return "This session is merging and closing, so it can't be moved.";
  if (stage === 'done') return "This session is done, so it can't be moved.";
  return null;
}

/** The stages a session of this kind can be moved to. */
export function moveTargets(kind: SessionKind): MoveTarget[] {
  return MOVE_TARGETS.filter((t) => (KIND_STAGES[kind] as Stage[]).includes(t));
}

/** Where a move to this kind lands unless another stage is picked: the current step if the kind has it, else its first. */
export function defaultStage(kind: SessionKind, current: Stage): MoveTarget {
  const targets = moveTargets(kind);
  const step = stepOf(current);
  return targets.find((t) => t === step) ?? targets[0];
}

/** Why this kind and stage don't make a valid move from where the session is, or null. */
export function moveError(from: { kind: SessionKind; stage: Stage }, to: { kind: SessionKind; stage: MoveTarget }): string | null {
  if (to.kind === 'review-pr') return 'A session can\'t become a Review a PR session. Start a new session and pick the PR.';
  if (!moveTargets(to.kind).includes(to.stage)) return `${KIND_LABELS[to.kind]} sessions have no ${STAGE_LABELS[to.stage]} stage.`;
  if (from.kind === to.kind && stepOf(from.stage) === to.stage) return `Nothing to change: this ${KIND_LABELS[to.kind]} session is already in ${STAGE_LABELS[to.stage]}.`;
  return null;
}

/** True when the move changes the stage bar's step, not just the type. */
export function movesStep(from: Stage, to: MoveTarget): boolean {
  return stepOf(from) !== to;
}

/**
 * Whether a move offers to send the new stage's prompt. PR & Review decides by itself (the PR
 * prompt goes out only when there's work to push), and a type-only change sends nothing.
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
  if (!movesStep(from.stage, to)) return `Only the type changes. The session stays in ${STAGE_LABELS[to]}.`;
  const out: string[] = [];
  if (to === 'design') out.push('AgentK blocks edits to the repo until you approve a design.');
  else if (to === 'code' && from.stage === 'design') out.push('Skips design approval.');
  else if (to === 'review') out.push("AgentK picks up the branch's open PR, or commits, pushes and opens one.");
  else if (to === 'explore') out.push('AgentK runs no design, PR or merge stages for this session.');
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
