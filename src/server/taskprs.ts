import { basename } from 'node:path';
import type { TaskPr, TaskPrs } from '../shared/types.js';
import type { Core } from './core.js';
import { all } from './db.js';
import { githubRemotes, githubRepo, prLocalBranch, readRemotes } from './git.js';
import * as github from './github.js';
import { errMsg } from './log.js';
import { HttpError } from './projects.js';

/** Most PRs the New session dialog's Pull request picker lists. */
export const MAX_TASK_PRS = 30;

/**
 * The open PRs a new task session can work on (New session → Advanced → Pull request): yours in
 * the session's repo (<repoPath>, the project's by default), newest first; with <pr> (a number,
 * #number, owner/name#number or link) just that one. Each says why a session can't start on it, if
 * it can't (see TaskPr.blocked).
 */
export async function taskPrs(core: Core, projectId: string, q: { pr?: unknown; repoPath?: unknown }): Promise<TaskPrs> {
  const p = core.projects.row(projectId);
  const repo = core.projects.sessionRepo(p, q.repoPath);
  // The repos a session's PR can be in: the clone's origin, or its upstream for a fork's clone.
  const { origin, upstream } = await readRemotes(repo);
  const own = [githubRepo(upstream), githubRepo(origin)].filter((r): r is string => !!r).map((r) => r.toLowerCase());
  let found: github.OpenPr[];
  if (q.pr != null && q.pr !== '') {
    if (typeof q.pr !== 'string') throw new HttpError(400, 'The PR must be text.');
    const want = github.parsePrInput(q.pr);
    if (!want) throw new HttpError(400, 'Type a PR number, owner/name#number, or a GitHub PR link.');
    const ghRepo = want.repo ?? githubRepo(upstream) ?? githubRepo(origin);
    if (!ghRepo) throw new HttpError(400, `${basename(repo)} has no GitHub remote. Type the PR's link instead.`);
    let info: github.PrInfo;
    try {
      info = await github.prInfo(repo, ghRepo, want.number);
    } catch (e) {
      throw new HttpError(400, `Couldn't find PR #${want.number} of ${ghRepo} on GitHub: ${errMsg(e)}`);
    }
    if (info.state !== 'OPEN') throw new HttpError(400, `PR #${info.number} of ${ghRepo} is ${info.state === 'MERGED' ? 'merged' : 'closed'}.`);
    found = [{
      repo: ghRepo, number: info.number, title: info.title, url: info.url, headRefName: info.headRefName ?? '', headRepo: info.headRepo ?? null,
      baseRefName: info.baseRefName, isCrossRepository: !!info.isCrossRepository, updatedAt: info.updatedAt ?? info.createdAt,
    }];
  } else {
    try {
      found = await github.myOpenPrs(repo, MAX_TASK_PRS);
    } catch (e) {
      return { prs: [], error: `Couldn't list your open PRs on GitHub: ${errMsg(e)}` };
    }
  }
  const remotes = await githubRemotes(repo);
  const running = all<{ name: string; branch: string }>(core.db, `SELECT name, branch FROM sessions WHERE desired = 'running' AND repo_path = ?`, repo);
  const prs = await Promise.all(found.map(async (pr): Promise<TaskPr> => {
    const remote = pr.headRepo ? remotes.get(pr.headRepo.toLowerCase()) : undefined;
    let blocked: string | null = null;
    if (!pr.headRepo) blocked = 'Its fork was deleted, so its branch can\'t be fetched.';
    else if (!own.includes(pr.repo.toLowerCase())) blocked = `${basename(repo)} isn't a clone of ${pr.repo} (its origin, or its upstream for a fork).`;
    else if (!remote) {
      blocked = `Its branch is on ${pr.headRepo}, which isn't a remote here. Add it: git remote add ${pr.headRepo.split('/')[0]} https://github.com/${pr.headRepo}.git`;
    } else {
      const branch = await prLocalBranch(repo, { remote, branch: pr.headRefName });
      const user = running.find((s) => s.branch === branch);
      if (user) blocked = `The session "${user.name}" works on its branch.`;
    }
    return {
      repo: pr.repo, number: pr.number, title: pr.title, url: pr.url, headRefName: pr.headRefName, headRepo: pr.headRepo ?? pr.repo,
      baseRefName: pr.baseRefName, updatedAt: pr.updatedAt, fork: pr.isCrossRepository, blocked,
    };
  }));
  return { prs };
}
