import { spawn } from 'node:child_process';
import { createConnection, type Socket } from 'node:net';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import {
  encodeFrame, FRAME_CONTROL, FRAME_DATA, FrameReader, lineReader, MIN_PROTOCOL, parseControl,
  type HelloReply, type HostControl, type HostRequest, type PaneState, type ViewerControl,
} from './host/protocol.js';
import type { Paths } from './paths.js';
import { hostServices, type ServiceManager } from './service.js';

export interface PaneInfo {
  session: string;
  dead: boolean;
  pid: number;
  exitStatus: number | null;
}

/** No session host answers on the socket (not running, or its socket is gone). */
export class NoHostError extends Error {}

/** What the host said when it refused a request; `code` is 'live' or 'missing' when it gave one. */
export class HostRequestError extends Error {
  constructor(message: string, readonly code?: string) {
    super(message);
  }
}

/** How long a request waits for its answer (a paste includes its pause before Enter). */
const REQUEST_MS = 30_000;
/** Keys by name (Enter, Escape, …), for the few keys AgentK presses itself. */
const KEYS: Record<string, string> = { Enter: '\r', Escape: '\x1b', Tab: '\t', Up: '\x1b[A', Down: '\x1b[B' };

/** A viewer's connection to a pane (the host's side is its `Viewer` and `attach()`). */
export interface HostStream {
  onData(fn: (data: Buffer) => void): void;
  onControl(fn: (m: HostControl) => void): void;
  /** The stream ended: the pane was killed, the host stopped, or the connection dropped. */
  onClose(fn: () => void): void;
  write(data: string): void;
  resize(cols: number, rows: number): void;
  /** Takes over the pane's size: the viewer was just used. */
  active(): void;
  pause(): void;
  resume(): void;
  close(): void;
}

/**
 * The server's side of the session host (host/host.ts). Every method opens its own connection,
 * so a failure never leaves a half-read stream behind. "No host" is told apart from any other
 * failure, so callers never mistake a hiccup for every pane being gone.
 */
export class SessionHost {
  /** The running host's hello, once one answered. */
  hello: (HelloReply & { outdated: boolean }) | null = null;

  constructor(
    private paths: Paths,
    private opts: {
      services?: () => ServiceManager | null;
      /** Starts a host when no service owns it. By default a detached `me_koder host`. */
      start?: () => Promise<void> | void;
      /** Called when the host's version or whether it's outdated changes (the UI's banner). */
      onHello?: () => void;
    } = {},
  ) {}

  /** Sends one request and returns the host's answer; throws NoHostError or HostRequestError. */
  async request<T extends Record<string, unknown> = Record<string, unknown>>(r: HostRequest, timeoutMs = REQUEST_MS): Promise<T> {
    return new Promise<T>((resolve, reject) => {
      const c = createConnection(this.paths.hostSock);
      let settled = false;
      const finish = (e: Error | null, v?: T) => {
        if (settled) return;
        settled = true;
        c.destroy();
        if (e) reject(e);
        else resolve(v!);
      };
      c.setTimeout(timeoutMs, () => finish(new Error(`the session host didn't answer ${r.op} in time`)));
      c.on('error', (e: NodeJS.ErrnoException) =>
        finish(e.code === 'ENOENT' || e.code === 'ECONNREFUSED' ? new NoHostError(`no session host on ${this.paths.hostSock}`) : e));
      c.on('connect', () => c.write(JSON.stringify(r) + '\n'));
      const feed = lineReader(64 * 1024 * 1024, (line) => {
        try {
          const m = JSON.parse(line) as { ok?: boolean; error?: string; code?: string } & T;
          if (m.ok) finish(null, m);
          else finish(new HostRequestError(m.error ?? 'the session host refused the request', m.code));
        } catch {
          finish(new Error('the session host sent a malformed answer'));
        }
      }, () => finish(new Error('the session host sent an answer too large to read')));
      c.on('data', feed);
      c.on('end', () => finish(new Error(`the session host closed the connection during ${r.op}`)));
    });
  }

  /**
   * True when a host answers, false when none runs (no socket, or nothing listening on it). Any
   * other failure (a timeout, a malformed answer) throws: a host that is only slow must never be
   * taken for a missing one, or recovery would start every Claude a second time.
   */
  async serverRunning(): Promise<boolean> {
    try {
      await this.sayHello();
      return true;
    } catch (e) {
      if (e instanceof NoHostError) return false;
      throw e;
    }
  }

  private async sayHello(): Promise<HelloReply> {
    const h = await this.request<HelloReply & Record<string, unknown>>({ op: 'hello' }, 5000);
    const before = this.hello;
    this.hello = { protocol: h.protocol, version: h.version, pid: h.pid, startedAt: h.startedAt, outdated: h.protocol < MIN_PROTOCOL };
    if (before?.version !== this.hello.version || before?.outdated !== this.hello.outdated) this.opts.onHello?.();
    return h;
  }

  /**
   * Makes sure a host is up. When the host service is installed it owns the host (so server
   * restarts never end sessions): start the service, never a host of our own, which would run in
   * the server's service and end with it. Otherwise start one detached. Calls at the same time
   * share one start.
   */
  ensureServer(): Promise<void> {
    this.starting ??= this.startServer().finally(() => {
      this.starting = null;
    });
    return this.starting;
  }

  private starting: Promise<void> | null = null;

  private async startServer(): Promise<void> {
    if (await this.serverRunning()) return this.checkProtocol();
    const svc = (this.opts.services ?? hostServices)();
    if (svc?.installed('host')) {
      svc.start('host');
      if (await this.waitUp(3000)) return this.checkProtocol();
      throw new Error(`The session host service (${svc.id('host')}) isn't running. Check: ${svc.statusHint('host')}`);
    }
    // A server running as a service without the host's: a host started here would run in the
    // server's service and end, with every Claude in it, at the next server restart or update.
    if (svc?.installed('server')) {
      throw new Error(`The session host service (${svc.id('host')}) isn't installed. Run: me_koder install`);
    }
    await (this.opts.start ?? (() => startDetached(this.paths)))();
    if (!(await this.waitUp(10_000))) throw new Error(`The session host didn't start. Check ${join(this.paths.logs, 'host.log')}`);
    return this.checkProtocol();
  }

  private checkProtocol(): void {
    if (this.hello?.outdated) {
      throw new Error(`The session host (version ${this.hello.version}) is too old for this server. Restart it with: me_koder host restart`);
    }
  }

  private async waitUp(ms: number): Promise<boolean> {
    for (const end = Date.now() + ms; Date.now() < end; await sleep(100)) {
      if (await this.serverRunning().catch(() => false)) return true;
    }
    return false;
  }

  /** Every pane. Empty when no host runs; any other failure throws. */
  async panes(): Promise<Map<string, PaneInfo>> {
    const out = new Map<string, PaneInfo>();
    let list: PaneState[];
    try {
      list = (await this.request<{ panes: PaneState[] }>({ op: 'list' })).panes;
    } catch (e) {
      if (e instanceof NoHostError) return out;
      throw e;
    }
    for (const p of list) out.set(p.name, { session: p.name, dead: p.dead, pid: p.pid, exitStatus: p.exitStatus });
    return out;
  }

  /** Starts `argv` in pane `name` (new, or in place of a dead one). Refused while the pane is live. */
  async spawn(name: string, cwd: string, argv: string[]): Promise<void> {
    await this.ensureServer();
    await this.request({ op: 'spawn', name, cwd, argv });
  }

  async kill(name: string): Promise<void> {
    await this.request({ op: 'kill', name }).catch(() => undefined);
  }

  /** Pastes text into the pane (bracketed, when the program turned that on), then presses Enter. */
  async submit(name: string, text: string): Promise<void> {
    await this.request({ op: 'paste', name, text, enter: true });
  }

  /** Presses keys by name (Enter, Escape, …); anything else is typed as is. */
  async sendKeys(name: string, ...keys: string[]): Promise<void> {
    await this.request({ op: 'write', name, data: keys.map((k) => KEYS[k] ?? k).join('') });
  }

  /** The pane's visible screen as plain text. */
  async capture(name: string): Promise<string> {
    return (await this.request<{ text: string }>({ op: 'capture', name })).text;
  }

  /** Ends every pane and stops the host. */
  async stop(): Promise<void> {
    await this.request({ op: 'stop' });
  }

  /** Attaches a viewer to a pane, at its size when it gives one. */
  attach(name: string, size: { cols?: number; rows?: number } = {}): Promise<HostStream> {
    return new Promise((resolve, reject) => {
      const c: Socket = createConnection(this.paths.hostSock);
      const dataFns: ((d: Buffer) => void)[] = [];
      const controlFns: ((m: HostControl) => void)[] = [];
      const closeFns: (() => void)[] = [];
      let open = false;
      let closed = false;
      const early: { type: number; payload: Buffer }[] = [];
      const dispatch = (type: number, payload: Buffer) => {
        if (type === FRAME_DATA) dataFns.forEach((f) => f(payload));
        else if (type === FRAME_CONTROL) {
          const m = parseControl<HostControl>(payload);
          if (m) controlFns.forEach((f) => f(m));
        }
      };
      const reader = new FrameReader((type, payload) => {
        // Frames that came with the answer wait for the caller's listeners.
        if (!dataFns.length && !controlFns.length) early.push({ type, payload: Buffer.from(payload) });
        else dispatch(type, payload);
      });
      const send = (type: number, payload: string) => {
        if (!closed && c.writable) c.write(encodeFrame(type, payload));
      };
      const stream: HostStream = {
        onData: (f) => {
          dataFns.push(f);
          flushEarly();
        },
        onControl: (f) => {
          controlFns.push(f);
          flushEarly();
        },
        onClose: (f) => void closeFns.push(f),
        write: (data) => send(FRAME_DATA, data),
        resize: (cols, rows) => send(FRAME_CONTROL, JSON.stringify({ type: 'resize', cols, rows } satisfies ViewerControl)),
        active: () => send(FRAME_CONTROL, JSON.stringify({ type: 'active' } satisfies ViewerControl)),
        pause: () => c.pause(),
        resume: () => c.resume(),
        close: () => c.destroy(),
      };
      const flushEarly = () => {
        if (!dataFns.length || !controlFns.length) return;
        for (const f of early.splice(0)) dispatch(f.type, f.payload);
      };
      c.on('error', (e: NodeJS.ErrnoException) => {
        if (!open) reject(e.code === 'ENOENT' || e.code === 'ECONNREFUSED' ? new NoHostError(`no session host on ${this.paths.hostSock}`) : e);
      });
      c.on('close', () => {
        closed = true;
        if (!open) reject(new Error('the session host closed the connection'));
        else closeFns.forEach((f) => f());
      });
      c.on('connect', () => c.write(JSON.stringify({ op: 'attach', name, cols: size.cols, rows: size.rows } satisfies HostRequest) + '\n'));
      const feed = lineReader(1024 * 1024, (line, rest) => {
        c.off('data', feed);
        let m: { ok?: boolean; error?: string; code?: string };
        try {
          m = JSON.parse(line) as typeof m;
        } catch {
          m = { ok: false, error: 'the session host sent a malformed answer' };
        }
        if (!m.ok) {
          reject(new HostRequestError(m.error ?? "couldn't attach", m.code));
          c.destroy();
          return;
        }
        open = true;
        c.on('data', (d: Buffer) => {
          try {
            reader.push(d);
          } catch {
            c.destroy();
          }
        });
        if (rest.length) reader.push(rest);
        resolve(stream);
      }, () => c.destroy());
      c.on('data', feed);
    });
  }
}

/** Starts `me_koder host` detached from this process, in its own process group. */
export function startDetached(paths: Paths): void {
  const here = fileURLToPath(import.meta.url);
  const cli = join(dirname(here), here.endsWith('.ts') ? 'cli.ts' : 'cli.js');
  const p = spawn(process.execPath, [...process.execArgv, cli, 'host'], {
    detached: true,
    stdio: 'ignore',
    cwd: paths.home,
    env: { ...process.env, AK_HOME: paths.home },
  });
  p.on('error', () => undefined);
  p.unref();
}

export const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

/**
 * A session's pane name. Project ids and slugs never contain "--" (see slugify), so the double
 * dash keeps names of two projects on one repo apart ("api" + "v2-fix" vs. "api-v2" + "fix").
 * Existing sessions keep their stored name.
 */
export function paneName(projectId: string, slug: string): string {
  return `ak-${projectId}--${slug}`.replace(/[.:]/g, '-');
}
