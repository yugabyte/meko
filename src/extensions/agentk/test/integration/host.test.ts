import { execFileSync, spawn } from 'node:child_process';
import { existsSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { createServer, type Socket } from 'node:net';
import { hostname } from 'node:os';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';
import pty from 'node-pty';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { CLOSE_WAIT_MS, startHost, type HostHandle } from '../../src/server/host/host.js';
import { MIN_PROTOCOL, type HostControl } from '../../src/server/host/protocol.js';
import { HostRequestError, NoHostError, SessionHost, type HostStream } from '../../src/server/hostclient.js';
import { getPaths, type Paths } from '../../src/server/paths.js';
import type { ServiceManager } from '../../src/server/service.js';
import { tempDir, waitFor } from '../helpers.js';

/** A viewer attached to a pane: everything it was sent, as text, and its size messages. */
async function viewer(host: SessionHost, name: string, size: { cols?: number; rows?: number } = {}) {
  const stream = await host.attach(name, size);
  const v = { stream, out: '', sizes: [] as Extract<HostControl, { type: 'size' }>[], exits: [] as string[], closed: false };
  stream.onData((d) => (v.out += d.toString('utf8')));
  stream.onControl((m) => (m.type === 'size' ? v.sizes.push(m) : v.exits.push(m.reason)));
  stream.onClose(() => (v.closed = true));
  return v;
}

const lastSize = (v: { sizes: Extract<HostControl, { type: 'size' }>[] }) => v.sizes.at(-1);

describe('session host (real ptys)', () => {
  let home: string;
  let paths: Paths;
  let handle: HostHandle | null;
  let host: SessionHost;
  const streams: HostStream[] = [];

  beforeEach(async () => {
    home = tempDir('akl-host-');
    paths = getPaths(join(home, 'ak'));
    handle = await startHost({ paths, version: '1.2.3', appDir: join(home, 'app', '1.2.3') });
    host = new SessionHost(paths, { services: () => null });
  });
  afterEach(async () => {
    for (const s of streams.splice(0)) s.close();
    await handle?.close();
    rmSync(home, { recursive: true, force: true });
  });

  it('starts, lists, captures and ends panes, keeping a dead one with its exit status', async () => {
    expect(await host.serverRunning()).toBe(true);
    expect(host.hello).toMatchObject({ version: '1.2.3', outdated: false });
    expect(JSON.parse(readFileSync(paths.hostState, 'utf8'))).toMatchObject({ version: '1.2.3', appDir: join(home, 'app', '1.2.3'), pid: process.pid });

    await host.spawn('ak-a', home, ['/bin/sh', '-c', 'printf "hello from a\\n"; sleep 30']);
    await host.spawn('ak-b', home, ['/bin/sh', '-c', 'exit 3']);
    expect(await waitFor(() => host.capture('ak-a'), (t) => t.includes('hello from a'))).toMatch(/^hello from a/);
    const panes = await waitFor(() => host.panes(), (p) => p.get('ak-b')?.dead === true);
    expect(panes.get('ak-a')).toMatchObject({ dead: false });
    expect(panes.get('ak-b')).toMatchObject({ dead: true, exitStatus: 3 });

    // Never replaces a live pane; a dead one starts again in place.
    await expect(host.spawn('ak-a', home, ['/bin/sh', '-c', 'true'])).rejects.toMatchObject({ code: 'live' });
    await host.spawn('ak-b', home, ['/bin/sh', '-c', 'sleep 30']);
    expect((await host.panes()).get('ak-b')).toMatchObject({ dead: false, exitStatus: null });

    await host.kill('ak-a');
    expect((await host.panes()).has('ak-a')).toBe(false);
    await expect(host.capture('ak-a')).rejects.toBeInstanceOf(HostRequestError);
  });

  it("runs a program with a color terminal, in the directory it was given, without tmux's or this install's variables", async () => {
    const vars = { TMUX: '/tmp/tmux-1/default,1,0', AK_APP_DIR: '/opt/ak/app/1.2.3', AK_VERSION: '1.2.3', AK_HOME: '/opt/ak' };
    Object.assign(process.env, vars);
    try {
      await host.spawn('ak-env', home, ['/bin/sh', '-c', 'printf "%s|%s|%s|%s|%s|%s|%s\\n" "$TERM" "$COLORTERM" "${TMUX:-none}" "${AK_APP_DIR:-none}" "${AK_VERSION:-none}" "${AK_HOME:-none}" "$(pwd)"; sleep 30']);
    } finally {
      for (const k of Object.keys(vars)) delete process.env[k];
    }
    expect(await waitFor(() => host.capture('ak-env'), (t) => t.includes('|'))).toContain(`xterm-256color|truecolor|none|none|none|none|${home}`);
  });

  it('leaves no pane behind when its program fails to start, so the session can be started again', async () => {
    const spawnSpy = vi.spyOn(pty, 'spawn').mockImplementationOnce(() => {
      throw new Error('forkpty(3) failed');
    });
    try {
      await expect(host.spawn('ak-fail', home, ['/bin/sh', '-c', 'sleep 30'])).rejects.toThrow(/forkpty/);
    } finally {
      spawnSpy.mockRestore();
    }
    expect((await host.panes()).has('ak-fail')).toBe(false);
    await host.spawn('ak-fail', home, ['/bin/sh', '-c', 'sleep 30']);
    expect((await host.panes()).get('ak-fail')).toMatchObject({ dead: false });
  });

  it("starts a program again from a clean screen, without the last one's hidden cursor", async () => {
    await host.spawn('ak-again', home, ['/bin/sh', '-c', "printf '\\033[?25l\\033[?1000h\\033[?1006hbye'"]);
    await waitFor(() => host.panes(), (p) => p.get('ak-again')?.dead === true);
    await host.spawn('ak-again', home, ['/bin/sh', '-c', 'printf "hello again"; sleep 30']);
    await waitFor(() => host.capture('ak-again'), (t) => t.includes('hello again'));
    const v = await viewer(host, 'ak-again', { cols: 80, rows: 24 });
    streams.push(v.stream);
    await waitFor(() => v.out, (o) => o.includes('hello again'));
    expect(v.out).not.toContain('\x1b[?25l');
    expect(v.out).not.toContain('\x1b[?1006h');
    expect(v.out).not.toContain('bye');
  });

  it('pastes two prompts at once without either one being lost, bracketed only when the program asked for it', async () => {
    const out = join(home, 'typed');
    // Raw mode, like Claude's TUI, so the bracketed paste markers arrive as typed.
    await host.spawn('ak-paste', home, ['/bin/sh', '-c', `stty raw -echo; printf '\\033[?2004h'; cat > ${out}`]);
    await waitFor(() => host.capture('ak-paste'), () => true);
    await new Promise((r) => setTimeout(r, 200));
    await Promise.all([host.submit('ak-paste', 'first prompt'), host.submit('ak-paste', 'second prompt')]);
    const typed = await waitFor(() => (existsSync(out) ? readFileSync(out, 'utf8') : ''), (s) => s.includes('second prompt\x1b[201~\r'), 5000, 'both pastes');
    expect(typed).toBe('\x1b[200~first prompt\x1b[201~\r\x1b[200~second prompt\x1b[201~\r');

    const plain = join(home, 'plain');
    await host.spawn('ak-plain', home, ['/bin/sh', '-c', `stty raw -echo; cat > ${plain}`]);
    await new Promise((r) => setTimeout(r, 200));
    await host.submit('ak-plain', 'a prompt with \x1b[201~ inside');
    await host.sendKeys('ak-plain', 'Escape');
    expect(await waitFor(() => (existsSync(plain) ? readFileSync(plain, 'utf8') : ''), (s) => s.endsWith('\x1b'))).toBe('a prompt with  inside\r\x1b');
  });

  it("answers the program's terminal queries itself, with no viewer attached", async () => {
    const out = join(home, 'answer');
    await host.spawn('ak-query', home, ['/bin/sh', '-c', `stty raw -echo; printf '\\033[c'; dd bs=1 count=6 of=${out} 2>/dev/null; sleep 30`]);
    // eslint-disable-next-line no-control-regex
    expect(await waitFor(() => (existsSync(out) ? readFileSync(out, 'utf8') : ''), (s) => s.length >= 6)).toMatch(/^\x1b\[\?/);
  });

  it('shows a new viewer the screen as it is, then what follows; the newest viewer sets the size until another is used', async () => {
    await host.spawn('ak-v', home, ['/bin/sh', '-c', 'printf "before\\n"; stty raw -echo; while :; do read -r -n 1 c 2>/dev/null || c=$(dd bs=1 count=1 2>/dev/null); printf "typed:%s\\n" "$c"; done']);
    await waitFor(() => host.capture('ak-v'), (t) => t.includes('before'));

    const desk = await viewer(host, 'ak-v', { cols: 120, rows: 40 });
    streams.push(desk.stream);
    await waitFor(() => desk.out, (o) => o.startsWith('\x1bc') && o.includes('before'), 5000, 'screen copy');
    expect(lastSize(desk)).toEqual({ type: 'size', cols: 120, rows: 40, active: true });

    const phone = await viewer(host, 'ak-v', { cols: 40, rows: 20 });
    streams.push(phone.stream);
    await waitFor(() => lastSize(desk), (s) => s?.active === false, 5000, 'desktop hands over');
    expect(lastSize(desk)).toEqual({ type: 'size', cols: 40, rows: 20, active: false });
    expect(lastSize(phone)).toEqual({ type: 'size', cols: 40, rows: 20, active: true });

    // Typing on the desktop takes the size back, and reaches the program.
    desk.stream.write('x');
    await waitFor(() => lastSize(desk), (s) => s?.active === true, 5000, 'desktop active again');
    expect(lastSize(phone)).toEqual({ type: 'size', cols: 120, rows: 40, active: false });
    await waitFor(() => phone.out, (o) => o.includes('typed:x'), 5000, 'both see the output');

    // A resize of an inactive viewer changes nothing; `active` makes it take over.
    phone.stream.resize(50, 25);
    await new Promise((r) => setTimeout(r, 100));
    expect(lastSize(desk)).toMatchObject({ cols: 120, rows: 40 });
    phone.stream.active();
    await waitFor(() => lastSize(desk), (s) => s?.cols === 50 && s.rows === 25, 5000, 'phone active');

    // When the active viewer leaves, the one used before it takes over.
    phone.stream.close();
    await waitFor(() => lastSize(desk), (s) => s?.active === true && s.cols === 120, 5000, 'desktop back');
  });

  it("never makes the program wait on a viewer that stopped reading, and sends it the screen afresh once it reads again", async () => {
    await host.spawn('ak-yes', home, ['/bin/sh', '-c', 'yes']);
    const slow = await viewer(host, 'ak-yes', { cols: 80, rows: 24 });
    streams.push(slow.stream);
    slow.stream.pause();
    const fast = await viewer(host, 'ak-yes', { cols: 80, rows: 24 });
    streams.push(fast.stream);
    // The other viewer keeps getting output while the slow one is stalled. Everything it got was
    // written to the slow one too, less what the socket's kernel buffers took (a few hundred KB on
    // Linux): well past the host's 1 MB, the slow one is surely skipped by now.
    const seen = fast.out.length;
    await waitFor(() => fast.out.length, (n) => n > seen + 3 * 1024 * 1024, 10000, 'output keeps flowing');
    const before = slow.out.length;
    slow.stream.resume();
    // It catches up from a fresh screen copy, not from everything it missed.
    await waitFor(() => slow.out.slice(before), (o) => o.includes('\x1bc'), 10000, 'fresh screen');
  });

  it('tells viewers when their pane ends, and ends every pane when stopped', async () => {
    await host.spawn('ak-k', home, ['/bin/sh', '-c', 'sleep 30']);
    const v = await viewer(host, 'ak-k');
    await host.kill('ak-k');
    await waitFor(() => v.closed, (c) => c);
    expect(v.exits).toEqual(['killed']);
    await expect(host.attach('ak-missing')).rejects.toMatchObject({ code: 'missing' });

    await host.spawn('ak-s', home, ['/bin/sh', '-c', 'sleep 30']);
    await host.stop();
    expect((await host.panes()).size).toBe(0);
  });

  it("refuses to start while another host holds the home's lease, even one that doesn't answer", async () => {
    await handle!.close();
    handle = null;
    // A host elsewhere (a home shared over NFS), or one too busy to answer: its lease still beats.
    writeFileSync(paths.hostLock, JSON.stringify({ host: `not-${hostname()}`, pid: 4242, version: 'x', port: 0, startedAt: Date.now(), beatAt: Date.now() }));
    await expect(startHost({ paths, version: 'other' })).rejects.toThrow(/already running for .* \(pid 4242 on not-/);
    rmSync(paths.hostLock);
    handle = await startHost({ paths, version: '1.2.3' });
    expect(JSON.parse(readFileSync(paths.hostLock, 'utf8'))).toMatchObject({ pid: process.pid });
    await handle.close();
    handle = null;
    expect(existsSync(paths.hostLock)).toBe(false);
  });

  it('waits on close for the programs it ended to exit, at most CLOSE_WAIT_MS, and leaves one that ignores SIGHUP running', { timeout: 20000 }, async () => {
    // Like a Claude still writing after SIGHUP (its last transcript lines): this one writes 300 ms later.
    const node = (name: string, onHup: string) => {
      const ready = join(home, `${name}.ready`);
      const script = `const fs = require('node:fs'); process.on('SIGHUP', () => { ${onHup} }); fs.writeFileSync(${JSON.stringify(ready)}, String(process.pid)); setInterval(() => undefined, 1000);`;
      return { argv: [process.execPath, '-e', script], ready };
    };
    const after = join(home, 'after-hup');
    const slow = node('slow', `setTimeout(() => { fs.writeFileSync(${JSON.stringify(after)}, 'x'); process.exit(0); }, 300);`);
    await host.spawn('ak-slow', home, slow.argv);
    await waitFor(() => existsSync(slow.ready), (x) => x, 10000, 'the program to start');
    // A second close (a SIGTERM during a stop request's) waits for the first one's end too.
    const first = handle!.close();
    await handle!.close();
    handle = null;
    expect(existsSync(after)).toBe(true);
    expect(existsSync(paths.hostLock)).toBe(false);
    await first;

    handle = await startHost({ paths, version: '1.2.3' });
    const deaf = node('deaf', '/* ignored */');
    await host.spawn('ak-deaf', home, deaf.argv);
    const pid = Number(await waitFor(() => (existsSync(deaf.ready) ? readFileSync(deaf.ready, 'utf8') : ''), (x) => !!x, 10000, 'the program to start'));
    const t0 = Date.now();
    await handle.close();
    handle = null;
    try {
      expect(Date.now() - t0).toBeGreaterThanOrEqual(CLOSE_WAIT_MS - 100);
      expect(Date.now() - t0).toBeLessThan(CLOSE_WAIT_MS + 2000);
      expect(() => process.kill(pid, 0)).not.toThrow();
    } finally {
      process.kill(pid, 'SIGKILL');
    }
  });

  it('takes only "nothing listening" for no host: a host that is slow to answer is never started again', async () => {
    await handle!.close();
    handle = null;
    // Something on the socket that never answers, as a host stuck for a while would.
    const conns: Socket[] = [];
    const stuck = createServer((c) => void conns.push(c));
    await new Promise<void>((r) => stuck.listen(paths.hostSock, () => r()));
    let started = 0;
    const client = new SessionHost(paths, { services: () => null, start: () => void started++ });
    try {
      await expect(client.serverRunning()).rejects.toThrow(/didn't answer hello in time/);
      await expect(client.ensureServer()).rejects.toThrow(/didn't answer hello in time/);
      expect(started).toBe(0);
    } finally {
      for (const c of conns) c.destroy();
      await new Promise<void>((r) => stuck.close(() => r()));
    }
  }, 20_000);

  it('starts one host for calls that come at the same time', async () => {
    await handle!.close();
    handle = null;
    let started = 0;
    const client = new SessionHost(paths, {
      services: () => null,
      start: async () => {
        started++;
        handle = await startHost({ paths, version: '1.2.3' });
      },
    });
    await Promise.all([client.ensureServer(), client.ensureServer(), client.ensureServer()]);
    expect(started).toBe(1);
  });

  it('lists no panes when no host runs, and never starts its own when the server or the host runs as a service', async () => {
    await handle!.close();
    handle = null;
    expect((await host.panes()).size).toBe(0);
    await expect(host.request({ op: 'list' })).rejects.toBeInstanceOf(NoHostError);
    expect(existsSync(paths.hostState)).toBe(false);

    let started = 0;
    const svc = {
      installed: () => true,
      start: () => void started++,
      id: () => 'agentk-host.service',
      statusHint: () => 'systemctl --user status agentk-host.service',
    } as unknown as ServiceManager;
    const owned = new SessionHost(paths, { services: () => svc, start: () => void (started += 100) });
    await expect(owned.ensureServer()).rejects.toThrow(/agentk-host\.service\) isn't running\. Check: systemctl --user status/);
    expect(started).toBe(1);

    // Nor when only the server runs as a service (adding the host's failed): it would end with the server.
    const serverOnly = { ...svc, installed: (s: string) => s === 'server' } as unknown as ServiceManager;
    const half = new SessionHost(paths, { services: () => serverOnly, start: () => void (started += 100) });
    await expect(half.ensureServer()).rejects.toThrow(/agentk-host\.service\) isn't installed\. Run: me_koder install/);
    expect(started).toBe(1);
  });
});

describe('a session host from an older version', () => {
  it('is reported as outdated and used for nothing until it is restarted', async () => {
    const home = tempDir('akl-oldhost-');
    let changes = 0;
    const host = new SessionHost(getPaths(home), { services: () => null, onHello: () => void changes++ });
    host.request = (async (r: { op: string }) => {
      if (r.op === 'hello') return { ok: true, protocol: MIN_PROTOCOL - 1, version: '0.20261001.1', pid: 1, startedAt: 0 };
      throw new Error(`unexpected ${r.op}`);
    }) as SessionHost['request'];
    await expect(host.ensureServer()).rejects.toThrow(/version 0\.20261001\.1\) is too old for this server\. Restart it with: me_koder host restart/);
    expect(host.hello).toMatchObject({ version: '0.20261001.1', outdated: true });
    expect(changes).toBe(1);
    rmSync(home, { recursive: true, force: true });
  });
});

describe('me_koder host, attach, stop and start (the commands)', () => {
  const cli = fileURLToPath(new URL('../../src/server/cli.ts', import.meta.url));
  const tsx = fileURLToPath(new URL('../../node_modules/.bin/tsx', import.meta.url));
  let home: string;
  beforeEach(() => {
    home = tempDir('akl-hostcli-');
  });
  afterEach(() => {
    try {
      execFileSync(tsx, [cli, 'host', 'stop'], { env: { ...process.env, AK_HOME: home }, stdio: 'ignore' });
    } catch {
      /* not running */
    }
    rmSync(home, { recursive: true, force: true });
  });

  it('runs a host that outlives its starter, refuses a second one, attaches a terminal, and stops on request', async () => {
    const env = { ...process.env, AK_HOME: home };
    const p = spawn(tsx, [cli, 'host'], { env, detached: true, stdio: 'ignore' });
    p.unref();
    const host = new SessionHost(getPaths(home), { services: () => null });
    await waitFor(() => host.serverRunning(), (up) => up, 20000, 'host up');

    let code: number | null = null;
    try {
      execFileSync(tsx, [cli, 'host'], { env, stdio: 'pipe' });
    } catch (e) {
      code = (e as { status: number }).status;
    }
    expect(code).toBe(75);

    const typed = join(home, 'typed');
    await host.spawn('ak-demo--fix-it', home, ['/bin/sh', '-c', `printf "pane says hi\n"; stty raw -echo; cat > ${typed}`]);
    expect(execFileSync(tsx, [cli, 'attach'], { env, encoding: 'utf8' })).toContain('  ak-demo--fix-it\n');

    // Attached by its slug in a terminal: the screen shows, and Ctrl+\ detaches, leaving it running.
    const term = pty.spawn(tsx, [cli, 'attach', 'fix-it'], { cols: 100, rows: 30, cwd: home, env });
    let out = '';
    term.onData((d) => (out += d));
    const exited = new Promise<number>((r) => term.onExit(({ exitCode }) => r(exitCode)));
    await waitFor(() => out, (o) => o.includes('pane says hi'), 20000, 'the pane on screen');
    // The terminal's own answer to a query is dropped (the host answered it); typing goes through.
    term.write('\x1b[?1;2c');
    await new Promise((r) => setTimeout(r, 300));
    term.write('typed');
    await waitFor(() => (existsSync(typed) ? readFileSync(typed, 'utf8') : ''), (t) => t === 'typed', 10000, 'the typing');
    term.write('\x1c');
    expect(await exited).toBe(0);
    expect(out).toContain('Detached from ak-demo--fix-it. Claude keeps running.');
    expect((await host.panes()).get('ak-demo--fix-it')?.dead).toBe(false);

    // While a server runs on this home, stopping the host would only see every session resumed.
    const lock = join(home, 'server.lock');
    writeFileSync(lock, JSON.stringify({ host: hostname(), pid: process.pid, version: 'x', port: 7777, startedAt: Date.now(), beatAt: Date.now() }));
    let refused = '';
    try {
      execFileSync(tsx, [cli, 'host', 'stop'], { env, stdio: 'pipe' });
    } catch (e) {
      refused = String((e as { stderr: Buffer }).stderr);
    }
    expect(refused).toContain('The AgentK server for this home is running');
    expect((await host.panes()).size).toBe(1);

    // `stop` can't signal a server on another host sharing this home.
    writeFileSync(lock, JSON.stringify({ host: 'elsewhere', pid: 1, version: 'x', port: 7777, startedAt: Date.now(), beatAt: Date.now() }));
    refused = '';
    try {
      execFileSync(tsx, [cli, 'stop'], { env, stdio: 'pipe' });
    } catch (e) {
      refused = String((e as { stderr: Buffer }).stderr);
    }
    expect(refused).toContain('runs on elsewhere (pid 1): stop it there');
    expect((await host.panes()).size).toBe(1);

    // A lease whose live pid isn't a server (left by a crash, the pid since reused: here, this test) isn't signalled.
    writeFileSync(lock, JSON.stringify({ host: hostname(), pid: process.pid, version: 'x', port: 7777, startedAt: Date.now(), beatAt: Date.now() }));
    expect(execFileSync(tsx, [cli, 'stop'], { env, encoding: 'utf8' })).toContain('Stopped AgentK and the 1 session(s) in it. They resume at: me_koder serve');
    await waitFor(() => host.serverRunning(), (up) => !up, 10000, 'host down');
    rmSync(lock);
    expect(execFileSync(tsx, [cli, 'stop'], { env, encoding: 'utf8' })).toContain('AgentK is not running.');
    // Run by meko stop, one row of Meko's summary.
    expect(execFileSync(tsx, [cli, 'stop'], { env: { ...env, MEKO_SUMMARY: '1' }, encoding: 'utf8' })).toBe("  ✓ AgentK          wasn't running\n");

    // Without services there is nothing for `start` to start.
    refused = '';
    try {
      execFileSync(tsx, [cli, 'start'], { env, stdio: 'pipe' });
    } catch (e) {
      refused = String((e as { stderr: Buffer }).stderr);
    }
    expect(refused).toContain('No background services run this home');
  }, 60000);
});
