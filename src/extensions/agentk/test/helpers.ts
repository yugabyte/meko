import { execFileSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import { existsSync, mkdirSync, mkdtempSync, readdirSync, readFileSync, realpathSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { DEFAULT_CONFIG, type Config } from '../src/server/config.js';
import { Core } from '../src/server/core.js';
import { openDb } from '../src/server/db.js';
import { defaultDeps, type InstanceDeps } from '../src/server/instance.js';
import { silentLogger } from '../src/server/log.js';
import { pinSlug, sessionAgentId, type MekoClient, type TokenTry } from '../src/server/meko.js';
import { MekoApi } from '../src/server/mekoapi.js';
import type { MekoAgentsRead } from '../src/server/mekoagents.js';
import type { MekoTools } from '../src/server/mekosettings.js';
import type { LocalMekoProbe } from '../src/server/mekolocal.js';
import { getPaths } from '../src/server/paths.js';
import { startHost, type HostHandle } from '../src/server/host/host.js';
import { SessionHost } from '../src/server/hostclient.js';

export const FAKE_CLAUDE = fileURLToPath(new URL('./fixtures/fake-claude.mjs', import.meta.url));
export const FAKE_KIRO = fileURLToPath(new URL('./fixtures/fake-kiro.mjs', import.meta.url));
export const FAKE_OPENCODE = fileURLToPath(new URL('./fixtures/fake-opencode.mjs', import.meta.url));
export const FAKE_CAPTURE = fileURLToPath(new URL('./fixtures/fake-capture.mjs', import.meta.url));

export function gitRepo(dir: string): string {
  const g = (...a: string[]) => execFileSync('git', a, { cwd: dir, stdio: 'ignore' });
  execFileSync('mkdir', ['-p', dir]);
  g('init', '-q', '-b', 'main');
  g('config', 'user.email', 'test@example.com');
  g('config', 'user.name', 'Test');
  writeFileSync(join(dir, 'README.md'), '# test\n');
  g('add', '.');
  g('commit', '-q', '-m', 'init');
  return dir;
}

/** A call the stand-in Meko client got. */
export interface StubMekoCall {
  name: string;
  args: unknown[];
}

/** In-memory stand-in for the Meko MCP client. */
export function stubMeko(overrides: Partial<Record<keyof MekoClient, unknown>> = {}): MekoClient & { calls: StubMekoCall[] } {
  const calls: StubMekoCall[] = [];
  const rec = (name: string, ret: unknown) => async (...args: unknown[]) => {
    calls.push({ name, args });
    return typeof ret === 'function' ? (ret as (...a: unknown[]) => unknown)(...args) : ret;
  };
  const base = {
    calls,
    check: rec('check', undefined),
    close: rec('close', undefined),
    reset: rec('reset', undefined),
    listDatapacks: rec('listDatapacks', [
      { id: 'dp-shared', name: 'team/kb', grant: 'owner', createdAt: null, ownerName: 'Test', ownerEmail: 'test@example.com', description: null, ownerUserId: 'u-self' },
    ]),
    createDatapack: rec('createDatapack', (name: unknown) => ({ id: `dp-${String(name).replace(/\W+/g, '-')}`, name })),
    setDatapackDescription: rec('setDatapackDescription', undefined),
    deleteDatapack: rec('deleteDatapack', undefined),
    describeDatapack: rec('describeDatapack', { memoryCount: 3, knowledgeCount: 7, grant: 'owner', ownerName: 'Test', ownerEmail: 'test@example.com' }),
    search: rec('search', []),
    addMemory: rec('addMemory', undefined),
    updateMemory: rec('updateMemory', undefined),
    similar: rec('similar', []),
    // Meko keeps a file under its content's SHA-256, which artifact_put returns.
    putArtifact: rec('putArtifact', (_name: unknown, content: unknown) => createHash('sha256').update(String(content), 'utf8').digest('hex')),
    getArtifact: rec('getArtifact', null),
    ensureConversation: rec('ensureConversation', 'conv-1'),
    retitleConversation: rec('retitleConversation', undefined),
    ...overrides,
  };
  return base as unknown as MekoClient & { calls: typeof calls };
}

/** A request the fake Meko REST API got. */
export interface ApiCall {
  method: string;
  path: string;
  body: unknown;
  /** A multipart body (a knowledge-base upload). */
  form?: FormData;
  auth: string | null;
  /** The Content-Type sent, if any. */
  type?: string;
}

export type ApiHandler = (call: ApiCall) => { status?: number; body?: unknown } | undefined;

/**
 * Meko's REST API, faked through `fetch`, and the storage its presigned uploads go to (a call's
 * `path` is the whole URL when it isn't under `baseUrl`). Without a handler (or when it returns
 * undefined) every route answers 401, as Meko does for a key it doesn't accept.
 */
export function fakeMekoApi(baseUrl = 'https://meko.test/api/v1') {
  const calls: ApiCall[] = [];
  const state: { handler: ApiHandler | null } = { handler: null };
  const fetchImpl = (async (input: string | URL | Request, init?: RequestInit) => {
    const url = String(input);
    const headers = (init?.headers ?? {}) as Record<string, string>;
    const call: ApiCall = {
      method: init?.method ?? 'GET',
      path: url.startsWith(baseUrl) ? url.slice(baseUrl.length) : url,
      body: typeof init?.body === 'string' ? JSON.parse(init.body) : undefined,
      form: init?.body instanceof FormData ? init.body : undefined,
      auth: headers.Authorization ?? null,
      type: headers['Content-Type'],
    };
    calls.push(call);
    const r = state.handler?.(call) ?? { status: 401, body: { error: { code: 'UNAUTHORIZED', message: 'invalid or expired token' } } };
    const status = r.status ?? 200;
    // A string body goes as it is (S3's XML); 204 has none.
    const body = status === 204 ? null : typeof r.body === 'string' ? r.body : r.body === undefined ? '' : JSON.stringify(r.body);
    return new Response(body, { status, headers: { 'Content-Type': typeof r.body === 'string' ? 'application/xml' : 'application/json' } });
  }) as typeof fetch;
  const api = new MekoApi(() => ({ baseUrl, apiKey: 'test-key' }), fetchImpl);
  return {
    api,
    calls,
    /** Answers requests from now on (undefined falls back to 401). */
    handle(h: ApiHandler | null) {
      state.handler = h;
    },
  };
}

/** Meko's setup for the agents (Settings › Harness), recorded instead of run. */
export function stubMekoTools() {
  const calls: { name: string; args: unknown[] }[] = [];
  const state: {
    setupError: string | null;
    /** What trying Claude Code's token finds; a token not listed is rejected. */
    tokens: Record<string, TokenTry>;
    /** The command to run by hand each setup was given, in order. */
    manuals: string[];
  } = { setupError: null, tokens: {}, manuals: [] };
  const tools: MekoTools = {
    async setUpClaude(url, apiKey, log, manual) {
      calls.push({ name: 'setUpClaude', args: [url, apiKey] });
      state.manuals.push(manual);
      log('setting up');
      if (state.setupError) throw new Error(state.setupError);
    },
    async setUpKiro(url, apiKey, log, manual) {
      calls.push({ name: 'setUpKiro', args: [url, apiKey] });
      state.manuals.push(manual);
      log('setting up');
      if (state.setupError) throw new Error(state.setupError);
    },
    async tryToken(url, token) {
      calls.push({ name: 'tryToken', args: [url, token] });
      return state.tokens[token] ?? { ok: false, rejected: true, error: 'invalid_token' };
    },
  };
  return { tools, calls, state };
}

/** The MCP URL of the Meko a test env's Core uses (the stub answers its calls; nothing listens there). */
export const TEST_MEKO_URL = 'http://127.0.0.1:7778/mcp';
/** The Meko Cloud account the test env's Meko is linked to (Meko's `cloud.account`). */
export const TEST_ACCOUNT = 'test-account0000';

export interface TestEnv {
  home: string;
  repo: string;
  core: Core;
  meko: ReturnType<typeof stubMeko>;
  mekoApi: ReturnType<typeof fakeMekoApi>;
  mekoTools: ReturnType<typeof stubMekoTools>;
  /**
   * What the Meko on this computer answers when Core reads it (one running at `TEST_MEKO_URL` with
   * the token `test-key`, linked to Meko Cloud as `TEST_ACCOUNT`, at first: Kode's Meko); never
   * the real `meko`.
   */
  local: LocalMekoProbe;
  /** What Meko's list of agents (GET /api/v1/agents) answers when Core reads it: no answer at first, so nothing is gated. */
  mekoAgents: { value: MekoAgentsRead };
  fakeDir: string;
  /** Stops the session host, ending every Claude in it, as a host crash would. The next ensureServer starts it again. */
  killHost(): Promise<void>;
  cleanup(): Promise<void>;
}

/** The Meko datapack pin file for a session's agent id. */
export function sessionPinFile(session: { id: string; slug: string }): string {
  return join(process.env.MEKO_WATERMARK_DIR!, `pin-${pinSlug(sessionAgentId(session))}.json`);
}

/** The datapack id a session's pin names. */
export function pinnedDatapack(session: { id: string; slug: string }): string {
  return JSON.parse(readFileSync(sessionPinFile(session), 'utf8')).datapack_id;
}

/** Every Meko datapack pin file written so far. */
export function pinFiles(): string[] {
  const dir = process.env.MEKO_WATERMARK_DIR!;
  return existsSync(dir) ? readdirSync(dir).filter((f) => f.startsWith('pin-')) : [];
}

/**
 * Lease deps for a host started in a test: its home is new, so no rival can write the lease, and
 * the 1 s read-back each start waits out (most of a short integration test) is skipped.
 */
export function testLockDeps(): InstanceDeps {
  return { ...defaultDeps(), settleMs: 0 };
}

/**
 * A fresh temp dir by its real path. On macOS the temp dir is under /var, a symlink to
 * /private/var, and git and `process.cwd()` report the resolved path.
 */
export function tempDir(prefix: string): string {
  return realpathSync(mkdtempSync(join(tmpdir(), prefix)));
}

/**
 * `kiro`: offer Kiro, run by the fake (`fake-kiro.mjs`) with its home in the test's folder; `opencode`:
 * offer OpenCode, run by `fake-opencode.mjs`. Both off by default. `review`: the code review before
 * a PR opens on its default strategy (`[code_review]`); off by default, so Open PR sends the PR prompt at once.
 * Its findings go to the task at once (`check` off): a session's `review: { check: true }` makes them wait on the user.
 */
export async function makeEnv(opts: { start?: boolean; capture?: boolean; kiro?: boolean; opencode?: boolean; review?: boolean } = {}): Promise<TestEnv> {
  const home = tempDir('akl-test-');
  const repo = gitRepo(join(home, 'code', 'demo-repo'));
  const fakeDir = join(home, 'fake-claude');
  process.env.MEKO_WATERMARK_DIR = join(home, 'meko-capture');
  process.env.FAKE_CLAUDE_DIR = fakeDir;
  // Keep tests away from the real ~/.claude.json (folder trust) and ~/.kiro.
  process.env.CLAUDE_CONFIG_DIR = join(home, 'claude-config');
  process.env.KIRO_HOME = join(home, 'kiro');
  process.env.FAKE_KIRO_DIR = join(home, 'fake-kiro');
  process.env.FAKE_OPENCODE_DIR = join(home, 'fake-opencode');
  // Made here, since the fakes' version reads (which cleanup doesn't wait for) write nothing.
  for (const d of [process.env.FAKE_KIRO_DIR, process.env.FAKE_OPENCODE_DIR]) mkdirSync(d, { recursive: true });
  const paths = getPaths(join(home, 'ak'));
  // The session host runs in this process, and never through this machine's background services.
  const cfg: Config = structuredClone(DEFAULT_CONFIG);
  cfg.repos.roots = [join(home, 'code')];
  cfg.repos.cloneInto = join(home, 'code');
  cfg.claude.command = FAKE_CLAUDE;
  cfg.kiro.command = FAKE_KIRO;
  cfg.kiro.enabled = !!opts.kiro;
  cfg.opencode.command = FAKE_OPENCODE;
  cfg.opencode.enabled = !!opts.opencode;
  cfg.meko.captureTranscripts = opts.capture !== false;
  if (!opts.review) cfg.codeReview.strategy = 'off';
  cfg.codeReview.check = false;
  const db = openDb(join(paths.home, 'state.db'));
  const meko = stubMeko();
  const mekoApi = fakeMekoApi();
  const mekoTools = stubMekoTools();
  const local: LocalMekoProbe = { state: 'running', meko: { url: TEST_MEKO_URL, token: 'test-key' }, cloud: { account: TEST_ACCOUNT, webUrl: 'https://cloud.mekodata.ai' } };
  const mekoAgents: { value: MekoAgentsRead } = { value: { kind: 'failed', error: "Meko isn't running on this computer" } };
  let hostHandle: HostHandle | null = null;
  const host = new SessionHost(paths, {
    services: () => null,
    start: async () => {
      hostHandle ??= await startHost({ paths, version: '0.0.0-test', lockDeps: testLockDeps() });
    },
  });
  const killHost = async () => {
    const h = hostHandle;
    hostHandle = null;
    await h?.close();
  };
  const core = new Core({
    paths, config: cfg, db, version: '0.0.0-test', meko, mekoCaptureScript: opts.capture === false ? null : FAKE_CAPTURE,
    host, services: () => null, log: silentLogger, timers: false, mekoApi: mekoApi.api, mekoTools: mekoTools.tools,
    localMeko: async () => ({ ...local }),
    mekoAgents: async () => mekoAgents.value,
  });
  // Core reads the Meko on this computer at its start; one not started reads it here.
  if (opts.start !== false) await core.start();
  else await core.refreshMeko();
  return {
    home, repo, core, meko, mekoApi, mekoTools, local, mekoAgents, fakeDir, killHost,
    async cleanup() {
      await core.stop().catch(() => undefined);
      await killHost();
      db.close();
      // core.stop() waited for what writes into the home after a test: the transcript captures (into
      // fakeDir), Kiro's and OpenCode's one-shot runs (into FAKE_KIRO_DIR, FAKE_OPENCODE_DIR), and the
      // turn ends and lifecycle steps still running (the diff refresh, a PR poll's git fetch, into
      // the repo's .git); killHost() waited for the fake Claudes it sent SIGHUP to exit (one still
      // starting writes its transcript and session-start.jsonl first). Anything else still writing fails the
      // removal with ENOTEMPTY, retries or not: they only try the folder again, and remove nothing
      // written after their first pass. Name what was left, to find the writer.
      try {
        rmSync(home, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 });
      } catch (e) {
        const left = existsSync(home) ? readdirSync(home, { recursive: true }).join(', ') : 'nothing';
        throw new Error(`removing the test home failed (${(e as Error).message}); left in it: ${left}`, { cause: e });
      }
    },
  };
}

/**
 * The records of a JSONL file that another process appends to, such as a fake agent's logs, or none
 * if it doesn't exist yet. A read can catch an append halfway on Linux (ext4 and tmpfs copy a write in
 * page by page while reads take no lock), so a last line without its newline is a record still being
 * written: it's left for the next read. Every record must end in a newline. Untyped like JSON.parse's
 * result unless T says what the records are.
 */
export function readJsonl<T = ReturnType<typeof JSON.parse>>(file: string): T[] {
  if (!existsSync(file)) return [];
  const text = readFileSync(file, 'utf8');
  return text.slice(0, text.lastIndexOf('\n') + 1).split('\n').filter((l) => l.trim()).map((l) => JSON.parse(l) as T);
}

export async function waitFor<T>(fn: () => T | Promise<T>, pred: (v: T) => boolean, ms = 10000, what = 'condition'): Promise<T> {
  const start = Date.now();
  let last: T | undefined;
  while (Date.now() - start < ms) {
    last = await fn();
    if (pred(last)) return last;
    await new Promise((r) => setTimeout(r, 100));
  }
  throw new Error(`timed out waiting for ${what}; last value: ${JSON.stringify(last)}`);
}

/** A question on the fake Meko's Context Gaps: `askedBy` are the conversations that asked it. */
export interface FakeQuestion {
  key: string;
  query: string;
  quality?: 'empty' | 'weak';
  askedBy?: string[];
  state?: 'open' | 'dismissed' | 'taught';
}

/** A request the fake Meko's Context Gaps routes got. */
export interface FakeQuestionsRequest {
  method: string;
  datapack: string;
  /** What follows `/questions` in the path ('' for the list). */
  path: string;
  query: Record<string, string>;
  body: Record<string, unknown> | null;
  headers: Record<string, string | string[] | undefined>;
}

/**
 * A stand-in for the Meko on this computer's Context Gaps routes (`/api/v1/datapacks/:id/questions`,
 * its mark and its asks), on a port of its own, with the questions of each datapack it has. A datapack
 * it doesn't have, or every request while `old` is set (a Meko too old to keep questions), is a 404.
 */
export async function fakeMekoQuestions() {
  const { createServer } = await import('node:http');
  const datapacks = new Map<string, FakeQuestion[]>();
  const requests: FakeQuestionsRequest[] = [];
  const state = { old: false };
  /** The datapacks the fake keeps as the linked Meko Cloud's: its list says `location: "cloud"`. */
  const cloud = new Set<string>();
  const server = createServer((req, res) => {
    let raw = '';
    req.on('data', (c) => (raw += c));
    req.on('end', () => {
      const u = new URL(req.url ?? '/', 'http://x');
      const m = /^\/api\/v1\/datapacks\/([^/]+)\/questions(\/[a-z]+)?$/.exec(u.pathname);
      const send = (status: number, data?: unknown) => {
        res.writeHead(status, { 'Content-Type': 'application/json' });
        res.end(data === undefined ? undefined : JSON.stringify(status < 300 ? { data } : { error: { code: 'NOT_FOUND', message: 'not found' } }));
      };
      if (!m) return send(404, null);
      const r: FakeQuestionsRequest = {
        method: req.method ?? 'GET', datapack: decodeURIComponent(m[1]), path: m[2] ?? '', query: Object.fromEntries(u.searchParams),
        body: raw ? JSON.parse(raw) : null, headers: req.headers,
      };
      requests.push(r);
      const qs = datapacks.get(r.datapack);
      if (state.old || !qs) return send(404, null);
      if (r.method === 'GET' && r.path === '') {
        const want = r.query.state || 'open';
        const open = qs.filter((q) => (q.state ?? 'open') === 'open');
        return send(200, {
          questions: qs.filter((q) => want === 'all' || (q.state ?? 'open') === want).map((q, i) => ({
            key: q.key, query: q.query, quality: q.quality ?? 'empty', state: q.state ?? 'open', last_at: new Date(10_000 - i).toISOString(),
            asked_by_conversation: !!r.query.conversation_id && (q.askedBy ?? []).includes(r.query.conversation_id),
          })),
          more: false,
          open: { empty: open.filter((q) => (q.quality ?? 'empty') === 'empty').length, weak: open.filter((q) => q.quality === 'weak').length },
          location: cloud.has(r.datapack) ? 'cloud' : 'local',
        });
      }
      if (r.method === 'POST' && r.path === '/mark') {
        const q = qs.find((x) => x.key === r.body?.key);
        if (!q) return send(404, null);
        q.state = (r.body?.state as FakeQuestion['state'] | null) ?? 'open';
        res.writeHead(204).end();
        return;
      }
      if (r.method === 'DELETE' && r.path === '/asks') {
        const conv = r.query.conversation_id;
        const before = qs.length;
        datapacks.set(r.datapack, qs.filter((q) => !(q.askedBy?.length === 1 && q.askedBy[0] === conv)));
        return send(200, { deleted: before - datapacks.get(r.datapack)!.length });
      }
      send(404, null);
    });
  });
  await new Promise<void>((r) => server.listen(0, '127.0.0.1', r));
  const { port } = server.address() as { port: number };
  return {
    url: `http://127.0.0.1:${port}`,
    datapacks,
    requests,
    state,
    cloud,
    close: () => new Promise<void>((r) => {
      server.closeAllConnections();
      server.close(() => r());
    }),
  };
}

/** Makes `env` use the Meko on this computer at `url` (the fake's), with the token `local-token`. */
export async function useLocalMeko(env: TestEnv, url: string): Promise<void> {
  env.local.state = 'running';
  env.local.meko = { url: `${url}/mcp`, token: 'local-token' };
  await env.core.refreshMeko();
}

/** Makes `env` use its usual Meko again (`TEST_MEKO_URL`, `test-key`) after `noMeko` or `useLocalMeko`. */
export async function testMeko(env: TestEnv): Promise<void> {
  env.local.state = 'running';
  env.local.meko = { url: TEST_MEKO_URL, token: 'test-key' };
  await env.core.refreshMeko();
}

/** Makes `env` have no Meko: none running on this computer (`state`: stopped, by default, or not there). */
export async function noMeko(env: TestEnv, state: 'stopped' | 'none' = 'stopped'): Promise<void> {
  env.local.state = state;
  env.local.meko = null;
  await env.core.refreshMeko();
}
