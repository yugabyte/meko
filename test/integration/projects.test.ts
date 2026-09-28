import { symlinkSync } from 'node:fs';
import { join } from 'node:path';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { makeEnv, type TestEnv } from '../helpers.js';

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

  it('keeps project names unique per repo when renaming', async () => {
    const a = await create('Alpha');
    const b = await create('Beta');
    expect(() => env.core.projects.update(b.id, { name: ' alpha ' })).toThrow(/already has a project named "Alpha"/);
    expect(env.core.projects.get(b.id).name).toBe('Beta');
    // Renaming a project to its own name (in any case) is fine.
    expect(env.core.projects.update(a.id, { name: 'ALPHA' }).name).toBe('ALPHA');
  });

  it('puts several projects on one repo folder, however each reaches it', async () => {
    const link = join(env.home, 'elsewhere');
    symlinkSync(env.repo, link);
    const a = await create('Alpha');
    const b = await create('Beta', link);
    expect(a.id).toBe('demo-repo');
    // The second project on the repo adds its own name to the id, even through the symlink.
    expect(b.id).toBe('demo-repo-beta');
    await expect(create('ALPHA', link)).rejects.toThrow(/already has a project named "Alpha"/);
    expect(() => env.core.projects.update(a.id, { name: 'beta' })).toThrow(/already has a project named "Beta"/);
    const listed = (await env.core.projects.repos()).repos.find((r) => r.path === env.repo);
    expect(listed?.projects.map((p) => p.name).sort()).toEqual(['Alpha', 'Beta']);
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

  it("doesn't ask Meko for counts while it's off", async () => {
    const p = await create('Alpha');
    env.core.updateConfig((c) => {
      c.meko.enabled = false;
    });
    const before = env.meko.calls.length;
    expect((await env.core.projects.home(p.id)).datapacks[0]).toMatchObject({ memoryCount: null });
    expect(env.meko.calls.slice(before).some((c) => c.name === 'describeDatapack')).toBe(false);
  });
});
