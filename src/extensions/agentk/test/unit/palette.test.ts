import { describe, expect, it } from 'vitest';
import type { AskAgentkAction, JoinByLinkResult, LinkPlan, ProjectWithSessions, SessionMatch, WorkItem, WorkLinkMatch } from '../../src/shared/types.js';
import { askEntries, doAllActions, filterItems, findEntries, joinEntry, joinedText, labelText, linkEntries, tourEntries, workLinkEntries, type LabelPart } from '../../src/web/palette.js';

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

describe('the Kode tour', () => {
  const found = (tour: string | null, q: string) => filterItems(tourEntries(tour), q).map((t) => t.kind);

  it('offers Start the Kode tour while none is open, found however it is asked for', () => {
    expect(tourEntries(null).map((t) => labelText(t.label))).toEqual(['Start the Kode tour']);
    for (const q of ['tour', 'start the tour again', 'restart tour', 'begin the tour']) expect(found(null, q)).toEqual(['start']);
  });

  it('offers Go to and Start over while one is open, with taking it again finding only Start over', () => {
    expect(tourEntries('p1').map((t) => [t.kind, labelText(t.label)])).toEqual([
      ['go', 'Go to the Kode tour'],
      ['over', 'Start the Kode tour over'],
    ]);
    expect(found('p1', 'tour')).toEqual(['go', 'over']);
    for (const q of ['start the tour again', 'restart tour', 'tour over', 'reset the tour', 'start the tour']) expect(found('p1', q)).toEqual(['over']);
    expect(found('p1', 'go to tour')).toEqual(['go']);
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

describe('a pasted Meko share link', () => {
  it('offers to join it, and says what joining came to when there is nothing to open', () => {
    expect(joinEntry(' https://cloud.mekodata.ai/share/abc ', 'https://cloud.mekodata.ai')?.id).toBe('join-link');
    const r: JoinByLinkResult = { datapackId: 'd', name: 'adlc/proj/x/y', grant: 'viewer', alreadyMember: false, invite: null, project: null };
    expect(joinedText(r)).toMatch(/^You joined adlc\/proj\/x\/y as viewer\. A viewer can’t open it as a project/);
    expect(joinedText({ ...r, name: null })).toBe('You joined the datapack as viewer. It shows in Open project once Meko lists it.');
    expect(joinedText({ ...r, grant: 'contributor', alreadyMember: true })).toMatch(/^You already have access to adlc\/proj\/x\/y\. Pick it in Open project/);
  });
});

describe("Kode's proposed actions", () => {
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
    [{ type: 'link', ref: 'acme/app#5', projectId: 'p1', start: 'design' }, [['Work on acme/app#5 in agent-k-local', 'Looks up the GitHub link', false]]],
    [{ type: 'openProject', repo: 'acme/app', name: 'App', datapack: 'new' }, [['Open a project on acme/app named App', 'With a new datapack', false]]],
  ])('names %j', (a, want) => {
    expect(rows(a)).toEqual(want);
  });

  it('says a session runs to merge in its Go to hint', () => {
    const on = [{ ...projects[0], sessions: [{ id: 's1', name: 'Dark mode', stage: 'review', autoRun: true }] }] as unknown as ProjectWithSessions[];
    const [go] = askEntries([{ type: 'go', view: 'session', projectId: 'p1', sessionId: 's1' }], on);
    expect(go.hint).toBe('agent-k-local · Pull Request · Run to merge');
  });

  it('offers Do all for two or more steps of one instruction, leaving out the go actions', () => {
    const go: AskAgentkAction = { type: 'go', view: 'usage' };
    const move: AskAgentkAction = { type: 'move', sessionId: 's1', stage: 'code', instructions: '' };
    const link: AskAgentkAction = { type: 'link', ref: 'acme/app#5', projectId: 'p1' };
    expect(doAllActions([link, go, move], true)).toEqual([link, move]);
    expect(doAllActions([link, move], false)).toEqual([]);
    expect(doAllActions([link, go], true)).toEqual([]);
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

describe('work items holding a pasted link', () => {
  const projects = [{ id: 'p1', name: 'agent-k-local' }, { id: 'p2', name: 'docs' }] as unknown as ProjectWithSessions[];
  const item = (id: string, title: string, status: 'open' | 'closed' = 'open') => ({ id, title, status } as WorkItem);
  const session = (s: Partial<NonNullable<WorkLinkMatch['session']>>) => ({ id: 's1', projectId: 'p1', name: 'Fix split', stage: 'code' as const, closed: false, prNumber: null, prUrl: null, ...s });

  it('lists the sessions first, then each item, with Start session for one with no open session', () => {
    const entries = workLinkEntries([
      { item: item('a', 'Split race'), session: session({}) },
      { item: item('b', 'Docs for split'), session: session({ id: 's2', projectId: 'p2', name: 'Old docs', closed: true, prNumber: 4, prUrl: 'https://github.com/o/d/pull/4' }) },
      { item: item('c', 'Done', 'closed'), session: null },
    ], projects, 'p1');
    expect(entries.map((e) => [labelText(e.label), e.hint, e.action])).toEqual([
      ['Fix split', 'Code · Work item: Split race', { go: { projectId: 'p1', sessionId: 's1' } }],
      ['Old docs', 'docs · Closed · PR #4 · Work item: Docs for split', { open: 'https://github.com/o/d/pull/4' }],
      ['Work item: Split race', 'Work queue', { queue: 'a' }],
      ['Work item: Docs for split', 'Work queue', { queue: 'b' }],
      ['Start a session for Docs for split…', 'Action', { start: item('b', 'Docs for split') }],
      ['Work item: Done', 'Dismissed work item', { queue: 'c' }],
    ]);
  });
});
