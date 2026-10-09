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
import { tempDir, waitFor } from '../helpers.js';

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
    handle = await startHost({ paths, version: 'test' });
    host = new SessionHost(paths, { services: () => null });
  });
  afterEach(async () => {
    await handle.close();
    rmSync(home, { recursive: true, force: true });
  });

  const coreFor = () => ({ sessions: { row: () => ({ desired: 'running', tmux_name: 'x' }) }, host }) as unknown as Core;
  const text = (ws: ReturnType<typeof fakeSocket>) => ws.sent.filter((m): m is Buffer => m instanceof Buffer).map((b) => b.toString('utf8')).join('');
  const sizeMessage = (ws: ReturnType<typeof fakeSocket>) => ws.sent.find((m): m is string => typeof m === 'string' && m.includes('"size"'));

  it('ignores malformed messages instead of throwing (which would stop the server)', async () => {
    await host.spawn('x', home, ['/bin/sh', '-c', 'sleep 5']);
    const ws = fakeSocket();
    attachTerminal(coreFor(), ws as unknown as WebSocket, 's1', { cols: '100', rows: '30' });
    try {
      for (const m of ['null', '5', '"x"', '[]', '{"type":"resize","cols":"a","rows":null}', '{"type":"input","data":7}', '{not json']) {
        expect(() => ws.emit('message', Buffer.from(m), false)).not.toThrow();
      }
      expect(() => ws.emit('message', Buffer.from('{"type":"resize","cols":90,"rows":20}'), false)).not.toThrow();
      // The browser that opened it sets the size.
      await waitFor(() => sizeMessage(ws), (m) => !!m);
      expect(JSON.parse(sizeMessage(ws)!)).toEqual({ type: 'size', cols: 100, rows: 30, active: true });
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
