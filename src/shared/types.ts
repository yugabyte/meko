// API contract shared by the server (src/server) and the web UI (src/web).
// Every REST route and WebSocket message is described here.

export type SessionStatus =
  | 'starting'
  | 'working'
  | 'needs_input'
  | 'idle'
  | 'exited'
  | 'recovered'
  | 'archived';

/** What a starting session is doing right now. */
export type StartupStep = 'worktree' | 'launch' | 'loading' | 'prompt';

export const STARTUP_STEPS: { step: StartupStep; label: string }[] = [
  { step: 'worktree', label: 'Creating the worktree' },
  { step: 'launch', label: 'Starting Claude Code' },
  { step: 'loading', label: 'Loading plugins, MCP servers and hooks' },
  { step: 'prompt', label: 'Sending your task' },
];

export interface StartupState {
  step: StartupStep;
  /** When this step began (ms). */
  stepSince: number;
  /** When startup began (ms). */
  startedAt: number;
  /** Resuming an existing conversation (no worktree step). */
  resumed: boolean;
  /** A first prompt will be sent once Claude is up. */
  hasPrompt: boolean;
}

export type Stage = 'design' | 'code' | 'pr' | 'review' | 'merge' | 'cleanup' | 'done';

/** The stages shown in the stage bar, in order. `done` is terminal and not shown. */
export const STAGES: Exclude<Stage, 'done'>[] = ['design', 'code', 'pr', 'review', 'merge', 'cleanup'];

export const STAGE_LABELS: Record<Stage, string> = {
  design: 'Design',
  code: 'Code',
  pr: 'PR',
  review: 'Review',
  merge: 'Merge',
  cleanup: 'Cleanup',
  done: 'Done',
};

export type ReviewState =
  | 'none' // no PR yet
  | 'waiting' // PR open, no review yet
  | 'in_review' // AgentK acked and is reviewing
  | 'refused' // AgentK declined (e.g. PR too large); user may force
  | 'addressing' // a review arrived and Claude is working through it
  | 'addressed' // Claude finished addressing the latest review
  | 'approved'; // required approvals are in / nothing left to address

export type CiState = 'none' | 'pending' | 'success' | 'failure';

export type ReviewerMode = 'agentk' | 'people';

export type MergeMethod = 'repo-default' | 'squash' | 'merge' | 'rebase';

/**
 * Token counts from Claude Code transcripts (main conversation and subagents).
 * `input` is uncached prompt tokens; cache reads and writes are counted separately.
 */
export interface TokenUsage {
  input: number;
  output: number;
  cacheRead: number;
  cacheWrite: number;
  /** Model responses counted. */
  messages: number;
}

export interface ModelUsage {
  model: string;
  usage: TokenUsage;
}

export interface SessionUsage {
  sessionId: string;
  name: string;
  stage: Stage;
  archived: boolean;
  usage: TokenUsage;
}

export interface DailyUsage {
  /** YYYY-MM-DD in the server's time zone. */
  day: string;
  usage: TokenUsage;
}

export interface ProjectUsage {
  total: TokenUsage;
  byModel: ModelUsage[];
  /** Every session with usage, archived ones included, largest first. */
  bySession: SessionUsage[];
  /** The last 14 days, oldest first, zero-filled. */
  daily: DailyUsage[];
}

export interface ProjectSettings {
  /** Send a continue prompt to sessions that were mid-turn when the host went down. */
  autoContinue: boolean;
  reviewer: ReviewerMode;
  /** GitHub login of the AgentK bot, matched with or without a `[bot]` suffix. */
  reviewerLogin: string;
  branchPrefix: string;
  mergeMethod: MergeMethod;
  /** Browser notifications for this project. */
  notify: boolean;
}

export interface DatapackRef {
  id: string;
  name: string;
  role: 'dedicated' | 'shared';
}

export interface Project {
  id: string;
  name: string;
  repoPath: string;
  defaultBranch: string;
  position: number;
  settings: ProjectSettings;
  datapacks: DatapackRef[];
  createdAt: number;
}

export interface Session {
  id: string;
  projectId: string;
  name: string;
  claudeSessionId: string;
  branch: string;
  baseBranch: string;
  worktreePath: string;
  tmuxName: string;
  desired: 'running' | 'archived';
  status: SessionStatus;
  /** Human-readable detail for the status, e.g. the Notification message. */
  statusDetail: string | null;
  stage: Stage;
  prNumber: number | null;
  prUrl: string | null;
  reviewState: ReviewState;
  ciState: CiState;
  /** True when the Merge gate may be passed. */
  canMerge: boolean;
  /** Why Merge is disabled, in plain words. Empty when canMerge. */
  mergeBlockers: string[];
  /** True while a gate action or stage transition is in flight. */
  busy: boolean;
  hasDesign: boolean;
  /** Lines added/removed vs. the base branch, refreshed periodically. */
  diffStat: { added: number; removed: number } | null;
  usage: TokenUsage;
  /** Set while status is `starting`. */
  startup: StartupState | null;
  lastEventAt: number | null;
  createdAt: number;
  archivedAt: number | null;
  recoveredAt: number | null;
}

export interface ProjectWithSessions extends Project {
  sessions: Session[];
  /** Token usage across all the project's sessions, archived ones included. */
  usage: TokenUsage;
}

export interface UpdateStatus {
  current: string;
  channel: 'stable' | 'prerelease' | 'off';
  latest: string | null;
  available: boolean;
  applying: boolean;
  lastCheckedAt: number | null;
  lastError: string | null;
  /** Set for one boot after a successful update, for the "Updated to vX" notice. */
  justUpdatedFrom: string | null;
  releaseUrl: string | null;
}

export interface MekoStatus {
  ok: boolean;
  error: string | null;
  checkedAt: number | null;
}

export interface AppState {
  projects: ProjectWithSessions[];
  meko: MekoStatus;
  update: UpdateStatus;
  version: string;
  username: string;
}

export interface RepoInfo {
  path: string;
  name: string;
  remote: string | null;
  branch: string | null;
  lastTouchedAt: number;
  /** Set when a project already uses this repo. */
  projectId: string | null;
  projectName: string | null;
}

export interface ReposResponse {
  roots: string[];
  repos: RepoInfo[];
}

export interface MekoDatapack {
  id: string;
  name: string;
  grant: string | null;
  createdAt: string | null;
}

export interface ActivityItem {
  at: number;
  sessionId: string | null;
  sessionName: string | null;
  kind: string;
  text: string;
}

export interface DatapackWithCounts extends DatapackRef {
  memoryCount: number | null;
  knowledgeCount: number | null;
}

export interface ProjectHome {
  project: Project;
  datapacks: DatapackWithCounts[];
  activity: ActivityItem[];
  usage: ProjectUsage;
}

export interface SearchResult {
  source: 'memory' | 'knowledge_base' | 'conversation';
  text: string;
  score: number | null;
  datapackName: string | null;
}

export interface AuthStatus {
  setupRequired: boolean;
  loggedIn: boolean;
  username: string | null;
  totpEnabled: boolean;
}

export interface Settings {
  reposRoots: string[];
  reposDepth: number;
  cloneInto: string;
  updateChannel: 'stable' | 'prerelease' | 'off';
  bind: string;
  port: number;
}

export interface DesignDoc {
  markdown: string | null;
  updatedAt: number | null;
}

// ---- Requests ----

export interface SetupRequest {
  token: string;
  username: string;
  password: string;
}

export interface LoginRequest {
  username: string;
  password: string;
  totp?: string;
}

export interface LoginError {
  error: string;
  totpRequired?: boolean;
}

export type DatapackChoice =
  | { mode: 'create'; name: string }
  | { mode: 'existing'; id: string; name: string };

export interface CreateProjectRequest {
  /** Exactly one of repoPath / cloneUrl. */
  repoPath?: string;
  cloneUrl?: string;
  name: string;
  datapack: DatapackChoice;
}

export interface UpdateProjectRequest {
  name?: string;
  position?: number;
  settings?: Partial<ProjectSettings>;
}

export interface AttachDatapackRequest {
  id: string;
  name: string;
}

export interface CreateSessionRequest {
  name: string;
  prompt?: string;
  baseBranch?: string;
  /** Start in the Code stage instead of Design. */
  skipDesign?: boolean;
}

export type SessionAction =
  | 'approve-design'
  | 'open-pr'
  | 'merge'
  | 'request-review'
  | 'force-review'
  | 'resume'
  | 'archive';

export interface TotpEnrollResponse {
  secret: string;
  otpauthUrl: string;
  qrSvg: string;
}

// ---- WebSockets ----
//
// /ws/events: server → client JSON ServerEvent. A full `state` is sent on
// connect and again (debounced) after every change.
//
// /ws/term/:sessionId: client → server JSON TermClientMessage (text frames).
// server → client: binary frames are raw terminal output; text frames are
// JSON TermServerMessage.

export type ServerEvent =
  | { type: 'state'; state: AppState }
  | { type: 'notify'; projectId: string; sessionId: string; title: string; body: string };

export type TermClientMessage =
  | { type: 'input'; data: string }
  | { type: 'resize'; cols: number; rows: number };

export type TermServerMessage =
  | { type: 'exit'; reason: string }
  | { type: 'error'; error: string };

// ---- REST routes (all JSON; errors are { error: string } with a 4xx/5xx) ----
//
// GET    /healthz                              → { ok: true, version }
// GET    /api/auth/status                      → AuthStatus            (no auth)
// POST   /api/auth/setup      SetupRequest     → 204 + cookie          (no auth)
// POST   /api/auth/login      LoginRequest     → 204 + cookie | 401 LoginError | 429
// POST   /api/auth/logout                      → 204
// POST   /api/auth/password   { current, next }→ 204
// POST   /api/auth/totp/enroll                 → TotpEnrollResponse
// POST   /api/auth/totp/confirm { code }       → 204
// POST   /api/auth/totp/disable { code }       → 204
// GET    /api/state                            → AppState
// GET    /api/repos                            → ReposResponse
// GET    /api/meko/datapacks                   → MekoDatapack[]
// POST   /api/projects        CreateProjectRequest → Project
// PATCH  /api/projects/:id    UpdateProjectRequest → Project
// DELETE /api/projects/:id                     → 204
// GET    /api/projects/:id/branches            → string[]
// POST   /api/projects/:id/datapacks AttachDatapackRequest → Project
// DELETE /api/projects/:id/datapacks/:dpId     → Project
// GET    /api/projects/:id/home                → ProjectHome
// POST   /api/projects/:id/search { query }    → SearchResult[]
// POST   /api/projects/:id/sessions CreateSessionRequest → Session
// POST   /api/sessions/:id/actions/:action     → Session   (action: SessionAction)
// GET    /api/sessions/:id/design              → DesignDoc
// GET    /api/settings                         → Settings
// PUT    /api/settings        Partial<Settings>→ Settings
// GET    /api/update                           → UpdateStatus
// POST   /api/update/check                     → UpdateStatus
// POST   /api/update/apply                     → UpdateStatus
