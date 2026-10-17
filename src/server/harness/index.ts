import { HARNESS_IDS, harnessOf, type HarnessId } from '../../shared/harnesses.js';
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
   * The harness a new session starts on: the one asked for, else `[agents] default`. 400 when it
   * isn't available (turned off, not installed, or too old), naming what to do.
   */
  pick(cfg: Config, asked: unknown): Harness {
    if (asked != null && !HARNESS_IDS.includes(asked as HarnessId)) throw new HttpError(400, `Pick a harness: ${HARNESS_IDS.join(' or ')}.`);
    const h = this.of(asked ?? cfg.agents.default);
    const s = h.status(cfg);
    if (s.available) return h;
    const how = !s.enabled
      ? `turn it on in Settings → General ([${h.id}] enabled in config.toml)`
      : s.version === null
        ? `install it, or set [${h.id}] command in config.toml`
        : `update it to ${s.minVersion} or later (it's ${s.version})`;
    throw new HttpError(400, `${h.caps.label} isn't available for new sessions: ${how}.`);
  }
}
