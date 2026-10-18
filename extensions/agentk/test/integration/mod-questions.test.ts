import { existsSync, mkdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';
import { all } from '../../src/server/db.js';
import { makeEnv, waitFor, type TestEnv } from '../helpers.js';

/** Lines of a JSONL file the fake Claude writes, or none yet. */
function jsonl<T>(env: TestEnv, file: string): T[] {
  const f = join(env.fakeDir, file);
  return existsSync(f) ? readFileSync(f, 'utf8').trim().split('\n').map((l) => JSON.parse(l) as T) : [];
}

describe("Claude's questions and the design through AgentK's mod (real session host + git, fake claude)", () => {
  let env: TestEnv;
  afterEach(async () => {
    await env.cleanup();
  });

  async function start(files: Record<string, string>, opts: { prompt?: string; kind?: 'quick' } = {}) {
    env = await makeEnv();
    mkdirSync(env.fakeDir, { recursive: true });
    for (const [f, text] of Object.entries({ mod: '', ...files })) writeFileSync(join(env.fakeDir, f), text);
    const p = await env.core.projects.create({ repoPath: env.repo, name: 'Demo', datapack: { mode: 'existing', id: 'dp-1', name: 'team/demo' } });
    const s = await env.core.sessions.create(p.id, { name: 'Ask', prompt: opts.prompt ?? 'do it', ...(opts.kind ? { kind: opts.kind } : {}) });
    await waitFor(() => env.core.sessions.get(s.id), (x) => x.status === 'idle' || x.status === 'needs_input', 10000, 'first turn');
    await waitFor(() => env.core.sessions.get(s.id).modLive, (l) => l, 10000, 'the mod to poll');
    return s.id;
  }
  const get = (id: string) => env.core.sessions.get(id);
  const row = (id: string) => env.core.sessions.row(id);
  const transcript = (id: string) => readFileSync(row(id).transcript_path!, 'utf8');
  const events = (id: string, kind: string) =>
    all<{ text: string }>(env.core.db, 'SELECT text FROM session_events WHERE session_id = ? AND kind = ?', id, kind).map((e) => e.text);
  const slow = (ms: number) => writeFileSync(join(env.fakeDir, 'turn-ms'), String(ms));

  it("shows Claude's question, and its answer from AgentK Local resumes the turn", async () => {
    const id = await start({}, { kind: 'quick' });
    await env.core.sessions.submit(id, 'ASK_USER now');
    const s = await waitFor(() => get(id), (x) => !!x.asking, 10000, 'the question');
    expect(s.status).toBe('needs_input');
    expect(s.statusDetail).toBe('AgentK asks: Which color?');
    expect(s.asking!.questions).toEqual([{
      question: 'Which color?', header: 'Color', multiSelect: false,
      options: [{ label: 'Red', description: 'Warm', preview: null }, { label: 'Blue', description: 'Cool', preview: null }],
    }]);
    const ask = s.asking!.id;
    expect(() => env.core.sessions.answerAsk(id, 'another', { answers: [{ question: 0, labels: ['Red'], text: '' }] })).toThrow(expect.objectContaining({ status: 409 }));
    expect(() => env.core.sessions.answerAsk(id, ask, { answers: [{ question: 0, labels: ['Green'], text: '' }] })).toThrow(expect.objectContaining({ status: 400 }));

    env.core.sessions.answerAsk(id, ask, { answers: [{ question: 0, labels: ['Blue'], text: '' }] });
    expect(get(id).asking).toBeNull();
    await waitFor(() => jsonl(env, 'mod-asks.jsonl'), (a) => a.length === 1, 10000, 'the answer in Claude');
    expect(jsonl(env, 'mod-asks.jsonl')).toEqual([{ by: 'agentk', answers: { 0: 'Blue' } }]);
    await waitFor(() => get(id).status, (st) => st === 'idle', 10000, 'the turn to end');
    expect(events(id, 'ask')).toEqual(["Answered AgentK's question in AgentK Local"]);
  });

  it("shows Claude's Meko call while it runs, and forgets one whose end never came when the turn ends", async () => {
    const id = await start({ 'meko-ms': '1500' }, { kind: 'quick' });
    await env.core.sessions.submit(id, 'MEKO_SEARCH now');
    expect((await waitFor(() => get(id), (x) => x.mekoCall, 10000, 'the Meko call')).status).toBe('working');
    await waitFor(() => get(id), (x) => !x.mekoCall && x.status === 'idle', 10000, 'the call and the turn to end');

    writeFileSync(join(env.fakeDir, 'meko-lost'), '');
    await env.core.sessions.submit(id, 'MEKO_SEARCH again');
    await waitFor(() => get(id).mekoCall, (m) => m, 10000, 'the second Meko call');
    await waitFor(() => get(id), (x) => !x.mekoCall && x.status === 'idle', 10000, 'the turn to end');
  });

  it('drops the question once the terminal answers it first', async () => {
    const id = await start({ 'ask-terminal': '600' }, { kind: 'quick' });
    await env.core.sessions.submit(id, 'ASK_USER now');
    const ask = (await waitFor(() => get(id).asking, (a) => !!a, 10000, 'the question'))!.id;
    await waitFor(() => jsonl(env, 'mod-asks.jsonl'), (a) => a.length === 1, 10000, 'the terminal answer');
    expect(jsonl(env, 'mod-asks.jsonl')).toEqual([{ by: 'terminal', answers: { 0: 'Red' } }]);
    await waitFor(() => get(id).asking, (a) => a === null, 5000, 'the question to go');
    expect(() => env.core.sessions.answerAsk(id, ask, { answers: [{ question: 0, labels: ['Blue'], text: '' }] }))
      .toThrow(expect.objectContaining({ status: 409, message: expect.stringMatching(/answered in the terminal/) }));
  });

  it('takes answers and approvals mid-turn, and questions from the tool before the turn ends', async () => {
    const id = await start({}, { prompt: 'DESIGN_MD it' });
    await waitFor(() => get(id), (x) => x.hasDesign && x.status === 'needs_input', 10000, 'the design');
    const revise = (b: unknown) => env.core.lifecycle.reviseDesign(row(id), b);
    const approve = (b?: unknown) => env.core.sessions.action(id, 'approve-design', b);
    const working = () => waitFor(() => get(id).status, (st) => st === 'working', 10000, 'a turn');

    // Answers sent while AgentK works go now, and Claude takes them when the turn ends; while they
    // wait in Claude, an approval is refused.
    slow(1500);
    await env.core.sessions.submit(id, 'think about it');
    await working();
    await revise({ answers: [], feedback: 'Now?' });
    expect(get(id).status).toBe('working');
    await expect(approve()).rejects.toMatchObject({ status: 409, message: expect.stringMatching(/revised the design/) });
    await waitFor(() => transcript(id), (t) => t.includes('Now?'), 15000, 'the answers to run');
    await waitFor(() => get(id).status, (st) => st !== 'working', 10000, 'between turns');

    // The tool's questions show while the turn goes on; a bad call is told what's wrong.
    await env.core.sessions.submit(id, 'DESIGN_TOOL now');
    await waitFor(() => get(id), (x) => x.designQuestions.open === 1, 10000, "the tool's questions");
    expect(get(id).status).toBe('working');
    // The fake logs each call once its answer is back, which can be after the store shows it.
    const calls = await waitFor(() => jsonl<{ status: number; body: { error?: string; text?: string } }>(env, 'mod-tool.jsonl'), (c) => c.length === 2, 10000, "the tool's answers");
    expect(calls[0]).toMatchObject({ status: 400, body: { error: expect.stringMatching(/Question 1 needs an "id"/) } });
    expect(calls[1]).toMatchObject({ status: 200, body: { text: expect.stringMatching(/^Saved 1 question \(1 open\)/) } });
    expect(JSON.parse(readFileSync(env.core.sessions.questionsFile(row(id)), 'utf8')).questions[0]).toMatchObject({ id: 'store', recommended: 0 });

    // Approved mid-turn: it waits for the turn, which changed the questions, so it's dropped.
    const queued = await approve();
    expect(queued).toMatchObject({ approvalQueued: true, stage: 'design' });
    await expect(revise({ answers: [], feedback: 'More?' })).rejects.toMatchObject({ status: 409, message: expect.stringMatching(/approved the design/) });
    await waitFor(() => get(id), (x) => !x.approvalQueued && x.status === 'needs_input', 10000, 'the turn to end');
    expect(get(id).stage).toBe('design');
    expect(events(id, 'design')).toContain('Queued approval dropped: AgentK changed the design during its turn');
    expect(transcript(id)).not.toContain('The design is approved');

    // Withdrawn, then approved again on a turn that leaves the design alone: it goes when the turn ends.
    await env.core.sessions.submit(id, 'think again');
    await working();
    await approve({ answers: [{ questionId: 'store', choice: 0, label: 'Datapack', text: '' }], feedback: '' });
    await env.core.sessions.action(id, 'cancel-approval');
    expect(get(id).approvalQueued).toBe(false);
    await expect(env.core.sessions.action(id, 'cancel-approval')).rejects.toMatchObject({ status: 409 });
    await approve({ answers: [{ questionId: 'store', choice: 0, label: 'Datapack', text: '' }], feedback: '' });
    expect(get(id).stage).toBe('design');
    await waitFor(() => get(id).stage, (st) => st === 'code', 10000, 'the approval');
    const sent = await waitFor(() => transcript(id), (t) => t.includes('The design is approved'), 10000, 'the approval prompt');
    expect(sent).toContain('Answer: Datapack');
  }, 60_000);

  it('forgets answers another plugin refused, and queues no approval without a turn running', async () => {
    const id = await start({}, { prompt: 'DESIGN_MD it' });
    await waitFor(() => get(id), (x) => x.hasDesign && x.status === 'needs_input', 10000, 'the design');
    const drop = (on: boolean) => (on ? writeFileSync(join(env.fakeDir, 'mod-drop'), '') : rmSync(join(env.fakeDir, 'mod-drop')));

    // Between turns, a refused prompt leaves Needs you with no turn behind it: no approval is queued.
    drop(true);
    await env.core.sessions.submit(id, 'hello');
    await waitFor(() => get(id).statusDetail ?? '', (d) => d.startsWith('A plugin in Claude refused'), 10000, 'the refusal');
    expect(get(id)).toMatchObject({ midTurn: false, promptsHeld: 0 });
    await expect(env.core.sessions.action(id, 'approve-design')).rejects.toMatchObject({ status: 409, message: expect.stringMatching(/waiting on you in the terminal/) });
    drop(false);

    // Answers sent mid-turn are held; once refused they no longer hold up the approval.
    slow(1500);
    await env.core.sessions.submit(id, 'think about it');
    await waitFor(() => transcript(id), (t) => t.includes('think about it'), 10000, 'the turn to start');
    drop(true);
    await env.core.lifecycle.reviseDesign(row(id), { answers: [], feedback: 'Refused.' });
    await waitFor(() => get(id).promptsHeld, (n) => n === 0, 10000, 'the refused answers to be let go');
    drop(false);
    slow(200);
    // (The refusal shows as Needs you while the turn still runs.)
    await waitFor(() => get(id), (x) => x.status === 'idle' && !x.midTurn, 10000, 'the turn to end');
    await env.core.sessions.action(id, 'approve-design');
    expect(get(id).stage).toBe('code');
  }, 60_000);

  it('holds the approval back while answers wait in Claude, and the same questions again change nothing', async () => {
    const id = await start({}, { prompt: 'DESIGN_MD it' });
    await waitFor(() => get(id), (x) => x.hasDesign && x.status === 'needs_input', 10000, 'the design');
    slow(1500);
    await env.core.sessions.submit(id, 'DESIGN_TOOL first');
    await waitFor(() => get(id).designQuestions.open, (n) => n === 1, 10000, "the tool's questions");
    await waitFor(() => get(id).status, (st) => st !== 'working', 10000, 'the turn to end');
    const at = get(id).designUpdatedAt;
    // The same questions again: not a change, so an approval queued during the turn goes.
    await env.core.sessions.submit(id, 'DESIGN_TOOL again');
    await waitFor(() => get(id).status, (st) => st === 'working', 10000, 'a turn');
    await env.core.sessions.action(id, 'approve-design');
    expect(get(id).approvalQueued).toBe(true);
    await waitFor(() => get(id).stage, (st) => st === 'code', 10000, 'the approval');
    expect(events(id, 'design')).not.toContain('Queued approval dropped: AgentK changed the design during its turn');
    // Sent, the approval leaves the question to AgentK: the design's time moves on for that only.
    // (The stage moves to Code before the approval reaches Claude, and the question is resolved after.)
    const q = await waitFor(() => env.core.questions.get(row(id)).questions[0], (x) => x.status === 'resolved', 10000, 'the question resolved');
    expect(q).toMatchObject({ id: 'store', decision: 'Left to AgentK: Datapack' });
    expect(get(id).designUpdatedAt).toBeGreaterThanOrEqual(at!);
  }, 60_000);

  it('drops a queued approval when Claude dies without a word', async () => {
    const id = await start({}, { prompt: 'DESIGN_MD it' });
    await waitFor(() => get(id), (x) => x.hasDesign && x.status === 'needs_input', 10000, 'the design');
    slow(5000);
    await env.core.sessions.submit(id, 'think about it');
    await waitFor(() => get(id).status, (st) => st === 'working', 10000, 'a turn');
    await env.core.sessions.action(id, 'approve-design');
    expect(get(id).approvalQueued).toBe(true);
    await env.core.host.submit(row(id).tmux_name, 'crash');
    const s = await waitFor(async () => {
      await env.core.sessions.reconcile();
      return get(id);
    }, (x) => x.status === 'exited', 10000, 'the exit');
    expect(s).toMatchObject({ approvalQueued: false, stage: 'design', promptsHeld: 0 });
    expect(events(id, 'design')).toContain('Queued approval dropped: Claude exited');
  }, 30_000);
});
