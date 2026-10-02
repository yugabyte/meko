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

/** What a poll answers: the prompts to submit, or null for none. */
export type PollAnswer = { prompts: { id: string; text: string }[] } | null;

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
}

/**
 * Each session's link to AgentK's mod running in its Claude process (src/mod/agentk): whether the
 * mod is there, and the prompts on their way to it. Kept in memory only: after a server restart the
 * mod's next poll brings the link back, and the prompts AgentK queues are in the database anyway.
 *
 * A prompt goes to the mod only while the link is live. Once it has been handed over it is never
 * also typed into the terminal: a send that isn't acked in time fails, as a failed paste does, and
 * the caller keeps the prompt for later.
 */
export class ModLinks {
  private links = new Map<string, Link>();
  /** The server is stopping (closeAll): no poll is held from now on. */
  closed = false;

  constructor(
    private onDropped: (session: string, reason: string) => void = () => undefined,
    private now: () => number = Date.now,
  ) {}

  private link(session: string): Link {
    let l = this.links.get(session);
    if (!l) this.links.set(session, (l = { seenAt: 0, waiter: null, queue: [] }));
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
   * A newer poll answers the one before it with nothing (a reloaded module's loop replacing the old
   * one's). `signal` aborts when the mod's connection closes.
   */
  poll(session: string, signal?: AbortSignal): Promise<PollAnswer> {
    const l = this.link(session);
    l.seenAt = this.now();
    l.waiter?.answer(null);
    const ready = this.hand(l);
    if (ready) return Promise.resolve(ready);
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
   * Sends a prompt through the mod. Resolves once the mod says it took it (Claude submits it when
   * it's next idle); rejects when it doesn't say so within ACCEPT_MS, or the link is reset.
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

  /** The mod's word on a prompt: it took it (accepted), or another plugin refused it (dropped). */
  ack(session: string, prompt: unknown, state: unknown, reason?: unknown): void {
    const l = this.links.get(session);
    if (!l) return;
    l.seenAt = this.now();
    if (state === 'dropped') {
      this.onDropped(session, typeof reason === 'string' && reason.trim() ? reason.trim() : 'no reason given');
      return;
    }
    const p = l.queue.find((q) => q.id === prompt);
    if (state !== 'accepted' || !p) return;
    this.remove(l, p);
    p.resolve();
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
   * The server is stopping: answers every held poll, so the hook socket can close at once, and
   * holds no new one (the mod is told to come back later instead).
   */
  closeAll(): void {
    this.closed = true;
    for (const s of [...this.links.keys()]) this.reset(s, 'AgentK Local is stopping');
  }
}
