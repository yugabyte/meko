import type { AppState, EventsClientMessage, ServerEvent } from '../shared/types';
import { WS_ABNORMAL } from './signin';

export type Conn = 'connecting' | 'open' | 'closed';

/** After this long without a message, the socket is asked to prove it's still there. */
export const QUIET_MS = 30_000;
/** How long a ping's answer may take before the socket is given up on. */
export const PROBE_MS = 5_000;
/** How long a new socket may take to open. */
export const CONNECT_MS = 10_000;
/** How long an open socket may take to send its first state, a full snapshot even on a slow link. */
export const STATE_MS = 30_000;
/** Each deadline that passes doubles the next ones, up to 2^MAX_SLOW times. */
export const MAX_SLOW = 3;
/** While an update installs the server restarts into it: a dropped socket retries this often... */
export const UPDATE_RETRY_MS = 1000;
/** ...for this long, then backs off as usual (a new version that won't start takes a while to roll back). */
export const UPDATE_RETRY_FOR_MS = 120_000;
/** How long the connection may be down before the page says so (UPDATE_RETRY_FOR_MS once the server says it's restarting into an update). */
export const LOST_GRACE_MS = 2000;

/** The parts of a browser WebSocket the connection uses. */
export type SocketLike = Pick<WebSocket, 'readyState' | 'onopen' | 'onmessage' | 'onclose' | 'onerror' | 'send' | 'close'>;

export interface EventsHandlers {
  open: () => SocketLike;
  onState: (s: AppState) => void;
  /** What to tell the user: changes only when that changes (see EventsConnection). */
  onConn: (c: Conn) => void;
  onEvent: (ev: Extract<ServerEvent, { type: 'notify' | 'invite' | 'knowledge' }>) => void;
  /** After an `unauthorized` close: resolves true to keep retrying, false when the sign-in is gone. */
  checkSignIn: () => Promise<boolean>;
  /** Close codes that mean the socket was refused for want of a sign-in. */
  unauthorized: readonly number[];
}

/**
 * The /ws/events connection: reconnects with exponential backoff, and doesn't trust a quiet
 * socket. A phone that puts the page in the background (or changes networks) often never hears
 * that the server dropped its socket, so the page would keep showing the old state with nothing
 * coming in. A socket that has been quiet for QUIET_MS, or whose page was just shown again
 * (resume()), is pinged; one that doesn't answer within PROBE_MS is dropped for a new one, which
 * starts with the current state.
 *
 * A browser hands over a message only once all of it has arrived, so on a slow link a ping's answer
 * (queued behind a backlog of snapshots) or a new socket's first state can miss its deadline on a
 * live socket. Each deadline that passes doubles the next ones (up to 2^MAX_SLOW times), so such a
 * link gets the time it needs instead of starting over forever; a ping answered within PROBE_MS
 * shows the link is quick again.
 *
 * A socket dropped while Meko installs an update of Kode (`setUpdateInstalling`, from the job the
 * page follows on Meko: `useMekoUpdate`) retries every UPDATE_RETRY_MS rather than backing off, so
 * the page sees the new version (and reloads into it) as soon as it's up.
 *
 * onConn reports what to tell the user, not each socket's ups and downs: `connecting` until the
 * first socket proves itself, `open` once a socket's state arrives, and `closed` only once the
 * connection has been down for LOST_GRACE_MS. When the server said it's restarting into an update
 * Meko installed (a `restarting` message, sent just before it closes the socket), the wait is
 * UPDATE_RETRY_FOR_MS: the update's own banner says what's happening. A drop before that, while the
 * update still downloads and checks itself, gets the usual wait. Retries report nothing, so the
 * lost-connection banner shows once and stays until the server is back instead of going off for
 * every try.
 */
/** Meko is installing an update of Kode: every connection retries fast while it does. */
let installing = false;

/** Says whether Meko is installing an update of Kode (its job, which `useMekoUpdate` follows). */
export function setUpdateInstalling(on: boolean): void {
  installing = on;
}

export class EventsConnection {
  private ws: SocketLike | null = null;
  private retryTimer: ReturnType<typeof setTimeout> | null = null;
  private quietTimer: ReturnType<typeof setTimeout> | null = null;
  /** Running while a ping, or a new socket's opening or first state, is awaited. */
  private deadline: ReturnType<typeof setTimeout> | null = null;
  /** When the ping that's out was sent, or 0. */
  private probeAt = 0;
  /** How many times the deadlines have been doubled. */
  private slow = 0;
  private attempt = 0;
  /** When the socket was dropped during an update, or 0. */
  private droppedAt = 0;
  /** The server said it's restarting into an update, since its last state. */
  private restarting = false;
  /** Running from a drop until the page says the connection is lost. */
  private lostTimer: ReturnType<typeof setTimeout> | null = null;
  /** What onConn last reported, or null before the first. */
  private shown: Conn | null = null;
  private stopped = false;

  constructor(private readonly h: EventsHandlers) {}

  start(): void {
    this.connect();
  }

  stop(): void {
    this.stopped = true;
    if (this.retryTimer) clearTimeout(this.retryTimer);
    this.retryTimer = null;
    if (this.lostTimer) clearTimeout(this.lostTimer);
    this.lostTimer = null;
    this.drop();
  }

  /**
   * The page was shown again or came back online: a socket waiting to reconnect does so now, and
   * the current one is checked.
   */
  resume(): void {
    if (this.stopped) return;
    if (this.ws) {
      this.probe();
      return;
    }
    if (this.retryTimer) clearTimeout(this.retryTimer);
    this.retryTimer = null;
    this.attempt = 0;
    this.connect();
  }

  private connect(): void {
    if (this.stopped) return;
    this.retryTimer = null;
    // Only the first socket says so: a retry leaves the page saying what it did.
    if (!this.shown) this.report('connecting');
    const ws = this.h.open();
    this.ws = ws;
    this.arm(CONNECT_MS);
    // Not `open` yet: the server upgrades, then may refuse.
    ws.onopen = () => this.arm(STATE_MS);
    ws.onmessage = (ev) => this.message(ev.data);
    ws.onclose = (ev) => {
      this.drop();
      this.retry(ev.code);
    };
    ws.onerror = () => ws.close();
  }

  private message(data: unknown): void {
    // Anything at all shows the socket is alive, and a quick answer that the link is quick.
    if (this.probeAt && Date.now() - this.probeAt < PROBE_MS) this.slow = 0;
    this.disarm();
    if (this.quietTimer) clearTimeout(this.quietTimer);
    this.quietTimer = setTimeout(() => this.probe(), QUIET_MS);
    let msg: ServerEvent;
    try {
      msg = JSON.parse(data as string) as ServerEvent;
    } catch {
      return;
    }
    if (msg.type === 'state') {
      // The server sends the state first thing, so the connection has proved itself: the
      // backoff starts over. (An open alone doesn't: the server upgrades, then may refuse.)
      this.attempt = 0;
      this.droppedAt = 0;
      this.restarting = false;
      if (this.lostTimer) clearTimeout(this.lostTimer);
      this.lostTimer = null;
      this.report('open');
      this.h.onState(msg.state);
    } else if (msg.type === 'restarting') this.restarting = true;
    else if (msg.type === 'notify' || msg.type === 'invite' || msg.type === 'knowledge') this.h.onEvent(msg);
  }

  private probe(): void {
    const ws = this.ws;
    // A socket still connecting has its deadline already; one with a ping out is waiting on it.
    if (!ws || this.deadline) return;
    if (ws.readyState !== WebSocket.OPEN) {
      // Closing or closed, with a close event that may take minutes: start over now.
      this.drop();
      this.retry(null);
      return;
    }
    try {
      ws.send(JSON.stringify({ type: 'ping' } satisfies EventsClientMessage));
    } catch {
      /* the deadline below gives up on it */
    }
    this.arm(PROBE_MS);
    this.probeAt = Date.now();
  }

  /** Gives up on the socket after <ms> (doubled for a slow link) without a message. */
  private arm(ms: number): void {
    this.disarm();
    this.deadline = setTimeout(() => {
      this.deadline = null;
      this.slow = Math.min(MAX_SLOW, this.slow + 1);
      // A dead socket's close can take minutes to fire, so don't wait for it.
      this.drop();
      this.retry(null);
    }, ms * 2 ** this.slow);
  }

  private disarm(): void {
    if (this.deadline) clearTimeout(this.deadline);
    this.deadline = null;
    this.probeAt = 0;
  }

  /** Lets go of the current socket, without hearing from it again. */
  private drop(): void {
    this.disarm();
    if (this.quietTimer) clearTimeout(this.quietTimer);
    this.quietTimer = null;
    const ws = this.ws;
    this.ws = null;
    if (!ws) return;
    ws.onopen = ws.onmessage = ws.onclose = ws.onerror = null;
    try {
      ws.close();
    } catch {
      /* already closed */
    }
  }

  private report(c: Conn): void {
    if (this.shown === c) return;
    this.shown = c;
    this.h.onConn(c);
  }

  private retry(code: number | null): void {
    if (this.stopped) return;
    // The first drop of an outage starts the wait before saying so; later ones leave it running.
    if (!this.lostTimer && this.shown !== 'closed') {
      this.lostTimer = setTimeout(() => {
        this.lostTimer = null;
        this.report('closed');
      }, this.restarting ? UPDATE_RETRY_FOR_MS : LOST_GRACE_MS);
    }
    if (installing) this.droppedAt ||= Date.now();
    const delay = installing && Date.now() - this.droppedAt < UPDATE_RETRY_FOR_MS
      ? UPDATE_RETRY_MS
      : Math.min(15000, 500 * 2 ** this.attempt++) + Math.random() * 300;
    const later = () => {
      if (!this.stopped && !this.ws) this.retryTimer = setTimeout(() => this.connect(), delay);
    };
    // Signed out, or the sign-in expired: checkSignIn goes to Meko's sign-in screen.
    if (code !== null && this.h.unauthorized.includes(code)) void this.h.checkSignIn().then((ok) => ok && later());
    else {
      if (code === WS_ABNORMAL) void this.h.checkSignIn();
      later();
    }
  }
}
