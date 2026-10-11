import { describe, expect, it } from 'vitest';
import type { LinkPlan } from '../../src/shared/types.js';
import { filterItems, labelText, linkEntries, type LabelPart } from '../../src/web/palette.js';

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
    project: { id: 'w', name: 'Widget', why: 'datapack', clone: '/code/demo' }, newProject: null, existing: [],
  };
  const elsewhere = { project: null, newProject: { name: 'demo', repoPath: null, cloneUrl: 'https://github.com/acme/demo.git', datapack: null, reopens: null } };
  const rows = (plan: Partial<LinkPlan>) =>
    linkEntries({ ...base, ...plan }).map((e) => ({ label: labelText(e.label), hint: e.hint, action: e.action, clones: e.clones }));

  it.each<[string, Partial<LinkPlan>, ReturnType<typeof rows>]>([
    ['reviews a PR in the project naming its datapack', {}, [
      { label: 'Review #7 Add the widget in Widget', hint: 'Names its datapack', action: { start: null }, clones: false },
    ]],
    ['goes to the review already running instead', { existing: [{ sessionId: 's1', projectId: 'w', name: 'Review #7 Add the widget' }] }, [
      { label: 'Go to Review #7 Add the widget', hint: 'Already reviewing it', action: { go: { projectId: 'w', sessionId: 's1' } }, clones: false },
    ]],
    ['says why a merged PR has nothing to review', { state: 'merged' }, [
      { label: "PR #7 of acme/demo is merged, so there's nothing to review.", hint: '', action: null, clones: false },
    ]],
    ['opens a new project, cloning the repo', elsewhere, [
      { label: 'Review #7 Add the widget in a new project demo', hint: 'Opens a new project · Clones acme/demo', action: { start: null }, clones: true },
    ]],
    ['reopens a closed project', { project: null, newProject: { ...elsewhere.newProject, repoPath: '/code/demo', datapack: { id: 'd', name: 'adlc/proj/demo/widget', invite: false }, reopens: 'w' } }, [
      { label: 'Review #7 Add the widget in demo', hint: 'Opens the project again', action: { start: null }, clones: false },
    ]],
    ['works on an issue from Design or Code, after the session already on it', {
      kind: 'issue', state: 'closed', title: 'Dark mode', project: { id: 'w', name: 'Widget', why: 'repo', clone: null },
      existing: [{ sessionId: 's2', projectId: 'w', name: '#7 Dark mode' }],
    }, [
      { label: 'Go to #7 Dark mode', hint: 'Already working on it', action: { go: { projectId: 'w', sessionId: 's2' } }, clones: false },
      { label: 'Work on issue #7 Dark mode in Widget', hint: 'Closed issue · Starts at Design · Same repo · Clones acme/demo', action: { start: 'design' }, clones: true },
      { label: 'Work on issue #7 in Widget, straight to Code', hint: 'Closed issue · Starts at Code · Same repo · Clones acme/demo', action: { start: 'code' }, clones: true },
    ]],
    ['says a new project gets a new datapack', {
      kind: 'issue', title: 'Dark mode', ...elsewhere, newProject: { ...elsewhere.newProject, repoPath: '/code/demo', datapack: 'create' },
    }, [
      { label: 'Work on issue #7 Dark mode in a new project demo', hint: 'Starts at Design · Opens a new project · New datapack', action: { start: 'design' }, clones: false },
      { label: 'Work on issue #7 in a new project demo, straight to Code', hint: 'Starts at Code · Opens a new project · New datapack', action: { start: 'code' }, clones: false },
    ]],
  ])('%s', (_, plan, want) => {
    expect(rows(plan)).toEqual(want);
  });
});
