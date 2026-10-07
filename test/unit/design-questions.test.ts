import { mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { DesignAnswerInput, DesignQuestion, Session } from '../../src/shared/types.js';
import { designAnswers } from '../../src/server/answers.js';
import {
  ANSWER_MAX, MAX_OPTIONS, MAX_QUESTIONS, MAX_QUESTIONS_FILE, answersCount, formatAnswers, parseDesignQuestions, questionsFromMarkdown, readDesignQuestions,
} from '../../src/server/design.js';
import { DEFAULT_PROMPTS, designQuestionsLine, render } from '../../src/server/prompts.js';
import { defaultDraft, draftFor, draftsBlocker, parseDrafts, toRequest, type Draft } from '../../src/web/design-answers.js';
import { approveLabel } from '../../src/web/util.js';

const question = (over: Partial<DesignQuestion>): DesignQuestion => ({
  id: 'q', question: 'Which?', context: null, options: [], recommended: null, status: 'open', decision: null, ...over,
});
const answer = (over: Partial<DesignAnswerInput>): DesignAnswerInput => ({ questionId: 'q', choice: null, label: null, text: '', ...over });
const opts = (...labels: string[]) => labels.map((label) => ({ label, detail: null }));

describe('reading the questions file', () => {
  it('normalizes questions, options and recommendations', () => {
    const qs = parseDesignQuestions(JSON.stringify({
      questions: [
        {
          id: 'Approve-With Answers', question: '  Can Approve take unsent answers? ', context: 'Drafts would be lost.',
          options: [{ label: 'Fold them in', detail: 'They go with the approval.' }, 'Require sending first', { label: '' }, 7],
          recommended: 0, status: 'open',
        },
        { id: 'meko', question: 'A decisions file?', options: [{ label: 'Yes', description: 'A new file' }], recommended: 3, status: 'Resolved', decision: 'No.' },
      ],
    }));
    expect(qs).toEqual([
      {
        id: 'approve-with-answers', question: 'Can Approve take unsent answers?', context: 'Drafts would be lost.',
        options: [{ label: 'Fold them in', detail: 'They go with the approval.' }, { label: 'Require sending first', detail: null }],
        recommended: 0, status: 'open', decision: null,
      },
      { id: 'meko', question: 'A decisions file?', context: null, options: [{ label: 'Yes', detail: 'A new file' }], recommended: null, status: 'resolved', decision: 'No.' },
    ]);
  });

  it('takes a bare array, and drops entries without an id or a question, and repeated ids', () => {
    const qs = parseDesignQuestions(JSON.stringify([
      { id: 'a', question: 'One?' },
      { id: 'a', question: 'Again?' },
      { id: '!!!', question: 'No id?' },
      { id: 'b' },
      'not an object',
      { id: 3, question: 'Numbered?' },
    ]));
    expect(qs.map((q) => [q.id, q.question])).toEqual([['a', 'One?'], ['3', 'Numbered?']]);
  });

  it('caps how many questions and options it keeps, and long text', () => {
    const many = Array.from({ length: MAX_QUESTIONS + 5 }, (_, i) => ({ id: `q${i}`, question: `Q${i}?`, options: Array.from({ length: 9 }, (_, j) => `o${j}`) }));
    const qs = parseDesignQuestions(JSON.stringify({ questions: many }));
    expect(qs).toHaveLength(MAX_QUESTIONS);
    expect(qs[0].options).toHaveLength(MAX_OPTIONS);
    const [long] = parseDesignQuestions(JSON.stringify([{ id: 'x', question: 'y'.repeat(5000) }]));
    expect(long.question).toHaveLength(1001);
    expect(long.question.endsWith('…')).toBe(true);
  });

  it('throws on JSON that is not a list of questions', () => {
    expect(() => parseDesignQuestions('{not json')).toThrow();
    expect(() => parseDesignQuestions('{"items": []}')).toThrow(/questions/);
  });

  describe('from a file', () => {
    let dir: string;
    beforeEach(() => {
      dir = mkdtempSync(join(tmpdir(), 'akl-questions-'));
    });
    afterEach(() => rmSync(dir, { recursive: true, force: true }));

    it('is null when the file is missing or empty, and throws when it is too large or not a file', () => {
      const f = join(dir, 'questions.json');
      expect(readDesignQuestions(f)).toBeNull();
      writeFileSync(f, '  \n');
      expect(readDesignQuestions(f)).toBeNull();
      writeFileSync(f, '{"questions": []}');
      expect(readDesignQuestions(f)).toEqual([]);
      writeFileSync(f, ' '.repeat(MAX_QUESTIONS_FILE + 1));
      expect(() => readDesignQuestions(f)).toThrow(/larger than 256 KB/);
      expect(() => readDesignQuestions(dir)).toThrow(/not a file/);
    });
  });
});

describe('questions from a markdown design', () => {
  it('lists the items under an "Open questions" heading', () => {
    const md = [
      '# Design', '', '## Approach', '- not a question?', '',
      '## Open questions', '',
      '1. **Where do designs live?** Only in the datapack,',
      '   or also in the repo.',
      '2. Should Approve take answers?', '   - nested detail', '',
      '```', '- inside a fence', '```',
      '### A sub-heading', '- Still in the section?',
      '## Test plan', '- after the section',
    ].join('\n');
    const qs = questionsFromMarkdown(md);
    expect(qs.map((q) => q.question)).toEqual([
      'Where do designs live? Only in the datapack, or also in the repo.',
      'Should Approve take answers? - nested detail ``` - inside a fence ```',
      'Still in the section?',
    ]);
    expect(qs.every((q) => /^md-[0-9a-f]{8}$/.test(q.id) && q.status === 'open' && q.options.length === 0)).toBe(true);
    // The same text keeps the same id.
    expect(questionsFromMarkdown(md).map((q) => q.id)).toEqual(qs.map((q) => q.id));
  });

  it('keeps only the asks under a heading that also names risks', () => {
    const md = ['## Risks and open questions', '- The parser may miss items.', '- Should we keep the fallback?', '* Is 30 enough?'].join('\n');
    expect(questionsFromMarkdown(md).map((q) => q.question)).toEqual(['Should we keep the fallback?', 'Is 30 enough?']);
  });

  it('finds nothing without the heading', () => {
    expect(questionsFromMarkdown('# Design\n\n- Is this a question?\n')).toEqual([]);
    expect(questionsFromMarkdown('')).toEqual([]);
  });
});

describe('formatting answers for Claude', () => {
  const questions = [
    question({ id: 'store', question: 'Where?', options: opts('Datapack', 'Repo'), recommended: 0 }),
    question({ id: 'done', question: 'Decided?', status: 'resolved', decision: 'Yes.' }),
    question({ id: 'tabs', question: 'Tabs?' }),
    question({ id: 'layout', question: 'Layout?', options: opts('Tabs', 'Columns'), recommended: 1 }),
    question({ id: 'name', question: 'Name?', options: opts('A', 'B') }),
  ];

  it('lists every open question in order, answered or not, then other feedback', () => {
    const out = formatAnswers([
      answer({ questionId: 'tabs', text: 'One column.' }),
      answer({ questionId: 'store', choice: 1, label: 'Repo', text: ' Under docs/designs. ' }),
      answer({ questionId: 'layout', choice: 0, label: 'Tabs' }),
    ], ' Shorter, please. ', questions);
    expect(out).toBe([
      '### Q1 (id: store): Where?\nAnswer: Repo\nNote: Under docs/designs.',
      '### Q3 (id: tabs): Tabs?\nAnswer (custom): One column.',
      '### Q4 (id: layout): Layout?\nAnswer: Tabs',
      '### Q5 (id: name): Name?\nNo answer: use your judgment, and say what you picked.',
      '### Other feedback\nShorter, please.',
    ].join('\n\n'));
    expect(formatAnswers([], '', [questions[0]])).toBe('### Q1 (id: store): Where?\nNo answer: go with your recommendation.');
  });

  it('counts answers and feedback for the first line', () => {
    expect(answersCount([answer({})], '')).toBe('1 answer to an open question');
    expect(answersCount([answer({}), answer({ questionId: 'b' })], ' ')).toBe('2 answers to open questions');
    expect(answersCount([answer({})], 'More.')).toBe('1 answer to an open question and other feedback');
    expect(answersCount([], 'More.')).toBe('feedback');
  });
});

describe('checking the answers a browser sends', () => {
  const questions = [
    question({ id: 'store', question: 'Where?', options: opts('Datapack', 'Repo'), recommended: 0 }),
    question({ id: 'done', question: 'Decided?', status: 'resolved', decision: 'Yes.' }),
    question({ id: 'tabs', question: 'Tabs?' }),
  ];
  const check = (body: unknown) => () => designAnswers(body, questions);

  it('formats valid answers, and is null with nothing in them', () => {
    expect(designAnswers(undefined, questions)).toBeNull();
    expect(designAnswers({}, questions)).toBeNull();
    expect(designAnswers({ answers: [], feedback: '  ' }, questions)).toBeNull();
    const r = designAnswers({ answers: [{ questionId: 'store', choice: 1, label: 'Repo', text: 'why' }], feedback: 'More.' }, questions)!;
    expect(r.count).toBe('1 answer to an open question and other feedback');
    expect(r.text).toContain('### Q1 (id: store): Where?\nAnswer: Repo\nNote: why');
    expect(r.text).toContain('### Q3 (id: tabs): Tabs?\nNo answer: use your judgment');
    expect(designAnswers({ answers: [{ questionId: 'tabs', choice: null, label: null, text: 'Mine' }] }, questions)!.text).toContain('Answer (custom): Mine');
  });

  it('refuses answers to questions AgentK has changed since', () => {
    for (const a of [
      { questionId: 'gone', choice: 0, label: 'Datapack', text: '' },
      { questionId: 'done', choice: null, label: null, text: 'Late' },
      { questionId: 'store', choice: 0, label: 'Repo', text: '' },
      { questionId: 'store', choice: 5, label: 'Repo', text: '' },
    ]) {
      expect(check({ answers: [a] })).toThrow(expect.objectContaining({ status: 409, message: expect.stringMatching(/changed its questions/) }));
    }
  });

  it('refuses malformed answers', () => {
    for (const body of [
      'text', [], { answers: 'x' }, { feedback: 3 }, { answers: [7] }, { answers: [{ choice: 0 }] },
      { answers: [{ questionId: 'store', choice: 1.5, label: 'Repo', text: '' }] },
      { answers: [{ questionId: 'store', choice: 0 }] },
      { answers: [{ questionId: 'store', choice: 0, label: 'Datapack', text: 4 }] },
      { answers: [{ questionId: 'tabs', choice: null, label: null, text: '  ' }] },
      { answers: [{ questionId: 'tabs', text: 'a' }, { questionId: 'tabs', text: 'b' }] },
      { answers: [{ questionId: 'tabs', text: 'x'.repeat(ANSWER_MAX + 1) }] },
      { feedback: 'x'.repeat(ANSWER_MAX + 1) },
    ]) {
      expect(check(body)).toThrow(expect.objectContaining({ status: 400 }));
    }
    expect(check({ answers: [{ questionId: 'tabs', text: '  ' }] })).toThrow(/Write your answer to Q3/);
  });

  it('refuses answers too long for one prompt', () => {
    const many = Array.from({ length: 6 }, (_, i) => question({ id: `q${i}` }));
    const body = { answers: many.map((q) => ({ questionId: q.id, text: 'x'.repeat(ANSWER_MAX) })) };
    expect(() => designAnswers(body, many)).toThrow(expect.objectContaining({ status: 400, message: expect.stringMatching(/Shorten some/) }));
  });
});

describe('the design prompts', () => {
  it('asks for the questions file, and tells Claude how to apply answers', () => {
    const line = designQuestionsLine('/d/s1/questions.json');
    expect(line).toContain('or else write that JSON to /d/s1/questions.json');
    expect(line).toContain('pass that list to the mcp__agentk__design_questions tool if you have it');
    expect(line).toMatch(/keep each question's id/);
    const sent = render(DEFAULT_PROMPTS['design-answers'], { count: '1 answer to an open question', answers: '### Q1', questionsFile: '/d/q.json' });
    expect(sent).toContain('The user sent 1 answer to an open question on your design');
    expect(sent).toContain('In the design\'s questions (/d/q.json, or the design_questions tool when you have it), mark each answered question "resolved"');
    expect(sent).toMatch(/Don't change anything in the repo/);
    expect(render(DEFAULT_PROMPTS['approved-answers'], { answers: '### Q1' })).toMatch(/follow the answer\.\n\n### Q1$/);
  });

  it('warns that a Mermaid block in an Artifact must not hold < or > as text', () => {
    // The browser parses an Artifact's <pre class="mermaid"> before Mermaid reads it, so a
    // `<session>` in a label becomes tags and the diagram fails (#50). Markdown fences don't.
    const design = DEFAULT_PROMPTS.design;
    expect(design).toContain("A Mermaid block in an Artifact's HTML must not contain `<` or `>` as text (such as `<session>` or `List<T>`)");
    expect(design).toContain('Write `{session}` or "session id" instead.');
    expect(design).toContain('```mermaid blocks render as diagrams there.');
  });
});

describe('answers in the browser', () => {
  const store = question({ id: 'store', options: opts('Datapack', 'Repo'), recommended: 1 });
  const pick = question({ id: 'pick', options: opts('A', 'B') });
  const free = question({ id: 'free' });
  const draft = (over: Partial<Draft>): Draft => ({ choice: null, label: null, custom: false, text: '', ...over });

  it('starts on the recommendation, nothing, or a written answer for a question without options', () => {
    expect(defaultDraft(store)).toEqual({ choice: 1, label: 'Repo', custom: false, text: '' });
    expect(defaultDraft(pick)).toEqual({ choice: null, label: null, custom: false, text: '' });
    expect(defaultDraft(free)).toEqual({ choice: null, label: null, custom: true, text: '' });
  });

  it('finds a picked option again by its label after a revision', () => {
    const moved = { ...store, options: opts('Repo', 'Datapack') };
    expect(draftFor(moved, draft({ choice: 1, label: 'Datapack', text: 'n' }))).toEqual(draft({ choice: 1, label: 'Datapack', text: 'n' }));
    expect(draftFor(moved, draft({ choice: 0, label: 'Datapack' }))).toMatchObject({ choice: 1, label: 'Datapack' });
    // Gone: back to the default, keeping the note.
    expect(draftFor({ ...store, options: opts('Other', 'Repo') }, draft({ choice: 0, label: 'Datapack', text: 'n' })))
      .toEqual({ choice: 1, label: 'Repo', custom: false, text: 'n' });
    expect(draftFor(store, draft({ custom: true, choice: 0, label: 'Datapack', text: 'mine' }))).toEqual(draft({ custom: true, text: 'mine' }));
  });

  it('builds the request, leaving out questions with no answer', () => {
    const resolved = question({ id: 'old', status: 'resolved', options: opts('X'), recommended: 0 });
    const r = toRequest([store, pick, free, resolved], { answers: { pick: draft({ custom: true, text: 'Neither' }), free: draft({ custom: true, text: ' ' }) }, feedback: 'More.' });
    expect(r).toEqual({
      answers: [
        { questionId: 'store', choice: 1, label: 'Repo', text: '' },
        { questionId: 'pick', choice: null, label: null, text: 'Neither' },
      ],
      feedback: 'More.',
    });
  });

  it('holds the answers back while a picked custom answer is empty', () => {
    expect(draftsBlocker([store, pick, free], { answers: {}, feedback: '' })).toBeNull();
    expect(draftsBlocker([store, pick], { answers: { pick: draft({ custom: true }) }, feedback: '' })).toBe('Write your answer to Q2, or pick one of AgentK’s options.');
  });

  it('reads stored drafts, dropping what isn’t one', () => {
    expect(parseDrafts(null)).toEqual({ answers: {}, feedback: '' });
    expect(parseDrafts('{oops')).toEqual({ answers: {}, feedback: '' });
    expect(parseDrafts(JSON.stringify({ answers: { a: draft({ choice: 0, label: 'A' }), b: { choice: 'x' } }, feedback: 'f' })))
      .toEqual({ answers: { a: draft({ choice: 0, label: 'A' }) }, feedback: 'f' });
  });

  describe('kept in localStorage', () => {
    let items: Map<string, string>;
    let full: boolean;
    beforeEach(() => {
      items = new Map();
      full = false;
      vi.stubGlobal('localStorage', {
        getItem: (k: string) => items.get(k) ?? null,
        setItem: (k: string, v: string) => {
          if (full && !k.endsWith(':probe')) throw new Error('QuotaExceededError');
          items.set(k, v);
        },
        removeItem: (k: string) => void items.delete(k),
      });
      vi.resetModules();
    });
    afterEach(() => vi.unstubAllGlobals());
    const fresh = () => import('../../src/web/design-answers.js');

    it('reads what another tab wrote, never a stale copy', async () => {
      const m = await fresh();
      expect(m.draftsPersist()).toBe(true);
      expect(m.loadDrafts('s1')).toEqual({ answers: {}, feedback: '' });
      items.set('ak-design-answers:s1', JSON.stringify({ answers: {}, feedback: 'From the other tab' }));
      expect(m.loadDrafts('s1').feedback).toBe('From the other tab');
      // The other tab sent them: they're gone here too.
      items.delete('ak-design-answers:s1');
      expect(m.loadDrafts('s1').feedback).toBe('');
    });

    it('stops counting as reload-safe once a save fails, keeping the draft for the page', async () => {
      const m = await fresh();
      expect(m.draftsPersist()).toBe(true);
      full = true;
      m.saveDrafts('s1', { answers: {}, feedback: 'Too big to store' });
      expect(m.draftsPersist()).toBe(false);
      expect(items.has('ak-design-answers:s1')).toBe(false);
      expect(m.loadDrafts('s1').feedback).toBe('Too big to store');
    });

    it('keeps the answers of an approval waiting for the turn, until it goes or is dropped', async () => {
      const m = await fresh();
      const s = (over: Partial<Session>) => ({ id: 's1', stage: 'design', approvalQueued: false, ...over }) as Session;
      m.saveDrafts('s1', { answers: {}, feedback: 'Ship it.' });
      m.approvalSent(s({ approvalQueued: true }));
      expect(m.loadDrafts('s1')).toEqual({ answers: {}, feedback: 'Ship it.', approved: true });
      m.settleApprovals([s({ approvalQueued: true })]); // still waiting
      expect(m.loadDrafts('s1').approved).toBe(true);
      // Dropped (the turn changed the design): drafts again, to send with the next approval.
      m.settleApprovals([s({})]);
      expect(m.loadDrafts('s1')).toEqual({ answers: {}, feedback: 'Ship it.' });
      // Sent: the session left Design, and the answers went with the approval.
      m.approvalSent(s({ approvalQueued: true }));
      m.settleApprovals([s({ stage: 'code' })]);
      expect(items.has('ak-design-answers:s1')).toBe(false);
      // An approval that went at once forgets them straight away.
      m.saveDrafts('s1', { answers: {}, feedback: 'Again.' });
      m.approvalSent(s({ stage: 'code' }));
      expect(m.loadDrafts('s1')).toEqual({ answers: {}, feedback: '' });
    });
  });

  it('labels Approve', () => {
    expect(approveLabel(0)).toBe('Approve design');
    expect(approveLabel(2)).toBe('Answer & approve');
  });
});
