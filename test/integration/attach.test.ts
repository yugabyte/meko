import { execFileSync } from 'node:child_process';
import { readFileSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { OpenPr, PrInfo, PrRef, PrStatus } from '../../src/server/github.js';

// GitHub is mocked; git (with "GitHub" repos as local bare repos), tmux and the fake claude are real.
const gh = vi.hoisted(() => ({
  info: new Map<string, PrInfo>(),
  /** What findPr finds for a branch name. */
  byBranch: new Map<string, PrRef>(),
  /** What prByNumber finds. */
  byNumber: new Map<number, PrRef>(),
  mine: [] as OpenPr[],
  status: null as null | PrStatus,
}));

vi.mock('../../src/server/github.js', async (orig) => {
  const real = await orig<typeof import('../../src/server/github.js')>();
  return {
    ...real,
    prInfo: async (_cwd: string, repo: string, n: number) => {
      const v = gh.info.get(`${repo}#${n}`);
      if (!v) throw new Error(`no PR ${repo}#${n}`);
      return v;
    },
    findPr: async (_cwd: string, branch: string) => gh.byBranch.get(branch) ?? null,
    prByNumber: async (_cwd: string, n: number) => {
      const v = gh.byNumber.get(n);
      if (!v) throw new Error(`no PR #${n}`);
      return v;
    },
    myOpenPrs: async () => gh.mine,
    // Like GitHub after a push: the PR's head is the branch tip.
    prStatus: async (w: string, n: number) => ({ ...gh.status!, number: n, headRefOid: tip(w) }),
    issueText: async () => ({ title: '', body: '' }),
    viewerLogin: async () => 'me',
    botLogins: async () => [],
  };
});

function tip(dir: string): string {
  try {
    return execFileSync('git', ['rev-parse', 'HEAD'], { cwd: dir, encoding: 'utf8' }).trim();
  } catch {
    return '';
  }
}

const { makeEnv, gitRepo, waitFor } = await import('../helpers.js');
const { get } = await import('../../src/server/db.js');
const { taskPrs } = await import('../../src/server/taskprs.js');
type TestEnv = Awaited<ReturnType<typeof makeEnv>>;

const git = (cwd: string, ...a: string[]) => execFileSync('git', a, { cwd, encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'] }).trim();

describe('sessions on an existing branch or an open PR (real tmux + git, fake claude, GitHub mocked)', () => {
  let env: TestEnv;
  /** "github.com/acme/demo": a bare repo, reached through url.insteadOf. */
  let upstream: string;
  /** "github.com/dana/demo": Dana's fork of it. */
  let fork: string;
  /** A clone of acme/demo under the scanned repos folder: the project's repo. */
  let clone: string;
  /** Dana, pushing to acme/demo and to her fork. */
  let author: string;

  const alias = (dir: string, repo: string, bare: string) => git(dir, 'config', '--add', `url.${bare}.insteadOf`, `https://github.com/${repo}.git`);

  /** A commit on <branch> in the author's clone, pushed to <to> (acme/demo by default) as <as>. */
  function push(branch: string, file: string, to = upstream, as = branch): string {
    git(author, 'checkout', '-q', '-B', branch, 'origin/main');
    writeFileSync(join(author, file), `${file}\n`);
    git(author, 'add', '.');
    git(author, 'commit', '-q', '-m', `change ${file}`);
    git(author, 'push', '-q', '-f', to, `HEAD:refs/heads/${as}`);
    return git(author, 'rev-parse', 'HEAD');
  }

  function pr(n: number, over: Partial<PrInfo> = {}): PrInfo {
    const info: PrInfo = {
      number: n, url: `https://github.com/acme/demo/pull/${n}`, title: 'Add the widget', author: 'me', state: 'OPEN', baseRefName: 'main',
      headRefOid: '', createdAt: '2026-01-01T00:00:00Z', body: 'Adds the widget.', headRefName: 'feature/widget', headRepo: 'acme/demo',
      isCrossRepository: false, ...over,
    };
    gh.info.set(`acme/demo#${n}`, info);
    return info;
  }

  const project = () => env.core.projects.create({ repoPath: clone, name: 'Widget', datapack: { mode: 'existing', id: 'dp-1', name: 'team/widget' } });
  const event = (id: string, kind: string) =>
    get<{ text: string }>(env.core.db, 'SELECT text FROM session_events WHERE session_id = ? AND kind = ? ORDER BY id DESC LIMIT 1', id, kind)?.text;
  const transcript = (id: string) => readFileSync(join(env.fakeDir, `${env.core.sessions.row(id).claude_session_id}.jsonl`), 'utf8');
  const idle = (id: string) => waitFor(() => env.core.sessions.get(id), (x) => x.status === 'idle', 10000, `${id} idle`);
  const systemPrompt = () => {
    const a = (readFileSync(join(env.fakeDir, 'argv.jsonl'), 'utf8').trim().split('\n').map((l) => JSON.parse(l) as { args: string[] }).at(-1)!).args;
    return a[a.indexOf('--append-system-prompt') + 1];
  };
  const sessionCount = () => get<{ n: number }>(env.core.db, 'SELECT COUNT(*) AS n FROM sessions')!.n;
  const branches = () => git(clone, 'branch', '--list', '--format=%(refname:short)').split('\n').filter(Boolean);

  beforeEach(async () => {
    gh.info.clear();
    gh.byBranch.clear();
    gh.byNumber.clear();
    gh.mine = [];
    gh.status = null;
    env = await makeEnv();
    const seed = gitRepo(join(env.home, 'seed'));
    git(env.home, 'clone', '-q', '--bare', seed, 'acme-demo.git');
    upstream = join(env.home, 'acme-demo.git');
    git(env.home, 'clone', '-q', '--bare', seed, 'dana-demo.git');
    fork = join(env.home, 'dana-demo.git');
    author = join(env.home, 'author');
    git(env.home, 'clone', '-q', upstream, author);
    git(author, 'config', 'user.email', 'dana@example.com');
    git(author, 'config', 'user.name', 'Dana');
    git(author, 'push', '-q', upstream, 'main:refs/heads/release');
    clone = join(env.home, 'code', 'demo');
    git(env.home, '-c', `url.${upstream}.insteadOf=https://github.com/acme/demo.git`, 'clone', '-q', 'https://github.com/acme/demo.git', clone);
    alias(clone, 'acme/demo', upstream);
    alias(clone, 'dana/demo', fork);
    git(clone, 'config', 'user.email', 'me@example.com');
    git(clone, 'config', 'user.name', 'Me');
  });
  afterEach(async () => {
    await env.cleanup();
  });

  it("works on a PR's branch against its base, with the PR linked and AgentK told to build on it", async () => {
    const p = await project();
    const head = push('feature/widget', 'widget.ts');
    pr(12, { baseRefName: 'release' });
    const s = await env.core.sessions.create(p.id, { name: '', prompt: 'Address the review', kind: 'task', start: 'code', pr: { repo: 'acme/demo', number: 12 } });
    await idle(s.id);
    const got = env.core.sessions.get(s.id);
    expect(got).toMatchObject({
      name: 'Add the widget', stage: 'code', branch: 'feature/widget', baseBranch: 'release', branchReused: true,
      prNumber: 12, prUrl: 'https://github.com/acme/demo/pull/12',
    });
    expect(git(got.worktreePath, 'rev-parse', 'HEAD')).toBe(head);
    expect(event(s.id, 'created')).toBe('Started on feature/widget, the branch of PR #12 (base release)');
    expect(env.core.sessions.row(s.id)).toMatchObject({ push_remote: null, push_branch: null });
    // The first prompt says what's there before the task.
    const t = transcript(s.id);
    expect(t).toContain("This session continues existing work: you're on the branch feature/widget, which is the branch of PR #12 (https://github.com/acme/demo/pull/12)");
    expect(t).toContain('`gh pr view 12 --comments`');
    expect(t.indexOf('This session continues existing work')).toBeLessThan(t.indexOf('Address the review'));
    expect(systemPrompt()).toContain('The branch existed before this session and is the branch of PR #12');
    // What Try again would ask for.
    expect(env.core.sessions.row(s.id).branch_reused).toBe(1);

    // Moving to PR & Review picks up the linked PR, not a new one.
    gh.byNumber.set(12, { number: 12, url: got.prUrl!, state: 'OPEN', createdAt: '2026-01-01T00:00:00Z', baseRefName: 'release' });
    gh.status = {
      number: 12, url: got.prUrl!, title: 'Add the widget', body: '', state: 'OPEN', reviews: [], comments: [],
      statusCheckRollup: [], reviewDecision: '', mergeable: 'MERGEABLE', mergeStateStatus: 'CLEAN', headRefOid: '',
    };
    await env.core.sessions.move(s.id, { stage: 'review' });
    expect(env.core.sessions.get(s.id)).toMatchObject({ stage: 'review', prNumber: 12 });
    expect(event(s.id, 'pr-again')).toBe('Back on PR #12');
  });

  it("works on a fork's PR from the fork's remote, on a branch of its own name, and pushes there", async () => {
    const p = await project();
    // Dana's PR is from her fork's main, and the clone has a main of its own.
    const head = push('main', 'fork.ts', fork, 'main');
    pr(20, { headRefName: 'main', headRepo: 'dana/demo', isCrossRepository: true, title: 'Fix from a fork' });
    await expect(env.core.sessions.create(p.id, { name: 'Fork', kind: 'task', pr: { repo: 'acme/demo', number: 20 } }))
      .rejects.toMatchObject({ status: 400, message: expect.stringMatching(/isn't a remote of demo\. Add it \(git remote add dana https:\/\/github\.com\/dana\/demo\.git\)/) });
    expect(sessionCount()).toBe(0);

    git(clone, 'remote', 'add', 'dana', 'https://github.com/dana/demo.git');
    const s = await env.core.sessions.create(p.id, { name: 'Fork', prompt: 'carry on', kind: 'task', start: 'code', pr: { repo: 'acme/demo', number: 20 } });
    await idle(s.id);
    const got = env.core.sessions.get(s.id);
    expect(got).toMatchObject({ branch: 'dana-main', prNumber: 20, branchReused: true });
    expect(git(got.worktreePath, 'rev-parse', 'HEAD')).toBe(head);
    expect(git(clone, 'rev-parse', 'main')).not.toBe(head); // the local main is untouched
    expect(env.core.sessions.row(s.id)).toMatchObject({ push_remote: 'dana', push_branch: 'main' });
    expect(git(clone, 'config', '--get', 'branch.dana-main.remote')).toBe('dana');
    expect(git(clone, 'config', '--get', 'branch.dana-main.merge')).toBe('refs/heads/main');
    expect(transcript(s.id)).toContain('Push this branch to dana as main, where the PR\'s head is, not to origin: `git push dana HEAD:main`.');
    expect(systemPrompt()).toContain('`git push dana HEAD:main`');

    // Its own listing shows it in use; one from a fork with no remote here says how to add it.
    gh.mine = [
      { repo: 'acme/demo', number: 20, title: 'Fix from a fork', url: 'https://github.com/acme/demo/pull/20', headRefName: 'main', headRepo: 'dana/demo', baseRefName: 'main', isCrossRepository: true, updatedAt: '2026-01-02T00:00:00Z' },
      { repo: 'acme/demo', number: 21, title: 'Other fork', url: 'https://github.com/acme/demo/pull/21', headRefName: 'fix', headRepo: 'eve/demo', baseRefName: 'main', isCrossRepository: true, updatedAt: '2026-01-01T00:00:00Z' },
    ];
    const list = await taskPrs(env.core, p.id, {});
    expect(list.prs.map((x) => [x.number, x.fork, x.blocked])).toEqual([
      [20, true, 'The session "Fork" works on its branch.'],
      [21, true, 'Its branch is on eve/demo, which isn\'t a remote here. Add it: git remote add eve https://github.com/eve/demo.git'],
    ]);
  });

  it('refuses a closed PR, a PR of another repo, and a typed PR lookup that isn\'t open', async () => {
    const p = await project();
    pr(30, { state: 'CLOSED' });
    await expect(env.core.sessions.create(p.id, { name: 'X', kind: 'task', pr: { repo: 'acme/demo', number: 30 } }))
      .rejects.toMatchObject({ status: 400, message: expect.stringMatching(/is closed/) });
    await expect(env.core.sessions.create(p.id, { name: 'X', kind: 'task', pr: { repo: 'other/thing', number: 1 } }))
      .rejects.toMatchObject({ status: 400, message: expect.stringMatching(/isn't a clone of other\/thing/) });
    await expect(taskPrs(env.core, p.id, { pr: '#30' })).rejects.toMatchObject({ status: 400, message: 'PR #30 of acme/demo is closed.' });
    push('feature/widget', 'w.ts');
    pr(31);
    const found = await taskPrs(env.core, p.id, { pr: 'https://github.com/acme/demo/pull/31' });
    expect(found.prs).toMatchObject([{ number: 31, headRefName: 'feature/widget', fork: false, blocked: null }]);
    expect(sessionCount()).toBe(0);
  });

  it('works on an existing branch: refuses a missing one, catches up with origin, and links its open PR', async () => {
    const p = await project();
    await expect(env.core.sessions.create(p.id, { name: 'Typo', kind: 'task', branch: 'feature/nope', existingBranch: true }))
      .rejects.toMatchObject({ status: 400, message: "Branch feature/nope isn't in demo or on its origin. Check the name, or pick New branch to start it." });
    await expect(env.core.sessions.create(p.id, { name: 'Empty', kind: 'task', existingBranch: true }))
      .rejects.toMatchObject({ status: 400 });
    expect(sessionCount()).toBe(0);

    // A local branch behind origin is fast-forwarded first.
    push('feature/behind', 'one.ts');
    git(clone, 'fetch', '-q', 'origin');
    git(clone, 'branch', 'feature/behind', 'origin/feature/behind');
    git(author, 'checkout', '-q', 'feature/behind');
    writeFileSync(join(author, 'two.ts'), 'two\n');
    git(author, 'add', '.');
    git(author, 'commit', '-q', '-m', 'two');
    git(author, 'push', '-q', upstream, 'feature/behind');
    const latest = git(author, 'rev-parse', 'HEAD');
    gh.byBranch.set('feature/behind', { number: 5, url: 'https://github.com/acme/demo/pull/5', state: 'OPEN', createdAt: '2026-01-01T00:00:00Z', baseRefName: 'release' });
    const s = await env.core.sessions.create(p.id, { name: 'Behind', prompt: 'hi', kind: 'task', start: 'code', branch: 'feature/behind', existingBranch: true, baseBranch: 'main' });
    await idle(s.id);
    expect(git(env.core.sessions.get(s.id).worktreePath, 'rev-parse', 'HEAD')).toBe(latest);
    expect(event(s.id, 'created')).toBe('Started on the existing branch feature/behind (base main); fast-forwarded it to origin/feature/behind');
    expect(env.core.sessions.get(s.id)).toMatchObject({ prNumber: 5, baseBranch: 'release', branchReused: true });
    expect(event(s.id, 'pr-linked')).toBe('Linked PR #5; base set to release, its base');
    expect(transcript(s.id)).toContain('which is the branch of PR #5');

    // A local branch with commits origin lacks is used as it is.
    push('feature/diverged', 'a.ts');
    git(clone, 'fetch', '-q', 'origin');
    git(clone, 'branch', 'feature/diverged', 'origin/feature/diverged');
    const wt = join(env.home, 'side');
    git(clone, 'worktree', 'add', '-q', wt, 'feature/diverged');
    writeFileSync(join(wt, 'mine.ts'), 'mine\n');
    git(wt, 'add', '.');
    git(wt, 'commit', '-q', '-m', 'mine');
    const mine = git(wt, 'rev-parse', 'HEAD');
    git(clone, 'worktree', 'remove', wt);
    push('feature/diverged', 'b.ts');
    const d = await env.core.sessions.create(p.id, { name: 'Diverged', prompt: 'hi', kind: 'task', start: 'code', branch: 'feature/diverged', existingBranch: true });
    await idle(d.id);
    expect(git(env.core.sessions.get(d.id).worktreePath, 'rev-parse', 'HEAD')).toBe(mine);
    expect(event(d.id, 'created')).toBe("Started on the existing branch feature/diverged (base main); it wasn't updated from origin/feature/diverged: local feature/diverged has commits that aren't on origin");
    expect(env.core.sessions.get(d.id).prNumber).toBeNull();

    // Discard keeps a branch that existed before the session.
    expect(await env.core.sessions.discardCheck(d.id)).toMatchObject({ unpushedCommits: 0 });
    await env.core.sessions.action(d.id, 'discard');
    expect(branches()).toContain('feature/diverged');
  });
});
