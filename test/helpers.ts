import { execFileSync } from 'node:child_process';
import { mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { DEFAULT_CONFIG, type Config } from '../src/server/config.js';
import { Core } from '../src/server/core.js';
import { openDb } from '../src/server/db.js';
import { silentLogger } from '../src/server/log.js';
import type { MekoClient } from '../src/server/meko.js';
import { getPaths } from '../src/server/paths.js';
import { Tmux } from '../src/server/tmux.js';

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

/** In-memory stand-in for the Meko MCP client. */
export function stubMeko(overrides: Partial<Record<keyof MekoClient, unknown>> = {}): MekoClient & { calls: { name: string; args: unknown[] }[] } {
  const calls: { name: string; args: unknown[] }[] = [];
  const rec = (name: string, ret: unknown) => async (...args: unknown[]) => {
    calls.push({ name, args });
    return typeof ret === 'function' ? (ret as (...a: unknown[]) => unknown)(...args) : ret;
  };
  const base = {
    calls,
    check: rec('check', undefined),
    close: rec('close', undefined),
    listDatapacks: rec('listDatapacks', [{ id: 'dp-shared', name: 'team/kb', grant: 'owner', createdAt: null }]),
    createDatapack: rec('createDatapack', (name: unknown) => ({ id: `dp-${String(name).replace(/\W+/g, '-')}`, name })),
    describeDatapack: rec('describeDatapack', { memoryCount: 3, knowledgeCount: 7 }),
    search: rec('search', []),
    addMemory: rec('addMemory', undefined),
    putArtifact: rec('putArtifact', undefined),
    ensureConversation: rec('ensureConversation', 'conv-1'),
    ...overrides,
  };
  return base as unknown as MekoClient & { calls: typeof calls };
}

export interface TestEnv {
  home: string;
  repo: string;
  core: Core;
  meko: ReturnType<typeof stubMeko>;
  fakeDir: string;
  socket: string;
  cleanup(): Promise<void>;
}

let seq = 0;

export async function makeEnv(opts: { start?: boolean } = {}): Promise<TestEnv> {
  const home = mkdtempSync(join(tmpdir(), 'akl-test-'));
  const repo = gitRepo(join(home, 'code', 'demo-repo'));
  const fakeDir = join(home, 'fake-claude');
  process.env.MEKO_WATERMARK_DIR = join(home, 'meko-capture');
  process.env.FAKE_CLAUDE_DIR = fakeDir;
  // Keep tests away from the real ~/.claude.json (folder trust).
  process.env.CLAUDE_CONFIG_DIR = join(home, 'claude-config');
  const paths = getPaths(join(home, 'ak'));
  const socket = `akl-test-${process.pid}-${seq++}`;
  const cfg: Config = structuredClone(DEFAULT_CONFIG);
  cfg.server.bind = '127.0.0.1';
  cfg.repos.roots = [join(home, 'code')];
  cfg.repos.cloneInto = join(home, 'code');
  cfg.tmux.socket = socket;
  cfg.claude.command = FAKE_CLAUDE;
  cfg.meko.apiKey = 'test-key';
  const db = openDb(join(paths.home, 'state.db'));
  const meko = stubMeko();
  const core = new Core({ paths, config: cfg, db, version: '0.0.0-test', meko, mekoCaptureScript: FAKE_CAPTURE, tmux: new Tmux(socket, paths.tmuxConf), log: silentLogger, timers: false });
  if (opts.start !== false) await core.start();
  return {
    home, repo, core, meko, fakeDir, socket,
    async cleanup() {
      await core.stop().catch(() => undefined);
      try {
        execFileSync('tmux', ['-L', socket, 'kill-server'], { stdio: 'ignore' });
      } catch {
        /* not running */
      }
      db.close();
      rmSync(home, { recursive: true, force: true });
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
