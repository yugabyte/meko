import { spawn, spawnSync } from 'node:child_process';
import { chmodSync, existsSync, mkdirSync, readFileSync, rmSync, statSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
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

  it('keeps a Meko Cloud URL and key, readable only by this user, and not the local Meko\'s', () => {
    mkdirSync(home, { recursive: true });
    const cfg = join(home, 'config.json');
    writeFileSync(cfg, '{}\n');
    chmodSync(cfg, 0o644);
    expect(presenter('install', '--meko-url', 'https://meko.example.com/mcp', '--meko-key', 'tok', '--non-interactive').code).toBe(0);
    expect(JSON.parse(readFileSync(cfg, 'utf8'))).toEqual({ meko: { url: 'https://meko.example.com/mcp', key: 'tok' } });
    expect(statSync(cfg).mode & 0o077).toBe(0);
    // What `meko install_extension presenter` passes: asked of `meko token --json` each time instead.
    expect(presenter('install', '--meko-url', 'http://127.0.0.1:7778/mcp', '--meko-key', 'local', '--non-interactive').code).toBe(0);
    expect(JSON.parse(readFileSync(cfg, 'utf8'))).toEqual({});
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

  it('starts the server through a release\'s launcher, which counts the starts of an update', () => {
    // A stand-in for packaging/meko_presenter: notes the start, then runs the server as this test does.
    const launcher = join(dir, 'launcher');
    writeFileSync(launcher, `#!/bin/sh\necho "$@" >> "${join(dir, 'launched')}"\nexec "${process.execPath}" --import tsx "${CLI}" "$@"\n`);
    chmodSync(launcher, 0o755);
    expect(presenterWith({ PRESENTER_LAUNCHER: launcher }, 'start').out).toContain('Presenter started');
    expect(readFileSync(join(dir, 'launched'), 'utf8')).toBe('serve\n');
    expect(presenter('status').code).toBe(0);
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
