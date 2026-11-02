import { existsSync, mkdirSync, readFileSync, rmSync, symlinkSync, writeFileSync } from 'node:fs';
import { homedir } from 'node:os';
import { join } from 'node:path';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { makeEnv, tempDir, waitFor, type TestEnv } from '../helpers.js';
import type { HookPayload } from '../../src/server/hooks.js';

// Stands in for a home under /net: each `from` folder has the local spelling `to` (claudePath),
// a symlink to it here, so both spellings reach the same files as they do on such a machine.
const views = vi.hoisted(() => [] as { from: string; to: string }[]);
vi.mock('../../src/server/paths.js', async (orig) => {
  const real = await orig<typeof import('../../src/server/paths.js')>();
  return {
    ...real,
    claudePath: (p: string) => {
      const v = views.find((x) => p === x.from || p.startsWith(x.from + '/'));
      return v ? v.to + p.slice(v.from.length) : p;
    },
  };
});

describe('a session whose home Claude reaches by its local spelling (real session host + git, fake claude)', () => {
  let env: TestEnv;
  let links: string;
  const local = (p: string) => {
    const v = views.find((x) => p === x.from || p.startsWith(x.from + '/'))!;
    return v.to + p.slice(v.from.length);
  };

  beforeEach(async () => {
    links = tempDir('akl-local-');
    env = await makeEnv({ start: false, review: true });
    for (const [from, name] of [[env.home, 'ak-home'], [homedir(), 'user-home']] as const) {
      symlinkSync(from, join(links, name));
      views.push({ from, to: join(links, name) });
    }
    // Claude Code's folder trust lives here; with no file, AgentK records none.
    mkdirSync(process.env.CLAUDE_CONFIG_DIR!, { recursive: true });
    writeFileSync(join(process.env.CLAUDE_CONFIG_DIR!, '.claude.json'), JSON.stringify({ projects: {} }));
    await env.core.start();
  });
  afterEach(async () => {
    views.length = 0;
    await env.cleanup();
    rmSync(links, { recursive: true, force: true });
  });

  it("starts Claude there, tells it that spelling, and still guards the repo in Design when Claude uses it", async () => {
    const p = await env.core.projects.create({ repoPath: env.repo, name: 'Demo', datapack: { mode: 'existing', id: 'dp-Demo', name: 'team/Demo' } });
    const s = await env.core.sessions.create(p.id, { name: 'Design it', prompt: 'hi', start: 'design' });
    await waitFor(() => env.core.sessions.get(s.id).status, (st) => st === 'idle', 10000, 'the session to start');
    const row = env.core.sessions.row(s.id);

    // HOME in the local spelling (launcher-local-path.test runs a launcher for the worktree and flags).
    const launcher = readFileSync(join(env.core.paths.run, `${s.id}.sh`), 'utf8');
    expect(launcher).toContain(`export HOME='${local(homedir())}'`);
    // The hook socket too: Claude Code refuses a mod's fetch to a socket under /net, so the mod would never reach AgentK.
    expect(launcher).toContain(`export AK_HOOK_SOCK='${local(env.core.paths.hookSock)}'`);

    // Started there, the worktree isn't tied back to its repo's trust, so it's trusted itself.
    const trusted = JSON.parse(readFileSync(join(process.env.CLAUDE_CONFIG_DIR!, '.claude.json'), 'utf8')).projects;
    expect(trusted[local(row.worktree_path)]).toMatchObject({ hasTrustDialogAccepted: true });

    // The design file and pasted images, as Claude is told them.
    expect(env.core.sessions.designFile(row)).toBe(local(join(env.core.paths.designs, s.id, 'design.md')));
    const pasted = env.core.sessions.pasteImage(s.id, Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 1, 2, 3]));
    expect(pasted.path.startsWith(local(env.core.paths.pastes) + '/')).toBe(true);

    // The Design-stage guard: an edit by either spelling of the worktree is denied; the design file is not.
    const edit = (file: string) =>
      env.core.sessions.guard(row.hook_token, { tool_name: 'Edit', tool_input: { file_path: file }, cwd: local(row.worktree_path) } as HookPayload);
    const denied = { hookSpecificOutput: expect.objectContaining({ permissionDecision: 'deny' }) };
    expect(edit(join(local(row.worktree_path), 'README.md'))).toMatchObject(denied);
    expect(edit('README.md')).toMatchObject(denied);
    expect(edit(join(row.worktree_path, 'README.md'))).toMatchObject(denied);
    expect(edit(env.core.sessions.designFile(row))).toBeNull();
  });

  it("names the worktree and the review's folder to a code review's steps by that spelling", async () => {
    const p = await env.core.projects.create({ repoPath: env.repo, name: 'Demo', datapack: { mode: 'none' } });
    const s = await env.core.sessions.create(p.id, { name: 'Fix it', prompt: 'hi', start: 'code', review: { strategy: 'sirius-1.0' } });
    await waitFor(() => env.core.sessions.get(s.id).status, (st) => st === 'idle', 10000, 'the session to start');
    const row = env.core.sessions.row(s.id);
    writeFileSync(join(row.worktree_path, 'a.txt'), 'changed\n');
    // Past what goes in the prompt whole: each file's patch is a file the passes read by its path.
    writeFileSync(join(row.worktree_path, 'big.txt'), 'line\n'.repeat(60_000));
    await env.core.sessions.action(s.id, 'open-pr');
    const review = await waitFor(() => env.core.sessions.get(s.id).codeReview, (r) => !!r, 10000, 'the review to start');
    const base = join(env.core.paths.run, 'reviews', review!.id, 'r1', 'base.md');
    await waitFor(() => existsSync(base), (x) => x, 10000, "the review's base prompt");
    // Claude Code asks before reading through /net, which would stop the review at a permission prompt.
    const text = readFileSync(base, 'utf8');
    expect(text).toContain(`The working directory is ${local(row.worktree_path)}.`);
    expect(text).toContain(local(join(env.core.paths.run, 'reviews', review!.id, 'r1')));
    expect(text).toMatch(new RegExp(`patch: ${local(join(env.core.paths.run, 'reviews', review!.id, 'r1', 'patches'))}/`));
    expect(text).not.toContain(row.worktree_path);
  });
});
