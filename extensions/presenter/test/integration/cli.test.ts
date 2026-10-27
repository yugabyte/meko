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
  home = join(dir, 'meko', 'extensions', 'presenter');
});
afterEach(() => {
  presenter('stop');
  rmSync(dir, { recursive: true, force: true });
});

function presenter(...args: string[]) {
  return presenterWith({}, ...args);
}

function presenterWith(env: Record<string, string>, ...args: string[]) {
  const r = spawnSync(process.execPath, ['--import', 'tsx', CLI, ...args], {
    env: { ...process.env, PRESENTER_HOME: home, MEKO_HOME: join(dir, 'meko'), PATH: '/usr/bin:/bin', ...env },
    encoding: 'utf8',
    timeout: 30000,
  });
  return { code: r.status, out: r.stdout + r.stderr };
}

describe('the commands Meko runs', () => {
  it('says its version as Meko checks a release it unpacks', () => {
    const r = presenter('version', '--json');
    expect(r.code).toBe(0);
    expect(JSON.parse(r.out)).toEqual({ version: expect.any(String) });
  });

  it('names the socket Meko serves at /ext/presenter/', () => {
    const r = presenter('_handoff', '--json');
    expect(r.code).toBe(0);
    expect(JSON.parse(r.out)).toEqual({ socket: getPaths(home).webSock, legacy: null });
  });

  it.each([
    ['removes a Meko URL and key an older Presenter kept, and the file with nothing else in it', { meko: { url: 'https://meko.example.com/mcp', key: 'tok' } }, null],
    ['keeps the rest of config.json, readable only by this user', { meko: { url: 'https://meko.example.com/mcp', key: 'tok' }, other: 1 }, { other: 1 }],
  ])('%s', (_n, before, after) => {
    mkdirSync(home, { recursive: true });
    const cfg = join(home, 'config.json');
    writeFileSync(cfg, JSON.stringify(before));
    chmodSync(cfg, 0o644);
    // What `meko install_extension presenter` passes: neither is kept, Meko is asked each time.
    expect(presenter('install', '--meko-url', 'http://127.0.0.1:7778/mcp', '--meko-key', 'local', '--non-interactive').code).toBe(0);
    if (!after) return expect(existsSync(cfg)).toBe(false);
    expect(JSON.parse(readFileSync(cfg, 'utf8'))).toEqual(after);
    expect(statSync(cfg).mode & 0o077).toBe(0);
  });

  it('starts, says it runs, and stops', () => {
    expect(presenter('status').code).toBe(3);
    const start = presenter('start');
    expect(start.out, readFileSync(join(home, 'logs', 'server.log'), 'utf8')).toContain('Presenter started');
    expect(statSync(getPaths(home).webSock).isSocket()).toBe(true);
    expect(statSync(join(home, 'run')).mode & 0o077).toBe(0);
    const status = presenter('status');
    expect([status.code, status.out]).toEqual([0, expect.stringMatching(/Server: running \(pid \d+\)/)]);
    expect(presenter('stop').out).toContain('Presenter stopped');
    expect(existsSync(join(home, 'run', 'server.pid'))).toBe(false);
  });

  it('starts the server through a release\'s launcher, which hands it to Meko\'s', () => {
    // A stand-in for packaging/meko_presenter: notes the start, then runs the server as this test does.
    const launcher = join(dir, 'launcher');
    writeFileSync(launcher, `#!/bin/sh\necho "$@" >> "${join(dir, 'launched')}"\nexec "${process.execPath}" --import tsx "${CLI}" "$@"\n`);
    chmodSync(launcher, 0o755);
    expect(presenterWith({ PRESENTER_LAUNCHER: launcher }, 'start').out).toContain('Presenter started');
    expect(readFileSync(join(dir, 'launched'), 'utf8')).toBe('serve\n');
    expect(presenter('status').code).toBe(0);
  });

  it('says no talk is live when the server isn\'t running, or is but has no talk', () => {
    expect(presenter('_busy').code).toBe(1);
    expect(presenter('start').out).toContain('Presenter started');
    expect(presenter('_busy')).toEqual({ code: 1, out: 'No presentation is live\n' });
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
          env: { ...process.env, PRESENTER_HOME: home, MEKO_HOME: join(dir, 'meko'), PATH: '/usr/bin:/bin' },
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
      expect(presenter('stop').out).toContain('Presenter isn\'t running');
      await new Promise((r) => setTimeout(r, 200));
      expect(other.exitCode === null && other.signalCode === null).toBe(true);
    } finally {
      other.kill();
    }
  });

  it('refuses a Meko that isn\'t its own', () => {
    const r = spawnSync(process.execPath, ['--import', 'tsx', CLI, 'status'], {
      env: { ...process.env, PRESENTER_HOME: home, MEKO_HOME: join(dir, 'other') }, encoding: 'utf8',
    });
    expect([r.status, r.stderr]).toEqual([1, expect.stringContaining('belongs to the Meko in')]);
  });
});
