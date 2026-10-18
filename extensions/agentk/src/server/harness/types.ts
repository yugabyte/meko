import type { HarnessCaps, HarnessId } from '../../shared/harnesses.js';
import type { HarnessStatus } from '../../shared/types.js';
import type { Asker } from '../askagentk.js';
import type { Config } from '../config.js';
import type { Db } from '../db.js';
import type { GuardDecision, HookContext, HookPayload } from '../hooks.js';
import type { LauncherSpec } from '../launcher.js';
import type { Logger } from '../log.js';
import type { Paths } from '../paths.js';
import type { SessionRow } from '../store.js';

/** What the session manager gives a harness to launch a session with. */
export interface LaunchInput {
  row: SessionRow;
  /** Continue the session's conversation instead of starting it. */
  resume: boolean;
  /** The first prompt (or a continue prompt), or null. */
  prompt: string | null;
  /** The session's system-prompt lines (project, task, branch, workflow, Meko). */
  systemPrompt: string;
  /** Extra folders the agent may work in (the session's design folder). */
  addDirs: string[];
  /** AgentK's mod's folder (`modDir`), or null to launch without it. */
  modDir: string | null;
  /** Design in Claude Code's plan mode (sessions from before designs left it). */
  planMode: boolean;
  /** AgentK's session variables (AK_*, MEKO_*). */
  env: Record<string, string>;
  unsetEnv: string[];
  /** The session works on a PR head whose code isn't trusted (headtrust.ts): leave out the repo's own config. */
  untrusted: boolean;
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
 * A coding agent sessions can run on. Each adapter turns what AgentK wants into its CLI's terms:
 * the command line, its hook payloads as AgentK's events (Claude Code's hook shape, which every
 * adapter maps onto), what its guard and context hooks print, where its conversation and usage
 * are on disk, and how it answers Ask AgentK. Code outside the adapters reads a harness's
 * differences from here or from `HARNESS_CAPS`, not from its id; docs/design/harnesses.md lists the
 * few places that still name one.
 */
export interface Harness {
  id: HarnessId;
  caps: HarnessCaps;
  /**
   * Writes the files its sessions are launched with (AgentK's mod, Kiro's agent file) and reads its
   * version. Called at server start, in the background, and when it's turned on: AgentK's shared
   * hook files are written by Core first, and a harness that fails here only logs.
   */
  prepare(paths: Paths, cfg: Config): Promise<void>;
  /**
   * Before each launch: what may have changed since `prepare` (Kiro's derived agent), and what the
   * CLI needs settled first (Claude Code's folder trust). Never throws.
   */
  beforeLaunch(c: LaunchContext): Promise<void>;
  /**
   * AgentK's mod's folder for a launch (`--plugin-dir`), or null when it's off: turned off in the
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
   * the repo when a project opens, the worktrees folder at start, the repo again when the dialog
   * shows). `exact`: only an entry for `dir` itself counts, not one for a parent. Never throws.
   */
  trustFolder?(dir: string, log: Logger, exact?: boolean): Promise<void>;
  /** Text its folder-trust dialog shows on screen; the CLI fires no hooks until it's answered. */
  trustPrompt?: RegExp;
  /** The launcher for one launch. */
  launcher(paths: Paths, cfg: Config, l: LaunchInput): LauncherSpec;
  /** True when the system prompt goes in through the context hook at every start (no flag for it). */
  systemPromptByContext: boolean;
  /** A hook payload in the CLI's own shape as AgentK's event, or null to ignore it. */
  normalize(raw: HookPayload): HookPayload | null;
  /** What the guard hook prints to deny a tool call, with the reason. */
  deny(reason: string): GuardDecision;
  /** What the context hook prints to add `text` to the model's context. */
  context(text: string): HookContext;
  /** The paths a tool call (an event from `normalize`) would write, or null when it isn't a file edit. */
  editedPaths(p: HookPayload): string[] | null;
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
  /** Reads the session's turns since the last read into `usage_messages`. May throw. */
  readUsage(db: Db, row: SessionRow): UsageRead;
  /**
   * The transcript AgentK sends to the session's Meko conversation (with capture on). Absent when
   * the CLI's own Meko hooks send its turns (Kiro).
   */
  captureTranscript?(row: SessionRow): string;
  /** How it answers Ask AgentK. */
  ask: Asker;
  /** Its version and whether sessions can start on it. */
  status(cfg: Config): HarnessStatus;
  /** Reads its version again when the last read is stale (or `force`). */
  version(cfg: Config, force?: boolean): Promise<string | null>;
  /** Runs its own updater, then reads the version again. 409 while one runs. */
  update(cfg: Config): Promise<HarnessStatus>;
}
