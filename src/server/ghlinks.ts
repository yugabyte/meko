import { existsSync } from 'node:fs';
import { defaultProjectDatapackName, freeDatapackName, parseProjectDatapack } from '../shared/datapacks.js';
import { parseGithubRef, type GithubRef } from '../shared/github-refs.js';
import type {
  CreateSessionRequest, DatapackChoice, LinkNewProject, LinkPlan, LinkProject, Project, StartFromLinkRequest, StartFromLinkResult,
} from '../shared/types.js';
import type { Core } from './core.js';
import { all, get } from './db.js';
import { isMainRepo, remoteFor } from './git.js';
import * as github from './github.js';
import { errMsg } from './log.js';
import { repoKey } from './paths.js';
import { datapackIdsIn, localClone } from './prreview.js';
import { HttpError } from './projects.js';
import { projectDatapacks, projectRows } from './store.js';

/** A new project as planned, with the datapack choice create() takes (an invite's id included). */
interface NewProjectPlan {
  plan: LinkNewProject;
  choice: DatapackChoice;
}

/**
 * Starting work from a GitHub link pasted into Search: a Review a PR session on a PR, a task that
 * fixes or implements an issue. Finds the project it belongs to (one with a datapack the
 * description's Meko lines name, else one on its repo), or opens one, cloning the repo if needed.
 */
export class GithubLinkService {
  constructor(private core: Core) {}

  private get db() {
    return this.core.db;
  }

  async plan(ref: unknown): Promise<LinkPlan> {
    const { repo, item } = await this.read(ref);
    const dpIds = datapackIdsIn(item.body);
    const project = await this.bestProject(repo, dpIds);
    return {
      kind: item.kind, repo, number: item.number, title: item.title, url: item.url, author: item.author, state: item.state,
      project,
      newProject: project ? null : (await this.newProject(repo, item, dpIds)).plan,
      existing: this.existing(repo, item),
    };
  }

  /** Starts the session the plan offers, in `projectId` or in the project the link fits (opened if need be). */
  async start(req: StartFromLinkRequest): Promise<StartFromLinkResult> {
    if (req.projectId != null && typeof req.projectId !== 'string') throw new HttpError(400, 'The project must be its id.');
    if (req.start != null && req.start !== 'design' && req.start !== 'code') throw new HttpError(400, "Start an issue's session at design or code.");
    const { repo, item } = await this.read(req.ref);
    if (item.kind === 'pr') {
      if (item.state !== 'open') throw new HttpError(400, `PR #${item.number} of ${repo} is ${item.state}, so there's nothing to review.`);
      const running = this.existing(repo, item)[0];
      if (running) throw new HttpError(409, `PR #${item.number} of ${repo} is already being reviewed in "${running.name}".`);
    }
    let projectId: string;
    let clone: string | null;
    if (req.projectId) {
      const p = this.core.projects.row(req.projectId);
      projectId = p.id;
      clone = await this.cloneFor(p.repo_path, repo);
    } else {
      const dpIds = datapackIdsIn(item.body);
      // Planned again rather than taken from the browser: an open project that fits by now is used.
      const best = await this.bestProject(repo, dpIds);
      if (best) {
        projectId = best.id;
        clone = best.clone;
      } else {
        const opened = await this.openProject(repo, await this.newProject(repo, item, dpIds));
        projectId = opened.id;
        clone = opened.repoPath;
      }
    }
    const p = this.core.projects.row(projectId);
    clone ??= repoKey(await this.core.projects.clone(cloneUrl(repo)));
    const repoPath = clone === p.repo_path ? undefined : clone;
    const create: CreateSessionRequest = item.kind === 'pr'
      ? { name: '', kind: 'review-pr', pr: { repo, number: item.number }, repoPath }
      : { name: issueSessionName(item), kind: 'task', start: req.start ?? 'design', prompt: issuePrompt(repo, item), repoPath };
    const { session, done } = await this.core.sessions.begin(projectId, create);
    done.catch((e: unknown) => this.core.log.warn(`session ${session.id} didn't start`, errMsg(e)));
    return { project: this.core.projects.get(projectId), session };
  }

  /** The PR or issue <ref> names, and its repo as GitHub spells it (the link it answers with). */
  private async read(ref: unknown): Promise<{ repo: string; item: github.IssueOrPr }> {
    const want: GithubRef | null = typeof ref === 'string' ? parseGithubRef(ref) : null;
    if (!want) throw new HttpError(400, 'Paste a GitHub PR or issue link, or type owner/name#number.');
    let item: github.IssueOrPr;
    try {
      item = await github.issueOrPr(this.core.paths.home, want.repo, want.number);
    } catch (e) {
      throw new HttpError(400, `Couldn't find #${want.number} of ${want.repo} on GitHub: ${errMsg(e)}`);
    }
    return { repo: parseGithubRef(item.url)?.repo ?? want.repo, item };
  }

  /**
   * The open project to start in: the first (in rail order) with a dedicated datapack the
   * description names, then one with it as a shared datapack, then the first on the repo.
   */
  private async bestProject(repo: string, dpIds: string[]): Promise<LinkProject | null> {
    const rows = projectRows(this.db);
    const named = new Set(dpIds);
    let pick: { row: (typeof rows)[number]; why: LinkProject['why'] } | null = null;
    if (named.size) {
      for (const role of ['dedicated', 'shared'] as const) {
        const row = rows.find((p) => projectDatapacks(this.db, p.id).some((d) => d.role === role && named.has(d.id)));
        if (row) {
          pick = { row, why: 'datapack' };
          break;
        }
      }
    }
    if (!pick) {
      for (const row of rows) {
        if (await isCloneOf(row.repo_path, repo)) {
          pick = { row, why: 'repo' };
          break;
        }
      }
    }
    if (!pick) return null;
    return { id: pick.row.id, name: pick.row.name, why: pick.why, clone: await this.cloneFor(pick.row.repo_path, repo) };
  }

  /** Where a session of a project on <projectRepo> works on <repo>: that repo if it's a clone, else a scanned clone, else none. */
  private async cloneFor(projectRepo: string, repo: string): Promise<string | null> {
    if (await isCloneOf(projectRepo, repo)) return projectRepo;
    return localClone((await this.core.projects.repos()).repos, repo);
  }

  /**
   * The project to open when no open project fits. Its datapack: one the description names that we
   * can open (a closed project with it as its dedicated datapack opens again as itself), else a new
   * one for an issue's project (a review only reads code), else none. Without Meko, none.
   */
  private async newProject(repo: string, item: github.IssueOrPr, dpIds: string[]): Promise<NewProjectPlan> {
    const repoName = repo.split('/')[1];
    let name = repoName;
    let repoPath: string | null = null;
    let datapack: LinkNewProject['datapack'] = null;
    let choice: DatapackChoice = { mode: 'none' };
    let reopens: string | null = null;
    if (this.core.mekoStatus.configured && dpIds.length) {
      const closed = this.core.projects.closed().find((c) => dpIds.includes(c.datapackId));
      if (closed) {
        const dpName = get<{ name: string }>(this.db, 'SELECT name FROM project_datapacks WHERE project_id = ? AND datapack_id = ?', closed.id, closed.datapackId)?.name
          ?? closed.datapackId;
        reopens = closed.id;
        name = closed.name;
        datapack = { id: closed.datapackId, name: dpName, invite: false };
        choice = { mode: 'existing', id: closed.datapackId, name: dpName };
        if (await isCloneOf(closed.repoPath, repo)) repoPath = closed.repoPath;
      } else {
        const listed = await this.core.meko.listDatapacks().catch(() => []);
        const dp = dpIds.map((id) => listed.find((d) => d.id === id)).find((d) => !!d);
        const invite = dp ? undefined : this.core.invites.open().find((i) => dpIds.includes(i.datapackId));
        const found = dp ?? (invite ? { id: invite.datapackId, name: invite.name } : null);
        if (found) {
          datapack = { id: found.id, name: found.name, invite: !!invite?.inviteId };
          choice = { mode: 'existing', id: found.id, name: found.name, ...(invite?.inviteId ? { inviteId: invite.inviteId } : {}) };
          name = parseProjectDatapack(found.name)?.project ?? repoName;
        }
      }
    }
    if (!datapack && this.core.mekoStatus.configured && item.kind === 'issue') {
      datapack = 'create';
      // Named when the project opens, against the datapacks there are then.
      choice = { mode: 'create', name: defaultProjectDatapackName(repoName, name) };
    }
    repoPath ??= localClone((await this.core.projects.repos()).repos, repo);
    return { plan: { name, repoPath, cloneUrl: cloneUrl(repo), datapack, reopens }, choice };
  }

  /** Opens the planned project. A new datapack gets a free name; at the plan's datapack limit the project opens without one. */
  private async openProject(repo: string, np: NewProjectPlan): Promise<Project> {
    const where = np.plan.repoPath ? { repoPath: np.plan.repoPath } : { cloneUrl: np.plan.cloneUrl };
    let choice = np.choice;
    if (choice.mode === 'create') {
      const taken = new Set((await this.core.meko.listDatapacks().catch(() => [])).map((d) => d.name));
      choice = { mode: 'create', name: freeDatapackName(choice.name, taken) };
    }
    try {
      return await this.core.projects.create({ ...where, name: np.plan.name, datapack: choice });
    } catch (e) {
      if (!(e instanceof HttpError && e.code === 'DATAPACK_LIMIT')) throw e;
      this.core.log.warn(`opening a project for ${repo} without a datapack: ${e.message}`);
      return this.core.projects.create({ ...where, name: np.plan.name, datapack: { mode: 'none' } });
    }
  }

  /** Running sessions on it: reviews of the PR, or tasks whose first prompt has the issue's link. */
  private existing(repo: string, item: github.IssueOrPr): LinkPlan['existing'] {
    const rows = item.kind === 'pr'
      ? all<{ id: string; project_id: string; name: string }>(this.db, `SELECT s.id, s.project_id, s.name FROM sessions s JOIN projects p ON p.id = s.project_id
          WHERE s.desired = 'running' AND p.deleted_at IS NULL AND s.kind = 'review-pr' AND lower(s.pr_repo) = lower(?) AND s.pr_number = ?
          ORDER BY s.created_at`, repo, item.number)
      : all<{ id: string; project_id: string; name: string }>(this.db, `SELECT s.id, s.project_id, s.name FROM sessions s JOIN projects p ON p.id = s.project_id
          WHERE s.desired = 'running' AND p.deleted_at IS NULL AND s.kind != 'review-pr' AND instr(s.initial_prompt, ?) > 0
          ORDER BY s.created_at`, item.url);
    return rows.map((r) => ({ sessionId: r.id, projectId: r.project_id, name: r.name }));
  }
}

function cloneUrl(repo: string): string {
  return `https://github.com/${repo}.git`;
}

async function isCloneOf(path: string, repo: string): Promise<boolean> {
  return existsSync(path) && isMainRepo(path) && !!(await remoteFor(path, repo));
}

/** An issue's task name: `#45 Add dark mode`, on one line. */
export function issueSessionName(item: { number: number; title: string }): string {
  return `#${item.number} ${item.title.replace(/\s+/g, ' ')}`.slice(0, 100).trim();
}

/**
 * An issue's task. The number stands alone (`#45`), so Meko recall at start reads the issue too
 * (issueRefs() skips `owner/name#45`); Claude reads the issue itself, with its comments.
 */
export function issuePrompt(repo: string, item: { number: number; title: string; url: string }): string {
  return [
    `Fix or implement issue #${item.number} of ${repo}: ${item.title.replace(/\s+/g, ' ').trim()}`,
    item.url,
    `Read the issue and its comments first (gh issue view ${item.number} --repo ${repo} --comments). If it reports a bug, fix it; if it asks for a feature or a change, implement it.`,
  ].join('\n');
}
