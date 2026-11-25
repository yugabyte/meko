import { mkdtempSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { describe, expect, it } from 'vitest';
import { parseWorkLink } from '../../src/shared/worklinks.js';
import { all, openDb, run, schemaVersion } from '../../src/server/db.js';
import type { ReviewRequest } from '../../src/server/github.js';
import type { AskRun } from '../../src/server/askagentk.js';
import { DEFAULT_CONFIG } from '../../src/server/config.js';
import { readList, readSlackList, signInSettings, slackArgs, slackListId, slackPrompt, type SlackListItem } from '../../src/server/slack.js';
import { moveItem } from '../../src/shared/workorder.js';
import { setItemSession, WorkQueue, type WorkQueueDeps } from '../../src/server/workqueue.js';
import { AmpWorkStore, type WorkSql } from '../../src/server/workstore.js';
import { PGlite } from '@electric-sql/pglite';
import { freshPglite, pgliteSql } from '../pglite.js';

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

/** A database that stops answering while `down` is set, as AMP does when it can't be reached. */
function flaky(sql: WorkSql): WorkSql & { down: boolean } {
  const refuse = () => Object.assign(new Error('connect ECONNREFUSED 203.0.113.9:5432'), { code: 'ECONNREFUSED' });
  const f: WorkSql & { down: boolean } = {
    down: false,
    query: async (q, p) => {
      if (f.down) throw refuse();
      return sql.query(q, p);
    },
    tx: async (fn) => {
      if (f.down) throw refuse();
      return sql.tx(fn);
    },
    close: () => sql.close(),
  };
  return f;
}

/**
 * A queue on its own state.db, on a database of its own (or `pg`, shared with another queue: another of
 * the user's computers), unless `attach` is false: then it's as before set-up.
 */
async function setup(o: { reviews?: ReviewRequest[]; slack?: SlackListItem[] | Error; claude?: boolean; title?: string; pg?: PGlite; attach?: boolean; before?: (db: ReturnType<typeof openDb>) => void } = {}) {
  const db = openDb(':memory:');
  run(db, `INSERT INTO projects(id, name, repo_path, default_branch, created_at) VALUES ('p', 'P', '/r', 'main', 1)`);
  const session = (id: string, desired = 'running') =>
    run(db, `INSERT INTO sessions(id, project_id, slug, name, claude_session_id, branch, base_branch, worktree_path, tmux_name, hook_token,
             desired, status, stage, created_at) VALUES (?, 'p', ?, ?, 'c', ?, 'main', '/w', 't', ?, ?, 'idle', 'code', 1)`,
      id, id, `Session ${id}`, `ak/${id}`, `tok-${id}`, desired);
  o.before?.(db);
  const state = {
    reviews: o.reviews ?? [], slack: o.slack ?? ([] as SlackListItem[] | Error), slackReads: 0, changes: 0, reviewReads: 0, unreachable: 0,
    settings: { github: true, slack: true, syncMinutes: 15 },
  };
  const deps: WorkQueueDeps = {
    db,
    changed: () => void state.changes++,
    warn: () => undefined,
    reviewRequests: async () => {
      state.reviewReads++;
      return state.reviews;
    },
    readSlack: async () => {
      state.slackReads++;
      if (state.slack instanceof Error) throw state.slack;
      return state.slack;
    },
    claudeAvailable: () => o.claude ?? true,
    githubTitle: async () => o.title ?? 'Title from GitHub',
    settings: () => state.settings,
    notReady: () => 'Set it up in Settings › Work queue.',
    unreachable: () => void state.unreachable++,
  };
  const q = new WorkQueue(deps);
  const sql = flaky(pgliteSql(o.pg ?? (await freshPglite())));
  const store = new AmpWorkStore(sql);
  if (o.attach !== false) await q.attach(store, async () => (await store.migrate()).readOnly);
  return { db, q, session, state, sql, store };
}

const titles = (q: WorkQueue) => q.list().map((i) => i.title);

describe('the work queue', () => {
  it('adds items at the bottom, reads a missing title from GitHub, and keeps each link once', async () => {
    const { q } = await setup();
    await q.create({ title: 'First', links: ['DB-1', 'https://yugabyte.atlassian.net/browse/DB-1'] });
    const second = await q.create({ links: ['https://github.com/o/r/pull/7'] });
    expect(titles(q)).toEqual(['First', 'Title from GitHub']);
    expect(q.list()[0].links.map((l) => l.key)).toEqual(['jira:DB-1']);
    expect(second).toMatchObject({ source: 'manual', unplaced: false });
    await expect(q.create({ links: ['DB-2'] })).rejects.toThrow(/title/);
    await expect(q.create({ title: 'x', links: ['not a link'] })).rejects.toThrow(/isn't a link/);
  });

  it('moves, reorders, closes and reopens items', async () => {
    const { q } = await setup();
    for (const t of ['a', 'b', 'c']) await q.create({ title: t });
    const id = (t: string) => q.list().find((i) => i.title === t)!.id;
    await q.update(id('c'), { move: 'top' });
    expect(titles(q)).toEqual(['c', 'a', 'b']);
    await q.update(id('c'), { move: 'down' });
    expect(titles(q)).toEqual(['a', 'c', 'b']);
    await q.update(id('a'), { move: 'bottom' });
    expect(titles(q)).toEqual(['c', 'b', 'a']);
    await expect(q.update(id('a'), { move: 'sideways' as 'up' })).rejects.toThrow(/top or bottom/);
    await q.reorder({ ids: [id('b'), id('a'), id('c')] });
    expect(titles(q)).toEqual(['b', 'a', 'c']);
    await expect(q.reorder({ ids: [id('b'), id('a')] })).rejects.toThrow(/changed meanwhile/);
    await q.update(id('b'), { status: 'closed' });
    expect(titles(q)).toEqual(['a', 'c']);
    expect((await q.closed()).map((i) => i.title)).toEqual(['b']);
    await q.update((await q.closed())[0].id, { status: 'open' });
    expect(titles(q)).toEqual(['a', 'c', 'b']);
  });

  it.each([
    ['top', ['c', 'a', 'b', 'd']],
    ['bottom', ['a', 'b', 'd', 'c']],
    ['up', ['a', 'c', 'b', 'd']],
    ['down', ['a', 'b', 'd', 'c']],
  ] as const)('moves an item to the %s, as the page shows it before the server has it', (to, want) => {
    expect(moveItem(['a', 'b', 'c', 'd'], 'c', to)).toEqual(want);
    expect(moveItem(['a'], 'x', to)).toEqual(['a']);
  });

  it('dismisses all the open items it is given, and only those', async () => {
    const { q, state } = await setup();
    for (const t of ['a', 'b', 'c']) await q.create({ title: t });
    const [a, b, c] = q.list();
    await q.update(a.id, { status: 'closed' });
    const before = state.changes;
    expect(await q.closeAll({ ids: [a.id, b.id, b.id, 'gone'] })).toBe(1);
    expect(titles(q)).toEqual(['c']);
    expect((await q.closed()).map((i) => i.title).sort()).toEqual(['a', 'b']);
    expect(state.changes).toBe(before + 1);
    expect(await q.closeAll({ ids: [] })).toBe(0);
    await expect(q.closeAll({ ids: 'all' as unknown as string[] })).rejects.toThrow(/list of ids/);
    expect((await q.get(c.id)).status).toBe('open');
  });

  it.each([
    ['a session moves off its other item', 's1', 'B', ['B']],
    ['an item replaces its session', 's2', 'A', ['A']],
  ])('links one to one: %s', async (_what, sessionId, to, want) => {
    const { q, session } = await setup();
    session('s1');
    session('s2');
    const a = await q.create({ title: 'A' });
    const b = await q.create({ title: 'B' });
    await q.update(a.id, { sessionId: 's1' });
    await q.update(to === 'A' ? a.id : b.id, { sessionId });
    const holding = q.list().filter((i) => i.sessionId === sessionId).map((i) => i.title);
    expect(holding).toEqual(want);
    expect(q.list().filter((i) => i.sessionId).length).toBe(1);
  });

  it('finds an item and its session by any spelling of a link, closed ones last', async () => {
    const { q, db, session } = await setup();
    session('s1', 'archived');
    const a = await q.create({ title: 'Fix split', links: ['https://github.com/o/r/pull/9'] });
    setItemSession(db, a.id, 's1');
    const b = await q.create({ title: 'Also on it', links: ['o/r#9'] });
    await q.update(a.id, { status: 'closed' });
    const found = await q.byLink('https://github.com/O/R/pull/9/files');
    expect(found.map((m) => m.item.title)).toEqual(['Also on it', 'Fix split']);
    expect(found[1].session).toMatchObject({ id: 's1', closed: true });
    expect(q.soleItemFor('o/r#9')).toBe(b.id);
    await expect(q.byLink('no link here')).rejects.toThrow(/isn't a link/);
  });

  it('turns away an empty session id, and keeps a synced link\'s label when the item is edited', async () => {
    const { q, state } = await setup({ slack: [{ url: 'https://acme.slack.com/archives/C1/p1700000000000001', text: 'look', notes: 'Hi @Alice, can you look?', reporter: 'Bob', channel: 'db-dev' }] });
    const a = await q.create({ title: 'A' });
    await expect(q.update(a.id, { sessionId: '' })).rejects.toThrow(/must be its id/);
    await q.sync(true);
    const msg = q.list()[1];
    expect(msg.notes).toBe('Bob: Hi @Alice, can you look?');
    await q.update(msg.id, { title: 'Look at it', links: [msg.links[0].url!, 'DB-1'] });
    expect((await q.get(msg.id)).links.map((l) => l.label)).toEqual(['Slack #db-dev', 'DB-1']);
    expect(state.slackReads).toBe(1);
  });

  it('checks the item a new session is for', async () => {
    const { q } = await setup();
    const a = await q.create({ title: 'A' });
    expect(q.checkStartItem(a.id)).toBe(a.id);
    expect(q.checkStartItem(undefined)).toBeNull();
    await q.update(a.id, { status: 'closed' });
    expect(() => q.checkStartItem(a.id)).toThrow(/closed or no longer exists/);
    expect(() => q.checkStartItem(7)).toThrow(/must be its id/);
  });
});

describe('the work queue in Meko Cloud', () => {
  const pr = (n: number): ReviewRequest => ({ repo: 'o/r', number: n, title: `PR ${n}`, url: `https://github.com/o/r/pull/${n}` });

  it('is empty before set-up, and refuses edits and a pressed Sync with what to do', async () => {
    const { q, state, session, db } = await setup({ attach: false, reviews: [pr(1)] });
    session('s1');
    setItemSession(db, '00000000-0000-4000-8000-000000000001', 's1');
    expect(q.list()).toEqual([]);
    expect(await q.closed()).toEqual([]);
    expect(await q.byLink('o/r#1')).toEqual([]);
    await expect(q.create({ title: 'x' })).rejects.toMatchObject({ status: 409, message: /Settings › Work queue/ });
    await expect(q.sync(true)).rejects.toMatchObject({ status: 409 });
    await q.sync(false);
    expect(state.reviewReads).toBe(0);
    // Linking needs the queue; unlinking is this computer's alone.
    await expect(q.update('00000000-0000-4000-8000-000000000002', { sessionId: 's1' })).rejects.toMatchObject({ status: 409 });
    // Merge & Close's dismiss waits for the database, though the list has nothing yet.
    expect(q.closeForSession('s1')).toBeNull();
    expect(all(db, 'SELECT item_id FROM work_pending_close')).toEqual([{ item_id: '00000000-0000-4000-8000-000000000001' }]);
    await q.update('00000000-0000-4000-8000-000000000001', { sessionId: null });
    expect(all(db, 'SELECT * FROM work_item_sessions')).toEqual([]);
  });

  it('is one queue for two computers: a sync at once adds each PR once, and an edit on one shows on the other', async () => {
    const pg = await freshPglite();
    const a = await setup({ pg, reviews: [pr(1), pr(2)] });
    const b = await setup({ pg, reviews: [pr(2), pr(1)] });
    await Promise.all([a.q.sync(false), b.q.sync(false)]);
    await a.q.refresh();
    expect(titles(a.q)).toEqual(titles(b.q));
    expect(titles(a.q).length).toBe(2);
    // B's gh sees a request A's doesn't (another account): A doesn't mark it done.
    b.state.reviews = [pr(1), pr(2), pr(3)];
    await b.q.sync(false);
    await a.q.sync(false);
    expect(a.q.list().map((i) => i.githubDone)).toEqual([false, false, false]);
    // A reorder of a list B changed meanwhile is refused, and A then has the list as it is now.
    const stale = a.q.list().map((i) => i.id);
    await b.q.update(stale[2], { status: 'closed' });
    await expect(a.q.reorder({ ids: [...stale].reverse() })).rejects.toMatchObject({ status: 409 });
    expect(a.q.list().map((i) => i.id)).toEqual(stale.slice(0, 2));
    await a.q.update(a.q.list()[0].id, { title: 'Renamed on A' });
    await b.q.refresh();
    expect(titles(b.q)).toContain('Renamed on A');
    // A session is the computer's own: one linked on A isn't on B.
    a.session('s1');
    await a.q.update(a.q.list()[0].id, { sessionId: 's1' });
    expect(b.q.list()[0].sessionId).toBeNull();
  });

  it("keeps the last list while the database can't be reached, refuses edits, and dismisses Merge & Close's item once it's back", async () => {
    const { q, sql, state, session, db } = await setup();
    session('s1');
    session('s2');
    const a = await q.create({ title: 'A' });
    const b = await q.create({ title: 'B' });
    await q.update(a.id, { sessionId: 's1' });
    sql.down = true;
    await expect(q.update(b.id, { title: 'B2' })).rejects.toMatchObject({ status: 503, message: /Can't reach your work queue in Meko Cloud: connect ECONNREFUSED/ });
    expect(q.unreachableError).toMatch(/ECONNREFUSED/);
    expect(state.unreachable).toBeGreaterThan(0);
    expect(titles(q)).toEqual(['A', 'B']);
    // Linking a session and starting one from an item are this computer's own, so they still work.
    await q.update(b.id, { sessionId: 's2' });
    expect(q.checkStartItem(b.id)).toBe(b.id);
    expect(q.closeForSession('s1')).toBe('A');
    expect(all(db, 'SELECT item_id FROM work_pending_close')).toEqual([{ item_id: a.id }]);
    // Sync doesn't read GitHub or Slack while it couldn't add what it found.
    await expect(q.sync(true)).rejects.toMatchObject({ status: 503 });
    expect([state.reviewReads, state.slackReads]).toEqual([0, 0]);
    sql.down = false;
    await q.refresh();
    expect([titles(q), q.unreachableError]).toEqual([['B'], null]);
    expect(all(db, 'SELECT item_id FROM work_pending_close')).toEqual([]);
    // A closed item's session can still be unlinked.
    setItemSession(db, a.id, 's1');
    expect(await q.update(a.id, { sessionId: null })).toMatchObject({ id: a.id, status: 'closed', sessionId: null });
  });

  it("shows a first read that fails, and refuses writes until one works", async () => {
    const { q, store } = await setup({ attach: false });
    await expect(q.attach(store, async () => {
      throw new Error('permission denied for schema kode');
    })).rejects.toThrow(/permission denied/);
    expect(q.unreachableError).toMatch(/permission denied/);
    await expect(q.create({ title: 'x' })).rejects.toMatchObject({ status: 503, message: /isn't open yet/ });
  });

  it("only reads a database a newer Kode set up", async () => {
    const pg = await freshPglite();
    const first = await setup({ pg });
    await first.q.create({ title: 'A' });
    await pg.exec("UPDATE kode.meta SET value = '99' WHERE key = 'schema_version'; DROP INDEX kode.work_items_open");
    const { q, session, db } = await setup({ pg });
    expect(titles(q)).toEqual(['A']);
    expect(q.isReadOnly).toBe(true);
    await expect(q.create({ title: 'B' })).rejects.toMatchObject({ status: 503, message: /newer Kode/ });
    // Its tables stay as the newer Kode left them, and nothing waits to be written.
    expect((await pg.query("SELECT 1 FROM pg_indexes WHERE indexname = 'work_items_open'")).rows).toEqual([]);
    session('s1');
    setItemSession(db, q.list()[0].id, 's1');
    expect(q.closeForSession('s1')).toBeNull();
    expect(all(db, 'SELECT * FROM work_pending_close')).toEqual([]);
  });

  it("copies this computer's items from before the move in once, in their order, new, with their links", async () => {
    const legacy = (db: ReturnType<typeof openDb>, rows: [string, string, number, 'open' | 'closed'][]) => {
      for (const [id, title, position, status] of rows) {
        run(db, `INSERT INTO work_items(id, title, position, status, source, created_at, closed_at) VALUES (?, ?, ?, ?, 'manual', 1, ?)`,
          id, title, position, status, status === 'closed' ? 5 : null);
        run(db, `INSERT INTO work_item_links(item_id, ord, key, kind, url, label) VALUES (?, 0, ?, 'jira', NULL, ?)`, id, `jira:DB-${position}`, `DB-${position}`);
      }
    };
    const id = (n: number) => `00000000-0000-4000-8000-00000000000${n}`;
    const synced = (db: ReturnType<typeof openDb>, itemId: string) =>
      run(db, `INSERT INTO work_items(id, title, position, source, source_key, created_at) VALUES (?, 'Review o/r#9', 5, 'github', 'gh:o/r#9', 1)`, itemId);
    const pg = await freshPglite();
    const a = await setup({ pg, before: (db) => {
      legacy(db, [[id(1), 'Second', 2, 'open'], [id(2), 'First', 1, 'open'], [id(3), 'Done', 3, 'closed']]);
      synced(db, id(6));
    } });
    await a.q.create({ title: 'Made after' });
    expect(titles(a.q)).toEqual(['First', 'Second', 'Review o/r#9', 'Made after']);
    expect(a.q.list()[0]).toMatchObject({ unplaced: true, links: [{ key: 'jira:DB-1' }] });
    expect((await a.q.closed()).map((i) => i.title)).toEqual(['Done']);
    // A release from before the move, run after a rollback, adds one more: the next start copies it in too.
    legacy(a.db, [[id(5), 'After rollback', 4, 'open']]);
    await a.q.attach(a.store, async () => (await a.store.migrate()).readOnly);
    expect(titles(a.q)).toEqual(['First', 'Second', 'Review o/r#9', 'Made after', 'After rollback']);
    // Another computer with one of the same items copies only its own; its session on a review request
    // computer A added already moves to A's item.
    const b = await setup({ pg, attach: false, before: (db) => legacy(db, [[id(2), 'First', 1, 'open'], [id(4), 'From B', 1, 'open']]) });
    b.session('s1');
    synced(b.db, id(7));
    setItemSession(b.db, id(7), 's1');
    await b.q.attach(b.store, async () => (await b.store.migrate()).readOnly);
    expect(titles(b.q)).toEqual(['First', 'Second', 'Review o/r#9', 'Made after', 'After rollback', 'From B']);
    expect(b.q.list().find((i) => i.title === 'Review o/r#9')).toMatchObject({ id: id(6), sessionId: 's1' });
  });

  it("moves each session's item into work_item_sessions, and again what a rollback's release linked", () => {
    const path = join(mkdtempSync(join(tmpdir(), 'ak-wq-')), 'state.db');
    let db = openDb(path);
    const v = schemaVersion(db);
    db.exec('DROP TABLE work_item_sessions; DROP TABLE work_pending_close; PRAGMA user_version = 54');
    run(db, `INSERT INTO projects(id, name, repo_path, default_branch, created_at) VALUES ('p', 'P', '/r', 'main', 1)`);
    run(db, `INSERT INTO sessions(id, project_id, slug, name, claude_session_id, branch, base_branch, worktree_path, tmux_name, hook_token,
             desired, status, stage, created_at) VALUES ('s1', 'p', 's1', 'S', 'c', 'b', 'main', '/w', 't', 'tok', 'running', 'idle', 'code', 1)`);
    run(db, `INSERT INTO work_items(id, title, position, session_id, created_at) VALUES ('i1', 'A', 1, 's1', 1), ('i2', 'B', 2, NULL, 1)`);
    db.close();
    db = openDb(path);
    expect(schemaVersion(db)).toBe(v);
    expect(all(db, 'SELECT item_id, session_id FROM work_item_sessions')).toEqual([{ item_id: 'i1', session_id: 's1' }]);
    // Unlinked here, then a release from before 55 (after a rollback) links another session in work_items.
    run(db, `INSERT INTO sessions(id, project_id, slug, name, claude_session_id, branch, base_branch, worktree_path, tmux_name, hook_token,
             desired, status, stage, created_at) VALUES ('s2', 'p', 's2', 'S2', 'c', 'b2', 'main', '/w2', 't2', 'tok2', 'running', 'idle', 'code', 1)`);
    setItemSession(db, 'i1', null);
    run(db, "UPDATE work_items SET session_id = 's2' WHERE id = 'i2'");
    db.close();
    db = openDb(path);
    expect(all(db, 'SELECT item_id, session_id FROM work_item_sessions')).toEqual([{ item_id: 'i2', session_id: 's2' }]);
    db.close();
  });
});

describe('syncing the work queue', () => {
  const pr = (n: number): ReviewRequest => ({ repo: 'o/r', number: n, title: `PR ${n}`, url: `https://github.com/o/r/pull/${n}` });
  const msg = (ts: string, text: string): SlackListItem => ({ url: `https://acme.slack.com/archives/C1/p${ts}`, text, notes: '', reporter: '', channel: 'db-dev' });

  it('adds review requests and Slack list messages at the bottom, new, and never again once closed', async () => {
    const { q, state } = await setup({ reviews: [pr(1), pr(2)], slack: [msg('1700000000000001', 'can you look at\nthe flaky test')] });
    await q.create({ title: 'Mine', links: ['o/r#2'] });
    const r = await q.sync(true);
    expect(r).toMatchObject({ github: { ok: true, added: 1 }, slack: { ok: true, added: 1 }, slackAuto: true });
    expect(titles(q)).toEqual(['Mine', 'Review o/r#1: PR 1', 'can you look at']);
    expect(q.list().slice(1).every((i) => i.unplaced)).toBe(true);
    expect(q.list()[2].links[0].label).toBe('Slack #db-dev');
    await q.update(q.list()[1].id, { status: 'closed' });
    await q.update(q.list()[1].id, { placed: true });
    await q.sync(true);
    expect(titles(q)).toEqual(['Mine', 'can you look at']);
    expect(q.list()[1].unplaced).toBe(false);
    expect(state.slackReads).toBe(2);
  });

  it('marks a review request done on GitHub when it is gone, and not once it is back', async () => {
    const { q, state } = await setup({ reviews: [pr(1)] });
    await q.sync(false);
    state.reviews = [];
    await q.sync(false);
    expect(q.list()[0].githubDone).toBe(true);
    state.reviews = [pr(1)];
    await q.sync(false);
    expect(q.list()[0].githubDone).toBe(false);
  });

  it('marks nothing done on GitHub from a full page of review requests, which may have left some out', async () => {
    const { q, state } = await setup({ reviews: [pr(1)] });
    await q.sync(false);
    state.reviews = Array.from({ length: 100 }, (_, i) => pr(i + 2));
    await q.sync(false);
    expect(q.list().find((i) => i.title.startsWith('Review o/r#1:'))!.githubDone).toBe(false);
  });

  it('reads Slack on the timer only after a Sync reached it, and stops after a failure', async () => {
    const { q, state } = await setup({ reviews: [pr(1)], slack: new Error('no Slack tools') });
    await q.sync(false);
    expect(state.slackReads).toBe(0);
    const r = await q.sync(true);
    expect(r).toMatchObject({ github: { ok: true }, slack: { ok: false, error: 'no Slack tools' }, slackAuto: false });
    state.slack = [];
    expect((await q.sync(true)).slackAuto).toBe(true);
    await q.sync(false);
    expect(state.slackReads).toBe(3);
    state.slack = new Error('timed out');
    await q.sync(false);
    expect((await q.sync(false)).slack).toBeNull();
    expect(state.slackReads).toBe(4);
  });

  it('reads only the sources Settings has on, and refuses Sync with none', async () => {
    const { q, state } = await setup({ reviews: [pr(1)] });
    state.settings = { github: false, slack: true, syncMinutes: 15 };
    expect(await q.sync(true)).toMatchObject({ github: null, slack: { ok: true }, sources: { github: false, slack: true } });
    state.settings = { github: true, slack: false, syncMinutes: 15 };
    expect(await q.sync(true)).toMatchObject({ github: { added: 1 }, slack: null });
    state.settings = { github: false, slack: false, syncMinutes: 15 };
    await expect(q.sync(true)).rejects.toThrow(/Sync is off/);
    expect([state.reviewReads, state.slackReads]).toEqual([1, 1]);
  });

  it('syncs on the timer once the minutes Settings gives have passed, and never at 0', async () => {
    const { q, state } = await setup();
    const now = Date.now();
    await q.tick(now + 14 * 60_000);
    await q.tick(now + 15 * 60_000 + 1);
    state.settings = { ...state.settings, syncMinutes: 0 };
    await q.tick(now + 60 * 60_000);
    expect(state.reviewReads).toBe(1);
  });

  it('drops the last Slack result when the list link changes, so the line waits for a Sync press', async () => {
    const { q, state } = await setup({ slack: new Error('Set the link to your Slack list in Settings › Work queue.') });
    expect((await q.sync(true)).slack).toMatchObject({ ok: false });
    const changes = state.changes;
    q.slackListChanged();
    expect(q.syncState()).toMatchObject({ slack: null, slackAuto: false });
    expect(state.changes).toBe(changes + 1);
    q.slackListChanged();
    expect(state.changes).toBe(changes + 1);
  });

  it('says Slack needs Claude Code without running it', async () => {
    const { q, state } = await setup({ claude: false });
    expect((await q.sync(true)).slack).toMatchObject({ ok: false, error: expect.stringMatching(/needs Claude Code/) });
    expect(state.slackReads).toBe(0);
  });
});

describe("reading the user's Slack list", () => {
  const ran = (out: unknown, o: Partial<AskRun> = {}): AskRun => ({ stdout: JSON.stringify(out), stderr: '', code: 0, spawnError: null, timedOut: false, aborted: false, ...o });

  it('loads your connectors but none of your permissions, and no built-in tools but ToolSearch', () => {
    const args = slackArgs('sonnet', ['mcp__claude_ai_Slack__slack_search_public_and_private'], 'F0LIST', { env: { A: '1' } });
    const after = (flag: string) => args[args.indexOf(flag) + 1];
    expect(args).not.toContain('--strict-mcp-config');
    expect([after('--tools'), after('--permission-mode'), after('--setting-sources')]).toEqual(['ToolSearch', 'default', 'project']);
    expect(after('--system-prompt')).toContain('"select:mcp__claude_ai_Slack__slack_search_public_and_private"');
    expect(args.slice(args.indexOf('--allowedTools') + 1, args.indexOf('--disallowedTools'))).toEqual(['mcp__claude_ai_Slack__slack_search_public_and_private']);
    expect(args).toContain('mcp__claude_ai_Slack__slack_send_message');
    expect(JSON.parse(after('--settings'))).toEqual({ env: { A: '1' }, disableAllHooks: true });
    expect(after('--system-prompt')).toContain('the file F0LIST');
    expect(slackArgs('', [], 'F0LIST')).not.toContain('--allowedTools');
    expect(slackPrompt([], 'F0LIST')).not.toContain('ToolSearch');
  });

  it('takes only the sign-in part of your Claude Code settings', () => {
    const dir = mkdtempSync(join(tmpdir(), 'ak-slack-'));
    writeFileSync(join(dir, 'settings.json'), JSON.stringify({ env: { X: '1' }, apiKeyHelper: 'k', permissions: { allow: ['mcp__github'], defaultMode: 'bypassPermissions' } }));
    expect(signInSettings(dir)).toEqual({ env: { X: '1' }, apiKeyHelper: 'k' });
    expect(signInSettings(join(dir, 'none'))).toEqual({});
  });

  it.each([
    ['the messages it found', ran({ structured_output: { found: true, items: [{ url: 'u', text: ' hi ', notes: ' Hi @Alice ', reporter: ' Bob ', channel: 'c' }, { url: 'v', text: 'no notes' }, { text: 'no url' }] } }), [{ url: 'u', text: 'hi', notes: 'Hi @Alice', reporter: 'Bob', channel: 'c' }, { url: 'v', text: 'no notes', notes: '', reporter: '', channel: '' }]],
    ['a list with no Message column', ran({ structured_output: { found: true, noMessageColumn: true, items: [] } }), /has no Message column/],
    ['that it could not reach the list', ran({ structured_output: { found: false, items: [] } }), /couldn't read your Slack list.*slack_read_file/],
    ['a sign-in it needs', ran({ is_error: true, result: 'Not logged in · Please run /login' }, { code: 1 }), /isn't signed in/],
    ['a timeout', ran({}, { timedOut: true, code: null }), /over 180 seconds/],
  ])('reads %s', (_what, r, want) => {
    if (want instanceof RegExp) expect(() => readSlackList(r, 'claude', 180)).toThrow(want);
    else expect(readSlackList(r, 'claude', 180)).toEqual(want);
  });

  it.each([
    ['https://acme.slack.com/lists/T0ACME/F0CDR40FFS5', 'F0CDR40FFS5'],
    [' https://acme.enterprise.slack.com/lists/T0ACME/F0CDR40FFS5/?view=all ', 'F0CDR40FFS5'],
    ['https://acme.slack.com/archives/C024BE91L/p1700000000123456', null],
    ['http://acme.slack.com/lists/T0ACME/F0CDR40FFS5', null],
    ['https://slack.com.example.com/lists/T0ACME/F0CDR40FFS5', null],
    ['F0CDR40FFS5', null],
  ])('reads the list in %s as %s', (link, id) => {
    expect(slackListId(link)).toBe(id);
  });

  it('says to set the list without running Claude Code when there is no link to one', async () => {
    let runs = 0;
    const cfg = structuredClone(DEFAULT_CONFIG);
    cfg.workQueue.slackList = 'https://acme.slack.com/archives/C1/p1700000000000001';
    const dir = mkdtempSync(join(tmpdir(), 'ak-slack-'));
    await expect(readList(cfg, dir, async () => (runs++, ran({})))).rejects.toThrow(/Set the link to your Slack list/);
    expect(runs).toBe(0);
  });
});
