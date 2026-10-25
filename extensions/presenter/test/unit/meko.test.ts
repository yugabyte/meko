import { chmodSync, mkdirSync, rmSync, writeFileSync } from 'node:fs';
import { createServer, type IncomingMessage, type Server, type ServerResponse } from 'node:http';
import type { AddressInfo } from 'node:net';
import { join } from 'node:path';
import { StreamableHTTPError } from '@modelcontextprotocol/sdk/client/streamableHttp.js';
import { McpServer } from '@modelcontextprotocol/sdk/server/mcp.js';
import { StreamableHTTPServerTransport } from '@modelcontextprotocol/sdk/server/streamableHttp.js';
import { ErrorCode, McpError } from '@modelcontextprotocol/sdk/types.js';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { apiUrlOf, mayRetry, MekoClient, MekoError, mekoFinder, probeMeko, type MekoProbe } from '../../src/server/meko.js';
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

  /** A `meko` on PATH that prints this for `meko token --json`. */
  function fakeMeko(out: string): NodeJS.ProcessEnv {
    const bin = join(dir, 'bin');
    mkdirSync(bin, { recursive: true });
    writeFileSync(join(bin, 'meko'), `#!/bin/sh\ncat <<'EOF'\n${out}\nEOF\n`);
    chmodSync(join(bin, 'meko'), 0o755);
    return { PATH: `${bin}:/usr/bin:/bin`, HOME: dir };
  }

  const LOCAL = { url: 'http://127.0.0.1:7778/mcp', token: 'local-token' };
  const LINK = { set: true, url: 'https://mcp.meko.example/mcp', api_key: 'cloud-key', web_url: 'https://cloud.meko.example', api_url: 'https://cloud.meko.example/api/v1' };

  it.each<[string, unknown, MekoProbe]>([
    ['the Meko on this computer, running, from a Meko too old to keep a link', { ...LOCAL, running: true },
      { state: 'running', local: { url: LOCAL.url, token: LOCAL.token } }],
    ['the link, while the Meko on this computer is stopped', { ...LOCAL, running: false, cloud: LINK },
      { state: 'stopped', local: null, cloud: { url: LINK.url, apiKey: 'cloud-key', apiUrl: LINK.api_url } }],
    ['an unlinked link as empty', { ...LOCAL, running: true, cloud: { set: true, url: '', api_key: '', web_url: '', api_url: '' } },
      { state: 'running', local: { url: LOCAL.url, token: LOCAL.token }, cloud: { url: '', apiKey: '', apiUrl: '' } }],
    ['a link Meko couldn\'t read as unreadable', { ...LOCAL, running: true, cloud: { error: 'the Keychain refused' } },
      { state: 'running', local: { url: LOCAL.url, token: LOCAL.token }, cloudUnreadable: true }],
    ['a link URL that isn\'t http as no link', { ...LOCAL, running: true, cloud: { ...LINK, url: 'file:///etc/passwd' } },
      { state: 'running', local: { url: LOCAL.url, token: LOCAL.token }, cloud: { url: '', apiKey: '', apiUrl: '' } }],
    ['something else as failed', { hello: 'world' }, { state: 'failed', local: null }],
  ])('reads %s', async (_n, out, want) => {
    expect(await probeMeko(getPaths(join(dir, 'home')), fakeMeko(JSON.stringify(out)))).toEqual(want);
  });

  const local: MekoProbe = { state: 'running', local: { url: 'http://127.0.0.1:7790/mcp', token: 't' } };
  const linked: MekoProbe = { ...local, cloud: { url: LINK.url, apiKey: 'cloud-key', apiUrl: LINK.api_url } };
  const LOCAL_CREDS = { url: 'http://127.0.0.1:7790/mcp', key: 't', apiUrl: 'http://127.0.0.1:7790/api/v1', kind: 'local' };
  const CLOUD_CREDS = { url: LINK.url, key: 'cloud-key', apiUrl: LINK.api_url, kind: 'cloud' };

  it.each<[string, MekoProbe[], unknown]>([
    ['uses Meko Cloud, with its own REST host, when Meko links it', [linked], CLOUD_CREDS],
    ['uses the Meko on this computer when unlinked', [linked, { ...local, cloud: { url: '', apiKey: '', apiUrl: '' } }], LOCAL_CREDS],
    ['uses the Meko on this computer with a Meko too old to keep a link', [linked, local], LOCAL_CREDS],
    ['keeps the link when Meko couldn\'t read it', [linked, { ...local, cloudUnreadable: true }], CLOUD_CREDS],
    ['keeps the link when meko failed', [linked, { state: 'failed', local: null }], CLOUD_CREDS],
    ['finds none with no meko and no link read before', [{ state: 'none', local: null }], null],
    ['finds none, not the Meko on this computer, when the first read couldn\'t read the link', [{ ...local, cloudUnreadable: true }], null],
    ['finds none when a linked Meko names no REST API URL, rather than guess it from the MCP host', [{ ...local, cloud: { url: LINK.url, apiKey: 'cloud-key', apiUrl: '' } }], null],
  ])('%s', async (_n, probes, want) => {
    const find = mekoFinder(async () => probes.shift()!);
    let found;
    while (probes.length) found = await find();
    expect(found!.creds).toEqual(want);
  });

  it('forgets the link it last read when Meko says it changed', async () => {
    const probes: MekoProbe[] = [linked, { ...local, cloudUnreadable: true }, { ...local, cloudUnreadable: true }];
    const find = mekoFinder(async () => probes.shift()!);
    await find();
    expect((await find()).creds).toEqual(CLOUD_CREDS);
    find.forget!();
    expect(await find()).toEqual({ state: 'failed', creds: null, why: "Meko couldn't read its Meko Cloud link: check meko status." });
  });
});

describe('Meko Cloud\'s REST API', () => {
  let servers: Server[] = [];
  afterEach(async () => {
    await Promise.all(servers.map((s) => new Promise((r) => s.close(r))));
    servers = [];
  });

  async function listen(handler: (req: IncomingMessage, res: ServerResponse) => void): Promise<string> {
    const s = createServer(handler);
    servers.push(s);
    await new Promise<void>((r) => s.listen(0, '127.0.0.1', r));
    return `http://127.0.0.1:${(s.address() as AddressInfo).port}`;
  }

  /** Meko's MCP endpoint, stateless as Meko's own is; `status` answers every request instead. */
  async function mcp(status?: number): Promise<string> {
    return `${await listen(async (req, res) => {
      if (status) return void res.writeHead(status).end();
      const chunks: Buffer[] = [];
      for await (const c of req) chunks.push(c as Buffer);
      const server = new McpServer({ name: 'meko', version: '1' });
      const transport = new StreamableHTTPServerTransport({ sessionIdGenerator: undefined, enableJsonResponse: true });
      await server.connect(transport);
      await transport.handleRequest(req, res, JSON.parse(Buffer.concat(chunks).toString() || 'null'));
    })}/mcp`;
  }

  const client = (url: string, apiUrl: string) =>
    new MekoClient(async () => ({ state: 'ready', creds: { url, key: 'cloud-key', apiUrl, kind: 'cloud' } }), 'test');

  it('lists artifacts on the link\'s REST host, not the MCP one', async () => {
    const seen: { url: string; auth: string | undefined }[] = [];
    const api = await listen((req, res) => {
      seen.push({ url: req.url!, auth: req.headers.authorization });
      res.writeHead(200, { 'content-type': 'application/json' }).end(JSON.stringify({ data: { items: [
        { filename: 'deck.json', content_hash: 'a'.repeat(64), size_bytes: 10, created_at: '2026-11-01T10:00:00Z' },
        { filename: 'old/deck.json', content_hash: 'b'.repeat(64), size_bytes: 10, created_at: '2026-11-01T09:00:00Z' },
      ], has_more: false } }));
    });
    const m = client(await mcp(), `${api}/api/v1`);
    try {
      expect(await m.listArtifacts('dp 1', 'deck.json')).toEqual([{ hash: 'a'.repeat(64), filename: 'deck.json', size: 10, createdAt: '2026-11-01T10:00:00Z' }]);
    } finally {
      await m.close();
    }
    expect(seen).toEqual([{ url: '/api/v1/datapacks/dp%201/artifacts?q=deck.json&page=1&page_size=200', auth: 'Bearer cloud-key' }]);
  });

  it.each([
    ['its MCP endpoint', 401, 200],
    ['its REST API', 200, 403],
  ])('says to link again when Meko Cloud refuses the key at %s', async (_n, mcpStatus, apiStatus) => {
    const api = await listen((_req, res) => void res.writeHead(apiStatus).end());
    const m = client(await mcp(mcpStatus === 200 ? undefined : mcpStatus), `${api}/api/v1`);
    try {
      await expect(m.listArtifacts('dp1', 'deck.json')).rejects.toThrow(/Meko Cloud refused the key: link it again in Meko's Settings › Meko Cloud/);
    } finally {
      await m.close();
    }
  });

  it('gives Claude the URL and key Meko gives now, not those of a connection still open', async () => {
    const url = await mcp();
    let key = 'old-token';
    const m = new MekoClient(async () => ({ state: 'ready', creds: { url, key, apiUrl: apiUrlOf(url), kind: 'local' } }), 'test');
    try {
      expect((await m.current())?.key).toBe('old-token');
      key = 'new-token';
      expect((await m.current())?.key).toBe('new-token');
    } finally {
      await m.close();
    }
  });

  it('connects once for callers that ask at once', async () => {
    let n = 0;
    const m = new MekoClient(async () => {
      n++;
      return { state: 'ready', creds: { url: 'http://127.0.0.1:1/mcp', key: 'k', apiUrl: '', kind: 'local' } };
    }, 'test');
    vi.spyOn(globalThis, 'fetch').mockRejectedValue(Object.assign(new TypeError('fetch failed'), { cause: { code: 'ECONNREFUSED' } }));
    try {
      await Promise.all([m.at(), m.at(), m.current()]);
      expect(n).toBe(1);
    } finally {
      vi.restoreAllMocks();
    }
  });

  it('finds Meko again after reset(), and names the Meko it found even when it can\'t be reached, without trying again', async () => {
    let n = 0;
    const m = new MekoClient(async () => {
      n++;
      return { state: 'ready', creds: { url: n === 1 ? 'http://127.0.0.1:1/mcp' : 'https://mcp.meko.example/mcp', key: 'k', apiUrl: '', kind: n === 1 ? 'local' : 'cloud' } };
    }, 'test');
    // Neither answers: at() names the Meko it found all the same, so the pages can say which.
    vi.spyOn(globalThis, 'fetch').mockRejectedValue(Object.assign(new TypeError('fetch failed'), { cause: { code: 'ECONNREFUSED' } }));
    try {
      expect(await m.at()).toEqual({ kind: 'local', host: '127.0.0.1:1' });
      expect(await m.at()).toEqual({ kind: 'local', host: '127.0.0.1:1' });
      expect(n).toBe(1);
      m.reset();
      expect(await m.at()).toEqual({ kind: 'cloud', host: 'mcp.meko.example' });
      expect(n).toBe(2);
    } finally {
      vi.restoreAllMocks();
    }
  });
});
