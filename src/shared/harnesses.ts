/**
 * The coding agents (harnesses) a session can run on, and what each can do. The server and the web
 * UI gate features on the same capabilities, so a feature only Claude Code has never shows for a
 * session on another harness. The adapters are in `src/server/harness/`.
 */

export const HARNESS_IDS = ['claude', 'kiro'] as const;
export type HarnessId = (typeof HARNESS_IDS)[number];

/** The harness sessions run on when nothing else says (`[agents] default`). */
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
  /** A hook says when it waits on the user (a permission prompt). */
  needsInputEvent: boolean;
  /** It reports the account's plan limits. */
  planLimits: boolean;
  /** What its usage is counted in: tokens priced at API list prices, or the credits it bills. */
  usage: 'tokens' | 'credits';
}

export const HARNESS_CAPS: Record<HarnessId, HarnessCaps> = {
  claude: { label: 'Claude Code', short: 'Claude', mod: true, artifacts: true, needsInputEvent: true, planLimits: true, usage: 'tokens' },
  kiro: { label: 'Kiro', short: 'Kiro', mod: false, artifacts: false, needsInputEvent: false, planLimits: false, usage: 'credits' },
};

export function isHarnessId(v: unknown): v is HarnessId {
  return typeof v === 'string' && (HARNESS_IDS as readonly string[]).includes(v);
}

/** A stored harness id as one this version knows; an unknown one (a newer install's) reads as Claude Code. */
export function harnessOf(v: unknown): HarnessId {
  return isHarnessId(v) ? v : DEFAULT_HARNESS;
}
