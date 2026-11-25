import type { HarnessCaps, HarnessId } from '../../shared/harnesses.js';
import type { HarnessStatus } from '../../shared/types.js';
import type { TermTheme } from '../../shared/termcolors.js';

/** A harness's status as its CLI gives it: everything but Meko's connection (`HarnessStatus.meko`, `autoConnect`). */
export type CliStatus = Omit<HarnessStatus, 'meko' | 'autoConnect'>;
import type { Asker } from '../askagentk.js';
import type { Config } from '../config.js';
import type { Db } from '../db.js';
import type { GuardDecision, HookContext, HookPayload } from '../hooks.js';
import type { LauncherSpec } from '../launcher.js';
import type { Logger } from '../log.js';
import type { Paths } from '../paths.js';
import type { SessionRow } from '../store.js';
import type { ReviewLaunch } from '../review/runner.js';

/** A place in a session's transcript: its file and how long it was then. */
export interface TranscriptMark {
  file: string;
  offset: number;
}

/** What the session manager gives a harness to launch a session with. */
export interface LaunchInput {
  row: SessionRow;
  /** Continue the session's conversation instead of starting it. */
  resume: boolean;
  /** The first prompt (or a continue prompt), or null. */
  prompt: string | null;
  /** The model picked for the session (`sessions.model`), in the CLI's own terms; null for its default. */
  model: string | null;
  /** The session's system-prompt lines (project, task, branch, workflow, Meko). */
  systemPrompt: string;
  /** Extra folders the agent may work in (the session's design folder). */
  addDirs: string[];
  /** Kode's mod's folder (`modDir`), or null to launch without it. */
  modDir: string | null;
  /** Design in Claude Code's plan mode (sessions from before designs left it). */
  planMode: boolean;
  /** Kode's session variables (AK_*, MEKO_*). */
  env: Record<string, string>;
  /** The terminal colors the session starts with (`Core.termTheme`), for a CLI that can be told them. */
  termTheme: TermTheme;
  unsetEnv: string[];
  /** The session works on a PR head whose code isn't trusted (headtrust.ts): leave out the repo's own config. */
  untrusted: boolean;
  /**
   * Launch with Kode's session tools (tools.ts): the `kode_sessions` MCP server and its skill, in
   * the CLI's own terms. A harness whose agent file is shared by every session (Kiro's) has them in
   * that file instead (`prepare`, `beforeLaunch`), and the tools' route refuses a session without them.
   */
  tools: boolean;
  /**
   * A code-review session's subagents (review/runner.ts), one per model tier: a harness whose
   * review steps are `subagents` (`HARNESS_CAPS.reviewSteps`) defines them for the session, read-only.
   * Null for every other session.
   */
  review?: ReviewLaunch | null;
}

/** What a harness gets before each launch. */
export interface LaunchContext {
  paths: Paths;
  cfg: Config;
  log: Logger;
  row: SessionRow;
  /** The session's repo (its main checkout), or null when it's gone. */
  repo: string | null;
}

/** A session's new usage, read by `readUsage`. */
export interface UsageRead {
  /** Its totals moved. */
  changed: boolean;
  /** How full its context window is after its last turn (percent), when the harness's files say (Kiro's). */
  contextPercent?: number | null;
}

/**
 * A coding agent sessions can run on. Each adapter turns what Kode wants into its CLI's terms:
 * the command line, its hook payloads as Kode's events (Claude Code's hook shape, which every
 * adapter maps onto), what its guard and context hooks print, where its conversation and usage
 * are on disk, and how it answers Ask Kode. Code outside the adapters reads a harness's
 * differences from here or from `HARNESS_CAPS`, not from its id; docs/design/harnesses.md lists the
 * few places that still name one.
 */
export interface Harness {
  id: HarnessId;
  caps: HarnessCaps;
  /**
   * Writes the files its sessions are launched with (Kode's mod, Kiro's agent file) and reads its
   * version. Called at server start, in the background, and when it's turned on: Kode's shared
   * hook files are written by Core first, and a harness that fails here only logs.
   */
  prepare(paths: Paths, cfg: Config): Promise<void>;
  /**
   * Before each launch: what may have changed since `prepare` (Kiro's derived agent), and what the
   * CLI needs settled first (Claude Code's folder trust). Never throws.
   */
  beforeLaunch(c: LaunchContext): Promise<void>;
  /**
   * Kode's mod's folder for a launch (`--plugin-dir`), or null when it's off: turned off in the
   * config, its files missing, or the CLI too old. Only a harness with `caps.mod` has it.
   */
  modDir?(paths: Paths, cfg: Config): Promise<string | null>;
  /**
   * Whether a prompt rendered now may name the mod's tools: `modDir` as far as is known now. A
   * prompt is often rendered before the launch that settles it, so a version not read yet counts.
   */
  offersMod?(paths: Paths, cfg: Config): boolean;
  /**
   * Marks a folder as trusted for a CLI that asks before working in one (Claude Code's folder trust:
   * the repo when a project opens, the repo again when the dialog shows). Only an entry for `dir`
   * itself counts, never one for a parent. Never throws.
   */
  trustFolder?(dir: string, log: Logger): Promise<void>;
  /** Text its folder-trust dialog shows on screen; the CLI fires no hooks until it's answered. */
  trustPrompt?: RegExp;
  /** Text its first-run setup shows on screen (Claude Code's text style and sign-in); no hooks fire until it's done either. */
  setupPrompt?: RegExp;
  /** The launcher for one launch. */
  launcher(paths: Paths, cfg: Config, l: LaunchInput): LauncherSpec;
  /** True when the system prompt goes in through the context hook at every start (no flag for it). */
  systemPromptByContext: boolean;
  /** A hook payload in the CLI's own shape as Kode's event, or null to ignore it. */
  normalize(raw: HookPayload): HookPayload | null;
  /** What the guard hook prints to deny a tool call, with the reason. */
  deny(reason: string): GuardDecision;
  /** What the context hook prints to add `text` to the model's context. */
  context(text: string): HookContext;
  /** The paths a tool call (an event from `normalize`) would write, or null when it isn't a file edit. */
  editedPaths(p: HookPayload): string[] | null;
  /**
   * For a CLI without Kode's mod: the key of the Meko tool call a `PreToolUse` or `PostToolUse`
   * event (from `normalize`) is about, the same at its start and its end, or null when it's another
   * tool. The session's dot shows Meko's m while a call is open (`MekoCalls`).
   */
  mekoTool?(p: HookPayload): string | null;
  /**
   * For a CLI that picks its own conversation id (`agent_session_id`): the one a session started in
   * its worktree, for a session no hook told it to. The session manager asks only while the session
   * runs, owns its worktree and can't resume, and keeps what it finds.
   */
  findConversation?(row: SessionRow): string | null;
  /** The session's conversation is on disk, so a launch resumes it rather than starting over. */
  canResume(row: SessionRow): boolean;
  /** The files the CLI writes as a session works (its transcript): their times stand in for missing hooks. */
  activityFiles(row: SessionRow): string[];
  /**
   * Where the session's transcript ends now, taken as a prompt goes between turns, for
   * `promptStarted`. Absent (or null) when the CLI's record can't tell whether a prompt ran.
   */
  transcriptMark?(row: SessionRow): TranscriptMark | null;
  /** Whether a prompt sent at `mark` started its turn: its text is in a user message written since. */
  promptStarted?(row: SessionRow, mark: TranscriptMark, text: string): boolean;
  /** Reads the session's turns since the last read into `usage_messages`. May throw. */
  readUsage(db: Db, row: SessionRow): UsageRead;
  /**
   * The transcript Kode sends to the session's Meko conversation (with capture on). Absent when
   * the CLI's own Meko hooks send its turns (Kiro).
   */
  captureTranscript?(row: SessionRow): string;
  /** Removes what Kode keeps for a session that can never run again (its project is gone): OpenCode's session folder. */
  forget?(row: SessionRow): void;
  /**
   * Meko's URL or key changed, or Meko was turned on or off: drops what was written with the old ones
   * (OpenCode's Meko server file, which holds the key). The next launch writes it again.
   */
  mekoChanged?(paths: Paths): void;
  /** How it answers Ask Kode. */
  ask: Asker;
  /** Its version and whether sessions could start on it, Meko's connection aside (`Harnesses.statusOf` adds it). */
  status(cfg: Config): CliStatus;
  /** Reads its version again when the last read is stale (or `force`). */
  version(cfg: Config, force?: boolean): Promise<string | null>;
  /**
   * The models its CLI lists, for New session (`HarnessCaps.models` `cli`): empty when they can't be
   * read (a CLI that fails, or Kiro signed out). `Harnesses.models` keeps the answer.
   */
  models?(cfg: Config): Promise<string[]>;
  /** Runs its own updater, then reads the version again. 409 while one runs. */
  update(cfg: Config): Promise<CliStatus>;
}
