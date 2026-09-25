import { Client } from '@modelcontextprotocol/sdk/client/index.js';
import { StreamableHTTPClientTransport } from '@modelcontextprotocol/sdk/client/streamableHttp.js';
import type { MekoDatapack, SearchResult } from '../shared/types.js';

export interface MekoCreds {
  url: string;
  apiKey: string;
}

export class MekoError extends Error {}

/** Agent id used for a project's Meko memory bucket and datapack pin. */
export function projectAgentId(projectId: string): string {
  return `claude_code:${projectId}`;
}

/**
 * Filename slug the Meko hooks use for pin files (mirror of datapackPinSlug in
 * meko-agent-skills' capture.js).
 */
export function pinSlug(agentId: string): string {
  return agentId.trim().replace(/[^A-Za-z0-9._-]+/g, '_').replace(/^_+|_+$/g, '') || 'meko_agent';
}

type Json = Record<string, unknown>;

/**
 * Thin client over the Meko MCP server (Streamable HTTP). Tools return a JSON
 * string in content[0].text; errors come back as `{"error": ...}` with
 * isError false, so both are checked.
 */
export class MekoClient {
  private client: Client | null = null;
  private connecting: Promise<Client> | null = null;
  /** Conversation used for server-originated calls (describe, list). */
  private serviceConversation: string | null = null;

  constructor(private creds: () => MekoCreds, private version = '0.0.0') {}

  private async connect(): Promise<Client> {
    if (this.client) return this.client;
    if (!this.connecting) {
      this.connecting = (async () => {
        const { url, apiKey } = this.creds();
        if (!apiKey) throw new MekoError('No Meko API key is configured.');
        const transport = new StreamableHTTPClientTransport(new URL(url), {
          requestInit: { headers: { Authorization: `Bearer ${apiKey}` } },
        });
        const c = new Client({ name: 'agent-k-local', version: this.version });
        await c.connect(transport);
        c.onclose = () => {
          if (this.client === c) this.client = null;
        };
        this.client = c;
        return c;
      })().finally(() => {
        this.connecting = null;
      });
    }
    return this.connecting;
  }

  async close(): Promise<void> {
    const c = this.client;
    this.client = null;
    await c?.close().catch(() => undefined);
  }

  async call(name: string, args: Json, timeoutMs = 30000): Promise<unknown> {
    let lastErr: unknown;
    for (let attempt = 0; attempt < 2; attempt++) {
      try {
        const c = await this.connect();
        const res = (await c.callTool({ name, arguments: args }, undefined, { timeout: timeoutMs })) as {
          content?: { type: string; text?: string }[];
          isError?: boolean;
        };
        const text = res.content?.find((x) => x.type === 'text')?.text ?? '';
        let parsed: unknown = text;
        try {
          parsed = JSON.parse(text);
        } catch {
          /* plain text result */
        }
        if (res.isError) throw new MekoError(typeof parsed === 'string' ? parsed : JSON.stringify(parsed));
        if (parsed && typeof parsed === 'object' && 'error' in parsed && !Array.isArray(parsed)) {
          const p = parsed as { error: unknown; detail?: unknown };
          throw new MekoError(`${String(p.error)}${p.detail ? `: ${String(p.detail)}` : ''}`);
        }
        return parsed;
      } catch (e) {
        lastErr = e;
        if (e instanceof MekoError) throw e;
        await this.close(); // transport problem: reconnect once
      }
    }
    throw lastErr instanceof Error ? lastErr : new MekoError(String(lastErr));
  }

  async ensureConversation(agentId: string, datapackId: string | null, key: string, title: string): Promise<string> {
    const r = (await this.call('conversation_create', {
      agent_id: agentId,
      datapack_id: datapackId,
      session_id: key,
      title,
    })) as Json;
    const id = String(r.id ?? r.conversation_id ?? '');
    if (!id) throw new MekoError('conversation_create returned no id');
    return id;
  }

  private async serviceConv(): Promise<string> {
    this.serviceConversation ??= await this.ensureConversation('agent-k-local', null, 'agent-k-local-service', 'agent-k-local service');
    return this.serviceConversation;
  }

  /** Cheap authenticated round trip, used for the health banner. */
  async check(): Promise<void> {
    await this.serviceConv();
  }

  async listDatapacks(): Promise<MekoDatapack[]> {
    const r = await this.call('datapack_list', { conversation_id: await this.serviceConv() }, 60000);
    const arr = Array.isArray(r) ? r : Array.isArray((r as Json)?.datapacks) ? ((r as Json).datapacks as Json[]) : [];
    return (arr as Json[])
      .map((d) => ({
        id: String(d.datapack_id ?? d.id ?? ''),
        name: String(d.datapack_name ?? d.name ?? ''),
        grant: d.grant == null ? null : String(d.grant),
        createdAt: d.created_at == null ? null : String(d.created_at),
      }))
      .filter((d) => d.id && d.name)
      .sort((a, b) => a.name.localeCompare(b.name));
  }

  async createDatapack(name: string): Promise<{ id: string; name: string }> {
    const r = (await this.call('datapack_create', { name, conversation_id: await this.serviceConv() }, 120000)) as Json;
    const d = (r.datapack as Json | undefined) ?? r;
    const id = String(d.datapack_id ?? d.id ?? '');
    if (!id) throw new MekoError(`datapack_create returned no id: ${JSON.stringify(r).slice(0, 200)}`);
    return { id, name: String(d.datapack_name ?? d.name ?? name) };
  }

  async describeDatapack(id: string): Promise<{ memoryCount: number | null; knowledgeCount: number | null }> {
    const r = (await this.call('datapack_describe', { datapack_id: id, conversation_id: await this.serviceConv() })) as Json;
    const num = (v: unknown) => (typeof v === 'number' ? v : null);
    return { memoryCount: num(r.memory_count), knowledgeCount: num(r.knowledge_chunk_count ?? r.knowledge_count) };
  }

  async search(query: string, datapack: { id: string; name: string }, agentId: string, conversationId: string): Promise<SearchResult[]> {
    const r = (await this.call('context_search', {
      query,
      conversation_id: conversationId,
      datapack_id: datapack.id,
      agent_id: agentId,
      limit: 8,
    })) as Json;
    const results = (r.results ?? {}) as Json;
    const out: SearchResult[] = [];
    const push = (source: SearchResult['source'], items: unknown) => {
      if (!Array.isArray(items)) return;
      for (const it of items as Json[]) {
        const text = pickText(it);
        if (text) out.push({ source, text, score: pickScore(it), datapackName: datapack.name });
      }
    };
    push('memory', results.memory);
    push('knowledge_base', results.knowledge_base);
    const conv = results.conversation as Json | undefined;
    push('conversation', Array.isArray(conv) ? conv : conv?.hits);
    return out;
  }

  async addMemory(text: string, datapackId: string, agentId: string, conversationId: string, metadata?: Json): Promise<void> {
    await this.call('memory_add', {
      text,
      datapack_id: datapackId,
      agent_id: agentId,
      conversation_id: conversationId,
      metadata: metadata ? JSON.stringify(metadata) : null,
    }, 60000);
  }

  async putArtifact(filename: string, content: string, contentType: string, datapackId: string, agentId: string, conversationId: string): Promise<void> {
    await this.call('artifact_put', {
      filename,
      content_base64: Buffer.from(content, 'utf8').toString('base64'),
      content_type: contentType,
      datapack_id: datapackId,
      agent_id: agentId,
      conversation_id: conversationId,
    }, 60000);
  }
}

function pickText(it: Json): string {
  for (const k of ['memory', 'text', 'content', 'chunk', 'summary', 'message', 'page_content']) {
    const v = it[k];
    if (typeof v === 'string' && v.trim()) return v.trim();
  }
  return '';
}

function pickScore(it: Json): number | null {
  for (const k of ['score', 'similarity', 'distance']) {
    if (typeof it[k] === 'number') return it[k] as number;
  }
  return null;
}

/** Standalone credential check for the installer. */
export async function checkMeko(url: string, apiKey: string): Promise<{ ok: true } | { ok: false; error: string }> {
  const c = new MekoClient(() => ({ url, apiKey }));
  try {
    await c.check();
    return { ok: true };
  } catch (e) {
    return { ok: false, error: e instanceof Error ? e.message : String(e) };
  } finally {
    await c.close();
  }
}
