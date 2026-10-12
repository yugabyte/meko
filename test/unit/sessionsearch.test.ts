import { describe, expect, it } from 'vitest';
import { openDb, run } from '../../src/server/db.js';
import { searchSessions, searchTerms, stem } from '../../src/server/sessionsearch.js';

function setup() {
  const db = openDb(':memory:');
  run(db, `INSERT INTO projects(id, name, repo_path, default_branch, created_at) VALUES ('p', 'P', '/r', 'main', 1), ('q', 'Q', '/q', 'main', 1)`);
  run(db, `INSERT INTO projects(id, name, repo_path, default_branch, created_at, deleted_at) VALUES ('gone', 'Gone', '/g', 'main', 1, 2)`);
  const session = (id: string, project: string, name: string, o: { prompt?: string; branch?: string; desired?: string; pr?: [number, string]; at?: number } = {}) =>
    run(db, `INSERT INTO sessions(id, project_id, slug, name, claude_session_id, branch, base_branch, worktree_path, tmux_name, hook_token,
             desired, status, stage, initial_prompt, pr_number, pr_url, created_at) VALUES (?, ?, ?, ?, 'c', ?, 'main', '/w', 't', ?, ?, 'idle', 'code', ?, ?, ?, ?)`,
      id, project, id, name, o.branch ?? `ak/${id}`, `tok-${id}`, o.desired ?? 'running', o.prompt ?? null,
      o.pr?.[0] ?? null, o.pr?.[1] ?? null, o.at ?? 1);
  session('login', 'p', 'Fix login', { prompt: 'Users get logged out after a password change.' });
  session('totp', 'p', 'Two-step sign-in', { prompt: 'Add TOTP to the login screen', desired: 'archived', pr: [12, 'https://github.com/o/r/pull/12'] });
  session('palette', 'p', 'Palette tweaks', { prompt: 'Make the command palette rotate its hints' });
  session('search', 'p', 'Find things', { branch: 'ak/searching-sessions', at: 5 });
  session('other', 'q', 'Login page copy', { prompt: 'Reword the login page', at: 0 });
  session('deleted', 'gone', 'Login redo');
  run(db, `INSERT INTO designs(session_id, markdown, updated_at) VALUES ('palette', '# Palette\n\nThe hints rotate every second, and a password manager never fills it.', 1)`);
  return db;
}

describe('finding a session', () => {
  it.each([
    ['the session id and matched fields, open before closed, in every open project', 'login', undefined,
      [['login', 'name'], ['other', 'name'], ['totp', 'task']]],
    ['one project only', 'login', 'p', [['login', 'name'], ['totp', 'task']]],
    ['a word in a design, and in a task', 'password', 'p', [['login', 'task'], ['palette', 'design']]],
    ['a word ending another way, in a branch', 'searches', 'p', [['search', 'branch']]],
    ['an -ing word by its stem', 'rotating', 'p', [['palette', 'task']]],
    ['at least half the words, the most first', 'login password rotate', 'p', [['login', 'name'], ['palette', 'task']]],
    ['a PR by number', '#12', 'p', [['totp', 'pr']]],
    ['stop words when nothing else is left', 'the', 'p', [['palette', 'task'], ['totp', 'task']]],
  ])('finds %s', (_what, q, project, want) => {
    expect(searchSessions(setup(), q, project).map((m) => [m.sessionId, m.field])).toEqual(want);
  });

  it('says where it found the words and what a closed session has', () => {
    const [m] = searchSessions(setup(), 'totp', 'p');
    expect(m).toMatchObject({ name: 'Two-step sign-in', closed: true, prNumber: 12, prUrl: 'https://github.com/o/r/pull/12', field: 'task', snippet: 'Add TOTP to the login screen' });
    const [d] = searchSessions(setup(), 'manager', 'p');
    expect(d.snippet).toMatch(/^….*password manager never fills it\.$/);
  });

  it('turns away a query that is too long or not text, and more than one project', () => {
    expect(() => searchSessions(setup(), 'x'.repeat(201))).toThrow(/at most 200/);
    expect(() => searchSessions(setup(), ['login'])).toThrow(/one q parameter/);
    expect(() => searchSessions(setup(), 'login', ['p', 'q'])).toThrow(/at most one project/);
    expect(searchTerms('Fix the LOGIN, login page!')).toEqual(['fix', 'login', 'page']);
  });

  it.each([
    ['searching', 'search'], ['searches', 'search'], ['rotating', 'rotat'], ['logging', 'log'], ['fixed', 'fix'],
    ['press', 'pres'], ['used', 'used'], ['login', 'login'],
  ])('stems %s as %s', (word, want) => {
    expect(stem(word)).toBe(want);
  });
});
