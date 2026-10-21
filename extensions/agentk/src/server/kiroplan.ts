import { mkdirSync, realpathSync } from 'node:fs';
import { join } from 'node:path';
import type { KiroPlan } from '../shared/types.js';
import { runClaudeOnce, type AskRunner } from './askagentk.js';
import { get, kvGet, kvSet, type Db } from './db.js';
import { askAgent, deleteKiroSession, kiroSessionsIn, kiroUsageArgs, parseKiroPlan, readKiroRun, USAGE_AGENT, writeKiroAgent } from './kiro.js';
import { errMsg, type Logger } from './log.js';

const KV_KEY = 'kiro_plan';
/** How often the plan is read while Kiro is in use. */
export const KIRO_PLAN_EVERY_MS = 60 * 60 * 1000;
/** A `/usage` run takes about 4 s. */
const RUN_TIMEOUT_MS = 20_000;

export interface KiroPlanDeps {
  db: Db;
  /** Whether sessions can run on Kiro (turned on, installed, recent enough). */
  available: () => boolean;
  command: () => string;
  /** An empty folder the runs happen in, so the session Kiro keeps of each is never a worktree's. */
  cwd: string;
  changed: () => void;
  log: Logger;
  run?: AskRunner;
  /** Kiro's home, whose session store is swept (tests name their own). */
  kiroHome?: string;
  /** Deletes one of Kiro's sessions (tests record it); by default `kiro-cli chat --delete-session`. */
  deleteSession?: (cmd: string, id: string, cwd: string) => void;
}

/**
 * Kiro's plan allowance for the rail, read with its `/usage` command (`kiro-cli chat
 * --no-interactive /usage`, which bills nothing) at start, hourly, and when the first Kiro session
 * starts. Only once a session has run on Kiro in AgentK: an install that never uses Kiro never runs
 * it for this. A read that fails keeps the last one, which the rail shows with its age. Each run is
 * the inert `agentk-usage` agent's, in a folder of its own, and every session Kiro keeps of a run
 * there is deleted afterwards, whether or not the run said its id.
 */
export class KiroPlanStore {
  private last: KiroPlan | null = null;
  private reading: Promise<void> | null = null;
  private warned = false;
  private run: AskRunner;

  constructor(private deps: KiroPlanDeps) {
    this.run = deps.run ?? runClaudeOnce;
    try {
      this.last = JSON.parse(kvGet(deps.db, KV_KEY) ?? 'null') as KiroPlan | null;
    } catch {
      this.last = null;
    }
  }

  /** Some session, archived ones included, has run on Kiro. */
  used(): boolean {
    return !!get(this.deps.db, `SELECT 1 FROM sessions WHERE harness = 'kiro' LIMIT 1`);
  }

  /** Reads the plan once more; a read already running is the one waited on. Never throws. */
  refresh(): Promise<void> {
    this.reading ??= this.read().finally(() => {
      this.reading = null;
    });
    return this.reading;
  }

  /** Resolves once the read running now, if any, is done (the deletes it started may still run). Never throws. */
  settled(): Promise<void> {
    return this.reading ?? Promise.resolve();
  }

  /** A Kiro session was just created: the first one in AgentK reads the plan, rather than waiting for the hour. */
  sessionCreated(): void {
    const n = get<{ n: number }>(this.deps.db, `SELECT COUNT(*) AS n FROM sessions WHERE harness = 'kiro'`)?.n ?? 0;
    if (n === 1) void this.refresh();
  }

  /** The last reading, unless Kiro isn't available or the reading's period has reset. */
  current(now = Date.now()): KiroPlan | null {
    if (!this.last || !this.deps.available()) return null;
    if (this.last.resetsOn && Date.parse(this.last.resetsOn) <= now) return null;
    return this.last;
  }

  private async read(): Promise<void> {
    try {
      if (!this.deps.available() || !this.used()) return;
      const cmd = this.deps.command();
      const cwd = this.deps.cwd;
      mkdirSync(cwd, { recursive: true });
      // Kiro finds the run's agent in the folder it runs in, so it never shows in the user's agent list.
      writeKiroAgent(askAgent('', USAGE_AGENT), join(cwd, '.kiro'));
      const out = await this.run(cmd, kiroUsageArgs(), { input: '', cwd, timeoutMs: RUN_TIMEOUT_MS });
      const r = readKiroRun(out.stdout);
      this.sweep(cmd, r.sessionId);
      const plan = r.ok ? parseKiroPlan(r.text, Date.now()) : null;
      if (!plan) {
        const why = out.spawnError ?? (out.timedOut ? 'it timed out' : r.error ?? (r.ok ? `its answer wasn't one AgentK knows: ${r.text.slice(0, 200)}` : `exit ${out.code}`));
        this.warn(`reading Kiro's plan usage failed (${why}); the rail keeps the last reading`);
        return;
      }
      this.warned = false;
      this.last = plan;
      kvSet(this.deps.db, KV_KEY, JSON.stringify(plan));
      this.deps.changed();
    } catch (e) {
      this.warn(`reading Kiro's plan usage failed: ${errMsg(e)}`);
    }
  }

  /**
   * Kiro keeps every run as a session, with no turns and nobody to resume it: deletes the one the run
   * named, and any other in the run's folder (a run cut off at its timeout never names its own).
   */
  private sweep(cmd: string, named: string | null): void {
    const del = this.deps.deleteSession ?? deleteKiroSession;
    let cwds = [this.deps.cwd];
    try {
      cwds = [...new Set([this.deps.cwd, realpathSync(this.deps.cwd)])];
    } catch {
      /* the folder is there: it was just made */
    }
    const ids = new Set([...(named ? [named] : []), ...kiroSessionsIn(cwds, this.deps.kiroHome)]);
    for (const id of ids) del(cmd, id, this.deps.cwd);
  }

  /** Once until a read works again, so an hourly failure (signed out, say) doesn't fill the log. */
  private warn(msg: string): void {
    if (this.warned) return;
    this.warned = true;
    this.deps.log.warn(msg);
  }
}
