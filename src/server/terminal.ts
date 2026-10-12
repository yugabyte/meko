import type { WebSocket } from 'ws';
import type { TermClientMessage, TermServerMessage } from '../shared/types.js';
import type { Core } from './core.js';
import type { HostStream } from './hostclient.js';
import { clampCols, clampRows } from './host/protocol.js';
import { errMsg, type Logger } from './log.js';

/** Unsent output past which the host stream is paused until the browser catches up. */
const MAX_BUFFERED = 1024 * 1024;
/** Sizing lines one terminal socket logs per window; past that they're only counted, so a resize loop can't flood the log. */
const SIZING_LOG_MAX = 20;
const SIZING_LOG_WINDOW_MS = 60_000;

const sizeOf = (v: unknown, clamp: (n: number) => number): number | undefined =>
  v !== undefined && v !== '' && Number.isFinite(Number(v)) ? clamp(Number(v)) : undefined;

/** A number per terminal socket, so the log tells viewers of one session apart. */
let viewerCount = 0;

/** What kind of input a browser sent, for the log (never the input itself). */
function inputKind(data: string): string {
  if (data.startsWith('\x1b[<') || data.startsWith('\x1b[M')) return 'a mouse report';
  if (data === '\x1b[I' || data === '\x1b[O') return 'a focus report';
  return 'keys';
}

/** Logs up to SIZING_LOG_MAX lines a window, then counts the rest; `flush` logs the count. */
function sizingLog(log: Logger, prefix: string) {
  let start = 0;
  let n = 0;
  let dropped = 0;
  const flush = () => {
    if (dropped) log.info(`${prefix}: ${dropped} more sizing events not logged`);
    dropped = 0;
  };
  const line = (msg: string) => {
    const now = Date.now();
    if (now - start >= SIZING_LOG_WINDOW_MS) {
      flush();
      start = now;
      n = 0;
    }
    if (n++ < SIZING_LOG_MAX) log.info(`${prefix}: ${msg}`);
    else dropped++;
  };
  return { line, flush };
}

/**
 * Bridges one browser terminal to a session's pane in the session host: an attach stream per
 * viewer. Closing the socket ends only that stream, never Claude. `size` is the browser's
 * terminal size from the socket URL, if it sent one: the pane takes it, as the newest viewer's.
 * `client` says where the socket came from, for the log.
 *
 * The log gets each socket's attach and close, and its sizing: the sizes it asks for, the pane's
 * size as the host reports it and whether this viewer set it, and input that arrives while another
 * viewer has the size (it takes the size). That is what tells which viewer resized a session.
 */
export function attachTerminal(
  core: Core,
  ws: WebSocket,
  sessionId: string,
  size: { cols?: unknown; rows?: unknown } = {},
  client = '',
): void {
  const send = (m: TermServerMessage) => ws.readyState === ws.OPEN && ws.send(JSON.stringify(m));
  const prefix = `terminal ${++viewerCount} (session ${sessionId})`;
  const sizing = sizingLog(core.log, prefix);
  const openedAt = Date.now();
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
  // The pane's size and whether this viewer set it, as the host last said.
  let pane: { cols: number; rows: number; active: boolean } | null = null;
  let asked = '';

  const handle = (m: TermClientMessage) => {
    if (!stream) {
      early.push(m);
      return;
    }
    if (m.type === 'input' && typeof m.data === 'string') {
      if (pane && !pane.active) sizing.line(`${inputKind(m.data)} while another viewer has the size`);
      stream.write(m.data);
    } else if (m.type === 'resize' && Number.isFinite(m.cols) && Number.isFinite(m.rows)) {
      const cols = clampCols(m.cols);
      const rows = clampRows(m.rows);
      if (`${cols}x${rows}` !== asked) sizing.line(`asks for ${cols}x${rows}`);
      asked = `${cols}x${rows}`;
      stream.resize(cols, rows);
    } else if (m.type === 'active') {
      if (pane && !pane.active) sizing.line('claims the size (focused)');
      stream.active();
    }
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
  ws.on('close', (code: number) => {
    if (stream) {
      sizing.flush();
      core.log.info(`${prefix}: closed after ${Math.round((Date.now() - openedAt) / 1000)} s (code ${code})`);
    }
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

  const cols = sizeOf(size.cols, clampCols);
  const rows = sizeOf(size.rows, clampRows);
  core.host
    .attach(row.tmux_name, { cols, rows })
    .then(
      (s) => {
        if (closed) {
          s.close();
          return;
        }
        stream = s;
        core.log.info(`${prefix}: attached at ${cols && rows ? `${cols}x${rows}` : 'no size'}${client ? ` from ${client}` : ''}`);
        s.onData((d) => {
          pending.push(d);
          flushTimer ??= setTimeout(flush, 16);
        });
        s.onControl((m) => {
          if (m.type !== 'size') return;
          if (!pane || pane.cols !== m.cols || pane.rows !== m.rows || pane.active !== m.active) {
            sizing.line(`pane is ${m.cols}x${m.rows}, ${m.active ? "this viewer's size" : "another viewer's size"}`);
          }
          pane = { cols: m.cols, rows: m.rows, active: m.active };
          send({ type: 'size', cols: m.cols, rows: m.rows, active: m.active });
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
