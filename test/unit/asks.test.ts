import { describe, expect, it } from 'vitest';
import type { AskQuestion, Session } from '../../src/shared/types.js';
import { askAnswers, parseAsk } from '../../src/server/asks.js';
import { MAX_QUESTIONS_FILE, checkToolQuestions } from '../../src/server/design.js';
import { askDetail } from '../../src/server/sessions.js';
import { designApprovalBlocker, designReviseBlocker, heldForTurnEnd } from '../../src/web/util.js';

const COLOR = { question: 'Which color?', header: 'Color', options: [{ label: 'Red', description: 'Warm' }, { label: 'Blue', description: 'Cool', preview: '#00f' }], multiSelect: false };
const SIDES = { question: 'Which sides?', header: 'Sides', options: [{ label: 'Fries' }, { label: 'Salad' }, { label: 'Soup' }], multiSelect: true };

describe("Claude's AskUserQuestion questions", () => {
  it('are checked and kept for the browser, labels whole', () => {
    expect(parseAsk([COLOR, SIDES])).toEqual<AskQuestion[]>([
      {
        question: 'Which color?', header: 'Color', multiSelect: false,
        options: [{ label: 'Red', description: 'Warm', preview: null }, { label: 'Blue', description: 'Cool', preview: '#00f' }],
      },
      {
        question: 'Which sides?', header: 'Sides', multiSelect: true,
        options: ['Fries', 'Salad', 'Soup'].map((label) => ({ label, description: null, preview: null })),
      },
    ]);
    const long = parseAsk([{ ...COLOR, question: 'Q'.repeat(2000), options: [{ label: 'A', preview: 'p'.repeat(9000) }] }])[0];
    expect(long.question.length).toBe(1001);
    expect(long.options[0].preview!.length).toBe(8001);
  });

  it("refuse what AgentK can't show as Claude's dialog does, leaving it to the terminal", () => {
    expect(() => parseAsk([])).toThrow(/1 to 4/);
    expect(() => parseAsk([COLOR, COLOR, COLOR, COLOR, COLOR])).toThrow(/1 to 4/);
    expect(() => parseAsk('Which?')).toThrow(/1 to 4/);
    expect(() => parseAsk([{ ...COLOR, kind: 'text' }])).toThrow(/isn't a choice/);
    expect(() => parseAsk([{ ...COLOR, options: [] }])).toThrow(/1 to 6 options/);
    expect(() => parseAsk([{ ...COLOR, options: [{ label: '' }] }])).toThrow(/needs a label/);
    expect(() => parseAsk([{ ...COLOR, options: [{ label: 'A' }, { label: 'A' }] }])).toThrow(/two options labelled A/);
    expect(() => parseAsk([{ ...COLOR, question: ' ' }])).toThrow(/question 1 must be text/);
    expect(parseAsk([{ ...COLOR, kind: 'choice' }])).toHaveLength(1);
  });

  it('name the first question in the status, shortened', () => {
    expect(askDetail('Which\n color?')).toBe('AgentK asks: Which color?');
    expect(askDetail('x'.repeat(300))).toHaveLength('AgentK asks: '.length + 120);
  });
});

describe('answers to them from the browser', () => {
  const questions = parseAsk([COLOR, SIDES]);

  it('come out as Claude\'s dialog gives them: labels comma-joined, then the user\'s own words', () => {
    expect(askAnswers({ answers: [{ question: 0, labels: ['Blue'], text: '' }] }, questions)).toEqual({ 0: 'Blue' });
    expect(askAnswers({ answers: [{ question: 1, labels: ['Fries', 'Soup'], text: ' and bread ' }, { question: 0, labels: [], text: 'Green' }] }, questions))
      .toEqual({ 1: 'Fries, Soup, and bread', 0: 'Green' });
  });

  it("refuse answers that don't fit the questions", () => {
    const check = (body: unknown) => () => askAnswers(body, questions);
    expect(check(null)).toThrow(expect.objectContaining({ status: 400 }));
    expect(check({ answers: [] })).toThrow(/at least one/);
    expect(check({ answers: [{ question: 2, labels: ['Red'] }] })).toThrow(/isn't asking/);
    expect(check({ answers: [{ question: 0, labels: ['Red', 'Blue'] }] })).toThrow(/Pick one option/);
    expect(check({ answers: [{ question: 0, labels: ['Green'] }] })).toThrow(/Green isn't one of the options/);
    expect(check({ answers: [{ question: 1, labels: ['Fries', 'Fries'] }] })).toThrow(/picked twice/);
    expect(check({ answers: [{ question: 0, labels: [], text: '  ' }] })).toThrow(/Pick an option/);
    expect(check({ answers: [{ question: 0, labels: ['Red'] }, { question: 0, labels: ['Blue'] }] })).toThrow(/two answers/);
    expect(check({ answers: [{ question: 0, labels: [], text: 'x'.repeat(10_001) }] })).toThrow(/at most/);
    expect(check({ answers: [{ question: 0, labels: 'Red' }] })).toThrow(/list of labels/);
  });
});

describe('questions from the design_questions tool', () => {
  it('are normalized as a questions file is', () => {
    const r = checkToolQuestions([
      { id: 'Store It', question: 'Where do designs live?', options: [{ label: 'Datapack', detail: 'Shared' }, 'Repo'], recommended: 0 },
      { id: 'tabs', question: 'Tabs?', status: 'resolved', decision: 'One column.' },
    ]);
    expect(r).toEqual({
      questions: [
        { id: 'store-it', question: 'Where do designs live?', context: null, options: [{ label: 'Datapack', detail: 'Shared' }, { label: 'Repo', detail: null }], recommended: 0, status: 'open', decision: null },
        { id: 'tabs', question: 'Tabs?', context: null, options: [], recommended: null, status: 'resolved', decision: 'One column.' },
      ],
    });
    expect(checkToolQuestions([])).toEqual({ questions: [] });
  });

  it("say what's wrong, for Claude to fix in the same turn", () => {
    const error = (input: unknown) => {
      const r = checkToolQuestions(input);
      return 'error' in r ? r.error : null;
    };
    expect(error({ questions: [] })).toMatch(/must be a list/);
    expect(error([{ question: 'Where?' }])).toMatch(/Question 1 needs an "id"/);
    expect(error([{ id: 'a', question: 'A?' }, { id: 'A', question: 'B?' }])).toMatch(/Question 2 repeats the id "a"/);
    expect(error([{ id: 'a' }])).toMatch(/needs its "question" text/);
    expect(error([{ id: 'a', question: 'A?', options: 'x' }])).toMatch(/"options" must be a list/);
    expect(error([{ id: 'a', question: 'A?', options: [{ detail: 'no label' }] }])).toMatch(/each option needs a "label"/);
    expect(error([{ id: 'a', question: 'A?', options: ['x'], recommended: 1 }])).toMatch(/"recommended" must be the index/);
    expect(error([{ id: 'a', question: 'A?', status: 'done' }])).toMatch(/"status" must be "open" or "resolved"/);
    expect(error(Array.from({ length: 31 }, (_, i) => ({ id: `q${i}`, question: '?' })))).toMatch(/at most 30/);
    // Too much for the questions file to be read back.
    const big = Array.from({ length: 30 }, (_, i) => ({
      id: `q${i}`, question: 'q'.repeat(1000), context: 'c'.repeat(4000), options: Array.from({ length: 6 }, (_, j) => ({ label: `${j}`.repeat(200), detail: 'd'.repeat(1000) })),
    }));
    expect(error(big)).toMatch(new RegExp(`more than ${MAX_QUESTIONS_FILE / 1024} KB`));
  });
});

describe('approving and revising while AgentK is busy, in the browser', () => {
  const session = (over: Partial<Session>) => ({
    hasDesign: true, status: 'idle', statusDetail: null, modLive: false, midTurn: false, promptsHeld: 0, approvalQueued: false, ...over,
  }) as Session;
  const working = session({ status: 'working', midTurn: true });
  const permission = session({ status: 'needs_input', statusDetail: 'Permission needed for Bash', midTurn: true });

  it('waits for the turn to end without the mod', () => {
    expect(heldForTurnEnd(working)).toBe(false);
    expect(designApprovalBlocker(working)).toMatch(/Wait for AgentK/);
    expect(designReviseBlocker(working)).toMatch(/Wait for AgentK/);
    expect(designApprovalBlocker(permission)).toMatch(/waiting on you in the terminal/);
    expect(designReviseBlocker(permission)).toMatch(/waiting on you in the terminal/);
    expect(designApprovalBlocker(session({ status: 'needs_input', statusDetail: 'Design ready for your approval' }))).toBeNull();
  });

  it('goes ahead mid-turn with the mod: the approval waits here, the answers in Claude', () => {
    for (const s of [{ ...working, modLive: true }, { ...permission, modLive: true }]) {
      expect(heldForTurnEnd(s)).toBe(true);
      expect(designApprovalBlocker(s)).toBeNull();
      expect(designReviseBlocker(s)).toBeNull();
    }
    expect(designApprovalBlocker(session({ status: 'starting', modLive: true }))).toMatch(/Wait for AgentK/);
    expect(designApprovalBlocker(session({ status: 'exited', modLive: true }))).toMatch(/Resume/);
  });

  it("doesn't queue an approval for a Needs you with no turn behind it", () => {
    const parked = session({ status: 'needs_input', statusDetail: 'Recovered twice in the last hour; waiting for you before continuing', modLive: true });
    expect(heldForTurnEnd(parked)).toBe(false);
    expect(designApprovalBlocker(parked)).toMatch(/waiting on you in the terminal/);
    expect(designReviseBlocker(parked)).toMatch(/waiting on you in the terminal/);
  });

  it("treats Claude's question as one, even when it mentions a plan", () => {
    const ask = session({ status: 'needs_input', statusDetail: 'AgentK asks: Which plan should I follow?', midTurn: true });
    expect(designApprovalBlocker(ask)).toMatch(/waiting on you in the terminal/);
    expect(designReviseBlocker(ask)).toMatch(/waiting on you in the terminal/);
    // Plan mode's own approval dialog still lets the approval through.
    expect(designApprovalBlocker(session({ status: 'needs_input', statusDetail: 'Claude has a plan for you to approve' }))).toBeNull();
  });

  it('waits for answers Claude holds before approving', () => {
    const held = session({ status: 'needs_input', statusDetail: 'Design ready for your approval', modLive: true, promptsHeld: 1 });
    expect(designApprovalBlocker(held)).toMatch(/answers from you to take first/);
    expect(designApprovalBlocker({ ...working, modLive: true, promptsHeld: 1 })).toMatch(/answers from you to take first/);
  });

  it('holds both while an approval is queued', () => {
    const queued = session({ status: 'working', midTurn: true, modLive: true, approvalQueued: true });
    expect(designApprovalBlocker(queued)).toMatch(/when this turn ends/);
    expect(designReviseBlocker(queued)).toMatch(/Cancel the approval/);
  });
});
