import { execFileSync } from 'node:child_process';
import { existsSync, readFileSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { all, get, kvGet, run } from '../../src/server/db.js';
import { NO_PR_IN_WORKSHOP } from '../../src/server/lifecycle.js';
import { buildServer } from '../../src/server/server.js';
import { parseMekoMd } from '../../src/shared/mekomd.js';
import { AGENTK_TOUR, type TourActionId, type TourState } from '../../src/shared/types.js';
import { fakeMekoQuestions, makeEnv, noMeko, readJsonl, useLocalMeko, waitFor, type ApiCall, type TestEnv } from '../helpers.js';

const gitIn = (dir: string, ...args: string[]) => execFileSync('git', args, { cwd: dir, encoding: 'utf8' }).trim();

describe('the Kode tour (a bundled workshop Kode runs itself; real session host + git, fake claude)', () => {
  let env: TestEnv;
  beforeEach(async () => {
    env = await makeEnv();
  });
  afterEach(async () => {
    await env.cleanup();
  });

  /** The prompts a session's fake Claude got after its first one, oldest first. */
  const prompts = (claudeSessionId: string) =>
    readJsonl<{ type: string; text: string }>(join(env.fakeDir, `${claudeSessionId}.jsonl`)).filter((e) => e.type === 'user').map((e) => e.text);
  /** The prompt a session's fake Claude was launched with (the last argument), found by its --session-id. */
  const firstPrompt = (sessionId: string) => {
    const conv = env.core.sessions.get(sessionId).claudeSessionId;
    return readJsonl<{ args: string[] }>(join(env.fakeDir, 'argv.jsonl')).find((a) => a.args.includes(conv))?.args.at(-1) ?? '';
  };
  const fake = (file: string, v: unknown) => writeFileSync(join(env.fakeDir, file), JSON.stringify(v));
  /** Meko's datapack ids are UUIDs, which MEKO.md's rows need: the start makes the gotchas one, then the project's; each one made again gets the next. */
  const GOTCHAS_ID = '0f6b1c1e-0000-4000-8000-000000000001';
  const DEDICATED_ID = '0f6b1c1e-0000-4000-8000-000000000002';
  const uuids = () => {
    let made = 0;
    env.meko.createDatapack = (async (name: string) => ({ id: `0f6b1c1e-0000-4000-8000-${String(++made).padStart(12, '0')}`, name })) as typeof env.meko.createDatapack;
  };
  const deleted = () => env.meko.calls.filter((c) => c.name === 'deleteDatapack').map((c) => c.args[0]);
  /** The datapack MEKO.md on <ref> lists under a name ending in <suffix>. */
  const listed = (repo: string, ref: string, suffix: string) => parseMekoMd(gitIn(repo, 'show', `${ref}:MEKO.md`)).find((d) => d.name.endsWith(suffix))?.id;
  /** Meko already has the gotchas' bytes: the upload is indexed at once. */
  const gotchasKnown = () => env.mekoApi.handle((c: ApiCall) => c.path.endsWith('/upload-url')
    ? { body: { data: { url: 'https://storage.test/kb', fields: {}, key: 'dk-gotchas', already_uploaded: true } } }
    : undefined);
  const tourOf = (projectId: string) => (): TourState => env.core.tours.state(projectId)!;
  const act = (stage: string, action: TourActionId, level?: string) => env.core.tours.act(AGENTK_TOUR, { stage, action, ...(level ? { level } : {}) });
  const ids = (t: TourState) => t.actions.map((a) => a.id);
  const reviews = (taskId: string) => all<{ by: string; purpose: string; strategy: string; hand_off: number; state: string }>(env.core.db,
    'SELECT by, purpose, strategy, hand_off, state FROM code_reviews WHERE task_id = ? ORDER BY created_at, rowid', taskId);
  /** The review with the gotchas finds the trap, crediting MG-2, on the fix's line. */
  const MG2_FINDING = {
    all: { summary: 'The reset is in the wrong place.', findings: [{ file: 'src/game.js', line: 40, severity: 'blocker', comment: "Zeroes the daily challenge's running total.", gotchas: [{ id: 'MG-2', quote: 'newGame() must keep state.score' }] }] },
  };
  /** Level 1 from Start the tour to the review with context, done; the task. */
  const toReviewWithContext = async (tour: () => TourState) => {
    await act('repo', 'begin');
    await act('repo', 'next');
    const out = await act('review-cold', 'run');
    await waitFor(tour, (t) => t.phase === 'done', 30000, 'the first look');
    await act('review-cold', 'next');
    await act('teach', 'run');
    await waitFor(tour, (t) => t.phase === 'done', 10000, 'the gotchas indexed');
    await act('teach', 'next');
    await act('review-context', 'run');
    await waitFor(tour, (t) => t.phase === 'done', 30000, 'the review with the gotchas');
    return env.core.sessions.get(out.sessionId!);
  };

  it('builds the demo with no remote and opens it as the workshop\'s project, its datapacks in MEKO.md, on the welcome card; no session is begun', async () => {
    uuids();
    gotchasKnown();
    const r = await env.core.workshops.start(AGENTK_TOUR);
    const p = env.core.projects.get(r.projectId);
    expect(p).toMatchObject({ name: 'Kode tour', workshop: AGENTK_TOUR, repoPath: join(env.home, 'code', 'kode-demo') });
    const repo = p.repoPath;
    expect(gitIn(repo, 'remote')).toBe('');
    // Every install gets the same history: made-up authors, not the user's identity.
    expect(gitIn(repo, 'log', '--format=%an <%ae>', 'main')).toBe('Kode Demo <demo@example.com>');
    expect(gitIn(repo, 'log', '--format=%an <%ae>', '-1', 'demo/score-fix')).toBe('Alice <alice@example.com>');
    // The review finds the gotchas datapack through MEKO.md (on both branches and without the gotchas: the unit test's).
    expect(parseMekoMd(readFileSync(join(repo, 'MEKO.md'), 'utf8'))).toEqual([
      { name: 'adlc/repo/kode-demo/gotchas', id: GOTCHAS_ID, about: expect.any(String) },
      { name: p.datapacks[0].name, id: DEDICATED_ID, about: expect.any(String) },
    ]);
    // The gotchas datapack isn't the project's: only the review reads it.
    expect(p.datapacks.map((d) => d.role)).toEqual(['dedicated']);
    // It starts empty: Level 1's first look reviews before Teach it adds the gotchas.
    expect(env.mekoApi.calls.some((c) => c.path.endsWith('/upload-url'))).toBe(false);
    // No host: the tour is Kode's own, on its welcome card, in the state the page gets.
    expect(env.core.sessions.listForProject(p.id)).toEqual([]);
    const welcome = env.core.state().projects.find((x) => x.id === p.id)?.tour;
    expect(welcome).toMatchObject({ workshop: AGENTK_TOUR, stage: 'repo', welcome: true, title: 'Welcome to Kode and Meko', score: null, sessionId: null, finished: false });
    expect(welcome?.text).toContain('agentic software development lifecycle');
    expect(welcome?.actions).toEqual([{ id: 'begin', label: 'Start the tour', primary: true }]);
    await expect(act('repo', 'next')).rejects.toMatchObject({ status: 409 });

    // Start the tour: Level 1's first stage, with what the level teaches over it, and Play the game first.
    await act('repo', 'begin');
    const tour = tourOf(p.id);
    expect(tour()).toMatchObject({ level: '1-review', stage: 'repo', kind: 'repo', phase: 'done', welcome: false });
    expect(tour().lead).toContain("An AI code review only knows what's in the code");
    expect(tour().levels.map((l) => [l.id, l.stages.map((s) => s.status)])).toEqual([
      ['1-review', ['current', 'todo', 'todo', 'todo']],
      ['2-learn', ['todo', 'todo', 'todo']],
    ]);
    expect(ids(tour())).toEqual(['next']);
    expect(tour().links).toEqual([{ label: 'Play the game', href: `api/workshops/${AGENTK_TOUR}/play/main/index.html`, kind: 'play', first: true }]);
    expect(tour().score).toEqual({ cold: null, context: null, difference: null });
    expect(tour().text).toContain('New game');
    await expect(act('repo', 'begin')).rejects.toMatchObject({ status: 409 });
    // Other projects have none.
    const other = await env.core.projects.create({ repoPath: env.repo, name: 'Other', datapack: { mode: 'none' } });
    expect(env.core.state().projects.find((x) => x.id === other.id)).not.toHaveProperty('tour');

    // Starting it again goes to the same project.
    expect(await env.core.workshops.start(AGENTK_TOUR)).toEqual(r);
    expect(env.core.workshops.list()).toEqual([expect.objectContaining({ id: AGENTK_TOUR, projectId: p.id })]);
  });

  it('runs Level 1 from its buttons: the gotchas datapack made again, the look without context, the gotchas, the review with them and the level finished; no PR', async () => {
    uuids();
    gotchasKnown();
    // As on an install: a round's findings wait for the user (tests turn that off).
    env.core.updateConfig((c) => {
      c.codeReview.check = true;
    });
    const r = await env.core.workshops.start(AGENTK_TOUR);
    const repo = env.core.projects.get(r.projectId).repoPath;
    const tour = tourOf(r.projectId);
    // A button pressed on a stage the tour has left is refused, and so is one the card doesn't offer.
    await expect(act('review-cold', 'run')).rejects.toMatchObject({ status: 409 });
    await act('repo', 'begin');
    await expect(act('repo', 'finish')).rejects.toMatchObject({ status: 409 });
    expect(await act('repo', 'next')).toEqual({ sessionId: null });
    expect(tour()).toMatchObject({ stage: 'review-cold', phase: 'ready', lead: null });
    expect(ids(tour())).toEqual(['run']);

    // Review without context: Kode drops the gotchas datapack and makes it again, empty, with MEKO.md on
    // main and the teammate's branch naming the new one (the branch's own diff stays the fix); then it
    // begins the level's task on that branch, whose first look runs by itself.
    const out = await act('review-cold', 'run');
    expect(deleted()).toEqual([GOTCHAS_ID]);
    const gotchas = listed(repo, 'main', '/gotchas')!;
    expect(gotchas).not.toBe(GOTCHAS_ID);
    expect(listed(repo, 'demo/score-fix', '/gotchas')).toBe(gotchas);
    expect(gitIn(repo, 'diff', '--name-only', 'main...demo/score-fix')).not.toContain('MEKO.md');
    const task = env.core.sessions.get(out.sessionId!);
    expect(task).toMatchObject({ name: 'Review the score fix', stage: 'code', branch: 'demo/score-fix', purpose: null });
    expect(await waitFor(() => firstPrompt(task.id), (t) => !!t, 10000, "the task's launch")).toContain("Kode's code review is checking this change right now");
    await waitFor(tour, (t) => t.phase === 'done', 30000, 'the first look');
    expect(reviews(task.id)).toEqual([{ by: 'auto', purpose: 'demand', strategy: 'sirius-1.0', hand_off: 0, state: 'done' }]);
    // The review session starts on the workshop's model for its agent; the task keeps its own.
    expect(get<{ model: string | null }>(env.core.db, 'SELECT s.model FROM code_reviews c JOIN sessions s ON s.id = c.session_id WHERE c.task_id = ?', task.id)?.model).toBe('sonnet');
    expect(env.core.sessions.row(task.id).model).toBeNull();
    expect(tour().score).toMatchObject({ cold: { kept: 0, caught: 0, gotchas: [] }, context: null, difference: null });
    expect(tour().text).toContain('The review found 0 issues');
    expect(tour().sessionId).toBe(task.id);
    expect(env.mekoApi.calls.some((c) => c.path.endsWith('/upload-url'))).toBe(false);

    // Mine and store context in Meko: the card lists the gotchas, which go into Meko, with a receipt naming them.
    expect((await act('review-cold', 'next')).sessionId).toBe(task.id);
    expect(tour()).toMatchObject({ stage: 'teach', phase: 'ready' });
    expect(tour().text).toContain('- **MG-2:** The daily challenge carries the score through `newGame()`');
    expect(ids(tour())).toEqual(['run']);
    await act('teach', 'run');
    await waitFor(tour, (t) => t.phase === 'done', 10000, 'the gotchas indexed');
    expect(tour().text).toContain('MG-1, MG-2 and MG-3');
    expect(env.mekoApi.calls.find((c) => c.path.endsWith('/upload-url'))?.path).toBe(`/datapacks/${gotchas}/knowledge-bases/upload-url`);

    // Review with context: the review proper, which this time credits MG-2; its findings wait on the user.
    fake('review-reports.json', MG2_FINDING);
    await act('teach', 'next');
    expect(tour()).toMatchObject({ stage: 'review-context', phase: 'ready' });
    await act('review-context', 'run');
    await waitFor(tour, (t) => t.phase === 'done', 30000, 'the review with the gotchas');
    expect(reviews(task.id)[1]).toMatchObject({ by: 'auto', purpose: 'demand', strategy: 'sirius-1.0', hand_off: 1, state: 'triage' });
    expect(tour().score).toMatchObject({ cold: { kept: 0, caught: 0 }, context: { kept: 1, caught: 1, gotchas: ['MG-2'] } });
    expect(tour().score?.difference).toMatch(/^Caught with Meko: MG-2/);
    expect(tour().text).toContain("It caught the trap from the team's gotcha (MG-2)");
    expect(tour().actions).toEqual([{ id: 'finish', label: 'Move on to Level 2', primary: true }]);
    // /go has nothing left to do.
    await expect(env.core.workshops.go(AGENTK_TOUR)).rejects.toMatchObject({ status: 409 });
    expect(env.core.projects.get(r.projectId).settings.codeReview).toEqual({ strategy: 'sirius-1.0' });
    // Neither review asked the task for a PR description: the demo opens no PR.
    expect(prompts(task.claudeSessionId).some((t) => t.includes("write the PR's title and description"))).toBe(false);

    // The demo has no GitHub repo: Open PR, Run to merge and a move to Pull Request are refused.
    const row = env.core.sessions.row(task.id);
    await expect(env.core.lifecycle.openPr(row)).rejects.toMatchObject({ status: 409, message: NO_PR_IN_WORKSHOP });
    await expect(env.core.lifecycle.setAutoRun(row, true)).rejects.toMatchObject({ status: 409 });
    await expect(env.core.lifecycle.move(row, { stage: 'review' })).rejects.toMatchObject({ status: 409 });

    // While the level's task is open on the branch, making the datapack again is refused before
    // anything is dropped.
    const dropped = deleted().length;
    await expect(env.core.workshops.freshGotchas(r.projectId)).rejects.toMatchObject({ status: 409 });
    expect(deleted()).toHaveLength(dropped);

    // Fixes left uncommitted (as after Send to Kode): Archive keeps that worktree, on the branch.
    const wt = row.worktree_path;
    writeFileSync(join(wt, 'README.md'), 'a fix in progress\n');

    // Move on to Level 2 archives the level's sessions (done before it answers) and finishes the level.
    await act('review-context', 'finish');
    expect(env.core.sessions.row(task.id).desired).toBe('archived');
    expect(gitIn(wt, 'rev-parse', '--abbrev-ref', 'HEAD')).toBe('demo/score-fix');
    expect(tour()).toMatchObject({ level: '2-learn', stage: 'learn', phase: 'ready' });
    expect(tour().lead).toContain('Every new agent session that has to rediscover a project burns tokens');
    expect(tour().levels[0]).toMatchObject({ finished: true });
    expect(tour().levels[0].stages.every((s) => s.status === 'done')).toBe(true);
    // The scoreboard is Level 1's: Level 2 has none.
    expect(tour().score).toBeNull();
    expect(get(env.core.db, "SELECT COUNT(*) AS n FROM sessions WHERE purpose = 'guide'")).toEqual({ n: 0 });

    // Taking Level 1 again: the gotchas datapack is made again, so its review looks without them first.
    // The kept worktree lets go of the branch, its fix still there, and the branch is made again on
    // the new MEKO.md.
    await act('learn', 'goto', '1-review');
    expect(tour()).toMatchObject({ level: '1-review', stage: 'repo' });
    expect(tour().levels[0].finished).toBe(false);
    await act('repo', 'next');
    const again = await act('review-cold', 'run');
    expect(deleted()).toEqual([GOTCHAS_ID, gotchas]);
    expect(listed(repo, 'demo/score-fix', '/gotchas')).toBe(listed(repo, 'main', '/gotchas'));
    expect(gitIn(wt, 'rev-parse', '--abbrev-ref', 'HEAD')).toBe('HEAD');
    expect(readFileSync(join(wt, 'README.md'), 'utf8')).toBe('a fix in progress\n');
    await waitFor(tour, (t) => t.phase === 'done', 30000, "the replay's first look");
    expect(reviews(again.sessionId!)).toEqual([expect.objectContaining({ hand_off: 0, state: 'done' })]);
    expect(tour().text).toContain('The review found');
    await act('review-cold', 'next');
    expect(tour()).toMatchObject({ stage: 'teach', phase: 'ready' });
  });

  it('a review with context that credits no gotcha says so, and Run the review again runs another', async () => {
    uuids();
    gotchasKnown();
    const r = await env.core.workshops.start(AGENTK_TOUR);
    const tour = tourOf(r.projectId);
    const task = await toReviewWithContext(tour);
    expect(tour().text).toContain('none of its findings credits a gotcha this time');
    expect(tour().actions).toEqual([{ id: 'run', label: 'Run the review again' }, { id: 'finish', label: 'Move on to Level 2', primary: true }]);
    fake('review-reports.json', MG2_FINDING);
    await act('review-context', 'run');
    await waitFor(tour, (t) => t.phase === 'done' && t.text.includes('It caught the trap'), 30000, 'the second review with the gotchas');
    expect(reviews(task.id).map((x) => x.hand_off)).toEqual([0, 1, 1]);
  });

  it('Level 2 from its buttons: Learn the project on a datapack made again and on Sonnet, the question session and its learn step, the question asked again, and the tour\'s end', async () => {
    uuids();
    const gaps = await fakeMekoQuestions();
    try {
      await useLocalMeko(env, gaps.url);
      env.local.meko = { ...env.local.meko!, webUrl: 'http://meko.test/' };
      await env.core.refreshMeko();
      Object.assign(env.meko, { ensureConversation: async (_agent: string, _dp: string, key: string) => `conv-${key}` });
      const r = await env.core.workshops.start(AGENTK_TOUR);
      const repo = env.core.projects.get(r.projectId).repoPath;
      const tour = tourOf(r.projectId);
      // The home's Learn the project opens with the tour's brief, and is offered though Level 1 may have saved a memory there.
      expect(env.core.workshops.list()[0].learn).toMatchObject({ about: expect.stringContaining('Snake'), months: 0 });
      expect((await env.core.projects.home(r.projectId)).learnOffered).toBe(true);

      await act('repo', 'goto', '2-learn');
      expect(tour()).toMatchObject({ level: '2-learn', stage: 'learn', phase: 'ready' });
      const name = env.core.projects.get(r.projectId).datapacks[0].name;
      expect(tour().text).toContain(name);
      // Learn the project drops the project's datapack and makes it again, empty, under its name, which the
      // project keeps as its own and MEKO.md names; it starts with the level's brief, on the tour's model.
      const learnOut = await act('learn', 'run');
      expect(deleted()).toEqual([DEDICATED_ID]);
      const dp = env.core.projects.get(r.projectId).datapacks;
      expect(dp).toEqual([expect.objectContaining({ name, role: 'dedicated' })]);
      expect(dp[0].id).not.toBe(DEDICATED_ID);
      expect(listed(repo, 'main', name)).toBe(dp[0].id);
      expect(env.core.sessions.get(learnOut.sessionId!)).toMatchObject({ purpose: 'learn', stage: 'explore' });
      expect(env.core.sessions.row(learnOut.sessionId!).model).toBe('sonnet');
      expect(tour().phase).toBe('running');
      expect(tour().links).toEqual([{ label: `Open ${name} in Meko`, href: `http://meko.test/datapacks/${dp[0].id}/memories`, kind: 'meko' }]);
      await waitFor(() => env.core.sessions.get(learnOut.sessionId!).status, (st) => st === 'idle', 10000, "the learn session's turn");
      // A learn session closes itself once it's done.
      await env.core.sessions.archive(env.core.sessions.row(learnOut.sessionId!), { keepBranch: true });
      expect(tour().phase).toBe('done');

      // Ask about the game: its first turn ends, and Kode runs the learn step on it by itself, once.
      await act('learn', 'next');
      expect(tour().text).toContain('2. Which of the game\'s rules have no test yet?');
      const askOut = await act('ask', 'run');
      const ask = env.core.sessions.get(askOut.sessionId!);
      expect(ask).toMatchObject({ name: 'Ask about the game', stage: 'explore' });
      expect(await waitFor(() => firstPrompt(ask.id), (t) => !!t, 10000, 'its launch')).toContain('Which of the game\'s rules have no test yet');
      const learns = () => all<{ trigger: string }>(env.core.db, 'SELECT trigger FROM meko_learns WHERE session_id = ?', ask.id);
      await waitFor(learns, (l) => l.length === 1, 20000, 'the learn step');
      expect(learns()).toEqual([{ trigger: 'manual' }]);
      await waitFor(tour, (t) => t.phase === 'done', 30000, 'the learn step done');
      expect(learns()).toHaveLength(1);
      expect(tour().links).toEqual([{ label: "Meko's Context Gaps", href: `http://meko.test/datapacks/${dp[0].id}/gaps/taught`, kind: 'meko' }]);

      // Ask again replaces it: Kode closes "Ask about the game" as it starts, and runs no learn step on it.
      await act('ask', 'next');
      const againOut = await act('ask-again', 'run');
      await waitFor(() => env.core.sessions.row(ask.id).desired, (d) => d === 'archived', 10000, '"Ask about the game" closed');
      await waitFor(tour, (t) => t.phase === 'done', 30000, "the second session's turn");
      expect(all(env.core.db, 'SELECT 1 FROM meko_learns WHERE session_id = ?', againOut.sessionId)).toEqual([]);
      expect(ids(tour())).toEqual(['finish']);

      // A session still saving what it learned stays open at the level's end: Archive would cut the save short.
      const running = env.core.learn.running.bind(env.core.learn);
      const saving = vi.spyOn(env.core.learn, 'running').mockImplementation((id) => id === againOut.sessionId || running(id));
      await act('ask-again', 'finish');
      saving.mockRestore();
      expect(env.core.sessions.row(againOut.sessionId!).desired).toBe('running');
      // Level 1 isn't finished: the tour goes there.
      expect(tour()).toMatchObject({ level: '1-review', stage: 'repo', finished: false });
      expect(tour().levels[1].finished).toBe(true);
    } finally {
      await gaps.close();
    }
  });

  it('ends the tour once every level is finished, with Use Kode on your own repo', async () => {
    uuids();
    const r = await env.core.workshops.start(AGENTK_TOUR);
    const tour = tourOf(r.projectId);
    // Skip this level is offered only where a level can't run; Finish passes it here too.
    run(env.core.db, `INSERT INTO workshop_tours (project_id, level, stage, level_at, skipped_json, finished_json, updated_at)
      VALUES (?, '2-learn', 'ask-again', 0, '[]', '["1-review"]', 0)`, r.projectId);
    const s = await env.core.sessions.create(r.projectId, { name: 'Ask again', prompt: 'Answer again.', start: 'explore' });
    await waitFor(tour, (t) => t.phase === 'done', 20000, 'its turn');
    await act('ask-again', 'finish');
    await waitFor(() => env.core.sessions.row(s.id).desired, (d) => d === 'archived', 10000, 'the level closed');
    expect(tour()).toMatchObject({ finished: true, phase: 'done', sessionId: null });
    expect(tour().text).toContain('Use Kode on your own repo next');
    expect(tour().actions).toEqual([{ id: 'own-repo', label: 'Use Kode on your own repo', primary: true }, { id: 'close', label: 'Close tour', danger: true }]);
    await expect(act(tour().stage, 'next')).rejects.toMatchObject({ status: 409 });
  });

  it("deletes the datapacks it made when the demo can't be built, and opens no project", async () => {
    uuids();
    const blocked = join(env.home, 'not-a-folder');
    writeFileSync(blocked, '');
    env.core.updateConfig((c) => {
      c.repos.cloneInto = blocked;
    });
    await expect(env.core.workshops.start(AGENTK_TOUR)).rejects.toMatchObject({ status: 500 });
    expect(env.meko.calls.filter((c) => c.name === 'deleteDatapack').map((c) => c.args[0]).sort()).toEqual([DEDICATED_ID, GOTCHAS_ID].sort());
    expect(env.core.workshops.list()[0].projectId).toBeNull();
  });

  it("opens without Meko, with no datapacks and no MEKO.md: Run the review says to start the tour again, and Level 2 says why it can't run", async () => {
    await noMeko(env);
    const r = await env.core.workshops.start(AGENTK_TOUR);
    const p = env.core.projects.get(r.projectId);
    expect(p.datapacks).toEqual([]);
    expect(existsSync(join(p.repoPath, 'MEKO.md'))).toBe(false);
    expect(env.meko.calls.filter((c) => c.name === 'createDatapack')).toEqual([]);
    const tour = tourOf(r.projectId);
    expect(tour().levels[1].blocked).toMatch(/datapack of the project's own/);
    await act('repo', 'begin');
    await act('repo', 'next');
    await expect(act('review-cold', 'run')).rejects.toMatchObject({ status: 409, message: expect.stringContaining("Meko wasn't running when it started") });
    expect(env.core.sessions.listForProject(p.id)).toEqual([]);
    // Level 2 can't run: its card says why and offers to skip it.
    await act('review-cold', 'goto', '2-learn');
    expect(tour()).toMatchObject({ level: '2-learn', phase: 'blocked' });
    expect(tour().text).toMatch(/datapack of the project's own/);
    expect(ids(tour())).toEqual(['finish', 'own-repo']);
  });

  it("archives a tour host from before Kode ran tours itself, and the tour goes on from its records", async () => {
    uuids();
    const r = await env.core.workshops.start(AGENTK_TOUR);
    const host = await env.core.sessions.create(r.projectId, { name: 'Kode tour', prompt: 'Host the Kode tour.', start: 'explore' });
    run(env.core.db, "UPDATE sessions SET purpose = 'guide' WHERE id = ?", host.id);
    await waitFor(() => env.core.sessions.get(host.id).status, (s) => s === 'idle', 10000, "the host's turn");
    await env.core.tours.archiveOldHosts();
    expect(env.core.sessions.row(host.id).desired).toBe('archived');
    expect(env.core.tours.state(r.projectId)).toMatchObject({ stage: 'repo' });
  });

  it('serves the demo to play, read-only and sandboxed, with its scripts and stylesheet inline', async () => {
    uuids();
    const r = await env.core.workshops.start(AGENTK_TOUR);
    const page = env.core.tours.playFile(AGENTK_TOUR, 'main', 'index.html');
    expect(page.type).toMatch(/^text\/html/);
    expect(String(page.body)).toContain('var Game');
    expect(String(page.body)).not.toMatch(/<script src=|rel="stylesheet"/);
    // With the workshop's line in a bar on top: the page opens in a tab of its own.
    expect(String(page.body)).toContain("When you're done playing, go back to the Kode tab to continue the tour.");
    expect(env.core.tours.playFile(AGENTK_TOUR, 'main', '').type).toMatch(/^text\/html/);
    expect(String(env.core.tours.playFile(AGENTK_TOUR, 'main', 'src/game.js').body)).toContain('newGame');
    // Nothing outside the demo, no dot-file, no session of another project.
    for (const [where, path] of [['main', '../x'], ['main', '.git/config'], ['main', 'nope.html'], ['main', 'README.md'], ['nope', 'index.html']]) {
      expect(() => env.core.tours.playFile(AGENTK_TOUR, where, path), `${where}/${path}`).toThrow(expect.objectContaining({ status: 404 }));
    }
    const other = await env.core.projects.create({ repoPath: env.repo, name: 'Other', datapack: { mode: 'none' } });
    const elsewhere = await env.core.sessions.create(other.id, { name: 'Elsewhere', prompt: 'hi', start: 'explore' });
    expect(() => env.core.tours.playFile(AGENTK_TOUR, elsewhere.id, 'index.html')).toThrow(expect.objectContaining({ status: 404 }));
    await waitFor(() => env.core.sessions.get(elsewhere.id).status, (s) => s === 'idle', 10000, 'its turn');
    expect(r.projectId).toBeTruthy();

    const app = await buildServer(env.core, { webDir: null, updates: null });
    try {
      const res = await app.inject({ method: 'GET', url: `/api/workshops/${AGENTK_TOUR}/play/main/index.html` });
      expect(res.statusCode).toBe(200);
      expect(res.headers['content-security-policy']).toMatch(/^sandbox allow-scripts;/);
      expect((await app.inject({ method: 'GET', url: `/api/workshops/${AGENTK_TOUR}/play/main/.git/HEAD` })).statusCode).toBe(404);
    } finally {
      await app.close();
    }
  });
});

describe("a fresh install's tour ([welcome] tour)", () => {
  let env: TestEnv;
  beforeEach(async () => {
    env = await makeEnv({ start: false });
  });
  afterEach(async () => {
    await env.cleanup();
  });

  const welcome = () => env.core.updateConfig((c) => {
    c.welcome.tour = AGENTK_TOUR;
  });

  it('starts once at the server\'s start while there are no projects, then the key goes', async () => {
    welcome();
    await env.core.start();
    await waitFor(() => env.core.workshops.list()[0]?.projectId ?? null, (id) => !!id, 15000, 'the tour\'s project');
    await waitFor(() => env.core.config.welcome.tour, (t) => t === '', 5000, 'key removed');
    expect(kvGet(env.core.db, 'tour-started')).toBe(AGENTK_TOUR);
    expect(get(env.core.db, 'SELECT COUNT(*) AS n FROM projects WHERE workshop IS NOT NULL')).toEqual({ n: 1 });
  });

  it('starts nothing when the install already has a project, and drops the key', async () => {
    await env.core.projects.create({ repoPath: env.repo, name: 'Mine', datapack: { mode: 'none' } });
    welcome();
    await env.core.start();
    await waitFor(() => env.core.config.welcome.tour, (t) => t === '', 5000, 'key removed');
    expect(get(env.core.db, 'SELECT COUNT(*) AS n FROM projects WHERE workshop IS NOT NULL')).toEqual({ n: 0 });
  });
});
