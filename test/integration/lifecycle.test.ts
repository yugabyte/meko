import { execFileSync } from 'node:child_process';
import { existsSync, mkdirSync, readFileSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { PrStatus } from '../../src/server/github.js';
import type { ReviewerMode } from '../../src/shared/types.js';

// GitHub is mocked; git, tmux and the (fake) claude process are real.
const gh = vi.hoisted(() => ({
  pr: null as null | { number: number; url: string; state: 'OPEN' | 'MERGED' | 'CLOSED' },
  status: null as null | PrStatus,
  comments: [] as string[],
  merges: [] as string[],
  bots: [] as string[],
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
    botLogins: async () => gh.bots,
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
    gh.bots = [];
    env = await makeEnv();
  });
  afterEach(async () => {
    await env.cleanup();
  });

  it('walks a session through every stage', async () => {
    const p = await env.core.projects.create({ repoPath: env.repo, name: 'Demo', datapack: { mode: 'existing', id: 'dp-1', name: 'team/demo' } });
    const s = await env.core.sessions.create(p.id, { name: 'Fix the thing', prompt: 'do it', kind: 'quick' });
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

  /** A session in Review with PR #7 open and approved by AgentK (unless AgentK isn't listened to). */
  async function inReview(name: string, reviewer?: ReviewerMode) {
    const p = await env.core.projects.create({ repoPath: env.repo, name: 'Demo', datapack: { mode: 'existing', id: 'dp-1', name: 'team/demo' } });
    if (reviewer) env.core.projects.update(p.id, { settings: { reviewer } });
    const s = await env.core.sessions.create(p.id, { name, prompt: 'do it', kind: 'quick' });
    await waitFor(() => env.core.sessions.get(s.id), (x) => x.status === 'idle', 10000, 'first turn');
    gh.pr = { number: 7, url: 'https://github.com/acme/demo/pull/7', state: 'OPEN' };
    gh.status = status({ reviews: [{ id: 'r1', author: { login: 'yb-agentk-dev' }, state: 'COMMENTED', body: '**0 inline comments.**', submittedAt: new Date().toISOString() }] });
    await env.core.sessions.action(s.id, 'open-pr');
    await waitFor(() => env.core.sessions.get(s.id), (x) => x.stage === 'review' && (reviewer === 'people' || x.reviewState === 'approved') && x.status === 'idle', 10000, 'review stage');
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
    const s = await env.core.sessions.create(p.id, { name: 'Big change', prompt: 'do it', kind: 'quick' });
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
    // Claude also gets the CI prompt, so it's working on the failure.
    expect(await blockersFor({ statusCheckRollup: [{ status: 'COMPLETED', conclusion: 'FAILURE' }] })).toEqual(['AgentK is still working.', 'CI is failing.']);
    await waitFor(() => env.core.sessions.get(s.id), (x) => x.status === 'idle', 10000, 'CI prompt done');
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
    expect(await blockersFor({})).toEqual(['AgentK is waiting on you in the terminal.']);
    env.core.sessions.setStatus(s.id, 'idle');

    gh.status = { ...approved, statusCheckRollup: [{ status: 'COMPLETED', conclusion: 'FAILURE' }] };
    await expect(env.core.sessions.action(s.id, 'merge')).rejects.toMatchObject({ status: 409, message: expect.stringContaining('CI is failing.') as string });
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
    expect(lastEvent(s.id, 'merged')).toBe('PR #7 merged; closing when AgentK finishes');
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
    expect(queued(s.id).review?.state).toBe('queued'); // kept in the database across restarts
    // The other turn ends: the review goes in, and the state waits for that turn.
    await waitFor(() => transcript(s.id), (t) => t.includes('A review from'), 10000, 'review prompt');
    expect(env.core.sessions.get(s.id).reviewState).toBe('addressing');
    await waitFor(() => env.core.sessions.get(s.id), (x) => x.reviewState === 'addressed' && x.status === 'idle', 10000, 'addressed');
    expect(queued(s.id)).toEqual({});
    expect(transcript(s.id).match(/A review from/g)).toHaveLength(1);
  });

  it('delivers a review queued while Claude was exited once it is back', async () => {
    const s = await inReview('Exited review');
    env.core.sessions.setStatus(s.id, 'exited', 'Claude exited');
    gh.status = { ...gh.status!, reviews: [...gh.status!.reviews, finding('r2')] };
    await env.core.lifecycle.pollReview(env.core.sessions.row(s.id));
    expect(queued(s.id).review?.state).toBe('queued');
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
    await expect(env.core.sessions.action(s.id, 'approve-design')).rejects.toMatchObject({ status: 409, message: 'AgentK is waiting on you in the terminal. Answer it first.' });
    expect(env.core.sessions.row(s.id).stage).toBe('design');
  });

  it('stays in Code when the PR prompt could not be typed', async () => {
    const p = await env.core.projects.create({ repoPath: env.repo, name: 'Demo', datapack: { mode: 'existing', id: 'dp-1', name: 'team/demo' } });
    const s = await env.core.sessions.create(p.id, { name: 'Flaky tmux', prompt: 'do it', kind: 'quick' });
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
  const queued = (id: string) =>
    JSON.parse(dbGet<{ q: string }>(env.core.db, 'SELECT pending_prompts AS q FROM sessions WHERE id = ?', id)!.q) as Record<string, { state: string; vars?: Record<string, string> }>;
  const transcript = (id: string) => readFileSync(join(env.fakeDir, `${env.core.sessions.row(id).claude_session_id}.jsonl`), 'utf8');
  const pollAll = () => (env.core.lifecycle as unknown as { pollAll(): Promise<void> }).pollAll();

  const review = (id: string, login: string, body: string, state = 'COMMENTED') => ({ id, author: { login }, state, body, submittedAt: new Date().toISOString() });
  const failing = (over: Record<string, string> = {}) => ({
    __typename: 'CheckRun', name: 'test (macos-15)', status: 'COMPLETED', conclusion: 'FAILURE',
    detailsUrl: 'https://github.com/acme/demo/actions/runs/1/job/2', ...over,
  });
  const idle = (id: string, what: string) => waitFor(() => env.core.sessions.get(id), (x) => x.status === 'idle', 10000, what);
  const poll = (id: string) => env.core.lifecycle.pollReview(env.core.sessions.row(id));
  const count = (text: string, re: RegExp) => text.match(re)?.length ?? 0;

  it('sends reviews from people, but not other bots, to Claude in AgentK + People', async () => {
    const s = await inReview('People too');
    gh.bots = ['yb-agentk-dev[bot]', 'copilot[bot]'];
    gh.status = { ...gh.status!, reviews: [...gh.status!.reviews, review('h1', 'copilot', 'Consider renaming x.'), review('h2', 'me', ''), review('h3', 'alice', 'Please add a test.')] };
    await poll(s.id);
    expect(lastEvent(s.id, 'review')).toBe('Review from alice');
    await waitFor(() => transcript(s.id), (t) => t.includes('Reviewers left feedback'), 10000, 'people prompt');
    await waitFor(() => env.core.sessions.get(s.id), (x) => x.reviewState === 'addressed' && x.status === 'idle', 10000, 'addressed');
    // An empty review (a reply on a thread) and a bot's review send nothing more.
    gh.status = { ...gh.status!, reviews: [...gh.status!.reviews, review('h4', 'me', ''), review('h5', 'copilot', 'Another nit.')] };
    await poll(s.id);
    expect(count(transcript(s.id), /Reviewers left feedback/g)).toBe(1);
  });

  it('sends other bots\' reviews too in All', async () => {
    const s = await inReview('Everyone', 'all');
    gh.bots = ['copilot[bot]'];
    gh.status = { ...gh.status!, reviews: [...gh.status!.reviews, review('h1', 'copilot', 'Consider renaming x.')] };
    await poll(s.id);
    expect(lastEvent(s.id, 'review')).toBe('Review from copilot');
    await waitFor(() => transcript(s.id), (t) => t.includes('Reviewers left feedback'), 10000, 'people prompt');
  });

  it('ignores AgentK in People only and never waits for it', async () => {
    const s = await inReview('No AgentK', 'people');
    gh.status = { ...gh.status!, reviews: [...gh.status!.reviews, finding('r2')] };
    await poll(s.id);
    expect(env.core.sessions.get(s.id)).toMatchObject({ reviewState: 'waiting', mergeBlockers: [] });
    expect(queued(s.id)).toEqual({});
    expect(transcript(s.id)).not.toContain('A review from');
  });

  it('waits for AgentK only once it is active on the PR', async () => {
    const p = await env.core.projects.create({ repoPath: env.repo, name: 'Demo', datapack: { mode: 'existing', id: 'dp-1', name: 'team/demo' } });
    expect(p.settings.reviewer).toBe('agentk-people');
    const s = await env.core.sessions.create(p.id, { name: 'Unwatched repo', prompt: 'do it', kind: 'quick' });
    await idle(s.id, 'first turn');
    gh.pr = { number: 7, url: 'https://github.com/acme/demo/pull/7', state: 'OPEN' };
    gh.status = status();
    await env.core.sessions.action(s.id, 'open-pr');
    await waitFor(() => env.core.sessions.get(s.id), (x) => x.stage === 'review' && x.status === 'idle', 10000, 'review stage');
    await poll(s.id);
    expect(env.core.sessions.get(s.id)).toMatchObject({ reviewState: 'waiting', agentkSeen: false, canMerge: true });
    await env.core.sessions.action(s.id, 'request-review');
    expect(gh.comments).toEqual(['/agentk review']);
    await poll(s.id);
    expect(env.core.sessions.get(s.id)).toMatchObject({ agentkSeen: true, mergeBlockers: ['Waiting for the AgentK review.'] });
  });

  it('refuses an unknown reviewer mode', async () => {
    const p = await env.core.projects.create({ repoPath: env.repo, name: 'Demo', datapack: { mode: 'existing', id: 'dp-1', name: 'team/demo' } });
    expect(() => env.core.projects.update(p.id, { settings: { reviewer: 'agentk' as ReviewerMode } })).toThrow(/Reviewer must be/);
  });

  it('asks Claude to fix a CI failure once the checks finish, once per head', async () => {
    const s = await inReview('CI fails');
    gh.status = { ...gh.status!, statusCheckRollup: [failing(), { name: 'test (ubuntu)', status: 'IN_PROGRESS' }] };
    await poll(s.id);
    expect(queued(s.id)).toEqual({}); // the other check is still running
    gh.status = { ...gh.status!, statusCheckRollup: [failing(), { name: 'test (ubuntu)', status: 'COMPLETED', conclusion: 'SUCCESS' }] };
    await poll(s.id);
    expect(lastEvent(s.id, 'ci')).toBe('CI failed: test (macos-15)');
    await waitFor(() => transcript(s.id), (t) => t.includes('CI failed on PR #7'), 10000, 'CI prompt');
    expect(transcript(s.id)).toContain('- test (macos-15): failure (https://github.com/acme/demo/actions/runs/1/job/2)');
    await idle(s.id, 'CI turn');
    await poll(s.id); // the same head, or a rerun of it, isn't sent again
    expect(count(transcript(s.id), /CI failed on PR/g)).toBe(1);
  });

  it('hands a CI failure to the user after three heads in a row, and resets on green', async () => {
    const s = await inReview('CI keeps failing');
    const wt = env.core.sessions.row(s.id).worktree_path;
    gh.status = { ...gh.status!, statusCheckRollup: [failing()] };
    for (let i = 1; i <= 4; i++) {
      if (i > 1) commitIn(wt); // Claude pushed a fix; CI failed again on it
      await poll(s.id);
      await idle(s.id, `head ${i}`);
    }
    expect(count(transcript(s.id), /CI failed on PR/g)).toBe(3);
    expect(lastEvent(s.id, 'ci')).toBe('CI failed: test (macos-15). Still failing after 3 fix attempts; over to you');
    gh.status = { ...gh.status!, statusCheckRollup: [{ status: 'COMPLETED', conclusion: 'SUCCESS' }] };
    await poll(s.id);
    expect(env.core.sessions.row(s.id).ci_fixes).toBe(0);
  });

  it('holds a CI prompt while the branch has unpushed commits', async () => {
    const s = await inReview('Fix in progress');
    const wt = env.core.sessions.row(s.id).worktree_path;
    const pushed = tip(wt);
    commitIn(wt);
    gh.status = { ...gh.status!, headRefOid: pushed, statusCheckRollup: [failing()] };
    await poll(s.id);
    expect(queued(s.id)).toEqual({});
    expect(lastEvent(s.id, 'ci')).toBeUndefined();
  });

  it('sends a review before a CI failure, and drops the CI prompt when that turn pushes', async () => {
    const s = await inReview('Both at once');
    slowTurns();
    await env.core.sessions.submit(s.id, 'something else');
    await waitFor(() => env.core.sessions.get(s.id), (x) => x.status === 'working', 10000, 'working');
    gh.status = { ...gh.status!, reviews: [...gh.status!.reviews, finding('r2')], statusCheckRollup: [failing()] };
    await poll(s.id);
    expect(queued(s.id)).toMatchObject({ review: { state: 'queued' }, ci: { state: 'queued' } });
    // The review goes in first; while Claude addresses it, it pushes and CI starts over.
    await waitFor(() => transcript(s.id), (t) => t.includes('A review from'), 10000, 'review prompt');
    commitIn(env.core.sessions.row(s.id).worktree_path);
    gh.status = { ...gh.status!, statusCheckRollup: [{ status: 'IN_PROGRESS' }] };
    await waitFor(() => env.core.sessions.get(s.id), (x) => x.reviewState === 'addressed' && x.status === 'idle', 10000, 'addressed');
    // The turn end marks the review addressed before its PR poll drops the stale CI prompt.
    await waitFor(() => queued(s.id), (q) => !q.ci, 10000, 'CI prompt dropped');
    expect(queued(s.id)).toEqual({});
    expect(transcript(s.id)).not.toContain('CI failed on PR');
  });

  it('delivers a CI failure found mid-turn when the turn ends', async () => {
    const s = await inReview('CI mid-turn');
    slowTurns();
    await env.core.sessions.submit(s.id, 'something else');
    await waitFor(() => env.core.sessions.get(s.id), (x) => x.status === 'working', 10000, 'working');
    gh.status = { ...gh.status!, statusCheckRollup: [failing()] };
    await poll(s.id);
    expect(queued(s.id).ci).toMatchObject({ state: 'queued', vars: { sha: tip(env.core.sessions.row(s.id).worktree_path) } });
    await waitFor(() => transcript(s.id), (t) => t.includes('CI failed on PR #7'), 10000, 'CI prompt');
  });

  it('holds queued prompts until the turn that got one has ended, unless Claude was relaunched', async () => {
    const s = await inReview('One at a time');
    const setQueue = (q: object) => env.core.sessions.update(s.id, { pending_prompts: JSON.stringify(q) });
    const ci = { state: 'queued', vars: { sha: 'abc1234', checks: '- test: failure' } };
    // Idle, but the Stop of the review's turn isn't handled yet.
    setQueue({ review: { state: 'sent' }, ci });
    await pollAll();
    expect(transcript(s.id)).not.toContain('CI failed on PR');
    // After a restart the turn is gone, so the queue moves on.
    env.core.sessions.setStatus(s.id, 'recovered');
    await pollAll();
    await waitFor(() => transcript(s.id), (t) => t.includes('CI failed on PR #7 (https://github.com/acme/demo/pull/7) at commit abc1234'), 10000, 'CI prompt');
  });

  it('refuses Open PR while Claude is working', async () => {
    const p = await env.core.projects.create({ repoPath: env.repo, name: 'Demo', datapack: { mode: 'existing', id: 'dp-1', name: 'team/demo' } });
    mkdirSync(env.fakeDir, { recursive: true });
    writeFileSync(join(env.fakeDir, 'turn-ms'), '3000');
    const s = await env.core.sessions.create(p.id, { name: 'Busy', prompt: 'slow', kind: 'quick' });
    await waitFor(() => env.core.sessions.get(s.id), (x) => x.status === 'working', 10000, 'working');
    await expect(env.core.sessions.action(s.id, 'open-pr')).rejects.toThrow(/still working/);
  });
});
