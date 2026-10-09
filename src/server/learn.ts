import { randomUUID } from 'node:crypto';
import { homedir } from 'node:os';
import type { Core } from './core.js';
import { all, get, run } from './db.js';
import type { ModReply } from './hooks.js';
import { errMsg } from './log.js';
import { launchedAgentId, mekoTag, type SimilarMemory } from './meko.js';
import { LEARN_AGENT } from './mekousage.js';
import { HttpError } from './projects.js';
import type { DatapackRef } from '../shared/types.js';
import { sessionDatapack, sessionRow, type SessionRow } from './store.js';

export type LearnTrigger = 'merge' | 'manual';
export type LearnPath = 'mod' | 'turn' | 'none';
export type LearnOutcome = 'done' | 'failed' | 'interrupted' | 'skipped';

/** How long a learn step through the mod may take, all steps together, before it's given up. */
export const LEARN_TIMEOUT_MS = 180_000;
/** How long a finished step's answer is kept, for a mod that posts the same step again. */
const DONE_KEEP_MS = 10 * 60 * 1000;
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

/** What the fork is asked: the facts this session learned, as JSON. The saving rule is in the system prompt it reuses. */
export const EXTRACT_PROMPT = `Don't call any tools. List what this session learned that passes the saving rule in your system prompt: facts about how the product and repo work, decisions and why, root causes, gotchas, quirks, measured numbers. Leave out anything about this machine, one person's preferences, task or PR status, and what the diff or PR description already says. Each fact must make sense on its own to a teammate who never saw this session; name the component, file or function it's about.

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
      kind: typeof f.kind === 'string' && f.kind in KINDS ? f.kind : 'how-it-works',
      component: typeof f.component === 'string' ? f.component.trim().slice(0, 200) : '',
      text: t.slice(0, FACT_CHARS),
    });
  }
  return out;
}

/**
 * Facts that name this machine are left out, whatever the model thought: the home folder, AgentK's
 * own folders, the worktree. The datapack is shared by a team, so these never help anyone else.
 */
export function machineSpecific(f: Fact, paths: string[]): boolean {
  const s = `${f.component} ${f.text}`;
  return paths.some((p) => p.length > 1 && s.includes(p));
}

/** The text saved for a fact: its kind and component first, as the saving rule asks for. */
export function memoryText(f: Fact): string {
  const label = KINDS[f.kind] ?? 'How it works';
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
  step: 'extract' | 'dedup';
  facts: Fact[];
  similar: SimilarMemory[][];
  /** The duplicate check's prompt, once built (a repeated facts post gets it again). */
  dedup: string | null;
  timer: NodeJS.Timeout;
}

/**
 * The learn step: what a session learned about the product and repo goes to the project's
 * dedicated datapack. Through AgentK's mod when it's live: the mod forks the session's own context
 * for the facts, AgentK finds the saved memories most like each, the mod asks a small model which
 * are new, known or corrections, and AgentK writes them. Without the mod, a typed `learn` prompt
 * asks Claude to save them itself. Runs at Merge & Close (before cleanup), and when the user asks
 * (the session menu, or /agentk_learn in the terminal).
 */
export class Learner {
  private jobs = new Map<string, Job>();
  /** Answers of finished mod steps, by job id, for a repeated post. */
  private done = new Map<string, { sessionId: string; reply: ModReply; at: number }>();
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

  /** Claude is there and between turns, so a learn step can run in it now. */
  private idle(row: SessionRow): boolean {
    return row.desired === 'running' && (row.status === 'idle' || row.status === 'recovered') && !this.core.sessions.runtime(row.id).busy;
  }

  /**
   * Starts a learn step for the session: through the mod when it's live, else as a typed prompt.
   * Throws a 409 saying why it can't run now.
   */
  async start(row: SessionRow, trigger: LearnTrigger): Promise<LearnPath> {
    const dp = this.datapack(row);
    if (!dp) throw new HttpError(409, "This session's project has no Meko datapack to save to.");
    if (row.desired !== 'running') throw new HttpError(409, 'This session is archived.');
    if (this.openRow(row.id)) throw new HttpError(409, 'AgentK is already saving what this session learned.');
    if (!this.idle(row)) throw new HttpError(409, 'Claude is busy. Try again when its turn has ended.');
    if (this.core.config.claude.mod && this.core.sessions.mod.learnReady(row.id)) {
      this.startMod(row, trigger, dp);
      return 'mod';
    }
    await this.startTurn(row, trigger);
    return 'turn';
  }

  /**
   * At Merge & Close, before cleanup: true while the cleanup should wait for a learn step (one ran
   * now, or is still running); `merged` runs again once it ends (Lifecycle.afterLearn). A session
   * that can't run one (Claude isn't there or is busy) is recorded as skipped.
   */
  async beforeClose(row: SessionRow): Promise<boolean> {
    if (this.openRow(row.id)) return true;
    if (get(this.db, "SELECT 1 AS x FROM meko_learns WHERE session_id = ? AND trigger = 'merge'", row.id)) return false;
    if (!this.datapack(row)) return false;
    if (!this.idle(row)) {
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
  }

  // ---- through the mod ----

  private startMod(row: SessionRow, trigger: LearnTrigger, dp: DatapackRef): void {
    const id = randomUUID();
    this.insert(row.id, id, trigger, 'mod', null);
    const timer = setTimeout(() => {
      const job = this.jobs.get(id);
      if (job) void this.fail(job, job.step, "AgentK's mod in Claude didn't finish in time");
    }, this.timeoutMs);
    timer.unref?.();
    this.jobs.set(id, { id, sessionId: row.id, trigger, datapack: dp, step: 'extract', facts: [], similar: [], dedup: null, timer });
    this.core.sessions.mod.queueLearn(row.id, { id, prompt: EXTRACT_PROMPT });
    this.core.log.info(`saving what ${row.id} learned to ${dp.name} through AgentK's mod`);
  }

  /**
   * The mod's `/mod/learn` posts: the fork's reply (`step: "facts"`), answered with the duplicate
   * check's prompt and model (or `done` when there's nothing to check), then the small model's
   * reply (`step: "decisions"`), answered `done` with what was saved. 410 for a step AgentK gave up
   * on. Each post carries the call's usage, recorded as the session's tokens.
   */
  async fromMod(row: SessionRow, body: Record<string, unknown>): Promise<ModReply> {
    const id = typeof body.id === 'string' ? body.id : '';
    this.sweepDone();
    const finished = this.done.get(id);
    if (finished?.sessionId === row.id) return finished.reply;
    const job = this.jobs.get(id);
    if (!job || job.sessionId !== row.id) return { status: 410 };
    const step = body.step === 'decisions' ? 'dedup' : 'extract';
    this.recordUsage(row.id, id, step, body.model, body.usage);
    if (step === 'extract') {
      if (job.dedup) return { status: 200, body: { prompt: job.dedup, model: DEDUP_MODEL } };
      if (job.step !== 'extract') return { status: 409 };
      return this.facts(row, job, body);
    }
    if (job.step !== 'dedup') return { status: 409 };
    return this.decisions(row, job, body);
  }

  private async facts(row: SessionRow, job: Job, body: Record<string, unknown>): Promise<ModReply> {
    if (typeof body.text !== 'string') return this.fail(job, 'extract', reasonOf(body.reason, 'the fork gave no reply'));
    const parsed = parseFacts(body.text);
    if (!parsed) return this.fail(job, 'extract', "the fork's reply wasn't the JSON asked for");
    const local = [homedir(), this.core.paths.home, row.worktree_path, row.repo_path ?? ''].filter(Boolean);
    const facts = parsed.filter((f) => !machineSpecific(f, local));
    job.facts = facts;
    run(this.db, 'UPDATE meko_learns SET candidates = ? WHERE id = ?', facts.length, job.id);
    if (!facts.length) return this.finish(job, 0, 0, 0);
    try {
      const conv = await this.core.sessions.conversationId(row, job.datapack);
      const agent = launchedAgentId(row);
      job.similar = await mekoTag.run(job.id, () => Promise.all(facts.map((f) =>
        this.core.meko.similar(memoryText(f), job.datapack.id, agent, conv, SIMILAR).catch((): SimilarMemory[] => []))));
    } catch (e) {
      return this.fail(job, 'search', errMsg(e));
    }
    if (!this.jobs.has(job.id)) return { status: 410 }; // timed out meanwhile
    job.step = 'dedup';
    job.dedup = dedupPrompt(facts, job.similar);
    return { status: 200, body: { prompt: job.dedup, model: DEDUP_MODEL } };
  }

  private async decisions(row: SessionRow, job: Job, body: Record<string, unknown>): Promise<ModReply> {
    if (typeof body.text !== 'string') return this.fail(job, 'dedup', reasonOf(body.reason, 'the duplicate check gave no reply'));
    const decided = parseDecisions(body.text);
    if (!decided) return this.fail(job, 'dedup', "the duplicate check's reply wasn't the JSON asked for");
    // Taken off the jobs at once: the timeout can't fail it while the writes run, and a repeated
    // post waits for nothing (it gets 410 until the answer is kept).
    this.jobs.delete(job.id);
    clearTimeout(job.timer);
    let saves = 0;
    let updates = 0;
    let skips = 0;
    try {
      const conv = await this.core.sessions.conversationId(row, job.datapack);
      const agent = launchedAgentId(row);
      await mekoTag.run(job.id, async () => {
        for (const [i, f] of job.facts.entries()) {
          const d = decided.get(i + 1) ?? { action: 'add', id: null, text: null };
          const target = d.action === 'update' ? (job.similar[i] ?? []).find((m) => m.id === d.id) : undefined;
          try {
            if (d.action === 'skip') {
              skips++;
            } else if (target) {
              await this.core.meko.updateMemory(target.id, d.text ?? memoryText(f), job.datapack.id, target.agentId, conv);
              updates++;
            } else {
              await this.core.meko.addMemory(memoryText(f), job.datapack.id, agent, conv, {
                source: 'agent-k-local', kind: 'learned', fact: f.kind, component: f.component || null, session: row.id, learn: job.id,
              });
              saves++;
            }
          } catch (e) {
            this.core.log.warn(`saving a learned fact for ${row.id} failed`, errMsg(e));
          }
        }
      });
    } catch (e) {
      return this.fail(job, 'save', errMsg(e));
    }
    return this.finish(job, saves, updates, skips);
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

  private finish(job: Job, saves: number, updates: number, skips: number): ModReply {
    this.jobs.delete(job.id);
    clearTimeout(job.timer);
    const c = get<{ lookups: number; errors: number; sent: number }>(this.db,
      `SELECT COALESCE(SUM(tool = 'context_search'), 0) AS lookups, COALESCE(SUM(ok = 0), 0) AS errors, COALESCE(SUM(input_chars), 0) AS sent
       FROM meko_agentk_calls WHERE learn_id = ?`, job.id);
    run(this.db, `UPDATE meko_learns SET ended_at = ?, outcome = 'done', saves = ?, updates = ?, skips = ?, lookups = ?, errors = ?, sent_chars = ?
                  WHERE id = ? AND ended_at IS NULL`,
      Date.now(), saves, updates, skips, c?.lookups ?? 0, c?.errors ?? 0, c?.sent ?? 0, job.id);
    const text = job.facts.length ? learnSummary(saves, updates, job.datapack.name) : `Nothing new to save to ${job.datapack.name}`;
    this.core.sessions.event(job.sessionId, 'learned', text, { saves, updates, skips, candidates: job.facts.length });
    return this.closed(job, { status: 200, body: { done: `${text}.` } });
  }

  private fail(job: Job, step: string, reason: string): ModReply {
    this.jobs.delete(job.id);
    clearTimeout(job.timer);
    run(this.db, "UPDATE meko_learns SET ended_at = ?, outcome = 'failed', failed_step = ? WHERE id = ? AND ended_at IS NULL", Date.now(), step, job.id);
    this.core.sessions.event(job.sessionId, 'learned', `AgentK couldn't save what this session learned: ${reason}`);
    this.core.log.warn(`the learn step for ${job.sessionId} failed at ${step}`, reason);
    return this.closed(job, { status: 200, body: { done: `AgentK couldn't save what this session learned: ${reason}.` } });
  }

  private closed(job: Job, reply: ModReply): ModReply {
    this.done.set(job.id, { sessionId: job.sessionId, reply, at: Date.now() });
    this.core.changed();
    this.core.lifecycle.afterLearn(job.sessionId, job.trigger);
    return reply;
  }

  private sweepDone(): void {
    const now = Date.now();
    for (const [id, d] of this.done) if (now - d.at > DONE_KEEP_MS) this.done.delete(id);
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
    run(this.db, "UPDATE meko_learns SET ended_at = ?, outcome = 'interrupted' WHERE id = ?", Date.now(), open.id);
    this.core.lifecycle.afterLearn(sessionId, open.trigger);
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

  /** The server is stopping: no timer outlives it. */
  stop(): void {
    for (const j of this.jobs.values()) clearTimeout(j.timer);
    this.jobs.clear();
  }
}

function reasonOf(v: unknown, fallback: string): string {
  return typeof v === 'string' && v.trim() ? v.trim().slice(0, 200) : fallback;
}
