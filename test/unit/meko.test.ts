import { createServer, type IncomingMessage, type Server as HttpServer, type ServerResponse } from 'node:http';
import type { AddressInfo } from 'node:net';
import { Server } from '@modelcontextprotocol/sdk/server/index.js';
import { StreamableHTTPServerTransport } from '@modelcontextprotocol/sdk/server/streamableHttp.js';
import { CallToolRequestSchema, ListToolsRequestSchema } from '@modelcontextprotocol/sdk/types.js';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { MekoClient } from '../../src/server/meko.js';

type Handler = (args: Record<string, unknown>) => unknown | Promise<unknown>;

/** A local Meko stand-in: a stateless MCP server over Streamable HTTP. */
class FakeMeko {
  http: HttpServer | null = null;
  port = 0;
  calls: { name: string; args: Record<string, unknown> }[] = [];
  tools: Record<string, Handler> = {};
  /** Tools whose HTTP connection is cut once the call has reached the server. */
  drop = new Set<string>();

  async start(port = 0): Promise<void> {
    this.http = createServer((req, res) => void this.handle(req, res));
    await new Promise<void>((r) => this.http!.listen(port, '127.0.0.1', r));
    this.port = (this.http.address() as AddressInfo).port;
  }

  async stop(): Promise<void> {
    this.http?.closeAllConnections();
    await new Promise<void>((r) => (this.http ? this.http.close(() => r()) : r()));
    this.http = null;
  }

  get url(): string {
    return `http://127.0.0.1:${this.port}/mcp`;
  }

  private async handle(req: IncomingMessage, res: ServerResponse): Promise<void> {
    let raw = '';
    for await (const c of req) raw += c;
    const body = raw ? JSON.parse(raw) : undefined;
    if (body?.method === 'tools/call') {
      const { name, arguments: args } = body.params as { name: string; arguments: Record<string, unknown> };
      this.calls.push({ name, args });
      if (this.drop.has(name)) {
        req.socket.destroy();
        return;
      }
    }
    const server = new Server({ name: 'fake-meko', version: '0' }, { capabilities: { tools: {} } });
    server.setRequestHandler(ListToolsRequestSchema, async () => ({ tools: [] }));
    server.setRequestHandler(CallToolRequestSchema, async (r) => {
      const h = this.tools[r.params.name];
      const out = h ? await h((r.params.arguments ?? {}) as Record<string, unknown>) : { error: 'unknown_tool' };
      return { content: [{ type: 'text', text: JSON.stringify(out) }] };
    });
    const transport = new StreamableHTTPServerTransport({ sessionIdGenerator: undefined, enableJsonResponse: true });
    res.on('close', () => void transport.close());
    await server.connect(transport);
    await transport.handleRequest(req, res, body);
  }
}

describe('Meko client', () => {
  let meko: FakeMeko;
  let client: MekoClient;

  beforeEach(async () => {
    meko = new FakeMeko();
    await meko.start();
    client = new MekoClient(() => ({ url: meko.url, apiKey: 'test-key' }));
  });
  afterEach(async () => {
    await client.close();
    await meko.stop();
  });

  it('checks health with a real round trip every time', async () => {
    await client.check();
    const port = meko.port;
    await meko.stop();
    await expect(client.check()).rejects.toThrow();
    await meko.start(port);
    await client.check();
  });

  it('lists and creates datapacks for a user whose account has no default datapack', async () => {
    meko.tools.conversation_create = () => ({ error: 'datapack_id_required' });
    meko.tools.datapack_list = (a) => (/^[0-9a-f]{32}$/.test(String(a.conversation_id)) ? { datapacks: [{ datapack_id: 'd1', datapack_name: 'mine' }] } : { error: 'conversation_id_required' });
    meko.tools.datapack_create = (a) => (a.conversation_id ? { datapack_id: 'd2', datapack_name: a.name } : { error: 'conversation_id_required' });
    expect(await client.listDatapacks()).toEqual([{ id: 'd1', name: 'mine', grant: null, createdAt: null }]);
    expect(await client.createDatapack('new')).toEqual({ id: 'd2', name: 'new' });
  });

  it('uses the service conversation when Meko creates one', async () => {
    meko.tools.conversation_create = () => ({ id: 'conv-1' });
    meko.tools.datapack_list = () => [];
    await client.listDatapacks();
    await client.listDatapacks();
    expect(meko.calls.filter((c) => c.name === 'conversation_create')).toHaveLength(1);
    expect(meko.calls.filter((c) => c.name === 'datapack_list').map((c) => c.args.conversation_id)).toEqual(['conv-1', 'conv-1']);
  });

  it('never sends a write twice after it may have reached Meko, but retries reads', async () => {
    meko.drop.add('memory_add');
    await expect(client.addMemory('x', 'd1', 'agent', 'conv')).rejects.toThrow();
    expect(meko.calls.filter((c) => c.name === 'memory_add')).toHaveLength(1);

    meko.drop.add('context_search');
    await expect(client.search('q', { id: 'd1', name: 'dp' }, 'agent', 'conv')).rejects.toThrow();
    expect(meko.calls.filter((c) => c.name === 'context_search')).toHaveLength(2);
  });

  it('does not retry a call that timed out', async () => {
    meko.tools.datapack_describe = () => new Promise((r) => setTimeout(() => r({ memory_count: 1 }), 500));
    meko.tools.conversation_create = () => ({ id: 'conv-1' });
    await expect(client.describeDatapack('d1', 100)).rejects.toThrow(/timed out/i);
    expect(meko.calls.filter((c) => c.name === 'datapack_describe')).toHaveLength(1);
  });
});
