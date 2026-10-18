import { existsSync } from 'node:fs';
import { basename } from 'node:path';
import type { PrReviewer, PrReviewers, RepoInfo, ReviewablePr, ReviewablePrs } from '../shared/types.js';
import { matchRepo, normaliseRemote, type RepoCandidate } from '../shared/datapacks.js';
import type { Core } from './core.js';
import { kvGet, kvSet } from './db.js';
import { githubRepo, readRemotes } from './git.js';
import * as github from './github.js';
import { errMsg } from './log.js';
import { repoKey } from './paths.js';
import { HttpError } from './projects.js';
import { mekoDatapackLineId } from './prompts.js';
import { projectDatapacks, sessionRepo, sessionRows } from './store.js';

/** Most PRs the Review a PR picker lists. */
export const MAX_REVIEWABLE = 20;
/** How long a GitHub profile (name, public email) is kept before it's read again. */
const PROFILE_TTL_MS = 60 * 60 * 1000;
const LOGIN_RE = /^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})$/;
const EMAIL_RE = /^[^\s@]+@[^\s@]+\.[^\s@]+$/;

/** The kv key an email typed for a GitHub login is remembered under. */
export function reviewerEmailKey(login: string): string {
  return `reviewer-email:${login.toLowerCase()}`;
}

/**
 * Reviewing other people's PRs, and sharing a project with its PRs' reviewers: the PRs a Review a
 * PR session can start on (found on GitHub by the project's datapack ids, in any repo, each matched
 * to a local clone), cloning a PR's repo, and who reviews the project's open PRs.
 */
export class PrReviewService {
  private profiles = new Map<string, { at: number; name: string | null; email: string | null }>();

  constructor(private core: Core) {}

  private get db() {
    return this.core.db;
  }

  /**
   * The open PRs whose description names one of the project's datapacks (the PRs its sessions
   * opened end with the datapack's id), newest first. With <pr>, just that PR: a bare number is
   * looked up in <repoPath> (the project's repo by default).
   */
  async reviewable(projectId: string, q: { pr?: unknown; repoPath?: unknown }): Promise<ReviewablePrs> {
    const p = this.core.projects.row(projectId);
    const clones = await this.clones(projectId);
    const local = (repo: string) => localClone(clones, repo);
    if (q.pr != null && q.pr !== '') {
      if (typeof q.pr !== 'string') throw new HttpError(400, 'The PR must be text.');
      const want = github.parsePrInput(q.pr);
      if (!want) throw new HttpError(400, 'Type a PR number, owner/name#number, or a GitHub PR link.');
      const where = this.core.projects.sessionRepo(p, q.repoPath);
      const repo = want.repo ?? (await githubRemote(where));
      if (!repo) throw new HttpError(400, `${basename(where)} has no GitHub remote. Type the PR's link instead.`);
      let info: github.PrInfo;
      try {
        info = await github.prInfo(where, repo, want.number);
      } catch (e) {
        throw new HttpError(400, `Couldn't find PR #${want.number} of ${repo} on GitHub: ${errMsg(e)}`);
      }
      if (info.state !== 'OPEN') throw new HttpError(400, `PR #${info.number} of ${repo} is ${info.state === 'MERGED' ? 'merged' : 'closed'}.`);
      const ours = new Set(projectDatapacks(this.db, projectId).map((d) => d.id));
      const other = datapackIdsIn(info.body).find((id) => !ours.has(id)) ?? null;
      return { prs: [toReviewable({ ...info, repo }, local(repo), other)] };
    }
    const ids = projectDatapacks(this.db, projectId).map((d) => d.id);
    if (!ids.length) return { prs: [] };
    const found = await Promise.allSettled(ids.map((id) => github.searchOpenPrs(p.repo_path, id, MAX_REVIEWABLE)));
    const failed = found.find((f): f is PromiseRejectedResult => f.status === 'rejected');
    if (failed && found.every((f) => f.status === 'rejected')) {
      return { prs: [], error: `Couldn't search GitHub for the project's PRs: ${errMsg(failed.reason)}` };
    }
    const seen = new Set<string>();
    const prs: ReviewablePr[] = [];
    for (const f of found) {
      if (f.status !== 'fulfilled') continue;
      for (const pr of f.value) {
        const key = `${pr.repo.toLowerCase()}#${pr.number}`;
        if (seen.has(key)) continue;
        seen.add(key);
        prs.push(toReviewable(pr, local(pr.repo)));
      }
    }
    prs.sort((a, b) => Date.parse(b.createdAt) - Date.parse(a.createdAt));
    return { prs: prs.slice(0, MAX_REVIEWABLE) };
  }

  /** Clones a PR's repo into the repos folder (or finds the clone already there), for the PR picker. */
  async clone(url: unknown): Promise<RepoInfo> {
    if (typeof url !== 'string' || !normaliseRemote(url)) throw new HttpError(400, 'Give the URL of the repo to clone.');
    const dest = repoKey(await this.core.projects.clone(url.trim()));
    const listed = (await this.core.projects.repos()).repos.find((r) => repoKey(r.path) === dest);
    if (listed) return listed;
    // Cloned outside the folders the repo list scans: still reported, though sessions can't pick it.
    const { origin, upstream } = await readRemotes(dest);
    return { path: dest, name: basename(dest), remote: origin, upstream, branch: null, lastTouchedAt: Date.now(), projects: [] };
  }

  /**
   * Who reviews the open PRs of the project's running sessions (each in its own session's repo):
   * the people asked to review and the people who have, without bots, the PR's author or us.
   */
  async reviewers(projectId: string): Promise<PrReviewers[]> {
    this.core.projects.row(projectId);
    const rows = sessionRows(this.db, projectId)
      .filter((r) => r.desired === 'running' && r.kind !== 'review-pr' && r.pr_url && (r.stage === 'review' || r.stage === 'merge'))
      .reverse();
    return Promise.all(rows.map(async (r): Promise<PrReviewers> => {
      const pr = github.prFromUrl(r.pr_url);
      const base = { sessionId: r.id, repo: pr?.repo ?? '', number: r.pr_number ?? 0, title: '', url: r.pr_url ?? '', reviewers: [], teams: [] };
      if (!pr) return { ...base, error: "The PR's link isn't a GitHub PR link." };
      const cwd = existsSync(r.worktree_path) && this.core.sessions.ownsWorktree(r) ? r.worktree_path : (sessionRepo(this.db, r) ?? r.worktree_path);
      try {
        const [people, me] = await Promise.all([github.prPeople(cwd, pr.repo, pr.number), this.core.ghLogin(cwd)]);
        const logins = people.users.filter((u) => LOGIN_RE.test(u) && !same(u, people.author) && !(me && same(u, me)));
        return { ...base, title: people.title, teams: people.teams, reviewers: await Promise.all(logins.map((l) => this.reviewer(cwd, l))) };
      } catch (e) {
        return { ...base, error: `Couldn't read PR #${pr.number}'s reviewers from GitHub: ${errMsg(e)}` };
      }
    }));
  }

  /** Remembers the email the user shared with for a GitHub login, so the next PR fills it in. */
  rememberEmail(login: unknown, email: string): void {
    if (typeof login !== 'string' || !LOGIN_RE.test(login) || !EMAIL_RE.test(email)) return;
    kvSet(this.db, reviewerEmailKey(login), email);
  }

  private async reviewer(cwd: string, login: string): Promise<PrReviewer> {
    const remembered = kvGet(this.db, reviewerEmailKey(login));
    const profile = await this.profile(cwd, login);
    if (remembered) return { login, name: profile.name, email: remembered, emailFrom: 'remembered' };
    return { login, name: profile.name, email: profile.email, emailFrom: profile.email ? 'github' : null };
  }

  /** A GitHub profile, kept for an hour; a failure gives an empty one (the user types the email). */
  private async profile(cwd: string, login: string): Promise<{ name: string | null; email: string | null }> {
    const key = login.toLowerCase();
    const hit = this.profiles.get(key);
    if (hit && Date.now() - hit.at < PROFILE_TTL_MS) return hit;
    const got = await github.userProfile(cwd, login).catch(() => ({ name: null, email: null }));
    this.profiles.set(key, { at: Date.now(), ...got });
    return got;
  }

  /** The repos a PR's repo can be matched to: the scanned ones, and the project's and its sessions' repos. */
  private async clones(projectId: string): Promise<RepoCandidate[]> {
    const scanned: RepoCandidate[] = (await this.core.projects.repos()).repos;
    const paths = new Set(scanned.map((r) => repoKey(r.path)));
    const p = this.core.projects.row(projectId);
    const extra = [p.repo_path, ...sessionRows(this.db, projectId).map((r) => r.repo_path)]
      .filter((x): x is string => !!x && existsSync(x) && !paths.has(repoKey(x)));
    const more = await Promise.all([...new Set(extra)].map(async (path) => {
      const { origin, upstream } = await readRemotes(path);
      return { path, name: basename(path), remote: origin, upstream, lastTouchedAt: 0 };
    }));
    return [...scanned, ...more];
  }
}

function same(a: string, b: string): boolean {
  return a.toLowerCase() === b.toLowerCase();
}

/** The local clone of GitHub's <repo>: a fork whose upstream it is first, then a clone of it. */
export function localClone(clones: RepoCandidate[], repo: string): string | null {
  const m = matchRepo(clones, '', `github.com/${repo}`);
  return m.by === 'upstream' || m.by === 'remote' ? (m.repo?.path ?? null) : null;
}

/** The GitHub repo a local clone works on: its upstream for a fork, else its origin. */
async function githubRemote(path: string): Promise<string | null> {
  const { origin, upstream } = await readRemotes(path);
  return githubRepo(upstream) ?? githubRepo(origin);
}

/** The datapack ids a PR description's Meko lines name (see mekoDatapackLineId). */
export function datapackIdsIn(body: string): string[] {
  return body.split('\n').map(mekoDatapackLineId).filter((id): id is string => id !== null);
}

function toReviewable(
  pr: { repo: string; number: number; title: string; author: string; url: string; createdAt: string }, localRepo: string | null, otherDatapackId: string | null = null,
): ReviewablePr {
  return {
    repo: pr.repo,
    number: pr.number,
    title: pr.title,
    author: pr.author,
    url: pr.url,
    createdAt: pr.createdAt,
    localRepo,
    cloneUrl: `https://github.com/${pr.repo}.git`,
    otherDatapackId,
  };
}
