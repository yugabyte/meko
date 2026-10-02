import type { AppState, EventsClientMessage, ServerEvent } from '../shared/types';

export type Conn = 'connecting' | 'open' | 'closed';

/** After this long without a message, the socket is asked to prove it's still there. */
export const QUIET_MS = 30_000;
/** How long a ping's answer may take before the socket is given up on. */
export const PROBE_MS = 5_000;
/** How long a new socket may take to send its first state. */
export const CONNECT_MS = 10_000;

/** The parts of a browser WebSocket the connection uses. */
export type SocketLike = Pick<WebSocket, 'readyState' | 'onopen' | 'onmessage' | 'onclose' | 'onerror' | 'send' | 'close'>;

/** WebSocket.OPEN, which isn't defined outside a browser. */
const OPEN = 1;

export interface EventsHandlers {
  open: () => SocketLike;
  onState: (s: AppState) => void;
  onConn: (c: Conn) => void;
  onEvent: (ev: Extract<ServerEvent, { type: 'notify' | 'invite' }>) => void;
  /** After a 4401 close: resolves true to keep retrying, false when the login is gone. */
  checkLogin: () => Promise<boolean>;
  /** Close code that means the login is gone. */
  unauthorized: number;
}

/**
 * The /ws/events connection: reconnects with exponential backoff, and doesn't trust a quiet
 * socket. A phone that puts the page in the background (or changes networks) often never hears
 * that the server dropped its socket, so the page would keep showing the old state with nothing
 * coming in. A socket that has been quiet for QUIET_MS, or whose page was just shown again
 * (resume()), is pinged; one that doesn't answer within PROBE_MS is dropped for a new one, which
 * starts with the current state.
 */
export class EventsConnection {
  private ws: SocketLike | null = null;
  private retryTimer: ReturnType<typeof setTimeout> | null = null;
  private quietTimer: ReturnType<typeof setTimeout> | null = null;
  /** Running while a ping, or a new socket's first state, is awaited. */
  private deadline: ReturnType<typeof setTimeout> | null = null;
  private attempt = 0;
  private stopped = false;

  constructor(private readonly h: EventsHandlers) {}

  start(): void {
    this.connect();
  }

  stop(): void {
    this.stopped = true;
    if (this.retryTimer) clearTimeout(this.retryTimer);
    this.retryTimer = null;
    this.drop();
  }

  /**
   * The page was shown again or came back online: a socket waiting to reconnect does so now, and
   * an open one is checked.
   */
  resume(): void {
    if (this.stopped) return;
    if (!this.ws) {
      if (this.retryTimer) clearTimeout(this.retryTimer);
      this.retryTimer = null;
      this.attempt = 0;
      this.connect();
    } else if (this.ws.readyState === OPEN) {
      this.probe();
    }
    // Still connecting: its own deadline covers it.
  }

  private connect(): void {
    if (this.stopped) return;
    this.retryTimer = null;
    this.h.onConn('connecting');
    const ws = this.h.open();
    this.ws = ws;
    this.arm(CONNECT_MS);
    ws.onopen = () => this.h.onConn('open');
    ws.onmessage = (ev) => this.message(ev.data);
    ws.onclose = (ev) => {
      this.drop();
      this.retry(ev.code);
    };
    ws.onerror = () => ws.close();
  }

  private message(data: unknown): void {
    // Anything at all shows the socket is alive.
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
      this.h.onState(msg.state);
    } else if (msg.type === 'notify' || msg.type === 'invite') this.h.onEvent(msg);
  }

  private probe(): void {
    const ws = this.ws;
    if (!ws || ws.readyState !== OPEN || this.deadline) return;
    try {
      ws.send(JSON.stringify({ type: 'ping' } satisfies EventsClientMessage));
    } catch {
      /* the deadline below gives up on it */
    }
    this.arm(PROBE_MS);
  }

  /** Gives up on the socket after <ms> without a message. */
  private arm(ms: number): void {
    this.disarm();
    this.deadline = setTimeout(() => {
      this.deadline = null;
      // A dead socket's close can take minutes to fire, so don't wait for it.
      this.drop();
      this.retry(null);
    }, ms);
  }

  private disarm(): void {
    if (this.deadline) clearTimeout(this.deadline);
    this.deadline = null;
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

  private retry(code: number | null): void {
    if (this.stopped) return;
    this.h.onConn('closed');
    const delay = Math.min(15000, 500 * 2 ** this.attempt++) + Math.random() * 300;
    const later = () => {
      if (!this.stopped && !this.ws) this.retryTimer = setTimeout(() => this.connect(), delay);
    };
    // Logged out, password changed or login expired: the 401 handler shows the login screen.
    if (code === this.h.unauthorized) void this.h.checkLogin().then((ok) => ok && later());
    else later();
  }
}
