import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { AppState } from '../../src/shared/types.js';
import { CONNECT_MS, EventsConnection, PROBE_MS, QUIET_MS, STATE_MS, type Conn, type SocketLike } from '../../src/web/events.js';

/** A socket the test drives: what the page sent, and whether it let go of it. */
class FakeSocket {
  readyState = 0;
  onopen: SocketLike['onopen'] = null;
  onmessage: SocketLike['onmessage'] = null;
  onclose: SocketLike['onclose'] = null;
  onerror: SocketLike['onerror'] = null;
  sent: string[] = [];
  closed = false;
  send(data: string) {
    this.sent.push(data);
  }
  close() {
    this.closed = true;
  }
  /** The server accepts the socket and sends the state. */
  accept(state = { v: 1 }) {
    this.opened();
    this.receive({ type: 'state', state });
  }
  opened() {
    this.readyState = 1;
    this.onopen?.call(this as never, {} as Event);
  }
  receive(msg: unknown) {
    this.onmessage?.call(this as never, { data: JSON.stringify(msg) } as MessageEvent);
  }
  serverClose(code = 1006) {
    this.readyState = 3;
    this.onclose?.call(this as never, { code } as CloseEvent);
  }
}

function setup(o: { checkLogin?: () => Promise<boolean> } = {}) {
  const sockets: FakeSocket[] = [];
  const states: unknown[] = [];
  const conns: Conn[] = [];
  const events: unknown[] = [];
  const conn = new EventsConnection({
    open: () => {
      const s = new FakeSocket();
      sockets.push(s);
      return s as unknown as SocketLike;
    },
    onState: (s: AppState) => states.push(s),
    onConn: (c) => conns.push(c),
    onEvent: (ev) => events.push(ev),
    checkLogin: o.checkLogin ?? (() => Promise.resolve(true)),
    unauthorized: 4401,
  });
  conn.start();
  return { conn, sockets, states, conns, events, last: () => sockets[sockets.length - 1]! };
}

describe('the events connection', () => {
  beforeEach(() => vi.useFakeTimers());
  afterEach(() => vi.useRealTimers());

  it('passes on state, notifications and invites, and ignores pongs', () => {
    const t = setup();
    t.last().accept({ v: 1 });
    t.last().receive({ type: 'notify', projectId: 'p', sessionId: 's', title: 'a', body: 'b' });
    t.last().receive({ type: 'invite', datapackId: 'd', title: 'a', body: 'b' });
    t.last().receive({ type: 'pong' });
    expect(t.states).toEqual([{ v: 1 }]);
    expect(t.events.map((e) => (e as { type: string }).type)).toEqual(['notify', 'invite']);
    expect(t.conns).toEqual(['connecting', 'open']);
    t.conn.stop();
  });

  it('pings a socket that has been quiet, and keeps it when the server answers', () => {
    const t = setup();
    t.last().accept();
    vi.advanceTimersByTime(QUIET_MS);
    expect(t.last().sent).toEqual([JSON.stringify({ type: 'ping' })]);
    t.last().receive({ type: 'pong' });
    vi.advanceTimersByTime(PROBE_MS * 2);
    expect(t.sockets).toHaveLength(1);
    expect(t.last().closed).toBe(false);
    t.conn.stop();
  });

  it('drops a socket that stops answering and starts over with the current state', () => {
    const t = setup();
    const dead = t.last();
    dead.accept({ v: 1 });
    vi.advanceTimersByTime(QUIET_MS + PROBE_MS);
    expect(dead.closed).toBe(true);
    expect(t.conns.at(-1)).toBe('closed');
    vi.advanceTimersByTime(1000);
    expect(t.sockets).toHaveLength(2);
    t.last().accept({ v: 2 });
    expect(t.states).toEqual([{ v: 1 }, { v: 2 }]);
    // The dead socket's late close doesn't start a third.
    dead.serverClose();
    vi.advanceTimersByTime(20_000);
    expect(t.sockets).toHaveLength(2);
    t.conn.stop();
  });

  it('checks the socket at once when the page is shown again', () => {
    const t = setup();
    t.last().accept();
    vi.advanceTimersByTime(QUIET_MS - 1000);
    t.conn.resume();
    expect(t.last().sent).toHaveLength(1);
    // A second resume while the ping is out sends no other.
    t.conn.resume();
    expect(t.last().sent).toHaveLength(1);
    vi.advanceTimersByTime(PROBE_MS);
    vi.advanceTimersByTime(1000);
    expect(t.sockets).toHaveLength(2);
    t.conn.stop();
  });

  it('reconnects at once on resume instead of waiting out the backoff', () => {
    const t = setup();
    // Each failed attempt waits longer: by the fifth, 8 seconds.
    for (let i = 0; i < 4; i++) {
      t.last().serverClose();
      const n = t.sockets.length;
      while (t.sockets.length === n) vi.advanceTimersByTime(100);
    }
    t.last().serverClose();
    vi.advanceTimersByTime(2000);
    expect(t.sockets).toHaveLength(5);
    t.conn.resume();
    expect(t.sockets).toHaveLength(6);
    t.conn.stop();
  });

  it('gives up on a socket that never opens', () => {
    const t = setup();
    vi.advanceTimersByTime(CONNECT_MS);
    expect(t.sockets[0]!.closed).toBe(true);
    vi.advanceTimersByTime(1000);
    expect(t.sockets).toHaveLength(2);
    t.conn.stop();
  });

  it('gives an open socket longer for its first state, and more on each try', () => {
    const t = setup();
    t.last().opened();
    vi.advanceTimersByTime(STATE_MS - 1);
    expect(t.last().closed).toBe(false);
    vi.advanceTimersByTime(1);
    expect(t.sockets[0]!.closed).toBe(true);
    // A link this slow gets twice as long on the next socket, so a big snapshot can come through.
    vi.advanceTimersByTime(1000);
    t.last().opened();
    vi.advanceTimersByTime(STATE_MS * 2 - 1);
    expect(t.last().closed).toBe(false);
    t.last().receive({ type: 'state', state: { v: 2 } });
    expect(t.states).toEqual([{ v: 2 }]);
    expect(t.sockets).toHaveLength(2);
    t.conn.stop();
  });

  it('goes back to the short ping deadline once a ping is answered quickly', () => {
    const t = setup();
    t.last().accept();
    vi.advanceTimersByTime(QUIET_MS + PROBE_MS);
    vi.advanceTimersByTime(1000);
    t.last().accept();
    // The next ping gets twice as long: the dropped socket may have been a slow one.
    t.conn.resume();
    vi.advanceTimersByTime(PROBE_MS);
    expect(t.last().closed).toBe(false);
    // A slow answer doesn't show the link is quick; a quick one does.
    t.last().receive({ type: 'pong' });
    t.conn.resume();
    vi.advanceTimersByTime(100);
    t.last().receive({ type: 'pong' });
    t.conn.resume();
    vi.advanceTimersByTime(PROBE_MS);
    expect(t.last().closed).toBe(true);
    t.conn.stop();
  });

  it('starts over at once on resume when the socket is closing but its close never came', () => {
    const t = setup();
    t.last().accept();
    t.last().readyState = 2;
    t.conn.resume();
    expect(t.sockets[0]!.closed).toBe(true);
    vi.advanceTimersByTime(1000);
    expect(t.sockets).toHaveLength(2);
    t.conn.stop();
  });

  it('stops retrying when the login is gone', async () => {
    const t = setup({ checkLogin: () => Promise.resolve(false) });
    t.last().accept();
    t.last().serverClose(4401);
    await vi.advanceTimersByTimeAsync(20_000);
    expect(t.sockets).toHaveLength(1);
    t.conn.stop();
  });

  it('opens nothing more once stopped', () => {
    const t = setup();
    t.last().accept();
    t.conn.stop();
    expect(t.last().closed).toBe(true);
    t.conn.resume();
    vi.advanceTimersByTime(60_000);
    expect(t.sockets).toHaveLength(1);
  });
});
