import { randomUUID } from 'node:crypto';
import type { AskQuestion, AskRequest } from '../shared/types.js';

/** How long the server holds a mod's poll open while it has nothing to send. */
export const POLL_HOLD_MS = 20_000;
/**
 * A mod that isn't polling now and hasn't for this long is gone (Claude exited, the module failed).
 * Its loop polls again within milliseconds of each answer, or 3 seconds after a failed poll.
 */
export const LINK_STALE_MS = 10_000;
/** How long a prompt handed to the mod has to be acked before the send fails. */
export const ACCEPT_MS = 5_000;
/** How long the text of a prompt the mod took is kept, to name it if another plugin refuses it. */
const TAKEN_KEEP_MS = 10 * 60 * 1000;

/** How often links are checked for going stale, and asks for being abandoned. */
const SWEEP_MS = 2_000;
/** The most Meko calls in flight kept per session (Claude runs a few in parallel at most). */
const MEKO_MAX = 20;

/**
 * A part of a learn step for the mod (learn.ts): `extract`, fork the session with `prompt`; `check`,
 * run `prompt` on `model`. Either way it posts the reply to /mod/learn, which answers at once.
 */
export interface LearnJob {
  id: string;
  kind: 'extract' | 'check';
  prompt: string;
  model?: string;
}

/**
 * What a poll answers: the prompts to submit, a part of a learn step to run, and a line for the mod
 * to show (what a learn step saved); null for none of them.
 */
export type PollAnswer = { prompts: { id: string; text: string; hidden?: true }[]; learn?: LearnJob; toast?: string } | null;

/** The mod protocol that runs learn steps (its /mod/hello and every poll say which it speaks). */
export const LEARN_PROTOCOL = 2;

/** A prompt another plugin in Claude refused after the mod took it. */
export interface DroppedPrompt {
  /** Its text, when the server still has it. */
  text: string | null;
  reason: string;
  /** The mod put the text in Claude's prompt box (it was empty), for the user to send. */
  filled: boolean;
}

interface Pending {
  id: string;
  text: string;
  /**
   * A workshop host's events: the mod hides a bare "OK" in its turn. The prompt's own row is hidden by
   * Kode's view mod, by its first line (KODE_HIDDEN_PROMPT), not by this.
   */
  hidden: boolean;
  /** Answered to a poll; waiting for the mod's ack. */
  handed: boolean;
  timer: NodeJS.Timeout;
  resolve(id: string): void;
  reject(e: Error): void;
}

/**
 * What the mod's held `/mod/ask` gets: the user's answers by question index, null when the hold
 * ended first (the mod asks again), or `gone` when the ask was dropped (the mod leaves it to the
 * terminal).
 */
export type AskReply = { answers: Record<number, string> } | null | 'gone';

/** Questions Claude asks with AskUserQuestion, forwarded by the mod while the tool call waits. */
interface Ask {
  /** The tool call's id, as the mod sends it. */
  id: string;
  questions: AskQuestion[];
  /** When the mod last asked; it asks again after each hold while the call waits. */
  seenAt: number;
  waiter: { answer(a: AskReply): void } | null;
  /** The user's answers, kept until the mod has them (it may have to ask again to get them). */
  answers: Record<number, string> | null;
}

interface Link {
  /** When the mod last said hello or polled; 0 before it ever has. */
  seenAt: number;
  /**
   * The protocol the mod speaks (1 before learn steps), from its /mod/hello and from every poll
   * (after a server restart, only from polls); 0 until either names one.
   */
  protocol: number;
  /** A part of a learn step waiting for the mod, until a poll hands it over. */
  learn: LearnJob | null;
  /** A line for the mod to show, until a poll hands it over. */
  toast: string | null;
  /** Whether the link was live when that was last reported (onChange). */
  reported: boolean;
  /** The questions Claude is asking through the mod, if any: one AskUserQuestion runs at a time. */
  ask: Ask | null;
  /** Claude's Meko tool calls running now, by tool call id. */
  meko: Set<string>;
  /**
   * Prompts the mod submitted that Claude holds until its turn ends, by id, oldest first: added when
   * a prompt is sent mid-turn, and replaced by what the mod reports (`/mod/poll`, `/mod/held`).
   */
  held: Set<string>;
  /** The poll held open now, if any. */
  waiter: { answer(a: PollAnswer): void } | null;
  queue: Pending[];
  /** The prompts the mod took, by id: their text and when, kept TAKEN_KEEP_MS. */
  taken: Map<string, { text: string; at: number }>;
}

/**
 * Each session's link to Kode's mod running in its Claude process (src/mod/agentk): whether the
 * mod is there, and the prompts on their way to it. Kept in memory only: after a server restart the
 * mod's next poll brings the link back, and the prompts Kode queues are in the database anyway.
 *
 * A prompt goes to the mod only while the link is live, and is submitted by the mod only once the
 * server answered its `accepted` ack with 204, which is also when the send resolves. A prompt not
 * acked within ACCEPT_MS (or reset) is given up on: the send fails, as a failed paste does, the
 * caller keeps the prompt, and a late ack is answered 410 so the mod never submits it. So a prompt
 * is never run twice, and never both handed to the mod and typed into the terminal.
 *
 * The mod also forwards Claude's AskUserQuestion calls (`openAsk`, then `waitAsk` held like a poll
 * and asked again after each hold) while Claude's own dialog is open in the terminal: whichever
 * answers first wins. An ask lives only while the mod keeps asking: it's dropped when the mod says
 * it has its answer, when it stops asking for LINK_STALE_MS, at the turn's end, and on a reset; and
 * since the mod sends the questions each time, a server restart brings it back with the next ask.
 *
 * It reports Claude's Meko tool calls too (`mekoCall`), each by its tool call id, before the call
 * runs and once it's over; they're forgotten at the turn's end (`endMeko`) and on a reset, for an
 * end that never got through.
 */
export class ModLinks {
  private links = new Map<string, Link>();
  /** False while the hook socket is closing (a rebind or a stop): polls aren't held then. */
  accepting = true;
  private sweeper: NodeJS.Timeout;

  /**
   * `onChange` is told when a session's link goes live or stale, when the questions it's asking
   * change (`asking`), and when Claude starts or stops calling Meko (`callingMeko`).
   */
  constructor(
    private onDropped: (session: string, dropped: DroppedPrompt) => void = () => undefined,
    private now: () => number = Date.now,
    private onChange: (session: string) => void = () => undefined,
  ) {
    this.sweeper = setInterval(() => this.sweep(), SWEEP_MS);
    this.sweeper.unref?.();
  }

  private link(session: string): Link {
    let l = this.links.get(session);
    if (!l) this.links.set(session, (l = { seenAt: 0, protocol: 0, learn: null, toast: null, reported: false, ask: null, meko: new Set(), held: new Set(), waiter: null, queue: [], taken: new Map() }));
    return l;
  }

  /** The mod was heard from: reports the link live if it wasn't. */
  private seen(session: string, l: Link): void {
    l.seenAt = this.now();
    if (l.reported) return;
    l.reported = true;
    this.onChange(session);
  }

  /** Reports links that went stale, and drops asks the mod stopped asking (the call ended without a word). */
  private sweep(): void {
    for (const [session, l] of this.links) {
      if (l.ask && !l.ask.waiter && this.now() - l.ask.seenAt >= LINK_STALE_MS) this.endAsk(session, l.ask.id);
      if (l.reported && !this.live(session)) {
        l.reported = false;
        this.onChange(session);
      }
    }
  }

  /** True while the session's mod is polling: prompts for it go through the mod. */
  live(session: string): boolean {
    const l = this.links.get(session);
    return !!l && (!!l.waiter || (l.seenAt > 0 && this.now() - l.seenAt < LINK_STALE_MS));
  }

  /** The mod loaded in the session's Claude. True the first time since the session was last launched. */
  hello(session: string, protocol?: unknown): boolean {
    const l = this.link(session);
    const first = l.seenAt === 0;
    l.protocol = typeof protocol === 'number' && Number.isInteger(protocol) ? protocol : 1;
    this.seen(session, l);
    return first;
  }

  /**
   * The protocol a poll names. A mod says hello once per load, so after a server restart only its
   * polls say what it speaks; a mod from before learn steps names none and stays at what it was.
   */
  reportProtocol(session: string, protocol: unknown): void {
    if (typeof protocol === 'number' && Number.isInteger(protocol)) this.link(session).protocol = protocol;
  }

  /** True while the session's mod is live and runs learn steps (a mod loaded before them doesn't). */
  learnReady(session: string): boolean {
    return this.live(session) && (this.links.get(session)?.protocol ?? 0) >= LEARN_PROTOCOL;
  }

  /** Hands a part of a learn step to the mod with its next poll (at once when one is held). */
  queueLearn(session: string, job: LearnJob): void {
    this.link(session).learn = job;
    this.handNow(session);
  }

  /** Takes back a part of learn step `id` the mod hasn't polled for yet: the step is over. */
  withdrawLearn(session: string, id: string): void {
    const l = this.links.get(session);
    if (l?.learn?.id === id) l.learn = null;
  }

  /** Has the mod show a line (what a learn step saved) with its next poll. */
  queueToast(session: string, text: string): void {
    const l = this.links.get(session);
    if (!l) return;
    l.toast = text;
    this.handNow(session);
  }

  private handNow(session: string): void {
    const l = this.links.get(session);
    if (!l?.waiter) return;
    const a = this.hand(l);
    if (a) l.waiter.answer(a);
  }

  /**
   * The mod asks for prompts: answered at once when some are waiting, else held up to POLL_HOLD_MS.
   * A newer poll answers the one before it with nothing (the mod's loop polled again after a failure
   * the server didn't see). `signal` aborts when the mod's connection closes.
   */
  poll(session: string, signal?: AbortSignal): Promise<PollAnswer> {
    const l = this.link(session);
    this.seen(session, l);
    l.waiter?.answer(null);
    const ready = this.hand(l);
    if (ready || !this.accepting) return Promise.resolve(ready);
    return new Promise((resolve) => {
      const done = (a: PollAnswer) => {
        clearTimeout(timer);
        signal?.removeEventListener('abort', gone);
        if (l.waiter === waiter) l.waiter = null;
        l.seenAt = this.now();
        resolve(a);
      };
      const gone = () => done(null);
      const waiter = { answer: done };
      const timer = setTimeout(() => done(null), POLL_HOLD_MS);
      timer.unref?.();
      l.waiter = waiter;
      if (signal?.aborted) gone();
      else signal?.addEventListener('abort', gone, { once: true });
    });
  }

  /** The prompts not handed over yet, marked handed, a learn step's part and a line waiting; null when there are none. */
  private hand(l: Link): PollAnswer {
    const fresh = l.queue.filter((p) => !p.handed);
    const { learn, toast } = l;
    if (!fresh.length && !learn && !toast) return null;
    for (const p of fresh) p.handed = true;
    l.learn = null;
    l.toast = null;
    return { prompts: fresh.map((p) => ({ id: p.id, text: p.text, ...(p.hidden ? { hidden: true as const } : {}) })), ...(learn ? { learn } : {}), ...(toast ? { toast } : {}) };
  }

  /**
   * Sends a prompt through the mod. Resolves with its id once the server has told the mod to submit
   * it (Claude runs it when it's next idle); rejects when the mod doesn't ack it within ACCEPT_MS, or the link
   * is reset, and then the mod is told not to submit it. For a `hidden` one (a workshop host's events)
   * the mod draws a bare "OK" in its turn as nothing; Kode's view mod hides the prompt's own row by
   * its first line (KODE_HIDDEN_PROMPT).
   */
  send(session: string, text: string, hidden = false): Promise<string> {
    const l = this.link(session);
    return new Promise((resolve, reject) => {
      const p: Pending = {
        id: randomUUID(),
        text,
        hidden,
        handed: false,
        resolve,
        reject,
        timer: setTimeout(() => {
          this.remove(l, p);
          reject(new Error("Kode's mod in Claude didn't take the prompt in time"));
        }, ACCEPT_MS),
      };
      p.timer.unref?.();
      l.queue.push(p);
      if (l.waiter) {
        const a = this.hand(l);
        if (a) l.waiter.answer(a);
      }
    });
  }

  private remove(l: Link, p: Pending): void {
    clearTimeout(p.timer);
    l.queue = l.queue.filter((q) => q !== p);
  }

  /**
   * The mod took a prompt it was handed. True when the send is still waiting for it: the send
   * resolves, and the mod submits the prompt. False when it was given up on (timed out, reset) or
   * is unknown: the mod must not submit it.
   */
  accept(session: string, prompt: unknown): boolean {
    const l = this.links.get(session);
    if (!l) return false;
    l.seenAt = this.now();
    const p = l.queue.find((q) => q.id === prompt && q.handed);
    if (!p) return false;
    this.remove(l, p);
    const now = this.now();
    for (const [id, t] of l.taken) if (now - t.at > TAKEN_KEEP_MS) l.taken.delete(id);
    l.taken.set(p.id, { text: p.text, at: now });
    p.resolve(p.id);
    return true;
  }

  /** Another plugin in Claude refused a prompt the mod took (its `prompt.submit` hook dropped it). */
  dropped(session: string, prompt: unknown, reason: unknown, filled: unknown): void {
    const l = this.links.get(session);
    const taken = typeof prompt === 'string' ? l?.taken.get(prompt) : undefined;
    if (taken) l!.taken.delete(prompt as string);
    this.onDropped(session, {
      text: taken?.text ?? null,
      reason: typeof reason === 'string' && reason.trim() ? reason.trim() : 'no reason given',
      filled: filled === true,
    });
  }

  /** The session's Claude was relaunched or ended: forget its mod, failing what was on its way. */
  reset(session: string, why = 'Claude restarted before taking the prompt'): void {
    const l = this.links.get(session);
    if (!l) return;
    this.links.delete(session);
    l.waiter?.answer(null);
    l.ask?.waiter?.answer('gone');
    for (const p of l.queue) {
      clearTimeout(p.timer);
      p.reject(new Error(why));
    }
    if (l.reported || l.ask || l.meko.size) this.onChange(session);
  }

  // ---- prompts Claude holds until its turn ends ----

  /** A prompt sent mid-turn: Claude holds it until the turn ends. */
  hold(session: string, prompt: string): void {
    this.link(session).held.add(prompt);
  }

  /**
   * What the mod says it still holds (a list of prompt ids): it drops a prompt once its turn
   * starts, or another plugin refused it. Ignored when it isn't a list of ids.
   */
  reportHeld(session: string, ids: unknown): void {
    if (!Array.isArray(ids) || ids.some((i) => typeof i !== 'string')) return;
    const l = this.links.get(session);
    if (l) l.held = new Set(ids as string[]);
  }

  /** A prompt started its turn: the oldest held one, for a mod that doesn't report what it holds. */
  started(session: string): void {
    const l = this.links.get(session);
    const first = l?.held.values().next();
    if (l && first && !first.done) l.held.delete(first.value);
  }

  /** How many prompts sent mid-turn Claude still holds. */
  heldCount(session: string): number {
    return this.links.get(session)?.held.size ?? 0;
  }

  // ---- Claude's Meko calls ----

  /** True while Claude runs a Meko tool call, as the mod reports (`/mod/meko`). */
  callingMeko(session: string): boolean {
    return !!this.links.get(session)?.meko.size;
  }

  /**
   * A Meko call started or ended. Reported (onChange) when the first starts and when the last ends;
   * a bad id or state, or a start past MEKO_MAX, is ignored. An end only looks the link up: it can
   * arrive after a reset (Claude exited), and mustn't bring the link back as live. True when it took
   * a new call's start.
   */
  mekoCall(session: string, id: unknown, state: unknown): boolean {
    if (typeof id !== 'string' || !id || id.length > 200) return false;
    if (state === 'end') {
      const l = this.links.get(session);
      if (l?.meko.delete(id) && !l.meko.size) this.onChange(session);
      return false;
    }
    if (state !== 'start') return false;
    const l = this.link(session);
    this.seen(session, l);
    if (l.meko.size >= MEKO_MAX || l.meko.has(id)) return false;
    l.meko.add(id);
    if (l.meko.size === 1) this.onChange(session);
    return true;
  }

  /** The turn ended: no tool call outlives it, so an end the mod never got through is assumed. */
  endMeko(session: string): void {
    const l = this.links.get(session);
    if (!l?.meko.size) return;
    l.meko.clear();
    this.onChange(session);
  }

  // ---- Claude's questions (AskUserQuestion) ----

  /** The questions Claude is asking through the mod, until they're answered (here or in the terminal) or dropped. */
  asking(session: string): AskRequest | null {
    const a = this.links.get(session)?.ask;
    return a && !a.answers ? { id: a.id, questions: a.questions } : null;
  }

  /**
   * The mod forwards an AskUserQuestion call (and again after each hold, with the same id). True
   * when it's new: one asked before it is dropped, since Claude asks one at a time.
   */
  openAsk(session: string, id: string, questions: AskQuestion[]): boolean {
    const l = this.link(session);
    this.seen(session, l);
    if (l.ask?.id === id) {
      l.ask.seenAt = this.now();
      return false;
    }
    l.ask?.waiter?.answer('gone');
    l.ask = { id, questions, seenAt: this.now(), waiter: null, answers: null };
    this.onChange(session);
    return true;
  }

  /**
   * Holds the mod's ask until the user answers it (at once when they already have), POLL_HOLD_MS
   * passes (null: the mod asks again), or the ask is dropped (`gone`). `signal` aborts when the
   * mod's connection closes.
   */
  waitAsk(session: string, id: string, signal?: AbortSignal): Promise<AskReply> {
    const l = this.links.get(session);
    const a = l?.ask;
    if (!l || !a || a.id !== id) return Promise.resolve('gone');
    a.seenAt = this.now();
    if (a.answers) return Promise.resolve({ answers: a.answers });
    a.waiter?.answer(null);
    if (!this.accepting) return Promise.resolve(null);
    return new Promise((resolve) => {
      const done = (r: AskReply) => {
        clearTimeout(timer);
        signal?.removeEventListener('abort', gone);
        if (a.waiter === waiter) a.waiter = null;
        a.seenAt = this.now();
        resolve(r);
      };
      const gone = () => done(null);
      const waiter = { answer: done };
      const timer = setTimeout(() => done(null), POLL_HOLD_MS);
      timer.unref?.();
      a.waiter = waiter;
      if (signal?.aborted) gone();
      else signal?.addEventListener('abort', gone, { once: true });
    });
  }

  /**
   * The user answered the questions Claude is asking. False when they're gone or already answered:
   * the terminal's dialog answered first, or Claude moved on.
   */
  answerAsk(session: string, id: string, answers: Record<number, string>): boolean {
    const a = this.links.get(session)?.ask;
    if (!a || a.id !== id || a.answers) return false;
    a.answers = answers;
    a.waiter?.answer({ answers });
    this.onChange(session);
    return true;
  }

  /**
   * Drops the ask `id` (any, without one): the mod has its answer (from the user or the terminal),
   * the turn ended, or the mod stopped asking. A held ask is told it's gone.
   */
  endAsk(session: string, id?: string): void {
    const l = this.links.get(session);
    const a = l?.ask;
    if (!l || !a || (id !== undefined && a.id !== id)) return;
    l.ask = null;
    a.waiter?.answer('gone');
    if (!a.answers) this.onChange(session);
  }

  /**
   * The hook socket is about to close (a rebind, or the server stopping): answers every held poll
   * with nothing and holds no new one, so the socket closes at once. `resume` once it's bound again.
   */
  pause(): void {
    this.accepting = false;
    for (const l of this.links.values()) {
      l.waiter?.answer(null);
      l.ask?.waiter?.answer(null);
    }
  }

  resume(): void {
    this.accepting = true;
  }

  /** The server is stopping: pauses polls for good, and fails every prompt on its way. */
  closeAll(): void {
    clearInterval(this.sweeper);
    this.pause();
    for (const s of [...this.links.keys()]) this.reset(s, 'Kode is stopping');
  }
}
