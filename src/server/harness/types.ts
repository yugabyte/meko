import type { HarnessCaps, HarnessId } from '../../shared/harnesses.js';
import type { HarnessStatus } from '../../shared/types.js';
import type { Config } from '../config.js';
import type { GuardDecision, HookContext, HookPayload } from '../hooks.js';
import type { LauncherSpec } from '../launcher.js';
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
  /** Plugins to load (AgentK's mod; Claude Code only). */
  pluginDirs: string[];
  /** Design in Claude Code's plan mode (sessions from before designs left it). */
  planMode: boolean;
  /** AgentK's session variables (AK_*, MEKO_*). */
  env: Record<string, string>;
  unsetEnv: string[];
}

/**
 * A coding agent sessions can run on. Each adapter turns what AgentK wants into its CLI's terms:
 * the command line, its hook payloads as AgentK's events (Claude Code's hook shape, which every
 * adapter maps onto), and what its guard and context hooks print.
 */
export interface Harness {
  id: HarnessId;
  caps: HarnessCaps;
  /** How the CLI is run (`[claude] command`, `[kiro] command`). */
  command(cfg: Config): string;
  /** Turned on in the config. */
  enabled(cfg: Config): boolean;
  /** Writes the files its sessions are launched with (hook scripts, settings, agent files). Called at server start; never throws. */
  prepare(paths: Paths, cfg: Config): Promise<void>;
  /** Before each launch: refreshes what may have changed since `prepare` (Kiro's derived agent). Never throws. */
  beforeLaunch?(paths: Paths, cfg: Config): void;
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
  /** The session's conversation is on disk, so a launch resumes it rather than starting over. */
  canResume(row: SessionRow): boolean;
  /** Its version and whether sessions can start on it. */
  status(cfg: Config): HarnessStatus;
  /** Reads its version again when the last read is stale (or `force`). */
  version(cfg: Config, force?: boolean): Promise<string | null>;
  /** Runs its own updater, then reads the version again. 409 while one runs. */
  update(cfg: Config): Promise<HarnessStatus>;
}
