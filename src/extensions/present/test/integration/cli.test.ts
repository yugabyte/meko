import { spawn, spawnSync } from 'node:child_process';
import { chmodSync, existsSync, mkdirSync, readFileSync, rmSync, statSync, writeFileSync } from 'node:fs';
import { createServer } from 'node:http';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { getPaths } from '../../src/server/paths.js';
import { tempDir } from '../helpers.js';

const CLI = fileURLToPath(new URL('../../src/server/cli.ts', import.meta.url));
let dir: string;
let home: string;
beforeEach(() => {
  dir = tempDir('pres-cli-');
  // A home in a Meko data folder, as Meko runs it.
  home = join(dir, 'meko', 'extensions', 'present');
});
afterEach(() => {
  present('stop');
  rmSync(dir, { recursive: true, force: true });
});

function present(...args: string[]) {
  return presentWith({}, ...args);
}

function presentWith(env: Record<string, string>, ...args: string[]) {
  const r = spawnSync(process.execPath, ['--import', 'tsx', CLI, ...args], {
    env: { ...process.env, PRESENT_HOME: home, MEKO_HOME: join(dir, 'meko'), PATH: '/usr/bin:/bin', ...env },
    encoding: 'utf8',
    timeout: 30000,
  });
  return { code: r.status, out: r.stdout + r.stderr };
}

describe('the commands Meko runs', () => {
  it('says its version as Meko checks a release it unpacks', () => {
    const r = present('version', '--json');
    expect(r.code).toBe(0);
    expect(JSON.parse(r.out)).toEqual({ version: expect.any(String) });
  });

  it('names the socket Meko serves at /ext/present/', () => {
    const r = present('_handoff', '--json');
    expect(r.code).toBe(0);
    expect(JSON.parse(r.out)).toEqual({ socket: getPaths(home).webSock, legacy: null });
  });

  it.each([
    ['removes a Meko URL and key an older Present kept, and the file with nothing else in it', { meko: { url: 'https://meko.example.com/mcp', key: 'tok' } }, null],
    ['keeps the rest of config.json, readable only by this user', { meko: { url: 'https://meko.example.com/mcp', key: 'tok' }, other: 1 }, { other: 1 }],
  ])('%s', (_n, before, after) => {
    mkdirSync(home, { recursive: true });
    const cfg = join(home, 'config.json');
    writeFileSync(cfg, JSON.stringify(before));
    chmodSync(cfg, 0o644);
    // What `meko extension install present` passes: neither is kept, Meko is asked each time.
    expect(present('install', '--meko-url', 'http://127.0.0.1:7778/mcp', '--meko-key', 'local', '--non-interactive').code).toBe(0);
    if (!after) return expect(existsSync(cfg)).toBe(false);
    expect(JSON.parse(readFileSync(cfg, 'utf8'))).toEqual(after);
    expect(statSync(cfg).mode & 0o077).toBe(0);
  });

  it('starts, says it runs, and stops', () => {
    expect(present('status').code).toBe(3);
    const start = present('start');
    expect(start.out, readFileSync(join(home, 'logs', 'server.log'), 'utf8')).toContain('Present started');
    expect(statSync(getPaths(home).webSock).isSocket()).toBe(true);
    expect(statSync(join(home, 'run')).mode & 0o077).toBe(0);
    const status = present('status');
    expect([status.code, status.out]).toEqual([0, expect.stringMatching(/^✓ Present \S+ is running \(pid \d+, up \d+s\)\n {2}✓ Server {10}healthy\n/)]);
    expect(present('stop').out).toContain('Present stopped');
    expect(existsSync(join(home, 'run', 'server.pid'))).toBe(false);
  });

  it("prints one row of Meko's summary when meko start and meko stop run it", () => {
    expect(presentWith({ MEKO_SUMMARY: '1' }, 'start').out).toBe('  ✓ Present         started\n');
    expect(presentWith({ MEKO_SUMMARY: '1' }, 'start').out).toBe('  ✓ Present         running\n');
    expect(presentWith({ MEKO_SUMMARY: '1' }, 'stop').out).toBe('  ✓ Present         stopped\n');
    expect(presentWith({ MEKO_SUMMARY: '1' }, 'stop').out).toBe("  ✓ Present         wasn't running\n");
  });

  it('starts the server through a release\'s launcher, which hands it to Meko\'s', () => {
    // A stand-in for packaging/me_present: notes the start, then runs the server as this test does.
    const launcher = join(dir, 'launcher');
    writeFileSync(launcher, `#!/bin/sh\necho "$@" >> "${join(dir, 'launched')}"\nexec "${process.execPath}" --import tsx "${CLI}" "$@"\n`);
    chmodSync(launcher, 0o755);
    expect(presentWith({ PRESENT_LAUNCHER: launcher }, 'start').out).toContain('Present started');
    expect(readFileSync(join(dir, 'launched'), 'utf8')).toBe('serve\n');
    expect(present('status').code).toBe(0);
  });

  it('says no talk is live when the server isn\'t running, or is but has no talk', () => {
    expect(present('_busy').code).toBe(1);
    expect(present('start').out).toContain('Present started');
    expect(present('_busy')).toEqual({ code: 1, out: 'No presentation is live\n' });
  });

  it('says a talk is live when the server says so', async () => {
    // A stand-in for the server on its socket, answering as one with a talk on.
    const sock = getPaths(home).webSock;
    mkdirSync(dirname(sock), { recursive: true, mode: 0o700 });
    const server = createServer((req, res) => res.end(JSON.stringify({ busy: req.url === '/_meko/busy' })));
    await new Promise<void>((resolve) => server.listen(sock, resolve));
    try {
      const r = await new Promise<{ code: number | null; out: string }>((resolve) => {
        const child = spawn(process.execPath, ['--import', 'tsx', CLI, '_busy'], {
          env: { ...process.env, PRESENT_HOME: home, MEKO_HOME: join(dir, 'meko'), PATH: '/usr/bin:/bin' },
        });
        let out = '';
        child.stdout.on('data', (c: Buffer) => (out += String(c)));
        child.on('close', (code) => resolve({ code, out }));
      });
      expect(r).toEqual({ code: 0, out: 'A presentation is live\n' });
    } finally {
      await new Promise((resolve) => server.close(resolve));
    }
  });

  it('leaves alone a process that has the pid a crashed server left', async () => {
    const other = spawn('sleep', ['30'], { stdio: 'ignore' });
    try {
      mkdirSync(join(home, 'run'), { recursive: true });
      writeFileSync(join(home, 'run', 'server.pid'), String(other.pid));
      expect(present('stop').out).toContain('Present isn\'t running');
      await new Promise((r) => setTimeout(r, 200));
      expect(other.exitCode === null && other.signalCode === null).toBe(true);
    } finally {
      other.kill();
    }
  });

  it('refuses a Meko that isn\'t its own', () => {
    const r = spawnSync(process.execPath, ['--import', 'tsx', CLI, 'status'], {
      env: { ...process.env, PRESENT_HOME: home, MEKO_HOME: join(dir, 'other') }, encoding: 'utf8',
    });
    expect([r.status, r.stderr]).toEqual([1, expect.stringContaining('belongs to the Meko in')]);
  });
});
