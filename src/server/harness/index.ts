import { HARNESS_IDS, NO_AGENT, harnessOf, type HarnessId } from '../../shared/harnesses.js';
import type { HarnessStatus } from '../../shared/types.js';
import type { ClaudeCode } from '../claudecode.js';
import type { Config } from '../config.js';
import type { Logger } from '../log.js';
import { HttpError } from '../projects.js';
import { claudeHarness } from './claude.js';
import { kiroHarness } from './kiro.js';
import type { Harness } from './types.js';

export type { Harness, LaunchInput } from './types.js';

/** Every harness sessions can run on, by id (src/shared/harnesses.ts). */
export class Harnesses {
  private all: Record<HarnessId, Harness>;

  constructor(deps: { claudeCode: ClaudeCode; log: Logger; changed: () => void; kiroVersion?: (cmd: string) => Promise<string | null> }) {
    this.all = {
      claude: claudeHarness(deps),
      kiro: kiroHarness({ log: deps.log, changed: deps.changed, readVersion: deps.kiroVersion }),
    };
  }

  /** A session's harness (one stored by a newer version reads as Claude Code). */
  of(id: unknown): Harness {
    return this.all[harnessOf(id)];
  }

  list(): Harness[] {
    return HARNESS_IDS.map((id) => this.all[id]);
  }

  status(cfg: Config): HarnessStatus[] {
    return this.list().map((h) => h.status(cfg));
  }

  /**
   * What runs when nobody picks: `[agents] default` while it's available, else the first available
   * harness (HARNESS_IDS order), else null (no coding agent is installed, turned on and new enough).
   */
  defaultFor(cfg: Config): Harness | null {
    const h = this.of(cfg.agents.default);
    return h.status(cfg).available ? h : (this.list().find((x) => x.status(cfg).available) ?? null);
  }

  /**
   * `defaultFor`, after reading again the version of any harness that's turned on but has none (not
   * read yet, or its last read failed), so a read that failed once doesn't refuse until the next one.
   */
  async ensureDefault(cfg: Config): Promise<Harness | null> {
    const asked = this.of(cfg.agents.default);
    await this.reread(cfg, asked);
    if (!this.defaultFor(cfg)) await Promise.all(this.list().filter((h) => h !== asked).map((h) => this.reread(cfg, h)));
    return this.defaultFor(cfg);
  }

  /**
   * The harness a new session starts on: the one asked for, else the default (`ensureDefault`). 400
   * when the one asked for isn't available (turned off, not installed, or too old), naming what to
   * do; 409 when no harness is available at all.
   */
  async pick(cfg: Config, asked: unknown): Promise<Harness> {
    if (asked != null && !HARNESS_IDS.includes(asked as HarnessId)) throw new HttpError(400, `Pick a harness: ${HARNESS_IDS.join(' or ')}.`);
    if (asked == null) {
      const h = await this.ensureDefault(cfg);
      if (!h) throw new HttpError(409, NO_AGENT);
      return h;
    }
    const h = this.of(asked);
    await this.reread(cfg, h);
    const s = h.status(cfg);
    if (s.available) return h;
    const how = !s.enabled
      ? `turn it on in Settings → General ([${h.id}] enabled in config.toml)`
      : s.version === null
        ? `install it, or set [${h.id}] command in config.toml`
        : `update it to ${s.minVersion} or later (it's ${s.version})`;
    throw new HttpError(400, `${h.caps.label} isn't available for new sessions: ${how}.`);
  }

  /** Reads a harness's version again when it's turned on but has none. */
  private async reread(cfg: Config, h: Harness): Promise<void> {
    if (h.enabled(cfg) && h.status(cfg).version === null) await h.version(cfg, true);
  }
}
