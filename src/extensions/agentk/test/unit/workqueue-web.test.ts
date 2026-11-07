import { describe, expect, it } from 'vitest';
import type { ProjectWithSessions, WorkItem, WorkSync } from '../../src/shared/types.js';
import { parseWorkLink } from '../../src/shared/worklinks.js';
import { itemProject, itemSession, itemStartRequest, movedOrder, queueBadge, queueSummary, workSyncText } from '../../src/web/workqueue.js';

const links = (...l: string[]) => l.map((x) => parseWorkLink(x)!);
const item = (o: Partial<WorkItem>): WorkItem => ({
  id: 'w1', title: 'Fix split race', notes: '', links: [], status: 'open', source: 'manual', sessionId: null, unplaced: false, githubDone: false,
  createdAt: 1, closedAt: null, ...o,
});
const projects = [
  { id: 'p1', name: 'One', sessions: [{ id: 's1', desired: 'running', status: 'needs_input', prUrl: null, background: [] }] },
  { id: 'p2', name: 'Two', sessions: [{ id: 's2', desired: 'running', status: 'working', prUrl: 'https://github.com/O/R/pull/3', background: [] }] },
] as unknown as ProjectWithSessions[];

describe('the work queue page', () => {
  it.each([
    ['a task at Design with the item as its prompt', item({ notes: 'Seen on CI', links: links('DB-1', 'o/r#9') }),
      { name: 'Fix split race', kind: 'task', start: 'design', prompt: 'Fix split race\n\nSeen on CI\n\nLinks:\nDB-1\nhttps://github.com/o/r/issues/9', workItemId: 'w1' }],
    ['a review of a GitHub review request', item({ source: 'github', links: links('https://github.com/o/r/pull/9') }),
      { name: '', kind: 'review-pr', pr: { repo: 'o/r', number: 9 }, workItemId: 'w1' }],
  ])('starts %s', (_what, it_, want) => {
    expect(itemStartRequest(it_)).toEqual(want);
  });

  it.each([
    ['the project with a PR on the repo', links('o/r#9'), 'p1', 'p2'],
    ['the project on screen', links('x/y#1'), 'p1', 'p1'],
    ['the first project', [], null, 'p1'],
  ])('starts in %s', (_what, l, current, want) => {
    expect(itemProject(item({ links: l }), projects, current)).toBe(want);
  });

  it("shows an item's open session, and the queue's open and new counts", () => {
    const items = [item({ sessionId: 's2' }), item({ id: 'w2', sessionId: 's1', unplaced: true }), item({ id: 'w3', sessionId: 'gone' })];
    expect(itemSession(items[2], projects)).toBeNull();
    expect(queueSummary(items)).toEqual({ open: 3, unplaced: 1 });
  });

  it.each([[1, '1'], [99, '99'], [100, '99+']])('counts %i open items on the rail button as %s', (n, want) => {
    expect(queueBadge(n)).toBe(want);
  });

  it('moves an item to where it was dropped', () => {
    expect(movedOrder(['a', 'b', 'c'], 'c', 0)).toEqual(['c', 'a', 'b']);
    expect(movedOrder(['a', 'b', 'c'], 'a', 9)).toEqual(['b', 'c', 'a']);
  });

  it.each<[Partial<WorkSync>, string]>([
    [{}, 'Not synced since Kode started'],
    [{ at: 0, github: { ok: true, added: 2, error: null }, slack: null }, 'Synced just now · GitHub: 2 new · Slack: only when you press Sync'],
    [{ at: 0, github: { ok: false, added: 0, error: 'gh is signed out' }, slack: { ok: true, added: 0, error: null }, slackAuto: true }, 'Synced just now · GitHub: gh is signed out · Slack: nothing new'],
    [{ sources: { github: false, slack: false } }, 'Sync is off (Settings › Work queue)'],
  ])('says how the last sync went: %j', (s, want) => {
    const base: WorkSync = { running: false, at: null, github: null, slack: null, slackAuto: false, sources: { github: true, slack: true } };
    expect(workSyncText({ ...base, ...s, ...(s.at === 0 ? { at: 1000 } : {}) }, 1000)).toBe(want);
  });
});
