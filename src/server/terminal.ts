import type { WebSocket } from 'ws';
import type { TermClientMessage, TermServerMessage } from '../shared/types.js';
import type { Core } from './core.js';
import type { HostStream } from './hostclient.js';
import { clampCols, clampRows } from './host/protocol.js';
import { errMsg } from './log.js';

/** Unsent output past which the host stream is paused until the browser catches up. */
const MAX_BUFFERED = 1024 * 1024;

const sizeOf = (v: unknown, clamp: (n: number) => number): number | undefined =>
  v !== undefined && v !== '' && Number.isFinite(Number(v)) ? clamp(Number(v)) : undefined;

/**
 * Bridges one browser terminal to a session's pane in the session host: an attach stream per
 * viewer. Closing the socket ends only that stream, never Claude. `size` is the browser's
 * terminal size from the socket URL, if it sent one: the pane takes it, as the newest viewer's.
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

  let stream: HostStream | null = null;
  let closed = false;
  // Input that came before the stream was open.
  const early: TermClientMessage[] = [];
  // Coalesce output into ~16ms frames so bursts don't become thousands of messages.
  let pending: Buffer[] = [];
  let flushTimer: NodeJS.Timeout | null = null;
  let drainTimer: NodeJS.Timeout | null = null;

  const handle = (m: TermClientMessage) => {
    if (!stream) {
      early.push(m);
      return;
    }
    if (m.type === 'input' && typeof m.data === 'string') stream.write(m.data);
    else if (m.type === 'resize' && Number.isFinite(m.cols) && Number.isFinite(m.rows)) stream.resize(clampCols(m.cols), clampRows(m.rows));
    else if (m.type === 'active') stream.active();
  };

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
    handle(m);
  });
  ws.on('close', () => {
    closed = true;
    if (flushTimer) clearTimeout(flushTimer);
    if (drainTimer) clearInterval(drainTimer);
    stream?.close();
  });

  const flush = () => {
    flushTimer = null;
    if (!pending.length || ws.readyState !== ws.OPEN || !stream) return;
    ws.send(Buffer.concat(pending), { binary: true });
    pending = [];
    // A browser that can't keep up: stop reading from the host until it drains, so output
    // doesn't pile up here. The host sends a fresh screen once it catches up; Claude never waits.
    if (ws.bufferedAmount > MAX_BUFFERED && !drainTimer) {
      const s = stream;
      s.pause();
      drainTimer = setInterval(() => {
        if (ws.readyState === ws.OPEN && ws.bufferedAmount > MAX_BUFFERED / 4) return;
        clearInterval(drainTimer!);
        drainTimer = null;
        s.resume();
      }, 50);
    }
  };

  core.host
    .attach(row.tmux_name, { cols: sizeOf(size.cols, clampCols), rows: sizeOf(size.rows, clampRows) })
    .then(
      (s) => {
        if (closed) {
          s.close();
          return;
        }
        stream = s;
        s.onData((d) => {
          pending.push(d);
          flushTimer ??= setTimeout(flush, 16);
        });
        s.onControl((m) => {
          if (m.type === 'size') send({ type: 'size', cols: m.cols, rows: m.rows, active: m.active });
        });
        s.onClose(() => {
          flush();
          send({ type: 'exit', reason: 'detached' });
          ws.close();
        });
        for (const m of early.splice(0)) handle(m);
      },
      (e: unknown) => {
        send({ type: 'error', error: `Couldn't attach: ${errMsg(e)}`, retry: true });
        ws.close();
      },
    );
}
