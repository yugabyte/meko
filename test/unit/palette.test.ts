import { describe, expect, it } from 'vitest';
import type { AskAgentkAction, LinkPlan, ProjectWithSessions, SessionMatch } from '../../src/shared/types.js';
import { askEntries, filterItems, findEntries, labelText, linkEntries, type LabelPart } from '../../src/web/palette.js';

const item = (id: string, label: LabelPart[], keywords = '') => ({ id, label, keywords });
const items = [
  item('move', ['Move ', { session: 'Settings page' }, ' to another stage'], 'change stage'),
  item('s1', [{ session: 'Fix login' }], 'agent-k-local Fix login ak/fix-login'),
  item('home', [{ project: 'agent-k-local' }, ': project home']),
  item('settings', ['Settings'], 'settings password'),
];
const ids = (q: string) => filterItems(items, q).map((it) => it.id);

describe('command palette labels', () => {
  it('reads a label with names in it as plain text', () => {
    expect(labelText(items[0].label)).toBe('Move Settings page to another stage');
    expect(labelText(items[2].label)).toBe('agent-k-local: project home');
    expect(labelText(['Usage'])).toBe('Usage');
  });

  it('finds items by the names in their labels and by their keywords', () => {
    expect(ids('')).toEqual(['move', 's1', 'home', 'settings']);
    expect(ids('settings')).toEqual(['move', 'settings']);
    expect(ids('move settings page')).toEqual(['move']);
    expect(ids('page settings move')).toEqual(['move']);
    expect(ids('AGENT-K-LOCAL home')).toEqual(['home']);
    expect(ids('fix-login')).toEqual(['s1']);
    expect(ids('nothing like this')).toEqual([]);
  });
});

describe('a pasted GitHub link', () => {
  const base: LinkPlan = {
    kind: 'pr', repo: 'acme/demo', number: 7, title: 'Add the widget', url: 'https://github.com/acme/demo/pull/7', author: 'dana', state: 'open',
    project: { id: 'w', name: 'Widget', why: 'datapack', clone: '/code/demo' }, others: [], newProject: null, existing: [],
  };
  const elsewhere = { project: null, newProject: { name: 'demo', repoPath: null, cloneUrl: 'https://github.com/acme/demo.git', datapack: null, reopens: null } };
  const rows = (plan: Partial<LinkPlan>) =>
    linkEntries({ ...base, ...plan }).map((e) => ({ label: labelText(e.label), hint: e.hint, action: e.action, clones: e.clones }));

  it.each<[string, Partial<LinkPlan>, ReturnType<typeof rows>]>([
    ['reviews a PR in the project naming its datapack', {}, [
      { label: 'Review #7 Add the widget in Widget', hint: 'Names its datapack', action: { start: null, projectId: 'w' }, clones: false },
    ]],
    ['reviews a PR in this project, or in another on the repo', {
      project: { id: 'w', name: 'Widget', why: 'current', clone: '/code/demo' }, others: [{ id: 'g', name: 'Gadget', why: 'repo', clone: '/code/demo' }],
    }, [
      { label: 'Review #7 Add the widget in Widget', hint: 'This project', action: { start: null, projectId: 'w' }, clones: false },
      { label: 'Review #7 Add the widget in Gadget', hint: 'Also on acme/demo', action: { start: null, projectId: 'g' }, clones: false },
    ]],
    ['goes to the review already running instead', { existing: [{ sessionId: 's1', projectId: 'w', name: 'Review #7 Add the widget' }] }, [
      { label: 'Go to Review #7 Add the widget', hint: 'Already reviewing it', action: { go: { projectId: 'w', sessionId: 's1' } }, clones: false },
    ]],
    ['says why a merged PR has nothing to review', { state: 'merged' }, [
      { label: "PR #7 of acme/demo is merged, so there's nothing to review.", hint: '', action: null, clones: false },
    ]],
    ['opens a new project, cloning the repo', elsewhere, [
      { label: 'Review #7 Add the widget in a new project demo', hint: 'Opens a new project · Clones acme/demo', action: { start: null, projectId: undefined }, clones: true },
    ]],
    ['reopens a closed project', { project: null, newProject: { ...elsewhere.newProject, repoPath: '/code/demo', datapack: { id: 'd', name: 'adlc/proj/demo/widget', invite: false }, reopens: 'w' } }, [
      { label: 'Review #7 Add the widget in demo', hint: 'Opens the project again', action: { start: null, projectId: undefined }, clones: false },
    ]],
    ['works on an issue from Design or Code, after the session already on it, then from Design in the other projects on the repo', {
      kind: 'issue', state: 'closed', title: 'Dark mode', project: { id: 'w', name: 'Widget', why: 'repo', clone: null },
      others: [{ id: 'g', name: 'Gadget', why: 'repo', clone: '/code/demo' }],
      existing: [{ sessionId: 's2', projectId: 'w', name: '#7 Dark mode' }],
    }, [
      { label: 'Go to #7 Dark mode', hint: 'Already working on it', action: { go: { projectId: 'w', sessionId: 's2' } }, clones: false },
      { label: 'Work on issue #7 Dark mode in Widget', hint: 'Closed issue · Starts at Design · On acme/demo · Clones acme/demo', action: { start: 'design', projectId: 'w' }, clones: true },
      { label: 'Work on issue #7 in Widget, straight to Code', hint: 'Closed issue · Starts at Code · On acme/demo · Clones acme/demo', action: { start: 'code', projectId: 'w' }, clones: true },
      { label: 'Work on issue #7 Dark mode in Gadget', hint: 'Closed issue · Starts at Design · Also on acme/demo', action: { start: 'design', projectId: 'g' }, clones: false },
    ]],
    ['says a new project gets a new datapack', {
      kind: 'issue', title: 'Dark mode', ...elsewhere, newProject: { ...elsewhere.newProject, repoPath: '/code/demo', datapack: 'create' },
    }, [
      { label: 'Work on issue #7 Dark mode in a new project demo', hint: 'Starts at Design · Opens a new project · New datapack', action: { start: 'design', projectId: undefined }, clones: false },
      { label: 'Work on issue #7 in a new project demo, straight to Code', hint: 'Starts at Code · Opens a new project · New datapack', action: { start: 'code', projectId: undefined }, clones: false },
    ]],
  ])('%s', (_, plan, want) => {
    expect(rows(plan)).toEqual(want);
  });
});

describe("AgentK's proposed actions", () => {
  const projects = [{ id: 'p1', name: 'agent-k-local', sessions: [{ id: 's1', name: 'Dark mode', stage: 'code' }] }] as unknown as ProjectWithSessions[];
  const rows = (a: AskAgentkAction) => askEntries([a], projects).map((e) => [labelText(e.label), e.hint, !!e.edit]);

  it.each<[AskAgentkAction, (string | boolean)[][]]>([
    [{ type: 'start', projectId: 'p1', start: 'design', name: 'Usage dark mode', prompt: '\nAdd dark mode to the Usage page.\nKeep the chart colors.' }, [
      ['Start Usage dark mode in agent-k-local at Design', 'Add dark mode to the Usage page.', false],
      ['Edit Usage dark mode in New session…', 'Change it before it starts', true],
    ]],
    [{ type: 'start', projectId: 'p1', start: 'code', name: 'Fix the login', prompt: 'Fix the login.' }, [
      ['Start Fix the login in agent-k-local at Code', 'Action', false],
      ['Edit Fix the login in New session…', 'Change it before it starts', true],
    ]],
    [{ type: 'go', view: 'session', projectId: 'p1', sessionId: 's1' }, [['Go to Dark mode', 'agent-k-local · Code', false]]],
    [{ type: 'openPr', sessionId: 's1', instructions: 'x'.repeat(100) }, [['Open PR for Dark mode', `${'x'.repeat(79)}…`, false]]],
    [{ type: 'move', sessionId: 'gone', stage: 'code', instructions: '' }, [['Move a session to Code', 'Action', false]]],
  ])('names %j', (a, want) => {
    expect(rows(a)).toEqual(want);
  });
});

describe('sessions a search found', () => {
  const projects = [{ id: 'p1', name: 'agent-k-local' }, { id: 'p2', name: 'docs' }] as unknown as ProjectWithSessions[];
  const match = (m: Partial<SessionMatch>): SessionMatch => ({
    sessionId: 's1', projectId: 'p1', name: 'Dark mode', stage: 'code', closed: false, prNumber: null, prUrl: null, field: 'name', snippet: '', ...m,
  });

  it.each<[Partial<SessionMatch>, string, unknown]>([
    [{}, 'Code', { go: { projectId: 'p1', sessionId: 's1' } }],
    [{ projectId: 'p2', field: 'task', snippet: 'Add dark mode…' }, 'docs · Code · Task: Add dark mode…', { go: { projectId: 'p2', sessionId: 's1' } }],
    [{ closed: true, prNumber: 7, prUrl: 'https://github.com/o/r/pull/7', field: 'design', snippet: '…the theme' }, 'Closed · PR #7 · Design: …the theme', { open: 'https://github.com/o/r/pull/7' }],
    [{ closed: true }, 'Closed', null],
  ])('describes %j', (m, hint, action) => {
    const [e] = findEntries([match(m)], projects, 'p1');
    expect([labelText(e.label), e.hint, e.action]).toEqual(['Dark mode', hint, action]);
  });
});
