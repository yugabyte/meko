import { randomUUID } from 'node:crypto';
import { homedir } from 'node:os';
import type { Core } from './core.js';
import { all, get, run } from './db.js';
import type { ModReply } from './hooks.js';
import { errMsg } from './log.js';
import { launchedAgentId, mekoTag, withMekoScope, type SimilarMemory } from './meko.js';
import type { MekoQuestion, QuestionMark } from './mekoquestions.js';
import { LEARN_AGENT } from './mekousage.js';
import { claudePath } from './paths.js';
import { HttpError } from './projects.js';
import { SAVING_RULE } from './prompts.js';
import type { DatapackRef, LearnProgress } from '../shared/types.js';
import { sessionDatapack, sessionRepo, sessionRow, type SessionRow } from './store.js';
import { agentName } from '../shared/harnesses.js';

export type LearnTrigger = 'merge' | 'manual' | 'discard';
export type LearnPath = 'mod' | 'turn' | 'none';
export type LearnOutcome = 'done' | 'failed' | 'interrupted' | 'skipped';

/** How long a learn step through the mod may take, all its parts and writes together, before it's given up. */
export const LEARN_TIMEOUT_MS = 180_000;
/** The small model that checks the facts against what Meko already has. */
export const DEDUP_MODEL = 'claude-haiku-5-5';
/** How many saved memories each fact is checked against. */
const SIMILAR = 3;
/** A fact longer than this is cut (a reply that put a whole document in one fact). */
const FACT_CHARS = 2000;
/** The most facts from earlier learn steps a new one is told about (the latest), and how much of each. */
const COVERED_MAX = 60;
const COVERED_CHARS = 400;
/** The most of Meko's open questions a learn step is offered (the session's own first), and how much of each. */
export const LEARN_QUESTIONS_MAX = 50;
export const LEARN_QUESTION_CHARS = 300;

/**
 * The open questions on Meko's Context Gaps a learn step offers the fork: those the session's own
 * conversation asked first, then the rest, each in Meko's order (latest asked first), at most
 * LEARN_QUESTIONS_MAX, each query on one line and cut to LEARN_QUESTION_CHARS.
 */
export function learnQuestions(open: MekoQuestion[]): { key: string; query: string; own: boolean }[] {
  return [...open.filter((q) => q.own), ...open.filter((q) => !q.own)].slice(0, LEARN_QUESTIONS_MAX)
    .map((q) => ({ key: q.key, query: q.query.replace(/\s+/g, ' ').trim().slice(0, LEARN_QUESTION_CHARS), own: q.own }));
}

const KINDS: Record<string, string> = {
  'how-it-works': 'How it works',
  decision: 'Decision',
  'root-cause': 'Root cause',
  gotcha: 'Gotcha',
  measured: 'Measured',
};

/**
 * What the fork is asked: the facts this session learned, as JSON, leaving out what it already
 * saved itself and what earlier learn steps covered (`covered`; a fork leaves nothing in the
 * transcript, so it can't see them). It states the saving rule in full: a session launched before
 * the rule was in the system prompt doesn't have it. With `questions` (the open questions on Meko's
 * Context Gaps, the session's own first, `learnQuestions`), each fact also names the ones it answers, by number.
 */
export function extractPrompt(covered: string[], questions: { query: string; own: boolean }[] = []): string {
  const facts = questions.length
    ? '{"facts":[{"kind":"how-it-works|decision|root-cause|gotcha|measured","component":"…","text":"one self-contained fact","answers":[1]}]}'
    : '{"facts":[{"kind":"how-it-works|decision|root-cause|gotcha|measured","component":"…","text":"one self-contained fact"}]}';
  return `Don't call any tools. List what this session learned that is worth keeping for the team working on this product. ${SAVING_RULE} Each fact must make sense on its own to a teammate who never saw this session. Leave out what you already saved to Meko yourself in this session (memory_add), unless the session has since shown it wrong.${coveredText(covered)}${questionsText(questions)}

Reply with JSON only, no other text:
${facts}
Reply {"facts":[]} when there is nothing new worth saving.`;
}

/**
 * The questions part of the fork's prompt: the searches Meko couldn't answer, numbered in one run,
 * this session's (they come first) and then the others on the datapack (any agent's that searched it
 * through Meko). Empty when there are none.
 */
function questionsText(questions: { query: string; own: boolean }[]): string {
  if (!questions.length) return '';
  const numbered = questions.map((q, i) => ({ own: q.own, line: `${i + 1}. ${q.query}` }));
  const own = numbered.filter((q) => q.own).map((q) => q.line).join('\n');
  const others = numbered.filter((q) => !q.own).map((q) => q.line).join('\n');
  const lists = [
    own && `Meko couldn't answer these searches from this session:\n${own}`,
    others && (own ? `Nor these, other searches on this datapack:\n${others}` : `Meko couldn't answer these other searches on this datapack:\n${others}`),
  ].filter(Boolean).join('\n\n');
  return `\n\n${lists}\nFor each one this session found the answer to, include a fact that answers it, and list its number in that fact's "answers" (a fact that answers none has "answers":[]). One fact can answer several that ask the same thing. Include it even if you saved the answer to Meko yourself: AgentK checks it against what Meko has. Leave a question out when this session didn't find the answer, or when the only answer is about this task or this machine. Don't guess: answer another search's question only when this session established the answer itself.`;
}

/**
 * The facts earlier learn steps covered, for the next step's prompt (the fork's, or the typed
 * one's): the latest `COVERED_MAX`, oldest first, each cut to `COVERED_CHARS`. Empty when there are none.
 */
export function coveredText(covered: string[]): string {
  if (!covered.length) return '';
  const list = covered.slice(-COVERED_MAX).map((t) => {
    const one = t.replace(/\s+/g, ' ').trim();
    return `- ${one.length > COVERED_CHARS ? `${one.slice(0, COVERED_CHARS)}…` : one}`;
  }).join('\n');
  return `\n\nEarlier learn steps in this session already saved these, or found them already in Meko. Leave them out, and anything that only restates them; this step is for what's new since, or what shows one of them wrong:\n${list}`;
}

export interface Fact {
  kind: string;
  component: string;
  text: string;
  /** The questions it answers, by their number in the prompt (from 1). */
  answers: number[];
}

export interface Decision {
  action: 'add' | 'skip' | 'update';
  /** For an update: the memory it corrects; for a skip: the one that already says it. */
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

/**
 * The facts in the fork's reply, or null when it isn't the JSON asked for. Facts without text are
 * dropped, and so are answers that aren't the number of one of the `questions` it was offered.
 */
export function parseFacts(text: string, questions = 0): Fact[] | null {
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
      answers: Array.isArray(f.answers)
        ? [...new Set((f.answers as unknown[]).filter((n): n is number => Number.isInteger(n) && (n as number) >= 1 && (n as number) <= questions))]
        : [],
    });
  }
  return out;
}

/**
 * Text that names this machine is left out, whatever the model thought: the home folder, AgentK's
 * own folders, the worktree, the repo. The datapack is shared by a team, so it never helps anyone else.
 * A path counts only whole: a short one (a home's local spelling can be just `/data`) doesn't
 * match inside a longer name such as `src/database.ts`.
 */
export function machineSpecific(text: string, paths: string[]): boolean {
  return paths.some((p) => p.length > 1 && namesPath(text, p));
}

const PATH_CHAR = /[\w.~-]/;

function namesPath(text: string, p: string): boolean {
  for (let i = text.indexOf(p); i >= 0; i = text.indexOf(p, i + 1)) {
    const before = text[i - 1];
    const after = text[i + p.length];
    if ((before === undefined || !PATH_CHAR.test(before)) && (after === undefined || !PATH_CHAR.test(after))) return true;
  }
  return false;
}

/** The text saved for a fact: its kind and component first, as the saving rule asks for. */
export function memoryText(f: Pick<Fact, 'kind' | 'component' | 'text'>): string {
  const label = Object.hasOwn(KINDS, f.kind) ? KINDS[f.kind] : 'How it works';
  if (f.text.toLowerCase().startsWith(label.toLowerCase())) return f.text;
  return `${label}${f.component ? ` (${f.component})` : ''}: ${f.text}`;
}

/** What the small model is asked: for each fact, add it, skip it, or correct a memory with it. */
export function dedupPrompt(facts: Pick<Fact, 'kind' | 'component' | 'text'>[], similar: SimilarMemory[][]): string {
  const list = facts.map((f, i) => {
    const near = similar[i] ?? [];
    const mems = near.length ? near.map((m) => `   - [${m.id}] ${m.text.replace(/\s+/g, ' ').slice(0, 600)}`).join('\n') : '   (none)';
    return `${i + 1}. ${memoryText(f)}\n   Saved memories most like it:\n${mems}`;
  }).join('\n\n');
  return `A coding session learned the facts below. Each is followed by the memories already saved that are most like it. For each fact, decide:
- "skip" if one of those memories already says it (the same point, even in other words): give that memory's id
- "update" if one of those memories says something this fact shows is wrong or out of date: give that memory's id and its corrected text
- "add" otherwise

Reply with JSON only, no other text:
{"decisions":[{"n":1,"action":"add|skip|update","id":"memory id, for update or skip","text":"corrected text, for update"}]}

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

/**
 * "Saved 3 memories and corrected 1 in team/demo", the timeline's line for a learn step, then what
 * it did with the project's questions Meko couldn't answer (other sessions' too), when it did anything.
 */
export function learnSummary(saves: number, updates: number, datapack: string, taught = 0, dismissed = 0): string {
  const base = `Saved ${saves} ${saves === 1 ? 'memory' : 'memories'} and corrected ${updates} in ${datapack}`;
  const q = (n: number) => `${n} ${n === 1 ? 'question' : 'questions'}`;
  const parts = [
    ...(taught ? [`taught Meko ${q(taught)} it couldn't answer`] : []),
    ...(dismissed ? [`dismissed ${q(dismissed)} it knew already`] : []),
  ];
  return parts.length ? `${base}; ${parts.join(' and ')}` : base;
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
  /** Meko's open questions on the datapack, as offered to the fork (its numbers are their places, from 1). */
  questions: { key: string; query: string }[];
  /** The session's Meko conversation, which the questions were read for and its marks are made in (null when Meko didn't give it). */
  conversation: string | null;
  saves: number;
  updates: number;
  skips: number;
  /** Facts the save part has been through, whatever became of each (for `Session.learnProgress`). */
  tried: number;
  /** Questions marked taught (a fact answering them was written) or dismissed (Meko already said it). */
  taught: number;
  dismissed: number;
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
 * Close (before cleanup) and Discard (before the session ends), unless the user asked to skip it,
 * and when the user asks (the session menu, or /agentk_learn in the terminal).
 */
export class Learner {
  private jobs = new Map<string, Job>();
  /**
   * By session, the facts its learn steps through the mod covered in the datapack they wrote to, for
   * the next step's prompt. Kept in memory only: after a restart the next step lists them again, and
   * the duplicate check skips them.
   */
  private coveredFacts = new Map<string, { datapack: string; facts: string[] }>();
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

  /** How the session's latest learn step ran (or was skipped); null before its first. */
  lastPath(sessionId: string): LearnPath | null {
    return get<{ path: LearnPath }>(this.db, 'SELECT path FROM meko_learns WHERE session_id = ? ORDER BY started_at DESC LIMIT 1', sessionId)?.path ?? null;
  }

  /**
   * What the session's earlier learn steps through the mod saved, corrected or found already saved
   * in `datapack`, oldest first. None after the session moved to another datapack: those facts aren't in it.
   */
  private covered(sessionId: string, datapack: string): string[] {
    const c = this.coveredFacts.get(sessionId);
    return c?.datapack === datapack ? c.facts : [];
  }

  /** Notes the text a step wrote (or found already saved) for the session's next learn step (the latest COVERED_MAX). */
  private cover(job: Job, text: string): void {
    const facts = this.covered(job.sessionId, job.datapack.id).filter((t) => t !== text);
    this.coveredFacts.set(job.sessionId, { datapack: job.datapack.id, facts: [...facts, text].slice(-COVERED_MAX) });
  }

  /** The session was archived or its project deleted: its covered facts go. */
  forget(sessionId: string): void {
    this.coveredFacts.delete(sessionId);
  }

  /** True while the session has a learn step running. */
  running(sessionId: string): boolean {
    return !!this.openRow(sessionId);
  }

  /** Where the session's running learn step is (`Session.learnProgress`); null when none runs. */
  progress(sessionId: string): LearnProgress | null {
    const row = this.openRow(sessionId);
    if (!row) return null;
    const job = this.jobs.get(row.id);
    // A step through the mod has its job until it closes (steps left open by an earlier server are
    // closed at startup), so a missing one reads as just started.
    if (row.path !== 'mod' || !job) return { step: row.path === 'turn' ? 'turn' : 'extract', facts: 0, done: 0, startedAt: row.started_at };
    return { step: job.step, facts: job.facts.length, done: job.tried, startedAt: row.started_at };
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
    if (row.status === 'exited') return `${agentName(row)} has exited in this session. Resume it, then try again.`;
    if (row.status === 'starting') return `${agentName(row)} is still starting. Try again in a moment.`;
    if ((row.status !== 'idle' && row.status !== 'recovered') || this.core.sessions.runtime(row.id).busy) {
      return `${agentName(row)} is busy. Try again when its turn has ended.`;
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
    await this.startTurn(row, trigger, dp);
    return 'turn';
  }

  /**
   * At Merge & Close, before cleanup: true while the cleanup should wait for a learn step (one ran
   * now, or one is still running, Save learnings' too); `merged` runs again once it ends
   * (Lifecycle.afterLearn). One that finished since the session got to Merge & Close covers it, and
   * none runs when the user asked Merge & Close to skip it (`skip_learn`). A session that can't run
   * one (Claude isn't there or is busy) is recorded as skipped.
   */
  async beforeClose(row: SessionRow): Promise<boolean> {
    if (this.openRow(row.id)) return true;
    if (get(this.db, "SELECT 1 AS x FROM meko_learns WHERE session_id = ? AND trigger = 'merge'", row.id)) return false;
    if (!this.datapack(row)) return false;
    if (row.skip_learn) {
      this.skipped(row);
      return false;
    }
    const merging = get<{ at: number | null }>(this.db, "SELECT MAX(entered_at) AS at FROM session_stages WHERE session_id = ? AND stage = 'merge'", row.id)?.at;
    if (merging && get(this.db, "SELECT 1 AS x FROM meko_learns WHERE session_id = ? AND outcome = 'done' AND ended_at >= ?", row.id, merging)) return false;
    return this.beforeEnd(row, 'merge');
  }

  /**
   * At Discard, unless the user asked to skip it: true while the discard should wait for a learn
   * step (one ran now, or Save learnings' is still running); Lifecycle.afterLearn discards the
   * session once it ends. A session that can't run one (Claude isn't there or is busy) is recorded
   * as skipped.
   */
  async beforeDiscard(row: SessionRow): Promise<boolean> {
    if (this.openRow(row.id)) return true;
    if (!this.datapack(row)) return false;
    return this.beforeEnd(row, 'discard');
  }

  /**
   * Notes in the timeline, once, that the user asked a closing session to skip the learn step (when
   * it had one to run). Not while one runs: Discard now cuts it short, and it's recorded as interrupted.
   */
  skipped(row: SessionRow): void {
    if (!this.datapack(row) || this.openRow(row.id) || get(this.db, "SELECT 1 AS x FROM session_events WHERE session_id = ? AND kind = 'learn-skipped'", row.id)) return;
    this.core.sessions.event(row.id, 'learn-skipped', SKIPPED_TEXT);
  }

  /** Starts the learn step a closing session runs; false (recorded as skipped) when it can't run. */
  private async beforeEnd(row: SessionRow, trigger: 'merge' | 'discard'): Promise<boolean> {
    if (this.notNow(row)) {
      this.insert(row.id, randomUUID(), trigger, 'none', 'skipped');
      return false;
    }
    try {
      await this.start(row, trigger);
      return true;
    } catch (e) {
      this.core.log.warn(`the learn step didn't start for ${row.id}`, errMsg(e));
      if (!this.openRow(row.id)) this.insert(row.id, randomUUID(), trigger, 'none', 'skipped');
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

  /**
   * Starts a step through the mod and returns at once, its timeout armed: nothing that starts one
   * (the hook socket's `/mod/learn/start`, Merge & Close, Discard) waits on Meko. The session's
   * conversation and Meko's open questions on the datapack (the session's own first; none when Meko
   * doesn't keep them) are read in the background, then the fork's prompt is handed to the mod.
   */
  private startMod(row: SessionRow, trigger: LearnTrigger, dp: DatapackRef): void {
    const id = randomUUID();
    this.insert(row.id, id, trigger, 'mod', null);
    const timer = setTimeout(() => {
      const job = this.jobs.get(id);
      if (job) this.fail(job, job.step, TIMED_OUT[job.step]);
    }, this.timeoutMs);
    timer.unref?.();
    const job: Job = { id, sessionId: row.id, trigger, datapack: dp, step: 'extract', facts: [], similar: [], questions: [], conversation: null, saves: 0, updates: 0, skips: 0, tried: 0, taught: 0, dismissed: 0, timer };
    this.jobs.set(id, job);
    this.core.log.info(`saving what ${row.id} learned to ${dp.name} through AgentK's mod`);
    void (async () => {
      const conversation = await this.core.sessions.conversationId(row, dp).catch(() => null);
      const questions = learnQuestions((await this.core.mekoQuestions.openQuestions(dp.id, conversation)) ?? []);
      // Closed meanwhile (timed out, the turn was interrupted, or the session went): nothing to hand the mod.
      if (this.jobs.get(id) !== job) return;
      job.questions = questions;
      job.conversation = conversation;
      run(this.db, 'UPDATE meko_learns SET questions = ? WHERE id = ?', questions.length, id);
      this.core.sessions.mod.queueLearn(row.id, { id, kind: 'extract', prompt: extractPrompt(this.covered(row.id, dp.id), questions) });
    })().catch((e: unknown) => {
      const live = this.jobs.get(id);
      if (live) this.fail(live, 'extract', errMsg(e));
    });
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

  /** The local paths a fact must not name, by their stored spelling and the one Claude was given (claudePath). */
  private localPaths(row: SessionRow): string[] {
    const stored = [homedir(), this.core.paths.home, row.worktree_path, sessionRepo(this.db, row) ?? ''].filter(Boolean);
    return [...new Set(stored.flatMap((p) => [p, claudePath(p)]))];
  }

  private facts(row: SessionRow, job: Job, body: Record<string, unknown>): void {
    if (typeof body.text !== 'string') return this.fail(job, 'extract', noReply(body.reason, 'look back over the session'));
    const parsed = parseFacts(body.text, job.questions.length);
    if (!parsed) return this.fail(job, 'extract', "Claude didn't list them in a form AgentK could read");
    const local = this.localPaths(row);
    job.facts = parsed.filter((f) => !machineSpecific(`${f.component} ${f.text}`, local));
    run(this.db, 'UPDATE meko_learns SET candidates = ? WHERE id = ?', job.facts.length, job.id);
    if (!job.facts.length) return this.finish(job);
    job.step = 'search';
    this.core.changed(); // Session.learnProgress
    void this.search(row, job);
  }

  /** Finds the saved memories most like each fact, then hands the mod the duplicate check. */
  private async search(row: SessionRow, job: Job): Promise<void> {
    try {
      const conv = await this.core.sessions.conversationId(row, job.datapack);
      const agent = launchedAgentId(row);
      job.similar = await withMekoScope(row, () => mekoTag.run(job.id, () => Promise.all(job.facts.map((f) =>
        this.core.meko.similar(memoryText(f), job.datapack.id, agent, conv, SIMILAR).catch((): SimilarMemory[] => [])))));
    } catch (e) {
      if (this.jobs.has(job.id)) this.fail(job, 'search', `Meko couldn't be searched (${errMsg(e)})`);
      return;
    }
    if (!this.jobs.has(job.id)) return; // timed out meanwhile
    job.step = 'check';
    this.core.changed();
    this.core.sessions.mod.queueLearn(job.sessionId, { id: job.id, kind: 'check', prompt: dedupPrompt(job.facts, job.similar), model: DEDUP_MODEL });
  }

  private decisions(row: SessionRow, job: Job, body: Record<string, unknown>): void {
    if (typeof body.text !== 'string') return this.fail(job, 'dedup', noReply(body.reason, 'check them against what Meko has'));
    const decided = parseDecisions(body.text);
    if (!decided) return this.fail(job, 'dedup', "the check against what Meko has didn't answer in a form AgentK could read");
    job.step = 'save';
    this.core.changed();
    void this.save(row, job, decided);
  }

  /** Writes what the duplicate check decided, one fact at a time, until the step's time is up. */
  private async save(row: SessionRow, job: Job, decided: Map<number, Decision>): Promise<void> {
    const local = this.localPaths(row);
    try {
      const conv = await this.core.sessions.conversationId(row, job.datapack);
      const agent = launchedAgentId(row);
      await withMekoScope(row, () => mekoTag.run(job.id, async () => {
        for (const [i, f] of job.facts.entries()) {
          if (!this.jobs.has(job.id)) return; // the time was up: the timer closed the step
          const d = decided.get(i + 1) ?? { action: 'add', id: null, text: null };
          const target = d.action === 'update' ? (job.similar[i] ?? []).find((m) => m.id === d.id) : undefined;
          try {
            // What Meko now holds for the fact: a correction's own text, else the fact's.
            let written = memoryText(f);
            const questions = f.answers.map((n) => job.questions[n - 1]).filter(Boolean);
            if (d.action === 'skip') {
              job.skips++;
              // Meko already says it: the questions it answers are duplicates of what Meko has.
              const known = (job.similar[i] ?? []).find((m) => m.id === d.id);
              await this.markQuestions(job, questions, { state: 'dismissed', memoryId: known?.id ?? null });
            } else if (target) {
              written = d.text && !machineSpecific(d.text, local) ? d.text : written;
              await this.core.meko.updateMemory(target.id, written, job.datapack.id, target.agentId, conv);
              job.updates++;
              await this.markQuestions(job, questions, { state: 'taught', text: written, memoryId: target.id });
            } else {
              await this.core.meko.addMemory(written, job.datapack.id, agent, conv, {
                source: 'agent-k-local', kind: 'learned', fact: f.kind, component: f.component || null, session: row.id, learn: job.id,
                ...(questions.length ? { questions: questions.map((q) => q.query) } : {}),
              });
              job.saves++;
              await this.markQuestions(job, questions, { state: 'taught', text: written });
            }
            this.cover(job, written);
          } catch (e) {
            this.core.log.warn(`saving a learned fact for ${row.id} failed`, errMsg(e));
          }
          job.tried++;
          this.core.changed(); // the count saved so far
        }
      }));
    } catch (e) {
      if (this.jobs.has(job.id)) this.fail(job, 'save', `Meko couldn't be reached (${errMsg(e)})`);
      return;
    }
    if (this.jobs.has(job.id)) this.finish(job);
  }

  /**
   * Marks the questions a fact answers taught or dismissed on Meko's Context Gaps, in the session's
   * conversation, each only while it's still open: one a person taught or dismissed while the step
   * ran keeps their mark, and one asked again and answered since is off the list. Nothing once the
   * step has closed (its time ran out while the write was on its way), or when Meko doesn't answer:
   * its counts are only what Meko took.
   */
  private async markQuestions(job: Job, questions: { key: string }[], mark: QuestionMark & { state: 'taught' | 'dismissed' }): Promise<void> {
    if (!questions.length || !this.jobs.has(job.id)) return;
    const open = new Set(((await this.core.mekoQuestions.openQuestions(job.datapack.id, null)) ?? []).map((q) => q.key));
    for (const q of questions) {
      if (!open.has(q.key) || !this.jobs.has(job.id)) continue;
      open.delete(q.key);
      if (!(await this.core.mekoQuestions.mark(job.datapack.id, q.key, mark, job.conversation))) continue;
      if (mark.state === 'taught') job.taught++;
      else job.dismissed++;
    }
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
    run(this.db, `UPDATE meko_learns SET ended_at = ?, outcome = ?, failed_step = ?, saves = ?, updates = ?, skips = ?, taught = ?, dismissed = ?,
                  lookups = ?, errors = ?, sent_chars = ? WHERE id = ? AND ended_at IS NULL`,
      Date.now(), outcome, failedStep, job.saves, job.updates, job.skips, job.taught, job.dismissed, c?.lookups ?? 0, c?.errors ?? 0, c?.sent ?? 0, job.id);
    this.core.changed();
    this.core.lifecycle.afterLearn(job.sessionId);
  }

  private finish(job: Job): void {
    this.close(job, 'done', null);
    const text = job.facts.length ? learnSummary(job.saves, job.updates, job.datapack.name, job.taught, job.dismissed) : `Nothing new to save to ${job.datapack.name}`;
    this.core.sessions.event(job.sessionId, 'learned', text,
      { saves: job.saves, updates: job.updates, skips: job.skips, candidates: job.facts.length, taught: job.taught, dismissed: job.dismissed });
    this.core.sessions.mod.queueToast(job.sessionId, `${text}.`);
  }

  private fail(job: Job, step: string, reason: string): void {
    this.close(job, 'failed', step);
    const saved = job.saves + job.updates
      ? ` (${learnSummary(job.saves, job.updates, job.datapack.name, job.taught, job.dismissed).replace(/^S/, 's')} first)` : '';
    // Save learnings can be tried again; at Merge & Close or Discard the session is closing.
    const again = job.trigger === 'manual' ? ' Try Save learnings again.' : '';
    const text = `AgentK couldn't save what this session learned: ${reason}${saved}.${again}`;
    this.core.sessions.event(job.sessionId, 'learned', text);
    this.core.sessions.mod.queueToast(job.sessionId, text);
    this.core.log.warn(`the learn step for ${job.sessionId} failed at ${step}`, reason);
  }

  // ---- as a typed turn (no mod) ----

  private async startTurn(row: SessionRow, trigger: LearnTrigger, dp: DatapackRef): Promise<void> {
    const id = randomUUID();
    this.insert(row.id, id, trigger, 'turn', null);
    try {
      // Claude sees its own earlier saves in the transcript, but not what a step through the mod saved.
      await this.core.sessions.submit(row.id, this.core.sessions.prompt(row, 'learn') + coveredText(this.covered(row.id, dp.id)));
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
    run(this.db, "UPDATE meko_learns SET ended_at = ?, outcome = 'interrupted', saves = ?, updates = ?, skips = ?, taught = ?, dismissed = ? WHERE id = ?",
      Date.now(), job?.saves ?? 0, job?.updates ?? 0, job?.skips ?? 0, job?.taught ?? 0, job?.dismissed ?? 0, open.id);
    this.core.lifecycle.afterLearn(sessionId);
  }

  /**
   * A learn session left Explore: moved on (from then on it's a task like any other), or closed. Its
   * searches while it seeded checked before each save into the datapack it was filling, so finding
   * nothing was expected, not something Meko should be taught: Meko forgets them as questions (its
   * Context Gaps). One that found something already answered another session's question there. In
   * the background, never failing the move or the close; nothing with Meko off or not answering.
   */
  seeded(row: SessionRow): void {
    const dp = this.core.mekoStatus.configured ? sessionDatapack(this.db, row) : null;
    if (row.purpose !== 'learn' || !dp) return;
    // Only its first time in Explore was seeding: moved back there later, it's a task like any other,
    // and its searches since are questions. With one Explore ever, every ask before now was made in it.
    if ((get<{ n: number }>(this.db, "SELECT COUNT(*) AS n FROM session_stages WHERE session_id = ? AND stage = 'explore'", row.id)?.n ?? 0) > 1) return;
    const before = Date.now();
    void (async () => {
      const conversation = await this.core.sessions.conversationId(row, dp);
      if (!(await this.core.mekoQuestions.forgetAsks(dp.id, conversation, before))) return;
      this.core.log.info(`Meko forgot ${row.id}'s searches while it seeded ${dp.name}`);
    })().catch((e: unknown) => this.core.log.warn(`forgetting ${row.id}'s searches while it seeded failed`, errMsg(e)));
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

/** The timeline's line for a Merge & Close or Discard the user asked to skip the learn step. */
export const SKIPPED_TEXT = "Didn't save what this session learned: you asked to skip it";

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
