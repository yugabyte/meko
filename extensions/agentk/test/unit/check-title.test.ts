import { execFileSync, spawnSync } from 'node:child_process';
import { readFileSync, rmSync } from 'node:fs';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { gitRepo, tempDir } from '../helpers.js';
import { RELEASE_TYPES } from '../../src/server/releasenotes.js';

// The repo root's scripts/check-title.sh holds the commit subject and PR title rules the commit
// skill follows; scripts/release/release-notes.sh decides which of those types release.
const CHECK = fileURLToPath(new URL('../../../../scripts/check-title.sh', import.meta.url));
const NOTES = fileURLToPath(new URL('../../../../scripts/release/release-notes.sh', import.meta.url));
const SCOPE = fileURLToPath(new URL('../../../../scripts/ci-scope.sh', import.meta.url));

const check = (...args: string[]) => spawnSync('sh', [CHECK, ...args], { encoding: 'utf8' });

/** A `NAME='a|b|c'` list in a script. */
function list(file: string, name: string): string[] {
  const m = new RegExp(`^${name}='([^']+)'$`, 'm').exec(readFileSync(file, 'utf8'));
  if (!m) throw new Error(`no ${name} in ${file}`);
  return m[1]!.split('|');
}
const checkerTypes = () => list(CHECK, 'TYPES');

describe('check-title.sh', () => {
  it('has exactly the nine types', () => {
    expect(checkerTypes().sort()).toEqual(['chore', 'ci', 'deps', 'docs', 'feat', 'fix', 'nit', 'perf', 'test']);
  });

  it('names each component of scripts/ci-scope.sh, and repo for the shared root', () => {
    const scope = spawnSync('sh', [SCOPE, '--all'], { encoding: 'utf8' }).stdout;
    // ci-scope.sh names Meko by its folder, backend/.
    const ci = [...scope.matchAll(/^(\w+)=/gm)].map((m) => (m[1] === 'backend' ? 'meko' : m[1]!));
    expect(list(CHECK, 'COMPONENTS').sort()).toEqual([...ci, 'repo'].sort());
  });

  it.each([
    ...['feat', 'fix', 'perf', 'deps', 'ci', 'test', 'docs', 'nit', 'chore'].map((t) => `${t}(agentk): do the thing`),
    'fix(agentk/web): stop clipping the rail',
    'nit(meko/some-new-area2): fix a comment',
    'feat(agentk/meko)!: drop the old datapack format',
    'deps(agentk)!: bump fastify from 5.13.0 to 6.0.0',
    'fix(agentk): handle a: colon in the summary',
    `fix(agentk/web): ${'a'.repeat(55)}`,
    `fix(agentk/web): ${'a'.repeat(55)} (#123)`,
  ])('accepts %s', (title) => {
    const r = check(title);
    expect(r.stderr).toBe('');
    expect(r.status).toBe(0);
  });

  it.each([
    ['Update foo', 'Start with'],
    ['refactor(agentk): x', 'Unknown type "refactor"'],
    ['feature(agentk): x', 'Unknown type "feature"'],
    ['Fix(agentk): x', 'Unknown type "Fix"'],
    ['Revert "feat(agentk): x"', 'Unknown type'],
    ['(agentk): x', 'Unknown type ""'],
    ['fix: x', 'Name the component in parentheses after the type: one of agentk, meko, presenter, repo.'],
    ['fix(web): x', 'Unknown component "web"'],
    ['fix(): x', 'Unknown component ""'],
    ['fix(agentk) : x', 'Bad component'],
    ['fix!(agentk): x', 'Bad component'],
    ['fix(agentk(web)): x', 'Bad component'],
    ['fix(agentk/): x', 'Bad area ""'],
    ['fix(agentk/Web): x', 'Bad area "Web"'],
    ['fix(agentk/web/rail): x', 'Bad area "web/rail"'],
    ['fix(agentk):x', 'Start with'],
    ['fix(agentk): ', 'The summary is empty'],
    ['fix(agentk):  x', 'starts with a space'],
    ['fix(agentk): x.', 'ends with a period'],
    ['fix(agentk): x. (#12)', 'ends with a period'],
    [`fix(agentk/web): ${'a'.repeat(56)}`, 'The title is 73 characters; keep it to 72.'],
    ['nit(agentk)!: x', "\"nit\" doesn't release, so it can't be breaking"],
    ['chore(meko/store)!: x', "\"chore\" doesn't release"],
    ['fix(agentk): (#12)', 'The summary is empty'],
    ['fix(agentk): x ', 'ends with a space'],
    ['fix(agentk): x  (#12)', 'ends with a space'],
    ['fix(agentk): x\nmore', 'one line'],
  ])('rejects %j', (title, why) => {
    const r = check(title);
    expect(r.status).toBe(1);
    expect(r.stderr).toContain(why);
    // Every rejection shows the rules.
    expect(r.stderr).toContain('Format: <type>(<component>[/<area>])[!]: <summary>');
    expect(r.stderr).toContain('Types: feat, fix, perf, deps, ci, test, docs, nit, chore');
    expect(r.stderr).toContain('Components: agentk, meko, presenter, repo');
  });

  it('skips only the length limit with --no-length', () => {
    const long = `deps(agentk): bump the development group across 1 directory with ${'x'.repeat(40)}`;
    expect(check(long).status).toBe(1);
    expect(check('--no-length', long).status).toBe(0);
    expect(check('--no-length', 'refactor(agentk): x').status).toBe(1);
    expect(check('--no-length', 'deps: x').status).toBe(1);
  });

  it('counts characters, not bytes, in any locale', () => {
    // 72 characters, 190 bytes in UTF-8.
    const title = `fix(agentk): ${'—'.repeat(59)}`;
    for (const LC_ALL of ['C', 'en_US.UTF-8']) {
      const env = { ...process.env, LC_ALL };
      expect(spawnSync('sh', [CHECK, title], { encoding: 'utf8', env }).status, LC_ALL).toBe(0);
      const r = spawnSync('sh', [CHECK, `${title}—`], { encoding: 'utf8', env });
      expect(r.stderr, LC_ALL).toContain('The title is 73 characters');
    }
  });

  it('exits 2 on bad usage', () => {
    expect(check().status).toBe(2);
    expect(check('fix(agentk): a', 'fix(agentk): b').status).toBe(2);
  });
});

describe('the PR title check', () => {
  const workflow = (name: string) => readFileSync(fileURLToPath(new URL(`../../../../.github/workflows/${name}`, import.meta.url)), 'utf8');

  it('runs the script on the PR title, and again when the PR is edited', () => {
    const wf = workflow('pr-title.yml');
    expect(wf).toMatch(/^ {2}pull_request:\n {4}types: \[opened, synchronize, reopened, edited\]$/m);
    // Merging into main requires the check by this name.
    expect(wf).toContain('    name: PR title\n');
    // The title goes through the environment, never into the script.
    expect(wf).toContain('TITLE: ${{ github.event.pull_request.title }}');
    expect(wf).toContain('sh scripts/check-title.sh "$TITLE"');
    expect(wf).toContain('sh scripts/check-title.sh --no-length "$TITLE"');
    expect(wf.match(/\$\{\{/g)).toHaveLength(2);
  });

  it('stays out of CI, so an edit never starts a CI run that skips the required tests', () => {
    const ci = workflow('ci.yml');
    expect(ci).not.toContain('edited');
    expect(ci).not.toContain('check-title');
  });
});

describe('release types', () => {
  let root: string;
  let repo: string;
  beforeEach(() => {
    root = tempDir('akl-titles-');
    repo = gitRepo(join(root, 'repo'));
    execFileSync('git', ['tag', 'v0.20260101.0'], { cwd: repo });
  });
  afterEach(() => rmSync(root, { recursive: true, force: true }));

  const commit = (msg: string) => execFileSync('git', ['commit', '-q', '--allow-empty', '-m', msg], { cwd: repo });
  const notes = () => spawnSync('sh', [NOTES], { cwd: repo, encoding: 'utf8' });

  it('only releases on types the checker accepts, and both scripts agree on which', () => {
    const sections = [...readFileSync(NOTES, 'utf8').matchAll(/^\s*section '[^']+' '([^']+)'$/gm)].map((m) => m[1]!);
    expect(sections.sort()).toEqual(['deps', 'feat', 'fix', 'perf']);
    expect(list(NOTES, 'RELEASE').sort()).toEqual(sections);
    expect(list(CHECK, 'RELEASE').sort()).toEqual(sections);
    // What's new in the app (src/server/releasenotes.ts) lists the same types.
    expect([...RELEASE_TYPES].sort()).toEqual(sections);
    for (const t of sections) expect(checkerTypes()).toContain(t);
  });

  it('does not release nit, docs, ci, test or chore commits', () => {
    for (const t of ['nit', 'docs', 'ci', 'test', 'chore']) commit(`${t}(agentk): something`);
    commit('chore(meko/store): add a table');
    expect(notes().status).toBe(3);
  });

  it('does not release a breaking marker on a type that does not release', () => {
    commit('chore(repo)!: drop the old dev script');
    commit('docs(agentk): rewrite the install guide\n\nBREAKING CHANGE: the old guide is gone');
    expect(notes().status).toBe(3);
  });

  it('lists breaking changes from releasing commits', () => {
    commit('feat(agentk/server)!: drop the v1 API');
    commit('fix(agentk): rename the config key\n\nBREAKING CHANGE: [web] is now [ui]');
    commit('nit(agentk)!: not this one');
    const r = notes();
    expect(r.status).toBe(0);
    expect(r.stdout).toContain('### Breaking changes\n\n- BREAKING CHANGE: [web] is now [ui]\n- feat(agentk/server)!: drop the v1 API\n\n');
    expect(r.stdout).not.toContain('not this one');
  });

  it('releases deps commits under Dependencies', () => {
    commit('fix: an old subject, from before titles named the component');
    commit('nit(agentk): fix a comment');
    commit('deps(agentk): bump fastify from 5.12.5 to 5.13.0');
    commit('fix(agentk/web): stop clipping the rail');
    const r = notes();
    expect(r.status).toBe(0);
    expect(r.stdout).toContain('### Fixes\n\n- agentk/web: stop clipping the rail');
    expect(r.stdout).toContain('- an old subject, from before titles named the component');
    expect(r.stdout).toMatch(/### Dependencies\n\n- agentk: bump fastify from 5\.12\.5 to 5\.13\.0 \([0-9a-f]+\)/);
    expect(r.stdout.indexOf('### Fixes')).toBeLessThan(r.stdout.indexOf('### Dependencies'));
    expect(r.stdout).not.toContain('fix a comment');
  });
});
