import { randomUUID } from 'node:crypto';
import { homedir } from 'node:os';
import type { Core } from './core.js';
import { all, get, run } from './db.js';
import type { ModReply } from './hooks.js';
import { errMsg } from './log.js';
import { launchedAgentId, mekoTag, type SimilarMemory } from './meko.js';
import { LEARN_AGENT } from './mekousage.js';
import { HttpError } from './projects.js';
import { SAVING_RULE } from './prompts.js';
import type { DatapackRef } from '../shared/types.js';
import { sessionDatapack, sessionRepo, sessionRow, type SessionRow } from './store.js';

export type LearnTrigger = 'merge' | 'manual';
export type LearnPath = 'mod' | 'turn' | 'none';
export type LearnOutcome = 'done' | 'failed' | 'interrupted' | 'skipped';

/** How long a learn step through the mod may take, all its parts and writes together, before it's given up. */
export const LEARN_TIMEOUT_MS = 180_000;
/** The small model that checks the facts against what Meko already has. */
export const DEDUP_MODEL = 'claude-haiku-4-5-20251001';
/** How many saved memories each fact is checked against. */
const SIMILAR = 3;
/** A fact longer than this is cut (a reply that put a whole document in one fact). */
const FACT_CHARS = 2000;

const KINDS: Record<string, string> = {
  'how-it-works': 'How it works',
  decision: 'Decision',
  'root-cause': 'Root cause',
  gotcha: 'Gotcha',
  measured: 'Measured',
};

/**
 * What the fork is asked: the facts this session learned, as JSON. It states the saving rule in
 * full: a session launched before the rule was in the system prompt doesn't have it.
 */
export const EXTRACT_PROMPT = `Don't call any tools. List what this session learned that is worth keeping for the team working on this product. ${SAVING_RULE} Each fact must make sense on its own to a teammate who never saw this session.

Reply with JSON only, no other text:
{"facts":[{"kind":"how-it-works|decision|root-cause|gotcha|measured","component":"…","text":"one self-contained fact"}]}
Reply {"facts":[]} when there is nothing worth saving.`;

export interface Fact {
  kind: string;
  component: string;
  text: string;
}

export interface Decision {
  action: 'add' | 'skip' | 'update';
  /** For an update: the memory it corrects. */
  id: string | null;
  /** For an update: the corrected text (the fact's own when left out). */
  text: string | null;
}

/** The first JSON object in a reply (a model may wrap it in a code fence or a sentence), or null. */
export function firstJson(text: string): Record<string, unknown> | null {
  const start = text.indexOf('{');
  if (start < 0) return null;
  for (let end = text.lastIndexOf('}'); end > start; end = text.lastIndexOf('}', end - 1)) {
    try {
      const v: unknown = JSON.parse(text.slice(start, end + 1));
      if (v && typeof v === 'object' && !Array.isArray(v)) return v as Record<string, unknown>;
    } catch {
      /* try a shorter span */
    }
  }
  return null;
}

/** The facts in the fork's reply, or null when it isn't the JSON asked for. Facts without text are dropped. */
export function parseFacts(text: string): Fact[] | null {
  const facts = firstJson(text)?.facts;
  if (!Array.isArray(facts)) return null;
  const out: Fact[] = [];
  for (const f of facts as Record<string, unknown>[]) {
    const t = typeof f?.text === 'string' ? f.text.trim() : '';
    if (!t) continue;
    out.push({
      kind: typeof f.kind === 'string' && Object.hasOwn(KINDS, f.kind) ? f.kind : 'how-it-works',
      component: typeof f.component === 'string' ? f.component.trim().slice(0, 200) : '',
      text: t.slice(0, FACT_CHARS),
    });
  }
  return out;
}

/**
 * Text that names this machine is left out, whatever the model thought: the home folder, AgentK's
 * own folders, the worktree, the repo. The datapack is shared by a team, so it never helps anyone else.
 */
export function machineSpecific(text: string, paths: string[]): boolean {
  return paths.some((p) => p.length > 1 && text.includes(p));
}

/** The text saved for a fact: its kind and component first, as the saving rule asks for. */
export function memoryText(f: Fact): string {
  const label = Object.hasOwn(KINDS, f.kind) ? KINDS[f.kind] : 'How it works';
  if (f.text.toLowerCase().startsWith(label.toLowerCase())) return f.text;
  return `${label}${f.component ? ` (${f.component})` : ''}: ${f.text}`;
}

/** What the small model is asked: for each fact, add it, skip it, or correct a memory with it. */
export function dedupPrompt(facts: Fact[], similar: SimilarMemory[][]): string {
  const list = facts.map((f, i) => {
    const near = similar[i] ?? [];
    const mems = near.length ? near.map((m) => `   - [${m.id}] ${m.text.replace(/\s+/g, ' ').slice(0, 600)}`).join('\n') : '   (none)';
    return `${i + 1}. ${memoryText(f)}\n   Saved memories most like it:\n${mems}`;
  }).join('\n\n');
  return `A coding session learned the facts below. Each is followed by the memories already saved that are most like it. For each fact, decide:
- "skip" if one of those memories already says it (the same point, even in other words)
- "update" if one of those memories says something this fact shows is wrong or out of date: give that memory's id and its corrected text
- "add" otherwise

Reply with JSON only, no other text:
{"decisions":[{"n":1,"action":"add|skip|update","id":"memory id, for update","text":"corrected text, for update"}]}

${list}`;
}

/** The decisions in the small model's reply, by fact number; null when it isn't the JSON asked for. */
export function parseDecisions(text: string): Map<number, Decision> | null {
  const ds = firstJson(text)?.decisions;
  if (!Array.isArray(ds)) return null;
  const out = new Map<number, Decision>();
  for (const d of ds as Record<string, unknown>[]) {
    const n = typeof d?.n === 'number' ? d.n : Number(d?.n);
    if (!Number.isInteger(n) || n < 1) continue;
    const action = d.action === 'skip' || d.action === 'update' ? d.action : 'add';
    out.set(n, {
      action,
      id: typeof d.id === 'string' && d.id ? d.id : null,
      text: typeof d.text === 'string' && d.text.trim() ? d.text.trim().slice(0, FACT_CHARS) : null,
    });
  }
  return out;
}

/** "Saved 3 memories and corrected 1 in team/demo", the timeline's line for a learn step. */
export function learnSummary(saves: number, updates: number, datapack: string): string {
  return `Saved ${saves} ${saves === 1 ? 'memory' : 'memories'} and corrected ${updates} in ${datapack}`;
}

/** Token counts as a model call reports them (the API's names). */
interface ModelUsage {
  input_tokens?: unknown;
  output_tokens?: unknown;
  cache_read_input_tokens?: unknown;
  cache_creation_input_tokens?: unknown;
}

const count = (v: unknown) => (typeof v === 'number' && Number.isFinite(v) && v > 0 ? Math.round(v) : 0);

interface LearnRow {
  id: string;
  session_id: string;
  trigger: LearnTrigger;
  path: LearnPath;
  started_at: number;
}

/** A learn step through the mod, while it runs. */
interface Job {
  id: string;
  sessionId: string;
  trigger: LearnTrigger;
  datapack: DatapackRef;
  /** extract: waiting for the fork; search: AgentK looks up what Meko has; check: waiting for the duplicate check; save: AgentK writes. */
  step: 'extract' | 'search' | 'check' | 'save';
  facts: Fact[];
  similar: SimilarMemory[][];
  saves: number;
  updates: number;
  skips: number;
  /** Fails the step when its time is up, wherever it is (writes included). */
  timer: NodeJS.Timeout;
}

/**
 * The learn step: what a session learned about the product and repo goes to the project's
 * dedicated datapack. Through AgentK's mod when it's live: the mod forks the session's own context
 * for the facts, AgentK finds the saved memories most like each, the mod asks a small model which
 * are new, known or corrections, and AgentK writes them. Every part of it reaches the mod on a poll
 * and its reply comes back on /mod/learn, which answers at once, so no request is held while Meko
 * works. Without the mod, a typed `learn` prompt asks Claude to save them itself. Runs at Merge &
 * Close (before cleanup), and when the user asks (the session menu, or /agentk_learn in the terminal).
 */
export class Learner {
  private jobs = new Map<string, Job>();
  /** How long a step through the mod may take (tests shorten it). */
  timeoutMs = LEARN_TIMEOUT_MS;

  constructor(private core: Core) {}

  private get db() {
    return this.core.db;
  }

  /** The session's learn step still running, if any. */
  private openRow(sessionId: string): LearnRow | null {
    return get<LearnRow>(this.db, 'SELECT id, session_id, trigger, path, started_at FROM meko_learns WHERE session_id = ? AND ended_at IS NULL ORDER BY started_at DESC LIMIT 1', sessionId) ?? null;
  }

  /** True while the session has a learn step running. */
  running(sessionId: string): boolean {
    return !!this.openRow(sessionId);
  }

  /** The dedicated datapack the session saves to, when Meko is on; null otherwise. */
  private datapack(row: SessionRow): DatapackRef | null {
    if (!this.core.mekoStatus.configured) return null;
    const dp = sessionDatapack(this.db, row);
    return dp?.role === 'dedicated' ? dp : null;
  }

  /** Why a learn step can't run in the session's Claude now, or null when it can. */
  private notNow(row: SessionRow): string | null {
    if (row.desired !== 'running') return 'This session is archived.';
    if (row.status === 'exited') return 'Claude has exited in this session. Resume it, then try again.';
    if (row.status === 'starting') return 'Claude is still starting. Try again in a moment.';
    if ((row.status !== 'idle' && row.status !== 'recovered') || this.core.sessions.runtime(row.id).busy) {
      return 'Claude is busy. Try again when its turn has ended.';
    }
    return null;
  }

  /**
   * Starts a learn step for the session: through the mod when it's live, else as a typed prompt.
   * Throws a 409 saying why it can't run now.
   */
  async start(row: SessionRow, trigger: LearnTrigger): Promise<LearnPath> {
    const dp = this.datapack(row);
    if (!dp) throw new HttpError(409, "This session's project has no Meko datapack to save to.");
    if (this.openRow(row.id)) throw new HttpError(409, 'AgentK is already saving what this session learned.');
    const why = this.notNow(row);
    if (why) throw new HttpError(409, why);
    if (this.core.config.claude.mod && this.core.sessions.mod.learnReady(row.id)) {
      this.startMod(row, trigger, dp);
      return 'mod';
    }
    await this.startTurn(row, trigger);
    return 'turn';
  }

  /**
   * At Merge & Close, before cleanup: true while the cleanup should wait for a learn step (one ran
   * now, or one is still running, Save learnings' too); `merged` runs again once it ends
   * (Lifecycle.afterLearn). One that finished since the session got to Merge & Close covers it. A
   * session that can't run one (Claude isn't there or is busy) is recorded as skipped.
   */
  async beforeClose(row: SessionRow): Promise<boolean> {
    if (this.openRow(row.id)) return true;
    if (get(this.db, "SELECT 1 AS x FROM meko_learns WHERE session_id = ? AND trigger = 'merge'", row.id)) return false;
    if (!this.datapack(row)) return false;
    const merging = get<{ at: number | null }>(this.db, "SELECT MAX(entered_at) AS at FROM session_stages WHERE session_id = ? AND stage = 'merge'", row.id)?.at;
    if (merging && get(this.db, "SELECT 1 AS x FROM meko_learns WHERE session_id = ? AND outcome = 'done' AND ended_at >= ?", row.id, merging)) return false;
    if (this.notNow(row)) {
      this.insert(row.id, randomUUID(), 'merge', 'none', 'skipped');
      return false;
    }
    try {
      await this.start(row, 'merge');
      return true;
    } catch (e) {
      this.core.log.warn(`the learn step didn't start for ${row.id}`, errMsg(e));
      if (!this.openRow(row.id)) this.insert(row.id, randomUUID(), 'merge', 'none', 'skipped');
      return false;
    }
  }

  private insert(sessionId: string, id: string, trigger: LearnTrigger, path: LearnPath, outcome: LearnOutcome | null): void {
    const now = Date.now();
    run(this.db, 'INSERT INTO meko_learns(id, session_id, trigger, path, started_at, ended_at, outcome) VALUES (?, ?, ?, ?, ?, ?, ?)',
      id, sessionId, trigger, path, now, outcome ? now : null, outcome);
    this.core.changed(); // Session.learning
  }

  // ---- through the mod ----

  private startMod(row: SessionRow, trigger: LearnTrigger, dp: DatapackRef): void {
    const id = randomUUID();
    this.insert(row.id, id, trigger, 'mod', null);
    const timer = setTimeout(() => {
      const job = this.jobs.get(id);
      if (job) this.fail(job, job.step, TIMED_OUT[job.step]);
    }, this.timeoutMs);
    timer.unref?.();
    this.jobs.set(id, { id, sessionId: row.id, trigger, datapack: dp, step: 'extract', facts: [], similar: [], saves: 0, updates: 0, skips: 0, timer });
    this.core.sessions.mod.queueLearn(row.id, { id, kind: 'extract', prompt: EXTRACT_PROMPT });
    this.core.log.info(`saving what ${row.id} learned to ${dp.name} through AgentK's mod`);
  }

  /**
   * The mod's `/mod/learn` posts: the fork's reply (`step: "facts"`), then the duplicate check's
   * (`step: "decisions"`). Answered at once (204); what follows comes on the mod's polls. Each post's
   * usage is recorded as the session's tokens, even for a step AgentK gave up on meanwhile (410).
   */
  fromMod(row: SessionRow, body: Record<string, unknown>): ModReply {
    const id = typeof body.id === 'string' ? body.id : '';
    if (!get(this.db, 'SELECT 1 AS x FROM meko_learns WHERE id = ? AND session_id = ?', id, row.id)) return { status: 410 };
    const step = body.step === 'decisions' ? 'check' : 'extract';
    this.recordUsage(row.id, id, step === 'check' ? 'dedup' : 'extract', body.model, body.usage);
    const job = this.jobs.get(id);
    if (!job) return { status: 410 };
    if (job.step !== step) return { status: 204 }; // a repeat
    if (step === 'extract') this.facts(row, job, body);
    else this.decisions(row, job, body);
    return { status: 204 };
  }

  /** The local paths a fact must not name. */
  private localPaths(row: SessionRow): string[] {
    return [homedir(), this.core.paths.home, row.worktree_path, sessionRepo(this.db, row) ?? ''].filter(Boolean);
  }

  private facts(row: SessionRow, job: Job, body: Record<string, unknown>): void {
    if (typeof body.text !== 'string') return this.fail(job, 'extract', noReply(body.reason, 'look back over the session'));
    const parsed = parseFacts(body.text);
    if (!parsed) return this.fail(job, 'extract', "Claude didn't list them in a form AgentK could read");
    const local = this.localPaths(row);
    job.facts = parsed.filter((f) => !machineSpecific(`${f.component} ${f.text}`, local));
    run(this.db, 'UPDATE meko_learns SET candidates = ? WHERE id = ?', job.facts.length, job.id);
    if (!job.facts.length) return this.finish(job);
    job.step = 'search';
    void this.search(row, job);
  }

  /** Finds the saved memories most like each fact, then hands the mod the duplicate check. */
  private async search(row: SessionRow, job: Job): Promise<void> {
    try {
      const conv = await this.core.sessions.conversationId(row, job.datapack);
      const agent = launchedAgentId(row);
      job.similar = await mekoTag.run(job.id, () => Promise.all(job.facts.map((f) =>
        this.core.meko.similar(memoryText(f), job.datapack.id, agent, conv, SIMILAR).catch((): SimilarMemory[] => []))));
    } catch (e) {
      if (this.jobs.has(job.id)) this.fail(job, 'search', `Meko couldn't be searched (${errMsg(e)})`);
      return;
    }
    if (!this.jobs.has(job.id)) return; // timed out meanwhile
    job.step = 'check';
    this.core.sessions.mod.queueLearn(job.sessionId, { id: job.id, kind: 'check', prompt: dedupPrompt(job.facts, job.similar), model: DEDUP_MODEL });
  }

  private decisions(row: SessionRow, job: Job, body: Record<string, unknown>): void {
    if (typeof body.text !== 'string') return this.fail(job, 'dedup', noReply(body.reason, 'check them against what Meko has'));
    const decided = parseDecisions(body.text);
    if (!decided) return this.fail(job, 'dedup', "the check against what Meko has didn't answer in a form AgentK could read");
    job.step = 'save';
    void this.save(row, job, decided);
  }

  /** Writes what the duplicate check decided, one fact at a time, until the step's time is up. */
  private async save(row: SessionRow, job: Job, decided: Map<number, Decision>): Promise<void> {
    const local = this.localPaths(row);
    try {
      const conv = await this.core.sessions.conversationId(row, job.datapack);
      const agent = launchedAgentId(row);
      await mekoTag.run(job.id, async () => {
        for (const [i, f] of job.facts.entries()) {
          if (!this.jobs.has(job.id)) return; // the time was up: the timer closed the step
          const d = decided.get(i + 1) ?? { action: 'add', id: null, text: null };
          const target = d.action === 'update' ? (job.similar[i] ?? []).find((m) => m.id === d.id) : undefined;
          try {
            if (d.action === 'skip') {
              job.skips++;
            } else if (target) {
              const text = d.text && !machineSpecific(d.text, local) ? d.text : memoryText(f);
              await this.core.meko.updateMemory(target.id, text, job.datapack.id, target.agentId, conv);
              job.updates++;
            } else {
              await this.core.meko.addMemory(memoryText(f), job.datapack.id, agent, conv, {
                source: 'agent-k-local', kind: 'learned', fact: f.kind, component: f.component || null, session: row.id, learn: job.id,
              });
              job.saves++;
            }
          } catch (e) {
            this.core.log.warn(`saving a learned fact for ${row.id} failed`, errMsg(e));
          }
        }
      });
    } catch (e) {
      if (this.jobs.has(job.id)) this.fail(job, 'save', `Meko couldn't be reached (${errMsg(e)})`);
      return;
    }
    if (this.jobs.has(job.id)) this.finish(job);
  }

  /** Records a learn model call's tokens as one of the session's responses (so usage shows them too). */
  private recordUsage(sessionId: string, id: string, phase: 'extract' | 'dedup', model: unknown, usage: unknown): void {
    if (!usage || typeof usage !== 'object') return;
    const u = usage as ModelUsage;
    run(this.db, `INSERT OR IGNORE INTO usage_messages(session_id, message_id, agent, model, at, input, output, cache_read, cache_write, cache_write_1h)
                  VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 0)`,
      sessionId, `learn:${id}:${phase}`, LEARN_AGENT, typeof model === 'string' && model ? model.slice(0, 100) : 'unknown', Date.now(),
      count(u.input_tokens), count(u.output_tokens), count(u.cache_read_input_tokens), count(u.cache_creation_input_tokens));
  }

  /** Closes a step through the mod with what it got done, done or failed (with the part it reached). */
  private close(job: Job, outcome: 'done' | 'failed', failedStep: string | null): void {
    this.jobs.delete(job.id);
    clearTimeout(job.timer);
    // A part the mod hasn't polled for yet would make it fork the session for nothing.
    this.core.sessions.mod.withdrawLearn(job.sessionId, job.id);
    const c = get<{ lookups: number; errors: number; sent: number }>(this.db,
      `SELECT COALESCE(SUM(tool = 'context_search'), 0) AS lookups, COALESCE(SUM(ok = 0), 0) AS errors, COALESCE(SUM(input_chars), 0) AS sent
       FROM meko_agentk_calls WHERE learn_id = ?`, job.id);
    run(this.db, `UPDATE meko_learns SET ended_at = ?, outcome = ?, failed_step = ?, saves = ?, updates = ?, skips = ?, lookups = ?, errors = ?, sent_chars = ?
                  WHERE id = ? AND ended_at IS NULL`,
      Date.now(), outcome, failedStep, job.saves, job.updates, job.skips, c?.lookups ?? 0, c?.errors ?? 0, c?.sent ?? 0, job.id);
    this.core.changed();
    this.core.lifecycle.afterLearn(job.sessionId);
  }

  private finish(job: Job): void {
    this.close(job, 'done', null);
    const text = job.facts.length ? learnSummary(job.saves, job.updates, job.datapack.name) : `Nothing new to save to ${job.datapack.name}`;
    this.core.sessions.event(job.sessionId, 'learned', text, { saves: job.saves, updates: job.updates, skips: job.skips, candidates: job.facts.length });
    this.core.sessions.mod.queueToast(job.sessionId, `${text}.`);
  }

  private fail(job: Job, step: string, reason: string): void {
    this.close(job, 'failed', step);
    const saved = job.saves + job.updates ? ` (${learnSummary(job.saves, job.updates, job.datapack.name).replace(/^S/, 's')} first)` : '';
    // Save learnings can be tried again; at Merge & Close the session is closing.
    const again = job.trigger === 'manual' ? ' Try Save learnings again.' : '';
    const text = `AgentK couldn't save what this session learned: ${reason}${saved}.${again}`;
    this.core.sessions.event(job.sessionId, 'learned', text);
    this.core.sessions.mod.queueToast(job.sessionId, text);
    this.core.log.warn(`the learn step for ${job.sessionId} failed at ${step}`, reason);
  }

  // ---- as a typed turn (no mod) ----

  private async startTurn(row: SessionRow, trigger: LearnTrigger): Promise<void> {
    const id = randomUUID();
    this.insert(row.id, id, trigger, 'turn', null);
    try {
      await this.core.sessions.submit(row.id, this.core.sessions.prompt(row, 'learn'));
    } catch (e) {
      run(this.db, 'DELETE FROM meko_learns WHERE id = ?', id);
      throw e;
    }
  }

  /**
   * A turn ended (Lifecycle.onTurnEnd): closes a learn step that ran as a typed prompt, counting
   * what Claude saved and looked up during it (its Meko calls in the transcript, read by then).
   */
  turnEnded(sessionId: string): void {
    const open = this.openRow(sessionId);
    if (!open || open.path !== 'turn') return;
    const now = Date.now();
    const c = get<{ saves: number; updates: number; lookups: number; errors: number; sent: number }>(this.db,
      `SELECT COALESCE(SUM(tool = 'memory_add'), 0) AS saves, COALESCE(SUM(tool = 'memory_update'), 0) AS updates,
              COALESCE(SUM(kind = 'lookup'), 0) AS lookups, COALESCE(SUM(outcome = 'error'), 0) AS errors, COALESCE(SUM(input_chars), 0) AS sent
       FROM meko_calls WHERE session_id = ? AND at >= ? AND at <= ?`, sessionId, open.started_at, now);
    const saves = c?.saves ?? 0;
    const updates = c?.updates ?? 0;
    run(this.db, `UPDATE meko_learns SET ended_at = ?, outcome = 'done', candidates = ?, saves = ?, updates = ?, lookups = ?, errors = ?, sent_chars = ?
                  WHERE id = ?`, now, saves + updates, saves, updates, c?.lookups ?? 0, c?.errors ?? 0, c?.sent ?? 0, open.id);
    const row = sessionRow(this.db, sessionId);
    const dp = row ? sessionDatapack(this.db, row) : null;
    this.core.sessions.event(sessionId, 'learned', learnSummary(saves, updates, dp?.name ?? 'Meko'), { saves, updates });
    this.core.changed();
    // At Merge & Close, Lifecycle.onTurnEnd looks at the PR next, which finishes the close.
  }

  // ---- ends ----

  /** Claude exited, was relaunched or the session was archived: a learn step running in it is over. */
  interrupted(sessionId: string): void {
    const open = this.openRow(sessionId);
    if (!open) return;
    const job = this.jobs.get(open.id);
    if (job) {
      clearTimeout(job.timer);
      this.jobs.delete(open.id);
    }
    run(this.db, "UPDATE meko_learns SET ended_at = ?, outcome = 'interrupted', saves = ?, updates = ?, skips = ? WHERE id = ?",
      Date.now(), job?.saves ?? 0, job?.updates ?? 0, job?.skips ?? 0, open.id);
    this.core.lifecycle.afterLearn(sessionId);
  }

  /**
   * At startup: learn steps left running by the server before are over (a mod step lived in its
   * memory; a typed one's turn may still end, but its count can't be trusted any more).
   */
  recover(): void {
    for (const r of all<{ id: string }>(this.db, 'SELECT id FROM meko_learns WHERE ended_at IS NULL')) {
      run(this.db, "UPDATE meko_learns SET ended_at = ?, outcome = 'interrupted' WHERE id = ?", Date.now(), r.id);
    }
  }

  /** The server is stopping: no timer outlives it, and a step still writing stops at its next fact. */
  stop(): void {
    for (const j of this.jobs.values()) clearTimeout(j.timer);
    this.jobs.clear();
  }
}

/** What a step's timer says, by the part the step was in. */
const TIMED_OUT: Record<Job['step'], string> = {
  extract: "AgentK's mod in Claude didn't answer in time",
  search: 'Meko took too long to look them up',
  check: "AgentK's mod in Claude didn't answer in time",
  save: 'Meko took too long to save them',
};

/** Why a model call the mod ran gave no reply, in plain words (`reason` is the engine's code). */
function noReply(reason: unknown, what: string): string {
  switch (reason) {
    case 'nothing-to-fork': return `Claude had nothing to look back on yet (the session was just started or cleared)`;
    case 'api-error': return `the model call to ${what} failed`;
    case 'empty-reply': return `Claude gave an empty answer when asked to ${what}`;
    case 'aborted': return `the model call to ${what} was interrupted`;
    default: return `this Claude Code couldn't ${what}`;
  }
}
