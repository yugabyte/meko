import { describe, expect, it } from 'vitest';
import { dedupPrompt, firstJson, learnSummary, machineSpecific, memoryText, parseDecisions, parseFacts } from '../../src/server/learn.js';

describe('the learn step', () => {
  it('reads the facts from a reply, whatever wraps the JSON', () => {
    const json = '{"facts":[{"kind":"gotcha","component":"src/server/db.ts","text":"Migrations are append-only."}]}';
    const fact = { kind: 'gotcha', component: 'src/server/db.ts', text: 'Migrations are append-only.' };
    expect(parseFacts(json)).toEqual([fact]);
    expect(parseFacts('Here they are:\n```json\n' + json + '\n```\nDone.')).toEqual([fact]);
    expect(parseFacts('{"facts":[]}')).toEqual([]);
    // Not what was asked for: no facts list at all.
    expect(parseFacts('I learned a lot today.')).toBeNull();
    expect(parseFacts('{"notes":[]}')).toBeNull();
  });

  it('keeps only facts with text, an unknown kind read as how it works, long text cut', () => {
    const facts = parseFacts(JSON.stringify({ facts: [
      { kind: 'trivia', text: '  The cache key includes the model.  ' },
      { kind: 'gotcha', component: 'x' },
      { text: 'y'.repeat(5000) },
      'not an object',
    ] }));
    expect(facts).toEqual([
      { kind: 'how-it-works', component: '', text: 'The cache key includes the model.' },
      { kind: 'how-it-works', component: '', text: 'y'.repeat(2000) },
    ]);
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

  it("reads a kind named like an object's own property as how it works", () => {
    const facts = parseFacts(JSON.stringify({ facts: ['constructor', 'toString', '__proto__', 'hasOwnProperty'].map((kind) => ({ kind, text: 'x' })) }));
    expect(facts?.map((f) => f.kind)).toEqual(['how-it-works', 'how-it-works', 'how-it-works', 'how-it-works']);
    expect(memoryText({ kind: 'constructor', component: '', text: 'x' })).toBe('How it works: x');
  });

  it('saves each fact with its kind and component first', () => {
    expect(memoryText({ kind: 'gotcha', component: 'src/server/db.ts', text: 'Migrations are append-only.' }))
      .toBe('Gotcha (src/server/db.ts): Migrations are append-only.');
    expect(memoryText({ kind: 'root-cause', component: '', text: 'The poll ran twice.' })).toBe('Root cause: The poll ran twice.');
    expect(memoryText({ kind: 'decision', component: 'x', text: 'Decision: use SQLite.' })).toBe('Decision: use SQLite.');
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

  it('reads the decisions by fact number; an unknown action adds', () => {
    const d = parseDecisions('{"decisions":[{"n":1,"action":"skip"},{"n":"2","action":"update","id":"m1","text":" New text. "},{"n":3,"action":"maybe"},{"n":0,"action":"skip"}]}');
    expect(d && [...d]).toEqual([
      [1, { action: 'skip', id: null, text: null }],
      [2, { action: 'update', id: 'm1', text: 'New text.' }],
      [3, { action: 'add', id: null, text: null }],
    ]);
    expect(parseDecisions('no')).toBeNull();
  });

  it('says what it saved', () => {
    expect(learnSummary(1, 0, 'team/demo')).toBe('Saved 1 memory and corrected 0 in team/demo');
    expect(learnSummary(3, 2, 'team/demo')).toBe('Saved 3 memories and corrected 2 in team/demo');
  });
});
