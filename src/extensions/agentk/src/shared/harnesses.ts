/**
 * The coding agents (harnesses) a session can run on, and what each can do. The server gates
 * Kode's mod and Artifact designs on these capabilities, and the server and the web UI both read
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
  /** Kode's mod (src/mod/agentk) runs in it: prompts mid-turn, AskUserQuestion, the learn fork, interrupted turns. */
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
  /**
   * How a code review runs its steps (docs/design/code-review.md): `subagents`, each pass and
   * scorer as a subagent defined for the review session, on its tier's model; `turns`, each as a
   * prompt of its own after clearing the conversation, for a CLI with no subagents Kode can define.
   */
  reviewSteps: 'subagents' | 'turns';
  /**
   * The models New session offers for it besides Default and Other…: a fixed list (Claude Code's
   * aliases, which follow its newest models), or `cli`, read from the CLI (`Harness.models`), since
   * its models depend on the providers and plan the user has.
   */
  models: readonly ModelChoice[] | 'cli';
  /**
   * Kode connects it to Meko by itself, once (`autoConnectDue`, `HarnessStatus.autoConnect`): its
   * `meko` server is a file Meko edits. Not Claude Code, whose Meko setup changes only when the user asks.
   */
  autoConnect: boolean;
}

/** A model New session offers: the name passed to the CLI, and what the picker shows. */
export interface ModelChoice {
  id: string;
  label: string;
}

/** Claude Code's model aliases (`claude --model`), largest first. */
const CLAUDE_MODELS: readonly ModelChoice[] = [
  { id: 'fable', label: 'Fable' },
  { id: 'opus', label: 'Opus' },
  { id: 'sonnet', label: 'Sonnet' },
  { id: 'haiku', label: 'Haiku' },
];

/** The most characters a session's model name may have. */
export const MODEL_MAX = 200;

/**
 * Why `model` can't be passed to an agent, or null when it can: Kode checks only its form (a
 * name the CLI doesn't know fails in the CLI). Letters, digits and `._:/@[]-`, not starting with
 * `-` (it would read as an option), at most MODEL_MAX characters: an alias (`opus`), a full id
 * (`claude-opus-5-5`, `opus[1m]`) or OpenCode's `provider/model`.
 */
export function checkModel(model: string): string | null {
  if (!model) return 'Name a model.';
  if (model.length > MODEL_MAX) return `A model name has at most ${MODEL_MAX} characters.`;
  if (model.startsWith('-') || !/^[A-Za-z0-9._:/@[\]-]+$/.test(model)) {
    return `${model} isn't a model name: use letters, digits and . _ : / @ [ ] - only.`;
  }
  return null;
}

/** The label of a model on a harness's fixed list (`Opus`), else the model as given. */
export function modelLabel(harness: HarnessId, model: string): string {
  const list = HARNESS_CAPS[harness].models;
  return (list !== 'cli' && list.find((m) => m.id === model)?.label) || model;
}

export const HARNESS_CAPS: Record<HarnessId, HarnessCaps> = {
  claude: { label: 'Claude Code', short: 'Claude', login: 'claude auth login', mod: true, artifacts: true, usage: 'tokens', labelModels: false,
    userModels: false, minVersion: null, loads: 'its plugins, MCP servers and startup hooks', reviewSteps: 'subagents',
    models: CLAUDE_MODELS, autoConnect: false },
  opencode: { label: 'OpenCode', short: 'OpenCode', login: 'opencode auth login', mod: false, artifacts: false, usage: 'tokens', labelModels: true,
    userModels: true, minVersion: '2.0.0', upgrade: 'opencode upgrade --major', loads: 'its plugins and MCP servers', reviewSteps: 'subagents',
    models: 'cli', autoConnect: true },
  kiro: { label: 'Kiro', short: 'Kiro', login: 'kiro-cli login', mod: false, artifacts: false, usage: 'credits', labelModels: true,
    userModels: false, minVersion: '2.32.0', loads: 'its agent and MCP servers', reviewSteps: 'turns', models: 'cli', autoConnect: true },
};

/** Why nothing can start when no harness is available: a new session, Ask Kode, the banner. */
export const NO_AGENT =
  'No coding agent is available: install Claude Code, OpenCode or Kiro CLI where Kode runs, or turn one on in Settings → Harness.';

/** Why an agent that's otherwise ready isn't offered: Meko doesn't report it connected (`HarnessStatus.meko`). */
export function notConnectedText(id: HarnessId): string {
  return `${HARNESS_CAPS[id].label} isn't connected to Meko: connect it in Settings › Harness, or run meko agent connect ${id}`;
}

/** NO_AGENT, then the fix for each agent only Meko's connection keeps off (`notConnectedText`). */
export function noAgentText(notConnected: HarnessId[]): string {
  return [NO_AGENT, ...notConnected.map((id) => `${notConnectedText(id)}.`)].join(' ');
}

/**
 * Meko's report keeps an agent from new sessions: its `meko` MCP server is `none`, `stale` or
 * `elsewhere`. Not `missing`: Meko looks for agents only on its own PATH and a few folders, never at
 * Kode's `[<id>] command`, so an agent Kode runs can be missing from its list with nothing to fix.
 */
export function mekoGates(meko: { state: string } | null): boolean {
  return !!meko && meko.state !== 'connected' && meko.state !== 'missing';
}

/** The agent is ready for sessions but for Meko: on, new enough, and Meko reports it isn't connected (`mekoGates`). */
export function onlyMekoBlocks(h: { enabled: boolean; version: string | null; minVersion: string | null; meko: { state: string } | null }): boolean {
  return h.enabled && h.version !== null && (!h.minVersion || versionAtLeast(h.version, h.minVersion)) && mekoGates(h.meko);
}

/**
 * An agent Kode connects to Meko by itself, unless it already tried: one it may
 * (`HarnessCaps.autoConnect`), ready
 * but for Meko (`onlyMekoBlocks`), and with no `meko` server or a stale one of Meko's own. Never one
 * pointing elsewhere, which a connect would replace.
 */
export function autoConnectDue(h: { id: HarnessId; enabled: boolean; version: string | null; minVersion: string | null; meko: { state: string } | null }): boolean {
  return HARNESS_CAPS[h.id].autoConnect && onlyMekoBlocks(h) && (h.meko?.state === 'none' || h.meko?.state === 'stale');
}

/** Whether dotted version `v` is `min` or later (numeric parts; `2.1.10` is after `2.1.9`). */
export function versionAtLeast(v: string, min: string): boolean {
  const a = v.split('.').map(Number);
  const b = min.split('.').map(Number);
  for (let i = 0; i < Math.max(a.length, b.length); i++) {
    const x = a[i] || 0;
    const y = b[i] || 0;
    if (x !== y) return x > y;
  }
  return true;
}

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
