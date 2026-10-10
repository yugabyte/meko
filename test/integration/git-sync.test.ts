import { execFileSync } from 'node:child_process';
import { existsSync, mkdirSync, readFileSync, rmSync, symlinkSync, unlinkSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { gitRepo, tempDir } from '../helpers.js';
import { BranchGoneError, addWorktree, checkBranchName, restoreWorktree } from '../../src/server/git.js';
import { changedImage, changes, diffStat } from '../../src/server/changes.js';

const git = (cwd: string, ...a: string[]) => execFileSync('git', a, { cwd, encoding: 'utf8', stdio: ['ignore', 'pipe', 'ignore'] }).trim();
/** git refuses to fetch into a branch being rebased or bisected from 2.38 on. */
const [gitMajor, gitMinor] = git('.', 'version').match(/(\d+)\.(\d+)/)!.slice(1).map(Number);
const gitProtectsRebase = gitMajor > 2 || (gitMajor === 2 && gitMinor >= 38);

function identity(dir: string) {
  git(dir, 'config', 'user.email', 'test@example.com');
  git(dir, 'config', 'user.name', 'Test');
}

describe('new sessions start from an up-to-date base (real git)', () => {
  let home: string;
  let repo: string; // the user's main checkout, main checked out
  let other: string; // someone else's clone, pushing to origin

  beforeEach(() => {
    home = tempDir('akl-sync-');
    const seed = gitRepo(join(home, 'seed'));
    git(home, 'clone', '-q', '--bare', seed, 'origin.git');
    git(home, 'clone', '-q', 'origin.git', 'repo');
    git(home, 'clone', '-q', 'origin.git', 'other');
    repo = join(home, 'repo');
    other = join(home, 'other');
    identity(repo);
    identity(other);
  });
  afterEach(() => rmSync(home, { recursive: true, force: true }));

  /** Pushes a new commit to origin/main from the other clone; returns its sha. */
  function pushUpstream(file = 'upstream.txt', text = 'from upstream\n'): string {
    writeFileSync(join(other, file), text);
    git(other, 'add', '.');
    git(other, 'commit', '-q', '-m', 'upstream change');
    git(other, 'push', '-q', 'origin', 'main');
    return git(other, 'rev-parse', 'HEAD');
  }

  const wt = () => join(home, 'wt');

  it('fast-forwards main in the main checkout and branches from it', async () => {
    const up = pushUpstream();
    expect(await addWorktree(repo, wt(), 'ak/x', 'main')).toEqual({ kind: 'updated' });
    expect(git(repo, 'rev-parse', 'main')).toBe(up);
    expect(existsSync(join(repo, 'upstream.txt'))).toBe(true); // working tree moved too
    expect(git(wt(), 'rev-parse', 'HEAD')).toBe(up);
    expect(await addWorktree(repo, join(home, 'wt2'), 'ak/y', 'main')).toEqual({ kind: 'current' });
  });

  it('moves only the ref when main is not checked out anywhere', async () => {
    git(repo, 'checkout', '-q', '-b', 'side');
    const up = pushUpstream();
    expect(await addWorktree(repo, wt(), 'ak/x', 'main')).toEqual({ kind: 'updated' });
    expect(git(repo, 'rev-parse', 'refs/heads/main')).toBe(up);
    expect(existsSync(join(repo, 'upstream.txt'))).toBe(false); // the checkout on "side" is untouched
    expect(git(wt(), 'rev-parse', 'HEAD')).toBe(up);
  });

  it('leaves a diverged main alone and starts from origin/main', async () => {
    writeFileSync(join(repo, 'local.txt'), 'local\n');
    git(repo, 'add', '.');
    git(repo, 'commit', '-q', '-m', 'local only');
    const local = git(repo, 'rev-parse', 'HEAD');
    const up = pushUpstream();
    const r = await addWorktree(repo, wt(), 'ak/x', 'main');
    expect(r).toMatchObject({ kind: 'skipped', reason: expect.stringMatching(/commits that aren't on origin/) });
    expect(git(repo, 'rev-parse', 'main')).toBe(local);
    expect(git(wt(), 'rev-parse', 'HEAD')).toBe(up);
  });

  it('never overwrites or stashes local edits in the main checkout, even with merge.autoStash on', async () => {
    const up = pushUpstream('README.md', '# changed upstream\n');
    git(repo, 'config', 'merge.autoStash', 'true');
    writeFileSync(join(repo, 'README.md'), '# my edit\n');
    const r = await addWorktree(repo, wt(), 'ak/x', 'main');
    expect(r).toMatchObject({ kind: 'skipped', reason: expect.stringMatching(/couldn't fast-forward it in/) });
    expect(readFileSync(join(repo, 'README.md'), 'utf8')).toBe('# my edit\n');
    expect(git(repo, 'stash', 'list')).toBe('');
    expect(git(wt(), 'rev-parse', 'HEAD')).toBe(up);
  });

  it("leaves a base checked out in another session's worktree alone", async () => {
    const before = git(repo, 'rev-parse', 'HEAD');
    await addWorktree(repo, join(home, 'wt-a'), 'ak/a', 'main');
    git(repo, 'push', '-q', 'origin', 'ak/a');
    // ak/a moves on origin (pushed from elsewhere); a new session starts from ak/a.
    git(other, 'fetch', '-q', 'origin');
    git(other, 'checkout', '-q', 'ak/a');
    writeFileSync(join(other, 'a.txt'), 'a\n');
    git(other, 'add', '.');
    git(other, 'commit', '-q', '-m', 'on a');
    git(other, 'push', '-q', 'origin', 'ak/a');
    const up = git(other, 'rev-parse', 'HEAD');
    const r = await addWorktree(repo, wt(), 'ak/b', 'ak/a');
    expect(r).toMatchObject({ kind: 'skipped', reason: expect.stringMatching(/checked out in another worktree/) });
    expect(git(join(home, 'wt-a'), 'rev-parse', 'HEAD')).toBe(before); // the other session's worktree didn't move
    expect(existsSync(join(home, 'wt-a', 'a.txt'))).toBe(false);
    expect(git(wt(), 'rev-parse', 'HEAD')).toBe(up); // started from origin/ak/a
  });

  it.runIf(gitProtectsRebase)('never moves a branch that is being rebased in the main checkout', async () => {
    const main = git(repo, 'rev-parse', 'main');
    // Stop a rebase of main part way: HEAD is detached and main is mid-rewrite.
    expect(() => git(repo, 'rebase', '--root', '--exec', 'false')).toThrow();
    const up = pushUpstream();
    const r = await addWorktree(repo, wt(), 'ak/x', 'main');
    expect(r).toMatchObject({ kind: 'skipped' });
    expect(git(repo, 'rev-parse', 'refs/heads/main')).toBe(main);
    expect(git(wt(), 'rev-parse', 'HEAD')).toBe(up);
  });

  it('refuses base branch names git would read as options', async () => {
    for (const base of ['--upload-pack=touch pwned', '-x', 'a..b', '']) {
      await expect(addWorktree(repo, wt(), 'ak/x', base), base).rejects.toThrow(/isn't a valid branch name/);
    }
    expect(existsSync(wt())).toBe(false);
  });

  it('blames a missing repo, not a good branch name, when git can\'t run', async () => {
    await expect(checkBranchName(repo, 'main')).resolves.toBeUndefined();
    const gone = join(home, 'gone');
    await expect(checkBranchName(gone, 'main')).rejects.toThrow(`${gone} isn't there any more, so git can't run in it. Put it back there and try again.`);
    await expect(checkBranchName(gone, 'a..b')).rejects.toThrow(/isn't there any more/);
    await expect(addWorktree(gone, wt(), 'ak/x', 'main')).rejects.toThrow(/isn't there any more/);
  });

  it('restores a missing worktree from its branch, or origin, without touching main', async () => {
    await addWorktree(repo, wt(), 'ak/x', 'main');
    writeFileSync(join(wt(), 'work.txt'), 'work\n');
    git(wt(), 'add', '.');
    git(wt(), 'commit', '-q', '-m', 'work');
    const work = git(wt(), 'rev-parse', 'HEAD');
    git(wt(), 'push', '-q', 'origin', 'ak/x');
    const main = git(repo, 'rev-parse', 'main');
    pushUpstream();
    git(repo, 'fetch', '-q', 'origin');

    rmSync(wt(), { recursive: true, force: true });
    await restoreWorktree(repo, wt(), 'ak/x', 'main');
    expect(git(wt(), 'rev-parse', 'HEAD')).toBe(work);
    expect(git(repo, 'rev-parse', 'main')).toBe(main); // no fast-forward during recovery

    // The local branch is gone but origin has it.
    rmSync(wt(), { recursive: true, force: true });
    git(repo, 'worktree', 'prune');
    git(repo, 'branch', '-D', 'ak/x');
    await restoreWorktree(repo, wt(), 'ak/x', 'main');
    expect(git(wt(), 'rev-parse', 'HEAD')).toBe(work);

    // Gone everywhere: fail loudly rather than start a fresh branch.
    rmSync(wt(), { recursive: true, force: true });
    git(repo, 'worktree', 'prune');
    git(repo, 'branch', '-D', 'ak/x');
    git(repo, 'update-ref', '-d', 'refs/remotes/origin/ak/x');
    await expect(restoreWorktree(repo, wt(), 'ak/x', 'main')).rejects.toBeInstanceOf(BranchGoneError);
    expect(existsSync(wt())).toBe(false);
    expect(git(repo, 'branch', '--list', 'ak/x')).toBe('');
  });

  it('works without an origin remote, and reuses an existing branch', async () => {
    const solo = gitRepo(join(home, 'solo'));
    expect(await addWorktree(solo, wt(), 'ak/x', 'main')).toEqual({ kind: 'no-upstream' });
    git(solo, 'worktree', 'remove', wt());
    expect(await addWorktree(solo, wt(), 'ak/x', 'main')).toBeNull();
  });
});

describe('changes vs. the base (real git)', () => {
  let home: string;
  let repo: string;
  let wt: string;

  beforeEach(async () => {
    home = tempDir('akl-changes-');
    repo = gitRepo(join(home, 'repo'));
    writeFileSync(join(repo, 'gone.txt'), 'bye\n');
    writeFileSync(join(repo, 'old name.txt'), 'one\ntwo\nthree\nfour\nfive\n');
    git(repo, 'add', '.');
    git(repo, 'commit', '-q', '-m', 'more files');
    wt = join(home, 'wt');
    await addWorktree(repo, wt, 'ak/x', 'main');
  });
  afterEach(() => rmSync(home, { recursive: true, force: true }));

  it('lists committed, uncommitted and untracked changes with a patch each', async () => {
    expect(await changes(wt, 'main')).toMatchObject({ files: [], added: 0, removed: 0 }); // a fresh branch
    // Committed on the branch: modify, add, delete, rename.
    writeFileSync(join(wt, 'README.md'), '# test\nmore\n');
    writeFileSync(join(wt, 'added.txt'), 'a\nb\n');
    unlinkSync(join(wt, 'gone.txt'));
    git(wt, 'mv', 'old name.txt', 'new name.txt');
    git(wt, 'add', '-A');
    git(wt, 'commit', '-q', '-m', 'work');
    // Not committed: an edit to a committed file, an untracked text file and an untracked binary.
    writeFileSync(join(wt, 'added.txt'), 'a\nb\nc\n');
    writeFileSync(join(wt, 'draft.md'), 'x\ny');
    writeFileSync(join(wt, 'blob.bin'), Buffer.from([0, 1, 2, 3]));

    const c = (await changes(wt, 'main'))!;
    expect(c.base).toBe('main');
    const by = Object.fromEntries(c.files.map((f) => [f.path, f]));
    expect(c.files.map((f) => f.path)).toEqual(['README.md', 'added.txt', 'blob.bin', 'draft.md', 'gone.txt', 'new name.txt']);
    expect(by['README.md']).toMatchObject({ kind: 'modified', added: 1, removed: 0, uncommitted: false });
    expect(by['README.md'].patch).toMatch(/^diff --git [\s\S]*\n@@ [\s\S]*\n\+more$/);
    expect(by['added.txt']).toMatchObject({ kind: 'added', added: 3, removed: 0, uncommitted: true });
    expect(by['gone.txt']).toMatchObject({ kind: 'deleted', added: 0, removed: 1 });
    expect(by['new name.txt']).toMatchObject({ kind: 'renamed', oldPath: 'old name.txt', added: 0, removed: 0 });
    expect(by['draft.md']).toMatchObject({ kind: 'untracked', added: 2, uncommitted: true, patch: '@@ -0,0 +1,2 @@\n+x\n+y\n\\ No newline at end of file' });
    expect(by['blob.bin']).toMatchObject({ kind: 'untracked', binary: true, patch: null, added: 0 });
    expect(c.omittedPatches).toBe(0);
    expect(c.added).toBe(6);
    expect(c.removed).toBe(1);

    // The footer's numbers match the panel, untracked files included.
    expect(await diffStat(wt, 'main')).toEqual({ added: c.added, removed: c.removed });
  });

  it('serves both sides of a changed image, and only images inside the repo', async () => {
    const png = (n: number) => Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0, 0, 0, n]);
    mkdirSync(join(repo, 'img'));
    writeFileSync(join(repo, 'img', 'logo.png'), png(1));
    writeFileSync(join(repo, 'img', 'gone.png'), png(2));
    git(repo, 'add', '.');
    git(repo, 'commit', '-q', '-m', 'images');
    rmSync(wt, { recursive: true, force: true });
    git(repo, 'worktree', 'prune');
    git(repo, 'branch', '-D', 'ak/x');
    await addWorktree(repo, wt, 'ak/x', 'main');
    writeFileSync(join(wt, 'img', 'logo.png'), png(9)); // uncommitted edit
    unlinkSync(join(wt, 'img', 'gone.png'));
    writeFileSync(join(wt, 'new.png'), png(5)); // untracked

    expect(await changedImage(wt, 'main', 'img/logo.png', 'old')).toEqual({ type: 'image/png', data: png(1) });
    expect(await changedImage(wt, 'main', 'img/logo.png', 'new')).toEqual({ type: 'image/png', data: png(9) });
    expect(await changedImage(wt, 'main', 'new.png', 'old')).toBeNull();
    expect((await changedImage(wt, 'main', 'new.png', 'new'))?.data).toEqual(png(5));
    expect(await changedImage(wt, 'main', 'img/gone.png', 'new')).toBeNull();
    expect((await changedImage(wt, 'main', 'img/gone.png', 'old'))?.data).toEqual(png(2));
    const c = (await changes(wt, 'main'))!;
    expect(c.files.find((f) => f.path === 'img/logo.png')).toMatchObject({ binary: true, patch: null });

    // Never a non-image, a path out of the repo, or a symlink that leads out of it.
    writeFileSync(join(home, 'secret.png'), png(7));
    symlinkSync(join(home, 'secret.png'), join(wt, 'link.png'));
    for (const [path, side] of [['README.md', 'new'], ['../secret.png', 'new'], ['img/../../secret.png', 'old'], [join(home, 'secret.png'), 'new'], ['link.png', 'new']] as const) {
      await expect(changedImage(wt, 'main', path, side), `${path} (${side})`).rejects.toThrow(/not a previewable image/);
    }
  });

  it('keeps every patch when a file became a symlink, whatever the user\'s diff config', async () => {
    // Settings that would change the patch format if they reached our git calls.
    for (const [k, v] of [['diff.suppressBlankEmpty', 'true'], ['diff.noprefix', 'true'], ['diff.submodule', 'diff'], ['color.diff', 'always'], ['core.quotePath', 'true']]) {
      git(repo, 'config', k, v);
    }
    writeFileSync(join(wt, 'README.md'), '# test\n\nmore\n');
    unlinkSync(join(wt, 'gone.txt'));
    symlinkSync('README.md', join(wt, 'gone.txt')); // file → symlink: a type change
    writeFileSync(join(wt, 'tab\tname.txt'), 'x\n');
    writeFileSync(join(wt, 'ünï.txt'), 'x\n');
    git(wt, 'add', '-A');
    git(wt, 'commit', '-q', '-m', 'types');

    const c = (await changes(wt, 'main'))!;
    const by = Object.fromEntries(c.files.map((f) => [f.path, f]));
    expect(c.omittedPatches).toBe(0);
    expect(by['README.md'].patch).toMatch(/^diff --git a\/README.md b\/README.md\n/);
    expect(by['README.md'].patch).toMatch(/\n@@ -1 \+1,3 @@\n # test\n\+\n\+more$/); // the blank line stays "+"
    expect(by['gone.txt'].patch).toMatch(/^diff --git a\/gone.txt b\/gone.txt\ndeleted file mode 100644\n[\s\S]*\nnew file mode 120000\n@@ -1 \+0,0 @@\n-bye\n@@ -0,0 \+1 @@\n\+README.md\n/);
    expect(by['tab\tname.txt'].patch).toMatch(/^diff --git "a\/tab\\tname.txt" "b\/tab\\tname.txt"\n/);
    expect(by['ünï.txt'].patch).toMatch(/\n\+x$/);
  });

  it('bounds how much of untracked files it reads', async () => {
    writeFileSync(join(wt, 'big.log'), 'line\n'.repeat(300_000)); // 1.5 MB: not read
    writeFileSync(join(wt, 'small.txt'), 'a\nb\n');
    const c = (await changes(wt, 'main'))!;
    const by = Object.fromEntries(c.files.map((f) => [f.path, f]));
    expect(by['big.log']).toMatchObject({ kind: 'untracked', added: 0, patch: null });
    expect(by['small.txt']).toMatchObject({ kind: 'untracked', added: 2, patch: '@@ -0,0 +1,2 @@\n+a\n+b' });
  });
});

describe('the diff base of a fork (real git)', () => {
  let home: string;
  let repo: string; // the user's clone of their fork: origin is the fork, upstream the repo it forks
  let maint: string; // a maintainer's clone, pushing to upstream
  let wt: string;
  const png = (n: number) => Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0, 0, 0, n]);

  beforeEach(async () => {
    home = tempDir('akl-fork-');
    const seed = gitRepo(join(home, 'seed'));
    writeFileSync(join(seed, 'docs.txt'), 'old docs\n'.repeat(50));
    writeFileSync(join(seed, 'logo.png'), png(1));
    git(seed, 'add', '.');
    git(seed, 'commit', '-q', '-m', 'docs');
    git(home, 'clone', '-q', '--bare', seed, 'upstream.git');
    git(home, 'clone', '-q', '--bare', 'upstream.git', 'origin.git');
    git(home, 'clone', '-q', 'origin.git', 'repo');
    git(home, 'clone', '-q', 'upstream.git', 'maint');
    repo = join(home, 'repo');
    maint = join(home, 'maint');
    identity(repo);
    identity(maint);
    git(repo, 'remote', 'add', 'upstream', join(home, 'upstream.git'));
    git(repo, 'fetch', '-q', 'upstream');
    wt = join(home, 'wt');
  });
  afterEach(() => rmSync(home, { recursive: true, force: true }));

  /** Upstream moves on without the fork: the docs go and the logo changes. */
  function advanceUpstream() {
    unlinkSync(join(maint, 'docs.txt'));
    writeFileSync(join(maint, 'logo.png'), png(2));
    git(maint, 'add', '-A');
    git(maint, 'commit', '-q', '-m', 'drop the old docs');
    git(maint, 'push', '-q', 'origin', 'main');
  }

  /** One small commit on the session's branch. */
  function work() {
    writeFileSync(join(wt, 'README.md'), '# test\nmore\n');
    git(wt, 'commit', '-q', '-am', 'work');
  }

  it("counts only the branch's work once it's rebased onto upstream", async () => {
    await addWorktree(repo, wt, 'ak/x', 'main'); // starts from the fork's main, now behind
    advanceUpstream();
    work();
    git(wt, 'fetch', '-q', 'upstream');
    git(wt, 'rebase', '-q', 'upstream/main');

    const c = (await changes(wt, 'main'))!;
    expect(c.base).toBe('upstream/main');
    expect(c.files.map((f) => f.path)).toEqual(['README.md']);
    expect(await diffStat(wt, 'main')).toEqual({ added: 1, removed: 0 });
    // The Before side of an image is upstream's.
    expect((await changedImage(wt, 'main', 'logo.png', 'old'))?.data).toEqual(png(2));
  });

  it("uses origin when the fork's base is ahead of upstream's", async () => {
    advanceUpstream();
    git(repo, 'pull', '-q', '--ff-only', join(home, 'upstream.git'), 'main');
    writeFileSync(join(repo, 'fork.txt'), 'fork only\n');
    git(repo, 'add', '.');
    git(repo, 'commit', '-q', '-m', 'fork only');
    git(repo, 'push', '-q', 'origin', 'main');
    await addWorktree(repo, wt, 'ak/x', 'main');
    work();

    const c = (await changes(wt, 'main'))!;
    expect(c.base).toBe('origin/main');
    expect(c.files.map((f) => f.path)).toEqual(['README.md']);
  });

  it('uses origin when the fork and upstream agree, or there is no upstream', async () => {
    await addWorktree(repo, wt, 'ak/x', 'main');
    work();
    expect((await changes(wt, 'main'))!.base).toBe('origin/main');

    git(repo, 'remote', 'remove', 'upstream');
    expect((await changes(wt, 'main'))!.base).toBe('origin/main');
    expect(await diffStat(wt, 'main')).toEqual({ added: 1, removed: 0 });
  });
});
