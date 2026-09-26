import { execFile } from 'node:child_process';
import { promisify } from 'node:util';
import type { CiState, MergeMethod } from '../shared/types.js';

const exec = promisify(execFile);

async function gh(cwd: string, ...args: string[]): Promise<string> {
  const { stdout } = await exec('gh', args, { cwd, maxBuffer: 32 * 1024 * 1024, env: { ...process.env, GH_PROMPT_DISABLED: '1', NO_COLOR: '1' } });
  return stdout.trim();
}

export interface PrRef {
  number: number;
  url: string;
  state: 'OPEN' | 'CLOSED' | 'MERGED';
  createdAt: string;
}

/** Leeway for the host's clock being ahead of GitHub's. */
const CLOCK_SKEW_MS = 60_000;

/**
 * The PR for a branch that was opened at or after `since` (ms). Branch names can be reused,
 * so an older PR from the same branch is never taken for this one.
 */
export async function findPr(worktree: string, branch: string, since: number): Promise<PrRef | null> {
  const out = await gh(worktree, 'pr', 'list', '--head', branch, '--state', 'all', '--json', 'number,url,state,createdAt', '--limit', '20');
  return pickPr(JSON.parse(out || '[]') as PrRef[], since);
}

/** The newest PR opened since `since`, an open one first. */
export function pickPr(prs: PrRef[], since: number): PrRef | null {
  const fresh = prs
    .filter((p) => Date.parse(p.createdAt) >= since - CLOCK_SKEW_MS)
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
