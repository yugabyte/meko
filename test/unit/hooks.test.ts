import { chmodSync, mkdirSync, rmSync, statSync } from 'node:fs';
import type { Server } from 'node:http';
import { join } from 'node:path';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { startHookServer, type HookPayload } from '../../src/server/hooks.js';
import { tempDir, waitFor } from '../helpers.js';
import { postHook } from '../hook-client.js';

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
});
