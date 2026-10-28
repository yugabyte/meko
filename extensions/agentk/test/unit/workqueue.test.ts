import { describe, expect, it } from 'vitest';
import { parseWorkLink } from '../../src/shared/worklinks.js';
import { get, kvGet, openDb, run } from '../../src/server/db.js';
import type { ReviewRequest } from '../../src/server/github.js';
import type { AskRun } from '../../src/server/askagentk.js';
import { readSlackLater, slackArgs, type SlackLater } from '../../src/server/slack.js';
import { checkStartItem, setItemSession, WorkQueue, type WorkQueueDeps } from '../../src/server/workqueue.js';

describe('work item links', () => {
  it.each([
    ['https://github.com/Yugabyte/yugabyte-db/pull/29811', 'github', 'gh:yugabyte/yugabyte-db#29811', 'yugabyte-db#29811'],
    ['https://github.com/yugabyte/yugabyte-db/issues/29811#issuecomment-1', 'github', 'gh:yugabyte/yugabyte-db#29811', 'yugabyte-db#29811'],
    ['yugabyte/yugabyte-db#29811', 'github', 'gh:yugabyte/yugabyte-db#29811', 'yugabyte-db#29811'],
    ['https://yugabyte.atlassian.net/browse/DB-18234', 'jira', 'jira:DB-18234', 'DB-18234'],
    ['https://yugabyte.atlassian.net/jira/software/projects/DB/boards/1?selectedIssue=DB-18234', 'jira', 'jira:DB-18234', 'DB-18234'],
    ['DB-18234', 'jira', 'jira:DB-18234', 'DB-18234'],
    ['https://acme.slack.com/archives/C024BE91L/p1700000000123456', 'slack', 'slack:C024BE91L/p1700000000123456', 'Slack'],
    ['https://acme.slack.com/archives/C024BE91L/p1700000000123456?thread_ts=1700000000.000100&cid=C024BE91L', 'slack', 'slack:C024BE91L/p1700000000123456', 'Slack'],
    ['https://www.example.com/doc', 'other', 'url:https://www.example.com/doc', 'example.com'],
  ])('reads %s', (input, kind, key, label) => {
    expect(parseWorkLink(input)).toMatchObject({ kind, key, label });
  });

  it.each(['fix the login', 'db-12', 'ftp://example.com/x', ''])('turns away %j', (input) => {
    expect(parseWorkLink(input)).toBeNull();
  });
});

function setup(o: { reviews?: ReviewRequest[]; slack?: SlackLater[] | Error; claude?: boolean; title?: string } = {}) {
  const db = openDb(':memory:');
  run(db, `INSERT INTO projects(id, name, repo_path, default_branch, created_at) VALUES ('p', 'P', '/r', 'main', 1)`);
  const session = (id: string, desired = 'running') =>
    run(db, `INSERT INTO sessions(id, project_id, slug, name, claude_session_id, branch, base_branch, worktree_path, tmux_name, hook_token,
             desired, status, stage, created_at) VALUES (?, 'p', ?, ?, 'c', ?, 'main', '/w', 't', ?, ?, 'idle', 'code', 1)`,
      id, id, `Session ${id}`, `ak/${id}`, `tok-${id}`, desired);
  const state = { reviews: o.reviews ?? [], slack: o.slack ?? ([] as SlackLater[] | Error), slackReads: 0, changes: 0 };
  const deps: WorkQueueDeps = {
    db,
    changed: () => void state.changes++,
    warn: () => undefined,
    reviewRequests: async () => state.reviews,
    readSlack: async () => {
      state.slackReads++;
      if (state.slack instanceof Error) throw state.slack;
      return state.slack;
    },
    claudeAvailable: () => o.claude ?? true,
    githubTitle: async () => o.title ?? 'Title from GitHub',
  };
  return { db, q: new WorkQueue(deps), session, state };
}

const titles = (q: WorkQueue) => q.list().map((i) => i.title);

describe('the work queue', () => {
  it('adds items at the bottom, reads a missing title from GitHub, and keeps each link once', async () => {
    const { q } = setup();
    await q.create({ title: 'First', links: ['DB-1', 'https://yugabyte.atlassian.net/browse/DB-1'] });
    const second = await q.create({ links: ['https://github.com/o/r/pull/7'] });
    expect(titles(q)).toEqual(['First', 'Title from GitHub']);
    expect(q.list()[0].links.map((l) => l.key)).toEqual(['jira:DB-1']);
    expect(second).toMatchObject({ source: 'manual', unplaced: false });
    await expect(q.create({ links: ['DB-2'] })).rejects.toThrow(/title/);
    await expect(q.create({ title: 'x', links: ['not a link'] })).rejects.toThrow(/isn't a link/);
  });

  it('moves, reorders, closes and reopens items', async () => {
    const { q } = setup();
    for (const t of ['a', 'b', 'c']) await q.create({ title: t });
    const id = (t: string) => q.list().find((i) => i.title === t)!.id;
    q.update(id('c'), { move: 'top' });
    expect(titles(q)).toEqual(['c', 'a', 'b']);
    q.update(id('c'), { move: 'down' });
    expect(titles(q)).toEqual(['a', 'c', 'b']);
    q.reorder({ ids: [id('b'), id('a'), id('c')] });
    expect(titles(q)).toEqual(['b', 'a', 'c']);
    expect(() => q.reorder({ ids: [id('b'), id('a')] })).toThrow(/changed meanwhile/);
    q.update(id('b'), { status: 'closed' });
    expect(titles(q)).toEqual(['a', 'c']);
    expect(q.closed().map((i) => i.title)).toEqual(['b']);
    q.update(q.closed()[0].id, { status: 'open' });
    expect(titles(q)).toEqual(['a', 'c', 'b']);
  });

  it.each([
    ['a session moves off its other item', 's1', 'B', ['B']],
    ['an item replaces its session', 's2', 'A', ['A']],
  ])('links one to one: %s', async (_what, sessionId, to, want) => {
    const { q, db, session } = setup();
    session('s1');
    session('s2');
    const a = await q.create({ title: 'A' });
    const b = await q.create({ title: 'B' });
    q.update(a.id, { sessionId: 's1' });
    q.update(to === 'A' ? a.id : b.id, { sessionId });
    const holding = q.list().filter((i) => i.sessionId === sessionId).map((i) => i.title);
    expect(holding).toEqual(want);
    expect(get<{ n: number }>(db, 'SELECT COUNT(*) AS n FROM work_items WHERE session_id IS NOT NULL')!.n).toBe(1);
  });

  it('finds an item and its session by any spelling of a link, closed ones last', async () => {
    const { q, db, session } = setup();
    session('s1', 'archived');
    const a = await q.create({ title: 'Fix split', links: ['https://github.com/o/r/pull/9'] });
    setItemSession(db, a.id, 's1');
    const b = await q.create({ title: 'Also on it', links: ['o/r#9'] });
    q.update(a.id, { status: 'closed' });
    const found = q.byLink('https://github.com/O/R/pull/9/files');
    expect(found.map((m) => m.item.title)).toEqual(['Also on it', 'Fix split']);
    expect(found[1].session).toMatchObject({ id: 's1', closed: true });
    expect(q.soleItemFor('o/r#9')).toBe(b.id);
    expect(() => q.byLink('no link here')).toThrow(/isn't a link/);
  });

  it('checks the item a new session is for', async () => {
    const { q, db } = setup();
    const a = await q.create({ title: 'A' });
    expect(checkStartItem(db, a.id)).toBe(a.id);
    expect(checkStartItem(db, undefined)).toBeNull();
    q.update(a.id, { status: 'closed' });
    expect(() => checkStartItem(db, a.id)).toThrow(/closed/);
    expect(() => checkStartItem(db, 'nope')).toThrow(/no longer exists/);
  });
});

describe('syncing the work queue', () => {
  const pr = (n: number): ReviewRequest => ({ repo: 'o/r', number: n, title: `PR ${n}`, url: `https://github.com/o/r/pull/${n}` });
  const msg = (ts: string, text: string): SlackLater => ({ url: `https://acme.slack.com/archives/C1/p${ts}`, text, channel: 'db-dev' });

  it('adds review requests and Later messages at the bottom, new, and never again once closed', async () => {
    const { q, state } = setup({ reviews: [pr(1), pr(2)], slack: [msg('1700000000000001', 'can you look at\nthe flaky test')] });
    await q.create({ title: 'Mine', links: ['o/r#2'] });
    const r = await q.sync(true);
    expect(r).toMatchObject({ github: { ok: true, added: 1 }, slack: { ok: true, added: 1 }, slackAuto: true });
    expect(titles(q)).toEqual(['Mine', 'Review o/r#1: PR 1', 'can you look at']);
    expect(q.list().slice(1).every((i) => i.unplaced)).toBe(true);
    expect(q.list()[2].links[0].label).toBe('Slack #db-dev');
    q.update(q.list()[1].id, { status: 'closed' });
    q.update(q.list()[1].id, { placed: true });
    await q.sync(true);
    expect(titles(q)).toEqual(['Mine', 'can you look at']);
    expect(q.list()[1].unplaced).toBe(false);
    expect(state.slackReads).toBe(2);
  });

  it('marks a review request done on GitHub when it is gone, and not once it is back', async () => {
    const { q, state } = setup({ reviews: [pr(1)] });
    await q.sync(false);
    state.reviews = [];
    await q.sync(false);
    expect(q.list()[0].githubDone).toBe(true);
    state.reviews = [pr(1)];
    await q.sync(false);
    expect(q.list()[0].githubDone).toBe(false);
  });

  it('reads Slack on the timer only after a Sync reached it, and stops after a failure', async () => {
    const { q, db, state } = setup({ reviews: [pr(1)], slack: new Error('no Slack tools') });
    await q.sync(false);
    expect(state.slackReads).toBe(0);
    const r = await q.sync(true);
    expect(r).toMatchObject({ github: { ok: true }, slack: { ok: false, error: 'no Slack tools' }, slackAuto: false });
    state.slack = [];
    await q.sync(true);
    expect(kvGet(db, 'work-slack-auto')).toBe('1');
    await q.sync(false);
    expect(state.slackReads).toBe(3);
    state.slack = new Error('timed out');
    await q.sync(false);
    await q.sync(false);
    expect(state.slackReads).toBe(4);
  });

  it('says Slack needs Claude Code without running it', async () => {
    const { q, state } = setup({ claude: false });
    expect((await q.sync(true)).slack).toMatchObject({ ok: false, error: expect.stringMatching(/needs Claude Code/) });
    expect(state.slackReads).toBe(0);
  });
});

describe("reading Slack's Later list", () => {
  const ran = (out: unknown, o: Partial<AskRun> = {}): AskRun => ({ stdout: JSON.stringify(out), stderr: '', code: 0, spawnError: null, timedOut: false, aborted: false, ...o });

  it('loads your connectors but lets the run only read Slack', () => {
    const args = slackArgs('sonnet', ['mcp__claude_ai_Slack__slack_search_public_and_private']);
    expect(args).not.toContain('--strict-mcp-config');
    expect(args.slice(args.indexOf('--allowedTools') + 1, args.indexOf('--disallowedTools'))).toEqual(['mcp__claude_ai_Slack__slack_search_public_and_private']);
    expect(args).toEqual(expect.arrayContaining(['mcp__claude_ai_Slack__slack_send_message', 'Bash']));
    expect(slackArgs('', [])).not.toContain('--allowedTools');
  });

  it.each([
    ['the messages it found', ran({ structured_output: { found: true, items: [{ url: 'u', text: ' hi ', channel: 'c' }, { text: 'no url' }] } }), [{ url: 'u', text: 'hi', channel: 'c' }]],
    ['that it could not reach the list', ran({ structured_output: { found: false, items: [] } }), /couldn't reach your Later list/],
    ['a sign-in it needs', ran({ is_error: true, result: 'Not logged in · Please run /login' }, { code: 1 }), /isn't signed in/],
    ['a timeout', ran({}, { timedOut: true, code: null }), /over 180 seconds/],
  ])('reads %s', (_what, r, want) => {
    if (want instanceof RegExp) expect(() => readSlackLater(r, 'claude', 180)).toThrow(want);
    else expect(readSlackLater(r, 'claude', 180)).toEqual(want);
  });
});
