import { readFileSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
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

    // The revision resolves a question, and the design is ready again.
    await designReady(s.id, 'revised design', (x) => x.designQuestions.open === 1);
    expect(questions(s.id)[0]).toMatchObject({ id: 'store', status: 'resolved', decision: 'Datapack.' });
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

    // A broken or oversized questions file leaves the questions alone.
    for (const bad of ['{oops', `{"questions": []}${' '.repeat(300 * 1024)}`]) {
      writeFileSync(join(env.core.paths.designs, s.id, 'questions.json'), bad);
      await env.core.lifecycle.collectDesign(row(s.id));
      expect(get(s.id).designQuestions.open).toBe(2);
    }
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
