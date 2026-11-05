// API contract shared by the server (src/server) and the web UI (src/web).
// Every REST route and WebSocket message is described here.

import type { CodeReviewSummary, ReviewOverrides, ReviewStrategySetting, SeverityCounts } from './codereview.js';
import type { HarnessId } from './harnesses.js';

export type SessionStatus =
  | 'starting'
  | 'working'
  | 'needs_input'
  | 'idle'
  | 'exited'
  | 'recovered'
  | 'archived';

/** What a starting session is doing right now (the Starting view's labels: `startupSteps()` in `stages.ts`). */
export type StartupStep = 'worktree' | 'launch' | 'loading' | 'prompt';

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
 * `reviewing-code` is a code-review session's only stage: AgentK's code review of its parent task's
 * change before the PR opens (docs/design/code-review.md).
 */
export type Stage = 'explore' | 'design' | 'code' | 'pr' | 'review' | 'merge' | 'done' | 'reviewing-pr' | 'reviewing-code';

/**
 * What a session is: a task, which runs on Explore → Design → Code → Review & PR → Merge & Close
 * from the stage it starts at, a Review a PR (someone else's PR), which has one stage, or a
 * code-review session: AgentK's code review of a task's change, a child of the task
 * (`Session.parentId`) that AgentK starts at Open PR and shows only as part of the task's tab.
 */
export type SessionKind = 'task' | 'review-pr' | 'code-review';

/**
 * A session's kind as `sessions.kind` stores it and `Session.kind` sends it: `review-pr`, or a task
 * under one of the old session types' names (`plan` for new ones). Pages and releases from before
 * start stages index their stage lists with it, so it never says `task`; read it with `kindOf()`
 * in `stages.ts`.
 */
export type StoredKind = 'explore' | 'plan' | 'quick' | 'review-pr' | 'code-review';

export const KIND_LABELS: Record<SessionKind, string> = {
  task: 'Task',
  'review-pr': 'Review a PR',
  'code-review': 'Code review',
};

/** Where a new task session starts. It gets to Review & PR only through Open PR or a move. */
export type StartStage = 'explore' | 'design' | 'code';

export const START_STAGES: StartStage[] = ['explore', 'design', 'code'];

/**
 * What a session is for, beyond its kind: `learn`, a learn session (`CreateSessionRequest.learn`),
 * a task started at Explore that seeds the project's dedicated datapack from the repo and its PRs,
 * and closes by itself when it's done; `guide`, a workshop's host (`src/server/workshops.ts`), a
 * task at Explore that walks the user through a bundled workshop by starting other sessions. Null
 * for every other session.
 */
export type SessionPurpose = 'learn' | 'guide';

/** How many months back a learn session reads PRs, by default and at most (0 reads none). */
export const LEARN_MONTHS_DEFAULT = 24;
export const LEARN_MONTHS_MAX = 120;
/** The most characters each of a learn session's two fields takes. */
export const LEARN_TEXT_MAX = 4_000;

/** What the user tells a learn session (`CreateSessionRequest.learn`). */
export interface LearnBrief {
  /** What the project is about. Required. */
  about: string;
  /** Where its files are: folders and files in the repo, docs, other places to look. */
  where: string;
  /** Read PRs merged in this many months back (0 to LEARN_MONTHS_MAX; 0 reads none). */
  months: number;
}

/** Past either, the New session dialog warns that a learn session will read a lot (`LearnScope`). */
export const LEARN_WARN_PRS = 200;
export const LEARN_WARN_FILES = 5_000;

/**
 * A workshop bundled with AgentK (`src/workshops/<id>/`): a script for a host session (Session.purpose
 * `guide`) and the demo repo it's played in, built locally with no remote (docs/design/workshops.md).
 */
export interface Workshop {
  id: string;
  title: string;
  summary: string;
  /** Its open project, or null when it isn't open. */
  projectId: string | null;
  /** That project's open host session, or null. */
  hostId: string | null;
}

/** The AgentK tour: the workshop a fresh install starts by itself, and the empty state and palette offer. */
export const AGENTK_TOUR = 'agentk-101';

/** POST /api/workshops/:id/start: the workshop's project and its host session. */
export interface WorkshopStart {
  projectId: string;
  sessionId: string;
}

/**
 * How much a learn session's brief covers, estimated from local git before it starts
 * (`GET /api/projects/:id/learn-scope`): the merged PRs in its window and the files, in the paths
 * its Where names, or in the whole repo when it names none the repo has.
 */
export interface LearnScope {
  /** First-parent commits on the base branch in the window: one per PR for squash or merge commits. */
  prs: number;
  /** Tracked files on the base branch. */
  files: number;
  /** The words of Where that name files in the repo (git pathspecs); empty for the whole repo. */
  paths: string[];
}

/** One line on what each start (and Review a PR, and a learn session) is for, shown under the choice in the New session dialog. */
export const START_HINTS: Record<StartStage | 'review-pr' | 'learn', string> = {
  explore: "Investigate, answer questions, build POCs and run experiments. Move on to Design, Code or a PR when you're ready.",
  design: 'AgentK writes a design for you to approve, then codes, opens a PR, handles review and merges.',
  code: 'For small tasks. AgentK codes straight away, then opens a PR, handles review and merges.',
  'review-pr': "Review someone else's PR. AgentK checks it out, sums it up for you, then helps with whatever you ask. It posts to GitHub only when you say.",
  learn: "AgentK reads the code you point it to and the repo's past PRs, and saves what it learns to Meko for your team. It changes no files, and the session closes when it's done.",
};

/**
 * An example of the work each start (and Review a PR) suits, shown as the placeholders of the New
 * session dialog's Name and First prompt fields.
 */
export const START_EXAMPLES: Record<StartStage | 'review-pr' | 'learn', { name: string; prompt: string }> = {
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
  learn: {
    name: 'Learn the project',
    prompt: "e.g. The billing service: invoices, proration and the Stripe sync. We're moving it from cron jobs to a queue.",
  },
};

/**
 * The steps shown in the stage bar for each kind, in order. `pr` (opening the PR) shows as part of
 * the Review & PR step (see stepOf in `stages.ts`), and `done` is terminal and not shown. A task's
 * steps are numbered from 0, so Explore is 0 and Design 1.
 */
export const KIND_STAGES: Record<SessionKind, Exclude<Stage, 'done' | 'pr'>[]> = {
  task: ['explore', 'design', 'code', 'review', 'merge'],
  'review-pr': ['reviewing-pr'],
  'code-review': ['reviewing-code'],
};

/**
 * `pr` and `review` are one step in the UI, so they share a label. Its "Review" is the PR's
 * (reviews, CI, conflicts); the code review runs in Code, before it.
 */
export const STAGE_LABELS: Record<Stage, string> = {
  explore: 'Explore',
  design: 'Design',
  code: 'Code',
  pr: 'Review & PR',
  review: 'Review & PR',
  merge: 'Merge & Close',
  done: 'Done',
  'reviewing-pr': 'Reviewing PR',
  'reviewing-code': 'Code review',
};

export type ReviewState =
  | 'none' // no PR yet
  | 'waiting' // PR open, no review yet
  | 'addressing' // a review arrived and Claude is working through it
  | 'addressed' // Claude finished addressing the latest review
  | 'approved'; // required approvals are in / nothing left to address

export type CiState = 'none' | 'pending' | 'success' | 'failure';

/**
 * A gate pressed before it could go, kept until it can (`Session.queued`): Open PR pressed while
 * AgentK was mid-turn goes when the turn ends; Merge & Close pressed while only blockers that clear
 * by themselves were left goes once nothing blocks it.
 */
export type QueuedGate = 'open-pr' | 'merge';

/** A piece of work a session's agent left running in the background (`Session.background`). */
export interface BackgroundTask {
  /** What it is, as the agent names it: `shell`, `subagent`, `monitor`, `workflow`… */
  type: string;
  /** It in one line: a shell's command, else the agent's description of it. */
  description: string;
}

/**
 * Whose reviews go straight to Claude besides the user's own, which always do: `mine` (none: yours
 * only, the default for new projects), `people` (people with a role on the repo) or `all` (people
 * and bots). Every other review is held for the user to send or dismiss (`HeldReview`), and merge
 * waits while one is held.
 */
export type ReviewerMode = 'mine' | 'people' | 'all';

export const REVIEWER_LABELS: Record<ReviewerMode, string> = {
  mine: 'Yours only',
  people: 'People',
  all: 'All reviewers',
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
  /** Model responses counted (for Kiro, turns). */
  messages: number;
  /** Credits Kiro billed, which aren't priced in `UsageCost`; absent or 0 for Claude Code. */
  credits?: number;
}

export interface ModelUsage {
  /** The agent the responses came from: the same model id under two agents is two rows. */
  harness: HarnessId;
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
  /** About how many tokens the calls sent to Meko (their input, characters ÷ 4). */
  sentTokens: number;
  savings: MekoSavings;
}

/**
 * What the learn step (src/server/learn.ts) saved to Meko and the tokens it used, for the project home
 * and the Usage page.
 */
export interface MekoLearning {
  /** Learn steps that ran, and those of them that didn't finish (failed or interrupted). */
  runs: number;
  failed: number;
  /** Merge & Close sessions whose Claude was gone or busy, so no step ran. */
  skipped: number;
  /** Memories added and corrected, and facts found already known. */
  saves: number;
  updates: number;
  skips: number;
  /** Tokens the steps' model calls used, all kinds together. */
  tokens: number;
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
  /** The part of the day's tokens code reviews' sessions spent (in `usage` too), for the chart's stacked bar. */
  reviewTokens: number;
}

/**
 * Code reviews (docs/design/code-review.md) counted from `code_reviews`:
 * each review by its current state, and what its rounds found and the task did with it.
 */
export interface CodeReviewCounts {
  /** Reviews started; a Retry goes on in its review, so it isn't another. */
  runs: number;
  /** By the review's state now: describing, preparing, sweeping, scoring, triage and fixing are `running`. */
  byOutcome: { done: number; failed: number; cancelled: number; running: number };
  /** Done reviews that kept no finding. */
  lgtm: number;
  /** Reviews whose PR prompt went, Open anyway included. */
  prs: number;
  /**
   * Rounds run, the first and every re-review: each that reached its passes. One that stopped, or
   * found nothing changed, while it prepared doesn't count.
   */
  rounds: number;
  /**
   * Every candidate the passes raised, before scoring and Validate, each once: each finished
   * round's, and the ones a round that ran and hasn't finished (still under way, or stopped) has
   * raised so far.
   */
  candidates: number;
  /** Kept findings, every round's: what the `me_ak_review:` line says. */
  findings: SeverityCounts;
  /** What the task said it did with kept findings (`review_outcome`). */
  fixed: number;
  declined: number;
}

/** Code reviews for the Code Reviews section: their counts, and their sessions' tokens. */
export interface CodeReviewUsage extends CodeReviewCounts {
  usage: TokenUsage;
}

export interface ProjectUsage {
  total: TokenUsage;
  cost: UsageCost;
  meko: MekoUsage;
  mekoFirstTurn: MekoFirstTurn;
  mekoLearning: MekoLearning;
  codeReviews: CodeReviewUsage;
  byModel: ModelUsage[];
  /** Every session with usage, archived ones included, the most recently used first, the largest first among ties. */
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
  mekoLearning: MekoLearning;
  codeReviews: CodeReviewUsage;
  mergedPrs: number;
  directPushes: number;
  directCommits: number;
  /** Current projects first, then deleted ones; in each, the most recently used first, the largest first among ties. */
  byProject: ProjectUsageSummary[];
  byModel: ModelUsage[];
  /** The last 14 days, oldest first, zero-filled. */
  daily: DailyUsage[];
}

// ---- usage stats (Call Home) ----
//
// The report each release install sends once a day (hourly in its first day) to YugabyteDB's call home
// service, as the payload of YbCallHome. docs/call-home-report.schema.json is the same contract as a JSON
// Schema; change both together.
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
  claudeCode: string;
  /** Kiro CLI's version, or '' when it's off or can't be read. */
  kiro?: string;
  /** OpenCode's version, left out when it's off or can't be read. */
  opencode?: string;
  /** 'meko': the Meko on this computer serves AgentK to browsers. Older versions sent 'local' or 'remote'. */
  access: 'meko';
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
  /** About how many tokens Claude's calls sent to Meko (characters ÷ 4); left out by installs from before it. */
  sentTokens?: number;
  /** The same by tool; tools with none are left out. */
  sentByTool?: Partial<Record<CallHomeMekoTool, number>>;
  /** Learn steps (src/server/learn.ts) begun then; left out when there were none. */
  learn?: CallHomeLearn;
  /** The Meko calls AgentK made itself (searches at start, designs, the learn step's); left out when there were none. */
  agentk?: CallHomeAgentkMeko;
  /** The Meko plugin's transcript captures AgentK ran; left out when there were none. */
  capture?: { runs: number; failures: number; ms: number };
}

/** Learn steps as the report counts them, by when they began. */
export interface CallHomeLearn {
  runs: number;
  /** `discard` is optional in the schema: reports from before Discard ran learn steps lack it. */
  byTrigger: { merge: number; manual: number; discard: number };
  /** `mod`: through AgentK's mod; `turn`: a typed prompt; `none`: skipped at Merge & Close or Discard. */
  byPath: { mod: number; turn: number; none: number };
  byOutcome: { done: number; failed: number; interrupted: number; skipped: number };
  /** Facts proposed, then added, corrected and found known already (a typed turn counts its adds and corrections). */
  candidates: number;
  saves: number;
  updates: number;
  skips: number;
  /** Steps that ended done with nothing saved or corrected. */
  empty: number;
  /**
   * Questions on Meko's Context Gaps the steps marked taught (a fact answering them was saved)
   * or dismissed (Meko already had the answer). Optional in the schema: reports from before lack
   * them. How many were offered isn't sent: the report has no room left for it in 64 KB.
   */
  taught: number;
  dismissed: number;
  /** The step's Meko searches, its failed Meko calls, and about how many tokens it sent to Meko. */
  lookups: number;
  errors: number;
  sentTokens: number;
  /** What the step's model calls cost: the fork (`extract`), the duplicate check (`dedup`), and typed turns (`turn`). */
  tokens: { extract: CallHomeTokens; dedup: CallHomeTokens; turn: CallHomeTokens };
  /** Their price in whole cents (models without a known price count 0). */
  usdCents: number;
}

/** AgentK's own Meko calls: how many, how many failed, about how many tokens they sent, the time they took, and by tool. */
export interface CallHomeAgentkMeko {
  calls: number;
  errors: number;
  sentTokens: number;
  ms: number;
  byTool: Record<string, number>;
}

/**
 * Code reviews on a day: the reviews started then (counted by their state now, so a later
 * report corrects one that ended after), and the tokens their sessions spent then with their price in
 * whole cents and, from Kiro, the credits billed (left out when none).
 */
export interface CallHomeCodeReview extends CodeReviewCounts {
  tokens: CallHomeTokens;
  usdCents: number;
  credits?: number;
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
    /** Sessions by the harness they run on. */
    byHarness?: Record<HarnessId, number>;
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
  /** Kiro on the day: sessions started on it, its finished turns, and the credits they billed. Left out of a day with none. */
  kiro?: { sessionsCreated: number; turns: number; credits: number };
  /** Ask AgentK on the day: asks, the ones that failed, and the actions the answers proposed. Left out of a day with none. */
  askAgentk?: { asks: number; failed: number; actions: number };
  /** Code reviews on the day: started then, or whose sessions spent tokens then. Left out of a day with neither. */
  codeReview?: CallHomeCodeReview;
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

/**
 * The report as YugabyteDB's call home service (YB_DIAGNOSTICS_URL in callhome.ts) takes it: yugabyted's
 * envelope, with the report as its payload. The service routes it by callhome_type to a table of its own.
 */
export interface YbCallHome {
  callhome_type: 'agentk';
  command: 'heartbeat';
  /** UTC, with microseconds and no zone (`2026-11-09T17:05:32.123000`): the only form the service parses. */
  timestamp: string;
  install_id: string;
  version: string;
  payload: CallHomeReport;
}

export interface ProjectSettings {
  /** Send a continue prompt to sessions that were mid-turn when the host went down. */
  autoContinue: boolean;
  reviewer: ReviewerMode;
  /** New sessions' branches start with it; `{login}` in it is your GitHub login (expandBranchPrefix). */
  branchPrefix: string;
  mergeMethod: MergeMethod;
  /** Browser notifications for this project. */
  notify: boolean;
  /**
   * The project's code review settings over the global ones (`[code_review]`): each one left out
   * keeps the global value. A session's own (`Session.review`) go over these.
   */
  codeReview: ReviewOverrides;
}

/**
 * The open questions on Meko's Context Gaps for a project's dedicated datapack (the searches Meko
 * found nothing, or nothing relevant, for), by how their latest ask went: the project home's line,
 * which links to the tab (both counts 0 with none open). Meko keeps them; AgentK only reads the counts.
 */
export interface MekoGaps {
  datapackId: string;
  /** The datapack is the linked Meko Cloud's: Meko's web UI shows its Context Gaps on a page of their own. */
  cloud: boolean;
  empty: number;
  weak: number;
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
  /**
   * The bundled workshop that opened it (`Workshop.id`), or null for a project the user opened. A
   * workshop's project is on a demo repo AgentK built with no remote, so it opens no PR: Open PR,
   * Run to merge and a move to Review & PR are refused there, and its Code gate offers Run code
   * review instead.
   */
  workshop: string | null;
}

/** A running learn step's place (`Session.learnProgress`), for the UI to say what a Discard or Merge & Close waits on. */
export interface LearnProgress {
  /**
   * Through the mod: `extract`, Claude looks back over the session for what it learned; `search`,
   * AgentK finds what Meko already has on each fact; `check`, a small model checks them against
   * it; `save`, AgentK writes them. Without the mod, `turn`: Claude saves them itself in a typed turn.
   */
  step: 'extract' | 'search' | 'check' | 'save' | 'turn';
  /** The facts Claude found (0 while extracting, and for a turn). */
  facts: number;
  /** Facts the save part has been through so far, whatever became of each: written, corrected, found already saved or failed to write (in `save`). */
  done: number;
  /** When the learn step started (ms). */
  startedAt: number;
}

/**
 * Where a session in Merge & Close is: `merge`, waiting for GitHub to merge the PR; `turn`, merged
 * and waiting for Claude's turn to end; `learn`, saving what it learned (`Session.learnProgress`);
 * `cleanup`, letting go of the worktree and branches.
 */
export type CloseStep = 'merge' | 'turn' | 'learn' | 'cleanup';

export interface Session {
  id: string;
  projectId: string;
  name: string;
  /** The coding agent the session runs on, fixed when it starts (`src/shared/harnesses.ts`). */
  harness: HarnessId;
  /**
   * The model picked for it at the start (`CreateSessionRequest.model`), passed on every launch of
   * its agent; null for the agent's own default.
   */
  model: string | null;
  /**
   * AgentK's key for the session's conversation: Claude Code's session id (Resume continues it), and
   * for every harness the key of its Meko conversation.
   */
  claudeSessionId: string;
  branch: string;
  baseBranch: string;
  /** The branch existed before the session started on it (an existing branch or a PR's): Discard keeps it. */
  branchReused: boolean;
  /** The repo the session works in (the project's unless another one was picked). */
  repoPath: string;
  worktreePath: string;
  /** The session's pane in the session host (`me_koder attach <paneName>`). */
  paneName: string;
  desired: 'running' | 'archived';
  status: SessionStatus;
  /** Human-readable detail for the status, e.g. the Notification message. */
  statusDetail: string | null;
  /** As stored (see StoredKind); `kindOf(session.kind)` is `task` or `review-pr`. */
  kind: StoredKind;
  stage: Stage;
  /** The session has a task (its first prompt, or its name); only one started at Explore without a first prompt before the name became its task has none. */
  hasTask: boolean;
  /** What it's for beyond its kind: `learn` for a learn session; null otherwise. */
  purpose: SessionPurpose | null;
  prNumber: number | null;
  prUrl: string | null;
  reviewState: ReviewState;
  ciState: CiState;
  /** True when the Merge gate may be passed. */
  canMerge: boolean;
  /** Why Merge is disabled, in plain words. Empty when canMerge. */
  mergeBlockers: string[];
  /**
   * Merge & Close can be queued: it isn't mergeable yet, but every blocker clears by itself (AgentK
   * working or waiting on you mid-turn, CI running, or a review's findings being addressed). The
   * gate reads Merge when ready.
   */
  mergeQueueable: boolean;
  /**
   * The merge blockers only the user can clear (a part of `mergeBlockers`): a review held for them,
   * failing CI or merge conflicts AgentK handed over after its attempts on this head, or conflicts
   * while Resolve merge conflicts on PRs is off; and with Run to merge every other blocker that doesn't
   * clear by itself, except a question in the terminal (the status says it), changes requested while
   * AgentK addresses a review, and failing CI or conflicts AgentK still has attempts for. In Review &
   * PR the session's dot shows Needs you while there's one (`dotState()`).
   */
  waitsOnYou: string[];
  /** A gate pressed early, waiting to go (kept in state.db across restarts); null when none. */
  queued: QueuedGate | null;
  /** The user's instructions for a queued Open PR (OpenPrRequest); null when it has none, or none is queued. */
  queuedInstructions: string | null;
  /**
   * Run to merge: AgentK passes Open PR when a Code turn ends and Merge & Close once nothing blocks
   * it, by keeping the gate queued (`queued`) in each stage. In Design it waits for the approval.
   */
  autoRun: boolean;
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
  /** Files changed and lines added/removed vs. the base branch, refreshed periodically. */
  diffStat: DiffStat | null;
  /** Review comments on the diff not yet sent to Claude (drafts) or waiting for its turn to end (queued). */
  comments: { drafts: number; queued: number };
  /** How many of the design's questions are open, for the user to answer. */
  designQuestions: { open: number };
  /** Reviews on the session's PR held for the user to send to AgentK or dismiss, and their authors. */
  heldReviews: { count: number; authors: string[] };
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
  /** AgentK is saving what the session learned to its project's datapack (the learn step, or its typed turn). */
  learning: boolean;
  /** Where that learn step is, while `learning`; null otherwise. */
  learnProgress: LearnProgress | null;
  /** Where Merge & Close is, while the session is in that stage; null in every other stage. */
  closeStep: CloseStep | null;
  /**
   * The session's agent is running a Meko tool call (Claude's as AgentK's mod reports it, Kiro's and
   * OpenCode's as their hooks do), or started one less than 1.5 s ago, so a quick call still shows.
   */
  mekoCall: boolean;
  /**
   * Work the agent left running in the background when its last turn ended, which wakes it again
   * when it's done: Claude Code's background shells, subagents and monitors (its `Stop` hook's
   * `background_tasks`), less the watch on each Artifact the agent publishes, which never ends;
   * OpenCode's background shells and subagents (from AgentK's plugin). Empty when there's none, and
   * always for Kiro, whose hooks don't say. Kept in memory: replaced at each turn's end, and cleared
   * when the agent exits or relaunches.
   */
  background: BackgroundTask[];
  /** Discard was asked for: the session is discarded once its learn step ends. */
  discarding: boolean;
  /** The PR a Review a PR session reviews; null for other kinds. */
  reviewingPr: ReviewingPr | null;
  /** A code-review session's task (its parent); null for every other session. */
  parentId: string | null;
  /**
   * A task's latest code review, or a code-review session's own; null when it has none. The tab and
   * the rail show it as the magnifier joined to the task.
   */
  codeReview: CodeReviewSummary | null;
  /** The session's own code review settings, over its project's and the global ones; `{}` keeps them. */
  review: ReviewOverrides;
  usage: TokenUsage;
  /**
   * How full Claude's context window is, in percent, as AgentK's mod last measured it (after each
   * turn); null without the mod, or before its first reading since Claude started or was cleared.
   */
  contextPercent: number | null;
  /** Set while status is `starting`. */
  startup: StartupState | null;
  lastEventAt: number | null;
  /** Its place in the project's list, lowest first: a new session, or one moved to the top, goes first. */
  position: number;
  /** The work item it's for (one session per item, `WorkItem.sessionId`); null when it's on none. */
  workItemId: string | null;
  createdAt: number;
  archivedAt: number | null;
  recoveredAt: number | null;
  /**
   * When AgentK stopped the session's agent because it sat idle past `[sessions] idle_stop_hours`
   * (ms); null otherwise. The status is `exited` meanwhile, and the agent starts again with
   * `--resume` on Resume, a key typed into its terminal, an action that sends it a prompt, or an
   * AgentK step that needs it.
   */
  idleStoppedAt: number | null;
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
  /** The PR's design (from Meko) has a version AgentK hasn't read: Update to latest has it read, with no new commits too. */
  designUnread: boolean;
}

export interface ProjectWithSessions extends Project {
  sessions: Session[];
  /** Token usage across all the project's sessions, archived ones included. */
  usage: TokenUsage;
}

/**
 * What AgentK knows about its own updates. Meko installs them and keeps what's available, the job and
 * the switch for automatic updates (the page reads those from Meko's own routes: `MekoExtension`).
 */
export interface UpdateStatus {
  current: string;
  /** The version Meko updated from, while this one is on probation (its first 5 minutes): the "Updated to vX" notice. */
  justUpdatedFrom: string | null;
  /** The version the last update came from, kept after probation (What's new in Settings); null before any update. */
  lastUpdatedFrom: string | null;
}

/**
 * AgentK's row in Meko's extension list (`GET /api/v1/extensions` on Meko's origin, not AgentK's): the
 * fields the Update banner and Settings › Updates read. An older Meko leaves the update fields out.
 */
export interface MekoExtension {
  name: string;
  installed?: boolean;
  /** The release Meko installed, when it manages it. */
  version?: string;
  /** Meko can update it. */
  updatable?: boolean;
  /** Why Meko can't, when it's installed and not updatable (a build from source, or still in ~/.agent-k-local); an older Meko never says. */
  not_updatable?: string;
  /** The newest release the last check found, rolled-back versions left out. */
  latest?: string;
  update_available?: boolean;
  /** RFC 3339. */
  checked_at?: string;
  check_error?: string;
  /** An update on probation. */
  pending?: { from: string; to: string; attempts: number; at: string };
  rolled_back?: string[];
  /** Update automatically (Meko's switch for this extension, on by default). */
  auto_update?: boolean;
  /** Why an automatic update is waiting. */
  waiting?: string;
  job?: MekoExtensionJob;
}

/** The last action Meko ran on an extension since it started (its install, update, start or stop). */
export interface MekoExtensionJob {
  action: string;
  state: 'running' | 'done' | 'failed';
  started_at: string;
  finished_at?: string;
  error?: string;
  output: string;
  /** Started by Meko's scheduled check (Update automatically), not by someone. */
  auto?: boolean;
}

/** One change in What's new: a commit subject with a releasing type (see the repo root's scripts/release/release-notes.sh). */
export interface ReleaseNoteItem {
  /** The component in the subject's parentheses, if any. */
  component: string | null;
  summary: string;
  /** The PR it was merged in, from "(#123)" at the end of its subject. */
  pr: number | null;
  /** The commit, shortened. */
  sha: string;
  /** The PR when there is one, else the commit. */
  url: string;
}

export interface ReleaseNoteSection {
  /** Breaking changes, Features, Fixes, Performance or Dependencies; empty ones are left out. */
  title: string;
  items: ReleaseNoteItem[];
}

/** What changed between two versions (GET /api/update/notes), from the commits between their tags. */
export interface ReleaseNotes {
  from: string;
  to: string;
  sections: ReleaseNoteSection[];
  /** The commits read, at most 250 (GitHub's limit for one comparison); `totalCommits` is all of them. */
  commits: number;
  totalCommits: number;
  /** The comparison on GitHub, every commit included. */
  compareUrl: string;
}

export interface MekoStatus {
  /**
   * There's a Meko to use (Meko's Meko Cloud link, or the Meko on this computer); when false projects
   * work without datapacks. Meko isn't polled: an action that needs it and fails says why.
   */
  configured: boolean;
  /** The Meko MCP server AgentK uses (and sets the agents up with); null when there's none. */
  url: string | null;
  /** Where its key comes from (`MekoKeySource`); null when there's none. */
  source: MekoKeySource | null;
  /** Meko web app base URL, for links to datapacks. Null when unknown. */
  webUrl: string | null;
  /**
   * AgentK can share datapacks through Meko's API: there's a Meko and its API URL is known (what
   * `InviteService.requireSharing()` checks). Usually false on the Meko on this computer, which has
   * no share routes: Share then points to Meko Cloud's sign-up instead. An API URL on its own origin,
   * kept in config.toml from before Meko owned the link (`forKey()` in config.ts), makes it true.
   */
  sharing: boolean;
  /**
   * Whether Claude Code's sessions reach the Meko AgentK uses: its `meko` MCP server, as Meko reports
   * it (`HarnessStatus.meko`), then what Meko's capture needs in Claude Code: the plugin, and the
   * server and a token that works (accepted, and seeing AgentK's project datapacks) in its settings.
   * Fixed only when the user asks (Meko's connect, then POST /api/settings/meko/claude-setup).
   * Empty lists while Meko is off.
   */
  claude: MekoClaudeStatus;
  /**
   * Whether Kiro's sessions reach the Meko AgentK uses: its `meko` MCP server, as Meko reports it
   * (`HarnessStatus.meko`), and the agent Kiro sessions are built from loading it (its
   * `includeMcpJson`). Meko's capture hooks aren't checked. Null when Kiro isn't available or Meko is
   * off. Fixed only when the user asks (Meko's connect, then POST /api/settings/meko/kiro-setup).
   */
  kiro: MekoAgentGaps | null;
  /**
   * Which Meko OpenCode's sessions use: AgentK's (`own: false`), which AgentK adds to their config
   * as a remote `meko` MCP server, or OpenCode's own `meko` server (`own: true`), which AgentK leaves
   * alone. Own when Meko reports one (`HarnessStatus.meko`, any state but `none`), else when OpenCode's
   * own config has one. Null when OpenCode isn't available or Meko is off.
   */
  opencode: { own: boolean } | null;
  /**
   * Meko's list of the agents on this computer (GET /api/v1/agents, which `HarnessStatus.meko`
   * comes from): `read` (now, or the last list kept while Meko doesn't answer), `old` (Meko answered
   * 404: too old to list them, so it says to update Meko), or `unread` (never read). Nothing is gated
   * unless it's `read`.
   */
  agentList: 'read' | 'old' | 'unread';
  /**
   * The Meko on this computer, as last read. While it's `none` (no `meko` at all), a banner that
   * can't be dismissed says to install Meko, with a Meko Cloud link too.
   */
  local: LocalMekoState;
}

/** What keeps an agent's sessions off AgentK's Meko, one sentence each. */
export interface MekoAgentGaps {
  /** Everything: its MCP server, as Meko reports it (`HarnessStatus.meko`), then `capture`. */
  gaps: string[];
  /**
   * The gaps Meko's setup package fixes (Claude Code's plugin and settings, Kiro's agent), which
   * AgentK checks itself; while there are any, Settings offers Set up … with this Meko.
   */
  capture: string[];
}

/** Claude Code's: the banner shows while `gaps` isn't empty. */
export interface MekoClaudeStatus extends MekoAgentGaps {
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
  /** The login gh is signed in as, or null until it has said (gh missing or signed out): `{login}` in a branch prefix. */
  ghLogin: string | null;
  /** The name shown next to the user name: `[server] name` from config.toml, else the machine's short host name. */
  serverName: string;
  /** The machine's full host name as the OS reports it, for the tooltip when it differs from `serverName`. */
  serverHost: string;
  /** The account's plan usage limits, or null when no session has reported them (API-key accounts never do). */
  limits: PlanLimits | null;
  /** Kiro's plan allowance (`kiro-cli chat /usage`), or null until Kiro has been used in AgentK and read, or while it isn't available. */
  kiroPlan: KiroPlan | null;
  /** Project datapacks shared with you that aren't a project here yet (not the ones put off with Later). */
  invites: DatapackInvite[];
  /** New sessions whose worktree couldn't be made, newest first, until dismissed (kept in memory: a restart forgets them). */
  failedStarts: FailedStart[];
  /** The Claude Code sessions launch with, and whether it runs AgentK's mod. */
  claudeCode: ClaudeCodeStatus;
  /** Each harness sessions can run on, in HARNESS_IDS order, and the default for new sessions. */
  harnesses: HarnessStatus[];
  /** `[ask_agentk] enabled` and a coding agent available to answer: the command palette offers Ask AgentK. */
  askAgentk: boolean;
  /** The session host that runs every Claude, as it last answered. */
  sessionHost: SessionHostStatus;
  /** The work queue's open items, in its order (closed ones: `GET /api/work-items?status=closed`). */
  workQueue: WorkItem[];
  /** The work queue's last sync and whether one is running. */
  workSync: WorkSync;
}

/** GET /api/harnesses/:id/models: the models a harness's CLI lists (`provider/model` for OpenCode). */
export interface HarnessModels {
  models: string[];
}

/** A coding agent sessions can run on (`[agents] default`, `[claude]`, `[opencode]`, `[kiro]` in config.toml). */
export interface HarnessStatus {
  id: HarnessId;
  /** The CLI is turned on in the config (`[claude] enabled`, `[opencode] enabled`, `[kiro] enabled`). */
  enabled: boolean;
  /** What `<command> --version` printed, or null when it can't be read (or hasn't been yet). */
  version: string | null;
  /** The oldest version AgentK runs sessions on. */
  minVersion: string | null;
  /**
   * New sessions can start on it: enabled, its version read and new enough, and not kept off by
   * Meko (`meko` isn't `none`, `stale` or `elsewhere`; `mekoGates`).
   */
  available: boolean;
  /**
   * Its `meko` MCP server, as the Meko on this computer reports it (GET /api/v1/agents): `connected`
   * (at Meko's target, with its token or key), `stale` (at one of Meko's own, but not the target),
   * `elsewhere` (another server, at `url`), `none`, or `missing` (Meko didn't find it installed).
   * Null when nothing is gated: no list was ever read, or Meko is too old to list agents (404).
   * While Meko doesn't answer, the last list read stands.
   */
  meko: HarnessMeko | null;
  /**
   * Its version has been read (or tried) for the command it's run with since it was last turned on
   * (turning it off forgets the read), so `available: false` means it really isn't there rather than
   * not asked yet. True while it's off.
   */
  checked: boolean;
  /** `[agents] default`: new sessions start on it unless the New session dialog picks another. */
  isDefault: boolean;
  /** Its update command is running (POST /api/harnesses/:id/update). */
  updating: boolean;
}

export type HarnessMekoState = 'connected' | 'stale' | 'elsewhere' | 'none' | 'missing';

export interface HarnessMeko {
  state: HarnessMekoState;
  /** The URL of its `meko` server, or null when it has none (or Meko didn't find it). */
  url: string | null;
}

/** The session host (`me_koder host`). It keeps running across updates, so it can be older than the server. */
export interface SessionHostStatus {
  /** Its version, or null before it first answered. */
  version: string | null;
  /** Too old for this server: it has to be restarted (`me_koder host restart`), which restarts and resumes every session. */
  outdated: boolean;
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

/**
 * Kiro's plan allowance: the account's credits this period, as `kiro-cli chat /usage` prints it
 * (all Kiro use, in AgentK or not). Kiro calls it an estimate.
 */
export interface KiroPlan {
  /** As Kiro names it, without "Kiro": "Free", "Pro". Null when it didn't say. */
  plan: string | null;
  /** Credits used this period, and how many the plan covers. */
  used: number;
  covered: number;
  /** The day it resets (YYYY-MM-DD, as Kiro prints it); null when it didn't say. */
  resetsOn: string | null;
  /** Bonus credits (paid plans; the format is a best guess); null when Kiro printed no bonus line. */
  bonus: { used: number; total: number; expires: string | null; line: string } | null;
  /** Overage (paid plans): credits billed past the plan, and whether it's turned on. Only the `Overages: Disabled` form is confirmed; the credit-bearing form is a guess. Null without an overage line. */
  overage: { credits: number | null; enabled: boolean | null; line: string } | null;
  /** When it was read, in ms since the epoch. */
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

/** From the least access to the most, the order the Share dialog lists them in. */
export const SHARE_GRANTS: ShareGrant[] = ['viewer', 'contributor', 'maintainer'];

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

/** Exactly one of `email` and `emails` (at most MAX_SHARE_EMAILS, invited one after another). */
export interface ShareDatapackRequest {
  email?: string;
  emails?: string[];
  grant: ShareGrant;
  /** The GitHub login this email is for (sharing with a PR's reviewer): the email is remembered for it. Only with `email`. */
  login?: string;
}

/** An address Meko didn't invite, and why. */
export interface ShareFailure {
  email: string;
  error: string;
}

/**
 * The lists after inviting, with who was invited and who wasn't. When none went through, the
 * route fails with the first error instead.
 */
export interface ShareDatapackResult extends DatapackPeople {
  invited: string[];
  failed: ShareFailure[];
}

/** The roles a datapack's share link can give (Meko doesn't offer maintainer for links). */
export type LinkGrant = 'viewer' | 'contributor';

export const LINK_GRANTS: LinkGrant[] = ['viewer', 'contributor'];

/** A datapack's share link: anyone signed in to Meko who opens `url` joins with `grant`. */
export interface ShareLink {
  active: boolean;
  grant: LinkGrant;
  /** `{web app}/share/{token}`; null while the link is off. */
  url: string | null;
  /** How many people have joined through it. */
  joined: number;
  /** How many members have the link's role through it: changing the role changes theirs too. */
  atLinkRole: number;
}

/**
 * GET /api/meko/datapacks/:id/share-link. With no link: `permission` (only the owner or a
 * maintainer can share by link), `unsupported` (this Meko has no share links), `no-web-url` (Meko's
 * web app URL isn't known, so the link can't be built).
 */
export type ShareLinkState = { link: ShareLink } | { link: null; unavailable: 'permission' | 'unsupported' | 'no-web-url' };

/** PUT /api/meko/datapacks/:id/share-link: one change. Turning the link on names the role it gives. */
export type ShareLinkChange = { active: true; grant: LinkGrant } | { active: false } | { grant: LinkGrant } | { reset: true };

/** POST /api/meko/share-links/join: a share link pasted into Ask AgentK. */
export interface JoinByLinkRequest {
  url: string;
}

export interface JoinByLinkResult {
  datapackId: string;
  /** From the datapack list; null while Meko's list doesn't show it yet. */
  name: string | null;
  /** Our access: from the datapack list (a maintainer may paste a viewer link), else the link's role. */
  grant: ShareGrant | 'owner' | null;
  /** They had access already: nothing changed in Meko. */
  alreadyMember: boolean;
  /** The share to open as a project (a project datapack, not a viewer one, no project here uses it). Not a banner. */
  invite: DatapackInvite | null;
  /** The open project here that already uses the datapack. */
  project: { id: string; name: string } | null;
}

/** Someone the Share dialog has invited before (in any project), suggested in it again. */
export interface ShareContact {
  /** As last typed. */
  email: string;
  /** From Meko's member lists, once they've accepted an invite. */
  name: string | null;
  /** Invites that went through. */
  shares: number;
  /** When the latest did (ms). */
  lastAt: number;
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

/**
 * GET /api/github-link?ref=: what pasting a GitHub PR or issue into Search would start, and where.
 * A PR gets a Review a PR session, an issue a task that fixes or implements it.
 */
export interface LinkPlan {
  kind: 'pr' | 'issue';
  /** `owner/name` on GitHub. */
  repo: string;
  number: number;
  title: string;
  url: string;
  author: string;
  state: 'open' | 'closed' | 'merged';
  /**
   * The open project to start in: one with a datapack the description's Meko lines name, else the
   * project on screen (`?project=`) when it's on the repo, else the first on the repo. Null when none
   * fits (then `newProject` is set).
   */
  project: LinkProject | null;
  /** The other open projects on the repo, in rail order, offered too (`why: 'repo'`). Empty when `project` is null. */
  others: LinkProject[];
  /** The project AgentK opens when no open project fits. */
  newProject: LinkNewProject | null;
  /** Running sessions already on it: a review of the PR, or tasks whose first prompt names the issue's link. */
  existing: { sessionId: string; projectId: string; name: string }[];
}

export interface LinkProject {
  id: string;
  name: string;
  /**
   * `datapack`: it has a datapack the description names; `current`: it's the project on screen and its
   * repo is a clone of the link's; `repo`: its repo is a clone of the link's.
   */
  why: 'datapack' | 'current' | 'repo';
  /** The clone of the link's repo the session works in; null when it has to be cloned first. */
  clone: string | null;
}

export interface LinkNewProject {
  name: string;
  /** A local clone of the link's repo it opens on; null when it clones `cloneUrl`. */
  repoPath: string | null;
  cloneUrl: string;
  /**
   * Its dedicated datapack: one the description names (`invite`: a share accepted on the way),
   * `create` for a new one (an issue's project), or none.
   */
  datapack: { id: string; name: string; invite: boolean } | 'create' | null;
  /** The closed project (its id) that opens again with that datapack, if any. */
  reopens: string | null;
}

/** POST /api/github-link/start. */
export interface StartFromLinkRequest {
  /** The pasted link, or `owner/name#N`. */
  ref: string;
  /** The open project to start in; left out, AgentK opens the plan's new project (or uses an open one that fits by now). */
  projectId?: string;
  /** An issue's task: where it starts. Defaults to `design`. */
  start?: 'design' | 'code';
  /** An issue's task at Code: Run to merge (Session.autoRun) from the start. 400 for a PR, or a task at Design. */
  autoRun?: boolean;
  /** An issue's task: its own code review settings (CreateSessionRequest.review). Ignored for a PR. */
  review?: ReviewOverrides;
  /** The agent the session runs on (CreateSessionRequest.harness). */
  harness?: HarnessId;
  /** The model its agent runs on (CreateSessionRequest.model). */
  model?: string;
}

export interface StartFromLinkResult {
  project: Project;
  session: Session;
}

/** Where a session search found the words, from the field that counts most to the one that counts least. */
export type SessionMatchField = 'name' | 'pr' | 'task' | 'branch' | 'design';

/** The most characters a session search takes (`GET /api/sessions/search?q=`). */
export const SESSION_SEARCH_MAX = 200;

/** A session whose name, task, PR, branch or design has the words searched for (`GET /api/sessions/search`). */
export interface SessionMatch {
  sessionId: string;
  projectId: string;
  name: string;
  stage: Stage;
  /** Archived (merged, discarded or closed): it isn't in AppState, so only its PR can be opened. */
  closed: boolean;
  prNumber: number | null;
  prUrl: string | null;
  /** The field that counts most among those with a match. */
  field: SessionMatchField;
  /** The text around the first match in that field, on one line; empty when it's the name. */
  snippet: string;
}

/** What a work item's link points at: a GitHub PR or issue, a Jira issue, a Slack message, or any other page. */
export type WorkLinkKind = 'github' | 'jira' | 'slack' | 'other';

/** A link on a work item, as `parseWorkLink()` (src/shared/worklinks.ts) reads it. */
export interface WorkLink {
  kind: WorkLinkKind;
  /** What it's matched by: `gh:owner/name#N`, `jira:KEY-N`, `slack:CHANNEL/pTS`, or `url:<the link>`. */
  key: string;
  /** The link to open; null for a bare Jira key, which names no site. */
  url: string | null;
  /** Its chip's text: `name#N`, `KEY-N`, `Slack`, or the site's host. */
  label: string;
}

/** Where a work item came from: added by hand, or by a sync (a GitHub review request, a message on the user's Slack list). */
export type WorkItemSource = 'manual' | 'github' | 'slack';

/** One thing on the work queue. */
export interface WorkItem {
  id: string;
  title: string;
  notes: string;
  links: WorkLink[];
  status: 'open' | 'closed';
  source: WorkItemSource;
  /** Its session, open or closed (a closed one stays until another session is started for it); null when it has none. */
  sessionId: string | null;
  /** A sync added it at the bottom and nobody has moved it to its place yet (the New chip). */
  unplaced: boolean;
  /** A GitHub review request that the last GitHub sync didn't find any more: merged, closed, or no longer asked of you. */
  githubDone: boolean;
  createdAt: number;
  closedAt: number | null;
}

/** The most characters of a work item's title, notes and each link; the most links it takes. */
export const WORK_TITLE_MAX = 300;
export const WORK_NOTES_MAX = 10_000;
export const WORK_LINK_MAX = 2000;
export const WORK_LINKS_MAX = 20;

/** POST /api/work-items. A title can be left out when the first link is a GitHub PR or issue: it's read from GitHub. */
export interface CreateWorkItemRequest {
  title?: string;
  notes?: string;
  /** Links as typed, one each (anything `parseWorkLink` reads). */
  links?: string[];
}

/** PATCH /api/work-items/:id: any of these. */
export interface UpdateWorkItemRequest {
  title?: string;
  notes?: string;
  links?: string[];
  status?: 'open' | 'closed';
  /** Move it one place up or down, or to the top or bottom (an open item); any move places it (`unplaced` off). */
  move?: WorkMove;
  /** Keep it where it is: clears `unplaced`. */
  placed?: true;
  /** Link this session to it (taking it from any other item, and replacing its own), or null to unlink. */
  sessionId?: string | null;
}

/** Where a work item's ⋯ actions move it. */
export type WorkMove = 'up' | 'down' | 'top' | 'bottom';

/** POST /api/work-items/close: Dismiss all, with the open items the page showed (any closed meanwhile are skipped). */
export interface CloseWorkItemsRequest {
  ids: string[];
}

/** PUT /api/work-items/order: every open item's id in the new order, and the one that was moved (it's placed). */
export interface ReorderWorkItemsRequest {
  ids: string[];
  moved?: string;
}

/** How one source of a sync went: the items it added, or what went wrong. */
export interface WorkSourceResult {
  ok: boolean;
  added: number;
  error: string | null;
}

/** The work queue's sync (POST /api/work-items/sync, and every `[work_queue] sync_minutes`). */
export interface WorkSync {
  running: boolean;
  /** When the last sync ended; null before the first since the server started. */
  at: number | null;
  github: WorkSourceResult | null;
  /** Null when the last sync left Slack out (turned off, or a timed one while Slack isn't synced on the timer). */
  slack: WorkSourceResult | null;
  /** Slack is synced on the timer too: the last Slack sync worked. */
  slackAuto: boolean;
  /** The sources Settings › Work queue has on; with neither, there's no sync. */
  sources: { github: boolean; slack: boolean };
}

/** GET /api/work-items/by-link: an item holding the link, with its session when it has one. */
export interface WorkLinkMatch {
  item: WorkItem;
  session: { id: string; projectId: string; name: string; stage: Stage; closed: boolean; prNumber: number | null; prUrl: string | null } | null;
}

/** The most characters an instruction for Ask AgentK takes (`AskAgentkRequest.text`). */
export const ASK_AGENTK_TEXT_MAX = 2000;
/** The most earlier exchanges an ask carries (`AskAgentkRequest.history`); older ones are left out. */
export const ASK_AGENTK_HISTORY_MAX = 10;

/**
 * POST /api/ask-agentk: an instruction in plain words from the command palette's Ask AgentK. AgentK
 * hands it to Claude Code once, with no tools, and Claude answers with a reply and the actions to offer.
 */
export interface AskAgentkRequest {
  /** At most ASK_AGENTK_TEXT_MAX characters. */
  text: string;
  /** The exchange so far while the palette is open, oldest first (the last ASK_AGENTK_HISTORY_MAX are used). */
  history?: AskAgentkTurn[];
  /** What's on screen, so "this project" and "this session" mean something. */
  projectId?: string | null;
  sessionId?: string | null;
}

export interface AskAgentkTurn {
  text: string;
  reply: string;
}

/**
 * An action Claude proposed, checked against the state when it answered. The browser runs one it
 * picks through the route the UI already uses, which checks it again: `start` as POST
 * /api/projects/:id/sessions, `link` through the palette's GitHub link lookup, `move` and `openPr`
 * as the session's move and open-pr routes, `openProject` as POST /api/actions/run; `go` only
 * navigates. **Do all** runs every one but `go` with POST /api/actions/run, as AgentK's session
 * tools do (`src/server/actions.ts`).
 */
export type AskAgentkAction =
  /**
   * `autoRun`: Run to merge (Session.autoRun), only with `start` `code`. `harness` and `model`: the
   * agent and its model (CreateSessionRequest's), which only AgentK's session tools give. `branch`:
   * an existing branch to work on as it is (the session tools' start_sessions), as the New session
   * dialog's Existing branch.
   */
  | {
    type: 'start'; projectId: string; start: StartStage; name: string; prompt: string; branch?: string; autoRun?: boolean; review?: ReviewOverrides;
    harness?: HarnessId; model?: string;
  }
  /**
   * A PR or issue (`owner/name#N` or its link). In Do all (POST /api/actions/run) it runs as
   * `githubLinks.start` does, in `projectId` or the project the link fits: a PR as a Review a PR
   * session, an issue as a task at `start` (Design by default), refusing an issue a running task works on.
   */
  | {
    type: 'link'; ref: string; projectId?: string; start?: 'design' | 'code'; autoRun?: boolean; review?: ReviewOverrides;
    harness?: HarnessId; model?: string;
  }
  | { type: 'go'; view: 'session'; projectId: string; sessionId: string }
  | { type: 'go'; view: 'home'; projectId: string }
  | { type: 'go'; view: 'usage' | 'settings' }
  | { type: 'move'; sessionId: string; stage: MoveTarget; instructions: string }
  | { type: 'openPr'; sessionId: string; instructions: string }
  /**
   * Opens a project on `repo`: a scanned repo's path, a clone URL, or GitHub's `owner/name` (cloned
   * when there's no clone here). `datapack`: `new` (a new dedicated datapack while Meko is on, the
   * Open project dialog's default), `none`, or a datapack's id.
   */
  | { type: 'openProject'; repo: string; name: string; datapack: string };

/** The most actions an answer carries (`AskAgentkResult.actions`), and the most one run takes (`RunActionsRequest`). */
export const ACTIONS_MAX = 8;

export interface AskAgentkResult {
  /** What AgentK says back: what it proposes, an answer, or a question when it can't tell what's meant. */
  reply: string;
  /** At most ACTIONS_MAX, in the order Claude gave them. */
  actions: AskAgentkAction[];
  /**
   * The actions are the steps of one instruction ("start #1 and #5 and move X to Code"), so the
   * palette offers **Do all** first; false, they're alternatives to pick one of.
   */
  together: boolean;
  /** Why a proposed action was left out (a project or session that doesn't exist, a move that isn't allowed). */
  notes: string[];
}

/** POST /api/actions/run: actions to run in order (Ask AgentK's **Do all**), checked again against the state now. */
export interface RunActionsRequest {
  /** At most ACTIONS_MAX; `go` actions are left out (they only navigate). */
  actions: AskAgentkAction[];
}

/** What one action came to, in the order they ran. One that fails doesn't stop the rest. */
export interface ActionResult {
  ok: boolean;
  /** What happened, or why it didn't, in a line. */
  text: string;
  /** Where it led: the session started or changed, the project opened. */
  projectId?: string;
  sessionId?: string;
}

export interface RunActionsResult {
  results: ActionResult[];
  /** Why an action was left out before anything ran (as `AskAgentkResult.notes`). */
  notes: string[];
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
  /**
   * A learn session is open on the dedicated datapack (one launched on a datapack since switched
   * away from doesn't count), so the home doesn't offer another for it while it's empty.
   */
  learnSessionOpen: boolean;
  activity: ActivityItem[];
  usage: ProjectUsage;
  /** Open questions on Meko's Context Gaps, while AgentK uses the Meko on this computer and it answers (null otherwise). */
  mekoGaps: MekoGaps | null;
}

export interface SearchResult {
  /** Meko's id for the item, when it has one. */
  id?: string;
  source: 'memory' | 'knowledge_base' | 'conversation';
  text: string;
  score: number | null;
  datapackName: string | null;
  /** The datapack's id, for a design to cite the item by. */
  datapackId?: string;
}

export interface Settings {
  reposRoots: string[];
  reposDepth: number;
  cloneInto: string;
  /** `[server] name`: shown in the rail next to the user name; '' uses the machine's host name. */
  serverName: string;
  /** The machine's short host name, what an empty `serverName` shows. Read-only. */
  hostName: string;
  /** Idle worktrees each repo keeps for its next session, build outputs and all (0 to 20; 0 removes them). */
  idleWorktreesPerRepo: number;
  /** Days an idle worktree may go unused before it's removed (0 to 365; 0 keeps it until the cap trims it). */
  idleWorktreeDays: number;
  /** `[sessions] idle_stop_hours`: hours a session's agent may sit idle before AgentK stops it (0 to MAX_IDLE_STOP_HOURS; 0 never stops one). */
  idleStopHours: number;
  /** `[call_home] enabled`: send anonymous usage stats (the checkbox on Settings › General). */
  callHomeEnabled: boolean;
  /**
   * Set while YB_DISABLE_CALLHOME in AgentK's environment turns usage stats off whatever `callHomeEnabled`
   * says: the checkbox is then off and disabled. Read-only.
   */
  callHomeOffBy?: 'YB_DISABLE_CALLHOME';
  /** `[pr] resolve_conflicts`: AgentK resolves a PR's merge conflicts with its base (the General tab's checkbox). */
  resolveConflicts: boolean;
  /**
   * `[tools] enabled`: sessions get AgentK's session tools (the `agentk_sessions` MCP server and its
   * skill), which open projects, start sessions and move them between stages (the General tab's checkbox).
   */
  toolsEnabled: boolean;
  /** `[agents] default`: the harness new sessions start on unless the New session dialog picks another. */
  defaultHarness: HarnessId;
  /** `[claude] enabled`: offer Claude Code for new sessions when its CLI answers. */
  claudeEnabled: boolean;
  /** `[opencode] enabled`: offer OpenCode for new sessions when its CLI answers. */
  opencodeEnabled: boolean;
  /**
   * `[opencode] disable_tracking`: the OpenCode AgentK runs sends no telemetry, checks for no update and offers none of
   * OpenCode Zen's free models (whose providers may train on prompts), from a session's next launch or resume.
   */
  opencodeDisableTracking: boolean;
  /** `[kiro] enabled`: offer Kiro for new sessions when its CLI answers. */
  kiroEnabled: boolean;
  /** `[kiro] trust_tools`: Kiro sessions start with every tool trusted (`--trust-tools=*`; on Kiro 3.0 as `agentk-trusted`), so Kiro runs any tool without asking. */
  kiroTrustTools: boolean;
  /** `[work_queue] github`: the work queue's sync pulls in the PRs asking for your review (Settings › Work queue). */
  workQueueGithub: boolean;
  /** `[work_queue] slack`: the work queue's sync pulls in the messages on your Slack list. */
  workQueueSlack: boolean;
  /** `[work_queue] slack_list`: that list's link (`https://<workspace>.slack.com/lists/<team>/<F…>`), or empty. */
  workQueueSlackList: string;
  /** `[work_queue] sync_minutes`: how often it syncs by itself (0 to WORK_SYNC_MINUTES_MAX; 0: only when you press Sync). */
  workQueueSyncMinutes: number;
  /** `[code_review] strategy`: the code review before a PR opens, or `off`. */
  codeReviewStrategy: ReviewStrategySetting;
  /** `[code_review] rounds`: re-reviews after the first (0 to MAX_ROUNDS). */
  codeReviewRounds: number;
  /** `[code_review] depth`: sweeps in each review (1 to MAX_DEPTH). */
  codeReviewDepth: number;
  /** `[code_review] check`: a round's findings wait for you to note or dismiss them before they go to the task (Run to merge never waits). */
  codeReviewCheck: boolean;
}

/** The longest the work queue's sync interval can be (`Settings.workQueueSyncMinutes`), in minutes: a day. */
export const WORK_SYNC_MINUTES_MAX = 1440;

/**
 * Where AgentK's Meko key comes from: Meko's Meko Cloud link (`meko`), or the Meko running on this
 * computer (`local`, its client token). Before Meko owns the link (a Meko too old to keep one, or
 * the one-time handover still to run), AgentK's own: config.toml, the environment or Claude Code's
 * settings.json.
 */
export type MekoKeySource = 'meko' | 'local' | 'config' | 'env' | 'claude';

/**
 * The Meko on this computer, as `meko token --json` last said: running, installed but stopped, none
 * found, or `meko` there but failing (an error, too slow, or an answer AgentK can't read).
 */
export type LocalMekoState = 'running' | 'stopped' | 'none' | 'failed';

export type SettingsUpdate = Partial<Omit<Settings, 'hostName'>>;

/** The most hours `[sessions] idle_stop_hours` takes (30 days). */
export const MAX_IDLE_STOP_HOURS = 720;

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

/** An image or file pasted into (or dropped on) a session's terminal, saved on the host: the browser pastes this path for Claude. */
export interface PastedImage {
  path: string;
}

/**
 * The file extensions Meko's knowledge base takes, lower-case and without the dot: a copy of
 * `uploadMimeByExt` in Meko's API server (internal/handlers/knowledge_base_handler.go). Meko checks
 * the bytes against the extension itself.
 */
export const KB_UPLOAD_EXTS = ['pdf', 'txt', 'text', 'md', 'json', 'csv', 'xml', 'html', 'htm'] as const;

/**
 * The largest file the knowledge route takes: a standard plan's per-file cap in Meko (a free plan's
 * is smaller, and Meko's 400 says so). Upload to Meko sends files held in a terminal's strip, which
 * holds files up to MAX_PASTE_BYTES, so from the UI that's the limit.
 */
export const MAX_KB_UPLOAD_BYTES = 50 * 1024 * 1024;

/**
 * POST /api/sessions/:id/knowledge: a file added to the knowledge base of the session's project
 * datapack. `processing`: the file is in Meko's storage and Meko indexes it, and AgentK reports when
 * it's done (a `knowledge` event). `already_uploaded`: Meko already had the same bytes. The server
 * no longer sends `uploaded` or `already_processed` (answers of Meko's removed single-call upload
 * route); the UI still reads them as added and as already there.
 */
export interface KnowledgeUpload {
  filename: string;
  documentKey: string;
  datapackName: string;
  status: 'processing' | 'uploaded' | 'already_uploaded' | 'already_processed';
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

/** A changed file's new side as text, for the Changes panel's ↑ and ↓ (at most 1 MB). */
export interface ChangedText {
  text: string;
  /** The file's patch, diffed again with the text: the panel uses the text only while it's the patch on screen. */
  patch: string;
}

/** A session's diff size vs. its base, as the footer shows it. */
export interface DiffStat {
  /** Changed files, untracked ones included (the Changes panel's count). */
  files: number;
  added: number;
  removed: number;
}

/** Everything changed on a session's branch vs. its base: committed, uncommitted and untracked. */
export interface SessionChanges {
  /** What the changes are compared against, e.g. `origin/main`. */
  base: string;
  /** Sorted by path. At most 1,000 (`MAX_FILES`): the files with review comments, then the first by path. */
  files: ChangedFile[];
  /** Over every changed file, the ones left out of `files` included. */
  added: number;
  removed: number;
  /** Changed files left out of `files` by the cap. */
  omittedFiles: number;
  /** Files whose patch was left out because the whole diff hit the size cap. */
  omittedPatches: number;
  /** Commits on the branch since it left the base. */
  ahead: number;
  /**
   * A branch of origin's or upstream's much closer to the branch than its base, when the branch is
   * far past its base (it was moved onto another branch, such as a release branch for a backport):
   * offered as the session's base. The name is without its remote, as `Session.baseBranch` is. Null
   * when there's none, and for a session that can't change its base (a review, or one with a PR).
   */
  suggestedBase: { branch: string; ahead: number } | null;
  /** The branch's commits since it left the base, newest first, at most MAX_COMMITS. */
  commits: BranchCommit[];
  /** The one commit shown, when the diff is a single commit's rather than everything vs. the base. */
  commit: BranchCommit | null;
}

/** A commit on a session's branch, as the Changes panel's commit menu lists it. */
export interface BranchCommit {
  sha: string;
  subject: string;
  /** Commit time, ms since the epoch. */
  at: number;
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

/**
 * Who wrote a held review: a person with a role on the repo (held in Yours only), a bot (held
 * except in All reviewers), or someone with no role on the repo (held in every mode).
 */
export type HeldReviewAuthor = 'person' | 'bot' | 'outsider';

/** An inline comment of a held review. */
export interface HeldReviewComment {
  path: string;
  /** The line it's on in the current diff; null for an outdated comment. */
  line: number | null;
  body: string;
  url: string;
}

/** A review on the session's PR that waits for the user before it goes to Claude. */
export interface HeldReview {
  /** GitHub's node id for the review. */
  id: string;
  author: string;
  kind: HeldReviewAuthor;
  /** CHANGES_REQUESTED or COMMENTED. */
  state: string;
  body: string;
  url: string;
  submittedAt: string;
  comments: HeldReviewComment[];
  /** Inline comments beyond `comments`, left out (read them on GitHub). */
  moreComments: number;
}

/** PATCH /api/sessions/:id/code-review/findings/:key: a field left out keeps its value; the note is trimmed and clipped to MAX_FINDING_NOTE. */
export interface FindingChoiceRequest {
  dismissed?: boolean;
  note?: string;
}

export interface HeldReviewsRequest {
  action: 'send' | 'dismiss';
  /** HeldReview ids; ones already sent or dismissed are skipped. */
  ids: string[];
}

export interface HeldReviewsResult {
  /** How many were sent or dismissed. */
  done: number;
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
  /**
   * For a Review a PR session: the Meko name of its PR's design (`design-<slug>.md` or `.html`, from
   * the PR's `meko_design:` line), when this is that design fetched from Meko; null otherwise.
   */
  fromPr: string | null;
}

export interface DesignQuestionOption {
  label: string;
  detail: string | null;
}

/** A Meko item a design decision (or a recommendation) rests on, as AgentK cited it. */
export interface MekoRef {
  kind: 'memory' | 'knowledge';
  /** Meko's id for the item. */
  id: string;
  /** The datapack it's in, when AgentK said. */
  datapackId: string | null;
  /** The words AgentK relied on, as it read them. */
  excerpt: string | null;
}

/**
 * One of the design's Key Design Decisions (KDDs): a question AgentK asks the user, with the options it
 * sees, if any, or (`kind: 'decision'`) a decision AgentK made itself, with the alternatives it
 * weighed as its options and the one it picked as `recommended`.
 */
export interface DesignQuestion {
  /** Stable across revisions of the design: lowercase letters, digits and dashes. */
  id: string;
  /** `decision`: one AgentK made without asking. It's always resolved, and the user can change it. */
  kind: 'question' | 'decision';
  /** The question, or for a decision, the decision point. */
  question: string;
  /** Why it matters; for a decision, why AgentK chose as it did. */
  context: string | null;
  options: DesignQuestionOption[];
  /** The option AgentK recommends, or picked (an index into `options`), or null. */
  recommended: number | null;
  status: 'open' | 'resolved';
  /** What was decided, once resolved. */
  decision: string | null;
  /**
   * Who settled it, once resolved: the user (an answer, or a change to AgentK's decision) or
   * AgentK (its own decision, or a question left to it); null while open, or when not known.
   */
  decidedBy: 'user' | 'agentk' | null;
  /** The Meko items its decision or recommendation rests on (at most 5). */
  meko: MekoRef[];
}

/**
 * The user's answer to one of the design's open questions, or a change to one of AgentK's own
 * decisions. Answers aren't stored on the server: the browser keeps them until an approval goes to
 * AgentK, and after a request to revise until their questions are no longer open (a decision's
 * until it changes). Once sent, the server resolves the question with the answer as its decision,
 * in the questions file (a markdown design's questions only with an approval).
 */
export interface DesignAnswerInput {
  questionId: string;
  /** The option picked (an index into the question's options), or null for a custom answer. */
  choice: number | null;
  /** The picked option's label, to catch a revision that changed the options; null with `choice`. */
  label: string | null;
  /** With an option, an optional note on it; for a custom answer, the answer itself (required). */
  text: string;
  /**
   * For a change to one of AgentK's own decisions: the decision as the user saw it, so a change to
   * a decision AgentK has revised since is refused (409). Required for a decision.
   */
  decision?: string | null;
}

/**
 * The body of the merge and discard actions. Both save what the session learned to the project's
 * datapack first (the learn step), unless skipLearn.
 */
export interface CloseSessionRequest {
  skipLearn?: boolean;
}

/**
 * Answers to the design's open questions, changes to AgentK's own decisions, and other feedback on
 * it. An open question left out is sent as unanswered: AgentK goes with its recommendation, or its
 * own judgment when it has none; a decision left out stays as AgentK made it.
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

/**
 * What opening a project answers: the project, and whether its dedicated datapack is empty (one the
 * open made, or one Meko says has no memories and no knowledge), so the New session dialog offers a
 * learn session first. False with no datapack, and when Meko couldn't say in time.
 */
export interface OpenedProject extends Project {
  datapackEmpty: boolean;
}

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
  /**
   * The new base branch: a local branch, or one of origin's or upstream's, named without its remote.
   * Not for a review or a session with a PR, whose base is the PR's.
   */
  baseBranch?: string;
  /** Moves the session to the top of its project's list. */
  toTop?: boolean;
  /** The session's own code review settings (`Session.review`), replacing them; `{}` goes back to the project's. */
  review?: ReviewOverrides;
  /**
   * Turns Run to merge (Session.autoRun) on or off. On in Design waits for the approval; in Code it
   * queues Open PR, and in Review & PR the merge.
   */
  autoRun?: boolean;
}

export interface AttachDatapackRequest {
  id: string;
  name: string;
}

export interface CreateSessionRequest {
  name: string;
  /**
   * The harness the session runs on; it must be available (`AppState.harnesses`). Left out,
   * `[agents] default` while that's available, else the first available one (HARNESS_IDS order),
   * else the create is a 409 (NO_AGENT).
   */
  harness?: HarnessId;
  /**
   * The model the session's agent runs on, passed on every launch (Claude Code's and Kiro's
   * `--model`, OpenCode's config): an alias (`opus`), a full id or OpenCode's `provider/model`,
   * checked only for its form (`checkModel` in `src/shared/harnesses.ts`; a 400 otherwise). Left
   * out or empty, the agent's own default.
   */
  model?: string;
  /** The first prompt. Left out, a task starting at Design or Code uses `name`; one starting at Explore starts with none. */
  prompt?: string;
  /** Defaults to the repo's default branch. */
  baseBranch?: string;
  /** A main git repo to work in instead of the project's (one of the scanned repos). */
  repoPath?: string;
  /**
   * The branch, used as typed. Left out or empty, it's `<branchPrefix><slug of the name>`, with
   * `{login}` in the prefix filled in. An existing branch (local, or only on origin) is picked up
   * as it is, unless a running session uses it or another checkout has it.
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
  /** Run to merge (Session.autoRun) from the start: only for a task starting at Code. */
  autoRun?: boolean;
  /** The task's own code review settings (`Session.review`); left out, the project's apply. Not for a review. */
  review?: ReviewOverrides;
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
   * A learn session (`Session.purpose` `learn`): a task at Explore that reads the files and PRs the
   * brief names and saves what it learns to the project's dedicated datapack, so it needs Meko and a
   * dedicated datapack. Its task is made from the brief: `prompt`, `start` and the
   * images are ignored. Not with `kind: 'review-pr'` or `pr`.
   */
  learn?: LearnBrief;
  /**
   * Images for the first prompt (a review's notes), by the ids `POST /api/projects/:id/images`
   * returned for this project. They're kept apart from the task, and their paths go with it (a
   * review's notes) in Claude's prompts, for Claude to read.
   * A failed start keeps them in its request, but the images themselves are gone by then.
   */
  images?: string[];
  /** The open work item the session is for: it's linked to it as it's written, taking the item from any session it had. */
  workItemId?: string;
}

/** The stages a session can be moved to by hand. `review` is the Review & PR step. */
export type MoveTarget = 'explore' | 'design' | 'code' | 'review';

/** The most characters of instructions a move or Open PR takes (`MoveSessionRequest`, `OpenPrRequest`). */
export const INSTRUCTIONS_MAX = 10_000;

/** Moves a task session to another stage (see `src/shared/stages.ts` for the rules). */
export interface MoveSessionRequest {
  stage: MoveTarget;
  /**
   * Send the new stage's prompt. Left out, it follows promptByDefault, as the move dialog leaves it;
   * kept for pages from before the dialog dropped its checkbox. Ignored for Review & PR, which
   * picks up the branch's open PR, or, when there's work to push, does what Open PR does: with the
   * code review on, the session goes to Code and the review sends the PR prompt when it's done;
   * else the PR prompt goes now.
   */
  prompt?: boolean;
  /**
   * The task, for a session that has none yet (one started at Explore without a first prompt, before
   * the name became the task); the stage's start prompt then goes with it. Optional: without it, the
   * move's prompt has AgentK carry on from the conversation, as it does for a session that has a task.
   */
  task?: string;
  /**
   * The user's instructions for the new stage (at most INSTRUCTIONS_MAX characters; blank is
   * none): sent after the stage's prompt, or after the `moved` line when the move sends none.
   */
  instructions?: string;
}

/** The optional body of the open-pr action. */
export interface OpenPrRequest {
  /** The user's instructions, sent after the PR prompt (at most INSTRUCTIONS_MAX characters; blank is none). */
  instructions?: string;
}

export type SessionAction =
  | 'approve-design'
  /** Withdraws an approval waiting for AgentK's turn to end (`Session.approvalQueued`). */
  | 'cancel-approval'
  /** Sends the PR prompt, or mid-turn queues it for the turn's end (`Session.queued`); takes an optional OpenPrRequest. */
  | 'open-pr'
  /** Merges now, or queues the merge when only blockers that clear by themselves are left (`Session.mergeQueueable`). */
  | 'merge'
  /** Withdraws a queued Open PR or merge (`Session.queued`). */
  | 'cancel-queued'
  | 'resume'
  | 'archive'
  | 'discard'
  /** Review a PR: check out the PR's latest head and have AgentK sum up what changed. */
  | 'update-pr'
  /** Review a PR: end the review (archives the session and deletes its local review branch). */
  | 'close-review'
  /** Save what the session learned to its project's dedicated datapack (the learn step); 409 while Claude is busy or one runs. */
  | 'learn'
  /**
   * A task's code review, run now in Code or Review & PR (with the strategy Off, the default one).
   * It sends no PR prompt: Open PR later sends the PR with its line instead of reviewing again.
   */
  | 'review-run'
  /** A task's code review: stop it; Open PR then waits for Retry or Open anyway (not after one the user ran). */
  | 'review-cancel'
  /** A task's failed or cancelled code review: review again, reusing the steps already reported. */
  | 'review-retry'
  /** A task's failed or cancelled code review (not one the user ran): send the PR prompt without a `me_ak_review:` line. */
  | 'review-open-anyway'
  /** A task's code review whose findings wait on the user (`triage`): send them to the task, with the user's notes, leaving out the dismissed ones. */
  | 'review-send';

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
// /ws/term/:sessionId[?cols=&rows=&screen=&browser=&theme=&claim=1]: `cols` and `rows` are the
// browser terminal's size. A session is live on one screen at a time (src/server/screens.ts):
// `screen` is the page's id (one per page load), `browser` the browser's (to name "another tab in
// this browser"), and `claim=1` asks for the session as the socket opens (the terminal is on screen).
// `theme` (`dark` or `light`) is the browser's resolved terminal colors, which the server keeps
// (`Core.setTermTheme`) for the sessions started next, as it does a later `theme` message.
// Only the screen that has the session gets output and sends input and its size to Claude.
// Client → server JSON TermClientMessage (text frames). server → client: binary frames are raw
// terminal output (the screen as it is first, then what follows); text frames are JSON
// TermServerMessage.

export type ServerEvent =
  | { type: 'state'; state: AppState }
  | { type: 'notify'; projectId: string; sessionId: string; title: string; body: string }
  /**
   * A file added to a session's knowledge base finished indexing (`indexed`), failed (`failed`), or
   * AgentK stopped following it without an answer (`unknown`): still indexing after 15 minutes, or
   * Meko refused to say (a refused key, no status route).
   */
  | { type: 'knowledge'; sessionId: string; filename: string; result: 'indexed' | 'failed' | 'unknown' }
  /** A project datapack was shared with you: sent once per datapack. */
  | { type: 'invite'; datapackId: string; title: string; body: string }
  /** The answer to a client's `ping`. */
  | { type: 'pong' }
  /** The server is stopping to restart into an update Meko installed: sent right before its sockets close. */
  | { type: 'restarting' };

export type EventsClientMessage = { type: 'ping' };

export type TermClientMessage =
  | { type: 'input'; data: string }
  /** This terminal's size. The pane takes it while this viewer is the active one. */
  | { type: 'resize'; cols: number; rows: number }
  /** The terminal was used (focused, typed in): it becomes the active viewer and the pane takes its size. */
  | { type: 'active' }
  /** This screen shows the session (opened, switched to, page shown, terminal focused): it asks for it. */
  | { type: 'show' }
  /** This screen stopped showing it: a request is dropped; the screen that has it counts as away. */
  | { type: 'hide' }
  /** Move to this screen: take the session now. */
  | { type: 'move' }
  /** This browser's terminal colors changed (Settings → General → Terminal colors): sessions start with them. */
  | { type: 'theme'; theme: 'dark' | 'light' };

/** How long the screen that has a session keeps it, unused, from another screen that asks. */
export const SCREEN_IDLE_MS = 10_000;

/** Another screen, named for the one told: its browser and system, and whether it's this same browser. */
export type ScreenName = { name: string; sameBrowser: boolean };

/**
 * Which screen has the session, as the one told sees it. `here`: this one (`waiting` names a screen
 * that asked for it). `waiting`: this one asked, and gets it in `moveInMs` unless the other is used.
 * `elsewhere`: another screen has it (`other` is null once that screen closed).
 */
export type ScreenState =
  | { state: 'here'; waiting?: ScreenName }
  | { state: 'waiting'; other: ScreenName; moveInMs: number }
  | { state: 'elsewhere'; other: ScreenName | null };

export type TermServerMessage =
  | { type: 'exit'; reason: string }
  /**
   * The pane's size, sent on attach and when it changes. `active`: this viewer sets it; any other
   * viewer shows the pane at this size until it's used.
   */
  | { type: 'size'; cols: number; rows: number; active: boolean }
  /** `retry`: the failure may pass (the attach couldn't start), so reconnect with backoff; otherwise it's final. */
  | { type: 'error'; error: string; retry?: boolean }
  /** Which screen has the session: sent on connect and whenever it changes. */
  | ({ type: 'screen' } & ScreenState);

// ---- REST routes (all JSON; errors are { error: string } with a 4xx/5xx) ----
//
// Request bodies must be JSON (`Content-Type: application/json`); other body types get 415.
// Some errors add a machine-readable `code` next to `error`: DATAPACK_LIMIT (402, the Meko plan's
// datapack limit), MEKO_OFF (409).
// The server listens only on its unix socket (`Paths.webSock`), and Meko serves it to browsers at
// /ext/agentk/ with the prefix stripped: Meko decides who gets in, so every request on the socket is
// let in. Writes and WebSocket upgrades still refuse a cross-origin `Origin`.
//
// GET    /healthz                              → { ok: true, version, pid, startedAt }
// GET    /api/state                            → AppState
// GET    /api/repos                            → ReposResponse
// GET    /api/repos/branches?path=             → RepoBranches   (400 unless the path is a scanned repo or a project's)
// POST   /api/repos/clone     CloneRepoRequest → RepoInfo   (clones a network remote into the repos folder; an existing
//        clone of the same remote there is returned as it is, anything else there is a 409)
// GET    /api/meko/datapacks                   → MekoDatapack[]
// GET    /api/meko/datapacks/:id/people        → DatapackPeople   (the datapack must be in a project here)
// POST   /api/meko/datapacks/:id/people ShareDatapackRequest → ShareDatapackResult   (invites by email, or a list of
//        emails one after another; Meko emails them; fails with the first error when none went through; with `login`,
//        the email is remembered for that GitHub login; every address invited is counted in the share contacts)
// POST   /api/meko/datapacks/:id/people/invites/:inviteId/resend → DatapackPeople
// DELETE /api/meko/datapacks/:id/people/invites/:inviteId        → DatapackPeople   (revokes a pending invite)
// GET    /api/meko/datapacks/:id/share-link    → ShareLinkState   (the datapack's share link, or why there's none)
// PUT    /api/meko/datapacks/:id/share-link ShareLinkChange → ShareLink   (turns it on with a role, off, changes the
//        role or resets it; the token is never logged)
//        The six above answer 409 when AgentK can't share (Meko is off, or its API URL isn't known).
// POST   /api/meko/share-links/join JoinByLinkRequest → JoinByLinkResult   (joins the datapack a share link on Meko's
//        web app shares, then syncs the shares; 400 for any other URL, which never reaches Meko)
// GET    /api/share-contacts                   → ShareContact[]   (the people invited before, every project's, ranked:
//        often and lately first)
// DELETE /api/share-contacts/:email            → 204   (stops suggesting them; 404 when they weren't kept)
// POST   /api/invites/check                    → InviteCheck   (lists datapacks and syncs shares with that list; the Open project dialog's one read of Meko)
// POST   /api/invites/:datapackId/accept       → 204   (viewer invites; other invites are accepted by opening the project)
// POST   /api/invites/:datapackId/decline      → 204
// POST   /api/invites/:datapackId/later        → 204   (hides it here; the invite stays in Meko)
// POST   /api/projects        CreateProjectRequest → OpenedProject   (opens a project on a repo here, a clone, or a new repo;
//        409 when a new repo's folder is already taken)
// GET    /api/workshops                        → Workshop[]   (the bundled workshops, each with its open project and host)
// POST   /api/workshops/:id/start              → WorkshopStart   (builds the demo repo, opens it as a project and starts its
//        host; a workshop whose project is open goes to its host, starting one when none is open; 404 for an unknown
//        workshop, 500 with the reason when the demo can't be built)
// POST   /api/workshops/:id/go                 → 204   (the user's go at a level's pause, as "go" typed to its host does:
//        after the first look, the gotchas go into Meko and AgentK reviews again; 404 when the workshop isn't open,
//        409 when nothing waits for a go)
// GET    /api/projects/:id                     → Project   (404 once it's closed: the UI asks for a project it just
//        opened when a state push comes without it)
// PATCH  /api/projects/:id    UpdateProjectRequest → Project
// DELETE /api/projects/:id                     → 204   (closes it: the repo, branches and datapacks stay)
// GET    /api/projects/:id/branches            → string[]
// GET    /api/projects/:id/learn-scope?months=&where=&base=&repoPath= → LearnScope   (a learn session's estimate from
//        local git, for the New session dialog; 400 for months outside 0–LEARN_MONTHS_MAX, where over LEARN_TEXT_MAX,
//        a repo that isn't the project's or a scanned one, or a base in neither the repo nor its origin)
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
// GET    /api/github-link?ref=[&project=]  → LinkPlan   (a pasted GitHub PR or issue: link or owner/name#N; `project`, the one
//        on screen, is preferred among those on its repo; 400 for anything else or one GitHub can't find)
// POST   /api/github-link/start StartFromLinkRequest → StartFromLinkResult   (starts a Review a PR session on a PR, or a
//        task on an issue, in `projectId` or in a project it opens (cloning the repo if needed); 400 for a PR that isn't
//        open, 409 for a PR a running session reviews; then as POST /api/projects/:id/sessions)
// POST   /api/ask-agentk AskAgentkRequest  → AskAgentkResult   (an instruction in plain words, answered by one `claude -p`
//        with no tools, or Kiro's one-shot chat; 400 for empty or too long text, 403 when [ask_agentk] is off, 409 when no
//        coding agent is available, 429 with two asks already running,
//        502 when Claude Code fails, isn't signed in or its answer can't be read, 503 when it can't be run, 504 after
//        [ask_agentk] timeout_seconds. Closing the request stops Claude.)
// POST   /api/actions/run RunActionsRequest → RunActionsResult   (Ask AgentK's Do all: the actions checked against the state
//        now, those that fit run in order, each with its result; 400 for more than ACTIONS_MAX or none that fit)
// GET    /api/sessions/search?q=[&project=] → SessionMatch[]   (sessions, archived ones included, whose name, task, PR
//        title, branch or design has the words in `q` (at least half of them), best first, at most 20; in the
//        projects open, or only `project`; 400 for a `q` over SESSION_SEARCH_MAX characters or given twice, or two `project`s)
// GET    /api/work-items?status=closed         → WorkItem[]   (closed items, last closed first, at most 200; open ones are AppState.workQueue)
// POST   /api/work-items CreateWorkItemRequest → WorkItem   (400 for no title and no GitHub link to read one from, text over the
//        WORK_*_MAX limits, more than WORK_LINKS_MAX links, or a link parseWorkLink can't read; 502 when GitHub can't give the title)
// PATCH  /api/work-items/:id UpdateWorkItemRequest → WorkItem   (404 for an unknown item or session; 400 for bad values, or a move or
//        session for a closed item)
// PUT    /api/work-items/order ReorderWorkItemsRequest → 204   (409 when `ids` isn't exactly the open items)
// POST   /api/work-items/close CloseWorkItemsRequest → { closed }   (Dismiss all: closes the open items among `ids`; 400 for no list of ids)
// POST   /api/work-items/sync                  → WorkSync   (GitHub review requests and your Slack list's messages into the queue; 409 while one runs)
// GET    /api/work-items/by-link?link=         → WorkLinkMatch[]   (open and closed items holding the link's key, open first; 400 for a
//        link parseWorkLink can't read)
// GET    /api/usage                            → UsageOverview
// POST   /api/projects/:id/search { query }    → SearchResult[]
// POST   /api/projects/:id/sessions CreateSessionRequest → Session   (400 for an unknown kind or start, a harness that isn't available, a bad branch name, a
//        base branch neither the repo nor its origin has, a repo that isn't a scanned one, an `existingBranch` that doesn't exist, or a task's
//        `pr` that isn't open or whose head repo no remote of the repo is; 409 for a branch a running session uses or another checkout has, or
//        when no agent is available (NO_AGENT, naming any agent only Meko's connection keeps off). A harness that isn't
//        connected to Meko (`HarnessStatus.meko`) is a 400 naming the fix: Settings › Harness, or meko agent connect <id>. Review a PR:
//        400 without `pr`, for a PR that isn't open, or a repoPath that isn't a clone of the PR's repo; 400 for
//        `images` that aren't this project's staged images, or more than MAX_START_IMAGES; 400 for `autoRun` on anything but a
//        task starting at Code. A learn session (`learn`): 400 without
//        an `about`, for text over LEARN_TEXT_MAX or `months` outside 0–LEARN_MONTHS_MAX, with `kind: 'review-pr'` or `pr`, and
//        when Meko is off or the project has no dedicated datapack). Answers once
//        the session is written, still starting: the worktree and Claude start after (Session.startup shows where it
//        is). A worktree or PR fetch that fails then removes the session (and its images) and adds it to AppState.failedStarts.
// POST   /api/projects/:id/images  image bytes → StagedImage   (an image added in the New session dialog: checked as
//        for POST /api/sessions/:id/images, 404 for an unknown project. Saved under ~/.agent-k-local/pastes/.new/<project>/
//        until a session is created with it, which moves it to the session's folder; removed after a day otherwise,
//        or when the project is closed)
// PATCH  /api/sessions/:id    UpdateSessionRequest → Session   (renames it in AgentK and its Meko conversation; the
//        branch, worktree, Meko agent id and Claude Code's own name for the conversation stay; 400 for an empty or too
//        long name or one with control characters, 409 for an archived session. A new baseBranch is what the diff, the
//        PR and the prompts use from then on: 400 for a branch that isn't here, on origin or on upstream, or that has no
//        history in common with the session's; 409 for a review, a session with a PR, or one whose worktree is gone.
//        toTop moves it first in its project's list; 400 when it isn't a boolean, 409 for an archived session.
//        autoRun turns Run to merge on or off: on in Design waits for the approval, in Code it queues Open PR (sent now
//        when AgentK is at its prompt) and in Review & PR the merge, which then stays queued through any blocker; off
//        drops the gate it queued. 400 when it isn't a boolean, 409 for an archived session, a review, Explore, Merge &
//        Close, a design in plan mode, or while the merge is being requested)
// GET    /api/sessions/:id/base-branches       → string[]   (the branches the session's base can be: local, origin's
//        and upstream's, by name, newest commit first; 409 when its worktree is gone)
// DELETE /api/failed-starts/:id                → 204   (dismisses a failed start; 204 when it's already gone)
// POST   /api/sessions/:id/actions/:action     → Session   (action: SessionAction; approve-design takes an optional
//        DesignAnswersRequest, sent ahead of the approval: 400 for a bad answer or answers too long for one prompt, 409
//        when AgentK's questions changed since, in plan mode, or while Claude holds answers sent during a turn
//        (Session.promptsHeld). Mid-turn (Session.midTurn), with the mod live, the approval is queued for the turn's
//        end (Session.approvalQueued) and dropped if the turn changes the design; without the mod that's a 409. Once
//        sent, every open question is resolved, with its answer or left to AgentK (reopened on a move back to Design).
//        cancel-approval withdraws it: 409 when none is queued. open-pr while AgentK is mid-turn or Claude is starting is
//        queued for the turn's end (Session.queued), and 409 when Claude exited or waits on the user with no turn behind it;
//        it takes an optional OpenPrRequest (400 for instructions that aren't text or are too long), whose instructions
//        go after the PR prompt and are kept with a queued one (Session.queuedInstructions). Pressed again while one is
//        queued, a request with instructions replaces them (blank ones remove them); one without keeps them.
//        merge while the only blockers clear by themselves (Session.mergeQueueable) is queued and goes at the first poll
//        that finds none; any other blocker is a 409, and one found later drops a queued merge, mid-turn or not (the user
//        is notified). cancel-queued withdraws either, a queued Open PR up to the moment its prompt is typed: 409 when
//        none is queued, or while the merge is being requested). learn starts the learn step: 409 without a
//        dedicated datapack or Meko, while Claude is busy, starting or exited, or while one runs. merge and discard take
//        an optional CloseSessionRequest (400 for a skipLearn that isn't a boolean): skipLearn skips the learn step they
//        run first. A discard that runs one answers with the session still running (Session.discarding) and discards it
//        when the step ends; asked again with skipLearn, it discards now). review-run starts a task's code review now, in
//        Code or Review & PR (409 for a session that isn't a task, in another stage, in Review & PR before the PR is found or
//        while the last one's fixes wait to be taken, while one runs, and while AgentK is mid-turn, waiting on the user or
//        exited). review-cancel stops a task's running code review
//        (409 when none runs); review-retry reviews again, reusing the steps already reported, and review-open-anyway sends
//        the PR prompt without a me_ak_review line: both 409 unless the review failed or was cancelled; review-retry
//        409 too for a review the user ran in another stage than the task's now, and review-open-anyway for any review
//        the user ran. review-send sends the findings that wait on the user (a review in triage) to the task, with
//        their notes and without the dismissed ones: the fix prompt through the prompt queue, or the PR prompt; 409
//        when none wait, or when the task left the review's stage)
// POST   /api/sessions/:id/move MoveSessionRequest → Session   (400 for a stage the session can't move to, a
//        missing task, or instructions that aren't text or are too long; 409 while AgentK is mid-turn, in Merge & Close,
//        or a prompt (the stage's, or the user's instructions) is due and Claude is waiting or exited, and for a move to
//        Review & PR with work to push, while prompts from the user wait or a stopped code review holds the PR in Code
//        (Retry it, or Open anyway), whether the review is on or off now, as for Open PR; 502 when its prompt couldn't be sent: the move stays made, except a PR prompt's, which
//        goes back to where it was. A move to Review & PR that the code review takes over answers with the session
//        in Code, the review running)
// GET    /api/sessions/:id/design              → DesignDoc
// POST   /api/sessions/:id/design/answers DesignAnswersRequest → 204   (sends them for AgentK to revise the design,
//        and resolves the questions answered with their answers in questions.json, unless the questions come from a
//        markdown design's list; 409 outside Design, while AgentK works or Claude waits on something else (mid-turn with the mod live they're
//        sent anyway, and Claude takes them when the turn ends), while an approval is queued, or when AgentK's questions
//        changed since; 400 for a bad answer or answers too long for one prompt; 502 when sending failed)
// POST   /api/sessions/:id/ask/:askId AskAnswerRequest → 204   (answers the questions Claude is asking, Session.asking;
//        400 for an answer that doesn't fit them, 409 once they're gone: answered in the terminal, or Claude moved on)
// GET    /api/sessions/:id/code-review          → CodeReviewDetail   (a task's latest code review, or a code-review
//        session's own, with every candidate, kept or dropped, and what the task did with it; 404 when it has none)
// PATCH  /api/sessions/:id/code-review/findings/:key FindingChoiceRequest → CodeReviewFinding   (the user's note on a
//        finding of the round that waits on them, or its dismissal, a field left out keeping its value; no note and no
//        dismissal clears it; 400 for a field of the wrong type, 404 for a key that isn't a kept finding of that round, 409 when no findings wait)
// GET    /api/sessions/:id/discard-check       → DiscardCheck
// GET    /api/sessions/:id/changes?commit=     → SessionChanges   (commit: one of the branch's commits alone, by full sha)
// GET    /api/sessions/:id/changes/image?path=&side=old|new&commit= → image bytes (previewable types only)
// GET    /api/sessions/:id/changes/file?path=&old=&commit= → ChangedText   (a changed file's new side, the worktree's or
//        the commit's, with its patch diffed again; old: a rename's old path. Only a file that diff changes, inside the
//        repo, text, at most 1 MB: 400 for a path outside the repo, 404 for a file it doesn't change, one that isn't
//        there, isn't text or is larger, or a commit that isn't listed; 409 when the worktree is gone)
// POST   /api/sessions/:id/images  image bytes → PastedImage   (the body is the image, with an image/* Content-Type;
//        PNG, JPEG, GIF or WebP by its bytes, else 415; at most 20 MB, else 413; 409 for an archived session. Saved
//        under ~/.agent-k-local/pastes/<session>/ and removed when the session is archived)
// POST   /api/sessions/:id/files?name=  file bytes → PastedImage   (any file for Claude, as application/octet-stream,
//        else 415; empty 400, over 20 MB 413, 409 for an archived session. Saved owner-only under
//        ~/.agent-k-local/pastes/<session>/ with a name the server picks, ending in the safe part of `name`)
// POST   /api/sessions/:id/knowledge?name=  file bytes → KnowledgeUpload   (the body is the file, as
//        application/octet-stream, else 415; `name` is its base name, with an extension in KB_UPLOAD_EXTS, else 400;
//        empty 400, over MAX_KB_UPLOAD_BYTES 413; 409 for an archived session, with Meko off, or a project with no
//        dedicated datapack. Sent through Meko's presigned upload: upload-url, then the file to Meko's storage.
//        Meko's 400, 403, 413 and 415 come back with their status, its 402 (no room on the plan) as a 413; anything
//        else Meko or its storage answers (a refused key included), or either unreachable, is a 502)
// GET    /api/sessions/:id/comments            → ReviewComment[]
// POST   /api/sessions/:id/comments NewComment → ReviewComment
// PATCH  /api/sessions/:id/comments/:cid { body } → ReviewComment   (drafts only)
// DELETE /api/sessions/:id/comments/:cid       → 204               (drafts only)
// POST   /api/sessions/:id/comments/send       → SendCommentsResult
// GET    /api/sessions/:id/held-reviews        → HeldReview[]   (held for the user, oldest first)
// POST   /api/sessions/:id/held-reviews HeldReviewsRequest → HeldReviewsResult   (send to AgentK in Review & PR, or dismiss)
// GET    /api/settings                         → Settings   (reads the Meko on this computer and its Meko Cloud link again first,
//                                                           waiting 1.5 s at most, and Meko's list of agents in the
//                                                           background. The link is Meko's: its web UI's
//                                                           Settings › Meko Cloud, PUT /api/v1/settings/meko-cloud there)
// PUT    /api/settings        SettingsUpdate   → Settings   (bind and port are read-only: set them in config.toml;
//                                                           a lower idleWorktreesPerRepo or idleWorktreeDays removes
//                                                           the idle worktrees now beyond it; idleStopHours is a whole
//                                                           number from 0 to MAX_IDLE_STOP_HOURS; serverName is trimmed,
//                                                           at most 64 characters with no control characters, and
//                                                           '' goes back to the host name; callHomeEnabled,
//                                                           resolveConflicts and toolsEnabled are booleans; defaultHarness must be a
//                                                           known harness (while it isn't available, new sessions and
//                                                           Ask AgentK that name none start on the first available one);
//                                                           claudeEnabled, opencodeEnabled, opencodeDisableTracking, kiroEnabled
//                                                           and kiroTrustTools are booleans; the agents'
//                                                           commands are set in config.toml)
// POST   /api/settings/meko/claude-setup       → Settings   (runs Meko's setup for Claude Code with AgentK's Meko, only when
//                                                           the user asks and only while what Meko's capture needs is
//                                                           missing (the plugin, its settings); then checks Claude Code's
//                                                           Meko and Meko's list of agents again. The page connects the
//                                                           MCP server first, through Meko's own route (below))
// POST   /api/settings/meko/kiro-setup         → Settings   (the same for Kiro (`--client kiro`): runs only while the agent
//                                                           Kiro sessions are built from doesn't load its MCP servers)
//        Meko's own routes, which the page calls on its origin (not under BASE) with its sign-in, in Meko's
//        { data, error } envelope: POST /api/v1/agents/:id/connect connects an agent's `meko` MCP server (Settings ›
//        Harness's Connect to Meko, and before either setup above), then the page calls POST /api/harnesses/refresh.
//        AgentK's server only reads GET /api/v1/agents, on Meko's API listener with the client token: at startup,
//        on Settings open and Check again, after either setup, and when Meko's Meko Cloud link changes.
// POST   /_meko/changed       { what }         → 204        (Meko, on AgentK's socket: something AgentK reads from Meko changed;
//                                                           `cloud`, the Meko Cloud link, reads `meko token --json` again.
//                                                           Outside /api, a write from another origin is refused)
// POST   /api/harnesses/refresh                → HarnessStatus[]   (Check again in Settings: reads the version of every harness
//        that's turned on again now (one that's off isn't read), and Meko's list of agents; a change is pushed to every page)
// GET    /api/harnesses/:id/models             → HarnessModels   (the models New session offers for a harness whose list
//        comes from its CLI (`HarnessCaps.models` `cli`: `opencode models`, `kiro-cli chat --list-models`), read at most
//        every 10 minutes; an empty list when it can't be read, the harness is off, or its list is fixed (Claude Code's))
// POST   /api/harnesses/:id/update             → HarnessStatus   (runs the harness's own updater (`claude update`,
//        `opencode upgrade`, `kiro-cli update`), then reads its version again; 409 while one runs. Settings › Harness's
//        Update OpenCode and Update Kiro call it; the UI updates Claude Code through POST /api/claude/update, whose answer carries the outcome)
// POST   /api/claude/update                    → ClaudeCodeStatus   (runs `claude update`, Claude Code's own updater, then
//        reads its version again; the outcome is in `lastUpdate`, a failed update included; 409 while one runs. Called
//        by the too-old banner's Update Claude Code and by Settings › Harness's)
// GET    /api/update                           → UpdateStatus   (the version, and the update it came from: Meko installs
//        updates; the page checks, updates and sets Update automatically through Meko's own routes, on its origin:
//        GET /api/v1/extensions, POST /api/v1/extensions/agentk/check and /update, PATCH /api/v1/extensions/agentk)
// GET    /api/update/notes?from=&to=           → ReleaseNotes   (What's new: the feat, fix, perf and deps commits between
//        the two versions' tags (agentk-v… or v…), from GitHub; 400 unless both are versions and from is older,
//        502 when GitHub can't compare them)
