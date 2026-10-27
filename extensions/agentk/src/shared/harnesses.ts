/**
 * The coding agents (harnesses) a session can run on, and what each can do. The server gates
 * AgentK's mod and Artifact designs on these capabilities, and the server and the web UI both read
 * a session's usage as tokens or credits from them. The adapters are in `src/server/harness/`.
 */

export const HARNESS_IDS = ['claude', 'opencode', 'kiro'] as const;
export type HarnessId = (typeof HARNESS_IDS)[number];

/** The harness `[agents] default` names when config.toml doesn't (the first available one runs while it isn't). */
export const DEFAULT_HARNESS: HarnessId = 'claude';

export interface HarnessCaps {
  /** Its name in anything people read: "Claude Code", "OpenCode", "Kiro". */
  label: string;
  /** Its short name for the process and its terminal: "Claude exited", "Resume Kiro". */
  short: string;
  /** The command that signs it in, for what Settings says when it doesn't answer: "kiro-cli login". */
  login: string;
  /** AgentK's mod (src/mod/agentk) runs in it: prompts mid-turn, AskUserQuestion, the learn fork, interrupted turns. */
  mod: boolean;
  /** It has the Artifact tool, so a design can be a published page. */
  artifacts: boolean;
  /** What its usage is counted in: tokens priced at API list prices, or the credits it bills. */
  usage: 'tokens' | 'credits';
  /** A by-model usage row names it before the model ("Kiro · auto", "OpenCode · claude-sonnet-4-5"): its model ids don't say which agent ran them. */
  labelModels: boolean;
  /**
   * Its model ids can be names the user gave (models of a provider set up in its own config), so the
   * usage report sends only the ones the price table knows, and `other` for the rest.
   */
  userModels: boolean;
  /** The oldest version sessions start on (the one its hooks and files were checked against), or null for any. */
  minVersion: string | null;
  /**
   * The command that brings a version older than `minVersion` up to it, when its own update can't
   * (OpenCode 1's `opencode upgrade` stays on 1.x), for what Settings and the installer say.
   */
  upgrade?: string;
  /** What it loads before it takes a session's first prompt, for the Starting view: "Kiro loads <this> first". */
  loads: string;
}

export const HARNESS_CAPS: Record<HarnessId, HarnessCaps> = {
  claude: { label: 'Claude Code', short: 'Claude', login: 'claude auth login', mod: true, artifacts: true, usage: 'tokens', labelModels: false,
    userModels: false, minVersion: null, loads: 'its plugins, MCP servers and startup hooks' },
  opencode: { label: 'OpenCode', short: 'OpenCode', login: 'opencode auth login', mod: false, artifacts: false, usage: 'tokens', labelModels: true,
    userModels: true, minVersion: '2.0.0', upgrade: 'opencode upgrade --major', loads: 'its plugins and MCP servers' },
  kiro: { label: 'Kiro', short: 'Kiro', login: 'kiro-cli login', mod: false, artifacts: false, usage: 'credits', labelModels: true,
    userModels: false, minVersion: '2.32.0', loads: 'its agent and MCP servers' },
};

/** Why nothing can start when no harness is available: a new session, Ask AgentK, the banner. */
export const NO_AGENT =
  'No coding agent is available: install Claude Code, OpenCode or Kiro CLI where AgentK runs, or turn one on in Settings → Harness.';

export function isHarnessId(v: unknown): v is HarnessId {
  return typeof v === 'string' && (HARNESS_IDS as readonly string[]).includes(v);
}

/** A stored harness id as one this version knows; an unknown one (a newer install's) reads as Claude Code. */
export function harnessOf(v: unknown): HarnessId {
  return isHarnessId(v) ? v : DEFAULT_HARNESS;
}

/** The short name of a session's agent, for text about its process and terminal: "Claude", "OpenCode", "Kiro". */
export function agentName(s: { harness: string }): string {
  return HARNESS_CAPS[harnessOf(s.harness)].short;
}
