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
  { step: 'worktree', label: 'Updating the base branch and creating the worktree' },
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

/** `merge` is Merge & Close: merging the PR, then the Meko summary, worktree and branch cleanup. */
export type Stage = 'design' | 'code' | 'pr' | 'review' | 'merge' | 'done';

/** The stages shown in the stage bar, in order. `done` is terminal and not shown. */
export const STAGES: Exclude<Stage, 'done'>[] = ['design', 'code', 'pr', 'review', 'merge'];

export const STAGE_LABELS: Record<Stage, string> = {
  design: 'Design',
  code: 'Code',
  pr: 'PR',
  review: 'Review',
  merge: 'Merge & Close',
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

export interface ProjectUsageSummary {
  projectId: string;
  name: string;
  repoPath: string;
  /** Deleted projects keep their sessions and usage. */
  deleted: boolean;
  usage: TokenUsage;
  sessions: number;
  /** Sessions whose PR was merged. */
  mergedPrs: number;
  /** Pushes a session made straight to the project's base branch, and the commits they carried. */
  directPushes: number;
  directCommits: number;
  /** The newest model response in any of its sessions. */
  lastActiveAt: number | null;
}

/** Usage across every project, deleted ones included. */
export interface UsageOverview {
  total: TokenUsage;
  mergedPrs: number;
  directPushes: number;
  directCommits: number;
  /** Current projects first, then deleted ones; each by total tokens, largest first. */
  byProject: ProjectUsageSummary[];
  byModel: ModelUsage[];
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
  /** When the design was last written or republished; null before there is one. */
  designUpdatedAt: number | null;
  /** Lines added/removed vs. the base branch, refreshed periodically. */
  diffStat: { added: number; removed: number } | null;
  /** Review comments on the diff not yet sent to Claude (drafts) or waiting for its turn to end (queued). */
  comments: { drafts: number; queued: number };
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
  /** Meko web app base URL, for links to datapacks. Null when unknown. */
  webUrl: string | null;
}

export interface AppState {
  projects: ProjectWithSessions[];
  meko: MekoStatus;
  update: UpdateStatus;
  version: string;
  username: string;
  /** The account's plan usage limits, or null when no session has reported them (API-key accounts never do). */
  limits: PlanLimits | null;
}

/** One plan usage window, as Claude Code reports it to its status line (Pro and Max plans). */
export interface LimitWindow {
  /** 0–100. */
  usedPercent: number;
  /** When the window resets, in ms since the epoch. */
  resetsAt: number;
}

/** The 5-hour and weekly limits from the newest status line report of any session. */
export interface PlanLimits {
  fiveHour: LimitWindow | null;
  sevenDay: LimitWindow | null;
  /** When a session last reported them. */
  updatedAt: number;
}

export interface RepoInfo {
  path: string;
  name: string;
  remote: string | null;
  branch: string | null;
  lastTouchedAt: number;
  /** Projects that already use this repo (a repo can have several). */
  projects: { id: string; name: string }[];
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

/** What Discard would throw away. */
export interface DiscardCheck {
  worktreeExists: boolean;
  /** Changed or untracked files in the worktree. */
  uncommittedFiles: number;
  /** Commits on the branch that no remote has. */
  unpushedCommits: number;
}

export type ChangeKind = 'added' | 'modified' | 'deleted' | 'renamed' | 'untracked';

/** Image files the Changes panel previews instead of "Binary file", by extension, with their content type. */
export const PREVIEW_IMAGE_TYPES: Record<string, string> = {
  png: 'image/png', jpg: 'image/jpeg', jpeg: 'image/jpeg', gif: 'image/gif', webp: 'image/webp', avif: 'image/avif', bmp: 'image/bmp', ico: 'image/x-icon',
};

/** The content type to preview a path with, or null when it isn't a previewable image. */
export function previewImageType(path: string): string | null {
  const ext = /\.([a-z0-9]+)$/i.exec(path)?.[1]?.toLowerCase();
  return (ext && PREVIEW_IMAGE_TYPES[ext]) || null;
}

/** Which version of a changed file: the base's (old) or the worktree's (new). */
export type ChangeSide = 'old' | 'new';

/** One file that differs between a session's worktree and where its branch left the base. */
export interface ChangedFile {
  path: string;
  /** The previous path, for renames. */
  oldPath: string | null;
  kind: ChangeKind;
  added: number;
  removed: number;
  binary: boolean;
  /** Has changes that aren't committed yet (untracked files always do). */
  uncommitted: boolean;
  /** Unified diff for this file. Null for binary files and diffs too large to show. */
  patch: string | null;
  /** The patch was cut short at the per-file size cap. */
  truncated: boolean;
}

/** Everything changed on a session's branch vs. its base: committed, uncommitted and untracked. */
export interface SessionChanges {
  /** What the changes are compared against, e.g. `origin/main`. */
  base: string;
  /** Sorted by path. */
  files: ChangedFile[];
  added: number;
  removed: number;
  /** Files whose patch was left out because the whole diff hit the size cap. */
  omittedPatches: number;
}

export type CommentSide = 'old' | 'new';
export type CommentState = 'draft' | 'queued' | 'sent';

/**
 * A review comment on lines of a session's diff. `new` points at added or unchanged lines by
 * their number in the worktree, `old` at removed lines by their number in the base. The
 * excerpt is the text of the lines when the comment was written, so the comment can follow
 * them when they move and Claude can find them after they change.
 */
export interface ReviewComment {
  id: string;
  path: string;
  side: CommentSide;
  line: number;
  /** Last line of the range; equal to `line` for a single line. */
  endLine: number;
  excerpt: string;
  body: string;
  state: CommentState;
  createdAt: number;
  updatedAt: number;
  sentAt: number | null;
}

export interface NewComment {
  path: string;
  side: CommentSide;
  line: number;
  endLine: number;
  excerpt: string;
  body: string;
}

export interface SendCommentsResult {
  /** Pasted into Claude now. */
  sent: number;
  /** Waiting for Claude to finish its turn. */
  queued: number;
}

/**
 * A session's design: markdown for simple designs, or a published Artifact (its HTML,
 * shown in a sandboxed frame, and its claude.ai link) for complex ones.
 */
export interface DesignDoc {
  markdown: string | null;
  html: string | null;
  artifactUrl: string | null;
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
  | 'archive'
  | 'discard';

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
// Both sockets close with code 4401 when the login is missing or gone (logout, a password
// change or reset); the client should show the login screen rather than reconnect. The
// server pings every 30 s and drops sockets that don't answer.
//
// /ws/term/:sessionId[?cols=&rows=]: the optional query is the browser terminal's size, used
// to start the tmux client at that size. Client → server JSON TermClientMessage (text frames).
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
  /** `retry`: the failure may pass (the attach couldn't start), so reconnect with backoff; otherwise it's final. */
  | { type: 'error'; error: string; retry?: boolean };

// ---- REST routes (all JSON; errors are { error: string } with a 4xx/5xx) ----
//
// Request bodies must be JSON (`Content-Type: application/json`); other body types get 415.
// Every /api route except the three marked "no auth" answers 401 without a login cookie, and the
// cookie is re-issued (fresh Max-Age) as its expiry slides forward.
//
// GET    /healthz                              → { ok: true, version }
// GET    /api/auth/status                      → AuthStatus            (no auth)
// POST   /api/auth/setup      SetupRequest     → 204 + cookie | 400 | 403 (bad link) | 429   (no auth)
// POST   /api/auth/login      LoginRequest     → 204 + cookie | 401 LoginError | 429
// POST   /api/auth/logout                      → 204
// POST   /api/auth/password   { current, next }→ 204 | 400; signs out every other browser
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
// GET    /api/usage                            → UsageOverview
// POST   /api/projects/:id/search { query }    → SearchResult[]
// POST   /api/projects/:id/sessions CreateSessionRequest → Session
// POST   /api/sessions/:id/actions/:action     → Session   (action: SessionAction)
// GET    /api/sessions/:id/design              → DesignDoc
// GET    /api/sessions/:id/discard-check       → DiscardCheck
// GET    /api/sessions/:id/changes             → SessionChanges
// GET    /api/sessions/:id/changes/image?path=&side=old|new → image bytes (previewable types only)
// GET    /api/sessions/:id/comments            → ReviewComment[]
// POST   /api/sessions/:id/comments NewComment → ReviewComment
// PATCH  /api/sessions/:id/comments/:cid { body } → ReviewComment   (drafts only)
// DELETE /api/sessions/:id/comments/:cid       → 204               (drafts only)
// POST   /api/sessions/:id/comments/send       → SendCommentsResult
// GET    /api/settings                         → Settings
// PUT    /api/settings        Partial<Settings>→ Settings   (bind and port are read-only: set them in config.toml)
// GET    /api/update                           → UpdateStatus
// POST   /api/update/check                     → UpdateStatus
// POST   /api/update/apply                     → UpdateStatus
