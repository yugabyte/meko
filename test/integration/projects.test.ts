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

  const create = (name: string) =>
    env.core.projects.create({ repoPath: env.repo, name, datapack: { mode: 'existing', id: `dp-${name}`, name } });

  it('keeps project names unique per repo when renaming', async () => {
    const a = await create('Alpha');
    const b = await create('Beta');
    expect(() => env.core.projects.update(b.id, { name: ' alpha ' })).toThrow(/already has a project named "Alpha"/);
    expect(env.core.projects.get(b.id).name).toBe('Beta');
    // Renaming a project to its own name (in any case) is fine.
    expect(env.core.projects.update(a.id, { name: 'ALPHA' }).name).toBe('ALPHA');
  });

  it("shows the project home without datapack counts while Meko is down, without waiting on it", async () => {
    const p = await create('Alpha');
    env.core.mekoStatus = { ...env.core.mekoStatus, ok: false, error: 'down' };
    const before = env.meko.calls.length;
    const home = await env.core.projects.home(p.id);
    expect(home.datapacks[0]).toMatchObject({ id: 'dp-Alpha', memoryCount: null, knowledgeCount: null });
    expect(env.meko.calls.slice(before).some((c) => c.name === 'describeDatapack')).toBe(false);

    env.core.mekoStatus = { ...env.core.mekoStatus, ok: true, error: null };
    expect((await env.core.projects.home(p.id)).datapacks[0]).toMatchObject({ memoryCount: 3, knowledgeCount: 7 });
    expect(env.meko.calls.find((c) => c.name === 'describeDatapack')?.args).toEqual(['dp-Alpha', 5000]);
  });
});
