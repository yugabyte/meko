// Presenter's door to Meko: datapacks and artifacts, through Meko's MCP tools, and the artifact list
// through its REST route (artifact_get only takes a hash, so finding the newest deck.json needs the
// list). The credentials are Presenter's own config (Meko Cloud), else the Meko on this computer.

import { execFile } from 'node:child_process';
import { createHash } from 'node:crypto';
import { existsSync, readFileSync } from 'node:fs';
import { homedir } from 'node:os';
import { Client } from '@modelcontextprotocol/sdk/client/index.js';
import { StreamableHTTPClientTransport, StreamableHTTPError } from '@modelcontextprotocol/sdk/client/streamableHttp.js';
import { ErrorCode, McpError } from '@modelcontextprotocol/sdk/types.js';
import type { DatapackRow } from '../shared/datapacks.js';
import { mekoDirOf, withLocalBin, type Paths } from './paths.js';

export const AGENT_ID = 'presenter';

/** The conversation Presenter's calls name when Meko refuses conversation_create (no default datapack). */
const SERVICE_CONVERSATION = createHash('sha256').update('presenter-service').digest('hex').slice(0, 32);

/** Tools that make something new on every call: never sent again once a call may have reached Meko. */
const NOT_IDEMPOTENT = new Set(['datapack_create']);

/** The request surely never ran: Meko answered it with a 4xx (an expired session), or nothing was listening. */
function notDelivered(e: unknown): boolean {
  if (e instanceof StreamableHTTPError) return typeof e.code === 'number' && e.code >= 400 && e.code < 500;
  const code = (e as { cause?: { code?: unknown } } | null)?.cause?.code;
  return code === 'ECONNREFUSED' || code === 'ENOTFOUND' || code === 'EAI_AGAIN';
}

/**
 * Whether a call that failed on its way to Meko may be sent once more on a fresh connection: never
 * after a timeout (it may still land), and a tool that makes something only when it surely never ran.
 */
export function mayRetry(tool: string, e: unknown, connected: boolean): boolean {
  if (e instanceof McpError && e.code === ErrorCode.RequestTimeout) return false;
  return !NOT_IDEMPOTENT.has(tool) || !connected || notDelivered(e);
}

export class MekoError extends Error {}

export interface ArtifactRow {
  hash: string;
  filename: string;
  size: number;
  createdAt: string;
}

/** What Presenter asks of Meko. The server takes it as a dependency, so tests give it a fake. */
export interface MekoPort {
  /** Whether Meko can be reached now. */
  status(): Promise<'ready' | 'none' | 'failed'>;
  listDatapacks(): Promise<DatapackRow[]>;
  createDatapack(name: string, description: string): Promise<DatapackRow>;
  /** Saves bytes and returns the SHA-256 Meko keeps them under. The same bytes are kept once. */
  putArtifact(datapackId: string, filename: string, bytes: Uint8Array, type: string): Promise<string>;
  /** The bytes kept under a hash, or null when the datapack doesn't have them. */
  getArtifact(datapackId: string, hash: string): Promise<Uint8Array | null>;
  /** The datapack's artifacts named exactly `filename`, newest first. */
  listArtifacts(datapackId: string, filename: string): Promise<ArtifactRow[]>;
}

export interface MekoCreds {
  /** Meko's MCP URL: http://127.0.0.1:7778/mcp, or Meko Cloud's. */
  url: string;
  key: string;
}

export interface PresenterConfig {
  meko?: { url?: string; key?: string };
}

export function loadConfig(paths: Paths): PresenterConfig {
  if (!existsSync(paths.config)) return {};
  try {
    return JSON.parse(readFileSync(paths.config, 'utf8')) as PresenterConfig;
  } catch {
    return {};
  }
}

/**
 * The Meko on this computer, as `meko token --json` reports it (`meko` on PATH or in ~/.local/bin),
 * for the data folder this home is in. Null when there's no `meko`, it isn't running, or it failed.
 */
export function localMeko(paths: Paths, env: NodeJS.ProcessEnv = process.env): Promise<{ state: 'ready' | 'none' | 'failed'; creds: MekoCreds | null }> {
  const mekoDir = mekoDirOf(paths.home);
  const run = { ...env, PATH: withLocalBin(env.PATH ?? '', env.HOME || homedir()), ...(mekoDir ? { MEKO_HOME: mekoDir } : {}) };
  return new Promise((resolve) => {
    execFile('meko', ['token', '--json'], { env: run, timeout: 5000, encoding: 'utf8' }, (err, stdout) => {
      if (err) return resolve({ state: (err as NodeJS.ErrnoException).code === 'ENOENT' ? 'none' : 'failed', creds: null });
      try {
        const r = JSON.parse(stdout) as { url?: unknown; token?: unknown; running?: unknown };
        if (typeof r.url !== 'string' || typeof r.token !== 'string' || !r.token) return resolve({ state: 'failed', creds: null });
        resolve(r.running === true ? { state: 'ready', creds: { url: r.url, key: r.token } } : { state: 'none', creds: null });
      } catch {
        resolve({ state: 'failed', creds: null });
      }
    });
  });
}

/** Whether a Meko URL is the Meko on this computer, whose URL and key `meko token --json` gives. */
export function isLoopback(url: string): boolean {
  try {
    return ['localhost', '127.0.0.1', '[::1]'].includes(new URL(url).hostname);
  } catch {
    return false;
  }
}

/**
 * Meko's URL and key: config.json's (Meko Cloud), else the Meko on this computer as `meko token
 * --json` reports it now. A copy of the local Meko's pair in config.json isn't used: it goes stale
 * when Meko moves to another port or makes a new client token.
 */
export function findMeko(paths: Paths, env: NodeJS.ProcessEnv = process.env): Promise<{ state: 'ready' | 'none' | 'failed'; creds: MekoCreds | null }> {
  const cfg = loadConfig(paths).meko;
  if (cfg?.url && cfg.key && !isLoopback(cfg.url)) return Promise.resolve({ state: 'ready', creds: { url: cfg.url, key: cfg.key } });
  return localMeko(paths, env);
}

type Json = Record<string, unknown>;
const HASH = /^[0-9a-f]{64}$/;

/** Meko over MCP and REST, with credentials found again when a connection fails. */
export class MekoClient implements MekoPort {
  private client: Client | null = null;
  private creds: MekoCreds | null = null;
  private conversation: string | null = null;

  constructor(private find: () => Promise<{ state: 'ready' | 'none' | 'failed'; creds: MekoCreds | null }>, private version: string) {}

  static forPaths(paths: Paths, version: string): MekoClient {
    return new MekoClient(() => findMeko(paths), version);
  }

  async status(): Promise<'ready' | 'none' | 'failed'> {
    try {
      await this.connect();
      return 'ready';
    } catch (e) {
      return e instanceof MekoError && /isn't running|no Meko/.test(e.message) ? 'none' : 'failed';
    }
  }

  private async connect(): Promise<Client> {
    if (this.client) return this.client;
    const found = await this.find();
    if (!found.creds) {
      throw new MekoError(found.state === 'none'
        ? "Meko isn't running here: start it with meko start (or no Meko is installed)."
        : "Meko on this computer didn't answer: check meko status.");
    }
    const transport = new StreamableHTTPClientTransport(new URL(found.creds.url), {
      requestInit: { headers: { Authorization: `Bearer ${found.creds.key}` } },
    });
    const c = new Client({ name: 'presenter', version: this.version });
    await c.connect(transport, { timeout: 15000 });
    c.onclose = () => {
      if (this.client === c) this.client = null;
    };
    this.creds = found.creds;
    this.client = c;
    return c;
  }

  async close(): Promise<void> {
    const c = this.client;
    this.client = null;
    await c?.close().catch(() => undefined);
  }

  /** One MCP tool call, tried once more on a fresh connection when the first fails on its way (mayRetry). */
  async call(name: string, args: Json, timeoutMs = 60000): Promise<Json> {
    for (let attempt = 0; ; attempt++) {
      let c: Client | null = null;
      try {
        c = await this.connect();
        const res = (await c.callTool({ name, arguments: args }, undefined, { timeout: timeoutMs })) as { content?: { type: string; text?: string }[]; isError?: boolean };
        const text = res.content?.find((x) => x.type === 'text')?.text ?? '';
        let parsed: unknown = text;
        try {
          parsed = JSON.parse(text);
        } catch {
          /* plain text */
        }
        if (res.isError) throw new MekoError(typeof parsed === 'string' ? parsed : JSON.stringify(parsed));
        if (parsed && typeof parsed === 'object' && 'error' in parsed) {
          const p = parsed as { error: unknown; detail?: unknown };
          throw new MekoError(`${String(p.error)}${p.detail ? `: ${String(p.detail)}` : ''}`);
        }
        return (parsed && typeof parsed === 'object' ? parsed : {}) as Json;
      } catch (e) {
        if (e instanceof MekoError) throw e;
        // A connection that failed or timed out may be wedged: the next call connects afresh.
        if (c && this.client === c) this.client = null;
        void c?.close().catch(() => undefined);
        if (attempt > 0 || !mayRetry(name, e, c !== null)) throw e instanceof Error ? e : new MekoError(String(e));
      }
    }
  }

  /**
   * The conversation Presenter's calls belong to. conversation_create needs a datapack, or Meko's
   * default one, which the user may have deleted; then a fixed id stands in, since the other tools
   * only use it to group their calls.
   */
  private async conv(): Promise<string> {
    if (this.conversation) return this.conversation;
    try {
      const r = await this.call('conversation_create', { agent_id: AGENT_ID, session_id: 'presenter-service', title: 'Presenter' });
      this.conversation = String(r.id ?? r.conversation_id ?? SERVICE_CONVERSATION);
    } catch (e) {
      if (!(e instanceof MekoError)) throw e;
      this.conversation = SERVICE_CONVERSATION;
    }
    return this.conversation;
  }

  async listDatapacks(): Promise<DatapackRow[]> {
    const out: DatapackRow[] = [];
    let cursor = '';
    for (let page = 0; page < 50; page++) {
      const r = await this.call('datapack_search', { limit: 100, cursor, conversation_id: await this.conv() });
      for (const d of (r.datapacks as Json[] | undefined) ?? []) {
        out.push({ id: String(d.datapack_id ?? ''), name: String(d.datapack_name ?? d.name ?? ''), description: String(d.description ?? '') });
      }
      cursor = String(r.next_cursor ?? '');
      if (!r.has_more || !cursor) break;
    }
    return out.filter((d) => d.id && d.name);
  }

  async createDatapack(name: string, description: string): Promise<DatapackRow> {
    const conversation_id = await this.conv();
    const r = await this.call('datapack_create', { name, conversation_id }, 120000);
    const id = String(r.datapack_id ?? '');
    if (!id) throw new MekoError('datapack_create returned no datapack id');
    // datapack_create takes no description; the deck works without one if this fails.
    if (description) await this.call('datapack_update', { datapack_id: id, description, conversation_id }).catch(() => undefined);
    return { id, name: String(r.name ?? name), description };
  }

  async putArtifact(datapackId: string, filename: string, bytes: Uint8Array, type: string): Promise<string> {
    const r = await this.call('artifact_put', {
      filename,
      content_base64: Buffer.from(bytes).toString('base64'),
      content_type: type,
      datapack_id: datapackId,
      agent_id: AGENT_ID,
      conversation_id: await this.conv(),
    }, 120000);
    const hash = String(r.content_hash ?? '').toLowerCase();
    if (!HASH.test(hash)) throw new MekoError('artifact_put returned no content hash');
    return hash;
  }

  async getArtifact(datapackId: string, hash: string): Promise<Uint8Array | null> {
    let r: Json;
    try {
      r = await this.call('artifact_get', { content_hash: hash, datapack_id: datapackId, agent_id: AGENT_ID, conversation_id: await this.conv() }, 120000);
    } catch (e) {
      if (e instanceof MekoError && /not[_ ]found/i.test(e.message)) return null;
      throw e;
    }
    return typeof r.content_base64 === 'string' ? new Uint8Array(Buffer.from(r.content_base64, 'base64')) : null;
  }

  /** REST: GET /api/v1/datapacks/:id/artifacts?q=, every page, filtered to the exact filename. */
  async listArtifacts(datapackId: string, filename: string): Promise<ArtifactRow[]> {
    await this.connect();
    const creds = this.creds!;
    const base = creds.url.replace(/\/mcp\/?$/, '');
    const out: ArtifactRow[] = [];
    for (let page = 1; page <= 100; page++) {
      const url = `${base}/api/v1/datapacks/${encodeURIComponent(datapackId)}/artifacts?q=${encodeURIComponent(filename)}&page=${page}&page_size=200`;
      const res = await fetch(url, { headers: { Authorization: `Bearer ${creds.key}` }, signal: AbortSignal.timeout(30000) });
      if (!res.ok) throw new MekoError(`Meko's artifact list answered ${res.status}`);
      const body = (await res.json()) as { data?: { items?: Json[]; has_more?: boolean } };
      for (const a of body.data?.items ?? []) {
        if (a.filename !== filename) continue;
        out.push({ hash: String(a.content_hash), filename, size: Number(a.size_bytes ?? 0), createdAt: String(a.created_at ?? '') });
      }
      if (!body.data?.has_more) break;
    }
    // Meko lists them newest first. Its times drop trailing zeros, so a string sort could swap two
    // saves made in the same second.
    return out;
  }
}
