import { AsyncLocalStorage } from 'node:async_hooks';
import { createHash } from 'node:crypto';
import { readFileSync, statSync } from 'node:fs';
import { Client } from '@modelcontextprotocol/sdk/client/index.js';
import { StreamableHTTPClientTransport, StreamableHTTPError } from '@modelcontextprotocol/sdk/client/streamableHttp.js';
import { ErrorCode, McpError } from '@modelcontextprotocol/sdk/types.js';
import { harnessOf, type HarnessId } from '../shared/harnesses.js';
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

/** Agent id of AgentK's own Meko calls: its service conversation, a project's conversation and search. */
export const AGENTK_AGENT_ID = 'agent-k-local';

/** The client part of a session's agent id, by its harness (Meko's `<client>:<project>` convention). */
const AGENT_ID_CLIENTS: Record<HarnessId, string> = { claude: 'claude_code', opencode: 'opencode', kiro: 'kiro' };

/**
 * Agent id of a session's Meko calls, capture and datapack pin: its harness's client name and its
 * slug, with a short hash of its id since a slug is only unique within its project. It doesn't name
 * the project: Meko reads memories across agents within a datapack, so the id only says which
 * session wrote one, and every session of the project sees what the others saved.
 */
export function sessionAgentId(session: { id: string; slug: string; harness?: string }): string {
  const client = AGENT_ID_CLIENTS[harnessOf(session.harness)];
  return `${client}:${session.slug}-${createHash('sha256').update(session.id).digest('hex').slice(0, 8)}`;
}

/**
 * The agent id a session's Claude runs with, for AgentK's writes and conversation for it: its own,
 * or for a session launched before sessions had their own, its project's, until it's launched again.
 */
export function launchedAgentId(r: { project_id: string; meko_agent_id: string | null }): string {
  return r.meko_agent_id ?? legacyAgentId(r.project_id);
}

/** The project-wide agent id sessions ran with before they had their own. */
export function legacyAgentId(projectId: string): string {
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

/** One Meko call AgentK made itself (not Claude's), for the usage report: never its content. */
export interface MekoCallRecord {
  tool: string;
  at: number;
  inputChars: number;
  resultChars: number;
  ok: boolean;
  ms: number;
  /** The learn step that made it (mekoTag), if any. */
  tag: string | null;
}

/** Tags AgentK's Meko calls made inside `mekoTag.run(tag, fn)`, such as a learn step's. */
export const mekoTag = new AsyncLocalStorage<string>();

/**
 * The session AgentK's Meko calls made inside `withMekoScope(row, fn)` are for (its Meko call log):
 * its id, and when that start of it was made, since Try again reuses a failed start's id.
 */
export interface MekoScope {
  id: string;
  createdAt: number;
}

export const mekoScope = new AsyncLocalStorage<MekoScope>();

/** Runs `fn` with its Meko calls (and those of the work it starts) counted as the session's. */
export function withMekoScope<T>(session: { id: string; created_at: number }, fn: () => T): T {
  return mekoScope.run({ id: session.id, createdAt: session.created_at }, fn);
}

/** One Meko call AgentK made, with its content, for a session's Meko call log (mekolog.ts). */
export interface MekoTrace {
  tool: string;
  at: number;
  ms: number;
  ok: boolean;
  /** Tries: 2 when the call was sent again after a transport failure. */
  attempts: number;
  args: Json;
  /** The reply's text, or null when none came. */
  text: string | null;
  /** Why it failed. */
  error: string | null;
  /** The learn step that made it (mekoTag), if any. */
  tag: string | null;
  /** The session it was made for (mekoScope), if any. */
  session: MekoScope | null;
}

/** A memory like a given text, as `similar` finds them: what an update needs to name it. */
export interface SimilarMemory {
  id: string;
  text: string;
  /** The agent that wrote it: memory_update must name the writer's agent id. */
  agentId: string | null;
}

/** Session key of the conversation used for server-originated calls. */
const SERVICE_KEY = 'agent-k-local-service';

/** Tools that write something new on every call: never sent twice once a call may have reached Meko. */
const NOT_IDEMPOTENT = new Set(['datapack_create', 'memory_add', 'artifact_put', 'conversation_add_message']);

/** How long connecting (the MCP handshake) may take. */
const CONNECT_TIMEOUT_MS = 15000;

/**
 * The header on every request AgentK's own client sends Meko: Meko records no question on its
 * Context Gaps for these calls (the search at session start, whose query is the task, and the learn
 * step's lookups), only for the agents' own searches. Not a tool argument, which Meko Cloud refuses.
 */
export const MEKO_CLIENT_HEADER = { 'X-Meko-Client': 'agentk' } as const;
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

  /** Told of every call (onCall in Core records them); a failing listener never fails the call. */
  onCall: ((r: MekoCallRecord) => void) | null = null;

  /** Told of every call with what was sent and what came back (the Meko call log); never fails the call either. */
  onTrace: ((t: MekoTrace) => void) | null = null;

  /**
   * Told when connecting fails, before the retry: Core reads the Meko on this computer again then,
   * as it may have restarted on another port or with a new token. Never fails the call.
   */
  onConnectFail: (() => Promise<void>) | null = null;

  constructor(private creds: () => MekoCreds, private version = '0.0.0') {}

  private async connect(): Promise<Client> {
    if (this.client) return this.client;
    if (!this.connecting) {
      const gen = this.gen;
      const p: Promise<Client> = (async () => {
        const { url, apiKey } = this.creds();
        if (!apiKey) throw new MekoError('No Meko API key is configured.');
        const transport = new StreamableHTTPClientTransport(new URL(url), {
          requestInit: { headers: { Authorization: `Bearer ${apiKey}`, ...MEKO_CLIENT_HEADER } },
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
  private async request<T>(idempotent: boolean, fn: (c: Client) => Promise<T>, tried?: (attempts: number) => void): Promise<T> {
    for (let attempt = 0; ; attempt++) {
      let c: Client | null = null;
      tried?.(attempt + 1);
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
        if (c === null) await this.onConnectFail?.().catch(() => undefined);
      }
    }
  }

  async call(name: string, args: Json, timeoutMs = 30000): Promise<unknown> {
    const at = Date.now();
    let text = null as string | null; // (set in a callback, so TS must not narrow it to null)
    let attempts = 0;
    let ok = false;
    let error: string | null = null;
    try {
      const r = await this.callOnce(name, args, timeoutMs, (t) => (text = t), (n) => (attempts = n));
      ok = true;
      return r;
    } catch (e) {
      error = e instanceof Error ? e.message : String(e);
      throw e;
    } finally {
      const ms = Date.now() - at;
      const tag = mekoTag.getStore() ?? null;
      try {
        this.onCall?.({ tool: name, at, inputChars: JSON.stringify(args).length, resultChars: text?.length ?? 0, ok, ms, tag });
      } catch {
        /* the record is best-effort */
      }
      try {
        this.onTrace?.({ tool: name, at, ms, ok, attempts, args, text, error, tag, session: mekoScope.getStore() ?? null });
      } catch {
        /* so is the trace */
      }
    }
  }

  private callOnce(name: string, args: Json, timeoutMs: number, got: (text: string) => void, tried: (attempts: number) => void): Promise<unknown> {
    return this.request(!NOT_IDEMPOTENT.has(name), async (c) => {
      const res = (await c.callTool({ name, arguments: args }, undefined, { timeout: timeoutMs })) as {
        content?: { type: string; text?: string }[];
        isError?: boolean;
      };
      const text = res.content?.find((x) => x.type === 'text')?.text ?? '';
      got(text);
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
    }, tried);
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

  /** Gives a conversation a new title; Meko changes it only when `agentId` owns the conversation. */
  async retitleConversation(agentId: string, datapackId: string, conversationId: string, title: string, timeoutMs?: number): Promise<void> {
    await this.call('conversation_update', { agent_id: agentId, datapack_id: datapackId, conversation_id: conversationId, title }, timeoutMs);
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
      id = await this.ensureConversation(AGENTK_AGENT_ID, null, SERVICE_KEY, 'AgentK service', timeoutMs);
    } catch (e) {
      if (!(e instanceof MekoError)) throw e; // Meko unreachable: try again next time
      id = createHash('sha256').update(SERVICE_KEY).digest('hex').slice(0, 32);
    }
    // Kept only if the Meko URL and key are still the ones it was made with.
    if (gen === this.gen) this.serviceConversation = id;
    return id;
  }

  /** Cheap authenticated round trip to Meko: checks a URL and key (the installer, under a Meko too old to keep the link). */
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
        if (text) out.push({ ...id, source, text, score: pickScore(it), datapackName: datapack.name, datapackId: datapack.id });
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

  /**
   * Memories in a datapack like `query` (context_search, memories only), with the ids and writers
   * an update needs. At most `limit`.
   */
  async similar(query: string, datapackId: string, agentId: string, conversationId: string, limit = 3, timeoutMs = 30000): Promise<SimilarMemory[]> {
    const r = (await this.call('context_search', {
      query,
      conversation_id: conversationId,
      datapack_id: datapackId,
      agent_id: agentId,
      force_source: 'memory',
      limit,
    }, timeoutMs)) as Json;
    const items = ((r.results ?? {}) as Json).memory;
    if (!Array.isArray(items)) return [];
    const out: SimilarMemory[] = [];
    for (const it of items as Json[]) {
      const text = pickText(it);
      if (typeof it.id === 'string' && it.id && text) out.push({ id: it.id, text, agentId: strOrNull(it.agent_id) });
    }
    return out.slice(0, limit);
  }

  /** Overwrites a memory's text. `agentId` must be the agent that wrote it. */
  async updateMemory(memoryId: string, text: string, datapackId: string, agentId: string | null, conversationId: string): Promise<void> {
    await this.call('memory_update', {
      memory_id: memoryId,
      text,
      datapack_id: datapackId,
      agent_id: agentId,
      conversation_id: conversationId,
    }, 60000);
  }

  /**
   * Saves a file to a datapack and returns the hash Meko keeps it under, the only key artifact_get
   * takes: the reply's content_hash, or the file's SHA-256 (which that is) when the reply has none.
   */
  async putArtifact(filename: string, content: string, contentType: string, datapackId: string, agentId: string, conversationId: string): Promise<string> {
    const r = (await this.call('artifact_put', {
      filename,
      content_base64: Buffer.from(content, 'utf8').toString('base64'),
      content_type: contentType,
      datapack_id: datapackId,
      agent_id: agentId,
      conversation_id: conversationId,
    }, 60000)) as Json | null;
    const hash = typeof r?.content_hash === 'string' ? r.content_hash.trim().toLowerCase() : '';
    return /^[0-9a-f]{64}$/.test(hash) ? hash : createHash('sha256').update(content, 'utf8').digest('hex');
  }

  /**
   * A file saved with artifact_put, by its hash, as text: inline in the reply for a small one, or
   * from the local file Meko's client writes a large one to. Null when Meko doesn't have it (or it
   * expired) or it's larger than `maxBytes`; throws when Meko can't be asked.
   */
  async getArtifact(hash: string, datapackId: string, agentId: string, conversationId: string, maxBytes: number, timeoutMs = 30000): Promise<string | null> {
    let r: Json | null;
    try {
      r = (await this.call('artifact_get', { content_hash: hash, datapack_id: datapackId, agent_id: agentId, conversation_id: conversationId }, timeoutMs)) as Json | null;
    } catch (e) {
      if (e instanceof MekoError && /not[_ ]found|expired/i.test(e.message)) return null;
      throw e;
    }
    if (typeof r?.content_base64 === 'string') {
      const bytes = Buffer.from(r.content_base64, 'base64');
      return bytes.length <= maxBytes ? bytes.toString('utf8') : null;
    }
    if (typeof r?.local_path === 'string') {
      try {
        const st = statSync(r.local_path);
        return st.isFile() && st.size <= maxBytes ? readFileSync(r.local_path, 'utf8') : null;
      } catch {
        return null;
      }
    }
    return null;
  }
}

/** A non-blank string as it is, else null. */
export function strOrNull(v: unknown): string | null {
  return typeof v === 'string' && v.trim() ? v : null;
}

function pickText(it: Json): string {
  // chunk_text: a knowledge-base hit, as Meko Cloud and the local Meko send it.
  for (const k of ['memory', 'text', 'content', 'chunk', 'chunk_text', 'summary', 'message', 'page_content']) {
    const v = it[k];
    if (typeof v === 'string' && v.trim()) return v.trim();
  }
  return '';
}

function pickScore(it: Json): number | null {
  // rerank_score: a knowledge-base hit's 0-1 relevance, which beats its distance (lower is closer).
  for (const k of ['score', 'similarity', 'rerank_score', 'distance']) {
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

/** What trying a token on a Meko server found: the datapacks its account sees, or why not. */
export type TokenTry = { ok: true; datapacks: string[] } | { ok: false; rejected: boolean; error: string };

/**
 * Tries a token on a Meko server by listing the datapacks its account sees. `rejected` when Meko
 * refused the token (HTTP 401 or 403), as against Meko being down or slow.
 */
export async function tryMekoToken(url: string, apiKey: string): Promise<TokenTry> {
  const c = new MekoClient(() => ({ url, apiKey }));
  try {
    return { ok: true, datapacks: (await c.listDatapacks()).map((d) => d.id) };
  } catch (e) {
    const rejected = e instanceof StreamableHTTPError && (e.code === 401 || e.code === 403);
    return { ok: false, rejected, error: (e instanceof Error ? e.message : String(e)).slice(0, 200) };
  } finally {
    await c.close();
  }
}
