import { createHash } from 'node:crypto';
import { Client } from '@modelcontextprotocol/sdk/client/index.js';
import { StreamableHTTPClientTransport, StreamableHTTPError } from '@modelcontextprotocol/sdk/client/streamableHttp.js';
import { ErrorCode, McpError } from '@modelcontextprotocol/sdk/types.js';
import type { MekoDatapack, SearchResult } from '../shared/types.js';

export interface MekoCreds {
  url: string;
  apiKey: string;
}

export class MekoError extends Error {}

/** A datapack_list row: what the UI gets, plus the owner's Meko account id (ours on the rows we own). */
export interface MekoDatapackRow extends MekoDatapack {
  ownerUserId: string | null;
}

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

/** Session key of the conversation used for server-originated calls. */
const SERVICE_KEY = 'agent-k-local-service';

/** Tools that write something new on every call: never sent twice once a call may have reached Meko. */
const NOT_IDEMPOTENT = new Set(['datapack_create', 'memory_add', 'artifact_put', 'conversation_add_message']);

/** How long connecting (the MCP handshake) may take. */
const CONNECT_TIMEOUT_MS = 15000;
/** How long the health check waits for Meko. */
const CHECK_TIMEOUT_MS = 10000;

function isTimeout(e: unknown): boolean {
  return e instanceof McpError && e.code === ErrorCode.RequestTimeout;
}

/**
 * The request surely never ran: Meko answered it with an HTTP 4xx (an expired session, a
 * rejected key) or the connection was refused before anything was sent.
 */
function notDelivered(e: unknown): boolean {
  if (e instanceof StreamableHTTPError) return typeof e.code === 'number' && e.code >= 400 && e.code < 500;
  const code = (e as { cause?: { code?: unknown } } | null)?.cause?.code;
  return code === 'ECONNREFUSED' || code === 'ENOTFOUND' || code === 'EAI_AGAIN';
}

/**
 * Thin client over the Meko MCP server (Streamable HTTP). Tools return a JSON
 * string in content[0].text; errors come back as `{"error": ...}` with
 * isError false, so both are checked.
 */
export class MekoClient {
  private client: Client | null = null;
  private connecting: Promise<Client> | null = null;
  /** Bumped by reset(): a connection opened with the credentials from before it is dropped. */
  private gen = 0;
  /** Conversation used for server-originated calls (describe, list). */
  private serviceConversation: string | null = null;

  constructor(private creds: () => MekoCreds, private version = '0.0.0') {}

  private async connect(): Promise<Client> {
    if (this.client) return this.client;
    if (!this.connecting) {
      const gen = this.gen;
      const p: Promise<Client> = (async () => {
        const { url, apiKey } = this.creds();
        if (!apiKey) throw new MekoError('No Meko API key is configured.');
        const transport = new StreamableHTTPClientTransport(new URL(url), {
          requestInit: { headers: { Authorization: `Bearer ${apiKey}` } },
        });
        const c = new Client({ name: 'agent-k-local', version: this.version });
        await c.connect(transport, { timeout: CONNECT_TIMEOUT_MS });
        if (gen !== this.gen) {
          // The URL or key changed while this was connecting: connect again with the new ones.
          void c.close().catch(() => undefined);
          return this.connect();
        }
        c.onclose = () => {
          if (this.client === c) this.client = null;
        };
        this.client = c;
        return c;
      })().finally(() => {
        if (this.connecting === p) this.connecting = null;
      });
      this.connecting = p;
    }
    return this.connecting;
  }

  async close(): Promise<void> {
    const c = this.client;
    this.client = null;
    await c?.close().catch(() => undefined);
  }

  /**
   * Forgets the connection and the service conversation, for when the Meko URL or key changes. A
   * connection still being opened with the old ones is dropped when it's up (see connect).
   */
  async reset(): Promise<void> {
    this.gen++;
    this.connecting = null;
    this.serviceConversation = null;
    await this.close();
  }

  /** Drops a broken client, unless another call already replaced it. */
  private closeIf(c: Client): void {
    if (this.client !== c) return;
    this.client = null;
    void c.close().catch(() => undefined);
  }

  /**
   * Runs one request, reconnecting and trying once more after a transport failure. A timeout
   * is not retried, and a call that isn't idempotent is retried only when it surely never ran.
   */
  private async request<T>(idempotent: boolean, fn: (c: Client) => Promise<T>): Promise<T> {
    for (let attempt = 0; ; attempt++) {
      let c: Client | null = null;
      try {
        c = await this.connect();
        return await fn(c);
      } catch (e) {
        if (e instanceof MekoError) throw e;
        // A timed-out connection may be wedged: drop it so the next call connects afresh, but
        // don't retry (the request may still land).
        if (c) this.closeIf(c);
        if (isTimeout(e)) throw e;
        const retry = attempt === 0 && (idempotent || c === null || notDelivered(e));
        if (!retry) throw e instanceof Error ? e : new MekoError(String(e));
      }
    }
  }

  async call(name: string, args: Json, timeoutMs = 30000): Promise<unknown> {
    return this.request(!NOT_IDEMPOTENT.has(name), async (c) => {
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
    });
  }

  async ensureConversation(agentId: string, datapackId: string | null, key: string, title: string, timeoutMs?: number): Promise<string> {
    const r = (await this.call('conversation_create', {
      agent_id: agentId,
      datapack_id: datapackId,
      session_id: key,
      title,
    }, timeoutMs)) as Json;
    const id = String(r.id ?? r.conversation_id ?? '');
    if (!id) throw new MekoError('conversation_create returned no id');
    return id;
  }

  /**
   * Conversation id for server-originated calls (list, create, describe), which Meko requires.
   * conversation_create needs a datapack (or an account default), which a new user doesn't have
   * yet, so when Meko refuses it a fixed trace id stands in; Meko only uses it to nest traces.
   */
  private async serviceConv(timeoutMs?: number): Promise<string> {
    if (this.serviceConversation) return this.serviceConversation;
    const gen = this.gen;
    let id: string;
    try {
      id = await this.ensureConversation('agent-k-local', null, SERVICE_KEY, 'AgentK Local service', timeoutMs);
    } catch (e) {
      if (!(e instanceof MekoError)) throw e; // Meko unreachable: try again next time
      id = createHash('sha256').update(SERVICE_KEY).digest('hex').slice(0, 32);
    }
    // Kept only if the Meko URL and key are still the ones it was made with.
    if (gen === this.gen) this.serviceConversation = id;
    return id;
  }

  /** Cheap authenticated round trip to Meko: checks a URL and key (the installer, the Meko tab). */
  async check(): Promise<void> {
    await this.request(true, (c) => c.ping({ timeout: CHECK_TIMEOUT_MS }));
  }

  async listDatapacks(): Promise<MekoDatapackRow[]> {
    const r = await this.call('datapack_list', { conversation_id: await this.serviceConv() }, 60000);
    // An unexpected reply is an error, not an empty list: callers act on what's missing from it.
    const arr = Array.isArray(r) ? r : Array.isArray((r as Json)?.datapacks) ? ((r as Json).datapacks as Json[]) : null;
    if (!arr) throw new MekoError(`datapack_list returned an unexpected reply: ${JSON.stringify(r).slice(0, 200)}`);
    return (arr as Json[])
      .map((d) => ({
        id: String(d.datapack_id ?? d.id ?? ''),
        name: String(d.datapack_name ?? d.name ?? ''),
        grant: d.grant == null ? null : String(d.grant),
        createdAt: d.created_at == null ? null : String(d.created_at),
        ownerName: strOrNull(d.datapack_owner_name),
        ownerEmail: strOrNull(d.datapack_owner_email_id),
        description: strOrNull(d.description),
        ownerUserId: strOrNull(d.user_id),
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

  async setDatapackDescription(id: string, description: string): Promise<void> {
    await this.call('datapack_update', { datapack_id: id, description, conversation_id: await this.serviceConv() }, 60000);
  }

  async describeDatapack(id: string, timeoutMs = 30000): Promise<{
    memoryCount: number | null; knowledgeCount: number | null; grant: string | null; ownerName: string | null; ownerEmail: string | null;
  }> {
    const r = (await this.call('datapack_describe', { datapack_id: id, conversation_id: await this.serviceConv(timeoutMs) }, timeoutMs)) as Json;
    const num = (v: unknown) => (typeof v === 'number' ? v : null);
    return {
      memoryCount: num(r.memory_count),
      knowledgeCount: num(r.knowledge_chunk_count ?? r.knowledge_count),
      grant: strOrNull(r.grant),
      ownerName: strOrNull(r.datapack_owner_name),
      ownerEmail: strOrNull(r.datapack_owner_email_id),
    };
  }

  async search(
    query: string, datapack: { id: string; name: string }, agentId: string, conversationId: string, timeoutMs = 30000,
  ): Promise<SearchResult[]> {
    const r = (await this.call('context_search', {
      query,
      conversation_id: conversationId,
      datapack_id: datapack.id,
      agent_id: agentId,
      limit: 8,
    }, timeoutMs)) as Json;
    const results = (r.results ?? {}) as Json;
    const out: SearchResult[] = [];
    const push = (source: SearchResult['source'], items: unknown) => {
      if (!Array.isArray(items)) return;
      for (const it of items as Json[]) {
        const text = pickText(it);
        const id = typeof it.id === 'string' && it.id ? { id: it.id } : {};
        if (text) out.push({ ...id, source, text, score: pickScore(it), datapackName: datapack.name });
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

/** A non-blank string as it is, else null. */
export function strOrNull(v: unknown): string | null {
  return typeof v === 'string' && v.trim() ? v : null;
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
