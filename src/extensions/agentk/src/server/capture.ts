import { execFile } from 'node:child_process';
import { existsSync, readFileSync } from 'node:fs';
import { homedir } from 'node:os';
import { join } from 'node:path';
import { claudeUserDir } from './claude.js';
import { errMsg, type Logger } from './log.js';

const MEKO_PLUGIN_PREFIX = 'meko-agent-skills@';

/** Where the Meko plugin keeps its watermarks and datapack pins. */
export function mekoCaptureDir(): string {
  return process.env.MEKO_WATERMARK_DIR || join(homedir(), '.claude', 'meko-capture');
}

/** The meko-agent-skills entries of Claude Code's plugin registry, by plugin key; null without a registry. */
function mekoRegistryEntries(claudeDir: string): [string, unknown][] | null {
  try {
    const j = JSON.parse(readFileSync(join(claudeDir, 'plugins', 'installed_plugins.json'), 'utf8'));
    const plugins = (j && typeof j.plugins === 'object' ? j.plugins : j) as Record<string, unknown> | null;
    return Object.entries(plugins ?? {}).filter(([k]) => k.startsWith(MEKO_PLUGIN_PREFIX));
  } catch {
    return null; // no plugin registry
  }
}

/**
 * The Meko plugin's keys (`<plugin>@<marketplace>`) for `enabledPlugins`: those in Claude Code's
 * plugin registry, and the key the Meko installer uses, in case it's installed after Kode starts.
 */
export function mekoPluginKeys(claudeDir: string = claudeUserDir()): string[] {
  const keys = new Set((mekoRegistryEntries(claudeDir) ?? []).map(([k]) => k));
  keys.add(`${MEKO_PLUGIN_PREFIX}meko-agent-skills`);
  return [...keys];
}

/** Install paths of the meko-agent-skills plugin from Claude Code's plugin registry. */
export function mekoPluginInstalls(claudeDir: string = claudeUserDir()): string[] | null {
  const entries = mekoRegistryEntries(claudeDir);
  if (!entries) return null;
  const out: string[] = [];
  for (const [, v] of entries) {
    for (const e of Array.isArray(v) ? v : [v]) {
      const p = (e as { installPath?: unknown } | null)?.installPath;
      if (typeof p === 'string' && p) out.push(p);
    }
  }
  return out;
}

/** The Meko plugin's transcript capture script, or null when the plugin isn't installed. */
export function findMekoCaptureScript(claudeDir?: string): string | null {
  for (const dir of mekoPluginInstalls(claudeDir) ?? []) {
    const script = join(dir, 'hooks-handlers', 'lib', 'capture.js');
    if (existsSync(script)) return script;
  }
  return null;
}

/**
 * Sends new transcript turns to the session's Meko conversation by running
 * the Meko plugin's own capture script in checkpoint mode. The plugin only
 * captures on compaction, on exit, and from a timer it starts when the
 * transcript already exists at SessionStart, which it never does for a new
 * `--session-id` session. The script keeps a watermark per session and Meko
 * dedups turns, so running it after every turn is cheap and safe.
 */
export class TranscriptCapture {
  private running = new Map<string, Promise<void>>();
  private again = new Set<string>();

  constructor(
    private script: string | null,
    private env: () => Record<string, string>,
    private log: Logger,
    private timeoutMs = 60000,
    /** Told how each run went (for the usage report): whether it worked, and how long it took. */
    private onRun: (ok: boolean, ms: number) => void = () => undefined,
  ) {}

  get enabled(): boolean {
    return !!this.script;
  }

  /** Captures the session; one run at a time, and a request during a run queues one more. Never throws. */
  capture(sessionId: string, transcriptPath: string): Promise<void> {
    if (!this.script || !existsSync(transcriptPath)) return Promise.resolve();
    const inFlight = this.running.get(sessionId);
    if (inFlight) {
      this.again.add(sessionId);
      return inFlight;
    }
    const p = (async () => {
      do {
        this.again.delete(sessionId);
        await this.runOnce(sessionId, transcriptPath);
      } while (this.again.has(sessionId));
    })().finally(() => this.running.delete(sessionId));
    this.running.set(sessionId, p);
    return p;
  }

  /** Resolves once no capture is running (including ones started meanwhile), or after <ms>. */
  async settled(ms: number): Promise<void> {
    const idle = (async () => {
      while (this.running.size) await Promise.all(this.running.values());
    })();
    let timer: NodeJS.Timeout | undefined;
    await Promise.race([idle, new Promise<void>((r) => (timer = setTimeout(r, ms)))]);
    clearTimeout(timer);
  }

  private runOnce(sessionId: string, transcriptPath: string): Promise<void> {
    const started = Date.now();
    return new Promise((resolve) => {
      const child = execFile(
        process.execPath,
        [this.script!, 'checkpoint'],
        { timeout: this.timeoutMs, env: { ...process.env, ...this.env() } },
        (err, _stdout, stderr) => {
          const failed = !!err || /failed/i.test(stderr);
          if (err) this.log.warn(`Meko transcript capture failed for ${sessionId}`, errMsg(err));
          else if (failed) this.log.warn(`Meko transcript capture for ${sessionId} reported errors`, stderr.trim().split('\n').slice(-3).join(' '));
          try {
            this.onRun(!failed, Date.now() - started);
          } catch {
            /* the record is best-effort */
          }
          resolve();
        },
      );
      child.stdin?.on('error', () => undefined);
      child.stdin?.end(JSON.stringify({ transcript_path: transcriptPath }));
    });
  }
}
