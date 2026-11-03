import { HARNESS_IDS, harnessOf, mekoGates, noAgentText, notConnectedText, onlyMekoBlocks, type HarnessId } from '../../shared/harnesses.js';
import type { HarnessMeko, HarnessStatus } from '../../shared/types.js';
import type { ClaudeCode } from '../claudecode.js';
import type { Config } from '../config.js';
import type { Logger } from '../log.js';
import { harnessMeko, type MekoAgentList } from '../mekoagents.js';
import { openCodeHasOwnMeko } from '../opencode.js';
import type { Paths } from '../paths.js';
import { HttpError } from '../projects.js';
import { claudeHarness } from './claude.js';
import { kiroHarness } from './kiro.js';
import { openCodeHarness } from './opencode.js';
import type { Harness } from './types.js';

export type { Harness, LaunchInput } from './types.js';

/** How long a harness's model list is kept (`Harnesses.models`). */
export const MODELS_TTL_MS = 10 * 60 * 1000;

/**
 * Every harness sessions can run on, by id (src/shared/harnesses.ts). A new session starts only on
 * one Meko doesn't report unconnected (`HarnessStatus.meko` `none`, `stale` or `elsewhere`, from
 * Meko's list of agents, `mekoagents.ts`; `mekoGates`), while there's a list to go by: none ever
 * read, or a Meko too old to list agents, gates nothing, and neither does one Meko didn't find.
 */
export class Harnesses {
  private all: Record<HarnessId, Harness>;
  private mekoList: () => MekoAgentList | null;
  private rereadMeko: () => Promise<void>;
  /** Each harness's last model list (or the read under way), and what it was read with: a new version or setting reads again. */
  private modelLists = new Map<HarnessId, { at: number; key: string; models: Promise<string[]>; read?: string[] }>();

  constructor(deps: {
    claudeCode: ClaudeCode; paths: Paths; log: Logger; changed: () => void;
    kiroVersion?: (cmd: string) => Promise<string | null>; openCodeVersion?: (cmd: string) => Promise<string | null>;
    /** Meko's list of agents to gate on, or null for none (`MekoAgents.current`); left out, nothing is gated. */
    meko?: () => MekoAgentList | null;
    /** Reads Meko's list again (`MekoAgents.refresh`), before a new session the list kept would refuse. */
    refreshMeko?: () => Promise<void>;
  }) {
    this.mekoList = deps.meko ?? (() => null);
    this.rereadMeko = deps.refreshMeko ?? (async () => undefined);
    this.all = {
      claude: claudeHarness(deps),
      opencode: openCodeHarness({ paths: deps.paths, log: deps.log, changed: deps.changed, readVersion: deps.openCodeVersion, ownMeko: () => this.openCodeOwnMeko() }),
      kiro: kiroHarness({ log: deps.log, changed: deps.changed, readVersion: deps.kiroVersion }),
    };
  }

  /** A harness's `meko` MCP server as Meko reports it, or null while nothing is gated. */
  mekoOf(id: HarnessId): HarnessMeko | null {
    const list = this.mekoList();
    return list ? harnessMeko(list, id) : null;
  }

  /**
   * OpenCode has a `meko` MCP server of its own, which AgentK then leaves alone (`MekoStatus.opencode`):
   * any Meko reports, else (nothing gated, or Meko didn't find it) one in its own config.
   */
  openCodeOwnMeko(): boolean {
    const m = this.mekoOf('opencode');
    return m && m.state !== 'missing' ? m.state !== 'none' : openCodeHasOwnMeko();
  }

  /** A harness's status: its CLI's, with Meko's connection, which `available` then requires (`mekoGates`). */
  statusOf(h: Harness, cfg: Config): HarnessStatus {
    const s = h.status(cfg);
    const meko = this.mekoOf(h.id);
    return { ...s, meko, available: s.available && !mekoGates(meko) };
  }

  /** A session's harness (one stored by a newer version reads as Claude Code). */
  of(id: unknown): Harness {
    return this.all[harnessOf(id)];
  }

  list(): Harness[] {
    return HARNESS_IDS.map((id) => this.all[id]);
  }

  status(cfg: Config): HarnessStatus[] {
    return this.list().map((h) => this.statusOf(h, cfg));
  }

  /** NO_AGENT, naming the fix for each agent only Meko's connection keeps off. */
  noAgent(cfg: Config): string {
    return noAgentText(this.status(cfg).filter(onlyMekoBlocks).map((s) => s.id));
  }

  /**
   * Reads every harness's version again once the last read is stale (`force`: now), since the CLIs
   * update themselves. Nothing polls: this runs when a page opens, and on Check again.
   */
  async refresh(cfg: Config, force = false): Promise<HarnessStatus[]> {
    // Check again reads the model lists again too (an agent signed in or set up since), at their next ask.
    if (force) for (const kept of this.modelLists.values()) kept.at = 0;
    await Promise.all(this.list().map((h) => h.version(cfg, force)));
    return this.status(cfg);
  }

  /**
   * The models a harness's CLI lists for New session (`HarnessCaps.models` `cli`), kept MODELS_TTL_MS
   * and shared by reads meanwhile; empty for one whose list is fixed, that's off or not installed, or
   * whose list can't be read.
   */
  async models(cfg: Config, id: HarnessId): Promise<string[]> {
    const h = this.all[id];
    const s = h.status(cfg);
    if (!h.models || !s.enabled || s.version === null) return [];
    const key = `${s.version}|${JSON.stringify(cfg[id])}`;
    const kept = this.modelLists.get(id);
    if (kept && kept.key === key && Date.now() - kept.at < MODELS_TTL_MS) return kept.models;
    // The list last read stays until the new one lands, for `modelsKept`.
    const entry: { at: number; key: string; models: Promise<string[]>; read?: string[] } = { at: Date.now(), key, models: Promise.resolve([]), read: kept?.read };
    entry.models = h.models(cfg).catch(() => [] as string[]).then((list) => {
      entry.read = list;
      return list;
    });
    this.modelLists.set(id, entry);
    return entry.models;
  }

  /**
   * `models` without waiting: the list last read (one that has expired too, while the next read is
   * under way, which this starts), or null while none has been. For answers that mustn't wait on a
   * CLI, such as the session tools' `list`.
   */
  modelsKept(cfg: Config, id: HarnessId): string[] | null {
    void this.models(cfg, id);
    return this.modelLists.get(id)?.read ?? null;
  }

  /**
   * What runs when nobody picks: `[agents] default` while it's available, else the first available
   * harness (HARNESS_IDS order), else null (no agent is installed, turned on, new enough and
   * connected to Meko).
   */
  defaultFor(cfg: Config): Harness | null {
    const h = this.of(cfg.agents.default);
    return this.statusOf(h, cfg).available ? h : (this.list().find((x) => this.statusOf(x, cfg).available) ?? null);
  }

  /**
   * `defaultFor`, after reading versions again (`reread`), so a read that failed once, or an agent
   * installed or updated since, doesn't refuse until the next page load.
   */
  async ensureDefault(cfg: Config): Promise<Harness | null> {
    const asked = this.of(cfg.agents.default);
    await this.reread(cfg, asked);
    await this.rereadMekoFor(cfg, asked);
    if (!this.defaultFor(cfg)) await Promise.all(this.list().filter((h) => h !== asked).map((h) => this.reread(cfg, h)));
    return this.defaultFor(cfg);
  }

  /**
   * The harness a new session starts on: the one asked for, else the default (`ensureDefault`). 400
   * when the one asked for isn't available (turned off, not installed, too old, or not connected to
   * Meko), naming what to do; 409 when no harness is available at all. Its version is read again
   * first (`reread`).
   */
  async pick(cfg: Config, asked: unknown): Promise<Harness> {
    if (asked != null && !HARNESS_IDS.includes(asked as HarnessId)) throw new HttpError(400, `Pick a harness: ${HARNESS_IDS.join(', ')}.`);
    if (asked == null) {
      const h = await this.ensureDefault(cfg);
      if (!h) throw new HttpError(409, this.noAgent(cfg));
      return h;
    }
    const h = this.of(asked);
    await this.reread(cfg, h);
    await this.rereadMekoFor(cfg, h);
    const s = this.statusOf(h, cfg);
    if (s.available) return h;
    if (onlyMekoBlocks(s)) throw new HttpError(400, `${notConnectedText(h.id)}.`);
    const how = !s.enabled
      ? `turn it on in Settings → Harness ([${h.id}] enabled in config.toml)`
      : s.version === null
        ? `install it, or set [${h.id}] command in config.toml`
        : `update it to ${s.minVersion} or later (it's ${s.version})${h.caps.upgrade ? `: run ${h.caps.upgrade}` : ''}`;
    throw new HttpError(400, `${h.caps.label} isn't available for new sessions: ${how}.`);
  }

  /**
   * Reads a harness's version again: now while it's turned on but not available (not read yet, its
   * last read failed, or too old: it may have been installed or updated since), else once stale.
   */
  private async reread(cfg: Config, h: Harness): Promise<void> {
    const s = h.status(cfg);
    await h.version(cfg, s.enabled && !s.available);
  }

  /**
   * Reads Meko's list again when the one kept would refuse a harness only for its connection: it may
   * have been connected since (`meko agent connect` in a terminal), without Settings opening.
   */
  private async rereadMekoFor(cfg: Config, h: Harness): Promise<void> {
    if (onlyMekoBlocks(this.statusOf(h, cfg))) await this.rereadMeko();
  }
}
