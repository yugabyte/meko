import { readFileSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { makeEnv, waitFor, type TestEnv } from '../helpers.js';
import { designQuestions } from '../../src/server/answers.js';
import { all, run } from '../../src/server/db.js';
import { DESIGN_FEEDBACK_ID } from '../../src/shared/types.js';

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
  const transcript = (id: string) => readFileSync(join(env.fakeDir, `${get(id).claudeSessionId}.jsonl`), 'utf8');
  const events = (id: string) => all<{ text: string }>(env.core.db, `SELECT text FROM session_events WHERE session_id = ? AND kind = 'design'`, id).map((e) => e.text);
  const put = (id: string, qid: string, b: unknown) => env.core.answers.put(id, qid, b);
  const designReady = (id: string, what: string, more: (s: ReturnType<typeof get>) => boolean = () => true) =>
    waitFor(() => get(id), (x) => x.status === 'needs_input' && x.hasDesign && more(x), 10000, what);

  it('lists the questions, takes answers, and sends them to AgentK to revise the design', async () => {
    const p = await newProject();
    const s = await env.core.sessions.create(p.id, { name: 'Ask me', prompt: 'DESIGN_QUESTIONS it' });
    const ready = await designReady(s.id, 'design with questions');
    expect(transcript(s.id)).toContain(join(env.core.paths.designs, s.id, 'questions.json')); // the prompt names the file
    expect(ready.designQuestions).toEqual({ open: 2, unsent: 0, changedAt: null });
    expect(designQuestions(env.core.db, s.id).map((q) => [q.id, q.status, q.recommended])).toEqual([['store', 'open', 0], ['tabs', 'open', null]]);

    // One draft per question, changed field by field; one left empty is removed.
    put(s.id, 'store', { choice: 0 });
    put(s.id, 'store', { text: 'RESOLVE_STORE please' });
    put(s.id, 'tabs', { yourCall: true });
    expect(put(s.id, 'tabs', { yourCall: false }).map((a) => a.questionId)).toEqual(['store']);
    expect(put(s.id, DESIGN_FEEDBACK_ID, { text: 'Keep it short.' }).map((a) => [a.questionId, a.choiceLabel, a.text])).toEqual([
      ['store', 'Datapack', 'RESOLVE_STORE please'],
      [DESIGN_FEEDBACK_ID, null, 'Keep it short.'],
    ]);
    expect(() => put(s.id, 'store', { choice: 2 })).toThrow(expect.objectContaining({ status: 400 }));
    expect(() => put(s.id, 'store', { text: 5 })).toThrow(expect.objectContaining({ status: 400 }));
    expect(() => put(s.id, 'nope', { text: 'x' })).toThrow(expect.objectContaining({ status: 400 }));
    expect(() => put(s.id, DESIGN_FEEDBACK_ID, { choice: 0 })).toThrow(expect.objectContaining({ status: 400 }));
    expect(get(s.id).designQuestions).toMatchObject({ open: 2, unsent: 2, changedAt: expect.any(Number) });

    // While the design waits for approval, they're typed in at once.
    await expect(env.core.answers.send(s.id)).resolves.toEqual({ sent: 2, queued: 0 });
    const sent = await waitFor(() => transcript(s.id), (t) => t.includes('Answer: Datapack'), 10000, 'answers prompt');
    expect(sent).toContain('The user sent 1 answer to an open question and other feedback on your design');
    expect(sent).toContain('### Q1 (id: store): Where do designs live? Answer: Datapack Note: RESOLVE_STORE please');
    expect(sent).toMatch(/### Other feedback Keep it short\./);
    expect(events(s.id)).toContain('Sent 1 answer to an open question and other feedback to AgentK');
    await expect(env.core.answers.send(s.id)).rejects.toMatchObject({ status: 409 });

    // The revision resolves a question, and the design is ready again.
    await designReady(s.id, 'revised design', (x) => x.designQuestions.open === 1);
    expect(designQuestions(env.core.db, s.id)[0]).toMatchObject({ id: 'store', status: 'resolved', decision: 'Datapack.' });
    expect(() => put(s.id, 'store', { text: 'more' })).toThrow(expect.objectContaining({ status: 409 }));
    expect(env.core.answers.list(s.id).map((a) => a.state)).toEqual(['sent', 'sent']);

    // While AgentK works, they wait for its turn to end.
    writeFileSync(join(env.fakeDir, 'turn-ms'), '1500');
    await env.core.sessions.submit(s.id, 'think about it');
    put(s.id, 'tabs', { text: 'One column.' });
    await expect(env.core.answers.send(s.id)).resolves.toEqual({ sent: 0, queued: 1 });
    expect(get(s.id).designQuestions.unsent).toBe(1);
    expect(() => put(s.id, 'tabs', { text: 'Changed my mind' })).toThrow(expect.objectContaining({ status: 409 }));
    writeFileSync(join(env.fakeDir, 'turn-ms'), '200');
    await waitFor(() => transcript(s.id), (t) => t.includes('Answer: One column.'), 10000, 'queued answer sent at turn end');
    await waitFor(() => get(s.id), (x) => x.status !== 'working' && x.designQuestions.unsent === 0, 10000, 'answer turn over');

    // Approving takes the answers not yet sent along.
    await waitFor(() => get(s.id).status, (st) => st === 'needs_input' || st === 'idle', 10000, 'between turns');
    put(s.id, DESIGN_FEEDBACK_ID, { text: 'Ship it.' });
    await env.core.sessions.action(s.id, 'approve-design');
    expect(get(s.id)).toMatchObject({ stage: 'code', designQuestions: { open: 1, unsent: 0 } });
    const approval = await waitFor(() => transcript(s.id), (t) => t.includes('The design is approved'), 10000, 'approval prompt');
    expect(approval).toMatch(/as they approved it\. Where an answer differs from the design, follow the answer\.\s+### Other feedback Ship it\.\s+The design is approved/);
    expect(events(s.id)).toContain('Approved with 1 answer');

    // Past Design, answers can't change.
    expect(() => put(s.id, DESIGN_FEEDBACK_ID, { text: 'More' })).toThrow(expect.objectContaining({ status: 409 }));
  });

  it('lists a markdown design\'s "Open questions" when there is no questions file', async () => {
    const p = await newProject();
    const s = await env.core.sessions.create(p.id, { name: 'Plain', prompt: 'DESIGN_MD it' });
    await designReady(s.id, 'markdown design');
    expect(get(s.id).designQuestions.open).toBe(0);
    writeFileSync(join(env.core.paths.designs, s.id, 'design.md'), '# Design\n\n## Open questions\n\n- Tabs or one column?\n- Keep the fallback?\n');
    await env.core.sessions.submit(s.id, 'revise');
    await designReady(s.id, 'design with questions', (x) => x.designQuestions.open === 2);
    expect(designQuestions(env.core.db, s.id).map((q) => q.question)).toEqual(['Tabs or one column?', 'Keep the fallback?']);

    // A broken or oversized questions file leaves the questions alone.
    for (const bad of ['{oops', `{"questions": []}${' '.repeat(300 * 1024)}`]) {
      writeFileSync(join(env.core.paths.designs, s.id, 'questions.json'), bad);
      await env.core.lifecycle.collectDesign(env.core.sessions.row(s.id));
      expect(get(s.id).designQuestions.open).toBe(2);
    }
  });

  it('keeps a draft on the option the user picked when a revision changes the options', async () => {
    const p = await newProject();
    const s = await env.core.sessions.create(p.id, { name: 'Shift', prompt: 'DESIGN_QUESTIONS it' });
    await designReady(s.id, 'design with questions');
    put(s.id, 'store', { choice: 1 }); // Repo
    const file = join(env.core.paths.designs, s.id, 'questions.json');
    const revise = async (options: string[]) => {
      writeFileSync(file, JSON.stringify({ questions: [{ id: 'store', question: 'Where do designs live?', options }] }));
      await env.core.lifecycle.collectDesign(env.core.sessions.row(s.id));
      return env.core.answers.list(s.id)[0];
    };
    // Moved: the pick follows its label.
    expect(await revise(['Repo', 'Datapack'])).toMatchObject({ choice: 0, choiceLabel: 'Repo' });
    // Gone: nothing is picked for the user, and their pick still goes to Claude.
    expect(await revise(['Datapack'])).toMatchObject({ choice: null, choiceLabel: 'Repo' });
    expect(put(s.id, 'store', { text: 'A note' })[0]).toMatchObject({ choice: null, choiceLabel: 'Repo', text: 'A note' });
    expect(env.core.answers.forApproval(s.id)!.text).toBe('### Q1 (id: store): Where do designs live?\nAnswer: Repo\nNote: A note');
    // Picking again replaces it.
    expect(put(s.id, 'store', { choice: 0 })[0]).toMatchObject({ choice: 0, choiceLabel: 'Datapack' });
  });

  it('refuses to approve with answers too long for one prompt', async () => {
    const p = await newProject();
    const s = await env.core.sessions.create(p.id, { name: 'Long', prompt: 'DESIGN_QUESTIONS it' });
    await designReady(s.id, 'design with questions');
    const now = Date.now();
    for (let i = 0; i < 6; i++) {
      run(env.core.db, `INSERT INTO design_answers(id, session_id, question_id, question, text, state, created_at, updated_at)
                        VALUES (?, ?, ?, 'Old?', ?, 'draft', ?, ?)`, `a${i}`, s.id, `old-${i}`, 'x'.repeat(10_000), now, now);
    }
    await expect(env.core.sessions.action(s.id, 'approve-design')).rejects.toMatchObject({ status: 400, message: expect.stringMatching(/Shorten some/) });
    expect(get(s.id).stage).toBe('design');
  });

  it('turns queued answers back into drafts when the session leaves Design', async () => {
    const p = await newProject();
    const s = await env.core.sessions.create(p.id, { name: 'Leave', prompt: 'DESIGN_QUESTIONS it' });
    await designReady(s.id, 'design with questions');
    put(s.id, 'store', { choice: 1 });
    run(env.core.db, `UPDATE design_answers SET state = 'queued' WHERE session_id = ?`, s.id);
    await env.core.lifecycle.move(env.core.sessions.row(s.id), { stage: 'code', prompt: false });
    expect(env.core.answers.list(s.id).map((a) => a.state)).toEqual(['draft']);
    await expect(env.core.answers.send(s.id)).rejects.toMatchObject({ status: 409 });
    expect(() => put(s.id, 'store', { choice: 0 })).toThrow(expect.objectContaining({ status: 409 }));
    // The draft can still be dropped.
    env.core.answers.remove(s.id, 'store');
    expect(env.core.answers.list(s.id)).toEqual([]);

    // Every way out of Design does it, the PR prompt's too (it sets the stage itself).
    await env.core.lifecycle.move(env.core.sessions.row(s.id), { stage: 'design', prompt: false });
    put(s.id, 'store', { choice: 1 });
    run(env.core.db, `UPDATE design_answers SET state = 'queued' WHERE session_id = ?`, s.id);
    env.core.sessions.setStage(s.id, 'pr', 'you');
    expect(env.core.answers.list(s.id).map((a) => a.state)).toEqual(['draft']);
  });
});
