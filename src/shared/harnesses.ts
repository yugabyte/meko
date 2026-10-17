/**
 * The coding agents (harnesses) a session can run on, and what each can do. The server gates
 * AgentK's mod and Artifact designs on these capabilities, and the server and the web UI both read
 * a session's usage as tokens or credits from them. The adapters are in `src/server/harness/`.
 */

export const HARNESS_IDS = ['claude', 'kiro'] as const;
export type HarnessId = (typeof HARNESS_IDS)[number];

/** The harness `[agents] default` names when config.toml doesn't (the first available one runs while it isn't). */
export const DEFAULT_HARNESS: HarnessId = 'claude';

export interface HarnessCaps {
  /** Its name in anything people read: "Claude Code", "Kiro". */
  label: string;
  /** Its short name for the process and its terminal: "Claude exited", "Resume Kiro". */
  short: string;
  /** AgentK's mod (src/mod/agentk) runs in it: prompts mid-turn, AskUserQuestion, the learn fork, interrupted turns. */
  mod: boolean;
  /** It has the Artifact tool, so a design can be a published page. */
  artifacts: boolean;
  /** What its usage is counted in: tokens priced at API list prices, or the credits it bills. */
  usage: 'tokens' | 'credits';
}

export const HARNESS_CAPS: Record<HarnessId, HarnessCaps> = {
  claude: { label: 'Claude Code', short: 'Claude', mod: true, artifacts: true, usage: 'tokens' },
  kiro: { label: 'Kiro', short: 'Kiro', mod: false, artifacts: false, usage: 'credits' },
};

/** Why nothing can start when no harness is available: a new session, Ask AgentK, the banner. */
export const NO_AGENT =
  'No coding agent is available: install Claude Code or Kiro CLI where AgentK Local runs, or turn one on in Settings → Harness.';

export function isHarnessId(v: unknown): v is HarnessId {
  return typeof v === 'string' && (HARNESS_IDS as readonly string[]).includes(v);
}

/** A stored harness id as one this version knows; an unknown one (a newer install's) reads as Claude Code. */
export function harnessOf(v: unknown): HarnessId {
  return isHarnessId(v) ? v : DEFAULT_HARNESS;
}

/** The short name of a session's agent, for text about its process and terminal: "Claude", "Kiro". */
export function agentName(s: { harness: string }): string {
  return HARNESS_CAPS[harnessOf(s.harness)].short;
}
