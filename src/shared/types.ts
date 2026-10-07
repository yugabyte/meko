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
 * `explore` is a task's optional first stage: no gates, the user moves the session on from it.
 * `reviewing-pr` is a Review a PR session's only stage: the user reviews someone else's PR, with
 * AgentK assisting, until they close the review.
 */
export type Stage = 'explore' | 'design' | 'code' | 'pr' | 'review' | 'merge' | 'done' | 'reviewing-pr';

/**
 * What a session is: a task, which runs on Explore → Design → Code → PR & Review → Merge & Close
 * from the stage it starts at, or a Review a PR (someone else's PR), which has one stage.
 */
export type SessionKind = 'task' | 'review-pr';

export const SESSION_KINDS: SessionKind[] = ['task', 'review-pr'];

/**
 * A session's kind as `sessions.kind` stores it and `Session.kind` sends it: `review-pr`, or a task
 * under one of the old session types' names (`plan` for new ones). Pages and releases from before
 * start stages index their stage lists with it, so it never says `task`; read it with `kindOf()`
 * in `stages.ts`.
 */
export type StoredKind = 'explore' | 'plan' | 'quick' | 'review-pr';

export const KIND_LABELS: Record<SessionKind, string> = {
  task: 'Task',
  'review-pr': 'Review a PR',
};

/** Where a new task session starts. It gets to PR & Review only through Open PR or a move. */
export type StartStage = 'explore' | 'design' | 'code';

export const START_STAGES: StartStage[] = ['explore', 'design', 'code'];

/** One line on what each start (and Review a PR) is for, shown under the choice in the New session dialog. */
export const START_HINTS: Record<StartStage | 'review-pr', string> = {
  explore: "Investigate, answer questions, build POCs and run experiments. Move on to Design, Code or a PR when you're ready.",
  design: 'AgentK writes a design for you to approve, then codes, opens a PR, handles review and merges.',
  code: 'For small tasks. AgentK codes straight away, then opens a PR, handles review and merges.',
  'review-pr': "Review someone else's PR. AgentK checks it out, sums it up for you, then helps with whatever you ask. It posts to GitHub only when you say.",
};

/**
 * An example of the work each start (and Review a PR) suits, shown as the placeholders of the New
 * session dialog's Name and First prompt fields.
 */
export const START_EXAMPLES: Record<StartStage | 'review-pr', { name: string; prompt: string }> = {
  explore: {
    name: 'why is CI slow on macOS',
    prompt: 'e.g. CI takes twice as long on macOS as on Linux. Find out where the time goes and what we could cut.',
  },
  design: {
    name: 'add rate limiting to the API',
    prompt:
      "e.g. Add per-user rate limits to the public API so one client can't starve the rest: a limit per route, and a 429 with Retry-After when one is hit.",
  },
  code: {
    name: 'fix typo in README',
    prompt: 'e.g. The install section of the README says "npm instal". Make it "npm install".',
  },
  'review-pr': { name: 'review #21', prompt: 'e.g. the migration and the polling' },
};

/**
 * The steps shown in the stage bar for each kind, in order. `pr` (opening the PR) shows as part of
 * the PR & Review step (see stepOf in `stages.ts`), and `done` is terminal and not shown. A task's
 * steps are numbered from 0, so Explore is 0 and Design 1.
 */
export const KIND_STAGES: Record<SessionKind, Exclude<Stage, 'done' | 'pr'>[]> = {
  task: ['explore', 'design', 'code', 'review', 'merge'],
  'review-pr': ['reviewing-pr'],
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
  'reviewing-pr': 'Reviewing PR',
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
 * About how many tokens Meko brought into sessions' first turns: what AgentK's search at session
 * start passed to Claude (the seed, `src/server/recall.ts`), and Claude's own lookups before the turn ended.
 * Characters ÷ 4, like `MekoUsage.foundTokens`. Only sessions in projects with datapacks count.
 */
export interface MekoFirstTurn {
  /** Sessions whose first turn has ended. */
  sessions: number;
  /** …where AgentK's search at session start ran (sessions from before it was added have none). */
  startSearches: number;
  /** Tokens from lookups by Claude (or a subagent) that found something before the first turn ended. */
  searchedTokens: number;
  /** Tokens AgentK's search at session start passed to Claude: the seed. */
  seedTokens: number;
}

/**
 * An estimate of what Meko saved: each search that found something, and each seed AgentK passed at
 * session start, is credited with the exploring it most likely replaced, by a fixed formula
 * (`src/server/mekosavings.ts`).
 */
export interface MekoSavings {
  /** Searches that found something: the only calls that count. */
  searches: number;
  /** Seeds: what AgentK's search at session start passed to Claude, each credited like a search. */
  seeds: number;
  tokens: number;
  /** USD at API list prices; null when none of the searches' (or seeds') models has a known price. */
  usd: number | null;
  /** Searches on a model with no known price: in `tokens`, left out of `usd`. */
  unpricedSearches: number;
  /** Seeds on a model with no known price, likewise. */
  unpricedSeeds: number;
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
  mekoFirstTurn: MekoFirstTurn;
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
  mekoFirstTurn: MekoFirstTurn;
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
// The report each install sends once a day (hourly in its first day) to the usage stats collector
// (yugabyte/agent-k, services/ak-local-call-home). docs/call-home-report.schema.json is the same contract as a JSON
// Schema, and the collector keeps an identical copy; change all three together.
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
  /** Sharing is 'api' when AgentK shares through Meko's API, 'none' when it can't (Meko off, or no API URL). */
  meko: { enabled: boolean; keySource: MekoKeySource | 'none'; sharing: 'api' | 'none' };
  /** The UTC day this install first built a report. */
  firstDay: string;
}

/**
 * A Meko tool the report names by name: the searches, fetches and saves Usage counts. Every other
 * Meko tool (datapacks, conversations, deletes) is `other`.
 */
export type CallHomeMekoTool =
  | 'memory_search' | 'knowledgebase_search' | 'context_search'
  | 'memory_get_all' | 'memory_get_by_id' | 'conversation_get' | 'artifact_get'
  | 'memory_add' | 'memory_update' | 'memory_promote' | 'artifact_put' | 'conversation_add_message'
  | 'other';

/** Meko use, all time or on one day: `MekoUsage` as counts, plus calls by tool and the searches at session start. */
export interface CallHomeMeko {
  calls: number;
  lookups: number;
  found: number;
  empty: number;
  errors: number;
  saves: number;
  foundTokens: number;
  /** Calls by tool; tools with none are left out. */
  byTool: Partial<Record<CallHomeMekoTool, number>>;
  /** `MekoSavings`, with the dollars in whole cents (0 when nothing was priced) and never below 0. */
  savings: { searches: number; seeds: number; tokens: number; usdCents: number; unpricedSearches: number; unpricedSeeds: number };
  /** AgentK's search at session start, by outcome, and the milliseconds they took in all (for an average). */
  starts: { found: number; empty: number; error: number; late: number; ms: number };
}

/**
 * Sessions as the report counts them, by where they started: `explore`, `plan` (Design) and `quick`
 * (Code) are the starts under the names the session types had, and `review-pr` a Review a PR.
 */
export type CallHomeKind = 'explore' | 'plan' | 'quick' | 'review-pr';

export interface CallHomeTotals {
  projects: { current: number; deleted: number; withDatapack: number; sharedDatapacks: number; repos: number };
  sessions: {
    total: number;
    open: number;
    archived: number;
    byKind: Record<CallHomeKind, number>;
    openByStage: Record<Stage, number>;
  };
  /** prCommits, prAdditions and prDeletions cover merges recorded since this was added. */
  shipped: { mergedPrs: number; directPushes: number; directCommits: number; prCommits: number; prAdditions: number; prDeletions: number };
  fileTypes: FileTypeCount[];
  tokens: CallHomeTokens;
  worktrees: { idle: number };
  meko: CallHomeMeko;
  mekoFirstTurn: MekoFirstTurn;
}

/** One UTC day. Days are sent again in later reports; the latest report for a day wins. */
export interface CallHomeDay {
  day: string;
  activeProjects: number;
  activeSessions: number;
  messages: number;
  /** Projects opened: new ones, and closed ones opened again. */
  projectsCreated: number;
  /** Projects closed, counting closes a later reopen undid. */
  projectsDeleted: number;
  sessionsCreated: Record<CallHomeKind, number>;
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
  /** Left out of a day with no Meko calls or searches at session start. */
  meko?: CallHomeMeko;
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
  /** The branch existed before the session started on it (an existing branch or a PR's): Discard keeps it. */
  branchReused: boolean;
  /** The repo the session works in (the project's unless another one was picked). */
  repoPath: string;
  worktreePath: string;
  tmuxName: string;
  desired: 'running' | 'archived';
  status: SessionStatus;
  /** Human-readable detail for the status, e.g. the Notification message. */
  statusDetail: string | null;
  /** As stored (see StoredKind); `kindOf(session.kind)` is `task` or `review-pr`. */
  kind: StoredKind;
  stage: Stage;
  /** The session has a task (its first prompt, or its name); one started at Explore without a first prompt has none. */
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
   * Steps before the current one that the session never went through, e.g. Explore and Design for a
   * task started at Code. The stage bar shows them as skipped.
   */
  skippedStages: Stage[];
  hasDesign: boolean;
  /** When the design was last written or republished; null before there is one. */
  designUpdatedAt: number | null;
  /** Lines added/removed vs. the base branch, refreshed periodically. */
  diffStat: { added: number; removed: number } | null;
  /** Review comments on the diff not yet sent to Claude (drafts) or waiting for its turn to end (queued). */
  comments: { drafts: number; queued: number };
  /** How many of the design's questions are open, for the user to answer. */
  designQuestions: { open: number };
  /**
   * AgentK's mod is running in the session's Claude (`[claude] mod` on, and the mod polling): answers
   * to the design can be sent and the design approved while AgentK works, and Claude's questions
   * (`asking`) answered here.
   */
  modLive: boolean;
  /**
   * A turn is running: AgentK is working, or Claude waits on you (a permission prompt, a question)
   * during a turn. False for a Needs you with no turn behind it, like a design waiting for approval.
   */
  midTurn: boolean;
  /** Answers you sent during a turn that Claude holds until it ends (through the mod); approving waits for them. */
  promptsHeld: number;
  /** The question Claude is asking with its AskUserQuestion tool, waiting for an answer; null when none (or without the mod). */
  asking: AskRequest | null;
  /** The user approved the design while AgentK was busy: the approval goes when this turn ends, if the turn leaves the design alone. */
  approvalQueued: boolean;
  /** The PR a Review a PR session reviews; null for other kinds. */
  reviewingPr: ReviewingPr | null;
  usage: TokenUsage;
  /**
   * How full Claude's context window is, in percent, as AgentK's mod last measured it (after each
   * turn); null without the mod, or before its first reading since Claude started or was cleared.
   */
  contextPercent: number | null;
  /** Set while status is `starting`. */
  startup: StartupState | null;
  lastEventAt: number | null;
  createdAt: number;
  archivedAt: number | null;
  recoveredAt: number | null;
}

/** What a Review a PR session knows about the PR it reviews, from its last look at GitHub. */
export interface ReviewingPr {
  /** The PR's repo on GitHub, `owner/name`. */
  repo: string;
  title: string;
  author: string;
  /** The commit checked out in the worktree: the PR's head when the review started, or at the last Update to latest. */
  reviewSha: string;
  /** The PR's head on GitHub. */
  headSha: string;
  /** Commits on the PR since reviewSha; null when the PR's history was rewritten (a force-push). */
  newCommits: number | null;
  prState: 'OPEN' | 'CLOSED' | 'MERGED';
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
   * AgentK shares datapacks and answers invites through Meko's share API with our key: Meko is on
   * and its API URL is known.
   */
  sharing: boolean;
  /**
   * Whether Claude Code's sessions reach the Meko AgentK uses: the same server in Claude Code's
   * settings and on its `meko` MCP server, tokens there that work (accepted, and seeing AgentK's
   * project datapacks), and the plugin. Fixed only when the user asks (POST
   * /api/settings/meko/claude-setup). Empty lists while Meko is off.
   */
  claude: MekoClaudeStatus;
}

export interface MekoClaudeStatus {
  /** What's wrong, one sentence each; the banner shows while this isn't empty. */
  gaps: string[];
  /** Token checks that couldn't run (Meko unreachable or slow), one sentence each. Never raises the banner. */
  unchecked: string[];
  /** A token check is running. */
  checking: boolean;
}

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
  /** New sessions whose worktree couldn't be made, newest first, until dismissed (kept in memory: a restart forgets them). */
  failedStarts: FailedStart[];
  /** The Claude Code sessions launch with, and whether it runs AgentK's mod. */
  claudeCode: ClaudeCodeStatus;
}

/**
 * The Claude Code the next session launch gets (`[claude] command`), as AgentK's mod needs it. A
 * running session keeps the version it started with until Claude restarts in it.
 */
export interface ClaudeCodeStatus {
  /** What `claude --version` printed, or null when it can't be read (or hasn't been yet). */
  version: string | null;
  /** The oldest Claude Code AgentK's mod runs in. */
  minModVersion: string;
  /** Whether `version` runs the mod; null while the version is unknown. */
  supportsMod: boolean | null;
  /** `[claude] mod`: when it's off, the version doesn't matter to AgentK. */
  modEnabled: boolean;
  /** `claude update` is running (POST /api/claude/update). */
  updating: boolean;
  /**
   * The last update's outcome until the server restarts: whether it worked, the versions before and
   * after, and the last lines Claude Code printed.
   */
  lastUpdate: { ok: boolean; from: string | null; to: string | null; message: string; at: number } | null;
}

/**
 * A new session that couldn't start: its worktree (or, for a Review a PR, the PR's head) couldn't
 * be had after the New session dialog had closed, so the session was removed.
 */
export interface FailedStart {
  /** The id the session had. */
  id: string;
  projectId: string;
  name: string;
  error: string;
  at: number;
  /** What was asked, so Try again can open the New session dialog filled in. */
  request: CreateSessionRequest;
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
  /** Where Clone from URL and New repo put repos (`[repos] clone_into`, as configured). */
  cloneInto: string;
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

/** POST /api/invites/check: the datapack list it read, the shares after syncing with it, and the closed projects. */
export interface InviteCheck {
  datapacks: MekoDatapack[];
  /** Open ones, the ones put off with Later included. */
  invites: DatapackInvite[];
  /** Closed projects by their dedicated datapack (the latest closed per datapack): picking it opens one again. */
  closed: ClosedProject[];
}

export interface ClosedProject {
  datapackId: string;
  id: string;
  name: string;
  repoPath: string;
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
  /** The GitHub login this email is for (sharing with a PR's reviewer): the email is remembered for it. */
  login?: string;
}

/** A person who reviews, or is asked to review, one of the project's PRs. */
export interface PrReviewer {
  login: string;
  name: string | null;
  /** An email typed for this login before, else the public one on their GitHub profile, else null. */
  email: string | null;
  emailFrom: 'remembered' | 'github' | null;
}

/** A PR of one of the project's running sessions, and who reviews it. */
export interface PrReviewers {
  sessionId: string;
  /** `owner/name` on GitHub. */
  repo: string;
  number: number;
  title: string;
  url: string;
  reviewers: PrReviewer[];
  /** Teams asked to review (`org/slug`); their members are invited by email. */
  teams: string[];
  /** Why GitHub couldn't be read for this PR, if it couldn't. */
  error?: string;
}

/** An open PR a Review a PR session can be started on. */
export interface ReviewablePr {
  /** `owner/name` on GitHub. */
  repo: string;
  number: number;
  title: string;
  author: string;
  url: string;
  createdAt: string;
  /** A local clone of the PR's repo (a fork whose upstream it is first), or null when there's none. */
  localRepo: string | null;
  /** What to clone when there's no local clone. */
  cloneUrl: string;
  /**
   * A typed PR only: a datapack its description names (AgentK's Meko lines) that isn't one of the
   * project's, so the author's design can be had by attaching it. Null otherwise.
   */
  otherDatapackId: string | null;
}

/** GET /api/projects/:id/review-prs: the open PRs whose description names one of the project's datapacks, or the one asked for. */
export interface ReviewablePrs {
  prs: ReviewablePr[];
  /** Set when GitHub couldn't be searched (the list is then empty). */
  error?: string;
}

/** An open PR a new task session can work on (New session → Advanced → Pull request). */
export interface TaskPr {
  /** `owner/name` on GitHub: the repo the PR is in. */
  repo: string;
  number: number;
  title: string;
  url: string;
  /** The PR's head branch, in `headRepo`. */
  headRefName: string;
  /** `owner/name` of the repo the head branch is in: a fork's, for a PR from a fork. */
  headRepo: string;
  baseRefName: string;
  updatedAt: string;
  /** From a fork (the head is in another repo). */
  fork: boolean;
  /**
   * Why a session can't start on it, or null when it can: its fork was deleted, the PR isn't in
   * the repo's own repo (its origin, or its upstream for a fork's clone), no remote of the repo is
   * the PR's head repo, or a running session works on its branch.
   */
  blocked: string | null;
}

/** GET /api/projects/:id/task-prs: your open PRs in a repo, or the one asked for. */
export interface TaskPrs {
  prs: TaskPr[];
  /** Set when GitHub couldn't be asked (the list is then empty). */
  error?: string;
}

export interface CloneRepoRequest {
  url: string;
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
  /** Meko's id for the item, when it has one. */
  id?: string;
  source: 'memory' | 'knowledge_base' | 'conversation';
  text: string;
  score: number | null;
  datapackName: string | null;
}

export interface AuthStatus {
  setupRequired: boolean;
  /** The setup asked for replaces an existing login (a `setup-link` run to recover a lost password). */
  reset: boolean;
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
  /** `[pr] resolve_conflicts`: AgentK resolves a PR's merge conflicts with its base (the General tab's checkbox). */
  resolveConflicts: boolean;
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
  /** The MCP server AgentK would use with `url` empty (from the environment, Claude Code's settings or Meko Cloud). */
  fallbackUrl: string;
  /** Web app and API URLs as set; empty means worked out from the MCP URL. */
  webUrl: string;
  apiUrl: string;
  /** The web app URL MEKO_WEB_URL or Claude Code's settings give, used when `webUrl` is empty; '' when unset. */
  envWebUrl: string;
  /** What they work out to now; null when they can't be (your own Meko with no URL set). */
  effectiveWebUrl: string | null;
  effectiveApiUrl: string | null;
  /** The key's last four characters, or null when there's no key. */
  keyHint: string | null;
  keySource: MekoKeySource | null;
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

/** The largest image that can be pasted into a terminal. Claude Code scales big images down itself. */
export const MAX_PASTE_BYTES = 20 * 1024 * 1024;

/** An image pasted into a session's terminal, saved on the host: the browser pastes this path for Claude. */
export interface PastedImage {
  path: string;
}

/** The most images a new session can start with. */
export const MAX_START_IMAGES = 20;

/** An image added in the New session dialog, saved on the host until the session is created with it. */
export interface StagedImage {
  /** What `CreateSessionRequest.images` names it by. */
  id: string;
}

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
  /** The design's questions for the user, open and resolved, in the order AgentK listed them. */
  questions: DesignQuestion[];
  /**
   * Where the questions came from: `file` (the questions file AgentK writes beside the design),
   * `markdown` (the list under an "Open questions" heading of a markdown design), or null (none).
   */
  questionsSource: 'file' | 'markdown' | null;
}

export interface DesignQuestionOption {
  label: string;
  detail: string | null;
}

/** A question AgentK asks the user about its design, with the options it sees, if any. */
export interface DesignQuestion {
  /** Stable across revisions of the design: lowercase letters, digits and dashes. */
  id: string;
  question: string;
  context: string | null;
  options: DesignQuestionOption[];
  /** The option AgentK recommends (an index into `options`), or null. */
  recommended: number | null;
  status: 'open' | 'resolved';
  /** What was decided, once resolved. */
  decision: string | null;
}

/**
 * The user's answer to one of the design's open questions. Answers aren't stored on the server:
 * the browser keeps them until they go to AgentK with an approval or a request to revise.
 */
export interface DesignAnswerInput {
  questionId: string;
  /** The option picked (an index into the question's options), or null for a custom answer. */
  choice: number | null;
  /** The picked option's label, to catch a revision that changed the options; null with `choice`. */
  label: string | null;
  /** With an option, an optional note on it; for a custom answer, the answer itself (required). */
  text: string;
}

/**
 * Answers to the design's open questions and other feedback on it. An open question left out is
 * sent as unanswered: AgentK goes with its recommendation, or its own judgment when it has none.
 */
export interface DesignAnswersRequest {
  answers: DesignAnswerInput[];
  feedback: string;
}

export interface AskOption {
  label: string;
  description: string | null;
  /** What the option looks like (a mockup, a snippet), shown as preformatted text. */
  preview: string | null;
}

/** One question of Claude's AskUserQuestion tool. */
export interface AskQuestion {
  question: string;
  /** A short label for it (a chip). */
  header: string;
  options: AskOption[];
  /** Several options can be picked. */
  multiSelect: boolean;
}

/**
 * Questions Claude is asking with its AskUserQuestion tool (1 to 4), through AgentK's mod. They're
 * kept in memory only while Claude's tool call waits: answered here or in the terminal, whichever
 * comes first.
 */
export interface AskRequest {
  id: string;
  questions: AskQuestion[];
}

/** The answer to one of an AskRequest's questions: the options picked (by label), and/or text of the user's own. */
export interface AskAnswerInput {
  /** The question's index in `AskRequest.questions`. */
  question: number;
  /** The labels picked: at most one unless the question is multiSelect. */
  labels: string[];
  /** A custom answer (the dialog's "Other"); with labels, added after them. */
  text: string;
}

export interface AskAnswerRequest {
  answers: AskAnswerInput[];
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
  /** Exactly one of repoPath / cloneUrl / newRepo. */
  repoPath?: string;
  cloneUrl?: string;
  /**
   * A folder name for a new repo in the clone folder (`[repos] clone_into`): git init and an empty
   * first commit. A repo there just as an earlier try left it is used again.
   */
  newRepo?: string;
  name: string;
  datapack: DatapackChoice;
}

/**
 * What's wrong with <name> as a new repo's folder name, or null when nothing is: one plain folder
 * name, not hidden (a ".git" in the repos folder would make it look like a repo), up to 100
 * characters. Checked by the server and, as it's typed, by the Open project dialog.
 */
export function newRepoNameProblem(name: string): string | null {
  const n = name.trim();
  if (!n) return 'Name the new repo.';
  if (n.length > 100) return 'Keep the repo name to 100 characters or fewer.';
  if (!/^[A-Za-z0-9._-]+$/.test(n)) return 'Use only letters, digits, “.”, “_” and “-” in the repo name.';
  if (/^[.-]/.test(n)) return 'Start the repo name with a letter, a digit or “_”.';
  return null;
}

export interface UpdateProjectRequest {
  name?: string;
  position?: number;
  settings?: Partial<ProjectSettings>;
}

/** The longest session name a rename takes. */
export const SESSION_NAME_MAX = 100;

export interface UpdateSessionRequest {
  /** The new name: trimmed, at most SESSION_NAME_MAX characters, no control characters. */
  name?: string;
}

export interface AttachDatapackRequest {
  id: string;
  name: string;
}

export interface CreateSessionRequest {
  name: string;
  /** The first prompt. Left out, a task starting at Design or Code uses `name`; one starting at Explore starts with none. */
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
  /**
   * With `branch`: the session works on that existing branch (Advanced → Existing branch), so a
   * branch that's neither local nor on origin is a 400 rather than a new branch. Its open PR, if
   * any, is linked as the session starts and its base becomes the session's base.
   */
  existingBranch?: boolean;
  /**
   * Defaults to `task`. The old session types still work: `explore`, `plan` and `quick` are a task
   * starting at Explore, Design and Code (`start` wins when both are given).
   */
  kind?: SessionKind | StoredKind;
  /** Where a task starts. Defaults to `design`. Ignored for a Review a PR. */
  start?: StartStage;
  /** Deprecated: use `start: 'code'`. True starts a task at Code when neither `start` nor `kind` says otherwise. */
  skipDesign?: boolean;
  /**
   * For `kind: 'review-pr'` (and required there): the PR to review. `repoPath` is then a local clone
   * of `repo`; the base and branch come from the PR, and `prompt` holds the user's notes for AgentK.
   * For a task (Advanced → Pull request): the open PR to keep working on, in the repo's origin (or
   * its upstream, for a fork's clone). The session works on the PR's head branch (fetched from the
   * remote of the PR's head repo, a fork's included) against the PR's base, with the PR linked;
   * `branch` and `baseBranch` are ignored.
   */
  pr?: { repo: string; number: number };
  /**
   * Images for the first prompt (a review's notes), by the ids `POST /api/projects/:id/images`
   * returned for this project. Their paths are added to the task (or the notes) for Claude to read.
   * A failed start keeps them in its request, but the images themselves are gone by then.
   */
  images?: string[];
}

/** The stages a session can be moved to by hand. `review` is the PR & Review step. */
export type MoveTarget = 'explore' | 'design' | 'code' | 'review';

/** Moves a task session to another stage (see `src/shared/stages.ts` for the rules). */
export interface MoveSessionRequest {
  stage: MoveTarget;
  /**
   * Send the new stage's prompt. Left out, it follows promptByDefault. Ignored for PR & Review,
   * which picks up the branch's open PR, or sends the PR prompt when there's work to push.
   */
  prompt?: boolean;
  /** The task, for a session that has none yet (one started at Explore without a first prompt). */
  task?: string;
}

export type SessionAction =
  | 'approve-design'
  /** Withdraws an approval waiting for AgentK's turn to end (`Session.approvalQueued`). */
  | 'cancel-approval'
  | 'open-pr'
  | 'merge'
  | 'request-review'
  | 'force-review'
  | 'resume'
  | 'archive'
  | 'discard'
  /** Review a PR: check out the PR's latest head and have AgentK sum up what changed. */
  | 'update-pr'
  /** Review a PR: end the review (archives the session and deletes its local review branch). */
  | 'close-review';

export interface TotpEnrollResponse {
  secret: string;
  otpauthUrl: string;
  qrSvg: string;
}

// ---- WebSockets ----
//
// /ws/events: server → client JSON ServerEvent. A full `state` is sent on
// connect and again (debounced) after every change. Client → server JSON
// EventsClientMessage: a `ping` is answered with a `pong`, so a page can tell a
// socket the server dropped (a phone back from the background) from a quiet one.
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
  | { type: 'invite'; datapackId: string; title: string; body: string }
  /** The answer to a client's `ping`. */
  | { type: 'pong' };

export type EventsClientMessage = { type: 'ping' };

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
// GET    /api/auth/status                      → AuthStatus            (no auth; reset: a setup link replaces the login)
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
// POST   /api/repos/clone     CloneRepoRequest → RepoInfo   (clones a network remote into the repos folder; an existing
//        clone of the same remote there is returned as it is, anything else there is a 409)
// GET    /api/meko/datapacks                   → MekoDatapack[]
// GET    /api/meko/datapacks/:id/people        → DatapackPeople   (the datapack must be in a project here)
// POST   /api/meko/datapacks/:id/people ShareDatapackRequest → DatapackPeople   (invites by email; Meko emails them;
//        with `login`, the email is remembered for that GitHub login)
// POST   /api/meko/datapacks/:id/people/invites/:inviteId/resend → DatapackPeople
// DELETE /api/meko/datapacks/:id/people/invites/:inviteId        → DatapackPeople   (revokes a pending invite)
//        The four above answer 409 when AgentK can't share (AppState.meko.sharing false).
// POST   /api/invites/check                    → InviteCheck   (lists datapacks and syncs shares with that list; the Open project dialog's one read of Meko)
// POST   /api/invites/:datapackId/accept       → 204   (viewer invites; other invites are accepted by opening the project)
// POST   /api/invites/:datapackId/decline      → 204
// POST   /api/invites/:datapackId/later        → 204   (hides it here; the invite stays in Meko)
// POST   /api/projects        CreateProjectRequest → Project   (opens a project on a repo here, a clone, or a new repo;
//        409 when a new repo's folder is already taken)
// GET    /api/projects/:id                     → Project   (404 once it's closed: the UI asks for a project it just
//        opened when a state push comes without it)
// PATCH  /api/projects/:id    UpdateProjectRequest → Project
// DELETE /api/projects/:id                     → 204   (closes it: the repo, branches and datapacks stay)
// GET    /api/projects/:id/branches            → string[]
// POST   /api/projects/:id/datapacks AttachDatapackRequest → Project
// POST   /api/projects/:id/dedicated-datapack SetDedicatedDatapackRequest → Project   (a project without one gets its
//        dedicated datapack; 409 if it has one or Meko is off, 402 with code DATAPACK_LIMIT at the plan's limit.
//        With `replace` it switches: the old one stays attached as shared, or is detached with `detachOld`;
//        409 if `replace` isn't the dedicated datapack any more)
// DELETE /api/projects/:id/datapacks/:dpId     → Project
// GET    /api/projects/:id/home                → ProjectHome
// GET    /api/projects/:id/pr-reviewers        → PrReviewers[]   (the open PRs of the project's running sessions)
// GET    /api/projects/:id/review-prs?pr=&repoPath= → ReviewablePrs   (open PRs whose description names one of the
//        project's datapacks, in any repo; with `pr` (a number, #number, owner/name#number or PR link) that one PR,
//        a bare number looked up in `repoPath` (default: the project's repo); 400 for a PR that can't be found)
// GET    /api/projects/:id/task-prs?repoPath=&pr= → TaskPrs   (your open PRs in `repoPath` (default: the project's
//        repo), newest first; with `pr` (a number, #number, owner/name#number or PR link) that one PR; 400 for a PR
//        that can't be found or isn't open)
// GET    /api/usage                            → UsageOverview
// POST   /api/projects/:id/search { query }    → SearchResult[]
// POST   /api/projects/:id/sessions CreateSessionRequest → Session   (400 for an unknown kind or start, a bad branch name, a
//        base branch neither the repo nor its origin has, a repo that isn't a scanned one, an `existingBranch` that doesn't exist, or a task's
//        `pr` that isn't open or whose head repo no remote of the repo is; 409 for a branch a running session uses or another checkout has. Review a PR:
//        400 without `pr`, for a PR that isn't open, or a repoPath that isn't a clone of the PR's repo; 400 for
//        `images` that aren't this project's staged images, or more than MAX_START_IMAGES). Answers once
//        the session is written, still starting: the worktree and Claude start after (Session.startup shows where it
//        is). A worktree or PR fetch that fails then removes the session (and its images) and adds it to AppState.failedStarts.
// POST   /api/projects/:id/images  image bytes → StagedImage   (an image added in the New session dialog: checked as
//        for POST /api/sessions/:id/images, 404 for an unknown project. Saved under ~/.agent-k-local/pastes/.new/<project>/
//        until a session is created with it, which moves it to the session's folder; removed after a day otherwise,
//        or when the project is closed)
// PATCH  /api/sessions/:id    UpdateSessionRequest → Session   (renames it in AgentK and its Meko conversation; the
//        branch, worktree, Meko agent id and Claude Code's own name for the conversation stay; 400 for an empty or too
//        long name or one with control characters, 409 for an archived session)
// DELETE /api/failed-starts/:id                → 204   (dismisses a failed start; 204 when it's already gone)
// POST   /api/sessions/:id/actions/:action     → Session   (action: SessionAction; approve-design takes an optional
//        DesignAnswersRequest, sent ahead of the approval: 400 for a bad answer or answers too long for one prompt, 409
//        when AgentK's questions changed since, in plan mode, or while Claude holds answers sent during a turn
//        (Session.promptsHeld). Mid-turn (Session.midTurn), with the mod live, the approval is queued for the turn's
//        end (Session.approvalQueued) and dropped if the turn changes the design; without the mod that's a 409.
//        cancel-approval withdraws it: 409 when none is queued)
// POST   /api/sessions/:id/move MoveSessionRequest → Session   (400 for a stage the session can't move to or a
//        missing task; 409 while AgentK is mid-turn, in Merge & Close, or a prompt is due and Claude is waiting or exited;
//        502 when the move was made but its prompt couldn't be sent)
// GET    /api/sessions/:id/design              → DesignDoc
// POST   /api/sessions/:id/design/answers DesignAnswersRequest → 204   (sends them for AgentK to revise the design;
//        409 outside Design, while AgentK works or Claude waits on something else (mid-turn with the mod live they're
//        sent anyway, and Claude takes them when the turn ends), while an approval is queued, or when AgentK's questions
//        changed since; 400 for a bad answer or answers too long for one prompt; 502 when sending failed)
// POST   /api/sessions/:id/ask/:askId AskAnswerRequest → 204   (answers the questions Claude is asking, Session.asking;
//        400 for an answer that doesn't fit them, 409 once they're gone: answered in the terminal, or Claude moved on)
// GET    /api/sessions/:id/discard-check       → DiscardCheck
// GET    /api/sessions/:id/changes             → SessionChanges
// GET    /api/sessions/:id/changes/image?path=&side=old|new → image bytes (previewable types only)
// POST   /api/sessions/:id/images  image bytes → PastedImage   (the body is the image, with an image/* Content-Type;
//        PNG, JPEG, GIF or WebP by its bytes, else 415; at most 20 MB, else 413; 409 for an archived session. Saved
//        under ~/.agent-k-local/pastes/<session>/ and removed when the session is archived)
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
//                                                           '' goes back to the host name; callHomeEnabled and
//                                                           resolveConflicts are booleans)
// POST   /api/settings/meko/claude-setup       → Settings   (runs Meko's setup for Claude Code with AgentK's Meko, only when
//                                                           the user asks; then checks Claude Code's Meko again)
// POST   /api/claude/update                    → ClaudeCodeStatus   (runs `claude update`, Claude Code's own updater, then
//        reads its version again; the outcome is in `lastUpdate`, a failed update included; 409 while one runs)
// GET    /api/update                           → UpdateStatus
// POST   /api/update/check                     → UpdateStatus
// POST   /api/update/apply                     → UpdateStatus
