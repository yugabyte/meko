import { execFileSync } from 'node:child_process';
import { mkdirSync, rmSync, writeFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { globRegex, learnScope, parseTree, whereWords, wherePaths } from '../../src/server/learnscope.js';
import { LEARN_WARN_FILES, LEARN_WARN_PRS, type LearnScope } from '../../src/shared/types.js';
import { learnScopeHint } from '../../src/web/util.js';
import { gitRepo, tempDir } from '../helpers.js';

describe("a learn session's Where, read as paths", () => {
  it('keeps the words that could be paths, trimmed, and drops prose and URLs', () => {
    expect(whereWords('e.g. services/billing/, `libs/payments/`; docs/billing/*.md and ./README.md.')).toEqual(
      ['e.g', 'services/billing/', 'libs/payments/', 'docs/billing/*.md', 'README.md'],
    );
    expect(whereWords('The billing service, and https://example.com/wiki/billing (the wiki)')).toEqual([]);
    expect(whereWords('/src/server (the server) ../outside :(exclude)src')).toEqual(['src/server']);
    expect(whereWords('')).toEqual([]);
  });

  it('reads globs as git does: * and ? inside a folder, ** across, and a folder named takes what is under it', () => {
    expect(globRegex('docs/*.md').test('docs/a.md')).toBe(true);
    expect(globRegex('docs/*.md').test('docs/x/a.md')).toBe(false);
    expect(globRegex('**/learn.ts').test('src/server/learn.ts')).toBe(true);
    expect(globRegex('**/learn.ts').test('learn.ts')).toBe(true);
    expect(globRegex('src/**').test('src/a/b.ts')).toBe(true);
    expect(globRegex('src/*/').test('src/a/b.ts')).toBe(true);
    expect(globRegex('a?.[tj]s').test('ab.ts')).toBe(true);
    expect(globRegex('a?.[!tj]s').test('ab.ts')).toBe(false);
    expect(globRegex('a+b.(x)').test('a+b.(x)')).toBe(true);
  });

  it('keeps only the words that name files the repo has', () => {
    const files = ['README.md', 'services/billing/a.ts', 'docs/billing/x.md', 'docs/other.md'].map((path) => ({ path, size: 1 }));
    expect(wherePaths('services/billing/ docs/billing/*.md services/payments/ e.g. README.md', files))
      .toEqual(['services/billing/', 'docs/billing/*.md', 'README.md']);
    expect(wherePaths('the whole thing', files)).toEqual([]);
    // A folder is matched whole, not as the start of another name.
    expect(wherePaths('docs/bill', files)).toEqual([]);
  });

  it('reads ls-tree -r -l -z, leaving out submodules', () => {
    const out = '100644 blob aaaa     120\tsrc/a b.ts\x00160000 commit bbbb       -\tvendor/sub\x00100644 blob cccc 5\tREADME.md\x00';
    expect(parseTree(out)).toEqual([{ path: 'src/a b.ts', size: 120 }, { path: 'README.md', size: 5 }]);
  });
});

describe('learnScope, on a real repo', () => {
  let home: string;
  let repo: string;
  const git = (...a: string[]) => execFileSync('git', a, { cwd: repo, encoding: 'utf8', stdio: ['ignore', 'pipe', 'ignore'] }).trim();
  /** A commit on the current branch, dated <iso>, adding <file>. */
  const commit = (file: string, iso: string, body = 'x\n') => {
    mkdirSync(dirname(join(repo, file)), { recursive: true });
    writeFileSync(join(repo, file), body);
    git('add', '.');
    execFileSync('git', ['commit', '-q', '-m', `add ${file}`], {
      cwd: repo, stdio: 'ignore', env: { ...process.env, GIT_AUTHOR_DATE: iso, GIT_COMMITTER_DATE: iso },
    });
  };
  const now = Date.parse('2026-10-23T12:00:00Z');

  beforeEach(() => {
    home = tempDir('akl-scope-');
    repo = gitRepo(join(home, 'repo'));
    // The first commit, made now, is dated before the rest.
    execFileSync('git', ['commit', '-q', '--amend', '--no-edit'], {
      cwd: repo, stdio: 'ignore', env: { ...process.env, GIT_AUTHOR_DATE: '2019-01-01T00:00:00Z', GIT_COMMITTER_DATE: '2019-01-01T00:00:00Z' },
    });
    commit('svc/old.ts', '2020-01-01T00:00:00Z');
    commit('svc/a.ts', '2026-03-01T00:00:00Z', 'abcdef\n');
    commit('docs/a.md', '2026-06-01T00:00:00Z');
    // A side branch merged in: only its merge commit is on the first-parent line.
    git('checkout', '-q', '-b', 'feature');
    commit('svc/b.ts', '2026-07-01T00:00:00Z');
    commit('svc/c.ts', '2026-07-02T00:00:00Z');
    git('checkout', '-q', 'main');
    execFileSync('git', ['merge', '-q', '--no-ff', '-m', 'Merge feature', 'feature'], {
      cwd: repo, stdio: 'ignore', env: { ...process.env, GIT_AUTHOR_DATE: '2026-07-03T00:00:00Z', GIT_COMMITTER_DATE: '2026-07-03T00:00:00Z' },
    });
  });
  afterEach(() => rmSync(home, { recursive: true, force: true }));

  it('counts the PRs in the window and the files, in the whole repo when Where names none of its paths', async () => {
    // README, old, a, docs/a.md, b, c; the first-parent commits since 2025-10-23: svc/a, docs/a, the merge.
    expect(await learnScope(repo, 'main', 'the billing service', 12, now)).toMatchObject({ prs: 3, files: 6, paths: [] });
    expect(await learnScope(repo, 'main', '', 0, now)).toMatchObject({ prs: 0, files: 6 });
  });

  it('counts only what the paths Where names cover', async () => {
    const s = await learnScope(repo, 'main', 'svc/*.ts and docs/missing/', 12, now);
    expect(s).toEqual({ prs: 2, files: 4, bytes: 2 + 7 + 2 + 2, paths: ['svc/*.ts'] });
    expect(await learnScope(repo, 'main', 'docs/', 12, now)).toMatchObject({ prs: 1, files: 1, paths: ['docs/'] });
  });

  it("reads origin's branch over the local one, and gives null for a branch in neither", async () => {
    const clone = join(home, 'clone');
    execFileSync('git', ['clone', '-q', repo, clone], { stdio: 'ignore' });
    // The clone's local main falls behind its origin/main.
    execFileSync('git', ['reset', '-q', '--hard', 'HEAD~1'], { cwd: clone, stdio: 'ignore' });
    expect(await learnScope(clone, 'main', '', 12, now)).toMatchObject({ prs: 3, files: 6 });
    expect(await learnScope(clone, 'nope', '', 12, now)).toBeNull();
  });
});

describe("the New session dialog's estimate for a learn session", () => {
  const scope = (o: Partial<LearnScope>): LearnScope => ({ prs: 12, files: 340, bytes: 1, paths: [], ...o });

  it('says how many PRs and files, and where', () => {
    expect(learnScopeHint(scope({}), 24)).toEqual({ text: 'About 12 merged PRs and 340 files in the whole repo.', warn: false });
    expect(learnScopeHint(scope({ prs: 1, files: 1, paths: ['svc/'] }), 24).text).toBe('About 1 merged PR and 1 file in svc/.');
    expect(learnScopeHint(scope({ paths: ['a/', 'b/'] }), 24).text).toContain('in a/ and b/.');
    expect(learnScopeHint(scope({ paths: ['a/', 'b/', 'c/'] }), 24).text).toContain('in a/, b/ and c/.');
    expect(learnScopeHint(scope({ paths: ['a/', 'b/', 'c/', 'd/'] }), 24).text).toContain('in a/, b/ and 2 more.');
    expect(learnScopeHint(scope({ prs: 0 }), 0).text).toBe('No PRs and 340 files in the whole repo.');
  });

  it('warns past either limit, with what would make it smaller', () => {
    const many = learnScopeHint(scope({ prs: LEARN_WARN_PRS + 1, files: 18_300 }), 24);
    expect(many).toEqual({
      text: `About ${(LEARN_WARN_PRS + 1).toLocaleString('en-US')} merged PRs and 18,300 files in the whole repo. That's a lot to read: it can take hours and use millions of tokens. Name the folders to learn, or read fewer months.`,
      warn: true,
    });
    expect(learnScopeHint(scope({ prs: LEARN_WARN_PRS }), 24).warn).toBe(false);
    expect(learnScopeHint(scope({ files: LEARN_WARN_FILES + 1, paths: ['svc/'] }), 24).text).toMatch(/tokens\. Name fewer folders\.$/);
    // No PRs are read at 0 months, so the count can't warn.
    expect(learnScopeHint(scope({ prs: 9_999 }), 0).warn).toBe(false);
  });
});
