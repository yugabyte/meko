import { existsSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { makeEnv, waitFor, type TestEnv } from '../helpers.js';
import { all } from '../../src/server/db.js';
import type { DesignAnswersRequest } from '../../src/shared/types.js';

describe("a design's open questions (real tmux + git, fake claude)", () => {
  let env: TestEnv;
  beforeEach(async () => {
    env = await makeEnv();
  });
  afterEach(async () => {
    await env.cleanup();
  });

  const newProject = () => env.core.projects.create({ repoPath: env.repo, name: 'Demo', datapack: { mode: 'create', name: 'agent-k-local/test/demo' } });
  const get = (id: string) => env.core.sessions.get(id);
  const row = (id: string) => env.core.sessions.row(id);
  const questions = (id: string) => env.core.questions.get(row(id)).questions;
  const decided = (id: string) => questions(id).map((q) => [q.id, q.status, q.decision]);
  const onDisk = (id: string) => (JSON.parse(readFileSync(env.core.sessions.questionsFile(row(id)), 'utf8')) as { questions: unknown[] }).questions;
  const transcript = (id: string) => readFileSync(join(env.fakeDir, `${get(id).claudeSessionId}.jsonl`), 'utf8');
  const events = (id: string) => all<{ text: string }>(env.core.db, `SELECT text FROM session_events WHERE session_id = ? AND kind = 'design'`, id).map((e) => e.text);
  const revise = (id: string, b: DesignAnswersRequest) => env.core.lifecycle.reviseDesign(row(id), b);
  const designReady = (id: string, what: string, more: (s: ReturnType<typeof get>) => boolean = () => true) =>
    waitFor(() => get(id), (x) => x.status === 'needs_input' && x.hasDesign && more(x), 10000, what);

  it('lists the questions, sends answers to revise the design, and approves with answers', async () => {
    const p = await newProject();
    const s = await env.core.sessions.create(p.id, { name: 'Ask me', prompt: 'DESIGN_QUESTIONS it' });
    const ready = await designReady(s.id, 'design with questions');
    expect(transcript(s.id)).toContain(join(env.core.paths.designs, s.id, 'questions.json')); // the prompt names the file
    expect(ready.designQuestions).toEqual({ open: 2 });
    expect(questions(s.id).map((q) => [q.id, q.status, q.recommended])).toEqual([['store', 'open', 0], ['tabs', 'open', null]]);

    // While the design waits for approval, answers are typed in at once; nothing is kept.
    await revise(s.id, { answers: [{ questionId: 'store', choice: 0, label: 'Datapack', text: 'RESOLVE_STORE please' }], feedback: 'Keep it short.' });
    const sent = await waitFor(() => transcript(s.id), (t) => t.includes('Answer: Datapack'), 10000, 'answers prompt');
    expect(sent).toContain('The user sent 1 answer to an open question and other feedback on your design');
    expect(sent).toContain('### Q1 (id: store): Where do designs live? Answer: Datapack Note: RESOLVE_STORE please');
    expect(sent).toContain('### Q2 (id: tabs): Tabs or one column? No answer: use your judgment, and say what you picked.');
    expect(sent).toMatch(/### Other feedback Keep it short\./);
    expect(events(s.id)).toContain('Sent 1 answer to an open question and other feedback to AgentK');
    // The answered question is resolved with the answer as it goes, on disk too; the other waits for AgentK.
    expect(sent).toContain('AgentK Local has already marked each question with an answer');
    expect(decided(s.id)).toEqual([['store', 'resolved', 'Datapack — RESOLVE_STORE please'], ['tabs', 'open', null]]);
    expect(onDisk(s.id)).toEqual(questions(s.id));

    // The revision (which rewords the decision), and the design is ready again.
    await designReady(s.id, 'revised design', () => questions(s.id)[0].decision === 'Datapack.');
    expect(get(s.id).designQuestions.open).toBe(1);
    // An answer to it now is to a question that changed.
    await expect(revise(s.id, { answers: [{ questionId: 'store', choice: 1, label: 'Repo', text: '' }], feedback: '' }))
      .rejects.toMatchObject({ status: 409, message: expect.stringMatching(/changed its questions/) });
    await expect(revise(s.id, { answers: [], feedback: ' ' })).rejects.toMatchObject({ status: 400 });

    // While AgentK works, they don't go, and aren't kept for later.
    writeFileSync(join(env.fakeDir, 'turn-ms'), '1500');
    await env.core.sessions.submit(s.id, 'think about it');
    await expect(revise(s.id, { answers: [], feedback: 'Now?' })).rejects.toMatchObject({ status: 409, message: expect.stringMatching(/still working/) });
    writeFileSync(join(env.fakeDir, 'turn-ms'), '200');
    await waitFor(() => get(s.id).status, (st) => st === 'needs_input' || st === 'idle', 10000, 'between turns');
    expect(transcript(s.id)).not.toContain('Now?');

    // Approving takes the answers along, ahead of the approval.
    await env.core.sessions.action(s.id, 'approve-design', { answers: [{ questionId: 'tabs', choice: null, label: null, text: 'One column.' }], feedback: 'Ship it.' });
    expect(get(s.id).stage).toBe('code');
    const approval = await waitFor(() => transcript(s.id), (t) => t.includes('The design is approved'), 10000, 'approval prompt');
    expect(approval).toMatch(
      /as they approved it\. Where an answer differs from the design, follow the answer\.\s+### Q2 \(id: tabs\): Tabs or one column\? Answer \(custom\): One column\.\s+### Other feedback Ship it\.\s+The design is approved/,
    );
    expect(events(s.id)).toContain('Approved with 1 answer to an open question and other feedback');
    // Nothing revisits the questions after the approval: they keep the answers.
    expect(decided(s.id)).toEqual([['store', 'resolved', 'Datapack.'], ['tabs', 'resolved', 'One column.']]);
    expect(onDisk(s.id)).toEqual(questions(s.id));

    // Past Design, answers don't go.
    await expect(revise(s.id, { answers: [], feedback: 'More' })).rejects.toMatchObject({ status: 409 });
  });

  it('approves without answers, and refuses answers to options AgentK has changed', async () => {
    const p = await newProject();
    const s = await env.core.sessions.create(p.id, { name: 'Stale', prompt: 'DESIGN_QUESTIONS it' });
    await designReady(s.id, 'design with questions');
    await expect(env.core.sessions.action(s.id, 'approve-design', { answers: [{ questionId: 'store', choice: 0, label: 'Repo', text: '' }], feedback: '' }))
      .rejects.toMatchObject({ status: 409 });
    expect(get(s.id).stage).toBe('design');
    expect(transcript(s.id)).not.toContain('The design is approved');

    // No answers (an empty body, as Approve sends with no open questions): the plain approval.
    await env.core.sessions.action(s.id, 'approve-design', {});
    expect(get(s.id).stage).toBe('code');
    const approval = await waitFor(() => transcript(s.id), (t) => t.includes('The design is approved'), 10000, 'approval prompt');
    expect(approval).not.toContain('as they approved it');
    // Questions left unanswered are left to AgentK, on its recommendation.
    expect(decided(s.id)).toEqual([['store', 'resolved', 'Left to AgentK: Datapack'], ['tabs', 'resolved', 'Left to AgentK']]);
  });

  it("leaves the questions open when the answers or the approval couldn't be sent", async () => {
    const p = await newProject();
    const s = await env.core.sessions.create(p.id, { name: 'Fails', prompt: 'DESIGN_QUESTIONS it' });
    await designReady(s.id, 'design with questions');
    const before = readFileSync(env.core.sessions.questionsFile(row(s.id)), 'utf8');
    const answers = { answers: [{ questionId: 'store', choice: 1, label: 'Repo', text: '' }], feedback: '' };
    vi.spyOn(env.core.tmux, 'submit').mockRejectedValueOnce(new Error('no pane'));
    await expect(revise(s.id, answers)).rejects.toMatchObject({ status: 502 });
    vi.spyOn(env.core.tmux, 'submit').mockRejectedValueOnce(new Error('no pane'));
    await expect(env.core.sessions.action(s.id, 'approve-design', answers)).rejects.toMatchObject({ status: 502 });
    expect(get(s.id).stage).toBe('design');
    expect(decided(s.id)).toEqual([['store', 'open', null], ['tabs', 'open', null]]);
    expect(readFileSync(env.core.sessions.questionsFile(row(s.id)), 'utf8')).toBe(before);
  });

  it('lists a markdown design\'s "Open questions" when there is no questions file', async () => {
    const p = await newProject();
    const s = await env.core.sessions.create(p.id, { name: 'Plain', prompt: 'DESIGN_MD it' });
    await designReady(s.id, 'markdown design');
    expect(get(s.id).designQuestions.open).toBe(0);
    writeFileSync(join(env.core.paths.designs, s.id, 'design.md'), '# Design\n\n## Open questions\n\n- Tabs or one column?\n- Keep the fallback?\n');
    await env.core.sessions.submit(s.id, 'revise');
    await designReady(s.id, 'design with questions', (x) => x.designQuestions.open === 2);
    expect(questions(s.id).map((q) => q.question)).toEqual(['Tabs or one column?', 'Keep the fallback?']);
    expect(env.core.questions.get(row(s.id)).source).toBe('markdown');
    const file = env.core.sessions.questionsFile(row(s.id));

    // Answers to revise it write no questions file: AgentK revises the design's list.
    const tabs = questions(s.id)[0].id;
    await revise(s.id, { answers: [{ questionId: tabs, choice: null, label: null, text: 'Tabs.' }], feedback: '' });
    expect(existsSync(file)).toBe(false);
    expect(get(s.id).designQuestions.open).toBe(2);
    await waitFor(() => transcript(s.id), (t) => t.includes('Answer (custom): Tabs.'), 10000, 'answers prompt');
    await waitFor(() => get(s.id).status, (st) => st === 'idle', 10000, 'the turn to end');

    // A broken or oversized questions file leaves the questions alone.
    for (const bad of ['{oops', `{"questions": []}${' '.repeat(300 * 1024)}`]) {
      writeFileSync(file, bad);
      await env.core.lifecycle.collectDesign(row(s.id));
      expect(get(s.id).designQuestions.open).toBe(2);
    }
    rmSync(file);

    // The approval does write them, answered or left to AgentK.
    await env.core.sessions.action(s.id, 'approve-design', { answers: [{ questionId: tabs, choice: null, label: null, text: 'One column.' }], feedback: '' });
    expect(decided(s.id)).toEqual([[tabs, 'resolved', 'One column.'], [questions(s.id)[1].id, 'resolved', 'Left to AgentK']]);
    expect(onDisk(s.id)).toEqual(questions(s.id));
  });

  it('reads the questions from disk again after a restart, and still sees them change', async () => {
    const p = await newProject();
    const s = await env.core.sessions.create(p.id, { name: 'Restart', prompt: 'DESIGN_QUESTIONS it' });
    await designReady(s.id, 'design with questions');
    const file = join(env.core.paths.designs, s.id, 'questions.json');

    // As after a restart: nothing kept, then loaded at startup for sessions in Design.
    env.core.questions.forget(s.id);
    env.core.questions.preload();
    expect(get(s.id).designQuestions.open).toBe(2);
    const before = get(s.id).designUpdatedAt ?? 0;
    writeFileSync(file, JSON.stringify({ questions: [{ id: 'store', question: 'Where do designs live?', status: 'resolved', decision: 'Datapack.' }] }));
    expect(await env.core.lifecycle.collectDesign(row(s.id))).toBe(true);
    expect(get(s.id).designQuestions.open).toBe(0);
    expect(get(s.id).designUpdatedAt).toBeGreaterThanOrEqual(before);
    // Nothing changed since: not a change.
    expect(await env.core.lifecycle.collectDesign(row(s.id))).toBe(false);

    // A broken file with nothing kept (a restart) shows no questions, and the activity log says why.
    writeFileSync(file, '{oops');
    env.core.questions.forget(s.id);
    env.core.questions.preload();
    expect(get(s.id).designQuestions.open).toBe(0);
    expect(events(s.id).some((e) => /questions file can't be read .* Ask AgentK to fix .*questions\.json/.test(e))).toBe(true);
  });
});
