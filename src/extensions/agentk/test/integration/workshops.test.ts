import { execFileSync } from 'node:child_process';
import { existsSync, readFileSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { all, get, kvGet } from '../../src/server/db.js';
import { NO_PR_IN_WORKSHOP } from '../../src/server/lifecycle.js';
import { parseMekoMd } from '../../src/shared/mekomd.js';
import { AGENTK_TOUR } from '../../src/shared/types.js';
import { fakeMekoQuestions, makeEnv, noMeko, readJsonl, useLocalMeko, waitFor, type ApiCall, type TestEnv } from '../helpers.js';

const gitIn = (dir: string, ...args: string[]) => execFileSync('git', args, { cwd: dir, encoding: 'utf8' }).trim();

describe('the AgentK tour (a bundled workshop; real session host + git, fake claude)', () => {
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
  /**
   * Like the user typing into the host, then the host's turn: sent while the host is idle, and
   * again until it goes in. AgentK sends the host its events too (GuideEvents): typed during their
   * turn, the fake Claude would run both turns at once, and a prompt that waited for theirs to go
   * in is refused (submit's 409).
   */
  const say = async (hostId: string, text: string) => {
    // The idle check and the submit run in one tick, so no events prompt can go in between them.
    await waitFor(() => env.core.sessions.get(hostId).status !== 'idle' ? false : env.core.sessions.submit(hostId, text).then(() => true, (e: unknown) => {
      if ((e as { status?: number }).status === 409 && /another prompt/.test(String(e))) return false;
      throw e;
    }), (sent) => sent, 20000, `the host to take "${text}"`);
    await waitFor(() => env.core.sessions.get(hostId).status, (s) => s === 'idle', 10000, "host's turn");
  };
  /** Meko's datapack ids are UUIDs, which MEKO.md's rows need. */
  const GOTCHAS_ID = '0f6b1c1e-0000-4000-8000-00000000a001';
  const DEDICATED_ID = '0f6b1c1e-0000-4000-8000-00000000a002';
  const uuids = () => {
    env.meko.createDatapack = (async (name: string) => ({ id: name.endsWith('/gotchas') ? GOTCHAS_ID : DEDICATED_ID, name })) as typeof env.meko.createDatapack;
  };
  /** Meko already has the gotchas' bytes: the upload is indexed at once. */
  const gotchasKnown = () => env.mekoApi.handle((c: ApiCall) => c.path.endsWith('/upload-url')
    ? { body: { data: { url: 'https://storage.test/kb', fields: {}, key: 'dk-gotchas', already_uploaded: true } } }
    : undefined);

  it('builds the demo with no remote, opens it as the workshop\'s project with its datapacks in MEKO.md, and begins its host on GUIDE.md', async () => {
    uuids();
    gotchasKnown();
    const r = await env.core.workshops.start(AGENTK_TOUR);
    const p = env.core.projects.get(r.projectId);
    expect(p).toMatchObject({ name: 'AgentK tour', workshop: AGENTK_TOUR, repoPath: join(env.home, 'code', 'agentk-demo') });
    const repo = p.repoPath;
    expect(gitIn(repo, 'remote')).toBe('');
    // Every install gets the same history: made-up authors, not the user's identity.
    expect(gitIn(repo, 'log', '--format=%an <%ae>', 'main')).toBe('AgentK Demo <demo@example.com>');
    expect(gitIn(repo, 'log', '--format=%an <%ae>', '-1', 'demo/score-fix')).toBe('Alice <alice@example.com>');
    // The review finds the gotchas datapack through MEKO.md (on both branches and without the gotchas: the unit test's).
    const mekoMd = readFileSync(join(repo, 'MEKO.md'), 'utf8');
    expect(parseMekoMd(mekoMd)).toEqual([
      { name: 'adlc/repo/agentk-demo/gotchas', id: GOTCHAS_ID, about: expect.any(String) },
      { name: p.datapacks[0].name, id: DEDICATED_ID, about: expect.any(String) },
    ]);
    // The gotchas datapack isn't the project's: only the review reads it.
    expect(p.datapacks.map((d) => d.role)).toEqual(['dedicated']);
    // The gotchas datapack starts empty: Level 1's first look reviews before they're added.
    expect(env.mekoApi.calls.some((c) => c.path.endsWith('/upload-url'))).toBe(false);

    const host = env.core.sessions.get(r.sessionId);
    expect(host).toMatchObject({ stage: 'explore', purpose: 'guide', projectId: p.id });
    await waitFor(() => env.core.sessions.get(host.id).status, (s) => s === 'idle', 10000, "host's first turn");
    const first = firstPrompt(host.id);
    expect(first).toContain('host of the AgentK tour');
    expect(first).toContain(`projectId ${p.id}`);
    expect(first).toContain(`${repo}/index.html`);
    expect(first).toContain('adlc/repo/agentk-demo/gotchas');
    // AgentK's view mod hides the host's events prompts by their first line.
    const conv = env.core.sessions.get(host.id).claudeSessionId;
    const launch = readJsonl<{ args: string[]; env: Record<string, string> }>(join(env.fakeDir, 'argv.jsonl')).find((a) => a.args.includes(conv));
    expect(launch?.env.AK_HIDDEN_PROMPT).toBe('AgentK events:');

    // Starting it again goes to the same project and host.
    expect(await env.core.workshops.start(AGENTK_TOUR)).toEqual(r);
    expect(env.core.workshops.list()).toEqual([expect.objectContaining({ id: AGENTK_TOUR, projectId: p.id, hostId: host.id })]);
  });

  it("sends the host's events again when Claude goes before their turn starts", async () => {
    uuids();
    gotchasKnown();
    const r = await env.core.workshops.start(AGENTK_TOUR);
    const host = env.core.sessions.get(r.sessionId);
    await waitFor(() => env.core.sessions.get(host.id).status, (s) => s === 'idle', 10000, "host's first turn");
    // As when Claude exits after the prompt counted as sent but before its turn started.
    const submit = env.core.sessions.submit.bind(env.core.sessions);
    let dropped = 0;
    vi.spyOn(env.core.sessions, 'submit').mockImplementation(async (id, text, lost, hidden) => {
      if (text.startsWith('AgentK events:') && !dropped++) return lost?.();
      return submit(id, text, lost, hidden);
    });
    env.core.workshops.events.tell(r.projectId, 'The demo is ready.');
    const told = () => prompts(host.claudeSessionId).filter((t) => t.startsWith('AgentK events:') && t.includes('The demo is ready.'));
    await waitFor(told, (t) => t.length === 1, 20000, 'the events sent again');
    expect(dropped).toBe(2);
  });

  it("reviews the level first with the gotchas datapack empty, then on the user's go adds the gotchas and reviews again; tells the host, and opens no PR there", async () => {
    uuids();
    gotchasKnown();
    const r = await env.core.workshops.start(AGENTK_TOUR);
    const host = env.core.sessions.get(r.sessionId);
    await waitFor(() => env.core.sessions.get(host.id).status, (s) => s === 'idle', 10000, "host's first turn");

    // The host starts the level on the teammate's branch, as its script says.
    const out = await env.core.tools.call(env.core.sessions.row(host.id), {
      tool: 'start_sessions',
      input: { sessions: [{ name: 'Review the score fix', prompt: 'Read the change.', stage: 'code', branch: 'demo/score-fix' }] },
    });
    expect((out.body as { text: string }).text).toContain('on demo/score-fix');
    const task = env.core.sessions.listForProject(r.projectId).find((s) => s.name === 'Review the score fix')!;
    expect(task).toMatchObject({ stage: 'code', branch: 'demo/score-fix' });
    await waitFor(() => env.core.sessions.get(task.id).status, (s) => s === 'idle', 10000, "task's first turn");
    // AgentK reviews the level by itself on the tour's Sirius 1.0, with no PR description first: a first
    // look that keeps its findings, then, on the user's go, once the gotchas are uploaded and indexed, the review proper.
    const reviews = () => all<{ by: string; purpose: string; strategy: string; hand_off: number; state: string }>(env.core.db,
      'SELECT by, purpose, strategy, hand_off, state FROM code_reviews WHERE task_id = ? ORDER BY created_at, rowid', task.id);
    const events = () => prompts(host.claudeSessionId).filter((t) => t.startsWith('AgentK events:')).join('\n');
    await waitFor(events, (t) => t.includes("AgentK waits for the user's go"), 20000, 'the pause');
    expect(reviews()).toEqual([{ by: 'auto', purpose: 'demand', strategy: 'sirius-1.0', hand_off: 0, state: 'done' }]);
    // The review session starts on the workshop's model for its agent; the task keeps its own.
    expect(get<{ model: string | null }>(env.core.db, 'SELECT s.model FROM code_reviews c JOIN sessions s ON s.id = c.session_id WHERE c.task_id = ?', task.id)?.model).toBe('sonnet');
    expect(env.core.sessions.row(task.id).model).toBeNull();
    expect(env.mekoApi.calls.some((c) => c.path.endsWith('/upload-url'))).toBe(false);
    // Anything but a go leaves it waiting.
    await say(host.id, 'What happens next?');
    expect(reviews()).toHaveLength(1);

    await say(host.id, 'go');
    await waitFor(reviews, (rs) => rs.length === 2, 20000, 'the review with the gotchas');
    expect(reviews()[1]).toMatchObject({ by: 'auto', purpose: 'demand', strategy: 'sirius-1.0', hand_off: 1 });
    expect(env.mekoApi.calls.find((c) => c.path.endsWith('/upload-url'))?.path).toBe(`/datapacks/${GOTCHAS_ID}/knowledge-bases/upload-url`);
    // Nothing waits for a go now.
    await expect(env.core.workshops.go(AGENTK_TOUR)).rejects.toMatchObject({ status: 409 });
    expect(env.core.projects.get(r.projectId).settings.codeReview).toEqual({ strategy: 'sirius-1.0' });
    expect(prompts(env.core.sessions.get(task.id).claudeSessionId).some((t) => t.includes("write the PR's title and description"))).toBe(false);
    await waitFor(events,
      (t) => ["Review the score fix: AgentK's turn ended", 'Review the score fix: AgentK ran a first look', 'Review the score fix: First look done',
        'Review the score fix: AgentK ran the code review'].every((x) => t.includes(x)), 20000, 'events');

    // A session in another project tells this host nothing.
    const other = await env.core.projects.create({ repoPath: env.repo, name: 'Other', datapack: { mode: 'none' } });
    const elsewhere = await env.core.sessions.create(other.id, { name: 'Elsewhere', prompt: 'hi', start: 'explore' });
    await waitFor(() => env.core.sessions.get(elsewhere.id).status, (s) => s === 'idle', 10000, 'other turn');
    expect(prompts(host.claudeSessionId).some((t) => t.includes('Elsewhere'))).toBe(false);

    // The demo has no GitHub repo: Open PR, Run to merge and a move to Pull Request are refused.
    const row = env.core.sessions.row(task.id);
    await expect(env.core.lifecycle.openPr(row)).rejects.toMatchObject({ status: 409, message: NO_PR_IN_WORKSHOP });
    await expect(env.core.lifecycle.setAutoRun(row, true)).rejects.toMatchObject({ status: 409 });
    await expect(env.core.lifecycle.move(row, { stage: 'review' })).rejects.toMatchObject({ status: 409 });
    expect(env.core.sessions.get(task.id).stage).toBe('code');

    // A last line of "That's the end of Level 1" archives the level's session; the phrase anywhere else leaves it.
    const fix = join(env.core.sessions.row(task.id).worktree_path, 'src', 'fix.js');
    writeFileSync(fix, '// the level\'s fix, not committed\n');
    await say(host.id, "SAY:I'll say That's the end of Level 1 once you've played the fix.\\nPlay it now.");
    expect(env.core.sessions.row(task.id).desired).toBe('running');
    await say(host.id, "SAY:Meko caught the trap.\\nThat's the end of Level 1.");
    await waitFor(() => env.core.sessions.row(task.id).desired, (d) => d === 'archived', 10000, 'the level closed');
    expect(env.core.sessions.row(host.id).desired).toBe('running');
    expect(env.core.sessions.row(elsewhere.id).desired).toBe('running');
    // Archived, not discarded: the uncommitted fix is still there.
    expect(readFileSync(fix, 'utf8')).toContain("the level's fix");

    // A replay of the level, once that worktree is gone, gets its review by itself: the gotchas are
    // in, so no first look. Run code review there writes no PR description either.
    gitIn(env.core.projects.get(r.projectId).repoPath, 'worktree', 'remove', '--force', env.core.sessions.row(task.id).worktree_path);
    const replay = await env.core.sessions.create(r.projectId, { name: 'Replay', prompt: 'Read the change.', start: 'code', branch: 'demo/score-fix', existingBranch: true });
    const replayed = () => all<{ by: string; hand_off: number; state: string }>(env.core.db, 'SELECT by, hand_off, state FROM code_reviews WHERE task_id = ? ORDER BY created_at, rowid', replay.id);
    await waitFor(replayed, (rs) => rs[0]?.state === 'done', 20000, "the replay's review");
    expect(replayed()).toEqual([{ by: 'auto', hand_off: 1, state: 'done' }]);
    await env.core.sessions.action(replay.id, 'review-run');
    await waitFor(replayed, (rs) => rs.length === 2 && rs[1].state === 'done', 20000, 'Run code review');
    expect(prompts(env.core.sessions.get(replay.id).claudeSessionId).some((t) => t.includes("write the PR's title and description"))).toBe(false);
  });

  it("Level 2: offers Learn the project with the tour's brief, runs the learn step after the question session's turn, and gives the host the Meko line and Context Gaps links", async () => {
    uuids();
    const gaps = await fakeMekoQuestions();
    try {
      await useLocalMeko(env, gaps.url);
      env.local.meko = { ...env.local.meko!, webUrl: 'http://meko.test/' };
      await env.core.refreshMeko();
      Object.assign(env.meko, { ensureConversation: async (_agent: string, _dp: string, key: string) => `conv-${key}` });
      const r = await env.core.workshops.start(AGENTK_TOUR);
      const host = env.core.sessions.get(r.sessionId);
      await waitFor(() => env.core.sessions.get(host.id).status, (s) => s === 'idle', 10000, "host's first turn");
      const events = () => prompts(host.claudeSessionId).filter((t) => t.startsWith('AgentK events:')).join('\n');

      // The dialog opens with the tour's brief; the home offers it, though Level 1 may have saved a memory there (3 in the fake).
      expect(env.core.workshops.list()[0].learn).toMatchObject({ about: expect.stringContaining('Snake'), months: 0 });
      expect((await env.core.projects.home(r.projectId)).learnOffered).toBe(true);
      // The host's script names the project's own datapack, which Learn fills.
      expect(firstPrompt(host.id)).toContain(env.core.projects.get(r.projectId).datapacks[0].name);

      // The question session: Meko can't answer one of its questions, which Context Gaps lists for its conversation.
      const out = await env.core.tools.call(env.core.sessions.row(host.id), {
        tool: 'start_sessions', input: { sessions: [{ name: 'Ask about the game', prompt: 'MEKO_SEARCH Answer the two questions.', stage: 'explore' }] },
      });
      expect((out.body as { text: string }).text).toContain('Ask about the game');
      const ask = env.core.sessions.listForProject(r.projectId).find((s) => s.name === 'Ask about the game')!;
      gaps.datapacks.set(DEDICATED_ID, [{ key: 'untested rules', query: 'Which rules have no test', askedBy: [`conv-${ask.claudeSessionId}`] }]);
      // Its first turn ends: the host hears the Meko line and where the question is, and AgentK runs the learn step by itself, once.
      const link = `http://meko.test/datapacks/${DEDICATED_ID}/gaps`;
      await waitFor(events, (t) => t.includes("Ask about the game: AgentK's turn ended. Meko: 1 lookup"), 20000, 'the Meko line');
      expect(events()).toContain(`1 question it asked Meko couldn't answer is on Meko's Context Gaps: ${link}`);
      await waitFor(events, (t) => t.includes('AgentK is saving what Ask about the game learned to Meko'), 20000, 'the learn step');
      const learns = () => all<{ trigger: string }>(env.core.db, 'SELECT trigger FROM meko_learns WHERE session_id = ?', ask.id);
      await waitFor(learns, (l) => l.length === 1, 10000, 'learn row');
      expect(learns()).toEqual([{ trigger: 'manual' }]);
      await waitFor(() => prompts(ask.claudeSessionId).join('\n'), (t) => t.includes('Save what this session learned to Meko'), 10000, 'the learn prompt');
      await waitFor(() => env.core.sessions.get(ask.id).status, (s) => s === 'idle', 10000, "the learn step's turn");
      expect(learns()).toHaveLength(1);

      // A learn step that taught the question: the host gets the link again, to see it under Taught.
      env.core.sessions.event(ask.id, 'learned', "Saved 1 memory and corrected 0 in the demo; taught Meko 1 question it couldn't answer", { saves: 1, taught: 1 });
      await waitFor(events, (t) => t.includes(`that question is now under Taught on Meko's Context Gaps: ${link}`), 20000, 'the taught link');

      // "Ask again" has no learn step after its turn.
      await env.core.tools.call(env.core.sessions.row(host.id), {
        tool: 'start_sessions', input: { sessions: [{ name: 'Ask again', prompt: 'Answer again.', stage: 'explore' }] },
      });
      const again = env.core.sessions.listForProject(r.projectId).find((s) => s.name === 'Ask again')!;
      // It replaces the level's earlier session: the host starts it on the user's go, and AgentK closes that one.
      await waitFor(() => env.core.sessions.row(ask.id).desired, (d) => d === 'archived', 10000, '"Ask about the game" closed');
      await waitFor(events, (t) => t.includes("Ask again: AgentK's turn ended"), 20000, "the second session's turn");
      expect(all(env.core.db, 'SELECT 1 FROM meko_learns WHERE session_id = ?', again.id)).toEqual([]);

      // Once a learn session ran on the datapack, the home stops offering it.
      const learn = await env.core.sessions.create(r.projectId, { name: 'Learn the game', prompt: '', learn: { about: 'A game', where: '', months: 0 } });
      expect((await env.core.projects.home(r.projectId)).learnOffered).toBe(false);

      // The end of Level 2 archives every open session in the project but the host.
      // A session still saving what it learned stays open at the level's end: Archive would cut the save short.
      const running = env.core.learn.running.bind(env.core.learn);
      const saving = vi.spyOn(env.core.learn, 'running').mockImplementation((id) => id === again.id || running(id));
      await say(host.id, "SAY:Meko learned it.\\nThat's the end of Level 2.");
      await waitFor(() => env.core.sessions.row(learn.id).desired, (d) => d === 'archived', 10000, 'the level closed');
      expect(env.core.sessions.row(again.id).desired).toBe('running');
      expect(env.core.sessions.row(host.id).desired).toBe('running');
      saving.mockRestore();
      // Once a learn session has run, the offer stays off while the datapack has memories, and comes back on an empty one.
      expect((await env.core.projects.home(r.projectId)).learnOffered).toBe(false);
      env.meko.describeDatapack = (async () => ({ memoryCount: 0, knowledgeCount: 0, grant: 'owner', ownerName: 'Test', ownerEmail: 'test@example.com' })) as typeof env.meko.describeDatapack;
      expect((await env.core.projects.home(r.projectId)).learnOffered).toBe(true);
    } finally {
      await gaps.close();
    }
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

  it("starts without Meko: no datapacks and no MEKO.md, and the host's script says Meko is off", async () => {
    await noMeko(env);
    const r = await env.core.workshops.start(AGENTK_TOUR);
    const p = env.core.projects.get(r.projectId);
    expect(p.datapacks).toEqual([]);
    expect(existsSync(join(p.repoPath, 'MEKO.md'))).toBe(false);
    expect(env.meko.calls.filter((c) => c.name === 'createDatapack')).toEqual([]);
    await waitFor(() => env.core.sessions.get(r.sessionId).status, (s) => s === 'idle', 10000, "host's first turn");
    expect(firstPrompt(r.sessionId)).toMatch(/Meko[^\n]*\boff\b/);
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
    await waitFor(() => env.core.workshops.list()[0]?.hostId ?? null, (id) => !!id, 15000, 'tour host');
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
