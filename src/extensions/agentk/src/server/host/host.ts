import { mkdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { createConnection, createServer, type Server, type Socket } from 'node:net';
import { dirname } from 'node:path';
import pty from 'node-pty';
import headless from '@xterm/headless';
import serialize from '@xterm/addon-serialize';
import { listenPrivate, socketId } from '../hooks.js';
import { BEAT_MS, InstanceBusyError, InstanceLock, type InstanceDeps } from '../instance.js';
import { errMsg, silentLogger, type Logger } from '../log.js';
import type { Paths } from '../paths.js';
import {
  clampCols, clampRows, encodeFrame, FRAME_CONTROL, FRAME_DATA, FrameReader, lineReader, MAX_REQUEST, parseControl, PROTOCOL,
  type HostControl, type HostRequest, type PaneState, type ViewerControl,
} from './protocol.js';

/**
 * The session host: the process that holds every Claude in a pty, so restarting or updating
 * the web server never ends a session. It runs in its own background service (or, without
 * one, detached from the server), listens only on `host.sock`, and keeps a copy of each
 * pane's screen (a headless xterm) so a viewer that attaches gets the screen as it is now.
 * It interprets no keys of its own: everything a viewer types reaches the program.
 *
 * Keep it small: a running host keeps the code it started with across updates, and
 * restarting it ends every Claude (they are resumed by the server's recovery).
 */

const { Terminal } = headless;
type HeadlessTerminal = InstanceType<typeof Terminal>;
const { SerializeAddon } = serialize;

/** Unsent output to a viewer past which it is skipped until it drains; it then gets a fresh screen. */
const MAX_BUFFERED = 1024 * 1024;
/** Output held for a viewer while its screen is being copied; past this it starts over. */
const MAX_PENDING = 4 * 1024 * 1024;
/** Pause between a paste and the Enter that submits it, so Claude takes the paste first. */
const PASTE_ENTER_MS = 300;
const DEFAULT_COLS = 200;
const DEFAULT_ROWS = 50;

export interface HostOptions {
  paths: Paths;
  version: string;
  /** The app directory the host runs from (kept by the updater while the host runs). */
  appDir?: string | null;
  log?: Logger;
  /** Lines of scrollback kept per pane. */
  scrollback?: number;
  /** Called after a `stop` request has ended every pane, or once another host took the lease (the CLI exits there). */
  onStop?: () => void;
  /** For tests: how the lease sees this process and the time. */
  lockDeps?: InstanceDeps;
}

export interface HostHandle {
  /** Ends every pane and stops listening. */
  close(): Promise<void>;
}

/** Another host already runs on this home. */
export class HostRunningError extends Error {}

class Viewer {
  cols = 0;
  rows = 0;
  lastActive = Date.now();
  /** Behind on output: skipped until the socket drains. */
  lagging = false;
  /** Output held while this viewer's screen copy is made (null when not copying). */
  pending: Buffer[] | null = null;
  pendingBytes = 0;
  closed = false;

  constructor(readonly sock: Socket) {}

  write(frame: Buffer): void {
    if (this.closed) return;
    this.sock.write(frame);
  }
}

class Pane {
  proc: pty.IPty | null = null;
  readonly term: HeadlessTerminal;
  private readonly serializer = new SerializeAddon();
  dead = false;
  exitStatus: number | null = null;
  pid = 0;
  cols: number;
  rows: number;
  readonly viewers = new Set<Viewer>();
  active: Viewer | null = null;
  /** Pastes go one at a time, each with its Enter. */
  pasting: Promise<void> = Promise.resolve();
  /** Modes the screen copy doesn't carry: mouse encoding (1005, 1006, 1015) and a hidden cursor. */
  private mouseEncoding: number | null = null;
  private cursorHidden = false;

  /**
   * A clean screen for a program started again, the modes tracked beside it included. Queued
   * behind the output already written: the terminal parses writes later, so a reset done now
   * would let the last program's final output land on the new screen.
   */
  reset(): void {
    this.term.write('', () => {
      this.term.reset();
      this.mouseEncoding = null;
      this.cursorHidden = false;
    });
  }

  constructor(readonly name: string, cols: number, rows: number, scrollback: number) {
    this.cols = cols;
    this.rows = rows;
    this.term = new Terminal({ cols, rows, scrollback, allowProposedApi: true });
    this.term.loadAddon(this.serializer as unknown as Parameters<HeadlessTerminal['loadAddon']>[0]);
    // The program's queries (device attributes, cursor position, colors) are answered here, once,
    // whether or not anyone is watching. Viewers' own answers are dropped by the browser.
    this.term.onData((d) => this.proc?.write(d));
    const track = (on: boolean) => (params: (number | number[])[]) => {
      for (const p of params) {
        if (p === 1005 || p === 1006 || p === 1015) this.mouseEncoding = on ? p : this.mouseEncoding === p ? null : this.mouseEncoding;
        if (p === 25) this.cursorHidden = !on;
      }
      return false; // let the terminal handle it as well
    };
    this.term.parser.registerCsiHandler({ prefix: '?', final: 'h' }, track(true));
    this.term.parser.registerCsiHandler({ prefix: '?', final: 'l' }, track(false));
  }

  state(): PaneState {
    return { name: this.name, dead: this.dead, pid: this.pid, exitStatus: this.exitStatus, cols: this.cols, rows: this.rows };
  }

  /** Waits until all output so far is on the copy of the screen. */
  flushed(): Promise<void> {
    return new Promise((r) => this.term.write('', r));
  }

  /** The screen as a viewer needs it: a full reset, the screen and scrollback, then the modes. */
  snapshot(): string {
    let modes = '';
    if (this.term.modes.mouseTrackingMode !== 'none' && this.mouseEncoding) modes += `\x1b[?${this.mouseEncoding}h`;
    if (this.term.modes.sendFocusMode) modes += '\x1b[?1004h';
    if (this.term.modes.applicationCursorKeysMode) modes += '\x1b[?1h';
    if (this.term.modes.applicationKeypadMode) modes += '\x1b=';
    if (this.cursorHidden) modes += '\x1b[?25l';
    return `\x1bc${this.serializer.serialize()}${modes}`;
  }

  /** The visible screen as plain text. */
  capture(): string {
    const b = this.term.buffer.active;
    const lines: string[] = [];
    for (let y = 0; y < this.term.rows; y++) lines.push(b.getLine(b.viewportY + y)?.translateToString(true) ?? '');
    return lines.join('\n');
  }

  resize(cols: number, rows: number): void {
    if (cols === this.cols && rows === this.rows) return;
    this.cols = cols;
    this.rows = rows;
    this.term.resize(cols, rows);
    if (!this.dead) {
      try {
        this.proc?.resize(cols, rows);
      } catch {
        /* exited meanwhile */
      }
    }
  }

  dispose(): void {
    this.term.dispose();
  }
}

/** Variables of this install that sessions must not inherit (a checkout run in a session would take them as its own). */
const HOST_ONLY_ENV = new Set(['TMUX', 'TMUX_PANE', 'AK_HOME', 'AK_APP_DIR', 'AK_VERSION']);

/**
 * Starts the session host on `paths.hostSock`. Throws HostRunningError when another host runs on
 * this home: it holds the home's host lease (`host.lock`, the way `server.lock` guards the server)
 * for as long as it runs, so a host that is only slow to answer is never replaced.
 */
export async function startHost(o: HostOptions): Promise<HostHandle> {
  const log = o.log ?? silentLogger;
  const scrollback = o.scrollback ?? 5000;
  const startedAt = Date.now();
  const panes = new Map<string, Pane>();
  const sockets = new Set<Socket>();

  // A quick refusal for a host that answers; the lease below is what keeps a second one out.
  if (await answers(o.paths.hostSock)) throw new HostRunningError(`a session host is already running on ${o.paths.hostSock}`);
  mkdirSync(o.paths.home, { recursive: true, mode: 0o700 });
  let lock: InstanceLock;
  try {
    lock = await InstanceLock.acquire(o.paths.hostLock, o.paths.home, { version: o.version, port: 0 }, o.lockDeps);
  } catch (e) {
    if (e instanceof InstanceBusyError) {
      throw new HostRunningError(`a session host is already running for ${o.paths.home} (pid ${e.lease.pid} on ${e.lease.host})`);
    }
    throw e;
  }

  const childEnv = (): Record<string, string> => {
    const env: Record<string, string> = {};
    for (const [k, v] of Object.entries(process.env)) if (v !== undefined && !HOST_ONLY_ENV.has(k)) env[k] = v;
    env.TERM = 'xterm-256color';
    env.COLORTERM = 'truecolor';
    return env;
  };

  const sendControl = (v: Viewer, m: HostControl) => v.write(encodeFrame(FRAME_CONTROL, JSON.stringify(m)));

  /** Tells every viewer the pane's size and whether it's the one setting it. */
  const broadcastSize = (p: Pane) => {
    for (const v of p.viewers) sendControl(v, { type: 'size', cols: p.cols, rows: p.rows, active: p.active === v });
  };

  /** Makes `v` the viewer whose size the pane takes. */
  const activate = (p: Pane, v: Viewer) => {
    v.lastActive = Date.now();
    if (p.active === v) return;
    p.active = v;
    if (v.cols && v.rows) p.resize(v.cols, v.rows);
    broadcastSize(p);
  };

  /** Sends a viewer the screen as it is now, then the output that came while it was copied. */
  const resync = (p: Pane, v: Viewer) => {
    v.pending = [];
    v.pendingBytes = 0;
    void p.flushed().then(() => {
      if (v.closed || panes.get(p.name) !== p) return;
      const held = v.pending ?? [];
      v.pending = null;
      v.pendingBytes = 0;
      v.write(encodeFrame(FRAME_DATA, p.snapshot()));
      for (const f of held) v.write(f);
    });
  };

  const output = (p: Pane, data: string) => {
    p.term.write(data);
    if (!p.viewers.size) return;
    const frame = encodeFrame(FRAME_DATA, data);
    for (const v of p.viewers) {
      if (v.lagging) continue;
      if (v.pending) {
        v.pending.push(frame);
        v.pendingBytes += frame.length;
        if (v.pendingBytes > MAX_PENDING) resync(p, v);
        continue;
      }
      v.write(frame);
      // A viewer that can't keep up: stop sending until it drains, then send it the screen
      // afresh. The program never waits on a viewer.
      if (v.sock.writableLength > MAX_BUFFERED) {
        v.lagging = true;
        v.sock.once('drain', () => {
          v.lagging = false;
          if (!v.closed && p.viewers.has(v)) resync(p, v);
        });
      }
    }
  };

  const spawn = (r: Extract<HostRequest, { op: 'spawn' }>): void => {
    if (!r.argv?.length || typeof r.argv[0] !== 'string') throw new Error('spawn needs a command');
    let p = panes.get(r.name);
    if (p && !p.dead) throw Object.assign(new Error(`${r.name} is already running`), { code: 'live' });
    const cols = p?.active?.cols || (r.cols ? clampCols(r.cols) : p?.cols ?? DEFAULT_COLS);
    const rows = p?.active?.rows || (r.rows ? clampRows(r.rows) : p?.rows ?? DEFAULT_ROWS);
    // Start the program before the pane counts as live: a failed start must not leave one behind.
    const proc = pty.spawn(r.argv[0], r.argv.slice(1), { name: 'xterm-256color', cols, rows, cwd: r.cwd, env: childEnv() });
    if (!p) {
      p = new Pane(r.name, cols, rows, scrollback);
      panes.set(r.name, p);
    } else {
      p.reset();
      p.resize(cols, rows);
    }
    const pane = p;
    pane.proc = proc;
    pane.dead = false;
    pane.exitStatus = null;
    pane.pid = proc.pid;
    proc.onData((d) => {
      if (pane.proc === proc) output(pane, d);
    });
    proc.onExit(({ exitCode, signal }) => {
      if (pane.proc !== proc) return;
      pane.dead = true;
      pane.exitStatus = signal ? 128 + signal : exitCode;
      log.info(`${pane.name} exited (${pane.exitStatus})`);
    });
    // Viewers of a pane that is started again see it from a clean screen.
    for (const v of pane.viewers) resync(pane, v);
    log.info(`${r.name} started (pid ${proc.pid})`);
  };

  const kill = (name: string, reason: 'killed' | 'stopped' = 'killed') => {
    const p = panes.get(name);
    if (!p) return;
    panes.delete(name);
    const proc = p.proc;
    p.proc = null;
    if (proc && !p.dead) {
      try {
        proc.kill('SIGHUP');
      } catch {
        /* exited meanwhile */
      }
    }
    for (const v of p.viewers) {
      sendControl(v, { type: 'exit', reason });
      v.closed = true;
      v.sock.end();
    }
    p.viewers.clear();
    p.dispose();
  };

  const paste = (p: Pane, text: string, enter: boolean): Promise<void> => {
    const run = p.pasting.then(async () => {
      await p.flushed();
      // A bracketed paste can't be ended early by the text itself.
      // eslint-disable-next-line no-control-regex
      const clean = text.replace(/\x1b\[20[01]~/g, '');
      p.proc?.write(p.term.modes.bracketedPasteMode ? `\x1b[200~${clean}\x1b[201~` : clean);
      if (enter) {
        await new Promise((r) => setTimeout(r, PASTE_ENTER_MS));
        p.proc?.write('\r');
      }
    });
    p.pasting = run.catch(() => undefined);
    return run;
  };

  const need = (name: unknown): Pane => {
    const p = typeof name === 'string' ? panes.get(name) : undefined;
    if (!p) throw Object.assign(new Error(`no pane named ${String(name)}`), { code: 'missing' });
    return p;
  };

  const attach = (sock: Socket, r: Extract<HostRequest, { op: 'attach' }>, rest: Buffer) => {
    const p = need(r.name);
    const v = new Viewer(sock);
    if (Number.isFinite(r.cols) && Number.isFinite(r.rows)) {
      v.cols = clampCols(r.cols!);
      v.rows = clampRows(r.rows!);
    }
    p.viewers.add(v);
    sock.write(JSON.stringify({ ok: true, protocol: PROTOCOL }) + '\n');
    // The newest viewer takes the size: it's the one the user just opened.
    if (v.cols) activate(p, v);
    else sendControl(v, { type: 'size', cols: p.cols, rows: p.rows, active: false });
    resync(p, v);
    const reader = new FrameReader((type, payload) => {
      if (panes.get(p.name) !== p) return;
      if (type === FRAME_DATA) {
        activate(p, v);
        if (!p.dead) p.proc?.write(payload.toString('utf8'));
      } else if (type === FRAME_CONTROL) {
        const m = parseControl<ViewerControl>(payload);
        if (m?.type === 'resize' && Number.isFinite(m.cols) && Number.isFinite(m.rows)) {
          v.cols = clampCols(m.cols);
          v.rows = clampRows(m.rows);
          if (p.active === v) {
            p.resize(v.cols, v.rows);
            broadcastSize(p);
          }
        } else if (m?.type === 'active') activate(p, v);
      }
    });
    const onData = (c: Buffer) => {
      try {
        reader.push(c);
      } catch {
        sock.destroy();
      }
    };
    sock.on('data', onData);
    if (rest.length) onData(rest);
    sock.on('close', () => {
      v.closed = true;
      p.viewers.delete(v);
      if (p.active !== v) return;
      // The viewer used last before it takes over the size.
      p.active = null;
      const next = [...p.viewers].sort((a, b) => b.lastActive - a.lastActive)[0];
      if (next) activate(p, next);
    });
  };

  let stopping = false;
  const handle = async (sock: Socket, r: HostRequest, rest: Buffer): Promise<Record<string, unknown> | null> => {
    switch (r.op) {
      case 'hello':
        return { version: o.version, pid: process.pid, startedAt };
      case 'list':
        return { panes: [...panes.values()].map((p) => p.state()) };
      case 'spawn':
        spawn(r);
        return {};
      case 'kill':
        kill(r.name);
        return {};
      case 'write': {
        const p = need(r.name);
        if (!p.dead) p.proc?.write(String(r.data));
        return {};
      }
      case 'paste':
        await paste(need(r.name), String(r.text), r.enter !== false);
        return {};
      case 'capture': {
        const p = need(r.name);
        await p.flushed();
        return { text: p.capture() };
      }
      case 'stop':
        stopping = true;
        for (const name of [...panes.keys()]) kill(name, 'stopped');
        return {};
      case 'attach':
        attach(sock, r, rest);
        return null;
      default:
        throw new Error(`unknown request ${(r as { op?: unknown }).op as string}`);
    }
  };

  const server: Server = createServer((sock) => {
    sockets.add(sock);
    sock.on('close', () => sockets.delete(sock));
    sock.on('error', () => undefined);
    const reply = (m: Record<string, unknown>) => {
      if (!sock.destroyed) sock.end(JSON.stringify({ protocol: PROTOCOL, ...m }) + '\n');
    };
    const feed = lineReader(MAX_REQUEST, (line, rest) => {
      sock.off('data', feed);
      let r: HostRequest;
      try {
        r = JSON.parse(line) as HostRequest;
        if (!r || typeof r !== 'object' || typeof r.op !== 'string') throw new Error('not a request');
      } catch {
        reply({ ok: false, error: 'malformed request' });
        return;
      }
      handle(sock, r, rest).then(
        (out) => {
          if (out) reply({ ok: true, ...out });
          if (r.op === 'stop') setImmediate(() => o.onStop?.());
        },
        (e: unknown) => reply({ ok: false, error: errMsg(e), code: (e as { code?: string }).code }),
      );
    }, () => sock.destroy());
    sock.on('data', feed);
  });
  await listenPrivate(server, o.paths.hostSock, 'the session host socket');
  const sockFile = socketId(o.paths.hostSock);
  mkdirSync(dirname(o.paths.hostState), { recursive: true });
  writeFileSync(o.paths.hostState, JSON.stringify({ pid: process.pid, version: o.version, appDir: o.appDir ?? null, protocol: PROTOCOL, startedAt }) + '\n');
  log.info(`session host ${o.version} listening on ${o.paths.hostSock}`);

  let closed = false;
  const host: HostHandle = {
    async close() {
      if (closed) return;
      closed = true;
      clearInterval(beat);
      if (!stopping) for (const name of [...panes.keys()]) kill(name, 'stopped');
      await new Promise<void>((r) => {
        server.close(() => r());
        for (const s of sockets) s.destroy();
      });
      // Only our own files: another host may have taken the socket since.
      if (socketId(o.paths.hostSock) === sockFile) rmSync(o.paths.hostSock, { force: true });
      try {
        if ((JSON.parse(readFileSync(o.paths.hostState, 'utf8')) as { pid?: number; startedAt?: number }).startedAt === startedAt) rmSync(o.paths.hostState, { force: true });
      } catch {
        /* gone already */
      }
      lock.release();
    },
  };
  const beat = setInterval(() => {
    if (lock.beat()) return;
    // This host stalled past the lease and another one took the home: its Claudes are out of the
    // server's reach, and would run beside the ones recovery starts there. End them.
    log.error(`another session host took over ${o.paths.home}; ending this one`);
    void host.close().finally(() => o.onStop?.());
  }, BEAT_MS);
  beat.unref();
  return host;
}

/** True when something answers a hello on the socket. */
function answers(sock: string): Promise<boolean> {
  return new Promise((resolve) => {
    const c = createConnection(sock);
    let buf = '';
    const done = (v: boolean) => {
      c.destroy();
      resolve(v);
    };
    c.setTimeout(2000, () => done(false));
    c.on('error', () => done(false));
    c.on('connect', () => c.write(JSON.stringify({ op: 'hello' }) + '\n'));
    c.on('data', (d) => {
      buf += d.toString('utf8');
      if (buf.includes('\n')) done(true);
    });
    c.on('end', () => done(buf.length > 0));
  });
}
