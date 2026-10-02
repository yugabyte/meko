import { execFile } from 'node:child_process';
import { promisify } from 'node:util';
import { chmodSync, mkdirSync, readFileSync, rmSync, statSync } from 'node:fs';
import { request, type Server } from 'node:http';
import { join } from 'node:path';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { contextScriptPath, hooksSettingsPath, writeHookFiles } from '../../src/server/claude.js';
import { socketId, startHookServer, type HookPayload } from '../../src/server/hooks.js';
import { getPaths } from '../../src/server/paths.js';
import { tempDir, waitFor } from '../helpers.js';
import { postHook, postHookFull } from '../hook-client.js';

describe('hook socket', () => {
  let home: string;
  let server: Server | null = null;
  beforeEach(() => {
    home = tempDir('akl-hooks-');
  });
  afterEach(async () => {
    await new Promise<void>((r) => (server ? server.close(() => r()) : r()));
    server = null;
    rmSync(home, { recursive: true, force: true });
  });

  it('is owner-only: directory 0700 (tightened if looser), socket 0600', async () => {
    const dir = join(home, 'run');
    mkdirSync(dir);
    chmodSync(dir, 0o755);
    const sock = join(dir, 'hooks.sock');
    server = await startHookServer(sock, () => undefined);
    expect(statSync(dir).mode & 0o777).toBe(0o700);
    expect(statSync(sock).mode & 0o777).toBe(0o600);
  });

  it('tells the socket it bound from one put in its place', async () => {
    const sock = join(home, 'run', 'hooks.sock');
    server = await startHookServer(sock, () => undefined);
    const mine = socketId(sock);
    expect(mine).toMatch(/^\d+:\d+:\d+$/);
    rmSync(sock);
    expect(socketId(sock)).toBeNull();
    const other = await startHookServer(sock, () => undefined);
    try {
      expect(socketId(sock)).not.toBeNull();
      expect(socketId(sock)).not.toBe(mine);
    } finally {
      await new Promise<void>((r) => other.close(() => r()));
    }
    expect(socketId(join(home, 'run'))).toBeNull(); // not a socket
  });

  it('passes events on with their token, and drops ones without a token or on other paths', async () => {
    const sock = join(home, 'run', 'hooks.sock');
    const got: [string, HookPayload][] = [];
    server = await startHookServer(sock, (token, p) => got.push([token, p]));
    expect(await postHook(sock, '/hook', null, { hook_event_name: 'Stop' })).toBe(204);
    expect(await postHook(sock, '/other', 'tok', { hook_event_name: 'Stop' })).toBe(404);
    expect(await postHook(sock, '/hook', 'tok', { hook_event_name: 'SessionStart' })).toBe(204);
    await waitFor(() => got.length, (n) => n === 1, 2000, 'event');
    expect(got).toEqual([['tok', { hook_event_name: 'SessionStart' }]]);
  });

  it('answers /context with what the handler returns, as JSON, and 204 without one', async () => {
    const sock = join(home, 'run', 'hooks.sock');
    const asked: [string, HookPayload][] = [];
    server = await startHookServer(sock, () => undefined, undefined, undefined, async (token, p) => {
      asked.push([token, p]);
      if (p.source === 'boom') throw new Error('broken');
      return p.source === 'startup' ? { hookSpecificOutput: { hookEventName: 'SessionStart', additionalContext: 'hello' } } : null;
    });
    const r = await postHookFull(sock, '/context', 'tok', { hook_event_name: 'SessionStart', source: 'startup' });
    expect(r.status).toBe(200);
    expect(JSON.parse(r.body)).toEqual({ hookSpecificOutput: { hookEventName: 'SessionStart', additionalContext: 'hello' } });
    expect((await postHookFull(sock, '/context', 'tok', { source: 'resume' })).status).toBe(204);
    expect((await postHookFull(sock, '/context', 'tok', { source: 'boom' })).status).toBe(204);
    expect((await postHookFull(sock, '/context', null, { source: 'startup' })).status).toBe(204);
    expect(asked.map(([t, p]) => [t, p.source])).toEqual([['tok', 'startup'], ['tok', 'resume'], ['tok', 'boom']]);
  });

  it("answers the mod's routes with what the handler replies, and 403 without a token", async () => {
    const sock = join(home, 'run', 'hooks.sock');
    const asked: [string, string, Record<string, unknown>][] = [];
    server = await startHookServer(sock, () => undefined, undefined, undefined, undefined, async (token, route, body) => {
      asked.push([token, route, body]);
      if (route === '/mod/poll') return { status: 200, body: { prompts: [{ id: 'p1', text: 'hi' }] } };
      if (route === '/mod/measure') throw new Error('broken');
      return { status: 204 };
    });
    const poll = await postHookFull(sock, '/mod/poll', 'tok', {});
    expect(poll.status).toBe(200);
    expect(JSON.parse(poll.body)).toEqual({ prompts: [{ id: 'p1', text: 'hi' }] });
    expect(await postHook(sock, '/mod/ack', 'tok', { id: 'p1', state: 'accepted' })).toBe(204);
    expect(await postHook(sock, '/mod/measure', 'tok', {})).toBe(500);
    expect(await postHook(sock, '/mod/hello', null, {})).toBe(403);
    expect(await postHook(sock, '/mod/other', 'tok', {})).toBe(404);
    expect(asked).toEqual([['tok', '/mod/poll', {}], ['tok', '/mod/ack', { id: 'p1', state: 'accepted' }], ['tok', '/mod/measure', {}]]);
  });

  it("aborts a held mod poll's signal when the mod's connection closes", async () => {
    const sock = join(home, 'run', 'hooks.sock');
    let aborted = false;
    server = await startHookServer(sock, () => undefined, undefined, undefined, undefined, (_t, _r, _b, signal) =>
      new Promise((resolve) => signal.addEventListener('abort', () => {
        aborted = true;
        resolve({ status: 204 });
      })));
    const req = request({ socketPath: sock, path: '/mod/poll', method: 'POST', headers: { 'X-AK-Token': 'tok' } });
    req.on('error', () => undefined);
    req.end('{}');
    await new Promise((r) => setTimeout(r, 100));
    req.destroy();
    await waitFor(() => aborted, (a) => a, 2000, 'abort');
  });
});

describe('the SessionStart context hook', () => {
  let home: string;
  let server: Server | null = null;
  beforeEach(() => {
    home = tempDir('akl-context-');
  });
  afterEach(async () => {
    await new Promise<void>((r) => (server ? server.close(() => r()) : r()));
    server = null;
    rmSync(home, { recursive: true, force: true });
  });

  it('is registered for SessionStart, next to the forwarder, with a 5-second limit', () => {
    const paths = getPaths(join(home, 'ak'));
    writeHookFiles(paths);
    const settings = JSON.parse(readFileSync(hooksSettingsPath(paths), 'utf8'));
    expect(settings.hooks.SessionStart).toEqual([{ hooks: [
      { type: 'command', command: join(paths.run, 'hook.sh'), timeout: 5 },
      { type: 'command', command: contextScriptPath(paths), timeout: 5 },
    ] }]);
  });

  it("prints the server's answer, and nothing (exit 0) when there is none or no server", async () => {
    const paths = getPaths(join(home, 'ak'));
    writeHookFiles(paths);
    const sock = join(home, 'run', 'hooks.sock');
    server = await startHookServer(sock, () => undefined, undefined, undefined, async (token) =>
      token === 'good' ? { hookSpecificOutput: { hookEventName: 'SessionStart', additionalContext: 'from Meko' } } : null);
    // Asynchronously: the server answering it runs in this process.
    const runScript = async (env: Record<string, string>) => {
      const child = promisify(execFile)(contextScriptPath(paths), { env: { PATH: process.env.PATH ?? '', ...env } });
      child.child.stdin?.end('{"hook_event_name":"SessionStart","source":"startup"}');
      return (await child).stdout;
    };
    expect(JSON.parse(await runScript({ AK_HOOK_SOCK: sock, AK_HOOK_TOKEN: 'good' }))).toEqual(
      { hookSpecificOutput: { hookEventName: 'SessionStart', additionalContext: 'from Meko' } });
    expect(await runScript({ AK_HOOK_SOCK: sock, AK_HOOK_TOKEN: 'other' })).toBe('');
    expect(await runScript({ AK_HOOK_SOCK: join(home, 'missing.sock'), AK_HOOK_TOKEN: 'good' })).toBe('');
    expect(await runScript({})).toBe('');
  });
});
