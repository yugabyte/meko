import { execFileSync } from 'node:child_process';
import { existsSync, mkdtempSync, readdirSync, readFileSync, realpathSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { DEFAULT_CONFIG, type Config } from '../src/server/config.js';
import { Core } from '../src/server/core.js';
import { openDb } from '../src/server/db.js';
import { silentLogger } from '../src/server/log.js';
import { mekoScope, pinSlug, sessionAgentId, type MekoClient, type TokenTry } from '../src/server/meko.js';
import { MekoApi } from '../src/server/mekoapi.js';
import type { MekoTools } from '../src/server/mekosettings.js';
import { getPaths } from '../src/server/paths.js';
import { startHost, type HostHandle } from '../src/server/host/host.js';
import { SessionHost } from '../src/server/hostclient.js';

export const FAKE_CLAUDE = fileURLToPath(new URL('./fixtures/fake-claude.mjs', import.meta.url));
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

/** A call the stand-in Meko client got: `scope` is the session it was made for (mekoScope), if any. */
export interface StubMekoCall {
  name: string;
  args: unknown[];
  scope?: string | null;
}

/** In-memory stand-in for the Meko MCP client. */
export function stubMeko(overrides: Partial<Record<keyof MekoClient, unknown>> = {}): MekoClient & { calls: StubMekoCall[] } {
  const calls: StubMekoCall[] = [];
  const rec = (name: string, ret: unknown) => async (...args: unknown[]) => {
    calls.push({ name, args, scope: mekoScope.getStore()?.id ?? null });
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
    describeDatapack: rec('describeDatapack', { memoryCount: 3, knowledgeCount: 7, grant: 'owner', ownerName: 'Test', ownerEmail: 'test@example.com' }),
    search: rec('search', []),
    addMemory: rec('addMemory', undefined),
    updateMemory: rec('updateMemory', undefined),
    similar: rec('similar', []),
    putArtifact: rec('putArtifact', undefined),
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
  auth: string | null;
}

export type ApiHandler = (call: ApiCall) => { status?: number; body?: unknown } | undefined;

/**
 * Meko's REST API, faked through `fetch`. Without a handler (or when it returns undefined) every
 * route answers 401, as Meko does for a key it doesn't accept.
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
      auth: headers.Authorization ?? null,
    };
    calls.push(call);
    const r = state.handler?.(call) ?? { status: 401, body: { error: { code: 'UNAUTHORIZED', message: 'invalid or expired token' } } };
    return new Response(r.body === undefined ? '' : JSON.stringify(r.body), { status: r.status ?? 200, headers: { 'Content-Type': 'application/json' } });
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

/** The Meko tab's checks and Claude Code setup, recorded instead of run. */
export function stubMekoTools() {
  const calls: { name: string; args: unknown[] }[] = [];
  const state: {
    checkError: string | null; apiError: string | null; setupError: string | null;
    /** What trying each of Claude Code's tokens finds; a token not listed is rejected. */
    tokens: Record<string, TokenTry>;
    /** Whether `claude mcp get meko` finds a server (null: it couldn't be asked). */
    claudeHasMcp: boolean | null;
  } = { checkError: null, apiError: null, setupError: null, tokens: {}, claudeHasMcp: false };
  const tools: MekoTools = {
    async check(url, apiKey) {
      calls.push({ name: 'check', args: [url, apiKey] });
      return state.checkError ? { ok: false, error: state.checkError } : { ok: true };
    },
    async checkApi(apiUrl) {
      calls.push({ name: 'checkApi', args: [apiUrl] });
      return state.apiError ? { ok: false, error: state.apiError } : { ok: true };
    },
    async setUpClaude(url, apiKey, log) {
      calls.push({ name: 'setUpClaude', args: [url, apiKey] });
      log('setting up');
      if (state.setupError) throw new Error(state.setupError);
    },
    async tryToken(url, token) {
      calls.push({ name: 'tryToken', args: [url, token] });
      return state.tokens[token] ?? { ok: false, rejected: true, error: 'invalid_token' };
    },
    async claudeHasMcp(command) {
      calls.push({ name: 'claudeHasMcp', args: [command] });
      return state.claudeHasMcp;
    },
  };
  return { tools, calls, state };
}

export interface TestEnv {
  home: string;
  repo: string;
  core: Core;
  meko: ReturnType<typeof stubMeko>;
  mekoApi: ReturnType<typeof fakeMekoApi>;
  mekoTools: ReturnType<typeof stubMekoTools>;
  fakeDir: string;
  /** The tmux socket name this env's server would move sessions off (legacytmux.ts). */
  socket: string;
  /** Stops the session host, ending every Claude in it, as a host crash would. The next ensureServer starts it again. */
  killHost(): Promise<void>;
  cleanup(): Promise<void>;
}

let seq = 0;

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
 * A fresh temp dir by its real path. On macOS the temp dir is under /var, a symlink to
 * /private/var, and git and `process.cwd()` report the resolved path.
 */
export function tempDir(prefix: string): string {
  return realpathSync(mkdtempSync(join(tmpdir(), prefix)));
}

export async function makeEnv(opts: { start?: boolean; capture?: boolean } = {}): Promise<TestEnv> {
  const home = tempDir('akl-test-');
  const repo = gitRepo(join(home, 'code', 'demo-repo'));
  const fakeDir = join(home, 'fake-claude');
  process.env.MEKO_WATERMARK_DIR = join(home, 'meko-capture');
  process.env.FAKE_CLAUDE_DIR = fakeDir;
  // Keep tests away from the real ~/.claude.json (folder trust).
  process.env.CLAUDE_CONFIG_DIR = join(home, 'claude-config');
  const paths = getPaths(join(home, 'ak'));
  const socket = `akl-test-${process.pid}-${seq++}`;
  // The session host runs in this process, and never through this machine's background services.
  const cfg: Config = structuredClone(DEFAULT_CONFIG);
  // A network bind, as on a remote-access install: a loopback one skips the login (localAccess).
  cfg.server.bind = '0.0.0.0';
  cfg.repos.roots = [join(home, 'code')];
  cfg.repos.cloneInto = join(home, 'code');
  cfg.claude.command = FAKE_CLAUDE;
  cfg.meko.apiKey = 'test-key';
  cfg.meko.captureTranscripts = opts.capture !== false;
  const db = openDb(join(paths.home, 'state.db'));
  const meko = stubMeko();
  const mekoApi = fakeMekoApi();
  const mekoTools = stubMekoTools();
  let hostHandle: HostHandle | null = null;
  const host = new SessionHost(paths, {
    services: () => null,
    start: async () => {
      hostHandle ??= await startHost({ paths, version: '0.0.0-test' });
    },
  });
  const killHost = async () => {
    const h = hostHandle;
    hostHandle = null;
    await h?.close();
  };
  const core = new Core({
    paths, config: cfg, db, version: '0.0.0-test', meko, mekoCaptureScript: opts.capture === false ? null : FAKE_CAPTURE,
    host, services: () => null, legacyTmuxSocket: socket, log: silentLogger, timers: false, mekoApi: mekoApi.api, mekoTools: mekoTools.tools,
  });
  if (opts.start !== false) await core.start();
  return {
    home, repo, core, meko, mekoApi, mekoTools, fakeDir, socket, killHost,
    async cleanup() {
      await core.stop().catch(() => undefined);
      await killHost();
      db.close();
      // core.stop() waited for the transcript captures that write into fakeDir; the retries cover
      // any other late writer (removing a folder while something writes into it fails with ENOTEMPTY).
      rmSync(home, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 });
    },
  };
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
