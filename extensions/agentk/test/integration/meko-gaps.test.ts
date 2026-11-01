import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import type { FastifyInstance } from 'fastify';
import { buildServer } from '../../src/server/server.js';
import { run } from '../../src/server/db.js';
import { mekoGapsPath } from '../../src/shared/mekourls.js';
import type { ProjectHome } from '../../src/shared/types.js';
import { fakeMekoQuestions, makeEnv, useLocalMeko, waitFor, type TestEnv } from '../helpers.js';

describe("Meko's Context Gaps in AgentK", () => {
  let env: TestEnv;
  let app: FastifyInstance;
  let meko: Awaited<ReturnType<typeof fakeMekoQuestions>>;

  beforeEach(async () => {
    env = await makeEnv({ start: false });
    app = await buildServer(env.core, { webDir: null, updates: null });
    meko = await fakeMekoQuestions();
  });
  afterEach(async () => {
    await app.close();
    await meko.close();
    await env.cleanup();
  });

  const home = async (projectId: string) => (await app.inject({ method: 'GET', url: `/api/projects/${projectId}/home` })).json() as ProjectHome;
  const project = (name: string, datapack = true) =>
    env.core.projects.create({ repoPath: env.repo, name, datapack: datapack ? { mode: 'existing', id: `dp-${name}`, name: `team/${name}` } : { mode: 'none' } });
  const session = (projectId: string, id: string, purpose: string | null, stage = 'explore') =>
    run(env.core.db, `INSERT INTO sessions(id, project_id, slug, name, claude_session_id, branch, base_branch, worktree_path, tmux_name,
      hook_token, desired, status, stage, created_at, purpose) VALUES (?, ?, ?, ?, ?, 'b', 'main', '/w', 't', 'tok', 'running', 'idle', ?, 0, ?)`,
      id, projectId, id, id, `claude-${id}`, stage, purpose);

  it("counts the dedicated datapack's open questions from the Meko on this computer, and shows none without them", async () => {
    const p = await project('Alpha');
    meko.datapacks.set('dp-Alpha', [
      { key: 'release tag retention', query: 'Release tag retention' },
      { key: 'offline mode', query: 'Offline mode', quality: 'weak' },
      { key: 'old', query: 'Old', state: 'dismissed' },
    ]);
    // With a Meko Cloud key (or any Meko but the one on this computer), Meko isn't asked.
    expect((await home(p.id)).mekoGaps).toBeNull();
    expect(meko.requests).toEqual([]);

    await useLocalMeko(env, meko.url);
    expect((await home(p.id)).mekoGaps).toEqual({ datapackId: 'dp-Alpha', cloud: false, empty: 1, weak: 1 });
    expect(meko.requests.at(-1)).toMatchObject({ method: 'GET', datapack: 'dp-Alpha', query: { state: 'open' }, headers: { authorization: 'Bearer local-token' } });
    // A linked Meko Cloud's datapack: its searches pass through this Meko, which keeps its questions.
    meko.cloud.add('dp-Alpha');
    expect((await home(p.id)).mekoGaps).toEqual({ datapackId: 'dp-Alpha', cloud: true, empty: 1, weak: 1 });
    // The line opens the tab, or for Meko Cloud's datapack, Meko's page for its Context Gaps.
    expect([mekoGapsPath('dp-Alpha'), mekoGapsPath('dp-Alpha', true)]).toEqual(['/datapacks/dp-Alpha/gaps', '/gaps/dp-Alpha']);
    // Nothing open, a Meko too old to keep questions, and a project with no dedicated datapack: no line.
    meko.datapacks.set('dp-Alpha', [{ key: 'old', query: 'Old', state: 'dismissed' }]);
    expect((await home(p.id)).mekoGaps).toBeNull();
    meko.state.old = true;
    expect((await home(p.id)).mekoGaps).toBeNull();
    const plain = await project('Plain', false);
    const asked = meko.requests.length;
    expect((await home(plain.id)).mekoGaps).toBeNull();
    expect(meko.requests).toHaveLength(asked);
    // AgentK's own page is gone.
    expect((await app.inject({ method: 'GET', url: `/api/projects/${p.id}/meko-gaps` })).statusCode).toBe(404);
  });

  it("has Meko forget a learn session's searches while it seeded, once, when it moves on from Explore", async () => {
    const p = await project('Alpha');
    meko.datapacks.set('dp-Alpha', [
      { key: 'how releases work', query: 'How releases work', askedBy: ['conv-claude-seed'] },
      { key: 'offline mode', query: 'Offline mode', askedBy: ['conv-claude-seed', 'conv-claude-task'] },
    ]);
    Object.assign(env.meko, { ensureConversation: async (_agent: string, _dp: string, key: string) => `conv-${key}` });
    await useLocalMeko(env, meko.url);
    session(p.id, 'seed', 'learn');
    run(env.core.db, "INSERT INTO session_stages(session_id, stage, entered_at, moved_by) VALUES ('seed', 'explore', 0, 'auto')");
    session(p.id, 'task', null);
    const forgets = () => meko.requests.filter((r) => r.method === 'DELETE');

    env.core.sessions.setStage('task', 'code', 'you');
    const before = Date.now();
    env.core.sessions.setStage('seed', 'code', 'you');
    await waitFor(forgets, (f) => f.length === 1, 5000, "Meko to be asked to forget the seed's searches");
    expect(forgets()[0]).toMatchObject({ datapack: 'dp-Alpha', path: '/asks', query: { conversation_id: 'conv-claude-seed' } });
    expect(Date.parse(forgets()[0].query.before)).toBeGreaterThanOrEqual(before - 1000);
    // Moved on again, it's a task: nothing more is forgotten, even back through Explore, whose
    // searches are then a task's questions.
    env.core.sessions.setStage('seed', 'explore', 'you');
    env.core.sessions.setStage('seed', 'code', 'you');
    env.core.sessions.setStage('seed', 'pr', 'you');
    await new Promise((r) => setTimeout(r, 100));
    expect(forgets()).toHaveLength(1);
  });
});
