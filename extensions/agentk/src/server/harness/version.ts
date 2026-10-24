import { execFile } from 'node:child_process';
import { HARNESS_CAPS, type HarnessId } from '../../shared/harnesses.js';
import type { HarnessStatus } from '../../shared/types.js';
import type { Config } from '../config.js';
import { openCodeCommand } from '../opencode.js';
import { HttpError } from '../projects.js';

/** The version `<command> --version` prints (`2.1.287 (Claude Code)` → `2.1.287`), or null. */
export function cliVersion(command: string): Promise<string | null> {
  return new Promise((resolve) => {
    execFile(command, ['--version'], { timeout: 10_000, env: { ...process.env, NO_COLOR: '1' } }, (err, stdout) => {
      resolve(err ? null : (/(\d+\.\d+\.\d+)/.exec(String(stdout))?.[1] ?? null));
    });
  });
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

/** The command a harness runs as: `[<id>] command`, and for OpenCode's default also `~/.opencode/bin/opencode`. */
export function harnessCommand(cfg: Config, id: HarnessId): string {
  return id === 'opencode' ? openCodeCommand(cfg.opencode.command) : cfg[id].command;
}

/** How long a version read is used before it's read again (the CLIs update themselves). */
const VERSION_TTL_MS = 10 * 60 * 1000;

/** A CLI's version as AgentK last read it, and its updates: what Kiro's and OpenCode's adapters share. */
export interface CliState {
  /** Reads the version again when the last read is stale, is for another command, or `force`; null while it's turned off. */
  version(cfg: Config, force?: boolean): Promise<string | null>;
  status(cfg: Config): HarnessStatus;
  /** Runs its updater (`run`), one at a time (409 while one runs), then reads the version again. */
  update(cfg: Config, run: (command: string) => Promise<void>): Promise<HarnessStatus>;
}

/**
 * The version read of a harness whose config section is `[<id>] command, enabled`: read at most
 * every 10 minutes for the command it's run with (`command`, `[<id>] command` by default), and
 * forgotten while it's turned off, so turning it back on reads it again before it's offered.
 */
export function cliState(o: {
  id: Exclude<HarnessId, 'claude'>;
  changed: () => void;
  readVersion?: (command: string) => Promise<string | null>;
  command?: (cfg: Config) => string;
}): CliState {
  const readVersion = o.readVersion ?? cliVersion;
  const label = HARNESS_CAPS[o.id].label;
  const min = HARNESS_CAPS[o.id].minVersion;
  const command = o.command ?? ((cfg: Config) => cfg[o.id].command);
  let ver: { command: string; at: number; read: Promise<string | null> } | null = null;
  let value: string | null = null;
  /** The command the last finished read was for (`checked`). */
  let readFor: string | null = null;
  let updating = false;

  const version = (cfg: Config, force = false): Promise<string | null> => {
    const cmd = command(cfg);
    if (!cfg[o.id].enabled) {
      ver = null;
      readFor = null;
      return Promise.resolve(null);
    }
    const now = Date.now();
    if (force || !ver || ver.command !== cmd || now - ver.at > VERSION_TTL_MS) {
      const read = readVersion(cmd).then((v) => {
        if (ver?.read === read && (v !== value || readFor !== cmd)) {
          value = v;
          readFor = cmd;
          o.changed();
        }
        return v;
      });
      ver = { command: cmd, at: now, read };
    }
    return ver.read;
  };
  const status = (cfg: Config): HarnessStatus => {
    const { enabled } = cfg[o.id];
    const read = readFor === command(cfg);
    const v = enabled && read ? value : null;
    return {
      id: o.id, enabled, version: v, minVersion: min, available: !!v && (!min || versionAtLeast(v, min)),
      checked: !enabled || read, isDefault: cfg.agents.default === o.id, updating,
    };
  };
  return {
    version,
    status,
    async update(cfg, run) {
      if (updating) throw new HttpError(409, `${label} is already being updated.`);
      updating = true;
      o.changed();
      try {
        await run(command(cfg));
        await version(cfg, true);
      } finally {
        updating = false;
        o.changed();
      }
      return status(cfg);
    },
  };
}
