import { commentCounts } from './comments.js';
import { all, get, run, type Db } from './db.js';
import { heldSummary } from './heldreviews.js';
import type {
  StartupState,
  TokenUsage,
  AskRequest, BackgroundTask, CiState, CloseStep, DatapackRef, DiffStat, LearnProgress, Project, ProjectSettings, QueuedGate, ReviewState, ReviewingPr, Session, SessionStatus, Stage, StoredKind,
} from '../shared/types.js';
import { KIND_STAGES, REVIEWER_LABELS } from '../shared/types.js';
import { kindOf, skippedSteps, stepOf } from '../shared/stages.js';
import { DEFAULT_BRANCH_PREFIX } from '../shared/branches.js';
import { harnessOf } from '../shared/harnesses.js';
import { readOverrides, type CodeReviewSummary, type ReviewOverrides } from '../shared/codereview.js';

/**
 * What new task sessions store in `sessions.kind` (see StoredKind): a name a release or page from
 * before start stages can still index its stage lists with, after a rollback or before a reload.
 */
export const STORED_TASK_KIND: StoredKind = 'plan';

export const DEFAULT_SETTINGS: ProjectSettings = {
  autoContinue: true,
  reviewer: 'mine',
  branchPrefix: DEFAULT_BRANCH_PREFIX,
  mergeMethod: 'repo-default',
  notify: true,
  mekoLog: false,
  codeReview: {},
};

export interface ProjectRow {
  id: string;
  name: string;
  repo_path: string;
  default_branch: string;
  position: number;
  settings_json: string;
  meko_conversation_id: string | null;
  created_at: number;
  deleted_at: number | null;
}

export interface SessionRow {
  id: string;
  project_id: string;
  slug: string;
  name: string;
  /** The harness the session runs on (`HarnessId`; `harnessOf()` reads an unknown one as Claude Code). */
  harness: string;
  /** AgentK's key for the conversation: Claude Code's session id, and the Meko conversation's key for every harness. */
  claude_session_id: string;
  /** The conversation id a harness picks itself (Kiro's session id), once AgentK has learned it; null for Claude Code. */
  agent_session_id: string | null;
  transcript_path: string | null;
  branch: string;
  base_branch: string;
  /** The repo the session works in: the project's, or another one picked when it started. */
  repo_path: string | null;
  /** 1 when the session started on a branch that already existed. */
  branch_reused: number;
  worktree_path: string;
  /** 1 once the session let go of its worktree (idle, another session's, or removed). */
  worktree_released: number;
  /** The session's pane name in the session host (the column kept its name from when panes were tmux sessions). */
  tmux_name: string;
  hook_token: string;
  desired: 'running' | 'archived';
  status: SessionStatus;
  status_detail: string | null;
  kind: StoredKind;
  stage: Stage;
  pr_number: number | null;
  pr_url: string | null;
  review_state: ReviewState;
  ci_state: CiState;
  merge_state: string | null;
  seen_review_ids: string;
  initial_prompt: string | null;
  auto_continue_at: string;
  last_event_at: number | null;
  created_at: number;
  archived_at: number | null;
  recovered_at: number | null;
  interrupted: number;
  /** 1 for sessions whose Design stage runs in Claude's plan mode (created before designs moved out of it). */
  plan_mode: number;
  position: number;
  /** Prompts waiting for Claude: JSON map of prompt name to { state, vars }. */
  pending_prompts: string;
  /** The PR head that last got a CI-fix prompt (or was handed to the user after the cap). */
  ci_fix_head: string | null;
  /** CI-fix prompts for consecutive failing heads since CI was last green. */
  ci_fixes: number;
  /** The PR head that last got a prompt to resolve merge conflicts (or was handed to the user after the cap). */
  conflict_head: string | null;
  /** Conflict prompts for consecutive conflicting heads since the PR last merged cleanly. */
  conflict_fixes: number;
  /** The dedicated datapack the session was last launched with (see sessionDatapack). */
  meko_datapack_id: string | null;
  meko_datapack_name: string | null;
  /** The Meko agent id it was launched with (launchedAgentId); NULL from before sessions had their own. */
  meko_agent_id: string | null;
  // Review a PR sessions only (NULL otherwise): see ReviewingPr.
  /** The PR's repo on GitHub, `owner/name`. */
  pr_repo: string | null;
  pr_title: string | null;
  pr_author: string | null;
  /** The commit checked out for the review. */
  review_sha: string | null;
  /** The PR's head at the last look at GitHub. */
  pr_head_sha: string | null;
  /** Commits on the PR since review_sha; NULL when its history was rewritten. */
  pr_new_commits: number | null;
  pr_state: 'OPEN' | 'CLOSED' | 'MERGED' | null;
  /** The clone's remote the PR is fetched from (`origin`, or `upstream` for a fork). */
  pr_remote: string | null;
  /** The user's notes for AgentK, given when the review started. */
  review_notes: string | null;
  /** The images added in the New session dialog that reached the session: a JSON list of paths. */
  start_images: string | null;
  /** A gate pressed before it could go (Session.queued). */
  queued_gate: QueuedGate | null;
  /** The user's instructions for a queued Open PR, sent after its prompt; cleared with queued_gate. */
  queued_instructions: string | null;
  /** 1 while Run to merge is on (Session.autoRun). */
  auto_run: number;
  /** 1 when Run to merge queued `queued_gate`, rather than the user's press; cleared with it. */
  queued_auto: number;
  /** Discard was asked for and waits for the learn step (Session.discarding). */
  discarding: number;
  /** The user asked Merge & Close to skip the learn step. */
  skip_learn: number;
  /**
   * A task started on a PR whose head is on another remote, or under another name, than origin's
   * branch of the session's name (a fork's PR): where its pushes go. NULL: origin.
   */
  push_remote: string | null;
  /** The branch on push_remote (the PR's head branch); NULL: the session's branch. */
  push_branch: string | null;
  /**
   * 1: it works on a PR's head on a public repo (or one gh couldn't read), so it runs without the
   * repo's own config (headtrust.ts). 0: trusted. NULL: no PR head, or not checked yet (at every launch).
   */
  untrusted: number | null;
  /** What it's for beyond its kind (`SessionPurpose`: 'learn'); NULL for the rest. */
  purpose: string | null;
  /** A code-review session's task (Session.parentId); NULL for every other session. */
  parent_id: string | null;
  /** The session's own local review settings (ReviewOverrides, JSON); NULL for none. */
  review_json: string | null;
}

export function parseSettings(json: string): ProjectSettings {
  let s: ProjectSettings;
  try {
    s = { ...DEFAULT_SETTINGS, ...(JSON.parse(json) as Partial<ProjectSettings>) };
  } catch {
    return { ...DEFAULT_SETTINGS };
  }
  // A mode this version doesn't know (from a newer one) falls back to the default.
  if (!Object.hasOwn(REVIEWER_LABELS, s.reviewer)) s.reviewer = DEFAULT_SETTINGS.reviewer;
  // The AgentK reviewer's login, gone since migration 47, from a page loaded before it.
  delete (s as { reviewerLogin?: unknown }).reviewerLogin;
  s.codeReview = readOverrides(s.codeReview);
  return s;
}

export function projectDatapacks(db: Db, projectId: string): DatapackRef[] {
  return all<{ datapack_id: string; name: string; role: 'dedicated' | 'shared' }>(
    db,
    `SELECT datapack_id, name, role FROM project_datapacks WHERE project_id = ?
     ORDER BY CASE role WHEN 'dedicated' THEN 0 ELSE 1 END, name`,
    projectId,
  ).map((r) => ({ id: r.datapack_id, name: r.name, role: r.role }));
}

/** The datapacks of every open project, dedicated and shared, each once. */
export function openProjectDatapacks(db: Db): { id: string; name: string }[] {
  return all<{ datapack_id: string; name: string }>(
    db,
    `SELECT d.datapack_id, MIN(d.name) AS name FROM project_datapacks d JOIN projects p ON p.id = d.project_id
     WHERE p.deleted_at IS NULL GROUP BY d.datapack_id ORDER BY name`,
  ).map((r) => ({ id: r.datapack_id, name: r.name }));
}

export function toProject(db: Db, r: ProjectRow): Project {
  return {
    id: r.id,
    name: r.name,
    repoPath: r.repo_path,
    defaultBranch: r.default_branch,
    position: r.position,
    settings: parseSettings(r.settings_json),
    datapacks: projectDatapacks(db, r.id),
    createdAt: r.created_at,
  };
}

export function projectRow(db: Db, id: string): ProjectRow | undefined {
  return get<ProjectRow>(db, 'SELECT * FROM projects WHERE id = ? AND deleted_at IS NULL', id);
}

export function projectRows(db: Db): ProjectRow[] {
  return all<ProjectRow>(db, 'SELECT * FROM projects WHERE deleted_at IS NULL ORDER BY position, created_at');
}

export function sessionRow(db: Db, id: string): SessionRow | undefined {
  return get<SessionRow>(db, 'SELECT * FROM sessions WHERE id = ?', id);
}

/**
 * The repo a session works in. Every session has its own since it may use another repo than
 * its project's; a row without one (never expected) falls back to the project's, deleted or not.
 */
export function sessionRepo(db: Db, r: SessionRow): string | null {
  return r.repo_path ?? get<{ repo_path: string }>(db, 'SELECT repo_path FROM projects WHERE id = ?', r.project_id)?.repo_path ?? null;
}

/**
 * The datapack AgentK writes to for a session: the dedicated one it was launched with, which it
 * keeps until it's launched again (a switch leaves running sessions on the old one), else the
 * project's dedicated datapack.
 */
export function sessionDatapack(db: Db, r: SessionRow): DatapackRef | null {
  if (r.meko_datapack_id && r.meko_datapack_name) return { id: r.meko_datapack_id, name: r.meko_datapack_name, role: 'dedicated' };
  return dedicatedDatapack(db, r.project_id);
}

export function sessionRows(db: Db, projectId?: string): SessionRow[] {
  return projectId
    ? all<SessionRow>(db, 'SELECT * FROM sessions WHERE project_id = ? ORDER BY created_at', projectId)
    : all<SessionRow>(db, 'SELECT * FROM sessions ORDER BY created_at');
}

export function dedicatedDatapack(db: Db, projectId: string): DatapackRef | null {
  return projectDatapacks(db, projectId).find((d) => d.role === 'dedicated') ?? null;
}

/** Whether the Merge gate can be passed now, why not, and whether the merge can be queued (Session.mergeQueueable). */
export interface MergeInfo {
  canMerge: boolean;
  blockers: string[];
  queueable: boolean;
  /** The blockers that don't clear by themselves (a part of `blockers`). */
  hard: string[];
  /** The blockers only the user can clear (a part of `hard`; Session.waitsOnYou). */
  yours: string[];
  /** `yours`, and while Run to merge is on, the other hard blockers it waits on the user for (lifecycle.ts mergeInfo). */
  yoursWithAutoRun: string[];
}

export interface SessionRuntime {
  usage: TokenUsage;
  startup: StartupState | null;
  busy: boolean;
  diffStat: DiffStat | null;
  canMerge: boolean;
  mergeBlockers: string[];
  mergeQueueable: boolean;
  /** MergeInfo's `yours` and `yoursWithAutoRun`; `toSession` picks by `auto_run`. */
  yours: string[];
  yoursWithAutoRun: string[];
  contextPercent: number | null;
  modLive: boolean;
  midTurn: boolean;
  promptsHeld: number;
  asking: AskRequest | null;
  mekoCall: boolean;
  background: BackgroundTask[];
  approvalQueued: boolean;
  /** The task's latest local review, or a code-review session's own (review/runner.ts). */
  codeReview: CodeReviewSummary | null;
}

/** A session's own local review settings, from `review_json`. */
export function sessionReview(r: Pick<SessionRow, 'review_json'>): ReviewOverrides {
  if (!r.review_json) return {};
  try {
    return readOverrides(JSON.parse(r.review_json));
  } catch {
    return {};
  }
}

/**
 * Where a session in Merge & Close is (`Session.closeStep`): not merged yet, whatever else runs (a
 * Save learnings step can run while GitHub hasn't merged it); then learning, the close's own step
 * or Save learnings' still running, before a turn, since a typed learn step is one; then merged
 * but in a turn (the close waits for its end, Lifecycle.merged); else cleaning up. Null in every
 * other stage.
 */
export function closeStep(stage: Stage, s: { learning: boolean; merged: boolean; inTurn: boolean }): CloseStep | null {
  if (stage !== 'merge') return null;
  if (!s.merged) return 'merge';
  if (s.learning) return 'learn';
  return s.inTurn ? 'turn' : 'cleanup';
}

export function toSession(db: Db, r: SessionRow, rt: SessionRuntime, openQuestions: number, learnProgress: LearnProgress | null): Session {
  const design = get<{ updated_at: number; source: string | null; source_told: string | null }>(db, 'SELECT updated_at, source, source_told FROM designs WHERE session_id = ?', r.id);
  return {
    id: r.id,
    projectId: r.project_id,
    name: r.name,
    harness: harnessOf(r.harness),
    claudeSessionId: r.claude_session_id,
    branch: r.branch,
    baseBranch: r.base_branch,
    branchReused: !!r.branch_reused,
    repoPath: sessionRepo(db, r) ?? '',
    worktreePath: r.worktree_path,
    paneName: r.tmux_name,
    desired: r.desired,
    status: r.status,
    statusDetail: r.status_detail,
    kind: r.kind,
    stage: r.stage,
    hasTask: !!r.initial_prompt,
    purpose: r.purpose === 'learn' ? 'learn' : null,
    prNumber: r.pr_number,
    prUrl: r.pr_url,
    reviewState: r.review_state,
    ciState: r.ci_state,
    canMerge: rt.canMerge,
    mergeBlockers: rt.mergeBlockers,
    mergeQueueable: rt.mergeQueueable,
    // Read from the row, so turning Run to merge off or on shows before the next look at the PR.
    waitsOnYou: r.auto_run ? rt.yoursWithAutoRun : rt.yours,
    queued: r.queued_gate,
    queuedInstructions: r.queued_gate === 'open-pr' ? r.queued_instructions : null,
    autoRun: !!r.auto_run,
    busy: rt.busy,
    skippedStages: skippedStages(db, r),
    hasDesign: !!design,
    designUpdatedAt: design?.updated_at ?? null,
    diffStat: rt.diffStat,
    comments: commentCounts(db, r.id),
    designQuestions: { open: openQuestions },
    heldReviews: heldSummary(db, r.id, r.pr_number),
    reviewingPr: reviewingPr(r, design ?? null),
    parentId: r.parent_id,
    codeReview: rt.codeReview,
    review: sessionReview(r),
    usage: rt.usage,
    contextPercent: rt.contextPercent,
    modLive: rt.modLive,
    midTurn: rt.midTurn,
    promptsHeld: rt.promptsHeld,
    asking: rt.asking,
    mekoCall: rt.mekoCall,
    background: rt.background,
    approvalQueued: rt.approvalQueued,
    learning: !!learnProgress,
    learnProgress,
    closeStep: closeStep(r.stage, {
      learning: !!learnProgress,
      merged: r.stage === 'merge' && !!get(db, "SELECT 1 AS x FROM session_events WHERE session_id = ? AND kind = 'merged'", r.id),
      inTurn: r.status === 'working' || r.status === 'starting' || rt.busy,
    }),
    discarding: !!r.discarding,
    startup: r.status === 'starting' ? rt.startup : null,
    lastEventAt: r.last_event_at,
    position: r.position,
    workItemId: get<{ id: string }>(db, 'SELECT id FROM work_items WHERE session_id = ?', r.id)?.id ?? null,
    createdAt: r.created_at,
    archivedAt: r.archived_at,
    recoveredAt: r.recovered_at,
  };
}

/**
 * A Review a PR session's PR, or null for other kinds (and a row missing the PR, never expected).
 * `design` is its copy of the PR's design: unread while Claude hasn't been pointed at that version.
 */
function reviewingPr(r: SessionRow, design: { source: string | null; source_told: string | null } | null): ReviewingPr | null {
  if (r.kind !== 'review-pr' || !r.pr_repo || !r.review_sha) return null;
  let unread = false;
  try {
    unread = !!design?.source && (JSON.parse(design.source) as { sha256?: unknown }).sha256 !== design.source_told;
  } catch {
    /* a source that isn't JSON: nothing to read */
  }
  return {
    repo: r.pr_repo,
    title: r.pr_title ?? '',
    author: r.pr_author ?? '',
    reviewSha: r.review_sha,
    headSha: r.pr_head_sha ?? r.review_sha,
    newCommits: r.pr_new_commits,
    prState: r.pr_state ?? 'OPEN',
    designUnread: unread,
  };
}

/**
 * Steps before the current one that the session never went through. Only a session past its
 * kind's first step can have one, so the stage history is read only then.
 */
function skippedStages(db: Db, r: SessionRow): Stage[] {
  const kind = kindOf(r.kind);
  if (r.desired !== 'running' || (KIND_STAGES[kind] as Stage[]).indexOf(stepOf(r.stage)) <= 0) return [];
  const visited = all<{ stage: Stage }>(db, 'SELECT DISTINCT stage FROM session_stages WHERE session_id = ?', r.id);
  return skippedSteps(kind, r.stage, visited.map((x) => x.stage));
}

/** Lowercase, [a-z0-9-] only; the same rule Meko uses for agent ids. */
export function slugify(s: string, max = 40): string {
  return s.toLowerCase().replace(/[^a-z0-9-]+/g, '-').replace(/-{2,}/g, '-').replace(/^-+|-+$/g, '').slice(0, max).replace(/-+$/, '') || 'x';
}

/**
 * Drops session events older than 30 days, except merges: the Usage page counts merged PRs
 * from them, for all time.
 */
export function pruneSessionEvents(db: Db, now = Date.now()): void {
  run(db, `DELETE FROM session_events WHERE at < ? AND kind != 'merged'`, now - 30 * 24 * 3600 * 1000);
}
