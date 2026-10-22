import { createServer, type IncomingMessage, type Server as HttpServer, type ServerResponse } from 'node:http';
import type { AddressInfo } from 'node:net';
import { Server } from '@modelcontextprotocol/sdk/server/index.js';
import { StreamableHTTPServerTransport } from '@modelcontextprotocol/sdk/server/streamableHttp.js';
import { CallToolRequestSchema, ListToolsRequestSchema } from '@modelcontextprotocol/sdk/types.js';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { MekoClient, mekoTag, withMekoScope, type MekoCallRecord, type MekoTrace } from '../../src/server/meko.js';

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
    expect(await client.listDatapacks()).toEqual([
      { id: 'd1', name: 'mine', grant: null, createdAt: null, ownerName: null, ownerEmail: null, description: null, ownerUserId: null },
    ]);
    expect(await client.createDatapack('new')).toEqual({ id: 'd2', name: 'new' });
  });

  it('fails on an unexpected datapack_list reply instead of reading it as no datapacks', async () => {
    meko.tools.conversation_create = () => ({ id: 'conv-1' });
    meko.tools.datapack_list = () => ({ items: [] });
    await expect(client.listDatapacks()).rejects.toThrow(/unexpected reply/);
  });

  it("keeps the owner, the description and the owner's account id", async () => {
    meko.tools.conversation_create = () => ({ id: 'conv-1' });
    meko.tools.datapack_list = () => [{
      datapack_id: 'd1', datapack_name: 'adlc/proj/a/b', grant: 'contributor', user_id: 'u-alice', datapack_owner_name: 'Alice Ng',
      datapack_owner_email_id: 'alice@example.com', description: 'AgentK project "b" · repo git@github.com:corp/a.git',
    }];
    expect(await client.listDatapacks()).toEqual([{
      id: 'd1', name: 'adlc/proj/a/b', grant: 'contributor', createdAt: null, ownerName: 'Alice Ng', ownerEmail: 'alice@example.com',
      description: 'AgentK project "b" · repo git@github.com:corp/a.git', ownerUserId: 'u-alice',
    }]);
  });

  it('sets a description, reads the grant, and starts a new service conversation after a reset', async () => {
    let convs = 0;
    meko.tools.conversation_create = () => ({ id: `conv-${++convs}` });
    meko.tools.datapack_update = () => ({ status: 'updated' });
    meko.tools.datapack_describe = () => ({ memory_count: 2, knowledge_chunk_count: 5, grant: 'maintainer' });
    await client.setDatapackDescription('d1', 'hello');
    expect(meko.calls.find((c) => c.name === 'datapack_update')?.args).toEqual({ datapack_id: 'd1', description: 'hello', conversation_id: 'conv-1' });
    expect(await client.describeDatapack('d1')).toEqual({ memoryCount: 2, knowledgeCount: 5, grant: 'maintainer', ownerName: null, ownerEmail: null });
    await client.reset();
    await client.describeDatapack('d1');
    expect(meko.calls.filter((c) => c.name === 'datapack_describe').map((c) => c.args.conversation_id)).toEqual(['conv-1', 'conv-2']);
  });

  it("retitles a conversation in its datapack, as the agent that owns it", async () => {
    meko.tools.conversation_update = () => ({ status: 'updated' });
    await client.retitleConversation('claude_code:x-1', 'd1', 'conv-9', 'New (Demo)');
    expect(meko.calls.find((c) => c.name === 'conversation_update')?.args).toEqual({
      agent_id: 'claude_code:x-1', datapack_id: 'd1', conversation_id: 'conv-9', title: 'New (Demo)',
    });
    meko.tools.conversation_update = () => ({ error: 'not_found' });
    await expect(client.retitleConversation('claude_code:x-1', 'd1', 'conv-9', 'New (Demo)')).rejects.toThrow(/not_found/);
  });

  it('finds memories like a text, with the ids and writers an update needs, and updates one as its writer', async () => {
    meko.tools.context_search = () => ({ results: { memory: [
      { id: 'm1', memory: 'Migrations are append-only.', agent_id: 'claude_code:a-1', score: 0.8 },
      { id: 'm2', memory: '  ', agent_id: 'claude_code:a-2' },
      { memory: 'No id.' },
    ], knowledge_base: [{ id: 'k1', text: 'From the knowledge base.' }] } });
    meko.tools.memory_update = () => ({ status: 'updated' });
    expect(await client.similar('migrations', 'd1', 'claude_code:me', 'conv')).toEqual([{ id: 'm1', text: 'Migrations are append-only.', agentId: 'claude_code:a-1' }]);
    expect(meko.calls.find((c) => c.name === 'context_search')?.args).toEqual({
      query: 'migrations', conversation_id: 'conv', datapack_id: 'd1', agent_id: 'claude_code:me', force_source: 'memory', limit: 3,
    });
    await client.updateMemory('m1', 'Corrected.', 'd1', 'claude_code:a-1', 'conv');
    expect(meko.calls.find((c) => c.name === 'memory_update')?.args).toEqual({
      memory_id: 'm1', text: 'Corrected.', datapack_id: 'd1', agent_id: 'claude_code:a-1', conversation_id: 'conv',
    });
  });

  it("reads knowledge-base hits in Meko's shape: the text in chunk_text, the rerank score as the score", async () => {
    meko.tools.context_search = () => ({ results: { memory: [], knowledge_base: [
      { id: 'c1', chunk_text: 'Deploys need two approvals.', document_id: 'doc1', distance: 0.3, rerank_score: 0.97,
        match_type: 'hybrid', metadata_filters: {} },
      { id: 'c2', chunk_text: 'No rerank score.', document_id: 'doc1', distance: 0.4, match_type: 'semantic', metadata_filters: {} },
    ] } });
    const hits = await client.search('deploys', { id: 'd1', name: 'dp' }, 'agent', 'conv');
    expect(hits.map((h) => [h.source, h.id, h.text, h.score])).toEqual([
      ['knowledge_base', 'c1', 'Deploys need two approvals.', 0.97],
      ['knowledge_base', 'c2', 'No rerank score.', 0.4],
    ]);
  });

  it("records each of AgentK's calls by tool and size, tagged with the step that made it, never their content", async () => {
    const records: MekoCallRecord[] = [];
    client.onCall = (r) => records.push(r);
    meko.tools.memory_add = () => ({ results: [] });
    meko.tools.memory_update = () => ({ error: 'not_found' });
    await mekoTag.run('L1', () => client.addMemory('Gotcha: x.', 'd1', 'agent', 'conv'));
    await expect(client.updateMemory('m9', 'y', 'd1', null, 'conv')).rejects.toThrow(/not_found/);
    expect(records).toEqual([
      { tool: 'memory_add', at: expect.any(Number), inputChars: expect.any(Number), resultChars: '{"results":[]}'.length, ok: true, ms: expect.any(Number), tag: 'L1' },
      { tool: 'memory_update', at: expect.any(Number), inputChars: expect.any(Number), resultChars: '{"error":"not_found"}'.length, ok: false, ms: expect.any(Number), tag: null },
    ]);
    expect(records[0].inputChars).toBeGreaterThan('Gotcha: x.'.length);
    // A listener that throws never fails the call.
    client.onCall = () => {
      throw new Error('db closed');
    };
    await client.addMemory('z', 'd1', 'agent', 'conv');
  });

  it('traces each call with what was sent and what came back, and the session and step it was for', async () => {
    const traces: MekoTrace[] = [];
    client.onTrace = (t) => traces.push(t);
    meko.tools.memory_add = () => ({ results: [{ id: 'm1' }] });
    meko.tools.memory_update = () => ({ error: 'not_found', detail: 'no such memory' });
    meko.tools.context_search = () => ({ results: { memory: [] } });
    await withMekoScope({ id: 's1', created_at: 5 }, () => mekoTag.run('L1', () => client.addMemory('Gotcha: x.', 'd1', 'agent', 'conv')));
    await expect(withMekoScope({ id: 's1', created_at: 5 }, () => client.updateMemory('m9', 'y', 'd1', null, 'conv'))).rejects.toThrow(/not_found/);
    await client.search('q', { id: 'd1', name: 'dp' }, 'agent', 'conv');
    expect(traces).toEqual([
      {
        tool: 'memory_add', at: expect.any(Number), ms: expect.any(Number), ok: true, attempts: 1,
        args: { text: 'Gotcha: x.', datapack_id: 'd1', agent_id: 'agent', conversation_id: 'conv', metadata: null },
        text: '{"results":[{"id":"m1"}]}', error: null, tag: 'L1', session: { id: 's1', createdAt: 5 },
      },
      {
        tool: 'memory_update', at: expect.any(Number), ms: expect.any(Number), ok: false, attempts: 1,
        args: { memory_id: 'm9', text: 'y', datapack_id: 'd1', agent_id: null, conversation_id: 'conv' },
        text: '{"error":"not_found","detail":"no such memory"}', error: 'not_found: no such memory', tag: null, session: { id: 's1', createdAt: 5 },
      },
      expect.objectContaining({ tool: 'context_search', ok: true, session: null, tag: null }),
    ]);
    // The key is in a header only: never in a trace.
    expect(JSON.stringify(traces)).not.toContain('test-key');
    // A listener that throws never fails the call.
    client.onTrace = () => {
      throw new Error('disk full');
    };
    await client.addMemory('z', 'd1', 'agent', 'conv');
  });

  it('never sends a write twice after it may have reached Meko, but retries reads, tracing that as two attempts with no reply', async () => {
    const traces: MekoTrace[] = [];
    client.onTrace = (t) => traces.push(t);
    meko.drop.add('memory_add');
    await expect(client.addMemory('x', 'd1', 'agent', 'conv')).rejects.toThrow();
    expect(meko.calls.filter((c) => c.name === 'memory_add')).toHaveLength(1);

    meko.drop.add('context_search');
    await expect(client.search('q', { id: 'd1', name: 'dp' }, 'agent', 'conv')).rejects.toThrow();
    expect(meko.calls.filter((c) => c.name === 'context_search')).toHaveLength(2);
    expect(traces).toEqual([
      expect.objectContaining({ tool: 'memory_add', ok: false, attempts: 1 }),
      expect.objectContaining({ tool: 'context_search', ok: false, attempts: 2, text: null, error: expect.any(String) }),
    ]);
  });

  it('drops a connection opened with the old URL when it is reset while connecting', async () => {
    const other = new FakeMeko();
    await other.start();
    try {
      let url = meko.url;
      const c = new MekoClient(() => ({ url, apiKey: 'k' }));
      meko.tools.conversation_create = () => ({ id: 'conv-old' });
      other.tools.conversation_create = () => ({ id: 'conv-new' });
      other.tools.datapack_list = () => [];
      meko.tools.datapack_list = () => [];
      const checking = c.check(); // starts connecting to the old server
      await c.reset();
      url = other.url;
      await checking;
      await c.listDatapacks();
      expect(meko.calls.filter((x) => x.name === 'datapack_list')).toHaveLength(0);
      expect(other.calls.filter((x) => x.name === 'datapack_list')).toHaveLength(1);
      await c.close();
    } finally {
      await other.stop();
    }
  });

  it('does not retry a call that timed out', async () => {
    meko.tools.datapack_describe = () => new Promise((r) => setTimeout(() => r({ memory_count: 1 }), 500));
    meko.tools.conversation_create = () => ({ id: 'conv-1' });
    await expect(client.describeDatapack('d1', 100)).rejects.toThrow(/timed out/i);
    expect(meko.calls.filter((c) => c.name === 'datapack_describe')).toHaveLength(1);
  });
});
