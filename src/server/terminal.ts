import { homedir } from 'node:os';
import pty from 'node-pty';
import type { WebSocket } from 'ws';
import type { TermClientMessage, TermServerMessage } from '../shared/types.js';
import type { Core } from './core.js';
import { errMsg } from './log.js';

/**
 * Bridges one browser terminal to a tmux session: a pty running
 * `tmux attach` per viewer. Closing the socket kills only the attach client,
 * never Claude.
 */
export function attachTerminal(core: Core, ws: WebSocket, sessionId: string): void {
  const send = (m: TermServerMessage) => ws.readyState === ws.OPEN && ws.send(JSON.stringify(m));
  let row;
  try {
    row = core.sessions.row(sessionId);
  } catch {
    send({ type: 'error', error: 'That session no longer exists.' });
    ws.close();
    return;
  }
  if (row.desired !== 'running') {
    send({ type: 'error', error: 'This session is archived.' });
    ws.close();
    return;
  }

  const env: Record<string, string> = {};
  for (const [k, v] of Object.entries(process.env)) if (v !== undefined && k !== 'TMUX' && k !== 'TMUX_PANE') env[k] = v;
  env.TERM = 'xterm-256color';
  env.COLORTERM = 'truecolor';

  let term: pty.IPty;
  try {
    term = pty.spawn(core.tmux.binary, core.tmux.attachArgs(row.tmux_name), {
      name: 'xterm-256color',
      cols: 120,
      rows: 36,
      cwd: homedir(),
      env,
    });
  } catch (e) {
    send({ type: 'error', error: `Couldn't attach: ${errMsg(e)}` });
    ws.close();
    return;
  }

  // Coalesce output into ~16ms frames so bursts don't become thousands of messages.
  let pending: string[] = [];
  let flushTimer: NodeJS.Timeout | null = null;
  const flush = () => {
    flushTimer = null;
    if (!pending.length || ws.readyState !== ws.OPEN) return;
    ws.send(Buffer.from(pending.join(''), 'utf8'), { binary: true });
    pending = [];
  };
  term.onData((d) => {
    pending.push(d);
    flushTimer ??= setTimeout(flush, 16);
  });
  term.onExit(() => {
    flush();
    send({ type: 'exit', reason: 'detached' });
    ws.close();
  });

  ws.on('message', (raw, isBinary) => {
    if (isBinary) return;
    let m: TermClientMessage;
    try {
      m = JSON.parse(String(raw)) as TermClientMessage;
    } catch {
      return;
    }
    if (m.type === 'input' && typeof m.data === 'string') term.write(m.data);
    else if (m.type === 'resize') {
      const cols = Math.max(20, Math.min(500, Math.floor(m.cols)));
      const rows = Math.max(5, Math.min(300, Math.floor(m.rows)));
      try {
        term.resize(cols, rows);
      } catch {
        /* pty already gone */
      }
    }
  });
  ws.on('close', () => {
    if (flushTimer) clearTimeout(flushTimer);
    try {
      term.kill();
    } catch {
      /* already exited */
    }
  });
}
