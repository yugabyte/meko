import { mkdirSync } from 'node:fs';
import type { KiroPlan } from '../shared/types.js';
import { runClaudeOnce, type AskRunner } from './askagentk.js';
import { get, kvGet, kvSet, type Db } from './db.js';
import { deleteKiroSession, kiroUsageArgs, parseKiroPlan, readKiroRun } from './kiro.js';
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
}

/**
 * Kiro's plan allowance for the rail, read with its `/usage` command (`kiro-cli chat
 * --no-interactive /usage`, which bills nothing) at start, hourly, and when the first Kiro session
 * starts. Only once a session has run on Kiro in AgentK: an install that never uses Kiro never runs
 * it for this. A read that fails keeps the last one, which the rail shows with its age.
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
    if (!this.deps.available() || !this.used()) return;
    const cmd = this.deps.command();
    try {
      mkdirSync(this.deps.cwd, { recursive: true });
      const out = await this.run(cmd, kiroUsageArgs(), { input: '', cwd: this.deps.cwd, timeoutMs: RUN_TIMEOUT_MS });
      const r = readKiroRun(out.stdout);
      // Kiro keeps every run as a session; this one has no turns and nobody resumes it.
      if (r.sessionId) deleteKiroSession(cmd, r.sessionId, this.deps.cwd);
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

  /** Once until a read works again, so an hourly failure (signed out, say) doesn't fill the log. */
  private warn(msg: string): void {
    if (this.warned) return;
    this.warned = true;
    this.deps.log.warn(msg);
  }
}
