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
  { step: 'worktree', label: 'Updating the base branch and preparing the worktree' },
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

/**
 * `merge` is Merge & Close: merging the PR, then the Meko summary, worktree and branch cleanup.
 * `explore` is an Exploration session's only stage: no gates, it ends when the user archives it.
 */
export type Stage = 'explore' | 'design' | 'code' | 'pr' | 'review' | 'merge' | 'done';

/** What a session is for, picked when it's created. It decides the stages it goes through. */
export type SessionKind = 'explore' | 'plan' | 'quick';

export const SESSION_KINDS: SessionKind[] = ['explore', 'plan', 'quick'];

export const KIND_LABELS: Record<SessionKind, string> = {
  explore: 'Exploration',
  plan: 'Plan & Implement',
  quick: 'Quick Code',
};

/** One line on what each kind is for, shown under the choice in the New session dialog. */
export const KIND_HINTS: Record<SessionKind, string> = {
  explore: 'Investigate, answer questions, build POCs and run experiments. No design, PR or merge.',
  plan: 'AgentK writes a design for you to approve, then codes, opens a PR, handles review and merges.',
  quick: 'For small tasks. AgentK codes straight away, then opens a PR, handles review and merges.',
};

/**
 * The steps shown in the stage bar for each kind, in order. `pr` (opening the PR) shows as part of
 * the PR & Review step (see stepOf in `stages.ts`), and `done` is terminal and not shown.
 */
export const KIND_STAGES: Record<SessionKind, Exclude<Stage, 'done' | 'pr'>[]> = {
  explore: ['explore'],
  plan: ['design', 'code', 'review', 'merge'],
  quick: ['code', 'review', 'merge'],
};

/** `pr` and `review` are one step in the UI, so they share a label. */
export const STAGE_LABELS: Record<Stage, string> = {
  explore: 'Explore',
  design: 'Design',
  code: 'Code',
  pr: 'PR & Review',
  review: 'PR & Review',
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

/**
 * Whose reviews Claude addresses: `all` (AgentK, people and other bots), `agentk-people`
 * (AgentK and people) or `people` (people only). Merge waits for AgentK only once it's
 * active on the PR, and never in `people`.
 */
export type ReviewerMode = 'all' | 'agentk-people' | 'people';

export const REVIEWER_LABELS: Record<ReviewerMode, string> = {
  all: 'All reviewers',
  'agentk-people': 'AgentK + People',
  people: 'People only',
};

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

/** What tokens would cost at Anthropic's API list prices (`src/shared/pricing.ts`). */
export interface UsageCost {
  /** USD, for the models with a known price. */
  usd: number;
  /** Responses from models with no known price, left out of `usd`. */
  unpricedMessages: number;
}

/**
 * Meko calls Claude made, read from the session transcripts. A lookup searches or fetches from Meko;
 * a save writes to it. Sizes are estimated as characters ÷ 4.
 */
export interface MekoUsage {
  /** Every Meko call, admin ones (datapacks, conversations) included. */
  calls: number;
  lookups: number;
  /** Lookups that returned something. */
  found: number;
  /** Lookups that returned nothing. */
  empty: number;
  /** Lookups and saves that failed. */
  errors: number;
  saves: number;
  /** About how many tokens the lookups that returned something brought in. */
  foundTokens: number;
  savings: MekoSavings;
}

/**
 * An estimate of what Meko saved: each search that found something is credited with the exploring
 * it most likely replaced, by a fixed formula (`src/server/mekosavings.ts`).
 */
export interface MekoSavings {
  /** Searches that found something: the only calls that count. */
  searches: number;
  tokens: number;
  /** USD at API list prices; null when none of the searches' models has a known price. */
  usd: number | null;
  /** Searches on a model with no known price: in `tokens`, left out of `usd`. */
  unpricedSearches: number;
}

export interface SessionUsage {
  sessionId: string;
  name: string;
  stage: Stage;
  archived: boolean;
  usage: TokenUsage;
  meko: { lookups: number; found: number; saves: number };
}

export interface DailyUsage {
  /** YYYY-MM-DD in the server's time zone. */
  day: string;
  usage: TokenUsage;
}

export interface ProjectUsage {
  total: TokenUsage;
  cost: UsageCost;
  meko: MekoUsage;
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
  cost: UsageCost;
  meko: MekoUsage;
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
  cost: UsageCost;
  meko: MekoUsage;
  mergedPrs: number;
  directPushes: number;
  directCommits: number;
  /** Current projects first, then deleted ones; each by total tokens, largest first. */
  byProject: ProjectUsageSummary[];
  byModel: ModelUsage[];
  /** The last 14 days, oldest first, zero-filled. */
  daily: DailyUsage[];
}

// ---- usage stats (Call Home) ----
//
// The report each install sends once a day to the usage stats collector (yugabyte/agent-k,
// services/ak-local-call-home). docs/call-home-report.schema.json is the collector's copy of this
// contract; keep the two in step.
// Only counts, enums, version strings, model ids and file extensions: never a name, path, prompt
// or anything else that says who someone is or what they work on.

/** Files changed with one extension, and their lines. ext is lowercase, `none` (no suffix) or `other`. */
export interface FileTypeCount {
  ext: string;
  files: number;
  additions: number;
  deletions: number;
}

export interface CallHomeTokens {
  input: number;
  output: number;
  cacheRead: number;
  cacheWrite: number;
  messages: number;
}

export interface CallHomeInstall {
  version: string;
  channel: 'stable' | 'prerelease' | 'off';
  platform: string;
  arch: string;
  node: string;
  /** The version number only, or '' when it can't be read. */
  tmux: string;
  claudeCode: string;
  access: 'local' | 'remote';
  service: 'systemd' | 'launchd' | 'none';
  /** 'none' when Meko is off (or the sharing mode isn't known yet). */
  meko: { enabled: boolean; keySource: MekoKeySource | 'none'; sharing: SharingMode | 'none' };
  /** The UTC day this install first built a report. */
  firstDay: string;
}

export interface CallHomeTotals {
  projects: { current: number; deleted: number; withDatapack: number; sharedDatapacks: number; repos: number };
  sessions: {
    total: number;
    open: number;
    archived: number;
    byKind: Record<SessionKind, number>;
    openByStage: Record<Stage, number>;
  };
  /** prCommits, prAdditions and prDeletions cover merges recorded since this was added. */
  shipped: { mergedPrs: number; directPushes: number; directCommits: number; prCommits: number; prAdditions: number; prDeletions: number };
  fileTypes: FileTypeCount[];
  tokens: CallHomeTokens;
  worktrees: { idle: number };
}

/** One UTC day. Days are sent again in later reports; the latest report for a day wins. */
export interface CallHomeDay {
  day: string;
  activeProjects: number;
  activeSessions: number;
  messages: number;
  projectsCreated: number;
  projectsDeleted: number;
  sessionsCreated: Record<SessionKind, number>;
  sessionsArchived: number;
  stageEntries: Record<Stage, number>;
  prsOpened: number;
  prsMerged: number;
  prCommits: number;
  prAdditions: number;
  prDeletions: number;
  directPushes: number;
  directCommits: number;
  reviews: number;
  ciFailures: number;
  commentBatches: number;
  recoveries: number;
  resumes: number;
  worktreesReused: number;
  fileTypes: FileTypeCount[];
  tokensByModel: (CallHomeTokens & { model: string })[];
}

export interface CallHomeReport {
  schema: 1;
  /** A random id made on first use, one per AgentK home. */
  installId: string;
  sentAt: string;
  install: CallHomeInstall;
  totals: CallHomeTotals;
  /** Oldest first: the last 7 UTC days, or 30 in an install's first report. */
  days: CallHomeDay[];
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
  /** The repo the session works in (the project's unless another one was picked). */
  repoPath: string;
  worktreePath: string;
  tmuxName: string;
  desired: 'running' | 'archived';
  status: SessionStatus;
  /** Human-readable detail for the status, e.g. the Notification message. */
  statusDetail: string | null;
  kind: SessionKind;
  stage: Stage;
  /** The session has a task (its first prompt, or its name); an Exploration started without a first prompt has none. */
  hasTask: boolean;
  prNumber: number | null;
  prUrl: string | null;
  reviewState: ReviewState;
  ciState: CiState;
  /** AgentK has commented on or reviewed the PR, or someone asked it to (`/agentk review`). */
  agentkSeen: boolean;
  /** True when the Merge gate may be passed. */
  canMerge: boolean;
  /** Why Merge is disabled, in plain words. Empty when canMerge. */
  mergeBlockers: string[];
  /** True while a gate action or stage transition is in flight. */
  busy: boolean;
  /**
   * Steps before the current one that the session never went through, e.g. Design for a Quick
   * Code session changed to Plan & Implement. The stage bar shows them as skipped.
   */
  skippedStages: Stage[];
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
  /**
   * Meko is on (a key is set and Meko isn't turned off); when false projects work without
   * datapacks. Meko isn't polled: an action that needs it and fails says why.
   */
  configured: boolean;
  /** Meko web app base URL, for links to datapacks. Null when unknown. */
  webUrl: string | null;
  /**
   * How datapacks are shared: `api` through Meko's share API with our key, `link` in Meko's web
   * app (this Meko doesn't take API keys for sharing yet), null before it has been checked.
   */
  sharing: SharingMode | null;
}

export type SharingMode = 'api' | 'link';

export interface AppState {
  projects: ProjectWithSessions[];
  meko: MekoStatus;
  update: UpdateStatus;
  version: string;
  username: string;
  /** The name shown next to the user name: `[server] name` from config.toml, else the machine's short host name. */
  serverName: string;
  /** The machine's full host name as the OS reports it, for the tooltip when it differs from `serverName`. */
  serverHost: string;
  /** The account's plan usage limits, or null when no session has reported them (API-key accounts never do). */
  limits: PlanLimits | null;
  /** Project datapacks shared with you that aren't a project here yet (not the ones put off with Later). */
  invites: DatapackInvite[];
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
  /** The `upstream` remote of a fork (`github.com/org/repo`), when it has one. */
  upstream: string | null;
  branch: string | null;
  lastTouchedAt: number;
  /** Projects that already use this repo (a repo can have several). */
  projects: { id: string; name: string }[];
}

/** A repo's branches for the New session dialog. */
export interface RepoBranches {
  defaultBranch: string;
  /** Local branches, most recent commit first. */
  branches: string[];
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
  ownerName: string | null;
  ownerEmail: string | null;
  description: string | null;
}

/** Access to a shared datapack, as Meko names it. */
export type ShareGrant = 'viewer' | 'contributor' | 'maintainer';

export const SHARE_GRANTS: ShareGrant[] = ['contributor', 'maintainer', 'viewer'];

/**
 * A project datapack (named `adlc/proj/…`) shared with you. `invited`: an invite waiting in Meko
 * (`inviteId` set); `shared`: already accepted, not yet a project here; `later`: put off with Later.
 */
export interface DatapackInvite {
  datapackId: string;
  name: string;
  inviteId: string | null;
  grant: ShareGrant | null;
  ownerName: string | null;
  ownerEmail: string | null;
  /** The repo's remote, from the datapack's description once Meko lets us read it. */
  repoRemote: string | null;
  state: 'invited' | 'shared' | 'later';
  firstSeenAt: number;
}

/** POST /api/invites/check: the datapack list it read, and the shares after syncing with it. */
export interface InviteCheck {
  datapacks: MekoDatapack[];
  /** Open ones, the ones put off with Later included. */
  invites: DatapackInvite[];
}

/** Someone with access to a datapack (the owner included). */
export interface DatapackMember {
  name: string;
  email: string;
  grant: ShareGrant | 'owner';
}

/** An invite to a datapack that hasn't been accepted yet. */
export interface DatapackPendingInvite {
  inviteId: string;
  email: string;
  grant: ShareGrant;
}

export interface DatapackPeople {
  members: DatapackMember[];
  invites: DatapackPendingInvite[];
  /**
   * Set when a change (invite, resend, revoke) went through but reading the lists again failed:
   * the lists are then empty and not to be shown; the change still happened.
   */
  refreshError?: string;
}

export interface ShareDatapackRequest {
  email: string;
  grant: ShareGrant;
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
  /** Our access to it (`owner`, or a share grant); null when Meko didn't say. */
  grant: string | null;
  /** Its owner, when Meko says (who to ask to share it). */
  ownerName: string | null;
  ownerEmail: string | null;
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
  /**
   * No login needed on this connection: the server listens only on this computer (127.0.0.1 or
   * ::1) and the request came from it, to localhost.
   */
  localAccess: boolean;
}

export interface Settings {
  reposRoots: string[];
  reposDepth: number;
  cloneInto: string;
  updateChannel: 'stable' | 'prerelease' | 'off';
  bind: string;
  port: number;
  /** `[server] name`: shown in the rail next to the user name; '' uses the machine's host name. */
  serverName: string;
  /** The machine's short host name, what an empty `serverName` shows. Read-only. */
  hostName: string;
  /** Idle worktrees each repo keeps for its next session, build outputs and all (0 to 20; 0 removes them). */
  idleWorktreesPerRepo: number;
  /** Days an idle worktree may go unused before it's removed (0 to 365; 0 keeps it until the cap trims it). */
  idleWorktreeDays: number;
  /** `[call_home] enabled`: send anonymous usage stats (the Account tab's checkbox). */
  callHomeEnabled: boolean;
  meko: MekoSettings;
}

/** Where AgentK's Meko key comes from: config.toml, the environment, or Claude Code's settings.json. */
export type MekoKeySource = 'config' | 'env' | 'claude';

/** The Meko part of Settings. The key itself never leaves the server. */
export interface MekoSettings {
  /** False when Meko is turned off (`[meko] enabled = false`): AgentK then works without datapacks. */
  enabled: boolean;
  /** The MCP server URL as set in config.toml; empty means the environment, Claude Code or Meko Cloud decides. */
  url: string;
  /** The MCP server AgentK uses now. */
  effectiveUrl: string;
  /** Meko Cloud's MCP server, used when nothing else names one. */
  defaultUrl: string;
  /** Web app and API URLs as set; empty means worked out from the MCP URL. */
  webUrl: string;
  apiUrl: string;
  /** What they work out to now; null when they can't be (your own Meko with no URL set). */
  effectiveWebUrl: string | null;
  effectiveApiUrl: string | null;
  /** The key's last four characters, or null when there's no key. */
  keyHint: string | null;
  keySource: MekoKeySource | null;
  /** The MCP URL Claude Code's settings give its sessions (MEKO_MCP_URL), or null when unset. */
  claudeUrl: string | null;
  /** Claude Code's settings have the same key AgentK uses. */
  claudeKeyMatches: boolean;
}

/**
 * PUT /api/settings `meko`: a field left out is unchanged; `apiKey: ''` keeps the current key. A new
 * key turns Meko on; `enabled: false` turns it off (the key is kept).
 */
export interface MekoSettingsUpdate {
  enabled?: boolean;
  url?: string;
  apiKey?: string;
  webUrl?: string;
  apiUrl?: string;
}

export type SettingsUpdate = Partial<Omit<Settings, 'meko' | 'hostName'>> & { meko?: MekoSettingsUpdate };

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
  /** No dedicated datapack (Meko is off, or the plan's datapack limit is reached); one can be added later. */
  | { mode: 'none' }
  | { mode: 'create'; name: string }
  /** `inviteId`: an invite to accept in Meko before the project is created. */
  | { mode: 'existing'; id: string; name: string; inviteId?: string };

/**
 * POST /api/projects/:id/dedicated-datapack. Without `replace` the project must have no dedicated
 * datapack yet. With it, the choice replaces that one (a switch): `replace` names the dedicated
 * datapack the user saw, so a switch made meanwhile in another tab isn't undone.
 */
export type SetDedicatedDatapackRequest = DatapackChoice & {
  replace?: string;
  /** Switch only: detach the replaced datapack instead of keeping it attached as shared. */
  detachOld?: boolean;
};

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
  /** The first prompt. Left out, Plan & Implement and Quick Code sessions use `name`; an Exploration starts with none. */
  prompt?: string;
  /** Defaults to the repo's default branch. */
  baseBranch?: string;
  /** A main git repo to work in instead of the project's (one of the scanned repos). */
  repoPath?: string;
  /**
   * The branch, used as typed. Left out or empty, it's `<branchPrefix><slug of the name>`. An
   * existing branch (local, or only on origin) is picked up as it is, unless a running session
   * uses it or another checkout has it.
   */
  branch?: string;
  /** Defaults to `plan`. */
  kind?: SessionKind;
  /** Deprecated: use `kind: 'quick'`. True starts a Quick Code session when `kind` is unset. */
  skipDesign?: boolean;
}

/** The stages a session can be moved to by hand. `review` is the PR & Review step. */
export type MoveTarget = 'explore' | 'design' | 'code' | 'review';

/** Changes a session's type, its stage, or both (see `src/shared/stages.ts` for the rules). */
export interface MoveSessionRequest {
  kind: SessionKind;
  stage: MoveTarget;
  /**
   * Send the new stage's prompt. Left out, it follows promptByDefault. Ignored for PR & Review,
   * which picks up the branch's open PR, or sends the PR prompt when there's work to push.
   */
  prompt?: boolean;
  /** The task, for a session that has none yet (an Exploration started without a first prompt). */
  task?: string;
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
  | { type: 'notify'; projectId: string; sessionId: string; title: string; body: string }
  /** A project datapack was shared with you: sent once per datapack. */
  | { type: 'invite'; datapackId: string; title: string; body: string };

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
// Some errors add a machine-readable `code` next to `error`: DATAPACK_LIMIT (402, the Meko plan's
// datapack limit), MEKO_OFF (409).
// Every /api route except the three marked "no auth" answers 401 without a login cookie, and the
// cookie is re-issued (fresh Max-Age) as its expiry slides forward. The exception is local access
// (AuthStatus.localAccess): when the server listens only on loopback, requests from this computer
// to localhost need no cookie.
//
// GET    /healthz                              → { ok: true, version }
// GET    /api/auth/status                      → AuthStatus            (no auth)
// POST   /api/auth/setup      SetupRequest     → 204 + cookie | 400 | 403 (bad link) | 429   (no auth)
// POST   /api/auth/login      LoginRequest     → 204 + cookie | 401 LoginError | 429
// POST   /api/auth/logout                      → 204
// POST   /api/auth/password   { current, next }→ 204 | 400 | 409 (no login set up); signs out every other browser
// POST   /api/auth/totp/enroll                 → TotpEnrollResponse | 409 (no login set up)
// POST   /api/auth/totp/confirm { code }       → 204
// POST   /api/auth/totp/disable { code }       → 204
// GET    /api/state                            → AppState
// GET    /api/repos                            → ReposResponse
// GET    /api/repos/branches?path=             → RepoBranches   (400 unless the path is a scanned repo or a project's)
// GET    /api/meko/datapacks                   → MekoDatapack[]
// GET    /api/meko/datapacks/:id/people        → DatapackPeople   (the datapack must be in a project here)
// POST   /api/meko/datapacks/:id/people ShareDatapackRequest → DatapackPeople   (invites by email; Meko emails them)
// POST   /api/meko/datapacks/:id/people/invites/:inviteId/resend → DatapackPeople
// DELETE /api/meko/datapacks/:id/people/invites/:inviteId        → DatapackPeople   (revokes a pending invite)
//        The four above answer 409 in link mode (AppState.meko.sharing 'link').
// POST   /api/invites/check                    → InviteCheck   (lists datapacks and syncs shares with that list; the New project dialog's one read of Meko)
// POST   /api/invites/:datapackId/accept       → 204   (viewer invites; other invites are accepted by creating the project)
// POST   /api/invites/:datapackId/decline      → 204
// POST   /api/invites/:datapackId/later        → 204   (hides it here; the invite stays in Meko)
// POST   /api/projects        CreateProjectRequest → Project
// PATCH  /api/projects/:id    UpdateProjectRequest → Project
// DELETE /api/projects/:id                     → 204
// GET    /api/projects/:id/branches            → string[]
// POST   /api/projects/:id/datapacks AttachDatapackRequest → Project
// POST   /api/projects/:id/dedicated-datapack SetDedicatedDatapackRequest → Project   (a project without one gets its
//        dedicated datapack; 409 if it has one or Meko is off, 402 with code DATAPACK_LIMIT at the plan's limit.
//        With `replace` it switches: the old one stays attached as shared, or is detached with `detachOld`;
//        409 if `replace` isn't the dedicated datapack any more)
// DELETE /api/projects/:id/datapacks/:dpId     → Project
// GET    /api/projects/:id/home                → ProjectHome
// GET    /api/usage                            → UsageOverview
// POST   /api/projects/:id/search { query }    → SearchResult[]
// POST   /api/projects/:id/sessions CreateSessionRequest → Session   (400 for an unknown kind, a bad branch name or a
//        repo that isn't a scanned one; 409 for a branch a running session uses or another checkout has)
// POST   /api/sessions/:id/actions/:action     → Session   (action: SessionAction)
// POST   /api/sessions/:id/move MoveSessionRequest → Session   (400 for a type and stage that don't go together or a
//        missing task; 409 while AgentK is mid-turn, in Merge & Close, or a prompt is due and Claude is waiting or exited;
//        502 when the move was made but its prompt couldn't be typed)
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
// PUT    /api/settings        SettingsUpdate   → Settings   (bind and port are read-only: set them in config.toml;
//                                                           a `meko` change is checked against Meko before it's saved;
//                                                           a lower idleWorktreesPerRepo or idleWorktreeDays removes
//                                                           the idle worktrees now beyond it; serverName is trimmed,
//                                                           at most 64 characters with no control characters, and
//                                                           '' goes back to the host name; callHomeEnabled is a boolean)
// POST   /api/settings/meko/claude-setup       → Settings   (runs Meko's setup for Claude Code with AgentK's Meko)
// GET    /api/update                           → UpdateStatus
// POST   /api/update/check                     → UpdateStatus
// POST   /api/update/apply                     → UpdateStatus
