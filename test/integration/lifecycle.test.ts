import { execFileSync } from 'node:child_process';
import { existsSync, mkdirSync, readFileSync, writeFileSync } from 'node:fs';
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
    // Like GitHub after a push: the PR's head is the branch tip, unless a test says otherwise.
    prStatus: async (w: string) => ({ ...gh.status!, headRefOid: gh.status!.headRefOid || tip(w) }),
    comment: async (_w: string, _n: number, body: string) => void gh.comments.push(body),
    merge: async (_w: string, url: string) => {
      gh.merges.push(url);
      gh.status = { ...gh.status!, state: 'MERGED' };
    },
    resolveMergeMethod: async () => 'squash' as const,
    detectReviewer: async () => 'agentk' as const,
  };
});

function tip(dir: string): string {
  try {
    return execFileSync('git', ['rev-parse', 'HEAD'], { cwd: dir, encoding: 'utf8' }).trim();
  } catch {
    return '';
  }
}

const { makeEnv, waitFor } = await import('../helpers.js');
const { all, get: dbGet } = await import('../../src/server/db.js');
type TestEnv = Awaited<ReturnType<typeof makeEnv>>;

function status(over: Partial<PrStatus> = {}): PrStatus {
  return {
    number: 7, url: 'https://github.com/acme/demo/pull/7', title: 'Fix the thing', body: 'Summary of the change.',
    state: 'OPEN', reviews: [], comments: [], statusCheckRollup: [{ status: 'COMPLETED', conclusion: 'SUCCESS' }],
    reviewDecision: '', mergeable: 'MERGEABLE', mergeStateStatus: 'CLEAN', headRefOid: '', ...over,
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
    // The refusal came before the request, so it no longer counts.
    const later = new Date(Date.now() + 1000).toISOString();
    gh.status = { ...gh.status!, comments: [...gh.status!.comments, { id: 'c2', author: { login: 'me' }, body: '/agentk review force', createdAt: later }] };
    await env.core.lifecycle.pollReview(env.core.sessions.row(s.id));
    expect(env.core.sessions.get(s.id).reviewState).toBe('waiting');
  });

  it('lists every merge blocker and refuses to merge while there are any', async () => {
    const s = await inReview('Blocked');
    const approved = gh.status!;
    const blockersFor = async (over: Partial<PrStatus>) => {
      gh.status = { ...approved, ...over };
      await env.core.lifecycle.pollReview(env.core.sessions.row(s.id));
      return env.core.sessions.get(s.id).mergeBlockers;
    };
    expect(await blockersFor({})).toEqual([]);
    expect(await blockersFor({ statusCheckRollup: [{ status: 'COMPLETED', conclusion: 'FAILURE' }] })).toEqual(['CI is failing.']);
    expect(await blockersFor({ statusCheckRollup: [{ status: 'IN_PROGRESS' }] })).toEqual(['CI is still running.']);
    expect(await blockersFor({ reviewDecision: 'CHANGES_REQUESTED' })).toEqual(['Changes were requested.']);
    expect(await blockersFor({ mergeable: 'CONFLICTING' })).toEqual(['The branch has merge conflicts.']);
    expect(await blockersFor({ mergeStateStatus: 'BEHIND' })).toEqual(['The branch is behind its base.']);
    expect(await blockersFor({ mergeStateStatus: 'DRAFT' })).toEqual(['The PR is a draft.']);

    // A commit Claude made but didn't push.
    const pushed = tip(env.core.sessions.row(s.id).worktree_path);
    commitIn(env.core.sessions.row(s.id).worktree_path);
    expect(await blockersFor({ headRefOid: pushed })).toEqual(["The branch has commits that aren't pushed to the PR."]);
    // Behind the PR (it has everything the branch has) is fine.
    expect(await blockersFor({ headRefOid: '' })).toEqual([]);

    // A question in the terminal mid-turn.
    env.core.sessions.setStatus(s.id, 'needs_input', 'Permission needed for Bash');
    expect(await blockersFor({})).toEqual(['Claude is waiting on you in the terminal.']);
    env.core.sessions.setStatus(s.id, 'idle');

    gh.status = { ...approved, statusCheckRollup: [{ status: 'COMPLETED', conclusion: 'FAILURE' }] };
    await expect(env.core.sessions.action(s.id, 'merge')).rejects.toMatchObject({ status: 409, message: 'Not ready to merge: CI is failing.' });
    expect(gh.merges).toEqual([]);
    expect(env.core.sessions.row(s.id).stage).toBe('review');
  });

  it('sends one merge when Merge is pressed twice', async () => {
    const s = await inReview('Double merge');
    const results = await Promise.allSettled([env.core.sessions.action(s.id, 'merge'), env.core.sessions.action(s.id, 'merge')]);
    expect(gh.merges).toHaveLength(1);
    expect(results.filter((r) => r.status === 'rejected')).toHaveLength(1);
    expect(env.core.sessions.row(s.id).stage).toBe('done');
  });

  it('keeps the branch when it has commits the merged PR lacks', async () => {
    const s = await inReview('Unpushed');
    const row = env.core.sessions.row(s.id);
    const pushed = tip(row.worktree_path);
    commitIn(row.worktree_path);
    gh.status = { ...gh.status!, state: 'MERGED', headRefOid: pushed };
    await env.core.lifecycle.pollReview(row);
    expect(env.core.sessions.row(s.id).stage).toBe('done');
    expect(existsSync(row.worktree_path)).toBe(false);
    expect(execFileSync('git', ['branch', '--list', row.branch], { cwd: env.repo, encoding: 'utf8' }).trim()).not.toBe('');
    expect(lastEvent(s.id, 'cleanup')).toMatch(/branch .* kept because it has commits PR #7 doesn't have/);
  });

  it('waits for Claude to finish its turn before closing a merged session', async () => {
    const s = await inReview('Merged mid-turn');
    slowTurns();
    await env.core.sessions.submit(s.id, 'still working');
    await waitFor(() => env.core.sessions.get(s.id), (x) => x.status === 'working', 10000, 'working');
    gh.status = { ...gh.status!, state: 'MERGED' };
    await env.core.lifecycle.pollReview(env.core.sessions.row(s.id));
    expect(env.core.sessions.row(s.id)).toMatchObject({ stage: 'merge', desired: 'running' });
    expect(lastEvent(s.id, 'merged')).toBe('PR #7 merged; closing when Claude finishes');
    await env.core.lifecycle.pollReview(env.core.sessions.row(s.id)); // announced once
    await waitFor(() => env.core.sessions.row(s.id), (r) => r.stage === 'done', 10000, 'closed at turn end');
    expect(all(env.core.db, `SELECT 1 FROM session_events WHERE session_id = ? AND kind = 'merged'`, s.id)).toHaveLength(1);
    expect(shipped()).toBe(1);
  });

  it('finishes a cleanup cut short after archiving, without a second summary', async () => {
    const s = await inReview('Cut short');
    env.core.sessions.setStage(s.id, 'merge', 'you');
    // Cleanup saves the summary (and marks it) before archiving; the restart came after that.
    env.core.sessions.event(s.id, 'shipped', 'Saved what shipped');
    await env.core.sessions.archive(env.core.sessions.row(s.id), { keepBranch: true });
    gh.status = { ...gh.status!, state: 'MERGED' };
    (env.core.lifecycle as unknown as { lastPoll: Map<string, number> }).lastPoll.clear(); // as after the restart
    await pollAll();
    expect(env.core.sessions.row(s.id).stage).toBe('done');
    expect(shipped()).toBe(0);
  });

  it('still saves what shipped when the session was archived by hand while cleanup waited', async () => {
    const s = await inReview('Archived by hand');
    env.core.sessions.setStage(s.id, 'merge', 'you');
    await env.core.sessions.archive(env.core.sessions.row(s.id), { keepBranch: true });
    const archivedAt = env.core.sessions.row(s.id).archived_at;
    gh.status = { ...gh.status!, state: 'MERGED' };
    (env.core.lifecycle as unknown as { lastPoll: Map<string, number> }).lastPoll.clear();
    await pollAll();
    expect(env.core.sessions.row(s.id).stage).toBe('done');
    expect(shipped()).toBe(1);
    // Archiving again keeps the first archive time and adds no second "Archived" event.
    expect(env.core.sessions.row(s.id).archived_at).toBe(archivedAt);
    expect(dbGet<{ n: number }>(env.core.db, "SELECT COUNT(*) AS n FROM session_events WHERE session_id = ? AND kind = 'archived'", s.id)!.n).toBe(1);
  });

  it('goes back to Review when a requested merge does not happen', async () => {
    const s = await inReview('Ejected');
    env.core.sessions.setStage(s.id, 'merge', 'you');
    await env.core.lifecycle.pollReview(env.core.sessions.row(s.id));
    expect(env.core.sessions.row(s.id).stage).toBe('merge'); // still in the merge queue
    gh.status = { ...gh.status!, state: 'CLOSED' };
    await env.core.lifecycle.pollReview(env.core.sessions.row(s.id));
    expect(env.core.sessions.row(s.id).stage).toBe('review');
    expect(lastEvent(s.id, 'merge')).toBe('PR #7 was closed without merging');
  });

  it('delivers a review that arrives mid-turn when the turn ends, and only then counts it addressed', async () => {
    const s = await inReview('Queued review');
    slowTurns();
    await env.core.sessions.submit(s.id, 'something else');
    await waitFor(() => env.core.sessions.get(s.id), (x) => x.status === 'working', 10000, 'working');
    gh.status = { ...gh.status!, reviews: [...gh.status!.reviews, finding('r2')] };
    await env.core.lifecycle.pollReview(env.core.sessions.row(s.id));
    expect(env.core.sessions.get(s.id).reviewState).toBe('addressing');
    expect(pendingReview(s.id)).toBe('review'); // kept in the database across restarts
    // The other turn ends: the review goes in, and the state waits for that turn.
    await waitFor(() => transcript(s.id), (t) => t.includes('A review from'), 10000, 'review prompt');
    expect(env.core.sessions.get(s.id).reviewState).toBe('addressing');
    await waitFor(() => env.core.sessions.get(s.id), (x) => x.reviewState === 'addressed' && x.status === 'idle', 10000, 'addressed');
    expect(pendingReview(s.id)).toBeNull();
    expect(transcript(s.id).match(/A review from/g)).toHaveLength(1);
  });

  it('delivers a review queued while Claude was exited once it is back', async () => {
    const s = await inReview('Exited review');
    env.core.sessions.setStatus(s.id, 'exited', 'Claude exited');
    gh.status = { ...gh.status!, reviews: [...gh.status!.reviews, finding('r2')] };
    await env.core.lifecycle.pollReview(env.core.sessions.row(s.id));
    expect(pendingReview(s.id)).toBe('review');
    expect(transcript(s.id)).not.toContain('A review from');
    env.core.sessions.setStatus(s.id, 'idle'); // as after Resume
    await pollAll();
    await waitFor(() => transcript(s.id), (t) => t.includes('A review from'), 10000, 'review prompt');
    await waitFor(() => env.core.sessions.get(s.id), (x) => x.reviewState === 'addressed', 10000, 'addressed');
  });

  it('refuses to approve a design while Claude asks something else', async () => {
    const p = await env.core.projects.create({ repoPath: env.repo, name: 'Demo', datapack: { mode: 'existing', id: 'dp-1', name: 'team/demo' } });
    const s = await env.core.sessions.create(p.id, { name: 'Design', prompt: 'DESIGN_MD it' });
    await waitFor(() => env.core.sessions.get(s.id), (x) => x.status === 'needs_input' && x.hasDesign, 10000, 'design ready');
    env.core.sessions.setStatus(s.id, 'needs_input', 'Permission needed for Bash');
    await expect(env.core.sessions.action(s.id, 'approve-design')).rejects.toMatchObject({ status: 409, message: 'Claude is waiting on you in the terminal. Answer it first.' });
    expect(env.core.sessions.row(s.id).stage).toBe('design');
  });

  it('stays in Code when the PR prompt could not be typed', async () => {
    const p = await env.core.projects.create({ repoPath: env.repo, name: 'Demo', datapack: { mode: 'existing', id: 'dp-1', name: 'team/demo' } });
    const s = await env.core.sessions.create(p.id, { name: 'Flaky tmux', prompt: 'do it', skipDesign: true });
    await waitFor(() => env.core.sessions.get(s.id), (x) => x.status === 'idle', 10000, 'first turn');
    vi.spyOn(env.core.tmux, 'submit').mockRejectedValueOnce(new Error('no pane'));
    await expect(env.core.sessions.action(s.id, 'open-pr')).rejects.toMatchObject({ status: 502 });
    expect(env.core.sessions.get(s.id)).toMatchObject({ stage: 'code', busy: false });
  });

  function commitIn(dir: string) {
    execFileSync('git', ['-c', 'user.email=t@example.com', '-c', 'user.name=T', 'commit', '-q', '--allow-empty', '-m', 'more'], { cwd: dir });
  }
  function slowTurns() {
    mkdirSync(env.fakeDir, { recursive: true });
    writeFileSync(join(env.fakeDir, 'turn-ms'), '1500');
  }
  const finding = (id: string) => ({ id, author: { login: 'yb-agentk-dev' }, state: 'COMMENTED', body: '**1 inline comment — 0 blocker, 1 major.**', submittedAt: new Date().toISOString() });
  const lastEvent = (id: string, kind: string) =>
    dbGet<{ text: string }>(env.core.db, 'SELECT text FROM session_events WHERE session_id = ? AND kind = ? ORDER BY id DESC LIMIT 1', id, kind)?.text;
  const pendingReview = (id: string) => dbGet<{ p: string | null }>(env.core.db, 'SELECT pending_review AS p FROM sessions WHERE id = ?', id)?.p;
  const transcript = (id: string) => readFileSync(join(env.fakeDir, `${env.core.sessions.row(id).claude_session_id}.jsonl`), 'utf8');
  const pollAll = () => (env.core.lifecycle as unknown as { pollAll(): Promise<void> }).pollAll();

  it('refuses Open PR while Claude is working', async () => {
    const p = await env.core.projects.create({ repoPath: env.repo, name: 'Demo', datapack: { mode: 'existing', id: 'dp-1', name: 'team/demo' } });
    mkdirSync(env.fakeDir, { recursive: true });
    writeFileSync(join(env.fakeDir, 'turn-ms'), '3000');
    const s = await env.core.sessions.create(p.id, { name: 'Busy', prompt: 'slow', skipDesign: true });
    await waitFor(() => env.core.sessions.get(s.id), (x) => x.status === 'working', 10000, 'working');
    await expect(env.core.sessions.action(s.id, 'open-pr')).rejects.toThrow(/still working/);
  });
});
