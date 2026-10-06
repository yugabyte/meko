import type { ClaudeCodeStatus } from '../shared/types.js';
import { MIN_MOD_CLAUDE, claudeVersion, runClaudeUpdate, versionAtLeast } from './claude.js';
import type { Config } from './config.js';
import { HttpError } from './projects.js';

/** How long a version read is used before it's read again (Claude Code updates itself). */
export const CLAUDE_VERSION_TTL_MS = 10 * 60 * 1000;

export interface ClaudeCodeDeps {
  config: () => Config;
  /** Something visible changed: the state is pushed to the browser. */
  changed: () => void;
  readVersion?: (command: string) => Promise<string | null>;
  runUpdate?: (command: string) => Promise<{ ok: boolean; output: string }>;
  now?: () => number;
}

/** The last few lines of what `claude update` printed, for the banner: short enough to read at a glance. */
function summary(output: string): string {
  const lines = output.split('\n').map((l) => l.trim()).filter(Boolean);
  const text = lines.slice(-3).join(' ');
  return text.length > 300 ? `${text.slice(0, 299)}…` : text;
}

/**
 * The Claude Code sessions launch with (`[claude] command`): its version, read with
 * `--version` at most every CLAUDE_VERSION_TTL_MS, whether it's new enough for AgentK's mod
 * (`MIN_MOD_CLAUDE`), and updating it with its own `claude update`. A running session keeps the
 * version it started with until Claude restarts in it; this is what the next launch gets.
 */
export class ClaudeCode {
  private ver: { command: string; at: number; read: Promise<string | null> } | null = null;
  private value: string | null = null;
  private updating = false;
  private last: ClaudeCodeStatus['lastUpdate'] = null;
  private readVersion: (command: string) => Promise<string | null>;
  private runUpdate: (command: string) => Promise<{ ok: boolean; output: string }>;
  private now: () => number;

  constructor(private deps: ClaudeCodeDeps) {
    this.readVersion = deps.readVersion ?? claudeVersion;
    this.runUpdate = deps.runUpdate ?? runClaudeUpdate;
    this.now = deps.now ?? Date.now;
  }

  /** The version, read again when the last read is older than the TTL, `force`d, or for another command. */
  version(force = false): Promise<string | null> {
    const { command } = this.deps.config().claude;
    const now = this.now();
    if (force || !this.ver || this.ver.command !== command || now - this.ver.at > CLAUDE_VERSION_TTL_MS) {
      const read = this.readVersion(command).then((v) => {
        if (this.ver?.read === read && v !== this.value) {
          this.value = v;
          this.deps.changed();
        }
        return v;
      });
      this.ver = { command, at: now, read };
    }
    return this.ver.read;
  }

  /** Whether the Claude Code the next launch gets runs AgentK's mod (false when its version can't be read). */
  async supportsMod(): Promise<boolean> {
    const v = await this.version();
    return !!v && versionAtLeast(v, MIN_MOD_CLAUDE);
  }

  status(): ClaudeCodeStatus {
    return {
      version: this.value,
      minModVersion: MIN_MOD_CLAUDE,
      supportsMod: this.value === null ? null : versionAtLeast(this.value, MIN_MOD_CLAUDE),
      modEnabled: this.deps.config().claude.mod,
      updating: this.updating,
      lastUpdate: this.last,
    };
  }

  /**
   * Runs `claude update`, then reads the version again. Resolves the new status; the outcome is in
   * `lastUpdate` (the update failing is an outcome, not an error). 409 while one is running.
   */
  async update(): Promise<ClaudeCodeStatus> {
    if (this.updating) throw new HttpError(409, 'Claude Code is already being updated.');
    this.updating = true;
    this.deps.changed();
    const from = this.value;
    try {
      const r = await this.runUpdate(this.deps.config().claude.command);
      const to = await this.version(true);
      this.last = { ok: r.ok && to !== null, from, to, message: summary(r.output), at: this.now() };
    } finally {
      this.updating = false;
      this.deps.changed();
    }
    return this.status();
  }
}
