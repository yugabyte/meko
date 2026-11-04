import { symlinkSync } from 'node:fs';
import { join } from 'node:path';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { makeEnv, noMeko, type TestEnv } from '../helpers.js';
import { all, get, run } from '../../src/server/db.js';

describe('projects', () => {
  let env: TestEnv;

  beforeEach(async () => {
    env = await makeEnv({ start: false });
  });
  afterEach(async () => {
    await env.cleanup();
  });

  const create = (name: string, repoPath = env.repo) =>
    env.core.projects.create({ repoPath, name, datapack: { mode: 'existing', id: `dp-${name}`, name } });

  it('puts several projects on one repo folder, stored by its real path however each reaches it, with names unique on it', async () => {
    const link = join(env.home, 'elsewhere');
    symlinkSync(env.repo, link);
    // The first one through the symlink is still named after the repo's own folder.
    const a = await create('Alpha', link);
    expect(a).toMatchObject({ id: 'demo-repo', repoPath: env.repo });
    const b = await create('Beta');
    expect(b).toMatchObject({ id: 'demo-repo-beta', repoPath: env.repo });
    await expect(create('ALPHA', link)).rejects.toThrow(/already has a project named "Alpha"/);
    expect(() => env.core.projects.update(a.id, { name: ' beta ' })).toThrow(/already has a project named "Beta"/);
    const listed = (await env.core.projects.repos()).repos.find((r) => r.path === env.repo);
    expect(listed?.projects.map((p) => p.name).sort()).toEqual(['Alpha', 'Beta']);
    // Renaming a project to its own name (in any case) is fine.
    expect(env.core.projects.update(a.id, { name: 'ALPHA' }).name).toBe('ALPHA');
  });

  it('holds a name while its project is being created, so a second create or a rename to it is refused', async () => {
    const link = join(env.home, 'elsewhere');
    symlinkSync(env.repo, link);
    const other = await create('Other');
    // Meko is slow to answer for the first create's datapack.
    let answer!: () => void;
    const meko = env.meko as { describeDatapack: unknown };
    const describe = meko.describeDatapack;
    meko.describeDatapack = () => new Promise((res) => (answer = () => res({ grant: 'owner' })));
    const first = create('Alpha');
    await vi.waitFor(() => expect(answer).toBeDefined());
    meko.describeDatapack = describe;
    await expect(create(' alpha ', link)).rejects.toThrow(/"alpha" is being opened on that repo/);
    expect(() => env.core.projects.update(other.id, { name: 'ALPHA' })).toThrow(/is being opened on that repo/);
    // A different name goes ahead meanwhile, and the slow one still gets its id once it's written.
    const beta = await create('Beta');
    answer();
    const alpha = await first;
    expect([beta.id, alpha.id]).toEqual(['demo-repo-beta', 'demo-repo-alpha']);
    const listed = (await env.core.projects.repos()).repos.find((r) => r.path === env.repo);
    expect(listed?.projects.map((p) => p.name).sort()).toEqual(['Alpha', 'Beta', 'Other']);
    // Once written, the name is held by the project itself.
    await expect(create('Alpha')).rejects.toThrow(/already has a project named "Alpha"/);
  });

  it('opens a closed project again as itself when its dedicated datapack is picked', async () => {
    const first = await create('Alpha');
    env.core.projects.update(first.id, { settings: { ...first.settings, notify: false } });
    env.core.projects.attachDatapack(first.id, { id: 'dp-kb', name: 'team/kb' });
    run(env.core.db, `UPDATE projects SET meko_conversation_id = 'conv-alpha' WHERE id = ?`, first.id);
    await env.core.projects.delete(first.id);
    expect(() => env.core.projects.get(first.id)).toThrow();

    // Another name and a path through a symlink: still the same project, with what it had.
    const link = join(env.home, 'elsewhere');
    symlinkSync(env.repo, link);
    const again = await env.core.projects.create({ repoPath: link, name: 'Alpha 2', datapack: { mode: 'existing', id: 'dp-Alpha', name: 'Alpha renamed' } });
    expect(again).toMatchObject({ id: first.id, name: 'Alpha 2', repoPath: env.repo, createdAt: first.createdAt });
    expect(again.settings.notify).toBe(false);
    expect(again.datapacks).toEqual([
      { id: 'dp-Alpha', name: 'Alpha renamed', role: 'dedicated' },
      { id: 'dp-kb', name: 'team/kb', role: 'shared' },
    ]);
    expect(get<{ c: string }>(env.core.db, 'SELECT meko_conversation_id AS c FROM projects WHERE id = ?', first.id)?.c).toBe('conv-alpha');
    expect(env.core.state().projects.map((p) => p.id)).toEqual([first.id]);
  });

  it('lists closed projects by their dedicated datapack, the latest closed first', async () => {
    const a = await create('Alpha');
    await env.core.projects.delete(a.id);
    const b = await env.core.projects.create({ repoPath: env.repo, name: 'Beta', datapack: { mode: 'existing', id: 'dp-Alpha', name: 'Alpha' } });
    expect(b.id).toBe(a.id);
    const other = await env.core.projects.create({ repoPath: env.repo, name: 'Gamma', datapack: { mode: 'none' } });
    await env.core.projects.delete(other.id);
    expect(env.core.projects.closed()).toEqual([]);
    await env.core.projects.delete(b.id);
    expect(env.core.projects.closed()).toEqual([{ datapackId: 'dp-Alpha', id: a.id, name: 'Beta', repoPath: env.repo }]);
    const events = all<{ kind: string }>(env.core.db, 'SELECT kind FROM project_events WHERE project_id = ? ORDER BY rowid', a.id);
    expect(events.map((e) => e.kind)).toEqual(['closed', 'reopened', 'closed']);
  });

  it('opens a new project for a datapack no closed project had, or with none', async () => {
    const first = await create('Alpha');
    await env.core.projects.delete(first.id);
    const other = await create('Beta');
    expect(other.id).not.toBe(first.id);
    await env.core.projects.delete(other.id);
    const plain = await env.core.projects.create({ repoPath: env.repo, name: 'Plain', datapack: { mode: 'none' } });
    expect([first.id, other.id]).not.toContain(plain.id);
  });

  it('shows the project home without datapack counts when Meko fails, and asks it with a short timeout', async () => {
    const p = await create('Alpha');
    expect((await env.core.projects.home(p.id)).datapacks[0]).toMatchObject({ memoryCount: 3, knowledgeCount: 7, ownerName: 'Test' });
    expect(env.meko.calls.find((c) => c.name === 'describeDatapack')?.args).toEqual(['dp-Alpha', 5000]);

    (env.meko as { describeDatapack: unknown }).describeDatapack = async () => {
      throw new Error('Meko is down');
    };
    const home = await env.core.projects.home(p.id);
    expect(home.datapacks[0]).toMatchObject({ id: 'dp-Alpha', memoryCount: null, knowledgeCount: null, grant: null });
  });

  it.each([
    { what: 'a datapack the open made', datapack: { mode: 'create', name: 'team/new' }, describe: null, empty: true },
    { what: 'an existing one with no memories and no knowledge', datapack: { mode: 'existing', id: 'dp-e', name: 'team/e' }, describe: { memoryCount: 0, knowledgeCount: 0 }, empty: true },
    { what: 'an existing one with memories', datapack: { mode: 'existing', id: 'dp-e', name: 'team/e' }, describe: { memoryCount: 3, knowledgeCount: 0 }, empty: false },
    { what: 'an existing one Meko fails to describe', datapack: { mode: 'existing', id: 'dp-e', name: 'team/e' }, describe: 'fail', empty: false },
    { what: 'no datapack', datapack: { mode: 'none' }, describe: null, empty: false },
  ] as const)('says whether the opened project\'s datapack is empty: $what', async ({ datapack, describe, empty }) => {
    if (describe === 'fail') {
      (env.meko as { describeDatapack: unknown }).describeDatapack = async () => {
        throw new Error('Meko is down');
      };
    } else if (describe) {
      (env.meko as { describeDatapack: unknown }).describeDatapack = async () => ({ ...describe, grant: 'owner', ownerName: null, ownerEmail: null });
    }
    const p = await env.core.projects.open({ repoPath: env.repo, name: 'Opened', datapack });
    expect(p).toMatchObject({ name: 'Opened', datapackEmpty: empty });
  });

  it("doesn't ask Meko for counts while it's off", async () => {
    const p = await create('Alpha');
    await noMeko(env);
    const before = env.meko.calls.length;
    expect((await env.core.projects.home(p.id)).datapacks[0]).toMatchObject({ memoryCount: null });
    expect(env.meko.calls.slice(before).some((c) => c.name === 'describeDatapack')).toBe(false);
  });

  it("estimates a learn session's PRs and files from the repo's git, and refuses a brief that's off", async () => {
    const p = await create('Demo');
    expect(await env.core.projects.learnScope(p.id, { months: '24', where: 'README.md' }))
      .toEqual({ prs: 1, files: 1, paths: ['README.md'] });
    expect(await env.core.projects.learnScope(p.id, { months: '0', where: '', base: 'main' })).toMatchObject({ prs: 0, files: 1, paths: [] });
    for (const [q, why] of [
      [{ months: '121' }, /whole number from 0 to 120/],
      [{ months: '-1' }, /whole number from 0 to 120/],
      [{}, /whole number from 0 to 120/],
      [{ months: '1', where: 'x'.repeat(4_001) }, /4,000 characters/],
      [{ months: '1', base: 'nope' }, /Branch nope isn't in demo-repo or on its origin/],
      [{ months: '1', base: '-x' }, /isn't a valid branch name/],
      [{ months: '1', repoPath: env.home }, /./],
    ] as const) {
      await expect(env.core.projects.learnScope(p.id, q)).rejects.toMatchObject({ status: 400, message: expect.stringMatching(why) });
    }
  });
});
