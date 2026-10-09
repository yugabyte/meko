import { execFileSync } from 'node:child_process';
import { existsSync, readFileSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { FoundPr, PrInfo, PrPeople, PrWatch } from '../../src/server/github.js';

// GitHub is mocked; git (with "GitHub" repos as local bare repos), tmux and the fake claude are real.
const gh = vi.hoisted(() => ({
  info: new Map<string, PrInfo>(),
  watch: new Map<string, PrWatch>(),
  found: new Map<string, FoundPr[]>(),
  people: new Map<string, PrPeople>(),
  profiles: new Map<string, { name: string | null; email: string | null }>(),
  searchFails: false,
}));

vi.mock('../../src/server/github.js', async (orig) => {
  const real = await orig<typeof import('../../src/server/github.js')>();
  const one = <T>(m: Map<string, T>, repo: string, n: number): T => {
    const v = m.get(`${repo}#${n}`);
    if (!v) throw new Error(`no PR ${repo}#${n}`);
    return v;
  };
  return {
    ...real,
    prInfo: async (_cwd: string, repo: string, n: number) => one(gh.info, repo, n),
    prWatch: async (_cwd: string, repo: string, n: number) => one(gh.watch, repo, n),
    searchOpenPrs: async (_cwd: string, text: string) => {
      if (gh.searchFails) throw new Error('gh: search is down');
      return gh.found.get(text) ?? [];
    },
    prPeople: async (_cwd: string, repo: string, n: number) => one(gh.people, repo, n),
    userProfile: async (_cwd: string, login: string) => gh.profiles.get(login) ?? { name: null, email: null },
    viewerLogin: async () => 'me',
    issueText: async () => ({ title: '', body: '' }),
  };
});

const { makeEnv, gitRepo, waitFor } = await import('../helpers.js');
const { get, run } = await import('../../src/server/db.js');
const { usageOverview } = await import('../../src/server/usage.js');
type TestEnv = Awaited<ReturnType<typeof makeEnv>>;

const git = (cwd: string, ...a: string[]) => execFileSync('git', a, { cwd, encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'] }).trim();
const DP = '0f1e2d3c-1111-4222-8333-444455556666';

describe('Review a PR sessions (real tmux + git, fake claude, GitHub mocked)', () => {
  let env: TestEnv;
  /** "github.com/acme/demo": a bare repo, reached through url.insteadOf. */
  let upstream: string;
  /** A clone of acme/demo under the scanned repos folder. */
  let clone: string;
  /** Someone pushing to the PR. */
  let author: string;

  /** Makes https://github.com/<repo>.git fetch from <bare> in <dir> (and in git commands run with -c). */
  const alias = (dir: string, repo: string, bare: string) => git(dir, 'config', '--add', `url.${bare}.insteadOf`, `https://github.com/${repo}.git`);

  /** Pushes a new commit to refs/pull/<n>/head of acme/demo; returns it. */
  function pushToPr(n: number, file: string): string {
    writeFileSync(join(author, file), `${file}\n`);
    git(author, 'add', '.');
    git(author, 'commit', '-q', '-m', `change ${file}`);
    git(author, 'push', '-q', upstream, `HEAD:refs/pull/${n}/head`);
    return git(author, 'rev-parse', 'HEAD');
  }

  function pr(n: number, over: Partial<PrInfo> = {}, repo = 'acme/demo'): PrInfo {
    const info: PrInfo = {
      number: n, url: `https://github.com/${repo}/pull/${n}`, title: 'Add the widget', author: 'dana', state: 'OPEN', baseRefName: 'main',
      headRefOid: '', createdAt: new Date().toISOString(),
      body: `Adds the widget.\n\nmeko://${DP}\nmeko_conversation_id:abc123`, ...over,
    };
    gh.info.set(`${repo}#${n}`, info);
    return info;
  }

  const watch = (n: number, w: Partial<PrWatch>) => gh.watch.set(`acme/demo#${n}`, { state: 'OPEN', headRefOid: '', commits: [], reviews: [], ...gh.watch.get(`acme/demo#${n}`), ...w });
  const project = () => env.core.projects.create({ repoPath: env.repo, name: 'Widget', datapack: { mode: 'existing', id: DP, name: 'adlc/proj/demo/widget' } });
  const event = (id: string, kind: string) =>
    get<{ text: string }>(env.core.db, 'SELECT text FROM session_events WHERE session_id = ? AND kind = ? ORDER BY id DESC LIMIT 1', id, kind)?.text;
  const transcript = (id: string) => readFileSync(join(env.fakeDir, `${env.core.sessions.row(id).claude_session_id}.jsonl`), 'utf8');
  const idle = (id: string, what = 'idle') => waitFor(() => env.core.sessions.get(id), (x) => x.status === 'idle', 10000, what);
  const launchArgs = () => readFileSync(join(env.fakeDir, 'argv.jsonl'), 'utf8').trim().split('\n').map((l) => JSON.parse(l) as { args: string[] });
  const systemPrompt = () => {
    const a = launchArgs().at(-1)!.args;
    return a[a.indexOf('--append-system-prompt') + 1];
  };

  beforeEach(async () => {
    gh.info.clear();
    gh.watch.clear();
    gh.found.clear();
    gh.people.clear();
    gh.profiles.clear();
    gh.searchFails = false;
    env = await makeEnv();
    const seed = gitRepo(join(env.home, 'seed'));
    git(env.home, 'clone', '-q', '--bare', seed, 'acme-demo.git');
    upstream = join(env.home, 'acme-demo.git');
    author = join(env.home, 'author');
    git(env.home, 'clone', '-q', upstream, author);
    git(author, 'config', 'user.email', 'dana@example.com');
    git(author, 'config', 'user.name', 'Dana');
    clone = join(env.home, 'code', 'demo');
    git(env.home, '-c', `url.${upstream}.insteadOf=https://github.com/acme/demo.git`, 'clone', '-q', 'https://github.com/acme/demo.git', clone);
    alias(clone, 'acme/demo', upstream);
    git(clone, 'config', 'user.email', 'me@example.com');
    git(clone, 'config', 'user.name', 'Me');
  });
  afterEach(async () => {
    await env.cleanup();
  });

  it("checks out the PR's head on a review branch with nowhere to push, and tells AgentK to get ready and wait", async () => {
    const p = await project();
    const head = pushToPr(7, 'widget.ts');
    pr(7);
    // An image added in the dialog goes with the notes: a review's task is the PR.
    const image = env.core.sessions.stageImage(p.id, Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a])).id;
    const s = await env.core.sessions.create(p.id, {
      name: '', kind: 'review-pr', repoPath: clone, pr: { repo: 'acme/demo', number: 7 }, prompt: 'the polling', images: [image],
    });
    await idle(s.id, 'first turn');
    const got = env.core.sessions.get(s.id);
    expect(got).toMatchObject({
      name: 'Review #7 Add the widget', kind: 'review-pr', stage: 'reviewing-pr', branch: 'ak/review-pr-7', baseBranch: 'main', repoPath: clone,
      prNumber: 7, prUrl: 'https://github.com/acme/demo/pull/7', hasTask: true,
      reviewingPr: { repo: 'acme/demo', title: 'Add the widget', author: 'dana', reviewSha: head, headSha: head, newCommits: 0, prState: 'OPEN' },
    });
    expect(git(got.worktreePath, 'rev-parse', 'HEAD')).toBe(head);
    expect(git(got.worktreePath, 'symbolic-ref', '--short', 'HEAD')).toBe('ak/review-pr-7');
    // No upstream, and a bare `git push` is refused even where the push config would push a new branch.
    expect(() => git(got.worktreePath, 'rev-parse', '--abbrev-ref', '@{upstream}')).toThrow();
    git(clone, 'config', 'push.default', 'current');
    git(clone, 'config', 'push.autoSetupRemote', 'true');
    expect(() => git(got.worktreePath, 'push')).toThrow(/agentk-review-no-push/);
    expect(git(author, 'ls-remote', upstream, 'refs/heads/ak/*')).toBe('');
    expect(event(s.id, 'created')).toBe(`Checked out PR #7 of acme/demo at ${head.slice(0, 7)} on ak/review-pr-7 (base main)`);
    // The first prompt: get ready, sum up, wait; and search Meko on every why-question.
    const t = transcript(s.id);
    expect(t).toContain("You're assisting the user while they review PR #7 (https://github.com/acme/demo/pull/7) by dana");
    expect(t).toContain("The user's notes for you: the polling");
    // The transcript is JSON lines, so the prompt's new lines are escaped there.
    const notes = `The user's notes for you: the polling\n\nImages the user attached (open each with the Read tool to see it):\n- ${join(env.core.paths.pastes, s.id, image)}\n`;
    expect(t).toContain(JSON.stringify(notes).slice(1, -1));
    expect(env.core.sessions.row(s.id).initial_prompt).toBe('Review PR #7 of acme/demo: Add the widget');
    expect(t).toContain('gh pr view 7 --repo acme/demo --comments');
    // The diff is against the remote's base the review fetched, never the local branch.
    expect(t).toContain('the base is origin/main.');
    expect(t).toContain('git diff --stat origin/main...HEAD');
    expect(t).toContain("Don't list problems, rate the code or suggest a verdict");
    expect(t).toContain('Then stop and wait for the user to ask you something.');
    expect(t).toContain('Before the summary, search Meko for the design');
    expect(t).toContain('For every question about why something is the way it is');
    const sys = systemPrompt();
    expect(sys).toContain('PR: acme/demo#7 (https://github.com/acme/demo/pull/7)');
    expect(sys).toContain("Don't review it on your own; answer what they ask.");
    expect(sys).toContain('search Meko before answering questions about why the code is the way it is');
    // Nobody can move it to another stage.
    await expect(env.core.sessions.move(s.id, { stage: 'code' })).rejects.toMatchObject({ status: 409 });
  });

  it('leaves the Meko lines out with Meko off', async () => {
    const p = await project();
    pushToPr(7, 'widget.ts');
    pr(7);
    env.core.mekoStatus = { ...env.core.mekoStatus, configured: false };
    // A review's start is ignored, not checked: it always starts in Reviewing PR.
    const s = await env.core.sessions.create(p.id, { name: 'r', kind: 'review-pr', start: 'review' as never, repoPath: clone, pr: { repo: 'acme/demo', number: 7 } });
    expect(s).toMatchObject({ kind: 'review-pr', stage: 'reviewing-pr' });
    await idle(s.id);
    expect(transcript(s.id)).not.toContain('Meko');
    expect(systemPrompt()).not.toContain('Meko');
  });

  it('refuses a PR that is not open, a repo that is not its clone, and a missing PR, leaving nothing behind', async () => {
    const p = await project();
    pushToPr(7, 'widget.ts');
    pr(7, { state: 'MERGED' });
    const sessions = () => get<{ n: number }>(env.core.db, 'SELECT COUNT(*) AS n FROM sessions')!.n;
    await expect(env.core.sessions.create(p.id, { name: 'r', kind: 'review-pr', repoPath: clone, pr: { repo: 'acme/demo', number: 7 } }))
      .rejects.toMatchObject({ status: 400, message: expect.stringMatching(/is merged, so there's nothing to review/) });
    pr(7);
    // The project's own repo has no remote for acme/demo.
    await expect(env.core.sessions.create(p.id, { name: 'r', kind: 'review-pr', pr: { repo: 'acme/demo', number: 7 } }))
      .rejects.toMatchObject({ status: 400, message: expect.stringMatching(/demo-repo isn't a clone of acme\/demo/) });
    await expect(env.core.sessions.create(p.id, { name: 'r', kind: 'review-pr', repoPath: clone }))
      .rejects.toMatchObject({ status: 400, message: 'Pick the pull request to review.' });
    await expect(env.core.sessions.create(p.id, { name: 'r', kind: 'review-pr', repoPath: clone, pr: { repo: 'acme/demo', number: 8 } }))
      .rejects.toMatchObject({ status: 400, message: expect.stringMatching(/Couldn't read PR #8 of acme\/demo/) });
    expect(sessions()).toBe(0);

    // A head that can't be fetched shows up after the dialog has closed: a failed start, to try again.
    pr(9);
    const { session, done } = await env.core.sessions.begin(p.id, { name: '', kind: 'review-pr', repoPath: clone, pr: { repo: 'acme/demo', number: 9 }, prompt: 'notes' });
    expect(session).toMatchObject({ status: 'starting', startup: { step: 'worktree' } });
    await expect(done).rejects.toMatchObject({ status: 400, message: expect.stringMatching(/^Couldn't fetch PR #9 from origin: /) });
    expect(sessions()).toBe(0);
    expect(env.core.state().failedStarts).toMatchObject([{
      name: 'Review #9 Add the widget', request: { kind: 'review-pr', repoPath: clone, pr: { repo: 'acme/demo', number: 9 }, prompt: 'notes' },
    }]);
  });

  it("fetches a fork's PR from upstream, and gives the same number in two repos its own branch", async () => {
    const p = await project();
    const head = pushToPr(7, 'widget.ts');
    pr(7);
    // A fork: origin is hari/demo, upstream is acme/demo, where the PR is.
    const forkBare = join(env.home, 'hari-demo.git');
    git(env.home, 'clone', '-q', '--bare', upstream, forkBare);
    const fork = join(env.home, 'code', 'hari-demo');
    git(env.home, 'clone', '-q', forkBare, fork);
    git(fork, 'remote', 'set-url', 'origin', 'https://github.com/hari/demo.git');
    git(fork, 'remote', 'add', 'upstream', 'https://github.com/acme/demo.git');
    alias(fork, 'hari/demo', forkBare);
    alias(fork, 'acme/demo', upstream);
    const a = await env.core.sessions.create(p.id, { name: 'a', kind: 'review-pr', repoPath: fork, pr: { repo: 'acme/demo', number: 7 } });
    expect(git(env.core.sessions.get(a.id).worktreePath, 'rev-parse', 'HEAD')).toBe(head);
    await idle(a.id);
    expect(transcript(a.id)).toContain('git diff --stat upstream/main...HEAD');
    // PR #7 of another repo, in the same project.
    const otherBare = join(env.home, 'acme-other.git');
    git(env.home, 'clone', '-q', '--bare', join(env.home, 'seed'), otherBare);
    const otherAuthor = join(env.home, 'other-author');
    git(env.home, 'clone', '-q', otherBare, otherAuthor);
    git(otherAuthor, 'config', 'user.email', 'x@example.com');
    git(otherAuthor, 'config', 'user.name', 'X');
    writeFileSync(join(otherAuthor, 'o.txt'), 'o\n');
    git(otherAuthor, 'add', '.');
    git(otherAuthor, 'commit', '-q', '-m', 'o');
    git(otherAuthor, 'push', '-q', otherBare, 'HEAD:refs/pull/7/head');
    const other = join(env.home, 'code', 'other');
    git(env.home, 'clone', '-q', otherBare, other);
    git(other, 'remote', 'set-url', 'origin', 'https://github.com/acme/other.git');
    alias(other, 'acme/other', otherBare);
    pr(7, {}, 'acme/other');
    const b = await env.core.sessions.create(p.id, { name: 'b', kind: 'review-pr', repoPath: other, pr: { repo: 'acme/other', number: 7 } });
    expect(env.core.sessions.get(b.id)).toMatchObject({ branch: 'ak/review-pr-7-2', repoPath: other, reviewingPr: { repo: 'acme/other' } });
    expect(git(env.core.sessions.get(b.id).worktreePath, 'rev-parse', 'HEAD')).toBe(git(otherAuthor, 'rev-parse', 'HEAD'));
    // A repo that isn't a clone of the PR's repo is refused.
    await expect(env.core.sessions.create(p.id, { name: 'c', kind: 'review-pr', repoPath: other, pr: { repo: 'acme/demo', number: 7 } }))
      .rejects.toMatchObject({ status: 400, message: expect.stringMatching(/other isn't a clone of acme\/demo/) });
    // Discard ends a review too, and takes the PR's head ref with it.
    await env.core.sessions.action(a.id, 'discard', { skipLearn: true });
    expect(() => git(fork, 'rev-parse', '--verify', '--quiet', 'refs/agentk/pr/7')).toThrow();
    expect(git(fork, 'branch', '--list', 'ak/*')).toBe('');
  });

  it('flags new commits without typing anything, updates on request, and closes the review', async () => {
    const p = await project();
    const first = pushToPr(7, 'widget.ts');
    pr(7);
    const s = await env.core.sessions.create(p.id, { name: 'r', kind: 'review-pr', repoPath: clone, pr: { repo: 'acme/demo', number: 7 } });
    await idle(s.id, 'first turn');
    const poll = () => env.core.lifecycle.pollReviewedPr(env.core.sessions.row(s.id));
    watch(7, { headRefOid: first, commits: [first] });
    await poll();
    expect(env.core.sessions.get(s.id).reviewingPr).toMatchObject({ newCommits: 0, headSha: first });

    // The author pushes: the session says so once, and sends AgentK nothing.
    const second = pushToPr(7, 'more.ts');
    watch(7, { headRefOid: second, commits: [first, second] });
    const before = transcript(s.id);
    await poll();
    await poll();
    expect(env.core.sessions.get(s.id).reviewingPr).toMatchObject({ reviewSha: first, headSha: second, newCommits: 1 });
    expect(event(s.id, 'pr-update')).toBe('PR #7 has a new commit');
    expect(get<{ n: number }>(env.core.db, "SELECT COUNT(*) AS n FROM session_events WHERE session_id = ? AND kind = 'pr-update'", s.id)!.n).toBe(1);
    expect(transcript(s.id)).toBe(before);

    // A commit of the user's own on the review branch: Update to latest refuses and moves nothing.
    const wt = env.core.sessions.get(s.id).worktreePath;
    writeFileSync(join(wt, 'mine.txt'), 'x\n');
    await expect(env.core.sessions.action(s.id, 'update-pr')).rejects.toMatchObject({ status: 409, message: expect.stringMatching(/1 uncommitted file/) });
    git(wt, 'add', '.');
    git(wt, 'commit', '-q', '-m', 'my test');
    await expect(env.core.sessions.action(s.id, 'update-pr')).rejects.toMatchObject({ status: 409, message: expect.stringMatching(/has a commit of yours on top of the PR/) });
    git(wt, 'reset', '-q', '--hard', first);

    await env.core.sessions.action(s.id, 'update-pr');
    expect(git(wt, 'rev-parse', 'HEAD')).toBe(second);
    expect(env.core.sessions.get(s.id).reviewingPr).toMatchObject({ reviewSha: second, headSha: second, newCommits: 0 });
    expect(event(s.id, 'pr-update')).toBe(`Updated to PR #7 at ${second.slice(0, 7)}`);
    await waitFor(() => transcript(s.id), (t) => t.includes(`moved from ${first} to ${second} (1 new commit)`), 10000, 'update summary prompt');
    expect(transcript(s.id)).toContain('Without reviewing, show a short summary of what changed');
    await idle(s.id, 'summary turn');
    await waitFor(() => JSON.parse(env.core.sessions.row(s.id).pending_prompts) as object, (q) => !Object.keys(q).length, 5000, 'queue empty');

    // My own review is logged; the PR merges; Close review archives and deletes the branch and the PR's ref.
    watch(7, { reviews: [{ id: 'v1', author: { login: 'me' }, state: 'CHANGES_REQUESTED', body: 'See inline.', submittedAt: new Date().toISOString() }] });
    await poll();
    expect(event(s.id, 'review')).toBe('You requested changes on PR #7');
    watch(7, { state: 'MERGED' });
    await poll();
    expect(env.core.sessions.get(s.id).reviewingPr?.prState).toBe('MERGED');
    expect(event(s.id, 'pr-state')).toBe('PR #7 was merged');
    await env.core.sessions.action(s.id, 'close-review');
    const row = env.core.sessions.row(s.id);
    expect(row).toMatchObject({ desired: 'archived', stage: 'done' });
    expect(git(clone, 'branch', '--list', 'ak/*')).toBe('');
    expect(() => git(clone, 'rev-parse', '--verify', '--quiet', 'refs/agentk/pr/7')).toThrow();
    expect(event(s.id, 'review-closed')).toBe('Closed the review of PR #7');
    expect(existsSync(wt)).toBe(true); // idle, for the next session on the repo
    // Someone else's PR, closed: never one of the user's merged PRs.
    expect(usageOverview(env.core.db).mergedPrs).toBe(0);
  });

  it("keeps the user's own commits when the PR moved to a head not fetched here, and counts only those for Discard", async () => {
    const p = await project();
    const first = pushToPr(7, 'widget.ts');
    pr(7);
    const s = await env.core.sessions.create(p.id, { name: 'r', kind: 'review-pr', repoPath: clone, pr: { repo: 'acme/demo', number: 7 } });
    await idle(s.id, 'first turn');
    const wt = env.core.sessions.get(s.id).worktreePath;
    // The author's commits are on GitHub: Discard doesn't warn about them.
    expect((await env.core.sessions.discardCheck(s.id)).unpushedCommits).toBe(0);
    writeFileSync(join(wt, 'repro.test.ts'), 'test\n');
    git(wt, 'add', '.');
    git(wt, 'commit', '-q', '-m', 'my repro');
    expect((await env.core.sessions.discardCheck(s.id)).unpushedCommits).toBe(1);
    // GitHub reports a head this clone hasn't fetched.
    const unseen = 'f'.repeat(40);
    watch(7, { headRefOid: unseen, commits: [first, unseen] });
    await env.core.lifecycle.pollReviewedPr(env.core.sessions.row(s.id));
    expect(env.core.sessions.get(s.id).reviewingPr).toMatchObject({ headSha: unseen, newCommits: 1 });
    await env.core.sessions.action(s.id, 'close-review');
    expect(git(clone, 'branch', '--list', '--format=%(refname:short)', 'ak/*')).toBe('ak/review-pr-7');
    expect(event(s.id, 'review-closed')).toBe('Closed the review of PR #7; branch ak/review-pr-7 kept, since it has a commit of yours');
  });

  it('sends Changes panel comments as review comments to draft, not changes to make', async () => {
    const p = await project();
    pushToPr(7, 'widget.ts');
    pr(7);
    const s = await env.core.sessions.create(p.id, { name: 'r', kind: 'review-pr', repoPath: clone, pr: { repo: 'acme/demo', number: 7 } });
    await idle(s.id, 'first turn');
    env.core.comments.add(s.id, { path: 'widget.ts', side: 'new', line: 1, endLine: 1, excerpt: 'widget.ts', body: 'Why a file per widget?' });
    await env.core.comments.send(s.id);
    await waitFor(() => transcript(s.id), (t) => t.includes('These are their review comments'), 10000, 'comments prompt');
    expect(transcript(s.id)).toContain('gh api -X POST repos/acme/demo/pulls/7/reviews');
    expect(transcript(s.id)).toContain("Don't add comments of your own.");
    expect(transcript(s.id)).toContain('For every question about why something is the way it is');
  });

  it("lists the project's PRs in any repo, matched to a clone (a fork first), and looks one up", async () => {
    const p = await project();
    const fork = join(env.home, 'code', 'hari-demo');
    git(env.home, 'clone', '-q', upstream, fork);
    git(fork, 'remote', 'set-url', 'origin', 'https://github.com/hari/demo.git');
    git(fork, 'remote', 'add', 'upstream', 'https://github.com/acme/demo.git');
    gh.found.set(DP, [
      { repo: 'acme/demo', number: 7, title: 'Older', author: 'dana', url: 'https://github.com/acme/demo/pull/7', createdAt: '2026-09-01T00:00:00Z' },
      { repo: 'acme/tools', number: 3, title: 'Newer', author: 'dana', url: 'https://github.com/acme/tools/pull/3', createdAt: '2026-09-02T00:00:00Z' },
      { repo: 'acme/demo', number: 7, title: 'Older', author: 'dana', url: 'https://github.com/acme/demo/pull/7', createdAt: '2026-09-01T00:00:00Z' },
    ]);
    const r = await env.core.prReview.reviewable(p.id, {});
    expect(r.prs.map((x) => [x.repo, x.number, x.localRepo])).toEqual([
      ['acme/tools', 3, null],
      ['acme/demo', 7, fork],
    ]);
    expect(r.prs[0].cloneUrl).toBe('https://github.com/acme/tools.git');

    // One typed PR: a bare number in a clone, and a link naming another datapack.
    pr(9, { body: 'Other project.\n\nmeko://9a8b7c6d-0000-4000-8000-000000000000' });
    const typed = await env.core.prReview.reviewable(p.id, { pr: '#9', repoPath: clone });
    expect(typed.prs).toEqual([expect.objectContaining({ repo: 'acme/demo', number: 9, otherDatapackId: '9a8b7c6d-0000-4000-8000-000000000000' })]);
    pr(7);
    expect((await env.core.prReview.reviewable(p.id, { pr: 'https://github.com/acme/demo/pull/7/files' })).prs[0]).toMatchObject({ number: 7, otherDatapackId: null });
    await expect(env.core.prReview.reviewable(p.id, { pr: 'nope' })).rejects.toMatchObject({ status: 400 });
    await expect(env.core.prReview.reviewable(p.id, { pr: '7' })).rejects.toMatchObject({ status: 400, message: expect.stringMatching(/has no GitHub remote/) });

    gh.searchFails = true;
    expect(await env.core.prReview.reviewable(p.id, {})).toEqual({ prs: [], error: expect.stringMatching(/search is down/) });
  });

  it("lists the reviewers of the project's open PRs in each session's repo, once each, with remembered emails", async () => {
    const p = await project();
    const a = await env.core.sessions.create(p.id, { name: 'a', prompt: 'x', kind: 'quick' });
    const b = await env.core.sessions.create(p.id, { name: 'b', prompt: 'x', kind: 'quick', repoPath: clone });
    await idle(a.id);
    await idle(b.id);
    run(env.core.db, "UPDATE sessions SET stage = 'review', pr_number = 4, pr_url = 'https://github.com/acme/api/pull/4' WHERE id = ?", a.id);
    run(env.core.db, "UPDATE sessions SET stage = 'review', pr_number = 21, pr_url = 'https://github.com/acme/demo/pull/21' WHERE id = ?", b.id);
    gh.people.set('acme/api#4', { title: 'API change', author: 'me', users: ['alice', 'bob'], teams: [] });
    gh.people.set('acme/demo#21', { title: 'Demo change', author: 'me', users: ['alice', 'me', 'carol'], teams: ['acme/platform'] });
    gh.profiles.set('alice', { name: 'Alice', email: 'alice@example.com' });
    env.core.prReview.rememberEmail('carol', 'carol@example.com');
    env.core.prReview.rememberEmail('bad login!', 'x@example.com'); // ignored
    const r = await env.core.prReview.reviewers(p.id);
    expect(r.map((x) => [x.repo, x.number, x.title])).toEqual([['acme/demo', 21, 'Demo change'], ['acme/api', 4, 'API change']]);
    expect(r[0].reviewers).toEqual([
      { login: 'alice', name: 'Alice', email: 'alice@example.com', emailFrom: 'github' },
      { login: 'carol', name: null, email: 'carol@example.com', emailFrom: 'remembered' },
    ]);
    expect(r[0].teams).toEqual(['acme/platform']);
    expect(r[1].reviewers.map((x) => [x.login, x.email])).toEqual([['alice', 'alice@example.com'], ['bob', null]]);
    gh.people.delete('acme/api#4');
    expect((await env.core.prReview.reviewers(p.id))[1]).toMatchObject({ repo: 'acme/api', error: expect.stringMatching(/Couldn't read PR #4's reviewers/) });
  });
});
