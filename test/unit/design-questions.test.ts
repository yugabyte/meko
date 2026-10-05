import { mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { DESIGN_FEEDBACK_ID, type DesignAnswer, type DesignQuestion } from '../../src/shared/types.js';
import {
  MAX_OPTIONS, MAX_QUESTIONS, MAX_QUESTIONS_FILE, answersCount, formatAnswers, parseDesignQuestions, questionsFromMarkdown, readDesignQuestions,
} from '../../src/server/design.js';
import { DEFAULT_PROMPTS, designQuestionsLine, render } from '../../src/server/prompts.js';
import { answerSummary, answersByQuestion, sendLabel } from '../../src/web/design-answers.js';
import { approveLabel } from '../../src/web/util.js';

const question = (over: Partial<DesignQuestion>): DesignQuestion => ({
  id: 'q', question: 'Which?', context: null, options: [], recommended: null, status: 'open', decision: null, ...over,
});
const answer = (over: Partial<DesignAnswer>): DesignAnswer => ({
  id: 'a', questionId: 'q', question: 'Which?', choice: null, choiceLabel: null, yourCall: false, text: '', state: 'draft',
  createdAt: 0, updatedAt: 0, sentAt: null, ...over,
});

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

    it('is null when the file is missing, empty or too large', () => {
      const f = join(dir, 'questions.json');
      expect(readDesignQuestions(f)).toBeNull();
      writeFileSync(f, '  \n');
      expect(readDesignQuestions(f)).toBeNull();
      writeFileSync(f, ' '.repeat(MAX_QUESTIONS_FILE + 1));
      expect(readDesignQuestions(f)).toBeNull();
      writeFileSync(f, '{"questions": []}');
      expect(readDesignQuestions(f)).toEqual([]);
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
    question({ id: 'store', question: 'Where?', options: [{ label: 'Datapack', detail: null }, { label: 'Repo', detail: null }] }),
    question({ id: 'tabs', question: 'Tabs?' }),
  ];

  it('orders them as the design does, then gone questions, then other feedback', () => {
    const out = formatAnswers([
      answer({ questionId: DESIGN_FEEDBACK_ID, question: 'Other feedback', text: 'Shorter, please.' }),
      answer({ questionId: 'tabs', question: 'Tabs?', yourCall: true }),
      answer({ questionId: 'gone', question: 'Old one?', text: 'Yes.' }),
      answer({ questionId: 'store', question: 'Where?', choice: 1, choiceLabel: 'Repo', text: 'Under docs/designs.' }),
    ], questions);
    expect(out).toBe([
      '### Q1 (id: store): Where?\nAnswer: Repo\nNote: Under docs/designs.',
      '### Q2 (id: tabs): Tabs?\nAnswer: your call. Go with your recommendation.',
      '### Earlier question (id: gone): Old one?\nAnswer: Yes.',
      '### Other feedback\nShorter, please.',
    ].join('\n\n'));
  });

  it('counts answers and feedback for the first line', () => {
    expect(answersCount([answer({})])).toBe('1 answer to an open question');
    expect(answersCount([answer({}), answer({ questionId: 'b' })])).toBe('2 answers to open questions');
    expect(answersCount([answer({}), answer({ questionId: DESIGN_FEEDBACK_ID })])).toBe('1 answer to an open question and other feedback');
    expect(answersCount([answer({ questionId: DESIGN_FEEDBACK_ID })])).toBe('feedback');
  });
});

describe('the design prompts', () => {
  it('asks for the questions file, and tells Claude how to apply answers', () => {
    const line = designQuestionsLine('/d/s1/questions.json');
    expect(line).toContain('/d/s1/questions.json');
    expect(line).toMatch(/keep each question's id/);
    const sent = render(DEFAULT_PROMPTS['design-answers'], { count: '1 answer to an open question', answers: '### Q1', questionsFile: '/d/q.json' });
    expect(sent).toContain('The user sent 1 answer to an open question on your design');
    expect(sent).toContain('In /d/q.json, mark each answered question "resolved"');
    expect(sent).toMatch(/Don't change anything in the repo/);
    expect(render(DEFAULT_PROMPTS['approved-answers'], { answers: '### Q1' })).toMatch(/follow the answer\.\n\n### Q1$/);
  });
});

describe('answers in the UI', () => {
  it('keeps the unsent answer and the last one sent per question', () => {
    const { unsent, sent } = answersByQuestion([
      answer({ id: '1', state: 'sent', sentAt: 5, text: 'old' }),
      answer({ id: '2', state: 'sent', sentAt: 9, text: 'newer' }),
      answer({ id: '3', state: 'draft', text: 'draft' }),
      answer({ id: '4', questionId: 'b', state: 'queued' }),
    ]);
    expect(sent.get('q')?.id).toBe('2');
    expect(unsent.get('q')?.id).toBe('3');
    expect(unsent.get('b')?.id).toBe('4');
  });

  it('sums up an answer, labels Send, and labels Approve', () => {
    expect(answerSummary(answer({ choiceLabel: 'Repo', text: ' why ' }))).toBe('Repo · why');
    expect(answerSummary(answer({ yourCall: true }))).toBe('Your call');
    expect(answerSummary(answer({ text: 'Mine' }))).toBe('Mine');
    expect(sendLabel([answer({}), answer({ questionId: DESIGN_FEEDBACK_ID })])).toBe('Send 1 answer');
    expect(sendLabel([answer({ questionId: DESIGN_FEEDBACK_ID })])).toBe('Send feedback');
    expect(approveLabel(0)).toBe('Approve design');
    expect(approveLabel(1)).toBe('Approve with 1 answer');
    expect(approveLabel(3)).toBe('Approve with 3 answers');
  });
});
