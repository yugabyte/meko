import type { WebSocket } from 'ws';
import { isPassiveReport } from '../shared/termreports.js';
import { parseTermTheme } from '../shared/termcolors.js';
import type { ScreenState, TermClientMessage, TermServerMessage } from '../shared/types.js';
import type { Core } from './core.js';
import type { HostStream } from './hostclient.js';
import { clampCols, clampRows } from './host/protocol.js';
import { errMsg } from './log.js';
import { ScreenBoard, screenName, type Screen } from './screens.js';

/** Unsent output past which the host stream is paused until the browser catches up. */
const MAX_BUFFERED = 1024 * 1024;

const sizeOf = (v: unknown, clamp: (n: number) => number): number | undefined =>
  v !== undefined && v !== '' && Number.isFinite(Number(v)) ? clamp(Number(v)) : undefined;

/** Page and browser ids from the socket URL: short and plain, or none. */
const idOf = (v: unknown): string => (typeof v === 'string' && /^[\w-]{1,64}$/.test(v) ? v : '');

/** A number per terminal socket, so the log tells screens of one session apart. */
let viewerCount = 0;

/**
 * Each session's screens, while it has any. `released` settles once every host stream the session
 * moved away from is closed, attaches still in flight included, so the next screen's stream opens
 * after them and the pane ends at its size.
 */
const boards = new Map<string, { board: ScreenBoard; released: Promise<void> }>();

/** A socket's screen, with the log prefix the board's moves are logged under. */
type LoggedScreen = Screen & { prefix: string };

/** The query of a terminal socket's URL (see the /ws/term route in src/shared/types.ts). */
export interface TermQuery {
  cols?: unknown;
  rows?: unknown;
  screen?: unknown;
  browser?: unknown;
  claim?: unknown;
  /** This browser's terminal colors, `dark` or `light`, which sessions started next take. */
  theme?: unknown;
}

/**
 * Bridges one browser terminal to a session's pane in the session host. A session is live on one
 * screen at a time (screens.ts): only the screen that has it holds an attach stream, at its size,
 * and gets output; another screen's socket stays open to hear when the session moves, its input is
 * dropped, and its last size is kept for when the session moves to it. Closing a socket ends only
 * its stream, never Claude. `client` says where the socket came from, for the log.
 *
 * The log gets each socket's attach and close, and one line per move of the session between
 * screens (Move to this screen, the other screen idle, not showing the session, or closed).
 */
export function attachTerminal(
  core: Core,
  ws: WebSocket,
  sessionId: string,
  query: TermQuery = {},
  client: { ip?: string; userAgent?: string } = {},
): void {
  const send = (m: TermServerMessage) => ws.readyState === ws.OPEN && ws.send(JSON.stringify(m));
  const prefix = `terminal ${++viewerCount} (session ${sessionId})`;
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
  const pane = row.tmux_name;
  const shared = boards.get(sessionId) ?? {
    board: new ScreenBoard((to, from, why) => {
      if (why === 'first' || why === 'same screen') return;
      const t = to as LoggedScreen;
      core.log.info(`${t.prefix}: the session moved here${from ? ` from ${(from as LoggedScreen).prefix.split(' (')[0]}` : ''} (${why})`);
    }),
    released: Promise.resolve(),
  };
  boards.set(sessionId, shared);

  let stream: HostStream | null = null;
  /** The attach in flight, until its stream is open or it failed. */
  let attaching: Promise<unknown> | null = null;
  let closed = false;
  /** This screen has the session: it holds (or is opening) a stream. */
  let here = false;
  // Counts streams, so a stream that opens after the session moved away again is dropped.
  let streamGen = 0;
  // Input that came after the session moved here but before its stream was open.
  const early: TermClientMessage[] = [];
  // Coalesce output into ~16ms frames so bursts don't become thousands of messages.
  let pending: Buffer[] = [];
  let flushTimer: NodeJS.Timeout | null = null;
  let drainTimer: NodeJS.Timeout | null = null;
  // The size this browser last asked for: the pane takes it while this screen has the session.
  let cols = sizeOf(query.cols, clampCols);
  let rows = sizeOf(query.rows, clampRows);

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
        if (ws.readyState === ws.OPEN && ws.bufferedAmount > MAX_BUFFERED / 4 && stream === s) return;
        clearInterval(drainTimer!);
        drainTimer = null;
        if (stream === s) s.resume();
      }, 50);
    }
  };

  const stopStream = () => {
    const s = stream;
    stream = null;
    // An attach still in flight closes its stream when it resolves (startStream); the next screen's waits for that.
    if (attaching) {
      const settled = attaching.then(() => undefined, () => undefined);
      shared.released = shared.released.then(() => settled);
      attaching = null;
    }
    streamGen++;
    early.length = 0;
    pending = [];
    if (flushTimer) clearTimeout(flushTimer);
    flushTimer = null;
    if (drainTimer) clearInterval(drainTimer);
    drainTimer = null;
    s?.close();
  };

  const startStream = () => {
    const gen = ++streamGen;
    const opening = shared.released.then(() => (closed || gen !== streamGen ? null : core.host.attach(pane, { cols, rows })));
    attaching = opening;
    opening.then(
      (s) => {
        if (!s) return;
        if (closed || gen !== streamGen) {
          s.close();
          return;
        }
        if (attaching === opening) attaching = null;
        stream = s;
        s.onData((d) => {
          if (stream !== s) return;
          pending.push(d);
          flushTimer ??= setTimeout(flush, 16);
        });
        s.onControl((m) => {
          if (stream === s && m.type === 'size') send({ type: 'size', cols: m.cols, rows: m.rows, active: m.active });
        });
        s.onClose(() => {
          // A stream let go of because the session moved to another screen isn't the pane ending.
          if (stream !== s) return;
          flush();
          send({ type: 'exit', reason: 'detached' });
          ws.close();
        });
        // Without a size it attached as a watcher: make it the viewer whose size the pane takes.
        if (!cols || !rows) s.active();
        for (const m of early.splice(0)) handle(m);
      },
      (e: unknown) => {
        if (attaching === opening) attaching = null;
        if (closed || gen !== streamGen) return;
        send({ type: 'error', error: `Couldn't attach: ${errMsg(e)}`, retry: true });
        ws.close();
      },
    );
  };

  const screen: LoggedScreen = {
    id: idOf(query.screen) || `socket-${viewerCount}`,
    browser: idOf(query.browser),
    name: screenName(client.userAgent),
    prefix,
    tell: (state: ScreenState) => {
      if (closed) return;
      send({ type: 'screen', ...state });
      if (state.state === 'here' && !here) {
        here = true;
        startStream();
      } else if (state.state !== 'here' && here) {
        here = false;
        stopStream();
      }
    },
  };

  const b = shared.board;
  // The latest browser's terminal colors are the ones the next session starts with, whichever screen has this one.
  const reportTheme = (v: unknown) => {
    const theme = parseTermTheme(v);
    if (theme) core.setTermTheme(theme);
  };

  function handle(m: TermClientMessage) {
    if (m.type === 'show') return b.show(screen);
    if (m.type === 'hide') return b.hide(screen);
    if (m.type === 'move') return b.move(screen);
    if (m.type === 'theme') return reportTheme(m.theme);
    if (m.type === 'resize') {
      if (!Number.isFinite(m.cols) || !Number.isFinite(m.rows)) return;
      cols = clampCols(m.cols);
      rows = clampRows(m.rows);
    }
    // Only the screen that has the session reaches Claude; another one's size is kept for later.
    if (!here) return;
    if (!stream) {
      early.push(m);
      return;
    }
    if (m.type === 'input' && typeof m.data === 'string') {
      if (!isPassiveReport(m.data)) {
        b.used(screen);
        core.sessions.typed(sessionId);
      }
      stream.write(m.data);
    } else if (m.type === 'resize') stream.resize(cols!, rows!);
    else if (m.type === 'active') stream.active();
  }

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
    core.log.info(`${prefix}: closed after ${Math.round((Date.now() - openedAt) / 1000)} s (code ${code})`);
    closed = true;
    here = false;
    stopStream();
    b.leave(screen);
    if (b.empty && boards.get(sessionId) === shared) boards.delete(sessionId);
  });

  core.log.info(
    `${prefix}: attached at ${cols && rows ? `${cols}x${rows}` : 'no size'}${client.ip ? ` from ${client.ip}` : ''} (${(client.userAgent ?? 'no user agent').slice(0, 160)})`,
  );
  reportTheme(query.theme);
  // A page from before screens (no id) always asks, as every viewer used to take the session.
  b.join(screen, query.claim === '1' || !idOf(query.screen));
}
