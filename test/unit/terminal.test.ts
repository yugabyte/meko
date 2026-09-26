import { EventEmitter } from 'node:events';
import { describe, expect, it } from 'vitest';
import type { WebSocket } from 'ws';
import type { Core } from '../../src/server/core.js';
import { attachTerminal } from '../../src/server/terminal.js';

/** A socket stand-in: records what the server sends, and lets the test play the browser. */
function fakeSocket() {
  const ws = Object.assign(new EventEmitter(), { OPEN: 1, readyState: 1, bufferedAmount: 0, sent: [] as unknown[], send: (_m: unknown) => {}, close: () => {} });
  ws.send = (m) => void ws.sent.push(m);
  ws.close = () => {
    ws.readyState = 3;
    ws.emit('close');
  };
  return ws;
}

describe('terminal socket', () => {
  it('ignores malformed messages instead of throwing (which would stop the server)', () => {
    // The "tmux attach" is a shell that sleeps.
    const core = {
      sessions: { row: () => ({ desired: 'running', tmux_name: 'x' }) },
      tmux: { binary: '/bin/sh', attachArgs: () => ['-c', 'sleep 5'] },
    } as unknown as Core;
    const ws = fakeSocket();
    attachTerminal(core, ws as unknown as WebSocket, 's1', { cols: '100', rows: '30' });
    try {
      for (const m of ['null', '5', '"x"', '[]', '{"type":"resize","cols":"a","rows":null}', '{"type":"input","data":7}', '{not json']) {
        expect(() => ws.emit('message', Buffer.from(m), false)).not.toThrow();
      }
      expect(() => ws.emit('message', Buffer.from('{"type":"resize","cols":90,"rows":20}'), false)).not.toThrow();
    } finally {
      ws.close();
    }
  });

  it('stops reading terminal output while the browser has a backlog, and resumes once it drains', async () => {
    const core = {
      sessions: { row: () => ({ desired: 'running', tmux_name: 'x' }) },
      tmux: { binary: '/bin/sh', attachArgs: () => ['-c', 'yes'] },
    } as unknown as Core;
    const ws = fakeSocket();
    const wait = (ms: number) => new Promise((r) => setTimeout(r, ms));
    ws.bufferedAmount = 8 * 1024 * 1024; // a phone that stopped reading
    attachTerminal(core, ws as unknown as WebSocket, 's1');
    try {
      await wait(300);
      const stalled = ws.sent.length;
      expect(stalled).toBeGreaterThan(0);
      await wait(300);
      // Paused: at most the output already read before the pause arrives.
      expect(ws.sent.length).toBeLessThanOrEqual(stalled + 1);
      ws.bufferedAmount = 0;
      await wait(300);
      expect(ws.sent.length).toBeGreaterThan(stalled + 1);
    } finally {
      ws.close();
    }
  });
});
