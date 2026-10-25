// Presenter's door to Meko: datapacks and artifacts, through Meko's MCP tools, and the artifact list
// through its REST route (artifact_get only takes a hash, so finding the newest deck.json needs the
// list). Which Meko is Meko's to say: its Meko Cloud link when linked, else the Meko on this computer,
// both from `meko token --json` (Meko's docs/cloud.md).

import { execFile } from 'node:child_process';
import { createHash } from 'node:crypto';
import { homedir } from 'node:os';
import { Client } from '@modelcontextprotocol/sdk/client/index.js';
import { StreamableHTTPClientTransport, StreamableHTTPError } from '@modelcontextprotocol/sdk/client/streamableHttp.js';
import { ErrorCode, McpError } from '@modelcontextprotocol/sdk/types.js';
import type { DatapackRow, MekoAt } from '../shared/datapacks.js';
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
  /** Which Meko Presenter uses (found even when it can't be reached), or null when there's none. */
  at(): Promise<MekoAt | null>;
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
  /** Meko's REST API base: <origin>/api/v1 here, the link's own for Meko Cloud (another host). */
  apiUrl: string;
  /** The Meko on this computer, or the one Meko links to (Meko Cloud). */
  kind: MekoAt['kind'];
}

export interface Found {
  state: 'ready' | 'none' | 'failed';
  creds: MekoCreds | null;
}

/** Meko's Meko Cloud link: linked, `url` and `apiKey` are set; unlinked (or never set), they're ''. */
export interface CloudLink {
  url: string;
  apiKey: string;
  apiUrl: string;
}

/**
 * What `meko token --json` said. `running`/`stopped`: the Meko on this computer, with its URL and
 * token when it runs. `none`: no `meko`. `failed`: `meko` failed, took too long, or printed something
 * else. `cloud` is undefined from a Meko too old to keep a link, and when Meko couldn't read it
 * (`cloudUnreadable`).
 */
export interface MekoProbe {
  state: 'running' | 'stopped' | 'none' | 'failed';
  local: { url: string; token: string } | null;
  cloud?: CloudLink;
  cloudUnreadable?: true;
}

const str = (v: unknown) => (typeof v === 'string' ? v : '');
const HTTP = /^https?:\/\//;

/** Meko's `cloud`, or undefined when it isn't one (an older Meko has none) or Meko couldn't read it. */
export function cloudOf(v: unknown): CloudLink | undefined {
  if (!v || typeof v !== 'object' || typeof (v as { set?: unknown }).set !== 'boolean' || (v as { error?: unknown }).error) return undefined;
  const c = v as Record<string, unknown>;
  const url = HTTP.test(str(c.url)) ? str(c.url) : '';
  const apiKey = url ? str(c.api_key) : '';
  const apiUrl = apiKey && HTTP.test(str(c.api_url)) ? str(c.api_url) : '';
  return { url: apiKey ? url : '', apiKey, apiUrl };
}

/** Meko's REST API base on the MCP URL's own origin, as the Meko on this computer serves it. */
export function apiUrlOf(mcpUrl: string): string {
  return `${new URL(mcpUrl).origin}/api/v1`;
}

/**
 * Asks `meko token --json` (`meko` on PATH or in ~/.local/bin) about the data folder this home is in.
 * Meko answers it while its server is stopped too, and keeps its token and the link's key in the
 * Keychain on macOS, so Presenter asks rather than read its files.
 */
export function probeMeko(paths: Paths, env: NodeJS.ProcessEnv = process.env): Promise<MekoProbe> {
  const mekoDir = mekoDirOf(paths.home);
  const run = { ...env, PATH: withLocalBin(env.PATH ?? '', env.HOME || homedir()), ...(mekoDir ? { MEKO_HOME: mekoDir } : {}) };
  return new Promise((resolve) => {
    execFile('meko', ['token', '--json'], { env: run, timeout: 5000, encoding: 'utf8' }, (err, stdout) => {
      if (err) return resolve({ state: (err as NodeJS.ErrnoException).code === 'ENOENT' ? 'none' : 'failed', local: null });
      try {
        const r = JSON.parse(stdout) as { url?: unknown; token?: unknown; running?: unknown; cloud?: unknown };
        if (typeof r.url !== 'string' || !HTTP.test(r.url) || typeof r.token !== 'string' || !r.token) return resolve({ state: 'failed', local: null });
        const cloud = cloudOf(r.cloud);
        const unreadable = !!r.cloud && typeof r.cloud === 'object' && !!(r.cloud as { error?: unknown }).error;
        resolve({
          ...(r.running === true ? { state: 'running' as const, local: { url: r.url, token: r.token } } : { state: 'stopped' as const, local: null }),
          ...(cloud ? { cloud } : {}),
          ...(unreadable ? { cloudUnreadable: true as const } : {}),
        });
      } catch {
        resolve({ state: 'failed', local: null });
      }
    });
  });
}

/**
 * Finds Meko's URL and key each time it's called: Meko's Meko Cloud link when linked, else the Meko
 * on this computer. A read that couldn't ask `meko`, or whose link Meko couldn't read, keeps the link
 * last read (in memory only), so a slow `meko` or a locked Keychain doesn't move decks to another
 * Meko. A Meko too old to keep a link means the Meko on this computer.
 */
export function mekoFinder(probe: () => Promise<MekoProbe>): () => Promise<Found> {
  let last: CloudLink | undefined;
  return async () => {
    const p = await probe();
    const keep = !p.cloud && (p.cloudUnreadable || p.state === 'none' || p.state === 'failed');
    last = keep ? last : p.cloud;
    if (last?.url && last.apiKey) return { state: 'ready', creds: { url: last.url, key: last.apiKey, apiUrl: last.apiUrl || apiUrlOf(last.url), kind: 'cloud' } };
    if (p.local) return { state: 'ready', creds: { url: p.local.url, key: p.local.token, apiUrl: apiUrlOf(p.local.url), kind: 'local' } };
    return { state: p.state === 'failed' ? 'failed' : 'none', creds: null };
  };
}

/** Which Meko some credentials reach, as the pages and the drafts name it. */
export function atOf(creds: MekoCreds): MekoAt {
  return { kind: creds.kind, host: new URL(creds.url).host };
}

/** A refusal of Meko Cloud's key, which only linking again fixes. */
function refusedKey(status: number | undefined, creds: MekoCreds): MekoError | null {
  return creds.kind === 'cloud' && (status === 401 || status === 403)
    ? new MekoError("Meko Cloud refused the key: link it again in Meko's Settings › Meko Cloud.") : null;
}

type Json = Record<string, unknown>;
const HASH = /^[0-9a-f]{64}$/;

/** Meko over MCP and REST, with credentials found again when a connection fails. */
export class MekoClient implements MekoPort {
  private client: Client | null = null;
  /** What the last find gave, connected or not. */
  private creds: MekoCreds | null = null;
  private conversation: string | null = null;
  /** Counts reset()s, so a connection made for the Meko before one isn't kept. */
  private gen = 0;

  constructor(private find: () => Promise<Found>, private version: string) {}

  static forPaths(paths: Paths, version: string): MekoClient {
    return new MekoClient(mekoFinder(() => probeMeko(paths)), version);
  }

  /** Which Meko it found last, connected or not: a dropped connection doesn't hold up an edit to reconnect. */
  async at(): Promise<MekoAt | null> {
    if (!this.creds) await this.connect().catch(() => undefined);
    return this.creds ? atOf(this.creds) : null;
  }

  /** The credentials of the Meko Presenter uses now, for Claude's runs; null when it can't be reached. */
  async current(): Promise<MekoCreds | null> {
    try {
      await this.connect();
      return this.creds;
    } catch {
      return null;
    }
  }

  /**
   * Forgets the Meko it found (Meko's link changed): the next call finds it again. The conversation
   * id belongs to the old Meko, so it goes too.
   */
  reset(): void {
    const c = this.client;
    this.client = null;
    this.creds = null;
    this.conversation = null;
    this.gen++;
    void c?.close().catch(() => undefined);
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
    const gen = this.gen;
    const found = await this.find();
    if (gen !== this.gen) return this.connect();
    this.creds = found.creds;
    if (!found.creds) {
      throw new MekoError(found.state === 'none'
        ? "Meko isn't running here: start it with meko start (or no Meko is installed)."
        : "Meko on this computer didn't answer: check meko status.");
    }
    const creds = found.creds;
    const transport = new StreamableHTTPClientTransport(new URL(creds.url), {
      requestInit: { headers: { Authorization: `Bearer ${creds.key}` } },
    });
    const c = new Client({ name: 'presenter', version: this.version });
    try {
      await c.connect(transport, { timeout: 15000 });
    } catch (e) {
      throw (e instanceof StreamableHTTPError && refusedKey(e.code, creds)) || e;
    }
    c.onclose = () => {
      if (this.client === c) this.client = null;
    };
    // A reset() while it connected: this one is for the Meko before.
    if (gen !== this.gen) {
      void c.close().catch(() => undefined);
      return this.connect();
    }
    this.client = c;
    return c;
  }

  /** Stops for good (the server is stopping). */
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

  /**
   * REST: GET <apiUrl>/datapacks/:id/artifacts?q=, every page, filtered to the exact filename. Meko
   * on this computer serves Meko Cloud's routes and shapes, so only the base differs.
   */
  async listArtifacts(datapackId: string, filename: string): Promise<ArtifactRow[]> {
    await this.connect();
    const creds = this.creds!;
    const out: ArtifactRow[] = [];
    for (let page = 1; page <= 100; page++) {
      const url = `${creds.apiUrl}/datapacks/${encodeURIComponent(datapackId)}/artifacts?q=${encodeURIComponent(filename)}&page=${page}&page_size=200`;
      const res = await fetch(url, { headers: { Authorization: `Bearer ${creds.key}` }, signal: AbortSignal.timeout(30000) });
      if (!res.ok) throw refusedKey(res.status, creds) ?? new MekoError(`Meko's artifact list answered ${res.status}`);
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
