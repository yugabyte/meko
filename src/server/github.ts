import { execFile } from 'node:child_process';
import { promisify } from 'node:util';
import type { CiState, MergeMethod } from '../shared/types.js';

const exec = promisify(execFile);

async function gh(cwd: string, ...args: string[]): Promise<string> {
  return ghWithin(cwd, 0, ...args);
}

/** `gh` with a time limit (0: none). */
async function ghWithin(cwd: string, timeoutMs: number, ...args: string[]): Promise<string> {
  const { stdout } = await exec('gh', args, {
    cwd, maxBuffer: 32 * 1024 * 1024, timeout: timeoutMs, env: { ...process.env, GH_PROMPT_DISABLED: '1', NO_COLOR: '1' },
  });
  return stdout.trim();
}

export interface IssueText {
  title: string;
  body: string;
}

/** An issue's (or PR's) title and body, for a Meko search on a task that names it. */
export async function issueText(cwd: string, number: number, timeoutMs = 5000): Promise<IssueText> {
  const r = JSON.parse(await ghWithin(cwd, timeoutMs, 'issue', 'view', String(number), '--json', 'title,body')) as { title?: unknown; body?: unknown };
  return { title: typeof r.title === 'string' ? r.title : '', body: typeof r.body === 'string' ? r.body : '' };
}

export interface PrRef {
  number: number;
  url: string;
  state: 'OPEN' | 'CLOSED' | 'MERGED';
  createdAt: string;
  /** From a fork: `gh pr list --head` matches the branch name in any repo. */
  isCrossRepository?: boolean;
  baseRefName?: string;
}

/** Leeway for the host's clock being ahead of GitHub's. */
export const CLOCK_SKEW_MS = 60_000;

/**
 * The PR for a branch that was opened at or after `since` (ms). Branch names can be reused,
 * so an older PR from the same branch is never taken for this one.
 */
export async function findPr(worktree: string, branch: string, since: number, anyOpen = false): Promise<PrRef | null> {
  const out = await gh(worktree, 'pr', 'list', '--head', branch, '--state', 'all', '--json', 'number,url,state,createdAt,isCrossRepository,baseRefName', '--limit', '20');
  return pickPr(JSON.parse(out || '[]') as PrRef[], since, anyOpen);
}

/**
 * The newest PR opened since `since`, an open one first. With `anyOpen`, an older open PR from this
 * repo (not a fork's branch of the same name) counts too.
 */
export function pickPr(prs: PrRef[], since: number, anyOpen = false): PrRef | null {
  const fresh = prs
    .filter((p) => (anyOpen && p.state === 'OPEN' && p.isCrossRepository === false) || Date.parse(p.createdAt) >= since - CLOCK_SKEW_MS)
    .sort((a, b) => Date.parse(b.createdAt) - Date.parse(a.createdAt));
  return fresh.find((p) => p.state === 'OPEN') ?? fresh[0] ?? null;
}

interface Author {
  login: string;
}

export interface PrReview {
  id: string;
  author: Author | null;
  state: string;
  body: string;
  submittedAt: string;
}

export interface PrComment {
  id: string;
  author: Author | null;
  body: string;
  createdAt: string;
}

interface CheckRun {
  __typename?: string;
  name?: string;
  status?: string;
  conclusion?: string;
  detailsUrl?: string;
  // StatusContext
  context?: string;
  state?: string;
  targetUrl?: string;
}

export interface FailedCheck {
  name: string;
  conclusion: string;
  url: string;
}

export interface CiSummary {
  state: CiState;
  /** Some checks haven't finished. */
  pending: boolean;
  failed: FailedCheck[];
}

export interface PrStatus {
  number: number;
  url: string;
  title: string;
  body: string;
  state: 'OPEN' | 'CLOSED' | 'MERGED';
  reviews: PrReview[];
  comments: PrComment[];
  statusCheckRollup: CheckRun[] | null;
  reviewDecision: '' | 'APPROVED' | 'CHANGES_REQUESTED' | 'REVIEW_REQUIRED';
  mergeable: 'MERGEABLE' | 'CONFLICTING' | 'UNKNOWN';
  mergeStateStatus: string;
  /** The commit the PR's branch points at on GitHub (at the merge, once merged). */
  headRefOid: string;
}

export async function prStatus(worktree: string, number: number): Promise<PrStatus> {
  const out = await gh(worktree, 'pr', 'view', String(number), '--json',
    'number,url,title,body,state,reviews,comments,statusCheckRollup,reviewDecision,mergeable,mergeStateStatus,headRefOid');
  return JSON.parse(out) as PrStatus;
}

/** What a PR changed: its commits, lines, and each file (paths go only as far as filetypes.ts). */
export interface PrStats {
  commits: number;
  additions: number;
  deletions: number;
  files: { path: string; additions: number; deletions: number }[];
}

/** The size of a PR, by URL (so any folder will do as `cwd`). GitHub lists up to 100 files. */
export async function prStats(cwd: string, url: string): Promise<PrStats> {
  const r = JSON.parse(await gh(cwd, 'pr', 'view', url, '--json', 'commits,additions,deletions,files')) as {
    commits?: unknown[];
    additions?: number;
    deletions?: number;
    files?: { path?: string; additions?: number; deletions?: number }[] | null;
  };
  const n = (v: unknown) => (typeof v === 'number' && Number.isFinite(v) ? v : 0);
  return {
    commits: Array.isArray(r.commits) ? r.commits.length : 0,
    additions: n(r.additions),
    deletions: n(r.deletions),
    files: (r.files ?? []).map((f) => ({ path: String(f.path ?? ''), additions: n(f.additions), deletions: n(f.deletions) })),
  };
}

const FAILED = ['FAILURE', 'ERROR', 'TIMED_OUT', 'CANCELLED', 'ACTION_REQUIRED', 'STARTUP_FAILURE'];

export function ciSummary(rollup: CheckRun[] | null): CiSummary {
  const failed: FailedCheck[] = [];
  let pending = false;
  for (const c of rollup ?? []) {
    const conclusion = (c.conclusion ?? c.state ?? '').toUpperCase();
    const status = (c.status ?? '').toUpperCase();
    if (FAILED.includes(conclusion)) {
      failed.push({ name: c.name || c.context || 'unnamed check', conclusion, url: c.detailsUrl || c.targetUrl || '' });
    } else if ((status && status !== 'COMPLETED') || conclusion === 'PENDING' || conclusion === 'EXPECTED' || conclusion === '') {
      pending = true;
    }
  }
  const state: CiState = !rollup?.length ? 'none' : failed.length ? 'failure' : pending ? 'pending' : 'success';
  return { state, pending, failed };
}

export function ciState(rollup: CheckRun[] | null): CiState {
  return ciSummary(rollup).state;
}

export function isLogin(author: Author | null, login: string): boolean {
  if (!author) return false;
  const a = author.login.toLowerCase().replace(/\[bot\]$/, '');
  return a === login.toLowerCase().replace(/\[bot\]$/, '');
}

/**
 * Logins of the bots (GitHub Apps) that reviewed a PR. `gh pr view` doesn't say which authors
 * are bots; the REST API does.
 */
export async function botLogins(worktree: string, number: number): Promise<string[]> {
  const out = await gh(worktree, 'api', '--paginate', `repos/{owner}/{repo}/pulls/${number}/reviews?per_page=100`,
    '--jq', '.[] | select(.user.type == "Bot") | .user.login');
  return out.split('\n').map((l) => l.trim()).filter(Boolean);
}

export async function comment(worktree: string, number: number, body: string): Promise<void> {
  await gh(worktree, 'pr', 'comment', String(number), '--body', body);
}

export async function resolveMergeMethod(worktree: string, method: MergeMethod): Promise<'squash' | 'merge' | 'rebase'> {
  if (method !== 'repo-default') return method;
  try {
    const r = JSON.parse(await gh(worktree, 'repo', 'view', '--json',
      'viewerDefaultMergeMethod,squashMergeAllowed,mergeCommitAllowed,rebaseMergeAllowed')) as {
      viewerDefaultMergeMethod?: string;
      squashMergeAllowed?: boolean;
      mergeCommitAllowed?: boolean;
      rebaseMergeAllowed?: boolean;
    };
    const d = (r.viewerDefaultMergeMethod ?? '').toLowerCase();
    if (d === 'squash' || d === 'merge' || d === 'rebase') return d;
    if (r.squashMergeAllowed) return 'squash';
    if (r.mergeCommitAllowed) return 'merge';
    if (r.rebaseMergeAllowed) return 'rebase';
  } catch {
    /* fall through */
  }
  return 'squash';
}

/** Merges by URL so gh never switches branches in the worktree. */
export async function merge(worktree: string, url: string, method: 'squash' | 'merge' | 'rebase'): Promise<void> {
  await gh(worktree, 'pr', 'merge', url, `--${method}`);
}

export async function ghAvailable(): Promise<boolean> {
  try {
    await exec('gh', ['auth', 'status']);
    return true;
  } catch {
    return false;
  }
}

// ---- Review a PR ----

const REPO_RE = /^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/;

/** True for an `owner/name` GitHub repo. */
export function isRepoName(s: unknown): s is string {
  return typeof s === 'string' && REPO_RE.test(s) && !s.split('/').some((p) => p === '.' || p === '..');
}

/**
 * A PR the user typed: `21`, `#21`, `owner/name#21`, or a PR link
 * (`https://github.com/owner/name/pull/21`, with anything after the number). A bare number has
 * no repo (null). Null for anything else.
 */
export function parsePrInput(input: string): { repo: string | null; number: number } | null {
  const s = input.trim();
  let m = /^#?(\d{1,7})$/.exec(s);
  if (m) return Number(m[1]) > 0 ? { repo: null, number: Number(m[1]) } : null;
  m = /^([A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+)#(\d{1,7})$/.exec(s);
  if (!m) m = /^(?:https?:\/\/)?(?:www\.)?github\.com\/([A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+)\/pull\/(\d{1,7})(?:[/?#].*)?$/i.exec(s);
  if (!m || !isRepoName(m[1]) || Number(m[2]) <= 0) return null;
  return { repo: m[1], number: Number(m[2]) };
}

/** `owner/name` and the number from a PR's URL, or null. */
export function prFromUrl(url: string | null): { repo: string; number: number } | null {
  const p = url ? parsePrInput(url) : null;
  return p?.repo ? { repo: p.repo, number: p.number } : null;
}

export interface PrInfo {
  number: number;
  url: string;
  title: string;
  body: string;
  author: string;
  state: 'OPEN' | 'CLOSED' | 'MERGED';
  baseRefName: string;
  headRefOid: string;
  createdAt: string;
}

/** A PR of any repo, by its `owner/name` and number. */
export async function prInfo(cwd: string, repo: string, number: number): Promise<PrInfo> {
  const r = JSON.parse(await gh(cwd, 'pr', 'view', String(number), '--repo', repo, '--json',
    'number,url,title,body,author,state,baseRefName,headRefOid,createdAt')) as Omit<PrInfo, 'author'> & { author: Author | null };
  return { ...r, author: r.author?.login ?? '' };
}

export interface FoundPr {
  repo: string;
  number: number;
  title: string;
  author: string;
  url: string;
  createdAt: string;
}

/**
 * Open PRs, in any repo the user can read, whose description has <text> in it (a datapack id),
 * newest first. GitHub's search lags new PRs by a minute or so.
 */
export async function searchOpenPrs(cwd: string, text: string, limit = 20): Promise<FoundPr[]> {
  const out = await ghWithin(cwd, 20_000, 'search', 'prs', `"${text.replace(/"/g, '')}"`, '--match', 'body', '--state', 'open',
    '--sort', 'created', '--order', 'desc', '--json', 'repository,number,title,author,url,createdAt', '--limit', String(limit));
  const rows = JSON.parse(out || '[]') as { repository?: { nameWithOwner?: string }; number: number; title: string; author?: Author | null; url: string; createdAt: string }[];
  return rows
    .filter((r) => isRepoName(r.repository?.nameWithOwner))
    .map((r) => ({ repo: r.repository!.nameWithOwner!, number: r.number, title: r.title, author: r.author?.login ?? '', url: r.url, createdAt: r.createdAt }));
}

/** What a Review a PR session follows on its PR. */
export interface PrWatch {
  state: 'OPEN' | 'CLOSED' | 'MERGED';
  headRefOid: string;
  /** The PR's commits, oldest first (GitHub lists up to 250). */
  commits: string[];
  reviews: PrReview[];
}

export async function prWatch(cwd: string, repo: string, number: number): Promise<PrWatch> {
  const r = JSON.parse(await gh(cwd, 'pr', 'view', String(number), '--repo', repo, '--json', 'state,headRefOid,commits,reviews')) as {
    state: PrWatch['state'];
    headRefOid: string;
    commits?: { oid?: string }[] | null;
    reviews?: PrReview[] | null;
  };
  return {
    state: r.state,
    headRefOid: r.headRefOid,
    commits: (r.commits ?? []).map((c) => c.oid ?? '').filter(Boolean),
    reviews: r.reviews ?? [],
  };
}

/** The login `gh` is signed in as. */
export async function viewerLogin(cwd: string): Promise<string> {
  return gh(cwd, 'api', 'user', '--jq', '.login');
}

export interface PrPeople {
  title: string;
  author: string;
  /** People asked to review, and people who have reviewed: GitHub users, bots left out. */
  users: string[];
  /** Teams asked to review, as `org/slug`. */
  teams: string[];
}

/** Who reviews a PR, from GitHub's REST API (which says which accounts are bots). */
export async function prPeople(cwd: string, repo: string, number: number): Promise<PrPeople> {
  const [pr, requested, reviews] = await Promise.all([
    gh(cwd, 'api', `repos/${repo}/pulls/${number}`, '--jq', '{title: .title, author: .user.login}'),
    gh(cwd, 'api', `repos/${repo}/pulls/${number}/requested_reviewers`),
    gh(cwd, 'api', '--paginate', `repos/${repo}/pulls/${number}/reviews?per_page=100`, '--jq', '.[] | {login: .user.login, type: .user.type}'),
  ]);
  const head = JSON.parse(pr) as { title?: string; author?: string };
  const req = JSON.parse(requested || '{}') as { users?: { login?: string; type?: string }[]; teams?: { slug?: string }[] };
  const reviewed = reviews.split('\n').filter((l) => l.trim()).map((l) => JSON.parse(l) as { login?: string; type?: string });
  const users: string[] = [];
  for (const u of [...(req.users ?? []), ...reviewed]) {
    if (!u.login || u.type === 'Bot' || /\[bot\]$/i.test(u.login) || users.includes(u.login)) continue;
    users.push(u.login);
  }
  const org = repo.split('/')[0];
  return {
    title: head.title ?? '',
    author: head.author ?? '',
    users,
    teams: (req.teams ?? []).map((t) => (t.slug ? `${org}/${t.slug}` : '')).filter(Boolean),
  };
}

/** A GitHub user's display name and public email (null when they don't show one). */
export async function userProfile(cwd: string, login: string): Promise<{ name: string | null; email: string | null }> {
  const r = JSON.parse(await gh(cwd, 'api', `users/${login}`, '--jq', '{name: .name, email: .email}')) as { name?: string | null; email?: string | null };
  return { name: r.name || null, email: r.email || null };
}
