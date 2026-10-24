import { execFileSync } from 'node:child_process';
import { chmodSync, mkdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { claudeLauncher, hooksSettingsPath, transcriptPathFor, writeHookFiles } from '../../src/server/claude.js';
import { shq, writeLauncher } from '../../src/server/launcher.js';
import { getPaths, type Paths } from '../../src/server/paths.js';
import { tempDir } from '../helpers.js';

// Stands in for a home under /net: paths under `from` have the local spelling `to` (claudePath).
const view = vi.hoisted(() => ({ from: '\0', to: '\0' }));
vi.mock('../../src/server/paths.js', async (orig) => {
  const real = await orig<typeof import('../../src/server/paths.js')>();
  return {
    ...real,
    claudePath: (p: string) => (p === view.from || p.startsWith(view.from + '/') ? view.to + p.slice(view.from.length) : p),
  };
});

describe('paths Claude is started with, for a home it reaches by another spelling', () => {
  let root: string;
  let paths: Paths;
  let out: string;
  beforeEach(() => {
    root = tempDir('akl-local-path-');
    // Both spellings must be real folders for the launcher's cd; only the local one is used.
    view.from = join(root, 'net-view');
    view.to = join(root, 'local');
    mkdirSync(view.from, { recursive: true });
    paths = getPaths(join(view.from, 'ak'));
    out = join(root, 'out');
    process.env.CLAUDE_CONFIG_DIR = join(root, 'claude-config');
  });
  afterEach(() => {
    delete process.env.CLAUDE_CONFIG_DIR;
    rmSync(root, { recursive: true, force: true });
  });

  it('starts Claude in the local spelling of the worktree, with local --settings, --add-dir and --plugin-dir', () => {
    const fake = join(root, 'fake-claude');
    writeFileSync(fake, `#!/bin/sh\nfor a in "$@"; do printf '%s\\0' "$a"; done > ${shq(out + '.args')}\nprintf '%s' "$PWD" > ${shq(out + '.pwd')}\n`);
    chmodSync(fake, 0o755);
    const worktree = join(view.from, 'ak', 'worktrees', 'repo.1a2b3c4d', 'wt-1');
    mkdirSync(worktree.replace(view.from, view.to), { recursive: true });
    const script = writeLauncher(paths, claudeLauncher(paths, {
      sessionId: 'demo--x', worktree, claudeCommand: fake, claudeSessionId: 'c0ffee', resume: true, name: 'x', planMode: false,
      addDirs: [join(paths.designs, 'demo--x')], pluginDirs: [join(paths.run, 'mod', 'agentk'), '/elsewhere/plugin'],
      appendSystemPrompt: '', prompt: null, env: {},
    }));
    execFileSync('sh', [script], { env: { ...process.env, HOME: root }, stdio: 'ignore' });
    const local = (p: string) => p.replace(view.from, view.to);
    expect(readFileSync(out + '.pwd', 'utf8')).toBe(local(worktree));
    expect(readFileSync(out + '.args', 'utf8').split('\0').slice(0, -1)).toEqual([
      '--settings', local(hooksSettingsPath(paths)), '--resume', 'c0ffee',
      '--add-dir', local(join(paths.designs, 'demo--x')),
      '--plugin-dir', local(join(paths.run, 'mod', 'agentk')), '--plugin-dir', '/elsewhere/plugin',
    ]);
  });

  it('writes the allow rules for designs and pasted images with the local spelling', () => {
    writeHookFiles(paths);
    const allow: string[] = JSON.parse(readFileSync(hooksSettingsPath(paths), 'utf8')).permissions.allow;
    expect(allow).toEqual([`Edit(/${join(view.to, 'ak', 'designs')}/**)`, 'Artifact', `Read(/${join(view.to, 'ak', 'pastes')}/**)`, 'mcp__agentk_sessions__list']);
  });

  it("finds a transcript under either spelling's folder, the local one when there is none yet", () => {
    const cwd = join(view.from, 'ak', 'worktrees', 'repo.1a2b3c4d', 'wt-1');
    const folder = (dir: string) => join(root, 'claude-config', 'projects', dir.replace(/[^A-Za-z0-9]/g, '-'));
    const localFile = join(folder(cwd.replace(view.from, view.to)), 'c0ffee.jsonl');
    const storedFile = join(folder(cwd), 'c0ffee.jsonl');
    expect(transcriptPathFor(cwd, 'c0ffee')).toBe(localFile);
    // A session launched before AgentK used the local spelling: Claude kept adding to this one.
    mkdirSync(folder(cwd), { recursive: true });
    writeFileSync(storedFile, '{}\n');
    expect(transcriptPathFor(cwd, 'c0ffee')).toBe(storedFile);
    mkdirSync(folder(cwd.replace(view.from, view.to)), { recursive: true });
    writeFileSync(localFile, '{}\n');
    expect(transcriptPathFor(cwd, 'c0ffee')).toBe(localFile);
  });
});
