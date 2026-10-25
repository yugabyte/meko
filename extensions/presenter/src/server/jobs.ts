// Claude jobs on a deck: drafting the story map, building scenes, and asks. Each deck has one Claude
// session that its jobs continue one at a time; a build runs its scenes side by side on forks of it.
// A map or build that touches nothing the author edited by hand lands at once (Undo takes it back);
// an ask, or anything that would overwrite hand edits, comes back as a change set to review.

import { randomUUID } from 'node:crypto';
import { existsSync, mkdirSync, readFileSync, realpathSync, statSync, writeFileSync } from 'node:fs';
import { join, resolve, sep } from 'node:path';
import { ANIM_PREFIX } from '../shared/datapacks.js';
import { ASSET_NAME, assetFits, type Deck } from '../shared/deck.js';
import { describeOp, tryOps, type Op } from '../shared/ops.js';
import type { ChangeSet, DeckState, Job, JobRequest, ServerEvent } from '../shared/types.js';
import { activityOf, claudeArgs, MEKO_KEY_ENV, notSignedIn, sessionMissing, type ClaudeRunner, type RunResult, type SessionArgs } from './claude.js';
import { HttpError, type DeckStore } from './decks.js';
import type { MekoCreds, MekoPort } from './meko.js';
import { withLocalBin } from './paths.js';
import { ANSWER_SCHEMA, askMessage, buildMessage, mapMessage, readAnswer, retryMessage, SYSTEM_PROMPT, type DatapackRef } from './prompts.js';

/** Scenes a build runs at once. */
const FORKS = 3;
const MIN = 60_000;
const TIMEOUT = { map: 10 * MIN, build: 10 * MIN, askSmall: 3 * MIN, askLarge: 10 * MIN };
/** Activity lines kept per job. */
const ACTIVITY_MAX = 200;

export interface JobsDeps {
  decks: DeckStore;
  meko: MekoPort;
  /** The Claude Code command, with any arguments before Presenter's (tests run a stub with node). */
  claude: string[];
  run: ClaudeRunner;
  /** Meko's MCP URL and key for Claude's config, or null when there's no Meko. */
  mekoCreds: () => Promise<MekoCreds | null>;
  emit: (e: ServerEvent) => void;
}

interface Running extends Job {
  abort: AbortController;
}

interface SessionFile {
  id: string;
  started: boolean;
}

export class Jobs {
  private jobs = new Map<string, Running>();
  private sets = new Map<string, ChangeSet>();
  private chains = new Map<string, Promise<void>>();

  constructor(private d: JobsDeps) {}

  private pub(j: Running): Job {
    const { abort: _a, ...job } = j;
    return job;
  }

  private changed(j: Running): void {
    this.d.emit({ type: 'job', job: this.pub(j) });
  }

  list(deckId: string): Job[] {
    return [...this.jobs.values()].filter((j) => j.deckId === deckId).map((j) => this.pub(j)).sort((a, b) => b.createdAt - a.createdAt);
  }

  changeSets(deckId: string): ChangeSet[] {
    return [...this.sets.values()].filter((c) => c.deckId === deckId && c.status === 'open').sort((a, b) => b.createdAt - a.createdAt);
  }

  /** Queues a job on the deck's session: it runs after the deck's earlier jobs. */
  async start(deckId: string, req: JobRequest): Promise<Job> {
    const deck = await this.d.decks.deck(deckId);
    if (req.kind === 'ask' && !req.instruction.trim()) throw new HttpError(400, 'Say what to change.');
    const title = req.kind === 'map' ? (req.note ? 'Redraw the story map' : 'Draft the story map')
      : req.kind === 'build' ? `Build ${req.scenes?.length === 1 ? `"${deck.scenes[req.scenes[0]!]?.title ?? req.scenes[0]}"` : 'slides'}`
        : req.instruction.trim().slice(0, 120);
    const j: Running = {
      id: randomUUID(), deckId, kind: req.kind, title, status: 'queued', activity: [], error: null, changeSet: null,
      createdAt: Date.now(), endedAt: null, abort: new AbortController(),
    };
    this.jobs.set(j.id, j);
    this.changed(j);
    const prev = this.chains.get(deckId) ?? Promise.resolve();
    const next = prev.then(() => this.run(j, req)).catch(() => undefined);
    this.chains.set(deckId, next);
    return this.pub(j);
  }

  cancel(id: string): Job {
    const j = this.jobs.get(id);
    if (!j) throw new HttpError(404, 'There\'s no such job.');
    if (j.status === 'queued' || j.status === 'running') {
      j.abort.abort();
      if (j.status === 'queued') this.end(j, 'cancelled', null);
    }
    return this.pub(j);
  }

  /** Cancels every job, as the server stops: Claude's runs end with it rather than run on unseen. */
  cancelAll(): void {
    for (const j of this.jobs.values()) if (j.status === 'queued' || j.status === 'running') this.cancel(j.id);
  }

  /** Applies the ticked operations of a change set to the deck as it is now. */
  async apply(id: string, take: number[]): Promise<DeckState> {
    const c = this.sets.get(id);
    if (!c || c.status !== 'open') throw new HttpError(404, 'That change set is gone.');
    const ops = take.filter((i) => Number.isInteger(i) && i >= 0 && i < c.ops.length).sort((a, b) => a - b).map((i) => c.ops[i]!);
    if (!ops.length) throw new HttpError(400, 'Tick at least one change to apply, or discard the set.');
    const state = await this.d.decks.apply(c.deckId, ops);
    c.status = 'applied';
    this.d.emit({ type: 'changeset', changeSet: c });
    return state;
  }

  discard(id: string): ChangeSet {
    const c = this.sets.get(id);
    if (!c) throw new HttpError(404, 'That change set is gone.');
    c.status = 'discarded';
    this.d.emit({ type: 'changeset', changeSet: c });
    return c;
  }

  /** Forgets the deck's Claude session: the next job starts a new one. */
  freshSession(deckId: string): void {
    this.writeSession(deckId, { id: randomUUID(), started: false });
  }

  // ---- running ----

  private workDir(deckId: string): string {
    const dir = join(this.d.decks.dir(deckId), 'work');
    mkdirSync(join(dir, 'assets'), { recursive: true, mode: 0o700 });
    return dir;
  }

  private session(deckId: string): SessionFile {
    const f = join(this.d.decks.dir(deckId), 'session.json');
    try {
      const s = JSON.parse(readFileSync(f, 'utf8')) as SessionFile;
      if (typeof s.id === 'string' && typeof s.started === 'boolean') return s;
    } catch {
      /* a new one below */
    }
    const s = { id: randomUUID(), started: false };
    this.writeSession(deckId, s);
    return s;
  }

  private writeSession(deckId: string, s: SessionFile): void {
    mkdirSync(this.d.decks.dir(deckId), { recursive: true, mode: 0o700 });
    writeFileSync(join(this.d.decks.dir(deckId), 'session.json'), JSON.stringify(s), { mode: 0o600 });
  }

  private note(j: Running, line: string): void {
    j.activity = [...j.activity, line].slice(-ACTIVITY_MAX);
    this.changed(j);
  }

  private end(j: Running, status: Job['status'], error: string | null): void {
    if (j.endedAt) return;
    j.status = status;
    j.error = error;
    j.endedAt = Date.now();
    this.changed(j);
  }

  private async packs(deck: Deck): Promise<{ content: DatapackRef[]; animations: DatapackRef[] }> {
    const rows = await this.d.meko.listDatapacks().catch(() => []);
    const ref = (name: string) => ({ id: rows.find((r) => r.name === name)?.id ?? '', name });
    return {
      content: deck.sources.content.map(ref),
      animations: [...new Set([...deck.sources.animations, ...rows.filter((r) => r.name.startsWith(ANIM_PREFIX)).map((r) => r.name)])].map(ref),
    };
  }

  /**
   * One Claude run. `deckSession` says the session is the deck's own (a map's or an ask's): it's
   * recorded as started as soon as Claude names it, so a run cut short is resumed next time, and a
   * new one is started when the deck's isn't on this computer. A build's scenes and their retries run
   * on sessions of their own, which never replace the deck's.
   */
  private async claude(j: Running, session: SessionArgs | null, message: string, timeoutMs: number, prefix = '', deckSession = true): Promise<RunResult> {
    const creds = await this.d.mekoCreds();
    // The PATH the health check looks in (~/.local/bin, where Claude Code's installer puts claude).
    const env = { ...process.env, PATH: withLocalBin(process.env.PATH ?? ''), ...(creds ? { [MEKO_KEY_ENV]: creds.key } : {}) };
    const go = (s: SessionArgs | null) => {
      let noted = !(deckSession && s && !s.fork);
      return this.d.run(this.d.claude, claudeArgs({ session: s, schema: ANSWER_SCHEMA, systemPrompt: SYSTEM_PROMPT, mekoUrl: creds?.url ?? null }), {
        cwd: this.workDir(j.deckId), input: message, env, timeoutMs, signal: j.abort.signal,
        onEvent: (e) => {
          if (!noted && typeof e.session_id === 'string') {
            noted = true;
            this.writeSession(j.deckId, { id: s!.id, started: true });
          }
          activityOf(e).forEach((a) => this.note(j, prefix + a));
        },
      });
    };
    let r = await go(session);
    if (deckSession && session && !session.fork && session.started && sessionMissing(r.error)) {
      const fresh = { id: randomUUID(), started: false };
      this.writeSession(j.deckId, fresh);
      this.note(j, 'the deck\'s Claude session isn\'t on this computer: starting a new one');
      r = await go(fresh);
      session.id = fresh.id;
    }
    if (deckSession && session && !session.fork && (r.sessionId || r.code === 0)) this.writeSession(j.deckId, { id: session.id, started: true });
    return r;
  }

  private failure(r: RunResult): string | null {
    if (r.cancelled) return 'Cancelled.';
    if (r.spawnError) return `Claude Code couldn't be run (${r.spawnError}). Install it, and sign in with claude.`;
    if (r.timedOut) return 'Claude took too long and was stopped. Try again, or ask for less at once.';
    if (notSignedIn(r.error)) return 'Claude Code isn\'t signed in. Run claude in a terminal and log in, then try again.';
    if (r.error) return `Claude Code failed: ${r.error.slice(0, 300)}`;
    return null;
  }

  /**
   * Claude's answer as operations that apply to the deck as it is now: its files kept as assets
   * first. Asks once more, on the same session, when they don't apply.
   */
  private async answerOps(j: Running, first: RunResult, again: (msg: string) => Promise<RunResult>): Promise<{ ops: Op[]; summary: string }> {
    let r = first;
    for (let attempt = 0; ; attempt++) {
      const fail = this.failure(r);
      if (fail) throw new Error(fail);
      const a = readAnswer(r.answer);
      let problems: string[];
      if (typeof a === 'string') problems = [a];
      else if (a.ops.some((o) => (o as { op?: unknown } | null)?.op === 'addAsset')) {
        // An asset needs its bytes here: Presenter adds the files Claude lists, and only those.
        problems = ['Don\'t use addAsset: list the files you made in "files", and Presenter adds them.'];
      } else {
        try {
          const deck = await this.d.decks.deck(j.deckId);
          const ops = [...this.keepFiles(j.deckId, deck, a.files), ...(a.ops as Op[])];
          const t = tryOps(deck, ops);
          if (t.ok) return { ops, summary: a.summary };
          problems = t.problems;
        } catch (e) {
          problems = [(e as Error).message];
        }
      }
      if (attempt > 0) throw new Error(`Claude's changes would break the deck: ${problems.slice(0, 3).join(' ')}`);
      this.note(j, 'the answer didn\'t fit the deck: asking again');
      r = await again(retryMessage(problems));
    }
  }

  /** The files Claude made, kept as assets: only from its folder, within the size limits. */
  private keepFiles(deckId: string, deck: Deck, files: { name: string; path: string; type: string }[]): Op[] {
    const root = realpathSync(this.workDir(deckId));
    const ops: Op[] = [];
    for (const f of files) {
      if (!ASSET_NAME.test(f.name)) throw new Error(`File name "${f.name}" isn't allowed: use letters, digits, dots, dashes and underscores.`);
      const abs = resolve(root, f.path);
      if (!existsSync(abs)) throw new Error(`The file ${f.path} isn't there.`);
      const real = realpathSync(abs);
      if (!real.startsWith(root + sep) || !statSync(real).isFile()) throw new Error(`The file ${f.path} isn't in the deck's folder.`);
      const bytes = new Uint8Array(readFileSync(real));
      const why = assetFits(deck, bytes.length, f.name);
      if (why) throw new Error(`${f.name}: ${why}`);
      const hash = this.d.decks.pieces.keep(bytes);
      ops.push({ op: 'addAsset', name: f.name, asset: { hash, size: bytes.length, type: f.type || 'application/octet-stream' } });
    }
    return ops;
  }

  /** Applies at once when nothing hand-edited is touched (and it isn't an ask), else makes a change set. */
  private async land(j: Running, ops: Op[], summary: string, review: boolean): Promise<void> {
    if (j.abort.signal.aborted) throw new Error('Cancelled.');
    const deck = await this.d.decks.deck(j.deckId);
    if (!review && !ops.some((o) => describeOp(deck, o).handEdited)) {
      await this.d.decks.apply(j.deckId, ops);
      this.note(j, summary);
      return;
    }
    const c: ChangeSet = { id: randomUUID(), deckId: j.deckId, jobId: j.id, title: j.title, summary, ops, createdAt: Date.now(), status: 'open' };
    this.sets.set(c.id, c);
    j.changeSet = c.id;
    this.d.emit({ type: 'changeset', changeSet: c });
  }

  private async run(j: Running, req: JobRequest): Promise<void> {
    if (j.abort.signal.aborted) return;
    j.status = 'running';
    this.changed(j);
    try {
      const deck = await this.d.decks.deck(j.deckId);
      const packs = await this.packs(deck);
      if (req.kind === 'build') {
        await this.build(j, deck, packs, req.scenes);
      } else {
        const session = this.session(j.deckId);
        const s: SessionArgs = { id: session.id, started: session.started };
        const message = req.kind === 'map' ? mapMessage(deck, packs, req.note) : askMessage(deck, packs, req.instruction, req.scope);
        const timeout = req.kind === 'map' ? TIMEOUT.map : req.scope.kind === 'block' || req.scope.kind === 'slide' ? TIMEOUT.askSmall : TIMEOUT.askLarge;
        const first = await this.claude(j, s, message, timeout);
        const { ops, summary } = await this.answerOps(j, first, (msg) => this.claude(j, { id: s.id, started: true }, msg, timeout));
        await this.land(j, ops, summary, req.kind === 'ask');
      }
      this.end(j, 'done', null);
    } catch (e) {
      this.end(j, j.abort.signal.aborted ? 'cancelled' : 'failed', (e as Error).message);
    }
  }

  private async build(j: Running, deck: Deck, packs: { content: DatapackRef[]; animations: DatapackRef[] }, only?: string[]): Promise<void> {
    const scenes = (only?.length ? only : Object.keys(deck.scenes)).filter((id) => deck.scenes[id]);
    if (!scenes.length) throw new Error('There are no scenes to build. Draft the story map first.');
    const main = this.session(j.deckId);
    const failed: string[] = [];
    let next = 0;
    const worker = async () => {
      while (next < scenes.length && !j.abort.signal.aborted) {
        const id = scenes[next++]!;
        const title = deck.scenes[id]!.title;
        const prefix = `${title}: `;
        try {
          const now = await this.d.decks.deck(j.deckId);
          if (!now.scenes[id]) continue;
          // Forks start from what the deck's session already read; without one, a session of the
          // scene's own. Either way the retry below resumes it, and the deck's session stays as it is.
          const message = buildMessage(now, packs, id);
          const own = (): SessionArgs => ({ id: randomUUID(), started: false });
          let first = await this.claude(j, main.started ? { id: main.id, started: true, fork: true } : own(), message, TIMEOUT.build, prefix, false);
          if (main.started && sessionMissing(first.error)) {
            // The deck's session isn't on this computer any more (Claude Code clears old ones): the
            // deck's next job starts a new one, and this scene runs on a session of its own.
            if (this.session(j.deckId).id === main.id) this.writeSession(j.deckId, { id: randomUUID(), started: false });
            first = await this.claude(j, own(), message, TIMEOUT.build, prefix, false);
          }
          const { ops, summary } = await this.answerOps(j, first, (msg) =>
            this.claude(j, first.sessionId ? { id: first.sessionId, started: true } : null, msg, TIMEOUT.build, prefix, false));
          await this.land(j, ops, prefix + summary, false);
        } catch (e) {
          failed.push(title);
          this.note(j, `${prefix}${(e as Error).message}`);
        }
      }
    };
    await Promise.all(Array.from({ length: Math.min(FORKS, scenes.length) }, worker));
    if (j.abort.signal.aborted) throw new Error('Cancelled.');
    if (failed.length) throw new Error(`${failed.length === 1 ? 'A scene' : `${failed.length} scenes`} didn't build: ${failed.join(', ')}. Build ${failed.length === 1 ? 'it' : 'them'} again.`);
  }
}
