import { execFileSync, spawnSync } from 'node:child_process';
import { readFileSync, rmSync } from 'node:fs';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { gitRepo, tempDir } from '../helpers.js';
import { RELEASE_TYPES } from '../../src/server/releasenotes.js';

// The repo root's scripts/check-title.sh holds the commit subject and PR title rules the commit
// skill follows; AgentK's scripts/release-notes.sh decides which of those types release.
const CHECK = fileURLToPath(new URL('../../../../scripts/check-title.sh', import.meta.url));
const NOTES = fileURLToPath(new URL('../../scripts/release-notes.sh', import.meta.url));

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

  it.each([
    ...['feat', 'fix', 'perf', 'deps', 'ci', 'test', 'docs', 'nit', 'chore'].map((t) => `${t}: do the thing`),
    'fix(web): stop clipping the rail',
    'nit(some-new-area): fix a comment',
    'nit(v2): fix a comment',
    'feat(meko)!: drop the old datapack format',
    'deps!: bump fastify from 5.13.0 to 6.0.0',
    'fix: handle a: colon in the summary',
    `fix: ${'a'.repeat(67)}`,
    `fix: ${'a'.repeat(67)} (#123)`,
  ])('accepts %s', (title) => {
    const r = check(title);
    expect(r.stderr).toBe('');
    expect(r.status).toBe(0);
  });

  it.each([
    ['Update foo', 'Start with'],
    ['refactor: x', 'Unknown type "refactor"'],
    ['feature: x', 'Unknown type "feature"'],
    ['Fix: x', 'Unknown type "Fix"'],
    ['Revert "feat: x"', 'Unknown type'],
    ['(web): x', 'Unknown type ""'],
    ['fix(Web): x', 'Bad component "(Web)"'],
    ['fix(): x', 'Bad component "()"'],
    ['fix(web) : x', 'Bad component'],
    ['fix!(web): x', 'Bad component'],
    ['fix:x', 'Start with'],
    ['fix: ', 'The summary is empty'],
    ['fix:  x', 'starts with a space'],
    ['fix: x.', 'ends with a period'],
    ['fix: x. (#12)', 'ends with a period'],
    [`fix: ${'a'.repeat(68)}`, 'The title is 73 characters; keep it to 72.'],
    ['nit!: x', "\"nit\" doesn't release, so it can't be breaking"],
    ['chore(web)!: x', "\"chore\" doesn't release"],
    ['fix: (#12)', 'The summary is empty'],
    ['fix: x ', 'ends with a space'],
    ['fix: x  (#12)', 'ends with a space'],
    ['fix: x\nmore', 'one line'],
  ])('rejects %j', (title, why) => {
    const r = check(title);
    expect(r.status).toBe(1);
    expect(r.stderr).toContain(why);
    // Every rejection shows the rules.
    expect(r.stderr).toContain('Format: <type>[(<component>)][!]: <summary>');
    expect(r.stderr).toContain('Types: feat, fix, perf, deps, ci, test, docs, nit, chore');
  });

  it('skips only the length limit with --no-length', () => {
    const long = `deps: bump the development group across 1 directory with ${'x'.repeat(40)}`;
    expect(check(long).status).toBe(1);
    expect(check('--no-length', long).status).toBe(0);
    expect(check('--no-length', 'refactor: x').status).toBe(1);
  });

  it('counts characters, not bytes, in any locale', () => {
    // 72 characters, 140 bytes in UTF-8.
    const title = `fix: ${'—'.repeat(67)}`;
    for (const LC_ALL of ['C', 'en_US.UTF-8']) {
      const env = { ...process.env, LC_ALL };
      expect(spawnSync('sh', [CHECK, title], { encoding: 'utf8', env }).status, LC_ALL).toBe(0);
      const r = spawnSync('sh', [CHECK, `${title}—`], { encoding: 'utf8', env });
      expect(r.stderr, LC_ALL).toContain('The title is 73 characters');
    }
  });

  it('exits 2 on bad usage', () => {
    expect(check().status).toBe(2);
    expect(check('fix: a', 'fix: b').status).toBe(2);
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
    for (const t of ['nit', 'docs', 'ci', 'test', 'chore']) commit(`${t}: something`);
    commit('nit(server): fix a comment');
    expect(notes().status).toBe(3);
  });

  it('does not release a breaking marker on a type that does not release', () => {
    commit('chore!: drop the old dev script');
    commit('docs: rewrite the install guide\n\nBREAKING CHANGE: the old guide is gone');
    expect(notes().status).toBe(3);
  });

  it('lists breaking changes from releasing commits', () => {
    commit('feat(server)!: drop the v1 API');
    commit('fix: rename the config key\n\nBREAKING CHANGE: [web] is now [ui]');
    commit('nit!: not this one');
    const r = notes();
    expect(r.status).toBe(0);
    expect(r.stdout).toContain('### Breaking changes\n\n- BREAKING CHANGE: [web] is now [ui]\n- feat(server)!: drop the v1 API\n\n');
    expect(r.stdout).not.toContain('not this one');
  });

  it('releases deps commits under Dependencies', () => {
    commit('nit: fix a comment');
    commit('deps: bump fastify from 5.12.5 to 5.13.0');
    commit('fix(web): stop clipping the rail');
    const r = notes();
    expect(r.status).toBe(0);
    expect(r.stdout).toContain('### Fixes\n\n- web: stop clipping the rail');
    expect(r.stdout).toMatch(/### Dependencies\n\n- bump fastify from 5\.12\.5 to 5\.13\.0 \([0-9a-f]+\)/);
    expect(r.stdout.indexOf('### Fixes')).toBeLessThan(r.stdout.indexOf('### Dependencies'));
    expect(r.stdout).not.toContain('fix a comment');
  });
});
