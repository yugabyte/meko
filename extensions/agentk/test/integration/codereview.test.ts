import { existsSync, mkdirSync, readFileSync, writeFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

// GitHub is mocked (nothing in these tests reaches a PR); git, the session host and the fake claude are real.
vi.mock('../../src/server/github.js', async (orig) => {
  const real = await orig<typeof import('../../src/server/github.js')>();
  return { ...real, findPr: async () => null, viewerLogin: async () => 'me', repoVisibility: async () => 'private' };
});

const { makeEnv, readJsonl, waitFor } = await import('../helpers.js');
const { all } = await import('../../src/server/db.js');
const { mekoLogPath } = await import('../../src/server/mekolog.js');
type TestEnv = Awaited<ReturnType<typeof makeEnv>>;

/** Reports for the ledger passes that clear the change: the dependents and claims passes need their ledgers. */
const LEDGERS = {
  dependents: { state_ledger: [], summary: 'LGTM', findings: [] },
  claims: { claims: [], summary: 'LGTM', findings: [] },
};

describe('local code review before the PR (real session host + git, fake claude running the review steps)', { timeout: 60000 }, () => {
  let env: TestEnv;
  beforeEach(async () => {
    env = await makeEnv({ review: true });
  });
  afterEach(async () => {
    await env.cleanup();
  });

  const prompts = (claudeSessionId: string) => readJsonl<{ type: string; text: string }>(join(env.fakeDir, `${claudeSessionId}.jsonl`)).filter((e) => e.type === 'user').map((e) => e.text);
  const fake = (file: string, v: unknown) => writeFileSync(join(env.fakeDir, file), JSON.stringify(v));
  /** The task's PR prompt, once it has reached Claude (the stage moves to PR just before it's typed). */
  const prPrompt = (id: string) => waitFor(() => prompts(id).at(-1) ?? '', (p) => p.includes('Open a pull request'), 10000, 'the PR prompt');

  /** A task at Code whose worktree has a new file with six lines, ready for Open PR. */
  async function task(req: Record<string, unknown> = {}) {
    const p = await env.core.projects.create({ repoPath: env.repo, name: 'Demo', datapack: { mode: 'none' } });
    const s = await env.core.sessions.create(p.id, { name: 'Fix the thing', prompt: 'do it', start: 'code', ...req });
    await waitFor(() => env.core.sessions.get(s.id), (x) => x.status === 'idle', 10000, 'first turn');
    const wt = env.core.sessions.row(s.id).worktree_path;
    mkdirSync(join(wt, 'src'), { recursive: true });
    writeFileSync(join(wt, 'src', 'a.ts'), ['export function f(n: number) {', '  return n + 1;', '}', 'export const x = f(1);', 'export const y = f(2);', ''].join('\n'));
    return s;
  }

  it('reviews the change at Open PR in a child session, then sends the PR prompt with the findings and the me_ak_review line', async () => {
    const s = await task();
    fake('review-reports.json', {
      ...LEDGERS,
      diff: { summary: 'LGTM', findings: [{ file: 'src/a.ts', line: 2, severity: 'blocker', comment: 'Off by one.', gotchas: [] }] },
      guide: { summary: 'LGTM', findings: [{ file: 'src/a.ts', line: 4, severity: 'major', comment: 'Breaks the guide.', gotchas: [{ id: 'MG-3', quote: 'Never do this.' }] }] },
      // Outside the diff: Validate withholds it.
      history: { summary: 'LGTM', findings: [{ file: 'README.md', line: 1, severity: 'minor', comment: 'Not this change.', gotchas: [] }] },
    });
    await env.core.sessions.action(s.id, 'open-pr');
    // The review holds the PR: the task stays in Code, with its review running.
    expect(env.core.sessions.get(s.id).stage).toBe('code');
    const done = await waitFor(() => env.core.sessions.get(s.id), (x) => x.stage === 'pr', 30000, 'the PR prompt after the review');
    expect(done.codeReview).toMatchObject({ strategy: 'sagittarius-3.0', state: 'done', counts: { blocker: 1, major: 1, minor: 0 } });
    const pr = await prPrompt(s.claudeSessionId);
    expect(pr).toContain('AgentK reviewed this change locally (Sagittarius 3.0) before this PR.');
    expect(pr).toContain('[1:1] `src/a.ts:2` **blocker**: Off by one.');
    expect(pr).toContain('`src/a.ts:4` **major** (Meko gotcha MG-3): Breaks the guide.');
    expect(pr).not.toContain('Not this change.');
    expect(pr).toContain('me_ak_review:1 blocker,1 major addressed');

    // The review ran in a code-review session on the task's worktree, with read-only subagents.
    const childId = all<{ id: string }>(env.core.db, 'SELECT id FROM sessions WHERE parent_id = ?', s.id)[0].id;
    const child = env.core.sessions.row(childId);
    expect(child).toMatchObject({ kind: 'code-review', parent_id: s.id, stage: 'reviewing-code', worktree_path: env.core.sessions.row(s.id).worktree_path, worktree_released: 1 });
    const launch = readJsonl<{ args: string[]; env: Record<string, string | undefined> }>(join(env.fakeDir, 'argv.jsonl')).find((a) => a.args.includes('--agents'))!;
    const argv = launch.args;
    expect(Object.keys(JSON.parse(argv[argv.indexOf('--agents') + 1]))).toEqual(['agentk-review-large', 'agentk-review-mid', 'agentk-review-small']);
    // Subagents in the foreground: a backgrounded one ends the turn, the sweep, before it reports.
    expect(launch.env.CLAUDE_CODE_DISABLE_BACKGROUND_TASKS).toBe('1');
    expect(argv[argv.indexOf('--disallowedTools') + 1]).toContain('Edit');
    // Five passes, then a scorer for each of the three candidates.
    const calls = readJsonl<{ step: string; status: number }>(join(env.fakeDir, 'review-calls.jsonl'));
    expect(calls.map((c) => c.step)).toEqual(['r1s1:guide', 'r1s1:diff', 'r1s1:dependents', 'r1s1:history', 'r1s1:claims', 'r1c1', 'r1c2', 'r1c3']);

    // The findings panel has every candidate, with why the one outside the diff was dropped.
    const detail = env.core.reviews.detail(s.id);
    expect(detail.findings.map((f) => [f.file, f.line, f.kept, f.dropped])).toEqual(
      expect.arrayContaining([['src/a.ts', 2, true, null], ['src/a.ts', 4, true, null], ['README.md', 1, false, 'file is not changed by this change']]),
    );
    // Once done, the review session stops (its findings stay with the review); the task's worktree stays.
    await waitFor(() => env.core.sessions.row(childId), (x) => x.desired === 'archived', 10000, 'the review session to stop');
    expect(env.core.sessions.get(s.id).codeReview!.sessionId).toBeNull();
    expect(env.core.sessions.row(s.id).worktree_released).toBe(0);

    // The tab shows the counts until the task says what it did with each finding, then LGTM.
    expect(done.codeReview!.addressed).toBe(false);
    const kept = detail.findings.filter((f) => f.kept);
    env.core.reviews.onTool(env.core.sessions.row(s.id), 'review_outcome', { outcomes: kept.slice(1).map((f) => ({ key: f.key, status: 'fixed', note: 'Done.' })) });
    expect(env.core.sessions.get(s.id).codeReview!.addressed).toBe(false);
    env.core.reviews.onTool(env.core.sessions.row(s.id), 'review_outcome', { outcomes: [{ key: kept[0].key, status: 'declined', note: 'On purpose.' }] });
    expect(env.core.sessions.get(s.id).codeReview).toMatchObject({ addressed: true, counts: { blocker: 1, major: 1, minor: 0 } });
  });

  it('opens the PR with me_ak_review:LGTM when Sirius 1.0 finds nothing, set for the session alone', async () => {
    const s = await task({ review: { strategy: 'sirius-1.0' } });
    expect(env.core.sessions.get(s.id).review).toEqual({ strategy: 'sirius-1.0' });
    await env.core.sessions.action(s.id, 'open-pr');
    await waitFor(() => env.core.sessions.get(s.id), (x) => x.stage === 'pr', 30000, 'the PR prompt after the review');
    expect(await prPrompt(s.claudeSessionId)).toContain('me_ak_review:LGTM');
    expect(readJsonl<{ step: string }>(join(env.fakeDir, 'review-calls.jsonl')).map((c) => c.step)).toEqual(['r1s1:all']);
  });

  it('asks the task to fix a round\'s findings, reviews again with what it did, and adds both rounds up in the line', async () => {
    const s = await task({ review: { rounds: 1 } });
    fake('review-reports.json', { ...LEDGERS, diff: { summary: 'LGTM', findings: [{ file: 'src/a.ts', line: 2, severity: 'blocker', comment: 'Off by one.', gotchas: [] }] } });
    await env.core.sessions.action(s.id, 'open-pr');
    // Round 1's findings go to the task as a fix prompt; the PR waits.
    await waitFor(() => prompts(s.claudeSessionId).at(-1)!, (p) => p.includes('round 1'), 30000, 'the fix prompt');
    const fix = prompts(s.claudeSessionId).at(-1)!;
    expect(fix).toContain("Don't commit, push or open the PR yet");
    expect(fix).toContain('[1:1] `src/a.ts:2` **blocker**: Off by one.');
    // Round 2 finds a minor; its prompt names round 1's finding.
    fake('review-reports.json', { ...LEDGERS, diff: { summary: 'LGTM', findings: [{ file: 'src/a.ts', line: 5, severity: 'minor', comment: 'Name it better.', gotchas: [] }] } });
    const done = await waitFor(() => env.core.sessions.get(s.id), (x) => x.stage === 'pr', 30000, 'the PR prompt after round 2');
    expect(done.codeReview).toMatchObject({ round: 2, state: 'done', counts: { blocker: 1, major: 0, minor: 1 } });
    expect(await prPrompt(s.claudeSessionId)).toContain('me_ak_review:1 blocker,1 minor addressed');
    // Round 2 running isn't round 1's outcome: the counts stay until the task reports both rounds'.
    const outcome = (key: string) => env.core.reviews.onTool(env.core.sessions.row(s.id), 'review_outcome', { outcomes: [{ key, status: 'fixed', note: 'Done.' }] });
    outcome('2:1');
    expect(env.core.sessions.get(s.id).codeReview!.addressed).toBe(false);
    outcome('1:1');
    expect(env.core.sessions.get(s.id).codeReview!.addressed).toBe(true);
    const round2 = readFileSync(join(env.core.paths.run, 'reviews', done.codeReview!.id, 'r2', 'base.md'), 'utf8');
    expect(round2).toContain('Off by one.');
  });

  it('holds the PR when no pass reports, until Open anyway sends it without a line', async () => {
    const s = await task({ review: { strategy: 'sirius-1.0' } });
    fake('review-skip', ['r1s1:all']);
    await env.core.sessions.action(s.id, 'open-pr');
    const failed = await waitFor(() => env.core.sessions.get(s.id), (x) => x.codeReview?.state === 'failed', 30000, 'the review to fail');
    expect(failed.stage).toBe('code');
    expect(failed.codeReview!.detail).toBe('no review pass reported');
    await expect(env.core.sessions.action(s.id, 'open-pr')).rejects.toThrow(/Retry it, or open the PR anyway/);
    await env.core.sessions.action(s.id, 'review-open-anyway');
    await waitFor(() => env.core.sessions.get(s.id), (x) => x.stage === 'pr', 10000, 'the PR prompt');
    expect(await prPrompt(s.claudeSessionId)).not.toContain('me_ak_review');
  });

  it('runs the review for Run to merge\'s Open PR too, and the PR goes when it\'s done', async () => {
    const p = await env.core.projects.create({ repoPath: env.repo, name: 'Demo', datapack: { mode: 'none' } });
    const s = await env.core.sessions.create(p.id, { name: 'Fix the thing', prompt: 'do it', start: 'code', autoRun: true, review: { strategy: 'sirius-1.0' } });
    const done = await waitFor(() => env.core.sessions.get(s.id), (x) => x.stage === 'pr', 30000, 'the PR prompt after the review');
    expect(done.codeReview).toMatchObject({ strategy: 'sirius-1.0', state: 'done' });
    expect(await prPrompt(s.claudeSessionId)).toContain('me_ak_review:LGTM');
  });

  it('skips the review when the project turns it off', async () => {
    const s = await task();
    env.core.projects.update(s.projectId, { settings: { codeReview: { strategy: 'off' } } });
    await env.core.sessions.action(s.id, 'open-pr');
    expect(env.core.sessions.get(s.id)).toMatchObject({ stage: 'pr', codeReview: null });
  });

  it('archives the review session with its task, and leaves the task\'s worktree and branch alone', async () => {
    const s = await task({ review: { strategy: 'sirius-1.0' } });
    fake('review-skip', ['r1s1:all']);
    await env.core.sessions.action(s.id, 'open-pr');
    const r = await waitFor(() => env.core.sessions.get(s.id), (x) => x.codeReview?.state === 'failed', 30000, 'the review to fail');
    const childId = all<{ id: string }>(env.core.db, 'SELECT id FROM sessions WHERE parent_id = ?', s.id)[0].id;
    const log = mekoLogPath(env.core.paths.designs, childId);
    mkdirSync(dirname(log), { recursive: true });
    writeFileSync(log, '');
    // Archiving the review session alone keeps the task's worktree, and its Meko call log for the task's life.
    await env.core.sessions.archive(env.core.sessions.row(childId), { keepBranch: false });
    expect(env.core.sessions.row(s.id).worktree_released).toBe(0);
    expect(r.codeReview!.sessionId).toBe(childId);
    expect(env.core.sessions.get(s.id).codeReview!.sessionId).toBeNull();
    expect(existsSync(log)).toBe(true);
    await env.core.sessions.archive(env.core.sessions.row(s.id), { keepBranch: true });
    expect(existsSync(log)).toBe(false);
  });
});
