import { EventEmitter } from 'node:events';
import { rmSync } from 'node:fs';
import { join } from 'node:path';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import type { WebSocket } from 'ws';
import type { Core } from '../../src/server/core.js';
import { startHost, type HostHandle } from '../../src/server/host/host.js';
import { SessionHost } from '../../src/server/hostclient.js';
import { getPaths } from '../../src/server/paths.js';
import { attachTerminal } from '../../src/server/terminal.js';
import { tempDir, testLockDeps, waitFor } from '../helpers.js';

/** A socket stand-in: records what the server sends, and lets the test play the browser. */
function fakeSocket() {
  const ws = Object.assign(new EventEmitter(), { OPEN: 1, readyState: 1, bufferedAmount: 0, sent: [] as unknown[], send: (_m: unknown) => {}, close: () => {} });
  ws.send = (m) => void ws.sent.push(m);
  ws.close = () => {
    if (ws.readyState === 3) return;
    ws.readyState = 3;
    ws.emit('close');
  };
  return ws;
}

describe('terminal socket', () => {
  let home: string;
  let handle: HostHandle;
  let host: SessionHost;
  beforeEach(async () => {
    home = tempDir('akl-term-');
    const paths = getPaths(join(home, 'ak'));
    handle = await startHost({ paths, version: 'test', lockDeps: testLockDeps() });
    host = new SessionHost(paths, { services: () => null });
  });
  afterEach(async () => {
    await handle.close();
    rmSync(home, { recursive: true, force: true });
  });

  const coreFor = (lines: string[] = []) =>
    ({ sessions: { row: () => ({ desired: 'running', tmux_name: 'x' }), typed: () => {} }, host, log: { info: (m: string) => lines.push(m), warn: () => {}, error: () => {} } }) as unknown as Core;
  const text = (ws: ReturnType<typeof fakeSocket>) => ws.sent.filter((m): m is Buffer => m instanceof Buffer).map((b) => b.toString('utf8')).join('');
  const sizeMessage = (ws: ReturnType<typeof fakeSocket>) => ws.sent.find((m): m is string => typeof m === 'string' && m.includes('"size"'));

  it('ignores malformed messages instead of throwing (which would stop the server)', async () => {
    await host.spawn('x', home, ['/bin/sh', '-c', 'sleep 5']);
    const ws = fakeSocket();
    attachTerminal(coreFor(), ws as unknown as WebSocket, 's1', { cols: '100', rows: '30' });
    try {
      // The browser that opened it sets the size.
      await waitFor(() => sizeMessage(ws), (m) => !!m);
      expect(JSON.parse(sizeMessage(ws)!)).toEqual({ type: 'size', cols: 100, rows: 30, active: true });
      for (const m of ['null', '5', '"x"', '[]', '{"type":"resize","cols":"a","rows":null}', '{"type":"input","data":7}', '{not json']) {
        expect(() => ws.emit('message', Buffer.from(m), false)).not.toThrow();
      }
      expect(() => ws.emit('message', Buffer.from('{"type":"resize","cols":90,"rows":20}'), false)).not.toThrow();
      await waitFor(() => ws.sent.filter((m) => typeof m === 'string' && m.includes('"cols":90')).length, (n) => n > 0);
    } finally {
      ws.close();
    }
  });

  it('shows a new viewer the screen as it is, and passes typing to the program', async () => {
    await host.spawn('x', home, ['/bin/sh', '-c', 'printf "ready\\n"; read line; printf "got %s\\n" "$line"; sleep 5']);
    await waitFor(() => host.capture('x'), (t) => t.includes('ready'));
    const ws = fakeSocket();
    attachTerminal(coreFor(), ws as unknown as WebSocket, 's1', { cols: '80', rows: '24' });
    try {
      // Output from before it attached arrives as the screen copy.
      await waitFor(() => text(ws), (t) => t.includes('ready'));
      ws.emit('message', Buffer.from(JSON.stringify({ type: 'input', data: 'hello\r' })), false);
      await waitFor(() => text(ws), (t) => t.includes('got hello'));
    } finally {
      ws.close();
    }
    // Closing the viewer leaves the program running.
    expect((await host.panes()).get('x')?.dead).toBe(false);
  });

  it('keeps a session on one screen: another waits, and Move to this screen takes it, its output and its size', async () => {
    await host.spawn('x', home, ['/bin/sh', '-c', 'printf "ready\\n"; read line; printf "got %s\\n" "$line"; sleep 5']);
    await waitFor(() => host.capture('x'), (t) => t.includes('ready'));
    const lines: string[] = [];
    const core = coreFor(lines);
    // Host streams opened and closed, by width; `onAttach` runs as an attach starts.
    const streams: string[] = [];
    let onAttach: (() => void) | null = null;
    Object.assign(core, {
      host: Object.assign(Object.create(host) as SessionHost, {
        attach: async (pane: string, size: { cols?: number; rows?: number }) => {
          streams.push(`attach ${size.cols}`);
          onAttach?.();
          const s = await host.attach(pane, size);
          const close = s.close.bind(s);
          return Object.assign(s, { close: () => (streams.push(`close ${size.cols}`), close()) });
        },
      }),
    });
    const screens = (ws: ReturnType<typeof fakeSocket>) =>
      ws.sent.filter((m): m is string => typeof m === 'string').map((m) => JSON.parse(m)).filter((m) => m.type === 'screen');
    const lastSize = (ws: ReturnType<typeof fakeSocket>) =>
      ws.sent.filter((m): m is string => typeof m === 'string' && m.includes('"size"')).map((m) => JSON.parse(m)).at(-1);
    const input = (ws: ReturnType<typeof fakeSocket>, m: object) => ws.emit('message', Buffer.from(JSON.stringify(m)), false);
    const laptop = fakeSocket();
    const phone = fakeSocket();
    attachTerminal(core, laptop as unknown as WebSocket, 's1', { cols: '100', rows: '30', screen: 'lap', browser: 'b1', claim: '1' }, { userAgent: 'Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36' });
    try {
      await waitFor(() => text(laptop), (t) => t.includes('ready'));
      input(laptop, { type: 'input', data: 'a' });
      attachTerminal(core, phone as unknown as WebSocket, 's1', { cols: '40', rows: '20', screen: 'ph', browser: 'b2', claim: '1' }, { userAgent: 'Mozilla/5.0 (iPhone; CPU iPhone OS 18_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.5 Mobile/15E148 Safari/604.1' });
      // The laptop was just used: the phone waits, without output, and its keys and size don't reach Claude.
      expect(screens(phone).at(-1)).toMatchObject({ state: 'waiting', other: { name: 'Chrome on macOS', sameBrowser: false } });
      expect(screens(laptop).at(-1)).toEqual({ type: 'screen', state: 'here', waiting: { name: 'Safari on iPhone', sameBrowser: false } });
      input(phone, { type: 'input', data: 'z' });
      input(phone, { type: 'active' });
      expect(lastSize(laptop)).toEqual({ type: 'size', cols: 100, rows: 30, active: true });
      expect(phone.sent.some((m) => m instanceof Buffer)).toBe(false);

      input(phone, { type: 'move' });
      expect(screens(laptop).at(-1)).toEqual({ type: 'screen', state: 'elsewhere', other: { name: 'Safari on iPhone', sameBrowser: false } });
      await waitFor(() => lastSize(phone), (m) => m?.active === true);
      expect(lastSize(phone)).toEqual({ type: 'size', cols: 40, rows: 20, active: true });
      const laptopSent = laptop.sent.length;
      input(laptop, { type: 'input', data: 'x' });
      input(phone, { type: 'input', data: 'hello\r' });
      await waitFor(() => text(phone), (t) => t.includes('got ahello'));
      // The laptop shows its last picture: no more output, and its keys were dropped.
      expect(laptop.sent.length).toBe(laptopSent);
      expect(lines.join('\n')).toMatch(/terminal \d+ \(session s1\): the session moved here from terminal \d+ \(Move to this screen\)/);

      // Moved back to the laptop and away again while the laptop's attach is in flight: its stream
      // closes before the phone's opens, so the pane ends at the phone's size.
      streams.length = 0;
      onAttach = () => {
        onAttach = null;
        input(phone, { type: 'move' });
      };
      input(laptop, { type: 'move' });
      await waitFor(() => streams.length, (n) => n >= 4);
      expect(streams).toEqual(['close 40', 'attach 100', 'close 100', 'attach 40']);
      await waitFor(() => lastSize(phone), (m) => m?.cols === 40 && m.active === true);
    } finally {
      laptop.close();
      phone.close();
    }
  });

  it('stops reading terminal output while the browser has a backlog, and resumes once it drains', async () => {
    await host.spawn('x', home, ['/bin/sh', '-c', 'yes']);
    const ws = fakeSocket();
    const wait = (ms: number) => new Promise((r) => setTimeout(r, ms));
    ws.bufferedAmount = 8 * 1024 * 1024; // a phone that stopped reading
    attachTerminal(coreFor(), ws as unknown as WebSocket, 's1');
    try {
      await waitFor(() => ws.sent.some((m) => m instanceof Buffer), (v) => v);
      await wait(300);
      const stalled = ws.sent.length;
      await wait(300);
      // Paused: at most the output already read before the pause arrives.
      expect(ws.sent.length).toBeLessThanOrEqual(stalled + 1);
      ws.bufferedAmount = 0;
      await wait(500);
      expect(ws.sent.length).toBeGreaterThan(stalled + 1);
    } finally {
      ws.close();
    }
  });
});
