import { execFileSync } from 'node:child_process';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { PrStatus } from '../../src/server/github.js';
import { GH_NEEDED, type CreateSessionRequest, type MoveSessionRequest } from '../../src/shared/types.js';

// GitHub is mocked; git, the session host and the (fake) claude process are real.
const gh = vi.hoisted(() => ({
  pr: null as null | { number: number; url: string; state: 'OPEN' | 'MERGED' | 'CLOSED'; createdAt?: string },
  status: null as null | PrStatus,
}));

vi.mock('../../src/server/github.js', async (orig) => {
  const real = await orig<typeof import('../../src/server/github.js')>();
  return {
    ...real,
    findPr: async () => gh.pr,
    // Like GitHub after a push: the PR's head is the branch tip, unless a test says otherwise.
    prStatus: async (w: string) => ({ ...gh.status!, headRefOid: gh.status!.headRefOid || tip(w) }),
    botLogins: async () => [],
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
const { taskPrs } = await import('../../src/server/taskprs.js');
const { runActions } = await import('../../src/server/actions.js');
type TestEnv = Awaited<ReturnType<typeof makeEnv>>;

const PR = { number: 7, url: 'https://github.com/acme/demo/pull/7', state: 'OPEN' as const };

function status(over: Partial<PrStatus> = {}): PrStatus {
  return {
    number: 7, url: PR.url, title: 'Fix the thing', body: 'Summary of the change.',
    state: 'OPEN', reviews: [], statusCheckRollup: [{ status: 'COMPLETED', conclusion: 'SUCCESS' }],
    reviewDecision: '', mergeable: 'MERGEABLE', mergeStateStatus: 'CLEAN', headRefOid: '', ...over,
  };
}

describe('moves: moving a task session between stages', () => {
  let env: TestEnv;
  let projectId: string | null;
  beforeEach(async () => {
    gh.pr = null;
    gh.status = null;
    projectId = null;
    env = await makeEnv();
  });
  afterEach(async () => {
    await env.cleanup();
  });

  async function session(req: Partial<CreateSessionRequest>) {
    projectId ??= (await env.core.projects.create({ repoPath: env.repo, name: 'Demo', datapack: { mode: 'existing', id: 'dp-1', name: 'team/demo' } })).id;
    const s = await env.core.sessions.create(projectId, { name: 'Fix the thing', ...req });
    await waitFor(() => env.core.sessions.get(s.id), (x) => x.status === 'idle' || x.status === 'needs_input', 10000, 'first turn');
    return s;
  }

  const move = (id: string, req: MoveSessionRequest) => env.core.sessions.move(id, req);
  const get = (id: string) => env.core.sessions.get(id);
  const row = (id: string) => env.core.sessions.row(id);
  const idle = (id: string, what: string) => waitFor(() => get(id), (x) => x.status === 'idle', 10000, what);
  const stages = (id: string) =>
    all<{ stage: string; moved_by: string }>(env.core.db, 'SELECT stage, moved_by FROM session_stages WHERE session_id = ? ORDER BY rowid', id);
  const events = (id: string, kind: string) =>
    all<{ text: string }>(env.core.db, 'SELECT text FROM session_events WHERE session_id = ? AND kind = ? ORDER BY id', id, kind).map((e) => e.text);
  const queue = (id: string) => dbGet<{ q: string }>(env.core.db, 'SELECT pending_prompts AS q FROM sessions WHERE id = ?', id)!.q;
  const transcript = (id: string) => readFileSync(join(env.fakeDir, `${row(id).claude_session_id}.jsonl`), 'utf8');
  /** Everything typed into Claude from now on. */
  const typed = () => {
    const spy = vi.spyOn(env.core.host, 'submit');
    return () => spy.mock.calls.map((c) => String(c[1]));
  };

  it('moves on from Explore to Code, carrying on from the conversation, or with the task given to a session without one', async () => {
    const s = await session({ start: 'explore', prompt: 'look around' });
    const sent = typed();
    const moved = await move(s.id, { stage: 'code' });
    // The code review is off here (makeEnv), so the Code Review step is skipped ahead of time.
    expect(moved).toMatchObject({ kind: 'plan', stage: 'code', hasTask: true, skippedStages: ['design', 'reviewing-code'] });
    expect(stages(s.id)).toEqual([{ stage: 'explore', moved_by: 'you' }, { stage: 'code', moved_by: 'you' }]);
    expect(events(s.id, 'kind')).toEqual([]);
    const text = sent()[0];
    expect(text).toMatch(/^Kode moved this session to the Code stage\. Stages: Explore → Design → Code → Pull Request → Merge & Close/);
    expect(text).not.toContain("It's in Explore now");
    // Not the first prompt again: the conversation may have moved on from it.
    expect(text).toContain('Implement what this session has worked out so far');
    expect(text).not.toContain('Task:');
    expect(text).toContain("Don't open a pull request yet.");

    // A session without a task (started at Explore with no first prompt, before the name became the
    // task): one given with the move becomes its task, in the Code stage's start prompt.
    const noTask = async (name: string) => {
      const x = await session({ start: 'explore', name });
      env.core.sessions.update(x.id, { initial_prompt: null });
      return x;
    };
    const t = await noTask('Retries');
    await move(t.id, { stage: 'code', task: 'Add retries' });
    expect(row(t.id).initial_prompt).toBe('Add retries');
    expect(sent().at(-1)).toContain('Task: Retries\n\nAdd retries');
    // Without one, it moves all the same.
    const u = await noTask('Nothing yet');
    expect(await move(u.id, { stage: 'code' })).toMatchObject({ stage: 'code', hasTask: false });
    expect(sent().at(-1)).toContain('Implement what this session has worked out so far');
  });

  it("refuses what works on GitHub while gh isn't usable, starting nothing, and goes on once it is", async () => {
    const s = await session({ start: 'code', prompt: 'Fix it.' });
    env.gh.state = 'missing';
    await env.core.gh.check();
    const needed = { status: 409, code: GH_NEEDED };
    await expect(env.core.lifecycle.openPr(row(s.id))).rejects.toMatchObject(needed);
    // Do all says which step wants gh, with the code, so the page opens the dialog too.
    const [step] = await runActions(env.core, [{ type: 'openPr', sessionId: s.id, instructions: '' }], { session: null });
    expect(step).toMatchObject({ ok: false, code: GH_NEEDED });
    await expect(env.core.lifecycle.setAutoRun(row(s.id), true)).rejects.toMatchObject(needed);
    await expect(move(s.id, { stage: 'review' })).rejects.toMatchObject(needed);
    await expect(env.core.sessions.create(projectId!, { name: 'Review it', kind: 'review-pr', pr: { repo: 'acme/demo', number: 7 } })).rejects.toMatchObject(needed);
    await expect(env.core.prReview.reviewable(projectId!, { pr: '7' })).rejects.toMatchObject(needed);
    // The list alone is read for a project just opened too: no PRs, and why, but no dialog.
    expect(await env.core.prReview.reviewable(projectId!, {})).toEqual({ prs: [], error: expect.stringMatching(/^Review a PR needs gh/) });
    await expect(taskPrs(env.core, projectId!, {})).rejects.toMatchObject(needed);
    await expect(env.core.githubLinks.start({ ref: 'acme/demo#7' })).rejects.toMatchObject(needed);
    // The palette reads a link's plan as typing pauses: the reason, but no code, so no dialog.
    const plan = await env.core.githubLinks.plan('acme/demo#7').catch((e: unknown) => e);
    expect(plan).toMatchObject({ status: 409, message: expect.stringMatching(/^Starting from a GitHub link needs gh/) });
    expect((plan as { code?: string }).code).toBeUndefined();
    expect(get(s.id)).toMatchObject({ stage: 'code', autoRun: false });
    expect(env.core.sessions.listForProject(projectId!).length).toBe(1);

    env.gh.state = 'ok';
    await env.core.lifecycle.openPr(row(s.id));
    await waitFor(() => transcript(s.id), (t) => t.includes('gh pr create'), 10000, 'the PR prompt');
  });

  it('opens a PR from Explore in one move, then picks the PR up with one Pull Request line', async () => {
    const s = await session({ start: 'explore', prompt: 'look around' });
    const sent = typed();
    await move(s.id, { stage: 'review' });
    expect(row(s.id).kind).toBe('plan');
    expect(['pr', 'review']).toContain(row(s.id).stage);
    expect(sent()[0]).toMatch(/^Kode moved this session to the Pull Request stage\./);
    expect(sent()[0]).toContain('Open a pull request for this work.');
    await idle(s.id, 'PR turn');
    gh.pr = PR;
    gh.status = status();
    await env.core.lifecycle.pollPrStage(row(s.id));
    await waitFor(() => get(s.id), (x) => x.stage === 'review' && x.prNumber === 7, 10000, 'review stage');
    expect(events(s.id, 'pr')).toEqual(['Opened PR #7']);
    expect(events(s.id, 'stage').filter((t) => t === 'Moved to Pull Request')).toHaveLength(1);
    // The PR went without a code review (off here): Code Review was skipped too.
    expect(get(s.id).skippedStages).toEqual(['design', 'code', 'reviewing-code']);
  });

  it('picks up an open PR with nothing to push, without a prompt, and sends the PR prompt when there is', async () => {
    const s = await session({ start: 'code', prompt: 'do it' });
    gh.pr = PR;
    gh.status = status({ headRefOid: '0123456789abcdef0123456789abcdef01234567' });
    const sent = typed();
    // The PR's head isn't the branch tip: the branch has work to push.
    await move(s.id, { stage: 'review' });
    expect(sent()[0]).toMatch(/^Open a pull request for this work\./); // the same as Open PR: no moved line
    await waitFor(() => get(s.id), (x) => x.stage === 'review' && x.status === 'idle', 10000, 'review stage');

    const t = await session({ start: 'code', prompt: 'do it', name: 'Already opened' });
    gh.status = status();
    const before = sent().length;
    await move(t.id, { stage: 'review' });
    expect(get(t.id)).toMatchObject({ stage: 'review', prNumber: 7, prUrl: PR.url });
    expect(sent()).toHaveLength(before);
    expect(events(t.id, 'pr')).toEqual(['Picked up PR #7']);
  });

  it('moves back to Code from Pull Request, keeps the PR, and holds reviews until it comes back', async () => {
    const s = await session({ start: 'code', prompt: 'do it' });
    env.core.projects.update(projectId!, { settings: { reviewer: 'people' } });
    gh.pr = PR;
    gh.status = status();
    await move(s.id, { stage: 'review' });
    // A CI prompt for an old head, and one whose turn is over (its Stop not handled yet).
    env.core.sessions.update(s.id, { pending_prompts: JSON.stringify({ ci: { state: 'queued', vars: { sha: 'abc', checks: '- test' } }, 'review-people': { state: 'sent', vars: { reviewers: 'olivia' } } }) });

    const sent = typed();
    await move(s.id, { stage: 'code' });
    expect(get(s.id)).toMatchObject({ stage: 'code', prNumber: 7 });
    expect(JSON.parse(queue(s.id))).toEqual({ ci: { state: 'queued', vars: { sha: 'abc', checks: '- test' } } });
    expect(sent()).toEqual([]); // back from Pull Request, the Code prompt is off by default

    // A review that arrives while in Code waits.
    gh.status = status({
      reviews: [{ id: 'r1', author: { login: 'olivia' }, authorAssociation: 'MEMBER', state: 'COMMENTED', body: 'Add a test.', submittedAt: new Date().toISOString() }],
    });
    await env.core.lifecycle.pollReview(row(s.id));
    expect(get(s.id).reviewState).toBe('waiting');
    expect(sent()).toEqual([]);

    // Back in Pull Request: the same PR, and the review goes out once.
    await move(s.id, { stage: 'review' });
    expect(events(s.id, 'pr-again')).toEqual(['Back on PR #7']);
    expect(events(s.id, 'pr')).toEqual(['Picked up PR #7']);
    await waitFor(() => transcript(s.id), (x) => x.includes('Reviewers left feedback on PR #7 (https://github.com/acme/demo/pull/7) from olivia.'), 10000, 'review prompt');
    expect(sent().filter((x) => x.startsWith('Reviewers left feedback'))).toHaveLength(1);
    // CI is green at the PR's head: the old CI prompt is dropped, never sent.
    await waitFor(() => JSON.parse(queue(s.id)) as object, (q) => !('ci' in q), 10000, 'stale CI prompt dropped');
    expect(sent().some((x) => x.startsWith('CI failed'))).toBe(false);
  });

  it("keeps an approval when it comes back to the same PR", async () => {
    const s = await session({ start: 'code', prompt: 'do it' });
    gh.pr = PR;
    gh.status = status({ reviewDecision: 'APPROVED' });
    await move(s.id, { stage: 'review' });
    expect(get(s.id)).toMatchObject({ reviewState: 'approved', canMerge: true });
    await move(s.id, { stage: 'code' });
    await move(s.id, { stage: 'review' });
    expect(get(s.id)).toMatchObject({ stage: 'review', reviewState: 'approved', canMerge: true });
  });

  it("never takes back the PR it had once that PR is merged, and waits for the new one", async () => {
    const s = await session({ start: 'code', prompt: 'do it' });
    gh.pr = PR;
    gh.status = status();
    await move(s.id, { stage: 'review' });
    await move(s.id, { stage: 'code' });
    // PR #7 is merged on GitHub while the session is back in Code; Open PR asks for a new one.
    gh.pr = { ...PR, state: 'MERGED' };
    gh.status = status({ state: 'MERGED' });
    await env.core.sessions.action(s.id, 'open-pr');
    await idle(s.id, 'PR turn');
    await env.core.lifecycle.pollPrStage(row(s.id));
    expect(row(s.id)).toMatchObject({ stage: 'pr', desired: 'running' });
    gh.pr = { number: 8, url: 'https://github.com/acme/demo/pull/8', state: 'OPEN' };
    gh.status = status({ number: 8, url: 'https://github.com/acme/demo/pull/8' });
    await env.core.lifecycle.pollPrStage(row(s.id));
    expect(get(s.id)).toMatchObject({ stage: 'review', prNumber: 8 });
    expect(events(s.id, 'pr')).toEqual(['Picked up PR #7', 'Opened PR #8']);
  });

  it('clears a design waiting for approval when it moves to Pull Request and picks up a PR', async () => {
    const s = await session({ prompt: 'DESIGN_MD it' });
    await waitFor(() => get(s.id), (x) => x.status === 'needs_input' && x.hasDesign, 10000, 'design ready');
    gh.pr = PR;
    gh.status = status();
    await move(s.id, { stage: 'review' });
    expect(get(s.id)).toMatchObject({ stage: 'review', status: 'idle', prNumber: 7 });
  });

  it('goes back to Design: guards the repo again, sends the Design prompt, and can be approved', async () => {
    const s = await session({ start: 'code', prompt: 'DESIGN_MD it' });
    const sent = typed();
    await move(s.id, { stage: 'design' });
    expect(row(s.id)).toMatchObject({ stage: 'design', plan_mode: 0 });
    expect(sent()[0]).toMatch(/^Kode moved this session to the Design stage\./);
    expect(sent()[0]).toContain('Design what this session is working toward now');
    expect(sent()[0]).toContain("Also give the user the design's open questions");
    expect(sent()[0]).not.toContain('Task: DESIGN_MD it');
    const wt = row(s.id).worktree_path;
    const decision = env.core.sessions.guard(row(s.id).hook_token, { hook_event_name: 'PreToolUse', tool_name: 'Write', tool_input: { file_path: join(wt, 'x.ts') }, cwd: wt });
    expect(decision).toMatchObject({ hookSpecificOutput: { permissionDecision: 'deny' } });

    await waitFor(() => get(s.id), (x) => x.status === 'needs_input' && x.hasDesign, 10000, 'design ready');
    await env.core.sessions.action(s.id, 'approve-design');
    expect(row(s.id).stage).toBe('code');
  });

  it('skips design approval when it moves on to Code, with or without the Code prompt', async () => {
    const s = await session({ prompt: 'DESIGN_MD it' });
    await waitFor(() => get(s.id), (x) => x.status === 'needs_input' && x.hasDesign, 10000, 'design ready');
    const sent = typed();
    await move(s.id, { stage: 'code' });
    expect(row(s.id).stage).toBe('code');
    expect(events(s.id, 'design')).toContain('Design approval skipped');
    expect(sent()[0]).toContain('Implement what this session has worked out so far');
    expect(sent()[0]).not.toContain('Task: DESIGN_MD it');
    expect(sent()[0]).not.toContain('The design is approved');

    const t = await session({ prompt: 'DESIGN_MD it', name: 'Too small to design' });
    await waitFor(() => get(t.id), (x) => x.status === 'needs_input' && x.hasDesign, 10000, 'design ready');
    await move(t.id, { stage: 'code', prompt: false });
    // No longer waiting on a design approval.
    expect(get(t.id)).toMatchObject({ kind: 'plan', stage: 'code', status: 'idle' });
  });

  it('goes back to Explore without a prompt, and ignores the type an older page still sends', async () => {
    const s = await session({ start: 'code', prompt: 'do it' });
    expect(get(s.id)).toMatchObject({ kind: 'plan', stage: 'code', skippedStages: ['explore', 'design', 'reviewing-code'] });
    const sent = typed();
    await move(s.id, { stage: 'explore' });
    expect(get(s.id)).toMatchObject({ stage: 'explore', skippedStages: ['reviewing-code'] });
    expect(sent()).toEqual([]);
    await move(s.id, { kind: 'quick', stage: 'design', prompt: false } as MoveSessionRequest);
    expect(row(s.id)).toMatchObject({ stage: 'design' });
    expect(events(s.id, 'kind')).toEqual([]);
  });

  it("sends the user's instructions after the stage prompt, or with the moved line when there's none", async () => {
    const s = await session({ start: 'explore', prompt: 'look around' });
    const sent = typed();
    await move(s.id, { stage: 'design', instructions: ' Design only the server side. ' });
    const text = sent()[0];
    expect(text).toMatch(/^Kode moved this session to the Design stage\./);
    expect(text.indexOf('Design what this session is working toward now')).toBeLessThan(text.indexOf('The user added these instructions'));
    expect(text).not.toContain('Task: Fix the thing');
    expect(text.endsWith('\n\nDesign only the server side.')).toBe(true);

    // Back from Code to Explore sends no stage prompt: only the moved line and the instructions.
    const t = await session({ start: 'code', prompt: 'do it', name: 'Look first' });
    await move(t.id, { stage: 'explore', instructions: 'Find out why the retries pile up.' });
    const back = sent().at(-1)!;
    expect(back).toMatch(/^Kode moved this session to the Explore stage\./);
    expect(back).not.toContain('You are in the Explore stage');
    expect(back.endsWith('\n\nFind out why the retries pile up.')).toBe(true);
    expect(row(t.id).stage).toBe('explore');
  });

  it('refuses instructions Claude can’t take, before moving', async () => {
    const s = await session({ start: 'code', prompt: 'do it' });
    await expect(move(s.id, { stage: 'explore', instructions: 5 } as unknown as MoveSessionRequest)).rejects.toMatchObject({ status: 400, message: 'Instructions must be text.' });
    env.core.sessions.setStatus(s.id, 'needs_input', 'Permission needed for Bash');
    await expect(move(s.id, { stage: 'explore', instructions: 'Look around.' })).rejects.toMatchObject({ status: 409, message: 'Kode is waiting on you in the terminal. Answer it first.' });
    await expect(move(s.id, { stage: 'review', instructions: 'Look around.' })).rejects.toMatchObject({ status: 409 });
    expect(row(s.id).stage).toBe('code');
    // Blank instructions are none: the move goes, typing nothing.
    const sent = typed();
    await move(s.id, { stage: 'explore', instructions: '  ' });
    expect(row(s.id).stage).toBe('explore');
    expect(sent()).toEqual([]);
  });

  it('sends instructions with the PR prompt, or before picking up an open PR', async () => {
    const s = await session({ start: 'explore', prompt: 'look around' });
    const sent = typed();
    await move(s.id, { stage: 'review', instructions: 'Open it as a draft.' });
    expect(sent()[0]).toMatch(/^Kode moved this session to the Pull Request stage\./);
    expect(sent()[0].indexOf('Open a pull request')).toBeLessThan(sent()[0].indexOf('The user added these instructions'));
    expect(sent()[0].endsWith('\n\nOpen it as a draft.')).toBe(true);

    const t = await session({ start: 'code', prompt: 'do it', name: 'Already opened' });
    gh.pr = PR;
    gh.status = status();
    const before = sent().length;
    await move(t.id, { stage: 'review', instructions: 'Address the open threads first.' });
    expect(get(t.id)).toMatchObject({ stage: 'review', prNumber: 7 });
    expect(events(t.id, 'pr')).toEqual(['Picked up PR #7']);
    const picked = sent().slice(before);
    expect(picked).toHaveLength(1);
    expect(picked[0]).toMatch(/^Kode moved this session to the Pull Request stage\./);
    expect(picked[0]).not.toContain('Open a pull request');
    expect(picked[0].endsWith('\n\nAddress the open threads first.')).toBe(true);
    await idle(t.id, 'instructions turn');

    // The PR is picked up first: if the instructions can't be typed, the session stays in Pull Request.
    await move(t.id, { stage: 'code' });
    vi.spyOn(env.core.host, 'submit').mockRejectedValueOnce(new Error('no pane'));
    await expect(move(t.id, { stage: 'review', instructions: 'Address the open threads first.' })).rejects.toMatchObject({
      status: 502,
      message: /^Moved to Pull Request, but couldn't send your instructions to Claude: no pane\./,
    });
    expect(get(t.id)).toMatchObject({ stage: 'review', prNumber: 7, busy: false });
  });

  it('gets a PR stage out of waiting when the turn ended without a PR', async () => {
    const s = await session({ start: 'code', prompt: 'do it' });
    await env.core.sessions.action(s.id, 'open-pr');
    await idle(s.id, 'PR turn without a PR');
    expect(row(s.id).stage).toBe('pr');
    await move(s.id, { stage: 'code' });
    expect(row(s.id).stage).toBe('code');
  });

  it('refuses moves that make no sense, and while Kode or the session is busy or archived', async () => {
    const s = await session({ start: 'code', prompt: 'do it' });
    await expect(move(s.id, { stage: 'code' })).rejects.toMatchObject({ status: 400, message: 'Nothing to change: this session is already in Code.' });
    await expect(move(s.id, { stage: 'merge' } as unknown as MoveSessionRequest)).rejects.toMatchObject({ status: 400 });

    env.core.sessions.setStatus(s.id, 'working');
    await expect(move(s.id, { stage: 'explore' })).rejects.toMatchObject({ status: 409, message: 'Kode is still working. Wait for the turn to finish.' });
    // Waiting on the user: a move that types nothing is fine, one that types a prompt isn't.
    env.core.sessions.setStatus(s.id, 'needs_input', 'Permission needed for Bash');
    await expect(move(s.id, { stage: 'design' })).rejects.toMatchObject({ status: 409, message: 'Kode is waiting on you in the terminal. Answer it first.' });
    env.core.sessions.setStatus(s.id, 'exited');
    await expect(move(s.id, { stage: 'design' })).rejects.toMatchObject({ status: 409, message: 'Claude has exited. Resume the session first.' });
    await move(s.id, { stage: 'explore' });
    expect(row(s.id).stage).toBe('explore');

    env.core.sessions.setStatus(s.id, 'idle');
    env.core.sessions.setStage(s.id, 'merge', 'auto');
    await expect(move(s.id, { stage: 'code' })).rejects.toMatchObject({ status: 409, message: "This session is merging and closing, so it can't be moved." });

    const legacy = await session({ prompt: 'plan it', name: 'Old style' });
    env.core.sessions.update(legacy.id, { plan_mode: 1 });
    await expect(move(legacy.id, { stage: 'code' })).rejects.toMatchObject({ status: 409, message: /plan mode/ });
    await env.core.sessions.action(legacy.id, 'archive');
    await expect(move(legacy.id, { stage: 'code', task: 'x' })).rejects.toMatchObject({ status: 409, message: 'This session is archived.' });
  });

  it("keeps a move whose prompt couldn't be typed, but rolls back a PR prompt that couldn't", async () => {
    const s = await session({ start: 'explore', prompt: 'look around' });
    vi.spyOn(env.core.host, 'submit').mockRejectedValueOnce(new Error('no pane'));
    await expect(move(s.id, { stage: 'code' })).rejects.toMatchObject({ status: 502, message: /^Moved to Code, but couldn't type the Code prompt/ });
    expect(row(s.id).stage).toBe('code');

    const t = await session({ start: 'explore', prompt: 'look around', name: 'Ship it' });
    vi.spyOn(env.core.host, 'submit').mockRejectedValueOnce(new Error('no pane'));
    await expect(move(t.id, { stage: 'review' })).rejects.toMatchObject({ status: 502 });
    expect(get(t.id)).toMatchObject({ stage: 'explore', busy: false });
  });
});
