import { execFile } from 'node:child_process';
import { existsSync } from 'node:fs';
import { promisify } from 'node:util';
import { isRepoName, parsePrInput } from '../shared/github-refs.js';
import type { CiState, MergeMethod } from '../shared/types.js';
import { TRUSTED_ASSOCIATIONS } from './headtrust.js';

const exec = promisify(execFile);

async function gh(cwd: string, ...args: string[]): Promise<string> {
  return ghWithin(cwd, 0, ...args);
}

/** Whether the login shell gave the server no PATH at its start (launcher.ts adoptLoginPath, set by `serve`), for gh's error. */
let loginPathMissing = false;

export function setLoginPathMissing(missing: boolean): void {
  loginPathMissing = missing;
}

/** Whether the login shell gave the server no PATH at its start: gh may be installed where it isn't looked for. */
export function isLoginPathMissing(): boolean {
  return loginPathMissing;
}

/** `gh` with a time limit (0: none). */
async function ghWithin(cwd: string, timeoutMs: number, ...args: string[]): Promise<string> {
  try {
    const { stdout } = await exec('gh', args, {
      cwd, maxBuffer: 32 * 1024 * 1024, timeout: timeoutMs, env: { ...process.env, GH_PROMPT_DISABLED: '1', NO_COLOR: '1' },
    });
    return stdout.trim();
  } catch (e) {
    throw ghMissing(e, cwd) ?? e;
  }
}

/**
 * "spawn gh ENOENT" in words, or null for any other error. Node says the same for a folder to run
 * in that's gone, so that keeps its own error.
 */
function ghMissing(e: unknown, cwd: string): Error | null {
  if ((e as NodeJS.ErrnoException).code !== 'ENOENT' || !existsSync(cwd)) return null;
  if (!loginPathMissing) return new Error("AgentK can't find gh, GitHub's command-line tool. AgentK offers to install it the next time you open a PR, or install it from https://cli.github.com.");
  const allow = process.platform === 'darwin'
    ? ' If macOS asked whether meko may open a folder, allow it (System Settings › Privacy & Security › Files and Folders), then run meko kode restart.'
    : ' Run meko kode restart.';
  return new Error(`AgentK can't find gh, because it couldn't read PATH from your login shell when it started.${allow}`);
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
  /** `owner/name` of the repo the head branch is in, when gh says (findPr reads it). */
  headRepo?: string | null;
}

/** Leeway for the host's clock being ahead of GitHub's. */
export const CLOCK_SKEW_MS = 60_000;

/**
 * The PR for a branch that was opened at or after `since` (ms). Branch names can be reused,
 * so an older PR from the same branch is never taken for this one. `headRepo`: the `owner/name`
 * the branch is pushed to (see pickPr).
 */
export async function findPr(worktree: string, branch: string, since: number, anyOpen = false, headRepo: string | null = null): Promise<PrRef | null> {
  const out = await gh(worktree, 'pr', 'list', '--head', branch, '--state', 'all', '--json',
    'number,url,state,createdAt,isCrossRepository,baseRefName,headRepository,headRepositoryOwner', '--limit', '20');
  const prs = (JSON.parse(out || '[]') as (PrRef & HeadFields)[]).map(({ headRepository, headRepositoryOwner, ...p }) =>
    ({ ...p, headRepo: headRepoOf({ headRepository, headRepositoryOwner }) }));
  return pickPr(prs, since, anyOpen, headRepo);
}

/** `--repo <owner/name>` when <repo> is given, so gh reads that repo's PR rather than its default repo for the folder. */
const repoArg = (repo?: string | null): string[] => (repo ? ['--repo', repo] : []);

/** PR <number> of <repo> (default: the repo gh works on from <cwd>), as findPr reports one. */
export async function prByNumber(cwd: string, number: number, repo?: string | null): Promise<PrRef> {
  return JSON.parse(await gh(cwd, 'pr', 'view', String(number), ...repoArg(repo), '--json', 'number,url,state,createdAt,isCrossRepository,baseRefName')) as PrRef;
}

/**
 * The newest PR opened since `since`, an open one first. With `anyOpen`, an older open PR counts too.
 * Only a PR whose head is in `headRepo` (the repo the branch is pushed to) is the branch's: anyone
 * can open a PR from a fork's branch of the same name, and taking it would have AgentK follow and
 * merge it. Without `headRepo` (a remote whose URL doesn't name its GitHub repo), any PR opened
 * since counts, a fork's too (the user's own, from a clone of their fork), but an older one only
 * from the base repo itself.
 */
export function pickPr(prs: PrRef[], since: number, anyOpen = false, headRepo: string | null = null): PrRef | null {
  const ours = (p: PrRef) => !headRepo || p.headRepo?.toLowerCase() === headRepo.toLowerCase();
  const fresh = prs
    .filter(ours)
    .filter((p) => (anyOpen && p.state === 'OPEN' && (headRepo || p.isCrossRepository === false)) || Date.parse(p.createdAt) >= since - CLOCK_SKEW_MS)
    .sort((a, b) => Date.parse(b.createdAt) - Date.parse(a.createdAt));
  return fresh.find((p) => p.state === 'OPEN') ?? fresh[0] ?? null;
}

interface Author {
  login: string;
}

export interface PrReview {
  id: string;
  author: Author | null;
  /** How the author relates to the repo (OWNER, MEMBER, COLLABORATOR, CONTRIBUTOR, NONE, …). */
  authorAssociation?: string;
  state: string;
  body: string;
  submittedAt: string;
}

interface CheckRun {
  __typename?: string;
  name?: string;
  status?: string;
  conclusion?: string;
  detailsUrl?: string;
  workflowName?: string;
  startedAt?: string;
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
  statusCheckRollup: CheckRun[] | null;
  reviewDecision: '' | 'APPROVED' | 'CHANGES_REQUESTED' | 'REVIEW_REQUIRED';
  mergeable: 'MERGEABLE' | 'CONFLICTING' | 'UNKNOWN';
  mergeStateStatus: string;
  /** The commit the PR's branch points at on GitHub (at the merge, once merged). */
  headRefOid: string;
}

export async function prStatus(worktree: string, number: number, repo?: string | null): Promise<PrStatus> {
  const out = await gh(worktree, 'pr', 'view', String(number), ...repoArg(repo), '--json',
    'number,url,title,body,state,reviews,statusCheckRollup,reviewDecision,mergeable,mergeStateStatus,headRefOid');
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

/**
 * The newest run of each check. The rollup lists every run on the head commit, so a workflow that
 * ran again on the same commit (one that reruns when the PR is edited, say) lists its checks once
 * per run, and an old failure or cancellation would outlive the run that replaced it. A run that
 * hasn't started yet is the newest.
 */
function latestRuns(rollup: CheckRun[]): CheckRun[] {
  const started = (c: CheckRun) => {
    const t = c.startedAt ? Date.parse(c.startedAt) : NaN;
    return Number.isNaN(t) || t <= 0 ? Infinity : t;
  };
  const out = new Map<string, CheckRun>();
  rollup.forEach((c, i) => {
    const name = c.name || c.context;
    const key = name ? `${c.workflowName ?? ''}\0${name}` : `#${i}`;
    const prev = out.get(key);
    if (!prev || started(c) >= started(prev)) out.set(key, c);
  });
  return [...out.values()];
}

export function ciSummary(rollup: CheckRun[] | null): CiSummary {
  const failed: FailedCheck[] = [];
  let pending = false;
  for (const c of latestRuns(rollup ?? [])) {
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
 * Whether feedback from this author may be acted on: someone with a role on the repo. Bots are
 * looked up separately (`botLogins`).
 */
export function isTrustedAuthor(item: { author: Author | null; authorAssociation?: string }): boolean {
  if (!item.author) return false;
  return TRUSTED_ASSOCIATIONS.includes(item.authorAssociation ?? '');
}

/**
 * Logins of the bots (GitHub Apps) that reviewed a PR. `gh pr view` doesn't say which authors
 * are bots; the REST API does.
 */
export async function botLogins(worktree: string, number: number, repo?: string | null): Promise<string[]> {
  const out = await gh(worktree, 'api', '--paginate', `repos/${repo ?? '{owner}/{repo}'}/pulls/${number}/reviews?per_page=100`,
    '--jq', '.[] | select(.user.type == "Bot") | .user.login');
  return out.split('\n').map((l) => l.trim()).filter(Boolean);
}

/** What reviewDetails tells about one review. */
export interface ReviewDetail {
  dbId: number | null;
  url: string;
  /** `line` is null for an outdated comment (GitHub gives only its line in an old diff). */
  comments: { path: string; line: number | null; body: string; url: string }[];
  totalComments: number;
}

/** Most node ids GitHub's `nodes(ids:)` takes in one query. */
const NODES_MAX = 100;

/**
 * The GraphQL query for reviews' details by node id, with their first <first> inline comments.
 * Ids that aren't GitHub node ids are refused, since they go into the query.
 */
export function reviewDetailsQuery(ids: string[], first = 50): string {
  if (ids.some((id) => !/^[A-Za-z0-9_=-]+$/.test(id))) throw new Error('Not a GitHub review id.');
  return `{ nodes(ids: ${JSON.stringify(ids)}) { ... on PullRequestReview { id databaseId url comments(first: ${first}) { totalCount nodes { path line body url } } } } }`;
}

/** reviewDetailsQuery's answer, by node id; ids GitHub has no review for are left out. */
export function parseReviewDetails(json: string): Map<string, ReviewDetail> {
  const r = JSON.parse(json) as {
    data?: { nodes?: ({ id?: string; databaseId?: number | null; url?: string; comments?: { totalCount?: number; nodes?: { path?: string; line?: number | null; body?: string; url?: string }[] } } | null)[] };
  };
  const out = new Map<string, ReviewDetail>();
  for (const n of r.data?.nodes ?? []) {
    if (!n?.id) continue;
    const comments = (n.comments?.nodes ?? []).map((c) => ({ path: c.path ?? '', line: c.line ?? null, body: c.body ?? '', url: c.url ?? '' }));
    out.set(n.id, { dbId: n.databaseId ?? null, url: n.url ?? '', comments, totalComments: n.comments?.totalCount ?? comments.length });
  }
  return out;
}

/** Reviews' REST ids, links and first 50 inline comments, by the node ids `gh pr view` gives (a GraphQL call per 100). */
export async function reviewDetails(cwd: string, ids: string[]): Promise<Map<string, ReviewDetail>> {
  const out = new Map<string, ReviewDetail>();
  for (let i = 0; i < ids.length; i += NODES_MAX) {
    const query = reviewDetailsQuery(ids.slice(i, i + NODES_MAX));
    for (const [id, d] of parseReviewDetails(await gh(cwd, 'api', 'graphql', '-f', `query=${query}`))) out.set(id, d);
  }
  return out;
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

// ---- Review a PR ----

export { isRepoName, parsePrInput };

/** `owner/name` and the number from a PR's URL, or null. */
export function prFromUrl(url: string | null): { repo: string; number: number } | null {
  const p = url ? parsePrInput(url) : null;
  return p?.repo ? { repo: p.repo, number: p.number } : null;
}

/** A PR or an issue, read through the issues API, which answers for both. */
export interface IssueOrPr {
  kind: 'pr' | 'issue';
  number: number;
  url: string;
  title: string;
  body: string;
  author: string;
  /** How the author relates to the repo (OWNER, MEMBER, COLLABORATOR, CONTRIBUTOR, NONE, …), when GitHub says. */
  authorAssociation?: string;
  state: 'open' | 'closed' | 'merged';
}

export type RepoVisibility = 'public' | 'private' | 'internal';

/** Who can see <repo> (`owner/name`) on GitHub. Throws when gh can't say. */
export async function repoVisibility(cwd: string, repo: string): Promise<RepoVisibility> {
  if (!isRepoName(repo)) throw new Error(`${repo} isn't a GitHub repo name.`);
  const r = JSON.parse(await ghWithin(cwd, 20_000, 'api', `repos/${repo}`)) as { visibility?: unknown; private?: unknown };
  if (r.visibility === 'public' || r.visibility === 'private' || r.visibility === 'internal') return r.visibility;
  if (typeof r.private === 'boolean') return r.private ? 'private' : 'public';
  throw new Error(`GitHub didn't say who can see ${repo}.`);
}

/** PR or issue <number> of <repo> (`owner/name`): GitHub numbers them together, so this says which. */
export async function issueOrPr(cwd: string, repo: string, number: number): Promise<IssueOrPr> {
  const r = JSON.parse(await ghWithin(cwd, 20_000, 'api', `repos/${repo}/issues/${number}`)) as {
    number?: number; html_url?: string; title?: string | null; body?: string | null; state?: string;
    user?: { login?: string } | null; author_association?: string; pull_request?: { merged_at?: string | null } | null;
  };
  const pr = r.pull_request ?? null;
  return {
    kind: pr ? 'pr' : 'issue',
    number: r.number ?? number,
    url: r.html_url ?? `https://github.com/${repo}/${pr ? 'pull' : 'issues'}/${number}`,
    title: r.title ?? '',
    body: r.body ?? '',
    author: r.user?.login ?? '',
    authorAssociation: r.author_association,
    state: r.state === 'open' ? 'open' : pr?.merged_at ? 'merged' : 'closed',
  };
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
  updatedAt?: string;
  /** The head branch, in `headRepo`. */
  headRefName?: string;
  /** `owner/name` of the repo the head branch is in (a fork's, for a PR from a fork); null when GitHub doesn't say (the fork was deleted). */
  headRepo?: string | null;
  isCrossRepository?: boolean;
}

interface HeadFields {
  headRepository?: { name?: string } | null;
  headRepositoryOwner?: { login?: string } | null;
}

/** `owner/name` of a PR's head repo, from gh's headRepository and headRepositoryOwner. */
function headRepoOf(r: HeadFields): string | null {
  const owner = r.headRepositoryOwner?.login;
  const name = r.headRepository?.name;
  return owner && name ? `${owner}/${name}` : null;
}

/** A PR of any repo, by its `owner/name` and number. */
export async function prInfo(cwd: string, repo: string, number: number): Promise<PrInfo> {
  const r = JSON.parse(await gh(cwd, 'pr', 'view', String(number), '--repo', repo, '--json',
    'number,url,title,body,author,state,baseRefName,headRefOid,createdAt,updatedAt,headRefName,headRepository,headRepositoryOwner,isCrossRepository')) as
    Omit<PrInfo, 'author' | 'headRepo'> & HeadFields & { author: Author | null };
  return {
    number: r.number, url: r.url, title: r.title, body: r.body, author: r.author?.login ?? '', state: r.state, baseRefName: r.baseRefName,
    headRefOid: r.headRefOid, createdAt: r.createdAt, updatedAt: r.updatedAt, headRefName: r.headRefName, headRepo: headRepoOf(r), isCrossRepository: r.isCrossRepository,
  };
}

export interface OpenPr {
  /** `owner/name` of the repo the PR is in (from its URL). */
  repo: string;
  number: number;
  title: string;
  url: string;
  headRefName: string;
  headRepo: string | null;
  baseRefName: string;
  isCrossRepository: boolean;
  updatedAt: string;
}

/** The viewer's open PRs in the repo gh works on from <cwd>, newest first. */
export async function myOpenPrs(cwd: string, limit = 30): Promise<OpenPr[]> {
  const out = await gh(cwd, 'pr', 'list', '--author', '@me', '--state', 'open', '--limit', String(limit), '--json',
    'number,title,url,headRefName,headRepository,headRepositoryOwner,baseRefName,isCrossRepository,updatedAt');
  const prs = JSON.parse(out || '[]') as (Omit<OpenPr, 'repo' | 'headRepo'> & HeadFields)[];
  return prs
    .map((p): OpenPr | null => {
      const at = prFromUrl(p.url);
      if (!at) return null;
      return {
        repo: at.repo, number: p.number, title: p.title, url: p.url, headRefName: p.headRefName, headRepo: headRepoOf(p),
        baseRefName: p.baseRefName, isCrossRepository: !!p.isCrossRepository, updatedAt: p.updatedAt,
      };
    })
    .filter((p): p is OpenPr => p !== null)
    .sort((a, b) => Date.parse(b.updatedAt) - Date.parse(a.updatedAt));
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

/** An open PR that asks for the viewer's review. */
export interface ReviewRequest {
  repo: string;
  number: number;
  title: string;
  url: string;
}

/** The most review requests one read asks for; GitHub's search returns them in no set order. */
export const REVIEW_REQUESTS_MAX = 100;

/** The open PRs, in any repo, that ask for the viewer's review (`gh search prs --review-requested=@me`), at most `limit`. */
export async function reviewRequests(cwd: string, limit = REVIEW_REQUESTS_MAX): Promise<ReviewRequest[]> {
  const out = await ghWithin(cwd, 30_000, 'search', 'prs', '--review-requested', '@me', '--state', 'open',
    '--json', 'repository,number,title,url', '--limit', String(limit));
  const rows = JSON.parse(out || '[]') as { repository?: { nameWithOwner?: string }; number: number; title: string; url: string }[];
  return rows
    .filter((r) => isRepoName(r.repository?.nameWithOwner))
    .map((r) => ({ repo: r.repository!.nameWithOwner!, number: r.number, title: r.title, url: r.url }));
}

/** What a Review a PR session follows on its PR. */
export interface PrWatch {
  state: 'OPEN' | 'CLOSED' | 'MERGED';
  headRefOid: string;
  /** The PR's commits, oldest first (GitHub lists up to 250). */
  commits: string[];
  reviews: PrReview[];
  /** The PR's description (its `meko_design:` line names the design). */
  body: string;
}

export async function prWatch(cwd: string, repo: string, number: number): Promise<PrWatch> {
  const r = JSON.parse(await gh(cwd, 'pr', 'view', String(number), '--repo', repo, '--json', 'state,headRefOid,commits,reviews,body')) as {
    state: PrWatch['state'];
    headRefOid: string;
    commits?: { oid?: string }[] | null;
    reviews?: PrReview[] | null;
    body?: string | null;
  };
  return {
    state: r.state,
    headRefOid: r.headRefOid,
    commits: (r.commits ?? []).map((c) => c.oid ?? '').filter(Boolean),
    reviews: r.reviews ?? [],
    body: r.body ?? '',
  };
}

/** A PR's description, as it is on GitHub now. */
export async function prBody(cwd: string, number: number, repo?: string | null): Promise<string> {
  const r = JSON.parse(await gh(cwd, 'pr', 'view', String(number), ...repoArg(repo), '--json', 'body')) as { body?: string | null };
  return r.body ?? '';
}

/** Replaces a PR's description (`-f` sends it as text, so a leading @ is never read as a file). */
export async function setPrBody(cwd: string, number: number, body: string, repo?: string | null): Promise<void> {
  await gh(cwd, 'api', '-X', 'PATCH', `repos/${repo || '{owner}/{repo}'}/pulls/${number}`, '-f', `body=${body}`, '--silent');
}

/** The login `gh` is signed in as (with a time limit; 0: none). */
export async function viewerLogin(cwd: string, timeoutMs = 0): Promise<string> {
  return ghWithin(cwd, timeoutMs, 'api', 'user', '--jq', '.login');
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
