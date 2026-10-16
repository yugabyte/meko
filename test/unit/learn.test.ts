import { describe, expect, it } from 'vitest';
import { openDb, run } from '../../src/server/db.js';
import { coveredText, dedupPrompt, firstJson, machineSpecific, memoryText, parseDecisions, parseFacts } from '../../src/server/learn.js';
import { GAP_LEARN_MAX, learnGaps } from '../../src/server/mekogaps.js';

describe('the learn step', () => {
  it('reads the facts from a reply, whatever wraps the JSON', () => {
    const json = '{"facts":[{"kind":"gotcha","component":"src/server/db.ts","text":"Migrations are append-only."}]}';
    const fact = { kind: 'gotcha', component: 'src/server/db.ts', text: 'Migrations are append-only.', answers: [] };
    expect(parseFacts(json)).toEqual([fact]);
    expect(parseFacts('Here they are:\n```json\n' + json + '\n```\nDone.')).toEqual([fact]);
    expect(parseFacts('{"facts":[]}')).toEqual([]);
    // Not what was asked for: no facts list at all.
    expect(parseFacts('I learned a lot today.')).toBeNull();
    expect(parseFacts('{"notes":[]}')).toBeNull();
  });

  it('keeps only facts with text, an unknown kind read as how it works, long text cut, and answers to questions it was offered', () => {
    const facts = parseFacts(JSON.stringify({ facts: [
      // Three questions were offered: numbers outside 1-3, repeats and non-integers are dropped.
      { kind: 'trivia', text: '  The cache key includes the model.  ', answers: [2, 0, 4, 2, 1.5, '3', 3] },
      { kind: 'gotcha', component: 'x' },
      { text: 'y'.repeat(5000) },
      'not an object',
      // Named like an object's own property: still unknown.
      ...['constructor', 'toString', '__proto__', 'hasOwnProperty'].map((kind) => ({ kind, text: 'x' })),
    ] }), 3);
    expect(facts).toEqual([
      { kind: 'how-it-works', component: '', text: 'The cache key includes the model.', answers: [2, 3] },
      { kind: 'how-it-works', component: '', text: 'y'.repeat(2000), answers: [] },
      ...Array.from({ length: 4 }, () => ({ kind: 'how-it-works', component: '', text: 'x', answers: [] })),
    ]);
  });

  it("offers at most GAP_LEARN_MAX of the project's open questions, the session's own first", () => {
    const db = openDb(':memory:');
    run(db, `INSERT INTO projects(id, name, repo_path, default_branch, created_at) VALUES ('p', 'P', '/r', 'main', 1)`);
    for (const id of ['s', 'other']) {
      run(db, `INSERT INTO sessions(id, project_id, slug, name, claude_session_id, branch, base_branch, worktree_path, tmux_name, hook_token,
               desired, status, stage, created_at) VALUES (?, 'p', ?, ?, 'c', 'b', 'main', '/w', 't', ?, 'running', 'idle', 'code', 1)`, id, id, id, `tok-${id}`);
    }
    const search = (session: string, query: string, at: number) =>
      run(db, `INSERT INTO meko_calls(session_id, tool_use_id, tool, kind, at, input_chars, outcome, results, query, query_key, quality)
               VALUES (?, ?, 'context_search', 'lookup', ?, 40, 'empty', 0, ?, ?, 'empty')`, session, `${session}-${at}`, at, query, query.toLowerCase());
    // The session's own: asked early. Other sessions': asked later, more than the cap leaves room for.
    for (let i = 0; i < 10; i++) search('s', `Own ${i}`, 100 + i);
    for (let i = 0; i < GAP_LEARN_MAX; i++) search('other', `Other ${i}`, 1000 + i);
    const offered = learnGaps(db, 'p', 's');
    expect(offered).toHaveLength(GAP_LEARN_MAX);
    expect([offered[0].query, offered.at(-1)?.query]).toEqual(['Own 9', 'Other 10']);
  });

  it('takes the first JSON object that parses', () => {
    expect(firstJson('a {"x":1} b {"y":2}')).toEqual({ x: 1 });
    expect(firstJson('{"x": {"y": 2}} trailing }')).toEqual({ x: { y: 2 } });
    expect(firstJson('[1, 2]')).toBeNull();
    expect(firstJson('')).toBeNull();
  });

  it('leaves out text that names this machine', () => {
    const paths = ['/home/alice', '/home/alice/.agent-k-local', '/work/wt-3'];
    expect(machineSpecific('The worktree at /work/wt-3 has a stale lock.', paths)).toBe(true);
    expect(machineSpecific('/home/alice/notes x', paths)).toBe(true);
    expect(machineSpecific('src/server/db.ts Migrations are append-only.', paths)).toBe(false);
    // An empty or root path names nothing.
    expect(machineSpecific('a/b', ['', '/'])).toBe(false);
  });

  it('matches a path only whole, so a short local spelling of the home leaves longer names alone', () => {
    const paths = ['/data', '/data/ak/worktrees/repo.1a2b3c4d/wt-1'];
    expect(machineSpecific('Facts quote /data/ak/worktrees/repo.1a2b3c4d/wt-1/src as the root.', paths)).toBe(true);
    expect(machineSpecific('The home is /data, mounted locally.', paths)).toBe(true);
    expect(machineSpecific('`/data/notes.md` is mine', paths)).toBe(true);
    expect(machineSpecific('src/shared/types.ts and src/database.ts hold it; /data-sets and app/data/x too.', paths)).toBe(false);
  });

  it('saves each fact with its kind and component first', () => {
    expect(memoryText({ kind: 'gotcha', component: 'src/server/db.ts', text: 'Migrations are append-only.' }))
      .toBe('Gotcha (src/server/db.ts): Migrations are append-only.');
    expect(memoryText({ kind: 'root-cause', component: '', text: 'The poll ran twice.' })).toBe('Root cause: The poll ran twice.');
    expect(memoryText({ kind: 'decision', component: 'x', text: 'Decision: use SQLite.' })).toBe('Decision: use SQLite.');
    expect(memoryText({ kind: 'constructor', component: '', text: 'x' })).toBe('How it works: x');
  });

  it('tells a learn step the latest facts earlier ones covered, each cut short', () => {
    expect(coveredText([])).toBe('');
    const list = coveredText([...Array.from({ length: 61 }, (_, i) => `Fact ${i}.`), `Long\n${'x'.repeat(500)}`]).split('\n').filter((l) => l.startsWith('- '));
    expect(list).toHaveLength(60);
    expect(list[0]).toBe('- Fact 2.');
    expect(list[59]).toBe(`- Long ${'x'.repeat(395)}…`);
  });

  it('asks the duplicate check about each fact beside the memories most like it', () => {
    const p = dedupPrompt(
      [{ kind: 'gotcha', component: 'db.ts', text: 'Migrations are append-only.' }, { kind: 'measured', component: '', text: 'A fork costs 2 s.' }],
      [[{ id: 'm1', text: 'Never edit a shipped\nmigration.', agentId: 'a' }], []],
    );
    expect(p).toContain('1. Gotcha (db.ts): Migrations are append-only.\n   Saved memories most like it:\n   - [m1] Never edit a shipped migration.');
    expect(p).toContain('2. Measured: A fork costs 2 s.\n   Saved memories most like it:\n   (none)');
    expect(p).toContain('{"decisions":[{"n":1,"action":"add|skip|update"');
  });

  it('reads the decisions by fact number, with the memory a skip or an update names; an unknown action adds', () => {
    const d = parseDecisions('{"decisions":[{"n":1,"action":"skip","id":"m9"},{"n":"2","action":"update","id":"m1","text":" New text. "},{"n":3,"action":"maybe"},{"n":0,"action":"skip"}]}');
    expect(d && [...d]).toEqual([
      [1, { action: 'skip', id: 'm9', text: null }],
      [2, { action: 'update', id: 'm1', text: 'New text.' }],
      [3, { action: 'add', id: null, text: null }],
    ]);
    expect(parseDecisions('no')).toBeNull();
  });
});
