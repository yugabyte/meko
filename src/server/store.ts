import { commentCounts } from './comments.js';
import { all, get, run, type Db } from './db.js';
import type {
  StartupState,
  TokenUsage,
  AskRequest, CiState, DatapackRef, Project, ProjectSettings, ReviewState, ReviewingPr, Session, SessionStatus, Stage, StoredKind,
} from '../shared/types.js';
import { KIND_STAGES, REVIEWER_LABELS } from '../shared/types.js';
import { kindOf, skippedSteps, stepOf } from '../shared/stages.js';

/**
 * What new task sessions store in `sessions.kind` (see StoredKind): a name a release or page from
 * before start stages can still index its stage lists with, after a rollback or before a reload.
 */
export const STORED_TASK_KIND: StoredKind = 'plan';

export const DEFAULT_SETTINGS: ProjectSettings = {
  autoContinue: true,
  reviewer: 'agentk-people',
  reviewerLogin: 'yb-agentk-dev',
  branchPrefix: 'ak/',
  mergeMethod: 'repo-default',
  notify: true,
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
  claude_session_id: string;
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
  agentk_seen: number;
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
  /**
   * A task started on a PR whose head is on another remote, or under another name, than origin's
   * branch of the session's name (a fork's PR): where its pushes go. NULL: origin.
   */
  push_remote: string | null;
  /** The branch on push_remote (the PR's head branch); NULL: the session's branch. */
  push_branch: string | null;
}

export function parseSettings(json: string): ProjectSettings {
  let s: ProjectSettings;
  try {
    s = { ...DEFAULT_SETTINGS, ...(JSON.parse(json) as Partial<ProjectSettings>) };
  } catch {
    return { ...DEFAULT_SETTINGS };
  }
  // A mode from an older version ('agentk') falls back to the default.
  if (!Object.hasOwn(REVIEWER_LABELS, s.reviewer)) s.reviewer = DEFAULT_SETTINGS.reviewer;
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

export interface SessionRuntime {
  usage: TokenUsage;
  startup: StartupState | null;
  busy: boolean;
  diffStat: { added: number; removed: number } | null;
  canMerge: boolean;
  mergeBlockers: string[];
  contextPercent: number | null;
  modLive: boolean;
  midTurn: boolean;
  promptsHeld: number;
  asking: AskRequest | null;
  approvalQueued: boolean;
}

export function toSession(db: Db, r: SessionRow, rt: SessionRuntime, openQuestions: number): Session {
  const design = get<{ updated_at: number }>(db, 'SELECT updated_at FROM designs WHERE session_id = ?', r.id);
  return {
    id: r.id,
    projectId: r.project_id,
    name: r.name,
    claudeSessionId: r.claude_session_id,
    branch: r.branch,
    baseBranch: r.base_branch,
    branchReused: !!r.branch_reused,
    repoPath: sessionRepo(db, r) ?? '',
    worktreePath: r.worktree_path,
    tmuxName: r.tmux_name,
    desired: r.desired,
    status: r.status,
    statusDetail: r.status_detail,
    kind: r.kind,
    stage: r.stage,
    hasTask: !!r.initial_prompt,
    prNumber: r.pr_number,
    prUrl: r.pr_url,
    reviewState: r.review_state,
    ciState: r.ci_state,
    agentkSeen: !!r.agentk_seen,
    canMerge: rt.canMerge,
    mergeBlockers: rt.mergeBlockers,
    busy: rt.busy,
    skippedStages: skippedStages(db, r),
    hasDesign: !!design,
    designUpdatedAt: design?.updated_at ?? null,
    diffStat: rt.diffStat,
    comments: commentCounts(db, r.id),
    designQuestions: { open: openQuestions },
    reviewingPr: reviewingPr(r),
    usage: rt.usage,
    contextPercent: rt.contextPercent,
    modLive: rt.modLive,
    midTurn: rt.midTurn,
    promptsHeld: rt.promptsHeld,
    asking: rt.asking,
    approvalQueued: rt.approvalQueued,
    startup: r.status === 'starting' ? rt.startup : null,
    lastEventAt: r.last_event_at,
    createdAt: r.created_at,
    archivedAt: r.archived_at,
    recoveredAt: r.recovered_at,
  };
}

/** A Review a PR session's PR, or null for other kinds (and a row missing the PR, never expected). */
function reviewingPr(r: SessionRow): ReviewingPr | null {
  if (r.kind !== 'review-pr' || !r.pr_repo || !r.review_sha) return null;
  return {
    repo: r.pr_repo,
    title: r.pr_title ?? '',
    author: r.pr_author ?? '',
    reviewSha: r.review_sha,
    headSha: r.pr_head_sha ?? r.review_sha,
    newCommits: r.pr_new_commits,
    prState: r.pr_state ?? 'OPEN',
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
