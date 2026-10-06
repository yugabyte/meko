import { randomUUID } from 'node:crypto';

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

/** What a poll answers: the prompts to submit, or null for none. */
export type PollAnswer = { prompts: { id: string; text: string }[] } | null;

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
  /** Answered to a poll; waiting for the mod's ack. */
  handed: boolean;
  timer: NodeJS.Timeout;
  resolve(): void;
  reject(e: Error): void;
}

interface Link {
  /** When the mod last said hello or polled; 0 before it ever has. */
  seenAt: number;
  /** The poll held open now, if any. */
  waiter: { answer(a: PollAnswer): void } | null;
  queue: Pending[];
  /** The prompts the mod took, by id: their text and when, kept TAKEN_KEEP_MS. */
  taken: Map<string, { text: string; at: number }>;
}

/**
 * Each session's link to AgentK's mod running in its Claude process (src/mod/agentk): whether the
 * mod is there, and the prompts on their way to it. Kept in memory only: after a server restart the
 * mod's next poll brings the link back, and the prompts AgentK queues are in the database anyway.
 *
 * A prompt goes to the mod only while the link is live, and is submitted by the mod only once the
 * server answered its `accepted` ack with 204, which is also when the send resolves. A prompt not
 * acked within ACCEPT_MS (or reset) is given up on: the send fails, as a failed paste does, the
 * caller keeps the prompt, and a late ack is answered 410 so the mod never submits it. So a prompt
 * is never run twice, and never both handed to the mod and typed into the terminal.
 */
export class ModLinks {
  private links = new Map<string, Link>();
  /** False while the hook socket is closing (a rebind or a stop): polls aren't held then. */
  accepting = true;

  constructor(
    private onDropped: (session: string, dropped: DroppedPrompt) => void = () => undefined,
    private now: () => number = Date.now,
  ) {}

  private link(session: string): Link {
    let l = this.links.get(session);
    if (!l) this.links.set(session, (l = { seenAt: 0, waiter: null, queue: [], taken: new Map() }));
    return l;
  }

  /** True while the session's mod is polling: prompts for it go through the mod. */
  live(session: string): boolean {
    const l = this.links.get(session);
    return !!l && (!!l.waiter || (l.seenAt > 0 && this.now() - l.seenAt < LINK_STALE_MS));
  }

  /** The mod loaded in the session's Claude. True the first time since the session was last launched. */
  hello(session: string): boolean {
    const l = this.link(session);
    const first = l.seenAt === 0;
    l.seenAt = this.now();
    return first;
  }

  /**
   * The mod asks for prompts: answered at once when some are waiting, else held up to POLL_HOLD_MS.
   * A newer poll answers the one before it with nothing (the mod's loop polled again after a failure
   * the server didn't see). `signal` aborts when the mod's connection closes.
   */
  poll(session: string, signal?: AbortSignal): Promise<PollAnswer> {
    const l = this.link(session);
    l.seenAt = this.now();
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

  /** The prompts not handed over yet, marked handed; null when there are none. */
  private hand(l: Link): PollAnswer {
    const fresh = l.queue.filter((p) => !p.handed);
    if (!fresh.length) return null;
    for (const p of fresh) p.handed = true;
    return { prompts: fresh.map((p) => ({ id: p.id, text: p.text })) };
  }

  /**
   * Sends a prompt through the mod. Resolves once the server has told the mod to submit it (Claude
   * runs it when it's next idle); rejects when the mod doesn't ack it within ACCEPT_MS, or the link
   * is reset, and then the mod is told not to submit it.
   */
  send(session: string, text: string): Promise<void> {
    const l = this.link(session);
    return new Promise((resolve, reject) => {
      const p: Pending = {
        id: randomUUID(),
        text,
        handed: false,
        resolve,
        reject,
        timer: setTimeout(() => {
          this.remove(l, p);
          reject(new Error("AgentK's mod in Claude didn't take the prompt in time"));
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
    p.resolve();
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
    for (const p of l.queue) {
      clearTimeout(p.timer);
      p.reject(new Error(why));
    }
  }

  /**
   * The hook socket is about to close (a rebind, or the server stopping): answers every held poll
   * with nothing and holds no new one, so the socket closes at once. `resume` once it's bound again.
   */
  pause(): void {
    this.accepting = false;
    for (const l of this.links.values()) l.waiter?.answer(null);
  }

  resume(): void {
    this.accepting = true;
  }

  /** The server is stopping: pauses polls for good, and fails every prompt on its way. */
  closeAll(): void {
    this.pause();
    for (const s of [...this.links.keys()]) this.reset(s, 'AgentK Local is stopping');
  }
}
