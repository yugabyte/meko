import { all, get, type Db } from './db.js';
import type {
  CiState, DatapackRef, Project, ProjectSettings, ReviewState, Session, SessionStatus, Stage,
} from '../shared/types.js';

export const DEFAULT_SETTINGS: ProjectSettings = {
  autoContinue: true,
  reviewer: 'agentk',
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
  worktree_path: string;
  tmux_name: string;
  hook_token: string;
  desired: 'running' | 'archived';
  status: SessionStatus;
  status_detail: string | null;
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
}

export function parseSettings(json: string): ProjectSettings {
  try {
    return { ...DEFAULT_SETTINGS, ...(JSON.parse(json) as Partial<ProjectSettings>) };
  } catch {
    return { ...DEFAULT_SETTINGS };
  }
}

export function projectDatapacks(db: Db, projectId: string): DatapackRef[] {
  return all<{ datapack_id: string; name: string; role: 'dedicated' | 'shared' }>(
    db,
    `SELECT datapack_id, name, role FROM project_datapacks WHERE project_id = ?
     ORDER BY CASE role WHEN 'dedicated' THEN 0 ELSE 1 END, name`,
    projectId,
  ).map((r) => ({ id: r.datapack_id, name: r.name, role: r.role }));
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

export function sessionRows(db: Db, projectId?: string): SessionRow[] {
  return projectId
    ? all<SessionRow>(db, 'SELECT * FROM sessions WHERE project_id = ? ORDER BY created_at', projectId)
    : all<SessionRow>(db, 'SELECT * FROM sessions ORDER BY created_at');
}

export function dedicatedDatapack(db: Db, projectId: string): DatapackRef | null {
  return projectDatapacks(db, projectId).find((d) => d.role === 'dedicated') ?? null;
}

export interface SessionRuntime {
  busy: boolean;
  diffStat: { added: number; removed: number } | null;
  canMerge: boolean;
  mergeBlockers: string[];
}

export function toSession(db: Db, r: SessionRow, rt: SessionRuntime): Session {
  const hasDesign = !!get(db, 'SELECT 1 AS x FROM designs WHERE session_id = ?', r.id);
  return {
    id: r.id,
    projectId: r.project_id,
    name: r.name,
    claudeSessionId: r.claude_session_id,
    branch: r.branch,
    baseBranch: r.base_branch,
    worktreePath: r.worktree_path,
    tmuxName: r.tmux_name,
    desired: r.desired,
    status: r.status,
    statusDetail: r.status_detail,
    stage: r.stage,
    prNumber: r.pr_number,
    prUrl: r.pr_url,
    reviewState: r.review_state,
    ciState: r.ci_state,
    canMerge: rt.canMerge,
    mergeBlockers: rt.mergeBlockers,
    busy: rt.busy,
    hasDesign,
    diffStat: rt.diffStat,
    lastEventAt: r.last_event_at,
    createdAt: r.created_at,
    archivedAt: r.archived_at,
    recoveredAt: r.recovered_at,
  };
}

/** Lowercase, [a-z0-9-] only; the same rule Meko uses for agent ids. */
export function slugify(s: string, max = 40): string {
  return s.toLowerCase().replace(/[^a-z0-9-]+/g, '-').replace(/-{2,}/g, '-').replace(/^-+|-+$/g, '').slice(0, max).replace(/-+$/, '') || 'x';
}
