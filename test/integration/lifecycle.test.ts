import { execFileSync } from 'node:child_process';
import { existsSync, mkdirSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { PrStatus } from '../../src/server/github.js';

// GitHub is mocked; git, tmux and the (fake) claude process are real.
const gh = vi.hoisted(() => ({
  pr: null as null | { number: number; url: string; state: 'OPEN' | 'MERGED' | 'CLOSED' },
  status: null as null | PrStatus,
  comments: [] as string[],
  merges: [] as string[],
}));

vi.mock('../../src/server/github.js', async (orig) => {
  const real = await orig<typeof import('../../src/server/github.js')>();
  return {
    ...real,
    findPr: async () => gh.pr,
    prStatus: async () => gh.status!,
    comment: async (_w: string, _n: number, body: string) => void gh.comments.push(body),
    merge: async (_w: string, url: string) => {
      gh.merges.push(url);
      gh.status = { ...gh.status!, state: 'MERGED' };
    },
    resolveMergeMethod: async () => 'squash' as const,
    detectReviewer: async () => 'agentk' as const,
  };
});

const { makeEnv, waitFor } = await import('../helpers.js');
const { all } = await import('../../src/server/db.js');
type TestEnv = Awaited<ReturnType<typeof makeEnv>>;

function status(over: Partial<PrStatus> = {}): PrStatus {
  return {
    number: 7, url: 'https://github.com/acme/demo/pull/7', title: 'Fix the thing', body: 'Summary of the change.',
    state: 'OPEN', reviews: [], comments: [], statusCheckRollup: [{ status: 'COMPLETED', conclusion: 'SUCCESS' }],
    reviewDecision: '', mergeable: 'MERGEABLE', mergeStateStatus: 'CLEAN', ...over,
  };
}

describe('lifecycle: PR → AgentK review → Merge & Close', () => {
  let env: TestEnv;
  beforeEach(async () => {
    gh.pr = null;
    gh.status = null;
    gh.comments = [];
    gh.merges = [];
    env = await makeEnv();
  });
  afterEach(async () => {
    await env.cleanup();
  });

  it('walks a session through every stage', async () => {
    const p = await env.core.projects.create({ repoPath: env.repo, name: 'Demo', datapack: { mode: 'existing', id: 'dp-1', name: 'team/demo' } });
    const s = await env.core.sessions.create(p.id, { name: 'Fix the thing', prompt: 'do it', skipDesign: true });
    const get = () => env.core.sessions.get(s.id);
    await waitFor(get, (x) => x.status === 'idle', 10000, 'first turn');

    // Open PR: Claude gets the PR prompt; when its turn ends the server finds the PR.
    gh.pr = { number: 7, url: 'https://github.com/acme/demo/pull/7', state: 'OPEN' };
    gh.status = status({ comments: [{ id: 'c1', author: { login: 'yb-agentk-dev' }, body: 'AgentK has started reviewing this PR.', createdAt: new Date().toISOString() }] });
    await env.core.sessions.action(s.id, 'open-pr');
    // Under load the stub's short turn may already have ended and moved the session on.
    expect(['pr', 'review']).toContain(get().stage);
    await waitFor(get, (x) => x.stage === 'review', 10000, 'review stage');
    expect(get().prNumber).toBe(7);
    expect(get().reviewState).toBe('in_review');
    expect(get().canMerge).toBe(false);
    expect(get().mergeBlockers).toContain('Waiting for the AgentK review.');

    // AgentK posts a review with findings: Claude is told to address it.
    gh.status = status({
      reviews: [{ id: 'r1', author: { login: 'yb-agentk-dev' }, state: 'COMMENTED', body: '**2 inline comments — 0 blocker, 1 major, 1 minor.**', submittedAt: new Date().toISOString() }],
    });
    await waitFor(get, (x) => x.status === 'idle', 10000, 'idle before poll');
    await env.core.lifecycle.pollReview(env.core.sessions.row(s.id));
    expect(get().reviewState).toBe('addressing');
    await waitFor(get, (x) => x.reviewState === 'addressed' && x.status === 'idle', 10000, 'findings addressed');
    expect(get().canMerge).toBe(true);

    // Request another round posts the command.
    await env.core.sessions.action(s.id, 'request-review');
    expect(gh.comments).toEqual(['/agentk review']);
    expect(get().reviewState).toBe('waiting');

    // The next review is clean: Merge & Close becomes available, merges, then cleans up.
    gh.status = status({
      reviews: [
        ...gh.status.reviews,
        { id: 'r2', author: { login: 'yb-agentk-dev' }, state: 'COMMENTED', body: '**0 inline comments.** Looks good.', submittedAt: new Date().toISOString() },
      ],
    });
    await env.core.lifecycle.pollReview(env.core.sessions.row(s.id));
    expect(get().reviewState).toBe('approved');
    expect(get().canMerge).toBe(true);

    const worktree = get().worktreePath;
    await env.core.sessions.action(s.id, 'merge');
    expect(gh.merges).toEqual(['https://github.com/acme/demo/pull/7']);
    const row = env.core.sessions.row(s.id);
    expect(row.stage).toBe('done');
    expect(row.desired).toBe('archived');
    expect(existsSync(worktree)).toBe(false);
    const branches = execFileSync('git', ['branch', '--list', 'ak/*'], { cwd: env.repo, encoding: 'utf8' });
    expect(branches.trim()).toBe(''); // cleanup deletes the local branch
    const memory = env.meko.calls.find((c) => c.name === 'addMemory');
    expect(String(memory?.args[0])).toMatch(/Shipped in Demo: "Fix the thing" \(PR #7/);
    expect(stages(s.id)).toEqual(['code', 'pr', 'review', 'merge', 'done']);
  });

  /** A session in Review with PR #7 open and approved. */
  async function inReview(name: string) {
    const p = await env.core.projects.create({ repoPath: env.repo, name: 'Demo', datapack: { mode: 'existing', id: 'dp-1', name: 'team/demo' } });
    const s = await env.core.sessions.create(p.id, { name, prompt: 'do it', skipDesign: true });
    await waitFor(() => env.core.sessions.get(s.id), (x) => x.status === 'idle', 10000, 'first turn');
    gh.pr = { number: 7, url: 'https://github.com/acme/demo/pull/7', state: 'OPEN' };
    gh.status = status({ reviews: [{ id: 'r1', author: { login: 'yb-agentk-dev' }, state: 'COMMENTED', body: '**0 inline comments.**', submittedAt: new Date().toISOString() }] });
    await env.core.sessions.action(s.id, 'open-pr');
    await waitFor(() => env.core.sessions.get(s.id), (x) => x.stage === 'review' && x.reviewState === 'approved', 10000, 'review stage');
    return s;
  }

  const stages = (id: string) => all<{ stage: string }>(env.core.db, 'SELECT stage FROM session_stages WHERE session_id = ? ORDER BY rowid', id).map((r) => r.stage);
  const shipped = () => env.meko.calls.filter((c) => c.name === 'addMemory').length;

  it('closes a session whose PR was merged on GitHub', async () => {
    const s = await inReview('Merged elsewhere');
    gh.status = { ...gh.status!, state: 'MERGED' };
    await env.core.lifecycle.pollReview(env.core.sessions.row(s.id));
    expect(env.core.sessions.row(s.id).stage).toBe('done');
    expect(stages(s.id)).toEqual(['code', 'pr', 'review', 'merge', 'done']);
    expect(shipped()).toBe(1);
  });

  it('cleans up once when polls overlap', async () => {
    const s = await inReview('Overlap');
    gh.status = { ...gh.status!, state: 'MERGED' };
    const row = env.core.sessions.row(s.id);
    await Promise.all([env.core.lifecycle.pollReview(row), env.core.lifecycle.pollReview(row)]);
    expect(env.core.sessions.row(s.id).stage).toBe('done');
    expect(shipped()).toBe(1);
    expect(stages(s.id).filter((x) => x === 'merge')).toHaveLength(1);
  });

  it('finishes closing a session left in Merge & Close', async () => {
    // As after a restart between the merge and the cleanup.
    const s = await inReview('Interrupted');
    env.core.sessions.setStage(s.id, 'merge', 'you');
    gh.status = { ...gh.status!, state: 'MERGED' };
    await env.core.lifecycle.pollReview(env.core.sessions.row(s.id));
    expect(env.core.sessions.row(s.id).stage).toBe('done');
    expect(shipped()).toBe(1);
  });

  it('surfaces an AgentK refusal and allows forcing a review', async () => {
    const p = await env.core.projects.create({ repoPath: env.repo, name: 'Demo', datapack: { mode: 'existing', id: 'dp-1', name: 'team/demo' } });
    const s = await env.core.sessions.create(p.id, { name: 'Big change', prompt: 'do it', skipDesign: true });
    await waitFor(() => env.core.sessions.get(s.id), (x) => x.status === 'idle', 10000, 'idle');
    gh.pr = { number: 8, url: 'https://github.com/acme/demo/pull/8', state: 'OPEN' };
    gh.status = status({ number: 8, comments: [{ id: 'c1', author: { login: 'yb-agentk-dev' }, body: 'No review: this PR changes 4000 lines.', createdAt: new Date().toISOString() }] });
    await env.core.sessions.action(s.id, 'open-pr');
    await waitFor(() => env.core.sessions.get(s.id), (x) => x.reviewState === 'refused', 10000, 'refused');
    await env.core.sessions.action(s.id, 'force-review');
    expect(gh.comments).toEqual(['/agentk review force']);
  });

  it('refuses Open PR while Claude is working', async () => {
    const p = await env.core.projects.create({ repoPath: env.repo, name: 'Demo', datapack: { mode: 'existing', id: 'dp-1', name: 'team/demo' } });
    mkdirSync(env.fakeDir, { recursive: true });
    writeFileSync(join(env.fakeDir, 'turn-ms'), '3000');
    const s = await env.core.sessions.create(p.id, { name: 'Busy', prompt: 'slow', skipDesign: true });
    await waitFor(() => env.core.sessions.get(s.id), (x) => x.status === 'working', 10000, 'working');
    await expect(env.core.sessions.action(s.id, 'open-pr')).rejects.toThrow(/still working/);
  });
});
