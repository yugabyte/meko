import { readFileSync, rmSync, symlinkSync } from 'node:fs';
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
    env = await makeEnv({ start: false });
    for (const [from, name] of [[env.home, 'ak-home'], [homedir(), 'user-home']] as const) {
      symlinkSync(from, join(links, name));
      views.push({ from, to: join(links, name) });
    }
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

    // The launcher: the worktree, the design folder and HOME in the local spelling.
    const launcher = readFileSync(join(env.core.paths.run, `${s.id}.sh`), 'utf8');
    expect(launcher).toContain(`cd '${local(row.worktree_path)}' || exit 1`);
    expect(launcher).toContain(`'--add-dir' '${local(join(env.core.paths.designs, s.id))}'`);
    expect(launcher).toContain(`export HOME='${local(homedir())}'`);

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
});
