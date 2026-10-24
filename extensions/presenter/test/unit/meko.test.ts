import { chmodSync, mkdirSync, rmSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { StreamableHTTPError } from '@modelcontextprotocol/sdk/client/streamableHttp.js';
import { ErrorCode, McpError } from '@modelcontextprotocol/sdk/types.js';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { findMeko, mayRetry, MekoClient, MekoError } from '../../src/server/meko.js';
import { getPaths } from '../../src/server/paths.js';
import { tempDir } from '../helpers.js';

/** A MekoClient whose tool calls are answered here, as Meko's MCP tools answer them. */
class Recording extends MekoClient {
  calls: { name: string; args: Record<string, unknown> }[] = [];
  constructor(private answer: (name: string, args: Record<string, unknown>) => Record<string, unknown>) {
    super(async () => ({ state: 'none', creds: null }), 'test');
  }
  override async call(name: string, args: Record<string, unknown>) {
    this.calls.push({ name, args });
    return this.answer(name, args);
  }
}

describe('Meko\'s tools as Presenter calls them', () => {
  it('names the conversation when it makes a deck\'s datapack, and sets its description after', async () => {
    const m = new Recording((name, args) => {
      if (name === 'conversation_create') return { id: 'c1' };
      // Meko refuses datapack_create without one: conversation_id_required.
      if (name === 'datapack_create') return args.conversation_id ? { datapack_id: 'dp1', name: args.name } : {};
      return { message: 'datapack updated' };
    });
    expect(await m.createDatapack('presenter/deck/acme', 'Presenter deck "Acme"')).toEqual({ id: 'dp1', name: 'presenter/deck/acme', description: 'Presenter deck "Acme"' });
    expect(m.calls.slice(1)).toEqual([
      { name: 'datapack_create', args: { name: 'presenter/deck/acme', conversation_id: 'c1' } },
      { name: 'datapack_update', args: { datapack_id: 'dp1', description: 'Presenter deck "Acme"', conversation_id: 'c1' } },
    ]);
  });

  it('names a fixed conversation when Meko has no default datapack to make one in', async () => {
    const m = new Recording((name) => {
      if (name === 'conversation_create') throw new MekoError('datapack_id_required: Pass datapack_id');
      return { datapacks: [], has_more: false };
    });
    await m.listDatapacks();
    expect(m.calls[1]!.args.conversation_id).toMatch(/^[0-9a-f]{32}$/);
  });

  it.each<[string, string, unknown, boolean, boolean]>([
    ['a dropped connection, for a call that can be sent twice', 'artifact_put', new TypeError('fetch failed'), true, true],
    ['a timeout', 'artifact_get', new McpError(ErrorCode.RequestTimeout, 'Request timed out'), true, false],
    ['datapack_create that may have reached Meko', 'datapack_create', new TypeError('fetch failed'), true, false],
    ['datapack_create that never connected', 'datapack_create', new TypeError('fetch failed'), false, true],
    ['datapack_create refused before it ran (an expired session)', 'datapack_create', new StreamableHTTPError(404, 'Session not found'), true, true],
  ])('tries again after %s: %s', (_n, tool, e, connected, want) => {
    expect(mayRetry(tool, e, connected)).toBe(want);
  });
});

describe('where Meko is', () => {
  let dir: string;
  beforeEach(() => {
    dir = tempDir('pres-meko-');
  });
  afterEach(() => rmSync(dir, { recursive: true, force: true }));

  /** A `meko` on PATH that reports the local Meko as running, with this URL and token. */
  function fakeMeko(url: string, token: string): NodeJS.ProcessEnv {
    const bin = join(dir, 'bin');
    mkdirSync(bin);
    writeFileSync(join(bin, 'meko'), `#!/bin/sh\necho '${JSON.stringify({ url, token, running: true })}'\n`);
    chmodSync(join(bin, 'meko'), 0o755);
    return { PATH: `${bin}:/usr/bin:/bin`, HOME: dir };
  }

  it.each([
    ['takes a Meko Cloud URL and key from config.json', 'https://meko.example.com/mcp', 'cloud-key', { url: 'https://meko.example.com/mcp', key: 'cloud-key' }],
    ['asks the local Meko for its URL and key, not config.json\'s old copy', 'http://127.0.0.1:7778/mcp', 'old-token', { url: 'http://127.0.0.1:7790/mcp', key: 'new-token' }],
  ])('%s', async (_n, url, key, want) => {
    const paths = getPaths(join(dir, 'home'));
    mkdirSync(paths.home, { recursive: true });
    writeFileSync(paths.config, JSON.stringify({ meko: { url, key } }));
    expect((await findMeko(paths, fakeMeko('http://127.0.0.1:7790/mcp', 'new-token'))).creds).toEqual(want);
  });
});
