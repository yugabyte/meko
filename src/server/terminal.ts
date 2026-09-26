import { homedir } from 'node:os';
import pty from 'node-pty';
import type { WebSocket } from 'ws';
import type { TermClientMessage, TermServerMessage } from '../shared/types.js';
import type { Core } from './core.js';
import { errMsg } from './log.js';

/** Unsent output past which the pty is paused until the browser catches up. */
const MAX_BUFFERED = 1024 * 1024;
const clampCols = (n: number) => Math.max(20, Math.min(500, Math.floor(n)));
const clampRows = (n: number) => Math.max(5, Math.min(300, Math.floor(n)));

/**
 * Bridges one browser terminal to a tmux session: a pty running
 * `tmux attach` per viewer. Closing the socket kills only the attach client,
 * never Claude. `size` is the browser's terminal size from the socket URL, if it sent one.
 */
export function attachTerminal(core: Core, ws: WebSocket, sessionId: string, size: { cols?: unknown; rows?: unknown } = {}): void {
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
      // Start at the browser's size when it sent one, so a reconnect doesn't resize the window
      // to a default and back (tmux sizes a window to its smallest client).
      cols: Number.isFinite(Number(size.cols)) && size.cols !== '' ? clampCols(Number(size.cols)) : 120,
      rows: Number.isFinite(Number(size.rows)) && size.rows !== '' ? clampRows(Number(size.rows)) : 36,
      cwd: homedir(),
      env,
    });
  } catch (e) {
    send({ type: 'error', error: `Couldn't attach: ${errMsg(e)}`, retry: true });
    ws.close();
    return;
  }

  // Coalesce output into ~16ms frames so bursts don't become thousands of messages.
  let pending: string[] = [];
  let flushTimer: NodeJS.Timeout | null = null;
  let drainTimer: NodeJS.Timeout | null = null;
  const flush = () => {
    flushTimer = null;
    if (!pending.length || ws.readyState !== ws.OPEN) return;
    ws.send(Buffer.from(pending.join(''), 'utf8'), { binary: true });
    pending = [];
    // A browser that can't keep up: stop reading from `tmux attach` until it drains, so output
    // doesn't pile up in memory. tmux copes with a slow client (and redraws), Claude never waits.
    if (ws.bufferedAmount > MAX_BUFFERED && !drainTimer) {
      term.pause();
      drainTimer = setInterval(() => {
        if (ws.readyState === ws.OPEN && ws.bufferedAmount > MAX_BUFFERED / 4) return;
        clearInterval(drainTimer!);
        drainTimer = null;
        term.resume();
      }, 50);
    }
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
    // A throw in this listener would take the whole server down: accept only well-formed messages.
    if (!m || typeof m !== 'object') return;
    if (m.type === 'input' && typeof m.data === 'string') term.write(m.data);
    else if (m.type === 'resize' && Number.isFinite(m.cols) && Number.isFinite(m.rows)) {
      try {
        term.resize(clampCols(m.cols), clampRows(m.rows));
      } catch {
        /* pty already gone */
      }
    }
  });
  ws.on('close', () => {
    if (flushTimer) clearTimeout(flushTimer);
    if (drainTimer) clearInterval(drainTimer);
    try {
      term.kill();
    } catch {
      /* already exited */
    }
  });
}
