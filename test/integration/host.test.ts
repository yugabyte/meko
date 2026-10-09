import { execFileSync, spawn } from 'node:child_process';
import { existsSync, readFileSync, rmSync } from 'node:fs';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';
import pty from 'node-pty';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { HostRunningError, startHost, type HostHandle } from '../../src/server/host/host.js';
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

  it('runs a program with a color terminal, in the directory it was given, without tmux variables', async () => {
    process.env.TMUX = '/tmp/tmux-1/default,1,0';
    try {
      await host.spawn('ak-env', home, ['/bin/sh', '-c', 'printf "%s|%s|%s|%s\\n" "$TERM" "$COLORTERM" "${TMUX:-none}" "$(pwd)"; sleep 30']);
    } finally {
      delete process.env.TMUX;
    }
    expect(await waitFor(() => host.capture('ak-env'), (t) => t.includes('|'))).toContain(`xterm-256color|truecolor|none|${home}`);
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
    // The other viewer keeps getting output while the slow one is stalled.
    const seen = fast.out.length;
    await waitFor(() => fast.out.length, (n) => n > seen + 1024 * 1024, 10000, 'output keeps flowing');
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

  it('refuses to start a second host on the same socket', async () => {
    await expect(startHost({ paths, version: 'other' })).rejects.toBeInstanceOf(HostRunningError);
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
      id: () => 'agent-k-local-host.service',
      statusHint: () => 'systemctl --user status agent-k-local-host.service',
    } as unknown as ServiceManager;
    const owned = new SessionHost(paths, { services: () => svc, start: () => void (started += 100) });
    await expect(owned.ensureServer()).rejects.toThrow(/agent-k-local-host\.service\) isn't running\. Check: systemctl --user status/);
    expect(started).toBe(1);

    // Nor when only the server runs as a service (adding the host's failed): it would end with the server.
    const serverOnly = { ...svc, installed: (s: string) => s === 'server' } as unknown as ServiceManager;
    const half = new SessionHost(paths, { services: () => serverOnly, start: () => void (started += 100) });
    await expect(half.ensureServer()).rejects.toThrow(/agent-k-local-host\.service\) isn't installed\. Run: agent-k-local install/);
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
    await expect(host.ensureServer()).rejects.toThrow(/version 0\.20261001\.1\) is too old for this server\. Restart it with: agent-k-local host restart/);
    expect(host.hello).toMatchObject({ version: '0.20261001.1', outdated: true });
    expect(changes).toBe(1);
    rmSync(home, { recursive: true, force: true });
  });
});

describe('agent-k-local host and attach (the commands)', () => {
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

    await host.spawn('ak-demo--fix-it', home, ['/bin/sh', '-c', 'printf "pane says hi\n"; sleep 30']);
    expect(execFileSync(tsx, [cli, 'attach'], { env, encoding: 'utf8' })).toContain('  ak-demo--fix-it\n');

    // Attached by its slug in a terminal: the screen shows, and Ctrl+\ detaches, leaving it running.
    const term = pty.spawn(tsx, [cli, 'attach', 'fix-it'], { cols: 100, rows: 30, cwd: home, env });
    let out = '';
    term.onData((d) => (out += d));
    const exited = new Promise<number>((r) => term.onExit(({ exitCode }) => r(exitCode)));
    await waitFor(() => out, (o) => o.includes('pane says hi'), 20000, 'the pane on screen');
    term.write('\x1c');
    expect(await exited).toBe(0);
    expect(out).toContain('Detached from ak-demo--fix-it. Claude keeps running.');
    expect((await host.panes()).get('ak-demo--fix-it')?.dead).toBe(false);

    expect(execFileSync(tsx, [cli, 'host', 'stop'], { env, encoding: 'utf8' })).toContain('Stopped the session host and the 1 session(s) in it.');
    await waitFor(() => host.serverRunning(), (up) => !up, 10000, 'host down');
  }, 60000);
});
