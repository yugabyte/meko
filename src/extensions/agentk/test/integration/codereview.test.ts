import { existsSync, mkdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

// GitHub is mocked (no PR is found; one set on a session by hand reads as open, with `gh.body`,
// after `gh.statusMs`); git, the session host and the fake claude are real.
const gh = vi.hoisted(() => ({ body: '', statusMs: 0 }));
vi.mock('../../src/server/github.js', async (orig) => {
  const real = await orig<typeof import('../../src/server/github.js')>();
  return {
    ...real, findPr: async () => null, viewerLogin: async () => 'me', repoVisibility: async () => 'private',
    prStatus: async (_cwd: string, number: number) => (await new Promise((r) => setTimeout(r, gh.statusMs)), {
      number, url: `https://github.com/acme/demo-repo/pull/${number}`, title: 'feat(demo): add f', body: gh.body, state: 'OPEN', reviews: [],
      statusCheckRollup: [], reviewDecision: '', mergeable: 'MERGEABLE', mergeStateStatus: 'CLEAN', headRefOid: 'abc123',
    }),
    prBody: async () => gh.body,
    setPrBody: async (_cwd: string, _number: number, body: string) => {
      gh.body = body;
    },
  };
});

const { makeEnv, readJsonl, waitFor } = await import('../helpers.js');
const { all, run } = await import('../../src/server/db.js');
const { DESCRIBE_INTERRUPTED, DESCRIBE_LOST, REVIEW_LEFT_CODE, REVIEW_LEFT_REVIEW } = await import('../../src/shared/codereview.js');
type TestEnv = Awaited<ReturnType<typeof makeEnv>>;

/** Reports for the ledger passes that clear the change: the dependents and claims passes need their ledgers. */
const LEDGERS = {
  dependents: { state_ledger: [], summary: 'LGTM', findings: [] },
  claims: { claims: [], summary: 'LGTM', findings: [] },
};

describe('code review before the PR (real session host + git, fake claude running the review steps)', { timeout: 60000 }, () => {
  let env: TestEnv;
  beforeEach(async () => {
    gh.statusMs = 0;
    env = await makeEnv({ review: true });
  });
  afterEach(async () => {
    await env.cleanup();
  });

  const prompts = (claudeSessionId: string) => readJsonl<{ type: string; text: string }>(join(env.fakeDir, `${claudeSessionId}.jsonl`)).filter((e) => e.type === 'user' && typeof e.text === 'string').map((e) => e.text);
  const fake = (file: string, v: unknown) => writeFileSync(join(env.fakeDir, file), JSON.stringify(v));
  /** The task's PR prompt, once it has reached Claude (the stage moves to PR just before it's typed). */
  const prPrompt = (id: string) => waitFor(() => prompts(id).at(-1) ?? '', (p) => p.includes('Open a pull request'), 10000, 'the PR prompt');

  /** A task at Code (or where `req.start` says) whose worktree has a new file with six lines, ready for Open PR. */
  async function task(req: Record<string, unknown> = {}, datapack: Parameters<TestEnv['core']['projects']['create']>[0]['datapack'] = { mode: 'none' }) {
    const p = await env.core.projects.create({ repoPath: env.repo, name: 'Demo', datapack });
    const s = await env.core.sessions.create(p.id, { name: 'Fix the thing', prompt: 'do it', start: 'code', ...req });
    await waitFor(() => env.core.sessions.get(s.id), (x) => x.status === 'idle', 10000, 'first turn');
    const wt = env.core.sessions.row(s.id).worktree_path;
    mkdirSync(join(wt, 'src'), { recursive: true });
    writeFileSync(join(wt, 'src', 'a.ts'), ['export function f(n: number) {', '  return n + 1;', '}', 'export const x = f(1);', 'export const y = f(2);', ''].join('\n'));
    return s;
  }

  it('reviews the change at Open PR in a child session, then sends the PR prompt with the findings and the me_kode_review line', async () => {
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
    expect(pr).toContain('Kode reviewed this change locally (Sagittarius 3.0) before this PR.');
    expect(pr).toContain('[1:1] `src/a.ts:2` **blocker**: Off by one.');
    expect(pr).toContain('`src/a.ts:4` **major** (Meko gotcha MG-3): Breaks the guide.');
    expect(pr).not.toContain('Not this change.');
    expect(pr).toContain('me_kode_review:1 blocker,1 major addressed;Sagittarius3.0');

    // The review ran in a code-review session on the task's worktree, with read-only subagents.
    const childId = all<{ id: string }>(env.core.db, 'SELECT id FROM sessions WHERE parent_id = ?', s.id)[0].id;
    const child = env.core.sessions.row(childId);
    expect(child).toMatchObject({ kind: 'code-review', parent_id: s.id, stage: 'reviewing-code', worktree_path: env.core.sessions.row(s.id).worktree_path, worktree_released: 1 });
    const launch = readJsonl<{ args: string[]; env: Record<string, string | undefined> }>(join(env.fakeDir, 'argv.jsonl')).find((a) => a.args.includes('--agents'))!;
    const argv = launch.args;
    expect(Object.keys(JSON.parse(argv[argv.indexOf('--agents') + 1]))).toEqual(['kode-review-large', 'kode-review-mid', 'kode-review-small']);
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
    // Its datapacks are names, the gotchas one marked: what each holds is for the prompt only.
    run(env.core.db, 'UPDATE code_reviews SET datapacks_json = ? WHERE task_id = ?', JSON.stringify([{ name: 'adlc/repo/demo-repo/gotchas', id: 'g-id', about: 'Gotchas.', gotchas: true }]), s.id);
    expect(env.core.reviews.detail(s.id).datapacks).toEqual([{ name: 'adlc/repo/demo-repo/gotchas', id: 'g-id', gotchas: true }]);
    expect(detail.findings.some((f) => f.pending)).toBe(false);
    // While a round runs, after the stored findings, every candidate of the round comes marked pending.
    run(env.core.db, `UPDATE code_reviews SET state = 'sweeping' WHERE task_id = ?`, s.id);
    const live = env.core.reviews.detail(s.id).findings;
    expect(live.slice(detail.findings.length).map((f) => [f.file, f.kept, f.dropped, f.pending])).toEqual(
      expect.arrayContaining([['src/a.ts', false, null, true], ['README.md', false, null, true]]),
    );
    run(env.core.db, `UPDATE code_reviews SET state = 'done' WHERE task_id = ?`, s.id);
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

  it('scores the candidates in batches of 5, and asks again for a missing score in a batch of its own', async () => {
    const s = await task();
    const wt = env.core.sessions.row(s.id).worktree_path;
    writeFileSync(join(wt, 'src', 'b.ts'), Array.from({ length: 8 }, (_, i) => `export const b${i} = ${i};`).join('\n') + '\n');
    const findings = Array.from({ length: 7 }, (_, i) => ({ file: 'src/b.ts', line: i + 1, severity: 'minor', comment: `Finding ${i + 1}.`, gotchas: [] }));
    fake('review-reports.json', { ...LEDGERS, diff: { summary: 'LGTM', findings } });
    fake('review-skip-once', ['r1c6']);
    await env.core.sessions.action(s.id, 'open-pr');
    const done = await waitFor(() => env.core.sessions.get(s.id), (x) => x.stage === 'pr', 30000, 'the PR prompt after the review');
    expect(done.codeReview).toMatchObject({ state: 'done', counts: { blocker: 0, major: 0, minor: 7 } });

    const childId = all<{ id: string }>(env.core.db, 'SELECT id FROM sessions WHERE parent_id = ?', s.id)[0].id;
    const scoring = prompts(env.core.sessions.row(childId).claude_session_id).filter((p) => p.includes('score 7 findings'));
    expect(scoring).toHaveLength(2);
    const [first, again] = scoring;
    expect(first).toContain('Batch 2:');
    // Asked again, only the missing one, in a batch of its own.
    expect(again).toMatch(/Batch 1:\s+1\. score 6: subagent `kode-review-small`/);
    expect(again).not.toContain('score 1:');
    const calls = readJsonl<{ step: string }>(join(env.fakeDir, 'review-calls.jsonl')).map((c) => c.step).filter((c) => /^r1c/.test(c));
    expect(calls).toEqual(['r1c1', 'r1c2', 'r1c3', 'r1c4', 'r1c5', 'r1c7', 'r1c6']);
    expect(env.core.reviews.detail(s.id).findings.map((f) => f.verdict)).toEqual(Array(7).fill('confirmed'));
  });

  it('asks the task for the PR description with its Meko lines first, reviews the change against it, then opens the PR with it', async () => {
    // A datapack id as Meko gives them: hex, which a PR description's datapack line is told by.
    const DP = '0f1e2d3c-1111';
    const s = await task({ review: { strategy: 'sirius-1.0' } }, { mode: 'existing', id: DP, name: 'team/demo' });
    mkdirSync(env.fakeDir, { recursive: true });
    writeFileSync(join(env.fakeDir, 'pr-description.md'), `feat(demo): add f\n\nAdds f and calls it twice.\n\ncloud.mekodata.ai://${DP};team/demo\nmeko_conversation_id:conv-1\n`);
    await env.core.sessions.action(s.id, 'open-pr');
    const done = await waitFor(() => env.core.sessions.get(s.id), (x) => x.stage === 'pr', 30000, 'the PR prompt after the review');
    const file = env.core.sessions.prDescriptionFile(env.core.sessions.row(s.id));
    const asked = prompts(s.claudeSessionId).find((p) => p.includes(`write the PR's title and description to ${file}`))!;
    expect(asked).toMatch(/End the PR description with these lines.*cloud\.mekodata\.ai:\/\/0f1e2d3c-1111;team\/demo\s+meko_conversation_id:conv-1$/s);
    // The passes read it as the PR's title and description, the task it was written for after it.
    const base = readFileSync(join(env.core.paths.run, 'reviews', done.codeReview!.id, 'r1', 'base.md'), 'utf8');
    // Its title too, quoted: the change's heading stays the task's own name.
    expect(base).toContain('# The change: Fix the thing');
    expect(base).toContain(`> feat(demo): add f\n>\n> Adds f and calls it twice.\n>\n> cloud.mekodata.ai://${DP};team/demo\n> meko_conversation_id:conv-1\n\nThe task it was written for:`);
    expect(await prPrompt(s.claudeSessionId)).toContain(`Use the title and description you wrote to ${file} for the PR`);
  });

  it.each([['Open PR', false], ['Retry', true]])('waits on a PR description turn the user interrupted, through a restart, and goes on at %s', async (_how, retry) => {
    // Through the mod, which reports an interrupted turn (no Stop hook fires for one).
    mkdirSync(env.fakeDir, { recursive: true });
    writeFileSync(join(env.fakeDir, 'mod'), '');
    const s = await task({ review: { strategy: 'sirius-1.0' } });
    await waitFor(() => env.core.sessions.mod.live(s.id), (l) => l, 10000, 'the mod to poll');
    writeFileSync(join(env.fakeDir, 'interrupt'), '');
    await env.core.sessions.action(s.id, 'open-pr');
    const interrupted = () => all<{ text: string }>(env.core.db, "SELECT text FROM session_events WHERE session_id = ? AND kind = 'turn'", s.id).some((e) => e.text === 'You interrupted the turn');
    await waitFor(() => interrupted() && env.core.sessions.get(s.id).status === 'idle', (x) => x, 30000, 'the interrupted description turn');
    expect(env.core.sessions.get(s.id).codeReview).toMatchObject({ state: 'describing', detail: DESCRIBE_INTERRUPTED });
    rmSync(join(env.fakeDir, 'interrupt'));
    // A restart leaves it waiting on the user, as an interrupted fix turn does.
    await env.core.reviews.recover();
    const id = env.core.sessions.get(s.id).codeReview!.id;
    await env.core.reviews.settled(id);
    expect(env.core.sessions.get(s.id).codeReview!.state).toBe('describing');
    const asks = () => prompts(s.claudeSessionId).filter((p) => p.includes("write the PR's title and description")).length;
    if (retry) {
      // Cancelled before round 1 had a description, a Retry asks for it again.
      await env.core.sessions.action(s.id, 'review-cancel');
      await env.core.sessions.action(s.id, 'review-retry');
      await waitFor(asks, (n) => n === 2, 10000, 'the ask again');
    } else {
      await env.core.sessions.action(s.id, 'open-pr');
    }
    const done = await waitFor(() => env.core.sessions.get(s.id), (x) => x.stage === 'pr', 30000, 'the PR prompt after the review');
    expect(done.codeReview).toMatchObject({ round: 1, state: 'done' });
    if (!retry) expect(asks()).toBe(1);
  });

  it('asks for the PR description again at Open PR when Claude exited before taking the ask', async () => {
    mkdirSync(env.fakeDir, { recursive: true });
    writeFileSync(join(env.fakeDir, 'mod'), '');
    const s = await task({ review: { strategy: 'sirius-1.0' } });
    await waitFor(() => env.core.sessions.mod.live(s.id), (l) => l, 10000, 'the mod to poll');
    writeFileSync(join(env.fakeDir, 'exit-unrun'), "write the PR's title and description");
    await env.core.sessions.action(s.id, 'open-pr');
    await waitFor(() => env.core.sessions.get(s.id), (x) => x.status === 'exited' && x.codeReview?.detail === DESCRIBE_LOST, 30000, 'the lost ask');
    expect(env.core.sessions.get(s.id)).toMatchObject({ busy: false, codeReview: { state: 'describing' } });
    await env.core.sessions.action(s.id, 'resume');
    await waitFor(() => env.core.sessions.get(s.id).status, (st) => st === 'idle', 10000, 'Claude back');
    await env.core.sessions.action(s.id, 'open-pr');
    const done = await waitFor(() => env.core.sessions.get(s.id), (x) => x.stage === 'pr', 30000, 'the PR prompt after the review');
    expect(done.codeReview).toMatchObject({ round: 1, state: 'done' });
    expect(prompts(s.claudeSessionId).filter((p) => p.includes("write the PR's title and description"))).toHaveLength(1);
  });

  it('opens the PR with me_kode_review:LGTM;Sirius1.0 when Sirius 1.0 finds nothing, set for the session alone', async () => {
    const s = await task({ review: { strategy: 'sirius-1.0' } });
    expect(env.core.sessions.get(s.id).review).toEqual({ strategy: 'sirius-1.0' });
    await env.core.sessions.action(s.id, 'open-pr');
    await waitFor(() => env.core.sessions.get(s.id), (x) => x.stage === 'pr', 30000, 'the PR prompt after the review');
    expect(await prPrompt(s.claudeSessionId)).toContain('me_kode_review:LGTM;Sirius1.0');
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
    expect(await prPrompt(s.claudeSessionId)).toContain('me_kode_review:1 blocker,1 minor addressed;Sagittarius3.0');
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
    // It holds the PR: Open PR starts nothing, and the bar is on its Code Review step.
    expect(failed).toMatchObject({ openPrReview: null });
    expect(failed.skippedStages).not.toContain('reviewing-code');
    await expect(env.core.sessions.action(s.id, 'open-pr')).rejects.toThrow(/Retry it, or open the PR anyway/);
    // A move to Pull Request is refused alike: it would open the PR past the stopped review. So it is
    // with the review turned off since, as Open PR is.
    await expect(env.core.sessions.move(s.id, { stage: 'review' })).rejects.toThrow(/Retry it, or open the PR anyway/);
    env.core.projects.update(s.projectId, { settings: { codeReview: { strategy: 'off' } } });
    await expect(env.core.sessions.move(s.id, { stage: 'review' })).rejects.toThrow(/Retry it, or open the PR anyway/);
    expect(env.core.sessions.get(s.id).stage).toBe('code');
    expect(env.core.sessions.get(s.id).skippedStages).not.toContain('reviewing-code');
    await env.core.sessions.action(s.id, 'review-open-anyway');
    await waitFor(() => env.core.sessions.get(s.id), (x) => x.stage === 'pr', 10000, 'the PR prompt');
    expect(await prPrompt(s.claudeSessionId)).not.toContain('me_kode_review');
    // The PR went without a finished review: Code Review was skipped.
    expect(env.core.sessions.get(s.id)).toMatchObject({ openPrReview: null, skippedStages: ['explore', 'design', 'reviewing-code'] });
  });

  it('runs the review for Run to merge\'s Open PR too, and the PR goes when it\'s done', async () => {
    const p = await env.core.projects.create({ repoPath: env.repo, name: 'Demo', datapack: { mode: 'none' } });
    const s = await env.core.sessions.create(p.id, { name: 'Fix the thing', prompt: 'do it', start: 'code', autoRun: true, review: { strategy: 'sirius-1.0' } });
    const done = await waitFor(() => env.core.sessions.get(s.id), (x) => x.stage === 'pr', 30000, 'the PR prompt after the review');
    expect(done.codeReview).toMatchObject({ strategy: 'sirius-1.0', state: 'done' });
    expect(await prPrompt(s.claudeSessionId)).toContain('me_kode_review:LGTM;Sirius1.0');
    // The PR went with the review: Code Review is done, not skipped.
    expect(env.core.sessions.get(s.id).skippedStages).toEqual(['explore', 'design']);
  });

  describe("a review that finishes while the task's turn runs", () => {
    const finished = (id: string) => all<{ finished_at: number | null }>(env.core.db, 'SELECT finished_at FROM code_reviews WHERE task_id = ?', id)[0].finished_at;
    const events = (id: string, kind: string) => all<{ text: string }>(env.core.db, 'SELECT text FROM session_events WHERE session_id = ? AND kind = ? ORDER BY id', id, kind).map((e) => e.text);
    /** Resolves once every step queued for the session is done: a stuck one never lets it (#415). */
    const stepsDone = (id: string) => Promise.race([
      (env.core.lifecycle as unknown as { serial(id: string, f: () => Promise<string>): Promise<string> }).serial(id, async () => 'done'),
      new Promise((r) => setTimeout(() => r('stuck'), 5000)),
    ]);
    /** A prompt of the user's starts a turn on the task just as the review's PR prompt is to go, so it finds Kode working. */
    function userTurnAtFinish(id: string) {
      const send = env.core.lifecycle.sendReviewedPr.bind(env.core.lifecycle);
      vi.spyOn(env.core.lifecycle, 'sendReviewedPr').mockImplementationOnce(async (t, o) => {
        writeFileSync(join(env.fakeDir, 'turn-ms'), '1500');
        await env.core.sessions.submit(id, 'Tidy up the tests.');
        await waitFor(() => env.core.sessions.get(id).status, (st) => st === 'working', 10000, "the user's turn");
        rmSync(join(env.fakeDir, 'turn-ms'), { force: true });
        return send(t, o);
      });
    }
    /** The PR prompt went at the end of the user's turn, with the review's line, and nothing is left queued. */
    async function sentAfterTurn(id: string, claudeSessionId: string) {
      await waitFor(() => env.core.sessions.get(id), (x) => x.stage === 'pr', 15000, "the PR prompt at the turn's end");
      expect(await prPrompt(claudeSessionId)).toContain('me_kode_review:LGTM;Sirius1.0');
      const sent = prompts(claudeSessionId);
      expect(sent.at(-2)).toBe('Tidy up the tests.');
      expect(sent.filter((p) => p.includes('Open a pull request'))).toHaveLength(1);
      expect(finished(id)).not.toBeNull();
      expect(env.core.sessions.row(id).queued_gate).toBeNull();
      expect(await stepsDone(id)).toBe('done');
    }

    it('queues Open PR, and its PR prompt goes when the turn ends', async () => {
      const s = await task({ review: { strategy: 'sirius-1.0' } });
      userTurnAtFinish(s.id);
      await env.core.sessions.action(s.id, 'open-pr');
      await waitFor(() => events(s.id, 'review'), (e) => e.includes('The code review is done: the PR prompt goes when Kode finishes its turn'), 30000, 'the review to finish');
      expect(env.core.sessions.get(s.id)).toMatchObject({ stage: 'code', status: 'working', queued: 'open-pr', codeReview: { state: 'done' } });
      expect(env.core.sessions.row(s.id).queued_auto).toBe(0);
      expect(finished(s.id)).toBeNull();
      await sentAfterTurn(s.id, s.claudeSessionId);
      expect(events(s.id, 'review').some((e) => e.startsWith("The PR prompt didn't go"))).toBe(false);
    });

    it("sends it at the turn's end with Run to merge's Open PR, queued at the turn's start", async () => {
      const s = await task({ review: { strategy: 'sirius-1.0' } });
      userTurnAtFinish(s.id);
      await env.core.sessions.edit(s.id, { autoRun: true });
      await waitFor(() => events(s.id, 'review'), (e) => e.includes('The code review is done: the PR prompt goes when Kode finishes its turn'), 30000, 'the review to finish');
      expect(env.core.sessions.row(s.id)).toMatchObject({ queued_gate: 'open-pr', queued_auto: 1 });
      await sentAfterTurn(s.id, s.claudeSessionId);
    });

    it("doesn't queue Run to merge's review PR once Run to merge is off: it waits for your Open PR", async () => {
      const s = await task({ review: { strategy: 'sirius-1.0' } });
      userTurnAtFinish(s.id);
      await env.core.sessions.edit(s.id, { autoRun: true });
      expect(env.core.sessions.get(s.id).codeReview).toMatchObject({ state: expect.stringMatching(/describing|preparing|sweeping|scoring/) });
      await env.core.sessions.edit(s.id, { autoRun: false });
      await waitFor(() => events(s.id, 'review'), (e) => e.some((t) => t.startsWith("The PR prompt didn't go after the code review: Kode is still working.")), 30000, 'the review to finish');
      expect(env.core.sessions.row(s.id)).toMatchObject({ queued_gate: null, auto_run: 0 });
      await waitFor(() => env.core.sessions.get(s.id).status, (st) => st === 'idle', 10000, "the user's turn to end");
      expect(env.core.sessions.get(s.id)).toMatchObject({ stage: 'code', codeReview: { state: 'done' } });
      expect(finished(s.id)).toBeNull();
      // Your Open PR sends it, as yours.
      await env.core.sessions.action(s.id, 'open-pr');
      await sentAfterTurn(s.id, s.claudeSessionId);
      expect(all<{ by: string }>(env.core.db, 'SELECT by FROM code_reviews WHERE task_id = ?', s.id)[0].by).toBe('you');
    });

    it('sends it at a move to Pull Request between turns, once the queued Open PR was withdrawn', async () => {
      const s = await task({ review: { strategy: 'sirius-1.0' } });
      userTurnAtFinish(s.id);
      await env.core.sessions.action(s.id, 'open-pr');
      await waitFor(() => env.core.sessions.row(s.id).queued_gate, (g) => g === 'open-pr', 30000, 'Open PR queued for the turn');
      await env.core.sessions.action(s.id, 'cancel-queued');
      await waitFor(() => env.core.sessions.get(s.id).status, (st) => st === 'idle', 10000, "the user's turn to end");
      expect(env.core.sessions.get(s.id)).toMatchObject({ stage: 'code', codeReview: { state: 'done' } });
      expect(finished(s.id)).toBeNull();
      await env.core.sessions.move(s.id, { stage: 'review' });
      await sentAfterTurn(s.id, s.claudeSessionId);
    });

    it('sends nothing when the queued Open PR is withdrawn while its PR prompt is being prepared', async () => {
      const s = await task({ review: { strategy: 'sirius-1.0' } });
      userTurnAtFinish(s.id);
      await env.core.sessions.action(s.id, 'open-pr');
      await waitFor(() => env.core.sessions.row(s.id).queued_gate, (g) => g === 'open-pr', 30000, 'Open PR queued for the turn');
      // The Cancel lands while sendPr waits on Meko for the PR's lines, after the turn's end took the queued Open PR.
      const lc = env.core.lifecycle as unknown as { prFooter(row: unknown): Promise<string> };
      const footer = lc.prFooter.bind(lc);
      let cancelled = false;
      vi.spyOn(lc, 'prFooter').mockImplementationOnce(async (row) => {
        await env.core.sessions.action(s.id, 'cancel-queued');
        cancelled = true;
        return footer(row);
      });
      await waitFor(() => cancelled, (c) => c, 10000, 'the Cancel while the PR prompt is prepared');
      expect(await stepsDone(s.id)).toBe('done');
      expect(env.core.sessions.get(s.id)).toMatchObject({ stage: 'code', status: 'idle', queued: null, codeReview: { state: 'done' } });
      expect(finished(s.id)).toBeNull();
      expect(prompts(s.claudeSessionId).some((p) => p.includes('Open a pull request'))).toBe(false);
      expect(events(s.id, 'queued').at(-1)).toBe('Queued Open PR withdrawn');
      expect(events(s.id, 'review').some((e) => e.startsWith("The PR prompt didn't go"))).toBe(false);
    });
  });

  it.each([['through the mod', true], ['typed', false]])('sends the PR prompt again once Claude is back, when Claude exits before taking it (%s)', async (_how, mod) => {
    if (mod) {
      mkdirSync(env.fakeDir, { recursive: true });
      writeFileSync(join(env.fakeDir, 'mod'), '');
    }
    const s = await task({ review: { strategy: 'sirius-1.0' } });
    // Like Claude Code moving the conversation to its background sessions as the PR prompt comes.
    writeFileSync(join(env.fakeDir, 'exit-unrun'), 'Open a pull request');
    // Typed: Claude's SessionEnd lands before the session host answers the paste.
    let answered: Promise<void> = Promise.resolve();
    if (!mod) {
      const submit = env.core.host.submit.bind(env.core.host);
      vi.spyOn(env.core.host, 'submit').mockImplementation(async (name, text) => {
        if (!text.includes('Open a pull request')) return submit(name, text);
        vi.mocked(env.core.host.submit).mockRestore();
        answered = (async () => {
          await submit(name, text);
          await waitFor(() => env.core.sessions.get(s.id).status, (st) => st === 'exited', 10000, 'SessionEnd');
        })();
        return answered;
      });
    }
    await env.core.sessions.action(s.id, 'open-pr');
    await waitFor(() => env.core.sessions.get(s.id).status, (st) => st === 'exited', 30000, 'Claude to exit with the PR prompt');
    // Typed, the send finishes after the exit: it mustn't count the prompt as sent, or the session as working.
    await answered;
    // Past the send's end too (sendReviewedPr runs in the session's serial queue).
    await (env.core.lifecycle as unknown as { serial(id: string, f: () => Promise<void>): Promise<void> }).serial(s.id, async () => undefined);
    await new Promise((r) => setImmediate(r));
    const lost = env.core.sessions.get(s.id);
    if (mod) expect(readJsonl<{ text: string }>(join(env.fakeDir, 'mod-prompts.jsonl')).some((p) => p.text.includes('Open a pull request'))).toBe(true);
    // Back where it was, not busy, and no turn was cut short: the review's PR counts as not sent.
    expect(lost).toMatchObject({ status: 'exited', stage: 'code', busy: false, codeReview: { state: 'done' } });
    expect(env.core.sessions.row(s.id).interrupted).toBe(0);
    const finished = () => all<{ finished_at: number | null }>(env.core.db, 'SELECT finished_at FROM code_reviews WHERE task_id = ?', s.id)[0].finished_at;
    expect(finished()).toBeNull();
    expect(all<{ text: string }>(env.core.db, "SELECT text FROM session_events WHERE session_id = ? AND kind = 'pr'", s.id).map((e) => e.text))
      .toEqual(['Claude exited before taking the PR prompt; it goes again once Claude is back']);
    await env.core.sessions.action(s.id, 'resume');
    await waitFor(() => env.core.sessions.get(s.id), (x) => x.stage === 'pr', 10000, 'the PR prompt again');
    expect(await prPrompt(s.claudeSessionId)).toContain('me_kode_review:LGTM;Sirius1.0');
    expect(prompts(s.claudeSessionId).filter((p) => p.includes('Open a pull request'))).toHaveLength(1);
    // Written once the prompt is typed, after the stage moved to Pull Request (Lifecycle.sendPr).
    await waitFor(finished, (f) => f !== null, 10000, "the review's finish");
  });

  it('reviews the change in Code when the session is moved to Pull Request by hand, then sends the PR prompt with the move\'s instructions', async () => {
    const s = await task({ start: 'explore', review: { strategy: 'sirius-1.0' } });
    await env.core.sessions.move(s.id, { stage: 'review', instructions: 'Mention the demo.' });
    // The review holds the PR, in Code, as for Open PR.
    expect(env.core.sessions.get(s.id).stage).toBe('code');
    const done = await waitFor(() => env.core.sessions.get(s.id), (x) => x.stage === 'pr', 30000, 'the PR prompt after the review');
    expect(done.codeReview).toMatchObject({ strategy: 'sirius-1.0', state: 'done' });
    const pr = await prPrompt(s.claudeSessionId);
    // Claude's system prompt still says Explore (no PR): the moved line leads the review's first prompt to it, the ask for the PR description.
    const asked = prompts(s.claudeSessionId).find((p) => p.includes("write the PR's title and description"))!;
    expect(asked.startsWith('Kode moved this session to the Code stage.')).toBe(true);
    expect(pr.startsWith('Kode moved this session')).toBe(false);
    expect(pr).toContain('me_kode_review:LGTM;Sirius1.0');
    expect(pr).toContain('Mention the demo.');
    expect(all<{ stage: string }>(env.core.db, 'SELECT stage FROM session_stages WHERE session_id = ? ORDER BY rowid', s.id).map((r) => r.stage)).toEqual(['explore', 'code', 'pr']);
  });

  it('starts a new review on a move to Pull Request from outside Code, past one that failed in Code', async () => {
    const s = await task({ review: { strategy: 'sirius-1.0' } });
    fake('review-skip', ['r1s1:all']);
    await env.core.sessions.action(s.id, 'open-pr');
    const failed = await waitFor(() => env.core.sessions.get(s.id), (x) => x.codeReview?.state === 'failed', 30000, 'the review to fail');
    // Its Retry and Open anyway are Code's: leaving Code lets it go.
    await env.core.sessions.move(s.id, { stage: 'explore', prompt: false });
    expect(env.core.sessions.get(s.id).codeReview).toMatchObject({ state: 'cancelled', detail: REVIEW_LEFT_CODE });
    fake('review-skip', []);
    await env.core.sessions.move(s.id, { stage: 'review' });
    const done = await waitFor(() => env.core.sessions.get(s.id), (x) => x.stage === 'pr', 30000, 'the PR prompt after the new review');
    expect(done.codeReview).toMatchObject({ state: 'done' });
    expect(done.codeReview!.id).not.toBe(failed.codeReview!.id);
    expect(await prPrompt(s.claudeSessionId)).toContain('me_kode_review:LGTM;Sirius1.0');
  });

  describe('a fix turn the user interrupted', () => {
    /**
     * Round 1 finds a blocker whose fix turn is interrupted (the fake interrupts a prompt saying
     * INTERRUPT): the review waits for fixes. `check`: the findings wait on the user first, and Send
     * queues the fix prompt (`local-review`) rather than typing it straight in.
     */
    async function interruptedFix(check = false) {
      // Through the mod, which reports an interrupted turn (no Stop hook fires for one).
      mkdirSync(env.fakeDir, { recursive: true });
      writeFileSync(join(env.fakeDir, 'mod'), '');
      const s = await task({ review: { rounds: 1, check } });
      await waitFor(() => env.core.sessions.mod.live(s.id), (l) => l, 10000, 'the mod to poll');
      fake('review-reports.json', { ...LEDGERS, diff: { summary: 'LGTM', findings: [{ file: 'src/a.ts', line: 2, severity: 'blocker', comment: 'Off by one. INTERRUPT', gotchas: [] }] } });
      await env.core.sessions.action(s.id, 'open-pr');
      if (check) {
        await waitFor(() => env.core.sessions.get(s.id).codeReview?.state, (st) => st === 'triage', 30000, 'the findings to wait on you');
        await env.core.sessions.action(s.id, 'review-send');
      }
      const interrupted = () => all<{ text: string }>(env.core.db, "SELECT text FROM session_events WHERE session_id = ? AND kind = 'turn'", s.id).some((e) => e.text === 'You interrupted the turn');
      await waitFor(() => interrupted() && env.core.sessions.get(s.id).status === 'idle', (x) => x, 30000, 'the interrupted fix turn');
      expect(env.core.sessions.get(s.id).codeReview!.state).toBe('fixing');
      fake('review-reports.json', LEDGERS);
      return s;
    }

    it.each([['typed straight in', false], ['sent from triage, through the queue', true]])('goes on with the next round at Open PR, its fix prompt %s', async (_how, check) => {
      const s = await interruptedFix(check);
      await env.core.sessions.action(s.id, 'open-pr');
      const done = await waitFor(() => env.core.sessions.get(s.id), (x) => x.stage === 'pr', 30000, 'the PR prompt after round 2');
      expect(done.codeReview).toMatchObject({ round: 2, state: 'done' });
    });

    it('is cancelled by a move out of Code, and a move back to Pull Request starts a new review', async () => {
      const s = await interruptedFix();
      const first = env.core.sessions.get(s.id).codeReview!.id;
      await env.core.sessions.move(s.id, { stage: 'explore', prompt: false });
      expect(env.core.sessions.get(s.id).codeReview).toMatchObject({ state: 'cancelled', detail: REVIEW_LEFT_CODE });
      await env.core.sessions.move(s.id, { stage: 'review' });
      const done = await waitFor(() => env.core.sessions.get(s.id), (x) => x.stage === 'pr', 30000, 'the PR prompt after the new review');
      expect(done.codeReview).toMatchObject({ round: 1, state: 'done' });
      expect(done.codeReview!.id).not.toBe(first);
    });
  });

  describe('a review the user runs (review-run)', () => {
    const reviews = (id: string) => all<{ id: string; purpose: string; stage: string; strategy: string }>(env.core.db, 'SELECT id, purpose, stage, strategy FROM code_reviews WHERE task_id = ? ORDER BY created_at', id);
    /** The task took the `n`th ask for its PR description, and that turn is over. */
    const described = (s: { id: string; claudeSessionId: string }, n: number) => waitFor(
      () => ({ asks: prompts(s.claudeSessionId).filter((p) => p.includes("write the PR's title and description")).length, row: env.core.sessions.get(s.id) }),
      (v) => v.asks >= n && v.row.status === 'idle' && !v.row.busy, 10000, 'the PR description turn');

    it('runs in Code with the default strategy while it is Off, fixes its findings, and Open PR then sends the PR with its line without reviewing again', async () => {
      const s = await task();
      env.core.projects.update(s.projectId, { settings: { codeReview: { strategy: 'off' } } });
      fake('review-reports.json', { ...LEDGERS, diff: { summary: 'LGTM', findings: [{ file: 'src/a.ts', line: 2, severity: 'blocker', comment: 'Off by one.', gotchas: [] }] } });
      await env.core.sessions.action(s.id, 'review-run');
      const done = await waitFor(() => env.core.sessions.get(s.id), (x) => x.codeReview?.state === 'done', 30000, 'the review to finish');
      expect(done).toMatchObject({ stage: 'code', codeReview: { strategy: 'sagittarius-3.0', onDemand: true, stage: 'code', counts: { blocker: 1, major: 0, minor: 0 } } });
      // Its last round's findings go to the task as a fix prompt: no PR prompt carries them.
      const fix = await waitFor(() => prompts(s.claudeSessionId).at(-1)!, (p) => p.includes('round 1'), 10000, 'the fix prompt');
      expect(fix).toContain("Kode reviewed this change locally (Sagittarius 3.0, round 1) at the user's request.");
      expect(fix).toContain("Don't open the PR yet: when it's opened, it goes with this review's result.");
      expect(fix).toContain('[1:1] `src/a.ts:2` **blocker**: Off by one.');
      await waitFor(() => env.core.sessions.get(s.id).status, (st) => st === 'idle', 10000, 'the fix turn');
      expect(prompts(s.claudeSessionId).some((p) => p.includes('Open a pull request'))).toBe(false);
      // Done and waiting to go out with the PR, the review Off meanwhile: Open PR reviews nothing, and
      // Code Review isn't skipped.
      const waiting = env.core.sessions.get(s.id);
      expect(waiting.openPrReview).toBeNull();
      expect(waiting.skippedStages).not.toContain('reviewing-code');

      // The stage moves to Pull Request before the PR prompt is typed (Lifecycle.sendPr): hold the
      // typing, and Code Review isn't skipped meanwhile either.
      let release!: () => void;
      const held = new Promise<void>((r) => { release = r; });
      const submit = env.core.host.submit.bind(env.core.host);
      vi.spyOn(env.core.host, 'submit').mockImplementation(async (name, text) => {
        if (text.includes('Open a pull request')) await held;
        return submit(name, text);
      });
      // Open PR answers once the prompt is typed: not awaited while the typing is held.
      const opened = env.core.sessions.action(s.id, 'open-pr');
      await waitFor(() => env.core.sessions.get(s.id), (x) => x.stage === 'pr', 10000, 'the PR prompt');
      expect(env.core.sessions.get(s.id).skippedStages).not.toContain('reviewing-code');
      release();
      await opened;
      await waitFor(() => all<{ f: number | null }>(env.core.db, 'SELECT finished_at AS f FROM code_reviews WHERE task_id = ?', s.id)[0].f, (f) => f !== null, 10000, "the review's finish");
      vi.mocked(env.core.host.submit).mockRestore();
      expect(env.core.sessions.get(s.id).skippedStages).not.toContain('reviewing-code');
      const pr = await prPrompt(s.claudeSessionId);
      expect(pr).toContain('me_kode_review:1 blocker addressed;Sagittarius3.0');
      expect(pr).not.toContain('Off by one.');
      expect(reviews(s.id)).toHaveLength(1);
    });

    it("queues its fix prompt while the task can't take it, and Open PR sends findings never taken with the PR prompt", async () => {
      const s = await task({ review: { strategy: 'sirius-1.0' } });
      fake('review-reports.json', { all: { summary: 'LGTM', findings: [{ file: 'src/a.ts', line: 2, severity: 'blocker', comment: 'Off by one.', gotchas: [] }] } });
      await env.core.sessions.action(s.id, 'review-run');
      await described(s, 1);
      // Kode waits on the user in the terminal: the fix prompt isn't typed into it.
      run(env.core.db, `UPDATE sessions SET status = 'needs_input' WHERE id = ?`, s.id);
      await waitFor(() => env.core.sessions.get(s.id).codeReview!.state, (st) => st === 'done', 30000, 'the review to finish');
      expect(env.core.lifecycle.codeReviewPending(s.id)).toBe(true);
      expect(prompts(s.claudeSessionId).some((p) => p.includes('Off by one.'))).toBe(false);
      await expect(env.core.sessions.action(s.id, 'review-run')).rejects.toThrow(/hasn't taken the last code review's fixes/);
      run(env.core.db, `UPDATE sessions SET status = 'idle' WHERE id = ?`, s.id);
      await env.core.sessions.action(s.id, 'open-pr');
      const pr = await prPrompt(s.claudeSessionId);
      expect(pr).toContain('[1:1] `src/a.ts:2` **blocker**: Off by one.');
      expect(pr).toContain('me_kode_review:1 blocker addressed;Sirius1.0');
      expect(env.core.lifecycle.codeReviewPending(s.id)).toBe(false);
    });

    it('is taken over by a move to Pull Request while it runs, with the strategy Off, and the PR goes when it is done', async () => {
      const s = await task();
      env.core.projects.update(s.projectId, { settings: { codeReview: { strategy: 'off' } } });
      await env.core.sessions.action(s.id, 'review-run');
      await expect(env.core.sessions.action(s.id, 'review-run')).rejects.toThrow(/already running/);
      await described(s, 1);
      await env.core.sessions.move(s.id, { stage: 'review' });
      expect(env.core.sessions.get(s.id)).toMatchObject({ stage: 'code', codeReview: { onDemand: false } });
      await waitFor(() => env.core.sessions.get(s.id), (x) => x.stage === 'pr', 30000, 'the PR prompt after the review');
      expect(await prPrompt(s.claudeSessionId)).toContain('me_kode_review:LGTM;Sagittarius3.0');
      expect(reviews(s.id)).toEqual([expect.objectContaining({ purpose: 'pr', stage: 'code' })]);
    });

    it('is taken over by an Open PR queued during its fix turn', async () => {
      const s = await task({ review: { strategy: 'sirius-1.0', rounds: 1 } });
      fake('review-reports.json', { all: { summary: 'LGTM', findings: [{ file: 'src/a.ts', line: 2, severity: 'blocker', comment: 'Off by one.', gotchas: [] }] } });
      writeFileSync(join(env.fakeDir, 'turn-ms'), '1500');
      await env.core.sessions.action(s.id, 'review-run');
      await waitFor(() => env.core.sessions.get(s.id), (x) => x.codeReview?.state === 'fixing' && x.status === 'working', 30000, 'the fix turn');
      await env.core.sessions.action(s.id, 'open-pr');
      expect(env.core.sessions.get(s.id).queued).toBe('open-pr');
      rmSync(join(env.fakeDir, 'turn-ms'), { force: true });
      fake('review-reports.json', {});
      await waitFor(() => env.core.sessions.get(s.id), (x) => x.stage === 'pr', 30000, 'the PR prompt after round 2');
      expect(await prPrompt(s.claudeSessionId)).toContain('me_kode_review:1 blocker addressed;Sirius1.0');
      expect(reviews(s.id)).toEqual([expect.objectContaining({ purpose: 'pr' })]);
    });

    it("doesn't hold Open PR once cancelled, and goes stale once the task leaves Code", async () => {
      const s = await task({ review: { strategy: 'sirius-1.0' } });
      await expect(env.core.sessions.action(s.id, 'review-cancel')).rejects.toThrow(/No code review is running/);
      await env.core.sessions.action(s.id, 'review-run');
      await env.core.sessions.action(s.id, 'review-cancel');
      expect(env.core.sessions.get(s.id).codeReview).toMatchObject({ state: 'cancelled', onDemand: true });
      await expect(env.core.sessions.action(s.id, 'review-open-anyway')).rejects.toThrow(/no stopped code review/);
      // Run again, to the end: leaving Code makes it stale, so a move back to Pull Request reviews again.
      await described(s, 1);
      await env.core.sessions.action(s.id, 'review-run');
      await waitFor(() => env.core.sessions.get(s.id), (x) => x.codeReview?.state === 'done' && x.status === 'idle', 30000, 'the review to finish');
      await env.core.sessions.move(s.id, { stage: 'design', prompt: false });
      await expect(env.core.sessions.action(s.id, 'review-run')).rejects.toThrow(/runs in Code, or in Pull Request/);
      await env.core.sessions.move(s.id, { stage: 'review' });
      await waitFor(() => env.core.sessions.get(s.id), (x) => x.stage === 'pr', 30000, 'the PR prompt after a new review');
      expect(reviews(s.id).map((r) => r.purpose)).toEqual(['demand', 'demand', 'pr']);
    });

    it('runs in Pull Request on the PR, queues its fixes, pushes once after the last round, puts its line in the PR, and holds the merge meanwhile', async () => {
      const s = await task({ review: { rounds: 1 } });
      gh.body = 'Adds f.';
      run(env.core.db, `UPDATE sessions SET stage = 'review', pr_number = 7, pr_url = 'https://github.com/acme/demo-repo/pull/7' WHERE id = ?`, s.id);
      fake('review-reports.json', { ...LEDGERS, diff: { summary: 'LGTM', findings: [{ file: 'src/a.ts', line: 2, severity: 'blocker', comment: 'Off by one.', gotchas: [] }] } });
      await env.core.sessions.action(s.id, 'review-run');
      const st = await (await import('../../src/server/github.js')).prStatus('', 7);
      const info = env.core.lifecycle.mergeInfo(env.core.sessions.row(s.id), st, { uncommitted: 1 });
      expect(info).toMatchObject({ canMerge: false, queueable: true, hard: [] });
      expect(info.blockers).toContain('The code review is still running.');
      // With Claude gone, nothing clears it by itself: a queued merge is dropped, with a notice.
      const exited = env.core.lifecycle.mergeInfo({ ...env.core.sessions.row(s.id), status: 'exited' }, st, { uncommitted: 1 });
      expect(exited.hard).toEqual(expect.arrayContaining(['The code review is still running.', 'The worktree has 1 uncommitted file: commit or discard them.']));
      // No PR description turn: round 1 reads the PR's own title and description.
      const fix = await waitFor(() => prompts(s.claudeSessionId).at(-1)!, (p) => p.includes('round 1'), 30000, 'the fix prompt');
      expect(fix).toContain("Don't commit or push yet: Kode reviews the change again first.");
      const id = env.core.sessions.get(s.id).codeReview!.id;
      expect(readFileSync(join(env.core.paths.run, 'reviews', id, 'r1', 'base.md'), 'utf8')).toContain('> feat(demo): add f\n>\n> Adds f.');
      // Round 2 finds nothing: the fixes, uncommitted, are pushed once.
      fake('review-reports.json', LEDGERS);
      const push = await waitFor(() => prompts(s.claudeSessionId).at(-1)!, (p) => p.includes('found nothing more'), 30000, 'the push prompt');
      expect(push).toContain("push them to the PR's branch");
      const done = await waitFor(() => env.core.sessions.get(s.id), (x) => x.codeReview?.state === 'done', 10000, 'the review to finish');
      expect(done).toMatchObject({ stage: 'review', codeReview: { round: 2, onDemand: true, stage: 'review' } });
      await waitFor(() => gh.body, (b) => b.includes('me_kode_review:1 blocker addressed;Sagittarius3.0'), 10000, "the review's line in the PR");
      expect(prompts(s.claudeSessionId).filter((p) => p.includes("write the PR's title and description"))).toHaveLength(0);
      await waitFor(() => env.core.lifecycle.codeReviewPending(s.id), (p) => !p, 10000, 'the push turn to end');
    });

    it('is cancelled when the task leaves Pull Request', async () => {
      const s = await task({ review: { strategy: 'sirius-1.0' } });
      run(env.core.db, `UPDATE sessions SET stage = 'review', pr_number = 7, pr_url = 'https://github.com/acme/demo-repo/pull/7' WHERE id = ?`, s.id);
      fake('review-skip', ['r1s1:all']);
      await env.core.sessions.action(s.id, 'review-run');
      await env.core.sessions.move(s.id, { stage: 'code', prompt: false });
      expect(env.core.sessions.get(s.id).codeReview).toMatchObject({ state: 'cancelled', detail: REVIEW_LEFT_REVIEW, stage: 'review' });
    });
  });

  describe('findings that wait on you (check)', () => {
    const one = (line: number, severity: string, comment: string) => ({ file: 'src/a.ts', line, severity, comment, gotchas: [] });
    const waiting = (id: string) => waitFor(() => env.core.sessions.get(id), (x) => x.codeReview?.state === 'triage', 30000, 'the findings to wait on you');
    const keyAt = (id: string, line: number) => env.core.reviews.detail(id).findings.find((f) => f.line === line)!.key;

    it("holds a round's findings for your notes and dismissals, through Open PR, then sends the rest with the notes", async () => {
      const s = await task({ review: { rounds: 1, check: true } });
      fake('review-reports.json', { ...LEDGERS, diff: { summary: 'LGTM', findings: [one(2, 'blocker', 'Off by one.'), one(4, 'major', 'Rename it.')] } });
      await env.core.sessions.action(s.id, 'open-pr');
      const held = await waiting(s.id);
      expect(held).toMatchObject({ stage: 'code', codeReview: { round: 1, counts: { blocker: 1, major: 1, minor: 0 } } });
      const asked = prompts(s.claudeSessionId).length;
      // Open PR, or a move to Pull Request, keeps it waiting: only Send moves it on.
      await env.core.sessions.action(s.id, 'open-pr');
      await env.core.sessions.move(s.id, { stage: 'review' });
      expect(env.core.sessions.get(s.id)).toMatchObject({ stage: 'code', codeReview: { state: 'triage' } });
      env.core.reviews.setFinding(s.id, keyAt(s.id, 2), { note: 'Use <= here.' });
      env.core.reviews.setFinding(s.id, keyAt(s.id, 4), { dismissed: true, note: 'The name is fine.' });
      // A field left out keeps its value: a new note leaves the dismissal.
      expect(env.core.reviews.setFinding(s.id, keyAt(s.id, 4), { note: 'The name reads fine.' }).you).toEqual({ dismissed: true, note: 'The name reads fine.' });
      env.core.reviews.setFinding(s.id, keyAt(s.id, 4), { note: 'The name is fine.' });
      expect(() => env.core.reviews.setFinding(s.id, '9:9', { note: 'x' })).toThrow(/no finding 9:9/);
      expect(() => env.core.reviews.setFinding(s.id, keyAt(s.id, 2), { dismissed: 'yes' })).toThrow(/dismissed must be true or false/);
      expect(prompts(s.claudeSessionId)).toHaveLength(asked);

      fake('review-reports.json', LEDGERS);
      await env.core.sessions.action(s.id, 'review-send');
      const fix = await waitFor(() => prompts(s.claudeSessionId).at(-1)!, (p) => p.includes('round 1'), 30000, 'the fix prompt');
      // Through the prompt queue, as the user may have been working with the task meanwhile.
      expect(fix).toContain(`[${keyAt(s.id, 2)}] \`src/a.ts:2\` **blocker**: Off by one.`);
      expect(fix).toContain("- The user's note: Use <= here.");
      expect(fix).not.toContain('Rename it.');
      await expect(env.core.sessions.action(s.id, 'review-send')).rejects.toThrow(/No code review findings wait on you/);
      // Round 2 finds nothing: the PR goes, its line without the dismissed one, and round 2 was told of it.
      const done = await waitFor(() => env.core.sessions.get(s.id), (x) => x.stage === 'pr', 30000, 'the PR prompt after round 2');
      expect(done.codeReview).toMatchObject({ round: 2, state: 'done', counts: { blocker: 1, major: 0, minor: 0 } });
      expect(await prPrompt(s.claudeSessionId)).toContain('me_kode_review:1 blocker addressed;Sagittarius3.0');
      const round2 = readFileSync(join(env.core.paths.run, 'reviews', done.codeReview!.id, 'r2', 'base.md'), 'utf8');
      expect(round2).toContain('  > Rename it.\n  - the user dismissed it:\n    > The name is fine.');
      expect(env.core.reviews.detail(s.id).findings.find((f) => f.line === 4)).toMatchObject({ kept: false, dropped: 'dismissed by you' });
    });

    it('waits again with the round counted once after a Retry of a review cancelled while it waited', async () => {
      const s = await task({ review: { check: true } });
      fake('review-reports.json', { ...LEDGERS, diff: { summary: 'LGTM', findings: [one(2, 'blocker', 'Off by one.')] } });
      await env.core.sessions.action(s.id, 'open-pr');
      await waiting(s.id);
      await env.core.sessions.action(s.id, 'review-cancel');
      await env.core.sessions.action(s.id, 'review-retry');
      const again = await waiting(s.id);
      expect(again.codeReview!.counts).toEqual({ blocker: 1, major: 0, minor: 0 });
      expect(env.core.reviews.detail(s.id).findings.filter((f) => f.kept)).toHaveLength(1);
      await env.core.sessions.action(s.id, 'review-send');
      await waitFor(() => env.core.sessions.get(s.id), (x) => x.stage === 'pr', 30000, 'the PR prompt');
      expect(await prPrompt(s.claudeSessionId)).toContain('me_kode_review:1 blocker addressed;Sagittarius3.0');
    });

    it.each([['you dismiss every finding and send', 'dismiss'], ['Run to merge is turned on', 'auto']] as const)(
      "sends the last round's PR prompt once %s",
      async (_how, how) => {
        const s = await task({ review: { check: true } });
        fake('review-reports.json', { ...LEDGERS, diff: { summary: 'LGTM', findings: [one(2, 'blocker', 'Off by one.')] } });
        await env.core.sessions.action(s.id, 'open-pr');
        await waiting(s.id);
        if (how === 'dismiss') {
          env.core.reviews.setFinding(s.id, keyAt(s.id, 2), { dismissed: true });
          await env.core.sessions.action(s.id, 'review-send');
        } else await env.core.sessions.edit(s.id, { autoRun: true });
        const done = await waitFor(() => env.core.sessions.get(s.id), (x) => x.stage === 'pr', 30000, 'the PR prompt');
        const pr = await prPrompt(s.claudeSessionId);
        if (how === 'dismiss') {
          expect(pr).not.toContain('Off by one.');
          expect(pr).toContain('me_kode_review:LGTM;Sagittarius3.0');
          expect(done.codeReview!.counts).toEqual({ blocker: 0, major: 0, minor: 0 });
        } else {
          expect(pr).toContain('`src/a.ts:2` **blocker**: Off by one.');
          expect(pr).toContain('me_kode_review:1 blocker addressed;Sagittarius3.0');
        }
      },
    );

    it("doesn't wait while Run to merge is on", async () => {
      const s = await task({ review: { strategy: 'sirius-1.0', check: true } });
      fake('review-reports.json', { all: { summary: 'LGTM', findings: [one(2, 'blocker', 'Off by one.')] } });
      await env.core.sessions.edit(s.id, { autoRun: true });
      await waitFor(() => env.core.sessions.get(s.id), (x) => x.stage === 'pr', 30000, 'the PR prompt');
      expect(await prPrompt(s.claudeSessionId)).toContain('Off by one.');
      expect(all(env.core.db, `SELECT 1 FROM session_events WHERE session_id = ? AND text LIKE '%wait on you%'`, s.id)).toEqual([]);
    });

    it('blocks the merge in Pull Request as yours until you send them, and the PR poll sending them starts ends before the server stops', async () => {
      const s = await task({ review: { strategy: 'sirius-1.0', check: true } });
      run(env.core.db, `UPDATE sessions SET stage = 'review', pr_number = 7, pr_url = 'https://github.com/acme/demo-repo/pull/7' WHERE id = ?`, s.id);
      fake('review-reports.json', { all: { summary: 'LGTM', findings: [one(2, 'blocker', 'Off by one.')] } });
      await env.core.sessions.action(s.id, 'review-run');
      await waiting(s.id);
      const st = await (await import('../../src/server/github.js')).prStatus('', 7);
      const info = env.core.lifecycle.mergeInfo(env.core.sessions.row(s.id), st);
      const waits = "The code review's findings wait on you: send them to Kode or dismiss them.";
      expect(info).toMatchObject({ canMerge: false, hard: [waits], yours: [waits] });
      expect(info.blockers).not.toContain('The code review is still running.');
      // Sending them polls the PR, which compares the branch with its head (abc123, no commit here)
      // and so fetches the PR from origin: the repo has none, but even a failed fetch writes
      // FETCH_HEAD. A slow GitHub keeps that poll going past the fix prompt.
      await env.core.lifecycle.settled();
      const fetchHead = join(env.repo, '.git', 'FETCH_HEAD');
      rmSync(fetchHead, { force: true });
      gh.statusMs = 1000;
      await env.core.sessions.action(s.id, 'review-send');
      const fix = await waitFor(() => prompts(s.claudeSessionId).at(-1)!, (p) => p.includes('round 1'), 30000, 'the fix prompt');
      expect(fix).toContain('Off by one.');
      expect(env.core.lifecycle.mergeInfo(env.core.sessions.row(s.id), st).yours).toEqual([]);
      // The server waits for that poll as it stops, so its fetch never writes into a removed repo.
      await env.core.stop();
      expect(existsSync(fetchHead)).toBe(true);
    });
  });

  it('skips the review when the project turns it off, and says so: Open PR, and Code Review skipped', async () => {
    const s = await task();
    expect(env.core.sessions.get(s.id)).toMatchObject({ openPrReview: 'new', skippedStages: ['explore', 'design'] });
    env.core.projects.update(s.projectId, { settings: { codeReview: { strategy: 'off' } } });
    expect(env.core.sessions.get(s.id)).toMatchObject({ openPrReview: 'off', skippedStages: ['explore', 'design', 'reviewing-code'] });
    // The session's own settings go over the project's.
    await env.core.sessions.edit(s.id, { review: { strategy: 'sirius-1.0' } });
    expect(env.core.sessions.get(s.id)).toMatchObject({ openPrReview: 'new', skippedStages: ['explore', 'design'] });
    await env.core.sessions.edit(s.id, { review: {} });
    await env.core.sessions.action(s.id, 'open-pr');
    expect(env.core.sessions.get(s.id)).toMatchObject({ stage: 'pr', codeReview: null, openPrReview: null, skippedStages: ['explore', 'design', 'reviewing-code'] });
  });

  it("skips Code Review for a PR prompt that goes without a review, though an earlier one went with one", async () => {
    const s = await task({ review: { strategy: 'sirius-1.0' } });
    await env.core.sessions.action(s.id, 'open-pr');
    await waitFor(() => env.core.sessions.get(s.id), (x) => x.stage === 'pr', 30000, 'the PR prompt after the review');
    await prPrompt(s.claudeSessionId);
    expect(env.core.sessions.get(s.id).skippedStages).toEqual(['explore', 'design']);
    await waitFor(() => env.core.sessions.get(s.id), (x) => x.status === 'idle' && !x.busy, 10000, 'the PR turn');
    // Back to Code, with the review Off now: that review was the earlier PR prompt's.
    await env.core.sessions.move(s.id, { stage: 'code' });
    await env.core.sessions.edit(s.id, { review: { strategy: 'off' } });
    expect(env.core.sessions.get(s.id)).toMatchObject({ stage: 'code', openPrReview: 'off', skippedStages: ['explore', 'design', 'reviewing-code'] });
    await waitFor(() => env.core.sessions.get(s.id), (x) => x.status === 'idle' && !x.busy, 10000, 'the move');
    await env.core.sessions.action(s.id, 'open-pr');
    expect(env.core.sessions.get(s.id)).toMatchObject({ stage: 'pr', skippedStages: ['explore', 'design', 'reviewing-code'] });
    // An older review left done and unsent behind a newer one (cancelled here) isn't this PR's: only
    // the latest unsent one is a PR prompt still being typed.
    run(env.core.db, 'UPDATE code_reviews SET finished_at = NULL WHERE task_id = ?', s.id);
    run(env.core.db, `INSERT INTO code_reviews (id, task_id, strategy, rounds, depth, state, created_at, updated_at) VALUES ('newer', ?, 'sirius-1.0', 1, 1, 'cancelled', ?, ?)`, s.id, Date.now() + 1000, Date.now() + 1000);
    expect(env.core.sessions.get(s.id).skippedStages).toEqual(['explore', 'design', 'reviewing-code']);
    // At Done too, while the session runs (set straight there, so Merge & Close was skipped as well);
    // an archived one marks no step skipped.
    run(env.core.db, `UPDATE sessions SET stage = 'done' WHERE id = ?`, s.id);
    expect(env.core.sessions.get(s.id).skippedStages).toEqual(['explore', 'design', 'merge', 'reviewing-code']);
    run(env.core.db, `UPDATE sessions SET desired = 'archived' WHERE id = ?`, s.id);
    expect(env.core.sessions.get(s.id).skippedStages).toEqual([]);
  });

  it('says the review waits on you while its session asks in its terminal, and only while its steps run', async () => {
    const s = await task({ review: { strategy: 'sirius-1.0' } });
    fake('review-skip', ['r1s1:all']);
    await env.core.sessions.action(s.id, 'open-pr');
    const r = await waitFor(() => env.core.sessions.get(s.id), (x) => x.codeReview?.state === 'failed', 30000, 'the review to fail');
    const childId = r.codeReview!.sessionId!;
    run(env.core.db, `UPDATE sessions SET status = 'needs_input' WHERE id = ?`, childId);
    expect(env.core.sessions.get(s.id).codeReview!.waiting).toBe(false);
    run(env.core.db, `UPDATE code_reviews SET state = 'fixing' WHERE id = ?`, r.codeReview!.id);
    expect(env.core.sessions.get(s.id).codeReview!.waiting).toBe(false);
    run(env.core.db, `UPDATE code_reviews SET state = 'sweeping' WHERE id = ?`, r.codeReview!.id);
    expect(env.core.sessions.get(s.id).codeReview!.waiting).toBe(true);
    run(env.core.db, `UPDATE sessions SET status = 'working' WHERE id = ?`, childId);
    expect(env.core.sessions.get(s.id).codeReview!.waiting).toBe(false);
  });

  it('archives the review session with its task, and leaves the task\'s worktree and branch alone', async () => {
    const s = await task({ review: { strategy: 'sirius-1.0' } });
    fake('review-skip', ['r1s1:all']);
    await env.core.sessions.action(s.id, 'open-pr');
    const r = await waitFor(() => env.core.sessions.get(s.id), (x) => x.codeReview?.state === 'failed', 30000, 'the review to fail');
    const childId = all<{ id: string }>(env.core.db, 'SELECT id FROM sessions WHERE parent_id = ?', s.id)[0].id;
    // Archiving the review session alone keeps the task's worktree.
    await env.core.sessions.archive(env.core.sessions.row(childId), { keepBranch: false });
    expect(env.core.sessions.row(s.id).worktree_released).toBe(0);
    expect(r.codeReview!.sessionId).toBe(childId);
    expect(env.core.sessions.get(s.id).codeReview!.sessionId).toBeNull();
  });
});
